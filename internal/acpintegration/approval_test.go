package acpintegration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/memohai/memoh/internal/toolapproval"
)

// TestApprovalRoundTripThroughRealDB exercises toolapproval.RunFlow against a
// real migrated SQLite DB: a forced approval is created (pending row lands in
// the DB), a concurrent user decision unblocks the wait, and the flow reports
// the decision as user-made. This is the path ACP generic permissions take;
// faking the service would hide the DB write/read seam.
func TestApprovalRoundTripThroughRealDB(t *testing.T) {
	queries, sqlDB := newMigratedSQLite(t)
	seedBotSession(t, sqlDB)
	svc := toolapproval.NewService(nil, queries, nil)
	ctx := context.Background()

	for _, tc := range []struct {
		name        string
		decide      func(t *testing.T, approvalID string)
		wantApprove bool
		wantStatus  string
	}{
		{
			name: "user approves",
			decide: func(t *testing.T, approvalID string) {
				if _, err := svc.Approve(ctx, approvalID, testChannelID, "looks fine"); err != nil {
					t.Errorf("Approve: %v", err)
				}
			},
			wantApprove: true,
			wantStatus:  toolapproval.StatusApproved,
		},
		{
			name: "user rejects",
			decide: func(t *testing.T, approvalID string) {
				if _, err := svc.Reject(ctx, approvalID, testChannelID, "no"); err != nil {
					t.Errorf("Reject: %v", err)
				}
			},
			wantApprove: false,
			wantStatus:  toolapproval.StatusRejected,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			emitted := make(chan toolapproval.Request, 4)
			resultCh := make(chan toolapproval.FlowResult, 1)
			errCh := make(chan error, 1)
			go func() {
				result, err := toolapproval.RunFlow(ctx, svc, toolapproval.FlowRequest{
					Input: toolapproval.CreatePendingInput{
						BotID:                        testBotID,
						SessionID:                    testSessionID,
						ChannelIdentityID:            testChannelID,
						RequestedByChannelIdentityID: testChannelID,
						ToolCallID:                   "call-" + tc.name,
						ToolName:                     "exec",
						ToolInput:                    map[string]any{"command": "rm -rf build"},
					},
					Interactive:   true,
					ForceApproval: true,
					Emit:          func(req toolapproval.Request) { emitted <- req },
				})
				if err != nil {
					errCh <- err
					return
				}
				resultCh <- result
			}()

			// The pending snapshot must be emitted and a pending row must
			// exist in the DB before anyone decides.
			var pending toolapproval.Request
			select {
			case pending = <-emitted:
			case err := <-errCh:
				t.Fatalf("RunFlow errored before pending: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("no pending approval emitted")
			}
			if toolapproval.NormalizedStatus(pending.Status) != toolapproval.StatusPending {
				t.Fatalf("first emit status = %q, want pending", pending.Status)
			}
			stored, err := svc.Get(ctx, pending.ID)
			if err != nil {
				t.Fatalf("Get pending: %v", err)
			}
			if stored.Status != toolapproval.StatusPending {
				t.Fatalf("stored status = %q, want pending", stored.Status)
			}

			tc.decide(t, pending.ID)

			select {
			case result := <-resultCh:
				if result.Approved != tc.wantApprove {
					t.Fatalf("Approved = %v, want %v", result.Approved, tc.wantApprove)
				}
				if result.Status != tc.wantStatus {
					t.Fatalf("Status = %q, want %q", result.Status, tc.wantStatus)
				}
				if !result.DecidedByUser {
					t.Fatal("DecidedByUser = false, want true for a live user decision")
				}
			case err := <-errCh:
				t.Fatalf("RunFlow: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("RunFlow did not return after decision")
			}
		})
	}
}

// TestApprovalUpsertDoesNotReplayDecision is the cross-service guard for the
// reused tool_call_id: through the real service + real DB, a re-created
// approval for the same (session, tool_call_id) must come back pending — the
// previous approval must never authorize the new request.
func TestApprovalUpsertDoesNotReplayDecision(t *testing.T) {
	queries, sqlDB := newMigratedSQLite(t)
	seedBotSession(t, sqlDB)
	svc := toolapproval.NewService(nil, queries, nil)
	ctx := context.Background()

	input := toolapproval.CreatePendingInput{
		BotID:                        testBotID,
		SessionID:                    testSessionID,
		ChannelIdentityID:            testChannelID,
		RequestedByChannelIdentityID: testChannelID,
		ToolCallID:                   "call-reused",
		ToolName:                     "exec",
		ToolInput:                    map[string]any{"command": "rm build"},
	}
	created, err := svc.CreatePending(ctx, input)
	if err != nil {
		t.Fatalf("CreatePending: %v", err)
	}
	if _, err := svc.Approve(ctx, created.ID, testChannelID, "ok"); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	// The agent asks again under the same tool_call_id with a different (more
	// dangerous) command. This is a NEW ask: a brand-new row with a new id,
	// pending — and the OLD id must no longer be addressable, so a stale
	// approval card cannot land a decision on the new request.
	input.ToolInput = map[string]any{"command": "rm -rf /"}
	recreated, err := svc.CreatePending(ctx, input)
	if err != nil {
		t.Fatalf("CreatePending (reuse): %v", err)
	}
	if recreated.ID == created.ID {
		t.Fatal("re-created approval reused the old row id; a stale card could approve the new request")
	}
	if recreated.Status != toolapproval.StatusPending {
		t.Fatalf("re-created approval status = %q, want pending (the old decision must not replay)", recreated.Status)
	}
	if !toolapproval.CanApprove(recreated.Status) {
		t.Fatal("re-created approval is not actionable")
	}
	// The old approval id is gone — a stale card addressing it resolves to
	// nothing, instead of the freshly-pending row.
	if _, err := svc.Get(ctx, created.ID); !errors.Is(err, toolapproval.ErrNotFound) {
		t.Fatalf("old approval id still resolves (err=%v); the stale card must miss", err)
	}

	// WaitForDecision on the fresh row must block (it is genuinely pending),
	// not return the stale approval immediately.
	waitCtx, cancel := context.WithTimeout(ctx, 400*time.Millisecond)
	defer cancel()
	if _, err := svc.WaitForDecision(waitCtx, recreated.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitForDecision on fresh row = %v, want it to block (DeadlineExceeded)", err)
	}
}
