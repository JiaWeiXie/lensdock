package hostmanager

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/update"
)

// Housekeeping is the manager's own low-priority upkeep: once a day it runs
// the typed expire update of each project that is due, so a project that runs
// for months still drops unused old system data. It is an ordinary typed
// update operation, so admission, the project lock and the global helper lock
// serialize it with everything else, and the deployment's own setting
// (expireAfterDays, 0 disables), throttle and protected references apply.
//
// It never competes with foreground work:
//   - it is scheduled only while no operation is queued or running anywhere;
//   - a queued housekeeping operation is canceled in the very commit that adds
//     any other operation of the project, and is invisible to every handler;
//   - while it runs it does not change the project's visible state, does not
//     become its last operation, and a start request for a running project is
//     answered immediately.

// housekeepingDeadline bounds how long a queued housekeeping operation may wait
// for admission before it fails (and is tried again the next day).
const housekeepingDeadline = 15 * time.Minute

// isHousekeeping reports whether op is scheduler-created housekeeping.
func isHousekeeping(op *opRecord) bool {
	return op.Kind == KindUpdate && op.Update != nil && op.Update.Kind == UpdateExpire && op.Update.Automatic
}

func queuedHousekeeping(op *opRecord) bool { return op.Phase == PhaseQueued && isHousekeeping(op) }

// quietHousekeeping is housekeeping that holds a worker and must stay out of
// the project's visible state.
func quietHousekeeping(op *opRecord) bool {
	return isHousekeeping(op) && workerPhase(op.Phase)
}

// cancelSupersededHousekeeping drops a queued housekeeping operation when its
// project has any other unresolved operation or was revoked. Every registry
// commit runs it.
func (r *registry) cancelSupersededHousekeeping(now time.Time) {
	for _, hk := range r.queue() {
		if !isHousekeeping(hk) {
			continue
		}
		if rec := r.Projects[hk.ProjectID]; rec == nil || rec.Revoked {
			cancelQueued(r, hk, "the project is revoked", now)
			continue
		}
		for _, other := range r.Ops {
			if other != hk && other.ProjectID == hk.ProjectID && unresolvedPhase(other.Phase) {
				cancelQueued(r, hk, "superseded by foreground work", now)
				break
			}
		}
	}
}

// idleForHousekeeping reports that no operation is queued or has a worker.
func (r *registry) idleForHousekeeping() bool {
	for _, op := range r.Ops {
		if op.Phase == PhaseQueued || workerPhase(op.Phase) {
			return false
		}
	}
	return true
}

// expiryEnabled reads expireAfterDays of a project's deployment without
// validating or locking it. A project that never started has no deployment
// and nothing to expire. Unreadable configuration is an error: the caller then
// schedules nothing.
func expiryEnabled(projectDir string) (bool, error) {
	data, err := readPrivateFile(filepath.Join(projectDir, "deployment.json"), 4<<20)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var d struct {
		ExpireAfterDays *int `json:"expireAfterDays"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return false, err
	}
	days := update.DefaultExpireAfterDays
	if d.ExpireAfterDays != nil {
		days = *d.ExpireAfterDays
	}
	return days > 0, nil
}

// scheduleHousekeeping runs at the end of a scheduler pass, under the pass
// lock, so it can never enqueue work after shutdown was accepted. A project is
// considered once per update.AutoExpireInterval: the first sighting only
// starts its clock.
func (s *server) scheduleHousekeeping() {
	select {
	case <-s.shutdownCh:
		return
	default:
	}
	now := s.now()

	s.mu.Lock()
	// Freshness is not required to queue: the queued operation itself makes
	// the manager inspect before admitting it.
	if s.admissionHoldLocked(false) != "" || !s.reg.idleForHousekeeping() {
		s.mu.Unlock()
		return
	}
	var due []string
	first := map[string]bool{}
	for id, rec := range s.reg.Projects {
		if rec.Revoked || rec.Observed == StateUnknown || s.reg.unresolvedOp(id) != nil {
			continue
		}
		if rec.ExpiryCheckedAt.IsZero() {
			first[id] = true
		} else if age := now.Sub(rec.ExpiryCheckedAt); age >= 0 && age < update.AutoExpireInterval {
			continue
		}
		due = append(due, id)
	}
	s.mu.Unlock()
	if len(due) == 0 {
		return
	}
	slices.Sort(due)

	enabled := map[string]bool{}
	for _, id := range due {
		if first[id] {
			continue
		}
		ok, err := expiryEnabled(projectStateDir(s.root, id))
		if err != nil {
			s.log.Warn("housekeeping skipped: deployment settings are unreadable", "project", id, "error", truncate(err.Error(), 200))
		}
		enabled[id] = err == nil && ok
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	scheduled := false
	err := s.commitIfChangedLocked(func(r *registry) error {
		for _, id := range due {
			rec := r.Projects[id]
			if rec == nil || rec.Revoked || r.unresolvedOp(id) != nil {
				continue
			}
			switch {
			case first[id] || !enabled[id]:
				rec.ExpiryCheckedAt = now
			case r.idleForHousekeeping():
				suffix, err := randomHex(8)
				if err != nil {
					return err
				}
				opID := "op-" + suffix
				r.NextSeq++
				r.Ops[opID] = &opRecord{
					Operation: Operation{
						ID: opID, ProjectID: id, Kind: KindUpdate, Phase: PhaseQueued, CreatedAt: now,
						Update: &UpdateAction{Kind: UpdateExpire, Confirmed: true, Automatic: true},
					},
					Seq:      r.NextSeq,
					Deadline: now.Add(housekeepingDeadline),
				}
				rec.ExpiryCheckedAt = now
				scheduled = true
			}
		}
		return nil
	})
	if err != nil {
		s.log.Error("schedule housekeeping", "error", err)
		return
	}
	if scheduled {
		s.kick()
	}
}
