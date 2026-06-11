package streamevent

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestEventJSONUsesCamelCaseWireContract locks the JSON keys the frontend
// consumes off the WS event stream. These tags are the wire contract; a typo
// here breaks the FE silently with no Go-side failure, so pin the camelCase
// keys (and forbid the snake_case slips) explicitly.
func TestEventJSONUsesCamelCaseWireContract(t *testing.T) {
	data, err := json.Marshal(Event{
		Type:        ToolApprovalRequest,
		ToolName:    "write",
		ToolCallID:  "tc-1",
		ApprovalID:  "ap-1",
		UserInputID: "ui-1",
		ShortID:     7,
		Status:      "pending",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(data)
	for _, want := range []string{
		`"type":"tool_approval_request"`,
		`"toolName":"write"`,
		`"toolCallId":"tc-1"`,
		`"approvalId":"ap-1"`,
		`"userInputId":"ui-1"`,
		`"shortId":7`,
		`"status":"pending"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("marshaled event missing %s\ngot: %s", want, got)
		}
	}
	for _, bad := range []string{"tool_name", "tool_call_id", "approval_id", "user_input_id", "short_id"} {
		if strings.Contains(got, `"`+bad+`"`) {
			t.Errorf("marshaled event used snake_case %q (FE expects camelCase): %s", bad, got)
		}
	}
}

// TestEventIsTerminal pins the end-of-stream predicate: only agent_end and
// agent_abort terminate; error/retry do not (a terminal frame follows).
func TestEventIsTerminal(t *testing.T) {
	for _, ty := range []Type{AgentEnd, AgentAbort} {
		if !(Event{Type: ty}).IsTerminal() {
			t.Errorf("Event{%s}.IsTerminal() = false, want true", ty)
		}
	}
	for _, ty := range []Type{AgentStart, TextDelta, ToolCallStart, ToolCallEnd, ToolApprovalRequest, Error, Retry} {
		if (Event{Type: ty}).IsTerminal() {
			t.Errorf("Event{%s}.IsTerminal() = true, want false", ty)
		}
	}
}
