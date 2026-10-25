package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const testWait = 5 * time.Second

// hookRecorder turns lease hooks into an ordered event stream.
type hookRecorder struct{ ch chan string }

func newHookRecorder() *hookRecorder { return &hookRecorder{ch: make(chan string, 64)} }

func (r *hookRecorder) hooks() Hooks {
	return Hooks{
		Ready:            func() { r.ch <- "ready" },
		NotReady:         func(code string) { r.ch <- "not-ready:" + code },
		BrowserRestarted: func() { r.ch <- "browser-restarted" },
	}
}

// expect consumes the next events and fails unless they are exactly want.
func (r *hookRecorder) expect(t *testing.T, want ...string) {
	t.Helper()
	for _, w := range want {
		select {
		case got := <-r.ch:
			if got != w {
				t.Fatalf("hook event = %q, want %q", got, w)
			}
		case <-time.After(testWait):
			t.Fatalf("timed out waiting for hook event %q", w)
		}
	}
}

// fakeChild is a process that exits on demand or when signalled.
type fakeChild struct {
	*child
	finish func(error)
}

func newFakeChild() *fakeChild {
	c := &child{pid: 4242, done: make(chan struct{})}
	var once sync.Once
	finish := func(err error) {
		once.Do(func() {
			c.err = err
			close(c.done)
		})
	}
	c.sigHook = func(syscall.Signal, bool) error {
		finish(errors.New("signal: terminated"))
		return nil
	}
	return &fakeChild{child: c, finish: finish}
}

func (f *fakeChild) exited() bool {
	select {
	case <-f.done:
		return true
	default:
		return false
	}
}

type unitStep struct {
	err  error    // launch error; the process is not started
	gate gateFunc // nil passes immediately
}

// fakeUnit launches fake processes following steps; the last step repeats.
type fakeUnit struct {
	mu    sync.Mutex
	steps []unitStep
	calls int
	procs chan *fakeChild
}

func newFakeUnit(steps ...unitStep) *fakeUnit {
	if len(steps) == 0 {
		steps = []unitStep{{}}
	}
	return &fakeUnit{steps: steps, procs: make(chan *fakeChild, 32)}
}

func (f *fakeUnit) launch(context.Context) (*child, gateFunc, error) {
	f.mu.Lock()
	step := f.steps[min(f.calls, len(f.steps)-1)]
	f.calls++
	f.mu.Unlock()
	if step.err != nil {
		return nil, nil, step.err
	}
	p := newFakeChild()
	f.procs <- p
	gate := step.gate
	if gate == nil {
		gate = func(context.Context, *child) error { return nil }
	}
	return p.child, gate, nil
}

func (f *fakeUnit) launches() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeUnit) nextProc(t *testing.T) *fakeChild {
	t.Helper()
	select {
	case p := <-f.procs:
		return p
	case <-time.After(testWait):
		t.Fatal("unit was not launched")
		return nil
	}
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type fixture struct {
	s     *Supervisor
	rec   *hookRecorder
	clock *fakeClock

	mu     sync.Mutex
	delays []time.Duration
}

// newFixture wires fake units into a Supervisor with a fake clock and restart
// timers that fire immediately (recording the requested delay).
func newFixture(units ...*unit) *fixture {
	f := &fixture{rec: newHookRecorder(), clock: &fakeClock{now: time.Unix(1000, 0)}}
	f.s = newSupervisor(f.rec.hooks(), slog.New(slog.DiscardHandler))
	f.s.units = units
	f.s.now = f.clock.Now
	f.s.after = func(d time.Duration) <-chan time.Time {
		f.mu.Lock()
		f.delays = append(f.delays, d)
		f.mu.Unlock()
		ch := make(chan time.Time, 1)
		ch <- time.Time{}
		return ch
	}
	return f
}

func (f *fixture) restartDelays() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.delays)
}

// start runs the supervisor in the background; cancel ends it.
func (f *fixture) start(t *testing.T) (cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ch := make(chan error, 1)
	go func() { ch <- f.s.run(ctx) }()
	return cancel, ch
}

func waitRun(t *testing.T, done <-chan error, within time.Duration) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(within):
		t.Fatalf("Run did not return within %s", within)
		return nil
	}
}

func testUnit(name string, fatal, browser bool, f *fakeUnit) *unit {
	return newUnit(name, fatal, browser, time.Second, f.launch)
}

func unitState(u *unit) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.state
}

func TestRunReportsReadyOnlyAfterEveryGatePasses(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	browser := newFakeUnit(unitStep{gate: func(ctx context.Context, _ *child) error {
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}})
	f := newFixture(
		testUnit("a", false, false, newFakeUnit()),
		testUnit("b", false, false, newFakeUnit()),
		testUnit("browser", false, true, browser),
	)
	cancel, done := f.start(t)

	<-entered // a and b passed their gates before the browser unit was launched
	if f.s.Ready() || len(f.rec.ch) != 0 {
		t.Fatal("ready reported while the browser gate is still pending")
	}
	close(release)
	f.rec.expect(t, "ready")
	if !f.s.Ready() {
		t.Fatal("Ready() is false after the ready hook fired")
	}

	cancel()
	if err := waitRun(t, done, testWait); err != nil {
		t.Fatalf("clean shutdown returned %v", err)
	}
	if f.s.Ready() {
		t.Fatal("Ready() stays true after shutdown")
	}
}

func TestNonBrowserUnitDeathLocksThenRestartsToReady(t *testing.T) {
	vncUnit := newFakeUnit()
	f := newFixture(
		testUnit("vnc", false, false, vncUnit),
		testUnit("browser", false, true, newFakeUnit()),
	)
	cancel, done := f.start(t)
	f.rec.expect(t, "ready")

	vncUnit.nextProc(t).finish(errors.New("exit status 1"))
	// The lock fires and the browser generation is NOT rotated; the next
	// event is readiness after the restart.
	f.rec.expect(t, "not-ready:RUNTIME_NOT_READY", "ready")
	if got := vncUnit.launches(); got != 2 {
		t.Fatalf("launches = %d, want 2", got)
	}
	if got := f.restartDelays(); !slices.Equal(got, []time.Duration{backoffMin}) {
		t.Fatalf("restart delays = %v, want [%v]", got, backoffMin)
	}

	cancel()
	if err := waitRun(t, done, testWait); err != nil {
		t.Fatal(err)
	}
}

// The generation must rotate before the lock: a held lease then ends as a
// generation change (CDP close 4002), not as a plain lock (4001).
func TestBrowserExitRotatesGenerationBeforeLock(t *testing.T) {
	browser := newFakeUnit()
	f := newFixture(
		testUnit("a", false, false, newFakeUnit()),
		testUnit("browser", false, true, browser),
	)
	cancel, done := f.start(t)
	f.rec.expect(t, "ready")

	browser.nextProc(t).finish(errors.New("signal: killed"))
	f.rec.expect(t, "browser-restarted", "not-ready:RUNTIME_NOT_READY", "ready")

	cancel()
	if err := waitRun(t, done, testWait); err != nil {
		t.Fatal(err)
	}
}

func TestBlockedBrowserStaysNotReadyAndIsNotRetried(t *testing.T) {
	blocker := newFakeUnit(unitStep{err: &blockedError{
		code: "SANDBOX_PREFLIGHT_FAILED",
		err:  errors.New("userns unavailable"),
	}})
	browser := testUnit("browser", false, true, blocker)
	other := newFakeUnit()
	f := newFixture(testUnit("a", false, false, other), browser)
	cancel, done := f.start(t)

	f.rec.expect(t, "not-ready:SANDBOX_PREFLIGHT_FAILED")
	if f.s.Ready() {
		t.Fatal("Ready() is true while blocked")
	}
	if got := unitState(browser); got != stateBlocked {
		t.Fatalf("browser state = %q, want %q", got, stateBlocked)
	}
	if p := other.nextProc(t); p.exited() {
		t.Fatal("a blocked browser must leave display and VNC units running")
	}

	cancel()
	if err := waitRun(t, done, testWait); err != nil {
		t.Fatalf("shutdown while blocked returned %v", err)
	}
	if got := blocker.launches(); got != 1 {
		t.Fatalf("blocked unit launched %d times, want 1", got)
	}
	if got := f.restartDelays(); len(got) != 0 {
		t.Fatalf("blocked unit scheduled restarts: %v", got)
	}
}

func TestAbortedStartupStopsPromptlyWithoutWaitingForUnlaunchedUnits(t *testing.T) {
	first := newFakeUnit()
	failing := newFakeUnit(unitStep{err: errors.New("exec failed")})
	never1, never2 := newFakeUnit(), newFakeUnit()
	f := newFixture(
		testUnit("a", false, false, first),
		testUnit("b", false, false, failing),
		testUnit("c", false, false, never1),
		testUnit("d", false, true, never2),
	)
	_, done := f.start(t)

	// Each unit that never launched used to cost unitFinishWait (5 s).
	err := waitRun(t, done, unitFinishWait/2)
	if err == nil || !strings.Contains(err.Error(), "start b") {
		t.Fatalf("Run error = %v, want a start b failure", err)
	}
	if never1.launches() != 0 || never2.launches() != 0 {
		t.Fatal("units after the failed one were launched")
	}
	if !first.nextProc(t).exited() {
		t.Fatal("unit launched before the failure was not stopped")
	}
}

func TestShutdownRequestDuringStartupIsNotAnError(t *testing.T) {
	t.Run("while a gate is pending", func(t *testing.T) {
		entered := make(chan struct{})
		pending := newFakeUnit(unitStep{gate: func(ctx context.Context, _ *child) error {
			close(entered)
			<-ctx.Done()
			return fmt.Errorf("a not ready: %w", ctx.Err())
		}})
		f := newFixture(testUnit("a", true, false, pending), testUnit("b", false, false, newFakeUnit()))
		cancel, done := f.start(t)

		<-entered
		cancel()
		if err := waitRun(t, done, testWait); err != nil {
			t.Fatalf("SIGTERM during startup returned %v, want nil", err)
		}
		if !pending.nextProc(t).exited() {
			t.Fatal("pending unit was not stopped")
		}
	})

	t.Run("before any unit reports", func(t *testing.T) {
		// Run races the unit's context error against ctx.Done; both orders
		// must end cleanly.
		for i := range 50 {
			f := newFixture(
				testUnit("a", true, false, newFakeUnit()),
				testUnit("b", false, false, newFakeUnit()),
			)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := f.s.run(ctx); err != nil {
				t.Fatalf("iteration %d: run with a canceled context returned %v", i, err)
			}
		}
	})
}

func TestFatalUnitExitEndsRunWithError(t *testing.T) {
	xvfb := newFakeUnit()
	other := newFakeUnit()
	f := newFixture(testUnit("xvfb", true, false, xvfb), testUnit("b", false, false, other))
	_, done := f.start(t)
	f.rec.expect(t, "ready")

	xvfb.nextProc(t).finish(errors.New("exit status 1"))
	err := waitRun(t, done, testWait)
	if err == nil || !strings.Contains(err.Error(), "xvfb") {
		t.Fatalf("Run error = %v, want the xvfb failure", err)
	}
	if !other.nextProc(t).exited() {
		t.Fatal("remaining units were not stopped after a fatal exit")
	}
}

func TestLaunchFailuresGrowTheRestartDelay(t *testing.T) {
	boom := errors.New("exec failed")
	browser := newFakeUnit(unitStep{err: boom}, unitStep{err: boom}, unitStep{err: boom}, unitStep{err: boom}, unitStep{})
	f := newFixture(testUnit("browser", false, true, browser))
	cancel, done := f.start(t)

	// Failed launches never ran a browser a lease could bind to, so the
	// generation does not rotate; the retries must still back off.
	f.rec.expect(t, "ready")
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
	if got := f.restartDelays(); !slices.Equal(got, want) {
		t.Fatalf("restart delays = %v, want %v", got, want)
	}

	cancel()
	if err := waitRun(t, done, testWait); err != nil {
		t.Fatal(err)
	}
}

func TestStableRunResetsDelayButLaterLaunchFailuresStillGrow(t *testing.T) {
	boom := errors.New("exec failed")
	browser := newFakeUnit(unitStep{}, unitStep{err: boom}, unitStep{err: boom}, unitStep{})
	f := newFixture(testUnit("browser", false, true, browser))
	cancel, done := f.start(t)
	f.rec.expect(t, "ready")

	f.clock.Advance(2 * backoffStable)
	browser.nextProc(t).finish(errors.New("signal: killed"))
	f.rec.expect(t, "browser-restarted", "not-ready:RUNTIME_NOT_READY", "ready")
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	if got := f.restartDelays(); !slices.Equal(got, want) {
		t.Fatalf("restart delays = %v, want %v", got, want)
	}

	cancel()
	if err := waitRun(t, done, testWait); err != nil {
		t.Fatal(err)
	}
}
