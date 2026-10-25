package hostmanager

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/update"
)

// settle starts a project and runs passes until its worker result is applied.
func settle(env *testEnv, p Project) {
	env.t.Helper()
	env.ensure(p)
	env.pass()
	env.pass()
	if got := env.status(p); got.State != StateRunning {
		env.t.Fatalf("project %s should be running, is %s (%s)", p.ID, got.State, got.Blocker)
	}
}

func opIDs(execs []string) []string {
	var ids []string
	for _, e := range execs {
		_, id, _ := strings.Cut(e, ":")
		ids = append(ids, id)
	}
	return ids
}

func TestStrictFIFOAcrossProjects(t *testing.T) {
	env := newEnv(t, testPolicy(1))
	a, b, c := env.authorize(), env.authorize(), env.authorize()
	ra, rb, rc := env.ensure(a), env.ensure(b), env.ensure(c)
	env.pass()

	if env.op(ra.Operation.ID).Phase != PhaseAdmitted {
		t.Fatalf("the head of the queue must be admitted, got %s", env.op(ra.Operation.ID).Phase)
	}
	gotB, gotC := env.status(b), env.status(c)
	if gotB.State != StateQueued || gotB.QueuePosition != 1 || !strings.Contains(gotB.Blocker, "budget") {
		t.Fatalf("b must be first in the queue waiting for budget: %+v", gotB)
	}
	if gotC.State != StateQueued || gotC.QueuePosition != 2 || !strings.Contains(gotC.Blocker, "behind") {
		t.Fatalf("c must wait behind b: %+v", gotC)
	}
	env.pass()
	if env.op(ra.Operation.ID).Phase != PhaseSucceeded {
		t.Fatalf("the worker receipt must complete the operation, got %s", env.op(ra.Operation.ID).Phase)
	}
	if len(env.backend.executions()) != 1 {
		t.Fatalf("only the head may execute: %v", env.backend.executions())
	}

	env.stopProject(a)
	env.pass()
	env.pass()
	got := opIDs(env.backend.executions())
	if len(got) != 3 || got[0] != ra.Operation.ID || got[2] != rb.Operation.ID {
		t.Fatalf("after the stop, b (not c) must start next: %v", env.backend.executions())
	}
	if env.op(rc.Operation.ID).Phase != PhaseQueued {
		t.Fatalf("c must stay queued, got %s", env.op(rc.Operation.ID).Phase)
	}
	if st := env.status(a); st.Reserved {
		t.Fatalf("a stopped project must release its reservation: %+v", st)
	}
}

func TestSameProjectRequestsCoalesceAndRunningProjectStartsNothing(t *testing.T) {
	env := newEnv(t, testPolicy(2))
	p := env.authorize()
	r1, r2 := env.ensure(p), env.ensure(p)
	if r1.Operation.ID != r2.Operation.ID || r1.Waiter.ID == r2.Waiter.ID {
		t.Fatalf("concurrent ensures share one operation and keep separate waiters: %+v %+v", r1, r2)
	}
	env.pass()
	env.pass()
	if got := env.launchedOps(); len(got) != 1 {
		t.Fatalf("exactly one worker may launch, got %v", got)
	}
	r3 := env.ensure(p)
	if r3.Operation != nil || r3.Project.State != StateRunning {
		t.Fatalf("a running project must not start another worker: %+v", r3)
	}
	env.pass()
	if len(env.launchedOps()) != 1 || len(env.backend.executions()) != 1 {
		t.Fatal("a running project must cause no new execution")
	}
}

func TestUnownedRunningMappingIsUnknownAndRefusesStart(t *testing.T) {
	env := newEnv(t, testPolicy(2))
	p := env.authorize()
	env.backend.set(p.ID, Observation{State: StateRunning, PortOwned: false})
	_, err := env.call(principal{projectID: p.ID}, Request{Action: ActionEnsure})
	wantStatus(t, err, http.StatusConflict)
	if got := env.status(p); got.State != StateUnknown || !got.Reserved {
		t.Fatalf("an unowned mapping is unknown and keeps its accounting: %+v", got)
	}
}

func TestWaiterCancellationHeartbeatAndLiveness(t *testing.T) {
	env := newEnv(t, testPolicy(1))
	a, b := env.authorize(), env.authorize()
	settle(env, a)
	agentB := principal{projectID: b.ID}

	r1, r2 := env.ensure(b), env.ensure(b)
	env.mustCall(agentB, Request{Action: ActionCancel, WaiterID: r1.Waiter.ID})
	if env.op(r1.Operation.ID).Phase != PhaseQueued {
		t.Fatal("removing one waiter must not cancel the others' queued start")
	}
	env.clock.Advance(10 * time.Second)
	hb := env.mustCall(agentB, Request{Action: ActionHeartbeat, WaiterID: r2.Waiter.ID})
	if hb.QueuePosition != 1 || hb.Operation.ID != r1.Operation.ID {
		t.Fatalf("heartbeat reports the queue position: %+v", hb)
	}
	env.clock.Advance(10 * time.Second)
	env.pass()
	if env.op(r1.Operation.ID).Phase != PhaseQueued {
		t.Fatal("a heartbeating waiter keeps the start alive")
	}
	env.clock.Advance(16 * time.Second)
	env.pass()
	if env.op(r1.Operation.ID).Phase != PhaseCanceled {
		t.Fatalf("without a live waiter the queued start must be removed, got %s", env.op(r1.Operation.ID).Phase)
	}

	// Capacity freed later never starts the canceled work.
	env.stopProject(a)
	env.pass()
	env.pass()
	if slices.Contains(opIDs(env.backend.executions()), r1.Operation.ID) {
		t.Fatal("a canceled start must never execute")
	}
	if got := env.status(b); got.State != StateStopped || got.Reserved {
		t.Fatalf("b must stay stopped and unreserved: %+v", got)
	}
}

func TestWaiterDeadlineEndsTheWait(t *testing.T) {
	env := newEnv(t, testPolicy(1))
	a, b := env.authorize(), env.authorize()
	settle(env, a)
	agentB := principal{projectID: b.ID}
	r := env.mustCall(agentB, Request{Action: ActionEnsure, Deadline: env.clock.Now().Add(30 * time.Second)})
	for range 2 {
		env.clock.Advance(10 * time.Second)
		env.mustCall(agentB, Request{Action: ActionHeartbeat, WaiterID: r.Waiter.ID})
	}
	env.clock.Advance(15 * time.Second)
	_, err := env.call(agentB, Request{Action: ActionHeartbeat, WaiterID: r.Waiter.ID})
	wantStatus(t, err, http.StatusGone)
	var re *rpcError
	if !errors.As(err, &re) || re.resp.Operation == nil || re.resp.Operation.ID != r.Operation.ID {
		t.Fatalf("an expired wait still names its operation: %v", err)
	}
	if env.op(r.Operation.ID).Phase != PhaseCanceled {
		t.Fatalf("the unattended start must be canceled, got %s", env.op(r.Operation.ID).Phase)
	}
	if _, err := env.call(agentB, Request{Action: ActionEnsure, Deadline: env.clock.Now().Add(-time.Second)}); statusCode(err) != http.StatusBadRequest {
		t.Fatalf("a past deadline is refused, got %v", err)
	}
}

func TestCancelAfterAdmissionDetachesOnly(t *testing.T) {
	env := newEnv(t, testPolicy(2))
	env.runInline = false
	p := env.authorize()
	r := env.ensure(p)
	env.pass()
	resp := env.mustCall(principal{projectID: p.ID}, Request{Action: ActionCancel, WaiterID: r.Waiter.ID})
	if resp.Operation.Phase != PhaseAdmitted {
		t.Fatalf("cancel after admission only detaches, got %s", resp.Operation.Phase)
	}
	env.pass()
	if env.op(r.Operation.ID).Phase != PhaseAdmitted || !env.status(p).Reserved {
		t.Fatal("the admitted operation and its reservation must survive the detach")
	}
	for _, e := range env.backend.executions() {
		if strings.HasPrefix(e, KindStop+":") {
			t.Fatal("canceling a wait must never stop a project")
		}
	}
}

func TestCapacityFormulaCountsHelpersHeadroomAndExternalDemand(t *testing.T) {
	env := newEnv(t, testPolicy(2))
	pol := env.srv.cfg.Policy
	exact := addRes(addRes(testPair, pol.Helpers), pol.Headroom)
	env.backend.capacity = exact
	a, b := env.authorize(), env.authorize()
	settle(env, a)
	env.ensure(b)
	env.pass()
	if got := env.status(b); got.State != StateQueued || !strings.Contains(got.Blocker, "engine capacity") {
		t.Fatalf("the engine, not the budget, must hold b back: %+v", got)
	}
	env.backend.capacity = addRes(exact, testPair)
	if err := env.srv.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	env.pass()
	if got := env.status(b); got.State != StateStarting {
		t.Fatalf("b must start once the engine has room: %+v", got)
	}
}

func TestDeclaredExternalDemandCountsAgainstTheEngine(t *testing.T) {
	env := newEnv(t, testPolicy(2))
	pol := env.srv.cfg.Policy
	env.backend.capacity = addRes(addRes(testPair, pol.Helpers), pol.Headroom)
	env.backend.external = Resources{MemoryBytes: gib, NanoCPUs: 1_000_000_000}
	p := env.authorize()
	r := env.ensure(p)
	env.pass()
	if op := env.op(r.Operation.ID); op.Phase != PhaseFailed || !strings.Contains(op.Error, "engine cannot hold") {
		t.Fatalf("declared external containers must count against the engine: %+v", op)
	}
}

func TestImpossibleAndUnboundedRequestsAreRefused(t *testing.T) {
	env := newEnv(t, testPolicy(2))
	p := env.authorize()
	env.backend.capacity = Resources{MemoryBytes: 2 * gib, NanoCPUs: 2_000_000_000}
	r := env.ensure(p)
	env.pass()
	if op := env.op(r.Operation.ID); op.Phase != PhaseFailed || !strings.Contains(op.Error, "engine cannot hold") {
		t.Fatalf("a request that never fits must fail instead of waiting: %+v", op)
	}
	if len(env.launchedOps()) != 0 || env.status(p).Reserved {
		t.Fatal("a refused request reserves and launches nothing")
	}

	env.backend.capacity = Resources{MemoryBytes: 64 * gib, NanoCPUs: 64_000_000_000}
	env.backend.unbounded = []string{"foreign-db"}
	r = env.ensure(p)
	env.pass()
	if op := env.op(r.Operation.ID); op.Phase != PhaseFailed || !strings.Contains(op.Error, "resource caps") {
		t.Fatalf("unbounded external demand without an allowance must refuse admission: %+v", op)
	}

	pol := *env.mustCall(admin, Request{Action: ActionPolicy}).Policy
	pol.AllowUnboundedExternal = true
	pol.UnboundedExternalAllowance = Resources{MemoryBytes: gib, NanoCPUs: 1_000_000_000}
	env.mustCall(admin, Request{Action: ActionPolicy, Policy: &pol})
	r = env.ensure(p)
	env.pass()
	if env.op(r.Operation.ID).Phase != PhaseAdmitted {
		t.Fatalf("an explicit allowance admits the project, got %s", env.op(r.Operation.ID).Phase)
	}
}

func TestDockerOutageNeverFreesCapacityOrAdmits(t *testing.T) {
	env := newEnv(t, testPolicy(2))
	a, b := env.authorize(), env.authorize()
	settle(env, a)
	env.backend.mu.Lock()
	env.backend.inspectErr = errors.New("daemon down")
	env.backend.mu.Unlock()
	if err := env.srv.refresh(context.Background()); err == nil {
		t.Fatal("the failed inspection must be reported")
	}
	if got := env.status(a); !got.Reserved || got.State != StateRunning {
		t.Fatalf("an outage must keep the accounting: %+v", got)
	}
	env.ensure(b)
	env.pass()
	if got := env.status(b); got.State != StateQueued || !strings.Contains(got.Blocker, "Docker cannot be inspected") {
		t.Fatalf("admission must wait with an explanation during an outage: %+v", got)
	}
	if len(env.launchedOps()) != 1 {
		t.Fatal("no worker may launch while Docker cannot be inspected")
	}
	env.backend.mu.Lock()
	env.backend.inspectErr = nil
	env.backend.mu.Unlock()
	if err := env.srv.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	env.pass()
	if len(env.launchedOps()) != 2 {
		t.Fatal("admission resumes once Docker answers")
	}
}

func TestEngineIdentityChangeBlocksAdmission(t *testing.T) {
	env := newEnv(t, testPolicy(2))
	p := env.authorize()
	env.backend.mu.Lock()
	env.backend.engine = "some-other-engine"
	env.backend.mu.Unlock()
	err := env.srv.refresh(context.Background())
	if err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("a different engine must be refused, got %v", err)
	}
	env.ensure(p)
	env.pass()
	if len(env.launchedOps()) != 0 {
		t.Fatal("nothing may start on an engine the manager is not bound to")
	}
}

func TestPolicyCannotShrinkBelowReservations(t *testing.T) {
	env := newEnv(t, testPolicy(2))
	a, b := env.authorize(), env.authorize()
	settle(env, a)
	settle(env, b)
	pol := *env.mustCall(admin, Request{Action: ActionPolicy}).Policy
	smaller := pol
	smaller.Budget = testPair
	_, err := env.call(admin, Request{Action: ActionPolicy, Policy: &smaller})
	wantStatus(t, err, http.StatusConflict)
	invalid := pol
	invalid.Helpers = Resources{}
	_, err = env.call(admin, Request{Action: ActionPolicy, Policy: &invalid})
	wantStatus(t, err, http.StatusBadRequest)
	bigger := pol
	bigger.Budget = addRes(pol.Budget, testPair)
	env.mustCall(admin, Request{Action: ActionPolicy, Policy: &bigger})
	reloaded, err := LoadConfig(env.cfg.Root)
	if err != nil || reloaded.Policy.Budget != bigger.Budget {
		t.Fatalf("the policy must persist: %v", err)
	}
}

func TestStopRules(t *testing.T) {
	env := newEnv(t, testPolicy(1))
	a, b := env.authorize(), env.authorize()
	agentA, agentB := principal{projectID: a.ID}, principal{projectID: b.ID}

	_, err := env.call(agentA, Request{Action: ActionStop})
	wantStatus(t, err, http.StatusBadRequest)
	_, err = env.call(agentA, Request{Action: ActionStop, DiscardPageState: true, Force: true, ExpectedGeneration: "g1"})
	wantStatus(t, err, http.StatusForbidden)
	_, err = env.call(admin, Request{Action: ActionStop, ProjectID: a.ID, DiscardPageState: true, Force: true})
	wantStatus(t, err, http.StatusBadRequest)
	_, err = env.call(agentA, Request{Action: ActionStop, DiscardPageState: true, ExpectedGeneration: "g1"})
	wantStatus(t, err, http.StatusBadRequest)
	if resp := env.stopProject(a); resp.Operation != nil {
		t.Fatalf("an idle stopped project needs no stop operation: %+v", resp.Operation)
	}

	settle(env, a)
	s1, s2 := env.stopProject(a), env.stopProject(a)
	if s1.Operation.ID != s2.Operation.ID || s1.Operation.Kind != KindStop || !s1.Operation.DiscardPageState {
		t.Fatalf("identical stops coalesce: %+v %+v", s1.Operation, s2.Operation)
	}
	_, err = env.call(admin, Request{Action: ActionStop, ProjectID: a.ID, DiscardPageState: true, Force: true, ExpectedGeneration: "g2"})
	wantStatus(t, err, http.StatusConflict)
	env.pass()
	env.pass()
	if got := env.status(a); got.State != StateStopped || got.Reserved {
		t.Fatalf("a stop releases the project: %+v", got)
	}

	// An explicit stop cancels a start that is still queued.
	settle(env, a)
	rb := env.ensure(b)
	if sb := env.stopProject(b); sb.Operation != nil {
		t.Fatalf("a never-started project needs no stop operation: %+v", sb.Operation)
	}
	if env.op(rb.Operation.ID).Phase != PhaseCanceled {
		t.Fatalf("the queued start must be canceled, got %s", env.op(rb.Operation.ID).Phase)
	}
	hb := env.mustCall(agentB, Request{Action: ActionHeartbeat, WaiterID: rb.Waiter.ID})
	if hb.Operation.Phase != PhaseCanceled {
		t.Fatalf("its waiter must observe the cancellation: %+v", hb.Operation)
	}
}

func TestUpdateReservesCandidateCapacityBeforeMaintenance(t *testing.T) {
	env := newEnv(t, testPolicy(2))
	a, b := env.authorize(), env.authorize()
	settle(env, a)
	settle(env, b)
	apply := &UpdateAction{Kind: UpdateApply, PlanFile: "/plan.json", Confirmed: true}
	upd := env.mustCall(admin, Request{Action: ActionUpdate, ProjectID: a.ID, Update: apply})
	if upd.Operation.Phase != PhaseQueued || upd.Operation.Update == nil || upd.Operation.Update.Kind != UpdateApply {
		t.Fatalf("unexpected update operation %+v", upd.Operation)
	}
	env.pass()
	if got := env.status(a); !strings.Contains(got.Blocker, "budget") {
		t.Fatalf("the candidate pair must wait for budget: %+v", got)
	}
	_, err := env.call(admin, Request{Action: ActionUpdate, ProjectID: a.ID, Update: &UpdateAction{Kind: UpdateAbort, UpdateID: "u1"}})
	wantStatus(t, err, http.StatusConflict)

	env.stopProject(b)
	env.pass()
	env.pass()
	if got := env.op(upd.Operation.ID).Phase; got != PhaseAdmitted {
		t.Fatalf("freeing b must admit the update, got %s", got)
	}
	env.pass()
	if got := env.op(upd.Operation.ID).Phase; got != PhaseSucceeded {
		t.Fatalf("the update must finish, got %s", got)
	}
	if got := env.status(a); got.Reservation != testPair || got.State != StateRunning {
		t.Fatalf("once the candidate is gone only the pair stays reserved: %+v", got)
	}
}

// stopAwaitingApproval leaves a stopped project whose update waits for approval.
func stopAwaitingApproval(env *testEnv, p Project) {
	env.t.Helper()
	env.backend.execFn = func(_ context.Context, _ Config, p Project, _ Operation, _ []*os.File) error {
		env.backend.set(p.ID, Observation{State: StateStopped})
		return fmt.Errorf("apply: %w", update.ErrAwaitingApproval)
	}
	upd := env.mustCall(admin, Request{Action: ActionUpdate, ProjectID: p.ID, Update: &UpdateAction{Kind: UpdateApply, PlanFile: "/plan.json", Confirmed: true}})
	for range 3 {
		env.pass()
	}
	env.backend.execFn = nil
	if err := env.srv.refresh(context.Background()); err != nil {
		env.t.Fatal(err)
	}
	if got := env.op(upd.Operation.ID).Phase; got != PhaseAwaitingApproval {
		env.t.Fatalf("the update must wait for approval, got %s", got)
	}
	if got := env.status(p); got.State != StateStopped || got.Reserved {
		env.t.Fatalf("the awaiting project must be stopped and hold no capacity: %+v", got)
	}
}

func updateExecutions(env *testEnv) int {
	n := 0
	for _, e := range env.backend.executions() {
		if strings.HasPrefix(e, KindUpdate+":") {
			n++
		}
	}
	return n
}

func TestApproveAndAbortWaitForCapacityAfterTheProjectWasStopped(t *testing.T) {
	for _, kind := range []string{UpdateApprove, UpdateAbort} {
		for _, limit := range []string{"budget", "engine"} {
			t.Run(kind+" "+limit, func(t *testing.T) {
				env := newEnv(t, testPolicy(2))
				a, b, c := env.authorize(), env.authorize(), env.authorize()
				settle(env, a)
				stopAwaitingApproval(env, a)
				filled, want := []Project{b, c}, "budget"
				if limit == "engine" {
					pol := env.srv.cfg.Policy
					env.backend.capacity = addRes(addRes(testPair, pol.Helpers), pol.Headroom)
					filled, want = []Project{b}, "engine capacity"
				}
				for _, p := range filled {
					settle(env, p)
				}
				before := updateExecutions(env)

				resp := env.mustCall(admin, Request{Action: ActionUpdate, ProjectID: a.ID, Update: &UpdateAction{Kind: kind, UpdateID: "u1", Confirmed: true}})
				env.pass()
				env.pass()
				if got := env.op(resp.Operation.ID).Phase; got != PhaseQueued {
					t.Fatalf("%s must wait for a pair while others hold the capacity, got %s", kind, got)
				}
				if got := env.status(a); !strings.Contains(got.Blocker, want) {
					t.Fatalf("%s must wait for %s: %+v", kind, want, got)
				}
				if updateExecutions(env) != before {
					t.Fatalf("%s must not execute before capacity is free", kind)
				}

				env.stopProject(filled[0])
				for range 3 {
					env.pass()
				}
				if got := env.op(resp.Operation.ID).Phase; got != PhaseSucceeded || updateExecutions(env) != before+1 {
					t.Fatalf("freed capacity must admit %s exactly once, got %s", kind, got)
				}
			})
		}
	}
}

// inspectHook runs during after the wrapped inspection completes, as a slow
// Docker answer that arrives late would.
type inspectHook struct {
	Backend
	during func()
}

func (h inspectHook) Inspect(ctx context.Context, cfg Config, projects []Project) (Inventory, error) {
	inv, err := h.Backend.Inspect(ctx, cfg, projects)
	h.during()
	return inv, err
}

func slowInspection(env *testEnv, during func()) {
	env.srv.opts.inventoryMaxAge = time.Second
	env.clock.Advance(5 * time.Second)
	fired := false
	env.srv.backend = inspectHook{Backend: env.backend, during: func() {
		if !fired {
			fired = true
			during()
		}
	}}
}

func TestWaiterExpiringDuringInspectionIsNeverAdmitted(t *testing.T) {
	env := newEnv(t, testPolicy(2))
	p := env.authorize()
	r := env.ensure(p)
	slowInspection(env, func() { env.clock.Advance(20 * time.Second) })
	env.pass()
	env.pass()
	if got := env.op(r.Operation.ID).Phase; got != PhaseCanceled {
		t.Fatalf("the unattended start must be canceled, got %s", got)
	}
	if len(env.launchedOps()) != 0 || len(env.backend.executions()) != 0 || env.status(p).Reserved {
		t.Fatal("a start nobody waits for must neither reserve nor launch")
	}
}

func TestLiveCoalescedWaiterKeepsTheStartEligibleWhenAnotherExpiresDuringInspection(t *testing.T) {
	env := newEnv(t, testPolicy(2))
	p := env.authorize()
	r1, r2 := env.ensure(p), env.ensure(p)
	slowInspection(env, func() {
		env.clock.Advance(20 * time.Second)
		if _, err := env.srv.heartbeat(p.ID, r2.Waiter.ID); err != nil {
			t.Errorf("the live waiter must heartbeat: %v", err)
		}
	})
	env.pass()
	env.pass()
	if got := env.launchedOps(); len(got) != 1 || got[0] != r1.Operation.ID {
		t.Fatalf("the start must run once for the live waiter: %v", got)
	}
	if got := env.status(p); got.State != StateRunning {
		t.Fatalf("the project must be running: %+v", got)
	}
	_, err := env.call(principal{projectID: p.ID}, Request{Action: ActionHeartbeat, WaiterID: r1.Waiter.ID})
	wantStatus(t, err, http.StatusNotFound)
}

func TestQueuedUpdateDeadlinePassingDuringInspectionIsNeverAdmitted(t *testing.T) {
	env := newEnv(t, testPolicy(2))
	p := env.authorize()
	settle(env, p)
	upd := env.mustCall(admin, Request{
		Action: ActionUpdate, ProjectID: p.ID, Deadline: env.clock.Now().Add(30 * time.Second),
		Update: &UpdateAction{Kind: UpdateApply, PlanFile: "/plan.json", Confirmed: true},
	})
	slowInspection(env, func() { env.clock.Advance(time.Minute) })
	env.pass()
	env.pass()
	if op := env.op(upd.Operation.ID); op.Phase != PhaseFailed || !strings.Contains(op.Error, "timed out") {
		t.Fatalf("an update past its deadline must fail instead of starting: %+v", op)
	}
	if updateExecutions(env) != 0 {
		t.Fatal("an expired update must never execute")
	}
}

func TestUpdateValidationAndAdminOnly(t *testing.T) {
	env := newEnv(t, testPolicy(2))
	p := env.authorize()
	bad := []*UpdateAction{
		nil,
		{Kind: "shell"},
		{Kind: UpdateApply},
		{Kind: UpdateApply, PlanFile: "/plan.json"},
		{Kind: UpdateApprove, Confirmed: true},
		{Kind: UpdateDelete, Confirmed: true},
		{Kind: UpdateVerify},
		{Kind: UpdateAbort, UpdateID: "a\nb"},
	}
	for _, action := range bad {
		_, err := env.call(admin, Request{Action: ActionUpdate, ProjectID: p.ID, Update: action})
		wantStatus(t, err, http.StatusBadRequest)
	}
	_, err := env.call(principal{projectID: p.ID}, Request{Action: ActionUpdate, Update: &UpdateAction{Kind: UpdateAbort, UpdateID: "u"}})
	wantStatus(t, err, http.StatusForbidden)

	resp := env.mustCall(admin, Request{Action: ActionUpdate, ProjectID: p.ID, Update: &UpdateAction{Kind: UpdateVerify, CheckpointID: "cp1"}})
	env.pass()
	env.pass()
	op := env.op(resp.Operation.ID)
	if op.Phase != PhaseSucceeded || op.Update.Kind != UpdateVerify {
		t.Fatalf("a verify needs no candidate capacity and runs directly: %+v", op)
	}
	if got := filepath.Base(env.status(p).StateDir); got != p.ID {
		t.Fatalf("unexpected state directory %s", got)
	}
}
