package vnc

import (
	"context"
	"errors"
	"net"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeRFB listens on loopback and runs handle for each accepted connection.
func fakeRFB(t *testing.T, handle func(net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				handle(c)
			}()
		}
	}()
	return ln.Addr().String()
}

func TestProbe(t *testing.T) {
	tests := []struct {
		name    string
		banner  string
		want    string
		wantErr error
	}{
		{"rfb 3.8", "RFB 003.008\n", "003.008", nil},
		{"rfb 3.3", "RFB 003.003\n", "003.003", nil},
		{"garbage", "HTTP/1.1 400\r\n", "", ErrBadBanner},
		{"wrong separator", "RFB 003-008\n", "", ErrBadBanner},
		{"non-digit version", "RFB 00x.008\n", "", ErrBadBanner},
		{"missing newline", "RFB 003.008 ", "", ErrBadBanner},
		{"lowercase magic", "rfb 003.008\n", "", ErrBadBanner},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addr := fakeRFB(t, func(c net.Conn) { _, _ = c.Write([]byte(tt.banner)) })
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			got, err := Probe(ctx, addr)
			if !errors.Is(err, tt.wantErr) || got != tt.want {
				t.Fatalf("Probe = %q, %v; want %q, %v", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

func TestProbeShortBannerThenCloseFails(t *testing.T) {
	addr := fakeRFB(t, func(c net.Conn) { _, _ = c.Write([]byte("RFB 003")) })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if v, err := Probe(ctx, addr); err == nil {
		t.Fatalf("Probe = %q, nil; want error", v)
	}
}

func TestProbeTimesOutOnSilentPeer(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	addr := fakeRFB(t, func(c net.Conn) { <-release })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Probe(ctx, addr)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Probe took %v despite a 100ms deadline", elapsed)
	}
}

func TestProbeHonorsCancellationWithoutDeadline(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	addr := fakeRFB(t, func(c net.Conn) { <-release })

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	if _, err := Probe(ctx, addr); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want Canceled", err)
	}
}

func TestProbeConnectionRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := Probe(ctx, addr); err == nil {
		t.Fatal("Probe succeeded against a closed port")
	}
}

func TestArgsNeverCarryPasswordsAndOnlyViewIsReadOnly(t *testing.T) {
	control := Control.Args(":99", "/run/lensdock/browser/Xauthority")
	view := View.Args(":99", "/run/lensdock/browser/Xauthority")

	for name, args := range map[string][]string{"control": control, "view": view} {
		if !slices.Contains(args, "-localhost") {
			t.Errorf("%s: must listen on loopback only: %v", name, args)
		}
		for _, a := range args {
			if strings.HasPrefix(a, "-passwd") || a == "-rfbauth" {
				t.Errorf("%s: password material on argv: %v", name, args)
			}
		}
	}
	if slices.Contains(control, "-viewonly") || slices.Contains(control, "-nosel") {
		t.Errorf("control instance must accept input: %v", control)
	}
	if !slices.Contains(view, "-viewonly") || !slices.Contains(view, "-nosel") {
		t.Errorf("view instance must be view-only without selections: %v", view)
	}
}
