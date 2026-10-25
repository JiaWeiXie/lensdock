// Package update implements the host-side release verification, update
// planning, clean checkpointing, candidate verification, paired rollback and
// release tooling of LensDock (see docs/UPDATE-RUNBOOK.md).
//
// Everything here runs on the host with Docker access. No part of it is
// reachable from the Gateway or the browser runtime, neither of which mounts a
// Docker socket. External programs are always started with an argv, never
// through a shell.
package update

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// DeploymentVersion is the only supported deployment.json schema version.
const DeploymentVersion = 1

// Default values applied by LoadDeployment.
const (
	DefaultRetainCheckpoints = 3
	DefaultMinFreeBytes      = 512 << 20
	DefaultStopGraceSeconds  = 40
	DefaultDrainSeconds      = 45
	DefaultHealthSeconds     = 180
	DefaultProbeURL          = "http://127.0.0.1:8080"
)

// Dataset roles.
const (
	// RoleSnapshot datasets are checkpointed before an update, cloned into
	// candidate volumes and restored as a pair on rollback.
	RoleSnapshot = "snapshot"
	// RoleRetained datasets (downloads, feedback artifacts, private drafts)
	// are never copied or rolled back; the new generation reattaches them.
	RoleRetained = "retained"
)

// Dataset kinds. The kind selects clean-state checks and status labels.
const (
	KindProfile   = "profile"
	KindKeyring   = "keyring"
	KindDownloads = "downloads"
	KindDB        = "db"
	KindArtifacts = "artifacts"
	KindPrivate   = "private"
	KindOther     = "other"
)

// Candidate access modes of retained datasets.
const (
	CandidateScratch  = "scratch"
	CandidateReadOnly = "readonly"
)

// defaultAccess fills CandidateAccess of retained datasets in place.
func defaultAccess(sets []Dataset) {
	for i := range sets {
		if sets[i].Role != RoleRetained || sets[i].CandidateAccess != "" {
			continue
		}
		switch sets[i].Kind {
		case KindArtifacts, KindOther:
			sets[i].CandidateAccess = CandidateReadOnly
		default:
			sets[i].CandidateAccess = CandidateScratch
		}
	}
}

var (
	namePattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)
	projectPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	// workspaceIDPattern mirrors internal/supervisor.workspaceIDPattern.
	workspaceIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

// Deployment is the host-side description of one LensDock deployment: where
// state, backups and releases live, which compose project runs it, and which
// named volumes hold which data. It is the contract between the product
// compose files and the admin CLI (deployment.json).
//
// Relative paths are resolved against the directory of deployment.json.
// Unknown JSON fields are rejected.
type Deployment struct {
	Version int `json:"version"`
	// ManagedBy routes lifecycle mutations through the owning host manager.
	ManagedBy *ManagedOwner `json:"managedBy,omitempty"`
	// StateDir holds state/, updates/ and generations/. Defaults to the
	// directory of deployment.json.
	StateDir string `json:"stateDir,omitempty"`
	// BackupDir holds checkpoints (encrypted). Default <stateDir>/backups.
	BackupDir string `json:"backupDir,omitempty"`
	// ReleasesDir holds <release-id>/manifest.json directories. Default
	// <stateDir>/releases.
	ReleasesDir string `json:"releasesDir,omitempty"`
	// BackupKeyFile is a 0600 file with 64 hex characters (AES-256). It must
	// not live inside BackupDir. Default <stateDir>/keys/backup.key.
	BackupKeyFile string `json:"backupKeyFile,omitempty"`
	// PublicStatusFile receives the sanitized status JSON the Gateway serves
	// read-only (docs: local updates-wire). Empty disables publishing.
	PublicStatusFile string `json:"publicStatusFile,omitempty"`

	RetainCheckpoints    int   `json:"retainCheckpoints,omitempty"`
	MinFreeBytes         int64 `json:"minFreeBytes,omitempty"`
	StopGraceSeconds     int   `json:"stopGraceSeconds,omitempty"`
	DrainTimeoutSeconds  int   `json:"drainTimeoutSeconds,omitempty"`
	HealthTimeoutSeconds int   `json:"healthTimeoutSeconds,omitempty"`

	Compose ComposeConfig `json:"compose"`
	// LinuxCLI maps a Docker engine architecture (amd64, arm64) to the Linux
	// lensdock binary run inside candidate/probe sidecars. Its SHA-256 must
	// match the release manifest. When empty and the host is Linux, the
	// running executable is used.
	LinuxCLI map[string]string `json:"linuxCli,omitempty"`
	// TrustedKeys are the Ed25519 release signing keys accepted for the
	// "release" channel.
	TrustedKeys []TrustedKey `json:"trustedKeys,omitempty"`

	Workspaces map[string]WorkspaceConfig `json:"workspaces"`
	Gateway    *GatewayConfig             `json:"gateway,omitempty"`

	// File is the absolute path deployment.json was loaded from.
	File string `json:"-"`
}

// ComposeConfig names the compose project the CLI drives.
type ComposeConfig struct {
	Project string `json:"project"`
	// Files are the base compose files, in order. They are never modified;
	// the CLI layers a generated per-generation override on top.
	Files []string `json:"files"`
	// ProjectDir is passed as --project-directory. Default: directory of the
	// first file.
	ProjectDir string `json:"projectDir,omitempty"`
	// EnvFiles are passed as --env-file to every compose invocation (config,
	// up, stop, ... and `lensdock update compose`), so interpolation inputs
	// such as the state directory, host port and public origin do not depend
	// on the caller's environment.
	EnvFiles []string `json:"envFiles,omitempty"`
}

// Dataset is one named volume of a service.
type Dataset struct {
	// Key is the top-level volume key in the compose file.
	Key  string `json:"key"`
	Kind string `json:"kind"`
	Role string `json:"role"`
	// MountPath is where the service mounts it. Required for the Gateway
	// database when MigrateArgv is used.
	MountPath string `json:"mountPath,omitempty"`
	// CandidateAccess says how verification containers see a retained
	// dataset: "scratch" (a new empty volume) or "readonly" (the real volume,
	// mounted read-only; every compose mount of it must be read_only).
	// Defaults: scratch for downloads and private, readonly for artifacts and
	// other. Snapshot datasets are always cloned and must leave it empty.
	CandidateAccess string `json:"candidateAccess,omitempty"`
}

// WorkspaceConfig describes one browser runtime.
type WorkspaceConfig struct {
	RuntimeService string `json:"runtimeService"`
	// APIURL is the runtime API as seen from the host (the M0 compose
	// publishes 127.0.0.1:18080). Empty means the runtime is private; probes
	// then run through a sidecar in the runtime container's network
	// namespace.
	APIURL string `json:"apiUrl,omitempty"`
	// OperatorTokenFile enables lease inspection and the runtime
	// MAINTENANCE fence. Empty skips both; stopping the runtime still
	// revokes every lease.
	OperatorTokenFile string      `json:"operatorTokenFile,omitempty"`
	Probe             ProbeConfig `json:"probe,omitempty"`
	Datasets          []Dataset   `json:"datasets"`
}

// ProbeConfig says how a sidecar reaches the runtime API from inside the
// runtime container's network namespace. The TLS files are read on the host
// and handed to the sidecar on stdin; they are never mounted or put in argv.
type ProbeConfig struct {
	// URL defaults to DefaultProbeURL.
	URL        string `json:"url,omitempty"`
	CAFile     string `json:"caFile,omitempty"`
	CertFile   string `json:"certFile,omitempty"`
	KeyFile    string `json:"keyFile,omitempty"`
	ServerName string `json:"serverName,omitempty"`
}

// GatewayConfig describes the Gateway service and its volumes.
type GatewayConfig struct {
	Service  string    `json:"service"`
	Datasets []Dataset `json:"datasets"`
	// MigrateArgv are the arguments passed after the candidate Gateway image
	// name (the image entrypoint is kept), e.g. ["migrate","--data-dir","/data"].
	// The container mounts every gateway dataset at its mountPath: the
	// database cloned from the checkpoint, retained datasets per
	// candidateAccess; it runs with network none, a read-only root, no
	// capabilities and the uid:gid of User. Empty means no offline migration.
	MigrateArgv []string `json:"migrateArgv,omitempty"`
	// User is "uid:gid" for the migration container. Default "10000:10000".
	User string `json:"user,omitempty"`
	// ReadyURL is polled from the host after the switch. Empty skips it.
	ReadyURL string `json:"readyUrl,omitempty"`
}

// defaultUser is the uid:gid the Gateway image runs as.
const defaultUser = "10000:10000"

// LoadDeployment reads, defaults and validates deployment.json.
func LoadDeployment(file string) (*Deployment, error) {
	abs, err := filepath.Abs(file)
	if err != nil {
		return nil, fmt.Errorf("resolve deployment path: %w", err)
	}
	data, err := readFileLimit(abs, 1<<20)
	if err != nil {
		return nil, fmt.Errorf("read deployment: %w", err)
	}
	var d Deployment
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("decode deployment: %w", err)
	}
	if err := ensureJSONEOF(dec); err != nil {
		return nil, fmt.Errorf("decode deployment: %w", err)
	}
	d.File = abs
	d.applyDefaults()
	if err := d.Validate(); err != nil {
		return nil, err
	}
	return &d, nil
}

// applyDefaults resolves relative paths against the directory of File and
// fills defaults. It is idempotent.
func (d *Deployment) applyDefaults() {
	base := filepath.Dir(d.File)
	rel := func(p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(base, p)
	}
	if d.StateDir == "" {
		d.StateDir = base
	}
	d.StateDir = rel(d.StateDir)
	if d.BackupDir == "" {
		d.BackupDir = filepath.Join(d.StateDir, "backups")
	}
	if d.ReleasesDir == "" {
		d.ReleasesDir = filepath.Join(d.StateDir, "releases")
	}
	if d.BackupKeyFile == "" {
		d.BackupKeyFile = filepath.Join(d.StateDir, "keys", "backup.key")
	}
	d.BackupDir = rel(d.BackupDir)
	d.ReleasesDir = rel(d.ReleasesDir)
	d.BackupKeyFile = rel(d.BackupKeyFile)
	d.PublicStatusFile = rel(d.PublicStatusFile)
	if d.RetainCheckpoints == 0 {
		d.RetainCheckpoints = DefaultRetainCheckpoints
	}
	if d.MinFreeBytes == 0 {
		d.MinFreeBytes = DefaultMinFreeBytes
	}
	if d.StopGraceSeconds == 0 {
		d.StopGraceSeconds = DefaultStopGraceSeconds
	}
	if d.DrainTimeoutSeconds == 0 {
		d.DrainTimeoutSeconds = DefaultDrainSeconds
	}
	if d.HealthTimeoutSeconds == 0 {
		d.HealthTimeoutSeconds = DefaultHealthSeconds
	}
	for i, f := range d.Compose.Files {
		d.Compose.Files[i] = rel(f)
	}
	for i, f := range d.Compose.EnvFiles {
		d.Compose.EnvFiles[i] = rel(f)
	}
	d.Compose.ProjectDir = rel(d.Compose.ProjectDir)
	if d.Compose.ProjectDir == "" && len(d.Compose.Files) > 0 {
		d.Compose.ProjectDir = filepath.Dir(d.Compose.Files[0])
	}
	for arch, p := range d.LinuxCLI {
		d.LinuxCLI[arch] = rel(p)
	}
	for id, ws := range d.Workspaces {
		defaultAccess(ws.Datasets)
		ws.OperatorTokenFile = rel(ws.OperatorTokenFile)
		ws.Probe.CAFile = rel(ws.Probe.CAFile)
		ws.Probe.CertFile = rel(ws.Probe.CertFile)
		ws.Probe.KeyFile = rel(ws.Probe.KeyFile)
		if ws.Probe.URL == "" {
			ws.Probe.URL = DefaultProbeURL
		}
		d.Workspaces[id] = ws
	}
	if d.Gateway != nil && d.Gateway.User == "" {
		d.Gateway.User = defaultUser
	}
	if d.Gateway != nil {
		defaultAccess(d.Gateway.Datasets)
	}
}

var (
	userPattern    = regexp.MustCompile(`^[0-9]{1,10}:[0-9]{1,10}$`)
	keyIDPattern   = regexp.MustCompile(`^[0-9a-f]{16}$`)
	validKinds     = []string{KindProfile, KindKeyring, KindDownloads, KindDB, KindArtifacts, KindPrivate, KindOther}
	validRoles     = []string{RoleSnapshot, RoleRetained}
	validArchNames = []string{"amd64", "arm64"}
)

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// Validate checks the invariants the update logic relies on. Every problem
// is reported.
func (d *Deployment) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if d.Version != DeploymentVersion {
		add("version must be %d", DeploymentVersion)
	}
	if d.ManagedBy != nil {
		if err := d.ManagedBy.validate(); err != nil {
			add("%v", err)
		}
	}
	for name, p := range map[string]string{
		"stateDir": d.StateDir, "backupDir": d.BackupDir, "releasesDir": d.ReleasesDir, "backupKeyFile": d.BackupKeyFile,
	} {
		if !filepath.IsAbs(p) {
			add("%s must be an absolute path after resolution", name)
		}
	}
	if filepath.IsAbs(d.BackupKeyFile) && filepath.IsAbs(d.BackupDir) && pathWithin(d.BackupDir, d.BackupKeyFile) {
		add("backupKeyFile must not be inside backupDir: the key must never be stored with the backups")
	}
	if filepath.IsAbs(d.BackupDir) && filepath.IsAbs(d.ReleasesDir) {
		if pathWithin(d.ReleasesDir, d.BackupDir) || pathWithin(d.BackupDir, d.ReleasesDir) {
			add("backupDir and releasesDir must not contain each other")
		}
	}
	if d.PublicStatusFile != "" {
		if !filepath.IsAbs(d.PublicStatusFile) {
			add("publicStatusFile must be an absolute path after resolution")
		} else if pathWithin(d.BackupDir, d.PublicStatusFile) || pathWithin(filepath.Dir(d.BackupKeyFile), d.PublicStatusFile) {
			add("publicStatusFile must not be inside backupDir or the key directory")
		}
	}
	if d.RetainCheckpoints < 1 {
		add("retainCheckpoints must be at least 1")
	}
	if d.MinFreeBytes < 0 {
		add("minFreeBytes must not be negative")
	}
	for name, v := range map[string]int{
		"stopGraceSeconds": d.StopGraceSeconds, "drainTimeoutSeconds": d.DrainTimeoutSeconds, "healthTimeoutSeconds": d.HealthTimeoutSeconds,
	} {
		if v < 1 || v > 3600 {
			add("%s must be between 1 and 3600", name)
		}
	}

	if !projectPattern.MatchString(d.Compose.Project) {
		add("compose.project must match %s", projectPattern)
	}
	if len(d.Compose.Files) == 0 {
		add("compose.files must list at least one compose file")
	}
	for _, f := range d.Compose.Files {
		if !filepath.IsAbs(f) {
			add("compose file %q must be absolute after resolution", f)
		}
	}
	for _, f := range d.Compose.EnvFiles {
		if !filepath.IsAbs(f) {
			add("compose env file %q must be absolute after resolution", f)
		} else if fi, err := os.Stat(f); err != nil || !fi.Mode().IsRegular() {
			add("compose env file %q must be an existing regular file", f)
		}
	}
	for arch, p := range d.LinuxCLI {
		if !contains(validArchNames, arch) {
			add("linuxCli: unsupported architecture %q", arch)
		}
		if !filepath.IsAbs(p) {
			add("linuxCli[%s] must be absolute after resolution", arch)
		}
	}
	seenKeyIDs := map[string]bool{}
	for i, k := range d.TrustedKeys {
		if err := k.validate(); err != nil {
			add("trustedKeys[%d]: %v", i, err)
		} else if seenKeyIDs[k.KeyID] {
			add("trustedKeys[%d]: duplicate keyId %s", i, k.KeyID)
		}
		seenKeyIDs[k.KeyID] = true
	}

	if len(d.Workspaces) == 0 {
		add("workspaces must define at least one workspace")
	}
	allKeys := map[string]string{}
	services := map[string]string{}
	checkDatasets := func(owner string, sets []Dataset) {
		for i, ds := range sets {
			where := fmt.Sprintf("%s.datasets[%d]", owner, i)
			if !namePattern.MatchString(ds.Key) {
				add("%s.key %q must match %s", where, ds.Key, namePattern)
			}
			if !contains(validKinds, ds.Kind) {
				add("%s.kind %q must be one of %s", where, ds.Kind, strings.Join(validKinds, ", "))
			}
			if !contains(validRoles, ds.Role) {
				add("%s.role %q must be %s or %s", where, ds.Role, RoleSnapshot, RoleRetained)
			}
			if ds.MountPath != "" && (!path.IsAbs(ds.MountPath) || path.Clean(ds.MountPath) != ds.MountPath) {
				add("%s.mountPath must be a clean absolute container path", where)
			}
			switch {
			case ds.Role == RoleSnapshot && ds.CandidateAccess != "":
				add("%s.candidateAccess only applies to retained datasets; snapshot datasets are always cloned", where)
			case ds.Role == RoleRetained && ds.CandidateAccess != CandidateScratch && ds.CandidateAccess != CandidateReadOnly:
				add("%s.candidateAccess must be %q or %q", where, CandidateScratch, CandidateReadOnly)
			}
			if prev, dup := allKeys[ds.Key]; dup {
				add("%s.key %q is already used by %s: volume keys are global in the compose project", where, ds.Key, prev)
			}
			allKeys[ds.Key] = owner
		}
	}
	ids := make([]string, 0, len(d.Workspaces))
	for id := range d.Workspaces {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		ws := d.Workspaces[id]
		owner := "workspaces." + id
		if !workspaceIDPattern.MatchString(id) {
			add("workspace id %q must match %s", id, workspaceIDPattern)
		}
		if !namePattern.MatchString(ws.RuntimeService) {
			add("%s.runtimeService %q must match %s", owner, ws.RuntimeService, namePattern)
		} else if prev, dup := services[ws.RuntimeService]; dup {
			add("%s.runtimeService %q is already used by %s", owner, ws.RuntimeService, prev)
		}
		services[ws.RuntimeService] = owner
		if ws.APIURL != "" {
			if u, err := url.Parse(ws.APIURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				add("%s.apiUrl must be an http(s) URL", owner)
			}
		}
		if ws.OperatorTokenFile != "" && !filepath.IsAbs(ws.OperatorTokenFile) {
			add("%s.operatorTokenFile must be absolute after resolution", owner)
		}
		if u, err := url.Parse(ws.Probe.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Port() == "" {
			add("%s.probe.url must be an http(s) URL with an explicit port", owner)
		}
		tlsFiles := 0
		for _, f := range []string{ws.Probe.CAFile, ws.Probe.CertFile, ws.Probe.KeyFile} {
			if f != "" {
				tlsFiles++
			}
		}
		if tlsFiles != 0 && tlsFiles != 3 {
			add("%s.probe: caFile, certFile and keyFile must be set together", owner)
		}
		checkDatasets(owner, ws.Datasets)
		profile := 0
		for _, ds := range ws.Datasets {
			if ds.Kind == KindProfile {
				profile++
				if ds.Role != RoleSnapshot {
					add("%s: the profile dataset %q must have role %s", owner, ds.Key, RoleSnapshot)
				}
			}
			if ds.Kind == KindKeyring && ds.Role != RoleSnapshot {
				add("%s: the keyring dataset %q must have role %s (it is backed up and rolled back with the profile)", owner, ds.Key, RoleSnapshot)
			}
		}
		if profile != 1 {
			add("%s must declare exactly one dataset of kind %s", owner, KindProfile)
		}
	}
	if g := d.Gateway; g != nil {
		if !namePattern.MatchString(g.Service) {
			add("gateway.service %q must match %s", g.Service, namePattern)
		} else if prev, dup := services[g.Service]; dup {
			add("gateway.service %q is already used by %s", g.Service, prev)
		}
		checkDatasets("gateway", g.Datasets)
		dbs := 0
		for _, ds := range g.Datasets {
			if ds.Kind == KindDB {
				dbs++
				if ds.Role != RoleSnapshot {
					add("gateway: the db dataset %q must have role %s", ds.Key, RoleSnapshot)
				}
			}
			if len(g.MigrateArgv) > 0 && ds.MountPath == "" {
				add("gateway: dataset %q needs mountPath: the migration container mounts every gateway dataset", ds.Key)
			}
			if ds.Kind == KindProfile || ds.Kind == KindKeyring {
				add("gateway: dataset %q: the gateway must never mount a profile or keyring", ds.Key)
			}
		}
		if dbs != 1 {
			add("gateway must declare exactly one dataset of kind %s", KindDB)
		}
		for _, a := range g.MigrateArgv {
			if a == "" || strings.ContainsRune(a, 0) {
				add("gateway.migrateArgv must not contain empty arguments")
				break
			}
		}
		if !userPattern.MatchString(g.User) {
			add("gateway.user must look like uid:gid")
		}
		if g.ReadyURL != "" {
			if u, err := url.Parse(g.ReadyURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				add("gateway.readyUrl must be an http(s) URL")
			}
		}
	}
	return errors.Join(errs...)
}

// WorkspaceIDs returns the configured workspace ids in sorted order.
func (d *Deployment) WorkspaceIDs() []string {
	ids := make([]string, 0, len(d.Workspaces))
	for id := range d.Workspaces {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Workspace returns the configuration of id.
func (d *Deployment) Workspace(id string) (WorkspaceConfig, error) {
	ws, ok := d.Workspaces[id]
	if !ok {
		return WorkspaceConfig{}, fmt.Errorf("workspace %q is not defined in %s (defined: %s)", id, filepath.Base(d.File), strings.Join(d.WorkspaceIDs(), ", "))
	}
	return ws, nil
}

// Save writes the deployment as deployment.json at File with mode 0600
// (it may name credential files, never their content).
func (d *Deployment) Save() error {
	if d.File == "" {
		return errors.New("deployment has no file path")
	}
	return writeJSONAtomic(d.File, d, 0o600)
}

// CandidateProbeURL is the probe URL with the scheme forced to http: candidate
// runtimes run without the TLS environment so the verifier needs no
// certificates.
func (p ProbeConfig) CandidateProbeURL() string {
	u, err := url.Parse(p.URL)
	if err != nil || u.Host == "" {
		return DefaultProbeURL
	}
	return "http://" + u.Host
}

// TrustedKey is an accepted Ed25519 release signing key.
type TrustedKey struct {
	KeyID string `json:"keyId"`
	// PublicKey is the raw 32-byte Ed25519 public key, standard base64.
	PublicKey string `json:"publicKey"`
}

func (k TrustedKey) validate() error {
	if !keyIDPattern.MatchString(k.KeyID) {
		return errors.New("keyId must be 16 lowercase hex characters")
	}
	raw, err := base64.StdEncoding.DecodeString(k.PublicKey)
	if err != nil || len(raw) != 32 {
		return errors.New("publicKey must be a base64 encoded 32-byte Ed25519 key")
	}
	if got := keyIDOfPublic(raw); got != k.KeyID {
		return fmt.Errorf("keyId does not match the public key (expected %s)", got)
	}
	return nil
}

// ScaffoldM0 returns the deployment for the M0 compose file in the repository
// (runtime service "runtime", profile, downloads and runtime policy volumes,
// API published on 127.0.0.1:18080). All paths are absolute.
func ScaffoldM0(file, stateDir, composeFile, linuxCLIDir string) *Deployment {
	d := &Deployment{
		Version:  DeploymentVersion,
		File:     file,
		StateDir: stateDir,
		Compose: ComposeConfig{
			Project: "lensdock",
			Files:   []string{composeFile},
		},
		LinuxCLI: map[string]string{
			"amd64": filepath.Join(linuxCLIDir, "linux-amd64", "lensdock"),
			"arm64": filepath.Join(linuxCLIDir, "linux-arm64", "lensdock"),
		},
		Workspaces: map[string]WorkspaceConfig{
			"default": {
				RuntimeService:    "runtime",
				APIURL:            "http://127.0.0.1:18080",
				OperatorTokenFile: filepath.Join(stateDir, "operator-token"),
				Probe:             ProbeConfig{URL: DefaultProbeURL},
				Datasets: []Dataset{
					{Key: "lensdock-profile", Kind: KindProfile, Role: RoleSnapshot},
					{Key: "lensdock-downloads", Kind: KindDownloads, Role: RoleRetained},
					{Key: "lensdock-runtime-policy", Kind: KindOther, Role: RoleRetained, CandidateAccess: CandidateScratch, MountPath: "/data/runtime"},
				},
			},
		},
	}
	d.applyDefaults()
	return d
}

// ensureDirs creates the directories the CLI owns with mode 0700.
func (d *Deployment) ensureDirs() error {
	for _, dir := range []string{d.StateDir, d.BackupDir, d.ReleasesDir, filepath.Dir(d.BackupKeyFile), d.stateSubdir(), d.updatesDir(), d.generationsDir()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", filepath.Base(dir), err)
		}
	}
	return nil
}

func (d *Deployment) stateSubdir() string    { return filepath.Join(d.StateDir, "state") }
func (d *Deployment) updatesDir() string     { return filepath.Join(d.StateDir, "updates") }
func (d *Deployment) generationsDir() string { return filepath.Join(d.StateDir, "generations") }
func (d *Deployment) plansDir() string       { return filepath.Join(d.StateDir, "plans") }
func (d *Deployment) lockPath() string       { return filepath.Join(d.stateSubdir(), "lock") }
func (d *Deployment) stateFile() string      { return filepath.Join(d.stateSubdir(), "state.json") }
func (d *Deployment) generationDir(n int) string {
	return filepath.Join(d.generationsDir(), fmt.Sprintf("%d", n))
}

// allDatasets returns every dataset of the deployment with its owner id
// ("runtime/<workspace>" or "gateway").
func (d *Deployment) allDatasets() map[string][]Dataset {
	out := map[string][]Dataset{}
	for id, ws := range d.Workspaces {
		out["runtime/"+id] = ws.Datasets
	}
	if d.Gateway != nil {
		out["gateway"] = d.Gateway.Datasets
	}
	return out
}
