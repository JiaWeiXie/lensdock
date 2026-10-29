package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/auth"
)

func destinationRecord(id, name, workspace, origin string, at time.Time) DestinationRecord {
	return DestinationRecord{Version: DestinationRecordVersion, ID: id, Name: name, WorkspaceID: workspace, Origin: origin, PublishedAt: at}
}

func writeDestinationFile(t *testing.T, dir, file string, v any) {
	t.Helper()
	b, ok := v.([]byte)
	if !ok {
		var err error
		if b, err = json.Marshal(v); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, file), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

type destinationGateway struct {
	origin string
	dir    string
}

// startDestinationGateway serves a real Gateway whose Runtime reports NOVNC
// mode, the state in which every Agent route is otherwise closed.
func startDestinationGateway(t *testing.T, deploymentID string, publish func(dir, origin string)) *destinationGateway {
	t.Helper()
	web := t.TempDir()
	if err := os.WriteFile(filepath.Join(web, "index.html"), []byte("<title>test</title>"), 0o600); err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(web, "runtime-token")
	if err := auth.WritePrivateFile(tokenFile, []byte("runtime-secret")); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/workspaces/alpha/agent" {
			_, _ = io.WriteString(w, `{"mode":"NOVNC","paused":false,"revision":1,"updatedAt":"2026-01-01T00:00:00Z"}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(upstream.Close)
	edge := httptest.NewUnstartedServer(nil)
	t.Cleanup(edge.Close)
	origin := "http://" + edge.Listener.Addr().String()
	dir := t.TempDir()
	if publish != nil {
		publish(dir, origin)
	}
	g, err := New(Config{
		Origin: origin, DeploymentID: deploymentID, WebDir: web, RequirePairing: true, DestinationsDir: dir, DestinationName: "Current project",
		Identities: []Identity{
			{ID: "viewer", Role: "viewer", WorkspaceID: "alpha", TokenSHA256: digest("viewer-token")},
			{ID: "human", Role: "human", WorkspaceID: "alpha", TokenSHA256: digest("human-token")},
			{ID: "admin", Role: "admin", WorkspaceID: "alpha", TokenSHA256: digest("admin-token")},
			{ID: "agent", Role: "agent", WorkspaceID: "alpha", TokenSHA256: digest("agent-token")},
		},
		Runtimes: []RuntimeConfig{{WorkspaceID: "alpha", URL: upstream.URL, OperatorTokenFile: tokenFile}},
	}, Integrations{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.Close)
	edge.Config.Handler = g
	edge.Start()
	return &destinationGateway{origin: origin, dir: dir}
}

func (g *destinationGateway) get(t *testing.T, method, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, g.origin+"/api/v1/browser-destinations", nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, b
}

func decodeDestinations(t *testing.T, body []byte) []Destination {
	t.Helper()
	var reply struct {
		Destinations []Destination `json:"destinations"`
	}
	if err := json.Unmarshal(body, &reply); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return reply.Destinations
}

func TestBrowserDestinationsListCurrentFirstForEveryHumanRoleEvenInNoVNC(t *testing.T) {
	now := time.Now()
	g := startDestinationGateway(t, "proj-a", func(dir, origin string) {
		writeDestinationFile(t, dir, "proj-b.json", destinationRecord("proj-b", "Beta", "beta", "http://127.0.0.1:19082", now))
		writeDestinationFile(t, dir, "standalone-18081.json", destinationRecord("standalone-18081", "alpha site", "default", "http://127.0.0.1:18081", now))
		// A stale record of this very deployment, and another claiming its origin,
		// must never be offered as a second destination.
		writeDestinationFile(t, dir, "proj-a.json", destinationRecord("proj-a", "Old name", "alpha", "http://127.0.0.1:19999", now))
		writeDestinationFile(t, dir, "impostor.json", destinationRecord("impostor", "Impostor", "evil", origin, now))
	})
	for _, token := range []string{"viewer-token", "human-token", "admin-token"} {
		status, body := g.get(t, "GET", token)
		if status != 200 {
			t.Fatalf("%s: %d %s", token, status, body)
		}
		got := decodeDestinations(t, body)
		want := []Destination{
			{ID: "proj-a", Name: "Current project", WorkspaceID: "alpha", Origin: g.origin, Current: true},
			{ID: "standalone-18081", Name: "alpha site", WorkspaceID: "default", Origin: "http://127.0.0.1:18081"},
			{ID: "proj-b", Name: "Beta", WorkspaceID: "beta", Origin: "http://127.0.0.1:19082"},
		}
		if len(got) != len(want) {
			t.Fatalf("%s: destinations = %+v, want %+v", token, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s: destination %d = %+v, want %+v", token, i, got[i], want[i])
			}
		}
		var raw struct {
			Destinations []map[string]any `json:"destinations"`
		}
		if err := json.Unmarshal(body, &raw); err != nil {
			t.Fatal(err)
		}
		for _, d := range raw.Destinations {
			if len(d) != 5 {
				t.Fatalf("a destination exposes unexpected fields: %v", d)
			}
		}
	}
}

func TestBrowserDestinationsNeverEnumerateForAgentsOrAnonymousCallers(t *testing.T) {
	g := startDestinationGateway(t, "proj-a", func(dir, origin string) {
		writeDestinationFile(t, dir, "proj-b.json", destinationRecord("proj-b", "Secret sibling", "beta", "http://127.0.0.1:19082", time.Now()))
	})
	status, body := g.get(t, "GET", "agent-token")
	if status != 403 || strings.Contains(string(body), "Secret sibling") || strings.Contains(string(body), "19082") {
		t.Fatalf("agent: %d %s", status, body)
	}
	if status, _ := g.get(t, "GET", ""); status != 401 {
		t.Fatalf("anonymous: %d", status)
	}
	if status, _ := g.get(t, "GET", "wrong-token"); status != 401 {
		t.Fatalf("unknown token: %d", status)
	}
	if status, _ := g.get(t, "POST", "human-token"); status != 405 {
		t.Fatalf("POST: %d", status)
	}
}

func TestBrowserDestinationsOfAnOlderDeploymentWithoutMountListOnlyItself(t *testing.T) {
	g := startDestinationGateway(t, "", nil)
	if err := os.RemoveAll(g.dir); err != nil {
		t.Fatal(err)
	}
	status, body := g.get(t, "GET", "viewer-token")
	if status != 200 {
		t.Fatalf("%d %s", status, body)
	}
	got := decodeDestinations(t, body)
	if len(got) != 1 || !got[0].Current || got[0].Origin != g.origin || got[0].WorkspaceID != "alpha" || !strings.HasPrefix(got[0].ID, "standalone-") {
		t.Fatalf("destinations = %+v", got)
	}
}

func TestReadDestinationRecordsDropsEverythingUnsafe(t *testing.T) {
	now := time.Now()
	valid := destinationRecord("ok", "Fine", "alpha", "http://127.0.0.1:19081", now)
	dir := t.TempDir()
	write := func(file string, v any) { writeDestinationFile(t, dir, file, v) }
	with := func(mutate func(*DestinationRecord)) DestinationRecord {
		r := valid
		mutate(&r)
		return r
	}
	write("ok.json", valid)
	for file, rec := range map[string]DestinationRecord{
		"https.json":    with(func(r *DestinationRecord) { r.ID = "https"; r.Origin = "https://127.0.0.1:19081" }),
		"remote.json":   with(func(r *DestinationRecord) { r.ID = "remote"; r.Origin = "http://evil.example:19081" }),
		"path.json":     with(func(r *DestinationRecord) { r.ID = "path"; r.Origin = "http://127.0.0.1:19082/app" }),
		"slash.json":    with(func(r *DestinationRecord) { r.ID = "slash"; r.Origin = "http://127.0.0.1:19083/" }),
		"noport.json":   with(func(r *DestinationRecord) { r.ID = "noport"; r.Origin = "http://127.0.0.1" }),
		"port80.json":   with(func(r *DestinationRecord) { r.ID = "port80"; r.Origin = "http://127.0.0.1:80" }),
		"userinfo.json": with(func(r *DestinationRecord) { r.ID = "userinfo"; r.Origin = "http://user:pw@127.0.0.1:19084" }),
		"script.json":   with(func(r *DestinationRecord) { r.ID = "script"; r.Origin = "javascript:alert(1)" }),
		"leadzero.json": with(func(r *DestinationRecord) { r.ID = "leadzero"; r.Origin = "http://127.0.0.1:019085" }),
		"mismatch.json": with(func(r *DestinationRecord) { r.ID = "other-id"; r.Origin = "http://127.0.0.1:19086" }),
		"version.json":  with(func(r *DestinationRecord) { r.ID = "version"; r.Version = 2; r.Origin = "http://127.0.0.1:19087" }),
		"control.json": with(func(r *DestinationRecord) {
			r.ID = "control"
			r.Name = "bad\nname"
			r.Origin = "http://127.0.0.1:19088"
		}),
		"emptyname.json": with(func(r *DestinationRecord) { r.ID = "emptyname"; r.Name = ""; r.Origin = "http://127.0.0.1:19089" }),
		"badws.json": with(func(r *DestinationRecord) {
			r.ID = "badws"
			r.WorkspaceID = "../x"
			r.Origin = "http://127.0.0.1:19090"
		}),
		"unpublished.json": with(func(r *DestinationRecord) {
			r.ID = "unpublished"
			r.PublishedAt = time.Time{}
			r.Origin = "http://127.0.0.1:19091"
		}),
	} {
		write(file, rec)
	}
	write("unknown.json", []byte(`{"version":1,"id":"unknown","name":"X","workspaceId":"alpha","origin":"http://127.0.0.1:19092","publishedAt":"2026-01-01T00:00:00Z","token":"secret"}`))
	write("trailing.json", []byte(`{"version":1,"id":"trailing","name":"X","workspaceId":"alpha","origin":"http://127.0.0.1:19093","publishedAt":"2026-01-01T00:00:00Z"} {}`))
	write("garbage.json", []byte("not json"))
	write("huge.json", []byte(`{"version":1,"id":"huge","name":"`+strings.Repeat("x", 8<<10)+`"}`))
	write("notes.txt", valid)
	if err := os.Mkdir(filepath.Join(dir, "dir.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "outside.json")
	writeDestinationFile(t, filepath.Dir(target), filepath.Base(target), destinationRecord("link", "Linked", "alpha", "http://127.0.0.1:19094", now))
	if err := os.Symlink(target, filepath.Join(dir, "link.json")); err != nil {
		t.Fatal(err)
	}
	got := ReadDestinationRecords(dir)
	if len(got) != 1 || got[0].ID != "ok" {
		t.Fatalf("records = %+v, want only the valid one", got)
	}
	if got := ReadDestinationRecords(filepath.Join(dir, "absent")); len(got) != 0 {
		t.Fatalf("absent directory yielded %+v", got)
	}
}

func TestReadDestinationRecordsKeepsTheNewestRecordPerOrigin(t *testing.T) {
	dir := t.TempDir()
	older := time.Now().Add(-time.Hour)
	newer := time.Now()
	writeDestinationFile(t, dir, "old-owner.json", destinationRecord("old-owner", "Old owner", "alpha", "http://127.0.0.1:19081", older))
	writeDestinationFile(t, dir, "new-owner.json", destinationRecord("new-owner", "New owner", "alpha", "http://127.0.0.1:19081", newer))
	got := ReadDestinationRecords(dir)
	if len(got) != 1 || got[0].ID != "new-owner" {
		t.Fatalf("records = %+v, want the newest owner of the origin", got)
	}
}

func TestDestinationRecordOfNeedsOneWorkspaceAndAPublishableOrigin(t *testing.T) {
	now := time.Now()
	one := Config{Origin: "http://127.0.0.1:18081", Runtimes: []RuntimeConfig{{WorkspaceID: "default"}}}
	rec, err := DestinationRecordOf(one, now)
	if err != nil || rec.ID != "standalone-18081" || rec.WorkspaceID != "default" || rec.Name != "LensDock :18081" {
		t.Fatalf("standalone record = %+v, %v", rec, err)
	}
	managed := Config{Origin: "http://127.0.0.1:19081", DeploymentID: "0123456789abcdef", DestinationName: "my-project", Runtimes: []RuntimeConfig{{WorkspaceID: "0123456789abcdef"}}}
	if rec, err = DestinationRecordOf(managed, now); err != nil || rec.ID != "0123456789abcdef" || rec.Name != "my-project" {
		t.Fatalf("managed record = %+v, %v", rec, err)
	}
	many := one
	many.Runtimes = append(many.Runtimes, RuntimeConfig{WorkspaceID: "other"})
	if _, err = DestinationRecordOf(many, now); err == nil {
		t.Fatal("a multi-workspace Gateway was published")
	}
	remote := one
	remote.Origin = "https://lensdock.example.com"
	if _, err = DestinationRecordOf(remote, now); err == nil {
		t.Fatal("a remote origin was published")
	}
}
