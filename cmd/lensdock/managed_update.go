package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/hostmanager"
	"github.com/JiaWeiXie/lensdock/internal/update"
)

// A deployment with ManagedBy belongs to a host manager. Every mutation of it
// is admitted, queued and executed by that manager under its project lock. The
// CLI keeps the user interaction (flags, confirmations, progress output) and
// sends only flat, typed update actions. Read-only commands run the existing
// Engine on the manager's fixed Docker engine after the owner identity has
// been validated. A managed deployment never falls back to the standalone
// engine: if its owner is unavailable the command fails.

// managerAPI is the host manager surface the update commands use. Tests
// replace the package-level manager.
type managerAPI interface {
	LoadConfig(root string) (hostmanager.Config, error)
	EnsureRunning(ctx context.Context, root string) error
	Call(ctx context.Context, root, tokenFile string, req hostmanager.Request) (hostmanager.Response, error)
	AdminTokenFile(root string) string
	// Docker returns the fixed-engine Docker for dep. Read-only commands pass
	// no lifecycle locks.
	Docker(cfg hostmanager.Config, dep *update.Deployment) update.Docker
}

type realManager struct{}

func (realManager) LoadConfig(root string) (hostmanager.Config, error) {
	return hostmanager.LoadConfig(root)
}

func (realManager) EnsureRunning(ctx context.Context, root string) error {
	return hostmanager.EnsureRunning(ctx, root)
}

func (realManager) Call(ctx context.Context, root, tokenFile string, req hostmanager.Request) (hostmanager.Response, error) {
	return hostmanager.Call(ctx, root, tokenFile, req)
}

func (realManager) AdminTokenFile(root string) string { return hostmanager.AdminTokenFile(root) }

func (realManager) Docker(cfg hostmanager.Config, dep *update.Deployment) update.Docker {
	return hostmanager.DeploymentDocker(cfg, dep, nil)
}

var manager managerAPI = realManager{}

// Poll timing; tests shorten it.
var (
	// managedPollInterval separates status polls of a running operation.
	managedPollInterval = 2 * time.Second
	// managedCallTimeout bounds each status poll.
	managedCallTimeout = 30 * time.Second
	// managedPollFailures is the number of consecutive failed polls after which
	// the CLI stops waiting. The operation itself keeps running.
	managedPollFailures = 5
)

// managedOwner is a validated owner of a managed deployment.
type managedOwner struct {
	dep       *update.Deployment
	cfg       hostmanager.Config
	root      string
	projectID string
	// projectDir is the canonical <root>/projects/<id> directory.
	projectDir string
}

// resolveManagedOwner returns nil for a standalone deployment. For a managed
// one it loads the manager configuration recorded in deployment.json and fails
// closed unless the manager identity and the deployment location match.
func resolveManagedOwner(dep *update.Deployment) (*managedOwner, error) {
	m := dep.ManagedBy
	if m == nil {
		return nil, nil
	}
	fail := func(format string, args ...any) (*managedOwner, error) {
		return nil, fmt.Errorf("deployment %s is managed by host manager %q but its ownership is not valid: %s (managed deployments have no standalone fallback)", dep.File, m.ManagerID, fmt.Sprintf(format, args...))
	}
	if m.ManagerID == "" || m.ProjectID == "" || m.ProjectID == "." || m.ProjectID == ".." || m.ProjectID != filepath.Base(m.ProjectID) {
		return fail("manager or project ID is invalid")
	}
	if !filepath.IsAbs(m.ManagerRoot) || filepath.Clean(m.ManagerRoot) != m.ManagerRoot {
		return fail("manager root %q is not a canonical absolute path", m.ManagerRoot)
	}
	cfg, err := manager.LoadConfig(m.ManagerRoot)
	if err != nil {
		return nil, fmt.Errorf("deployment %s is managed by the host manager at %s, whose configuration cannot be loaded: %w (managed deployments have no standalone fallback)", dep.File, m.ManagerRoot, err)
	}
	if cfg.ID != m.ManagerID {
		return fail("the manager at %s has ID %q", m.ManagerRoot, cfg.ID)
	}
	if same, err := sameDirectory(cfg.Root, m.ManagerRoot); err != nil || !same {
		return fail("the manager configuration belongs to a different root")
	}
	projectDir, err := filepath.EvalSymlinks(filepath.Join(m.ManagerRoot, "projects", m.ProjectID))
	if err != nil {
		return fail("project directory: %v", err)
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(dep.File))
	if err != nil || dir != projectDir || filepath.Base(dep.File) != "deployment.json" {
		return fail("the file is not <manager root>/projects/%s/deployment.json", m.ProjectID)
	}
	if fi, err := os.Lstat(dep.File); err != nil || !fi.Mode().IsRegular() {
		return fail("deployment.json is not a regular file")
	}
	if same, err := sameDirectory(dep.StateDir, filepath.Join(projectDir, "product")); err != nil || !same {
		return fail("stateDir %s is not the project's product directory", dep.StateDir)
	}
	return &managedOwner{dep: dep, cfg: cfg, root: m.ManagerRoot, projectID: m.ProjectID, projectDir: projectDir}, nil
}

func sameDirectory(a, b string) (bool, error) {
	ca, err := filepath.EvalSymlinks(a)
	if err != nil {
		return false, err
	}
	cb, err := filepath.EvalSymlinks(b)
	if err != nil {
		return false, err
	}
	return ca == cb, nil
}

// engine returns the read-only Engine of the deployment on the manager's
// fixed Docker engine. Publishing the public status file is a mutation owned
// by the manager, so it is not available here.
func (o *managedOwner) engine(c *cli) *update.Engine {
	dep := *o.dep
	dep.PublicStatusFile = ""
	return update.NewEngine(&dep, manager.Docker(o.cfg, &dep), c.stderr)
}

// admit starts the manager if needed and verifies that it registered this
// project at this state directory. Only then may a mutation be submitted.
func (o *managedOwner) admit(ctx context.Context) error {
	if err := manager.EnsureRunning(ctx, o.root); err != nil {
		return fmt.Errorf("host manager %s is unavailable: %w (managed deployments have no standalone fallback)", o.cfg.ID, err)
	}
	resp, err := manager.Call(ctx, o.root, manager.AdminTokenFile(o.root), hostmanager.Request{Action: "status", ProjectID: o.projectID})
	if err != nil {
		return fmt.Errorf("host manager status for project %s: %w", o.projectID, err)
	}
	p := resp.Project
	if p == nil || p.ID != o.projectID {
		return fmt.Errorf("host manager does not report project %s", o.projectID)
	}
	if p.Revoked {
		return fmt.Errorf("project %s is revoked in the host manager", o.projectID)
	}
	if same, err := sameDirectory(p.StateDir, o.projectDir); err != nil || !same {
		return fmt.Errorf("host manager registered project %s at state directory %s, not %s", o.projectID, p.StateDir, o.projectDir)
	}
	return nil
}

// managedUpdate submits action to the owning manager and follows the
// operation until it ends. Interrupting the wait detaches; it never stops the
// update.
func (c *cli) managedUpdate(ctx context.Context, o *managedOwner, action hostmanager.UpdateAction) error {
	if err := o.admit(ctx); err != nil {
		return err
	}
	resp, err := manager.Call(ctx, o.root, manager.AdminTokenFile(o.root), hostmanager.Request{Action: "update", ProjectID: o.projectID, Update: &action})
	if err != nil {
		if resp.Operation != nil && resp.Operation.ID != "" {
			fmt.Fprintf(c.stderr, "update operation %s\n", resp.Operation.ID)
		}
		return fmt.Errorf("host manager did not accept update %s: %w", action.Kind, err)
	}
	if resp.Operation == nil || resp.Operation.ID == "" {
		return fmt.Errorf("host manager accepted update %s without an operation ID", action.Kind)
	}
	fmt.Fprintf(c.stderr, "update operation %s accepted by host manager %s (%s)\n", resp.Operation.ID, o.cfg.ID, resp.Operation.Phase)
	return c.awaitManagedOperation(ctx, o, *resp.Operation)
}

func awaitingApproval(op hostmanager.Operation) bool {
	return op.Phase == "awaiting-approval" || strings.Contains(op.Error, update.ErrAwaitingApproval.Error())
}

func (c *cli) awaitManagedOperation(ctx context.Context, o *managedOwner, op hostmanager.Operation) error {
	failures := 0
	for {
		if done, err := c.managedOutcome(ctx, o, op); done {
			return err
		}
		select {
		case <-ctx.Done():
			return c.detach(o, op, ctx.Err())
		case <-time.After(managedPollInterval):
		}
		callCtx, cancel := context.WithTimeout(ctx, managedCallTimeout)
		resp, err := manager.Call(callCtx, o.root, manager.AdminTokenFile(o.root), hostmanager.Request{Action: "status", ProjectID: o.projectID, OperationID: op.ID})
		cancel()
		if err == nil && (resp.Operation == nil || resp.Operation.ID != op.ID) {
			err = fmt.Errorf("host manager did not report operation %s", op.ID)
		}
		if err != nil {
			if ctx.Err() != nil {
				return c.detach(o, op, ctx.Err())
			}
			if failures++; failures >= managedPollFailures {
				return c.detach(o, op, fmt.Errorf("status polling failed %d times: %w", failures, err))
			}
			continue
		}
		failures = 0
		op = *resp.Operation
	}
}

// detach stops waiting. The operation keeps running in the manager.
func (c *cli) detach(o *managedOwner, op hostmanager.Operation, cause error) error {
	fmt.Fprintf(c.stderr, "no longer waiting for update operation %s; it continues in the host manager and is not stopped.\nInspect it with: %s\n", op.ID, o.command("status"))
	return fmt.Errorf("stopped waiting for update operation %s: %w", op.ID, cause)
}

// managedOutcome reports whether op ended and the command result.
func (c *cli) managedOutcome(ctx context.Context, o *managedOwner, op hostmanager.Operation) (bool, error) {
	if awaitingApproval(op) {
		c.reportAwaiting(ctx, o, op)
		return true, update.ErrAwaitingApproval
	}
	switch op.Phase {
	case "succeeded":
		return true, c.writeJSONLine(map[string]any{"operation": op.ID, "project": o.projectID, "kind": op.Kind, "phase": op.Phase})
	case "failed", "canceled", "blocked", "abandoned":
		msg := op.Error
		if msg == "" {
			msg = "no error was recorded"
		}
		return true, fmt.Errorf("update operation %s %s: %s", op.ID, op.Phase, msg)
	}
	return false, nil
}

// reportAwaiting explains how to finish an update that stopped before the
// switch. The old services stay stopped until it is approved or aborted.
func (c *cli) reportAwaiting(ctx context.Context, o *managedOwner, op hostmanager.Operation) {
	id := "ID"
	if st, err := o.engine(c).Status(ctx); err == nil {
		if last := st.Update.LastOperation; last != nil && last.Kind == "update" {
			id = last.ID
		}
	}
	fmt.Fprintf(c.stderr, "update operation %s is waiting for approval of the switch; the old services stay stopped.\nFinish it with: %s\nor undo it with: %s\n", op.ID, o.command("approve", "--update", id), o.command("abort", "--update", id))
}

// command is the shell command line that runs `lensdock update args...` on this
// deployment: it names the exact deployment file, so it never falls back to the
// standalone default. Guidance prints a command at the end of its line.
func (o *managedOwner) command(args ...string) string {
	words := append([]string{"lensdock", "update"}, args...)
	words = append(words, "--deployment", o.dep.File)
	for i, w := range words {
		words[i] = shellWord(w)
	}
	return strings.Join(words, " ")
}

// shellWord quotes s as one POSIX shell word. Text that needs no quoting is
// returned as is; anything else is single-quoted, the only form in which a
// shell interprets nothing, with each embedded quote written as '\”.
func shellWord(s string) string {
	if s != "" && strings.Trim(s, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_@%+=:,./-") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// managedComposeReadOnly lists the compose commands that cannot change the
// project. Everything else must go through typed project or update commands.
var managedComposeReadOnly = map[string]bool{"ps": true, "logs": true, "config": true}

func managedComposeGuidance(cmd string) string {
	switch cmd {
	case "up", "start", "restart":
		return "start the project with `lensdock project up`, or roll a new release out with `lensdock update apply`"
	case "down", "stop":
		return "stop the project with `lensdock project stop`"
	}
	return "only ps, logs and config are available"
}

// managedCompose runs a read-only compose command through the fixed engine
// runner with the deployment's recorded Compose environment.
func (c *cli) managedCompose(ctx context.Context, o *managedOwner, args []string) error {
	if len(args) == 0 {
		return usageErrorf("update compose: a compose command is required (ps, logs or config for a managed deployment)")
	}
	if !managedComposeReadOnly[args[0]] {
		return fmt.Errorf("update compose %s would change managed deployment %s through the manager's back door and is refused: %s", args[0], o.projectID, managedComposeGuidance(args[0]))
	}
	eng := o.engine(c)
	name, argv, err := eng.ComposeCommand(args)
	if err != nil {
		return err
	}
	if err := eng.Docker.Run.Run(ctx, update.Cmd{Name: name, Args: argv, Stdin: c.stdin, Stdout: c.stdout, Stderr: c.stderr}); err != nil {
		return fmt.Errorf("docker compose: %w", err)
	}
	return nil
}

// errManagedInit explains why update init does not apply to a managed deployment.
func errManagedInit(dep *update.Deployment) error {
	return fmt.Errorf("deployment %s is owned by host manager %q: its backup key and public status are created by the manager, so update init is refused (it is for standalone deployments)", dep.File, dep.ManagedBy.ManagerID)
}
