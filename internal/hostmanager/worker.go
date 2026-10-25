package hostmanager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/update"
)

// Worker receipt results.
const (
	ResultSucceeded        = "succeeded"
	ResultFailed           = "failed"
	ResultAwaitingApproval = "awaiting-approval"
)

// Lock waits. They are variables so tests can shorten them.
var (
	workerProjectLockWait = 2 * time.Minute
	workerHelperLockWait  = 15 * time.Minute
)

// manifest is the only input of a worker. The manager writes it before it
// commits the admission; the worker trusts it only after matching it against
// the registry.
type manifest struct {
	Version   int       `json:"version"`
	ManagerID string    `json:"managerId"`
	Root      string    `json:"root"`
	Operation Operation `json:"operation"`
	Project   Project   `json:"project"`
}

// begunMarker is durable before the first side effect. A worker that dies
// after writing it, without a receipt, blocks the project.
type begunMarker struct {
	OperationID string    `json:"operationId"`
	PID         int       `json:"pid"`
	StartedAt   time.Time `json:"startedAt"`
}

// receipt is written after Backend.Execute returns, independent of the
// registry. It carries no credentials: only the backend's error text.
type receipt struct {
	OperationID string    `json:"operationId"`
	Result      string    `json:"result"`
	Error       string    `json:"error,omitempty"`
	FinishedAt  time.Time `json:"finishedAt"`
}

func writeManifest(root, managerID string, op *opRecord) error {
	dir := opDir(root, op.ID)
	if err := ensurePrivateDir(dir); err != nil {
		return err
	}
	m := manifest{
		Version:   1,
		ManagerID: managerID,
		Root:      root,
		Operation: cloneOperation(op.Operation),
		Project:   op.Project,
	}
	return writeJSONAtomic(filepath.Join(dir, manifestName), m, 0o600)
}

func removeOpDir(root, id string) {
	if opPattern.MatchString(id) {
		_ = os.RemoveAll(opDir(root, id))
	}
}

func markerExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// readReceipt returns nil without an error when no receipt exists.
func readReceipt(root, opID string) (*receipt, error) {
	var r receipt
	err := readPrivateJSON(filepath.Join(opDir(root, opID), receiptName), 1<<20, &r)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if r.OperationID != opID {
		return nil, errors.New("receipt belongs to another operation")
	}
	return &r, nil
}

type probeResult struct {
	alive   bool
	begun   bool
	receipt *receipt
	err     error
}

// probeWorker inspects a worker's markers while holding the project lock, so
// no worker can start or finish between the checks. A busy lock means a
// worker or a surviving child is alive.
func probeWorker(root, projectID, opID string) probeResult {
	lock, busy, err := tryFlock(projectLockPath(root, projectID))
	if err != nil {
		return probeResult{err: err}
	}
	begun := markerExists(filepath.Join(opDir(root, opID), begunName))
	if busy {
		return probeResult{alive: true, begun: begun}
	}
	defer lock.Close()
	rcpt, _ := readReceipt(root, opID) // an unreadable receipt counts as none
	return probeResult{begun: begun, receipt: rcpt}
}

// childEnv is the fixed environment of manager-launched processes. The caller's
// PATH and Docker target variables never reach them; the backend selects the
// Docker target from the configuration.
func childEnv(cfg Config) []string {
	path := strings.Join([]string{filepath.Dir(cfg.DockerBinary), "/usr/local/bin", "/opt/homebrew/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin"}, ":")
	env := []string{"PATH=" + path}
	for _, key := range []string{"HOME", "USER", "LOGNAME", "TMPDIR", "LANG", "LC_ALL"} {
		if v := os.Getenv(key); v != "" {
			env = append(env, key+"="+v)
		}
	}
	return env
}

// spawnDetached starts the fixed manager executable in its own session so it
// outlives the caller and its context.
func spawnDetached(cfg Config, logPath string, args ...string) (*exec.Cmd, error) {
	logf, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open log: %w", err)
	}
	defer logf.Close()
	cmd := exec.Command(cfg.Executable, append([]string{"__manager"}, args...)...)
	cmd.Dir = "/"
	cmd.Env = childEnv(cfg)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", filepath.Base(cfg.Executable), err)
	}
	return cmd, nil
}

func spawnWorker(_ context.Context, cfg Config, opID string) error {
	if !opPattern.MatchString(opID) {
		return errors.New("invalid operation ID")
	}
	cmd, err := spawnDetached(cfg, filepath.Join(opDir(cfg.Root, opID), workerLogName), "worker", "--root", cfg.Root, "--operation", opID)
	if err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// AcquireHelper takes the global helper lock with the same path and wait as a
// worker. Callers that run a lifecycle helper outside a worker (read-only
// diagnostics) hold it for the helper's lifetime and pass the descriptor to
// the child so the lock outlives a killed caller.
func AcquireHelper(ctx context.Context, root string) (*os.File, error) {
	cfg, err := LoadConfig(root)
	if err != nil {
		return nil, err
	}
	return waitFlock(ctx, helperLockPath(cfg.Root), workerHelperLockWait)
}

func jsonEqual(a, b any) bool {
	x, err1 := json.Marshal(a)
	y, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && bytes.Equal(x, y)
}

// loadVerified reads the manifest and matches it with the registry: manager
// and root identity, canonical paths, the operation's current phase and the
// admission snapshot. It returns the manifest and registry copy.
func loadVerified(cfg Config, opID string) (manifest, *registry, error) {
	var m manifest
	if err := readPrivateJSON(filepath.Join(opDir(cfg.Root, opID), manifestName), 1<<20, &m); err != nil {
		return m, nil, fmt.Errorf("manifest: %w", err)
	}
	reg, err := loadRegistry(cfg.Root)
	if err != nil {
		return m, nil, err
	}
	if m.Version != 1 || m.ManagerID != cfg.ID || m.Root != cfg.Root {
		return m, nil, errors.New("manifest does not belong to this manager")
	}
	pid := m.Operation.ProjectID
	p := m.Project
	switch {
	case m.Operation.ID != opID || !projectPattern.MatchString(pid) || p.ID != pid:
		return m, nil, errors.New("manifest identifiers do not match the operation")
	case p.StateDir != projectStateDir(cfg.Root, pid), p.TokenFile != projectTokenFile(cfg.Root, pid),
		p.ComposeProject != "ld-"+pid, p.Workspace != pid:
		return m, nil, errors.New("manifest paths do not match the manager layout")
	}
	op, rec := reg.Ops[opID], reg.Projects[pid]
	if op == nil || rec == nil || op.ProjectID != pid {
		return m, nil, errors.New("the operation is not registered")
	}
	if !workerPhase(op.Phase) {
		return m, nil, fmt.Errorf("operation %s is %s, not current", opID, op.Phase)
	}
	a, b := cloneOperation(m.Operation), cloneOperation(op.Operation)
	a.Phase, b.Phase = "", ""
	if !jsonEqual(a, b) || !jsonEqual(m.Project, op.Project) {
		return m, nil, errors.New("manifest differs from the registered operation")
	}
	return m, reg, nil
}

func writeReceiptRetry(root string, r receipt) error {
	var err error
	for range 3 {
		if err = writeJSONAtomic(filepath.Join(opDir(root, r.OperationID), receiptName), r, 0o600); err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return err
}

// RunWorker executes one manager-admitted operation. It holds the project
// lifecycle lock and the global helper lock for the whole backend call; the
// backend passes both descriptors to every child process so the locks outlive
// a killed worker. A duplicate worker never repeats work: under the lock it
// finds the receipt (and exits) or the begun marker (and refuses to replay).
func RunWorker(ctx context.Context, root, operationID string, backend Backend) error {
	if !opPattern.MatchString(operationID) {
		return errors.New("invalid operation ID")
	}
	cfg, err := LoadConfig(root)
	if err != nil {
		return err
	}
	first, _, err := loadVerified(cfg, operationID)
	if err != nil {
		return err
	}
	pid := first.Operation.ProjectID
	projectLock, err := waitFlock(ctx, projectLockPath(cfg.Root, pid), workerProjectLockWait)
	if err != nil {
		return fmt.Errorf("project lifecycle lock: %w", err)
	}
	defer projectLock.Close()
	helperLock, err := waitFlock(ctx, helperLockPath(cfg.Root), workerHelperLockWait)
	if err != nil {
		return fmt.Errorf("helper lock: %w", err)
	}
	defer helperLock.Close()

	// Everything below runs under both locks.
	// A worker can wait behind another project before taking the helper slot.
	// Policy changes and helper execution share that lock, so reload here.
	cfg, err = LoadConfig(cfg.Root)
	if err != nil {
		return err
	}
	dir := opDir(cfg.Root, operationID)
	if rcpt, _ := readReceipt(cfg.Root, operationID); rcpt != nil {
		return nil
	}
	if markerExists(filepath.Join(dir, begunName)) {
		return fmt.Errorf("operation %s already began without a receipt; refusing to replay it", operationID)
	}
	m, reg, err := loadVerified(cfg, operationID)
	if err != nil {
		return err
	}
	rec := reg.Projects[pid]
	fail := func(msg string) error {
		if err := writeReceiptRetry(cfg.Root, receipt{OperationID: operationID, Result: ResultFailed, Error: msg, FinishedAt: time.Now().UTC()}); err != nil {
			return err
		}
		return errors.New(msg)
	}
	// No side effect has happened yet: refuse new work for revoked or moved
	// projects with a receipt instead of acting.
	if m.Operation.Kind == KindStart {
		if rec.Revoked {
			return fail("the project grant was revoked")
		}
		if err := checkDirectory(rec); err != nil {
			return fail(err.Error())
		}
	}
	begun := begunMarker{OperationID: operationID, PID: os.Getpid(), StartedAt: time.Now().UTC()}
	if err := writeJSONAtomic(filepath.Join(dir, begunName), begun, 0o600); err != nil {
		return fmt.Errorf("record begun marker: %w", err)
	}
	op := m.Operation
	op.Phase = PhaseRunning
	execErr := executeRecovered(ctx, backend, cfg, m.Project, op, []*os.File{projectLock, helperLock})

	r := receipt{OperationID: operationID, Result: ResultSucceeded, FinishedAt: time.Now().UTC()}
	switch {
	case execErr == nil:
	case errors.Is(execErr, update.ErrAwaitingApproval):
		r.Result, r.Error = ResultAwaitingApproval, truncate(execErr.Error(), 2048)
	default:
		r.Result, r.Error = ResultFailed, truncate(execErr.Error(), 2048)
	}
	if err := writeReceiptRetry(cfg.Root, r); err != nil {
		slog.Error("write receipt", "operation", operationID, "error", err)
		return fmt.Errorf("write receipt: %w", err)
	}
	if r.Result == ResultAwaitingApproval {
		return nil
	}
	return execErr
}

func executeRecovered(ctx context.Context, backend Backend, cfg Config, p Project, op Operation, locks []*os.File) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("backend panicked: %v", v)
		}
	}()
	return backend.Execute(ctx, cfg, p, op, locks)
}
