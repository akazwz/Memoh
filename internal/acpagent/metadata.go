package acpagent

import (
	"encoding/json"
	"strings"
)

const MetadataKeyACP = "acp"

func MetadataAgentEnabled(metadata map[string]any, agentID string) bool {
	agentID = normalizeProfileID(agentID)
	if agentID == "" {
		agentID = CodexAgentID
	}
	acpConfig, ok := metadataRecord(metadata[MetadataKeyACP])
	if !ok {
		return false
	}

	if agents, ok := metadataRecord(acpConfig["agents"]); ok {
		if enabled, ok := metadataBool(agents[agentID]); ok {
			return enabled
		}
		if agentConfig, ok := metadataRecord(agents[agentID]); ok {
			if enabled, ok := metadataBool(agentConfig["enabled"]); ok {
				return enabled
			}
		}
	}

	for _, enabledAgentID := range metadataStringSlice(acpConfig["enabled_agents"]) {
		if normalizeProfileID(enabledAgentID) == agentID {
			return true
		}
	}

	if agentID == CodexAgentID {
		if enabled, ok := metadataBool(acpConfig["codex_enabled"]); ok {
			return enabled
		}
	}
	return false
}

func MetadataAgentEnabledRaw(raw []byte, agentID string) bool {
	if len(raw) == 0 {
		return false
	}
	var metadata map[string]any
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return false
	}
	return MetadataAgentEnabled(metadata, agentID)
}

func metadataRecord(value any) (map[string]any, bool) {
	switch v := value.(type) {
	case map[string]any:
		return v, true
	default:
		return nil, false
	}
}

func metadataBool(value any) (bool, bool) {
	switch v := value.(type) {
	case bool:
		return v, true
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true", "1", "yes", "on", "enabled":
			return true, true
		case "false", "0", "no", "off", "disabled":
			return false, true
		default:
			return false, false
		}
	default:
		return false, false
	}
}

func metadataStringSlice(value any) []string {
	switch v := value.(type) {
	case []string:
		return append([]string(nil), v...)
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}
