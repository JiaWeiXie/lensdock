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
	"strconv"
	"strings"
	"time"
)

// Automatic expiry of unused old system data.
//
// What it may remove (all of it created by this deployment's update logic):
//
//   - checkpoints that no journal references any more,
//   - managed volumes of finished operations that no generation uses any more
//     (retired branches, failed candidates, orphaned scratch volumes),
//   - obsolete generation records below the running generation,
//   - abandoned staging: incomplete checkpoint directories and saved plans.
//
// What it never touches: the running generation and everything it uses, the
// checkpoint paired with the running generation, anything an unfinished
// operation (running, awaiting approval, held) references, retained datasets
// (downloads, feedback artifacts, private drafts), volumes it did not create,
// and images. Any metadata it cannot read or interpret keeps the item, and
// uncertain global inventory (state, journals) keeps everything.
//
// Policy precedence: ExpireAfterDays (default 30, 0 disables) and
// RetainCheckpoints (default 3, applied only by an explicit prune) are
// independent. Age expiry never reads the count; the count never reads age;
// protection beats both.

// Expiry scopes.
const (
	ScopeCheckpoints = "checkpoints"
	ScopeVolumes     = "volumes"
	ScopeGenerations = "generations"
	ScopeStaging     = "staging"
)

// ExpiryScopes lists every scope in report order.
var ExpiryScopes = []string{ScopeCheckpoints, ScopeVolumes, ScopeGenerations, ScopeStaging}

// Outcomes of one expiry item.
const (
	ExpiryExpired     = "expired"
	ExpiryWouldExpire = "would-expire"
	ExpiryKept        = "kept"
	ExpiryFailed      = "failed"
)

// Triggers recorded in a report.
const (
	TriggerManual    = "manual"
	TriggerAutomatic = "automatic"
)

const (
	expiryReportSchema = 1
	expiryLastName     = "expiry-last.json"
	// AutoExpireInterval is the shortest time between two automatic runs of a
	// deployment, so frequent lifecycle activity does not rescan everything.
	AutoExpireInterval = 24 * time.Hour
)

// ExpiryItem is the decision about one resource. Young items that need no
// attention are not listed.
type ExpiryItem struct {
	Scope   string `json:"scope"`
	ID      string `json:"id"`
	Outcome string `json:"outcome"`
	Reason  string `json:"reason,omitempty"`
	// Since is when the item's age starts (checkpoint creation, retirement of
	// a volume, supersession of a generation, staging creation).
	Since *time.Time `json:"since,omitempty"`
	Bytes int64      `json:"bytes,omitempty"`
	Error string     `json:"error,omitempty"`
}

// ExpiryReport is the result of one run or preview.
type ExpiryReport struct {
	SchemaVersion int       `json:"schemaVersion"`
	GeneratedAt   time.Time `json:"generatedAt"`
	Trigger       string    `json:"trigger"`
	DryRun        bool      `json:"dryRun"`
	AfterDays     int       `json:"afterDays"`
	// Disabled is true when the age is 0: nothing was inspected.
	Disabled    bool              `json:"disabled,omitempty"`
	Scopes      []string          `json:"scopes"`
	Items       []ExpiryItem      `json:"items"`
	ScopeErrors map[string]string `json:"scopeErrors,omitempty"`
	Expired     int               `json:"expired"`
	WouldExpire int               `json:"wouldExpire"`
	Kept        int               `json:"kept"`
	Failed      int               `json:"failed"`
}

// ExpireOptions select one expiry run.
type ExpireOptions struct {
	// Scopes limits the run; empty means every scope.
	Scopes []string
	// AfterDays overrides the deployment's expireAfterDays for this run only.
	// Nil uses the deployment setting; a pointer to 0 disables the run.
	AfterDays *int
	// DryRun previews decisions without removing anything.
	DryRun  bool
	trigger string
}

// ParseExpiryScopes validates a scope list. Empty means every scope.
func ParseExpiryScopes(scopes []string) ([]string, error) {
	if len(scopes) == 0 {
		return append([]string{}, ExpiryScopes...), nil
	}
	want := map[string]bool{}
	for _, s := range scopes {
		if !contains(ExpiryScopes, s) {
			return nil, fmt.Errorf("unknown cleanup scope %q (known: %s)", s, strings.Join(ExpiryScopes, ", "))
		}
		want[s] = true
	}
	var out []string
	for _, s := range ExpiryScopes {
		if want[s] {
			out = append(out, s)
		}
	}
	return out, nil
}

// idTime extracts the creation time every generated id carries
// (<prefix>-<UTC timestamp>-<hex>).
func idTime(id string) (time.Time, bool) {
	parts := strings.Split(id, "-")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	t, err := time.Parse("20060102T150405Z", parts[1])
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

func journalTerminal(j *Journal) bool {
	switch j.Status {
	case JournalCompleted, JournalAborted, JournalReverted:
		return true
	}
	// An unknown status is treated as unfinished.
	return false
}

// expiryWorld is the verified view of the deployment's operations. It is only
// built when state and every journal can be read.
type expiryWorld struct {
	state    State
	tracked  bool
	journals []*Journal
	byID     map[string]*Journal
}

func loadExpiryWorld(d *Deployment, now func() time.Time) (*expiryWorld, error) {
	st, tracked, err := LoadState(d)
	if err != nil {
		return nil, fmt.Errorf("current state is unreadable, refusing to expire anything: %w", err)
	}
	js, errs := listJournals(d, now)
	if len(errs) > 0 {
		return nil, fmt.Errorf("operation journals are unreadable, refusing to expire anything: %w", errors.Join(errs...))
	}
	w := &expiryWorld{state: st, tracked: tracked, journals: js, byID: map[string]*Journal{}}
	for _, j := range js {
		w.byID[j.ID] = j
		if !tracked && j.Status == JournalCompleted {
			return nil, errors.New("state.json is missing although completed operations exist, refusing to expire anything")
		}
	}
	if err := w.verifyRunningProducer(); err != nil {
		return nil, err
	}
	return w, nil
}

// verifyRunningProducer refuses the world when the operation that produced
// the running generation cannot be found or does not name the checkpoint that
// pairs with it. state.json names that operation in lastUpdateId; the
// operation's journal is the only record of the paired rollback checkpoint,
// so a missing or inconsistent journal must never read as "nothing is
// protected". A pristine generation (no lastUpdateId) has no producer.
//
// Every production path that writes a nonempty lastUpdateId (the update
// switch and the rollback switch) first saves the journal with the intent it
// then persists as state.json and with the paired checkpoint id, and the
// rollback restore paths only write back an earlier state. So a tracked
// producer must carry an intent that equals the running generation and a
// well-formed checkpoint id, the same linkage status requires before it
// offers a rollback. A journal without them cannot have produced this state.
func (w *expiryWorld) verifyRunningProducer() error {
	if !w.tracked || w.state.LastUpdateID == "" {
		return nil
	}
	id := w.state.LastUpdateID
	j := w.byID[id]
	if j == nil {
		return fmt.Errorf("generation %d names operation %s as its producer but that journal is missing, so its paired rollback checkpoint is unknown, refusing to expire anything", w.state.Generation, id)
	}
	if j.Intent == nil {
		return fmt.Errorf("generation %d names operation %s as its producer but that journal records no switch intent, refusing to expire anything", w.state.Generation, id)
	}
	if j.Intent.Generation != w.state.Generation {
		return fmt.Errorf("generation %d names operation %s as its producer but that journal switched to generation %d, refusing to expire anything", w.state.Generation, id, j.Intent.Generation)
	}
	if !checkpointIDPattern.MatchString(j.CheckpointID) {
		return fmt.Errorf("operation %s produced generation %d but its journal records no valid paired checkpoint (%q), refusing to expire anything", id, w.state.Generation, j.CheckpointID)
	}
	return nil
}

// volumeOwned reports whether a journal of this deployment created the
// volume (its update id label) or names it as a candidate, scratch or retired
// volume. The managed label alone is shared by every deployment on the engine
// and proves nothing about ownership.
func volumeOwned(name string, labels map[string]string, js []*Journal) bool {
	for _, j := range js {
		if j.ID == labels[LabelUpdate] || contains(j.RetiredVolumes, name) {
			return true
		}
		if j.Candidate != nil {
			if contains(j.Candidate.Scratch, name) {
				return true
			}
			for _, v := range j.Candidate.Volumes {
				if v == name {
					return true
				}
			}
		}
	}
	return false
}

// protectedCheckpoints are the ids no policy may remove: referenced by an
// unfinished operation, or by the operation that produced the running
// generation (its paired rollback checkpoint).
func (w *expiryWorld) protectedCheckpoints() map[string]bool {
	out := map[string]bool{}
	for _, j := range w.journals {
		if j.CheckpointID == "" {
			continue
		}
		if !journalTerminal(j) || (w.tracked && j.ID == w.state.LastUpdateID) {
			out[j.CheckpointID] = true
		}
	}
	return out
}

// protectedVolumes are the volumes of the running generation and every volume
// an unfinished operation references.
func (w *expiryWorld) protectedVolumes() map[string]bool {
	out := map[string]bool{}
	add := func(s *State) {
		if s == nil {
			return
		}
		for _, c := range s.Components {
			for _, v := range c.Volumes {
				out[v] = true
			}
		}
	}
	add(&w.state)
	for _, j := range w.journals {
		if journalTerminal(j) {
			continue
		}
		add(j.Previous)
		add(j.Intent)
		for _, v := range j.RetiredVolumes {
			out[v] = true
		}
		if j.Candidate != nil {
			for _, v := range j.Candidate.Volumes {
				out[v] = true
			}
			for _, v := range j.Candidate.Scratch {
				out[v] = true
			}
		}
	}
	return out
}

// expiryAction is one decision with the removal that carries it out.
type expiryAction struct {
	item ExpiryItem
	// remove runs for would-expire items on a real run. A non-empty keep
	// reason turns the item into a kept one (a last check refused it).
	remove func() (keep string, err error)
}

func keptItem(scope, id, reason string, since *time.Time) expiryAction {
	return expiryAction{item: ExpiryItem{Scope: scope, ID: id, Outcome: ExpiryKept, Reason: reason, Since: since}}
}

// expireCtx carries one run's clock and policy.
type expireCtx struct {
	e      *Engine
	w      *expiryWorld
	now    time.Time
	cutoff time.Time
	days   int
}

// due classifies an age start: the item is old enough when it is at least
// days old (since <= cutoff). A start in the future cannot be aged and keeps
// the item.
func (c expireCtx) due(since time.Time) (old bool, problem string) {
	switch {
	case since.IsZero():
		return false, "no usable creation time"
	case since.After(c.now):
		return false, "creation time is in the future"
	}
	return !since.After(c.cutoff), ""
}

func (c expireCtx) reason() string {
	return fmt.Sprintf("older than %d days and unused", c.days)
}

func (c expireCtx) checkpoints() ([]expiryAction, error) {
	store := CheckpointStore{Dir: c.e.Dep.BackupDir}
	inv, err := store.Inventory()
	if err != nil {
		return nil, fmt.Errorf("backup directory: %w", err)
	}
	protected := c.w.protectedCheckpoints()
	var out []expiryAction
	for _, cp := range inv.Complete {
		since := cp.CreatedAt.UTC()
		old, problem := c.due(since)
		switch {
		case protected[cp.ID]:
			out = append(out, keptItemBytes(ScopeCheckpoints, cp.ID, "protected: paired with the running generation or used by an unfinished operation", &since, cp.TotalSealedBytes()))
		case problem != "":
			out = append(out, keptItemBytes(ScopeCheckpoints, cp.ID, problem, nil, cp.TotalSealedBytes()))
		case old:
			dir := filepath.Join(store.Dir, cp.ID)
			out = append(out, expiryAction{
				item:   ExpiryItem{Scope: ScopeCheckpoints, ID: cp.ID, Outcome: ExpiryWouldExpire, Reason: c.reason(), Since: &since, Bytes: cp.TotalSealedBytes()},
				remove: func() (string, error) { return "", os.RemoveAll(dir) },
			})
		}
	}
	for _, name := range inv.Malformed {
		out = append(out, keptItem(ScopeCheckpoints, name, "metadata is unreadable or invalid; only an explicit prune removes it", nil))
	}
	return out, nil
}

func keptItemBytes(scope, id, reason string, since *time.Time, size int64) expiryAction {
	a := keptItem(scope, id, reason, since)
	a.item.Bytes = size
	return a
}

func (c expireCtx) staging() ([]expiryAction, error) {
	var out []expiryAction
	inflightCP, inflightPlan := map[string]bool{}, map[string]bool{}
	for _, j := range c.w.journals {
		if journalTerminal(j) {
			continue
		}
		if j.CheckpointID != "" {
			inflightCP[j.CheckpointID] = true
		}
		if j.PlanID != "" {
			inflightPlan[j.PlanID] = true
		}
	}
	consider := func(id, name string, inflight bool, remove func() error) {
		since, ok := idTime(id)
		if !ok {
			out = append(out, keptItem(ScopeStaging, name, "name carries no creation time", nil))
			return
		}
		old, problem := c.due(since)
		switch {
		case inflight:
			out = append(out, keptItem(ScopeStaging, name, "referenced by an unfinished operation", &since))
		case problem != "":
			out = append(out, keptItem(ScopeStaging, name, problem, nil))
		case old:
			out = append(out, expiryAction{
				item:   ExpiryItem{Scope: ScopeStaging, ID: name, Outcome: ExpiryWouldExpire, Reason: c.reason(), Since: &since},
				remove: func() (string, error) { return "", remove() },
			})
		}
	}

	store := CheckpointStore{Dir: c.e.Dep.BackupDir}
	inv, err := store.Inventory()
	if err != nil {
		return nil, fmt.Errorf("backup directory: %w", err)
	}
	for _, name := range inv.Incomplete {
		id := strings.TrimPrefix(name, incompletePrefix)
		if !checkpointIDPattern.MatchString(id) {
			out = append(out, keptItem(ScopeStaging, name, "unrecognized staging directory name", nil))
			continue
		}
		dir := filepath.Join(store.Dir, name)
		consider(id, name, inflightCP[id], func() error { return os.RemoveAll(dir) })
	}

	entries, err := os.ReadDir(c.e.Dep.plansDir())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("plans directory: %w", err)
	}
	for _, ent := range entries {
		id := strings.TrimSuffix(ent.Name(), ".json")
		if id == ent.Name() || !planIDPattern.MatchString(id) || !ent.Type().IsRegular() {
			continue
		}
		path := filepath.Join(c.e.Dep.plansDir(), ent.Name())
		consider(id, ent.Name(), inflightPlan[id], func() error { return os.Remove(path) })
	}
	return out, nil
}

func (c expireCtx) generations() ([]expiryAction, error) {
	dir := c.e.Dep.generationsDir()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("generations directory: %w", err)
	}
	var nums []int
	for _, ent := range entries {
		n, err := strconv.Atoi(ent.Name())
		if err != nil || n < 1 || strconv.Itoa(n) != ent.Name() || !ent.IsDir() {
			continue
		}
		nums = append(nums, n)
	}
	if len(nums) == 0 {
		return nil, nil
	}
	sort.Ints(nums)
	if !c.w.tracked {
		return nil, errors.New("state.json is missing, so the running generation is unknown")
	}

	protected := map[int]bool{}
	store := CheckpointStore{Dir: c.e.Dep.BackupDir}
	for id := range c.w.protectedCheckpoints() {
		cp, err := store.Load(id)
		if err != nil {
			return nil, fmt.Errorf("protected checkpoint %s is unreadable: %w", id, err)
		}
		protected[cp.Previous.Generation] = true
	}
	for _, j := range c.w.journals {
		if journalTerminal(j) {
			continue
		}
		for _, s := range []*State{j.Previous, j.Intent} {
			if s != nil {
				protected[s.Generation] = true
			}
		}
	}

	states := map[int]State{}
	valid := map[int]bool{}
	for _, n := range nums {
		st, ok, err := readStateFile(filepath.Join(c.e.Dep.generationDir(n), "state.json"))
		if err == nil && ok && st.Generation == n && !st.UpdatedAt.IsZero() {
			states[n], valid[n] = st, true
		}
	}

	var out []expiryAction
	for _, n := range nums {
		if n >= c.w.state.Generation {
			continue
		}
		id := strconv.Itoa(n)
		if protected[n] {
			out = append(out, keptItem(ScopeGenerations, id, "protected: referenced by the paired rollback checkpoint or an unfinished operation", nil))
			continue
		}
		if !valid[n] {
			out = append(out, keptItem(ScopeGenerations, id, "generation record is unreadable or invalid", nil))
			continue
		}
		// The generation became obsolete when the next recorded one took over.
		var successor *State
		for _, m := range nums {
			if m > n && valid[m] {
				s := states[m]
				successor = &s
				break
			}
		}
		if successor == nil {
			out = append(out, keptItem(ScopeGenerations, id, "no later generation record dates its retirement", nil))
			continue
		}
		since := successor.UpdatedAt.UTC()
		old, problem := c.due(since)
		gdir := c.e.Dep.generationDir(n)
		if problem == "" && old {
			if reason := unexpectedGenerationContent(gdir); reason != "" {
				out = append(out, keptItem(ScopeGenerations, id, reason, &since))
				continue
			}
		}
		switch {
		case problem != "":
			out = append(out, keptItem(ScopeGenerations, id, problem, nil))
		case old:
			out = append(out, expiryAction{
				item:   ExpiryItem{Scope: ScopeGenerations, ID: id, Outcome: ExpiryWouldExpire, Reason: c.reason(), Since: &since},
				remove: func() (string, error) { return "", os.RemoveAll(gdir) },
			})
		}
	}
	return out, nil
}

// unexpectedGenerationContent keeps a generation directory that holds more
// than the files the update logic writes there.
func unexpectedGenerationContent(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "generation directory is unreadable"
	}
	for _, ent := range entries {
		if !ent.Type().IsRegular() || (ent.Name() != "state.json" && ent.Name() != "compose.override.yaml") {
			return "generation directory holds unexpected content"
		}
	}
	return ""
}

func (c expireCtx) volumes(ctx context.Context) ([]expiryAction, error) {
	vols, err := c.e.Docker.ManagedVolumes(ctx)
	if err != nil {
		return nil, fmt.Errorf("list managed volumes: %w", err)
	}
	protected := c.w.protectedVolumes()

	// Retirement evidence comes only from journals that completed: a failed
	// or reverted operation's retired list may name volumes that run again.
	retiredAt := map[string]time.Time{}
	listed := map[string]bool{}
	named := map[string]bool{}
	for _, j := range c.w.journals {
		for _, v := range j.RetiredVolumes {
			listed[v], named[v] = true, true
			if j.Status == JournalCompleted && j.FinishedAt != nil && j.FinishedAt.After(retiredAt[v]) {
				retiredAt[v] = j.FinishedAt.UTC()
			}
		}
		if j.Candidate != nil {
			for _, v := range j.Candidate.Volumes {
				named[v] = true
			}
			for _, v := range j.Candidate.Scratch {
				named[v] = true
			}
		}
	}

	sort.Slice(vols, func(a, b int) bool { return vols[a].Name < vols[b].Name })
	var out []expiryAction
	for _, v := range vols {
		j := c.w.byID[v.Labels[LabelUpdate]]
		if j == nil {
			if named[v.Name] {
				out = append(out, keptItem(ScopeVolumes, v.Name, "the operation that created it has no readable record", nil))
			}
			// Otherwise another deployment on this engine owns the volume.
			continue
		}
		if protected[v.Name] {
			out = append(out, keptItem(ScopeVolumes, v.Name, "protected: used by the running generation or an unfinished operation", nil))
			continue
		}
		if !journalTerminal(j) {
			out = append(out, keptItem(ScopeVolumes, v.Name, "created by an unfinished operation", nil))
			continue
		}
		created, err := time.Parse(time.RFC3339, v.Labels[LabelCreated])
		if err != nil {
			out = append(out, keptItem(ScopeVolumes, v.Name, "creation label is missing or malformed", nil))
			continue
		}
		since := created.UTC()
		switch {
		case listed[v.Name]:
			at, ok := retiredAt[v.Name]
			if !ok {
				out = append(out, keptItem(ScopeVolumes, v.Name, "listed as retired only by an operation that did not complete", nil))
				continue
			}
			if at.After(since) {
				since = at
			}
		case j.Status == JournalCompleted:
			out = append(out, keptItem(ScopeVolumes, v.Name, "no record says it was retired", nil))
			continue
		case j.FinishedAt == nil:
			out = append(out, keptItem(ScopeVolumes, v.Name, "the operation that created it has no finish time", nil))
			continue
		default:
			if j.FinishedAt.After(since) {
				since = j.FinishedAt.UTC()
			}
		}
		old, problem := c.due(since)
		if problem != "" {
			out = append(out, keptItem(ScopeVolumes, v.Name, problem, nil))
			continue
		}
		if !old {
			continue
		}
		name := v.Name
		busy := func() (string, error) {
			users, err := c.e.Docker.VolumeUsers(ctx, name)
			if err != nil {
				return "", fmt.Errorf("inspect users of volume: %w", err)
			}
			if len(users) > 0 {
				return fmt.Sprintf("in use by %d container(s)", len(users)), nil
			}
			return "", nil
		}
		reason, err := busy()
		switch {
		case err != nil:
			out = append(out, expiryAction{item: ExpiryItem{Scope: ScopeVolumes, ID: name, Outcome: ExpiryFailed, Since: &since, Error: err.Error()}})
		case reason != "":
			out = append(out, keptItem(ScopeVolumes, name, reason, &since))
		default:
			out = append(out, expiryAction{
				item: ExpiryItem{Scope: ScopeVolumes, ID: name, Outcome: ExpiryWouldExpire, Reason: c.reason(), Since: &since},
				remove: func() (string, error) {
					if reason, err := busy(); err != nil || reason != "" {
						return reason, err
					}
					return "", c.e.Docker.VolumeRemove(ctx, name)
				},
			})
		}
	}
	return out, nil
}

// ExpireUnused removes (or, with DryRun, lists) unused old system data older
// than the configured age. It holds the deployment lock like every other
// mutation and returns ErrOperationInProgress when an update or rollback runs.
// The report is always filled; the error joins scope errors and removal
// failures. An age of 0 disables the run without inspecting anything.
func (e *Engine) ExpireUnused(ctx context.Context, o ExpireOptions) (ExpiryReport, error) {
	days := e.Dep.ExpireAfter()
	if o.AfterDays != nil {
		days = *o.AfterDays
	}
	trigger := o.trigger
	if trigger == "" {
		trigger = TriggerManual
	}
	report := ExpiryReport{SchemaVersion: expiryReportSchema, GeneratedAt: e.Now().UTC(), Trigger: trigger, DryRun: o.DryRun, AfterDays: days, Items: []ExpiryItem{}}
	if days < 0 || days > MaxExpireAfterDays {
		return report, fmt.Errorf("expiry age must be between 0 (disabled) and %d days", MaxExpireAfterDays)
	}
	scopes, err := ParseExpiryScopes(o.Scopes)
	if err != nil {
		return report, err
	}
	report.Scopes = scopes
	if days == 0 {
		report.Disabled = true
		return report, nil
	}

	if err := os.MkdirAll(e.Dep.stateSubdir(), 0o700); err != nil {
		return report, fmt.Errorf("create state directory: %w", err)
	}
	unlock, err := lockFile(e.Dep.lockPath())
	if err != nil {
		return report, err
	}
	defer unlock()

	w, err := loadExpiryWorld(e.Dep, e.Now)
	if err != nil {
		return report, err
	}
	now := report.GeneratedAt
	c := expireCtx{e: e, w: w, now: now, cutoff: now.Add(-time.Duration(days) * 24 * time.Hour), days: days}

	var errs []error
	for _, scope := range scopes {
		var actions []expiryAction
		var err error
		switch scope {
		case ScopeCheckpoints:
			actions, err = c.checkpoints()
		case ScopeVolumes:
			actions, err = c.volumes(ctx)
		case ScopeGenerations:
			actions, err = c.generations()
		case ScopeStaging:
			actions, err = c.staging()
		}
		if err != nil {
			if report.ScopeErrors == nil {
				report.ScopeErrors = map[string]string{}
			}
			report.ScopeErrors[scope] = err.Error()
			errs = append(errs, fmt.Errorf("%s: %w", scope, err))
			continue
		}
		sort.SliceStable(actions, func(a, b int) bool { return actions[a].item.ID < actions[b].item.ID })
		for _, a := range actions {
			if a.item.Outcome == ExpiryWouldExpire && !o.DryRun {
				keep, rerr := a.remove()
				switch {
				case rerr != nil:
					a.item.Outcome, a.item.Error = ExpiryFailed, rerr.Error()
				case keep != "":
					a.item.Outcome, a.item.Reason = ExpiryKept, keep
				default:
					a.item.Outcome = ExpiryExpired
				}
			}
			if a.item.Outcome == ExpiryFailed {
				errs = append(errs, fmt.Errorf("%s %s: %s", a.item.Scope, a.item.ID, a.item.Error))
			}
			report.Items = append(report.Items, a.item)
		}
	}
	for _, it := range report.Items {
		switch it.Outcome {
		case ExpiryExpired:
			report.Expired++
		case ExpiryWouldExpire:
			report.WouldExpire++
		case ExpiryKept:
			report.Kept++
		case ExpiryFailed:
			report.Failed++
		}
	}
	if !o.DryRun {
		if err := os.MkdirAll(e.Dep.stateSubdir(), 0o700); err == nil {
			if err := writeJSONAtomic(e.Dep.expiryLastFile(), report, 0o600); err != nil {
				errs = append(errs, fmt.Errorf("record expiry report: %w", err))
			}
		} else {
			errs = append(errs, fmt.Errorf("record expiry report: %w", err))
		}
	}
	return report, errors.Join(errs...)
}

func (d *Deployment) expiryLastFile() string { return filepath.Join(d.stateSubdir(), expiryLastName) }

// LastExpiry returns the report of the most recent real (non-preview) run. A
// missing file yields ok=false.
func (e *Engine) LastExpiry() (ExpiryReport, bool, error) {
	var r ExpiryReport
	data, err := readFileLimit(e.Dep.expiryLastFile(), 16<<20)
	if errors.Is(err, os.ErrNotExist) {
		return r, false, nil
	}
	if err != nil {
		return r, false, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return ExpiryReport{}, false, fmt.Errorf("decode expiry report: %w", err)
	}
	if err := ensureJSONEOF(dec); err != nil {
		return ExpiryReport{}, false, fmt.Errorf("decode expiry report: %w", err)
	}
	if r.SchemaVersion != expiryReportSchema {
		return ExpiryReport{}, false, fmt.Errorf("expiry report schema version %d is not supported", r.SchemaVersion)
	}
	return r, true, nil
}

// AutoExpire is the automatic trigger, called after ordinary lifecycle
// activity. It runs at most once per AutoExpireInterval unless the previous
// automatic run failed, does nothing when expiry is disabled, and silently
// yields to a running update or rollback. ran reports whether it executed.
func (e *Engine) AutoExpire(ctx context.Context) (report ExpiryReport, ran bool, err error) {
	if e.Dep.ExpireAfter() == 0 {
		return ExpiryReport{}, false, nil
	}
	if last, ok, lerr := e.LastExpiry(); lerr == nil && ok && last.Trigger == TriggerAutomatic && !last.DryRun &&
		last.Failed == 0 && len(last.ScopeErrors) == 0 {
		if age := e.Now().UTC().Sub(last.GeneratedAt); age >= 0 && age < AutoExpireInterval {
			return ExpiryReport{}, false, nil
		}
	}
	report, err = e.ExpireUnused(ctx, ExpireOptions{trigger: TriggerAutomatic})
	if errors.Is(err, ErrOperationInProgress) {
		return ExpiryReport{}, false, nil
	}
	return report, true, err
}

// SetExpireAfterDays stores expireAfterDays (0 disables age expiry) in
// deployment.json. It holds the deployment lock, so it cannot interleave with
// an update or rollback, and rereads the file under that lock, so a concurrent
// edit of any other setting is preserved instead of overwritten. A managed
// deployment is written by its manager's worker (the typed policy action),
// which holds the project and helper locks as well.
func (e *Engine) SetExpireAfterDays(days int) error {
	if days < 0 || days > MaxExpireAfterDays {
		return fmt.Errorf("expireAfterDays must be between 0 (disabled) and %d", MaxExpireAfterDays)
	}
	if e.Dep.File == "" {
		return errors.New("deployment has no file path")
	}
	if err := os.MkdirAll(e.Dep.stateSubdir(), 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	unlock, err := lockFile(e.Dep.lockPath())
	if err != nil {
		return err
	}
	defer unlock()
	fresh, err := LoadDeployment(e.Dep.File)
	if err != nil {
		return err
	}
	fresh.ExpireAfterDays = &days
	if err := fresh.Save(); err != nil {
		return err
	}
	e.Dep.ExpireAfterDays = &days
	return nil
}
