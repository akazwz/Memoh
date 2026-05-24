package command

import (
	"context"
	"fmt"
	"strings"
)

// CommandManifest is the UI-facing description of a slash command. It is
// intentionally separate from the executable slash-command registry because
// some chat commands are routed by the conversation flow instead of the
// channel command handler.
type CommandManifest struct {
	ID          string   `json:"id"`
	Command     string   `json:"command"`
	InsertText  string   `json:"insert_text"`
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	Source      string   `json:"source"`
	PluginID    string   `json:"plugin_id,omitempty"`
	PluginName  string   `json:"plugin_name,omitempty"`
	Capability  string   `json:"capability"`
	Action      string   `json:"action"`
	Icon        string   `json:"icon,omitempty"`
	Enabled     bool     `json:"enabled"`
	Scopes      []string `json:"scopes,omitempty"`
}

// ManifestRequest carries context that providers may use to decide whether a
// command should be visible or enabled.
type ManifestRequest struct {
	BotID       string
	SessionID   string
	Scope       string
	BotMetadata map[string]any
}

// ManifestProvider contributes UI-facing slash commands.
type ManifestProvider interface {
	Commands(ctx context.Context, req ManifestRequest) ([]CommandManifest, error)
}

// ManifestRegistry collects command manifests from registered providers.
type ManifestRegistry struct {
	providers []ManifestProvider
}

func NewManifestRegistry(providers ...ManifestProvider) *ManifestRegistry {
	out := make([]ManifestProvider, 0, len(providers))
	for _, provider := range providers {
		if provider != nil {
			out = append(out, provider)
		}
	}
	return &ManifestRegistry{providers: out}
}

func (r *ManifestRegistry) Commands(ctx context.Context, req ManifestRequest) ([]CommandManifest, error) {
	if r == nil {
		return nil, nil
	}
	req.Scope = strings.TrimSpace(req.Scope)
	var out []CommandManifest
	for _, provider := range r.providers {
		commands, err := provider.Commands(ctx, req)
		if err != nil {
			return nil, err
		}
		for _, cmd := range commands {
			if commandVisibleInScope(cmd, req.Scope) {
				out = append(out, normalizeManifest(cmd))
			}
		}
	}
	return out, nil
}

func normalizeManifest(cmd CommandManifest) CommandManifest {
	cmd.ID = strings.TrimSpace(cmd.ID)
	cmd.Command = strings.TrimSpace(cmd.Command)
	cmd.InsertText = strings.TrimRight(cmd.InsertText, "\r\n")
	cmd.Title = strings.TrimSpace(cmd.Title)
	cmd.Description = strings.TrimSpace(cmd.Description)
	cmd.Source = strings.TrimSpace(cmd.Source)
	cmd.PluginID = strings.TrimSpace(cmd.PluginID)
	cmd.PluginName = strings.TrimSpace(cmd.PluginName)
	cmd.Capability = strings.TrimSpace(cmd.Capability)
	cmd.Action = strings.TrimSpace(cmd.Action)
	cmd.Icon = strings.TrimSpace(cmd.Icon)
	return cmd
}

func commandVisibleInScope(cmd CommandManifest, scope string) bool {
	if scope == "" || len(cmd.Scopes) == 0 {
		return true
	}
	for _, candidate := range cmd.Scopes {
		if strings.TrimSpace(candidate) == scope {
			return true
		}
	}
	return false
}

func commandManifestID(pluginID, action string) string {
	return fmt.Sprintf("%s.%s", strings.TrimSpace(pluginID), strings.TrimSpace(action))
}
