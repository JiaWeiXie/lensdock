//go:build e2e

package e2e

import (
	"context"
	"errors"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
	"github.com/JiaWeiXie/lensdock/tests/e2e/internal/cdpc"
	"github.com/JiaWeiXie/lensdock/tests/e2e/internal/harness"
)

// statusRow is one line of the CT-10 status table.
type statusRow struct {
	Name    string
	Method  string
	Path    string
	Token   string // never recorded in the evidence
	Headers map[string]string
	Body    []byte
	Status  int
	Code    string // expected apiwire error code, "" for success rows
}

type statusResult struct {
	Name       string `json:"name"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	WantStatus int    `json:"wantStatus"`
	WantCode   string `json:"wantCode,omitempty"`
	GotStatus  int    `json:"gotStatus"`
	GotCode    string `json:"gotCode,omitempty"`
	OK         bool   `json:"ok"`
}

// TestCT10 (CT-10, CT-07 subset): without an AGENT lease generic CDP is refused
// completely; the status table below is the contract of the runtime API for
// credentials of the wrong kind, ended leases, foreign origins and unknown routes.
func TestCT10(t *testing.T) {
	api := harness.NewAPI(t)
	ev := harness.NewEvidence(t)
	var table []statusResult

	check := func(t *testing.T, rows []statusRow) {
		t.Helper()
		for _, r := range rows {
			resp, err := api.Do(timeout(t, 20*time.Second), r.Method, r.Path, r.Token, r.Headers, r.Body)
			if err != nil {
				t.Errorf("%s: %v", r.Name, err)
				continue
			}
			body, _ := resp.ErrorBody()
			res := statusResult{Name: r.Name, Method: r.Method, Path: strings.SplitN(r.Path, "?", 2)[0],
				WantStatus: r.Status, WantCode: r.Code, GotStatus: resp.Status, GotCode: body.Code}
			res.OK = resp.Status == r.Status && (r.Code == "" || body.Code == r.Code)
			table = append(table, res)
			if !res.OK {
				t.Errorf("%s: got HTTP %d code %q, want HTTP %d code %q", r.Name, resp.Status, body.Code, r.Status, r.Code)
			}
		}
	}
	t.Cleanup(func() { ev.SaveJSON(t, "TestCT10/status-table.json", table) })

	cdpVersion := cdpPath(api, "/json/version")
	wsPath := cdpPath(api, "/devtools/browser/00000000-0000-4000-8000-000000000000")
	acquireBody := []byte(`{"mode":"AGENT"}`)
	idem := map[string]string{"Idempotency-Key": "e2e-ct10-fixed-key", "Content-Type": "application/json"}

	t.Run("no_lease_held", func(t *testing.T) {
		check(t, []statusRow{
			{Name: "healthz is open", Method: "GET", Path: "/healthz", Status: 200},
			{Name: "readyz is open", Method: "GET", Path: "/readyz", Status: 200},
			{Name: "readyz detail needs the operator", Method: "GET", Path: "/readyz?detail=1", Status: 401, Code: apiwire.CodeUnauthenticated},
			{Name: "readyz detail rejects a made-up token", Method: "GET", Path: "/readyz?detail=1", Token: "ldl_notatoken", Status: 401, Code: apiwire.CodeUnauthenticated},
			{Name: "workspace needs the operator", Method: "GET", Path: api.WorkspacePath(""), Status: 401, Code: apiwire.CodeUnauthenticated},
			{Name: "unknown workspace", Method: "GET", Path: "/api/v1/workspaces/does-not-exist", Token: api.Operator, Status: 404, Code: apiwire.CodeNotFound},
			{Name: "cdp without credentials", Method: "GET", Path: cdpVersion, Status: 401, Code: apiwire.CodeUnauthenticated},
			{Name: "cdp with the operator token", Method: "GET", Path: cdpVersion, Token: api.Operator, Status: 401, Code: apiwire.CodeUnauthenticated},
			{Name: "cdp with a made-up lease token", Method: "GET", Path: cdpVersion, Token: "ldl_notatoken", Status: 401, Code: apiwire.CodeUnauthenticated},
			{Name: "cdp with Basic credentials", Method: "GET", Path: cdpVersion, Headers: map[string]string{"Authorization": "Basic dXNlcjpwYXNz"}, Status: 401, Code: apiwire.CodeUnauthenticated},
			{Name: "acquire without Idempotency-Key", Method: "POST", Path: api.WorkspacePath("/leases"), Token: api.Operator, Headers: map[string]string{"Content-Type": "application/json"}, Body: acquireBody, Status: 400, Code: apiwire.CodeInvalidRequest},
			{Name: "acquire with an unknown mode", Method: "POST", Path: api.WorkspacePath("/leases"), Token: api.Operator, Headers: idem, Body: []byte(`{"mode":"ROOT"}`), Status: 400, Code: apiwire.CodeInvalidRequest},
			{Name: "acquire with a lease token instead of the operator token", Method: "POST", Path: api.WorkspacePath("/leases"), Token: "ldl_notatoken", Headers: idem, Body: acquireBody, Status: 401, Code: apiwire.CodeUnauthenticated},
			{Name: "clipboard without credentials", Method: "PUT", Path: api.WorkspacePath("/clipboard"), Headers: map[string]string{"Content-Type": "text/plain; charset=utf-8"}, Body: []byte("x"), Status: 401, Code: apiwire.CodeUnauthenticated},
			{Name: "clipboard with the operator token", Method: "PUT", Path: api.WorkspacePath("/clipboard"), Token: api.Operator, Headers: map[string]string{"Content-Type": "text/plain; charset=utf-8"}, Body: []byte("x"), Status: 401, Code: apiwire.CodeUnauthenticated},
		})
		for name, token := range map[string]string{"no credentials": "", "operator token": api.Operator} {
			_, err := api.CDP(token).Dial(timeout(t, 15*time.Second), "ws://x"+wsPath)
			wantHTTPError(t, "CDP WebSocket upgrade with "+name, err, 401)
		}
	})

	t.Run("human_lease_held", func(t *testing.T) {
		gh := acquire(t, api, modeHuman, 60)
		ctx := timeout(t, 30*time.Second)
		textPlain := map[string]string{"Content-Type": "text/plain; charset=utf-8"}
		check(t, []statusRow{
			{Name: "HUMAN token on /json/version", Method: "GET", Path: cdpVersion, Token: gh.Token, Status: 403, Code: apiwire.CodeWrongLeaseMode},
			{Name: "HUMAN token on /json/list", Method: "GET", Path: cdpPath(api, "/json/list"), Token: gh.Token, Status: 403, Code: apiwire.CodeWrongLeaseMode},
			{Name: "HUMAN token on /json/new", Method: "PUT", Path: cdpPath(api, "/json/new?about:blank"), Token: gh.Token, Status: 403, Code: apiwire.CodeWrongLeaseMode},
			{Name: "second lease while HUMAN holds", Method: "POST", Path: api.WorkspacePath("/leases"), Token: api.Operator, Headers: idem, Body: acquireBody, Status: 409, Code: apiwire.CodeLeaseConflict},
			{Name: "clipboard with the HUMAN token", Method: "PUT", Path: api.WorkspacePath("/clipboard"), Token: gh.Token, Headers: textPlain, Body: []byte("ct10"), Status: 204},
			{Name: "clipboard with invalid UTF-8", Method: "PUT", Path: api.WorkspacePath("/clipboard"), Token: gh.Token, Headers: textPlain, Body: []byte{0xff, 0xfe, 'a'}, Status: 400, Code: apiwire.CodeInvalidUTF8},
			{Name: "clipboard over 64 KiB", Method: "PUT", Path: api.WorkspacePath("/clipboard"), Token: gh.Token, Headers: textPlain, Body: []byte(strings.Repeat("a", 64*1024+1)), Status: 413, Code: apiwire.CodeBodyTooLarge},
		})
		_, err := api.CDP(gh.Token).Dial(ctx, "ws://x"+wsPath)
		wantHTTPError(t, "CDP WebSocket upgrade with a HUMAN token (generic Runtime.evaluate path)", err, 403)
		release(t, api, gh)
		check(t, []statusRow{
			{Name: "ended HUMAN token on clipboard", Method: "PUT", Path: api.WorkspacePath("/clipboard"), Token: gh.Token, Headers: textPlain, Body: []byte("x"), Status: 409, Code: apiwire.CodeLeaseEnded},
			{Name: "ended HUMAN token on cdp", Method: "GET", Path: cdpVersion, Token: gh.Token, Status: 409, Code: apiwire.CodeLeaseEnded},
		})
	})

	t.Run("agent_lease_held", func(t *testing.T) {
		ga := acquire(t, api, modeAgent, 60)
		ctx := timeout(t, 60*time.Second)
		check(t, []statusRow{
			{Name: "AGENT token on /json/version", Method: "GET", Path: cdpVersion, Token: ga.Token, Status: 200},
			{Name: "AGENT token on /json/list", Method: "GET", Path: cdpPath(api, "/json/list"), Token: ga.Token, Status: 200},
			{Name: "foreign Origin with a valid token", Method: "GET", Path: cdpVersion, Token: ga.Token, Headers: map[string]string{"Origin": "http://evil.example"}, Status: 403, Code: apiwire.CodeOriginNotAllowed},
			{Name: "foreign Origin without a token", Method: "GET", Path: cdpVersion, Headers: map[string]string{"Origin": "http://evil.example"}, Status: 403, Code: apiwire.CodeOriginNotAllowed},
			{Name: "route outside the allowlist", Method: "GET", Path: cdpPath(api, "/json/foo"), Token: ga.Token, Status: 404, Code: apiwire.CodeNotFound},
			{Name: "devtools frontend is not proxied", Method: "GET", Path: cdpPath(api, "/devtools/inspector.html"), Token: ga.Token, Status: 404, Code: apiwire.CodeNotFound},
			{Name: "AGENT token cannot write the clipboard", Method: "PUT", Path: api.WorkspacePath("/clipboard"), Token: ga.Token, Headers: map[string]string{"Content-Type": "text/plain; charset=utf-8"}, Body: []byte("x"), Status: 403, Code: apiwire.CodeWrongLeaseMode},
			{Name: "AGENT token cannot acquire leases", Method: "POST", Path: api.WorkspacePath("/leases"), Token: ga.Token, Headers: idem, Body: acquireBody, Status: 401, Code: apiwire.CodeUnauthenticated},
			{Name: "AGENT token cannot read the workspace", Method: "GET", Path: api.WorkspacePath(""), Token: ga.Token, Status: 401, Code: apiwire.CodeUnauthenticated},
		})

		// Discovery documents point back at the proxy and never leak Chromium's own endpoints.
		for _, p := range []string{"/json/version", "/json/list", "/json/protocol"} {
			resp, err := api.Do(ctx, http.MethodGet, cdpPath(api, p), ga.Token, nil, nil)
			if err != nil || resp.Status != 200 {
				t.Errorf("GET %s = %v, %d", p, err, resp.Status)
				continue
			}
			body := string(resp.Body)
			if p != "/json/protocol" && strings.Contains(body, "9223") {
				t.Errorf("%s leaks the upstream CDP port: %s", p, trim(body))
			}
			if strings.Contains(body, "devtoolsFrontendUrl") {
				t.Errorf("%s still carries devtoolsFrontendUrl", p)
			}
			if strings.Contains(body, ga.Token) {
				t.Errorf("%s echoes the lease token", p)
			}
		}

		// A WebSocket upgrade with a foreign Origin is refused as well.
		hdr := http.Header{"Authorization": {"Bearer " + ga.Token}, "Origin": {"http://evil.example"}}
		_, err := cdpc.Dial(ctx, "ws"+strings.TrimPrefix(api.Base, "http")+wsPath, cdpc.DialOptions{Header: hdr})
		wantHTTPError(t, "WebSocket upgrade with a foreign Origin", err, 403)

		// Denied methods get a synthetic CDP error and the connection survives.
		c := browserClient(t, api, ga.Token)
		for _, m := range []string{"Browser.close", "Browser.crash", "Target.exposeDevToolsProtocol", "Extensions.loadUnpacked"} {
			err := c.Call(ctx, m, map[string]any{}, nil)
			var ce *cdpc.Error
			if !errors.As(err, &ce) || ce.Code != -32000 || !strings.Contains(ce.Message, "denied") {
				t.Errorf("%s: got %v, want CDP error -32000 \"Method denied by LensDock policy\"", m, err)
			}
		}
		if err := c.Call(ctx, "Browser.getVersion", nil, nil); err != nil {
			t.Errorf("connection did not survive denied methods: %v", err)
		}
		c.Close()

		release(t, api, ga)
		check(t, []statusRow{
			{Name: "ended AGENT token on cdp", Method: "GET", Path: cdpVersion, Token: ga.Token, Status: 409, Code: apiwire.CodeLeaseEnded},
		})
		_, err = api.CDP(ga.Token).Dial(ctx, "ws://x"+wsPath)
		wantHTTPError(t, "CDP WebSocket upgrade with an ended AGENT token", err, 409)
	})

	t.Run("published_ports_are_only_the_api", func(t *testing.T) {
		// The CDP (9223) and VNC (5900/5901) ports are loopback-only inside the
		// container. A host dial proves nothing: a host service may own the same
		// port number (macOS Screen Sharing listens on *:5900), so the container's
		// published-port list is the authoritative check.
		dk := harness.NewDocker(t)
		out, err := exec.CommandContext(timeout(t, 15*time.Second), "docker", "port", dk.Container).CombinedOutput()
		if err != nil {
			t.Fatalf("docker port: %v: %s", err, out)
		}
		ev.Save(t, "TestCT10/docker-port.txt", out)
		var lines []string
		for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if l = strings.TrimSpace(l); l != "" {
				lines = append(lines, l)
			}
		}
		for _, l := range lines {
			if !strings.HasPrefix(l, "8080/tcp") || !strings.HasSuffix(l, "127.0.0.1:18080") {
				t.Errorf("unexpected published port mapping %q (only 8080/tcp -> 127.0.0.1:18080 is allowed)", l)
			}
		}
		if len(lines) == 0 {
			t.Error("docker port reports no mapping; the API should be published on 127.0.0.1:18080")
		}
	})

	t.Run("bridge_refuses_human_lease_and_non_loopback_listen", func(t *testing.T) {
		cli := harness.NewCLI(t, api)
		dir := t.TempDir()

		humanFile := filepath.Join(dir, "human.json")
		if err := cli.AcquireToFile(timeout(t, 30*time.Second), modeHuman, 30, humanFile); err != nil {
			t.Fatalf("lensdock lease acquire (human): %v", err)
		}
		runBridge := func(file, listen string) harness.Result {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			res, err := cli.Run(ctx, "cdp", "bridge", "--lease-file", file, "--listen", listen)
			if err != nil || ctx.Err() != nil {
				t.Fatalf("bridge did not refuse within 20s (listen %s): %v", listen, err)
			}
			return res
		}
		res := runBridge(humanFile, "127.0.0.1:0")
		if res.ExitCode != 2 || len(strings.TrimSpace(string(res.Stdout))) != 0 {
			t.Errorf("bridge with a HUMAN lease file: exit %d, stdout %q; want exit 2 (usage) and no endpoints", res.ExitCode, res.Stdout)
		}
		if r, err := cli.ReleaseFile(timeout(t, 15*time.Second), humanFile); err != nil || r.ExitCode != 0 {
			t.Fatalf("release of the human lease: exit %d, %v", r.ExitCode, err)
		}

		agentFile := filepath.Join(dir, "agent.json")
		if err := cli.AcquireToFile(timeout(t, 30*time.Second), modeAgent, 30, agentFile); err != nil {
			t.Fatalf("lensdock lease acquire (agent): %v", err)
		}
		t.Cleanup(func() { _, _ = cli.ReleaseFile(timeout(t, 15*time.Second), agentFile) })
		res = runBridge(agentFile, "0.0.0.0:0")
		if res.ExitCode != 2 || len(strings.TrimSpace(string(res.Stdout))) != 0 {
			t.Errorf("bridge with --listen 0.0.0.0:0: exit %d, stdout %q; want exit 2 (usage) and no endpoints", res.ExitCode, res.Stdout)
		}
	})
}

func trim(s string) string {
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}
