package tools

import (
	"reflect"
	"testing"

	"github.com/memohai/memoh/internal/mcp"
	"github.com/memohai/memoh/internal/streamevent"
)

// TestStreamEmitterForBridgesSideEffectsToMCP locks the first hop of the ACP
// tool path: a native tool's side-effects (attachment/reaction/speech), emitted
// as tools.ToolStreamEvent inside the tool, must convert to the canonical
// mcp.ToolStreamEvent with every field intact — so an ACP agent calling the
// tool over MCP gets the same attachment/reaction/TTS surface as the in-process
// agent loop. Empty payloads and non-side-effect types must NOT emit.
func TestStreamEmitterForBridgesSideEffectsToMCP(t *testing.T) {
	att := Attachment{
		Type: "image", Path: "/data/out.png", URL: "https://x/out.png", Base64: "AAAA",
		PlatformKey: "pk-1", Mime: "image/png", Name: "out.png", ContentHash: "h1", Size: 42,
		Metadata: map[string]any{"k": "v"},
	}
	cases := []struct {
		name string
		in   ToolStreamEvent
		want mcp.ToolStreamEvent
	}{
		{
			name: "attachment",
			in:   ToolStreamEvent{Type: StreamEventAttachment, Attachments: []Attachment{att}},
			want: mcp.ToolStreamEvent{
				Type: string(streamevent.Attachment),
				Attachments: []streamevent.FileAttachment{{
					Type: "image", Path: "/data/out.png", URL: "https://x/out.png", Base64: "AAAA",
					PlatformKey: "pk-1", Mime: "image/png", Name: "out.png", ContentHash: "h1", Size: 42,
					Metadata: map[string]any{"k": "v"},
				}},
			},
		},
		{
			name: "reaction",
			in:   ToolStreamEvent{Type: StreamEventReaction, Reactions: []Reaction{{Emoji: "👍"}}},
			want: mcp.ToolStreamEvent{Type: string(streamevent.Reaction), Reactions: []streamevent.ReactionItem{{Emoji: "👍"}}},
		},
		{
			name: "speech",
			in:   ToolStreamEvent{Type: StreamEventSpeech, Speeches: []Speech{{Text: "hi"}}},
			want: mcp.ToolStreamEvent{Type: string(streamevent.Speech), Speeches: []streamevent.SpeechItem{{Text: "hi"}}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := &nativeSourceToolEvents{delivered: true}
			source := NewNativeToolSource(nil, nil, NativeToolSourceOptions{ToolEvents: fake})
			source.streamEmitterFor(mcp.ToolSessionContext{SessionID: "s1"})(c.in)
			if len(fake.events) != 1 {
				t.Fatalf("appended %d events, want exactly 1: %#v", len(fake.events), fake.events)
			}
			if !reflect.DeepEqual(fake.events[0], c.want) {
				t.Fatalf("bridged %s =\n  %#v\nwant\n  %#v", c.name, fake.events[0], c.want)
			}
		})
	}

	// Empty payloads and non-side-effect types must early-return (no event).
	for _, in := range []ToolStreamEvent{
		{Type: StreamEventAttachment},    // no attachments
		{Type: StreamEventReaction},      // no reactions
		{Type: StreamEventSpeech},        // no speeches
		{Type: StreamEventSpawnHeartbeat}, // not a side-effect
	} {
		fake := &nativeSourceToolEvents{delivered: true}
		source := NewNativeToolSource(nil, nil, NativeToolSourceOptions{ToolEvents: fake})
		source.streamEmitterFor(mcp.ToolSessionContext{SessionID: "s1"})(in)
		if len(fake.events) != 0 {
			t.Fatalf("type %q emitted %d events, want 0 (early return)", in.Type, len(fake.events))
		}
	}
}
