package acpclient

import (
	"strconv"
	"testing"

	acp "github.com/coder/acp-go-sdk"
)

func TestACPGenericExecuteToolMapsToNativeExecEvents(t *testing.T) {
	t.Parallel()

	mapper := newACPToolEventMapper()
	start := mapper.eventsFromNotification(acp.SessionNotification{
		Update: acp.StartToolCall(
			acp.ToolCallId("call-1"),
			"Shell",
			acp.WithStartKind(acp.ToolKindExecute),
			acp.WithStartStatus(acp.ToolCallStatusInProgress),
			acp.WithStartRawInput(map[string]any{"command": "date '+%Y-%m-%d %H:%M:%S %Z'"}),
		),
	})
	if len(start) != 1 {
		t.Fatalf("start events = %#v, want 1", start)
	}
	if start[0].Type != StreamEventToolCallStart || start[0].ToolName != "exec" || start[0].ToolCallID != "call-1" {
		t.Fatalf("start event = %#v, want native exec start", start[0])
	}
	input, ok := start[0].Input.(map[string]any)
	if !ok || input["command"] != "date '+%Y-%m-%d %H:%M:%S %Z'" {
		t.Fatalf("start input = %#v", start[0].Input)
	}

	end := mapper.eventsFromNotification(acp.SessionNotification{
		Update: acp.UpdateToolCall(
			acp.ToolCallId("call-1"),
			acp.WithUpdateStatus(acp.ToolCallStatusCompleted),
			acp.WithUpdateRawOutput(map[string]any{
				"stdout":    "2026-05-28 14:51:39 UTC\n",
				"exit_code": 0,
			}),
		),
	})
	if len(end) != 1 {
		t.Fatalf("end events = %#v, want 1", end)
	}
	if end[0].Type != StreamEventToolCallEnd || end[0].ToolName != "exec" || end[0].Error != "" {
		t.Fatalf("end event = %#v, want native exec end", end[0])
	}
	result, ok := end[0].Result.(map[string]any)
	if !ok {
		t.Fatalf("end result = %#v, want object", end[0].Result)
	}
	if result["stdout"] != "2026-05-28 14:51:39 UTC\n" || result["exit_code"] != 0 {
		t.Fatalf("end result = %#v", result)
	}
}

func TestACPGenericExecuteCompletionWithoutStartEmitsStartThenEnd(t *testing.T) {
	t.Parallel()

	mapper := newACPToolEventMapper()
	events := mapper.eventsFromNotification(acp.SessionNotification{
		Update: acp.UpdateToolCall(
			acp.ToolCallId("call-1"),
			acp.WithUpdateKind(acp.ToolKindExecute),
			acp.WithUpdateStatus(acp.ToolCallStatusCompleted),
			acp.WithUpdateRawInput(map[string]any{"cmd": "pwd"}),
			acp.WithUpdateRawOutput("workspace\n"),
		),
	})
	if len(events) != 2 {
		t.Fatalf("events = %#v, want start + end", events)
	}
	if events[0].Type != StreamEventToolCallStart || events[0].ToolName != "exec" {
		t.Fatalf("first event = %#v, want exec start", events[0])
	}
	if events[1].Type != StreamEventToolCallEnd || events[1].ToolName != "exec" {
		t.Fatalf("second event = %#v, want exec end", events[1])
	}
	result, ok := events[1].Result.(map[string]any)
	if !ok || result["stdout"] != "workspace\n" {
		t.Fatalf("end result = %#v", events[1].Result)
	}
}

func TestEventCollectorBoundsStoredEvents(t *testing.T) {
	t.Parallel()

	collector := newEventCollector()
	for i := 0; i < maxCollectedStreamEvents+10; i++ {
		collector.record(StreamEvent{
			Type:       StreamEventToolCallStart,
			ToolCallID: string(rune('a' + (i % 26))),
			ToolName:   "exec",
		})
	}

	result := collector.result()
	if len(result.Events) != maxCollectedStreamEvents {
		t.Fatalf("stored events = %d, want %d", len(result.Events), maxCollectedStreamEvents)
	}
}

func TestACPToolEventMapperBoundsTrackedTools(t *testing.T) {
	t.Parallel()

	mapper := newACPToolEventMapper()
	for i := 0; i < maxTrackedACPToolStates+10; i++ {
		_ = mapper.eventsFromNotification(acp.SessionNotification{
			Update: acp.StartToolCall(
				acp.ToolCallId("call-"+strconv.Itoa(i)),
				"Shell",
				acp.WithStartKind(acp.ToolKindExecute),
				acp.WithStartStatus(acp.ToolCallStatusInProgress),
				acp.WithStartRawInput(map[string]any{"command": "pwd"}),
			),
		})
	}
	if len(mapper.tools) > maxTrackedACPToolStates {
		t.Fatalf("tracked tools = %d, want <= %d", len(mapper.tools), maxTrackedACPToolStates)
	}
}
