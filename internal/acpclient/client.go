package acpclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/memohai/memoh/internal/workspace/bridge"
)

const (
	DefaultRunTimeout = 20 * time.Minute
)

var ErrUnsupportedWorkspace = errors.New("ACP agent requires a local workspace")

type Workspace interface {
	bridge.Provider
	bridge.WorkspaceInfoProvider
}

type Runner struct {
	logger    *slog.Logger
	workspace Workspace
	command   string
	args      []string
	timeout   time.Duration
}

type RunRequest struct {
	BotID       string
	Task        string
	ProjectPath string
	Command     string
	Args        []string
	Timeout     time.Duration
}

type RunResult struct {
	SessionID   string        `json:"session_id,omitempty"`
	ProjectPath string        `json:"project_path,omitempty"`
	Text        string        `json:"text,omitempty"`
	StopReason  string        `json:"stop_reason,omitempty"`
	ToolCalls   []ToolSummary `json:"tool_calls,omitempty"`
	Plan        []PlanItem    `json:"plan,omitempty"`
}

func NewRunner(log *slog.Logger, workspace Workspace) *Runner {
	if log == nil {
		log = slog.Default()
	}
	return &Runner{
		logger:    log.With(slog.String("component", "acpclient")),
		workspace: workspace,
		timeout:   DefaultRunTimeout,
	}
}

func (r *Runner) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	if r == nil || r.workspace == nil {
		return RunResult{}, errors.New("ACP workspace provider is not configured")
	}
	if strings.TrimSpace(req.BotID) == "" {
		return RunResult{}, errors.New("bot_id is required")
	}
	if strings.TrimSpace(req.Task) == "" {
		return RunResult{}, errors.New("task is required")
	}

	info, err := r.workspace.WorkspaceInfo(ctx, req.BotID)
	if err != nil {
		return RunResult{}, fmt.Errorf("resolve workspace: %w", err)
	}
	if !strings.EqualFold(info.Backend, bridge.WorkspaceBackendLocal) {
		return RunResult{}, fmt.Errorf("%w: backend %q is not supported in the first-stage ACP integration", ErrUnsupportedWorkspace, info.Backend)
	}
	root, err := resolveRoot(info.DefaultWorkDir)
	if err != nil {
		return RunResult{}, err
	}
	projectPath, err := ResolvePathUnderRoot(root, req.ProjectPath)
	if err != nil {
		return RunResult{}, fmt.Errorf("invalid project_path: %w", err)
	}

	client, err := r.workspace.MCPClient(ctx, req.BotID)
	if err != nil {
		return RunResult{}, fmt.Errorf("connect workspace bridge: %w", err)
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = r.timeout
	}
	if timeout <= 0 {
		timeout = DefaultRunTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	command := strings.TrimSpace(req.Command)
	args := append([]string(nil), req.Args...)
	if command == "" {
		command = strings.TrimSpace(r.command)
		if len(args) == 0 {
			args = append(args, r.args...)
		}
	}
	proc, err := startBridgeProcess(runCtx, client, command, args, projectPath, timeout)
	if err != nil {
		return RunResult{}, fmt.Errorf("start %s: %w", buildShellCommand(command, args), err)
	}
	defer func() { _ = proc.Close() }()

	collector := newEventCollector()
	callbacks := newClientCallbacks(runCtx, client, root, projectPath, timeout, collector)
	defer callbacks.close()

	result, err := runOverStdio(runCtx, proc, callbacks, projectPath, req.Task, r.logger)
	result.ProjectPath = projectPath
	if err != nil {
		return result, proc.errorWithStderr(err)
	}
	return result, nil
}

func runOverStdio(ctx context.Context, proc stdioProcess, callbacks *clientCallbacks, cwd, prompt string, log *slog.Logger) (RunResult, error) {
	conn := acp.NewClientSideConnection(callbacks, proc, proc)
	if log != nil {
		conn.SetLogger(log.With(slog.String("protocol", "acp")))
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
		return callbacks.result(), fmt.Errorf("initialize ACP agent: %w", err)
	}

	sess, err := conn.NewSession(ctx, acp.NewSessionRequest{
		Cwd:        cwd,
		McpServers: []acp.McpServer{},
	})
	if err != nil {
		return callbacks.result(), fmt.Errorf("create ACP session: %w", err)
	}

	resp, err := conn.Prompt(ctx, acp.PromptRequest{
		SessionId: sess.SessionId,
		Prompt:    []acp.ContentBlock{acp.TextBlock(prompt)},
	})
	result := callbacks.result()
	result.SessionID = string(sess.SessionId)
	if resp.StopReason != "" {
		result.StopReason = string(resp.StopReason)
	}
	if err != nil {
		return result, fmt.Errorf("send ACP prompt: %w", err)
	}
	return result, nil
}

type clientCallbacks struct {
	client    *bridge.Client
	root      string
	cwd       string
	collector *eventCollector
	sink      EventSink
	terminals *terminalManager
}

var _ acp.Client = (*clientCallbacks)(nil)

func newClientCallbacks(ctx context.Context, client *bridge.Client, root, cwd string, timeout time.Duration, collector *eventCollector) *clientCallbacks {
	return newClientCallbacksWithSink(ctx, client, root, cwd, timeout, collector, nil)
}

func newClientCallbacksWithSink(ctx context.Context, client *bridge.Client, root, cwd string, timeout time.Duration, collector *eventCollector, sink EventSink) *clientCallbacks {
	timeoutSeconds := int32(timeout.Seconds())
	if timeoutSeconds <= 0 {
		timeoutSeconds = defaultTerminalTimeout
	}
	return &clientCallbacks{
		client:    client,
		root:      root,
		cwd:       cwd,
		collector: collector,
		sink:      sink,
		terminals: newTerminalManager(ctx, client, root, cwd, timeoutSeconds),
	}
}

func (c *clientCallbacks) close() {
	if c != nil && c.terminals != nil {
		c.terminals.killAll()
	}
}

func (c *clientCallbacks) result() RunResult {
	if c == nil || c.collector == nil {
		return RunResult{}
	}
	return c.collector.result()
}

func (c *clientCallbacks) ReadTextFile(ctx context.Context, p acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	path, err := ResolvePathUnderRoot(c.root, p.Path)
	if err != nil {
		return acp.ReadTextFileResponse{}, err
	}
	line := int32(1)
	if p.Line != nil && *p.Line > 0 {
		line = boundedPositiveInt32(*p.Line)
	}
	limit := int32(0)
	if p.Limit != nil && *p.Limit > 0 {
		limit = boundedPositiveInt32(*p.Limit)
	}
	resp, err := c.client.ReadFile(ctx, path, line, limit)
	if err != nil {
		return acp.ReadTextFileResponse{}, err
	}
	if resp.GetBinary() {
		return acp.ReadTextFileResponse{}, fmt.Errorf("path %q is binary; ACP text file reads only support text", p.Path)
	}
	content := resp.GetContent()
	if content == "" {
		content = "\n"
	}
	return acp.ReadTextFileResponse{Content: content}, nil
}

func (c *clientCallbacks) WriteTextFile(ctx context.Context, p acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	path, err := ResolvePathUnderRoot(c.root, p.Path)
	if err != nil {
		return acp.WriteTextFileResponse{}, err
	}
	if err := c.client.WriteFile(ctx, path, []byte(p.Content)); err != nil {
		return acp.WriteTextFileResponse{}, err
	}
	return acp.WriteTextFileResponse{}, nil
}

func (c *clientCallbacks) RequestPermission(_ context.Context, p acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	if err := c.validatePermissionScope(p); err != nil {
		return cancelledPermission(), nil
	}
	for _, opt := range p.Options {
		if opt.Kind == acp.PermissionOptionKindAllowOnce {
			return selectedPermission(opt.OptionId), nil
		}
	}
	for _, opt := range p.Options {
		if opt.Kind == acp.PermissionOptionKindAllowAlways {
			return selectedPermission(opt.OptionId), nil
		}
	}
	return cancelledPermission(), nil
}

func (c *clientCallbacks) SessionUpdate(_ context.Context, p acp.SessionNotification) error {
	if c.collector != nil {
		c.collector.apply(p)
	}
	if c.sink != nil {
		for _, event := range streamEventsFromNotification(p) {
			c.sink.EmitACPEvent(event)
		}
	}
	return nil
}

func (c *clientCallbacks) CreateTerminal(ctx context.Context, p acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return c.terminals.CreateTerminal(ctx, p)
}

func (c *clientCallbacks) KillTerminal(ctx context.Context, p acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return c.terminals.KillTerminal(ctx, p)
}

func (c *clientCallbacks) TerminalOutput(ctx context.Context, p acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return c.terminals.TerminalOutput(ctx, p)
}

func (c *clientCallbacks) ReleaseTerminal(ctx context.Context, p acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return c.terminals.ReleaseTerminal(ctx, p)
}

func (c *clientCallbacks) WaitForTerminalExit(ctx context.Context, p acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return c.terminals.WaitForTerminalExit(ctx, p)
}

func (c *clientCallbacks) validatePermissionScope(p acp.RequestPermissionRequest) error {
	for _, loc := range p.ToolCall.Locations {
		if strings.TrimSpace(loc.Path) == "" {
			continue
		}
		if _, err := ResolvePathUnderRoot(c.root, loc.Path); err != nil {
			return err
		}
	}
	if raw, ok := p.ToolCall.RawInput.(map[string]any); ok {
		for _, key := range []string{"cwd", "work_dir", "path", "old_path", "new_path"} {
			value, ok := raw[key].(string)
			if !ok || strings.TrimSpace(value) == "" {
				continue
			}
			if _, err := ResolvePathUnderRoot(c.root, value); err != nil {
				return err
			}
		}
	}
	return nil
}

func selectedPermission(id acp.PermissionOptionId) acp.RequestPermissionResponse {
	return acp.RequestPermissionResponse{
		Outcome: acp.RequestPermissionOutcome{
			Selected: &acp.RequestPermissionOutcomeSelected{OptionId: id},
		},
	}
}

func cancelledPermission() acp.RequestPermissionResponse {
	return acp.RequestPermissionResponse{
		Outcome: acp.RequestPermissionOutcome{
			Cancelled: &acp.RequestPermissionOutcomeCancelled{},
		},
	}
}

func boundedPositiveInt32(v int) int32 {
	const maxInt32 = int(^uint32(0) >> 1)
	if v <= 0 {
		return 0
	}
	if v > maxInt32 {
		return int32(maxInt32) //nolint:gosec // maxInt32 is exactly the largest int32 value.
	}
	return int32(v) //nolint:gosec // v is bounded to the int32 range above.
}
