package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/JiaWeiXie/lensdock/internal/update"
)

// updateUsageText is appended to usageText by the CLI dispatcher integration.
const updateUsageText = `  lensdock update init [--deployment FILE] [--state-dir DIR] [--compose-file FILE] [--linux-cli-dir DIR]
  lensdock update check [--deployment FILE] [--workspace ID]
  lensdock update plan --release ID|DIR [--workspace ID] [--output FILE] [--allow-dev] [--allow-downgrade]
  lensdock update apply --plan FILE [--yes] [--approve-switch] [--revoke-leases] [--accept-crash-snapshot] [--dry-run]
  lensdock update approve --update ID [--yes]
  lensdock update abort --update ID [--keep-candidate]
  lensdock update rollback --checkpoint ID [--yes] [--revoke-leases] [--allow-crash-checkpoint] [--dry-run]
  lensdock update status [--publish]
  lensdock update checkpoints list|verify|prune
  lensdock update branches list | delete VOLUME [--yes]
  lensdock update compose [--deployment FILE] -- up|down|stop|start|restart|ps|logs|config [ARGS...]
`

const (
	envDeployment = "LENSDOCK_DEPLOYMENT"
	envStateDir   = "LENSDOCK_STATE_DIR"
)

// updateSubcommands is the CLI dispatch seam owned by cmd/lensdock/main.go.
func (c *cli) updateSubcommands(ctx context.Context) map[string]func([]string) error {
	return map[string]func([]string) error{
		"init":        func(a []string) error { return c.updateInit(ctx, a) },
		"check":       func(a []string) error { return c.updateCheck(ctx, a) },
		"plan":        func(a []string) error { return c.updatePlan(ctx, a) },
		"apply":       func(a []string) error { return c.updateApply(ctx, a) },
		"approve":     func(a []string) error { return c.updateApprove(ctx, a) },
		"abort":       func(a []string) error { return c.updateAbort(ctx, a) },
		"rollback":    func(a []string) error { return c.updateRollback(ctx, a) },
		"status":      func(a []string) error { return c.updateStatus(ctx, a) },
		"checkpoints": func(a []string) error { return c.updateCheckpoints(ctx, a) },
		"branches":    func(a []string) error { return c.updateBranches(ctx, a) },
		"compose":     func(a []string) error { return c.updateCompose(ctx, a) },
		"__probe":     func(a []string) error { return c.updateProbe(ctx, a) },
	}
}

type deploymentFlags struct {
	deployment *string
	stateDir   *string
}

func (c *cli) addDeploymentFlags(fs *flag.FlagSet) deploymentFlags {
	stateDefault := c.defaultStateDir()
	depDefault := c.getenv(envDeployment)
	return deploymentFlags{
		deployment: fs.String("deployment", depDefault, "deployment.json (default $LENSDOCK_DEPLOYMENT or <state-dir>/deployment.json)"),
		stateDir:   fs.String("state-dir", stateDefault, "state directory used to find deployment.json"),
	}
}

func (f deploymentFlags) path() (string, error) {
	p := *f.deployment
	if p == "" {
		p = filepath.Join(*f.stateDir, "deployment.json")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return abs, nil
}

func (c *cli) engine(fs *flag.FlagSet, args []string, min, max int) (*update.Engine, []string, error) {
	df := c.addDeploymentFlags(fs)
	pos, err := parse(fs, args, min, max)
	if err != nil {
		return nil, nil, err
	}
	path, err := df.path()
	if err != nil {
		return nil, nil, err
	}
	dep, err := update.LoadDeployment(path)
	if err != nil {
		return nil, nil, err
	}
	return update.NewEngine(dep, c.deploymentDocker(dep), c.stderr), pos, nil
}

func (c *cli) updateInit(ctx context.Context, args []string) error {
	fs := c.newFlagSet("update init")
	df := c.addDeploymentFlags(fs)
	compose := fs.String("compose-file", "docker/browser/compose.yaml", "M0 compose file for a new deployment")
	linuxCLI := fs.String("linux-cli-dir", "bin", "directory containing linux-amd64/lensdock and linux-arm64/lensdock")
	public := fs.String("public-status-file", "", "sanitized deployment status file the Gateway mounts read-only")
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	depFile, err := df.path()
	if err != nil {
		return err
	}
	var dep *update.Deployment
	if _, err := os.Lstat(depFile); err == nil {
		dep, err = update.LoadDeployment(depFile)
		if err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	} else {
		stateDir, err := filepath.Abs(*df.stateDir)
		if err != nil {
			return err
		}
		composeFile, err := filepath.Abs(*compose)
		if err != nil {
			return err
		}
		cliDir, err := filepath.Abs(*linuxCLI)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(depFile), 0o700); err != nil {
			return err
		}
		dep = update.ScaffoldM0(depFile, stateDir, composeFile, cliDir)
		if *public != "" {
			p, err := filepath.Abs(*public)
			if err != nil {
				return err
			}
			dep.PublicStatusFile = p
		}
		if err := dep.Validate(); err != nil {
			return err
		}
		if err := dep.Save(); err != nil {
			return err
		}
		fmt.Fprintf(c.stdout, "created %s (M0 runtime deployment; edit it for the product Gateway compose)\n", depFile)
	}
	id, created, err := update.InitDeployment(dep)
	if err != nil {
		return err
	}
	if created {
		fmt.Fprintf(c.stdout, "created backup key %s (key id %s; keep it outside backups and protect it)\n", dep.BackupKeyFile, id)
	} else {
		fmt.Fprintf(c.stdout, "backup key exists (key id %s)\n", id)
	}
	eng := update.NewEngine(dep, c.deploymentDocker(dep), c.stderr)
	if err := eng.PublishStatus(ctx); err != nil {
		fmt.Fprintf(c.stderr, "warning: publish status: %v\n", err)
	}
	return nil
}

func (c *cli) updateCheck(ctx context.Context, args []string) error {
	fs := c.newFlagSet("update check")
	workspace := fs.String("workspace", "default", "workspace id")
	eng, _, err := c.engine(fs, args, 0, 0)
	if err != nil {
		return err
	}
	status, err := eng.PublishCandidates(ctx, *workspace)
	if err != nil {
		return err
	}
	return c.writeJSONLine(status)
}

func (c *cli) updatePlan(ctx context.Context, args []string) error {
	fs := c.newFlagSet("update plan")
	release := fs.String("release", "", "release id or release directory")
	workspace := fs.String("workspace", "default", "workspace id")
	output := fs.String("output", "", "applicable plan file (default <state>/plans/<id>.json)")
	allowDev := fs.Bool("allow-dev", false, "admit a dev-channel release")
	allowDown := fs.Bool("allow-downgrade", false, "admit Chromium/schema downgrade (still uses a paired checkpoint)")
	eng, _, err := c.engine(fs, args, 0, 0)
	if err != nil {
		return err
	}
	if *release == "" {
		return usageErrorf("update plan: --release is required")
	}
	plan, err := eng.BuildPlan(ctx, update.PlanRequest{Release: *release, Workspace: *workspace, AllowDev: *allowDev, AllowDowngrade: *allowDown})
	if err != nil {
		return err
	}
	if err := c.writeJSONLine(plan); err != nil {
		return err
	}
	if len(plan.Blockers) > 0 {
		return fmt.Errorf("plan has %d blocker(s); no applicable plan file was written", len(plan.Blockers))
	}
	path, err := eng.SavePlan(plan, *output)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.stderr, "applicable plan: %s\n", path)
	if err := eng.PublishStatus(ctx); err != nil {
		fmt.Fprintf(c.stderr, "warning: publish status: %v\n", err)
	}
	return nil
}

func (c *cli) updateApply(ctx context.Context, args []string) error {
	fs := c.newFlagSet("update apply")
	plan := fs.String("plan", "", "plan file")
	yes := fs.Bool("yes", false, "confirm entering maintenance without a prompt")
	approve := fs.Bool("approve-switch", false, "also approve the switch after candidate verification")
	revoke := fs.Bool("revoke-leases", false, "revoke a lease that does not drain before the timeout")
	crash := fs.Bool("accept-crash-snapshot", false, "continue with a crash snapshot when clean stop checks fail")
	dry := fs.Bool("dry-run", false, "re-verify the plan and print the steps without changing anything")
	eng, _, err := c.engine(fs, args, 0, 0)
	if err != nil {
		return err
	}
	if *plan == "" {
		return usageErrorf("update apply: --plan is required")
	}
	confirmed := *yes || *dry
	if !confirmed {
		confirmed, err = c.confirm("Enter maintenance, stop the workspace and create the checkpoint?", false)
		if err != nil || !confirmed {
			if err != nil {
				return err
			}
			return update.ErrNotConfirmed
		}
	}
	var switchApproval func(string) (bool, error)
	if !*approve && !*dry {
		switchApproval = func(summary string) (bool, error) {
			fmt.Fprint(c.stdout, summary)
			return c.confirm("Approve the switch to the verified candidate?", false)
		}
	}
	res, err := eng.Apply(ctx, update.ApplyOptions{
		PlanFile: *plan, DryRun: *dry, Confirmed: confirmed, ApproveSwitch: *approve,
		SwitchApproval: switchApproval, RevokeLeases: *revoke, AcceptCrashSnapshot: *crash,
	})
	if res != nil {
		for _, line := range res.Lines {
			fmt.Fprintln(c.stdout, line)
		}
		_ = c.writeJSONLine(res)
	}
	return err
}

func (c *cli) updateApprove(ctx context.Context, args []string) error {
	fs := c.newFlagSet("update approve")
	id := fs.String("update", "", "update id")
	yes := fs.Bool("yes", false, "confirm the switch without a prompt")
	eng, _, err := c.engine(fs, args, 0, 0)
	if err != nil {
		return err
	}
	if *id == "" {
		return usageErrorf("update approve: --update is required")
	}
	confirmed := *yes
	if !confirmed {
		confirmed, err = c.confirm("Switch to the verified candidate?", false)
		if err != nil || !confirmed {
			if err != nil {
				return err
			}
			return update.ErrNotConfirmed
		}
	}
	res, err := eng.Approve(ctx, update.ApproveOptions{UpdateID: *id, Confirmed: confirmed})
	if res != nil {
		_ = c.writeJSONLine(res)
	}
	return err
}

func (c *cli) updateAbort(ctx context.Context, args []string) error {
	fs := c.newFlagSet("update abort")
	id := fs.String("update", "", "update id")
	keep := fs.Bool("keep-candidate", false, "keep candidate volumes for diagnosis")
	eng, _, err := c.engine(fs, args, 0, 0)
	if err != nil {
		return err
	}
	if *id == "" {
		return usageErrorf("update abort: --update is required")
	}
	res, err := eng.Abort(ctx, update.AbortOptions{UpdateID: *id, KeepCandidate: *keep})
	if res != nil {
		_ = c.writeJSONLine(res)
	}
	return err
}

func (c *cli) updateRollback(ctx context.Context, args []string) error {
	fs := c.newFlagSet("update rollback")
	checkpoint := fs.String("checkpoint", "", "checkpoint id")
	yes := fs.Bool("yes", false, "confirm without a prompt")
	revoke := fs.Bool("revoke-leases", false, "revoke a lease that does not drain")
	allowCrash := fs.Bool("allow-crash-checkpoint", false, "admit a checkpoint marked crash")
	dry := fs.Bool("dry-run", false, "verify and show the rollback effect without changing anything")
	eng, _, err := c.engine(fs, args, 0, 0)
	if err != nil {
		return err
	}
	if *checkpoint == "" {
		return usageErrorf("update rollback: --checkpoint is required")
	}
	var confirm func(string) (bool, error)
	if !*yes && !*dry {
		confirm = func(summary string) (bool, error) {
			fmt.Fprint(c.stdout, summary)
			return c.confirm("Proceed with this paired rollback?", false)
		}
	}
	res, err := eng.Rollback(ctx, update.RollbackOptions{
		CheckpointID: *checkpoint, DryRun: *dry, Confirmed: *yes || *dry,
		Confirm: confirm, RevokeLeases: *revoke, AllowCrashCheckpoint: *allowCrash,
	})
	if res != nil {
		for _, line := range res.Lines {
			fmt.Fprintln(c.stdout, line)
		}
		_ = c.writeJSONLine(res)
	}
	return err
}

func (c *cli) updateStatus(ctx context.Context, args []string) error {
	fs := c.newFlagSet("update status")
	publish := fs.Bool("publish", false, "atomically write the sanitized publicStatusFile")
	eng, _, err := c.engine(fs, args, 0, 0)
	if err != nil {
		return err
	}
	status, err := eng.Status(ctx)
	if err != nil {
		return err
	}
	if *publish {
		if err := eng.PublishStatus(ctx); err != nil {
			return err
		}
	}
	return c.writeJSONLine(status)
}

func (c *cli) updateCheckpoints(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usageErrorf("update checkpoints: missing subcommand")
	}
	sub, rest := args[0], args[1:]
	fs := c.newFlagSet("update checkpoints " + sub)
	eng, pos, err := c.engine(fs, rest, 0, 1)
	if err != nil {
		return err
	}
	switch sub {
	case "list":
		if len(pos) != 0 {
			return usageErrorf("update checkpoints list: no positional arguments")
		}
		status, err := eng.Status(ctx)
		if err != nil {
			return err
		}
		return c.writeJSONLine(status.Update.Backup)
	case "verify":
		if len(pos) != 1 {
			return usageErrorf("update checkpoints verify: checkpoint id is required")
		}
		cp, err := eng.VerifyCheckpoint(pos[0])
		if err != nil {
			return err
		}
		return c.writeJSONLine(cp)
	case "prune":
		if len(pos) != 0 {
			return usageErrorf("update checkpoints prune: no positional arguments")
		}
		removed, err := eng.PruneCheckpoints()
		if err != nil {
			return err
		}
		return c.writeJSONLine(map[string]any{"removed": removed})
	default:
		return usageErrorf("update checkpoints: unknown subcommand %q", sub)
	}
}

func (c *cli) updateBranches(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usageErrorf("update branches: missing subcommand")
	}
	sub, rest := args[0], args[1:]
	fs := c.newFlagSet("update branches " + sub)
	yes := fs.Bool("yes", false, "confirm deletion without a prompt")
	eng, pos, err := c.engine(fs, rest, 0, 1)
	if err != nil {
		return err
	}
	switch sub {
	case "list":
		if len(pos) != 0 {
			return usageErrorf("update branches list: no positional arguments")
		}
		branches, err := eng.Branches(ctx)
		if err != nil {
			return err
		}
		return c.writeJSONLine(map[string]any{"branches": branches})
	case "delete":
		if len(pos) != 1 {
			return usageErrorf("update branches delete: volume is required")
		}
		confirmed := *yes
		if !confirmed {
			confirmed, err = c.confirm("Delete managed branch volume "+pos[0]+"? This cannot be undone.", false)
			if err != nil {
				return err
			}
		}
		if !confirmed {
			return update.ErrNotConfirmed
		}
		return eng.DeleteBranch(ctx, pos[0])
	default:
		return usageErrorf("update branches: unknown subcommand %q", sub)
	}
}

func (c *cli) updateCompose(ctx context.Context, args []string) error {
	fs := c.newFlagSet("update compose")
	df := c.addDeploymentFlags(fs)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return &exitError{code: exitOK}
		}
		return &exitError{code: exitUsage}
	}
	pos := fs.Args()
	depPath, err := df.path()
	if err != nil {
		return err
	}
	dep, err := update.LoadDeployment(depPath)
	if err != nil {
		return err
	}
	eng := update.NewEngine(dep, c.deploymentDocker(dep), c.stderr)
	name, argv, err := eng.ComposeCommand(pos)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, name, argv...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = c.stdin, c.stdout, c.stderr
	if env := composeEnv(dep); len(env) > 0 {
		// The deployment's recorded variables win over the caller's environment.
		cmd.Env = append(os.Environ(), env...)
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker compose: %w", err)
	}
	return nil
}

func (c *cli) updateProbe(ctx context.Context, args []string) error {
	fs := c.newFlagSet("update __probe")
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	return update.RunProbe(ctx, c.stdin, c.stdout)
}

func (c *cli) confirm(prompt string, defaultYes bool) (bool, error) {
	suffix := " [y/N] "
	if defaultYes {
		suffix = " [Y/n] "
	}
	fmt.Fprint(c.stderr, prompt+suffix)
	line, err := bufio.NewReader(c.stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	line = strings.ToLower(strings.TrimSpace(line))
	if line == "" {
		return defaultYes, nil
	}
	return line == "y" || line == "yes", nil
}
