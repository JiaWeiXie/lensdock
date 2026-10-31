package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/JiaWeiXie/lensdock/internal/apiclient"
)

func TestDesktopFailureWaitsForTheReadersLeaseReason(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	done := make(chan struct{})
	result := make(chan error, 1)
	go func() { result <- desktopFailure(ctx, done, fmt.Errorf("rfb: write: %w", net.ErrClosed)) }()
	// The writer failed before the reader published the close frame. Returning
	// that write error now would lose the positive evidence of lease loss.
	select {
	case err := <-result:
		t.Fatalf("returned before the reader classified the close: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	cancel(websocket.CloseError{Code: 4001, Reason: "lease ended"})
	close(done)
	select {
	case err := <-result:
		if !apiclient.DesktopLeaseEnded(err) {
			t.Fatalf("lease reason lost: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reader completion did not release the error path")
	}
}

func TestDesktopFailureDoesNotTurnTransportClosureIntoLeaseLoss(t *testing.T) {
	done := make(chan struct{})
	close(done)
	err := fmt.Errorf("rfb: write: %w", net.ErrClosed)
	got := desktopFailure(context.Background(), done, err)
	if !errors.Is(got, net.ErrClosed) || apiclient.DesktopLeaseEnded(got) {
		t.Fatalf("plain transport closure misclassified: %v", got)
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(context.Canceled)
	if got := desktopFailure(ctx, done, err); !errors.Is(got, net.ErrClosed) {
		t.Fatalf("caller cancellation replaced the transport error: %v", got)
	}
}

func TestDesktopFailureWaitIsBounded(t *testing.T) {
	result := make(chan error, 1)
	go func() {
		result <- desktopFailure(context.Background(), make(chan struct{}), net.ErrClosed)
	}()
	select {
	case err := <-result:
		if !errors.Is(err, net.ErrClosed) || apiclient.DesktopLeaseEnded(err) {
			t.Fatalf("unclassified close was changed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stalled reader made error classification unbounded")
	}
}
