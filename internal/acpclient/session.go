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

	"github.com/memohai/memoh/internal/workspace/bridge"
)

type StartRequest struct {
	BotID       string
	ProjectPath string
	Command     string
	Args        []string
	Timeout     time.Duration
}

type PromptResult struct {
	StopReason string
}

type Session struct {
	logger      *slog.Logger
	proc        *bridgeProcess
	callbacks   *clientCallbacks
	conn        *acp.ClientSideConnection
	sessionID   acp.SessionId
	projectPath string
	cancel      context.CancelFunc

	mu     sync.Mutex
	closed bool
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
	if !strings.EqualFold(info.Backend, bridge.WorkspaceBackendLocal) {
		return nil, fmt.Errorf("%w: backend %q is not supported in the first-stage ACP integration", ErrUnsupportedWorkspace, info.Backend)
	}
	root, err := resolveRoot(info.DefaultWorkDir)
	if err != nil {
		return nil, err
	}
	projectPath, err := ResolvePathUnderRoot(root, req.ProjectPath)
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

	lifecycleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	command := strings.TrimSpace(req.Command)
	args := append([]string(nil), req.Args...)
	if command == "" {
		command = strings.TrimSpace(r.command)
		if len(args) == 0 {
			args = append(args, r.args...)
		}
	}
	if command == "" {
		command = DefaultACPCommand
		if len(args) == 0 {
			args = append(args, DefaultACPArgs...)
		}
	}

	proc, err := startBridgeProcess(lifecycleCtx, client, command, args, projectPath, timeout)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("start %s: %w", buildShellCommand(command, args), err)
	}

	callbacks := newClientCallbacksWithSink(lifecycleCtx, client, root, projectPath, timeout, nil, sink)
	conn := acp.NewClientSideConnection(callbacks, proc, proc)
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

	return &Session{
		logger:      r.logger,
		proc:        proc,
		callbacks:   callbacks,
		conn:        conn,
		sessionID:   sess.SessionId,
		projectPath: projectPath,
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

func (s *Session) Prompt(ctx context.Context, prompt string) (PromptResult, error) {
	if s == nil || s.conn == nil {
		return PromptResult{}, errors.New("ACP session is not initialized")
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return PromptResult{}, errors.New("prompt is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return PromptResult{}, errors.New("ACP session is closed")
	}

	resp, err := s.conn.Prompt(ctx, acp.PromptRequest{
		SessionId: s.sessionID,
		Prompt:    []acp.ContentBlock{acp.TextBlock(prompt)},
	})
	result := PromptResult{StopReason: string(resp.StopReason)}
	if err != nil {
		if s.proc != nil {
			return result, s.proc.errorWithStderr(fmt.Errorf("send ACP prompt: %w", err))
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
	s.mu.Unlock()

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
