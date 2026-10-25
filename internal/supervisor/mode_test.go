package supervisor

import (
	"strings"
	"testing"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
)

func TestLoadConfigAgentModeAndPolicyDir(t *testing.T) {
	const digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	load := func(extra map[string]string) (Config, error) {
		env := map[string]string{EnvOperatorTokenSHA256: digest}
		for k, v := range extra {
			env[k] = v
		}
		return LoadConfig(envMap(env))
	}

	cfg, err := load(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AgentMode != apiwire.AgentModeAGENT || cfg.PolicyDir != DefaultPolicyDir {
		t.Fatalf("defaults = %q %q", cfg.AgentMode, cfg.PolicyDir)
	}
	for in, want := range map[string]string{
		"NOVNC": apiwire.AgentModeNOVNC, "novnc": apiwire.AgentModeNOVNC, " NoVnc ": apiwire.AgentModeNOVNC,
		"AGENT": apiwire.AgentModeAGENT, "agent": apiwire.AgentModeAGENT,
	} {
		cfg, err := load(map[string]string{EnvMode: in})
		if err != nil || cfg.AgentMode != want {
			t.Errorf("%s=%q: mode %q err %v, want %q", EnvMode, in, cfg.AgentMode, err, want)
		}
	}
	for _, bad := range []string{"BROWSER", "true", "NOVNC,AGENT"} {
		if _, err := load(map[string]string{EnvMode: bad}); err == nil || !strings.Contains(err.Error(), EnvMode) {
			t.Errorf("%s=%q: err = %v, want an error naming the variable", EnvMode, bad, err)
		}
	}

	cfg, err = load(map[string]string{EnvPolicyDir: "/data/other"})
	if err != nil || cfg.PolicyDir != "/data/other" {
		t.Fatalf("custom dir = %q, %v", cfg.PolicyDir, err)
	}
	for _, bad := range []string{"relative/dir", "/data/../etc", "/data/runtime/", "/data//runtime"} {
		if _, err := load(map[string]string{EnvPolicyDir: bad}); err == nil || !strings.Contains(err.Error(), EnvPolicyDir) {
			t.Errorf("%s=%q: err = %v, want an error naming the variable", EnvPolicyDir, bad, err)
		}
	}
}
