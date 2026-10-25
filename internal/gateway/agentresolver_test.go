package gateway

import (
	"slices"
	"testing"
)

func resolverConfig(def string) Config {
	return Config{
		Identities: []Identity{
			{ID: "human", Role: "human", WorkspaceID: "alpha"},
			{ID: "agent-a", Role: "agent", WorkspaceID: "alpha"},
			{ID: "agent-b", Role: "agent", WorkspaceID: "alpha"},
			{ID: "admin", Role: "admin", WorkspaceID: "alpha"},
			{ID: "agent-z", Role: "agent", WorkspaceID: "beta"},
		},
		Runtimes: []RuntimeConfig{{WorkspaceID: "alpha", DefaultAgentID: def}, {WorkspaceID: "beta"}},
	}
}

func TestAgentRosterIsScopedByConfiguredRoleAndWorkspace(t *testing.T) {
	c := resolverConfig("agent-b")
	r := agentRoster(c, "alpha")
	if !slices.Equal(r.AgentIDs, []string{"agent-a", "agent-b"}) || r.DefaultAgentID != "agent-b" {
		t.Fatalf("alpha roster = %+v", r)
	}
	if r := agentRoster(c, "beta"); !slices.Equal(r.AgentIDs, []string{"agent-z"}) || r.DefaultAgentID != "" {
		t.Fatalf("beta roster = %+v", r)
	}
	if r := agentRoster(c, "missing"); len(r.AgentIDs) != 0 || r.DefaultAgentID != "" {
		t.Fatalf("unknown workspace roster = %+v", r)
	}
}

func TestDefaultAgentMustBeAnAgentOfItsOwnWorkspace(t *testing.T) {
	for _, ok := range []string{"", "agent-a", "agent-b"} {
		if err := validateAgentDefaults(resolverConfig(ok)); err != nil {
			t.Errorf("default %q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"human", "admin", "agent-z", "nobody"} {
		if err := validateAgentDefaults(resolverConfig(bad)); err == nil {
			t.Errorf("default %q accepted", bad)
		}
	}
}
