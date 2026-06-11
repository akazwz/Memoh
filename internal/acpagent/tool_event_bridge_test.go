package acpagent

import (
	"reflect"
	"testing"

	"github.com/memohai/memoh/internal/acpclient"
	"github.com/memohai/memoh/internal/mcp"
	"github.com/memohai/memoh/internal/streamevent"
)

// captureSink records every acpclient.StreamEvent the bridge emits.
type captureSink struct{ events []acpclient.StreamEvent }

func (c *captureSink) EmitACPEvent(e acpclient.StreamEvent) { c.events = append(c.events, e) }

// TestEmitToolStreamEventBridgesNativeMCPToolEventsToACP locks the ACP entry
// bridge: when an ACP agent calls a Memoh native tool over MCP, every event the
// gateway emits (lifecycle, approval, user-input, side-effects) must cross to
// the ACP stream with all relevant fields intact — otherwise the ACP turn would
// render the tool differently from the in-process (native) agent loop. This is
// the per-field guard behind the native↔ACP UI parity for MCP tools.
func TestEmitToolStreamEventBridgesNativeMCPToolEventsToACP(t *testing.T) {
	input := map[string]any{"path": "/data/shot.png"}
	imageResult := map[string]any{"content": []any{map[string]any{"type": "image", "mimeType": "image/png"}}}
	approvalMeta := map[string]any{"approval": map[string]any{"approval_id": "ap-1", "status": "pending"}}
	atts := []streamevent.FileAttachment{{Type: "image", Name: "out.png", Mime: "image/png"}}
	reactions := []streamevent.ReactionItem{{Emoji: "👍"}}
	speeches := []streamevent.SpeechItem{{Text: "done"}}

	cases := []struct {
		name string
		in   mcp.ToolStreamEvent
		want acpclient.StreamEvent
	}{
		{
			name: "tool_call_start",
			in:   mcp.ToolStreamEvent{Type: string(acpclient.StreamEventToolCallStart), ToolCallID: "c1", ToolName: "read_media", Input: input},
			want: acpclient.StreamEvent{Type: acpclient.StreamEventToolCallStart, ToolCallID: "c1", ToolName: "read_media", Input: input},
		},
		{
			// read_media returns its image as the tool result content block; the
			// bridge must carry Result through so the ACP agent gets the visual
			// payload exactly like the native loop.
			name: "tool_call_end_with_image_result",
			in:   mcp.ToolStreamEvent{Type: string(acpclient.StreamEventToolCallEnd), ToolCallID: "c1", ToolName: "read_media", Input: input, Result: imageResult},
			want: acpclient.StreamEvent{Type: acpclient.StreamEventToolCallEnd, ToolCallID: "c1", ToolName: "read_media", Input: input, Result: imageResult},
		},
		{
			name: "tool_approval_request",
			in:   mcp.ToolStreamEvent{Type: string(acpclient.StreamEventToolApprovalRequest), ToolCallID: "c1", ToolName: "exec", Input: input, ApprovalID: "ap-1", ShortID: 7, Status: "pending", Metadata: approvalMeta},
			want: acpclient.StreamEvent{Type: acpclient.StreamEventToolApprovalRequest, ToolCallID: "c1", ToolName: "exec", Input: input, ApprovalID: "ap-1", ShortID: 7, Status: "pending", Metadata: approvalMeta},
		},
		{
			name: "user_input_request",
			in:   mcp.ToolStreamEvent{Type: string(acpclient.StreamEventUserInputRequest), ToolCallID: "c1", ToolName: "ask_user", Input: input, UserInputID: "ui-1", ShortID: 3, Status: "pending", Metadata: approvalMeta},
			want: acpclient.StreamEvent{Type: acpclient.StreamEventUserInputRequest, ToolCallID: "c1", ToolName: "ask_user", Input: input, UserInputID: "ui-1", ShortID: 3, Status: "pending", Metadata: approvalMeta},
		},
		{
			name: "attachment_side_effect",
			in:   mcp.ToolStreamEvent{Type: string(acpclient.StreamEventAttachment), Attachments: atts},
			want: acpclient.StreamEvent{Type: acpclient.StreamEventAttachment, Attachments: atts},
		},
		{
			name: "reaction_side_effect",
			in:   mcp.ToolStreamEvent{Type: string(acpclient.StreamEventReaction), Reactions: reactions},
			want: acpclient.StreamEvent{Type: acpclient.StreamEventReaction, Reactions: reactions},
		},
		{
			name: "speech_side_effect",
			in:   mcp.ToolStreamEvent{Type: string(acpclient.StreamEventSpeech), Speeches: speeches},
			want: acpclient.StreamEvent{Type: acpclient.StreamEventSpeech, Speeches: speeches},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := &captureSink{}
			newPromptToolEventSink(rec).EmitToolStreamEvent(c.in)
			if len(rec.events) != 1 {
				t.Fatalf("emitted %d events, want exactly 1: %#v", len(rec.events), rec.events)
			}
			if !reflect.DeepEqual(rec.events[0], c.want) {
				t.Fatalf("bridged %s =\n  %#v\nwant\n  %#v", c.name, rec.events[0], c.want)
			}
		})
	}
}
