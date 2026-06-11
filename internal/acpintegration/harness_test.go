// Package acpintegration holds in-process, full-stack integration tests for
// the ACP agent path. Unlike the per-package unit tests (which fake their
// neighbours), these wire REAL collaborators together — real SQLite via real
// migrations, the real toolapproval.Service, the real MCP tool gateway — so
// they catch the seam bugs that single-layer tests with fakes structurally
// cannot: text-vs-timestamp comparison in the DB, decision replay across an
// upsert, approval events taking the wrong channel between the gateway and
// the runtime.
//
// The one thing left faked is the external ACP agent itself; agent behaviour
// is covered by the live smoke tests (codex_live_test.go) instead. Everything
// on Memoh's own side of the contract is real here.
package acpintegration

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"testing"

	embeddeddb "github.com/memohai/memoh/db"
	"github.com/memohai/memoh/internal/config"
	"github.com/memohai/memoh/internal/db"
	sqlitestore "github.com/memohai/memoh/internal/db/sqlite/store"
	dbstore "github.com/memohai/memoh/internal/db/store"
)

const (
	testBotID     = "11111111-1111-1111-1111-111111111111"
	testSessionID = "22222222-2222-2222-2222-222222222222"
	testChannelID = "33333333-3333-3333-3333-333333333333"
)

// newMigratedSQLite opens a fresh SQLite database, runs the real migrations,
// and returns the dbstore.Queries adapter the production services consume.
func newMigratedSQLite(t *testing.T) (dbstore.Queries, *sql.DB) {
	t.Helper()
	dsn := "sqlite://" + filepath.Join(t.TempDir(), "memoh.db")
	migrations, err := fs.Sub(embeddeddb.MigrationsFS, "sqlite/migrations")
	if err != nil {
		t.Fatalf("sqlite migrations fs: %v", err)
	}
	if err := db.RunMigrateTarget(nil, db.MigrationTarget{Driver: db.DriverSQLite, DSN: dsn}, migrations, "up", nil); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	sqlDB, err := db.OpenSQLite(context.Background(), config.SQLiteConfig{DSN: dsn})
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

// seedBotSession inserts the bot + session + channel-identity rows the tool
// approval foreign keys require.
func seedBotSession(t *testing.T, sqlDB *sql.DB) {
	t.Helper()
	statements := []string{
		`INSERT INTO users(id,email,role) VALUES('00000000-0000-0000-0000-000000000001','acp@example.com','member')`,
		`INSERT INTO bots(id,owner_user_id,type,name,display_name) VALUES('` + testBotID + `','00000000-0000-0000-0000-000000000001','personal','acp-bot','ACP Bot')`,
		`INSERT INTO bot_sessions(id,bot_id,type,title,metadata) VALUES('` + testSessionID + `','` + testBotID + `','acp_agent','Codex','{}')`,
		`INSERT INTO channel_identities(id,channel_type,channel_subject_id) VALUES('` + testChannelID + `','local','acp-user')`,
	}
	for _, stmt := range statements {
		if _, err := sqlDB.ExecContext(context.Background(), stmt); err != nil {
			t.Fatalf("seed exec %q: %v", stmt, err)
		}
	}
}
