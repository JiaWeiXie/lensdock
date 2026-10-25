package runtimeapi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
	"github.com/JiaWeiXie/lensdock/internal/auth"
	"github.com/JiaWeiXie/lensdock/internal/lease"
)

const agentPath = workspaceURL + "/agent"

func (e *env) agentControl(r resp) apiwire.AgentControl {
	e.t.Helper()
	if r.Code != http.StatusOK {
		e.t.Fatalf("agent control: status %d body %s", r.Code, r.Body.String())
	}
	var c apiwire.AgentControl
	if err := json.Unmarshal(r.Body.Bytes(), &c); err != nil {
		e.t.Fatal(err)
	}
	return c
}

func (e *env) setMode(mode string) resp {
	return e.do(http.MethodPut, agentPath, operatorTok, nil, []byte(`{"mode":"`+mode+`"}`))
}

func TestAgentRoutesRequireOperatorAndWorkspace(t *testing.T) {
	e := newEnv(t)
	human := e.mustAcquire("HUMAN")
	for _, rt := range []struct{ method, path string }{
		{"GET", agentPath}, {"PUT", agentPath},
		{"POST", agentPath + "/pause"}, {"POST", agentPath + "/resume"},
		{"GET", agentPath + "/events"},
	} {
		body := []byte(`{}`)
		if rt.method == "PUT" {
			body = []byte(`{"mode":"AGENT"}`)
		}
		expect(t, rt.method+" "+rt.path+" no token", e.do(rt.method, rt.path, "", nil, body), 401, apiwire.CodeUnauthenticated)
		expect(t, rt.method+" "+rt.path+" lease token", e.do(rt.method, rt.path, human.CapabilityToken, nil, body), 401, apiwire.CodeUnauthenticated)
		other := strings.Replace(rt.path, "/default/", "/other/", 1)
		expect(t, rt.method+" "+other, e.do(rt.method, other, operatorTok, nil, body), 404, apiwire.CodeNotFound)
	}
	expect(t, "pause wrong method", e.do("PUT", agentPath+"/pause", operatorTok, nil, []byte(`{}`)), 405, apiwire.CodeMethodNotAllowed)
	expect(t, "events wrong method", e.do("POST", agentPath+"/events", operatorTok, nil, []byte(`{}`)), 405, apiwire.CodeMethodNotAllowed)
	expect(t, "agent wrong method", e.do("DELETE", agentPath, operatorTok, nil, nil), 405, apiwire.CodeMethodNotAllowed)
}

func TestAgentControlInitialStateAndWorkspaceViews(t *testing.T) {
	e := newEnv(t)
	c := e.agentControl(e.do("GET", agentPath, operatorTok, nil, nil))
	if c.Mode != apiwire.AgentModeAGENT || c.Paused || c.Revision != 1 || c.UpdatedAt.IsZero() {
		t.Fatalf("initial = %+v", c)
	}
	var ws apiwire.Workspace
	if err := json.Unmarshal(e.do("GET", workspaceURL, operatorTok, nil, nil).Body.Bytes(), &ws); err != nil {
		t.Fatal(err)
	}
	if ws.AgentControl != c {
		t.Fatalf("workspace agentControl = %+v, want %+v", ws.AgentControl, c)
	}
	var st struct {
		Workspace apiwire.Workspace `json:"workspace"`
	}
	if err := json.Unmarshal(e.do("GET", workspaceURL+"/state", operatorTok, nil, nil).Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Workspace.AgentControl != c {
		t.Fatalf("state agentControl = %+v", st.Workspace.AgentControl)
	}
	e.agentControl(e.setMode(apiwire.AgentModeNOVNC))
	json.Unmarshal(e.do("GET", workspaceURL, operatorTok, nil, nil).Body.Bytes(), &ws)
	if ws.AgentControl.Mode != apiwire.AgentModeNOVNC || ws.AgentControl.Revision != 2 {
		t.Fatalf("workspace after switch = %+v", ws.AgentControl)
	}
}

func TestAgentModeBodyValidation(t *testing.T) {
	e := newEnv(t)
	for name, body := range map[string]string{
		"empty":         ``,
		"lower case":    `{"mode":"novnc"}`,
		"unknown mode":  `{"mode":"OTHER"}`,
		"missing":       `{}`,
		"extra field":   `{"mode":"AGENT","paused":true}`,
		"trailing":      `{"mode":"AGENT"} {}`,
		"wrong type":    `{"mode":1}`,
		"array":         `["AGENT"]`,
		"invalid utf-8": "{\"mode\":\"\xff\"}",
	} {
		r := e.do("PUT", agentPath, operatorTok, nil, []byte(body))
		if r.Code != 400 {
			t.Errorf("%s: status %d body %s", name, r.Code, r.Body.String())
		}
	}
	if c := e.agentControl(e.do("GET", agentPath, operatorTok, nil, nil)); c.Revision != 1 {
		t.Fatalf("rejected requests changed the policy: %+v", c)
	}
	for _, p := range []string{"/pause", "/resume"} {
		for _, body := range []string{`{"x":1}`, `[]`, `nope`} {
			if r := e.do("POST", agentPath+p, operatorTok, nil, []byte(body)); r.Code != 400 {
				t.Errorf("POST %s %q: status %d", p, body, r.Code)
			}
		}
	}
}

func TestAgentNOVNCRevokesAgentAndStaysStableErrors(t *testing.T) {
	e := newEnv(t)
	agent := e.mustAcquire("AGENT")
	tok := agent.CapabilityToken
	l := agent.Lease
	renew := func() resp {
		return e.do("POST", leasesPath+"/"+l.LeaseID+"/renew", tok, nil,
			[]byte(`{"epoch":`+itoa(int(l.Epoch))+`,"runtimeGeneration":"`+l.RuntimeGeneration+`"}`))
	}
	expect(t, "renew before", renew(), 200, "")
	ticket, release, err := e.leases.AuthorizeTracked(tok, lease.ModeAgent)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	c := e.agentControl(e.setMode(apiwire.AgentModeNOVNC))
	if c.Mode != apiwire.AgentModeNOVNC || c.Paused || c.Revision != 2 {
		t.Fatalf("NOVNC = %+v", c)
	}
	select {
	case <-ticket.Done():
	default:
		t.Fatal("the AGENT ticket (CDP/VNC connections) stayed open")
	}
	expect(t, "renew after", renew(), 409, apiwire.CodeAgentModeDisabled)
	expect(t, "current after", e.do("GET", leasesPath+"/current", tok, nil, nil), 409, apiwire.CodeAgentModeDisabled)
	expect(t, "acquire AGENT", e.acquire(operatorTok, "AGENT", 30), 409, apiwire.CodeAgentModeDisabled)
	expect(t, "pause", e.do("POST", agentPath+"/pause", operatorTok, nil, []byte(`{}`)), 409, apiwire.CodeAgentModeDisabled)
	expect(t, "resume", e.do("POST", agentPath+"/resume", operatorTok, nil, []byte(`{}`)), 409, apiwire.CodeAgentModeDisabled)
	expect(t, "HUMAN acquire", e.acquire(operatorTok, "HUMAN", 30), 201, "")

	// Enabling AGENT starts unpaused and never revives the old lease.
	c = e.agentControl(e.setMode(apiwire.AgentModeAGENT))
	if c.Mode != apiwire.AgentModeAGENT || c.Paused || c.Revision != 3 {
		t.Fatalf("AGENT = %+v", c)
	}
	expect(t, "renew old lease after enable", renew(), 409, apiwire.CodeLeaseEnded)
}

func TestAgentPauseResumeFlow(t *testing.T) {
	e := newEnv(t)
	agent := e.mustAcquire("AGENT")
	tok := agent.CapabilityToken

	// An empty body and "{}" are both accepted.
	c := e.agentControl(e.do("POST", agentPath+"/pause", operatorTok, nil, nil))
	if !c.Paused || c.Mode != apiwire.AgentModeAGENT || c.Revision != 2 {
		t.Fatalf("pause = %+v", c)
	}
	if again := e.agentControl(e.do("POST", agentPath+"/pause", operatorTok, nil, []byte(`{}`))); again != c {
		t.Fatalf("second pause = %+v, want %+v", again, c)
	}
	expect(t, "old token while paused", e.do("GET", leasesPath+"/current", tok, nil, nil), 409, apiwire.CodeAgentPaused)
	expect(t, "acquire while paused", e.acquire(operatorTok, "AGENT", 30), 409, apiwire.CodeAgentPaused)
	if st := e.leases.Status(); st.Lease != nil {
		t.Fatalf("lease survived the pause: %+v", st.Lease)
	}

	c = e.agentControl(e.do("POST", agentPath+"/resume", operatorTok, nil, []byte(`{}`)))
	if c.Paused || c.Revision != 3 {
		t.Fatalf("resume = %+v", c)
	}
	if st := e.leases.Status(); st.Lease != nil {
		t.Fatalf("resume must not acquire: %+v", st.Lease)
	}
	expect(t, "old token after resume", e.do("GET", leasesPath+"/current", tok, nil, nil), 409, apiwire.CodeLeaseEnded)
	fresh := e.mustAcquire("AGENT")
	if fresh.CapabilityToken == tok || fresh.Lease.Epoch <= agent.Lease.Epoch {
		t.Fatalf("fresh grant = %+v", fresh.Lease)
	}
}

func TestAgentPauseKeepsHumanLease(t *testing.T) {
	e := newEnv(t)
	human := e.mustAcquire("HUMAN")
	e.agentControl(e.do("POST", agentPath+"/pause", operatorTok, nil, []byte(`{}`)))
	expect(t, "human current", e.do("GET", leasesPath+"/current", human.CapabilityToken, nil, nil), 200, "")
	e.agentControl(e.setMode(apiwire.AgentModeNOVNC))
	expect(t, "human current in NOVNC", e.do("GET", leasesPath+"/current", human.CapabilityToken, nil, nil), 200, "")
	e.agentControl(e.setMode(apiwire.AgentModeAGENT))
	expect(t, "human current after enable", e.do("GET", leasesPath+"/current", human.CapabilityToken, nil, nil), 200, "")
	expect(t, "AGENT next to HUMAN", e.acquire(operatorTok, "AGENT", 30), 409, apiwire.CodeLeaseConflict)
}

// failingStore is a lease.PolicyStore whose Save can be made to fail.
type failingStore struct {
	mu   sync.Mutex
	ctl  apiwire.AgentControl
	have bool
	fail bool
}

func (s *failingStore) Load() (apiwire.AgentControl, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ctl, s.have, nil
}

func (s *failingStore) Save(c apiwire.AgentControl) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return errors.New("disk full")
	}
	s.ctl, s.have = c, true
	return nil
}

func TestAgentPersistFailureStillRestricts(t *testing.T) {
	e := newEnv(t)
	store := &failingStore{}
	m, err := lease.NewManager(lease.Config{WorkspaceID: testWS, Clock: e.clock, PolicyStore: store})
	if err != nil {
		t.Fatal(err)
	}
	m.SetLocked(false, "")
	t.Cleanup(m.Shutdown)
	e.leases = m
	e.h = NewHandler(Deps{WorkspaceID: testWS, Operator: auth.DigestOf(operatorTok), Leases: m, Health: e.health})

	agent := e.mustAcquire("AGENT")
	ticket, release, err := m.AuthorizeTracked(agent.CapabilityToken, lease.ModeAgent)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	store.mu.Lock()
	store.fail = true
	store.mu.Unlock()

	r := e.do("POST", agentPath+"/pause", operatorTok, nil, []byte(`{}`))
	expect(t, "pause with failing store", r, 500, apiwire.CodeInternal)
	var we apiwire.Error
	if err := json.Unmarshal(r.Body.Bytes(), &we); err != nil || we.Message == "" {
		t.Fatalf("error body = %s", r.Body.String())
	}
	select {
	case <-ticket.Done():
	default:
		t.Fatal("a failed save kept the AGENT lease alive")
	}
	if c := e.agentControl(e.do("GET", agentPath, operatorTok, nil, nil)); !c.Paused {
		t.Fatalf("the pause must be in force: %+v", c)
	}
	// Resuming is relaxing: refused while the save fails, and not applied.
	expect(t, "resume with failing store", e.do("POST", agentPath+"/resume", operatorTok, nil, []byte(`{}`)), 500, apiwire.CodeInternal)
	if c := e.agentControl(e.do("GET", agentPath, operatorTok, nil, nil)); !c.Paused {
		t.Fatalf("unsaved resume applied: %+v", c)
	}
}

// sseReader parses server-sent events from an HTTP response body.
type sseReader struct {
	t  *testing.T
	sc *bufio.Scanner
}

type sseEvent struct {
	id, event, data string
	comment         bool
}

// next reads one event or comment. It fails the test after a generous
// deadline so a broken stream cannot hang the suite; the deadline never
// decides the outcome of a passing run.
func (r *sseReader) next() (sseEvent, bool) {
	r.t.Helper()
	type out struct {
		ev sseEvent
		ok bool
	}
	ch := make(chan out, 1)
	go func() {
		var ev sseEvent
		seen := false
		for r.sc.Scan() {
			line := r.sc.Text()
			switch {
			case line == "":
				if seen {
					ch <- out{ev, true}
					return
				}
			case strings.HasPrefix(line, ":"):
				ch <- out{sseEvent{comment: true, data: strings.TrimSpace(line[1:])}, true}
				return
			case strings.HasPrefix(line, "id: "):
				ev.id, seen = line[4:], true
			case strings.HasPrefix(line, "event: "):
				ev.event, seen = line[7:], true
			case strings.HasPrefix(line, "data: "):
				ev.data, seen = line[6:], true
			}
		}
		ch <- out{}
	}()
	select {
	case o := <-ch:
		return o.ev, o.ok
	case <-time.After(10 * time.Second):
		r.t.Fatal("timed out waiting for an SSE event")
		return sseEvent{}, false
	}
}

func (r *sseReader) nextControl() apiwire.AgentControl {
	r.t.Helper()
	for {
		ev, ok := r.next()
		if !ok {
			r.t.Fatal("stream ended")
		}
		if ev.comment {
			continue
		}
		if ev.event != "agent-control" {
			r.t.Fatalf("event = %q", ev.event)
		}
		var c apiwire.AgentControl
		if err := json.Unmarshal([]byte(ev.data), &c); err != nil {
			r.t.Fatal(err)
		}
		if ev.id != itoa(int(c.Revision)) {
			r.t.Fatalf("id %q does not match revision %d", ev.id, c.Revision)
		}
		return c
	}
}

func openEvents(t *testing.T, h http.Handler, token string) (*sseReader, *http.Response, context.CancelFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+agentPath+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return &sseReader{t: t, sc: bufio.NewScanner(resp.Body)}, resp, cancel
}

func TestAgentEventsStreamsStateAndChanges(t *testing.T) {
	e := newEnv(t)
	rd, resp, _ := openEvents(t, e.h, operatorTok)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d content-type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("cache-control = %q", resp.Header.Get("Cache-Control"))
	}
	if c := rd.nextControl(); c.Mode != apiwire.AgentModeAGENT || c.Paused || c.Revision != 1 {
		t.Fatalf("initial event = %+v", c)
	}
	if _, err := e.leases.PauseAgent(); err != nil {
		t.Fatal(err)
	}
	if c := rd.nextControl(); !c.Paused || c.Revision != 2 {
		t.Fatalf("pause event = %+v", c)
	}
	// The stream stays open and authoritative in NOVNC mode.
	e.agentControl(e.setMode(apiwire.AgentModeNOVNC))
	if c := rd.nextControl(); c.Mode != apiwire.AgentModeNOVNC || c.Paused || c.Revision != 3 {
		t.Fatalf("NOVNC event = %+v", c)
	}
	e.agentControl(e.setMode(apiwire.AgentModeAGENT))
	if c := rd.nextControl(); c.Mode != apiwire.AgentModeAGENT || c.Revision != 4 {
		t.Fatalf("enable event = %+v", c)
	}
	// A late subscriber starts from the current state.
	rd2, _, _ := openEvents(t, e.h, operatorTok)
	if c := rd2.nextControl(); c.Revision != 4 {
		t.Fatalf("late subscriber initial = %+v", c)
	}
}

func TestAgentEventsRequiresOperator(t *testing.T) {
	e := newEnv(t)
	_, resp, _ := openEvents(t, e.h, "")
	if resp.StatusCode != 401 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestAgentEventsHeartbeatAndShutdown(t *testing.T) {
	e := newEnv(t)
	h := NewHandler(Deps{WorkspaceID: testWS, Operator: auth.DigestOf(operatorTok), Leases: e.leases, Health: e.health, AgentHeartbeat: 5 * time.Millisecond})
	rd, _, _ := openEvents(t, h, operatorTok)
	if c := rd.nextControl(); c.Revision != 1 {
		t.Fatalf("initial = %+v", c)
	}
	ev, ok := rd.next()
	if !ok || !ev.comment || ev.data != "heartbeat" {
		t.Fatalf("expected a heartbeat comment, got %+v ok=%v", ev, ok)
	}
	e.leases.Shutdown()
	for {
		ev, ok := rd.next()
		if !ok {
			break // the stream ended with the runtime
		}
		if !ev.comment {
			t.Fatalf("unexpected event after shutdown: %+v", ev)
		}
	}
}

func TestAgentEventsEndOnClientCancel(t *testing.T) {
	e := newEnv(t)
	rd, resp, cancel := openEvents(t, e.h, operatorTok)
	rd.nextControl()
	cancel()
	if _, ok := rd.next(); ok {
		t.Fatal("stream continued after the client went away")
	}
	_ = resp
}
