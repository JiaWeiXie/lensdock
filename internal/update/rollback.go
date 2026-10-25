package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// RollbackOptions are the inputs of Rollback.
type RollbackOptions struct {
	CheckpointID string
	// DryRun verifies the checkpoint and reports the effect without changing
	// anything.
	DryRun bool
	// Confirmed is the operator's confirmation; Confirm asks interactively.
	Confirmed bool
	Confirm   func(summary string) (bool, error)
	// RevokeLeases ends a lease that outlives the drain timeout.
	RevokeLeases bool
	// AllowCrashCheckpoint admits a checkpoint that was not taken after a
	// clean stop.
	AllowCrashCheckpoint bool
}

type rollbackRun struct {
	e       *Engine
	cp      Checkpoint
	cur     State
	tracked bool
	arch    string
	cc      ComposeCLI // the running generation
	helper  VolumeHelper
	key     []byte
	store   CheckpointStore
	j       *Journal
	opts    RollbackOptions
	sets    []ownedDataset
	restore map[string]string // snapshot dataset key -> restored volume
}

// rollbackSummary is what the operator confirms.
func (r *rollbackRun) summary() string {
	var b strings.Builder
	age := r.e.Now().UTC().Sub(r.cp.CreatedAt).Round(time.Minute)
	fmt.Fprintf(&b, "Roll workspace %s back to the state of checkpoint %s?\n", r.cp.Workspace, r.cp.ID)
	fmt.Fprintf(&b, "  checkpoint time: %s (%s ago); this is the RPO: everything written after it is not restored\n", r.cp.CreatedAt.Format(time.RFC3339), age)
	b.WriteString("  restored together (paired): the old images, the Profile/keyring snapshot and the Gateway database snapshot\n")
	b.WriteString("  the current Profile/database volumes are NOT deleted: they stay as a retired branch to recover later data from\n")
	if len(r.cp.RetainedNotRolledBack) > 0 {
		fmt.Fprintf(&b, "  not affected by the rollback (retained as they are): %s\n", strings.Join(r.cp.RetainedNotRolledBack, ", "))
	}
	if r.cp.Consistency != ConsistencyClean {
		b.WriteString("  WARNING: this checkpoint is a crash snapshot (the stores were not closed cleanly)\n")
	}
	return b.String()
}

// rollbackTargets lists the images a rollback starts (the checkpoint's) and
// the ones a failed rollback starts again (the running generation's).
func rollbackTargets(cp Checkpoint, cur State) []retainTarget {
	old := componentTargets(cp.Previous.Components, true)
	for i := range old {
		old[i].Name = "old " + old[i].Name
	}
	running := componentTargets(cur.Components, true)
	for i := range running {
		running[i].Name = "running " + running[i].Name
	}
	return append(old, running...)
}

// rollbackPlan loads and verifies everything that can be checked without
// changing the deployment.
func (e *Engine) rollbackPrepare(ctx context.Context, o RollbackOptions) (*rollbackRun, error) {
	if !checkpointIDPattern.MatchString(o.CheckpointID) {
		return nil, fmt.Errorf("invalid checkpoint id %q", o.CheckpointID)
	}
	key, err := LoadBackupKey(e.Dep.BackupKeyFile)
	if err != nil {
		return nil, err
	}
	store := CheckpointStore{Dir: e.Dep.BackupDir}
	cp, err := store.Verify(o.CheckpointID, key, true)
	if err != nil {
		return nil, fmt.Errorf("checkpoint %s failed verification, nothing was changed: %w", o.CheckpointID, err)
	}
	if cp.Consistency != ConsistencyClean && !o.AllowCrashCheckpoint {
		return nil, fmt.Errorf("checkpoint %s is a crash snapshot (%s): it is no safe rollback target; --allow-crash-checkpoint overrides", cp.ID, strings.Join(cp.Clean.Reasons, "; "))
	}
	arch, err := e.engineArch(ctx)
	if err != nil {
		return nil, err
	}
	cur, tracked, _, err := e.resolveState(ctx)
	if err != nil {
		return nil, err
	}
	if !tracked {
		return nil, errors.New("this deployment has no managed generation: there is nothing to roll back")
	}
	if cp.Previous.Generation >= cur.Generation {
		return nil, fmt.Errorf("checkpoint %s belongs to generation %d, which is not older than the running generation %d", cp.ID, cp.Previous.Generation, cur.Generation)
	}
	if _, err := e.Dep.Workspace(cp.Workspace); err != nil {
		return nil, err
	}
	for id, before := range cp.Previous.Components {
		now, ok := cur.Components[id]
		if !ok {
			return nil, fmt.Errorf("checkpoint component %s no longer exists in this deployment", id)
		}
		if id != runtimeComponent(cp.Workspace) && id != gatewayComponent {
			if before.Image != now.Image || before.ImageID != now.ImageID || !equalStringMaps(before.Volumes, now.Volumes) {
				return nil, fmt.Errorf("component %s changed after checkpoint %s; this checkpoint cannot roll back %s without also reverting unsnapshotted state", id, cp.ID, cp.Workspace)
			}
		}
	}
	sets, err := e.datasetsFor(cp.Workspace, cur)
	if err != nil {
		return nil, err
	}
	for _, ds := range sets {
		if ds.Role == RoleSnapshot {
			if _, ok := cp.Dataset(ds.Owner, ds.Key); !ok {
				return nil, fmt.Errorf("checkpoint %s has no dataset %s/%s", cp.ID, ds.Owner, ds.Key)
			}
		}
	}
	// The old images, and the running generation's images that a failed
	// rollback restarts, must be keepable by exact digest before anything is
	// stopped. Nothing is changed here, and pulling stays an explicit
	// operator action so disk use is visible.
	if _, err := e.planRetentions(ctx, arch, rollbackTargets(cp, cur)); err != nil {
		return nil, fmt.Errorf("an image of the rollback or of the running generation is not available with its exact digest, nothing was changed: %w", err)
	}
	prevRuntime := cp.Previous.Components[runtimeComponent(cp.Workspace)]
	return &rollbackRun{
		e: e, cp: cp, cur: cur, tracked: tracked, arch: arch,
		cc:     e.Dep.composeFor(e.Docker, cur, tracked),
		helper: VolumeHelper{D: e.Docker, Image: prevRuntime.Image},
		key:    key, store: store, opts: o, sets: sets, restore: map[string]string{},
	}, nil
}

// Rollback restores the old images together with the pre-update checkpoint
// (Profile, keyring and Gateway database) into new volumes and switches to
// them. Nothing is deleted: the volumes of the running generation become a
// retired branch. A rollback that fails leaves the running generation up.
func (e *Engine) Rollback(ctx context.Context, o RollbackOptions) (*ApplyResult, error) {
	r, err := e.rollbackPrepare(ctx, o)
	if err != nil {
		return nil, err
	}
	if o.DryRun {
		lines := []string{"dry run: the checkpoint decrypted and verified; the old images are present; nothing was changed"}
		lines = append(lines, strings.Split(strings.TrimSpace(r.summary()), "\n")...)
		return &ApplyResult{Status: "dry-run", CheckpointID: r.cp.ID, Lines: lines, Message: "dry run complete"}, nil
	}
	confirmed := o.Confirmed
	if !confirmed && o.Confirm != nil {
		confirmed, err = o.Confirm(r.summary())
		if err != nil {
			return nil, err
		}
	}
	if !confirmed {
		return nil, ErrNotConfirmed
	}
	if err := e.prepareDirs(); err != nil {
		return nil, err
	}
	unlock, err := lockFile(e.Dep.lockPath())
	if err != nil {
		return nil, err
	}
	defer unlock()
	if pj := pendingJournal(e.Dep, e.Now); pj != nil {
		return nil, fmt.Errorf("%s %s is %s: approve or abort it first (lensdock update status)", pj.Kind, pj.ID, pj.Status)
	}
	id, err := newID("r", e.Now())
	if err != nil {
		return nil, err
	}
	r.j, err = newJournal(e.Dep, "rollback", id, "", r.cp.Workspace, r.cp.Previous.ReleaseID, e.Now)
	if err != nil {
		return nil, err
	}
	r.j.CheckpointID = r.cp.ID
	prev := r.cur.Clone()
	r.j.Previous = &prev
	if err := r.j.save(); err != nil {
		return nil, err
	}
	res, err := r.execute(ctx)
	e.publishQuiet(ctx)
	if err != nil && res != nil && res.Status == JournalReverted {
		return res, nil
	}
	return res, err
}

func (r *rollbackRun) step(id string, fn func() (string, error)) error {
	if err := r.j.begin(id); err != nil {
		return err
	}
	r.e.logf("step %s %s", id, r.j.step(id).Name)
	detail, err := fn()
	if err != nil {
		r.j.fail(id, err)
		return err
	}
	return r.j.ok(id, detail)
}

func (r *rollbackRun) result(msg string) *ApplyResult {
	res := &ApplyResult{UpdateID: r.j.ID, Status: r.j.Status, CheckpointID: r.cp.ID, Message: msg}
	if r.j.Intent != nil {
		res.Generation = r.j.Intent.Generation
	}
	return res
}

func (r *rollbackRun) services() (rtSvc, gwSvc string) {
	rtSvc = r.cur.Components[runtimeComponent(r.cp.Workspace)].Service
	if r.e.Dep.Gateway != nil {
		gwSvc = r.cur.Components[gatewayComponent].Service
	}
	return rtSvc, gwSvc
}

func (r *rollbackRun) execute(ctx context.Context) (*ApplyResult, error) {
	steps := []struct {
		id string
		fn func(context.Context) (string, error)
	}{
		{"R1", r.verify},
		{"R2", r.capacity},
		{"R3", r.maintenance},
		{"R4", r.stopCurrent},
		{"R5", r.restoreVolumes},
	}
	for _, s := range steps {
		fn := s.fn
		if err := r.step(s.id, func() (string, error) { return fn(ctx) }); err != nil {
			return r.fail(ctx, err, s.id == "R5")
		}
	}
	if err := r.step("R6", func() (string, error) { return r.doSwitch(ctx) }); err != nil {
		return r.fail(ctx, err, false)
	}
	if err := r.step("R7", func() (string, error) { return r.health(ctx) }); err != nil {
		recovery := fmt.Sprintf("rollback generation %d was activated but the final health read failed (%v). Inspect it with `lensdock update status`; the paired old images and restored data remain active", r.j.Intent.Generation, err)
		r.j.finish(JournalHeld, recovery)
		return r.result(recovery), err
	}
	r.j.finish(JournalCompleted, "")
	return r.result("rollback completed"), nil
}

func (r *rollbackRun) verify(ctx context.Context) (string, error) {
	if _, err := r.store.Verify(r.cp.ID, r.key, true); err != nil {
		return "", err
	}
	pinned, err := r.e.retainImages(ctx, r.arch, rollbackTargets(r.cp, r.cur))
	if err != nil {
		return "", fmt.Errorf("the old and the running images cannot be kept, so nothing was stopped: %w", err)
	}
	return fmt.Sprintf("checkpoint %s decrypts and its %d datasets verify; images: %s", r.cp.ID, len(r.cp.Datasets), pinned), nil
}

func (r *rollbackRun) capacity(ctx context.Context) (string, error) {
	var total int64
	var first string
	for _, cd := range r.cp.Datasets {
		total += cd.PlainBytes
	}
	for _, ds := range r.sets {
		if ds.Role == RoleSnapshot {
			first = ds.Volume
			break
		}
	}
	need := total + r.e.Dep.MinFreeBytes
	free, err := r.helper.FreeBytes(ctx, first)
	if err != nil {
		return "", fmt.Errorf("docker data root: %w", err)
	}
	if free < need {
		return "", fmt.Errorf("docker data root has %s free, the restored volumes need %s: free space or prune retired branches first", humanBytes(free), humanBytes(need))
	}
	return fmt.Sprintf("%s free for %s of restored data", humanBytes(free), humanBytes(total)), nil
}

func (r *rollbackRun) isRunning(ctx context.Context, service string) bool {
	_, err := r.e.runningContainer(ctx, r.cc, service)
	return err == nil
}

func (r *rollbackRun) stopService(ctx context.Context, service string) error {
	if err := r.cc.Stop(ctx, time.Duration(r.e.Dep.StopGraceSeconds)*time.Second, service); err != nil {
		return err
	}
	if !contains(r.j.StoppedServices, service) {
		r.j.StoppedServices = append(r.j.StoppedServices, service)
	}
	return r.j.save()
}

// maintenance stops the Gateway and fences the runtime. The running
// generation may be the broken one, so an unreachable runtime is not an
// error here.
func (r *rollbackRun) maintenance(ctx context.Context) (string, error) {
	rtSvc, gwSvc := r.services()
	var notes []string
	if gwSvc != "" && r.isRunning(ctx, gwSvc) {
		if err := r.stopService(ctx, gwSvc); err != nil {
			return "", fmt.Errorf("stop gateway: %w", err)
		}
		notes = append(notes, "gateway stopped")
	}
	if !r.isRunning(ctx, rtSvc) {
		return strings.Join(append(notes, "runtime is not running"), "; "), nil
	}
	resp, err := r.e.runtimeProbe(ctx, r.cp.Workspace, r.cur, r.tracked, ProbeRequest{Op: ProbeMaintenanceEnter, Reason: "lensdock update " + r.j.ID})
	switch {
	case err != nil:
		notes = append(notes, "runtime not fenced ("+err.Error()+"): the clean stop revokes every lease")
	case resp.Error != "":
		notes = append(notes, "runtime not fenced ("+resp.Error+"): the clean stop revokes every lease")
	default:
		r.j.MaintenanceFenced = true
		_ = r.j.save()
		notes = append(notes, "runtime fenced in MAINTENANCE")
	}
	return strings.Join(notes, "; "), nil
}

func (r *rollbackRun) stopCurrent(ctx context.Context) (string, error) {
	rtSvc, _ := r.services()
	if r.isRunning(ctx, rtSvc) {
		if err := r.stopService(ctx, rtSvc); err != nil {
			return "", fmt.Errorf("stop runtime: %w", err)
		}
	}
	var retired []string
	for _, ds := range r.sets {
		if ds.Role == RoleSnapshot {
			retired = append(retired, ds.Volume)
		}
	}
	r.j.RetiredVolumes = retired
	if err := r.j.save(); err != nil {
		return "", err
	}
	return fmt.Sprintf("stopped; %d volumes of the running generation are kept as the retired branch", len(retired)), nil
}

func (r *rollbackRun) restoreVolumes(ctx context.Context) (string, error) {
	r.j.Candidate = &CandidateRecord{Volumes: map[string]string{}}
	short := r.j.ID[len(r.j.ID)-6:]
	for _, ds := range r.sets {
		if ds.Role != RoleSnapshot {
			continue
		}
		cd, _ := r.cp.Dataset(ds.Owner, ds.Key)
		name := fmt.Sprintf("%s-rest-%s-g%d-%s", r.e.Dep.Compose.Project, ds.Key, r.cur.Generation+1, short)
		labels := map[string]string{
			LabelManaged: "true", LabelUpdate: r.j.ID, LabelRole: RoleRestored, LabelDataset: ds.Owner + "/" + ds.Key,
			LabelCreated: r.e.Now().UTC().Format(time.RFC3339),
		}
		if err := r.e.Docker.VolumeCreate(ctx, name, labels); err != nil {
			return "", err
		}
		r.j.Candidate.Volumes[ds.Key] = name
		r.restore[ds.Key] = name
		if err := r.j.save(); err != nil {
			return "", err
		}
		rd, closer, err := r.store.OpenDataset(r.cp, cd, r.key)
		if err != nil {
			return "", err
		}
		err = r.helper.TarIn(ctx, name, rd)
		if err == nil {
			_, err = io.Copy(io.Discard, rd)
		}
		_ = closer.Close()
		if err != nil {
			return "", fmt.Errorf("restore %s into %s: %w", ds.Key, name, err)
		}
		fp, err := r.helper.Fingerprint(ctx, name)
		if err != nil {
			return "", err
		}
		if !fp.Equal(cd.Fingerprint) {
			return "", fmt.Errorf("restored volume %s differs from the checkpoint of %s", name, ds.Key)
		}
	}
	return fmt.Sprintf("%d volumes restored from the checkpoint and compared by fingerprint", len(r.restore)), nil
}

func (r *rollbackRun) intent() State {
	next := r.cp.Previous.Clone()
	next.Generation = r.cur.Generation + 1
	next.UpdatedAt = r.e.Now().UTC()
	next.LastUpdateID = r.j.ID
	next.ReleaseID = r.cp.Previous.ReleaseID
	for id, c := range next.Components {
		// Retained datasets keep the volumes of the running generation:
		// they are never copied or rolled back.
		curc := r.cur.Components[id]
		for _, ds := range r.sets {
			if ds.Owner != id {
				continue
			}
			if ds.Role == RoleRetained {
				c.Volumes[ds.Key] = curc.Volumes[ds.Key]
			} else {
				c.Volumes[ds.Key] = r.restore[ds.Key]
			}
		}
		next.Components[id] = c
	}
	return next
}

func (r *rollbackRun) doSwitch(ctx context.Context) (string, error) {
	next := r.intent()
	r.j.Intent = &next
	if err := r.j.save(); err != nil {
		return "", err
	}
	rtSvc, gwSvc := r.services()
	data, err := RenderGenerationOverride(next)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(r.e.Dep.generationDir(next.Generation), 0o700); err != nil {
		return "", err
	}
	if err := writeFileAtomic(r.e.Dep.overridePath(next.Generation), data, 0o600); err != nil {
		return "", err
	}
	nc := r.e.Dep.composeFor(r.e.Docker, next, true)
	if _, err := nc.Config(ctx); err != nil {
		return "", fmt.Errorf("rollback generation configuration: %w", err)
	}
	timeout := time.Duration(r.e.Dep.HealthTimeoutSeconds) * time.Second
	if err := nc.Up(ctx, UpOptions{NoDeps: true, Wait: true, WaitTimeout: timeout}, rtSvc); err != nil {
		return "", fmt.Errorf("start the old runtime: %w", err)
	}
	if err := r.e.waitUntil(ctx, timeout, 2*time.Second, func(ctx context.Context) (bool, error) {
		resp, err := r.e.runtimeProbe(ctx, r.cp.Workspace, next, true, ProbeRequest{Op: ProbeStatus})
		if errors.Is(err, ErrNoOperatorToken) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		if perr := probeErr(resp); perr != nil || resp.Status == nil {
			return false, fmt.Errorf("status: %v", perr)
		}
		if resp.Status.Locked {
			return false, fmt.Errorf("runtime is locked (%s)", resp.Status.LockCode)
		}
		return resp.Status.Ready, errors.New("runtime is not ready")
	}); err != nil {
		return "", fmt.Errorf("the old runtime did not become ready: %w", err)
	}
	if gwSvc != "" {
		if err := nc.Up(ctx, UpOptions{NoDeps: true, Wait: true, WaitTimeout: timeout}, gwSvc); err != nil {
			return "", fmt.Errorf("start the old gateway: %w", err)
		}
		if url := r.e.Dep.Gateway.ReadyURL; url != "" {
			if err := r.e.waitUntil(ctx, timeout, 2*time.Second, func(ctx context.Context) (bool, error) { return httpOK(ctx, url) }); err != nil {
				return "", fmt.Errorf("the old gateway did not become ready: %w", err)
			}
		}
	}
	if err := SaveState(r.e.Dep, next); err != nil {
		return "", fmt.Errorf("record the rollback generation: %w", err)
	}
	return fmt.Sprintf("generation %d runs the old images on the restored volumes", next.Generation), nil
}

func (r *rollbackRun) health(ctx context.Context) (string, error) {
	next := *r.j.Intent
	detail := fmt.Sprintf("generation %d", next.Generation)
	resp, err := r.e.runtimeProbe(ctx, r.cp.Workspace, next, true, ProbeRequest{Op: ProbeStatus})
	switch {
	case errors.Is(err, ErrNoOperatorToken):
		detail += "; runtime state not inspected (no operator token)"
	case err != nil:
		return detail, err
	default:
		if perr := probeErr(resp); perr != nil || resp.Status == nil {
			return detail, fmt.Errorf("status: %v", perr)
		}
		r.j.RuntimeGenAfter = resp.Status.RuntimeGeneration
		detail += fmt.Sprintf("; runtime generation %s: users and agents must acquire new leases", resp.Status.RuntimeGeneration)
	}
	return detail, r.j.save()
}

// fail handles a failed rollback: nothing is deleted. A failed restore removes
// the restored volumes it created (they are copies of the checkpoint); the
// running generation is started again.
func (r *rollbackRun) fail(ctx context.Context, cause error, removeRestored bool) (*ApplyResult, error) {
	cctx, cancel := cleanupContext(ctx)
	defer cancel()
	if removeRestored && r.j.Candidate != nil {
		for _, v := range r.j.Candidate.Volumes {
			if err := r.e.Docker.VolumeRemove(cctx, v); err != nil {
				r.e.logf("warning: remove restored volume %s: %v", v, err)
			}
		}
		r.j.Candidate.Volumes = nil
	}
	if r.j.Intent != nil {
		nc := r.e.Dep.composeFor(r.e.Docker, *r.j.Intent, true)
		rtSvc, gwSvc := r.services()
		var svcs []string
		for _, s := range []string{gwSvc, rtSvc} {
			if s != "" {
				svcs = append(svcs, s)
			}
		}
		if err := nc.Stop(cctx, time.Duration(r.e.Dep.StopGraceSeconds)*time.Second, svcs...); err != nil {
			r.e.logf("warning: stop the rollback services: %v", err)
		}
	}
	timeout := time.Duration(r.e.Dep.HealthTimeoutSeconds) * time.Second
	var errs []error
	for i := len(r.j.StoppedServices) - 1; i >= 0; i-- {
		svc := r.j.StoppedServices[i]
		r.e.logf("restoring %s on the running generation", svc)
		if err := r.cc.Up(cctx, UpOptions{NoDeps: true, Wait: true, WaitTimeout: timeout}, svc); err != nil {
			errs = append(errs, fmt.Errorf("restart %s: %w", svc, err))
		}
	}
	if len(errs) == 0 && r.j.MaintenanceFenced {
		rtSvc, _ := r.services()
		if !contains(r.j.StoppedServices, rtSvc) {
			_, _ = r.e.runtimeProbe(cctx, r.cp.Workspace, r.cur, r.tracked, ProbeRequest{Op: ProbeMaintenanceExit})
		}
	}
	if len(errs) > 0 {
		recovery := fmt.Sprintf("the rollback failed and the running generation did not restart automatically (%v). The checkpoint %s, the retired volumes and any restored volumes are intact; start the services with `lensdock update compose -- up -d` after fixing the cause, then retry the rollback", errors.Join(errs...), r.cp.ID)
		r.j.finish(JournalHeld, recovery)
		return r.result(recovery), fmt.Errorf("%w; the running generation also failed to restart", cause)
	}
	r.j.StoppedServices, r.j.MaintenanceFenced = nil, false
	r.j.finish(JournalReverted, "the rollback failed; the running generation was started again, nothing was deleted")
	return r.result("rollback failed; the running generation was restored"), cause
}

// abortRollbackJournal recovers an interrupted or held rollback. It starts the
// exact state that was running before the rollback; restored volumes are
// deleted only when explicitly requested by omitting --keep-candidate.
func (e *Engine) abortRollbackJournal(ctx context.Context, j *Journal, keepCandidate bool) (*ApplyResult, error) {
	switch j.Status {
	case JournalRunning, JournalHeld:
	default:
		return nil, fmt.Errorf("rollback %s is %s: nothing to abort", j.ID, j.Status)
	}
	if j.Previous == nil {
		return nil, errors.New("rollback journal has no previous state")
	}
	cctx, cancel := cleanupContext(ctx)
	defer cancel()
	if j.Intent != nil {
		intentCompose := e.Dep.composeFor(e.Docker, *j.Intent, true)
		var services []string
		for _, component := range j.Intent.Components {
			if !contains(services, component.Service) {
				services = append(services, component.Service)
			}
		}
		if len(services) > 0 {
			_ = intentCompose.Stop(cctx, time.Duration(e.Dep.StopGraceSeconds)*time.Second, services...)
		}
	}
	previousCompose := e.Dep.composeFor(e.Docker, *j.Previous, true)
	timeout := time.Duration(e.Dep.HealthTimeoutSeconds) * time.Second
	services := append([]string{}, j.StoppedServices...)
	if len(services) == 0 {
		for _, component := range j.Previous.Components {
			if !contains(services, component.Service) {
				services = append(services, component.Service)
			}
		}
	}
	var errs []error
	for i := len(services) - 1; i >= 0; i-- {
		if err := previousCompose.Up(cctx, UpOptions{NoDeps: true, Wait: true, WaitTimeout: timeout}, services[i]); err != nil {
			errs = append(errs, fmt.Errorf("restart %s: %w", services[i], err))
		}
	}
	if len(errs) > 0 {
		recovery := "the pre-rollback generation could not be restarted; all volumes and the checkpoint are intact"
		j.finish(JournalHeld, recovery)
		return &ApplyResult{UpdateID: j.ID, Status: j.Status, CheckpointID: j.CheckpointID, Message: recovery}, errors.Join(errs...)
	}
	if st, ok, err := LoadState(e.Dep); err == nil && ok && st.LastUpdateID == j.ID {
		if err := SaveState(e.Dep, *j.Previous); err != nil {
			j.finish(JournalHeld, "services restored but state.json could not be restored")
			return nil, err
		}
	}
	if !keepCandidate && j.Candidate != nil {
		for _, volume := range j.Candidate.Volumes {
			if err := e.Docker.VolumeRemove(cctx, volume); err != nil {
				e.logf("warning: remove restored volume %s: %v", volume, err)
			}
		}
	}
	j.StoppedServices, j.MaintenanceFenced = nil, false
	j.finish(JournalAborted, "aborted by the operator: the pre-rollback generation runs again; no working volume was overwritten")
	return &ApplyResult{UpdateID: j.ID, Status: j.Status, CheckpointID: j.CheckpointID, Message: "rollback aborted; previous generation restored"}, nil
}
