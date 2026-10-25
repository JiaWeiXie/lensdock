package update

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/JiaWeiXie/lensdock/internal/auth"
	"github.com/JiaWeiXie/lensdock/internal/lease"
	"github.com/JiaWeiXie/lensdock/internal/runtimeapi"
	"github.com/JiaWeiXie/lensdock/internal/supervisor"
)

const (
	probeTestWorkspace = "default"
	probeTestOperator  = "ldo_probe-test-operator-token"
)

type readyHealth struct{}

func (readyHealth) Ready() bool { return true }
func (readyHealth) Detail() supervisor.Detail {
	return supervisor.Detail{ChromiumVersion: "test-chromium"}
}

// probeRuntime is the real runtime API handler over a real lease manager,
// reached through the probe's own HTTP client. maintenanceCalls counts the
// requests that reached POST /maintenance.
type probeRuntime struct {
	leases           *lease.Manager
	srv              *httptest.Server
	maintenanceCalls atomic.Int64
}

func newProbeRuntime(t *testing.T) *probeRuntime {
	t.Helper()
	m, err := lease.NewManager(lease.Config{WorkspaceID: probeTestWorkspace})
	if err != nil {
		t.Fatal(err)
	}
	m.SetLocked(false, "")
	t.Cleanup(m.Shutdown)
	api := runtimeapi.NewHandler(runtimeapi.Deps{
		WorkspaceID: probeTestWorkspace,
		Operator:    auth.DigestOf(probeTestOperator),
		Leases:      m,
		Health:      readyHealth{},
	})
	rt := &probeRuntime{leases: m}
	rt.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/maintenance") {
			rt.maintenanceCalls.Add(1)
		}
		api.ServeHTTP(w, r)
	}))
	t.Cleanup(rt.srv.Close)
	return rt
}

func (rt *probeRuntime) request(op string) ProbeRequest {
	return ProbeRequest{Op: op, BaseURL: rt.srv.URL, Workspace: probeTestWorkspace, Token: probeTestOperator}
}

// observedGeneration reads the runtime generation the way the caller does: with
// a status probe.
func (rt *probeRuntime) observedGeneration(t *testing.T) string {
	t.Helper()
	resp := HandleProbe(context.Background(), rt.request(ProbeStatus))
	if resp.Error != "" || resp.Status == nil || resp.Status.RuntimeGeneration == "" {
		t.Fatalf("status probe = %+v", resp)
	}
	return resp.Status.RuntimeGeneration
}

func (rt *probeRuntime) conditional(gen string) ProbeRequest {
	req := rt.request(ProbeMaintenanceEnter)
	req.Reason = "explicit project stop"
	req.IfIdle = true
	req.ExpectedGeneration = gen
	return req
}

func TestProbeConditionalMaintenanceAdmitsIdleRuntime(t *testing.T) {
	rt := newProbeRuntime(t)
	gen := rt.observedGeneration(t)

	resp := HandleProbe(context.Background(), rt.conditional(gen))
	if resp.Error != "" || resp.Maintenance == nil {
		t.Fatalf("conditional enter = %+v", resp)
	}
	if m := resp.Maintenance; m.State != "MAINTENANCE" || !m.Drained || m.RuntimeGeneration != gen {
		t.Fatalf("maintenance = %+v", *m)
	}
	if _, _, err := rt.leases.Acquire("late", lease.ModeAgent, 0, "k"); err == nil {
		t.Fatal("a grant after the conditional fence must fail")
	}
	// A repeated conditional entry is a typed conflict, not an idempotent success.
	again := HandleProbe(context.Background(), rt.conditional(gen))
	if again.Maintenance != nil || again.Code != "http-409" || !strings.Contains(again.Error, "MAINTENANCE") {
		t.Fatalf("second conditional enter = %+v", again)
	}
}

func TestProbeConditionalMaintenanceRefusesLiveLeaseAndStaleGeneration(t *testing.T) {
	for _, mode := range []lease.Mode{lease.ModeHuman, lease.ModeAgent} {
		t.Run(string(mode), func(t *testing.T) {
			rt := newProbeRuntime(t)
			l, tok, err := rt.leases.Acquire("holder", mode, 0, "k")
			if err != nil {
				t.Fatal(err)
			}
			gen := rt.observedGeneration(t)

			resp := HandleProbe(context.Background(), rt.conditional(gen))
			if resp.Maintenance != nil || resp.Code != "http-409" || !strings.Contains(resp.Error, "LEASE_CONFLICT") {
				t.Fatalf("conditional enter with a live %s lease = %+v", mode, resp)
			}
			stale := HandleProbe(context.Background(), rt.conditional("stale-generation"))
			if stale.Maintenance != nil || stale.Code != "http-412" || !strings.Contains(stale.Error, "GENERATION_MISMATCH") {
				t.Fatalf("stale generation = %+v", stale)
			}

			st := HandleProbe(context.Background(), rt.request(ProbeStatus))
			if st.Status == nil || st.Status.LeaseID != l.ID || st.Status.RuntimeGeneration != gen || st.Status.ControlState == "MAINTENANCE" {
				t.Fatalf("status after refusals = %+v", st)
			}
			if _, err := rt.leases.Authorize(tok.Reveal(), mode); err != nil {
				t.Fatalf("the lease ticket must stay valid after refusals: %v", err)
			}
		})
	}
}

func TestProbeRejectsAmbiguousConditionalInputBeforeAnyRequest(t *testing.T) {
	rt := newProbeRuntime(t)
	gen := rt.observedGeneration(t)
	for name, mutate := range map[string]func(*ProbeRequest){
		"ifIdle without generation": func(r *ProbeRequest) { r.IfIdle, r.ExpectedGeneration = true, "" },
		"oversized generation":      func(r *ProbeRequest) { r.IfIdle, r.ExpectedGeneration = true, strings.Repeat("g", 129) },
		"generation without ifIdle": func(r *ProbeRequest) { r.IfIdle, r.ExpectedGeneration = false, gen },
	} {
		req := rt.request(ProbeMaintenanceEnter)
		mutate(&req)
		if resp := HandleProbe(context.Background(), req); resp.Error == "" || resp.Maintenance != nil {
			t.Errorf("%s: response = %+v, want a validation error", name, resp)
		}
	}
	if n := rt.maintenanceCalls.Load(); n != 0 {
		t.Fatalf("%d invalid requests reached the runtime", n)
	}
	if rt.leases.Status().Maintenance {
		t.Fatal("an invalid request entered maintenance")
	}
}

func TestProbeUnconditionalMaintenanceStillFencesLiveLease(t *testing.T) {
	rt := newProbeRuntime(t)
	_, tok, err := rt.leases.Acquire("holder", lease.ModeAgent, 0, "k")
	if err != nil {
		t.Fatal(err)
	}
	req := rt.request(ProbeMaintenanceEnter)
	req.Reason = "lensdock update j1"
	resp := HandleProbe(context.Background(), req)
	if resp.Error != "" || resp.Maintenance == nil || resp.Maintenance.State != "MAINTENANCE" {
		t.Fatalf("unconditional enter = %+v", resp)
	}
	if _, err := rt.leases.Authorize(tok.Reveal(), lease.ModeAgent); err == nil {
		t.Fatal("the unconditional probe must still fence the live lease")
	}
}

func TestRunProbeAcceptsConditionalMaintenanceDocument(t *testing.T) {
	rt := newProbeRuntime(t)
	gen := rt.observedGeneration(t)
	doc, err := json.Marshal(rt.conditional(gen))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := RunProbe(context.Background(), bytes.NewReader(doc), &out); err != nil {
		t.Fatal(err)
	}
	var resp ProbeResponse
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != "" || resp.Maintenance == nil || resp.Maintenance.State != "MAINTENANCE" {
		t.Fatalf("RunProbe response = %+v", resp)
	}
}
