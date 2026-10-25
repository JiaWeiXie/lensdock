package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
)

// errDraining is the cancellation cause of a stream the Gateway ended because
// it is shutting down.
var errDraining = errors.New("gateway: shutting down")

// streamGroup tracks the requests that never finish on their own: event
// subscriptions (SSE) and upgraded VNC/CDP WebSockets.
//
// http.Server.Shutdown cannot end them. It waits for every active request, so
// one live subscriber keeps it running until its deadline, and it ignores
// hijacked connections altogether, so an upgraded socket is neither closed nor
// waited for. The Gateway therefore cancels the streams itself (drain) and then
// waits for their handlers to return (wait) before the caller closes storage.
//
// The zero value is ready to use.
type streamGroup struct {
	mu       sync.Mutex
	draining bool
	next     uint64
	active   map[uint64]context.CancelCauseFunc
	idle     chan struct{} // closed once draining and no stream is active
}

// begin admits one stream. The returned context ends with parent or when drain
// is called; release must run when the handler returns. ok is false once
// draining has started, so no stream starts while the old ones unwind.
func (s *streamGroup) begin(parent context.Context) (ctx context.Context, release func(), ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.draining {
		return nil, nil, false
	}
	ctx, cancel := context.WithCancelCause(parent)
	if s.active == nil {
		s.active = map[uint64]context.CancelCauseFunc{}
	}
	s.next++
	id := s.next
	s.active[id] = cancel
	return ctx, func() {
		cancel(nil)
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.active, id)
		s.settleLocked()
	}, true
}

// admit binds r to the group for one stream. While the Gateway drains it
// answers 503 itself and reports ok=false; the caller must then return.
func (s *streamGroup) admit(w http.ResponseWriter, r *http.Request) (bound *http.Request, release func(), ok bool) {
	ctx, release, ok := s.begin(r.Context())
	if !ok {
		fail(w, http.StatusServiceUnavailable, "UNAVAILABLE", "gateway is shutting down")
		return r, nil, false
	}
	return r.WithContext(ctx), release, true
}

// drain refuses new streams and cancels every active one. It returns a channel
// that is closed once the last stream handler has returned. It is idempotent.
func (s *streamGroup) drain() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.draining {
		s.draining = true
		s.idle = make(chan struct{})
		for _, cancel := range s.active {
			cancel(errDraining)
		}
		s.settleLocked()
	}
	return s.idle
}

// wait drains and then blocks until every stream handler has returned or ctx
// ends. A stream that outlives ctx is reported, never ignored.
func (s *streamGroup) wait(ctx context.Context) error {
	idle := s.drain()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
	}
	select {
	case <-idle:
		return nil
	default:
		return fmt.Errorf("gateway: %d stream(s) still open: %w", s.open(), ctx.Err())
	}
}

func (s *streamGroup) open() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.active)
}

// settleLocked closes idle the moment a draining group has no active stream.
// The receive doubles as a close-once guard; s.mu is held.
func (s *streamGroup) settleLocked() {
	if !s.draining || len(s.active) > 0 {
		return
	}
	select {
	case <-s.idle:
	default:
		close(s.idle)
	}
}

// Shutdown stops the Gateway's HTTP side without orphaning work that still
// uses its storage:
//
//  1. New streams are refused (503) and every live event subscription and
//     upgraded VNC/CDP WebSocket is cancelled. An SSE handler ends its
//     response, so the client reads a clean EOF. A proxied WebSocket closes
//     both halves; the runtime sees its connection drop and drains the lease
//     ticket as it does for any client disconnect. The Gateway never forges a
//     4001/4002 fence close, which only the runtime may issue.
//  2. srv.Shutdown closes the listeners and waits for ordinary in-flight
//     requests until ctx ends. Their deadline error is returned, not swallowed.
//  3. Wait for every stream handler to return, since srv.Shutdown cannot see
//     hijacked sockets.
//
// Storage (OpenFeedback's closer) and Close must only run after Shutdown has
// returned, so no handler ever touches a closed database.
func (g *Gateway) Shutdown(ctx context.Context, srv *http.Server) error {
	g.streams.drain()
	serverErr := srv.Shutdown(ctx)
	return errors.Join(serverErr, g.streams.wait(ctx))
}
