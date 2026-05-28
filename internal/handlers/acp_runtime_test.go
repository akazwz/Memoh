package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v4"

	"github.com/memohai/memoh/internal/acpagent"
	"github.com/memohai/memoh/internal/acpclient"
	"github.com/memohai/memoh/internal/acpprofile"
	"github.com/memohai/memoh/internal/bots"
	"github.com/memohai/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/memohai/memoh/internal/db/store"
	"github.com/memohai/memoh/internal/session"
)

type acpRuntimeQueries struct {
	dbstore.Queries
	bot     sqlc.GetBotByIDRow
	session sqlc.BotSession
}

type fakeACPRuntimePool struct {
	status        acpagent.RuntimeStatus
	ensureInput   acpagent.PromptInput
	setModelInput acpagent.PromptInput
	setModelID    string
}

func (*fakeACPRuntimePool) RuntimeStatus(sessionID, agentID, projectPath string) acpagent.RuntimeStatus {
	return acpagent.RuntimeStatus{
		SessionID:   sessionID,
		AgentID:     agentID,
		ProjectPath: projectPath,
		State:       "idle",
	}
}

func (p *fakeACPRuntimePool) Ensure(_ context.Context, input acpagent.PromptInput) (acpagent.RuntimeStatus, error) {
	p.ensureInput = input
	return p.status, nil
}

func (p *fakeACPRuntimePool) SetModel(_ context.Context, input acpagent.PromptInput, modelID string) (acpagent.RuntimeStatus, error) {
	p.setModelInput = input
	p.setModelID = modelID
	return p.status, nil
}

func (q acpRuntimeQueries) GetBotByID(_ context.Context, _ pgtype.UUID) (sqlc.GetBotByIDRow, error) {
	return q.bot, nil
}

func (q acpRuntimeQueries) GetSessionByID(_ context.Context, _ pgtype.UUID) (sqlc.BotSession, error) {
	return q.session, nil
}

func TestACPRuntimeHandlerReturnsIdleStatus(t *testing.T) {
	botID := "11111111-1111-1111-1111-111111111111"
	sessionID := "22222222-2222-2222-2222-222222222222"
	queries := acpRuntimeQueries{
		bot: testBotRow(botID, map[string]any{}),
		session: sqlc.BotSession{
			ID:    testUUID(sessionID),
			BotID: testUUID(botID),
			Type:  session.TypeACPAgent,
			Title: "Codex",
			Metadata: testJSON(map[string]any{
				"acp_agent_id": acpprofile.AgentCodexID,
				"project_path": "/data/app",
			}),
		},
	}
	handler := NewACPRuntimeHandler(
		nil,
		session.NewService(nil, queries),
		bots.NewService(nil, queries),
		newTestAdminAccountService("admin"),
	)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/bots/"+botID+"/sessions/"+sessionID+"/acp-runtime", nil)
	rec := httptest.NewRecorder()
	ctx := testAuthContext(e, req, rec, "user-1")
	ctx.SetPath("/bots/:bot_id/sessions/:session_id/acp-runtime")
	ctx.SetParamNames("bot_id", "session_id")
	ctx.SetParamValues(botID, sessionID)

	if err := handler.GetRuntime(ctx); err != nil {
		t.Fatalf("GetRuntime() error = %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got["state"] != "idle" {
		t.Fatalf("runtime status = %#v, want idle", got)
	}
	if _, ok := got["status"]; ok {
		t.Fatalf("status field should be dropped from response, got %#v", got)
	}
	if _, ok := got["turn_status"]; ok {
		t.Fatalf("turn_status field should be dropped from response, got %#v", got)
	}
	if got["agent_id"] != acpprofile.AgentCodexID || got["project_path"] != "/data/app" {
		t.Fatalf("runtime metadata = %#v", got)
	}
}

func TestACPRuntimeHandlerEnsureStartsRuntimeAndReturnsModels(t *testing.T) {
	botID := "11111111-1111-1111-1111-111111111111"
	sessionID := "44444444-4444-4444-4444-444444444444"
	queries := acpRuntimeQueries{
		bot: testBotRow(botID, map[string]any{}),
		session: sqlc.BotSession{
			ID:    testUUID(sessionID),
			BotID: testUUID(botID),
			Type:  session.TypeACPAgent,
			Title: "Codex",
			Metadata: testJSON(map[string]any{
				"acp_agent_id": acpprofile.AgentCodexID,
				"project_path": "/data/app",
			}),
		},
	}
	pool := &fakeACPRuntimePool{
		status: acpagent.RuntimeStatus{
			SessionID:   sessionID,
			AgentID:     acpprofile.AgentCodexID,
			ProjectPath: "/data/app",
			State:       "idle",
			ACPSession:  "acp-session-1",
			Models: &acpclient.ModelState{
				Supported:      true,
				CurrentModelID: "gpt-5.1-codex",
				Available: []acpclient.ModelInfo{{
					ID:   "gpt-5.1-codex",
					Name: "GPT-5.1 Codex",
				}},
			},
		},
	}
	handler := newACPRuntimeHandler(
		pool,
		session.NewService(nil, queries),
		bots.NewService(nil, queries),
		newTestAdminAccountService("admin"),
	)

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/bots/"+botID+"/sessions/"+sessionID+"/acp-runtime", nil)
	req.Header.Set("Authorization", "Bearer token-1")
	rec := httptest.NewRecorder()
	ctx := testAuthContext(e, req, rec, "user-1")
	ctx.SetPath("/bots/:bot_id/sessions/:session_id/acp-runtime")
	ctx.SetParamNames("bot_id", "session_id")
	ctx.SetParamValues(botID, sessionID)

	if err := handler.EnsureRuntime(ctx); err != nil {
		t.Fatalf("EnsureRuntime() error = %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if pool.ensureInput.BotID != botID || pool.ensureInput.SessionID != sessionID || pool.ensureInput.AgentID != acpprofile.AgentCodexID || pool.ensureInput.ProjectPath != "/data/app" {
		t.Fatalf("Ensure input = %#v", pool.ensureInput)
	}
	if pool.ensureInput.SessionToken != "token-1" || pool.ensureInput.ToolHTTPURL != "http://example.com/bots/"+botID+"/tools" {
		t.Fatalf("Ensure tool context = %#v", pool.ensureInput)
	}
	var got acpagent.RuntimeStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.ACPSession != "acp-session-1" || got.Models == nil || !got.Models.Supported || got.Models.CurrentModelID != "gpt-5.1-codex" {
		t.Fatalf("EnsureRuntime response = %#v", got)
	}
	if len(got.Models.Available) != 1 || got.Models.Available[0].ID != "gpt-5.1-codex" {
		t.Fatalf("EnsureRuntime models = %#v", got.Models)
	}
}

func TestACPRuntimeHandlerSetModel(t *testing.T) {
	botID := "11111111-1111-1111-1111-111111111111"
	sessionID := "55555555-5555-5555-5555-555555555555"
	queries := acpRuntimeQueries{
		bot: testBotRow(botID, map[string]any{}),
		session: sqlc.BotSession{
			ID:    testUUID(sessionID),
			BotID: testUUID(botID),
			Type:  session.TypeACPAgent,
			Title: "Codex",
			Metadata: testJSON(map[string]any{
				"acp_agent_id": acpprofile.AgentCodexID,
				"project_path": "/data/app",
			}),
		},
	}
	pool := &fakeACPRuntimePool{
		status: acpagent.RuntimeStatus{
			SessionID:   sessionID,
			AgentID:     acpprofile.AgentCodexID,
			ProjectPath: "/data/app",
			State:       "idle",
			ACPSession:  "acp-session-1",
			Models: &acpclient.ModelState{
				Supported:      true,
				CurrentModelID: "gpt-5.1-codex-high",
				Available: []acpclient.ModelInfo{{
					ID:   "gpt-5.1-codex-high",
					Name: "GPT-5.1 Codex High",
				}},
			},
		},
	}
	handler := newACPRuntimeHandler(
		pool,
		session.NewService(nil, queries),
		bots.NewService(nil, queries),
		newTestAdminAccountService("admin"),
	)

	e := echo.New()
	req := httptest.NewRequest(
		http.MethodPatch,
		"/bots/"+botID+"/sessions/"+sessionID+"/acp-runtime/model",
		bytes.NewBufferString(`{"model_id":"gpt-5.1-codex-high"}`),
	)
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	req.Header.Set("Authorization", "Bearer token-2")
	rec := httptest.NewRecorder()
	ctx := testAuthContext(e, req, rec, "user-1")
	ctx.SetPath("/bots/:bot_id/sessions/:session_id/acp-runtime/model")
	ctx.SetParamNames("bot_id", "session_id")
	ctx.SetParamValues(botID, sessionID)

	if err := handler.SetModel(ctx); err != nil {
		t.Fatalf("SetModel() error = %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if pool.setModelInput.BotID != botID || pool.setModelInput.SessionID != sessionID || pool.setModelInput.AgentID != acpprofile.AgentCodexID || pool.setModelInput.ProjectPath != "/data/app" {
		t.Fatalf("SetModel input = %#v", pool.setModelInput)
	}
	if pool.setModelInput.SessionToken != "token-2" || pool.setModelInput.ToolHTTPURL != "http://example.com/bots/"+botID+"/tools" {
		t.Fatalf("SetModel tool context = %#v", pool.setModelInput)
	}
	if pool.setModelID != "gpt-5.1-codex-high" {
		t.Fatalf("SetModel model id = %q", pool.setModelID)
	}
	var got acpagent.RuntimeStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Models == nil || got.Models.CurrentModelID != "gpt-5.1-codex-high" {
		t.Fatalf("SetModel response = %#v", got)
	}
}

func TestACPRuntimeHandlerRejectsNonACPSession(t *testing.T) {
	botID := "11111111-1111-1111-1111-111111111111"
	sessionID := "33333333-3333-3333-3333-333333333333"
	queries := acpRuntimeQueries{
		bot: testBotRow(botID, map[string]any{}),
		session: sqlc.BotSession{
			ID:       testUUID(sessionID),
			BotID:    testUUID(botID),
			Type:     session.TypeChat,
			Title:    "Chat",
			Metadata: testJSON(map[string]any{}),
		},
	}
	handler := NewACPRuntimeHandler(
		nil,
		session.NewService(nil, queries),
		bots.NewService(nil, queries),
		newTestAdminAccountService("admin"),
	)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/bots/"+botID+"/sessions/"+sessionID+"/acp-runtime", nil)
	rec := httptest.NewRecorder()
	ctx := testAuthContext(e, req, rec, "user-1")
	ctx.SetPath("/bots/:bot_id/sessions/:session_id/acp-runtime")
	ctx.SetParamNames("bot_id", "session_id")
	ctx.SetParamValues(botID, sessionID)

	err := handler.GetRuntime(ctx)
	if err == nil {
		t.Fatalf("GetRuntime() error = nil, want HTTP 400")
	}
	var httpErr *echo.HTTPError
	if !errors.As(err, &httpErr) || httpErr.Code != http.StatusBadRequest {
		t.Fatalf("GetRuntime() error = %v, want HTTP 400", err)
	}
}

func testBotRow(botID string, metadata map[string]any) sqlc.GetBotByIDRow {
	return sqlc.GetBotByIDRow{
		ID:          testUUID(botID),
		OwnerUserID: testUUID("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"),
		DisplayName: pgtype.Text{
			String: "bot",
			Valid:  true,
		},
		IsActive:  true,
		Status:    bots.BotStatusCreating,
		Metadata:  testJSON(metadata),
		CreatedAt: pgtype.Timestamptz{Valid: true},
		UpdatedAt: pgtype.Timestamptz{Valid: true},
	}
}
