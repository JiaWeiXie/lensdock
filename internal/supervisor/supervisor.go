package supervisor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
	"github.com/JiaWeiXie/lensdock/internal/clipboard"
	"github.com/JiaWeiXie/lensdock/internal/vnc"
)

const (
	stopTimeoutChromium = 20 * time.Second
	stopTimeoutDefault  = 2 * time.Second
	killWait            = 3 * time.Second
	unitFinishWait      = 5 * time.Second

	dbusGateTimeout    = 10 * time.Second
	xvfbGateTimeout    = 10 * time.Second
	openboxSettle      = 500 * time.Millisecond
	vncGateTimeout     = 15 * time.Second
	cdpGateTimeout     = 30 * time.Second
	sandboxGateTimeout = 15 * time.Second
	probeInterval      = 250 * time.Millisecond
	probeAttemptLimit  = 2 * time.Second
	maxVersionBody     = 1 << 20
	// Browser.close runs normal Chromium teardown, including Singleton cleanup.
	// Bound both the request and exit wait before falling back to OS signals.
	browserCloseWait = 3 * time.Second
	browserTermGrace = 5 * time.Second
)

// waitClosed reports whether done was closed within d.
func waitClosed(done <-chan struct{}, d time.Duration) bool {
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// exitStatus describes how a child ended without exposing its output: "clean"
// for exit status 0, "signal: terminated"/"signal: killed" when it died from a
// signal, or the wait error's text (never child output).
func exitStatus(err error) string {
	if err == nil {
		return "clean"
	}
	return err.Error()
}

// Unit states reported by Detail.
const (
	stateStarting = "starting"
	stateRunning  = "running"
	stateBackoff  = "backoff"
	stateStopped  = "stopped"
	stateBlocked  = "blocked"
	stateFailed   = "failed"
)

// Hooks connects the supervisor to the lease manager. All fields are optional.
type Hooks struct {
	// Ready is called when every readiness gate has passed.
	Ready func()
	// NotReady is called when readiness is lost or blocked, with an error
	// code such as SANDBOX_PREFLIGHT_FAILED or RUNTIME_NOT_READY.
	NotReady func(code string)
	// BrowserRestarted is called once when a running Chromium exits
	// unexpectedly, before readiness drops. The lease manager rotates the
	// lease generation here, so a held lease ends as a generation change
	// rather than as the lock that NotReady applies next. Failed relaunches do
	// not call it: the runtime stays locked, so no lease can bind to them.
	BrowserRestarted func()
	// StopBrowser requests normal Chromium shutdown before sending OS signals.
	// SIGTERM uses SessionEnding's immediate exit and leaves Singleton locks,
	// so it is only a bounded fallback. The group kill remains the last resort.
	StopBrowser func(ctx context.Context) error
}

// blockedError marks a failure that must not be retried: the runtime stays
// not-ready with the given code while display and VNC remain up.
type blockedError struct {
	code string
	err  error
}

func (e *blockedError) Error() string { return e.err.Error() }
func (e *blockedError) Unwrap() error { return e.err }

type gateFunc func(ctx context.Context, c *child) error

type unit struct {
	name        string
	fatal       bool // exit ends Run with an error
	browser     bool // first-start failure is not fatal; exit bumps the generation
	stopTimeout time.Duration
	launch      func(ctx context.Context) (*child, gateFunc, error)

	first    chan error // first start outcome (buffered, 1)
	finished chan struct{}

	mu       sync.Mutex
	cur      *child
	state    string
	restarts int
	stopped  bool
	started  bool // a supervise goroutine owns the unit; only then is finished closed
}

func (u *unit) setState(s string) {
	u.mu.Lock()
	u.state = s
	u.mu.Unlock()
}

// Supervisor runs the container's process tree.
type Supervisor struct {
	hooks Hooks
	log   *slog.Logger
	units []*unit
	now   func() time.Time
	after func(time.Duration) <-chan time.Time // restart delay timer

	running atomic.Bool
	ready   atomic.Bool
	fatal   chan error

	hookMu sync.Mutex // serializes readiness hook calls

	mu          sync.Mutex
	unitReady   map[string]bool
	blocked     string
	lastReady   bool
	lastCode    string
	chromiumVer string
	sandbox     SandboxReport

	// chromiumSandbox is the configured opt-in to Chromium's sandbox. It is
	// fixed at construction and never changes while running.
	chromiumSandbox bool

	profileMu   sync.Mutex
	profileLock *profileLock

	detailMu sync.Mutex
	fonts    map[string]string
	packages map[string]string
}

// New builds a Supervisor. It does not start anything. chromiumSandbox opts
// in to Chromium's own sandbox: when true the user-namespace preflight and the
// renderer sandbox proof are required and fail closed; when false Chromium
// starts with --no-sandbox.
func New(hooks Hooks, log *slog.Logger, chromiumSandbox bool) *Supervisor {
	s := newSupervisor(hooks, log)
	s.chromiumSandbox = chromiumSandbox
	s.units = s.buildUnits()
	return s
}

func newSupervisor(hooks Hooks, log *slog.Logger) *Supervisor {
	if log == nil {
		log = slog.Default()
	}
	return &Supervisor{
		hooks:     hooks,
		log:       log,
		now:       time.Now,
		after:     time.After,
		fatal:     make(chan error, 1),
		unitReady: make(map[string]bool),
		lastCode:  apiwire.CodeRuntimeNotReady,
	}
}

func newUnit(name string, fatal, browser bool, stop time.Duration, launch func(context.Context) (*child, gateFunc, error)) *unit {
	return &unit{
		name: name, fatal: fatal, browser: browser, stopTimeout: stop, launch: launch,
		first: make(chan error, 1), finished: make(chan struct{}), state: stateStopped,
	}
}

func (s *Supervisor) buildUnits() []*unit {
	mk := newUnit
	simple := func(name string, argv []string, gate gateFunc) func(context.Context) (*child, gateFunc, error) {
		return func(context.Context) (*child, gateFunc, error) {
			c, err := s.start(name, argv)
			return c, gate, err
		}
	}
	vncUnit := func(inst vnc.Instance) *unit {
		argv := append([]string{"/usr/bin/x11vnc"}, inst.Args(display, xauthorityPath)...)
		addr := fmt.Sprintf("127.0.0.1:%d", inst.Port)
		return mk(inst.Name, false, false, stopTimeoutDefault, simple(inst.Name, argv, func(ctx context.Context, _ *child) error {
			return pollUntil(ctx, vncGateTimeout, inst.Name, func(ctx context.Context) error {
				_, err := vnc.Probe(ctx, addr)
				return err
			})
		}))
	}
	return []*unit{
		mk("dbus", false, false, stopTimeoutDefault, simple("dbus",
			[]string{"/usr/bin/dbus-daemon", "--session", "--address=" + dbusAddress, "--nofork", "--nopidfile"},
			func(ctx context.Context, _ *child) error {
				return pollUntil(ctx, dbusGateTimeout, "dbus socket", func(ctx context.Context) error {
					var d net.Dialer
					conn, err := d.DialContext(ctx, "unix", dbusSocketPath)
					if err != nil {
						return err
					}
					return conn.Close()
				})
			})),
		mk("xvfb", true, false, stopTimeoutDefault, s.launchXvfb),
		mk("openbox", false, false, stopTimeoutDefault, simple("openbox",
			[]string{"/usr/bin/openbox", "--config-file", openboxRC},
			func(ctx context.Context, c *child) error {
				select {
				case <-time.After(openboxSettle):
					return nil
				case <-c.done:
					return errors.New("openbox exited during startup")
				case <-ctx.Done():
					return ctx.Err()
				}
			})),
		vncUnit(vnc.View),
		vncUnit(vnc.Control),
		mk("chromium", false, true, stopTimeoutChromium, s.launchChromium),
	}
}

// start launches argv with output captured into capped log lines.
func (s *Supervisor) start(name string, argv []string) (*child, error) {
	return startChild(argv, startOpts{
		Stdout: newLineLogger(s.log, name, "stdout"),
		Stderr: newLineLogger(s.log, name, "stderr"),
	})
}

func (s *Supervisor) launchXvfb(context.Context) (*child, gateFunc, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	argv := []string{
		"/usr/bin/Xvfb", display, "-screen", "0", screenGeometry,
		"-auth", xauthorityPath, "-nolisten", "tcp", "-noreset", "-displayfd", "3",
	}
	c, err := startChild(argv, startOpts{
		Stdout:     newLineLogger(s.log, "xvfb", "stdout"),
		Stderr:     newLineLogger(s.log, "xvfb", "stderr"),
		ExtraFiles: []*os.File{w},
	})
	_ = w.Close() // the child holds its own copy
	if err != nil {
		_ = r.Close()
		return nil, nil, err
	}
	gate := func(ctx context.Context, _ *child) error {
		defer r.Close()
		ctx, cancel := context.WithTimeout(ctx, xvfbGateTimeout)
		defer cancel()
		return readDisplayFD(ctx, r)
	}
	return c, gate, nil
}

// readDisplayFD waits for Xvfb to report its display number on -displayfd,
// which it does only once it is accepting connections.
func readDisplayFD(ctx context.Context, r *os.File) error {
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := bufio.NewReader(io.LimitReader(r, 64)).ReadString('\n')
		ch <- result{line, err}
	}()
	select {
	case res := <-ch:
		if res.err != nil {
			return fmt.Errorf("xvfb displayfd: %w", res.err)
		}
		if got := strings.TrimSpace(res.line); got != fmt.Sprint(displayNumber) {
			return fmt.Errorf("xvfb picked display %q, want %d", got, displayNumber)
		}
		return nil
	case <-ctx.Done():
		_ = r.Close()
		return fmt.Errorf("xvfb not ready: %w", ctx.Err())
	}
}

func (s *Supervisor) launchChromium(ctx context.Context) (*child, gateFunc, error) {
	if s.chromiumSandbox {
		if err := Preflight(ctx); err != nil {
			return nil, nil, &blockedError{code: apiwire.CodeSandboxPreflightFailed, err: err}
		}
	}
	lock, err := s.acquireProfileLock(ctx)
	if err != nil {
		return nil, nil, err
	}
	c, err := s.start("chromium", ChromiumArgv(s.chromiumSandbox))
	if err != nil {
		return nil, nil, err
	}
	go s.watchProfileLock(lock, c)
	return c, s.chromiumGate, nil
}

// chromiumGate always waits for the CDP endpoint. With the sandbox enabled it
// then proves the renderer sandbox; a sandbox that cannot be proven blocks
// readiness permanently. With the sandbox disabled there is nothing to prove.
func (s *Supervisor) chromiumGate(ctx context.Context, c *child) error {
	var version string
	err := pollUntil(ctx, cdpGateTimeout, "CDP /json/version", func(ctx context.Context) error {
		v, err := fetchBrowserVersion(ctx)
		version = v
		return err
	})
	if err != nil {
		return err
	}

	if s.chromiumSandbox {
		var report SandboxReport
		err = pollUntil(ctx, sandboxGateTimeout, "sandbox verification", func(context.Context) error {
			r, err := InspectSandbox(procRoot, c.pid)
			report = r
			return err
		})
		s.mu.Lock()
		s.sandbox = report
		s.mu.Unlock()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return &blockedError{code: apiwire.CodeSandboxPreflightFailed, err: err}
		}
	}

	s.mu.Lock()
	s.chromiumVer = version
	s.mu.Unlock()
	return nil
}

// fetchBrowserVersion issues GET /json/version against Chromium and returns
// the Browser field.
func fetchBrowserVersion(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, probeAttemptLimit)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+cdpAddr+"/json/version", nil)
	if err != nil {
		return "", err
	}
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxVersionBody))
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}
	var body struct {
		Browser string `json:"Browser"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxVersionBody)).Decode(&body); err != nil {
		return "", fmt.Errorf("decode: %w", err)
	}
	return body.Browser, nil
}

// pollUntil retries probe every probeInterval until it succeeds, the timeout
// elapses, or ctx ends.
func pollUntil(ctx context.Context, timeout time.Duration, what string, probe func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var last error
	for {
		attempt, cancelAttempt := context.WithTimeout(ctx, probeAttemptLimit)
		last = probe(attempt)
		cancelAttempt()
		if last == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s not ready after %s: %w", what, timeout, last)
		case <-time.After(probeInterval):
		}
	}
}

// Run starts every unit in order, gating each on its readiness check, then
// supervises until ctx ends (nil) or a fatal unit fails (error). Units are
// stopped in reverse order before Run returns. Call it after DropPrivileges.
func (s *Supervisor) Run(ctx context.Context) error {
	if !s.running.CompareAndSwap(false, true) {
		return errors.New("supervisor: already running")
	}
	if _, err := os.Lstat(xauthorityPath); err != nil {
		return fmt.Errorf("supervisor: xauthority missing (PrepareRoot must run first): %w", err)
	}
	return s.run(ctx)
}

func (s *Supervisor) run(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var runErr error

start:
	for _, u := range s.units {
		u.mu.Lock()
		u.started = true
		u.mu.Unlock()
		go s.supervise(runCtx, u)
		if u.browser {
			continue
		}
		select {
		case err := <-u.first:
			if err != nil {
				// A shutdown request that lands mid-startup reaches the unit
				// as its context error; that is a clean exit, not a failure.
				if ctx.Err() == nil {
					runErr = fmt.Errorf("start %s: %w", u.name, err)
				}
				break start
			}
		case err := <-s.fatal:
			runErr = err
			break start
		case <-ctx.Done():
			break start
		}
	}
	if runErr == nil && ctx.Err() == nil {
		select {
		case <-ctx.Done():
		case runErr = <-s.fatal:
		}
	}

	cancel()
	s.shutdown()
	return runErr
}

// supervise owns one unit's lifecycle: launch, gate, wait, restart.
func (s *Supervisor) supervise(ctx context.Context, u *unit) {
	defer close(u.finished)
	bo := newBackoff(s.now)
	firstSent := false
	report := func(err error) {
		if !firstSent {
			firstSent = true
			u.first <- err
		}
	}

	for {
		if ctx.Err() != nil {
			u.setState(stateStopped)
			report(ctx.Err())
			return
		}
		u.setState(stateStarting)
		// Every attempt counts, so repeated launch failures keep growing the
		// restart delay instead of resetting it.
		bo.Started()
		c, gate, err := u.launch(ctx)
		var blocked *blockedError
		if errors.As(err, &blocked) {
			s.block(u, blocked)
			report(err)
			<-ctx.Done()
			return
		}
		if err == nil {
			u.mu.Lock()
			if u.stopped {
				u.mu.Unlock()
				_ = c.killGroup()
				<-c.done
				return
			}
			u.cur = c
			u.mu.Unlock()
			err = s.runGate(ctx, c, gate)
		}

		if err == nil {
			u.setState(stateRunning)
			s.setUnitReady(u.name, true)
			report(nil)
			<-c.done
			if u.browser && ctx.Err() == nil && s.hooks.BrowserRestarted != nil {
				s.hooks.BrowserRestarted()
			}
			s.setUnitReady(u.name, false)
			err = fmt.Errorf("exited: %w", orExit(c.err))
		} else if c != nil {
			if ctx.Err() == nil {
				// Gate failed or the process died early: make sure it is gone.
				_ = c.killGroup()
			}
			<-c.done
			if errors.As(err, &blocked) {
				s.block(u, blocked)
				report(err)
				<-ctx.Done()
				return
			}
		}

		if ctx.Err() != nil {
			u.setState(stateStopped)
			report(ctx.Err())
			return
		}
		s.log.Warn("unit stopped", "unit", u.name, "error", err.Error())
		if c != nil {
			_ = c.killGroup() // sweep survivors of the group
		}

		if u.fatal {
			u.setState(stateFailed)
			report(err)
			s.reportFatal(fmt.Errorf("%s: %w", u.name, err))
			return
		}
		if !firstSent && !u.browser {
			u.setState(stateFailed)
			report(err)
			return
		}
		report(err)

		delay := bo.Exited()
		u.mu.Lock()
		u.restarts++
		u.mu.Unlock()
		u.setState(stateBackoff)
		select {
		case <-ctx.Done():
		case <-s.after(delay):
		}
	}
}

func orExit(err error) error {
	if err == nil {
		return errors.New("process exited")
	}
	return err
}

// runGate runs gate concurrently with the process; it fails early if the
// process exits first.
func (s *Supervisor) runGate(ctx context.Context, c *child, gate gateFunc) error {
	gctx, cancel := context.WithCancel(ctx)
	defer cancel()
	res := make(chan error, 1)
	go func() { res <- gate(gctx, c) }()
	select {
	case err := <-res:
		return err
	case <-c.done:
		return fmt.Errorf("exited before ready: %w", orExit(c.err))
	}
}

func (s *Supervisor) reportFatal(err error) {
	select {
	case s.fatal <- err:
	default:
	}
}

func (s *Supervisor) block(u *unit, b *blockedError) {
	s.log.Error("runtime blocked", "unit", u.name, "code", b.code, "error", b.err.Error())
	u.setState(stateBlocked)
	s.mu.Lock()
	s.blocked = b.code
	s.mu.Unlock()
	s.updateReadiness()
}

func (s *Supervisor) setUnitReady(name string, ok bool) {
	s.mu.Lock()
	s.unitReady[name] = ok
	s.mu.Unlock()
	s.updateReadiness()
}

// updateReadiness recomputes aggregate readiness and fires hooks on change.
func (s *Supervisor) updateReadiness() {
	s.hookMu.Lock()
	defer s.hookMu.Unlock()

	s.mu.Lock()
	ready := s.blocked == ""
	for _, u := range s.units {
		ready = ready && s.unitReady[u.name]
	}
	code := apiwire.CodeRuntimeNotReady
	if s.blocked != "" {
		code = s.blocked
	}
	changed := ready != s.lastReady || (!ready && code != s.lastCode)
	s.lastReady, s.lastCode = ready, code
	s.mu.Unlock()

	s.ready.Store(ready)
	if !changed {
		return
	}
	if ready {
		if s.hooks.Ready != nil {
			s.hooks.Ready()
		}
	} else if s.hooks.NotReady != nil {
		s.hooks.NotReady(code)
	}
}

// shutdown stops units in reverse start order.
func (s *Supervisor) shutdown() {
	for i := len(s.units) - 1; i >= 0; i-- {
		s.stopUnit(s.units[i])
	}
	// The profile lock outlives Chromium by design: it is released only
	// after the browser process is gone.
	s.releaseProfileLock()
	s.ready.Store(false)
}

func (s *Supervisor) stopUnit(u *unit) {
	u.mu.Lock()
	u.stopped = true
	c := u.cur
	started := u.started
	u.mu.Unlock()

	if c != nil {
		select {
		case <-c.done:
		default:
			exited := false
			if u.browser && s.hooks.StopBrowser != nil {
				ctx, cancel := context.WithTimeout(context.Background(), browserCloseWait)
				if err := s.hooks.StopBrowser(ctx); err != nil {
					// Chromium may close the channel before replying.
					s.log.Debug("browser close request ended", "unit", u.name, "error", err.Error())
				}
				cancel()
				exited = waitClosed(c.done, browserCloseWait)
			}
			if !exited {
				if err := c.signal(syscall.SIGTERM); err != nil {
					s.log.Warn("sigterm failed", "unit", u.name, "error", err.Error())
				}
				grace := u.stopTimeout
				if u.browser {
					grace = min(browserTermGrace, u.stopTimeout)
				}
				exited = waitClosed(c.done, grace)
			}
			switch {
			case exited:
				s.log.Info("unit exited", "unit", u.name, "status", exitStatus(c.err))
			default:
				s.log.Warn("unit ignored SIGTERM, killing group", "unit", u.name)
				_ = c.killGroup()
				if !waitClosed(c.done, killWait) {
					s.log.Error("unit did not exit after SIGKILL", "unit", u.name)
				}
			}
		}
		_ = c.killGroup() // sweep survivors of the group
	}
	if started {
		select {
		case <-u.finished:
		case <-time.After(unitFinishWait):
			s.log.Warn("unit supervisor did not finish", "unit", u.name)
		}
	}
	u.setState(stateStopped)
}

// Ready reports whether every readiness gate currently passes.
func (s *Supervisor) Ready() bool { return s.ready.Load() }

// StartBrowserProcess starts argv (absolute binary path) as the browser user
// with the child environment allowlist. Output is discarded: callers pass
// secrets-adjacent data on stdin and nothing from the child is logged.
func (s *Supervisor) StartBrowserProcess(ctx context.Context, argv []string, stdin io.Reader) (clipboard.Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c, err := startChild(argv, startOpts{Stdin: stdin})
	if err != nil {
		return nil, err
	}
	return &browserProc{c: c}, nil
}

type browserProc struct{ c *child }

func (p *browserProc) Wait() error {
	<-p.c.done
	return p.c.err
}

func (p *browserProc) Kill() error { return p.c.signal(syscall.SIGKILL) }

// errOutputTooLarge reports a child whose stdout exceeded the caller's limit.
var errOutputTooLarge = errors.New("child output exceeds the limit")

// OutputBrowserProcess runs argv (absolute binary path) as the browser user
// and returns its stdout, at most maxBytes when positive (zero means no
// limit). Stderr is discarded, and neither the output nor any part of it is
// logged or put into an error. The child is killed when ctx ends.
func (s *Supervisor) OutputBrowserProcess(ctx context.Context, argv []string, maxBytes int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := &cappedBuffer{max: maxBytes}
	c, err := startChild(argv, startOpts{Stdout: out})
	if err != nil {
		return nil, err
	}
	select {
	case <-c.done:
	case <-ctx.Done():
		_ = c.killGroup()
		<-c.done
		return nil, ctx.Err()
	}
	if c.err != nil {
		return nil, c.err
	}
	if out.overflow {
		return nil, errOutputTooLarge
	}
	return out.buf.Bytes(), nil
}

// cappedBuffer keeps the first max bytes written (all of them when max is not
// positive) and records overflow. It accepts every write so the child never
// blocks on a full pipe.
type cappedBuffer struct {
	max      int
	buf      bytes.Buffer
	overflow bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); b.max > 0 && len(p) > room {
		b.overflow = true
		if room > 0 {
			b.buf.Write(p[:room])
		}
		return len(p), nil
	}
	b.buf.Write(p)
	return len(p), nil
}

// Detail returns a diagnostic snapshot. CPU and memory come from live /proc
// reads; fonts and the package manifest are cached after the first complete
// read.
func (s *Supervisor) Detail() Detail {
	table, _ := readProcTable(procRoot)
	pageSize := os.Getpagesize()

	d := Detail{Units: make([]UnitDetail, 0, len(s.units))}
	for _, u := range s.units {
		u.mu.Lock()
		ud := UnitDetail{Name: u.name, State: u.state, Restarts: u.restarts}
		if u.cur != nil {
			select {
			case <-u.cur.done:
			default:
				ud.PID = u.cur.pid
			}
		}
		u.mu.Unlock()
		if ud.PID > 0 {
			ud.CPUTicks, ud.RSSBytes = resourceUsage(table, ud.PID, pageSize)
		}
		d.Units = append(d.Units, ud)
	}

	s.mu.Lock()
	d.ChromiumVersion = s.chromiumVer
	d.Sandbox = s.sandbox
	d.Sandbox.Enabled = s.chromiumSandbox
	s.mu.Unlock()

	s.detailMu.Lock()
	defer s.detailMu.Unlock()
	if s.fonts == nil {
		if fonts, ok := matchFonts(); ok {
			s.fonts = fonts
		} else {
			d.FcMatch = fonts
		}
	}
	if s.fonts != nil {
		d.FcMatch = s.fonts
	}
	if s.packages == nil {
		if pkgs, err := readManifest(); err == nil {
			s.packages = pkgs
		}
	}
	d.Packages = s.packages
	return d
}
