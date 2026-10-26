package update

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/auth"
)

// Engine runs updates, rollbacks and status queries of one deployment. All
// Docker access goes through Docker (argv based); nothing is mounted into the
// browser or the Gateway.
type Engine struct {
	Dep    *Deployment
	Docker Docker
	// Out receives progress lines. They never contain credentials, token
	// digests, PEM data or file contents.
	Out io.Writer
	Now func() time.Time
	// Sleep waits d or until ctx ends. Tests replace it.
	Sleep func(ctx context.Context, d time.Duration) error

	arch string
}

// NewEngine returns an Engine using the real clock.
func NewEngine(dep *Deployment, dk Docker, out io.Writer) *Engine {
	if out == nil {
		out = io.Discard
	}
	return &Engine{Dep: dep, Docker: dk, Out: out, Now: time.Now, Sleep: sleepCtx}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (e *Engine) logf(format string, args ...any) {
	fmt.Fprintf(e.Out, format+"\n", args...)
}

// engineArch returns the architecture of the Docker engine (amd64 or arm64).
func (e *Engine) engineArch(ctx context.Context) (string, error) {
	if e.arch != "" {
		return e.arch, nil
	}
	info, err := e.Docker.Info(ctx)
	if err != nil {
		return "", err
	}
	if !contains(validArchNames, info.Arch) {
		return "", fmt.Errorf("docker engine architecture %q is not supported (amd64 and arm64 are)", info.Arch)
	}
	e.arch = info.Arch
	return e.arch, nil
}

// repoOf strips the tag and the digest from an image reference.
func repoOf(ref string) string {
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	slash := strings.LastIndex(ref, "/")
	if colon := strings.LastIndex(ref, ":"); colon > slash {
		ref = ref[:colon]
	}
	return ref
}

// pinImage resolves a configured image reference to repository@digest and the
// verified digest. When runningImage names the image a container currently
// runs, that image is pinned instead of what the tag points to today.
func (e *Engine) pinImage(ctx context.Context, configured, runningImage string) (ref, digest string, err error) {
	src := configured
	if runningImage != "" {
		src = runningImage
	}
	info, err := e.Docker.ImageInspect(ctx, src)
	if err != nil && runningImage != "" {
		info, err = e.Docker.ImageInspect(ctx, configured)
	}
	if err != nil {
		return "", "", fmt.Errorf("inspect image %s: %w", configured, err)
	}
	repo := repoOf(configured)
	if i := strings.Index(configured, "@sha256:"); i >= 0 && runningImage == "" {
		digest = configured[i+1:]
	} else if info.Descriptor != nil && info.Descriptor.Digest != "" {
		digest = info.Descriptor.Digest
	} else {
		for _, rd := range info.RepoDigests {
			if r, d, ok := strings.Cut(rd, "@"); ok && r == repo {
				digest = d
			}
		}
	}
	if !digestPattern.MatchString(digest) {
		return "", "", fmt.Errorf("image %s has no immutable manifest digest (it was never pushed and the engine does not use the containerd image store); push it to a registry or build it with the containerd image store so it can be pinned", configured)
	}
	ref = repo + "@" + digest
	chk, err := e.Docker.ImageInspect(ctx, ref)
	if err != nil {
		return "", "", fmt.Errorf("image %s cannot be addressed by its digest: %w", configured, err)
	}
	if !chk.HasDigest(digest) {
		return "", "", fmt.Errorf("image %s resolves to another digest than %s", ref, digest)
	}
	return ref, digest, nil
}

// resolveState returns the current state of the deployment. tracked is false
// for the baseline of a deployment that was never updated through the CLI: it
// is derived from the compose files and the images the containers run now.
func (e *Engine) resolveState(ctx context.Context) (st State, tracked bool, model ComposeModel, err error) {
	st, tracked, err = LoadState(e.Dep)
	if err != nil {
		return State{}, false, ComposeModel{}, err
	}
	compose := e.Dep.composeFor(e.Docker, st, tracked)
	model, err = compose.Config(ctx)
	if err != nil {
		return State{}, false, ComposeModel{}, err
	}
	if tracked {
		if err := e.includeRetainedDatasets(&st, model); err != nil {
			return State{}, false, ComposeModel{}, err
		}
		return st, true, model, nil
	}
	st, err = e.baseline(ctx, model, compose)
	return st, false, model, err
}

// includeRetainedDatasets admits newly declared retained volumes without
// rewriting the active generation during a read-only plan. Capacity checks
// still require the volume to exist; the next successful switch persists its
// mapping. Snapshot datasets must already belong to the tracked generation.
func (e *Engine) includeRetainedDatasets(st *State, model ComposeModel) error {
	for owner, sets := range e.Dep.allDatasets() {
		component, exists := st.Components[owner]
		if !exists {
			continue
		}
		for _, ds := range sets {
			if _, exists := component.Volumes[ds.Key]; exists || ds.Role != RoleRetained {
				continue
			}
			volume, ok := model.Volumes[ds.Key]
			if !ok || volume.Name == "" {
				return fmt.Errorf("new retained dataset %s/%s has no resolved compose volume", owner, ds.Key)
			}
			if err := checkDockerName("retained volume", volume.Name); err != nil {
				return err
			}
			if component.Volumes == nil {
				component.Volumes = make(map[string]string)
			}
			component.Volumes[ds.Key] = volume.Name
		}
		st.Components[owner] = component
	}
	return nil
}

func (e *Engine) baseline(ctx context.Context, model ComposeModel, compose ComposeCLI) (State, error) {
	st := State{SchemaVersion: stateSchemaVersion, Components: map[string]ComponentState{}}
	add := func(id, service string, sets []Dataset) error {
		svc, ok := model.Services[service]
		if !ok {
			return fmt.Errorf("service %q is not defined in the compose files", service)
		}
		running := ""
		if ids, err := compose.ContainerIDs(ctx, service); err == nil {
			for _, cid := range ids {
				if ci, err := e.Docker.ContainerInspect(ctx, cid); err == nil && ci.State.Running {
					running = ci.Image
					break
				}
			}
		}
		ref, digest, err := e.pinImage(ctx, svc.Image, running)
		if err != nil {
			return fmt.Errorf("service %s: %w", service, err)
		}
		vols := map[string]string{}
		for _, ds := range sets {
			v, ok := model.Volumes[ds.Key]
			if !ok || v.Name == "" {
				return fmt.Errorf("volume key %q of %s is not declared in the compose files", ds.Key, id)
			}
			vols[ds.Key] = v.Name
		}
		st.Components[id] = ComponentState{Service: service, Image: ref, ImageID: digest, Volumes: vols}
		return nil
	}
	for _, id := range e.Dep.WorkspaceIDs() {
		ws := e.Dep.Workspaces[id]
		if err := add(runtimeComponent(id), ws.RuntimeService, ws.Datasets); err != nil {
			return State{}, err
		}
	}
	if g := e.Dep.Gateway; g != nil {
		if err := add(gatewayComponent, g.Service, g.Datasets); err != nil {
			return State{}, err
		}
	}
	return st, nil
}

// ownedDataset is a dataset with its owner and the volume the current
// generation uses.
type ownedDataset struct {
	Owner   string // runtime/<workspace> or gateway
	Service string
	Dataset
	Volume string
}

// datasetsFor lists the datasets an update of workspace touches: the
// workspace's and, when configured, the Gateway's.
func (e *Engine) datasetsFor(workspace string, st State) ([]ownedDataset, error) {
	var out []ownedDataset
	add := func(owner string, sets []Dataset) error {
		c, ok := st.Components[owner]
		if !ok {
			return fmt.Errorf("state has no component %s", owner)
		}
		for _, ds := range sets {
			v, ok := c.Volumes[ds.Key]
			if !ok {
				return fmt.Errorf("state has no volume for %s/%s", owner, ds.Key)
			}
			out = append(out, ownedDataset{Owner: owner, Service: c.Service, Dataset: ds, Volume: v})
		}
		return nil
	}
	ws, err := e.Dep.Workspace(workspace)
	if err != nil {
		return nil, err
	}
	if err := add(runtimeComponent(workspace), ws.Datasets); err != nil {
		return nil, err
	}
	if g := e.Dep.Gateway; g != nil {
		if err := add(gatewayComponent, g.Datasets); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// linuxCLI returns the Linux lensdock binary run in sidecars for arch and its
// SHA-256.
func (e *Engine) linuxCLI(arch string) (path, sum string, err error) {
	path = e.Dep.LinuxCLI[arch]
	if path == "" && runtime.GOOS == "linux" && runtime.GOARCH == arch {
		if exe, err := os.Executable(); err == nil {
			path = exe
		}
	}
	if path == "" {
		return "", "", fmt.Errorf("no Linux lensdock binary for %s: set linuxCli.%s in deployment.json (mise run build:linux)", arch, arch)
	}
	if strings.ContainsAny(path, ",\"\n") {
		return "", "", fmt.Errorf("linux lensdock path %q contains a character that cannot be used in a mount", path)
	}
	sum, _, err = sha256File(path)
	if err != nil {
		return "", "", fmt.Errorf("linux lensdock binary for %s: %w", arch, err)
	}
	return path, sum, nil
}

// sidecarProbe runs one probe in a throwaway container that shares the network
// namespace of containerID. The container has no capabilities, a read-only
// root file system and only the Linux lensdock binary mounted read-only; the
// request (with credentials) travels on stdin.
func (e *Engine) sidecarProbe(ctx context.Context, image, containerID, cli string, req ProbeRequest) (ProbeResponse, error) {
	if err := checkImageRef(image); err != nil {
		return ProbeResponse{}, err
	}
	if err := checkDockerName("container", containerID); err != nil {
		return ProbeResponse{}, err
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return ProbeResponse{}, err
	}
	args := []string{
		"run", "--rm", "-i",
		"--network", "container:" + containerID,
		"--read-only", "--cap-drop", "ALL",
		"--security-opt", "no-new-privileges:true",
		"--user", "65532:65532",
		"--mount", "type=bind,src=" + cli + ",dst=/usr/local/bin/lensdock,readonly",
		"--entrypoint", "/usr/local/bin/lensdock",
		image, "update", "__probe",
	}
	var out bytes.Buffer
	err = e.Docker.Run.Run(ctx, Cmd{
		Name: e.Docker.bin(), Args: args,
		Stdin:  bytes.NewReader(payload),
		Stdout: &limitWriter{w: &out, max: maxProbeBody},
	})
	if err != nil {
		return ProbeResponse{}, fmt.Errorf("probe sidecar: %w", err)
	}
	var resp ProbeResponse
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		return ProbeResponse{}, errors.New("probe sidecar returned a malformed response")
	}
	return resp, nil
}

// ErrNoOperatorToken is returned by probes when the workspace configures no
// operator token file.
var ErrNoOperatorToken = errors.New("no operatorTokenFile configured for the workspace")

func (e *Engine) readOperatorToken(ws WorkspaceConfig) (string, error) {
	if ws.OperatorTokenFile == "" {
		return "", ErrNoOperatorToken
	}
	data, err := auth.ReadPrivateFile(ws.OperatorTokenFile, 4096)
	if err != nil {
		return "", fmt.Errorf("operator token file: %w", err)
	}
	tok := strings.TrimSpace(string(data))
	if tok == "" {
		return "", errors.New("operator token file is empty")
	}
	return tok, nil
}

func readProbeTLS(p ProbeConfig) (*ProbeTLS, error) {
	if p.CAFile == "" {
		return nil, nil
	}
	read := func(path string) (string, error) {
		data, err := readFileLimit(path, 1<<20)
		if err != nil {
			return "", fmt.Errorf("probe TLS file %s: %w", path[strings.LastIndex(path, "/")+1:], err)
		}
		return string(data), nil
	}
	ca, err := read(p.CAFile)
	if err != nil {
		return nil, err
	}
	cert, err := read(p.CertFile)
	if err != nil {
		return nil, err
	}
	key, err := read(p.KeyFile)
	if err != nil {
		return nil, err
	}
	return &ProbeTLS{CAPEM: ca, CertPEM: cert, KeyPEM: key, ServerName: p.ServerName}, nil
}

// probeErr converts a response-level failure into an error.
func probeErr(resp ProbeResponse) error {
	if resp.Error == "" {
		return nil
	}
	return &probeError{code: resp.Code, msg: resp.Error}
}

// runtimeProbe performs a probe against the running runtime of workspace. With
// an apiUrl the request runs in this process; without one it runs in a
// sidecar in the runtime container's network namespace.
func (e *Engine) runtimeProbe(ctx context.Context, workspace string, st State, tracked bool, req ProbeRequest) (ProbeResponse, error) {
	ws, err := e.Dep.Workspace(workspace)
	if err != nil {
		return ProbeResponse{}, err
	}
	tok, err := e.readOperatorToken(ws)
	if err != nil {
		return ProbeResponse{}, err
	}
	req.Workspace, req.Token = workspace, tok
	if ws.APIURL != "" {
		req.BaseURL = ws.APIURL
		if req.TLS, err = readProbeTLS(ws.Probe); err != nil {
			return ProbeResponse{}, err
		}
		return HandleProbe(ctx, req), nil
	}
	comp, ok := st.Components[runtimeComponent(workspace)]
	if !ok {
		return ProbeResponse{}, fmt.Errorf("state has no runtime component for workspace %s", workspace)
	}
	compose := e.Dep.composeFor(e.Docker, st, tracked)
	id, err := e.runningContainer(ctx, compose, comp.Service)
	if err != nil {
		return ProbeResponse{}, err
	}
	arch, err := e.engineArch(ctx)
	if err != nil {
		return ProbeResponse{}, err
	}
	cli, _, err := e.linuxCLI(arch)
	if err != nil {
		return ProbeResponse{}, err
	}
	req.BaseURL = ws.Probe.URL
	if req.TLS, err = readProbeTLS(ws.Probe); err != nil {
		return ProbeResponse{}, err
	}
	return e.sidecarProbe(ctx, comp.Image, id, cli, req)
}

// ErrNotRunning is returned when a service has no running container.
var ErrNotRunning = errors.New("service has no running container")

// runningContainer returns the id of the running container of service.
func (e *Engine) runningContainer(ctx context.Context, c ComposeCLI, service string) (string, error) {
	ids, err := c.ContainerIDs(ctx, service)
	if err != nil {
		return "", err
	}
	for _, id := range ids {
		ci, err := e.Docker.ContainerInspect(ctx, id)
		if err == nil && ci.State.Running {
			return ci.ID, nil
		}
	}
	return "", fmt.Errorf("%w: %s", ErrNotRunning, service)
}

// waitUntil polls fn every interval until it reports done, fails, or the
// timeout passes.
func (e *Engine) waitUntil(ctx context.Context, timeout, interval time.Duration, fn func(ctx context.Context) (done bool, err error)) error {
	deadline := e.Now().Add(timeout)
	var last error
	for {
		done, err := fn(ctx)
		if done {
			return nil
		}
		last = err
		if !e.Now().Before(deadline) {
			if last == nil {
				last = errors.New("condition not met")
			}
			return fmt.Errorf("timed out after %s: %w", timeout, last)
		}
		if err := e.Sleep(ctx, interval); err != nil {
			return err
		}
	}
}

// checkImageIdentity proves info is the image ref names: the exact digest, as
// a linux image for the engine's architecture.
func checkImageIdentity(info ImageInfo, ref, digest, arch string) error {
	if !info.HasDigest(digest) {
		return fmt.Errorf("image %s resolved to a different digest (%s)", ref, info.ID)
	}
	if info.Arch != arch || info.OS != "linux" {
		return fmt.Errorf("image %s is %s/%s, the engine needs linux/%s", ref, info.OS, info.Arch, arch)
	}
	return nil
}

// ensureImage makes sure ref (repository@digest) is present locally and has
// that exact digest. Pulling is always an explicit operator action performed
// before planning, so disk checks cannot be invalidated by an implicit pull.
func (e *Engine) ensureImage(ctx context.Context, ref, digest, arch string) error {
	info, err := e.Docker.ImageInspect(ctx, ref)
	if errors.Is(err, ErrNotFound) {
		return fmt.Errorf("image %s is not present locally", ref)
	}
	if err != nil {
		return err
	}
	return checkImageIdentity(info, ref, digest, arch)
}
