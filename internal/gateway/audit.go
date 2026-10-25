package gateway

import (
	"bufio"
	"context"
	"log/slog"
	"net"
	"net/http"
)

func (g *Gateway) record(ctx context.Context, p Identity, action string) {
	if g.integrations.Audit == nil {
		return
	}
	if err := g.integrations.Audit(ctx, p, action, p.WorkspaceID); err != nil {
		// Never include headers, request bodies, URLs, credentials or runtime errors.
		slog.Error("audit persistence failed", "action", action, "workspaceId", p.WorkspaceID)
	}
}

// Preserve flushing and hijacking for SSE and WebSocket routes while recording
// only the status code. Request/response contents never enter the audit trail.
type auditResponse struct {
	http.ResponseWriter
	status int
}

func (w *auditResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *auditResponse) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *auditResponse) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}
func (w *auditResponse) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}
func (w *auditResponse) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(w.ResponseWriter).Hijack()
}
