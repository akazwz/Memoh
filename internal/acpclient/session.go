package acpclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

type StartRequest struct {
	AgentID      string
	BotID        string
	ProjectPath  string
	Command      string
	Args         []string
	LocalCommand string
	LocalArgs    []string
	Env          []string
	SetupMode    SetupMode
	Timeout      time.Duration
}

type PromptResult struct {
	StopReason string        `json:"stop_reason,omitempty"`
	Text       string        `json:"text,omitempty"`
	ToolCalls  []ToolSummary `json:"tool_calls,omitempty"`
	Plan       []PlanItem    `json:"plan,omitempty"`
	Events     []StreamEvent `json:"events,omitempty"`
}

type Session struct {
	logger      *slog.Logger
	proc        *bridgeProcess
	callbacks   *clientCallbacks
	conn        *clientConnection
	sessionID   acp.SessionId
	projectPath string
	modelState  ModelState
	defaultSink EventSink
	cancel      context.CancelFunc

	promptMu     sync.Mutex
	mu           sync.Mutex
	promptCancel context.CancelFunc
	promptToken  *struct{}
	closed       bool
}

func (r *Runner) StartSession(ctx context.Context, req StartRequest, sink EventSink) (*Session, error) {
	if r == nil || r.workspace == nil {
		return nil, errors.New("ACP workspace provider is not configured")
	}
	if strings.TrimSpace(req.BotID) == "" {
		return nil, errors.New("bot_id is required")
	}

	info, err := r.workspace.WorkspaceInfo(ctx, req.BotID)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace: %w", err)
	}
	root, projectPath, backend, err := resolveWorkspacePaths(info, req.ProjectPath)
	if err != nil {
		return nil, fmt.Errorf("invalid project_path: %w", err)
	}

	client, err := r.workspace.MCPClient(ctx, req.BotID)
	if err != nil {
		return nil, fmt.Errorf("connect workspace bridge: %w", err)
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = r.timeout
	}
	if timeout <= 0 {
		timeout = DefaultRunTimeout
	}

	lifecycleCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	startupDone := make(chan struct{})
	var startupDoneOnce sync.Once
	finishStartup := func() {
		startupDoneOnce.Do(func() {
			close(startupDone)
		})
	}
	defer finishStartup()
	go func() {
		select {
		case <-ctx.Done():
			cancel()
		case <-startupDone:
		}
	}()
	command := strings.TrimSpace(req.Command)
	args := append([]string(nil), req.Args...)
	if backend == WorkspaceBackendLocal && strings.TrimSpace(req.LocalCommand) != "" {
		command = strings.TrimSpace(req.LocalCommand)
		args = append([]string(nil), req.LocalArgs...)
	}
	if command == "" {
		command = strings.TrimSpace(r.command)
		if len(args) == 0 {
			args = append(args, r.args...)
		}
	}

	proc, err := startBridgeProcess(lifecycleCtx, client, command, args, projectPath, timeout, processOptions{
		Backend:   backend,
		AgentID:   req.AgentID,
		SetupMode: req.SetupMode,
		Env:       req.Env,
		NoTimeout: true,
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("start %s: %w", buildShellCommand(command, args), err)
	}

	callbacks := newClientCallbacks(lifecycleCtx, client, root, projectPath, timeout, sink, proc.env, backend == WorkspaceBackendContainer)
	conn := newClientConnection(callbacks, proc, proc)
	if r.logger != nil {
		conn.SetLogger(r.logger.With(slog.String("protocol", "acp")))
	}

	if _, err := conn.Initialize(ctx, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
		ClientInfo:      &acp.Implementation{Name: "memoh", Version: "dev"},
		ClientCapabilities: acp.ClientCapabilities{
			Fs: acp.FileSystemCapabilities{
				ReadTextFile:  true,
				WriteTextFile: true,
			},
			Terminal: true,
		},
	}); err != nil {
		callbacks.close()
		_ = proc.Close()
		cancel()
		return nil, fmt.Errorf("initialize ACP agent: %w", err)
	}

	sess, err := conn.NewSession(ctx, acp.NewSessionRequest{
		Cwd:        projectPath,
		McpServers: []acp.McpServer{},
	})
	if err != nil {
		callbacks.close()
		_ = proc.Close()
		cancel()
		return nil, fmt.Errorf("create ACP session: %w", err)
	}

	finishStartup()
	return &Session{
		logger:      r.logger,
		proc:        proc,
		callbacks:   callbacks,
		conn:        conn,
		sessionID:   sess.SessionId,
		projectPath: projectPath,
		modelState:  modelStateFromACP(sess.Models),
		defaultSink: sink,
		cancel:      cancel,
	}, nil
}

func (s *Session) ID() string {
	if s == nil {
		return ""
	}
	return string(s.sessionID)
}

func (s *Session) ProjectPath() string {
	if s == nil {
		return ""
	}
	return s.projectPath
}

func (s *Session) Prompt(ctx context.Context, prompt string, sinks ...EventSink) (PromptResult, error) {
	if s == nil || s.conn == nil {
		return PromptResult{}, ErrSessionNotInitialized
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return PromptResult{}, ErrPromptRequired
	}

	s.promptMu.Lock()
	defer s.promptMu.Unlock()

	promptCtx, cancelPrompt := context.WithCancel(ctx)
	defer cancelPrompt()
	promptToken := &struct{}{}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return PromptResult{}, ErrSessionClosed
	}
	s.promptCancel = cancelPrompt
	s.promptToken = promptToken
	conn := s.conn
	sessionID := s.sessionID
	callbacks := s.callbacks
	proc := s.proc
	defaultSink := s.defaultSink
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.promptToken == promptToken {
			s.promptCancel = nil
			s.promptToken = nil
		}
		s.mu.Unlock()
	}()
	if conn == nil {
		return PromptResult{}, ErrSessionNotInitialized
	}

	collector := newEventCollector()
	sink := defaultSink
	if len(sinks) > 0 {
		sink = sinks[0]
	}
	if callbacks != nil {
		callbacks.setPromptState(collector, sink)
	}
	defer func() {
		if callbacks != nil {
			callbacks.setPromptState(nil, nil)
		}
	}()

	resp, err := conn.Prompt(promptCtx, acp.PromptRequest{
		SessionId: sessionID,
		Prompt:    []acp.ContentBlock{acp.TextBlock(prompt)},
	})
	collected := collector.result()
	result := PromptResult{
		StopReason: string(resp.StopReason),
		Text:       collected.Text,
		ToolCalls:  collected.ToolCalls,
		Plan:       collected.Plan,
		Events:     collected.Events,
	}
	if err != nil {
		if proc != nil {
			return result, proc.errorWithStderr(fmt.Errorf("send ACP prompt: %w", err))
		}
		return result, fmt.Errorf("send ACP prompt: %w", err)
	}
	return result, nil
}

func (s *Session) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	conn := s.conn
	sessionID := s.sessionID
	callbacks := s.callbacks
	proc := s.proc
	cancel := s.cancel
	promptCancel := s.promptCancel
	s.mu.Unlock()

	if promptCancel != nil {
		promptCancel()
	}
	if conn != nil && sessionID != "" {
		ctx, cancelClose := context.WithTimeout(context.Background(), 2*time.Second)
		_, _ = conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: sessionID})
		cancelClose()
	}
	if callbacks != nil {
		callbacks.close()
	}
	if cancel != nil {
		cancel()
	}
	if proc != nil {
		return proc.Close()
	}
	return nil
}
