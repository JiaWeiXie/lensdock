package delivery

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// withSizes returns the served feedback JSON with the declared artifact sizes
// replaced (the digests are not checked while decoding).
func withSizes(t *testing.T, g *gateway, redacted, annotated int64) []byte {
	t.Helper()
	m := map[string]any{}
	if err := json.Unmarshal(g.feedbackJSON(), &m); err != nil {
		t.Fatal(err)
	}
	arts := m["artifacts"].([]any)
	arts[0].(map[string]any)["sizeBytes"] = redacted
	arts[1].(map[string]any)["sizeBytes"] = annotated
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestNoSizeLimitsByDefault(t *testing.T) {
	g, _ := newGateway(t)
	// 3 x 40 MiB declared: far above any former per-artifact or bundle cap.
	if _, err := decodeFeedback(withSizes(t, g, 40<<20, 40<<20), "fb1", 1, Limits{}); err != nil {
		t.Fatalf("large declared artifacts refused without limits: %v", err)
	}

	m := Manifest{SchemaVersion: SchemaVersion, FeedbackID: "fb1", Revision: 1, WorkspaceID: "ws"}
	for _, name := range bundleOrder {
		m.Files = append(m.Files, ManifestFile{Name: name, MimeType: bundleMIME[name], SHA256: strings.Repeat("a", 64), SizeBytes: 100 << 20})
	}
	if err := m.validate(Limits{}); err != nil {
		t.Fatalf("large manifest refused without limits: %v", err)
	}
}

func TestDeclaredSizesStillMustBeSane(t *testing.T) {
	g, _ := newGateway(t)
	for name, size := range map[string]int64{"zero": 0, "negative": -1, "beyond exact JSON integers": int64(maxDeclaredBytes) + 1} {
		if _, err := decodeFeedback(withSizes(t, g, size, 10), "fb1", 1, Limits{}); err == nil {
			t.Errorf("%s: declared size accepted", name)
		}
	}
}

func TestConfiguredArtifactAndBundleLimitsAreExactBoundaries(t *testing.T) {
	g, _ := newGateway(t)
	data := withSizes(t, g, 1000, 2000)

	if _, err := decodeFeedback(data, "fb1", 1, Limits{MaxArtifactBytes: 2000}); err != nil {
		t.Fatalf("artifact at the limit refused: %v", err)
	}
	if _, err := decodeFeedback(data, "fb1", 1, Limits{MaxArtifactBytes: 1999}); err == nil {
		t.Fatal("artifact above the limit accepted")
	}
	if _, err := decodeFeedback(data, "fb1", 1, Limits{MaxBundleBytes: 3000}); err != nil {
		t.Fatalf("bundle at the limit refused: %v", err)
	}
	if _, err := decodeFeedback(data, "fb1", 1, Limits{MaxBundleBytes: 2999}); err == nil {
		t.Fatal("bundle above the limit accepted")
	}
}

func TestPullHonoursConfiguredBundleLimitAndLeavesNothing(t *testing.T) {
	g, c := newGateway(t)
	total := int64(len(g.redacted) + len(g.annotated))

	limited, err := New(c.BaseURL(), tokens, nil, WithLimits(Limits{MaxBundleBytes: total - 1}))
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "bundle")
	if _, err := limited.Pull(context.Background(), PullOptions{WorkspaceID: "ws", FeedbackID: "fb1", Output: out}); err == nil {
		t.Fatal("pull above the bundle limit succeeded")
	}
	if _, err := os.Lstat(out); err == nil {
		t.Fatal("a refused pull left a bundle behind")
	}

	exact, err := New(c.BaseURL(), tokens, nil, WithLimits(Limits{MaxBundleBytes: total}))
	if err != nil {
		t.Fatal(err)
	}
	res, err := exact.Pull(context.Background(), PullOptions{WorkspaceID: "ws", FeedbackID: "fb1", Output: out})
	if err != nil {
		t.Fatalf("pull at the bundle limit: %v", err)
	}
	if _, err := VerifyBundle(res.Dir, Limits{MaxBundleBytes: total}); err != nil {
		t.Fatalf("verify at the bundle limit: %v", err)
	}
	if _, err := VerifyBundle(res.Dir, Limits{MaxBundleBytes: total - 1}); err == nil {
		t.Fatal("verify above the bundle limit succeeded")
	}
	if _, err := VerifyBundle(res.Dir, Limits{MaxJSONBytes: 10}); err == nil {
		t.Fatal("feedback.json above the JSON limit verified")
	}
}

func TestJSONResponseLimitIsOptIn(t *testing.T) {
	g, _ := newGateway(t)
	// A valid document padded with 3 MiB of whitespace, above the former 2 MiB cap.
	body := append(g.feedbackJSON(), []byte(strings.Repeat(" ", 3<<20))...)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	open, err := New(srv.URL, tokens, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := open.Get(context.Background(), "fb1", 1); err != nil {
		t.Fatalf("large JSON response refused without a limit: %v", err)
	}

	capped, err := New(srv.URL, tokens, nil, WithLimits(Limits{MaxJSONBytes: 1 << 20}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := capped.Get(context.Background(), "fb1", 1); err == nil {
		t.Fatal("JSON response above the configured limit accepted")
	}
}

func TestNegativeLimitsAreRefused(t *testing.T) {
	for _, l := range []Limits{{MaxArtifactBytes: -1}, {MaxBundleBytes: -1}, {MaxJSONBytes: -1}} {
		if _, err := New("http://127.0.0.1:1", tokens, nil, WithLimits(l)); err == nil {
			t.Errorf("limits %+v accepted", l)
		}
	}
}

func TestRequestDeadlinesAreOptIn(t *testing.T) {
	g, _ := newGateway(t)
	body := g.feedbackJSON()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	open, err := New(srv.URL, tokens, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := open.Get(context.Background(), "fb1", 1); err != nil {
		t.Fatalf("a slow response failed without a configured deadline: %v", err)
	}

	bounded, err := New(srv.URL, tokens, nil, WithTimeouts(Timeouts{JSON: 50 * time.Millisecond}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bounded.Get(context.Background(), "fb1", 1); err == nil {
		t.Fatal("a response slower than the configured deadline succeeded")
	}

	if _, err := New(srv.URL, tokens, nil, WithTimeouts(Timeouts{Artifact: -1})); err == nil {
		t.Fatal("negative timeout accepted")
	}
}
