package hostmanager

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"time"
)

// Operation kinds understood by Backend.Execute.
const (
	KindStart  = "start"
	KindStop   = "stop"
	KindUpdate = "update"
)

// Operation phases. Queued, admitted, running and blocked operations are
// unresolved: they hold the project's lifecycle and its reservation.
const (
	PhaseQueued           = "queued"
	PhaseAdmitted         = "admitted"
	PhaseRunning          = "running"
	PhaseSucceeded        = "succeeded"
	PhaseFailed           = "failed"
	PhaseCanceled         = "canceled"
	PhaseBlocked          = "blocked"
	PhaseAwaitingApproval = "awaiting-approval"
	PhaseAbandoned        = "abandoned"
)

// Project and observation states.
const (
	StateStopped  = "stopped"
	StateRunning  = "running"
	StatePartial  = "partial"
	StateUnknown  = "unknown"
	StateQueued   = "queued"
	StateStarting = "starting"
	StateStopping = "stopping"
	StateUpdating = "updating"
	StateBlocked  = "blocked"
)

// Request actions.
const (
	ActionAuthorize = "authorize"
	ActionList      = "list"
	ActionRelocate  = "relocate"
	ActionRevoke    = "revoke"
	ActionPolicy    = "policy"
	ActionRecover   = "recover"
	ActionEnsure    = "ensure"
	ActionStatus    = "status"
	ActionCancel    = "cancel"
	ActionHeartbeat = "heartbeat"
	ActionStop      = "stop"
	ActionUpdate    = "update"
	ActionConnect   = "connect"
)

// Update action kinds.
const (
	UpdateApply    = "apply"
	UpdateApprove  = "approve"
	UpdateAbort    = "abort"
	UpdateRollback = "rollback"
	UpdatePrune    = "prune"
	UpdateDelete   = "delete"
	UpdateVerify   = "verify"
)

const maxTerminalOps = 256

func unresolvedPhase(phase string) bool {
	switch phase {
	case PhaseQueued, PhaseAdmitted, PhaseRunning, PhaseBlocked:
		return true
	}
	return false
}

// workerPhase reports phases in which a worker may own the project.
func workerPhase(phase string) bool { return phase == PhaseAdmitted || phase == PhaseRunning }

// registry is the versioned, atomically replaced authority for grants,
// identities, operations, queue order, waiters and accounting.
type registry struct {
	Version  int                       `json:"version"`
	Revision uint64                    `json:"revision"`
	NextSeq  uint64                    `json:"nextSeq"`
	Projects map[string]*projectRecord `json:"projects"`
	Ops      map[string]*opRecord      `json:"ops"`
	Waiters  map[string]*waiterRecord  `json:"waiters"`
}

type projectRecord struct {
	ID        string            `json:"id"`
	Root      string            `json:"root"`
	Directory DirectoryIdentity `json:"directory"`
	Revoked   bool              `json:"revoked"`
	Port      int               `json:"port"`
	Origin    string            `json:"origin"`
	// TokenHash is the SHA-256 of the project capability, empty once revoked.
	TokenHash string    `json:"tokenHash,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	Limits    Limits    `json:"limits"`
	// Reservation is the declared peak held for an unresolved or recent
	// operation; accounting also considers Enforced and the observed state.
	Reservation     Resources `json:"reservation"`
	Observed        string    `json:"observed"`
	ObservedBlocker string    `json:"observedBlocker,omitempty"`
	Enforced        Resources `json:"enforced"`
	// SettledRevision is the registry revision at which the latest operation
	// finished. Observations that began earlier cannot release the reservation.
	SettledRevision uint64 `json:"settledRevision"`
	LastOp          string `json:"lastOp,omitempty"`
}

type opRecord struct {
	Operation
	Seq        uint64    `json:"seq"`
	Deadline   time.Time `json:"deadline,omitempty"`
	Project    Project   `json:"project"`
	Target     Resources `json:"target"`
	AdmittedAt time.Time `json:"admittedAt,omitempty"`
	LaunchedAt time.Time `json:"launchedAt,omitempty"`
	FinishedAt time.Time `json:"finishedAt,omitempty"`
	Attempts   int       `json:"attempts,omitempty"`
}

type waiterRecord struct {
	ID          string    `json:"id"`
	ProjectID   string    `json:"projectId"`
	OperationID string    `json:"operationId"`
	Deadline    time.Time `json:"deadline"`
}

func newRegistry() *registry {
	return &registry{
		Version:  registryVersion,
		Projects: map[string]*projectRecord{},
		Ops:      map[string]*opRecord{},
		Waiters:  map[string]*waiterRecord{},
	}
}

func (r *registry) clone() *registry {
	data, err := json.Marshal(r)
	if err != nil {
		panic(fmt.Sprintf("hostmanager: encode registry: %v", err))
	}
	out := &registry{}
	if err := json.Unmarshal(data, out); err != nil {
		panic(fmt.Sprintf("hostmanager: decode registry: %v", err))
	}
	out.normalize()
	return out
}

func (r *registry) normalize() {
	if r.Projects == nil {
		r.Projects = map[string]*projectRecord{}
	}
	if r.Ops == nil {
		r.Ops = map[string]*opRecord{}
	}
	if r.Waiters == nil {
		r.Waiters = map[string]*waiterRecord{}
	}
}

func (r *registry) validate() error {
	if r.Version != registryVersion {
		return fmt.Errorf("unsupported registry version %d", r.Version)
	}
	for id, p := range r.Projects {
		if p == nil || p.ID != id || !projectPattern.MatchString(id) {
			return fmt.Errorf("registry project %q is malformed", id)
		}
	}
	for id, op := range r.Ops {
		if op == nil || op.ID != id || !opPattern.MatchString(id) || r.Projects[op.ProjectID] == nil {
			return fmt.Errorf("registry operation %q is malformed", id)
		}
	}
	for id, w := range r.Waiters {
		if w == nil || w.ID != id || r.Ops[w.OperationID] == nil || r.Projects[w.ProjectID] == nil {
			return fmt.Errorf("registry waiter %q is malformed", id)
		}
	}
	seen := map[string]string{}
	for _, op := range r.Ops {
		if !unresolvedPhase(op.Phase) {
			continue
		}
		if other, dup := seen[op.ProjectID]; dup {
			return fmt.Errorf("registry project %s has two unresolved operations (%s, %s)", op.ProjectID, other, op.ID)
		}
		seen[op.ProjectID] = op.ID
	}
	return nil
}

func registryPath(root string) string { return filepath.Join(root, registryName) }

// loadRegistry reads the registry without modifying it.
func loadRegistry(root string) (*registry, error) {
	var r registry
	if err := readPrivateJSON(registryPath(root), maxJSONFile, &r); err != nil {
		return nil, fmt.Errorf("registry: %w", err)
	}
	r.normalize()
	if err := r.validate(); err != nil {
		return nil, err
	}
	return &r, nil
}

func saveRegistry(root string, r *registry) error {
	return writeJSONAtomic(registryPath(root), r, 0o600)
}

// unresolvedOp returns the project's single unresolved operation, if any.
func (r *registry) unresolvedOp(projectID string) *opRecord {
	for _, op := range r.Ops {
		if op.ProjectID == projectID && unresolvedPhase(op.Phase) {
			return op
		}
	}
	return nil
}

// queue lists queued operations in strict FIFO order.
func (r *registry) queue() []*opRecord {
	var out []*opRecord
	for _, op := range r.Ops {
		if op.Phase == PhaseQueued {
			out = append(out, op)
		}
	}
	slices.SortFunc(out, func(a, b *opRecord) int {
		switch {
		case a.Seq < b.Seq:
			return -1
		case a.Seq > b.Seq:
			return 1
		}
		return 0
	})
	return out
}

func (r *registry) waitersOf(opID string) []*waiterRecord {
	var out []*waiterRecord
	for _, w := range r.Waiters {
		if w.OperationID == opID {
			out = append(out, w)
		}
	}
	return out
}

// effective is the capacity accounted to a project. It never drops below the
// enforced caps the backend reported, and while the project is not positively
// stopped or has a worker operation it never drops below one project pair.
func (r *registry) effective(p *projectRecord) Resources {
	out := maxRes(p.Reservation, p.Enforced)
	state := p.Observed
	if state == "" {
		state = StateStopped
	}
	if op := r.unresolvedOp(p.ID); state != StateStopped || (op != nil && op.Phase != PhaseQueued) {
		out = maxRes(out, pairOf(p.Limits))
	}
	return out
}

func (r *registry) reservedExcluding(projectID string) Resources {
	var total Resources
	for id, p := range r.Projects {
		if id != projectID {
			total = addRes(total, r.effective(p))
		}
	}
	return total
}

// pruneHistory drops the oldest finished operations beyond the cap, never an
// unresolved one and never the latest operation of a project. It returns the
// dropped IDs so their private directories can be removed.
func (r *registry) pruneHistory() []string {
	keep := map[string]bool{}
	for _, p := range r.Projects {
		keep[p.LastOp] = true
	}
	var finished []*opRecord
	for _, op := range r.Ops {
		if !unresolvedPhase(op.Phase) && !keep[op.ID] {
			finished = append(finished, op)
		}
	}
	if len(finished) <= maxTerminalOps {
		return nil
	}
	slices.SortFunc(finished, func(a, b *opRecord) int {
		switch {
		case a.Seq < b.Seq:
			return -1
		case a.Seq > b.Seq:
			return 1
		}
		return 0
	})
	var dropped []string
	for _, op := range finished[:len(finished)-maxTerminalOps] {
		if len(r.waitersOf(op.ID)) > 0 {
			continue
		}
		delete(r.Ops, op.ID)
		dropped = append(dropped, op.ID)
	}
	return dropped
}

// viewExtra carries volatile daemon state that is not persisted.
type viewExtra struct {
	reasons map[string]string
	usage   map[string]Resources
	usageOK map[string]bool
}

// buildProject renders the public view of a project record.
func buildProject(root string, r *registry, p *projectRecord, x *viewExtra) Project {
	observed := p.Observed
	if observed == "" {
		observed = StateStopped
	}
	state := observed
	blocker := ""
	opID := ""
	if observed == StatePartial || observed == StateUnknown {
		blocker = p.ObservedBlocker
	}
	if op := r.unresolvedOp(p.ID); op != nil {
		opID = op.ID
		switch op.Phase {
		case PhaseBlocked:
			state = StateBlocked
			blocker = fmt.Sprintf("operation %s needs recovery: %s", op.ID, op.Error)
		case PhaseAdmitted, PhaseRunning:
			switch op.Kind {
			case KindStop:
				state = StateStopping
			case KindUpdate:
				state = StateUpdating
			default:
				state = StateStarting
			}
		case PhaseQueued:
			if observed != StateRunning {
				state = StateQueued
			}
			if x != nil && x.reasons[op.ID] != "" {
				blocker = x.reasons[op.ID]
			}
		}
	}
	eff := r.effective(p)
	out := Project{
		ID:             p.ID,
		Root:           p.Root,
		Directory:      p.Directory,
		Revoked:        p.Revoked,
		StateDir:       projectStateDir(root, p.ID),
		ComposeProject: "ld-" + p.ID,
		Workspace:      p.ID,
		Origin:         p.Origin,
		Port:           p.Port,
		TokenFile:      projectTokenFile(root, p.ID),
		Limits:         p.Limits,
		State:          state,
		Blocker:        blocker,
		OperationID:    opID,
		Reserved:       !eff.isZero(),
		Reservation:    eff,
	}
	if x != nil {
		out.Usage = x.usage[p.ID]
		out.UsageAvailable = x.usageOK[p.ID]
	}
	if op := r.unresolvedOp(p.ID); op != nil && op.Phase == PhaseQueued {
		for i, q := range r.queue() {
			if q.ID == op.ID {
				out.QueuePosition = i + 1
			}
		}
		out.Waiters = len(r.waitersOf(op.ID))
	}
	return out
}
