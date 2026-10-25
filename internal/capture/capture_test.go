package capture

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash/crc32"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakePage is the scriptable state of one fake target.
type fakePage struct {
	id       string
	typ      string
	title    string
	url      string
	loaderID string
	entryID  int
	scrollX  float64
	scrollY  float64
	vw, vh   float64
	dpr      float64
	visible  string // document.visibilityState; "" makes Runtime.evaluate fail
	shot     []byte // PNG returned by Page.captureScreenshot
	hang     bool   // Page.captureScreenshot never answers
	noWS     bool   // the WebSocket upgrade is refused
	// onShot runs while the screenshot is being taken (to simulate a
	// navigation or scroll mid-capture).
	onShot func(p *fakePage, attempt int)
}

type fakeChromium struct {
	t   *testing.T
	srv *httptest.Server

	mu    sync.Mutex
	pages []*fakePage
	shots map[string]int
	calls []string // "<target>:<method>"
}

func newFakeChromium(t *testing.T, pages ...*fakePage) *fakeChromium {
	t.Helper()
	f := &fakeChromium{t: t, pages: pages, shots: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeChromium) endpoint() string { return strings.TrimPrefix(f.srv.URL, "http://") }

func (f *fakeChromium) page(id string) *fakePage {
	for _, p := range f.pages {
		if p.id == id {
			return p
		}
	}
	return nil
}

func (f *fakeChromium) record(target, method string) {
	f.mu.Lock()
	f.calls = append(f.calls, target+":"+method)
	f.mu.Unlock()
}

func (f *fakeChromium) called(target, method string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == target+":"+method {
			return true
		}
	}
	return false
}

func (f *fakeChromium) serve(w http.ResponseWriter, r *http.Request) {
	if _, ok := r.Header["Origin"]; ok {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	switch {
	case r.URL.Path == "/json/version":
		_ = json.NewEncoder(w).Encode(map[string]string{
			"Browser":              "Chrome/139.0.0.0",
			"webSocketDebuggerUrl": "ws://" + f.endpoint() + "/devtools/browser/abc",
		})
	case r.URL.Path == "/json/list":
		f.mu.Lock()
		out := make([]map[string]string, 0, len(f.pages))
		for _, p := range f.pages {
			out = append(out, map[string]string{"id": p.id, "type": p.typ, "title": p.title, "url": p.url})
		}
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(out)
	case strings.HasPrefix(r.URL.Path, "/devtools/page/"):
		p := f.page(strings.TrimPrefix(r.URL.Path, "/devtools/page/"))
		if p == nil || p.noWS {
			http.NotFound(w, r)
			return
		}
		f.serveWS(w, r, p)
	case r.URL.Path == "/devtools/browser/abc":
		f.serveWS(w, r, nil)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeChromium) serveWS(w http.ResponseWriter, r *http.Request, p *fakePage) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	defer c.CloseNow()
	c.SetReadLimit(1 << 20)
	target := "browser"
	if p != nil {
		target = p.id
	}
	for {
		_, data, err := c.Read(r.Context())
		if err != nil {
			return
		}
		var req struct {
			ID     int64          `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if json.Unmarshal(data, &req) != nil {
			return
		}
		f.record(target, req.Method)
		// An unrelated event first: the client must skip it.
		_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"method":"Page.frameNavigated","params":{}}`))
		result, rpcErr, hang := f.handle(p, req.Method, req.Params)
		if hang {
			<-r.Context().Done()
			return
		}
		resp := map[string]any{"id": req.ID}
		if rpcErr != "" {
			resp["error"] = map[string]any{"code": -32000, "message": rpcErr}
		} else {
			resp["result"] = result
		}
		b, _ := json.Marshal(resp)
		if err := c.Write(r.Context(), websocket.MessageText, b); err != nil {
			return
		}
	}
}

func (f *fakeChromium) handle(p *fakePage, method string, params map[string]any) (result any, rpcErr string, hang bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p == nil {
		return nil, "unknown browser method", false
	}
	switch method {
	case "Page.getFrameTree":
		return map[string]any{"frameTree": map[string]any{"frame": map[string]any{"id": "F", "loaderId": p.loaderID, "url": p.url}}}, "", false
	case "Page.getNavigationHistory":
		return map[string]any{"currentIndex": 1, "entries": []map[string]any{
			{"id": p.entryID - 1, "url": "about:blank", "title": ""},
			{"id": p.entryID, "url": p.url, "title": p.title},
		}}, "", false
	case "Page.getLayoutMetrics":
		return map[string]any{
			"cssLayoutViewport": map[string]any{"pageX": p.scrollX, "pageY": p.scrollY, "clientWidth": p.vw, "clientHeight": p.vh},
			"cssVisualViewport": map[string]any{"pageX": p.scrollX, "pageY": p.scrollY, "clientWidth": p.vw, "clientHeight": p.vh, "scale": 1, "zoom": 1},
		}, "", false
	case "Runtime.evaluate":
		expr, _ := params["expression"].(string)
		if expr == "document.visibilityState" {
			if p.visible == "" {
				return nil, "evaluation failed", false
			}
			return map[string]any{"result": map[string]any{"type": "string", "value": p.visible}}, "", false
		}
		return map[string]any{"result": map[string]any{"type": "number", "value": p.dpr}}, "", false
	case "Page.bringToFront":
		return map[string]any{}, "", false
	case "Page.captureScreenshot":
		if p.hang {
			return nil, "", true
		}
		f.shots[p.id]++
		attempt := f.shots[p.id]
		if p.onShot != nil {
			p.onShot(p, attempt)
		}
		return map[string]any{"data": base64.StdEncoding.EncodeToString(p.shot)}, "", false
	}
	return nil, "unknown method " + method, false
}

func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// headerOnlyPNG is a valid PNG prefix (signature, IHDR, empty IDAT) that
// declares w x h pixels, enough for png.DecodeConfig without allocating them.
func headerOnlyPNG(w, h uint32) []byte {
	chunk := func(typ string, data []byte) []byte {
		out := binary.BigEndian.AppendUint32(nil, uint32(len(data)))
		out = append(out, typ...)
		out = append(out, data...)
		return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(append([]byte(typ), data...)))
	}
	ihdr := binary.BigEndian.AppendUint32(nil, w)
	ihdr = binary.BigEndian.AppendUint32(ihdr, h)
	ihdr = append(ihdr, 8, 0, 0, 0, 0) // 8-bit grayscale
	out := []byte("\x89PNG\r\n\x1a\n")
	out = append(out, chunk("IHDR", ihdr)...)
	return append(out, chunk("IDAT", nil)...)
}

func basePage(t *testing.T, id string) *fakePage {
	return &fakePage{
		id: id, typ: "page", title: "Checkout", url: "https://app.test/cart?step=2",
		loaderID: "L1", entryID: 3, vw: 1000, vh: 500, dpr: 2, visible: "visible",
		shot: testPNG(t, 2000, 1000),
	}
}

func newCapturer(t *testing.T, f *fakeChromium) *Capturer {
	t.Helper()
	c, err := New(Config{Endpoint: f.endpoint(), CallTimeout: 2 * time.Second, ScreenshotTimeout: 400 * time.Millisecond,
		Now: func() time.Time { return time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC) }})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCaptureStablePageRecordsMetadataAndScale(t *testing.T) {
	p := basePage(t, "AAAA1111")
	f := newFakeChromium(t, p)
	res, img, err := newCapturer(t, f).Capture(context.Background(), "AAAA1111", Options{})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(img)
	if res.Source != Source || res.TargetID != "AAAA1111" || res.Consistency != ConsistencyStable || res.Attempts != 1 {
		t.Fatalf("result = %+v", res)
	}
	if res.Image.Width != 2000 || res.Image.Height != 1000 || res.Image.Bytes != len(img) ||
		res.Image.SHA256 != hex.EncodeToString(sum[:]) || res.Image.ContentType != "image/png" {
		t.Fatalf("image = %+v", res.Image)
	}
	if res.Scale.PixelsPerCSSX != 2 || res.Scale.PixelsPerCSSY != 2 {
		t.Fatalf("scale = %+v, want 2 (image px / viewport css px)", res.Scale)
	}
	if res.NavigationID != "L1:3" || res.BrowserVersion != "Chrome/139.0.0.0" || !strings.HasPrefix(res.CaptureID, "rcap_") {
		t.Fatalf("ids = %q %q %q", res.NavigationID, res.BrowserVersion, res.CaptureID)
	}
	want := State{
		TargetID: "AAAA1111", URL: "https://app.test/cart?step=2", Title: "Checkout", NavigationEntryID: 3, LoaderID: "L1",
		DevicePixelRatio: 2, Viewport: Viewport{CSSWidth: 1000, CSSHeight: 500, Scale: 1, Zoom: 1},
	}
	if res.Pre != want || res.Post != want {
		t.Fatalf("pre = %+v\npost = %+v\nwant %+v", res.Pre, res.Post, want)
	}
	if !res.CapturedAt.Equal(time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("capturedAt = %v", res.CapturedAt)
	}
	if f.called("AAAA1111", "Page.bringToFront") {
		t.Fatal("a capture must not take focus unless asked")
	}
	if cfg, err := png.DecodeConfig(bytes.NewReader(img)); err != nil || cfg.Width != 2000 {
		t.Fatalf("returned bytes are not the screenshot: %v", err)
	}
}

func TestCaptureRetriesOnceWhenTheDocumentChangesMidCapture(t *testing.T) {
	p := basePage(t, "AAAA1111")
	p.onShot = func(p *fakePage, attempt int) {
		if attempt == 1 {
			p.loaderID, p.entryID, p.url = "L2", 4, "https://app.test/done"
		}
	}
	f := newFakeChromium(t, p)
	res, _, err := newCapturer(t, f).Capture(context.Background(), "AAAA1111", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Attempts != 2 || res.Consistency != ConsistencyStable || res.NavigationID != "L2:4" || res.Pre.URL != "https://app.test/done" {
		t.Fatalf("result = %+v, want a stable second attempt on the new document", res)
	}
}

func TestCaptureReportsUnknownWhenNavigationKeepsChanging(t *testing.T) {
	p := basePage(t, "AAAA1111")
	n := 10
	p.onShot = func(p *fakePage, _ int) { n++; p.loaderID, p.entryID = "L"+string(rune('A'+n%20)), n }
	f := newFakeChromium(t, p)
	res, img, err := newCapturer(t, f).Capture(context.Background(), "AAAA1111", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Consistency != ConsistencyUnknown || res.Attempts != 2 || len(img) == 0 {
		t.Fatalf("result = %+v, want unknown after the retry", res)
	}
	if res.Pre.LoaderID == res.Post.LoaderID {
		t.Fatal("pre and post must show the navigation that happened")
	}
}

func TestCaptureReportsBestEffortWhenOnlyScrollChanges(t *testing.T) {
	p := basePage(t, "AAAA1111")
	p.onShot = func(p *fakePage, _ int) { p.scrollY = 240 }
	f := newFakeChromium(t, p)
	res, _, err := newCapturer(t, f).Capture(context.Background(), "AAAA1111", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Consistency != ConsistencyBestEffort || res.Attempts != 1 || res.Pre.Scroll.Y != 0 || res.Post.Scroll.Y != 240 {
		t.Fatalf("result = %+v", res)
	}
}

func TestCaptureRefusesUnknownAndMalformedTargets(t *testing.T) {
	p := basePage(t, "AAAA1111")
	worker := basePage(t, "WORKER01")
	worker.typ = "service_worker"
	f := newFakeChromium(t, p, worker)
	c := newCapturer(t, f)

	if _, _, err := c.Capture(context.Background(), "NOPE", Options{}); !errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("unknown target = %v", err)
	}
	if _, _, err := c.Capture(context.Background(), "WORKER01", Options{}); !errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("non-page target = %v, want ErrTargetNotFound", err)
	}
	for _, id := range []string{"", "../json/version", "a/b", "x y", strings.Repeat("a", 257), "a?b"} {
		if _, _, err := c.Capture(context.Background(), id, Options{}); !errors.Is(err, ErrInvalidTarget) {
			t.Errorf("Capture(%q) = %v, want ErrInvalidTarget", id, err)
		}
	}
	if f.called("AAAA1111", "Page.captureScreenshot") {
		t.Fatal("a refused capture must not screenshot any page")
	}
}

func TestCaptureOfAHiddenTabFailsClearlyAndActivateIsExplicit(t *testing.T) {
	p := basePage(t, "AAAA1111")
	p.hang = true
	f := newFakeChromium(t, p)
	c := newCapturer(t, f)
	_, _, err := c.Capture(context.Background(), "AAAA1111", Options{})
	if !errors.Is(err, ErrCaptureFailed) || !strings.Contains(err.Error(), "activate") {
		t.Fatalf("hidden tab = %v, want ErrCaptureFailed hinting at activate", err)
	}
	if f.called("AAAA1111", "Page.bringToFront") {
		t.Fatal("focus was taken without Activate")
	}
	_, _, err = c.Capture(context.Background(), "AAAA1111", Options{Activate: true})
	if !errors.Is(err, ErrCaptureFailed) {
		t.Fatalf("hung screenshot with activate = %v", err)
	}
	if !f.called("AAAA1111", "Page.bringToFront") {
		t.Fatal("Activate did not bring the target to the front")
	}
}

func TestCaptureRejectsOversizedAndBrokenImages(t *testing.T) {
	cases := map[string]struct {
		shot []byte
		want error
	}{
		"too many pixels": {headerOnlyPNG(5000, 5000), ErrImageLimit},
		"not a png":       {[]byte("GIF89a not a png"), ErrCaptureFailed},
		"empty":           {nil, ErrCaptureFailed},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			p := basePage(t, "AAAA1111")
			p.shot = tc.shot
			f := newFakeChromium(t, p)
			if _, _, err := newCapturer(t, f).Capture(context.Background(), "AAAA1111", Options{}); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
	// 24 megapixels exactly is accepted.
	p := basePage(t, "AAAA1111")
	p.shot = headerOnlyPNG(6000, 4000)
	f := newFakeChromium(t, p)
	if res, _, err := newCapturer(t, f).Capture(context.Background(), "AAAA1111", Options{}); err != nil || res.Image.Width != 6000 {
		t.Fatalf("24 MP capture = %+v, %v", res.Image, err)
	}
}

func TestCaptureSurfacesBrowserErrorsAsCaptureFailed(t *testing.T) {
	p := basePage(t, "AAAA1111")
	p.noWS = true
	f := newFakeChromium(t, p)
	if _, _, err := newCapturer(t, f).Capture(context.Background(), "AAAA1111", Options{}); !errors.Is(err, ErrCaptureFailed) {
		t.Fatalf("refused WebSocket = %v, want ErrCaptureFailed", err)
	}
}

func TestListTargetsFiltersPagesAndProbesVisibility(t *testing.T) {
	fg := basePage(t, "FOREGND1")
	bg := basePage(t, "BACKGND1")
	bg.visible = "hidden"
	broken := basePage(t, "BROKEN01")
	broken.visible = "" // the probe fails
	gone := basePage(t, "NOSOCK01")
	gone.noWS = true
	ext := basePage(t, "EXT00001")
	ext.typ = "background_page"
	f := newFakeChromium(t, fg, bg, broken, gone, ext)

	ts, err := newCapturer(t, f).ListTargets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 4 {
		t.Fatalf("targets = %+v, want 4 pages", ts)
	}
	byID := map[string]Target{}
	for _, tg := range ts {
		byID[tg.ID] = tg
	}
	if v := byID["FOREGND1"].Visible; v == nil || !*v {
		t.Errorf("foreground visible = %v", v)
	}
	if v := byID["BACKGND1"].Visible; v == nil || *v {
		t.Errorf("background visible = %v", v)
	}
	for _, id := range []string{"BROKEN01", "NOSOCK01"} {
		if byID[id].Visible != nil {
			t.Errorf("%s visible = %v, want nil (unknown)", id, *byID[id].Visible)
		}
	}
	if tg := byID["FOREGND1"]; tg.Type != "page" || tg.Title != "Checkout" || tg.URL != "https://app.test/cart?step=2" {
		t.Errorf("target = %+v", tg)
	}
	if _, ok := byID["EXT00001"]; ok {
		t.Error("non-page target listed")
	}
	// The wire shape is stable: visible is null, not absent.
	raw, _ := json.Marshal(byID["BROKEN01"])
	if !strings.Contains(string(raw), `"visible":null`) {
		t.Errorf("json = %s", raw)
	}
}

func TestNewRejectsABadEndpoint(t *testing.T) {
	for _, e := range []string{"", "127.0.0.1", ":9223", "http://127.0.0.1:9223"} {
		if _, err := New(Config{Endpoint: e}); err == nil {
			t.Errorf("New(%q) succeeded", e)
		}
	}
}

func TestClassify(t *testing.T) {
	base := State{TargetID: "T", LoaderID: "L", NavigationEntryID: 1, DevicePixelRatio: 2, Viewport: Viewport{CSSWidth: 100, CSSHeight: 50, Scale: 1, Zoom: 1}}
	mod := func(f func(*State)) State { s := base; f(&s); return s }
	for name, tc := range map[string]struct {
		post State
		ok   bool
		want string
	}{
		"identical":        {base, true, ConsistencyStable},
		"post unavailable": {base, false, ConsistencyUnknown},
		"new document":     {mod(func(s *State) { s.LoaderID = "L2" }), true, ConsistencyUnknown},
		"new entry":        {mod(func(s *State) { s.NavigationEntryID = 2 }), true, ConsistencyUnknown},
		"other target":     {mod(func(s *State) { s.TargetID = "U" }), true, ConsistencyUnknown},
		"scrolled":         {mod(func(s *State) { s.Scroll.Y = 5 }), true, ConsistencyBestEffort},
		"resized":          {mod(func(s *State) { s.Viewport.CSSWidth = 80 }), true, ConsistencyBestEffort},
		"zoomed":           {mod(func(s *State) { s.Viewport.Zoom = 1.5 }), true, ConsistencyBestEffort},
		"dpr changed":      {mod(func(s *State) { s.DevicePixelRatio = 1 }), true, ConsistencyBestEffort},
	} {
		if got := classify(base, tc.post, tc.ok); got != tc.want {
			t.Errorf("%s: classify = %s, want %s", name, got, tc.want)
		}
	}
}

func TestStoreHoldsImagesBrieflyAndReplaysByKey(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	s := NewStore(time.Minute, 2, clock)
	r := func(id string) Result { return Result{CaptureID: id, TargetID: "T"} }

	s.Put("k1", "fp1", r("c1"), []byte("one"))
	if b, ok := s.Image("c1"); !ok || string(b) != "one" {
		t.Fatalf("Image(c1) = %q, %v", b, ok)
	}
	if got, found, mismatch := s.Replay("k1", "fp1"); !found || mismatch || got.CaptureID != "c1" {
		t.Fatalf("Replay = %+v %v %v", got, found, mismatch)
	}
	if _, found, mismatch := s.Replay("k1", "other"); !found || !mismatch {
		t.Fatalf("Replay with another fingerprint = found %v mismatch %v, want a mismatch", found, mismatch)
	}
	if _, found, _ := s.Replay("nope", "fp1"); found {
		t.Fatal("unknown key replayed")
	}

	// The oldest image is evicted beyond the limit.
	s.Put("k2", "fp", r("c2"), []byte("two"))
	s.Put("k3", "fp", r("c3"), []byte("three"))
	if _, ok := s.Image("c1"); ok {
		t.Fatal("c1 should have been evicted")
	}
	if _, ok := s.Image("c3"); !ok {
		t.Fatal("c3 missing")
	}
	// Replayed metadata outlives the image's eviction (idempotent retry).
	if _, found, _ := s.Replay("k1", "fp1"); !found {
		t.Fatal("replay metadata must survive image eviction")
	}

	if !s.Delete("c3") || s.Delete("c3") {
		t.Fatal("Delete must report whether the image existed")
	}
	if _, ok := s.Image("c3"); ok {
		t.Fatal("deleted image still served")
	}

	// Images expire with the TTL; replay metadata with its own longer one.
	now = now.Add(time.Minute + time.Second)
	if _, ok := s.Image("c2"); ok {
		t.Fatal("expired image served")
	}
	if _, found, _ := s.Replay("k1", "fp1"); !found {
		t.Fatal("replay must outlive the image TTL")
	}
	now = now.Add(replayTTL)
	if _, found, _ := s.Replay("k1", "fp1"); found {
		t.Fatal("replay must expire")
	}
}
