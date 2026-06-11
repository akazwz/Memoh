package acpintegration

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/memohai/memoh/internal/acpagent"
	"github.com/memohai/memoh/internal/acpclient"
	"github.com/memohai/memoh/internal/toolapproval"
)

// waitForPendingApproval polls the real DB until a pending approval for the
// session appears (the agent is blocked inside RequestPermission).
func (s *scenarioPool) waitForPendingApproval(t *testing.T) toolapproval.Request {
	t.Helper()
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		pending, err := s.approval.ListPendingBySession(context.Background(), s.botID, s.sessionID)
		if err != nil {
			t.Fatalf("ListPendingBySession: %v", err)
		}
		if len(pending) > 0 {
			return pending[0]
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no pending approval appeared")
	return toolapproval.Request{}
}

// TestACPScenarioApprovalApproveExecutes drives the full approval round-trip
// through the real stack: the scripted agent requests permission (blocking the
// turn), a pending row lands in the real DB, the user approves, and the agent
// proceeds — and the approval event reaches the pool's event sink (the
// canonical channel the persisted round and turn mirror read).
func TestACPScenarioApprovalApproveExecutes(t *testing.T) {
	sp := newScenarioPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sink := &collectSink{}
	type outcome struct {
		result acpclient.PromptResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := sp.prompt(t, ctx, "PERMISSION delete the build dir", sink)
		done <- outcome{result, err}
	}()

	pending := sp.waitForPendingApproval(t)
	if pending.ToolName == "" {
		t.Fatalf("pending approval has no tool name: %#v", pending)
	}
	if _, err := sp.approval.Approve(context.Background(), pending.ID, testChannelID, "ok"); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	var got outcome
	select {
	case got = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("prompt did not finish after approval")
	}
	if got.err != nil {
		t.Fatalf("Prompt: %v", got.err)
	}
	if !strings.Contains(got.result.Text, "[permission-allowed]") {
		t.Fatalf("agent did not see the approval: %q", got.result.Text)
	}
	// The approval surfaced on the canonical event sink as a terminal
	// approved snapshot, not just a hub publish.
	if !hasApprovalEvent(sink.events, toolapproval.StatusApproved) {
		t.Fatalf("approval event missing from pool sink: %#v", sink.events)
	}
}

// TestACPScenarioApprovalRejectKeepsTurnAlive proves a denied permission is a
// clean refusal, not a turn-killer: the agent gets the rejection, streams its
// fallback, and the turn still completes successfully.
func TestACPScenarioApprovalRejectKeepsTurnAlive(t *testing.T) {
	sp := newScenarioPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sink := &collectSink{}
	done := make(chan acpclient.PromptResult, 1)
	errCh := make(chan error, 1)
	go func() {
		result, err := sp.prompt(t, ctx, "PERMISSION rm -rf everything\nSAY recovered after denial", sink)
		if err != nil {
			errCh <- err
			return
		}
		done <- result
	}()

	pending := sp.waitForPendingApproval(t)
	if _, err := sp.approval.Reject(context.Background(), pending.ID, testChannelID, "too risky"); err != nil {
		t.Fatalf("Reject: %v", err)
	}

	select {
	case result := <-done:
		if !strings.Contains(result.Text, "[permission-denied]") {
			t.Fatalf("agent did not see the denial: %q", result.Text)
		}
		if !strings.Contains(result.Text, "recovered after denial") {
			t.Fatalf("turn did not continue past the denial: %q", result.Text)
		}
	case err := <-errCh:
		t.Fatalf("a denied permission killed the turn: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("prompt did not finish after rejection")
	}
}

// TestACPScenarioAbortKeepsRuntimeWarm exercises the lifecycle invariant end
// to end: a hung turn is aborted out-of-band, the prompt unwinds as cancelled,
// and the SAME warm runtime serves the next prompt.
func TestACPScenarioAbortKeepsRuntimeWarm(t *testing.T) {
	sp := newScenarioPool(t)

	type outcome struct {
		result acpclient.PromptResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := sp.prompt(t, context.Background(), "HANG", &collectSink{})
		done <- outcome{result, err}
	}()

	// Wait for the turn to be in flight, then abort it by turn id.
	var status acpagent.RuntimeStatus
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		status = sp.pool.RuntimeStatus(sp.sessionID, "codex", "/data/project")
		if status.TurnID != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if status.TurnID == "" {
		t.Fatal("turn never started")
	}
	if _, err := sp.pool.AbortTurn(sp.botID, sp.sessionID, status.TurnID); err != nil {
		t.Fatalf("AbortTurn: %v", err)
	}

	select {
	case got := <-done:
		if !errors.Is(got.err, acpagent.ErrPromptAborted) {
			t.Fatalf("aborted prompt error = %v, want ErrPromptAborted", got.err)
		}
		if got.result.StopReason != "cancelled" {
			t.Fatalf("stop reason = %q, want cancelled", got.result.StopReason)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("aborted prompt never returned")
	}

	// The warm runtime must still serve the next prompt without a cold start.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	result, err := sp.prompt(t, ctx, "SAY back after abort", &collectSink{})
	if err != nil {
		t.Fatalf("post-abort prompt: %v", err)
	}
	if !strings.Contains(result.Text, "back after abort") {
		t.Fatalf("post-abort result = %q", result.Text)
	}
}

// TestACPScenarioExecRunsThroughBridge proves the terminal client callback
// path works end to end: the scripted agent runs a command, which flows
// through approval (bypassed by default policy), the real terminal manager,
// and the real workspace bridge that actually executes it — then the turn
// completes with the agent's post-exec output.
func TestACPScenarioExecRunsThroughBridge(t *testing.T) {
	sp := newScenarioPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sink := &collectSink{}
	result, err := sp.prompt(t, ctx, "EXEC printf done-from-bridge\nSAY turn complete", sink)
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if !strings.Contains(result.Text, "[exec-done]") {
		t.Fatalf("terminal command did not run to completion: %q", result.Text)
	}
	if !strings.Contains(result.Text, "turn complete") {
		t.Fatalf("turn did not continue after exec: %q", result.Text)
	}
	// The exec must surface as a tool-call lifecycle on the event stream.
	var sawExecStart bool
	for _, event := range sink.events {
		if event.Type == acpclient.StreamEventToolCallStart && event.ToolName == "exec" {
			sawExecStart = true
		}
	}
	if !sawExecStart {
		t.Fatalf("no exec tool_call_start on the event stream: %#v", sink.events)
	}
}

func hasApprovalEvent(events []acpclient.StreamEvent, status string) bool {
	for _, event := range events {
		if event.Type == acpclient.StreamEventToolApprovalRequest && strings.EqualFold(event.Status, status) {
			return true
		}
	}
	return false
}
