package lease

import (
	"errors"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock is a deterministic Clock. Advance runs due callbacks outside its
// own lock, as the Clock contract requires.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	c       *fakeClock
	when    time.Time
	f       func()
	stopped bool
	fired   bool
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{c: c, when: c.now.Add(d), f: f}
	c.timers = append(c.timers, t)
	return t
}

func (t *fakeTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	was := !t.stopped && !t.fired
	t.stopped = true
	return was
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	target := c.now.Add(d)
	c.mu.Unlock()
	for {
		c.mu.Lock()
		var due []*fakeTimer
		for _, t := range c.timers {
			if !t.stopped && !t.fired && !t.when.After(target) {
				due = append(due, t)
			}
		}
		if len(due) == 0 {
			if target.After(c.now) {
				c.now = target
			}
			c.mu.Unlock()
			return
		}
		sort.Slice(due, func(i, j int) bool { return due[i].when.Before(due[j].when) })
		t := due[0]
		t.fired = true
		if t.when.After(c.now) {
			c.now = t.when
		}
		c.mu.Unlock()
		t.f()
	}
}

// AdvanceWithoutFiring moves time forward without running timers, simulating
// a timer that is late.
func (c *fakeClock) AdvanceWithoutFiring(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newReadyManager(t *testing.T) (*Manager, *fakeClock) {
	t.Helper()
	clk := newFakeClock()
	m, err := NewManager(Config{WorkspaceID: "default", Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	m.SetLocked(false, "")
	return m, clk
}

func mustAcquire(t *testing.T, m *Manager, mode Mode, ttl time.Duration, key string) (Lease, string) {
	t.Helper()
	l, tok, err := m.Acquire("operator", mode, ttl, key)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	return l, tok.Reveal()
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func TestNewManagerStartsLockedWithUUIDv4Generation(t *testing.T) {
	m, err := NewManager(Config{WorkspaceID: "default", Clock: newFakeClock()})
	if err != nil {
		t.Fatal(err)
	}
	st := m.Status()
	if !st.Locked || st.LockCode != "RUNTIME_NOT_READY" || st.State != StateErrorLocked || st.Lease != nil || st.HolderID != nil {
		t.Fatalf("initial status = %+v", st)
	}
	uuidV4 := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if !uuidV4.MatchString(st.Generation) {
		t.Fatalf("generation %q is not a UUIDv4", st.Generation)
	}
	if g2 := m.StartGeneration(); g2 == st.Generation || !uuidV4.MatchString(g2) {
		t.Fatalf("StartGeneration = %q, previous %q", g2, st.Generation)
	}
}

func TestNewManagerRejectsInvalidConfig(t *testing.T) {
	for name, cfg := range map[string]Config{
		"no workspace":     {},
		"default below":    {WorkspaceID: "w", MinTTL: 20 * time.Second, DefaultTTL: 10 * time.Second, MaxTTL: time.Minute},
		"default above":    {WorkspaceID: "w", MinTTL: time.Second, DefaultTTL: 2 * time.Minute, MaxTTL: time.Minute},
		"negative minimum": {WorkspaceID: "w", MinTTL: -time.Second},
	} {
		if _, err := NewManager(cfg); err == nil {
			t.Errorf("%s: NewManager succeeded", name)
		}
	}
}

func TestLockedRejectsAcquire(t *testing.T) {
	m, err := NewManager(Config{WorkspaceID: "default", Clock: newFakeClock()})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = m.Acquire("operator", ModeAgent, 0, "k")
	var locked *LockedError
	if !errors.As(err, &locked) || locked.Code != "RUNTIME_NOT_READY" {
		t.Fatalf("err = %v, want *LockedError RUNTIME_NOT_READY", err)
	}

	m.SetLocked(true, "SANDBOX_PREFLIGHT_FAILED")
	_, _, err = m.Acquire("operator", ModeAgent, 0, "k")
	if !errors.As(err, &locked) || locked.Code != "SANDBOX_PREFLIGHT_FAILED" {
		t.Fatalf("err = %v, want *LockedError SANDBOX_PREFLIGHT_FAILED", err)
	}

	m.SetLocked(false, "")
	if _, _, err := m.Acquire("operator", ModeAgent, 0, "k"); err != nil {
		t.Fatalf("after unlock: %v", err)
	}
	if st := m.Status(); st.Locked || st.LockCode != "" || st.State != StateAgent {
		t.Fatalf("status after unlock+acquire = %+v", st)
	}
}

func TestLockingEndsHeldLeaseWithEndLocked(t *testing.T) {
	m, _ := newReadyManager(t)
	l, tok := mustAcquire(t, m, ModeAgent, 0, "k")
	tk1, err := m.Authorize(tok, ModeAgent)
	if err != nil {
		t.Fatal(err)
	}
	tk2, err := m.Authorize(tok, ModeAgent)
	if err != nil {
		t.Fatal(err)
	}

	m.SetLocked(true, "SANDBOX_PREFLIGHT_FAILED")
	for i, tk := range []Ticket{tk1, tk2} {
		if !isClosed(tk.Done()) || tk.Reason() != EndLocked {
			t.Fatalf("ticket %d after lock: closed=%v reason=%q, want closed with %q", i, isClosed(tk.Done()), tk.Reason(), EndLocked)
		}
	}
	if st := m.Status(); st.State != StateErrorLocked || st.Lease != nil || st.HolderID != nil || st.LockCode != "SANDBOX_PREFLIGHT_FAILED" {
		t.Fatalf("status = %+v, want ERROR_LOCKED without a lease", st)
	}

	var locked *LockedError
	if _, err := m.Renew(tok, l.ID, l.Epoch, l.Generation); !errors.As(err, &locked) || locked.Code != "SANDBOX_PREFLIGHT_FAILED" {
		t.Fatalf("Renew while locked: err = %v, want *LockedError SANDBOX_PREFLIGHT_FAILED", err)
	}
	var ended *EndedError
	if _, err := m.Authorize(tok, ModeAgent); !errors.As(err, &ended) || ended.Reason != EndLocked {
		t.Fatalf("Authorize with locked-out token: err = %v, want EndedError(LOCKED)", err)
	}
	if _, err := m.Identify(tok); !errors.As(err, &ended) || ended.Reason != EndLocked {
		t.Fatalf("Identify with locked-out token: err = %v, want EndedError(LOCKED)", err)
	}
	if err := m.Release(l.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Release of locked-out lease: err = %v, want ErrNotFound", err)
	}
	if _, _, err := m.Acquire("operator", ModeAgent, 0, "k2"); !errors.As(err, &locked) {
		t.Fatalf("Acquire while locked: err = %v, want *LockedError", err)
	}

	// Unlocking never revives the old lease; a new one gets a new epoch.
	m.SetLocked(false, "")
	if _, err := m.Authorize(tok, ModeAgent); !errors.As(err, &ended) || ended.Reason != EndLocked {
		t.Fatalf("old token after unlock: err = %v, want EndedError(LOCKED)", err)
	}
	l2, tok2 := mustAcquire(t, m, ModeAgent, 0, "k3")
	if l2.Epoch <= l.Epoch {
		t.Fatalf("epoch after relock = %d, previous %d", l2.Epoch, l.Epoch)
	}
	if _, err := m.Renew(tok2, l2.ID, l2.Epoch, l2.Generation); err != nil {
		t.Fatalf("Renew of the new lease: %v", err)
	}
	if _, err := m.Renew(tok, l.ID, l.Epoch, l.Generation); !errors.As(err, &ended) || ended.Reason != EndLocked {
		t.Fatalf("Renew with old token after unlock: err = %v, want EndedError(LOCKED)", err)
	}
}

func TestSingleLeaseConflict(t *testing.T) {
	m, _ := newReadyManager(t)
	mustAcquire(t, m, ModeAgent, 0, "k1")
	if _, _, err := m.Acquire("other", ModeHuman, 0, "k2"); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if _, _, err := m.Acquire("other", ModeAgent, 0, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
}

func TestAcquireValidation(t *testing.T) {
	m, _ := newReadyManager(t)
	tests := []struct {
		name   string
		holder string
		mode   Mode
		ttl    time.Duration
		want   error
	}{
		{"ttl below minimum", "h", ModeAgent, 9 * time.Second, ErrInvalidTTL},
		{"ttl above maximum", "h", ModeAgent, 121 * time.Second, ErrInvalidTTL},
		{"negative ttl", "h", ModeAgent, -time.Second, ErrInvalidTTL},
		{"unknown mode", "h", Mode("ROOT"), 30 * time.Second, ErrInvalidArgument},
		{"empty holder", "", ModeAgent, 30 * time.Second, ErrInvalidArgument},
		{"oversize holder", strings.Repeat("h", 129), ModeAgent, 30 * time.Second, ErrInvalidArgument},
	}
	for _, tt := range tests {
		if _, _, err := m.Acquire(tt.holder, tt.mode, tt.ttl, ""); !errors.Is(err, tt.want) {
			t.Errorf("%s: err = %v, want %v", tt.name, err, tt.want)
		}
	}
	if m.Status().Lease != nil {
		t.Fatal("a rejected acquire left a lease behind")
	}
	// Boundaries are inclusive.
	for _, ttl := range []time.Duration{10 * time.Second, 120 * time.Second} {
		l, _ := mustAcquire(t, m, ModeAgent, ttl, "")
		if err := m.Release(l.ID); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAcquireGrantsLeaseWithDefaultTTL(t *testing.T) {
	m, clk := newReadyManager(t)
	l, tok := mustAcquire(t, m, ModeHuman, 0, "")
	if !strings.HasPrefix(tok, "ldl_") {
		t.Fatalf("token %q lacks ldl_ prefix", tok)
	}
	if want := clk.Now().Add(30 * time.Second); !l.ExpiresAt.Equal(want) {
		t.Fatalf("ExpiresAt = %v, want %v", l.ExpiresAt, want)
	}
	if l.WorkspaceID != "default" || l.HolderID != "operator" || l.Mode != ModeHuman || l.Epoch != 1 || l.Generation != m.Status().Generation {
		t.Fatalf("lease = %+v", l)
	}
	st := m.Status()
	if st.State != StateHuman || st.HolderID == nil || *st.HolderID != "operator" || st.Lease == nil || *st.Lease != l {
		t.Fatalf("status = %+v", st)
	}
}

func TestIdempotencyReplay(t *testing.T) {
	m, _ := newReadyManager(t)
	l, _ := mustAcquire(t, m, ModeAgent, 0, "same-key")

	_, tok, err := m.Acquire("operator", ModeAgent, 0, "same-key")
	if !errors.Is(err, ErrIdempotencyReplay) {
		t.Fatalf("err = %v, want ErrIdempotencyReplay", err)
	}
	if tok != "" {
		t.Fatal("replay re-issued a token")
	}
	if _, _, err := m.Acquire("operator", ModeAgent, 0, "other-key"); !errors.Is(err, ErrConflict) {
		t.Fatalf("different key: err = %v, want ErrConflict", err)
	}

	// The key is only remembered while its lease is active.
	if err := m.Release(l.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.Acquire("operator", ModeAgent, 0, "same-key"); err != nil {
		t.Fatalf("key reuse after release: %v", err)
	}
}

func TestEpochIsStrictlyIncreasingAcrossReleaseExpiryAndGeneration(t *testing.T) {
	m, clk := newReadyManager(t)
	var last uint64
	check := func(l Lease) {
		t.Helper()
		if l.Epoch <= last {
			t.Fatalf("epoch %d not greater than %d", l.Epoch, last)
		}
		last = l.Epoch
	}

	l, _ := mustAcquire(t, m, ModeAgent, 0, "")
	check(l)
	if err := m.Release(l.ID); err != nil {
		t.Fatal(err)
	}
	l, _ = mustAcquire(t, m, ModeHuman, 10*time.Second, "")
	check(l)
	clk.Advance(10 * time.Second)
	l, _ = mustAcquire(t, m, ModeAgent, 0, "")
	check(l)
	m.StartGeneration()
	l, _ = mustAcquire(t, m, ModeAgent, 0, "")
	check(l)
}

func TestTTLExpiryClosesTicketsWithExpiredReason(t *testing.T) {
	m, clk := newReadyManager(t)
	l, tok := mustAcquire(t, m, ModeAgent, 30*time.Second, "")
	t1, err := m.Authorize(tok, ModeAgent)
	if err != nil {
		t.Fatal(err)
	}
	t2, err := m.Authorize(tok, ModeAgent)
	if err != nil {
		t.Fatal(err)
	}
	if t1.Lease() != l || t1.Reason() != "" {
		t.Fatalf("ticket before end: lease %+v reason %q", t1.Lease(), t1.Reason())
	}

	clk.Advance(29*time.Second + 999*time.Millisecond)
	if isClosed(t1.Done()) {
		t.Fatal("ticket closed before the TTL elapsed")
	}
	clk.Advance(time.Millisecond)
	for i, tk := range []Ticket{t1, t2} {
		if !isClosed(tk.Done()) {
			t.Fatalf("ticket %d not closed at TTL", i)
		}
		if tk.Reason() != EndExpired {
			t.Fatalf("ticket %d reason = %q, want %q", i, tk.Reason(), EndExpired)
		}
	}
	if st := m.Status(); st.Lease != nil || st.State != StateIdle {
		t.Fatalf("status after expiry = %+v", st)
	}
	var ended *EndedError
	if _, err := m.Authorize(tok, ModeAgent); !errors.As(err, &ended) || ended.Reason != EndExpired {
		t.Fatalf("Authorize after expiry: err = %v, want EndedError(EXPIRED)", err)
	}
	if _, _, err := m.Acquire("operator", ModeAgent, 0, ""); err != nil {
		t.Fatalf("Acquire after expiry: %v", err)
	}
}

func TestExpiryIsDetectedEvenIfTimerIsLate(t *testing.T) {
	m, clk := newReadyManager(t)
	_, tok := mustAcquire(t, m, ModeAgent, 10*time.Second, "")
	clk.AdvanceWithoutFiring(10 * time.Second)

	var ended *EndedError
	if _, err := m.Authorize(tok, ModeAgent); !errors.As(err, &ended) || ended.Reason != EndExpired {
		t.Fatalf("err = %v, want EndedError(EXPIRED)", err)
	}
	if _, _, err := m.Acquire("operator", ModeAgent, 0, ""); err != nil {
		t.Fatalf("Acquire after lazy expiry: %v", err)
	}
}

func TestRenewExtendsDeadlineByOriginalTTL(t *testing.T) {
	m, clk := newReadyManager(t)
	l, tok := mustAcquire(t, m, ModeAgent, 20*time.Second, "")
	tk, err := m.Authorize(tok, ModeAgent)
	if err != nil {
		t.Fatal(err)
	}

	clk.Advance(15 * time.Second)
	renewed, err := m.Renew(tok, l.ID, l.Epoch, l.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if want := clk.Now().Add(20 * time.Second); !renewed.ExpiresAt.Equal(want) {
		t.Fatalf("ExpiresAt = %v, want %v", renewed.ExpiresAt, want)
	}
	if renewed.Epoch != l.Epoch || renewed.ID != l.ID {
		t.Fatalf("renew changed identity: %+v -> %+v", l, renewed)
	}

	// The original deadline passes without ending the lease.
	clk.Advance(10 * time.Second)
	if isClosed(tk.Done()) {
		t.Fatal("stale timer ended a renewed lease")
	}
	clk.Advance(10 * time.Second)
	if !isClosed(tk.Done()) || tk.Reason() != EndExpired {
		t.Fatalf("lease still active after the renewed TTL (reason %q)", tk.Reason())
	}
}

func TestRenewRejectsMismatches(t *testing.T) {
	m, _ := newReadyManager(t)
	l, tok := mustAcquire(t, m, ModeAgent, 0, "")

	tests := []struct {
		name    string
		token   string
		leaseID string
		epoch   uint64
		gen     string
		want    error
	}{
		{"epoch mismatch", tok, l.ID, l.Epoch + 1, l.Generation, ErrEpochMismatch},
		{"epoch zero", tok, l.ID, 0, l.Generation, ErrEpochMismatch},
		{"generation mismatch", tok, l.ID, l.Epoch, "00000000-0000-4000-8000-000000000000", ErrGenerationMismatch},
		{"empty generation", tok, l.ID, l.Epoch, "", ErrGenerationMismatch},
		{"other lease id", tok, "lse_other", l.Epoch, l.Generation, ErrNotFound},
		{"unknown token", "ldl_unknown", l.ID, l.Epoch, l.Generation, ErrUnauthenticated},
	}
	for _, tt := range tests {
		if _, err := m.Renew(tt.token, tt.leaseID, tt.epoch, tt.gen); !errors.Is(err, tt.want) {
			t.Errorf("%s: err = %v, want %v", tt.name, err, tt.want)
		}
	}
	// None of the failures ended or altered the lease.
	if cur, err := m.Identify(tok); err != nil || cur != l {
		t.Fatalf("lease after failed renewals = %+v, %v", cur, err)
	}
}

func TestStartGenerationEndsLeaseWithEndGeneration(t *testing.T) {
	m, _ := newReadyManager(t)
	l, tok := mustAcquire(t, m, ModeAgent, 0, "")
	tk, err := m.Authorize(tok, ModeAgent)
	if err != nil {
		t.Fatal(err)
	}
	old := m.Status().Generation

	gen := m.StartGeneration()
	if gen == old || m.Status().Generation != gen {
		t.Fatalf("generation = %q (status %q), old %q", gen, m.Status().Generation, old)
	}
	if !isClosed(tk.Done()) || tk.Reason() != EndGeneration {
		t.Fatalf("ticket after generation change: closed=%v reason=%q", isClosed(tk.Done()), tk.Reason())
	}
	var ended *EndedError
	if _, err := m.Renew(tok, l.ID, l.Epoch, l.Generation); !errors.As(err, &ended) || ended.Reason != EndGeneration {
		t.Fatalf("Renew with old token: err = %v, want EndedError(GENERATION)", err)
	}
	// With no lease active StartGeneration still rotates the generation.
	if g3 := m.StartGeneration(); g3 == gen {
		t.Fatal("generation did not rotate without an active lease")
	}
}

func TestLeaseAcquiredBeforeGenerationChangeCannotRenewAgainstNewGeneration(t *testing.T) {
	m, _ := newReadyManager(t)
	l, _ := mustAcquire(t, m, ModeAgent, 0, "")
	newGen := m.StartGeneration()
	l2, tok2 := mustAcquire(t, m, ModeAgent, 0, "")
	if l2.Generation != newGen {
		t.Fatalf("new lease generation = %q, want %q", l2.Generation, newGen)
	}
	if _, err := m.Renew(tok2, l2.ID, l2.Epoch, l.Generation); !errors.Is(err, ErrGenerationMismatch) {
		t.Fatalf("err = %v, want ErrGenerationMismatch", err)
	}
}

func TestEndedTokenReturnsEndedError(t *testing.T) {
	m, _ := newReadyManager(t)
	l, tok := mustAcquire(t, m, ModeAgent, 0, "")
	if err := m.Release(l.ID); err != nil {
		t.Fatal(err)
	}

	check := func(op string, err error) {
		t.Helper()
		var ended *EndedError
		if !errors.As(err, &ended) || ended.Reason != EndReleased {
			t.Errorf("%s: err = %v, want EndedError(RELEASED)", op, err)
		}
		if errors.Is(err, ErrUnauthenticated) {
			t.Errorf("%s: ended token reported as unauthenticated", op)
		}
	}
	_, err := m.Authorize(tok, ModeAgent)
	check("Authorize", err)
	_, err = m.Identify(tok)
	check("Identify", err)
	_, err = m.Renew(tok, l.ID, l.Epoch, l.Generation)
	check("Renew", err)

	if err := m.Release(l.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second Release: err = %v, want ErrNotFound", err)
	}
	if _, err := m.Authorize("ldl_never_issued", ModeAgent); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("unknown token: err = %v, want ErrUnauthenticated", err)
	}
}

func TestEndedTokenRingKeepsLastSixteen(t *testing.T) {
	m, _ := newReadyManager(t)
	var tokens []string
	for range endedRingSize + 1 {
		l, tok := mustAcquire(t, m, ModeAgent, 0, "")
		tokens = append(tokens, tok)
		if err := m.Release(l.ID); err != nil {
			t.Fatal(err)
		}
	}
	var ended *EndedError
	if _, err := m.Identify(tokens[0]); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("oldest token fell out of the ring: err = %v, want ErrUnauthenticated", err)
	}
	for i, tok := range tokens[1:] {
		if _, err := m.Identify(tok); !errors.As(err, &ended) {
			t.Errorf("token %d: err = %v, want EndedError", i+1, err)
		}
	}
}

func TestAuthorizeEnforcesModeAndIdentity(t *testing.T) {
	m, _ := newReadyManager(t)
	_, tok := mustAcquire(t, m, ModeHuman, 0, "")
	if _, err := m.Authorize(tok, ModeAgent); !errors.Is(err, ErrWrongMode) {
		t.Fatalf("err = %v, want ErrWrongMode", err)
	}
	if _, err := m.Authorize("", ModeHuman); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("empty token: err = %v, want ErrUnauthenticated", err)
	}
	tk, err := m.Authorize(tok, ModeHuman)
	if err != nil || tk.Lease().Mode != ModeHuman {
		t.Fatalf("Authorize = %+v, %v", tk.Lease(), err)
	}
}

func TestReleaseEndsTicketsAndRejectsUnknownLease(t *testing.T) {
	m, _ := newReadyManager(t)
	l, tok := mustAcquire(t, m, ModeAgent, 0, "")
	tk, err := m.Authorize(tok, ModeAgent)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Release("lse_other"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if isClosed(tk.Done()) {
		t.Fatal("releasing an unknown lease ended the active one")
	}
	if err := m.Release(l.ID); err != nil {
		t.Fatal(err)
	}
	if !isClosed(tk.Done()) || tk.Reason() != EndReleased {
		t.Fatalf("closed=%v reason=%q", isClosed(tk.Done()), tk.Reason())
	}
}

func TestShutdown(t *testing.T) {
	m, _ := newReadyManager(t)
	l, tok := mustAcquire(t, m, ModeAgent, 0, "")
	tk, err := m.Authorize(tok, ModeAgent)
	if err != nil {
		t.Fatal(err)
	}
	gen := m.Status().Generation

	m.Shutdown()
	m.Shutdown() // idempotent

	if !isClosed(tk.Done()) || tk.Reason() != EndShutdown {
		t.Fatalf("closed=%v reason=%q", isClosed(tk.Done()), tk.Reason())
	}
	if _, _, err := m.Acquire("operator", ModeAgent, 0, ""); !errors.Is(err, ErrShutdown) {
		t.Errorf("Acquire: err = %v, want ErrShutdown", err)
	}
	if _, err := m.Renew(tok, l.ID, l.Epoch, l.Generation); !errors.Is(err, ErrShutdown) {
		t.Errorf("Renew: err = %v, want ErrShutdown", err)
	}
	if _, err := m.Authorize(tok, ModeAgent); !errors.Is(err, ErrShutdown) {
		t.Errorf("Authorize: err = %v, want ErrShutdown", err)
	}
	if _, err := m.Identify(tok); !errors.Is(err, ErrShutdown) {
		t.Errorf("Identify: err = %v, want ErrShutdown", err)
	}
	if err := m.Release(l.ID); !errors.Is(err, ErrShutdown) {
		t.Errorf("Release: err = %v, want ErrShutdown", err)
	}
	if got := m.StartGeneration(); got != gen {
		t.Errorf("StartGeneration after shutdown changed generation %q -> %q", gen, got)
	}
}

func TestStatusIdleAfterShutdownDoesNotPanic(t *testing.T) {
	m, _ := newReadyManager(t)
	m.Shutdown()
	if st := m.Status(); st.Lease != nil {
		t.Fatalf("status = %+v", st)
	}
}

// TestConcurrentLifecycleClosesEachLeaseOnce hammers every state transition
// from many goroutines. A double close would panic; -race checks the rest.
func TestConcurrentLifecycleClosesEachLeaseOnce(t *testing.T) {
	m, clk := newReadyManager(t)
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 200 {
				l, tok, err := m.Acquire("operator", ModeAgent, 10*time.Second, "")
				if err == nil {
					tk, aerr := m.Authorize(tok.Reveal(), ModeAgent)
					if aerr == nil && tk.Lease().ID != l.ID {
						t.Errorf("ticket for wrong lease")
					}
					_, _ = m.Renew(tok.Reveal(), l.ID, l.Epoch, l.Generation)
					if i%3 == 0 {
						_ = m.Release(l.ID)
					}
				}
				switch (g + i) % 5 {
				case 0:
					m.StartGeneration()
				case 1:
					clk.Advance(4 * time.Second)
				case 2:
					_ = m.Status()
				}
			}
		}()
	}
	wg.Wait()
	m.Shutdown()
}
