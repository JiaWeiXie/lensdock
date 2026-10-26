package hostmanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JiaWeiXie/lensdock/internal/update"
)

func TestDefaultPolicyHasNoLimits(t *testing.T) {
	if got := DefaultPolicy(); got != (Policy{AllowUnboundedExternal: true}) {
		t.Fatalf("the default policy must configure no limit and allow uncapped external containers: %+v", got)
	}
	if err := validatePolicy(DefaultPolicy()); err != nil {
		t.Fatalf("an unlimited policy must be valid: %v", err)
	}
	env := newEnv(t, DefaultPolicy())
	cfg, err := LoadConfig(env.cfg.Root)
	if err != nil || cfg.Policy != DefaultPolicy() {
		t.Fatalf("initialization must persist the unlimited policy: %v %+v", err, cfg.Policy)
	}
	if p := env.authorize(); p.Limits != (Limits{}) {
		t.Fatalf("new projects must have no caps by default: %+v", p.Limits)
	}
}

func TestUnlimitedDefaultsStartEverythingDespiteSmallEngineAndUncappedDemand(t *testing.T) {
	env := newEnv(t, DefaultPolicy())
	env.backend.capacity = Resources{MemoryBytes: gib, NanoCPUs: 1_000_000_000}
	env.backend.external = Resources{MemoryBytes: 512 * gib, NanoCPUs: 512_000_000_000}
	env.backend.unboundedMemory = []string{"foreign-db"}
	env.backend.unboundedCPU = []string{"foreign-db"}

	var projects []Project
	for range 4 {
		p := env.authorize()
		projects = append(projects, p)
		settle(env, p)
	}
	for _, p := range projects {
		got := env.status(p)
		if got.State != StateRunning || got.Reserved || got.Reservation != (Resources{}) || got.QueuePosition != 0 {
			t.Fatalf("an unlimited project must run without holding or waiting for capacity: %+v", got)
		}
	}
	if list := env.mustCall(admin, Request{Action: ActionList}); len(list.Warnings) != 0 {
		t.Fatalf("uncapped external containers must not warn or refuse by default: %v", list.Warnings)
	}
}

// dimension describes a one-dimension policy: only memory or only CPU is
// configured, and the other dimension must stay completely unconstrained.
type dimension struct {
	name   string
	policy func(pairs int64) Policy
	on     func(int64) Resources // quantity in the configured dimension
	off    func(int64) Resources // quantity in the unconfigured dimension
	// unbounded marks an external container as uncapped in the configured or
	// the other dimension.
	unbounded func(b *fakeBackend, configured bool)
}

var dimensions = []dimension{
	{
		name: "memory-only",
		policy: func(pairs int64) Policy {
			return Policy{
				Budget:        Resources{MemoryBytes: (2*gib + 256*mib) * pairs},
				ProjectLimits: Limits{Runtime: Resources{MemoryBytes: 2 * gib}, Gateway: Resources{MemoryBytes: 256 * mib}},
			}
		},
		on:  func(n int64) Resources { return Resources{MemoryBytes: n} },
		off: func(n int64) Resources { return Resources{NanoCPUs: n} },
		unbounded: func(b *fakeBackend, configured bool) {
			if configured {
				b.unboundedMemory = []string{"foreign-db"}
			} else {
				b.unboundedCPU = []string{"foreign-db"}
			}
		},
	},
	{
		name: "cpu-only",
		policy: func(pairs int64) Policy {
			return Policy{
				Budget:        Resources{NanoCPUs: 2_500_000_000 * pairs},
				ProjectLimits: Limits{Runtime: Resources{NanoCPUs: 2_000_000_000}, Gateway: Resources{NanoCPUs: 500_000_000}},
			}
		},
		on:  func(n int64) Resources { return Resources{NanoCPUs: n} },
		off: func(n int64) Resources { return Resources{MemoryBytes: n} },
		unbounded: func(b *fakeBackend, configured bool) {
			if configured {
				b.unboundedCPU = []string{"foreign-db"}
			} else {
				b.unboundedMemory = []string{"foreign-db"}
			}
		},
	},
}

func TestPartialPolicyEnforcesOnlyItsOwnDimension(t *testing.T) {
	for _, d := range dimensions {
		t.Run(d.name, func(t *testing.T) {
			if err := validatePolicy(d.policy(1)); err != nil {
				t.Fatal(err)
			}
			// The other dimension is absurd: a tiny engine, huge external demand
			// and an uncapped container. None of it may matter.
			env := newEnv(t, d.policy(1))
			env.backend.capacity = addRes(d.on(1<<40), d.off(1))
			env.backend.external = d.off(1 << 40)
			d.unbounded(env.backend, false)
			a, b := env.authorize(), env.authorize()
			settle(env, a)
			if got := env.status(a); !got.Reserved || got.Reservation != pairOf(a.Limits) {
				t.Fatalf("the configured dimension must be reserved: %+v", got)
			}
			env.ensure(b)
			env.pass()
			if got := env.status(b); got.State != StateQueued || !strings.Contains(got.Blocker, "budget") {
				t.Fatalf("the configured budget must hold the second project: %+v", got)
			}
			env.stopProject(a)
			for range 4 {
				env.pass()
			}
			if got := env.status(b); got.State != StateRunning {
				t.Fatalf("freed budget must start the queued project: %+v", got)
			}

			// An uncapped container in the configured dimension is refused as before.
			refused := newEnv(t, d.policy(1))
			d.unbounded(refused.backend, true)
			p := refused.authorize()
			r := refused.ensure(p)
			refused.pass()
			if op := refused.op(r.Operation.ID); op.Phase != PhaseFailed || !strings.Contains(op.Error, "resource caps") {
				t.Fatalf("uncapped demand in the configured dimension must refuse admission: %+v", op)
			}

			// The engine must physically hold the configured dimension.
			small := newEnv(t, d.policy(1))
			small.backend.capacity = d.on(1 << 30)
			p = small.authorize()
			r = small.ensure(p)
			small.pass()
			if op := small.op(r.Operation.ID); op.Phase != PhaseFailed || !strings.Contains(op.Error, "engine cannot hold") {
				t.Fatalf("physical capacity of the configured dimension must be checked: %+v", op)
			}
		})
	}
}

func TestPolicyValidationTreatsEveryQuantityAsOptional(t *testing.T) {
	valid := map[string]Policy{
		"nothing":            {},
		"helper memory only": {Helpers: Resources{MemoryBytes: gib}},
		"helper CPU only":    {Helpers: Resources{NanoCPUs: 500_000_000}},
		"headroom only":      {Headroom: Resources{MemoryBytes: gib}},
		"project memory only": {
			ProjectLimits: Limits{Runtime: Resources{MemoryBytes: gib}},
		},
		"explicit allowance": {AllowUnboundedExternal: true, UnboundedExternalAllowance: Resources{MemoryBytes: gib}},
		"restricted":         restrictedPolicy(),
	}
	for name, p := range valid {
		if err := validatePolicy(p); err != nil {
			t.Errorf("%s must be valid: %v", name, err)
		}
	}
	invalid := map[string]Policy{
		"negative budget":      {Budget: Resources{MemoryBytes: -1}},
		"negative helper":      {Helpers: Resources{NanoCPUs: -1}},
		"absurd quantity":      {Headroom: Resources{MemoryBytes: maxQuantity + 1}},
		"allowance not opted":  {UnboundedExternalAllowance: Resources{MemoryBytes: gib}},
		"budget without caps":  {Budget: Resources{MemoryBytes: gib}},
		"budget missing a cap": {Budget: Resources{MemoryBytes: 4 * gib}, ProjectLimits: Limits{Runtime: Resources{MemoryBytes: gib}}},
		"pair above budget": {
			Budget:        Resources{MemoryBytes: gib},
			ProjectLimits: Limits{Runtime: Resources{MemoryBytes: gib}, Gateway: Resources{MemoryBytes: gib}},
		},
	}
	for name, p := range invalid {
		if err := validatePolicy(p); err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
}

func TestPolicyBudgetRequiresCapsOnExistingProjects(t *testing.T) {
	env := newEnv(t, DefaultPolicy())
	p := env.authorize()
	budgeted := Policy{
		Budget:        Resources{MemoryBytes: 4 * gib},
		ProjectLimits: Limits{Runtime: Resources{MemoryBytes: gib}, Gateway: Resources{MemoryBytes: 256 * mib}},
	}
	_, err := env.call(admin, Request{Action: ActionPolicy, Policy: &budgeted})
	wantStatus(t, err, http.StatusConflict)
	if cfg, err := LoadConfig(env.cfg.Root); err != nil || cfg.Policy != DefaultPolicy() {
		t.Fatalf("a refused policy must not persist: %v %+v", err, cfg.Policy)
	}
	env.mustCall(admin, Request{Action: ActionLimits, ProjectID: p.ID, Limits: &budgeted.ProjectLimits})
	env.mustCall(admin, Request{Action: ActionPolicy, Policy: &budgeted})
}

func TestNewProjectsInheritOptInProjectLimits(t *testing.T) {
	policy := dimensions[0].policy(2)
	env := newEnv(t, policy)
	if p := env.authorize(); p.Limits != policy.ProjectLimits {
		t.Fatalf("an authorization must take the configured project limits: %+v", p.Limits)
	}
}

// legacyProfile renders a resource profile exactly as releases before optional
// limits did.
func legacyProfile(cfg Config, p Project) string {
	var text strings.Builder
	text.WriteString("services:\n")
	for _, svc := range []struct {
		name string
		r    Resources
	}{{"runtime", p.Limits.Runtime}, {"gateway", p.Limits.Gateway}} {
		fmt.Fprintf(&text, "  %s:\n    mem_limit: %d\n    memswap_limit: %d\n    cpus: %.9f\n    labels:\n      %s: %q\n      %s: %q\n", svc.name, svc.r.MemoryBytes, svc.r.MemoryBytes, float64(svc.r.NanoCPUs)/1e9, managerLabel, cfg.ID, projectLabel, p.ID)
	}
	fmt.Fprintf(&text, "  config-init:\n    mem_limit: ${LENSDOCK_MANAGER_HELPER_MEMORY:?managed helper policy is required}\n    memswap_limit: ${LENSDOCK_MANAGER_HELPER_MEMORY:?managed helper policy is required}\n    cpus: ${LENSDOCK_MANAGER_HELPER_CPUS:?managed helper policy is required}\n    labels:\n      %s: %q\n      %s: %q\n      %s: \"true\"\n", managerLabel, cfg.ID, projectLabel, p.ID, helperLabel)
	return text.String()
}

func TestResourceProfileKeepsExplicitProfilesAndDisablesOmittedCaps(t *testing.T) {
	env := newEnv(t, testPolicy(2))
	p := env.authorize()
	if got, want := resourceProfile(env.cfg, p), legacyProfile(env.cfg, p); got != want {
		t.Fatalf("an explicit policy's profile must stay byte-identical:\n%s\nwant\n%s", got, want)
	}

	unlimited := p
	unlimited.Limits = Limits{}
	profile := resourceProfile(env.cfg, unlimited)
	for _, svc := range []string{"runtime", "gateway"} {
		block := "  " + svc + ":\n    mem_limit: 0\n    memswap_limit: 0\n    cpus: 0\n"
		if !strings.Contains(profile, block) {
			t.Fatalf("%s must explicitly disable the base file's default caps:\n%s", svc, profile)
		}
	}

	partial := p
	partial.Limits = Limits{Runtime: Resources{MemoryBytes: 2 * gib}, Gateway: Resources{NanoCPUs: 500_000_000}}
	profile = resourceProfile(env.cfg, partial)
	for _, want := range []string{
		"  runtime:\n    mem_limit: 2147483648\n    memswap_limit: 2147483648\n    cpus: 0\n",
		"  gateway:\n    mem_limit: 0\n    memswap_limit: 0\n    cpus: 0.500000000\n",
	} {
		if !strings.Contains(profile, want) {
			t.Fatalf("partial profile lacks %q:\n%s", want, profile)
		}
	}
}

// recordingDocker is a Docker stand-in that records its arguments and the
// helper policy it received through the environment.
func recordingDocker(t *testing.T) (binary, output string) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ldd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	output = filepath.Join(dir, "out")
	binary = filepath.Join(dir, "docker")
	script := "#!/bin/sh\n{ for a in \"$@\"; do printf 'ARG=%s\\n' \"$a\"; done; env | grep '^LENSDOCK_MANAGER_HELPER_'; } > " + output + "\nexit 0\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return binary, output
}

func runRecorded(t *testing.T, helpers Resources, args ...string) ([]string, error) {
	return runRecordedPolicy(t, Policy{Helpers: helpers}, args...)
}

func runRecordedPolicy(t *testing.T, policy Policy, args ...string) ([]string, error) {
	t.Helper()
	binary, output := recordingDocker(t)
	cfg := Config{
		ID: "mgr1", Root: t.TempDir(), DockerBinary: binary, DockerHost: "unix:///tmp/ld-test.sock", DockerConfig: t.TempDir(),
		Policy: policy,
	}
	lock, err := os.CreateTemp(t.TempDir(), "lock")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	err = fixedDockerRunner{cfg: cfg, locks: []*os.File{lock}}.Run(context.Background(), update.Cmd{Name: binary, Args: args})
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimSpace(string(mustRead(t, output))), "\n"), nil
}

func hasLine(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}

func TestHelperContainersOmitCapsWhenTheAllowanceIsUnlimited(t *testing.T) {
	lines, err := runRecorded(t, Resources{}, "run", "--rm", "image:test")
	if err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"ARG=--memory", "ARG=--memory-swap", "ARG=--cpus"} {
		if hasLine(lines, flag) {
			t.Fatalf("an unlimited helper must not receive %s: %v", flag, lines)
		}
	}
	if !hasLine(lines, "ARG=--label") || !hasLine(lines, "ARG=io.lensdock.manager-helper=true") {
		t.Fatalf("helper ownership labels are not a limit and must remain: %v", lines)
	}

	lines, err = runRecorded(t, Resources{}, "compose", "up")
	if err != nil {
		t.Fatal(err)
	}
	if !hasLine(lines, "LENSDOCK_MANAGER_HELPER_MEMORY=0") || !hasLine(lines, "LENSDOCK_MANAGER_HELPER_CPUS=0") || !hasLine(lines, "LENSDOCK_MANAGER_HELPER_PIDS=0") {
		t.Fatalf("Compose helpers must be told to run uncapped: %v", lines)
	}
}

func TestHelperContainersEnforceConfiguredDimensionsOnly(t *testing.T) {
	lines, err := runRecorded(t, Resources{MemoryBytes: 512 * mib}, "run", "--rm", "image:test")
	if err != nil {
		t.Fatal(err)
	}
	if !hasLine(lines, "ARG=--memory") || !hasLine(lines, "ARG=536870912") || !hasLine(lines, "ARG=--memory-swap") || hasLine(lines, "ARG=--cpus") {
		t.Fatalf("memory-only allowance must cap memory only: %v", lines)
	}
	lines, err = runRecorded(t, Resources{NanoCPUs: 500_000_000}, "run", "--rm", "image:test")
	if err != nil {
		t.Fatal(err)
	}
	if hasLine(lines, "ARG=--memory") || !hasLine(lines, "ARG=--cpus") || !hasLine(lines, "ARG=0.500000000") {
		t.Fatalf("CPU-only allowance must cap CPU only: %v", lines)
	}
	lines, err = runRecorded(t, Resources{MemoryBytes: gib, NanoCPUs: 1_000_000_000}, "compose", "up")
	if err != nil {
		t.Fatal(err)
	}
	if !hasLine(lines, "LENSDOCK_MANAGER_HELPER_MEMORY=1073741824") || !hasLine(lines, "LENSDOCK_MANAGER_HELPER_CPUS=1.000000000") {
		t.Fatalf("explicit allowances must reach Compose unchanged: %v", lines)
	}
}

func TestExplicitHelperAllowanceStillBoundsCallerMemory(t *testing.T) {
	if _, err := runRecorded(t, Resources{MemoryBytes: 512 * mib}, "run", "--memory", "1g", "image:test"); err == nil {
		t.Fatal("a helper above the configured allowance must be refused")
	}
	if _, err := runRecorded(t, Resources{MemoryBytes: 512 * mib}, "run", "--memory", "0", "image:test"); err == nil {
		t.Fatal("a caller must not lift a configured allowance with --memory 0")
	}
	lines, err := runRecorded(t, Resources{MemoryBytes: 512 * mib}, "run", "--memory", "256m", "image:test")
	if err != nil || !hasLine(lines, "ARG=268435456") {
		t.Fatalf("a caller may tighten the allowance: %v %v", lines, err)
	}
	lines, err = runRecorded(t, Resources{}, "run", "--memory", "256m", "image:test")
	if err != nil || !hasLine(lines, "ARG=268435456") {
		t.Fatalf("an explicit caller cap is honored when no allowance is configured: %v %v", lines, err)
	}
}

// inspectingDocker answers the read-only engine, container list and inspect
// queries of DockerBackend.Inspect from fixed JSON.
func inspectingDocker(t *testing.T, containers string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ldi")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	inspect := filepath.Join(dir, "inspect.json")
	if err := os.WriteFile(inspect, []byte(containers), 0o600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "docker")
	script := "#!/bin/sh\nshift 4\ncase \"$1\" in\n" +
		"info) echo '{\"ID\":\"engine-1\",\"MemTotal\":17179869184,\"NCPU\":8,\"OSType\":\"linux\"}';;\n" +
		"ps) echo c1; echo c2; echo c3;;\n" +
		"container) cat " + inspect + ";;\n" +
		"esac\nexit 0\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return binary
}

const inspectFixture = `[
 {"Id":"c1","Name":"/uncapped-db","Config":{"Labels":{}},"State":{"Running":true},"HostConfig":{}},
 {"Id":"c2","Name":"/memory-capped","Config":{"Labels":{}},"State":{"Running":true},"HostConfig":{"Memory":1073741824}},
 {"Id":"c3","Name":"/owned-runtime","Config":{"Labels":{"io.lensdock.manager-id":"mgr1","io.lensdock.project-id":"0123456789abcdef","com.docker.compose.project":"ld-0123456789abcdef","com.docker.compose.service":"runtime"}},"State":{"Running":true,"Health":{"Status":"healthy"}},"HostConfig":{}}
]`

func inspectProject(t *testing.T, policy Policy, limits Limits) (Inventory, error) {
	t.Helper()
	cfg := Config{ID: "mgr1", Root: t.TempDir(), DockerBinary: inspectingDocker(t, inspectFixture), DockerHost: "unix:///tmp/ld-test.sock", DockerConfig: t.TempDir(), EngineID: "engine-1", Policy: policy}
	p := Project{ID: "0123456789abcdef", ComposeProject: "ld-0123456789abcdef", Port: 19090, Limits: limits}
	return DockerBackend{}.Inspect(context.Background(), cfg, []Project{p})
}

func TestInspectionReportsUncappedExternalContainersPerDimension(t *testing.T) {
	inv, err := inspectProject(t, Policy{}, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(inv.UnboundedMemory, ","); got != "uncapped-db" {
		t.Fatalf("memory-uncapped containers = %q", got)
	}
	if got := strings.Join(inv.UnboundedCPU, ","); got != "uncapped-db,memory-capped" {
		t.Fatalf("CPU-uncapped containers = %q", got)
	}
	if inv.External.MemoryBytes != gib || inv.External.NanoCPUs != 0 {
		t.Fatalf("only declared caps count as external demand: %+v", inv.External)
	}
}

func TestInspectionOnlyRequiresConfiguredProjectCaps(t *testing.T) {
	inv, err := inspectProject(t, Policy{}, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if obs := inv.Projects["0123456789abcdef"]; obs.State != StatePartial || obs.Blocker != "" && strings.Contains(obs.Blocker, "caps") {
		t.Fatalf("an unlimited project's uncapped container must not make it unknown: %+v", obs)
	}
	for name, limits := range map[string]Limits{
		"memory": {Runtime: Resources{MemoryBytes: gib}},
		"CPU":    {Runtime: Resources{NanoCPUs: 1_000_000_000}},
	} {
		inv, err := inspectProject(t, Policy{}, limits)
		if err != nil {
			t.Fatal(err)
		}
		if obs := inv.Projects["0123456789abcdef"]; obs.State != StateUnknown || !strings.Contains(obs.Blocker, "caps") {
			t.Fatalf("a configured %s cap that the container lacks must be reported: %+v", name, obs)
		}
	}
}

func TestInspectionHelperCapsFollowTheConfiguredAllowance(t *testing.T) {
	const helper = `[{"Id":"c1","Name":"/h1","Config":{"Labels":{"io.lensdock.manager-id":"mgr1","io.lensdock.manager-helper":"true"}},"State":{"Running":true},"HostConfig":{"Memory":536870912}},
 {"Id":"c2","Name":"/x","Config":{"Labels":{}},"State":{"Running":false},"HostConfig":{}},
 {"Id":"c3","Name":"/y","Config":{"Labels":{}},"State":{"Running":false},"HostConfig":{}}]`
	run := func(allowance Resources) error {
		cfg := Config{ID: "mgr1", Root: t.TempDir(), DockerBinary: inspectingDocker(t, helper), DockerHost: "unix:///tmp/ld-test.sock", DockerConfig: t.TempDir(), EngineID: "engine-1", Policy: Policy{Helpers: allowance}}
		_, err := DockerBackend{}.Inspect(context.Background(), cfg, nil)
		return err
	}
	if err := run(Resources{}); err != nil {
		t.Fatalf("a capped helper under an unlimited allowance is fine: %v", err)
	}
	if err := run(Resources{MemoryBytes: 512 * mib}); err != nil {
		t.Fatalf("a helper within its memory-only allowance is fine: %v", err)
	}
	if err := run(Resources{MemoryBytes: 256 * mib}); err == nil {
		t.Fatal("a helper above the configured allowance must be refused")
	}
	if err := run(Resources{NanoCPUs: 1_000_000_000}); err == nil {
		t.Fatal("a helper without the configured CPU cap must be refused")
	}
}

// limitsOf is a deliberately reconfigured Runtime/Gateway pair.
var limitsOf = func(runtimeMem, gatewayMem int64) Limits {
	return Limits{Runtime: Resources{MemoryBytes: runtimeMem, NanoCPUs: 1_000_000_000}, Gateway: Resources{MemoryBytes: gatewayMem, NanoCPUs: 250_000_000}}
}

func TestLimitsActionReconfiguresAStoppedProject(t *testing.T) {
	env := newEnv(t, DefaultPolicy())
	p := env.authorize()
	want := limitsOf(gib, 128*mib)
	resp := env.mustCall(admin, Request{Action: ActionLimits, ProjectID: p.ID, Limits: &want})
	if resp.Project == nil || resp.Project.Limits != want {
		t.Fatalf("the response must carry the new limits: %+v", resp.Project)
	}

	restarted := env.startServer()
	got, ok := restarted.projectLocked(p.ID)
	if !ok || got.Limits != want {
		t.Fatalf("the new limits must survive a manager restart: %+v", got.Limits)
	}

	settle(env, p)
	if st := env.status(p); st.Reservation != pairOf(want) || st.Limits != want {
		t.Fatalf("the next start must reserve the new limits: %+v", st)
	}

	// Removing every cap is deliberate too, after the project stopped again.
	env.stopProject(p)
	env.pass()
	env.pass()
	none := Limits{}
	env.mustCall(admin, Request{Action: ActionLimits, ProjectID: p.ID, Limits: &none})
	if st := env.status(p); st.Limits != none {
		t.Fatalf("zero must remove the caps: %+v", st.Limits)
	}
}

func TestLimitsActionIsAdminOnlyAndValidated(t *testing.T) {
	env := newEnv(t, DefaultPolicy())
	p := env.authorize()
	ok := limitsOf(gib, 128*mib)
	_, err := env.call(principal{projectID: p.ID}, Request{Action: ActionLimits, ProjectID: p.ID, Limits: &ok})
	wantStatus(t, err, http.StatusForbidden)
	_, err = env.call(admin, Request{Action: ActionLimits, ProjectID: p.ID})
	wantStatus(t, err, http.StatusBadRequest)
	negative := Limits{Runtime: Resources{MemoryBytes: -1}}
	_, err = env.call(admin, Request{Action: ActionLimits, ProjectID: p.ID, Limits: &negative})
	wantStatus(t, err, http.StatusBadRequest)
	absurd := Limits{Gateway: Resources{NanoCPUs: maxQuantity + 1}}
	_, err = env.call(admin, Request{Action: ActionLimits, ProjectID: p.ID, Limits: &absurd})
	wantStatus(t, err, http.StatusBadRequest)
	_, err = env.call(admin, Request{Action: ActionLimits, ProjectID: "0123456789abcdef", Limits: &ok})
	wantStatus(t, err, http.StatusNotFound)
	if st := env.status(p); st.Limits != (Limits{}) {
		t.Fatalf("refused requests must not change limits: %+v", st.Limits)
	}
}

func TestLimitsActionNeverResizesARunningOrQueuedProject(t *testing.T) {
	env := newEnv(t, testPolicy(2))
	running := env.authorize()
	settle(env, running)
	want := limitsOf(gib, 128*mib)
	_, err := env.call(admin, Request{Action: ActionLimits, ProjectID: running.ID, Limits: &want})
	wantStatus(t, err, http.StatusConflict)
	if st := env.status(running); st.Limits != running.Limits || st.State != StateRunning {
		t.Fatalf("a running browser must not be resized: %+v", st)
	}

	env.runInline = false
	queued := env.authorize()
	r := env.ensure(queued)
	for range 2 {
		_, err = env.call(admin, Request{Action: ActionLimits, ProjectID: queued.ID, Limits: &want})
		wantStatus(t, err, http.StatusConflict)
		env.pass()
	}
	if op := env.op(r.Operation.ID); op.Phase == PhaseFailed || op.Phase == PhaseCanceled {
		t.Fatalf("a refused reconfiguration must not disturb the pending start: %+v", op)
	}
	if st := env.status(queued); st.Limits != queued.Limits {
		t.Fatalf("a queued or admitted project must keep its limits: %+v", st.Limits)
	}
}

func TestLimitsActionRefusesWhenStoppedCannotBeConfirmed(t *testing.T) {
	env := newEnv(t, DefaultPolicy())
	p := env.authorize()
	env.backend.mu.Lock()
	env.backend.inspectErr = errors.New("daemon down")
	env.backend.mu.Unlock()
	want := limitsOf(gib, 128*mib)
	_, err := env.call(admin, Request{Action: ActionLimits, ProjectID: p.ID, Limits: &want})
	wantStatus(t, err, http.StatusConflict)

	env.backend.mu.Lock()
	env.backend.inspectErr = nil
	env.backend.mu.Unlock()
	env.backend.set(p.ID, Observation{State: StateUnknown, Blocker: "unrecognized"})
	_, err = env.call(admin, Request{Action: ActionLimits, ProjectID: p.ID, Limits: &want})
	wantStatus(t, err, http.StatusConflict)
	if st := env.status(p); st.Limits != (Limits{}) {
		t.Fatalf("an unconfirmed state must keep the limits: %+v", st.Limits)
	}
}

func TestLimitsActionWaitsForAnApprovalPendingUpdate(t *testing.T) {
	env := newEnv(t, DefaultPolicy())
	p := env.authorize()
	settle(env, p)
	stopAwaitingApproval(env, p)
	want := limitsOf(gib, 128*mib)
	_, err := env.call(admin, Request{Action: ActionLimits, ProjectID: p.ID, Limits: &want})
	wantStatus(t, err, http.StatusConflict)
}

func TestLimitsActionHonorsAnExplicitBudget(t *testing.T) {
	env := newEnv(t, testPolicy(1))
	p := env.authorize()
	for name, bad := range map[string]Limits{
		"above the budget":         limitsOf(64*gib, 128*mib),
		"uncapped budgeted memory": {Runtime: Resources{NanoCPUs: 1_000_000_000}, Gateway: Resources{NanoCPUs: 1_000_000_000}},
	} {
		_, err := env.call(admin, Request{Action: ActionLimits, ProjectID: p.ID, Limits: &bad})
		if statusCode(err) != http.StatusBadRequest {
			t.Errorf("%s must be refused as a bad request, got %v", name, err)
		}
	}
	if st := env.status(p); st.Limits != p.Limits {
		t.Fatalf("refused limits must not apply: %+v", st.Limits)
	}
	smaller := limitsOf(gib, 128*mib)
	env.mustCall(admin, Request{Action: ActionLimits, ProjectID: p.ID, Limits: &smaller})
}

func TestLimitsActionReconcilesTheResourceProfileWithoutBypassingTamperDetection(t *testing.T) {
	env := newEnv(t, DefaultPolicy())
	p := env.authorize()
	state := filepath.Join(p.StateDir, "product")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	dep := update.ScaffoldM0(filepath.Join(p.StateDir, "deployment.json"), state, filepath.Join(p.StateDir, "compose.yaml"), filepath.Join(p.StateDir, "bin"))
	if err := ensureResourceProfile(env.cfg, p, dep); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(state, "resources.yaml")

	first := limitsOf(gib, 128*mib)
	env.mustCall(admin, Request{Action: ActionLimits, ProjectID: p.ID, Limits: &first})
	updated := env.status(p)
	if got, want := string(mustRead(t, path)), resourceProfile(env.cfg, updated); got != want {
		t.Fatalf("the established profile must follow the authorized limits:\n%s\nwant\n%s", got, want)
	}
	if err := ensureResourceProfile(env.cfg, updated, dep); err != nil {
		t.Fatalf("the next operation must accept the reconciled profile: %v", err)
	}
	if err := ensureResourceProfile(env.cfg, p, dep); err == nil {
		t.Fatal("the superseded profile must no longer be accepted")
	}

	// A profile that is neither the current nor the requested one is tampering.
	tampered := []byte("services:\n  runtime:\n    privileged: true\n")
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	second := limitsOf(2*gib, 256*mib)
	_, err := env.call(admin, Request{Action: ActionLimits, ProjectID: p.ID, Limits: &second})
	wantStatus(t, err, http.StatusConflict)
	if string(mustRead(t, path)) != string(tampered) || env.status(p).Limits != first {
		t.Fatal("a tampered profile must be left alone and the limits unchanged")
	}

	// An interrupted attempt (profile already rewritten, registry not) completes
	// when the same request is repeated.
	if err := os.WriteFile(path, []byte(resourceProfile(env.cfg, withLimits(updated, second))), 0o600); err != nil {
		t.Fatal(err)
	}
	env.mustCall(admin, Request{Action: ActionLimits, ProjectID: p.ID, Limits: &second})
	if st := env.status(p); st.Limits != second {
		t.Fatalf("repeating the request must finish it: %+v", st.Limits)
	}
}

func withLimits(p Project, l Limits) Project {
	p.Limits = l
	return p
}

func TestReconfiguredLimitsReachTheStartedProject(t *testing.T) {
	env := newEnv(t, DefaultPolicy())
	p := env.authorize()
	want := limitsOf(gib, 128*mib)
	env.mustCall(admin, Request{Action: ActionLimits, ProjectID: p.ID, Limits: &want})
	var started Project
	env.backend.execFn = func(_ context.Context, _ Config, p Project, op Operation, _ []*os.File) error {
		started = p
		env.backend.applyDefault(p, op)
		return nil
	}
	settle(env, p)
	if started.Limits != want {
		t.Fatalf("the worker must start with the reconfigured limits: %+v", started.Limits)
	}
}

func TestOptingIntoOneProjectCapAddsNoUncappedContainerProhibition(t *testing.T) {
	policy := DefaultPolicy()
	policy.ProjectLimits = Limits{Runtime: Resources{MemoryBytes: gib}, Gateway: Resources{MemoryBytes: 256 * mib}}
	env := newEnv(t, policy)
	env.backend.unboundedMemory = []string{"foreign-db"}
	env.backend.unboundedCPU = []string{"foreign-db"}
	settle(env, env.authorize())

	// Prohibiting uncapped external containers remains an explicit option.
	policy.AllowUnboundedExternal = false
	strict := newEnv(t, policy)
	strict.backend.unboundedMemory = []string{"foreign-db"}
	p := strict.authorize()
	r := strict.ensure(p)
	strict.pass()
	if op := strict.op(r.Operation.ID); op.Phase != PhaseFailed || !strings.Contains(op.Error, "resource caps") {
		t.Fatalf("an explicit prohibition must still refuse admission: %+v", op)
	}
}

func TestHelperContainersHaveNoPidCapUnlessThePolicyOptsIn(t *testing.T) {
	lines, err := runRecordedPolicy(t, DefaultPolicy(), "run", "--rm", "image:test")
	if err != nil || hasLine(lines, "ARG=--pids-limit") {
		t.Fatalf("a default helper must not receive a PID cap: %v %v", lines, err)
	}
	capped := DefaultPolicy()
	capped.HelperPidsLimit = 128
	lines, err = runRecordedPolicy(t, capped, "run", "--rm", "image:test")
	if err != nil || !hasLine(lines, "ARG=--pids-limit") || !hasLine(lines, "ARG=128") {
		t.Fatalf("an opted-in PID cap must reach docker run: %v %v", lines, err)
	}
	// The policy owns the cap: a caller or deployment value neither loosens nor
	// adds one.
	lines, err = runRecordedPolicy(t, capped, "run", "--pids-limit", "100000", "--rm", "image:test")
	if err != nil || hasLine(lines, "ARG=100000") || !hasLine(lines, "ARG=128") {
		t.Fatalf("a caller must not override the PID policy: %v %v", lines, err)
	}
	lines, err = runRecordedPolicy(t, DefaultPolicy(), "run", "--pids-limit=7", "--rm", "image:test")
	if err != nil || hasPidsFlag(lines) {
		t.Fatalf("an unlimited policy must not inherit a caller cap: %v %v", lines, err)
	}

	if err := validatePolicy(Policy{HelperPidsLimit: -1}); err == nil {
		t.Fatal("a negative PID limit must be refused")
	}
	env := newEnv(t, DefaultPolicy())
	helper, err := AcquireHelper(context.Background(), env.cfg.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer helper.Close()
	next := DefaultPolicy()
	next.HelperPidsLimit = 64
	_, err = env.call(admin, Request{Action: ActionPolicy, Policy: &next})
	wantStatus(t, err, http.StatusConflict)
}

func hasPidsFlag(lines []string) bool {
	for _, l := range lines {
		if strings.HasPrefix(l, "ARG=--pids-limit") || l == "ARG=7" {
			return true
		}
	}
	return false
}

func TestLimitsActionRefusesAStaleSnapshotBeforeMutatingAnything(t *testing.T) {
	env := newEnv(t, DefaultPolicy())
	p := env.authorize()
	state := filepath.Join(p.StateDir, "product")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(state, "resources.yaml")
	if err := os.WriteFile(profile, []byte(resourceProfile(env.cfg, p)), 0o600); err != nil {
		t.Fatal(err)
	}

	original := p.Limits
	first := limitsOf(gib, 128*mib)
	env.mustCall(admin, Request{Action: ActionLimits, ProjectID: p.ID, Limits: &first, ExpectedLimits: &original})
	afterFirst := mustRead(t, profile)

	// A writer that still holds the pre-change snapshot is refused whole.
	second := limitsOf(2*gib, 256*mib)
	_, err := env.call(admin, Request{Action: ActionLimits, ProjectID: p.ID, Limits: &second, ExpectedLimits: &original})
	wantStatus(t, err, http.StatusConflict)
	if st := env.status(p); st.Limits != first || string(mustRead(t, profile)) != string(afterFirst) {
		t.Fatalf("a stale snapshot must change neither the limits nor the profile: %+v", st.Limits)
	}

	// The same request with the current snapshot succeeds.
	env.mustCall(admin, Request{Action: ActionLimits, ProjectID: p.ID, Limits: &second, ExpectedLimits: &first})
	if st := env.status(p); st.Limits != second {
		t.Fatalf("a current snapshot must apply: %+v", st.Limits)
	}
}

func TestConcurrentPartialLimitUpdatesNeverLoseEachOther(t *testing.T) {
	env := newEnv(t, DefaultPolicy())
	p := env.authorize()
	edits := []struct {
		name  string
		apply func(*Limits)
	}{
		{"runtime memory", func(l *Limits) { l.Runtime.MemoryBytes = gib }},
		{"runtime CPU", func(l *Limits) { l.Runtime.NanoCPUs = 1_500_000_000 }},
		{"gateway memory", func(l *Limits) { l.Gateway.MemoryBytes = 256 * mib }},
		{"gateway CPU", func(l *Limits) { l.Gateway.NanoCPUs = 500_000_000 }},
	}
	results := make(chan error, len(edits))
	for _, edit := range edits {
		go func() {
			// A partial-flag client: read the project, change one dimension and
			// send the whole value with the snapshot it read; retry on a refusal
			// caused by someone else's change.
			for range 500 {
				current, err := env.call(admin, Request{Action: ActionStatus, ProjectID: p.ID})
				if err != nil {
					results <- err
					return
				}
				observed := current.Project.Limits
				next := observed
				edit.apply(&next)
				_, err = env.call(admin, Request{Action: ActionLimits, ProjectID: p.ID, Limits: &next, ExpectedLimits: &observed})
				if err == nil {
					results <- nil
					return
				}
				if statusCode(err) != http.StatusConflict {
					results <- fmt.Errorf("%s: %w", edit.name, err)
					return
				}
			}
			results <- fmt.Errorf("%s never succeeded", edit.name)
		}()
	}
	for range edits {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	want := Limits{
		Runtime: Resources{MemoryBytes: gib, NanoCPUs: 1_500_000_000},
		Gateway: Resources{MemoryBytes: 256 * mib, NanoCPUs: 500_000_000},
	}
	if got := env.status(p).Limits; got != want {
		t.Fatalf("a partial update was lost: %+v want %+v", got, want)
	}
}

// legacyComposeBase stands in for a pinned release copy whose config-init
// predates any PID setting.
const legacyComposeBase = `services:
  config-init:
    image: example/gateway:test
    profiles: [setup]
  runtime:
    image: example/runtime:test
  gateway:
    image: example/gateway:test
`

// resolvedConfigInitPids resolves the files through real Docker Compose and
// returns config-init's pids_limit (0 when the cap is absent).
func resolvedConfigInitPids(t *testing.T, files []string, env []string) int64 {
	t.Helper()
	args := []string{"compose", "--profile", "setup", "-p", "lensdock-test"}
	for _, f := range files {
		args = append(args, "-f", f)
	}
	args = append(args, "config", "--format", "json")
	var process []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "LENSDOCK_") && !strings.HasPrefix(e, "COMPOSE_") {
			process = append(process, e)
		}
	}
	cmd := exec.Command("docker", args...)
	cmd.Env = append(process, env...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("docker compose config: %v", err)
	}
	var cfg struct {
		Services map[string]struct {
			Pids int64 `json:"pids_limit"`
		} `json:"services"`
	}
	if err := json.Unmarshal(out, &cfg); err != nil {
		t.Fatal(err)
	}
	svc, ok := cfg.Services["config-init"]
	if !ok {
		t.Fatal("the setup profile must resolve config-init")
	}
	return svc.Pids
}

func TestManagedConfigInitPidCapFollowsPolicyWithoutRewritingTheProfile(t *testing.T) {
	if err := exec.Command("docker", "compose", "version").Run(); err != nil {
		t.Skip("docker compose is not available")
	}
	stateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stateDir, "runtime.env"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(t.TempDir(), "compose.yaml")
	if err := os.WriteFile(legacy, []byte(legacyComposeBase), 0o600); err != nil {
		t.Fatal(err)
	}
	deployment := []string{"LENSDOCK_STATE_DIR=" + stateDir, "LENSDOCK_HELPER_PIDS_LIMIT=100000"}
	current, err := filepath.Abs(filepath.Join("..", "..", "docker", "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	for name, base := range map[string]string{
		"current bundle":                     current,
		"release copy predating the setting": legacy,
	} {
		t.Run(name, func(t *testing.T) {
			env := newEnv(t, testPolicy(2))
			p := env.authorize()
			state := filepath.Join(p.StateDir, "product")
			if err := os.MkdirAll(state, 0o700); err != nil {
				t.Fatal(err)
			}
			dep := update.ScaffoldM0(filepath.Join(p.StateDir, "deployment.json"), state, base, filepath.Join(p.StateDir, "bin"))
			if err := ensureResourceProfile(env.cfg, p, dep); err != nil {
				t.Fatal(err)
			}
			resources := mustRead(t, filepath.Join(state, "resources.yaml"))
			policyFile := mustRead(t, filepath.Join(state, "helper-policy.yaml"))

			for _, limit := range []int64{64, 0, 16} {
				policy := env.cfg.Policy
				policy.HelperPidsLimit = limit
				got := resolvedConfigInitPids(t, dep.Compose.Files, append(append([]string{}, deployment...), composeHelperPolicyEnv(policy)...))
				if got != limit {
					t.Fatalf("manager PID policy %d resolved config-init pids_limit %d; a deployment value of 100000 must not win", limit, got)
				}
				if err := ensureResourceProfile(env.cfg, p, dep); err != nil {
					t.Fatalf("a helper policy change must not prevent the next operation: %v", err)
				}
				if string(mustRead(t, filepath.Join(state, "resources.yaml"))) != string(resources) || string(mustRead(t, filepath.Join(state, "helper-policy.yaml"))) != string(policyFile) {
					t.Fatal("a helper PID policy change rewrote an immutable profile file")
				}
			}
			if len(dep.Compose.Files) != 3 {
				t.Fatalf("repeated operations must not relist profile files: %v", dep.Compose.Files)
			}

			tampered := []byte("services:\n  config-init:\n    pids_limit: 100000\n")
			if err := os.WriteFile(filepath.Join(state, "helper-policy.yaml"), tampered, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := ensureResourceProfile(env.cfg, p, dep); err == nil {
				t.Fatal("a tampered helper policy profile must be refused")
			}
			if string(mustRead(t, filepath.Join(state, "helper-policy.yaml"))) != string(tampered) {
				t.Fatal("a refused profile must be left alone")
			}
		})
	}
}

func TestEnsureResourceProfileAddsTheHelperPolicyToAnEstablishedDeployment(t *testing.T) {
	env := newEnv(t, testPolicy(2))
	p := env.authorize()
	state := filepath.Join(p.StateDir, "product")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	// A deployment established before the helper policy profile existed
	// already lists its authorized resource profile.
	dep := update.ScaffoldM0(filepath.Join(p.StateDir, "deployment.json"), state, filepath.Join(p.StateDir, "compose.yaml"), filepath.Join(p.StateDir, "bin"))
	resources := filepath.Join(state, "resources.yaml")
	if err := os.WriteFile(resources, []byte(resourceProfile(env.cfg, p)), 0o600); err != nil {
		t.Fatal(err)
	}
	dep.Compose.Files = append(dep.Compose.Files, resources)
	before := mustRead(t, resources)
	if err := ensureResourceProfile(env.cfg, p, dep); err != nil {
		t.Fatal(err)
	}
	if string(mustRead(t, resources)) != string(before) {
		t.Fatal("adding the helper policy rewrote the authorized resource profile")
	}
	want := filepath.Join(state, "helper-policy.yaml")
	if n := len(dep.Compose.Files); n != 3 || dep.Compose.Files[2] != want {
		t.Fatalf("the helper policy must follow the existing profile: %v", dep.Compose.Files)
	}
	loaded, err := update.LoadDeployment(dep.File)
	if err != nil || len(loaded.Compose.Files) != 3 {
		t.Fatalf("the added file must be persisted with the deployment: %v %v", err, loaded)
	}
}
