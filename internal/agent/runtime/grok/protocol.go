// Package grok runs the official Grok Build CLI through ACP. Native state and
// extensions belong here; the generic ACP runtime has no Grok-specific policy.
package grok

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"

	acp "github.com/coder/acp-go-sdk"

	"github.com/felinics/memoh/internal/agent/event"
	"github.com/felinics/memoh/internal/agent/runtime/external"
)

type modelState struct {
	CurrentModelID  string        `json:"currentModelId"`
	AvailableModels []nativeModel `json:"availableModels"`
}
type nativeModel struct {
	ID          string `json:"modelId"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Meta        struct {
		ReasoningEffort  string `json:"reasoningEffort"`
		ReasoningEfforts []struct {
			ID          string `json:"id"`
			Value       string `json:"value"`
			Label       string `json:"label"`
			Description string `json:"description"`
			Default     bool   `json:"default"`
		} `json:"reasoningEfforts"`
	} `json:"_meta"`
}
type nativeMode struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}
type sessionResponse struct {
	SessionID string     `json:"sessionId"`
	Models    modelState `json:"models"`
	Modes     struct {
		CurrentModeID  string       `json:"currentModeId"`
		AvailableModes []nativeMode `json:"availableModes"`
	} `json:"modes"`
	ConfigOptions []struct {
		ID           string `json:"id"`
		CurrentValue string `json:"currentValue"`
		Options      []struct {
			Value string `json:"value"`
			Name  string `json:"name"`
		} `json:"options"`
	} `json:"configOptions"`
	Meta json.RawMessage `json:"_meta"`
}
type initializeResponse struct {
	ProtocolVersion   int `json:"protocolVersion"`
	AgentCapabilities struct {
		LoadSession        bool `json:"loadSession"`
		PromptCapabilities struct {
			Image bool `json:"image"`
		} `json:"promptCapabilities"`
		MCPCapabilities struct {
			HTTP bool `json:"http"`
		} `json:"mcpCapabilities"`
	} `json:"agentCapabilities"`
	AuthMethods []struct {
		ID string `json:"id"`
	} `json:"authMethods"`
	Meta struct {
		GrokShell    bool       `json:"grokShell"`
		AgentVersion string     `json:"agentVersion"`
		ModelState   modelState `json:"modelState"`
	} `json:"_meta"`
}
type promptResponse struct {
	StopReason string `json:"stopReason"`
	Meta       struct {
		PromptID string       `json:"promptId"`
		Usage    *nativeUsage `json:"usage"`
	} `json:"_meta"`
}

type nativeProcess interface {
	io.ReadWriter
	Close() error
	CloseStdin()
	Done() <-chan struct{}
	Err() error
}

type turnSession struct {
	conn      *acp.Connection
	input     external.PromptInput
	driver    *Driver
	ctx       context.Context
	cancel    context.CancelFunc
	logger    *slog.Logger
	recorder  *external.TranscriptRecorder
	mu        sync.Mutex
	sessionID string
	restoring bool
	closed    bool
	decisions sync.WaitGroup
	text      strings.Builder
	tools     map[string]*toolState
	metadata  map[string]any
}
type toolState struct {
	title          string
	input          any
	content        any
	started, ended bool
}

func newTurnSession(ctx context.Context, d *Driver, input external.PromptInput, rw io.ReadWriter) *turnSession {
	decisionCtx, cancel := context.WithCancel(ctx)
	t := &turnSession{input: input, driver: d, ctx: decisionCtx, cancel: cancel, logger: d.logger, recorder: external.NewTranscriptRecorder(input.ToolOutputLimit), tools: map[string]*toolState{}, metadata: map[string]any{}}
	t.conn = acp.NewConnection(t.handle, rw, rw)
	t.conn.SetLogger(d.logger)
	return t
}

func (t *turnSession) closeDecisions() {
	t.mu.Lock()
	t.closed = true
	t.cancel()
	t.mu.Unlock()
	t.decisions.Wait()
}

func (t *turnSession) emit(ev event.StreamEvent) {
	t.recorder.Add(ev)
	if t.input.Sink != nil {
		t.input.Sink.EmitStreamEvent(ev)
	}
}

// RPC cancellation is joined below; tenant/run values come from the turn.
//
//nolint:contextcheck // The transport context does not contain application ownership.
func (t *turnSession) handle(rpcCtx context.Context, method string, params json.RawMessage) (any, *acp.RequestError) {
	if method == "session/update" {
		if err := t.update(params); err != nil {
			return nil, acp.NewInvalidParams(nil)
		}
		return nil, nil
	}
	switch method {
	case "session/request_permission", "_x.ai/ask_user_question", "_x.ai/exit_plan_mode", "_x.ai/mcp/elicit":
		var identity struct {
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(params, &identity) != nil {
			return nil, acp.NewInvalidParams(nil)
		}
		t.mu.Lock()
		if t.closed || t.restoring || identity.SessionID == "" || identity.SessionID != t.sessionID {
			t.mu.Unlock()
			return nil, acp.NewInvalidParams(nil)
		}
		t.decisions.Add(1)
		t.mu.Unlock()
		defer t.decisions.Done()
		ctx, cancel := context.WithCancel(t.ctx)
		defer cancel()
		stop := context.AfterFunc(rpcCtx, cancel)
		defer stop()
		switch method {
		case "session/request_permission":
			return t.permission(ctx, params)
		case "_x.ai/ask_user_question":
			return t.questions(ctx, params)
		case "_x.ai/exit_plan_mode":
			return t.exitPlan(ctx, params)
		default:
			return t.elicit(ctx, params)
		}
	default:
		// Optional extension notifications are not terminal prompt responses. In
		// particular, completion of a subagent must not end this Memoh run.
		return nil, acp.NewMethodNotFound(method)
	}
}

func (t *turnSession) update(raw json.RawMessage) error {
	var p struct {
		SessionID string `json:"sessionId"`
		Meta      struct {
			IsReplay bool `json:"isReplay"`
		} `json:"_meta"`
		Update struct {
			Kind              string          `json:"sessionUpdate"`
			Content           json.RawMessage `json:"content"`
			ToolCallID        string          `json:"toolCallId"`
			Title             string          `json:"title"`
			Status            string          `json:"status"`
			RawInput          any             `json:"rawInput"`
			RawOutput         any             `json:"rawOutput"`
			CurrentModeID     string          `json:"currentModeId"`
			AvailableCommands json.RawMessage `json:"availableCommands"`
		} `json:"update"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.restoring || p.Meta.IsReplay || (t.sessionID != "" && p.SessionID != t.sessionID) {
		return nil
	}
	u := p.Update
	switch u.Kind {
	case "agent_message_chunk", "agent_thought_chunk":
		var block struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(u.Content, &block) != nil || block.Type != "text" {
			return nil
		}
		kind := event.TextDelta
		if u.Kind == "agent_thought_chunk" {
			kind = event.ReasoningDelta
		} else {
			t.text.WriteString(block.Text)
		}
		t.emit(event.StreamEvent{Type: kind, Delta: block.Text})
	case "tool_call", "tool_call_update":
		if u.ToolCallID == "" {
			return errors.New("missing Grok tool call id")
		}
		tool := t.tools[u.ToolCallID]
		if tool == nil {
			tool = &toolState{}
			t.tools[u.ToolCallID] = tool
		}
		if u.Title != "" {
			tool.title = u.Title
		}
		if u.RawInput != nil {
			tool.input = u.RawInput
		}
		if u.RawOutput != nil {
			tool.content = u.RawOutput
		} else if len(u.Content) > 0 {
			var content any
			if json.Unmarshal(u.Content, &content) == nil {
				tool.content = content
			}
		}
		if !tool.ended && (!tool.started || u.RawInput != nil) {
			tool.started = true
			t.emit(event.StreamEvent{Type: event.ToolCallStart, ToolCallID: u.ToolCallID, ToolName: first(tool.title, "grok_tool"), Input: tool.input})
		}
		if (u.Status == "completed" || u.Status == "failed") && !tool.ended {
			tool.ended = true
			t.emit(event.StreamEvent{Type: event.ToolCallEnd, ToolCallID: u.ToolCallID, ToolName: first(tool.title, "grok_tool"), Input: tool.input, Result: tool.content, Status: u.Status})
		}
	case "current_mode_update":
		if u.CurrentModeID == "default" || u.CurrentModeID == "plan" {
			t.metadata["grok_native_mode"] = u.CurrentModeID
			// Native plan approval/abandonment changes the mode within a turn.
			// Persist that transition in the same field used by the composer and
			// the next lease, or both would keep re-entering the old plan mode.
			t.metadata["collaboration_mode"] = u.CurrentModeID
		}
	case "available_commands_update":
		var commands any
		if json.Unmarshal(u.AvailableCommands, &commands) == nil {
			t.metadata["grok_commands"] = commands
		}
	}
	return nil
}

func call[T any](ctx context.Context, t *turnSession, method string, params any) (T, error) {
	return acp.SendRequest[T](t.conn, ctx, method, params)
}

func first(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
func metaString(m map[string]any, key string) string { v, _ := m[key].(string); return v }
