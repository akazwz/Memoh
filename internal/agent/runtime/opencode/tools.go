package opencode

import (
	"context"
	"errors"

	"github.com/felinics/memoh/internal/agent/runtime/external"
	"github.com/felinics/memoh/internal/agent/runtime/toolmount"
	"github.com/felinics/memoh/internal/agent/sessionmode"
	"github.com/felinics/memoh/internal/mcp"
	"github.com/felinics/memoh/internal/runtimefence"
	"github.com/felinics/memoh/internal/workspace/bridge"
)

func (d *Driver) mountTools(ctx context.Context, client *bridge.Client, info bridge.WorkspaceInfo, input external.PromptInput) (*toolmount.Mount, error) {
	base := toolmount.ResolveBaseURL(info, input.ToolHTTPURL)
	if base == "" {
		return nil, errors.New("opencode: missing Memoh tool gateway URL")
	}
	session := turnToolSession(ctx, input)
	return toolmount.Serve(ctx, client, base, d.toolGateway, func() mcp.ToolSessionContext { return session })
}

// turnToolSession builds the trusted tool identity for one turn. Everything
// comes from the prompt input and the turn context, never from HTTP requests.
func turnToolSession(ctx context.Context, input external.PromptInput) mcp.ToolSessionContext {
	session := mcp.ToolSessionContext{
		BotID:                     input.BotID,
		ChatID:                    firstNonEmpty(input.ChatID, input.BotID),
		SessionID:                 input.ThreadID,
		RunID:                     input.RunID,
		SessionType:               firstNonEmpty(input.SessionMode, sessionmode.Chat),
		RouteID:                   input.RouteID,
		CurrentPlatform:           input.CurrentPlatform,
		ReplyTarget:               input.ReplyTarget,
		ConversationType:          input.ConversationType,
		ChannelIdentityID:         input.ChannelIdentityID,
		SessionToken:              input.SessionToken,
		CanRequestUserInput:       input.CanRequestUserInput,
		CanListUserInput:          true,
		RuntimeActive:             true,
		RequireActiveRun:          true,
		SupportsImageInput:        true,
		ContextBudgetMaxTokens:    input.ContextBudgetMaxTokens,
		ContextToolExchangePolicy: input.ContextToolExchangePolicy,
	}
	if fence, ok := runtimefence.FromContext(ctx); ok {
		session.RuntimeFence = fence
	}
	session.RunContext = ctx
	return session
}
