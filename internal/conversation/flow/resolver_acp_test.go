package flow

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/memohai/memoh/internal/acpagent"
	"github.com/memohai/memoh/internal/acpclient"
	agentpkg "github.com/memohai/memoh/internal/agent"
	"github.com/memohai/memoh/internal/conversation"
	"github.com/memohai/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/memohai/memoh/internal/db/store"
	memprovider "github.com/memohai/memoh/internal/memory/adapters"
	"github.com/memohai/memoh/internal/session"
	"github.com/memohai/memoh/internal/settings"
)

const (
	storeRoundBotID            = "11111111-1111-1111-1111-111111111111"
	storeRoundMemoryProviderID = "22222222-2222-2222-2222-222222222222"
)

func TestStreamChatWSRoutesACPAgentSessionToACPPool(t *testing.T) {
	t.Parallel()

	messages := &recordingMessageService{}
	pool := &recordingACPPrompter{
		result: acpclient.PromptResult{
			Text:       "done from codex",
			StopReason: "end_turn",
		},
	}
	resolver := &Resolver{
		messageService: messages,
		acpPool:        pool,
		sessionService: &fakeBackgroundSessionService{
			getFn: func(_ context.Context, sessionID string) (session.Session, error) {
				if sessionID != "session-1" {
					t.Fatalf("unexpected session id: %s", sessionID)
				}
				return session.Session{
					ID:    "session-1",
					BotID: "bot-1",
					Type:  session.TypeACPAgent,
					Metadata: map[string]any{
						"acp_agent_id": "codex",
						"project_path": "/data/app",
					},
				}, nil
			},
		},
		logger: slog.New(slog.DiscardHandler),
	}

	eventCh := make(chan WSStreamEvent, 8)
	if err := resolver.StreamChatWS(
		context.Background(),
		conversation.ChatRequest{
			BotID:     "bot-1",
			SessionID: "session-1",
			Query:     "inspect the app",
		},
		eventCh,
		make(chan struct{}),
	); err != nil {
		t.Fatalf("StreamChatWS() error = %v", err)
	}

	if pool.calls != 1 {
		t.Fatalf("ACP pool calls = %d, want 1", pool.calls)
	}
	if pool.input.BotID != "bot-1" || pool.input.SessionID != "session-1" || pool.input.AgentID != "codex" || pool.input.ProjectPath != "/data/app" {
		t.Fatalf("ACP prompt input = %#v", pool.input)
	}
	if len(messages.persisted) != 2 {
		t.Fatalf("persisted %d messages, want user + assistant", len(messages.persisted))
	}
	if messages.persisted[0].Role != "user" || messages.persisted[1].Role != "assistant" {
		t.Fatalf("persisted roles = %q, %q", messages.persisted[0].Role, messages.persisted[1].Role)
	}
	if got := messages.persisted[1].Metadata["acp_agent_id"]; got != "codex" {
		t.Fatalf("assistant acp_agent_id = %#v, want codex", got)
	}

	events := drainAgentEvents(t, eventCh)
	if !containsStreamEvent(events, agentpkg.EventAgentStart) || !containsStreamEvent(events, agentpkg.EventAgentEnd) {
		t.Fatalf("events = %#v, want agent start/end", events)
	}
	if !containsTextDelta(events, "streamed from acp") {
		t.Fatalf("events = %#v, want ACP stream delta", events)
	}
}

func TestStreamACPAgentWSRequestsAutoTitle(t *testing.T) {
	t.Parallel()

	sessionGets := make(chan string, 2)
	messages := &recordingMessageService{}
	pool := &recordingACPPrompter{
		result: acpclient.PromptResult{
			Text:       "done",
			StopReason: "end_turn",
		},
	}
	resolver := &Resolver{
		messageService: messages,
		acpPool:        pool,
		sessionService: &fakeBackgroundSessionService{
			getFn: func(_ context.Context, sessionID string) (session.Session, error) {
				recordSessionGet(sessionGets, sessionID)
				return session.Session{
					ID:    sessionID,
					BotID: "bot-1",
					Type:  session.TypeACPAgent,
					Metadata: map[string]any{
						"acp_agent_id": "codex",
						"project_path": "/data/app",
					},
				}, nil
			},
		},
		logger: slog.New(slog.DiscardHandler),
	}

	if err := resolver.streamACPAgentWS(
		context.Background(),
		conversation.ChatRequest{
			BotID:     "bot-1",
			SessionID: "session-1",
			Query:     "inspect the app",
		},
		make(chan WSStreamEvent, 8),
		make(chan struct{}),
	); err != nil {
		t.Fatalf("streamACPAgentWS() error = %v", err)
	}

	waitForSessionGets(t, sessionGets, 2)
}

func TestPersistACPRoundUsesDedicatedSessionMetadata(t *testing.T) {
	t.Parallel()

	messages := &recordingMessageService{}
	resolver := &Resolver{
		messageService: messages,
		logger:         slog.New(slog.DiscardHandler),
	}

	err := resolver.persistACPRound(
		context.Background(),
		conversation.ChatRequest{
			BotID:     "bot-1",
			SessionID: "session-1",
			Query:     "inspect the project",
		},
		"codex",
		"/data/app",
		acpclient.PromptResult{
			Text:       "done",
			StopReason: "end_turn",
			ToolCalls: []acpclient.ToolSummary{{
				ID:     "tool-1",
				Title:  "Read main.go",
				Status: "completed",
				Kind:   "fs_read",
			}},
		},
		nil,
	)
	if err != nil {
		t.Fatalf("persistACPRound returned error: %v", err)
	}
	if len(messages.persisted) != 2 {
		t.Fatalf("persisted %d messages, want 2", len(messages.persisted))
	}

	userMeta := messages.persisted[0].Metadata
	if _, ok := userMeta["source"]; ok {
		t.Fatalf("user metadata must not use ACP source markers: %#v", userMeta)
	}

	assistantMeta := messages.persisted[1].Metadata
	if _, ok := assistantMeta["source"]; ok {
		t.Fatalf("assistant metadata must not use ACP source markers: %#v", assistantMeta)
	}
	if assistantMeta["acp_agent_id"] != "codex" {
		t.Fatalf("acp_agent_id = %#v, want codex", assistantMeta["acp_agent_id"])
	}
	if assistantMeta["project_path"] != "/data/app" {
		t.Fatalf("project_path = %#v, want /data/app", assistantMeta["project_path"])
	}
	if assistantMeta["stop_reason"] != "end_turn" {
		t.Fatalf("stop_reason = %#v, want end_turn", assistantMeta["stop_reason"])
	}
	if _, ok := assistantMeta["acp_actions"]; !ok {
		t.Fatalf("expected acp_actions metadata, got %#v", assistantMeta)
	}
}

func TestPersistACPRoundEmptyTextLeavesAssistantBlank(t *testing.T) {
	t.Parallel()

	// Backend must not inject any UI placeholder text; an empty assistant
	// message is the source of truth and the web client renders an i18n
	// placeholder based on the acp_actions / acp_plan metadata.
	tests := []struct {
		name   string
		result acpclient.PromptResult
	}{
		{
			name: "actions without text",
			result: acpclient.PromptResult{
				ToolCalls: []acpclient.ToolSummary{{ID: "tool-1", Status: "completed"}},
			},
		},
		{
			name:   "no visible output",
			result: acpclient.PromptResult{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			messages := &recordingMessageService{}
			resolver := &Resolver{
				messageService: messages,
				logger:         slog.New(slog.DiscardHandler),
			}
			if err := resolver.persistACPRound(
				context.Background(),
				conversation.ChatRequest{BotID: "bot-1", SessionID: "session-1", Query: "run"},
				"codex",
				"/data/app",
				tt.result,
				nil,
			); err != nil {
				t.Fatalf("persistACPRound() error = %v", err)
			}
			if len(messages.persisted) != 2 {
				t.Fatalf("persisted %d messages, want 2", len(messages.persisted))
			}
			if got := persistedText(t, messages.persisted[1].Content); got != "" {
				t.Fatalf("assistant text = %q, want empty (frontend renders placeholder)", got)
			}
		})
	}
}

func TestStreamACPAgentWSFailurePersistsRoundAndSkipsMemory(t *testing.T) {
	t.Parallel()

	messages := &recordingMessageService{}
	memory := &storeRoundMemoryProvider{afterChat: make(chan memprovider.AfterChatRequest, 1)}
	registry := memprovider.NewRegistry(slog.New(slog.DiscardHandler))
	registry.Register(storeRoundMemoryProviderID, memory)
	pool := &recordingACPPrompter{err: errors.New("missing codex-acp")}
	resolver := &Resolver{
		messageService:  messages,
		memoryRegistry:  registry,
		settingsService: settings.NewService(slog.New(slog.DiscardHandler), &storeRoundSettingsQueries{}, nil, nil),
		acpPool:         pool,
		sessionService: &fakeBackgroundSessionService{
			getFn: func(_ context.Context, sessionID string) (session.Session, error) {
				return session.Session{
					ID:    sessionID,
					BotID: storeRoundBotID,
					Type:  session.TypeACPAgent,
					Metadata: map[string]any{
						"acp_agent_id": "codex",
						"project_path": "/data/app",
					},
				}, nil
			},
		},
		logger: slog.New(slog.DiscardHandler),
	}

	eventCh := make(chan WSStreamEvent, 8)
	if err := resolver.streamACPAgentWS(
		context.Background(),
		conversation.ChatRequest{
			BotID:     storeRoundBotID,
			SessionID: "session-1",
			Query:     "inspect",
		},
		eventCh,
		make(chan struct{}),
	); err != nil {
		t.Fatalf("streamACPAgentWS() error = %v", err)
	}

	if len(messages.persisted) != 2 {
		t.Fatalf("persisted %d messages, want user + assistant", len(messages.persisted))
	}
	if got := persistedText(t, messages.persisted[1].Content); !strings.Contains(got, "missing codex-acp") {
		t.Fatalf("assistant failure text = %q, want raw upstream error", got)
	}
	if got, _ := messages.persisted[1].Metadata["error"].(string); !strings.Contains(got, "missing codex-acp") {
		t.Fatalf("assistant error metadata = %#v", messages.persisted[1].Metadata)
	}
	events := drainAgentEvents(t, eventCh)
	if !containsStreamEvent(events, agentpkg.EventAgentAbort) {
		t.Fatalf("events = %#v, want agent abort", events)
	}
	select {
	case got := <-memory.afterChat:
		t.Fatalf("memory was called for ACP stream despite SkipMemory=true: %#v", got)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestACPFailureResultPreservesPartialOutput(t *testing.T) {
	t.Parallel()

	partial := acpclient.PromptResult{
		Text: "partial answer",
		ToolCalls: []acpclient.ToolSummary{{
			ID:     "tool-1",
			Title:  "Inspect file",
			Status: "running",
		}},
	}
	got, delta := acpFailureResult(partial, errors.New("adapter crashed"))
	if !strings.Contains(got.Text, "partial answer") || !strings.Contains(got.Text, "adapter crashed") || len(got.ToolCalls) != 1 {
		t.Fatalf("acpFailureResult() = %#v, want partial output preserved", got)
	}
	if !strings.Contains(delta, "adapter crashed") {
		t.Fatalf("failure delta = %q, want raw upstream error", delta)
	}

	empty, delta := acpFailureResult(acpclient.PromptResult{}, errors.New("missing codex-acp"))
	if empty.Text == "" {
		t.Fatalf("empty failure result should contain the upstream error text")
	}
	if empty.Text != delta {
		t.Fatalf("empty failure result text = %q, delta = %q; want same visible text", empty.Text, delta)
	}
	if empty.Text != "missing codex-acp" {
		t.Fatalf("empty failure result text = %q, want exact upstream error", empty.Text)
	}
}

func TestMapACPToolUpdateTerminalStatusEndsTool(t *testing.T) {
	t.Parallel()

	events := mapACPStreamEvent(acpclient.StreamEvent{
		Type: acpclient.StreamEventToolUpdate,
		Tool: acpclient.ToolSummary{
			ID:     "tool-1",
			Title:  "Read file",
			Kind:   "read",
			Status: "completed",
		},
	})
	if len(events) != 1 {
		t.Fatalf("events len = %d, want 1", len(events))
	}
	if events[0].Type != agentpkg.EventToolCallEnd {
		t.Fatalf("event type = %q, want %q", events[0].Type, agentpkg.EventToolCallEnd)
	}
	result, ok := events[0].Result.(map[string]any)
	if !ok || result["status"] != "completed" {
		t.Fatalf("event result = %#v, want completed status", events[0].Result)
	}
}

func TestMapACPPlanDoesNotLeaveRunningTool(t *testing.T) {
	t.Parallel()

	events := mapACPStreamEvent(acpclient.StreamEvent{
		Type: acpclient.StreamEventPlan,
		Plan: []acpclient.PlanItem{{Content: "Inspect", Status: "pending"}},
	})
	if len(events) != 1 {
		t.Fatalf("events len = %d, want 1", len(events))
	}
	if events[0].Type != agentpkg.EventToolCallEnd || events[0].ToolName != "acp_plan" {
		t.Fatalf("event = %#v, want acp_plan tool end", events[0])
	}
}

func TestShouldGenerateSessionTitleAllowsACPPlaceholderTitle(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		sess session.Session
		want bool
	}{
		{
			name: "empty title",
			sess: session.Session{Type: session.TypeChat},
			want: true,
		},
		{
			name: "normal chat existing title",
			sess: session.Session{Type: session.TypeChat, Title: "Existing"},
			want: false,
		},
		{
			name: "acp display placeholder",
			sess: session.Session{
				Type:  session.TypeACPAgent,
				Title: "Codex",
				Metadata: map[string]any{
					"acp_agent_id": "codex",
				},
			},
			want: true,
		},
		{
			name: "acp user title",
			sess: session.Session{
				Type:  session.TypeACPAgent,
				Title: "Real work",
				Metadata: map[string]any{
					"acp_agent_id": "codex",
				},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldGenerateSessionTitle(tt.sess); got != tt.want {
				t.Fatalf("shouldGenerateSessionTitle() = %v, want %v", got, tt.want)
			}
		})
	}
}

type recordingACPPrompter struct {
	calls  int
	input  acpagent.PromptInput
	result acpclient.PromptResult
	err    error
}

type storeRoundMemoryProvider struct {
	memprovider.Provider
	afterChat chan memprovider.AfterChatRequest
}

func (*storeRoundMemoryProvider) Type() string {
	return "test"
}

func (p *storeRoundMemoryProvider) OnAfterChat(_ context.Context, req memprovider.AfterChatRequest) error {
	p.afterChat <- req
	return nil
}

type storeRoundSettingsQueries struct {
	dbstore.Queries
}

func (*storeRoundSettingsQueries) GetSettingsByBotID(_ context.Context, botID pgtype.UUID) (sqlc.GetSettingsByBotIDRow, error) {
	return sqlc.GetSettingsByBotIDRow{
		BotID:             botID,
		Language:          "auto",
		ReasoningEffort:   "medium",
		HeartbeatInterval: 30,
		CompactionRatio:   80,
		MemoryProviderID:  flowTestUUID(storeRoundMemoryProviderID),
	}, nil
}

func flowTestUUID(value string) pgtype.UUID {
	var out pgtype.UUID
	if err := out.Scan(value); err != nil {
		panic(err)
	}
	return out
}

func (p *recordingACPPrompter) Prompt(_ context.Context, input acpagent.PromptInput) (acpclient.PromptResult, error) {
	p.calls++
	p.input = input
	if input.Sink != nil {
		input.Sink.EmitACPEvent(acpclient.StreamEvent{Type: acpclient.StreamEventTextDelta, Delta: "streamed from acp"})
	}
	return p.result, p.err
}

func drainAgentEvents(t *testing.T, eventCh <-chan WSStreamEvent) []agentpkg.StreamEvent {
	t.Helper()
	events := make([]agentpkg.StreamEvent, 0, len(eventCh))
	for len(eventCh) > 0 {
		var event agentpkg.StreamEvent
		if err := json.Unmarshal(<-eventCh, &event); err != nil {
			t.Fatalf("decode stream event: %v", err)
		}
		events = append(events, event)
	}
	return events
}

func containsStreamEvent(events []agentpkg.StreamEvent, eventType agentpkg.StreamEventType) bool {
	for _, event := range events {
		if event.Type == eventType {
			return true
		}
	}
	return false
}

func containsTextDelta(events []agentpkg.StreamEvent, delta string) bool {
	for _, event := range events {
		if event.Type == agentpkg.EventTextDelta && event.Delta == delta {
			return true
		}
	}
	return false
}

func recordSessionGet(ch chan<- string, sessionID string) {
	select {
	case ch <- sessionID:
	default:
	}
}

func waitForSessionGets(t *testing.T, ch <-chan string, want int) {
	t.Helper()
	deadline := time.After(time.Second)
	for count := 0; count < want; count++ {
		select {
		case <-ch:
		case <-deadline:
			t.Fatalf("observed %d session Get calls, want %d", count, want)
		}
	}
}

func persistedText(t *testing.T, content json.RawMessage) string {
	t.Helper()
	var msg conversation.ModelMessage
	if err := json.Unmarshal(content, &msg); err != nil {
		t.Fatalf("decode persisted content: %v", err)
	}
	return msg.TextContent()
}
