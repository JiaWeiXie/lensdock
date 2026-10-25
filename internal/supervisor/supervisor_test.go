package supervisor

import (
	"context"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/netx"
)

func TestChromiumArgvKeepsSandboxAndLoopbackCDP(t *testing.T) {
	forbiddenExact := []string{
		"--no-sandbox", "--disable-seccomp-filter-sandbox", "--no-zygote",
		"--password-store=basic", "--disable-gpu", "--single-process",
		"--disable-namespace-sandbox",
	}
	argv := ChromiumArgv()
	if len(argv) == 0 {
		t.Fatal("empty argv")
	}
	var debugAddress string
	for _, a := range argv[1:] {
		for _, bad := range forbiddenExact {
			if a == bad {
				t.Errorf("forbidden flag %s", a)
			}
		}
		if strings.HasPrefix(a, "--remote-allow-origins") {
			t.Errorf("forbidden flag %s", a)
		}
		if v, ok := strings.CutPrefix(a, "--remote-debugging-address="); ok {
			debugAddress = v
		}
	}
	if !netx.IsLoopbackIP(debugAddress) {
		t.Errorf("remote debugging address %q is not loopback", debugAddress)
	}
}

func TestBackoffSequenceAndReset(t *testing.T) {
	now := time.Unix(1000, 0)
	b := newBackoff(func() time.Time { return now })

	run := func(ran time.Duration) time.Duration {
		b.Started()
		now = now.Add(ran)
		return b.Exited()
	}

	want := []time.Duration{1, 2, 4, 8, 16, 30, 30}
	for i, w := range want {
		if got := run(time.Second); got != w*time.Second {
			t.Fatalf("crash %d: delay = %v, want %v", i+1, got, w*time.Second)
		}
	}
	if got := run(59 * time.Second); got != 30*time.Second {
		t.Fatalf("59s run must not reset backoff, got %v", got)
	}
	if got := run(60 * time.Second); got != time.Second {
		t.Fatalf("stable run must reset backoff, got %v", got)
	}
	if got := run(time.Second); got != 2*time.Second {
		t.Fatalf("delay after reset = %v, want 2s", got)
	}
}

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadConfig(t *testing.T) {
	const digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	valid := func() map[string]string {
		return map[string]string{EnvOperatorTokenSHA256: digest}
	}

	t.Run("overrides", func(t *testing.T) {
		env := valid()
		env[EnvWorkspaceID] = "ws-1"
		env[EnvListen] = "127.0.0.1:9000"
		env[EnvClipboardTTL] = "5m"
		cfg, err := LoadConfig(envMap(env))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.WorkspaceID != "ws-1" || cfg.Listen != "127.0.0.1:9000" || cfg.ClipboardTTL != 5*time.Minute {
			t.Fatalf("cfg = %+v", cfg)
		}
		if cfg.OperatorDigest.Hex() != digest {
			t.Fatalf("digest = %s", cfg.OperatorDigest.Hex())
		}
	})

	invalid := []struct {
		name   string
		mutate func(map[string]string)
		want   string
	}{
		{"missing digest", func(m map[string]string) { delete(m, EnvOperatorTokenSHA256) }, EnvOperatorTokenSHA256},
		{"short digest", func(m map[string]string) { m[EnvOperatorTokenSHA256] = "abcd" }, EnvOperatorTokenSHA256},
		{"non-hex digest", func(m map[string]string) { m[EnvOperatorTokenSHA256] = strings.Repeat("z", 64) }, EnvOperatorTokenSHA256},
		{"workspace with slash", func(m map[string]string) { m[EnvWorkspaceID] = "a/b" }, EnvWorkspaceID},
		{"listen without port", func(m map[string]string) { m[EnvListen] = "localhost" }, EnvListen},
		{"listen port zero", func(m map[string]string) { m[EnvListen] = ":0" }, EnvListen},
		{"ttl not a duration", func(m map[string]string) { m[EnvClipboardTTL] = "soon" }, EnvClipboardTTL},
		{"ttl zero", func(m map[string]string) { m[EnvClipboardTTL] = "0s" }, EnvClipboardTTL},
		{"ttl too long", func(m map[string]string) { m[EnvClipboardTTL] = "24h" }, EnvClipboardTTL},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			env := valid()
			tt.mutate(env)
			_, err := LoadConfig(envMap(env))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want mention of %s", err, tt.want)
			}
		})
	}

	t.Run("all problems reported without echoing the digest", func(t *testing.T) {
		secretish := "not-a-digest-but-sensitive"
		_, err := LoadConfig(envMap(map[string]string{EnvOperatorTokenSHA256: secretish, EnvWorkspaceID: "a/b"}))
		if err == nil {
			t.Fatal("expected error")
		}
		if strings.Contains(err.Error(), secretish) {
			t.Fatal("error echoes the digest value")
		}
		if !strings.Contains(err.Error(), EnvOperatorTokenSHA256) || !strings.Contains(err.Error(), EnvWorkspaceID) {
			t.Fatalf("error does not report both problems: %v", err)
		}
	})
}

func TestVerifyDir(t *testing.T) {
	uid := uint32(os.Getuid())
	base := t.TempDir()
	mk := func(name string, mode fs.FileMode) string {
		p := filepath.Join(base, name)
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}

	if err := verifyDir(mk("private", 0o700), uid); err != nil {
		t.Errorf("private dir rejected: %v", err)
	}
	if err := verifyDir(mk("open", 0o755), uid); err == nil {
		t.Error("group/other readable dir accepted")
	}
	if err := verifyDir(mk("wrongowner", 0o700), uid+1); err == nil {
		t.Error("wrong owner accepted")
	}

	target := mk("target", 0o700)
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := verifyDir(link, uid); err == nil {
		t.Error("symlink accepted")
	}

	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyDir(file, uid); err == nil {
		t.Error("regular file accepted")
	}
	if err := verifyDir(filepath.Join(base, "missing"), uid); err == nil {
		t.Error("missing dir accepted")
	}
}

func TestParseSignalArgs(t *testing.T) {
	good := []struct {
		args []string
		sig  syscall.Signal
		pgid int
	}{
		{[]string{"15"}, syscall.SIGTERM, 0},
		{[]string{"9"}, syscall.SIGKILL, 0},
		{[]string{"9", "4242"}, syscall.SIGKILL, 4242},
	}
	for _, g := range good {
		sig, pgid, err := parseSignalArgs(g.args)
		if err != nil || sig != g.sig || pgid != g.pgid {
			t.Errorf("parseSignalArgs(%v) = %v, %d, %v", g.args, sig, pgid, err)
		}
	}
	for _, bad := range [][]string{
		nil, {"0"}, {"1"}, {"18"}, {"x"}, {"9", "0"}, {"9", "1"}, {"9", "-5"}, {"9", "abc"}, {"9", "10", "11"},
	} {
		if _, _, err := parseSignalArgs(bad); err == nil {
			t.Errorf("parseSignalArgs(%v) succeeded", bad)
		}
	}
}

func TestParseManifestSkipsMalformedLines(t *testing.T) {
	got := parseManifest([]byte("chromium\t156.0.1\tarm64\nbroken line\n\t1.0\tamd64\nxvfb\t2:21.1\tarm64\nonlyname\t\n"))
	if len(got) != 2 || got["chromium"] != "156.0.1" || got["xvfb"] != "2:21.1" {
		t.Fatalf("manifest = %v", got)
	}
}

func TestRunHealthcheck(t *testing.T) {
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
	}))
	defer srv.Close()

	if err := RunHealthcheck(context.Background(), srv.URL+"/readyz"); err != nil {
		t.Fatalf("ready server: %v", err)
	}
	status = http.StatusServiceUnavailable
	if err := RunHealthcheck(context.Background(), srv.URL+"/readyz"); err == nil {
		t.Fatal("503 treated as healthy")
	}
	status = http.StatusFound
	if err := RunHealthcheck(context.Background(), srv.URL+"/readyz"); err == nil {
		t.Fatal("redirect treated as healthy")
	}
	for _, u := range []string{"http://example.com/readyz", "https://127.0.0.1/readyz", "://bad"} {
		if err := RunHealthcheck(context.Background(), u); err == nil {
			t.Errorf("RunHealthcheck(%q) succeeded", u)
		}
	}
	srv.Close()
	if err := RunHealthcheck(context.Background(), srv.URL+"/readyz"); err == nil {
		t.Fatal("closed server treated as healthy")
	}
}
