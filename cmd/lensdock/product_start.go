package main

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

	"github.com/JiaWeiXie/lensdock/internal/auth"
	"github.com/JiaWeiXie/lensdock/internal/update"
)

const (
	dockerBinary = "docker"
	envPull      = "LENSDOCK_PULL"

	pullAlways  = "always"
	pullMissing = "missing"
	pullNever   = "never"

	// startedFile is written next to the pin file once the first start of a
	// release deployment has succeeded.
	startedFile = "started"

	readyTimeout = 90 * time.Second
)

// productStart starts the product of an initialized deployment.
//
// The first start of a release deployment resolves the configured images once
// (pulling them), writes the exact digests to the pin file that sits after the
// base Compose file, prepares the credential volumes (config-init) and starts
// the services. Every later start only runs `up --pull=never` through the
// deployment's own Compose arguments: the pin file wins over any
// LENSDOCK_*_IMAGE variable, so a restart never moves to a newer `latest`, and
// image changes go through `lensdock update`.
//
// Until a first start has succeeded, the Compose project must hold no
// resources that this deployment did not create: config-init would replace
// the credentials of foreign containers and volumes.
func (c *cli) productStart(ctx context.Context, args []string) error {
	fs := c.newFlagSet("admin product-start")
	df := c.addDeploymentFlags(fs)
	pullDefault := c.getenv(envPull)
	if pullDefault == "" {
		pullDefault = pullAlways
	}
	pull := fs.String("pull", pullDefault, "image pull policy of the first start: always, missing or never (default $"+envPull+" or always)")
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	switch *pull {
	case pullAlways, pullMissing, pullNever:
	default:
		return usageErrorf("--pull / %s must be always, missing or never", envPull)
	}
	depPath, err := df.path()
	if err != nil {
		return err
	}
	dep, err := update.LoadDeployment(depPath)
	if err != nil {
		return err
	}
	d := c.deploymentDocker(dep)
	if err := c.checkDocker(ctx, d); err != nil {
		return err
	}
	eng := update.NewEngine(dep, d, c.stderr)
	_, tracked, err := update.LoadState(dep)
	if err != nil {
		return err
	}
	pin, err := readPin(dep)
	if err != nil {
		return err
	}
	startedPath := filepath.Join(dep.StateDir, startedFile)
	_, startedErr := os.Lstat(startedPath)
	bootstrap := !tracked && pin.listed && startedErr != nil

	// A deployment that update apply manages keeps its credential volumes
	// and images in its generation; starting it is only `up`.
	if !tracked {
		if bootstrap {
			if err := c.requireOwnedProject(ctx, dep, pin); err != nil {
				return err
			}
			if pin.pending {
				if err := c.pinFirstStartImages(ctx, d, eng, dep, *pull); err != nil {
					return err
				}
			}
		}
		cc := update.ComposeCLI{D: d, Project: dep.Compose.Project, Files: dep.Compose.Files, EnvFiles: dep.Compose.EnvFiles, Dir: dep.Compose.ProjectDir}
		if err := d.Run.Run(ctx, update.Cmd{Name: dockerBinary, Args: cc.Args("run", "--rm", "-T", "config-init"), Stdout: c.stderr, Stderr: c.stderr}); err != nil {
			return fmt.Errorf("prepare credentials (config-init): %w", err)
		}
	}
	name, argv, err := eng.ComposeCommand([]string{"up", "-d", "--wait", "--wait-timeout", "300", "--pull=never"})
	if err != nil {
		return err
	}
	if err := d.Run.Run(ctx, update.Cmd{Name: name, Args: argv, Stdout: c.stderr, Stderr: c.stderr}); err != nil {
		return fmt.Errorf("start the product: %w (inspect with `lensdock update compose --deployment %s -- logs`)", err, dep.File)
	}
	ready := dep.Gateway != nil && dep.Gateway.ReadyURL != ""
	if ready {
		if err := waitReady(ctx, dep.Gateway.ReadyURL); err != nil {
			return fmt.Errorf("the Gateway did not become ready: %w (inspect with `lensdock update compose --deployment %s -- logs gateway runtime`)", err, dep.File)
		}
	}
	if bootstrap {
		if err := writeFileAtomic(startedPath, []byte("started\n"), 0o600); err != nil {
			return fmt.Errorf("record the first start: %w", err)
		}
	}
	if ready {
		_, err = fmt.Fprintf(c.stdout, "LensDock is ready at %s/\nPair with the contents of the private file %s (never put a token in a URL or command line).\n", strings.TrimSuffix(dep.Gateway.ReadyURL, "/readyz"), filepath.Join(dep.StateDir, "admin-token"))
		return err
	}
	return nil
}

func (c *cli) runDocker(ctx context.Context, cmd update.Cmd) error {
	return c.baseRunner().Run(ctx, cmd)
}

// capture returns the standard output of a docker command.
func (c *cli) capture(ctx context.Context, args ...string) (string, error) {
	var out bytes.Buffer
	err := c.runDocker(ctx, update.Cmd{Name: dockerBinary, Args: args, Stdout: &out})
	return out.String(), err
}

// checkDocker fails clearly when there is no usable Docker engine with Compose v2.
func (c *cli) checkDocker(ctx context.Context, d update.Docker) error {
	if _, err := d.Info(ctx); err != nil {
		return fmt.Errorf("%w (start Docker Desktop, OrbStack or the docker service, then retry)", err)
	}
	if _, err := d.ComposeVersion(ctx); err != nil {
		return fmt.Errorf("Docker Compose v2 is required: %w", err)
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
	pin := filepath.Join(dep.StateDir, pinFile)
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
	s.pending = s.content == pinPlaceholder
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
func (c *cli) requireOwnedProject(ctx context.Context, dep *update.Deployment, pin pinState) error {
	project := dep.Compose.Project
	label := "label=com.docker.compose.project=" + project
	volumes, err := c.capture(ctx, "volume", "ls", "-q", "--filter", label)
	if err != nil {
		return fmt.Errorf("look for existing %q resources: %w", project, err)
	}
	containers, err := c.capture(ctx, "ps", "-a", "-q", "--filter", label)
	if err != nil {
		return fmt.Errorf("look for existing %q resources: %w", project, err)
	}
	if strings.TrimSpace(volumes) == "" && strings.TrimSpace(containers) == "" {
		return nil
	}
	foreign := fmt.Errorf("Docker already has containers or volumes of the Compose project %q that this deployment did not create, so starting it could replace the credentials those services use. Choose a different project with `lensdock admin product-init --project NAME` (or $%s), or operate the existing deployment from its own state directory", project, envComposeProject)
	if pin.pending {
		// Nothing of this deployment ran yet, so everything there is foreign.
		return foreign
	}
	owned, err := c.ownsCredentials(ctx, dep, pin, volumes)
	if err != nil {
		fmt.Fprintf(c.stderr, "warning: cannot verify who created the existing %q resources: %v\n", project, err)
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
func (c *cli) ownsCredentials(ctx context.Context, dep *update.Deployment, pin pinState, volumes string) (bool, error) {
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
	theirs, err := c.capture(ctx, "run", "--rm", "--pull", "never", "--network", "none", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--user", "0", "-v", volume+":/proof:ro", "--entrypoint", "cat", image, "/proof/ca.crt")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(theirs) == strings.TrimSpace(string(ours)), nil
}

// pinFirstStartImages pulls and pins the images named in compose.env and
// writes them to the pin file. The atomic rename of the pin file is the commit
// point: before it the first start can be repeated, after it the images are
// fixed.
func (c *cli) pinFirstStartImages(ctx context.Context, d update.Docker, eng *update.Engine, dep *update.Deployment, policy string) error {
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
	runtimeRef, err := c.resolveImage(ctx, d, eng, env[envRuntimeImage], envRuntimeImage, policy)
	if err != nil {
		return err
	}
	gatewayRef, err := c.resolveImage(ctx, d, eng, env[envGatewayImage], envGatewayImage, policy)
	if err != nil {
		return err
	}
	// config-init runs the Gateway image.
	yaml := fmt.Sprintf("# Written by `lensdock admin product-start` at the first start. Compose applies this file\n# after the base Compose file, so LENSDOCK_RUNTIME_IMAGE and LENSDOCK_GATEWAY_IMAGE in the\n# environment cannot move a started deployment. Change images with `lensdock update`.\nservices:\n  %s:\n    image: %q\n  %s:\n    image: %q\n  config-init:\n    image: %q\n", runtimeSvc, runtimeRef, dep.Gateway.Service, gatewayRef, gatewayRef)
	if err := writeFileAtomic(filepath.Join(dep.StateDir, pinFile), []byte(yaml), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", pinFile, err)
	}
	fmt.Fprintf(c.stderr, "Pinned runtime %s\nPinned gateway %s\n", runtimeRef, gatewayRef)
	return nil
}

// resolveImage makes ref available per the pull policy and returns its
// immutable repository@digest reference. An image without a registry digest
// cannot be pinned and fails instead of staying a mutable tag.
func (c *cli) resolveImage(ctx context.Context, d update.Docker, eng *update.Engine, ref, variable, policy string) (string, error) {
	if ref == "" {
		return "", fmt.Errorf("compose.env has no %s", variable)
	}
	if err := update.CheckImageRef(ref); err != nil {
		return "", fmt.Errorf("%s: %w", variable, err)
	}
	present := true
	if _, err := d.ImageInspect(ctx, ref); errors.Is(err, update.ErrNotFound) {
		present = false
	} else if err != nil {
		return "", err
	}
	switch {
	case policy == pullAlways || policy == pullMissing && !present:
		fmt.Fprintf(c.stderr, "Pulling %s\n", ref)
		if err := c.runDocker(ctx, update.Cmd{Name: dockerBinary, Args: []string{"pull", ref}, Stdout: c.stderr, Stderr: c.stderr}); err != nil {
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

// readComposeEnv reads the KEY=VALUE lines of a private compose env file.
func readComposeEnv(path string) (map[string]string, error) {
	data, err := auth.ReadPrivateFile(path, 1<<16)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		if k, v, ok := parseEnvLine(line); ok {
			out[k] = v
		}
	}
	return out, nil
}

// writeFileAtomic replaces path with data through a same-directory rename.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-"+filepath.Base(path)+"-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, werr := tmp.Write(data)
	cherr := tmp.Chmod(mode)
	serr := tmp.Sync()
	clerr := tmp.Close()
	if err := errors.Join(werr, cherr, serr, clerr); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
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
