package acpintegration

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/memohai/memoh/internal/acpclient"
)

// codexLiveBotMetadata builds the bot metadata for a managed (api_key) Codex
// session pointed at the test endpoint, or skips when the env is absent. The
// symmetric counterpart of claudeLiveBotMetadata. Gated on
// MEMOH_LIVE_CODEX_ACP=1 + OPENAI_API_KEY (+ optional MEMOH_LIVE_CODEX_BASE_URL).
func codexLiveBotMetadata(t *testing.T) string {
	t.Helper()
	if os.Getenv("MEMOH_LIVE_CODEX_ACP") != "1" {
		t.Skip("set MEMOH_LIVE_CODEX_ACP=1 to run the live Codex ACP pool tests")
	}
	apiKey := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	if apiKey == "" {
		t.Skip("OPENAI_API_KEY is required for the live Codex ACP pool tests")
	}
	managed := map[string]string{"api_key": apiKey}
	if baseURL := strings.TrimSpace(os.Getenv("MEMOH_LIVE_CODEX_BASE_URL")); baseURL != "" {
		managed["base_url"] = baseURL
	}
	managedJSON, _ := json.Marshal(managed)
	return `{"acp":{"agents":{"codex":{"enabled":true,"setup_mode":"api_key","managed":` + string(managedJSON) + `}}}}`
}

// TestACPLivePoolCodex runs the WHOLE real stack against the REAL codex-acp
// adapter and a real model endpoint: real SessionPool, real workspace bridge,
// real managed BYOK config (api_key + base_url written to a scoped
// CODEX_HOME), real codex process. It is the L3 capstone — same pool the L1
// scenarios drive, only the agent backend is real.
//
// Gated on MEMOH_LIVE_CODEX_ACP=1 + OPENAI_API_KEY (+ optional
// MEMOH_LIVE_CODEX_BASE_URL for an alternate OpenAI-compatible endpoint).
func TestACPLivePoolCodex(t *testing.T) {
	if os.Getenv("MEMOH_LIVE_CODEX_ACP") != "1" {
		t.Skip("set MEMOH_LIVE_CODEX_ACP=1 to run the live Codex ACP pool test")
	}
	apiKey := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	if apiKey == "" {
		t.Skip("OPENAI_API_KEY is required for the live Codex ACP pool test")
	}
	managed := map[string]string{"api_key": apiKey}
	if baseURL := strings.TrimSpace(os.Getenv("MEMOH_LIVE_CODEX_BASE_URL")); baseURL != "" {
		managed["base_url"] = baseURL
	}
	managedJSON, err := json.Marshal(managed)
	if err != nil {
		t.Fatalf("marshal managed config: %v", err)
	}
	botMetadata := `{"acp":{"agents":{"codex":{"enabled":true,"setup_mode":"api_key","managed":` + string(managedJSON) + `}}}}`

	// scenarioDirs creates the project working directory the agent process
	// chdir's into; a bare TempDir would make the real codex process fail to
	// fork (chdir to a missing cwd).
	root, _ := scenarioDirs(t)
	sp := assembleScenarioPool(t, root, botMetadata, "codex", "")

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Natural language — the real model, not the scripted directive grammar.
	sink := &collectSink{}
	result, err := sp.prompt(t, ctx,
		"Reply with exactly this text and nothing else, and do not modify any files: memoh-pool-live-ok",
		sink)
	if err != nil {
		if isExternalQuotaError(err) {
			t.Skipf("live Codex pool test skipped (external quota/account): %v", err)
		}
		t.Fatalf("live Codex pool prompt failed: %v", err)
	}
	if !strings.Contains(strings.ToLower(result.Text), "memoh-pool-live-ok") {
		t.Fatalf("live Codex pool text = %q, want marker memoh-pool-live-ok", result.Text)
	}
}

// TestACPLivePoolCodexToolUse drives the REAL codex through a tool-using turn:
// it is asked to write a file, which (if codex uses its file capability)
// flows codex -> ACP fs.write_text_file callback -> approval -> real workspace
// bridge -> disk. This exercises the client-capability + bridge path with a
// real agent, not just the scripted one.
func TestACPLivePoolCodexToolUse(t *testing.T) {
	if os.Getenv("MEMOH_LIVE_CODEX_ACP") != "1" {
		t.Skip("set MEMOH_LIVE_CODEX_ACP=1 to run the live Codex ACP pool tool test")
	}
	apiKey := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	if apiKey == "" {
		t.Skip("OPENAI_API_KEY is required for the live Codex ACP pool tool test")
	}
	managed := map[string]string{"api_key": apiKey}
	if baseURL := strings.TrimSpace(os.Getenv("MEMOH_LIVE_CODEX_BASE_URL")); baseURL != "" {
		managed["base_url"] = baseURL
	}
	managedJSON, _ := json.Marshal(managed)
	botMetadata := `{"acp":{"agents":{"codex":{"enabled":true,"setup_mode":"api_key","managed":` + string(managedJSON) + `}}}}`

	root, _ := scenarioDirs(t)
	sp := assembleScenarioPool(t, root, botMetadata, "codex", "")

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	sink := &collectSink{}
	_, err := sp.prompt(t, ctx,
		"Create a file named codex-live-proof.txt in the current working directory whose entire contents are exactly the line: codex-was-here. Use your file-writing tool to do it, then reply DONE.",
		sink)
	if err != nil {
		if isExternalQuotaError(err) {
			t.Skipf("live Codex tool test skipped (external quota/account): %v", err)
		}
		t.Fatalf("live Codex tool prompt failed: %v", err)
	}

	// The real agent must have driven a write tool call through the pipeline.
	var sawWriteToolCall bool
	for _, event := range sink.events {
		if event.Type == acpclient.StreamEventToolCallStart && (event.ToolName == "write" || event.ToolName == "edit") {
			sawWriteToolCall = true
		}
	}
	// And the file must actually exist on the workspace bridge's disk.
	found := findFileUnder(root, "codex-live-proof.txt")
	if !sawWriteToolCall && found == "" {
		t.Fatalf("real codex neither emitted a write tool call nor created the file; events=%#v", sink.events)
	}
	if found == "" {
		t.Fatalf("write tool call was emitted but the file never landed on the bridge disk under %s", root)
	}
	content, readErr := os.ReadFile(found) //nolint:gosec // reads a file under t.TempDir.
	if readErr != nil {
		t.Fatalf("read written file: %v", readErr)
	}
	if !strings.Contains(string(content), "codex-was-here") {
		t.Fatalf("written file content = %q, want it to contain codex-was-here", string(content))
	}
	t.Logf("real codex wrote %s (%d bytes)", found, len(content))
}

// TestACPLivePoolCodexApproval is the deepest live scenario: the REAL codex
// is asked to write a file, write approval is FORCED (not bypassed), so the
// turn blocks on a real pending approval row in the real DB. A concurrent
// "user" approves it, and only then does the file land on disk — proving the
// approval flow actually gates a real agent's real tool call.
func TestACPLivePoolCodexApproval(t *testing.T) {
	if os.Getenv("MEMOH_LIVE_CODEX_ACP") != "1" {
		t.Skip("set MEMOH_LIVE_CODEX_ACP=1 to run the live Codex ACP approval test")
	}
	apiKey := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	if apiKey == "" {
		t.Skip("OPENAI_API_KEY is required for the live Codex ACP approval test")
	}
	managed := map[string]string{"api_key": apiKey}
	if baseURL := strings.TrimSpace(os.Getenv("MEMOH_LIVE_CODEX_BASE_URL")); baseURL != "" {
		managed["base_url"] = baseURL
	}
	managedJSON, _ := json.Marshal(managed)
	botMetadata := `{"acp":{"agents":{"codex":{"enabled":true,"setup_mode":"api_key","managed":` + string(managedJSON) + `}}}}`

	root, _ := scenarioDirs(t)
	sp := assembleScenarioPool(t, root, botMetadata, "codex", forceReviewWriteConfig)

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	type outcome struct {
		result acpclient.PromptResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := sp.prompt(t, ctx,
			"Create a file named codex-approval-proof.txt in the current working directory containing exactly: approved-write. Use your file-writing tool, then reply DONE.",
			&collectSink{})
		done <- outcome{result, err}
	}()

	// A real pending approval must appear (the write is gated, not bypassed).
	approved := false
	approveDeadline := time.Now().Add(60 * time.Second)
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
		// Done early without any approval would mean codex never used the
		// gated tool (e.g. answered in text) — fail loudly below.
		select {
		case got := <-done:
			if isExternalQuotaError(got.err) {
				t.Skipf("live approval test skipped (external quota): %v", got.err)
			}
			t.Fatalf("turn finished before any approval appeared (codex did not use the gated write tool); err=%v text=%q", got.err, got.result.Text)
		default:
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !approved {
		t.Fatal("no pending write approval appeared within the deadline")
	}

	var got outcome
	select {
	case got = <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("turn did not finish after approval")
	}
	if got.err != nil {
		if isExternalQuotaError(got.err) {
			t.Skipf("live approval test skipped (external quota): %v", got.err)
		}
		t.Fatalf("live approval prompt failed: %v", got.err)
	}

	// The file only lands AFTER approval — proving the gate held.
	found := findFileUnder(root, "codex-approval-proof.txt")
	if found == "" {
		t.Fatalf("approved write did not produce the file under %s", root)
	}
	content, _ := os.ReadFile(found) //nolint:gosec // reads under t.TempDir.
	if !strings.Contains(string(content), "approved-write") {
		t.Fatalf("approved file content = %q, want approved-write", string(content))
	}
	t.Logf("real codex write was gated by approval, then landed: %s", found)
}

// findFileUnder returns the first path of a file with the given base name
// under root, or "" if none exists.
func findFileUnder(root, name string) string {
	var match string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || match != "" {
			return nil
		}
		if !d.IsDir() && d.Name() == name {
			match = path
		}
		return nil
	})
	return match
}

func isExternalQuotaError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "usage_limit_exceeded") ||
		strings.Contains(msg, "you've hit your usage limit") ||
		strings.Contains(msg, "insufficient_quota") ||
		strings.Contains(msg, "quota") ||
		// Billing-balance wall on the BYOK proxy: the key was accepted and
		// routed (so containment/config is fine), but the account lacks funds
		// to cover this request — either genuinely empty OR the proxy's upfront
		// hold (max_tokens × price) exceeds the remaining balance. The live
		// Claude tests pin a cheap model to keep the hold small; this stays as a
		// safety net so a truly under-funded run skips instead of red-failing.
		// Matched narrowly so a genuine auth/config failure ("invalid api key",
		// 401, "x-api-key required") still fails loudly.
		strings.Contains(msg, "balance is insufficient") ||
		strings.Contains(msg, "insufficient balance") ||
		strings.Contains(msg, "recharge")
}

// TestACPLivePoolCodexReject is the live reject counterpart of CodexApproval:
// the real codex's gated write is DENIED, and the turn must finish as a clean
// refusal with no file landing. Symmetric with TestACPLivePoolClaudeReject.
func TestACPLivePoolCodexReject(t *testing.T) {
	botMetadata := codexLiveBotMetadata(t)
	root, _ := scenarioDirs(t)
	sp := assembleScenarioPool(t, root, botMetadata, "codex", forceReviewAllConfig)

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	done := make(chan livePromptOutcome, 1)
	go func() {
		result, err := sp.prompt(t, ctx,
			"Create a file named codex-reject-proof.txt in the current working directory containing exactly: should-not-exist. Use your file-writing tool, then reply DONE.",
			&collectSink{})
		done <- livePromptOutcome{result, err}
	}()

	rejectFirstPendingAndAssertClean(t, sp, root, "codex-reject-proof.txt", done)
}

// TestACPLivePoolCodexAbort is the live abort counterpart: a real codex turn is
// parked on a pending approval, aborted out-of-band by turn id, and the same
// warm runtime then serves the next prompt. Symmetric with
// TestACPLivePoolClaudeAbort.
func TestACPLivePoolCodexAbort(t *testing.T) {
	botMetadata := codexLiveBotMetadata(t)
	root, _ := scenarioDirs(t)
	sp := assembleScenarioPool(t, root, botMetadata, "codex", forceReviewAllConfig)

	done := make(chan livePromptOutcome, 1)
	go func() {
		result, err := sp.prompt(t, context.Background(),
			"Create a file named codex-abort-proof.txt in the current working directory containing exactly: x. Use your file-writing tool, then reply DONE.",
			&collectSink{})
		done <- livePromptOutcome{result, err}
	}()

	abortInFlightAndAssertWarm(t, sp, "codex", done)
}
