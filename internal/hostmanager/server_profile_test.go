package hostmanager

import (
	"strings"
	"testing"
)

func TestServerPolicyDefaultsReachAuthorizedResourceOverride(t *testing.T) {
	policy := ServerPolicy()
	if err := validatePolicy(policy); err != nil {
		t.Fatal(err)
	}
	env := newEnv(t, policy)
	p := env.authorize()
	cfg, err := LoadConfig(env.cfg.Root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Policy != policy || p.Limits != policy.ProjectLimits {
		t.Fatal("server policy was not persisted or inherited during authorization")
	}
	for _, block := range []string{
		"  runtime:\n    mem_limit: 4294967296\n    memswap_limit: 4294967296\n    cpus: 2.000000000\n",
		"  gateway:\n    mem_limit: 1073741824\n    memswap_limit: 1073741824\n    cpus: 1.000000000\n",
	} {
		if !strings.Contains(resourceProfile(cfg, p), block) {
			t.Fatalf("server defaults lost from final resource override: %q", block)
		}
	}
	// An explicit later unlimited authorization must not regain profile caps.
	p.Limits = Limits{}
	for _, svc := range []string{"runtime", "gateway"} {
		block := "  " + svc + ":\n    mem_limit: 0\n    memswap_limit: 0\n    cpus: 0\n"
		if !strings.Contains(resourceProfile(cfg, p), block) {
			t.Fatal("explicit zero was replaced by a profile default")
		}
	}
}
