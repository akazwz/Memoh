package toolapproval

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/memohai/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/memohai/memoh/internal/db/store"
)

type lifecycleQueries struct {
	dbstore.Queries
	mu         sync.Mutex
	cancelArgs []sqlc.CancelPendingToolApprovalsBySessionParams
	expireArgs []sqlc.ExpireStaleToolApprovalsParams
}

func (q *lifecycleQueries) CancelPendingToolApprovalsBySession(_ context.Context, arg sqlc.CancelPendingToolApprovalsBySessionParams) ([]sqlc.ToolApprovalRequest, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.cancelArgs = append(q.cancelArgs, arg)
	return []sqlc.ToolApprovalRequest{{Status: StatusCancelled}}, nil
}

func (q *lifecycleQueries) ExpireStaleToolApprovals(_ context.Context, arg sqlc.ExpireStaleToolApprovalsParams) ([]sqlc.ToolApprovalRequest, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.expireArgs = append(q.expireArgs, arg)
	return []sqlc.ToolApprovalRequest{{Status: StatusExpired}}, nil
}

func TestCancelPendingForSession(t *testing.T) {
	t.Parallel()

	queries := &lifecycleQueries{}
	svc := NewService(slog.New(slog.DiscardHandler), queries, nil)

	botID := "11111111-1111-1111-1111-111111111111"
	sessionID := "22222222-2222-2222-2222-222222222222"
	cancelled, err := svc.CancelPendingForSession(context.Background(), botID, sessionID, "")
	if err != nil {
		t.Fatalf("CancelPendingForSession() error = %v", err)
	}
	if len(cancelled) != 1 || cancelled[0].Status != StatusCancelled {
		t.Fatalf("cancelled = %#v, want one cancelled request", cancelled)
	}
	if len(queries.cancelArgs) != 1 {
		t.Fatalf("cancel query calls = %d, want 1", len(queries.cancelArgs))
	}
	if queries.cancelArgs[0].Reason == "" {
		t.Fatal("empty reason was not defaulted")
	}

	// Non-UUID identifiers fail loudly instead of issuing a broken query.
	if _, err := svc.CancelPendingForSession(context.Background(), botID, "not-a-uuid", "r"); err == nil {
		t.Fatal("CancelPendingForSession accepted a malformed session id")
	}
}

func TestExpireStalePending(t *testing.T) {
	t.Parallel()

	queries := &lifecycleQueries{}
	svc := NewService(slog.New(slog.DiscardHandler), queries, nil)

	before := time.Now().Add(-(DefaultWaitTimeout + expirySweepGrace))
	expired, err := svc.ExpireStalePending(context.Background(), before)
	if err != nil {
		t.Fatalf("ExpireStalePending() error = %v", err)
	}
	if len(expired) != 1 || expired[0].Status != StatusExpired {
		t.Fatalf("expired = %#v, want one expired request", expired)
	}
	if len(queries.expireArgs) != 1 {
		t.Fatalf("expire query calls = %d, want 1", len(queries.expireArgs))
	}
	arg := queries.expireArgs[0]
	// The cutoff is truncated to whole seconds so the sqlite adapter's
	// RFC3339Nano rendering stays parseable by datetime().
	wantCutoff := before.UTC().Truncate(time.Second)
	if !arg.CreatedAt.Valid || !arg.CreatedAt.Time.Equal(wantCutoff) {
		t.Fatalf("expire cutoff = %#v, want %v", arg.CreatedAt, wantCutoff)
	}
	if arg.Reason == "" {
		t.Fatal("expire reason is empty")
	}
}
