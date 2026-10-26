package harness

import (
	"encoding/json"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
)

// ReadyDetail is the operator view of GET /readyz?detail=1 (see
// internal/runtimeapi and internal/supervisor.Detail). Only the fields the
// tests read are declared.
type ReadyDetail struct {
	Ready     bool              `json:"ready"`
	Workspace apiwire.Workspace `json:"workspace"`
	Locked    bool              `json:"locked"`
	LockCode  string            `json:"lockCode"`
	Lease     *apiwire.Lease    `json:"lease"`
	Detail    struct {
		Units []struct {
			Name     string `json:"name"`
			PID      int    `json:"pid"`
			State    string `json:"state"`
			Restarts int    `json:"restarts"`
			CPUTicks uint64 `json:"cpuTicks"`
			RSSBytes uint64 `json:"rssBytes"`
		} `json:"units"`
		ChromiumVersion string `json:"chromiumVersion"`
		Sandbox         struct {
			Enabled   bool              `json:"enabled"`
			Verified  bool              `json:"verified"`
			Processes []json.RawMessage `json:"processes"`
		} `json:"sandbox"`
		FcMatch  map[string]string `json:"fcMatch"`
		Packages map[string]string `json:"packages"`
	} `json:"detail"`
}
