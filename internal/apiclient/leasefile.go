package apiclient

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
	"github.com/JiaWeiXie/lensdock/internal/auth"
)

// LeaseFileVersion is the only supported lease file format version.
const LeaseFileVersion = 1

const maxLeaseFileSize = 64 << 10

// LeaseFile is the on-disk handoff of an acquired lease. It contains the
// capability token, so it is only written and read as a private file.
type LeaseFile struct {
	Version         int           `json:"version"`
	APIBaseURL      string        `json:"apiBaseUrl"`
	WorkspaceID     string        `json:"workspaceId"`
	Lease           apiwire.Lease `json:"lease"`
	CapabilityToken auth.Secret   `json:"capabilityToken"`
}

// WriteLeaseFile creates path with mode 0600. It refuses to overwrite an
// existing file or follow a symlink.
func WriteLeaseFile(path string, f LeaseFile) error {
	if f.Version == 0 {
		f.Version = LeaseFileVersion
	}
	if f.Version != LeaseFileVersion {
		return fmt.Errorf("apiclient: unsupported lease file version %d", f.Version)
	}
	if f.CapabilityToken == "" {
		return errors.New("apiclient: lease file requires a capability token")
	}
	data, err := json.Marshal(f)
	if err != nil {
		return errors.New("apiclient: encode lease file")
	}
	data = append(data, '\n')
	return auth.WritePrivateFile(path, data)
}

// ReadLeaseFile reads a lease file after checking that it is a private
// regular file. Unknown versions, unknown fields and a missing token are
// rejected.
func ReadLeaseFile(path string) (LeaseFile, error) {
	data, err := auth.ReadPrivateFile(path, maxLeaseFileSize)
	if err != nil {
		return LeaseFile{}, err
	}
	var f LeaseFile
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return LeaseFile{}, errors.New("apiclient: malformed lease file")
	}
	if f.Version != LeaseFileVersion {
		return LeaseFile{}, fmt.Errorf("apiclient: unsupported lease file version %d", f.Version)
	}
	if f.CapabilityToken == "" || f.WorkspaceID == "" || f.Lease.LeaseID == "" || f.APIBaseURL == "" {
		return LeaseFile{}, errors.New("apiclient: incomplete lease file")
	}
	return f, nil
}
