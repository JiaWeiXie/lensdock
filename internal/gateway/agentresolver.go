package gateway

import (
	"fmt"

	"github.com/JiaWeiXie/lensdock/internal/feedback"
)

// agentRoster lists the configured agents of one workspace: identities whose
// configured role is agent and whose configured workspace is workspaceID. The
// administrative default is the runtime's defaultAgentId. Nothing is derived
// from requests, sessions or stored feedback.
func agentRoster(c Config, workspaceID string) feedback.AgentRoster {
	var r feedback.AgentRoster
	for _, p := range c.Identities {
		if p.Role == "agent" && p.WorkspaceID == workspaceID {
			r.AgentIDs = append(r.AgentIDs, p.ID)
		}
	}
	for _, rc := range c.Runtimes {
		if rc.WorkspaceID == workspaceID {
			r.DefaultAgentID = rc.DefaultAgentID
		}
	}
	return r
}

// validateAgentDefaults refuses a Gateway whose defaultAgentId does not name a
// configured agent of its own workspace, so a misconfiguration surfaces at
// startup instead of at the user's first Send.
func validateAgentDefaults(c Config) error {
	for _, rc := range c.Runtimes {
		if rc.DefaultAgentID == "" {
			continue
		}
		ok := false
		for _, id := range agentRoster(c, rc.WorkspaceID).AgentIDs {
			ok = ok || id == rc.DefaultAgentID
		}
		if !ok {
			return fmt.Errorf("gateway: defaultAgentId of workspace %q is not a configured agent identity of that workspace", rc.WorkspaceID)
		}
	}
	return nil
}
