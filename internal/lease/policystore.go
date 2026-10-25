package lease

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
	"github.com/JiaWeiXie/lensdock/internal/auth"
)

const (
	// PolicyFileName is the name of the saved policy inside the policy
	// directory.
	PolicyFileName = "agent-control.json"

	policyFileVersion = 1
	maxPolicyFileSize = 4 << 10
)

// ErrStalePolicy is returned by FilePolicyStore.Save when the stored policy
// has a revision at or above the one being saved: another runtime wrote it
// since this one loaded it, and overwriting would undo a newer decision.
var ErrStalePolicy = errors.New("lease: stored agent policy is newer")

// policyFile is the on-disk form of the policy.
type policyFile struct {
	Version   int       `json:"version"`
	Mode      string    `json:"mode"`
	Paused    bool      `json:"paused"`
	Revision  uint64    `json:"revision"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// FilePolicyStore keeps the policy in one JSON file inside Dir. Dir must be a
// directory only the runtime can write (the supervisor prepares it with
// supervisor.PreparePolicyDir); the browser user must not have access to it.
// Writes are atomic: a temporary 0600 file is synced and renamed over the
// policy, then the directory is synced.
type FilePolicyStore struct {
	Dir string
}

func (s FilePolicyStore) path() string { return filepath.Join(s.Dir, PolicyFileName) }

// Load reads the saved policy. A missing file is found=false; a symlink, a
// non-regular file, an oversized, malformed or invalid policy is an error.
func (s FilePolicyStore) Load() (apiwire.AgentControl, bool, error) {
	c, found, err := readPolicyFile(s.path())
	return c, found, err
}

func readPolicyFile(path string) (apiwire.AgentControl, bool, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return apiwire.AgentControl{}, false, nil
	}
	if err != nil {
		return apiwire.AgentControl{}, false, err
	}
	if !fi.Mode().IsRegular() {
		return apiwire.AgentControl{}, false, fmt.Errorf("%s is not a regular file", filepath.Base(path))
	}
	f, err := os.Open(path)
	if err != nil {
		return apiwire.AgentControl{}, false, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxPolicyFileSize+1))
	if err != nil {
		return apiwire.AgentControl{}, false, err
	}
	if len(data) > maxPolicyFileSize {
		return apiwire.AgentControl{}, false, fmt.Errorf("%s is larger than %d bytes", filepath.Base(path), maxPolicyFileSize)
	}
	var pf policyFile
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&pf); err != nil {
		return apiwire.AgentControl{}, false, fmt.Errorf("%s is not a valid policy file", filepath.Base(path))
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return apiwire.AgentControl{}, false, fmt.Errorf("%s has trailing data", filepath.Base(path))
	}
	if pf.Version != policyFileVersion {
		return apiwire.AgentControl{}, false, fmt.Errorf("%s has unsupported version %d", filepath.Base(path), pf.Version)
	}
	c := apiwire.AgentControl{Mode: pf.Mode, Paused: pf.Paused, Revision: pf.Revision, UpdatedAt: pf.UpdatedAt}
	if err := ValidateAgentControl(c); err != nil {
		return apiwire.AgentControl{}, false, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return c, true, nil
}

// Save atomically replaces the policy. It refuses (ErrStalePolicy) to replace
// a stored policy whose revision is not lower than c's, and fails when the
// stored file is unreadable, so a corrupt file is never silently overwritten.
func (s FilePolicyStore) Save(c apiwire.AgentControl) error {
	if err := ValidateAgentControl(c); err != nil {
		return err
	}
	cur, found, err := readPolicyFile(s.path())
	if err != nil {
		return err
	}
	if found && cur.Revision >= c.Revision {
		return ErrStalePolicy
	}
	data, err := json.Marshal(policyFile{
		Version: policyFileVersion, Mode: c.Mode, Paused: c.Paused,
		Revision: c.Revision, UpdatedAt: c.UpdatedAt.UTC(),
	})
	if err != nil {
		return err
	}
	data = append(data, '\n')

	suffix, err := auth.RandomHex(8)
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.Dir, "."+PolicyFileName+".tmp-"+suffix)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path()); err != nil {
		return err
	}
	ok = true
	d, err := os.Open(s.Dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
