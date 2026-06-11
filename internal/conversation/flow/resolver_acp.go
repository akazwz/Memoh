package flow

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	sdk "github.com/memohai/twilight-ai/sdk"

	"github.com/memohai/memoh/internal/acpagent"
	"github.com/memohai/memoh/internal/acpclient"
	agentpkg "github.com/memohai/memoh/internal/agent"
	"github.com/memohai/memoh/internal/agent/background"
	"github.com/memohai/memoh/internal/conversation"
	"github.com/memohai/memoh/internal/session"
	"github.com/memohai/memoh/internal/toolapproval"
	"github.com/memohai/memoh/internal/userinput"
)

type acpPrompter interface {
	Prompt(ctx context.Context, input acpagent.PromptInput) (acpclient.PromptResult, error)
	// AbortTurn cancels the session's in-flight turn out-of-band (any client,
	// any connection — not just the one that started the turn).
	AbortTurn(botID, sessionID, turnID string) (acpagent.RuntimeStatus, error)
}

func (r *Resolver) SetACPSessionPool(pool acpPrompter) {
	r.acpPool = pool
}

func (r *Resolver) isACPAgentSession(ctx context.Context, sessionID string) (bool, error) {
	sessionID = strings.TrimSpace(sessionID)
	if r == nil || r.sessionService == nil || sessionID == "" {
		return false, nil
	}
	sess, err := r.sessionService.Get(ctx, sessionID)
	if err != nil {
		return false, err
	}
	return sess.Type == session.TypeACPAgent, nil
}

// backgroundNotificationStreamPrefix marks the synthetic stream ID of a
// background-notification delivery turn (vs a user-initiated turn).
const backgroundNotificationStreamPrefix = "bgnotif-"

func (r *Resolver) streamACPAgentWS(ctx context.Context, req conversation.ChatRequest, eventCh chan<- WSStreamEvent, abortCh <-chan struct{}) error {
	if r.acpPool == nil {
		return errors.New("ACP session pool is not configured")
	}
	sess, err := r.sessionService.Get(ctx, req.SessionID)
	if err != nil {
		return err
	}
	agentID := metadataString(sess.Metadata, "acp_agent_id")
	projectPath := metadataString(sess.Metadata, "project_path")
	contextMarkdown := r.buildACPContextMarkdown(ctx, req, agentID, projectPath)
	timezoneName, _ := r.resolveTimezone(ctx, req.BotID, req.UserID)

	doneTurn := r.enterSessionTurn(ctx, req.BotID, req.SessionID)
	defer doneTurn()

	if req.RawQuery == "" {
		req.RawQuery = strings.TrimSpace(req.Query)
	}
	req.Query = strings.TrimSpace(req.Query)
	// Background-notification turns must not seed the session title from the
	// notification text (the native background path doesn't generate titles
	// either) — only user-initiated turns do.
	if !strings.HasPrefix(req.StreamID, backgroundNotificationStreamPrefix) {
		go r.maybeGenerateSessionTitle(context.WithoutCancel(ctx), req, req.Query)
	}

	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-abortCh:
			cancel()
		case <-streamCtx.Done():
		}
	}()

	// turnStream mirrors the turn onto the bot event hub and keeps the
	// reconnect snapshot. It is fed unconditionally — before the eventCh
	// select — so a headless turn (this WS gone, streamCtx cancelled) stays
	// observable end to end.
	turnStream := r.beginACPTurnStream(req.BotID, req.SessionID)
	defer turnStream.finish("end")

	emit := func(event agentpkg.StreamEvent) {
		turnStream.handleEvent(event)
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
	// No eager text_start here: the UI message converter allocates block IDs
	// in arrival order and the frontend sorts by ID, so pre-creating the text
	// block would pin the answer text above any reasoning that streams first.
	// The first text_delta lazily creates the text block instead.

	result, err := r.acpPool.Prompt(streamCtx, acpagent.PromptInput{
		BotID:             req.BotID,
		ChatID:            req.ChatID,
		SessionID:         req.SessionID,
		StreamID:          req.StreamID,
		RouteID:           req.RouteID,
		AgentID:           agentID,
		ProjectPath:       projectPath,
		Prompt:            req.Query,
		ChannelIdentityID: req.SourceChannelIdentityID,
		SessionToken:      req.Token,
		CurrentPlatform:   req.CurrentChannel,
		ReplyTarget:       req.ReplyTarget,
		ConversationType:  req.ConversationType,
		Timezone:          timezoneName,
		ToolHTTPURL:       req.ToolHTTPURL,
		ContextURI:        acpContextURI,
		ContextMarkdown:   contextMarkdown,
		HistoryResources: func() ([]acpclient.PromptResource, error) {
			return r.buildACPHistoryResources(streamCtx, req)
		},
		OnTurnStart: turnStream.turnStarted,
		Sink: acpclient.EventSinkFunc(func(event acpclient.StreamEvent) {
			if mapped, ok := MapACPStreamEvent(event); ok {
				emit(mapped)
			}
		}),
	})
	if err != nil {
		if errors.Is(err, acpagent.ErrSessionBusy) {
			// The previous turn is still running (often started by a
			// connection that is gone). The prompt was never delivered, so no
			// round is persisted. Busy propagates to the caller — the WS
			// layer renders it as an in-stream error, the background
			// notification path requeues — instead of being swallowed here.
			// This stream's turn was never granted, so finish announces
			// nothing and cannot clobber the running turn's snapshot.
			turnStream.finish("busy")
			return err
		}
		if errors.Is(err, acpagent.ErrPromptAborted) || streamCtx.Err() != nil {
			// The turn was aborted — in-band (this stream's abort) or
			// out-of-band (AbortTurn from another client). The pool kept the
			// runtime warm and session/cancel went out; persist the partial
			// round as a cancelled turn — no failure text, no error metadata —
			// mirroring the native pipeline's partial snapshot on abort.
			if strings.TrimSpace(result.StopReason) == "" {
				result.StopReason = "cancelled"
			}
			r.cancelPendingACPApprovals(context.WithoutCancel(ctx), req, "tool approval cancelled: the turn was aborted before a decision arrived")
			if persistErr := r.persistACPRound(context.WithoutCancel(ctx), req, agentID, projectPath, result, errACPTurnCancelled); persistErr != nil {
				r.logger.Error("ACP cancelled persist failed", slog.Any("error", persistErr), slog.String("session_id", req.SessionID))
			}
			emit(agentpkg.StreamEvent{Type: agentpkg.EventTextEnd})
			emit(agentpkg.StreamEvent{Type: agentpkg.EventAgentAbort})
			turnStream.finish("cancelled")
			return nil
		}
		r.logger.Error("ACP prompt failed",
			slog.String("bot_id", req.BotID),
			slog.String("session_id", req.SessionID),
			slog.Any("error", err),
		)
		r.cancelPendingACPApprovals(context.WithoutCancel(ctx), req, "tool approval cancelled: the turn failed before a decision arrived")
		failedResult, failureDelta := acpFailureResult(result, err)
		if failureDelta != "" {
			emit(agentpkg.StreamEvent{Type: agentpkg.EventTextDelta, Delta: failureDelta})
		}
		_ = r.persistACPRound(context.WithoutCancel(ctx), req, agentID, projectPath, failedResult, err)
		emit(agentpkg.StreamEvent{Type: agentpkg.EventTextEnd})
		emit(agentpkg.StreamEvent{Type: agentpkg.EventAgentAbort})
		turnStream.finish("error")
		return nil
	}

	emit(agentpkg.StreamEvent{Type: agentpkg.EventTextEnd})
	if err := r.persistACPRound(context.WithoutCancel(ctx), req, agentID, projectPath, result, nil); err != nil {
		r.logger.Error("ACP persist failed", slog.Any("error", err), slog.String("session_id", req.SessionID))
	}
	emit(agentpkg.StreamEvent{Type: agentpkg.EventAgentEnd})
	return nil
}

// MapACPStreamEvent converts an ACP event mechanically: every field crosses
// over, and the Type needs no translation because acpclient and the agent
// share the streamevent vocabulary. Per-type field cherry-picking is what
// previously let the two pipelines drift, so a new field only needs to exist
// on both structs to propagate. Exported so the native/ACP parity suite can
// drive the real conversion.
func MapACPStreamEvent(event acpclient.StreamEvent) (agentpkg.StreamEvent, bool) {
	if event.Delta == "" &&
		(event.Type == acpclient.StreamEventTextDelta || event.Type == acpclient.StreamEventReasoningDelta) {
		return agentpkg.StreamEvent{}, false
	}
	return agentpkg.StreamEvent{
		Type:        event.Type,
		Delta:       event.Delta,
		ToolName:    event.ToolName,
		ToolCallID:  event.ToolCallID,
		Input:       event.Input,
		Result:      event.Result,
		Error:       event.Error,
		ApprovalID:  event.ApprovalID,
		UserInputID: event.UserInputID,
		ShortID:     event.ShortID,
		Status:      event.Status,
		Metadata:    event.Metadata,
		Attachments: event.Attachments,
		Reactions:   event.Reactions,
		Speeches:    event.Speeches,
	}, true
}

// errACPTurnCancelled marks a persistACPRound call as a user abort: the round
// is stored as an ordinary partial turn (stop_reason cancelled, no error
// metadata) but, like failures, skips memory extraction — the turn never
// completed.
var errACPTurnCancelled = errors.New("ACP turn cancelled by user")

// deliverACPBackgroundNotifications sends queued background-task
// notifications to an ACP session as a real prompt through the full turn
// pipeline — persistence, turn mirror, history replay, approvals all behave
// exactly as for a user message. The native pipeline injects notifications at
// agent step boundaries; for an external agent the equivalent boundary is a
// fresh turn. Returns ErrSessionBusy (notifications untouched by the caller's
// requeue) when a turn slipped in concurrently.
func (r *Resolver) deliverACPBackgroundNotifications(ctx context.Context, botID, sessionID string, notifications []background.Notification) error {
	if r == nil || r.acpPool == nil {
		return errors.New("ACP session pool is not configured")
	}
	var sb strings.Builder
	for _, notification := range notifications {
		text := strings.TrimSpace(notification.MessageText())
		if text == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(text)
	}
	query := strings.TrimSpace(sb.String())
	if query == "" {
		return nil
	}

	req := conversation.ChatRequest{
		BotID:     botID,
		SessionID: sessionID,
		Query:     query,
		// A synthetic stream ID keeps tool approvals interactive during the
		// notification turn: the cards reach users through the turn mirror
		// and the persisted round, exactly like a user-initiated turn. The
		// prefix also marks this as a background turn (no title generation).
		StreamID: backgroundNotificationStreamPrefix + uuid.NewString(),
	}
	// Best-effort delivery context (route / channel / reply target) so tools
	// invoked during the turn address the same conversation the native
	// pipeline would.
	if delivery, err := r.resolveBackgroundDeliveryContext(ctx, botID, sessionID); err == nil {
		req.RouteID = delivery.routeID
		req.CurrentChannel = delivery.channelType
		req.ReplyTarget = delivery.replyTarget
	}

	// Nobody subscribes to the WS channel of this headless delivery; the turn
	// mirror (acp_turn_stream) and the persisted round are the user-facing
	// surfaces. Drain the channel so emits never block.
	eventCh := make(chan WSStreamEvent, 64)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for range eventCh {
		}
	}()
	defer func() {
		close(eventCh)
		<-drained
	}()
	return r.streamACPAgentWS(ctx, req, eventCh, make(chan struct{}))
}

// cancelPendingACPApprovals closes the residual approval window when a turn
// dies abnormally: any pending row for the session belonged to that turn (the
// pool's turn slot guarantees one turn per session), and its waiter is gone —
// left pending, the persisted card would stay actionable forever and a late
// approve would flip a row nobody executes.
func (r *Resolver) cancelPendingACPApprovals(ctx context.Context, req conversation.ChatRequest, reason string) {
	if r == nil || r.toolApproval == nil {
		return
	}
	cancelled, err := r.toolApproval.CancelPendingForSession(ctx, req.BotID, req.SessionID, reason)
	if err != nil {
		r.logger.Warn("cancel pending ACP approvals failed",
			slog.String("bot_id", req.BotID),
			slog.String("session_id", req.SessionID),
			slog.Any("error", err))
		return
	}
	if len(cancelled) > 0 {
		r.logger.Info("cancelled pending ACP approvals with their turn",
			slog.String("session_id", req.SessionID),
			slog.Int("count", len(cancelled)))
	}
}

func (r *Resolver) persistACPRound(ctx context.Context, req conversation.ChatRequest, agentID, projectPath string, result acpclient.PromptResult, promptErr error) error {
	meta := map[string]any{
		"acp_agent_id": agentID,
		"project_path": projectPath,
		"stop_reason":  result.StopReason,
	}
	if promptErr != nil && !errors.Is(promptErr, errACPTurnCancelled) {
		meta["error"] = promptErr.Error()
	}
	output := acpResultOutputMessages(result)
	round := make([]conversation.ModelMessage, 0, 1+len(output))
	round = append(round, conversation.ModelMessage{Role: "user", Content: conversation.NewTextContent(req.Query)})
	round = append(round, output...)

	return r.storeRoundWithOptions(ctx, req, round, "", storeRoundOptions{
		SkipMemory:              promptErr != nil,
		AllowEmptyAssistantText: true,
		// Role-keyed, not index-keyed: storeRoundWithOptions mutates the round
		// (tool-closure repair inserts, dedupe/filtering removes) before
		// persisting, so positional metadata would attach to the wrong rows.
		AssistantMessageMetadata: meta,
	})
}

func acpResultOutputMessages(result acpclient.PromptResult) []conversation.ModelMessage {
	output := make([]conversation.ModelMessage, 0)
	assistantParts := make([]sdk.MessagePart, 0)
	var reasoning strings.Builder
	var text strings.Builder
	sawTextDelta := false

	flushReasoning := func() {
		if reasoning.Len() == 0 {
			return
		}
		assistantParts = append(assistantParts, sdk.ReasoningPart{Text: reasoning.String()})
		reasoning.Reset()
	}
	flushText := func() {
		if text.Len() == 0 {
			return
		}
		assistantParts = append(assistantParts, sdk.TextPart{Text: text.String()})
		text.Reset()
	}
	assistantHasToolCall := func() bool {
		for _, part := range assistantParts {
			if _, ok := part.(sdk.ToolCallPart); ok {
				return true
			}
		}
		return false
	}
	flushAssistant := func() {
		flushReasoning()
		flushText()
		if len(assistantParts) == 0 {
			return
		}
		converted := sdkMessagesToModelMessages([]sdk.Message{{
			Role:    sdk.MessageRoleAssistant,
			Content: assistantParts,
		}})
		output = append(output, converted...)
		assistantParts = assistantParts[:0]
	}
	appendText := func(delta string) {
		if delta == "" {
			return
		}
		if assistantHasToolCall() {
			flushAssistant()
		}
		sawTextDelta = true
		text.WriteString(delta)
	}
	appendReasoning := func(delta string) {
		if delta == "" {
			return
		}
		if text.Len() > 0 || assistantHasToolCall() {
			flushAssistant()
		}
		reasoning.WriteString(delta)
	}
	appendToolResult := func(event acpclient.StreamEvent) {
		result := event.Result
		isError := strings.TrimSpace(event.Error) != ""
		if result == nil && isError {
			result = strings.TrimSpace(event.Error)
		}
		converted := sdkMessagesToModelMessages([]sdk.Message{
			sdk.ToolMessage(sdk.ToolResultPart{
				ToolCallID: strings.TrimSpace(event.ToolCallID),
				ToolName:   strings.TrimSpace(event.ToolName),
				Result:     result,
				IsError:    isError,
			}),
		})
		output = append(output, converted...)
	}
	// findToolCallPart matches by ID when the event has one, by name otherwise.
	// Approval events can arrive before the matching tool_call_start, so both
	// attachToolMetadata and upsertToolCallStart merge into the same part.
	findToolCallPart := func(toolCallID, toolName string) int {
		for idx, part := range assistantParts {
			toolCall, ok := part.(sdk.ToolCallPart)
			if !ok {
				continue
			}
			if toolCallID != "" {
				if strings.TrimSpace(toolCall.ToolCallID) == toolCallID {
					return idx
				}
				continue
			}
			if toolName != "" && strings.TrimSpace(toolCall.ToolName) == toolName {
				return idx
			}
		}
		return -1
	}
	attachToolMetadata := func(event acpclient.StreamEvent, key string, value map[string]any) {
		toolCallID := strings.TrimSpace(event.ToolCallID)
		toolName := strings.TrimSpace(event.ToolName)
		if idx := findToolCallPart(toolCallID, toolName); idx >= 0 {
			toolCall := assistantParts[idx].(sdk.ToolCallPart)
			if toolCall.ProviderMetadata == nil {
				toolCall.ProviderMetadata = map[string]any{}
			}
			toolCall.ProviderMetadata[key] = value
			if event.Input != nil {
				toolCall.Input = event.Input
			}
			assistantParts[idx] = toolCall
			return
		}
		// Fold pending reasoning/text into parts of the SAME assistant
		// message (exactly like upsertToolCallStart below) instead of closing
		// the message: an approval that precedes its tool_call_start must not
		// split the round differently than the native pipeline would.
		flushReasoning()
		flushText()
		assistantParts = append(assistantParts, sdk.ToolCallPart{
			ToolCallID: toolCallID,
			ToolName:   toolName,
			Input:      event.Input,
			ProviderMetadata: map[string]any{
				key: value,
			},
		})
	}
	upsertToolCallStart := func(event acpclient.StreamEvent) {
		flushReasoning()
		flushText()
		toolCallID := strings.TrimSpace(event.ToolCallID)
		toolName := strings.TrimSpace(event.ToolName)
		if idx := findToolCallPart(toolCallID, toolName); idx >= 0 {
			toolCall := assistantParts[idx].(sdk.ToolCallPart)
			if toolCallID != "" {
				toolCall.ToolCallID = toolCallID
			}
			if toolName != "" {
				toolCall.ToolName = toolName
			}
			if event.Input != nil {
				toolCall.Input = event.Input
			}
			assistantParts[idx] = toolCall
			return
		}
		assistantParts = append(assistantParts, sdk.ToolCallPart{
			ToolCallID: toolCallID,
			ToolName:   toolName,
			Input:      event.Input,
		})
	}
	userInputMetadata := func(event acpclient.StreamEvent) map[string]any {
		status := strings.TrimSpace(event.Status)
		if status == "" {
			status = userinput.StatusPending
		}
		userInputID := strings.TrimSpace(event.UserInputID)
		if userInputID == "" {
			if value, _ := event.Metadata["user_input_id"].(string); value != "" {
				userInputID = strings.TrimSpace(value)
			}
		}
		return map[string]any{
			"user_input_id": userInputID,
			"short_id":      event.ShortID,
			"status":        status,
			"ui_payload":    event.Metadata["ui_payload"],
		}
	}
	approvalMetadata := func(event acpclient.StreamEvent) map[string]any {
		approvalID := strings.TrimSpace(event.ApprovalID)
		if approvalID == "" {
			if value, _ := event.Metadata["approval_id"].(string); value != "" {
				approvalID = strings.TrimSpace(value)
			}
		}
		return toolapproval.RequestMetadata(toolapproval.Request{
			ID:      approvalID,
			ShortID: event.ShortID,
			Status:  event.Status,
		})
	}
	for _, event := range result.Events {
		switch event.Type {
		case acpclient.StreamEventReasoningDelta:
			appendReasoning(event.Delta)
		case acpclient.StreamEventTextDelta:
			appendText(event.Delta)
		case acpclient.StreamEventToolCallStart:
			upsertToolCallStart(event)
		case acpclient.StreamEventToolCallEnd:
			flushAssistant()
			appendToolResult(event)
		case acpclient.StreamEventToolApprovalRequest:
			attachToolMetadata(event, "approval", approvalMetadata(event))
		case acpclient.StreamEventUserInputRequest:
			attachToolMetadata(event, "user_input", userInputMetadata(event))
		}
	}
	if !sawTextDelta {
		appendText(strings.TrimSpace(result.Text))
	}
	flushAssistant()

	if len(output) == 0 {
		return []conversation.ModelMessage{{Role: "assistant", Content: conversation.NewTextContent("")}}
	}
	return output
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
