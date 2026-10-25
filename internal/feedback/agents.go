package feedback

import (
	"net/http"
	"slices"
)

// AgentRoster is the administrator-configured set of agents of one workspace.
// AgentIDs are identities whose configured role is agent and whose configured
// workspace is the one asked for. DefaultAgentID is the optional administrative
// choice among them.
type AgentRoster struct {
	AgentIDs       []string
	DefaultAgentID string
}

var (
	errNoAgent        = newError(http.StatusConflict, CodeAgentNotConfigured, "No agent is configured for this workspace. Ask an administrator to add an agent identity to the Gateway configuration. Nothing was changed.")
	errBadDefault     = newError(http.StatusConflict, CodeAgentNotConfigured, "The default agent of this workspace is not a configured agent of it. Ask an administrator to correct defaultAgentId in the Gateway configuration. Nothing was changed.")
	errAmbiguousAgent = newError(http.StatusConflict, CodeAgentAmbiguous, "Several agents are configured for this workspace and none is the default. Ask an administrator to set defaultAgentId in the Gateway configuration. Nothing was changed.")
)

func (s *Service) roster(workspaceID string) AgentRoster {
	if s.opt.Agents == nil {
		return AgentRoster{}
	}
	return s.opt.Agents(workspaceID)
}

// resolveAgent returns the agent a revision of workspaceID is queued for.
// An explicit requested agent must be a configured agent of the workspace.
// Without one the server picks the administrative default, or the only
// configured agent. It never guesses among several and never leaves the
// workspace.
func (s *Service) resolveAgent(workspaceID, requested string) (string, error) {
	r := s.roster(workspaceID)
	if requested != "" {
		if !validAgentID(requested) {
			return "", badRequest("agentId must be 1-128 printable ASCII characters")
		}
		if !slices.Contains(r.AgentIDs, requested) {
			return "", badRequest("agentId is not a configured agent of this workspace")
		}
		return requested, nil
	}
	if r.DefaultAgentID != "" {
		if !slices.Contains(r.AgentIDs, r.DefaultAgentID) {
			return "", errBadDefault
		}
		return r.DefaultAgentID, nil
	}
	switch len(r.AgentIDs) {
	case 0:
		return "", errNoAgent
	case 1:
		return r.AgentIDs[0], nil
	}
	return "", errAmbiguousAgent
}
