package flow

import (
	"context"
	"database/sql"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	embeddeddb "github.com/memohai/memoh/db"
	"github.com/memohai/memoh/internal/acpagent"
	"github.com/memohai/memoh/internal/acpagent/acptest"
	"github.com/memohai/memoh/internal/acpclient"
	agentpkg "github.com/memohai/memoh/internal/agent"
	agenttools "github.com/memohai/memoh/internal/agent/tools"
	"github.com/memohai/memoh/internal/bots"
	"github.com/memohai/memoh/internal/config"
	"github.com/memohai/memoh/internal/conversation"
	"github.com/memohai/memoh/internal/db"
	sqlitestore "github.com/memohai/memoh/internal/db/sqlite/store"
	dbstore "github.com/memohai/memoh/internal/db/store"
	"github.com/memohai/memoh/internal/mcp"
	messageevent "github.com/memohai/memoh/internal/message/event"
	"github.com/memohai/memoh/internal/session"
	"github.com/memohai/memoh/internal/settings"
	"github.com/memohai/memoh/internal/toolapproval"
	"github.com/memohai/memoh/internal/workspace/bridge"

	"log/slog"
)

// This is the ACP BACKEND end-to-end test: it wires a REAL Resolver to a REAL
// SessionPool (real workspace bridge, real toolapproval.Service over real
// SQLite, real MCP gateway + NativeToolSource), with only the external ACP
// agent faked (the acptest scripted agent). It enters at StreamChatWS — the
// request-lifecycle entry — so it covers the resolver/flow layer that the
// pool-level integration tests skip (context injection, round persistence, the
// acp_turn_stream mirror, approval cascade) AND the pool layer that the
// resolver unit tests fake. The real-Resolver→real-pool seam is exercised
// nowhere else.

const (
	e2eBotID     = "11111111-1111-1111-1111-111111111111"
	e2eSessionID = "22222222-2222-2222-2222-222222222222"
	e2eChannelID = "33333333-3333-3333-3333-333333333333"
)

// TestACPBackendE2EScriptedAgent is the re-exec target for the scripted fake
// ACP agent (a no-op in normal runs; the pool spawns the test binary back into
// it via the PATH-shadowed npx stub).
func TestACPBackendE2EScriptedAgent(_ *testing.T) {
	acptest.RunScriptedAgentIfInvoked()
}

type backendE2E struct {
	resolver *Resolver
	pool     *acpagent.SessionPool
	approval *toolapproval.Service
	messages *recordingMessageService
	events   *collectingPublisher
	root     string
}

// collectingPublisher records the bot-event-hub publishes (the acp_turn_stream
// turn mirror) so the test can assert the turn was mirrored end to end.
type collectingPublisher struct {
	mu     sync.Mutex
	events []messageevent.Event
}

func (p *collectingPublisher) Publish(event messageevent.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, event)
}

func (p *collectingPublisher) byType(typ messageevent.EventType) []messageevent.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []messageevent.Event
	for _, e := range p.events {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

// newBackendE2E wires the whole real backend with a scripted agent. toolApprovalConfig
// (optional) installs a tool-approval policy so write/exec actually gate.
func newBackendE2E(t *testing.T, toolApprovalConfig string) *backendE2E {
	t.Helper()
	queries, sqlDB := newMigratedSQLiteForFlow(t)
	seedBackendE2EBotSession(t, sqlDB, toolApprovalConfig)

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "project"), 0o750); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o750); err != nil {
		t.Fatal(err)
	}
	acptest.WriteScriptedAgentScript(t, binDir, "npx", "TestACPBackendE2EScriptedAgent")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	workspace := acptest.Workspace{
		Client: acptest.BridgeClient(t, root),
		Info:   bridge.WorkspaceInfo{Backend: bridge.WorkspaceBackendLocal, DefaultWorkDir: root},
	}

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

	messages := &recordingMessageService{}
	events := &collectingPublisher{}
	resolver := &Resolver{
		acpPool:        pool,
		messageService: messages,
		toolApproval:   approval,
		eventPublisher: events,
		queries:        queries,
		sessionService: &fakeBackgroundSessionService{
			getFn: func(_ context.Context, sessionID string) (session.Session, error) {
				return session.Session{
					ID:    sessionID,
					BotID: e2eBotID,
					Type:  session.TypeACPAgent,
					Metadata: map[string]any{
						"acp_agent_id": "codex",
						"project_path": "/data/project",
					},
				}, nil
			},
		},
		logger: slog.New(slog.DiscardHandler),
	}

	return &backendE2E{resolver: resolver, pool: pool, approval: approval, messages: messages, events: events, root: root}
}

func newMigratedSQLiteForFlow(t *testing.T) (queries dbstore.Queries, sqlDB *sql.DB) {
	t.Helper()
	dsn := "sqlite://" + filepath.Join(t.TempDir(), "memoh.db")
	migrations, err := fs.Sub(embeddeddb.MigrationsFS, "sqlite/migrations")
	if err != nil {
		t.Fatalf("sqlite migrations fs: %v", err)
	}
	if err := db.RunMigrateTarget(nil, db.MigrationTarget{Driver: db.DriverSQLite, DSN: dsn}, migrations, "up", nil); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	sqlDB, err = db.OpenSQLite(context.Background(), config.SQLiteConfig{DSN: dsn})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	store, err := sqlitestore.New(sqlDB)
	if err != nil {
		t.Fatalf("sqlite store: %v", err)
	}
	return sqlitestore.NewQueries(store), sqlDB
}

func seedBackendE2EBotSession(t *testing.T, sqlDB *sql.DB, toolApprovalConfig string) {
	t.Helper()
	const botMetadata = `{"acp":{"agents":{"codex":{"enabled":true,"setup_mode":"self","managed":{}}}}}`
	botInsert := `INSERT INTO bots(id,owner_user_id,type,name,display_name,metadata) VALUES('` +
		e2eBotID + `','00000000-0000-0000-0000-000000000001','personal','acp-bot','ACP Bot','` + botMetadata + `')`
	if toolApprovalConfig != "" {
		botInsert = `INSERT INTO bots(id,owner_user_id,type,name,display_name,metadata,tool_approval_config) VALUES('` +
			e2eBotID + `','00000000-0000-0000-0000-000000000001','personal','acp-bot','ACP Bot','` + botMetadata + `','` + toolApprovalConfig + `')`
	}
	statements := []string{
		`INSERT INTO users(id,email,role) VALUES('00000000-0000-0000-0000-000000000001','acp@example.com','member')`,
		botInsert,
		`INSERT INTO bot_sessions(id,bot_id,type,title,metadata) VALUES('` + e2eSessionID + `','` + e2eBotID +
			`','acp_agent','Codex','{"acp_agent_id":"codex","project_path":"/data/project"}')`,
		`INSERT INTO channel_identities(id,channel_type,channel_subject_id) VALUES('` + e2eChannelID + `','local','acp-user')`,
	}
	for _, stmt := range statements {
		if _, err := sqlDB.ExecContext(context.Background(), stmt); err != nil {
			t.Fatalf("seed exec %q: %v", stmt, err)
		}
	}
}

// TestACPBackendE2EChatPersistsAndMirrors is the smoke that proves the whole
// real backend boots through the resolver entry point: a scripted SAY turn
// streams text to the WS channel, persists a user+assistant round (with ACP
// metadata), and mirrors the turn onto the event hub.
func TestACPBackendE2EChatPersistsAndMirrors(t *testing.T) {
	e2e := newBackendE2E(t, "")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	eventCh := make(chan WSStreamEvent, 64)
	err := e2e.resolver.StreamChatWS(ctx, conversation.ChatRequest{
		BotID:     e2eBotID,
		SessionID: e2eSessionID,
		StreamID:  "stream-1",
		Query:     "SAY hello from backend e2e",
	}, eventCh, make(chan struct{}))
	if err != nil {
		t.Fatalf("StreamChatWS: %v", err)
	}
	close(eventCh)

	// 1) WS event stream carries the streamed text end to end.
	events := drainAgentEvents(t, eventCh)
	if !containsStreamEvent(events, agentpkg.EventAgentStart) || !containsStreamEvent(events, agentpkg.EventAgentEnd) {
		t.Fatalf("WS stream missing agent start/end: %#v", events)
	}
	if !containsTextDelta(events, "hello from backend e2e") {
		t.Fatalf("WS stream missing text delta: %#v", events)
	}

	// 2) The round persisted through the real flow: user + assistant, ACP metadata.
	if len(e2e.messages.persisted) != 2 {
		t.Fatalf("persisted %d messages, want user+assistant: %#v", len(e2e.messages.persisted), e2e.messages.persisted)
	}
	if e2e.messages.persisted[0].Role != "user" || e2e.messages.persisted[1].Role != "assistant" {
		t.Fatalf("persisted roles = %q, %q", e2e.messages.persisted[0].Role, e2e.messages.persisted[1].Role)
	}
	if got := e2e.messages.persisted[1].Metadata["acp_agent_id"]; got != "codex" {
		t.Fatalf("assistant acp_agent_id metadata = %#v, want codex", got)
	}

	// 3) The turn was mirrored onto the event hub (reconnect/turn surface).
	if mirror := e2e.events.byType(messageevent.EventTypeACPTurnStream); len(mirror) == 0 {
		t.Fatalf("no acp_turn_stream mirror events published; turn was not mirrored")
	}
}

// forceReviewWriteConfig gates every write (no bypass) so a scripted WRITE goes
// through the real approval flow instead of bypassing.
const forceReviewWriteConfig = `{"enabled":true,` +
	`"write":{"require_approval":true,"bypass_globs":[],"force_review_globs":["**"]},` +
	`"edit":{"require_approval":true,"bypass_globs":[],"force_review_globs":["**"]},` +
	`"exec":{"require_approval":false,"bypass_commands":[],"force_review_commands":[]}}`

// TestACPBackendE2EWriteApprovalGatesAndPersists is the approval capstone for
// the whole backend: a scripted WRITE enters at StreamChatWS, the real toolapproval
// service lands a pending row in the real DB, a concurrent "user" approves it,
// and only then does the file land on the real workspace bridge — while the
// approval request surfaces on the WS stream and the turn is mirrored. This is
// the full request→approval→side-effect lifecycle the pool-only tests can't see
// from above and the resolver-only tests fake from below.
func TestACPBackendE2EWriteApprovalGatesAndPersists(t *testing.T) {
	e2e := newBackendE2E(t, forceReviewWriteConfig)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	eventCh := make(chan WSStreamEvent, 256)
	done := make(chan error, 1)
	go func() {
		done <- e2e.resolver.StreamChatWS(ctx, conversation.ChatRequest{
			BotID:     e2eBotID,
			SessionID: e2eSessionID,
			StreamID:  "stream-approval",
			Query:     "WRITE proof.txt approved-content",
		}, eventCh, make(chan struct{}))
	}()

	// A real pending approval must land in the real DB; approve it.
	approved := false
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		pending, err := e2e.approval.ListPendingBySession(context.Background(), e2eBotID, e2eSessionID)
		if err != nil {
			t.Fatalf("ListPendingBySession: %v", err)
		}
		if len(pending) > 0 {
			if _, err := e2e.approval.Approve(context.Background(), pending[0].ID, e2eChannelID, "ok"); err != nil {
				t.Fatalf("Approve: %v", err)
			}
			approved = true
			break
		}
		select {
		case err := <-done:
			t.Fatalf("turn finished before any approval appeared (write was not gated): %v", err)
		default:
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !approved {
		t.Fatal("no pending write approval appeared; the write was not gated")
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("StreamChatWS after approval: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("turn did not finish after approval")
	}
	close(eventCh)

	// 1) The file only lands AFTER approval — real client callback → real bridge.
	if findFileUnderFlow(e2e.root, "proof.txt") == "" {
		t.Fatalf("approved write did not land on disk under %s", e2e.root)
	}
	// 2) The approval request surfaced on the WS stream (the user saw a card).
	events := drainAgentEvents(t, eventCh)
	if !containsStreamEvent(events, agentpkg.EventToolApprovalRequest) {
		t.Fatalf("WS stream missing tool_approval_request: %#v", events)
	}
	// 3) The round persisted and the turn was mirrored.
	if len(e2e.messages.persisted) < 2 {
		t.Fatalf("persisted %d messages, want >=2 (user+assistant)", len(e2e.messages.persisted))
	}
	if len(e2e.events.byType(messageevent.EventTypeACPTurnStream)) == 0 {
		t.Fatal("no acp_turn_stream mirror events published")
	}
}

func findFileUnderFlow(root, name string) string {
	var match string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || match != "" {
			return nil
		}
		if !d.IsDir() && d.Name() == name {
			match = path
		}
		return nil
	})
	return match
}
