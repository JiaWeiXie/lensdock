package apiclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
)

func TestAgentControlStreamDeliversStateAndCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/workspaces/default/agent/events" || r.Header.Get("Authorization") != "Bearer test-secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, ": connected\n\nevent: unrelated\ndata: not-json\n\nevent: agent-control\ndata: {\"mode\":\"AGENT\",\"paused\":true,\n")
		_, _ = io.WriteString(w, "data: \"revision\":2,\"updatedAt\":\"2026-10-25T00:00:00Z\"}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	var got apiwire.AgentControl
	err := c.WatchAgentControl(ctx, "default", "test-secret", func(state apiwire.AgentControl) error {
		got = state
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) || got.Mode != "AGENT" || !got.Paused || got.Revision != 2 {
		t.Fatalf("state=%+v error=%v", got, err)
	}
}

func TestAgentControlStreamFailsClosedOnInvalidOrLostState(t *testing.T) {
	for name, frame := range map[string]string{
		"unknown mode":      `{"mode":"AUTO","paused":false,"revision":1,"updatedAt":"2026-10-25T00:00:00Z"}`,
		"missing revision":  `{"mode":"AGENT","paused":false,"updatedAt":"2026-10-25T00:00:00Z"}`,
		"invalid off state": `{"mode":"NOVNC","paused":true,"revision":1,"updatedAt":"2026-10-25T00:00:00Z"}`,
		"missing timestamp": `{"mode":"AGENT","paused":false,"revision":1}`,
		"oversized frame":   strings.Repeat("x", maxErrorBody+1),
	} {
		t.Run(name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "event: agent-control\ndata: "+frame+"\n\n")
			})
			called := false
			err := c.WatchAgentControl(context.Background(), "default", "test-secret", func(apiwire.AgentControl) error { called = true; return nil })
			if err == nil || called || strings.Contains(err.Error(), "test-secret") {
				t.Fatalf("invalid state delivered=%v error=%v", called, err)
			}
		})
	}
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, ": disconnected\n\n")
	})
	if err := c.WatchAgentControl(context.Background(), "default", "test-secret", func(apiwire.AgentControl) error { return nil }); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("stream loss must not imply success: %v", err)
	}
}
