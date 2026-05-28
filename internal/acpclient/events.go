package acpclient

import (
	"fmt"
	"strings"
	"sync"

	acp "github.com/coder/acp-go-sdk"
)

type StreamEventType string

const (
	StreamEventTextDelta     StreamEventType = "text_delta"
	StreamEventToolCallStart StreamEventType = "tool_call_start"
	StreamEventToolCallEnd   StreamEventType = "tool_call_end"

	maxCollectedStreamEvents = 4096
	maxTrackedACPToolStates  = 1024
)

type StreamEvent struct {
	Type       StreamEventType `json:"type"`
	Delta      string          `json:"delta,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	ToolName   string          `json:"tool_name,omitempty"`
	Input      any             `json:"input,omitempty"`
	Result     any             `json:"result,omitempty"`
	Error      string          `json:"error,omitempty"`
}

type EventSink interface {
	EmitACPEvent(StreamEvent)
}

type EventSinkFunc func(StreamEvent)

func (f EventSinkFunc) EmitACPEvent(event StreamEvent) {
	if f != nil {
		f(event)
	}
}

type toolEventEmitter struct {
	mu        sync.RWMutex
	collector *eventCollector
	sink      EventSink
}

func (e *toolEventEmitter) setPromptState(collector *eventCollector, sink EventSink) {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.collector = collector
	e.sink = sink
	e.mu.Unlock()
}

func (e *toolEventEmitter) emit(event StreamEvent) {
	if e == nil {
		return
	}
	e.mu.RLock()
	collector := e.collector
	sink := e.sink
	e.mu.RUnlock()
	if collector != nil {
		collector.record(event)
	}
	if sink != nil {
		sink.EmitACPEvent(event)
	}
}

type eventCollector struct {
	mu     sync.Mutex
	text   strings.Builder
	events []StreamEvent
}

func newEventCollector() *eventCollector {
	return &eventCollector{}
}

func (c *eventCollector) record(event StreamEvent) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = appendBoundedStreamEvents(c.events, event)
}

func (c *eventCollector) apply(n acp.SessionNotification, events []StreamEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()

	update := n.Update
	c.events = appendBoundedStreamEvents(c.events, events...)
	if update.AgentMessageChunk != nil {
		c.text.WriteString(contentText(update.AgentMessageChunk.Content))
	}
}

func (c *eventCollector) result() RunResult {
	c.mu.Lock()
	defer c.mu.Unlock()

	events := append([]StreamEvent(nil), c.events...)
	return RunResult{
		Text:   strings.TrimSpace(c.text.String()),
		Events: events,
	}
}

func contentText(block acp.ContentBlock) string {
	if block.Text != nil {
		return block.Text.Text
	}
	if block.ResourceLink != nil {
		return block.ResourceLink.Uri
	}
	return ""
}

type acpToolEventMapper struct {
	mu    sync.Mutex
	tools map[string]*acpToolState
}

type acpToolState struct {
	id        string
	title     string
	kind      string
	status    string
	input     any
	output    any
	locations []acp.ToolCallLocation
	content   []acp.ToolCallContent
	name      string
	nativeIn  map[string]any
	started   bool
	done      bool
}

func newACPToolEventMapper() *acpToolEventMapper {
	return &acpToolEventMapper{tools: map[string]*acpToolState{}}
}

func (m *acpToolEventMapper) eventsFromNotification(n acp.SessionNotification) []StreamEvent {
	update := n.Update
	switch {
	case update.AgentMessageChunk != nil:
		text := contentText(update.AgentMessageChunk.Content)
		if text == "" {
			return nil
		}
		return []StreamEvent{{
			Type:  StreamEventTextDelta,
			Delta: text,
		}}
	case update.ToolCall != nil:
		return m.applyToolCall(*update.ToolCall)
	case update.ToolCallUpdate != nil:
		return m.applyToolUpdate(*update.ToolCallUpdate)
	default:
		return nil
	}
}

func (m *acpToolEventMapper) applyToolCall(tc acp.SessionUpdateToolCall) []StreamEvent {
	id := strings.TrimSpace(string(tc.ToolCallId))
	if id == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	state := m.ensureTool(id)
	state.title = strings.TrimSpace(tc.Title)
	state.kind = strings.TrimSpace(string(tc.Kind))
	state.status = strings.TrimSpace(string(tc.Status))
	state.input = tc.RawInput
	state.output = tc.RawOutput
	state.locations = append([]acp.ToolCallLocation(nil), tc.Locations...)
	state.content = append([]acp.ToolCallContent(nil), tc.Content...)
	return m.eventsForState(state)
}

func (m *acpToolEventMapper) applyToolUpdate(tc acp.SessionToolCallUpdate) []StreamEvent {
	id := strings.TrimSpace(string(tc.ToolCallId))
	if id == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	state := m.ensureTool(id)
	if tc.Title != nil {
		state.title = strings.TrimSpace(*tc.Title)
	}
	if tc.Kind != nil {
		state.kind = strings.TrimSpace(string(*tc.Kind))
	}
	if tc.Status != nil {
		state.status = strings.TrimSpace(string(*tc.Status))
	}
	if tc.RawInput != nil {
		state.input = tc.RawInput
	}
	if tc.RawOutput != nil {
		state.output = tc.RawOutput
	}
	if len(tc.Locations) > 0 {
		state.locations = append([]acp.ToolCallLocation(nil), tc.Locations...)
	}
	if len(tc.Content) > 0 {
		state.content = append([]acp.ToolCallContent(nil), tc.Content...)
	}
	return m.eventsForState(state)
}

func (m *acpToolEventMapper) ensureTool(id string) *acpToolState {
	state := m.tools[id]
	if state == nil {
		if len(m.tools) >= maxTrackedACPToolStates {
			for staleID := range m.tools {
				delete(m.tools, staleID)
				break
			}
		}
		state = &acpToolState{id: id}
		m.tools[id] = state
	}
	return state
}

func appendBoundedStreamEvents(events []StreamEvent, incoming ...StreamEvent) []StreamEvent {
	if len(incoming) == 0 {
		return events
	}
	events = append(events, incoming...)
	if len(events) <= maxCollectedStreamEvents {
		return events
	}
	return append([]StreamEvent(nil), events[len(events)-maxCollectedStreamEvents:]...)
}

func (m *acpToolEventMapper) eventsForState(state *acpToolState) []StreamEvent {
	name, input, ok := nativeToolFromACPState(state)
	if !ok {
		return nil
	}
	state.name = name
	state.nativeIn = input

	events := make([]StreamEvent, 0, 2)
	if !state.started {
		state.started = true
		events = append(events, StreamEvent{
			Type:       StreamEventToolCallStart,
			ToolCallID: state.id,
			ToolName:   state.name,
			Input:      state.nativeIn,
		})
	}
	if isTerminalACPToolStatus(state.status) && !state.done {
		state.done = true
		event := StreamEvent{
			Type:       StreamEventToolCallEnd,
			ToolCallID: state.id,
			ToolName:   state.name,
			Input:      state.nativeIn,
			Result:     nativeToolResultFromACPState(state),
		}
		if isFailedACPToolStatus(state.status) {
			event.Error = state.status
		}
		events = append(events, event)
		delete(m.tools, state.id)
	}
	return events
}

func nativeToolFromACPState(state *acpToolState) (string, map[string]any, bool) {
	if state == nil {
		return "", nil, false
	}
	switch strings.ToLower(strings.TrimSpace(state.kind)) {
	case string(acp.ToolKindExecute):
		command := commandFromACPInput(state.input)
		if command == "" {
			command = commandFromACPTitle(state.title)
		}
		if command == "" {
			return "", nil, false
		}
		return "exec", map[string]any{"command": command}, true
	case string(acp.ToolKindRead):
		path := pathFromACPInput(state.input)
		if path == "" && len(state.locations) > 0 {
			path = strings.TrimSpace(state.locations[0].Path)
		}
		if path == "" {
			return "", nil, false
		}
		return "read", map[string]any{"path": path}, true
	default:
		return "", nil, false
	}
}

func nativeToolResultFromACPState(state *acpToolState) any {
	if state == nil {
		return nil
	}
	result := normalizeACPToolOutput(state.output)
	if result == nil {
		if text := toolContentText(state.content); text != "" {
			result = map[string]any{"stdout": text}
		}
	}
	if result == nil {
		result = map[string]any{}
	}
	if isFailedACPToolStatus(state.status) {
		if m, ok := result.(map[string]any); ok {
			m["isError"] = true
			if _, ok := m["content"]; !ok {
				text := firstNonEmptyString(
					stringFromAny(m["stderr"]),
					stringFromAny(m["stdout"]),
					strings.TrimSpace(state.status),
				)
				m["content"] = []map[string]any{{"type": "text", "text": text}}
			}
		}
	}
	return result
}

func normalizeACPToolOutput(value any) any {
	switch v := value.(type) {
	case nil:
		return nil
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, val := range v {
			out[k] = val
		}
		if code, ok := numberFromAny(firstPresent(v, "exit_code", "exitCode", "code")); ok {
			out["exit_code"] = code
		}
		if stdout := firstNonEmptyRawString(
			rawStringFromAny(firstPresent(v, "stdout", "output", "text")),
			toolTextFromContentValue(v["content"]),
		); stdout != "" {
			out["stdout"] = stdout
		}
		if stderr := rawStringFromAny(firstPresent(v, "stderr", "error")); strings.TrimSpace(stderr) != "" {
			out["stderr"] = stderr
		}
		return out
	case string:
		if strings.TrimSpace(v) == "" {
			return nil
		}
		return map[string]any{"stdout": v}
	default:
		return value
	}
}

func commandFromACPInput(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			if s := stringFromAny(item); s != "" {
				parts = append(parts, shellQuoteIfNeeded(s))
			}
		}
		return strings.Join(parts, " ")
	case map[string]any:
		if cmd := firstNonEmptyString(
			stringFromAny(firstPresent(v, "command", "cmd", "shell_command", "shellCommand", "script")),
			commandFromACPInput(v["argv"]),
			commandFromACPInput(v["args"]),
		); cmd != "" {
			return cmd
		}
	}
	return ""
}

func commandFromACPTitle(title string) string {
	title = strings.TrimSpace(title)
	switch strings.ToLower(title) {
	case "", "shell", "command", "run command", "execute", "exec":
		return ""
	default:
		return title
	}
}

func pathFromACPInput(value any) string {
	if m, ok := value.(map[string]any); ok {
		return stringFromAny(firstPresent(m, "path", "file", "filename"))
	}
	return ""
}

func toolContentText(contents []acp.ToolCallContent) string {
	if len(contents) == 0 {
		return ""
	}
	lines := make([]string, 0, len(contents))
	for _, item := range contents {
		if item.Content != nil {
			if text := contentText(item.Content.Content); text != "" {
				lines = append(lines, text)
			}
		}
		if item.Diff != nil {
			if item.Diff.Path != "" {
				lines = append(lines, item.Diff.Path)
			}
		}
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func toolTextFromContentValue(value any) string {
	items, ok := value.([]any)
	if !ok {
		return ""
	}
	lines := make([]string, 0, len(items))
	for _, raw := range items {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if strings.EqualFold(stringFromAny(m["type"]), "text") {
			if text := stringFromAny(m["text"]); text != "" {
				lines = append(lines, text)
			}
		}
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func isTerminalACPToolStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "completed", "complete", "done", "failed", "error", "cancelled", "canceled":
		return true
	default:
		return false
	}
}

func isFailedACPToolStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "failed", "error", "cancelled", "canceled":
		return true
	default:
		return false
	}
}

func firstPresent(m map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := m[key]; ok {
			return value
		}
	}
	return nil
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func firstNonEmptyRawString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func stringFromAny(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case fmt.Stringer:
		return strings.TrimSpace(v.String())
	default:
		return ""
	}
}

func rawStringFromAny(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	return stringFromAny(value)
}

func numberFromAny(value any) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, true
	case int32:
		return int(v), true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	case float32:
		return int(v), true
	default:
		return 0, false
	}
}

func shellQuoteIfNeeded(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if strings.ContainsAny(value, " \t\n'\"$`\\") {
		return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
	}
	return value
}
