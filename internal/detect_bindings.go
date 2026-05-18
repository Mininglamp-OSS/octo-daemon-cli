package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"
)

// openclawBindingJSON mirrors one element of `openclaw agents bindings --json`.
// Example:
//   {"agentId":"main","match":{"channel":"dmwork","accountId":"27gshU6HOTb87d88a0f_bot"},"description":"..."}
type openclawBindingJSON struct {
	AgentID string `json:"agentId"`
	Match   struct {
		Channel   string `json:"channel"`
		AccountID string `json:"accountId"`
	} `json:"match"`
	Description string `json:"description"`
}

// parseOpenclawBindingsJSON parses `openclaw agents bindings --json` output and
// groups bindings by agentId. Each route is formatted as "channel/accountId"
// — stable, machine-parseable, preserves channel context for multi-channel setups.
// Bindings with empty agentId or accountId are skipped.
func parseOpenclawBindingsJSON(data []byte) (map[string][]string, error) {
	arr := extractJSONArray(data)
	if arr == nil {
		return nil, fmt.Errorf("no JSON array found in bindings output")
	}
	var raw []openclawBindingJSON
	if err := json.Unmarshal(arr, &raw); err != nil {
		return nil, fmt.Errorf("unmarshal bindings list: %w", err)
	}
	out := make(map[string][]string)
	for _, b := range raw {
		if b.AgentID == "" || b.Match.AccountID == "" {
			continue
		}
		channel := b.Match.Channel
		if channel == "" {
			channel = "unknown"
		}
		route := channel + "/" + b.Match.AccountID
		out[b.AgentID] = append(out[b.AgentID], route)
	}
	return out, nil
}

// DetectOpenclawBindings runs `openclaw agents bindings --json` and returns
// a map of agentId → []route. Empty map on error (caller treats as no bindings).
func DetectOpenclawBindings(binPath string) map[string][]string {
	if binPath == "" {
		p, err := exec.LookPath("openclaw")
		if err != nil {
			return nil
		}
		binPath = p
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binPath, "agents", "bindings", "--json")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	bindings, err := parseOpenclawBindingsJSON(out)
	if err != nil {
		return nil
	}
	return bindings
}

// mergeBindingsIntoAgents populates each agent's Routes using the bindings map.
// No-op for agents not present in the map.
func mergeBindingsIntoAgents(agents []AgentEntry, bindings map[string][]string) {
	for i := range agents {
		if routes, ok := bindings[agents[i].ID]; ok {
			agents[i].Routes = routes
		}
	}
}
