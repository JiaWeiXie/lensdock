package product

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/gateway"
)

func destinationRecordFile(dir, id string) string {
	return filepath.Join(dir, id+gateway.DestinationRecordSuffix)
}

func writeDestinationRecord(t *testing.T, dir string, rec gateway.DestinationRecord) {
	t.Helper()
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, destinationRecordFile(dir, rec.ID), string(b), 0o644)
}

func recordFor(id, origin string) gateway.DestinationRecord {
	return gateway.DestinationRecord{Version: gateway.DestinationRecordVersion, ID: id, Name: "Peer " + id, WorkspaceID: "default", Origin: origin, PublishedAt: time.Now()}
}

// peerGateway answers the unauthenticated identity endpoint like a Gateway of
// deploymentID and returns the record that publishes it.
func peerGateway(t *testing.T, deploymentID string) gateway.DestinationRecord {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/deployment" {
			_ = json.NewEncoder(w).Encode(map[string]string{"deploymentId": deploymentID})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return recordFor(gateway.DestinationID(deploymentID, srv.URL), srv.URL)
}

// deadOrigin returns an origin whose port refuses connections.
func deadOrigin(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	origin := "http://" + ln.Addr().String()
	_ = ln.Close()
	return origin
}

func publishedIDs(dir string) []string {
	var ids []string
	for _, r := range gateway.ReadDestinationRecords(dir) {
		ids = append(ids, r.ID)
	}
	return ids
}

func TestStartMountsAndPublishesAnInstalledReleaseWithoutTouchingItsCopiedCompose(t *testing.T) {
	t.Setenv(HelperPidsEnv, "")
	dep, f := startFixture(t)
	base := dep.Compose.Files[0]
	if err := os.Chmod(base, 0o600); err != nil {
		t.Fatal(err)
	}
	writeFile(t, base, legacyReleaseCompose, 0o600)
	// An older deployment recorded no directory: its default is under HOME.
	setComposeEnv(t, dep, EnvDestinationsDir, "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	wantDir := DestinationsDirIn(home)

	if err := start(t, dep, f, ""); err != nil {
		t.Fatal(err)
	}
	if readFile(t, base) != legacyReleaseCompose {
		t.Fatal("the pinned release copy of the Compose file changed")
	}
	got, err := DeploymentDestinationsDir(dep)
	if err != nil || got != wantDir {
		t.Fatalf("recorded directory = %q, %v; want %q", got, err, wantDir)
	}
	fi, err := os.Stat(got)
	if err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("the shared directory must exist and be readable by the Gateway user: %v %v", fi, err)
	}
	records := gateway.ReadDestinationRecords(got)
	if len(records) != 1 || records[0].Origin != strings.TrimSuffix(dep.Gateway.ReadyURL, "/readyz") || records[0].WorkspaceID != DefaultWorkspace || !strings.HasPrefix(records[0].ID, "standalone-") {
		t.Fatalf("published records = %+v", records)
	}
	data := readFile(t, destinationRecordFile(got, records[0].ID))
	for _, secret := range []string{dep.StateDir, "token", "gateway.key", "operator"} {
		if strings.Contains(data, secret) {
			t.Fatalf("the published record leaks %q:\n%s", secret, data)
		}
	}

}

func TestStartRefusesAnUnrecognizedDestinationMount(t *testing.T) {
	dep, f := startFixture(t)
	dir, err := DeploymentDestinationsDir(dep)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dep.StateDir, DestinationsFile), "services: {}\n", 0o600)
	var log strings.Builder
	if err := Start(context.Background(), dep, DeploymentDocker(dep, f), StartOptions{}, &log); err == nil {
		t.Fatal("an unrecognized destination mount must not be used")
	}
	if ids := publishedIDs(dir); len(ids) != 0 {
		t.Fatalf("a deployment without the mount published %v", ids)
	}
}

func TestPublishKeepsUnavailablePeersAndDropsReplacedDeployments(t *testing.T) {
	dep, f := startFixture(t)
	dir, err := DeploymentDestinationsDir(dep)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureDirPublic(dir); err != nil {
		t.Fatal(err)
	}
	alive := peerGateway(t, "")
	managedAlive := peerGateway(t, "0123456789abcdef")
	unavailable := recordFor("standalone-1", deadOrigin(t))
	restarting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(restarting.Close)
	restartingPeer := recordFor("restarting", restarting.URL)

	// A foreign service now owns another record's port: it answers, but not
	// as the deployment the record names.
	foreign := peerGateway(t, "someone-else")
	foreign.ID, foreign.Name = "reused-port", "Reused port"
	notAGateway := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(notAGateway.Close)
	web := recordFor("web-app", notAGateway.URL)

	for _, rec := range []gateway.DestinationRecord{alive, managedAlive, unavailable, restartingPeer, foreign, web} {
		writeDestinationRecord(t, dir, rec)
	}
	if err := start(t, dep, f, ""); err != nil {
		t.Fatal(err)
	}
	got := publishedIDs(dir)
	want := map[string]bool{alive.ID: true, managedAlive.ID: true, unavailable.ID: true, restartingPeer.ID: true}
	own := ""
	for _, id := range got {
		if want[id] {
			delete(want, id)
		} else if strings.HasPrefix(id, "standalone-") && id != "standalone-1" {
			own = id
		} else {
			t.Fatalf("a replaced deployment stayed listed: %v", got)
		}
	}
	if len(want) != 0 || own == "" {
		t.Fatalf("records after publish = %v; live and temporarily unavailable peers must stay listed", got)
	}
}

func TestPublishReplacesARecordThatClaimsTheSameOrigin(t *testing.T) {
	dep, f := startFixture(t)
	dir, err := DeploymentDestinationsDir(dep)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureDirPublic(dir); err != nil {
		t.Fatal(err)
	}
	writeDestinationRecord(t, dir, recordFor("0123456789abcdef", strings.TrimSuffix(dep.Gateway.ReadyURL, "/readyz")))
	if err := start(t, dep, f, ""); err != nil {
		t.Fatal(err)
	}
	for _, id := range publishedIDs(dir) {
		if id == "0123456789abcdef" {
			t.Fatal("an older project record kept the origin of the newly started deployment")
		}
	}
	if len(publishedIDs(dir)) != 1 {
		t.Fatalf("records = %v", publishedIDs(dir))
	}
}

func TestRemoveDestinationWithdrawsOnlyThatDeployment(t *testing.T) {
	dep, f := startFixture(t)
	dir, err := DeploymentDestinationsDir(dep)
	if err != nil {
		t.Fatal(err)
	}
	peer := peerGateway(t, "")
	if err := ensureDirPublic(dir); err != nil {
		t.Fatal(err)
	}
	writeDestinationRecord(t, dir, peer)
	if err := start(t, dep, f, ""); err != nil {
		t.Fatal(err)
	}
	if len(publishedIDs(dir)) != 2 {
		t.Fatalf("records = %v", publishedIDs(dir))
	}
	if err := RemoveDestination(context.Background(), dep); err != nil {
		t.Fatal(err)
	}
	if got := publishedIDs(dir); len(got) != 1 || got[0] != peer.ID {
		t.Fatalf("records after removal = %v, want only the peer", got)
	}
	// Removing again, or a deployment that never started, is not an error.
	if err := RemoveDestination(context.Background(), dep); err != nil {
		t.Fatal(err)
	}
}

func TestInitializeRecordsTheDestinationsDirectoryAndName(t *testing.T) {
	opts := releaseOptions(t, "")
	opts.DestinationName = "my-project"
	dep, err := Initialize(opts)
	if err != nil {
		t.Fatal(err)
	}
	env := readFile(t, filepath.Join(dep.StateDir, "compose.env"))
	if !strings.Contains(env, EnvDestinationsDir+"='"+opts.DestinationsDir+"'\n") {
		t.Fatalf("compose.env does not record the directory:\n%s", env)
	}
	if got, err := DeploymentDestinationsDir(dep); err != nil || got != opts.DestinationsDir {
		t.Fatalf("directory = %q, %v", got, err)
	}
	cfg, err := deploymentGatewayConfig(dep)
	if err != nil || cfg.DestinationName != "my-project" {
		t.Fatalf("gateway.json name = %q, %v", cfg.DestinationName, err)
	}
	for _, bad := range []func(*InitOptions){
		func(o *InitOptions) { o.DestinationsDir = "relative/dir" },
		func(o *InitOptions) { o.DestinationsDir = "/tmp/a\nb" },
		func(o *InitOptions) { o.DestinationName = "bad\x00name" },
	} {
		o := releaseOptions(t, "")
		bad(&o)
		if _, err := Initialize(o); err == nil {
			t.Fatal("an unsafe destination option was accepted")
		}
	}
}
