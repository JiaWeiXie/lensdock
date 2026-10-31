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

const localActorID = "local-human"

// SessionLimits are optional session restrictions; zero leaves each dimension unlimited.
type SessionLimits struct {
	TTLSeconds  int64 `json:"ttlSeconds,omitempty"`
	MaxSessions int   `json:"maxSessions,omitempty"`
}

type session struct {
	Actor       Identity
	CSRF        string
	Expires     time.Time
	LocalAccess bool
}

// identities holds sessions. sessionCookie and localOffCookie are namespaced by
// origin and deployment ID so same-host Gateways on other ports do not clobber
// each other's cookies.
type identities struct {
	mu             sync.Mutex
	users          []Identity
	sessions       map[string]session
	origin         string
	host           string
	secure         bool
	sessionCookie  string
	localOffCookie string
	localAccess    *Identity
	limits         SessionLimits
	// audit records security-relevant session events; nil in unit tests.
	audit func(*http.Request, Identity, string)
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

// cookieNames derives the session and local-signout cookie names from the exact
// configured origin and deployment ID. Cookies are shared across ports of one
// host, so Gateways on the same hostname must not use the same names. Hashing
// keeps the names stable, valid cookie tokens and free of configuration text.
// An empty deploymentID yields an origin-only namespace.
func cookieNames(origin, deploymentID string) (sessionName, localOffName string) {
	sum := sha256.Sum256([]byte("lensdock-cookie-namespace-v1\x00" + origin + "\x00" + deploymentID))
	ns := hex.EncodeToString(sum[:16])
	return "lensdock-session-" + ns, "lensdock-local-off-" + ns
}

func newIdentities(users []Identity, origin, deploymentID string) (*identities, error) {
	if err := ValidateDeploymentID(deploymentID); err != nil {
		return nil, err
	}
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
	sessionCookie, localOffCookie := cookieNames(origin, deploymentID)
	return &identities{users: users, sessions: make(map[string]session), origin: origin, secure: u.Scheme == "https", sessionCookie: sessionCookie, localOffCookie: localOffCookie}, nil
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
	c, err := r.Cookie(a.sessionCookie)
	if err != nil {
		return session{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	key := digest(c.Value)
	s, ok := a.sessions[key]
	if ok && !s.Expires.IsZero() && !time.Now().Before(s.Expires) {
		delete(a.sessions, key)
		return session{}, false
	}
	return s, ok
}

// enableLocalAccess lets same-origin local browsers enter one workspace without
// pairing. It never overrides a configured identity.
func (a *identities) enableLocalAccess(workspaceID string) error {
	u, err := url.Parse(a.origin)
	if err != nil {
		return errors.New("gateway: invalid origin")
	}
	if h := u.Hostname(); h != "127.0.0.1" && h != "localhost" && h != "::1" {
		return errors.New("gateway: automatic access requires a loopback origin")
	}
	if workspaceID == "" {
		return errors.New("gateway: automatic access requires one workspace")
	}
	for _, p := range a.users {
		if p.ID == localActorID {
			return errors.New("gateway: identity ID reserved for automatic local access")
		}
	}
	a.localAccess = &Identity{ID: localActorID, Role: "human", WorkspaceID: workspaceID}
	a.host = u.Host
	return nil
}

// localFetch accepts only a same-origin browser fetch()/XHR to the configured
// Host. Browsers cannot forge Sec-Fetch-*, so cross-site pages, subresource loads
// and navigations fail it. It does not authenticate the caller: any non-browser
// client, such as a local program or a client of a local proxy, can send these
// headers. Browser protections (Sec-Fetch-*, Origin checks, Local Network Access
// prompts) bind web pages only and are no defence against such clients. This is
// why local entry is an explicit opt-in (Config.LocalAutoEntry).
func (a *identities) localFetch(r *http.Request) bool {
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

// serveSession handles pairing, probing and logout. Local access adds automatic
// GET entry and explicit POST {"localAccess":true} re-entry after logout.
func (a *identities) serveSession(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "POST":
		if !a.sameOrigin(r) {
			fail(w, 403, "FORBIDDEN", "origin rejected")
			return
		}
		var in struct {
			Token       string `json:"token"`
			LocalAccess bool   `json:"localAccess,omitempty"`
		}
		if !decode(w, r, &in, 8192) {
			return
		}
		if in.LocalAccess {
			if in.Token != "" {
				fail(w, 400, "INVALID_ARGUMENT", "local entry takes no token")
				return
			}
			if a.localAccess == nil {
				fail(w, 401, "UNAUTHENTICATED", "human or viewer pairing token required")
				return
			}
			if !a.localFetch(r) {
				fail(w, 403, "FORBIDDEN", "origin rejected")
				return
			}
			a.issue(w, r, *a.localAccess, true)
			return
		}
		actor, ok := a.bearer(in.Token)
		if !ok || actor.Role == "agent" {
			fail(w, 401, "UNAUTHENTICATED", "human or viewer pairing token required")
			return
		}
		a.issue(w, r, actor, false)
	case "GET":
		s, ok := a.lookup(r)
		if ok && a.sameOrigin(r) {
			reply := map[string]any{"actor": s.Actor, "csrfToken": s.CSRF}
			if s.LocalAccess {
				reply["localAccess"] = true
			}
			jsonReply(w, 200, reply)
			return
		}
		if a.localAccess != nil && a.localFetch(r) {
			// Logout remains effective across reloads until explicit re-entry.
			if _, err := r.Cookie(a.localOffCookie); err == nil {
				fail(w, 401, "LOCAL_SIGNED_OUT", "local sign-in is available")
				return
			}
			a.issue(w, r, *a.localAccess, true)
			return
		}
		fail(w, 401, "UNAUTHENTICATED", "session required")
	case "DELETE":
		s, ok := a.lookup(r)
		if !ok || !a.sameOrigin(r) || !equal(r.Header.Get("X-CSRF-Token"), s.CSRF) {
			fail(w, 403, "FORBIDDEN", "valid session and CSRF required")
			return
		}
		c, _ := r.Cookie(a.sessionCookie)
		a.mu.Lock()
		delete(a.sessions, digest(c.Value))
		a.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: a.sessionCookie, Value: "", Path: "/", HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteStrictMode, MaxAge: -1})
		if s.LocalAccess {
			http.SetCookie(w, &http.Cookie{Name: a.localOffCookie, Value: "1", Path: "/", HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteStrictMode})
		}
		w.WriteHeader(204)
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		fail(w, 405, "METHOD_NOT_ALLOWED", "method not allowed")
	}
}

// issue creates a browser session, applying only explicitly configured limits.
func (a *identities) issue(w http.ResponseWriter, r *http.Request, actor Identity, localAccess bool) {
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
	s := session{Actor: actor, CSRF: csrf, LocalAccess: localAccess}
	if a.limits.TTLSeconds > 0 {
		s.Expires = now.Add(time.Duration(a.limits.TTLSeconds) * time.Second)
	}
	a.mu.Lock()
	if a.limits.TTLSeconds > 0 {
		for k, v := range a.sessions {
			if !v.Expires.IsZero() && !now.Before(v.Expires) {
				delete(a.sessions, k)
			}
		}
	}
	if a.limits.MaxSessions > 0 && len(a.sessions) >= a.limits.MaxSessions {
		a.mu.Unlock()
		fail(w, 429, "RESOURCE_LIMIT", "configured session capacity reached")
		return
	}
	a.sessions[digest(token)] = s
	a.mu.Unlock()
	if localAccess && a.audit != nil {
		a.audit(r, actor, "session.local")
	}
	cookie := &http.Cookie{Name: a.sessionCookie, Value: token, Path: "/", HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteStrictMode}
	if a.limits.TTLSeconds > 0 {
		cookie.MaxAge = int(a.limits.TTLSeconds)
	}
	http.SetCookie(w, cookie)
	reply := map[string]any{"actor": actor, "csrfToken": csrf}
	if localAccess {
		http.SetCookie(w, &http.Cookie{Name: a.localOffCookie, Value: "", Path: "/", HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteStrictMode, MaxAge: -1})
		reply["localAccess"] = true
	}
	jsonReply(w, 200, reply)
}
