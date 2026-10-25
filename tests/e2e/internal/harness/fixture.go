package harness

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"
)

// FixtureAddr is where the sidecar serves tests/fixtures. It is a loopback
// address inside the network namespace shared with the runtime container, so
// the runtime's Chromium can load fixtures without any mount or published port.
const FixtureAddr = "127.0.0.1:18081"

// ServeFixtures serves dir on addr until the test ends and returns the base URL.
//
// TEST-ONLY PATTERN: sidecar tests run with `--network container:lensdock-runtime`
// so this listener lives on the runtime container's loopback. That is how the
// e2e suite reaches the loopback-only VNC and CDP ports without changing the
// product image. Nothing like it exists or may be added to the product.
func ServeFixtures(t TB, dir, addr string) string {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("fixture server cannot listen on %s: %v", addr, err)
	}
	fs := http.FileServer(http.Dir(dir))
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			fs.ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Logf("fixture server stopped: %v", err)
		}
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return "http://" + ln.Addr().String()
}
