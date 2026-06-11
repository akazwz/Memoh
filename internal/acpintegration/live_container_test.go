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

// TestACPLiveContainerClaudeBashApproval is the production-path containment
// test: Claude runs INSIDE the toolkit container with a managed HOME, reading
// HOME/.claude/settings.json (not the local-backend CLAUDE_CONFIG_DIR). Its
// Bash command must be gated by Memoh's exec approval — proving containment
// holds on the backend production actually uses.
//
// Gated on MEMOH_LIVE_CLAUDE_ACP_CONTAINER=1 + ANTHROPIC_API_KEY (+ optional
// ANTHROPIC_BASE_URL / MEMOH_LIVE_CLAUDE_BASE_URL). Builds the toolkit image
// (~5 min, or reuse via MEMOH_LIVE_ACP_CONTAINER_IMAGE) and needs docker.
func TestACPLiveContainerClaudeBashApproval(t *testing.T) {
	if os.Getenv("MEMOH_LIVE_CLAUDE_ACP_CONTAINER") != "1" {
		t.Skip("set MEMOH_LIVE_CLAUDE_ACP_CONTAINER=1 to run the live container Claude test")
	}
	apiKey := strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY"))
	if apiKey == "" {
		t.Skip("ANTHROPIC_API_KEY is required for the live container Claude test")
	}
	managed := claudeLiveManagedMap(apiKey)
	managedJSON, _ := json.Marshal(managed)
	botMetadata := `{"acp":{"agents":{"claude-code":{"enabled":true,"setup_mode":"api_key","managed":` + string(managedJSON) + `}}}}`

	sp := assembleContainerScenarioPool(t, botMetadata, "claude-code", execApprovalConfig)

	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
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

	approved := false
	deadline := time.Now().Add(120 * time.Second)
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
				t.Skipf("container Claude test skipped (external quota): %v", got.err)
			}
			var sawExec bool
			for _, e := range sink.events {
				if e.Type == acpclient.StreamEventToolCallStart && e.ToolName == "exec" {
					sawExec = true
				}
			}
			t.Fatalf("container Claude ran Bash with no approval (exec tool_call seen=%v) — production-path containment did not hold; err=%v text=%q",
				sawExec, got.err, got.result.Text)
		default:
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !approved {
		t.Fatal("no Bash approval appeared in the container; production-path containment may not have held")
	}

	select {
	case got := <-done:
		if got.err != nil {
			if isExternalQuotaError(got.err) {
				// Containment already held: the exec approval gate fired and we
				// approved it above (approved==true), so production-path
				// containment is PROVEN. Only the post-approval completion was
				// cut short by the BYOK proxy account running out of balance.
				t.Skipf("container Claude containment held (Bash was gated + approved); post-approval completion skipped (external quota): %v", got.err)
			}
			t.Fatalf("container Claude prompt failed after approval: %v", got.err)
		}
		t.Logf("container Claude Bash was gated by Memoh approval, then ran: %q", got.result.Text)
	case <-time.After(90 * time.Second):
		t.Fatal("turn did not finish after container Bash approval")
	}
}

// TestACPLiveContainerClaudeWriteApproval is the production-path codex↔claude
// write parity test. Claude runs INSIDE the container, so its file write could
// in principle go straight to the container filesystem (bypassing Memoh)
// instead of through the client fs capability. This proves it does NOT: a real
// claude write on the container backend is gated by Memoh's force-review
// policy — pending approval, approve, then the file lands in the bind-mounted
// /data/project — the same containment codex gets, on the backend production
// uses. Diagnostic: if the file lands with no approval, it fails loudly.
func TestACPLiveContainerClaudeWriteApproval(t *testing.T) {
	if os.Getenv("MEMOH_LIVE_CLAUDE_ACP_CONTAINER") != "1" {
		t.Skip("set MEMOH_LIVE_CLAUDE_ACP_CONTAINER=1 to run the live container Claude write test")
	}
	apiKey := strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY"))
	if apiKey == "" {
		t.Skip("ANTHROPIC_API_KEY is required for the live container Claude write test")
	}
	managed := claudeLiveManagedMap(apiKey)
	managedJSON, _ := json.Marshal(managed)
	botMetadata := `{"acp":{"agents":{"claude-code":{"enabled":true,"setup_mode":"api_key","managed":` + string(managedJSON) + `}}}}`

	sp := assembleContainerScenarioPool(t, botMetadata, "claude-code", forceReviewWriteConfig)
	projectDir := filepath.Join(sp.dataRoot, "project")

	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
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
	approveDeadline := time.Now().Add(120 * time.Second)
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
				t.Skipf("container Claude write test skipped (external quota): %v", got.err)
			}
			landed := findFileUnder(projectDir, "claude-approval-proof.txt")
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
				t.Fatalf("container claude created the file with NO approval (write tool_call=%v exec tool_call=%v) — its write bypassed Memoh's gate and hit the container FS directly; production-path codex parity does NOT hold; path=%s err=%v text=%q",
					sawWrite, sawExec, landed, got.err, got.result.Text)
			}
			t.Fatalf("container turn finished before any approval and no file landed (write tool_call=%v exec tool_call=%v); err=%v text=%q",
				sawWrite, sawExec, got.err, got.result.Text)
		default:
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !approved {
		t.Fatal("no pending write approval appeared in the container; claude's write may have bypassed Memoh's gate")
	}

	var got outcome
	select {
	case got = <-done:
	case <-time.After(90 * time.Second):
		t.Fatal("container turn did not finish after write approval")
	}
	if got.err != nil {
		if isExternalQuotaError(got.err) {
			t.Skipf("container Claude write test skipped (external quota): %v", got.err)
		}
		t.Fatalf("container Claude write prompt failed after approval: %v", got.err)
	}

	found := findFileUnder(projectDir, "claude-approval-proof.txt")
	if found == "" {
		t.Fatalf("approved container write did not produce the file under %s", projectDir)
	}
	content, _ := os.ReadFile(found) //nolint:gosec // reads under t.TempDir bind mount.
	if !strings.Contains(string(content), "approved-write") {
		t.Fatalf("approved container file content = %q, want approved-write", string(content))
	}
	t.Logf("real container claude write was gated by approval (production-path codex parity holds), then landed: %s", found)
}

// TestACPLiveContainerCodex is the container-backend smoke for codex: real
// codex in the toolkit container with managed CODEX_HOME, exercising the
// production container path for the thin agent.
func TestACPLiveContainerCodex(t *testing.T) {
	if os.Getenv("MEMOH_LIVE_CODEX_ACP_CONTAINER") != "1" {
		t.Skip("set MEMOH_LIVE_CODEX_ACP_CONTAINER=1 to run the live container Codex pool test")
	}
	apiKey := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	if apiKey == "" {
		t.Skip("OPENAI_API_KEY is required for the live container Codex test")
	}
	managed := map[string]string{"api_key": apiKey}
	if baseURL := strings.TrimSpace(os.Getenv("MEMOH_LIVE_CODEX_BASE_URL")); baseURL != "" {
		managed["base_url"] = baseURL
	}
	managedJSON, _ := json.Marshal(managed)
	botMetadata := `{"acp":{"agents":{"codex":{"enabled":true,"setup_mode":"api_key","managed":` + string(managedJSON) + `}}}}`

	sp := assembleContainerScenarioPool(t, botMetadata, "codex", "")

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	result, err := sp.prompt(t, ctx,
		"Reply with exactly this text and nothing else, and do not modify any files: memoh-container-live-ok",
		&collectSink{})
	if err != nil {
		if isExternalQuotaError(err) {
			t.Skipf("container Codex test skipped (external quota): %v", err)
		}
		t.Fatalf("container Codex prompt failed: %v", err)
	}
	if !strings.Contains(strings.ToLower(result.Text), "memoh-container-live-ok") {
		t.Fatalf("container Codex text = %q, want marker", result.Text)
	}
}
