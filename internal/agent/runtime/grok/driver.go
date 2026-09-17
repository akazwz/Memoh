package grok

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/google/uuid"

	"github.com/felinics/memoh/internal/agent/decision/approval"
	feedback "github.com/felinics/memoh/internal/agent/decision/feedback"
	userinput "github.com/felinics/memoh/internal/agent/decision/input"
	"github.com/felinics/memoh/internal/agent/runtime/agentprocess"
	"github.com/felinics/memoh/internal/agent/runtime/agentstate"
	"github.com/felinics/memoh/internal/agent/runtime/external"
	"github.com/felinics/memoh/internal/agent/runtime/grok/grokcfg"
	"github.com/felinics/memoh/internal/agent/runtime/toolmount"
	"github.com/felinics/memoh/internal/agentcredential"
	"github.com/felinics/memoh/internal/apperror"
	"github.com/felinics/memoh/internal/botagents"
	"github.com/felinics/memoh/internal/mcp"
	"github.com/felinics/memoh/internal/runtimefence"
	"github.com/felinics/memoh/internal/runtimekind"
	"github.com/felinics/memoh/internal/workspace/bridge"
	"github.com/felinics/memoh/internal/workspace/vpath"
)

const (
	RuntimeType    = string(runtimekind.Grok)
	dependencyID   = "grok"
	containerPath  = "/data/.memoh/deps/bin:/opt/memoh/toolkit/bin:/usr/local/bin:/usr/bin:/bin"
	interruptGrace = 10 * time.Second
)

type BridgeSource interface {
	MCPClient(context.Context, string) (*bridge.Client, error)
	WorkspaceInfo(context.Context, string) (bridge.WorkspaceInfo, error)
}
type ApprovalService interface {
	approval.FlowService
	RegisterWaiter(string) func()
}
type activeProcess struct {
	bot, agent string
	cancel     context.CancelFunc
	done       chan struct{}
}
type Driver struct {
	bridges     BridgeSource
	agents      *botagents.Service
	credentials *agentcredential.Service
	approval    ApprovalService
	userInput   userinput.FlowService
	stateStore  agentstate.SessionStateStore
	toolGateway toolmount.Gateway
	logger      *slog.Logger
	launchers   external.LauncherResolver
	mu          sync.Mutex
	active      map[string]*activeProcess
	closed      bool
}

func NewDriver(bridges BridgeSource, agents *botagents.Service, credentials *agentcredential.Service, approvals ApprovalService, state agentstate.SessionStateStore, gateway toolmount.Gateway, logger *slog.Logger) *Driver {
	if logger == nil {
		logger = slog.Default()
	}
	return &Driver{bridges: bridges, agents: agents, credentials: credentials, approval: approvals, stateStore: state, toolGateway: gateway, logger: logger.With(slog.String("runtime", RuntimeType)), active: map[string]*activeProcess{}}
}
func (*Driver) RuntimeType() string                               { return RuntimeType }
func (*Driver) RequiredDependency() string                        { return dependencyID }
func (d *Driver) SetLauncherResolver(r external.LauncherResolver) { d.launchers = r }
func (d *Driver) SetUserInputService(s userinput.FlowService)     { d.userInput = s }
func (d *Driver) ResetBot(botID string)                           { d.reset(botID, "") }
func (d *Driver) ResetBotAgent(botID, agentID string)             { d.reset(botID, agentID) }
func (d *Driver) reset(botID, agentID string) {
	d.mu.Lock()
	var pending []*activeProcess
	for _, p := range d.active {
		if (botID == "" || p.bot == botID) && (agentID == "" || p.agent == agentID) {
			p.cancel()
			pending = append(pending, p)
		}
	}
	d.mu.Unlock()
	timer := time.NewTimer(interruptGrace)
	defer timer.Stop()
	for _, p := range pending {
		select {
		case <-p.done:
		case <-timer.C:
			return
		}
	}
}
func (d *Driver) Close() { d.mu.Lock(); d.closed = true; d.mu.Unlock(); d.reset("", "") }
func (d *Driver) PurgeBotAgentAuth(ctx context.Context, botID, agentID string) error {
	if _, err := uuid.Parse(agentID); err != nil {
		return err
	}
	d.ResetBotAgent(botID, agentID)
	client, err := d.bridges.MCPClient(ctx, botID)
	if err != nil {
		return err
	}
	err = client.DeleteFile(ctx, path.Join("/data/.memoh/grok", agentID, "auth"), true)
	if errors.Is(err, bridge.ErrNotFound) {
		return nil
	}
	return err
}

func (d *Driver) resolveLauncher(ctx context.Context, botID string) (external.Launcher, error) {
	if d.launchers == nil {
		return external.Launcher{Path: "/opt/memoh/toolkit/bin/grok", Source: external.LauncherSourceToolkit}, nil
	}
	launcher, err := d.launchers.ResolveLauncher(ctx, botID, dependencyID)
	var missing *external.DependencyMissingError
	if errors.As(err, &missing) {
		return external.Launcher{}, feedback.New(feedback.CodeAgentDependencyMissing, "dependency_missing", http.StatusConflict, "chat.externalAgent.dependencyMissing", "Grok Build is not installed in this workspace.", map[string]string{"dep_id": dependencyID, "install_task_id": missing.TaskID, "operation_in_progress": strconv.FormatBool(missing.OperationInProgress || missing.TaskID != "")})
	}
	if err != nil {
		return external.Launcher{}, err
	}
	if launcher.Path == "" {
		return external.Launcher{}, errors.New("empty Grok launcher")
	}
	return launcher, nil
}

func runtimeError(err error) error {
	if err == nil || apperror.CodeOf(err) != "" {
		return err
	}
	var f *feedback.Error
	if errors.As(err, &f) {
		return err
	}
	return apperror.Wrap(apperror.CodeExternalRuntimeUnavailable, err, map[string]string{"runtime": RuntimeType})
}

type lease struct {
	d                                 *Driver
	client                            *bridge.Client
	proc                              nativeProcess
	t                                 *turnSession
	caps                              initializeResponse
	session                           sessionResponse
	home, cwd, authPath, credentialID string
	cfg                               grokcfg.Config
	epoch                             agentstate.RuntimeConfigEpoch
	mount                             *toolmount.Mount
	cancel                            context.CancelFunc
	unregister                        func()
	release                           func()
	input                             external.PromptInput
}

func (l *lease) close(ctx context.Context) {
	if l.t != nil {
		l.t.closeDecisions()
	}
	if l.proc != nil {
		_ = l.proc.Close()
	}
	l.persistAuth(ctx, l.input)
	if l.mount != nil {
		l.mount.Stop()
	}
	if l.unregister != nil {
		l.unregister()
	}
	l.cancel()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if l.client != nil && l.home != "" {
		if err := l.client.DeleteFile(ctx, l.home, true); err != nil && !errors.Is(err, bridge.ErrNotFound) {
			l.d.logger.Warn("remove Grok lease", slog.Any("error", err))
		}
	}
	l.release()
}

func (d *Driver) open(ctx context.Context, input external.PromptInput, withTools bool) (l *lease, err error) {
	processCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	key := uuid.NewString()
	active := &activeProcess{bot: input.BotID, agent: input.BotAgentID, cancel: cancel, done: make(chan struct{})}
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		cancel()
		return nil, errors.New("grok driver closed")
	}
	d.active[key] = active
	d.mu.Unlock()
	l = &lease{d: d, input: input, cancel: cancel, release: func() { d.mu.Lock(); delete(d.active, key); close(active.done); d.mu.Unlock() }}
	defer func() {
		if err != nil {
			l.close(ctx)
		}
	}()
	// Bootstrap cancellation follows both the caller and runtime reset. The
	// running process retains its own lifetime for the native cancel handshake.
	bootCtx, bootCancel := context.WithCancel(ctx)
	defer bootCancel()
	stop := context.AfterFunc(processCtx, bootCancel)
	defer stop()
	if _, err = uuid.Parse(input.BotAgentID); err != nil {
		return l, err
	}
	l.epoch, err = d.stateStore.RuntimeConfigEpoch(bootCtx, input.BotID, input.ThreadID)
	if err != nil {
		return l, err
	}
	agent, err := d.agents.Get(bootCtx, input.BotID, input.BotAgentID)
	if err != nil {
		return l, err
	}
	l.cfg, err = grokcfg.ParseAgentConfig(agent.Metadata)
	if err != nil {
		return l, err
	}
	credential, err := d.credentials.ResolveForBotAgent(bootCtx, input.BotID, input.BotAgentID)
	if err != nil {
		return l, external.CredentialError(err)
	}
	if !botagents.AcceptsCredential(agent, credential.AuthKind) {
		return l, external.CredentialError(agentcredential.ErrIncompatible)
	}
	l.credentialID = credential.ID
	l.client, err = d.bridges.MCPClient(bootCtx, input.BotID)
	if err != nil {
		return l, err
	}
	info, err := d.bridges.WorkspaceInfo(bootCtx, input.BotID)
	if err != nil {
		return l, err
	}
	if err = external.RequireContainerWorkspace(info, RuntimeType); err != nil {
		return l, err
	}
	launcher, err := d.resolveLauncher(bootCtx, input.BotID)
	if err != nil {
		return l, err
	}
	l.cwd, err = vpath.ResolveUnderRoot("/data", metaString(input.RuntimeMetadata, "project_path"))
	if err != nil {
		return l, err
	}
	l.home = path.Join("/data/.memoh/grok", input.BotAgentID, "leases", key)
	permission := first(metaString(input.RuntimeMetadata, "permission_mode"), l.cfg.PermissionMode, "ask")
	if !grokcfg.ValidPermissionMode(permission) {
		return l, external.ErrModeUnavailable
	}
	config := "[cli]\nauto_update = false\nuse_leader = false\n[ui]\npermission_mode = " + strconv.Quote(permission) + "\n[features]\ntelemetry = false\n[telemetry]\ntrace_upload = false\n[grok_com_config]\npreferred_method = "
	if l.cfg.Auth == grokcfg.AuthAPIKey {
		config += "\"api_key\"\n"
	} else {
		config += "\"oidc\"\n"
	}
	err = d.stateStore.GuardRuntimeSync(bootCtx, input.BotID, l.epoch.Bot, func(guardCtx context.Context) error {
		if _, e := l.client.WriteRawNoFollow(guardCtx, "/data", strings.TrimPrefix(path.Join(l.home, "config.toml"), "/data/"), strings.NewReader(config)); e != nil {
			return e
		}
		if l.cfg.Auth == grokcfg.AuthOAuth {
			var e error
			l.authPath, e = l.projectAuth(guardCtx, input, credential)
			return e
		}
		return nil
	})
	if err != nil {
		return l, err
	}
	sessionID := ""
	if input.ThreadID != "" {
		sessionID, err = d.restore(bootCtx, l.client, input, l.home, l.cwd)
		if err != nil {
			return l, apperror.Wrap(apperror.CodeSessionHistoryInconsistent, err, nil)
		}
	}
	env := []string{"PATH=" + containerPath, "HOME=/data", "GROK_HOME=" + l.home, "GROK_WORKSPACE_HOME=" + path.Join(l.home, "workspace")}
	if l.cfg.Auth == grokcfg.AuthAPIKey {
		env = append(env, "XAI_API_KEY="+credential.Secret["api_key"])
	} else {
		env = append(env, "GROK_AUTH_PATH="+l.authPath)
	}
	l.proc, err = agentprocess.Start(processCtx, l.client, shellQuote(launcher.Path)+" --no-auto-update agent --no-leader stdio", l.cwd, env)
	if err != nil {
		return l, err
	}
	l.t = newTurnSession(ctx, d, input, l.proc)
	l.caps, err = call[initializeResponse](bootCtx, l.t, "initialize", map[string]any{"protocolVersion": 1, "clientInfo": map[string]any{"name": "memoh", "version": "1"}, "clientCapabilities": map[string]any{}})
	if err != nil {
		return l, err
	}
	if !l.caps.Meta.GrokShell || l.caps.ProtocolVersion != 1 {
		return l, errors.New("executable did not identify as Grok Build ACP")
	}
	if observer, ok := d.launchers.(external.VersionObserver); ok {
		observer.ObserveLauncherVersion(bootCtx, input.BotID, dependencyID, l.caps.Meta.AgentVersion)
	}
	method := "xai.api_key"
	if l.cfg.Auth == grokcfg.AuthOAuth {
		method = "grok.com"
	}
	if _, err = call[map[string]any](bootCtx, l.t, "authenticate", map[string]any{"methodId": method, "_meta": map[string]any{"headless": true}}); err != nil {
		return l, external.CredentialError(err)
	}
	seed, isFork, seedErr := decodeFork(input.RuntimeMetadata)
	if seedErr != nil {
		return l, seedErr
	}
	if isFork && sessionID == seed.SourceSessionID {
		sessionID, err = l.fork(bootCtx, seed)
		if err != nil {
			return l, err
		}
	} else if isFork && sessionID == "" && !input.ForceFreshRuntime {
		return l, apperror.New(apperror.CodeSessionHistoryInconsistent, nil)
	}
	servers := []any{}
	if withTools {
		if !l.caps.AgentCapabilities.MCPCapabilities.HTTP {
			return l, errors.New("grok Build does not support HTTP MCP")
		}
		baseURL := toolmount.ResolveBaseURL(info, input.ToolHTTPURL)
		if baseURL == "" {
			return l, errors.New("memoh tool gateway unavailable")
		}
		toolSession := turnToolSession(ctx, input, l.caps.AgentCapabilities.PromptCapabilities.Image)
		l.mount, err = toolmount.Serve(processCtx, l.client, baseURL, d.toolGateway, func() mcp.ToolSessionContext { return toolSession })
		if err != nil {
			return l, err
		}
		servers = append(servers, map[string]any{"type": "http", "name": "memoh", "url": l.mount.URL, "headers": []any{}})
		l.unregister = toolmount.RegisterTurnSink(d.toolGateway.Contexts, input.BotID, input.ThreadID, input.RunID, l.t.emit)
	}
	params := map[string]any{"cwd": l.cwd, "mcpServers": servers, "_meta": map[string]any{"yoloMode": permission == "always-approve", "autoMode": permission == "auto"}}
	method = "session/new"
	if sessionID != "" {
		if !l.caps.AgentCapabilities.LoadSession {
			return l, errors.New("grok Build cannot load the published session")
		}
		method = "session/load"
		params["sessionId"] = sessionID
	}
	l.t.mu.Lock()
	l.t.sessionID = sessionID
	l.t.restoring = sessionID != ""
	l.t.mu.Unlock()
	l.session, err = call[sessionResponse](bootCtx, l.t, method, params)
	if err != nil {
		return l, err
	}
	if sessionID == "" {
		sessionID = l.session.SessionID
	}
	if !safeSessionID(sessionID) {
		return l, errors.New("invalid Grok session identity")
	}
	l.t.mu.Lock()
	l.t.sessionID = sessionID
	l.t.restoring = false
	l.t.mu.Unlock()
	if err = l.configure(bootCtx, input); err != nil {
		return l, err
	}
	return l, nil
}

func (l *lease) drain() error {
	l.proc.CloseStdin()
	timer := time.NewTimer(interruptGrace)
	defer timer.Stop()
	select {
	case <-l.proc.Done():
		return l.proc.Err()
	case <-timer.C:
		_ = l.proc.Close()
		return errors.New("grok did not exit after stdin EOF")
	}
}

func (d *Driver) Prompt(ctx context.Context, input external.PromptInput) (external.PromptResult, error) {
	l, err := d.open(ctx, input, true)
	if err != nil {
		return external.PromptResult{}, runtimeError(err)
	}
	defer l.close(ctx)
	if input.Command != "" {
		return external.PromptResult{}, external.ErrCommandUnavailable
	}
	blocks, err := l.promptBlocks(ctx, input)
	if err != nil {
		return external.PromptResult{}, runtimeError(err)
	}
	before, beforeErr := l.promptCount(ctx)
	response, err := l.prompt(ctx, blocks)
	anchor := ""
	if err == nil && response.StopReason != "cancelled" && beforeErr == nil && ctx.Err() == nil {
		countCtx, countCancel := context.WithTimeout(ctx, 10*time.Second)
		after, countErr := l.promptCount(countCtx)
		countCancel()
		if countErr == nil && after == before+1 {
			anchor = encodeBoundary(l.t.sessionID, response.Meta.PromptID, after)
		}
	}
	if err == nil && !validStopReason(response.StopReason) {
		err = errors.New("invalid Grok prompt terminal response")
	}
	l.t.closeDecisions()
	exitErr := l.drain()
	l.persistAuth(ctx, input)
	l.t.mu.Lock()
	result := external.PromptResult{Output: l.t.recorder.Messages(""), Text: l.t.text.String(), StopReason: response.StopReason, TurnCompleted: err == nil && ctx.Err() == nil && response.StopReason != "cancelled", AgentTurnID: anchor, RuntimeMetadata: l.metadata(input)}
	l.t.mu.Unlock()
	if response.Meta.Usage != nil {
		result.Usage = response.Meta.Usage.sdk()
	}
	// No interjection admission: the application's queue retains ownership of
	// follow-up messages and dispatches them in a new fenced run.
	if result.TurnCompleted {
		result.Checkpoint = external.CheckpointDeclined
		if exitErr == nil {
			stageCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
			stageErr := d.stage(stageCtx, l.client, input, l.home, l.t.sessionID, l.cwd, l.epoch)
			cancel()
			if stageErr == nil {
				result.Checkpoint = external.CheckpointStaged
			} else {
				d.logger.Error("Grok checkpoint staging failed", slog.Any("error", stageErr))
				err = apperror.Wrap(apperror.CodeSessionHistoryInconsistent, stageErr, nil)
				result.TurnCompleted = false
			}
		} else {
			err = exitErr
			result.TurnCompleted = false
		}
	}
	if ctx.Err() != nil {
		return result, nil
	}
	return result, runtimeError(err)
}

func (l *lease) prompt(ctx context.Context, blocks []any) (promptResponse, error) {
	type outcome struct {
		response promptResponse
		err      error
	}
	requestCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	done := make(chan outcome, 1)
	go func() {
		r, e := call[promptResponse](requestCtx, l.t, "session/prompt", map[string]any{"sessionId": l.t.sessionID, "prompt": blocks})
		done <- outcome{r, e}
	}()
	select {
	case result := <-done:
		return result.response, result.err
	case <-ctx.Done():
	}
	l.t.cancel()
	// Preserve the original request until its terminal response and SDK
	// notification watermark are drained. Canceling its Go context loses both.
	graceCtx, graceCancel := context.WithTimeout(context.WithoutCancel(ctx), interruptGrace)
	defer graceCancel()
	cancelDone := make(chan error, 1)
	go func() {
		cancelDone <- l.t.conn.SendNotification(graceCtx, "session/cancel", map[string]any{"sessionId": l.t.sessionID})
	}()
	var result outcome
	select {
	case result = <-done:
	case <-graceCtx.Done():
		_ = l.proc.Close()
		cancel()
		<-done
		return promptResponse{}, errors.New("grok cancellation unconfirmed")
	}
	select {
	case err := <-cancelDone:
		if err != nil {
			return result.response, err
		}
	case <-graceCtx.Done():
		return result.response, graceCtx.Err()
	}
	return result.response, result.err
}

func (l *lease) promptBlocks(ctx context.Context, input external.PromptInput) ([]any, error) {
	blocks := []any{}
	if input.ContextMarkdown != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": input.ContextMarkdown})
	}
	blocks = append(blocks, map[string]any{"type": "text", "text": input.Prompt})
	for i, img := range input.Images {
		if l.caps.AgentCapabilities.PromptCapabilities.Image {
			blocks = append(blocks, map[string]any{"type": "image", "data": base64.StdEncoding.EncodeToString(img.Data), "mimeType": first(img.MimeType, "image/png")})
			continue
		}
		if !input.CanFallbackImagesToFiles {
			return nil, feedback.New(feedback.CodeImageInputUnsupported, "image_input_unsupported", http.StatusBadRequest, "", "This Grok Build version does not accept image prompts.", nil)
		}
		rel := path.Join(".memoh/attachments/grok", input.RunID, fmt.Sprintf("image-%d", i))
		if _, err := l.client.WriteRawNoFollow(ctx, "/data", rel, bytes.NewReader(img.Data)); err != nil {
			return nil, err
		}
		blocks = append(blocks, map[string]any{"type": "text", "text": "Attached image file: " + path.Join("/data", rel) + " (" + img.MimeType + ")"})
	}
	return blocks, nil
}

func (l *lease) metadata(input external.PromptInput) map[string]any {
	out := map[string]any{"grok_session_id": l.t.sessionID, "grok_thread_id": input.ThreadID, "grok_snapshot_version": 1, "grok_fork": nil, "grok_cli_version": l.caps.Meta.AgentVersion}
	for k, v := range l.t.metadata {
		out[k] = v
	}
	return out
}

func turnToolSession(ctx context.Context, input external.PromptInput, images bool) mcp.ToolSessionContext {
	s := mcp.ToolSessionContext{BotID: input.BotID, ChatID: first(input.ChatID, input.BotID), SessionID: input.ThreadID, RunID: input.RunID, SessionType: first(input.SessionMode, "chat"), RouteID: input.RouteID, CurrentPlatform: input.CurrentPlatform, ReplyTarget: input.ReplyTarget, ConversationType: input.ConversationType, ChannelIdentityID: input.ChannelIdentityID, SessionToken: input.SessionToken, CanRequestUserInput: input.CanRequestUserInput, CanListUserInput: true, RuntimeActive: true, RequireActiveRun: true, SupportsImageInput: images, ContextBudgetMaxTokens: input.ContextBudgetMaxTokens, ContextToolExchangePolicy: input.ContextToolExchangePolicy, RunContext: ctx}
	if fence, ok := runtimefence.FromContext(ctx); ok {
		s.RuntimeFence = fence
	}
	return s
}
func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'" }

type nativeUsage struct {
	Input      int  `json:"input_tokens"`
	Output     int  `json:"output_tokens"`
	Total      int  `json:"total_tokens"`
	Cached     int  `json:"cached_read_tokens"`
	Reasoning  int  `json:"reasoning_tokens"`
	Incomplete bool `json:"usageIsIncomplete"`
}

func (u *nativeUsage) sdk() *sdk.Usage {
	return &sdk.Usage{InputTokens: u.Input, OutputTokens: u.Output, TotalTokens: u.Total, CachedInputTokens: u.Cached, ReasoningTokens: u.Reasoning}
}

func validStopReason(reason string) bool {
	switch reason {
	case "end_turn", "max_tokens", "max_turn_requests", "refusal", "cancelled":
		return true
	default:
		return false
	}
}
