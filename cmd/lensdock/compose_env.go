package main

import (
	"context"
	"io"
	"os"
	"regexp"
	"strings"

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

// parseEnvLine reads one KEY=VALUE line of a compose env file. It accepts only
// what it can interpret exactly: plain values and the single-quoted form that
// product-init writes. Anything else is left to Compose itself.
func parseEnvLine(line string) (key, value string, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
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

// composeEnv returns the KEY=VALUE variables defined by the Compose env files
// of dep, in file order (a later definition wins).
func composeEnv(dep *update.Deployment) []string {
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

// composeEnvRunner adds env to every command it starts.
type composeEnvRunner struct {
	base update.Runner
	env  []string
}

func (r composeEnvRunner) Run(ctx context.Context, c update.Cmd) error {
	if len(r.env) > 0 {
		env := make([]string, 0, len(r.env)+len(c.Env))
		env = append(env, r.env...)
		c.Env = append(env, c.Env...)
	}
	return r.base.Run(ctx, c)
}

// baseRunner starts external commands (update.ExecRunner unless a test
// replaced it).
func (c *cli) baseRunner() update.Runner {
	if c.runner != nil {
		return c.runner
	}
	return update.ExecRunner{}
}

// deploymentDocker is the Docker wrapper for commands that operate dep.
func (c *cli) deploymentDocker(dep *update.Deployment) update.Docker {
	return update.Docker{Run: composeEnvRunner{base: c.baseRunner(), env: composeEnv(dep)}}
}
