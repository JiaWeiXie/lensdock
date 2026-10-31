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

const projectUsageText = `  lensdock admin manager init [--bundle-dir DIR] [--docker-context NAME] [--require-pairing] [--local-auto-entry]
  lensdock admin manager policy [--memory-mib N] [--cpus N]
      [--require-project-authorization] [--require-explicit-start] [--require-stop-confirmation]
  lensdock admin manager stop
      Stops only the daemon; first stop projects if you also want their browsers stopped.
  lensdock admin project authorize PATH...
  lensdock admin project list [--json]
  lensdock admin project limits --id ID [--runtime-memory-mib N] [--runtime-cpus N] [--gateway-memory-mib N] [--gateway-cpus N]
  lensdock admin project relocate --id ID PATH | revoke --id ID | recover --id ID --operation ID
  lensdock admin project stop --project PATH [--force --expected-generation ID]
  lensdock project up [--project PATH] [--wait DURATION]
  lensdock project status [--project PATH]
  lensdock project cancel [--project PATH] --waiter ID
  lensdock project stop [--project PATH] [--wait DURATION] [--discard-page-state]
      Project paths default to the current directory. New projects initialize and start automatically.
      Limits and --wait default to 0 (unlimited); configured restrictions remain effective.
      LENSDOCK_MANAGER_DIR selects the private manager directory.
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
		"stop":   func(a []string) error { return c.managerShutdown(ctx, a) },
	})
}

func policyFlags(fs *flag.FlagSet, p *hostmanager.Policy) func() error {
	memory := fs.Int64("memory-mib", p.Budget.MemoryBytes>>20, "managed deployment memory budget in MiB; 0 disables it")
	cpu := fs.Float64("cpus", float64(p.Budget.NanoCPUs)/1e9, "managed deployment CPU budget; 0 disables it")
	headroom := fs.Int64("headroom-mib", p.Headroom.MemoryBytes>>20, "memory reserved for the VM and system in MiB")
	headroomCPU := fs.Float64("headroom-cpus", float64(p.Headroom.NanoCPUs)/1e9, "CPU capacity reserved for the VM and system")
	helper := fs.Int64("helper-memory-mib", p.Helpers.MemoryBytes>>20, "serialized lifecycle helper memory cap in MiB; 0 is unlimited")
	helperCPU := fs.Float64("helper-cpus", float64(p.Helpers.NanoCPUs)/1e9, "serialized lifecycle helper CPU cap; 0 is unlimited")
	fs.Int64Var(&p.HelperPidsLimit, "helper-pids-limit", p.HelperPidsLimit, "serialized lifecycle helper process cap; 0 is unlimited")
	runtimeMem := fs.Int64("runtime-memory-mib", p.ProjectLimits.Runtime.MemoryBytes>>20, "runtime cap for new projects in MiB; 0 is unlimited")
	runtimeCPU := fs.Float64("runtime-cpus", float64(p.ProjectLimits.Runtime.NanoCPUs)/1e9, "runtime CPU cap for new projects; 0 is unlimited")
	gatewayMem := fs.Int64("gateway-memory-mib", p.ProjectLimits.Gateway.MemoryBytes>>20, "Gateway cap for new projects in MiB; 0 is unlimited")
	gatewayCPU := fs.Float64("gateway-cpus", float64(p.ProjectLimits.Gateway.NanoCPUs)/1e9, "Gateway CPU cap for new projects; 0 is unlimited")
	allow := fs.Bool("allow-unbounded-external", p.AllowUnboundedExternal, "allow starts alongside uncapped external containers")
	externalMem := fs.Int64("unbounded-external-memory-mib", p.UnboundedExternalAllowance.MemoryBytes>>20, "aggregate memory allowance for uncapped external containers")
	externalCPU := fs.Float64("unbounded-external-cpus", float64(p.UnboundedExternalAllowance.NanoCPUs)/1e9, "aggregate CPU allowance for uncapped external containers")
	fs.BoolVar(&p.RequireProjectAuthorization, "require-project-authorization", p.RequireProjectAuthorization, "require explicit authorization before enrolling new project directories")
	fs.BoolVar(&p.RequireExplicitStart, "require-explicit-start", p.RequireExplicitStart, "require project up before project-bound commands")
	fs.BoolVar(&p.RequireStopConfirmation, "require-stop-confirmation", p.RequireStopConfirmation, "require --discard-page-state when stopping a project")
	return func() error {
		for _, n := range []int64{*memory, *headroom, *helper, *runtimeMem, *gatewayMem, *externalMem} {
			if n < 0 || n > math.MaxInt64>>20 {
				return usageErrorf("memory quantities must be nonnegative and fit a signed 64-bit byte count")
			}
		}
		for _, n := range []float64{*cpu, *headroomCPU, *helperCPU, *runtimeCPU, *gatewayCPU, *externalCPU} {
			if math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n*1e9 >= float64(math.MaxInt64) || n > 0 && n*1e9 < 1 {
				return usageErrorf("CPU quantities must be nonnegative and representable in nanocpus")
			}
		}
		p.Budget = hostmanager.Resources{MemoryBytes: *memory << 20, NanoCPUs: int64(*cpu * 1e9)}
		p.Headroom = hostmanager.Resources{MemoryBytes: *headroom << 20, NanoCPUs: int64(*headroomCPU * 1e9)}
		p.Helpers = hostmanager.Resources{MemoryBytes: *helper << 20, NanoCPUs: int64(*helperCPU * 1e9)}
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
	runtimeImage := fs.String("runtime-image", c.getenv(product.EnvRuntimeImage), "initial managed Runtime image (default $"+product.EnvRuntimeImage+" or "+product.ReleaseRuntimeImage+")")
	gatewayImage := fs.String("gateway-image", c.getenv(product.EnvGatewayImage), "initial managed Gateway image (default $"+product.EnvGatewayImage+" or "+product.ReleaseGatewayImage+")")
	pullDefault := c.getenv(product.EnvPull)
	if pullDefault == "" {
		pullDefault = product.PullAlways
	}
	pull := fs.String("pull", pullDefault, "first-start pull policy: always, missing, never (default $"+product.EnvPull+" or always)")
	first := fs.Int("port-first", 19081, "first stable managed loopback port")
	last := fs.Int("port-last", 19999, "last stable managed loopback port")
	requirePairing := fs.Bool("require-pairing", false, "require browser pairing for new managed projects (the default; wins over --local-auto-entry)")
	localAutoEntry := fs.Bool("local-auto-entry", false, "let local browsers open a human session in new managed projects without a token; any local process can do the same")
	policy := hostmanager.DefaultPolicy()
	finish := policyFlags(fs, &policy)
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	if err := finish(); err != nil {
		return err
	}
	if *runtimeImage == "" {
		*runtimeImage = product.ReleaseRuntimeImage
	}
	if *gatewayImage == "" {
		*gatewayImage = product.ReleaseGatewayImage
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
	cfg.RequirePairing = *requirePairing
	cfg.LocalAutoEntry = *localAutoEntry
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
	if args[0] == "limits" {
		return c.projectLimits(ctx, args[1:])
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
	selector := fs.String("project", "", "project directory (defaults to the current directory)")
	id := fs.String("id", "", "stable project ID (Host Admin authority required)")
	wait := fs.Duration("wait", 0, "maximum time to wait; 0 waits until completion or cancellation")
	discard := fs.Bool("discard-page-state", false, "acknowledge loss of in-memory pages when stop-confirmation policy is enabled")
	force := fs.Bool("force", false, "Host Admin: revoke the observed lease before conditional stop")
	generation := fs.String("expected-generation", "", "runtime generation explicitly observed for a forced stop")
	waiter := fs.String("waiter", "", "waiter ID to cancel")
	if _, err := parse(fs, args[1:], 0, 0); err != nil {
		return err
	}
	if *selector != "" && *id != "" {
		return usageErrorf("--project and --id cannot be combined")
	}
	if *selector == "" && *id == "" {
		*selector = "."
	}
	if !admin && (*id != "" || *force) {
		return usageErrorf("--id and --force require lensdock admin project stop")
	}
	if *wait < 0 {
		return usageErrorf("--wait must be nonnegative")
	}
	if *force && *generation == "" {
		return usageErrorf("--force requires --expected-generation")
	}
	root, err := c.managerRoot()
	if err != nil {
		return err
	}
	var p hostmanager.Project
	if *selector != "" {
		if action == "up" {
			root, p, err = c.managedProject(ctx, *selector)
		} else {
			err = hostmanager.EnsureRunning(ctx, root)
			if err == nil {
				p, err = hostmanager.ResolveProject(root, *selector)
			}
		}
		if err != nil {
			return err
		}
	} else {
		if err := hostmanager.EnsureRunning(ctx, root); err != nil {
			return err
		}
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
	req := hostmanager.Request{Action: action, ProjectID: p.ID, DiscardPageState: *discard, Force: *force, ExpectedGeneration: *generation, WaiterID: *waiter}
	if *wait > 0 {
		req.Deadline = time.Now().Add(*wait)
	}
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
	result, err := c.waitProjectResult(ctx, root, token, p, resp, timeout, connect)
	if err != nil {
		return err
	}
	if connect {
		operationID := ""
		if result.Operation != nil {
			operationID = result.Operation.ID
		}
		return c.writeJSONLine(map[string]any{"operationId": operationID, "connection": result.Connection})
	}
	return c.writeJSONLine(result)
}

func (c *cli) waitProjectResult(ctx context.Context, root, token string, p hostmanager.Project, resp hostmanager.Response, timeout time.Duration, connect bool) (hostmanager.Response, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	waiterID, operationID := "", ""
	if resp.Waiter != nil {
		waiterID = resp.Waiter.ID
	}
	if resp.Operation != nil {
		operationID = resp.Operation.ID
		fmt.Fprintf(c.stderr, "Project operation: %s\n", operationID)
	}
	defer func() {
		if waiterID != "" {
			cleanup, done := context.WithTimeout(context.Background(), 3*time.Second)
			defer done()
			_, _ = hostmanager.Call(cleanup, root, token, hostmanager.Request{Action: "cancel", ProjectID: p.ID, WaiterID: waiterID})
		}
	}()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if resp.Operation != nil {
			switch resp.Operation.Phase {
			case "failed", "blocked", "canceled", "abandoned":
				return hostmanager.Response{}, fmt.Errorf("project operation %s %s: %s", resp.Operation.ID, resp.Operation.Phase, resp.Operation.Error)
			case "succeeded":
				if !connect {
					return resp, nil
				}
			}
		}
		if connect && resp.Project != nil && resp.Project.State == "running" && (resp.Operation == nil || resp.Operation.Phase == "succeeded") {
			verified, err := hostmanager.Call(ctx, root, token, hostmanager.Request{Action: "connect", ProjectID: p.ID})
			if err != nil {
				return hostmanager.Response{}, err
			}
			connection, err := checkProjectConnection(p, verified)
			if err != nil {
				return hostmanager.Response{}, err
			}
			verified.Connection, verified.Operation = &connection, resp.Operation
			return verified, nil
		}
		if !connect && resp.Operation == nil && resp.Project != nil && resp.Project.State == "stopped" {
			return resp, nil
		}
		select {
		case <-ctx.Done():
			fmt.Fprintf(c.stderr, "Stopped waiting for operation %s; any admitted operation and browser remain running.\n", operationID)
			return hostmanager.Response{}, ctx.Err()
		case <-ticker.C:
		}
		if waiterID != "" {
			if _, err := hostmanager.Call(ctx, root, token, hostmanager.Request{Action: "heartbeat", ProjectID: p.ID, WaiterID: waiterID}); err != nil {
				return hostmanager.Response{}, err
			}
		}
		var err error
		resp, err = hostmanager.Call(ctx, root, token, hostmanager.Request{Action: "status", ProjectID: p.ID, OperationID: operationID})
		if err != nil {
			return hostmanager.Response{}, err
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
	resp, err := hostmanager.Call(ctx, root, token, hostmanager.Request{Action: "ensure", ProjectID: owner.projectID})
	if err != nil {
		return err
	}
	if resp.Project == nil {
		return errors.New("manager returned no project")
	}
	return c.waitProject(ctx, root, token, *resp.Project, resp, 0, true)
}
