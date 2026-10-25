package lease

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
)

func TestEnterMaintenanceIfIdleAdmitsIdleWorkspaceAndBlocksNewControl(t *testing.T) {
	m, _ := newReadyManager(t)
	gen := m.Status().Generation

	d, err := m.EnterMaintenanceIfIdle(gen)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Wait(context.Background()) {
		t.Fatal("nothing was fenced, so the Drain must already be drained")
	}
	if st := m.Status(); st.State != StateMaintenance || !st.Maintenance || st.Generation != gen {
		t.Fatalf("status = %+v", st)
	}
	if _, _, err := m.Acquire("late", ModeAgent, 0, "k"); !errors.Is(err, ErrMaintenance) {
		t.Fatalf("Acquire after idle admission = %v, want ErrMaintenance", err)
	}
	if _, _, err := m.Acquire("late", ModeHuman, 0, "k2"); !errors.Is(err, ErrMaintenance) {
		t.Fatalf("HUMAN Acquire after idle admission = %v, want ErrMaintenance", err)
	}
	if _, err := m.BeginHandoff("t", false); !errors.Is(err, ErrMaintenance) {
		t.Fatalf("takeover after idle admission = %v, want ErrMaintenance", err)
	}
	if _, err := m.BeginCapture("", true); !errors.Is(err, ErrMaintenance) {
		t.Fatalf("capture after idle admission = %v, want ErrMaintenance", err)
	}
	// A second conditional entry is a conflict, never an idempotent success:
	// the caller must not believe it owns a fence somebody else created.
	if _, err := m.EnterMaintenanceIfIdle(gen); !errors.Is(err, ErrMaintenance) {
		t.Fatalf("second conditional entry = %v, want ErrMaintenance", err)
	}
	if err := m.ExitMaintenance(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.Acquire("late", ModeAgent, 0, "k3"); err != nil {
		t.Fatalf("Acquire after exit: %v", err)
	}
}

func TestEnterMaintenanceIfIdleRefusesLiveLeaseAndKeepsItIntact(t *testing.T) {
	for _, mode := range []Mode{ModeHuman, ModeAgent} {
		t.Run(string(mode), func(t *testing.T) {
			m, _ := newReadyManager(t)
			gen := m.Status().Generation
			l, tok := mustAcquire(t, m, mode, 0, "holder")
			ticket, release, err := m.AuthorizeTracked(tok, mode)
			if err != nil {
				t.Fatal(err)
			}
			defer release()

			if _, err := m.EnterMaintenanceIfIdle(gen); !errors.Is(err, ErrConflict) {
				t.Fatalf("conditional entry with a live %s lease = %v, want ErrConflict", mode, err)
			}
			if isClosed(ticket.Done()) {
				t.Fatal("a refused conditional entry fenced the live ticket")
			}
			if _, err := m.Authorize(tok, mode); err != nil {
				t.Fatalf("ticket must stay valid after the refusal: %v", err)
			}
			if _, err := m.Renew(tok, l.ID, l.Epoch, l.Generation); err != nil {
				t.Fatalf("lease must stay renewable after the refusal: %v", err)
			}
			st := m.Status()
			if st.Maintenance || st.Generation != gen || st.Lease == nil || st.Lease.ID != l.ID {
				t.Fatalf("status after refusal = %+v, want the original lease %s in generation %s", st, l.ID, gen)
			}
			// The lease still blocks a second holder exactly as before.
			if _, _, err := m.Acquire("other", ModeAgent, 0, "other"); !errors.Is(err, ErrConflict) {
				t.Fatalf("second Acquire = %v, want ErrConflict", err)
			}
		})
	}
}

func TestEnterMaintenanceIfIdleThenUnconditionalStillFencesLiveLease(t *testing.T) {
	m, _ := newReadyManager(t)
	gen := m.Status().Generation
	_, tok := mustAcquire(t, m, ModeHuman, 0, "h")
	ticket, release, err := m.AuthorizeTracked(tok, ModeHuman)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := m.EnterMaintenanceIfIdle(gen); !errors.Is(err, ErrConflict) {
		t.Fatalf("conditional entry = %v, want ErrConflict", err)
	}
	// The updater's contract is unchanged: it still fences a live lease.
	if _, err := m.EnterMaintenance(); err != nil {
		t.Fatalf("unconditional EnterMaintenance: %v", err)
	}
	if !isClosed(ticket.Done()) || ticket.Reason() != EndMaintenance {
		t.Fatal("unconditional maintenance must still fence the lease")
	}
}

func TestEnterMaintenanceIfIdleRefusesTakeoverAndKeepsIt(t *testing.T) {
	m, _ := newReadyManager(t)
	gen := m.Status().Generation
	h, err := m.BeginHandoff("t", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.EnterMaintenanceIfIdle(gen); !errors.Is(err, ErrConflict) {
		t.Fatalf("conditional entry during takeover = %v, want ErrConflict", err)
	}
	if cur, ok := m.Handoff(); !ok || cur.RequestID != h.RequestID || cur.Status != HandoffReady {
		t.Fatalf("takeover after refusal = %+v ok=%v, want the original %s READY", cur, ok, h.RequestID)
	}
	if st := m.Status(); st.Maintenance || st.Generation != gen {
		t.Fatalf("status after refusal = %+v", st)
	}
	l, _, err := m.AcquireTakeover("human", 0, "k", h.RequestID)
	if err != nil || l.Mode != ModeHuman {
		t.Fatalf("the takeover must still be claimable: %+v, %v", l, err)
	}
}

func TestEnterMaintenanceIfIdleDuringCaptureKeepsRestoration(t *testing.T) {
	m, _ := newReadyManager(t)
	gen := m.Status().Generation
	prior, tok := mustAcquire(t, m, ModeHuman, 0, "h")
	sess, err := m.BeginCapture(prior.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.EnterMaintenanceIfIdle(gen); !errors.Is(err, ErrConflict) {
		t.Fatalf("conditional entry during capture = %v, want ErrConflict", err)
	}
	if st := m.Status(); !st.Capture || st.Maintenance {
		t.Fatalf("status after refusal = %+v, want the capture still held", st)
	}
	out := sess.End()
	if out.Restored == nil || out.Restored.HolderID != prior.HolderID {
		t.Fatalf("capture outcome = %+v, want the prior holder restored", out)
	}
	if _, err := m.Authorize(tok, ModeHuman); err != nil {
		t.Fatalf("the restored holder keeps its token: %v", err)
	}
}

func TestEnterMaintenanceIfIdleChecksGenerationAndArgument(t *testing.T) {
	m, _ := newReadyManager(t)
	oldGen := m.Status().Generation
	l, tok := mustAcquire(t, m, ModeAgent, 0, "a")

	_, err := m.EnterMaintenanceIfIdle("")
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty generation = %v, want ErrInvalidArgument", err)
	}
	if status, code, ok := HTTPError(err); !ok || status != http.StatusBadRequest || code != apiwire.CodeInvalidRequest {
		t.Fatalf("HTTPError(empty generation) = %d %q %v", status, code, ok)
	}
	_, err = m.EnterMaintenanceIfIdle("not-the-generation")
	if !errors.Is(err, ErrGenerationMismatch) {
		t.Fatalf("wrong generation = %v, want ErrGenerationMismatch", err)
	}
	if status, code, ok := HTTPError(err); !ok || status != http.StatusPreconditionFailed || code != apiwire.CodeGenerationMismatch {
		t.Fatalf("HTTPError(wrong generation) = %d %q %v", status, code, ok)
	}
	// The generation is checked before the busy state, so a stale observation
	// is reported as such even while a lease is held, and nothing changed.
	if _, err := m.Authorize(tok, ModeAgent); err != nil {
		t.Fatalf("lease must be untouched by the refusals: %v", err)
	}
	if st := m.Status(); st.Generation != oldGen || st.Lease == nil || st.Lease.ID != l.ID || st.Maintenance {
		t.Fatalf("status = %+v", st)
	}

	newGen := m.StartGeneration()
	if _, err := m.EnterMaintenanceIfIdle(oldGen); !errors.Is(err, ErrGenerationMismatch) {
		t.Fatalf("observed generation after a restart = %v, want ErrGenerationMismatch", err)
	}
	if m.Status().Maintenance {
		t.Fatal("a stale generation must not enter maintenance")
	}
	if _, err := m.EnterMaintenanceIfIdle(newGen); err != nil {
		t.Fatalf("current generation: %v", err)
	}
}

func TestEnterMaintenanceIfIdleExpiresDeadLeaseFirst(t *testing.T) {
	t.Run("timer fired", func(t *testing.T) {
		m, clk := newReadyManager(t)
		gen := m.Status().Generation
		_, tok := mustAcquire(t, m, ModeAgent, MinTTL, "a")
		clk.Advance(MinTTL + time.Second)
		if _, err := m.EnterMaintenanceIfIdle(gen); err != nil {
			t.Fatalf("an expired lease must not block the fence: %v", err)
		}
		var ended *EndedError
		if _, err := m.Authorize(tok, ModeAgent); !errors.As(err, &ended) || ended.Reason != EndExpired {
			t.Fatalf("expired token = %v, want EndedError(EXPIRED)", err)
		}
	})
	t.Run("timer late", func(t *testing.T) {
		m, clk := newReadyManager(t)
		gen := m.Status().Generation
		mustAcquire(t, m, ModeHuman, MinTTL, "h")
		clk.AdvanceWithoutFiring(MinTTL + time.Second)
		if _, err := m.EnterMaintenanceIfIdle(gen); err != nil {
			t.Fatalf("a lease past its deadline must be expired first: %v", err)
		}
	})
}

func TestEnterMaintenanceIfIdleAfterShutdown(t *testing.T) {
	m, _ := newReadyManager(t)
	gen := m.Status().Generation
	m.Shutdown()
	if _, err := m.EnterMaintenanceIfIdle(gen); !errors.Is(err, ErrShutdown) {
		t.Fatalf("after Shutdown = %v, want ErrShutdown", err)
	}
}

// TestEnterMaintenanceIfIdleRacesAcquireWithOneWinner runs Acquire against
// the conditional fence. Exactly one of them may win: either the lease is
// granted and the fence is refused, or the fence is entered and the grant is
// refused with ErrMaintenance.
func TestEnterMaintenanceIfIdleRacesAcquireWithOneWinner(t *testing.T) {
	const rounds = 200
	var fenceWins, acquireWins int
	for i := range rounds {
		m, _ := newReadyManager(t)
		gen := m.Status().Generation
		var (
			wg                   sync.WaitGroup
			fenceErr, acquireErr error
		)
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, fenceErr = m.EnterMaintenanceIfIdle(gen)
		}()
		go func() {
			defer wg.Done()
			<-start
			_, _, acquireErr = m.Acquire("racer", ModeAgent, 0, "race")
		}()
		close(start)
		wg.Wait()
		leaseGranted, fenceEnter := acquireErr == nil, fenceErr == nil

		switch {
		case leaseGranted && fenceEnter:
			t.Fatalf("round %d: both the lease and the fence were granted", i)
		case !leaseGranted && !fenceEnter:
			t.Fatalf("round %d: neither won: acquire=%v fence=%v", i, acquireErr, fenceErr)
		case leaseGranted:
			acquireWins++
			if !errors.Is(fenceErr, ErrConflict) {
				t.Fatalf("round %d: fence loser = %v, want ErrConflict", i, fenceErr)
			}
			if st := m.Status(); st.Maintenance || st.Lease == nil {
				t.Fatalf("round %d: status = %+v, want the granted lease", i, st)
			}
		default:
			fenceWins++
			if !errors.Is(acquireErr, ErrMaintenance) {
				t.Fatalf("round %d: acquire loser = %v, want ErrMaintenance", i, acquireErr)
			}
			if st := m.Status(); !st.Maintenance || st.Lease != nil {
				t.Fatalf("round %d: status = %+v, want maintenance without a lease", i, st)
			}
		}
	}
	t.Logf("fence won %d, acquire won %d of %d rounds", fenceWins, acquireWins, rounds)
}
