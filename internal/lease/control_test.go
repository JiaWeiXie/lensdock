package lease

import (
	"context"
	"errors"
	"testing"
	"time"
)

// activeTimers counts timers that have neither fired nor been stopped.
func (c *fakeClock) activeTimers() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, t := range c.timers {
		if !t.stopped && !t.fired {
			n++
		}
	}
	return n
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func handoffStatus(m *Manager) HandoffStatus {
	h, ok := m.Handoff()
	if !ok {
		return ""
	}
	return h.Status
}

func TestTakeoverWithoutLeaseIsReadyAndClaimedOnlyByItsRequestID(t *testing.T) {
	m, _ := newReadyManager(t)
	h, err := m.BeginHandoff("k1", false)
	if err != nil {
		t.Fatal(err)
	}
	if h.Status != HandoffReady || !h.Drained || h.Prior != nil {
		t.Fatalf("handoff = %+v", h)
	}
	if st := m.Status(); st.State != StateHandoffPending || st.Handoff == nil || st.HolderID != nil {
		t.Fatalf("status = %+v", st)
	}
	if _, _, err := m.Acquire("someone", ModeAgent, 0, "a"); !errors.Is(err, ErrConflict) {
		t.Fatalf("plain acquire during takeover = %v, want ErrConflict", err)
	}
	if _, _, err := m.Acquire("someone", ModeHuman, 0, "a"); !errors.Is(err, ErrConflict) {
		t.Fatalf("plain HUMAN acquire during takeover = %v, want ErrConflict", err)
	}
	if _, _, err := m.AcquireTakeover("human", 0, "b", "tko_wrong"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong takeover id = %v, want ErrNotFound", err)
	}
	if again, err := m.BeginHandoff("k1", false); err != nil || again.RequestID != h.RequestID {
		t.Fatalf("idempotent replay = %+v, %v", again, err)
	}
	if _, err := m.BeginHandoff("k2", false); !errors.Is(err, ErrConflict) {
		t.Fatalf("second takeover = %v, want ErrConflict", err)
	}
	l, _, err := m.AcquireTakeover("human", 0, "b", h.RequestID)
	if err != nil || l.Mode != ModeHuman || l.HolderID != "human" {
		t.Fatalf("AcquireTakeover = %+v, %v", l, err)
	}
	if _, ok := m.Handoff(); ok {
		t.Fatal("takeover must end once the HUMAN lease is granted")
	}
	if _, _, err := m.AcquireTakeover("human", 0, "c", h.RequestID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("takeover id reuse = %v, want ErrNotFound", err)
	}
}

func TestTakeoverFencesBeforeAnyGrantAndWaitsForDrain(t *testing.T) {
	m, _ := newReadyManager(t)
	old, tok := mustAcquire(t, m, ModeAgent, 0, "agent")
	ticket, release, err := m.AuthorizeTracked(tok, ModeAgent)
	if err != nil {
		t.Fatal(err)
	}

	h, err := m.BeginHandoff("t", false)
	if err != nil {
		t.Fatal(err)
	}
	if h.Status != HandoffDraining || h.Drained || !h.UncertainEffects || h.Prior == nil || h.Prior.LeaseID != old.ID {
		t.Fatalf("handoff = %+v", h)
	}
	// The old writer is fenced the moment the takeover starts.
	if !isClosed(ticket.Done()) || ticket.Reason() != EndTakeover {
		t.Fatalf("ticket done=%v reason=%q, want closed with TAKEOVER", isClosed(ticket.Done()), ticket.Reason())
	}
	if _, err := m.Authorize(tok, ModeAgent); err == nil {
		t.Fatal("old token must not authorize during the takeover")
	}
	var ended *EndedError
	if _, err := m.Renew(tok, old.ID, old.Epoch, old.Generation); !errors.As(err, &ended) || ended.Reason != EndTakeover {
		t.Fatalf("renew of fenced lease = %v, want EndedError(TAKEOVER)", err)
	}
	// No HUMAN grant while the old connection is still draining.
	if _, _, err := m.AcquireTakeover("human", 0, "h", h.RequestID); !errors.Is(err, ErrConflict) {
		t.Fatalf("AcquireTakeover while draining = %v, want ErrConflict", err)
	}

	release()
	eventually(t, "takeover READY", func() bool { return handoffStatus(m) == HandoffReady })
	if h, _ := m.Handoff(); !h.Drained || h.Forced {
		t.Fatalf("handoff after drain = %+v", h)
	}
	l, _, err := m.AcquireTakeover("human", 0, "h", h.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if l.Epoch <= old.Epoch {
		t.Fatalf("new epoch %d must exceed old %d", l.Epoch, old.Epoch)
	}
	if _, err := m.Authorize(tok, ModeAgent); err == nil {
		t.Fatal("old agent token authorized after the HUMAN grant")
	}
}

func TestTakeoverDrainTimeoutNeedsForce(t *testing.T) {
	m, clk := newReadyManager(t)
	_, tok := mustAcquire(t, m, ModeAgent, 0, "agent")
	_, release, err := m.AuthorizeTracked(tok, ModeAgent)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	h, err := m.BeginHandoff("t", false)
	if err != nil || h.Status != HandoffDraining {
		t.Fatalf("BeginHandoff = %+v, %v", h, err)
	}
	// handoff expiry timer + drain timer
	eventually(t, "drain timer", func() bool { return clk.activeTimers() == 2 })
	clk.Advance(DefaultDrainTimeout)
	eventually(t, "DRAIN_TIMEOUT", func() bool { return handoffStatus(m) == HandoffDrainTimeout })
	if _, _, err := m.AcquireTakeover("human", 0, "h", h.RequestID); !errors.Is(err, ErrConflict) {
		t.Fatalf("AcquireTakeover after timeout = %v, want ErrConflict", err)
	}
	forced, err := m.BeginHandoff("t2", true)
	if err != nil || forced.Status != HandoffReady || !forced.Forced || forced.Drained {
		t.Fatalf("forced handoff = %+v, %v", forced, err)
	}
	if forced.RequestID != h.RequestID {
		t.Fatal("force must keep the request id")
	}
	if _, _, err := m.AcquireTakeover("human", 0, "h", h.RequestID); err != nil {
		t.Fatalf("AcquireTakeover after force: %v", err)
	}
}

func TestTakeoverWithForceUpFrontAutoForcesAfterDrainLimit(t *testing.T) {
	m, clk := newReadyManager(t)
	_, tok := mustAcquire(t, m, ModeAgent, 0, "agent")
	_, release, _ := m.AuthorizeTracked(tok, ModeAgent)
	defer release()
	if _, err := m.BeginHandoff("t", true); err != nil {
		t.Fatal(err)
	}
	eventually(t, "drain timer", func() bool { return clk.activeTimers() == 2 })
	clk.Advance(DefaultDrainTimeout)
	eventually(t, "forced READY", func() bool { return handoffStatus(m) == HandoffReady })
	if h, _ := m.Handoff(); !h.Forced || h.Drained {
		t.Fatalf("handoff = %+v", h)
	}
}

func TestTakeoverReservationExpiresAndCancelDoesNotRestore(t *testing.T) {
	m, clk := newReadyManager(t)
	_, tok := mustAcquire(t, m, ModeHuman, 0, "h")
	if _, err := m.BeginHandoff("t", false); err != nil {
		t.Fatal(err)
	}
	clk.Advance(DefaultHandoffTTL)
	if st := m.Status(); st.State != StateIdle || st.Handoff != nil {
		t.Fatalf("status after reservation expiry = %+v", st)
	}
	if _, err := m.Authorize(tok, ModeHuman); err == nil {
		t.Fatal("fenced lease must stay dead after the reservation expired")
	}
	if _, err := m.BeginHandoff("t2", false); err != nil {
		t.Fatal(err)
	}
	if err := m.CancelHandoff(); err != nil {
		t.Fatal(err)
	}
	if err := m.CancelHandoff(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second cancel = %v, want ErrNotFound", err)
	}
	if st := m.Status(); st.State != StateIdle {
		t.Fatalf("state after cancel = %s", st.State)
	}
}

func TestGenerationAndLockDropTakeoverReservation(t *testing.T) {
	m, _ := newReadyManager(t)
	if _, err := m.BeginHandoff("t", false); err != nil {
		t.Fatal(err)
	}
	m.StartGeneration()
	if _, ok := m.Handoff(); ok {
		t.Fatal("generation change must drop the takeover")
	}
	if _, err := m.BeginHandoff("t", false); err != nil {
		t.Fatal(err)
	}
	m.SetLocked(true, "")
	if _, ok := m.Handoff(); ok {
		t.Fatal("lock must drop the takeover")
	}
}

func TestCaptureRestoresPriorHolderAtNewEpoch(t *testing.T) {
	m, _ := newReadyManager(t)
	old, tok := mustAcquire(t, m, ModeAgent, 0, "agent")
	ticket, release, err := m.AuthorizeTracked(tok, ModeAgent)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := m.BeginCapture("", false); !errors.Is(err, ErrInterruptNotAllowed) {
		t.Fatalf("BeginCapture without allowInterrupt = %v", err)
	}
	if _, err := m.BeginCapture("lse_other", true); !errors.Is(err, ErrLeaseMismatch) {
		t.Fatalf("BeginCapture with wrong expected lease = %v", err)
	}
	sess, err := m.BeginCapture(old.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Prior == nil || sess.Prior.LeaseID != old.ID {
		t.Fatalf("prior = %+v", sess.Prior)
	}
	if !isClosed(ticket.Done()) || ticket.Reason() != EndCapture {
		t.Fatal("capture must close the holder's tickets")
	}
	if st := m.Status(); st.State != StateCapture || !st.Capture {
		t.Fatalf("status = %+v", st)
	}
	var ended *EndedError
	if _, err := m.Authorize(tok, ModeAgent); !errors.As(err, &ended) || ended.Reason != EndCapture {
		t.Fatalf("holder during capture = %v, want EndedError(CAPTURE)", err)
	}
	if _, _, err := m.Acquire("x", ModeHuman, 0, "x"); !errors.Is(err, ErrConflict) {
		t.Fatalf("acquire during capture = %v, want ErrConflict", err)
	}
	if _, err := m.BeginCapture("", true); !errors.Is(err, ErrConflict) {
		t.Fatalf("second capture = %v, want ErrConflict", err)
	}

	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	if sess.WaitDrained(short) {
		t.Fatal("WaitDrained must not report drained while a connection is tracked")
	}
	cancel()
	release()
	if !sess.WaitDrained(context.Background()) {
		t.Fatal("WaitDrained after release = false")
	}

	out := sess.End()
	if out.Restored == nil || out.State != StateAgent {
		t.Fatalf("outcome = %+v", out)
	}
	r := *out.Restored
	if r.Epoch <= old.Epoch || r.ID == old.ID || r.HolderID != old.HolderID || r.Mode != old.Mode || r.Generation != old.Generation {
		t.Fatalf("restored %+v vs old %+v", r, old)
	}
	// The same capability token works, but only at the new epoch.
	got, err := m.Authorize(tok, ModeAgent)
	if err != nil || got.Lease().Epoch != r.Epoch {
		t.Fatalf("Authorize after restore = %+v, %v", got.Lease(), err)
	}
	if _, err := m.Renew(tok, old.ID, old.Epoch, old.Generation); !errors.Is(err, ErrNotFound) {
		t.Fatalf("renew with the old lease id = %v, want ErrNotFound", err)
	}
	if _, err := m.Renew(tok, r.ID, old.Epoch, r.Generation); !errors.Is(err, ErrEpochMismatch) {
		t.Fatalf("renew with the old epoch = %v, want ErrEpochMismatch", err)
	}
	if _, err := m.Renew(tok, r.ID, r.Epoch, r.Generation); err != nil {
		t.Fatalf("renew with the new epoch: %v", err)
	}
	if again := sess.End(); again.Restored == nil || again.Restored.ID != r.ID {
		t.Fatalf("End must be idempotent, got %+v", again)
	}
}

func TestCaptureDoesNotRestoreReleasedExpiredOrStaleLeases(t *testing.T) {
	t.Run("released during capture", func(t *testing.T) {
		m, _ := newReadyManager(t)
		old, tok := mustAcquire(t, m, ModeHuman, 0, "h")
		sess, err := m.BeginCapture(old.ID, true)
		if err != nil {
			t.Fatal(err)
		}
		if err := m.Relinquish("ldl_wrong", old.ID); err == nil {
			t.Fatal("Relinquish with a wrong token succeeded")
		}
		if err := m.Relinquish(tok, old.ID); err != nil {
			t.Fatalf("Relinquish of the fenced lease: %v", err)
		}
		if out := sess.End(); out.Restored != nil || out.State != StateIdle {
			t.Fatalf("outcome = %+v, want IDLE without restore", out)
		}
	})
	t.Run("released by operator id during capture", func(t *testing.T) {
		m, _ := newReadyManager(t)
		old, _ := mustAcquire(t, m, ModeHuman, 0, "h")
		sess, _ := m.BeginCapture("", true)
		if err := m.Release(old.ID); err != nil {
			t.Fatal(err)
		}
		if out := sess.End(); out.Restored != nil || out.State != StateIdle {
			t.Fatalf("outcome = %+v", out)
		}
	})
	t.Run("ttl expired during capture", func(t *testing.T) {
		m, clk := newReadyManager(t)
		old, _ := mustAcquire(t, m, ModeAgent, MinTTL, "a")
		sess, _ := m.BeginCapture(old.ID, true)
		clk.Advance(MinTTL + time.Second)
		out := sess.End()
		if out.Restored != nil || out.State != StateIdle || out.Prior == nil {
			t.Fatalf("outcome = %+v, want IDLE without restore", out)
		}
		if st := m.Status(); st.Lease != nil {
			t.Fatal("no lease may be issued after an expired prior lease")
		}
	})
	t.Run("generation changed", func(t *testing.T) {
		m, _ := newReadyManager(t)
		old, _ := mustAcquire(t, m, ModeAgent, 0, "a")
		sess, _ := m.BeginCapture(old.ID, true)
		m.StartGeneration()
		if out := sess.End(); out.Restored != nil {
			t.Fatalf("restored across a generation change: %+v", out)
		}
	})
	t.Run("locked", func(t *testing.T) {
		m, _ := newReadyManager(t)
		old, _ := mustAcquire(t, m, ModeAgent, 0, "a")
		sess, _ := m.BeginCapture(old.ID, true)
		m.SetLocked(true, "")
		if out := sess.End(); out.Restored != nil || out.State != StateErrorLocked {
			t.Fatalf("outcome = %+v", out)
		}
	})
}

func TestCaptureWithoutLeaseAndSafetyTimeout(t *testing.T) {
	m, clk := newReadyManager(t)
	if _, err := m.BeginCapture("lse_none", true); !errors.Is(err, ErrLeaseMismatch) {
		t.Fatalf("expected lease with none held = %v", err)
	}
	sess, err := m.BeginCapture("", false)
	if err != nil || sess.Prior != nil {
		t.Fatalf("BeginCapture = %+v, %v", sess, err)
	}
	if !sess.WaitDrained(context.Background()) {
		t.Fatal("nothing fenced: must be drained")
	}
	if out := sess.End(); out.State != StateIdle || out.Restored != nil {
		t.Fatalf("outcome = %+v", out)
	}

	// A capture nobody ends is ended by the safety timer and still restores
	// a holder whose TTL is long enough.
	l, tok := mustAcquire(t, m, ModeHuman, MaxTTL, "h")
	sess, err = m.BeginCapture(l.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	clk.Advance(DefaultCaptureTimeout)
	if st := m.Status(); st.Capture || st.State != StateHuman {
		t.Fatalf("status after safety timeout = %+v", st)
	}
	if _, err := m.Authorize(tok, ModeHuman); err != nil {
		t.Fatalf("holder after safety restore: %v", err)
	}
	if out := sess.End(); out.Restored == nil {
		t.Fatal("End after the safety timer must return the same outcome")
	}
}

func TestMaintenanceBlocksGrantsAndDrains(t *testing.T) {
	m, _ := newReadyManager(t)
	_, tok := mustAcquire(t, m, ModeHuman, 0, "h")
	ticket, release, err := m.AuthorizeTracked(tok, ModeHuman)
	if err != nil {
		t.Fatal(err)
	}
	d, err := m.EnterMaintenance()
	if err != nil {
		t.Fatal(err)
	}
	if !isClosed(ticket.Done()) || ticket.Reason() != EndMaintenance {
		t.Fatal("maintenance must fence the lease")
	}
	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	if d.Wait(short) {
		t.Fatal("Drain.Wait must not complete while a connection is tracked")
	}
	cancel()
	release()
	if !d.Wait(context.Background()) {
		t.Fatal("Drain.Wait after release = false")
	}
	if st := m.Status(); st.State != StateMaintenance || !st.Maintenance {
		t.Fatalf("status = %+v", st)
	}
	if _, _, err := m.Acquire("x", ModeAgent, 0, "k"); !errors.Is(err, ErrMaintenance) {
		t.Fatalf("acquire in maintenance = %v", err)
	}
	if _, err := m.BeginHandoff("t", false); !errors.Is(err, ErrMaintenance) {
		t.Fatalf("takeover in maintenance = %v", err)
	}
	if _, err := m.BeginCapture("", true); !errors.Is(err, ErrMaintenance) {
		t.Fatalf("capture in maintenance = %v", err)
	}
	if _, err := m.EnterMaintenance(); err != nil {
		t.Fatalf("EnterMaintenance must be idempotent: %v", err)
	}
	// A browser restart does not end maintenance.
	m.StartGeneration()
	if st := m.Status(); st.State != StateMaintenance {
		t.Fatalf("state after generation change = %s", st.State)
	}
	if err := m.ExitMaintenance(); err != nil {
		t.Fatal(err)
	}
	if err := m.ExitMaintenance(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second exit = %v", err)
	}
	if _, _, err := m.Acquire("x", ModeAgent, 0, "k"); err != nil {
		t.Fatalf("acquire after maintenance: %v", err)
	}
}

func TestMaintenanceRefusedDuringCapture(t *testing.T) {
	m, _ := newReadyManager(t)
	if _, err := m.BeginCapture("", false); err != nil {
		t.Fatal(err)
	}
	if _, err := m.EnterMaintenance(); !errors.Is(err, ErrConflict) {
		t.Fatalf("EnterMaintenance during capture = %v", err)
	}
}

func TestTrackedReleaseIsIdempotentAndIdleNeedsEnd(t *testing.T) {
	m, _ := newReadyManager(t)
	l, tok := mustAcquire(t, m, ModeAgent, 0, "a")
	_, release, err := m.AuthorizeTracked(tok, ModeAgent)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.AuthorizeTracked(tok, ModeHuman); !errors.Is(err, ErrWrongMode) {
		t.Fatalf("wrong mode = %v", err)
	}
	release()
	release()
	m.mu.Lock()
	s := m.cur
	idle := isClosed(s.idle)
	active := s.active
	m.mu.Unlock()
	if idle || active != 0 {
		t.Fatalf("a live lease must not be idle (idle=%v active=%d)", idle, active)
	}
	if err := m.Release(l.ID); err != nil {
		t.Fatal(err)
	}
	if !isClosed(s.idle) {
		t.Fatal("an ended lease with no tracked use must be idle")
	}
}

func TestBeginHandoffRefusedWhileLockedOrCapturing(t *testing.T) {
	m, err := NewManager(Config{WorkspaceID: "default", Clock: newFakeClock()})
	if err != nil {
		t.Fatal(err)
	}
	var locked *LockedError
	if _, err := m.BeginHandoff("t", false); !errors.As(err, &locked) {
		t.Fatalf("takeover while locked = %v", err)
	}
	m.SetLocked(false, "")
	if _, err := m.BeginCapture("", false); err != nil {
		t.Fatal(err)
	}
	if _, err := m.BeginHandoff("t", false); !errors.Is(err, ErrConflict) {
		t.Fatalf("takeover during capture = %v", err)
	}
}
