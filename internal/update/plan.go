package update

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	planSchemaVersion = 1
	planValidity      = 24 * time.Hour
	minComposeMajor   = 2
	minComposeMinor   = 24
)

// PlanRequest is the input of BuildPlan.
type PlanRequest struct {
	// Release is a release id in releasesDir or the path of a release
	// directory / manifest.json.
	Release   string
	Workspace string
	// AllowDev admits releases of the dev channel.
	AllowDev bool
	// AllowDowngrade admits an older Chromium major or an older Gateway
	// database schema than the running one.
	AllowDowngrade bool
}

// Plan check statuses.
const (
	PlanOK   = "ok"
	PlanWarn = "warn"
	PlanFail = "fail"
)

// PlanCheck is one finding of planning.
type PlanCheck struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// PlanRelease identifies the verified manifest a plan is bound to.
type PlanRelease struct {
	ID             string   `json:"id"`
	Channel        string   `json:"channel"`
	ManifestSHA256 string   `json:"manifestSha256"`
	Signed         bool     `json:"signed"`
	SignatureKeyID string   `json:"signatureKeyId,omitempty"`
	Dir            string   `json:"dir"`
	Chromium       string   `json:"chromium"`
	Warnings       []string `json:"warnings,omitempty"`
}

// PlanCurrent is the state the plan was made against; apply refuses to run
// when the deployment drifted from it.
type PlanCurrent struct {
	Generation int                       `json:"generation"`
	Tracked    bool                      `json:"tracked"`
	ReleaseID  string                    `json:"releaseId,omitempty"`
	Components map[string]ComponentState `json:"components"`
}

// PlanImage is a pinned target image.
type PlanImage struct {
	Ref    string `json:"ref"`
	Digest string `json:"digest"`
}

// PlanDataset is a dataset the update touches.
type PlanDataset struct {
	Owner     string `json:"owner"`
	Key       string `json:"key"`
	Kind      string `json:"kind"`
	Role      string `json:"role"`
	Volume    string `json:"volume"`
	UsedBytes int64  `json:"usedBytes"`
}

// PlanCapacity documents the disk checks (RT: UP-04).
type PlanCapacity struct {
	SnapshotBytes       int64  `json:"snapshotBytes"`
	BackupFreeBytes     uint64 `json:"backupFreeBytes"`
	BackupRequiredBytes uint64 `json:"backupRequiredBytes"`
	DockerFreeBytes     int64  `json:"dockerFreeBytes"`
	DockerRequiredBytes int64  `json:"dockerRequiredBytes"`
}

// PlanRuntime is what planning saw of the running workspace.
type PlanRuntime struct {
	Inspected    bool   `json:"inspected"`
	Ready        bool   `json:"ready"`
	ControlState string `json:"controlState,omitempty"`
	LeaseHeld    bool   `json:"leaseHeld"`
	HolderID     string `json:"holderId,omitempty"`
	Note         string `json:"note,omitempty"`
}

// Plan is the dry result of `lensdock update plan`: every check with its
// evidence, the impact, and the exact image pins `apply` will use. A plan with
// blockers is reported but never written as an applicable file.
type Plan struct {
	SchemaVersion int                  `json:"schemaVersion"`
	ID            string               `json:"id"`
	CreatedAt     time.Time            `json:"createdAt"`
	ExpiresAt     time.Time            `json:"expiresAt"`
	Workspace     string               `json:"workspace"`
	Arch          string               `json:"arch"`
	Release       PlanRelease          `json:"release"`
	Current       PlanCurrent          `json:"current"`
	Target        map[string]PlanImage `json:"target"`
	Datasets      []PlanDataset        `json:"datasets"`
	Capacity      PlanCapacity         `json:"capacity"`
	Runtime       PlanRuntime          `json:"runtime"`
	Diff          []VersionDiff        `json:"diff,omitempty"`
	Risk          Risk                 `json:"risk"`
	Checks        []PlanCheck          `json:"checks"`
	Impact        []string             `json:"impact"`
	Steps         []string             `json:"steps"`
	Warnings      []string             `json:"warnings,omitempty"`
	Blockers      []string             `json:"blockers,omitempty"`
	Digest        string               `json:"digest"`
}

// VersionDiff is one changed component version.
type VersionDiff struct {
	Component string `json:"component"`
	From      string `json:"from"`
	To        string `json:"to"`
}

// Risk levels.
const (
	RiskPatch   = "patch"
	RiskMinor   = "minor"
	RiskMajor   = "major"
	RiskBlocked = "blocked"
)

// Risk classifies an update.
type Risk struct {
	Level   string   `json:"level"`
	Reasons []string `json:"reasons"`
}

func diffComponents(from *Components, to Components) []VersionDiff {
	pairs := []struct {
		name     string
		from, to string
	}{
		{"chromium", "", to.Chromium},
		{"novnc", "", to.NoVNC},
		{"agentBrowser", "", to.AgentBrowser},
		{"gateway", "", to.Gateway},
		{"cli", "", to.CLI},
	}
	if from != nil {
		pairs[0].from, pairs[1].from, pairs[2].from, pairs[3].from, pairs[4].from = from.Chromium, from.NoVNC, from.AgentBrowser, from.Gateway, from.CLI
	}
	var out []VersionDiff
	for _, p := range pairs {
		if p.from != p.to {
			out = append(out, VersionDiff{Component: p.name, From: p.from, To: p.to})
		}
	}
	return out
}

// classifyRisk compares the running release (nil when unknown) with a
// candidate.
func classifyRisk(cur *Manifest, tgt *Manifest) Risk {
	if cur == nil {
		return Risk{Level: RiskMajor, Reasons: []string{"the running release is not tracked by a manifest: treated as a major change with a full backup and candidate verification"}}
	}
	r := Risk{Level: RiskPatch}
	raise := func(level, reason string) {
		order := map[string]int{RiskPatch: 0, RiskMinor: 1, RiskMajor: 2}
		if order[level] > order[r.Level] {
			r.Level = level
		}
		r.Reasons = append(r.Reasons, reason)
	}
	if a, b := cur.ChromiumMajor(), tgt.ChromiumMajor(); a != b {
		raise(RiskMajor, fmt.Sprintf("Chromium major %d to %d: full Profile compatibility and rollback test needed", a, b))
	}
	if cur.Base.Digest != tgt.Base.Digest {
		raise(RiskMinor, "the Debian base image changes")
	}
	if cur.Schema.GatewayDB != tgt.Schema.GatewayDB {
		raise(RiskMinor, fmt.Sprintf("Gateway database schema %d to %d: the migration runs on a clone first", cur.Schema.GatewayDB, tgt.Schema.GatewayDB))
	}
	if cur.Components.NoVNC != tgt.Components.NoVNC || cur.Components.AgentBrowser != tgt.Components.AgentBrowser {
		raise(RiskMinor, "noVNC or agent-browser version changes")
	}
	if len(r.Reasons) == 0 {
		r.Reasons = []string{"package-level security update without major component changes"}
	}
	return r
}

type planBuilder struct{ p *Plan }

func (b planBuilder) add(status, id, format string, args ...any) {
	detail := fmt.Sprintf(format, args...)
	b.p.Checks = append(b.p.Checks, PlanCheck{ID: id, Status: status, Detail: detail})
	switch status {
	case PlanWarn:
		b.p.Warnings = append(b.p.Warnings, id+": "+detail)
	case PlanFail:
		b.p.Blockers = append(b.p.Blockers, id+": "+detail)
	}
}

func (b planBuilder) ok(id, format string, args ...any)   { b.add(PlanOK, id, format, args...) }
func (b planBuilder) warn(id, format string, args ...any) { b.add(PlanWarn, id, format, args...) }
func (b planBuilder) fail(id, format string, args ...any) { b.add(PlanFail, id, format, args...) }

// currentRelease loads the manifest of the running release when tracked.
func (e *Engine) currentRelease(st State) *Release {
	if st.ReleaseID == "" || !releaseIDPattern.MatchString(st.ReleaseID) {
		return nil
	}
	rel, err := LoadRelease(filepath.Join(e.Dep.ReleasesDir, st.ReleaseID), VerifyPolicy{TrustedKeys: e.Dep.TrustedKeys, Now: e.Now})
	if err != nil {
		return nil
	}
	return rel
}

// BuildPlan checks everything that can be checked without changing the
// deployment: release manifest and
// signature, exact digests, schema and Chromium compatibility, disk space,
// backup location and key, and the maintenance impact. The Browser is not
// touched. A manifest that is incomplete or invalid yields an error, not a
// plan.
func (e *Engine) BuildPlan(ctx context.Context, req PlanRequest) (*Plan, error) {
	ws, err := e.Dep.Workspace(req.Workspace)
	if err != nil {
		return nil, err
	}
	arch, err := e.engineArch(ctx)
	if err != nil {
		return nil, err
	}
	dir, err := ResolveRelease(e.Dep.ReleasesDir, req.Release)
	if err != nil {
		return nil, err
	}
	rel, err := LoadRelease(dir, VerifyPolicy{TrustedKeys: e.Dep.TrustedKeys, Now: e.Now})
	if err != nil {
		return nil, err
	}
	m := rel.Manifest

	now := e.Now().UTC()
	id, err := newID("p", now)
	if err != nil {
		return nil, err
	}
	p := &Plan{
		SchemaVersion: planSchemaVersion, ID: id, CreatedAt: now, ExpiresAt: now.Add(planValidity),
		Workspace: req.Workspace, Arch: arch,
		Release: PlanRelease{
			ID: m.ReleaseID, Channel: m.Channel, ManifestSHA256: rel.SHA256, Signed: rel.Signed,
			SignatureKeyID: rel.SignatureKeyID, Dir: dir, Chromium: m.Components.Chromium, Warnings: rel.Warnings,
		},
		Target: map[string]PlanImage{},
	}
	b := planBuilder{p}
	b.ok("manifest", "release %s (%s channel) manifest, evidence files and digests verified", m.ReleaseID, m.Channel)
	switch {
	case rel.Signed:
		b.ok("signature", "manifest signed by trusted key %s", rel.SignatureKeyID)
	case m.Channel == ChannelRelease:
		b.fail("signature", "the release channel needs a trusted signature")
	default:
		b.warn("signature", "unsigned dev release")
	}
	if m.Channel == ChannelDev {
		if req.AllowDev {
			b.warn("channel", "dev channel release admitted with --allow-dev: not supply-chain verified")
		} else {
			b.fail("channel", "release %s is on the dev channel; pass --allow-dev to plan it", m.ReleaseID)
		}
	}
	for _, w := range rel.Warnings {
		if m.Channel == ChannelDev && (strings.HasPrefix(w, "unsigned") || strings.HasPrefix(w, "dev channel")) {
			continue
		}
		b.warn("evidence", "%s", w)
	}

	if v, err := e.Docker.ComposeVersion(ctx); err != nil {
		b.fail("compose", "%v", err)
	} else if !versionAtLeast(v, minComposeMajor, minComposeMinor) {
		b.fail("compose", "docker compose %s is older than %d.%d (needed for override tags)", v, minComposeMajor, minComposeMinor)
	} else {
		b.ok("compose", "docker compose %s", v)
	}

	imgs, haveArch := m.Images[arch]
	if !haveArch {
		b.fail("arch", "the release has no images for this engine's architecture (%s); it ships %s", arch, strings.Join(sortedArches(m.Images), ", "))
	} else {
		b.ok("arch", "release ships linux/%s images", arch)
	}
	gw := e.Dep.Gateway
	if haveArch {
		switch {
		case gw != nil && imgs.Gateway == nil:
			b.fail("gateway-image", "the deployment runs a Gateway but the release has no Gateway image")
		case gw == nil && imgs.Gateway != nil:
			b.warn("gateway-image", "the release ships a Gateway image but the deployment has no Gateway: it is ignored")
		}
		p.Target[runtimeComponent(req.Workspace)] = PlanImage{Ref: imgs.Runtime.Ref(), Digest: imgs.Runtime.Digest}
		if gw != nil && imgs.Gateway != nil {
			p.Target[gatewayComponent] = PlanImage{Ref: imgs.Gateway.Ref(), Digest: imgs.Gateway.Digest}
		}
	}

	if j := pendingJournal(e.Dep, e.Now); j != nil {
		b.fail("pending-operation", "%s %s is %s: finish it with `lensdock update status`, approve or abort first", j.Kind, j.ID, j.Status)
	}

	st, tracked, model, err := e.resolveState(ctx)
	if err != nil {
		return nil, err
	}
	p.Current = PlanCurrent{Generation: st.Generation, Tracked: tracked, ReleaseID: st.ReleaseID, Components: st.Clone().Components}

	cur := e.currentRelease(st)
	var curComponents *Components
	var curManifest *Manifest
	if cur != nil {
		curManifest = &cur.Manifest
		curComponents = &cur.Manifest.Components
	}
	if st.ReleaseID != "" && cur == nil {
		b.fail("current-release", "running release %s has no valid manifest under releasesDir; schema and Chromium compatibility cannot be proved", st.ReleaseID)
	}
	p.Diff = diffComponents(curComponents, m.Components)
	p.Risk = classifyRisk(curManifest, &m)

	sameImages := haveArch && st.Components[runtimeComponent(req.Workspace)].ImageID == imgs.Runtime.Digest
	if sameImages && gw != nil && imgs.Gateway != nil {
		sameImages = st.Components[gatewayComponent].ImageID == imgs.Gateway.Digest
	}
	if st.ReleaseID == m.ReleaseID || sameImages {
		b.fail("already-running", "the workspace already runs release %s", m.ReleaseID)
	}

	e.checkCompat(b, cur, &m, req)
	e.checkCLIArtifact(b, arch, &m)
	if haveArch {
		e.checkImages(ctx, b, arch, imgs, gw != nil)
	}
	e.checkRetention(ctx, b, arch, st)
	e.checkCompose(b, model, req.Workspace, st)
	e.checkCapacity(ctx, b, p, st, req.Workspace)
	e.checkBackupTarget(b)
	e.checkRuntime(ctx, b, p, st, tracked, req.Workspace, ws)

	if other := otherWorkspaces(e.Dep, req.Workspace); len(other) > 0 {
		b.warn("other-workspaces", "workspaces %s keep their current runtime image and run next to the new Gateway", strings.Join(other, ", "))
	}

	p.Impact = e.impact(req.Workspace, p)
	p.Steps = planSteps(gw != nil)
	sort.Strings(p.Warnings)
	p.Digest = planDigest(p)
	return p, nil
}

func otherWorkspaces(d *Deployment, ws string) []string {
	var out []string
	for _, id := range d.WorkspaceIDs() {
		if id != ws {
			out = append(out, id)
		}
	}
	return out
}

func (e *Engine) checkCompat(b planBuilder, cur *Release, m *Manifest, req PlanRequest) {
	if cur == nil {
		b.warn("compat", "the running release has no manifest in %s: Chromium and schema downgrade checks are skipped", filepath.Base(e.Dep.ReleasesDir))
		return
	}
	cm := &cur.Manifest
	if a, c := cm.ChromiumMajor(), m.ChromiumMajor(); a > 0 && c > 0 && c < a {
		if req.AllowDowngrade {
			b.warn("chromium", "Chromium major %d to %d is a downgrade (admitted with --allow-downgrade)", a, c)
		} else {
			b.fail("chromium", "Chromium major %d to %d is a downgrade: an older Chromium must not open a Profile a newer one wrote", a, c)
		}
	} else {
		b.ok("chromium", "Chromium %s to %s", cm.Components.Chromium, m.Components.Chromium)
	}
	if !m.HasGateway() || !cm.HasGateway() {
		return
	}
	switch {
	case cm.Schema.GatewayDB < m.Schema.MinGatewayDB:
		b.fail("schema", "the running database schema %d is older than the %d this release can migrate from", cm.Schema.GatewayDB, m.Schema.MinGatewayDB)
	case cm.Schema.GatewayDB > m.Schema.GatewayDB:
		if req.AllowDowngrade {
			b.warn("schema", "Gateway database schema %d to %d is a downgrade (admitted with --allow-downgrade)", cm.Schema.GatewayDB, m.Schema.GatewayDB)
		} else {
			b.fail("schema", "Gateway database schema %d to %d: an older Gateway must not read a newer schema", cm.Schema.GatewayDB, m.Schema.GatewayDB)
		}
	default:
		b.ok("schema", "Gateway database schema %d to %d (migrates from %d or newer)", cm.Schema.GatewayDB, m.Schema.GatewayDB, m.Schema.MinGatewayDB)
	}
}

func (e *Engine) checkCLIArtifact(b planBuilder, arch string, m *Manifest) {
	path, sum, err := e.linuxCLI(arch)
	if err != nil {
		b.fail("verifier", "%v", err)
		return
	}
	want, ok := m.CLI[arch]
	switch {
	case !ok:
		b.fail("verifier", "the manifest pins no Linux lensdock binary for %s", arch)
	case want.SHA256 != sum:
		b.fail("verifier", "%s does not match the manifest (sha256 %s, manifest %s): rebuild it from the release commit", filepath.Base(filepath.Dir(path))+"/"+filepath.Base(path), sum[:12], want.SHA256[:12])
	default:
		b.ok("verifier", "linux lensdock %s matches the manifest", sum[:12])
	}
}

func (e *Engine) checkImages(ctx context.Context, b planBuilder, arch string, imgs ArchImages, gateway bool) {
	check := func(name string, r ImageRef) {
		info, err := e.Docker.ImageInspect(ctx, r.Ref())
		switch {
		case errors.Is(err, ErrNotFound):
			b.fail("image-"+name, "%s is not present locally; pull this exact digest before planning so Docker disk capacity and identity can be checked: docker pull %s", r.Ref(), r.Ref())
		case err != nil:
			b.fail("image-"+name, "%v", err)
		case !info.HasDigest(r.Digest):
			b.fail("image-"+name, "%s resolved to another digest (%s)", r.Ref(), info.ID)
		case info.Arch != arch || info.OS != "linux":
			b.fail("image-"+name, "%s is %s/%s, the engine needs linux/%s", r.Ref(), info.OS, info.Arch, arch)
		default:
			b.ok("image-"+name, "%s present with the exact digest", r.Ref())
		}
	}
	check("runtime", imgs.Runtime)
	if gateway && imgs.Gateway != nil {
		check("gateway", *imgs.Gateway)
	}
}

// checkRetention proves, before anything can be stopped, that every image the
// running services use can be kept under its own repository: the restore after
// a failed update and the rollback both start these exact digests again.
// Nothing is changed here; a previous image whose repository@digest no longer
// resolves but has a verified local copy is a warning, apply re-tags it.
func (e *Engine) checkRetention(ctx context.Context, b planBuilder, arch string, st State) {
	for _, t := range componentTargets(st.Components, true) {
		id := "retain-runtime"
		if t.Name == gatewayComponent {
			id = "retain-gateway"
		}
		p, err := e.planRetention(ctx, t, arch)
		switch {
		case err != nil:
			b.fail(id, "%s: %v", t.Name, err)
		case p.recovers():
			b.warn(id, "%s: %s no longer resolves locally; apply re-tags the local copy %s, whose exact digest was verified, as %s", t.Name, t.Ref, p.From, p.Keep)
		default:
			b.ok(id, "%s: %s is present with the exact digest; apply keeps it as %s", t.Name, t.Ref, p.Keep)
		}
	}
}

// checkCompose validates declared datasets and the read-only mounts inherited
// by the candidate Runtime. Gateway migration uses explicit mounts instead,
// with candidateAccess enforced by migrateGateway independently of live access.
func (e *Engine) checkCompose(b planBuilder, model ComposeModel, workspace string, st State) {
	owners := map[string][]Dataset{runtimeComponent(workspace): e.Dep.Workspaces[workspace].Datasets}
	if e.Dep.Gateway != nil {
		owners[gatewayComponent] = e.Dep.Gateway.Datasets
	}
	for _, owner := range sortedKeys(owners) {
		comp := st.Components[owner]
		svc, ok := model.Services[comp.Service]
		if !ok {
			b.fail("compose", "service %q is not defined in the compose files", comp.Service)
			continue
		}
		for _, ds := range owners[owner] {
			if _, ok := model.Volumes[ds.Key]; !ok {
				b.fail("compose", "volume key %q is not declared in the compose files", ds.Key)
				continue
			}
			mounts := 0
			for _, mt := range svc.Volumes {
				if mt.Type != "volume" || mt.Source != ds.Key {
					continue
				}
				mounts++
				if owner != gatewayComponent && ds.Role == RoleRetained && ds.CandidateAccess == CandidateReadOnly && !mt.ReadOnly {
					b.fail("compose", "dataset %s is shared read-only with verification containers but service %s mounts it writable at %s", ds.Key, comp.Service, mt.Target)
				}
			}
			if mounts == 0 {
				b.warn("compose", "service %s does not mount dataset %s (volume key %s)", comp.Service, owner, ds.Key)
			}
		}
		known := map[string]bool{}
		for _, ds := range owners[owner] {
			known[ds.Key] = true
		}
		for _, mt := range svc.Volumes {
			switch mt.Type {
			case "volume":
				if !known[mt.Source] {
					b.fail("compose", "service %s mounts volume %q which deployment.json does not describe: a candidate could not isolate it", comp.Service, mt.Source)
				}
			case "bind":
				if filepath.Clean(mt.Source) == "/var/run/docker.sock" || filepath.Clean(mt.Target) == "/var/run/docker.sock" {
					b.fail("compose", "service %s must not mount the Docker socket", comp.Service)
				} else {
					b.warn("compose", "service %s bind-mounts %s: candidate verification shares that host path", comp.Service, mt.Source)
				}
			}
		}
	}
	b.ok("compose", "services and volume keys resolved from the compose files")
}

func (e *Engine) checkCapacity(ctx context.Context, b planBuilder, p *Plan, st State, workspace string) {
	sets, err := e.datasetsFor(workspace, st)
	if err != nil {
		b.fail("datasets", "%v", err)
		return
	}
	comp := st.Components[runtimeComponent(workspace)]
	helper := VolumeHelper{D: e.Docker, Image: comp.Image}
	var total int64
	var firstSnapshot string
	for _, ds := range sets {
		pd := PlanDataset{Owner: ds.Owner, Key: ds.Key, Kind: ds.Kind, Role: ds.Role, Volume: ds.Volume}
		if _, err := e.Docker.VolumeInspect(ctx, ds.Volume); err != nil {
			b.fail("volume", "volume %s of dataset %s: %v", ds.Volume, ds.Key, err)
			p.Datasets = append(p.Datasets, pd)
			continue
		}
		if ds.Role == RoleSnapshot {
			n, err := helper.UsedBytes(ctx, ds.Volume)
			if err != nil {
				b.fail("capacity", "measure %s: %v", ds.Key, err)
			}
			pd.UsedBytes = n
			total += n
			if firstSnapshot == "" {
				firstSnapshot = ds.Volume
			}
		}
		p.Datasets = append(p.Datasets, pd)
	}
	p.Capacity.SnapshotBytes = total
	need := total + e.Dep.MinFreeBytes
	p.Capacity.BackupRequiredBytes = uint64(need)
	p.Capacity.DockerRequiredBytes = need
	if free, err := freeBytes(e.Dep.BackupDir); err != nil {
		b.fail("capacity", "backup location: %v", err)
	} else {
		p.Capacity.BackupFreeBytes = free
		if free < uint64(need) {
			b.fail("capacity", "backup location has %s free, the checkpoint needs %s (snapshot data %s plus %s headroom)", humanBytes(int64(free)), humanBytes(need), humanBytes(total), humanBytes(e.Dep.MinFreeBytes))
		} else {
			b.ok("capacity", "backup location has %s free, needs %s", humanBytes(int64(free)), humanBytes(need))
		}
	}
	if firstSnapshot != "" {
		if free, err := helper.FreeBytes(ctx, firstSnapshot); err != nil {
			b.fail("capacity", "docker data root: %v", err)
		} else {
			p.Capacity.DockerFreeBytes = free
			if free < need {
				b.fail("capacity", "docker data root has %s free, the candidate volumes need %s", humanBytes(free), humanBytes(need))
			} else {
				b.ok("capacity", "docker data root has %s free for the candidate volumes", humanBytes(free))
			}
		}
	}
}

func (e *Engine) checkBackupTarget(b planBuilder) {
	if err := syscall.Access(nearestExisting(e.Dep.BackupDir), 2); err != nil {
		b.fail("backup-dir", "backup location is not writable: %v", err)
	} else {
		b.ok("backup-dir", "backup location is writable")
	}
	key, err := LoadBackupKey(e.Dep.BackupKeyFile)
	if err != nil {
		b.fail("backup-key", "%v (create one with `lensdock update init`)", err)
		return
	}
	b.ok("backup-key", "backup key %s is readable and kept outside the backup location", KeyID(key))
}

func (e *Engine) checkRuntime(ctx context.Context, b planBuilder, p *Plan, st State, tracked bool, workspace string, ws WorkspaceConfig) {
	resp, err := e.runtimeProbe(ctx, workspace, st, tracked, ProbeRequest{Op: ProbeStatus})
	switch {
	case errors.Is(err, ErrNoOperatorToken):
		p.Runtime.Note = "no operator token configured: lease state is not inspected; stopping the runtime revokes every lease"
		b.warn("runtime", "%s", p.Runtime.Note)
		return
	case err != nil:
		p.Runtime.Note = "runtime not inspected: " + err.Error()
		b.warn("runtime", "%s", p.Runtime.Note)
		return
	}
	if perr := probeErr(resp); perr != nil || resp.Status == nil {
		p.Runtime.Note = "runtime not inspected: " + fmt.Sprint(perr)
		b.warn("runtime", "%s", p.Runtime.Note)
		return
	}
	s := resp.Status
	p.Runtime = PlanRuntime{Inspected: true, Ready: s.Ready, ControlState: s.ControlState, LeaseHeld: s.LeaseHeld(), HolderID: s.HolderID}
	if s.LeaseHeld() {
		b.warn("lease", "a %s lease held by %s is active; apply waits for it to end (%ds) or needs --revoke-leases", s.LeaseMode, s.HolderID, e.Dep.DrainTimeoutSeconds)
	} else {
		b.ok("lease", "no active lease (control state %s)", s.ControlState)
	}
	if !s.Ready {
		b.warn("runtime", "the runtime is not ready (%s)", s.LockCode)
	}
}

func (e *Engine) impact(workspace string, p *Plan) []string {
	out := []string{
		fmt.Sprintf("Workspace %s stops: no lease can be acquired and the browser closes cleanly (up to %ds). Downtime lasts until the new generation is healthy, including snapshot and candidate verification.", workspace, e.Dep.StopGraceSeconds),
	}
	if e.Dep.Gateway != nil {
		out = append(out, "The Gateway stops first, so the UI and agents cannot take new leases; its database is snapshotted after a clean stop.")
	}
	out = append(out,
		"Open tabs are restored by Chromium session restore only if the new Chromium does so; no lease survives and agents must acquire new leases. The last destructive agent action is never resumed.",
		fmt.Sprintf("A checkpoint of %s (encrypted with key from %s) is written before anything is switched. Rolling back restores the old images with exactly that snapshot: changes made after it are not restored, they stay in the retired branch volumes.", humanBytes(p.Capacity.SnapshotBytes), filepath.Base(e.Dep.BackupKeyFile)),
		"Not affected by rollback: downloads, feedback artifacts and private drafts (retained volumes are reattached, never copied).",
		"The candidate runs on cloned volumes without any outside network; real-account testing is not part of this procedure.",
		"Host kernel and Docker Engine are not changed by this update (they are updated by the host administrator).",
	)
	return out
}

func planSteps(gateway bool) []string {
	steps := []string{
		"01 verify the release manifest, signature and exact image digests again, and keep the images the running services use under their own repositories",
		"02 re-check volumes and free space for the checkpoint and the candidate volumes",
	}
	if gateway {
		steps = append(steps, "03 stop the Gateway (no new leases) and fence the runtime (MAINTENANCE)")
	} else {
		steps = append(steps, "03 fence the runtime (MAINTENANCE)")
	}
	return append(steps,
		"04 wait for the active lease to end, or revoke it with --revoke-leases",
		"05 stop the runtime cleanly; check exit status, Profile lock files and exit_type",
		"06 write the encrypted, verified checkpoint of Profile, keyring and Gateway database",
		"07 create candidate volumes from the checkpoint and compare their fingerprints",
		"08 start the candidate (sandbox when enabled, fonts, packages, CDP, local fixture, Gateway migration on the clone)",
		"09 prove the candidate has no route to the outside",
		"10 after your approval: switch to the new images and candidate volumes (new generation)",
		"11 check health; leases must be acquired again",
	)
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// planDigest hashes the plan with an empty Digest field.
func planDigest(p *Plan) string {
	cp := *p
	cp.Digest = ""
	data, _ := json.Marshal(cp)
	return sha256Hex(data)
}

// ErrPlanExpired is returned when a plan is older than its validity.
var ErrPlanExpired = errors.New("the plan has expired: make a new one with `lensdock update plan`")

// SavePlan writes the plan to path (default plans/<id>.json) with mode 0600.
// Plans with blockers are refused.
func (e *Engine) SavePlan(p *Plan, path string) (string, error) {
	if len(p.Blockers) > 0 {
		return "", fmt.Errorf("the plan has %d blockers and is not applicable", len(p.Blockers))
	}
	if path == "" {
		if err := os.MkdirAll(e.Dep.plansDir(), 0o700); err != nil {
			return "", fmt.Errorf("create plans directory: %w", err)
		}
		path = filepath.Join(e.Dep.plansDir(), p.ID+".json")
	}
	if _, err := os.Lstat(path); err == nil {
		return "", fmt.Errorf("%s already exists", path)
	}
	return path, writeJSONAtomic(path, p, 0o600)
}

// LoadPlan reads a plan file and checks its integrity digest, version and
// validity.
func LoadPlan(path string, now time.Time) (*Plan, error) {
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
	if p.SchemaVersion != planSchemaVersion || !planIDPattern.MatchString(p.ID) {
		return nil, errors.New("not a lensdock update plan")
	}
	if planDigest(&p) != p.Digest {
		return nil, errors.New("the plan was modified after it was created (digest mismatch)")
	}
	if len(p.Blockers) > 0 {
		return nil, errors.New("the plan has blockers")
	}
	if now.After(p.ExpiresAt) {
		return nil, ErrPlanExpired
	}
	return &p, nil
}
