package product

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/JiaWeiXie/lensdock/internal/auth"
	"github.com/JiaWeiXie/lensdock/internal/update"
)

// A deployment records its interpolation inputs (state directory, host port,
// public origin, mode, images) in compose.env. Compose lets the process
// environment win over an --env-file, so an exported LENSDOCK_STATE_DIR (mise
// sets one for the source checkout) or LENSDOCK_RUNTIME_IMAGE would silently
// rewire or roll a deployment that lives somewhere else. Every docker command
// of a deployment therefore starts with the variables its env files define.
// Variables the env files do not define keep coming from the environment.

var (
	envNamePattern   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	plainEnvPattern  = regexp.MustCompile(`^[A-Za-z0-9_./:@+,=-]*$`)
	maxComposeEnvLen = int64(1 << 16)
)

// composeEnvValue quotes a Compose env value. Single-quoted values do not
// interpolate dollar signs.
func composeEnvValue(value string) string { return "'" + strings.ReplaceAll(value, "'", "\\'") + "'" }

// parseEnvLine reads one KEY=VALUE line of a compose env file. It accepts only
// what it can interpret exactly: plain values and the single-quoted form that
// Initialize writes. Anything else is left to Compose itself.
func parseEnvLine(line string) (key, value string, ok bool) {
	k, v, found := strings.Cut(line, "=")
	if !found {
		return "", "", false
	}
	k, v = strings.TrimSpace(k), strings.TrimSpace(v)
	if !envNamePattern.MatchString(k) || strings.ContainsRune(v, 0) {
		return "", "", false
	}
	switch {
	case len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'':
		return k, strings.ReplaceAll(v[1:len(v)-1], `\'`, `'`), true
	case plainEnvPattern.MatchString(v):
		return k, v, true
	}
	return "", "", false
}

// ComposeEnv returns the KEY=VALUE variables defined by the Compose env files
// of dep, in file order (a later definition wins).
func ComposeEnv(dep *update.Deployment) []string {
	var env []string
	for _, f := range dep.Compose.EnvFiles {
		fh, err := os.Open(f)
		if err != nil {
			continue // Compose reports an unreadable env file itself
		}
		data, err := io.ReadAll(io.LimitReader(fh, maxComposeEnvLen))
		fh.Close()
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			if k, v, ok := parseEnvLine(line); ok {
				env = append(env, k+"="+v)
			}
		}
	}
	return env
}

// readComposeEnv reads the KEY=VALUE lines of a private compose env file.
func readComposeEnv(path string) (map[string]string, error) {
	data, err := auth.ReadPrivateFile(path, maxComposeEnvLen)
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

// HelperPidsEnv is the opt-in deployment setting that caps the processes of
// every helper container (migration, verifier, probe sidecar, volume helper).
// Unset or 0 means unlimited. A Compose env file definition wins over the
// process environment.
const HelperPidsEnv = "LENSDOCK_HELPER_PIDS_LIMIT"

// helperPidsLimit reads HelperPidsEnv from the deployment variables, falling
// back to the process environment.
func helperPidsLimit(env []string) (int64, error) {
	value := os.Getenv(HelperPidsEnv)
	for _, entry := range env {
		if k, v, ok := strings.Cut(entry, "="); ok && k == HelperPidsEnv {
			value = v
		}
	}
	if value == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer; 0 means unlimited", HelperPidsEnv)
	}
	return n, nil
}

// HelperPolicyFile is the Compose override, in the product directory, that
// applies the helper PID cap to config-init. A deployment owns it next to its
// pin file: the copied docker/compose.yaml of an installed release is kept
// byte for byte and may predate the setting, so the cap cannot live there.
// A managed deployment's file has other content, owned by the host manager.
const HelperPolicyFile = "helper-policy.yaml"

// standaloneHelperPolicy resolves the optional cap from the deployment's
// interpolation inputs (see HelperPidsEnv). 0 or unset leaves config-init
// unlimited.
const standaloneHelperPolicy = "# Applies the optional LENSDOCK_HELPER_PIDS_LIMIT cap to config-init, whatever release\n# copy of docker/compose.yaml this deployment pins. 0 or unset is unlimited.\nservices:\n  config-init:\n    pids_limit: ${LENSDOCK_HELPER_PIDS_LIMIT:-0}\n"

// ensureHelperPolicy creates the helper policy override of a standalone
// deployment and lists it after the deployment's Compose files, so the cap
// reaches config-init through the deployment's own Compose arguments. The
// content is fixed: any other content is refused rather than trusted, since
// it could lift the cap. Base files and the pin file are never touched.
func ensureHelperPolicy(dep *update.Deployment) error {
	file := filepath.Join(dep.StateDir, HelperPolicyFile)
	data, err := os.ReadFile(file)
	switch {
	case err == nil:
		if string(data) != standaloneHelperPolicy {
			return fmt.Errorf("%s differs from the helper policy this deployment owns; refusing to use it", file)
		}
	case os.IsNotExist(err):
		if err := auth.WritePrivateFile(file, []byte(standaloneHelperPolicy)); err != nil {
			return err
		}
	default:
		return err
	}
	for _, f := range dep.Compose.Files {
		if f == file {
			return nil
		}
	}
	dep.Compose.Files = append(dep.Compose.Files, file)
	return dep.Save()
}

// composeEnvRunner adds env to every command it starts and validates the
// optional helper PID cap: `docker run` receives it as a flag, while Compose
// interpolates it through helper-policy.yaml, so an
// invalid value is refused instead of reaching Compose.
type composeEnvRunner struct {
	base update.Runner
	env  []string
}

func (r composeEnvRunner) Run(ctx context.Context, c update.Cmd) error {
	if len(c.Args) > 0 && (c.Args[0] == "run" || c.Args[0] == "compose") {
		pids, err := helperPidsLimit(r.env)
		if err != nil {
			return err
		}
		if c.Args[0] == "run" && pids > 0 {
			c.Args = append([]string{"run", "--pids-limit", strconv.FormatInt(pids, 10)}, c.Args[1:]...)
		}
	}
	if len(r.env) > 0 {
		env := make([]string, 0, len(r.env)+len(c.Env))
		env = append(env, r.env...)
		c.Env = append(env, c.Env...)
	}
	return r.base.Run(ctx, c)
}

// DeploymentDocker returns the Docker wrapper for commands that operate dep:
// every command starts with the variables dep's Compose env files define, then
// the caller's own Cmd.Env (later entries win), and runs through base. A nil
// base runs real commands (update.ExecRunner). A caller that must pin the
// docker binary or target engine puts that restriction in base; the wrapper
// adds nothing but the variables and leaves Docker.Bin to that caller.
func DeploymentDocker(dep *update.Deployment, base update.Runner) update.Docker {
	if base == nil {
		base = update.ExecRunner{}
	}
	return update.Docker{Run: composeEnvRunner{base: base, env: ComposeEnv(dep)}}
}
