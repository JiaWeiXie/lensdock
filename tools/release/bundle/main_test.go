package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const testVersion = "9.8.7"

// tinyBuild holds <goos>-<goarch>/lensdock for every release target. They are
// real cross-compiled Go binaries built the way package.sh builds the CLI, so
// verify is exercised against genuine ELF and Mach-O files.
var tinyBuild string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "bundle-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := 1
	if err := buildTiny(dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
	} else {
		tinyBuild = filepath.Join(dir, "build")
		code = m.Run()
	}
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func writeFiles(root string, files [][2]string) error {
	for _, f := range files {
		p := filepath.Join(root, filepath.FromSlash(f[0]))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(f[1]), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func buildTiny(dir string) error {
	src := filepath.Join(dir, "src")
	err := writeFiles(src, [][2]string{
		{"go.mod", "module github.com/JiaWeiXie/lensdock\n\ngo 1.21\n"},
		{"cmd/lensdock/main.go", "package main\n\nvar version string\n\nfunc main() { println(version) }\n"},
	})
	if err != nil {
		return err
	}
	for _, t := range targets {
		dest := filepath.Join(dir, "build", t.os+"-"+t.arch, binaryName)
		cmd := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-trimpath",
			"-ldflags", "-s -w -X main.version="+testVersion, "-o", dest, "./cmd/lensdock")
		cmd.Dir = src
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+t.os, "GOARCH="+t.arch, "GOTOOLCHAIN=local", "GOFLAGS=")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("build %s/%s: %w\n%s", t.os, t.arch, err, out)
		}
	}
	return nil
}

func packInto(t *testing.T, buildDir string) string {
	t.Helper()
	root := t.TempDir()
	files := [][2]string{{"install.sh", "#!/usr/bin/env bash\n"}}
	for _, name := range requiredFiles {
		files = append(files, [2]string{name, "fixture for " + name + "\n"})
	}
	err := writeFiles(root, files)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "dist")
	err = pack(packOptions{
		version: testVersion, sourceRoot: root, buildDir: buildDir, out: out,
		installer: filepath.Join(root, "install.sh"), epoch: time.Unix(1700000000, 0).UTC(),
	})
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	return out
}

func TestPackThenVerify(t *testing.T) {
	out := packInto(t, tinyBuild)
	if err := verify(out, testVersion); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if err := verify(out, "9.8.8"); err == nil {
		t.Fatal("verify accepted a release directory of another version")
	}
}

func TestVerifyRejectsTamperedChecksum(t *testing.T) {
	out := packInto(t, tinyBuild)
	sums := filepath.Join(out, sumsName)
	b, err := os.ReadFile(sums)
	if err != nil {
		t.Fatal(err)
	}
	if b[0] == '0' {
		b[0] = '1'
	} else {
		b[0] = '0'
	}
	if err := os.WriteFile(sums, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verify(out, testVersion); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("verify = %v, want a checksum mismatch", err)
	}
}

func TestVerifyRejectsWrongPlatformHelper(t *testing.T) {
	wrong := t.TempDir()
	for _, tg := range targets {
		src := filepath.Join(tinyBuild, tg.os+"-"+tg.arch, binaryName)
		if tg == (target{"linux", "arm64"}) {
			src = filepath.Join(tinyBuild, "darwin-arm64", binaryName) // a Mach-O file in the linux/arm64 slot
		}
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		dest := filepath.Join(wrong, tg.os+"-"+tg.arch, binaryName)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dest, data, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	err := verify(packInto(t, wrong), testVersion)
	if err == nil || !strings.Contains(err.Error(), "ELF") {
		t.Fatalf("verify = %v, want an ELF error", err)
	}
}
