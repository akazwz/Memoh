package acpagent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/memohai/memoh/internal/acpclient"
	"github.com/memohai/memoh/internal/conversation"
)

type fakeStarter struct {
	mu      sync.Mutex
	reqs    []acpclient.StartRequest
	session *fakeACPSession
}

func (s *fakeStarter) StartSession(_ context.Context, req acpclient.StartRequest, sink acpclient.EventSink) (ACPSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = append(s.reqs, req)
	s.session = &fakeACPSession{
		id:      "acp-session-1",
		project: "/workspace/app",
		sink:    sink,
		prompts: make(chan string, 8),
	}
	return s.session, nil
}

type fakeACPSession struct {
	id      string
	project string
	sink    acpclient.EventSink
	prompts chan string
}

func (s *fakeACPSession) ID() string          { return s.id }
func (s *fakeACPSession) ProjectPath() string { return s.project }
func (*fakeACPSession) Close() error          { return nil }

func (s *fakeACPSession) Prompt(_ context.Context, prompt string) (acpclient.PromptResult, error) {
	s.prompts <- prompt
	s.sink.EmitACPEvent(acpclient.StreamEvent{
		Type:  acpclient.StreamEventTextDelta,
		Delta: "done: " + prompt,
	})
	return acpclient.PromptResult{StopReason: "end_turn"}, nil
}

func TestServiceStartStreamsAndCompletes(t *testing.T) {
	starter := &fakeStarter{}
	svc := NewService(nil, starter)

	streamCh := make(chan StreamOutput, 16)
	completeCh := make(chan Completion, 4)
	sequenceCh := make(chan string, 8)
	svc.SetStreamPublisher(func(out StreamOutput) {
		if typ, _ := out.Stream["type"].(string); typ != "" {
			sequenceCh <- "stream:" + typ
		}
		streamCh <- out
	})
	svc.SetCompleteFunc(func(_ context.Context, event Completion) {
		sequenceCh <- "complete"
		completeCh <- event
	})

	snapshot, err := svc.Start(context.Background(), StartInput{
		BotID:                "bot-1",
		ChatID:               "chat-1",
		SessionID:            "session-1",
		ChannelIdentityID:    "user-1",
		Task:                 "build it",
		ProjectPath:          "app",
		UserMessagePersisted: true,
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if snapshot.TaskID == "" || snapshot.ACPSession != "acp-session-1" || snapshot.ProjectPath != "/workspace/app" {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if snapshot.AgentID != CodexAgentID || snapshot.AgentName != CodexAgentName {
		t.Fatalf("snapshot agent = %q/%q, want %q/%q", snapshot.AgentID, snapshot.AgentName, CodexAgentID, CodexAgentName)
	}
	if !svc.Active("bot-1", "session-1") {
		t.Fatal("service should report active ACP agent session")
	}

	completion := waitCompletion(t, completeCh)
	if completion.Prompt != "build it" || completion.Text != "done: build it" || !completion.UserMessagePersisted {
		t.Fatalf("completion = %+v", completion)
	}

	seenStart := false
	seenMessage := false
	seenEnd := false
	for i := 0; i < 3; i++ {
		out := waitStream(t, streamCh)
		switch out.Stream["type"] {
		case "start":
			seenStart = true
		case "message":
			msg, ok := out.Stream["data"].(conversation.UIMessage)
			if !ok {
				t.Fatalf("message payload = %#v", out.Stream["data"])
			}
			seenMessage = strings.Contains(msg.Content, "done: build it")
		case "end":
			seenEnd = true
		}
	}
	if !seenStart || !seenMessage || !seenEnd {
		t.Fatalf("stream flags start=%v message=%v end=%v", seenStart, seenMessage, seenEnd)
	}
	wantSequence := []string{"stream:start", "stream:message", "complete", "stream:end"}
	for _, want := range wantSequence {
		if got := waitSequence(t, sequenceCh); got != want {
			t.Fatalf("sequence = %q, want %q", got, want)
		}
	}
}

func TestServiceSendReusesActiveSession(t *testing.T) {
	starter := &fakeStarter{}
	svc := NewService(nil, starter)
	completeCh := make(chan Completion, 4)
	svc.SetCompleteFunc(func(_ context.Context, event Completion) {
		completeCh <- event
	})

	if _, err := svc.Start(context.Background(), StartInput{
		BotID:     "bot-1",
		SessionID: "session-1",
		Task:      "first",
	}); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	_ = waitCompletion(t, completeCh)

	if _, err := svc.Send(context.Background(), SendInput{
		BotID:                "bot-1",
		SessionID:            "session-1",
		Text:                 "follow up",
		UserMessagePersisted: false,
	}); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	completion := waitCompletion(t, completeCh)
	if completion.Prompt != "follow up" || completion.Text != "done: follow up" || completion.UserMessagePersisted {
		t.Fatalf("completion = %+v", completion)
	}

	starter.mu.Lock()
	startCount := len(starter.reqs)
	starter.mu.Unlock()
	if startCount != 1 {
		t.Fatalf("StartSession calls = %d, want 1", startCount)
	}
}

func TestServiceStartUsesRequestedProfile(t *testing.T) {
	starter := &fakeStarter{}
	svc := NewService(nil, starter, Profile{
		ID:          "gemini",
		DisplayName: "Gemini",
		Command:     "gemini",
		Args:        []string{"--acp"},
	})
	completeCh := make(chan Completion, 1)
	svc.SetCompleteFunc(func(_ context.Context, event Completion) {
		completeCh <- event
	})

	snapshot, err := svc.Start(context.Background(), StartInput{
		AgentID:   "gemini",
		BotID:     "bot-1",
		SessionID: "session-1",
		Task:      "fix it",
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if snapshot.AgentID != "gemini" || snapshot.AgentName != "Gemini" {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	completion := waitCompletion(t, completeCh)
	if completion.AgentID != "gemini" || completion.AgentName != "Gemini" {
		t.Fatalf("completion = %+v", completion)
	}

	starter.mu.Lock()
	defer starter.mu.Unlock()
	if len(starter.reqs) != 1 {
		t.Fatalf("StartSession calls = %d, want 1", len(starter.reqs))
	}
	if starter.reqs[0].Command != "gemini" || strings.Join(starter.reqs[0].Args, " ") != "--acp" {
		t.Fatalf("StartSession request = %+v", starter.reqs[0])
	}
}

func TestCodexProfileOwnsAdapterCommand(t *testing.T) {
	profile := CodexProfile()
	if profile.Command != codexACPCommand {
		t.Fatalf("CodexProfile command = %q, want %q", profile.Command, codexACPCommand)
	}
	if strings.Join(profile.Args, " ") != "-lc "+codexACPShell {
		t.Fatalf("CodexProfile args = %q", strings.Join(profile.Args, " "))
	}
}

func TestConvertACPEventLabelsAgentToolActivity(t *testing.T) {
	run := &promptRun{
		converter: conversation.NewUIMessageStreamConverter(),
		toolSeen:  map[string]struct{}{},
	}
	task := &Task{agentID: CodexAgentID, agentName: CodexAgentName}

	start := convertACPEventLocked(run, task, acpclient.StreamEvent{
		Type: acpclient.StreamEventToolStart,
		Tool: acpclient.ToolSummary{
			ID:     "tool-1",
			Title:  "Inspect files",
			Status: "pending",
		},
	})
	if len(start) != 1 {
		t.Fatalf("expected one tool start message, got %#v", start)
	}
	if start[0].Name != acpAgentToolName {
		t.Fatalf("tool name = %q, want %q", start[0].Name, acpAgentToolName)
	}
	if start[0].Metadata["source"] != SourceACPAgent || start[0].Metadata["agent_id"] != CodexAgentID {
		t.Fatalf("expected ACP agent metadata, got %#v", start[0].Metadata)
	}
}

func TestServiceStartStreamsHandoffBeforeCodexOutput(t *testing.T) {
	starter := &fakeStarter{}
	svc := NewService(nil, starter)
	streamCh := make(chan StreamOutput, 16)
	completeCh := make(chan Completion, 1)
	svc.SetStreamPublisher(func(out StreamOutput) {
		streamCh <- out
	})
	svc.SetCompleteFunc(func(_ context.Context, event Completion) {
		completeCh <- event
	})

	if _, err := svc.Start(context.Background(), StartInput{
		BotID:       "bot-1",
		SessionID:   "session-1",
		Task:        "build it",
		HandoffText: "接下来由 Codex 回复。",
	}); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	_ = waitCompletion(t, completeCh)
	_ = waitStream(t, streamCh) // start
	handoff := waitStream(t, streamCh)
	msg, ok := handoff.Stream["data"].(conversation.UIMessage)
	if !ok || msg.Content != "接下来由 Codex 回复。" {
		t.Fatalf("expected handoff message, got %#v", handoff.Stream["data"])
	}
	codexOutput := waitStream(t, streamCh)
	msg, ok = codexOutput.Stream["data"].(conversation.UIMessage)
	if !ok || !strings.Contains(msg.Content, "done: build it") || msg.ID == 0 {
		t.Fatalf("expected Codex output after handoff, got %#v", codexOutput.Stream["data"])
	}
}

func waitStream(t *testing.T, ch <-chan StreamOutput) StreamOutput {
	t.Helper()
	select {
	case out := <-ch:
		return out
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for stream")
		return StreamOutput{}
	}
}

func waitCompletion(t *testing.T, ch <-chan Completion) Completion {
	t.Helper()
	select {
	case event := <-ch:
		return event
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for completion")
		return Completion{}
	}
}

func waitSequence(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case event := <-ch:
		return event
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for sequence event")
		return ""
	}
}
