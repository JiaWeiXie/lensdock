package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// installerFixture is a machine-local release: archives and SHA256SUMS in one directory.
type installerFixture struct {
	t       *testing.T
	release string
	home    string
	lib     string
	bin     string
	state   string
}

func newInstallerFixture(t *testing.T) *installerFixture {
	t.Helper()
	if (runtime.GOOS != "linux" && runtime.GOOS != "darwin") || (runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64") {
		t.Skipf("the installer supports linux and darwin on amd64 and arm64, not %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	for _, tool := range []string{"bash", "tar", "awk"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not available", tool)
		}
	}
	root := t.TempDir()
	return &installerFixture{
		t: t, release: filepath.Join(root, "release"), home: filepath.Join(root, "home"),
		lib: filepath.Join(root, "lib"), bin: filepath.Join(root, "bin"), state: filepath.Join(root, "state"),
	}
}

// archive writes lensdock_<version>_<os>_<arch>.tar.gz and lists it in SHA256SUMS.
// extra may add entries to the archive.
func (f *installerFixture) archive(version string, extra func(*tar.Writer)) string {
	f.t.Helper()
	if err := os.MkdirAll(f.release, 0o755); err != nil {
		f.t.Fatal(err)
	}
	var raw bytes.Buffer
	gz := gzip.NewWriter(&raw)
	tw := tar.NewWriter(gz)
	add := func(name, body string, mode int64) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			f.t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			f.t.Fatal(err)
		}
	}
	add("lensdock", "#!/bin/sh\necho \"lensdock "+version+"\"\n", 0o755)
	add("VERSION", version+"\n", 0o644)
	add("docker/compose.yaml", "services: {}\n", 0o644)
	add("docker/browser/seccomp/chromium.json", "{}\n", 0o644)
	add("bin/linux-amd64/lensdock", "amd64 helper\n", 0o755)
	add("bin/linux-arm64/lensdock", "arm64 helper\n", 0o755)
	if extra != nil {
		extra(tw)
	}
	if err := tw.Close(); err != nil {
		f.t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		f.t.Fatal(err)
	}
	name := fmt.Sprintf("lensdock_%s_%s_%s.tar.gz", version, runtime.GOOS, runtime.GOARCH)
	path := filepath.Join(f.release, name)
	if err := os.WriteFile(path, raw.Bytes(), 0o644); err != nil {
		f.t.Fatal(err)
	}
	sum := sha256.Sum256(raw.Bytes())
	sums, _ := os.ReadFile(filepath.Join(f.release, "SHA256SUMS"))
	line := hex.EncodeToString(sum[:]) + "  " + name + "\n"
	if err := os.WriteFile(filepath.Join(f.release, "SHA256SUMS"), append(sums, line...), 0o644); err != nil {
		f.t.Fatal(err)
	}
	return path
}

// run executes install.sh with a clean environment.
func (f *installerFixture) run(archive string, args ...string) (string, error) {
	f.t.Helper()
	if err := os.MkdirAll(f.home, 0o755); err != nil {
		f.t.Fatal(err)
	}
	cmd := exec.Command("bash", append([]string{filepath.Join("..", "..", "install.sh")}, args...)...)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + f.home, "TMPDIR=" + f.t.TempDir(),
		"LENSDOCK_INSTALL_ARCHIVE=" + archive, "LENSDOCK_INSTALL_DIR=" + f.lib,
		"LENSDOCK_BIN_DIR=" + f.bin, "LENSDOCK_STATE_DIR=" + f.state,
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestInstallerInstallsVersionsSideBySideAndNeverOverwrites(t *testing.T) {
	f := newInstallerFixture(t)
	v1 := f.archive("1.2.3", nil)
	if out, err := f.run(v1, "--install-only"); err != nil {
		t.Fatalf("install failed: %v\n%s", err, out)
	}
	link := filepath.Join(f.bin, "lensdock")
	if target, err := os.Readlink(link); err != nil || target != filepath.Join(f.lib, "1.2.3", "lensdock") {
		t.Fatalf("link = %q, %v", target, err)
	}
	for _, rel := range []string{"lensdock", "VERSION", "docker/compose.yaml", "docker/browser/seccomp/chromium.json", "bin/linux-amd64/lensdock", "bin/linux-arm64/lensdock"} {
		if _, err := os.Stat(filepath.Join(f.lib, "1.2.3", filepath.FromSlash(rel))); err != nil {
			t.Errorf("the bundle lost %s: %v", rel, err)
		}
	}
	if fi, err := os.Stat(filepath.Join(f.lib, "1.2.3", "bin", "linux-arm64", "lensdock")); err != nil || fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("helper is not executable: %v %v", err, fi)
	}
	if _, err := os.Stat(f.state); !os.IsNotExist(err) {
		t.Errorf("--install-only created state: %v", err)
	}
	if out, err := exec.Command(link, "version").CombinedOutput(); err != nil || !strings.Contains(string(out), "1.2.3") {
		t.Errorf("the linked CLI does not run: %v %q", err, out)
	}

	// Reinstalling the same version keeps the directory as it is.
	marker := filepath.Join(f.lib, "1.2.3", "local-note")
	if err := os.WriteFile(marker, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := f.run(v1, "--install-only"); err != nil || !strings.Contains(out, "already installed") {
		t.Fatalf("reinstall: %v\n%s", err, out)
	}
	if b, err := os.ReadFile(marker); err != nil || string(b) != "keep" {
		t.Fatalf("a reinstall changed the installed version: %v %q", err, b)
	}
	if entries, _ := filepath.Glob(filepath.Join(f.lib, ".stage.*")); len(entries) != 0 {
		t.Errorf("staging directories were left behind: %v", entries)
	}

	// A new version is added next to it and the link moves.
	v2 := f.archive("1.2.4", nil)
	if out, err := f.run(v2, "--install-only"); err != nil {
		t.Fatalf("upgrade failed: %v\n%s", err, out)
	}
	if target, _ := os.Readlink(link); target != filepath.Join(f.lib, "1.2.4", "lensdock") {
		t.Fatalf("link after upgrade = %q", target)
	}
	if b, err := os.ReadFile(marker); err != nil || string(b) != "keep" {
		t.Fatalf("an upgrade touched the previous version: %v %q", err, b)
	}

	// A file that is not the installer's link is never replaced.
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(link, []byte("mine"), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := f.run(v2, "--install-only"); err == nil || !strings.Contains(out, "not a link managed by this installer") {
		t.Fatalf("a foreign file was not refused: %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(link); string(b) != "mine" {
		t.Fatalf("the foreign file was replaced: %q", b)
	}
}

func TestInstallerRejectsAnUnverifiedOrUnsafeArchive(t *testing.T) {
	f := newInstallerFixture(t)
	good := f.archive("1.0.0", nil)
	sums := filepath.Join(f.release, "SHA256SUMS")
	raw, _ := os.ReadFile(sums)
	if err := os.WriteFile(sums, bytes.Replace(raw, raw[:8], []byte("00000000"), 1), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := f.run(good, "--install-only"); err == nil || !strings.Contains(out, "checksum") {
		t.Fatalf("a checksum mismatch was not refused: %v\n%s", err, out)
	}
	for _, p := range []string{filepath.Join(f.lib, "1.0.0"), filepath.Join(f.bin, "lensdock")} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("a refused install left %s: %v", p, err)
		}
	}

	link := newInstallerFixture(t)
	bad := link.archive("2.0.0", func(tw *tar.Writer) {
		if err := tw.WriteHeader(&tar.Header{Name: "escape", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd", Mode: 0o777}); err != nil {
			t.Fatal(err)
		}
	})
	if out, err := link.run(bad, "--install-only"); err == nil || !strings.Contains(out, "links or special files") {
		t.Fatalf("an archive with a symlink was not refused: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(link.lib, "2.0.0")); !os.IsNotExist(err) {
		t.Errorf("an unsafe archive was installed: %v", err)
	}
	traversal := newInstallerFixture(t)
	evil := traversal.archive("3.0.0", func(tw *tar.Writer) {
		body := "x"
		if err := tw.WriteHeader(&tar.Header{Name: "../outside", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(body))
	})
	if out, err := traversal.run(evil, "--install-only"); err == nil || !strings.Contains(out, "unsafe path") {
		t.Fatalf("a path traversal was not refused: %v\n%s", err, out)
	}
}

func TestInstallerLeavesAnExistingDeploymentUntouched(t *testing.T) {
	f := newInstallerFixture(t)
	archive := f.archive("1.2.3", nil)
	if err := os.MkdirAll(f.state, 0o700); err != nil {
		t.Fatal(err)
	}
	deployment := filepath.Join(f.state, "deployment.json")
	if err := os.WriteFile(deployment, []byte("{\"keep\":true}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Docker is not needed: an existing deployment is decided before any Docker check.
	out, err := f.run(archive)
	if err != nil {
		t.Fatalf("install over an existing deployment failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "will not be changed or started") {
		t.Errorf("the installer did not say it left the deployment alone:\n%s", out)
	}
	if b, _ := os.ReadFile(deployment); string(b) != "{\"keep\":true}\n" {
		t.Errorf("deployment.json changed: %q", b)
	}
	entries, _ := os.ReadDir(f.state)
	if len(entries) != 1 {
		t.Errorf("the installer added files to the existing state: %v", entries)
	}
}
