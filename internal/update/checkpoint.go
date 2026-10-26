package update

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	checkpointSchemaVersion = 1
	checkpointFileName      = "checkpoint.json"
	incompletePrefix        = ".incomplete-"

	// ConsistencyClean marks a checkpoint taken after a verified clean stop.
	ConsistencyClean = "clean"
	// ConsistencyCrash marks a snapshot of a store that was not cleanly
	// closed. It is never offered as a clean backup.
	ConsistencyCrash = "crash"
)

var datasetFilePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,150}\.tar\.sealed$`)

// CleanEvidence documents why a checkpoint is (not) clean.
type CleanEvidence struct {
	RuntimeExitCode *int `json:"runtimeExitCode,omitempty"`
	GatewayExitCode *int `json:"gatewayExitCode,omitempty"`
	// ProfileExitType is Chromium's profile.exit_type from Default/Preferences
	// ("Normal" or "SessionEnded" after a clean exit), empty for a new profile.
	ProfileExitType  string   `json:"profileExitType,omitempty"`
	SingletonEntries []string `json:"singletonEntries,omitempty"`
	HotJournals      []string `json:"hotJournals,omitempty"`
	// Reasons lists every finding that makes the checkpoint a crash snapshot.
	Reasons []string `json:"reasons,omitempty"`
}

// CheckpointDataset is one sealed volume copy of a checkpoint.
type CheckpointDataset struct {
	Owner        string      `json:"owner"`
	Key          string      `json:"key"`
	Kind         string      `json:"kind"`
	SourceVolume string      `json:"sourceVolume"`
	File         string      `json:"file"`
	SealedSHA256 string      `json:"sealedSha256"`
	SealedBytes  int64       `json:"sealedBytes"`
	PlainSHA256  string      `json:"plainSha256"`
	PlainBytes   int64       `json:"plainBytes"`
	Entries      int         `json:"entries"`
	Fingerprint  Fingerprint `json:"fingerprint"`
}

// Checkpoint is a consistent, encrypted copy of the snapshot datasets of one
// workspace and the Gateway database, together with the state (images and
// volumes) it belongs to. Restoring it is the only rollback.
type Checkpoint struct {
	SchemaVersion int       `json:"schemaVersion"`
	ID            string    `json:"id"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdateID      string    `json:"updateId"`
	Workspace     string    `json:"workspace"`
	// Consistency is ConsistencyClean or ConsistencyCrash.
	Consistency   string        `json:"consistency"`
	Clean         CleanEvidence `json:"clean"`
	Previous      State         `json:"previous"`
	TargetRelease string        `json:"targetRelease,omitempty"`
	// ChromiumBefore and GatewayDBSchemaBefore describe the paired old
	// release when known (0/"" otherwise).
	ChromiumBefore        string              `json:"chromiumBefore,omitempty"`
	GatewayDBSchemaBefore int                 `json:"gatewayDbSchemaBefore,omitempty"`
	KeyID                 string              `json:"keyId"`
	Datasets              []CheckpointDataset `json:"datasets"`
	LastVerifiedAt        *time.Time          `json:"lastVerifiedAt,omitempty"`
	RetainedNotRolledBack []string            `json:"retainedNotRolledBack,omitempty"`
}

// TotalSealedBytes sums the sealed file sizes.
func (c Checkpoint) TotalSealedBytes() int64 {
	var n int64
	for _, d := range c.Datasets {
		n += d.SealedBytes
	}
	return n
}

// Dataset returns the dataset of owner and key.
func (c Checkpoint) Dataset(owner, key string) (CheckpointDataset, bool) {
	for _, d := range c.Datasets {
		if d.Owner == owner && d.Key == key {
			return d, true
		}
	}
	return CheckpointDataset{}, false
}

// CheckpointStore is the backup directory.
type CheckpointStore struct {
	Dir string
}

func (s CheckpointStore) path(id string) (string, error) {
	if !checkpointIDPattern.MatchString(id) {
		return "", fmt.Errorf("invalid checkpoint id %q", id)
	}
	return filepath.Join(s.Dir, id), nil
}

// Load reads and validates a checkpoint's metadata.
func (s CheckpointStore) Load(id string) (Checkpoint, error) {
	dir, err := s.path(id)
	if err != nil {
		return Checkpoint{}, err
	}
	data, err := readFileLimit(filepath.Join(dir, checkpointFileName), 4<<20)
	if err != nil {
		return Checkpoint{}, fmt.Errorf("read checkpoint %s: %w", id, err)
	}
	var cp Checkpoint
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cp); err != nil {
		return Checkpoint{}, fmt.Errorf("decode checkpoint %s: %w", id, err)
	}
	if err := ensureJSONEOF(dec); err != nil {
		return Checkpoint{}, fmt.Errorf("decode checkpoint %s: %w", id, err)
	}
	if cp.SchemaVersion != checkpointSchemaVersion || cp.ID != id {
		return Checkpoint{}, fmt.Errorf("checkpoint %s: unsupported schema or mismatching id", id)
	}
	if cp.Consistency != ConsistencyClean && cp.Consistency != ConsistencyCrash {
		return Checkpoint{}, fmt.Errorf("checkpoint %s: unknown consistency %q", id, cp.Consistency)
	}
	for _, ds := range cp.Datasets {
		if !datasetFilePattern.MatchString(ds.File) {
			return Checkpoint{}, fmt.Errorf("checkpoint %s: invalid dataset file name %q", id, ds.File)
		}
	}
	return cp, nil
}

// CheckpointInventory is a classified view of the backup directory. Malformed
// holds checkpoint directories whose metadata cannot be read or validated: they
// are neither complete nor provably abandoned, so only an explicit prune
// removes them.
type CheckpointInventory struct {
	Complete   []Checkpoint
	Incomplete []string
	Malformed  []string
}

// Inventory classifies every directory of the store. Complete is newest first.
func (s CheckpointStore) Inventory() (CheckpointInventory, error) {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return CheckpointInventory{}, nil
		}
		return CheckpointInventory{}, err
	}
	var inv CheckpointInventory
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		switch {
		case strings.HasPrefix(e.Name(), incompletePrefix):
			inv.Incomplete = append(inv.Incomplete, e.Name())
		case checkpointIDPattern.MatchString(e.Name()):
			cp, err := s.Load(e.Name())
			if err != nil {
				inv.Malformed = append(inv.Malformed, e.Name())
				continue
			}
			inv.Complete = append(inv.Complete, cp)
		}
	}
	sort.Slice(inv.Complete, func(a, b int) bool { return inv.Complete[a].CreatedAt.After(inv.Complete[b].CreatedAt) })
	sort.Strings(inv.Incomplete)
	sort.Strings(inv.Malformed)
	return inv, nil
}

// List returns the complete checkpoints, newest first, and the names of
// leftover incomplete or unreadable directories.
func (s CheckpointStore) List() ([]Checkpoint, []string, error) {
	inv, err := s.Inventory()
	if err != nil {
		return nil, nil, err
	}
	incomplete := append(inv.Incomplete, inv.Malformed...)
	sort.Strings(incomplete)
	return inv.Complete, incomplete, nil
}

// Usage returns the number of checkpoints and the bytes below the backup
// directory.
func (s CheckpointStore) Usage() (int, int64, error) {
	cps, _, err := s.List()
	if err != nil {
		return 0, 0, err
	}
	size, err := dirSize(s.Dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return len(cps), 0, err
	}
	return len(cps), size, nil
}

// countWriter counts bytes.
type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }

// CheckpointWriter builds a checkpoint in a hidden directory and publishes it
// with one atomic rename once everything is written and verified.
type CheckpointWriter struct {
	store CheckpointStore
	id    string
	dir   string
	key   []byte
}

// Begin creates the incomplete directory of checkpoint id.
func (s CheckpointStore) Begin(id string, key []byte) (*CheckpointWriter, error) {
	if _, err := s.path(id); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("create backup directory: %w", err)
	}
	dir := filepath.Join(s.Dir, incompletePrefix+id)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create checkpoint directory: %w", err)
	}
	return &CheckpointWriter{store: s, id: id, dir: dir, key: key}, nil
}

func datasetFileName(owner, key string) string {
	return strings.NewReplacer("/", "_").Replace(owner) + "_" + key + ".tar.sealed"
}

// WriteDataset seals the volume into the checkpoint directory: it fingerprints
// the volume, streams a tar archive through AES-GCM into a new file, then
// decrypts the file again and checks its length, hash and tar structure.
func (w *CheckpointWriter) WriteDataset(ctx context.Context, vh VolumeHelper, owner, kind, key, volume string) (CheckpointDataset, error) {
	name := datasetFileName(owner, key)
	if !datasetFilePattern.MatchString(name) {
		return CheckpointDataset{}, fmt.Errorf("dataset %s/%s yields an invalid file name", owner, key)
	}
	fp, err := vh.Fingerprint(ctx, volume)
	if err != nil {
		return CheckpointDataset{}, err
	}
	path := filepath.Join(w.dir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return CheckpointDataset{}, fmt.Errorf("create sealed file: %w", err)
	}
	sealedHash := sha256.New()
	sealedCount := &countWriter{}
	sw, err := NewSealWriter(io.MultiWriter(f, sealedHash, sealedCount), w.key)
	if err != nil {
		_ = f.Close()
		return CheckpointDataset{}, err
	}
	plainHash := sha256.New()
	plainCount := &countWriter{}
	if err := vh.TarOut(ctx, volume, io.MultiWriter(sw, plainHash, plainCount)); err != nil {
		_ = f.Close()
		return CheckpointDataset{}, fmt.Errorf("archive volume %s: %w", volume, err)
	}
	if err := sw.Close(); err != nil {
		_ = f.Close()
		return CheckpointDataset{}, fmt.Errorf("finish sealed file: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return CheckpointDataset{}, fmt.Errorf("sync sealed file: %w", err)
	}
	if err := f.Close(); err != nil {
		return CheckpointDataset{}, fmt.Errorf("close sealed file: %w", err)
	}
	ds := CheckpointDataset{
		Owner: owner, Key: key, Kind: kind, SourceVolume: volume, File: name,
		SealedSHA256: hex.EncodeToString(sealedHash.Sum(nil)), SealedBytes: sealedCount.n,
		PlainSHA256: hex.EncodeToString(plainHash.Sum(nil)), PlainBytes: plainCount.n,
		Fingerprint: fp,
	}
	entries, err := verifyDatasetFile(w.dir, ds, w.key, true)
	if err != nil {
		return CheckpointDataset{}, fmt.Errorf("verify sealed %s: %w", name, err)
	}
	ds.Entries = entries
	return ds, nil
}

// Commit writes checkpoint.json last and publishes the directory.
func (w *CheckpointWriter) Commit(cp Checkpoint) error {
	cp.SchemaVersion = checkpointSchemaVersion
	cp.ID = w.id
	cp.KeyID = KeyID(w.key)
	if err := writeJSONAtomic(filepath.Join(w.dir, checkpointFileName), cp, 0o600); err != nil {
		return err
	}
	if err := syncDir(w.dir); err != nil {
		return err
	}
	final := filepath.Join(w.store.Dir, w.id)
	if err := os.Rename(w.dir, final); err != nil {
		return fmt.Errorf("publish checkpoint: %w", err)
	}
	return syncDir(w.store.Dir)
}

// Abort removes the incomplete directory.
func (w *CheckpointWriter) Abort() { _ = os.RemoveAll(w.dir) }

// verifyDatasetFile checks a sealed file against its metadata. With deep it
// decrypts the whole stream and walks the tar structure; it returns the number
// of tar entries.
func verifyDatasetFile(dir string, ds CheckpointDataset, key []byte, deep bool) (int, error) {
	path := filepath.Join(dir, ds.File)
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if !fi.Mode().IsRegular() {
		return 0, fmt.Errorf("%s is not a regular file", ds.File)
	}
	if fi.Size() != ds.SealedBytes {
		return 0, fmt.Errorf("%s has %d bytes, the checkpoint recorded %d", ds.File, fi.Size(), ds.SealedBytes)
	}
	sealed := sha256.New()
	var src io.Reader = io.TeeReader(f, sealed)
	if !deep {
		if _, err := io.Copy(io.Discard, src); err != nil {
			return 0, err
		}
		if got := hex.EncodeToString(sealed.Sum(nil)); got != ds.SealedSHA256 {
			return 0, fmt.Errorf("%s sha256 %s differs from the recorded %s", ds.File, got, ds.SealedSHA256)
		}
		return 0, nil
	}
	sr, err := NewSealReader(src, key)
	if err != nil {
		return 0, err
	}
	plain := sha256.New()
	counter := &countWriter{}
	tee := io.TeeReader(sr, io.MultiWriter(plain, counter))
	entries, err := countTarEntries(tee)
	if err != nil {
		return 0, err
	}
	if _, err := io.Copy(io.Discard, tee); err != nil { // trailing padding
		return 0, err
	}
	if got := hex.EncodeToString(sealed.Sum(nil)); got != ds.SealedSHA256 {
		return 0, fmt.Errorf("%s sha256 %s differs from the recorded %s", ds.File, got, ds.SealedSHA256)
	}
	if got := hex.EncodeToString(plain.Sum(nil)); got != ds.PlainSHA256 || counter.n != ds.PlainBytes {
		return 0, fmt.Errorf("%s decrypts to %d bytes with sha256 %s, expected %d bytes with %s", ds.File, counter.n, got, ds.PlainBytes, ds.PlainSHA256)
	}
	return entries, nil
}

func countTarEntries(r io.Reader) (int, error) {
	tr := tar.NewReader(r)
	n := 0
	for {
		_, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return n, fmt.Errorf("tar structure: %w", err)
		}
		n++
		if _, err := io.Copy(io.Discard, tr); err != nil {
			return n, fmt.Errorf("tar entry: %w", err)
		}
	}
}

// Verify checks every dataset file of checkpoint id. A shallow check compares
// sizes and the SHA-256 of the sealed files; deep also decrypts them and walks
// the archives, which proves the key and the content.
func (s CheckpointStore) Verify(id string, key []byte, deep bool) (Checkpoint, error) {
	cp, err := s.Load(id)
	if err != nil {
		return Checkpoint{}, err
	}
	dir, _ := s.path(id)
	if deep && KeyID(key) != cp.KeyID {
		return cp, fmt.Errorf("checkpoint %s was sealed with key %s, the configured key is %s", id, cp.KeyID, KeyID(key))
	}
	var errs []error
	if len(cp.Datasets) == 0 {
		errs = append(errs, errors.New("checkpoint has no datasets"))
	}
	for _, ds := range cp.Datasets {
		if _, err := verifyDatasetFile(dir, ds, key, deep); err != nil {
			errs = append(errs, fmt.Errorf("%s/%s: %w", ds.Owner, ds.Key, err))
		}
	}
	return cp, errors.Join(errs...)
}

// OpenDataset opens a verified-on-read decrypting stream of one dataset. The
// caller must read it to EOF (EOF means the final frame authenticated) and
// close the returned closer.
func (s CheckpointStore) OpenDataset(cp Checkpoint, ds CheckpointDataset, key []byte) (io.Reader, io.Closer, error) {
	dir, err := s.path(cp.ID)
	if err != nil {
		return nil, nil, err
	}
	if !datasetFilePattern.MatchString(ds.File) {
		return nil, nil, fmt.Errorf("invalid dataset file name %q", ds.File)
	}
	f, err := os.Open(filepath.Join(dir, ds.File))
	if err != nil {
		return nil, nil, err
	}
	sr, err := NewSealReader(f, key)
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	return sr, f, nil
}

// MarkVerified records the time of a successful deep verification.
func (s CheckpointStore) MarkVerified(id string, at time.Time) error {
	cp, err := s.Load(id)
	if err != nil {
		return err
	}
	t := at.UTC()
	cp.LastVerifiedAt = &t
	dir, _ := s.path(id)
	return writeJSONAtomic(filepath.Join(dir, checkpointFileName), cp, 0o600)
}

// Prune deletes the oldest checkpoints beyond keep newest clean ones and every
// incomplete leftover, but never a protected id (referenced by a journal in
// flight or paired with the running generation). It returns the removed ids.
func (s CheckpointStore) Prune(keep int, protected map[string]bool) ([]string, error) {
	if keep < 1 {
		return nil, errors.New("prune: keep must be at least 1")
	}
	cps, incomplete, err := s.List()
	if err != nil {
		return nil, err
	}
	var removed []string
	kept := 0
	for _, cp := range cps {
		clean := cp.Consistency == ConsistencyClean
		if protected[cp.ID] {
			if clean {
				kept++
			}
			continue
		}
		if clean && kept < keep {
			kept++
			continue
		}
		if err := os.RemoveAll(filepath.Join(s.Dir, cp.ID)); err != nil {
			return removed, err
		}
		removed = append(removed, cp.ID)
	}
	for _, name := range incomplete {
		id := strings.TrimPrefix(name, incompletePrefix)
		if protected[id] || protected[name] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(s.Dir, name)); err != nil {
			return removed, err
		}
		removed = append(removed, name)
	}
	return removed, nil
}
