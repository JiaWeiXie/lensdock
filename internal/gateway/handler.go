package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
	"github.com/JiaWeiXie/lensdock/internal/feedback"
)

// Integrations keep persistence and feedback transactions outside the edge.
// A failed grant hook revokes the lease before a capability reaches the client.
type Integrations struct {
	Feedback         func(http.ResponseWriter, *http.Request, Identity)
	BeforeAgentGrant func(context.Context, Identity, string, int) (int, error)
	AgentGranted     func(context.Context, Identity, string, int, apiwire.Lease) error
	Audit            func(context.Context, Identity, string, string) error
	FeedbackStats    func(context.Context, Identity) (feedback.Stats, error)
}
type Gateway struct {
	config       Config
	identities   *identities
	runtimes     map[string]*runtimeEdge
	leases       leaseRegistry
	integrations Integrations
	static       http.Handler
	schema       map[string]int
	streams      streamGroup
}

func New(c Config, d Integrations) (*Gateway, error) {
	identities, err := newIdentities(c.Identities, c.Origin, c.DeploymentID)
	if err != nil {
		return nil, err
	}
	if c.SessionLimits.TTLSeconds < 0 || c.SessionLimits.TTLSeconds > int64((1<<63-1)/time.Second) || c.SessionLimits.MaxSessions < 0 {
		return nil, errors.New("gateway: session limits must be nonnegative and session TTL must fit a duration")
	}
	identities.limits = c.SessionLimits
	origin, _ := url.Parse(c.Origin)
	local := origin.Hostname() == "127.0.0.1" || origin.Hostname() == "localhost" || origin.Hostname() == "::1"
	if c.LocalAutoEntry && !(local && len(c.Runtimes) == 1) {
		return nil, errors.New("gateway: local auto-entry requires a loopback origin and exactly one workspace; every other Gateway requires pairing")
	}
	switch {
	case c.LocalAutoEntry && c.RequirePairing:
		slog.Warn("local auto-entry is configured but pairing is required; pairing wins")
	case c.LocalAutoEntry:
		if err := identities.enableLocalAccess(c.Runtimes[0].WorkspaceID); err != nil {
			return nil, err
		}
		slog.Warn("local auto-entry is enabled: any process on this host that can reach the Gateway can open a human session without a token")
	default:
		slog.Info("pairing is required for every browser session")
	}
	g := &Gateway{config: c, identities: identities, runtimes: map[string]*runtimeEdge{}, leases: leaseRegistry{byToken: map[string]ownedLease{}}, integrations: d}
	identities.audit = func(r *http.Request, p Identity, action string) { g.record(r.Context(), p, action) }
	for _, rc := range c.Runtimes {
		if rc.WorkspaceID == "" || strings.ContainsAny(rc.WorkspaceID, "/\\.\x00") || g.runtimes[rc.WorkspaceID] != nil {
			return nil, errors.New("gateway: invalid or duplicate workspace")
		}
		e, err := newRuntimeEdge(rc, &g.streams)
		if err != nil {
			return nil, err
		}
		g.runtimes[rc.WorkspaceID] = e
		e.publicOrigin, _ = url.Parse(c.Origin)
	}
	for _, p := range c.Identities {
		if g.runtimes[p.WorkspaceID] == nil {
			return nil, errors.New("gateway: identity references unknown workspace")
		}
	}
	if err := validateAgentDefaults(c); err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(c.WebDir, "index.html")); err != nil {
		return nil, errors.New("gateway: built web index unavailable")
	}
	g.static = http.FileServer(http.Dir(c.WebDir))
	return g, nil
}
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' blob: data:; font-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
	if g.identities.secure {
		w.Header().Set("Strict-Transport-Security", "max-age=31536000")
	}
	origin, _ := url.Parse(g.config.Origin)
	if r.Host != origin.Host {
		fail(w, 421, "MISDIRECTED_REQUEST", "unexpected host")
		return
	}
	if r.URL.Path == "/healthz" && r.Method == "GET" {
		jsonReply(w, 200, map[string]bool{"ok": true})
		return
	}
	if r.URL.Path == "/api/v1/deployment" {
		if r.Method != "GET" {
			w.Header().Set("Allow", "GET")
			fail(w, 405, "METHOD_NOT_ALLOWED", "method not allowed")
			return
		}
		jsonReply(w, 200, map[string]string{"deploymentId": g.config.DeploymentID})
		return
	}
	if r.URL.Path == "/readyz" && r.Method == "GET" {
		ready := g.integrations.Feedback != nil
		for _, e := range g.runtimes {
			status, _, _, err := e.call(r.Context(), "GET", "/readyz", e.operator, "", nil, nil)
			if err != nil || status != 200 {
				ready = false
			}
		}
		status := 200
		if !ready {
			status = 503
		}
		jsonReply(w, status, map[string]bool{"ready": ready})
		return
	}
	if r.URL.Path == "/api/v1/session" {
		if r.Method == "DELETE" {
			if p, ok := g.identities.authenticate(r); ok {
				g.revokeActor(r.Context(), p)
			}
		}
		g.identities.serveSession(w, r)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		if r.Method != "GET" && r.Method != "HEAD" {
			fail(w, 405, "METHOD_NOT_ALLOWED", "read only")
			return
		}
		if r.URL.Path != "/" && strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		g.static.ServeHTTP(w, r)
		return
	}
	p, ok := g.identities.authenticate(r)
	if !ok {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if l, found := g.leases.get(token); found && g.identities.sameOrigin(r) {
			p = l.Actor
			ok = true
		}
	}
	if !ok {
		fail(w, 401, "UNAUTHENTICATED", "valid workspace credentials required")
		return
	}
	audit := &auditResponse{ResponseWriter: w}
	w = audit
	defer func() {
		if audit.status == 401 || audit.status == 403 || audit.status == 404 {
			g.record(r.Context(), p, "access.denied")
		}
	}()
	if !g.identities.sameOrigin(r) {
		fail(w, 403, "FORBIDDEN", "origin rejected")
		return
	}
	if r.URL.Query().Has("token") || r.URL.Query().Has("access_token") {
		fail(w, 400, "INVALID_REQUEST", "credentials in URLs are forbidden")
		return
	}
	r, releaseMode, ok := g.guardMode(w, r, p)
	if !ok {
		return
	}
	defer releaseMode()
	if r.URL.Path == "/api/v1/system" && r.Method == "GET" {
		g.system(w, r, p)
		return
	}
	if r.URL.Path == "/api/v1/browser-destinations" {
		g.browserDestinations(w, r, p)
		return
	}
	if r.URL.Path == "/api/v1/workspaces" && r.Method == "GET" {
		jsonReply(w, 200, map[string]any{"workspaces": []map[string]any{g.workspaceSnapshot(r.Context(), p.WorkspaceID)}})
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/v1/workspaces/") {
		path := strings.TrimPrefix(r.URL.Path, "/api/v1/workspaces/")
		ws, suffix, _ := strings.Cut(path, "/")
		if ws != p.WorkspaceID || g.runtimes[ws] == nil {
			fail(w, 404, "NOT_FOUND", "workspace not found")
			return
		}
		if suffix == "captures" {
			g.feedback(w, r, p)
			return
		}
		g.workspace(w, r, p, g.runtimes[ws], suffix)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/v1/feedback") || strings.HasPrefix(r.URL.Path, "/api/v1/captures/") || r.URL.Path == "/api/v1/events" || r.URL.Path == "/api/v1/audit" {
		g.feedback(w, r, p)
		return
	}
	fail(w, 404, "NOT_FOUND", "no such route")
}
func (g *Gateway) feedback(w http.ResponseWriter, r *http.Request, p Identity) {
	if g.integrations.Feedback == nil {
		fail(w, 503, "UNAVAILABLE", "feedback storage unavailable")
		return
	}
	g.integrations.Feedback(w, r, p)
}
func human(p Identity) bool { return p.Role == "human" || p.Role == "admin" }
func (g *Gateway) workspace(w http.ResponseWriter, r *http.Request, p Identity, e *runtimeEdge, suffix string) {
	switch {
	case suffix == "agent" || strings.HasPrefix(suffix, "agent/"):
		g.agentRoute(w, r, p, e, suffix)
	case suffix == "leases" && r.Method == "POST":
		g.acquire(w, r, p, e, false)
	case suffix == "takeover" && r.Method == "POST":
		g.acquire(w, r, p, e, true)
	case suffix == "takeover" && (r.Method == "GET" || r.Method == "DELETE"):
		if !human(p) {
			fail(w, 403, "FORBIDDEN", "human takeover required")
			return
		}
		e.stream(w, r, e.operator, p.ID)
	case suffix == "leases/current" && r.Method == "GET":
		g.currentLease(w, r, p, e)
	case suffix == "health" && r.Method == "GET":
		clone := r.Clone(r.Context())
		u := *r.URL
		u.Path = "/readyz"
		u.RawQuery = "detail=1"
		clone.URL = &u
		e.stream(w, clone, e.operator, p.ID)
	case suffix == "maintenance" && (r.Method == "POST" || r.Method == "DELETE"):
		if p.Role != "admin" {
			fail(w, 403, "FORBIDDEN", "administrator required")
			return
		}
		e.stream(w, r, e.operator, p.ID)
	case strings.HasPrefix(suffix, "leases/"):
		g.leaseAction(w, r, p, e, suffix)
	case suffix == "" && r.Method == "GET":
		jsonReply(w, 200, g.workspaceSnapshot(r.Context(), p.WorkspaceID))
	case suffix == "targets" && r.Method == "GET", suffix == "files" && r.Method == "GET", suffix == "resolution" && r.Method == "GET":
		if suffix == "files" && !human(p) {
			fail(w, 403, "FORBIDDEN", "human file access required")
			return
		}
		e.stream(w, r, e.operator, p.ID)
	case suffix == "vnc/view" && r.Method == "GET":
		if p.Role == "agent" {
			fail(w, 403, "FORBIDDEN", "agent uses scoped CDP only")
			return
		}
		e.stream(w, r, e.operator, p.ID)
	case suffix == "vnc/control" && r.Method == "GET", suffix == "clipboard" && r.Method == "PUT", suffix == "resolution" && r.Method == "PUT":
		l, ok := g.requestLease(r, p)
		// The desktop (Chromium toolbar, menus, native dialogs) and its
		// clipboard belong to the lease holder: a human holds a HUMAN lease,
		// an Agent an AGENT lease. Resizing the display stays with humans.
		if !ok || !desktopLease(p, l, suffix != "resolution") {
			fail(w, 403, "LEASE_REQUIRED", "active lease of your role required")
			return
		}
		if p.Role == "agent" && r.Header.Get("Origin") != "" {
			fail(w, 403, "ORIGIN_NOT_ALLOWED", "browser access with an agent credential is forbidden")
			return
		}
		if suffix == "vnc/control" {
			epoch := strconv.FormatUint(l.Grant.Lease.Epoch, 10)
			if r.URL.Query().Get("epoch") != epoch || r.URL.Query().Get("generation") != l.Grant.Lease.RuntimeGeneration {
				fail(w, 412, "EPOCH_MISMATCH", "control socket requires the current lease epoch and generation")
				return
			}
			r = r.Clone(r.Context())
			r.Header = r.Header.Clone()
			r.Header.Set("X-LensDock-Lease-Epoch", epoch)
			r.Header.Set("X-LensDock-Runtime-Generation", l.Grant.Lease.RuntimeGeneration)
			u := *r.URL
			u.RawQuery = ""
			r.URL = &u
		}
		e.stream(w, r, l.Grant.CapabilityToken, p.ID)
	case strings.HasPrefix(suffix, "cdp/"):
		l, ok := g.requestLease(r, p)
		if !ok || p.Role != "agent" || l.Grant.Lease.Mode != "AGENT" {
			fail(w, 403, "LEASE_REQUIRED", "active agent lease required")
			return
		}
		if r.Header.Get("Origin") != "" {
			fail(w, 403, "ORIGIN_NOT_ALLOWED", "browser CDP access forbidden")
			return
		}
		if r.Header.Get("X-LensDock-Lease-Epoch") != strconv.FormatUint(l.Grant.Lease.Epoch, 10) || r.Header.Get("X-LensDock-Runtime-Generation") != l.Grant.Lease.RuntimeGeneration {
			fail(w, 412, "EPOCH_MISMATCH", "CDP requires the current lease epoch and generation")
			return
		}
		e.stream(w, r, l.Grant.CapabilityToken, p.ID)
	case strings.HasPrefix(suffix, "files/"):
		if !human(p) {
			fail(w, 403, "FORBIDDEN", "human file access required")
			return
		}
		e.stream(w, r, e.operator, p.ID)
	default:
		fail(w, 404, "NOT_FOUND", "no such workspace route")
	}
}

// desktopLease reports whether the lease may drive the workspace desktop for
// the identity: HUMAN lease for a human or admin, AGENT lease for an Agent
// when agentOK.
func desktopLease(p Identity, l ownedLease, agentOK bool) bool {
	switch l.Grant.Lease.Mode {
	case "HUMAN":
		return human(p)
	case "AGENT":
		return agentOK && p.Role == "agent"
	}
	return false
}

func (g *Gateway) requestLease(r *http.Request, p Identity) (ownedLease, bool) {
	tok := r.Header.Get("X-LensDock-Lease")
	if tok == "" {
		tok = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	if tok != "" {
		if l, ok := g.leases.get(tok); ok && l.Actor.ID == p.ID && l.Actor.WorkspaceID == p.WorkspaceID {
			return l, true
		}
		if _, identityToken := g.identities.bearer(tok); !identityToken {
			return ownedLease{}, false
		}
	}
	return g.leases.forActor(p)
}
func (g *Gateway) acquire(w http.ResponseWriter, r *http.Request, p Identity, e *runtimeEdge, takeover bool) {
	var in struct {
		Mode             string `json:"mode,omitempty"`
		TTLSeconds       *int   `json:"ttlSeconds,omitempty"`
		FeedbackID       string `json:"feedbackId,omitempty"`
		FeedbackRevision int    `json:"feedbackRevision,omitempty"`
		Reason           string `json:"reason,omitempty"`
		Force            bool   `json:"force,omitempty"`
	}
	if !decode(w, r, &in, 8192) {
		return
	}
	e.control.Lock()
	defer e.control.Unlock()
	if takeover {
		in.Mode = "HUMAN"
	}
	if (in.Mode == "HUMAN" && !human(p)) || (in.Mode == "AGENT" && p.Role != "agent") || (in.Mode != "HUMAN" && in.Mode != "AGENT") {
		fail(w, 403, "FORBIDDEN", "role cannot acquire requested control mode")
		return
	}
	if in.FeedbackRevision < 0 || (in.FeedbackRevision != 0 && in.FeedbackID == "") {
		fail(w, 400, "INVALID_REQUEST", "feedbackRevision must be positive and requires feedbackId")
		return
	}
	if in.FeedbackID != "" {
		if in.Mode != "AGENT" || g.integrations.BeforeAgentGrant == nil {
			fail(w, 400, "INVALID_REQUEST", "feedback requires an agent lease")
			return
		}
		revision, err := g.integrations.BeforeAgentGrant(r.Context(), p, in.FeedbackID, in.FeedbackRevision)
		if err != nil {
			fail(w, 409, "FEEDBACK_CONFLICT", err.Error())
			return
		}
		in.FeedbackRevision = revision
	}
	var body []byte
	if takeover {
		body, _ = json.Marshal(map[string]any{"reason": in.Reason, "force": in.Force})
	} else {
		body, _ = json.Marshal(map[string]any{"mode": in.Mode, "ttlSeconds": in.TTLSeconds})
	}
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		fail(w, 400, "INVALID_REQUEST", "Idempotency-Key required")
		return
	}
	headers.Set("Idempotency-Key", digest(p.ID+":"+key))
	if id := r.Header.Get("X-LensDock-Takeover-ID"); id != "" {
		headers.Set("X-LensDock-Takeover-ID", id)
	}
	status, h, b, err := e.call(r.Context(), "POST", r.URL.Path, e.operator, p.ID, body, headers)
	if err != nil {
		fail(w, 502, "UPSTREAM_UNAVAILABLE", "lease request failed")
		return
	}
	if takeover {
		if status == 202 {
			g.leases.removeWorkspace(p.WorkspaceID)
			g.record(r.Context(), p, "takeover.started")
		}
		forwardReply(w, status, h, b)
		return
	}
	if status >= 200 && status < 300 {
		grant, err := decodeGrant(b)
		if err != nil {
			fail(w, 502, "UPSTREAM_INVALID", "invalid runtime lease response")
			return
		}
		if in.FeedbackID != "" && g.integrations.AgentGranted != nil {
			if err := g.integrations.AgentGranted(r.Context(), p, in.FeedbackID, in.FeedbackRevision, grant.Lease); err != nil {
				_, _, _, _ = e.call(r.Context(), "DELETE", "/api/v1/workspaces/"+p.WorkspaceID+"/leases/"+grant.Lease.LeaseID, e.operator, p.ID, nil, nil)
				fail(w, 409, "FEEDBACK_CONFLICT", "feedback transition failed; lease revoked")
				return
			}
		}
		g.leases.removeWorkspace(p.WorkspaceID)
		g.leases.put(ownedLease{Actor: p, Grant: grant, FeedbackID: in.FeedbackID})
		g.record(r.Context(), p, "lease.acquired")
		if r.Header.Get("Authorization") == "" {
			jsonReply(w, status, map[string]any{"lease": grant.Lease})
			return
		}
	}
	forwardReply(w, status, h, b)
}
func (g *Gateway) leaseAction(w http.ResponseWriter, r *http.Request, p Identity, e *runtimeEdge, suffix string) {
	e.control.Lock()
	defer e.control.Unlock()
	l, ok := g.requestLease(r, p)
	if !ok {
		fail(w, 403, "LEASE_REQUIRED", "owned lease required")
		return
	}
	parts := strings.Split(suffix, "/")
	if len(parts) < 2 || parts[1] != l.Grant.Lease.LeaseID {
		fail(w, 404, "NOT_FOUND", "lease not found")
		return
	}
	renew := len(parts) == 3 && parts[2] == "renew" && r.Method == "POST"
	release := len(parts) == 2 && r.Method == "DELETE"
	if !renew && !release {
		fail(w, 405, "METHOD_NOT_ALLOWED", "invalid lease operation")
		return
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8192))
	if err != nil {
		fail(w, 400, "INVALID_REQUEST", "body exceeds limit")
		return
	}
	status, h, out, err := e.call(r.Context(), r.Method, r.URL.Path, l.Grant.CapabilityToken, p.ID, b, r.Header)
	if err != nil {
		fail(w, 502, "UPSTREAM_UNAVAILABLE", "lease operation failed")
		return
	}
	if status >= 200 && status < 300 {
		if release {
			g.leases.removeWorkspace(p.WorkspaceID)
			g.record(r.Context(), p, "lease.released")
		} else {
			var renewed apiwire.Lease
			if json.Unmarshal(out, &renewed) == nil {
				l.Grant.Lease = renewed
				g.leases.put(l)
			}
		}
	}
	forwardReply(w, status, h, out)
}
func (g *Gateway) currentLease(w http.ResponseWriter, r *http.Request, p Identity, e *runtimeEdge) {
	l, ok := g.requestLease(r, p)
	if !ok {
		fail(w, 403, "LEASE_REQUIRED", "owned lease required")
		return
	}
	status, h, b, err := e.call(r.Context(), "GET", r.URL.Path, l.Grant.CapabilityToken, p.ID, nil, nil)
	if err != nil {
		fail(w, 502, "UPSTREAM_UNAVAILABLE", "lease lookup failed")
		return
	}
	if status == 200 {
		var current apiwire.Lease
		if err := json.Unmarshal(b, &current); err != nil {
			fail(w, 502, "UPSTREAM_INVALID", "invalid runtime lease")
			return
		}
		l.Grant.Lease = current
		g.leases.put(l)
	}
	forwardReply(w, status, h, b)
}
func (g *Gateway) revokeActor(ctx context.Context, p Identity) {
	e := g.runtimes[p.WorkspaceID]
	e.control.Lock()
	defer e.control.Unlock()
	if l, ok := g.leases.forActor(p); ok {
		_, _, _, _ = e.call(ctx, "DELETE", "/api/v1/workspaces/"+p.WorkspaceID+"/leases/"+l.Grant.Lease.LeaseID, e.operator, p.ID, nil, nil)
		g.leases.removeWorkspace(p.WorkspaceID)
	}
}
func (g *Gateway) Close() {
	for _, e := range g.runtimes {
		e.client.CloseIdleConnections()
	}
}
func Server(c Config, h http.Handler) *http.Server {
	return &http.Server{Addr: c.Listen, Handler: h, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
}
