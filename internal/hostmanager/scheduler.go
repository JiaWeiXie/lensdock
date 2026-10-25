package hostmanager

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// pass runs one scheduler iteration: expire waiters, reconcile workers, admit
// capacity-free work, refresh the Docker inventory and admit queued work in
// strict FIFO order. Docker calls never run under the registry mutex.
func (s *server) pass(ctx context.Context) {
	s.passMu.Lock()
	defer s.passMu.Unlock()
	s.expire()
	s.reconcileWorkers()
	s.admit(true)
	s.launchPending(ctx)
	if s.wantInventory() {
		if err := s.refresh(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("inventory refresh failed", "error", truncate(err.Error(), 300))
		}
	}
	s.admit(false)
	s.launchPending(ctx)
	s.cleanReasons()
}

func (s *server) cleanReasons() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.reasons {
		if op := s.reg.Ops[id]; op == nil || op.Phase != PhaseQueued {
			delete(s.reasons, id)
		}
	}
}

// commit persists a mutated registry copy. With onlyIfChanged the registry is
// left untouched, and nothing is written, when fn changes nothing durable.
func (s *server) commit(fn func(r *registry) error, onlyIfChanged bool) error {
	next := s.reg.clone()
	next.Revision = s.reg.Revision + 1
	if err := fn(next); err != nil {
		return err
	}
	if onlyIfChanged {
		before, after := s.reg.Revision, next.Revision
		s.reg.Revision, next.Revision = 0, 0
		a, _ := json.Marshal(s.reg)
		b, _ := json.Marshal(next)
		s.reg.Revision, next.Revision = before, after
		if bytes.Equal(a, b) {
			return nil
		}
	}
	dropped := next.pruneHistory()
	if err := saveRegistry(s.root, next); err != nil {
		return fmt.Errorf("persist registry: %w", err)
	}
	s.reg = next
	for _, id := range dropped {
		removeOpDir(s.root, id)
	}
	return nil
}

func (s *server) commitIfChangedLocked(fn func(r *registry) error) error { return s.commit(fn, true) }

// finishOp ends an operation. settle marks the project as having had real
// side effects: only observations that started after this commit may release
// its reservation.
func finishOp(r *registry, op *opRecord, phase, msg string, now time.Time, settle bool) {
	op.Phase = phase
	op.Error = truncate(msg, 2048)
	op.FinishedAt = now
	if settle {
		if rec := r.Projects[op.ProjectID]; rec != nil {
			rec.SettledRevision = r.Revision
			rec.LastOp = op.ID
		}
	}
}

func cancelQueued(r *registry, op *opRecord, reason string, now time.Time) {
	finishOp(r, op, PhaseCanceled, reason, now, false)
}

func applyReceipt(r *registry, op *opRecord, rcpt *receipt, now time.Time) {
	phase := PhaseFailed
	switch rcpt.Result {
	case ResultSucceeded:
		phase = PhaseSucceeded
	case ResultAwaitingApproval:
		phase = PhaseAwaitingApproval
	}
	finishOp(r, op, phase, rcpt.Error, now, true)
}

// dropWaiters removes waiters and cancels queued starts nobody waits for.
func dropWaiters(r *registry, ids []string, now time.Time) {
	for _, id := range ids {
		delete(r.Waiters, id)
	}
	for _, op := range r.queue() {
		if op.Kind == KindStart && len(r.waitersOf(op.ID)) == 0 {
			cancelQueued(r, op, "no waiting caller remains", now)
		}
	}
}

func (s *server) removeWaitersLocked(ids []string, now time.Time) error {
	if err := s.commitLocked(func(r *registry) error {
		dropWaiters(r, ids, now)
		return nil
	}); err != nil {
		return err
	}
	for _, id := range ids {
		delete(s.seen, id)
	}
	return nil
}

// expire runs expireLocked at the current time.
func (s *server) expire() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.expireLocked(s.now()); err != nil {
		s.log.Error("expire waiters", "error", err)
	}
}

// expireLocked removes waiters past their deadline or heartbeat TTL and fails
// queued updates that waited past their deadline. Admitted work is only
// detached. The caller holds s.mu.
func (s *server) expireLocked(now time.Time) error {
	var dead, late []string
	for id, w := range s.reg.Waiters {
		seen, ok := s.seen[id]
		if !ok {
			seen = now
			s.seen[id] = now
		}
		if !now.Before(w.Deadline) || now.Sub(seen) > s.opts.heartbeatTTL {
			dead = append(dead, id)
		}
	}
	for _, op := range s.reg.queue() {
		if op.Kind == KindUpdate && !op.Deadline.IsZero() && !now.Before(op.Deadline) {
			late = append(late, op.ID)
		}
	}
	if len(dead) == 0 && len(late) == 0 {
		return nil
	}
	err := s.commitLocked(func(r *registry) error {
		for _, id := range late {
			finishOp(r, r.Ops[id], PhaseFailed, "timed out waiting for capacity", now, false)
		}
		dropWaiters(r, dead, now)
		return nil
	})
	if err != nil {
		return err
	}
	for _, id := range dead {
		delete(s.seen, id)
	}
	return nil
}

type workerObs struct {
	opID, projectID, phase string
	probe                  probeResult
}

// reconcileWorkers decides, per admitted or running operation, whether its
// worker is alive, finished, dead after side effects (blocked) or never began.
func (s *server) reconcileWorkers() {
	s.mu.Lock()
	var list []workerObs
	for _, op := range s.reg.Ops {
		if workerPhase(op.Phase) {
			list = append(list, workerObs{opID: op.ID, projectID: op.ProjectID, phase: op.Phase})
		}
	}
	s.mu.Unlock()
	if len(list) == 0 {
		return
	}
	for i := range list {
		list[i].probe = probeWorker(s.root, list[i].projectID, list[i].opID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	err := s.commitIfChangedLocked(func(r *registry) error {
		for _, o := range list {
			op := r.Ops[o.opID]
			if op == nil || op.Phase != o.phase {
				continue
			}
			pr := o.probe
			switch {
			case pr.err != nil:
				s.log.Warn("probe worker", "operation", o.opID, "error", pr.err)
			case pr.alive:
				if pr.begun && op.Phase == PhaseAdmitted {
					op.Phase = PhaseRunning
				}
			case pr.receipt != nil:
				applyReceipt(r, op, pr.receipt, now)
			case pr.begun:
				op.Phase = PhaseBlocked
				op.Error = "the worker ended without a receipt after side effects began; the reservation is retained until a human recovers the project"
			case op.LaunchedAt.IsZero() || now.Sub(op.LaunchedAt) < s.opts.relaunchGrace:
			case op.Attempts >= s.opts.maxAttempts:
				finishOp(r, op, PhaseFailed, "the worker could not be started; see the operation's worker.log", now, true)
			default:
				op.LaunchedAt = time.Time{}
			}
		}
		return nil
	})
	if err != nil {
		s.log.Error("reconcile workers", "error", err)
	}
}

// launchPending starts a worker for each admitted operation without a live
// launch attempt. The attempt is durable before the process exists.
func (s *server) launchPending(ctx context.Context) {
	s.mu.Lock()
	var ids []string
	for _, op := range s.reg.Ops {
		if op.Phase == PhaseAdmitted && op.LaunchedAt.IsZero() {
			ids = append(ids, op.ID)
		}
	}
	if len(ids) == 0 {
		s.mu.Unlock()
		return
	}
	slices.Sort(ids)
	now := s.now()
	cfg := s.cfg
	err := s.commitLocked(func(r *registry) error {
		for _, id := range ids {
			op := r.Ops[id]
			op.LaunchedAt = now
			op.Attempts++
			if err := writeManifest(s.root, cfg.ID, op); err != nil {
				return err
			}
		}
		return nil
	})
	s.mu.Unlock()
	if err != nil {
		s.log.Error("record worker launch", "error", err)
		return
	}
	for _, id := range ids {
		if err := s.opts.launch(ctx, cfg, id); err != nil {
			s.log.Error("launch worker", "operation", id, "error", err)
		}
	}
}

func (s *server) wantInventory() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	maxAge := s.opts.inventoryMaxAge
	if s.inv == nil || s.invErr != nil {
		return s.invTried.IsZero() || now.Sub(s.invTried) >= maxAge
	}
	active := len(s.reg.queue()) > 0
	for _, p := range s.reg.Projects {
		if p.SettledRevision > s.refreshedRev {
			return true
		}
		if p.Observed != StateStopped && p.Observed != "" || s.reg.unresolvedOp(p.ID) != nil {
			active = true
		}
	}
	age := now.Sub(s.invAt)
	return (active && age >= maxAge) || age >= 6*maxAge
}

func normalizeObservation(obs Observation) (state, blocker string) {
	state, blocker = obs.State, obs.Blocker
	switch state {
	case StateStopped, StatePartial, StateUnknown:
	case StateRunning:
		if !obs.PortOwned {
			return StateUnknown, "the published port is not owned by this project's Gateway"
		}
	default:
		return StateUnknown, fmt.Sprintf("the backend reported an invalid state %q", state)
	}
	return state, blocker
}

// refresh inspects all registered projects outside the registry mutex. The
// capacity snapshot and ownership inventory always cover the same project set.
// A failed inspection keeps all accounting and blocks unsafe admission.
func (s *server) refresh(ctx context.Context) error {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	s.mu.Lock()
	snapRev := s.reg.Revision
	var ids []string
	for id := range s.reg.Projects {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	var projects []Project
	var asked []string
	for _, id := range ids {
		if p, ok := s.projectLocked(id); ok {
			projects = append(projects, p)
			asked = append(asked, id)
		}
	}
	cfg := s.cfg
	s.invTried = s.now()
	s.mu.Unlock()

	ictx, cancel := context.WithTimeout(ctx, s.opts.refreshTimeout)
	defer cancel()
	inv, err := s.backend.Inspect(ictx, cfg, projects)
	if err == nil && inv.EngineID != cfg.EngineID {
		err = fmt.Errorf("the Docker engine identity changed (reported %q, bound to %q)", inv.EngineID, cfg.EngineID)
	}
	if err != nil && ctx.Err() != nil {
		// The caller left; that says nothing about Docker.
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.invErr = err
		return err
	}
	now := s.now()
	if inv.MeasuredAt.IsZero() {
		inv.MeasuredAt = now
	}
	s.inv = cloneInventory(&inv)
	s.invAt = now
	s.invErr = nil
	s.refreshedRev = snapRev
	return s.applyInventoryLocked(inv, asked, snapRev)
}

func (s *server) applyInventoryLocked(inv Inventory, asked []string, snapRev uint64) error {
	observe := func(id string) Observation {
		obs, ok := inv.Projects[id]
		if !ok {
			return Observation{State: StateUnknown, Blocker: "the backend returned no observation for this project"}
		}
		return obs
	}
	for _, id := range asked {
		obs := observe(id)
		s.usage[id] = obs.Usage
		s.usageOK[id] = obs.UsageAvailable
	}
	return s.commitIfChangedLocked(func(r *registry) error {
		for _, id := range asked {
			rec := r.Projects[id]
			if rec == nil || rec.SettledRevision > snapRev {
				// The inspection began before the latest operation ended.
				continue
			}
			obs := observe(id)
			state, blocker := normalizeObservation(obs)
			rec.Observed = state
			rec.ObservedBlocker = truncate(blocker, 512)
			rec.Enforced = Resources{MemoryBytes: max(obs.Enforced.MemoryBytes, 0), NanoCPUs: max(obs.Enforced.NanoCPUs, 0)}
			if op := r.unresolvedOp(id); op == nil || op.Phase == PhaseQueued {
				rec.Reservation = Resources{}
			}
		}
		return nil
	})
}

// admissionStateLocked reports why admissions must wait (hold) or are refused.
func (s *server) admissionStateLocked(requireFresh bool) (hold, refuse string) {
	switch {
	case s.invErr != nil:
		hold = "Docker cannot be inspected: " + truncate(s.invErr.Error(), 240)
	case s.inv == nil:
		hold = "waiting for the first Docker inspection"
	case requireFresh && s.now().Sub(s.invAt) > s.opts.inventoryMaxAge:
		hold = "waiting for a fresh Docker inspection"
	}
	if s.inv != nil && len(s.inv.Unbounded) > 0 && !s.cfg.Policy.AllowUnboundedExternal {
		refuse = "containers without resource caps are running (" + truncate(strings.Join(s.inv.Unbounded, ", "), 200) + "); a human must allow unbounded external demand or cap them"
	}
	return hold, refuse
}

// fitLocked evaluates the capacity formula for a project's target reservation:
// managed reservations stay within the budget and, with helpers, declared
// external demand, the unbounded allowance and headroom, within the engine.
func (s *server) fitLocked(r *registry, pid string, target Resources) (impossible, wait string) {
	pol := s.cfg.Policy
	fixed := addRes(addRes(pol.Helpers, pol.Headroom), s.inv.External)
	if pol.AllowUnboundedExternal {
		fixed = addRes(fixed, pol.UnboundedExternalAllowance)
	}
	capacity := s.inv.Capacity
	switch {
	case !target.fitsIn(pol.Budget):
		return "the request exceeds the managed resource budget", ""
	case !addRes(target, fixed).fitsIn(capacity):
		return "the engine cannot hold this request next to helpers, external containers and headroom even when all managed projects are stopped", ""
	}
	total := addRes(r.reservedExcluding(pid), target)
	switch {
	case !total.fitsIn(pol.Budget):
		return "", "waiting for managed budget"
	case !addRes(total, fixed).fitsIn(capacity):
		return "", "waiting for engine capacity"
	}
	return "", ""
}

func needOf(target, eff Resources) Resources {
	return Resources{MemoryBytes: max(target.MemoryBytes-eff.MemoryBytes, 0), NanoCPUs: max(target.NanoCPUs-eff.NanoCPUs, 0)}
}

// admit moves queued operations to admitted in strict FIFO order. Stops and
// operations that need no extra capacity bypass the queue head; everything
// else waits behind the first operation that does not fit. Waiter liveness and
// update deadlines are revalidated at admission time in the same critical
// section, so an inspection that outlived them admits nothing. The
// reservation, the manifest and the registry commit all precede any worker
// launch.
func (s *server) admit(bypassOnly bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.reg.queue()) == 0 {
		return
	}
	now := s.now()
	if err := s.expireLocked(now); err != nil {
		s.log.Error("expire waiters before admission", "error", err)
		return
	}
	hold, refuse := s.admissionStateLocked(true)
	err := s.commitIfChangedLocked(func(r *registry) error {
		blocked := ""
		var admitted []*opRecord
		for _, q := range r.queue() {
			op := r.Ops[q.ID]
			rec := r.Projects[op.ProjectID]
			if op.Kind == KindStop {
				s.admitOp(r, op, rec, rec.Limits, r.effective(rec), now)
				admitted = append(admitted, op)
				continue
			}
			eff := r.effective(rec)
			limits := rec.Limits
			target := eff
			switch {
			case op.Kind == KindStart || op.Kind == KindUpdate && restoresServices(op.Update):
				target = maxRes(eff, pairOf(limits))
			case op.Kind == KindUpdate && needsCandidate(op.Update):
				target = addRes(eff, pairOf(limits))
			}
			free := needOf(target, eff).isZero()
			if bypassOnly && !free {
				continue
			}
			switch {
			case hold != "":
				s.reasons[op.ID] = hold
				if blocked == "" {
					blocked = hold
				}
				continue
			case refuse != "":
				finishOp(r, op, PhaseFailed, refuse, now, false)
				continue
			case op.Kind == KindStart && rec.Observed == StateRunning:
				finishOp(r, op, PhaseSucceeded, "the project is already running", now, false)
				continue
			case op.Kind == KindStart && rec.Observed == StateUnknown:
				finishOp(r, op, PhaseFailed, "project state is unknown: "+rec.ObservedBlocker, now, false)
				continue
			}
			if !free {
				impossible, wait := s.fitLocked(r, rec.ID, target)
				switch {
				case impossible != "":
					finishOp(r, op, PhaseFailed, impossible, now, false)
					continue
				case blocked != "":
					s.reasons[op.ID] = "waiting behind an earlier queued operation"
					continue
				case wait != "":
					blocked = wait
					s.reasons[op.ID] = wait
					continue
				}
			}
			s.admitOp(r, op, rec, limits, target, now)
			admitted = append(admitted, op)
		}
		for _, op := range admitted {
			if err := writeManifest(s.root, s.cfg.ID, op); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		s.log.Error("admit queued operations", "error", err)
	}
}

// admitOp records the reservation and the immutable project snapshot.
func (s *server) admitOp(r *registry, op *opRecord, rec *projectRecord, limits Limits, target Resources, now time.Time) {
	op.Phase = PhaseAdmitted
	op.AdmittedAt = now
	op.Target = target
	op.LaunchedAt = time.Time{}
	op.Attempts = 0
	if op.Kind != KindStop {
		rec.Limits = limits
		rec.Reservation = maxRes(rec.Reservation, target)
	}
	op.Project = snapshotProject(buildProject(s.root, r, rec, nil))
	delete(s.reasons, op.ID)
}
