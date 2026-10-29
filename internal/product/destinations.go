package product

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/auth"
	"github.com/JiaWeiXie/lensdock/internal/gateway"
	"github.com/JiaWeiXie/lensdock/internal/update"
)

// Browser destinations: every started deployment publishes one small record
// (ID, name, workspace and loopback origin, no secret and no path) into a host
// directory that each Gateway mounts read-only, so its UI can offer the other
// local deployments. The Gateway only reads; the host side alone writes, and
// it publishes only ready deployments and preserves peers during temporary
// connection failures or Gateway restarts.

const (
	// EnvDestinationsDir names the host directory of destination records. A
	// deployment records it in compose.env, which then wins over the process
	// environment like every other deployment input.
	EnvDestinationsDir = "LENSDOCK_DESTINATIONS_DIR"

	// DestinationsFile is the Compose override, in the product directory, that
	// mounts the host directory read-only into the Gateway. The copied
	// docker/compose.yaml of an installed release is kept byte for byte and
	// predates the mount, so an existing deployment picks it up through this
	// file at its next start, keeping its volumes, credentials and images.
	DestinationsFile = "destinations.yaml"

	destinationsOverlay = "# Mounts the host directory of published LensDock destinations read-only into the Gateway so its UI\n# can list the other local deployments. Written by the product start; the directory comes from\n# LENSDOCK_DESTINATIONS_DIR in compose.env and holds only non-secret destination metadata.\nservices:\n  gateway:\n    volumes:\n      - ${LENSDOCK_DESTINATIONS_DIR:?the destinations directory is required}:" + gateway.DefaultDestinationsDir + ":ro\n"

	destinationProbeTimeout = 1500 * time.Millisecond
)

// DestinationsDirIn is the per-user directory shared by every deployment of
// a machine, under home, when none is configured.
func DestinationsDirIn(home string) string {
	return filepath.Join(home, ".local", "share", "lensdock-destinations")
}

// DefaultDestinationsDir is DestinationsDirIn the current user's home.
func DefaultDestinationsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home directory: %w", err)
	}
	return DestinationsDirIn(home), nil
}

// CheckDestinationsDir accepts a canonical absolute directory path that can
// be recorded in compose.env.
func CheckDestinationsDir(dir string) error {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir || dir == string(filepath.Separator) || strings.ContainsAny(dir, "\x00\r\n") {
		return fmt.Errorf("destinations directory %q must be a canonical absolute path", dir)
	}
	return nil
}

// ResolveDestinationsDir returns dir when set, else the default.
func ResolveDestinationsDir(dir string) (string, error) {
	if dir == "" {
		return DefaultDestinationsDir()
	}
	if err := CheckDestinationsDir(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// DeploymentDestinationsDir returns the directory dep publishes to: its
// compose.env value, else the default (the directory an older deployment that
// never recorded one is given at its next start).
func DeploymentDestinationsDir(dep *update.Deployment) (string, error) {
	dir := os.Getenv(EnvDestinationsDir)
	for _, entry := range ComposeEnv(dep) {
		if k, v, ok := strings.Cut(entry, "="); ok && k == EnvDestinationsDir {
			dir = v
		}
	}
	return ResolveDestinationsDir(dir)
}

// deploymentGatewayConfig reads the Gateway configuration of dep, which is the
// source of the identity the Gateway itself reports as its current destination.
func deploymentGatewayConfig(dep *update.Deployment) (gateway.Config, error) {
	var c gateway.Config
	b, err := auth.ReadPrivateFile(filepath.Join(dep.StateDir, "gateway.json"), 1<<20)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, errors.New("invalid gateway.json")
	}
	return c, nil
}

// ensureDestinationsMount makes dep's Gateway mount the destinations
// directory: it records the directory in compose.env when missing, creates the
// directory (so Docker never creates it as another owner), writes the fixed
// override and lists it after the deployment's Compose files. Nothing else of
// the deployment changes, so an existing install keeps its volumes, images and
// credentials.
func ensureDestinationsMount(dep *update.Deployment) error {
	envFile := filepath.Join(dep.StateDir, "compose.env")
	listed := false
	for _, f := range dep.Compose.EnvFiles {
		listed = listed || f == envFile
	}
	if !listed {
		return fmt.Errorf("%s is not a Compose env file of this deployment", envFile)
	}
	dir, err := DeploymentDestinationsDir(dep)
	if err != nil {
		return err
	}
	if err := ensureDirPublic(dir); err != nil {
		return err
	}
	data, err := auth.ReadPrivateFile(envFile, maxComposeEnvLen)
	if err != nil {
		return err
	}
	recorded := false
	for _, line := range strings.Split(string(data), "\n") {
		if k, v, ok := parseEnvLine(line); ok && k == EnvDestinationsDir && v != "" {
			recorded = true
		}
	}
	if !recorded {
		text := string(data)
		if text != "" && !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		text += EnvDestinationsDir + "=" + composeEnvValue(dir) + "\n"
		if err := writeFileAtomic(envFile, []byte(text), 0o600); err != nil {
			return err
		}
	}
	file := filepath.Join(dep.StateDir, DestinationsFile)
	existing, err := os.ReadFile(file)
	switch {
	case err == nil:
		if string(existing) != destinationsOverlay {
			return fmt.Errorf("%s differs from the destinations mount this deployment owns; refusing to use it", file)
		}
	case os.IsNotExist(err):
		if err := auth.WritePrivateFile(file, []byte(destinationsOverlay)); err != nil {
			return err
		}
	default:
		return err
	}
	for _, f := range dep.Compose.Files {
		if f == file {
			return nil
		}
	}
	dep.Compose.Files = append(dep.Compose.Files, file)
	return dep.Save()
}

// ensureDirPublic creates dir (and parents) readable by every user, because
// the Gateway reads it as an unprivileged container user. An existing
// directory is left as it is.
func ensureDirPublic(dir string) error {
	if fi, err := os.Stat(dir); err == nil {
		if !fi.IsDir() {
			return fmt.Errorf("%s is not a directory", dir)
		}
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.Chmod(dir, 0o755)
}

// destinationOf returns the record and directory of dep.
func destinationOf(dep *update.Deployment) (gateway.DestinationRecord, string, error) {
	c, err := deploymentGatewayConfig(dep)
	if err != nil {
		return gateway.DestinationRecord{}, "", err
	}
	rec, err := gateway.DestinationRecordOf(c, time.Now())
	if err != nil {
		return gateway.DestinationRecord{}, "", err
	}
	dir, err := DeploymentDestinationsDir(dep)
	return rec, dir, err
}

// PublishDestination makes the running deployment dep visible to every Gateway
// that mounts the destinations directory, replaces any other record claiming
// its origin and drops records whose origin answers as another deployment.
// Call it only once dep's Gateway is ready.
func PublishDestination(ctx context.Context, dep *update.Deployment) error {
	rec, dir, err := destinationOf(dep)
	if err != nil {
		return err
	}
	if err := ensureDirPublic(dir); err != nil {
		return err
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(dir, rec.ID+gateway.DestinationRecordSuffix), append(data, '\n'), 0o644); err != nil {
		return err
	}
	pruneDestinations(ctx, dir, rec.ID, rec.Origin)
	return nil
}

// RemoveDestination withdraws dep from the destinations directory (it is
// stopped or no longer authorized) and drops records whose origin answers as
// another deployment. A deployment that never published has nothing to remove.
func RemoveDestination(ctx context.Context, dep *update.Deployment) error {
	c, err := deploymentGatewayConfig(dep)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	dir, err := DeploymentDestinationsDir(dep)
	if err != nil {
		return err
	}
	id := gateway.DestinationID(c.DeploymentID, c.Origin)
	if err := os.Remove(filepath.Join(dir, id+gateway.DestinationRecordSuffix)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	pruneDestinations(ctx, dir, id, c.Origin)
	return nil
}

// pruneDestinations removes every other valid record that claims origin or
// whose origin answers as another service or deployment. Unavailable peers
// retain their records during restarts. It only contacts origins named by
// records; no port range is scanned.
func pruneDestinations(ctx context.Context, dir, selfID, selfOrigin string) {
	records := gateway.ReadDestinationRecords(dir)
	var wg sync.WaitGroup
	for _, rec := range records {
		if rec.ID == selfID {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if rec.Origin == selfOrigin || !destinationAnswers(ctx, rec) {
				_ = os.Remove(filepath.Join(dir, rec.ID+gateway.DestinationRecordSuffix))
			}
		}()
	}
	wg.Wait()
}

// destinationAnswers reports whether rec's origin currently answers as the
// deployment the record names: its unauthenticated identity endpoint must
// return the deployment ID that yields the record's ID. Transport failures,
// server errors and incomplete responses are unknown and keep the record.
func destinationAnswers(parent context.Context, rec gateway.DestinationRecord) bool {
	ctx, cancel := context.WithTimeout(parent, destinationProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rec.Origin+"/api/v1/deployment", nil)
	if err != nil {
		return false
	}
	client := &http.Client{
		Transport:     &http.Transport{Proxy: nil, DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return true
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusInternalServerError {
		return true
	}
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var got struct {
		DeploymentID *string `json:"deploymentId"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<10)).Decode(&got) != nil {
		return true
	}
	if got.DeploymentID == nil {
		return false
	}
	return gateway.DestinationID(*got.DeploymentID, rec.Origin) == rec.ID
}
