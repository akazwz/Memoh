package toolapproval

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/memohai/memoh/internal/db"
	"github.com/memohai/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/memohai/memoh/internal/db/store"
	"github.com/memohai/memoh/internal/settings"
)

type Service struct {
	queries  dbstore.Queries
	settings *settings.Service
	logger   *slog.Logger
}

func NewService(log *slog.Logger, queries dbstore.Queries, settings *settings.Service) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		queries:  queries,
		settings: settings,
		logger:   log.With(slog.String("service", "toolapproval")),
	}
}

func (s *Service) Evaluate(ctx context.Context, input CreatePendingInput) (Evaluation, error) {
	eval, err := s.EvaluatePolicy(ctx, input)
	if err != nil || eval.Decision == DecisionBypass {
		return eval, err
	}
	req, err := s.CreatePending(ctx, input)
	if err != nil {
		return Evaluation{}, err
	}
	return Evaluation{Decision: DecisionNeedsApproval, Request: req}, nil
}

func (s *Service) EvaluatePolicy(ctx context.Context, input CreatePendingInput) (Evaluation, error) {
	if s == nil || s.settings == nil {
		return Evaluation{Decision: DecisionBypass}, nil
	}
	botSettings, err := s.settings.GetBot(ctx, input.BotID)
	if err != nil {
		return Evaluation{}, err
	}
	if !needsApproval(botSettings.ToolApprovalConfig, input.ToolName, input.ToolInput) {
		return Evaluation{Decision: DecisionBypass}, nil
	}
	return Evaluation{Decision: DecisionNeedsApproval}, nil
}

func (s *Service) CreatePending(ctx context.Context, input CreatePendingInput) (Request, error) {
	if s == nil || s.queries == nil {
		return Request{}, errors.New("tool approval queries not configured")
	}
	botID, err := db.ParseUUID(input.BotID)
	if err != nil {
		return Request{}, err
	}
	sessionID, err := db.ParseUUID(input.SessionID)
	if err != nil {
		return Request{}, err
	}
	toolInput, err := json.Marshal(input.ToolInput)
	if err != nil {
		return Request{}, err
	}
	channelIdentityID, err := s.optionalChannelIdentityUUID(ctx, input.ChannelIdentityID)
	if err != nil {
		return Request{}, err
	}
	requestedByID, err := s.optionalChannelIdentityUUID(ctx, input.RequestedByChannelIdentityID)
	if err != nil {
		return Request{}, err
	}
	// A re-used (session_id, tool_call_id) is a NEW ask: drop any prior row
	// first so the fresh request gets a brand-new id and short_id. Reusing the
	// row (an upsert) would let a stale approval card or reply — addressed by
	// the old id — land a decision on the new request. Deleting is safe: the
	// old request's waiter has already unwound (the agent only re-asks after
	// the previous attempt resolved or the turn restarted).
	//
	// The DELETE+INSERT is intentionally not wrapped in a transaction. It is
	// safe because the swap is serialized per (session_id, tool_call_id): a
	// session runs one turn at a time (the pool's turn slot), and an ACP/native
	// agent asks permission synchronously — it never has two in-flight asks for
	// the same tool_call_id — so two concurrent CreatePending for the same key
	// cannot occur. (If that invariant ever weakens, wrap both statements in a
	// single tx as defense-in-depth against the lost-row window / UNIQUE race.)
	if err := s.queries.DeleteToolApprovalRequestsBySessionToolCall(ctx, sqlc.DeleteToolApprovalRequestsBySessionToolCallParams{
		BotID:      botID,
		SessionID:  sessionID,
		ToolCallID: strings.TrimSpace(input.ToolCallID),
	}); err != nil {
		return Request{}, err
	}
	row, err := s.queries.CreateToolApprovalRequest(ctx, sqlc.CreateToolApprovalRequestParams{
		BotID:                        botID,
		SessionID:                    sessionID,
		RouteID:                      optionalUUID(input.RouteID),
		ChannelIdentityID:            channelIdentityID,
		ToolCallID:                   strings.TrimSpace(input.ToolCallID),
		ToolName:                     strings.TrimSpace(input.ToolName),
		ToolInput:                    toolInput,
		RequestedByChannelIdentityID: requestedByID,
		RequestedMessageID:           optionalUUID(input.RequestedMessageID),
		SourcePlatform:               strings.TrimSpace(input.SourcePlatform),
		ReplyTarget:                  strings.TrimSpace(input.ReplyTarget),
		ConversationType:             strings.TrimSpace(input.ConversationType),
	})
	if err != nil {
		return Request{}, err
	}
	return requestFromRow(row), nil
}

func (s *Service) ResolveTarget(ctx context.Context, input ResolveInput) (Request, error) {
	botID, err := db.ParseUUID(input.BotID)
	if err != nil {
		return Request{}, err
	}
	explicit := strings.TrimSpace(input.ExplicitID)
	if strings.TrimSpace(input.SessionID) == "" && explicit != "" {
		if parsed, err := db.ParseUUID(explicit); err == nil {
			row, err := s.queries.GetToolApprovalRequest(ctx, parsed)
			if err != nil {
				return Request{}, mapLookupErr(err)
			}
			req := requestFromRow(row)
			if req.BotID != uuid.UUID(botID.Bytes).String() || req.Status != StatusPending {
				return Request{}, ErrNotFound
			}
			return req, nil
		}
		return Request{}, ErrNotFound
	}
	sessionID, err := db.ParseUUID(input.SessionID)
	if err != nil {
		return Request{}, err
	}
	if explicit != "" {
		if shortID, err := strconv.Atoi(explicit); err == nil {
			row, err := s.queries.GetPendingToolApprovalBySessionShortID(ctx, sqlc.GetPendingToolApprovalBySessionShortIDParams{
				BotID:     botID,
				SessionID: sessionID,
				ShortID:   int32(shortID), //nolint:gosec // user-facing approval numbers are small positive integers.
			})
			return requestFromRowOrErr(row, err)
		}
		if parsed, err := db.ParseUUID(explicit); err == nil {
			row, err := s.queries.GetToolApprovalRequest(ctx, parsed)
			if err != nil {
				return Request{}, mapLookupErr(err)
			}
			req := requestFromRow(row)
			if req.BotID != uuid.UUID(botID.Bytes).String() || req.SessionID != uuid.UUID(sessionID.Bytes).String() || req.Status != StatusPending {
				return Request{}, ErrNotFound
			}
			return req, nil
		}
		return Request{}, ErrNotFound
	}
	if replyID := strings.TrimSpace(input.ReplyExternalMessageID); replyID != "" {
		row, err := s.queries.GetPendingToolApprovalByReplyMessage(ctx, sqlc.GetPendingToolApprovalByReplyMessageParams{
			BotID:                   botID,
			SessionID:               sessionID,
			PromptExternalMessageID: replyID,
		})
		if err == nil {
			return requestFromRow(row), nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return Request{}, err
		}
	}
	row, err := s.queries.GetLatestPendingToolApprovalBySession(ctx, sqlc.GetLatestPendingToolApprovalBySessionParams{
		BotID:     botID,
		SessionID: sessionID,
	})
	return requestFromRowOrErr(row, err)
}

func (s *Service) Approve(ctx context.Context, approvalID, actorID, reason string) (Request, error) {
	id, err := db.ParseUUID(approvalID)
	if err != nil {
		return Request{}, err
	}
	decidedBy, err := s.optionalChannelIdentityUUID(ctx, actorID)
	if err != nil {
		return Request{}, err
	}
	row, err := s.queries.ApproveToolApprovalRequest(ctx, sqlc.ApproveToolApprovalRequestParams{
		ID:                         id,
		Reason:                     strings.TrimSpace(reason),
		DecidedByChannelIdentityID: decidedBy,
	})
	return requestFromRowOrErr(row, err)
}

func (s *Service) Reject(ctx context.Context, approvalID, actorID, reason string) (Request, error) {
	id, err := db.ParseUUID(approvalID)
	if err != nil {
		return Request{}, err
	}
	decidedBy, err := s.optionalChannelIdentityUUID(ctx, actorID)
	if err != nil {
		return Request{}, err
	}
	row, err := s.queries.RejectToolApprovalRequest(ctx, sqlc.RejectToolApprovalRequestParams{
		ID:                         id,
		Reason:                     strings.TrimSpace(reason),
		DecidedByChannelIdentityID: decidedBy,
	})
	return requestFromRowOrErr(row, err)
}

func (s *Service) Get(ctx context.Context, approvalID string) (Request, error) {
	if s == nil || s.queries == nil {
		return Request{}, errors.New("tool approval queries not configured")
	}
	id, err := db.ParseUUID(approvalID)
	if err != nil {
		return Request{}, err
	}
	row, err := s.queries.GetToolApprovalRequest(ctx, id)
	return requestFromRowOrErr(row, err)
}

func (s *Service) WaitForDecision(ctx context.Context, approvalID string) (Request, error) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		req, err := s.Get(ctx, approvalID)
		if err != nil {
			return Request{}, err
		}
		if req.Status != StatusPending {
			return req, nil
		}
		select {
		case <-ctx.Done():
			return Request{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *Service) UpdatePromptMessage(ctx context.Context, approvalID, promptMessageID, externalID string) (Request, error) {
	id, err := db.ParseUUID(approvalID)
	if err != nil {
		return Request{}, err
	}
	row, err := s.queries.UpdateToolApprovalPromptMessage(ctx, sqlc.UpdateToolApprovalPromptMessageParams{
		ID:                      id,
		PromptMessageID:         optionalUUID(promptMessageID),
		PromptExternalMessageID: strings.TrimSpace(externalID),
	})
	return requestFromRowOrErr(row, err)
}

// CancelPendingForSession cancels every pending approval for the session as
// a system outcome. The session-pool turn slot guarantees at most one turn in
// flight per session, so a session's pending approvals always belong to the
// turn that just ended — calling this when a turn aborts or fails closes the
// residual window where a dead waiter leaves a permanently actionable card.
func (s *Service) CancelPendingForSession(ctx context.Context, botID, sessionID, reason string) ([]Request, error) {
	if s == nil || s.queries == nil {
		return nil, errors.New("tool approval queries not configured")
	}
	botUUID, err := db.ParseUUID(botID)
	if err != nil {
		return nil, err
	}
	sessionUUID, err := db.ParseUUID(sessionID)
	if err != nil {
		return nil, err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "tool approval cancelled: the turn that requested it ended"
	}
	rows, err := s.queries.CancelPendingToolApprovalsBySession(ctx, sqlc.CancelPendingToolApprovalsBySessionParams{
		BotID:     botUUID,
		SessionID: sessionUUID,
		Reason:    reason,
	})
	if err != nil {
		return nil, err
	}
	out := make([]Request, 0, len(rows))
	for _, row := range rows {
		out = append(out, requestFromRow(row))
	}
	return out, nil
}

// expirySweepGrace pads the flow's wait window before a pending row is
// declared orphaned: any live waiter resolves (or timeout-rejects) its row
// within DefaultWaitTimeout, so a pending row older than the window plus
// grace provably has no waiter — its server died or its runtime is gone.
const expirySweepGrace = 5 * time.Minute

// ExpireStalePending expires pending approvals created before the cutoff.
func (s *Service) ExpireStalePending(ctx context.Context, before time.Time) ([]Request, error) {
	if s == nil || s.queries == nil {
		return nil, errors.New("tool approval queries not configured")
	}
	rows, err := s.queries.ExpireStaleToolApprovals(ctx, sqlc.ExpireStaleToolApprovalsParams{
		Reason: "tool approval expired: no live turn to serve it",
		// Whole-second cutoff: the sqlite adapter renders timestamps as
		// RFC3339Nano and the query normalizes via datetime(), which does not
		// parse arbitrary fractional precision.
		CreatedAt: pgtype.Timestamptz{Time: before.UTC().Truncate(time.Second), Valid: true},
	})
	if err != nil {
		return nil, err
	}
	out := make([]Request, 0, len(rows))
	for _, row := range rows {
		out = append(out, requestFromRow(row))
	}
	return out, nil
}

// StartExpirySweeper periodically expires orphaned pending approvals (waiter
// died with its server or runtime). Without it a pending row survives
// forever and its persisted card stays actionable — approving it later flips
// a row nobody executes.
func (s *Service) StartExpirySweeper(ctx context.Context) {
	if s == nil {
		return
	}
	ticker := time.NewTicker(time.Minute)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				expired, err := s.ExpireStalePending(ctx, time.Now().Add(-(DefaultWaitTimeout + expirySweepGrace)))
				if err != nil {
					s.logger.Warn("tool approval expiry sweep failed", slog.Any("error", err))
					continue
				}
				if len(expired) > 0 {
					s.logger.Info("expired orphaned tool approvals", slog.Int("count", len(expired)))
				}
			case <-ctx.Done():
				return
			}
		}
	}()
}

func (s *Service) ListPendingBySession(ctx context.Context, botID, sessionID string) ([]Request, error) {
	return s.listBySession(ctx, botID, sessionID, true)
}

func (s *Service) ListBySession(ctx context.Context, botID, sessionID string) ([]Request, error) {
	return s.listBySession(ctx, botID, sessionID, false)
}

func (s *Service) listBySession(ctx context.Context, botID, sessionID string, pendingOnly bool) ([]Request, error) {
	pgBotID, err := db.ParseUUID(botID)
	if err != nil {
		return nil, err
	}
	pgSessionID, err := db.ParseUUID(sessionID)
	if err != nil {
		return nil, err
	}
	var rows []sqlc.ToolApprovalRequest
	if pendingOnly {
		rows, err = s.queries.ListPendingToolApprovalsBySession(ctx, sqlc.ListPendingToolApprovalsBySessionParams{
			BotID:     pgBotID,
			SessionID: pgSessionID,
		})
	} else {
		rows, err = s.queries.ListToolApprovalsBySession(ctx, sqlc.ListToolApprovalsBySessionParams{
			BotID:     pgBotID,
			SessionID: pgSessionID,
		})
	}
	if err != nil {
		return nil, err
	}
	result := make([]Request, 0, len(rows))
	for _, row := range rows {
		result = append(result, requestFromRow(row))
	}
	return result, nil
}

func requestFromRowOrErr(row sqlc.ToolApprovalRequest, err error) (Request, error) {
	if err != nil {
		return Request{}, mapLookupErr(err)
	}
	return requestFromRow(row), nil
}

func mapLookupErr(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func optionalUUID(value string) pgtype.UUID {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return pgtype.UUID{}
	}
	parsed, err := db.ParseUUID(trimmed)
	if err != nil {
		return pgtype.UUID{}
	}
	return parsed
}

func (s *Service) optionalChannelIdentityUUID(ctx context.Context, value string) (pgtype.UUID, error) {
	id := optionalUUID(value)
	if !id.Valid {
		return pgtype.UUID{}, nil
	}
	if s == nil || s.queries == nil {
		return pgtype.UUID{}, nil
	}
	if _, err := s.queries.GetChannelIdentityByID(ctx, id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return pgtype.UUID{}, nil
		}
		return pgtype.UUID{}, err
	}
	return id, nil
}

func requestFromRow(row sqlc.ToolApprovalRequest) Request {
	var input map[string]any
	_ = json.Unmarshal(row.ToolInput, &input)
	req := Request{
		ID:                      uuid.UUID(row.ID.Bytes).String(),
		BotID:                   uuid.UUID(row.BotID.Bytes).String(),
		SessionID:               uuid.UUID(row.SessionID.Bytes).String(),
		ToolCallID:              strings.TrimSpace(row.ToolCallID),
		ToolName:                strings.TrimSpace(row.ToolName),
		ToolInput:               input,
		ShortID:                 int(row.ShortID),
		Status:                  strings.TrimSpace(row.Status),
		DecisionReason:          strings.TrimSpace(row.DecisionReason),
		PromptExternalMessageID: strings.TrimSpace(row.PromptExternalMessageID),
		SourcePlatform:          strings.TrimSpace(row.SourcePlatform),
		ReplyTarget:             strings.TrimSpace(row.ReplyTarget),
		ConversationType:        strings.TrimSpace(row.ConversationType),
		CreatedAt:               row.CreatedAt.Time,
	}
	if row.RouteID.Valid {
		req.RouteID = uuid.UUID(row.RouteID.Bytes).String()
	}
	if row.ChannelIdentityID.Valid {
		req.ChannelIdentityID = uuid.UUID(row.ChannelIdentityID.Bytes).String()
	}
	if row.DecidedAt.Valid {
		decided := row.DecidedAt.Time
		req.DecidedAt = &decided
	}
	return req
}
