// Package vnc describes the two x11vnc instances of the runtime container and
// probes them for readiness.
package vnc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
)

// Instance is one x11vnc server.
type Instance struct {
	Name     string
	Port     int
	ViewOnly bool
}

var (
	// Control accepts input; View is view-only and ignores selections.
	Control = Instance{Name: "vnc-control", Port: 5900, ViewOnly: false}
	View    = Instance{Name: "vnc-view", Port: 5901, ViewOnly: true}
)

// Args returns the x11vnc argv (after the binary name) for i. The server
// listens on localhost only, has no password (access is gated by the loopback
// bind and the container boundary), and never takes a secret on its command
// line.
func (i Instance) Args(display, xauthority string) []string {
	args := []string{
		"-display", display,
		"-auth", xauthority,
		"-rfbport", strconv.Itoa(i.Port),
		"-localhost",
		"-noipv6",
		"-forever",
		"-shared",
		"-nopw",
		"-norc",
		"-xkb",
		// Follow RandR screen size changes (PUT /resolution) instead of
		// keeping the framebuffer size seen at startup.
		"-xrandr",
		"-quiet",
	}
	if i.ViewOnly {
		args = append(args, "-viewonly", "-nosel")
	}
	return args
}

// ErrBadBanner is returned by Probe when the peer does not send an RFB
// protocol version banner.
var ErrBadBanner = errors.New("vnc: peer did not send an RFB version banner")

const bannerLen = len("RFB 003.008\n")

// Probe dials addr, reads the 12-byte RFB ProtocolVersion message and returns
// the version, e.g. "003.008". The deadline and cancellation come from ctx.
func Probe(ctx context.Context, addr string) (string, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", fmt.Errorf("vnc: dial: %w", err)
	}
	defer conn.Close()

	// Cancellation and deadline both come from ctx: AfterFunc closes the
	// connection only after ctx is done, so a failed read always sees
	// ctx.Err(). A separate conn deadline would race with it.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	var buf [bannerLen]byte
	if _, err := io.ReadFull(conn, buf[:]); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("vnc: read banner: %w", ctxErr)
		}
		return "", fmt.Errorf("vnc: read banner: %w", err)
	}
	version, ok := parseBanner(buf[:])
	if !ok {
		return "", ErrBadBanner
	}
	return version, nil
}

// parseBanner validates "RFB xxx.yyy\n" with decimal digits and returns
// "xxx.yyy".
func parseBanner(b []byte) (string, bool) {
	if len(b) != bannerLen || string(b[:4]) != "RFB " || b[7] != '.' || b[11] != '\n' {
		return "", false
	}
	for _, i := range [...]int{4, 5, 6, 8, 9, 10} {
		if b[i] < '0' || b[i] > '9' {
			return "", false
		}
	}
	return string(b[4:11]), true
}
