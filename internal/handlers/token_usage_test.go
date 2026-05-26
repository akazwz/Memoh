package handlers

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v4"

	"github.com/memohai/memoh/internal/bots"
	"github.com/memohai/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/memohai/memoh/internal/db/store"
)

type tokenUsageQueries struct {
	dbstore.Queries
	bot         sqlc.GetBotByIDRow
	listCalled  bool
	countCalled bool
	listParams  sqlc.ListTokenUsageRecordsParams
	countParams sqlc.CountTokenUsageRecordsParams
}

func (q *tokenUsageQueries) GetBotByID(_ context.Context, _ pgtype.UUID) (sqlc.GetBotByIDRow, error) {
	return q.bot, nil
}

func (q *tokenUsageQueries) ListTokenUsageRecords(_ context.Context, arg sqlc.ListTokenUsageRecordsParams) ([]sqlc.ListTokenUsageRecordsRow, error) {
	q.listCalled = true
	q.listParams = arg
	return []sqlc.ListTokenUsageRecordsRow{
		{
			ID:          testUUID("55555555-5555-5555-5555-555555555555"),
			SessionID:   testUUID("66666666-6666-6666-6666-666666666666"),
			SessionType: "acp_agent",
			ModelSlug:   "codex",
			ModelName:   "Codex",
		},
	}, nil
}

func (q *tokenUsageQueries) CountTokenUsageRecords(_ context.Context, arg sqlc.CountTokenUsageRecordsParams) (int64, error) {
	q.countCalled = true
	q.countParams = arg
	return 1, nil
}

func TestListTokenUsageRecordsAllowsACPAgentFilter(t *testing.T) {
	botID := "11111111-1111-1111-1111-111111111111"
	queries := &tokenUsageQueries{bot: testBotRow(botID, map[string]any{})}
	handler := NewTokenUsageHandler(
		slog.Default(),
		queries,
		bots.NewService(nil, queries),
		newTestAdminAccountService("admin"),
	)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/bots/"+botID+"/token-usage/records?from=2026-05-01&to=2026-05-02&session_type=acp_agent", nil)
	rec := httptest.NewRecorder()
	ctx := testAuthContext(e, req, rec, "user-1")
	ctx.SetPath("/bots/:bot_id/token-usage/records")
	ctx.SetParamNames("bot_id")
	ctx.SetParamValues(botID)

	if err := handler.ListTokenUsageRecords(ctx); err != nil {
		t.Fatalf("ListTokenUsageRecords() error = %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !queries.listCalled || !queries.countCalled {
		t.Fatalf("expected list and count queries to run")
	}
	if !queries.listParams.SessionType.Valid || queries.listParams.SessionType.String != "acp_agent" {
		t.Fatalf("list session type = %#v, want acp_agent", queries.listParams.SessionType)
	}
	if !queries.countParams.SessionType.Valid || queries.countParams.SessionType.String != "acp_agent" {
		t.Fatalf("count session type = %#v, want acp_agent", queries.countParams.SessionType)
	}
}

func TestListTokenUsageRecordsRejectsUnknownSessionType(t *testing.T) {
	botID := "11111111-1111-1111-1111-111111111111"
	queries := &tokenUsageQueries{bot: testBotRow(botID, map[string]any{})}
	handler := NewTokenUsageHandler(
		slog.Default(),
		queries,
		bots.NewService(nil, queries),
		newTestAdminAccountService("admin"),
	)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/bots/"+botID+"/token-usage/records?from=2026-05-01&to=2026-05-02&session_type=conversation", nil)
	rec := httptest.NewRecorder()
	ctx := testAuthContext(e, req, rec, "user-1")
	ctx.SetPath("/bots/:bot_id/token-usage/records")
	ctx.SetParamNames("bot_id")
	ctx.SetParamValues(botID)

	err := handler.ListTokenUsageRecords(ctx)
	if err == nil {
		t.Fatalf("ListTokenUsageRecords() error = nil, want HTTP 400")
	}
	var httpErr *echo.HTTPError
	if !errors.As(err, &httpErr) || httpErr.Code != http.StatusBadRequest {
		t.Fatalf("ListTokenUsageRecords() error = %v, want HTTP 400", err)
	}
	if queries.listCalled || queries.countCalled {
		t.Fatalf("usage queries should not run for invalid session_type")
	}
}
