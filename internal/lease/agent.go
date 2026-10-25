package lease

import (
	"fmt"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
)

// maxAgentWatchers bounds the policy notification subscribers.
const maxAgentWatchers = 64

// PolicyStore persists the usage policy. Load reports found=false when no
// policy was ever saved; any other failure (unreadable, corrupt, invalid)
// is an error so the runtime fails closed instead of guessing a mode. Save
// replaces the stored policy atomically. Both are called with the manager
// mutex held, so they must not call back into the Manager.
type PolicyStore interface {
	Load() (ctl apiwire.AgentControl, found bool, err error)
	Save(ctl apiwire.AgentControl) error
}

// initialPolicy resolves the policy at startup: the saved policy when the
// store has one, else the configured initial mode (default AGENT), which is
// saved at once so a later change of the startup flag or environment cannot
// override a decision made in settings.
func initialPolicy(cfg Config) (apiwire.AgentControl, error) {
	if cfg.PolicyStore != nil {
		saved, found, err := cfg.PolicyStore.Load()
		if err != nil {
			return apiwire.AgentControl{}, fmt.Errorf("lease: load agent policy: %w", err)
		}
		if found {
			if err := ValidateAgentControl(saved); err != nil {
				return apiwire.AgentControl{}, fmt.Errorf("lease: saved agent policy: %w", err)
			}
			return saved, nil
		}
	}
	mode := cfg.AgentMode
	if mode == "" {
		mode = apiwire.AgentModeAGENT
	}
	if !apiwire.ValidAgentMode(mode) {
		return apiwire.AgentControl{}, fmt.Errorf("%w: agent mode", ErrInvalidArgument)
	}
	ctl := apiwire.AgentControl{Mode: mode, Revision: 1, UpdatedAt: cfg.Clock.Now().UTC()}
	if cfg.PolicyStore != nil {
		if err := cfg.PolicyStore.Save(ctl); err != nil {
			return apiwire.AgentControl{}, fmt.Errorf("lease: save initial agent policy: %w", err)
		}
	}
	return ctl, nil
}

// ValidateAgentControl checks the invariants of a policy: a known mode, no
// pause in NOVNC mode, a positive revision and a timestamp.
func ValidateAgentControl(c apiwire.AgentControl) error {
	switch {
	case !apiwire.ValidAgentMode(c.Mode):
		return fmt.Errorf("unknown mode %q", c.Mode)
	case c.Paused && c.Mode != apiwire.AgentModeAGENT:
		return fmt.Errorf("paused is only valid in %s mode", apiwire.AgentModeAGENT)
	case c.Revision == 0:
		return fmt.Errorf("revision must be positive")
	case c.UpdatedAt.IsZero():
		return fmt.Errorf("updatedAt is required")
	}
	return nil
}

// agentGateLocked reports why AGENT control is currently not allowed.
func (m *Manager) agentGateLocked() error {
	switch {
	case m.agent.Mode != apiwire.AgentModeAGENT:
		return ErrAgentDisabled
	case m.agent.Paused:
		return ErrAgentPaused
	}
	return nil
}

// checkAgentUseLocked refuses use of an AGENT lease while the policy forbids
// AGENT control. Fencing makes this unreachable for a live lease; it keeps
// the invariant local to every admission path.
func (m *Manager) checkAgentUseLocked(s *leaseState) error {
	if s.lease.Mode != ModeAgent {
		return nil
	}
	return m.agentGateLocked()
}

// AgentControl returns the current usage policy.
func (m *Manager) AgentControl() apiwire.AgentControl {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.agent
}

// SetAgentMode switches the usage mode. Switching to NOVNC fences a held
// AGENT lease, discards the restoration of an AGENT lease fenced by a
// running capture and clears the pause. Switching to AGENT starts unpaused
// and never revives a lease. Setting the current mode changes nothing (a
// paused AGENT workspace stays paused). On ErrPolicyPersist a restrictive
// change is in force and the returned policy is the new one; for a relaxing
// change nothing changed.
func (m *Manager) SetAgentMode(mode string) (apiwire.AgentControl, error) {
	if !apiwire.ValidAgentMode(mode) {
		return apiwire.AgentControl{}, fmt.Errorf("%w: agent mode", ErrInvalidArgument)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shutdown {
		return apiwire.AgentControl{}, ErrShutdown
	}
	if m.agent.Mode == mode {
		return m.agent, nil
	}
	next := m.agent
	next.Mode = mode
	next.Paused = false
	return m.changeAgentLocked(next, mode == apiwire.AgentModeNOVNC)
}

// PauseAgent pauses the Agent: a held AGENT lease is fenced at once and no
// AGENT lease is granted, renewed or admitted until ResumeAgent. It is
// ErrAgentDisabled in NOVNC mode and a no-op when already paused.
func (m *Manager) PauseAgent() (apiwire.AgentControl, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shutdown {
		return apiwire.AgentControl{}, ErrShutdown
	}
	if m.agent.Mode != apiwire.AgentModeAGENT {
		return apiwire.AgentControl{}, ErrAgentDisabled
	}
	if m.agent.Paused {
		return m.agent, nil
	}
	next := m.agent
	next.Paused = true
	return m.changeAgentLocked(next, true)
}

// ResumeAgent lifts a pause. It allows a fresh acquisition only: fenced
// leases and capture restorations stay dead, and no lease is granted or
// stolen. It is ErrAgentDisabled in NOVNC mode and a no-op when not paused.
func (m *Manager) ResumeAgent() (apiwire.AgentControl, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shutdown {
		return apiwire.AgentControl{}, ErrShutdown
	}
	if m.agent.Mode != apiwire.AgentModeAGENT {
		return apiwire.AgentControl{}, ErrAgentDisabled
	}
	if !m.agent.Paused {
		return m.agent, nil
	}
	next := m.agent
	next.Paused = false
	return m.changeAgentLocked(next, false)
}

// changeAgentLocked commits next as the new policy under the manager mutex,
// the same one Acquire, Renew and Authorize take, so no AGENT grant or use
// can interleave with the change.
//
// A relaxing change is saved first and applied only when the save succeeds.
// A restrictive change is applied (and the AGENT lease fenced) first; a save
// failure is reported but never leaves the old AGENT lease active.
func (m *Manager) changeAgentLocked(next apiwire.AgentControl, restrictive bool) (apiwire.AgentControl, error) {
	next.Revision = m.agent.Revision + 1
	next.UpdatedAt = m.clock.Now().UTC()
	if !restrictive {
		if err := m.saveLocked(next); err != nil {
			return m.agent, err
		}
		m.agent = next
		m.notifyAgentLocked()
		return next, nil
	}
	m.agent = next
	m.fenceAgentLocked()
	m.notifyAgentLocked()
	if err := m.saveLocked(next); err != nil {
		return next, err
	}
	return next, nil
}

func (m *Manager) saveLocked(c apiwire.AgentControl) error {
	if m.store == nil {
		return nil
	}
	if err := m.store.Save(c); err != nil {
		return fmt.Errorf("%w: %v", ErrPolicyPersist, err)
	}
	return nil
}

// fenceAgentLocked ends a held AGENT lease (closing its tickets) and cancels
// the restoration of an AGENT lease that a capture fenced. HUMAN leases and
// captures are untouched.
func (m *Manager) fenceAgentLocked() {
	if s := m.cur; s != nil && s.lease.Mode == ModeAgent {
		m.endLocked(EndAgentPolicy)
	}
	if c := m.capture; c != nil && c.prior != nil && c.prior.lease.Mode == ModeAgent {
		c.prior.released = true
	}
}

// AgentWatch is a subscription to policy changes. C receives a value
// whenever the policy may have changed since the last receive; the
// notification carries no data and coalesces, so the reader asks the
// manager for the latest AgentControl. Closed is closed when the manager
// shuts down.
type AgentWatch struct {
	C      <-chan struct{}
	ch     chan struct{}
	m      *Manager
	closed <-chan struct{}
}

// Closed is closed when the manager has shut down.
func (w *AgentWatch) Closed() <-chan struct{} { return w.closed }

// Close unsubscribes. It is idempotent.
func (w *AgentWatch) Close() {
	w.m.mu.Lock()
	defer w.m.mu.Unlock()
	delete(w.m.watchers, w)
}

// WatchAgentControl subscribes to policy changes. The returned watch holds a
// pending notification, so the first receive yields the current state.
// ErrTooManyWatchers bounds the subscriber count.
func (m *Manager) WatchAgentControl() (*AgentWatch, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shutdown {
		return nil, ErrShutdown
	}
	if len(m.watchers) >= maxAgentWatchers {
		return nil, ErrTooManyWatchers
	}
	ch := make(chan struct{}, 1)
	ch <- struct{}{}
	w := &AgentWatch{C: ch, ch: ch, m: m, closed: m.done}
	m.watchers[w] = struct{}{}
	return w, nil
}

func (m *Manager) notifyAgentLocked() {
	for w := range m.watchers {
		select {
		case w.ch <- struct{}{}:
		default: // a notification is already pending; it coalesces
		}
	}
}
