package db

import (
	"context"
	"testing"
	"time"

	sqlitesqlc "github.com/memohai/memoh/internal/db/sqlite/sqlc"
)

// TestSQLiteToolApprovalExpiryComparesTimestampsNotText is the regression
// guard for the expiry sweep: created_at is CURRENT_TIMESTAMP text
// ('YYYY-MM-DD HH:MM:SS') while the cutoff arrives from the store adapter as
// RFC3339 ('...T...Z'). Raw text comparison orders ' ' before 'T', which
// expired every same-day pending row; the query must normalize both sides.
func TestSQLiteToolApprovalExpiryComparesTimestampsNotText(t *testing.T) {
	migrations := sqliteMigrationsFS(t)
	dsn := tempSQLiteMigrationDSN(t)
	if err := RunMigrateTarget(nil, MigrationTarget{Driver: DriverSQLite, DSN: dsn}, migrations, "up", nil); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	db := openMigrationSQLite(t, dsn)
	defer closeMigrationSQLite(t, db)
	insertSQLiteACPAgentSession(t, db)

	ctx := context.Background()
	queries := sqlitesqlc.New(db)
	created, err := queries.CreateToolApprovalRequest(ctx, sqlitesqlc.CreateToolApprovalRequestParams{
		BotID:      "00000000-0000-0000-0000-000000000002",
		SessionID:  "00000000-0000-0000-0000-000000000003",
		ToolCallID: "call-fresh",
		ToolName:   "exec",
		ToolInput:  `{"command":"pwd"}`,
	})
	if err != nil {
		t.Fatalf("create approval: %v", err)
	}
	if created.Status != "pending" {
		t.Fatalf("created status = %q, want pending", created.Status)
	}

	// The cutoff is in the past relative to the fresh row, rendered exactly
	// as the store adapter renders timestamps (RFC3339, whole seconds).
	cutoff := time.Now().UTC().Add(-15 * time.Minute).Truncate(time.Second).Format(time.RFC3339)
	expired, err := queries.ExpireStaleToolApprovals(ctx, sqlitesqlc.ExpireStaleToolApprovalsParams{
		Reason:    "expired by test",
		CreatedAt: cutoff,
	})
	if err != nil {
		t.Fatalf("expire (fresh row): %v", err)
	}
	if len(expired) != 0 {
		t.Fatalf("expiry swept %d fresh rows, want 0: %#v", len(expired), expired)
	}

	// Backdate the row past the cutoff: now it must expire.
	if _, err := db.ExecContext(ctx, `UPDATE tool_approval_requests SET created_at = datetime('now','-30 minutes') WHERE id = ?`, created.ID); err != nil {
		t.Fatalf("backdate approval: %v", err)
	}
	expired, err = queries.ExpireStaleToolApprovals(ctx, sqlitesqlc.ExpireStaleToolApprovalsParams{
		Reason:    "expired by test",
		CreatedAt: cutoff,
	})
	if err != nil {
		t.Fatalf("expire (stale row): %v", err)
	}
	if len(expired) != 1 || expired[0].Status != "expired" {
		t.Fatalf("expiry swept %#v, want exactly the backdated row as expired", expired)
	}
}

// TestSQLiteToolApprovalUpsertResetsDecision is the regression guard for the
// (session_id, tool_call_id) is unique and CreateToolApprovalRequest is a
// plain INSERT (no upsert): re-using the pair without deleting the old row
// first must conflict, not silently revive the old row. The new-ask-new-row
// semantics live one layer up in toolapproval.Service.CreatePending (which
// deletes then inserts); this test pins the query contract that makes that
// safe — a stale id can never be addressed because re-creation never reuses
// it.
func TestSQLiteToolApprovalCreateIsInsertNotUpsert(t *testing.T) {
	migrations := sqliteMigrationsFS(t)
	dsn := tempSQLiteMigrationDSN(t)
	if err := RunMigrateTarget(nil, MigrationTarget{Driver: DriverSQLite, DSN: dsn}, migrations, "up", nil); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	db := openMigrationSQLite(t, dsn)
	defer closeMigrationSQLite(t, db)
	insertSQLiteACPAgentSession(t, db)

	ctx := context.Background()
	queries := sqlitesqlc.New(db)
	params := sqlitesqlc.CreateToolApprovalRequestParams{
		BotID:      "00000000-0000-0000-0000-000000000002",
		SessionID:  "00000000-0000-0000-0000-000000000003",
		ToolCallID: "call-reused",
		ToolName:   "exec",
		ToolInput:  `{"command":"rm -rf build"}`,
	}
	if _, err := queries.CreateToolApprovalRequest(ctx, params); err != nil {
		t.Fatalf("create approval: %v", err)
	}
	// A second insert for the same (session, tool_call_id) must conflict —
	// proving there is no upsert that could revive the old row.
	if _, err := queries.CreateToolApprovalRequest(ctx, params); err == nil {
		t.Fatal("second CreateToolApprovalRequest for the same tool_call_id did not conflict; an upsert would silently reuse the row")
	}

	// After deleting the old row (the path CreatePending takes), re-creation
	// succeeds with a fresh row.
	if err := queries.DeleteToolApprovalRequestsBySessionToolCall(ctx, sqlitesqlc.DeleteToolApprovalRequestsBySessionToolCallParams{
		BotID:      params.BotID,
		SessionID:  params.SessionID,
		ToolCallID: params.ToolCallID,
	}); err != nil {
		t.Fatalf("delete prior approval: %v", err)
	}
	if _, err := queries.CreateToolApprovalRequest(ctx, params); err != nil {
		t.Fatalf("re-create after delete: %v", err)
	}
}
