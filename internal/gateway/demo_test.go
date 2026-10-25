package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const demoOrigin = "http://127.0.0.1:18081"

func demoIdentities(t *testing.T, enable bool) *identities {
	t.Helper()
	a, err := newIdentities([]Identity{{ID: "human", Role: "human", WorkspaceID: "alpha", TokenSHA256: digest("tok")}}, demoOrigin, "")
	if err != nil {
		t.Fatal(err)
	}
	if enable {
		if err := a.enableDemo("alpha"); err != nil {
			t.Fatal(err)
		}
	}
	return a
}

// browserFetch builds the request a same-origin fetch() from the served UI sends.
func browserFetch(method, body string) *http.Request {
	r := httptest.NewRequest(method, demoOrigin+"/api/v1/session", strings.NewReader(body))
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Set("Sec-Fetch-Mode", "cors")
	r.Header.Set("Sec-Fetch-Dest", "empty")
	if method == "POST" {
		r.Header.Set("Origin", demoOrigin)
	}
	return r
}

func serve(a *identities, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	a.serveSession(w, r)
	return w
}

func cookieNamed(w *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func TestDemoOffByDefaultStillRequiresToken(t *testing.T) {
	a := demoIdentities(t, false)
	if w := serve(a, browserFetch("GET", "")); w.Code != 401 || cookieNamed(w, a.sessionCookie) != nil {
		t.Fatalf("anonymous GET without demo: %d", w.Code)
	}
	if w := serve(a, browserFetch("POST", `{"demo":true}`)); w.Code != 401 || cookieNamed(w, a.sessionCookie) != nil {
		t.Fatalf("demo POST without demo enabled: %d", w.Code)
	}
	if len(a.sessions) != 0 {
		t.Fatal("session allocated with demo off")
	}
	if w := serve(a, browserFetch("POST", `{"token":"tok"}`)); w.Code != 200 {
		t.Fatalf("token login broken: %d", w.Code)
	}
}

func TestDemoAutoSessionAndBoundaries(t *testing.T) {
	a := demoIdentities(t, true)
	w := serve(a, browserFetch("GET", ""))
	if w.Code != 200 {
		t.Fatalf("demo GET: %d %s", w.Code, w.Body)
	}
	c := cookieNamed(w, a.sessionCookie)
	if c == nil || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.MaxAge != 0 || !c.Expires.IsZero() {
		t.Fatalf("demo cookie must be HttpOnly, SameSite=Strict and a browser-session cookie: %+v", c)
	}
	var body struct {
		Actor Identity `json:"actor"`
		CSRF  string   `json:"csrfToken"`
		Demo  bool     `json:"demo"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Actor.ID != "demo-human" || body.Actor.Role != "human" || body.Actor.WorkspaceID != "alpha" || !body.Demo || body.CSRF == "" {
		t.Fatalf("unexpected demo session: %+v", body)
	}

	// The cookie is reused: no new session on the next probe.
	again := browserFetch("GET", "")
	again.AddCookie(c)
	if w := serve(a, again); w.Code != 200 || cookieNamed(w, a.sessionCookie) != nil || len(a.sessions) != 1 {
		t.Fatalf("existing demo session not reused: %d, %d sessions", w.Code, len(a.sessions))
	}

	// Mutations still need the CSRF token and the exact origin.
	for _, tc := range []struct {
		name, origin, csrf string
		allowed            bool
	}{
		{"with CSRF", demoOrigin, body.CSRF, true},
		{"missing CSRF", demoOrigin, "", false},
		{"wrong CSRF", demoOrigin, "x", false},
		{"foreign origin", "https://attacker.example", body.CSRF, false},
	} {
		r := httptest.NewRequest("POST", demoOrigin+"/api/v1/workspaces/alpha/leases", nil)
		r.AddCookie(c)
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("X-CSRF-Token", tc.csrf)
		p, ok := a.authenticate(r)
		if ok != tc.allowed {
			t.Fatalf("%s: allowed=%v", tc.name, ok)
		}
		if ok && (p.ID != "demo-human" || p.Role != "human" || p.TokenSHA256 != "") {
			t.Fatalf("%s: wrong actor %+v", tc.name, p)
		}
	}
	// Cross-site browser requests carrying the cookie are fenced like any session.
	cross := httptest.NewRequest("GET", demoOrigin+"/api/v1/system", nil)
	cross.AddCookie(c)
	cross.Header.Set("Sec-Fetch-Site", "cross-site")
	if _, ok := a.authenticate(cross); ok {
		t.Fatal("cross-site request authenticated")
	}
	// No credentials at all: still anonymous.
	if _, ok := a.authenticate(httptest.NewRequest("GET", demoOrigin+"/api/v1/system", nil)); ok {
		t.Fatal("anonymous request authenticated")
	}
}

func TestDemoEntryRequiresSameOriginBrowserFetch(t *testing.T) {
	a := demoIdentities(t, true)
	mutate := map[string]func(*http.Request){
		"no fetch metadata (curl)": func(r *http.Request) {
			r.Header.Del("Sec-Fetch-Site")
			r.Header.Del("Sec-Fetch-Mode")
			r.Header.Del("Sec-Fetch-Dest")
		},
		"cross-site":           func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") },
		"same-site other port": func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-site") },
		"typed URL navigation": func(r *http.Request) {
			r.Header.Set("Sec-Fetch-Site", "none")
			r.Header.Set("Sec-Fetch-Mode", "navigate")
			r.Header.Set("Sec-Fetch-Dest", "document")
		},
		"subresource (img/script)": func(r *http.Request) {
			r.Header.Set("Sec-Fetch-Mode", "no-cors")
			r.Header.Set("Sec-Fetch-Dest", "image")
		},
		"wrong Host (rebinding)": func(r *http.Request) { r.Host = "evil.example:18081" },
		"foreign Origin header":  func(r *http.Request) { r.Header.Set("Origin", "https://attacker.example") },
	}
	for name, f := range mutate {
		for _, method := range []string{"GET", "POST"} {
			body := ""
			if method == "POST" {
				body = `{"demo":true}`
			}
			r := browserFetch(method, body)
			f(r)
			w := serve(a, r)
			if w.Code == 200 || cookieNamed(w, a.sessionCookie) != nil {
				t.Fatalf("%s %s created a session (%d)", name, method, w.Code)
			}
		}
	}
	// A POST without an Origin header is not a browser form/fetch from our UI.
	r := browserFetch("POST", `{"demo":true}`)
	r.Header.Del("Origin")
	if w := serve(a, r); w.Code == 200 {
		t.Fatal("demo POST without Origin accepted")
	}
	if r := browserFetch("POST", `{"demo":true,"token":"tok"}`); serve(a, r).Code != 400 {
		t.Fatal("demo plus token accepted")
	}
	if len(a.sessions) != 0 {
		t.Fatalf("%d sessions allocated by rejected requests", len(a.sessions))
	}
}

func TestDemoSessionDoesNotExpireButTokenSessionsDo(t *testing.T) {
	a := demoIdentities(t, true)
	w := serve(a, browserFetch("GET", ""))
	demo := cookieNamed(w, a.sessionCookie)
	tok := cookieNamed(serve(a, browserFetch("POST", `{"token":"tok"}`)), a.sessionCookie)
	if demo == nil || tok == nil {
		t.Fatal("sessions not created")
	}
	// Age every session far beyond the 8 hour token lifetime.
	a.mu.Lock()
	for k, s := range a.sessions {
		s.Created = s.Created.Add(-30 * 24 * time.Hour)
		if !s.Demo {
			s.Expires = time.Now().Add(-time.Second)
		}
		a.sessions[k] = s
	}
	a.mu.Unlock()
	d := httptest.NewRequest("GET", demoOrigin+"/api/v1/system", nil)
	d.AddCookie(demo)
	if p, ok := a.authenticate(d); !ok || p.ID != "demo-human" {
		t.Fatal("demo session expired")
	}
	e := httptest.NewRequest("GET", demoOrigin+"/api/v1/system", nil)
	e.AddCookie(tok)
	if _, ok := a.authenticate(e); ok {
		t.Fatal("expired token session still valid")
	}
	if c := cookieNamed(serve(a, browserFetch("POST", `{"token":"tok"}`)), a.sessionCookie); c == nil || c.MaxAge != int((8*time.Hour)/time.Second) {
		t.Fatalf("token session cookie lifetime changed: %+v", c)
	}
}

func TestDemoSessionAllocationIsBounded(t *testing.T) {
	a := demoIdentities(t, true)
	for i := range 3 * maxDemoSessions {
		if w := serve(a, browserFetch("GET", "")); w.Code != 200 {
			t.Fatalf("demo GET %d: %d", i, w.Code)
		}
	}
	if len(a.sessions) != maxDemoSessions {
		t.Fatalf("demo sessions = %d, want %d", len(a.sessions), maxDemoSessions)
	}
}

func TestDemoLogoutRequiresExplicitReentry(t *testing.T) {
	a := demoIdentities(t, true)
	w := serve(a, browserFetch("GET", ""))
	c := cookieNamed(w, a.sessionCookie)
	var body struct {
		CSRF string `json:"csrfToken"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)

	noCSRF := browserFetch("DELETE", "")
	noCSRF.AddCookie(c)
	if serve(a, noCSRF).Code != 403 || len(a.sessions) != 1 {
		t.Fatal("logout without CSRF accepted")
	}
	out := browserFetch("DELETE", "")
	out.AddCookie(c)
	out.Header.Set("X-CSRF-Token", body.CSRF)
	lw := serve(a, out)
	if lw.Code != 204 || len(a.sessions) != 0 {
		t.Fatalf("logout: %d, %d sessions", lw.Code, len(a.sessions))
	}
	marker := cookieNamed(lw, a.demoOffCookie)
	if marker == nil || !marker.HttpOnly || marker.SameSite != http.SameSiteStrictMode {
		t.Fatalf("demo sign-out marker missing: %+v", marker)
	}

	// A reload after sign-out does not silently re-enter.
	probe := browserFetch("GET", "")
	probe.AddCookie(marker)
	pw := serve(a, probe)
	if pw.Code != 401 || !strings.Contains(pw.Body.String(), "DEMO_SIGNED_OUT") || len(a.sessions) != 0 {
		t.Fatalf("reload after logout: %d %s", pw.Code, pw.Body)
	}
	// Explicit entry works and clears the marker.
	entry := browserFetch("POST", `{"demo":true}`)
	entry.AddCookie(marker)
	ew := serve(a, entry)
	if ew.Code != 200 || cookieNamed(ew, a.sessionCookie) == nil {
		t.Fatalf("explicit demo entry: %d", ew.Code)
	}
	if m := cookieNamed(ew, a.demoOffCookie); m == nil || m.MaxAge >= 0 {
		t.Fatal("demo sign-out marker not cleared")
	}
}

func TestDemoRejectsRemoteOriginAndReservedIdentity(t *testing.T) {
	for _, origin := range []string{"https://lensdock.example.com", "https://203.0.113.5:8443", "http://localhost.example.com:18081"} {
		a, err := newIdentities([]Identity{{ID: "human", Role: "human", WorkspaceID: "w", TokenSHA256: digest("tok")}}, origin, "")
		if err != nil {
			continue // already rejected as an origin
		}
		if a.enableDemo("w") == nil {
			t.Fatalf("demo enabled for remote origin %s", origin)
		}
	}
	for _, origin := range []string{"http://127.0.0.1:18081", "http://localhost:18081", "http://[::1]:18081"} {
		a, err := newIdentities([]Identity{{ID: "human", Role: "human", WorkspaceID: "w", TokenSHA256: digest("tok")}}, origin, "")
		if err != nil || a.enableDemo("w") != nil {
			t.Fatalf("loopback origin %s rejected: %v", origin, err)
		}
	}
	a, _ := newIdentities([]Identity{{ID: "demo-human", Role: "admin", WorkspaceID: "w", TokenSHA256: digest("tok")}}, demoOrigin, "")
	if a.enableDemo("w") == nil {
		t.Fatal("demo actor ID collision accepted")
	}
}

func TestNewRejectsDemoWithRemoteOriginOrManyWorkspaces(t *testing.T) {
	id := []Identity{{ID: "h", Role: "human", WorkspaceID: "w", TokenSHA256: digest("tok")}}
	rt := RuntimeConfig{WorkspaceID: "w", URL: "http://127.0.0.1:1"}
	if _, err := New(Config{Origin: "https://lensdock.example.com", Identities: id, Runtimes: []RuntimeConfig{rt}, DemoNoAuth: true}, Integrations{}); err == nil {
		t.Fatal("remote demo startup accepted")
	}
	two := []RuntimeConfig{rt, {WorkspaceID: "x", URL: "http://127.0.0.1:1"}}
	if _, err := New(Config{Origin: demoOrigin, Identities: id, Runtimes: two, DemoNoAuth: true}, Integrations{}); err == nil {
		t.Fatal("demo with several workspaces accepted")
	}
}
