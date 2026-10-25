package update

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Cmd describes one external command. It is always executed from an argv; no
// shell is involved and no argument is ever interpreted.
type Cmd struct {
	Name   string
	Args   []string
	Dir    string
	Env    []string // appended to the process environment
	Stdin  io.Reader
	Stdout io.Writer
	// Stderr receives the command's standard error. When nil the last bytes
	// are captured into ExitError.
	Stderr io.Writer
}

// Runner starts external commands. Tests replace it.
type Runner interface {
	Run(ctx context.Context, c Cmd) error
}

// ExitError is a command that ran and failed.
type ExitError struct {
	Argv   []string
	Code   int
	Stderr string
}

func (e *ExitError) Error() string {
	argv := strings.Join(e.Argv, " ")
	if len(argv) > 200 {
		argv = argv[:200] + "..."
	}
	msg := fmt.Sprintf("%s: exit status %d", argv, e.Code)
	if s := strings.TrimSpace(e.Stderr); s != "" {
		msg += ": " + s
	}
	return msg
}

// ExecRunner runs commands with os/exec.
type ExecRunner struct{}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

// Run implements Runner. On cancellation the command receives SIGTERM first
// (so a docker client forwards it to its container) and is killed after a
// delay.
func (ExecRunner) Run(ctx context.Context, c Cmd) error {
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Dir = c.Dir
	if len(c.Env) > 0 {
		cmd.Env = append(os.Environ(), c.Env...)
	}
	cmd.Stdin = c.Stdin
	cmd.Stdout = c.Stdout
	var tail *tailBuffer
	if c.Stderr != nil {
		cmd.Stderr = c.Stderr
	} else {
		tail = &tailBuffer{max: 4096}
		cmd.Stderr = tail
	}
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 15 * time.Second
	err := cmd.Run()
	if err == nil {
		return nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		out := &ExitError{Argv: append([]string{c.Name}, c.Args...), Code: ee.ExitCode()}
		if tail != nil {
			out.Stderr = string(tail.buf)
		}
		if ctx.Err() != nil {
			return fmt.Errorf("%w: %w", ctx.Err(), out)
		}
		return out
	}
	return fmt.Errorf("run %s: %w", c.Name, err)
}

// limitWriter fails once more than max bytes were written.
type limitWriter struct {
	w   io.Writer
	max int
	n   int
}

var errOutputLimit = errors.New("command output exceeds the limit")

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.n+len(p) > l.max {
		return 0, errOutputLimit
	}
	l.n += len(p)
	return l.w.Write(p)
}

const maxCommandOutput = 64 << 20

// output runs c and returns its standard output.
func output(ctx context.Context, r Runner, c Cmd) ([]byte, error) {
	var buf bytes.Buffer
	c.Stdout = &limitWriter{w: &buf, max: maxCommandOutput}
	if err := r.Run(ctx, c); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ErrNotFound reports that Docker has no such image, volume or container.
var ErrNotFound = errors.New("docker: no such object")

func isNotFound(err error) bool {
	var ee *ExitError
	if errors.As(err, &ee) {
		s := strings.ToLower(ee.Stderr)
		return strings.Contains(s, "no such") || strings.Contains(s, "not found")
	}
	return false
}

// Docker wraps the docker CLI. Bin defaults to "docker".
type Docker struct {
	Run Runner
	Bin string
}

func (d Docker) bin() string {
	if d.Bin == "" {
		return "docker"
	}
	return d.Bin
}

func (d Docker) out(ctx context.Context, args ...string) ([]byte, error) {
	return output(ctx, d.Run, Cmd{Name: d.bin(), Args: args})
}

func (d Docker) do(ctx context.Context, args ...string) error {
	return d.Run.Run(ctx, Cmd{Name: d.bin(), Args: args})
}

// dockerName guards names that become argv elements of docker commands: they
// must not start with "-" and use the characters Docker allows.
var dockerNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

func checkDockerName(kind, name string) error {
	if !dockerNamePattern.MatchString(name) {
		return fmt.Errorf("invalid %s name %q", kind, name)
	}
	return nil
}

// imageRefPattern accepts repository[:tag][@sha256:digest] without options
// injection.
var imageRefPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:-]{0,254}(?:@sha256:[0-9a-f]{64})?$`)

func checkImageRef(ref string) error {
	if !imageRefPattern.MatchString(ref) {
		return fmt.Errorf("invalid image reference %q", ref)
	}
	return nil
}

// ServerInfo is what the update logic needs to know about the Docker engine.
type ServerInfo struct {
	Version string
	Arch    string // amd64 or arm64
	OS      string
}

// Info queries the engine.
func (d Docker) Info(ctx context.Context) (ServerInfo, error) {
	raw, err := d.out(ctx, "version", "--format", "{{json .Server}}")
	if err != nil {
		return ServerInfo{}, fmt.Errorf("docker engine is not reachable: %w", err)
	}
	var s struct {
		Version string
		Os      string
		Arch    string
	}
	if err := json.Unmarshal(bytes.TrimSpace(raw), &s); err != nil {
		return ServerInfo{}, fmt.Errorf("decode docker version: %w", err)
	}
	arch := s.Arch
	switch arch {
	case "x86_64":
		arch = "amd64"
	case "aarch64":
		arch = "arm64"
	}
	if s.Os != "linux" {
		return ServerInfo{}, fmt.Errorf("docker engine OS is %q; LensDock needs a Linux engine", s.Os)
	}
	return ServerInfo{Version: s.Version, Arch: arch, OS: s.Os}, nil
}

// ComposeVersion returns the compose plugin version without the leading v.
func (d Docker) ComposeVersion(ctx context.Context) (string, error) {
	raw, err := d.out(ctx, "compose", "version", "--short")
	if err != nil {
		return "", fmt.Errorf("docker compose is not available: %w", err)
	}
	return strings.TrimPrefix(strings.TrimSpace(string(raw)), "v"), nil
}

// versionAtLeast compares dotted numeric versions (non numeric suffixes are
// ignored).
func versionAtLeast(have string, want ...int) bool {
	parts := strings.Split(have, ".")
	for i, w := range want {
		n := 0
		if i < len(parts) {
			digits := leadingInt.FindString(parts[i])
			n, _ = strconv.Atoi(digits)
		}
		if n != w {
			return n > w
		}
	}
	return true
}

// ImageInfo is the subset of docker image inspect the update logic uses.
type ImageInfo struct {
	ID          string   `json:"Id"`
	RepoDigests []string `json:"RepoDigests"`
	RepoTags    []string `json:"RepoTags"`
	OS          string   `json:"Os"`
	Arch        string   `json:"Architecture"`
	Created     string   `json:"Created"`
	Descriptor  *struct {
		Digest string `json:"digest"`
	} `json:"Descriptor"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
}

// Digests returns every digest this image is known by locally.
func (i ImageInfo) Digests() []string {
	out := []string{i.ID}
	if i.Descriptor != nil && i.Descriptor.Digest != "" {
		out = append(out, i.Descriptor.Digest)
	}
	for _, rd := range i.RepoDigests {
		if _, d, ok := strings.Cut(rd, "@"); ok {
			out = append(out, d)
		}
	}
	return out
}

// HasDigest reports whether digest identifies this image.
func (i ImageInfo) HasDigest(digest string) bool { return contains(i.Digests(), digest) }

// ImageInspect inspects an image reference. A missing image yields
// ErrNotFound.
func (d Docker) ImageInspect(ctx context.Context, ref string) (ImageInfo, error) {
	if err := checkImageRef(ref); err != nil {
		return ImageInfo{}, err
	}
	raw, err := d.out(ctx, "image", "inspect", "--format", "{{json .}}", ref)
	if err != nil {
		if isNotFound(err) {
			return ImageInfo{}, fmt.Errorf("%w: image %s", ErrNotFound, ref)
		}
		return ImageInfo{}, err
	}
	var info ImageInfo
	if err := json.Unmarshal(bytes.TrimSpace(raw), &info); err != nil {
		return ImageInfo{}, fmt.Errorf("decode image inspect: %w", err)
	}
	return info, nil
}

// Pull pulls ref, which must be pinned by digest.
func (d Docker) Pull(ctx context.Context, ref string) error {
	if err := checkImageRef(ref); err != nil {
		return err
	}
	if !strings.Contains(ref, "@sha256:") {
		return fmt.Errorf("refusing to pull %q: only digest-pinned references are pulled", ref)
	}
	return d.do(ctx, "pull", "--quiet", ref)
}

// Tag adds a local tag to an image.
func (d Docker) Tag(ctx context.Context, source, target string) error {
	if err := checkImageRef(source); err != nil {
		return err
	}
	if err := checkImageRef(target); err != nil {
		return err
	}
	return d.do(ctx, "image", "tag", source, target)
}

// VolumeInfo is the subset of docker volume inspect in use.
type VolumeInfo struct {
	Name   string            `json:"Name"`
	Driver string            `json:"Driver"`
	Labels map[string]string `json:"Labels"`
}

// VolumeInspect returns ErrNotFound for a missing volume.
func (d Docker) VolumeInspect(ctx context.Context, name string) (VolumeInfo, error) {
	if err := checkDockerName("volume", name); err != nil {
		return VolumeInfo{}, err
	}
	raw, err := d.out(ctx, "volume", "inspect", "--format", "{{json .}}", name)
	if err != nil {
		if isNotFound(err) {
			return VolumeInfo{}, fmt.Errorf("%w: volume %s", ErrNotFound, name)
		}
		return VolumeInfo{}, err
	}
	var v VolumeInfo
	if err := json.Unmarshal(bytes.TrimSpace(raw), &v); err != nil {
		return VolumeInfo{}, fmt.Errorf("decode volume inspect: %w", err)
	}
	return v, nil
}

// VolumeCreate creates a labelled local volume. It fails when the volume
// already exists with other labels, so an update never adopts foreign data.
func (d Docker) VolumeCreate(ctx context.Context, name string, labels map[string]string) error {
	if err := checkDockerName("volume", name); err != nil {
		return err
	}
	if _, err := d.VolumeInspect(ctx, name); err == nil {
		return fmt.Errorf("volume %s already exists; refusing to reuse it", name)
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	args := []string{"volume", "create"}
	for _, k := range sortedKeys(labels) {
		args = append(args, "--label", k+"="+labels[k])
	}
	args = append(args, name)
	return d.do(ctx, args...)
}

// VolumeRemove removes a volume. It only removes volumes carrying the
// io.lensdock.managed label so a configuration mistake cannot delete working
// data.
func (d Docker) VolumeRemove(ctx context.Context, name string) error {
	v, err := d.VolumeInspect(ctx, name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	if v.Labels[LabelManaged] != "true" {
		return fmt.Errorf("refusing to remove volume %s: it was not created by lensdock update", name)
	}
	return d.do(ctx, "volume", "rm", name)
}

// ContainerInfo is the subset of docker container inspect in use.
type ContainerInfo struct {
	ID    string `json:"Id"`
	Name  string `json:"Name"`
	Image string `json:"Image"`
	State struct {
		Status     string `json:"Status"`
		Running    bool   `json:"Running"`
		ExitCode   int    `json:"ExitCode"`
		OOMKilled  bool   `json:"OOMKilled"`
		Error      string `json:"Error"`
		FinishedAt string `json:"FinishedAt"`
	} `json:"State"`
	Config struct {
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	Mounts []struct {
		Type   string `json:"Type"`
		Name   string `json:"Name"`
		Source string `json:"Source"`
	} `json:"Mounts"`
}

// ContainerInspect returns ErrNotFound for a missing container.
func (d Docker) ContainerInspect(ctx context.Context, id string) (ContainerInfo, error) {
	if err := checkDockerName("container", id); err != nil {
		return ContainerInfo{}, err
	}
	raw, err := d.out(ctx, "container", "inspect", "--format", "{{json .}}", id)
	if err != nil {
		if isNotFound(err) {
			return ContainerInfo{}, fmt.Errorf("%w: container %s", ErrNotFound, id)
		}
		return ContainerInfo{}, err
	}
	var c ContainerInfo
	if err := json.Unmarshal(bytes.TrimSpace(raw), &c); err != nil {
		return ContainerInfo{}, fmt.Errorf("decode container inspect: %w", err)
	}
	return c, nil
}

// NetworkInternal reports whether the named network is internal (no route to
// the outside).
func (d Docker) NetworkInternal(ctx context.Context, name string) (bool, error) {
	if err := checkDockerName("network", name); err != nil {
		return false, err
	}
	raw, err := d.out(ctx, "network", "inspect", "--format", "{{.Internal}}", name)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(raw)) == "true", nil
}

// ManagedVolume is a volume created by lensdock update.
type ManagedVolume struct {
	Name   string
	Labels map[string]string
}

// ManagedVolumes lists the volumes carrying the io.lensdock.managed label.
func (d Docker) ManagedVolumes(ctx context.Context) ([]ManagedVolume, error) {
	raw, err := d.out(ctx, "volume", "ls", "--quiet", "--filter", "label="+LabelManaged+"=true")
	if err != nil {
		return nil, err
	}
	var out []ManagedVolume
	for _, name := range strings.Fields(string(raw)) {
		v, err := d.VolumeInspect(ctx, name)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return nil, err
		}
		out = append(out, ManagedVolume{Name: v.Name, Labels: v.Labels})
	}
	return out, nil
}

// Label keys put on everything the update logic creates.
const (
	LabelManaged = "io.lensdock.managed"
	LabelUpdate  = "io.lensdock.update-id"
	LabelRole    = "io.lensdock.role"
	LabelDataset = "io.lensdock.dataset"
	LabelCreated = "io.lensdock.created-at"
)

// Volume roles recorded in LabelRole.
const (
	RoleCandidate = "candidate"
	RoleScratch   = "scratch"
	RoleRestored  = "restored"
	RoleRetired   = "retired-branch"
	RoleFailed    = "failed-candidate"
)

// ComposeCLI drives `docker compose` for one project and file list.
type ComposeCLI struct {
	D        Docker
	Project  string
	Files    []string
	EnvFiles []string
	Dir      string
}

// Args returns the argv after the docker binary for a compose subcommand:
// compose --env-file F... -p PROJECT [--project-directory DIR] -f FILE... extra.
func (c ComposeCLI) Args(extra ...string) []string { return c.args(extra...) }

func (c ComposeCLI) args(extra ...string) []string {
	a := []string{"compose"}
	for _, f := range c.EnvFiles {
		a = append(a, "--env-file", f)
	}
	a = append(a, "-p", c.Project)
	if c.Dir != "" {
		a = append(a, "--project-directory", c.Dir)
	}
	for _, f := range c.Files {
		a = append(a, "-f", f)
	}
	return append(a, extra...)
}

// ComposeModel is the resolved compose configuration (docker compose config
// --format json) reduced to what the update logic reads.
type ComposeModel struct {
	Name     string                    `json:"name"`
	Services map[string]ComposeService `json:"services"`
	Volumes  map[string]ComposeVolume  `json:"volumes"`
}

// ComposeService is one resolved service.
type ComposeService struct {
	Image         string             `json:"image"`
	ContainerName string             `json:"container_name"`
	Volumes       []ComposeMount     `json:"volumes"`
	Environment   map[string]*string `json:"environment"`
	DependsOn     map[string]any     `json:"depends_on"`
}

// ComposeMount is one volume mount of a service.
type ComposeMount struct {
	Type     string `json:"type"`
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only"`
}

// ComposeVolume is one top-level volume. Name is the resolved Docker volume
// name.
type ComposeVolume struct {
	Name     string `json:"name"`
	External bool   `json:"external"`
}

// Config resolves the compose model.
func (c ComposeCLI) Config(ctx context.Context) (ComposeModel, error) {
	raw, err := c.D.out(ctx, c.args("config", "--format", "json")...)
	if err != nil {
		return ComposeModel{}, fmt.Errorf("resolve compose configuration: %w", err)
	}
	var m ComposeModel
	if err := json.Unmarshal(bytes.TrimSpace(raw), &m); err != nil {
		return ComposeModel{}, fmt.Errorf("decode compose configuration: %w", err)
	}
	return m, nil
}

// UpOptions are the flags of ComposeCLI.Up.
type UpOptions struct {
	NoDeps      bool
	Wait        bool
	WaitTimeout time.Duration
}

// Up starts services detached. Compose never pulls or builds here: the update
// logic made sure the exact images exist locally.
func (c ComposeCLI) Up(ctx context.Context, o UpOptions, services ...string) error {
	extra := []string{"up", "--detach", "--no-build", "--pull", "never"}
	if o.NoDeps {
		extra = append(extra, "--no-deps")
	}
	if o.Wait {
		extra = append(extra, "--wait")
		if o.WaitTimeout > 0 {
			extra = append(extra, "--wait-timeout", strconv.Itoa(int(o.WaitTimeout.Seconds())))
		}
	}
	for _, s := range services {
		if err := checkDockerName("service", s); err != nil {
			return err
		}
	}
	return c.D.do(ctx, c.args(append(extra, services...)...)...)
}

// Stop stops services, waiting up to grace for a clean exit.
func (c ComposeCLI) Stop(ctx context.Context, grace time.Duration, services ...string) error {
	for _, s := range services {
		if err := checkDockerName("service", s); err != nil {
			return err
		}
	}
	extra := append([]string{"stop", "--timeout", strconv.Itoa(int(grace.Seconds()))}, services...)
	return c.D.do(ctx, c.args(extra...)...)
}

// Down removes the project's containers and networks (never volumes).
func (c ComposeCLI) Down(ctx context.Context) error {
	// Do not override service stop_grace_period: the Runtime needs its full
	// grace to close Chromium and release the Profile lock.
	return c.D.do(ctx, c.args("down", "--remove-orphans")...)
}

// ContainerIDs lists the container ids of a service, running or not.
func (c ComposeCLI) ContainerIDs(ctx context.Context, service string) ([]string, error) {
	if err := checkDockerName("service", service); err != nil {
		return nil, err
	}
	raw, err := c.D.out(ctx, c.args("ps", "--all", "--quiet", service)...)
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(raw)), nil
}

// Logs copies the service logs to w.
func (c ComposeCLI) Logs(ctx context.Context, w io.Writer, service string) error {
	if err := checkDockerName("service", service); err != nil {
		return err
	}
	return c.D.Run.Run(ctx, Cmd{Name: c.D.bin(), Args: c.args("logs", "--no-color", "--no-log-prefix", "--tail", "500", service), Stdout: w})
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
