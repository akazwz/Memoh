package acpprofile

import (
	"encoding/json"
	"sort"
	"strings"
)

const (
	AgentCodexID        = "codex"
	AgentCodexName      = "Codex"
	AgentClaudeCodeID   = "claude-code"
	AgentClaudeCodeName = "Claude Code"

	MetadataKeyACP = "acp"

	setupModeAPIKey = "api_key"
	setupModeOAuth  = "oauth"
	setupModeSelf   = "self"

	// SetupModeAPIKey/OAuth name the managed setup modes for
	// RequiredManagedFields declarations.
	SetupModeAPIKey = setupModeAPIKey
	SetupModeOAuth  = setupModeOAuth

	// ContainerHomePersistent keeps the managed container HOME on the
	// workspace data mount: the agent stores config and session state under
	// $HOME and it should survive runtime recycles (Codex). The empty default
	// gives each managed session a disposable HOME for credential isolation
	// (Claude Code).
	ContainerHomePersistent = "persistent"

	// ManagedSettingsClaude marks agents that need the Claude-style managed
	// settings file written into their config dir (the explicit ask/deny
	// rules that make Memoh policy the single authority — see
	// acpclient.WriteClaudeManagedSettings).
	ManagedSettingsClaude = "claude"

	// ManagedEnvClaude marks agents whose managed credentials are injected
	// via the Anthropic/Claude env contract (acpagent.managedProcessEnv).
	ManagedEnvClaude = "claude"

	// ManagedConfigCodex marks agents whose managed credentials are written
	// as CODEX_HOME config files via the workspace bridge
	// (acpclient.WriteCodexManagedConfig*).
	ManagedConfigCodex = "codex"
)

type Profile struct {
	ID           string
	DisplayName  string
	Description  string
	Command      string
	Args         []string
	LocalCommand string
	LocalArgs    []string
	// SessionModeID, when set, is the ACP session mode Memoh pins right after
	// session/new so tool permissions flow through ACP regardless of ambient
	// agent-side configuration (e.g. a host ~/.claude/settings.json).
	SessionModeID string
	// SessionConfigValues are ACP session config options pinned after
	// session/new when the agent advertises them (e.g. Claude Code's "effort"
	// select, which gates extended thinking on newer models). Options the
	// agent does not expose are skipped.
	SessionConfigValues map[string]string
	ManagedFields       []ManagedField
	SupportedBackends   []string
	SetupModes          []string

	// Containment declarations (architecture doc §5.6). Per-agent managed-mode
	// behavior is data here, so onboarding an agent with known shapes is
	// declaration-only: the owning packages switch on these capabilities,
	// never on agent IDs.

	// ContainerHomeStrategy selects the managed container HOME:
	// ContainerHomePersistent or "" (disposable per-session HOME, default).
	ContainerHomeStrategy string
	// LocalIsolationEnv/LocalIsolationDir: for local (desktop) BYOK, the env
	// var pointed at a bot-scoped config dir under the workspace root so
	// managed credentials never touch the host's real config (CODEX_HOME →
	// .codex, CLAUDE_CONFIG_DIR → .memoh/claude). Empty = no isolation env.
	LocalIsolationEnv string
	LocalIsolationDir string
	// ManagedSettings selects the managed settings writer (ManagedSettingsClaude
	// or empty).
	ManagedSettings string
	// ManagedEnv selects the managed credential env contract (ManagedEnvClaude
	// or empty).
	ManagedEnv string
	// ManagedThinkingTokens, when > 0, pins the agent's thinking budget in the
	// managed env (Claude's MAX_THINKING_TOKENS — the older-model thinking
	// control; newer models gate on effort/SessionConfigValues instead). 0
	// leaves the agent's own default. Declared here so per-agent tuning never
	// becomes a code conditional.
	ManagedThinkingTokens int
	// ManagedConfig selects the managed config-file reconciler
	// (ManagedConfigCodex or empty).
	ManagedConfig string
	// RequiredManagedFields lists, per managed setup mode, the managed field
	// IDs that must be non-empty before a session may start. Modes absent
	// from the map fall back to the ManagedFields Required flags.
	RequiredManagedFields map[string][]string
}

type ManagedField struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Type        string `json:"type"`
	Required    bool   `json:"required,omitempty"`
	Sensitive   bool   `json:"sensitive,omitempty"`
	Placeholder string `json:"placeholder,omitempty"`
	Help        string `json:"help,omitempty"`
}

type PublicProfile struct {
	ID                string         `json:"id"`
	DisplayName       string         `json:"display_name"`
	Description       string         `json:"description,omitempty"`
	ManagedFields     []ManagedField `json:"managed_fields,omitempty"`
	SupportedBackends []string       `json:"supported_backends,omitempty"`
	SetupModes        []string       `json:"setup_modes,omitempty"`
}

type ProfilesResponse struct {
	Items []PublicProfile `json:"items"`
}

type AgentSetup struct {
	AgentID string
	Enabled bool
	Mode    string
	// ModeSet is true only when setup_mode was explicitly present in the bot
	// metadata. When false the Mode field carries the package default (api_key)
	// and callers that need to distinguish "explicitly api_key" from "legacy /
	// unset" should check this flag rather than comparing Mode directly.
	ModeSet bool
	Managed map[string]string
}

// registry holds all known ACP agent profiles keyed by NormalizeAgentID.
// It is initialised via init() in this package; downstream code should only
// access it via Lookup / List / Register so we keep the registration logic
// in a single place.
var registry = map[string]Profile{}

func init() {
	Register(codexProfile())
	Register(claudeCodeProfile())
}

// Register adds (or replaces) a profile in the registry. Intended to be
// called from package init() blocks. Profiles with an empty ID are ignored.
func Register(profile Profile) {
	id := NormalizeAgentID(profile.ID)
	if id == "" {
		return
	}
	profile.ID = id
	registry[id] = profile
}

func codexProfile() Profile {
	return Profile{
		ID:           AgentCodexID,
		DisplayName:  AgentCodexName,
		Description:  "OpenAI Codex ACP adapter",
		Command:      "codex-acp",
		LocalCommand: "npx",
		LocalArgs: []string{
			"-y",
			"@zed-industries/codex-acp@0.15.0",
		},
		ManagedFields: []ManagedField{
			{
				ID:          "api_key",
				Label:       "OpenAI API key",
				Type:        "password",
				Sensitive:   true,
				Placeholder: "sk-...",
				Help:        "Used by API key setup to authenticate Codex.",
			},
			{
				ID:          "base_url",
				Label:       "OpenAI base URL",
				Type:        "url",
				Placeholder: "https://api.openai.com/v1",
				Help:        "Optional Codex provider base URL.",
			},
		},
		SupportedBackends: []string{"local", "container"},
		SetupModes:        []string{setupModeAPIKey, setupModeOAuth, setupModeSelf},
		// Codex stores config and session state under $HOME; keep it on the
		// persistent mount. Managed credentials are CODEX_HOME config files,
		// isolated under the workspace root on local backends.
		ContainerHomeStrategy: ContainerHomePersistent,
		LocalIsolationEnv:     "CODEX_HOME",
		LocalIsolationDir:     ".codex",
		ManagedConfig:         ManagedConfigCodex,
		RequiredManagedFields: map[string][]string{
			SetupModeAPIKey: {"api_key"},
			SetupModeOAuth:  {},
		},
	}
}

func claudeCodeProfile() Profile {
	return Profile{
		ID:          AgentClaudeCodeID,
		DisplayName: AgentClaudeCodeName,
		Description: "Claude Code ACP adapter",
		Command:     "claude-agent-acp",
		// "default" routes every gated tool through session/request_permission;
		// without the pin a host-level Claude settings file (defaultMode auto /
		// acceptEdits) silently bypasses Memoh's approval flow.
		SessionModeID: "default",
		// Newer Claude models gate extended thinking on the effort level, not
		// MAX_THINKING_TOKENS; "high" is the counterpart of Codex's xhigh
		// reasoning config. Models without effort support skip this pin.
		SessionConfigValues: map[string]string{"effort": "high"},
		LocalCommand:        "npx",
		LocalArgs: []string{
			"-y",
			// 0.40+ fixes two thinking bugs: MAX_THINKING_TOKENS now maps to a
			// real thinking config (the old maxThinkingTokens option is
			// deprecated and near-no-op on current models), and un-streamed
			// thinking blocks are forwarded instead of silently dropped.
			"@agentclientprotocol/claude-agent-acp@0.44.0",
		},
		ManagedFields: []ManagedField{
			{
				ID:          "api_key",
				Label:       "Anthropic API key",
				Type:        "password",
				Required:    true,
				Sensitive:   true,
				Placeholder: "sk-ant-...",
				Help:        "Used by API key setup to authenticate Claude Code.",
			},
			{
				ID:          "base_url",
				Label:       "Anthropic base URL",
				Type:        "url",
				Placeholder: "https://api.anthropic.com",
				Help:        "Optional Claude Code API endpoint override.",
			},
			{
				ID:          "oauth_token",
				Label:       "Claude Code OAuth token",
				Type:        "password",
				Required:    true,
				Sensitive:   true,
				Placeholder: "Token from claude setup-token",
				Help:        "Used by OAuth setup to authenticate Claude Code.",
			},
		},
		SupportedBackends: []string{"local", "container"},
		SetupModes:        []string{setupModeAPIKey, setupModeOAuth, setupModeSelf},
		// Claude Code gets a disposable HOME (credential isolation), the
		// managed settings file (ask/deny pins), env-injected credentials,
		// and a scoped CLAUDE_CONFIG_DIR on local backends. The dir is
		// deliberately not ".claude": the project's own .claude (a settings
		// source) must stay distinct from Memoh's managed config.
		LocalIsolationEnv: "CLAUDE_CONFIG_DIR",
		LocalIsolationDir: ".memoh/claude",
		ManagedSettings:       ManagedSettingsClaude,
		ManagedEnv:            ManagedEnvClaude,
		ManagedThinkingTokens: 16000,
		RequiredManagedFields: map[string][]string{
			SetupModeAPIKey: {"api_key"},
			SetupModeOAuth:  {"oauth_token"},
		},
	}
}

// List returns all registered public profiles, sorted by ID for stable
// API responses.
func List() []PublicProfile {
	out := make([]PublicProfile, 0, len(registry))
	for _, profile := range registry {
		out = append(out, profile.Public())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Lookup returns the registered profile for id (case-insensitive).
func Lookup(id string) (Profile, bool) {
	id = NormalizeAgentID(id)
	profile, ok := registry[id]
	return profile, ok
}

func (p Profile) Public() PublicProfile {
	return PublicProfile{
		ID:                p.ID,
		DisplayName:       p.DisplayName,
		Description:       p.Description,
		ManagedFields:     append([]ManagedField(nil), p.ManagedFields...),
		SupportedBackends: append([]string(nil), p.SupportedBackends...),
		SetupModes:        append([]string(nil), p.SetupModes...),
	}
}

func MetadataAgentEnabled(metadata map[string]any, agentID string) bool {
	setup := ParseAgentSetup(metadata, agentID)
	return setup.Enabled
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

func ParseAgentSetup(metadata map[string]any, agentID string) AgentSetup {
	agentID = NormalizeAgentID(agentID)
	setup := AgentSetup{
		AgentID: agentID,
		Mode:    setupModeAPIKey,
		Managed: map[string]string{},
	}
	if agentID == "" {
		return setup
	}
	acpConfig, ok := metadataRecord(metadata[MetadataKeyACP])
	if !ok {
		return setup
	}

	if agents, ok := metadataRecord(acpConfig["agents"]); ok {
		if agentConfig, ok := metadataRecord(agents[agentID]); ok {
			if enabled, ok := metadataBool(agentConfig["enabled"]); ok {
				setup.Enabled = enabled
			}
			if mode, ok := agentConfig["setup_mode"].(string); ok && strings.TrimSpace(mode) != "" {
				setup.Mode = strings.TrimSpace(strings.ToLower(mode))
				setup.ModeSet = true
			}
			if managed, ok := metadataRecord(agentConfig["managed"]); ok {
				for key, value := range managed {
					if s, ok := value.(string); ok {
						setup.Managed[key] = s
					}
				}
			}
			setup.Mode = normalizeSetupMode(setup.Mode)
			return setup
		}
	}

	return setup
}

func normalizeSetupMode(mode string) string {
	mode = NormalizeAgentID(mode)
	switch mode {
	case setupModeOAuth, setupModeSelf:
		return mode
	case setupModeAPIKey, "":
		return setupModeAPIKey
	default:
		return mode
	}
}

func NormalizeAgentID(agentID string) string {
	return strings.ToLower(strings.TrimSpace(agentID))
}

func ScrubMetadataForResponse(metadata map[string]any) map[string]any {
	cloned := cloneMap(metadata)
	acpConfig, ok := metadataRecord(cloned[MetadataKeyACP])
	if !ok {
		return cloned
	}
	agents, ok := metadataRecord(acpConfig["agents"])
	if !ok {
		return cloned
	}
	for rawAgentID, rawAgent := range agents {
		agentConfig, ok := metadataRecord(rawAgent)
		if !ok {
			continue
		}
		managed, ok := metadataRecord(agentConfig["managed"])
		if !ok {
			continue
		}
		profile, _ := Lookup(rawAgentID)
		sensitive := sensitiveFieldSet(profile)
		for key, value := range managed {
			if !sensitive[key] && !looksSensitiveKey(key) {
				continue
			}
			if s, ok := value.(string); ok && strings.TrimSpace(s) != "" {
				managed[key] = maskSecret(s)
			}
		}
	}
	return cloned
}

func MergeSensitiveFieldsForUpdate(existing, incoming map[string]any) map[string]any {
	merged := cloneMap(incoming)
	existingACP, okExistingACP := metadataRecord(existing[MetadataKeyACP])
	incomingACP, okIncomingACP := metadataRecord(merged[MetadataKeyACP])
	if !okExistingACP || !okIncomingACP {
		return merged
	}
	existingAgents, okExistingAgents := metadataRecord(existingACP["agents"])
	incomingAgents, okIncomingAgents := metadataRecord(incomingACP["agents"])
	if !okExistingAgents || !okIncomingAgents {
		return merged
	}

	for rawAgentID, rawIncomingAgent := range incomingAgents {
		incomingAgent, ok := metadataRecord(rawIncomingAgent)
		if !ok {
			continue
		}
		incomingManaged, ok := metadataRecord(incomingAgent["managed"])
		if !ok {
			continue
		}
		existingAgent, ok := metadataRecord(existingAgents[rawAgentID])
		if !ok {
			continue
		}
		existingManaged, ok := metadataRecord(existingAgent["managed"])
		if !ok {
			continue
		}
		profile, _ := Lookup(rawAgentID)
		sensitive := sensitiveFieldSet(profile)
		for key := range existingManaged {
			if !sensitive[key] && !looksSensitiveKey(key) {
				continue
			}
			value, exists := incomingManaged[key]
			switch {
			case !exists:
				incomingManaged[key] = existingManaged[key]
			case value == nil:
				delete(incomingManaged, key)
			case isMaskedSecretValue(value):
				incomingManaged[key] = existingManaged[key]
			case isEmptyString(value):
				incomingManaged[key] = existingManaged[key]
			}
		}
	}
	return merged
}

func sensitiveFieldSet(profile Profile) map[string]bool {
	out := map[string]bool{}
	for _, field := range profile.ManagedFields {
		if field.Sensitive || field.Type == "password" {
			out[field.ID] = true
		}
	}
	return out
}

func maskSecret(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if strings.HasPrefix(value, "sk-") && len(value) > 7 {
		return "sk-..." + value[len(value)-4:]
	}
	if len(value) > 4 {
		return "***" + value[len(value)-4:]
	}
	return "***"
}

func isMaskedSecretValue(value any) bool {
	s, ok := value.(string)
	if !ok {
		return false
	}
	s = strings.TrimSpace(s)
	if s == "***" {
		return true
	}
	if strings.HasPrefix(s, "sk-...") {
		return len([]rune(strings.TrimPrefix(s, "sk-..."))) == 4
	}
	if strings.HasPrefix(s, "***") {
		return len([]rune(strings.TrimPrefix(s, "***"))) == 4
	}
	return false
}

func isEmptyString(value any) bool {
	s, ok := value.(string)
	return ok && strings.TrimSpace(s) == ""
}

func looksSensitiveKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	return strings.Contains(key, "key") ||
		strings.Contains(key, "token") ||
		strings.Contains(key, "secret") ||
		strings.Contains(key, "password")
}

func cloneMap(in map[string]any) map[string]any {
	if in == nil {
		return map[string]any{}
	}
	payload, err := json.Marshal(in)
	if err != nil {
		out := make(map[string]any, len(in))
		for key, value := range in {
			out[key] = value
		}
		return out
	}
	var out map[string]any
	if err := json.Unmarshal(payload, &out); err != nil || out == nil {
		out = map[string]any{}
	}
	return out
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
