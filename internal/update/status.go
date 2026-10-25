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
	"time"
)

// PublicStatusSchemaVersion is the deployment-status.json schema version.
const PublicStatusSchemaVersion = 1

// PublicStatus is the sanitized, read-only status written by the host CLI and
// served by the Gateway. It contains no credentials, host paths or volume
// names.
type PublicStatus struct {
	SchemaVersion int           `json:"schemaVersion,omitempty"`
	GeneratedAt   time.Time     `json:"generatedAt,omitempty"`
	Release       PublicRelease `json:"release"`
	Update        PublicUpdate  `json:"update"`
}

type PublicRelease struct {
	ID             string                 `json:"id"`
	Channel        *string                `json:"channel,omitempty"`
	Verified       bool                   `json:"verified"`
	VerifiedAt     *time.Time             `json:"verifiedAt,omitempty"`
	Signed         bool                   `json:"signed,omitempty"`
	SignatureKeyID *string                `json:"signatureKeyId,omitempty"`
	CreatedAt      *time.Time             `json:"createdAt,omitempty"`
	GitCommit      *string                `json:"gitCommit,omitempty"`
	ManifestSHA256 *string                `json:"manifestSha256,omitempty"`
	Versions       *Components            `json:"versions,omitempty"`
	Images         map[string]PublicImage `json:"images,omitempty"`
	Schema         *SchemaInfo            `json:"schema,omitempty"`
	Evidence       *PublicEvidence        `json:"evidence,omitempty"`
	Generation     int                    `json:"generation"`
}

type PublicImage struct {
	Repository string `json:"repository"`
	Digest     string `json:"digest"`
}

type PublicEvidence struct {
	SBOM       bool        `json:"sbom"`
	Provenance bool        `json:"provenance"`
	Scan       *PublicScan `json:"scan,omitempty"`
	Tests      bool        `json:"tests"`
}

// PublicScan sums the scanned release images. Partial is true when at least
// one release image has no scan, so the counts do not cover the whole release.
type PublicScan struct {
	Tool     string `json:"tool"`
	Critical int    `json:"critical"`
	High     int    `json:"high"`
	Medium   int    `json:"medium"`
	Low      int    `json:"low"`
	Partial  bool   `json:"partial,omitempty"`
}

type PublicUpdate struct {
	Phase          string            `json:"phase"`
	LastBackupAt   *time.Time        `json:"lastBackupAt"`
	LastVerifiedAt *time.Time        `json:"lastVerifiedAt"`
	Candidates     []PublicCandidate `json:"candidates,omitempty"`
	PendingPlan    *PublicPending    `json:"pendingPlan,omitempty"`
	Storage        PublicStorage     `json:"storage"`
	Backup         PublicBackup      `json:"backup"`
	Rollback       PublicRollback    `json:"rollback"`
	LastOperation  *PublicOperation  `json:"lastOperation,omitempty"`
}

// PublicStorage is current named-volume usage aggregated by data kind. It
// exposes byte counts only, never volume names or mount paths.
type PublicStorage struct {
	ProfileBytes   *int64 `json:"profileBytes,omitempty"`
	KeyringBytes   *int64 `json:"keyringBytes,omitempty"`
	DownloadsBytes *int64 `json:"downloadsBytes,omitempty"`
	GatewayDBBytes *int64 `json:"gatewayDbBytes,omitempty"`
	ArtifactsBytes *int64 `json:"artifactsBytes,omitempty"`
	PrivateBytes   *int64 `json:"privateBytes,omitempty"`
	OtherBytes     *int64 `json:"otherBytes,omitempty"`
	TotalBytes     *int64 `json:"totalBytes,omitempty"`
}

type PublicCandidate struct {
	ID           string             `json:"id"`
	Channel      string             `json:"channel"`
	CreatedAt    time.Time          `json:"createdAt"`
	GitCommit    string             `json:"gitCommit"`
	Versions     Components         `json:"versions"`
	Risk         Risk               `json:"risk"`
	Diff         []VersionDiff      `json:"diff,omitempty"`
	Issues       []string           `json:"issues,omitempty"`
	HostCommands PublicHostCommands `json:"hostCommands"`
}

type PublicHostCommands struct {
	Plan  string `json:"plan"`
	Apply string `json:"apply,omitempty"`
}

type PublicPending struct {
	ID        string    `json:"id"`
	Release   string    `json:"release"`
	Workspace string    `json:"workspace"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
	Blockers  []string  `json:"blockers,omitempty"`
	Warnings  []string  `json:"warnings,omitempty"`
}

type PublicBackup struct {
	Count          int               `json:"count"`
	Retain         int               `json:"retain"`
	TotalBytes     int64             `json:"totalBytes"`
	FreeBytes      *uint64           `json:"freeBytes"`
	LastCheckpoint *PublicCheckpoint `json:"lastCheckpoint,omitempty"`
}

type PublicCheckpoint struct {
	ID          string                 `json:"id"`
	CreatedAt   time.Time              `json:"createdAt"`
	Consistency string                 `json:"consistency"`
	ReleaseID   string                 `json:"releaseId,omitempty"`
	Datasets    []PublicCheckpointData `json:"datasets"`
}

type PublicCheckpointData struct {
	Role  string `json:"role"`
	Bytes int64  `json:"bytes"`
}

type PublicRollback struct {
	Available           bool       `json:"available"`
	CheckpointID        *string    `json:"checkpointId,omitempty"`
	CheckpointCreatedAt *time.Time `json:"checkpointCreatedAt,omitempty"`
	TargetRelease       *string    `json:"targetRelease,omitempty"`
	Reason              *string    `json:"reason,omitempty"`
	DataLossNote        string     `json:"dataLossNote"`
	Unaffected          []string   `json:"unaffected"`
	HostCommand         string     `json:"hostCommand,omitempty"`
}

type PublicOperation struct {
	Kind       string     `json:"kind"`
	ID         string     `json:"id"`
	Status     string     `json:"status"`
	StartedAt  time.Time  `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	Release    string     `json:"release,omitempty"`
	Error      *string    `json:"error,omitempty"`
}

// ReadPublicStatus strictly reads a PublicStatus file. The Gateway may use
// this projection to serve it without knowing the update internals.
func ReadPublicStatus(path string) (PublicStatus, error) {
	data, err := readFileLimit(path, 4<<20)
	if err != nil {
		return PublicStatus{}, err
	}
	var s PublicStatus
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return PublicStatus{}, err
	}
	if err := ensureJSONEOF(dec); err != nil {
		return PublicStatus{}, err
	}
	if s.SchemaVersion != 0 && s.SchemaVersion != PublicStatusSchemaVersion {
		return PublicStatus{}, fmt.Errorf("unsupported public status schema version %d", s.SchemaVersion)
	}
	if s.Release.ID == "" {
		return PublicStatus{}, errors.New("public status has no release id")
	}
	return s, nil
}

// Status builds the current sanitized status without writing it.
func (e *Engine) Status(ctx context.Context) (PublicStatus, error) {
	out := PublicStatus{
		SchemaVersion: PublicStatusSchemaVersion,
		GeneratedAt:   e.Now().UTC(),
		Release:       PublicRelease{ID: "development"},
		Update:        PublicUpdate{Phase: "idle"},
	}
	if err := e.Dep.ensureDirs(); err != nil {
		return out, err
	}
	st, tracked, _, err := e.resolveState(ctx)
	if err != nil {
		out.Update.Phase = "not-configured"
		return out, err
	}
	out.Release.Generation = st.Generation
	if st.ReleaseID != "" {
		out.Release.ID = st.ReleaseID
	}
	if tracked {
		if rel := e.currentRelease(st); rel != nil {
			out.Release = publicRelease(rel, st.Generation, e.archOrHost(ctx))
		}
	}
	if ids := e.Dep.WorkspaceIDs(); len(ids) > 0 {
		if comp, ok := st.Components[runtimeComponent(ids[0])]; ok {
			helper := VolumeHelper{D: e.Docker, Image: comp.Image}
			add := func(dst **int64, n int64) {
				if *dst == nil {
					*dst = new(n)
				} else {
					**dst += n
				}
			}
			for owner, sets := range e.Dep.allDatasets() {
				c, ok := st.Components[owner]
				if !ok {
					continue
				}
				for _, ds := range sets {
					n, err := helper.UsedBytes(ctx, c.Volumes[ds.Key])
					if err != nil {
						continue
					}
					add(&out.Update.Storage.TotalBytes, n)
					switch ds.Kind {
					case KindProfile:
						add(&out.Update.Storage.ProfileBytes, n)
					case KindKeyring:
						add(&out.Update.Storage.KeyringBytes, n)
					case KindDownloads:
						add(&out.Update.Storage.DownloadsBytes, n)
					case KindDB:
						add(&out.Update.Storage.GatewayDBBytes, n)
					case KindArtifacts:
						add(&out.Update.Storage.ArtifactsBytes, n)
					case KindPrivate:
						add(&out.Update.Storage.PrivateBytes, n)
					default:
						add(&out.Update.Storage.OtherBytes, n)
					}
				}
			}
		}
	}
	store := CheckpointStore{Dir: e.Dep.BackupDir}
	cps, _, cpErr := store.List()
	if cpErr == nil {
		out.Update.Backup.Count = len(cps)
		out.Update.Backup.Retain = e.Dep.RetainCheckpoints
		if _, n, err := store.Usage(); err == nil {
			out.Update.Backup.TotalBytes = n
		}
		if free, err := freeBytes(e.Dep.BackupDir); err == nil {
			out.Update.Backup.FreeBytes = new(free)
		}
		for _, cp := range cps {
			if cp.Consistency != ConsistencyClean {
				continue
			}
			out.Update.LastBackupAt = new(cp.CreatedAt)
			out.Update.Backup.LastCheckpoint = publicCheckpoint(cp)
			break
		}
	}
	reason := "the running generation has no paired clean pre-update checkpoint"
	out.Update.Rollback = publicRollback(Checkpoint{}, false, reason)
	if st.LastUpdateID != "" {
		if j, err := loadJournal(e.Dep, st.LastUpdateID, e.Now); err == nil && j.Kind == "update" && j.CheckpointID != "" && j.Intent != nil && j.Intent.Generation == st.Generation {
			if cp, err := store.Load(j.CheckpointID); err == nil && cp.Consistency == ConsistencyClean {
				out.Update.Rollback = publicRollback(cp, true, "")
			}
		}
	}
	js, _ := listJournals(e.Dep, e.Now)
	if len(js) > 0 {
		j := js[len(js)-1]
		out.Update.LastOperation = publicOperation(j)
		out.Update.Phase = publicPhase(j)
		for i := len(js) - 1; i >= 0; i-- {
			if js[i].VerifiedAt != nil {
				out.Update.LastVerifiedAt = js[i].VerifiedAt
				break
			}
		}
	}
	out.Update.PendingPlan = publicPendingPlan(e.Dep, st, tracked, js, e.Now())
	return out, cpErr
}

func (e *Engine) archOrHost(ctx context.Context) string {
	if a, err := e.engineArch(ctx); err == nil {
		return a
	}
	return ""
}

func publicRelease(r *Release, generation int, arch string) PublicRelease {
	m := &r.Manifest
	out := PublicRelease{
		ID: m.ReleaseID, Channel: new(m.Channel), Verified: true, VerifiedAt: new(r.VerifiedAt), Signed: r.Signed,
		CreatedAt: new(m.CreatedAt), GitCommit: new(m.Source.GitCommit), ManifestSHA256: new(r.SHA256),
		Versions: &m.Components, Schema: &m.Schema, Generation: generation,
	}
	if r.Signed {
		out.SignatureKeyID = new(r.SignatureKeyID)
	}
	if imgs, ok := m.Images[arch]; ok {
		out.Images = map[string]PublicImage{"runtime": {Repository: imgs.Runtime.Repository, Digest: imgs.Runtime.Digest}}
		if imgs.Gateway != nil {
			out.Images["gateway"] = PublicImage{Repository: imgs.Gateway.Repository, Digest: imgs.Gateway.Digest}
		}
		refs := []ImageRef{imgs.Runtime}
		if imgs.Gateway != nil {
			refs = append(refs, *imgs.Gateway)
		}
		// An aggregate claim holds only when every release image carries the
		// evidence; one image with an SBOM must not vouch for the other. A
		// scan summary adds up the scanned images and is marked Partial when
		// any image has no scan.
		ev := PublicEvidence{Tests: m.Tests != nil && m.Tests.Passed, SBOM: true, Provenance: true}
		var scan *PublicScan
		tools := map[string]bool{}
		for _, ref := range refs {
			ev.SBOM = ev.SBOM && ref.SBOM != nil
			ev.Provenance = ev.Provenance && ref.Provenance != nil
			if ref.Scan == nil {
				continue
			}
			if scan == nil {
				scan = &PublicScan{}
			}
			tools[ref.Scan.Tool] = true
			scan.Critical += ref.Scan.Summary.Critical
			scan.High += ref.Scan.Summary.High
			scan.Medium += ref.Scan.Summary.Medium
			scan.Low += ref.Scan.Summary.Low
		}
		if scan != nil {
			names := make([]string, 0, len(tools))
			for t := range tools {
				names = append(names, t)
			}
			sort.Strings(names)
			scan.Tool = strings.Join(names, ", ")
			for _, ref := range refs {
				if ref.Scan == nil {
					scan.Partial = true
				}
			}
		}
		ev.Scan = scan
		out.Evidence = &ev
	}
	return out
}

func publicCheckpoint(cp Checkpoint) *PublicCheckpoint {
	pc := &PublicCheckpoint{ID: cp.ID, CreatedAt: cp.CreatedAt, Consistency: cp.Consistency, ReleaseID: cp.Previous.ReleaseID}
	for _, ds := range cp.Datasets {
		pc.Datasets = append(pc.Datasets, PublicCheckpointData{Role: ds.Kind, Bytes: ds.PlainBytes})
	}
	return pc
}

func publicRollback(cp Checkpoint, available bool, reason string) PublicRollback {
	out := PublicRollback{
		Available:    available,
		DataLossNote: "state written after the checkpoint is not restored; it is retained as a branch",
		Unaffected:   []string{"downloads", "gateway artifacts", "gateway private"},
	}
	if available {
		out.CheckpointID, out.TargetRelease, out.CheckpointCreatedAt = new(cp.ID), new(cp.Previous.ReleaseID), new(cp.CreatedAt)
		out.HostCommand = "lensdock update rollback --checkpoint " + cp.ID
	} else {
		out.Reason = new(reason)
	}
	return out
}

func publicOperation(j *Journal) *PublicOperation {
	out := &PublicOperation{Kind: j.Kind, ID: j.ID, Status: j.Status, StartedAt: j.StartedAt, FinishedAt: j.FinishedAt, Release: j.Release}
	if j.Error != "" {
		out.Error = new(sanitizeError(j.Error))
	}
	return out
}

func publicPhase(j *Journal) string {
	switch j.Status {
	case JournalAwaitingApproval:
		return "awaiting-approval"
	case JournalRunning:
		if j.Kind == "rollback" {
			return "rolling-back"
		}
		return "applying"
	case JournalHeld:
		return "failed-held"
	case JournalReverted, JournalAborted:
		return "failed-reverted"
	case JournalCompleted:
		if j.Kind == "rollback" {
			return "rolled-back"
		}
		return "completed"
	default:
		return "idle"
	}
}

func sanitizeError(_ string) string {
	// Public status is mounted into the Gateway and must not expose a host
	// path, volume name, credential location, command stderr or data detail.
	// Exact errors remain in the 0600 host journal.
	return "operation failed; inspect the host update journal with the LensDock CLI"
}

// Intentionally no pointer helper: Go 1.27's new(expr) is used at call sites.

// publicPlanCheckIDs are the plan check identifiers the public status may
// name. They are literals chosen by the planner, never operator data; a
// plan file carrying any other identifier is summarized as "other".
var publicPlanCheckIDs = map[string]bool{
	"manifest": true, "signature": true, "channel": true, "evidence": true, "compose": true,
	"arch": true, "gateway-image": true, "pending-operation": true, "current-release": true,
	"already-running": true, "other-workspaces": true, "compat": true, "chromium": true,
	"schema": true, "verifier": true, "image-runtime": true, "image-gateway": true,
	"retain-runtime": true, "retain-gateway": true,
	"datasets": true, "volume": true, "capacity": true, "backup-dir": true, "backup-key": true,
	"runtime": true, "lease": true,
}

// publicPlanIssues summarizes the plan checks with the given status as one
// line per check category: the category and a count, never the check detail.
// Details name host paths, volumes and Docker output; they stay in the 0600
// host plan and the CLI plan output.
func publicPlanIssues(p *Plan, status string) []string {
	counts := map[string]int{}
	for _, c := range p.Checks {
		if c.Status != status {
			continue
		}
		id := c.ID
		if !publicPlanCheckIDs[id] {
			id = "other"
		}
		counts[id]++
	}
	ids := make([]string, 0, len(counts))
	for id := range counts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	noun := "warning"
	if status == PlanFail {
		noun = "blocker"
	}
	var out []string
	for _, id := range ids {
		n := counts[id]
		plural := ""
		if n != 1 {
			plural = "s"
		}
		out = append(out, fmt.Sprintf("%s: %d %s%s; exact detail is in the private host plan", id, n, noun, plural))
	}
	return out
}

// publicPendingPlan returns the newest saved plan an operator can still
// apply, or nil. A plan is not pending when it expired, when an update
// journal consumed it (running, awaiting approval, held or completed; an
// aborted or reverted update restored the old services and left the plan
// reusable), or when the deployment drifted from the state it was made
// against. Plans that are unreadable or fail their integrity digest are
// ignored.
func publicPendingPlan(d *Deployment, st State, tracked bool, js []*Journal, now time.Time) *PublicPending {
	entries, err := os.ReadDir(d.plansDir())
	if err != nil {
		return nil
	}
	consumed := map[string]bool{}
	for _, j := range js {
		if j.PlanID != "" && j.Status != JournalAborted && j.Status != JournalReverted {
			consumed[j.PlanID] = true
		}
	}
	var best *Plan
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		p, err := loadPlanFile(filepath.Join(d.plansDir(), e.Name()))
		if err != nil || now.After(p.ExpiresAt) || consumed[p.ID] || checkDrift(p, st, tracked) != nil {
			continue
		}
		// Published fields must be identifiers, never free text.
		if !planIDPattern.MatchString(p.ID) || !releaseIDPattern.MatchString(p.Release.ID) {
			continue
		}
		if _, err := d.Workspace(p.Workspace); err != nil {
			continue
		}
		if best == nil || p.CreatedAt.After(best.CreatedAt) {
			best = p
		}
	}
	if best == nil {
		return nil
	}
	return &PublicPending{
		ID: best.ID, Release: best.Release.ID, Workspace: best.Workspace, CreatedAt: best.CreatedAt, ExpiresAt: best.ExpiresAt,
		Blockers: publicPlanIssues(best, PlanFail), Warnings: publicPlanIssues(best, PlanWarn),
	}
}

// PublishStatus writes Status to publicStatusFile atomically with mode 0644.
// It creates the parent directory mode 0755. Empty publicStatusFile is a no-op.
func (e *Engine) PublishStatus(ctx context.Context) error {
	if e.Dep.PublicStatusFile == "" {
		return nil
	}
	s, err := e.Status(ctx)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(e.Dep.PublicStatusFile), 0o755); err != nil {
		return fmt.Errorf("create public status directory: %w", err)
	}
	return writeJSONAtomic(e.Dep.PublicStatusFile, s, 0o644)
}

func (e *Engine) publishQuiet(ctx context.Context) {
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := e.PublishStatus(pctx); err != nil {
		e.logf("warning: publish status: %v", err)
	}
}

// Check scans releasesDir and returns a public candidate list. Invalid
// releases are included with issues; incomplete releases are never silently
// omitted.
func (e *Engine) Check(ctx context.Context, workspace string) ([]PublicCandidate, error) {
	if _, err := e.Dep.Workspace(workspace); err != nil {
		return nil, err
	}
	st, _, _, err := e.resolveState(ctx)
	if err != nil {
		return nil, err
	}
	cur := e.currentRelease(st)
	var curM *Manifest
	var curC *Components
	if cur != nil {
		curM, curC = &cur.Manifest, &cur.Manifest.Components
	}
	entries, err := os.ReadDir(e.Dep.ReleasesDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []PublicCandidate
	for _, entry := range entries {
		if !entry.IsDir() || !releaseIDPattern.MatchString(entry.Name()) {
			continue
		}
		pc := PublicCandidate{ID: entry.Name(), HostCommands: PublicHostCommands{Plan: fmt.Sprintf("lensdock update plan --release %s --workspace %s", entry.Name(), workspace)}}
		rel, err := LoadRelease(filepath.Join(e.Dep.ReleasesDir, entry.Name()), VerifyPolicy{TrustedKeys: e.Dep.TrustedKeys, Now: e.Now})
		if err != nil {
			pc.Risk = Risk{Level: RiskBlocked, Reasons: []string{"the release is incomplete"}}
			pc.Issues = []string{sanitizeError(err.Error())}
			out = append(out, pc)
			continue
		}
		m := &rel.Manifest
		pc.Channel, pc.CreatedAt, pc.GitCommit, pc.Versions = m.Channel, m.CreatedAt, m.Source.GitCommit, m.Components
		pc.Diff, pc.Risk = diffComponents(curC, m.Components), classifyRisk(curM, m)
		out = append(out, pc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// Branch is a managed update volume that is not used by the current
// generation: a failed candidate, a retired generation or an orphaned scratch
// volume. Deleting one is always an explicit command.
type Branch struct {
	Volume    string `json:"volume"`
	Role      string `json:"role"`
	UpdateID  string `json:"updateId,omitempty"`
	Dataset   string `json:"dataset,omitempty"`
	CreatedAt string `json:"createdAt,omitempty"`
	InUse     bool   `json:"inUse"`
}

// Branches lists every managed volume plus every retired volume recorded in
// an operation journal, including an unlabelled baseline compose volume.
func (e *Engine) Branches(ctx context.Context) ([]Branch, error) {
	st, _, _, err := e.resolveState(ctx)
	if err != nil {
		return nil, err
	}
	used := map[string]bool{}
	for _, c := range st.Components {
		for _, v := range c.Volumes {
			used[v] = true
		}
	}
	byName := map[string]Branch{}
	vs, err := e.Docker.ManagedVolumes(ctx)
	if err != nil {
		return nil, err
	}
	for _, v := range vs {
		byName[v.Name] = Branch{
			Volume: v.Name, Role: v.Labels[LabelRole], UpdateID: v.Labels[LabelUpdate], Dataset: v.Labels[LabelDataset], CreatedAt: v.Labels[LabelCreated], InUse: used[v.Name],
		}
	}
	js, _ := listJournals(e.Dep, e.Now)
	for _, j := range js {
		for _, name := range j.RetiredVolumes {
			if b, ok := byName[name]; ok {
				if b.Role == "" || b.Role == RoleCandidate || b.Role == RoleRestored {
					b.Role = RoleRetired
				}
				b.InUse = used[name]
				byName[name] = b
				continue
			}
			byName[name] = Branch{Volume: name, Role: RoleRetired, UpdateID: j.ID, InUse: used[name]}
		}
	}
	out := make([]Branch, 0, len(byName))
	for _, b := range byName {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Volume < out[j].Volume })
	return out, nil
}
