package opencode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/felinics/memoh/internal/agent/runtime/external"
	"github.com/felinics/memoh/internal/agent/runtime/opencode/opencodecfg"
)

func TestCatalogResolvesDefaultWithoutPinningPreference(t *testing.T) {
	model := nativeModel{Name: "Model", Options: map[string]any{"reasoningEffort": "medium"}, Variants: map[string]map[string]any{"low": {"reasoningEffort": "low"}, "high": {"reasoningEffort": "high"}}}
	build := nativeAgent{Name: "build", Mode: "primary", Options: map[string]any{"reasoningEffort": "high"}}
	plan := nativeAgent{Name: "plan", Mode: "primary", Options: map[string]any{"reasoningEffort": "low"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/config/providers":
			_ = json.NewEncoder(w).Encode(map[string]any{"providers": []any{map[string]any{"id": "test", "models": map[string]any{"model": model}}}})
		case "/config":
			_ = json.NewEncoder(w).Encode(map[string]any{"model": "test/model"})
		case "/agent":
			_ = json.NewEncoder(w).Encode([]nativeAgent{build, plan})
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	api := &apiClient{http: server.Client(), baseURL: server.URL}
	for mode, want := range map[string]string{"default": "high", "plan": "low"} {
		catalog, err := readModelCatalog(context.Background(), api, opencodecfg.Config{}, external.ModelCatalogRequest{ResolveDefaults: true, CollaborationMode: mode})
		if err != nil {
			t.Fatal(err)
		}
		option := catalog.Models[0]
		if option.DefaultReasoningEffort != "default" || option.ResolvedDefaultReasoningEffort != want {
			t.Fatalf("mode %s: default was pinned or resolved incorrectly: %+v", mode, option)
		}
		body, err := promptBody(external.PromptInput{ReasoningEffort: option.DefaultReasoningEffort}, opencodecfg.Config{}, "msg_test")
		if err != nil {
			t.Fatal(err)
		}
		if _, pinned := body["variant"]; pinned {
			t.Fatal("display resolution pinned the native variant")
		}
	}
}

func TestDefaultEffortRequiresEvidence(t *testing.T) {
	model := nativeModel{Variants: map[string]map[string]any{"high": {"reasoningEffort": "high"}, "low": {"reasoningEffort": "low"}}}
	if got := resolveDefaultEffort("custom", "m", model, nativeAgent{}, false); got != "" {
		t.Fatalf("guessed %q from available variants", got)
	}
	if got := resolveDefaultEffort("opencode-go", "deepseek-v4.1-flash", model, nativeAgent{}, false); got != "high" {
		t.Fatalf("DeepSeek default = %q", got)
	}
	if got := resolveDefaultEffort("opencode-go", "deepseek-v4.1-flash", model, nativeAgent{}, true); got != "" {
		t.Fatalf("custom endpoint inherited provider default: %q", got)
	}
	agent := nativeAgent{Variant: "low", Options: map[string]any{"reasoningEffort": "high"}}
	agent.Model.ProviderID, agent.Model.ModelID = "test", "m"
	if got := resolveDefaultEffort("test", "m", model, agent, false); got != "low" {
		t.Fatalf("agent variant precedence = %q", got)
	}
	if got := resolveDefaultEffort("test", "other", model, agent, false); got != "high" {
		t.Fatalf("agent variant leaked to another model: %q", got)
	}
}

func TestPlanKeepsNativeDenyRulesAndApprovalChoice(t *testing.T) {
	plan := []map[string]string{
		{"permission": "*", "pattern": "*", "action": "allow"},
		{"permission": "edit", "pattern": "*", "action": "deny"},
		{"permission": "edit", "pattern": ".opencode/plans/*.md", "action": "allow"},
		{"permission": "task", "pattern": "general", "action": "deny"},
	}
	for _, mode := range []string{"default", "allow", "inherit"} {
		rules, err := sessionPermissions(mode)
		if err != nil {
			t.Fatal(err)
		}
		rules = constrainPlanPermissions(rules, plan)
		if len(rules) < 3 || rules[len(rules)-3]["action"] != "deny" || rules[len(rules)-2]["pattern"] != ".opencode/plans/*.md" {
			t.Fatalf("plan restrictions lost in %s: %+v", mode, rules)
		}
		if mode == "default" && rules[0]["action"] != "ask" {
			t.Fatal("plan bypassed approval mode")
		}
	}
	for _, mode := range []string{"default", "plan"} {
		body, err := promptBody(external.PromptInput{RuntimeMetadata: map[string]any{"collaboration_mode": mode}}, opencodecfg.Config{}, "msg_test")
		if err != nil {
			t.Fatal(err)
		}
		if mode == "default" && body["agent"] == "default" {
			t.Fatal("sent a nonexistent native agent")
		}
		if mode == "plan" && body["agent"] != "plan" {
			t.Fatal("plan mode was not applied")
		}
	}
}
