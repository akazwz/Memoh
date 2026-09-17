package opencode

import (
	"context"
	"net/http"
	"net/url"

	"github.com/felinics/memoh/internal/agent/runtime/external"
	"github.com/felinics/memoh/internal/agent/runtime/opencode/opencodecfg"
)

func (d *Driver) Modes(ctx context.Context, input external.PromptInput) (external.ModeState, error) {
	agent, err := d.agents.Get(ctx, input.BotID, input.BotAgentID)
	if err != nil {
		return external.ModeState{}, err
	}
	return permissionModes(firstNonEmpty(metadataString(input.RuntimeMetadata, "permission_mode"), metadataString(agent.Metadata, "permission_mode"), "default"))
}

func (d *Driver) PlanMode(ctx context.Context, input external.PromptInput) (external.ModeState, error) {
	agent, err := d.agents.Get(ctx, input.BotID, input.BotAgentID)
	if err != nil {
		return external.ModeState{}, err
	}
	mode := metadataString(input.RuntimeMetadata, "collaboration_mode")
	if mode == "" && metadataString(agent.Metadata, "agent") == "plan" {
		mode = "plan"
	}
	return planMode(firstNonEmpty(mode, "default"))
}

func (*Driver) SetPlanMode(_ context.Context, _ external.PromptInput, mode string) (external.ModeState, error) {
	return planMode(mode)
}

func planMode(mode string) (external.ModeState, error) {
	if mode != "default" && mode != "plan" {
		return external.ModeState{}, external.ErrModeUnavailable
	}
	return external.ModeState{Supported: true, CurrentModeID: mode, AvailableModes: []external.Mode{
		{ID: "default", I18nKey: "runtime.planModes.default"},
		{ID: "plan", I18nKey: "runtime.planModes.plan"},
	}}, nil
}

func promptAgent(mode, configured string) string {
	if mode == "plan" {
		return "plan"
	}
	if mode == "default" && configured == "plan" {
		return "build"
	}
	return configured
}

func (t *turnRunner) applyPermissions(ctx context.Context, cfg opencodecfg.Config) error {
	mode := firstNonEmpty(metadataString(t.input.RuntimeMetadata, "permission_mode"), cfg.PermissionMode, "default")
	rules, err := sessionPermissions(mode)
	if err != nil {
		return err
	}
	if promptAgent(metadataString(t.input.RuntimeMetadata, "collaboration_mode"), cfg.Agent) == "plan" {
		var agents []nativeAgent
		if err := t.api.call(ctx, http.MethodGet, "/agent", nil, &agents); err != nil {
			return err
		}
		found := false
		for _, agent := range agents {
			if agent.Name != "plan" || agent.Mode == "subagent" {
				continue
			}
			found = true
			rules = constrainPlanPermissions(rules, agent.Permission)
		}
		if !found {
			return external.ErrModeUnavailable
		}
	}
	return t.api.call(ctx, http.MethodPatch, "/session/"+url.PathEscape(t.sessionID), map[string]any{"permission": rules}, nil)
}

// Session presets must not erase the plan agent's deny rules. Keep later
// exceptions (such as its native plans directory), without copying a broad
// allow rule that would disable the selected approval mode.
func constrainPlanPermissions(rules, plan []map[string]string) []map[string]string {
	denied := map[string]bool{}
	for _, rule := range plan {
		permission := rule["permission"]
		if rule["action"] == "deny" {
			denied[permission] = true
		}
		if denied[permission] {
			rules = append(rules, rule)
		}
	}
	return rules
}

func (*Driver) SetMode(_ context.Context, _ external.PromptInput, mode string) (external.ModeState, error) {
	return permissionModes(mode)
}

func permissionModes(mode string) (external.ModeState, error) {
	switch mode {
	case "default", "allow", "inherit":
	default:
		return external.ModeState{}, external.ErrModeUnavailable
	}
	return external.ModeState{Kind: "permission", ApplyOnNextTurn: true, Supported: true, CurrentModeID: mode, AvailableModes: []external.Mode{
		{ID: "default", I18nKey: "runtime.opencode.modes.default", Icon: "hand"},
		{ID: "inherit", I18nKey: "runtime.opencode.modes.inherit", Icon: "shield-terminal"},
		{ID: "allow", I18nKey: "runtime.opencode.modes.allow", Icon: "shield-alert", Warning: true},
	}}, nil
}

// A per-session ruleset comes after project and agent rules. Setting it each
// turn makes next-turn mode changes effective even for resumed sessions.
func sessionPermissions(mode string) ([]map[string]string, error) {
	if _, err := permissionModes(mode); err != nil {
		return nil, err
	}
	rules := []map[string]string{}
	if mode == "inherit" {
		return rules, nil
	}
	action := "ask"
	if mode == "allow" {
		action = "allow"
	}
	rules = append(rules, map[string]string{"permission": "*", "pattern": "*", "action": action})
	for _, name := range []string{"read", "glob", "grep", "question", "memoh_*"} {
		rules = append(rules, map[string]string{"permission": name, "pattern": "*", "action": "allow"})
	}
	return rules, nil
}
