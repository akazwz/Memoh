package grok

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/felinics/memoh/internal/agent/decision/approval"
	userinput "github.com/felinics/memoh/internal/agent/decision/input"
	"github.com/felinics/memoh/internal/agent/event"
)

func (t *turnSession) permission(ctx context.Context, raw json.RawMessage) (any, *acp.RequestError) {
	var p struct {
		ToolCall struct {
			ID       string `json:"toolCallId"`
			Title    string `json:"title"`
			RawInput any    `json:"rawInput"`
		} `json:"toolCall"`
		Options []struct {
			ID   string `json:"optionId"`
			Name string `json:"name"`
			Kind string `json:"kind"`
		} `json:"options"`
	}
	cancelled := map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}
	if json.Unmarshal(raw, &p) != nil || p.ToolCall.ID == "" || len(p.Options) == 0 {
		return nil, acp.NewInvalidParams(nil)
	}
	if t.driver.approval == nil {
		return cancelled, nil
	}
	options := make([]approval.PermissionOption, 0, len(p.Options))
	for _, o := range p.Options {
		options = append(options, approval.PermissionOption{ID: o.ID, Name: o.Name, Kind: o.Kind})
	}
	detail, marshalErr := json.Marshal(p.ToolCall.RawInput)
	if marshalErr != nil {
		return nil, acp.NewInvalidParams(nil)
	}
	permissionInput := map[string]any{"title": first(p.ToolCall.Title, "Grok tool"), "request": string(detail), "request_lang": "json"}
	flow, err := approval.RunRuntimeFlow(ctx, t.driver.approval, approval.FlowRequest{
		Input:       approval.CreatePendingInput{BotID: t.input.BotID, SessionID: t.input.ThreadID, RouteID: t.input.RouteID, ChannelIdentityID: t.input.ChannelIdentityID, RequestedByChannelIdentityID: t.input.ChannelIdentityID, ToolCallID: p.ToolCall.ID, ToolName: "permission", ToolInput: permissionInput, Options: options, SourcePlatform: t.input.CurrentPlatform, ReplyTarget: t.input.ReplyTarget, ConversationType: t.input.ConversationType},
		Interactive: t.input.CanRequestUserInput, RegisterWaiter: t.driver.approval.RegisterWaiter,
		Emit: func(req approval.Request) bool {
			t.emit(event.StreamEvent{Type: event.ToolApprovalRequest, ToolCallID: req.ToolCallID, ToolName: req.ToolName, Input: req.ToolInput, ApprovalID: req.ID, ShortID: req.ShortID, Status: approval.NormalizedStatus(req.Status), Metadata: map[string]any{"approval": approval.RequestMetadata(req)}})
			return t.input.Sink != nil
		},
	})
	if err != nil || ctx.Err() != nil || !flow.DecidedByUser {
		return cancelled, nil
	}
	for _, o := range options {
		if o.ID == flow.SelectedOptionID && o.Approves() == flow.Approved {
			return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": o.ID}}, nil
		}
	}
	// Older decision clients do not submit option IDs. Never turn a one-time
	// decision into a durable allowance; choose only an offered once option.
	kind := approval.OptionKindRejectOnce
	if flow.Approved {
		kind = approval.OptionKindAllowOnce
	}
	if flow.SelectedOptionID == "" {
		for _, o := range options {
			if o.Kind == kind {
				return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": o.ID}}, nil
			}
		}
	}
	return cancelled, nil
}

type nativeQuestion struct {
	Question    string `json:"question"`
	MultiSelect bool   `json:"multiSelect"`
	Options     []struct {
		Label       string `json:"label"`
		Description string `json:"description"`
		Preview     string `json:"preview"`
	} `json:"options"`
}

func (t *turnSession) questions(ctx context.Context, raw json.RawMessage) (any, *acp.RequestError) {
	var p struct {
		ToolCallID string           `json:"toolCallId"`
		Questions  []nativeQuestion `json:"questions"`
	}
	if json.Unmarshal(raw, &p) != nil || p.ToolCallID == "" || len(p.Questions) == 0 || len(p.Questions) > userinput.MaxQuestionsPerRequest {
		return nil, acp.NewInvalidParams(nil)
	}
	cancelled := map[string]any{"outcome": "cancelled"}
	questions := make([]any, 0, len(p.Questions))
	for _, q := range p.Questions {
		v := map[string]any{"text": q.Question, "kind": userinput.QuestionKindText}
		if len(q.Options) >= userinput.MinOptionsPerQuestion {
			// Truncation would silently remove native choices. Refuse an unsupported
			// shape instead so the model can ask a smaller question.
			if len(q.Options) > userinput.MaxOptionsPerQuestion {
				return nil, acp.NewInvalidParams(nil)
			}
			opts := make([]any, 0, len(q.Options))
			for _, o := range q.Options {
				opts = append(opts, map[string]any{"label": o.Label, "description": o.Description})
			}
			v["options"] = opts
			v["allow_custom"] = true
			v["kind"] = userinput.QuestionKindSingleSelect
			if q.MultiSelect {
				v["kind"] = userinput.QuestionKindMultiSelect
			}
		}
		questions = append(questions, v)
	}
	req, err := t.requestInput(ctx, p.ToolCallID, map[string]any{"questions": questions}, "grok_ask_user_question")
	if err != nil || req.Status != userinput.StatusSubmitted || ctx.Err() != nil {
		return cancelled, nil
	}
	answers := map[string][]string{}
	annotations := map[string]any{}
	byID := map[string]userinput.UIAnswer{}
	for _, a := range userinput.AnswersFromResult(req.Result) {
		byID[a.QuestionID] = a
	}
	for i, q := range p.Questions {
		a, ok := byID[fmt.Sprintf("q%d", i+1)]
		if !ok || a.Skipped {
			return cancelled, nil
		}
		values := []string{}
		ann := map[string]any{}
		for _, selected := range a.Selected {
			values = append(values, selected.Label)
			if !q.MultiSelect {
				for _, o := range q.Options {
					if o.Label == selected.Label && o.Preview != "" {
						ann["preview"] = o.Preview
					}
				}
			}
		}
		notes := first(a.CustomText, a.Text)
		if notes != "" {
			ann["notes"] = notes
			if len(values) == 0 {
				values = append(values, "Other")
			}
		}
		if len(values) == 0 {
			return cancelled, nil
		}
		answers[q.Question] = values
		if len(ann) > 0 {
			annotations[q.Question] = ann
		}
	}
	return map[string]any{"outcome": "accepted", "answers": answers, "annotations": annotations}, nil
}

func (t *turnSession) requestInput(ctx context.Context, callID string, input any, source string) (userinput.Request, error) {
	if t.driver.userInput == nil {
		return userinput.Request{}, errors.New("user input service unavailable")
	}
	expires := time.Now().Add(userinput.DefaultWaitTimeout + time.Minute)
	flow, err := userinput.RunFlow(ctx, t.driver.userInput, userinput.FlowRequest{
		Input:                  userinput.CreatePendingInput{BotID: t.input.BotID, SessionID: t.input.ThreadID, RouteID: t.input.RouteID, ChannelIdentityID: t.input.ChannelIdentityID, RequestedByChannelIdentityID: t.input.ChannelIdentityID, ToolCallID: callID, ToolName: userinput.ToolNameAskUser, Input: input, ProviderMetadata: map[string]any{"source": source, "run_id": t.input.RunID, "native_session_id": t.sessionID}, SourcePlatform: t.input.CurrentPlatform, ReplyTarget: t.input.ReplyTarget, ConversationType: t.input.ConversationType, ExpiresAt: &expires},
		ActorChannelIdentityID: t.input.ChannelIdentityID, Interactive: t.input.CanRequestUserInput, WaitTimeout: userinput.DefaultWaitTimeout,
		Emit: func(req userinput.Request) bool {
			t.emit(event.StreamEvent{Type: event.UserInputRequest, ToolCallID: req.ToolCallID, ToolName: req.ToolName, Input: req.Input, UserInputID: req.ID, ShortID: req.ShortID, Status: req.Status, Metadata: userinput.DeferredMetadata(req)})
			return t.input.Sink != nil
		},
		NonInteractiveReason: "Grok requested user input without an interactive stream", UndeliveredReason: "Grok user input was not delivered", TimeoutReason: "Grok user input timed out", AbortReason: "Grok user input aborted",
	})
	return flow.Request, err
}

func (t *turnSession) exitPlan(ctx context.Context, raw json.RawMessage) (any, *acp.RequestError) {
	var p struct {
		ToolCallID  string `json:"toolCallId"`
		PlanContent string `json:"planContent"`
	}
	if json.Unmarshal(raw, &p) != nil || p.ToolCallID == "" {
		return nil, acp.NewInvalidParams(nil)
	}
	req, err := t.requestInput(ctx, p.ToolCallID, map[string]any{"questions": []any{map[string]any{
		"text": first(p.PlanContent, "Review the Grok Build plan"), "kind": userinput.QuestionKindSingleSelect, "allow_custom": true,
		"options": []any{map[string]any{"label": "Approve plan"}, map[string]any{"label": "Abandon plan"}},
	}}}, "grok_exit_plan_mode")
	if err != nil || req.Status != userinput.StatusSubmitted || ctx.Err() != nil {
		return map[string]any{"outcome": "cancelled"}, nil
	}
	for _, a := range userinput.AnswersFromResult(req.Result) {
		feedback := strings.TrimSpace(first(a.CustomText, a.Text))
		if feedback != "" {
			return map[string]any{"outcome": "cancelled", "feedback": feedback}, nil
		}
		if len(a.Selected) == 1 && !a.Skipped {
			switch a.Selected[0].Label {
			case "Approve plan":
				return map[string]any{"outcome": "approved"}, nil
			case "Abandon plan":
				return map[string]any{"outcome": "abandoned"}, nil
			}
		}
	}
	return map[string]any{"outcome": "cancelled"}, nil
}
