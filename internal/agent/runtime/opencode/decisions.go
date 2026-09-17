package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/felinics/memoh/internal/agent/decision/approval"
	userinput "github.com/felinics/memoh/internal/agent/decision/input"
	"github.com/felinics/memoh/internal/agent/event"
)

func (t *turnRunner) permission(ctx context.Context, raw json.RawMessage) error {
	var request struct {
		ID         string         `json:"id"`
		Permission string         `json:"permission"`
		Patterns   []string       `json:"patterns"`
		Metadata   map[string]any `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		return err
	}
	reply := "reject"
	if strings.HasPrefix(request.Permission, "memoh_") {
		// The gateway enforces Memoh permissions with the trusted run identity.
		reply = "once"
	} else if t.approval != nil {
		input := map[string]any{"title": request.Permission, "request": strings.Join(request.Patterns, "\n")}
		if len(request.Metadata) > 0 {
			payload, err := json.MarshalIndent(request.Metadata, "", "  ")
			if err != nil {
				return err
			}
			input["request"] = string(payload)
			input["request_lang"] = "json"
		}
		result, err := approval.RunRuntimeFlow(ctx, t.approval, approval.FlowRequest{
			Input: approval.CreatePendingInput{
				BotID: t.input.BotID, SessionID: t.input.ThreadID, RouteID: t.input.RouteID,
				ChannelIdentityID: t.input.ChannelIdentityID, RequestedByChannelIdentityID: t.input.ChannelIdentityID,
				ToolCallID: "opencode-permission-" + request.ID, ToolName: "permission", ToolInput: input,
			},
			Interactive: t.input.CanRequestUserInput, RegisterWaiter: t.approval.RegisterWaiter,
			Emit: func(req approval.Request) bool {
				t.emit(event.StreamEvent{Type: event.ToolApprovalRequest, ToolCallID: req.ToolCallID, ToolName: req.ToolName, Input: req.ToolInput, ApprovalID: req.ID, ShortID: req.ShortID, Status: approval.NormalizedStatus(req.Status), Metadata: map[string]any{"approval": approval.RequestMetadata(req)}})
				return true
			},
		})
		if err != nil && ctx.Err() == nil {
			return err
		}
		if result.Approved {
			reply = "once"
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return t.api.call(ctx, http.MethodPost, "/permission/"+url.PathEscape(request.ID)+"/reply", map[string]string{"reply": reply}, nil)
}

func (t *turnRunner) question(ctx context.Context, raw json.RawMessage) error {
	var request struct {
		ID        string `json:"id"`
		Questions []struct {
			Question string `json:"question"`
			Multiple bool   `json:"multiple"`
			Custom   *bool  `json:"custom"`
			Options  []struct {
				Label       string `json:"label"`
				Description string `json:"description"`
			} `json:"options"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		return err
	}
	reject := func() error {
		return t.api.call(ctx, http.MethodPost, "/question/"+url.PathEscape(request.ID)+"/reject", nil, nil)
	}
	if t.userInput == nil || len(request.Questions) == 0 || len(request.Questions) > userinput.MaxQuestionsPerRequest {
		return reject()
	}
	questions := make([]any, 0, len(request.Questions))
	for _, q := range request.Questions {
		// Memoh select questions require at least two choices. Do not turn
		// a restricted native choice into unrestricted text input.
		if len(q.Options) > 0 && len(q.Options) < userinput.MinOptionsPerQuestion || len(q.Options) == 0 && q.Custom != nil && !*q.Custom {
			return reject()
		}
		question := map[string]any{"text": q.Question, "kind": userinput.QuestionKindText}
		if len(q.Options) >= userinput.MinOptionsPerQuestion {
			if len(q.Options) > userinput.MaxOptionsPerQuestion {
				return reject()
			}
			options := make([]any, 0, len(q.Options))
			for _, option := range q.Options {
				options = append(options, map[string]any{"label": option.Label, "description": option.Description})
			}
			question["options"] = options
			question["allow_custom"] = q.Custom == nil || *q.Custom
			question["kind"] = userinput.QuestionKindSingleSelect
			if q.Multiple {
				question["kind"] = userinput.QuestionKindMultiSelect
			}
		}
		questions = append(questions, question)
	}
	expires := time.Now().Add(userinput.DefaultWaitTimeout + time.Minute)
	flow, err := userinput.RunFlow(ctx, t.userInput, userinput.FlowRequest{
		Input: userinput.CreatePendingInput{
			BotID: t.input.BotID, SessionID: t.input.ThreadID, RouteID: t.input.RouteID,
			ChannelIdentityID: t.input.ChannelIdentityID, RequestedByChannelIdentityID: t.input.ChannelIdentityID,
			ToolCallID: "opencode-question-" + request.ID, ToolName: userinput.ToolNameAskUser,
			Input: map[string]any{"questions": questions}, ProviderMetadata: map[string]any{"source": "opencode", "run_id": t.input.RunID},
			SourcePlatform: t.input.CurrentPlatform, ReplyTarget: t.input.ReplyTarget, ConversationType: t.input.ConversationType, ExpiresAt: &expires,
		},
		ActorChannelIdentityID: t.input.ChannelIdentityID, Interactive: t.input.CanRequestUserInput,
		Emit: func(req userinput.Request) bool {
			t.emit(event.StreamEvent{Type: event.UserInputRequest, ToolCallID: req.ToolCallID, ToolName: req.ToolName, Input: req.Input, UserInputID: req.ID, ShortID: req.ShortID, Status: firstNonEmpty(req.Status, userinput.StatusPending), Metadata: userinput.DeferredMetadata(req)})
			return true
		},
	})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil || flow.Request.Status != userinput.StatusSubmitted {
		return reject()
	}
	byID := map[string]userinput.UIAnswer{}
	for _, answer := range userinput.AnswersFromResult(flow.Request.Result) {
		byID[answer.QuestionID] = answer
	}
	answers := make([][]string, len(questions))
	for i := range questions {
		answer, ok := byID[fmt.Sprintf("q%d", i+1)]
		if !ok || answer.Skipped {
			return reject()
		}
		for _, selected := range answer.Selected {
			answers[i] = append(answers[i], selected.Label)
		}
		if custom := strings.TrimSpace(answer.CustomText); custom != "" {
			answers[i] = append(answers[i], custom)
		}
		if text := strings.TrimSpace(answer.Text); text != "" {
			answers[i] = append(answers[i], text)
		}
		if len(answers[i]) == 0 {
			return reject()
		}
	}
	return t.api.call(ctx, http.MethodPost, "/question/"+url.PathEscape(request.ID)+"/reply", map[string]any{"answers": answers}, nil)
}
