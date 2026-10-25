package lease

import "testing"

func TestOnlyRestoredLeasesAreEpochBound(t *testing.T) {
	m, _ := newReadyManager(t)
	old, tok := mustAcquire(t, m, ModeHuman, 0, "h")
	t0, err := m.Authorize(tok, ModeHuman)
	if err != nil {
		t.Fatal(err)
	}
	if t0.EpochBound() {
		t.Fatal("a freshly granted lease must not be epoch-bound")
	}
	if (Ticket{}).EpochBound() {
		t.Fatal("the zero ticket must not be epoch-bound")
	}

	sess, err := m.BeginCapture(old.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	out := sess.End()
	if out.Restored == nil {
		t.Fatal("expected a restored lease")
	}
	t1, err := m.Authorize(tok, ModeHuman)
	if err != nil {
		t.Fatal(err)
	}
	if !t1.EpochBound() || t1.Lease().Epoch != out.Restored.Epoch {
		t.Fatalf("restored ticket: bound=%v epoch=%d, want bound at %d", t1.EpochBound(), t1.Lease().Epoch, out.Restored.Epoch)
	}

	// Releasing and acquiring afresh yields an unbound lease again.
	if err := m.Release(out.Restored.ID); err != nil {
		t.Fatal(err)
	}
	_, tok2 := mustAcquire(t, m, ModeAgent, 0, "a")
	t2, err := m.Authorize(tok2, ModeAgent)
	if err != nil {
		t.Fatal(err)
	}
	if t2.EpochBound() {
		t.Fatal("a new grant after a restore must not inherit the binding")
	}
}
