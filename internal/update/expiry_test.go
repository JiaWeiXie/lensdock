package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

const day = 24 * time.Hour

// expiryDocker is an in-memory engine for the volume commands expiry uses. Any
// other command fails the test run so expiry can never reach for images or a
// global prune.
type expiryDocker struct {
	volumes map[string]map[string]string // managed volumes: name -> labels
	users   map[string][]string          // volume -> container ids
	rmErr   map[string]error
	removed []string
	log     []string
}

func (f *expiryDocker) Run(_ context.Context, c Cmd) error {
	a := c.Args
	f.log = append(f.log, strings.Join(a, " "))
	switch {
	case len(a) > 1 && a[0] == "volume" && a[1] == "ls":
		names := make([]string, 0, len(f.volumes))
		for n := range f.volumes {
			names = append(names, n)
		}
		sort.Strings(names)
		_, err := io.WriteString(c.Stdout, strings.Join(names, "\n"))
		return err
	case len(a) > 1 && a[0] == "volume" && a[1] == "inspect":
		name := a[len(a)-1]
		labels, ok := f.volumes[name]
		if !ok {
			return &ExitError{Argv: a, Code: 1, Stderr: "Error: No such volume: " + name}
		}
		raw, _ := json.Marshal(VolumeInfo{Name: name, Driver: "local", Labels: labels})
		_, err := c.Stdout.Write(raw)
		return err
	case len(a) > 1 && a[0] == "container" && a[1] == "ls":
		name := strings.TrimPrefix(a[len(a)-1], "volume=")
		_, err := io.WriteString(c.Stdout, strings.Join(f.users[name], "\n"))
		return err
	case len(a) > 3 && a[0] == "compose" && a[len(a)-3] == "config":
		_, err := io.WriteString(c.Stdout, "{}")
		return err
	case len(a) == 3 && a[0] == "volume" && a[1] == "rm":
		if err := f.rmErr[a[2]]; err != nil {
			return err
		}
		f.removed = append(f.removed, a[2])
		delete(f.volumes, a[2])
		return nil
	}
	return fmt.Errorf("unexpected docker command %v", a)
}

type expiryFixture struct {
	t   *testing.T
	dep *Deployment
	now time.Time
	dk  *expiryDocker
	eng *Engine
}

func newExpiryFixture(t *testing.T) *expiryFixture {
	t.Helper()
	dir := t.TempDir()
	f := &expiryFixture{
		t:   t,
		now: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC),
		dk:  &expiryDocker{volumes: map[string]map[string]string{}, users: map[string][]string{}, rmErr: map[string]error{}},
	}
	f.dep = &Deployment{
		StateDir: dir, BackupDir: filepath.Join(dir, "backups"),
		Workspaces:      map[string]WorkspaceConfig{"default": {}},
		ExpireAfterDays: new(30),
	}
	for _, d := range []string{f.dep.BackupDir, f.dep.updatesDir(), f.dep.generationsDir(), f.dep.stateSubdir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	f.eng = &Engine{Dep: f.dep, Docker: Docker{Run: f.dk}, Out: io.Discard, Now: f.clock}
	return f
}

func (f *expiryFixture) clock() time.Time { return f.now }

func stamp(t time.Time) string { return t.UTC().Format("20060102T150405Z") }

// checkpoint writes a complete clean checkpoint created ago before the fixture clock.
func (f *expiryFixture) checkpoint(ago time.Duration, suffix string, previous int) string {
	f.t.Helper()
	at := f.now.Add(-ago)
	id := "cp-" + stamp(at) + "-" + suffix
	dir := filepath.Join(f.dep.BackupDir, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		f.t.Fatal(err)
	}
	cp := Checkpoint{SchemaVersion: checkpointSchemaVersion, ID: id, CreatedAt: at, Consistency: ConsistencyClean, Previous: State{Generation: previous}}
	if err := writeJSONAtomic(filepath.Join(dir, checkpointFileName), cp, 0o600); err != nil {
		f.t.Fatal(err)
	}
	return id
}

// journal writes an update journal; finishedAgo < 0 leaves it unfinished.
func (f *expiryFixture) journal(id, status string, finishedAgo time.Duration, mod func(j *Journal)) {
	f.t.Helper()
	j, err := newJournal(f.dep, "update", id, "", "default", "r", f.clock)
	if err != nil {
		f.t.Fatal(err)
	}
	j.Status = status
	if finishedAgo >= 0 {
		at := f.now.Add(-finishedAgo)
		j.FinishedAt = &at
		j.StartedAt = at.Add(-time.Hour)
	}
	if mod != nil {
		mod(j)
	}
	if err := j.save(); err != nil {
		f.t.Fatal(err)
	}
}

func uid(suffix string) string { return "u-20260101T000000Z-" + suffix }

func (f *expiryFixture) generation(n int, ago time.Duration) {
	f.t.Helper()
	st := State{Generation: n, UpdatedAt: f.now.Add(-ago), Components: map[string]ComponentState{}}
	if err := SaveState(f.dep, st); err != nil {
		f.t.Fatal(err)
	}
}

// current declares the running generation (written last so it is state.json).
func (f *expiryFixture) current(n int, ago time.Duration, lastUpdate string, volumes map[string]string) {
	f.t.Helper()
	st := State{Generation: n, UpdatedAt: f.now.Add(-ago), LastUpdateID: lastUpdate, Components: map[string]ComponentState{
		"runtime/default": {Service: "runtime", Volumes: volumes},
	}}
	if err := SaveState(f.dep, st); err != nil {
		f.t.Fatal(err)
	}
}

func (f *expiryFixture) volume(name, update string, createdAgo time.Duration) {
	f.dk.volumes[name] = map[string]string{
		LabelManaged: "true", LabelUpdate: update, LabelRole: RoleRetired,
		LabelCreated: f.now.Add(-createdAgo).Format(time.RFC3339),
	}
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	if err == nil {
		return true
	}
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	t.Fatal(err)
	return false
}

func findItem(r ExpiryReport, scope, id string) (ExpiryItem, bool) {
	for _, it := range r.Items {
		if it.Scope == scope && it.ID == id {
			return it, true
		}
	}
	return ExpiryItem{}, false
}

func wantOutcome(t *testing.T, r ExpiryReport, scope, id, outcome string) {
	t.Helper()
	it, ok := findItem(r, scope, id)
	if !ok || it.Outcome != outcome {
		t.Fatalf("%s %s: outcome = %q (listed %v), want %q", scope, id, it.Outcome, ok, outcome)
	}
}

func TestExpiryAgeBoundaryFollowsTheClock(t *testing.T) {
	f := newExpiryFixture(t)
	justYoung := f.checkpoint(30*day-time.Minute, "aaaaaa", 0)
	exactly := f.checkpoint(30*day, "bbbbbb", 0)
	older := f.checkpoint(31*day, "cccccc", 0)

	rep, err := f.eng.ExpireUnused(context.Background(), ExpireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !exists(t, filepath.Join(f.dep.BackupDir, justYoung)) {
		t.Fatal("a checkpoint one minute short of 30 days expired")
	}
	for _, id := range []string{exactly, older} {
		if exists(t, filepath.Join(f.dep.BackupDir, id)) {
			t.Fatalf("checkpoint %s is at least 30 days old and survived", id)
		}
		wantOutcome(t, rep, ScopeCheckpoints, id, ExpiryExpired)
	}
	if _, listed := findItem(rep, ScopeCheckpoints, justYoung); listed {
		t.Fatal("a young checkpoint must not appear in the report")
	}

	f.now = f.now.Add(time.Minute)
	if _, err := f.eng.ExpireUnused(context.Background(), ExpireOptions{}); err != nil {
		t.Fatal(err)
	}
	if exists(t, filepath.Join(f.dep.BackupDir, justYoung)) {
		t.Fatal("the checkpoint did not expire once the clock reached 30 days")
	}
}

func TestExpiryAgeSettings(t *testing.T) {
	zero, ten := 0, 10
	cases := []struct {
		name      string
		configure func(f *expiryFixture) *int
		expired   bool
	}{
		{"absent setting uses 30 days", func(f *expiryFixture) *int { f.dep.ExpireAfterDays = nil; return nil }, true},
		{"configured zero disables", func(f *expiryFixture) *int { f.dep.ExpireAfterDays = &zero; return nil }, false},
		{"explicit zero for one run disables", func(f *expiryFixture) *int { return &zero }, false},
		{"explicit age for one run overrides the configuration", func(f *expiryFixture) *int { f.dep.ExpireAfterDays = &zero; return &ten }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newExpiryFixture(t)
			old := f.checkpoint(40*day, "aaaaaa", 0)
			after := tc.configure(f)
			rep, err := f.eng.ExpireUnused(context.Background(), ExpireOptions{AfterDays: after})
			if err != nil {
				t.Fatal(err)
			}
			if got := !exists(t, filepath.Join(f.dep.BackupDir, old)); got != tc.expired {
				t.Fatalf("expired = %v, want %v (report %+v)", got, tc.expired, rep)
			}
			if !tc.expired {
				if !rep.Disabled || len(rep.Items) != 0 || len(f.dk.log) != 0 {
					t.Fatalf("a disabled run must inspect nothing: %+v, docker %v", rep, f.dk.log)
				}
				if exists(t, f.dep.expiryLastFile()) {
					t.Fatal("a disabled run must not record a report")
				}
			}
		})
	}
}

func TestExpiryNegativeAgeIsRejected(t *testing.T) {
	f := newExpiryFixture(t)
	f.checkpoint(40*day, "aaaaaa", 0)
	neg := -1
	if _, err := f.eng.ExpireUnused(context.Background(), ExpireOptions{AfterDays: &neg}); err == nil {
		t.Fatal("a negative age was accepted")
	}
}

func TestExpiryRefusesMissingCurrentGenerationJournal(t *testing.T) {
	for _, mode := range []string{"preview", "cleanup", "automatic"} {
		t.Run(mode, func(t *testing.T) {
			f := newExpiryFixture(t)
			paired := f.checkpoint(60*day, "aaaaaa", 2)
			producer := uid("333333")
			f.current(3, 60*day, producer, map[string]string{"profile": "live-profile"})
			f.journal(producer, JournalCompleted, 60*day, func(j *Journal) {
				j.CheckpointID = paired
				j.Intent = &State{Generation: 3}
			})
			if err := os.RemoveAll(filepath.Join(f.dep.updatesDir(), producer)); err != nil {
				t.Fatal(err)
			}
			var err error
			if mode == "automatic" {
				_, _, err = f.eng.AutoExpire(context.Background())
			} else {
				_, err = f.eng.ExpireUnused(context.Background(), ExpireOptions{DryRun: mode == "preview"})
			}
			if err == nil {
				t.Error("missing current-generation journal must refuse expiry")
			}
			if !exists(t, filepath.Join(f.dep.BackupDir, paired)) {
				t.Error("expiry removed the paired rollback checkpoint")
			}
			if len(f.dk.log) != 0 {
				t.Error("uncertain recovery references must stop before Docker inventory")
			}
		})
	}
}

// runningProducerFixture builds running generation 3 with an aged paired
// checkpoint and the completed producer journal its state names.
func runningProducerFixture(t *testing.T) (f *expiryFixture, paired string) {
	t.Helper()
	f = newExpiryFixture(t)
	paired = f.checkpoint(60*day, "aaaaaa", 2)
	producer := uid("333333")
	f.current(3, 60*day, producer, map[string]string{"profile": "live-profile"})
	f.journal(producer, JournalCompleted, 60*day, func(j *Journal) {
		j.CheckpointID = paired
		j.Intent = &State{Generation: 3}
	})
	return f, paired
}

func TestExpiryAndPruneRefuseBrokenCurrentGenerationLinkage(t *testing.T) {
	cases := []struct {
		name    string
		corrupt func(t *testing.T, f *expiryFixture, producer string)
	}{
		{"updates directory is gone", func(t *testing.T, f *expiryFixture, _ string) {
			if err := os.RemoveAll(f.dep.updatesDir()); err != nil {
				t.Fatal(err)
			}
		}},
		{"producer journal is gone while other journals remain", func(t *testing.T, f *expiryFixture, producer string) {
			f.journal(uid("111111"), JournalCompleted, 90*day, nil)
			if err := os.RemoveAll(filepath.Join(f.dep.updatesDir(), producer)); err != nil {
				t.Fatal(err)
			}
		}},
		{"state names a malformed operation id", func(t *testing.T, f *expiryFixture, _ string) {
			f.current(3, 60*day, "not-an-update-id", nil)
		}},
		{"producer journal records no checkpoint", func(t *testing.T, f *expiryFixture, producer string) {
			rewriteJournal(t, f, producer, func(j *Journal) { j.CheckpointID = "" })
		}},
		{"producer journal records a malformed checkpoint id", func(t *testing.T, f *expiryFixture, producer string) {
			rewriteJournal(t, f, producer, func(j *Journal) { j.CheckpointID = "../" + j.CheckpointID })
		}},
		{"producer switched to another generation", func(t *testing.T, f *expiryFixture, producer string) {
			rewriteJournal(t, f, producer, func(j *Journal) { j.Intent = &State{Generation: 2} })
		}},
		{"producer journal records no switch intent", func(t *testing.T, f *expiryFixture, producer string) {
			rewriteJournal(t, f, producer, func(j *Journal) { j.Intent = nil })
		}},
	}
	runs := []string{"preview", "cleanup", "automatic", "count prune"}
	for _, tc := range cases {
		for _, mode := range runs {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				f, paired := runningProducerFixture(t)
				f.dep.RetainCheckpoints = 1
				newest := f.checkpoint(time.Hour, "bbbbbb", 0)
				tc.corrupt(t, f, uid("333333"))
				var err error
				switch mode {
				case "automatic":
					_, _, err = f.eng.AutoExpire(context.Background())
				case "count prune":
					_, err = f.eng.PruneCheckpoints()
				default:
					_, err = f.eng.ExpireUnused(context.Background(), ExpireOptions{DryRun: mode == "preview"})
				}
				if err == nil {
					t.Error("uncertain current-generation linkage must refuse the run")
				}
				for _, id := range []string{paired, newest} {
					if !exists(t, filepath.Join(f.dep.BackupDir, id)) {
						t.Errorf("checkpoint %s was removed despite uncertain linkage", id)
					}
				}
				if len(f.dk.log) != 0 {
					t.Error("uncertain recovery references must stop before Docker inventory")
				}
			})
		}
	}
}

// rewriteJournal edits an already written journal in place.
func rewriteJournal(t *testing.T, f *expiryFixture, id string, mod func(j *Journal)) {
	t.Helper()
	j, err := loadJournal(f.dep, id, f.clock)
	if err != nil {
		t.Fatal(err)
	}
	mod(j)
	if err := j.save(); err != nil {
		t.Fatal(err)
	}
}

func TestExpiryAcceptsLegitimateCurrentGenerationStates(t *testing.T) {
	t.Run("pristine generation needs no producer", func(t *testing.T) {
		f := newExpiryFixture(t)
		old := f.checkpoint(90*day, "aaaaaa", 0)
		f.current(1, 90*day, "", nil)
		if _, err := f.eng.ExpireUnused(context.Background(), ExpireOptions{}); err != nil {
			t.Fatal(err)
		}
		if exists(t, filepath.Join(f.dep.BackupDir, old)) {
			t.Fatal("an unreferenced old checkpoint survived a pristine generation")
		}
	})

	t.Run("aborted update leaves the earlier producer paired", func(t *testing.T) {
		f := newExpiryFixture(t)
		paired := f.checkpoint(60*day, "aaaaaa", 2)
		abortedCP := f.checkpoint(50*day, "bbbbbb", 3)
		f.current(3, 60*day, uid("333333"), nil)
		f.journal(uid("333333"), JournalCompleted, 60*day, func(j *Journal) {
			j.CheckpointID = paired
			j.Intent = &State{Generation: 3}
		})
		f.journal(uid("444444"), JournalAborted, 50*day, func(j *Journal) {
			j.CheckpointID = abortedCP
			j.Intent = &State{Generation: 4}
		})
		if _, err := f.eng.ExpireUnused(context.Background(), ExpireOptions{}); err != nil {
			t.Fatal(err)
		}
		if !exists(t, filepath.Join(f.dep.BackupDir, paired)) {
			t.Fatal("the checkpoint paired with the running generation expired")
		}
		if exists(t, filepath.Join(f.dep.BackupDir, abortedCP)) {
			t.Fatal("the checkpoint of an aborted update survived")
		}
	})

	t.Run("rollback producer keeps the checkpoint it restored from", func(t *testing.T) {
		f := newExpiryFixture(t)
		restored := f.checkpoint(60*day, "aaaaaa", 2)
		rollback := "r-20260101T000000Z-777777"
		f.current(4, 60*day, rollback, nil)
		f.journal(rollback, JournalCompleted, 60*day, func(j *Journal) {
			j.Kind = "rollback"
			j.CheckpointID = restored
			j.Intent = &State{Generation: 4}
		})
		if _, err := f.eng.ExpireUnused(context.Background(), ExpireOptions{}); err != nil {
			t.Fatal(err)
		}
		if !exists(t, filepath.Join(f.dep.BackupDir, restored)) {
			t.Fatal("the checkpoint a rollback restored from expired")
		}
	})
}

func TestPruneCheckpointsKeepsThePairedCheckpointBeyondTheCount(t *testing.T) {
	f, paired := runningProducerFixture(t)
	f.dep.RetainCheckpoints = 1
	newest := f.checkpoint(time.Hour, "bbbbbb", 0)
	orphan := f.checkpoint(90*day, "cccccc", 0)
	removed, err := f.eng.PruneCheckpoints()
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != orphan {
		t.Fatalf("removed = %v, want only %s", removed, orphan)
	}
	for _, id := range []string{paired, newest} {
		if !exists(t, filepath.Join(f.dep.BackupDir, id)) {
			t.Fatalf("checkpoint %s was removed", id)
		}
	}
}

func TestExpiryKeepsEverythingTheRunningGenerationNeeds(t *testing.T) {
	f := newExpiryFixture(t)
	paired := f.checkpoint(60*day, "aaaaaa", 2)
	held := f.checkpoint(200*day, "bbbbbb", 0)
	aborted := f.checkpoint(200*day, "cccccc", 0)
	orphan := f.checkpoint(90*day, "dddddd", 0)
	young := f.checkpoint(5*day, "eeeeee", 0)

	f.generation(1, 120*day)
	f.generation(2, 90*day)
	f.current(3, 60*day, uid("333333"), nil)

	f.journal(uid("333333"), JournalCompleted, 60*day, func(j *Journal) {
		j.CheckpointID = paired
		j.Intent = &State{Generation: 3}
	})
	f.journal(uid("444444"), JournalHeld, 200*day, func(j *Journal) { j.CheckpointID = held })
	f.journal(uid("555555"), JournalAborted, 200*day, func(j *Journal) { j.CheckpointID = aborted })

	rep, err := f.eng.ExpireUnused(context.Background(), ExpireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{paired, held, young} {
		if !exists(t, filepath.Join(f.dep.BackupDir, id)) {
			t.Fatalf("checkpoint %s was removed", id)
		}
	}
	for _, id := range []string{aborted, orphan} {
		if exists(t, filepath.Join(f.dep.BackupDir, id)) {
			t.Fatalf("unreferenced checkpoint %s survived", id)
		}
	}
	wantOutcome(t, rep, ScopeCheckpoints, paired, ExpiryKept)
	wantOutcome(t, rep, ScopeCheckpoints, held, ExpiryKept)

	// Generation 2 is the previous generation of the paired checkpoint and 3 is
	// running; only generation 1 is obsolete.
	if !exists(t, f.dep.generationDir(2)) || !exists(t, f.dep.generationDir(3)) || !exists(t, f.dep.stateFile()) {
		t.Fatal("a protected or running generation record was removed")
	}
	if exists(t, f.dep.generationDir(1)) {
		t.Fatal("the obsolete generation record survived")
	}
	wantOutcome(t, rep, ScopeGenerations, "1", ExpiryExpired)
	wantOutcome(t, rep, ScopeGenerations, "2", ExpiryKept)
}

func TestExpiryRemovesOnlyOwnedUnreferencedVolumes(t *testing.T) {
	f := newExpiryFixture(t)
	f.current(2, 5*day, uid("222222"), map[string]string{"profile": "vol-current"})
	f.journal(uid("111111"), JournalCompleted, 50*day, func(j *Journal) {
		j.RetiredVolumes = []string{"vol-retired-old", "vol-retired-busy", "vol-baseline"}
	})
	pairedCP := f.checkpoint(5*day, "aaaaaa", 1)
	f.journal(uid("222222"), JournalCompleted, 5*day, func(j *Journal) {
		j.CheckpointID = pairedCP
		j.Intent = &State{Generation: 2}
		j.RetiredVolumes = []string{"vol-retired-young"}
	})
	f.journal(uid("333333"), JournalReverted, 40*day, func(j *Journal) {
		j.Candidate = &CandidateRecord{Volumes: map[string]string{"profile": "vol-failed"}, Scratch: []string{"vol-scratch"}}
	})
	f.journal(uid("444444"), JournalRunning, -1, func(j *Journal) {
		j.Candidate = &CandidateRecord{Volumes: map[string]string{"profile": "vol-inflight"}}
	})

	f.volume("vol-current", uid("222222"), 60*day)
	f.volume("vol-retired-old", uid("111111"), 90*day)
	f.volume("vol-retired-busy", uid("111111"), 90*day)
	f.volume("vol-retired-young", uid("111111"), 90*day)
	f.volume("vol-failed", uid("333333"), 45*day)
	f.volume("vol-scratch", uid("333333"), 45*day)
	f.volume("vol-inflight", uid("444444"), 45*day)
	f.volume("vol-norecord", uid("111111"), 90*day)
	f.volume("vol-badlabel", uid("333333"), 45*day)
	f.dk.volumes["vol-badlabel"][LabelCreated] = "last tuesday"
	// Another deployment's volume on the same engine, and an unmanaged one.
	f.volume("vol-foreign", "u-20250101T000000Z-ffffff", 400*day)
	f.dk.users["vol-retired-busy"] = []string{"c1"}

	rep, err := f.eng.ExpireUnused(context.Background(), ExpireOptions{Scopes: []string{ScopeVolumes}})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(f.dk.removed)
	if got, want := strings.Join(f.dk.removed, ","), "vol-failed,vol-retired-old,vol-scratch"; got != want {
		t.Fatalf("removed volumes = %s, want %s", got, want)
	}
	for _, id := range []string{"vol-current", "vol-inflight", "vol-retired-busy", "vol-norecord", "vol-badlabel"} {
		wantOutcome(t, rep, ScopeVolumes, id, ExpiryKept)
	}
	if it, _ := findItem(rep, ScopeVolumes, "vol-retired-busy"); !strings.Contains(it.Reason, "in use") {
		t.Fatalf("busy volume reason = %q", it.Reason)
	}
	for _, id := range []string{"vol-foreign", "vol-baseline", "vol-retired-young"} {
		if _, listed := findItem(rep, ScopeVolumes, id); listed {
			t.Fatalf("%s is not an old owned volume and must not be reported", id)
		}
	}
	for _, cmd := range f.dk.log {
		if strings.Contains(cmd, "prune") || strings.HasPrefix(cmd, "image") {
			t.Fatalf("expiry issued a global or image command: %s", cmd)
		}
	}
}

func TestExpiryPreviewChangesNothing(t *testing.T) {
	f := newExpiryFixture(t)
	cp := f.checkpoint(40*day, "aaaaaa", 0)
	f.journal(uid("333333"), JournalReverted, 40*day, func(j *Journal) {
		j.Candidate = &CandidateRecord{Scratch: []string{"vol-scratch"}}
	})
	f.volume("vol-scratch", uid("333333"), 45*day)

	rep, err := f.eng.ExpireUnused(context.Background(), ExpireOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.DryRun || rep.WouldExpire != 2 || rep.Expired != 0 {
		t.Fatalf("preview report = %+v", rep)
	}
	wantOutcome(t, rep, ScopeCheckpoints, cp, ExpiryWouldExpire)
	wantOutcome(t, rep, ScopeVolumes, "vol-scratch", ExpiryWouldExpire)
	if !exists(t, filepath.Join(f.dep.BackupDir, cp)) || len(f.dk.removed) != 0 {
		t.Fatal("a preview removed data")
	}
	if exists(t, f.dep.expiryLastFile()) {
		t.Fatal("a preview recorded a real run")
	}
}

func TestExpiryScopeSelection(t *testing.T) {
	f := newExpiryFixture(t)
	cp := f.checkpoint(40*day, "aaaaaa", 0)
	f.journal(uid("333333"), JournalReverted, 40*day, func(j *Journal) {
		j.Candidate = &CandidateRecord{Scratch: []string{"vol-scratch"}}
	})
	f.volume("vol-scratch", uid("333333"), 45*day)

	if _, err := f.eng.ExpireUnused(context.Background(), ExpireOptions{Scopes: []string{ScopeVolumes}}); err != nil {
		t.Fatal(err)
	}
	if !exists(t, filepath.Join(f.dep.BackupDir, cp)) || len(f.dk.removed) != 1 {
		t.Fatalf("a volumes-only run touched another scope (removed %v)", f.dk.removed)
	}
	if _, err := f.eng.ExpireUnused(context.Background(), ExpireOptions{Scopes: []string{"images"}}); err == nil {
		t.Fatal("an unknown scope was accepted")
	}
}

func TestExpiryTreatsMalformedMetadataAsUncertain(t *testing.T) {
	t.Run("one unreadable checkpoint is kept and the rest proceeds", func(t *testing.T) {
		f := newExpiryFixture(t)
		bad := "cp-" + stamp(f.now.Add(-90*day)) + "-badbad"
		if err := os.MkdirAll(filepath.Join(f.dep.BackupDir, bad), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(f.dep.BackupDir, bad, checkpointFileName), []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		old := f.checkpoint(90*day, "aaaaaa", 0)
		rep, err := f.eng.ExpireUnused(context.Background(), ExpireOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if !exists(t, filepath.Join(f.dep.BackupDir, bad)) {
			t.Fatal("a checkpoint with unreadable metadata was removed")
		}
		wantOutcome(t, rep, ScopeCheckpoints, bad, ExpiryKept)
		wantOutcome(t, rep, ScopeCheckpoints, old, ExpiryExpired)
	})

	t.Run("an unreadable journal stops every scope", func(t *testing.T) {
		f := newExpiryFixture(t)
		old := f.checkpoint(90*day, "aaaaaa", 0)
		dir := filepath.Join(f.dep.updatesDir(), uid("999999"))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "journal.json"), []byte("garbage"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := f.eng.ExpireUnused(context.Background(), ExpireOptions{}); err == nil {
			t.Fatal("expiry ran with an unreadable journal")
		}
		if !exists(t, filepath.Join(f.dep.BackupDir, old)) || len(f.dk.log) != 0 {
			t.Fatal("expiry changed or queried something despite uncertain inventory")
		}
	})

	t.Run("an unreadable state stops every scope", func(t *testing.T) {
		f := newExpiryFixture(t)
		old := f.checkpoint(90*day, "aaaaaa", 0)
		if err := os.WriteFile(f.dep.stateFile(), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := f.eng.ExpireUnused(context.Background(), ExpireOptions{}); err == nil {
			t.Fatal("expiry ran with an unreadable state")
		}
		if !exists(t, filepath.Join(f.dep.BackupDir, old)) {
			t.Fatal("a checkpoint was removed despite uncertain state")
		}
	})

	t.Run("a missing state with completed operations stops every scope", func(t *testing.T) {
		f := newExpiryFixture(t)
		old := f.checkpoint(90*day, "aaaaaa", 0)
		f.journal(uid("111111"), JournalCompleted, 90*day, func(j *Journal) { j.CheckpointID = old })
		if _, err := f.eng.ExpireUnused(context.Background(), ExpireOptions{}); err == nil {
			t.Fatal("expiry ran although the running generation is unknown")
		}
		if !exists(t, filepath.Join(f.dep.BackupDir, old)) {
			t.Fatal("the possibly paired checkpoint was removed")
		}
	})

	t.Run("a corrupt generation record is kept", func(t *testing.T) {
		f := newExpiryFixture(t)
		f.generation(1, 120*day)
		f.generation(2, 100*day)
		f.current(3, 60*day, "", nil)
		if err := os.WriteFile(filepath.Join(f.dep.generationDir(1), "state.json"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		rep, err := f.eng.ExpireUnused(context.Background(), ExpireOptions{Scopes: []string{ScopeGenerations}})
		if err != nil {
			t.Fatal(err)
		}
		if !exists(t, f.dep.generationDir(1)) {
			t.Fatal("a generation record with invalid content was removed")
		}
		wantOutcome(t, rep, ScopeGenerations, "1", ExpiryKept)
		wantOutcome(t, rep, ScopeGenerations, "2", ExpiryExpired)
	})
}

func TestExpiryOnlyRemovesAbandonedStaging(t *testing.T) {
	f := newExpiryFixture(t)
	oldStage := ".incomplete-cp-" + stamp(f.now.Add(-45*day)) + "-aaaaaa"
	youngStage := ".incomplete-cp-" + stamp(f.now.Add(-time.Hour)) + "-bbbbbb"
	heldID := "cp-" + stamp(f.now.Add(-45*day)) + "-cccccc"
	heldStage := incompletePrefix + heldID
	for _, d := range []string{oldStage, youngStage, heldStage} {
		if err := os.MkdirAll(filepath.Join(f.dep.BackupDir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	f.journal(uid("444444"), JournalRunning, -1, func(j *Journal) { j.CheckpointID = heldID; j.PlanID = "p-" + stamp(f.now.Add(-45*day)) + "-dddddd" })

	if err := os.MkdirAll(f.dep.plansDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	oldPlan := "p-" + stamp(f.now.Add(-60*day)) + "-eeeeee.json"
	usedPlan := "p-" + stamp(f.now.Add(-45*day)) + "-dddddd.json"
	for _, p := range []string{oldPlan, usedPlan} {
		if err := os.WriteFile(filepath.Join(f.dep.plansDir(), p), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := f.eng.ExpireUnused(context.Background(), ExpireOptions{Scopes: []string{ScopeStaging}}); err != nil {
		t.Fatal(err)
	}
	gone := []string{filepath.Join(f.dep.BackupDir, oldStage), filepath.Join(f.dep.plansDir(), oldPlan)}
	kept := []string{filepath.Join(f.dep.BackupDir, youngStage), filepath.Join(f.dep.BackupDir, heldStage), filepath.Join(f.dep.plansDir(), usedPlan)}
	for _, p := range gone {
		if exists(t, p) {
			t.Fatalf("abandoned staging %s survived", p)
		}
	}
	for _, p := range kept {
		if !exists(t, p) {
			t.Fatalf("staging %s of live or in-flight work was removed", p)
		}
	}
}

func TestExpiryReportsRemovalFailuresAndKeepsGoing(t *testing.T) {
	f := newExpiryFixture(t)
	f.journal(uid("333333"), JournalReverted, 40*day, func(j *Journal) {
		j.Candidate = &CandidateRecord{Scratch: []string{"vol-a", "vol-b"}}
	})
	f.volume("vol-a", uid("333333"), 45*day)
	f.volume("vol-b", uid("333333"), 45*day)
	f.dk.rmErr["vol-a"] = errors.New("docker: device busy")

	rep, err := f.eng.ExpireUnused(context.Background(), ExpireOptions{})
	if err == nil || !strings.Contains(err.Error(), "vol-a") {
		t.Fatalf("a failed removal must be an error naming the volume, got %v", err)
	}
	if rep.Failed != 1 || rep.Expired != 1 {
		t.Fatalf("report = %+v", rep)
	}
	wantOutcome(t, rep, ScopeVolumes, "vol-a", ExpiryFailed)
	wantOutcome(t, rep, ScopeVolumes, "vol-b", ExpiryExpired)
	if it, _ := findItem(rep, ScopeVolumes, "vol-a"); !strings.Contains(it.Error, "device busy") {
		t.Fatalf("the failure reason is lost: %+v", it)
	}
	last, ok, lerr := f.eng.LastExpiry()
	if lerr != nil || !ok || last.Failed != 1 {
		t.Fatalf("the failed run was not recorded: %+v %v %v", last, ok, lerr)
	}
}

func TestExpiryScopeFailureDoesNotBlockOtherScopes(t *testing.T) {
	f := newExpiryFixture(t)
	old := f.checkpoint(40*day, "aaaaaa", 0)
	f.journal(uid("333333"), JournalReverted, 40*day, func(j *Journal) {
		j.Candidate = &CandidateRecord{Scratch: []string{"vol-a"}}
	})
	f.volume("vol-a", uid("333333"), 45*day)
	broken := &brokenDocker{}
	f.eng.Docker = Docker{Run: broken}

	rep, err := f.eng.ExpireUnused(context.Background(), ExpireOptions{})
	if err == nil || rep.ScopeErrors[ScopeVolumes] == "" {
		t.Fatalf("an unreadable engine inventory must be reported: %v %+v", err, rep)
	}
	if exists(t, filepath.Join(f.dep.BackupDir, old)) {
		t.Fatal("the independent checkpoint scope did not run")
	}
}

type brokenDocker struct{}

func (brokenDocker) Run(context.Context, Cmd) error { return errors.New("docker daemon unreachable") }

func TestAutoExpireIsThrottledAndRetriesFailures(t *testing.T) {
	f := newExpiryFixture(t)
	f.journal(uid("333333"), JournalReverted, 40*day, func(j *Journal) {
		j.Candidate = &CandidateRecord{Scratch: []string{"vol-a"}}
	})
	f.volume("vol-a", uid("333333"), 45*day)
	f.dk.rmErr["vol-a"] = errors.New("docker: device busy")

	rep, ran, err := f.eng.AutoExpire(context.Background())
	if !ran || err == nil || rep.Failed != 1 || rep.Trigger != TriggerAutomatic {
		t.Fatalf("first automatic run: ran=%v err=%v %+v", ran, err, rep)
	}
	// A failed run is retried on the next lifecycle activity.
	delete(f.dk.rmErr, "vol-a")
	f.now = f.now.Add(time.Minute)
	rep, ran, err = f.eng.AutoExpire(context.Background())
	if !ran || err != nil || rep.Expired != 1 {
		t.Fatalf("retry: ran=%v err=%v %+v", ran, err, rep)
	}
	// A clean run is not repeated within the interval...
	f.now = f.now.Add(AutoExpireInterval - time.Hour)
	if _, ran, err := f.eng.AutoExpire(context.Background()); ran || err != nil {
		t.Fatalf("throttled run executed: ran=%v err=%v", ran, err)
	}
	// ...but is after it.
	f.now = f.now.Add(2 * time.Hour)
	if _, ran, err := f.eng.AutoExpire(context.Background()); !ran || err != nil {
		t.Fatalf("run after the interval: ran=%v err=%v", ran, err)
	}
}

func TestAutoExpireDoesNothingWhenDisabledOrBusy(t *testing.T) {
	f := newExpiryFixture(t)
	old := f.checkpoint(90*day, "aaaaaa", 0)

	unlock, err := lockFile(f.dep.lockPath())
	if err != nil {
		t.Fatal(err)
	}
	if _, ran, err := f.eng.AutoExpire(context.Background()); ran || err != nil {
		t.Fatalf("an automatic run must yield silently to a running operation: ran=%v err=%v", ran, err)
	}
	unlock()
	if !exists(t, filepath.Join(f.dep.BackupDir, old)) {
		t.Fatal("a checkpoint was removed while an operation held the lock")
	}

	f.dep.ExpireAfterDays = new(0)
	if _, ran, err := f.eng.AutoExpire(context.Background()); ran || err != nil {
		t.Fatalf("a disabled policy ran: ran=%v err=%v", ran, err)
	}
	if !exists(t, filepath.Join(f.dep.BackupDir, old)) {
		t.Fatal("a checkpoint expired although age expiry is disabled")
	}
}

func TestExpireAfterSettingSurvivesTheDeploymentFile(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name string
		set  *int
		want int
	}{
		{"absent is the default", nil, DefaultExpireAfterDays},
		{"explicit zero stays disabled", new(0), 0},
		{"explicit age is kept", new(7), 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &Deployment{File: filepath.Join(dir, "deployment.json"), StateDir: dir, ExpireAfterDays: tc.set}
			d.applyDefaults()
			raw, err := json.Marshal(d)
			if err != nil {
				t.Fatal(err)
			}
			var back Deployment
			if err := json.Unmarshal(raw, &back); err != nil {
				t.Fatal(err)
			}
			if back.ExpireAfter() != tc.want {
				t.Fatalf("round-tripped age = %d, want %d (%s)", back.ExpireAfter(), tc.want, raw)
			}
		})
	}
}

func TestVolumeOwnershipIsPerDeployment(t *testing.T) {
	f := newExpiryFixture(t)
	f.journal(uid("111111"), JournalCompleted, 10*day, func(j *Journal) {
		j.RetiredVolumes = []string{"vol-retired"}
		j.Candidate = &CandidateRecord{Volumes: map[string]string{"profile": "vol-cand"}, Scratch: []string{"vol-scratch"}}
	})
	js, errs := listJournals(f.dep, f.clock)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	managed := func(update string) map[string]string {
		return map[string]string{LabelManaged: "true", LabelUpdate: update}
	}
	for name, labels := range map[string]map[string]string{
		"vol-labelled": managed(uid("111111")),
		"vol-retired":  managed("u-20250101T000000Z-ffffff"),
		"vol-cand":     nil,
		"vol-scratch":  nil,
	} {
		if !volumeOwned(name, labels, js) {
			t.Fatalf("%s is created or named by this deployment's journal", name)
		}
	}
	if volumeOwned("vol-other", managed("u-20250101T000000Z-ffffff"), js) {
		t.Fatal("a managed volume of another deployment counted as owned")
	}
}

func branchNames(t *testing.T, f *expiryFixture) map[string]Branch {
	t.Helper()
	branches, err := f.eng.Branches(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]Branch{}
	for _, b := range branches {
		out[b.Volume] = b
	}
	return out
}

// otherDeployment is the update id label of a volume another deployment on
// the same engine created; no journal of the fixture knows it.
const otherDeployment = "u-20250101T000000Z-ffffff"

func branchFixture(t *testing.T) *expiryFixture {
	t.Helper()
	f := newExpiryFixture(t)
	f.current(2, 5*day, uid("222222"), map[string]string{"profile": "vol-current"})
	f.journal(uid("111111"), JournalCompleted, 50*day, func(j *Journal) { j.RetiredVolumes = []string{"vol-retired"} })
	f.journal(uid("222222"), JournalCompleted, 5*day, nil)
	f.journal(uid("333333"), JournalReverted, 40*day, func(j *Journal) {
		j.Candidate = &CandidateRecord{Scratch: []string{"vol-scratch"}}
	})
	f.volume("vol-current", uid("222222"), 60*day)
	f.volume("vol-retired", uid("111111"), 90*day)
	f.volume("vol-scratch", uid("333333"), 45*day)
	f.volume("vol-foreign", otherDeployment, 45*day)
	f.volume("vol-foreign-current", otherDeployment, 5*day)
	return f
}

func TestBranchesListOnlyThisDeploymentsVolumes(t *testing.T) {
	f := branchFixture(t)
	got := branchNames(t, f)
	for _, name := range []string{"vol-current", "vol-retired", "vol-scratch"} {
		if _, ok := got[name]; !ok {
			t.Fatalf("%s belongs to this deployment and is missing from %v", name, got)
		}
	}
	for _, name := range []string{"vol-foreign", "vol-foreign-current"} {
		if _, ok := got[name]; ok {
			t.Fatalf("%s belongs to another deployment on the same engine and must not be listed", name)
		}
	}
	if !got["vol-current"].InUse || got["vol-retired"].InUse {
		t.Fatalf("in-use flags are wrong: %+v", got)
	}
}

func TestDeleteBranchRefusesAnotherDeploymentsVolume(t *testing.T) {
	f := branchFixture(t)
	ctx := context.Background()
	for _, name := range []string{"vol-foreign", "vol-foreign-current"} {
		if err := f.eng.DeleteBranch(ctx, name); err == nil {
			t.Fatalf("DeleteBranch removed %s of another deployment", name)
		}
	}
	if err := f.eng.DeleteBranch(ctx, "vol-current"); err == nil {
		t.Fatal("DeleteBranch removed a volume of the running generation")
	}
	if len(f.dk.removed) != 0 {
		t.Fatalf("refused deletions still removed %v", f.dk.removed)
	}
	for _, name := range []string{"vol-retired", "vol-scratch"} {
		if err := f.eng.DeleteBranch(ctx, name); err != nil {
			t.Fatalf("own branch %s: %v", name, err)
		}
	}
	sort.Strings(f.dk.removed)
	if got := strings.Join(f.dk.removed, ","); got != "vol-retired,vol-scratch" {
		t.Fatalf("removed %s", got)
	}
}

func TestSetExpireAfterDaysRereadsUnderTheDeploymentLock(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "deployment.json")
	if err := ScaffoldM0(file, dir, filepath.Join(dir, "compose.yaml"), filepath.Join(dir, "bin")).Save(); err != nil {
		t.Fatal(err)
	}
	stale, err := LoadDeployment(file)
	if err != nil {
		t.Fatal(err)
	}
	// Another writer changes a different setting after this one loaded.
	other, err := LoadDeployment(file)
	if err != nil {
		t.Fatal(err)
	}
	other.RetainCheckpoints = 7
	if err := other.Save(); err != nil {
		t.Fatal(err)
	}

	eng := &Engine{Dep: stale, Out: io.Discard, Now: time.Now}
	if err := eng.SetExpireAfterDays(9); err != nil {
		t.Fatal(err)
	}
	got, err := LoadDeployment(file)
	if err != nil || got.RetainCheckpoints != 7 || got.ExpireAfter() != 9 {
		t.Fatalf("the concurrent edit was overwritten or the setting lost: retain=%d expire=%d err=%v", got.RetainCheckpoints, got.ExpireAfter(), err)
	}
	if stale.ExpireAfter() != 9 {
		t.Fatal("the engine's own deployment did not take the new setting")
	}

	if err := eng.SetExpireAfterDays(0); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadDeployment(file); err != nil || got.ExpireAfter() != 0 {
		t.Fatalf("an explicit zero must persist as disabled: %d %v", got.ExpireAfter(), err)
	}

	unlock, err := lockFile(stale.lockPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.SetExpireAfterDays(3); !errors.Is(err, ErrOperationInProgress) {
		t.Fatalf("a write beside a running update must be refused, got %v", err)
	}
	unlock()
	if got, _ := LoadDeployment(file); got.ExpireAfter() != 0 {
		t.Fatal("a refused write changed the file")
	}
	if err := eng.SetExpireAfterDays(MaxExpireAfterDays + 1); err == nil {
		t.Fatal("an out-of-range age was accepted")
	}
}
