package feedback

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/storage"
)

// NewEventsHandler serves GET /api/v1/events (Server-Sent Events, workspace
// scoped, resumable with Last-Event-ID or ?after=) and GET /api/v1/audit
// (administrators, JSON). The Gateway authenticates and passes the Principal
// through WithPrincipal. SSE is only a notification: clients refetch the
// state they care about; an event is not proof that an agent read anything.
func NewEventsHandler(s *Service) http.Handler {
	h := &handler{s: s}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/events", h.wrap(h.streamEvents))
	mux.HandleFunc("GET /api/v1/audit", h.wrap(h.listAudit))
	for pattern, allow := range map[string]string{"/api/v1/events": "GET", "/api/v1/audit": "GET"} {
		allow := allow
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			setSecurityHeaders(w)
			w.Header().Set("Allow", allow)
			writeErr(w, newError(http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed"))
		})
	}
	return mux
}

// publicEvent hides the free-text detail from everyone but administrators.
func publicEvent(e storage.Event, admin bool) storage.Event {
	if !admin {
		e.Detail = ""
	}
	return e
}

func (h *handler) streamEvents(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	p, err := requirePrincipal(ctx)
	if err != nil {
		return err
	}
	ws := r.URL.Query().Get("workspaceId")
	if ws == "" {
		return badRequest("workspaceId is required")
	}
	if !p.InWorkspace(ws) {
		return ErrForbidden
	}
	var after int64
	hasAfter := false
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return badRequest("invalid Last-Event-ID")
		}
		after, hasAfter = n, true
	} else if v := r.URL.Query().Get("after"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return badRequest("invalid after")
		}
		after, hasAfter = n, true
	}
	if !hasAfter {
		// A new subscriber sees what happens from now on.
		after, err = h.s.st.Events.MaxSeq(ctx)
		if err != nil {
			return h.s.internal("events.stream", err)
		}
	}
	rc := http.NewResponseController(w)
	hd := w.Header()
	hd.Set("Content-Type", "text/event-stream")
	hd.Set("Cache-Control", "no-store")
	hd.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if _, err := fmt.Fprint(w, ": connected\n\n"); err != nil {
		return nil
	}
	_ = rc.Flush()
	for {
		evs, err := h.s.ListEvents(ctx, ws, after, 100)
		if err != nil {
			return nil // headers are sent; the client reconnects
		}
		for _, e := range evs {
			b, merr := marshalCompact(publicEvent(e, p.Role == RoleAdmin))
			if merr != nil {
				return nil
			}
			if _, err := fmt.Fprintf(w, "id: %d\ndata: %s\n", e.Seq, bytesTrim(b)); err != nil {
				return nil
			}
			if _, err := fmt.Fprint(w, "\n"); err != nil {
				return nil
			}
			after = e.Seq
		}
		if len(evs) > 0 {
			_ = rc.Flush()
			continue
		}
		wctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		werr := h.s.WaitEvents(wctx, after)
		cancel()
		if ctx.Err() != nil {
			return nil
		}
		if werr != nil && !errors.Is(werr, context.DeadlineExceeded) {
			return nil
		}
		if werr != nil {
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return nil
			}
			_ = rc.Flush()
		}
	}
}

func bytesTrim(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

func (h *handler) listAudit(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	var after int64
	if v := q.Get("after"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return badRequest("invalid after")
		}
		after = n
	}
	limit := 100
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			return badRequest("limit must be 1-500")
		}
		limit = n
	}
	evs, err := h.s.ListAudit(ctx, q.Get("workspaceId"), after, limit)
	if err != nil {
		return err
	}
	next := after
	if len(evs) > 0 {
		next = evs[len(evs)-1].Seq
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": evs, "nextSeq": next})
	return nil
}
