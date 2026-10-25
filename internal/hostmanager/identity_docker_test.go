package hostmanager

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/JiaWeiXie/lensdock/internal/update"
)

func identityProject(t *testing.T, handler http.Handler) Project {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	return Project{ID: "p-expected", Origin: srv.URL, Port: n}
}

func TestIdentityVerificationRejectsForeignOriginsAndRedirects(t *testing.T) {
	for _, body := range []string{`{"deploymentId":"p-other"}`, `{"deploymentId":""}`, `{"deploymentId":"p-expected","extra":"config"}`, `{"deploymentId":"p-expected"}{}`} {
		t.Run(body, func(t *testing.T) {
			p := identityProject(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
					t.Error("identity request carried credentials")
				}
				fmt.Fprint(w, body)
			}))
			if err := verifyGatewayIdentity(context.Background(), p); err == nil {
				t.Fatal("untrusted identity response accepted")
			}
		})
	}
	var redirected atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Add(1)
		fmt.Fprint(w, `{"deploymentId":"p-expected"}`)
	}))
	defer target.Close()
	p := identityProject(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	if err := verifyGatewayIdentity(context.Background(), p); err == nil || redirected.Load() != 0 {
		t.Fatalf("redirect was followed: %v calls=%d", err, redirected.Load())
	}
}

func TestIdentityVerificationAcceptsOnlyTheExactPinnedDeployment(t *testing.T) {
	p := identityProject(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/deployment" || r.Header.Get("Authorization") != "" {
			t.Errorf("unexpected identity request %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"deploymentId":"p-expected"}`)
	}))
	if err := verifyGatewayIdentity(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	p.Origin = strings.Replace(p.Origin, "127.0.0.1", "localhost", 1)
	if err := verifyGatewayIdentity(context.Background(), p); err == nil {
		t.Fatal("an origin different from the registered IP/port was accepted")
	}
}

func TestContainerCapsDistinguishUnboundedAndQuotaLimitedDemand(t *testing.T) {
	var container dockerContainer
	container.HostConfig.Memory = 2 << 30
	if got := containerCaps(container); got.NanoCPUs != 0 {
		t.Fatalf("uncapped CPU was treated as bounded: %+v", got)
	}
	container.HostConfig.CpuQuota = 50000
	container.HostConfig.CpuPeriod = 100000
	if got := containerCaps(container); got.MemoryBytes != 2<<30 || got.NanoCPUs != 500000000 {
		t.Fatalf("quota cap: %+v", got)
	}
	container.HostConfig.NanoCpus = 1500000000
	if got := containerCaps(container); got.NanoCPUs != 1500000000 {
		t.Fatalf("NanoCPUs precedence: %+v", got)
	}
	container.HostConfig.NanoCpus = 0
	container.HostConfig.CpuQuota = 0
	container.HostConfig.CpusetCpus = "0-2,5"
	if got := containerCaps(container); got.NanoCPUs != 4000000000 {
		t.Fatalf("cpuset cap: %+v", got)
	}
	container.HostConfig.CpusetCpus = "2-1"
	if got := containerCaps(container); got.NanoCPUs != 0 {
		t.Fatalf("invalid cpuset inferred free capacity: %+v", got)
	}
}

func TestHelperPolicyChangesPreserveAuthorizedProjectLimits(t *testing.T) {
	env := newEnv(t, testPolicy(2))
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
	before := mustRead(t, path)
	policy := env.cfg.Policy
	policy.Helpers.MemoryBytes *= 2
	policy.Helpers.NanoCPUs *= 2
	env.mustCall(admin, Request{Action: ActionPolicy, Policy: &policy})
	cfg, err := LoadConfig(env.cfg.Root)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureResourceProfile(cfg, p, dep); err != nil {
		t.Fatalf("a helper allowance change must not prevent the project's next operation: %v", err)
	}
	if !bytes.Equal(before, mustRead(t, path)) || env.status(p).Limits != p.Limits {
		t.Fatal("helper policy must not change the authorized project resource profile")
	}
	changed := p
	changed.Limits.Runtime.MemoryBytes *= 2
	if err := ensureResourceProfile(cfg, changed, dep); err == nil {
		t.Fatal("silently resizing an established project must still be refused")
	}
	if !bytes.Equal(before, mustRead(t, path)) {
		t.Fatal("a refused resize modified the established resource profile")
	}
}

func TestHelperPolicyCannotChangeWhileAHelperIsRunning(t *testing.T) {
	env := newEnv(t, testPolicy(2))
	helper, err := AcquireHelper(context.Background(), env.cfg.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer helper.Close()
	next := env.cfg.Policy
	next.Helpers.MemoryBytes *= 2
	_, err = env.call(admin, Request{Action: ActionPolicy, Policy: &next})
	wantStatus(t, err, http.StatusConflict)
	cfg, err := LoadConfig(env.cfg.Root)
	if err != nil || cfg.Policy != env.cfg.Policy {
		t.Fatalf("a running helper's accounted allowance changed: %v %+v", err, cfg.Policy)
	}
	if err := helper.Close(); err != nil {
		t.Fatal(err)
	}
	env.mustCall(admin, Request{Action: ActionPolicy, Policy: &next})
	cfg, err = LoadConfig(env.cfg.Root)
	if err != nil || cfg.Policy != next {
		t.Fatalf("the released helper slot must permit the new policy: %v %+v", err, cfg.Policy)
	}
}

func TestManagedApplyOutcomePreservesApprovalAndFailure(t *testing.T) {
	result := &update.ApplyResult{UpdateID: "upd-real-result", Status: update.JournalAwaitingApproval}
	err := managedApplyOutcome(result, nil)
	if !errors.Is(err, update.ErrAwaitingApproval) || !strings.Contains(err.Error(), result.UpdateID) {
		t.Fatalf("an unapproved switch must retain its update identity and approval state: %v", err)
	}
	failed := errors.New("checkpoint failed")
	if got := managedApplyOutcome(result, failed); got != failed {
		t.Fatalf("the actual Engine failure must take precedence over a partial result: %v", got)
	}
	if err := managedApplyOutcome(&update.ApplyResult{Status: update.JournalCompleted}, nil); err != nil {
		t.Fatalf("a completed switch must not be reported as awaiting approval: %v", err)
	}
}
