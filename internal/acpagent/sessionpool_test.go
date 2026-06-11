package acpagent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/memohai/memoh/internal/acpclient"
	"github.com/memohai/memoh/internal/acpprofile"
	"github.com/memohai/memoh/internal/bots"
	"github.com/memohai/memoh/internal/config"
	"github.com/memohai/memoh/internal/mcp"
	sessionpkg "github.com/memohai/memoh/internal/session"
	"github.com/memohai/memoh/internal/streamevent"
	"github.com/memohai/memoh/internal/workspace/bridge"
	pb "github.com/memohai/memoh/internal/workspace/bridgepb"
	"github.com/memohai/memoh/internal/workspace/bridgesvc"
)

// injectRuntime registers a hand-built handle for tests that exercise
// internal state without booting a real agent process.
func injectRuntime(p *SessionPool, h *runtimeHandle) {
	p.mu.Lock()
	p.runtimes[h.id] = h
	if h.boundSession != "" {
		p.bySession[h.boundSession] = h.id
	}
	p.mu.Unlock()
}

func newFakeScriptPool(t *testing.T) *SessionPool {
	pool, _ := newFakeScriptPoolForBot(t, enabledACPBot("bot-1", "api_key", map[string]any{"api_key": "sk-local-byok"}))
	return pool
}

func newFakeScriptPoolForBot(t *testing.T, bot bots.Bot) (*SessionPool, string) {
	t.Helper()
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.MkdirAll(project, 0o750); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o750); err != nil {
		t.Fatal(err)
	}
	writeSessionPoolFakeAgentScript(t, binDir, "npx")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	runner := acpclient.NewRunner(nil, sessionPoolWorkspace{
		client: newSessionPoolBridgeClient(t, root),
		info: bridge.WorkspaceInfo{
			Backend:        bridge.WorkspaceBackendLocal,
			DefaultWorkDir: root,
		},
	})
	pool := newSessionPool(nil, runner, fakeBotGetter{bot: bot})
	t.Cleanup(pool.CloseAll)
	return pool, root
}

func TestSessionPoolPromptColdStartsBindsAndReuses(t *testing.T) {
	pool := newFakeScriptPool(t)
	pool.timeout = time.Hour

	input := PromptInput{
		BotID:           "bot-1",
		SessionID:       "session-1",
		StreamID:        "stream-1",
		AgentID:         acpprofile.AgentCodexID,
		ProjectPath:     "/data/project",
		Prompt:          "first prompt",
		CurrentPlatform: "web",
	}
	result, err := pool.Prompt(context.Background(), input)
	if err != nil {
		t.Fatalf("Prompt(first) error = %v", err)
	}
	if !strings.Contains(result.Text, "session-pool-ok") {
		t.Fatalf("first result text = %q", result.Text)
	}
	first := pool.sessionHandle("session-1")
	if first == nil || first.session == nil {
		t.Fatalf("cold start did not register a bound runtime")
	}
	if !strings.HasPrefix(first.id, runtimeIDPrefix) {
		t.Fatalf("runtime id = %q, want server-generated %q prefix", first.id, runtimeIDPrefix)
	}
	if first.boundSession != "session-1" {
		t.Fatalf("cold-start runtime bound to %q, want session-1", first.boundSession)
	}
	first.state.Lock()
	activeAfter := first.active
	statusAfter := first.status
	first.state.Unlock()
	if activeAfter != nil || statusAfter != stateIdle {
		t.Fatalf("per-prompt context not cleared after prompt: active=%v status=%q", activeAfter, statusAfter)
	}

	input.Prompt = "second prompt"
	if _, err := pool.Prompt(context.Background(), input); err != nil {
		t.Fatalf("Prompt(second) error = %v", err)
	}
	if got := pool.sessionHandle("session-1"); got != first {
		t.Fatalf("same session started a new runtime")
	}

	input.SessionID = "session-2"
	input.Prompt = "third prompt"
	if _, err := pool.Prompt(context.Background(), input); err != nil {
		t.Fatalf("Prompt(third) error = %v", err)
	}
	if got := pool.sessionHandle("session-2"); got == nil || got == first {
		t.Fatalf("different session did not get an independent runtime")
	}

	status := pool.RuntimeStatus("session-1", "", "")
	if status.State != "idle" || status.ACPSession == "" || status.ProjectPath != "/data/project" || status.RuntimeID != first.id {
		t.Fatalf("RuntimeStatus() = %#v", status)
	}
	if err := pool.CloseSession("session-1"); err != nil {
		t.Fatalf("CloseSession() error = %v", err)
	}
	if pool.sessionHandle("session-1") != nil {
		t.Fatalf("CloseSession did not remove the runtime")
	}
	pool.mu.RLock()
	_, stillRegistered := pool.runtimes[first.id]
	pool.mu.RUnlock()
	if stillRegistered {
		t.Fatalf("CloseSession left the handle registered")
	}
}

// TestPromptToolEventSinkBridgesToolSideEffects guards the gateway event
// bridge: attachment/reaction/speech events emitted by native tools the agent
// called over MCP must reach the ACP event stream, not be dropped by the
// lifecycle whitelist.
func TestPromptToolEventSinkBridgesToolSideEffects(t *testing.T) {
	t.Parallel()

	var forwarded []acpclient.StreamEvent
	sink := newPromptToolEventSink(acpclient.EventSinkFunc(func(event acpclient.StreamEvent) {
		forwarded = append(forwarded, event)
	}))

	sink.EmitToolStreamEvent(mcp.ToolStreamEvent{
		Type:        string(streamevent.Attachment),
		Attachments: []streamevent.FileAttachment{{Type: "image", Path: "/data/shot.png", Mime: "image/png"}},
	})
	sink.EmitToolStreamEvent(mcp.ToolStreamEvent{
		Type:     string(streamevent.Speech),
		Speeches: []streamevent.SpeechItem{{Text: "done"}},
	})
	sink.EmitToolStreamEvent(mcp.ToolStreamEvent{Type: "spawn_heartbeat"}) // not bridged

	events := sink.Events()
	if len(events) != 2 || len(forwarded) != 2 {
		t.Fatalf("bridged events = %d recorded / %d forwarded, want 2/2: %#v", len(events), len(forwarded), events)
	}
	if events[0].Type != streamevent.Attachment || len(events[0].Attachments) != 1 || events[0].Attachments[0].Path != "/data/shot.png" {
		t.Fatalf("attachment event = %#v", events[0])
	}
	if events[1].Type != streamevent.Speech || len(events[1].Speeches) != 1 {
		t.Fatalf("speech event = %#v", events[1])
	}
}

// waitForSessionActive polls until the session's runtime is serving a prompt.
func waitForSessionActive(t *testing.T, pool *SessionPool, sessionID string) *runtimeHandle {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if h := pool.sessionHandle(sessionID); h != nil {
			h.state.Lock()
			active := h.status == stateActive
			h.state.Unlock()
			if active {
				return h
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("runtime for %s never became active", sessionID)
	return nil
}

func TestSessionPoolPromptAbortKeepsRuntimeWarm(t *testing.T) {
	pool := newFakeScriptPool(t)
	pool.timeout = time.Hour

	input := PromptInput{
		BotID:           "bot-1",
		SessionID:       "session-1",
		StreamID:        "stream-1",
		AgentID:         acpprofile.AgentCodexID,
		ProjectPath:     "/data/project",
		Prompt:          "warm up",
		CurrentPlatform: "web",
	}
	if _, err := pool.Prompt(context.Background(), input); err != nil {
		t.Fatalf("Prompt(warm up) error = %v", err)
	}
	first := pool.sessionHandle("session-1")
	if first == nil {
		t.Fatal("no runtime after first prompt")
	}

	promptCtx, cancelPrompt := context.WithCancel(context.Background())
	defer cancelPrompt()
	type promptOutcome struct {
		result acpclient.PromptResult
		err    error
	}
	outcome := make(chan promptOutcome, 1)
	hang := input
	hang.Prompt = "please [hang] until cancelled"
	go func() {
		result, err := pool.Prompt(promptCtx, hang)
		outcome <- promptOutcome{result: result, err: err}
	}()
	waitForSessionActive(t, pool, "session-1")
	cancelPrompt()

	var got promptOutcome
	select {
	case got = <-outcome:
	case <-time.After(10 * time.Second):
		t.Fatal("aborted prompt did not return")
	}
	if !errors.Is(got.err, ErrPromptAborted) {
		t.Fatalf("aborted prompt error = %v, want ErrPromptAborted", got.err)
	}
	if got.result.StopReason != "cancelled" {
		t.Fatalf("aborted prompt stop reason = %q, want cancelled", got.result.StopReason)
	}

	// The abort must keep the runtime warm: same handle, idle, not closed.
	after := pool.sessionHandle("session-1")
	if after == nil {
		t.Fatal("abort tore the runtime down")
	}
	if after != first {
		t.Fatal("abort replaced the runtime")
	}
	after.state.Lock()
	closed := after.closed
	status := after.status
	after.state.Unlock()
	if closed || status != stateIdle {
		t.Fatalf("runtime after abort: closed=%v status=%q, want live idle", closed, status)
	}

	// And the same warm runtime serves the next prompt.
	followUp := input
	followUp.Prompt = "after abort"
	result, err := pool.Prompt(context.Background(), followUp)
	if err != nil {
		t.Fatalf("Prompt(after abort) error = %v", err)
	}
	if !strings.Contains(result.Text, "session-pool-ok") {
		t.Fatalf("follow-up result text = %q", result.Text)
	}
	if pool.sessionHandle("session-1") != first {
		t.Fatal("follow-up prompt cold-started a new runtime")
	}
}

func TestSessionPoolCloseSessionInterruptsInFlightPrompt(t *testing.T) {
	pool := newFakeScriptPool(t)
	pool.timeout = time.Hour

	input := PromptInput{
		BotID:           "bot-1",
		SessionID:       "session-1",
		StreamID:        "stream-1",
		AgentID:         acpprofile.AgentCodexID,
		ProjectPath:     "/data/project",
		Prompt:          "[hang] forever",
		CurrentPlatform: "web",
	}
	promptErr := make(chan error, 1)
	go func() {
		_, err := pool.Prompt(context.Background(), input)
		promptErr <- err
	}()
	waitForSessionActive(t, pool, "session-1")

	closeErr := make(chan error, 1)
	go func() { closeErr <- pool.CloseSession("session-1") }()
	select {
	case err := <-closeErr:
		if err != nil {
			t.Fatalf("CloseSession() error = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("CloseSession blocked behind the in-flight prompt")
	}
	select {
	case err := <-promptErr:
		if err == nil {
			t.Fatal("interrupted prompt returned nil error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("interrupted prompt never unwound")
	}
	if pool.sessionHandle("session-1") != nil {
		t.Fatal("runtime still registered after CloseSession")
	}
}

func TestSessionPoolPromptBusyRejectsConcurrentPrompt(t *testing.T) {
	pool := newFakeScriptPool(t)
	pool.timeout = time.Hour

	input := PromptInput{
		BotID:           "bot-1",
		SessionID:       "session-1",
		StreamID:        "stream-1",
		AgentID:         acpprofile.AgentCodexID,
		ProjectPath:     "/data/project",
		Prompt:          "[hang] long turn",
		CurrentPlatform: "web",
	}
	firstErr := make(chan error, 1)
	go func() {
		_, err := pool.Prompt(context.Background(), input)
		firstErr <- err
	}()
	waitForSessionActive(t, pool, "session-1")

	// The second prompt must report busy immediately — not queue behind the
	// running turn on an uncancellable mutex.
	second := input
	second.Prompt = "while busy"
	_, err := pool.Prompt(context.Background(), second)
	if !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("concurrent prompt error = %v, want ErrSessionBusy", err)
	}
	status := pool.RuntimeStatus("session-1", "", "")
	if status.TurnID == "" || status.TurnState != turnStateRunning {
		t.Fatalf("busy runtime status turn = %q/%q, want running turn", status.TurnID, status.TurnState)
	}

	// Aborting the turn frees the slot for the next prompt.
	if _, err := pool.AbortTurn("bot-1", "session-1", ""); err != nil {
		t.Fatalf("AbortTurn() error = %v", err)
	}
	select {
	case err := <-firstErr:
		if !errors.Is(err, ErrPromptAborted) {
			t.Fatalf("aborted turn error = %v, want ErrPromptAborted", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("aborted turn never unwound")
	}
	third := input
	third.Prompt = "after abort"
	result, err := pool.Prompt(context.Background(), third)
	if err != nil {
		t.Fatalf("Prompt(after abort) error = %v", err)
	}
	if !strings.Contains(result.Text, "session-pool-ok") {
		t.Fatalf("post-abort result text = %q", result.Text)
	}
}

func TestSessionPoolAbortTurnCancelsHeadlessPrompt(t *testing.T) {
	pool := newFakeScriptPool(t)
	pool.timeout = time.Hour

	input := PromptInput{
		BotID:           "bot-1",
		SessionID:       "session-1",
		StreamID:        "stream-1",
		AgentID:         acpprofile.AgentCodexID,
		ProjectPath:     "/data/project",
		Prompt:          "[hang] headless turn",
		CurrentPlatform: "web",
	}
	type promptOutcome struct {
		result acpclient.PromptResult
		err    error
	}
	outcome := make(chan promptOutcome, 1)
	// Background ctx simulates a turn whose originating connection is gone:
	// nobody holds a cancellable ctx for it anymore.
	go func() {
		result, err := pool.Prompt(context.Background(), input)
		outcome <- promptOutcome{result: result, err: err}
	}()
	first := waitForSessionActive(t, pool, "session-1")

	if _, err := pool.AbortTurn("bot-2", "session-1", ""); !errors.Is(err, ErrRuntimeNotFound) {
		t.Fatalf("cross-bot AbortTurn error = %v, want ErrRuntimeNotFound", err)
	}
	// A wrong turn id must not kill the running turn (stale-client guard).
	if _, err := pool.AbortTurn("bot-1", "session-1", "turn_stale"); !errors.Is(err, ErrNoActiveTurn) {
		t.Fatalf("stale-turn AbortTurn error = %v, want ErrNoActiveTurn", err)
	}
	currentTurn := pool.RuntimeStatus("session-1", "", "").TurnID
	status, err := pool.AbortTurn("bot-1", "session-1", currentTurn)
	if err != nil {
		t.Fatalf("AbortTurn() error = %v", err)
	}
	if status.TurnState != turnStateCancelling {
		t.Fatalf("AbortTurn status turn state = %q, want cancelling", status.TurnState)
	}

	var got promptOutcome
	select {
	case got = <-outcome:
	case <-time.After(10 * time.Second):
		t.Fatal("aborted headless turn never returned")
	}
	if !errors.Is(got.err, ErrPromptAborted) {
		t.Fatalf("headless turn error = %v, want ErrPromptAborted", got.err)
	}
	if got.result.StopReason != "cancelled" {
		t.Fatalf("headless turn stop reason = %q, want cancelled", got.result.StopReason)
	}
	// The runtime survives the out-of-band abort.
	if after := pool.sessionHandle("session-1"); after != first {
		t.Fatal("out-of-band abort destroyed or replaced the runtime")
	}
	if _, err := pool.AbortTurn("bot-1", "session-1", ""); !errors.Is(err, ErrNoActiveTurn) {
		t.Fatalf("AbortTurn with no turn error = %v, want ErrNoActiveTurn", err)
	}
}

func TestSessionPoolCloseSessionDoesNotResurrectRuntime(t *testing.T) {
	pool := newFakeScriptPool(t)
	pool.timeout = time.Hour

	input := PromptInput{
		BotID:           "bot-1",
		SessionID:       "session-1",
		StreamID:        "stream-1",
		AgentID:         acpprofile.AgentCodexID,
		ProjectPath:     "/data/project",
		Prompt:          "[hang] doomed turn",
		CurrentPlatform: "web",
	}
	promptErr := make(chan error, 1)
	go func() {
		_, err := pool.Prompt(context.Background(), input)
		promptErr <- err
	}()
	waitForSessionActive(t, pool, "session-1")

	if err := pool.CloseSession("session-1"); err != nil {
		t.Fatalf("CloseSession() error = %v", err)
	}
	select {
	case err := <-promptErr:
		if err == nil {
			t.Fatal("prompt racing CloseSession returned nil error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("prompt racing CloseSession never unwound")
	}
	// The retired runtime must not be transparently recreated by the
	// unwinding prompt: the session was deleted.
	if h := pool.sessionHandle("session-1"); h != nil {
		t.Fatalf("CloseSession race resurrected a runtime: %s", h.id)
	}
	pool.mu.RLock()
	leftover := len(pool.runtimes)
	pool.mu.RUnlock()
	if leftover != 0 {
		t.Fatalf("%d runtimes remain after CloseSession race, want 0", leftover)
	}
}

func TestSessionPoolPromptInjectsHistoryOncePerRuntime(t *testing.T) {
	pool := newFakeScriptPool(t)
	pool.timeout = time.Hour

	var historyCalls atomic.Int32
	var historyErr atomic.Bool
	input := PromptInput{
		BotID:           "bot-1",
		SessionID:       "session-1",
		StreamID:        "stream-1",
		AgentID:         acpprofile.AgentCodexID,
		ProjectPath:     "/data/project",
		Prompt:          "first",
		CurrentPlatform: "web",
		HistoryResources: func() ([]acpclient.PromptResource, error) {
			historyCalls.Add(1)
			if historyErr.Load() {
				return nil, errors.New("history store unavailable")
			}
			return []acpclient.PromptResource{{
				URI:      "memoh://context/session-history",
				MimeType: "text/markdown",
				Text:     "### User\n\nearlier turn",
			}}, nil
		},
	}

	// A render failure must not burn the once-per-runtime flag: the next
	// prompt retries the replay.
	historyErr.Store(true)
	result, err := pool.Prompt(context.Background(), input)
	if err != nil {
		t.Fatalf("Prompt(render failure) error = %v", err)
	}
	if strings.Contains(result.Text, "history-replayed") {
		t.Fatalf("failed render still injected history: %q", result.Text)
	}
	if got := historyCalls.Load(); got != 1 {
		t.Fatalf("history closure calls after failed render = %d, want 1", got)
	}

	historyErr.Store(false)
	result, err = pool.Prompt(context.Background(), input)
	if err != nil {
		t.Fatalf("Prompt(first) error = %v", err)
	}
	if !strings.Contains(result.Text, "history-replayed") {
		t.Fatalf("first successful prompt did not carry replayed history: %q", result.Text)
	}
	if got := historyCalls.Load(); got != 2 {
		t.Fatalf("history closure calls after retry = %d, want 2", got)
	}

	input.Prompt = "second"
	result, err = pool.Prompt(context.Background(), input)
	if err != nil {
		t.Fatalf("Prompt(second) error = %v", err)
	}
	if strings.Contains(result.Text, "history-replayed") {
		t.Fatalf("warm runtime replayed history again: %q", result.Text)
	}
	if got := historyCalls.Load(); got != 2 {
		t.Fatalf("history closure calls after second prompt = %d, want 2", got)
	}

	// A recycled runtime must replay the history it never saw.
	if err := pool.CloseSession("session-1"); err != nil {
		t.Fatalf("CloseSession() error = %v", err)
	}
	input.Prompt = "third after recycle"
	result, err = pool.Prompt(context.Background(), input)
	if err != nil {
		t.Fatalf("Prompt(third) error = %v", err)
	}
	if !strings.Contains(result.Text, "history-replayed") {
		t.Fatalf("recycled runtime did not replay history: %q", result.Text)
	}
	if got := historyCalls.Load(); got != 3 {
		t.Fatalf("history closure calls after recycle = %d, want 3", got)
	}
}

// TestSessionPoolTryCloseIdleSparesBoundRuntime guards the budget-eviction
// re-check: a victim selected as unbound but bound before the close must be
// spared.
func TestSessionPoolTryCloseIdleSparesBoundRuntime(t *testing.T) {
	pool := newSessionPool(nil, &blockingRunner{}, fakeBotGetter{bot: enabledACPBot("bot-1", "api_key", map[string]any{"api_key": "sk"})})
	h := &runtimeHandle{
		id:           newRuntimeID(),
		botID:        "bot-1",
		status:       stateIdle,
		lastActive:   time.Now().Add(-time.Hour),
		boundSession: "session-1",
	}
	injectRuntime(pool, h)

	if pool.tryCloseIdle(h, 0, true) {
		t.Fatal("eviction closed a runtime that is bound to a session")
	}
	h.state.Lock()
	closed := h.closed
	h.state.Unlock()
	if closed {
		t.Fatal("bound runtime was marked closed by spared eviction")
	}
	// The reaper path (requireUnbound=false) may still close it once stale.
	if !pool.tryCloseIdle(h, 0, false) {
		t.Fatal("reaper failed to close a stale idle runtime")
	}
}

func TestSessionPoolEnsureStartsRuntimeAndReportsModels(t *testing.T) {
	t.Setenv("MEMOH_ACP_SESSION_POOL_FAKE_AGENT_MODELS", "1")
	pool := newFakeScriptPool(t)

	status, err := pool.Ensure(context.Background(), PromptInput{
		BotID:       "bot-1",
		SessionID:   "session-1",
		AgentID:     acpprofile.AgentCodexID,
		ProjectPath: "/data/project",
	})
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	if status.State != "idle" || status.ACPSession == "" {
		t.Fatalf("Ensure() status = %#v, want idle runtime with ACP session id", status)
	}
	if !strings.HasPrefix(status.RuntimeID, runtimeIDPrefix) || status.SessionID != "session-1" {
		t.Fatalf("Ensure() identity = %#v, want bound server-generated runtime", status)
	}
	if status.Models == nil || !status.Models.Supported || status.Models.CurrentModelID != "gpt-5.1-codex" {
		t.Fatalf("Ensure() models = %#v, want protocol model state", status.Models)
	}
	if len(status.Models.Available) != 2 || status.Models.Available[0].ID != "gpt-5.1-codex" || status.Models.Available[1].ID != "gpt-5.1-codex-high" {
		t.Fatalf("Ensure() available models = %#v", status.Models.Available)
	}
	if status.DefaultModelID != "gpt-5.1-codex" {
		t.Fatalf("Ensure() default model = %q, want startup model", status.DefaultModelID)
	}
}

func TestSessionPoolStartRuntimeReconcilesManagedCodexAPIKeyConfig(t *testing.T) {
	pool, root := newFakeScriptPoolForBot(t, enabledACPBot("bot-1", "api_key", map[string]any{
		"api_key":  "sk-local-byok",
		"base_url": "https://proxy.example.com/v1",
	}))

	if _, err := pool.Ensure(context.Background(), PromptInput{
		BotID:       "bot-1",
		SessionID:   "session-1",
		AgentID:     acpprofile.AgentCodexID,
		ProjectPath: "/data/project",
	}); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}

	config := readSessionPoolFile(t, root, ".codex", "config.toml")
	for _, want := range []string{
		`model_provider = "OpenAI"`,
		`model_reasoning_summary = "detailed"`,
		`hide_agent_reasoning = false`,
		`show_raw_agent_reasoning = false`,
		`base_url = "https://proxy.example.com/v1"`,
	} {
		if !strings.Contains(config, want) {
			t.Fatalf("Codex config missing %q:\n%s", want, config)
		}
	}
	auth := readSessionPoolFile(t, root, ".codex", "auth.json")
	if !strings.Contains(auth, `"OPENAI_API_KEY": "sk-local-byok"`) {
		t.Fatalf("Codex auth missing managed key:\n%s", auth)
	}
}

func TestSessionPoolStartRuntimeReconcilesCodexOAuthConfigWithoutOverwritingAuth(t *testing.T) {
	pool, root := newFakeScriptPoolForBot(t, enabledACPBot("bot-1", "oauth", nil))
	authPath := filepath.Join(root, ".codex", "auth.json")
	if err := os.MkdirAll(filepath.Dir(authPath), 0o750); err != nil {
		t.Fatal(err)
	}
	const existingAuth = `{"auth_mode":"chatgpt","tokens":{"access_token":"existing"}}`
	if err := os.WriteFile(authPath, []byte(existingAuth), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Ensure(context.Background(), PromptInput{
		BotID:       "bot-1",
		SessionID:   "session-1",
		AgentID:     acpprofile.AgentCodexID,
		ProjectPath: "/data/project",
	}); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}

	config := readSessionPoolFile(t, root, ".codex", "config.toml")
	for _, want := range []string{
		`model_provider = "chatgpt-http"`,
		`model_reasoning_summary = "detailed"`,
		`hide_agent_reasoning = false`,
		`show_raw_agent_reasoning = false`,
		`requires_openai_auth = true`,
	} {
		if !strings.Contains(config, want) {
			t.Fatalf("Codex OAuth config missing %q:\n%s", want, config)
		}
	}
	if got := readSessionPoolFile(t, root, ".codex", "auth.json"); got != existingAuth {
		t.Fatalf("OAuth auth.json was overwritten:\n%s", got)
	}
}

func TestSessionPoolCreateRuntimeGeneratesIDAndReportsModels(t *testing.T) {
	t.Setenv("MEMOH_ACP_SESSION_POOL_FAKE_AGENT_MODELS", "1")
	pool := newFakeScriptPool(t)

	status, err := pool.CreateRuntime(context.Background(), CreateRuntimeInput{
		BotID:       "bot-1",
		AgentID:     acpprofile.AgentCodexID,
		ProjectPath: "/data/project",
	})
	if err != nil {
		t.Fatalf("CreateRuntime() error = %v", err)
	}
	if !strings.HasPrefix(status.RuntimeID, runtimeIDPrefix) {
		t.Fatalf("runtime id = %q, want server-generated %q prefix", status.RuntimeID, runtimeIDPrefix)
	}
	if status.SessionID != "" {
		t.Fatalf("fresh runtime should be unbound, got session %q", status.SessionID)
	}
	if status.State != "idle" || status.Models == nil || status.Models.CurrentModelID != "gpt-5.1-codex" {
		t.Fatalf("CreateRuntime() status = %#v", status)
	}
	if status.DefaultModelID != "gpt-5.1-codex" {
		t.Fatalf("default model = %q", status.DefaultModelID)
	}

	got, err := pool.RuntimeStatusByID("bot-1", status.RuntimeID)
	if err != nil {
		t.Fatalf("RuntimeStatusByID() error = %v", err)
	}
	if got.RuntimeID != status.RuntimeID || got.ACPSession == "" {
		t.Fatalf("RuntimeStatusByID() = %#v", got)
	}
}

func TestSessionPoolBindRuntimeAttachesWarmProcessToSession(t *testing.T) {
	t.Setenv("MEMOH_ACP_SESSION_POOL_FAKE_AGENT_MODELS", "1")
	pool := newFakeScriptPool(t)

	created, err := pool.CreateRuntime(context.Background(), CreateRuntimeInput{
		BotID:       "bot-1",
		AgentID:     acpprofile.AgentCodexID,
		ProjectPath: "/data/project",
	})
	if err != nil {
		t.Fatalf("CreateRuntime() error = %v", err)
	}
	if _, err := pool.SetRuntimeModel(context.Background(), "bot-1", created.RuntimeID, "gpt-5.1-codex-high"); err != nil {
		t.Fatalf("SetRuntimeModel() error = %v", err)
	}

	if err := pool.BindRuntime("bot-1", created.RuntimeID, "session-1", acpprofile.AgentCodexID, "/data/project"); err != nil {
		t.Fatalf("BindRuntime() error = %v", err)
	}
	h := pool.sessionHandle("session-1")
	if h == nil || h.id != created.RuntimeID {
		t.Fatalf("session index does not point at the bound runtime")
	}

	// The bound session reuses the warm process — including its model.
	status, err := pool.Ensure(context.Background(), PromptInput{
		BotID:       "bot-1",
		SessionID:   "session-1",
		AgentID:     acpprofile.AgentCodexID,
		ProjectPath: "/data/project",
	})
	if err != nil {
		t.Fatalf("Ensure(bound) error = %v", err)
	}
	if status.RuntimeID != created.RuntimeID {
		t.Fatalf("Ensure started a new runtime %q, want bound %q", status.RuntimeID, created.RuntimeID)
	}
	if status.Models == nil || status.Models.CurrentModelID != "gpt-5.1-codex-high" {
		t.Fatalf("bound runtime lost its model: %#v", status.Models)
	}
	if status.DefaultModelID != "gpt-5.1-codex" {
		t.Fatalf("default model = %q, want startup default", status.DefaultModelID)
	}

	// A bound runtime cannot be bound again.
	if err := pool.BindRuntime("bot-1", created.RuntimeID, "session-2", acpprofile.AgentCodexID, "/data/project"); !errors.Is(err, ErrRuntimeBindRejected) {
		t.Fatalf("second BindRuntime() error = %v, want ErrRuntimeBindRejected", err)
	}
}

func TestSessionPoolSetRuntimeModelEmptyResetsToDefault(t *testing.T) {
	t.Setenv("MEMOH_ACP_SESSION_POOL_FAKE_AGENT_MODELS", "1")
	pool := newFakeScriptPool(t)

	created, err := pool.CreateRuntime(context.Background(), CreateRuntimeInput{
		BotID:       "bot-1",
		AgentID:     acpprofile.AgentCodexID,
		ProjectPath: "/data/project",
	})
	if err != nil {
		t.Fatalf("CreateRuntime() error = %v", err)
	}
	status, err := pool.SetRuntimeModel(context.Background(), "bot-1", created.RuntimeID, "gpt-5.1-codex-high")
	if err != nil {
		t.Fatalf("SetRuntimeModel(high) error = %v", err)
	}
	if status.Models == nil || status.Models.CurrentModelID != "gpt-5.1-codex-high" {
		t.Fatalf("model after set = %#v", status.Models)
	}

	status, err = pool.SetRuntimeModel(context.Background(), "bot-1", created.RuntimeID, "")
	if err != nil {
		t.Fatalf("SetRuntimeModel(reset) error = %v", err)
	}
	if status.Models == nil || status.Models.CurrentModelID != "gpt-5.1-codex" {
		t.Fatalf("model after reset = %#v, want startup default", status.Models)
	}
}

func TestSessionPoolBindRuntimeRejectsMismatches(t *testing.T) {
	pool := newSessionPool(nil, nil, nil)
	live := &acpclient.Session{}
	pending := &runtimeHandle{
		id:          newRuntimeID(),
		botID:       "bot-2",
		agentID:     acpprofile.AgentCodexID,
		projectPath: "/data",
		session:     live,
		status:      stateIdle,
		lastActive:  time.Now(),
	}
	injectRuntime(pool, pending)

	cases := []struct {
		name                          string
		botID, sessionID, agent, path string
		wantErr                       error
	}{
		{"cross bot", "bot-1", "real", acpprofile.AgentCodexID, "/data", ErrRuntimeNotFound},
		{"wrong agent", "bot-2", "real", acpprofile.AgentClaudeCodeID, "/data", ErrRuntimeBindRejected},
		{"wrong project", "bot-2", "real", acpprofile.AgentCodexID, "/other", ErrRuntimeBindRejected},
	}
	for _, tc := range cases {
		if err := pool.BindRuntime(tc.botID, pending.id, tc.sessionID, tc.agent, tc.path); !errors.Is(err, tc.wantErr) {
			t.Fatalf("%s: BindRuntime() error = %v, want %v", tc.name, err, tc.wantErr)
		}
	}
	if err := pool.BindRuntime("bot-2", "rt_missing", "real", acpprofile.AgentCodexID, "/data"); !errors.Is(err, ErrRuntimeNotFound) {
		t.Fatalf("missing runtime: BindRuntime() error = %v, want ErrRuntimeNotFound", err)
	}

	// Session already served by another runtime.
	other := &runtimeHandle{id: newRuntimeID(), botID: "bot-2", boundSession: "real", status: stateIdle}
	injectRuntime(pool, other)
	if err := pool.BindRuntime("bot-2", pending.id, "real", acpprofile.AgentCodexID, "/data"); !errors.Is(err, ErrRuntimeBindRejected) {
		t.Fatalf("occupied session: BindRuntime() error = %v, want ErrRuntimeBindRejected", err)
	}

	// A still-starting runtime (no live process yet) is not bindable.
	starting := &runtimeHandle{id: newRuntimeID(), botID: "bot-2", agentID: acpprofile.AgentCodexID, projectPath: "/data", status: stateStarting}
	injectRuntime(pool, starting)
	if err := pool.BindRuntime("bot-2", starting.id, "real-2", acpprofile.AgentCodexID, "/data"); !errors.Is(err, ErrRuntimeBindRejected) {
		t.Fatalf("starting runtime: BindRuntime() error = %v, want ErrRuntimeBindRejected", err)
	}

	// Everything matching succeeds.
	if err := pool.BindRuntime("bot-2", pending.id, "real-2", acpprofile.AgentCodexID, "/data"); err != nil {
		t.Fatalf("matching BindRuntime() error = %v", err)
	}
	if pool.sessionHandle("real-2") != pending {
		t.Fatalf("bound session does not resolve to the runtime")
	}
}

func TestSessionPoolOwnedGateHasZeroSideEffectsAcrossBots(t *testing.T) {
	pool := newSessionPool(nil, nil, nil)
	foreign := &runtimeHandle{
		id:           newRuntimeID(),
		botID:        "bot-2",
		agentID:      acpprofile.AgentCodexID,
		projectPath:  "/data",
		session:      &acpclient.Session{},
		status:       stateIdle,
		lastActive:   time.Now(),
		boundSession: "their-session",
	}
	injectRuntime(pool, foreign)

	if _, err := pool.RuntimeStatusByID("bot-1", foreign.id); !errors.Is(err, ErrRuntimeNotFound) {
		t.Fatalf("RuntimeStatusByID(cross bot) error = %v, want ErrRuntimeNotFound", err)
	}
	if _, err := pool.SetRuntimeModel(context.Background(), "bot-1", foreign.id, "gpt-5.1-codex"); !errors.Is(err, ErrRuntimeNotFound) {
		t.Fatalf("SetRuntimeModel(cross bot) error = %v, want ErrRuntimeNotFound", err)
	}
	if err := pool.CloseRuntime("bot-1", foreign.id); !errors.Is(err, ErrRuntimeNotFound) {
		t.Fatalf("CloseRuntime(cross bot) error = %v, want ErrRuntimeNotFound", err)
	}
	if err := pool.BindRuntime("bot-1", foreign.id, "my-session", acpprofile.AgentCodexID, "/data"); !errors.Is(err, ErrRuntimeNotFound) {
		t.Fatalf("BindRuntime(cross bot) error = %v, want ErrRuntimeNotFound", err)
	}
	if _, ok := pool.ResolveRuntimeToolContext("bot-1", foreign.id); ok {
		t.Fatalf("ResolveRuntimeToolContext(cross bot) resolved")
	}

	// Zero side effects: the foreign runtime is fully intact.
	pool.mu.RLock()
	registered := pool.runtimes[foreign.id] == foreign
	indexed := pool.bySession["their-session"] == foreign.id
	pool.mu.RUnlock()
	foreign.state.Lock()
	untouched := !foreign.closed && foreign.session != nil && foreign.status == stateIdle
	foreign.state.Unlock()
	if !registered || !indexed || !untouched {
		t.Fatalf("cross-bot operations disturbed the runtime: registered=%v indexed=%v untouched=%v", registered, indexed, untouched)
	}

	// The owner can close it.
	if err := pool.CloseRuntime("bot-2", foreign.id); err != nil {
		t.Fatalf("CloseRuntime(owner) error = %v", err)
	}
	if pool.sessionHandle("their-session") != nil {
		t.Fatalf("owner close left the session index entry")
	}
}

func TestSessionPoolUnboundCapEvictsOldestIdle(t *testing.T) {
	pool := newFakeScriptPool(t)

	now := time.Now()
	for i := 0; i < maxUnboundRuntimesPerBot; i++ {
		injectRuntime(pool, &runtimeHandle{
			id:         fmt.Sprintf("rt_old-%d", i),
			botID:      "bot-1",
			agentID:    acpprofile.AgentCodexID,
			status:     stateIdle,
			lastActive: now.Add(-time.Duration(i+1) * time.Minute),
		})
	}
	// Bound and other-bot runtimes must not count toward the cap.
	injectRuntime(pool, &runtimeHandle{id: "rt_bound", botID: "bot-1", boundSession: "session-9", status: stateIdle, lastActive: now})
	injectRuntime(pool, &runtimeHandle{id: "rt_other-bot", botID: "bot-9", status: stateIdle, lastActive: now.Add(-time.Minute)})

	created, err := pool.CreateRuntime(context.Background(), CreateRuntimeInput{
		BotID:       "bot-1",
		AgentID:     acpprofile.AgentCodexID,
		ProjectPath: "/data/project",
	})
	if err != nil {
		t.Fatalf("CreateRuntime() error = %v", err)
	}

	pool.mu.RLock()
	_, oldestAlive := pool.runtimes[fmt.Sprintf("rt_old-%d", maxUnboundRuntimesPerBot-1)]
	_, newAlive := pool.runtimes[created.RuntimeID]
	_, boundAlive := pool.runtimes["rt_bound"]
	_, otherAlive := pool.runtimes["rt_other-bot"]
	survivors := 0
	for i := 0; i < maxUnboundRuntimesPerBot-1; i++ {
		if _, ok := pool.runtimes[fmt.Sprintf("rt_old-%d", i)]; ok {
			survivors++
		}
	}
	pool.mu.RUnlock()
	if oldestAlive {
		t.Fatalf("oldest idle unbound runtime should be evicted")
	}
	if !newAlive || !boundAlive || !otherAlive || survivors != maxUnboundRuntimesPerBot-1 {
		t.Fatalf("eviction touched the wrong runtimes: new=%v bound=%v other=%v survivors=%d", newAlive, boundAlive, otherAlive, survivors)
	}
}

func TestSessionPoolUnboundCapErrorsWhenAllBusy(t *testing.T) {
	pool := newSessionPool(nil, &recordingRunner{
		info: bridge.WorkspaceInfo{Backend: bridge.WorkspaceBackendContainer, DefaultWorkDir: "/data"},
	}, fakeBotGetter{bot: enabledACPBot("bot-1", "api_key", map[string]any{"api_key": "sk-test"})})
	for i := 0; i < maxUnboundRuntimesPerBot; i++ {
		injectRuntime(pool, &runtimeHandle{
			id:         fmt.Sprintf("rt_busy-%d", i),
			botID:      "bot-1",
			status:     stateActive,
			lastActive: time.Now(),
		})
	}

	_, err := pool.CreateRuntime(context.Background(), CreateRuntimeInput{
		BotID:       "bot-1",
		AgentID:     acpprofile.AgentCodexID,
		ProjectPath: "/data/project",
	})
	if !errors.Is(err, ErrTooManyRuntimes) {
		t.Fatalf("CreateRuntime() error = %v, want ErrTooManyRuntimes", err)
	}
	pool.mu.RLock()
	count := len(pool.runtimes)
	pool.mu.RUnlock()
	if count != maxUnboundRuntimesPerBot {
		t.Fatalf("capped create registered a runtime: %d handles", count)
	}
}

func TestSessionPoolEnsureReplacesMismatchedAgentRuntimeWithoutDeadlock(t *testing.T) {
	pool := newFakeScriptPool(t)

	// A stale bound runtime whose agent differs forces the replace path,
	// which formerly deadlocked on the per-session lock.
	injectRuntime(pool, &runtimeHandle{
		id:           newRuntimeID(),
		botID:        "bot-1",
		agentID:      acpprofile.AgentClaudeCodeID,
		projectPath:  "/data/project",
		status:       stateIdle,
		lastActive:   time.Now(),
		boundSession: "session-x",
		session:      &acpclient.Session{},
	})

	done := make(chan error, 1)
	go func() {
		_, err := pool.Ensure(context.Background(), PromptInput{
			BotID:       "bot-1",
			SessionID:   "session-x",
			AgentID:     acpprofile.AgentCodexID,
			ProjectPath: "/data/project",
		})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Ensure() error = %v", err)
		}
	case <-time.After(time.Minute):
		t.Fatal("Ensure() deadlocked while replacing a mismatched runtime")
	}
	replaced := pool.sessionHandle("session-x")
	if replaced == nil || replaced.session == nil || replaced.agentID != acpprofile.AgentCodexID {
		t.Fatalf("replaced runtime = %#v, want fresh codex runtime", replaced)
	}
}

func TestSessionPoolSetModelUpdatesRuntimeModel(t *testing.T) {
	t.Setenv("MEMOH_ACP_SESSION_POOL_FAKE_AGENT_MODELS", "1")
	pool := newFakeScriptPool(t)

	status, err := pool.SetModel(context.Background(), PromptInput{
		BotID:       "bot-1",
		SessionID:   "session-1",
		AgentID:     acpprofile.AgentCodexID,
		ProjectPath: "/data/project",
	}, "gpt-5.1-codex-high")
	if err != nil {
		t.Fatalf("SetModel() error = %v", err)
	}
	if status.State != "idle" || status.ACPSession == "" {
		t.Fatalf("SetModel() status = %#v, want idle runtime with ACP session id", status)
	}
	if status.Models == nil || !status.Models.Supported || status.Models.CurrentModelID != "gpt-5.1-codex-high" {
		t.Fatalf("SetModel() models = %#v, want selected model", status.Models)
	}
}

func TestSessionPoolRuntimeStatusReportsActiveDuringColdStart(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	runner := &blockingRunner{
		info:    bridge.WorkspaceInfo{Backend: bridge.WorkspaceBackendContainer, DefaultWorkDir: "/data"},
		started: started,
		release: release,
	}
	pool := newSessionPool(
		nil,
		runner,
		fakeBotGetter{bot: enabledACPBot("bot-1", "api_key", map[string]any{"api_key": "sk-test"})},
	)

	errCh := make(chan error, 1)
	go func() {
		_, err := pool.Prompt(context.Background(), PromptInput{
			BotID:       "bot-1",
			SessionID:   "session-1",
			AgentID:     "codex",
			ProjectPath: "/data/project",
			Prompt:      "run",
		})
		errCh <- err
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not start")
	}

	status := pool.RuntimeStatus("session-1", "", "")
	if status.State != "active" || status.ACPSession != "" {
		t.Fatalf("RuntimeStatus during cold start = %#v, want active without ACP session id", status)
	}

	close(release)
	if err := <-errCh; err == nil || err.Error() != "released" {
		t.Fatalf("Prompt() error = %v, want released", err)
	}
	status = pool.RuntimeStatus("session-1", "codex", "/data/project")
	if status.State != "idle" || status.ACPSession != "" {
		t.Fatalf("RuntimeStatus after failed start = %#v, want idle without process", status)
	}
}

func TestSessionPoolCloseDuringColdStartPreventsReinsert(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	runner := &delayedStartRunner{
		info:    bridge.WorkspaceInfo{Backend: bridge.WorkspaceBackendContainer, DefaultWorkDir: "/data"},
		started: started,
		release: release,
		session: &acpclient.Session{},
	}
	pool := newSessionPool(
		nil,
		runner,
		fakeBotGetter{bot: enabledACPBot("bot-1", "api_key", map[string]any{"api_key": "sk-test"})},
	)

	type startResult struct {
		handle *runtimeHandle
		err    error
	}
	resultCh := make(chan startResult, 1)
	go func() {
		h, _, err := pool.runtimeForSession(context.Background(), PromptInput{
			BotID:       "bot-1",
			SessionID:   "session-1",
			AgentID:     "codex",
			ProjectPath: "/data/project",
		}, false)
		resultCh <- startResult{handle: h, err: err}
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not start")
	}

	starting := pool.sessionHandle("session-1")
	if starting == nil {
		t.Fatal("starting handle was not registered in the session index")
	}
	closed := make(chan error, 1)
	go func() {
		closed <- pool.CloseSession("session-1")
	}()
	// Wait until CloseSession has aborted the start before releasing the
	// runner, mirroring a close that lands mid-startup.
	deadline := time.Now().Add(2 * time.Second)
	for {
		starting.state.Lock()
		aborted := starting.closed
		starting.state.Unlock()
		if aborted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("CloseSession did not abort the in-flight start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(release)

	var result startResult
	select {
	case result = <-resultCh:
	case <-time.After(2 * time.Second):
		t.Fatal("runtimeForSession did not return")
	}
	if result.handle != nil {
		t.Fatalf("runtimeForSession returned a handle after CloseSession during startup")
	}
	if result.err == nil || !strings.Contains(result.err.Error(), "closed during startup") {
		t.Fatalf("runtimeForSession error = %v, want closed during startup", result.err)
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("CloseSession() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CloseSession did not return")
	}
	if pool.sessionHandle("session-1") != nil {
		t.Fatalf("closed cold-start runtime was reinserted into the pool")
	}
}

func TestSessionPoolCloseDuringColdStartCancelsStartup(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	runner := &cancelAwareStartRunner{
		info:      bridge.WorkspaceInfo{Backend: bridge.WorkspaceBackendContainer, DefaultWorkDir: "/data"},
		started:   started,
		cancelled: cancelled,
	}
	pool := newSessionPool(
		nil,
		runner,
		fakeBotGetter{bot: enabledACPBot("bot-1", "api_key", map[string]any{"api_key": "sk-test"})},
	)

	type startResult struct {
		handle *runtimeHandle
		err    error
	}
	resultCh := make(chan startResult, 1)
	go func() {
		h, _, err := pool.runtimeForSession(context.Background(), PromptInput{
			BotID:       "bot-1",
			SessionID:   "session-1",
			AgentID:     "codex",
			ProjectPath: "/data/project",
		}, false)
		resultCh <- startResult{handle: h, err: err}
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not start")
	}

	closed := make(chan error, 1)
	go func() {
		closed <- pool.CloseSession("session-1")
	}()
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("startup context was not cancelled")
	}

	var result startResult
	select {
	case result = <-resultCh:
	case <-time.After(2 * time.Second):
		t.Fatal("runtimeForSession did not return after startup cancellation")
	}
	if result.handle != nil {
		t.Fatalf("runtimeForSession returned a handle after startup cancellation")
	}
	if !errors.Is(result.err, context.Canceled) {
		t.Fatalf("runtimeForSession error = %v, want context.Canceled", result.err)
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("CloseSession() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CloseSession did not return")
	}
	if pool.sessionHandle("session-1") != nil {
		t.Fatalf("cancelled cold-start runtime remained in the pool")
	}
}

func TestSessionPoolCloseSessionWaitsForInFlightOperation(t *testing.T) {
	pool := newSessionPool(nil, nil, nil)
	h := &runtimeHandle{
		id:           newRuntimeID(),
		botID:        "bot-1",
		boundSession: "session-1",
		status:       stateActive,
		lastActive:   time.Now(),
	}
	injectRuntime(pool, h)
	h.op.Lock()

	closed := make(chan error, 1)
	go func() {
		closed <- pool.CloseSession("session-1")
	}()

	select {
	case err := <-closed:
		t.Fatalf("CloseSession returned before the in-flight operation released: %v", err)
	case <-time.After(25 * time.Millisecond):
	}

	h.op.Unlock()

	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("CloseSession() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CloseSession did not unblock after the operation released")
	}
}

func TestSessionPoolSerializesColdStartForSameSession(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.MkdirAll(project, 0o750); err != nil {
		t.Fatal(err)
	}

	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o750); err != nil {
		t.Fatal(err)
	}
	writeSessionPoolFakeAgentScript(t, binDir, "npx")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	startLog := filepath.Join(root, "starts.log")
	t.Setenv("MEMOH_ACP_START_LOG", startLog)

	runner := acpclient.NewRunner(nil, sessionPoolWorkspace{
		client: newSessionPoolBridgeClient(t, root),
		info: bridge.WorkspaceInfo{
			Backend:        bridge.WorkspaceBackendLocal,
			DefaultWorkDir: root,
		},
	})
	pool := newSessionPool(nil, runner, fakeBotGetter{bot: enabledACPBot("bot-1", "api_key", map[string]any{"api_key": "sk-local-byok"})})
	t.Cleanup(pool.CloseAll)

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := pool.Prompt(context.Background(), PromptInput{
				BotID:       "bot-1",
				SessionID:   "session-1",
				AgentID:     "codex",
				ProjectPath: "/data/project",
				Prompt:      "same session",
			})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	// Concurrent prompts must never double-start the agent process. Under the
	// busy-turn semantics (architecture doc Q1) the overlapping prompt may be
	// rejected with ErrSessionBusy instead of being queued; any other error is
	// a failure.
	successes := 0
	for err := range errs {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrSessionBusy):
		default:
			t.Fatalf("Prompt() error = %v", err)
		}
	}
	if successes < 1 {
		t.Fatal("no concurrent prompt succeeded")
	}

	raw, err := os.ReadFile(startLog) //nolint:gosec // test path under t.TempDir.
	if err != nil {
		t.Fatalf("read start log: %v", err)
	}
	if starts := strings.Count(string(raw), "start\n"); starts != 1 {
		t.Fatalf("fake ACP process starts = %d, want 1; log=%q", starts, string(raw))
	}
}

func TestSessionPoolSetupModeResolution(t *testing.T) {
	missingAPIKey := newSessionPool(nil, &recordingRunner{
		info: bridge.WorkspaceInfo{Backend: bridge.WorkspaceBackendContainer, DefaultWorkDir: "/data"},
	}, fakeBotGetter{bot: enabledACPBot("bot-1", "api_key", nil)})
	_, err := missingAPIKey.Prompt(context.Background(), PromptInput{
		BotID:       "bot-1",
		SessionID:   "session-1",
		AgentID:     "codex",
		ProjectPath: "/data/project",
		Prompt:      "run",
	})
	if err == nil || !strings.Contains(err.Error(), "api_key required") {
		t.Fatalf("container api_key missing key error = %v", err)
	}

	apiKeyRunner := &recordingRunner{
		info:     bridge.WorkspaceInfo{Backend: bridge.WorkspaceBackendContainer, DefaultWorkDir: "/data", ACPToolsHTTPURL: "http://127.0.0.1:18732/mcp"},
		startErr: errors.New("started"),
	}
	apiKeyPool := newSessionPool(nil, apiKeyRunner, fakeBotGetter{bot: enabledACPBot("bot-1", "api_key", map[string]any{"api_key": "sk-test", "base_url": "https://proxy.example.com/v1"})})
	_, err = apiKeyPool.Prompt(context.Background(), PromptInput{
		BotID:       "bot-1",
		SessionID:   "session-1",
		AgentID:     "codex",
		ProjectPath: "/data/project",
		Prompt:      "run",
	})
	if err == nil || err.Error() != "started" {
		t.Fatalf("container api_key error = %v, want runner start error", err)
	}
	if apiKeyRunner.req.SetupMode != acpclient.SetupModeAPIKey {
		t.Fatalf("api_key setup mode = %q", apiKeyRunner.req.SetupMode)
	}
	if len(apiKeyRunner.req.Env) != 0 {
		t.Fatalf("api_key mode must use Codex files, not credential env: %v", apiKeyRunner.req.Env)
	}

	oauthRunner := &recordingRunner{
		info:     bridge.WorkspaceInfo{Backend: bridge.WorkspaceBackendContainer, DefaultWorkDir: "/data"},
		startErr: errors.New("started"),
	}
	oauthPool := newSessionPool(nil, oauthRunner, fakeBotGetter{bot: enabledACPBot("bot-1", "oauth", map[string]any{"provider_id": "provider-1"})})
	_, err = oauthPool.Prompt(context.Background(), PromptInput{
		BotID:       "bot-1",
		SessionID:   "session-1",
		AgentID:     "codex",
		ProjectPath: "/data/project",
		Prompt:      "run",
	})
	if err == nil || err.Error() != "started" {
		t.Fatalf("container oauth error = %v, want runner start error", err)
	}
	if oauthRunner.req.SetupMode != acpclient.SetupModeOAuth {
		t.Fatalf("oauth setup mode = %q", oauthRunner.req.SetupMode)
	}

	selfRunner := &recordingRunner{
		info:     bridge.WorkspaceInfo{Backend: bridge.WorkspaceBackendContainer, DefaultWorkDir: "/data"},
		startErr: errors.New("started"),
	}
	selfPool := newSessionPool(nil, selfRunner, fakeBotGetter{bot: enabledACPBot("bot-1", "self", nil)})
	_, err = selfPool.Prompt(context.Background(), PromptInput{
		BotID:       "bot-1",
		SessionID:   "session-1",
		AgentID:     "codex",
		ProjectPath: "/data/project",
		Prompt:      "run",
	})
	if err == nil || err.Error() != "started" {
		t.Fatalf("container self error = %v, want runner start error", err)
	}
	if selfRunner.req.SetupMode != acpclient.SetupModeSelf {
		t.Fatalf("self setup mode = %q", selfRunner.req.SetupMode)
	}
	if len(selfRunner.req.Env) != 0 {
		t.Fatalf("self mode injected credential env: %v", selfRunner.req.Env)
	}
	if got := selfPool.RuntimeStatus("session-1", "codex", "/data/project"); got.State != "idle" || got.ACPSession != "" {
		t.Fatalf("RuntimeStatus after failed start = %#v, want idle without process", got)
	}

	// Desktop BYOK: local api_key now validates managed fields just like
	// container. Codex carries no env (it is configured via CODEX_HOME files at
	// the process layer), so req.Env stays empty even with a key set.
	localRunner := &recordingRunner{
		info:     bridge.WorkspaceInfo{Backend: bridge.WorkspaceBackendLocal, DefaultWorkDir: t.TempDir()},
		startErr: errors.New("started"),
	}
	localPool := newSessionPool(nil, localRunner, fakeBotGetter{bot: enabledACPBot("bot-1", "api_key", map[string]any{"api_key": "sk-local-byok"})})
	_, err = localPool.Prompt(context.Background(), PromptInput{
		BotID:       "bot-1",
		SessionID:   "session-1",
		AgentID:     "codex",
		ProjectPath: "",
		Prompt:      "run",
	})
	if err == nil || err.Error() != "started" {
		t.Fatalf("local api_key error = %v, want runner start error", err)
	}
	if len(localRunner.req.Env) != 0 {
		t.Fatalf("local backend injected env: %v", localRunner.req.Env)
	}
	if localRunner.req.LocalCommand != "npx" || len(localRunner.req.LocalArgs) == 0 {
		t.Fatalf("local command not passed through: %#v", localRunner.req)
	}

	// Local api_key without a key must now be rejected (BYOK requires credentials).
	localMissingPool := newSessionPool(nil, &recordingRunner{
		info:     bridge.WorkspaceInfo{Backend: bridge.WorkspaceBackendLocal, DefaultWorkDir: t.TempDir()},
		startErr: errors.New("started"),
	}, fakeBotGetter{bot: enabledACPBot("bot-1", "api_key", nil)})
	_, err = localMissingPool.Prompt(context.Background(), PromptInput{
		BotID:       "bot-1",
		SessionID:   "session-1",
		AgentID:     "codex",
		ProjectPath: "",
		Prompt:      "run",
	})
	if err == nil || !strings.Contains(err.Error(), "api_key required") {
		t.Fatalf("local missing key error = %v, want api_key required validation", err)
	}

	claudeRunner := &recordingRunner{
		info:     bridge.WorkspaceInfo{Backend: bridge.WorkspaceBackendContainer, DefaultWorkDir: "/data"},
		startErr: errors.New("started"),
	}
	claudePool := newSessionPool(nil, claudeRunner, fakeBotGetter{bot: enabledACPAgentBot("bot-1", acpprofile.AgentClaudeCodeID, "api_key", map[string]any{
		"api_key":  "sk-ant-test",
		"base_url": "https://anthropic-proxy.example.com",
		"model":    "claude-haiku-4-5-20251001",
	})})
	_, err = claudePool.Prompt(context.Background(), PromptInput{
		BotID:       "bot-1",
		SessionID:   "session-1",
		AgentID:     acpprofile.AgentClaudeCodeID,
		ProjectPath: "/data/project",
		Prompt:      "run",
	})
	if err == nil || err.Error() != "started" {
		t.Fatalf("container Claude Code api_key error = %v, want runner start error", err)
	}
	if claudeRunner.req.Command != "claude-agent-acp" {
		t.Fatalf("Claude Code command = %q", claudeRunner.req.Command)
	}
	if !startRequestEnvHas(claudeRunner.req.Env, "ANTHROPIC_API_KEY", "sk-ant-test") ||
		!startRequestEnvHas(claudeRunner.req.Env, "ANTHROPIC_BASE_URL", "https://anthropic-proxy.example.com") {
		t.Fatalf("Claude Code env = %#v, want Anthropic managed env", claudeRunner.req.Env)
	}
	if !startRequestEnvHas(claudeRunner.req.Env, "ANTHROPIC_AUTH_TOKEN", "") ||
		!startRequestEnvHas(claudeRunner.req.Env, "CLAUDE_CODE_OAUTH_TOKEN", "") {
		t.Fatalf("Claude Code api_key env = %#v, want conflicting auth env cleared", claudeRunner.req.Env)
	}
	// Optional managed model override pins ANTHROPIC_MODEL so BYOK sessions can
	// target a model the endpoint actually serves (or a cheaper one).
	if !startRequestEnvHas(claudeRunner.req.Env, "ANTHROPIC_MODEL", "claude-haiku-4-5-20251001") {
		t.Fatalf("Claude Code env = %#v, want managed ANTHROPIC_MODEL pin", claudeRunner.req.Env)
	}

	claudeOAuthRunner := &recordingRunner{
		info:     bridge.WorkspaceInfo{Backend: bridge.WorkspaceBackendContainer, DefaultWorkDir: "/data"},
		startErr: errors.New("started"),
	}
	claudeOAuthManaged := map[string]any{ //nolint:gosec // Test fixture token, not a real credential.
		"oauth_token": "fake-claude-oauth-token",
		"base_url":    "https://anthropic-proxy.example.com",
	}
	claudeOAuthPool := newSessionPool(nil, claudeOAuthRunner, fakeBotGetter{bot: enabledACPAgentBot("bot-1", acpprofile.AgentClaudeCodeID, "oauth", claudeOAuthManaged)})
	_, err = claudeOAuthPool.Prompt(context.Background(), PromptInput{
		BotID:       "bot-1",
		SessionID:   "session-1",
		AgentID:     acpprofile.AgentClaudeCodeID,
		ProjectPath: "/data/project",
		Prompt:      "run",
	})
	if err == nil || err.Error() != "started" {
		t.Fatalf("container Claude Code oauth error = %v, want runner start error", err)
	}
	if !startRequestEnvHas(claudeOAuthRunner.req.Env, "CLAUDE_CODE_OAUTH_TOKEN", "fake-claude-oauth-token") ||
		!startRequestEnvHas(claudeOAuthRunner.req.Env, "ANTHROPIC_BASE_URL", "https://anthropic-proxy.example.com") {
		t.Fatalf("Claude Code oauth env = %#v, want Claude managed oauth env", claudeOAuthRunner.req.Env)
	}
	if !startRequestEnvHas(claudeOAuthRunner.req.Env, "ANTHROPIC_API_KEY", "") ||
		!startRequestEnvHas(claudeOAuthRunner.req.Env, "ANTHROPIC_AUTH_TOKEN", "") {
		t.Fatalf("Claude Code oauth env = %#v, want conflicting auth env cleared", claudeOAuthRunner.req.Env)
	}
	// No managed model configured → no ANTHROPIC_MODEL pin (the adapter keeps
	// its own default model).
	for _, item := range claudeOAuthRunner.req.Env {
		if strings.HasPrefix(item, "ANTHROPIC_MODEL=") {
			t.Fatalf("Claude Code oauth env = %#v, want no ANTHROPIC_MODEL pin without managed model", claudeOAuthRunner.req.Env)
		}
	}
}

func TestSessionPoolUsesSessionMetadataAsRuntimeTruth(t *testing.T) {
	runner := &recordingRunner{
		info:     bridge.WorkspaceInfo{Backend: bridge.WorkspaceBackendContainer, DefaultWorkDir: "/data", ACPToolsHTTPURL: "http://127.0.0.1:18732/mcp"},
		startErr: errors.New("started"),
	}
	pool := newSessionPool(
		nil,
		runner,
		fakeBotGetter{bot: enabledACPBot("bot-1", "api_key", map[string]any{"api_key": "sk-test"})},
		fakeSessionGetter{session: sessionpkg.Session{
			ID:    "session-1",
			BotID: "bot-1",
			Type:  sessionpkg.TypeACPAgent,
			Metadata: map[string]any{
				"acp_agent_id": "codex",
				"project_path": "/data/from-session",
			},
		}},
	)

	_, err := pool.Prompt(context.Background(), PromptInput{
		BotID:       "bot-1",
		SessionID:   "session-1",
		AgentID:     "wrong-agent",
		ProjectPath: "/data/from-caller",
		Prompt:      "run",
	})
	if err == nil || err.Error() != "started" {
		t.Fatalf("Prompt() error = %v, want runner start error", err)
	}
	if runner.req.AgentID != "codex" {
		t.Fatalf("runner agent_id = %q, want session metadata codex", runner.req.AgentID)
	}
	if runner.req.ProjectPath != "/data/from-session" {
		t.Fatalf("runner project_path = %q, want session metadata project path", runner.req.ProjectPath)
	}
}

func TestSessionPoolBakesOnlyStableRuntimeIdentity(t *testing.T) {
	runner := &recordingRunner{
		info:     bridge.WorkspaceInfo{Backend: bridge.WorkspaceBackendContainer, DefaultWorkDir: "/data", ACPToolsHTTPURL: "http://127.0.0.1:18732/mcp"},
		startErr: errors.New("started"),
	}
	pool := newSessionPool(
		nil,
		runner,
		fakeBotGetter{bot: enabledACPBot("bot-1", "api_key", map[string]any{"api_key": "sk-test"})},
	)
	pool.SetToolGateway(mcp.NewToolGatewayService(nil, nil))
	contexts := mcp.NewToolSessionContextStore()
	pool.SetToolSessionContextStore(contexts)

	_, err := pool.Prompt(context.Background(), PromptInput{
		BotID:             "bot-1",
		ChatID:            "chat-1",
		SessionID:         "session-1",
		StreamID:          "stream-1",
		RouteID:           "route-1",
		AgentID:           "codex",
		ProjectPath:       "/data/project",
		Prompt:            "run",
		ChannelIdentityID: "user-1",
		SessionToken:      "token-1",
		CurrentPlatform:   "web",
		ReplyTarget:       "reply-1",
		ConversationType:  "private",
	})
	if err == nil || err.Error() != "started" {
		t.Fatalf("Prompt() error = %v, want runner start error", err)
	}
	if runner.req.ToolHTTPURL != "http://127.0.0.1:18732/mcp" {
		t.Fatalf("ToolHTTPURL = %q", runner.req.ToolHTTPURL)
	}
	// Only stable runtime identity may be baked into the process config: the
	// per-prompt fields (stream, token, reply target...) change every turn
	// and are resolved live from the handle instead.
	baked := runner.req.ToolSession
	if baked.BotID != "bot-1" || !strings.HasPrefix(baked.RuntimeID, runtimeIDPrefix) || baked.SessionType != sessionpkg.TypeACPAgent {
		t.Fatalf("baked identity = %#v, want stable runtime identity", baked)
	}
	if baked.SessionID != "" || baked.StreamID != "" || baked.SessionToken != "" || baked.ReplyTarget != "" || baked.RouteID != "" || baked.ChannelIdentityID != "" {
		t.Fatalf("baked identity leaks per-prompt fields: %#v", baked)
	}
	// The pool no longer publishes ACP contexts into the shared store.
	merged := contexts.Merge(mcp.ToolSessionContext{BotID: "bot-1", SessionID: "session-1"})
	if merged.StreamID != "" || merged.ConversationType != "" {
		t.Fatalf("ACP context leaked into the shared store: %#v", merged)
	}
}

func TestSessionPoolUsesRequestToolURLForLocalWorkspace(t *testing.T) {
	pool := newSessionPool(nil, nil, nil)
	pool.SetToolGateway(mcp.NewToolGatewayService(nil, nil))

	got, err := pool.resolveToolHTTPURL("http://127.0.0.1:18731/bots/bot-1/tools", bridge.WorkspaceInfo{Backend: bridge.WorkspaceBackendLocal})
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://127.0.0.1:18731/bots/bot-1/tools" {
		t.Fatalf("local ToolHTTPURL = %q", got)
	}
}

func TestSessionPoolUsesWorkspaceACPToolsEndpointForContainer(t *testing.T) {
	pool := newSessionPool(nil, nil, nil)
	pool.SetToolGateway(mcp.NewToolGatewayService(nil, nil))

	got, err := pool.resolveToolHTTPURL("", bridge.WorkspaceInfo{
		Backend:         bridge.WorkspaceBackendContainer,
		ACPToolsHTTPURL: "http://127.0.0.1:18732/mcp",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://127.0.0.1:18732/mcp" {
		t.Fatalf("container ToolHTTPURL = %q", got)
	}
}

func TestRuntimeHandleToolContextOverlaysActivePrompt(t *testing.T) {
	h := &runtimeHandle{
		id:           "rt_test",
		botID:        "bot-1",
		boundSession: "session-1",
	}

	// Idle: stable identity plus the binding.
	ctx := h.toolContext()
	if ctx.BotID != "bot-1" || ctx.RuntimeID != "rt_test" || ctx.SessionID != "session-1" || ctx.SessionType != sessionpkg.TypeACPAgent {
		t.Fatalf("idle tool context = %#v", ctx)
	}
	if ctx.StreamID != "" || ctx.SessionToken != "" || ctx.IsSubagent {
		t.Fatalf("idle tool context leaks per-prompt fields: %#v", ctx)
	}
	if ctx.RuntimeActive {
		t.Fatalf("idle tool context must not allow tools/call: %#v", ctx)
	}

	// During a prompt the live per-prompt fields overlay.
	active := acpclient.ToolSessionContext{
		ChatID:           "chat-1",
		SessionID:        "session-1",
		StreamID:         "stream-7",
		SessionToken:     "token-7",
		CurrentPlatform:  "web",
		ReplyTarget:      "reply-7",
		ConversationType: "private",
	}
	h.state.Lock()
	h.active = &active
	h.state.Unlock()
	ctx = h.toolContext()
	if ctx.StreamID != "stream-7" || ctx.SessionToken != "token-7" || ctx.ChatID != "chat-1" || ctx.ReplyTarget != "reply-7" || !ctx.RuntimeActive {
		t.Fatalf("active tool context = %#v", ctx)
	}
	if ctx.RuntimeID != "rt_test" || ctx.IsSubagent {
		t.Fatalf("active tool context lost stable identity: %#v", ctx)
	}

	// clearActive removes every per-prompt field again.
	h.clearActive()
	ctx = h.toolContext()
	if ctx.StreamID != "" || ctx.SessionToken != "" || ctx.ChatID != "bot-1" || ctx.RuntimeActive {
		t.Fatalf("cleared tool context = %#v", ctx)
	}
}

func TestSessionPoolResolveRuntimeToolContext(t *testing.T) {
	pool := newSessionPool(nil, nil, nil)
	h := &runtimeHandle{
		id:           "rt_live",
		botID:        "bot-1",
		boundSession: "session-1",
		status:       stateIdle,
		session:      &acpclient.Session{},
	}
	injectRuntime(pool, h)

	ctx, ok := pool.ResolveRuntimeToolContext("bot-1", "rt_live")
	if !ok || ctx.RuntimeID != "rt_live" || ctx.SessionID != "session-1" {
		t.Fatalf("ResolveRuntimeToolContext() = %#v, %v", ctx, ok)
	}
	if _, ok := pool.ResolveRuntimeToolContext("bot-2", "rt_live"); ok {
		t.Fatalf("cross-bot runtime context resolved")
	}
	if _, ok := pool.ResolveRuntimeToolContext("bot-1", "rt_missing"); ok {
		t.Fatalf("missing runtime context resolved")
	}

	h.state.Lock()
	h.closed = true
	h.state.Unlock()
	if _, ok := pool.ResolveRuntimeToolContext("bot-1", "rt_live"); ok {
		t.Fatalf("dead runtime context resolved; must fail closed")
	}
}

func TestPromptToolEventSinkPreservesACPAndHTTPToolEventOrder(t *testing.T) {
	sink := newPromptToolEventSink(nil)
	sink.EmitACPEvent(acpclient.StreamEvent{Type: acpclient.StreamEventTextDelta, Delta: "before"})
	sink.EmitToolStreamEvent(mcp.ToolStreamEvent{
		Type:       "tool_call_start",
		ToolCallID: "call-1",
		ToolName:   "schedule_list",
	})
	sink.EmitToolStreamEvent(mcp.ToolStreamEvent{
		Type:       "tool_call_end",
		ToolCallID: "call-1",
		ToolName:   "schedule_list",
		Result:     map[string]any{"ok": true},
	})
	sink.EmitACPEvent(acpclient.StreamEvent{Type: acpclient.StreamEventTextDelta, Delta: "after"})

	events := sink.Events()
	if len(events) != 4 {
		t.Fatalf("events = %#v", events)
	}
	if events[0].Type != acpclient.StreamEventTextDelta || events[1].Type != acpclient.StreamEventToolCallStart || events[2].Type != acpclient.StreamEventToolCallEnd || events[3].Type != acpclient.StreamEventTextDelta {
		t.Fatalf("events order = %#v", events)
	}
}

// Resolving a bound runtime (e.g. the UI keeping it ensured while the user
// types) counts as activity and must defer idle reaping.
func TestSessionPoolEnsureRefreshesIdleClock(t *testing.T) {
	pool := newFakeScriptPool(t)
	pool.timeout = 30 * time.Minute

	stale := time.Now().Add(-29 * time.Minute)
	h := &runtimeHandle{
		id:           newRuntimeID(),
		botID:        "bot-1",
		agentID:      acpprofile.AgentCodexID,
		projectPath:  "/data/project",
		status:       stateIdle,
		lastActive:   stale,
		boundSession: "session-1",
		session:      &acpclient.Session{},
	}
	injectRuntime(pool, h)

	if _, err := pool.Ensure(context.Background(), PromptInput{
		BotID:       "bot-1",
		SessionID:   "session-1",
		AgentID:     acpprofile.AgentCodexID,
		ProjectPath: "/data/project",
	}); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}

	h.state.Lock()
	refreshed := h.lastActive.After(stale)
	h.state.Unlock()
	if !refreshed {
		t.Fatalf("Ensure did not refresh the idle clock")
	}
	// Two minutes later (31 minutes after the original activity) the runtime
	// must survive the reaper because the ensure refreshed it.
	if got := pool.reapIdle(time.Now().Add(2 * time.Minute)); got != 0 {
		t.Fatalf("reapIdle() = %d, want 0 after ensure refresh", got)
	}
}

func TestSessionPoolReapIdlePolicies(t *testing.T) {
	pool := newSessionPool(nil, nil, nil)
	pool.timeout = 30 * time.Minute
	now := time.Now()

	injectRuntime(pool, &runtimeHandle{id: "rt_bound-stale", botID: "b", boundSession: "s1", status: stateIdle, lastActive: now.Add(-31 * time.Minute)})
	injectRuntime(pool, &runtimeHandle{id: "rt_bound-active", botID: "b", boundSession: "s2", status: stateActive, lastActive: now.Add(-31 * time.Minute)})
	injectRuntime(pool, &runtimeHandle{id: "rt_bound-fresh", botID: "b", boundSession: "s3", status: stateIdle, lastActive: now.Add(-30 * time.Second)})
	injectRuntime(pool, &runtimeHandle{id: "rt_unbound-stale", botID: "b", status: stateIdle, lastActive: now.Add(-6 * time.Minute)})
	injectRuntime(pool, &runtimeHandle{id: "rt_bound-6m", botID: "b", boundSession: "s4", status: stateIdle, lastActive: now.Add(-6 * time.Minute)})

	if got := pool.reapIdle(now); got != 2 {
		t.Fatalf("reapIdle() = %d, want 2", got)
	}
	pool.mu.RLock()
	defer pool.mu.RUnlock()
	if _, ok := pool.runtimes["rt_bound-stale"]; ok {
		t.Fatalf("stale bound runtime was not reaped")
	}
	if _, ok := pool.runtimes["rt_unbound-stale"]; ok {
		t.Fatalf("stale unbound runtime was not reaped (5 minute policy)")
	}
	if _, ok := pool.runtimes["rt_bound-active"]; !ok {
		t.Fatalf("active runtime must not be reaped")
	}
	if _, ok := pool.runtimes["rt_bound-fresh"]; !ok {
		t.Fatalf("fresh runtime must not be reaped")
	}
	if _, ok := pool.runtimes["rt_bound-6m"]; !ok {
		t.Fatalf("bound runtime must use the 30 minute policy")
	}
	if _, ok := pool.bySession["s1"]; ok {
		t.Fatalf("reap left the session index entry behind")
	}
}

type fakeBotGetter struct {
	bot bots.Bot
	err error
}

func (g fakeBotGetter) Get(context.Context, string) (bots.Bot, error) {
	return g.bot, g.err
}

type fakeSessionGetter struct {
	session sessionpkg.Session
	err     error
}

func (g fakeSessionGetter) Get(context.Context, string) (sessionpkg.Session, error) {
	return g.session, g.err
}

type recordingRunner struct {
	info     bridge.WorkspaceInfo
	req      acpclient.StartRequest
	startErr error
}

type blockingRunner struct {
	info    bridge.WorkspaceInfo
	started chan struct{}
	release chan struct{}
}

type delayedStartRunner struct {
	info    bridge.WorkspaceInfo
	started chan struct{}
	release chan struct{}
	session *acpclient.Session
}

type cancelAwareStartRunner struct {
	info      bridge.WorkspaceInfo
	started   chan struct{}
	cancelled chan struct{}
}

func (r *blockingRunner) WorkspaceInfo(context.Context, string) (bridge.WorkspaceInfo, error) {
	return r.info, nil
}

func (r *blockingRunner) StartSession(context.Context, acpclient.StartRequest, acpclient.EventSink) (*acpclient.Session, error) {
	close(r.started)
	<-r.release
	return nil, errors.New("released")
}

func (r *delayedStartRunner) WorkspaceInfo(context.Context, string) (bridge.WorkspaceInfo, error) {
	return r.info, nil
}

func (r *delayedStartRunner) StartSession(context.Context, acpclient.StartRequest, acpclient.EventSink) (*acpclient.Session, error) {
	close(r.started)
	<-r.release
	return r.session, nil
}

func (r *cancelAwareStartRunner) WorkspaceInfo(context.Context, string) (bridge.WorkspaceInfo, error) {
	return r.info, nil
}

func (r *cancelAwareStartRunner) StartSession(ctx context.Context, _ acpclient.StartRequest, _ acpclient.EventSink) (*acpclient.Session, error) {
	close(r.started)
	<-ctx.Done()
	close(r.cancelled)
	return nil, ctx.Err()
}

func (r *recordingRunner) WorkspaceInfo(context.Context, string) (bridge.WorkspaceInfo, error) {
	return r.info, nil
}

func (r *recordingRunner) StartSession(_ context.Context, req acpclient.StartRequest, _ acpclient.EventSink) (*acpclient.Session, error) {
	r.req = req
	return nil, r.startErr
}

type sessionPoolWorkspace struct {
	client *bridge.Client
	info   bridge.WorkspaceInfo
}

func (w sessionPoolWorkspace) MCPClient(context.Context, string) (*bridge.Client, error) {
	return w.client, nil
}

func (w sessionPoolWorkspace) WorkspaceInfo(context.Context, string) (bridge.WorkspaceInfo, error) {
	return w.info, nil
}

// TestSessionPoolProtocolPinsRespectSelfMode asserts the protocol pins
// (session mode, config options) follow the same rule as managed settings and
// env: managed setups get them, self means the host's own configuration as-is.
func TestSessionPoolProtocolPinsRespectSelfMode(t *testing.T) {
	t.Parallel()

	managedRunner := &recordingRunner{
		info:     bridge.WorkspaceInfo{Backend: "container"},
		startErr: errors.New("started"),
	}
	managedPool := newSessionPool(nil, managedRunner, fakeBotGetter{bot: enabledACPAgentBot("bot-1", acpprofile.AgentClaudeCodeID, "oauth", map[string]any{"oauth_token": "token"})})
	if _, err := managedPool.Prompt(context.Background(), PromptInput{
		BotID:       "bot-1",
		SessionID:   "session-1",
		AgentID:     acpprofile.AgentClaudeCodeID,
		ProjectPath: "/data/project",
		Prompt:      "run",
	}); err == nil || err.Error() != "started" {
		t.Fatalf("managed prompt error = %v, want runner start error", err)
	}
	if managedRunner.req.SessionMode != "default" {
		t.Fatalf("managed SessionMode = %q, want pinned default", managedRunner.req.SessionMode)
	}
	if managedRunner.req.SessionConfigValues["effort"] != "high" {
		t.Fatalf("managed SessionConfigValues = %v, want effort=high", managedRunner.req.SessionConfigValues)
	}

	selfRunner := &recordingRunner{
		info:     bridge.WorkspaceInfo{Backend: "container"},
		startErr: errors.New("started"),
	}
	selfPool := newSessionPool(nil, selfRunner, fakeBotGetter{bot: enabledACPAgentBot("bot-1", acpprofile.AgentClaudeCodeID, "self", nil)})
	if _, err := selfPool.Prompt(context.Background(), PromptInput{
		BotID:       "bot-1",
		SessionID:   "session-1",
		AgentID:     acpprofile.AgentClaudeCodeID,
		ProjectPath: "/data/project",
		Prompt:      "run",
	}); err == nil || err.Error() != "started" {
		t.Fatalf("self prompt error = %v, want runner start error", err)
	}
	if selfRunner.req.SessionMode != "" || selfRunner.req.SessionConfigValues != nil {
		t.Fatalf("self mode must not carry protocol pins: mode=%q config=%v",
			selfRunner.req.SessionMode, selfRunner.req.SessionConfigValues)
	}
}

func enabledACPBot(id, mode string, managed map[string]any) bots.Bot {
	return enabledACPAgentBot(id, acpprofile.AgentCodexID, mode, managed)
}

func enabledACPAgentBot(id, agentID, mode string, managed map[string]any) bots.Bot {
	if managed == nil {
		managed = map[string]any{}
	}
	return bots.Bot{
		ID: id,
		Metadata: map[string]any{
			"acp": map[string]any{
				"agents": map[string]any{
					agentID: map[string]any{
						"enabled":    true,
						"setup_mode": mode,
						"managed":    managed,
					},
				},
			},
		},
	}
}

func startRequestEnvHas(env []string, key, want string) bool {
	prefix := key + "="
	for _, item := range env {
		if strings.HasPrefix(item, prefix) {
			return strings.TrimPrefix(item, prefix) == want
		}
	}
	return false
}

func newSessionPoolBridgeClient(t *testing.T, root string) *bridge.Client {
	t.Helper()
	listener := bufconn.Listen(16 * 1024 * 1024)
	server := grpc.NewServer(
		grpc.MaxRecvMsgSize(16*1024*1024),
		grpc.MaxSendMsgSize(16*1024*1024),
	)
	pb.RegisterContainerServiceServer(server, bridgesvc.New(bridgesvc.Options{
		DefaultWorkDir:    root,
		WorkspaceRoot:     root,
		DataMount:         config.DefaultDataMount,
		AllowHostAbsolute: true,
	}))
	go func() {
		_ = server.Serve(listener)
	}()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})

	conn, err := grpc.NewClient("passthrough:///acpagent-sessionpool-test",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return listener.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(16*1024*1024),
			grpc.MaxCallSendMsgSize(16*1024*1024),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return bridge.NewClientFromConn(conn)
}

func readSessionPoolFile(t *testing.T, root string, parts ...string) string {
	t.Helper()
	pathParts := append([]string{root}, parts...)
	content, err := os.ReadFile(filepath.Join(pathParts...)) //nolint:gosec // reads from t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func writeSessionPoolFakeAgentScript(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	script := fmt.Sprintf("#!/bin/sh\nif [ -n \"${MEMOH_ACP_START_LOG:-}\" ]; then printf 'start\\n' >> \"$MEMOH_ACP_START_LOG\"; fi\nMEMOH_ACP_SESSION_POOL_FAKE_AGENT=1 exec %s -test.run '^TestSessionPoolFakeAgentHelper$' --\n", sessionPoolShellArg(os.Args[0]))
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // test helper must be executable.
		t.Fatal(err)
	}
	return path
}

func TestSessionPoolFakeAgentHelper(_ *testing.T) {
	if os.Getenv("MEMOH_ACP_SESSION_POOL_FAKE_AGENT") != "1" {
		return
	}
	agent := &sessionPoolFakeAgent{cancelled: make(chan struct{})}
	conn := acp.NewAgentSideConnection(agent, os.Stdout, os.Stdin)
	agent.conn = conn
	<-conn.Done()
	os.Exit(0)
}

type sessionPoolFakeAgent struct {
	conn       *acp.AgentSideConnection
	cancelOnce sync.Once
	cancelled  chan struct{}
}

func (*sessionPoolFakeAgent) Authenticate(context.Context, acp.AuthenticateRequest) (acp.AuthenticateResponse, error) {
	return acp.AuthenticateResponse{}, nil
}

func (*sessionPoolFakeAgent) Initialize(context.Context, acp.InitializeRequest) (acp.InitializeResponse, error) {
	return acp.InitializeResponse{
		ProtocolVersion:   acp.ProtocolVersionNumber,
		AgentCapabilities: acp.AgentCapabilities{LoadSession: false},
	}, nil
}

func (a *sessionPoolFakeAgent) Cancel(context.Context, acp.CancelNotification) error {
	a.cancelOnce.Do(func() { close(a.cancelled) })
	return nil
}

func (*sessionPoolFakeAgent) CloseSession(context.Context, acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	return acp.CloseSessionResponse{}, nil
}

func (*sessionPoolFakeAgent) ListSessions(context.Context, acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	return acp.ListSessionsResponse{}, nil
}

func (*sessionPoolFakeAgent) NewSession(context.Context, acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	resp := acp.NewSessionResponse{SessionId: acp.SessionId("session-pool-fake-session")}
	if os.Getenv("MEMOH_ACP_SESSION_POOL_FAKE_AGENT_MODELS") == "1" {
		description := "Highest reasoning"
		resp.Models = &acp.SessionModelState{
			CurrentModelId: acp.ModelId("gpt-5.1-codex"),
			AvailableModels: []acp.ModelInfo{
				{ModelId: acp.ModelId("gpt-5.1-codex"), Name: "GPT-5.1 Codex"},
				{ModelId: acp.ModelId("gpt-5.1-codex-high"), Name: "GPT-5.1 Codex High", Description: &description},
			},
		}
	}
	return resp, nil
}

func (*sessionPoolFakeAgent) UnstableSetSessionModel(_ context.Context, p acp.UnstableSetSessionModelRequest) (acp.UnstableSetSessionModelResponse, error) {
	if p.SessionId != acp.SessionId("session-pool-fake-session") {
		return acp.UnstableSetSessionModelResponse{}, fmt.Errorf("unexpected session id %q", p.SessionId)
	}
	if p.ModelId == "" {
		return acp.UnstableSetSessionModelResponse{}, errors.New("missing model id")
	}
	return acp.UnstableSetSessionModelResponse{}, nil
}

func (a *sessionPoolFakeAgent) Prompt(ctx context.Context, p acp.PromptRequest) (acp.PromptResponse, error) {
	text := sessionPoolPromptText(p)
	if strings.Contains(text, "[hang]") {
		// Behave like a real agent mid-turn: keep working until the client
		// sends session/cancel (or the connection dies), then stop cleanly.
		select {
		case <-a.cancelled:
		case <-ctx.Done():
		}
		return acp.PromptResponse{StopReason: acp.StopReasonCancelled}, nil
	}
	reply := "session-pool-ok"
	if strings.Contains(text, "memoh://context/session-history") {
		// Echo a marker so tests can assert whether the prompt carried the
		// replayed-history resource.
		reply = "history-replayed " + reply
	}
	_ = a.conn.SessionUpdate(ctx, acp.SessionNotification{
		SessionId: p.SessionId,
		Update:    acp.UpdateAgentMessageText(reply),
	})
	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

func sessionPoolPromptText(p acp.PromptRequest) string {
	var sb strings.Builder
	for _, block := range p.Prompt {
		if block.Text != nil {
			sb.WriteString(block.Text.Text)
		}
	}
	return sb.String()
}

func (*sessionPoolFakeAgent) ResumeSession(context.Context, acp.ResumeSessionRequest) (acp.ResumeSessionResponse, error) {
	return acp.ResumeSessionResponse{}, nil
}

func (*sessionPoolFakeAgent) SetSessionConfigOption(context.Context, acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	return acp.SetSessionConfigOptionResponse{}, nil
}

func (*sessionPoolFakeAgent) SetSessionMode(context.Context, acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	return acp.SetSessionModeResponse{}, nil
}

func sessionPoolShellArg(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
