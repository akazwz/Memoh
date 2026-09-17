package opencode

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/felinics/memoh/internal/agent/decision/approval"
	userinput "github.com/felinics/memoh/internal/agent/decision/input"
	"github.com/felinics/memoh/internal/agent/event"
	"github.com/felinics/memoh/internal/agent/runtime/external"
	"github.com/felinics/memoh/internal/agent/runtime/opencode/opencodecfg"
)

// TestLiveServer runs the actual CLI against a deterministic local model.
// It requires no provider secrets and never reads the user's OpenCode home.
// Run explicitly with OPENCODE_INTEGRATION_BINARY=/absolute/path/to/opencode.
func TestLiveServer(t *testing.T) {
	binary := os.Getenv("OPENCODE_INTEGRATION_BINARY")
	if binary == "" {
		t.Skip("set OPENCODE_INTEGRATION_BINARY to test the native executable")
	}
	var completions atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if request["stream"] != true {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "title", "object": "chat.completion", "model": "test", "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "Native test"}, "finish_reason": "stop"}}})
			return
		}
		n := int32(0)
		if tools, ok := request["tools"].([]any); ok && len(tools) > 0 {
			n = completions.Add(1)
		}
		delta := map[string]any{"content": "Native tool and question completed."}
		finish := "stop"
		switch n {
		case 1:
			delta = map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call_bash", "type": "function", "function": map[string]any{"name": "bash", "arguments": `{"command":"printf native-tool-ok","description":"Verify native execution"}`}}}}
			finish = "tool_calls"
		case 2:
			delta = map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call_question", "type": "function", "function": map[string]any{"name": "question", "arguments": `{"questions":[{"question":"Which route?","header":"Route","options":[{"label":"Native","description":"HTTP server"},{"label":"ACP","description":"Generic protocol"}]}]}`}}}}
			finish = "tool_calls"
		}
		w.Header().Set("Content-Type", "text/event-stream")
		frame := func(d map[string]any, reason any) {
			b, _ := json.Marshal(map[string]any{"id": fmt.Sprintf("response_%d", n), "object": "chat.completion.chunk", "created": 1, "model": "test", "choices": []any{map[string]any{"index": 0, "delta": d, "finish_reason": reason}}})
			_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		}
		frame(delta, nil)
		frame(map[string]any{}, finish)
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer model.Close()
	root := t.TempDir()
	config := map[string]any{"autoupdate": false, "share": "disabled", "model": "test/test", "small_model": "test/test", "enabled_providers": []string{"test"}, "provider": map[string]any{"test": map[string]any{"npm": "@ai-sdk/openai-compatible", "name": "Test", "options": map[string]any{"baseURL": model.URL + "/v1", "apiKey": "local-test"}, "models": map[string]any{"test": map[string]any{"name": "Test", "limit": map[string]any{"context": 32000, "output": 4000}}}}}}
	payload, _ := json.Marshal(config)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	api, stopServer := liveServer(t, ctx, binary, root, payload)
	sink := &recordingSink{}
	turn := newTurn(external.PromptInput{Prompt: "Verify native tools", ContextMarkdown: "You are a deterministic test agent.", ModelID: "test/test", Sink: sink, CanRequestUserInput: true}, &acceptingApprovals{}, &answeringQuestions{}, nil)
	turn.api = api
	if err := turn.run(ctx, opencodecfg.Config{}); err != nil {
		t.Fatal(err)
	}
	turn.close()
	if turn.result(true).Text != "Native tool and question completed." {
		t.Fatalf("unexpected response: %+v", turn.result(true))
	}
	approved, asked, tool := false, false, false
	for _, e := range sink.events {
		if e.Type == event.ToolApprovalRequest {
			approved = true
		}
		if e.Type == event.UserInputRequest {
			asked = true
		}
		if e.Type == event.ToolCallEnd && e.ToolName == "exec" {
			tool = true
		}
	}
	if !approved || !asked || !tool {
		t.Fatalf("missing native interaction: approval=%v question=%v exec=%v events=%+v", approved, asked, tool, sink.events)
	}
	resumed := newTurn(external.PromptInput{Prompt: "Continue", ContextMarkdown: "Updated context", RuntimeMetadata: turn.result(true).RuntimeMetadata, ModelID: "test/test", Sink: &recordingSink{}}, nil, nil, nil)
	stopServer()
	resumed.api, _ = liveServer(t, ctx, binary, root, payload)
	if err := resumed.run(ctx, opencodecfg.Config{}); err != nil {
		t.Fatal(err)
	}
	resumed.close()
	if resumed.sessionID != turn.sessionID {
		t.Fatal("real native session was not resumed")
	}
	// Native forking regenerates IDs. Exercise an old-round cut, then another
	// cut using the original Memoh anchor from the copied history.
	fork, err := forkNative(ctx, resumed.api, resumed.result(true).RuntimeMetadata, turn.messageID)
	if err != nil {
		t.Fatal(err)
	}
	if metadataString(fork, metadataMessageIDKey) == turn.messageID {
		t.Fatal("native fork did not remap its anchor")
	}
	nested, err := forkNative(ctx, resumed.api, fork, turn.messageID)
	if err != nil {
		t.Fatal(err)
	}
	if metadataString(nested, metadataSessionIDKey) == metadataString(fork, metadataSessionIDKey) {
		t.Fatal("nested fork reused its parent session")
	}
	branch := newTurn(external.PromptInput{Prompt: "Continue branch", RuntimeMetadata: fork, ModelID: "test/test", Sink: &recordingSink{}}, nil, nil, nil)
	branch.api, _ = liveServer(t, ctx, binary, root, payload)
	if err := branch.run(ctx, opencodecfg.Config{}); err != nil {
		t.Fatal(err)
	}
	branch.close()
	if branch.sessionID != metadataString(fork, metadataSessionIDKey) {
		t.Fatal("fork was replayed into a fresh session")
	}
	metadata, err := compactNative(ctx, resumed.api, external.PromptInput{RuntimeMetadata: resumed.result(true).RuntimeMetadata, ModelID: "test/test"})
	if err != nil {
		t.Fatal(err)
	}
	afterCompact := newTurn(external.PromptInput{Prompt: "Continue after compaction", RuntimeMetadata: metadata, ModelID: "test/test", Sink: &recordingSink{}}, nil, nil, nil)
	afterCompact.api = resumed.api
	if err := afterCompact.run(ctx, opencodecfg.Config{}); err != nil {
		t.Fatal(err)
	}
	afterCompact.close()
	if afterCompact.sessionID != resumed.sessionID {
		t.Fatal("compaction lost the native session")
	}
	t.Logf("verified native server: restart, tools, decisions, nested forks, independent branch process, compaction and continuation; model calls=%d", completions.Load())
}

type acceptingApprovals struct{}

func (*acceptingApprovals) RegisterWaiter(string) func() { return func() {} }
func (*acceptingApprovals) EvaluatePolicy(context.Context, approval.CreatePendingInput) (approval.Evaluation, error) {
	panic("native approval must not use MCP policy")
}

func (*acceptingApprovals) CreatePending(_ context.Context, i approval.CreatePendingInput) (approval.Request, error) {
	return approval.Request{ID: "approval", Status: approval.StatusPending, ToolCallID: i.ToolCallID, ToolName: i.ToolName, ToolInput: i.ToolInput.(map[string]any)}, nil
}

func (*acceptingApprovals) Get(context.Context, string) (approval.Request, error) {
	return approval.Request{}, approval.ErrNotFound
}

func (*acceptingApprovals) Reject(context.Context, string, string, string) (approval.Request, error) {
	return approval.Request{Status: approval.StatusRejected}, nil
}

func (*acceptingApprovals) WaitForDecision(context.Context, string) (approval.Request, error) {
	return approval.Request{Status: approval.StatusApproved}, nil
}

type answeringQuestions struct{}

func (*answeringQuestions) RegisterWaiter(string) func() { return func() {} }
func (*answeringQuestions) CreatePending(_ context.Context, i userinput.CreatePendingInput) (userinput.Request, error) {
	return userinput.Request{ID: "question", Status: userinput.StatusPending, ToolCallID: i.ToolCallID, ToolName: i.ToolName, Input: i.Input.(map[string]any)}, nil
}

func (*answeringQuestions) Cancel(context.Context, userinput.CancelInput) (userinput.Request, error) {
	return userinput.Request{Status: userinput.StatusCanceled}, nil
}

func (*answeringQuestions) WaitForRegisteredResponse(context.Context, string) (userinput.Request, error) {
	return userinput.Request{Status: userinput.StatusSubmitted, Result: map[string]any{"answers": []userinput.UIAnswer{{QuestionID: "q1", Selected: []userinput.UIOption{{Label: "Native"}}}}}}, nil
}

func liveServer(t *testing.T, ctx context.Context, binary, root string, payload []byte) (*apiClient, func()) {
	t.Helper()
	processCtx, stopProcess := context.WithCancel(ctx)
	cmd := exec.CommandContext(processCtx, binary, "serve", "--hostname", "127.0.0.1", "--port", "0") //nolint:gosec // Opt-in test executable explicitly selected by the developer.
	cmd.Dir = root
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "XDG_CONFIG_HOME=" + filepath.Join(root, "config"), "XDG_DATA_HOME=" + filepath.Join(root, "data"), "XDG_STATE_HOME=" + filepath.Join(root, "state"), "XDG_CACHE_HOME=" + filepath.Join(root, "cache"), "OPENCODE_SERVER_PASSWORD=test-password", "OPENCODE_DISABLE_AUTOUPDATE=true", "OPENCODE_DISABLE_PROJECT_CONFIG=true", "OPENCODE_CONFIG_CONTENT=" + string(payload)}
	stderr, err := os.Create(filepath.Join(root, "stderr.log")) //nolint:gosec // Test-owned temporary directory.
	if err != nil {
		t.Fatal(err)
	}

	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			stopProcess()
			_ = cmd.Wait()
			if t.Failed() {
				data, _ := os.ReadFile(stderr.Name()) //nolint:gosec // Diagnostic file created by this test.
				t.Logf("native diagnostics: %s", data)
			}
			_ = stderr.Close()
		})
	}
	t.Cleanup(stop)
	addresses := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if url, ok := strings.CutPrefix(scanner.Text(), "opencode server listening on "); ok {
				addresses <- url
				return
			}
		}
	}()
	var base string
	select {
	case base = <-addresses:
	case <-ctx.Done():
		t.Fatal("native server startup timed out")
	}
	return &apiClient{http: http.DefaultClient, baseURL: base, password: "test-password", directory: root}, stop
}
