package command

import (
	"context"
	"fmt"
	"sort"
	"strings"

	acpagent "github.com/memohai/memoh/internal/acpagent"
)

var acpAgentCommandScopes = []string{"web", "desktop", "local_chat"}

// ACPAgentManifestProvider exposes ACP-backed agent profiles as slash command
// entries. Today this only yields Codex commands because the service only
// registers the built-in codex profile.
type ACPAgentManifestProvider struct {
	service *acpagent.Service
}

func NewACPAgentManifestProvider(service *acpagent.Service) *ACPAgentManifestProvider {
	return &ACPAgentManifestProvider{service: service}
}

func (p *ACPAgentManifestProvider) Commands(context.Context, ManifestRequest) ([]CommandManifest, error) {
	if p == nil || p.service == nil {
		return nil, nil
	}
	profiles := p.service.Profiles()
	sort.SliceStable(profiles, func(i, j int) bool {
		return strings.TrimSpace(profiles[i].ID) < strings.TrimSpace(profiles[j].ID)
	})

	commands := make([]CommandManifest, 0, len(profiles)*2)
	for _, profile := range profiles {
		pluginID := strings.TrimSpace(profile.ID)
		if pluginID == "" {
			continue
		}
		pluginName := strings.TrimSpace(profile.DisplayName)
		if pluginName == "" {
			pluginName = pluginID
		}
		commandPrefix := "/" + pluginID
		commands = append(commands,
			CommandManifest{
				ID:          commandManifestID(pluginID, "start"),
				Command:     commandPrefix + " start",
				InsertText:  commandPrefix + " start ",
				Title:       fmt.Sprintf("Start %s", pluginName),
				Description: fmt.Sprintf("启动 %s ACP 子会话，可在后面直接写任务", pluginName),
				Source:      "plugin",
				PluginID:    pluginID,
				PluginName:  pluginName,
				Capability:  "acp_agent",
				Action:      "start",
				Icon:        "code",
				Enabled:     true,
				Scopes:      append([]string(nil), acpAgentCommandScopes...),
			},
			CommandManifest{
				ID:          commandManifestID(pluginID, "stop"),
				Command:     commandPrefix + " stop",
				InsertText:  commandPrefix + " stop",
				Title:       fmt.Sprintf("Stop %s", pluginName),
				Description: fmt.Sprintf("停止当前 %s ACP 子会话，后续消息交还给 Memoh", pluginName),
				Source:      "plugin",
				PluginID:    pluginID,
				PluginName:  pluginName,
				Capability:  "acp_agent",
				Action:      "stop",
				Icon:        "code",
				Enabled:     true,
				Scopes:      append([]string(nil), acpAgentCommandScopes...),
			},
		)
	}
	return commands, nil
}
