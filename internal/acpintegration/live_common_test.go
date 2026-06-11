package acpintegration

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/memohai/memoh/internal/acpagent"
	"github.com/memohai/memoh/internal/acpclient"
)

// livePromptOutcome is the result of a prompt run on a background goroutine in
// the live reject/abort tests.
type livePromptOutcome struct {
	result acpclient.PromptResult
	err    error
}

// rejectFirstPendingAndAssertClean rejects the first pending approval and
// asserts the turn finishes as a CLEAN refusal — no error, and the gated file
// never lands. This is the symmetric live counterpart of the L1
// reject-keeps-turn-alive scenario: it proves a real agent treats a denial as a
// refusal it can recover from, not a crash, AND that the gate actually blocked
// the side effect. Shared by the codex and claude reject tests.
func rejectFirstPendingAndAssertClean(t *testing.T, sp *scenarioPool, root, fileName string, done <-chan livePromptOutcome) {
	t.Helper()
	rejected := false
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		pending, err := sp.approval.ListPendingBySession(context.Background(), sp.botID, sp.sessionID)
		if err != nil {
			t.Fatalf("ListPendingBySession: %v", err)
		}
		if len(pending) > 0 {
			if _, err := sp.approval.Reject(context.Background(), pending[0].ID, testChannelID, "denied by test"); err != nil {
				t.Fatalf("Reject: %v", err)
			}
			rejected = true
			break
		}
		select {
		case got := <-done:
			if isExternalQuotaError(got.err) {
				t.Skipf("live reject test skipped (external quota): %v", got.err)
			}
			t.Fatalf("turn finished before any approval appeared (agent did not use the gated tool); err=%v text=%q", got.err, got.result.Text)
		default:
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !rejected {
		t.Fatal("no pending approval appeared to reject")
	}

	var got livePromptOutcome
	select {
	case got = <-done:
	case <-time.After(90 * time.Second):
		t.Fatal("turn did not finish after rejection")
	}
	if got.err != nil {
		if isExternalQuotaError(got.err) {
			t.Skipf("live reject test skipped (external quota): %v", got.err)
		}
		t.Fatalf("rejection killed the turn (a denial should be a clean refusal): %v", got.err)
	}
	if found := findFileUnder(root, fileName); found != "" {
		t.Fatalf("rejected action still produced %s — the gate did not actually block the side effect", found)
	}
	t.Logf("gated action was rejected; turn completed cleanly with no file: %q", got.result.Text)
}

// abortInFlightAndAssertWarm waits for the agent to park on a pending approval
// (turn definitively in flight), aborts the turn by id, asserts the prompt
// unwinds as cancelled, and asserts the SAME warm runtime serves the next
// prompt. Symmetric live counterpart of the L1 abort-keeps-runtime-warm
// scenario — proving a real agent process survives an out-of-band abort and is
// reused, not wedged. Shared by the codex and claude abort tests.
func abortInFlightAndAssertWarm(t *testing.T, sp *scenarioPool, agentID string, done <-chan livePromptOutcome) {
	t.Helper()
	parked := false
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		pending, err := sp.approval.ListPendingBySession(context.Background(), sp.botID, sp.sessionID)
		if err != nil {
			t.Fatalf("ListPendingBySession: %v", err)
		}
		if len(pending) > 0 {
			parked = true
			break
		}
		select {
		case got := <-done:
			if isExternalQuotaError(got.err) {
				t.Skipf("live abort test skipped (external quota): %v", got.err)
			}
			t.Fatalf("turn finished before parking on an approval (agent did not use the gated tool); err=%v text=%q", got.err, got.result.Text)
		default:
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !parked {
		t.Fatal("agent never parked on a pending approval to abort")
	}

	status := sp.pool.RuntimeStatus(sp.sessionID, agentID, "/data/project")
	if status.TurnID == "" {
		t.Fatal("runtime reports no in-flight turn id to abort")
	}
	if _, err := sp.pool.AbortTurn(sp.botID, sp.sessionID, status.TurnID); err != nil {
		t.Fatalf("AbortTurn: %v", err)
	}

	select {
	case got := <-done:
		if isExternalQuotaError(got.err) {
			t.Skipf("live abort test skipped (external quota): %v", got.err)
		}
		if !errors.Is(got.err, acpagent.ErrPromptAborted) {
			t.Fatalf("aborted prompt error = %v, want ErrPromptAborted", got.err)
		}
		if got.result.StopReason != "cancelled" {
			t.Fatalf("stop reason = %q, want cancelled", got.result.StopReason)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("aborted prompt never returned")
	}

	// The warm runtime must still serve the next prompt without a cold start.
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	result, err := sp.prompt(t, ctx, "Reply with exactly this and nothing else: warm-after-abort", &collectSink{})
	if err != nil {
		if isExternalQuotaError(err) {
			t.Skipf("live abort test skipped (external quota): %v", err)
		}
		t.Fatalf("post-abort prompt (runtime should be warm): %v", err)
	}
	if !strings.Contains(strings.ToLower(result.Text), "warm-after-abort") {
		t.Fatalf("post-abort result = %q, want warm-after-abort", result.Text)
	}
	t.Logf("%s turn aborted (cancelled), runtime stayed warm: %q", agentID, result.Text)
}
