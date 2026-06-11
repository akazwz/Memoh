package acpclient

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestClaudeManagedSettingsAsksBashAndHasNoDenyRule locks the managed-settings
// shape after the review. The ask:[Bash] containment pin must stay (it forces
// every Bash command through Memoh approval), and there must be NO fs deny
// rule: the old Read/Edit/Write("/**") rules were inert (claude-agent-acp routes
// fs through the client capability, which ignores settings deny) AND
// mis-anchored to the project root (single-slash anchors to cwd, not the config
// dir), so they protected nothing and would have bricked the workspace if ever
// honored. This guards against a broken deny rule creeping back in.
func TestClaudeManagedSettingsAsksBashAndHasNoDenyRule(t *testing.T) {
	var parsed struct {
		Permissions struct {
			Ask  []string `json:"ask"`
			Deny []string `json:"deny"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(claudeManagedSettings, &parsed); err != nil {
		t.Fatalf("claudeManagedSettings is not valid JSON: %v", err)
	}

	hasBashAsk := false
	for _, a := range parsed.Permissions.Ask {
		if a == "Bash" {
			hasBashAsk = true
		}
	}
	if !hasBashAsk {
		t.Fatalf("managed settings must keep ask:[Bash]; got ask=%v", parsed.Permissions.Ask)
	}
	if len(parsed.Permissions.Deny) != 0 {
		t.Fatalf("managed settings must not carry an fs deny rule (inert + mis-anchored footgun); got deny=%v", parsed.Permissions.Deny)
	}
	// Catch the specific project-root-anchored pattern even if it reappears
	// under a different key.
	if strings.Contains(string(claudeManagedSettings), `(/**)`) {
		t.Fatalf("managed settings reintroduced a project-root-anchored deny pattern:\n%s", claudeManagedSettings)
	}
}
