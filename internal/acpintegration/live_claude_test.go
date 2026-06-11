package acpintegration

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/memohai/memoh/internal/acpclient"
)

// claudeLiveManaged builds the bot metadata for a managed (api_key) Claude
// Code session pointed at the test endpoint, or skips when the env is absent.
// Gated on MEMOH_LIVE_CLAUDE_ACP=1 + ANTHROPIC_API_KEY (+ optional
// ANTHROPIC_BASE_URL / MEMOH_LIVE_CLAUDE_BASE_URL for an Anthropic-compatible
// endpoint such as aihubmix).
func claudeLiveBotMetadata(t *testing.T) string {
	t.Helper()
	if os.Getenv("MEMOH_LIVE_CLAUDE_ACP") != "1" {
		t.Skip("set MEMOH_LIVE_CLAUDE_ACP=1 to run the live Claude Code ACP pool tests")
	}
	apiKey := strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY"))
	if apiKey == "" {
		t.Skip("ANTHROPIC_API_KEY is required for the live Claude Code ACP pool tests")
	}
	managed := claudeLiveManagedMap(apiKey)
	managedJSON, _ := json.Marshal(managed)
	return `{"acp":{"agents":{"claude-code":{"enabled":true,"setup_mode":"api_key","managed":` + string(managedJSON) + `}}}}`
}

// defaultLiveClaudeModel keeps the live Claude tests affordable. The BYOK
// proxy pre-authorizes a hold = max_tokens × output_price before the call; with
// the adapter's default (expensive) model + Memoh's pinned thinking budget that
// upfront hold can exceed a small test balance even though the real call costs
// cents (observed: 403 "balance is insufficient" on a funded account). A cheap
// model shrinks the hold ~15× and exercises the SAME containment path — Bash
// still routes through Memoh approval regardless of model. Override with
// MEMOH_LIVE_CLAUDE_MODEL (set it to the expensive default to test that too).
const defaultLiveClaudeModel = "claude-haiku-4-5-20251001"

// claudeLiveManagedMap builds the managed BYOK config for the live Claude
// tests: api_key, optional base_url, and a model pin (so the call fits a small
// test balance). Shared by the local and container live tests.
func claudeLiveManagedMap(apiKey string) map[string]string {
	managed := map[string]string{"api_key": apiKey}
	baseURL := strings.TrimSpace(os.Getenv("MEMOH_LIVE_CLAUDE_BASE_URL"))
	if baseURL == "" {
		baseURL = strings.TrimSpace(os.Getenv("ANTHROPIC_BASE_URL"))
	}
	if baseURL != "" {
		managed["base_url"] = baseURL
	}
	model := strings.TrimSpace(os.Getenv("MEMOH_LIVE_CLAUDE_MODEL"))
	if model == "" {
		model = defaultLiveClaudeModel
	}
	managed["model"] = model
	return managed
}

// TestACPLivePoolClaude runs the whole real stack against the REAL
// claude-agent-acp adapter: real SessionPool, real workspace bridge, real
// managed BYOK config (scoped CLAUDE_CONFIG_DIR + ask:["Bash"] settings +
// ANTHROPIC_* env), real Claude process. Claude is a THICK agent (it runs
// tools in its own process behind its own permission engine), so this is the
// only place the managed-settings containment can be verified against the
// real CLI.
func TestACPLivePoolClaude(t *testing.T) {
	botMetadata := claudeLiveBotMetadata(t)
	root, _ := scenarioDirs(t)
	sp := assembleScenarioPool(t, root, botMetadata, "claude-code", "")

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	sink := &collectSink{}
	result, err := sp.prompt(t, ctx,
		"Reply with exactly this text and nothing else, and do not modify any files: memoh-claude-live-ok",
		sink)
	if err != nil {
		if isExternalQuotaError(err) {
			t.Skipf("live Claude pool test skipped (external quota/account): %v", err)
		}
		t.Fatalf("live Claude pool prompt failed: %v", err)
	}
	if !strings.Contains(strings.ToLower(result.Text), "memoh-claude-live-ok") {
		t.Fatalf("live Claude pool text = %q, want marker memoh-claude-live-ok", result.Text)
	}
}

// TestACPLivePoolClaudeBashApproval is the containment capstone: it proves
// Memoh gates a real Claude's Bash command.
//
// Diagnostic history (worth keeping): an earlier version of this test ran
// with exec approvals OFF and concluded the ask:["Bash"] containment "did not
// hold" because `pwd` ran unprompted. That conclusion was wrong — a test bug,
// not a Memoh bug. claude-agent-acp routes Bash through the client TERMINAL
// capability (CreateTerminal callback), so the command is gated by Memoh's
// EXEC approval policy, not (only) by the settings.json ask rule. With exec
// approvals OFF, the default policy bypassed it, which looked like an escape.
//
// With exec approvals ON (execApprovalConfig), the real Claude's `pwd` is
// correctly held as a pending approval, approved by the "user", then runs —
// proving the policy layer gates a real thick agent's tool use end to end.
// (Only a real Claude process reveals which path Bash takes; L1's scripted
// agent never could.) Env-gated; default CI skips it.
func TestACPLivePoolClaudeBashApproval(t *testing.T) {
	botMetadata := claudeLiveBotMetadata(t)
	root, _ := scenarioDirs(t)
	// exec approvals on, so if Claude runs Bash through the client TERMINAL
	// capability it is gated by Memoh's exec policy. If it still runs with no
	// approval AND no exec tool_call event, Claude ran the command in its own
	// process — bypassing Memoh entirely — which is the real finding.
	sp := assembleScenarioPool(t, root, botMetadata, "claude-code", execApprovalConfig)

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	type outcome struct {
		result acpclient.PromptResult
		err    error
	}
	sink := &collectSink{}
	done := make(chan outcome, 1)
	go func() {
		result, err := sp.prompt(t, ctx,
			"Run the shell command `pwd` using your Bash tool to show the current working directory, then tell me the path.",
			sink)
		done <- outcome{result, err}
	}()

	// The managed ask:["Bash"] rule must force `pwd` through approval rather
	// than the CLI's internal auto-allow.
	approved := false
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		pending, err := sp.approval.ListPendingBySession(context.Background(), sp.botID, sp.sessionID)
		if err != nil {
			t.Fatalf("ListPendingBySession: %v", err)
		}
		if len(pending) > 0 {
			if _, err := sp.approval.Approve(context.Background(), pending[0].ID, testChannelID, "ok"); err != nil {
				t.Fatalf("Approve: %v", err)
			}
			approved = true
			break
		}
		select {
		case got := <-done:
			if isExternalQuotaError(got.err) {
				t.Skipf("live Claude Bash test skipped (external quota): %v", got.err)
			}
			// Diagnose which path Claude took for the command.
			var sawExec bool
			for _, e := range sink.events {
				if e.Type == acpclient.StreamEventToolCallStart && e.ToolName == "exec" {
					sawExec = true
				}
			}
			t.Fatalf("Claude ran Bash with no approval (exec tool_call seen=%v) — containment did not hold; err=%v text=%q",
				sawExec, got.err, got.result.Text)
		default:
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !approved {
		t.Fatal("no Bash approval appeared; managed ask:[Bash] containment may not have held")
	}

	select {
	case got := <-done:
		if got.err != nil {
			if isExternalQuotaError(got.err) {
				t.Skipf("live Claude Bash test skipped (external quota): %v", got.err)
			}
			t.Fatalf("live Claude Bash prompt failed after approval: %v", got.err)
		}
		t.Logf("Claude Bash was gated by managed ask:[Bash], approved, then ran: %q", got.result.Text)
	case <-time.After(60 * time.Second):
		t.Fatal("turn did not finish after Bash approval")
	}
}

// TestACPLivePoolClaudeWriteApproval is the codex↔claude PARITY capstone for
// file writes. TestACPLivePoolCodexApproval already proved a real codex (a thin
// agent — all fs goes through client callbacks) has its file write gated by
// Memoh's force-review policy. Claude is a THICK/hybrid agent (it ran Bash
// through the client TERMINAL capability, but file writes might take a
// different route — its own process, or the client fs capability), so codex's
// proof does NOT transfer. Only a real claude write reveals the path.
//
// The behavior that must match across both agents: when the agent creates a
// file, Memoh's approval gate holds. This test is diagnostic — if claude's
// write bypasses the gate (file lands with no approval) it fails loudly and
// names the path, instead of a false green.
func TestACPLivePoolClaudeWriteApproval(t *testing.T) {
	botMetadata := claudeLiveBotMetadata(t)
	root, _ := scenarioDirs(t)
	// Force-review writes (no bypass), exactly like the codex approval test, so
	// a real claude write must surface as a pending approval row.
	sp := assembleScenarioPool(t, root, botMetadata, "claude-code", forceReviewWriteConfig)

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	type outcome struct {
		result acpclient.PromptResult
		err    error
	}
	sink := &collectSink{}
	done := make(chan outcome, 1)
	go func() {
		result, err := sp.prompt(t, ctx,
			"Create a file named claude-approval-proof.txt in the current working directory whose entire contents are exactly: approved-write. Use your file-writing (Write) tool, not a shell command, then reply DONE.",
			sink)
		done <- outcome{result, err}
	}()

	approved := false
	approveDeadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(approveDeadline) {
		pending, err := sp.approval.ListPendingBySession(context.Background(), sp.botID, sp.sessionID)
		if err != nil {
			t.Fatalf("ListPendingBySession: %v", err)
		}
		if len(pending) > 0 {
			if _, err := sp.approval.Approve(context.Background(), pending[0].ID, testChannelID, "ok"); err != nil {
				t.Fatalf("Approve: %v", err)
			}
			approved = true
			break
		}
		select {
		case got := <-done:
			if isExternalQuotaError(got.err) {
				t.Skipf("live Claude write approval test skipped (external quota): %v", got.err)
			}
			// Turn ended with no approval — diagnose which path claude took.
			landed := findFileUnder(root, "claude-approval-proof.txt")
			sawWrite, sawExec := false, false
			for _, e := range sink.events {
				if e.Type == acpclient.StreamEventToolCallStart {
					switch e.ToolName {
					case "write", "edit":
						sawWrite = true
					case "exec":
						sawExec = true
					}
				}
			}
			if landed != "" {
				t.Fatalf("claude created the file with NO approval (write tool_call=%v exec tool_call=%v) — its write bypassed Memoh's force-review gate; codex parity does NOT hold; path=%s err=%v text=%q",
					sawWrite, sawExec, landed, got.err, got.result.Text)
			}
			t.Fatalf("turn finished before any approval and no file landed (write tool_call=%v exec tool_call=%v) — claude refused or was blocked by its own deny rule; err=%v text=%q",
				sawWrite, sawExec, got.err, got.result.Text)
		default:
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !approved {
		t.Fatal("no pending write approval appeared; claude's file write may have bypassed Memoh's gate")
	}

	var got outcome
	select {
	case got = <-done:
	case <-time.After(90 * time.Second):
		t.Fatal("turn did not finish after write approval")
	}
	if got.err != nil {
		if isExternalQuotaError(got.err) {
			t.Skipf("live Claude write approval test skipped (external quota): %v", got.err)
		}
		t.Fatalf("live Claude write approval prompt failed after approval: %v", got.err)
	}

	// The file only lands AFTER approval — proving the gate held for claude the
	// same way it did for codex.
	found := findFileUnder(root, "claude-approval-proof.txt")
	if found == "" {
		t.Fatalf("approved write did not produce the file under %s", root)
	}
	content, _ := os.ReadFile(found) //nolint:gosec // reads under t.TempDir.
	if !strings.Contains(string(content), "approved-write") {
		t.Fatalf("approved file content = %q, want approved-write", string(content))
	}
	t.Logf("real claude write was gated by approval (codex parity holds), then landed: %s", found)
}

// TestACPLivePoolClaudeReject is the live reject counterpart, symmetric with
// TestACPLivePoolCodexReject: the real claude's gated file action is DENIED and
// the turn must finish as a clean refusal with no file landing. Both agents
// must treat a denial the same way — a recoverable refusal, not a crash.
func TestACPLivePoolClaudeReject(t *testing.T) {
	botMetadata := claudeLiveBotMetadata(t)
	root, _ := scenarioDirs(t)
	sp := assembleScenarioPool(t, root, botMetadata, "claude-code", forceReviewAllConfig)

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	done := make(chan livePromptOutcome, 1)
	go func() {
		result, err := sp.prompt(t, ctx,
			"Create a file named claude-reject-proof.txt in the current working directory containing exactly: should-not-exist. Use your file-writing (Write) tool, then reply DONE.",
			&collectSink{})
		done <- livePromptOutcome{result, err}
	}()

	rejectFirstPendingAndAssertClean(t, sp, root, "claude-reject-proof.txt", done)
}

// TestACPLivePoolClaudeAbort is the live abort counterpart, symmetric with
// TestACPLivePoolCodexAbort: a real claude turn parked on a pending approval is
// aborted out-of-band, and the same warm runtime then serves the next prompt.
func TestACPLivePoolClaudeAbort(t *testing.T) {
	botMetadata := claudeLiveBotMetadata(t)
	root, _ := scenarioDirs(t)
	sp := assembleScenarioPool(t, root, botMetadata, "claude-code", forceReviewAllConfig)

	done := make(chan livePromptOutcome, 1)
	go func() {
		result, err := sp.prompt(t, context.Background(),
			"Create a file named claude-abort-proof.txt in the current working directory containing exactly: x. Use your file-writing (Write) tool, then reply DONE.",
			&collectSink{})
		done <- livePromptOutcome{result, err}
	}()

	abortInFlightAndAssertWarm(t, sp, "claude-code", done)
}
