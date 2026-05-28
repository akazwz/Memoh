package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	"github.com/memohai/memoh/internal/mcp"
	messageevent "github.com/memohai/memoh/internal/message/event"
	"github.com/memohai/memoh/internal/toolapproval"
)

func TestNativeToolSourceAllowlistAndCall(t *testing.T) {
	provider := &nativeSourceTestProvider{
		tools: []sdk.Tool{
			{
				Name:        "safe_tool",
				Description: "Safe tool",
				Parameters:  map[string]any{"type": "object"},
				Execute: func(ctx *sdk.ToolExecContext, input any) (any, error) {
					args, _ := input.(map[string]any)
					return map[string]any{
						"tool":  ctx.ToolName,
						"value": args["value"],
					}, nil
				},
			},
			{
				Name:        "exec",
				Description: "Blocked tool",
				Parameters:  map[string]any{"type": "object"},
				Execute: func(_ *sdk.ToolExecContext, _ any) (any, error) {
					return "blocked", nil
				},
			},
		},
	}
	source := NewNativeToolSource(nil, []ToolProvider{provider}, NativeToolSourceOptions{
		AllowTools: map[string]bool{"safe_tool": true},
	})
	session := mcp.ToolSessionContext{
		BotID:     "bot-1",
		ChatID:    "chat-1",
		SessionID: "session-1",
	}

	tools, err := source.ListTools(context.Background(), session)
	if err != nil {
		t.Fatalf("ListTools() error = %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "safe_tool" {
		t.Fatalf("ListTools() = %#v, want only safe_tool", tools)
	}
	if provider.session.BotID != "bot-1" || provider.session.ChatID != "chat-1" || provider.session.SessionID != "session-1" {
		t.Fatalf("provider session = %#v", provider.session)
	}

	result, err := source.CallTool(context.Background(), session, "safe_tool", map[string]any{"value": "ok"})
	if err != nil {
		t.Fatalf("CallTool(safe_tool) error = %v", err)
	}
	structured, ok := result["structuredContent"].(map[string]any)
	if !ok || structured["tool"] != "safe_tool" || structured["value"] != "ok" {
		t.Fatalf("CallTool structuredContent = %#v", result["structuredContent"])
	}

	if _, err := source.CallTool(context.Background(), session, "exec", map[string]any{}); !errors.Is(err, mcp.ErrToolNotFound) {
		t.Fatalf("CallTool(exec) error = %v, want ErrToolNotFound", err)
	}
}

func TestNativeToolSourceDefaultsToDenyAll(t *testing.T) {
	provider := &nativeSourceTestProvider{
		tools: []sdk.Tool{{
			Name:        "safe_tool",
			Description: "Safe tool",
			Parameters:  map[string]any{"type": "object"},
			Execute: func(_ *sdk.ToolExecContext, _ any) (any, error) {
				return "ok", nil
			},
		}},
	}
	source := NewNativeToolSource(nil, []ToolProvider{provider}, NativeToolSourceOptions{})

	tools, err := source.ListTools(context.Background(), mcp.ToolSessionContext{BotID: "bot-1"})
	if err != nil {
		t.Fatalf("ListTools() error = %v", err)
	}
	if len(tools) != 0 {
		t.Fatalf("ListTools() = %#v, want default deny", tools)
	}
	if _, err := source.CallTool(context.Background(), mcp.ToolSessionContext{BotID: "bot-1"}, "safe_tool", nil); !errors.Is(err, mcp.ErrToolNotFound) {
		t.Fatalf("CallTool() error = %v, want ErrToolNotFound", err)
	}
}

func TestNativeToolSourceWaitsForApprovalAndPublishesRequest(t *testing.T) {
	executed := false
	provider := &nativeSourceTestProvider{
		tools: []sdk.Tool{{
			Name:       "exec",
			Parameters: map[string]any{"type": "object"},
			Execute: func(_ *sdk.ToolExecContext, input any) (any, error) {
				executed = true
				args, _ := input.(map[string]any)
				return map[string]any{"command": args["command"]}, nil
			},
		}},
	}
	approval := &nativeSourceApproval{
		decision: toolapproval.Request{
			ID:      "approval-1",
			ShortID: 7,
			Status:  toolapproval.StatusApproved,
		},
	}
	publisher := &nativeSourcePublisher{}
	source := NewNativeToolSource(nil, []ToolProvider{provider}, NativeToolSourceOptions{
		AllowAll:          true,
		Approval:          approval,
		ApprovalPublisher: publisher,
	})

	result, err := source.CallTool(context.Background(), mcp.ToolSessionContext{
		BotID:             "bot-1",
		SessionID:         "session-1",
		StreamID:          "stream-1",
		ChannelIdentityID: "user-1",
		CurrentPlatform:   "web",
		ReplyTarget:       "reply-1",
		ConversationType:  "private",
	}, "exec", map[string]any{"command": "make test"})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if !executed {
		t.Fatalf("approved tool was not executed")
	}
	if approval.created.ToolName != "exec" || approval.created.ToolInput.(map[string]any)["command"] != "make test" || approval.created.ConversationType != "private" {
		t.Fatalf("approval input = %#v", approval.created)
	}
	if len(publisher.events) != 3 {
		t.Fatalf("published events = %d, want start/message/end", len(publisher.events))
	}
	var payload map[string]any
	if err := json.Unmarshal(publisher.events[1].Data, &payload); err != nil {
		t.Fatalf("decode approval event: %v", err)
	}
	stream, _ := payload["stream"].(map[string]any)
	if stream["type"] != "message" || stream["stream_id"] != "stream-1" {
		t.Fatalf("stream payload = %#v", stream)
	}
	data, _ := stream["data"].(map[string]any)
	approvalPayload, _ := data["approval"].(map[string]any)
	if approvalPayload["approval_id"] != "approval-1" || approvalPayload["status"] != toolapproval.StatusPending {
		t.Fatalf("approval payload = %#v", approvalPayload)
	}
	structured, ok := result["structuredContent"].(map[string]any)
	if !ok || structured["command"] != "make test" {
		t.Fatalf("result = %#v", result)
	}
}

func TestNativeToolSourceRejectedApprovalDoesNotExecute(t *testing.T) {
	executed := false
	provider := &nativeSourceTestProvider{
		tools: []sdk.Tool{{
			Name:       "write",
			Parameters: map[string]any{"type": "object"},
			Execute: func(_ *sdk.ToolExecContext, _ any) (any, error) {
				executed = true
				return "wrote", nil
			},
		}},
	}
	source := NewNativeToolSource(nil, []ToolProvider{provider}, NativeToolSourceOptions{
		AllowAll: true,
		Approval: &nativeSourceApproval{
			decision: toolapproval.Request{
				ID:             "approval-2",
				ShortID:        8,
				Status:         toolapproval.StatusRejected,
				DecisionReason: "not now",
			},
		},
		ApprovalPublisher: &nativeSourcePublisher{},
	})

	result, err := source.CallTool(context.Background(), mcp.ToolSessionContext{
		BotID:     "bot-1",
		SessionID: "session-1",
		StreamID:  "stream-1",
	}, "write", map[string]any{"path": "file.txt", "content": "x"})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if executed {
		t.Fatalf("rejected tool should not execute")
	}
	if isError, _ := result["isError"].(bool); !isError {
		t.Fatalf("result = %#v, want MCP error result", result)
	}
}

type nativeSourceTestProvider struct {
	tools   []sdk.Tool
	session SessionContext
}

func (p *nativeSourceTestProvider) Tools(_ context.Context, session SessionContext) ([]sdk.Tool, error) {
	p.session = session
	return p.tools, nil
}

type nativeSourceApproval struct {
	created  toolapproval.CreatePendingInput
	decision toolapproval.Request
}

func (*nativeSourceApproval) EvaluatePolicy(context.Context, toolapproval.CreatePendingInput) (toolapproval.Evaluation, error) {
	return toolapproval.Evaluation{Decision: toolapproval.DecisionNeedsApproval}, nil
}

func (a *nativeSourceApproval) CreatePending(_ context.Context, input toolapproval.CreatePendingInput) (toolapproval.Request, error) {
	a.created = input
	return toolapproval.Request{
		ID:                a.decision.ID,
		BotID:             input.BotID,
		SessionID:         input.SessionID,
		RouteID:           input.RouteID,
		ChannelIdentityID: input.ChannelIdentityID,
		ToolCallID:        input.ToolCallID,
		ToolName:          input.ToolName,
		ToolInput:         input.ToolInput.(map[string]any),
		ShortID:           a.decision.ShortID,
		Status:            toolapproval.StatusPending,
	}, nil
}

func (*nativeSourceApproval) Reject(_ context.Context, approvalID, _, reason string) (toolapproval.Request, error) {
	return toolapproval.Request{
		ID:             approvalID,
		Status:         toolapproval.StatusRejected,
		DecisionReason: reason,
	}, nil
}

func (a *nativeSourceApproval) WaitForDecision(context.Context, string) (toolapproval.Request, error) {
	return a.decision, nil
}

type nativeSourcePublisher struct {
	events []messageevent.Event
}

func (p *nativeSourcePublisher) Publish(event messageevent.Event) {
	p.events = append(p.events, event)
}
