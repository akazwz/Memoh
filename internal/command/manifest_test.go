package command

import (
	"context"
	"testing"

	acpagent "github.com/memohai/memoh/internal/acpagent"
)

type staticManifestProvider []CommandManifest

func (p staticManifestProvider) Commands(context.Context, ManifestRequest) ([]CommandManifest, error) {
	return append([]CommandManifest(nil), p...), nil
}

func TestACPAgentManifestProviderCodexCommands(t *testing.T) {
	service := acpagent.NewService(nil, nil)
	provider := NewACPAgentManifestProvider(service)

	commands, err := provider.Commands(context.Background(), ManifestRequest{
		Scope: "web",
		BotMetadata: map[string]any{
			acpagent.MetadataKeyACP: map[string]any{
				"agents": map[string]any{
					acpagent.CodexAgentID: map[string]any{"enabled": true},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Commands() error = %v", err)
	}
	if len(commands) != 2 {
		t.Fatalf("Commands() len = %d, want 2", len(commands))
	}

	start := commands[0]
	if start.ID != "codex.start" || start.Command != "/codex start" || start.InsertText != "/codex start " {
		t.Fatalf("start command = %+v", start)
	}
	if start.PluginID != "codex" || start.PluginName != "Codex" || start.Capability != "acp_agent" || !start.Enabled {
		t.Fatalf("start metadata = %+v", start)
	}

	stop := commands[1]
	if stop.ID != "codex.stop" || stop.Command != "/codex stop" || stop.InsertText != "/codex stop" {
		t.Fatalf("stop command = %+v", stop)
	}
}

func TestACPAgentManifestProviderRequiresEnabledAgent(t *testing.T) {
	service := acpagent.NewService(nil, nil)
	provider := NewACPAgentManifestProvider(service)

	commands, err := provider.Commands(context.Background(), ManifestRequest{Scope: "web"})
	if err != nil {
		t.Fatalf("Commands() error = %v", err)
	}
	if len(commands) != 0 {
		t.Fatalf("Commands() len = %d, want 0: %+v", len(commands), commands)
	}
}

func TestManifestRegistryFiltersScope(t *testing.T) {
	registry := NewManifestRegistry(staticManifestProvider{
		{ID: "web", Command: "/web", Scopes: []string{"web"}},
		{ID: "im", Command: "/im", Scopes: []string{"im"}},
		{ID: "all", Command: "/all"},
	})

	commands, err := registry.Commands(context.Background(), ManifestRequest{Scope: "web"})
	if err != nil {
		t.Fatalf("Commands() error = %v", err)
	}
	if len(commands) != 2 {
		t.Fatalf("Commands() len = %d, want 2: %+v", len(commands), commands)
	}
	if commands[0].ID != "web" || commands[1].ID != "all" {
		t.Fatalf("Commands() = %+v", commands)
	}
}
