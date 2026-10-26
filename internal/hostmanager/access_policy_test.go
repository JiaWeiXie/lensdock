package hostmanager

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAutomaticEnrollmentReusesContainingProjectAndHonorsRevocation(t *testing.T) {
	env := newEnv(t, DefaultPolicy())
	dir := t.TempDir()
	child := filepath.Join(dir, "src")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	first := env.mustCall(admin, Request{Action: ActionEnroll, Path: dir})
	if first.Project == nil {
		t.Fatal("enrollment returned no project")
	}
	p := *first.Project
	nested := env.mustCall(admin, Request{Action: ActionEnroll, Path: child})
	if nested.Project == nil || nested.Project.ID != p.ID || nested.Project.TokenFile != p.TokenFile {
		t.Fatalf("nested enrollment did not reuse the containing project: %+v", nested.Project)
	}
	env.mustCall(admin, Request{Action: ActionRevoke, ProjectID: p.ID})
	for _, path := range []string{dir, child} {
		_, err := env.call(admin, Request{Action: ActionEnroll, Path: path})
		wantStatus(t, err, http.StatusForbidden)
	}
	if resolved, err := ResolveProject(env.cfg.Root, child); err == nil {
		t.Fatalf("automatic enrollment revived a revoked grant: %+v", resolved)
	}
}

func TestAutomaticEnrollmentHonorsExplicitAuthorizationAndDirectoryIdentity(t *testing.T) {
	policy := DefaultPolicy()
	policy.RequireProjectAuthorization = true
	env := newEnv(t, policy)
	dir := t.TempDir()
	_, err := env.call(admin, Request{Action: ActionEnroll, Path: dir})
	wantStatus(t, err, http.StatusForbidden)
	p := env.authorizeDir(dir)
	policy.RequireProjectAuthorization = false
	env.mustCall(admin, Request{Action: ActionPolicy, Policy: &policy})
	moved := dir + "-moved"
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(moved) })
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	_, err = env.call(admin, Request{Action: ActionEnroll, Path: dir})
	wantStatus(t, err, http.StatusConflict)
	if got := env.status(p); got.ID != p.ID {
		t.Fatalf("directory replacement changed the project identity: %+v", got)
	}
}

func TestUnlimitedWaitSurvivesWithHeartbeatsUntilCanceled(t *testing.T) {
	env := newEnv(t, testPolicy(1))
	active, queued := env.authorize(), env.authorize()
	settle(env, active)
	owner := principal{projectID: queued.ID}
	response := env.mustCall(owner, Request{Action: ActionEnsure})
	env.clock.Advance(48 * time.Hour)
	env.mustCall(owner, Request{Action: ActionHeartbeat, WaiterID: response.Waiter.ID})
	env.pass()
	if op := env.op(response.Operation.ID); op.Phase != PhaseQueued {
		t.Fatalf("live waiter expired without an explicit deadline: %+v", op)
	}
	env.mustCall(owner, Request{Action: ActionCancel, WaiterID: response.Waiter.ID})
	if op := env.op(response.Operation.ID); op.Phase != PhaseCanceled {
		t.Fatalf("cancellation did not remove the unlimited queued wait: %+v", op)
	}
}

func TestStopDoesNotRequireAnExtraConfirmationByDefault(t *testing.T) {
	env := newEnv(t, DefaultPolicy())
	p := env.authorize()
	settle(env, p)
	response := env.mustCall(principal{projectID: p.ID}, Request{Action: ActionStop})
	if response.Operation == nil || response.Operation.Kind != KindStop {
		t.Fatalf("explicit stop did not schedule a stop: %+v", response)
	}
	env.pass()
	env.pass()
	if got := env.status(p); got.State != StateStopped {
		t.Fatalf("explicit stop did not stop the project: %+v", got)
	}
}
