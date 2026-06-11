package db

import (
	"context"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	embeddeddb "github.com/memohai/memoh/db"
	postgressqlc "github.com/memohai/memoh/internal/db/postgres/sqlc"
)

// TestPostgresToolApprovalSemantics is the Postgres engine-level counterpart of
// the SQLite approval-semantics regressions (tool_approval_semantics_test.go).
// The two P0s this guards against were both SQL-dialect bugs (SQLite compared
// timestamps as text; an upsert replayed a stale decision), and those were only
// locked on SQLite — production runs Postgres. This runs the SAME two semantics
// against a REAL Postgres so the dialect can't drift.
//
// Env-gated on MEMOH_TEST_POSTGRES_DSN, which must be an ADMIN/maintenance DSN
// pointed at a maintenance database — the test CREATEs a throwaway database off
// it, migrates THAT, and DROPs it, so the app database is never touched.
// URL-encode the password if it contains reserved characters (e.g. p%40ss for
// p@ss).
func TestPostgresToolApprovalSemantics(t *testing.T) {
	adminDSN := strings.TrimSpace(os.Getenv("MEMOH_TEST_POSTGRES_DSN"))
	if adminDSN == "" {
		t.Skip("set MEMOH_TEST_POSTGRES_DSN (admin/maintenance DSN, NOT the app DB) to run the Postgres approval-semantics regression")
	}

	pool := withTempPostgresDB(t, adminDSN)
	seedPostgresACPAgentSession(t, pool)

	ctx := context.Background()
	queries := postgressqlc.New(pool)
	botID := pgUUID(t, "00000000-0000-0000-0000-000000000002")
	sessionID := pgUUID(t, "00000000-0000-0000-0000-000000000003")

	// (1) Expiry must compare timestamps as time, not text: a fresh row is not
	// swept by a past cutoff; a backdated row is.
	t.Run("ExpiryComparesTimestampsNotText", func(t *testing.T) {
		created, err := queries.CreateToolApprovalRequest(ctx, postgressqlc.CreateToolApprovalRequestParams{
			BotID:      botID,
			SessionID:  sessionID,
			ToolCallID: "call-fresh-pg",
			ToolName:   "exec",
			ToolInput:  []byte(`{"command":"pwd"}`),
		})
		if err != nil {
			t.Fatalf("create approval: %v", err)
		}
		if created.Status != "pending" {
			t.Fatalf("created status = %q, want pending", created.Status)
		}

		cutoff := pgtype.Timestamptz{Time: time.Now().UTC().Add(-15 * time.Minute).Truncate(time.Second), Valid: true}
		expired, err := queries.ExpireStaleToolApprovals(ctx, postgressqlc.ExpireStaleToolApprovalsParams{
			Reason:    "expired by test",
			CreatedAt: cutoff,
		})
		if err != nil {
			t.Fatalf("expire (fresh row): %v", err)
		}
		if len(expired) != 0 {
			t.Fatalf("expiry swept %d fresh rows, want 0: %#v", len(expired), expired)
		}

		if _, err := pool.Exec(ctx, "UPDATE tool_approval_requests SET created_at = now() - interval '30 minutes' WHERE id = $1", created.ID); err != nil {
			t.Fatalf("backdate approval: %v", err)
		}
		expired, err = queries.ExpireStaleToolApprovals(ctx, postgressqlc.ExpireStaleToolApprovalsParams{
			Reason:    "expired by test",
			CreatedAt: cutoff,
		})
		if err != nil {
			t.Fatalf("expire (stale row): %v", err)
		}
		if len(expired) != 1 || expired[0].Status != "expired" {
			t.Fatalf("expiry swept %#v, want exactly the backdated row as expired", expired)
		}
	})

	// (2) CreateToolApprovalRequest is a plain INSERT (no upsert): re-using the
	// same (session_id, tool_call_id) must conflict, never silently revive the
	// old row. new-ask-new-row safety in toolapproval.Service depends on this.
	t.Run("CreateIsInsertNotUpsert", func(t *testing.T) {
		params := postgressqlc.CreateToolApprovalRequestParams{
			BotID:      botID,
			SessionID:  sessionID,
			ToolCallID: "call-reused-pg",
			ToolName:   "exec",
			ToolInput:  []byte(`{"command":"rm -rf build"}`),
		}
		if _, err := queries.CreateToolApprovalRequest(ctx, params); err != nil {
			t.Fatalf("create approval: %v", err)
		}
		if _, err := queries.CreateToolApprovalRequest(ctx, params); err == nil {
			t.Fatal("second CreateToolApprovalRequest for the same tool_call_id did not conflict; an upsert would silently reuse the row")
		}
		if err := queries.DeleteToolApprovalRequestsBySessionToolCall(ctx, postgressqlc.DeleteToolApprovalRequestsBySessionToolCallParams{
			BotID:      botID,
			SessionID:  sessionID,
			ToolCallID: params.ToolCallID,
		}); err != nil {
			t.Fatalf("delete prior approval: %v", err)
		}
		if _, err := queries.CreateToolApprovalRequest(ctx, params); err != nil {
			t.Fatalf("re-create after delete: %v", err)
		}
	})
}

// withTempPostgresDB connects to the admin DSN, creates a unique throwaway
// database, migrates it up, and returns a pool to it. The throwaway database is
// DROPped on cleanup so the regression never touches the dev/app database.
func withTempPostgresDB(t *testing.T, adminDSN string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect admin postgres: %v", err)
	}
	dbName := fmt.Sprintf("memoh_acp_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, `CREATE DATABASE "`+dbName+`"`); err != nil {
		admin.Close()
		t.Fatalf("create temp database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `DROP DATABASE IF EXISTS "`+dbName+`" WITH (FORCE)`)
		admin.Close()
	})

	u, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatalf("parse admin dsn: %v", err)
	}
	u.Path = "/" + dbName
	testDSN := u.String()

	if err := RunMigrateTarget(nil, MigrationTarget{Driver: DriverPostgres, DSN: testDSN}, postgresMigrationsFS(t), "up", nil); err != nil {
		t.Fatalf("migrate temp db up: %v", err)
	}
	pool, err := pgxpool.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("connect temp db: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func postgresMigrationsFS(t *testing.T) fs.FS {
	t.Helper()
	migrations, err := fs.Sub(embeddeddb.MigrationsFS, "postgres/migrations")
	if err != nil {
		t.Fatalf("postgres migrations fs: %v", err)
	}
	return migrations
}

func seedPostgresACPAgentSession(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	statements := []string{
		`INSERT INTO users(id,email,role) VALUES('00000000-0000-0000-0000-000000000001','acp@example.com','member')`,
		// NB: Postgres dropped bots.type in migration 0033 (SQLite kept it — a
		// real cross-dialect schema drift), so the PG seed must omit it.
		`INSERT INTO bots(id,owner_user_id,name,display_name) VALUES('00000000-0000-0000-0000-000000000002','00000000-0000-0000-0000-000000000001','acp-bot','ACP Bot')`,
		`INSERT INTO bot_sessions(id,bot_id,type,title,metadata) VALUES('00000000-0000-0000-0000-000000000003','00000000-0000-0000-0000-000000000002','acp_agent','Codex','{}')`,
	}
	for _, stmt := range statements {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
}

func pgUUID(t *testing.T, s string) pgtype.UUID {
	t.Helper()
	var u pgtype.UUID
	if err := u.Scan(s); err != nil {
		t.Fatalf("parse uuid %q: %v", s, err)
	}
	return u
}
