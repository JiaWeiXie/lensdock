package runtimeapi

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
	"github.com/JiaWeiXie/lensdock/internal/lease"
)

func idleMaintenanceBody(generation string) []byte {
	return []byte(fmt.Sprintf(`{"reason":"explicit project stop","ifIdle":true,"expectedGeneration":%q}`, generation))
}

func TestConditionalMaintenanceAdmitsIdleWorkspaceAndBlocksGrants(t *testing.T) {
	e := newCEnv(t)
	gen := e.leases.Status().Generation

	r := e.call("POST", base+"/maintenance", operatorTok, nil, idleMaintenanceBody(gen))
	e.expect("conditional enter", r, http.StatusOK, "")
	var out struct {
		State             string
		Drained           bool
		RuntimeGeneration string
	}
	r.into(t, &out)
	if out.State != "MAINTENANCE" || !out.Drained || out.RuntimeGeneration != gen {
		t.Fatalf("maintenance = %+v, want MAINTENANCE drained in generation %s", out, gen)
	}
	e.expect("acquire after admission", e.acquire("AGENT", "actor:bot", 0, nil), http.StatusConflict, apiwire.CodeMaintenance)
	e.expect("HUMAN acquire after admission", e.acquire("HUMAN", "actor:alice", 0, nil), http.StatusConflict, apiwire.CodeMaintenance)
	// A second conditional request must not look like success.
	e.expect("second conditional", e.call("POST", base+"/maintenance", operatorTok, nil, idleMaintenanceBody(gen)), http.StatusConflict, apiwire.CodeMaintenance)
	e.expect("exit", e.call("DELETE", base+"/maintenance", operatorTok, nil, nil), http.StatusNoContent, "")
	e.expect("acquire after exit", e.acquire("AGENT", "actor:bot", 0, nil), http.StatusCreated, "")
}

func TestConditionalMaintenanceRefusesLiveLeaseWithoutFencingIt(t *testing.T) {
	for _, mode := range []string{"HUMAN", "AGENT"} {
		t.Run(mode, func(t *testing.T) {
			e := newCEnv(t)
			g := e.grant(mode, "actor:holder")
			gen := e.leases.Status().Generation
			if mode == "HUMAN" {
				sock, _, err := e.dial(base+"/vnc/control", g.CapabilityToken, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer sock.CloseNow()
			}

			r := e.call("POST", base+"/maintenance", operatorTok, nil, idleMaintenanceBody(gen))
			e.expect("conditional enter with a live lease", r, http.StatusConflict, apiwire.CodeLeaseConflict)

			if st := e.leases.Status(); st.Maintenance || st.Generation != gen || st.Lease == nil || string(st.Lease.Mode) != mode {
				t.Fatalf("status after refusal = %+v", st)
			}
			e.expect("lease still valid", e.call("GET", base+"/leases/current", g.CapabilityToken, nil, nil), http.StatusOK, "")
			if _, err := e.leases.Authorize(g.CapabilityToken, lease.Mode(mode)); err != nil {
				t.Fatalf("ticket must remain valid after the refusal: %v", err)
			}
			if mode == "HUMAN" {
				select {
				case <-e.ctlUp.closed:
					t.Fatal("the control connection was closed by a refused conditional request")
				default:
				}
			}
			e.expect("another holder is still refused", e.acquire("AGENT", "actor:other", 0, nil), http.StatusConflict, apiwire.CodeLeaseConflict)
		})
	}
}

func TestConditionalMaintenanceRefusesStaleGenerationWithoutChange(t *testing.T) {
	e := newCEnv(t)
	g := e.grant("AGENT", "actor:bot")
	gen := e.leases.Status().Generation

	r := e.call("POST", base+"/maintenance", operatorTok, nil, idleMaintenanceBody("stale-generation"))
	e.expect("stale generation", r, http.StatusPreconditionFailed, apiwire.CodeGenerationMismatch)
	if st := e.leases.Status(); st.Maintenance || st.Generation != gen || st.Lease == nil {
		t.Fatalf("status after refusal = %+v", st)
	}
	if _, err := e.leases.Authorize(g.CapabilityToken, lease.ModeAgent); err != nil {
		t.Fatalf("ticket must remain valid: %v", err)
	}

	// A restarted browser also invalidates an observation of the earlier generation.
	e.leases.StartGeneration()
	e.expect("observed before restart", e.call("POST", base+"/maintenance", operatorTok, nil, idleMaintenanceBody(gen)), http.StatusPreconditionFailed, apiwire.CodeGenerationMismatch)
	if e.leases.Status().Maintenance {
		t.Fatal("a stale generation entered maintenance")
	}
}

func TestConditionalMaintenanceRefusesTakeoverAndCaptureWithoutChange(t *testing.T) {
	t.Run("takeover", func(t *testing.T) {
		e := newCEnv(t)
		gen := e.leases.Status().Generation
		h, err := e.leases.BeginHandoff("k-takeover", false)
		if err != nil {
			t.Fatal(err)
		}
		e.expect("conditional enter during takeover", e.call("POST", base+"/maintenance", operatorTok, nil, idleMaintenanceBody(gen)), http.StatusConflict, apiwire.CodeLeaseConflict)
		if cur, ok := e.leases.Handoff(); !ok || cur.RequestID != h.RequestID {
			t.Fatalf("takeover after refusal = %+v ok=%v, want %s", cur, ok, h.RequestID)
		}
		if e.leases.Status().Maintenance {
			t.Fatal("maintenance was entered during a takeover")
		}
	})
	t.Run("capture", func(t *testing.T) {
		e := newCEnv(t)
		g := e.grant("HUMAN", "actor:alice")
		gen := e.leases.Status().Generation
		sess, err := e.leases.BeginCapture("", true)
		if err != nil {
			t.Fatal(err)
		}
		e.expect("conditional enter during capture", e.call("POST", base+"/maintenance", operatorTok, nil, idleMaintenanceBody(gen)), http.StatusConflict, apiwire.CodeLeaseConflict)
		if st := e.leases.Status(); !st.Capture || st.Maintenance {
			t.Fatalf("status after refusal = %+v, want the capture still held", st)
		}
		if out := sess.End(); out.Restored == nil {
			t.Fatalf("capture outcome = %+v, want the holder restored", out)
		}
		if _, err := e.leases.Authorize(g.CapabilityToken, lease.ModeHuman); err != nil {
			t.Fatalf("the restored holder keeps its token: %v", err)
		}
	})
}

func TestMaintenanceRequestRejectsAmbiguousConditionalInput(t *testing.T) {
	e := newCEnv(t)
	g := e.grant("AGENT", "actor:bot")
	gen := e.leases.Status().Generation
	bodies := map[string]string{
		"ifIdle without generation":       `{"reason":"stop","ifIdle":true}`,
		"ifIdle with empty generation":    `{"reason":"stop","ifIdle":true,"expectedGeneration":""}`,
		"oversized generation":            `{"reason":"stop","ifIdle":true,"expectedGeneration":"` + strings.Repeat("g", 129) + `"}`,
		"generation without ifIdle":       fmt.Sprintf(`{"reason":"stop","expectedGeneration":%q}`, gen),
		"generation with ifIdle false":    fmt.Sprintf(`{"reason":"stop","ifIdle":false,"expectedGeneration":%q}`, gen),
		"ifIdle is not a boolean":         fmt.Sprintf(`{"reason":"stop","ifIdle":"yes","expectedGeneration":%q}`, gen),
		"unknown conditional-ish field":   fmt.Sprintf(`{"reason":"stop","ifIdle":true,"expectedGeneration":%q,"force":true}`, gen),
		"missing reason on conditional":   fmt.Sprintf(`{"ifIdle":true,"expectedGeneration":%q}`, gen),
		"expectedGeneration not a string": `{"reason":"stop","ifIdle":true,"expectedGeneration":7}`,
		"null ifIdle":                     `{"reason":"stop","ifIdle":null}`,
		"empty generation without ifIdle": `{"reason":"stop","expectedGeneration":""}`,
		"empty generation with false":     `{"reason":"stop","ifIdle":false,"expectedGeneration":""}`,
		"null generation without ifIdle":  `{"reason":"stop","expectedGeneration":null}`,
		"null generation with false":      `{"reason":"stop","ifIdle":false,"expectedGeneration":null}`,
		"null conditional generation":     `{"reason":"stop","ifIdle":true,"expectedGeneration":null}`,
	}
	for name, body := range bodies {
		e.expect(name, e.call("POST", base+"/maintenance", operatorTok, nil, []byte(body)), http.StatusBadRequest, apiwire.CodeInvalidRequest)
	}
	if st := e.leases.Status(); st.Maintenance || st.Lease == nil {
		t.Fatalf("a rejected request changed the workspace: %+v", st)
	}
	if _, err := e.leases.Authorize(g.CapabilityToken, lease.ModeAgent); err != nil {
		t.Fatalf("ticket must remain valid: %v", err)
	}
}

func TestMaintenanceWithoutIfIdleKeepsUnconditionalContract(t *testing.T) {
	// Neither an omitted ifIdle nor an explicit false changes the updater's
	// behaviour: the live lease is fenced and maintenance is entered.
	for name, body := range map[string]string{
		"omitted":        `{"reason":"update"}`,
		"explicit false": `{"reason":"update","ifIdle":false}`,
	} {
		t.Run(name, func(t *testing.T) {
			e := newCEnv(t)
			g := e.grant("AGENT", "actor:bot")
			ticket, release, err := e.leases.AuthorizeTracked(g.CapabilityToken, lease.ModeAgent)
			if err != nil {
				t.Fatal(err)
			}
			release()
			e.expect("enter", e.call("POST", base+"/maintenance", operatorTok, nil, []byte(body)), http.StatusOK, "")
			if st := e.leases.Status(); !st.Maintenance || st.Lease != nil {
				t.Fatalf("status = %+v, want MAINTENANCE with the lease fenced", st)
			}
			select {
			case <-ticket.Done():
			default:
				t.Fatal("unconditional maintenance must fence the live lease")
			}
		})
	}
}
