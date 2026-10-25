//go:build e2e

package e2e

import (
	"encoding/json"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/JiaWeiXie/lensdock/tests/e2e/internal/cdpc"
	"github.com/JiaWeiXie/lensdock/tests/e2e/internal/harness"
)

// TestRT02 (SPIKE-01, RT-02): the non-root Chromium is sandboxed and the runtime
// never fell back to --no-sandbox. Evidence is the readyz sandbox report, the
// container process table and the text of chrome://sandbox read over CDP.
func TestRT02(t *testing.T) {
	api := harness.NewAPI(t)
	ev := harness.NewEvidence(t)
	dk := harness.NewDocker(t)

	// 1. The supervisor's own report.
	resp, err := api.Readyz(timeout(t, 15*time.Second), true)
	if err != nil {
		t.Fatalf("GET /readyz?detail=1: %v", err)
	}
	ev.Save(t, "TestRT02/readyz-detail.json", resp.Body)
	if resp.Status != 200 {
		t.Fatalf("/readyz?detail=1 = HTTP %d, want 200 (runtime must be ready for RT-02)", resp.Status)
	}
	var detail harness.ReadyDetail
	if err := json.Unmarshal(resp.Body, &detail); err != nil {
		t.Fatalf("decode readyz detail: %v", err)
	}
	if !detail.Ready {
		t.Errorf("readyz detail ready = false (locked=%v lockCode=%q)", detail.Locked, detail.LockCode)
	}
	if !detail.Detail.Sandbox.Verified || len(detail.Detail.Sandbox.Processes) == 0 {
		t.Errorf("supervisor sandbox report: verified=%v with %d processes, want verified with at least one", detail.Detail.Sandbox.Verified, len(detail.Detail.Sandbox.Processes))
	}

	// 2. Process facts from inside the container.
	procs, raw, err := dk.ProcSnapshot(timeout(t, 30*time.Second))
	if err != nil {
		t.Fatalf("process snapshot: %v", err)
	}
	ev.Save(t, "TestRT02/procs.txt", []byte(raw))
	main, ok := harness.ChromiumMain(procs)
	if !ok {
		t.Fatal("no Chromium browser process in the container")
	}
	if main.RealUID() != 10001 {
		t.Errorf("Chromium runs as uid %d, want 10001", main.RealUID())
	}
	renderers := 0
	for _, p := range procs {
		if strings.Contains(p.Cmdline, "--no-sandbox") {
			t.Errorf("pid %d runs with --no-sandbox", p.PID)
		}
		if p.ChromiumType() == "renderer" {
			renderers++
			if p.Fields["Seccomp"] != "2" {
				t.Errorf("renderer pid %d Seccomp = %q, want 2 (filter)", p.PID, p.Fields["Seccomp"])
			}
			if p.NSpidDepth() < 2 {
				t.Errorf("renderer pid %d NSpid = %q: not in its own PID namespace", p.PID, p.Fields["NSpid"])
			}
		}
	}
	if renderers == 0 {
		t.Error("no renderer process found; cannot check the renderer sandbox")
	}

	// 3. What Chromium itself says about its sandbox.
	g := acquire(t, api, modeAgent, 120)
	disc := api.CDP(g.Token)
	ctx := timeout(t, 60*time.Second)
	if ver, err := disc.Version(ctx); err == nil {
		ev.SaveJSON(t, "TestRT02/json-version.json", ver)
	} else {
		t.Errorf("GET /json/version: %v", err)
	}
	tg, err := disc.NewTarget(ctx, "chrome://sandbox")
	if err != nil {
		t.Fatalf("open chrome://sandbox: %v", err)
	}
	defer func() { _ = disc.CloseTarget(timeout(t, 10*time.Second), tg.ID) }()
	c, err := disc.Dial(ctx, tg.WebSocketDebuggerURL)
	if err != nil {
		t.Fatalf("dial chrome://sandbox target: %v", err)
	}
	defer c.Close()

	var text string
	deadline := time.Now().Add(20 * time.Second)
	for {
		// The WebUI fills the page after load, so poll until the verdict line exists.
		if err := c.Evaluate(ctx, `document.body ? document.body.innerText : ""`, &text); err != nil {
			t.Fatalf("read chrome://sandbox text: %v", err)
		}
		if strings.Contains(strings.ToLower(text), "sandboxed") || time.Now().After(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	ev.Save(t, "TestRT02/chrome-sandbox.txt", []byte(text))
	lower := strings.ToLower(text)
	if !strings.Contains(lower, "adequately sandboxed") || strings.Contains(lower, "not adequately sandboxed") {
		t.Errorf("chrome://sandbox does not report an adequate sandbox:\n%s", text)
	}
	// chrome://sandbox renders "Layer 1 Sandbox<TAB>Namespace" for the user-namespace sandbox.
	if !regexp.MustCompile(`layer 1 sandbox\s+namespace`).MatchString(lower) {
		t.Errorf("chrome://sandbox does not show the namespace sandbox as layer 1:\n%s", text)
	}
	c.Close()
	release(t, api, g)
}

// TestRT03 (SPIKE-01, RT-03): closing the CLI bridge, the CDP client and the
// lease must leave Chromium and its page state untouched: same browser process,
// same runtime generation, same unsaved form input. The VNC half of RT-03 is
// TestRT03Viewers in the sidecar.
func TestRT03(t *testing.T) {
	api := harness.NewAPI(t)
	ev := harness.NewEvidence(t)
	dk := harness.NewDocker(t)
	cli := harness.NewCLI(t, api)

	const title = "LensDock RT-03 login page"
	const typed = "persist-me-繁體-😀"
	page := `<!doctype html><meta charset="utf-8"><title>` + title + `</title>` +
		`<form><input id="u" autocomplete="off"><input id="p" type="password" autocomplete="off"></form>`
	pageURL := "data:text/html;charset=utf-8," + url.PathEscape(page)

	before := mustWorkspace(t, api)
	procs, _, err := dk.ProcSnapshot(timeout(t, 30*time.Second))
	if err != nil {
		t.Fatalf("process snapshot: %v", err)
	}
	mainBefore, ok := harness.ChromiumMain(procs)
	if !ok {
		t.Fatal("no Chromium browser process before the test")
	}

	// Drive the page through the real CLI bridge.
	leaseFile := filepath.Join(t.TempDir(), "agent-lease.json")
	const leaseTTL = 60 * time.Second
	if err := cli.AcquireToFile(timeout(t, 30*time.Second), modeAgent, int(leaseTTL/time.Second), leaseFile); err != nil {
		t.Fatalf("lensdock lease acquire: %v", err)
	}
	t.Cleanup(func() { _, _ = cli.ReleaseFile(timeout(t, 15*time.Second), leaseFile) })
	bridge, err := cli.StartBridge(timeout(t, 5*time.Minute), leaseFile, 20*time.Second, false)
	if err != nil {
		t.Fatalf("lensdock cdp bridge: %v", err)
	}
	t.Cleanup(bridge.Kill)

	disc := &cdpc.Discovery{Base: strings.TrimSuffix(bridge.Endpoints.DiscoveryURL, "/")}
	ctx := timeout(t, 60*time.Second)
	tg, err := firstPage(ctx, disc)
	if err != nil {
		t.Fatalf("find page through the bridge: %v", err)
	}
	c, err := disc.Dial(ctx, tg.WebSocketDebuggerURL)
	if err != nil {
		t.Fatalf("dial page through the bridge: %v", err)
	}
	if err := c.Navigate(ctx, pageURL); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	if err := c.Evaluate(ctx, `document.getElementById("u").value = `+jsString(typed)+`; true`, nil); err != nil {
		t.Fatalf("type into the login form: %v", err)
	}

	// Close everything on the agent side: CDP client, lease (via the CLI), bridge process.
	c.Close()
	res, err := cli.ReleaseFile(timeout(t, 15*time.Second), leaseFile)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("lensdock lease release: exit %d, %v", res.ExitCode, err)
	}
	// The bridge notices the ended lease on its next renewal, one renew interval at most.
	exitBy := bridgeRenewInterval(leaseTTL) + 10*time.Second
	code, err := bridge.WaitExit(exitBy)
	if err != nil {
		bridge.Kill()
		t.Errorf("bridge did not exit within %v after the lease was released: %v\nstderr: %s", exitBy, err, bridge.Stderr())
	}
	time.Sleep(3 * time.Second)

	// A fresh AGENT lease must find the page exactly as it was left.
	g := acquire(t, api, modeAgent, 60)
	c2, _ := pageClient(t, api, g.Token)
	var gotTitle, gotTyped string
	if err := c2.Evaluate(ctx, `document.title`, &gotTitle); err != nil {
		t.Fatalf("read title: %v", err)
	}
	if err := c2.Evaluate(ctx, `document.getElementById("u").value`, &gotTyped); err != nil {
		t.Fatalf("read form value: %v", err)
	}
	c2.Close()
	release(t, api, g)

	after := mustWorkspace(t, api)
	procs2, raw, err := dk.ProcSnapshot(timeout(t, 30*time.Second))
	if err != nil {
		t.Fatalf("process snapshot after: %v", err)
	}
	mainAfter, ok := harness.ChromiumMain(procs2)
	if !ok {
		t.Fatal("no Chromium browser process after closing the agent side")
	}
	ev.Save(t, "TestRT03/procs-after.txt", []byte(raw))
	ev.SaveJSON(t, "TestRT03/result.json", map[string]any{
		"chromiumPidBefore": mainBefore.PID, "chromiumPidAfter": mainAfter.PID,
		"generationBefore": before.RuntimeGeneration, "generationAfter": after.RuntimeGeneration,
		"titleAfter": gotTitle, "formValueMatches": gotTyped == typed, "bridgeExitCode": code,
	})

	if mainAfter.PID != mainBefore.PID {
		t.Errorf("Chromium pid changed %d -> %d", mainBefore.PID, mainAfter.PID)
	}
	if after.RuntimeGeneration != before.RuntimeGeneration {
		t.Errorf("runtime generation changed %s -> %s", before.RuntimeGeneration, after.RuntimeGeneration)
	}
	if !after.Ready {
		t.Error("workspace is not ready after closing the agent side")
	}
	if gotTitle != title {
		t.Errorf("page title = %q, want %q (page was reloaded or replaced)", gotTitle, title)
	}
	if gotTyped != typed {
		t.Errorf("form value = %q, want %q (page state was lost)", gotTyped, typed)
	}
}

// jsString renders s as a JavaScript string literal.
func jsString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err) // a Go string always marshals
	}
	return string(b)
}
