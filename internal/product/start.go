package product

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/update"
)

// Image pull policies of the first start.
const (
	PullAlways  = "always"
	PullMissing = "missing"
	PullNever   = "never"
)

const (
	defaultDockerBinary = "docker"
	startLockFile       = ".lifecycle.lock"
	readyTimeout        = 90 * time.Second
)

// StartOptions are the caller's choices for Start.
type StartOptions struct {
	// PullPolicy is the image pull policy of the first start: PullAlways,
	// PullMissing or PullNever. Empty means PullAlways. A deployment that
	// already started never pulls.
	PullPolicy string
}

// CheckDocker fails clearly when there is no usable Docker engine with Compose v2.
func CheckDocker(ctx context.Context, d update.Docker) error {
	if _, err := d.Info(ctx); err != nil {
		return fmt.Errorf("%w (start Docker Desktop, OrbStack or the docker service, then retry)", err)
	}
	if _, err := d.ComposeVersion(ctx); err != nil {
		return fmt.Errorf("Docker Compose v2 is required: %w", err)
	}
	return nil
}

// starter bundles the Docker handle and the log of one Start call. Every
// Docker command of Start, including ownership queries and image pulls, runs
// through it.
type starter struct {
	d   update.Docker
	log io.Writer
}

// bin is the docker binary of the passed Docker; the passed Runner decides
// what actually runs.
func (s starter) bin() string {
	if s.d.Bin != "" {
		return s.d.Bin
	}
	return defaultDockerBinary
}

func (s starter) run(ctx context.Context, args ...string) error {
	return s.d.Run.Run(ctx, update.Cmd{Name: s.bin(), Args: args, Stdout: s.log, Stderr: s.log})
}

// capture returns the standard output of a docker command.
func (s starter) capture(ctx context.Context, args ...string) (string, error) {
	var out bytes.Buffer
	err := s.d.Run.Run(ctx, update.Cmd{Name: s.bin(), Args: args, Stdout: &out})
	return out.String(), err
}

// Start starts the product of an initialized deployment.
//
// docker must operate dep: build it with DeploymentDocker so the variables of
// the deployment's Compose env files win over the process environment. Every
// Docker command runs through docker.Run with docker.Bin, so a caller can pin
// the engine and the binary beneath it. Progress and Docker output go to log.
//
// The first start of a release deployment resolves the configured images once
// (pulling them per opts.PullPolicy), writes the exact digests to the pin file
// that sits after the base Compose file, prepares the credential volumes
// (config-init) and starts the services. Every later start only runs
// `up --pull=never` through the deployment's own Compose arguments: the pin
// file wins over any LENSDOCK_*_IMAGE variable, so a restart never moves to a
// newer `latest`, and image changes go through `lensdock update`.
//
// Until a first start has succeeded, the Compose project must hold no
// resources that this deployment did not create: config-init would replace
// the credentials of foreign containers and volumes. After the first start
// the only writes to the product directory are the pin file and the started
// marker.
func Start(ctx context.Context, dep *update.Deployment, docker update.Docker, opts StartOptions, log io.Writer) error {
	if log == nil {
		log = io.Discard
	}
	if docker.Run == nil {
		return errors.New("product: start requires a Docker runner")
	}
	policy := opts.PullPolicy
	if policy == "" {
		policy = PullAlways
	}
	switch policy {
	case PullAlways, PullMissing, PullNever:
	default:
		return optionErrorf("pull policy must be %s, %s or %s", PullAlways, PullMissing, PullNever)
	}
	unlock, err := lockFile(filepath.Join(dep.StateDir, startLockFile))
	if err != nil {
		return err
	}
	defer unlock()
	s := starter{d: docker, log: log}
	if err := CheckDocker(ctx, docker); err != nil {
		return err
	}
	eng := update.NewEngine(dep, docker, log)
	_, tracked, err := update.LoadState(dep)
	if err != nil {
		return err
	}
	pin, err := readPin(dep)
	if err != nil {
		return err
	}
	startedPath := filepath.Join(dep.StateDir, StartedFile)
	_, startedErr := os.Lstat(startedPath)
	bootstrap := !tracked && pin.listed && startedErr != nil

	// A deployment that update apply manages keeps its credential volumes
	// and images in its generation; starting it is only `up`.
	if !tracked {
		if bootstrap {
			if err := s.requireOwnedProject(ctx, dep, pin); err != nil {
				return err
			}
			if pin.pending {
				if err := s.pinFirstStartImages(ctx, eng, dep, policy); err != nil {
					return err
				}
			}
		}
		cc := update.ComposeCLI{D: docker, Project: dep.Compose.Project, Files: dep.Compose.Files, EnvFiles: dep.Compose.EnvFiles, Dir: dep.Compose.ProjectDir}
		if err := s.run(ctx, cc.Args("run", "--rm", "-T", "config-init")...); err != nil {
			return fmt.Errorf("prepare credentials (config-init): %w", err)
		}
	}
	name, argv, err := eng.ComposeCommand([]string{"up", "-d", "--wait", "--wait-timeout", "300", "--pull=never"})
	if err != nil {
		return err
	}
	if err := docker.Run.Run(ctx, update.Cmd{Name: name, Args: argv, Stdout: log, Stderr: log}); err != nil {
		return fmt.Errorf("start the product: %w (inspect with `lensdock update compose --deployment %s -- logs`)", err, dep.File)
	}
	if dep.Gateway != nil && dep.Gateway.ReadyURL != "" {
		if err := waitReady(ctx, dep.Gateway.ReadyURL); err != nil {
			return fmt.Errorf("the Gateway did not become ready: %w (inspect with `lensdock update compose --deployment %s -- logs gateway runtime`)", err, dep.File)
		}
	}
	if bootstrap {
		if err := writeFileAtomic(startedPath, []byte("started\n"), 0o600); err != nil {
			return fmt.Errorf("record the first start: %w", err)
		}
	}
	return nil
}

// pinState describes the pin file of a deployment.
type pinState struct {
	listed  bool   // the deployment lists the pin file: it was created from a release bundle
	pending bool   // the pin file is still the placeholder: no first start has resolved images
	content string // the pin file
}

// readPin reads the pin file of dep. A deployment that does not list one (a
// source checkout, or one created before pin files) keeps the images its
// compose.env or environment names.
func readPin(dep *update.Deployment) (pinState, error) {
	pin := filepath.Join(dep.StateDir, PinFile)
	var s pinState
	for _, f := range dep.Compose.Files {
		if f == pin {
			s.listed = true
		}
	}
	if !s.listed {
		return s, nil
	}
	data, err := os.ReadFile(pin)
	if err != nil {
		return s, fmt.Errorf("read %s: %w", pin, err)
	}
	s.content = string(data)
	s.pending = s.content == PinPlaceholder
	return s, nil
}

// pinnedImage returns the image the pin file sets for service.
func pinnedImage(content, service string) (string, bool) {
	lines := strings.Split(content, "\n")
	for i, l := range lines {
		if l != "  "+service+":" || i+1 >= len(lines) {
			continue
		}
		v, ok := strings.CutPrefix(lines[i+1], "    image: ")
		if !ok {
			return "", false
		}
		ref, err := strconv.Unquote(v)
		if err != nil || update.CheckImageRef(ref) != nil {
			return "", false
		}
		return ref, true
	}
	return "", false
}

// requireOwnedProject refuses to start a deployment that has not completed a
// first start while its Compose project holds Docker resources it did not
// create: config-init would replace the credentials those services use. A
// first start that failed half way leaves the credential volume of this
// deployment behind; it is recognized by the CA certificate it holds.
func (s starter) requireOwnedProject(ctx context.Context, dep *update.Deployment, pin pinState) error {
	project := dep.Compose.Project
	label := "label=com.docker.compose.project=" + project
	volumes, err := s.capture(ctx, "volume", "ls", "-q", "--filter", label)
	if err != nil {
		return fmt.Errorf("look for existing %q resources: %w", project, err)
	}
	containers, err := s.capture(ctx, "ps", "-a", "-q", "--filter", label)
	if err != nil {
		return fmt.Errorf("look for existing %q resources: %w", project, err)
	}
	if strings.TrimSpace(volumes) == "" && strings.TrimSpace(containers) == "" {
		return nil
	}
	foreign := fmt.Errorf("Docker already has containers or volumes of the Compose project %q that this deployment did not create, so starting it could replace the credentials those services use. Choose a different project with `lensdock admin product-init --project NAME` (or $%s), or operate the existing deployment from its own state directory", project, EnvComposeProject)
	if pin.pending {
		// Nothing of this deployment ran yet, so everything there is foreign.
		return foreign
	}
	owned, err := s.ownsCredentials(ctx, dep, pin, volumes)
	if err != nil {
		fmt.Fprintf(s.log, "warning: cannot verify who created the existing %q resources: %v\n", project, err)
		return foreign
	}
	if !owned {
		return foreign
	}
	return nil
}

// ownsCredentials reports whether the project's gateway credential volume
// holds the CA certificate of this deployment. Only the public certificate is
// read; nothing is printed.
func (s starter) ownsCredentials(ctx context.Context, dep *update.Deployment, pin pinState, volumes string) (bool, error) {
	volume := dep.Compose.Project + "_gateway-credentials"
	found := false
	for _, v := range strings.Fields(volumes) {
		if v == volume {
			found = true
		}
	}
	if !found {
		return false, nil
	}
	image, ok := pinnedImage(pin.content, "config-init")
	if !ok {
		return false, errors.New("the pin file names no config-init image")
	}
	ours, err := os.ReadFile(filepath.Join(dep.StateDir, "ca.crt"))
	if err != nil {
		return false, fmt.Errorf("read the deployment CA certificate: %w", err)
	}
	if len(bytes.TrimSpace(ours)) == 0 {
		return false, errors.New("the deployment CA certificate is empty")
	}
	theirs, err := s.capture(ctx, "run", "--rm", "--pull", "never", "--network", "none", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--user", "0", "-v", volume+":/proof:ro", "--entrypoint", "cat", image, "/proof/ca.crt")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(theirs) == strings.TrimSpace(string(ours)), nil
}

// pinFirstStartImages pulls and pins the images named in compose.env and
// writes them to the pin file. The atomic rename of the pin file is the commit
// point: before it the first start can be repeated, after it the images are
// fixed.
func (s starter) pinFirstStartImages(ctx context.Context, eng *update.Engine, dep *update.Deployment, policy string) error {
	envFile := filepath.Join(dep.StateDir, "compose.env")
	listed := false
	for _, f := range dep.Compose.EnvFiles {
		if f == envFile {
			listed = true
		}
	}
	if !listed {
		return fmt.Errorf("deployment %s does not list %s as a Compose env file", dep.File, envFile)
	}
	env, err := readComposeEnv(envFile)
	if err != nil {
		return err
	}
	runtimeSvc := ""
	for _, id := range dep.WorkspaceIDs() {
		runtimeSvc = dep.Workspaces[id].RuntimeService
		break
	}
	if runtimeSvc == "" || dep.Gateway == nil || dep.Gateway.Service == "" {
		return errors.New("the deployment names no runtime or gateway service to pin")
	}
	runtimeRef, err := s.resolveImage(ctx, eng, env[EnvRuntimeImage], EnvRuntimeImage, policy)
	if err != nil {
		return err
	}
	gatewayRef, err := s.resolveImage(ctx, eng, env[EnvGatewayImage], EnvGatewayImage, policy)
	if err != nil {
		return err
	}
	// config-init runs the Gateway image.
	yaml := fmt.Sprintf("# Written by `lensdock admin product-start` at the first start. Compose applies this file\n# after the base Compose file, so LENSDOCK_RUNTIME_IMAGE and LENSDOCK_GATEWAY_IMAGE in the\n# environment cannot move a started deployment. Change images with `lensdock update`.\nservices:\n  %s:\n    image: %q\n  %s:\n    image: %q\n  config-init:\n    image: %q\n", runtimeSvc, runtimeRef, dep.Gateway.Service, gatewayRef, gatewayRef)
	if err := writeFileAtomic(filepath.Join(dep.StateDir, PinFile), []byte(yaml), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", PinFile, err)
	}
	fmt.Fprintf(s.log, "Pinned runtime %s\nPinned gateway %s\n", runtimeRef, gatewayRef)
	return nil
}

// resolveImage makes ref available per the pull policy and returns its
// immutable repository@digest reference. An image without a registry digest
// cannot be pinned and fails instead of staying a mutable tag.
func (s starter) resolveImage(ctx context.Context, eng *update.Engine, ref, variable, policy string) (string, error) {
	if ref == "" {
		return "", fmt.Errorf("compose.env has no %s", variable)
	}
	if err := update.CheckImageRef(ref); err != nil {
		return "", fmt.Errorf("%s: %w", variable, err)
	}
	present := true
	if _, err := s.d.ImageInspect(ctx, ref); errors.Is(err, update.ErrNotFound) {
		present = false
	} else if err != nil {
		return "", err
	}
	switch {
	case policy == PullAlways || policy == PullMissing && !present:
		fmt.Fprintf(s.log, "Pulling %s\n", ref)
		if err := s.run(ctx, "pull", ref); err != nil {
			return "", fmt.Errorf("pull %s: %w\nIf this is a private GitHub Container Registry package, authenticate first with `docker login ghcr.io` (a token with read:packages), or pass --pull never with images that are already loaded", ref, err)
		}
	case !present:
		return "", fmt.Errorf("image %s is not present locally and --pull is %s", ref, policy)
	}
	pinned, _, err := eng.PinImage(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("pin %s: %w", ref, err)
	}
	return pinned, nil
}

// waitReady polls the Gateway readiness URL until it answers 200.
func waitReady(ctx context.Context, url string) error {
	client := &http.Client{Timeout: 3 * time.Second}
	deadline := time.Now().Add(readyTimeout)
	var last error
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			err = fmt.Errorf("%s answered %s", url, resp.Status)
		}
		last = err
		if time.Now().After(deadline) {
			return last
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
