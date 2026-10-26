package runtimeapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
	"github.com/JiaWeiXie/lensdock/internal/auth"
	"github.com/JiaWeiXie/lensdock/internal/capture"
	"github.com/JiaWeiXie/lensdock/internal/clipboard"
	"github.com/JiaWeiXie/lensdock/internal/lease"
	"github.com/JiaWeiXie/lensdock/internal/supervisor"
	"github.com/JiaWeiXie/lensdock/internal/transfer"
	"github.com/JiaWeiXie/lensdock/internal/vnc"
)

const base = "/api/v1/workspaces/default"

// testDrainTimeout is the lease manager's drain limit in these tests. It
// bounds real socket teardown, so it is generous; the fake clock advances it
// instantly where a drain cannot complete.
const testDrainTimeout = time.Second

// ---- fakes ------------------------------------------------------------

// pending counts timers that have neither fired nor been stopped.
func (c *fakeClock) pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, t := range c.timers {
		if !t.stopped && !t.fired {
			n++
		}
	}
	return n
}

type richHealth struct{ *fakeHealth }

func (richHealth) ChromiumVersion() string { return "Chromium/139.0" }

func (richHealth) Resources() supervisor.Resources {
	ns, cur, total := uint64(2_000_000_000), uint64(512<<20), uint64(100<<30)
	return supervisor.Resources{
		SampledAt: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC),
		CPU:       supervisor.CPUUsage{UsageNanos: &ns},
		Memory:    supervisor.MemoryUsage{CurrentBytes: &cur},
		Disk:      []supervisor.DiskUsage{{Name: "profile", TotalBytes: &total}},
	}
}

// tcpEcho stands in for an x11vnc instance.
type tcpEcho struct {
	ln       net.Listener
	mu       sync.Mutex
	received []byte
	closed   chan struct{} // closed when a connection saw EOF
	once     sync.Once
}

func newTCPEcho(t *testing.T) *tcpEcho {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e := &tcpEcho{ln: ln, closed: make(chan struct{})}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				buf := make([]byte, 1024)
				for {
					n, err := c.Read(buf)
					if n > 0 {
						e.mu.Lock()
						e.received = append(e.received, buf[:n]...)
						e.mu.Unlock()
						_, _ = c.Write(append([]byte("echo:"), buf[:n]...))
					}
					if err != nil {
						e.once.Do(func() { close(e.closed) })
						return
					}
				}
			}()
		}
	}()
	return e
}

func (e *tcpEcho) addr() string { return e.ln.Addr().String() }

func (e *tcpEcho) got() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return string(e.received)
}

// cdpStub is the CDP proxy: every request blocks until released or cancelled,
// like a long-lived DevTools WebSocket.
type cdpStub struct {
	started chan struct{}
	release chan struct{}
}

func (c *cdpStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	select {
	case c.started <- struct{}{}:
	default:
	}
	select {
	case <-r.Context().Done():
	case <-c.release:
	}
	w.WriteHeader(http.StatusNoContent)
}

type fakeCapturer struct {
	mu      sync.Mutex
	calls   int
	targets []capture.Target
	result  capture.Result
	png     []byte
	err     error
	hook    func()
	opts    capture.Options
}

func (f *fakeCapturer) ListTargets(context.Context) ([]capture.Target, error) {
	return f.targets, f.err
}

func (f *fakeCapturer) Capture(_ context.Context, id string, o capture.Options) (capture.Result, []byte, error) {
	f.mu.Lock()
	f.calls++
	f.opts = o
	hook, err, res, png := f.hook, f.err, f.result, f.png
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	if err != nil {
		return capture.Result{}, nil, err
	}
	res.TargetID = id
	return res, png, nil
}

func (f *fakeCapturer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

const xrandrOut = `Screen 0: minimum 1 x 1, current %d x %d, maximum 1920 x 1080
screen connected primary %dx%d+0+0 0mm x 0mm
   1920x1080     60.00
`

type fakeXrandr struct {
	mu  sync.Mutex
	cur vnc.Size
	set []string
}

func (x *fakeXrandr) OutputBrowserProcess(_ context.Context, argv []string, _ int) ([]byte, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	switch {
	case len(argv) == 2 && argv[1] == "--query":
		return []byte(fmt.Sprintf(xrandrOut, x.cur.Width, x.cur.Height, x.cur.Width, x.cur.Height)), nil
	case len(argv) == 3 && argv[1] == "--fb":
		x.set = append(x.set, argv[2])
		var w, h int
		_, _ = fmt.Sscanf(argv[2], "%dx%d", &w, &h)
		x.cur = vnc.Size{Width: w, Height: h}
		return nil, nil
	}
	return nil, errors.New("unexpected argv")
}

// ---- environment ------------------------------------------------------

type cenv struct {
	t        *testing.T
	srv      *httptest.Server
	clock    *fakeClock
	leases   *lease.Manager
	spawner  *fakeSpawner
	viewUp   *tcpEcho
	ctlUp    *tcpEcho
	cdp      *cdpStub
	cap      *fakeCapturer
	xr       *fakeXrandr
	filesDir string
	logs     *bytes.Buffer
	health   *fakeHealth
	hc       *http.Client
}

func newCEnv(t *testing.T) *cenv {
	t.Helper()
	e := &cenv{
		t: t, clock: newFakeClock(), spawner: &fakeSpawner{}, viewUp: newTCPEcho(t), ctlUp: newTCPEcho(t),
		cdp: &cdpStub{started: make(chan struct{}, 8), release: make(chan struct{})},
		cap: &fakeCapturer{
			png: []byte("\x89PNG\r\n\x1a\nfake-image-bytes"),
			result: capture.Result{
				CaptureID: "rcap_test", Source: capture.Source, NavigationID: "L1:3", Consistency: capture.ConsistencyStable, Attempts: 1,
				BrowserVersion: "Chromium/139.0", Image: capture.Image{Width: 2000, Height: 1000, Bytes: 26, SHA256: "ab", ContentType: "image/png"},
				Scale: capture.Scale{PixelsPerCSSX: 2, PixelsPerCSSY: 2},
			},
		},
		xr:       &fakeXrandr{cur: vnc.Size{Width: 1920, Height: 1080}},
		filesDir: t.TempDir(), logs: &bytes.Buffer{}, health: &fakeHealth{ready: true},
		hc: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}
	m, err := lease.NewManager(lease.Config{WorkspaceID: testWS, Clock: e.clock, DrainTimeout: testDrainTimeout})
	if err != nil {
		t.Fatal(err)
	}
	m.SetLocked(false, "")
	e.leases = m
	t.Cleanup(m.Shutdown)

	limits := transfer.Limits{QuotaBytes: 1 << 20, MaxFileBytes: 64 << 10, MaxFiles: 5}
	h := NewHandler(Deps{
		WorkspaceID: testWS,
		Operator:    auth.DigestOf(operatorTok),
		Leases:      m,
		CDP:         e.cdp,
		Clipboard:   clipboard.New(e.spawner, time.Hour, 0),
		Health:      richHealth{e.health},
		Logger:      slog.New(slog.NewJSONHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		VNCView:     vnc.NewProxy(vnc.ProxyConfig{Instance: vnc.View, Addr: e.viewUp.addr(), MaxConns: 4}),
		VNCControl:  vnc.NewProxy(vnc.ProxyConfig{Instance: vnc.Control, Addr: e.ctlUp.addr(), MaxConns: 1}),
		Resizer:     vnc.NewResizer(e.xr),
		Capture:     e.cap,
		Files:       transfer.NewDirStore(e.filesDir, limits),
		FileLimits:  limits,
	})
	e.srv = httptest.NewServer(h)
	t.Cleanup(e.srv.Close)
	return e
}

type cresp struct {
	status int
	body   []byte
	hdr    http.Header
}

func (r cresp) code() string {
	var we apiwire.Error
	_ = json.Unmarshal(r.body, &we)
	return we.Code
}

func (r cresp) into(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.body, v); err != nil {
		t.Fatalf("decode %q: %v", r.body, err)
	}
}

func (e *cenv) call(method, path, token string, hdr map[string]string, body []byte) cresp {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := e.hc.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return cresp{status: resp.StatusCode, body: b, hdr: resp.Header}
}

func (e *cenv) expect(name string, r cresp, status int, code string) {
	e.t.Helper()
	if r.status != status {
		e.t.Errorf("%s: status = %d, want %d (body %s)", name, r.status, status, strings.TrimSpace(string(r.body)))
		return
	}
	if code != "" && r.code() != code {
		e.t.Errorf("%s: code = %q, want %q (body %s)", name, r.code(), code, strings.TrimSpace(string(r.body)))
	}
}

var cseq atomic.Int64

func idem() map[string]string {
	return map[string]string{"Idempotency-Key": fmt.Sprintf("k-%d", cseq.Add(1))}
}

func hdrs(base map[string]string, kv ...string) map[string]string {
	out := map[string]string{}
	for k, v := range base {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i]] = kv[i+1]
	}
	return out
}

// grant acquires a lease as the operator on behalf of holder.
func (e *cenv) grant(mode, holder string) apiwire.LeaseGrant {
	e.t.Helper()
	r := e.acquire(mode, holder, 0, nil)
	if r.status != http.StatusCreated {
		e.t.Fatalf("acquire %s: status %d body %s", mode, r.status, r.body)
	}
	var g apiwire.LeaseGrant
	r.into(e.t, &g)
	return g
}

func (e *cenv) acquire(mode, holder string, ttl int, extra map[string]string) cresp {
	e.t.Helper()
	h := idem()
	if holder != "" {
		h[headerHolderID] = holder
	}
	for k, v := range extra {
		h[k] = v
	}
	body := `{"mode":"` + mode + `"`
	if ttl != 0 {
		body += fmt.Sprintf(`,"ttlSeconds":%d`, ttl)
	}
	return e.call("POST", base+"/leases", operatorTok, h, []byte(body+`}`))
}

func (e *cenv) wsURL(path string) string { return "ws" + strings.TrimPrefix(e.srv.URL, "http") + path }

func (e *cenv) dial(path, token string, hdr map[string]string) (*websocket.Conn, *http.Response, error) {
	e.t.Helper()
	h := http.Header{}
	if token != "" {
		h.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		h.Set(k, v)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return websocket.Dial(ctx, e.wsURL(path), &websocket.DialOptions{HTTPHeader: h, CompressionMode: websocket.CompressionDisabled})
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (e *cenv) takeoverStatus() (string, int) {
	e.t.Helper()
	r := e.call("GET", base+"/takeover", operatorTok, nil, nil)
	if r.status != http.StatusOK {
		return "", r.status
	}
	var h handoffView
	r.into(e.t, &h)
	return h.Status, r.status
}

// startCDP begins a CDP request that stays in flight until released.
func (e *cenv) startCDP(token string, headers map[string]string) (done chan cresp) {
	e.t.Helper()
	done = make(chan cresp, 1)
	go func() {
		req, _ := http.NewRequest("GET", e.srv.URL+base+"/cdp/json/version", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := e.hc.Do(req)
		if err != nil {
			done <- cresp{status: -1}
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		done <- cresp{status: resp.StatusCode, body: b}
	}()
	select {
	case <-e.cdp.started:
	case <-time.After(3 * time.Second):
		e.t.Fatal("the CDP request never reached the proxy")
	}
	return done
}

func readWS(t *testing.T, c *websocket.Conn) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := c.Read(ctx)
	return string(data), err
}

func mustWrite(t *testing.T, c *websocket.Conn, s string) {
	t.Helper()
	if err := c.Write(context.Background(), websocket.MessageBinary, []byte(s)); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// ---- holder identity and state -----------------------------------------

func TestAcquireRecordsTheTrustedHolderIDAndRejectsMalformedOnes(t *testing.T) {
	e := newCEnv(t)
	for _, bad := range []string{"has space", "a/b", "semi;colon", strings.Repeat("a", 129), "tab\there"} {
		e.expect("holder "+bad, e.acquire("HUMAN", bad, 0, nil), 400, apiwire.CodeInvalidRequest)
	}
	if e.leases.Status().Lease != nil {
		t.Fatal("a rejected acquire created a lease")
	}
	g := e.grant("HUMAN", "actor:alice@example.test")
	if g.Lease.HolderID != "actor:alice@example.test" {
		t.Fatalf("holderId = %q", g.Lease.HolderID)
	}
	var ws apiwire.Workspace
	e.call("GET", base, operatorTok, nil, nil).into(t, &ws)
	if ws.HolderID == nil || *ws.HolderID != "actor:alice@example.test" {
		t.Fatalf("workspace holder = %v", ws.HolderID)
	}

	e2 := newCEnv(t)
	def := e2.grant("AGENT", "")
	if def.Lease.HolderID != operatorHolderID {
		t.Fatalf("default holder = %q, want %q", def.Lease.HolderID, operatorHolderID)
	}
	// The header is operator-only: a lease token cannot acquire at all.
	e2.expect("lease token acquire", e2.call("POST", base+"/leases", def.CapabilityToken, hdrs(idem(), headerHolderID, "x"), []byte(`{"mode":"AGENT"}`)), 401, apiwire.CodeUnauthenticated)
}

func TestStateSnapshotCarriesControlDisplayAndResourcesButNoSecrets(t *testing.T) {
	e := newCEnv(t)
	e.expect("no token", e.call("GET", base+"/state", "", nil, nil), 401, apiwire.CodeUnauthenticated)
	e.expect("wrong workspace", e.call("GET", "/api/v1/workspaces/other/state", operatorTok, nil, nil), 404, apiwire.CodeNotFound)

	var idle stateView
	r := e.call("GET", base+"/state", operatorTok, nil, nil)
	e.expect("idle state", r, 200, "")
	r.into(t, &idle)
	if idle.Lease != nil || idle.Epoch != nil || idle.HolderMode != nil || idle.LeaseExpiresAt != nil || idle.Handoff != nil {
		t.Fatalf("idle state = %+v", idle)
	}
	if idle.Workspace.ControlState != "IDLE" || !idle.Workspace.Ready || idle.Locked || idle.Display != (vnc.Size{Width: 1920, Height: 1080}) {
		t.Fatalf("idle state = %+v", idle)
	}
	if idle.BrowserVersion != "Chromium/139.0" || idle.Resources == nil || idle.Resources.CPU.UsageNanos == nil || *idle.Resources.CPU.UsageNanos != 2_000_000_000 {
		t.Fatalf("browser/resources = %q %+v", idle.BrowserVersion, idle.Resources)
	}
	if idle.Resources.CPU.Cores != nil || idle.Resources.Memory.MaxBytes != nil {
		t.Fatal("unmeasured resources must stay null")
	}

	g := e.grant("HUMAN", "actor:alice")
	var st stateView
	raw := e.call("GET", base+"/state", operatorTok, nil, nil)
	raw.into(t, &st)
	if st.Lease == nil || st.Lease.LeaseID != g.Lease.LeaseID || st.Epoch == nil || *st.Epoch != g.Lease.Epoch ||
		st.HolderMode == nil || *st.HolderMode != "HUMAN" || st.LeaseExpiresAt == nil || st.Workspace.ControlState != "HUMAN" {
		t.Fatalf("state with lease = %+v", st)
	}
	if strings.Contains(string(raw.body), g.CapabilityToken) || strings.Contains(string(raw.body), "capabilityToken") {
		t.Fatalf("state leaks the capability token: %s", raw.body)
	}

	// readyz?detail=1 carries the same extras.
	var rd readyDetail
	e.call("GET", "/readyz?detail=1", operatorTok, nil, nil).into(t, &rd)
	if rd.Display != (vnc.Size{Width: 1920, Height: 1080}) || rd.Resources == nil || rd.Lease == nil {
		t.Fatalf("readyz detail = %+v", rd)
	}
}

func TestLeaseCurrentIdentifiesTheTokenWithoutBindingHeaders(t *testing.T) {
	e := newCEnv(t)
	g := e.grant("AGENT", "actor:bot")
	r := e.call("GET", base+"/leases/current", g.CapabilityToken, nil, nil)
	e.expect("current", r, 200, "")
	var l apiwire.Lease
	r.into(t, &l)
	if l.LeaseID != g.Lease.LeaseID || l.Epoch != g.Lease.Epoch || l.HolderID != "actor:bot" {
		t.Fatalf("lease = %+v", l)
	}
	e.expect("no token", e.call("GET", base+"/leases/current", "", nil, nil), 401, apiwire.CodeUnauthenticated)
	e.expect("garbage token", e.call("GET", base+"/leases/current", "ldl_nope", nil, nil), 401, apiwire.CodeUnauthenticated)
	// DELETE on the same prefix still means "release lease <id>".
	e.expect("delete current", e.call("DELETE", base+"/leases/current", g.CapabilityToken, nil, nil), 404, apiwire.CodeNotFound)
	if err := e.leases.Release(g.Lease.LeaseID); err != nil {
		t.Fatal(err)
	}
	e.expect("ended token", e.call("GET", base+"/leases/current", g.CapabilityToken, nil, nil), 409, apiwire.CodeLeaseEnded)
}

// ---- takeover ------------------------------------------------------------

func takeoverBody(force bool) []byte {
	return []byte(fmt.Sprintf(`{"reason":"I need the browser","force":%t}`, force))
}

func TestTakeoverGrantsHumanOnlyAfterTheAgentConnectionDrained(t *testing.T) {
	e := newCEnv(t)
	agent := e.grant("AGENT", "actor:bot")
	// A tracked use that never ends keeps the drain from completing for the
	// whole "while draining" phase; the CDP request proves that a real
	// connection is cut off by the takeover.
	_, release, err := e.leases.AuthorizeTracked(agent.CapabilityToken, lease.ModeAgent)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	inflight := e.startCDP(agent.CapabilityToken, nil)

	key := "takeover-1"
	r := e.call("POST", base+"/takeover", operatorTok, map[string]string{"Idempotency-Key": key}, takeoverBody(false))
	e.expect("begin takeover", r, 202, "")
	var pending struct{ Status, RequestID string }
	r.into(t, &pending)
	if pending.Status != "HANDOFF_PENDING" || !takeoverIDPattern.MatchString(pending.RequestID) {
		t.Fatalf("takeover = %+v", pending)
	}
	// A retry with the same key is the same takeover.
	var again struct{ RequestID string }
	e.call("POST", base+"/takeover", operatorTok, map[string]string{"Idempotency-Key": key}, takeoverBody(false)).into(t, &again)
	if again.RequestID != pending.RequestID {
		t.Fatalf("replay returned %q, want %q", again.RequestID, pending.RequestID)
	}
	e.expect("other takeover", e.call("POST", base+"/takeover", operatorTok, idem(), takeoverBody(false)), 409, apiwire.CodeLeaseConflict)

	var h handoffView
	e.call("GET", base+"/takeover", operatorTok, nil, nil).into(t, &h)
	if h.Status != "DRAINING" || h.Drained || !h.UncertainEffects || h.PriorLease == nil || h.PriorLease.Mode != "AGENT" || h.PriorLease.LeaseID != agent.Lease.LeaseID {
		t.Fatalf("takeover while draining = %+v", h)
	}
	var ws apiwire.Workspace
	e.call("GET", base, operatorTok, nil, nil).into(t, &ws)
	if ws.ControlState != "HANDOFF_PENDING" || ws.HolderID != nil {
		t.Fatalf("workspace = %+v", ws)
	}

	// No HUMAN grant while the old connection can still act, by any route.
	takeoverHdr := map[string]string{headerTakeoverID: pending.RequestID}
	e.expect("human while draining", e.acquire("HUMAN", "actor:alice", 0, takeoverHdr), 409, apiwire.CodeLeaseConflict)
	e.expect("plain acquire", e.acquire("HUMAN", "actor:alice", 0, nil), 409, apiwire.CodeLeaseConflict)
	e.expect("agent acquire", e.acquire("AGENT", "actor:bot2", 0, nil), 409, apiwire.CodeLeaseConflict)
	// The fenced token is dead for new requests immediately.
	e.expect("old token current", e.call("GET", base+"/leases/current", agent.CapabilityToken, nil, nil), 409, apiwire.CodeLeaseEnded)
	e.expect("old token renew", e.call("POST", base+"/leases/"+agent.Lease.LeaseID+"/renew", agent.CapabilityToken, nil,
		[]byte(fmt.Sprintf(`{"epoch":%d,"runtimeGeneration":%q}`, agent.Lease.Epoch, agent.Lease.RuntimeGeneration))), 409, apiwire.CodeLeaseEnded)

	// The fenced request finishes without being served, and the connection the
	// runtime could not close is the last one holding the drain.
	close(e.cdp.release)
	select {
	case <-inflight: // it must complete: a fenced connection cannot hang
	case <-time.After(5 * time.Second):
		t.Fatal("the fenced CDP request never finished")
	}
	release()
	waitUntil(t, "takeover READY", func() bool { s, _ := e.takeoverStatus(); return s == "READY" })
	e.call("GET", base+"/takeover", operatorTok, nil, nil).into(t, &h)
	if !h.Drained || h.Forced {
		t.Fatalf("takeover after drain = %+v", h)
	}

	e.expect("ready but no header", e.acquire("HUMAN", "actor:alice", 0, nil), 409, apiwire.CodeLeaseConflict)
	e.expect("other takeover id", e.acquire("HUMAN", "actor:alice", 0, map[string]string{headerTakeoverID: "tko_" + strings.Repeat("0", 32)}), 404, apiwire.CodeNotFound)
	e.expect("malformed takeover id", e.acquire("HUMAN", "actor:alice", 0, map[string]string{headerTakeoverID: "../etc"}), 400, apiwire.CodeInvalidRequest)
	e.expect("agent with takeover id", e.acquire("AGENT", "actor:alice", 0, takeoverHdr), 400, apiwire.CodeInvalidRequest)

	r = e.acquire("HUMAN", "actor:alice", 0, takeoverHdr)
	e.expect("claim takeover", r, 201, "")
	var human apiwire.LeaseGrant
	r.into(t, &human)
	if human.Lease.Mode != "HUMAN" || human.Lease.HolderID != "actor:alice" || human.Lease.Epoch <= agent.Lease.Epoch {
		t.Fatalf("human grant = %+v (old epoch %d)", human.Lease, agent.Lease.Epoch)
	}
	e.expect("takeover consumed", e.call("GET", base+"/takeover", operatorTok, nil, nil), 404, apiwire.CodeNotFound)
	e.expect("old token still dead", e.call("GET", base+"/leases/current", agent.CapabilityToken, nil, nil), 409, apiwire.CodeLeaseEnded)
}

func TestTakeoverDrainTimeoutNeedsAnExplicitForce(t *testing.T) {
	e := newCEnv(t)
	agent := e.grant("AGENT", "actor:bot")
	// A tracked use that never ends keeps the drain from completing.
	_, release, err := e.leases.AuthorizeTracked(agent.CapabilityToken, lease.ModeAgent)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	var pending struct{ RequestID string }
	e.call("POST", base+"/takeover", operatorTok, idem(), takeoverBody(false)).into(t, &pending)
	waitUntil(t, "drain timer", func() bool { return e.clock.pending() >= 2 })
	e.clock.Advance(testDrainTimeout)
	waitUntil(t, "DRAIN_TIMEOUT", func() bool { s, _ := e.takeoverStatus(); return s == "DRAIN_TIMEOUT" })

	hdr := map[string]string{headerTakeoverID: pending.RequestID}
	e.expect("claim after timeout", e.acquire("HUMAN", "actor:alice", 0, hdr), 409, apiwire.CodeLeaseConflict)

	r := e.call("POST", base+"/takeover", operatorTok, idem(), takeoverBody(true))
	e.expect("force", r, 202, "")
	var forced struct{ RequestID string }
	r.into(t, &forced)
	if forced.RequestID != pending.RequestID {
		t.Fatalf("force created a new takeover %q", forced.RequestID)
	}
	var h handoffView
	e.call("GET", base+"/takeover", operatorTok, nil, nil).into(t, &h)
	if h.Status != "READY" || !h.Forced || h.Drained || !h.UncertainEffects {
		t.Fatalf("forced takeover = %+v", h)
	}
	e.expect("claim forced", e.acquire("HUMAN", "actor:alice", 0, hdr), 201, "")
}

func TestTakeoverCancelDoesNotRestoreAndValidatesInput(t *testing.T) {
	e := newCEnv(t)
	human := e.grant("HUMAN", "actor:alice")
	e.expect("no key", e.call("POST", base+"/takeover", operatorTok, nil, takeoverBody(false)), 400, apiwire.CodeInvalidRequest)
	e.expect("no reason", e.call("POST", base+"/takeover", operatorTok, idem(), []byte(`{"reason":""}`)), 400, apiwire.CodeInvalidRequest)
	e.expect("long reason", e.call("POST", base+"/takeover", operatorTok, idem(), []byte(`{"reason":"`+strings.Repeat("x", 1001)+`"}`)), 400, apiwire.CodeInvalidRequest)
	e.expect("unknown field", e.call("POST", base+"/takeover", operatorTok, idem(), []byte(`{"reason":"x","admin":true}`)), 400, apiwire.CodeInvalidRequest)
	e.expect("lease token", e.call("POST", base+"/takeover", human.CapabilityToken, idem(), takeoverBody(false)), 401, apiwire.CodeUnauthenticated)
	e.expect("no takeover to read", e.call("GET", base+"/takeover", operatorTok, nil, nil), 404, apiwire.CodeNotFound)
	e.expect("no takeover to cancel", e.call("DELETE", base+"/takeover", operatorTok, nil, nil), 404, apiwire.CodeNotFound)
	if e.leases.Status().Lease == nil {
		t.Fatal("rejected takeover requests must not fence the lease")
	}

	e.expect("begin", e.call("POST", base+"/takeover", operatorTok, idem(), takeoverBody(false)), 202, "")
	e.expect("cancel", e.call("DELETE", base+"/takeover", operatorTok, nil, nil), 204, "")
	e.expect("fenced lease stays dead", e.call("GET", base+"/leases/current", human.CapabilityToken, nil, nil), 409, apiwire.CodeLeaseEnded)
	var ws apiwire.Workspace
	e.call("GET", base, operatorTok, nil, nil).into(t, &ws)
	if ws.ControlState != "IDLE" {
		t.Fatalf("state after cancel = %s", ws.ControlState)
	}
}

func TestTakeoverClosesTheVNCControlSocketBeforeGrantingAndNothingMoreIsForwarded(t *testing.T) {
	e := newCEnv(t)
	human := e.grant("HUMAN", "actor:alice")
	c, _, err := e.dial(base+"/vnc/control", human.CapabilityToken, nil)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, c, "typed-before")
	if got, err := readWS(t, c); err != nil || got != "echo:typed-before" {
		t.Fatalf("echo = %q, %v", got, err)
	}

	e.expect("takeover", e.call("POST", base+"/takeover", operatorTok, idem(), takeoverBody(false)), 202, "")
	_, err = readWS(t, c)
	if got := websocket.CloseStatus(err); got != vnc.CloseLeaseEnded {
		t.Fatalf("socket close = %d (%v), want %d", got, err, vnc.CloseLeaseEnded)
	}
	select {
	case <-e.ctlUp.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("the upstream VNC connection of the fenced lease stayed open")
	}
	_ = c.Write(context.Background(), websocket.MessageBinary, []byte("typed-after")) // must go nowhere
	waitUntil(t, "takeover READY", func() bool { s, _ := e.takeoverStatus(); return s == "READY" })

	var h handoffView
	e.call("GET", base+"/takeover", operatorTok, nil, nil).into(t, &h)
	r := e.acquire("HUMAN", "actor:bob", 0, map[string]string{headerTakeoverID: h.RequestID})
	e.expect("new human", r, 201, "")
	var bob apiwire.LeaseGrant
	r.into(t, &bob)
	time.Sleep(50 * time.Millisecond)
	if got := e.ctlUp.got(); got != "typed-before" {
		t.Fatalf("x11vnc received %q, want only the bytes sent before the takeover", got)
	}

	// The old holder cannot get back in; the new one can.
	if _, resp, err := e.dial(base+"/vnc/control", human.CapabilityToken, nil); err == nil || resp == nil || resp.StatusCode != 409 {
		t.Fatalf("old token reconnect = %v %v, want 409", err, resp)
	}
	c2, _, err := e.dial(base+"/vnc/control", bob.CapabilityToken, nil)
	if err != nil {
		t.Fatalf("new holder: %v", err)
	}
	defer c2.CloseNow()
	mustWrite(t, c2, "bob")
	if got, err := readWS(t, c2); err != nil || got != "echo:bob" {
		t.Fatalf("echo = %q, %v", got, err)
	}
}

// ---- VNC proxies -----------------------------------------------------------

func TestVNCViewIsOperatorOnlyAndNeverReachesTheControlInstance(t *testing.T) {
	e := newCEnv(t)
	human := e.grant("HUMAN", "actor:alice")

	if _, resp, err := e.dial(base+"/vnc/view", "", nil); err == nil || resp == nil || resp.StatusCode != 401 {
		t.Fatalf("no credentials = %v %v, want 401", err, resp)
	}
	if _, resp, err := e.dial(base+"/vnc/view", human.CapabilityToken, nil); err == nil || resp == nil || resp.StatusCode != 401 {
		t.Fatalf("lease token = %v %v, want 401 (viewers are operator-authenticated)", err, resp)
	}
	if _, resp, err := e.dial(base+"/vnc/view", operatorTok, map[string]string{"Origin": "https://evil.test"}); err == nil || resp == nil || resp.StatusCode != 403 {
		t.Fatalf("Origin = %v %v, want 403", err, resp)
	}
	if _, resp, err := e.dial("/api/v1/workspaces/other/vnc/view", operatorTok, nil); err == nil || resp == nil || resp.StatusCode != 404 {
		t.Fatalf("other workspace = %v %v, want 404", err, resp)
	}

	c, _, err := e.dial(base+"/vnc/view", operatorTok, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	// A viewer that sends key/pointer/clipboard messages talks to the
	// view-only instance; the control instance never sees a byte.
	mustWrite(t, c, "\x04\x01\x00\x00\x00\x00\xff\x0dKEY-EVENT")
	if _, err := readWS(t, c); err != nil {
		t.Fatal(err)
	}
	if got := e.viewUp.got(); !strings.Contains(got, "KEY-EVENT") {
		t.Fatalf("view instance received %q", got)
	}
	if got := e.ctlUp.got(); got != "" {
		t.Fatalf("control instance received %q from a viewer", got)
	}

	// A lease change does not touch a viewer.
	if err := e.leases.Release(human.Lease.LeaseID); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, c, "still-here")
	if got, err := readWS(t, c); err != nil || got != "echo:still-here" {
		t.Fatalf("viewer after lease release = %q, %v", got, err)
	}
}

func TestVNCControlAdmitsOnlyTheBoundHumanOrAgentLease(t *testing.T) {
	e := newCEnv(t)
	agent := e.grant("AGENT", "actor:bot")
	status := func(token string, hdr map[string]string) int {
		t.Helper()
		_, resp, err := e.dial(base+"/vnc/control", token, hdr)
		if err == nil {
			t.Fatalf("dial with %q unexpectedly succeeded", token)
		}
		if resp == nil {
			t.Fatalf("dial error without a response: %v", err)
		}
		return resp.StatusCode
	}
	if got := status("", nil); got != 401 {
		t.Errorf("no token = %d, want 401", got)
	}
	if got := status(operatorTok, nil); got != 401 {
		t.Errorf("operator token = %d, want 401 (control needs a HUMAN or AGENT lease)", got)
	}
	if got := status(agent.CapabilityToken, map[string]string{"Origin": "https://evil.test"}); got != 403 {
		t.Errorf("AGENT Origin = %d, want 403", got)
	}
	if got := status(agent.CapabilityToken, map[string]string{headerEpoch: fmt.Sprint(agent.Lease.Epoch + 1)}); got != 412 {
		t.Errorf("AGENT stale epoch = %d, want 412", got)
	}
	agentHdr := map[string]string{headerEpoch: fmt.Sprint(agent.Lease.Epoch), headerGeneration: agent.Lease.RuntimeGeneration}
	ac, _, err := e.dial(base+"/vnc/control", agent.CapabilityToken, agentHdr)
	if err != nil {
		t.Fatalf("bound AGENT dial: %v", err)
	}
	defer ac.CloseNow()
	mustWrite(t, ac, "from-agent")
	if got, err := readWS(t, ac); err != nil || got != "echo:from-agent" {
		t.Fatalf("AGENT echo = %q, %v", got, err)
	}
	e.expect("agent release", e.call("DELETE", base+"/leases/"+agent.Lease.LeaseID, agent.CapabilityToken, nil, nil), 204, "")
	if _, err := readWS(t, ac); websocket.CloseStatus(err) != vnc.CloseLeaseEnded {
		t.Fatalf("AGENT close after release = %d (%v)", websocket.CloseStatus(err), err)
	}
	beforeRejected := e.ctlUp.got()
	human := e.grant("HUMAN", "actor:alice")
	if got := status(human.CapabilityToken, map[string]string{"Origin": "https://evil.test"}); got != 403 {
		t.Errorf("Origin = %d, want 403", got)
	}
	if got := status(human.CapabilityToken, map[string]string{headerEpoch: fmt.Sprint(human.Lease.Epoch + 1)}); got != 412 {
		t.Errorf("stale epoch = %d, want 412", got)
	}
	if got := status(human.CapabilityToken, map[string]string{headerGeneration: "00000000-0000-4000-8000-000000000000"}); got != 412 {
		t.Errorf("stale generation = %d, want 412", got)
	}
	if got := status(human.CapabilityToken, map[string]string{headerEpoch: "abc"}); got != 400 {
		t.Errorf("malformed epoch = %d, want 400", got)
	}
	if got := e.ctlUp.got(); got != beforeRejected {
		t.Fatalf("a rejected connection reached x11vnc: %q", got)
	}

	hdr := map[string]string{headerEpoch: fmt.Sprint(human.Lease.Epoch), headerGeneration: human.Lease.RuntimeGeneration}
	c, _, err := e.dial(base+"/vnc/control", human.CapabilityToken, hdr)
	if err != nil {
		t.Fatalf("bound dial: %v", err)
	}
	defer c.CloseNow()
	// One control socket per lease.
	if got := status(human.CapabilityToken, nil); got != 429 {
		t.Errorf("second socket = %d, want 429", got)
	}
	mustWrite(t, c, "hello")
	if got, err := readWS(t, c); err != nil || got != "echo:hello" {
		t.Fatalf("echo = %q, %v", got, err)
	}

	// Release by the holder closes the socket with 4001.
	e.expect("release", e.call("DELETE", base+"/leases/"+human.Lease.LeaseID, human.CapabilityToken, nil, nil), 204, "")
	_, err = readWS(t, c)
	if got := websocket.CloseStatus(err); got != vnc.CloseLeaseEnded {
		t.Fatalf("close after release = %d (%v)", got, err)
	}
}

func TestVNCControlSocketClosesWith4002OnAGenerationChange(t *testing.T) {
	e := newCEnv(t)
	human := e.grant("HUMAN", "actor:alice")
	c, _, err := e.dial(base+"/vnc/control", human.CapabilityToken, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.leases.StartGeneration() // the browser crashed and was restarted
	_, err = readWS(t, c)
	if got := websocket.CloseStatus(err); got != vnc.CloseGenerationChanged {
		t.Fatalf("close = %d (%v), want %d", got, err, vnc.CloseGenerationChanged)
	}
	if _, resp, err := e.dial(base+"/vnc/control", human.CapabilityToken, nil); err == nil || resp == nil || resp.StatusCode != 409 {
		t.Fatalf("pre-restart token reconnect = %v %v, want 409", err, resp)
	}
}

func TestAgentVNCControlSocketEndsWhenTheAgentIsPausedOrDisabled(t *testing.T) {
	for name, end := range map[string]func(*cenv){
		"pause":      func(e *cenv) { e.leases.PauseAgent() },
		"novnc":      func(e *cenv) { e.leases.SetAgentMode(apiwire.AgentModeNOVNC) },
		"generation": func(e *cenv) { e.leases.StartGeneration() },
	} {
		t.Run(name, func(t *testing.T) {
			e := newCEnv(t)
			agent := e.grant("AGENT", "actor:bot")
			c, _, err := e.dial(base+"/vnc/control", agent.CapabilityToken, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer c.CloseNow()
			end(e)
			_, err = readWS(t, c)
			if got := websocket.CloseStatus(err); got != vnc.CloseLeaseEnded && got != vnc.CloseGenerationChanged {
				t.Fatalf("close = %d (%v), want 4001 or 4002", got, err)
			}
			if _, resp, err := e.dial(base+"/vnc/control", agent.CapabilityToken, nil); err == nil || resp == nil || resp.StatusCode < 400 {
				t.Fatalf("old agent token reconnect = %v %v, want a refusal", resp, err)
			}
		})
	}
}

func TestVNCRoutesAnswer502WhenTheProxyIsMissing(t *testing.T) {
	e := newCEnv(t)
	h := NewHandler(Deps{WorkspaceID: testWS, Operator: auth.DigestOf(operatorTok), Leases: e.leases, Health: e.health})
	srv := httptest.NewServer(h)
	defer srv.Close()
	human := e.grant("HUMAN", "actor:alice")
	for _, tc := range []struct{ path, token string }{{"/vnc/view", operatorTok}, {"/vnc/control", human.CapabilityToken}} {
		req, _ := http.NewRequest("GET", srv.URL+base+tc.path, nil)
		req.Header.Set("Authorization", "Bearer "+tc.token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 502 {
			t.Errorf("%s without proxy = %d, want 502", tc.path, resp.StatusCode)
		}
	}
}

// ---- capture -------------------------------------------------------------

func captureBody(lease string, allow bool) []byte {
	b := fmt.Sprintf(`{"targetId":"AAAA1111","allowInterrupt":%t`, allow)
	if lease != "" {
		b += fmt.Sprintf(`,"expectedLeaseId":%q`, lease)
	}
	return []byte(b + `}`)
}

func (e *cenv) capture(body []byte) cresp {
	e.t.Helper()
	return e.call("POST", base+"/captures/viewport", operatorTok, idem(), body)
}

func TestCaptureRestoresTheHolderAtANewEpochAndRequiresItOnReconnect(t *testing.T) {
	e := newCEnv(t)
	old := e.grant("HUMAN", "actor:alice")
	sock, _, err := e.dial(base+"/vnc/control", old.CapabilityToken, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Observe the workspace while the capture is in progress.
	var duringState, duringAcquire, duringToken int
	e.cap.hook = func() {
		var st stateView
		e.call("GET", base+"/state", operatorTok, nil, nil).into(t, &st)
		if st.Workspace.ControlState == "CAPTURE" && st.Capture && st.Lease == nil {
			duringState = 1
		}
		duringAcquire = e.acquire("AGENT", "actor:bot", 0, nil).status
		duringToken = e.call("GET", base+"/leases/current", old.CapabilityToken, nil, nil).status
	}

	r := e.capture(captureBody(old.Lease.LeaseID, true))
	e.expect("capture", r, 201, "")
	if duringState != 1 || duringAcquire != 409 || duringToken != 409 {
		t.Fatalf("during capture: state ok=%d acquire=%d token=%d, want CAPTURE/409/409", duringState, duringAcquire, duringToken)
	}
	if strings.Contains(string(r.body), "ldl_") || strings.Contains(string(r.body), "capabilityToken") {
		t.Fatalf("capture response leaks a token: %s", r.body)
	}
	var res capture.Result
	r.into(t, &res)
	if res.Source != "page-viewport" || res.Consistency != "stable-navigation" || res.Control.FinalState != "HUMAN" {
		t.Fatalf("result = %+v", res)
	}
	if res.Control.PriorLease == nil || res.Control.PriorLease.LeaseID != old.Lease.LeaseID || res.Control.PriorLease.HolderID != "actor:alice" || res.Control.PriorLease.Mode != "HUMAN" {
		t.Fatalf("priorLease = %+v", res.Control.PriorLease)
	}
	var restored apiwire.Lease
	if err := json.Unmarshal(res.Control.RestoredLease, &restored); err != nil {
		t.Fatalf("restoredLease %s: %v", res.Control.RestoredLease, err)
	}
	if restored.Epoch <= old.Lease.Epoch || restored.LeaseID == old.Lease.LeaseID || restored.HolderID != "actor:alice" ||
		restored.Mode != "HUMAN" || restored.RuntimeGeneration != old.Lease.RuntimeGeneration {
		t.Fatalf("restored %+v vs old %+v", restored, old.Lease)
	}
	if e.cap.opts.Activate {
		t.Fatal("activate was set without being asked")
	}

	// The control socket of the pre-capture grant was closed.
	_, err = readWS(t, sock)
	if got := websocket.CloseStatus(err); got != vnc.CloseLeaseEnded {
		t.Fatalf("pre-capture socket close = %d (%v)", got, err)
	}

	// Same token, new epoch: reconnects must present it.
	clip := hdrs(map[string]string{"Content-Type": "text/plain; charset=utf-8"})
	e.expect("clipboard without epoch", e.call("PUT", base+"/clipboard", old.CapabilityToken, clip, []byte("x")), 412, apiwire.CodeEpochMismatch)
	e.expect("clipboard with old epoch", e.call("PUT", base+"/clipboard", old.CapabilityToken,
		hdrs(clip, headerEpoch, fmt.Sprint(old.Lease.Epoch)), []byte("x")), 412, apiwire.CodeEpochMismatch)
	if _, resp, err := e.dial(base+"/vnc/control", old.CapabilityToken, nil); err == nil || resp == nil || resp.StatusCode != 412 {
		t.Fatalf("socket without epoch = %v %v, want 412", err, resp)
	}
	if _, resp, err := e.dial(base+"/vnc/control", old.CapabilityToken, map[string]string{headerEpoch: fmt.Sprint(old.Lease.Epoch)}); err == nil || resp == nil || resp.StatusCode != 412 {
		t.Fatalf("socket with the old epoch = %v %v, want 412", err, resp)
	}
	e.expect("old lease id renew", e.call("POST", base+"/leases/"+old.Lease.LeaseID+"/renew", old.CapabilityToken, nil,
		[]byte(fmt.Sprintf(`{"epoch":%d,"runtimeGeneration":%q}`, restored.Epoch, restored.RuntimeGeneration))), 404, apiwire.CodeNotFound)
	e.expect("old epoch renew", e.call("POST", base+"/leases/"+restored.LeaseID+"/renew", old.CapabilityToken, nil,
		[]byte(fmt.Sprintf(`{"epoch":%d,"runtimeGeneration":%q}`, old.Lease.Epoch, restored.RuntimeGeneration))), 412, apiwire.CodeEpochMismatch)

	// The holder learns the new epoch and carries on.
	var cur apiwire.Lease
	e.call("GET", base+"/leases/current", old.CapabilityToken, nil, nil).into(t, &cur)
	if cur.LeaseID != restored.LeaseID || cur.Epoch != restored.Epoch {
		t.Fatalf("leases/current = %+v, want the restored lease", cur)
	}
	good := hdrs(clip, headerEpoch, fmt.Sprint(cur.Epoch), headerGeneration, cur.RuntimeGeneration)
	e.expect("clipboard with new epoch", e.call("PUT", base+"/clipboard", old.CapabilityToken, good, []byte("ok")), 204, "")
	c, _, err := e.dial(base+"/vnc/control", old.CapabilityToken, map[string]string{headerEpoch: fmt.Sprint(cur.Epoch)})
	if err != nil {
		t.Fatalf("socket with the new epoch: %v", err)
	}
	c.CloseNow()
	e.expect("renew new lease", e.call("POST", base+"/leases/"+cur.LeaseID+"/renew", old.CapabilityToken, nil,
		[]byte(fmt.Sprintf(`{"epoch":%d,"runtimeGeneration":%q}`, cur.Epoch, cur.RuntimeGeneration))), 200, "")
	// A normal lease (never restored) keeps working without the headers.
	if err := e.leases.Release(cur.LeaseID); err != nil {
		t.Fatal(err)
	}
	fresh := e.grant("HUMAN", "actor:bob")
	e.expect("fresh lease, no headers", e.call("PUT", base+"/clipboard", fresh.CapabilityToken, clip, []byte("m0")), 204, "")
}

func TestCaptureRestoresAnAgentHolderToo(t *testing.T) {
	e := newCEnv(t)
	agent := e.grant("AGENT", "actor:bot")
	inflight := e.startCDP(agent.CapabilityToken, nil)
	go func() { time.Sleep(50 * time.Millisecond); close(e.cdp.release) }()
	e.expect("capture", e.capture(captureBody(agent.Lease.LeaseID, true)), 201, "")
	<-inflight
	var cur apiwire.Lease
	e.call("GET", base+"/leases/current", agent.CapabilityToken, nil, nil).into(t, &cur)
	if cur.Mode != "AGENT" || cur.Epoch <= agent.Lease.Epoch {
		t.Fatalf("restored agent lease = %+v", cur)
	}
	// The agent's CDP route needs the new epoch as well.
	e.expect("cdp without epoch", e.call("GET", base+"/cdp/json/version", agent.CapabilityToken, nil, nil), 412, apiwire.CodeEpochMismatch)
	e.expect("cdp with old epoch", e.call("GET", base+"/cdp/json/version", agent.CapabilityToken, map[string]string{headerEpoch: fmt.Sprint(agent.Lease.Epoch)}, nil), 412, apiwire.CodeEpochMismatch)
	e.expect("cdp with new epoch", e.call("GET", base+"/cdp/json/version", agent.CapabilityToken, map[string]string{headerEpoch: fmt.Sprint(cur.Epoch)}, nil), 204, "")
}

func TestCaptureRefusesToInterruptWithoutConsentAndChecksTheExpectedLease(t *testing.T) {
	e := newCEnv(t)
	agent := e.grant("AGENT", "actor:bot")
	e.expect("no consent", e.capture(captureBody("", false)), 409, apiwire.CodeLeaseConflict)
	e.expect("wrong expected lease", e.capture(captureBody("lse_other", true)), 412, apiwire.CodeLeaseMismatch)
	if e.cap.callCount() != 0 {
		t.Fatal("a refused capture reached the browser")
	}
	var cur apiwire.Lease
	e.call("GET", base+"/leases/current", agent.CapabilityToken, nil, nil).into(t, &cur)
	if cur.LeaseID != agent.Lease.LeaseID || cur.Epoch != agent.Lease.Epoch {
		t.Fatalf("a refused capture disturbed the lease: %+v", cur)
	}
	// An empty expectedLeaseId means "no check", consent still required.
	e.expect("empty expected id", e.capture([]byte(`{"targetId":"AAAA1111","allowInterrupt":true,"expectedLeaseId":""}`)), 201, "")
}

func TestCaptureWithoutALeaseLeavesNothingBehind(t *testing.T) {
	e := newCEnv(t)
	r := e.capture(captureBody("", false))
	e.expect("capture", r, 201, "")
	var res capture.Result
	r.into(t, &res)
	if res.Control.PriorLease != nil || string(res.Control.RestoredLease) != "null" || res.Control.FinalState != "IDLE" {
		t.Fatalf("control = %+v", res.Control)
	}
	e.expect("expected lease but none held", e.capture(captureBody("lse_x", true)), 412, apiwire.CodeLeaseMismatch)
}

func TestCaptureDoesNotRestoreAReleasedOrExpiredLease(t *testing.T) {
	t.Run("holder releases during the capture", func(t *testing.T) {
		e := newCEnv(t)
		g := e.grant("AGENT", "actor:bot")
		e.cap.hook = func() {
			if r := e.call("DELETE", base+"/leases/"+g.Lease.LeaseID, g.CapabilityToken, nil, nil); r.status != 204 {
				t.Errorf("release during capture = %d %s", r.status, r.body)
			}
		}
		r := e.capture(captureBody(g.Lease.LeaseID, true))
		e.expect("capture", r, 201, "")
		var res capture.Result
		r.into(t, &res)
		if string(res.Control.RestoredLease) != "null" || res.Control.FinalState != "IDLE" || res.Control.PriorLease == nil {
			t.Fatalf("control = %+v", res.Control)
		}
		e.expect("token is dead", e.call("GET", base+"/leases/current", g.CapabilityToken, nil, nil), 409, apiwire.CodeLeaseEnded)
		if st := e.leases.Status(); st.Lease != nil {
			t.Fatal("no lease may be issued after the holder released")
		}
	})
	t.Run("ttl runs out during the capture", func(t *testing.T) {
		e := newCEnv(t)
		r := e.acquire("HUMAN", "actor:alice", 10, nil)
		var g apiwire.LeaseGrant
		r.into(t, &g)
		e.cap.hook = func() { e.clock.Advance(11 * time.Second) }
		r = e.capture(captureBody(g.Lease.LeaseID, true))
		e.expect("capture", r, 201, "")
		var res capture.Result
		r.into(t, &res)
		if string(res.Control.RestoredLease) != "null" || res.Control.FinalState != "IDLE" {
			t.Fatalf("control = %+v, want IDLE without a restored lease", res.Control)
		}
		if st := e.leases.Status(); st.Lease != nil {
			t.Fatal("no lease may be issued after the TTL ran out")
		}
	})
}

func TestFailedCaptureStillRestoresTheHolderAndMapsErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		err    error
		status int
		code   string
	}{
		"unknown target": {capture.ErrTargetNotFound, 404, apiwire.CodeTargetNotFound},
		"bad target":     {capture.ErrInvalidTarget, 400, apiwire.CodeInvalidRequest},
		"too large":      {capture.ErrImageLimit, 413, apiwire.CodeBodyTooLarge},
		"cdp failure":    {fmt.Errorf("%w: boom", capture.ErrCaptureFailed), 502, apiwire.CodeCaptureFailed},
		"other":          {errors.New("secret internal detail"), 502, apiwire.CodeCaptureFailed},
	} {
		t.Run(name, func(t *testing.T) {
			e := newCEnv(t)
			g := e.grant("HUMAN", "actor:alice")
			e.cap.err = tc.err
			r := e.capture(captureBody(g.Lease.LeaseID, true))
			e.expect("capture", r, tc.status, tc.code)
			if strings.Contains(string(r.body), "secret internal detail") {
				t.Fatalf("error detail leaked: %s", r.body)
			}
			var cur apiwire.Lease
			cr := e.call("GET", base+"/leases/current", g.CapabilityToken, nil, nil)
			e.expect("holder after failed capture", cr, 200, "")
			cr.into(t, &cur)
			if cur.Epoch <= g.Lease.Epoch {
				t.Fatalf("restored epoch %d must exceed %d", cur.Epoch, g.Lease.Epoch)
			}
			if st := e.leases.Status(); st.Capture {
				t.Fatal("a failed capture left the workspace in CAPTURE")
			}
		})
	}
}

func TestCaptureRefusesWhileAConnectionOfTheHolderIsStillOpen(t *testing.T) {
	e := newCEnv(t)
	human := e.grant("HUMAN", "actor:alice")
	// A tracked use that never ends stands in for a connection the runtime
	// cannot close: the capture must not proceed, must not grant anything,
	// and must not restore the fenced holder.
	_, release, err := e.leases.AuthorizeTracked(human.CapabilityToken, lease.ModeHuman)
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	e.expect("capture", e.capture(captureBody(human.Lease.LeaseID, true)), 409, apiwire.CodeLeaseConflict)
	if time.Since(start) > 3*time.Second {
		t.Fatal("the drain wait must be bounded by the drain timeout")
	}
	if e.cap.callCount() != 0 {
		t.Fatal("the browser was captured although the old holder had not drained")
	}
	if st := e.leases.Status(); st.Capture || st.Lease != nil || st.State != lease.StateIdle {
		t.Fatalf("workspace after a refused capture = %+v, want IDLE without a grant", st)
	}
	e.expect("fenced token", e.call("GET", base+"/leases/current", human.CapabilityToken, nil, nil), 409, apiwire.CodeLeaseEnded)
	release()
}

func TestCaptureRequestValidationAndIdempotency(t *testing.T) {
	e := newCEnv(t)
	e.expect("no key", e.call("POST", base+"/captures/viewport", operatorTok, nil, captureBody("", false)), 400, apiwire.CodeInvalidRequest)
	e.expect("no target", e.capture([]byte(`{"allowInterrupt":false}`)), 400, apiwire.CodeInvalidRequest)
	e.expect("unknown field", e.capture([]byte(`{"targetId":"A","source":"desktop"}`)), 400, apiwire.CodeInvalidRequest)
	e.expect("not json", e.capture([]byte(`nope`)), 400, apiwire.CodeInvalidRequest)
	e.expect("no auth", e.call("POST", base+"/captures/viewport", "", idem(), captureBody("", false)), 401, apiwire.CodeUnauthenticated)
	g := e.grant("HUMAN", "actor:alice")
	e.expect("lease token", e.call("POST", base+"/captures/viewport", g.CapabilityToken, idem(), captureBody("", false)), 401, apiwire.CodeUnauthenticated)
	if e.cap.callCount() != 0 {
		t.Fatal("rejected requests reached the browser")
	}
	if err := e.leases.Release(g.Lease.LeaseID); err != nil {
		t.Fatal(err)
	}

	key := map[string]string{"Idempotency-Key": "capture-key-1"}
	r1 := e.call("POST", base+"/captures/viewport", operatorTok, key, captureBody("", false))
	e.expect("first", r1, 201, "")
	r2 := e.call("POST", base+"/captures/viewport", operatorTok, key, captureBody("", false))
	e.expect("replay", r2, 201, "")
	if !bytes.Equal(r1.body, r2.body) || e.cap.callCount() != 1 {
		t.Fatalf("replay must return the stored metadata without capturing again (calls=%d)", e.cap.callCount())
	}
	e.expect("same key, other request", e.call("POST", base+"/captures/viewport", operatorTok, key,
		[]byte(`{"targetId":"BBBB2222","allowInterrupt":false}`)), 409, apiwire.CodeIdempotencyReplay)
	if e.cap.callCount() != 1 {
		t.Fatal("a conflicting replay captured again")
	}
	e.cap.opts = capture.Options{}
	e.expect("activate flag", e.call("POST", base+"/captures/viewport", operatorTok, idem(),
		[]byte(`{"targetId":"AAAA1111","allowInterrupt":false,"activate":true}`)), 201, "")
	if !e.cap.opts.Activate {
		t.Fatal("activate was not forwarded to the capturer")
	}
}

func TestCaptureImageIsOperatorOnlyBoundedAndDeletable(t *testing.T) {
	e := newCEnv(t)
	var res capture.Result
	e.capture(captureBody("", false)).into(t, &res)
	path := base + "/captures/viewport/" + res.CaptureID + "/image"

	e.expect("no auth", e.call("GET", path, "", nil, nil), 401, apiwire.CodeUnauthenticated)
	g := e.grant("HUMAN", "actor:alice")
	e.expect("lease token", e.call("GET", path, g.CapabilityToken, nil, nil), 401, apiwire.CodeUnauthenticated)
	r := e.call("GET", path, operatorTok, nil, nil)
	e.expect("image", r, 200, "")
	if !bytes.Equal(r.body, e.cap.png) || r.hdr.Get("Content-Type") != "image/png" || r.hdr.Get("Cache-Control") != "no-store" ||
		r.hdr.Get("X-Content-Type-Options") != "nosniff" || r.hdr.Get("Content-Length") != fmt.Sprint(len(e.cap.png)) {
		t.Fatalf("image response: type=%q cache=%q len=%q", r.hdr.Get("Content-Type"), r.hdr.Get("Cache-Control"), r.hdr.Get("Content-Length"))
	}
	e.expect("unknown id", e.call("GET", base+"/captures/viewport/rcap_nope/image", operatorTok, nil, nil), 404, apiwire.CodeNotFound)
	e.expect("delete", e.call("DELETE", path, operatorTok, nil, nil), 204, "")
	e.expect("gone", e.call("GET", path, operatorTok, nil, nil), 404, apiwire.CodeNotFound)
	e.expect("delete twice", e.call("DELETE", path, operatorTok, nil, nil), 404, apiwire.CodeNotFound)
}

func TestTargetsListing(t *testing.T) {
	e := newCEnv(t)
	vis := true
	e.cap.targets = []capture.Target{{ID: "AAAA1111", Type: "page", Title: "Home", URL: "https://app.test/", Visible: &vis}, {ID: "BBBB2222", Type: "page", Title: "Bg", URL: "https://b.test/"}}
	e.expect("no auth", e.call("GET", base+"/targets", "", nil, nil), 401, apiwire.CodeUnauthenticated)
	r := e.call("GET", base+"/targets", operatorTok, nil, nil)
	e.expect("targets", r, 200, "")
	var out struct{ Targets []capture.Target }
	r.into(t, &out)
	if len(out.Targets) != 2 || out.Targets[0].Visible == nil || !*out.Targets[0].Visible || out.Targets[1].Visible != nil {
		t.Fatalf("targets = %+v", out.Targets)
	}
	if !strings.Contains(string(r.body), `"visible":null`) {
		t.Fatalf("unknown visibility must be null: %s", r.body)
	}
	e.cap.targets = nil
	if r := e.call("GET", base+"/targets", operatorTok, nil, nil); !strings.Contains(string(r.body), `"targets":[]`) {
		t.Fatalf("empty listing = %s, want an empty array", r.body)
	}
	e.cap.err = errors.New("chromium down")
	r = e.call("GET", base+"/targets", operatorTok, nil, nil)
	e.expect("browser down", r, 502, apiwire.CodeUpstreamUnavailable)
	if strings.Contains(string(r.body), "chromium down") {
		t.Fatalf("upstream error text leaked: %s", r.body)
	}
}

// ---- maintenance ---------------------------------------------------------

func TestMaintenanceFencesDrainsAndBlocksGrantsUntilExit(t *testing.T) {
	e := newCEnv(t)
	human := e.grant("HUMAN", "actor:alice")
	sock, _, err := e.dial(base+"/vnc/control", human.CapabilityToken, nil)
	if err != nil {
		t.Fatal(err)
	}

	e.expect("no reason", e.call("POST", base+"/maintenance", operatorTok, nil, []byte(`{}`)), 400, apiwire.CodeInvalidRequest)
	e.expect("lease token", e.call("POST", base+"/maintenance", human.CapabilityToken, nil, []byte(`{"reason":"backup"}`)), 401, apiwire.CodeUnauthenticated)
	r := e.call("POST", base+"/maintenance", operatorTok, nil, []byte(`{"reason":"profile backup"}`))
	e.expect("enter", r, 200, "")
	var out struct {
		State             string
		Drained           bool
		RuntimeGeneration string
	}
	r.into(t, &out)
	if out.State != "MAINTENANCE" || out.RuntimeGeneration != human.Lease.RuntimeGeneration {
		t.Fatalf("maintenance = %+v", out)
	}
	// The fenced control socket is closed with 4001, and the state is final
	// whatever the drain outcome reported.
	if _, err := readWS(t, sock); websocket.CloseStatus(err) != vnc.CloseLeaseEnded {
		t.Fatalf("socket after maintenance: %v", err)
	}
	select {
	case <-e.ctlUp.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("the upstream VNC connection stayed open after maintenance was entered")
	}
	e.expect("acquire", e.acquire("AGENT", "actor:bot", 0, nil), 409, apiwire.CodeMaintenance)
	e.expect("takeover", e.call("POST", base+"/takeover", operatorTok, idem(), takeoverBody(false)), 409, apiwire.CodeMaintenance)
	e.expect("capture", e.capture(captureBody("", false)), 409, apiwire.CodeMaintenance)
	var ws apiwire.Workspace
	e.call("GET", base, operatorTok, nil, nil).into(t, &ws)
	if ws.ControlState != "MAINTENANCE" {
		t.Fatalf("state = %s", ws.ControlState)
	}
	// A browser restart does not end maintenance.
	e.leases.StartGeneration()
	e.expect("acquire after restart", e.acquire("AGENT", "actor:bot", 0, nil), 409, apiwire.CodeMaintenance)

	e.expect("exit", e.call("DELETE", base+"/maintenance", operatorTok, nil, nil), 204, "")
	e.expect("exit twice", e.call("DELETE", base+"/maintenance", operatorTok, nil, nil), 404, apiwire.CodeNotFound)
	e.expect("acquire after exit", e.acquire("AGENT", "actor:bot", 0, nil), 201, "")
}

func TestMaintenanceReportsAnUndrainedConnection(t *testing.T) {
	e := newCEnv(t)
	agent := e.grant("AGENT", "actor:bot")
	// A tracked use that never ends: maintenance must report that it could not
	// confirm a drain instead of claiming one.
	_, release, err := e.leases.AuthorizeTracked(agent.CapabilityToken, lease.ModeAgent)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	r := e.call("POST", base+"/maintenance", operatorTok, nil, []byte(`{"reason":"update"}`))
	e.expect("enter", r, 200, "")
	var out struct{ Drained bool }
	r.into(t, &out)
	if out.Drained {
		t.Fatal("maintenance claimed a drain while a connection was still open")
	}
	e.expect("acquire still blocked", e.acquire("AGENT", "actor:bot", 0, nil), 409, apiwire.CodeMaintenance)
}

// ---- files ---------------------------------------------------------------

func octet() map[string]string { return map[string]string{"Content-Type": "application/octet-stream"} }

func TestFileTransferRoundTripAndAuthorization(t *testing.T) {
	e := newCEnv(t)
	g := e.grant("HUMAN", "actor:alice")
	up := base + "/files/uploads/%E6%B8%AC%E8%A9%A6.txt" // 測試.txt

	e.expect("upload without auth", e.call("PUT", up, "", octet(), []byte("hi")), 401, apiwire.CodeUnauthenticated)
	e.expect("upload with lease token", e.call("PUT", up, g.CapabilityToken, octet(), []byte("hi")), 401, apiwire.CodeUnauthenticated)
	r := e.call("PUT", up, operatorTok, octet(), []byte("hello"))
	e.expect("upload", r, 201, "")
	var ent transfer.Entry
	r.into(t, &ent)
	if ent.Name != "測試.txt" || ent.Size != 5 {
		t.Fatalf("entry = %+v", ent)
	}
	if _, err := os.Stat(filepath.Join(e.filesDir, "uploads", "測試.txt")); err != nil {
		t.Fatalf("file not stored: %v", err)
	}
	e.expect("duplicate", e.call("PUT", up, operatorTok, octet(), []byte("again")), 409, apiwire.CodeFileExists)
	e.expect("overwrite", e.call("PUT", up+"?overwrite=1", operatorTok, octet(), []byte("replaced")), 201, "")
	e.expect("bad overwrite flag", e.call("PUT", up+"?overwrite=yes", operatorTok, octet(), []byte("x")), 400, apiwire.CodeInvalidRequest)

	// A Chromium download shows up in the downloads area.
	if err := os.WriteFile(filepath.Join(e.filesDir, "report.pdf"), []byte("%PDF"), 0o600); err != nil {
		t.Fatal(err)
	}
	r = e.call("GET", base+"/files?area=downloads", operatorTok, nil, nil)
	e.expect("list downloads", r, 200, "")
	var l transfer.Listing
	r.into(t, &l)
	if l.Area != transfer.AreaDownloads || len(l.Files) != 1 || l.Files[0].Name != "report.pdf" || l.Usage.QuotaBytes != 1<<20 || l.Usage.MaxFiles != 5 || l.Usage.FileCount != 2 {
		t.Fatalf("listing = %+v", l)
	}
	e.expect("list without area", e.call("GET", base+"/files", operatorTok, nil, nil), 400, apiwire.CodeInvalidRequest)
	e.expect("list bad area", e.call("GET", base+"/files?area=etc", operatorTok, nil, nil), 400, apiwire.CodeInvalidRequest)
	e.expect("list lease token", e.call("GET", base+"/files?area=uploads", g.CapabilityToken, nil, nil), 401, apiwire.CodeUnauthenticated)

	r = e.call("GET", base+"/files/downloads/report.pdf", operatorTok, nil, nil)
	e.expect("download", r, 200, "")
	if string(r.body) != "%PDF" || r.hdr.Get("Content-Type") != "application/octet-stream" || r.hdr.Get("X-Content-Type-Options") != "nosniff" ||
		!strings.HasPrefix(r.hdr.Get("Content-Disposition"), "attachment;") || r.hdr.Get("Content-Length") != "4" {
		t.Fatalf("download headers: %v", r.hdr)
	}
	r = e.call("GET", up, operatorTok, nil, nil)
	e.expect("download upload", r, 200, "")
	if string(r.body) != "replaced" || !strings.Contains(r.hdr.Get("Content-Disposition"), "filename*=UTF-8''%E6%B8%AC%E8%A9%A6.txt") {
		t.Fatalf("download = %q disposition %q", r.body, r.hdr.Get("Content-Disposition"))
	}
	e.expect("download missing", e.call("GET", base+"/files/downloads/nope", operatorTok, nil, nil), 404, apiwire.CodeNotFound)
	e.expect("download no auth", e.call("GET", base+"/files/downloads/report.pdf", "", nil, nil), 401, apiwire.CodeUnauthenticated)

	e.expect("delete", e.call("DELETE", up, operatorTok, nil, nil), 204, "")
	e.expect("delete again", e.call("DELETE", up, operatorTok, nil, nil), 404, apiwire.CodeNotFound)
	e.expect("delete no auth", e.call("DELETE", base+"/files/downloads/report.pdf", "", nil, nil), 401, apiwire.CodeUnauthenticated)

	// File names are user data: neither names nor tokens reach the log.
	if logs := e.logs.String(); strings.Contains(logs, "report.pdf") || strings.Contains(logs, "測試") || strings.Contains(logs, g.CapabilityToken) {
		t.Fatalf("log leaks user data: %s", logs)
	}
}

func TestFilePathsAreSingleSegmentsAndTraversalIsRejected(t *testing.T) {
	e := newCEnv(t)
	secret := filepath.Join(filepath.Dir(e.filesDir), "runtime-secret")
	if err := os.WriteFile(secret, []byte("do not serve"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(secret) })

	for _, name := range []string{"..%2Fruntime-secret", "%2E%2E%2Fruntime-secret", "..%5Cruntime-secret", "a%2Fb", "%2Fetc%2Fpasswd", ".hidden", "x%00y", "..", "%2E%2E"} {
		for _, area := range []string{"downloads", "uploads"} {
			p := base + "/files/" + area + "/" + name
			r := e.call("GET", p, operatorTok, nil, nil)
			if strings.Contains(string(r.body), "do not serve") || r.status == 200 {
				t.Errorf("GET %s served data (%d)", p, r.status)
			}
			if r.status != 400 && r.status != 404 && r.status != 301 {
				t.Errorf("GET %s = %d, want a rejection", p, r.status)
			}
			if d := e.call("DELETE", p, operatorTok, nil, nil); d.status == 204 {
				t.Errorf("DELETE %s succeeded", p)
			}
		}
		if r := e.call("PUT", base+"/files/uploads/"+name, operatorTok, octet(), []byte("x")); r.status == 201 {
			t.Errorf("PUT %s succeeded", name)
		}
	}
	if b, err := os.ReadFile(secret); err != nil || string(b) != "do not serve" {
		t.Fatalf("file outside the area was modified: %q, %v", b, err)
	}
	entries, _ := os.ReadDir(e.filesDir)
	for _, en := range entries {
		t.Errorf("unexpected entry %q in the data dir after rejected uploads", en.Name())
	}

	// The explicit name checks return the contract's code.
	r := e.call("GET", base+"/files/downloads/..%2Fx", operatorTok, nil, nil)
	e.expect("encoded traversal", r, 400, apiwire.CodeInvalidName)
	e.expect("leading dot", e.call("PUT", base+"/files/uploads/.hidden", operatorTok, octet(), []byte("x")), 400, apiwire.CodeInvalidName)
	e.expect("download area is read-only", e.call("PUT", base+"/files/downloads/x", operatorTok, octet(), []byte("x")), 400, apiwire.CodeInvalidRequest)
}

func TestUploadLimitsAndFraming(t *testing.T) {
	e := newCEnv(t)
	up := base + "/files/uploads/"

	e.expect("wrong content type", e.call("PUT", up+"a", operatorTok, map[string]string{"Content-Type": "text/plain"}, []byte("x")), 400, apiwire.CodeInvalidRequest)
	e.expect("content type with params", e.call("PUT", up+"a", operatorTok, map[string]string{"Content-Type": "application/octet-stream; charset=utf-8"}, []byte("x")), 400, apiwire.CodeInvalidRequest)
	e.expect("over per-file limit", e.call("PUT", up+"big", operatorTok, octet(), bytes.Repeat([]byte("x"), 64<<10+1)), 413, apiwire.CodeBodyTooLarge)

	// Chunked bodies have no Content-Length and are refused.
	req, _ := http.NewRequest("PUT", e.srv.URL+up+"chunked", io.NopCloser(strings.NewReader("data")))
	req.ContentLength = -1
	req.Header.Set("Authorization", "Bearer "+operatorTok)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := e.hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusLengthRequired {
		t.Fatalf("chunked upload = %d, want 411", resp.StatusCode)
	}

	// Quota and file count: 1 MiB quota, 64 KiB per file, 5 files.
	chunk := bytes.Repeat([]byte("q"), 60<<10)
	for i := range 5 {
		e.expect(fmt.Sprintf("file %d", i), e.call("PUT", up+fmt.Sprintf("f%d", i), operatorTok, octet(), chunk[:10<<10]), 201, "")
	}
	e.expect("sixth file", e.call("PUT", up+"f5", operatorTok, octet(), []byte("x")), 413, apiwire.CodeQuotaExceeded)
	entries, _ := os.ReadDir(filepath.Join(e.filesDir, "uploads"))
	if len(entries) != 5 {
		t.Fatalf("uploads dir has %d entries, want 5 (no temp files)", len(entries))
	}
}

// ---- resolution -----------------------------------------------------------

func TestResolutionNeedsTheHumanLeaseAndOnlyAppliesSupportedSizes(t *testing.T) {
	e := newCEnv(t)
	agent := e.grant("AGENT", "actor:bot")
	put := func(token string, body string, hdr map[string]string) cresp {
		return e.call("PUT", base+"/resolution", token, hdr, []byte(body))
	}
	e.expect("no token", put("", `{"width":1280,"height":720}`, nil), 401, apiwire.CodeUnauthenticated)
	e.expect("operator token", put(operatorTok, `{"width":1280,"height":720}`, nil), 401, apiwire.CodeUnauthenticated)
	e.expect("agent lease", put(agent.CapabilityToken, `{"width":1280,"height":720}`, nil), 403, apiwire.CodeWrongLeaseMode)
	if err := e.leases.Release(agent.Lease.LeaseID); err != nil {
		t.Fatal(err)
	}
	human := e.grant("HUMAN", "actor:alice")

	e.expect("bad body", put(human.CapabilityToken, `{"width":"wide"}`, nil), 400, apiwire.CodeInvalidRequest)
	e.expect("zero size", put(human.CapabilityToken, `{"width":0,"height":0}`, nil), 400, apiwire.CodeInvalidRequest)
	e.expect("unknown field", put(human.CapabilityToken, `{"width":1280,"height":720,"depth":8}`, nil), 400, apiwire.CodeInvalidRequest)
	e.expect("not allowed size", put(human.CapabilityToken, `{"width":640,"height":480}`, nil), 400, apiwire.CodeResizeUnsupported)
	e.expect("too large", put(human.CapabilityToken, `{"width":3840,"height":2160}`, nil), 400, apiwire.CodeResizeUnsupported)
	e.expect("stale epoch", put(human.CapabilityToken, `{"width":1280,"height":720}`, map[string]string{headerEpoch: "999"}), 412, apiwire.CodeEpochMismatch)
	if len(e.xr.set) != 0 {
		t.Fatalf("xrandr -s ran for a rejected request: %v", e.xr.set)
	}

	r := put(human.CapabilityToken, `{"width":1280,"height":720}`, nil)
	e.expect("resize", r, 200, "")
	var got resolutionBody
	r.into(t, &got)
	if got != (resolutionBody{Width: 1280, Height: 720}) || !slices.Equal(e.xr.set, []string{"1280x720"}) {
		t.Fatalf("resize = %+v, xrandr %v", got, e.xr.set)
	}
	var st stateView
	e.call("GET", base+"/state", operatorTok, nil, nil).into(t, &st)
	if st.Display != (vnc.Size{Width: 1280, Height: 720}) {
		t.Fatalf("state display = %+v, want the new size", st.Display)
	}

	r = e.call("GET", base+"/resolution", operatorTok, nil, nil)
	e.expect("get resolution", r, 200, "")
	var d vnc.Display
	r.into(t, &d)
	if d.Current != (vnc.Size{Width: 1280, Height: 720}) || !slices.Contains(d.Supported, vnc.Size{Width: 1920, Height: 1080}) || len(d.Supported) != len(vnc.AllowedSizes) {
		t.Fatalf("display = %+v", d)
	}
	e.expect("get without auth", e.call("GET", base+"/resolution", "", nil, nil), 401, apiwire.CodeUnauthenticated)
	e.expect("get with lease token", e.call("GET", base+"/resolution", human.CapabilityToken, nil, nil), 401, apiwire.CodeUnauthenticated)
}

// ---- routing -------------------------------------------------------------

func TestWrongMethodsOnNewPathsAre405WithAllow(t *testing.T) {
	e := newCEnv(t)
	for _, tt := range []struct{ method, path, allow string }{
		{"PUT", "/takeover", "POST, GET, HEAD, DELETE"},
		{"PATCH", "/takeover", "POST, GET, HEAD, DELETE"},
		{"GET", "/maintenance", "POST, DELETE"},
		{"POST", "/state", "GET, HEAD"},
		{"POST", "/resolution", "GET, HEAD, PUT"},
		{"DELETE", "/resolution", "GET, HEAD, PUT"},
		{"POST", "/vnc/view", "GET, HEAD"},
		{"PUT", "/vnc/control", "GET, HEAD"},
		{"GET", "/captures/viewport", "POST"},
		{"PUT", "/captures/viewport/x/image", "GET, HEAD, DELETE"},
		{"POST", "/targets", "GET, HEAD"},
		{"POST", "/files", "GET, HEAD"},
		{"PATCH", "/files/uploads/x", "GET, HEAD, PUT, DELETE"},
		{"POST", "/leases/current", "DELETE"},
	} {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			r := e.call(tt.method, base+tt.path, operatorTok, nil, nil)
			e.expect("wrong method", r, http.StatusMethodNotAllowed, apiwire.CodeMethodNotAllowed)
			if got := r.hdr.Get("Allow"); got != tt.allow {
				t.Errorf("Allow = %q, want %q", got, tt.allow)
			}
		})
	}
}

func TestNewRoutesRejectTheWrongWorkspace(t *testing.T) {
	e := newCEnv(t)
	other := "/api/v1/workspaces/other"
	g := e.grant("HUMAN", "actor:alice")
	for _, tc := range []struct{ method, path, token string }{
		{"GET", "/state", operatorTok}, {"POST", "/takeover", operatorTok}, {"GET", "/takeover", operatorTok},
		{"POST", "/maintenance", operatorTok}, {"GET", "/targets", operatorTok}, {"POST", "/captures/viewport", operatorTok},
		{"GET", "/captures/viewport/x/image", operatorTok}, {"GET", "/files?area=downloads", operatorTok},
		{"GET", "/resolution", operatorTok}, {"PUT", "/resolution", g.CapabilityToken}, {"GET", "/leases/current", g.CapabilityToken},
		{"PUT", "/clipboard", g.CapabilityToken}, {"GET", "/vnc/view", operatorTok}, {"GET", "/vnc/control", g.CapabilityToken},
	} {
		r := e.call(tc.method, other+tc.path, tc.token, hdrs(idem(), "Content-Type", "text/plain; charset=utf-8"), []byte("{}"))
		if r.status != 404 || r.code() != apiwire.CodeNotFound {
			t.Errorf("%s %s on another workspace = %d %s, want 404 NOT_FOUND", tc.method, tc.path, r.status, r.code())
		}
	}
}

func TestHealthHandlerExposesOnlyHealthAndReadiness(t *testing.T) {
	h := &fakeHealth{ready: true}
	srv := httptest.NewServer(NewHealthHandler(h, nil))
	defer srv.Close()
	get := func(method, path string) (int, string) {
		req, _ := http.NewRequest(method, srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+operatorTok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, strings.TrimSpace(string(b))
	}
	if code, body := get("GET", "/readyz"); code != 200 || body != `{"ready":true}` {
		t.Fatalf("readyz = %d %s", code, body)
	}
	if code, body := get("GET", "/readyz?detail=1"); code != 200 || body != `{"ready":true}` {
		t.Fatalf("readyz detail must stay minimal on the plain listener: %d %s", code, body)
	}
	h.setReady(false)
	if code, _ := get("GET", "/readyz"); code != 503 {
		t.Fatalf("not ready = %d, want 503", code)
	}
	if code, _ := get("GET", "/healthz"); code != 200 {
		t.Fatalf("healthz = %d", code)
	}
	for _, p := range []string{base, base + "/state", base + "/leases", base + "/vnc/view", base + "/files?area=downloads", "/"} {
		if code, _ := get("GET", p); code != 404 {
			t.Errorf("GET %s on the health listener = %d, want 404", p, code)
		}
	}
	if code, _ := get("POST", base+"/leases"); code != 404 {
		t.Errorf("POST /leases on the health listener = %d, want 404", code)
	}
}
