package handlers

import (
	"testing"

	"github.com/memohai/memoh/internal/acpprofile"
	"github.com/memohai/memoh/internal/bots"
)

func TestScrubBotForResponseMasksACPManagedSecrets(t *testing.T) {
	original := bots.Bot{
		ID: "bot-1",
		Metadata: map[string]any{
			acpprofile.MetadataKeyACP: map[string]any{
				"agents": map[string]any{
					acpprofile.AgentCodexID: map[string]any{
						"enabled": true,
						"managed": map[string]any{
							"api_key":  "sk-original-secret",
							"base_url": "https://api.example.test/v1",
						},
					},
				},
			},
		},
	}

	resp := scrubBotForResponse(original)
	setup := acpprofile.ParseAgentSetup(resp.Metadata, acpprofile.AgentCodexID)
	if got := setup.Managed["api_key"]; got == "" || got == "sk-original-secret" {
		t.Fatalf("scrubbed api_key = %q, want masked non-empty value", got)
	}
	if got := setup.Managed["base_url"]; got != "https://api.example.test/v1" {
		t.Fatalf("base_url = %q, want non-sensitive value preserved", got)
	}

	originalSetup := acpprofile.ParseAgentSetup(original.Metadata, acpprofile.AgentCodexID)
	if got := originalSetup.Managed["api_key"]; got != "sk-original-secret" {
		t.Fatalf("original api_key = %q, want original metadata left untouched", got)
	}
}

func TestScrubBotsForResponseScrubsEachItem(t *testing.T) {
	items := []bots.Bot{
		{
			ID: "bot-1",
			Metadata: map[string]any{
				acpprofile.MetadataKeyACP: map[string]any{
					"agents": map[string]any{
						acpprofile.AgentCodexID: map[string]any{
							"managed": map[string]any{"api_key": "sk-one-secret"},
						},
					},
				},
			},
		},
		{
			ID: "bot-2",
			Metadata: map[string]any{
				acpprofile.MetadataKeyACP: map[string]any{
					"agents": map[string]any{
						acpprofile.AgentCodexID: map[string]any{
							"managed": map[string]any{"api_key": "sk-two-secret"},
						},
					},
				},
			},
		},
	}

	resp := scrubBotsForResponse(items)
	if len(resp) != len(items) {
		t.Fatalf("response len = %d, want %d", len(resp), len(items))
	}
	for _, item := range resp {
		setup := acpprofile.ParseAgentSetup(item.Metadata, acpprofile.AgentCodexID)
		if got := setup.Managed["api_key"]; got == "" || got == "sk-one-secret" || got == "sk-two-secret" {
			t.Fatalf("bot %s api_key = %q, want masked", item.ID, got)
		}
	}
}
