// Package gateway is the authenticated, workspace-scoped public LensDock edge.
package gateway

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type Identity struct {
	ID          string `json:"id"`
	Role        string `json:"role"`
	WorkspaceID string `json:"workspaceId"`
	TokenSHA256 string `json:"tokenSHA256,omitempty"`
}

const (
	sessionCookie   = "lensdock-session"
	demoOffCookie   = "lensdock-demo-off"
	demoActorID     = "demo-human"
	sessionTTL      = 8 * time.Hour
	maxSessions     = 1024
	maxDemoSessions = 16
)

// session.Expires is meaningful only for token sessions; demo sessions never expire.
type session struct {
	Actor   Identity
	CSRF    string
	Expires time.Time
	Created time.Time
	Demo    bool
}

type identities struct {
	mu       sync.Mutex
	users    []Identity
	sessions map[string]session
	origin   string
	host     string
	secure   bool
	demo     *Identity
}

func randomToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
func digest(s string) string { b := sha256.Sum256([]byte(s)); return hex.EncodeToString(b[:]) }
func equal(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }
func newIdentities(users []Identity, origin string) (*identities, error) {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, errors.New("gateway: origin must be an exact HTTP(S) origin")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1")) {
		return nil, errors.New("gateway: remote origin requires HTTPS")
	}
	seen := map[string]bool{}
	for _, p := range users {
		if p.ID == "" || seen[p.ID] || p.WorkspaceID == "" {
			return nil, errors.New("gateway: identities require unique IDs and workspace scope")
		}
		seen[p.ID] = true
		if p.Role != "viewer" && p.Role != "human" && p.Role != "agent" && p.Role != "admin" {
			return nil, errors.New("gateway: invalid identity role")
		}
		b, e := hex.DecodeString(p.TokenSHA256)
		if e != nil || len(b) != 32 {
			return nil, errors.New("gateway: invalid identity token digest")
		}
	}
	if len(users) == 0 {
		return nil, errors.New("gateway: at least one identity required")
	}
	return &identities{users: users, sessions: make(map[string]session), origin: origin, secure: u.Scheme == "https"}, nil
}
func (a *identities) bearer(token string) (Identity, bool) {
	h := digest(token)
	for _, u := range a.users {
		if equal(h, u.TokenSHA256) {
			u.TokenSHA256 = ""
			return u, true
		}
	}
	return Identity{}, false
}
func (a *identities) sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin != "" {
		return origin == a.origin
	}
	// Fetch metadata supplies an additional browser-only defense. Non-browser
	// bearer clients do not send it and still require a credential.
	site := r.Header.Get("Sec-Fetch-Site")
	return site == "" || site == "same-origin" || site == "none"
}
func (a *identities) authenticate(r *http.Request) (Identity, bool) {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return a.bearer(strings.TrimPrefix(h, "Bearer "))
	}
	s, ok := a.lookup(r)
	if !ok || !a.sameOrigin(r) {
		return Identity{}, false
	}
	if r.Method != "GET" && r.Method != "HEAD" && !equal(r.Header.Get("X-CSRF-Token"), s.CSRF) {
		return Identity{}, false
	}
	return s.Actor, true
}
func (a *identities) lookup(r *http.Request) (session, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return session{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	key := digest(c.Value)
	s, ok := a.sessions[key]
	if ok && !s.Demo && !time.Now().Before(s.Expires) {
		delete(a.sessions, key)
		return session{}, false
	}
	return s, ok
}

// enableDemo turns on explicit local demo no-auth for one workspace. It only
// accepts a loopback public origin and never overrides a configured identity.
func (a *identities) enableDemo(workspaceID string) error {
	u, err := url.Parse(a.origin)
	if err != nil {
		return errors.New("gateway: invalid origin")
	}
	if h := u.Hostname(); h != "127.0.0.1" && h != "localhost" && h != "::1" {
		return errors.New("gateway: demo no-auth requires a loopback origin")
	}
	if workspaceID == "" {
		return errors.New("gateway: demo no-auth requires one workspace")
	}
	for _, p := range a.users {
		if p.ID == demoActorID {
			return errors.New("gateway: identity ID reserved for demo no-auth")
		}
	}
	a.demo = &Identity{ID: demoActorID, Role: "human", WorkspaceID: workspaceID}
	a.host = u.Host
	return nil
}

// demoFetch accepts only a same-origin browser fetch()/XHR to the configured
// Host. Browsers cannot forge Sec-Fetch-*; cross-site pages, subresource loads,
// navigations and non-browser clients all fail it.
func (a *identities) demoFetch(r *http.Request) bool {
	h := r.Header
	if r.Host != a.host || h.Get("Sec-Fetch-Site") != "same-origin" || h.Get("Sec-Fetch-Mode") != "cors" || h.Get("Sec-Fetch-Dest") != "empty" {
		return false
	}
	if o := h.Get("Origin"); o != "" && o != a.origin {
		return false
	}
	return r.Method != "POST" || h.Get("Origin") == a.origin
}
func jsonReply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, code, message string) {
	id, _ := randomToken()
	w.Header().Set("X-Request-Id", id)
	jsonReply(w, status, map[string]string{"code": code, "message": message, "requestId": id})
}
func decode(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		fail(w, 400, "INVALID_ARGUMENT", "invalid request body")
		return false
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		fail(w, 400, "INVALID_ARGUMENT", "invalid trailing JSON")
		return false
	}
	return true
}

// serveSession handles pairing, probing and logout. Demo no-auth (enableDemo)
// adds exactly two ways to obtain a session without a token, both limited to
// same-origin browser fetches: GET (automatic entry) and POST {"demo":true}
// (explicit entry after a demo logout). Every other path is unchanged.
func (a *identities) serveSession(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "POST":
		if !a.sameOrigin(r) {
			fail(w, 403, "FORBIDDEN", "origin rejected")
			return
		}
		var in struct {
			Token string `json:"token"`
			Demo  bool   `json:"demo,omitempty"`
		}
		if !decode(w, r, &in, 8192) {
			return
		}
		if in.Demo {
			if in.Token != "" {
				fail(w, 400, "INVALID_ARGUMENT", "demo entry takes no token")
				return
			}
			if a.demo == nil {
				fail(w, 401, "UNAUTHENTICATED", "human or viewer pairing token required")
				return
			}
			if !a.demoFetch(r) {
				fail(w, 403, "FORBIDDEN", "origin rejected")
				return
			}
			a.issue(w, *a.demo, true)
			return
		}
		actor, ok := a.bearer(in.Token)
		if !ok || actor.Role == "agent" {
			fail(w, 401, "UNAUTHENTICATED", "human or viewer pairing token required")
			return
		}
		a.issue(w, actor, false)
	case "GET":
		s, ok := a.lookup(r)
		if ok && a.sameOrigin(r) {
			reply := map[string]any{"actor": s.Actor, "csrfToken": s.CSRF}
			if s.Demo {
				reply["demo"] = true
			}
			jsonReply(w, 200, reply)
			return
		}
		if a.demo != nil && a.demoFetch(r) {
			// A demo logout leaves a marker so a reload does not silently re-enter;
			// only the explicit POST above does, and it clears the marker.
			if _, err := r.Cookie(demoOffCookie); err == nil {
				fail(w, 401, "DEMO_SIGNED_OUT", "demo sign-in is available")
				return
			}
			a.issue(w, *a.demo, true)
			return
		}
		fail(w, 401, "UNAUTHENTICATED", "session required")
	case "DELETE":
		s, ok := a.lookup(r)
		if !ok || !a.sameOrigin(r) || !equal(r.Header.Get("X-CSRF-Token"), s.CSRF) {
			fail(w, 403, "FORBIDDEN", "valid session and CSRF required")
			return
		}
		c, _ := r.Cookie(sessionCookie)
		a.mu.Lock()
		delete(a.sessions, digest(c.Value))
		a.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteStrictMode, MaxAge: -1})
		if s.Demo {
			http.SetCookie(w, &http.Cookie{Name: demoOffCookie, Value: "1", Path: "/", HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteStrictMode})
		}
		w.WriteHeader(204)
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		fail(w, 405, "METHOD_NOT_ALLOWED", "method not allowed")
	}
}

// issue creates one session and its cookie. Token sessions expire after
// sessionTTL. Demo sessions never expire server-side and use a browser-session
// cookie (no Max-Age); their count is capped by evicting the oldest demo session.
func (a *identities) issue(w http.ResponseWriter, actor Identity, demo bool) {
	token, err := randomToken()
	if err != nil {
		fail(w, 500, "INTERNAL", "session creation failed")
		return
	}
	csrf, err := randomToken()
	if err != nil {
		fail(w, 500, "INTERNAL", "session creation failed")
		return
	}
	now := time.Now()
	s := session{Actor: actor, CSRF: csrf, Created: now, Demo: demo}
	if !demo {
		s.Expires = now.Add(sessionTTL)
	}
	a.mu.Lock()
	demos, oldest := 0, ""
	for k, v := range a.sessions {
		if !v.Demo && !now.Before(v.Expires) {
			delete(a.sessions, k)
			continue
		}
		if v.Demo {
			demos++
			if oldest == "" || v.Created.Before(a.sessions[oldest].Created) {
				oldest = k
			}
		}
	}
	if demo && demos >= maxDemoSessions {
		delete(a.sessions, oldest)
	}
	if len(a.sessions) >= maxSessions {
		a.mu.Unlock()
		fail(w, 429, "RESOURCE_LIMIT", "session capacity reached")
		return
	}
	a.sessions[digest(token)] = s
	a.mu.Unlock()
	cookie := &http.Cookie{Name: sessionCookie, Value: token, Path: "/", HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteStrictMode}
	if !demo {
		cookie.MaxAge = int(sessionTTL / time.Second)
	}
	http.SetCookie(w, cookie)
	reply := map[string]any{"actor": actor, "csrfToken": csrf}
	if demo {
		http.SetCookie(w, &http.Cookie{Name: demoOffCookie, Value: "", Path: "/", HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteStrictMode, MaxAge: -1})
		reply["demo"] = true
	}
	jsonReply(w, 200, reply)
}
