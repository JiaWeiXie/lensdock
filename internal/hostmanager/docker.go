package hostmanager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/auth"
	"github.com/JiaWeiXie/lensdock/internal/product"
	"github.com/JiaWeiXie/lensdock/internal/update"
)

const (
	managerLabel = "io.lensdock.manager-id"
	projectLabel = "io.lensdock.project-id"
	helperLabel  = "io.lensdock.manager-helper"
)

// DockerBackend uses only the target and bundle chosen at human initialization.
type DockerBackend struct{ Log io.Writer }

// DeploymentDocker fixes the Docker target before applying deployment env files.
// It is also used by the human CLI's read-only managed diagnostics.
func DeploymentDocker(cfg Config, dep *update.Deployment, locks []*os.File) update.Docker {
	d := product.DeploymentDocker(dep, fixedDockerRunner{cfg: cfg, locks: locks})
	d.Bin = cfg.DockerBinary
	return d
}

type fixedDockerRunner struct {
	cfg   Config
	locks []*os.File
}

func (r fixedDockerRunner) Run(ctx context.Context, c update.Cmd) error {
	if c.Name != "docker" && c.Name != r.cfg.DockerBinary {
		return errors.New("managed operations can execute only the configured Docker binary")
	}
	if !filepath.IsAbs(r.cfg.DockerBinary) || !strings.HasPrefix(r.cfg.DockerHost, "unix:///") {
		return errors.New("managed Docker target must name an absolute binary and local Unix socket")
	}
	args := c.Args
	locks := r.locks
	if len(args) > 0 && args[0] == "run" {
		if len(locks) == 0 {
			helper, err := AcquireHelper(ctx, r.cfg.Root)
			if err != nil {
				return err
			}
			defer helper.Close()
			locks = []*os.File{helper}
			// The caller may have waited while a human changed helper policy.
			// The lock prevents another change until this child exits.
			current, err := LoadConfig(r.cfg.Root)
			if err != nil {
				return err
			}
			r.cfg.Policy.Helpers = current.Policy.Helpers
			r.cfg.Policy.HelperPidsLimit = current.Policy.HelperPidsLimit
			if _, err := InspectEngine(ctx, r.cfg); err != nil {
				return err
			}
			all, err := containerInventory(ctx, r.cfg)
			if err != nil {
				return err
			}
			for _, existing := range all {
				if existing.State.Running && existing.Config.Labels[managerLabel] == r.cfg.ID && existing.Config.Labels[helperLabel] == "true" {
					return errors.New("an earlier lifecycle helper is still running; refusing another helper")
				}
			}
		}
		helpers := r.cfg.Policy.Helpers
		memory := helpers.MemoryBytes
		for i, arg := range args {
			if arg == "--memory" && i+1 < len(args) {
				var err error
				memory, err = parseMemory(args[i+1])
				if err != nil {
					return err
				}
				// A caller may only tighten a configured allowance.
				if helpers.MemoryBytes > 0 && (memory <= 0 || memory > helpers.MemoryBytes) {
					return errors.New("helper exceeds the configured lifecycle resource allowance")
				}
			}
		}
		prefix := []string{"run", "--label", managerLabel + "=" + r.cfg.ID, "--label", helperLabel + "=true"}
		if memory > 0 {
			prefix = append(prefix, "--memory", strconv.FormatInt(memory, 10), "--memory-swap", strconv.FormatInt(memory, 10))
		}
		if helpers.NanoCPUs > 0 {
			prefix = append(prefix, "--cpus", cpuArg(helpers.NanoCPUs))
		}
		if r.cfg.Policy.HelperPidsLimit > 0 {
			prefix = append(prefix, "--pids-limit", strconv.FormatInt(r.cfg.Policy.HelperPidsLimit, 10))
		}
		// The manager's policy owns the PID cap: a deployment or caller value is
		// dropped instead of overriding it (the last flag would win in Docker).
		rest := make([]string, 0, len(args))
		for i := 1; i < len(args); i++ {
			switch {
			case args[i] == "--pids-limit" && i+1 < len(args):
				i++
			case strings.HasPrefix(args[i], "--pids-limit="):
			default:
				rest = append(rest, args[i])
			}
		}
		args = append(prefix, rest...)
	}
	// The explicit endpoint wins over context files. No caller Docker/Compose
	// environment may select another engine, project, file, or profile.
	args = append([]string{"--config", r.cfg.DockerConfig, "--host", r.cfg.DockerHost}, args...)
	env := make([]string, 0, len(os.Environ())+len(c.Env))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "DOCKER_") || strings.HasPrefix(key, "COMPOSE_") || strings.HasPrefix(key, "LENSDOCK_") {
			continue
		}
		env = append(env, entry)
	}
	for _, entry := range c.Env {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "DOCKER_") || strings.HasPrefix(key, "COMPOSE_") {
			continue
		}
		env = append(env, entry)
	}
	// Helper limits are operation policy, not immutable project limits. These
	// trusted values win over both caller and deployment interpolation inputs.
	// Zero means unlimited and disables the Compose cap.
	if len(c.Args) > 0 && c.Args[0] == "compose" {
		env = append(env, composeHelperPolicyEnv(r.cfg.Policy)...)
	}
	c.Name, c.Args, c.Env, c.EnvExact = r.cfg.DockerBinary, args, env, true
	c.ExtraFiles = append(c.ExtraFiles, locks...)
	return (update.ExecRunner{}).Run(ctx, c)
}

// composeHelperPolicyEnv is the trusted Compose interpolation input that
// carries the manager's helper allowance (CPU, memory and PID cap). Entries
// are appended after every caller and deployment variable so they win.
func composeHelperPolicyEnv(p Policy) []string {
	return []string{
		"LENSDOCK_MANAGER_HELPER_MEMORY=" + strconv.FormatInt(p.Helpers.MemoryBytes, 10),
		"LENSDOCK_MANAGER_HELPER_CPUS=" + cpuArg(p.Helpers.NanoCPUs),
		"LENSDOCK_MANAGER_HELPER_PIDS=" + strconv.FormatInt(p.HelperPidsLimit, 10),
	}
}

func dockerOutput(ctx context.Context, cfg Config, args ...string) ([]byte, error) {
	var out bytes.Buffer
	err := (fixedDockerRunner{cfg: cfg}).Run(ctx, update.Cmd{Name: cfg.DockerBinary, Args: args, Stdout: &boundedOutput{w: &out, left: 16 << 20}})
	return out.Bytes(), err
}

type boundedOutput struct {
	w    io.Writer
	left int
}

func (w *boundedOutput) Write(p []byte) (int, error) {
	if len(p) > w.left {
		return 0, errors.New("Docker response exceeds the management response limit")
	}
	n, err := w.w.Write(p)
	w.left -= n
	return n, err
}

// InspectEngine performs a read-only identity/capacity query on the fixed target.
func InspectEngine(ctx context.Context, cfg Config) (Inventory, error) {
	raw, err := dockerOutput(ctx, cfg, "info", "--format", "{{json .}}")
	if err != nil {
		return Inventory{}, err
	}
	var info struct {
		ID       string
		MemTotal int64
		NCPU     int64
		OSType   string
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		return Inventory{}, fmt.Errorf("decode engine identity: %w", err)
	}
	if info.ID == "" || info.MemTotal <= 0 || info.NCPU <= 0 || info.OSType != "linux" {
		return Inventory{}, errors.New("Docker must report a Linux engine identity and positive memory/CPU capacity")
	}
	if cfg.EngineID != "" && cfg.EngineID != info.ID {
		return Inventory{}, errors.New("Docker engine identity changed; human manager reconfiguration is required")
	}
	return Inventory{EngineID: info.ID, Capacity: Resources{MemoryBytes: info.MemTotal, NanoCPUs: info.NCPU * 1e9}, Projects: map[string]Observation{}}, nil
}

type dockerContainer struct {
	ID     string `json:"Id"`
	Name   string
	Config struct{ Labels map[string]string }
	State  struct {
		Running    bool
		Restarting bool
		OOMKilled  bool
		Health     *struct{ Status string }
	}
	HostConfig struct {
		Memory     int64
		NanoCpus   int64
		CpuQuota   int64
		CpuPeriod  int64
		CpusetCpus string
	}
	NetworkSettings struct {
		Ports map[string][]struct {
			HostIp   string
			HostPort string
		}
	}
}

func containerInventory(ctx context.Context, cfg Config) ([]dockerContainer, error) {
	raw, err := dockerOutput(ctx, cfg, "ps", "-a", "-q", "--no-trunc")
	if err != nil {
		return nil, err
	}
	ids := strings.Fields(string(raw))
	if len(ids) == 0 {
		return nil, nil
	}
	if len(ids) > 4096 {
		return nil, errors.New("container inventory exceeds the supported management bound")
	}
	args := append([]string{"container", "inspect"}, ids...)
	raw, err = dockerOutput(ctx, cfg, args...)
	if err != nil {
		return nil, err
	}
	var all []dockerContainer
	if err := json.Unmarshal(raw, &all); err != nil {
		return nil, err
	}
	if len(all) != len(ids) {
		return nil, errors.New("incomplete container inventory")
	}
	return all, nil
}

// cpuArg renders a CPU quantity for Docker and Compose; zero is unlimited.
func cpuArg(nano int64) string {
	if nano <= 0 {
		return "0"
	}
	return strconv.FormatFloat(float64(nano)/1e9, 'f', 9, 64)
}

// capsHonored reports whether a container enforces every cap in want. A zero
// dimension of want is unlimited and requires nothing.
func capsHonored(caps, want Resources) bool {
	return (want.MemoryBytes <= 0 || caps.MemoryBytes > 0) && (want.NanoCPUs <= 0 || caps.NanoCPUs > 0)
}

// helperCapsMatch reports whether a running helper is capped within every
// configured dimension of the helper allowance.
func helperCapsMatch(caps, allowance Resources) bool {
	return (allowance.MemoryBytes <= 0 || caps.MemoryBytes > 0 && caps.MemoryBytes <= allowance.MemoryBytes) &&
		(allowance.NanoCPUs <= 0 || caps.NanoCPUs > 0 && caps.NanoCPUs <= allowance.NanoCPUs)
}

// configuredCaps returns the caps a project container of the given Compose
// service must enforce.
func configuredCaps(l Limits, service string) Resources {
	switch service {
	case "runtime":
		return l.Runtime
	case "gateway":
		return l.Gateway
	}
	return maxRes(l.Runtime, l.Gateway)
}

func containerCaps(c dockerContainer) Resources {
	cpu := c.HostConfig.NanoCpus
	if cpu <= 0 && c.HostConfig.CpuQuota > 0 {
		period := c.HostConfig.CpuPeriod
		if period <= 0 {
			period = 100000
		}
		cpu = c.HostConfig.CpuQuota * 1e9 / period
	}
	if cpu <= 0 && c.HostConfig.CpusetCpus != "" {
		count := int64(0)
		for _, span := range strings.Split(c.HostConfig.CpusetCpus, ",") {
			a, b, has := strings.Cut(span, "-")
			lo, err := strconv.ParseInt(a, 10, 32)
			if err != nil || lo < 0 {
				return Resources{MemoryBytes: c.HostConfig.Memory}
			}
			hi := lo
			if has {
				hi, err = strconv.ParseInt(b, 10, 32)
			}
			if err != nil || hi < lo || hi-lo > 4096 {
				return Resources{MemoryBytes: c.HostConfig.Memory}
			}
			count += hi - lo + 1
		}
		cpu = count * 1e9
	}
	return Resources{MemoryBytes: c.HostConfig.Memory, NanoCPUs: cpu}
}

func portOwned(c dockerContainer, p Project) bool {
	ports := c.NetworkSettings.Ports["8080/tcp"]
	return len(ports) == 1 && ports[0].HostIp == "127.0.0.1" && ports[0].HostPort == strconv.Itoa(p.Port)
}

func (b DockerBackend) Inspect(ctx context.Context, cfg Config, projects []Project) (Inventory, error) {
	inv, err := InspectEngine(ctx, cfg)
	if err != nil {
		return Inventory{}, err
	}
	all, err := containerInventory(ctx, cfg)
	if err != nil {
		return Inventory{}, err
	}
	known := make(map[string]Project, len(projects))
	for _, p := range projects {
		known[p.ID] = p
		inv.Projects[p.ID] = Observation{State: "stopped"}
	}
	runtimes, gateways := map[string]int{}, map[string]int{}
	for _, c := range all {
		labels := c.Config.Labels
		if labels[managerLabel] == cfg.ID && labels[helperLabel] == "true" {
			if c.State.Running {
				caps := containerCaps(c)
				if !helperCapsMatch(caps, cfg.Policy.Helpers) {
					return Inventory{}, errors.New("managed helper resource limits do not match its reserved allowance")
				}
			}
			continue
		}
		id := labels[projectLabel]
		p, ours := known[id]
		ours = ours && labels[managerLabel] == cfg.ID
		if labels[managerLabel] == cfg.ID && !ours {
			return Inventory{}, errors.New("unrecognized container claims this manager; reconcile its ownership before admission")
		}
		if !ours {
			for _, p := range projects {
				if labels["com.docker.compose.project"] == p.ComposeProject {
					o := inv.Projects[p.ID]
					o.State = "unknown"
					o.Blocker = "Compose project is occupied by an unowned container"
					inv.Projects[p.ID] = o
				}
			}
			if c.State.Running {
				caps := containerCaps(c)
				inv.External.MemoryBytes += max(caps.MemoryBytes, 0)
				inv.External.NanoCPUs += max(caps.NanoCPUs, 0)
				if caps.MemoryBytes <= 0 {
					inv.UnboundedMemory = append(inv.UnboundedMemory, strings.TrimPrefix(c.Name, "/"))
				}
				if caps.NanoCPUs <= 0 {
					inv.UnboundedCPU = append(inv.UnboundedCPU, strings.TrimPrefix(c.Name, "/"))
				}
			}
			continue
		}
		o := inv.Projects[id]
		if c.State.OOMKilled {
			o.Blocker = "container was OOM-killed; review limits before restarting"
		}
		if !c.State.Running {
			inv.Projects[id] = o
			continue
		}
		caps := containerCaps(c)
		o.Enforced.MemoryBytes += max(caps.MemoryBytes, 0)
		o.Enforced.NanoCPUs += max(caps.NanoCPUs, 0)
		if !capsHonored(caps, configuredCaps(p.Limits, labels["com.docker.compose.service"])) {
			o.State = "unknown"
			o.Blocker = "managed container lacks the resource caps configured for its project"
		}
		if o.State != "unknown" {
			o.State = "partial"
		}
		if labels["com.docker.compose.project"] == p.ComposeProject {
			switch labels["com.docker.compose.service"] {
			case "runtime":
				runtimes[id]++
			case "gateway":
				gateways[id]++
				o.PortOwned = portOwned(c, p)
			}
		}
		if c.State.Restarting || c.State.Health == nil || c.State.Health.Status != "healthy" {
			o.Blocker = "one or more owned services are not healthy"
		}
		inv.Projects[id] = o
	}
	for id, o := range inv.Projects {
		if runtimes[id] > 1 || gateways[id] > 1 {
			o.State = "unknown"
			o.Blocker = "duplicate owned service containers"
		}
		if o.State != "unknown" && runtimes[id] == 1 && gateways[id] == 1 && o.PortOwned && o.Blocker == "" {
			o.State = "running"
		}
		if gateways[id] > 0 && !o.PortOwned {
			o.State = "unknown"
			o.Blocker = "Gateway published port does not match the authorized origin"
		}
		inv.Projects[id] = o
	}
	b.measure(ctx, cfg, all, &inv)
	return inv, nil
}

func (b DockerBackend) measure(ctx context.Context, cfg Config, all []dockerContainer, inv *Inventory) {
	byID := map[string]string{}
	args := []string{"stats", "--no-stream", "--no-trunc", "--format", "{{json .}}"}
	for _, c := range all {
		if c.State.Running && c.Config.Labels[managerLabel] == cfg.ID && c.Config.Labels[helperLabel] != "true" {
			byID[c.ID] = c.Config.Labels[projectLabel]
			args = append(args, c.ID)
		}
	}
	if len(byID) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	raw, err := dockerOutput(ctx, cfg, args...)
	if err != nil {
		return
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	for {
		var s struct {
			ID       string
			CPUPerc  string
			MemUsage string
		}
		if err := dec.Decode(&s); err != nil {
			break
		}
		id, ok := byID[s.ID]
		if !ok {
			continue
		}
		mem, _, _ := strings.Cut(s.MemUsage, " /")
		memory, merr := parseMemory(mem)
		cpu, cerr := strconv.ParseFloat(strings.TrimSuffix(s.CPUPerc, "%"), 64)
		if merr != nil || cerr != nil {
			continue
		}
		o := inv.Projects[id]
		o.Usage.MemoryBytes += memory
		o.Usage.NanoCPUs += int64(cpu * 1e7)
		o.UsageAvailable = true
		inv.Projects[id] = o
	}
	inv.MeasuredAt = time.Now().UTC()
}

func parseMemory(value string) (int64, error) {
	value = strings.TrimSpace(value)
	i := 0
	for i < len(value) && (value[i] >= '0' && value[i] <= '9' || value[i] == '.') {
		i++
	}
	if i == 0 {
		return 0, fmt.Errorf("invalid memory quantity %q", value)
	}
	n, err := strconv.ParseFloat(value[:i], 64)
	if err != nil || n < 0 {
		return 0, errors.New("invalid memory quantity")
	}
	units := map[string]float64{"": 1, "b": 1, "k": 1 << 10, "kb": 1e3, "kib": 1 << 10, "m": 1 << 20, "mb": 1e6, "mib": 1 << 20, "g": 1 << 30, "gb": 1e9, "gib": 1 << 30, "t": 1 << 40, "tb": 1e12, "tib": 1 << 40}
	factor, ok := units[strings.ToLower(value[i:])]
	if !ok || n*factor > float64(1<<62) {
		return 0, errors.New("unsupported memory quantity")
	}
	return int64(n * factor), nil
}

func (b DockerBackend) VerifyConnection(ctx context.Context, cfg Config, p Project) (Connection, error) {
	if _, err := InspectEngine(ctx, cfg); err != nil {
		return Connection{}, err
	}
	all, err := containerInventory(ctx, cfg)
	if err != nil {
		return Connection{}, err
	}
	runtimes, gateways := 0, 0
	for _, container := range all {
		if !container.State.Running || container.Config.Labels["com.docker.compose.project"] != p.ComposeProject {
			continue
		}
		if container.Config.Labels[managerLabel] != cfg.ID || container.Config.Labels[projectLabel] != p.ID {
			return Connection{}, errors.New("foreign container occupies the project's Compose identity")
		}
		service := container.Config.Labels["com.docker.compose.service"]
		if service != "runtime" && service != "gateway" {
			continue
		}
		if container.State.Restarting || container.State.Health == nil || container.State.Health.Status != "healthy" {
			return Connection{}, errors.New("project services are not healthy; run lensdock project up first")
		}
		if service == "runtime" {
			runtimes++
		}
		if service == "gateway" {
			gateways++
			if !portOwned(container, p) {
				return Connection{}, errors.New("Gateway port is not owned by this project")
			}
		}
	}
	if runtimes != 1 || gateways != 1 {
		return Connection{}, errors.New("project is not running; run lensdock project up first")
	}
	if err := verifyGatewayIdentity(ctx, p); err != nil {
		return Connection{}, err
	}
	return Connection{ProjectID: p.ID, API: p.Origin, Workspace: p.Workspace, TokenFile: filepath.Join(p.StateDir, "product", "agent-token")}, nil
}

func verifyGatewayIdentity(ctx context.Context, p Project) error {
	if p.Origin != "http://127.0.0.1:"+strconv.Itoa(p.Port) {
		return errors.New("registered project origin does not match its port")
	}
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("Gateway identity redirects are refused")
	}, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.Origin+"/api/v1/deployment", nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("verify Gateway identity: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Gateway identity returned HTTP %d; refusing credentials", resp.StatusCode)
	}
	var identity struct {
		DeploymentID string `json:"deploymentId"`
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, 4097))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&identity); err != nil {
		return errors.New("Gateway identity response is invalid; refusing credentials")
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return errors.New("Gateway identity response has trailing data")
	}
	if identity.DeploymentID != p.ID {
		return errors.New("Gateway deployment identity mismatch; refusing credentials")
	}
	return nil
}

func (b DockerBackend) Execute(ctx context.Context, cfg Config, p Project, op Operation, locks []*os.File) error {
	if _, err := InspectEngine(ctx, cfg); err != nil {
		return err
	}
	all, err := containerInventory(ctx, cfg)
	if err != nil {
		return err
	}
	for _, c := range all {
		if c.Config.Labels["com.docker.compose.project"] == p.ComposeProject && (c.Config.Labels[managerLabel] != cfg.ID || c.Config.Labels[projectLabel] != p.ID) {
			return errors.New("authorized Compose project contains a foreign container")
		}
		if c.State.Running && c.Config.Labels[managerLabel] == cfg.ID && c.Config.Labels[helperLabel] == "true" {
			return errors.New("an earlier lifecycle helper is still running; retain its reservation and reconcile it before proceeding")
		}
	}
	depPath := filepath.Join(p.StateDir, "deployment.json")
	dep, err := update.LoadDeployment(depPath)
	if err != nil && op.Kind == "start" && errors.Is(err, os.ErrNotExist) {
		bundle, berr := product.OpenBundle(cfg.BundleDir)
		if berr != nil {
			return berr
		}
		dep, err = product.Initialize(product.InitOptions{StateDir: p.StateDir, Origin: p.Origin, WorkspaceID: p.Workspace, Mode: "AGENT", Project: p.ComposeProject, RuntimeImage: cfg.RuntimeImage, GatewayImage: cfg.GatewayImage, Bundle: bundle, DeploymentID: p.ID, RequirePairing: cfg.RequirePairing, LocalAutoEntry: cfg.LocalAutoEntry, Profile: cfg.Profile, DestinationsDir: cfg.DestinationsDir, DestinationName: destinationName(p.Root), ManagedBy: &update.ManagedOwner{ManagerID: cfg.ID, ProjectID: p.ID, ManagerRoot: cfg.Root}})
	}
	if err != nil {
		if op.Kind == "stop" && errors.Is(err, os.ErrNotExist) {
			for _, c := range all {
				if c.State.Running && c.Config.Labels[managerLabel] == cfg.ID && c.Config.Labels[projectLabel] == p.ID {
					return errors.New("owned containers remain but their deployment is missing; refusing to release the reservation")
				}
			}
			return nil
		}
		return err
	}
	if dep.ManagedBy == nil || dep.ManagedBy.ManagerID != cfg.ID || dep.ManagedBy.ProjectID != p.ID || dep.ManagedBy.ManagerRoot != cfg.Root || dep.Compose.Project != p.ComposeProject || dep.StateDir != filepath.Join(p.StateDir, "product") {
		return errors.New("deployment ownership differs from the authorized project")
	}
	if op.Kind != "stop" {
		if err := ensureResourceProfile(cfg, p, dep); err != nil {
			return err
		}
	}
	docker := DeploymentDocker(cfg, dep, locks)
	log := b.Log
	if log == nil {
		log = io.Discard
	}
	engine := update.NewEngine(dep, docker, log)
	switch op.Kind {
	case "start":
		gatewayRunning := false
		for _, c := range all {
			if c.Config.Labels["com.docker.compose.project"] != p.ComposeProject || !c.State.Running || c.Config.Labels["com.docker.compose.service"] != "gateway" {
				continue
			}
			if !portOwned(c, p) {
				return errors.New("existing Gateway port ownership is invalid")
			}
			gatewayRunning = true
		}
		if !gatewayRunning {
			ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p.Port))
			if err != nil {
				return fmt.Errorf("authorized origin is occupied; its port will not be changed: %w", err)
			}
			ln.Close()
		}
		if err := engine.CheckProjectStart(); err != nil {
			return err
		}
		if _, _, err := update.InitDeployment(dep); err != nil {
			return err
		}
		if err := product.Start(ctx, dep, docker, product.StartOptions{PullPolicy: cfg.PullPolicy}, log); err != nil {
			return err
		}
		if err := verifyGatewayIdentity(ctx, p); err != nil {
			// A Gateway that does not answer as this project must not stay listed.
			withdrawDestination(ctx, p.StateDir)
			return err
		}
		return nil
	case "stop":
		if err := engine.StopProject(ctx, p.Workspace, update.ProjectStopOptions{DiscardPageState: op.DiscardPageState, Force: op.Force, ExpectedGeneration: op.ExpectedGeneration}); err != nil {
			return err
		}
		withdrawDestination(ctx, p.StateDir)
		return nil
	case "update":
		return executeUpdate(ctx, engine, op.Update)
	default:
		return errors.New("unknown lifecycle operation")
	}
}

// capLines renders the Compose cap lines of one service. A zero quantity
// explicitly disables that cap, which also overrides any default cap of the
// base Compose file; nothing is replaced by a large placeholder.
func capLines(r Resources) string {
	mem := "0"
	if r.MemoryBytes > 0 {
		mem = strconv.FormatInt(r.MemoryBytes, 10)
	}
	return fmt.Sprintf("    mem_limit: %s\n    memswap_limit: %s\n    cpus: %s\n", mem, mem, cpuArg(r.NanoCPUs))
}

// resourceProfile is the exact Compose override of a project's authorized
// limits. Positive quantities render exactly as in earlier releases, so the
// profile of an existing explicit policy never changes.
func resourceProfile(cfg Config, p Project) string {
	var text strings.Builder
	text.WriteString("services:\n")
	for _, svc := range []struct {
		name      string
		resources Resources
	}{{"runtime", p.Limits.Runtime}, {"gateway", p.Limits.Gateway}} {
		fmt.Fprintf(&text, "  %s:\n%s    labels:\n      %s: %q\n      %s: %q\n", svc.name, capLines(svc.resources), managerLabel, cfg.ID, projectLabel, p.ID)
	}
	// Compose resolves helper caps from the manager's locked operation policy.
	// Changing that policy must not rewrite the authorized project pair.
	fmt.Fprintf(&text, "  config-init:\n    mem_limit: ${LENSDOCK_MANAGER_HELPER_MEMORY:?managed helper policy is required}\n    memswap_limit: ${LENSDOCK_MANAGER_HELPER_MEMORY:?managed helper policy is required}\n    cpus: ${LENSDOCK_MANAGER_HELPER_CPUS:?managed helper policy is required}\n    labels:\n      %s: %q\n      %s: %q\n      %s: \"true\"\n", managerLabel, cfg.ID, projectLabel, p.ID, helperLabel)
	return text.String()
}

// reconcileResourceProfile moves an established resource profile to new
// limits for an authorized reconfiguration. The file is replaced only when it
// is still exactly the profile of the project's current limits (or already the
// requested one after an interrupted attempt); any other content is tampering
// and is refused. A project that never started has no profile yet.
func reconcileResourceProfile(cfg Config, p Project, next Limits) error {
	file := filepath.Join(p.StateDir, "product", "resources.yaml")
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	target := p
	target.Limits = next
	want := resourceProfile(cfg, target)
	switch string(data) {
	case want:
		return nil
	case resourceProfile(cfg, p):
		return writeFileAtomic(file, []byte(want), 0o600)
	}
	return errors.New("existing resource profile differs from the authorized project; refusing silent resize")
}

// helperPolicyProfile is the fixed Compose override that applies the
// manager's helper PID cap to config-init. It carries no value of its own (the
// cap is interpolated from the trusted operation policy), so a policy change
// never rewrites it or the authorized project profile, and it also covers
// release copies whose base Compose file predates the setting. Its position
// after the base files makes the manager value win over a deployment one.
const helperPolicyProfile = "services:\n  config-init:\n    pids_limit: ${LENSDOCK_MANAGER_HELPER_PIDS:?managed helper policy is required}\n"

func ensureResourceProfile(cfg Config, p Project, dep *update.Deployment) error {
	changed := false
	for _, profile := range []struct{ name, text, what string }{
		{"resources.yaml", resourceProfile(cfg, p), "resource profile differs from the authorized project; refusing silent resize"},
		{product.HelperPolicyFile, helperPolicyProfile, "helper policy profile differs from the manager's; refusing to use it"},
	} {
		file := filepath.Join(dep.StateDir, profile.name)
		data, err := os.ReadFile(file)
		if err == nil {
			if string(data) != profile.text {
				return errors.New("existing " + profile.what)
			}
		} else if errors.Is(err, os.ErrNotExist) {
			if err := auth.WritePrivateFile(file, []byte(profile.text)); err != nil {
				return err
			}
		} else {
			return err
		}
		if !slices.Contains(dep.Compose.Files, file) {
			dep.Compose.Files = append(dep.Compose.Files, file)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return dep.Save()
}

func executeUpdate(ctx context.Context, engine *update.Engine, action *UpdateAction) error {
	if action == nil {
		return errors.New("update operation has no typed action")
	}
	switch action.Kind {
	case "apply":
		return managedApplyOutcome(engine.Apply(ctx, update.ApplyOptions{PlanFile: action.PlanFile, DryRun: action.DryRun, Confirmed: action.Confirmed, ApproveSwitch: action.ApproveSwitch, RevokeLeases: action.RevokeLeases, AcceptCrashSnapshot: action.AcceptCrashSnapshot}))
	case "approve":
		_, err := engine.Approve(ctx, update.ApproveOptions{UpdateID: action.UpdateID, Confirmed: action.Confirmed})
		return err
	case "abort":
		_, err := engine.Abort(ctx, update.AbortOptions{UpdateID: action.UpdateID, KeepCandidate: action.KeepCandidate})
		return err
	case "rollback":
		_, err := engine.Rollback(ctx, update.RollbackOptions{CheckpointID: action.CheckpointID, DryRun: action.DryRun, Confirmed: action.Confirmed, RevokeLeases: action.RevokeLeases, AllowCrashCheckpoint: action.AllowCrashCheckpoint})
		return err
	case "verify":
		_, err := engine.VerifyCheckpoint(action.CheckpointID)
		return err
	case "prune":
		_, err := engine.PruneCheckpoints()
		return err
	case "policy":
		if !action.Confirmed || action.AfterDays == nil {
			return update.ErrNotConfirmed
		}
		return engine.SetExpireAfterDays(*action.AfterDays)
	case "expire":
		if action.Automatic {
			// Scheduled housekeeping: the engine applies the deployment
			// setting (0 disables) and its own 24h throttle.
			_, _, err := engine.AutoExpire(ctx)
			return err
		}
		if !action.Confirmed && !action.DryRun {
			return update.ErrNotConfirmed
		}
		_, err := engine.ExpireUnused(ctx, update.ExpireOptions{Scopes: action.Scopes, AfterDays: action.AfterDays, DryRun: action.DryRun})
		return err
	case "delete":
		if !action.Confirmed {
			return update.ErrNotConfirmed
		}
		return engine.DeleteBranch(ctx, action.Volume)
	default:
		return errors.New("unsupported managed update action")
	}
}

func managedApplyOutcome(result *update.ApplyResult, err error) error {
	if err != nil {
		return err
	}
	if result != nil && result.Status == update.JournalAwaitingApproval {
		return fmt.Errorf("update %s: %w", result.UpdateID, update.ErrAwaitingApproval)
	}
	return nil
}
