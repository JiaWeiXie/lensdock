package update

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testDigest(ch byte) string { return "sha256:" + strings.Repeat(string(ch), 64) }

func TestResolveReleaseRejectsImageReference(t *testing.T) {
	_, err := ResolveRelease(t.TempDir(), "lensdock/runtime:latest")
	if err == nil {
		t.Fatalf("ResolveRelease image tag error = %v", err)
	}
}

func TestProfileCleanExitClassification(t *testing.T) {
	for _, exitType := range []string{"Normal", "SessionEnded"} {
		if !profileExitedCleanly(exitType) {
			t.Errorf("clean Chromium exit %q blocks checkpointing", exitType)
		}
	}
	for _, exitType := range []string{"Crashed", "", "unknown", "normal"} {
		if profileExitedCleanly(exitType) {
			t.Errorf("unproven clean exit %q permits checkpointing", exitType)
		}
	}
}

func TestValidateRelPath(t *testing.T) {
	bad := []string{"", "/absolute", "../x", "a/../b", "a//b", `a\\b`, "."}
	for _, p := range bad {
		if err := validateRelPath(p); err == nil {
			t.Errorf("validateRelPath(%q) accepted", p)
		}
	}
	for _, p := range []string{"manifest.json", "evidence/runtime-amd64.spdx.json", "a.b-c_1/x"} {
		if err := validateRelPath(p); err != nil {
			t.Errorf("validateRelPath(%q) = %v", p, err)
		}
	}
}

func TestLoadDeploymentRejectsUnknownFieldAndUnsafeKeyLocation(t *testing.T) {
	dir := t.TempDir()
	compose := filepath.Join(dir, "compose.yaml")
	env := filepath.Join(dir, "compose.env")
	if err := os.WriteFile(compose, []byte("services: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "deployment.json")
	data := `{"version":1,"backupDir":"backups","backupKeyFile":"backups/key","compose":{"project":"lensdock-product","files":["compose.yaml"],"envFiles":["compose.env"]},"workspaces":{"default":{"runtimeService":"runtime","datasets":[{"key":"lensdock-profile","kind":"profile","role":"snapshot"}]}},"unexpected":true}`
	if err := os.WriteFile(file, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDeployment(file); err == nil {
		t.Fatalf("LoadDeployment unknown field error = %v", err)
	}
	data = strings.ReplaceAll(data, `,"unexpected":true`, "")
	if err := os.WriteFile(file, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDeployment(file); err == nil {
		t.Fatalf("LoadDeployment unsafe key error = %v", err)
	}
}

func TestDeploymentCandidateAccessDefaults(t *testing.T) {
	d := &Deployment{
		Version: DeploymentVersion, File: filepath.Join(t.TempDir(), "deployment.json"),
		StateDir: t.TempDir(), BackupDir: t.TempDir(), ReleasesDir: t.TempDir(), BackupKeyFile: filepath.Join(t.TempDir(), "key"),
		Compose: ComposeConfig{Project: "lensdock-product", Files: []string{filepath.Join(t.TempDir(), "compose.yaml")}},
		Workspaces: map[string]WorkspaceConfig{"default": {RuntimeService: "runtime", Datasets: []Dataset{
			{Key: "profile", Kind: KindProfile, Role: RoleSnapshot},
			{Key: "downloads", Kind: KindDownloads, Role: RoleRetained},
			{Key: "credentials", Kind: KindOther, Role: RoleRetained},
		}}},
	}
	d.applyDefaults()
	sets := d.Workspaces["default"].Datasets
	if sets[1].CandidateAccess != CandidateScratch || sets[2].CandidateAccess != CandidateReadOnly {
		t.Fatalf("candidate defaults = %#v", sets)
	}
}

func TestCandidateReadonlyPolicyPreservesLiveGatewayWrites(t *testing.T) {
	e := &Engine{Dep: &Deployment{
		Workspaces: map[string]WorkspaceConfig{"default": {Datasets: []Dataset{
			{Key: "credentials", Role: RoleRetained, CandidateAccess: CandidateReadOnly},
		}}},
		Gateway: &GatewayConfig{Datasets: []Dataset{
			{Key: "artifacts", Role: RoleRetained, CandidateAccess: CandidateReadOnly},
		}},
	}}
	model := ComposeModel{
		Services: map[string]ComposeService{
			"runtime": {Volumes: []ComposeMount{{Type: "volume", Source: "credentials", Target: "/credentials", ReadOnly: true}}},
			"gateway": {Volumes: []ComposeMount{{Type: "volume", Source: "artifacts", Target: "/data/artifacts"}}},
		},
		Volumes: map[string]ComposeVolume{"credentials": {}, "artifacts": {}},
	}
	state := State{Components: map[string]ComponentState{
		runtimeComponent("default"): {Service: "runtime"},
		gatewayComponent:            {Service: "gateway"},
	}}
	var plan Plan
	e.checkCompose(planBuilder{p: &plan}, model, "default", state)
	if len(plan.Blockers) != 0 {
		t.Fatalf("live Gateway artifact writes must not block read-only migration: %v", plan.Blockers)
	}
	model.Services["runtime"].Volumes[0].ReadOnly = false
	plan = Plan{}
	e.checkCompose(planBuilder{p: &plan}, model, "default", state)
	if len(plan.Blockers) != 1 {
		t.Fatalf("candidate Runtime must not inherit writable retained credentials: %v", plan.Blockers)
	}
}

func TestManifestRejectsMissingExactEvidence(t *testing.T) {
	m := validTestManifest(t)
	m.Images["amd64"] = ArchImages{Runtime: ImageRef{Repository: "example.com/lensdock/runtime", Digest: testDigest('a')}}
	err := m.Validate()
	if !errors.Is(err, ErrIncompleteRelease) {
		t.Fatalf("Validate missing evidence = %v", err)
	}
}

func TestUncommittedManifestRequiresExplicitDirtyDevelopmentSource(t *testing.T) {
	m := validTestManifest(t)
	m.Channel = ChannelDev
	m.Source = Source{Dirty: true}
	if err := m.Validate(); err != nil {
		t.Fatalf("explicitly uncommitted development source: %v", err)
	}
	m.Source.Dirty = false
	if err := m.Validate(); !errors.Is(err, ErrIncompleteRelease) {
		t.Fatalf("uncommitted source must not claim to be clean: %v", err)
	}
	m.Source = Source{GitCommit: "not-a-commit", Dirty: true}
	if err := m.Validate(); !errors.Is(err, ErrIncompleteRelease) {
		t.Fatalf("dirty development must not accept a fabricated commit: %v", err)
	}
}

func TestReleaseSignatureBindsExactManifestBytes(t *testing.T) {
	pub, priv, err := GenerateSigningKey()
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"schemaVersion":1}`)
	sig := SignManifest(raw, priv)
	trusted := []TrustedKey{{KeyID: KeyIDOf(pub), PublicKey: base64.StdEncoding.EncodeToString(pub)}}
	if _, err := VerifyManifestSignature(raw, sig, trusted); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyManifestSignature(append(raw, '\n'), sig, trusted); err == nil {
		t.Fatal("signature accepted modified manifest bytes")
	}
	if sig.KeyID != keyIDOfPublic(ed25519.PublicKey(pub)) {
		t.Fatalf("key id = %s", sig.KeyID)
	}
}

func TestSealRoundTripAndCorruption(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, sealKeyBytes)
	plain := bytes.Repeat([]byte("LensDock-繁體中文-"), 200000)
	var sealed bytes.Buffer
	sw, err := NewSealWriter(&sealed, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sw.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := sw.Close(); err != nil {
		t.Fatal(err)
	}
	sr, err := NewSealReader(bytes.NewReader(sealed.Bytes()), key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(sr)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("round trip: bytes=%d err=%v", len(got), err)
	}
	for _, mutate := range []func([]byte) []byte{
		func(b []byte) []byte { b[len(b)/2] ^= 1; return b },
		func(b []byte) []byte { return b[:len(b)-1] },
		func(b []byte) []byte { return append(b, 'x') },
	} {
		b := mutate(bytes.Clone(sealed.Bytes()))
		r, err := NewSealReader(bytes.NewReader(b), key)
		if err == nil {
			_, err = io.ReadAll(r)
		}
		if err == nil {
			t.Fatal("corrupt sealed stream was accepted")
		}
	}
}

func TestFingerprintIsIndependentOfFindOrder(t *testing.T) {
	metaA := []byte("f\t600\t1\t2\t3\t\tb\x00d\t700\t1\t2\t9\t\t.\x00f\t600\t1\t2\t2\t\ta\x00")
	metaB := []byte("f\t600\t1\t2\t2\t\ta\x00f\t600\t1\t2\t3\t\tb\x00d\t700\t1\t2\t1\t\t.\x00")
	sumsA := []byte(strings.Repeat("a", 64) + "  /src/b\x00" + strings.Repeat("b", 64) + "  /src/a\x00")
	sumsB := []byte(strings.Repeat("b", 64) + "  /src/a\x00" + strings.Repeat("a", 64) + "  /src/b\x00")
	a, err := ComputeFingerprint(metaA, sumsA)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ComputeFingerprint(metaB, sumsB)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Equal(b) {
		t.Fatalf("fingerprints differ: %#v %#v", a, b)
	}
}

func TestColdSnapshotJournalClassification(t *testing.T) {
	zeroHeader := make([]byte, 8)
	for _, tt := range []struct {
		name, kind, path string
		header           []byte
		wantRecovery     bool
	}{
		{"committed persistent rollback journal", KindProfile, "Default/History-journal", zeroHeader, false},
		{"hot rollback journal", KindProfile, "Default/History-journal", []byte{0xd9, 0xd5, 0x05, 0xf9, 0x20, 0xa1, 0x63, 0xd7}, true},
		{"incomplete rollback header", KindProfile, "Default/History-journal", []byte{0, 0}, true},
		{"persistent browser WAL remains in snapshot", KindProfile, "cache.db-wal", nil, false},
		{"gateway WAL must have been truncated", KindDB, "gateway.db-wal", nil, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := journalNeedsRecovery(tt.kind, tt.path, tt.header); got != tt.wantRecovery {
				t.Fatalf("recovery required = %v, want %v", got, tt.wantRecovery)
			}
		})
	}
}

func TestGenerationOverridePinsPairedImagesAndVolumes(t *testing.T) {
	s := State{Generation: 2, Components: map[string]ComponentState{
		"runtime/default": {Service: "runtime", Image: "example.com/lensdock/runtime@" + testDigest('a'), Volumes: map[string]string{"profile": "profile-g2"}},
		"gateway":         {Service: "gateway", Image: "example.com/lensdock/gateway@" + testDigest('b'), Volumes: map[string]string{"db": "db-g2", "artifacts": "artifacts"}},
	}}
	data, err := RenderGenerationOverride(s)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{"runtime:", "gateway:", testDigest('a'), testDigest('b'), `name: "profile-g2"`, `name: "db-g2"`, `external: true`} {
		if !strings.Contains(text, want) {
			t.Errorf("override lacks %q:\n%s", want, text)
		}
	}
}

func TestPlanDigestDetectsModification(t *testing.T) {
	now := time.Date(2026, 10, 25, 1, 2, 3, 0, time.UTC)
	p := &Plan{SchemaVersion: planSchemaVersion, ID: "p-20261025T010203Z-abcdef", CreatedAt: now, ExpiresAt: now.Add(time.Hour), Workspace: "default"}
	p.Digest = planDigest(p)
	data, _ := json.Marshal(p)
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPlan(path, now); err != nil {
		t.Fatal(err)
	}
	p.Workspace = "other"
	data, _ = json.Marshal(p)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPlan(path, now); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("modified plan error = %v", err)
	}
}

func TestReadPublicStatusAcceptsBootstrapSubset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deployment-status.json")
	data := []byte(`{"release":{"id":"development","verified":false},"update":{"phase":"not-configured","lastBackupAt":null}}`)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := ReadPublicStatus(path)
	if err != nil || s.Release.ID != "development" || s.Update.Phase != "not-configured" {
		t.Fatalf("ReadPublicStatus = %#v, %v", s, err)
	}
}

const (
	privateHostPath = "/Users/alice/private-lab/.lensdock/product/public"
	privateVolume   = "lensdock-product_gateway-private-g7"
	privateSecret   = "operator-token=s3cr3t-value"
)

func publicTestDeployment(t *testing.T) *Deployment {
	t.Helper()
	return &Deployment{StateDir: t.TempDir(), Workspaces: map[string]WorkspaceConfig{"default": {}}}
}

func savePublicTestPlan(t *testing.T, d *Deployment, p *Plan) {
	t.Helper()
	p.SchemaVersion = planSchemaVersion
	p.Workspace = "default"
	p.Release.ID = "2026.10.25"
	p.Current = PlanCurrent{Generation: 1, Tracked: true, ReleaseID: "2026.10.20", Components: map[string]ComponentState{}}
	p.Digest = planDigest(p)
	if err := os.MkdirAll(d.plansDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(filepath.Join(d.plansDir(), p.ID+".json"), p, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPublicPendingPlanNeverDisclosesPrivateDiagnostics(t *testing.T) {
	d := publicTestDeployment(t)
	now := time.Date(2026, 10, 25, 2, 0, 0, 0, time.UTC)
	p := &Plan{
		ID: "p-20261025T010000Z-aaaaaa", CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(23 * time.Hour),
		Checks: []PlanCheck{
			{ID: "compose", Status: PlanWarn, Detail: "service gateway bind-mounts " + privateHostPath + ": candidate verification shares that host path"},
			{ID: "compose", Status: PlanWarn, Detail: "service runtime bind-mounts " + privateHostPath},
			{ID: "lease", Status: PlanWarn, Detail: "a lease held by " + privateSecret + " is active"},
			{ID: privateHostPath, Status: PlanWarn, Detail: "volume " + privateVolume},
			{ID: "capacity", Status: PlanFail, Detail: "measure " + privateVolume + ": docker stderr " + privateSecret},
			{ID: "manifest", Status: PlanOK, Detail: "verified " + privateHostPath},
		},
		Warnings: []string{"compose: service gateway bind-mounts " + privateHostPath},
		Blockers: []string{"capacity: " + privateVolume},
	}
	savePublicTestPlan(t, d, p)
	st := State{Generation: 1, ReleaseID: "2026.10.20"}

	got := publicPendingPlan(d, st, true, nil, now)
	if got == nil || got.ID != p.ID || got.Release != "2026.10.25" || got.Workspace != "default" {
		t.Fatalf("pending plan = %#v", got)
	}
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{privateHostPath, "/Users/", privateVolume, privateSecret, "stderr", "bind-mounts"} {
		if strings.Contains(string(data), leak) {
			t.Fatalf("public pending plan leaks %q: %s", leak, data)
		}
	}
	wantWarnings := []string{
		"compose: 2 warnings; exact detail is in the private host plan",
		"lease: 1 warning; exact detail is in the private host plan",
		"other: 1 warning; exact detail is in the private host plan",
	}
	if strings.Join(got.Warnings, "|") != strings.Join(wantWarnings, "|") {
		t.Fatalf("warnings = %q, want %q", got.Warnings, wantWarnings)
	}
	if len(got.Blockers) != 1 || got.Blockers[0] != "capacity: 1 blocker; exact detail is in the private host plan" {
		t.Fatalf("blockers = %q", got.Blockers)
	}

	// The private host plan keeps every exact diagnostic.
	private, err := loadPlanFile(filepath.Join(d.plansDir(), p.ID+".json"))
	if err != nil || len(private.Checks) != len(p.Checks) || !strings.Contains(private.Checks[0].Detail, privateHostPath) || len(private.Warnings) != 1 {
		t.Fatalf("private plan lost detail: %#v, %v", private, err)
	}
}

func TestPublicPendingPlanOmitsEmptyIssueCollections(t *testing.T) {
	d := publicTestDeployment(t)
	now := time.Date(2026, 10, 25, 2, 0, 0, 0, time.UTC)
	savePublicTestPlan(t, d, &Plan{
		ID: "p-20261025T010000Z-aaaaaa", CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour),
		Checks: []PlanCheck{{ID: "manifest", Status: PlanOK, Detail: "verified"}},
	})
	got := publicPendingPlan(d, State{Generation: 1, ReleaseID: "2026.10.20"}, true, nil, now)
	if got == nil {
		t.Fatal("a planned update is pending")
	}
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "blockers") || strings.Contains(string(data), "warnings") {
		t.Fatalf("empty optional collections must be omitted: %s", data)
	}
}

func TestPublicPendingPlanFollowsPlanLifecycle(t *testing.T) {
	now := time.Date(2026, 10, 25, 2, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	const planID = "p-20261025T010000Z-aaaaaa"
	current := State{Generation: 1, ReleaseID: "2026.10.20"}

	for _, tt := range []struct {
		name    string
		journal string // journal status of an update made from the plan; empty for none
		state   State
		now     time.Time
		pending bool
	}{
		{name: "planned and not yet applied", state: current, now: now, pending: true},
		{name: "update awaiting approval is shown as the operation, not as a plan", journal: JournalAwaitingApproval, state: current, now: now},
		{name: "update running", journal: JournalRunning, state: current, now: now},
		{name: "update held", journal: JournalHeld, state: current, now: now},
		{name: "update completed", journal: JournalCompleted, state: current, now: now},
		{name: "update completed and generation advanced", journal: JournalCompleted, state: State{Generation: 2, ReleaseID: "2026.10.25"}, now: now},
		{name: "update aborted restored the old services", journal: JournalAborted, state: current, now: now, pending: true},
		{name: "update reverted restored the old services", journal: JournalReverted, state: current, now: now, pending: true},
		{name: "deployment drifted without a journal", state: State{Generation: 2, ReleaseID: "2026.10.22"}, now: now},
		{name: "plan expired", state: current, now: now.Add(25 * time.Hour)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := publicTestDeployment(t)
			savePublicTestPlan(t, d, &Plan{ID: planID, CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(23 * time.Hour)})
			if tt.journal != "" {
				j, err := newJournal(d, "update", "u-20261025T015900Z-bbbbbb", planID, "default", "2026.10.25", clock)
				if err != nil {
					t.Fatal(err)
				}
				if tt.journal != JournalRunning {
					j.finish(tt.journal, "")
				}
			}
			js, errs := listJournals(d, clock)
			if len(errs) > 0 {
				t.Fatal(errs)
			}
			got := publicPendingPlan(d, tt.state, true, js, tt.now)
			if (got != nil) != tt.pending {
				t.Fatalf("pending plan = %#v, want pending=%t", got, tt.pending)
			}
		})
	}
}

func TestPublicPendingPlanPicksNewestActionablePlan(t *testing.T) {
	d := publicTestDeployment(t)
	now := time.Date(2026, 10, 25, 2, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	const (
		older    = "p-20261025T000000Z-aaaaaa"
		consumed = "p-20261025T010000Z-bbbbbb"
	)
	savePublicTestPlan(t, d, &Plan{ID: older, CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(22 * time.Hour)})
	savePublicTestPlan(t, d, &Plan{ID: consumed, CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(23 * time.Hour)})
	j, err := newJournal(d, "update", "u-20261025T015900Z-cccccc", consumed, "default", "2026.10.25", clock)
	if err != nil {
		t.Fatal(err)
	}
	j.finish(JournalCompleted, "")
	js, errs := listJournals(d, clock)
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	// The plan consumed by a completed update is newer, yet the older plan
	// that is still applicable is the pending one.
	got := publicPendingPlan(d, State{Generation: 1, ReleaseID: "2026.10.20"}, true, js, now)
	if got == nil || got.ID != older {
		t.Fatalf("pending plan = %#v, want %s", got, older)
	}
}

func TestPublicPendingPlanIgnoresPlanWithFreeTextIdentifiers(t *testing.T) {
	d := publicTestDeployment(t)
	now := time.Date(2026, 10, 25, 2, 0, 0, 0, time.UTC)
	p := &Plan{ID: "p-20261025T010000Z-aaaaaa", CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour)}
	savePublicTestPlan(t, d, p)
	p.Release.ID = privateHostPath
	p.Digest = planDigest(p)
	if err := os.Remove(filepath.Join(d.plansDir(), p.ID+".json")); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(filepath.Join(d.plansDir(), p.ID+".json"), p, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := publicPendingPlan(d, State{Generation: 1, ReleaseID: "2026.10.20"}, true, nil, now); got != nil {
		t.Fatalf("a plan with a non-identifier release id was published: %#v", got)
	}
}

func TestPublicReleaseEvidenceCoversEveryImage(t *testing.T) {
	gw := func(m *Manifest) {
		m.Images["amd64"] = ArchImages{Runtime: m.Images["amd64"].Runtime, Gateway: &ImageRef{Repository: "example.com/lensdock/gateway", Digest: testDigest('b')}}
	}
	sbom, prov := &SBOMEvidence{}, &ProvenanceEvidence{}
	scan := func(tool string, critical, high int) *ScanEvidence {
		return &ScanEvidence{Tool: tool, Summary: VulnSummary{Critical: critical, High: high}}
	}
	setImages := func(m *Manifest, runtime, gateway func(*ImageRef)) {
		imgs := m.Images["amd64"]
		runtime(&imgs.Runtime)
		gateway(imgs.Gateway)
		m.Images["amd64"] = imgs
	}
	build := func(runtime, gateway func(*ImageRef)) PublicEvidence {
		m := validTestManifest(t)
		gw(&m)
		setImages(&m, runtime, gateway)
		ev := publicRelease(&Release{Manifest: m}, 1, "amd64").Evidence
		if ev == nil {
			t.Fatal("release has images for the architecture but no evidence")
		}
		return *ev
	}
	none := func(*ImageRef) {}

	t.Run("one runtime SBOM does not prove an SBOM for the release", func(t *testing.T) {
		ev := build(func(r *ImageRef) { r.SBOM = sbom }, none)
		if ev.SBOM {
			t.Fatalf("evidence claims a complete SBOM while the gateway has none: %#v", ev)
		}
	})
	t.Run("one gateway provenance does not prove provenance for the release", func(t *testing.T) {
		ev := build(none, func(r *ImageRef) { r.Provenance = prov })
		if ev.Provenance {
			t.Fatalf("evidence claims complete provenance while the runtime has none: %#v", ev)
		}
	})
	t.Run("every image carries SBOM and provenance", func(t *testing.T) {
		both := func(r *ImageRef) { r.SBOM, r.Provenance = sbom, prov }
		ev := build(both, both)
		if !ev.SBOM || !ev.Provenance {
			t.Fatalf("complete evidence = %#v", ev)
		}
	})
	t.Run("a partial scan is marked and counts only scanned images", func(t *testing.T) {
		ev := build(func(r *ImageRef) { r.Scan = scan("trivy", 2, 3) }, none)
		if ev.Scan == nil || !ev.Scan.Partial || ev.Scan.Tool != "trivy" || ev.Scan.Critical != 2 || ev.Scan.High != 3 {
			t.Fatalf("partial scan = %#v", ev.Scan)
		}
	})
	t.Run("a complete scan sums every image and names each tool", func(t *testing.T) {
		ev := build(func(r *ImageRef) { r.Scan = scan("trivy", 1, 2) }, func(r *ImageRef) { r.Scan = scan("grype", 0, 4) })
		if ev.Scan == nil || ev.Scan.Partial || ev.Scan.Tool != "grype, trivy" || ev.Scan.Critical != 1 || ev.Scan.High != 6 {
			t.Fatalf("complete scan = %#v", ev.Scan)
		}
	})
	t.Run("no scan evidence yields no scan summary", func(t *testing.T) {
		if ev := build(none, none); ev.Scan != nil {
			t.Fatalf("scan = %#v", ev.Scan)
		}
	})
	t.Run("a runtime-only release is complete with the runtime evidence alone", func(t *testing.T) {
		m := validTestManifest(t)
		img := m.Images["amd64"]
		img.Runtime.SBOM, img.Runtime.Provenance = sbom, prov
		m.Images["amd64"] = img
		ev := publicRelease(&Release{Manifest: m}, 1, "amd64").Evidence
		if ev == nil || !ev.SBOM || !ev.Provenance {
			t.Fatalf("evidence = %#v", ev)
		}
	})
}

func validTestManifest(t *testing.T) Manifest {
	t.Helper()
	return Manifest{
		SchemaVersion: ManifestSchemaVersion, ReleaseID: "2026.10.25", Channel: ChannelRelease,
		CreatedAt: time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC), Source: Source{GitCommit: strings.Repeat("c", 40)},
		Base:       BaseImage{Name: "debian:trixie-slim", Digest: testDigest('d')},
		Images:     map[string]ArchImages{"amd64": {Runtime: ImageRef{Repository: "example.com/lensdock/runtime", Digest: testDigest('a')}}},
		APT:        APT{ObservedAt: time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC), Sources: []string{"https://deb.debian.org/debian"}, Packages: []Package{{Name: "chromium", Version: "138.0.1-1", Arch: "amd64"}}},
		Components: Components{Chromium: "138.0.1-1", NoVNC: "1.6.0", AgentBrowser: "0.39.2", Gateway: "1.0.0", CLI: "1.0.0"},
		CLI:        map[string]CLIBinary{"amd64": {SHA256: strings.Repeat("e", 64), Size: 10}},
		Rollback:   RollbackInfo{Mode: RollbackModePaired},
		Tests:      &TestsEvidence{Attachment: Attachment{Path: "tests.json", SHA256: strings.Repeat("f", 64), Size: 10}, Summary: "passed", Passed: true},
	}
}
