package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFeedbackSizeLimitsAreOptInAndValidated(t *testing.T) {
	tok := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tok, []byte("ldt_test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := []string{"feedback", "list", "--workspace", "ws", "--token-file", tok, "--api", "http://127.0.0.1:1"}

	// Malformed environment defaults are usage errors that name the variable.
	for _, name := range []string{envMaxArtifactBytes, envMaxBundleBytes, envMaxJSONBytes} {
		for _, bad := range []string{"many", "-1", "1.5"} {
			r := runCLI(t, context.Background(), "", map[string]string{name: bad}, base...)
			if r.code != exitUsage || !strings.Contains(r.stderr, name) {
				t.Errorf("%s=%q: exit %d, stderr %q", name, bad, r.code, r.stderr)
			}
		}
	}
	// Negative flag values are refused before any request.
	for _, flag := range []string{"--max-artifact-bytes", "--max-bundle-bytes", "--max-json-bytes"} {
		r := runCLI(t, context.Background(), "", nil, append(append([]string{}, base...), flag, "-1")...)
		if r.code != exitUsage {
			t.Errorf("%s -1: exit %d, stderr %q", flag, r.code, r.stderr)
		}
	}
}

func TestMCPInlineImageLimitIsValidated(t *testing.T) {
	tok := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tok, []byte("ldt_test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"mcp", "serve", "--workspace", "ws", "--token-file", tok, "--api", "http://127.0.0.1:1"}

	r := runCLI(t, context.Background(), "", map[string]string{envMaxInlineImageBytes: "lots"}, args...)
	if r.code != exitUsage || !strings.Contains(r.stderr, envMaxInlineImageBytes) {
		t.Errorf("malformed environment default: exit %d, stderr %q", r.code, r.stderr)
	}
	if r := runCLI(t, context.Background(), "", nil, append(append([]string{}, args...), "--max-inline-image-bytes", "-1")...); r.code == 0 {
		t.Error("a negative inline image limit was accepted")
	}
}

func TestFeedbackDeadlinesAreOptInAndValidated(t *testing.T) {
	tok := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tok, []byte("ldt_test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := []string{"feedback", "list", "--workspace", "ws", "--token-file", tok, "--api", "http://127.0.0.1:1"}
	for _, name := range []string{envJSONTimeout, envArtifactTimeout} {
		for _, bad := range []string{"soon", "-5s", "10"} {
			r := runCLI(t, context.Background(), "", map[string]string{name: bad}, base...)
			if r.code != exitUsage || !strings.Contains(r.stderr, name) {
				t.Errorf("%s=%q: exit %d, stderr %q", name, bad, r.code, r.stderr)
			}
		}
	}
	for _, flag := range []string{"--json-timeout", "--artifact-timeout"} {
		r := runCLI(t, context.Background(), "", nil, append(append([]string{}, base...), flag, "-1s")...)
		if r.code != exitUsage {
			t.Errorf("%s -1s: exit %d, stderr %q", flag, r.code, r.stderr)
		}
	}
}
