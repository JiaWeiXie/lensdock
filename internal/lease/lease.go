// Package lease implements the single-holder control lease state machine of a
// workspace. It has no HTTP or WebSocket dependencies: transports obtain a
// Ticket per admitted request or connection and close it when Ticket.Done is
// closed.
//
// Besides HUMAN and AGENT leases the manager owns the transient control
// states of SPEC §9: HANDOFF_PENDING (takeover), CAPTURE (page-viewport
// capture with restoration of the prior holder at a new epoch) and
// MAINTENANCE (host operations).
package lease

import (
	"errors"
	"sync/atomic"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
	"github.com/JiaWeiXie/lensdock/internal/auth"
)

// Mode is the kind of control a lease grants. The values are the wire values.
type Mode string

const (
	ModeHuman Mode = "HUMAN"
	ModeAgent Mode = "AGENT"
)

// State is the workspace control state. The values are the openapi
// controlState wire values.
type State string

const (
	StateIdle           State = "IDLE"
	StateHuman          State = "HUMAN"
	StateAgent          State = "AGENT"
	StateHandoffPending State = "HANDOFF_PENDING"
	StateCapture        State = "CAPTURE"
	StateMaintenance    State = "MAINTENANCE"
	StateErrorLocked    State = "ERROR_LOCKED"
)

// EndReason says why a lease ended.
type EndReason string

const (
	EndReleased   EndReason = "RELEASED"
	EndExpired    EndReason = "EXPIRED"
	EndGeneration EndReason = "GENERATION"
	EndShutdown   EndReason = "SHUTDOWN"
	// EndLocked means the workspace entered ERROR_LOCKED (SPEC §8) while the
	// lease was held.
	EndLocked EndReason = "LOCKED"
	// EndTakeover means a human takeover fenced the lease.
	EndTakeover EndReason = "TAKEOVER"
	// EndCapture means a page-viewport capture fenced the lease. The holder
	// may be restored at a new epoch when the capture ends.
	EndCapture EndReason = "CAPTURE"
	// EndMaintenance means the operator entered MAINTENANCE.
	EndMaintenance EndReason = "MAINTENANCE"
	// EndAgentPolicy means a human paused the Agent or switched the
	// workspace to NOVNC mode while the AGENT lease was held.
	EndAgentPolicy EndReason = "AGENT_POLICY"
)

// Lease is an immutable snapshot of a granted lease.
type Lease struct {
	ID          string
	WorkspaceID string
	HolderID    string
	Mode        Mode
	Epoch       uint64
	Generation  string
	ExpiresAt   time.Time
}

// PriorLease identifies a lease that a takeover or capture fenced. It never
// carries the capability token.
type PriorLease struct {
	LeaseID  string
	HolderID string
	Mode     Mode
}

// HandoffStatus is the progress of a takeover.
type HandoffStatus string

const (
	// HandoffDraining means the old lease is fenced and its in-flight
	// connections are still closing.
	HandoffDraining HandoffStatus = "DRAINING"
	// HandoffReady means every connection of the old lease is gone (or the
	// operator accepted the risk with force): a HUMAN lease may be granted.
	HandoffReady HandoffStatus = "READY"
	// HandoffDrainTimeout means the drain limit passed with connections still
	// open; the takeover waits for an explicit force.
	HandoffDrainTimeout HandoffStatus = "DRAIN_TIMEOUT"
)

// Handoff is a snapshot of the active takeover.
type Handoff struct {
	RequestID string
	Status    HandoffStatus
	// Drained is true when every connection of the fenced lease closed.
	Drained bool
	// Forced is true when the operator accepted a takeover whose drain could
	// not be confirmed.
	Forced bool
	// UncertainEffects is true when an AGENT lease was fenced: ending the
	// connections cannot undo browser or server side effects already
	// started (SPEC §9.3).
	UncertainEffects bool
	Prior            *PriorLease
	ExpiresAt        time.Time
}

// Status is a snapshot of the workspace control state.
type Status struct {
	WorkspaceID string
	Generation  string
	State       State
	HolderID    *string
	Locked      bool
	LockCode    string
	Lease       *Lease
	// Handoff is the active takeover, if any.
	Handoff *Handoff
	// Capture is true while a page-viewport capture holds the workspace.
	Capture bool
	// Maintenance is true between EnterMaintenance and ExitMaintenance.
	Maintenance bool
	// AgentControl is the authoritative usage mode and pause policy.
	AgentControl apiwire.AgentControl
}

// Ticket is issued by Manager.Authorize, one per admitted request or
// connection. All tickets of a lease share one Done channel that is closed
// exactly once when the lease ends for any reason.
type Ticket struct {
	lease Lease
	state *leaseState
}

// Lease returns the lease as it was when the ticket was issued.
func (t Ticket) Lease() Lease { return t.lease }

// Done is closed when the lease ends. It is nil for a zero Ticket.
func (t Ticket) Done() <-chan struct{} {
	if t.state == nil {
		return nil
	}
	return t.state.done
}

// EpochBound reports whether the lease was restored after a capture. Its
// capability token is the one the holder already had, so a request must
// present the lease's new epoch (X-LensDock-Lease-Epoch) for the use to be
// admitted; a client that kept reconnecting with its pre-capture state does
// not. Transports enforce this with the headers described in the runtime
// wire contract.
func (t Ticket) EpochBound() bool {
	return t.state != nil && t.state.epochBound
}

// Reason returns why the lease ended. It is empty until Done is closed.
func (t Ticket) Reason() EndReason {
	if t.state == nil {
		return ""
	}
	if r := t.state.reason.Load(); r != nil {
		return *r
	}
	return ""
}

// leaseState is the mutable part of a lease shared by its tickets. All fields
// except done and reason are guarded by Manager.mu.
type leaseState struct {
	lease    Lease
	digest   auth.Digest
	ttl      time.Duration
	idemKey  string
	timer    Timer
	timerSeq uint64
	done     chan struct{}
	reason   atomic.Pointer[EndReason]

	// active counts in-flight uses registered with AuthorizeTracked. idle is
	// closed once the lease has ended and active is zero: the lease's
	// connections are drained.
	active     int
	ended      bool
	idle       chan struct{}
	idleClosed bool
	// epochBound marks a lease restored after CAPTURE: its token is the
	// pre-capture token, so use of it must present the new epoch.
	epochBound bool
}

// Timer is a cancelable pending callback.
type Timer interface{ Stop() bool }

// Clock abstracts time so expiry can be tested deterministically. The
// callback passed to AfterFunc must be run without holding any lock that is
// also held while calling AfterFunc or Timer.Stop.
type Clock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) Timer
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func (systemClock) AfterFunc(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }

// SystemClock returns a Clock backed by the time package.
func SystemClock() Clock { return systemClock{} }

// Sentinel errors returned by Manager methods.
var (
	ErrUnauthenticated    = errors.New("lease: unauthenticated")
	ErrWrongMode          = errors.New("lease: wrong lease mode")
	ErrConflict           = errors.New("lease: workspace already has an active lease")
	ErrInvalidTTL         = errors.New("lease: ttl out of range")
	ErrInvalidArgument    = errors.New("lease: invalid argument")
	ErrEpochMismatch      = errors.New("lease: epoch mismatch")
	ErrGenerationMismatch = errors.New("lease: runtime generation mismatch")
	ErrIdempotencyReplay  = errors.New("lease: idempotency key already used by the active lease")
	ErrNotFound           = errors.New("lease: not found")
	ErrShutdown           = errors.New("lease: manager shut down")
	// ErrMaintenance is returned while the workspace is in MAINTENANCE.
	ErrMaintenance = errors.New("lease: workspace is in maintenance")
	// ErrInterruptNotAllowed is returned by BeginCapture when a lease is held
	// and the caller did not accept interrupting it.
	ErrInterruptNotAllowed = errors.New("lease: capture would interrupt the current lease")
	// ErrLeaseMismatch is returned when the lease a caller expected is not
	// the one currently held.
	ErrLeaseMismatch = errors.New("lease: expected lease is not the current lease")
	// ErrAgentDisabled is returned for AGENT control while the workspace is
	// in NOVNC mode.
	ErrAgentDisabled = errors.New("lease: agent mode is disabled")
	// ErrAgentPaused is returned for AGENT control while the Agent is paused.
	ErrAgentPaused = errors.New("lease: agent is paused")
	// ErrPolicyPersist is returned when the usage policy could not be saved.
	// For a restrictive change (pause, NOVNC) the restriction is nevertheless
	// in force in memory; for a relaxing change nothing changed.
	ErrPolicyPersist = errors.New("lease: agent policy could not be saved")
	// ErrTooManyWatchers is returned when the policy watcher limit is reached.
	ErrTooManyWatchers = errors.New("lease: too many policy watchers")
)

// LockedError is returned while the workspace is locked (not ready). Code is
// the wire error code, e.g. RUNTIME_NOT_READY or SANDBOX_PREFLIGHT_FAILED.
// The manager returns it as *LockedError.
type LockedError struct{ Code string }

func (e *LockedError) Error() string { return "lease: workspace locked: " + e.Code }

// EndedError is returned for the token of a recently ended lease instead of
// ErrUnauthenticated. The manager returns it as *EndedError.
type EndedError struct{ Reason EndReason }

func (e *EndedError) Error() string { return "lease: lease ended: " + string(e.Reason) }
