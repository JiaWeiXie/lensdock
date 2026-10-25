package gateway

import (
	"context"
	"encoding/json"
)

func (g *Gateway) workspaceSnapshot(ctx context.Context, ws string) map[string]any {
	offline := map[string]any{"id": ws, "ready": false, "controlState": "UNKNOWN", "runtimeError": "UPSTREAM_UNAVAILABLE"}
	e := g.runtimes[ws]
	if e == nil {
		return offline
	}
	status, _, b, err := e.call(ctx, "GET", "/api/v1/workspaces/"+ws+"/state", e.operator, "", nil, nil)
	if err != nil || status != 200 {
		return offline
	}
	var result map[string]any
	if json.Unmarshal(b, &result) != nil {
		return offline
	}
	workspace, ok := result["workspace"].(map[string]any)
	if !ok {
		return offline
	}
	delete(result, "workspace")
	for key, value := range workspace {
		result[key] = value
	}
	// A state without a valid usage policy is unknown, never assumed AGENT.
	raw, _ := json.Marshal(result["agentControl"])
	if _, ok := decodeControl(raw); !ok {
		return offline
	}
	return result
}
