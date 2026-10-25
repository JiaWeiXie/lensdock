package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/hostmanager"
	"github.com/JiaWeiXie/lensdock/internal/product"
	"github.com/JiaWeiXie/lensdock/internal/update"
)

const projectUsageText = `  lensdock admin manager init --bundle-dir DIR [--docker-context NAME] [--memory-mib 6912] [--cpus 7.5]
  lensdock admin manager policy [--memory-mib N] [--cpus N]
  lensdock admin project authorize PATH...
  lensdock admin project list [--json]
  lensdock admin project relocate --id ID PATH | revoke --id ID | recover --id ID --operation ID
  lensdock admin project stop --project PATH --discard-page-state [--force --expected-generation ID]
  lensdock project up --project PATH [--wait 10m]
  lensdock project status --project PATH
  lensdock project cancel --project PATH --waiter ID
  lensdock project stop --project PATH --discard-page-state [--wait 2m]
      LENSDOCK_MANAGER_DIR selects the initialized private manager directory.
      Cancelling a wait never stops an admitted operation or a running browser.
`

func (c *cli) managerRoot() (string, error) {
	if root := c.getenv("LENSDOCK_MANAGER_DIR"); root != "" {
		return filepath.Abs(root)
	}
	if home := c.homeDir(); home != "" {
		return filepath.Join(home, ".local", "share", "lensdock-manager"), nil
	}
	return hostmanager.DefaultRoot()
}

func (c *cli) managerCommand(ctx context.Context, args []string) error {
	return c.subcommand(args, "admin manager", map[string]func([]string) error{
		"init":   func(a []string) error { return c.managerInit(ctx, a) },
		"policy": func(a []string) error { return c.managerPolicy(ctx, a) },
	})
}

func policyFlags(fs *flag.FlagSet, p *hostmanager.Policy) func() error {
	memory := fs.Int64("memory-mib", p.Budget.MemoryBytes>>20, "managed deployment memory budget in MiB, excluding helpers and system headroom")
	cpu := fs.Float64("cpus", float64(p.Budget.NanoCPUs)/1e9, "managed deployment CPU cap budget")
	headroom := fs.Int64("headroom-mib", p.Headroom.MemoryBytes>>20, "memory reserved for the VM and system in MiB")
	helper := fs.Int64("helper-memory-mib", p.Helpers.MemoryBytes>>20, "serialized lifecycle helper allowance in MiB (at least 1024 for migrations)")
	runtimeMem := fs.Int64("runtime-memory-mib", p.ProjectLimits.Runtime.MemoryBytes>>20, "runtime cap for newly authorized projects, in MiB")
	runtimeCPU := fs.Float64("runtime-cpus", float64(p.ProjectLimits.Runtime.NanoCPUs)/1e9, "runtime CPU cap for newly authorized projects")
	gatewayMem := fs.Int64("gateway-memory-mib", p.ProjectLimits.Gateway.MemoryBytes>>20, "Gateway cap for newly authorized projects, in MiB")
	gatewayCPU := fs.Float64("gateway-cpus", float64(p.ProjectLimits.Gateway.NanoCPUs)/1e9, "Gateway CPU cap for newly authorized projects")
	allow := fs.Bool("allow-unbounded-external", p.AllowUnboundedExternal, "explicitly account uncapped external containers using the supplied aggregate allowance")
	externalMem := fs.Int64("unbounded-external-memory-mib", p.UnboundedExternalAllowance.MemoryBytes>>20, "aggregate memory allowance for uncapped external containers")
	externalCPU := fs.Float64("unbounded-external-cpus", float64(p.UnboundedExternalAllowance.NanoCPUs)/1e9, "aggregate CPU allowance for uncapped external containers")
	return func() error {
		for _, n := range []int64{*memory, *headroom, *helper, *runtimeMem, *gatewayMem, *externalMem} {
			if n < 0 || n > 1<<30 {
				return usageErrorf("memory quantities must be between 0 and 2^30 MiB")
			}
		}
		for _, n := range []float64{*cpu, *runtimeCPU, *gatewayCPU, *externalCPU} {
			if math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n > 100000 {
				return usageErrorf("CPU quantities must be finite and between 0 and 100000")
			}
		}
		p.Budget = hostmanager.Resources{MemoryBytes: *memory << 20, NanoCPUs: int64(*cpu * 1e9)}
		p.Headroom.MemoryBytes = *headroom << 20
		p.Helpers.MemoryBytes = *helper << 20
		p.ProjectLimits.Runtime = hostmanager.Resources{MemoryBytes: *runtimeMem << 20, NanoCPUs: int64(*runtimeCPU * 1e9)}
		p.ProjectLimits.Gateway = hostmanager.Resources{MemoryBytes: *gatewayMem << 20, NanoCPUs: int64(*gatewayCPU * 1e9)}
		p.AllowUnboundedExternal = *allow
		p.UnboundedExternalAllowance = hostmanager.Resources{MemoryBytes: *externalMem << 20, NanoCPUs: int64(*externalCPU * 1e9)}
		return nil
	}
}

func (c *cli) managerInit(ctx context.Context, args []string) error {
	fs := c.newFlagSet("admin manager init")
	bundleDir := fs.String("bundle-dir", "", "trusted release bundle (defaults to this executable's bundle)")
	dockerContext := fs.String("docker-context", "", "human-selected Docker context; its local endpoint and engine ID are recorded")
	runtimeImage := fs.String("runtime-image", "ghcr.io/jiaweixie/lensdock/runtime:latest", "initial managed Runtime image")
	gatewayImage := fs.String("gateway-image", "ghcr.io/jiaweixie/lensdock/gateway:latest", "initial managed Gateway image")
	pull := fs.String("pull", "always", "first-start pull policy: always, missing, never")
	first := fs.Int("port-first", 19081, "first stable managed loopback port")
	last := fs.Int("port-last", 19999, "last stable managed loopback port")
	policy := hostmanager.DefaultPolicy()
	finish := policyFlags(fs, &policy)
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	if err := finish(); err != nil {
		return err
	}
	for _, image := range []string{*runtimeImage, *gatewayImage} {
		if err := update.CheckImageRef(image); err != nil {
			return err
		}
	}
	if *pull != product.PullAlways && *pull != product.PullMissing && *pull != product.PullNever {
		return usageErrorf("--pull must be always, missing or never")
	}
	bundle, err := c.findBundle(*bundleDir)
	if err != nil {
		return err
	}
	if bundle == nil {
		return usageErrorf("manager initialization requires a trusted release bundle; pass --bundle-dir")
	}
	root, err := c.managerRoot()
	if err != nil {
		return err
	}
	executable := os.Executable
	if c.executable != nil {
		executable = c.executable
	}
	exe, err := executable()
	if err != nil {
		return err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return err
	}
	binary, err := exec.LookPath("docker")
	if err != nil {
		return err
	}
	binary, err = filepath.Abs(binary)
	if err != nil {
		return err
	}
	// Keep the invocation name: multicall Docker installations dispatch on
	// argv[0], so resolving the symlink can turn "docker" into another program.
	capture := func(a ...string) ([]byte, error) {
		var out bytes.Buffer
		err := c.baseRunner().Run(ctx, update.Cmd{Name: binary, Args: a, Stdout: &out})
		return out.Bytes(), err
	}
	selected := *dockerContext
	if selected == "" {
		raw, err := capture("context", "show")
		if err != nil {
			return err
		}
		selected = strings.TrimSpace(string(raw))
	}
	raw, err := capture("context", "inspect", selected)
	if err != nil {
		return err
	}
	var contexts []struct {
		Endpoints map[string]struct{ Host string }
	}
	if err := json.Unmarshal(raw, &contexts); err != nil || len(contexts) != 1 {
		return errors.New("Docker returned an invalid context description")
	}
	host := contexts[0].Endpoints["docker"].Host
	if *dockerContext == "" && c.getenv("DOCKER_CONTEXT") == "" && c.getenv("DOCKER_HOST") != "" {
		host = c.getenv("DOCKER_HOST")
	}
	if !strings.HasPrefix(host, "unix:///") {
		return usageErrorf("managed project origins require a local Docker Unix socket; selected endpoint is not local")
	}
	dockerConfig := c.getenv("DOCKER_CONFIG")
	if dockerConfig == "" {
		dockerConfig = filepath.Join(c.homeDir(), ".docker")
	}
	dockerConfig, err = filepath.Abs(dockerConfig)
	if err != nil {
		return err
	}
	cfg := hostmanager.Config{Version: 1, Root: root, Executable: exe, DockerBinary: binary, DockerContext: selected, DockerHost: host, DockerConfig: dockerConfig, BundleDir: bundle.Root, RuntimeImage: *runtimeImage, GatewayImage: *gatewayImage, PullPolicy: *pull, PortFirst: *first, PortLast: *last, Policy: policy}
	inventory, err := hostmanager.InspectEngine(ctx, cfg)
	if err != nil {
		return err
	}
	cfg.EngineID = inventory.EngineID
	cfg, err = hostmanager.Initialize(cfg)
	if err != nil {
		return err
	}
	return c.writeJSONLine(map[string]any{"managerId": cfg.ID, "root": cfg.Root, "engineId": cfg.EngineID, "policy": cfg.Policy})
}

func (c *cli) managerPolicy(ctx context.Context, args []string) error {
	root, err := c.managerRoot()
	if err != nil {
		return err
	}
	cfg, err := hostmanager.LoadConfig(root)
	if err != nil {
		return err
	}
	fs := c.newFlagSet("admin manager policy")
	policy := cfg.Policy
	finish := policyFlags(fs, &policy)
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	if err := finish(); err != nil {
		return err
	}
	if err := hostmanager.EnsureRunning(ctx, root); err != nil {
		return err
	}
	resp, err := hostmanager.Call(ctx, root, hostmanager.AdminTokenFile(root), hostmanager.Request{Action: "policy", Policy: &policy})
	if err != nil {
		return err
	}
	return c.writeJSONLine(resp)
}

func (c *cli) adminProject(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usageErrorf("admin project: missing subcommand")
	}
	if args[0] == "stop" {
		return c.projectCommand(ctx, args, true)
	}
	fs := c.newFlagSet("admin project " + args[0])
	id := fs.String("id", "", "stable project ID")
	operation := fs.String("operation", "", "operation ID explicitly acknowledged for recovery")
	asJSON := fs.Bool("json", false, "print structured project status")
	pos, err := parse(fs, args[1:], 0, 1024)
	if err != nil {
		return err
	}
	root, err := c.managerRoot()
	if err != nil {
		return err
	}
	if err := hostmanager.EnsureRunning(ctx, root); err != nil {
		return err
	}
	call := func(req hostmanager.Request) (hostmanager.Response, error) {
		return hostmanager.Call(ctx, root, hostmanager.AdminTokenFile(root), req)
	}
	switch args[0] {
	case "authorize":
		if len(pos) == 0 {
			return usageErrorf("authorize requires one or more exact project directories")
		}
		for _, path := range pos {
			path, err := filepath.Abs(path)
			if err != nil {
				return err
			}
			resp, err := call(hostmanager.Request{Action: "authorize", Path: path})
			if err != nil {
				return err
			}
			if err := c.writeJSONLine(resp); err != nil {
				return err
			}
		}
		return nil
	case "list":
		if len(pos) != 0 {
			return usageErrorf("list takes no paths")
		}
		resp, err := call(hostmanager.Request{Action: "list"})
		if err != nil {
			return err
		}
		if *asJSON {
			return c.writeJSONLine(resp)
		}
		w := tabwriter.NewWriter(c.stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "PROJECT\tSTATE\tQUEUE\tLIMIT MiB/CPU\tRESERVED MiB/CPU\tUSED MiB/CPU\tORIGIN\tROOT\tBLOCKER")
		for _, p := range resp.Projects {
			limit := hostmanager.Resources{MemoryBytes: p.Limits.Runtime.MemoryBytes + p.Limits.Gateway.MemoryBytes, NanoCPUs: p.Limits.Runtime.NanoCPUs + p.Limits.Gateway.NanoCPUs}
			quantity := func(r hostmanager.Resources) string {
				return fmt.Sprintf("%.1f/%.2f", float64(r.MemoryBytes)/(1<<20), float64(r.NanoCPUs)/1e9)
			}
			usage := "unavailable"
			if p.UsageAvailable {
				usage = quantity(p.Usage)
			}
			fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\n", p.ID, p.State, p.QueuePosition, quantity(limit), quantity(p.Reservation), usage, p.Origin, p.Root, p.Blocker)
		}
		for _, warning := range resp.Warnings {
			fmt.Fprintln(c.stderr, warning)
		}
		return w.Flush()
	case "relocate", "revoke", "recover":
		if *id == "" {
			return usageErrorf("--id is required")
		}
		req := hostmanager.Request{Action: args[0], ProjectID: *id, OperationID: *operation}
		if args[0] == "relocate" {
			if len(pos) != 1 {
				return usageErrorf("relocate requires the new directory")
			}
			req.Path, err = filepath.Abs(pos[0])
			if err != nil {
				return err
			}
		} else if len(pos) != 0 {
			return usageErrorf("unexpected project path")
		}
		if args[0] == "recover" && *operation == "" {
			return usageErrorf("recover requires --operation for the interrupted operation")
		}
		resp, err := call(req)
		if err != nil {
			return err
		}
		return c.writeJSONLine(resp)
	default:
		return usageErrorf("unknown admin project command %q", args[0])
	}
}

func (c *cli) projectCommand(ctx context.Context, args []string, admin bool) error {
	if len(args) == 0 {
		return usageErrorf("project: missing subcommand")
	}
	action := args[0]
	if action != "up" && action != "status" && action != "cancel" && action != "stop" {
		return usageErrorf("unknown project command %q", action)
	}
	fs := c.newFlagSet("project " + action)
	selector := fs.String("project", "", "authorized project directory")
	id := fs.String("id", "", "stable project ID (human administration only)")
	waitDefault := 10 * time.Minute
	if action == "stop" {
		waitDefault = 2 * time.Minute
	}
	wait := fs.Duration("wait", waitDefault, "maximum time to wait; cancellation never stops an admitted operation")
	discard := fs.Bool("discard-page-state", false, "explicitly discard in-memory pages, forms and network work when stopping")
	force := fs.Bool("force", false, "human-only: revoke the observed lease before conditional stop")
	generation := fs.String("expected-generation", "", "runtime generation explicitly observed for a forced stop")
	waiter := fs.String("waiter", "", "waiter ID to cancel")
	if _, err := parse(fs, args[1:], 0, 0); err != nil {
		return err
	}
	if (*selector == "") == (*id == "") {
		return usageErrorf("exactly one --project path or human --id is required")
	}
	if !admin && (*id != "" || *force) {
		return usageErrorf("--id and --force require lensdock admin project stop")
	}
	if *wait <= 0 || *wait > 24*time.Hour {
		return usageErrorf("--wait must be positive and at most 24h")
	}
	if action == "stop" && !*discard {
		return usageErrorf("stop requires --discard-page-state; lease release and disconnect are not stop consent")
	}
	if *force && *generation == "" {
		return usageErrorf("--force requires --expected-generation")
	}
	root, err := c.managerRoot()
	if err != nil {
		return err
	}
	if err := hostmanager.EnsureRunning(ctx, root); err != nil {
		return err
	}
	var p hostmanager.Project
	if *selector != "" {
		p, err = hostmanager.ResolveProject(root, *selector)
		if err != nil {
			return err
		}
	} else {
		resp, err := hostmanager.Call(ctx, root, hostmanager.AdminTokenFile(root), hostmanager.Request{Action: "status", ProjectID: *id})
		if err != nil {
			return err
		}
		if resp.Project == nil {
			return errors.New("manager returned no project")
		}
		p = *resp.Project
	}
	token := p.TokenFile
	if admin {
		token = hostmanager.AdminTokenFile(root)
	}
	req := hostmanager.Request{Action: action, ProjectID: p.ID, Deadline: time.Now().Add(*wait), DiscardPageState: *discard, Force: *force, ExpectedGeneration: *generation, WaiterID: *waiter}
	if action == "up" {
		req.Action = "ensure"
	}
	if action == "cancel" && *waiter == "" {
		return usageErrorf("cancel requires --waiter")
	}
	resp, err := hostmanager.Call(ctx, root, token, req)
	if err != nil {
		return err
	}
	if action == "status" || action == "cancel" {
		return c.writeJSONLine(resp)
	}
	return c.waitProject(ctx, root, token, p, resp, *wait, action == "up")
}

func (c *cli) waitProject(ctx context.Context, root, token string, p hostmanager.Project, resp hostmanager.Response, timeout time.Duration, connect bool) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	waiterID, operationID := "", ""
	if resp.Waiter != nil {
		waiterID = resp.Waiter.ID
	}
	if resp.Operation != nil {
		operationID = resp.Operation.ID
	}
	if operationID != "" {
		fmt.Fprintf(c.stderr, "Project operation: %s\n", operationID)
	}
	defer func() {
		if waiterID != "" {
			cleanup, done := context.WithTimeout(context.Background(), 3*time.Second)
			defer done()
			_, _ = hostmanager.Call(cleanup, root, token, hostmanager.Request{Action: "cancel", ProjectID: p.ID, WaiterID: waiterID})
		}
	}()
	for {
		if resp.Operation != nil {
			switch resp.Operation.Phase {
			case "failed", "blocked", "canceled", "abandoned":
				return fmt.Errorf("project operation %s %s: %s", resp.Operation.ID, resp.Operation.Phase, resp.Operation.Error)
			case "succeeded":
				if !connect {
					return c.writeJSONLine(resp)
				}
			}
		}
		if connect && resp.Project != nil && resp.Project.State == "running" && (resp.Operation == nil || resp.Operation.Phase == "succeeded") {
			verified, err := hostmanager.Call(ctx, root, token, hostmanager.Request{Action: "connect", ProjectID: p.ID})
			if err != nil {
				return err
			}
			connection, err := checkProjectConnection(p, verified)
			if err != nil {
				return err
			}
			return c.writeJSONLine(map[string]any{"operationId": operationID, "connection": connection})
		}
		if !connect && resp.Operation == nil && resp.Project != nil && resp.Project.State == "stopped" {
			return c.writeJSONLine(resp)
		}
		select {
		case <-ctx.Done():
			fmt.Fprintf(c.stderr, "Stopped waiting for operation %s; any admitted operation and browser remain running.\n", operationID)
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
		if waiterID != "" {
			if _, err := hostmanager.Call(ctx, root, token, hostmanager.Request{Action: "heartbeat", ProjectID: p.ID, WaiterID: waiterID}); err != nil {
				return err
			}
		}
		var err error
		resp, err = hostmanager.Call(ctx, root, token, hostmanager.Request{Action: "status", ProjectID: p.ID, OperationID: operationID})
		if err != nil {
			return err
		}
	}
}

func (c *cli) internalManager(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usageErrorf("internal manager command is missing")
	}
	fs := c.newFlagSet("__manager " + args[0])
	root := fs.String("root", "", "initialized manager root")
	operation := fs.String("operation", "", "durable operation ID")
	if _, err := parse(fs, args[1:], 0, 0); err != nil {
		return err
	}
	if *root == "" {
		return usageErrorf("manager root is required")
	}
	backend := hostmanager.DockerBackend{Log: c.stderr}
	switch args[0] {
	case "serve":
		return hostmanager.Serve(ctx, *root, backend)
	case "worker":
		if *operation == "" {
			return usageErrorf("worker operation is required")
		}
		return hostmanager.RunWorker(ctx, *root, *operation, backend)
	default:
		return usageErrorf("unknown internal manager command")
	}
}

func (c *cli) startManagedProduct(ctx context.Context, dep *update.Deployment) error {
	owner, err := resolveManagedOwner(dep)
	if err != nil {
		return err
	}
	if owner == nil {
		return errors.New("deployment is not managed")
	}
	if err := owner.admit(ctx); err != nil {
		return err
	}
	root, token := owner.root, hostmanager.AdminTokenFile(owner.root)
	resp, err := hostmanager.Call(ctx, root, token, hostmanager.Request{Action: "ensure", ProjectID: owner.projectID, Deadline: time.Now().Add(10 * time.Minute)})
	if err != nil {
		return err
	}
	if resp.Project == nil {
		return errors.New("manager returned no project")
	}
	return c.waitProject(ctx, root, token, *resp.Project, resp, 10*time.Minute, true)
}
