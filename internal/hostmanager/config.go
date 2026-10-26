package hostmanager

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/JiaWeiXie/lensdock/internal/product"
)

const (
	configVersion   = 1
	registryVersion = 1

	defaultPortFirst = 19081
	defaultPortLast  = 19999
	// standalonePort is the established single-deployment port, never managed.
	standalonePort = 18081

	// maxSocketPath keeps the Unix socket path below the smallest sun_path
	// limit among supported platforms (104 bytes on macOS) with margin.
	maxSocketPath = 100

	gib = int64(1) << 30
	mib = int64(1) << 20

	configName     = "config.json"
	registryName   = "registry.json"
	adminTokenName = "admin.token"
	serveLockName  = "manager.lock"
	launchLockName = "launch.lock"
	socketName     = "manager.sock"
	bundleName     = "bundle"
	logName        = "manager.log"
	helperLockName = "helpers.lock"
	manifestName   = "manifest.json"
	begunName      = "begun.json"
	receiptName    = "receipt.json"
	workerLogName  = "worker.log"
	projectToken   = "project.token"

	maxJSONFile = 8 << 20
)

var (
	idPattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)
	projectPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)
	opPattern      = regexp.MustCompile(`^op-[0-9a-f]{16}$`)
	waiterPattern  = regexp.MustCompile(`^w-[0-9a-f]{16}$`)
)

// snapshotBundle copies the trusted release bundle into dest. Tests replace it.
var snapshotBundle = func(src, dest string) error {
	b, err := product.OpenBundle(src)
	if err != nil {
		return err
	}
	return b.Snapshot(dest)
}

// DefaultRoot is the manager state directory. It is independent of the
// standalone deployment environment.
func DefaultRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home directory: %w", err)
	}
	return filepath.Join(home, ".local", "share", "lensdock-manager"), nil
}

// DefaultPolicy has no resource limits: no manager budget, helper allowance,
// headroom, project caps or helper PID cap. Zero means unlimited in each
// dimension; every limit is an explicit opt-in. It allows uncapped external
// containers, so opting into one project cap never adds an unrelated
// prohibition; AllowUnboundedExternal=false remains an explicit option.
// Initialize stores exactly the policy it is given.
func DefaultPolicy() Policy { return Policy{AllowUnboundedExternal: true} }

// AdminTokenFile is the human credential file of the manager at root.
func AdminTokenFile(root string) string { return filepath.Join(root, adminTokenName) }

func socketPath(root string) string          { return filepath.Join(root, "run", socketName) }
func projectStateDir(root, id string) string { return filepath.Join(root, "projects", id) }
func grantDir(root, id string) string        { return filepath.Join(root, "grants", id) }
func projectTokenFile(root, id string) string {
	return filepath.Join(grantDir(root, id), projectToken)
}
func projectLockPath(root, id string) string { return filepath.Join(root, "locks", id+".lock") }
func helperLockPath(root string) string      { return filepath.Join(root, "locks", helperLockName) }
func opDir(root, id string) string           { return filepath.Join(root, "ops", id) }

var privateSubdirs = []string{"run", "projects", "grants", "ops", "locks", "logs"}

func pairOf(l Limits) Resources { return addRes(l.Runtime, l.Gateway) }

func addRes(a, b Resources) Resources {
	return Resources{MemoryBytes: a.MemoryBytes + b.MemoryBytes, NanoCPUs: a.NanoCPUs + b.NanoCPUs}
}

func subRes(a, b Resources) Resources {
	return Resources{MemoryBytes: a.MemoryBytes - b.MemoryBytes, NanoCPUs: a.NanoCPUs - b.NanoCPUs}
}

func maxRes(a, b Resources) Resources {
	return Resources{MemoryBytes: max(a.MemoryBytes, b.MemoryBytes), NanoCPUs: max(a.NanoCPUs, b.NanoCPUs)}
}

// maxQuantity bounds every configured quantity so sums of quantities cannot
// overflow.
const maxQuantity = int64(1) << 60

// fitsLimit reports whether a is within limit on every configured dimension.
// A zero limit dimension is unlimited and always fits.
func (a Resources) fitsLimit(limit Resources) bool {
	return (limit.MemoryBytes == 0 || a.MemoryBytes <= limit.MemoryBytes) &&
		(limit.NanoCPUs == 0 || a.NanoCPUs <= limit.NanoCPUs)
}

func (a Resources) isZero() bool { return a.MemoryBytes == 0 && a.NanoCPUs == 0 }

func (a Resources) valid() bool {
	return a.MemoryBytes >= 0 && a.MemoryBytes <= maxQuantity && a.NanoCPUs >= 0 && a.NanoCPUs <= maxQuantity
}

// fitsEngine reports whether total is within capacity in every dimension that
// demand reserves. A dimension the operation does not reserve is unlimited and
// has no physical-capacity requirement.
func fitsEngine(total, capacity, demand Resources) bool {
	return (demand.MemoryBytes == 0 || total.MemoryBytes <= capacity.MemoryBytes) &&
		(demand.NanoCPUs == 0 || total.NanoCPUs <= capacity.NanoCPUs)
}

// missingBudgetCap explains which budgeted dimension has no positive cap on
// both the Runtime and the Gateway, or returns an empty string. Without those
// caps a budget would be unenforceable.
func missingBudgetCap(l Limits, budget Resources) string {
	for _, d := range []struct {
		name           string
		budget, rt, gw int64
	}{
		{"memory", budget.MemoryBytes, l.Runtime.MemoryBytes, l.Gateway.MemoryBytes},
		{"CPU", budget.NanoCPUs, l.Runtime.NanoCPUs, l.Gateway.NanoCPUs},
	} {
		if d.budget > 0 && (d.rt <= 0 || d.gw <= 0) {
			return fmt.Sprintf("the project has no %[1]s limit on its Runtime or Gateway while the manager enforces a %[1]s budget; set both %[1]s limits or remove the budget", d.name)
		}
	}
	return ""
}

// budgetBlocker explains why limits cannot be accounted in budget, or returns
// an empty string.
func budgetBlocker(l Limits, budget Resources) string {
	if msg := missingBudgetCap(l, budget); msg != "" {
		return msg
	}
	if !pairOf(l).fitsLimit(budget) {
		return "the project's Runtime and Gateway limits exceed the managed resource budget"
	}
	return ""
}

// validatePolicy checks structure only. Every quantity is optional: zero means
// no configured limit, and positive values are enforced as stated.
func validatePolicy(p Policy) error {
	for _, q := range []struct {
		label string
		r     Resources
	}{
		{"managed budget", p.Budget},
		{"helper allowance", p.Helpers},
		{"headroom", p.Headroom},
		{"unbounded external allowance", p.UnboundedExternalAllowance},
		{"project Runtime limits", p.ProjectLimits.Runtime},
		{"project Gateway limits", p.ProjectLimits.Gateway},
	} {
		if !q.r.valid() {
			return fmt.Errorf("policy: %s must be between 0 and 2^60", q.label)
		}
	}
	if p.HelperPidsLimit < 0 || p.HelperPidsLimit > maxQuantity {
		return errors.New("policy: helper PID limit must be between 0 and 2^60")
	}
	if !p.UnboundedExternalAllowance.isZero() && !p.AllowUnboundedExternal {
		return errors.New("policy: an unbounded external allowance requires allowUnboundedExternal")
	}
	if msg := budgetBlocker(p.ProjectLimits, p.Budget); msg != "" {
		return errors.New("policy: " + msg)
	}
	return nil
}

func validateExecutable(label, path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("%s must be a canonical absolute path", label)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%s is not an executable file", label)
	}
	return nil
}

func validateConfig(cfg Config, bundleExists bool) error {
	if cfg.Version != configVersion {
		return fmt.Errorf("unsupported manager config version %d", cfg.Version)
	}
	if !idPattern.MatchString(cfg.ID) {
		return errors.New("invalid manager ID")
	}
	if !filepath.IsAbs(cfg.Root) || filepath.Clean(cfg.Root) != cfg.Root {
		return errors.New("manager root must be a canonical absolute path")
	}
	if err := validateExecutable("manager executable", cfg.Executable); err != nil {
		return err
	}
	if err := validateExecutable("Docker binary", cfg.DockerBinary); err != nil {
		return err
	}
	if !filepath.IsAbs(cfg.BundleDir) || filepath.Clean(cfg.BundleDir) != cfg.BundleDir {
		return errors.New("bundle directory must be a canonical absolute path")
	}
	if fi, err := os.Stat(cfg.BundleDir); bundleExists && (err != nil || !fi.IsDir()) {
		return errors.New("bundle directory does not exist")
	}
	if cfg.EngineID == "" {
		return errors.New("the Docker engine identity is required")
	}
	for label, v := range map[string]string{
		"Docker context": cfg.DockerContext, "Docker host": cfg.DockerHost, "Docker config": cfg.DockerConfig,
		"engine ID": cfg.EngineID, "Runtime image": cfg.RuntimeImage, "Gateway image": cfg.GatewayImage,
	} {
		if strings.ContainsAny(v, "\x00\n\r") {
			return fmt.Errorf("%s contains control characters", label)
		}
	}
	if cfg.DockerConfig != "" && !filepath.IsAbs(cfg.DockerConfig) {
		return errors.New("Docker config directory must be absolute")
	}
	if cfg.RuntimeImage == "" || cfg.GatewayImage == "" || strings.ContainsAny(cfg.RuntimeImage+cfg.GatewayImage, " \t") {
		return errors.New("Runtime and Gateway images are required and must not contain spaces")
	}
	switch cfg.PullPolicy {
	case "always", "missing", "never":
	default:
		return errors.New("pull policy must be always, missing or never")
	}
	if cfg.PortFirst < 1024 || cfg.PortLast > 65535 || cfg.PortFirst > cfg.PortLast {
		return errors.New("manager port range must satisfy 1024 <= first <= last <= 65535")
	}
	if cfg.PortFirst <= standalonePort && standalonePort <= cfg.PortLast {
		return fmt.Errorf("manager port range must exclude the standalone port %d", standalonePort)
	}
	if len(socketPath(cfg.Root)) > maxSocketPath {
		return fmt.Errorf("manager root %q is too long for a Unix socket (%d bytes, limit %d); choose a shorter --root", cfg.Root, len(socketPath(cfg.Root)), maxSocketPath)
	}
	return validatePolicy(cfg.Policy)
}

// canonicalRoot resolves an existing manager root to its symlink-free path.
func canonicalRoot(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("manager root: %w", err)
	}
	return real, nil
}

// Initialize creates a private manager root, its registry and admin token,
// snapshots the trusted bundle into the root and writes the configuration
// last, so an interrupted run leaves no initialized manager. It refuses to
// overwrite an initialized root.
func Initialize(cfg Config) (Config, error) {
	if cfg.Root == "" {
		root, err := DefaultRoot()
		if err != nil {
			return Config{}, err
		}
		cfg.Root = root
	}
	abs, err := filepath.Abs(cfg.Root)
	if err != nil {
		return Config{}, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return Config{}, fmt.Errorf("create manager root parent: %w", err)
	}
	if err := ensurePrivateDir(abs); err != nil {
		return Config{}, fmt.Errorf("manager root: %w", err)
	}
	root, err := canonicalRoot(abs)
	if err != nil {
		return Config{}, err
	}
	cfg.Root = root
	if cfg.Version == 0 {
		cfg.Version = configVersion
	}
	if cfg.ID == "" {
		suffix, err := randomHex(8)
		if err != nil {
			return Config{}, err
		}
		cfg.ID = "mgr-" + suffix
	}
	if cfg.PullPolicy == "" {
		cfg.PullPolicy = "always"
	}
	if cfg.PortFirst == 0 && cfg.PortLast == 0 {
		cfg.PortFirst, cfg.PortLast = defaultPortFirst, defaultPortLast
	}
	source := cfg.BundleDir
	if !filepath.IsAbs(source) || filepath.Clean(source) != source {
		return Config{}, errors.New("bundle directory must be a canonical absolute path")
	}
	cfg.BundleDir = filepath.Join(root, bundleName)
	// The bundle path is validated after the snapshot exists; check everything
	// else first so a bad request changes nothing but the private root.
	if err := validateConfig(cfg, false); err != nil {
		return Config{}, err
	}

	lock, busy, err := tryFlock(filepath.Join(root, launchLockName))
	if err != nil {
		return Config{}, err
	}
	if busy {
		return Config{}, errors.New("another manager initialization or launch is running")
	}
	defer lock.Close()
	if _, err := os.Lstat(filepath.Join(root, configName)); err == nil {
		return Config{}, fmt.Errorf("manager at %s is already initialized; refusing to overwrite it", root)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Config{}, err
	}
	for _, sub := range privateSubdirs {
		if err := ensurePrivateDir(filepath.Join(root, sub)); err != nil {
			return Config{}, err
		}
	}
	if err := installBundle(source, cfg.BundleDir); err != nil {
		return Config{}, err
	}
	executable := filepath.Join(root, "lensdock")
	if err := installExecutable(cfg.Executable, executable); err != nil {
		return Config{}, err
	}
	cfg.Executable = executable
	if err := validateConfig(cfg, true); err != nil {
		return Config{}, err
	}
	// Files below are leftovers of an interrupted initialization when no
	// config exists; nobody has received this token yet.
	token, err := randomHex(32)
	if err != nil {
		return Config{}, err
	}
	if err := writeFileAtomic(AdminTokenFile(root), []byte(token+"\n"), 0o600); err != nil {
		return Config{}, err
	}
	if err := writeJSONAtomic(filepath.Join(root, registryName), newRegistry(), 0o600); err != nil {
		return Config{}, err
	}
	if err := writeJSONAtomic(filepath.Join(root, configName), cfg, 0o600); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// installBundle snapshots src next to dest and renames it into place. A
// leftover dest without config.json belongs to an interrupted initialization.
func installBundle(src, dest string) error {
	staging, err := os.MkdirTemp(filepath.Dir(dest), ".bundle-staging-")
	if err != nil {
		return fmt.Errorf("create bundle staging directory: %w", err)
	}
	defer os.RemoveAll(staging)
	snapshot := filepath.Join(staging, bundleName)
	if err := snapshotBundle(src, snapshot); err != nil {
		return fmt.Errorf("snapshot release bundle: %w", err)
	}
	if err := os.RemoveAll(dest); err != nil {
		return fmt.Errorf("remove interrupted bundle snapshot: %w", err)
	}
	if err := os.Rename(snapshot, dest); err != nil {
		return fmt.Errorf("install bundle snapshot: %w", err)
	}
	return syncDir(filepath.Dir(dest))
}

// installExecutable keeps daemon and worker startup independent of the release
// directory selected during initialization. The committed copy is executable
// only by the owner; callers cannot replace it through the management protocol.
func installExecutable(src, dest string) error {
	in, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 || info.Size() <= 0 || info.Size() > 256<<20 {
		return errors.New("manager executable must be a regular executable file no larger than 256 MiB")
	}
	out, err := os.CreateTemp(filepath.Dir(dest), ".executable-")
	if err != nil {
		return err
	}
	defer os.Remove(out.Name())
	defer out.Close()
	n, err := io.Copy(out, io.LimitReader(in, info.Size()+1))
	if err != nil {
		return err
	}
	if n != info.Size() {
		return errors.New("manager executable changed while copying")
	}
	if err := out.Chmod(0o700); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Rename(out.Name(), dest); err != nil {
		return err
	}
	return syncDir(filepath.Dir(dest))
}

// LoadConfig reads and validates the configuration of an initialized manager.
func LoadConfig(root string) (Config, error) {
	canonical, err := canonicalRoot(root)
	if err != nil {
		return Config{}, err
	}
	if err := checkPrivateDir(canonical); err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := readPrivateJSON(filepath.Join(canonical, configName), 1<<20, &cfg); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Config{}, fmt.Errorf("no initialized manager at %s", canonical)
		}
		return Config{}, err
	}
	if cfg.Root != canonical {
		return Config{}, fmt.Errorf("manager config belongs to %s, not %s; the directory was moved or copied", cfg.Root, canonical)
	}
	if err := validateConfig(cfg, true); err != nil {
		return Config{}, fmt.Errorf("manager config: %w", err)
	}
	return cfg, nil
}

func saveConfig(cfg Config) error {
	return writeJSONAtomic(filepath.Join(cfg.Root, configName), cfg, 0o600)
}
