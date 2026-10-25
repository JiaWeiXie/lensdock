package lease

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
	"github.com/JiaWeiXie/lensdock/internal/auth"
)

const (
	// MinTTL and MaxTTL are the TTL bounds a Manager enforces by default.
	MinTTL = 10 * time.Second
	MaxTTL = 120 * time.Second

	defaultTTL = 30 * time.Second

	// DefaultDrainTimeout bounds how long a takeover or maintenance entry
	// waits for the connections of a fenced lease to close.
	DefaultDrainTimeout = 5 * time.Second
	// DefaultHandoffTTL is how long a takeover reservation lives before the
	// workspace returns to IDLE.
	DefaultHandoffTTL = 60 * time.Second
	// DefaultCaptureTimeout is the safety bound of a CAPTURE hold: when the
	// capturing caller never ends it, the manager ends it itself.
	DefaultCaptureTimeout = 30 * time.Second

	// endedRingSize is how many ended-lease token digests are remembered so
	// late requests get EndedError rather than ErrUnauthenticated.
	endedRingSize = 16

	// maxHolderIDLen mirrors the openapi maxLength of Lease.holderId.
	maxHolderIDLen = 128

	// DefaultLockCode is the lock code used at startup and when SetLocked is
	// called with an empty code.
	DefaultLockCode = apiwire.CodeRuntimeNotReady

	tokenPrefix = "ldl_"
)

// Config configures a Manager. Zero fields take their defaults: SystemClock,
// MinTTL 10s, MaxTTL 120s, DefaultTTL 30s, DrainTimeout 5s, HandoffTTL 60s
// and CaptureTimeout 30s.
type Config struct {
	WorkspaceID    string
	Clock          Clock
	MinTTL         time.Duration
	MaxTTL         time.Duration
	DefaultTTL     time.Duration
	DrainTimeout   time.Duration
	HandoffTTL     time.Duration
	CaptureTimeout time.Duration
	// AgentMode is the initial usage mode (apiwire.AgentModeNOVNC or
	// AgentModeAGENT; empty means AGENT). It applies only when PolicyStore
	// is nil or holds no saved policy; a saved policy always wins.
	AgentMode string
	// PolicyStore persists the usage policy. Nil keeps it in memory only.
	PolicyStore PolicyStore
}

type endedToken struct {
	digest auth.Digest
	reason EndReason
}

// handoffState is the mutable record of a takeover.
type handoffState struct {
	snap      Handoff
	idemKey   string
	autoForce bool
	timer     Timer
}

// priorState is what CAPTURE needs to restore the fenced holder.
type priorState struct {
	lease   Lease
	digest  auth.Digest
	ttl     time.Duration
	idemKey string
	// released is set when the holder (or the operator) explicitly released
	// the fenced lease during the capture: it is still reported as the
	// capture's prior lease but is never restored.
	released bool
}

// captureState is the mutable record of a CAPTURE hold.
type captureState struct {
	prior   *priorState
	fenced  *leaseState
	timer   Timer
	outcome *CaptureOutcome // set once the capture has ended
}

// Manager owns the lease state of one workspace. All methods are safe for
// concurrent use.
type Manager struct {
	workspaceID    string
	clock          Clock
	minTTL         time.Duration
	maxTTL         time.Duration
	defaultTTL     time.Duration
	drainTimeout   time.Duration
	handoffTTL     time.Duration
	captureTimeout time.Duration

	mu          sync.Mutex
	generation  string
	locked      bool
	lockCode    string
	shutdown    bool
	maintenance bool
	epoch       uint64
	cur         *leaseState
	handoff     *handoffState
	capture     *captureState
	ended       [endedRingSize]endedToken
	endedCount  int
	endedNext   int

	// agent is the authoritative usage policy; store persists it (may be
	// nil); watchers are notified after every change; done is closed by
	// Shutdown.
	agent    apiwire.AgentControl
	store    PolicyStore
	watchers map[*AgentWatch]struct{}
	done     chan struct{}
}

// NewManager returns a Manager that starts locked (RUNTIME_NOT_READY) with a
// fresh runtime generation. Call SetLocked(false, "") once the runtime is
// ready to accept leases.
func NewManager(cfg Config) (*Manager, error) {
	if cfg.WorkspaceID == "" {
		return nil, errors.New("lease: workspace id is required")
	}
	if cfg.Clock == nil {
		cfg.Clock = SystemClock()
	}
	if cfg.MinTTL == 0 {
		cfg.MinTTL = MinTTL
	}
	if cfg.MaxTTL == 0 {
		cfg.MaxTTL = MaxTTL
	}
	if cfg.DefaultTTL == 0 {
		cfg.DefaultTTL = defaultTTL
	}
	if cfg.DrainTimeout == 0 {
		cfg.DrainTimeout = DefaultDrainTimeout
	}
	if cfg.HandoffTTL == 0 {
		cfg.HandoffTTL = DefaultHandoffTTL
	}
	if cfg.CaptureTimeout == 0 {
		cfg.CaptureTimeout = DefaultCaptureTimeout
	}
	if cfg.MinTTL < 0 || cfg.MinTTL > cfg.DefaultTTL || cfg.DefaultTTL > cfg.MaxTTL {
		return nil, fmt.Errorf("lease: require 0 <= MinTTL (%s) <= DefaultTTL (%s) <= MaxTTL (%s)",
			cfg.MinTTL, cfg.DefaultTTL, cfg.MaxTTL)
	}
	if cfg.DrainTimeout < 0 || cfg.HandoffTTL < 0 || cfg.CaptureTimeout < 0 {
		return nil, errors.New("lease: drain, handoff and capture timeouts must be positive")
	}
	agent, err := initialPolicy(cfg)
	if err != nil {
		return nil, err
	}
	return &Manager{
		workspaceID:    cfg.WorkspaceID,
		clock:          cfg.Clock,
		minTTL:         cfg.MinTTL,
		maxTTL:         cfg.MaxTTL,
		defaultTTL:     cfg.DefaultTTL,
		drainTimeout:   cfg.DrainTimeout,
		handoffTTL:     cfg.HandoffTTL,
		captureTimeout: cfg.CaptureTimeout,
		generation:     newUUID(),
		locked:         true,
		lockCode:       DefaultLockCode,
		agent:          agent,
		store:          cfg.PolicyStore,
		watchers:       make(map[*AgentWatch]struct{}),
		done:           make(chan struct{}),
	}, nil
}

// DrainTimeout returns how long takeover and maintenance wait for a fenced
// lease's connections to close.
func (m *Manager) DrainTimeout() time.Duration { return m.drainTimeout }

// CaptureTimeout returns the safety bound of a CAPTURE hold.
func (m *Manager) CaptureTimeout() time.Duration { return m.captureTimeout }

// Acquire grants a lease to holderID. A zero ttl selects the default TTL. A
// non-empty idemKey that equals the key of the still-active lease yields
// ErrIdempotencyReplay (the token is never re-issued). The epoch is strictly
// increasing for the life of the process. On success the returned secret is
// the only copy of the capability token; the manager keeps its digest.
//
// While the workspace is in MAINTENANCE it returns ErrMaintenance; during a
// takeover or capture it returns ErrConflict (use AcquireTakeover to claim a
// ready takeover).
func (m *Manager) Acquire(holderID string, mode Mode, ttl time.Duration, idemKey string) (Lease, auth.Secret, error) {
	return m.acquire(holderID, mode, ttl, idemKey, "")
}

// AcquireTakeover grants the HUMAN lease reserved by the takeover requestID.
// It fails with ErrNotFound when requestID is not the active takeover and
// with ErrConflict until the old lease has drained (HandoffReady).
func (m *Manager) AcquireTakeover(holderID string, ttl time.Duration, idemKey, requestID string) (Lease, auth.Secret, error) {
	if requestID == "" {
		return Lease{}, "", fmt.Errorf("%w: takeover id", ErrInvalidArgument)
	}
	return m.acquire(holderID, ModeHuman, ttl, idemKey, requestID)
}

func (m *Manager) acquire(holderID string, mode Mode, ttl time.Duration, idemKey, takeoverID string) (Lease, auth.Secret, error) {
	if mode != ModeHuman && mode != ModeAgent {
		return Lease{}, "", fmt.Errorf("%w: mode", ErrInvalidArgument)
	}
	if holderID == "" || len(holderID) > maxHolderIDLen {
		return Lease{}, "", fmt.Errorf("%w: holder id", ErrInvalidArgument)
	}
	if ttl == 0 {
		ttl = m.defaultTTL
	}
	if ttl < m.minTTL || ttl > m.maxTTL {
		return Lease{}, "", ErrInvalidTTL
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shutdown {
		return Lease{}, "", ErrShutdown
	}
	m.expireLocked()
	if mode == ModeAgent {
		if err := m.agentGateLocked(); err != nil {
			return Lease{}, "", err
		}
	}
	if m.locked {
		return Lease{}, "", &LockedError{Code: m.lockCode}
	}
	if m.maintenance {
		return Lease{}, "", ErrMaintenance
	}
	if m.capture != nil {
		return Lease{}, "", ErrConflict
	}
	if h := m.handoff; h != nil {
		switch {
		case takeoverID == "":
			return Lease{}, "", ErrConflict
		case takeoverID != h.snap.RequestID:
			return Lease{}, "", ErrNotFound
		case h.snap.Status != HandoffReady:
			return Lease{}, "", ErrConflict
		}
	} else if takeoverID != "" {
		return Lease{}, "", ErrNotFound
	}
	if m.cur != nil {
		if idemKey != "" && m.cur.idemKey == idemKey {
			return Lease{}, "", ErrIdempotencyReplay
		}
		return Lease{}, "", ErrConflict
	}

	token, err := auth.NewSecret(tokenPrefix)
	if err != nil {
		return Lease{}, "", err
	}
	idSuffix, err := auth.RandomHex(16)
	if err != nil {
		return Lease{}, "", err
	}
	m.epoch++
	s := &leaseState{
		lease: Lease{
			ID:          "lse_" + idSuffix,
			WorkspaceID: m.workspaceID,
			HolderID:    holderID,
			Mode:        mode,
			Epoch:       m.epoch,
			Generation:  m.generation,
			ExpiresAt:   m.clock.Now().Add(ttl),
		},
		digest:  auth.DigestOf(token.Reveal()),
		ttl:     ttl,
		idemKey: idemKey,
		done:    make(chan struct{}),
		idle:    make(chan struct{}),
	}
	m.cur = s
	m.clearHandoffLocked()
	m.armLocked(s)
	return s.lease, token, nil
}

// Renew extends the lease identified by token and leaseID to now plus its
// original TTL. epoch and generation must match the lease. While the
// workspace is locked it returns *LockedError, because ERROR_LOCKED forbids
// all control and a lease held at lock time has already ended.
func (m *Manager) Renew(token, leaseID string, epoch uint64, generation string) (Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.locked && !m.shutdown {
		return Lease{}, &LockedError{Code: m.lockCode}
	}
	s, err := m.lookupLocked(token)
	if err != nil {
		return Lease{}, err
	}
	if err := m.checkAgentUseLocked(s); err != nil {
		return Lease{}, err
	}
	if s.lease.ID != leaseID {
		return Lease{}, ErrNotFound
	}
	if s.lease.Epoch != epoch {
		return Lease{}, ErrEpochMismatch
	}
	if s.lease.Generation != generation {
		return Lease{}, ErrGenerationMismatch
	}
	s.lease.ExpiresAt = m.clock.Now().Add(s.ttl)
	m.armLocked(s)
	return s.lease, nil
}

// Identify returns the current lease for token.
func (m *Manager) Identify(token string) (Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.lookupLocked(token)
	if err != nil {
		return Lease{}, err
	}
	if err := m.checkAgentUseLocked(s); err != nil {
		return Lease{}, err
	}
	return s.lease, nil
}

// Release ends the current lease if its ID is leaseID (reason EndReleased).
// During CAPTURE, releasing the ID of the fenced lease cancels its
// restoration.
func (m *Manager) Release(leaseID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shutdown {
		return ErrShutdown
	}
	m.expireLocked()
	if m.cur == nil || m.cur.lease.ID != leaseID {
		if m.dropPriorLocked(leaseID) {
			return nil
		}
		return ErrNotFound
	}
	m.endLocked(EndReleased)
	return nil
}

// Relinquish is Release on behalf of the holder: token must belong to the
// current lease (or, during CAPTURE, to the fenced lease that is about to be
// restored) and leaseID must name it. A holder that relinquishes a fenced
// lease is never restored.
func (m *Manager) Relinquish(token, leaseID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shutdown {
		return ErrShutdown
	}
	m.expireLocked()
	d := auth.DigestOf(token)
	if s := m.cur; s != nil && subtle.ConstantTimeCompare(s.digest[:], d[:]) == 1 {
		if s.lease.ID != leaseID {
			return ErrNotFound
		}
		m.endLocked(EndReleased)
		return nil
	}
	if c := m.capture; c != nil && c.prior != nil && subtle.ConstantTimeCompare(c.prior.digest[:], d[:]) == 1 {
		if c.prior.lease.ID != leaseID {
			return ErrNotFound
		}
		// Keep the prior lease for reporting; only its restoration is canceled.
		c.prior.released = true
		return nil
	}
	if _, err := m.lookupLocked(token); err != nil {
		return err
	}
	return ErrNotFound
}

// dropPriorLocked cancels the restoration of the fenced lease leaseID,
// keeping it as the capture's prior lease for reporting.
func (m *Manager) dropPriorLocked(leaseID string) bool {
	if c := m.capture; c != nil && c.prior != nil && c.prior.lease.ID == leaseID {
		c.prior.released = true
		return true
	}
	return false
}

// Authorize admits a request or connection holding token for the wanted mode.
// ERROR_LOCKED forbids all control: SetLocked(true) ends the held lease, so
// while the workspace is locked no token is live and a token issued before
// the lock yields *EndedError (EndLocked).
func (m *Manager) Authorize(token string, want Mode) (Ticket, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.lookupLocked(token)
	if err != nil {
		return Ticket{}, err
	}
	if err := m.checkAgentUseLocked(s); err != nil {
		return Ticket{}, err
	}
	if s.lease.Mode != want {
		return Ticket{}, ErrWrongMode
	}
	return Ticket{lease: s.lease, state: s}, nil
}

// AuthorizeTracked is Authorize plus registration of an in-flight use. The
// returned release must be called exactly when the request or connection is
// finished (it is idempotent). A takeover, capture or maintenance entry
// waits for every tracked use of the lease it fences, so no connection of the
// old holder can still be writing when a new grant is issued.
func (m *Manager) AuthorizeTracked(token string, want Mode) (Ticket, func(), error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.lookupLocked(token)
	if err != nil {
		return Ticket{}, nil, err
	}
	if err := m.checkAgentUseLocked(s); err != nil {
		return Ticket{}, nil, err
	}
	if s.lease.Mode != want {
		return Ticket{}, nil, ErrWrongMode
	}
	s.active++
	var once sync.Once
	release := func() {
		once.Do(func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			s.active--
			m.closeIdleLocked(s)
		})
	}
	return Ticket{lease: s.lease, state: s}, release, nil
}

// StartGeneration ends the current lease with EndGeneration and begins a new
// runtime generation, which it returns. A takeover reservation and the
// restoration of a fenced capture holder belong to the old generation and are
// dropped. After Shutdown it changes nothing and returns the last generation.
func (m *Manager) StartGeneration() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shutdown {
		return m.generation
	}
	m.endLocked(EndGeneration)
	m.clearHandoffLocked()
	// A capture's prior lease is not restored across a generation change
	// (endCapture checks the generation); it stays reported as the prior
	// lease of that capture.
	m.generation = newUUID()
	return m.generation
}

// SetLocked blocks (or unblocks) new acquisitions, renewals and
// authorizations. Locking ends a held lease with EndLocked, which closes the
// done channel of its tickets, and drops any takeover reservation or capture
// restoration. An empty code defaults to DefaultLockCode; the code is cleared
// when unlocking.
func (m *Manager) SetLocked(locked bool, code string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.locked = locked
	switch {
	case !locked:
		m.lockCode = ""
	case code == "":
		m.lockCode = DefaultLockCode
	default:
		m.lockCode = code
	}
	if locked {
		m.endLocked(EndLocked)
		m.clearHandoffLocked()
		// A locked workspace restores nothing (endCapture checks m.locked);
		// the fenced lease stays reported as the capture's prior lease.
	}
}

// Status returns a snapshot of the workspace control state.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.shutdown {
		m.expireLocked()
	}
	return m.statusLocked()
}

func (m *Manager) statusLocked() Status {
	st := Status{
		WorkspaceID:  m.workspaceID,
		Generation:   m.generation,
		State:        StateIdle,
		Locked:       m.locked,
		LockCode:     m.lockCode,
		Capture:      m.capture != nil,
		Maintenance:  m.maintenance,
		AgentControl: m.agent,
	}
	if s := m.cur; s != nil {
		l := s.lease
		holder := l.HolderID
		st.Lease = &l
		st.HolderID = &holder
		if l.Mode == ModeHuman {
			st.State = StateHuman
		} else {
			st.State = StateAgent
		}
	}
	if h := m.handoff; h != nil {
		snap := h.snap
		if snap.Prior != nil {
			p := *snap.Prior
			snap.Prior = &p
		}
		st.Handoff = &snap
		st.State = StateHandoffPending
	}
	if m.capture != nil {
		st.State = StateCapture
	}
	if m.maintenance {
		st.State = StateMaintenance
	}
	if m.locked {
		st.State = StateErrorLocked
	}
	return st
}

// Shutdown ends the current lease with EndShutdown. Afterwards every method
// that takes a credential or lease fails with ErrShutdown. It is idempotent.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shutdown {
		return
	}
	m.endLocked(EndShutdown)
	m.clearHandoffLocked()
	// A capture cannot restore anything after shutdown (endCapture checks
	// m.shutdown); its prior lease is kept for the final report.
	m.shutdown = true
	close(m.done)
}

// BeginHandoff starts a human takeover. The current lease, if any, is fenced
// at once: its tickets close and no further grant is possible until every
// tracked connection of it has drained (or force accepts the risk). The
// returned snapshot says whether the drain already completed. A call with the
// idemKey of the active takeover returns it unchanged; force on a
// DRAIN_TIMEOUT takeover makes it READY (Forced). Any other call while a
// takeover is active is ErrConflict.
func (m *Manager) BeginHandoff(idemKey string, force bool) (Handoff, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shutdown {
		return Handoff{}, ErrShutdown
	}
	m.expireLocked()
	if m.locked {
		return Handoff{}, &LockedError{Code: m.lockCode}
	}
	if m.maintenance {
		return Handoff{}, ErrMaintenance
	}
	if m.capture != nil {
		return Handoff{}, ErrConflict
	}
	if h := m.handoff; h != nil {
		switch {
		case idemKey != "" && h.idemKey == idemKey:
			return h.snapshot(), nil
		case force && h.snap.Status == HandoffDrainTimeout:
			h.snap.Status = HandoffReady
			h.snap.Forced = true
			return h.snapshot(), nil
		}
		return Handoff{}, ErrConflict
	}

	suffix, err := auth.RandomHex(16)
	if err != nil {
		return Handoff{}, err
	}
	h := &handoffState{
		snap:      Handoff{RequestID: "tko_" + suffix, Status: HandoffReady, Drained: true, ExpiresAt: m.clock.Now().Add(m.handoffTTL)},
		idemKey:   idemKey,
		autoForce: force,
	}
	var fenced *leaseState
	if s := m.cur; s != nil {
		h.snap.Prior = &PriorLease{LeaseID: s.lease.ID, HolderID: s.lease.HolderID, Mode: s.lease.Mode}
		h.snap.UncertainEffects = s.lease.Mode == ModeAgent
		fenced = s
		m.endLocked(EndTakeover)
		if s.active > 0 {
			h.snap.Status = HandoffDraining
			h.snap.Drained = false
		}
	}
	m.handoff = h
	h.timer = m.clock.AfterFunc(m.handoffTTL, func() { m.onHandoffExpire(h) })
	if h.snap.Status == HandoffDraining {
		go m.awaitDrain(h, fenced)
	}
	return h.snapshot(), nil
}

func (h *handoffState) snapshot() Handoff {
	snap := h.snap
	if snap.Prior != nil {
		p := *snap.Prior
		snap.Prior = &p
	}
	return snap
}

// awaitDrain settles a DRAINING takeover when the fenced lease drains or the
// drain limit passes.
func (m *Manager) awaitDrain(h *handoffState, fenced *leaseState) {
	timedOut := make(chan struct{})
	t := m.clock.AfterFunc(m.drainTimeout, func() { close(timedOut) })
	defer t.Stop()
	select {
	case <-fenced.idle:
		m.settleDrain(h, true)
	case <-timedOut:
		m.settleDrain(h, false)
	}
}

func (m *Manager) settleDrain(h *handoffState, drained bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.handoff != h || h.snap.Status != HandoffDraining {
		return
	}
	switch {
	case drained:
		h.snap.Status = HandoffReady
		h.snap.Drained = true
	case h.autoForce:
		h.snap.Status = HandoffReady
		h.snap.Forced = true
	default:
		h.snap.Status = HandoffDrainTimeout
	}
}

func (m *Manager) onHandoffExpire(h *handoffState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.handoff == h {
		m.handoff = nil
	}
}

// Handoff returns the active takeover.
func (m *Manager) Handoff() (Handoff, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.handoff == nil {
		return Handoff{}, false
	}
	return m.handoff.snapshot(), true
}

// CancelHandoff drops the active takeover. The fenced lease is not restored.
func (m *Manager) CancelHandoff() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.handoff == nil {
		return ErrNotFound
	}
	m.clearHandoffLocked()
	return nil
}

func (m *Manager) clearHandoffLocked() {
	if m.handoff != nil {
		if m.handoff.timer != nil {
			m.handoff.timer.Stop()
		}
		m.handoff = nil
	}
}

// CaptureOutcome says how a CAPTURE hold ended.
type CaptureOutcome struct {
	// Prior is the lease the capture fenced, if any.
	Prior *PriorLease
	// Restored is the prior holder's new lease (same capability token, new
	// ID and new epoch). It is nil when no lease was fenced, the holder
	// released it, its TTL ran out during the capture, or the generation
	// changed.
	Restored *Lease
	// State is the workspace control state after the capture.
	State State
}

// CaptureSession is an active CAPTURE hold.
type CaptureSession struct {
	m *Manager
	c *captureState
	// Prior is the lease the capture fenced, if any.
	Prior *PriorLease
}

// BeginCapture enters CAPTURE. A held lease is fenced (its tickets close);
// that requires allowInterrupt, and expectedLeaseID, when non-empty, must name
// it (ErrLeaseMismatch otherwise, also when no lease is held). A takeover,
// maintenance or another capture yields ErrConflict / ErrMaintenance. The hold
// ends when End is called or after CaptureTimeout, whichever is first.
func (m *Manager) BeginCapture(expectedLeaseID string, allowInterrupt bool) (*CaptureSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shutdown {
		return nil, ErrShutdown
	}
	m.expireLocked()
	if m.locked {
		return nil, &LockedError{Code: m.lockCode}
	}
	if m.maintenance {
		return nil, ErrMaintenance
	}
	if m.capture != nil || m.handoff != nil {
		return nil, ErrConflict
	}
	if m.cur == nil && expectedLeaseID != "" {
		return nil, ErrLeaseMismatch
	}
	c := &captureState{}
	sess := &CaptureSession{m: m, c: c}
	if s := m.cur; s != nil {
		if expectedLeaseID != "" && s.lease.ID != expectedLeaseID {
			return nil, ErrLeaseMismatch
		}
		if !allowInterrupt {
			return nil, ErrInterruptNotAllowed
		}
		c.prior = &priorState{lease: s.lease, digest: s.digest, ttl: s.ttl, idemKey: s.idemKey}
		c.fenced = s
		sess.Prior = &PriorLease{LeaseID: s.lease.ID, HolderID: s.lease.HolderID, Mode: s.lease.Mode}
		m.endLocked(EndCapture)
	}
	m.capture = c
	c.timer = m.clock.AfterFunc(m.captureTimeout, func() { m.endCapture(c) })
	return sess, nil
}

// WaitDrained waits until every tracked connection of the fenced lease is
// gone. It reports false when ctx ends first. With no fenced lease it
// returns true at once.
func (s *CaptureSession) WaitDrained(ctx context.Context) bool {
	if s.c.fenced == nil {
		return true
	}
	select {
	case <-s.c.fenced.idle:
		return true
	case <-ctx.Done():
		return false
	}
}

// End leaves CAPTURE. If the fenced lease was neither released nor expired,
// the generation is unchanged and every connection of the fenced lease is
// gone, its holder is restored with the same capability token at a NEW epoch
// and lease ID, with a full TTL from now; the pre-capture epoch is never
// reused. Otherwise the workspace returns to IDLE. End is idempotent: later
// calls return the first outcome.
func (s *CaptureSession) End() CaptureOutcome { return s.m.endCapture(s.c) }

func (m *Manager) endCapture(c *captureState) CaptureOutcome {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c.outcome != nil {
		return *c.outcome
	}
	if m.capture == c {
		m.capture = nil
	}
	if c.timer != nil {
		c.timer.Stop()
	}
	out := CaptureOutcome{}
	if p := c.prior; p != nil {
		out.Prior = &PriorLease{LeaseID: p.lease.ID, HolderID: p.lease.HolderID, Mode: p.lease.Mode}
		drained := c.fenced == nil || closed(c.fenced.idle)
		if !p.released && drained && !m.shutdown && !m.locked && m.cur == nil &&
			(p.lease.Mode != ModeAgent || m.agentGateLocked() == nil) &&
			p.lease.Generation == m.generation && m.clock.Now().Before(p.lease.ExpiresAt) {
			if suffix, err := auth.RandomHex(16); err == nil {
				m.epoch++
				l := p.lease
				l.ID = "lse_" + suffix
				l.Epoch = m.epoch
				l.ExpiresAt = m.clock.Now().Add(p.ttl)
				s := &leaseState{
					lease:      l,
					digest:     p.digest,
					epochBound: true,
					ttl:        p.ttl,
					idemKey:    p.idemKey,
					done:       make(chan struct{}),
					idle:       make(chan struct{}),
				}
				m.cur = s
				m.armLocked(s)
				restored := l
				out.Restored = &restored
			}
		}
	}
	out.State = m.statusLocked().State
	c.outcome = &out
	return out
}

// Drain lets a caller wait for a fenced lease's connections to close.
type Drain struct{ idle <-chan struct{} }

// Wait reports whether every tracked connection of the fenced lease is gone
// before ctx ends. A zero Drain (nothing was fenced) is already drained.
func (d Drain) Wait(ctx context.Context) bool {
	if d.idle == nil {
		return true
	}
	select {
	case <-d.idle:
		return true
	case <-ctx.Done():
		return false
	}
}

// EnterMaintenance fences the current lease and blocks every acquisition until
// ExitMaintenance. The workspace stays in MAINTENANCE across browser
// restarts; a runtime restart starts IDLE. It is idempotent. The returned
// Drain tells whether the fenced lease's connections have closed.
func (m *Manager) EnterMaintenance() (Drain, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shutdown {
		return Drain{}, ErrShutdown
	}
	m.expireLocked()
	if m.capture != nil {
		return Drain{}, ErrConflict
	}
	if m.maintenance {
		return Drain{}, nil
	}
	var d Drain
	if s := m.cur; s != nil {
		d = Drain{idle: s.idle}
		m.endLocked(EndMaintenance)
	}
	m.clearHandoffLocked()
	m.maintenance = true
	return d, nil
}

// EnterMaintenanceIfIdle enters MAINTENANCE only when the control domain is
// idle for the exact runtime generation expectedGeneration, and otherwise
// changes nothing. Unlike EnterMaintenance it never fences a lease: a live
// HUMAN or AGENT lease, a takeover, a CAPTURE hold and MAINTENANCE itself are
// refused with ErrConflict / ErrMaintenance, and the generation, the active
// token, the takeover and the capture restoration stay exactly as they were.
// An expired lease is ended first, with the normal expiry semantics. An empty
// expectedGeneration is ErrInvalidArgument and a different current generation
// is ErrGenerationMismatch. The check and the entry happen under one lock, so
// a concurrent Acquire either wins (the entry is refused) or loses (it gets
// ErrMaintenance); there is never both.
//
// This guards the control domain only. It says nothing about browser pages,
// downloads or noVNC viewers: the caller has already accepted losing them.
// Nothing was fenced, so the returned Drain is already drained.
func (m *Manager) EnterMaintenanceIfIdle(expectedGeneration string) (Drain, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shutdown {
		return Drain{}, ErrShutdown
	}
	if expectedGeneration == "" {
		return Drain{}, fmt.Errorf("%w: expected generation is empty", ErrInvalidArgument)
	}
	m.expireLocked()
	if m.generation != expectedGeneration {
		return Drain{}, ErrGenerationMismatch
	}
	if m.maintenance {
		return Drain{}, ErrMaintenance
	}
	if m.capture != nil || m.handoff != nil || m.cur != nil {
		return Drain{}, ErrConflict
	}
	m.maintenance = true
	return Drain{}, nil
}

// ExitMaintenance leaves MAINTENANCE. ErrNotFound when not in maintenance.
func (m *Manager) ExitMaintenance() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shutdown {
		return ErrShutdown
	}
	if !m.maintenance {
		return ErrNotFound
	}
	m.maintenance = false
	return nil
}

// lookupLocked resolves token to the active lease. A token of a lease that
// ended recently yields *EndedError, an unknown token ErrUnauthenticated.
func (m *Manager) lookupLocked(token string) (*leaseState, error) {
	if m.shutdown {
		return nil, ErrShutdown
	}
	m.expireLocked()
	d := auth.DigestOf(token)
	if s := m.cur; s != nil && subtle.ConstantTimeCompare(s.digest[:], d[:]) == 1 {
		return s, nil
	}
	for i := range m.endedCount {
		e := &m.ended[i]
		if subtle.ConstantTimeCompare(e.digest[:], d[:]) == 1 {
			if e.reason == EndAgentPolicy {
				// Report the current policy rather than a stale reason: an
				// agent whose lease was fenced by a pause that has since
				// been resumed must see a plain ended lease, not "paused".
				if err := m.agentGateLocked(); err != nil {
					return nil, err
				}
			}
			return nil, &EndedError{Reason: e.reason}
		}
	}
	return nil, ErrUnauthenticated
}

// expireLocked ends the current lease if its deadline has passed. It covers
// the window in which the clock timer is late or has not run yet.
func (m *Manager) expireLocked() {
	if s := m.cur; s != nil && !m.clock.Now().Before(s.lease.ExpiresAt) {
		m.endLocked(EndExpired)
	}
}

// armLocked (re)schedules the expiry timer of s for its current deadline.
func (m *Manager) armLocked(s *leaseState) {
	if s.timer != nil {
		s.timer.Stop()
	}
	s.timerSeq++
	seq := s.timerSeq
	d := s.lease.ExpiresAt.Sub(m.clock.Now())
	s.timer = m.clock.AfterFunc(d, func() { m.onTimer(s, seq) })
}

func (m *Manager) onTimer(s *leaseState, seq uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur != s || s.timerSeq != seq {
		return // renewed, or already ended
	}
	m.endLocked(EndExpired)
}

// endLocked ends the current lease, if any. m.cur is cleared first, so the
// done channel is closed at most once per lease.
func (m *Manager) endLocked(reason EndReason) {
	s := m.cur
	if s == nil {
		return
	}
	m.cur = nil
	if s.timer != nil {
		s.timer.Stop()
	}
	s.reason.Store(&reason)
	m.ended[m.endedNext] = endedToken{digest: s.digest, reason: reason}
	m.endedNext = (m.endedNext + 1) % endedRingSize
	if m.endedCount < endedRingSize {
		m.endedCount++
	}
	s.ended = true
	close(s.done)
	m.closeIdleLocked(s)
}

// closeIdleLocked closes s.idle once the lease has ended and no tracked use
// remains.
func (m *Manager) closeIdleLocked(s *leaseState) {
	if s.ended && s.active == 0 && !s.idleClosed {
		s.idleClosed = true
		close(s.idle)
	}
}

// closed reports whether ch has been closed (read-only, no lock needed).
func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// newUUID returns a random RFC 4122 version 4 UUID. crypto/rand.Read never
// returns an error (it aborts the process instead), so none is surfaced.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("lease: crypto/rand failed: " + err.Error())
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:], b[10:])
	return string(out[:])
}
