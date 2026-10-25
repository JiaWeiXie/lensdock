package hostmanager

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

func cloneOperation(o Operation) Operation {
	if o.Update != nil {
		u := *o.Update
		o.Update = &u
	}
	return o
}

func opView(op *opRecord) *Operation {
	if op == nil {
		return nil
	}
	o := cloneOperation(op.Operation)
	return &o
}

// snapshotProject keeps the identity and configuration fields of a project
// view and drops volatile status, so a manifest can be compared to the
// registry copy byte for byte.
func snapshotProject(p Project) Project {
	p.State, p.Blocker, p.OperationID = "", "", ""
	p.Reserved, p.Reservation, p.Usage, p.UsageAvailable = false, Resources{}, Resources{}, false
	p.QueuePosition, p.Waiters = 0, 0
	return p
}

func cloneInventory(inv *Inventory) *Inventory {
	if inv == nil {
		return nil
	}
	out := *inv
	out.Unbounded = slices.Clone(inv.Unbounded)
	out.Projects = nil
	return &out
}

func newProjectToken() (token, hash string, err error) {
	token, err = randomHex(32)
	if err != nil {
		return "", "", err
	}
	return token, hashToken(token), nil
}

// usable refuses new work for revoked grants and for directories that are
// missing or replaced. It never retargets a project.
func (s *server) usable(pid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usableLocked(pid)
}

func (s *server) usableLocked(pid string) error {
	rec := s.reg.Projects[pid]
	if rec == nil {
		return rpcErrf(http.StatusNotFound, "unknown project")
	}
	if rec.Revoked {
		return rpcErrf(http.StatusForbidden, "the grant for project %s was revoked", pid)
	}
	if err := checkDirectory(rec); err != nil {
		return rpcErrf(http.StatusConflict, "%v", err)
	}
	return nil
}

func (s *server) authorize(path string) (Response, error) {
	real, id, err := canonicalDir(path)
	if err != nil {
		return Response{}, rpcErrf(http.StatusBadRequest, "%v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var same *projectRecord
	for _, p := range s.reg.Projects {
		switch {
		case p.Directory == id:
			same = p
		case p.Root == real:
			return Response{}, rpcErrf(http.StatusConflict, "%s was replaced after project %s was authorized; relocate or revoke that project first", real, p.ID)
		}
	}
	if same != nil {
		if same.Root != real {
			if err := checkDirectory(same); err != nil {
				return Response{}, rpcErrf(http.StatusConflict, "project %s was authorized as %s, which no longer resolves to this directory; use relocate", same.ID, same.Root)
			}
		}
		if !same.Revoked {
			project, _ := s.projectLocked(same.ID)
			return Response{Project: &project}, nil
		}
		if err := s.checkOverlapLocked(same.Root, same.ID); err != nil {
			return Response{}, err
		}
		return s.reinstateLocked(same.ID)
	}
	if err := s.checkOverlapLocked(real, ""); err != nil {
		return Response{}, err
	}
	suffix, err := randomHex(8)
	if err != nil {
		return Response{}, err
	}
	pid := suffix
	port, err := s.allocatePortLocked()
	if err != nil {
		return Response{}, rpcErrf(http.StatusConflict, "%v", err)
	}
	token, hash, err := newProjectToken()
	if err != nil {
		return Response{}, err
	}
	for _, dir := range []string{projectStateDir(s.root, pid), grantDir(s.root, pid)} {
		if err := ensurePrivateDir(dir); err != nil {
			return Response{}, err
		}
	}
	if err := writeFileAtomic(projectTokenFile(s.root, pid), []byte(token+"\n"), 0o600); err != nil {
		return Response{}, err
	}
	now := s.now()
	err = s.commitLocked(func(r *registry) error {
		r.Projects[pid] = &projectRecord{
			ID: pid, Root: real, Directory: id, Port: port,
			Origin:    fmt.Sprintf("http://127.0.0.1:%d", port),
			TokenHash: hash, CreatedAt: now,
			Limits:   s.cfg.Policy.ProjectLimits,
			Observed: StateStopped,
		}
		return nil
	})
	if err != nil {
		return Response{}, err
	}
	project, _ := s.projectLocked(pid)
	return Response{Project: &project}, nil
}

// checkOverlapLocked refuses a grant for root that overlaps any active grant
// other than exceptID, the grant being reinstated.
func (s *server) checkOverlapLocked(root, exceptID string) error {
	for _, p := range s.reg.Projects {
		if p.ID != exceptID && !p.Revoked && overlaps(p.Root, root) {
			return rpcErrf(http.StatusConflict, "%s overlaps the grant for %s (project %s); grants are exact directories", root, p.Root, p.ID)
		}
	}
	return nil
}

// reinstateLocked restores a revoked grant for the same directory with a new
// credential, keeping the project identity, port and data.
func (s *server) reinstateLocked(pid string) (Response, error) {
	token, hash, err := newProjectToken()
	if err != nil {
		return Response{}, err
	}
	if err := ensurePrivateDir(grantDir(s.root, pid)); err != nil {
		return Response{}, err
	}
	if err := writeFileAtomic(projectTokenFile(s.root, pid), []byte(token+"\n"), 0o600); err != nil {
		return Response{}, err
	}
	if err := s.commitLocked(func(r *registry) error {
		r.Projects[pid].Revoked = false
		r.Projects[pid].TokenHash = hash
		return nil
	}); err != nil {
		return Response{}, err
	}
	project, _ := s.projectLocked(pid)
	return Response{Project: &project}, nil
}

// allocatePortLocked picks the lowest range port that no project uses and that
// no foreign listener holds right now. The probe is a preference only: the
// backend's actual binding and inspected mapping stay authoritative.
func (s *server) allocatePortLocked() (int, error) {
	used := map[int]bool{}
	for _, p := range s.reg.Projects {
		used[p.Port] = true
	}
	for port := s.cfg.PortFirst; port <= s.cfg.PortLast; port++ {
		if used[port] {
			continue
		}
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			continue
		}
		_ = ln.Close()
		return port, nil
	}
	return 0, fmt.Errorf("no free port left in %d-%d", s.cfg.PortFirst, s.cfg.PortLast)
}

func (s *server) list() Response {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.reg.Projects))
	for id := range s.reg.Projects {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b string) int {
		pa, pb := s.reg.Projects[a], s.reg.Projects[b]
		if c := pa.CreatedAt.Compare(pb.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a, b)
	})
	resp := Response{Inventory: cloneInventory(s.inv)}
	for _, id := range ids {
		p, _ := s.projectLocked(id)
		resp.Projects = append(resp.Projects, p)
	}
	policy := s.cfg.Policy
	resp.Policy = &policy
	hold, refuse := s.admissionStateLocked(false)
	for _, w := range []string{hold, refuse} {
		if w != "" {
			resp.Warnings = append(resp.Warnings, w)
		}
	}
	return resp
}

func (s *server) relocate(pid, path string) (Response, error) {
	real, id, err := canonicalDir(path)
	if err != nil {
		return Response{}, rpcErrf(http.StatusBadRequest, "%v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.reg.Projects[pid]
	if rec == nil {
		return Response{}, rpcErrf(http.StatusNotFound, "unknown project")
	}
	if op := s.reg.unresolvedOp(pid); op != nil {
		return Response{}, rpcErrf(http.StatusConflict, "project has an unresolved operation (%s); relocate afterwards", op.ID)
	}
	for _, p := range s.reg.Projects {
		if p.ID == pid {
			continue
		}
		if p.Directory == id || (!p.Revoked && overlaps(p.Root, real)) {
			return Response{}, rpcErrf(http.StatusConflict, "%s overlaps project %s", real, p.ID)
		}
	}
	if err := s.commitLocked(func(r *registry) error {
		r.Projects[pid].Root = real
		r.Projects[pid].Directory = id
		return nil
	}); err != nil {
		return Response{}, err
	}
	project, _ := s.projectLocked(pid)
	return Response{Project: &project}, nil
}

// revoke removes a project's capability and cancels its queued work. Running
// containers and data stay; stop remains available to the human by ID.
func (s *server) revoke(pid string) (Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reg.Projects[pid] == nil {
		return Response{}, rpcErrf(http.StatusNotFound, "unknown project")
	}
	now := s.now()
	err := s.commitLocked(func(r *registry) error {
		rec := r.Projects[pid]
		rec.Revoked = true
		rec.TokenHash = ""
		for _, op := range r.queue() {
			if op.ProjectID == pid {
				cancelQueued(r, op, "the project grant was revoked", now)
			}
		}
		return nil
	})
	if err != nil {
		return Response{}, err
	}
	_ = os.Remove(projectTokenFile(s.root, pid))
	project, _ := s.projectLocked(pid)
	return Response{Project: &project}, nil
}

func (s *server) setPolicy(p *Policy) (Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p == nil {
		current := s.cfg.Policy
		return Response{Policy: &current}, nil
	}
	if err := validatePolicy(*p); err != nil {
		return Response{}, rpcErrf(http.StatusBadRequest, "%v", err)
	}
	if p.Helpers != s.cfg.Policy.Helpers {
		helper, busy, err := tryFlock(helperLockPath(s.root))
		if err != nil {
			return Response{}, err
		}
		if busy {
			return Response{}, rpcErrf(http.StatusConflict, "a lifecycle helper is active; change its resource allowance after the operation finishes")
		}
		defer helper.Close()
	}
	var reserved Resources
	for _, rec := range s.reg.Projects {
		reserved = addRes(reserved, s.reg.effective(rec))
	}
	if !reserved.fitsIn(p.Budget) {
		return Response{}, rpcErrf(http.StatusConflict, "current reservations exceed the new budget; stop projects first, running projects are never shrunk")
	}
	next := s.cfg
	next.Policy = *p
	if err := saveConfig(next); err != nil {
		return Response{}, err
	}
	s.cfg = next
	s.kick()
	current := s.cfg.Policy
	return Response{Policy: &current}, nil
}

func (s *server) recoverOp(pid, opID string) (Response, error) {
	if !opPattern.MatchString(opID) {
		return Response{}, rpcErrf(http.StatusBadRequest, "a valid operationId is required")
	}
	s.mu.Lock()
	op := s.reg.Ops[opID]
	if op == nil || op.ProjectID != pid {
		s.mu.Unlock()
		return Response{}, rpcErrf(http.StatusNotFound, "unknown operation for this project")
	}
	if op.Phase != PhaseBlocked && !workerPhase(op.Phase) {
		s.mu.Unlock()
		return Response{}, rpcErrf(http.StatusConflict, "operation %s is %s and needs no recovery", opID, op.Phase)
	}
	phase := op.Phase
	s.mu.Unlock()

	// A worker or a surviving child still holds the lifecycle lock when the
	// lock is busy: recovery must never overlap it.
	lock, busy, err := tryFlock(projectLockPath(s.root, pid))
	if err != nil {
		return Response{}, err
	}
	if busy {
		return Response{}, rpcErrf(http.StatusConflict, "a worker or one of its child processes still holds the project lifecycle lock; stop it first")
	}
	defer lock.Close()

	rcpt, _ := readReceipt(s.root, opID)
	s.mu.Lock()
	defer s.mu.Unlock()
	op = s.reg.Ops[opID]
	if op == nil || op.Phase != phase {
		return Response{}, rpcErrf(http.StatusConflict, "operation %s changed while recovering", opID)
	}
	now := s.now()
	err = s.commitLocked(func(r *registry) error {
		cur := r.Ops[opID]
		if rcpt != nil {
			applyReceipt(r, cur, rcpt, now)
			return nil
		}
		finishOp(r, cur, PhaseAbandoned, "abandoned by the operator after the worker ended without a receipt; verify the project before starting it again", now, true)
		rec := r.Projects[pid]
		rec.Observed = StateUnknown
		rec.ObservedBlocker = "recovered operation " + opID + "; waiting for a fresh inspection"
		return nil
	})
	if err != nil {
		return Response{}, err
	}
	s.kick()
	project, _ := s.projectLocked(pid)
	return Response{Project: &project, Operation: opView(s.reg.Ops[opID])}, nil
}

func (s *server) status(pid, opID string, p principal) (Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	project, ok := s.projectLocked(pid)
	if !ok {
		return Response{}, rpcErrf(http.StatusNotFound, "unknown project")
	}
	var op *opRecord
	switch {
	case opID != "":
		op = s.reg.Ops[opID]
		if op == nil || op.ProjectID != pid {
			return Response{}, rpcErrf(http.StatusNotFound, "unknown operation for this project")
		}
	default:
		if op = s.reg.unresolvedOp(pid); op == nil {
			op = s.reg.Ops[s.reg.Projects[pid].LastOp]
		}
	}
	resp := Response{Project: &project, Operation: opView(op), QueuePosition: project.QueuePosition}
	if p.admin {
		resp.Inventory = cloneInventory(s.inv)
	}
	return resp, nil
}

func normalizeDeadline(now, deadline time.Time, fallback time.Duration) (time.Time, error) {
	if deadline.IsZero() {
		return now.Add(fallback), nil
	}
	if !deadline.After(now) {
		return time.Time{}, rpcErrf(http.StatusBadRequest, "the deadline has already passed")
	}
	if deadline.After(now.Add(maxWaitDeadline)) {
		deadline = now.Add(maxWaitDeadline)
	}
	return deadline, nil
}

func (s *server) ensure(ctx context.Context, pid string, deadline time.Time) (Response, error) {
	now := s.now()
	deadline, err := normalizeDeadline(now, deadline, defaultWaitDeadline)
	if err != nil {
		return Response{}, err
	}
	if err := s.usable(pid); err != nil {
		return Response{}, err
	}
	// A fresh inspection decides whether anything must start. Docker outages
	// only prevent the shortcut: the request then queues and waits.
	refreshErr := s.refresh(ctx)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usableLocked(pid); err != nil {
		return Response{}, err
	}
	rec := s.reg.Projects[pid]
	now = s.now()
	op := s.reg.unresolvedOp(pid)
	var opID string
	waiterID, err := randomHex(8)
	if err != nil {
		return Response{}, err
	}
	waiterID = "w-" + waiterID
	switch {
	case op != nil && op.Phase == PhaseBlocked:
		return Response{}, &rpcError{status: http.StatusConflict, msg: fmt.Sprintf("project is blocked by operation %s: %s", op.ID, op.Error), resp: Response{Operation: opView(op)}}
	case op != nil && op.Kind != KindStart:
		return Response{}, &rpcError{status: http.StatusConflict, msg: fmt.Sprintf("project has a %s operation in progress (%s)", op.Kind, op.ID), resp: Response{Operation: opView(op)}}
	case op != nil:
		opID = op.ID
	case refreshErr == nil && rec.Observed == StateRunning:
		project, _ := s.projectLocked(pid)
		return Response{Project: &project}, nil
	case rec.Observed == StateUnknown:
		return Response{}, rpcErrf(http.StatusConflict, "project state is unknown: %s", rec.ObservedBlocker)
	default:
		if !pairOf(rec.Limits).fitsIn(s.cfg.Policy.Budget) {
			return Response{}, rpcErrf(http.StatusConflict, "the project exceeds the managed resource budget")
		}
		suffix, err := randomHex(8)
		if err != nil {
			return Response{}, err
		}
		opID = "op-" + suffix
	}
	err = s.commitLocked(func(r *registry) error {
		if r.Ops[opID] == nil {
			r.NextSeq++
			r.Ops[opID] = &opRecord{
				Operation: Operation{ID: opID, ProjectID: pid, Kind: KindStart, Phase: PhaseQueued, CreatedAt: now},
				Seq:       r.NextSeq,
			}
		}
		r.Waiters[waiterID] = &waiterRecord{ID: waiterID, ProjectID: pid, OperationID: opID, Deadline: deadline}
		return nil
	})
	if err != nil {
		return Response{}, err
	}
	s.seen[waiterID] = now
	s.kick()
	return s.waiterResponseLocked(waiterID), nil
}

func (s *server) waiterResponseLocked(waiterID string) Response {
	w := s.reg.Waiters[waiterID]
	project, _ := s.projectLocked(w.ProjectID)
	return Response{
		Project:       &project,
		Operation:     opView(s.reg.Ops[w.OperationID]),
		Waiter:        &Waiter{ID: w.ID, ProjectID: w.ProjectID, OperationID: w.OperationID, Deadline: w.Deadline, LastSeen: s.seen[w.ID]},
		QueuePosition: project.QueuePosition,
	}
}

func (s *server) heartbeat(pid, waiterID string) (Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.reg.Waiters[waiterID]
	if !waiterPattern.MatchString(waiterID) || w == nil || w.ProjectID != pid {
		return Response{}, rpcErrf(http.StatusNotFound, "unknown waiter: it expired, was canceled or never existed")
	}
	now := s.now()
	if !now.Before(w.Deadline) {
		resp := s.waiterResponseLocked(waiterID)
		if err := s.removeWaitersLocked([]string{waiterID}, now); err != nil {
			return Response{}, err
		}
		return Response{}, &rpcError{status: http.StatusGone, msg: "the wait deadline passed; the waiter was removed", resp: resp}
	}
	s.seen[waiterID] = now
	return s.waiterResponseLocked(waiterID), nil
}

// cancelWaiter withdraws one caller's interest. Other waiters keep the queued
// start alive; an admitted operation is only detached, never stopped.
func (s *server) cancelWaiter(pid, waiterID string) (Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.reg.Waiters[waiterID]
	if !waiterPattern.MatchString(waiterID) {
		return Response{}, rpcErrf(http.StatusBadRequest, "a valid waiterId is required")
	}
	if w == nil || w.ProjectID != pid {
		project, ok := s.projectLocked(pid)
		if !ok {
			return Response{}, rpcErrf(http.StatusNotFound, "unknown project")
		}
		return Response{Project: &project}, nil
	}
	resp := s.waiterResponseLocked(waiterID)
	if err := s.removeWaitersLocked([]string{waiterID}, s.now()); err != nil {
		return Response{}, err
	}
	if op := s.reg.Ops[w.OperationID]; op != nil {
		resp.Operation = opView(op)
	}
	project, _ := s.projectLocked(pid)
	resp.Project = &project
	resp.QueuePosition = project.QueuePosition
	delete(s.seen, waiterID)
	s.kick()
	return resp, nil
}

func (s *server) stop(ctx context.Context, p principal, req Request) (Response, error) {
	pid := req.ProjectID
	if !req.DiscardPageState {
		return Response{}, rpcErrf(http.StatusBadRequest, "stopping discards live page, form and network state; set discardPageState to acknowledge")
	}
	switch {
	case req.Force && !p.admin:
		return Response{}, rpcErrf(http.StatusForbidden, "overriding active control requires the human admin credential")
	case req.Force && req.ExpectedGeneration == "":
		return Response{}, rpcErrf(http.StatusBadRequest, "a forced stop requires the expected generation")
	case !req.Force && req.ExpectedGeneration != "":
		return Response{}, rpcErrf(http.StatusBadRequest, "expectedGeneration applies only to a forced stop")
	}
	s.mu.Lock()
	if s.reg.Projects[pid] == nil {
		s.mu.Unlock()
		return Response{}, rpcErrf(http.StatusNotFound, "unknown project")
	}
	s.mu.Unlock()
	refreshErr := s.refresh(ctx)

	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.reg.Projects[pid]
	if rec == nil {
		return Response{}, rpcErrf(http.StatusNotFound, "unknown project")
	}
	now := s.now()
	cancelStart := ""
	op := s.reg.unresolvedOp(pid)
	switch {
	case op == nil:
	case op.Phase == PhaseBlocked:
		return Response{}, &rpcError{status: http.StatusConflict, msg: fmt.Sprintf("project is blocked by operation %s: %s", op.ID, op.Error), resp: Response{Operation: opView(op)}}
	case op.Kind == KindStop && op.DiscardPageState && op.Force == req.Force && op.ExpectedGeneration == req.ExpectedGeneration:
		project, _ := s.projectLocked(pid)
		return Response{Project: &project, Operation: opView(op)}, nil
	case op.Kind == KindStart && op.Phase == PhaseQueued:
		cancelStart = op.ID
	default:
		return Response{}, &rpcError{status: http.StatusConflict, msg: fmt.Sprintf("project has a %s operation in progress (%s)", op.Kind, op.ID), resp: Response{Operation: opView(op)}}
	}
	idle := refreshErr == nil && rec.Observed == StateStopped && s.reg.effective(rec).isZero()
	var newID string
	if !idle {
		suffix, err := randomHex(8)
		if err != nil {
			return Response{}, err
		}
		newID = "op-" + suffix
	}
	err := s.commitLocked(func(r *registry) error {
		if cancelStart != "" {
			cancelQueued(r, r.Ops[cancelStart], "superseded by an explicit stop", now)
		}
		if newID != "" {
			r.NextSeq++
			r.Ops[newID] = &opRecord{
				Operation: Operation{
					ID: newID, ProjectID: pid, Kind: KindStop, Phase: PhaseQueued, CreatedAt: now,
					DiscardPageState: true, Force: req.Force, ExpectedGeneration: req.ExpectedGeneration,
				},
				Seq: r.NextSeq,
			}
		}
		return nil
	})
	if err != nil {
		return Response{}, err
	}
	s.kick()
	project, _ := s.projectLocked(pid)
	return Response{Project: &project, Operation: opView(s.reg.Ops[newID])}, nil
}

// validateUpdateAction checks the typed payload. It carries no function
// fields, paths are not interpreted here and confirmation is explicit.
func validateUpdateAction(a *UpdateAction) error {
	if a == nil {
		return rpcErrf(http.StatusBadRequest, "an update action is required")
	}
	for _, v := range []string{a.PlanFile, a.UpdateID, a.CheckpointID, a.Volume} {
		if len(v) > 4096 || strings.ContainsAny(v, "\x00\n\r") {
			return rpcErrf(http.StatusBadRequest, "update action fields must be single-line values below 4 KiB")
		}
	}
	need := func(ok bool, what string) error {
		if !ok {
			return rpcErrf(http.StatusBadRequest, "update %s requires %s", a.Kind, what)
		}
		return nil
	}
	switch a.Kind {
	case UpdateApply:
		if err := need(a.PlanFile != "", "a plan file"); err != nil {
			return err
		}
		return need(a.Confirmed || a.DryRun, "confirmation or a dry run")
	case UpdateApprove:
		if err := need(a.UpdateID != "", "an update ID"); err != nil {
			return err
		}
		return need(a.Confirmed, "confirmation")
	case UpdateAbort:
		return need(a.UpdateID != "", "an update ID")
	case UpdateRollback:
		if err := need(a.CheckpointID != "", "a checkpoint ID"); err != nil {
			return err
		}
		return need(a.Confirmed || a.DryRun, "confirmation or a dry run")
	case UpdateVerify:
		return need(a.CheckpointID != "", "a checkpoint ID")
	case UpdatePrune:
		return nil
	case UpdateDelete:
		if err := need(a.Volume != "", "a volume"); err != nil {
			return err
		}
		return need(a.Confirmed, "confirmation")
	}
	return rpcErrf(http.StatusBadRequest, "unknown update kind %q", a.Kind)
}

// needsCandidate reports whether an update may run a second Runtime/Gateway
// pair next to the current one and must therefore reserve it first.
func needsCandidate(a *UpdateAction) bool {
	return (a.Kind == UpdateApply || a.Kind == UpdateRollback) && !a.DryRun
}

// restoresServices reports whether an update may start or restore the project's
// services without a candidate: approve and abort act on a stopped project
// awaiting approval, so they must reserve one project pair first.
func restoresServices(a *UpdateAction) bool {
	return a.Kind == UpdateApprove || a.Kind == UpdateAbort
}

func (s *server) update(_ context.Context, req Request) (Response, error) {
	if err := validateUpdateAction(req.Update); err != nil {
		return Response{}, err
	}
	pid := req.ProjectID
	now := s.now()
	deadline, err := normalizeDeadline(now, req.Deadline, defaultUpdateWait)
	if err != nil {
		return Response{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.reg.Projects[pid]
	if rec == nil {
		return Response{}, rpcErrf(http.StatusNotFound, "unknown project")
	}
	if op := s.reg.unresolvedOp(pid); op != nil {
		return Response{}, &rpcError{status: http.StatusConflict, msg: fmt.Sprintf("project has an unresolved %s operation (%s, %s)", op.Kind, op.ID, op.Phase), resp: Response{Operation: opView(op)}}
	}
	mutating := req.Update.Kind != UpdateApprove && req.Update.Kind != UpdateAbort && req.Update.Kind != UpdateVerify
	if mutating && rec.Observed == StateUnknown {
		return Response{}, rpcErrf(http.StatusConflict, "project state is unknown (%s); resolve it or abort the pending update before another mutation", rec.ObservedBlocker)
	}
	if needsCandidate(req.Update) {
		limits := rec.Limits
		if !addRes(s.reg.effective(rec), pairOf(limits)).fitsIn(s.cfg.Policy.Budget) {
			return Response{}, rpcErrf(http.StatusConflict, "the candidate pair cannot fit in the managed budget even when other projects are stopped")
		}
	}
	suffix, err := randomHex(8)
	if err != nil {
		return Response{}, err
	}
	opID := "op-" + suffix
	action := *req.Update
	err = s.commitLocked(func(r *registry) error {
		r.NextSeq++
		r.Ops[opID] = &opRecord{
			Operation: Operation{ID: opID, ProjectID: pid, Kind: KindUpdate, Phase: PhaseQueued, CreatedAt: now, Update: &action},
			Seq:       r.NextSeq,
			Deadline:  deadline,
		}
		return nil
	})
	if err != nil {
		return Response{}, err
	}
	s.kick()
	project, _ := s.projectLocked(pid)
	return Response{Project: &project, Operation: opView(s.reg.Ops[opID]), QueuePosition: project.QueuePosition}, nil
}

func (s *server) connect(ctx context.Context, pid string) (Response, error) {
	s.mu.Lock()
	if err := s.usableLocked(pid); err != nil {
		s.mu.Unlock()
		return Response{}, err
	}
	if op := s.reg.unresolvedOp(pid); op != nil && (op.Phase == PhaseBlocked || op.Kind == KindStop) {
		s.mu.Unlock()
		return Response{}, &rpcError{status: http.StatusConflict, msg: fmt.Sprintf("project has a %s operation (%s, %s)", op.Kind, op.ID, op.Phase), resp: Response{Operation: opView(op)}}
	}
	project, _ := s.projectLocked(pid)
	cfg := s.cfg
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	conn, err := s.backend.VerifyConnection(ctx, cfg, project)
	if err != nil {
		return Response{}, rpcErrf(http.StatusConflict, "connection verification failed: %v", err)
	}
	if conn.ProjectID != pid || conn.Workspace != project.Workspace || conn.API != project.Origin || conn.TokenFile != filepath.Join(project.StateDir, "product", "agent-token") {
		return Response{}, rpcErrf(http.StatusConflict, "the backend returned a connection that does not belong to this project")
	}
	return Response{Project: &project, Connection: &conn}, nil
}
