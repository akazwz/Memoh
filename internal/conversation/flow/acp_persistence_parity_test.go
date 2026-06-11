package flow

import (
	"reflect"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	"github.com/memohai/memoh/internal/acpclient"
	"github.com/memohai/memoh/internal/toolapproval"
)

// TestACPResultOutputMessagesMatchesNativeRoundShape locks the persistence
// parity the UI parity suite cannot see: acpResultOutputMessages is a second
// hand-built transcript builder, and the round it persists for a canonical
// turn (reasoning → approval → tool call → tool result → answer) must equal
// the round the native pipeline would persist for the same SDK messages.
// The approval event deliberately precedes tool_call_start — that is the real
// ACP ordering (RequestPermission fires before the tool runs).
func TestACPResultOutputMessagesMatchesNativeRoundShape(t *testing.T) {
	t.Parallel()

	input := map[string]any{"command": "pwd"}
	result := map[string]any{"stdout": "/data"}
	approvalMeta := toolapproval.RequestMetadata(toolapproval.Request{
		ID:      "approval-1",
		ShortID: 7,
		Status:  toolapproval.StatusApproved,
	})

	got := acpResultOutputMessages(acpclient.PromptResult{
		Events: []acpclient.StreamEvent{
			{Type: acpclient.StreamEventReasoningDelta, Delta: "let me check"},
			{
				Type:       acpclient.StreamEventToolApprovalRequest,
				ToolCallID: "call-1",
				ToolName:   "exec",
				Input:      input,
				ApprovalID: "approval-1",
				ShortID:    7,
				Status:     toolapproval.StatusApproved,
			},
			{Type: acpclient.StreamEventToolCallStart, ToolCallID: "call-1", ToolName: "exec", Input: input},
			{Type: acpclient.StreamEventToolCallEnd, ToolCallID: "call-1", ToolName: "exec", Input: input, Result: result},
			{Type: acpclient.StreamEventTextDelta, Delta: "done"},
		},
	})

	want := sdkMessagesToModelMessages([]sdk.Message{
		{
			Role: sdk.MessageRoleAssistant,
			Content: []sdk.MessagePart{
				sdk.ReasoningPart{Text: "let me check"},
				sdk.ToolCallPart{
					ToolCallID: "call-1",
					ToolName:   "exec",
					Input:      input,
					ProviderMetadata: map[string]any{
						"approval": approvalMeta,
					},
				},
			},
		},
		sdk.ToolMessage(sdk.ToolResultPart{
			ToolCallID: "call-1",
			ToolName:   "exec",
			Result:     result,
		}),
		{
			Role:    sdk.MessageRoleAssistant,
			Content: []sdk.MessagePart{sdk.TextPart{Text: "done"}},
		},
	})

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ACP persisted round diverged from the native shape:\ngot=%#v\nwant=%#v", got, want)
	}
}
