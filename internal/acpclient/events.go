package acpclient

import (
	"strings"
	"sync"

	acp "github.com/coder/acp-go-sdk"
)

type StreamEventType string

const (
	StreamEventTextDelta  StreamEventType = "text_delta"
	StreamEventToolStart  StreamEventType = "tool_start"
	StreamEventToolUpdate StreamEventType = "tool_update"
	StreamEventPlan       StreamEventType = "plan"
)

type StreamEvent struct {
	Type  StreamEventType `json:"type"`
	Delta string          `json:"delta,omitempty"`
	Tool  ToolSummary     `json:"tool,omitempty"`
	Plan  []PlanItem      `json:"plan,omitempty"`
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

type ToolSummary struct {
	ID     string `json:"id,omitempty"`
	Title  string `json:"title,omitempty"`
	Kind   string `json:"kind,omitempty"`
	Status string `json:"status,omitempty"`
}

type PlanItem struct {
	Content  string `json:"content"`
	Status   string `json:"status,omitempty"`
	Priority string `json:"priority,omitempty"`
}

type eventCollector struct {
	mu        sync.Mutex
	text      strings.Builder
	toolOrder []string
	tools     map[string]ToolSummary
	plan      []PlanItem
	events    []StreamEvent
}

func newEventCollector() *eventCollector {
	return &eventCollector{tools: map[string]ToolSummary{}}
}

func (c *eventCollector) apply(n acp.SessionNotification) {
	c.mu.Lock()
	defer c.mu.Unlock()

	update := n.Update
	c.events = append(c.events, streamEventsFromNotification(n)...)
	switch {
	case update.AgentMessageChunk != nil:
		c.text.WriteString(contentText(update.AgentMessageChunk.Content))
	case update.ToolCall != nil:
		tc := update.ToolCall
		c.upsertTool(string(tc.ToolCallId), tc.Title, string(tc.Kind), string(tc.Status))
	case update.ToolCallUpdate != nil:
		tc := update.ToolCallUpdate
		title := ""
		if tc.Title != nil {
			title = *tc.Title
		}
		kind := ""
		if tc.Kind != nil {
			kind = string(*tc.Kind)
		}
		status := ""
		if tc.Status != nil {
			status = string(*tc.Status)
		}
		c.upsertTool(string(tc.ToolCallId), title, kind, status)
	case update.Plan != nil:
		c.plan = make([]PlanItem, 0, len(update.Plan.Entries))
		for _, entry := range update.Plan.Entries {
			c.plan = append(c.plan, PlanItem{
				Content:  entry.Content,
				Status:   string(entry.Status),
				Priority: string(entry.Priority),
			})
		}
	}
}

func (c *eventCollector) result() RunResult {
	c.mu.Lock()
	defer c.mu.Unlock()

	tools := make([]ToolSummary, 0, len(c.toolOrder))
	for _, id := range c.toolOrder {
		tools = append(tools, c.tools[id])
	}
	plan := append([]PlanItem(nil), c.plan...)
	events := append([]StreamEvent(nil), c.events...)
	return RunResult{
		Text:      strings.TrimSpace(c.text.String()),
		ToolCalls: tools,
		Plan:      plan,
		Events:    events,
	}
}

func (c *eventCollector) upsertTool(id, title, kind, status string) {
	if id == "" {
		return
	}
	current, ok := c.tools[id]
	if !ok {
		c.toolOrder = append(c.toolOrder, id)
		current.ID = id
	}
	if strings.TrimSpace(title) != "" {
		current.Title = strings.TrimSpace(title)
	}
	if strings.TrimSpace(kind) != "" {
		current.Kind = strings.TrimSpace(kind)
	}
	if strings.TrimSpace(status) != "" {
		current.Status = strings.TrimSpace(status)
	}
	c.tools[id] = current
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

func streamEventsFromNotification(n acp.SessionNotification) []StreamEvent {
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
		tc := update.ToolCall
		return []StreamEvent{{
			Type: StreamEventToolStart,
			Tool: ToolSummary{
				ID:     string(tc.ToolCallId),
				Title:  strings.TrimSpace(tc.Title),
				Kind:   string(tc.Kind),
				Status: string(tc.Status),
			},
		}}
	case update.ToolCallUpdate != nil:
		tc := update.ToolCallUpdate
		title := ""
		if tc.Title != nil {
			title = strings.TrimSpace(*tc.Title)
		}
		status := ""
		if tc.Status != nil {
			status = string(*tc.Status)
		}
		kind := ""
		if tc.Kind != nil {
			kind = string(*tc.Kind)
		}
		return []StreamEvent{{
			Type: StreamEventToolUpdate,
			Tool: ToolSummary{
				ID:     string(tc.ToolCallId),
				Title:  title,
				Kind:   kind,
				Status: status,
			},
		}}
	case update.Plan != nil:
		plan := make([]PlanItem, 0, len(update.Plan.Entries))
		for _, entry := range update.Plan.Entries {
			plan = append(plan, PlanItem{
				Content:  entry.Content,
				Status:   string(entry.Status),
				Priority: string(entry.Priority),
			})
		}
		if len(plan) == 0 {
			return nil
		}
		return []StreamEvent{{
			Type: StreamEventPlan,
			Plan: plan,
		}}
	default:
		return nil
	}
}
