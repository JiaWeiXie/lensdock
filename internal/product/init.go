// Package product creates and starts the Docker deployment of LensDock: the
// private configuration and credentials of a state directory, the pinned
// images of the first start and the Compose lifecycle of an initialized
// deployment. The standalone CLI and the host manager share it; callers choose
// defaults, the Docker runner and where output goes.
package product

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/JiaWeiXie/lensdock/internal/auth"
	"github.com/JiaWeiXie/lensdock/internal/gateway"
	"github.com/JiaWeiXie/lensdock/internal/update"
)

// Defaults and environment variables of the standalone product.
const (
	DefaultProject   = "lensdock-product"
	DefaultOrigin    = "http://127.0.0.1:18081"
	DefaultWorkspace = "default"

	EnvPublicOrigin   = "LENSDOCK_PUBLIC_ORIGIN"
	EnvComposeProject = "LENSDOCK_COMPOSE_PROJECT"
	EnvRuntimeImage   = "LENSDOCK_RUNTIME_IMAGE"
	EnvGatewayImage   = "LENSDOCK_GATEWAY_IMAGE"
	EnvMode           = "LENSDOCK_MODE"
	EnvPull           = "LENSDOCK_PULL"

	// A release bundle starts on the published images; a source checkout on
	// the images `mise run image:build` and `gateway:image` build locally.
	ReleaseRuntimeImage = "ghcr.io/jiaweixie/lensdock/runtime:latest"
	ReleaseGatewayImage = "ghcr.io/jiaweixie/lensdock/gateway:latest"
	SourceRuntimeImage  = "lensdock/runtime:dev"
	SourceGatewayImage  = "lensdock/gateway:dev"

	// ReleaseDir holds the private copy of the bundle files and PinFile the
	// Compose override that Start fills with the exact images of the first
	// start. Both live in the product directory.
	ReleaseDir = "release"
	PinFile    = "images.pin.yaml"
	// PinPlaceholder is the pin file until the first start. It changes nothing.
	PinPlaceholder = "# LensDock pins the exact Runtime and Gateway images in this file at the first start\n# (lensdock admin product-start). Until then it changes nothing.\nservices: {}\n"

	// StartedFile is written next to the pin file once the first start of a
	// release deployment has succeeded.
	StartedFile = "started"

	// OperatorTokenFile and RuntimeEnvFile are the Runtime operator
	// credential files that `lensdock admin init` creates in a state
	// directory.
	OperatorTokenFile = "operator-token"
	RuntimeEnvFile    = "runtime.env"

	// ProductSubdir is the directory of a state directory that holds the
	// product configuration; DeploymentFile is the durable deployment record
	// next to it.
	ProductSubdir  = "product"
	DeploymentFile = "deployment.json"
)

// DefaultImages returns the Runtime and Gateway images a deployment starts on
// when none are chosen: the published ones for a release bundle (b != nil),
// the locally built ones for a source checkout.
func DefaultImages(b *Bundle) (runtime, gateway string) {
	if b != nil {
		return ReleaseRuntimeImage, ReleaseGatewayImage
	}
	return SourceRuntimeImage, SourceGatewayImage
}

// OptionError reports an invalid caller-supplied option. Callers can show it
// as a usage error. Nothing was written or started when it is returned.
type OptionError struct{ Msg string }

func (e *OptionError) Error() string { return e.Msg }

func optionErrorf(format string, args ...any) error {
	return &OptionError{Msg: fmt.Sprintf(format, args...)}
}

// InitOptions describes one deployment to initialize.
type InitOptions struct {
	// StateDir is the private deployment state directory. Initialize creates
	// <StateDir>/product and <StateDir>/deployment.json, and reuses the
	// Runtime operator token of an earlier `lensdock admin init` there.
	StateDir string
	// Origin is the exact loopback HTTP origin of the Gateway with an
	// explicit non-default port. Required.
	Origin string
	// WorkspaceID names the Runtime workspace. Required.
	WorkspaceID string
	// Mode is the initial usage mode, NOVNC or AGENT (empty means AGENT).
	Mode string
	// Project is the Compose project name; its Docker volumes hold the data.
	// Required.
	Project string
	// RuntimeImage and GatewayImage are the images the first start resolves
	// and pins. Required; see DefaultImages.
	RuntimeImage, GatewayImage string
	// SourceDir is the source checkout whose docker/compose.yaml, bin/ helpers
	// and apps/web/dist the deployment points at. It is required when Bundle
	// is nil and ignored otherwise.
	SourceDir string
	// DeploymentID is the non-secret identity the Gateway exposes and mixes
	// into its cookie names. Empty means a standalone deployment.
	DeploymentID string
	// LocalAutoEntry opts into automatic same-origin local browser entry. Off by
	// default: every browser session then needs a pairing token.
	LocalAutoEntry bool
	// RequirePairing requires pairing even when LocalAutoEntry is set.
	RequirePairing bool
	// Bundle is the release bundle copied into the private state directory. A
	// release deployment never reads the bundle again, so removing or
	// upgrading it cannot break the deployment.
	Bundle *Bundle
	// ManagedBy records the host manager that must admit lifecycle changes of
	// the deployment. It requires DeploymentID to equal ManagedBy.ProjectID.
	ManagedBy *update.ManagedOwner
	// DestinationsDir is the host directory of published browser destinations
	// that this deployment records in compose.env. Empty means the per-user
	// default (see DefaultDestinationsDir).
	DestinationsDir string
	// DestinationName is the display name the Gateway shows and publishes for
	// this deployment. Empty derives one. It must not contain a path.
	DestinationName string
}

const (
	initLockFile      = ".product-init.lock"
	stagingName       = "product.staging"
	stagingMarkerFile = ".staging-owner"
	stagingMarker     = "lensdock product staging: created by Initialize; safe to remove while deployment.json is absent\n"
	recordFile        = ".init-record.json"
	recordMagic       = "lensdock-product-init-v1"
	maxRecordBytes    = 1 << 20
)

var workspacePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// initPlan is the validated, normalized form of InitOptions.
type initPlan struct {
	root, dir, staging string // state dir, final product dir, staging dir
	origin, host, port string
	workspace, mode    string
	project            string
	runtimeImage       string
	gatewayImage       string
	sourceDir          string
	deploymentID       string
	requirePairing     bool
	localAutoEntry     bool
	bundle             *Bundle
	managedBy          *update.ManagedOwner
	destinationsDir    string
	destinationName    string
	fingerprint        string
}

// pendingRecord is written last into a fully staged product directory. It
// carries the exact deployment.json to commit, whose paths already name the
// final product directory, so an interrupted initialization completes with
// the credentials it already generated.
type pendingRecord struct {
	Magic       string          `json:"magic"`
	Fingerprint string          `json:"fingerprint"`
	Deployment  json.RawMessage `json:"deployment"`
}

func newPlan(opts InitOptions) (*initPlan, error) {
	p := &initPlan{
		workspace: opts.WorkspaceID, project: opts.Project,
		runtimeImage: opts.RuntimeImage, gatewayImage: opts.GatewayImage,
		deploymentID: opts.DeploymentID, requirePairing: opts.RequirePairing, localAutoEntry: opts.LocalAutoEntry,
	}
	if opts.StateDir == "" {
		return nil, optionErrorf("state directory is required")
	}
	root, err := filepath.Abs(opts.StateDir)
	if err != nil {
		return nil, err
	}
	p.root = root
	p.dir = filepath.Join(root, ProductSubdir)
	p.staging = filepath.Join(root, stagingName)

	p.mode = strings.ToUpper(strings.TrimSpace(opts.Mode))
	if p.mode == "" {
		p.mode = "AGENT"
	}
	if p.mode != "AGENT" && p.mode != "NOVNC" {
		return nil, optionErrorf("mode must be NOVNC or AGENT")
	}
	u, err := url.Parse(opts.Origin)
	if err != nil || u.Host == "" || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Path != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, optionErrorf("product initialization supports a loopback HTTP origin; configure TLS explicitly before remote exposure")
	}
	port := u.Port()
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 || portNumber == 80 || strconv.Itoa(portNumber) != port {
		return nil, optionErrorf("origin requires an explicit canonical non-default HTTP port between 1 and 65535")
	}
	p.origin, p.host, p.port = opts.Origin, u.Host, port
	if !workspacePattern.MatchString(p.workspace) {
		return nil, optionErrorf("workspace must contain 1-64 ASCII letters, digits, underscores or hyphens and start with a letter or digit")
	}
	if err := update.CheckComposeProject(p.project); err != nil {
		return nil, optionErrorf("project: %v", err)
	}
	for name, ref := range map[string]string{"runtime image": p.runtimeImage, "gateway image": p.gatewayImage} {
		if ref == "" {
			return nil, optionErrorf("%s is required", name)
		}
		if err := update.CheckImageRef(ref); err != nil {
			return nil, optionErrorf("%s: %v", name, err)
		}
	}
	if err := gateway.ValidateDeploymentID(p.deploymentID); err != nil {
		return nil, optionErrorf("deployment ID: %v", err)
	}
	if p.destinationsDir, err = ResolveDestinationsDir(opts.DestinationsDir); err != nil {
		return nil, optionErrorf("%v", err)
	}
	if opts.DestinationName != "" {
		if err := gateway.ValidateDestinationName(opts.DestinationName); err != nil {
			return nil, optionErrorf("destination name: %v", err)
		}
		p.destinationName = opts.DestinationName
	}
	if opts.ManagedBy != nil {
		m := *opts.ManagedBy
		if m.ProjectID == "" || m.ProjectID != p.deploymentID {
			return nil, optionErrorf("a managed deployment requires its deployment ID to equal the managed project ID")
		}
		p.managedBy = &m
	}
	if opts.Bundle != nil {
		fresh, err := OpenBundle(opts.Bundle.Root)
		if err != nil {
			return nil, optionErrorf("release bundle: %v", err)
		}
		if fresh.Version != opts.Bundle.Version {
			return nil, optionErrorf("release bundle %s holds version %s, not %s", fresh.Root, fresh.Version, opts.Bundle.Version)
		}
		p.bundle = fresh
	} else {
		if opts.SourceDir == "" {
			return nil, optionErrorf("a release bundle or a source checkout directory is required")
		}
		if p.sourceDir, err = filepath.Abs(opts.SourceDir); err != nil {
			return nil, err
		}
		compose := filepath.Join(p.sourceDir, "docker", "compose.yaml")
		if fi, err := os.Stat(compose); err != nil || !fi.Mode().IsRegular() {
			return nil, optionErrorf("no release bundle was given and %s does not exist: initialize from a source checkout or from an installed release", compose)
		}
	}
	p.fingerprint = p.computeFingerprint()
	return p, nil
}

// computeFingerprint identifies the inputs an interrupted initialization was
// started with, so a retry with other inputs is refused instead of silently
// completing a deployment the caller did not ask for.
func (p *initPlan) computeFingerprint() string {
	in := struct {
		Root, Origin, Workspace, Mode, Project string
		RuntimeImage, GatewayImage             string
		SourceDir, DeploymentID                string
		BundleVersion                          string
		ManagedBy                              *update.ManagedOwner
		RequirePairing                         bool `json:",omitempty"`
		LocalAutoEntry                         bool `json:",omitempty"`
		DestinationsDir, DestinationName       string
	}{p.root, p.origin, p.workspace, p.mode, p.project, p.runtimeImage, p.gatewayImage, p.sourceDir, p.deploymentID, "", p.managedBy, p.requirePairing, p.localAutoEntry, p.destinationsDir, p.destinationName}
	if p.bundle != nil {
		in.BundleVersion = p.bundle.Version
	}
	b, _ := json.Marshal(in)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Initialize creates the product configuration, credentials and Compose
// deployment record of opts and returns the loaded deployment. It never starts
// anything, never overwrites an existing deployment and never rotates a secret
// that exists.
//
// The product directory is built under a staging name with every path inside
// it already naming the final location. Once complete and flushed, the staged
// directory is renamed into place and deployment.json is created last: that
// atomic creation is the commit point. A crash before it leaves either a
// staging directory (recognized by its owner marker and removed on the next
// call, or promoted when it is complete) or a product directory with a pending
// record, which the next call with the same options completes with the
// credentials it already holds. A Runtime operator token of an earlier
// `lensdock admin init` is reused; one that is missing is created.
func Initialize(opts InitOptions) (*update.Deployment, error) {
	p, err := newPlan(opts)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(p.root, 0o700); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	unlock, err := lockFile(filepath.Join(p.root, initLockFile))
	if err != nil {
		return nil, err
	}
	defer unlock()
	return p.run()
}

func (p *initPlan) existsError(path string) error {
	return fmt.Errorf("%s already exists: product initialization never overwrites or upgrades a deployment (operate it with `lensdock update compose`, change images with `lensdock update`)", path)
}

func (p *initPlan) run() (*update.Deployment, error) {
	depFile := filepath.Join(p.root, DeploymentFile)
	_, depErr := os.Lstat(depFile)
	_, dirErr := os.Lstat(p.dir)
	switch {
	case depErr == nil:
		// Committed. A leftover record of this very initialization means
		// the process stopped between the commit and its cleanup.
		if rec, err := p.readMatchingRecord(p.dir); err == nil && rec != nil {
			return p.commit(rec)
		}
		return nil, p.existsError(depFile)
	case !errors.Is(depErr, os.ErrNotExist):
		return nil, depErr
	case dirErr == nil:
		rec, err := p.readMatchingRecord(p.dir)
		if err != nil {
			return nil, err
		}
		if rec == nil {
			return nil, fmt.Errorf("%w (and it is not an interrupted initialization)", p.existsError(p.dir))
		}
		return p.commit(rec)
	case !errors.Is(dirErr, os.ErrNotExist):
		return nil, dirErr
	}
	rec, err := p.recoverStaging()
	if err != nil {
		return nil, err
	}
	if rec == nil {
		if rec, err = p.build(); err != nil {
			return nil, err
		}
	}
	if err := os.Rename(p.staging, p.dir); err != nil {
		return nil, fmt.Errorf("move the staged product configuration into place: %w", err)
	}
	if err := syncDir(p.root); err != nil {
		return nil, err
	}
	return p.commit(rec)
}

// readRecord returns the pending record of dir, or nil when dir holds none.
func readRecord(dir string) (*pendingRecord, error) {
	path := filepath.Join(dir, recordFile)
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() || fi.Size() > maxRecordBytes {
		return nil, fmt.Errorf("%s is not a pending initialization record", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rec pendingRecord
	if err := json.Unmarshal(data, &rec); err != nil || rec.Magic != recordMagic || len(rec.Deployment) == 0 {
		return nil, fmt.Errorf("%s is not a pending initialization record", path)
	}
	return &rec, nil
}

// readMatchingRecord returns the pending record of dir when it belongs to this
// plan, nil when dir has none, and an error when the record belongs to an
// initialization with other inputs.
func (p *initPlan) readMatchingRecord(dir string) (*pendingRecord, error) {
	rec, err := readRecord(dir)
	if err != nil || rec == nil {
		return nil, err
	}
	if rec.Fingerprint != p.fingerprint {
		return nil, fmt.Errorf("%s holds an interrupted initialization that used other options (state directory, origin, workspace, mode, project, images, deployment identity or release); repeat it with the original options to complete it, or remove %s and %s if nothing ever started from it", dir, p.dir, p.staging)
	}
	return rec, nil
}

// recoverStaging handles a staging directory left by an interrupted run. A
// complete one is returned for promotion. An incomplete one never held a
// credential anything used, and is removed when it carries this tool's owner
// marker; any other directory of that name is left alone and refused.
func (p *initPlan) recoverStaging() (*pendingRecord, error) {
	fi, err := os.Lstat(p.staging)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%s exists and is not a LensDock staging directory", p.staging)
	}
	marker, err := os.ReadFile(filepath.Join(p.staging, stagingMarkerFile))
	if err != nil || string(marker) != stagingMarker {
		return nil, fmt.Errorf("%s exists and is not a LensDock staging directory; it was not touched", p.staging)
	}
	rec, err := p.readMatchingRecord(p.staging)
	if err != nil || rec != nil {
		return rec, err
	}
	if err := os.RemoveAll(p.staging); err != nil {
		return nil, fmt.Errorf("remove the incomplete staging directory: %w", err)
	}
	return nil, syncDir(p.root)
}

// EnsureOperatorCredentials makes sure dir holds the Runtime operator token
// and the runtime.env with its digest, and returns the token file content. An
// existing token is reused (a runtime.env that an interruption left missing is
// recreated from it); nothing that exists is replaced.
func EnsureOperatorCredentials(dir string) ([]byte, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	tokenPath := filepath.Join(dir, OperatorTokenFile)
	envPath := filepath.Join(dir, RuntimeEnvFile)
	token, err := auth.ReadPrivateFile(tokenPath, 4096)
	if errors.Is(err, os.ErrNotExist) {
		tok, err := auth.NewSecret("ldo_")
		if err != nil {
			return nil, fmt.Errorf("generate operator token: %w", err)
		}
		token = []byte(tok.Reveal() + "\n")
		if err := publishNew(tokenPath, token, 0o600); err != nil {
			if !errors.Is(err, os.ErrExist) {
				return nil, fmt.Errorf("write operator token: %w", err)
			}
			if token, err = auth.ReadPrivateFile(tokenPath, 4096); err != nil {
				return nil, err
			}
		}
	} else if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(envPath); errors.Is(err, os.ErrNotExist) {
		line := "LENSDOCK_OPERATOR_TOKEN_SHA256=" + auth.DigestOf(strings.TrimSpace(string(token))).Hex() + "\n"
		if err := publishNew(envPath, []byte(line), 0o600); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("write runtime.env: %w", err)
		}
	} else if err != nil {
		return nil, err
	}
	return token, nil
}

// deployment returns the deployment record. Every path names the final
// product directory, never the staging directory.
func (p *initPlan) deployment() update.Deployment {
	dir := p.dir
	composeFiles := []string{filepath.Join(p.sourceDir, "docker", "compose.yaml")}
	linuxCLI := map[string]string{"arm64": filepath.Join(p.sourceDir, "bin", "linux-arm64", "lensdock"), "amd64": filepath.Join(p.sourceDir, "bin", "linux-amd64", "lensdock")}
	if p.bundle != nil {
		rel := filepath.Join(dir, ReleaseDir)
		composeFiles = []string{filepath.Join(rel, "docker", "compose.yaml"), filepath.Join(dir, PinFile)}
		linuxCLI = map[string]string{"arm64": filepath.Join(rel, "bin", "linux-arm64", "lensdock"), "amd64": filepath.Join(rel, "bin", "linux-amd64", "lensdock")}
	}
	return update.Deployment{
		Version: 1, ManagedBy: p.managedBy, StateDir: dir, PublicStatusFile: filepath.Join(dir, "public", "deployment-status.json"),
		Compose:  update.ComposeConfig{Project: p.project, Files: composeFiles, EnvFiles: []string{filepath.Join(dir, "compose.env")}},
		LinuxCLI: linuxCLI,
		Workspaces: map[string]update.WorkspaceConfig{p.workspace: {
			RuntimeService: "runtime", OperatorTokenFile: filepath.Join(dir, OperatorTokenFile),
			Probe: update.ProbeConfig{URL: "https://127.0.0.1:8080", CAFile: filepath.Join(dir, "ca.crt"), CertFile: filepath.Join(dir, "gateway.crt"), KeyFile: filepath.Join(dir, "gateway.key")},
			Datasets: []update.Dataset{
				{Key: "lensdock-profile", Kind: "profile", Role: "snapshot", MountPath: "/data/profile"},
				{Key: "lensdock-downloads", Kind: "downloads", Role: "retained", MountPath: "/data/downloads"},
				{Key: "lensdock-runtime-policy", Kind: "other", Role: "retained", CandidateAccess: "scratch", MountPath: "/data/runtime"},
				{Key: "runtime-credentials", Kind: "other", Role: "retained", MountPath: "/run/lensdock-credentials"},
			},
		}},
		Gateway: &update.GatewayConfig{Service: "gateway", MigrateArgv: []string{"migrate", "--data-dir", "/data"}, ReadyURL: p.origin + "/readyz", Datasets: []update.Dataset{{Key: "gateway-db", Kind: "db", Role: "snapshot", MountPath: "/data/db"}, {Key: "gateway-private", Kind: "private", Role: "retained", MountPath: "/data/private"}, {Key: "gateway-artifacts", Kind: "artifacts", Role: "retained", MountPath: "/data/artifacts"}, {Key: "gateway-credentials", Kind: "other", Role: "retained", MountPath: "/run/lensdock"}}},
	}
}

// build writes a complete product directory into the staging directory and
// returns its pending record. The staging directory is removed again on
// failure; once the record is durable it is kept for the next call.
func (p *initPlan) build() (rec *pendingRecord, err error) {
	op, err := EnsureOperatorCredentials(p.root)
	if err != nil {
		return nil, err
	}
	sd := p.staging
	if err := os.Mkdir(sd, 0o700); err != nil {
		return nil, fmt.Errorf("create product staging directory: %w", err)
	}
	durable := false
	defer func() {
		if !durable {
			_ = os.RemoveAll(sd)
		}
	}()
	if err := writeNew(filepath.Join(sd, stagingMarkerFile), []byte(stagingMarker), 0o600); err != nil {
		return nil, err
	}
	if err := writeNew(filepath.Join(sd, OperatorTokenFile), op, 0o600); err != nil {
		return nil, err
	}
	var identities []gateway.Identity
	for _, role := range []string{"admin", "human", "viewer", "agent"} {
		tok, err := auth.NewSecret("ld_" + role + "_")
		if err != nil {
			return nil, err
		}
		if err := writeNew(filepath.Join(sd, role+"-token"), []byte(tok.Reveal()+"\n"), 0o600); err != nil {
			return nil, err
		}
		identities = append(identities, gateway.Identity{ID: role, Role: role, WorkspaceID: p.workspace, TokenSHA256: auth.DigestOf(tok.Reveal()).Hex()})
	}
	if err := writeRuntimePKI(sd); err != nil {
		return nil, err
	}
	if err := os.Mkdir(filepath.Join(sd, "public"), 0o755); err != nil {
		return nil, err
	}
	if err := os.Chmod(filepath.Join(sd, "public"), 0o755); err != nil {
		return nil, err
	}
	if err := writeNew(filepath.Join(sd, "public", "deployment-status.json"), []byte("{\"release\":{\"id\":\"development\",\"verified\":false},\"update\":{\"phase\":\"not-configured\",\"lastBackupAt\":null}}\n"), 0o644); err != nil {
		return nil, err
	}
	cfg := gateway.Config{Listen: ":8080", Origin: p.origin, DeploymentID: p.deploymentID, DataDir: "/data", WebDir: "/app/web", Identities: identities, Runtimes: []gateway.RuntimeConfig{{WorkspaceID: p.workspace, URL: "https://runtime:8080", OperatorTokenFile: "/run/lensdock/operator-token", CAFile: "/run/lensdock/ca.crt", ClientCertFile: "/run/lensdock/gateway.crt", ClientKeyFile: "/run/lensdock/gateway.key"}}}
	cfg.RequirePairing = p.requirePairing
	cfg.LocalAutoEntry = p.localAutoEntry
	cfg.DestinationName = p.destinationName
	cfg.StatusFile = "/run/lensdock-status/deployment-status.json"
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := writeNew(filepath.Join(sd, "gateway.json"), append(b, '\n'), 0o600); err != nil {
		return nil, err
	}
	// The host-run Gateway of `mise run gateway:dev` exists only in a source checkout.
	if p.bundle == nil {
		cfg.Listen = p.host
		cfg.DataDir = filepath.Join(p.root, "gateway-data")
		cfg.WebDir = filepath.Join(p.sourceDir, "apps/web/dist")
		cfg.StatusFile = filepath.Join(p.dir, "public", "deployment-status.json")
		cfg.DestinationsDir = p.destinationsDir
		cfg.Runtimes = []gateway.RuntimeConfig{{WorkspaceID: p.workspace, URL: "http://127.0.0.1:18080", OperatorTokenFile: filepath.Join(p.root, OperatorTokenFile)}}
		if b, err = json.MarshalIndent(cfg, "", "  "); err != nil {
			return nil, err
		}
		if err := writeNew(filepath.Join(sd, "gateway-host.json"), append(b, '\n'), 0o600); err != nil {
			return nil, err
		}
	}
	env := fmt.Sprintf("LENSDOCK_WORKSPACE_ID=%s\nLENSDOCK_MODE=%s\nLENSDOCK_STATE_DIR=%s\nLENSDOCK_HOST_PORT=%s\nLENSDOCK_PUBLIC_ORIGIN=%s\n%s=%s\n%s=%s\n%s=%s\n", p.workspace, p.mode, composeEnvValue(p.root), p.port, composeEnvValue(p.origin), EnvRuntimeImage, composeEnvValue(p.runtimeImage), EnvGatewayImage, composeEnvValue(p.gatewayImage), EnvDestinationsDir, composeEnvValue(p.destinationsDir))
	if err := writeNew(filepath.Join(sd, "compose.env"), []byte(env), 0o600); err != nil {
		return nil, err
	}
	if p.bundle != nil {
		// A private copy keeps the deployment independent of the bundle's
		// location, so reinstalling or upgrading the CLI cannot break it.
		if err := p.bundle.Snapshot(filepath.Join(sd, ReleaseDir)); err != nil {
			return nil, err
		}
		if err := writeNew(filepath.Join(sd, PinFile), []byte(PinPlaceholder), 0o600); err != nil {
			return nil, err
		}
	}
	dep := p.deployment()
	raw, err := json.MarshalIndent(dep, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := p.check(dep); err != nil {
		return nil, err
	}
	if err := syncTree(sd); err != nil {
		return nil, err
	}
	rec = &pendingRecord{Magic: recordMagic, Fingerprint: p.fingerprint, Deployment: raw}
	data, err := json.Marshal(rec)
	if err != nil {
		return nil, err
	}
	if err := writeFileAtomic(filepath.Join(sd, recordFile), append(data, '\n'), 0o600); err != nil {
		return nil, err
	}
	durable = true
	return rec, nil
}

// check loads dep through the same validation LoadDeployment applies to the
// committed file, so an invalid deployment is refused before anything is
// promoted. The env file only exists in the staging directory yet.
func (p *initPlan) check(dep update.Deployment) error {
	dep.Compose.EnvFiles = []string{filepath.Join(p.staging, "compose.env")}
	raw, err := json.Marshal(dep)
	if err != nil {
		return err
	}
	path := filepath.Join(p.staging, ".deployment-check.json")
	if err := writeNew(path, raw, 0o600); err != nil {
		return err
	}
	defer func() { _ = os.Remove(path) }()
	if _, err := update.LoadDeployment(path); err != nil {
		return fmt.Errorf("the initialized deployment is invalid: %w", err)
	}
	return nil
}

// commit creates deployment.json from rec: the durable commit of the
// initialization. It then removes the staging markers of the product
// directory, which are only needed until the commit.
func (p *initPlan) commit(rec *pendingRecord) (*update.Deployment, error) {
	var probe struct {
		StateDir string `json:"stateDir"`
	}
	if err := json.Unmarshal(rec.Deployment, &probe); err != nil || probe.StateDir != p.dir {
		return nil, fmt.Errorf("the pending initialization record does not describe %s", p.dir)
	}
	depFile := filepath.Join(p.root, DeploymentFile)
	var body bytes.Buffer
	if err := json.Indent(&body, rec.Deployment, "", "  "); err != nil {
		return nil, fmt.Errorf("the pending initialization record holds an invalid deployment: %w", err)
	}
	body.WriteByte('\n')
	if err := publishNew(depFile, body.Bytes(), 0o600); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("commit the deployment: %w", err)
	}
	for _, name := range []string{recordFile, stagingMarkerFile} {
		_ = os.Remove(filepath.Join(p.dir, name))
	}
	_ = syncDir(p.dir)
	return update.LoadDeployment(depFile)
}
