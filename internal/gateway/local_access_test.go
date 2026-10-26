package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const localOrigin = "http://127.0.0.1:18081"

func localIdentities(t *testing.T, enable bool) *identities {
	t.Helper()
	a, err := newIdentities([]Identity{{ID: "human", Role: "human", WorkspaceID: "alpha", TokenSHA256: digest("tok")}}, localOrigin, "")
	if err != nil {
		t.Fatal(err)
	}
	if enable {
		if err := a.enableLocalAccess("alpha"); err != nil {
			t.Fatal(err)
		}
	}
	return a
}

// browserFetch builds the request a same-origin fetch() from the served UI sends.
func browserFetch(method, body string) *http.Request {
	r := httptest.NewRequest(method, localOrigin+"/api/v1/session", strings.NewReader(body))
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Set("Sec-Fetch-Mode", "cors")
	r.Header.Set("Sec-Fetch-Dest", "empty")
	if method == "POST" {
		r.Header.Set("Origin", localOrigin)
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

func TestPairingOnlyIdentitiesRequireToken(t *testing.T) {
	a := localIdentities(t, false)
	if w := serve(a, browserFetch("GET", "")); w.Code != 401 || cookieNamed(w, a.sessionCookie) != nil {
		t.Fatalf("anonymous GET with pairing required: %d", w.Code)
	}
	if w := serve(a, browserFetch("POST", `{"localAccess":true}`)); w.Code != 401 || cookieNamed(w, a.sessionCookie) != nil {
		t.Fatalf("local entry with pairing required: %d", w.Code)
	}
	if len(a.sessions) != 0 {
		t.Fatal("session allocated without required pairing")
	}
	if w := serve(a, browserFetch("POST", `{"token":"tok"}`)); w.Code != 200 {
		t.Fatalf("token login broken: %d", w.Code)
	}
}

func TestLocalAutoSessionAndBoundaries(t *testing.T) {
	a := localIdentities(t, true)
	w := serve(a, browserFetch("GET", ""))
	if w.Code != 200 {
		t.Fatalf("local GET: %d %s", w.Code, w.Body)
	}
	c := cookieNamed(w, a.sessionCookie)
	if c == nil || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.MaxAge != 0 || !c.Expires.IsZero() {
		t.Fatalf("local cookie must be HttpOnly, SameSite=Strict and a browser-session cookie: %+v", c)
	}
	var body struct {
		Actor       Identity `json:"actor"`
		CSRF        string   `json:"csrfToken"`
		LocalAccess bool     `json:"localAccess"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Actor.ID != "local-human" || body.Actor.Role != "human" || body.Actor.WorkspaceID != "alpha" || !body.LocalAccess || body.CSRF == "" {
		t.Fatalf("unexpected local session: %+v", body)
	}

	// The cookie is reused: no new session on the next probe.
	again := browserFetch("GET", "")
	again.AddCookie(c)
	if w := serve(a, again); w.Code != 200 || cookieNamed(w, a.sessionCookie) != nil || len(a.sessions) != 1 {
		t.Fatalf("existing local session not reused: %d, %d sessions", w.Code, len(a.sessions))
	}

	// Mutations still need the CSRF token and the exact origin.
	for _, tc := range []struct {
		name, origin, csrf string
		allowed            bool
	}{
		{"with CSRF", localOrigin, body.CSRF, true},
		{"missing CSRF", localOrigin, "", false},
		{"wrong CSRF", localOrigin, "x", false},
		{"foreign origin", "https://attacker.example", body.CSRF, false},
	} {
		r := httptest.NewRequest("POST", localOrigin+"/api/v1/workspaces/alpha/leases", nil)
		r.AddCookie(c)
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("X-CSRF-Token", tc.csrf)
		p, ok := a.authenticate(r)
		if ok != tc.allowed {
			t.Fatalf("%s: allowed=%v", tc.name, ok)
		}
		if ok && (p.ID != "local-human" || p.Role != "human" || p.TokenSHA256 != "") {
			t.Fatalf("%s: wrong actor %+v", tc.name, p)
		}
	}
	// Cross-site browser requests carrying the cookie are fenced like any session.
	cross := httptest.NewRequest("GET", localOrigin+"/api/v1/system", nil)
	cross.AddCookie(c)
	cross.Header.Set("Sec-Fetch-Site", "cross-site")
	if _, ok := a.authenticate(cross); ok {
		t.Fatal("cross-site request authenticated")
	}
	// No credentials at all: still anonymous.
	if _, ok := a.authenticate(httptest.NewRequest("GET", localOrigin+"/api/v1/system", nil)); ok {
		t.Fatal("anonymous request authenticated")
	}
}

func TestLocalEntryRequiresSameOriginBrowserFetch(t *testing.T) {
	a := localIdentities(t, true)
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
				body = `{"localAccess":true}`
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
	r := browserFetch("POST", `{"localAccess":true}`)
	r.Header.Del("Origin")
	if w := serve(a, r); w.Code == 200 {
		t.Fatal("local POST without Origin accepted")
	}
	if r := browserFetch("POST", `{"localAccess":true,"token":"tok"}`); serve(a, r).Code != 400 {
		t.Fatal("local entry plus token accepted")
	}
	if len(a.sessions) != 0 {
		t.Fatalf("%d sessions allocated by rejected requests", len(a.sessions))
	}
}

func TestExistingLocalSessionSurvivesAdditionalBrowserSessions(t *testing.T) {
	a := localIdentities(t, true)
	first := cookieNamed(serve(a, browserFetch("GET", "")), a.sessionCookie)
	if first == nil {
		t.Fatal("first session was not created")
	}
	for i := range 1100 {
		if w := serve(a, browserFetch("GET", "")); w.Code != 200 {
			t.Fatalf("local GET %d: %d", i, w.Code)
		}
	}
	r := browserFetch("GET", "")
	r.AddCookie(first)
	if actor, ok := a.authenticate(r); !ok || actor.ID != localActorID {
		t.Fatal("creating other browser sessions invalidated an existing session")
	}
}

func TestExplicitSessionCapacityAndExpiryApplyToBothAccessModes(t *testing.T) {
	for _, local := range []bool{false, true} {
		a := localIdentities(t, local)
		a.limits = SessionLimits{TTLSeconds: 60, MaxSessions: 1}
		entry := func() *http.Request {
			if local {
				return browserFetch("GET", "")
			}
			return browserFetch("POST", `{"token":"tok"}`)
		}
		first := serve(a, entry())
		cookie := cookieNamed(first, a.sessionCookie)
		if first.Code != 200 || cookie == nil || cookie.MaxAge != 60 {
			t.Fatalf("configured expiry not applied: %d %+v", first.Code, cookie)
		}
		if next := serve(a, entry()); next.Code != 429 {
			t.Fatalf("configured session capacity not enforced: %d", next.Code)
		}
		a.mu.Lock()
		s := a.sessions[digest(cookie.Value)]
		s.Expires = time.Now().Add(-time.Second)
		a.sessions[digest(cookie.Value)] = s
		a.mu.Unlock()
		r := browserFetch("GET", "")
		r.AddCookie(cookie)
		if _, ok := a.authenticate(r); ok {
			t.Fatal("expired session remained usable")
		}
		if next := serve(a, entry()); next.Code != 200 {
			t.Fatalf("expired session kept consuming configured capacity: %d", next.Code)
		}
	}
}

func TestLocalLogoutRequiresExplicitReentry(t *testing.T) {
	a := localIdentities(t, true)
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
	marker := cookieNamed(lw, a.localOffCookie)
	if marker == nil || !marker.HttpOnly || marker.SameSite != http.SameSiteStrictMode {
		t.Fatalf("local sign-out marker missing: %+v", marker)
	}

	// A reload after sign-out does not silently re-enter.
	probe := browserFetch("GET", "")
	probe.AddCookie(marker)
	pw := serve(a, probe)
	if pw.Code != 401 || !strings.Contains(pw.Body.String(), "LOCAL_SIGNED_OUT") || len(a.sessions) != 0 {
		t.Fatalf("reload after logout: %d %s", pw.Code, pw.Body)
	}
	// Explicit entry works and clears the marker.
	entry := browserFetch("POST", `{"localAccess":true}`)
	entry.AddCookie(marker)
	ew := serve(a, entry)
	if ew.Code != 200 || cookieNamed(ew, a.sessionCookie) == nil {
		t.Fatalf("explicit local entry: %d", ew.Code)
	}
	if m := cookieNamed(ew, a.localOffCookie); m == nil || m.MaxAge >= 0 {
		t.Fatal("local sign-out marker not cleared")
	}
}

func TestLocalAccessRejectsRemoteOriginAndReservedIdentity(t *testing.T) {
	for _, origin := range []string{"https://lensdock.example.com", "https://203.0.113.5:8443", "http://localhost.example.com:18081"} {
		a, err := newIdentities([]Identity{{ID: "human", Role: "human", WorkspaceID: "w", TokenSHA256: digest("tok")}}, origin, "")
		if err != nil {
			continue // already rejected as an origin
		}
		if a.enableLocalAccess("w") == nil {
			t.Fatalf("automatic access enabled for remote origin %s", origin)
		}
	}
	for _, origin := range []string{"http://127.0.0.1:18081", "http://localhost:18081", "http://[::1]:18081"} {
		a, err := newIdentities([]Identity{{ID: "human", Role: "human", WorkspaceID: "w", TokenSHA256: digest("tok")}}, origin, "")
		if err != nil || a.enableLocalAccess("w") != nil {
			t.Fatalf("loopback origin %s rejected: %v", origin, err)
		}
	}
	a, _ := newIdentities([]Identity{{ID: "local-human", Role: "admin", WorkspaceID: "w", TokenSHA256: digest("tok")}}, localOrigin, "")
	if a.enableLocalAccess("w") == nil {
		t.Fatal("local actor ID collision accepted")
	}
}

func TestLocalBrowserEntryHonorsPairingConfiguration(t *testing.T) {
	for _, local := range []bool{false, true} {
		g := startIsolatedGateway(t, "pairing-policy", "alpha", local)
		client, _ := newJarClient(t)
		status, session, body := g.probe(t, client, browserHeaders(""))
		if local {
			if status != 200 || session.Actor.ID != localActorID || session.Actor.Role != "human" {
				t.Fatalf("automatic local entry failed: %d %s", status, body)
			}
		} else if status != 401 {
			t.Fatalf("pairing requirement was bypassed: %d %s", status, body)
		}
	}
}
