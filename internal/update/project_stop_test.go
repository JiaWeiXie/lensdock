package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/JiaWeiXie/lensdock/internal/auth"
	"github.com/JiaWeiXie/lensdock/internal/lease"
)

type projectStopDocker struct {
	runtime        *probeRuntime
	stopped        bool
	inspectFailure bool
	stopCalls      int
}

func (d *projectStopDocker) Run(ctx context.Context, cmd Cmd) error {
	if len(cmd.Args) > 1 && cmd.Args[0] == "container" && cmd.Args[1] == "inspect" {
		if d.inspectFailure {
			return errors.New("engine disconnected")
		}
		_, err := fmt.Fprintf(cmd.Stdout, `{"Id":"runtime-id","Image":"sha256:%s","State":{"Running":%t}}`, strings.Repeat("a", 64), !d.stopped)
		return err
	}
	if len(cmd.Args) > 0 && cmd.Args[0] == "compose" {
		if slices.Contains(cmd.Args, "ps") {
			if !d.stopped {
				_, err := io.WriteString(cmd.Stdout, "runtime-id\n")
				return err
			}
			return nil
		}
		if slices.Contains(cmd.Args, "stop") {
			status := HandleProbe(ctx, d.runtime.request(ProbeStatus))
			if status.Status == nil || status.Status.ControlState != "MAINTENANCE" {
				return errors.New("Docker stop reached before the runtime control fence")
			}
			d.stopCalls++
			d.stopped = true
			return nil
		}
	}
	return fmt.Errorf("unexpected Docker command %v", cmd.Args)
}

func stopEngine(t *testing.T) (*Engine, *projectStopDocker, *probeRuntime) {
	t.Helper()
	rt := newProbeRuntime(t)
	root := t.TempDir()
	token := filepath.Join(root, "operator-token")
	if err := auth.WritePrivateFile(token, []byte(probeTestOperator)); err != nil {
		t.Fatal(err)
	}
	dep := &Deployment{Version: 1, StateDir: root, File: filepath.Join(root, "deployment.json"), Compose: ComposeConfig{Project: "stop-test", Files: []string{filepath.Join(root, "compose.yaml")}}, Workspaces: map[string]WorkspaceConfig{probeTestWorkspace: {RuntimeService: "runtime", APIURL: rt.srv.URL, OperatorTokenFile: token}}, Gateway: &GatewayConfig{Service: "gateway"}}
	dep.applyDefaults()
	docker := &projectStopDocker{runtime: rt}
	return NewEngine(dep, Docker{Run: docker}, io.Discard), docker, rt
}

func TestProjectStopRefusesLiveControlWithoutStoppingEitherService(t *testing.T) {
	for _, mode := range []lease.Mode{lease.ModeHuman, lease.ModeAgent} {
		t.Run(string(mode), func(t *testing.T) {
			engine, docker, rt := stopEngine(t)
			grant, token, err := rt.leases.Acquire("holder", mode, 0, "stop-test")
			if err != nil {
				t.Fatal(err)
			}
			err = engine.StopProject(context.Background(), probeTestWorkspace, ProjectStopOptions{DiscardPageState: true})
			if err == nil || docker.stopped || docker.stopCalls != 0 {
				t.Fatalf("stop error=%v stopped=%t calls=%d", err, docker.stopped, docker.stopCalls)
			}
			if _, err := rt.leases.Authorize(token.Reveal(), mode); err != nil {
				t.Fatalf("held lease was invalidated: %v", err)
			}
			status := HandleProbe(context.Background(), rt.request(ProbeStatus))
			if status.Status == nil || status.Status.LeaseID != grant.ID || status.Status.ControlState == "MAINTENANCE" {
				t.Fatalf("control changed on refused stop: %+v", status)
			}
		})
	}
}

func TestProjectStopFencesBeforeStoppingAndKeepsDeploymentData(t *testing.T) {
	engine, docker, rt := stopEngine(t)
	fixture := filepath.Join(engine.Dep.StateDir, "retained-fixture")
	if err := os.WriteFile(fixture, []byte("retained profile fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := engine.StopProject(context.Background(), probeTestWorkspace, ProjectStopOptions{DiscardPageState: true}); err != nil {
		t.Fatal(err)
	}
	if !docker.stopped || docker.stopCalls != 1 {
		t.Fatalf("stop state=%t calls=%d", docker.stopped, docker.stopCalls)
	}
	if _, _, err := rt.leases.Acquire("new-holder", lease.ModeAgent, 0, "after-stop"); !errors.Is(err, lease.ErrMaintenance) {
		t.Fatalf("new control after fence: %v", err)
	}
	data, err := os.ReadFile(fixture)
	if err != nil || string(data) != "retained profile fixture" {
		t.Fatalf("retained data changed: %q %v", data, err)
	}
}

func TestProjectStopFailsClosedOnLostInspectionAndStaleHumanOverride(t *testing.T) {
	t.Run("inspection unavailable", func(t *testing.T) {
		engine, docker, _ := stopEngine(t)
		docker.inspectFailure = true
		if err := engine.StopProject(context.Background(), probeTestWorkspace, ProjectStopOptions{DiscardPageState: true}); err == nil || docker.stopCalls != 0 {
			t.Fatalf("inspection failure allowed stop: %v", err)
		}
	})
	t.Run("stale force", func(t *testing.T) {
		engine, docker, rt := stopEngine(t)
		_, token, err := rt.leases.Acquire("holder", lease.ModeHuman, 0, "force-test")
		if err != nil {
			t.Fatal(err)
		}
		if err := engine.StopProject(context.Background(), probeTestWorkspace, ProjectStopOptions{DiscardPageState: true, Force: true, ExpectedGeneration: "stale"}); err == nil || docker.stopCalls != 0 {
			t.Fatalf("stale override allowed stop: %v", err)
		}
		if _, err := rt.leases.Authorize(token.Reveal(), lease.ModeHuman); err != nil {
			t.Fatalf("stale override revoked control: %v", err)
		}
	})
	t.Run("explicit force", func(t *testing.T) {
		engine, docker, rt := stopEngine(t)
		_, token, err := rt.leases.Acquire("holder", lease.ModeHuman, 0, "force-test")
		if err != nil {
			t.Fatal(err)
		}
		if err := engine.StopProject(context.Background(), probeTestWorkspace, ProjectStopOptions{DiscardPageState: true, Force: true, ExpectedGeneration: rt.observedGeneration(t)}); err != nil {
			t.Fatal(err)
		}
		if !docker.stopped {
			t.Fatal("explicit override did not stop services")
		}
		if _, err := rt.leases.Authorize(token.Reveal(), lease.ModeHuman); err == nil {
			t.Fatal("explicitly revoked lease stayed valid")
		}
	})
}

func TestProjectLifecycleWaitsForAnUnfinishedUpdate(t *testing.T) {
	for _, status := range []string{JournalRunning, JournalAwaitingApproval, JournalHeld} {
		t.Run(status, func(t *testing.T) {
			engine, docker, _ := stopEngine(t)
			journal, err := newJournal(engine.Dep, "update", "u-20261025T015900Z-bbbbbb", "plan-1", probeTestWorkspace, "r1", engine.Now)
			if err != nil {
				t.Fatal(err)
			}
			journal.Status = status
			if err := journal.save(); err != nil {
				t.Fatal(err)
			}
			if err := engine.CheckProjectStart(); err == nil {
				t.Fatal("unfinished update allowed a project restart")
			}
			if err := engine.StopProject(context.Background(), probeTestWorkspace, ProjectStopOptions{DiscardPageState: true}); err == nil || docker.stopCalls != 0 {
				t.Fatalf("unfinished update allowed stop: %v", err)
			}
			journal.Status = JournalCompleted
			if err := journal.save(); err != nil {
				t.Fatal(err)
			}
			if err := engine.CheckProjectStart(); err != nil {
				t.Fatalf("completed update still blocks restart: %v", err)
			}
		})
	}
	t.Run("corrupt journal", func(t *testing.T) {
		engine, docker, _ := stopEngine(t)
		journal, err := newJournal(engine.Dep, "update", "u-20261025T015900Z-bbbbbb", "plan-1", probeTestWorkspace, "r1", engine.Now)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(journal.dir, "journal.json"), []byte("{"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := engine.CheckProjectStart(); err == nil {
			t.Fatal("unreadable update journal allowed a restart")
		}
		if err := engine.StopProject(context.Background(), probeTestWorkspace, ProjectStopOptions{DiscardPageState: true}); err == nil || docker.stopCalls != 0 {
			t.Fatalf("unreadable update journal allowed stop: %v", err)
		}
	})
}
