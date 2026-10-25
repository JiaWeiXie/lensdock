//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JiaWeiXie/lensdock/tests/e2e/internal/cdpc"
	"github.com/JiaWeiXie/lensdock/tests/e2e/internal/harness"
)

// Environment of the SPIKE-02 helpers driven by tests/e2e/vnc-cpu.sh.
const (
	envPage      = "LENSDOCK_E2E_PAGE"         // static | animation
	envLoadPorts = "LENSDOCK_E2E_LOAD_PORTS"   // comma separated viewer ports, one RFB client each
	envLoadSecs  = "LENSDOCK_E2E_LOAD_SECONDS" // how long the viewers stay connected
	envLoadHz    = "LENSDOCK_E2E_LOAD_HZ"      // incremental update requests per second, default 30
	envLabel     = "LENSDOCK_E2E_LABEL"        // scenario label used in evidence names
)

// TestSpike02Page puts Chromium into a known display state for the VNC CPU
// scenarios: a static page or the full-viewport animation fixture.
func TestSpike02Page(t *testing.T) {
	if os.Getenv(envPage) == "" {
		t.Skipf("measurement driver: %s is set by tests/e2e/vnc-cpu.sh", envPage)
	}
	s := newSidecar(t)
	var target string
	switch p := harness.Require(t, envPage); p {
	case "static":
		target = "data:text/html;charset=utf-8," + url.PathEscape(`<!doctype html><title>static</title><body style="background:#fff;margin:0"><h1>LensDock static page</h1>`)
	case "animation":
		target = s.fixture + "/vnc-load.html"
	default:
		t.Fatalf("%s must be static or animation, got %q", envPage, p)
	}
	withAgent(t, s.api, func(ctx context.Context, c *cdpc.Client, _ *cdpc.Discovery) {
		if err := c.Navigate(ctx, target); err != nil {
			t.Fatalf("navigate: %v", err)
		}
		if err := c.BringToFront(ctx); err != nil {
			t.Fatalf("bring to front: %v", err)
		}
	})
	settle()
}

type loadStats struct {
	Port       int     `json:"port"`
	Seconds    float64 `json:"seconds"`
	Updates    int     `json:"framebufferUpdates"`
	Rectangles int     `json:"rectangles"`
	Pixels     int64   `json:"pixels"`
	Bytes      int64   `json:"payloadBytes"`
	Requests   int     `json:"updateRequests"`
	FirstFull  bool    `json:"receivedInitialFullFrame"`
}

// TestSpike02Load connects one RFB viewer per listed port, requests incremental
// updates at LOAD_HZ for LOAD_SECONDS and discards the pixels. tests/e2e/vnc-cpu.sh
// samples the runtime's per-process CPU from /readyz?detail=1 while it runs.
func TestSpike02Load(t *testing.T) {
	if os.Getenv(envLoadPorts) == "" {
		t.Skipf("measurement driver: %s is set by tests/e2e/vnc-cpu.sh", envLoadPorts)
	}
	s := newSidecar(t)
	var ports []int
	for _, f := range strings.Split(harness.Require(t, envLoadPorts), ",") {
		p, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil || p < 1 || p > 65535 {
			t.Fatalf("%s: bad port %q", envLoadPorts, f)
		}
		ports = append(ports, p)
	}
	secs, err := strconv.Atoi(harness.Require(t, envLoadSecs))
	if err != nil || secs < 1 {
		t.Fatalf("%s must be a positive integer", envLoadSecs)
	}
	hz := 30
	if v := os.Getenv(envLoadHz); v != "" {
		if hz, err = strconv.Atoi(v); err != nil || hz < 1 {
			t.Fatalf("%s must be a positive integer", envLoadHz)
		}
	}
	label := harness.Default(envLabel, "load")

	stats := make([]loadStats, len(ports))
	var wg sync.WaitGroup
	errs := make(chan error, len(ports))
	for i, port := range ports {
		c := s.dialRFB(t, port)
		c.DiscardPixels = true
		wg.Add(1)
		go func() {
			defer wg.Done()
			st := loadStats{Port: port}
			start := time.Now()
			end := start.Add(time.Duration(secs) * time.Second)
			_ = c.SetDeadline(end)
			var rwg sync.WaitGroup
			rwg.Add(1)
			go func() { // request writer
				defer rwg.Done()
				tick := time.NewTicker(time.Second / time.Duration(hz))
				defer tick.Stop()
				first := true
				for time.Now().Before(end) {
					if err := c.RequestUpdate(!first); err != nil {
						return
					}
					st.Requests++
					first = false
					<-tick.C
				}
			}()
			for {
				m, err := c.ReadMessage()
				if err != nil {
					break
				}
				if m.Type == 0 {
					st.Updates++
					st.Rectangles += m.Rects
					st.Pixels += int64(m.Pixels)
					st.Bytes += int64(m.Bytes)
					if m.Pixels >= c.Width*c.Height {
						st.FirstFull = true
					}
				}
			}
			_ = c.Close()
			rwg.Wait()
			st.Seconds = time.Since(start).Seconds()
			stats[i] = st
			if !st.FirstFull {
				errs <- fmt.Errorf("port %d: never received a full initial frame", port)
			}
		}()
	}
	wg.Wait()
	close(errs)
	s.ev.SaveJSON(t, "TestSpike02Load/"+label+".json", stats)
	for err := range errs {
		t.Error(err)
	}
}

// TestRT03Viewers (RT-03, VNC half): connecting and abruptly dropping viewers on
// both VNC ports, including a drop in the middle of an update, leaves Chromium, its
// page state and the runtime generation untouched.
func TestRT03Viewers(t *testing.T) {
	s := newSidecar(t)
	const title = "LensDock RT-03 viewers page"
	const typed = "still-here-繁體"
	page := `<!doctype html><meta charset="utf-8"><title>` + title + `</title><input id="u" autocomplete="off">`
	before := mustWorkspace(t, s.api)

	withAgent(t, s.api, func(ctx context.Context, c *cdpc.Client, _ *cdpc.Discovery) {
		if err := c.Navigate(ctx, "data:text/html;charset=utf-8,"+url.PathEscape(page)); err != nil {
			t.Fatalf("navigate: %v", err)
		}
		if err := c.Evaluate(ctx, `document.getElementById("u").value = `+jsString(typed)+`; true`, nil); err != nil {
			t.Fatalf("type: %v", err)
		}
	})

	for _, port := range []int{vncControlPort, vncViewPort} {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		v := s.dialRFB(t, port)
		if _, _, err := v.FetchFramebuffer(ctx); err != nil {
			t.Errorf("port %d: framebuffer: %v", port, err)
		}
		cancel()
		_ = v.Close() // abrupt close of an idle viewer

		w := s.dialRFB(t, port)
		_ = w.RequestUpdate(false)
		_ = w.Close() // close with a full update still in flight
	}
	settle()

	after := mustWorkspace(t, s.api)
	if after.RuntimeGeneration != before.RuntimeGeneration || !after.Ready {
		t.Errorf("generation %s -> %s, ready=%v after viewers disconnected", before.RuntimeGeneration, after.RuntimeGeneration, after.Ready)
	}
	withAgent(t, s.api, func(ctx context.Context, c *cdpc.Client, _ *cdpc.Discovery) {
		var gotTitle, gotTyped string
		if err := c.Evaluate(ctx, `document.title`, &gotTitle); err != nil {
			t.Fatalf("read title: %v", err)
		}
		if err := c.Evaluate(ctx, `document.getElementById("u").value`, &gotTyped); err != nil {
			t.Fatalf("read value: %v", err)
		}
		if gotTitle != title || gotTyped != typed {
			t.Errorf("page state changed: title %q, value %q", gotTitle, gotTyped)
		}
	})
}
