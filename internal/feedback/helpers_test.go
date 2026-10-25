package feedback

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/storage"
)

var (
	humanP   = Principal{ID: "human-1", Role: RoleHuman, WorkspaceID: "ws1"}
	human2P  = Principal{ID: "human-2", Role: RoleHuman, WorkspaceID: "ws1"}
	foreignP = Principal{ID: "human-9", Role: RoleHuman, WorkspaceID: "ws2"}
	agentP   = Principal{ID: "agent-1", Role: RoleAgent, WorkspaceID: "ws1"}
	agent2P  = Principal{ID: "agent-2", Role: RoleAgent, WorkspaceID: "ws1"}
	viewerP  = Principal{ID: "viewer-1", Role: RoleViewer, WorkspaceID: "ws1"}
	adminP   = Principal{ID: "admin-1", Role: RoleAdmin, WorkspaceID: AllWorkspaces}
)

// secret is a colour that only occurs inside the area that must be redacted.
var secret = color.RGBA{R: 0xAB, G: 0xCD, B: 0xEF, A: 0xFF}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type env struct {
	t     *testing.T
	dir   string
	cfg   storage.Config
	st    *storage.Store
	svc   *Service
	h     http.Handler
	clock *fakeClock
	opts  Options
	keys  int
}

// testAgents is the deterministic configured-agent fixture: ws1 has two agents
// and no default (so a recipient must be named), every other workspace none.
func testAgents(ws string) AgentRoster {
	if ws == "ws1" {
		return AgentRoster{AgentIDs: []string{"agent-1", "agent-2"}}
	}
	return AgentRoster{}
}

func newEnv(t *testing.T, mod ...func(*Options)) *env {
	t.Helper()
	dir := t.TempDir()
	e := &env{t: t, dir: dir, clock: &fakeClock{t: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}}
	e.cfg = storage.Config{DBPath: filepath.Join(dir, "db", "lensdock.db"), PrivateDir: filepath.Join(dir, "private"), ArtifactDir: filepath.Join(dir, "artifacts")}
	e.opts = Options{Now: e.clock.Now, RuntimeGeneration: func(string) string { return "gen-1" }, Agents: testAgents}
	for _, m := range mod {
		m(&e.opts)
	}
	e.open()
	t.Cleanup(func() { _ = e.st.Close() })
	return e
}

func (e *env) open() {
	e.t.Helper()
	st, err := storage.Open(context.Background(), e.cfg)
	if err != nil {
		e.t.Fatalf("storage.Open: %v", err)
	}
	svc, err := New(context.Background(), st, e.opts)
	if err != nil {
		e.t.Fatalf("New: %v", err)
	}
	e.st, e.svc, e.h = st, svc, NewHandler(svc)
}

// restart closes and reopens the store, simulating a Gateway restart.
func (e *env) restart() {
	e.t.Helper()
	if err := e.st.Close(); err != nil {
		e.t.Fatalf("close: %v", err)
	}
	e.open()
}

type resp struct {
	t  *testing.T
	rr *httptest.ResponseRecorder
}

func (r resp) status() int { return r.rr.Code }

func (r resp) body() []byte { return r.rr.Body.Bytes() }

func (r resp) into(v any) {
	r.t.Helper()
	if err := json.Unmarshal(r.rr.Body.Bytes(), v); err != nil {
		r.t.Fatalf("decode %q: %v", r.rr.Body.String(), err)
	}
}

func (r resp) code() string {
	var e struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(r.rr.Body.Bytes(), &e)
	return e.Code
}

func (r resp) want(status int) resp {
	r.t.Helper()
	if r.rr.Code != status {
		r.t.Fatalf("status = %d, want %d; body: %s", r.rr.Code, status, r.rr.Body.String())
	}
	return r
}

func (r resp) wantErr(status int, code string) resp {
	r.t.Helper()
	if r.rr.Code != status || r.code() != code {
		r.t.Fatalf("got %d %q (%s), want %d %q", r.rr.Code, r.code(), r.rr.Body.String(), status, code)
	}
	return r
}

// do sends a request as principal p (nil = unauthenticated). body may be nil,
// []byte (sent as is) or any value (sent as JSON).
func (e *env) do(p *Principal, method, path string, body any, hdr ...string) resp {
	e.t.Helper()
	var rdr *bytes.Reader
	ct := ""
	switch b := body.(type) {
	case nil:
		rdr = bytes.NewReader(nil)
	case []byte:
		rdr = bytes.NewReader(b)
	default:
		data, err := json.Marshal(b)
		if err != nil {
			e.t.Fatalf("marshal: %v", err)
		}
		rdr = bytes.NewReader(data)
		ct = "application/json"
	}
	req := httptest.NewRequest(method, path, rdr)
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	explicitKey := false
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
		if http.CanonicalHeaderKey(hdr[i]) == "Idempotency-Key" {
			explicitKey = true
		}
	}
	if !explicitKey && method != http.MethodGet && method != http.MethodDelete {
		e.keys++
		req.Header.Set("Idempotency-Key", fmt.Sprintf("key-%d", e.keys))
	}
	if p != nil {
		req = req.WithContext(WithPrincipal(req.Context(), *p))
	}
	rr := httptest.NewRecorder()
	e.h.ServeHTTP(rr, req)
	return resp{t: e.t, rr: rr}
}

func (e *env) png(p *Principal, method, path string, data []byte, hdr ...string) resp {
	e.t.Helper()
	return e.do(p, method, path, data, append([]string{"Content-Type", "image/png"}, hdr...)...)
}

// secretPNG is a 160x100 grey image with a secret-coloured block at pixels
// [40,100)x[30,70). Normalized redaction rect (0.2,0.25,0.5,0.5) covers it.
func secretPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 160, 100))
	for y := 0; y < 100; y++ {
		for x := 0; x < 160; x++ {
			img.SetRGBA(x, y, color.RGBA{R: 200, G: 200, B: 200, A: 255})
		}
	}
	for y := 30; y < 70; y++ {
		for x := 40; x < 100; x++ {
			img.SetRGBA(x, y, secret)
		}
	}
	return encode(t, img)
}

func encode(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	return buf.Bytes()
}

func hasSecret(t *testing.T, data []byte) bool {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			if uint8(r>>8) == secret.R && uint8(g>>8) == secret.G && uint8(bl>>8) == secret.B {
				return true
			}
		}
	}
	return false
}

func redactAnnotation(id string) map[string]any {
	return map[string]any{"id": id, "kind": "redact", "mode": "solid", "geometry": map[string]any{"x": 0.2, "y": 0.25, "width": 0.5, "height": 0.5}}
}

func rectAnnotation(id string) map[string]any {
	return map[string]any{"id": id, "kind": "rectangle", "comment": "overlap", "geometry": map[string]any{"x": 0.1, "y": 0.1, "width": 0.3, "height": 0.2}, "style": map[string]any{"stroke": "#ff0000", "strokeWidthPx": 3}}
}

func goodTask() map[string]any {
	return map[string]any{
		"kind": "bug", "intent": "modify-and-verify", "title": "Button overlaps panel",
		"description": "The primary button covers the filter label.", "expectedResult": "No overlap.", "actualResult": "Overlap.",
		"reproductionSteps": []string{"Open the page", "Resize to 800px"},
	}
}

// capture reserves and uploads a desktop capture for p and returns its ID.
func (e *env) capture(p Principal, data []byte) string {
	e.t.Helper()
	var st CaptureState
	e.do(&p, "POST", "/api/v1/workspaces/"+p.WorkspaceID+"/captures", map[string]any{"source": "desktop"}).want(202).into(&st)
	e.png(&p, "PUT", "/api/v1/captures/"+st.CaptureID+"/image", data).want(200).into(&st)
	if st.Status != StatusReady || st.ImageWidthPx != 160 {
		e.t.Fatalf("capture state = %+v", st)
	}
	return st.CaptureID
}

func (e *env) draft(p Principal, capID string) Draft {
	e.t.Helper()
	var d Draft
	e.do(&p, "POST", "/api/v1/feedback", map[string]any{"workspaceId": p.WorkspaceID, "captureId": capID, "task": goodTask()}).want(201).into(&d)
	return d
}

// annotated returns a draft with the given annotations and an uploaded preview
// (the preview deliberately still contains the secret, as an untrusted client
// render could).
func (e *env) annotated(p Principal, d Draft, annots ...map[string]any) Draft {
	e.t.Helper()
	var out Draft
	e.do(&p, "PATCH", "/api/v1/feedback/"+d.FeedbackID, map[string]any{"annotations": annots}, "If-Match", quote(d.ETag)).want(200).into(&out)
	var dd DraftDetail
	e.png(&p, "PUT", "/api/v1/feedback/"+d.FeedbackID+"/draft/preview", secretPNG(e.t), "If-Match", quote(out.ETag)).want(200).into(&dd)
	if !dd.PreviewReady || dd.ETag != out.ETag {
		e.t.Fatalf("preview not bound: %+v", dd)
	}
	return out
}

func quote(s string) string { return `"` + s + `"` }

func (e *env) publish(p Principal, d Draft) PublishedFeedback {
	e.t.Helper()
	var pf PublishedFeedback
	e.do(&p, "POST", "/api/v1/feedback/"+d.FeedbackID+"/publish", map[string]any{"humanApproved": true}, "If-Match", quote(d.ETag)).want(201).into(&pf)
	return pf
}

func (e *env) privateFiles() int {
	e.t.Helper()
	objs, err := e.st.Private.List()
	if err != nil {
		e.t.Fatalf("list private: %v", err)
	}
	return len(objs)
}

func crc32PNG(b []byte) uint32 { return crc32.ChecksumIEEE(b) }

func newRequest(ctx context.Context, method, target string, body io.Reader) *http.Request {
	return httptest.NewRequest(method, target, body).WithContext(ctx)
}

// flushRecorder is a concurrency-safe streaming ResponseWriter.
type flushRecorder struct {
	mu   sync.Mutex
	hdr  http.Header
	Code int
	buf  bytes.Buffer
}

func newFlushRecorder() *flushRecorder {
	return &flushRecorder{hdr: http.Header{}, Code: http.StatusOK}
}

func (f *flushRecorder) Header() http.Header { return f.hdr }

func (f *flushRecorder) WriteHeader(code int) {
	f.mu.Lock()
	f.Code = code
	f.mu.Unlock()
}

func (f *flushRecorder) Write(b []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.buf.Write(b)
}

func (f *flushRecorder) Flush() {}

func (f *flushRecorder) String() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.buf.String()
}
