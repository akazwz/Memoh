// Package acpagent manages long-lived ACP agent runtimes.
//
// Architecture note: this is an in-memory runtime pool for a single server
// instance only. A runtime is an OS process plus protocol state; it is
// identified by a server-generated runtime ID and optionally *bound* to one
// chat session. Sessions live in the database and survive restarts; runtimes
// do not — after a restart the next prompt simply cold-starts a fresh
// runtime. "First-class" here means code abstraction and lifecycle ownership,
// not persistence.
package acpagent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/memohai/memoh/internal/acpclient"
	"github.com/memohai/memoh/internal/acpprofile"
	"github.com/memohai/memoh/internal/bots"
	"github.com/memohai/memoh/internal/mcp"
	"github.com/memohai/memoh/internal/session"
	"github.com/memohai/memoh/internal/workspace/bridge"
)

const (
	// boundRuntimeIdleTimeout reaps runtimes attached to a session after
	// prolonged inactivity.
	boundRuntimeIdleTimeout = 30 * time.Minute
	// unboundRuntimeIdleTimeout reaps runtimes that were created for the
	// pre-session model picker but never bound to a session.
	unboundRuntimeIdleTimeout = 5 * time.Minute
	// maxUnboundRuntimesPerBot bounds pre-session runtimes per bot so a
	// single caller cannot spawn unbounded agent processes.
	maxUnboundRuntimesPerBot = 4

	runtimeIDPrefix = "rt_"
)

var (
	// ErrRuntimeNotFound reports that no runtime with the given ID is owned
	// by the calling bot. Cross-bot references intentionally behave exactly
	// like missing runtimes: no side effects, no existence leak.
	ErrRuntimeNotFound = errors.New("ACP runtime not found")
	// ErrRuntimeBindRejected reports that a runtime cannot be bound to the
	// session (already bound, still starting, closed, or agent/project
	// mismatch). Callers should fall back to a cold start.
	ErrRuntimeBindRejected = errors.New("ACP runtime cannot be bound to this session")
	// ErrTooManyRuntimes reports that the per-bot budget for unbound
	// runtimes is exhausted and every slot is busy.
	ErrTooManyRuntimes = errors.New("too many unbound ACP runtimes for this bot")
	// ErrSessionBusy reports that the session's runtime is already serving a
	// turn. The prompt was NOT delivered; the caller surfaces the active turn
	// to the user, who can abort it (AbortTurn) or wait it out. Deliberately
	// an immediate error instead of queueing: queueing on the op mutex is an
	// uncancellable wait of up to a full prompt duration.
	ErrSessionBusy = errors.New("ACP session already has a turn in progress")
	// ErrPromptAborted marks a prompt that ended because its turn was
	// cancelled — by the originating caller's context or by an out-of-band
	// AbortTurn. session/cancel went out, the runtime stays warm, and the
	// flow layer persists the partial round as a cancelled turn.
	ErrPromptAborted = errors.New("ACP turn aborted")
	// ErrNoActiveTurn reports an abort request for a session whose runtime
	// has no turn in flight.
	ErrNoActiveTurn = errors.New("ACP session has no turn in progress")
	// ErrRuntimeRetired reports an operation racing the runtime's permanent
	// retirement (session deleted, agent switched, server shutdown). Unlike a
	// replaced runtime, a retired one must never be transparently recreated.
	ErrRuntimeRetired = errors.New("ACP runtime is closing")
)

const (
	stateStarting = "starting"
	stateIdle     = "idle"
	stateActive   = "active"
	stateClosed   = "closed"

	// closedReason records WHY a runtime was destroyed, so an in-flight
	// prompt unwinding through teardown can tell a benign replacement
	// (transport hiccup, idle reap — transparently recreate on retry) from a
	// permanent retirement (session deleted, agent switched — fail the
	// operation; recreating would resurrect a runtime for a dead session).
	closedReasonReplaced = "replaced"
	closedReasonRetired  = "retired"

	turnStateRunning    = "running"
	turnStateCancelling = "cancelling"

	turnIDPrefix = "turn_"
)

// runtimeTurn is the pool's record of one in-flight prompt: the unit of
// busy-ness and the anchor for out-of-band cancellation (architecture doc
// §5.2, Phase 1 of the turn model — the event plane and approval linkage
// build on this identity later).
type runtimeTurn struct {
	id        string
	sessionID string
	state     string
	cancel    context.CancelFunc
	startedAt time.Time
}

// SessionPool owns every ACP runtime in the process. Runtimes are keyed by a
// server-generated runtime ID; bySession is a secondary index from a bound
// chat session to its runtime.
//
// Lock order: handle.op serializes operations on one runtime (prompt, model,
// bind, close). handle.state is the innermost leaf guarding the mutable
// snapshot fields. p.mu guards the maps; it may be held while taking
// handle.state (budget scans), but handle.state is never held while taking
// p.mu, and p.mu is never held while taking handle.op.
type SessionPool struct {
	logger   *slog.Logger
	runner   sessionRunner
	bots     botGetter
	store    sessionGetter
	tools    *mcp.ToolGatewayService
	contexts *mcp.ToolSessionContextStore
	approval acpclient.ToolApprovalService
	timeout  time.Duration

	mu        sync.RWMutex
	runtimes  map[string]*runtimeHandle
	bySession map[string]string
}

type sessionRunner interface {
	WorkspaceInfo(ctx context.Context, botID string) (bridge.WorkspaceInfo, error)
	StartSession(ctx context.Context, req acpclient.StartRequest, sink acpclient.EventSink) (*acpclient.Session, error)
}

type workspaceClientRunner interface {
	MCPClient(ctx context.Context, botID string) (*bridge.Client, error)
}

type botGetter interface {
	Get(ctx context.Context, botID string) (bots.Bot, error)
}

type sessionGetter interface {
	Get(ctx context.Context, sessionID string) (session.Session, error)
}

// runtimeHandle is the single owner of one agent process. All internal code
// operates on handles resolved through the pool's tenancy gate — never on
// bare string IDs — so cleanup can only ever touch the runtime it resolved.
type runtimeHandle struct {
	// Stable identity, fixed at creation.
	id          string
	botID       string
	agentID     string
	projectPath string

	// op serializes operations (start, prompt, set-model, bind, close).
	op sync.Mutex

	// state guards the mutable snapshot below. Leaf lock: never acquire
	// other locks while holding it.
	state          sync.Mutex
	session        *acpclient.Session
	status         string
	lastActive     time.Time
	boundSession   string
	defaultModelID string
	active         *acpclient.ToolSessionContext
	startCancel    context.CancelFunc
	closed         bool
	// closedReason qualifies closed (closedReasonReplaced / Retired). Set
	// once; teardown preserves the first writer's intent.
	closedReason string
	// turn is the in-flight prompt, when any. The turn slot doubles as the
	// busy gate: a second prompt finding it occupied reports ErrSessionBusy
	// instead of queueing behind op.
	turn *runtimeTurn
	// historyInjected records that this runtime already served a prompt and
	// therefore received the session's replayed history (when the caller
	// provided any). The agent process carries the conversation from there.
	historyInjected bool
}

// retired reports whether the handle was (or is being) permanently retired.
func (h *runtimeHandle) retired() bool {
	h.state.Lock()
	defer h.state.Unlock()
	return h.closedReason == closedReasonRetired
}

// PromptInput carries one prompt (or runtime control call) for a chat
// session. Session metadata (agent, project path) is resolved from the
// session store when available.
type PromptInput struct {
	BotID             string
	ChatID            string
	SessionID         string
	StreamID          string
	SessionType       string
	RouteID           string
	AgentID           string
	ProjectPath       string
	Prompt            string
	ChannelIdentityID string
	// SessionToken is consumed only by Prompt, where it flows into the
	// per-prompt tool context overlay. Ensure and SetModel ignore it.
	SessionToken     string //nolint:gosec // runtime session credential, not a hardcoded secret.
	CurrentPlatform  string
	ReplyTarget      string
	ConversationType string
	// Timezone is the IANA timezone resolved for the bot/user; it flows into
	// the tool session context so native tools render times correctly.
	Timezone        string
	ToolHTTPURL     string
	ContextURI      string
	ContextMarkdown string
	// HistoryResources lazily renders the session's prior conversation. The
	// pool invokes it at most once per runtime — on the first prompt the
	// runtime serves — so a replacement runtime (server restart, idle reap,
	// crash, agent switch) replays the durable history it never saw, while a
	// warm runtime pays nothing. A returned error keeps the once-per-runtime
	// flag unburned so the next prompt retries the replay.
	HistoryResources func() ([]acpclient.PromptResource, error)
	// OnTurnStart, when set, is invoked once the pool grants this prompt its
	// turn slot, with the server-generated turn ID — before any agent output
	// flows. Callers use it to label the turn's event stream so observers can
	// correlate live events, snapshots, and abort targets.
	OnTurnStart func(turnID string)
	Sink        acpclient.EventSink
}

// CreateRuntimeInput describes a pre-session runtime creation request.
type CreateRuntimeInput struct {
	BotID       string
	AgentID     string
	ProjectPath string
	ToolHTTPURL string
	Sink        acpclient.EventSink
}

// RuntimeStatus describes the live state of a pooled ACP runtime as exposed
// over the HTTP API.
type RuntimeStatus struct {
	RuntimeID      string                `json:"runtime_id,omitempty"`
	SessionID      string                `json:"session_id,omitempty"`
	AgentID        string                `json:"agent_id,omitempty"`
	ProjectPath    string                `json:"project_path,omitempty"`
	State          string                `json:"state"`
	ACPSession     string                `json:"acp_session_id,omitempty"`
	Models         *acpclient.ModelState `json:"models,omitempty"`
	DefaultModelID string                `json:"default_model_id,omitempty"`
	// TurnID/TurnState describe the in-flight turn, when any (running or
	// cancelling). Clients use them to render busy state and offer abort.
	TurnID    string `json:"turn_id,omitempty"`
	TurnState string `json:"turn_state,omitempty"`
}

func NewSessionPool(log *slog.Logger, runner *acpclient.Runner, botService *bots.Service, sessionServices ...*session.Service) *SessionPool {
	var sessionService sessionGetter
	if len(sessionServices) > 0 {
		sessionService = sessionServices[0]
	}
	return newSessionPool(log, runner, botService, sessionService)
}

func newSessionPool(log *slog.Logger, runner sessionRunner, botService botGetter, sessionServices ...sessionGetter) *SessionPool {
	if log == nil {
		log = slog.Default()
	}
	var sessionService sessionGetter
	if len(sessionServices) > 0 {
		sessionService = sessionServices[0]
	}
	return &SessionPool{
		logger:    log.With(slog.String("service", "acp_session_pool")),
		runner:    runner,
		bots:      botService,
		store:     sessionService,
		timeout:   boundRuntimeIdleTimeout,
		runtimes:  map[string]*runtimeHandle{},
		bySession: map[string]string{},
	}
}

func (p *SessionPool) SetToolGateway(gateway *mcp.ToolGatewayService) {
	if p != nil {
		p.tools = gateway
	}
}

func (p *SessionPool) SetToolSessionContextStore(store *mcp.ToolSessionContextStore) {
	if p != nil {
		p.contexts = store
	}
}

func (p *SessionPool) SetToolApprovalService(service acpclient.ToolApprovalService) {
	if p != nil {
		p.approval = service
	}
}

func newRuntimeID() string {
	return runtimeIDPrefix + uuid.NewString()
}

// owned is the single tenancy gate: every runtime-scoped operation resolves
// through here, and a cross-bot reference behaves exactly like a missing
// runtime — zero side effects.
func (p *SessionPool) owned(botID, runtimeID string) (*runtimeHandle, error) {
	botID = strings.TrimSpace(botID)
	runtimeID = strings.TrimSpace(runtimeID)
	if p == nil || botID == "" || runtimeID == "" {
		return nil, ErrRuntimeNotFound
	}
	p.mu.RLock()
	h := p.runtimes[runtimeID]
	p.mu.RUnlock()
	if h == nil || h.botID != botID {
		return nil, ErrRuntimeNotFound
	}
	return h, nil
}

// CreateRuntime starts an unbound runtime for the pre-session model picker.
// The runtime ID is server-generated; clients can never choose it.
func (p *SessionPool) CreateRuntime(ctx context.Context, input CreateRuntimeInput) (RuntimeStatus, error) {
	if p == nil || p.runner == nil || p.bots == nil {
		return RuntimeStatus{}, errors.New("ACP session pool is not configured")
	}
	botID := strings.TrimSpace(input.BotID)
	if botID == "" {
		return RuntimeStatus{}, errors.New("bot_id is required")
	}
	agentID := acpprofile.NormalizeAgentID(input.AgentID)
	if agentID == "" {
		agentID = acpprofile.AgentCodexID
	}
	projectPath := strings.TrimSpace(input.ProjectPath)

	p.reapIdle(time.Now()) //nolint:contextcheck // reaper close uses its own background ctx.

	h := &runtimeHandle{
		id:          newRuntimeID(),
		botID:       botID,
		agentID:     agentID,
		projectPath: projectPath,
		status:      stateStarting,
		lastActive:  time.Now(),
	}
	p.mu.Lock()
	victims, err := p.unboundBudgetLocked(botID)
	if err != nil {
		p.mu.Unlock()
		return RuntimeStatus{}, err
	}
	p.runtimes[h.id] = h
	p.mu.Unlock()
	for _, victim := range victims {
		p.logger.Info("evicting oldest unbound ACP runtime",
			slog.String("runtime_id", victim.id), slog.String("bot_id", botID))
		p.tryCloseIdle(victim, 0, true) //nolint:contextcheck // lifecycle close uses background ctx.
	}

	h.op.Lock()
	err = p.startRuntime(ctx, h, startOptions{
		ToolHTTPURL: input.ToolHTTPURL,
		Sink:        input.Sink,
	})
	h.op.Unlock()
	if err != nil {
		return RuntimeStatus{}, err
	}
	return p.statusOf(h), nil
}

// unboundBudgetLocked enforces the per-bot unbound runtime budget. Must be
// called with p.mu held. Returns the idle victims to evict (closed by the
// caller outside the lock); errors when the budget is full and every slot is
// busy starting or serving a request.
func (p *SessionPool) unboundBudgetLocked(botID string) ([]*runtimeHandle, error) {
	count := 0
	var oldest *runtimeHandle
	var oldestActive time.Time
	for _, h := range p.runtimes {
		if h == nil || h.botID != botID {
			continue
		}
		h.state.Lock()
		unbound := h.boundSession == "" && !h.closed
		idle := h.status == stateIdle
		last := h.lastActive
		h.state.Unlock()
		if !unbound {
			continue
		}
		count++
		if !idle {
			continue
		}
		if oldest == nil || last.Before(oldestActive) {
			oldest, oldestActive = h, last
		}
	}
	if count < maxUnboundRuntimesPerBot {
		return nil, nil
	}
	if oldest == nil {
		return nil, fmt.Errorf("%w (limit %d)", ErrTooManyRuntimes, maxUnboundRuntimesPerBot)
	}
	return []*runtimeHandle{oldest}, nil
}

// BindRuntime attaches an unbound runtime to a freshly created chat session.
// After binding, the runtime uses the normal (bound) idle timeout and the
// session's prompts reuse the warm process. Returns ErrRuntimeBindRejected
// when the runtime cannot serve this session; callers fall back to a cold
// start and must not treat that as fatal.
func (p *SessionPool) BindRuntime(botID, runtimeID, sessionID, agentID, projectPath string) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return errors.New("session_id is required")
	}
	h, err := p.owned(botID, runtimeID)
	if err != nil {
		return err
	}
	normalizedAgent := acpprofile.NormalizeAgentID(agentID)
	if normalizedAgent == "" {
		normalizedAgent = acpprofile.AgentCodexID
	}
	projectPath = strings.TrimSpace(projectPath)

	// Waits out an in-flight model change on the runtime.
	h.op.Lock()
	defer h.op.Unlock()

	h.state.Lock()
	ok := !h.closed && h.session != nil && h.boundSession == "" &&
		h.agentID == normalizedAgent && h.projectPath == projectPath
	h.state.Unlock()
	if !ok {
		return ErrRuntimeBindRejected
	}

	p.mu.Lock()
	if existing, taken := p.bySession[sessionID]; taken && existing != h.id {
		p.mu.Unlock()
		return ErrRuntimeBindRejected
	}
	p.bySession[sessionID] = h.id
	p.mu.Unlock()

	h.state.Lock()
	h.boundSession = sessionID
	h.lastActive = time.Now()
	h.state.Unlock()
	return nil
}

// RuntimeStatusByID reports the live state of an owned runtime.
func (p *SessionPool) RuntimeStatusByID(botID, runtimeID string) (RuntimeStatus, error) {
	h, err := p.owned(botID, runtimeID)
	if err != nil {
		return RuntimeStatus{}, err
	}
	return p.statusOf(h), nil
}

// SetRuntimeModel switches the runtime's model. An empty modelID resets the
// runtime to the agent default captured at startup.
func (p *SessionPool) SetRuntimeModel(ctx context.Context, botID, runtimeID, modelID string) (RuntimeStatus, error) {
	h, err := p.owned(botID, runtimeID)
	if err != nil {
		return RuntimeStatus{}, err
	}
	if err := p.setModelOnHandle(ctx, h, modelID); err != nil {
		return RuntimeStatus{}, err
	}
	return p.statusOf(h), nil
}

func (*SessionPool) setModelOnHandle(ctx context.Context, h *runtimeHandle, modelID string) error {
	modelID = strings.TrimSpace(modelID)

	h.op.Lock()
	defer h.op.Unlock()

	h.state.Lock()
	sess := h.session
	closed := h.closed
	if modelID == "" {
		modelID = h.defaultModelID
	}
	h.state.Unlock()
	if closed || sess == nil {
		return ErrRuntimeNotFound
	}
	if modelID == "" {
		// Reset requested but the agent never reported a default; nothing to do.
		return nil
	}
	if strings.TrimSpace(sess.ModelState().CurrentModelID) == modelID {
		return nil
	}

	h.setStatus(stateActive)
	_, err := sess.SetModel(ctx, modelID)
	// Model selection errors are validation/protocol issues, not process
	// failures; keep the runtime alive so the user can pick another model.
	h.setStatus(stateIdle)
	return err
}

// CloseRuntime tears down an owned runtime, waiting out any in-flight
// operation first.
func (p *SessionPool) CloseRuntime(botID, runtimeID string) error {
	h, err := p.owned(botID, runtimeID)
	if err != nil {
		return err
	}
	return p.closeHandle(h) //nolint:contextcheck // lifecycle close uses background ctx.
}

// ResolveRuntimeToolContext resolves the trusted MCP tool context for a
// runtime referenced by its stable ID (for example from baked process
// headers). Fails closed: dead or foreign runtimes resolve to nothing.
func (p *SessionPool) ResolveRuntimeToolContext(botID, runtimeID string) (mcp.ToolSessionContext, bool) {
	h, err := p.owned(botID, runtimeID)
	if err != nil {
		return mcp.ToolSessionContext{}, false
	}
	h.state.Lock()
	closed := h.closed
	h.state.Unlock()
	if closed {
		return mcp.ToolSessionContext{}, false
	}
	return h.toolContext(), true
}

// prepareInput validates pool wiring and required input fields, returning
// the input with session metadata applied.
func (p *SessionPool) prepareInput(ctx context.Context, input PromptInput) (PromptInput, error) {
	if p == nil || p.runner == nil || p.bots == nil {
		return PromptInput{}, errors.New("ACP session pool is not configured")
	}
	if strings.TrimSpace(input.SessionID) == "" {
		return PromptInput{}, errors.New("session_id is required")
	}
	resolved, err := p.resolveSessionMetadata(ctx, input)
	if err != nil {
		return PromptInput{}, err
	}
	if strings.TrimSpace(resolved.BotID) == "" {
		return PromptInput{}, errors.New("bot_id is required")
	}
	return resolved, nil
}

// Prompt sends a prompt to the runtime bound to input.SessionID, cold
// starting (and binding) one when the session has no live runtime.
//
//nolint:contextcheck // lifecycle close intentionally uses background ctx.
func (p *SessionPool) Prompt(ctx context.Context, input PromptInput) (acpclient.PromptResult, error) {
	input, err := p.prepareInput(ctx, input)
	if err != nil {
		return acpclient.PromptResult{}, err
	}
	if strings.TrimSpace(input.Prompt) == "" {
		return acpclient.PromptResult{}, errors.New("prompt is required")
	}

	p.reapIdle(time.Now())
	// A handle can be torn down between resolution and use (reaper, agent
	// change, a concurrent failed prompt); retry resolution a bounded number
	// of times instead of failing the user's message.
	for attempt := 0; attempt < 3; attempt++ {
		h, preclaimed, err := p.runtimeForSession(ctx, input, true)
		if err != nil {
			return acpclient.PromptResult{}, err
		}
		result, retry, err := p.promptOnHandle(ctx, h, preclaimed, input)
		if retry {
			continue
		}
		return result, err
	}
	return acpclient.PromptResult{}, errors.New("ACP runtime is restarting, retry the prompt")
}

func (p *SessionPool) promptOnHandle(ctx context.Context, h *runtimeHandle, preclaimed *runtimeTurn, input PromptInput) (acpclient.PromptResult, bool, error) {
	// The turn context is what AbortTurn (out-of-band) and the caller's own
	// cancellation (in-band) both cancel; classification below keys on it.
	turnCtx, cancelTurn := context.WithCancel(ctx)
	defer cancelTurn()

	// Claim the turn slot BEFORE touching op: the slot is what makes a busy
	// session report busy immediately instead of queueing the caller behind a
	// running prompt on an uncancellable mutex. A handle that is still
	// starting (session nil, not closed) passes through — the op wait below
	// is bounded by startup, not by a turn. A cold start arrives with the
	// slot pre-claimed (runtimeForSession), so the prompt that paid for the
	// start cannot lose it to a concurrent prompt.
	h.state.Lock()
	if h.closed {
		retired := h.closedReason == closedReasonRetired
		h.state.Unlock()
		if retired {
			return acpclient.PromptResult{}, false, ErrRuntimeRetired
		}
		return acpclient.PromptResult{}, true, nil
	}
	var turn *runtimeTurn
	if preclaimed != nil && h.turn == preclaimed {
		turn = preclaimed
		turn.cancel = cancelTurn
		if turn.state == turnStateCancelling {
			// AbortTurn landed during the cold start, before a cancel func
			// existed; honor it now.
			cancelTurn()
		}
	} else {
		if h.turn != nil {
			busyTurn := h.turn.id
			h.state.Unlock()
			return acpclient.PromptResult{}, false, fmt.Errorf("%w (turn %s)", ErrSessionBusy, busyTurn)
		}
		turn = &runtimeTurn{
			id:        turnIDPrefix + uuid.NewString(),
			sessionID: strings.TrimSpace(input.SessionID),
			state:     turnStateRunning,
			cancel:    cancelTurn,
			startedAt: time.Now(),
		}
		h.turn = turn
	}
	h.state.Unlock()
	defer func() {
		h.state.Lock()
		if h.turn == turn {
			h.turn = nil
		}
		h.state.Unlock()
	}()
	if input.OnTurnStart != nil {
		input.OnTurnStart(turn.id)
	}

	// With the slot claimed, op only contends with brief control operations
	// (set-model, bind, close), never with another prompt.
	h.op.Lock()
	defer h.op.Unlock()

	h.state.Lock()
	if h.closed || h.session == nil {
		retired := h.closedReason == closedReasonRetired
		h.state.Unlock()
		if retired {
			return acpclient.PromptResult{}, false, ErrRuntimeRetired
		}
		return acpclient.PromptResult{}, true, nil
	}
	sess := h.session
	h.status = stateActive
	h.lastActive = time.Now()
	toolCtx := toolSessionContext(input, h)
	h.active = &toolCtx
	injectHistory := !h.historyInjected
	h.historyInjected = true
	h.state.Unlock()
	// Cleanup is defer-based so error paths can never leave a stale
	// per-prompt context or sink behind.
	defer h.clearActive()

	toolSink := newPromptToolEventSink(input.Sink)
	unregisterToolSink := p.registerToolEventSink(input, toolSink)
	defer unregisterToolSink()

	resources := promptResources(input)
	if injectHistory && input.HistoryResources != nil {
		// First prompt this runtime serves: the agent process has no memory of
		// the session, so replay the durable history ahead of the live context.
		history, histErr := input.HistoryResources()
		if histErr != nil {
			// Render failure must not burn the once-per-runtime flag: leave it
			// unset so the next prompt retries the replay.
			h.state.Lock()
			h.historyInjected = false
			h.state.Unlock()
			p.logger.Warn("ACP history replay unavailable; will retry on next prompt",
				slog.String("runtime_id", h.id),
				slog.String("session_id", strings.TrimSpace(input.SessionID)),
				slog.Any("error", histErr))
		} else if len(history) > 0 {
			resources = append(history, resources...)
		}
	}

	result, err := sess.PromptWithToolContext(turnCtx, input.Prompt, resources, toolCtx, toolSink)
	// The pool-level sink is the single merge point for session events and
	// Memoh MCP tool events; its ordered view supersedes the session's
	// internal collector, which only sees the former.
	result.Events = toolSink.Events()
	if err != nil {
		switch {
		case errors.Is(err, acpclient.ErrSessionClosed):
			if h.retired() {
				// The session is being deleted or its agent switched: failing
				// is correct; a transparent retry would resurrect a runtime
				// for a dead session.
				return result, false, ErrRuntimeRetired
			}
			// The session was closed under us for a benign reason before the
			// prompt reached the agent; retry resolves a fresh runtime.
			_ = p.teardown(h, closedReasonReplaced) //nolint:contextcheck // lifecycle close uses background ctx.
			return acpclient.PromptResult{}, true, nil
		case turnCtx.Err() != nil && sess.Alive():
			// The turn was aborted (caller cancellation or AbortTurn):
			// session/cancel is already on the wire and the process is
			// healthy. Keep the runtime warm so the agent-side conversation
			// context survives the interruption. An abort racing a process
			// death falls through to teardown — keeping a dead runtime pooled
			// would fail the next message instead.
			return result, false, fmt.Errorf("%w: %w", ErrPromptAborted, err)
		default:
			// Other prompt failures indicate the ACP process is in a bad state
			// (transport hang, agent crash); drop the runtime so the next call
			// starts fresh.
			_ = p.teardown(h, closedReasonReplaced) //nolint:contextcheck // lifecycle close uses background ctx.
			return result, false, err
		}
	}
	return result, false, nil
}

// AbortTurn cancels the session's in-flight turn out-of-band: any client (a
// reconnected tab, another device) can stop a running turn without owning the
// request that started it. The turn unwinds through the abort-keepalive path,
// so the runtime stays warm and the originating goroutine persists the round
// as cancelled. A non-empty turnID makes the abort precise: a stale client
// (snapshot of a finished turn, delayed click, lagging tab) cannot kill a
// newer turn it never saw. Returns ErrNoActiveTurn when nothing is running or
// the named turn already finished.
func (p *SessionPool) AbortTurn(botID, sessionID, turnID string) (RuntimeStatus, error) {
	if p == nil {
		return RuntimeStatus{}, ErrRuntimeNotFound
	}
	botID = strings.TrimSpace(botID)
	turnID = strings.TrimSpace(turnID)
	h := p.sessionHandle(sessionID)
	if h == nil || botID == "" || h.botID != botID {
		// Cross-bot references behave exactly like missing runtimes.
		return RuntimeStatus{}, ErrRuntimeNotFound
	}
	h.state.Lock()
	turn := h.turn
	if turn == nil || h.closed || (turnID != "" && turn.id != turnID) {
		h.state.Unlock()
		return RuntimeStatus{}, ErrNoActiveTurn
	}
	turn.state = turnStateCancelling
	cancel := turn.cancel
	h.state.Unlock()
	if cancel != nil {
		cancel()
	}
	return p.statusOf(h), nil
}

// Ensure starts (or reuses) the runtime for a session without prompting it.
//
//nolint:contextcheck // lifecycle close intentionally uses background ctx.
func (p *SessionPool) Ensure(ctx context.Context, input PromptInput) (RuntimeStatus, error) {
	input, err := p.prepareInput(ctx, input)
	if err != nil {
		return RuntimeStatus{}, err
	}
	p.reapIdle(time.Now())
	h, _, err := p.runtimeForSession(ctx, input, false)
	if err != nil {
		return RuntimeStatus{}, err
	}
	return p.statusOf(h), nil
}

// SetModel switches the model of the runtime bound to a session, cold
// starting one when needed.
//
//nolint:contextcheck // lifecycle close intentionally uses background ctx.
func (p *SessionPool) SetModel(ctx context.Context, input PromptInput, modelID string) (RuntimeStatus, error) {
	if strings.TrimSpace(modelID) == "" {
		return RuntimeStatus{}, acpclient.ErrModelIDRequired
	}
	input, err := p.prepareInput(ctx, input)
	if err != nil {
		return RuntimeStatus{}, err
	}
	p.reapIdle(time.Now())
	h, _, err := p.runtimeForSession(ctx, input, false)
	if err != nil {
		return RuntimeStatus{}, err
	}
	if err := p.setModelOnHandle(ctx, h, modelID); err != nil {
		return RuntimeStatus{}, err
	}
	return p.statusOf(h), nil
}

// runtimeForSession resolves the runtime bound to a session, cold starting
// and binding a fresh one when the index misses. A bound runtime whose agent
// or project no longer matches the session metadata is replaced. claimTurn
// pre-claims the turn slot for cold-started handles inside the same critical
// section that publishes them, so the prompt that paid for the cold start
// cannot be busy-rejected by a concurrent prompt that slipped in first;
// Ensure/SetModel pass false (they never prompt and must not occupy the
// slot).
func (p *SessionPool) runtimeForSession(ctx context.Context, input PromptInput, claimTurn bool) (*runtimeHandle, *runtimeTurn, error) {
	sessionID := strings.TrimSpace(input.SessionID)
	agentID := acpprofile.NormalizeAgentID(input.AgentID)
	if agentID == "" {
		agentID = acpprofile.AgentCodexID
	}
	projectPath := strings.TrimSpace(input.ProjectPath)

	for attempt := 0; attempt < 3; attempt++ {
		p.mu.Lock()
		var h *runtimeHandle
		if rid, ok := p.bySession[sessionID]; ok {
			h = p.runtimes[rid]
			if h == nil {
				delete(p.bySession, sessionID)
			}
		}
		if h == nil {
			// Register the starting handle and the session index atomically
			// so a concurrent caller waits on this start instead of racing a
			// second one.
			h = &runtimeHandle{
				id:           newRuntimeID(),
				botID:        input.BotID,
				agentID:      agentID,
				projectPath:  projectPath,
				status:       stateStarting,
				lastActive:   time.Now(),
				boundSession: sessionID,
			}
			var preclaimed *runtimeTurn
			if claimTurn {
				preclaimed = &runtimeTurn{
					id:        turnIDPrefix + uuid.NewString(),
					sessionID: sessionID,
					state:     turnStateRunning,
					startedAt: time.Now(),
				}
				h.turn = preclaimed
			}
			p.runtimes[h.id] = h
			p.bySession[sessionID] = h.id
			p.mu.Unlock()

			h.op.Lock()
			err := p.startRuntime(ctx, h, startOptions{
				ToolHTTPURL: input.ToolHTTPURL,
				Sink:        input.Sink,
			})
			h.op.Unlock()
			if err != nil {
				return nil, nil, err
			}
			return h, preclaimed, nil
		}
		p.mu.Unlock()

		if h.botID != input.BotID {
			// resolveSessionMetadata already pins the session to the calling
			// bot, so this is purely defensive — and side-effect free.
			return nil, nil, ErrRuntimeNotFound
		}
		h.state.Lock()
		matches := h.agentID == agentID && h.projectPath == projectPath
		closed := h.closed
		if matches && !closed {
			// Resolving counts as activity: a session whose UI keeps the
			// runtime ensured (without prompting) must not be idle-reaped.
			h.lastActive = time.Now()
		}
		h.state.Unlock()
		if matches && !closed {
			return h, nil, nil
		}
		// Agent or project changed for this session: replace the runtime.
		_ = p.closeHandle(h) //nolint:contextcheck // lifecycle close uses background ctx.
	}
	return nil, nil, errors.New("ACP runtime is restarting, retry the request")
}

type startOptions struct {
	ToolHTTPURL string
	Sink        acpclient.EventSink
}

// startRuntime boots the agent process for a registered handle. Must be
// called with h.op held. On failure the handle is fully torn down (process,
// maps, context) before returning.
//
//nolint:contextcheck // startup failure cleanup intentionally uses background ctx.
func (p *SessionPool) startRuntime(ctx context.Context, h *runtimeHandle, opts startOptions) error {
	startCtx, cancelStart := context.WithCancel(ctx)
	defer cancelStart()
	h.state.Lock()
	if h.closed {
		h.state.Unlock()
		return errors.New("ACP runtime was closed during startup")
	}
	h.startCancel = cancelStart
	h.state.Unlock()

	fail := func(err error) error {
		_ = p.teardown(h, closedReasonReplaced)
		return err
	}

	bot, err := p.bots.Get(startCtx, h.botID)
	if err != nil {
		return fail(fmt.Errorf("load bot ACP setup: %w", err))
	}
	setup := acpprofile.ParseAgentSetup(bot.Metadata, h.agentID)
	if !setup.Enabled {
		return fail(fmt.Errorf("ACP agent %q is not enabled for this bot", h.agentID))
	}
	profile, ok := acpprofile.Lookup(h.agentID)
	if !ok {
		return fail(fmt.Errorf("unknown ACP agent %q", h.agentID))
	}
	workspaceInfo, err := p.runner.WorkspaceInfo(startCtx, h.botID)
	if err != nil {
		return fail(fmt.Errorf("resolve workspace: %w", err))
	}

	mode := acpclient.SetupMode(setup.Mode)
	if !setup.ModeSet {
		// Legacy bots created before setup_mode was introduced have no explicit
		// mode. For local workspaces the host already has Codex/Claude configured,
		// so default to self (use host credentials). For container workspaces
		// default to api_key to preserve the original validation behaviour.
		if workspaceInfo.Backend == bridge.WorkspaceBackendLocal {
			mode = acpclient.SetupModeSelf
		} else {
			mode = acpclient.SetupModeAPIKey
		}
	}
	if mode != acpclient.SetupModeSelf {
		if err := validateManagedFields(profile, setup.Managed, mode); err != nil {
			return fail(err)
		}
	}
	if err := p.reconcileManagedCodexConfig(startCtx, h.botID, profile, setup, mode); err != nil {
		return fail(fmt.Errorf("prepare Codex managed config: %w", err))
	}
	// Managed env (Claude Code BYOK tokens) is injected for every backend.
	// Local processes inherit the host env and only get our overrides appended;
	// managedProcessEnv returns nil for self mode and for Codex (which is
	// configured via CODEX_HOME files instead of env), so this is safe to run
	// for local desktop workspaces too.
	var env []string
	env, err = managedProcessEnv(profile, setup.Managed, mode)
	if err != nil {
		return fail(err)
	}

	toolHTTPURL, err := p.resolveToolHTTPURL(opts.ToolHTTPURL, workspaceInfo)
	if err != nil {
		return fail(err)
	}

	// Self mode means "the host's own agent and configuration as-is": the
	// protocol pins (session mode, config options) would override the user's
	// own defaults, so they only apply to managed setups — the same rule the
	// managed settings and env already follow.
	sessionMode := profile.SessionModeID
	sessionConfigValues := profile.SessionConfigValues
	if mode == acpclient.SetupModeSelf {
		sessionMode = ""
		sessionConfigValues = nil
	}
	sess, err := p.runner.StartSession(startCtx, acpclient.StartRequest{
		AgentID:             h.agentID,
		BotID:               h.botID,
		ProjectPath:         h.projectPath,
		Command:             profile.Command,
		Args:                profile.Args,
		LocalCommand:        profile.LocalCommand,
		LocalArgs:           profile.LocalArgs,
		Env:                 env,
		SetupMode:           mode,
		SessionMode:         sessionMode,
		SessionConfigValues: sessionConfigValues,
		Timeout:             0,
		ToolHTTPURL:         toolHTTPURL,
		// The handler resolves identity from the handle per request, so the
		// process configuration only ever carries stable runtime identity.
		ToolHTTPHandler: p.toolHTTPHandler(h),
		ToolSession:     h.stableToolIdentity(),
		ToolApproval:    p.approval,
	}, opts.Sink)
	if err != nil {
		return fail(err)
	}

	h.state.Lock()
	if h.closed {
		h.state.Unlock()
		if closeErr := sess.Close(); closeErr != nil {
			p.logger.Warn("failed to close ACP session after startup cancellation",
				slog.Any("error", closeErr), slog.String("runtime_id", h.id))
		}
		return errors.New("ACP runtime was closed during startup")
	}
	h.session = sess
	h.status = stateIdle
	h.lastActive = time.Now()
	h.startCancel = nil
	h.defaultModelID = strings.TrimSpace(sess.ModelState().CurrentModelID)
	h.state.Unlock()
	return nil
}

// RuntimeStatus reports the runtime state for a session, returning an idle
// skeleton when no runtime is live.
func (p *SessionPool) RuntimeStatus(sessionID, agentID, projectPath string) RuntimeStatus {
	sessionID = strings.TrimSpace(sessionID)
	idle := RuntimeStatus{
		SessionID:   sessionID,
		AgentID:     strings.TrimSpace(agentID),
		ProjectPath: strings.TrimSpace(projectPath),
		State:       stateIdle,
	}
	if p == nil {
		return idle
	}
	h := p.sessionHandle(sessionID)
	if h == nil {
		return idle
	}
	status := p.statusOf(h)
	if status.SessionID == "" {
		status.SessionID = sessionID
	}
	return status
}

func (p *SessionPool) sessionHandle(sessionID string) *runtimeHandle {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	rid, ok := p.bySession[sessionID]
	if !ok {
		return nil
	}
	return p.runtimes[rid]
}

func (*SessionPool) statusOf(h *runtimeHandle) RuntimeStatus {
	h.state.Lock()
	sess := h.session
	status := RuntimeStatus{
		RuntimeID:      h.id,
		SessionID:      h.boundSession,
		AgentID:        h.agentID,
		ProjectPath:    h.projectPath,
		State:          h.status,
		DefaultModelID: h.defaultModelID,
	}
	if !h.closed && h.turn != nil {
		status.TurnID = h.turn.id
		status.TurnState = h.turn.state
	}
	h.state.Unlock()
	switch status.State {
	case stateStarting:
		status.State = stateActive
	case stateClosed, "":
		status.State = stateIdle
	}
	if sess != nil {
		status.ACPSession = sess.ID()
		modelState := sess.ModelState()
		status.Models = &modelState
	}
	return status
}

// IsSessionActive reports whether the session's runtime is currently serving
// an operation.
func (p *SessionPool) IsSessionActive(sessionID string) bool {
	if p == nil {
		return false
	}
	h := p.sessionHandle(sessionID)
	if h == nil {
		return false
	}
	if !h.op.TryLock() {
		return true
	}
	h.op.Unlock()
	h.state.Lock()
	active := h.status == stateActive
	h.state.Unlock()
	return active
}

func (p *SessionPool) StartReaper(ctx context.Context) {
	if p == nil {
		return
	}
	ticker := time.NewTicker(time.Minute)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				p.reapIdle(time.Now()) //nolint:contextcheck // reaper close uses its own background ctx.
			case <-ctx.Done():
				return
			}
		}
	}()
}

// CloseSession tears down the runtime bound to a session (used when the
// session is deleted or its agent changes). Session IDs reaching this path
// are database-validated by the caller.
//
//nolint:contextcheck // lifecycle close intentionally uses background ctx so cleanup runs after caller cancels.
func (p *SessionPool) CloseSession(sessionID string) error {
	if p == nil {
		return nil
	}
	h := p.sessionHandle(sessionID)
	if h == nil {
		return nil
	}
	return p.closeHandle(h)
}

// closeHandle interrupts any in-flight operation, then destroys the runtime.
// An in-flight start is aborted (marked closed + cancelled) so the start
// unwinds instead of completing into a closed handle; an in-flight prompt is
// interrupted by closing the session — Close cancels the prompt context
// (which puts session/cancel on the wire) and returns within ~500ms — so op
// frees promptly instead of after the remaining prompt duration. Callers like
// the session DELETE handler therefore never block behind a long agent turn.
func (p *SessionPool) closeHandle(h *runtimeHandle) error {
	h.state.Lock()
	// Record the retirement intent before interrupting anything: an in-flight
	// prompt unwinding from here on must fail instead of transparently
	// recreating a runtime for a session that is being deleted or switched.
	if h.closedReason == "" {
		h.closedReason = closedReasonRetired
	}
	var sess *acpclient.Session
	if h.session == nil && !h.closed && h.startCancel != nil {
		h.closed = true
		h.status = stateClosed
		cancel := h.startCancel
		h.startCancel = nil
		h.state.Unlock()
		cancel()
	} else {
		sess = h.session
		h.state.Unlock()
	}
	var closeErr error
	if sess != nil {
		closeErr = sess.Close()
	}

	h.op.Lock()
	defer h.op.Unlock()
	if err := p.teardown(h, closedReasonRetired); err != nil {
		return err
	}
	return closeErr
}

// tryCloseIdle closes the handle only when it is idle and has been inactive
// for at least minIdle. Never blocks: a runtime that is busy (or becomes
// busy) is skipped, which is what makes it safe for the reaper and the
// eviction path. requireUnbound re-checks, inside the critical section, that
// the handle is still unbound — the budget eviction path selects its victim
// under p.mu only, so a runtime bound between selection and close must be
// spared.
func (p *SessionPool) tryCloseIdle(h *runtimeHandle, minIdle time.Duration, requireUnbound bool) bool {
	if !h.op.TryLock() {
		return false
	}
	defer h.op.Unlock()
	h.state.Lock()
	eligible := !h.closed && h.status == stateIdle &&
		(minIdle <= 0 || time.Since(h.lastActive) > minIdle) &&
		(!requireUnbound || h.boundSession == "")
	h.state.Unlock()
	if !eligible {
		return false
	}
	if err := p.teardown(h, closedReasonReplaced); err != nil {
		p.logger.Warn("failed to close idle ACP runtime",
			slog.Any("error", err), slog.String("runtime_id", h.id))
	}
	return true
}

// teardown is the single destruction path for a runtime: it marks the handle
// closed (recording why — first writer's reason wins), cancels a pending
// start and any in-flight turn, kills the agent process, and removes the
// handle from both pool indexes. Idempotent — and it always re-runs the map
// cleanup, because a handle can be marked closed (aborted start) before its
// registration is removed.
func (p *SessionPool) teardown(h *runtimeHandle, reason string) error {
	h.state.Lock()
	h.closed = true
	if h.closedReason == "" {
		h.closedReason = reason
	}
	h.status = stateClosed
	sess := h.session
	h.session = nil
	cancel := h.startCancel
	h.startCancel = nil
	h.active = nil
	turn := h.turn
	// Clear the turn under the same lock that sets stateClosed so a concurrent
	// statusOf can never observe "closed + running turn" (an impossible state).
	h.turn = nil
	bound := h.boundSession
	h.state.Unlock()

	if turn != nil && turn.cancel != nil {
		// Unblock an in-flight prompt promptly instead of relying on the
		// session erroring under it (matters for CloseAll, which tears down
		// without closing the session first).
		turn.cancel()
	}

	p.mu.Lock()
	delete(p.runtimes, h.id)
	if bound != "" && p.bySession[bound] == h.id {
		delete(p.bySession, bound)
	}
	p.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if sess != nil {
		return sess.Close()
	}
	return nil
}

func (p *SessionPool) CloseAll() {
	if p == nil {
		return
	}
	p.mu.RLock()
	handles := make([]*runtimeHandle, 0, len(p.runtimes))
	for _, h := range p.runtimes {
		if h != nil {
			handles = append(handles, h)
		}
	}
	p.mu.RUnlock()
	for _, h := range handles {
		// Shutdown must not wait for in-flight prompts: teardown directly,
		// the op holder unwinds via the cancelled turn and the closed flag.
		if err := p.teardown(h, closedReasonRetired); err != nil {
			p.logger.Warn("failed to close ACP runtime",
				slog.Any("error", err), slog.String("runtime_id", h.id))
		}
	}
}

func (p *SessionPool) reapIdle(now time.Time) int {
	if p == nil {
		return 0
	}
	p.mu.RLock()
	handles := make([]*runtimeHandle, 0, len(p.runtimes))
	for _, h := range p.runtimes {
		if h != nil {
			handles = append(handles, h)
		}
	}
	p.mu.RUnlock()

	reaped := 0
	for _, h := range handles {
		h.state.Lock()
		limit := unboundRuntimeIdleTimeout
		if h.boundSession != "" {
			limit = p.timeout
		}
		stale := !h.closed && h.status == stateIdle && !h.lastActive.IsZero() &&
			limit > 0 && now.Sub(h.lastActive) > limit
		h.state.Unlock()
		if !stale {
			continue
		}
		if p.tryCloseIdle(h, limit, false) {
			reaped++
		}
	}
	return reaped
}

func (p *SessionPool) resolveSessionMetadata(ctx context.Context, input PromptInput) (PromptInput, error) {
	if p == nil || p.store == nil {
		return input, nil
	}
	sess, err := p.store.Get(ctx, input.SessionID)
	if err != nil {
		return input, fmt.Errorf("load ACP session metadata: %w", err)
	}
	if sess.Type != session.TypeACPAgent {
		return input, fmt.Errorf("session %s is not an ACP agent session", input.SessionID)
	}
	if input.BotID != "" && sess.BotID != "" && input.BotID != sess.BotID {
		return input, fmt.Errorf("session %s does not belong to bot %s", input.SessionID, input.BotID)
	}
	if input.BotID == "" {
		input.BotID = sess.BotID
	}
	input.SessionType = sess.Type
	if agentID := metadataString(sess.Metadata, "acp_agent_id"); agentID != "" {
		input.AgentID = agentID
	}
	if projectPath := metadataString(sess.Metadata, "project_path"); projectPath != "" {
		input.ProjectPath = projectPath
	}
	return input, nil
}

// stableToolIdentity is the only identity baked into the agent process
// configuration (MCP HTTP headers): the runtime ID never changes for the
// life of the process, so binding the runtime to a session later requires no
// re-configuration.
func (h *runtimeHandle) stableToolIdentity() acpclient.ToolSessionContext {
	return acpclient.ToolSessionContext{
		BotID:       h.botID,
		ChatID:      h.botID,
		RuntimeID:   h.id,
		SessionType: session.TypeACPAgent,
		// MCP image content in tool results is protocol-standard; ACP agents
		// opt in so read_media et al. return real images instead of refusing.
		SupportsImageInput: true,
	}
}

// toolContext resolves the trusted MCP tool context for the runtime: stable
// identity plus, while a prompt is running, the per-prompt fields (stream,
// token, chat, reply target...).
func (h *runtimeHandle) toolContext() mcp.ToolSessionContext {
	h.state.Lock()
	defer h.state.Unlock()
	ctx := mcp.ToolSessionContext{
		BotID:              h.botID,
		ChatID:             h.botID,
		RuntimeID:          h.id,
		SessionID:          h.boundSession,
		SessionType:        session.TypeACPAgent,
		SupportsImageInput: true,
	}
	if h.active == nil {
		return ctx
	}
	ctx.RuntimeActive = true
	overlay := func(dst *string, value string) {
		if value = strings.TrimSpace(value); value != "" {
			*dst = value
		}
	}
	overlay(&ctx.ChatID, h.active.ChatID)
	overlay(&ctx.SessionID, h.active.SessionID)
	overlay(&ctx.StreamID, h.active.StreamID)
	overlay(&ctx.SessionType, h.active.SessionType)
	overlay(&ctx.RouteID, h.active.RouteID)
	overlay(&ctx.ChannelIdentityID, h.active.ChannelIdentityID)
	overlay(&ctx.SessionToken, h.active.SessionToken)
	overlay(&ctx.CurrentPlatform, h.active.CurrentPlatform)
	overlay(&ctx.ReplyTarget, h.active.ReplyTarget)
	overlay(&ctx.ConversationType, h.active.ConversationType)
	overlay(&ctx.Timezone, h.active.Timezone)
	return ctx
}

func (h *runtimeHandle) clearActive() {
	h.state.Lock()
	h.active = nil
	if !h.closed {
		h.status = stateIdle
	}
	h.lastActive = time.Now()
	h.state.Unlock()
}

func (h *runtimeHandle) setStatus(status string) {
	h.state.Lock()
	if !h.closed {
		h.status = status
	}
	h.lastActive = time.Now()
	h.state.Unlock()
}

func toolSessionContext(input PromptInput, h *runtimeHandle) acpclient.ToolSessionContext {
	return acpclient.ToolSessionContext{
		BotID:              h.botID,
		ChatID:             firstNonEmpty(input.ChatID, h.botID),
		RuntimeID:          h.id,
		SessionID:          strings.TrimSpace(input.SessionID),
		StreamID:           strings.TrimSpace(input.StreamID),
		SessionType:        firstNonEmpty(input.SessionType, session.TypeACPAgent),
		RouteID:            input.RouteID,
		ChannelIdentityID:  input.ChannelIdentityID,
		SessionToken:       input.SessionToken,
		CurrentPlatform:    input.CurrentPlatform,
		ReplyTarget:        input.ReplyTarget,
		ConversationType:   input.ConversationType,
		IsSubagent:         false,
		SupportsImageInput: true,
		Timezone:           strings.TrimSpace(input.Timezone),
	}
}

func promptResources(input PromptInput) []acpclient.PromptResource {
	markdown := strings.TrimSpace(input.ContextMarkdown)
	if markdown == "" {
		return nil
	}
	uri := strings.TrimSpace(input.ContextURI)
	if uri == "" {
		uri = "memoh://context/current-turn"
	}
	return []acpclient.PromptResource{{
		URI:      uri,
		MimeType: "text/markdown",
		Text:     markdown,
	}}
}

func (p *SessionPool) registerToolEventSink(input PromptInput, sink *promptToolEventSink) func() {
	if p == nil || p.contexts == nil || sink == nil {
		return func() {}
	}
	return p.contexts.RegisterToolEventSink(acpclient.ToolSessionContext{
		BotID:     input.BotID,
		SessionID: input.SessionID,
		StreamID:  input.StreamID,
	}, sink.EmitToolStreamEvent)
}

func (p *SessionPool) resolveToolHTTPURL(inputURL string, workspaceInfo bridge.WorkspaceInfo) (string, error) {
	if p == nil || p.tools == nil {
		return "", nil
	}
	backend := strings.TrimSpace(workspaceInfo.Backend)
	if backend == bridge.WorkspaceBackendLocal {
		return strings.TrimSpace(inputURL), nil
	}
	if backend == "" || backend == bridge.WorkspaceBackendContainer {
		return strings.TrimSpace(workspaceInfo.ACPToolsHTTPURL), nil
	}
	return strings.TrimSpace(inputURL), nil
}

// toolHTTPHandler serves the runtime's MCP tool requests. Identity comes
// from the handle (stable identity plus the live per-prompt context), never
// from request headers, so a runtime can be bound to a session after start
// without any re-configuration.
func (p *SessionPool) toolHTTPHandler(h *runtimeHandle) http.Handler {
	if p == nil || p.tools == nil {
		return nil
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mcp.ServeToolMCPHTTPWithoutContextMerge(w, req, p.logger, p.tools, p.contexts, h.toolContext())
	})
}

func (p *SessionPool) reconcileManagedCodexConfig(ctx context.Context, botID string, profile acpprofile.Profile, setup acpprofile.AgentSetup, mode acpclient.SetupMode) error {
	if profile.ManagedConfig != acpprofile.ManagedConfigCodex || mode == acpclient.SetupModeSelf {
		return nil
	}
	runner, ok := p.runner.(workspaceClientRunner)
	if !ok {
		return nil
	}
	client, err := runner.MCPClient(ctx, botID)
	if err != nil {
		return err
	}
	cfg := acpclient.CodexManagedConfig{
		Mode:    mode,
		Managed: setup.Managed,
	}
	if mode == acpclient.SetupModeOAuth {
		return acpclient.WriteCodexManagedConfigFile(ctx, client, cfg)
	}
	return acpclient.WriteCodexManagedConfigWithAuth(ctx, client, cfg)
}

type promptToolEventSink struct {
	mu     sync.Mutex
	next   acpclient.EventSink
	events []acpclient.StreamEvent
}

func newPromptToolEventSink(next acpclient.EventSink) *promptToolEventSink {
	return &promptToolEventSink{next: next}
}

// maxPromptToolEvents mirrors acpclient's collector bound: the sink's ordered
// view supersedes that collector, so it must carry the same cap or a long
// turn buffers unboundedly.
const maxPromptToolEvents = 4096

func (s *promptToolEventSink) EmitACPEvent(event acpclient.StreamEvent) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
	if len(s.events) > maxPromptToolEvents {
		s.events = append([]acpclient.StreamEvent(nil), s.events[len(s.events)-maxPromptToolEvents:]...)
	}
	// Emit to the live sink under the same lock that ordered the append, so the
	// persisted snapshot (s.events) and the live stream observe events in the
	// SAME order even when the session goroutine and the MCP tool goroutine race
	// at this merge point. The downstream sink is ctx-bounded (it never blocks
	// indefinitely) and never re-enters this sink, so holding the lock is safe.
	if s.next != nil {
		s.next.EmitACPEvent(event)
	}
}

func (s *promptToolEventSink) EmitToolStreamEvent(event mcp.ToolStreamEvent) {
	if s == nil {
		return
	}
	typ := acpclient.StreamEventType(strings.TrimSpace(event.Type))
	switch typ {
	case acpclient.StreamEventToolCallStart, acpclient.StreamEventToolCallEnd:
		s.EmitACPEvent(acpclient.StreamEvent{
			Type:       typ,
			ToolCallID: event.ToolCallID,
			ToolName:   event.ToolName,
			Input:      event.Input,
			Result:     event.Result,
			Error:      event.Error,
		})
	case acpclient.StreamEventUserInputRequest:
		s.EmitACPEvent(acpclient.StreamEvent{
			Type:        typ,
			ToolCallID:  event.ToolCallID,
			ToolName:    event.ToolName,
			Input:       event.Input,
			UserInputID: event.UserInputID,
			ShortID:     event.ShortID,
			Status:      event.Status,
			Metadata:    event.Metadata,
		})
	case acpclient.StreamEventToolApprovalRequest:
		// Approvals for native tools the agent called over MCP: same canonical
		// path as the ACP client's own permission emits, so the persisted
		// round and the turn mirror carry the approval metadata.
		s.EmitACPEvent(acpclient.StreamEvent{
			Type:       typ,
			ToolCallID: event.ToolCallID,
			ToolName:   event.ToolName,
			Input:      event.Input,
			ApprovalID: event.ApprovalID,
			ShortID:    event.ShortID,
			Status:     event.Status,
			Metadata:   event.Metadata,
		})
	case acpclient.StreamEventAttachment, acpclient.StreamEventReaction, acpclient.StreamEventSpeech:
		// Tool side-effects (attachments, reactions, TTS) bridged from native
		// tools the agent called over MCP: same surfaces as the in-process
		// agent loop.
		s.EmitACPEvent(acpclient.StreamEvent{
			Type:        typ,
			Attachments: event.Attachments,
			Reactions:   event.Reactions,
			Speeches:    event.Speeches,
		})
	}
}

func (s *promptToolEventSink) Events() []acpclient.StreamEvent {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]acpclient.StreamEvent(nil), s.events...)
}

func validateManagedFields(profile acpprofile.Profile, values map[string]string, mode acpclient.SetupMode) error {
	// Profiles declare their per-mode requirements (architecture doc §5.6);
	// modes absent from the declaration fall back to the ManagedFields
	// Required flags. OAuth defaults to api_key-mode requirements only when
	// neither is declared.
	if required, ok := profile.RequiredManagedFields[normalizedModeKey(mode)]; ok {
		for _, field := range required {
			if strings.TrimSpace(values[field]) == "" {
				return fmt.Errorf("%s required for %s %s setup", field, profile.DisplayName, normalizedModeKey(mode))
			}
		}
		return nil
	}
	for _, field := range profile.ManagedFields {
		if !field.Required {
			continue
		}
		if strings.TrimSpace(values[field.ID]) == "" {
			return fmt.Errorf("%s required for %s %s setup", field.ID, profile.DisplayName, mode)
		}
	}
	return nil
}

// normalizedModeKey maps a setup mode onto the RequiredManagedFields key
// space: anything that is not oauth/self behaves as api_key, matching
// acpclient's own mode normalization.
func normalizedModeKey(mode acpclient.SetupMode) string {
	switch mode {
	case acpclient.SetupModeOAuth:
		return acpprofile.SetupModeOAuth
	default:
		return acpprofile.SetupModeAPIKey
	}
}

func managedProcessEnv(profile acpprofile.Profile, values map[string]string, mode acpclient.SetupMode) ([]string, error) {
	switch profile.ManagedEnv {
	case acpprofile.ManagedEnvClaude:
		env := []string{
			"ANTHROPIC_AUTH_TOKEN=",
			"CLAUDE_CODE_USE_BEDROCK=",
			"CLAUDE_CODE_USE_VERTEX=",
			"CLAUDE_CODE_USE_FOUNDRY=",
		}
		if profile.ManagedThinkingTokens > 0 {
			// Claude Code does not think unless given a budget; this is the
			// counterpart of Codex's model_reasoning_effort in config.toml so
			// managed sessions stream reasoning by default. The budget is
			// declared per-agent in acpprofile, not hardcoded here.
			env = append(env, fmt.Sprintf("MAX_THINKING_TOKENS=%d", profile.ManagedThinkingTokens))
		}
		switch mode {
		case acpclient.SetupModeAPIKey:
			apiKey := strings.TrimSpace(values["api_key"])
			if apiKey == "" {
				return nil, fmt.Errorf("api_key required for %s api_key setup", profile.DisplayName)
			}
			env = append(env,
				"CLAUDE_CODE_OAUTH_TOKEN=",
				"ANTHROPIC_API_KEY="+apiKey,
			)
		case acpclient.SetupModeOAuth:
			token := strings.TrimSpace(values["oauth_token"])
			if token == "" {
				return nil, fmt.Errorf("oauth_token required for %s oauth setup", profile.DisplayName)
			}
			env = append(env,
				"ANTHROPIC_API_KEY=",
				"CLAUDE_CODE_OAUTH_TOKEN="+token,
			)
		default:
			return nil, nil
		}
		if baseURL := strings.TrimSpace(values["base_url"]); baseURL != "" {
			env = append(env, "ANTHROPIC_BASE_URL="+baseURL)
		}
		// Optional BYOK model override: when the managed config names a model,
		// pin it via ANTHROPIC_MODEL so the session targets a model the user's
		// endpoint actually serves. Custom Anthropic-compatible proxies often
		// expose only a subset of models (or the operator wants a cheaper model
		// to bound per-request cost — proxies pre-authorize a hold of
		// max_tokens × price, so an expensive default can exceed a small
		// balance even when the real call is cheap). Omitted → the adapter's
		// own default model.
		if model := strings.TrimSpace(values["model"]); model != "" {
			env = append(env, "ANTHROPIC_MODEL="+model)
		}
		return env, nil
	default:
		return nil, nil
	}
}

func metadataString(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	value, _ := metadata[key].(string)
	return strings.TrimSpace(value)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
