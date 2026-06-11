package acpintegration

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/memohai/memoh/internal/acpagent"
	"github.com/memohai/memoh/internal/acpagent/acptest"
	"github.com/memohai/memoh/internal/acpclient"
	agenttools "github.com/memohai/memoh/internal/agent/tools"
	"github.com/memohai/memoh/internal/bots"
	dbstore "github.com/memohai/memoh/internal/db/store"
	"github.com/memohai/memoh/internal/mcp"
	"github.com/memohai/memoh/internal/session"
	"github.com/memohai/memoh/internal/settings"
	"github.com/memohai/memoh/internal/toolapproval"
	"github.com/memohai/memoh/internal/workspace/bridge"
)

// TestACPIntegrationScriptedAgent is the re-exec target for the scripted fake
// ACP agent. In a normal run it is a no-op; the pool spawns the test binary
// back into it (via the PATH-shadowed npx stub) to act as the agent.
func TestACPIntegrationScriptedAgent(_ *testing.T) {
	acptest.RunScriptedAgentIfInvoked()
}

// scenarioPool is a fully-real ACP runtime stack with only the external agent
// faked: real SessionPool, real workspace bridge, real toolapproval.Service
// over real SQLite, real MCP gateway + NativeToolSource, real tool-event
// store. The scripted agent runs the directives embedded in each prompt.
type scenarioPool struct {
	pool      *acpagent.SessionPool
	approval  *toolapproval.Service
	contexts  *mcp.ToolSessionContextStore
	sqlDB     *sql.DB
	botID     string
	sessionID string
	agentID   string
	// dataRoot is the host path bind-mounted at /data for container pools
	// (empty for local pools), so a test can inspect files the agent wrote
	// inside the container.
	dataRoot string
}

const selfModeBotMetadata = `{"acp":{"agents":{"codex":{"enabled":true,"setup_mode":"self","managed":{}}}}}`

func scenarioDirs(t *testing.T) (root, binDir string) {
	t.Helper()
	root = t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "project"), 0o750); err != nil {
		t.Fatal(err)
	}
	binDir = filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o750); err != nil {
		t.Fatal(err)
	}
	return root, binDir
}

// newScenarioPool is the L1 stack: a PATH-shadowed scripted agent stands in
// for the external ACP process, everything else is real.
func newScenarioPool(t *testing.T) *scenarioPool {
	t.Helper()
	root, binDir := scenarioDirs(t)
	// The pool resolves the codex profile's LocalCommand ("npx") off PATH;
	// shadowing it with our stub makes the pool spawn the scripted agent.
	acptest.WriteScriptedAgentScript(t, binDir, "npx", "TestACPIntegrationScriptedAgent")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return assembleScenarioPool(t, root, selfModeBotMetadata, "codex", "")
}

// assembleScenarioPool wires the fully-real runtime stack against a LOCAL
// backend workspace rooted at root, with the given bot ACP metadata, ACP
// agent id, and optional tool-approval config. The L1 harness and the L3
// local live tests share this assembly.
func assembleScenarioPool(t *testing.T, root, botMetadata, agentID, toolApprovalConfig string) *scenarioPool {
	t.Helper()
	queries, sqlDB := newMigratedSQLite(t)
	seedScenarioBot(t, sqlDB, botMetadata, agentID, toolApprovalConfig)
	workspace := acptest.Workspace{
		Client: acptest.BridgeClient(t, root),
		Info: bridge.WorkspaceInfo{
			Backend:        bridge.WorkspaceBackendLocal,
			DefaultWorkDir: root,
		},
	}
	return finishScenarioPool(t, queries, sqlDB, agentID, workspace)
}

// assembleContainerScenarioPool wires the stack against the REAL container
// backend: the agent (codex/claude) runs inside the toolkit container with a
// managed HOME, exactly like production. This is the path the local-backend
// tests cannot reach (e.g. Claude reads HOME/.claude/settings.json, not
// CLAUDE_CONFIG_DIR). Requires docker.
func assembleContainerScenarioPool(t *testing.T, botMetadata, agentID, toolApprovalConfig string) *scenarioPool {
	t.Helper()
	dataRoot := t.TempDir()
	// The session's project_path is /data/project (see seedScenarioBot). The
	// container bind-mounts dataRoot at /data, and the pool chdirs the agent +
	// the command-resolution probes into the project path. Production paths are
	// pre-provisioned; here we must create the subdir on the host so it exists
	// inside the container, otherwise the cwd chdir fails as a fork/exec error.
	if err := os.MkdirAll(filepath.Join(dataRoot, "project"), 0o750); err != nil {
		t.Fatal(err)
	}
	queries, sqlDB := newMigratedSQLite(t)
	seedScenarioBot(t, sqlDB, botMetadata, agentID, toolApprovalConfig)
	workspace := acptest.Workspace{
		Client: acptest.ContainerBridgeClient(t, dataRoot),
		Info: bridge.WorkspaceInfo{
			Backend:        bridge.WorkspaceBackendContainer,
			DefaultWorkDir: "/data",
		},
	}
	sp := finishScenarioPool(t, queries, sqlDB, agentID, workspace)
	sp.dataRoot = dataRoot
	return sp
}

// finishScenarioPool assembles the runtime stack once the bot/session rows
// are seeded and the workspace (local or container) is chosen.
func finishScenarioPool(t *testing.T, queries dbstore.Queries, sqlDB *sql.DB, agentID string, workspace acptest.Workspace) *scenarioPool {
	t.Helper()
	// Real settings service so the approval policy is live: L1 bots carry the
	// default config (everything bypassed except ForceApproval permissions),
	// while a force-review bot makes real tools actually require approval.
	settingsSvc := settings.NewService(slog.New(slog.DiscardHandler), queries, nil, nil)
	approval := toolapproval.NewService(nil, queries, settingsSvc)
	contexts := mcp.NewToolSessionContextStore()
	nativeSource := agenttools.NewNativeToolSource(nil, nil, agenttools.NativeToolSourceOptions{
		AllowAll:   true,
		Approval:   approval,
		ToolEvents: contexts,
	})
	gateway := mcp.NewToolGatewayService(nil, []mcp.ToolSource{nativeSource})

	runner := acpclient.NewRunner(nil, workspace)
	pool := acpagent.NewSessionPool(nil, runner, bots.NewService(nil, queries), session.NewService(nil, queries))
	pool.SetToolApprovalService(approval)
	pool.SetToolSessionContextStore(contexts)
	pool.SetToolGateway(gateway)
	t.Cleanup(pool.CloseAll)

	return &scenarioPool{
		pool:      pool,
		approval:  approval,
		contexts:  contexts,
		sqlDB:     sqlDB,
		botID:     testBotID,
		sessionID: testSessionID,
		agentID:   agentID,
	}
}

func (s *scenarioPool) prompt(t *testing.T, ctx context.Context, script string, sink acpclient.EventSink) (acpclient.PromptResult, error) {
	t.Helper()
	agentID := s.agentID
	if agentID == "" {
		agentID = "codex"
	}
	return s.pool.Prompt(ctx, acpagent.PromptInput{
		BotID:       s.botID,
		SessionID:   s.sessionID,
		StreamID:    "stream-1",
		AgentID:     agentID,
		ProjectPath: "/data/project",
		Prompt:      script,
		Sink:        sink,
	})
}

// collectSink records every ACP stream event the pool emits.
type collectSink struct {
	events []acpclient.StreamEvent
}

func (c *collectSink) EmitACPEvent(event acpclient.StreamEvent) {
	c.events = append(c.events, event)
}

// forceReviewWriteConfig makes every write/edit require approval (no bypass),
// so a real agent writing a file actually triggers the approval flow.
const forceReviewWriteConfig = `{"enabled":true,` +
	`"write":{"require_approval":true,"bypass_globs":[],"force_review_globs":["**"]},` +
	`"edit":{"require_approval":true,"bypass_globs":[],"force_review_globs":["**"]},` +
	`"exec":{"require_approval":false,"bypass_commands":[],"force_review_commands":[]}}`

// execApprovalConfig requires approval for every exec/terminal command, so a
// command an agent runs through the client terminal capability is gated.
const execApprovalConfig = `{"enabled":true,` +
	`"write":{"require_approval":false,"bypass_globs":["**"],"force_review_globs":[]},` +
	`"edit":{"require_approval":false,"bypass_globs":["**"],"force_review_globs":[]},` +
	`"exec":{"require_approval":true,"bypass_commands":[],"force_review_commands":[]}}`

// forceReviewAllConfig gates write, edit AND exec, so a file-creating turn
// parks on a pending approval no matter which tool the agent reaches for
// (Write vs a shell heredoc). Used by the reject/abort live tests, which only
// need the agent definitively blocked mid-turn.
const forceReviewAllConfig = `{"enabled":true,` +
	`"write":{"require_approval":true,"bypass_globs":[],"force_review_globs":["**"]},` +
	`"edit":{"require_approval":true,"bypass_globs":[],"force_review_globs":["**"]},` +
	`"exec":{"require_approval":true,"bypass_commands":[],"force_review_commands":[]}}`

func seedScenarioBot(t *testing.T, sqlDB *sql.DB, botMetadata, agentID, toolApprovalConfig string) {
	t.Helper()
	sessionMetadata := `{"acp_agent_id":"` + agentID + `","project_path":"/data/project"}`
	botInsert := `INSERT INTO bots(id,owner_user_id,type,name,display_name,metadata) VALUES('` +
		testBotID + `','00000000-0000-0000-0000-000000000001','personal','acp-bot','ACP Bot','` + botMetadata + `')`
	if toolApprovalConfig != "" {
		botInsert = `INSERT INTO bots(id,owner_user_id,type,name,display_name,metadata,tool_approval_config) VALUES('` +
			testBotID + `','00000000-0000-0000-0000-000000000001','personal','acp-bot','ACP Bot','` +
			botMetadata + `','` + toolApprovalConfig + `')`
	}
	statements := []string{
		`INSERT INTO users(id,email,role) VALUES('00000000-0000-0000-0000-000000000001','acp@example.com','member')`,
		botInsert,
		`INSERT INTO bot_sessions(id,bot_id,type,title,metadata) VALUES('` + testSessionID + `','` + testBotID + `','acp_agent','Codex','` + sessionMetadata + `')`,
		`INSERT INTO channel_identities(id,channel_type,channel_subject_id) VALUES('` + testChannelID + `','local','acp-user')`,
	}
	for _, stmt := range statements {
		if _, err := sqlDB.ExecContext(context.Background(), stmt); err != nil {
			t.Fatalf("seed exec %q: %v", stmt, err)
		}
	}
}

// TestACPScenarioBasicPrompt is the smoke that proves the whole real stack
// boots: a scripted SAY/THINK turn streams reasoning + text through the real
// pool and a real (PATH-shadowed) agent process.
func TestACPScenarioBasicPrompt(t *testing.T) {
	sp := newScenarioPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sink := &collectSink{}
	result, err := sp.prompt(t, ctx, "THINK working on it\nSAY hello from the agent", sink)
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if !strings.Contains(result.Text, "hello from the agent") {
		t.Fatalf("result text = %q, want the streamed answer", result.Text)
	}
	if !hasEvent(sink.events, acpclient.StreamEventReasoningDelta, "working on it") {
		t.Fatalf("missing reasoning event: %#v", sink.events)
	}
	if !hasEvent(sink.events, acpclient.StreamEventTextDelta, "hello from the agent") {
		t.Fatalf("missing text event: %#v", sink.events)
	}
}

func hasEvent(events []acpclient.StreamEvent, typ acpclient.StreamEventType, contains string) bool {
	for _, event := range events {
		if event.Type == typ && strings.Contains(event.Delta, contains) {
			return true
		}
	}
	return false
}
