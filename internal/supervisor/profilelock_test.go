package supervisor

import (
	"bufio"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type helperRun struct {
	stdin  *io.PipeWriter
	lines  chan string
	result chan error
}

// startHelper runs RunProfileLockHelper on dir. It returns after the helper
// printed its first line or finished.
func startHelper(t *testing.T, dir string) *helperRun {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	h := &helperRun{stdin: inW, lines: make(chan string, 8), result: make(chan error, 1)}
	go func() {
		err := RunProfileLockHelper(inR, outW, dir)
		_ = outW.Close()
		h.result <- err
	}()
	go func() {
		sc := bufio.NewScanner(outR)
		for sc.Scan() {
			h.lines <- sc.Text()
		}
		close(h.lines)
	}()
	t.Cleanup(func() { _ = inW.Close() })
	return h
}

func (h *helperRun) line(t *testing.T) string {
	t.Helper()
	select {
	case l, ok := <-h.lines:
		if !ok {
			t.Fatal("helper output ended")
		}
		return l
	case <-time.After(5 * time.Second):
		t.Fatal("helper printed nothing")
	}
	return ""
}

func (h *helperRun) wait(t *testing.T) error {
	t.Helper()
	select {
	case err := <-h.result:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("helper did not finish")
	}
	return nil
}

func TestProfileLockIsExclusiveAndNeverDeletedOrStolen(t *testing.T) {
	dir := t.TempDir()
	first := startHelper(t, dir)
	if got := first.line(t); got != "locked" {
		t.Fatalf("first helper said %q, want locked", got)
	}

	// A second runtime on the same profile is refused outright.
	second := startHelper(t, dir)
	if err := second.wait(t); !errors.Is(err, ErrProfileLocked) {
		t.Fatalf("second helper = %v, want ErrProfileLocked", err)
	}
	if _, err := os.Stat(filepath.Join(dir, profileLockFile)); err != nil {
		t.Fatalf("the lock file must survive a refused attempt: %v", err)
	}
	// It stays refused for as long as the first holder lives.
	if err := startHelper(t, dir).wait(t); !errors.Is(err, ErrProfileLocked) {
		t.Fatalf("third helper = %v, want ErrProfileLocked", err)
	}

	// Ending the holder (EOF on stdin, as when the supervisor dies) releases it.
	_ = first.stdin.Close()
	if err := first.wait(t); err != nil {
		t.Fatalf("first helper = %v", err)
	}
	again := startHelper(t, dir)
	if got := again.line(t); got != "locked" {
		t.Fatalf("helper after release said %q, want locked", got)
	}
}

func TestProfileLockRemovesOnlyStaleSingletonFilesWhileHoldingTheLock(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink("old-container-4242", filepath.Join(dir, "SingletonLock")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/tmp/.org.chromium.Chromium.gone/SingletonSocket", filepath.Join(dir, "SingletonSocket")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SingletonCookie"), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "Default"), 0o700); err != nil {
		t.Fatal(err)
	}
	prefs := filepath.Join(dir, "Default", "Preferences")
	if err := os.WriteFile(prefs, []byte(`{"profile":{"exit_type":"Crashed"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	h := startHelper(t, dir)
	if got := h.line(t); got != "locked" {
		t.Fatalf("helper said %q", got)
	}
	for _, n := range singletonFiles {
		if _, err := os.Lstat(filepath.Join(dir, n)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still exists after taking the lock (%v)", n, err)
		}
	}
	if b, err := os.ReadFile(prefs); err != nil || !strings.Contains(string(b), "Crashed") {
		t.Fatalf("profile data was touched: %q, %v", b, err)
	}

	// A "clean" request (before a Chromium relaunch) removes new leftovers.
	if err := os.Symlink("host-1", filepath.Join(dir, "SingletonLock")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(h.stdin, "clean\n"); err != nil {
		t.Fatal(err)
	}
	if got := h.line(t); got != "clean" {
		t.Fatalf("helper said %q, want clean", got)
	}
	if _, err := os.Lstat(filepath.Join(dir, "SingletonLock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("SingletonLock survived a clean request (%v)", err)
	}
	// Unknown commands are ignored, not executed.
	if _, err := io.WriteString(h.stdin, "rm -rf /\n"); err != nil {
		t.Fatal(err)
	}
}

func TestProfileLockRefusesAContestedDirWithoutTouchingItsSingletons(t *testing.T) {
	dir := t.TempDir()
	holder := startHelper(t, dir)
	if got := holder.line(t); got != "locked" {
		t.Fatal(got)
	}
	// Another live runtime's Chromium owns these now.
	if err := os.Symlink("live-host-1", filepath.Join(dir, "SingletonLock")); err != nil {
		t.Fatal(err)
	}
	if err := startHelper(t, dir).wait(t); !errors.Is(err, ErrProfileLocked) {
		t.Fatalf("contender = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "SingletonLock")); err != nil {
		t.Fatalf("a refused contender must not remove SingletonLock: %v", err)
	}
}

func TestLineSinkSplitsLinesAndDropsOverflow(t *testing.T) {
	ch := make(chan string, 2)
	s := &lineSink{ch: ch}
	if _, err := io.WriteString(s, "locked\nclea"); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(s, "n\nthird\n"); err != nil {
		t.Fatal(err)
	}
	if got := <-ch; got != "locked" {
		t.Fatalf("line 1 = %q", got)
	}
	if got := <-ch; got != "clean" {
		t.Fatalf("line 2 = %q", got)
	}
	select {
	case got := <-ch:
		t.Fatalf("a full channel must drop lines, got %q", got)
	default:
	}
}

func TestLoadConfigTLSAndTransferSettings(t *testing.T) {
	const digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	base := func() map[string]string { return map[string]string{EnvOperatorTokenSHA256: digest} }
	tlsEnv := func(m map[string]string) {
		m[EnvTLSCertFile] = "/run/lensdock/runtime.crt"
		m[EnvTLSKeyFile] = "/run/lensdock/runtime.key"
		m[EnvTLSClientCAFile] = "/run/lensdock/ca.crt"
	}

	t.Run("plain by default", func(t *testing.T) {
		cfg, err := LoadConfig(envMap(base()))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.TLS.Enabled() || cfg.HealthListen != "127.0.0.1:8081" {
			t.Fatalf("cfg = %+v", cfg)
		}
	})
	t.Run("mTLS with names and limits", func(t *testing.T) {
		env := base()
		tlsEnv(env)
		env[EnvTLSClientNames] = "gateway, spiffe://lensdock/gateway"
		env[EnvHealthListen] = "127.0.0.1:9100"
		env[EnvFileQuotaBytes] = "1073741824"
		env[EnvFileMaxBytes] = "1048576"
		env[EnvFileMaxFiles] = "50"
		cfg, err := LoadConfig(envMap(env))
		if err != nil {
			t.Fatal(err)
		}
		if !cfg.TLS.Enabled() || len(cfg.TLS.ClientNames) != 2 || cfg.TLS.ClientNames[1] != "spiffe://lensdock/gateway" {
			t.Fatalf("tls = %+v", cfg.TLS)
		}
		if cfg.HealthListen != "127.0.0.1:9100" || cfg.FileQuotaBytes != 1<<30 || cfg.FileMaxBytes != 1<<20 || cfg.FileMaxFiles != 50 {
			t.Fatalf("cfg = %+v", cfg)
		}
	})

	invalid := []struct {
		name   string
		mutate func(map[string]string)
		want   string
	}{
		{"cert without key and CA", func(m map[string]string) { m[EnvTLSCertFile] = "/x/runtime.crt" }, "must be set together"},
		{"key and CA without cert", func(m map[string]string) {
			m[EnvTLSKeyFile] = "/x/k"
			m[EnvTLSClientCAFile] = "/x/ca"
		}, "must be set together"},
		{"relative cert path", func(m map[string]string) {
			tlsEnv(m)
			m[EnvTLSCertFile] = "runtime.crt"
		}, EnvTLSCertFile},
		{"names without TLS", func(m map[string]string) { m[EnvTLSClientNames] = "gateway" }, EnvTLSClientNames},
		{"empty name", func(m map[string]string) {
			tlsEnv(m)
			m[EnvTLSClientNames] = "gateway,,x"
		}, EnvTLSClientNames},
		{"health listen on all interfaces", func(m map[string]string) { m[EnvHealthListen] = "0.0.0.0:8081" }, EnvHealthListen},
		{"health listen without port", func(m map[string]string) { m[EnvHealthListen] = "127.0.0.1" }, EnvHealthListen},
		{"quota not a number", func(m map[string]string) { m[EnvFileQuotaBytes] = "lots" }, EnvFileQuotaBytes},
		{"max file zero", func(m map[string]string) { m[EnvFileMaxBytes] = "0" }, EnvFileMaxBytes},
		{"max file above quota", func(m map[string]string) {
			m[EnvFileQuotaBytes] = "100"
			m[EnvFileMaxBytes] = "200"
		}, EnvFileMaxBytes},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			env := base()
			tt.mutate(env)
			_, err := LoadConfig(envMap(env))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want mention of %s", err, tt.want)
			}
		})
	}
}
