package acpclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/google/uuid"

	"github.com/memohai/memoh/internal/mcp"
	"github.com/memohai/memoh/internal/toolapproval"
	"github.com/memohai/memoh/internal/workspace/bridge"
)

const (
	DefaultRunTimeout          = 20 * time.Minute
	maxWriteToolContentPreview = 64 * 1024
	// approvalGrantTTL bounds how long a RequestPermission grant stays
	// consumable by the follow-up client-capability callback. Deliberately its
	// own constant: it is unrelated to how long the approval flow waits for a
	// user decision, even though both happen to be generous.
	approvalGrantTTL = 10 * time.Minute
)

type Workspace interface {
	bridge.Provider
	bridge.WorkspaceInfoProvider
}

type ToolApprovalService interface {
	EvaluatePolicy(ctx context.Context, input toolapproval.CreatePendingInput) (toolapproval.Evaluation, error)
	CreatePending(ctx context.Context, input toolapproval.CreatePendingInput) (toolapproval.Request, error)
	Reject(ctx context.Context, approvalID, actorID, reason string) (toolapproval.Request, error)
	WaitForDecision(ctx context.Context, approvalID string) (toolapproval.Request, error)
}

type Runner struct {
	logger    *slog.Logger
	workspace Workspace
	command   string
	args      []string
	timeout   time.Duration
}

type RunRequest struct {
	AgentID      string
	BotID        string
	Task         string
	ProjectPath  string
	Command      string
	Args         []string
	LocalCommand string
	LocalArgs    []string
	Env          []string
	SetupMode    SetupMode
	Timeout      time.Duration
}

type RunResult struct {
	SessionID   string        `json:"session_id,omitempty"`
	ProjectPath string        `json:"project_path,omitempty"`
	Text        string        `json:"text,omitempty"`
	StopReason  string        `json:"stop_reason,omitempty"`
	Events      []StreamEvent `json:"events,omitempty"`
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

func (r *Runner) WorkspaceInfo(ctx context.Context, botID string) (bridge.WorkspaceInfo, error) {
	if r == nil || r.workspace == nil {
		return bridge.WorkspaceInfo{}, errors.New("ACP workspace provider is not configured")
	}
	return r.workspace.WorkspaceInfo(ctx, botID)
}

func (r *Runner) MCPClient(ctx context.Context, botID string) (*bridge.Client, error) {
	if r == nil || r.workspace == nil {
		return nil, errors.New("ACP workspace provider is not configured")
	}
	return r.workspace.MCPClient(ctx, botID)
}

// Run is a convenience wrapper that performs a single-shot ACP exchange:
// start a session, send one prompt, then close. Production code that needs a
// persistent session should use StartSession + (*Session).Prompt directly.
//
// (*Session).Close uses its own short-lived background context so cleanup
// always runs even if the caller's ctx was cancelled; that disconnect trips
// contextcheck, so we silence it here.
//
//nolint:contextcheck // lifecycle close intentionally uses background ctx.
func (r *Runner) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	if strings.TrimSpace(req.Task) == "" {
		return RunResult{}, errors.New("task is required")
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

	sess, err := r.StartSession(runCtx, StartRequest{
		AgentID:      req.AgentID,
		BotID:        req.BotID,
		ProjectPath:  req.ProjectPath,
		Command:      req.Command,
		Args:         req.Args,
		LocalCommand: req.LocalCommand,
		LocalArgs:    req.LocalArgs,
		Env:          req.Env,
		SetupMode:    req.SetupMode,
		Timeout:      timeout,
	}, nil)
	if err != nil {
		return RunResult{}, err
	}
	defer func() { _ = sess.Close() }()

	prompt, err := sess.Prompt(runCtx, req.Task)
	result := RunResult{
		SessionID:   sess.ID(),
		ProjectPath: sess.ProjectPath(),
		Text:        prompt.Text,
		StopReason:  prompt.StopReason,
		Events:      prompt.Events,
	}
	if err != nil {
		return result, err
	}
	return result, nil
}

func resolveWorkspacePaths(info bridge.WorkspaceInfo, rawProjectPath string) (string, string, WorkspaceBackend, error) {
	backend := WorkspaceBackendContainer
	if strings.EqualFold(info.Backend, bridge.WorkspaceBackendLocal) {
		backend = WorkspaceBackendLocal
	}
	root := strings.TrimSpace(info.DefaultWorkDir)
	if root == "" {
		root = dataMountPath
	}
	if backend == WorkspaceBackendLocal {
		resolvedRoot, err := resolveRoot(root)
		if err != nil {
			return "", "", backend, err
		}
		projectPath, err := ResolvePathUnderRoot(resolvedRoot, rawProjectPath)
		return resolvedRoot, projectPath, backend, err
	}
	root = dataMountPath
	projectPath, err := ResolvePathUnderVirtualRoot(root, rawProjectPath)
	return root, projectPath, backend, err
}

type clientCallbacks struct {
	client         *bridge.Client
	logger         *slog.Logger
	root           string
	cwd            string
	virtualRoot    bool
	approval       ToolApprovalService
	baseSession    ToolSessionContext
	mu             sync.RWMutex
	collector      *eventCollector
	sink           EventSink
	promptSession  ToolSessionContext
	approvalGrants map[string]approvedToolGrant
	events         *toolEventEmitter
	toolMapper     *acpToolEventMapper
	terminals      *terminalManager
}

type approvedToolGrant struct {
	ToolCallID string
	ExpiresAt  time.Time
}

func newClientCallbacks(ctx context.Context, client *bridge.Client, root, cwd string, timeout time.Duration, sink EventSink, env []string, virtualRoot bool, approval ToolApprovalService, toolSession ToolSessionContext) *clientCallbacks {
	timeoutSeconds := int32(timeout.Seconds())
	if timeoutSeconds <= 0 {
		timeoutSeconds = defaultTerminalTimeout
	}
	events := &toolEventEmitter{}
	return &clientCallbacks{
		client:      client,
		root:        root,
		cwd:         cwd,
		virtualRoot: virtualRoot,
		approval:    approval,
		baseSession: toolSession,
		sink:        sink,
		events:      events,
		toolMapper:  newACPToolEventMapper(),
		terminals:   newTerminalManager(ctx, client, root, cwd, timeoutSeconds, env, virtualRoot, events),
	}
}

func (c *clientCallbacks) close() {
	if c != nil && c.terminals != nil {
		c.terminals.killAll()
	}
}

func (c *clientCallbacks) setPromptState(collector *eventCollector, sink EventSink, toolSession ToolSessionContext) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.collector = collector
	c.sink = sink
	c.promptSession = toolSession
	c.approvalGrants = nil
	c.mu.Unlock()
	if collector != nil && c.toolMapper != nil {
		// A new prompt is starting: drop tool state left by the previous
		// (possibly cancelled) turn so trailing agent notifications cannot
		// attribute to this turn's tools.
		c.toolMapper.reset()
	}
	if c.events != nil {
		c.events.setPromptState(collector, sink)
	}
}

func (c *clientCallbacks) ReadTextFile(ctx context.Context, p acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	toolID := "read-" + uuid.NewString()
	input := map[string]any{"path": p.Path}
	if p.Line != nil && *p.Line > 0 {
		input["line"] = *p.Line
	}
	if p.Limit != nil && *p.Limit > 0 {
		input["limit"] = *p.Limit
	}
	toolID, approval, err := c.approveCallbackTool(ctx, toolID, "read", input)
	if err != nil {
		return acp.ReadTextFileResponse{}, err
	}
	if !approval.Approved {
		err := errors.New(toolapproval.RejectionMessage(approval))
		c.emitToolCallEnd(toolID, "read", input, toolErrorResult(err), err)
		return acp.ReadTextFileResponse{}, err
	}
	c.emitToolCallStart(toolID, "read", input)
	var toolErr error
	defer func() {
		result := map[string]any{}
		if toolErr != nil {
			result = toolErrorResult(toolErr)
		}
		c.emitToolCallEnd(toolID, "read", input, result, toolErr)
	}()

	path, err := c.resolvePath(p.Path)
	if err != nil {
		toolErr = err
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
		toolErr = err
		return acp.ReadTextFileResponse{}, err
	}
	if resp.GetBinary() {
		toolErr = fmt.Errorf("path %q is binary; ACP text file reads only support text", p.Path)
		return acp.ReadTextFileResponse{}, toolErr
	}
	content := resp.GetContent()
	if content == "" {
		content = "\n"
	}
	return acp.ReadTextFileResponse{Content: content}, nil
}

func (c *clientCallbacks) WriteTextFile(ctx context.Context, p acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	toolID := "write-" + uuid.NewString()
	input := writeToolInput(p.Path, p.Content)
	toolID, approval, err := c.approveCallbackTool(ctx, toolID, "write", input)
	if err != nil {
		return acp.WriteTextFileResponse{}, err
	}
	if !approval.Approved {
		err := errors.New(toolapproval.RejectionMessage(approval))
		c.emitToolCallEnd(toolID, "write", input, toolErrorResult(err), err)
		return acp.WriteTextFileResponse{}, err
	}
	c.emitToolCallStart(toolID, "write", input)
	var toolErr error
	defer func() {
		result := map[string]any{}
		if toolErr != nil {
			result = toolErrorResult(toolErr)
		}
		c.emitToolCallEnd(toolID, "write", input, result, toolErr)
	}()

	path, err := c.resolvePath(p.Path)
	if err != nil {
		toolErr = err
		return acp.WriteTextFileResponse{}, err
	}
	if err := c.client.WriteFile(ctx, path, []byte(p.Content)); err != nil {
		toolErr = err
		return acp.WriteTextFileResponse{}, err
	}
	return acp.WriteTextFileResponse{}, nil
}

func writeToolInput(path, content string) map[string]any {
	contentBytes := len(content)
	input := map[string]any{
		"path":               path,
		"content":            content,
		"content_bytes":      contentBytes,
		"content_line_count": lineCount(content),
	}
	if contentBytes <= maxWriteToolContentPreview {
		return input
	}
	sum := sha256.Sum256([]byte(content))
	preview := strings.ToValidUTF8(content[:maxWriteToolContentPreview], "")
	input["content"] = preview
	input["content_sha256"] = hex.EncodeToString(sum[:])
	input["content_truncated"] = true
	return input
}

func lineCount(value string) int {
	if value == "" {
		return 0
	}
	return strings.Count(value, "\n") + 1
}

func (c *clientCallbacks) emitToolCallStart(id, name string, input map[string]any) {
	if c == nil || c.events == nil {
		return
	}
	c.events.emit(StreamEvent{
		Type:       StreamEventToolCallStart,
		ToolCallID: id,
		ToolName:   name,
		Input:      input,
	})
}

func (c *clientCallbacks) emitToolCallEnd(id, name string, input map[string]any, result any, err error) {
	if c == nil || c.events == nil {
		return
	}
	event := StreamEvent{
		Type:       StreamEventToolCallEnd,
		ToolCallID: id,
		ToolName:   name,
		Input:      input,
		Result:     result,
	}
	if err != nil {
		event.Error = err.Error()
	}
	c.events.emit(event)
}

func toolErrorResult(err error) map[string]any {
	message := ""
	if err != nil {
		message = err.Error()
	}
	return map[string]any{
		"isError": true,
		"content": []map[string]any{{
			"type": "text",
			"text": message,
		}},
	}
}

func (c *clientCallbacks) RequestPermission(ctx context.Context, p acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	// ACP permissions stay scoped to the active prompt. When an ACP agent asks
	// permission before calling a client capability like fs/write_text_file, the
	// callback consumes this one-shot grant so users see one approval, not two.
	if c == nil {
		return cancelledPermission(), nil
	}
	if err := c.validatePermissionScope(p); err != nil {
		// Security-relevant rejection: an agent asked to act outside the
		// workspace root. Log it (an agent probing the boundary is exactly what
		// we want visibility into) before cancelling.
		if c.logger != nil {
			c.logger.Warn("cancelling out-of-scope ACP permission request", slog.Any("error", err))
		}
		return cancelledPermission(), nil
	}
	if c.approval == nil {
		return allowOncePermission(p), nil
	}
	toolCallID, toolName, input, native := permissionNativeTool(p)
	if !native {
		// Permissions that don't map onto a client-capability tool (plan-mode
		// switches, agent-specific actions) still go through Memoh approval
		// under a generic name instead of being silently cancelled. The
		// generic name means nothing to the per-tool policy, so the flow is
		// forced to ask the user — bypassing here would be fail-open.
		toolCallID, toolName, input = genericPermissionTool(p)
		if c.logger != nil {
			c.logger.Info("routing unmapped ACP permission request through generic approval",
				slog.String("tool_call_id", toolCallID),
				slog.String("tool_name", toolName),
				slog.String("title", stringFromAny(input["title"])))
		}
	}
	result, err := c.requireToolApproval(ctx, toolCallID, toolName, input, !native)
	if err != nil {
		return acp.RequestPermissionResponse{}, err
	}
	if !result.Approved {
		if result.DecidedByUser {
			// A live user said no: select the agent's reject option so the
			// turn continues with a clean refusal (for ExitPlanMode that is
			// "keep planning").
			return rejectOncePermission(p), nil
		}
		// System outcomes (no session identity, non-interactive auto-reject)
		// cancel the request: there was no user decision to report.
		return cancelledPermission(), nil
	}
	resp := allowOncePermission(p)
	if resp.Outcome.Cancelled != nil {
		// The user approved but the agent offered no allow_once option, so the
		// agent sees a cancellation and will not act — leaving a consumable
		// grant behind would let a later callback run on a permission the
		// agent believes was cancelled.
		if c.logger != nil {
			c.logger.Warn("approval granted but the agent offered no allow_once option; cancelling",
				slog.String("tool_call_id", toolCallID),
				slog.String("tool_name", toolName))
		}
		return resp, nil
	}
	if native {
		// One-shot grants only make sense for tools that arrive again as a
		// client-capability callback; generic permissions have no follow-up.
		c.rememberApprovalGrant(toolCallID, toolName, input)
	}
	return resp, nil
}

const genericPermissionToolName = "acp_permission"

// genericPermissionTool describes an unmappable ACP permission request as a
// generic approvable tool, preserving the agent's title/kind/raw input so the
// user can still judge what they are approving. The name is namespaced by the
// agent's tool kind (acp_permission:switch_mode, acp_permission:fetch, ...)
// so different action classes stay distinguishable in the UI and in future
// policy rules. Callers must pair this with ForceApproval: the per-tool
// policy does not know these synthetic names and would otherwise bypass them.
func genericPermissionTool(p acp.RequestPermissionRequest) (string, string, map[string]any) {
	toolCallID := strings.TrimSpace(string(p.ToolCall.ToolCallId))
	if toolCallID == "" {
		toolCallID = "acp-permission-" + uuid.NewString()
	}
	toolName := genericPermissionToolName
	input := map[string]any{}
	if p.ToolCall.Title != nil {
		if title := strings.TrimSpace(*p.ToolCall.Title); title != "" {
			input["title"] = title
		}
	}
	if p.ToolCall.Kind != nil {
		if kind := strings.TrimSpace(string(*p.ToolCall.Kind)); kind != "" {
			input["kind"] = kind
			toolName += ":" + kind
		}
	}
	if raw, ok := p.ToolCall.RawInput.(map[string]any); ok && len(raw) > 0 {
		input["raw_input"] = raw
	}
	return toolCallID, toolName, input
}

func allowOncePermission(p acp.RequestPermissionRequest) acp.RequestPermissionResponse {
	for _, opt := range p.Options {
		if opt.Kind == acp.PermissionOptionKindAllowOnce {
			return selectedPermission(opt.OptionId)
		}
	}
	return cancelledPermission()
}

// rejectOncePermission selects the agent's reject_once option so a denied
// approval reads as a clean user rejection (for ExitPlanMode that is "keep
// planning") instead of an aborted turn; cancellation is the fallback.
func rejectOncePermission(p acp.RequestPermissionRequest) acp.RequestPermissionResponse {
	for _, opt := range p.Options {
		if opt.Kind == acp.PermissionOptionKindRejectOnce {
			return selectedPermission(opt.OptionId)
		}
	}
	return cancelledPermission()
}

// approveCallbackTool resolves the approval for a client-capability tool call,
// consuming the one-shot grant left by RequestPermission when one matches. It
// returns the tool call ID the events should be emitted under.
func (c *clientCallbacks) approveCallbackTool(ctx context.Context, fallbackToolCallID, toolName string, input map[string]any) (string, toolapproval.FlowResult, error) {
	fallbackToolCallID = strings.TrimSpace(fallbackToolCallID)
	if fallbackToolCallID == "" {
		fallbackToolCallID = "acp-callback-" + uuid.NewString()
	}
	if grantedID, ok := c.consumeApprovalGrant(toolName, input); ok {
		// The grant exists because the user approved the matching
		// RequestPermission moments ago.
		return grantedID, toolapproval.FlowResult{
			Approved:      true,
			Status:        toolapproval.StatusApproved,
			DecidedByUser: true,
		}, nil
	}
	result, err := c.requireToolApproval(ctx, fallbackToolCallID, toolName, input, false)
	return fallbackToolCallID, result, err
}

func (c *clientCallbacks) requireToolApproval(ctx context.Context, toolCallID, toolName string, input map[string]any, forceApproval bool) (toolapproval.FlowResult, error) {
	if c == nil || c.approval == nil {
		return toolapproval.FlowResult{Approved: true}, nil
	}
	session := c.currentToolSession()
	if strings.TrimSpace(session.BotID) == "" || strings.TrimSpace(session.SessionID) == "" {
		return toolapproval.FlowResult{}, nil
	}
	return toolapproval.RunFlow(ctx, c.approval, toolapproval.FlowRequest{
		Input: toolapproval.CreatePendingInput{
			BotID:                        session.BotID,
			SessionID:                    session.SessionID,
			RouteID:                      session.RouteID,
			ChannelIdentityID:            session.ChannelIdentityID,
			RequestedByChannelIdentityID: session.ChannelIdentityID,
			ToolCallID:                   toolCallID,
			ToolName:                     toolName,
			ToolInput:                    input,
			SourcePlatform:               session.CurrentPlatform,
			ReplyTarget:                  session.ReplyTarget,
			ConversationType:             session.ConversationType,
		},
		Interactive:   strings.TrimSpace(session.StreamID) != "",
		ForceApproval: forceApproval,
		Emit:          c.emitToolApprovalRequest,
	})
}

func (c *clientCallbacks) rememberApprovalGrant(toolCallID, toolName string, input map[string]any) {
	if c == nil {
		return
	}
	key := approvalGrantKey(toolName, input)
	if key == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.approvalGrants == nil {
		c.approvalGrants = map[string]approvedToolGrant{}
	}
	c.approvalGrants[key] = approvedToolGrant{
		ToolCallID: strings.TrimSpace(toolCallID),
		ExpiresAt:  time.Now().Add(approvalGrantTTL),
	}
}

func (c *clientCallbacks) consumeApprovalGrant(toolName string, input map[string]any) (string, bool) {
	if c == nil {
		return "", false
	}
	key := approvalGrantKey(toolName, input)
	if key == "" {
		return "", false
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	grant, ok := c.approvalGrants[key]
	if !ok {
		return "", false
	}
	delete(c.approvalGrants, key)
	if !grant.ExpiresAt.IsZero() && now.After(grant.ExpiresAt) {
		return "", false
	}
	if strings.TrimSpace(grant.ToolCallID) == "" {
		return "", false
	}
	return grant.ToolCallID, true
}

func approvalGrantKey(toolName string, input map[string]any) string {
	toolName = strings.TrimSpace(toolName)
	if toolName == "" {
		return ""
	}
	normalized := map[string]any{}
	switch toolName {
	case "read":
		normalized["path"] = stringFromAny(input["path"])
	case "write":
		normalized["path"] = stringFromAny(input["path"])
		normalized["content"] = stringFromAny(input["content"])
		normalized["content_bytes"] = input["content_bytes"]
		normalized["content_sha256"] = stringFromAny(input["content_sha256"])
	case "edit":
		normalized["path"] = stringFromAny(input["path"])
		normalized["old_text"] = stringFromAny(input["old_text"])
		normalized["new_text"] = stringFromAny(input["new_text"])
	case "exec":
		// Permission-time input carries only the agent's raw command, while
		// terminal/create rebuilds it from Command+Args and may add a cwd the
		// permission request never mentioned. Key on the whitespace-normalized
		// command alone so the one-shot grant still matches.
		normalized["command"] = strings.Join(strings.Fields(stringFromAny(input["command"])), " ")
	default:
		for k, v := range input {
			normalized[k] = v
		}
	}
	raw, err := json.Marshal(normalized)
	if err != nil {
		return ""
	}
	return toolName + ":" + string(raw)
}

func (c *clientCallbacks) currentToolSession() ToolSessionContext {
	if c == nil {
		return ToolSessionContext{}
	}
	c.mu.RLock()
	base := c.baseSession
	prompt := c.promptSession
	c.mu.RUnlock()
	return mcp.MergeToolSessionContext(base, prompt)
}

func permissionNativeTool(p acp.RequestPermissionRequest) (toolCallID, toolName string, input map[string]any, ok bool) {
	toolCallID = strings.TrimSpace(string(p.ToolCall.ToolCallId))
	if toolCallID == "" {
		toolCallID = "acp-permission-" + uuid.NewString()
	}
	state := &acpToolState{
		id:        toolCallID,
		input:     p.ToolCall.RawInput,
		output:    p.ToolCall.RawOutput,
		locations: append([]acp.ToolCallLocation(nil), p.ToolCall.Locations...),
		content:   append([]acp.ToolCallContent(nil), p.ToolCall.Content...),
	}
	if p.ToolCall.Title != nil {
		state.title = strings.TrimSpace(*p.ToolCall.Title)
	}
	if p.ToolCall.Kind != nil {
		state.kind = strings.TrimSpace(string(*p.ToolCall.Kind))
	}
	if p.ToolCall.Status != nil {
		state.status = strings.TrimSpace(string(*p.ToolCall.Status))
	}
	// nativeToolFromACPState now applies the edit→write title reclassification
	// itself, so the approval name here always matches the streamed tool-event
	// name for the same call.
	toolName, input, ok = nativeToolFromACPState(state)
	if !ok {
		return "", "", nil, false
	}
	return toolCallID, toolName, input, true
}

func looksLikeWritePermission(title string) bool {
	title = strings.ToLower(strings.TrimSpace(title))
	return strings.Contains(title, "write") ||
		strings.Contains(title, "create") ||
		strings.Contains(title, "new file")
}

func (c *clientCallbacks) emitToolApprovalRequest(req toolapproval.Request) {
	if c == nil || c.events == nil {
		return
	}
	c.events.emit(StreamEvent{
		Type:       StreamEventToolApprovalRequest,
		ToolCallID: req.ToolCallID,
		ToolName:   req.ToolName,
		Input:      req.ToolInput,
		ApprovalID: req.ID,
		ShortID:    req.ShortID,
		Status:     toolapproval.NormalizedStatus(req.Status),
		Metadata: map[string]any{
			"approval": toolapproval.RequestMetadata(req),
		},
	})
}

func (c *clientCallbacks) SessionUpdate(_ context.Context, p acp.SessionNotification) error {
	c.mu.RLock()
	collector := c.collector
	sink := c.sink
	c.mu.RUnlock()
	var events []StreamEvent
	if c.toolMapper != nil {
		events = c.toolMapper.eventsFromNotification(p)
	}
	if collector != nil {
		collector.apply(p, events)
	}
	if sink != nil {
		for _, event := range events {
			sink.EmitACPEvent(event)
		}
	}
	return nil
}

func (c *clientCallbacks) CreateTerminal(ctx context.Context, p acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return c.terminals.CreateTerminal(ctx, p, func(toolCallID string, input map[string]any) (terminalApprovalResult, error) {
		id, approval, err := c.approveCallbackTool(ctx, toolCallID, "exec", input)
		return terminalApprovalResult{
			Approved:         approval.Approved,
			ToolCallID:       id,
			RejectionMessage: toolapproval.RejectionMessage(approval),
		}, err
	})
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

func (c *clientCallbacks) resolvePath(path string) (string, error) {
	if c.virtualRoot {
		return ResolvePathUnderVirtualRoot(c.root, path)
	}
	return ResolvePathUnderRoot(c.root, path)
}

// scopePathKeys is the union of every RawInput key any extraction layer reads
// a path from (pathFromACPInput plus the callback layer's cwd/old/new keys).
// The scope guard must validate at least the projection that later gets
// approved and executed: a key read by extraction but not validated here
// would let an out-of-root path through the pre-ask gate, and a thick agent
// executes the approved action itself — there is no later callback to catch
// it.
var scopePathKeys = []string{
	"cwd", "work_dir",
	"path", "file_path", "filePath", "file", "filename",
	"old_path", "new_path",
}

func (c *clientCallbacks) validatePermissionScope(p acp.RequestPermissionRequest) error {
	for _, loc := range p.ToolCall.Locations {
		if strings.TrimSpace(loc.Path) == "" {
			continue
		}
		if _, err := c.resolvePath(loc.Path); err != nil {
			return err
		}
	}
	if raw, ok := p.ToolCall.RawInput.(map[string]any); ok {
		for _, key := range scopePathKeys {
			value, ok := raw[key].(string)
			if !ok || strings.TrimSpace(value) == "" {
				continue
			}
			if _, err := c.resolvePath(value); err != nil {
				return err
			}
		}
	}
	// Edit permissions can carry their target path only inside a content
	// diff (editToolFromACPState falls back to it), so validate those too.
	for _, content := range p.ToolCall.Content {
		if content.Diff == nil || strings.TrimSpace(content.Diff.Path) == "" {
			continue
		}
		if _, err := c.resolvePath(content.Diff.Path); err != nil {
			return err
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
