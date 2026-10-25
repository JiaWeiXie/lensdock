package apiwire

import (
	"fmt"
	"strings"
	"time"
)

// Agent usage modes. The values are the wire values of AgentControl.Mode.
const (
	// AgentModeNOVNC is the browser-only mode: every Agent feature is
	// disabled and no AGENT lease can be granted.
	AgentModeNOVNC = "NOVNC"
	// AgentModeAGENT enables Agent control; it may still be paused.
	AgentModeAGENT = "AGENT"
)

// Error codes of the usage-mode policy.
const (
	// CodeAgentModeDisabled means the workspace is in NOVNC mode, so no
	// AGENT lease or Agent operation is available until a human enables
	// AGENT mode.
	CodeAgentModeDisabled = "AGENT_MODE_DISABLED"
	// CodeAgentPaused means a human paused the Agent: no AGENT lease can be
	// granted, renewed or used until a human resumes it.
	CodeAgentPaused = "AGENT_PAUSED"
)

// AgentControl is the authoritative usage policy of one workspace. It is
// separate from the controlState (IDLE/HUMAN/AGENT/...). Paused is always
// false in NOVNC mode. Revision increases by one for every change of Mode
// or Paused (starting at 1 for a never-changed workspace) and UpdatedAt is
// the time of that change.
type AgentControl struct {
	Mode      string    `json:"mode"`
	Paused    bool      `json:"paused"`
	Revision  uint64    `json:"revision"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// SetAgentMode is the body of PUT /api/v1/workspaces/{id}/agent.
type SetAgentMode struct {
	Mode string `json:"mode"`
}

// ValidAgentMode reports whether mode is a known usage mode.
func ValidAgentMode(mode string) bool {
	return mode == AgentModeNOVNC || mode == AgentModeAGENT
}

// ParseAgentMode normalizes an operator-supplied startup mode (flag or
// environment), which is case-insensitive. The wire accepts only the exact
// upper-case values; see ValidAgentMode.
func ParseAgentMode(s string) (string, error) {
	switch m := strings.ToUpper(strings.TrimSpace(s)); m {
	case AgentModeNOVNC, AgentModeAGENT:
		return m, nil
	}
	return "", fmt.Errorf("mode must be %s or %s", AgentModeNOVNC, AgentModeAGENT)
}
