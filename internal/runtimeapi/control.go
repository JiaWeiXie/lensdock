package runtimeapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
	"github.com/JiaWeiXie/lensdock/internal/capture"
	"github.com/JiaWeiXie/lensdock/internal/lease"
	"github.com/JiaWeiXie/lensdock/internal/supervisor"
	"github.com/JiaWeiXie/lensdock/internal/vnc"
)

// handoffView is the wire form of a takeover.
type handoffView struct {
	RequestID        string            `json:"requestId"`
	Status           string            `json:"status"`
	Drained          bool              `json:"drained"`
	Forced           bool              `json:"forced"`
	UncertainEffects bool              `json:"uncertainEffects"`
	PriorLease       *capture.LeaseRef `json:"priorLease"`
	ExpiresAt        time.Time         `json:"expiresAt"`
}

func handoffOf(h *lease.Handoff) *handoffView {
	if h == nil {
		return nil
	}
	v := &handoffView{
		RequestID:        h.RequestID,
		Status:           string(h.Status),
		Drained:          h.Drained,
		Forced:           h.Forced,
		UncertainEffects: h.UncertainEffects,
		ExpiresAt:        h.ExpiresAt,
	}
	if h.Prior != nil {
		v.PriorLease = &capture.LeaseRef{LeaseID: h.Prior.LeaseID, HolderID: h.Prior.HolderID, Mode: string(h.Prior.Mode)}
	}
	return v
}

// stateView is GET /state: everything a UI needs about the workspace in one
// read. It never carries a capability token.
type stateView struct {
	Workspace      apiwire.Workspace `json:"workspace"`
	Lease          *apiwire.Lease    `json:"lease"`
	Epoch          *uint64           `json:"epoch"`
	HolderMode     *string           `json:"holderMode"`
	LeaseExpiresAt *time.Time        `json:"leaseExpiresAt"`
	Locked         bool              `json:"locked"`
	LockCode       string            `json:"lockCode"`
	BrowserVersion string            `json:"browserVersion"`
	Display        vnc.Size          `json:"display"`
	Handoff        *handoffView      `json:"handoff"`
	Maintenance    bool              `json:"maintenance"`
	Capture        bool              `json:"capture"`
	// Resources is a bounded sample of container CPU, memory and volume
	// space; unmeasurable values are null.
	Resources *supervisor.Resources `json:"resources"`
}

func (a *api) state(w http.ResponseWriter, r *http.Request) {
	if !a.operatorRoute(w, r) {
		return
	}
	st := a.Leases.Status()
	out := stateView{
		Workspace:   a.workspaceView(st, a.Health.Ready()),
		Locked:      st.Locked,
		LockCode:    st.LockCode,
		Display:     *a.display.Load(),
		Handoff:     handoffOf(st.Handoff),
		Maintenance: st.Maintenance,
		Capture:     st.Capture,
	}
	if bv, ok := a.Health.(browserVersioner); ok {
		out.BrowserVersion = bv.ChromiumVersion()
	}
	if rr, ok := a.Health.(resourceReader); ok {
		res := rr.Resources()
		out.Resources = &res
	}
	if st.Lease != nil {
		l := leaseView(*st.Lease)
		mode := string(st.Lease.Mode)
		out.Lease = &l
		out.Epoch = &l.Epoch
		out.HolderMode = &mode
		out.LeaseExpiresAt = &l.ExpiresAt
	}
	writeJSON(w, http.StatusOK, out)
}

// reasonRequest is the body of takeover and maintenance requests.
type reasonRequest struct {
	Reason string `json:"reason"`
	Force  bool   `json:"force"`
}

func decodeReason(w http.ResponseWriter, r *http.Request) (reasonRequest, bool) {
	limitReads(w)
	var req reasonRequest
	if err := apiwire.Decode(r.Body, jsonBodyLimit, &req); err != nil {
		status, code, msg := bodyErrorStatus(err)
		writeError(w, status, code, msg)
		return req, false
	}
	if n := utf8.RuneCountInString(req.Reason); n < 1 || n > maxReasonRunes {
		writeError(w, http.StatusBadRequest, apiwire.CodeInvalidRequest, "reason must be 1 to 1000 characters")
		return req, false
	}
	return req, true
}

// maintenanceRequest rejects a "force" field that only takeover understands.
type maintenanceRequest struct {
	Reason string `json:"reason"`
}

func (a *api) beginTakeover(w http.ResponseWriter, r *http.Request) {
	if !a.operatorRoute(w, r) {
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if !validIdempotencyKey(key) {
		writeError(w, http.StatusBadRequest, apiwire.CodeInvalidRequest, "Idempotency-Key header is required (1-256 printable ASCII characters)")
		return
	}
	req, ok := decodeReason(w, r)
	if !ok {
		return
	}
	h, err := a.Leases.BeginHandoff(key, req.Force)
	if err != nil {
		writeLeaseError(w, err)
		return
	}
	attrs := []any{
		slog.String("requestId", w.Header().Get(requestIDHeader)),
		slog.String("takeoverId", h.RequestID),
		slog.String("status", string(h.Status)),
		slog.Bool("forced", h.Forced),
	}
	if h.Prior != nil {
		attrs = append(attrs, slog.String("priorLeaseId", h.Prior.LeaseID), slog.String("priorMode", string(h.Prior.Mode)))
	}
	a.Logger.Info("takeover", attrs...)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": string(lease.StateHandoffPending), "requestId": h.RequestID})
}

func (a *api) getTakeover(w http.ResponseWriter, r *http.Request) {
	if !a.operatorRoute(w, r) {
		return
	}
	h, ok := a.Leases.Handoff()
	if !ok {
		writeError(w, http.StatusNotFound, apiwire.CodeNotFound, "no takeover in progress")
		return
	}
	writeJSON(w, http.StatusOK, handoffOf(&h))
}

func (a *api) cancelTakeover(w http.ResponseWriter, r *http.Request) {
	if !a.operatorRoute(w, r) {
		return
	}
	if err := a.Leases.CancelHandoff(); err != nil {
		writeError(w, http.StatusNotFound, apiwire.CodeNotFound, "no takeover in progress")
		return
	}
	a.Logger.Info("takeover cancelled", slog.String("requestId", w.Header().Get(requestIDHeader)))
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) enterMaintenance(w http.ResponseWriter, r *http.Request) {
	if !a.operatorRoute(w, r) {
		return
	}
	limitReads(w)
	var req maintenanceRequest
	if err := apiwire.Decode(r.Body, jsonBodyLimit, &req); err != nil {
		status, code, msg := bodyErrorStatus(err)
		writeError(w, status, code, msg)
		return
	}
	if n := utf8.RuneCountInString(req.Reason); n < 1 || n > maxReasonRunes {
		writeError(w, http.StatusBadRequest, apiwire.CodeInvalidRequest, "reason must be 1 to 1000 characters")
		return
	}
	drain, err := a.Leases.EnterMaintenance()
	if err != nil {
		writeLeaseError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), a.Leases.DrainTimeout())
	drained := drain.Wait(ctx)
	cancel()
	a.Logger.Info("maintenance entered",
		slog.String("requestId", w.Header().Get(requestIDHeader)),
		slog.Bool("drained", drained),
	)
	writeJSON(w, http.StatusOK, map[string]any{
		"state":             string(lease.StateMaintenance),
		"drained":           drained,
		"runtimeGeneration": a.Leases.Status().Generation,
	})
}

func (a *api) exitMaintenance(w http.ResponseWriter, r *http.Request) {
	if !a.operatorRoute(w, r) {
		return
	}
	if err := a.Leases.ExitMaintenance(); err != nil {
		status, code, _ := leaseErrorStatus(err)
		writeError(w, status, code, "workspace is not in maintenance")
		return
	}
	a.Logger.Info("maintenance exited", slog.String("requestId", w.Header().Get(requestIDHeader)))
	w.WriteHeader(http.StatusNoContent)
}
