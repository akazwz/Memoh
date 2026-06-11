package flow_test

import (
	"reflect"
	"testing"

	"github.com/memohai/memoh/internal/acpclient"
	agentpkg "github.com/memohai/memoh/internal/agent"
	"github.com/memohai/memoh/internal/conversation"
	"github.com/memohai/memoh/internal/conversation/flow"
)

// renderParitySteps runs a logical scenario through both pipelines (native
// events directly, ACP events through the real MapACPStreamEvent conversion)
// and returns the rendered UI messages for each, so a test can assert they are
// identical.
func renderParitySteps(t *testing.T, steps []parityScenarioEvent) (native, acp map[int]conversation.UIMessage) {
	t.Helper()
	nativeEvents := make([]agentpkg.StreamEvent, 0, len(steps))
	acpEvents := make([]agentpkg.StreamEvent, 0, len(steps))
	for _, step := range steps {
		nativeEvents = append(nativeEvents, step.native)
		if mapped, ok := flow.MapACPStreamEvent(step.acp); ok {
			acpEvents = append(acpEvents, mapped)
		}
	}
	return renderNative(t, nativeEvents), renderNative(t, acpEvents)
}

// TestNativeACPPipelineParityReadMediaToolResult locks read_media (visual
// input) parity: a native tool the agent calls over MCP returns an image
// content block as its result, and that tool call must render identically
// whether it came from the in-process agent loop or an ACP agent over MCP. This
// is the visual-input MCP tool the local-backend live tests can't drive.
func TestNativeACPPipelineParityReadMediaToolResult(t *testing.T) {
	t.Parallel()

	input := map[string]any{"path": "/data/shot.png"}
	imageResult := map[string]any{
		"content": []any{map[string]any{"type": "image", "mimeType": "image/png", "data": "AAAA"}},
	}
	steps := []parityScenarioEvent{
		{
			native: agentpkg.StreamEvent{Type: agentpkg.EventToolCallStart, ToolCallID: "c1", ToolName: "read_media", Input: input},
			acp:    acpclient.StreamEvent{Type: acpclient.StreamEventToolCallStart, ToolCallID: "c1", ToolName: "read_media", Input: input},
		},
		{
			native: agentpkg.StreamEvent{Type: agentpkg.EventToolCallEnd, ToolCallID: "c1", ToolName: "read_media", Input: input, Result: imageResult},
			acp:    acpclient.StreamEvent{Type: acpclient.StreamEventToolCallEnd, ToolCallID: "c1", ToolName: "read_media", Input: input, Result: imageResult},
		},
	}

	nativeMsgs, acpMsgs := renderParitySteps(t, steps)
	if !reflect.DeepEqual(nativeMsgs, acpMsgs) {
		t.Fatalf("read_media pipelines diverged:\nnative=%#v\nacp=%#v", nativeMsgs, acpMsgs)
	}
	// Not trivially equal: the read_media call must actually render a tool block
	// carrying its image result.
	var toolBlocks int
	for id := range acpMsgs {
		if acpMsgs[id].Type == conversation.UIMessageTool {
			toolBlocks++
		}
	}
	if toolBlocks != 1 {
		t.Fatalf("read_media rendered %d tool blocks, want 1: %#v", toolBlocks, acpMsgs)
	}
}

// TestNativeACPPipelineParityUserInputRequest locks user-input parity: when a
// native tool the agent called over MCP asks the user a question, the ACP turn
// must surface it the same way the in-process agent loop does (not silently
// drop it).
func TestNativeACPPipelineParityUserInputRequest(t *testing.T) {
	t.Parallel()

	input := map[string]any{"questions": []any{map[string]any{"text": "proceed?", "kind": "text"}}}
	meta := map[string]any{"user_input": map[string]any{"user_input_id": "ui-1", "short_id": 5, "status": "pending"}}
	steps := []parityScenarioEvent{
		{
			native: agentpkg.StreamEvent{Type: agentpkg.EventToolCallStart, ToolCallID: "c2", ToolName: "ask_user", Input: input},
			acp:    acpclient.StreamEvent{Type: acpclient.StreamEventToolCallStart, ToolCallID: "c2", ToolName: "ask_user", Input: input},
		},
		{
			native: agentpkg.StreamEvent{Type: agentpkg.EventUserInputRequest, ToolCallID: "c2", ToolName: "ask_user", Input: input, UserInputID: "ui-1", ShortID: 5, Status: "pending", Metadata: meta},
			acp:    acpclient.StreamEvent{Type: acpclient.StreamEventUserInputRequest, ToolCallID: "c2", ToolName: "ask_user", Input: input, UserInputID: "ui-1", ShortID: 5, Status: "pending", Metadata: meta},
		},
	}

	nativeMsgs, acpMsgs := renderParitySteps(t, steps)
	if !reflect.DeepEqual(nativeMsgs, acpMsgs) {
		t.Fatalf("user_input pipelines diverged:\nnative=%#v\nacp=%#v", nativeMsgs, acpMsgs)
	}
	if len(acpMsgs) == 0 {
		t.Fatalf("user_input request rendered no UI message (silently dropped): %#v", acpMsgs)
	}
}
