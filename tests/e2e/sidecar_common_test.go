//go:build e2e

// Sidecar tests need the runtime's loopback-only VNC (5900/5901) and CDP ports.
//
// TEST-ONLY PATTERN: they run in a throwaway container started with
// `--network container:lensdock-runtime`, so 127.0.0.1 inside the test is the
// runtime container's loopback. Nothing in the product image or compose file is
// changed or published for this, and none of it may be reproduced in the product.

package e2e

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/rfb"
	"github.com/JiaWeiXie/lensdock/tests/e2e/internal/harness"
)

const (
	vncControlPort = 5900
	vncViewPort    = 5901
)

// sidecar bundles what every sidecar test needs.
type sidecar struct {
	api     *harness.API
	ev      *harness.Evidence
	fixture string // base URL of the fixture server inside the shared network namespace
	vncHost string
}

func newSidecar(t *testing.T) *sidecar {
	t.Helper()
	return &sidecar{
		api:     harness.NewAPI(t),
		ev:      harness.NewEvidence(t),
		fixture: harness.ServeFixtures(t, harness.Require(t, harness.EnvFixtures), harness.FixtureAddr),
		vncHost: harness.Default(harness.EnvVNCHost, "127.0.0.1"),
	}
}

func (s *sidecar) vncAddr(port int) string {
	return net.JoinHostPort(s.vncHost, strconv.Itoa(port))
}

// dialRFB connects an RFB client to one of the runtime's VNC ports.
func (s *sidecar) dialRFB(t *testing.T, port int) *rfb.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, err := rfb.Dial(ctx, s.vncAddr(port))
	if err != nil {
		t.Fatalf("RFB connect to port %d: %v", port, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// drain reads and discards server messages for d so the server's writes never block
// and any ServerCutText sent in reply to our input is collected.
func drain(c *rfb.Client, d time.Duration) (cuts [][]byte) {
	_ = c.SetDeadline(time.Now().Add(d))
	_ = c.RequestUpdate(true)
	for {
		m, err := c.ReadMessage()
		if err != nil {
			_ = c.SetDeadline(time.Time{})
			return cuts
		}
		if m.CutText != nil {
			cuts = append(cuts, m.CutText)
		}
	}
}

// settle gives Chromium and x11vnc time to process injected input.
func settle() { time.Sleep(1500 * time.Millisecond) }

// hex renders bytes for evidence.
func hexOf(b []byte) string { return fmt.Sprintf("%x", b) }
