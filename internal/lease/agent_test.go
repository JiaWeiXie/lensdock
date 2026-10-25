package lease

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
)

// memStore is a PolicyStore double with injectable failures.
type memStore struct {
	mu      sync.Mutex
	ctl     apiwire.AgentControl
	found   bool
	loadErr error
	saveErr error
	saves   int
}

func (s *memStore) Load() (apiwire.AgentControl, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ctl, s.found, s.loadErr
}

func (s *memStore) Save(c apiwire.AgentControl) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saveErr != nil {
		return s.saveErr
	}
	s.ctl, s.found = c, true
	s.saves++
	return nil
}

func newPolicyManager(t *testing.T, cfg Config) (*Manager, *fakeClock) {
	t.Helper()
	clk := newFakeClock()
	cfg.WorkspaceID = "default"
	cfg.Clock = clk
	m, err := NewManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m.SetLocked(false, "")
	return m, clk
}

// okCtl returns a checker for the (policy, error) pair of a Manager call:
// okCtl(t)(m.PauseAgent()) fails the test on error and yields the policy.
func okCtl(t *testing.T) func(apiwire.AgentControl, error) apiwire.AgentControl {
	t.Helper()
	return func(ctl apiwire.AgentControl, err error) apiwire.AgentControl {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return ctl
	}
}

func TestAgentPolicyDefaultsToUnpausedAgentRevisionOne(t *testing.T) {
	m, clk := newPolicyManager(t, Config{})
	got := m.AgentControl()
	want := apiwire.AgentControl{Mode: apiwire.AgentModeAGENT, Revision: 1, UpdatedAt: clk.Now()}
	if got != want {
		t.Fatalf("AgentControl = %+v, want %+v", got, want)
	}
	if m.Status().AgentControl != got {
		t.Fatal("Status must carry the policy")
	}
	if _, _, err := m.Acquire("a", ModeAgent, 0, "k"); err != nil {
		t.Fatalf("default mode must allow AGENT: %v", err)
	}
}

func TestInitialNOVNCModeAndInvalidMode(t *testing.T) {
	m, _ := newPolicyManager(t, Config{AgentMode: apiwire.AgentModeNOVNC})
	if _, _, err := m.Acquire("a", ModeAgent, 0, "k"); !errors.Is(err, ErrAgentDisabled) {
		t.Fatalf("AGENT acquire in NOVNC = %v, want ErrAgentDisabled", err)
	}
	if _, _, err := m.Acquire("h", ModeHuman, 0, "h"); err != nil {
		t.Fatalf("HUMAN acquire in NOVNC: %v", err)
	}
	if _, err := NewManager(Config{WorkspaceID: "default", AgentMode: "novnc"}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("lower-case mode on the wire type = %v, want ErrInvalidArgument", err)
	}
}

func TestNOVNCRestrictionsAndErrorCodes(t *testing.T) {
	m, _ := newPolicyManager(t, Config{AgentMode: apiwire.AgentModeNOVNC})
	if _, err := m.PauseAgent(); !errors.Is(err, ErrAgentDisabled) {
		t.Fatalf("pause in NOVNC = %v", err)
	}
	if _, err := m.ResumeAgent(); !errors.Is(err, ErrAgentDisabled) {
		t.Fatalf("resume in NOVNC = %v", err)
	}
	if _, err := m.SetAgentMode("bogus"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("invalid mode = %v", err)
	}
	for _, tt := range []struct {
		err  error
		code string
	}{
		{ErrAgentDisabled, apiwire.CodeAgentModeDisabled},
		{ErrAgentPaused, apiwire.CodeAgentPaused},
	} {
		status, code, ok := HTTPError(tt.err)
		if !ok || status != http.StatusConflict || code != tt.code {
			t.Errorf("HTTPError(%v) = %d %q %v", tt.err, status, code, ok)
		}
	}
}

func TestPauseFencesAgentLeaseAndResumeCannotReviveIt(t *testing.T) {
	m, _ := newPolicyManager(t, Config{})
	old, tok := mustAcquire(t, m, ModeAgent, 0, "k1")
	ticket, release, err := m.AuthorizeTracked(tok, ModeAgent)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	ctl := okCtl(t)(m.PauseAgent())
	if !ctl.Paused || ctl.Mode != apiwire.AgentModeAGENT || ctl.Revision != 2 {
		t.Fatalf("paused = %+v", ctl)
	}
	if !isClosed(ticket.Done()) || ticket.Reason() != EndAgentPolicy {
		t.Fatalf("ticket must be closed with %s, reason %q", EndAgentPolicy, ticket.Reason())
	}
	if st := m.Status(); st.State != StateIdle || st.Lease != nil {
		t.Fatalf("state after pause = %+v", st)
	}
	for name, err := range map[string]error{
		"authorize": func() error { _, e := m.Authorize(tok, ModeAgent); return e }(),
		"tracked":   func() error { _, _, e := m.AuthorizeTracked(tok, ModeAgent); return e }(),
		"renew":     func() error { _, e := m.Renew(tok, old.ID, old.Epoch, old.Generation); return e }(),
		"identify":  func() error { _, e := m.Identify(tok); return e }(),
		"acquire":   func() error { _, _, e := m.Acquire("a", ModeAgent, 0, "k2"); return e }(),
	} {
		if !errors.Is(err, ErrAgentPaused) {
			t.Errorf("%s while paused = %v, want ErrAgentPaused", name, err)
		}
	}

	ctl = okCtl(t)(m.ResumeAgent())
	if ctl.Paused || ctl.Revision != 3 {
		t.Fatalf("resumed = %+v", ctl)
	}
	if st := m.Status(); st.State != StateIdle || st.Lease != nil {
		t.Fatalf("resume must not auto-acquire: %+v", st)
	}
	var ended *EndedError
	if _, err := m.Authorize(tok, ModeAgent); !errors.As(err, &ended) || ended.Reason != EndAgentPolicy {
		t.Fatalf("old token after resume = %v, want EndedError(%s)", err, EndAgentPolicy)
	}
	if _, err := m.Renew(tok, old.ID, old.Epoch, old.Generation); !errors.As(err, &ended) {
		t.Fatalf("renew of old lease after resume = %v", err)
	}
	fresh, tok2 := mustAcquire(t, m, ModeAgent, 0, "k3")
	if fresh.Epoch <= old.Epoch || tok2 == tok {
		t.Fatalf("fresh acquisition must have a new epoch and token: %+v", fresh)
	}
	if _, err := m.Authorize(tok, ModeAgent); err == nil {
		t.Fatal("old token authorized next to a fresh lease")
	}
}

func TestPauseAndResumeAreIdempotent(t *testing.T) {
	m, _ := newPolicyManager(t, Config{})
	first := okCtl(t)(m.PauseAgent())
	again := okCtl(t)(m.PauseAgent())
	if first != again {
		t.Fatalf("second pause changed the policy: %+v then %+v", first, again)
	}
	r1 := okCtl(t)(m.ResumeAgent())
	r2 := okCtl(t)(m.ResumeAgent())
	if r1 != r2 || r1.Revision != first.Revision+1 {
		t.Fatalf("resume = %+v then %+v", r1, r2)
	}
	// PUT of the current mode must not silently resume a pause.
	okCtl(t)(m.PauseAgent())
	same := okCtl(t)(m.SetAgentMode(apiwire.AgentModeAGENT))
	if !same.Paused {
		t.Fatal("setting the current mode must not clear a pause")
	}
}

func TestPausePreservesHumanLease(t *testing.T) {
	m, _ := newPolicyManager(t, Config{})
	hl, tok := mustAcquire(t, m, ModeHuman, 0, "h")
	ticket, release, err := m.AuthorizeTracked(tok, ModeHuman)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	okCtl(t)(m.PauseAgent())
	okCtl(t)(m.SetAgentMode(apiwire.AgentModeNOVNC))
	if isClosed(ticket.Done()) {
		t.Fatal("pause/NOVNC must not close the HUMAN lease")
	}
	if _, err := m.Authorize(tok, ModeHuman); err != nil {
		t.Fatalf("HUMAN authorize: %v", err)
	}
	if _, err := m.Renew(tok, hl.ID, hl.Epoch, hl.Generation); err != nil {
		t.Fatalf("HUMAN renew: %v", err)
	}
	if st := m.Status(); st.State != StateHuman {
		t.Fatalf("state = %s", st.State)
	}
	// Resuming neither steals the HUMAN lease nor auto-acquires.
	okCtl(t)(m.SetAgentMode(apiwire.AgentModeAGENT))
	if st := m.Status(); st.State != StateHuman || st.Lease == nil || st.Lease.Mode != ModeHuman {
		t.Fatalf("enable AGENT disturbed the HUMAN lease: %+v", st)
	}
	if _, _, err := m.Acquire("a", ModeAgent, 0, "a"); !errors.Is(err, ErrConflict) {
		t.Fatalf("AGENT acquire next to HUMAN = %v, want ErrConflict", err)
	}
}

func TestSetAgentModeSemantics(t *testing.T) {
	m, _ := newPolicyManager(t, Config{})
	_, tok := mustAcquire(t, m, ModeAgent, 0, "k")
	okCtl(t)(m.PauseAgent())

	off := okCtl(t)(m.SetAgentMode(apiwire.AgentModeNOVNC))
	if off.Mode != apiwire.AgentModeNOVNC || off.Paused || off.Revision != 3 {
		t.Fatalf("NOVNC = %+v (must clear the pause)", off)
	}
	if _, err := m.Authorize(tok, ModeAgent); !errors.Is(err, ErrAgentDisabled) {
		t.Fatalf("old token in NOVNC = %v, want ErrAgentDisabled", err)
	}
	if again := okCtl(t)(m.SetAgentMode(apiwire.AgentModeNOVNC)); again != off {
		t.Fatalf("repeat NOVNC changed the policy: %+v", again)
	}

	on := okCtl(t)(m.SetAgentMode(apiwire.AgentModeAGENT))
	if on.Mode != apiwire.AgentModeAGENT || on.Paused || on.Revision != 4 {
		t.Fatalf("AGENT = %+v (must start unpaused)", on)
	}
	if st := m.Status(); st.Lease != nil {
		t.Fatalf("enabling AGENT must not revive a lease: %+v", st.Lease)
	}
	if _, err := m.Authorize(tok, ModeAgent); err == nil {
		t.Fatal("old token authorized after re-enable")
	}
	if on.UpdatedAt.Before(off.UpdatedAt) {
		t.Fatal("updatedAt must not go backwards")
	}
}

func TestStartGenerationAndLockDoNotResetPolicy(t *testing.T) {
	m, _ := newPolicyManager(t, Config{})
	okCtl(t)(m.PauseAgent())
	m.StartGeneration()
	m.SetLocked(true, "")
	m.SetLocked(false, "")
	if got := m.AgentControl(); !got.Paused || got.Revision != 2 {
		t.Fatalf("policy after generation/lock cycle = %+v", got)
	}
	if _, _, err := m.Acquire("a", ModeAgent, 0, "k"); !errors.Is(err, ErrAgentPaused) {
		t.Fatalf("acquire after generation change = %v", err)
	}
}

func TestPauseDuringCaptureDiscardsAgentRestoration(t *testing.T) {
	for name, stop := range map[string]func(t *testing.T, m *Manager){
		"pause": func(t *testing.T, m *Manager) { okCtl(t)(m.PauseAgent()) },
		"pause then resume": func(t *testing.T, m *Manager) {
			okCtl(t)(m.PauseAgent())
			okCtl(t)(m.ResumeAgent())
		},
		"novnc": func(t *testing.T, m *Manager) { okCtl(t)(m.SetAgentMode(apiwire.AgentModeNOVNC)) },
		"novnc then agent": func(t *testing.T, m *Manager) {
			okCtl(t)(m.SetAgentMode(apiwire.AgentModeNOVNC))
			okCtl(t)(m.SetAgentMode(apiwire.AgentModeAGENT))
		},
	} {
		t.Run(name, func(t *testing.T) {
			m, _ := newPolicyManager(t, Config{})
			l, tok := mustAcquire(t, m, ModeAgent, 0, "k")
			sess, err := m.BeginCapture(l.ID, true)
			if err != nil {
				t.Fatal(err)
			}
			stop(t, m)
			out := sess.End()
			if out.Restored != nil {
				t.Fatalf("an AGENT lease fenced by a capture was restored after %s: %+v", name, out.Restored)
			}
			if out.Prior == nil || out.Prior.LeaseID != l.ID || out.State != StateIdle {
				t.Fatalf("outcome = %+v", out)
			}
			if st := m.Status(); st.Lease != nil {
				t.Fatalf("lease after capture = %+v", st.Lease)
			}
			if _, err := m.Authorize(tok, ModeAgent); err == nil {
				t.Fatal("old token still authorizes after the capture")
			}
		})
	}
}

func TestCaptureStillRestoresHumanWhilePaused(t *testing.T) {
	m, _ := newPolicyManager(t, Config{})
	l, tok := mustAcquire(t, m, ModeHuman, 0, "h")
	sess, err := m.BeginCapture(l.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	okCtl(t)(m.PauseAgent())
	out := sess.End()
	if out.Restored == nil || out.State != StateHuman {
		t.Fatalf("HUMAN must be restored while paused: %+v", out)
	}
	if _, err := m.Authorize(tok, ModeHuman); err != nil {
		t.Fatalf("HUMAN after capture: %v", err)
	}
}

func TestCaptureStartedWhilePausedWorks(t *testing.T) {
	m, _ := newPolicyManager(t, Config{})
	okCtl(t)(m.PauseAgent())
	sess, err := m.BeginCapture("", false)
	if err != nil {
		t.Fatalf("a human capture while paused: %v", err)
	}
	if out := sess.End(); out.State != StateIdle {
		t.Fatalf("outcome = %+v", out)
	}
}

// TestAcquireVersusDisableRace runs an AGENT acquisition against a switch to
// NOVNC. Whatever the interleaving, once SetAgentMode has returned no AGENT
// lease may be live: either the acquisition lost, or it won and was fenced.
// The loop uses a start barrier and no sleeps; the race detector and the
// invariant (not timing) carry the check.
func TestAcquireVersusDisableRace(t *testing.T) {
	for i := range 300 {
		m, _ := newPolicyManager(t, Config{})
		start := make(chan struct{})
		type result struct {
			tok string
			err error
		}
		res := make(chan result, 1)
		go func() {
			<-start
			_, tok, err := m.Acquire("a", ModeAgent, 0, "k")
			res <- result{tok.Reveal(), err}
		}()
		close(start)
		runtime.Gosched()
		okCtl(t)(m.SetAgentMode(apiwire.AgentModeNOVNC))
		r := <-res
		if st := m.Status(); st.Lease != nil {
			t.Fatalf("iteration %d: lease live after disable: %+v", i, st.Lease)
		}
		switch {
		case r.err == nil:
			if _, err := m.Authorize(r.tok, ModeAgent); err == nil {
				t.Fatalf("iteration %d: acquired token still authorizes after disable", i)
			}
		case !errors.Is(r.err, ErrAgentDisabled):
			t.Fatalf("iteration %d: acquire = %v", i, r.err)
		}
		if _, _, err := m.Acquire("a", ModeAgent, 0, "k2"); !errors.Is(err, ErrAgentDisabled) {
			t.Fatalf("iteration %d: acquire after disable = %v", i, err)
		}
	}
}

func TestPolicyIsSavedAndSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	store := FilePolicyStore{Dir: dir}
	// The first start saves the initial mode.
	m1, _ := newPolicyManager(t, Config{AgentMode: apiwire.AgentModeNOVNC, PolicyStore: store})
	if got := m1.AgentControl(); got.Mode != apiwire.AgentModeNOVNC || got.Revision != 1 {
		t.Fatalf("first start = %+v", got)
	}
	// A later start with a different flag/env still gets the saved policy.
	m2, _ := newPolicyManager(t, Config{AgentMode: apiwire.AgentModeAGENT, PolicyStore: store})
	if got := m2.AgentControl(); got.Mode != apiwire.AgentModeNOVNC {
		t.Fatalf("the saved policy must win over the initial mode: %+v", got)
	}
	okCtl(t)(m2.SetAgentMode(apiwire.AgentModeAGENT))
	paused := okCtl(t)(m2.PauseAgent())

	m3, _ := newPolicyManager(t, Config{AgentMode: apiwire.AgentModeNOVNC, PolicyStore: store})
	got := m3.AgentControl()
	if got.Mode != apiwire.AgentModeAGENT || !got.Paused || got.Revision != paused.Revision || !got.UpdatedAt.Equal(paused.UpdatedAt) {
		t.Fatalf("restart lost the pause: %+v, want %+v", got, paused)
	}
	if _, _, err := m3.Acquire("a", ModeAgent, 0, "k"); !errors.Is(err, ErrAgentPaused) {
		t.Fatalf("a restart must not undo a pause: %v", err)
	}
	okCtl(t)(m3.ResumeAgent())
	m4, _ := newPolicyManager(t, Config{PolicyStore: store})
	if got := m4.AgentControl(); got.Paused || got.Revision != paused.Revision+1 {
		t.Fatalf("resumed state after restart = %+v", got)
	}

	path := filepath.Join(dir, PolicyFileName)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("policy file mode = %o, want 0600", fi.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
}

func TestCorruptOrInvalidPolicyFailsClosedAtStartup(t *testing.T) {
	for name, content := range map[string]string{
		"garbage":         "not json",
		"empty":           "",
		"trailing":        `{"version":1,"mode":"AGENT","paused":false,"revision":1,"updatedAt":"2026-10-24T12:00:00Z"} {}`,
		"unknown field":   `{"version":1,"mode":"AGENT","paused":false,"revision":1,"updatedAt":"2026-10-24T12:00:00Z","extra":1}`,
		"bad version":     `{"version":2,"mode":"AGENT","paused":false,"revision":1,"updatedAt":"2026-10-24T12:00:00Z"}`,
		"unknown mode":    `{"version":1,"mode":"OTHER","paused":false,"revision":1,"updatedAt":"2026-10-24T12:00:00Z"}`,
		"lower mode":      `{"version":1,"mode":"agent","paused":false,"revision":1,"updatedAt":"2026-10-24T12:00:00Z"}`,
		"paused novnc":    `{"version":1,"mode":"NOVNC","paused":true,"revision":1,"updatedAt":"2026-10-24T12:00:00Z"}`,
		"zero revision":   `{"version":1,"mode":"AGENT","paused":false,"revision":0,"updatedAt":"2026-10-24T12:00:00Z"}`,
		"missing updated": `{"version":1,"mode":"AGENT","paused":false,"revision":1}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, PolicyFileName), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			// Even a NOVNC-or-AGENT initial mode must not paper over a bad file.
			if _, err := NewManager(Config{WorkspaceID: "default", PolicyStore: FilePolicyStore{Dir: dir}}); err == nil {
				t.Fatal("NewManager accepted a corrupt policy")
			}
			// A corrupt file is never overwritten by a later save.
			if err := (FilePolicyStore{Dir: dir}).Save(apiwire.AgentControl{Mode: apiwire.AgentModeAGENT, Revision: 9, UpdatedAt: time.Now()}); err == nil {
				t.Fatal("Save overwrote a corrupt policy")
			}
		})
	}
}

func TestPolicyFileMustBeRegular(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.WriteFile(target, []byte(`{"version":1,"mode":"AGENT","paused":false,"revision":1,"updatedAt":"2026-10-24T12:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, PolicyFileName)); err != nil {
		t.Fatal(err)
	}
	if _, err := NewManager(Config{WorkspaceID: "default", PolicyStore: FilePolicyStore{Dir: dir}}); err == nil {
		t.Fatal("a symlinked policy file was accepted")
	}
}

func TestFilePolicyStoreRefusesStaleRevision(t *testing.T) {
	store := FilePolicyStore{Dir: t.TempDir()}
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	if err := store.Save(apiwire.AgentControl{Mode: apiwire.AgentModeAGENT, Revision: 5, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, rev := range []uint64{1, 5} {
		err := store.Save(apiwire.AgentControl{Mode: apiwire.AgentModeNOVNC, Revision: rev, UpdatedAt: now})
		if !errors.Is(err, ErrStalePolicy) {
			t.Fatalf("Save(revision %d) = %v, want ErrStalePolicy", rev, err)
		}
	}
	got, found, err := store.Load()
	if err != nil || !found || got.Mode != apiwire.AgentModeAGENT || got.Revision != 5 {
		t.Fatalf("stored policy changed: %+v %v %v", got, found, err)
	}
}

func TestInitialLoadAndSaveFailuresRefuseStartup(t *testing.T) {
	if _, err := NewManager(Config{WorkspaceID: "default", PolicyStore: &memStore{loadErr: errors.New("io")}}); err == nil {
		t.Fatal("load failure must fail startup")
	}
	if _, err := NewManager(Config{WorkspaceID: "default", PolicyStore: &memStore{saveErr: errors.New("read-only")}}); err == nil {
		t.Fatal("failing to save the initial policy must fail startup")
	}
	// A saved policy needs no write at startup (a read-only volume works).
	saved := &memStore{found: true, saveErr: errors.New("read-only"),
		ctl: apiwire.AgentControl{Mode: apiwire.AgentModeNOVNC, Revision: 4, UpdatedAt: time.Now()}}
	m, err := NewManager(Config{WorkspaceID: "default", PolicyStore: saved})
	if err != nil {
		t.Fatalf("startup with a saved policy on a read-only store: %v", err)
	}
	if got := m.AgentControl(); got.Mode != apiwire.AgentModeNOVNC || got.Revision != 4 {
		t.Fatalf("policy = %+v", got)
	}
}

func TestRestrictiveChangeSurvivesSaveFailure(t *testing.T) {
	for name, change := range map[string]func(*Manager) (apiwire.AgentControl, error){
		"pause": func(m *Manager) (apiwire.AgentControl, error) { return m.PauseAgent() },
		"novnc": func(m *Manager) (apiwire.AgentControl, error) { return m.SetAgentMode(apiwire.AgentModeNOVNC) },
	} {
		t.Run(name, func(t *testing.T) {
			store := &memStore{}
			m, _ := newPolicyManager(t, Config{PolicyStore: store})
			_, tok := mustAcquire(t, m, ModeAgent, 0, "k")
			ticket, release, err := m.AuthorizeTracked(tok, ModeAgent)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			store.mu.Lock()
			store.saveErr = errors.New("disk full")
			store.mu.Unlock()

			ctl, err := change(m)
			if !errors.Is(err, ErrPolicyPersist) {
				t.Fatalf("err = %v, want ErrPolicyPersist", err)
			}
			if status, code, ok := HTTPError(err); !ok || status != http.StatusInternalServerError || code != apiwire.CodeInternal {
				t.Fatalf("HTTPError = %d %q %v", status, code, ok)
			}
			if ctl.Revision != 2 || m.AgentControl() != ctl {
				t.Fatalf("the restriction must be in force: returned %+v, manager %+v", ctl, m.AgentControl())
			}
			if !isClosed(ticket.Done()) {
				t.Fatal("a failed save kept the old AGENT lease active")
			}
			if _, err := m.Authorize(tok, ModeAgent); err == nil {
				t.Fatal("old token still authorizes")
			}
			if store.ctl.Revision != 1 {
				t.Fatalf("store changed despite the failure: %+v", store.ctl)
			}
		})
	}
}

func TestRelaxingChangeIsAppliedOnlyAfterSave(t *testing.T) {
	store := &memStore{}
	m, _ := newPolicyManager(t, Config{PolicyStore: store})
	okCtl(t)(m.PauseAgent())
	before := m.AgentControl()
	store.mu.Lock()
	store.saveErr = errors.New("disk full")
	store.mu.Unlock()

	if _, err := m.ResumeAgent(); !errors.Is(err, ErrPolicyPersist) {
		t.Fatalf("resume = %v, want ErrPolicyPersist", err)
	}
	if got := m.AgentControl(); got != before {
		t.Fatalf("resume applied without being saved: %+v", got)
	}
	if _, _, err := m.Acquire("a", ModeAgent, 0, "k"); !errors.Is(err, ErrAgentPaused) {
		t.Fatalf("acquire after failed resume = %v", err)
	}
}

func TestEnablingAgentIsNotAppliedWithoutSave(t *testing.T) {
	store := &memStore{}
	m, _ := newPolicyManager(t, Config{AgentMode: apiwire.AgentModeNOVNC, PolicyStore: store})
	store.mu.Lock()
	store.saveErr = errors.New("disk full")
	store.mu.Unlock()
	if _, err := m.SetAgentMode(apiwire.AgentModeAGENT); !errors.Is(err, ErrPolicyPersist) {
		t.Fatalf("enable = %v, want ErrPolicyPersist", err)
	}
	if got := m.AgentControl(); got.Mode != apiwire.AgentModeNOVNC || got.Revision != 1 {
		t.Fatalf("enable applied without being saved: %+v", got)
	}
}

func TestWatchAgentControl(t *testing.T) {
	m, _ := newPolicyManager(t, Config{})
	w, err := m.WatchAgentControl()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	recv := func() {
		t.Helper()
		select {
		case <-w.C:
		default:
			t.Fatal("expected a pending notification")
		}
	}
	recv() // initial state
	select {
	case <-w.C:
		t.Fatal("unexpected notification")
	default:
	}

	okCtl(t)(m.PauseAgent())
	okCtl(t)(m.ResumeAgent())
	okCtl(t)(m.SetAgentMode(apiwire.AgentModeNOVNC))
	recv() // three changes coalesce into one notification
	select {
	case <-w.C:
		t.Fatal("notifications must coalesce")
	default:
	}
	if got := m.AgentControl(); got.Mode != apiwire.AgentModeNOVNC || got.Revision != 4 {
		t.Fatalf("latest = %+v", got)
	}
	// A no-op does not notify.
	okCtl(t)(m.SetAgentMode(apiwire.AgentModeNOVNC))
	select {
	case <-w.C:
		t.Fatal("a no-op notified")
	default:
	}

	if isClosed(w.Closed()) {
		t.Fatal("closed too early")
	}
	m.Shutdown()
	if !isClosed(w.Closed()) {
		t.Fatal("Shutdown must close the watch")
	}
	if _, err := m.WatchAgentControl(); !errors.Is(err, ErrShutdown) {
		t.Fatalf("watch after shutdown = %v", err)
	}
	if _, err := m.PauseAgent(); !errors.Is(err, ErrShutdown) {
		t.Fatalf("pause after shutdown = %v", err)
	}
}

func TestWatchAgentControlIsBounded(t *testing.T) {
	m, _ := newPolicyManager(t, Config{})
	var watches []*AgentWatch
	for {
		w, err := m.WatchAgentControl()
		if err != nil {
			if !errors.Is(err, ErrTooManyWatchers) {
				t.Fatal(err)
			}
			if status, code, ok := HTTPError(err); !ok || status != http.StatusTooManyRequests || code != apiwire.CodeTooManyConnections {
				t.Fatalf("HTTPError = %d %q %v", status, code, ok)
			}
			break
		}
		watches = append(watches, w)
		if len(watches) > 1000 {
			t.Fatal("no watcher limit")
		}
	}
	watches[0].Close()
	if _, err := m.WatchAgentControl(); err != nil {
		t.Fatalf("a closed watch must free its slot: %v", err)
	}
}
