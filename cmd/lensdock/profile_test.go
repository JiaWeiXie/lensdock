package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JiaWeiXie/lensdock/internal/hostmanager"
)

func TestProductCLIProfileAndLocalEntryPersistence(t *testing.T) {
	bundle := writeBundle(t, "1.0.0")
	for _, local := range []bool{false, true} {
		state := filepath.Join(t.TempDir(), "state")
		args := []string{"admin", "product-init", "--state-dir", state, "--bundle-dir", bundle, "--profile", "server"}
		if local {
			args = append(args, "--local-auto-entry")
		}
		r := runCLIWith(t, filepath.Join(bundle, "lensdock"), nil, nil, args...)
		if r.code != 0 {
			t.Fatalf("init failed: %s", r.stderr)
		}
		data, err := os.ReadFile(filepath.Join(state, "product", "gateway.json"))
		if err != nil {
			t.Fatal(err)
		}
		var cfg struct{ LocalAutoEntry bool }
		if err := json.Unmarshal(data, &cfg); err != nil || cfg.LocalAutoEntry != local {
			t.Fatalf("local entry flag not persisted: %v", err)
		}
		env, err := os.ReadFile(filepath.Join(state, "product", "compose.env"))
		if err != nil || !strings.Contains(string(env), "LENSDOCK_REQUIRE_PAIRING=1\n") {
			t.Fatalf("server pairing requirement not persisted: %v", err)
		}
	}
}

func TestManagerCLIProfileAndLocalEntryPersistence(t *testing.T) {
	base, err := os.MkdirTemp("/tmp", "ldprofile-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	// This fixture never invokes a real Docker client or engine.
	writeFile(t, filepath.Join(base, "docker"), `#!/bin/sh
if [ "$1" = context ] && [ "$2" = show ]; then
  printf 'local\n'
elif [ "$1" = context ] && [ "$2" = inspect ]; then
  printf '[{"Endpoints":{"docker":{"Host":"unix:///tmp/lensdock-fake-engine.sock"}}}]\n'
else
  printf '{"ID":"profile-test-engine","MemTotal":25769803776,"NCPU":16,"OSType":"linux"}\n'
fi
`, 0700)
	t.Setenv("PATH", base)
	bundle := writeBundle(t, "1.0.0")
	for _, tc := range []struct {
		name, profile  string
		local, pairing bool
	}{
		{"default", "", false, false},
		{"server-local", "server", true, false},
		{"server-pairing", "server", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(base, tc.name)
			args := []string{"admin", "manager", "init", "--bundle-dir", bundle}
			if tc.profile != "" {
				args = append(args, "--profile", tc.profile)
			}
			if tc.local {
				args = append(args, "--local-auto-entry")
			}
			if tc.pairing {
				args = append(args, "--require-pairing")
			}
			r := runCLIWith(t, filepath.Join(bundle, "lensdock"), nil, map[string]string{"LENSDOCK_MANAGER_DIR": root}, args...)
			if r.code != 0 {
				t.Fatalf("manager init failed: %s", r.stderr)
			}
			cfg, err := hostmanager.LoadConfig(root)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Profile != tc.profile || cfg.LocalAutoEntry != tc.local || cfg.RequirePairing != tc.pairing {
				t.Error("profile and pairing flags did not survive manager configuration reload")
			}
		})
	}
}
