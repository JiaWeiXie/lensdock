package update

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var (
	retRuntimeOld = testDigest('1')
	retRuntimeNew = testDigest('2')
	// retGatewayOld shares its first twelve digits with retGatewayClash, the
	// only part earlier versions put into their keep tag.
	retGatewayOld   = "sha256:0383c4d06b62" + strings.Repeat("a", 52)
	retGatewayClash = "sha256:0383c4d06b62" + strings.Repeat("b", 52)
	retGatewayNew   = testDigest('4')
)

func retRef(repo, digest string) string { return repo + "@" + digest }

func retKeep(repo, digest string) string {
	return repo + ":lensdock-keep-" + strings.TrimPrefix(digest, "sha256:")
}

// retentionEngine is a Docker runner stub that models how an engine resolves
// image names: repository@digest resolves only while some tag of that same
// repository points at the digest (a tag of another repository does not
// help), exactly what rebuilding :dev breaks. It records every command.
type retentionEngine struct {
	names map[string]string // repository:tag -> digest
	arch  map[string]string // digest -> architecture (default amd64)
	force map[string]ImageInfo
	// bareDigest makes sha256:<digest> resolve to a stored image, as engines
	// with the containerd image store do.
	bareDigest bool
	log        []string
}

func newRetentionEngine() *retentionEngine {
	return &retentionEngine{
		names: map[string]string{
			// the release images
			"lensdock/runtime:dev": retRuntimeNew,
			"lensdock/gateway:dev": retGatewayNew,
			// the images the running services use, under their own names
			"lensdock/runtime:previous": retRuntimeOld,
			"lensdock/gateway:previous": retGatewayOld,
		},
		arch:  map[string]string{},
		force: map[string]ImageInfo{},
	}
}

func (f *retentionEngine) info(digest string) ImageInfo {
	arch := f.arch[digest]
	if arch == "" {
		arch = "amd64"
	}
	return ImageInfo{ID: digest, OS: "linux", Arch: arch}
}

func (f *retentionEngine) resolve(ref string) (ImageInfo, bool) {
	if info, ok := f.force[ref]; ok {
		return info, true
	}
	switch {
	case strings.Contains(ref, "@"):
		repo, digest, _ := strings.Cut(ref, "@")
		for name, d := range f.names {
			if d == digest && repoOf(name) == repo {
				return f.info(d), true
			}
		}
	case strings.HasPrefix(ref, "sha256:"):
		if f.bareDigest {
			for _, d := range f.names {
				if d == ref {
					return f.info(d), true
				}
			}
		}
	default:
		if d, ok := f.names[ref]; ok {
			return f.info(d), true
		}
	}
	return ImageInfo{}, false
}

func (f *retentionEngine) Run(_ context.Context, c Cmd) error {
	f.log = append(f.log, strings.Join(c.Args, " "))
	a := c.Args
	missing := func(ref string) error {
		return &ExitError{Argv: a, Code: 1, Stderr: "Error response from daemon: No such image: " + ref}
	}
	switch {
	case len(a) >= 3 && a[0] == "image" && a[1] == "inspect":
		ref := a[len(a)-1]
		info, ok := f.resolve(ref)
		if !ok {
			return missing(ref)
		}
		data, err := json.Marshal(info)
		if err != nil {
			return err
		}
		_, err = c.Stdout.Write(data)
		return err
	case len(a) == 4 && a[0] == "image" && a[1] == "tag":
		info, ok := f.resolve(a[2])
		if !ok {
			return missing(a[2])
		}
		f.names[a[3]] = info.ID
		return nil
	}
	return nil
}

// first returns the index of the first command containing sub, or -1.
func (f *retentionEngine) first(sub string) int {
	for i, c := range f.log {
		if strings.Contains(c, sub) {
			return i
		}
	}
	return -1
}

// last returns the index of the last command starting with prefix, or -1.
func (f *retentionEngine) last(prefix string) int {
	for i := len(f.log) - 1; i >= 0; i-- {
		if strings.HasPrefix(f.log[i], prefix) {
			return i
		}
	}
	return -1
}

func (f *retentionEngine) tagged() []string {
	var out []string
	for _, c := range f.log {
		if strings.HasPrefix(c, "image tag ") {
			out = append(out, c)
		}
	}
	return out
}

func (f *retentionEngine) did(cmd string) bool {
	for _, c := range f.log {
		if c == cmd {
			return true
		}
	}
	return false
}

func newRetentionRun(t *testing.T, f *retentionEngine) *updateRun {
	t.Helper()
	dir := t.TempDir()
	cli := filepath.Join(dir, "lensdock-linux")
	content := []byte("linux lensdock binary")
	if err := os.WriteFile(cli, content, 0o755); err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(dir, "backup.key")
	if _, err := GenerateBackupKey(keyFile); err != nil {
		t.Fatal(err)
	}
	backups := filepath.Join(dir, "backups")
	if err := os.MkdirAll(backups, 0o700); err != nil {
		t.Fatal(err)
	}
	dep := &Deployment{
		StateDir: dir, BackupDir: backups, BackupKeyFile: keyFile,
		LinuxCLI:   map[string]string{"amd64": cli},
		Workspaces: map[string]WorkspaceConfig{"default": {RuntimeService: "runtime"}},
		Gateway:    &GatewayConfig{Service: "gateway"},
	}
	clock := func() time.Time { return time.Date(2026, 10, 25, 1, 2, 3, 0, time.UTC) }
	dk := Docker{Run: f}
	e := &Engine{Dep: dep, Docker: dk, Out: io.Discard, Now: clock, arch: "amd64"}
	prev := State{Generation: 1, ReleaseID: "r1", Components: map[string]ComponentState{
		"runtime/default": {Service: "runtime", Image: retRef("lensdock/runtime", retRuntimeOld), ImageID: retRuntimeOld, Volumes: map[string]string{}},
		"gateway":         {Service: "gateway", Image: retRef("lensdock/gateway", retGatewayOld), ImageID: retGatewayOld, Volumes: map[string]string{}},
	}}
	m := Manifest{
		ReleaseID: "r2", Channel: ChannelDev,
		Images: map[string]ArchImages{"amd64": {
			Runtime: ImageRef{Repository: "lensdock/runtime", Digest: retRuntimeNew},
			Gateway: &ImageRef{Repository: "lensdock/gateway", Digest: retGatewayNew},
		}},
		CLI: map[string]CLIBinary{"amd64": {SHA256: sha256Hex(content)}},
	}
	plan := &Plan{
		ID: "p-20261025T010203Z-abcdef", Workspace: "default", Arch: "amd64",
		Target: map[string]PlanImage{
			"runtime/default": {Ref: retRef("lensdock/runtime", retRuntimeNew), Digest: retRuntimeNew},
			"gateway":         {Ref: retRef("lensdock/gateway", retGatewayNew), Digest: retGatewayNew},
		},
	}
	j, err := newJournal(dep, "update", "u-20261025T010203Z-abcdef", plan.ID, "default", "r2", clock)
	if err != nil {
		t.Fatal(err)
	}
	return &updateRun{
		e: e, plan: plan, rel: &Release{Manifest: m}, arch: "amd64", prev: prev,
		old:    ComposeCLI{D: dk, Project: "lensdock"},
		helper: VolumeHelper{D: dk, Image: retRef("lensdock/runtime", retRuntimeOld)},
		j:      j, candRetain: map[string]string{},
	}
}

func TestKeepTagIsInsideTheRepositoryAndNamedByFullDigest(t *testing.T) {
	hex := strings.TrimPrefix(retGatewayOld, "sha256:")
	for repo, want := range map[string]string{
		"lensdock/gateway":                "lensdock/gateway:lensdock-keep-" + hex,
		"localhost:5000/lensdock/gateway": "localhost:5000/lensdock/gateway:lensdock-keep-" + hex,
	} {
		got, err := keepTag(retRef(repo, retGatewayOld))
		if err != nil || got != want {
			t.Errorf("keepTag(%s) = %q, %v; want %q", repo, got, err, want)
		}
		if err := checkImageRef(got); err != nil {
			t.Errorf("keep tag %q is not a valid image reference: %v", got, err)
		}
	}
	for _, ref := range []string{
		"lensdock/gateway:dev@" + retGatewayOld, "lensdock/gateway:dev", "lensdock/gateway@sha256:0383", "@" + retGatewayOld, "",
	} {
		if got, err := keepTag(ref); err == nil {
			t.Errorf("keepTag(%q) = %q; a reference that is not repository@sha256:<digest> must be rejected", ref, got)
		}
	}
}

func TestApplyKeepsRunningImagesBeforeMaintenance(t *testing.T) {
	f := newRetentionEngine()
	r := newRetentionRun(t, f)
	// The fake has no Profile volume, so the run ends at step 05 once it
	// needs one. Verification, retention and the fence ran before.
	res, err := r.execute(context.Background())
	if err == nil {
		t.Fatalf("execute() succeeded with %+v; step 05 should have stopped the fake run", res)
	}
	for _, id := range []string{"01", "02", "03", "04"} {
		if st := r.j.step(id).Status; st != StepOK {
			t.Fatalf("step %s is %s (%s), want ok", id, st, r.j.step(id).Error)
		}
	}
	wantRuntime := "image tag " + retRef("lensdock/runtime", retRuntimeOld) + " " + retKeep("lensdock/runtime", retRuntimeOld)
	wantGateway := "image tag " + retRef("lensdock/gateway", retGatewayOld) + " " + retKeep("lensdock/gateway", retGatewayOld)
	if !f.did(wantRuntime) || !f.did(wantGateway) {
		t.Fatalf("running images were not kept under their own repository; commands: %q", f.log)
	}
	if n := len(f.tagged()); n != 2 {
		t.Fatalf("step 01 wrote %d tags, want only the two running images: %q", n, f.tagged())
	}
	compose := f.first("compose ")
	if compose < 0 || f.last("image tag ") > compose {
		t.Fatalf("a keep tag was written after maintenance began (first compose command %d, last tag %d): %q", compose, f.last("image tag "), f.log)
	}

	// Rebuilding :dev moves the names the running images carried.
	delete(f.names, "lensdock/runtime:previous")
	delete(f.names, "lensdock/gateway:previous")
	for _, ref := range []string{retRef("lensdock/runtime", retRuntimeOld), retRef("lensdock/gateway", retGatewayOld)} {
		if _, ok := f.resolve(ref); !ok {
			t.Errorf("%s no longer resolves after its tag moved", ref)
		}
	}
	control := newRetentionEngine()
	delete(control.names, "lensdock/gateway:previous")
	if _, ok := control.resolve(retRef("lensdock/gateway", retGatewayOld)); ok {
		t.Fatal("the fake keeps an untagged image resolvable; it does not model the failure")
	}
}

func TestApplyBlocksBeforeStoppingWhenRunningImageIsGone(t *testing.T) {
	f := newRetentionEngine()
	delete(f.names, "lensdock/gateway:previous") // the gateway's exact image is gone everywhere
	r := newRetentionRun(t, f)
	res, err := r.execute(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no local copy with that exact digest") {
		t.Fatalf("execute() error = %v, want the unavailable previous image to block", err)
	}
	if res == nil || res.Status != JournalAborted {
		t.Fatalf("result = %+v, want an aborted update", res)
	}
	if st := r.j.step("01").Status; st != StepFailed {
		t.Fatalf("step 01 is %s, want failed", st)
	}
	for _, id := range []string{"02", "03", "05"} {
		if st := r.j.step(id).Status; st != StepPending {
			t.Errorf("step %s is %s; nothing after the preflight may run", id, st)
		}
	}
	if len(r.j.StoppedServices) != 0 {
		t.Errorf("services were stopped: %v", r.j.StoppedServices)
	}
	if c := f.first("compose "); c >= 0 {
		t.Errorf("a compose command ran before the update was blocked: %q", f.log[c])
	}
	if tags := f.tagged(); len(tags) != 0 {
		t.Errorf("tags were written although one image cannot be kept: %q", tags)
	}
}

func TestApplyRecoversRunningImageFromVerifiedLegacyKeepTag(t *testing.T) {
	f := newRetentionEngine()
	delete(f.names, "lensdock/gateway:previous") // :dev moved on
	f.names["lensdock-keep/gateway:0383c4d06b62"] = retGatewayOld
	ref := retRef("lensdock/gateway", retGatewayOld)
	if _, ok := f.resolve(ref); ok {
		t.Fatal("setup: the cross-repository keep tag must not make the pinned reference resolve")
	}
	r := newRetentionRun(t, f)
	if _, err := r.execute(context.Background()); err == nil {
		t.Fatal("execute() succeeded; the fake run should stop at step 05")
	}
	if st := r.j.step("01").Status; st != StepOK {
		t.Fatalf("step 01 is %s (%s), want ok", st, r.j.step("01").Error)
	}
	retag := "image tag lensdock-keep/gateway:0383c4d06b62 " + retKeep("lensdock/gateway", retGatewayOld)
	if !f.did(retag) {
		t.Fatalf("the verified legacy copy was not re-tagged inside the original repository: %q", f.log)
	}
	if compose := f.first("compose "); compose < 0 || f.last("image tag ") > compose {
		t.Fatalf("the repair happened after maintenance began: %q", f.log)
	}
	if _, ok := f.resolve(ref); !ok {
		t.Errorf("%s still does not resolve after recovery", ref)
	}
}

func TestRecoveryRejectsLegacyKeepTagWithAnotherDigest(t *testing.T) {
	f := newRetentionEngine()
	delete(f.names, "lensdock/gateway:previous")
	// Same twelve digits, another image: the old tag cannot tell them apart.
	f.names["lensdock-keep/gateway:0383c4d06b62"] = retGatewayClash
	r := newRetentionRun(t, f)
	res, err := r.execute(context.Background())
	if err == nil || !strings.Contains(err.Error(), "different digest") {
		t.Fatalf("execute() error = %v, want the digest mismatch reported", err)
	}
	if res == nil || res.Status != JournalAborted {
		t.Fatalf("result = %+v, want an aborted update", res)
	}
	if tags := f.tagged(); len(tags) != 0 {
		t.Errorf("a mismatching image was tagged: %q", tags)
	}
	if _, ok := f.names[retKeep("lensdock/gateway", retGatewayOld)]; ok {
		t.Error("the keep tag of the pinned digest exists although no verified source was found")
	}
	if c := f.first("compose "); c >= 0 {
		t.Errorf("maintenance began despite the mismatch: %q", f.log[c])
	}
}

func TestRecoveryByDigestNeedsAnEngineThatResolvesIt(t *testing.T) {
	ref := retRef("lensdock/gateway", retGatewayOld)
	target := retainTarget{Name: "gateway", Ref: ref, Digest: retGatewayOld, Recover: true}
	for _, tc := range []struct {
		name       string
		bareDigest bool
		wantErr    bool
	}{
		{"containerd image store", true, false},
		{"classic image store", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRetentionEngine()
			delete(f.names, "lensdock/gateway:previous")
			f.names["moby-dangling:old"] = retGatewayOld // stored, but not under this repository
			f.bareDigest = tc.bareDigest
			e := &Engine{Docker: Docker{Run: f}, Out: io.Discard}
			_, err := e.retainImages(context.Background(), "amd64", []retainTarget{target})
			if (err != nil) != tc.wantErr {
				t.Fatalf("retainImages() error = %v, want error %t", err, tc.wantErr)
			}
			_, resolves := f.resolve(ref)
			if resolves == tc.wantErr {
				t.Errorf("%s resolves = %t after the call", ref, resolves)
			}
			if tc.wantErr && len(f.tagged()) != 0 {
				t.Errorf("tags written without a verified source: %q", f.tagged())
			}
		})
	}
}

func TestRetentionRejectsWrongIdentity(t *testing.T) {
	gwRef := retRef("lensdock/gateway", retGatewayOld)
	for _, tc := range []struct {
		name  string
		setup func(*retentionEngine)
		want  string
	}{
		{"reference resolves to another digest", func(f *retentionEngine) {
			f.force[gwRef] = ImageInfo{ID: retGatewayClash, OS: "linux", Arch: "amd64"}
		}, "different digest"},
		{"image is for another platform", func(f *retentionEngine) { f.arch[retGatewayOld] = "arm64" }, "the engine needs linux/amd64"},
		{"keep tag names another image", func(f *retentionEngine) {
			f.names[retKeep("lensdock/gateway", retGatewayOld)] = retGatewayClash
		}, "refusing to move it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRetentionEngine()
			tc.setup(f)
			e := &Engine{Docker: Docker{Run: f}, Out: io.Discard}
			comps := map[string]ComponentState{
				"runtime/default": {Image: retRef("lensdock/runtime", retRuntimeOld), ImageID: retRuntimeOld},
				"gateway":         {Image: gwRef, ImageID: retGatewayOld},
			}
			_, err := e.retainImages(context.Background(), "amd64", componentTargets(comps, true))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("retainImages() error = %v, want %q", err, tc.want)
			}
			if tags := f.tagged(); len(tags) != 0 {
				t.Errorf("tags written although one image was rejected: %q", tags)
			}
		})
	}
}

func TestStateImageWithoutMatchingDigestIsRejected(t *testing.T) {
	f := newRetentionEngine()
	e := &Engine{Docker: Docker{Run: f}, Out: io.Discard}
	comps := map[string]ComponentState{"gateway": {Image: retRef("lensdock/gateway", retGatewayOld), ImageID: retGatewayNew}}
	if _, err := e.retainImages(context.Background(), "amd64", componentTargets(comps, true)); err == nil {
		t.Fatal("retainImages() accepted a state whose image id differs from the digest in its reference")
	}
	if len(f.log) != 0 {
		t.Errorf("docker was consulted for an inconsistent state: %q", f.log)
	}
}

func TestReleaseImagesAreNeverRecoveredFromOldKeepTags(t *testing.T) {
	f := newRetentionEngine()
	delete(f.names, "lensdock/gateway:dev")
	f.names["lensdock-keep/gateway:"+strings.TrimPrefix(retGatewayNew, "sha256:")[:12]] = retGatewayNew
	e := &Engine{Docker: Docker{Run: f}, Out: io.Discard}
	comps := map[string]ComponentState{"gateway": {Image: retRef("lensdock/gateway", retGatewayNew), ImageID: retGatewayNew}}
	_, err := e.retainImages(context.Background(), "amd64", componentTargets(comps, false))
	if err == nil || !strings.Contains(err.Error(), "is not present locally") {
		t.Fatalf("retainImages() error = %v, want the missing release image reported", err)
	}
	if tags := f.tagged(); len(tags) != 0 {
		t.Errorf("a release image was recovered from a keep tag: %q", tags)
	}
}

func TestSwitchKeepsNewImagesBeforeAnyGenerationIsWritten(t *testing.T) {
	intent := func(r *updateRun) {
		next := r.prev.Clone()
		next.Generation = 2
		next.ReleaseID = "r2"
		next.Components["runtime/default"] = ComponentState{Service: "runtime", Image: retRef("lensdock/runtime", retRuntimeNew), ImageID: retRuntimeNew, Volumes: map[string]string{}}
		next.Components["gateway"] = ComponentState{Service: "gateway", Image: retRef("lensdock/gateway", retGatewayNew), ImageID: retGatewayNew, Volumes: map[string]string{}}
		r.j.Intent = &next
	}
	t.Run("kept before compose resolves the new generation", func(t *testing.T) {
		f := newRetentionEngine()
		r := newRetentionRun(t, f)
		intent(r)
		// The fake answers compose config with nothing, so the switch stops
		// there; everything before it is what is under test.
		if _, err := r.doSwitch(context.Background()); err == nil {
			t.Fatal("doSwitch() succeeded against a fake without a compose model")
		}
		if !f.did("image tag " + retRef("lensdock/gateway", retGatewayNew) + " " + retKeep("lensdock/gateway", retGatewayNew)) {
			t.Fatalf("the new gateway image was not kept: %q", f.log)
		}
		cfg := f.first("config --format json")
		if cfg < 0 || f.last("image tag ") > cfg {
			t.Fatalf("new images were kept after compose read the new generation: %q", f.log)
		}
	})
	t.Run("missing new image blocks before the override is written", func(t *testing.T) {
		f := newRetentionEngine()
		delete(f.names, "lensdock/gateway:dev")
		r := newRetentionRun(t, f)
		intent(r)
		_, err := r.doSwitch(context.Background())
		if err == nil || !strings.Contains(err.Error(), "keep the images of the new generation") {
			t.Fatalf("doSwitch() error = %v, want the retention failure", err)
		}
		if _, statErr := os.Stat(r.e.Dep.overridePath(2)); !os.IsNotExist(statErr) {
			t.Errorf("a generation override exists after a failed retention: %v", statErr)
		}
		if f.first("config --format json") >= 0 || f.first(" up ") >= 0 || len(f.tagged()) != 0 {
			t.Errorf("the switch went on after a failed retention: %q", f.log)
		}
	})
}

func TestRestoreHealsRunningImageNameBeforeRestart(t *testing.T) {
	f := newRetentionEngine()
	delete(f.names, "lensdock/gateway:previous")
	f.names["lensdock-keep/gateway:0383c4d06b62"] = retGatewayOld
	r := newRetentionRun(t, f)
	r.j.StoppedServices = []string{"gateway"}
	if err := r.restoreOld(context.Background()); err != nil {
		t.Fatalf("restoreOld() = %v", err)
	}
	retag := f.first("image tag lensdock-keep/gateway:0383c4d06b62 " + retKeep("lensdock/gateway", retGatewayOld))
	up := f.first(" up ")
	if retag < 0 || up < 0 || retag > up {
		t.Fatalf("the image name was not repaired before the restart (tag %d, up %d): %q", retag, up, f.log)
	}
}

func TestRollbackKeepsOldAndRunningImagesBeforeStopping(t *testing.T) {
	cp := Checkpoint{Previous: State{Components: map[string]ComponentState{
		"gateway": {Image: retRef("lensdock/gateway", retGatewayOld), ImageID: retGatewayOld},
	}}}
	cur := State{Components: map[string]ComponentState{
		"gateway": {Image: retRef("lensdock/gateway", retGatewayNew), ImageID: retGatewayNew},
	}}
	t.Run("old image gone", func(t *testing.T) {
		f := newRetentionEngine()
		delete(f.names, "lensdock/gateway:previous")
		e := &Engine{Docker: Docker{Run: f}, Out: io.Discard}
		_, err := e.retainImages(context.Background(), "amd64", rollbackTargets(cp, cur))
		if err == nil || !strings.Contains(err.Error(), "old gateway") {
			t.Fatalf("retainImages() error = %v, want the old gateway image reported", err)
		}
		if tags := f.tagged(); len(tags) != 0 {
			t.Errorf("tags written although the rollback target is missing: %q", tags)
		}
	})
	t.Run("both kept", func(t *testing.T) {
		f := newRetentionEngine()
		e := &Engine{Docker: Docker{Run: f}, Out: io.Discard}
		if _, err := e.retainImages(context.Background(), "amd64", rollbackTargets(cp, cur)); err != nil {
			t.Fatal(err)
		}
		for _, d := range []string{retGatewayOld, retGatewayNew} {
			if !f.did("image tag " + retRef("lensdock/gateway", d) + " " + retKeep("lensdock/gateway", d)) {
				t.Errorf("image %s was not kept: %q", d, f.log)
			}
		}
	})
}

func TestPlanChecksRunningImagesWithoutChangingTheEngine(t *testing.T) {
	f := newRetentionEngine()
	delete(f.names, "lensdock/gateway:previous")
	delete(f.names, "lensdock/runtime:previous")
	f.names["lensdock-keep/runtime:"+strings.TrimPrefix(retRuntimeOld, "sha256:")[:12]] = retRuntimeOld
	e := &Engine{Docker: Docker{Run: f}, Out: io.Discard}
	st := State{Components: map[string]ComponentState{
		"runtime/default": {Image: retRef("lensdock/runtime", retRuntimeOld), ImageID: retRuntimeOld},
		"gateway":         {Image: retRef("lensdock/gateway", retGatewayOld), ImageID: retGatewayOld},
	}}
	p := &Plan{}
	e.checkRetention(context.Background(), planBuilder{p}, "amd64", st)
	got := map[string]string{}
	for _, c := range p.Checks {
		got[c.ID] = c.Status
	}
	if got["retain-gateway"] != PlanFail || got["retain-runtime"] != PlanWarn || len(p.Blockers) != 1 {
		t.Fatalf("plan checks = %v, blockers %v; want the missing gateway blocked and the recoverable runtime a warning", got, p.Blockers)
	}
	if tags := f.tagged(); len(tags) != 0 {
		t.Errorf("planning changed the engine: %q", tags)
	}
	if !publicPlanCheckIDs["retain-gateway"] || !publicPlanCheckIDs["retain-runtime"] {
		t.Error("the retention check ids are not in the public status allowlist")
	}
}
