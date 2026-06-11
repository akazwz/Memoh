package flow_test

import (
	"reflect"
	"testing"

	"github.com/memohai/memoh/internal/acpclient"
	agentpkg "github.com/memohai/memoh/internal/agent"
	"github.com/memohai/memoh/internal/conversation"
	"github.com/memohai/memoh/internal/conversation/flow"
	"github.com/memohai/memoh/internal/streamevent"
)

// The parity suite locks the native and ACP pipelines together: the same
// logical tool-call scenario, expressed once as native agent events and once
// as ACP events run through the real conversion, must render identical
// UIMessages. When one pipeline learns a behavior the other doesn't, these
// tests fail instead of users discovering the drift.

// Vocabulary parity needs no test anymore: acpclient.StreamEventType aliases
// streamevent.Type, so an ACP event type IS an agent event type by
// construction.

// TestACPEventFieldParity asserts the conversion is total: every populated
// field survives the crossing.
func TestACPEventFieldParity(t *testing.T) {
	t.Parallel()

	event := acpclient.StreamEvent{
		Type:        acpclient.StreamEventToolApprovalRequest,
		Delta:       "delta",
		ToolCallID:  "call-1",
		ToolName:    "exec",
		Input:       map[string]any{"command": "pwd"},
		Result:      map[string]any{"stdout": "/data"},
		Error:       "boom",
		ApprovalID:  "approval-1",
		UserInputID: "input-1",
		ShortID:     9,
		Status:      "pending",
		Metadata:    map[string]any{"approval": map[string]any{"approval_id": "approval-1"}},
		Attachments: []streamevent.FileAttachment{{Type: "image", Path: "/data/shot.png", Mime: "image/png"}},
		Reactions:   []streamevent.ReactionItem{{Emoji: "👍"}},
		Speeches:    []streamevent.SpeechItem{{Text: "done"}},
	}
	got, ok := flow.MapACPStreamEvent(event)
	if !ok {
		t.Fatal("event did not map")
	}
	want := agentpkg.StreamEvent{
		Type:        agentpkg.EventToolApprovalRequest,
		Delta:       event.Delta,
		ToolCallID:  event.ToolCallID,
		ToolName:    event.ToolName,
		Input:       event.Input,
		Result:      event.Result,
		Error:       event.Error,
		ApprovalID:  event.ApprovalID,
		UserInputID: event.UserInputID,
		ShortID:     event.ShortID,
		Status:      event.Status,
		Metadata:    event.Metadata,
		Attachments: event.Attachments,
		Reactions:   event.Reactions,
		Speeches:    event.Speeches,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mapped event = %#v\nwant %#v", got, want)
	}
}

type parityScenarioEvent struct {
	native agentpkg.StreamEvent
	acp    acpclient.StreamEvent
}

// approvalToolScenario is one full tool-call lifecycle: approval requested,
// decision made, tool started, tool finished, answer text streamed.
func approvalToolScenario(decision string) []parityScenarioEvent {
	approvalMeta := func(status string) map[string]any {
		return map[string]any{
			"approval": map[string]any{
				"approval_id": "approval-1",
				"short_id":    7,
				"status":      status,
				"can_approve": status == "pending",
			},
		}
	}
	input := map[string]any{"command": "pwd"}
	result := map[string]any{"stdout": "/data"}
	return []parityScenarioEvent{
		{
			native: agentpkg.StreamEvent{Type: agentpkg.EventToolApprovalRequest, ToolCallID: "call-1", ToolName: "exec", Input: input, ApprovalID: "approval-1", ShortID: 7, Status: "pending", Metadata: approvalMeta("pending")},
			acp:    acpclient.StreamEvent{Type: acpclient.StreamEventToolApprovalRequest, ToolCallID: "call-1", ToolName: "exec", Input: input, ApprovalID: "approval-1", ShortID: 7, Status: "pending", Metadata: approvalMeta("pending")},
		},
		{
			native: agentpkg.StreamEvent{Type: agentpkg.EventToolApprovalRequest, ToolCallID: "call-1", ToolName: "exec", Input: input, ApprovalID: "approval-1", ShortID: 7, Status: decision, Metadata: approvalMeta(decision)},
			acp:    acpclient.StreamEvent{Type: acpclient.StreamEventToolApprovalRequest, ToolCallID: "call-1", ToolName: "exec", Input: input, ApprovalID: "approval-1", ShortID: 7, Status: decision, Metadata: approvalMeta(decision)},
		},
		{
			native: agentpkg.StreamEvent{Type: agentpkg.EventToolCallStart, ToolCallID: "call-1", ToolName: "exec", Input: input},
			acp:    acpclient.StreamEvent{Type: acpclient.StreamEventToolCallStart, ToolCallID: "call-1", ToolName: "exec", Input: input},
		},
		{
			native: agentpkg.StreamEvent{Type: agentpkg.EventToolCallEnd, ToolCallID: "call-1", ToolName: "exec", Input: input, Result: result},
			acp:    acpclient.StreamEvent{Type: acpclient.StreamEventToolCallEnd, ToolCallID: "call-1", ToolName: "exec", Input: input, Result: result},
		},
		{
			native: agentpkg.StreamEvent{Type: agentpkg.EventTextDelta, Delta: "done"},
			acp:    acpclient.StreamEvent{Type: acpclient.StreamEventTextDelta, Delta: "done"},
		},
	}
}

// TestNativeACPPipelineParityCancelledTurn locks the cancelled path: a turn
// streams reasoning, partial text, and an in-flight tool, then the user
// aborts. The ACP pipeline synthesizes text_end before agent_abort
// (resolver_acp.go); the native pipeline aborts directly — the rendered UI
// must not diverge, and the partial output must survive the abort.
func TestNativeACPPipelineParityCancelledTurn(t *testing.T) {
	t.Parallel()

	input := map[string]any{"command": "sleep 100"}
	common := []parityScenarioEvent{
		{
			native: agentpkg.StreamEvent{Type: agentpkg.EventReasoningDelta, Delta: "let me check"},
			acp:    acpclient.StreamEvent{Type: acpclient.StreamEventReasoningDelta, Delta: "let me check"},
		},
		{
			native: agentpkg.StreamEvent{Type: agentpkg.EventTextDelta, Delta: "partial answ"},
			acp:    acpclient.StreamEvent{Type: acpclient.StreamEventTextDelta, Delta: "partial answ"},
		},
		{
			native: agentpkg.StreamEvent{Type: agentpkg.EventToolCallStart, ToolCallID: "call-1", ToolName: "exec", Input: input},
			acp:    acpclient.StreamEvent{Type: acpclient.StreamEventToolCallStart, ToolCallID: "call-1", ToolName: "exec", Input: input},
		},
	}

	nativeEvents := make([]agentpkg.StreamEvent, 0, len(common)+1)
	acpEvents := make([]agentpkg.StreamEvent, 0, len(common)+2)
	for _, step := range common {
		nativeEvents = append(nativeEvents, step.native)
		if mapped, ok := flow.MapACPStreamEvent(step.acp); ok {
			acpEvents = append(acpEvents, mapped)
		}
	}
	// Pipeline-specific terminal tails on abort.
	nativeEvents = append(nativeEvents, agentpkg.StreamEvent{Type: agentpkg.EventAgentAbort})
	acpEvents = append(acpEvents,
		agentpkg.StreamEvent{Type: agentpkg.EventTextEnd},
		agentpkg.StreamEvent{Type: agentpkg.EventAgentAbort},
	)

	nativeMessages := renderNative(t, nativeEvents)
	acpMessages := renderNative(t, acpEvents)
	if !reflect.DeepEqual(nativeMessages, acpMessages) {
		t.Fatalf("cancelled-turn pipelines diverged:\nnative=%#v\nacp=%#v", nativeMessages, acpMessages)
	}

	// The abort must not eat partial output: text and reasoning blocks stay.
	var sawText, sawReasoning bool
	for _, msg := range acpMessages {
		switch msg.Type {
		case conversation.UIMessageText:
			if msg.Content == "partial answ" {
				sawText = true
			}
		case conversation.UIMessageReasoning:
			if msg.Content == "let me check" {
				sawReasoning = true
			}
		}
	}
	if !sawText || !sawReasoning {
		t.Fatalf("cancelled turn lost partial output: text=%v reasoning=%v messages=%#v", sawText, sawReasoning, acpMessages)
	}
}

// TestNativeACPPipelineParitySideEffects locks the side-effect parity: a tool
// attachment renders to the same UI message whether it came from the native
// agent loop or an ACP tool over MCP, and reaction/speech are transient in
// BOTH pipelines (the UI converter has no case for them — they are outbound
// platform effects, not conversation messages). Persistence is intentionally
// out of scope: the native WS path does not persist tool side-effect
// attachments either (that rides the channel-adapter OutboundAssetCollector),
// so ACP not persisting them is the correct parity, not a gap.
func TestNativeACPPipelineParitySideEffects(t *testing.T) {
	t.Parallel()

	attachment := streamevent.FileAttachment{Type: "image", Path: "/data/out.png", Mime: "image/png", Name: "out.png"}

	nativeAttach := agentpkg.StreamEvent{Type: agentpkg.EventAttachment, Attachments: []streamevent.FileAttachment{attachment}}
	mappedAttach, ok := flow.MapACPStreamEvent(acpclient.StreamEvent{
		Type:        acpclient.StreamEventAttachment,
		Attachments: []streamevent.FileAttachment{attachment},
	})
	if !ok {
		t.Fatal("attachment event did not map")
	}

	nativeMsgs := renderNative(t, []agentpkg.StreamEvent{nativeAttach})
	acpMsgs := renderNative(t, []agentpkg.StreamEvent{mappedAttach})
	if !reflect.DeepEqual(nativeMsgs, acpMsgs) {
		t.Fatalf("attachment pipelines diverged:\nnative=%#v\nacp=%#v", nativeMsgs, acpMsgs)
	}
	var sawAttachmentMsg bool
	for _, msg := range acpMsgs {
		if msg.Type == conversation.UIMessageAttachments && len(msg.Attachments) == 1 && msg.Attachments[0].Name == "out.png" {
			sawAttachmentMsg = true
		}
	}
	if !sawAttachmentMsg {
		t.Fatalf("attachment did not render to a UI attachments message: %#v", acpMsgs)
	}

	// reaction/speech: transient in both pipelines (no UI message produced).
	for _, sideEffect := range []agentpkg.StreamEvent{
		{Type: agentpkg.EventReaction, Reactions: []streamevent.ReactionItem{{Emoji: "👍"}}},
		{Type: agentpkg.EventSpeech, Speeches: []streamevent.SpeechItem{{Text: "hi"}}},
	} {
		if msgs := renderNative(t, []agentpkg.StreamEvent{sideEffect}); len(msgs) != 0 {
			t.Fatalf("%s produced UI messages (%d); it must be transient like the native pipeline", sideEffect.Type, len(msgs))
		}
	}
}

func renderNative(t *testing.T, events []agentpkg.StreamEvent) map[int]conversation.UIMessage {
	t.Helper()
	converter := conversation.NewUIMessageStreamConverter()
	byID := map[int]conversation.UIMessage{}
	for _, event := range events {
		for _, msg := range converter.HandleEvent(conversation.UIStreamEventFromAgent(event)) {
			byID[msg.ID] = msg
		}
	}
	return byID
}

// TestNativeACPPipelineParity renders the same scenario through both
// pipelines and asserts the final UI messages are identical.
func TestNativeACPPipelineParity(t *testing.T) {
	t.Parallel()

	for _, decision := range []string{"approved", "rejected"} {
		t.Run(decision, func(t *testing.T) {
			t.Parallel()
			scenario := approvalToolScenario(decision)

			nativeEvents := make([]agentpkg.StreamEvent, 0, len(scenario))
			acpEvents := make([]agentpkg.StreamEvent, 0, len(scenario))
			for _, step := range scenario {
				nativeEvents = append(nativeEvents, step.native)
				if mapped, ok := flow.MapACPStreamEvent(step.acp); ok {
					acpEvents = append(acpEvents, mapped)
				}
			}

			nativeMessages := renderNative(t, nativeEvents)
			acpMessages := renderNative(t, acpEvents)

			if !reflect.DeepEqual(nativeMessages, acpMessages) {
				t.Fatalf("pipelines diverged:\nnative=%#v\nacp=%#v", nativeMessages, acpMessages)
			}

			// The scenario must actually exercise the approval surface: the
			// tool block carries the decision and can_approve is closed.
			var toolMsg *conversation.UIMessage
			for id := range nativeMessages {
				if msg := nativeMessages[id]; msg.Type == conversation.UIMessageTool {
					if toolMsg != nil {
						t.Fatalf("scenario rendered more than one tool block: %#v", nativeMessages)
					}
					toolMsg = &msg
				}
			}
			if toolMsg == nil || toolMsg.Approval == nil {
				t.Fatalf("scenario rendered no tool approval block: %#v", nativeMessages)
			}
			if toolMsg.Approval.Status != decision || toolMsg.Approval.CanApprove {
				t.Fatalf("tool approval state = %#v, want %s/can_approve=false", toolMsg.Approval, decision)
			}
		})
	}
}
