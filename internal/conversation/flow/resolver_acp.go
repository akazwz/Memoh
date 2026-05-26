package flow

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"

	"github.com/memohai/memoh/internal/acpagent"
	"github.com/memohai/memoh/internal/acpclient"
	agentpkg "github.com/memohai/memoh/internal/agent"
	"github.com/memohai/memoh/internal/conversation"
	"github.com/memohai/memoh/internal/session"
)

type acpPrompter interface {
	Prompt(ctx context.Context, input acpagent.PromptInput) (acpclient.PromptResult, error)
}

func (r *Resolver) SetACPSessionPool(pool acpPrompter) {
	r.acpPool = pool
}

func (r *Resolver) isACPAgentSession(ctx context.Context, req conversation.ChatRequest) (bool, error) {
	if r == nil || r.sessionService == nil || strings.TrimSpace(req.SessionID) == "" {
		return false, nil
	}
	sess, err := r.sessionService.Get(ctx, req.SessionID)
	if err != nil {
		return false, err
	}
	return sess.Type == session.TypeACPAgent, nil
}

func (r *Resolver) streamACPAgentWS(ctx context.Context, req conversation.ChatRequest, eventCh chan<- WSStreamEvent, abortCh <-chan struct{}) error {
	if r.acpPool == nil {
		return errors.New("ACP session pool is not configured")
	}
	sess, err := r.sessionService.Get(ctx, req.SessionID)
	if err != nil {
		return err
	}
	agentID := metadataString(sess.Metadata, "acp_agent_id")
	if agentID == "" {
		agentID = metadataString(sess.Metadata, "agent_id")
	}
	projectPath := metadataString(sess.Metadata, "project_path")

	doneTurn := r.enterSessionTurn(ctx, req.BotID, req.SessionID)
	defer doneTurn()

	if req.RawQuery == "" {
		req.RawQuery = strings.TrimSpace(req.Query)
	}
	req.Query = strings.TrimSpace(req.Query)
	go r.maybeGenerateSessionTitle(context.WithoutCancel(ctx), req, req.Query)

	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-abortCh:
			cancel()
		case <-streamCtx.Done():
		}
	}()

	emit := func(event agentpkg.StreamEvent) {
		data, err := json.Marshal(event)
		if err != nil {
			return
		}
		select {
		case eventCh <- json.RawMessage(data):
		case <-streamCtx.Done():
		}
	}

	emit(agentpkg.StreamEvent{Type: agentpkg.EventAgentStart})
	emit(agentpkg.StreamEvent{Type: agentpkg.EventTextStart})

	result, err := r.acpPool.Prompt(streamCtx, acpagent.PromptInput{
		BotID:       req.BotID,
		SessionID:   req.SessionID,
		AgentID:     agentID,
		ProjectPath: projectPath,
		Prompt:      req.Query,
		Sink: acpclient.EventSinkFunc(func(event acpclient.StreamEvent) {
			for _, mapped := range mapACPStreamEvent(event) {
				emit(mapped)
			}
		}),
	})
	if err != nil {
		r.logger.Error("ACP prompt failed",
			slog.String("bot_id", req.BotID),
			slog.String("session_id", req.SessionID),
			slog.Any("error", err),
		)
		failedResult, failureDelta := acpFailureResult(result, err)
		if failureDelta != "" {
			emit(agentpkg.StreamEvent{Type: agentpkg.EventTextDelta, Delta: failureDelta})
		}
		_ = r.persistACPRound(context.WithoutCancel(ctx), req, agentID, projectPath, failedResult, err)
		emit(agentpkg.StreamEvent{Type: agentpkg.EventTextEnd})
		emit(agentpkg.StreamEvent{Type: agentpkg.EventAgentAbort})
		return nil
	}

	emit(agentpkg.StreamEvent{Type: agentpkg.EventTextEnd})
	if err := r.persistACPRound(context.WithoutCancel(ctx), req, agentID, projectPath, result, nil); err != nil {
		r.logger.Error("ACP persist failed", slog.Any("error", err), slog.String("session_id", req.SessionID))
	}
	emit(agentpkg.StreamEvent{Type: agentpkg.EventAgentEnd})
	return nil
}

func mapACPStreamEvent(event acpclient.StreamEvent) []agentpkg.StreamEvent {
	switch event.Type {
	case acpclient.StreamEventTextDelta:
		if event.Delta == "" {
			return nil
		}
		return []agentpkg.StreamEvent{{Type: agentpkg.EventTextDelta, Delta: event.Delta}}
	case acpclient.StreamEventToolStart:
		return []agentpkg.StreamEvent{{
			Type:       agentpkg.EventToolCallStart,
			ToolName:   "acp_action",
			ToolCallID: event.Tool.ID,
			Input: map[string]any{
				"title": event.Tool.Title,
				"kind":  event.Tool.Kind,
			},
			Status: event.Tool.Status,
		}}
	case acpclient.StreamEventToolUpdate:
		result := map[string]any{
			"status": event.Tool.Status,
		}
		if event.Tool.Title != "" {
			result["title"] = event.Tool.Title
		}
		if event.Tool.Kind != "" {
			result["kind"] = event.Tool.Kind
		}
		if isTerminalACPToolStatus(event.Tool.Status) {
			mapped := agentpkg.StreamEvent{
				Type:       agentpkg.EventToolCallEnd,
				ToolName:   "acp_action",
				ToolCallID: event.Tool.ID,
				Result:     result,
			}
			if isFailedACPToolStatus(event.Tool.Status) {
				mapped.Error = event.Tool.Status
			}
			return []agentpkg.StreamEvent{mapped}
		}
		return []agentpkg.StreamEvent{{
			Type:       agentpkg.EventToolCallProgress,
			ToolName:   "acp_action",
			ToolCallID: event.Tool.ID,
			Status:     event.Tool.Status,
			Progress: map[string]any{
				"title": event.Tool.Title,
				"kind":  event.Tool.Kind,
			},
		}}
	case acpclient.StreamEventPlan:
		return []agentpkg.StreamEvent{{
			Type:       agentpkg.EventToolCallEnd,
			ToolName:   "acp_plan",
			ToolCallID: "acp_plan",
			Result: map[string]any{
				"plan": event.Plan,
			},
		}}
	default:
		return nil
	}
}

func isTerminalACPToolStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "completed", "complete", "done", "failed", "error", "cancelled", "canceled":
		return true
	default:
		return false
	}
}

func isFailedACPToolStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "failed", "error", "cancelled", "canceled":
		return true
	default:
		return false
	}
}

func (r *Resolver) persistACPRound(ctx context.Context, req conversation.ChatRequest, agentID, projectPath string, result acpclient.PromptResult, promptErr error) error {
	// Persist the assistant text exactly as the agent produced it (which may
	// be empty when the turn only emitted tool actions or a plan). The web UI
	// is responsible for rendering an i18n-aware placeholder based on the
	// presence of acp_actions / acp_plan / error metadata; encoding a
	// hard-coded English placeholder here would defeat that and leak the
	// "Codex" brand into stored history.
	text := strings.TrimSpace(result.Text)
	meta := map[string]any{
		"acp_agent_id": agentID,
		"project_path": projectPath,
		"stop_reason":  result.StopReason,
		"acp_actions":  result.ToolCalls,
		"acp_plan":     result.Plan,
		"acp_events":   result.Events,
	}
	if promptErr != nil {
		meta["error"] = promptErr.Error()
	}
	round := []conversation.ModelMessage{
		{Role: "user", Content: conversation.NewTextContent(req.Query)},
		{Role: "assistant", Content: conversation.NewTextContent(text)},
	}
	return r.storeRoundWithOptions(ctx, req, round, "", storeRoundOptions{
		SkipMemory:              true,
		AllowEmptyAssistantText: true,
		MessageMetadataByIndex: map[int]map[string]any{
			1: meta,
		},
	})
}

// acpFailureResult appends the raw upstream error (truncated, single-line) to
// the partial result so users see what went wrong inline. The frontend is
// responsible for any i18n "ACP agent failed" prefix; the backend only
// surfaces the technical detail.
func acpFailureResult(result acpclient.PromptResult, err error) (acpclient.PromptResult, string) {
	message := truncateOneLineError(err)
	if message == "" {
		return result, ""
	}
	if strings.TrimSpace(result.Text) != "" {
		delta := "\n\n" + message
		result.Text = strings.TrimSpace(result.Text + delta)
		result.Events = append(result.Events, acpclient.StreamEvent{Type: acpclient.StreamEventTextDelta, Delta: delta})
		return result, delta
	}
	result.Text = message
	result.Events = append(result.Events, acpclient.StreamEvent{Type: acpclient.StreamEventTextDelta, Delta: message})
	return result, message
}

func truncateOneLineError(err error) string {
	if err == nil {
		return ""
	}
	message := oneLine(err.Error())
	if message == "" {
		return ""
	}
	const maxRunes = 500
	runes := []rune(message)
	if len(runes) > maxRunes {
		message = string(runes[:maxRunes]) + "..."
	}
	return message
}

func oneLine(value string) string {
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return ""
	}
	return strings.Join(fields, " ")
}

func metadataString(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	value, _ := metadata[key].(string)
	return strings.TrimSpace(value)
}
