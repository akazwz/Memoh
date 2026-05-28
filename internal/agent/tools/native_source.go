package tools

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	sdk "github.com/memohai/twilight-ai/sdk"

	"github.com/memohai/memoh/internal/mcp"
	messageevent "github.com/memohai/memoh/internal/message/event"
	"github.com/memohai/memoh/internal/toolapproval"
)

const nativeToolApprovalWaitTimeout = 10 * time.Minute

type NativeToolSourceOptions struct {
	AllowAll          bool
	AllowTools        map[string]bool
	Approval          NativeToolApprovalService
	ApprovalPublisher messageevent.Publisher
}

type NativeToolApprovalService interface {
	EvaluatePolicy(ctx context.Context, input toolapproval.CreatePendingInput) (toolapproval.Evaluation, error)
	CreatePending(ctx context.Context, input toolapproval.CreatePendingInput) (toolapproval.Request, error)
	Reject(ctx context.Context, approvalID, actorID, reason string) (toolapproval.Request, error)
	WaitForDecision(ctx context.Context, approvalID string) (toolapproval.Request, error)
}

// NativeToolSource exposes Memoh-native ToolProvider tools through the MCP
// ToolSource interface used by ACP and external tool gateways.
type NativeToolSource struct {
	logger    *slog.Logger
	mu        sync.RWMutex
	providers []ToolProvider
	allowAll  bool
	allow     map[string]struct{}
	approval  NativeToolApprovalService
	publisher messageevent.Publisher
}

func NewNativeToolSource(log *slog.Logger, providers []ToolProvider, opts NativeToolSourceOptions) *NativeToolSource {
	if log == nil {
		log = slog.Default()
	}
	allow := map[string]struct{}{}
	for name, enabled := range opts.AllowTools {
		if !enabled {
			continue
		}
		if normalized := strings.TrimSpace(name); normalized != "" {
			allow[normalized] = struct{}{}
		}
	}
	source := &NativeToolSource{
		logger:    log.With(slog.String("tool_source", "native")),
		allowAll:  opts.AllowAll,
		allow:     allow,
		approval:  opts.Approval,
		publisher: opts.ApprovalPublisher,
	}
	source.SetProviders(providers)
	return source
}

func (s *NativeToolSource) SetProviders(providers []ToolProvider) {
	if s == nil {
		return
	}
	filtered := make([]ToolProvider, 0, len(providers))
	for _, provider := range providers {
		if provider != nil {
			filtered = append(filtered, provider)
		}
	}
	s.mu.Lock()
	s.providers = filtered
	s.mu.Unlock()
}

func (s *NativeToolSource) ListTools(ctx context.Context, session mcp.ToolSessionContext) ([]mcp.ToolDescriptor, error) {
	tools := s.loadTools(ctx, session)
	if len(tools) == 0 {
		return []mcp.ToolDescriptor{}, nil
	}
	seen := map[string]struct{}{}
	descriptors := make([]mcp.ToolDescriptor, 0, len(tools))
	for _, tool := range tools {
		name := strings.TrimSpace(tool.Name)
		if name == "" || tool.Execute == nil || !s.allowed(name) {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		descriptors = append(descriptors, mcp.ToolDescriptor{
			Name:        name,
			Description: strings.TrimSpace(tool.Description),
			InputSchema: toolInputSchema(tool.Parameters),
		})
	}
	return descriptors, nil
}

func (s *NativeToolSource) CallTool(ctx context.Context, session mcp.ToolSessionContext, toolName string, arguments map[string]any) (map[string]any, error) {
	toolName = strings.TrimSpace(toolName)
	if toolName == "" || !s.allowed(toolName) {
		return nil, mcp.ErrToolNotFound
	}
	tools := s.loadTools(ctx, session)
	for _, tool := range tools {
		if strings.TrimSpace(tool.Name) != toolName || tool.Execute == nil {
			continue
		}
		if arguments == nil {
			arguments = map[string]any{}
		}
		approval, err := s.requireApproval(ctx, session, toolName, arguments)
		if err != nil {
			return nil, err
		}
		if !approval.approved {
			return mcp.BuildToolErrorResult(approval.message), nil
		}
		result, err := tool.Execute(&sdk.ToolExecContext{
			Context:  ctx,
			ToolName: toolName,
		}, arguments)
		if err != nil {
			return nil, err
		}
		return mcp.BuildToolSuccessResult(result), nil
	}
	return nil, mcp.ErrToolNotFound
}

type nativeApprovalResult struct {
	approved bool
	message  string
}

func (s *NativeToolSource) requireApproval(ctx context.Context, session mcp.ToolSessionContext, toolName string, arguments map[string]any) (nativeApprovalResult, error) {
	if s == nil || s.approval == nil {
		return nativeApprovalResult{approved: true}, nil
	}
	input := toolapproval.CreatePendingInput{
		BotID:                        session.BotID,
		SessionID:                    session.SessionID,
		RouteID:                      session.RouteID,
		ChannelIdentityID:            session.ChannelIdentityID,
		RequestedByChannelIdentityID: session.ChannelIdentityID,
		ToolCallID:                   "mcp-" + uuid.NewString(),
		ToolName:                     toolName,
		ToolInput:                    arguments,
		SourcePlatform:               session.CurrentPlatform,
		ReplyTarget:                  session.ReplyTarget,
		ConversationType:             session.ConversationType,
	}
	eval, err := s.approval.EvaluatePolicy(ctx, input)
	if err != nil {
		return nativeApprovalResult{}, err
	}
	if eval.Decision == toolapproval.DecisionBypass {
		return nativeApprovalResult{approved: true}, nil
	}

	req, err := s.approval.CreatePending(ctx, input)
	if err != nil {
		return nativeApprovalResult{}, err
	}
	if strings.TrimSpace(session.StreamID) == "" {
		reason := "tool execution requires approval, but this ACP tool call is not attached to an interactive stream"
		rejected, rejectErr := s.approval.Reject(ctx, req.ID, session.ChannelIdentityID, reason)
		if rejectErr != nil {
			return nativeApprovalResult{}, rejectErr
		}
		return nativeApprovalResult{message: rejectedToolApprovalText(rejected.DecisionReason)}, nil
	}

	s.publishToolApprovalRequest(session, req)
	waitCtx, cancel := context.WithTimeout(ctx, nativeToolApprovalWaitTimeout)
	defer cancel()
	decided, err := s.approval.WaitForDecision(waitCtx, req.ID)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			rejectCtx, rejectCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer rejectCancel()
			rejected, rejectErr := s.approval.Reject(rejectCtx, req.ID, session.ChannelIdentityID, "tool approval timed out")
			if rejectErr != nil {
				return nativeApprovalResult{}, rejectErr
			}
			return nativeApprovalResult{message: rejectedToolApprovalText(rejected.DecisionReason)}, nil
		}
		return nativeApprovalResult{}, err
	}
	switch strings.ToLower(strings.TrimSpace(decided.Status)) {
	case toolapproval.StatusApproved:
		return nativeApprovalResult{approved: true}, nil
	case toolapproval.StatusRejected:
		return nativeApprovalResult{message: rejectedToolApprovalText(decided.DecisionReason)}, nil
	default:
		msg := "tool execution was not approved"
		if status := strings.TrimSpace(decided.Status); status != "" {
			msg += ": " + status
		}
		return nativeApprovalResult{message: msg}, nil
	}
}

func (s *NativeToolSource) publishToolApprovalRequest(session mcp.ToolSessionContext, req toolapproval.Request) {
	if s == nil || s.publisher == nil {
		return
	}
	streamID := strings.TrimSpace(session.StreamID)
	sessionID := strings.TrimSpace(session.SessionID)
	botID := strings.TrimSpace(session.BotID)
	if streamID == "" || sessionID == "" || botID == "" {
		return
	}

	running := false
	messageID := 1000000 + req.ShortID
	message := map[string]any{
		"id":           messageID,
		"type":         "tool",
		"name":         req.ToolName,
		"input":        req.ToolInput,
		"tool_call_id": req.ToolCallID,
		"running":      &running,
		"approval": map[string]any{
			"approval_id": req.ID,
			"short_id":    req.ShortID,
			"status":      toolapproval.StatusPending,
			"can_approve": true,
		},
	}
	s.publishAgentStream(botID, sessionID, map[string]any{
		"type":       "start",
		"stream_id":  streamID,
		"session_id": sessionID,
	})
	s.publishAgentStream(botID, sessionID, map[string]any{
		"type":       "message",
		"stream_id":  streamID,
		"session_id": sessionID,
		"data":       message,
	})
	s.publishAgentStream(botID, sessionID, map[string]any{
		"type":       "end",
		"stream_id":  streamID,
		"session_id": sessionID,
	})
}

func (s *NativeToolSource) publishAgentStream(botID, sessionID string, stream map[string]any) {
	payload := map[string]any{
		"session_id": sessionID,
		"stream":     stream,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	s.publisher.Publish(messageevent.Event{
		Type:  messageevent.EventTypeAgentStream,
		BotID: botID,
		Data:  data,
	})
}

func rejectedToolApprovalText(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return "tool execution rejected by user"
	}
	return "tool execution rejected by user: " + reason
}

func (s *NativeToolSource) loadTools(ctx context.Context, session mcp.ToolSessionContext) []sdk.Tool {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	providers := append([]ToolProvider(nil), s.providers...)
	s.mu.RUnlock()
	toolSession := SessionContext{
		BotID:             session.BotID,
		ChatID:            firstNonEmpty(session.ChatID, session.BotID),
		SessionID:         session.SessionID,
		SessionType:       session.SessionType,
		ChannelIdentityID: session.ChannelIdentityID,
		SessionToken:      session.SessionToken,
		CurrentPlatform:   session.CurrentPlatform,
		ReplyTarget:       session.ReplyTarget,
		ConversationType:  session.ConversationType,
		IsSubagent:        session.IsSubagent,
	}
	var out []sdk.Tool
	for _, provider := range providers {
		providerTools, err := provider.Tools(ctx, toolSession)
		if err != nil {
			s.logger.Warn("native tool provider failed", slog.Any("error", err))
			continue
		}
		out = append(out, providerTools...)
	}
	return out
}

func (s *NativeToolSource) allowed(name string) bool {
	if s == nil {
		return false
	}
	if s.allowAll {
		return strings.TrimSpace(name) != ""
	}
	if len(s.allow) == 0 {
		return false
	}
	_, ok := s.allow[strings.TrimSpace(name)]
	return ok
}

func toolInputSchema(parameters any) map[string]any {
	if parameters == nil {
		return emptyObjectSchema()
	}
	if schema, ok := parameters.(map[string]any); ok && schema != nil {
		return schema
	}
	raw, err := json.Marshal(parameters)
	if err != nil {
		return emptyObjectSchema()
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil || schema == nil {
		return emptyObjectSchema()
	}
	if strings.TrimSpace(StringArg(schema, "type")) == "" {
		schema["type"] = "object"
	}
	return schema
}
