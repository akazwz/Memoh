package acpclient

import (
	"testing"

	acp "github.com/coder/acp-go-sdk"
)

// TestEditToolWithWriteTitleReclassifiesConsistently locks the edit→write name
// fix: a write-titled edit must surface as "write" in BOTH the approval path
// and the streamed tool events. Both resolve through nativeToolFromACPState, so
// one action can never show two names (the prior bug streamed "edit" while the
// approval said "write").
func TestEditToolWithWriteTitleReclassifiesConsistently(t *testing.T) {
	writeTitled := &acpToolState{
		id:    "tc-1",
		kind:  string(acp.ToolKindEdit),
		title: "Write config.yaml",
		input: map[string]any{
			"file_path":  "config.yaml",
			"old_string": "a",
			"new_string": "b",
		},
	}
	if name, _, ok := nativeToolFromACPState(writeTitled); !ok || name != "write" {
		t.Fatalf("nativeToolFromACPState name=%q ok=%v, want write/true", name, ok)
	}
	// The streamed tool event must agree with the approval name.
	events := newACPToolEventMapper().eventsForState(writeTitled)
	if len(events) == 0 || events[0].ToolName != "write" {
		t.Fatalf("eventsForState first event = %+v, want ToolName=write", events)
	}

	// A plain edit (no write/create/new-file title) must stay "edit" — the
	// reclassification must not fire on every edit.
	plainEdit := &acpToolState{
		id:    "tc-2",
		kind:  string(acp.ToolKindEdit),
		title: "Edit config.yaml",
		input: map[string]any{
			"file_path":  "config.yaml",
			"old_string": "a",
			"new_string": "b",
		},
	}
	if name, _, ok := nativeToolFromACPState(plainEdit); !ok || name != "edit" {
		t.Fatalf("plain edit name=%q ok=%v, want edit/true", name, ok)
	}
}
