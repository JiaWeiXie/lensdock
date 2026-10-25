package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
	"github.com/JiaWeiXie/lensdock/internal/auth"
)

const modeBase = "/api/v1/workspaces/default/"

// modeRuntime is a runtime stand-in that owns the usage policy the way the
// real lease manager does, so the Gateway is tested only against its contract.
type modeRuntime struct {
	srv     *httptest.Server
	mu      sync.Mutex
	control apiwire.AgentControl
	broken  bool // GET .../agent answers 500
	noState bool // /state carries no agentControl
	hits    map[string]int
	putSeen chan string
}

func newModeRuntime(t *testing.T) *modeRuntime {
	t.Helper()
	m := &modeRuntime{
		control: apiwire.AgentControl{Mode: apiwire.AgentModeAGENT, Revision: 1, UpdatedAt: time.Unix(1, 0).UTC()},
		hits:    map[string]int{},
		putSeen: make(chan string, 8),
	}
	m.srv = httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *modeRuntime) set(f func(c *apiwire.AgentControl)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f(&m.control)
	m.control.Revision++
}

func (m *modeRuntime) hit(key string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hits[key]
}

func (m *modeRuntime) handle(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	m.hits[r.Method+" "+r.URL.Path]++
	c, broken, noState := m.control, m.broken, m.noState
	m.mu.Unlock()
	reply := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	switch {
	case r.URL.Path == modeBase+"agent" && r.Method == "GET":
		if broken {
			reply(500, map[string]string{"code": "INTERNAL"})
			return
		}
		reply(200, c)
	case r.URL.Path == modeBase+"agent" && r.Method == "PUT":
		var in apiwire.SetAgentMode
		_ = json.NewDecoder(r.Body).Decode(&in)
		m.putSeen <- in.Mode
		m.set(func(c *apiwire.AgentControl) { c.Mode, c.Paused = in.Mode, false })
		m.mu.Lock()
		c = m.control
		m.mu.Unlock()
		reply(200, c)
	case r.URL.Path == modeBase+"agent/pause" && r.Method == "POST":
		if c.Mode != apiwire.AgentModeAGENT {
			reply(409, apiwire.Error{Code: apiwire.CodeAgentModeDisabled})
			return
		}
		m.set(func(c *apiwire.AgentControl) { c.Paused = true })
		m.mu.Lock()
		c = m.control
		m.mu.Unlock()
		reply(200, c)
	case r.URL.Path == modeBase+"agent/resume" && r.Method == "POST":
		m.set(func(c *apiwire.AgentControl) { c.Paused = false })
		m.mu.Lock()
		c = m.control
		m.mu.Unlock()
		reply(200, c)
	case r.URL.Path == modeBase+"agent/events":
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		b, _ := json.Marshal(c)
		_, _ = io.WriteString(w, "event: agent-control\ndata: "+string(b)+"\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	case r.URL.Path == modeBase+"state":
		ws := map[string]any{"id": "default", "ready": true, "controlState": "IDLE"}
		if !noState {
			ws["agentControl"] = c
		}
		reply(200, map[string]any{"workspace": ws})
	default:
		_, _ = io.WriteString(w, "ok")
	}
}

type modeEnv struct {
	t        *testing.T
	g        *Gateway
	rt       *modeRuntime
	edge     *httptest.Server
	origin   string
	feedback atomic.Int32
	// epoch keeps seeded grants ordered, as the registry refuses an older one.
	epoch uint64
	// hook, set before a test sends requests, replaces the stub Feedback handler.
	hook func(w http.ResponseWriter, r *http.Request)
}

func newModeEnv(t *testing.T) *modeEnv {
	t.Helper()
	rt := newModeRuntime(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<title>t</title>"), 0o600); err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(dir, "token")
	if err := auth.WritePrivateFile(tokenFile, []byte("runtime-token")); err != nil {
		t.Fatal(err)
	}
	e := &modeEnv{t: t, rt: rt}
	e.edge = httptest.NewUnstartedServer(nil)
	t.Cleanup(e.edge.Close)
	e.origin = "http://" + e.edge.Listener.Addr().String()
	cfg := Config{Origin: e.origin, WebDir: dir,
		Identities: []Identity{
			{ID: "human", Role: "human", WorkspaceID: "default", TokenSHA256: digest("human-token")},
			{ID: "viewer", Role: "viewer", WorkspaceID: "default", TokenSHA256: digest("viewer-token")},
			{ID: "agent-1", Role: "agent", WorkspaceID: "default", TokenSHA256: digest("agent-token")},
		},
		Runtimes: []RuntimeConfig{{WorkspaceID: "default", URL: rt.srv.URL, OperatorTokenFile: tokenFile}}}
	g, err := New(cfg, Integrations{Feedback: func(w http.ResponseWriter, r *http.Request, p Identity) {
		e.feedback.Add(1)
		if e.hook != nil {
			e.hook(w, r)
			return
		}
		w.WriteHeader(200)
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.Close)
	e.g = g
	e.edge.Config.Handler = g
	e.edge.Start()
	return e
}

func (e *modeEnv) req(ctx context.Context, method, path, token, body string) *http.Request {
	r, err := http.NewRequestWithContext(ctx, method, e.edge.URL+path, bytes.NewBufferString(body))
	if err != nil {
		e.t.Fatal(err)
	}
	r.Header.Set("Origin", e.origin)
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

func (e *modeEnv) do(method, path, token, body string) (int, string) {
	e.t.Helper()
	resp, err := e.edge.Client().Do(e.req(context.Background(), method, path, token, body))
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (e *modeEnv) wantCode(method, path, token, body string, status int, code string) {
	e.t.Helper()
	got, b := e.do(method, path, token, body)
	if got != status || (code != "" && !strings.Contains(b, `"`+code+`"`)) {
		e.t.Fatalf("%s %s as %s = %d %s, want %d %s", method, path, token, got, b, status, code)
	}
}

func (e *modeEnv) wantStatus(method, path, token, body string, status int) {
	e.t.Helper()
	e.wantCode(method, path, token, body, status, "")
}

var collabRoutes = [][2]string{
	{"GET", "/api/v1/feedback?workspaceId=default"},
	{"POST", "/api/v1/feedback"},
	{"GET", "/api/v1/feedback/f1/bundle?revision=1"},
	{"GET", "/api/v1/feedback/f1/artifacts/feedback.json?revision=1"},
	{"GET", "/api/v1/captures/c1/image"},
	{"PUT", "/api/v1/captures/c1/image"},
	{"POST", "/api/v1/workspaces/default/captures"},
	{"GET", "/api/v1/events?workspaceId=default"},
}

func TestNOVNCClosesCollaborationForEveryRole(t *testing.T) {
	e := newModeEnv(t)
	for _, tok := range []string{"human-token", "viewer-token", "agent-token"} {
		for _, c := range collabRoutes[:len(collabRoutes)-1] {
			e.wantStatus(c[0], c[1], tok, `{}`, 200)
		}
	}
	before := e.feedback.Load()
	e.rt.set(func(c *apiwire.AgentControl) { c.Mode = apiwire.AgentModeNOVNC })
	for _, tok := range []string{"human-token", "viewer-token", "agent-token"} {
		for _, c := range collabRoutes {
			e.wantCode(c[0], c[1], tok, `{}`, 409, apiwire.CodeAgentModeDisabled)
		}
	}
	if got := e.feedback.Load(); got != before {
		t.Fatalf("%d collaboration requests reached storage in NOVNC", got-before)
	}
	// Ordinary browser operations of a human stay available.
	e.wantStatus("GET", modeBase+"files", "human-token", "", 200)
	e.wantStatus("GET", modeBase+"vnc/view", "human-token", "", 200)
	e.wantStatus("GET", "/api/v1/workspaces/default", "viewer-token", "", 200)
}

func TestUnknownPolicyFailsClosed(t *testing.T) {
	e := newModeEnv(t)
	e.rt.mu.Lock()
	e.rt.broken = true
	e.rt.mu.Unlock()
	for _, tok := range []string{"human-token", "viewer-token", "agent-token"} {
		for _, c := range collabRoutes {
			e.wantCode(c[0], c[1], tok, `{}`, 503, "UPSTREAM_UNAVAILABLE")
		}
	}
	for _, p := range []string{"targets", "resolution", "health", "cdp/json/version"} {
		e.wantCode("GET", modeBase+p, "agent-token", "", 503, "UPSTREAM_UNAVAILABLE")
	}
	e.wantCode("GET", modeBase+"agent/setup", "human-token", "", 503, "UPSTREAM_UNAVAILABLE")
	if e.feedback.Load() != 0 {
		t.Fatal("collaboration reached storage with an unknown policy")
	}
	// An unrecognised mode is as unknown as an error.
	e.rt.mu.Lock()
	e.rt.broken = false
	e.rt.control.Mode = "WIDE_OPEN"
	e.rt.mu.Unlock()
	e.wantCode("GET", "/api/v1/feedback", "human-token", "", 503, "UPSTREAM_UNAVAILABLE")
	e.wantCode("GET", modeBase+"targets", "agent-token", "", 503, "UPSTREAM_UNAVAILABLE")
	// A self-contradicting or incomplete policy is unknown too.
	for _, bad := range []func(c *apiwire.AgentControl){
		func(c *apiwire.AgentControl) { c.Mode, c.Paused = apiwire.AgentModeNOVNC, true },
		func(c *apiwire.AgentControl) { c.Mode, c.Paused, c.Revision = apiwire.AgentModeAGENT, false, 0 },
		func(c *apiwire.AgentControl) {
			c.Mode, c.Paused, c.UpdatedAt = apiwire.AgentModeAGENT, false, time.Time{}
		},
	} {
		e.rt.mu.Lock()
		e.rt.control = apiwire.AgentControl{Revision: 5, UpdatedAt: time.Unix(5, 0).UTC()}
		bad(&e.rt.control)
		e.rt.mu.Unlock()
		e.wantCode("GET", "/api/v1/feedback", "human-token", "", 503, "UPSTREAM_UNAVAILABLE")
		e.wantCode("GET", modeBase+"targets", "agent-token", "", 503, "UPSTREAM_UNAVAILABLE")
		if got, _ := e.do("GET", modeBase+"agent", "viewer-token", ""); got != 502 {
			t.Fatalf("malformed policy forwarded as %d", got)
		}
	}
	// A valid policy makes the state usable; without one it is offline, never AGENT.
	e.rt.mu.Lock()
	e.rt.control = apiwire.AgentControl{Mode: apiwire.AgentModeAGENT, Revision: 9, UpdatedAt: time.Unix(9, 0).UTC()}
	e.rt.mu.Unlock()
	if _, ok := e.do("GET", "/api/v1/workspaces/default", "viewer-token", ""); !strings.Contains(ok, `"ready":true`) || !strings.Contains(ok, `"mode":"AGENT"`) {
		t.Fatalf("valid state not served: %s", ok)
	}
	e.rt.mu.Lock()
	e.rt.noState = true
	e.rt.mu.Unlock()
	_, body := e.do("GET", "/api/v1/workspaces/default", "viewer-token", "")
	if !strings.Contains(body, `"ready":false`) || strings.Contains(body, "agentControl") {
		t.Fatalf("policy-less state was reported as usable: %s", body)
	}
}

func TestAgentIdentityInNOVNCReadsOnlyStateAndNotifications(t *testing.T) {
	e := newModeEnv(t)
	e.rt.set(func(c *apiwire.AgentControl) { c.Mode = apiwire.AgentModeNOVNC })
	e.wantStatus("GET", "/api/v1/workspaces", "agent-token", "", 200)
	e.wantStatus("GET", "/api/v1/workspaces/default", "agent-token", "", 200)
	e.wantStatus("GET", modeBase+"agent", "agent-token", "", 200)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := e.edge.Client().Do(e.req(ctx, "GET", modeBase+"agent/events", "agent-token", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	line, err := br.ReadString('\n')
	if resp.StatusCode != 200 || err != nil || line != "event: agent-control\n" {
		t.Fatalf("agent/events as NOVNC agent: %d %q %v", resp.StatusCode, line, err)
	}
	for _, p := range []string{"targets", "resolution", "health", "leases/current", "cdp/json/version", "files", "agent/setup"} {
		got, _ := e.do("GET", modeBase+p, "agent-token", "")
		if got == 200 {
			t.Fatalf("agent read %s in NOVNC", p)
		}
	}
	e.wantCode("GET", modeBase+"targets", "agent-token", "", 409, apiwire.CodeAgentModeDisabled)
	e.wantCode("POST", modeBase+"leases", "agent-token", `{"mode":"AGENT"}`, 409, apiwire.CodeAgentModeDisabled)
	e.wantCode("GET", "/api/v1/audit", "agent-token", "", 409, apiwire.CodeAgentModeDisabled)
	e.wantCode("GET", "/api/v1/feedback", "agent-token", "", 409, apiwire.CodeAgentModeDisabled)
	if n := e.rt.hit("GET " + modeBase + "targets"); n != 0 {
		t.Fatalf("targets reached the runtime %d times", n)
	}
}

func TestPausedAgentKeepsFeedbackButLosesTheBrowser(t *testing.T) {
	e := newModeEnv(t)
	e.rt.set(func(c *apiwire.AgentControl) { c.Paused = true })
	for _, p := range []string{"targets", "resolution", "cdp/json/version"} {
		e.wantCode("GET", modeBase+p, "agent-token", "", 409, apiwire.CodeAgentPaused)
	}
	e.wantCode("POST", modeBase+"leases", "agent-token", `{"mode":"AGENT"}`, 409, apiwire.CodeAgentPaused)
	e.wantCode("POST", modeBase+"leases/l1/renew", "agent-token", `{}`, 409, apiwire.CodeAgentPaused)
	if n := e.rt.hit("GET " + modeBase + "targets"); n != 0 {
		t.Fatal("paused agent reached the browser")
	}
	// Humans keep leaving feedback and the Agent keeps reading and answering it.
	for _, tok := range []string{"human-token", "agent-token"} {
		e.wantStatus("GET", "/api/v1/feedback?workspaceId=default", tok, "", 200)
	}
	e.wantStatus("POST", "/api/v1/feedback/f1/ack", "agent-token", `{}`, 200)
	e.wantStatus("POST", "/api/v1/feedback/f1/resolve", "agent-token", `{}`, 200)
	e.wantStatus("POST", "/api/v1/feedback", "human-token", `{}`, 200)
	e.wantStatus("POST", modeBase+"captures", "human-token", `{}`, 200)
	e.wantStatus("GET", modeBase+"targets", "human-token", "", 200)
	e.wantStatus("GET", modeBase+"files", "human-token", "", 200)
	// The Agent can still end a lease; the runtime decides what that means.
	e.wantStatus("DELETE", modeBase+"leases/l1", "agent-token", "", 403)
}

func (e *modeEnv) seedLeases() (agent, human string) {
	exp := time.Now().Add(time.Hour)
	e.epoch++
	e.g.leases.put(ownedLease{
		Actor: Identity{ID: "agent-1", Role: "agent", WorkspaceID: "default"},
		Grant: apiwire.LeaseGrant{CapabilityToken: "cap-agent", Lease: apiwire.Lease{LeaseID: "la", Mode: "AGENT", Epoch: e.epoch, ExpiresAt: exp}},
	})
	e.epoch++
	e.g.leases.put(ownedLease{
		Actor: Identity{ID: "human", Role: "human", WorkspaceID: "default"},
		Grant: apiwire.LeaseGrant{CapabilityToken: "cap-human", Lease: apiwire.Lease{LeaseID: "lh", Mode: "HUMAN", Epoch: e.epoch, ExpiresAt: exp}},
	})
	return "cap-agent", "cap-human"
}

func TestModeRoutesAreHumanOnlyScopedAndRevokeAgentGrants(t *testing.T) {
	e := newModeEnv(t)
	agentCap, humanCap := e.seedLeases()
	put := func(tok, body string) (int, string) { return e.do("PUT", modeBase+"agent", tok, body) }

	for _, tok := range []string{"viewer-token", "agent-token"} {
		if got, _ := put(tok, `{"mode":"NOVNC"}`); got != 403 {
			t.Fatalf("%s changed the mode: %d", tok, got)
		}
		for _, a := range []string{"pause", "resume"} {
			e.wantStatus("POST", modeBase+"agent/"+a, tok, `{}`, 403)
		}
		e.wantStatus("GET", modeBase+"agent", tok, "", 200)
	}
	if n := len(e.rt.putSeen); n != 0 || e.rt.hit("POST "+modeBase+"agent/pause") != 0 {
		t.Fatal("a refused role reached the runtime")
	}
	for _, body := range []string{``, `{}`, `{"mode":"AGENT","x":1}`, `{"mode":"OFF"}`, `{"mode":"novnc"}`, `{"mode":"NOVNC"} {}`} {
		if got, _ := put("human-token", body); got != 400 {
			t.Fatalf("body %q accepted: %d", body, got)
		}
	}
	e.wantStatus("POST", modeBase+"agent/pause", "human-token", `{"x":1}`, 400)
	e.wantStatus("POST", modeBase+"agent/pause", "human-token", ``, 400)
	e.wantStatus("PATCH", modeBase+"agent", "human-token", `{}`, 405)

	// Pause proxies, keeps the grant so the Agent reads AGENT_PAUSED, and the
	// Agent's own lease token cannot reach the browser meanwhile.
	e.wantStatus("POST", modeBase+"agent/pause", "human-token", `{}`, 200)
	e.wantCode("GET", modeBase+"targets", agentCap, "", 409, apiwire.CodeAgentPaused)
	// Resume allows a fresh acquisition only: the old grant is gone.
	e.wantStatus("POST", modeBase+"agent/resume", "human-token", `{}`, 200)
	if _, ok := e.g.leases.get(agentCap); ok {
		t.Fatal("resume revived the earlier Agent grant")
	}
	e.wantStatus("GET", modeBase+"targets", agentCap, "", 401)
	if _, ok := e.g.leases.get(humanCap); !ok {
		t.Fatal("resume dropped the HUMAN grant")
	}

	// NOVNC and back to AGENT revive nothing either.
	agentCap, _ = e.seedLeases()
	if got, b := put("human-token", `{"mode":"NOVNC"}`); got != 200 || !strings.Contains(b, `"mode":"NOVNC"`) {
		t.Fatalf("NOVNC: %d %s", got, b)
	}
	if mode := <-e.rt.putSeen; mode != "NOVNC" {
		t.Fatalf("runtime saw %q", mode)
	}
	if _, ok := e.g.leases.get(agentCap); ok {
		t.Fatal("NOVNC kept the Agent grant")
	}
	if _, ok := e.g.leases.get(humanCap); !ok {
		t.Fatal("NOVNC dropped the HUMAN grant")
	}
	e.wantStatus("GET", modeBase+"targets", agentCap, "", 401)
	e.wantCode("GET", modeBase+"targets", "agent-token", "", 409, apiwire.CodeAgentModeDisabled)
	e.wantCode("POST", modeBase+"agent/pause", "human-token", `{}`, 409, apiwire.CodeAgentModeDisabled)
	agentCap, _ = e.seedLeases()
	if got, _ := put("human-token", `{"mode":"AGENT"}`); got != 200 {
		t.Fatalf("enable AGENT: %d", got)
	}
	<-e.rt.putSeen
	if _, ok := e.g.leases.get(agentCap); ok {
		t.Fatal("enabling AGENT revived a lease")
	}
}

func TestNOVNCEndsOpenCollaborationRequestsButNotPolicyEvents(t *testing.T) {
	e := newModeEnv(t)
	entered := make(chan string, 4)
	ended := make(chan string, 4)
	e.hook = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		entered <- r.URL.Path
		<-r.Context().Done()
		ended <- r.URL.Path
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var bodies []io.Closer
	for _, p := range []string{"/api/v1/events?workspaceId=default", "/api/v1/captures/c1/image"} {
		for _, tok := range []string{"human-token", "agent-token"} {
			resp, err := e.edge.Client().Do(e.req(ctx, "GET", p, tok, ""))
			if err != nil {
				t.Fatal(err)
			}
			bodies = append(bodies, resp.Body)
			<-entered
		}
	}
	defer func() {
		for _, b := range bodies {
			b.Close()
		}
	}()
	policy, err := e.edge.Client().Do(e.req(ctx, "GET", modeBase+"agent/events", "agent-token", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer policy.Body.Close()
	pbr := bufio.NewReader(policy.Body)
	if _, err := pbr.ReadString('\n'); err != nil {
		t.Fatal(err)
	}

	if got, b := e.do("PUT", modeBase+"agent", "human-token", `{"mode":"NOVNC"}`); got != 200 {
		t.Fatalf("NOVNC: %d %s", got, b)
	}
	for i := range 4 {
		select {
		case <-ended:
		case <-ctx.Done():
			t.Fatalf("only %d of 4 open collaboration requests ended", i)
		}
	}
	// The notification stream stays up so an Agent still hears about a resume.
	if n := e.g.streams.open(); n != 1 {
		t.Fatalf("%d streams open after the switch, want only the policy stream", n)
	}
}

func TestNOVNCInterruptsArtifactResponseBytes(t *testing.T) {
	e := newModeEnv(t)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	const length = 1 << 20
	e.hook = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1048576")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-release
		// Artifact handlers write already-authorized bytes; net/http writes do
		// not stop merely because a derived request context was cancelled.
		_, _ = w.Write(bytes.Repeat([]byte{42}, length))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := e.edge.Client().Do(e.req(ctx, "GET", "/api/v1/feedback/f1/bundle?revision=1", "agent-token", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	e.wantStatus("PUT", modeBase+"agent", "human-token", `{"mode":"NOVNC"}`, 200)
	unblock()
	n, err := io.Copy(io.Discard, resp.Body)
	if n == length || err == nil {
		t.Fatalf("disabled Agent received complete artifact: bytes=%d error=%v", n, err)
	}
}

func TestModeChangeWaitsForCollaborationMutationsInFlight(t *testing.T) {
	e := newModeEnv(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	var finished atomic.Bool
	e.hook = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			close(entered)
			<-release
			finished.Store(true)
		}
		w.WriteHeader(200)
	}
	mutation := make(chan int, 1)
	go func() {
		got, _ := e.do("POST", "/api/v1/feedback/f1/publish", "human-token", `{}`)
		mutation <- got
	}()
	<-entered
	switched := make(chan int, 1)
	go func() {
		got, _ := e.do("PUT", modeBase+"agent", "human-token", `{"mode":"NOVNC"}`)
		switched <- got
	}()
	select {
	case <-e.rt.putSeen:
		t.Fatal("the runtime switched while a collaboration mutation was in flight")
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	if got := <-mutation; got != 200 {
		t.Fatalf("in-flight mutation = %d", got)
	}
	if got := <-switched; got != 200 || !finished.Load() {
		t.Fatalf("switch = %d, mutation finished first = %v", got, finished.Load())
	}
	e.wantCode("POST", "/api/v1/feedback/f1/publish", "human-token", `{}`, 409, apiwire.CodeAgentModeDisabled)
}

func TestAgentPolicyEventsAreProxiedAndDrainOnShutdown(t *testing.T) {
	e := newModeEnv(t)
	e.rt.set(func(c *apiwire.AgentControl) { c.Mode = apiwire.AgentModeNOVNC })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := e.edge.Client().Do(e.req(ctx, "GET", modeBase+"agent/events", "human-token", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("events: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	br := bufio.NewReader(resp.Body)
	if line, _ := br.ReadString('\n'); line != "event: agent-control\n" {
		t.Fatalf("first line %q", line)
	}
	data, _ := br.ReadString('\n')
	if !strings.Contains(data, `"mode":"NOVNC"`) {
		t.Fatalf("data line %q", data)
	}
	if e.g.streams.open() != 1 {
		t.Fatalf("policy stream not tracked: %d open", e.g.streams.open())
	}
	waitCtx, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	if err := e.g.streams.wait(waitCtx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	// The proxy aborts a stream it cancels, so only the end matters here.
	_, _ = io.Copy(io.Discard, br)
	if n := e.g.streams.open(); n != 0 {
		t.Fatalf("%d streams still open after the drain", n)
	}
	e.wantStatus("GET", modeBase+"agent/events", "human-token", "", 503)
}

func TestSetupPromptIsHumanOnlyAGENTOnlyAndSecretFree(t *testing.T) {
	e := newModeEnv(t)
	e.wantStatus("GET", modeBase+"agent/setup", "viewer-token", "", 403)
	e.wantStatus("GET", modeBase+"agent/setup", "agent-token", "", 403)
	e.wantStatus("POST", modeBase+"agent/setup", "human-token", `{}`, 405)
	status, body := e.do("GET", modeBase+"agent/setup", "human-token", "")
	if status != 200 {
		t.Fatalf("setup: %d %s", status, body)
	}
	var out struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || out.Prompt == "" {
		t.Fatalf("setup body: %v %s", err, body)
	}
	for _, secret := range []string{"human-token", "agent-token", "viewer-token", "runtime-token", digest("agent-token"), "lensdock-session", "capabilityToken", "cap-agent"} {
		if strings.Contains(out.Prompt, secret) {
			t.Errorf("prompt leaks %q", secret)
		}
	}
	// NOVNC closes it for everyone, an Agent identity included.
	e.rt.set(func(c *apiwire.AgentControl) { c.Mode = apiwire.AgentModeNOVNC })
	for _, tok := range []string{"human-token", "agent-token"} {
		e.wantCode("GET", modeBase+"agent/setup", tok, "", 409, apiwire.CodeAgentModeDisabled)
	}
}

func TestSetupPromptQuotesOriginAndWorkspaceForTheShell(t *testing.T) {
	p := agentSetupPrompt("https://lens.example", `a'b $(touch x) "c"`)
	if !strings.Contains(p, `WS='a'\''b $(touch x) "c"'`) {
		t.Error("workspace is not shell-quoted")
	}
}
