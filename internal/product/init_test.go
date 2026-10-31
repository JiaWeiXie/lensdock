package product

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JiaWeiXie/lensdock/internal/update"
)

func TestInitializeSnapshotsTheBundleAndSurvivesItsRemoval(t *testing.T) {
	opts := releaseOptions(t, "")
	dep, err := Initialize(opts)
	if err != nil {
		t.Fatal(err)
	}
	// The deployment must not depend on the bundle any more.
	if err := os.RemoveAll(opts.Bundle.Root); err != nil {
		t.Fatal(err)
	}
	loaded, err := update.LoadDeployment(filepath.Join(opts.StateDir, DeploymentFile))
	if err != nil {
		t.Fatal(err)
	}
	product := filepath.Join(opts.StateDir, ProductSubdir)
	rel := filepath.Join(product, ReleaseDir)
	if dep.StateDir != product || loaded.StateDir != product {
		t.Fatalf("state dir = %s / %s, want %s", dep.StateDir, loaded.StateDir, product)
	}
	if got, want := loaded.Compose.Files, []string{filepath.Join(rel, "docker", "compose.yaml"), filepath.Join(product, PinFile)}; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("compose files = %v, want %v", got, want)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		p := loaded.LinuxCLI[arch]
		if p != filepath.Join(rel, "bin", "linux-"+arch, "lensdock") {
			t.Fatalf("linuxCli[%s] = %s", arch, p)
		}
		fi, err := os.Stat(p)
		if err != nil || readFile(t, p) != "linux "+arch+" helper 1.2.3" || fi.Mode().Perm() != 0o755 {
			t.Fatalf("helper %s: %v %v", arch, err, fi)
		}
	}
	for _, p := range []string{rel, filepath.Join(rel, "docker"), product} {
		if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o700 {
			t.Fatalf("%s: %v %v", p, err, fi)
		}
	}
	if readFile(t, filepath.Join(product, PinFile)) != PinPlaceholder {
		t.Fatal("a new deployment must start with the placeholder pin file")
	}
	env := readFile(t, filepath.Join(product, "compose.env"))
	for _, want := range []string{"LENSDOCK_RUNTIME_IMAGE='ghcr.io/jiaweixie/lensdock/runtime:latest'\n", "LENSDOCK_GATEWAY_IMAGE='ghcr.io/jiaweixie/lensdock/gateway:latest'\n", "LENSDOCK_HOST_PORT=18581\n", "LENSDOCK_MODE=AGENT\n", "LENSDOCK_STATE_DIR='" + opts.StateDir + "'\n"} {
		if !strings.Contains(env, want) {
			t.Errorf("compose.env lacks %q:\n%s", want, env)
		}
	}
	if _, err := os.Stat(filepath.Join(product, "gateway-host.json")); !os.IsNotExist(err) {
		t.Errorf("a release deployment must not carry the source-checkout host Gateway config: %v", err)
	}
	if dep.ManagedBy != nil || strings.Contains(readFile(t, filepath.Join(product, "gateway.json")), "deploymentId") {
		t.Error("a standalone deployment must have no manager owner and no deployment identity")
	}
	// Nothing of the staging protocol remains once the deployment is committed.
	for _, leftover := range []string{stagingName, ProductSubdir + "/" + recordFile, ProductSubdir + "/" + stagingMarkerFile} {
		if _, err := os.Lstat(filepath.Join(opts.StateDir, filepath.FromSlash(leftover))); !os.IsNotExist(err) {
			t.Errorf("%s remains after a committed initialization: %v", leftover, err)
		}
	}
}

func TestInitializeRecordsTheManagedIdentity(t *testing.T) {
	opts := releaseOptions(t, "")
	opts.DeploymentID = "prj_1"
	opts.ManagedBy = &update.ManagedOwner{ManagerID: "mgr_1", ProjectID: "prj_1", ManagerRoot: "/var/lib/manager"}
	dep, err := Initialize(opts)
	if err != nil {
		t.Fatal(err)
	}
	if dep.ManagedBy == nil || *dep.ManagedBy != *opts.ManagedBy {
		t.Fatalf("managedBy = %+v", dep.ManagedBy)
	}
	if !strings.Contains(readFile(t, filepath.Join(dep.StateDir, "gateway.json")), `"deploymentId": "prj_1"`) {
		t.Fatal("the Gateway configuration lacks the deployment identity")
	}
	again, err := update.LoadDeployment(dep.File)
	if err != nil || again.ManagedBy == nil || again.ManagedBy.ProjectID != "prj_1" {
		t.Fatalf("the owner was not persisted: %v %+v", err, again)
	}
}

func TestInitializeValidatesBeforeCreatingState(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*InitOptions)
	}{
		{"mode", func(o *InitOptions) { o.Mode = "AUTO" }},
		{"project", func(o *InitOptions) { o.Project = "Bad_Name" }},
		{"image", func(o *InitOptions) { o.RuntimeImage = "bad image" }},
		{"image option", func(o *InitOptions) { o.GatewayImage = "-oops" }},
		{"missing image", func(o *InitOptions) { o.GatewayImage = "" }},
		{"origin port", func(o *InitOptions) { o.Origin = "http://127.0.0.1" }},
		{"origin host", func(o *InitOptions) { o.Origin = "http://example.com:18081" }},
		{"workspace", func(o *InitOptions) { o.WorkspaceID = "-bad" }},
		{"deployment id", func(o *InitOptions) { o.DeploymentID = "bad id" }},
		{"owner without matching deployment id", func(o *InitOptions) {
			o.ManagedBy = &update.ManagedOwner{ManagerID: "m", ProjectID: "p", ManagerRoot: "/r"}
		}},
		{"incomplete bundle", func(o *InitOptions) {
			broken := writeBundle(t, "1.0.1")
			if err := os.Remove(filepath.Join(broken, "docker", "browser", "seccomp", "chromium.json")); err != nil {
				t.Fatal(err)
			}
			o.Bundle = &Bundle{Root: broken, Version: "1.0.1"}
		}},
		{"forged bundle version", func(o *InitOptions) { o.Bundle = &Bundle{Root: o.Bundle.Root, Version: "9.9.9"} }},
		{"no bundle and no source checkout", func(o *InitOptions) { o.Bundle, o.SourceDir = nil, t.TempDir() }},
		{"no bundle and no source dir", func(o *InitOptions) { o.Bundle = nil }},
	} {
		opts := releaseOptions(t, "")
		tc.mutate(&opts)
		_, err := Initialize(opts)
		var oe *OptionError
		if !errors.As(err, &oe) {
			t.Errorf("%s: err = %v, want an OptionError", tc.name, err)
		}
		if _, err := os.Stat(opts.StateDir); !os.IsNotExist(err) {
			t.Errorf("%s: invalid input created state: %v", tc.name, err)
		}
	}
}

func TestInitializeKeepsTheSourceCheckoutFlow(t *testing.T) {
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "docker"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(src, "docker", "compose.yaml"), "services: {}\n", 0o644)
	runtime, gateway := DefaultImages(nil)
	state := filepath.Join(t.TempDir(), "state")
	dep, err := Initialize(InitOptions{StateDir: state, Origin: DefaultOrigin, WorkspaceID: DefaultWorkspace, Project: DefaultProject, RuntimeImage: runtime, GatewayImage: gateway, SourceDir: src, Mode: "novnc"})
	if err != nil {
		t.Fatal(err)
	}
	if len(dep.Compose.Files) != 1 || dep.Compose.Files[0] != filepath.Join(src, "docker", "compose.yaml") {
		t.Fatalf("compose files = %v", dep.Compose.Files)
	}
	if dep.LinuxCLI["amd64"] != filepath.Join(src, "bin", "linux-amd64", "lensdock") {
		t.Fatalf("linuxCli = %v", dep.LinuxCLI)
	}
	env := readFile(t, filepath.Join(dep.StateDir, "compose.env"))
	for _, want := range []string{"LENSDOCK_RUNTIME_IMAGE='lensdock/runtime:dev'\n", "LENSDOCK_GATEWAY_IMAGE='lensdock/gateway:dev'\n", "LENSDOCK_MODE=NOVNC\n"} {
		if !strings.Contains(env, want) {
			t.Errorf("compose.env lacks %q:\n%s", want, env)
		}
	}
	host := readFile(t, filepath.Join(dep.StateDir, "gateway-host.json"))
	if !strings.Contains(host, filepath.Join(src, "apps/web/dist")) || !strings.Contains(host, filepath.Join(state, "operator-token")) {
		t.Errorf("source checkout host Gateway config does not point at the checkout:\n%s", host)
	}
	if _, err := os.Stat(filepath.Join(dep.StateDir, PinFile)); !os.IsNotExist(err) {
		t.Errorf("a source checkout must not get a pin file: %v", err)
	}
}

func TestInitializeNeverOverwritesAnExistingDeployment(t *testing.T) {
	opts := releaseOptions(t, "")
	dep, err := Initialize(opts)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := func() []string {
		out := []string{readFile(t, dep.File)}
		for _, n := range []string{"admin-token", "agent-token", "gateway.json", "ca.crt", "compose.env"} {
			out = append(out, readFile(t, filepath.Join(dep.StateDir, n)))
		}
		return out
	}
	before := snapshot()
	if _, err := Initialize(opts); err == nil || !strings.Contains(err.Error(), "never overwrites") {
		t.Fatalf("second init: %v", err)
	}
	for i, after := range snapshot() {
		if after != before[i] {
			t.Fatalf("file %d of the existing deployment changed", i)
		}
	}
}

func TestInitializeReusesAndRepairsTheOperatorCredentials(t *testing.T) {
	// A token from `lensdock admin init` is reused whole, together with its runtime.env.
	opts := releaseOptions(t, "")
	token, err := EnsureOperatorCredentials(opts.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	env := readFile(t, filepath.Join(opts.StateDir, RuntimeEnvFile))
	if !strings.HasPrefix(env, "LENSDOCK_OPERATOR_TOKEN_SHA256=") {
		t.Fatalf("runtime.env = %q", env)
	}
	dep, err := Initialize(opts)
	if err != nil {
		t.Fatal(err)
	}
	if readFile(t, filepath.Join(dep.StateDir, OperatorTokenFile)) != string(token) || readFile(t, filepath.Join(opts.StateDir, OperatorTokenFile)) != string(token) {
		t.Fatal("the existing operator token was not reused")
	}
	if readFile(t, filepath.Join(opts.StateDir, RuntimeEnvFile)) != env {
		t.Fatal("runtime.env changed")
	}

	// An interruption between the token and its runtime.env is repaired from the token.
	other := t.TempDir()
	token, err = EnsureOperatorCredentials(other)
	if err != nil {
		t.Fatal(err)
	}
	want := readFile(t, filepath.Join(other, RuntimeEnvFile))
	if err := os.Remove(filepath.Join(other, RuntimeEnvFile)); err != nil {
		t.Fatal(err)
	}
	again, err := EnsureOperatorCredentials(other)
	if err != nil || string(again) != string(token) || readFile(t, filepath.Join(other, RuntimeEnvFile)) != want {
		t.Fatalf("repair: %v %q", err, again)
	}
}

// secrets returns the credential files of a product directory.
func secrets(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, n := range []string{"operator-token", "admin-token", "human-token", "viewer-token", "agent-token", "ca.crt", "runtime.key", "gateway.key", "gateway.json"} {
		out[n] = readFile(t, filepath.Join(dir, n))
	}
	return out
}

func sameSecrets(t *testing.T, want, got map[string]string) {
	t.Helper()
	for n, v := range want {
		if got[n] != v {
			t.Errorf("%s was rotated by the recovery", n)
		}
	}
}

func TestInterruptedInitializationCompletesWithTheSecretsItAlreadyHas(t *testing.T) {
	// Each step is a durable state a crash can leave behind.
	for _, tc := range []struct {
		name string
		// crash leaves the state directory in the state of the crash and
		// returns the directory that holds the generated credentials.
		crash func(t *testing.T, p *initPlan) string
	}{
		{"complete staging before it was moved", func(t *testing.T, p *initPlan) string {
			if _, err := p.build(); err != nil {
				t.Fatal(err)
			}
			return p.staging
		}},
		{"moved before the deployment was committed", func(t *testing.T, p *initPlan) string {
			if _, err := p.build(); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(p.staging, p.dir); err != nil {
				t.Fatal(err)
			}
			return p.dir
		}},
		{"committed before the markers were removed", func(t *testing.T, p *initPlan) string {
			rec, err := p.build()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(p.staging, p.dir); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(p.root, DeploymentFile), string(rec.Deployment)+"\n", 0o600)
			return p.dir
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := releaseOptions(t, "")
			p, err := newPlan(opts)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(p.root, 0o700); err != nil {
				t.Fatal(err)
			}
			before := secrets(t, tc.crash(t, p))
			dep, err := Initialize(opts)
			if err != nil {
				t.Fatalf("recovery failed: %v", err)
			}
			sameSecrets(t, before, secrets(t, dep.StateDir))
			if _, err := os.Stat(filepath.Join(opts.StateDir, stagingName)); !os.IsNotExist(err) {
				t.Errorf("staging remains: %v", err)
			}
			if _, err := update.LoadDeployment(dep.File); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestIncompleteStagingIsDiscardedAndForeignDirectoriesAreNeverTouched(t *testing.T) {
	opts := releaseOptions(t, "")
	p, err := newPlan(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(p.staging, 0o700); err != nil {
		t.Fatal(err)
	}

	// A directory without this tool's owner marker is somebody else's.
	writeFile(t, filepath.Join(p.staging, "keep"), "mine", 0o600)
	if _, err := Initialize(opts); err == nil || readFile(t, filepath.Join(p.staging, "keep")) != "mine" {
		t.Fatalf("a foreign staging directory was used or removed: %v", err)
	}

	// An owned staging directory without the final record is the remnant of a
	// crash before anything used its secrets.
	writeFile(t, filepath.Join(p.staging, stagingMarkerFile), stagingMarker, 0o600)
	writeFile(t, filepath.Join(p.staging, "admin-token"), "partial", 0o600)
	dep, err := Initialize(opts)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(readFile(t, filepath.Join(dep.StateDir, "admin-token")), "partial") {
		t.Fatal("an incomplete staging directory leaked into the deployment")
	}
	if _, err := os.Stat(p.staging); !os.IsNotExist(err) {
		t.Fatalf("staging remains: %v", err)
	}
}

func TestInitializeRefusesAProductDirectoryItDidNotStage(t *testing.T) {
	opts := releaseOptions(t, "")
	product := filepath.Join(opts.StateDir, ProductSubdir)
	if err := os.MkdirAll(product, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(product, "admin-token"), "established", 0o600)
	if _, err := Initialize(opts); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err = %v", err)
	}
	if readFile(t, filepath.Join(product, "admin-token")) != "established" {
		t.Fatal("an existing product directory was modified")
	}
	if _, err := os.Stat(filepath.Join(opts.StateDir, DeploymentFile)); !os.IsNotExist(err) {
		t.Fatalf("a deployment was committed over a foreign directory: %v", err)
	}
}

func TestInterruptedInitializationRefusesDifferentOptions(t *testing.T) {
	opts := releaseOptions(t, "")
	p, err := newPlan(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(p.root, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := p.build(); err != nil {
		t.Fatal(err)
	}
	before := secrets(t, p.staging)
	changed := opts
	changed.Origin = "http://127.0.0.1:18999"
	if _, err := Initialize(changed); err == nil || !strings.Contains(err.Error(), "other options") {
		t.Fatalf("err = %v", err)
	}
	sameSecrets(t, before, secrets(t, p.staging))
	if _, err := os.Stat(filepath.Join(opts.StateDir, DeploymentFile)); !os.IsNotExist(err) {
		t.Fatalf("a deployment with other options was committed: %v", err)
	}
	// The original options still complete it.
	if _, err := Initialize(opts); err != nil {
		t.Fatal(err)
	}
}

func TestFailedInitializationLeavesNoStagingAndRetriesWithTheSameOperatorToken(t *testing.T) {
	opts := releaseOptions(t, "")
	p, err := newPlan(opts)
	if err != nil {
		t.Fatal(err)
	}
	// The bundle is damaged after validation, so the copy fails midway.
	damaged := filepath.Join(opts.Bundle.Root, "bin", "linux-arm64", "lensdock")
	saved := readFile(t, damaged)
	if err := os.Remove(damaged); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(p.root, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := p.run(); err == nil {
		t.Fatal("initialization from a damaged bundle succeeded")
	}
	for _, name := range []string{stagingName, ProductSubdir, DeploymentFile} {
		if _, err := os.Lstat(filepath.Join(opts.StateDir, name)); !os.IsNotExist(err) {
			t.Fatalf("a failed initialization left %s: %v", name, err)
		}
	}
	token := readFile(t, filepath.Join(opts.StateDir, OperatorTokenFile))
	writeFile(t, damaged, saved, 0o755)
	dep, err := Initialize(opts)
	if err != nil {
		t.Fatal(err)
	}
	if readFile(t, filepath.Join(dep.StateDir, OperatorTokenFile)) != token {
		t.Fatal("the retry replaced the operator token")
	}
}

// Pairing is the default: the Gateway configuration carries neither option, and the
// Gateway then requires a token. Automatic local entry is an explicit opt-in.
func TestInitializeRequiresPairingUnlessLocalAutoEntryIsRequested(t *testing.T) {
	dep, err := Initialize(releaseOptions(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	cfg := readFile(t, filepath.Join(dep.StateDir, "gateway.json"))
	if strings.Contains(cfg, "localAutoEntry") || strings.Contains(cfg, "requirePairing") {
		t.Fatalf("default Gateway configuration sets a pairing option:\n%s", cfg)
	}
	opts := releaseOptions(t, "")
	opts.LocalAutoEntry = true
	dep, err = Initialize(opts)
	if err != nil {
		t.Fatal(err)
	}
	if cfg := readFile(t, filepath.Join(dep.StateDir, "gateway.json")); !strings.Contains(cfg, `"localAutoEntry": true`) {
		t.Fatalf("opt-in was not written to the Gateway configuration:\n%s", cfg)
	}
}

func TestServerProfileEnablesSandboxPairingAndLimits(t *testing.T) {
	opts := releaseOptions(t, "")
	opts.Profile = ProfileServer
	dep, err := Initialize(opts)
	if err != nil {
		t.Fatal(err)
	}
	runtimeEnv := readFile(t, filepath.Join(dep.StateDir, "..", RuntimeEnvFile))
	if !strings.Contains(runtimeEnv, "LENSDOCK_CHROMIUM_SANDBOX=true\n") || !strings.HasPrefix(runtimeEnv, "LENSDOCK_OPERATOR_TOKEN_SHA256=") {
		t.Fatalf("runtime.env:\n%s", runtimeEnv)
	}
	composeEnv := readFile(t, filepath.Join(dep.StateDir, "compose.env"))
	for _, want := range []string{
		"LENSDOCK_REQUIRE_PAIRING=1\n", "LENSDOCK_RUNTIME_MEMORY_LIMIT=4g\n", "LENSDOCK_RUNTIME_CPUS=2\n", "LENSDOCK_RUNTIME_PIDS_LIMIT=2048\n",
		"LENSDOCK_GATEWAY_MEMORY_LIMIT=1g\n", "LENSDOCK_GATEWAY_CPUS=1\n", "LENSDOCK_GATEWAY_PIDS_LIMIT=512\n",
	} {
		if !strings.Contains(composeEnv, want) {
			t.Errorf("compose.env lacks %q:\n%s", want, composeEnv)
		}
	}
	// The values must be readable by the same parser that Compose invocations use.
	env := map[string]string{}
	for _, kv := range ComposeEnv(dep) {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	if env["LENSDOCK_RUNTIME_MEMORY_LIMIT"] != "4g" || env["LENSDOCK_REQUIRE_PAIRING"] != "1" {
		t.Fatalf("parsed compose env: %v", env)
	}
}

func TestDefaultProfileAddsNothing(t *testing.T) {
	dep, err := Initialize(releaseOptions(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	all := readFile(t, filepath.Join(dep.StateDir, "..", RuntimeEnvFile)) + readFile(t, filepath.Join(dep.StateDir, "compose.env"))
	for _, forbidden := range []string{"CHROMIUM_SANDBOX", "MEMORY_LIMIT", "CPUS", "PIDS_LIMIT", "REQUIRE_PAIRING"} {
		if strings.Contains(all, forbidden) {
			t.Errorf("the default profile wrote %s", forbidden)
		}
	}
}

func TestUnknownProfileIsRejectedBeforeAnyStateExists(t *testing.T) {
	opts := releaseOptions(t, "")
	opts.Profile = "cloud"
	if _, err := Initialize(opts); err == nil || !strings.Contains(err.Error(), "profile") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(opts.StateDir); !os.IsNotExist(err) {
		t.Fatalf("state was created for an invalid profile: %v", err)
	}
}

// An existing runtime.env is never rewritten: a profile that needs lines it lacks
// is an error, one that already has them is fine (an interrupted init can resume).
func TestServerProfileNeverRewritesAnExistingRuntimeEnv(t *testing.T) {
	opts := releaseOptions(t, "")
	if err := os.MkdirAll(opts.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureOperatorCredentials(opts.StateDir); err != nil {
		t.Fatal(err)
	}
	before := readFile(t, filepath.Join(opts.StateDir, RuntimeEnvFile))
	opts.Profile = ProfileServer
	if _, err := Initialize(opts); err == nil || !strings.Contains(err.Error(), "LENSDOCK_CHROMIUM_SANDBOX=true") {
		t.Fatalf("err = %v", err)
	}
	if after := readFile(t, filepath.Join(opts.StateDir, RuntimeEnvFile)); after != before {
		t.Fatal("an existing runtime.env was modified")
	}
	writeFile(t, filepath.Join(opts.StateDir, RuntimeEnvFile), before+"LENSDOCK_CHROMIUM_SANDBOX=true\n", 0o600)
	if _, err := Initialize(opts); err != nil {
		t.Fatalf("profile with a prepared runtime.env: %v", err)
	}
}
