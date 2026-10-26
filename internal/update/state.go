package update

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/auth"
)

const stateSchemaVersion = 1

// Identifier patterns. Every id that comes from the command line is checked
// against one of these before it is used in a path.
var (
	updateIDPattern     = regexp.MustCompile(`^[ur]-[0-9]{8}T[0-9]{6}Z-[0-9a-f]{6}$`)
	checkpointIDPattern = regexp.MustCompile(`^cp-[0-9]{8}T[0-9]{6}Z-[0-9a-f]{6}$`)
	planIDPattern       = regexp.MustCompile(`^p-[0-9]{8}T[0-9]{6}Z-[0-9a-f]{6}$`)
)

// newID returns prefix-<UTC timestamp>-<6 hex>.
func newID(prefix string, now time.Time) (string, error) {
	suffix, err := auth.RandomHex(3)
	if err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return prefix + "-" + now.UTC().Format("20060102T150405Z") + "-" + suffix, nil
}

// Component ids in State.Components.
const gatewayComponent = "gateway"

func runtimeComponent(workspace string) string { return "runtime/" + workspace }

// ComponentState is what one service runs: image and volumes.
type ComponentState struct {
	Service string `json:"service"`
	// ReleaseID is empty for the untracked baseline (the compose file's own
	// image before the first managed update).
	ReleaseID string `json:"releaseId,omitempty"`
	// Image is the reference compose runs (repository@digest) and ImageID the
	// digest it was verified to resolve to.
	Image   string `json:"image"`
	ImageID string `json:"imageId"`
	// Volumes maps compose volume keys to the Docker volume names in use.
	Volumes map[string]string `json:"volumes"`
}

// State is the deployment's current generation. It is written atomically after
// a switch or rollback has been verified healthy.
type State struct {
	SchemaVersion int                       `json:"schemaVersion"`
	Generation    int                       `json:"generation"`
	Components    map[string]ComponentState `json:"components"`
	UpdatedAt     time.Time                 `json:"updatedAt"`
	LastUpdateID  string                    `json:"lastUpdateId,omitempty"`
	// ReleaseID is the release the deployment as a whole runs ("" when
	// untracked).
	ReleaseID string `json:"releaseId,omitempty"`
}

// Clone returns a deep copy.
func (s State) Clone() State {
	out := s
	out.Components = make(map[string]ComponentState, len(s.Components))
	for id, c := range s.Components {
		c.Volumes = cloneStringMap(c.Volumes)
		out.Components[id] = c
	}
	return out
}

func cloneStringMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// LoadState reads the state file. A missing file yields ok=false.
func LoadState(d *Deployment) (State, bool, error) {
	return readStateFile(d.stateFile())
}

// readStateFile strictly decodes one state.json (the current one or a
// generation copy). A missing file yields ok=false.
func readStateFile(path string) (State, bool, error) {
	data, err := readFileLimit(path, 4<<20)
	if errors.Is(err, os.ErrNotExist) {
		return State{}, false, nil
	}
	if err != nil {
		return State{}, false, fmt.Errorf("read state: %w", err)
	}
	var s State
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return State{}, false, fmt.Errorf("decode state: %w", err)
	}
	if err := ensureJSONEOF(dec); err != nil {
		return State{}, false, fmt.Errorf("decode state: %w", err)
	}
	if s.SchemaVersion != stateSchemaVersion {
		return State{}, false, fmt.Errorf("state schema version %d is not supported", s.SchemaVersion)
	}
	return s, true, nil
}

// SaveState writes the state file and a copy in generations/<n>/state.json so
// every generation stays inspectable.
func SaveState(d *Deployment, s State) error {
	s.SchemaVersion = stateSchemaVersion
	if err := os.MkdirAll(d.generationDir(s.Generation), 0o700); err != nil {
		return fmt.Errorf("create generation directory: %w", err)
	}
	if err := writeJSONAtomic(filepath.Join(d.generationDir(s.Generation), "state.json"), s, 0o600); err != nil {
		return err
	}
	return writeJSONAtomic(d.stateFile(), s, 0o600)
}

// overridePath is where the compose override of generation n lives.
func (d *Deployment) overridePath(n int) string {
	return filepath.Join(d.generationDir(n), "compose.override.yaml")
}

// composeFor returns the compose invocation of state s: the base files plus,
// once a generation exists, its override.
func (d *Deployment) composeFor(dk Docker, s State, tracked bool) ComposeCLI {
	files := append([]string{}, d.Compose.Files...)
	if tracked && s.Generation > 0 {
		files = append(files, d.overridePath(s.Generation))
	}
	return ComposeCLI{D: dk, Project: d.Compose.Project, Files: files, EnvFiles: d.Compose.EnvFiles, Dir: d.Compose.ProjectDir}
}

// Step ids and names of the update runbook (docs/UPDATE-RUNBOOK.md section 3).
var updateSteps = []struct{ ID, Name string }{
	{"01", "verify-release"},
	{"02", "capacity"},
	{"03", "maintenance"},
	{"04", "drain"},
	{"05", "stop-clean"},
	{"06", "snapshot"},
	{"07", "candidate-volumes"},
	{"08", "candidate-smoke"},
	{"09", "candidate-isolation"},
	{"10", "switch"},
	{"11", "health"},
}

// Rollback step ids.
var rollbackSteps = []struct{ ID, Name string }{
	{"R1", "verify-checkpoint"},
	{"R2", "capacity"},
	{"R3", "maintenance"},
	{"R4", "stop-current"},
	{"R5", "restore-volumes"},
	{"R6", "switch"},
	{"R7", "health"},
}

// Step statuses.
const (
	StepPending = "pending"
	StepRunning = "running"
	StepOK      = "ok"
	StepFailed  = "failed"
	StepSkipped = "skipped"
)

// Journal statuses.
const (
	JournalRunning          = "running"
	JournalAwaitingApproval = "awaiting-approval"
	JournalCompleted        = "completed"
	// JournalAborted: the operation stopped before changing anything that
	// matters and the previous services run again.
	JournalAborted = "aborted"
	// JournalReverted: the switch failed and the previous generation runs
	// again on its own volumes.
	JournalReverted = "failed-reverted"
	// JournalHeld: services are stopped or in an unknown state and an
	// operator has to act (Recovery says how).
	JournalHeld = "failed-held"
)

// Step is one runbook step in a journal.
type Step struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Status     string     `json:"status"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	Detail     string     `json:"detail,omitempty"`
	Error      string     `json:"error,omitempty"`
}

// CandidateRecord lists everything created for candidate verification.
type CandidateRecord struct {
	Project string `json:"project,omitempty"`
	// Volumes maps compose volume keys to the candidate (cloned) volumes that
	// become production on switch.
	Volumes map[string]string `json:"volumes,omitempty"`
	// Scratch are throwaway volumes of retained datasets.
	Scratch []string `json:"scratch,omitempty"`
}

// Journal records one update or rollback so that a failure leaves an
// explainable state (UP-04).
type Journal struct {
	SchemaVersion int        `json:"schemaVersion"`
	Kind          string     `json:"kind"` // "update" | "rollback"
	ID            string     `json:"id"`
	PlanID        string     `json:"planId,omitempty"`
	Workspace     string     `json:"workspace"`
	Release       string     `json:"release,omitempty"`
	CheckpointID  string     `json:"checkpointId,omitempty"`
	StartedAt     time.Time  `json:"startedAt"`
	FinishedAt    *time.Time `json:"finishedAt,omitempty"`
	Status        string     `json:"status"`
	Steps         []Step     `json:"steps"`
	// Previous is the state before the operation; Intent the state the
	// switch activates.
	Previous        *State           `json:"previous,omitempty"`
	Intent          *State           `json:"intent,omitempty"`
	Candidate       *CandidateRecord `json:"candidate,omitempty"`
	RetiredVolumes  []string         `json:"retiredVolumes,omitempty"`
	VerifiedAt      *time.Time       `json:"verifiedAt,omitempty"`
	Error           string           `json:"error,omitempty"`
	Recovery        string           `json:"recovery,omitempty"`
	Evidence        []string         `json:"evidence,omitempty"`
	RuntimeGenAfter string           `json:"runtimeGenerationAfter,omitempty"`
	// StoppedServices are the compose services this operation stopped, in
	// stop order; recovery restarts them in reverse order.
	StoppedServices []string `json:"stoppedServices,omitempty"`
	// MaintenanceFenced is true while the runtime MAINTENANCE fence this
	// operation set is active.
	MaintenanceFenced bool `json:"maintenanceFenced,omitempty"`

	dir string
	now func() time.Time
}

func newJournal(d *Deployment, kind, id, planID, workspace, release string, now func() time.Time) (*Journal, error) {
	dir := filepath.Join(d.updatesDir(), id)
	if err := os.MkdirAll(filepath.Join(dir, "evidence"), 0o700); err != nil {
		return nil, fmt.Errorf("create journal directory: %w", err)
	}
	steps := updateSteps
	if kind == "rollback" {
		steps = rollbackSteps
	}
	j := &Journal{
		SchemaVersion: 1, Kind: kind, ID: id, PlanID: planID, Workspace: workspace, Release: release,
		StartedAt: now().UTC(), Status: JournalRunning, dir: dir, now: now,
	}
	for _, s := range steps {
		j.Steps = append(j.Steps, Step{ID: s.ID, Name: s.Name, Status: StepPending})
	}
	return j, j.save()
}

// loadJournal reads updates/<id>/journal.json.
func loadJournal(d *Deployment, id string, now func() time.Time) (*Journal, error) {
	if !updateIDPattern.MatchString(id) {
		return nil, fmt.Errorf("invalid update id %q", id)
	}
	dir := filepath.Join(d.updatesDir(), id)
	data, err := readFileLimit(filepath.Join(dir, "journal.json"), 8<<20)
	if err != nil {
		return nil, fmt.Errorf("read journal %s: %w", id, err)
	}
	var j Journal
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, fmt.Errorf("decode journal %s: %w", id, err)
	}
	j.dir, j.now = dir, now
	return &j, nil
}

// listJournals returns all journals, oldest first. Unreadable ones are
// skipped and reported in the second result.
func listJournals(d *Deployment, now func() time.Time) ([]*Journal, []error) {
	entries, err := os.ReadDir(d.updatesDir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, []error{err}
	}
	var out []*Journal
	var errs []error
	for _, e := range entries {
		if !e.IsDir() || !updateIDPattern.MatchString(e.Name()) {
			continue
		}
		j, err := loadJournal(d, e.Name(), now)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, j)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].StartedAt.Before(out[b].StartedAt) })
	return out, errs
}

func (j *Journal) save() error {
	return writeJSONAtomic(filepath.Join(j.dir, "journal.json"), j, 0o600)
}

func (j *Journal) step(id string) *Step {
	for i := range j.Steps {
		if j.Steps[i].ID == id {
			return &j.Steps[i]
		}
	}
	panic("update: unknown journal step " + id)
}

func (j *Journal) begin(id string) error {
	s := j.step(id)
	t := j.now().UTC()
	s.Status, s.StartedAt, s.FinishedAt, s.Error = StepRunning, &t, nil, ""
	return j.save()
}

func (j *Journal) ok(id, detail string) error {
	s := j.step(id)
	t := j.now().UTC()
	s.Status, s.FinishedAt, s.Detail = StepOK, &t, detail
	return j.save()
}

func (j *Journal) skip(id, detail string) error {
	s := j.step(id)
	s.Status, s.Detail = StepSkipped, detail
	return j.save()
}

func (j *Journal) fail(id string, err error) {
	s := j.step(id)
	t := j.now().UTC()
	s.Status, s.FinishedAt, s.Error = StepFailed, &t, err.Error()
	j.Error = fmt.Sprintf("step %s (%s): %v", s.ID, s.Name, err)
	_ = j.save()
}

func (j *Journal) finish(status, recovery string) {
	t := j.now().UTC()
	j.Status, j.FinishedAt, j.Recovery = status, &t, recovery
	_ = j.save()
}

// evidencePath returns the path of an evidence file and records its name.
func (j *Journal) evidencePath(name string) string {
	if !contains(j.Evidence, name) {
		j.Evidence = append(j.Evidence, name)
	}
	return filepath.Join(j.dir, "evidence", name)
}

// pendingJournal returns the first journal that still needs an operator or
// holds the deployment mid-operation.
func pendingJournal(d *Deployment, now func() time.Time) *Journal {
	js, _ := listJournals(d, now)
	for _, j := range js {
		switch j.Status {
		case JournalRunning, JournalAwaitingApproval:
			return j
		case JournalHeld:
			// A final health read may fail after a coherent new generation
			// was saved. Rollback must remain available in that state; only a
			// held operation that did not finish its switch blocks another.
			if j.Intent != nil {
				if st, ok, err := LoadState(d); err == nil && ok && st.Generation == j.Intent.Generation && st.LastUpdateID == j.ID {
					continue
				}
			}
			return j
		}
	}
	return nil
}
