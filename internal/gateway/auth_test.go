package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSessionRequiresScopeCSRFAndOrigin(t *testing.T) {
	a, err := newIdentities([]Identity{{ID: "operator", Role: "human", WorkspaceID: "alpha", TokenSHA256: digest("test-pairing-token")}}, "http://127.0.0.1:18081")
	if err != nil {
		t.Fatal(err)
	}
	login := httptest.NewRequest("POST", "/api/v1/session", strings.NewReader(`{"token":"test-pairing-token"}`))
	login.Header.Set("Origin", "http://127.0.0.1:18081")
	w := httptest.NewRecorder()
	a.serveSession(w, login)
	if w.Code != 200 {
		t.Fatalf("login: %d", w.Code)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatal("session cookie protection missing")
	}
	var body struct {
		CSRF string `json:"csrfToken"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, method, origin, csrf string
		allowed                    bool
	}{
		{"read", "GET", "http://127.0.0.1:18081", "", true},
		{"write without CSRF", "POST", "http://127.0.0.1:18081", "", false},
		{"write with CSRF", "POST", "http://127.0.0.1:18081", body.CSRF, true},
		{"foreign origin", "POST", "https://attacker.example", body.CSRF, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "/api/v1/workspaces/alpha", nil)
			r.AddCookie(cookies[0])
			r.Header.Set("Origin", tc.origin)
			r.Header.Set("X-CSRF-Token", tc.csrf)
			p, ok := a.authenticate(r)
			if ok != tc.allowed {
				t.Fatalf("allowed=%v want %v", ok, tc.allowed)
			}
			if ok && (p.WorkspaceID != "alpha" || p.TokenSHA256 != "") {
				t.Fatal("scope or secret projection incorrect")
			}
		})
	}
	a.mu.Lock()
	for k, s := range a.sessions {
		s.Expires = time.Now().Add(-time.Second)
		a.sessions[k] = s
	}
	a.mu.Unlock()
	expired := httptest.NewRequest("GET", "/", nil)
	expired.AddCookie(cookies[0])
	if _, ok := a.authenticate(expired); ok {
		t.Fatal("expired session remained valid")
	}
}
func TestSessionRejectsMalformedTrailingBody(t *testing.T) {
	a, _ := newIdentities([]Identity{{ID: "user", Role: "human", WorkspaceID: "w", TokenSHA256: digest("token")}}, "http://localhost:18081")
	for _, body := range []string{`{"token":"token"} rubbish`, `{"token":"token"}{}`, `{"token":"token","unexpected":true}`} {
		w := httptest.NewRecorder()
		a.serveSession(w, httptest.NewRequest("POST", "/api/v1/session", strings.NewReader(body)))
		if w.Code != 400 {
			t.Fatalf("malformed body accepted: %d", w.Code)
		}
	}
}
func TestRemoteRuntimeRequiresMutualTLS(t *testing.T) {
	for _, u := range []string{"http://runtime:8080", "https://runtime:8080", "file:///etc/passwd", "http://user:password@localhost:8080"} {
		if _, _, err := runtimeTransport(RuntimeConfig{URL: u}); err == nil {
			t.Fatalf("unsafe or uncredentialed runtime accepted: %s", u)
		}
	}
}
