package runtimeapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
	"github.com/JiaWeiXie/lensdock/internal/auth"
	"github.com/JiaWeiXie/lensdock/internal/clipboard"
	"github.com/JiaWeiXie/lensdock/internal/lease"
	"github.com/JiaWeiXie/lensdock/internal/supervisor"
)

const (
	testWS       = "default"
	operatorTok  = "ldo_test-operator-token"
	leasesPath   = "/api/v1/workspaces/default/leases"
	clipPath     = "/api/v1/workspaces/default/clipboard"
	workspaceURL = "/api/v1/workspaces/default"
)

// fakeClock is a deterministic lease.Clock.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	c       *fakeClock
	when    time.Time
	f       func()
	stopped bool
	fired   bool
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) lease.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{c: c, when: c.now.Add(d), f: f}
	c.timers = append(c.timers, t)
	return t
}

func (t *fakeTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	was := !t.stopped && !t.fired
	t.stopped = true
	return was
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	target := c.now.Add(d)
	c.mu.Unlock()
	for {
		c.mu.Lock()
		var due []*fakeTimer
		for _, t := range c.timers {
			if !t.stopped && !t.fired && !t.when.After(target) {
				due = append(due, t)
			}
		}
		if len(due) == 0 {
			if target.After(c.now) {
				c.now = target
			}
			c.mu.Unlock()
			return
		}
		sort.Slice(due, func(i, j int) bool { return due[i].when.Before(due[j].when) })
		t := due[0]
		t.fired = true
		if t.when.After(c.now) {
			c.now = t.when
		}
		c.mu.Unlock()
		t.f()
	}
}

type fakeHealth struct {
	mu    sync.Mutex
	ready bool
}

func (h *fakeHealth) Ready() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ready
}

func (h *fakeHealth) setReady(v bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ready = v
}

func (h *fakeHealth) Detail() supervisor.Detail {
	return supervisor.Detail{ChromiumVersion: "test-chromium"}
}

// fakeSpawner records the stdin of each clipboard owner. Processes block in
// Wait until killed.
type fakeSpawner struct {
	mu     sync.Mutex
	stdins [][]byte
	procs  []*fakeProc
}

type fakeProc struct {
	killed chan struct{}
	once   sync.Once
}

func (p *fakeProc) Wait() error { <-p.killed; return nil }
func (p *fakeProc) Kill() error { p.once.Do(func() { close(p.killed) }); return nil }

func (s *fakeSpawner) StartBrowserProcess(_ context.Context, _ []string, stdin io.Reader) (clipboard.Process, error) {
	data, err := io.ReadAll(stdin)
	if err != nil {
		return nil, err
	}
	p := &fakeProc{killed: make(chan struct{})}
	s.mu.Lock()
	s.stdins = append(s.stdins, data)
	s.procs = append(s.procs, p)
	s.mu.Unlock()
	return p, nil
}

// OutputBrowserProcess answers the readback with the newest live owner's
// value, as xclip -out would.
func (s *fakeSpawner) OutputBrowserProcess(_ context.Context, _ []string, _ int) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n := len(s.procs); n > 0 {
		select {
		case <-s.procs[n-1].killed:
		default:
			return append([]byte(nil), s.stdins[n-1]...), nil
		}
	}
	return nil, errors.New("exit status 1")
}

func (s *fakeSpawner) last() ([]byte, *fakeProc, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.procs) == 0 {
		return nil, nil, false
	}
	return s.stdins[len(s.stdins)-1], s.procs[len(s.procs)-1], true
}

type env struct {
	t       *testing.T
	h       http.Handler
	clock   *fakeClock
	leases  *lease.Manager
	health  *fakeHealth
	spawner *fakeSpawner
	logs    *bytes.Buffer
	cdpPath string // path last seen by the fake CDP handler
	cdpHit  int
	cdpHook func(http.ResponseWriter, *http.Request)
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, clock: newFakeClock(), health: &fakeHealth{ready: true}, spawner: &fakeSpawner{}, logs: &bytes.Buffer{}}
	m, err := lease.NewManager(lease.Config{WorkspaceID: testWS, Clock: e.clock})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	m.SetLocked(false, "")
	e.leases = m
	cdp := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.cdpPath = r.URL.Path
		e.cdpHit++
		if e.cdpHook != nil {
			e.cdpHook(w, r)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusNoContent)
	})
	log := slog.New(slog.NewJSONHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e.h = NewHandler(Deps{
		WorkspaceID: testWS,
		Operator:    auth.DigestOf(operatorTok),
		Leases:      m,
		CDP:         cdp,
		Clipboard:   clipboard.New(e.spawner, time.Hour, 0),
		Health:      e.health,
		Logger:      log,
	})
	t.Cleanup(m.Shutdown)
	return e
}

type resp struct {
	*httptest.ResponseRecorder
}

func (r resp) errCode() string {
	var we apiwire.Error
	if err := json.Unmarshal(r.Body.Bytes(), &we); err != nil {
		return ""
	}
	return we.Code
}

func (e *env) do(method, path, token string, hdr map[string]string, body []byte) resp {
	e.t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return resp{rec}
}

var seq int

func (e *env) acquire(token string, mode string, ttl int) resp {
	seq++
	body := `{"mode":"` + mode + `"`
	if ttl != 0 {
		body += `,"ttlSeconds":` + itoa(ttl)
	}
	body += `}`
	return e.do(http.MethodPost, leasesPath, token, map[string]string{"Idempotency-Key": "idem-" + itoa(seq)}, []byte(body))
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func (e *env) mustAcquire(mode string) apiwire.LeaseGrant {
	e.t.Helper()
	r := e.acquire(operatorTok, mode, 30)
	if r.Code != http.StatusCreated {
		e.t.Fatalf("acquire %s: status %d body %s", mode, r.Code, r.Body.String())
	}
	var g apiwire.LeaseGrant
	if err := json.Unmarshal(r.Body.Bytes(), &g); err != nil {
		e.t.Fatal(err)
	}
	return g
}

func expect(t *testing.T, name string, r resp, status int, code string) {
	t.Helper()
	if r.Code != status {
		t.Errorf("%s: status = %d, want %d (body %s)", name, r.Code, status, strings.TrimSpace(r.Body.String()))
		return
	}
	if code != "" && r.errCode() != code {
		t.Errorf("%s: code = %q, want %q", name, r.errCode(), code)
	}
}

func TestOperatorEndpointsAuthorization(t *testing.T) {
	e := newEnv(t)
	agent := e.mustAcquire("AGENT")
	lt := agent.CapabilityToken

	t.Run("workspace", func(t *testing.T) {
		expect(t, "operator", e.do("GET", workspaceURL, operatorTok, nil, nil), 200, "")
		expect(t, "no token", e.do("GET", workspaceURL, "", nil, nil), 401, apiwire.CodeUnauthenticated)
		expect(t, "lease token", e.do("GET", workspaceURL, lt, nil, nil), 401, apiwire.CodeUnauthenticated)
		expect(t, "garbage token", e.do("GET", workspaceURL, "nope", nil, nil), 401, apiwire.CodeUnauthenticated)
		expect(t, "wrong ws", e.do("GET", "/api/v1/workspaces/other", operatorTok, nil, nil), 404, apiwire.CodeNotFound)
		r := e.do("GET", workspaceURL, "", nil, nil)
		if r.Header().Get("WWW-Authenticate") != "Bearer" {
			t.Error("401 lacks WWW-Authenticate")
		}
	})
	t.Run("basic scheme is rejected", func(t *testing.T) {
		req := httptest.NewRequest("GET", workspaceURL, nil)
		req.Header.Set("Authorization", "Basic "+operatorTok)
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		if rec.Code != 401 {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})
	t.Run("acquire", func(t *testing.T) {
		idem := map[string]string{"Idempotency-Key": "k-auth"}
		body := []byte(`{"mode":"AGENT"}`)
		expect(t, "no token", e.do("POST", leasesPath, "", idem, body), 401, apiwire.CodeUnauthenticated)
		expect(t, "lease token", e.do("POST", leasesPath, lt, idem, body), 401, apiwire.CodeUnauthenticated)
		expect(t, "wrong ws", e.do("POST", "/api/v1/workspaces/other/leases", operatorTok, idem, body), 404, apiwire.CodeNotFound)
	})
}

func TestWorkspaceStatusReflectsLeaseAndReadiness(t *testing.T) {
	e := newEnv(t)
	var ws apiwire.Workspace
	decode := func() {
		t.Helper()
		r := e.do("GET", workspaceURL, operatorTok, nil, nil)
		ws = apiwire.Workspace{}
		if err := json.Unmarshal(r.Body.Bytes(), &ws); err != nil || r.Code != 200 {
			t.Fatalf("workspace: %d %v", r.Code, err)
		}
	}
	decode()
	if ws.ID != testWS || ws.ControlState != "IDLE" || ws.HolderID != nil || !ws.Ready || ws.RuntimeGeneration == "" {
		t.Errorf("idle workspace = %+v", ws)
	}
	g := e.mustAcquire("HUMAN")
	decode()
	if ws.ControlState != "HUMAN" || ws.HolderID == nil || *ws.HolderID != g.Lease.HolderID {
		t.Errorf("held workspace = %+v", ws)
	}
	e.health.setReady(false)
	decode()
	if ws.Ready {
		t.Error("workspace reports ready while the supervisor is not ready")
	}
	e.leases.SetLocked(true, apiwire.CodeSandboxPreflightFailed)
	decode()
	if ws.ControlState != "ERROR_LOCKED" {
		t.Errorf("locked control state = %q", ws.ControlState)
	}
}

func TestAcquireValidationAndIdempotency(t *testing.T) {
	e := newEnv(t)
	idem := func(k string) map[string]string { return map[string]string{"Idempotency-Key": k} }
	post := func(k string, body string) resp {
		return e.do("POST", leasesPath, operatorTok, idem(k), []byte(body))
	}

	t.Run("idempotency key shape", func(t *testing.T) {
		expect(t, "missing", e.do("POST", leasesPath, operatorTok, nil, []byte(`{"mode":"AGENT"}`)), 400, apiwire.CodeInvalidRequest)
		expect(t, "too long", post(strings.Repeat("k", 257), `{"mode":"AGENT"}`), 400, apiwire.CodeInvalidRequest)
		expect(t, "space", post("a b", `{"mode":"AGENT"}`), 400, apiwire.CodeInvalidRequest)
	})
	t.Run("body", func(t *testing.T) {
		expect(t, "bad mode", post("k1", `{"mode":"ROOT"}`), 400, apiwire.CodeInvalidRequest)
		expect(t, "lowercase mode", post("k1", `{"mode":"agent"}`), 400, apiwire.CodeInvalidRequest)
		expect(t, "empty body", post("k1", ``), 400, apiwire.CodeInvalidRequest)
		expect(t, "unknown field", post("k1", `{"mode":"AGENT","extra":1}`), 400, apiwire.CodeInvalidRequest)
		expect(t, "trailing data", post("k1", `{"mode":"AGENT"}{}`), 400, apiwire.CodeInvalidRequest)
		expect(t, "invalid utf8", post("k1", "{\"mode\":\"AGENT\",\"x\":\"\xff\"}"), 400, apiwire.CodeInvalidUTF8)
		expect(t, "too large", post("k1", `{"mode":"AGENT","pad":"`+strings.Repeat("a", 9<<10)+`"}`), 413, apiwire.CodeBodyTooLarge)
	})
	t.Run("ttl", func(t *testing.T) {
		for _, ttl := range []string{"0", "-5", "5", "121", "86401", "1e3"} {
			expect(t, "ttl "+ttl, post("k-ttl", `{"mode":"AGENT","ttlSeconds":`+ttl+`}`), 400, apiwire.CodeInvalidRequest)
		}
	})
	t.Run("replay and conflict", func(t *testing.T) {
		first := post("same-key", `{"mode":"AGENT","ttlSeconds":20}`)
		if first.Code != 201 {
			t.Fatalf("first acquire status %d", first.Code)
		}
		var g apiwire.LeaseGrant
		if err := json.Unmarshal(first.Body.Bytes(), &g); err != nil {
			t.Fatal(err)
		}
		if g.Lease.HolderID != "operator" || g.Lease.Mode != "AGENT" || !strings.HasPrefix(g.CapabilityToken, "ldl_") {
			t.Errorf("grant = %+v", g.Lease)
		}
		if got := g.Lease.ExpiresAt.Sub(e.clock.Now()); got != 20*time.Second {
			t.Errorf("ttl = %v, want 20s", got)
		}
		if cc := first.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("Cache-Control = %q on a token response", cc)
		}
		replay := post("same-key", `{"mode":"AGENT","ttlSeconds":20}`)
		expect(t, "replay", replay, 409, apiwire.CodeIdempotencyReplay)
		if strings.Contains(replay.Body.String(), g.CapabilityToken) {
			t.Error("replay response re-issued the token")
		}
		expect(t, "other key", post("other-key", `{"mode":"AGENT"}`), 409, apiwire.CodeLeaseConflict)
	})
}

func TestAcquireWhileLocked(t *testing.T) {
	e := newEnv(t)
	e.leases.SetLocked(true, apiwire.CodeSandboxPreflightFailed)
	r := e.acquire(operatorTok, "AGENT", 0)
	expect(t, "locked", r, 409, apiwire.CodeSandboxPreflightFailed)
	e.leases.SetLocked(true, "")
	expect(t, "default lock code", e.acquire(operatorTok, "AGENT", 0), 409, apiwire.CodeRuntimeNotReady)
	e.leases.SetLocked(false, "")
	expect(t, "unlocked", e.acquire(operatorTok, "AGENT", 0), 201, "")
}

func TestLockEndsLeaseAndRefusesRenewAndRelease(t *testing.T) {
	e := newEnv(t)
	g := e.mustAcquire("AGENT")
	renewPath := leasesPath + "/" + g.Lease.LeaseID + "/renew"
	renewBody := []byte(`{"epoch":` + itoa(int(g.Lease.Epoch)) + `,"runtimeGeneration":"` + g.Lease.RuntimeGeneration + `"}`)

	e.leases.SetLocked(true, apiwire.CodeSandboxPreflightFailed)
	expect(t, "renew while locked", e.do("POST", renewPath, g.CapabilityToken, nil, renewBody), 409, apiwire.CodeSandboxPreflightFailed)
	expect(t, "release of locked-out lease", e.do("DELETE", leasesPath+"/"+g.Lease.LeaseID, g.CapabilityToken, nil, nil), 409, apiwire.CodeLeaseEnded)
	expect(t, "operator release of locked-out lease", e.do("DELETE", leasesPath+"/"+g.Lease.LeaseID, operatorTok, nil, nil), 404, apiwire.CodeNotFound)

	e.leases.SetLocked(false, "")
	expect(t, "old token after unlock", e.do("POST", renewPath, g.CapabilityToken, nil, renewBody), 409, apiwire.CodeLeaseEnded)
	expect(t, "acquire after unlock", e.acquire(operatorTok, "AGENT", 0), 201, "")
}

func TestRenew(t *testing.T) {
	e := newEnv(t)
	g := e.mustAcquire("AGENT")
	lt, id := g.CapabilityToken, g.Lease.LeaseID
	path := leasesPath + "/" + id + "/renew"
	renew := func(token string, epoch uint64, gen string) resp {
		b, _ := json.Marshal(apiwire.RenewLease{Epoch: epoch, RuntimeGeneration: gen})
		return e.do("POST", path, token, nil, b)
	}

	expect(t, "no token", renew("", g.Lease.Epoch, g.Lease.RuntimeGeneration), 401, apiwire.CodeUnauthenticated)
	expect(t, "operator token is not a lease token", renew(operatorTok, g.Lease.Epoch, g.Lease.RuntimeGeneration), 401, apiwire.CodeUnauthenticated)
	expect(t, "wrong epoch", renew(lt, g.Lease.Epoch+1, g.Lease.RuntimeGeneration), 412, apiwire.CodeEpochMismatch)
	expect(t, "wrong generation", renew(lt, g.Lease.Epoch, "stale"), 412, apiwire.CodeGenerationMismatch)
	expect(t, "wrong lease id", e.do("POST", leasesPath+"/lse_other/renew", lt, nil, []byte(`{"epoch":1,"runtimeGeneration":"x"}`)), 404, apiwire.CodeNotFound)
	expect(t, "wrong ws", e.do("POST", "/api/v1/workspaces/other/leases/"+id+"/renew", lt, nil, nil), 404, apiwire.CodeNotFound)
	expect(t, "bad body", e.do("POST", path, lt, nil, []byte(`{"epoch":"1"}`)), 400, apiwire.CodeInvalidRequest)

	e.clock.Advance(20 * time.Second)
	r := renew(lt, g.Lease.Epoch, g.Lease.RuntimeGeneration)
	expect(t, "renew", r, 200, "")
	var l apiwire.Lease
	if err := json.Unmarshal(r.Body.Bytes(), &l); err != nil {
		t.Fatal(err)
	}
	if want := e.clock.Now().Add(30 * time.Second); !l.ExpiresAt.Equal(want) {
		t.Errorf("renewed expiry = %v, want %v", l.ExpiresAt, want)
	}

	// The renewed lease survives past its original deadline, then expires.
	e.clock.Advance(20 * time.Second)
	expect(t, "renew again", renew(lt, g.Lease.Epoch, g.Lease.RuntimeGeneration), 200, "")
	e.clock.Advance(31 * time.Second)
	expect(t, "renew after expiry", renew(lt, g.Lease.Epoch, g.Lease.RuntimeGeneration), 409, apiwire.CodeLeaseEnded)

	// A new generation ends the lease; its token reports LEASE_ENDED.
	g2 := e.mustAcquire("AGENT")
	e.leases.StartGeneration()
	expect(t, "renew after generation change", e.do("POST", leasesPath+"/"+g2.Lease.LeaseID+"/renew", g2.CapabilityToken, nil,
		[]byte(`{"epoch":`+itoa(int(g2.Lease.Epoch))+`,"runtimeGeneration":"`+g2.Lease.RuntimeGeneration+`"}`)), 409, apiwire.CodeLeaseEnded)
}

func TestRelease(t *testing.T) {
	t.Run("by holder", func(t *testing.T) {
		e := newEnv(t)
		g := e.mustAcquire("AGENT")
		path := leasesPath + "/" + g.Lease.LeaseID
		expect(t, "no token", e.do("DELETE", path, "", nil, nil), 401, apiwire.CodeUnauthenticated)
		expect(t, "garbage token", e.do("DELETE", path, "ldl_unknown", nil, nil), 401, apiwire.CodeUnauthenticated)
		expect(t, "wrong lease id", e.do("DELETE", leasesPath+"/lse_other", g.CapabilityToken, nil, nil), 404, apiwire.CodeNotFound)
		expect(t, "wrong ws", e.do("DELETE", "/api/v1/workspaces/other/leases/"+g.Lease.LeaseID, g.CapabilityToken, nil, nil), 404, apiwire.CodeNotFound)
		expect(t, "release", e.do("DELETE", path, g.CapabilityToken, nil, nil), 204, "")
		expect(t, "second release by ended token", e.do("DELETE", path, g.CapabilityToken, nil, nil), 409, apiwire.CodeLeaseEnded)
		expect(t, "acquire after release", e.acquire(operatorTok, "AGENT", 0), 201, "")
	})
	t.Run("by operator", func(t *testing.T) {
		e := newEnv(t)
		g := e.mustAcquire("HUMAN")
		path := leasesPath + "/" + g.Lease.LeaseID
		expect(t, "unknown lease", e.do("DELETE", leasesPath+"/lse_other", operatorTok, nil, nil), 404, apiwire.CodeNotFound)
		expect(t, "release", e.do("DELETE", path, operatorTok, nil, nil), 204, "")
		expect(t, "release again", e.do("DELETE", path, operatorTok, nil, nil), 404, apiwire.CodeNotFound)
		if got := e.leases.Status().State; got != lease.StateIdle {
			t.Errorf("state after release = %s", got)
		}
	})
	t.Run("holder token of a replaced lease", func(t *testing.T) {
		e := newEnv(t)
		old := e.mustAcquire("AGENT")
		expect(t, "operator release", e.do("DELETE", leasesPath+"/"+old.Lease.LeaseID, operatorTok, nil, nil), 204, "")
		cur := e.mustAcquire("AGENT")
		expect(t, "old token cannot release the new lease", e.do("DELETE", leasesPath+"/"+cur.Lease.LeaseID, old.CapabilityToken, nil, nil), 409, apiwire.CodeLeaseEnded)
		if e.leases.Status().Lease == nil {
			t.Error("new lease was released by a stale token")
		}
	})
}

func TestClipboard(t *testing.T) {
	good := map[string]string{"Content-Type": "text/plain; charset=utf-8"}

	t.Run("authorization", func(t *testing.T) {
		e := newEnv(t)
		agent := e.mustAcquire("AGENT")
		expect(t, "no token", e.do("PUT", clipPath, "", good, []byte("x")), 401, apiwire.CodeUnauthenticated)
		expect(t, "operator token", e.do("PUT", clipPath, operatorTok, good, []byte("x")), 401, apiwire.CodeUnauthenticated)
		expect(t, "agent lease", e.do("PUT", clipPath, agent.CapabilityToken, good, []byte("x")), 403, apiwire.CodeWrongLeaseMode)
		expect(t, "wrong ws", e.do("PUT", "/api/v1/workspaces/other/clipboard", agent.CapabilityToken, good, []byte("x")), 404, apiwire.CodeNotFound)
		if _, _, ok := e.spawner.last(); ok {
			t.Error("clipboard owner was spawned for an unauthorized request")
		}
	})
	t.Run("lock ends the holder's lease", func(t *testing.T) {
		e := newEnv(t)
		human := e.mustAcquire("HUMAN")
		e.leases.SetLocked(true, "")
		expect(t, "old token after lock", e.do("PUT", clipPath, human.CapabilityToken, good, []byte("x")), 409, apiwire.CodeLeaseEnded)
		if _, _, ok := e.spawner.last(); ok {
			t.Error("clipboard owner was spawned after the lease was locked out")
		}
	})
	t.Run("content type", func(t *testing.T) {
		e := newEnv(t)
		human := e.mustAcquire("HUMAN")
		for _, ct := range []string{
			"", "text/plain", "text/plain; charset=latin1", "application/json", "text/html; charset=utf-8",
			"text/plain; charset=utf-8; format=flowed", "text/plain; charset=", "garbage;;",
		} {
			hdr := map[string]string{}
			if ct != "" {
				hdr["Content-Type"] = ct
			}
			expect(t, "content type "+ct, e.do("PUT", clipPath, human.CapabilityToken, hdr, []byte("x")), 400, apiwire.CodeInvalidRequest)
		}
		for _, ct := range []string{"text/plain; charset=utf-8", "text/plain; charset=UTF-8", `text/plain; charset="utf-8"`, "TEXT/PLAIN;charset=utf-8"} {
			expect(t, "content type "+ct, e.do("PUT", clipPath, human.CapabilityToken, map[string]string{"Content-Type": ct}, []byte("x")), 204, "")
		}
	})
	t.Run("text validation", func(t *testing.T) {
		e := newEnv(t)
		human := e.mustAcquire("HUMAN")
		put := func(b []byte) resp { return e.do("PUT", clipPath, human.CapabilityToken, good, b) }
		expect(t, "invalid utf8", put([]byte("ok \xff\xfe")), 400, apiwire.CodeInvalidUTF8)
		expect(t, "truncated sequence", put([]byte("\xe4\xb8")), 400, apiwire.CodeInvalidUTF8)
		expect(t, "NUL", put([]byte("a\x00b")), 400, apiwire.CodeInvalidRequest)
		expect(t, "one byte over limit", put(bytes.Repeat([]byte("a"), 64*1024+1)), 413, apiwire.CodeBodyTooLarge)
		expect(t, "exactly at limit", put(bytes.Repeat([]byte("a"), 64*1024)), 204, "")
	})
	t.Run("oversized without content length", func(t *testing.T) {
		e := newEnv(t)
		human := e.mustAcquire("HUMAN")
		req := httptest.NewRequest("PUT", clipPath, io.MultiReader(bytes.NewReader(bytes.Repeat([]byte("a"), 64*1024)), bytes.NewReader([]byte("b"))))
		req.ContentLength = -1
		req.Header.Set("Authorization", "Bearer "+human.CapabilityToken)
		req.Header.Set("Content-Type", "text/plain; charset=utf-8")
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		if rec.Code != 413 {
			t.Errorf("status = %d, want 413", rec.Code)
		}
	})
	t.Run("bytes are preserved and cleared with the lease", func(t *testing.T) {
		e := newEnv(t)
		human := e.mustAcquire("HUMAN")
		text := []byte("e\u0301 \U0001F468\u200D\U0001F469\u200D\U0001F467 \u7e41\u9ad4\u4e2d\u6587 \r\n\ttrailing  ")
		expect(t, "set", e.do("PUT", clipPath, human.CapabilityToken, good, text), 204, "")
		got, proc, ok := e.spawner.last()
		if !ok || !bytes.Equal(got, text) {
			t.Fatalf("owner stdin = %q (spawned %v), want %q", got, ok, text)
		}
		select {
		case <-proc.killed:
			t.Fatal("clipboard owner killed before the lease ended")
		default:
		}
		expect(t, "release", e.do("DELETE", leasesPath+"/"+human.Lease.LeaseID, human.CapabilityToken, nil, nil), 204, "")
		select {
		case <-proc.killed:
		case <-time.After(2 * time.Second):
			t.Fatal("clipboard owner was not killed when the lease ended")
		}
	})
}

func TestReadyz(t *testing.T) {
	e := newEnv(t)
	r := e.do("GET", "/readyz", "", nil, nil)
	if r.Code != 200 || strings.TrimSpace(r.Body.String()) != `{"ready":true}` {
		t.Errorf("ready: %d %s", r.Code, r.Body.String())
	}
	e.health.setReady(false)
	r = e.do("GET", "/readyz", "", nil, nil)
	if r.Code != 503 || strings.TrimSpace(r.Body.String()) != `{"ready":false}` {
		t.Errorf("not ready: %d %s", r.Code, r.Body.String())
	}
	if r := e.do("GET", "/healthz", "", nil, nil); r.Code != 200 || strings.TrimSpace(r.Body.String()) != `{"ok":true}` {
		t.Errorf("healthz: %d %s", r.Code, r.Body.String())
	}

	e.health.setReady(true)
	g := e.mustAcquire("AGENT")
	expect(t, "detail without credentials", e.do("GET", "/readyz?detail=1", "", nil, nil), 401, apiwire.CodeUnauthenticated)
	expect(t, "detail with lease token", e.do("GET", "/readyz?detail=1", g.CapabilityToken, nil, nil), 401, apiwire.CodeUnauthenticated)
	r = e.do("GET", "/readyz?detail=1", operatorTok, nil, nil)
	if r.Code != 200 {
		t.Fatalf("detail: %d %s", r.Code, r.Body.String())
	}
	var out struct {
		Ready  bool `json:"ready"`
		Lease  *apiwire.Lease
		Detail struct {
			ChromiumVersion string `json:"chromiumVersion"`
		} `json:"detail"`
		Workspace apiwire.Workspace `json:"workspace"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Ready || out.Detail.ChromiumVersion != "test-chromium" || out.Lease == nil || out.Lease.LeaseID != g.Lease.LeaseID || out.Workspace.ControlState != "AGENT" {
		t.Errorf("detail body = %s", r.Body.String())
	}
	if strings.Contains(r.Body.String(), g.CapabilityToken) {
		t.Error("detail leaks the lease token")
	}
	e.health.setReady(false)
	if r := e.do("GET", "/readyz?detail=1", operatorTok, nil, nil); r.Code != 503 {
		t.Errorf("detail while not ready: status %d, want 503", r.Code)
	}
}

func TestCDPMount(t *testing.T) {
	e := newEnv(t)
	r := e.do("GET", "/api/v1/workspaces/default/cdp/json/version", "", nil, nil)
	if r.Code != 204 || e.cdpPath != "/json/version" {
		t.Errorf("mount: status %d, upstream path %q", r.Code, e.cdpPath)
	}
	hits := e.cdpHit
	expect(t, "wrong ws", e.do("GET", "/api/v1/workspaces/other/cdp/json/version", "", nil, nil), 404, apiwire.CodeNotFound)
	if e.cdpHit != hits {
		t.Error("CDP handler reached for another workspace")
	}
}

func TestErrorsAreJSONWithRequestID(t *testing.T) {
	e := newEnv(t)
	idRe := regexp.MustCompile(`^[0-9a-f]{32}$`)
	seen := map[string]bool{}
	for _, c := range []struct{ method, path string }{
		{"GET", "/nope"}, {"GET", "/api/v1/workspaces"}, {"GET", workspaceURL}, {"PATCH", workspaceURL}, {"GET", "/healthz"},
	} {
		r := e.do(c.method, c.path, "", map[string]string{"X-Request-Id": "client-chosen"}, nil)
		id := r.Header().Get("X-Request-Id")
		if !idRe.MatchString(id) || seen[id] {
			t.Errorf("%s %s: request id %q is not fresh random hex", c.method, c.path, id)
		}
		seen[id] = true
		if r.Code == 200 {
			continue
		}
		var we apiwire.Error
		if err := json.Unmarshal(r.Body.Bytes(), &we); err != nil || we.Code == "" || we.RequestID != id {
			t.Errorf("%s %s: body %q is not an apiwire.Error carrying the request id", c.method, c.path, r.Body.String())
		}
		if ct := r.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("%s %s: content type %q", c.method, c.path, ct)
		}
	}
}

func TestWrongMethodOnKnownPathIs405(t *testing.T) {
	e := newEnv(t)
	tests := []struct {
		method, path, allow string
	}{
		{"PATCH", workspaceURL, "GET, HEAD"},
		{"POST", workspaceURL, "GET, HEAD"},
		{"GET", leasesPath, "POST"},
		{"DELETE", leasesPath, "POST"},
		{"GET", leasesPath + "/lse_x/renew", "POST"},
		{"POST", leasesPath + "/lse_x", "DELETE"},
		{"GET", leasesPath + "/lse_x", "DELETE"},
		{"POST", clipPath, "PUT"},
		{"GET", clipPath, "PUT"},
		{"POST", "/healthz", "GET, HEAD"},
		{"PUT", "/readyz", "GET, HEAD"},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			r := e.do(tt.method, tt.path, operatorTok, nil, nil)
			expect(t, "wrong method", r, http.StatusMethodNotAllowed, apiwire.CodeMethodNotAllowed)
			if got := r.Header().Get("Allow"); got != tt.allow {
				t.Errorf("Allow = %q, want %q", got, tt.allow)
			}
			if ct := r.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
		})
	}

	// Unknown paths stay 404, and the right method on a known path still routes.
	expect(t, "unknown path", e.do("PATCH", "/api/v1/nope", operatorTok, nil, nil), 404, apiwire.CodeNotFound)
	expect(t, "unknown lease sub-route", e.do("POST", leasesPath+"/lse_x/renew/extra", operatorTok, nil, nil), 404, apiwire.CodeNotFound)
	expect(t, "right method", e.do("GET", workspaceURL, operatorTok, nil, nil), 200, "")
	// The CDP mount forwards every method to the proxy.
	if r := e.do("POST", "/api/v1/workspaces/default/cdp/json/new", "", nil, nil); r.Code != 204 {
		t.Errorf("CDP POST: status %d, want the proxy's 204", r.Code)
	}
}

func TestPanicIsRecovered(t *testing.T) {
	e := newEnv(t)
	e.cdpHook = func(http.ResponseWriter, *http.Request) { panic("boom with SECRET-PANIC-VALUE") }
	r := e.do("GET", "/api/v1/workspaces/default/cdp/json/version", "", nil, nil)
	expect(t, "panic", r, 500, apiwire.CodeInternal)
	if strings.Contains(r.Body.String(), "SECRET-PANIC-VALUE") || strings.Contains(e.logs.String(), "SECRET-PANIC-VALUE") {
		t.Error("panic value leaked into the response or logs")
	}
	if !strings.Contains(e.logs.String(), "handler panic") {
		t.Error("panic was not logged")
	}
	expect(t, "next request", e.do("GET", "/healthz", "", nil, nil), 200, "")
}

func TestLogsNeverContainSecrets(t *testing.T) {
	e := newEnv(t)
	const (
		clipText    = "CLIPBOARD-SECRET-\u7e41\u9ad4-\U0001F512"
		cdpPayload  = "CDP-PAYLOAD-SECRET"
		cdpQuery    = "https://private.example/secret-path"
		garbageTok  = "ldl_garbage-token-not-issued"
		panicSecret = "PANIC-VALUE-SECRET"
	)
	agent := e.mustAcquire("AGENT")
	expect(t, "release agent", e.do("DELETE", leasesPath+"/"+agent.Lease.LeaseID, agent.CapabilityToken, nil, nil), 204, "")
	human := e.mustAcquire("HUMAN")
	good := map[string]string{"Content-Type": "text/plain; charset=utf-8"}

	e.do("GET", workspaceURL, operatorTok, nil, nil)
	e.do("GET", workspaceURL, garbageTok, nil, nil)
	e.do("POST", leasesPath+"/"+human.Lease.LeaseID+"/renew", human.CapabilityToken, nil,
		[]byte(`{"epoch":`+itoa(int(human.Lease.Epoch))+`,"runtimeGeneration":"`+human.Lease.RuntimeGeneration+`"}`))
	expect(t, "clipboard", e.do("PUT", clipPath, human.CapabilityToken, good, []byte(clipText)), 204, "")
	e.do("PUT", clipPath, human.CapabilityToken, good, []byte("bad \xff "+clipText))
	e.do("PUT", clipPath, garbageTok, good, []byte(clipText))
	e.do("POST", "/api/v1/workspaces/default/cdp/json/new?url="+cdpQuery, agent.CapabilityToken, nil, []byte(cdpPayload))
	e.cdpHook = func(http.ResponseWriter, *http.Request) { panic(panicSecret) }
	e.do("GET", "/api/v1/workspaces/default/cdp/json", human.CapabilityToken, nil, nil)
	e.do("DELETE", leasesPath+"/"+human.Lease.LeaseID, human.CapabilityToken, nil, nil)

	logs := e.logs.String()
	if !strings.Contains(logs, `"requestId"`) || !strings.Contains(logs, "lease acquired") {
		t.Fatalf("expected request and lease logs, got:\n%s", logs)
	}
	for name, secret := range map[string]string{
		"operator token": operatorTok, "agent lease token": agent.CapabilityToken, "human lease token": human.CapabilityToken,
		"garbage token": garbageTok, "clipboard text": clipText, "cdp payload": cdpPayload, "cdp query": cdpQuery,
		"panic value": panicSecret, "authorization scheme": "Bearer ",
	} {
		if strings.Contains(logs, secret) {
			t.Errorf("logs contain %s", name)
		}
	}
	if strings.Contains(logs, "\U0001F512") || strings.Contains(logs, "CLIPBOARD-SECRET") {
		t.Error("logs contain a clipboard fragment")
	}
}
