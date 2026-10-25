package hostmanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/update"
)

func shortLockWaits(t *testing.T) {
	t.Helper()
	project, helper := workerProjectLockWait, workerHelperLockWait
	workerProjectLockWait, workerHelperLockWait = 200*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { workerProjectLockWait, workerHelperLockWait = project, helper })
}

// admittedStart queues and admits a start without running its worker.
func admittedStart(t *testing.T, env *testEnv) (Project, string) {
	t.Helper()
	env.runInline = false
	p := env.authorize()
	r := env.ensure(p)
	env.pass()
	if env.op(r.Operation.ID).Phase != PhaseAdmitted {
		t.Fatalf("expected an admitted start, got %s", env.op(r.Operation.ID).Phase)
	}
	return p, r.Operation.ID
}

func writeBegun(t *testing.T, env *testEnv, opID string) {
	t.Helper()
	m := begunMarker{OperationID: opID, PID: 1, StartedAt: env.clock.Now()}
	if err := writeJSONAtomic(filepath.Join(opDir(env.cfg.Root, opID), begunName), m, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerRunsOnceAndDuplicatesNeverRepeat(t *testing.T) {
	shortLockWaits(t)
	env := newEnv(t, testPolicy(2))
	_, opID := admittedStart(t, env)
	for i := range 2 {
		if err := RunWorker(context.Background(), env.cfg.Root, opID, env.backend); err != nil {
			t.Fatalf("worker %d: %v", i, err)
		}
	}
	if got := env.backend.executions(); len(got) != 1 {
		t.Fatalf("a duplicate worker must find the receipt and not repeat: %v", got)
	}
	env.pass()
	if env.op(opID).Phase != PhaseSucceeded {
		t.Fatalf("the receipt completes the operation, got %s", env.op(opID).Phase)
	}
	if err := RunWorker(context.Background(), env.cfg.Root, opID, env.backend); err == nil {
		t.Fatal("a worker for a finished operation must refuse")
	}
	if len(env.backend.executions()) != 1 {
		t.Fatal("a finished operation never executes again")
	}
}

func TestWorkerRefusesToReplayAfterBegunMarker(t *testing.T) {
	shortLockWaits(t)
	env := newEnv(t, testPolicy(2))
	_, opID := admittedStart(t, env)
	writeBegun(t, env, opID)
	err := RunWorker(context.Background(), env.cfg.Root, opID, env.backend)
	if err == nil || !strings.Contains(err.Error(), "refusing to replay") {
		t.Fatalf("expected a replay refusal, got %v", err)
	}
	if len(env.backend.executions()) != 0 {
		t.Fatal("no side effect may repeat")
	}
	if rcpt, _ := readReceipt(env.cfg.Root, opID); rcpt != nil {
		t.Fatal("a refused replay must not invent a receipt")
	}
}

func TestWorkerHoldsBothLocksFromDurableMarkerUntilReceipt(t *testing.T) {
	shortLockWaits(t)
	env := newEnv(t, testPolicy(2))
	p, opID := admittedStart(t, env)
	var sawLocks, sawBegun bool
	env.backend.execFn = func(_ context.Context, cfg Config, _ Project, _ Operation, locks []*os.File) error {
		_, projectBusy, _ := tryFlock(projectLockPath(cfg.Root, p.ID))
		_, helperBusy, _ := tryFlock(helperLockPath(cfg.Root))
		sawLocks = len(locks) == 2 && projectBusy && helperBusy
		sawBegun = markerExists(filepath.Join(opDir(cfg.Root, opID), begunName)) &&
			!markerExists(filepath.Join(opDir(cfg.Root, opID), receiptName))
		return nil
	}
	if err := RunWorker(context.Background(), env.cfg.Root, opID, env.backend); err != nil {
		t.Fatal(err)
	}
	if !sawLocks || !sawBegun {
		t.Fatalf("Execute must run under both locks with a begun marker and no receipt (locks=%v begun=%v)", sawLocks, sawBegun)
	}
	for _, path := range []string{projectLockPath(env.cfg.Root, p.ID), helperLockPath(env.cfg.Root)} {
		f, busy, err := tryFlock(path)
		if err != nil || busy {
			t.Fatalf("%s must be free after the worker ended (busy=%v err=%v)", filepath.Base(path), busy, err)
		}
		f.Close()
	}
}

func TestWorkerWaitsForTheProjectLockWithoutActing(t *testing.T) {
	shortLockWaits(t)
	env := newEnv(t, testPolicy(2))
	p, opID := admittedStart(t, env)
	held, busy, err := tryFlock(projectLockPath(env.cfg.Root, p.ID))
	if err != nil || busy {
		t.Fatal("test setup could not take the lock")
	}
	defer held.Close()
	err = RunWorker(context.Background(), env.cfg.Root, opID, env.backend)
	if err == nil || !strings.Contains(err.Error(), "lifecycle lock") {
		t.Fatalf("expected a lock failure, got %v", err)
	}
	if markerExists(filepath.Join(opDir(env.cfg.Root, opID), begunName)) || len(env.backend.executions()) != 0 {
		t.Fatal("a worker without the lock must do nothing")
	}
}

func TestWorkerRejectsForeignAndTamperedManifests(t *testing.T) {
	shortLockWaits(t)
	env := newEnv(t, testPolicy(2))
	_, opID := admittedStart(t, env)
	path := filepath.Join(opDir(env.cfg.Root, opID), manifestName)
	original := mustRead(t, path)
	restore := func() {
		if err := writeFileAtomic(path, original, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tamper := map[string]func(*manifest){
		"manager":   func(m *manifest) { m.ManagerID = "mgr-other" },
		"root":      func(m *manifest) { m.Root = "/tmp/elsewhere" },
		"directory": func(m *manifest) { m.Project.Root = "/tmp/elsewhere" },
		"limits":    func(m *manifest) { m.Project.Limits.Runtime.MemoryBytes *= 4 },
		"state dir": func(m *manifest) { m.Project.StateDir = "/tmp/elsewhere" },
		"kind":      func(m *manifest) { m.Operation.Kind = KindStop },
	}
	for name, mutate := range tamper {
		var m manifest
		if err := readPrivateJSON(path, 1<<20, &m); err != nil {
			t.Fatal(err)
		}
		mutate(&m)
		if err := writeJSONAtomic(path, m, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := RunWorker(context.Background(), env.cfg.Root, opID, env.backend); err == nil {
			t.Fatalf("a tampered %s must be refused", name)
		}
		restore()
	}
	// A manifest for an operation the registry never admitted is not trusted.
	forged := "op-" + strings.Repeat("0", 16)
	if err := os.Mkdir(opDir(env.cfg.Root, forged), 0o700); err != nil {
		t.Fatal(err)
	}
	var m manifest
	if err := readPrivateJSON(path, 1<<20, &m); err != nil {
		t.Fatal(err)
	}
	m.Operation.ID = forged
	if err := writeJSONAtomic(filepath.Join(opDir(env.cfg.Root, forged), manifestName), m, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RunWorker(context.Background(), env.cfg.Root, forged, env.backend); err == nil {
		t.Fatal("an unregistered operation must be refused")
	}
	if err := RunWorker(context.Background(), env.cfg.Root, "../../etc", env.backend); err == nil {
		t.Fatal("an invalid operation ID must be refused")
	}
	if len(env.backend.executions()) != 0 {
		t.Fatalf("nothing may execute: %v", env.backend.executions())
	}
	if err := RunWorker(context.Background(), env.cfg.Root, opID, env.backend); err != nil {
		t.Fatalf("the untouched manifest still runs: %v", err)
	}
}

func TestWorkerRefusesReplacedDirectoryBeforeAnySideEffect(t *testing.T) {
	shortLockWaits(t)
	env := newEnv(t, testPolicy(2))
	dir := filepath.Join(t.TempDir(), "proj")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	env.runInline = false
	p := env.authorizeDir(dir)
	r := env.ensure(p)
	env.pass()
	if err := os.Rename(dir, dir+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	err := RunWorker(context.Background(), env.cfg.Root, r.Operation.ID, env.backend)
	if err == nil || !strings.Contains(err.Error(), "replaced") {
		t.Fatalf("expected a replaced-directory refusal, got %v", err)
	}
	if len(env.backend.executions()) != 0 || markerExists(filepath.Join(opDir(env.cfg.Root, r.Operation.ID), begunName)) {
		t.Fatal("no side effect may precede the identity check")
	}
	env.pass()
	if op := env.op(r.Operation.ID); op.Phase != PhaseFailed || !strings.Contains(op.Error, "replaced") {
		t.Fatalf("the refusal is a failed receipt: %+v", op)
	}
	if got := env.status(p); got.Reserved || got.State != StateStopped {
		t.Fatalf("an unstarted project must release its reservation: %+v", got)
	}
}

func TestWorkerReceiptsReportFailureAndAwaitingApproval(t *testing.T) {
	shortLockWaits(t)
	env := newEnv(t, testPolicy(2))
	p := env.authorize()
	env.backend.execFn = func(context.Context, Config, Project, Operation, []*os.File) error {
		return errors.New("boom")
	}
	r := env.ensure(p)
	env.pass()
	env.pass()
	if op := env.op(r.Operation.ID); op.Phase != PhaseFailed || op.Error != "boom" {
		t.Fatalf("a failed execution must be reported: %+v", op)
	}
	if got := env.status(p); got.Reserved || got.State == StateBlocked {
		t.Fatalf("a failed start that left nothing running releases capacity: %+v", got)
	}

	env.backend.execFn = nil
	settle(env, p)
	env.backend.execFn = func(_ context.Context, _ Config, _ Project, op Operation, _ []*os.File) error {
		return fmt.Errorf("apply: %w", update.ErrAwaitingApproval)
	}
	upd := env.mustCall(admin, Request{Action: ActionUpdate, ProjectID: p.ID, Update: &UpdateAction{Kind: UpdateApply, PlanFile: "/plan.json", Confirmed: true}})
	env.pass()
	env.pass()
	op := env.op(upd.Operation.ID)
	if op.Phase != PhaseAwaitingApproval || !strings.Contains(op.Error, "waiting for approval") {
		t.Fatalf("awaiting approval is its own terminal phase: %+v", op)
	}
	if got := env.status(p); got.State != StateRunning {
		t.Fatalf("awaiting approval does not block the project: %+v", got)
	}
}

func TestRestartKeepsReservationAndBlocksUnfinishedWorkerUntilRecovered(t *testing.T) {
	env := newEnv(t, testPolicy(1))
	env.runInline = false
	p, q := env.authorize(), env.authorize()
	r := env.ensure(p)
	env.pass()
	opID := r.Operation.ID

	env.srv = env.startServer() // the daemon restarts
	if !env.status(p).Reserved {
		t.Fatal("a restart must not free the reservation")
	}
	env.pass()
	if len(env.launchedOps()) != 1 {
		t.Fatal("a worker launched moments ago is not relaunched")
	}
	env.clock.Advance(20 * time.Second)
	env.pass()
	if got := env.launchedOps(); len(got) != 2 || got[1] != opID {
		t.Fatalf("an admitted operation without a begun marker is retried as the same operation: %v", got)
	}

	writeBegun(t, env, opID) // the worker died after side effects began
	env.pass()
	if env.op(opID).Phase != PhaseBlocked {
		t.Fatalf("an unfinished worker must block the operation, got %s", env.op(opID).Phase)
	}
	if got := env.status(p); got.State != StateBlocked || !got.Reserved {
		t.Fatalf("the project is blocked and keeps its reservation: %+v", got)
	}
	env.ensure(q)
	env.pass()
	if got := env.status(q); got.State != StateQueued {
		t.Fatalf("a blocked project's capacity must not be reused: %+v", got)
	}
	_, err := env.call(principal{projectID: p.ID}, Request{Action: ActionEnsure})
	wantStatus(t, err, http.StatusConflict)
	_, err = env.call(principal{projectID: p.ID}, Request{Action: ActionStop, DiscardPageState: true})
	wantStatus(t, err, http.StatusConflict)

	// Recovery never overlaps a live lock holder.
	lock, busy, err := tryFlock(projectLockPath(env.cfg.Root, p.ID))
	if err != nil || busy {
		t.Fatal("test setup could not take the lock")
	}
	_, err = env.call(admin, Request{Action: ActionRecover, ProjectID: p.ID, OperationID: opID})
	wantStatus(t, err, http.StatusConflict)
	lock.Close()
	_, err = env.call(admin, Request{Action: ActionRecover, ProjectID: q.ID, OperationID: opID})
	wantStatus(t, err, http.StatusNotFound)
	resp := env.mustCall(admin, Request{Action: ActionRecover, ProjectID: p.ID, OperationID: opID})
	if resp.Operation.Phase != PhaseAbandoned || !resp.Project.Reserved {
		t.Fatalf("recovery abandons the operation but keeps accounting until inspected: %+v", resp)
	}
	env.pass()
	if got := env.status(p); got.Reserved || got.State != StateStopped {
		t.Fatalf("a positive inspection releases the reservation: %+v", got)
	}
	if env.op(env.status(q).OperationID).Phase != PhaseAdmitted {
		t.Fatal("the queued project takes the freed capacity")
	}
}

func TestRegistryRecoversAdmittedOperationWithoutManifest(t *testing.T) {
	env := newEnv(t, testPolicy(2))
	env.runInline = false
	p := env.authorize()
	r := env.ensure(p)
	env.pass()
	opID := r.Operation.ID
	// Crash window: the registry committed the admission, the manifest is gone.
	if err := os.Remove(filepath.Join(opDir(env.cfg.Root, opID), manifestName)); err != nil {
		t.Fatal(err)
	}
	env.srv = env.startServer()
	env.clock.Advance(20 * time.Second)
	env.pass() // relaunch rewrites the manifest before the worker starts
	if _, err := os.Stat(filepath.Join(opDir(env.cfg.Root, opID), manifestName)); err != nil {
		t.Fatalf("the manifest must be rebuilt from the registry: %v", err)
	}
	shortLockWaits(t)
	if err := RunWorker(context.Background(), env.cfg.Root, opID, env.backend); err != nil {
		t.Fatal(err)
	}
	if len(env.backend.executions()) != 1 {
		t.Fatal("the recovered operation runs exactly once")
	}
}

// startSubprocessWorker runs the real detached-worker entry point of the test
// binary against an admitted operation.
func startSubprocessWorker(t *testing.T, root, opID string) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "__manager", "worker", "--root", root, "--operation", opID)
	cmd.Env = append(os.Environ(), "LDTEST_MODE=child")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func lockBusy(t *testing.T, path string) bool {
	t.Helper()
	f, busy, err := tryFlock(path)
	if err != nil {
		t.Fatal(err)
	}
	if f != nil {
		f.Close()
	}
	return busy
}

func TestSurvivingChildKeepsLocksAfterTheWorkerIsKilled(t *testing.T) {
	env := newEnv(t, testPolicy(1))
	p, opID := admittedStart(t, env)
	root := env.cfg.Root
	worker := startSubprocessWorker(t, root, opID)

	pidFile := filepath.Join(root, "child.pid")
	var childPID int
	waitFor(t, "the child process", func() bool {
		data, err := os.ReadFile(pidFile)
		if err != nil {
			return false
		}
		childPID, err = strconv.Atoi(strings.TrimSpace(string(data)))
		return err == nil
	})
	t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })
	if !lockBusy(t, projectLockPath(root, p.ID)) || !lockBusy(t, helperLockPath(root)) {
		t.Fatal("the worker must hold both locks while Execute runs")
	}
	env.pass()
	if env.op(opID).Phase != PhaseRunning {
		t.Fatalf("a live worker with a begun marker is running, got %s", env.op(opID).Phase)
	}

	if err := worker.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = worker.Wait()
	if !lockBusy(t, projectLockPath(root, p.ID)) || !lockBusy(t, helperLockPath(root)) {
		t.Fatal("a surviving child must keep both locks after its worker died")
	}
	env.pass()
	if env.op(opID).Phase != PhaseRunning {
		t.Fatalf("a held lock means the operation is still alive, got %s", env.op(opID).Phase)
	}
	if _, err := env.call(admin, Request{Action: ActionRecover, ProjectID: p.ID, OperationID: opID}); statusCode(err) != http.StatusConflict {
		t.Fatalf("recovery must refuse while the child holds the lock, got %v", err)
	}
	// A replacement worker cannot overlap the surviving child.
	shortLockWaits(t)
	if err := RunWorker(context.Background(), root, opID, env.backend); err == nil {
		t.Fatal("a replacement worker must not overlap the surviving child")
	}

	if err := syscall.Kill(childPID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the child's locks to release", func() bool { return !lockBusy(t, projectLockPath(root, p.ID)) })
	env.pass()
	if got := env.op(opID); got.Phase != PhaseBlocked {
		t.Fatalf("a dead worker without a receipt blocks the project, got %s", got.Phase)
	}
	if got := env.status(p); got.State != StateBlocked || !got.Reserved {
		t.Fatalf("the blocked project keeps its reservation: %+v", got)
	}
}

func daemonPID(t *testing.T, cfg Config) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://lensdock-manager/v1/identity", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := unixClient(socketPath(cfg.Root)).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var id identityResponse
	if err := json.NewDecoder(resp.Body).Decode(&id); err != nil {
		t.Fatal(err)
	}
	return id.PID
}

func TestEnsureRunningLaunchesOneIdentityCheckedDaemon(t *testing.T) {
	cfg := initTestManager(t, DefaultPolicy())
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			errs <- EnsureRunning(ctx, cfg.Root)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	pid := daemonPID(t, cfg)
	t.Cleanup(func() {
		_ = syscall.Kill(pid, syscall.SIGTERM)
		waitFor(t, "the daemon to exit", func() bool { return syscall.Kill(pid, 0) != nil })
	})
	if err := EnsureRunning(context.Background(), cfg.Root); err != nil || daemonPID(t, cfg) != pid {
		t.Fatalf("a running manager must be reused: %v", err)
	}
	if err := Serve(context.Background(), cfg.Root, subprocessBackend{}); err == nil || !strings.Contains(err.Error(), "already serves") {
		t.Fatalf("the singleton lock must refuse a second manager, got %v", err)
	}

	dir := t.TempDir()
	adminFile := AdminTokenFile(cfg.Root)
	resp, err := Call(context.Background(), cfg.Root, adminFile, Request{Action: ActionAuthorize, Path: dir})
	if err != nil || resp.Project == nil {
		t.Fatalf("authorize over the socket: %v", err)
	}
	got, err := ResolveProject(cfg.Root, dir)
	if err != nil || got.ID != resp.Project.ID {
		t.Fatalf("resolve after authorize: %v %+v", err, got)
	}
	status, err := Call(context.Background(), cfg.Root, resp.Project.TokenFile, Request{Action: ActionStatus})
	if err != nil || status.Project.ID != resp.Project.ID {
		t.Fatalf("project status over the socket: %v", err)
	}
	_, err = Call(context.Background(), cfg.Root, resp.Project.TokenFile, Request{Action: ActionList})
	var ce *CallError
	if !errors.As(err, &ce) || ce.Status != http.StatusForbidden {
		t.Fatalf("a project credential must not list projects: %v", err)
	}
	wrong := filepath.Join(t.TempDir(), "wrong.token")
	if err := os.WriteFile(wrong, []byte("nope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Call(context.Background(), cfg.Root, wrong, Request{Action: ActionList}); !errors.As(err, &ce) || ce.Status != http.StatusUnauthorized {
		t.Fatalf("a wrong credential must be unauthorized: %v", err)
	}
	open := filepath.Join(t.TempDir(), "open.token")
	if err := os.WriteFile(open, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Call(context.Background(), cfg.Root, open, Request{Action: ActionList}); err == nil {
		t.Fatal("a credential file readable by others must be refused")
	}
}

func TestForeignEndpointIsNotTheManager(t *testing.T) {
	cfg := initTestManager(t, DefaultPolicy())
	if err := ensurePrivateDir(filepath.Dir(socketPath(cfg.Root))); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", socketPath(cfg.Root))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/identity", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, identityResponse{Protocol: protocolVersion, ManagerID: "mgr-impostor", Root: cfg.Root, PID: 1})
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	if identityOK(context.Background(), cfg) {
		t.Fatal("an endpoint with another manager identity must be rejected")
	}
}
