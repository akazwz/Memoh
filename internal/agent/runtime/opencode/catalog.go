package opencode

import (
	"context"
	"net/http"
	"reflect"
	"sort"
	"strings"

	"github.com/felinics/memoh/internal/agent/runtime/external"
	"github.com/felinics/memoh/internal/agent/runtime/opencode/opencodecfg"
)

type nativeModel struct {
	Name string `json:"name"`
	API  struct {
		ID  string `json:"id"`
		NPM string `json:"npm"`
	} `json:"api"`
	Options  map[string]any            `json:"options"`
	Variants map[string]map[string]any `json:"variants"`
}

type nativeAgent struct {
	Name    string `json:"name"`
	Mode    string `json:"mode"`
	Hidden  bool   `json:"hidden"`
	Variant string `json:"variant"`
	Model   struct {
		ProviderID string `json:"providerID"`
		ModelID    string `json:"modelID"`
	} `json:"model"`
	Options    map[string]any      `json:"options"`
	Permission []map[string]string `json:"permission"`
}

func readModelCatalog(ctx context.Context, api *apiClient, cfg opencodecfg.Config, request external.ModelCatalogRequest) (external.ModelCatalog, error) {
	var response struct {
		Providers []struct {
			ID     string                 `json:"id"`
			Models map[string]nativeModel `json:"models"`
		} `json:"providers"`
	}
	if err := api.call(ctx, http.MethodGet, "/config/providers", nil, &response); err != nil {
		return external.ModelCatalog{}, err
	}
	var config struct {
		Model        string `json:"model"`
		DefaultAgent string `json:"default_agent"`
	}
	if err := api.call(ctx, http.MethodGet, "/config", nil, &config); err != nil {
		return external.ModelCatalog{}, err
	}
	var selected nativeAgent
	if request.ResolveDefaults {
		var agents []nativeAgent
		if err := api.call(ctx, http.MethodGet, "/agent", nil, &agents); err != nil {
			return external.ModelCatalog{}, err
		}
		name := firstNonEmpty(promptAgent(request.CollaborationMode, cfg.Agent), config.DefaultAgent, "build")
		for _, agent := range agents {
			if agent.Name == name {
				selected = agent
				break
			}
		}
	}
	catalog := external.ModelCatalog{Models: []external.ModelOption{}, ConfiguredModelID: firstNonEmpty(cfg.Model, config.Model)}
	for _, provider := range response.Providers {
		for id, model := range provider.Models {
			option := external.ModelOption{ID: provider.ID + "/" + id, Name: firstNonEmpty(model.Name, id), ReasoningEfforts: []external.ReasoningEffortOption{}}
			if len(model.Variants) > 0 {
				keys := make([]string, 0, len(model.Variants))
				for key := range model.Variants {
					if key != "default" {
						keys = append(keys, key)
					}
				}
				sort.Strings(keys)
				option.ReasoningEfforts = append(option.ReasoningEfforts, external.ReasoningEffortOption{ID: "default", Name: "default"})
				for _, key := range keys {
					option.ReasoningEfforts = append(option.ReasoningEfforts, external.ReasoningEffortOption{ID: key, Name: key})
				}
				option.DefaultReasoningEffort = "default"
				if request.ResolveDefaults {
					option.ResolvedDefaultReasoningEffort = resolveDefaultEffort(provider.ID, id, model, selected, cfg.BaseURL != "")
				}
			}
			option.Default = option.ID == catalog.ConfiguredModelID
			catalog.Models = append(catalog.Models, option)
		}
	}
	return catalog, nil
}

// OpenCode applies model options, then agent options, then an optional variant.
// Agent.variant is inherited only when that agent pins the selected model:
// https://github.com/anomalyco/opencode/blob/v1.18.31/packages/opencode/src/session/prompt.ts
func resolveDefaultEffort(provider, id string, model nativeModel, agent nativeAgent, customEndpoint bool) string {
	if agent.Model.ProviderID == provider && agent.Model.ModelID == id && model.Variants[agent.Variant] != nil {
		return agent.Variant
	}
	options := mergeOptions(model.Options, agent.Options)
	if value := effortValue(options); value != "" {
		return value
	}
	// Budget-based variants have no universal effort enum. Resolve only a
	// unique matching native preset, never pick the first advertised variant.
	match := ""
	for name, variant := range model.Variants {
		if !hasReasoningOptions(variant) || !containsOptions(options, variant) {
			continue
		}
		if match != "" {
			return ""
		}
		match = name
	}
	if match != "" {
		return match
	}
	if hasReasoningOptions(options) {
		return ""
	}
	// These are documented provider/runtime defaults, not guesses from the
	// variant names. Endpoint overrides can change provider defaults.
	// https://api-docs.deepseek.com/guides/thinking_mode
	if !customEndpoint && (provider == "opencode-go" || provider == "opencode") && strings.HasPrefix(id, "deepseek-v4") {
		return "high"
	}
	// OpenCode's request transform explicitly supplies medium for ordinary
	// GPT-5 models on OpenAI. Other transports (notably Azure) have exceptions.
	// https://github.com/anomalyco/opencode/blob/v1.18.31/packages/opencode/src/provider/transform.ts
	if model.API.NPM == "@ai-sdk/openai" && strings.Contains(model.API.ID, "gpt-5") && !strings.Contains(model.API.ID, "chat") && !strings.Contains(model.API.ID, "pro") {
		return "medium"
	}
	return ""
}

func effortValue(options map[string]any) string {
	for _, keys := range [][]string{{"reasoningEffort"}, {"effort"}, {"reasoning", "effort"}, {"thinkingConfig", "thinkingLevel"}, {"reasoningConfig", "maxReasoningEffort"}} {
		var value any = options
		for _, key := range keys {
			m, _ := value.(map[string]any)
			value = m[key]
		}
		if s, ok := value.(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func hasReasoningOptions(options map[string]any) bool {
	for _, key := range []string{"reasoningEffort", "effort", "reasoning", "thinking", "thinkingConfig", "reasoningConfig"} {
		if _, ok := options[key]; ok {
			return true
		}
	}
	return false
}

func mergeOptions(base, override map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(override))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range override {
		left, lok := out[k].(map[string]any)
		right, rok := v.(map[string]any)
		if lok && rok {
			out[k] = mergeOptions(left, right)
		} else {
			out[k] = v
		}
	}
	return out
}

func containsOptions(options, subset map[string]any) bool {
	for k, v := range subset {
		if m, ok := v.(map[string]any); ok {
			actual, _ := options[k].(map[string]any)
			if !containsOptions(actual, m) {
				return false
			}
		} else if !reflect.DeepEqual(options[k], v) {
			return false
		}
	}
	return true
}
