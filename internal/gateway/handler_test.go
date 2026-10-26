package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
	"github.com/JiaWeiXie/lensdock/internal/auth"
	"github.com/JiaWeiXie/lensdock/internal/lease"
	"github.com/JiaWeiXie/lensdock/internal/runtimeapi"
	"github.com/JiaWeiXie/lensdock/internal/supervisor"
)

type readyHealth struct{}

func (readyHealth) Ready() bool               { return true }
func (readyHealth) Detail() supervisor.Detail { return supervisor.Detail{} }

func TestGatewayScopesLeasesAndLogoutRevokesRuntime(t *testing.T) {
	manager, err := lease.NewManager(lease.Config{WorkspaceID: "default"})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Shutdown()
	manager.SetLocked(false, "")
	upstream := httptest.NewServer(runtimeapi.NewHandler(runtimeapi.Deps{WorkspaceID: "default", Operator: auth.DigestOf("runtime-token"), Leases: manager, Health: readyHealth{}}))
	defer upstream.Close()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<title>test</title>"), 0600); err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(dir, "token")
	if err := auth.WritePrivateFile(tokenFile, []byte("runtime-token")); err != nil {
		t.Fatal(err)
	}
	edge := httptest.NewUnstartedServer(nil)
	defer edge.Close()
	origin := "http://" + edge.Listener.Addr().String()
	g, err := New(Config{Origin: origin, WebDir: dir, Identities: []Identity{{ID: "human", Role: "human", WorkspaceID: "default", TokenSHA256: digest("human-token")}, {ID: "viewer", Role: "viewer", WorkspaceID: "default", TokenSHA256: digest("viewer-token")}}, Runtimes: []RuntimeConfig{{WorkspaceID: "default", URL: upstream.URL, OperatorTokenFile: tokenFile}}}, Integrations{})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	edge.Config.Handler = g
	edge.Start()
	request := func(method, path, token, csrf string, cookie *http.Cookie, body string) *http.Response {
		t.Helper()
		r, err := http.NewRequestWithContext(context.Background(), method, edge.URL+path, bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Origin", origin)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", "request-"+method+path)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		if csrf != "" {
			r.Header.Set("X-CSRF-Token", csrf)
		}
		if cookie != nil {
			r.AddCookie(cookie)
		}
		resp, err := edge.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}
	denied := request("POST", "/api/v1/workspaces/default/leases", "viewer-token", "", nil, `{"mode":"HUMAN"}`)
	if denied.StatusCode != 403 {
		t.Fatalf("viewer lease: %d", denied.StatusCode)
	}
	cross := request("GET", "/api/v1/workspaces/other", "human-token", "", nil, "")
	if cross.StatusCode != 404 {
		t.Fatalf("cross workspace: %d", cross.StatusCode)
	}
	login := request("POST", "/api/v1/session", "", "", nil, `{"token":"human-token"}`)
	if login.StatusCode != 200 {
		t.Fatalf("login: %d", login.StatusCode)
	}
	var session struct {
		CSRF string `json:"csrfToken"`
	}
	if err := json.NewDecoder(login.Body).Decode(&session); err != nil {
		t.Fatal(err)
	}
	cookie := login.Cookies()[0]
	missing := request("POST", "/api/v1/workspaces/default/leases", "", "", cookie, `{"mode":"HUMAN"}`)
	if missing.StatusCode != 401 {
		t.Fatalf("missing CSRF: %d", missing.StatusCode)
	}
	acquired := request("POST", "/api/v1/workspaces/default/leases", "", session.CSRF, cookie, `{"mode":"HUMAN"}`)
	if acquired.StatusCode != 201 {
		b, _ := io.ReadAll(acquired.Body)
		t.Fatalf("acquire %d: %s", acquired.StatusCode, b)
	}
	var grant apiwire.LeaseGrant
	if err := json.NewDecoder(acquired.Body).Decode(&grant); err != nil {
		t.Fatal(err)
	}
	if grant.Lease.HolderID != "human" {
		t.Fatalf("untrusted holder identity: %s", grant.Lease.HolderID)
	}
	if grant.CapabilityToken != "" {
		t.Fatal("browser session received a runtime capability")
	}
	if state := manager.Status(); state.Lease == nil || state.Lease.ID != grant.Lease.LeaseID || state.State != lease.StateHuman {
		t.Fatal("runtime did not grant the authenticated human control")
	}
	stale := request("GET", "/api/v1/workspaces/default/vnc/control?epoch=0&generation=old", "", "", cookie, "")
	if stale.StatusCode != 412 {
		t.Fatalf("stale control socket accepted: %d", stale.StatusCode)
	}
	logout := request("DELETE", "/api/v1/session", "", session.CSRF, cookie, "")
	if logout.StatusCode != 204 {
		t.Fatalf("logout %d", logout.StatusCode)
	}
	if state := manager.Status(); state.Lease != nil || state.State != lease.StateIdle {
		t.Fatal("logout left runtime control authorized")
	}
	reused := request("GET", "/api/v1/session", "", "", cookie, "")
	if reused.StatusCode != 401 {
		t.Fatalf("logged out cookie reused: %d", reused.StatusCode)
	}
}

func TestGatewayAdmitsTheAgentLeaseToTheDesktopRoutesOnly(t *testing.T) {
	manager, err := lease.NewManager(lease.Config{WorkspaceID: "default"})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Shutdown()
	manager.SetLocked(false, "")
	// No VNC proxy is configured: a request that passes the Gateway's
	// checks reaches the runtime and answers 502 there.
	upstream := httptest.NewServer(runtimeapi.NewHandler(runtimeapi.Deps{WorkspaceID: "default", Operator: auth.DigestOf("runtime-token"), Leases: manager, Health: readyHealth{}}))
	defer upstream.Close()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<title>test</title>"), 0600); err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(dir, "token")
	if err := auth.WritePrivateFile(tokenFile, []byte("runtime-token")); err != nil {
		t.Fatal(err)
	}
	edge := httptest.NewUnstartedServer(nil)
	defer edge.Close()
	origin := "http://" + edge.Listener.Addr().String()
	g, err := New(Config{Origin: origin, WebDir: dir, Identities: []Identity{{ID: "bot", Role: "agent", WorkspaceID: "default", TokenSHA256: digest("agent-token")}}, Runtimes: []RuntimeConfig{{WorkspaceID: "default", URL: upstream.URL, OperatorTokenFile: tokenFile}}}, Integrations{})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	edge.Config.Handler = g
	edge.Start()
	// A CLI sends no Origin header.
	call := func(method, path, token, body string, hdr map[string]string) (*http.Response, []byte) {
		t.Helper()
		r, err := http.NewRequestWithContext(context.Background(), method, edge.URL+path, bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", "k-"+method+path)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		resp, err := edge.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, b
	}
	const ws = "/api/v1/workspaces/default"
	if resp, _ := call("GET", ws+"/vnc/control?epoch=1&generation=x", "agent-token", "", nil); resp.StatusCode != 403 {
		t.Fatalf("agent without a lease: %d, want 403", resp.StatusCode)
	}
	resp, b := call("POST", ws+"/leases", "agent-token", `{"mode":"AGENT"}`, nil)
	if resp.StatusCode != 201 {
		t.Fatalf("agent acquire %d: %s", resp.StatusCode, b)
	}
	var grant apiwire.LeaseGrant
	if err := json.Unmarshal(b, &grant); err != nil || grant.CapabilityToken == "" {
		t.Fatalf("grant = %s, %v", b, err)
	}
	epoch := strconv.FormatUint(grant.Lease.Epoch, 10)
	control := ws + "/vnc/control?epoch=" + epoch + "&generation=" + grant.Lease.RuntimeGeneration
	for name, tc := range map[string]struct {
		method, path string
		hdr          map[string]string
		want         int
	}{
		"stale epoch":        {"GET", ws + "/vnc/control?epoch=0&generation=" + grant.Lease.RuntimeGeneration, nil, 412},
		"browser origin":     {"GET", control, map[string]string{"Origin": origin}, 403},
		"view stays closed":  {"GET", ws + "/vnc/view", nil, 403},
		"resize stays human": {"PUT", ws + "/resolution", nil, 403},
		"bound control":      {"GET", control, nil, 502},
	} {
		if resp, _ := call(tc.method, tc.path, grant.CapabilityToken, `{"width":1280,"height":720}`, tc.hdr); resp.StatusCode != tc.want {
			t.Errorf("%s: %d, want %d", name, resp.StatusCode, tc.want)
		}
	}
	// Once the Agent is paused the lease is gone: the control route refuses.
	if _, err := manager.PauseAgent(); err != nil {
		t.Fatal(err)
	}
	if resp, _ := call("GET", control, grant.CapabilityToken, "", nil); resp.StatusCode < 400 {
		t.Fatalf("control route after pause: %d, want a refusal", resp.StatusCode)
	}
}
