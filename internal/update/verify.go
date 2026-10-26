package update

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"image/png"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// Check statuses of a VerifyReport.
const (
	CheckPass = "pass"
	CheckFail = "fail"
	CheckSkip = "skip"
)

// Fixed local fixture content. The page is generated from these constants and
// served from the sidecar on loopback, so the candidate browser loads it
// without any outbound connection.
const (
	fixtureTitle = "LensDock candidate fixture"
	fixtureText  = "繁體中文 简体中文 日本語 한국어 😀 👍🏽 🇹🇼 👨‍👩‍👧"
)

// DefaultEgressTargets are connection targets a candidate must not reach: a
// public resolver (HTTPS and DNS) and the cloud metadata address.
var DefaultEgressTargets = []string{"1.1.1.1:443", "8.8.8.8:53", "169.254.169.254:80"}

// VerifySpec parametrizes candidate verification.
type VerifySpec struct {
	// ExpectChromium is the dpkg version of the chromium package from the
	// release manifest (for example 138.0.7204.100-1~deb13u1).
	ExpectChromium string `json:"expectChromium"`
	// ExpectPackages is the manifest's package set of the image architecture
	// (name to version). The running image must report exactly this set.
	ExpectPackages map[string]string `json:"expectPackages"`
	// EgressTargets are host:port pairs that must be unreachable; empty uses
	// DefaultEgressTargets.
	EgressTargets []string `json:"egressTargets,omitempty"`
	// ReadyTimeoutSeconds bounds the wait for readiness (default 90).
	ReadyTimeoutSeconds int `json:"readyTimeoutSeconds,omitempty"`
}

// Check is one verification result.
type Check struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
	// Reason is a stable machine-readable code; Detail is diagnostic text only.
	Reason string `json:"reason,omitempty"`
}

// VerifyReport is the evidence of candidate verification.
type VerifyReport struct {
	OK                bool      `json:"ok"`
	Checks            []Check   `json:"checks"`
	StartedAt         time.Time `json:"startedAt"`
	FinishedAt        time.Time `json:"finishedAt"`
	ChromiumVersion   string    `json:"chromiumVersion,omitempty"`
	RuntimeGeneration string    `json:"runtimeGeneration,omitempty"`
}

func (r *VerifyReport) add(id, title, status, detail string) {
	r.Checks = append(r.Checks, Check{ID: id, Title: title, Status: status, Detail: detail})
}

func (r *VerifyReport) finish() {
	r.FinishedAt = time.Now().UTC()
	r.OK = len(r.Checks) > 0
	for _, c := range r.Checks {
		if c.blocks() {
			r.OK = false
		}
	}
}

const sandboxDisabledReason = "chromium-sandbox-disabled"

// sandboxCheck maps the runtime's sandbox report to a check result. Only an
// explicit enabled=false is a skip; enabled or missing requires verified.
func sandboxCheck(enabled *bool, verified bool) Check {
	c := Check{ID: "sandbox", Title: "renderer sandbox verified"}
	switch {
	case enabled != nil && !*enabled && verified:
		c.Status, c.Detail = CheckFail, "the runtime reports a verified sandbox that is not enabled"
	case enabled != nil && !*enabled:
		c.Status = CheckSkip
		c.Reason = sandboxDisabledReason
		c.Detail = "Chromium sandbox not enabled (LENSDOCK_CHROMIUM_SANDBOX is off)"
	case verified:
		c.Status = CheckPass
	default:
		c.Status, c.Detail = CheckFail, "the runtime reports no verified sandbox"
	}
	return c
}

// blocks reports whether the check keeps a candidate from passing: anything
// but a pass, except the sandbox check of a runtime that has it disabled.
func (c Check) blocks() bool {
	if c.Status == CheckPass {
		return false
	}
	return !(c.ID == "sandbox" && c.Status == CheckSkip && c.Reason == sandboxDisabledReason)
}

// Failed returns the failing checks.
func (r VerifyReport) Failed() []Check {
	var out []Check
	for _, c := range r.Checks {
		if c.Status == CheckFail {
			out = append(out, c)
		}
	}
	return out
}

// upstreamVersion strips the epoch and the Debian revision from a dpkg
// version: "1:138.0.7204.100-1~deb13u1" gives "138.0.7204.100".
func upstreamVersion(v string) string {
	v = epochPattern.ReplaceAllString(v, "")
	if i := strings.LastIndex(v, "-"); i > 0 {
		v = v[:i]
	}
	return v
}

// comparePackages lists the differences between the manifest's package set
// and the one the running image reports.
func comparePackages(want, got map[string]string) []string {
	var diffs []string
	for _, name := range sortedKeys(want) {
		switch g, ok := got[name]; {
		case !ok:
			diffs = append(diffs, "missing "+name)
		case g != want[name]:
			diffs = append(diffs, fmt.Sprintf("%s is %s, manifest says %s", name, g, want[name]))
		}
	}
	for _, name := range sortedKeys(got) {
		if _, ok := want[name]; !ok {
			diffs = append(diffs, "unexpected "+name)
		}
	}
	return diffs
}

// checkFonts requires fontconfig to resolve every query of the runtime's font
// diagnostics.
func checkFonts(fc map[string]string) error {
	if len(fc) < 5 {
		return fmt.Errorf("fc-match reports %d queries, expected 5", len(fc))
	}
	for _, k := range sortedKeys(fc) {
		if strings.TrimSpace(fc[k]) == "" {
			return fmt.Errorf("fc-match %q resolved to nothing", k)
		}
	}
	for k, v := range fc {
		if k == "emoji" && !strings.Contains(strings.ToLower(v), "emoji") {
			return fmt.Errorf("fc-match emoji resolved to %q", v)
		}
	}
	return nil
}

// verifyCandidate runs the candidate checks against a runtime reachable
// through c. It never touches anything but the candidate: the lease it takes
// ends before it returns.
func verifyCandidate(ctx context.Context, c *runtimeClient, spec VerifySpec) VerifyReport {
	rep := VerifyReport{StartedAt: time.Now().UTC()}

	timeout := time.Duration(spec.ReadyTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	var doc readyDoc
	var st RuntimeStatus
	var lastErr error
	deadline := time.Now().Add(timeout)
	for {
		st, doc, lastErr = c.status(ctx)
		if lastErr == nil && st.Ready && !st.Locked {
			break
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
	ready := lastErr == nil && st.Ready && !st.Locked
	switch {
	case lastErr != nil:
		rep.add("ready", "runtime ready (Chromium, CDP and both VNC entries answer)", CheckFail, lastErr.Error())
	case !ready:
		rep.add("ready", "runtime ready (Chromium, CDP and both VNC entries answer)", CheckFail,
			fmt.Sprintf("not ready after %s (locked=%t lockCode=%q)", timeout, st.Locked, st.LockCode))
	default:
		rep.add("ready", "runtime ready (Chromium, CDP and both VNC entries answer)", CheckPass, "")
		rep.ChromiumVersion, rep.RuntimeGeneration = st.ChromiumVersion, st.RuntimeGeneration
	}

	if ready {
		// Sandbox: when enabled (or when the runtime does not say), the
		// renderer sandbox is a release gate. Only an explicit enabled=false
		// is a skip; an unverified sandbox is never read as disabled.
		rep.Checks = append(rep.Checks, sandboxCheck(doc.Detail.Sandbox.Enabled, doc.Detail.Sandbox.Verified))
		if err := checkFonts(doc.Detail.FcMatch); err != nil {
			rep.add("fonts", "CJK and emoji fonts resolve", CheckFail, err.Error())
		} else {
			rep.add("fonts", "CJK and emoji fonts resolve", CheckPass, "")
		}
		want := upstreamVersion(spec.ExpectChromium)
		if want != "" && strings.Contains(doc.Detail.ChromiumVersion, want) {
			rep.add("chromium-version", "Chromium matches the release manifest", CheckPass, doc.Detail.ChromiumVersion)
		} else {
			rep.add("chromium-version", "Chromium matches the release manifest", CheckFail,
				fmt.Sprintf("runtime reports %q, manifest needs %q", doc.Detail.ChromiumVersion, want))
		}
		if diffs := comparePackages(spec.ExpectPackages, doc.Detail.Packages); len(diffs) == 0 {
			rep.add("packages", "installed packages equal the manifest", CheckPass, fmt.Sprintf("%d packages", len(spec.ExpectPackages)))
		} else {
			if len(diffs) > 10 {
				diffs = append(diffs[:10], fmt.Sprintf("... and %d more", len(diffs)-10))
			}
			rep.add("packages", "installed packages equal the manifest", CheckFail, strings.Join(diffs, "; "))
		}
		verifyBrowser(ctx, c, spec, &rep)
	} else {
		for _, id := range []string{"sandbox", "fonts", "chromium-version", "packages", "cdp", "fixture"} {
			rep.add(id, id, CheckSkip, "runtime is not ready")
		}
	}

	targets := spec.EgressTargets
	if len(targets) == 0 {
		targets = DefaultEgressTargets
	}
	if err := checkEgressDenied(ctx, targets); err != nil {
		rep.add("egress-denied", "candidate has no route to the outside", CheckFail, err.Error())
	} else {
		rep.add("egress-denied", "candidate has no route to the outside", CheckPass, fmt.Sprintf("%d targets and DNS unreachable", len(targets)))
	}
	rep.finish()
	return rep
}

// checkEgressDenied fails when any target answers or a public name resolves.
func checkEgressDenied(ctx context.Context, targets []string) error {
	var reached []string
	for _, t := range targets {
		d := net.Dialer{Timeout: 3 * time.Second}
		conn, err := d.DialContext(ctx, "tcp", t)
		if err == nil {
			_ = conn.Close()
			reached = append(reached, t)
		}
	}
	if len(reached) > 0 {
		return fmt.Errorf("reachable from the candidate network: %s", strings.Join(reached, ", "))
	}
	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	r := net.Resolver{PreferGo: true}
	if addrs, err := r.LookupHost(rctx, "example.com"); err == nil && len(addrs) > 0 {
		return errors.New("a public DNS name resolved from the candidate network")
	}
	return nil
}

func fixtureHTML() string {
	return `<!doctype html><html lang="zh-Hant"><head><meta charset="utf-8"><title>` + html.EscapeString(fixtureTitle) +
		`</title></head><body><p id="t">` + html.EscapeString(fixtureText) + `</p></body></html>`
}

// verifyBrowser takes an AGENT lease, drives the candidate browser through the
// runtime's CDP proxy to a local fixture, and releases the lease.
func verifyBrowser(ctx context.Context, c *runtimeClient, spec VerifySpec, rep *VerifyReport) {
	leaseID, capability, err := c.acquireAgent(ctx)
	if err != nil {
		rep.add("cdp", "CDP through the lease-limited proxy", CheckFail, err.Error())
		rep.add("fixture", "local fixture renders", CheckFail, "no lease")
		return
	}
	defer func() {
		rctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = c.revokeLease(rctx, leaseID)
	}()

	cdpBase := c.wsPath("/cdp")
	code, body, err := c.call(ctx, http.MethodGet, cdpBase+"/json/version", nil, capability, nil)
	if err != nil || code != http.StatusOK {
		detail := "no answer"
		if err != nil {
			detail = err.Error()
		} else {
			detail = apiFailure("GET json/version", code, body).Error()
		}
		rep.add("cdp", "CDP through the lease-limited proxy", CheckFail, detail)
		rep.add("fixture", "local fixture renders", CheckFail, "no CDP")
		return
	}
	var ver struct {
		Browser string `json:"Browser"`
	}
	if err := json.Unmarshal(body, &ver); err != nil || !strings.Contains(ver.Browser, upstreamVersion(spec.ExpectChromium)) {
		rep.add("cdp", "CDP through the lease-limited proxy", CheckFail, fmt.Sprintf("CDP browser %q does not match the manifest", ver.Browser))
		rep.add("fixture", "local fixture renders", CheckFail, "wrong browser")
		return
	}
	rep.add("cdp", "CDP through the lease-limited proxy", CheckPass, ver.Browser)

	title, err := c.renderFixture(ctx, cdpBase, capability)
	if err != nil {
		rep.add("fixture", "local fixture renders", CheckFail, err.Error())
		return
	}
	rep.add("fixture", "local fixture renders", CheckPass, title)
}

// renderFixture serves the fixture on loopback, opens it in a new tab through
// the proxy, reads the text back over CDP, takes a screenshot and closes the
// tab.
func (c *runtimeClient) renderFixture(ctx context.Context, cdpBase, capability string) (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("fixture listener: %w", err)
	}
	var served atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/fixture.html", func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(fixtureHTML()))
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	pageURL := "http://" + ln.Addr().String() + "/fixture.html"

	code, body, err := c.call(ctx, http.MethodPut, cdpBase+"/json/new?"+pageURL, nil, capability, nil)
	if err != nil {
		return "", err
	}
	if code != http.StatusOK {
		return "", apiFailure("PUT json/new", code, body)
	}
	var tab struct {
		ID string `json:"id"`
		WS string `json:"webSocketDebuggerUrl"`
	}
	if err := json.Unmarshal(body, &tab); err != nil || tab.WS == "" || tab.ID == "" {
		return "", errors.New("malformed json/new response")
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _, _ = c.call(cctx, http.MethodGet, cdpBase+"/json/close/"+url.PathEscape(tab.ID), nil, capability, nil)
	}()
	wsURL, err := c.rerootWS(tab.WS)
	if err != nil {
		return "", err
	}
	s, err := c.dialCDP(ctx, wsURL, capability)
	if err != nil {
		return "", err
	}
	defer s.close()
	if err := s.call(ctx, "Page.enable", nil, nil); err != nil {
		return "", err
	}
	// The tab was opened at the fixture URL; wait for its load, then read it.
	loadCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		var state string
		if err := s.evaluate(loadCtx, `location.href + "|" + document.readyState`, &state); err != nil {
			return "", err
		}
		// The new tab starts at about:blank, which is already "complete".
		if state == pageURL+"|complete" {
			break
		}
		select {
		case <-loadCtx.Done():
			return "", errors.New("fixture did not finish loading")
		case <-time.After(250 * time.Millisecond):
		}
	}
	var gotTitle, gotText string
	if err := s.evaluate(ctx, "document.title", &gotTitle); err != nil {
		return "", err
	}
	if err := s.evaluate(ctx, "document.getElementById('t') ? document.getElementById('t').textContent : ''", &gotText); err != nil {
		return "", err
	}
	if gotTitle != fixtureTitle {
		return "", fmt.Errorf("fixture title %q, expected %q", gotTitle, fixtureTitle)
	}
	if gotText != fixtureText {
		return "", fmt.Errorf("fixture text differs from the UTF-8 original: got %q", gotText)
	}
	if served.Load() == 0 {
		return "", errors.New("the fixture server never saw a request: the page did not come from the local fixture")
	}
	var shot struct {
		Data string `json:"data"`
	}
	if err := s.call(ctx, "Page.captureScreenshot", map[string]string{"format": "png"}, &shot); err != nil {
		return "", err
	}
	raw, err := base64.StdEncoding.DecodeString(shot.Data)
	if err != nil {
		return "", errors.New("screenshot is not base64")
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(raw))
	if err != nil || cfg.Width < 1 || cfg.Height < 1 {
		return "", errors.New("screenshot is not a valid PNG")
	}
	return fmt.Sprintf("%s (screenshot %dx%d)", gotTitle, cfg.Width, cfg.Height), nil
}

// rerootWS keeps the path of the reported WebSocket URL and moves it onto the
// scheme and host of the client's base URL.
func (c *runtimeClient) rerootWS(reported string) (string, error) {
	u, err := url.Parse(reported)
	if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") || !strings.HasPrefix(u.Path, c.wsPath("/cdp/devtools/")) {
		return "", errors.New("the proxy reported an unexpected WebSocket URL")
	}
	scheme := "ws"
	if c.base.Scheme == "https" {
		scheme = "wss"
	}
	return scheme + "://" + c.base.Host + u.Path, nil
}

// cdpSession is a minimal synchronous CDP client: one command in flight, events
// are skipped.
type cdpSession struct {
	conn *websocket.Conn
	id   int64
}

func (c *runtimeClient) dialCDP(ctx context.Context, wsURL, capability string) (*cdpSession, error) {
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+capability)
	conn, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPClient: c.wsClient, HTTPHeader: hdr, CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("CDP WebSocket refused: HTTP %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("CDP WebSocket: %w", err)
	}
	conn.SetReadLimit(64 << 20)
	return &cdpSession{conn: conn}, nil
}

func (s *cdpSession) close() { _ = s.conn.Close(websocket.StatusNormalClosure, "") }

func (s *cdpSession) call(ctx context.Context, method string, params, out any) error {
	s.id++
	id := s.id
	req := struct {
		ID     int64  `json:"id"`
		Method string `json:"method"`
		Params any    `json:"params,omitempty"`
	}{id, method, params}
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if err := s.conn.Write(ctx, websocket.MessageText, data); err != nil {
		return fmt.Errorf("CDP %s: %w", method, err)
	}
	for {
		_, msg, err := s.conn.Read(ctx)
		if err != nil {
			return fmt.Errorf("CDP %s: %w", method, err)
		}
		var env struct {
			ID     *int64          `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(msg, &env) != nil || env.ID == nil || *env.ID != id {
			continue
		}
		if env.Error != nil {
			return fmt.Errorf("CDP %s: error %d: %s", method, env.Error.Code, env.Error.Message)
		}
		if out != nil && len(env.Result) > 0 {
			return json.Unmarshal(env.Result, out)
		}
		return nil
	}
}

func (s *cdpSession) evaluate(ctx context.Context, expr string, out any) error {
	var res struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	if err := s.call(ctx, "Runtime.evaluate", map[string]any{"expression": expr, "returnByValue": true}, &res); err != nil {
		return err
	}
	if res.ExceptionDetails != nil {
		return fmt.Errorf("JavaScript exception: %s", res.ExceptionDetails.Text)
	}
	return json.Unmarshal(res.Result.Value, out)
}

// failedChecksSummary renders the failing checks for an error message.
func failedChecksSummary(r VerifyReport) string {
	var parts []string
	for _, c := range r.Failed() {
		parts = append(parts, c.ID+": "+c.Detail)
	}
	sort.Strings(parts)
	return strings.Join(parts, "; ")
}
