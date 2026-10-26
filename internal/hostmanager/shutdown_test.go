package hostmanager

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestManagerShutdownRequiresAdminAndFencesQueuedAdmission(t *testing.T) {
	e := newEnv(t, DefaultPolicy())
	p := e.authorize()
	if _, err := e.call(principal{projectID: p.ID}, Request{Action: ActionShutdown}); statusCode(err) != http.StatusForbidden {
		t.Fatalf("project credential stopped the manager: %v", err)
	}
	pending := e.mustCall(admin, Request{Action: ActionEnsure, ProjectID: p.ID})
	if _, err := e.call(admin, Request{Action: ActionShutdown}); err != nil {
		t.Fatal(err)
	}
	e.pass()
	if len(e.launchedOps()) != 0 || e.op(pending.Operation.ID).Phase != PhaseQueued {
		t.Fatal("shutdown admitted queued work")
	}
	if _, err := e.call(admin, Request{Action: ActionEnsure, ProjectID: p.ID}); statusCode(err) != http.StatusServiceUnavailable {
		t.Fatalf("shutdown accepted new work: %v", err)
	}
}

func TestManagerShutdownRefusesActiveWorkerAndInheritedLocks(t *testing.T) {
	t.Run("worker", func(t *testing.T) {
		e := newEnv(t, DefaultPolicy())
		e.runInline = false
		p := e.authorize()
		e.mustCall(admin, Request{Action: ActionEnsure, ProjectID: p.ID})
		e.pass()
		if _, err := e.call(admin, Request{Action: ActionShutdown}); statusCode(err) != http.StatusConflict {
			t.Fatalf("active worker was not protected: %v", err)
		}
	})
	for _, kind := range []string{"project", "helper"} {
		t.Run(kind, func(t *testing.T) {
			e := newEnv(t, DefaultPolicy())
			p := e.authorize()
			path := projectLockPath(e.cfg.Root, p.ID)
			if kind == "helper" {
				path = helperLockPath(e.cfg.Root)
			}
			lock, busy, err := tryFlock(path)
			if err != nil || busy {
				t.Fatalf("hold lifecycle lock: %v", err)
			}
			defer lock.Close()
			if _, err := e.call(admin, Request{Action: ActionShutdown}); statusCode(err) != http.StatusConflict {
				t.Fatalf("inherited lock was not protected: %v", err)
			}
			select {
			case <-e.srv.shutdownCh:
				t.Fatal("refused shutdown still stopped admission")
			default:
			}
		})
	}
}

func TestShutdownWaitsForDaemonExitAndKeepsProjectData(t *testing.T) {
	cfg := initTestManager(t, DefaultPolicy())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var serveError error
	go func() {
		defer close(done)
		serveError = Serve(ctx, cfg.Root, subprocessBackend{})
	}()
	t.Cleanup(func() { cancel(); <-done })
	waitFor(t, "manager startup", func() bool { return identityOK(ctx, cfg) })
	projectDir := t.TempDir()
	resp, err := Call(ctx, cfg.Root, AdminTokenFile(cfg.Root), Request{Action: ActionAuthorize, Path: projectDir})
	if err != nil || resp.Project == nil {
		t.Fatalf("authorize project: %v", err)
	}
	if err := os.MkdirAll(resp.Project.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(resp.Project.StateDir, "keep.fixture")
	if err := os.WriteFile(marker, []byte("retained project data"), 0o600); err != nil {
		t.Fatal(err)
	}
	stopCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	if err := Shutdown(stopCtx, cfg.Root); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-stopCtx.Done():
		t.Fatal("shutdown returned without a stopped daemon")
	}
	if serveError != nil {
		t.Fatal(serveError)
	}
	if _, err := os.Lstat(socketPath(cfg.Root)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("manager socket remains: %v", err)
	}
	got, err := ResolveProject(cfg.Root, projectDir)
	if err != nil || got.ID != resp.Project.ID {
		t.Fatalf("shutdown lost the project registration: %+v %v", got, err)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "retained project data" {
		t.Fatalf("shutdown changed project data: %v", err)
	}
	if err := Shutdown(stopCtx, cfg.Root); err != nil {
		t.Fatalf("stopping an already stopped daemon: %v", err)
	}
	if _, err := os.Lstat(socketPath(cfg.Root)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a second shutdown restarted the manager: %v", err)
	}
}
