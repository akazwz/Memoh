package acpintegration

import (
	"context"
	"sync"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	agenttools "github.com/memohai/memoh/internal/agent/tools"
	"github.com/memohai/memoh/internal/mcp"
	"github.com/memohai/memoh/internal/streamevent"
)

// sideEffectProvider exposes one tool whose execution emits attachment,
// reaction and speech side-effects through the session emitter — the same way
// real tools (image generation, TTS, reactions) do.
type sideEffectProvider struct{ toolName string }

func (p *sideEffectProvider) Tools(_ context.Context, session agenttools.SessionContext) ([]sdk.Tool, error) {
	emitter := session.Emitter
	sawImageInput := session.SupportsImageInput
	return []sdk.Tool{{
		Name:       p.toolName,
		Parameters: map[string]any{"type": "object"},
		Execute: func(_ *sdk.ToolExecContext, _ any) (any, error) {
			if emitter != nil {
				emitter(agenttools.ToolStreamEvent{
					Type:        agenttools.StreamEventAttachment,
					Attachments: []agenttools.Attachment{{Type: "image", Path: "/data/shot.png", Mime: "image/png"}},
				})
				emitter(agenttools.ToolStreamEvent{
					Type:      agenttools.StreamEventReaction,
					Reactions: []agenttools.Reaction{{Emoji: "✅"}},
				})
				emitter(agenttools.ToolStreamEvent{
					Type:     agenttools.StreamEventSpeech,
					Speeches: []agenttools.Speech{{Text: "all done"}},
				})
			}
			return map[string]any{"ok": true, "saw_image_input": sawImageInput}, nil
		},
	}}, nil
}

// readMediaProvider exposes a tool that returns a ReadMediaToolOutput (an
// image read from the workspace), so we can assert the gateway turns it into a
// standard MCP image content block — the visual-input parity the in-process
// agent loop gets via message injection.
type readMediaProvider struct{ toolName string }

func (p *readMediaProvider) Tools(_ context.Context, _ agenttools.SessionContext) ([]sdk.Tool, error) {
	return []sdk.Tool{{
		Name:       p.toolName,
		Parameters: map[string]any{"type": "object"},
		Execute: func(_ *sdk.ToolExecContext, _ any) (any, error) {
			return agenttools.ReadMediaToolOutput{
				Public:         agenttools.ReadMediaToolResult{OK: true, Path: "/data/shot.png", Mime: "image/png", Size: 11},
				ImageBase64:    "aGVsbG8taW1hZ2U=", // "hello-image"
				ImageMediaType: "image/png",
			}, nil
		},
	}}, nil
}

// TestReadMediaReturnsMCPImageContent asserts the gateway-side visual-input
// parity: a tool returning an image produces an MCP image content block (data
// + mimeType), not just a textual description, so an external ACP agent
// actually receives the picture.
func TestReadMediaReturnsMCPImageContent(t *testing.T) {
	source := agenttools.NewNativeToolSource(nil, []agenttools.ToolProvider{&readMediaProvider{toolName: "read"}}, agenttools.NativeToolSourceOptions{
		AllowAll: true,
	})
	session := mcp.ToolSessionContext{
		BotID:              testBotID,
		SessionID:          testSessionID,
		StreamID:           "stream-1",
		SupportsImageInput: true,
	}
	result, err := source.CallTool(context.Background(), session, "read", map[string]any{"path": "/data/shot.png"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	content, _ := result["content"].([]map[string]any)
	var image map[string]any
	for _, block := range content {
		if block["type"] == "image" {
			image = block
		}
	}
	if image == nil {
		t.Fatalf("read result carried no image content block: %#v", result)
	}
	if image["data"] != "aGVsbG8taW1hZ2U=" || image["mimeType"] != "image/png" {
		t.Fatalf("image content block = %#v, want the base64 + mime", image)
	}
}

// TestToolSideEffectsReachACPSink wires the REAL NativeToolSource to the REAL
// ToolSessionContextStore the way the ACP runtime does, then drives a tool
// that emits attachment/reaction/speech side-effects. Every one of them must
// arrive on the canonical tool-event sink — the channel the ACP pool reads to
// build the persisted round and the turn mirror. This is the seam where the
// events were silently dropped (a guard that required a tool_call_id the
// turn-scoped side-effects never carry), and where read_media's image support
// must flow through to the gateway tool session.
func TestToolSideEffectsReachACPSink(t *testing.T) {
	store := mcp.NewToolSessionContextStore()

	session := mcp.ToolSessionContext{
		BotID:              testBotID,
		SessionID:          testSessionID,
		StreamID:           "stream-1",
		ToolCallID:         "call-1",
		SessionType:        "acp_agent",
		SupportsImageInput: true,
	}

	var mu sync.Mutex
	var received []mcp.ToolStreamEvent
	unregister := store.RegisterToolEventSink(session, func(event mcp.ToolStreamEvent) {
		mu.Lock()
		received = append(received, event)
		mu.Unlock()
	})
	defer unregister()

	source := agenttools.NewNativeToolSource(nil, []agenttools.ToolProvider{&sideEffectProvider{toolName: "make_image"}}, agenttools.NativeToolSourceOptions{
		AllowAll:   true,
		ToolEvents: store,
	})

	result, err := source.CallTool(context.Background(), session, "make_image", map[string]any{})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	// The gateway tool session must carry the ACP runtime's image-input
	// capability through to the tool.
	structured, _ := result["structuredContent"].(map[string]any)
	if structured == nil || structured["saw_image_input"] != true {
		t.Fatalf("tool did not observe SupportsImageInput through the gateway session: %#v", result)
	}

	mu.Lock()
	defer mu.Unlock()
	var sawAttachment, sawReaction, sawSpeech bool
	for _, event := range received {
		switch event.Type {
		case string(streamevent.Attachment):
			if len(event.Attachments) == 1 && event.Attachments[0].Path == "/data/shot.png" {
				sawAttachment = true
			}
		case string(streamevent.Reaction):
			if len(event.Reactions) == 1 && event.Reactions[0].Emoji == "✅" {
				sawReaction = true
			}
		case string(streamevent.Speech):
			if len(event.Speeches) == 1 && event.Speeches[0].Text == "all done" {
				sawSpeech = true
			}
		}
	}
	if !sawAttachment || !sawReaction || !sawSpeech {
		t.Fatalf("side-effects dropped at the ACP sink: attachment=%v reaction=%v speech=%v (received %d events)",
			sawAttachment, sawReaction, sawSpeech, len(received))
	}
}
