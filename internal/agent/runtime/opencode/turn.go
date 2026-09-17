package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	sdk "github.com/felinics/twilight/sdk"

	userinput "github.com/felinics/memoh/internal/agent/decision/input"
	"github.com/felinics/memoh/internal/agent/event"
	"github.com/felinics/memoh/internal/agent/runtime/external"
	"github.com/felinics/memoh/internal/agent/runtime/opencode/opencodecfg"
)

type turnRunner struct {
	input          external.PromptInput
	api            *apiClient
	approval       ApprovalService
	userInput      userinput.FlowService
	logger         *slog.Logger
	sessionID      string
	messageID      string
	parts          map[string]nativePart
	started        map[string]bool
	ended          map[string]bool
	decisions      map[string]bool
	decisionErrors chan error
	decisionCancel context.CancelFunc
	workers        sync.WaitGroup
	mu             sync.Mutex
	closed         bool
	recorder       *external.TranscriptRecorder
	text           string
	usage          sdk.Usage
}

func newTurn(input external.PromptInput, approvals ApprovalService, questions userinput.FlowService, logger *slog.Logger) *turnRunner {
	return &turnRunner{input: input, approval: approvals, userInput: questions, logger: logger, parts: map[string]nativePart{}, started: map[string]bool{}, ended: map[string]bool{}, decisions: map[string]bool{}, decisionErrors: make(chan error, 1), recorder: external.NewTranscriptRecorder(input.ToolOutputLimit)}
}

func (t *turnRunner) emit(ev event.StreamEvent) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	ev = external.LimitStreamEvent(ev, t.input.ToolOutputLimit)
	t.recorder.Add(ev)
	t.input.Sink.EmitStreamEvent(ev)
}

func (t *turnRunner) close() {
	if t.decisionCancel != nil {
		t.decisionCancel()
	}
	t.workers.Wait()
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
}

func (t *turnRunner) result(completed bool) external.PromptResult {
	out := external.PromptResult{Output: t.recorder.Messages(t.text), Text: t.text, TurnCompleted: completed, StopReason: "end_turn", AgentTurnID: t.messageID}
	if !completed {
		out.StopReason = "cancelled"
	}
	if t.sessionID != "" && t.messageID != "" {
		out.RuntimeMetadata = map[string]any{metadataSessionIDKey: t.sessionID, metadataMessageIDKey: t.messageID}
	}
	if t.usage.TotalTokens > 0 {
		out.Usage = &t.usage
	}
	return out
}

func (t *turnRunner) run(ctx context.Context, cfg opencodecfg.Config) error {
	if t.input.Command != "" {
		commands, err := nativeCommands(ctx, t.api)
		if err != nil {
			return err
		}
		command, ok := external.FindCommand(commands, t.input.Command)
		if !ok || command.Kind != external.CommandTurn {
			return external.ErrCommandUnavailable
		}
	}
	_, err := t.ensureSession(ctx)
	if err != nil {
		return err
	}
	if err := t.applyPermissions(ctx, cfg); err != nil {
		return err
	}
	t.messageID = nativeMessageID()
	body, err := promptBody(t.input, cfg, t.messageID)
	if err != nil {
		return err
	}
	requestCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	decisionCtx, decisionCancel := context.WithCancel(ctx)
	t.decisionCancel = decisionCancel
	defer decisionCancel()
	subscribeCtx, cancelSubscribe := context.WithTimeout(ctx, 30*time.Second)
	// The subscription lifetime is the run, not the connect timeout.
	stopConnect := context.AfterFunc(subscribeCtx, cancel)
	events, streamErrors, err := t.api.subscribe(requestCtx)
	stopConnect()
	cancelSubscribe()
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() {
		var response nativeMessage
		action := "/message"
		if t.input.Command != "" {
			action = "/command"
		}
		done <- t.api.call(requestCtx, http.MethodPost, "/session/"+url.PathEscape(t.sessionID)+action, body, &response)
	}()
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return errors.New("opencode event stream closed during a turn")
			}
			if err := t.handleEvent(decisionCtx, ev); err != nil {
				return err
			}
		case err := <-streamErrors:
			return fmt.Errorf("opencode event stream: %w", err)
		case err := <-t.decisionErrors:
			return err
		case err := <-done:
			if err != nil {
				return err
			}
			// The HTTP completion can race buffered SSE frames. Fold them
			// before reconciling with native persisted messages.
			for {
				select {
				case ev, ok := <-events:
					if !ok {
						return errors.New("opencode event stream closed")
					}
					if err := t.handleEvent(decisionCtx, ev); err != nil {
						return err
					}
				default:
					return t.reconcile(ctx)
				}
			}
		case <-ctx.Done():
			decisionCancel()
			abortCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer stop()
			_ = t.api.call(abortCtx, http.MethodPost, "/session/"+url.PathEscape(t.sessionID)+"/abort", nil, nil)
			select {
			case <-done:
			case <-abortCtx.Done():
			}
			_ = t.reconcile(abortCtx)
			return ctx.Err()
		}
	}
}

func (t *turnRunner) reconcile(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var messages []nativeMessage
	if err := t.api.call(ctx, http.MethodGet, "/session/"+url.PathEscape(t.sessionID)+"/message", nil, &messages); err != nil {
		return err
	}
	var runErr error
	t.usage = sdk.Usage{}
	for _, msg := range messages {
		if msg.Info.Role != "assistant" || msg.Info.ParentID != t.messageID {
			continue
		}
		var text strings.Builder
		for _, part := range msg.Parts {
			t.updatePart(part)
			if part.Type == "text" {
				text.WriteString(part.Text)
			}
		}
		if text.Len() > 0 {
			t.text = text.String()
		}
		tokens := msg.Info.Tokens
		t.usage.InputTokens += tokens.Input + tokens.Cache.Read + tokens.Cache.Write
		t.usage.OutputTokens += tokens.Output
		t.usage.ReasoningTokens += tokens.Reasoning
		t.usage.CachedInputTokens += tokens.Cache.Read
		t.usage.InputTokenDetails.CacheReadTokens += tokens.Cache.Read
		t.usage.InputTokenDetails.CacheWriteTokens += tokens.Cache.Write
		if msg.Info.Error != nil && msg.Info.Error.Name != "MessageAbortedError" {
			runErr = errors.New("opencode model execution failed")
		}
	}
	t.usage.TotalTokens = t.usage.InputTokens + t.usage.OutputTokens
	return runErr
}

func (t *turnRunner) handleEvent(ctx context.Context, ev nativeEvent) error {
	switch ev.Type {
	case "message.part.updated":
		var props struct {
			Part nativePart `json:"part"`
		}
		if err := json.Unmarshal(ev.Properties, &props); err != nil {
			return err
		}
		t.updatePart(props.Part)
	case "message.part.delta":
		var props struct {
			SessionID string `json:"sessionID"`
			MessageID string `json:"messageID"`
			PartID    string `json:"partID"`
			Field     string `json:"field"`
			Delta     string `json:"delta"`
		}
		if err := json.Unmarshal(ev.Properties, &props); err != nil {
			return err
		}
		if props.SessionID != t.sessionID || props.MessageID == t.messageID || props.Field != "text" {
			return nil
		}
		part, ok := t.parts[props.PartID]
		if ok {
			part.Text += props.Delta
			t.updatePart(part)
		}
	case "permission.asked", "question.asked":
		var identity struct {
			ID        string `json:"id"`
			SessionID string `json:"sessionID"`
		}
		if err := json.Unmarshal(ev.Properties, &identity); err != nil {
			return err
		}
		if identity.ID == "" || t.decisions[identity.ID] {
			return nil
		}
		belongs, err := t.ownsSession(ctx, identity.SessionID)
		if err != nil {
			return err
		}
		if !belongs {
			return nil
		}
		t.decisions[identity.ID] = true
		t.workers.Add(1)
		go func() {
			defer t.workers.Done()
			var err error
			if ev.Type == "permission.asked" {
				err = t.permission(ctx, ev.Properties)
			} else {
				err = t.question(ctx, ev.Properties)
			}
			if err != nil && ctx.Err() == nil {
				select {
				case t.decisionErrors <- err:
				default:
				}
			}
		}()
	}
	return nil
}

func (t *turnRunner) updatePart(part nativePart) {
	if part.SessionID != t.sessionID || part.MessageID == t.messageID || part.ID == "" {
		return
	}
	previous := t.parts[part.ID]
	t.parts[part.ID] = part
	switch part.Type {
	case "text", "reasoning":
		if !strings.HasPrefix(part.Text, previous.Text) {
			return
		}
		delta := strings.TrimPrefix(part.Text, previous.Text)
		if delta == "" {
			return
		}
		kind := event.TextDelta
		if part.Type == "reasoning" {
			kind = event.ReasoningDelta
		}
		t.emit(event.StreamEvent{Type: kind, Delta: delta})
		if part.Type == "text" {
			t.text += delta
		}
	case "tool":
		// The gateway emits the actual Memoh tool calls. Its OpenCode MCP
		// wrapper would duplicate those calls and approvals in the transcript.
		if strings.HasPrefix(part.Tool, "memoh_") {
			return
		}
		name, input := displayTool(part.Tool, part.State.Input)
		if !t.started[part.CallID] && part.State.Status != "pending" {
			t.started[part.CallID] = true
			t.emit(event.StreamEvent{Type: event.ToolCallStart, ToolCallID: part.CallID, ToolName: name, Input: input})
		}
		if !t.ended[part.CallID] && (part.State.Status == "completed" || part.State.Status == "error") {
			t.ended[part.CallID] = true
			status := "completed"
			if part.State.Status == "error" {
				status = "failed"
			}
			t.emit(event.StreamEvent{Type: event.ToolCallEnd, ToolCallID: part.CallID, ToolName: name, Status: status, Result: firstNonEmpty(part.State.Output, part.State.Error)})
		}
	}
}

func displayTool(name string, args map[string]any) (string, map[string]any) {
	input := make(map[string]any, len(args))
	for k, v := range args {
		input[k] = v
	}
	switch name {
	case "bash":
		name = "exec"
	case "webfetch":
		name = "web_fetch"
	case "websearch":
		name = "web_search"
	}
	for from, to := range map[string]string{"filePath": "path", "oldString": "old_text", "newString": "new_text"} {
		if value, ok := input[from]; ok {
			input[to] = value
			delete(input, from)
		}
	}
	return name, input
}

// Native task agents use child sessions. Only requests in this thread's
// ancestry may reach its decision UI; other persisted sessions are unrelated.
func (t *turnRunner) ownsSession(ctx context.Context, id string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	seen := map[string]bool{}
	for id != "" && !seen[id] {
		if id == t.sessionID {
			return true, nil
		}
		seen[id] = true
		var session struct {
			ParentID string `json:"parentID"`
		}
		if err := t.api.call(ctx, http.MethodGet, "/session/"+url.PathEscape(id), nil, &session); err != nil {
			return false, err
		}
		id = session.ParentID
	}
	return false, nil
}
