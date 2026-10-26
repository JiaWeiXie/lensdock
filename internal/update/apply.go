package update

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/auth"
)

// ApplyOptions are the inputs of Apply.
type ApplyOptions struct {
	PlanFile string
	// DryRun re-verifies the plan against the live deployment and reports what
	// apply would do, without changing anything.
	DryRun bool
	// Confirmed is the operator's confirmation to start maintenance.
	Confirmed bool
	// ApproveSwitch pre-approves the switch when candidate verification passes.
	ApproveSwitch bool
	// SwitchApproval asks the operator after candidate verification. When both
	// it and ApproveSwitch are unset the update stops at awaiting-approval with
	// the old services still stopped (finish with Approve or Abort).
	SwitchApproval func(summary string) (bool, error)
	// RevokeLeases ends a lease that outlives the drain timeout.
	RevokeLeases bool
	// AcceptCrashSnapshot continues after a stop that was not clean; the
	// checkpoint is then marked crash and is never a default rollback target.
	AcceptCrashSnapshot bool
}

// ApplyResult summarizes a finished or paused update.
type ApplyResult struct {
	UpdateID     string
	Status       string
	CheckpointID string
	Generation   int
	Message      string
	Lines        []string
}

// ErrNotConfirmed is returned when apply was not confirmed by the operator.
var ErrNotConfirmed = errors.New("apply needs the operator's confirmation (--yes, or answer the prompt)")

// ErrAwaitingApproval is not an error of the update: apply stopped before the
// switch because nobody approved it yet.
var ErrAwaitingApproval = errors.New("update is waiting for approval of the switch")

// updateRun carries the context of one update through its steps.
type updateRun struct {
	e       *Engine
	plan    *Plan
	rel     *Release
	arch    string
	ws      WorkspaceConfig
	prev    State
	tracked bool
	model   ComposeModel
	old     ComposeCLI
	helper  VolumeHelper
	key     []byte
	store   CheckpointStore
	j       *Journal
	opts    ApplyOptions
	sets    []ownedDataset

	clean       CleanEvidence
	cp          *Checkpoint
	candidate   CandidateRecord
	candRetain  map[string]string // retained dataset key -> volume the candidate sees
	candProject string
	candToken   string
	report      *VerifyReport
}

// ensureRuntimeDirs creates the state directories.
func (e *Engine) prepareDirs() error { return e.Dep.ensureDirs() }

func equalStringMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// checkDrift verifies that the deployment is still what the plan was made
// against.
func checkDrift(p *Plan, st State, tracked bool) error {
	if st.Generation != p.Current.Generation || tracked != p.Current.Tracked || st.ReleaseID != p.Current.ReleaseID {
		return fmt.Errorf("the deployment changed since the plan was made (generation %d, plan expects %d): make a new plan", st.Generation, p.Current.Generation)
	}
	for id, want := range p.Current.Components {
		got, ok := st.Components[id]
		if !ok || got.Image != want.Image || got.ImageID != want.ImageID || got.Service != want.Service || !equalStringMaps(got.Volumes, want.Volumes) {
			return fmt.Errorf("component %s changed since the plan was made: make a new plan", id)
		}
	}
	return nil
}

// prepareRun re-verifies the release and the deployment without changing
// anything.
func (e *Engine) prepareRun(ctx context.Context, plan *Plan, opts ApplyOptions) (*updateRun, error) {
	rel, err := LoadRelease(plan.Release.Dir, VerifyPolicy{TrustedKeys: e.Dep.TrustedKeys, Now: e.Now})
	if err != nil {
		return nil, err
	}
	if rel.SHA256 != plan.Release.ManifestSHA256 {
		return nil, errors.New("the release manifest changed since the plan was made: make a new plan")
	}
	arch, err := e.engineArch(ctx)
	if err != nil {
		return nil, err
	}
	if arch != plan.Arch {
		return nil, fmt.Errorf("the plan was made for %s, this engine is %s", plan.Arch, arch)
	}
	ws, err := e.Dep.Workspace(plan.Workspace)
	if err != nil {
		return nil, err
	}
	st, tracked, model, err := e.resolveState(ctx)
	if err != nil {
		return nil, err
	}
	if err := checkDrift(plan, st, tracked); err != nil {
		return nil, err
	}
	key, err := LoadBackupKey(e.Dep.BackupKeyFile)
	if err != nil {
		return nil, err
	}
	sets, err := e.datasetsFor(plan.Workspace, st)
	if err != nil {
		return nil, err
	}
	comp := st.Components[runtimeComponent(plan.Workspace)]
	return &updateRun{
		e: e, plan: plan, rel: rel, arch: arch, ws: ws, prev: st, tracked: tracked, model: model,
		old:    e.Dep.composeFor(e.Docker, st, tracked),
		helper: VolumeHelper{D: e.Docker, Image: comp.Image},
		key:    key, store: CheckpointStore{Dir: e.Dep.BackupDir}, opts: opts, sets: sets,
		candRetain: map[string]string{},
	}, nil
}

// Apply executes a plan: the runbook of docs/UPDATE-RUNBOOK.md up to the
// operator's approval of the switch, then the switch itself.
func (e *Engine) Apply(ctx context.Context, o ApplyOptions) (*ApplyResult, error) {
	plan, err := LoadPlan(o.PlanFile, e.Now())
	if err != nil {
		return nil, err
	}
	if o.DryRun {
		return e.dryRun(ctx, plan, o)
	}
	if !o.Confirmed {
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
	r, err := e.prepareRun(ctx, plan, o)
	if err != nil {
		return nil, err
	}
	id, err := newID("u", e.Now())
	if err != nil {
		return nil, err
	}
	r.j, err = newJournal(e.Dep, "update", id, plan.ID, plan.Workspace, plan.Release.ID, e.Now)
	if err != nil {
		return nil, err
	}
	if err := writeJSONAtomic(filepath.Join(r.j.dir, "plan.json"), plan, 0o600); err != nil {
		return nil, err
	}
	prev := r.prev.Clone()
	r.j.Previous = &prev
	if err := r.j.save(); err != nil {
		return nil, err
	}
	res, err := r.execute(ctx)
	e.publishQuiet(ctx)
	return res, err
}

// dryRun re-verifies the plan and prints what apply would do.
func (e *Engine) dryRun(ctx context.Context, plan *Plan, o ApplyOptions) (*ApplyResult, error) {
	r, err := e.prepareRun(ctx, plan, o)
	if err != nil {
		return nil, err
	}
	plans, err := e.planRetentions(ctx, r.arch, componentTargets(r.prev.Components, true))
	if err != nil {
		return nil, fmt.Errorf("the images the running services use cannot be kept for restore and rollback: %w", err)
	}
	lines := []string{
		fmt.Sprintf("dry run: plan %s for workspace %s, release %s (%s)", plan.ID, plan.Workspace, plan.Release.ID, plan.Release.Channel),
		"the manifest, the current deployment state and the backup key were re-verified; nothing was changed",
		fmt.Sprintf("the %d images of the running services are available with their exact digests; apply keeps each under its own repository before anything stops", len(plans)),
	}
	for _, p := range plans {
		if p.recovers() {
			lines = append(lines, fmt.Sprintf("  would re-tag %s (exact digest verified) as %s: %s no longer resolves", p.From, p.Keep, p.Ref))
		}
	}
	for _, s := range plan.Steps {
		lines = append(lines, "  would run: "+s)
	}
	for _, imp := range plan.Impact {
		lines = append(lines, "  impact: "+imp)
	}
	for _, w := range plan.Warnings {
		lines = append(lines, "  warning: "+w)
	}
	_ = o
	return &ApplyResult{Status: "dry-run", Lines: lines, Message: "dry run complete"}, nil
}

func (r *updateRun) result(msg string) *ApplyResult {
	res := &ApplyResult{UpdateID: r.j.ID, Status: r.j.Status, CheckpointID: r.j.CheckpointID, Message: msg}
	if r.j.Intent != nil {
		res.Generation = r.j.Intent.Generation
	}
	return res
}

// cleanupContext survives cancellation of ctx: recovery after an interrupt
// must still run.
func cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 15*time.Minute)
}

func (r *updateRun) step(id string, fn func() (string, error)) error {
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

// execute runs steps 01 to 09, the approval gate and steps 10 and 11.
func (r *updateRun) execute(ctx context.Context) (*ApplyResult, error) {
	steps := []struct {
		id string
		fn func(context.Context) (string, error)
	}{
		{"01", r.verifyRelease},
		{"02", r.checkCapacity},
		{"03", r.maintenance},
		{"04", r.drain},
		{"05", r.stopClean},
		{"06", r.snapshot},
		{"07", r.candidateVolumes},
		{"08", r.candidateSmoke},
		{"09", r.candidateIsolation},
	}
	for _, s := range steps {
		fn := s.fn
		if err := r.step(s.id, func() (string, error) { return fn(ctx) }); err != nil {
			return r.abort(ctx, err)
		}
	}
	r.teardownCandidate(ctx)
	now := r.e.Now().UTC()
	r.j.VerifiedAt = &now

	intent := r.buildIntent()
	r.j.Intent = &intent
	if err := r.j.save(); err != nil {
		return r.abort(ctx, err)
	}

	approved := r.opts.ApproveSwitch
	if !approved && r.opts.SwitchApproval != nil {
		ok, err := r.opts.SwitchApproval(r.approvalSummary())
		if err != nil {
			return r.abort(ctx, fmt.Errorf("approval: %w", err))
		}
		approved = ok
	}
	if !approved {
		recovery := fmt.Sprintf("the old services are stopped and untouched; candidate verification passed. Switch with `lensdock update approve --update %s`, or restore the old services with `lensdock update abort --update %s`", r.j.ID, r.j.ID)
		r.j.finish(JournalAwaitingApproval, recovery)
		return r.result("candidate verified; waiting for approval of the switch"), nil
	}
	return r.switchAndFinish(ctx)
}

// switchAndFinish runs steps 10 and 11.
func (r *updateRun) switchAndFinish(ctx context.Context) (*ApplyResult, error) {
	if err := r.step("10", func() (string, error) { return r.doSwitch(ctx) }); err != nil {
		return r.revert(ctx, err)
	}
	if err := r.step("11", func() (string, error) { return r.health(ctx) }); err != nil {
		// The new state was saved only after the stack became healthy. A
		// later status failure must remain visible and require the operator to
		// inspect or roll back; it is never called "completed".
		recovery := fmt.Sprintf("generation %d was activated but the final health read failed (%v). Inspect it with `lensdock update status`; roll back with `lensdock update rollback --checkpoint %s` if unhealthy", r.j.Intent.Generation, err, r.j.CheckpointID)
		r.j.finish(JournalHeld, recovery)
		return r.result(recovery), err
	}
	r.j.finish(JournalCompleted, "")
	return r.result("update completed"), nil
}

// ----- steps ---------------------------------------------------------------

func (r *updateRun) target() ArchImages { return r.rel.Manifest.Images[r.arch] }

func (r *updateRun) verifyRelease(ctx context.Context) (string, error) {
	imgs := r.target()
	m := &r.rel.Manifest
	if want := r.plan.Target[runtimeComponent(r.plan.Workspace)]; want.Ref != imgs.Runtime.Ref() {
		return "", errors.New("the plan's runtime image differs from the manifest")
	}
	if err := r.e.ensureImage(ctx, imgs.Runtime.Ref(), imgs.Runtime.Digest, r.arch); err != nil {
		return "", err
	}
	if r.e.Dep.Gateway != nil {
		if imgs.Gateway == nil {
			return "", errors.New("the release has no gateway image")
		}
		if err := r.e.ensureImage(ctx, imgs.Gateway.Ref(), imgs.Gateway.Digest, r.arch); err != nil {
			return "", err
		}
	}
	_, sum, err := r.e.linuxCLI(r.arch)
	if err != nil {
		return "", err
	}
	if sum != m.CLI[r.arch].SHA256 {
		return "", errors.New("the linux lensdock binary no longer matches the manifest")
	}
	pinned, err := r.e.retainImages(ctx, r.arch, componentTargets(r.prev.Components, true))
	if err != nil {
		return "", fmt.Errorf("the images the running services use cannot be kept for restore and rollback, so nothing was stopped: %w", err)
	}
	return fmt.Sprintf("release %s, %s, images present with exact digests; running images: %s", m.ReleaseID, m.Channel, pinned), nil
}

func (r *updateRun) checkCapacity(ctx context.Context) (string, error) {
	tmp := &Plan{}
	b := planBuilder{tmp}
	r.e.checkCapacity(ctx, b, tmp, r.prev, r.plan.Workspace)
	r.e.checkBackupTarget(b)
	if len(tmp.Blockers) > 0 {
		return "", errors.New(strings.Join(tmp.Blockers, "; "))
	}
	return fmt.Sprintf("%s of snapshot data, space for checkpoint and candidates", humanBytes(tmp.Capacity.SnapshotBytes)), nil
}

func (r *updateRun) services() (runtimeSvc, gatewaySvc string) {
	runtimeSvc = r.prev.Components[runtimeComponent(r.plan.Workspace)].Service
	if r.e.Dep.Gateway != nil {
		gatewaySvc = r.prev.Components[gatewayComponent].Service
	}
	return runtimeSvc, gatewaySvc
}

func (r *updateRun) isRunning(ctx context.Context, service string) bool {
	_, err := r.e.runningContainer(ctx, r.old, service)
	return err == nil
}

func (r *updateRun) stopService(ctx context.Context, service string) error {
	if err := r.old.Stop(ctx, time.Duration(r.e.Dep.StopGraceSeconds)*time.Second, service); err != nil {
		return err
	}
	if !contains(r.j.StoppedServices, service) {
		r.j.StoppedServices = append(r.j.StoppedServices, service)
	}
	return r.j.save()
}

func (r *updateRun) maintenance(ctx context.Context) (string, error) {
	rtSvc, gwSvc := r.services()
	var notes []string
	if gwSvc != "" && r.isRunning(ctx, gwSvc) {
		if err := r.stopService(ctx, gwSvc); err != nil {
			return "", fmt.Errorf("stop gateway: %w", err)
		}
		notes = append(notes, "gateway stopped (no new leases through the UI or agents)")
	}
	if !r.isRunning(ctx, rtSvc) {
		return strings.Join(append(notes, "runtime is not running"), "; "), nil
	}
	resp, err := r.e.runtimeProbe(ctx, r.plan.Workspace, r.prev, r.tracked, ProbeRequest{Op: ProbeMaintenanceEnter, Reason: "lensdock update " + r.j.ID})
	switch {
	case errors.Is(err, ErrNoOperatorToken):
		notes = append(notes, "no operator token: runtime not fenced, the clean stop revokes every lease")
	case err != nil:
		return "", fmt.Errorf("cannot fence the runtime: %w", err)
	case resp.Code == CodeUnsupported:
		notes = append(notes, "runtime has no maintenance endpoint: the clean stop revokes every lease")
	case resp.Error != "":
		return "", fmt.Errorf("cannot fence the runtime: %s", resp.Error)
	default:
		r.j.MaintenanceFenced = true
		if err := r.j.save(); err != nil {
			return "", err
		}
		drained := false
		if resp.Maintenance != nil {
			drained = resp.Maintenance.Drained
		}
		notes = append(notes, fmt.Sprintf("runtime fenced in MAINTENANCE (drained=%t)", drained))
	}
	return strings.Join(notes, "; "), nil
}

func (r *updateRun) drain(ctx context.Context) (string, error) {
	rtSvc, _ := r.services()
	if !r.isRunning(ctx, rtSvc) {
		return "runtime not running", nil
	}
	if r.ws.OperatorTokenFile == "" {
		return "lease state not inspectable without an operator token; the clean stop revokes every lease", nil
	}
	timeout := time.Duration(r.e.Dep.DrainTimeoutSeconds) * time.Second
	var last RuntimeStatus
	probe := func(ctx context.Context) (bool, error) {
		resp, err := r.e.runtimeProbe(ctx, r.plan.Workspace, r.prev, r.tracked, ProbeRequest{Op: ProbeStatus})
		if err != nil {
			return false, err
		}
		if perr := probeErr(resp); perr != nil || resp.Status == nil {
			return false, fmt.Errorf("status: %v", perr)
		}
		last = *resp.Status
		return !last.LeaseHeld(), nil
	}
	err := r.e.waitUntil(ctx, timeout, 2*time.Second, probe)
	if errors.Is(err, ErrNoOperatorToken) {
		return "lease state not inspectable without an operator token; the clean stop revokes every lease", nil
	}
	if err == nil {
		return "no active lease", nil
	}
	if !last.LeaseHeld() {
		return "", fmt.Errorf("cannot read the lease state: %w", err)
	}
	if !r.opts.RevokeLeases {
		return "", fmt.Errorf("a %s lease held by %s is still active after %s: wait, or run again with --revoke-leases (a revoked lease cannot undo effects already sent to websites)", last.LeaseMode, last.HolderID, timeout)
	}
	resp, err := r.e.runtimeProbe(ctx, r.plan.Workspace, r.prev, r.tracked, ProbeRequest{Op: ProbeRevokeLease, LeaseID: last.LeaseID})
	if err != nil {
		return "", fmt.Errorf("revoke lease: %w", err)
	}
	if perr := probeErr(resp); perr != nil {
		return "", fmt.Errorf("revoke lease: %w", perr)
	}
	return fmt.Sprintf("revoked the %s lease of %s (--revoke-leases)", last.LeaseMode, last.HolderID), nil
}

// profileVolume is the volume of the workspace's profile dataset.
func (r *updateRun) profileVolume() string {
	for _, ds := range r.sets {
		if ds.Owner == runtimeComponent(r.plan.Workspace) && ds.Kind == KindProfile {
			return ds.Volume
		}
	}
	return ""
}

// profileExitType extracts profile.exit_type from a Chromium Preferences file.
func profileExitType(prefs []byte) (string, error) {
	var doc struct {
		Profile struct {
			ExitType string `json:"exit_type"`
		} `json:"profile"`
	}
	if err := json.Unmarshal(prefs, &doc); err != nil {
		return "", fmt.Errorf("Default/Preferences is not JSON: %w", err)
	}
	return doc.Profile.ExitType, nil
}

func profileExitedCleanly(exitType string) bool {
	// Chromium records SessionEnded when the OS requests a graceful shutdown.
	return exitType == "Normal" || exitType == "SessionEnded"
}

func (r *updateRun) lastExitCode(ctx context.Context, service string) (*int, error) {
	ids, err := r.old.ContainerIDs(ctx, service)
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	ci, err := r.e.Docker.ContainerInspect(ctx, ids[0])
	if err != nil {
		return nil, err
	}
	if ci.State.Running {
		return nil, fmt.Errorf("%s is still running", service)
	}
	code := ci.State.ExitCode
	return &code, nil
}

func (r *updateRun) stopClean(ctx context.Context) (string, error) {
	rtSvc, gwSvc := r.services()
	if r.isRunning(ctx, rtSvc) {
		if err := r.stopService(ctx, rtSvc); err != nil {
			return "", fmt.Errorf("stop runtime: %w", err)
		}
	}
	var reasons []string
	code, err := r.lastExitCode(ctx, rtSvc)
	if err != nil {
		return "", fmt.Errorf("runtime exit status: %w", err)
	}
	r.clean.RuntimeExitCode = code
	if code != nil && *code != 0 {
		reasons = append(reasons, fmt.Sprintf("the runtime exited with status %d (137 means it was killed after the grace period)", *code))
	}
	if gwSvc != "" && contains(r.j.StoppedServices, gwSvc) {
		gcode, err := r.lastExitCode(ctx, gwSvc)
		if err != nil {
			return "", fmt.Errorf("gateway exit status: %w", err)
		}
		r.clean.GatewayExitCode = gcode
		if gcode != nil && *gcode != 0 {
			reasons = append(reasons, fmt.Sprintf("the gateway exited with status %d", *gcode))
		}
	}
	vol := r.profileVolume()
	singles, err := r.helper.TopLevelNames(ctx, vol, "Singleton*")
	if err != nil {
		return "", fmt.Errorf("check profile locks: %w", err)
	}
	r.clean.SingletonEntries = singles
	if len(singles) > 0 {
		reasons = append(reasons, "Chromium left profile lock files ("+strings.Join(singles, ", ")+")")
	}
	prefs, found, err := r.helper.ReadFile(ctx, vol, "Default/Preferences", 8<<20)
	if err != nil {
		return "", fmt.Errorf("read profile preferences: %w", err)
	}
	if found {
		et, err := profileExitType(prefs)
		if err != nil {
			return "", err
		}
		r.clean.ProfileExitType = et
		if !profileExitedCleanly(et) {
			reasons = append(reasons, fmt.Sprintf("profile.exit_type %q does not identify a clean shutdown", et))
		}
	}
	r.clean.Reasons = reasons
	if len(reasons) > 0 {
		if !r.opts.AcceptCrashSnapshot {
			return "", fmt.Errorf("the stop was not clean, a snapshot would be a crash snapshot: %s (the previous services are restored; --accept-crash-snapshot continues anyway)", strings.Join(reasons, "; "))
		}
		return "NOT clean, crash snapshot accepted: " + strings.Join(reasons, "; "), nil
	}
	detail := "runtime exited 0, no profile lock files"
	if found {
		detail += ", profile.exit_type " + r.clean.ProfileExitType
	} else {
		detail += ", profile has no Preferences yet"
	}
	return detail, nil
}

func (r *updateRun) snapshot(ctx context.Context) (string, error) {
	id, err := newID("cp", r.e.Now())
	if err != nil {
		return "", err
	}
	w, err := r.store.Begin(id, r.key)
	if err != nil {
		return "", err
	}
	var datasets []CheckpointDataset
	var retained []string
	for _, ds := range r.sets {
		if ds.Role != RoleSnapshot {
			retained = append(retained, ds.Key)
			continue
		}
		r.e.logf("snapshot %s (%s)", ds.Key, ds.Kind)
		cd, err := w.WriteDataset(ctx, r.helper, ds.Owner, ds.Kind, ds.Key, ds.Volume)
		if err != nil {
			w.Abort()
			return "", err
		}
		hot, err := r.helper.uncleanJournals(ctx, ds.Volume, ds.Kind, cd.Fingerprint)
		if err != nil {
			w.Abort()
			return "", err
		}
		if len(hot) > 0 {
			r.clean.HotJournals = append(r.clean.HotJournals, hot...)
			r.clean.Reasons = append(r.clean.Reasons, fmt.Sprintf("%s holds unflushed journals (%s)", ds.Key, strings.Join(hot, ", ")))
		}
		datasets = append(datasets, cd)
	}
	consistency := ConsistencyClean
	if len(r.clean.Reasons) > 0 {
		if !r.opts.AcceptCrashSnapshot {
			w.Abort()
			return "", fmt.Errorf("the stores were not closed cleanly: %s", strings.Join(r.clean.Reasons, "; "))
		}
		consistency = ConsistencyCrash
	}
	now := r.e.Now().UTC()
	cp := Checkpoint{
		CreatedAt: now, UpdateID: r.j.ID, Workspace: r.plan.Workspace, Consistency: consistency, Clean: r.clean,
		Previous: r.prev.Clone(), TargetRelease: r.plan.Release.ID, Datasets: datasets,
		LastVerifiedAt: &now, RetainedNotRolledBack: retained,
	}
	if cur := r.e.currentRelease(r.prev); cur != nil {
		cp.ChromiumBefore = cur.Manifest.Components.Chromium
		cp.GatewayDBSchemaBefore = cur.Manifest.Schema.GatewayDB
	}
	if err := w.Commit(cp); err != nil {
		w.Abort()
		return "", err
	}
	r.cp = &cp
	r.cp.ID, r.cp.KeyID = id, KeyID(r.key)
	r.j.CheckpointID = id
	if err := r.j.save(); err != nil {
		return "", err
	}
	var total int64
	for _, d := range datasets {
		total += d.SealedBytes
	}
	return fmt.Sprintf("checkpoint %s (%s, %d datasets, %s sealed, verified by decrypting)", id, consistency, len(datasets), humanBytes(total)), nil
}

// candidateName builds a volume name for the candidate of dataset key.
func (r *updateRun) candidateName(kind, key string) string {
	short := r.j.ID[len(r.j.ID)-6:]
	return fmt.Sprintf("%s-%s-%s-g%d-%s", r.e.Dep.Compose.Project, kind, key, r.prev.Generation+1, short)
}

func (r *updateRun) labels(role, dataset string) map[string]string {
	return map[string]string{
		LabelManaged: "true", LabelUpdate: r.j.ID, LabelRole: role, LabelDataset: dataset,
		LabelCreated: r.e.Now().UTC().Format(time.RFC3339),
	}
}

func (r *updateRun) candidateVolumes(ctx context.Context) (string, error) {
	r.candidate = CandidateRecord{Volumes: map[string]string{}}
	r.j.Candidate = &r.candidate
	var cloned int
	for _, ds := range r.sets {
		switch ds.Role {
		case RoleSnapshot:
			cd, ok := r.cp.Dataset(ds.Owner, ds.Key)
			if !ok {
				return "", fmt.Errorf("checkpoint has no dataset %s/%s", ds.Owner, ds.Key)
			}
			name := r.candidateName("cand", ds.Key)
			if err := r.e.Docker.VolumeCreate(ctx, name, r.labels(RoleCandidate, ds.Owner+"/"+ds.Key)); err != nil {
				return "", err
			}
			r.candidate.Volumes[ds.Key] = name
			if err := r.j.save(); err != nil {
				return "", err
			}
			rd, closer, err := r.store.OpenDataset(*r.cp, cd, r.key)
			if err != nil {
				return "", err
			}
			err = r.helper.TarIn(ctx, name, rd)
			if err == nil {
				// Reading to EOF authenticates the final frame.
				_, err = io.Copy(io.Discard, rd)
			}
			_ = closer.Close()
			if err != nil {
				return "", fmt.Errorf("clone %s into %s: %w", ds.Key, name, err)
			}
			fp, err := r.helper.Fingerprint(ctx, name)
			if err != nil {
				return "", err
			}
			if !fp.Equal(cd.Fingerprint) {
				return "", fmt.Errorf("candidate volume %s differs from the checkpoint of %s (fingerprint %s, expected %s)", name, ds.Key, fp.SHA256[:12], cd.Fingerprint.SHA256[:12])
			}
			cloned++
		case RoleRetained:
			if ds.CandidateAccess == CandidateReadOnly {
				r.candRetain[ds.Key] = ds.Volume
				continue
			}
			name := r.candidateName("scratch", ds.Key)
			if err := r.e.Docker.VolumeCreate(ctx, name, r.labels(RoleScratch, ds.Owner+"/"+ds.Key)); err != nil {
				return "", err
			}
			r.candidate.Scratch = append(r.candidate.Scratch, name)
			r.candRetain[ds.Key] = name
			if err := r.j.save(); err != nil {
				return "", err
			}
		}
	}
	return fmt.Sprintf("%d volumes cloned from the checkpoint and compared by fingerprint; retained data is %s", cloned, "scratch or read-only"), nil
}

// candidateMapping maps every volume key of a service to the volume a
// candidate sees.
func (r *updateRun) candidateMapping(owner string) map[string]string {
	out := map[string]string{}
	for _, ds := range r.sets {
		if ds.Owner != owner {
			continue
		}
		if ds.Role == RoleSnapshot {
			out[ds.Key] = r.candidate.Volumes[ds.Key]
		} else {
			out[ds.Key] = r.candRetain[ds.Key]
		}
	}
	return out
}

func (r *updateRun) expectedPackages() map[string]string {
	out := map[string]string{}
	for _, p := range r.rel.Manifest.APT.Packages {
		if p.Arch == r.arch || p.Arch == "all" {
			out[p.Name] = p.Version
		}
	}
	return out
}

func (r *updateRun) candidateSmoke(ctx context.Context) (string, error) {
	var notes []string
	if g := r.e.Dep.Gateway; g != nil && len(g.MigrateArgv) > 0 {
		if err := r.migrateGateway(ctx); err != nil {
			return "", err
		}
		notes = append(notes, "gateway migration ran on the cloned database")
	}
	tok, err := auth.NewSecret("ldo_")
	if err != nil {
		return "", err
	}
	r.candToken = tok.Reveal()
	rtSvc, _ := r.services()
	short := r.j.ID[len(r.j.ID)-6:]
	r.candProject = fmt.Sprintf("%s-cand-%s", r.e.Dep.Compose.Project, short)
	r.candidate.Project = r.candProject
	if err := r.j.save(); err != nil {
		return "", err
	}
	overridePath := filepath.Join(r.j.dir, "candidate.override.yaml")
	data, err := RenderCandidateOverride(CandidateOverride{
		Service: rtSvc, Image: r.target().Runtime.Ref(), TokenDigestHex: auth.DigestOf(r.candToken).Hex(),
		Volumes: r.candidateMapping(runtimeComponent(r.plan.Workspace)), BlankEnv: RuntimeTLSEnv,
	})
	if err != nil {
		return "", err
	}
	if err := writeFileAtomic(overridePath, data, 0o600); err != nil {
		return "", err
	}
	cc := r.candidateCompose(overridePath)
	if _, err := cc.Config(ctx); err != nil {
		return "", fmt.Errorf("candidate configuration: %w", err)
	}
	r.e.logf("starting the candidate in the isolated project %s", r.candProject)
	err = cc.Up(ctx, UpOptions{NoDeps: true, Wait: true, WaitTimeout: time.Duration(r.e.Dep.HealthTimeoutSeconds) * time.Second}, rtSvc)
	if err != nil {
		r.collectCandidateLogs(ctx, cc, rtSvc)
		return "", fmt.Errorf("start candidate: %w", err)
	}
	ids, err := cc.ContainerIDs(ctx, rtSvc)
	if err != nil || len(ids) == 0 {
		return "", fmt.Errorf("candidate container not found: %v", err)
	}
	cli, _, err := r.e.linuxCLI(r.arch)
	if err != nil {
		return "", err
	}
	req := ProbeRequest{
		Op: ProbeVerifyCandidate, BaseURL: r.ws.Probe.CandidateProbeURL(), Workspace: r.plan.Workspace, Token: r.candToken,
		Verify: &VerifySpec{ExpectChromium: r.rel.Manifest.Components.Chromium, ExpectPackages: r.expectedPackages()},
	}
	resp, err := r.e.sidecarProbe(ctx, r.target().Runtime.Ref(), ids[0], cli, req)
	if err != nil {
		r.collectCandidateLogs(ctx, cc, rtSvc)
		return "", err
	}
	r.collectCandidateLogs(ctx, cc, rtSvc)
	if perr := probeErr(resp); perr != nil || resp.Verify == nil {
		return "", fmt.Errorf("candidate verification did not run: %v", perr)
	}
	r.report = resp.Verify
	if err := writeJSONAtomic(r.j.evidencePath("candidate-verify.json"), resp.Verify, 0o600); err != nil {
		return "", err
	}
	var failed []string
	sandboxOn := false
	for _, c := range resp.Verify.Checks {
		if c.blocks() && c.ID != "egress-denied" {
			failed = append(failed, c.ID+": "+c.Detail)
		}
		if c.ID == "sandbox" && c.Status == CheckPass {
			sandboxOn = true
		}
	}
	if len(failed) > 0 {
		sort.Strings(failed)
		return "", fmt.Errorf("candidate verification failed: %s", strings.Join(failed, "; "))
	}
	passed := "fonts, packages, Chromium, CDP and the local fixture passed"
	if sandboxOn {
		passed = "sandbox, " + passed
	}
	notes = append(notes, passed)
	return strings.Join(notes, "; "), nil
}

func (r *updateRun) candidateCompose(overridePath string) ComposeCLI {
	files := append(append([]string{}, r.e.Dep.Compose.Files...), overridePath)
	return ComposeCLI{D: r.e.Docker, Project: r.candProject, Files: files, EnvFiles: r.e.Dep.Compose.EnvFiles, Dir: r.e.Dep.Compose.ProjectDir}
}

func (r *updateRun) collectCandidateLogs(ctx context.Context, cc ComposeCLI, service string) {
	f, err := os.OpenFile(r.j.evidencePath("candidate-runtime.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	_ = cc.Logs(lctx, f, service)
}

func (r *updateRun) candidateIsolation(ctx context.Context) (string, error) {
	if r.report == nil {
		return "", errors.New("no candidate verification report")
	}
	var egress *Check
	for i := range r.report.Checks {
		if r.report.Checks[i].ID == "egress-denied" {
			egress = &r.report.Checks[i]
		}
	}
	if egress == nil || egress.Status != CheckPass {
		detail := "no egress check ran"
		if egress != nil {
			detail = egress.Detail
		}
		return "", fmt.Errorf("the candidate is not isolated: %s", detail)
	}
	internal, err := r.e.Docker.NetworkInternal(ctx, r.candProject+"_default")
	if err != nil {
		return "", fmt.Errorf("inspect the candidate network: %w", err)
	}
	if !internal {
		return "", errors.New("the candidate network is not internal")
	}
	if err := r.stopCandidateClean(ctx); err != nil {
		return "", err
	}
	return "internal network without published ports; public addresses, DNS and the metadata address unreachable; candidate Chromium stopped cleanly", nil
}

func (r *updateRun) stopCandidateClean(ctx context.Context) error {
	cc := r.candidateCompose(filepath.Join(r.j.dir, "candidate.override.yaml"))
	if err := cc.Down(ctx); err != nil {
		return fmt.Errorf("stop candidate cleanly: %w", err)
	}
	r.candProject = ""
	profile := ""
	for _, ds := range r.sets {
		if ds.Owner == runtimeComponent(r.plan.Workspace) && ds.Kind == KindProfile {
			profile = r.candidate.Volumes[ds.Key]
			break
		}
	}
	locks, err := r.helper.TopLevelNames(ctx, profile, "Singleton*")
	if err != nil {
		return fmt.Errorf("check candidate Profile lock: %w", err)
	}
	if len(locks) > 0 {
		return fmt.Errorf("candidate Chromium left Profile lock files: %s", strings.Join(locks, ", "))
	}
	prefs, found, err := r.helper.ReadFile(ctx, profile, "Default/Preferences", 8<<20)
	if err != nil {
		return err
	}
	if found {
		exitType, err := profileExitType(prefs)
		if err != nil {
			return err
		}
		if !profileExitedCleanly(exitType) {
			return fmt.Errorf("candidate profile.exit_type %q does not identify a clean shutdown", exitType)
		}
	}
	for _, v := range r.candidate.Scratch {
		if err := r.e.Docker.VolumeRemove(ctx, v); err != nil {
			return fmt.Errorf("remove candidate scratch volume %s: %w", v, err)
		}
	}
	r.candidate.Scratch = nil
	return r.j.save()
}

// teardownCandidate removes the candidate project and its scratch volumes.
// The cloned candidate volumes stay: they become production on switch.
func (r *updateRun) teardownCandidate(ctx context.Context) {
	cctx, cancel := cleanupContext(ctx)
	defer cancel()
	if r.candProject != "" {
		cc := r.candidateCompose(filepath.Join(r.j.dir, "candidate.override.yaml"))
		if _, err := os.Stat(filepath.Join(r.j.dir, "candidate.override.yaml")); err == nil {
			if err := cc.Down(cctx); err != nil {
				r.e.logf("warning: remove candidate project: %v", err)
			}
		}
	}
	for _, v := range r.candidate.Scratch {
		if err := r.e.Docker.VolumeRemove(cctx, v); err != nil {
			r.e.logf("warning: remove scratch volume %s: %v", v, err)
		}
	}
	r.candidate.Scratch = nil
	_ = r.j.save()
}

func (r *updateRun) migrateGateway(ctx context.Context) error {
	g := r.e.Dep.Gateway
	img := r.target().Gateway
	if img == nil {
		return errors.New("no gateway image in the release")
	}
	args := []string{
		"run", "--rm", "--network", "none", "--read-only", "--cap-drop", "ALL",
		"--security-opt", "no-new-privileges:true",
		"--user", g.User, "--tmpfs", "/tmp:rw,noexec,nosuid,size=64m",
	}
	for _, ds := range r.sets {
		if ds.Owner != gatewayComponent {
			continue
		}
		vol := r.candidateMapping(gatewayComponent)[ds.Key]
		ro := ds.Role == RoleRetained && ds.CandidateAccess == CandidateReadOnly
		args = append(args, "--mount", mountArg(vol, ds.MountPath, ro))
	}
	args = append(args, img.Ref())
	args = append(args, g.MigrateArgv...)
	f, err := os.OpenFile(r.j.evidencePath("gateway-migrate.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	r.e.logf("running the gateway migration on the cloned database")
	if err := r.e.Docker.Run.Run(ctx, Cmd{Name: r.e.Docker.bin(), Args: args, Stdout: f, Stderr: f}); err != nil {
		return fmt.Errorf("gateway migration failed on the clone (log: %s): %w", filepath.Base(f.Name()), err)
	}
	return nil
}

// buildIntent is the state the switch activates.
func (r *updateRun) buildIntent() State {
	next := r.prev.Clone()
	next.Generation = r.prev.Generation + 1
	next.ReleaseID = r.plan.Release.ID
	next.UpdatedAt = r.e.Now().UTC()
	next.LastUpdateID = r.j.ID
	imgs := r.target()
	rt := next.Components[runtimeComponent(r.plan.Workspace)]
	rt.ReleaseID, rt.Image, rt.ImageID = r.plan.Release.ID, imgs.Runtime.Ref(), imgs.Runtime.Digest
	next.Components[runtimeComponent(r.plan.Workspace)] = rt
	if r.e.Dep.Gateway != nil && imgs.Gateway != nil {
		gw := next.Components[gatewayComponent]
		gw.ReleaseID, gw.Image, gw.ImageID = r.plan.Release.ID, imgs.Gateway.Ref(), imgs.Gateway.Digest
		next.Components[gatewayComponent] = gw
	}
	for _, ds := range r.sets {
		if ds.Role != RoleSnapshot {
			continue
		}
		c := next.Components[ds.Owner]
		c.Volumes[ds.Key] = r.candidate.Volumes[ds.Key]
		next.Components[ds.Owner] = c
	}
	return next
}

func (r *updateRun) approvalSummary() string {
	var b strings.Builder
	m := &r.rel.Manifest
	fmt.Fprintf(&b, "Switch workspace %s to release %s (%s)?\n", r.plan.Workspace, m.ReleaseID, m.Channel)
	for _, d := range r.plan.Diff {
		fmt.Fprintf(&b, "  %s: %s -> %s\n", d.Component, orDash(d.From), d.To)
	}
	fmt.Fprintf(&b, "  candidate verification: passed (%d checks) at %s\n", len(r.report.Checks), r.j.VerifiedAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "  checkpoint %s taken at %s: the rollback target (older images with exactly this data)\n", r.cp.ID, r.cp.CreatedAt.Format(time.RFC3339))
	b.WriteString("  the old services stay stopped until you answer; the old volumes are kept as a retired branch\n")
	return b.String()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ----- switch --------------------------------------------------------------

func (r *updateRun) doSwitch(ctx context.Context) (string, error) {
	if r.j.Intent == nil {
		return "", errors.New("the journal has no switch intent")
	}
	next := *r.j.Intent
	rtSvc, gwSvc := r.services()
	for _, svc := range []string{rtSvc, gwSvc} {
		if svc != "" && r.isRunning(ctx, svc) {
			return "", fmt.Errorf("service %s was started again after the checkpoint: its data changed after the snapshot; abort this update and make a new plan", svc)
		}
	}
	for key, name := range r.candidate.Volumes {
		if _, err := r.e.Docker.VolumeInspect(ctx, name); err != nil {
			return "", fmt.Errorf("candidate volume %s of %s: %w", name, key, err)
		}
	}
	// The new images must stay resolvable by repository@digest once their
	// tag (:dev) moves on. Nothing is switched yet, so a failure here reverts
	// to the previous generation.
	if _, err := r.e.retainImages(ctx, r.arch, componentTargets(next.Components, false)); err != nil {
		return "", fmt.Errorf("keep the images of the new generation: %w", err)
	}
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
		return "", fmt.Errorf("new generation configuration: %w", err)
	}
	timeout := time.Duration(r.e.Dep.HealthTimeoutSeconds) * time.Second
	if err := nc.Up(ctx, UpOptions{NoDeps: true, Wait: true, WaitTimeout: timeout}, rtSvc); err != nil {
		return "", fmt.Errorf("start the new runtime: %w", err)
	}
	if err := r.e.waitUntil(ctx, timeout, 2*time.Second, func(ctx context.Context) (bool, error) {
		resp, err := r.e.runtimeProbe(ctx, r.plan.Workspace, next, true, ProbeRequest{Op: ProbeStatus})
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
		return "", fmt.Errorf("the new runtime did not become ready: %w", err)
	}
	if gwSvc != "" {
		if err := nc.Up(ctx, UpOptions{NoDeps: true, Wait: true, WaitTimeout: timeout}, gwSvc); err != nil {
			return "", fmt.Errorf("start the new gateway: %w", err)
		}
		if url := r.e.Dep.Gateway.ReadyURL; url != "" {
			if err := r.e.waitUntil(ctx, timeout, 2*time.Second, func(ctx context.Context) (bool, error) { return httpOK(ctx, url) }); err != nil {
				return "", fmt.Errorf("the new gateway did not become ready: %w", err)
			}
		}
	}
	if err := SaveState(r.e.Dep, next); err != nil {
		return "", fmt.Errorf("record the new generation: %w", err)
	}
	return fmt.Sprintf("generation %d is active", next.Generation), nil
}

// httpOK performs a plain GET and reports whether the status is 200.
func httpOK(ctx context.Context, url string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}
	hc := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := hc.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return true, nil
}

func (r *updateRun) health(ctx context.Context) (string, error) {
	next := *r.j.Intent
	detail := fmt.Sprintf("generation %d", next.Generation)
	for _, ds := range r.sets {
		if ds.Role == RoleSnapshot {
			r.j.RetiredVolumes = append(r.j.RetiredVolumes, ds.Volume)
		}
	}
	if err := r.j.save(); err != nil {
		return detail, err
	}
	resp, err := r.e.runtimeProbe(ctx, r.plan.Workspace, next, true, ProbeRequest{Op: ProbeStatus})
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
		detail += fmt.Sprintf("; runtime generation %s, control state %s: users and agents must acquire new leases", resp.Status.RuntimeGeneration, resp.Status.ControlState)
	}

	if err := r.j.save(); err != nil {
		return detail, err
	}
	return detail, nil
}

// ----- failure handling -----------------------------------------------------

// restoreOld starts again what this operation stopped, in reverse order of
// stopping, from the previous generation (old images, old volumes), and lifts
// a runtime fence that is still active.
func (r *updateRun) restoreOld(ctx context.Context) error {
	cctx, cancel := cleanupContext(ctx)
	defer cancel()
	rtSvc, _ := r.services()
	if r.j.MaintenanceFenced && !contains(r.j.StoppedServices, rtSvc) {
		resp, err := r.e.runtimeProbe(cctx, r.plan.Workspace, r.prev, r.tracked, ProbeRequest{Op: ProbeMaintenanceExit})
		if err == nil && probeErr(resp) == nil {
			r.j.MaintenanceFenced = false
			_ = r.j.save()
		} else {
			r.e.logf("warning: could not lift the runtime fence: %v %v", err, probeErr(resp))
		}
	}
	if len(r.j.StoppedServices) > 0 {
		// Operations journaled before images were kept under their own
		// repository may have lost the name compose pins. Re-tagging a local
		// copy with the exact digest is the only repair attempted; the restart
		// below reports Docker's own error when none exists.
		if _, err := r.e.retainImages(cctx, r.arch, componentTargets(r.prev.Components, true)); err != nil {
			r.e.logf("warning: previous images: %v", err)
		}
	}
	timeout := time.Duration(r.e.Dep.HealthTimeoutSeconds) * time.Second
	var errs []error
	for i := len(r.j.StoppedServices) - 1; i >= 0; i-- {
		svc := r.j.StoppedServices[i]
		r.e.logf("restoring %s on the previous generation", svc)
		if err := r.old.Up(cctx, UpOptions{NoDeps: true, Wait: true, WaitTimeout: timeout}, svc); err != nil {
			errs = append(errs, fmt.Errorf("restart %s: %w", svc, err))
		}
	}
	if len(errs) == 0 {
		r.j.StoppedServices = nil
		r.j.MaintenanceFenced = false
		_ = r.j.save()
	}
	return errors.Join(errs...)
}

// removeCandidates deletes the volumes this run created for the candidate
// (clones and scratch). The checkpoint stays.
func (r *updateRun) removeCandidates(ctx context.Context) {
	cctx, cancel := cleanupContext(ctx)
	defer cancel()
	for _, v := range r.candidate.Volumes {
		if err := r.e.Docker.VolumeRemove(cctx, v); err != nil {
			r.e.logf("warning: remove candidate volume %s: %v", v, err)
		}
	}
	r.candidate.Volumes = nil
	for _, v := range r.candidate.Scratch {
		_ = r.e.Docker.VolumeRemove(cctx, v)
	}
	r.candidate.Scratch = nil
	_ = r.j.save()
}

// abort handles a failure before the switch: nothing the operator cares about
// changed, so the previous services come back. Candidate volumes of a failed
// verification are kept as evidence; a failed clone is deleted.
func (r *updateRun) abort(ctx context.Context, cause error) (*ApplyResult, error) {
	r.teardownCandidate(ctx)
	failedStep := ""
	for _, s := range r.j.Steps {
		if s.Status == StepFailed {
			failedStep = s.ID
		}
	}
	if failedStep == "07" || failedStep == "" {
		r.removeCandidates(ctx)
	}
	if err := r.restoreOld(ctx); err != nil {
		recovery := fmt.Sprintf("the previous services did not restart automatically (%v). Fix the cause, then run `lensdock update abort --update %s` to retry the restore. The checkpoint %s and all volumes are intact", err, r.j.ID, r.j.CheckpointID)
		r.j.finish(JournalHeld, recovery)
		return r.result(recovery), fmt.Errorf("%w; the previous services also failed to restart: %v", cause, err)
	}
	recovery := "the previous services run again on their own volumes; nothing was switched"
	if len(r.candidate.Volumes) > 0 {
		recovery += "; the candidate volumes are kept for diagnosis (lensdock update branches)"
	}
	r.j.finish(JournalAborted, recovery)
	return r.result(recovery), cause
}

// revert handles a failed switch: the new containers are stopped and the
// previous generation is started again on its own volumes, which the new
// Chromium never opened.
func (r *updateRun) revert(ctx context.Context, cause error) (*ApplyResult, error) {
	cctx, cancel := cleanupContext(ctx)
	defer cancel()
	rtSvc, gwSvc := r.services()
	if r.j.Intent != nil {
		nc := r.e.Dep.composeFor(r.e.Docker, *r.j.Intent, true)
		var svcs []string
		for _, s := range []string{gwSvc, rtSvc} {
			if s != "" {
				svcs = append(svcs, s)
			}
		}
		if err := nc.Stop(cctx, time.Duration(r.e.Dep.StopGraceSeconds)*time.Second, svcs...); err != nil {
			r.e.logf("warning: stop the new services: %v", err)
		}
	}
	if err := r.restoreOld(ctx); err != nil {
		recovery := fmt.Sprintf("the switch failed and the previous generation could not be restarted (%v). The old volumes and checkpoint %s are intact; run `lensdock update abort --update %s` after fixing the cause", err, r.j.CheckpointID, r.j.ID)
		r.j.finish(JournalHeld, recovery)
		return r.result(recovery), fmt.Errorf("%w; the previous services also failed to restart: %v", cause, err)
	}
	if st, ok, err := LoadState(r.e.Dep); err == nil && ok && st.LastUpdateID == r.j.ID && r.j.Previous != nil {
		if err := SaveState(r.e.Dep, r.prev); err != nil {
			recovery := fmt.Sprintf("the previous services run but deployment state could not be restored (%v); run abort again after fixing the state directory", err)
			r.j.finish(JournalHeld, recovery)
			return r.result(recovery), err
		}
	}
	r.j.finish(JournalReverted, "the new generation was stopped and the previous generation runs again on its own volumes; the candidate volumes are kept for diagnosis")
	return r.result("switch failed; previous generation restored"), cause
}

// ----- approve and abort ------------------------------------------------------

// ApproveOptions are the inputs of Approve.
type ApproveOptions struct {
	UpdateID  string
	Confirmed bool
}

// resume rebuilds the run of a journal that waits for approval or was
// interrupted.
func (e *Engine) resume(ctx context.Context, id string) (*updateRun, error) {
	j, err := loadJournal(e.Dep, id, e.Now)
	if err != nil {
		return nil, err
	}
	if j.Kind != "update" {
		return nil, fmt.Errorf("%s is a %s, not an update", id, j.Kind)
	}
	plan, err := loadPlanFile(filepath.Join(j.dir, "plan.json"))
	if err != nil {
		return nil, err
	}
	r, err := e.prepareRun(ctx, plan, ApplyOptions{})
	if err != nil && j.Intent != nil && j.Previous != nil {
		// The host may have stopped after SaveState but before the journal's
		// final step. Reconstruct both generations from the journal so abort
		// can stop the intent and restore the previous generation.
		cur, ok, stateErr := LoadState(e.Dep)
		if stateErr == nil && ok && cur.Generation == j.Intent.Generation && cur.LastUpdateID == j.ID {
			rel, relErr := LoadRelease(plan.Release.Dir, VerifyPolicy{TrustedKeys: e.Dep.TrustedKeys, Now: e.Now})
			ws, wsErr := e.Dep.Workspace(plan.Workspace)
			arch, archErr := e.engineArch(ctx)
			key, keyErr := LoadBackupKey(e.Dep.BackupKeyFile)
			sets, setsErr := e.datasetsFor(plan.Workspace, *j.Previous)
			switch {
			case relErr != nil:
				err = relErr
			case wsErr != nil:
				err = wsErr
			case archErr != nil:
				err = archErr
			case keyErr != nil:
				err = keyErr
			case setsErr != nil:
				err = setsErr
			default:
				prevRuntime := j.Previous.Components[runtimeComponent(plan.Workspace)]
				r = &updateRun{
					e: e, plan: plan, rel: rel, arch: arch, ws: ws, prev: j.Previous.Clone(), tracked: true,
					old:    e.Dep.composeFor(e.Docker, *j.Previous, true),
					helper: VolumeHelper{D: e.Docker, Image: prevRuntime.Image},
					key:    key, store: CheckpointStore{Dir: e.Dep.BackupDir}, sets: sets, candRetain: map[string]string{},
				}
				err = nil
			}
		}
	}
	if err != nil {
		return nil, err
	}
	r.j = j
	if j.Candidate != nil {
		r.candidate = *j.Candidate
		r.candProject = j.Candidate.Project
	}
	if r.candidate.Volumes == nil {
		r.candidate.Volumes = map[string]string{}
	}
	for _, ds := range r.sets {
		if ds.Role == RoleRetained && ds.CandidateAccess == CandidateReadOnly {
			r.candRetain[ds.Key] = ds.Volume
		}
	}
	if j.CheckpointID != "" {
		cp, err := r.store.Load(j.CheckpointID)
		if err != nil {
			return nil, err
		}
		r.cp = &cp
	}
	return r, nil
}

// Approve performs the switch of an update that passed candidate verification
// and waits for approval.
func (e *Engine) Approve(ctx context.Context, o ApproveOptions) (*ApplyResult, error) {
	if !o.Confirmed {
		return nil, ErrNotConfirmed
	}
	unlock, err := lockFile(e.Dep.lockPath())
	if err != nil {
		return nil, err
	}
	defer unlock()
	r, err := e.resume(ctx, o.UpdateID)
	if err != nil {
		return nil, err
	}
	if st, ok, err := LoadState(e.Dep); err == nil && ok && st.LastUpdateID == r.j.ID {
		return nil, fmt.Errorf("update %s already activated generation %d; inspect status, or use abort/rollback rather than approving it again", r.j.ID, st.Generation)
	}
	switch r.j.Status {
	case JournalAwaitingApproval:
	case JournalHeld:
		if r.j.step("09").Status != StepOK || r.j.Intent == nil {
			return nil, fmt.Errorf("update %s did not pass candidate verification; it cannot be approved", r.j.ID)
		}
	default:
		return nil, fmt.Errorf("update %s is %s and cannot be approved", r.j.ID, r.j.Status)
	}
	if r.j.Intent == nil || r.cp == nil {
		return nil, errors.New("the journal lacks the switch intent or the checkpoint")
	}
	r.j.Status, r.j.Error, r.j.Recovery, r.j.FinishedAt = JournalRunning, "", "", nil
	if err := r.j.save(); err != nil {
		return nil, err
	}
	res, err := r.switchAndFinish(ctx)
	e.publishQuiet(ctx)
	return res, err
}

// AbortOptions are the inputs of Abort.
type AbortOptions struct {
	UpdateID string
	// KeepCandidate keeps the candidate volumes instead of deleting them.
	KeepCandidate bool
}

// Abort ends an update that waits for approval, is held, or was interrupted:
// the previous services run again. It refuses to run while another process
// holds the deployment lock, so a live update is never aborted from outside.
func (e *Engine) Abort(ctx context.Context, o AbortOptions) (*ApplyResult, error) {
	unlock, err := lockFile(e.Dep.lockPath())
	if err != nil {
		return nil, err
	}
	defer unlock()
	j, jerr := loadJournal(e.Dep, o.UpdateID, e.Now)
	if jerr != nil {
		return nil, jerr
	}
	if j.Kind == "rollback" {
		res, err := e.abortRollbackJournal(ctx, j, o.KeepCandidate)
		e.publishQuiet(ctx)
		return res, err
	}
	r, err := e.resume(ctx, o.UpdateID)
	if err != nil {
		return nil, err
	}
	switch r.j.Status {
	case JournalAwaitingApproval, JournalHeld, JournalRunning:
	default:
		return nil, fmt.Errorf("update %s is %s: nothing to abort", r.j.ID, r.j.Status)
	}
	switchStarted := r.j.step("10").Status != StepPending
	var res *ApplyResult
	if switchStarted {
		res, err = r.revert(ctx, errors.New("aborted by the operator after the switch started"))
	} else {
		r.teardownCandidate(ctx)
		if !o.KeepCandidate {
			r.removeCandidates(ctx)
		}
		res, err = r.abortRestoreOnly(ctx)
	}
	e.publishQuiet(ctx)
	if err != nil && res != nil && (res.Status == JournalAborted || res.Status == JournalReverted) {
		return res, nil
	}
	return res, err
}

func (r *updateRun) abortRestoreOnly(ctx context.Context) (*ApplyResult, error) {
	if err := r.restoreOld(ctx); err != nil {
		recovery := fmt.Sprintf("the previous services did not restart automatically (%v); fix the cause and run abort again", err)
		r.j.finish(JournalHeld, recovery)
		return r.result(recovery), err
	}
	r.j.finish(JournalAborted, "aborted by the operator: the previous services run again; nothing was switched")
	return r.result("aborted; previous services restored"), nil
}

// loadPlanFile reads a plan without the validity check (resuming an update
// does not need a fresh plan, only an intact one).
func loadPlanFile(path string) (*Plan, error) {
	data, err := readFileLimit(path, 4<<20)
	if err != nil {
		return nil, fmt.Errorf("read plan: %w", err)
	}
	var p Plan
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("decode plan: %w", err)
	}
	if err := ensureJSONEOF(dec); err != nil {
		return nil, fmt.Errorf("decode plan: %w", err)
	}
	if planDigest(&p) != p.Digest {
		return nil, errors.New("the stored plan was modified (digest mismatch)")
	}
	return &p, nil
}
