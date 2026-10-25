package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
)

// ManifestName is the sha256sum-compatible index written next to the evidence.
const ManifestName = "evidence.sha256"

// Evidence writes test artefacts below one directory and records the sha256 of
// each in ManifestName ("<hex>  <relative name>", checkable with sha256sum -c).
// Files are never overwritten: evidence is immutable within a run.
type Evidence struct {
	dir string
	mu  sync.Mutex
}

// NewEvidence opens the directory named by LENSDOCK_E2E_OUT.
func NewEvidence(t TB) *Evidence {
	t.Helper()
	dir := Require(t, EnvOut)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create evidence directory: %v", err)
	}
	return &Evidence{dir: dir}
}

// NewEvidenceIn is NewEvidence for an explicit directory.
func NewEvidenceIn(dir string) (*Evidence, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create evidence directory: %w", err)
	}
	return &Evidence{dir: dir}, nil
}

// Dir returns the evidence directory.
func (e *Evidence) Dir() string { return e.dir }

// Write stores data under name (a clean relative slash path, no "..") and
// returns its sha256 in hex.
func (e *Evidence) Write(name string, data []byte) (string, error) {
	clean := path.Clean(name)
	if name == "" || clean != name || path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") || clean == "." {
		return "", fmt.Errorf("invalid evidence name %q", name)
	}
	if clean == ManifestName {
		return "", fmt.Errorf("evidence name %q is reserved", name)
	}
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])

	e.mu.Lock()
	defer e.mu.Unlock()
	full := filepath.Join(e.dir, filepath.FromSlash(clean))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "", fmt.Errorf("create evidence directory for %s: %w", name, err)
	}
	f, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "", fmt.Errorf("create evidence %s: %w", name, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("write evidence %s: %w", name, err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("close evidence %s: %w", name, err)
	}
	m, err := os.OpenFile(filepath.Join(e.dir, ManifestName), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return "", fmt.Errorf("open manifest: %w", err)
	}
	if _, err := fmt.Fprintf(m, "%s  %s\n", digest, clean); err != nil {
		_ = m.Close()
		return "", fmt.Errorf("append manifest: %w", err)
	}
	if err := m.Close(); err != nil {
		return "", fmt.Errorf("close manifest: %w", err)
	}
	return digest, nil
}

// WriteJSON stores v as indented JSON.
func (e *Evidence) WriteJSON(name string, v any) (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal evidence %s: %w", name, err)
	}
	return e.Write(name, append(b, '\n'))
}

// Save is Write that fails the test on error and logs the digest.
func (e *Evidence) Save(t TB, name string, data []byte) string {
	t.Helper()
	d, err := e.Write(name, data)
	if err != nil {
		t.Fatalf("%v", err)
	}
	t.Logf("evidence %s sha256=%s (%d bytes)", name, d, len(data))
	return d
}

// SaveJSON is WriteJSON that fails the test on error.
func (e *Evidence) SaveJSON(t TB, name string, v any) string {
	t.Helper()
	d, err := e.WriteJSON(name, v)
	if err != nil {
		t.Fatalf("%v", err)
	}
	t.Logf("evidence %s sha256=%s", name, d)
	return d
}
