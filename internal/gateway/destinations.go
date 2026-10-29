package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Browser destinations let the UI move a human between the LensDock Gateways
// of one machine: each local deployment is its own origin with its own session.
// The Gateway never proxies to another destination and never shares a
// credential with it. It only lists origins that the host published as safe
// metadata records in a read-only mounted directory.

const (
	// DefaultDestinationsDir is where the product Compose file mounts the host
	// directory of destination records, read-only. A missing directory (an
	// older deployment, or a host that never published one) means the current
	// destination is the only one.
	DefaultDestinationsDir = "/run/lensdock-destinations"

	// DestinationRecordVersion is the only supported record schema version.
	DestinationRecordVersion = 1

	// DestinationRecordSuffix ends every record file name; the stem is the
	// destination ID.
	DestinationRecordSuffix = ".json"

	maxDestinationRecordBytes = 4 << 10
)

var (
	destinationIDPattern        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	destinationWorkspacePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
)

// Destination is one entry of GET /api/v1/browser-destinations.
type Destination struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	WorkspaceID string `json:"workspaceId"`
	Origin      string `json:"origin"`
	Current     bool   `json:"current"`
}

// DestinationRecord is the file a host publishes for one running deployment.
// It holds no credential and no filesystem path.
type DestinationRecord struct {
	Version     int       `json:"version"`
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	WorkspaceID string    `json:"workspaceId"`
	Origin      string    `json:"origin"`
	PublishedAt time.Time `json:"publishedAt"`
}

// DestinationID derives the stable destination ID of a deployment: its
// deployment ID when managed, else "standalone-<port>" (a loopback port
// identifies the standalone deployment on its machine).
func DestinationID(deploymentID, origin string) string {
	if deploymentID != "" {
		return deploymentID
	}
	if u, err := url.Parse(origin); err == nil && u.Port() != "" {
		return "standalone-" + u.Port()
	}
	return "standalone"
}

// DefaultDestinationName names a deployment that has no configured name.
func DefaultDestinationName(deploymentID, workspaceID, origin string) string {
	port := ""
	if u, err := url.Parse(origin); err == nil && u.Port() != "" {
		port = ":" + u.Port()
	}
	if deploymentID != "" {
		short := deploymentID
		if len(short) > 8 {
			short = short[:8]
		}
		return "Project " + short
	}
	name := "LensDock"
	if workspaceID != "" && workspaceID != "default" {
		name += " " + workspaceID
	}
	return strings.TrimSpace(name + " " + port)
}

// ValidateDestinationName accepts printable names without surrounding space.
func ValidateDestinationName(name string) error {
	if name == "" || strings.TrimSpace(name) != name || !utf8.ValidString(name) {
		return errors.New("gateway: destination name must be printable without surrounding space")
	}
	for _, r := range name {
		if unicode.IsControl(r) || !unicode.IsPrint(r) {
			return errors.New("gateway: destination name must be printable")
		}
	}
	return nil
}

// ValidateDestinationOrigin accepts only a clean loopback HTTP origin with an
// explicit canonical port: the origins the product itself can publish.
func ValidateDestinationOrigin(origin string) error {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "http" || u.Host == "" || u.User != nil || u.Path != "" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return errors.New("gateway: destination origin must be a clean HTTP origin")
	}
	switch u.Hostname() {
	case "127.0.0.1", "localhost", "::1":
	default:
		return errors.New("gateway: destination origin must be loopback")
	}
	port, perr := strconv.Atoi(u.Port())
	if perr != nil || port < 1 || port > 65535 || port == 80 || strconv.Itoa(port) != u.Port() {
		return errors.New("gateway: destination origin requires an explicit canonical non-default port")
	}
	if origin != "http://"+u.Host {
		return errors.New("gateway: destination origin is not canonical")
	}
	return nil
}

// Validate checks every field of a record.
func (r DestinationRecord) Validate() error {
	switch {
	case r.Version != DestinationRecordVersion:
		return errors.New("gateway: unsupported destination record version")
	case !destinationIDPattern.MatchString(r.ID):
		return errors.New("gateway: invalid destination ID")
	case !destinationWorkspacePattern.MatchString(r.WorkspaceID):
		return errors.New("gateway: invalid destination workspace")
	case r.PublishedAt.IsZero():
		return errors.New("gateway: destination record has no publication time")
	}
	if err := ValidateDestinationName(r.Name); err != nil {
		return err
	}
	return ValidateDestinationOrigin(r.Origin)
}

// DestinationRecordOf returns the record that publishes c's deployment. Only a
// Gateway with one workspace and a publishable origin has one.
func DestinationRecordOf(c Config, now time.Time) (DestinationRecord, error) {
	if len(c.Runtimes) != 1 {
		return DestinationRecord{}, errors.New("gateway: only a single-workspace deployment is published as a destination")
	}
	ws := c.Runtimes[0].WorkspaceID
	name := c.DestinationName
	if name == "" {
		name = DefaultDestinationName(c.DeploymentID, ws, c.Origin)
	}
	r := DestinationRecord{Version: DestinationRecordVersion, ID: DestinationID(c.DeploymentID, c.Origin), Name: name, WorkspaceID: ws, Origin: c.Origin, PublishedAt: now.UTC()}
	return r, r.Validate()
}

// ReadDestinationRecords returns the valid records of dir, newest first per
// origin deduplicated and ordered by name then ID. An absent or unreadable
// directory yields none. Every file is checked on its own: a malformed,
// oversized, mismatched or non-regular entry is dropped, never repaired, so a
// damaged file cannot produce a link.
func ReadDestinationRecords(dir string) []DestinationRecord {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	byOrigin := map[string]DestinationRecord{}
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || !strings.HasSuffix(name, DestinationRecordSuffix) {
			continue
		}
		rec, ok := readDestinationRecord(filepath.Join(dir, name), strings.TrimSuffix(name, DestinationRecordSuffix))
		if !ok {
			continue
		}
		if prev, dup := byOrigin[rec.Origin]; dup && !newerRecord(rec, prev) {
			continue
		}
		byOrigin[rec.Origin] = rec
	}
	out := make([]DestinationRecord, 0, len(byOrigin))
	for _, rec := range byOrigin {
		out = append(out, rec)
	}
	slices.SortFunc(out, func(a, b DestinationRecord) int {
		if c := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

func newerRecord(a, b DestinationRecord) bool {
	if !a.PublishedAt.Equal(b.PublishedAt) {
		return a.PublishedAt.After(b.PublishedAt)
	}
	return a.ID > b.ID
}

func readDestinationRecord(path, stem string) (DestinationRecord, bool) {
	f, err := os.Open(path)
	if err != nil {
		return DestinationRecord{}, false
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() || st.Size() > maxDestinationRecordBytes {
		return DestinationRecord{}, false
	}
	b, err := io.ReadAll(io.LimitReader(f, maxDestinationRecordBytes+1))
	if err != nil || len(b) > maxDestinationRecordBytes {
		return DestinationRecord{}, false
	}
	var rec DestinationRecord
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if dec.Decode(&rec) != nil || dec.More() || rec.ID != stem || rec.Validate() != nil {
		return DestinationRecord{}, false
	}
	return rec, true
}

// browserDestinations answers GET /api/v1/browser-destinations: this Gateway's
// own destination, always first, then the other published deployments. Agent
// identities never enumerate other projects.
func (g *Gateway) browserDestinations(w http.ResponseWriter, r *http.Request, p Identity) {
	if r.Method != "GET" {
		methodNotAllowed(w, "GET")
		return
	}
	if p.Role == "agent" {
		fail(w, 403, "FORBIDDEN", "agent identities cannot list browser destinations")
		return
	}
	id := DestinationID(g.config.DeploymentID, g.config.Origin)
	name := g.config.DestinationName
	if name == "" {
		name = DefaultDestinationName(g.config.DeploymentID, p.WorkspaceID, g.config.Origin)
	}
	list := []Destination{{ID: id, Name: name, WorkspaceID: p.WorkspaceID, Origin: g.config.Origin, Current: true}}
	dir := g.config.DestinationsDir
	if dir == "" {
		dir = DefaultDestinationsDir
	}
	for _, rec := range ReadDestinationRecords(dir) {
		if rec.ID == id || rec.Origin == g.config.Origin {
			continue
		}
		list = append(list, Destination{ID: rec.ID, Name: rec.Name, WorkspaceID: rec.WorkspaceID, Origin: rec.Origin})
	}
	jsonReply(w, 200, map[string][]Destination{"destinations": list})
}
