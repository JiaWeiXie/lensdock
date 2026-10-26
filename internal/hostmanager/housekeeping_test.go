package hostmanager

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/update"
)

func writeProjectDeployment(t *testing.T, env *testEnv, p Project, body string) {
	t.Helper()
	dir := projectStateDir(env.cfg.Root, p.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "deployment.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func housekeepingOps(env *testEnv) []Operation {
	env.srv.mu.Lock()
	defer env.srv.mu.Unlock()
	var out []Operation
	for _, op := range env.srv.reg.Ops {
		if isHousekeeping(op) {
			out = append(out, op.Operation)
		}
	}
	return out
}

func updateRuns(env *testEnv) int {
	n := 0
	for _, e := range env.backend.executions() {
		if strings.HasPrefix(e, KindUpdate+":") {
			n++
		}
	}
	return n
}

// passes runs enough scheduler passes for one queued operation to be
// admitted, executed and reconciled.
func passes(env *testEnv) {
	for range 4 {
		env.pass()
	}
}

// dueHousekeeping starts a project's expiry clock and moves it past the interval.
func dueHousekeeping(t *testing.T, env *testEnv, body string) Project {
	t.Helper()
	p := env.authorize()
	writeProjectDeployment(t, env, p, body)
	env.pass() // the first sighting only starts the clock
	if n := len(housekeepingOps(env)); n != 0 {
		t.Fatalf("a first sighting scheduled %d operation(s)", n)
	}
	env.clock.Advance(update.AutoExpireInterval + time.Minute)
	return p
}

func TestHousekeepingRunsOncePerIntervalOnAnIdleManager(t *testing.T) {
	env := newEnv(t, testPolicy(2))
	p := env.authorize()
	writeProjectDeployment(t, env, p, `{"expireAfterDays":30}`)

	env.pass()
	env.clock.Advance(update.AutoExpireInterval - time.Minute)
	passes(env)
	if n := len(housekeepingOps(env)); n != 0 {
		t.Fatalf("housekeeping ran %d time(s) before the interval", n)
	}

	env.clock.Advance(2 * time.Minute)
	passes(env)
	ops := housekeepingOps(env)
	if len(ops) != 1 || ops[0].Phase != PhaseSucceeded || updateRuns(env) != 1 {
		t.Fatalf("one due project must run exactly one housekeeping operation: %+v, %v", ops, env.backend.executions())
	}
	if a := ops[0].Update; !a.Automatic || a.Kind != UpdateExpire || a.AfterDays != nil || len(a.Scopes) != 0 {
		t.Fatalf("housekeeping must apply the deployment's own setting: %+v", a)
	}
	if last := env.srv.reg.Projects[p.ID].LastOp; last != "" {
		t.Fatalf("housekeeping replaced the project's last operation with %s", last)
	}

	passes(env)
	if updateRuns(env) != 1 {
		t.Fatal("housekeeping repeated within the interval")
	}
	env.clock.Advance(update.AutoExpireInterval)
	passes(env)
	if updateRuns(env) != 2 {
		t.Fatalf("housekeeping did not run again after the interval: %v", env.backend.executions())
	}
}

func TestHousekeepingHonorsTheDeploymentSetting(t *testing.T) {
	for name, body := range map[string]string{
		"disabled by zero days": `{"expireAfterDays":0}`,
		"unreadable settings":   `{not json`,
	} {
		t.Run(name, func(t *testing.T) {
			env := newEnv(t, testPolicy(2))
			dueHousekeeping(t, env, body)
			passes(env)
			if len(housekeepingOps(env)) != 0 || updateRuns(env) != 0 {
				t.Fatalf("housekeeping ran despite %s", name)
			}
		})
	}

	t.Run("a project that never started is skipped and an absent setting is the default", func(t *testing.T) {
		env := newEnv(t, testPolicy(2))
		p := env.authorize()
		env.pass()
		env.clock.Advance(update.AutoExpireInterval + time.Minute)
		passes(env)
		if len(housekeepingOps(env)) != 0 {
			t.Fatal("a project without a deployment was scheduled")
		}
		env.clock.Advance(update.AutoExpireInterval)
		writeProjectDeployment(t, env, p, `{"version":1}`)
		passes(env)
		if len(housekeepingOps(env)) != 1 {
			t.Fatal("an absent expireAfterDays must mean the default and be scheduled")
		}
	})
}

func TestHousekeepingNeverDelaysForegroundWork(t *testing.T) {
	t.Run("it is canceled in the commit that adds a foreground operation", func(t *testing.T) {
		env := newEnv(t, testPolicy(2))
		p := dueHousekeeping(t, env, `{"expireAfterDays":30}`)
		env.pass() // queues the housekeeping operation
		ops := housekeepingOps(env)
		if len(ops) != 1 || ops[0].Phase != PhaseQueued {
			t.Fatalf("expected one queued housekeeping operation, got %+v", ops)
		}
		if got := env.status(p); got.OperationID != "" || got.State == StateQueued || got.State == StateUpdating {
			t.Fatalf("queued housekeeping must be invisible to the project's state: %+v", got)
		}

		resp := env.ensure(p)
		if resp.Operation == nil || resp.Operation.Kind != KindStart {
			t.Fatalf("a start request must create its own operation, got %+v", resp.Operation)
		}
		if got := env.op(ops[0].ID).Phase; got != PhaseCanceled {
			t.Fatalf("housekeeping must give way to foreground work, phase %s", got)
		}
		passes(env)
		if updateRuns(env) != 0 {
			t.Fatal("the superseded housekeeping executed")
		}
	})

	t.Run("it is not scheduled while any operation is queued or running", func(t *testing.T) {
		env := newEnv(t, testPolicy(2))
		p := dueHousekeeping(t, env, `{"expireAfterDays":30}`)
		env.ensure(p)
		env.pass() // admits and runs the start
		if n := len(housekeepingOps(env)); n != 0 {
			t.Fatalf("housekeeping was scheduled next to a start: %d", n)
		}
		passes(env)
		if len(housekeepingOps(env)) != 1 {
			t.Fatal("housekeeping must follow once the manager is idle again")
		}
	})

	t.Run("a running housekeeping does not block a start of a running project", func(t *testing.T) {
		env := newEnv(t, testPolicy(2))
		p := dueHousekeeping(t, env, `{"expireAfterDays":30}`)
		env.runInline = false // the worker never finishes: housekeeping stays running
		env.pass()
		env.pass()
		ops := housekeepingOps(env)
		if len(ops) != 1 || !workerPhase(ops[0].Phase) {
			t.Fatalf("expected housekeeping with a worker, got %+v", ops)
		}
		if got := env.status(p); got.State == StateUpdating || got.OperationID != "" {
			t.Fatalf("running housekeeping must not change the visible project state: %+v", got)
		}

		_, err := env.call(principal{projectID: p.ID}, Request{Action: ActionEnsure, Deadline: env.clock.Now().Add(time.Minute)})
		wantStatus(t, err, http.StatusConflict) // a stopped project cannot start beside a running helper

		env.backend.set(p.ID, Observation{State: StateRunning, PortOwned: true, Enforced: pairOf(p.Limits)})
		resp, err := env.call(principal{projectID: p.ID}, Request{Action: ActionEnsure, Deadline: env.clock.Now().Add(time.Minute)})
		if err != nil || resp.Waiter != nil || resp.Project == nil || resp.Project.State != StateRunning {
			t.Fatalf("a running project must answer immediately: %+v %v", resp, err)
		}
	})
}

func TestHousekeepingStopsAtShutdown(t *testing.T) {
	env := newEnv(t, testPolicy(2))
	dueHousekeeping(t, env, `{"expireAfterDays":30}`)
	close(env.srv.shutdownCh)
	env.srv.scheduleHousekeeping()
	if n := len(housekeepingOps(env)); n != 0 {
		t.Fatalf("housekeeping was scheduled after shutdown: %d", n)
	}
	passes(env)
	if updateRuns(env) != 0 {
		t.Fatal("housekeeping executed after shutdown")
	}
}

func TestAutomaticMarkerIsReservedForTheManager(t *testing.T) {
	env := newEnv(t, testPolicy(2))
	p := env.authorize()
	_, err := env.call(admin, Request{Action: ActionUpdate, ProjectID: p.ID, Update: &UpdateAction{Kind: UpdateExpire, Confirmed: true, Automatic: true}})
	wantStatus(t, err, http.StatusBadRequest)

	resp := env.mustCall(admin, Request{Action: ActionUpdate, ProjectID: p.ID, Update: &UpdateAction{Kind: UpdateExpire, Confirmed: true, Scopes: []string{update.ScopeCheckpoints}}})
	if resp.Operation == nil || resp.Operation.Update.Kind != UpdateExpire || resp.Operation.Update.Automatic {
		t.Fatalf("an explicit expire request is an ordinary visible operation: %+v", resp.Operation)
	}
}
