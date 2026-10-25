package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProductModeFlagOverridesEnvironment(t *testing.T) {
	for _, tc := range []struct{ env, flag, want string }{
		{"NOVNC", "AGENT", "AGENT"},
		{"novnc", "", "NOVNC"},
		{"", "novnc", "NOVNC"},
	} {
		dir := filepath.Join(t.TempDir(), "deployment")
		args := []string{"admin", "product-init", "--state-dir", dir}
		if tc.flag != "" {
			args = append(args, "--mode", tc.flag)
		}
		r := runCLI(t, context.Background(), "", map[string]string{"LENSDOCK_MODE": tc.env}, args...)
		if r.code != 0 {
			t.Fatalf("init failed: %s", r.stderr)
		}
		env, err := os.ReadFile(filepath.Join(dir, "product", "compose.env"))
		if err != nil || !strings.Contains(string(env), "LENSDOCK_MODE="+tc.want+"\n") {
			t.Fatalf("initial mode not persisted: %v", err)
		}
	}
}

func TestProductRejectsInvalidModeBeforeWritingCredentials(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "deployment")
	r := runCLI(t, context.Background(), "", nil, "admin", "product-init", "--state-dir", dir, "--mode", "AUTO")
	if r.code != exitUsage {
		t.Fatalf("invalid mode exit: %d", r.code)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("invalid mode wrote deployment files: %v", err)
	}
}
