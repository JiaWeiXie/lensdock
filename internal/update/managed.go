package update

import (
	"errors"
	"path/filepath"
)

// ManagedOwner identifies the host authority that must admit lifecycle changes.
// Ownership is retained even when its manager cannot currently be reached.
type ManagedOwner struct {
	ManagerID   string `json:"managerId"`
	ProjectID   string `json:"projectId"`
	ManagerRoot string `json:"managerRoot"`
}

func (m ManagedOwner) validate() error {
	if !namePattern.MatchString(m.ManagerID) || !namePattern.MatchString(m.ProjectID) || !filepath.IsAbs(m.ManagerRoot) || filepath.Clean(m.ManagerRoot) != m.ManagerRoot {
		return errors.New("managedBy requires valid manager/project IDs and a canonical absolute managerRoot")
	}
	return nil
}
