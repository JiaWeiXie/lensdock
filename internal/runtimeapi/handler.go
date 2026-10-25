// Package runtimeapi is the runtime container's HTTP API: health probes, the
// lease endpoints, takeover and maintenance, the lease-gated clipboard,
// display resize and CDP proxy, the VNC WebSocket proxies, page-target
// listing and viewport capture, and the controlled file transfer area.
//
// Two credentials exist. The operator token (the Gateway's identity) opens
// the operator routes; a lease capability token opens the routes bound to a
// lease (CDP, VNC control, clipboard, resolution). Lease-bound routes also
// compare the optional X-LensDock-Lease-Epoch and X-LensDock-Runtime-Generation
// headers with the live lease, and require the epoch for a lease that was
// restored after a capture.
//
// Logging never includes Authorization values, lease or operator tokens,
// clipboard text, query strings, file names or CDP payloads.
package runtimeapi

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
	"github.com/JiaWeiXie/lensdock/internal/auth"
	"github.com/JiaWeiXie/lensdock/internal/capture"
	"github.com/JiaWeiXie/lensdock/internal/clipboard"
	"github.com/JiaWeiXie/lensdock/internal/lease"
	"github.com/JiaWeiXie/lensdock/internal/supervisor"
	"github.com/JiaWeiXie/lensdock/internal/transfer"
	"github.com/JiaWeiXie/lensdock/internal/vnc"
)

const (
	jsonBodyLimit      = 8 << 10
	clipboardBodyLimit = clipboard.DefaultMaxBytes
	bodyReadTimeout    = 10 * time.Second
	maxIdemKeyLen      = 256
	maxTTLSeconds      = 24 * 60 * 60 // far above any MaxTTL; only guards overflow

	// operatorHolderID is the holder recorded for leases the operator acquires
	// without an X-LensDock-Holder-ID header.
	operatorHolderID = "operator"

	// Headers of the Gateway-facing contract.
	headerHolderID   = "X-LensDock-Holder-ID"
	headerTakeoverID = "X-LensDock-Takeover-ID"
	headerEpoch      = "X-LensDock-Lease-Epoch"
	headerGeneration = "X-LensDock-Runtime-Generation"

	maxReasonRunes = 1000
)

var (
	holderIDPattern   = regexp.MustCompile(`^[A-Za-z0-9._:@-]{1,128}$`)
	takeoverIDPattern = regexp.MustCompile(`^tko_[0-9a-f]{32}$`)
)

// HealthSource reports supervisor readiness and diagnostics.
type HealthSource interface {
	Ready() bool
	Detail() supervisor.Detail
}

// Deps are the collaborators of the API handler. Leases and Health are
// required; every other collaborator is optional and its routes answer 502
// UPSTREAM_UNAVAILABLE when it is nil.
type Deps struct {
	WorkspaceID string
	Operator    auth.Digest
	Leases      *lease.Manager
	// CDP serves the AGENT-only proxy. It is mounted under
	// /api/v1/workspaces/{ws}/cdp/ with that prefix stripped.
	CDP       http.Handler
	Clipboard *clipboard.Bridge
	Health    HealthSource
	Logger    *slog.Logger

	// VNCView fronts the view-only x11vnc (operator credentials); VNCControl
	// fronts the control x11vnc (HUMAN lease credentials).
	VNCView    *vnc.Proxy
	VNCControl *vnc.Proxy
	// Resizer changes the X screen size.
	Resizer *vnc.Resizer
	// Capture takes page-viewport screenshots and lists targets; Captures
	// holds the resulting PNGs briefly (a default store is made when nil).
	Capture  PageCapturer
	Captures *capture.Store
	// Files is the controlled upload/download area; FileLimits is used to
	// refuse oversized uploads before any byte is read.
	Files      transfer.Store
	FileLimits transfer.Limits
	// AgentHeartbeat is the interval of heartbeat comments on the agent
	// policy event stream; zero selects 15 seconds.
	AgentHeartbeat time.Duration
}

type api struct {
	Deps
	cdp     http.Handler
	display atomic.Pointer[vnc.Size]
}

// browserVersioner is implemented by the supervisor.
type browserVersioner interface{ ChromiumVersion() string }

// PageCapturer lists page targets and takes viewport captures.
// *capture.Capturer satisfies it.
type PageCapturer interface {
	ListTargets(ctx context.Context) ([]capture.Target, error)
	Capture(ctx context.Context, targetID string, opts capture.Options) (capture.Result, []byte, error)
}

// resourceReader is implemented by the supervisor.
type resourceReader interface{ Resources() supervisor.Resources }

// NewHandler builds the API handler. It panics on missing required
// dependencies, which is a wiring error rather than a runtime condition.
func NewHandler(d Deps) http.Handler {
	if d.Leases == nil || d.Health == nil || d.WorkspaceID == "" {
		panic("runtimeapi: WorkspaceID, Leases and Health are required")
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	if d.Captures == nil {
		d.Captures = capture.NewStore(0, 0, nil)
	}
	d.FileLimits = d.FileLimits.WithDefaults()
	a := &api{Deps: d}
	a.display.Store(&vnc.Size{Width: supervisor.ScreenWidth, Height: supervisor.ScreenHeight})
	if d.CDP != nil {
		a.cdp = http.StripPrefix("/api/v1/workspaces/"+d.WorkspaceID+"/cdp", d.CDP)
	}

	type route struct {
		method string
		h      http.HandlerFunc
	}
	mux := http.NewServeMux()
	// mount registers the method routes of one path and a path-only pattern
	// that answers every other method with 405 and an Allow header. The
	// method patterns win over the path-only one.
	mount := func(path string, routes ...route) {
		allow := make([]string, 0, len(routes))
		for _, rt := range routes {
			mux.HandleFunc(rt.method+" "+path, rt.h)
			if rt.method == http.MethodGet {
				allow = append(allow, "GET, HEAD")
			} else {
				allow = append(allow, rt.method)
			}
		}
		mux.HandleFunc(path, methodNotAllowed(strings.Join(allow, ", ")))
	}
	const ws = "/api/v1/workspaces/{ws}"
	mount("/healthz", route{http.MethodGet, a.healthz})
	mount("/readyz", route{http.MethodGet, a.readyz})
	mount(ws, route{http.MethodGet, a.workspace})
	mount(ws+"/state", route{http.MethodGet, a.state})
	mount(ws+"/leases", route{http.MethodPost, a.acquire})
	// No path-only 405 pattern here: "/leases/current" without a method would
	// overlap "DELETE /leases/{leaseId}" with neither more specific, which
	// ServeMux rejects. Other methods fall to the {leaseId} fallback.
	mux.HandleFunc("GET "+ws+"/leases/current", a.currentLease)
	mount(ws+"/leases/{leaseId}/renew", route{http.MethodPost, a.renew})
	mount(ws+"/leases/{leaseId}", route{http.MethodDelete, a.release})
	mount(ws+"/takeover",
		route{http.MethodPost, a.beginTakeover},
		route{http.MethodGet, a.getTakeover},
		route{http.MethodDelete, a.cancelTakeover})
	mount(ws+"/maintenance",
		route{http.MethodPost, a.enterMaintenance},
		route{http.MethodDelete, a.exitMaintenance})
	mount(ws+"/clipboard", route{http.MethodPut, a.setClipboard})
	mount(ws+"/resolution",
		route{http.MethodGet, a.getResolution},
		route{http.MethodPut, a.setResolution})
	mount(ws+"/vnc/view", route{http.MethodGet, a.vncView})
	mount(ws+"/vnc/control", route{http.MethodGet, a.vncControl})
	mount(ws+"/targets", route{http.MethodGet, a.targets})
	mount(ws+"/captures/viewport", route{http.MethodPost, a.createCapture})
	mount(ws+"/captures/viewport/{captureId}/image",
		route{http.MethodGet, a.captureImage},
		route{http.MethodDelete, a.deleteCapture})
	mount(ws+"/agent",
		route{http.MethodGet, a.agentGet},
		route{http.MethodPut, a.agentSet})
	mount(ws+"/agent/pause", route{http.MethodPost, a.agentPause})
	mount(ws+"/agent/resume", route{http.MethodPost, a.agentResume})
	mount(ws+"/agent/events", route{http.MethodGet, a.agentEvents})
	mount(ws+"/files", route{http.MethodGet, a.listFiles})
	mount(ws+"/files/{area}/{name}",
		route{http.MethodGet, a.getFile},
		route{http.MethodPut, a.putFile},
		route{http.MethodDelete, a.deleteFile})

	mux.HandleFunc(ws+"/cdp/", a.cdpMount)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, apiwire.CodeNotFound, "no such route")
	})
	return middleware(d.Logger, rejectUnsafePaths(mux))
}

// rejectUnsafePaths answers 400 before ServeMux can clean or redirect a path
// that contains a dot segment or an encoded separator. ServeMux would answer
// 301/307 for "..", and a client that follows it would resend a request that
// must never be interpreted as a file name because it could traverse directories.
func rejectUnsafePaths(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		escaped := r.URL.EscapedPath()
		if !unsafePath(escaped) {
			next.ServeHTTP(w, r)
			return
		}
		code := apiwire.CodeInvalidRequest
		if strings.Contains(escaped, "/files/") {
			code = apiwire.CodeInvalidName
		}
		writeError(w, http.StatusBadRequest, code, "path contains a dot segment or an encoded separator")
	})
}

// unsafePath reports whether escaped has a "."/".." segment (also when
// percent-encoded) or an encoded path separator or NUL.
func unsafePath(escaped string) bool {
	lower := strings.ToLower(escaped)
	if strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c") || strings.Contains(lower, "%00") {
		return true
	}
	for _, seg := range strings.Split(escaped, "/") {
		if seg == "." || seg == ".." {
			return true
		}
		if strings.Contains(seg, "%") {
			if d, err := url.PathUnescape(seg); err == nil && (d == "." || d == "..") {
				return true
			}
		}
	}
	return false
}

// NewServer returns an http.Server with header limits and timeouts. It has no
// WriteTimeout because CDP and VNC WebSockets, downloads and long discovery
// calls are long-lived; body reads are bounded per handler instead.
func NewServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		MaxHeaderBytes:    16 << 10,
		IdleTimeout:       60 * time.Second,
	}
}

func (a *api) healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// readyDetail is the operator-only extension of /readyz.
type readyDetail struct {
	Ready       bool                  `json:"ready"`
	Workspace   apiwire.Workspace     `json:"workspace"`
	Locked      bool                  `json:"locked"`
	LockCode    string                `json:"lockCode,omitempty"`
	Lease       *apiwire.Lease        `json:"lease"`
	Display     vnc.Size              `json:"display"`
	Handoff     *handoffView          `json:"handoff"`
	Capture     bool                  `json:"capture"`
	Maintenance bool                  `json:"maintenance"`
	Resources   *supervisor.Resources `json:"resources"`
	Detail      supervisor.Detail     `json:"detail"`
}

func (a *api) readyz(w http.ResponseWriter, r *http.Request) {
	ready := a.Health.Ready()
	status := http.StatusOK
	if !ready {
		status = http.StatusServiceUnavailable
	}
	if r.URL.Query().Get("detail") != "1" {
		writeJSON(w, status, map[string]bool{"ready": ready})
		return
	}
	if !a.requireOperator(w, r) {
		return
	}
	st := a.Leases.Status()
	out := readyDetail{
		Ready:       ready,
		Workspace:   a.workspaceView(st, ready),
		Locked:      st.Locked,
		LockCode:    st.LockCode,
		Display:     *a.display.Load(),
		Handoff:     handoffOf(st.Handoff),
		Capture:     st.Capture,
		Maintenance: st.Maintenance,
		Detail:      a.Health.Detail(),
	}
	if rr, ok := a.Health.(resourceReader); ok {
		res := rr.Resources()
		out.Resources = &res
	}
	if st.Lease != nil {
		l := leaseView(*st.Lease)
		out.Lease = &l
	}
	writeJSON(w, status, out)
}

// checkWorkspace answers 404 unless the path names this runtime's workspace.
func (a *api) checkWorkspace(w http.ResponseWriter, r *http.Request) bool {
	if r.PathValue("ws") != a.WorkspaceID {
		writeError(w, http.StatusNotFound, apiwire.CodeNotFound, "workspace not found")
		return false
	}
	return true
}

// requireOperator authenticates the operator token, writing 401 on failure.
func (a *api) requireOperator(w http.ResponseWriter, r *http.Request) bool {
	tok, ok := auth.BearerToken(r)
	if !ok || !a.Operator.Matches(tok) {
		writeError(w, http.StatusUnauthorized, apiwire.CodeUnauthenticated, "operator credentials required")
		return false
	}
	return true
}

// operatorRoute is the common prologue of operator routes.
func (a *api) operatorRoute(w http.ResponseWriter, r *http.Request) bool {
	return a.checkWorkspace(w, r) && a.requireOperator(w, r)
}

func bearer(w http.ResponseWriter, r *http.Request) (string, bool) {
	tok, ok := auth.BearerToken(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, apiwire.CodeUnauthenticated, "bearer token required")
	}
	return tok, ok
}

func writeLeaseError(w http.ResponseWriter, err error) {
	status, code, msg := leaseErrorStatus(err)
	writeError(w, status, code, msg)
}

// checkBinding compares the optional epoch and generation headers with the
// live lease of ticket. A lease restored after a capture shares its token
// with the pre-capture grant, so for it the epoch header is mandatory: a
// client that did not learn the new epoch (GET /leases/current) is refused.
func (a *api) checkBinding(w http.ResponseWriter, r *http.Request, t lease.Ticket) bool {
	l := t.Lease()
	epochs, gens := r.Header.Values(headerEpoch), r.Header.Values(headerGeneration)
	if len(epochs) > 1 || len(gens) > 1 {
		writeError(w, http.StatusBadRequest, apiwire.CodeInvalidRequest, "duplicate binding header")
		return false
	}
	if len(epochs) == 1 {
		n, err := strconv.ParseUint(epochs[0], 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, apiwire.CodeInvalidRequest, "invalid "+headerEpoch)
			return false
		}
		if n != l.Epoch {
			writeLeaseError(w, lease.ErrEpochMismatch)
			return false
		}
	} else if t.EpochBound() {
		writeError(w, http.StatusPreconditionFailed, apiwire.CodeEpochMismatch,
			"the lease was restored after a capture; send its current epoch in "+headerEpoch)
		return false
	}
	if len(gens) == 1 && gens[0] != l.Generation {
		writeLeaseError(w, lease.ErrGenerationMismatch)
		return false
	}
	return true
}

// authorizeLeaseRoute authenticates a lease-bound request: workspace, bearer
// lease token of the wanted mode (registered as an in-flight use, so a
// takeover waits for it), and epoch/generation binding. The returned release
// must be called when the request or connection ends.
func (a *api) authorizeLeaseRoute(w http.ResponseWriter, r *http.Request, want lease.Mode) (lease.Ticket, func(), bool) {
	if !a.checkWorkspace(w, r) {
		return lease.Ticket{}, nil, false
	}
	tok, ok := bearer(w, r)
	if !ok {
		return lease.Ticket{}, nil, false
	}
	ticket, release, err := a.Leases.AuthorizeTracked(tok, want)
	if err != nil {
		writeLeaseError(w, err)
		return lease.Ticket{}, nil, false
	}
	if !a.checkBinding(w, r, ticket) {
		release()
		return lease.Ticket{}, nil, false
	}
	return ticket, release, true
}

// limitReads bounds how long a client may take to send the request body.
func limitReads(w http.ResponseWriter) {
	// Unsupported writers (for example test recorders) simply keep no deadline.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(bodyReadTimeout))
}

func leaseView(l lease.Lease) apiwire.Lease {
	return apiwire.Lease{
		LeaseID:           l.ID,
		WorkspaceID:       l.WorkspaceID,
		HolderID:          l.HolderID,
		Mode:              string(l.Mode),
		Epoch:             l.Epoch,
		RuntimeGeneration: l.Generation,
		ExpiresAt:         l.ExpiresAt,
	}
}

func (a *api) workspaceView(st lease.Status, ready bool) apiwire.Workspace {
	return apiwire.Workspace{
		ID:                st.WorkspaceID,
		RuntimeGeneration: st.Generation,
		ControlState:      string(st.State),
		HolderID:          st.HolderID,
		Ready:             ready,
		AgentControl:      st.AgentControl,
	}
}

func (a *api) workspace(w http.ResponseWriter, r *http.Request) {
	if !a.operatorRoute(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, a.workspaceView(a.Leases.Status(), a.Health.Ready()))
}

func validIdempotencyKey(k string) bool {
	if k == "" || len(k) > maxIdemKeyLen {
		return false
	}
	for i := range len(k) {
		if k[i] < 0x21 || k[i] > 0x7e {
			return false
		}
	}
	return true
}

// holderID returns the validated X-LensDock-Holder-ID (operator by default).
func holderID(w http.ResponseWriter, r *http.Request) (string, bool) {
	vals := r.Header.Values(headerHolderID)
	switch len(vals) {
	case 0:
		return operatorHolderID, true
	case 1:
		if holderIDPattern.MatchString(vals[0]) {
			return vals[0], true
		}
	}
	writeError(w, http.StatusBadRequest, apiwire.CodeInvalidRequest, "invalid "+headerHolderID)
	return "", false
}

// validFeedbackID accepts an empty ID (no claim) or 1-128 printable ASCII
// characters. The runtime only validates it; the Gateway owns delivery state.
func validFeedbackID(k string) bool {
	if k == "" {
		return true
	}
	if len(k) > 128 {
		return false
	}
	for i := range len(k) {
		if k[i] < 0x21 || k[i] > 0x7e {
			return false
		}
	}
	return true
}

func (a *api) acquire(w http.ResponseWriter, r *http.Request) {
	if !a.operatorRoute(w, r) {
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if !validIdempotencyKey(key) {
		writeError(w, http.StatusBadRequest, apiwire.CodeInvalidRequest, "Idempotency-Key header is required (1-256 printable ASCII characters)")
		return
	}
	holder, ok := holderID(w, r)
	if !ok {
		return
	}
	takeoverID := ""
	if vals := r.Header.Values(headerTakeoverID); len(vals) > 0 {
		if len(vals) != 1 || !takeoverIDPattern.MatchString(vals[0]) {
			writeError(w, http.StatusBadRequest, apiwire.CodeInvalidRequest, "invalid "+headerTakeoverID)
			return
		}
		takeoverID = vals[0]
	}
	limitReads(w)
	var req apiwire.AcquireLease
	if err := apiwire.Decode(r.Body, jsonBodyLimit, &req); err != nil {
		status, code, msg := bodyErrorStatus(err)
		writeError(w, status, code, msg)
		return
	}
	mode := lease.Mode(req.Mode)
	if mode != lease.ModeAgent && mode != lease.ModeHuman {
		writeError(w, http.StatusBadRequest, apiwire.CodeInvalidRequest, "mode must be AGENT or HUMAN")
		return
	}
	if takeoverID != "" && mode != lease.ModeHuman {
		writeError(w, http.StatusBadRequest, apiwire.CodeInvalidRequest, "a takeover grants a HUMAN lease")
		return
	}
	if !validFeedbackID(req.FeedbackID) {
		writeError(w, http.StatusBadRequest, apiwire.CodeInvalidRequest, "feedbackId must be 1-128 printable ASCII characters")
		return
	}
	var ttl time.Duration
	if req.TTLSeconds != nil {
		if *req.TTLSeconds <= 0 || *req.TTLSeconds > maxTTLSeconds {
			writeError(w, http.StatusBadRequest, apiwire.CodeInvalidRequest, "ttlSeconds is out of range")
			return
		}
		ttl = time.Duration(*req.TTLSeconds) * time.Second
	}
	var (
		l     lease.Lease
		token auth.Secret
		err   error
	)
	if takeoverID != "" {
		l, token, err = a.Leases.AcquireTakeover(holder, ttl, key, takeoverID)
	} else {
		l, token, err = a.Leases.Acquire(holder, mode, ttl, key)
	}
	if err != nil {
		writeLeaseError(w, err)
		return
	}
	a.Logger.Info("lease acquired",
		slog.String("requestId", w.Header().Get(requestIDHeader)),
		slog.String("leaseId", l.ID),
		slog.String("mode", string(l.Mode)),
		slog.Uint64("epoch", l.Epoch),
		slog.Bool("takeover", takeoverID != ""),
	)
	writeJSON(w, http.StatusCreated, apiwire.LeaseGrant{Lease: leaseView(l), CapabilityToken: token.Reveal()})
}

func (a *api) renew(w http.ResponseWriter, r *http.Request) {
	if !a.checkWorkspace(w, r) {
		return
	}
	tok, ok := bearer(w, r)
	if !ok {
		return
	}
	limitReads(w)
	var req apiwire.RenewLease
	if err := apiwire.Decode(r.Body, jsonBodyLimit, &req); err != nil {
		status, code, msg := bodyErrorStatus(err)
		writeError(w, status, code, msg)
		return
	}
	l, err := a.Leases.Renew(tok, r.PathValue("leaseId"), req.Epoch, req.RuntimeGeneration)
	if err != nil {
		writeLeaseError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, leaseView(l))
}

// currentLease returns the live lease of the presented token. It carries no
// binding headers: it is how a holder learns the epoch it must then present,
// for example after a capture restored its lease.
func (a *api) currentLease(w http.ResponseWriter, r *http.Request) {
	if !a.checkWorkspace(w, r) {
		return
	}
	tok, ok := bearer(w, r)
	if !ok {
		return
	}
	l, err := a.Leases.Identify(tok)
	if err != nil {
		writeLeaseError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, leaseView(l))
}

func (a *api) release(w http.ResponseWriter, r *http.Request) {
	if !a.checkWorkspace(w, r) {
		return
	}
	tok, ok := bearer(w, r)
	if !ok {
		return
	}
	leaseID := r.PathValue("leaseId")
	var err error
	if a.Operator.Matches(tok) {
		err = a.Leases.Release(leaseID)
	} else {
		// Not the operator: the caller must hold exactly this lease.
		err = a.Leases.Relinquish(tok, leaseID)
	}
	if err != nil {
		writeLeaseError(w, err)
		return
	}
	a.Logger.Info("lease released",
		slog.String("requestId", w.Header().Get(requestIDHeader)),
		slog.String("leaseId", leaseID),
	)
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) cdpMount(w http.ResponseWriter, r *http.Request) {
	if !a.checkWorkspace(w, r) {
		return
	}
	if a.cdp == nil {
		writeError(w, http.StatusBadGateway, apiwire.CodeUpstreamUnavailable, "CDP proxy unavailable")
		return
	}
	// A valid AGENT token is registered as an in-flight use for the whole
	// request or WebSocket, so a takeover waits for it, and its epoch binding
	// is checked here. Anything else falls through to the proxy, which owns
	// the error responses for bad credentials.
	if tok, ok := auth.BearerToken(r); ok {
		if ticket, release, err := a.Leases.AuthorizeTracked(tok, lease.ModeAgent); err == nil {
			defer release()
			if !a.checkBinding(w, r, ticket) {
				return
			}
		}
	}
	a.cdp.ServeHTTP(w, r)
}

// isPlainUTF8 reports whether the Content-Type is text/plain with an explicit
// charset=utf-8 and no other parameters.
func isPlainUTF8(contentType string) bool {
	mt, params, err := mime.ParseMediaType(contentType)
	if err != nil || mt != "text/plain" || len(params) != 1 {
		return false
	}
	return strings.EqualFold(params["charset"], "utf-8")
}

func (a *api) setClipboard(w http.ResponseWriter, r *http.Request) {
	ticket, release, ok := a.authorizeLeaseRoute(w, r, lease.ModeHuman)
	if !ok {
		return
	}
	defer release()
	if !isPlainUTF8(r.Header.Get("Content-Type")) {
		writeError(w, http.StatusBadRequest, apiwire.CodeInvalidRequest, "Content-Type must be text/plain; charset=utf-8")
		return
	}
	if r.ContentLength > clipboardBodyLimit {
		writeError(w, http.StatusRequestEntityTooLarge, apiwire.CodeBodyTooLarge, "clipboard text too large")
		return
	}
	if a.Clipboard == nil {
		writeError(w, http.StatusBadGateway, apiwire.CodeUpstreamUnavailable, "clipboard unavailable")
		return
	}
	limitReads(w)
	text, err := io.ReadAll(io.LimitReader(r.Body, clipboardBodyLimit+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, apiwire.CodeInvalidRequest, "could not read request body")
		return
	}
	if len(text) > clipboardBodyLimit {
		writeError(w, http.StatusRequestEntityTooLarge, apiwire.CodeBodyTooLarge, "clipboard text too large")
		return
	}
	setCtx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := a.Clipboard.Set(setCtx, text, ticket); err != nil {
		status, code, msg := clipboardErrorStatus(err)
		if status >= http.StatusInternalServerError {
			a.Logger.Error("clipboard set failed",
				slog.String("requestId", w.Header().Get(requestIDHeader)),
				slog.String("errorType", errType(err)),
			)
		}
		writeError(w, status, code, msg)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// errType names an error's dynamic type without exposing its message.
func errType(err error) string { return fmt.Sprintf("%T", err) }
