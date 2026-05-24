package acpagent

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/memohai/memoh/internal/acpclient"
	"github.com/memohai/memoh/internal/conversation"
)

const (
	SourceACPAgent = "acp_agent"

	CodexAgentID   = "codex"
	CodexAgentName = "Codex"

	acpAgentToolName = "acp_agent_tool"
	acpAgentPlanName = "acp_agent_plan"
)

type Profile struct {
	ID          string
	DisplayName string
	Command     string
	Args        []string
}

func CodexProfile() Profile {
	return Profile{
		ID:          CodexAgentID,
		DisplayName: CodexAgentName,
		Command:     acpclient.DefaultCodexACPCommand,
		Args:        append([]string(nil), acpclient.DefaultCodexACPArgs...),
	}
}

type SessionStarter interface {
	StartSession(ctx context.Context, req acpclient.StartRequest, sink acpclient.EventSink) (ACPSession, error)
}

type ACPSession interface {
	ID() string
	ProjectPath() string
	Prompt(ctx context.Context, prompt string) (acpclient.PromptResult, error)
	Close() error
}

type StartInput struct {
	AgentID              string
	BotID                string
	ChatID               string
	SessionID            string
	ChannelIdentityID    string
	CurrentPlatform      string
	ReplyTarget          string
	ConversationType     string
	Task                 string
	ProjectPath          string
	HandoffText          string
	UserMessagePersisted bool
	Command              string
	Args                 []string
}

type SendInput struct {
	BotID                string
	ChatID               string
	SessionID            string
	ChannelIdentityID    string
	CurrentPlatform      string
	ReplyTarget          string
	ConversationType     string
	Text                 string
	UserMessagePersisted bool
}

type Snapshot struct {
	TaskID      string `json:"task_id"`
	AgentID     string `json:"agent_id,omitempty"`
	AgentName   string `json:"agent_name,omitempty"`
	BotID       string `json:"bot_id"`
	ChatID      string `json:"chat_id,omitempty"`
	SessionID   string `json:"session_id,omitempty"`
	ProjectPath string `json:"project_path,omitempty"`
	ACPSession  string `json:"acp_session_id,omitempty"`
	Status      string `json:"status"`
}

type StreamOutput struct {
	BotID     string
	SessionID string
	Stream    map[string]any
}

type Completion struct {
	TaskID               string
	AgentID              string
	AgentName            string
	BotID                string
	ChatID               string
	SessionID            string
	ChannelIdentityID    string
	CurrentPlatform      string
	ReplyTarget          string
	ConversationType     string
	Prompt               string
	Text                 string
	ProjectPath          string
	StopReason           string
	UserMessagePersisted bool
}

type Service struct {
	logger  *slog.Logger
	starter SessionStarter

	mu         sync.RWMutex
	profiles   map[string]Profile
	tasks      map[string]*Task
	publishFn  func(StreamOutput)
	completeFn func(context.Context, Completion)
}

type Task struct {
	id                string
	agentID           string
	agentName         string
	botID             string
	chatID            string
	sessionID         string
	channelIdentityID string
	currentPlatform   string
	replyTarget       string
	conversationType  string
	projectPath       string
	acpSessionID      string
	session           ACPSession

	promptMu sync.Mutex
	mu       sync.Mutex
	status   string
	current  *promptRun
}

type promptRun struct {
	prompt               string
	userMessagePersisted bool
	text                 strings.Builder
	converter            *conversation.UIMessageStreamConverter
	toolSeen             map[string]struct{}
}

func NewService(log *slog.Logger, starter SessionStarter, profiles ...Profile) *Service {
	if log == nil {
		log = slog.Default()
	}
	if len(profiles) == 0 {
		profiles = []Profile{CodexProfile()}
	}
	return &Service{
		logger:   log.With(slog.String("service", "acp_agent")),
		starter:  starter,
		profiles: normalizeProfiles(profiles),
		tasks:    map[string]*Task{},
	}
}

func (s *Service) Profile(id string) (Profile, bool) {
	if s == nil {
		return Profile{}, false
	}
	id = normalizeProfileID(id)
	if id == "" {
		id = CodexAgentID
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	profile, ok := s.profiles[id]
	return cloneProfile(profile), ok
}

func (s *Service) Profiles() []Profile {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Profile, 0, len(s.profiles))
	for _, profile := range s.profiles {
		out = append(out, cloneProfile(profile))
	}
	return out
}

func (s *Service) SetStreamPublisher(fn func(StreamOutput)) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.publishFn = fn
}

func (s *Service) SetCompleteFunc(fn func(context.Context, Completion)) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.completeFn = fn
}

func (s *Service) Start(ctx context.Context, input StartInput) (Snapshot, error) {
	if s == nil || s.starter == nil {
		return Snapshot{}, errors.New("ACP agent service is not configured")
	}
	profile, err := s.resolveProfile(input.AgentID)
	if err != nil {
		return Snapshot{}, err
	}
	botID := strings.TrimSpace(input.BotID)
	sessionID := normalizedSessionID(input.SessionID, input.ChatID)
	taskText := strings.TrimSpace(input.Task)
	if botID == "" {
		return Snapshot{}, errors.New("bot_id is required")
	}
	if sessionID == "" {
		return Snapshot{}, errors.New("session_id is required")
	}
	if taskText == "" {
		return Snapshot{}, errors.New("task is required")
	}
	if existing := s.lookup(botID, sessionID); existing != nil {
		existingAgentID, existingStatus := existing.state()
		switch {
		case existingStatus == "closed" || existingStatus == "failed":
			s.Stop(botID, sessionID)
		case profile.ID != "" && existingAgentID != profile.ID:
			return Snapshot{}, errors.New("another ACP agent session is already active for this conversation")
		default:
			promptCtx := context.WithoutCancel(ctx)
			go s.runPrompt(promptCtx, existing, taskText, input.UserMessagePersisted, "")
			return existing.snapshot(), nil
		}
	}

	taskID := uuid.NewString()
	command := firstNonEmpty(input.Command, profile.Command)
	args := append([]string(nil), input.Args...)
	if len(args) == 0 {
		args = append(args, profile.Args...)
	}
	sess, err := s.starter.StartSession(ctx, acpclient.StartRequest{
		BotID:       botID,
		ProjectPath: strings.TrimSpace(input.ProjectPath),
		Command:     command,
		Args:        args,
	}, acpclient.EventSinkFunc(func(event acpclient.StreamEvent) {
		s.handleACPEvent(taskID, event)
	}))
	if err != nil {
		return Snapshot{}, err
	}

	task := &Task{
		id:                taskID,
		agentID:           profile.ID,
		agentName:         profile.DisplayName,
		botID:             botID,
		chatID:            firstNonEmpty(input.ChatID, botID),
		sessionID:         sessionID,
		channelIdentityID: strings.TrimSpace(input.ChannelIdentityID),
		currentPlatform:   strings.TrimSpace(input.CurrentPlatform),
		replyTarget:       strings.TrimSpace(input.ReplyTarget),
		conversationType:  strings.TrimSpace(input.ConversationType),
		projectPath:       sess.ProjectPath(),
		acpSessionID:      sess.ID(),
		session:           sess,
		status:            "idle",
	}

	s.mu.Lock()
	s.tasks[taskKey(botID, sessionID)] = task
	s.mu.Unlock()

	promptCtx := context.WithoutCancel(ctx)
	go s.runPrompt(promptCtx, task, taskText, input.UserMessagePersisted, strings.TrimSpace(input.HandoffText))
	return task.snapshot(), nil
}

func (s *Service) Send(ctx context.Context, input SendInput) (Snapshot, error) {
	if s == nil {
		return Snapshot{}, errors.New("ACP agent service is not configured")
	}
	botID := strings.TrimSpace(input.BotID)
	sessionID := normalizedSessionID(input.SessionID, input.ChatID)
	text := strings.TrimSpace(input.Text)
	if botID == "" {
		return Snapshot{}, errors.New("bot_id is required")
	}
	if sessionID == "" {
		return Snapshot{}, errors.New("session_id is required")
	}
	if text == "" {
		return Snapshot{}, errors.New("message text is required")
	}
	task := s.lookup(botID, sessionID)
	if task == nil {
		return Snapshot{}, errors.New("no active ACP agent session for this conversation")
	}
	promptCtx := context.WithoutCancel(ctx)
	go s.runPrompt(promptCtx, task, text, input.UserMessagePersisted, "")
	return task.snapshot(), nil
}

func (s *Service) Active(botID, sessionID string) bool {
	_, ok := s.ActiveSnapshot(botID, sessionID)
	return ok
}

func (s *Service) ActiveSnapshot(botID, sessionID string) (Snapshot, bool) {
	task := s.lookup(botID, sessionID)
	if task == nil {
		return Snapshot{}, false
	}
	task.mu.Lock()
	defer task.mu.Unlock()
	if task.status == "closed" || task.status == "failed" {
		return Snapshot{}, false
	}
	return task.snapshotLocked(task.status), true
}

func (s *Service) Stop(botID, sessionID string) {
	if s == nil {
		return
	}
	key := taskKey(botID, sessionID)
	s.mu.Lock()
	task := s.tasks[key]
	delete(s.tasks, key)
	s.mu.Unlock()
	if task == nil {
		return
	}
	task.mu.Lock()
	task.status = "closed"
	task.mu.Unlock()
	if task.session != nil {
		_ = task.session.Close()
	}
}

func (s *Service) runPrompt(ctx context.Context, task *Task, prompt string, userMessagePersisted bool, handoffText string) {
	if s == nil || task == nil {
		return
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return
	}
	task.promptMu.Lock()
	defer task.promptMu.Unlock()

	run := &promptRun{
		prompt:               prompt,
		userMessagePersisted: userMessagePersisted,
		converter:            conversation.NewUIMessageStreamConverter(),
		toolSeen:             map[string]struct{}{},
	}

	task.mu.Lock()
	task.status = "running"
	task.current = run
	task.mu.Unlock()

	s.publish(task, map[string]any{
		"type":     "start",
		"metadata": task.uiMetadata(),
	})
	if handoffText = strings.TrimSpace(handoffText); handoffText != "" {
		for _, msg := range run.converter.HandleEvent(conversation.UIMessageStreamEvent{
			Type:  "text_delta",
			Delta: handoffText,
			Metadata: map[string]any{
				"source": "memoh",
				"agent":  "Memoh",
			},
		}) {
			s.publish(task, map[string]any{
				"type": "message",
				"data": msg,
			})
		}
		run.converter.HandleEvent(conversation.UIMessageStreamEvent{Type: "text_end"})
	}
	result, err := task.session.Prompt(ctx, prompt)

	task.mu.Lock()
	finalText := strings.TrimSpace(run.text.String())
	task.current = nil
	if err != nil {
		task.status = "failed"
	} else {
		task.status = "idle"
	}
	task.mu.Unlock()

	if finalText != "" {
		s.complete(ctx, Completion{
			TaskID:               task.id,
			AgentID:              task.agentID,
			AgentName:            task.agentName,
			BotID:                task.botID,
			ChatID:               task.chatID,
			SessionID:            task.sessionID,
			ChannelIdentityID:    task.channelIdentityID,
			CurrentPlatform:      task.currentPlatform,
			ReplyTarget:          task.replyTarget,
			ConversationType:     task.conversationType,
			Prompt:               prompt,
			Text:                 finalText,
			ProjectPath:          task.projectPath,
			StopReason:           result.StopReason,
			UserMessagePersisted: userMessagePersisted,
		})
	}
	if err != nil {
		s.publish(task, map[string]any{
			"type":     "error",
			"message":  strings.TrimSpace(err.Error()),
			"metadata": task.uiMetadata(),
		})
	} else {
		s.publish(task, map[string]any{
			"type":     "end",
			"metadata": task.uiMetadata(),
		})
	}
}

func (s *Service) handleACPEvent(taskID string, event acpclient.StreamEvent) {
	task := s.lookupByID(taskID)
	if task == nil {
		return
	}

	task.mu.Lock()
	run := task.current
	if run == nil {
		task.mu.Unlock()
		return
	}
	uiEvents := convertACPEventLocked(run, task, event)
	task.mu.Unlock()

	for _, msg := range uiEvents {
		s.publish(task, map[string]any{
			"type": "message",
			"data": msg,
		})
	}
}

func convertACPEventLocked(run *promptRun, task *Task, event acpclient.StreamEvent) []conversation.UIMessage {
	metadata := task.uiMetadata()
	switch event.Type {
	case acpclient.StreamEventTextDelta:
		if event.Delta == "" {
			return nil
		}
		run.text.WriteString(event.Delta)
		return run.converter.HandleEvent(conversation.UIMessageStreamEvent{
			Type:     "text_delta",
			Delta:    event.Delta,
			Metadata: metadata,
		})
	case acpclient.StreamEventToolStart:
		return run.converter.HandleEvent(conversation.UIMessageStreamEvent{
			Type:       "tool_call_start",
			ToolName:   acpAgentToolName,
			ToolCallID: event.Tool.ID,
			Input: map[string]any{
				"title":  event.Tool.Title,
				"status": event.Tool.Status,
			},
			Metadata: metadata,
		})
	case acpclient.StreamEventToolUpdate:
		id := strings.TrimSpace(event.Tool.ID)
		status := strings.ToLower(strings.TrimSpace(event.Tool.Status))
		if status == "completed" || status == "failed" {
			return run.converter.HandleEvent(conversation.UIMessageStreamEvent{
				Type:       "tool_call_end",
				ToolName:   acpAgentToolName,
				ToolCallID: id,
				Output: map[string]any{
					"title":  event.Tool.Title,
					"status": event.Tool.Status,
				},
				Metadata: metadata,
			})
		}
		return run.converter.HandleEvent(conversation.UIMessageStreamEvent{
			Type:       "tool_call_progress",
			ToolName:   acpAgentToolName,
			ToolCallID: id,
			Progress: map[string]any{
				"title":  event.Tool.Title,
				"status": event.Tool.Status,
			},
			Metadata: metadata,
		})
	case acpclient.StreamEventPlan:
		if len(event.Plan) == 0 {
			return nil
		}
		agentID := CodexAgentID
		if task != nil {
			agentID = firstNonEmpty(task.agentID, CodexAgentID)
		}
		return run.converter.HandleEvent(conversation.UIMessageStreamEvent{
			Type:       "tool_call_progress",
			ToolName:   acpAgentPlanName,
			ToolCallID: agentID + "-plan",
			Progress:   event.Plan,
			Metadata:   metadata,
		})
	default:
		return nil
	}
}

func (s *Service) publish(task *Task, stream map[string]any) {
	if s == nil || task == nil || len(stream) == 0 {
		return
	}
	s.mu.RLock()
	fn := s.publishFn
	s.mu.RUnlock()
	if fn == nil {
		return
	}
	fn(StreamOutput{
		BotID:     task.botID,
		SessionID: task.sessionID,
		Stream:    stream,
	})
}

func (s *Service) complete(ctx context.Context, event Completion) {
	if s == nil {
		return
	}
	s.mu.RLock()
	fn := s.completeFn
	s.mu.RUnlock()
	if fn == nil {
		return
	}
	fn(ctx, event)
}

func (s *Service) lookup(botID, sessionID string) *Task {
	if s == nil {
		return nil
	}
	key := taskKey(botID, sessionID)
	if key == "" {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.tasks[key]
}

func (s *Service) lookupByID(taskID string) *Task {
	if s == nil {
		return nil
	}
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, task := range s.tasks {
		if task.id == taskID {
			return task
		}
	}
	return nil
}

func (t *Task) snapshot() Snapshot {
	if t == nil {
		return Snapshot{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snapshotLocked(t.status)
}

func (t *Task) snapshotLocked(status string) Snapshot {
	if t == nil {
		return Snapshot{}
	}
	agentID := firstNonEmpty(t.agentID, CodexAgentID)
	agentName := firstNonEmpty(t.agentName, agentID)
	return Snapshot{
		TaskID:      t.id,
		AgentID:     agentID,
		AgentName:   agentName,
		BotID:       t.botID,
		ChatID:      t.chatID,
		SessionID:   t.sessionID,
		ProjectPath: t.projectPath,
		ACPSession:  t.acpSessionID,
		Status:      status,
	}
}

func (t *Task) state() (agentID, status string) {
	if t == nil {
		return "", ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.agentID, t.status
}

func (t *Task) uiMetadata() map[string]any {
	if t == nil {
		return agentUIMetadata(CodexAgentID, CodexAgentName)
	}
	return agentUIMetadata(t.agentID, t.agentName)
}

func normalizedSessionID(sessionID, chatID string) string {
	if trimmed := strings.TrimSpace(sessionID); trimmed != "" {
		return trimmed
	}
	return strings.TrimSpace(chatID)
}

func taskKey(botID, sessionID string) string {
	botID = strings.TrimSpace(botID)
	sessionID = strings.TrimSpace(sessionID)
	if botID == "" || sessionID == "" {
		return ""
	}
	return botID + ":" + sessionID
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func agentUIMetadata(agentID, agentName string) map[string]any {
	agentID = normalizeProfileID(agentID)
	if agentID == "" {
		agentID = CodexAgentID
	}
	agentName = firstNonEmpty(agentName, agentID)
	return map[string]any{
		"source":   SourceACPAgent,
		"agent_id": agentID,
		"agent":    agentName,
	}
}

func (s *Service) resolveProfile(id string) (Profile, error) {
	profile, ok := s.Profile(id)
	if !ok {
		return Profile{}, errors.New("unknown ACP agent profile")
	}
	return profile, nil
}

func normalizeProfiles(profiles []Profile) map[string]Profile {
	out := make(map[string]Profile, len(profiles))
	for _, profile := range profiles {
		normalized := normalizeProfile(profile)
		if normalized.ID == "" {
			continue
		}
		out[normalized.ID] = normalized
	}
	if len(out) == 0 {
		profile := CodexProfile()
		out[profile.ID] = profile
	}
	return out
}

func normalizeProfile(profile Profile) Profile {
	profile.ID = normalizeProfileID(profile.ID)
	profile.DisplayName = strings.TrimSpace(profile.DisplayName)
	profile.Command = strings.TrimSpace(profile.Command)
	profile.Args = append([]string(nil), profile.Args...)
	if profile.DisplayName == "" {
		profile.DisplayName = profile.ID
	}
	return profile
}

func cloneProfile(profile Profile) Profile {
	profile.Args = append([]string(nil), profile.Args...)
	return profile
}

func normalizeProfileID(id string) string {
	return strings.ToLower(strings.TrimSpace(id))
}
