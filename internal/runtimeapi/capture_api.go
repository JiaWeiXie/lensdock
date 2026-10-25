package runtimeapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
	"github.com/JiaWeiXie/lensdock/internal/capture"
	"github.com/JiaWeiXie/lensdock/internal/lease"
)

// captureSafetyMargin is how long before the manager's own CAPTURE timeout
// the handler stops working, so the handler (which restores the holder with a
// full outcome) wins the race with the safety timer.
const captureSafetyMargin = 5 * time.Second

type captureRequest struct {
	TargetID        string `json:"targetId"`
	AllowInterrupt  bool   `json:"allowInterrupt"`
	ExpectedLeaseID string `json:"expectedLeaseId"`
	Activate        bool   `json:"activate"`
}

func (a *api) targets(w http.ResponseWriter, r *http.Request) {
	if !a.operatorRoute(w, r) {
		return
	}
	if a.Capture == nil {
		writeError(w, http.StatusBadGateway, apiwire.CodeUpstreamUnavailable, "browser unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	ts, err := a.Capture.ListTargets(ctx)
	if err != nil {
		a.Logger.Warn("list targets failed",
			slog.String("requestId", w.Header().Get(requestIDHeader)),
			slog.String("errorType", errType(err)),
		)
		writeError(w, http.StatusBadGateway, apiwire.CodeUpstreamUnavailable, "browser unavailable")
		return
	}
	if ts == nil {
		ts = []capture.Target{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"targets": ts})
}

func fingerprint(req captureRequest) string {
	return req.TargetID + "|" + strconv.FormatBool(req.AllowInterrupt) + "|" + req.ExpectedLeaseID + "|" + strconv.FormatBool(req.Activate)
}

// createCapture takes a page-viewport capture. The workspace enters CAPTURE:
// a held lease is fenced (its CDP and VNC connections close, and the handler
// waits for them to drain), the screenshot is taken through the runtime's own
// CDP channel, and the prior holder is restored at a new epoch if its lease is
// still valid. The holder's capability token never appears in the response.
func (a *api) createCapture(w http.ResponseWriter, r *http.Request) {
	if !a.operatorRoute(w, r) {
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if !validIdempotencyKey(key) {
		writeError(w, http.StatusBadRequest, apiwire.CodeInvalidRequest, "Idempotency-Key header is required (1-256 printable ASCII characters)")
		return
	}
	limitReads(w)
	var req captureRequest
	if err := apiwire.Decode(r.Body, jsonBodyLimit, &req); err != nil {
		status, code, msg := bodyErrorStatus(err)
		writeError(w, status, code, msg)
		return
	}
	if req.TargetID == "" {
		writeError(w, http.StatusBadRequest, apiwire.CodeInvalidRequest, "targetId is required")
		return
	}
	if len(req.TargetID) > 256 || len(req.ExpectedLeaseID) > 128 {
		writeError(w, http.StatusBadRequest, apiwire.CodeInvalidRequest, "targetId or expectedLeaseId is too long")
		return
	}
	if a.Capture == nil {
		writeError(w, http.StatusBadGateway, apiwire.CodeUpstreamUnavailable, "browser unavailable")
		return
	}
	fp := fingerprint(req)
	if res, found, mismatch := a.Captures.Replay(key, fp); found {
		if mismatch {
			writeError(w, http.StatusConflict, apiwire.CodeIdempotencyReplay, "idempotency key was used with a different capture request")
			return
		}
		writeJSON(w, http.StatusCreated, res)
		return
	}

	sess, err := a.Leases.BeginCapture(req.ExpectedLeaseID, req.AllowInterrupt)
	if err != nil {
		writeLeaseError(w, err)
		return
	}
	var (
		once    sync.Once
		outcome lease.CaptureOutcome
	)
	end := func() lease.CaptureOutcome {
		once.Do(func() { outcome = sess.End() })
		return outcome
	}
	defer end() // never leave the workspace in CAPTURE, even on a panic

	budget := a.Leases.CaptureTimeout() - captureSafetyMargin
	if budget < time.Second {
		budget = a.Leases.CaptureTimeout() * 4 / 5
	}
	ctx, cancel := context.WithTimeout(r.Context(), budget)
	defer cancel()

	drainCtx, cancelDrain := context.WithTimeout(ctx, a.Leases.DrainTimeout())
	drained := sess.WaitDrained(drainCtx)
	cancelDrain()
	if !drained {
		end()
		writeError(w, http.StatusConflict, apiwire.CodeLeaseConflict, "the previous holder's connections did not drain; capture refused")
		return
	}

	res, png, err := a.Capture.Capture(ctx, req.TargetID, capture.Options{Activate: req.Activate})
	out := end()
	if err != nil {
		if !errors.Is(err, capture.ErrTargetNotFound) && !errors.Is(err, capture.ErrInvalidTarget) {
			a.Logger.Warn("capture failed",
				slog.String("requestId", w.Header().Get(requestIDHeader)),
				slog.String("errorType", errType(err)),
			)
		}
		status, code, msg := captureErrorStatus(err)
		writeError(w, status, code, msg)
		return
	}

	res.Control = capture.Control{FinalState: string(out.State), RestoredLease: json.RawMessage("null")}
	if out.Prior != nil {
		res.Control.PriorLease = &capture.LeaseRef{LeaseID: out.Prior.LeaseID, HolderID: out.Prior.HolderID, Mode: string(out.Prior.Mode)}
	}
	if out.Restored != nil {
		raw, merr := json.Marshal(leaseView(*out.Restored))
		if merr == nil {
			res.Control.RestoredLease = raw
		}
	}
	a.Captures.Put(key, fp, res, png)
	attrs := []any{
		slog.String("requestId", w.Header().Get(requestIDHeader)),
		slog.String("captureId", res.CaptureID),
		slog.String("consistency", res.Consistency),
		slog.Int("attempts", res.Attempts),
		slog.Bool("interrupted", out.Prior != nil),
		slog.Bool("restored", out.Restored != nil),
	}
	if out.Restored != nil {
		attrs = append(attrs, slog.Uint64("restoredEpoch", out.Restored.Epoch))
	}
	a.Logger.Info("viewport capture", attrs...)
	writeJSON(w, http.StatusCreated, res)
}

func (a *api) captureImage(w http.ResponseWriter, r *http.Request) {
	if !a.operatorRoute(w, r) {
		return
	}
	png, ok := a.Captures.Image(r.PathValue("captureId"))
	if !ok {
		writeError(w, http.StatusNotFound, apiwire.CodeNotFound, "capture not found or expired")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "image/png")
	h.Set("Content-Length", strconv.Itoa(len(png)))
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(png)
}

func (a *api) deleteCapture(w http.ResponseWriter, r *http.Request) {
	if !a.operatorRoute(w, r) {
		return
	}
	if !a.Captures.Delete(r.PathValue("captureId")) {
		writeError(w, http.StatusNotFound, apiwire.CodeNotFound, "capture not found or expired")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
