package grok

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/felinics/memoh/internal/agent/runtime/external"
	"github.com/felinics/memoh/internal/agent/runtime/grok/grokcfg"
)

func (l *lease) configure(ctx context.Context, input external.PromptInput) error {
	model := first(input.ModelID, l.cfg.Model)
	if model != "" {
		models := l.session.Models.AvailableModels
		if len(models) == 0 {
			models = l.caps.Meta.ModelState.AvailableModels
		}
		if !slices.ContainsFunc(models, func(m nativeModel) bool { return m.ID == model }) {
			return errors.New("grok model unavailable")
		}
		if _, err := call[map[string]any](ctx, l.t, "session/set_model", map[string]any{"sessionId": l.t.sessionID, "modelId": model}); err != nil {
			return err
		}
	}
	if model != "" {
		l.session.Models.CurrentModelID = model
		l.caps.Meta.ModelState.CurrentModelID = model
	}
	if effort := input.ReasoningEffort; effort != "" {
		response, err := call[sessionResponse](ctx, l.t, "session/set_config_option", map[string]any{"sessionId": l.t.sessionID, "configId": "reasoning_effort", "value": effort})
		if err != nil {
			return err
		}
		found := false
		for _, option := range response.ConfigOptions {
			if option.ID == "reasoning_effort" && option.CurrentValue == effort {
				found = true
			}
		}
		if !found {
			return errors.New("grok did not confirm reasoning effort")
		}
	}
	mode := metaString(input.RuntimeMetadata, "collaboration_mode")
	if mode != "" {
		if mode != "default" && mode != "plan" {
			return external.ErrModeUnavailable
		}
		if _, err := call[map[string]any](ctx, l.t, "session/set_mode", map[string]any{"sessionId": l.t.sessionID, "modeId": mode}); err != nil {
			return err
		}
	}
	return nil
}

func (d *Driver) ModelCatalog(ctx context.Context, request external.ModelCatalogRequest) (external.ModelCatalog, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	// Grok advertises every model's reasoning choices in the same catalog.
	// Applying the picker's selection here would turn it into the advertised
	// default and make selecting "Default" stick to the last explicit choice.
	input := external.PromptInput{BotID: request.BotID, BotAgentID: request.BotAgentID, RuntimeMetadata: map[string]any{"project_path": request.ProjectPath}}
	l, err := d.open(ctx, input, false)
	if err != nil {
		return external.ModelCatalog{}, runtimeError(err)
	}
	defer l.close(ctx)
	models := l.session.Models
	if len(models.AvailableModels) == 0 {
		models = l.caps.Meta.ModelState
	}
	catalog := catalogFromModels(models, l.cfg.Model)
	// Discovery has no checkpoint to publish. Once its RPCs have succeeded,
	// dispose of the private process via close; shutdown failure must not
	// discard the returned catalog (Grok 1.0.30 can crash on OAuth stdin EOF).
	return catalog, nil
}

func catalogFromModels(state modelState, configured string) external.ModelCatalog {
	result := external.ModelCatalog{ConfiguredModelID: configured, Models: []external.ModelOption{}}
	for _, m := range state.AvailableModels {
		option := external.ModelOption{ID: m.ID, Name: first(m.Name, m.ID), Description: m.Description, Default: m.ID == state.CurrentModelID, DefaultReasoningEffort: m.Meta.ReasoningEffort, ReasoningEfforts: []external.ReasoningEffortOption{}}
		for _, e := range m.Meta.ReasoningEfforts {
			option.ReasoningEfforts = append(option.ReasoningEfforts, external.ReasoningEffortOption{ID: first(e.ID, e.Value), Name: first(e.Label, e.ID, e.Value), Description: e.Description})
			if option.DefaultReasoningEffort == "" && e.Default {
				option.DefaultReasoningEffort = first(e.ID, e.Value)
			}
		}
		result.Models = append(result.Models, option)
	}
	return result
}

func (d *Driver) Modes(ctx context.Context, input external.PromptInput) (external.ModeState, error) {
	agent, err := d.agents.Get(ctx, input.BotID, input.BotAgentID)
	if err != nil {
		return external.ModeState{}, err
	}
	return permissionModes(first(metaString(input.RuntimeMetadata, "permission_mode"), metaString(agent.Metadata, "permission_mode"), "ask"))
}

func (*Driver) SetMode(_ context.Context, _ external.PromptInput, mode string) (external.ModeState, error) {
	return permissionModes(mode)
}

func permissionModes(mode string) (external.ModeState, error) {
	if mode == "" || !grokcfg.ValidPermissionMode(mode) {
		return external.ModeState{}, external.ErrModeUnavailable
	}
	return external.ModeState{Kind: "permission", Supported: true, ApplyOnNextTurn: true, CurrentModeID: mode, AvailableModes: []external.Mode{{ID: "ask", I18nKey: "runtime.grok.modes.ask", Icon: "hand"}, {ID: "auto", I18nKey: "runtime.grok.modes.auto", Icon: "shield-check"}, {ID: "always-approve", I18nKey: "runtime.grok.modes.alwaysApprove", Icon: "shield-alert", Warning: true}}}, nil
}

func (*Driver) PlanMode(_ context.Context, input external.PromptInput) (external.ModeState, error) {
	return planModes(first(metaString(input.RuntimeMetadata, "collaboration_mode"), metaString(input.RuntimeMetadata, "grok_native_mode"), "default"))
}

func (*Driver) SetPlanMode(_ context.Context, _ external.PromptInput, mode string) (external.ModeState, error) {
	return planModes(mode)
}

func planModes(mode string) (external.ModeState, error) {
	if mode != "default" && mode != "plan" {
		return external.ModeState{}, external.ErrModeUnavailable
	}
	return external.ModeState{Supported: true, ApplyOnNextTurn: true, CurrentModeID: mode, AvailableModes: []external.Mode{{ID: "default", I18nKey: "runtime.planModes.default"}, {ID: "plan", I18nKey: "runtime.planModes.plan"}}}, nil
}

// Compact's RPC resolves only after the native compaction actor completes.
// The caller already owns the run slot and publishes the staged revision.
func (d *Driver) Compact(ctx context.Context, input external.PromptInput) (external.CompactionResult, error) {
	l, err := d.open(ctx, input, true)
	if err != nil {
		return external.CompactionResult{}, runtimeError(err)
	}
	defer l.close(ctx)
	if metaString(input.RuntimeMetadata, "grok_session_id") == "" {
		return external.CompactionResult{}, external.ErrThreadUnavailable
	}
	requestCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, e := call[map[string]any](requestCtx, l.t, "_x.ai/compact_conversation", map[string]any{"sessionId": l.t.sessionID, "userContext": input.CommandArgs})
		done <- e
	}()
	select {
	case err = <-done:
	case <-ctx.Done():
		l.cancel()
		cancel()
		<-done
		return external.CompactionResult{}, ctx.Err()
	}
	if err != nil {
		return external.CompactionResult{}, runtimeError(err)
	}
	l.t.closeDecisions()
	if err = l.drain(); err != nil {
		return external.CompactionResult{}, runtimeError(err)
	}
	l.persistAuth(ctx, input)
	stageCtx, stageCancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer stageCancel()
	if err = d.stage(stageCtx, l.client, input, l.home, l.t.sessionID, l.cwd, l.epoch); err != nil {
		return external.CompactionResult{}, err
	}
	return external.CompactionResult{RuntimeMetadata: l.metadata(input), Checkpoint: external.CheckpointStaged}, nil
}

func (*Driver) Commands(_ context.Context, _ external.PromptInput) ([]external.Command, error) {
	return []external.Command{{Name: "compact", I18nKey: "runtime.grok.commands.compact", Kind: external.CommandOperation}}, nil
}

func (*Driver) ReadCommand(_ context.Context, _ external.PromptInput) (external.CommandResult, error) {
	return external.CommandResult{}, external.ErrCommandUnavailable
}
