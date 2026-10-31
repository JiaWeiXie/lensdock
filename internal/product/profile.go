package product

import "strings"

// ProfileServer is the deployment profile for a host that is not a single-user
// laptop: a shared machine, a server or a cloud VM.
const ProfileServer = "server"

// profileSettings holds the lines a profile adds to the Runtime and Compose
// environment files of a new deployment.
type profileSettings struct {
	runtimeEnv []string // runtime.env, read by the Runtime container
	composeEnv []string // compose.env, read by Compose
}

// profileFor returns the settings of a profile; the empty profile adds nothing.
//
// The server profile turns on what the laptop defaults leave off: Chromium's
// own sandbox (the Runtime then needs unprivileged user namespaces and stays not
// ready instead of falling back), required pairing, and memory, CPU and process
// limits. The limit values are starting points sized for one browser workspace;
// measure on the real host and adjust them in compose.env. The profile does not
// restrict network egress: add host firewall rules for that (docker/browser/README.md).
func profileFor(name string) (profileSettings, error) {
	switch name {
	case "":
		return profileSettings{}, nil
	case ProfileServer:
		return profileSettings{
			runtimeEnv: []string{"LENSDOCK_CHROMIUM_SANDBOX=true"},
			composeEnv: []string{
				"LENSDOCK_REQUIRE_PAIRING=1",
				"LENSDOCK_RUNTIME_MEMORY_LIMIT=4g",
				"LENSDOCK_RUNTIME_CPUS=2",
				"LENSDOCK_RUNTIME_PIDS_LIMIT=2048",
				"LENSDOCK_GATEWAY_MEMORY_LIMIT=1g",
				"LENSDOCK_GATEWAY_CPUS=1",
				"LENSDOCK_GATEWAY_PIDS_LIMIT=512",
			},
		}, nil
	}
	return profileSettings{}, optionErrorf("unknown profile %q: the only profile is %q", name, ProfileServer)
}

func (s profileSettings) runtimeEnvText() string { return linesText(s.runtimeEnv) }
func (s profileSettings) composeEnvText() string { return linesText(s.composeEnv) }

func linesText(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// checkRuntimeEnv verifies that an existing runtime.env already holds the
// profile's lines. An existing file is never rewritten, so a profile that cannot
// be applied to it is an error instead of a silent downgrade.
func (s profileSettings) checkRuntimeEnv(existing string) error {
	for _, required := range s.runtimeEnv {
		key, _, _ := strings.Cut(required, "=")
		count := 0
		for _, line := range strings.Split(existing, "\n") {
			line = strings.TrimSpace(line)
			// Refuse duplicate or alternate assignments rather than accepting a
			// matching line that a later env-file assignment could override.
			assignment := strings.TrimSpace(strings.TrimPrefix(line, "export "))
			candidate, _, _ := strings.Cut(assignment, "=")
			if strings.TrimSpace(candidate) != key {
				continue
			}
			count++
			if line != required || count > 1 {
				return optionErrorf("runtime.env must contain exactly one %q assignment for profile %s", required, ProfileServer)
			}
		}
		if count != 1 {
			return optionErrorf("runtime.env already exists without %q; add the line to it or choose a new state directory (profile %s)", required, ProfileServer)
		}
	}
	return nil
}
