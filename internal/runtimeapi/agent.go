package runtimeapi

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
)

// defaultAgentHeartbeat is how often the policy event stream sends a comment
// line so proxies and clients can tell a live stream from a dead one.
const defaultAgentHeartbeat = 15 * time.Second

// agentGet returns the authoritative usage policy.
func (a *api) agentGet(w http.ResponseWriter, r *http.Request) {
	if !a.operatorRoute(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, a.Leases.AgentControl())
}

// agentSet switches the usage mode (PUT body exactly {"mode": ...}).
func (a *api) agentSet(w http.ResponseWriter, r *http.Request) {
	if !a.operatorRoute(w, r) {
		return
	}
	limitReads(w)
	var req apiwire.SetAgentMode
	if err := apiwire.Decode(r.Body, jsonBodyLimit, &req); err != nil {
		status, code, msg := bodyErrorStatus(err)
		writeError(w, status, code, msg)
		return
	}
	if !apiwire.ValidAgentMode(req.Mode) {
		writeError(w, http.StatusBadRequest, apiwire.CodeInvalidRequest, "mode must be NOVNC or AGENT")
		return
	}
	ctl, err := a.Leases.SetAgentMode(req.Mode)
	a.finishAgentChange(w, "agent mode set", ctl, err)
}

func (a *api) agentPause(w http.ResponseWriter, r *http.Request) {
	if !a.operatorRoute(w, r) || !a.emptyBody(w, r) {
		return
	}
	ctl, err := a.Leases.PauseAgent()
	a.finishAgentChange(w, "agent paused", ctl, err)
}

func (a *api) agentResume(w http.ResponseWriter, r *http.Request) {
	if !a.operatorRoute(w, r) || !a.emptyBody(w, r) {
		return
	}
	ctl, err := a.Leases.ResumeAgent()
	a.finishAgentChange(w, "agent resumed", ctl, err)
}

// emptyBody accepts no body or exactly "{}".
func (a *api) emptyBody(w http.ResponseWriter, r *http.Request) bool {
	limitReads(w)
	body, err := io.ReadAll(io.LimitReader(r.Body, jsonBodyLimit+1))
	if err != nil || len(body) > jsonBodyLimit {
		writeError(w, http.StatusBadRequest, apiwire.CodeInvalidRequest, "invalid request body")
		return false
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return true
	}
	var empty struct{}
	if err := apiwire.Decode(bytes.NewReader(body), jsonBodyLimit, &empty); err != nil {
		status, code, msg := bodyErrorStatus(err)
		writeError(w, status, code, msg)
		return false
	}
	return true
}

func (a *api) finishAgentChange(w http.ResponseWriter, msg string, ctl apiwire.AgentControl, err error) {
	if err != nil {
		a.Logger.Warn(msg+" failed",
			slog.String("requestId", w.Header().Get(requestIDHeader)),
			slog.String("errorType", errType(err)),
		)
		writeLeaseError(w, err)
		return
	}
	a.Logger.Info(msg,
		slog.String("requestId", w.Header().Get(requestIDHeader)),
		slog.String("mode", ctl.Mode),
		slog.Bool("paused", ctl.Paused),
		slog.Uint64("revision", ctl.Revision),
	)
	writeJSON(w, http.StatusOK, ctl)
}

// agentEvents streams the usage policy as server-sent events: the current
// state first, then every change (latest wins when changes outpace the
// reader), with periodic heartbeat comments. The stream stays available in
// NOVNC mode and while paused, so an Agent can learn of a resume or enable.
// It carries only the policy: no feedback, lease or page data.
func (a *api) agentEvents(w http.ResponseWriter, r *http.Request) {
	if !a.operatorRoute(w, r) {
		return
	}
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Accel-Buffering", "no")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	watch, err := a.Leases.WatchAgentControl()
	if err != nil {
		h.Del("Content-Type")
		writeLeaseError(w, err)
		return
	}
	defer watch.Close()

	interval := a.AgentHeartbeat
	if interval <= 0 {
		interval = defaultAgentHeartbeat
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	w.WriteHeader(http.StatusOK)
	if rc.Flush() != nil {
		return
	}
	var sent uint64
	for {
		select {
		case <-r.Context().Done():
			return
		case <-watch.Closed():
			return
		case <-ticker.C:
			if _, err := io.WriteString(w, ": heartbeat\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		case <-watch.C:
			ctl := a.Leases.AgentControl()
			if ctl.Revision == sent {
				continue
			}
			data, err := json.Marshal(ctl)
			if err != nil {
				return
			}
			var buf bytes.Buffer
			buf.WriteString("id: " + strconv.FormatUint(ctl.Revision, 10) + "\nevent: agent-control\ndata: ")
			buf.Write(data)
			buf.WriteString("\n\n")
			if _, err := w.Write(buf.Bytes()); err != nil || rc.Flush() != nil {
				return
			}
			sent = ctl.Revision
		}
	}
}
