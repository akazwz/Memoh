package conversation

import "strings"

type uiTextStreamState struct {
	ID       int
	Content  string
	Metadata map[string]any
}

type uiToolStreamState struct {
	Message UIMessage
}

// UIMessageStreamConverter converts low-level stream events into complete UI messages.
type UIMessageStreamConverter struct {
	nextID    int
	text      *uiTextStreamState
	reasoning *uiTextStreamState
	tools     map[string]*uiToolStreamState
}

// NewUIMessageStreamConverter creates a new UI stream converter.
func NewUIMessageStreamConverter() *UIMessageStreamConverter {
	return &UIMessageStreamConverter{
		tools: map[string]*uiToolStreamState{},
	}
}

// HandleEvent updates converter state and returns zero or one complete UI messages.
func (c *UIMessageStreamConverter) HandleEvent(event UIMessageStreamEvent) []UIMessage {
	switch strings.ToLower(strings.TrimSpace(event.Type)) {
	case "text_start":
		c.text = &uiTextStreamState{ID: c.nextMessageID()}
		return nil

	case "text_delta":
		if c.text == nil {
			c.text = &uiTextStreamState{ID: c.nextMessageID()}
		}
		if len(event.Metadata) > 0 {
			c.text.Metadata = cloneUIMetadata(event.Metadata)
		}
		c.text.Content += event.Delta
		return []UIMessage{{
			ID:       c.text.ID,
			Type:     UIMessageText,
			Content:  c.text.Content,
			Metadata: cloneUIMetadata(c.text.Metadata),
		}}

	case "text_end":
		c.text = nil
		return nil

	case "reasoning_start":
		c.reasoning = &uiTextStreamState{ID: c.nextMessageID()}
		return nil

	case "reasoning_delta":
		if c.reasoning == nil {
			c.reasoning = &uiTextStreamState{ID: c.nextMessageID()}
		}
		if len(event.Metadata) > 0 {
			c.reasoning.Metadata = cloneUIMetadata(event.Metadata)
		}
		c.reasoning.Content += event.Delta
		return []UIMessage{{
			ID:       c.reasoning.ID,
			Type:     UIMessageReasoning,
			Content:  c.reasoning.Content,
			Metadata: cloneUIMetadata(c.reasoning.Metadata),
		}}

	case "reasoning_end":
		c.reasoning = nil
		return nil

	case "tool_call_start":
		state := c.findToolState(event.ToolCallID, event.ToolName)
		if state == nil {
			state = &uiToolStreamState{
				Message: UIMessage{
					ID:         c.nextMessageID(),
					Type:       UIMessageTool,
					Name:       strings.TrimSpace(event.ToolName),
					Input:      event.Input,
					ToolCallID: strings.TrimSpace(event.ToolCallID),
					Running:    uiBoolPtr(true),
				},
			}
		}
		if trimmed := strings.TrimSpace(event.ToolName); trimmed != "" {
			state.Message.Name = trimmed
		}
		if event.Input != nil {
			state.Message.Input = event.Input
		}
		if len(event.Metadata) > 0 {
			state.Message.Metadata = cloneUIMetadata(event.Metadata)
		}
		if trimmed := strings.TrimSpace(event.ToolCallID); trimmed != "" {
			state.Message.ToolCallID = trimmed
			c.tools[trimmed] = state
		}
		state.Message.Running = uiBoolPtr(true)
		c.text = nil
		return []UIMessage{cloneToolStreamMessage(state.Message)}

	case "tool_call_progress":
		state := c.findToolState(event.ToolCallID, event.ToolName)
		if state == nil {
			state = &uiToolStreamState{
				Message: UIMessage{
					ID:         c.nextMessageID(),
					Type:       UIMessageTool,
					Name:       strings.TrimSpace(event.ToolName),
					Input:      event.Input,
					ToolCallID: strings.TrimSpace(event.ToolCallID),
					Running:    uiBoolPtr(true),
				},
			}
			if state.Message.ToolCallID != "" {
				c.tools[state.Message.ToolCallID] = state
			}
		}
		state.Message.Progress = append(state.Message.Progress, event.Progress)
		if event.Input != nil {
			state.Message.Input = event.Input
		}
		if len(event.Metadata) > 0 {
			state.Message.Metadata = cloneUIMetadata(event.Metadata)
		}
		return []UIMessage{cloneToolStreamMessage(state.Message)}

	case "tool_approval_request":
		state := c.findToolState(event.ToolCallID, event.ToolName)
		if state == nil {
			state = &uiToolStreamState{
				Message: UIMessage{
					ID:         c.nextMessageID(),
					Type:       UIMessageTool,
					Name:       strings.TrimSpace(event.ToolName),
					Input:      event.Input,
					ToolCallID: strings.TrimSpace(event.ToolCallID),
				},
			}
			if state.Message.ToolCallID != "" {
				c.tools[state.Message.ToolCallID] = state
			}
		}
		if event.Input != nil {
			state.Message.Input = event.Input
		}
		if len(event.Metadata) > 0 {
			state.Message.Metadata = cloneUIMetadata(event.Metadata)
		}
		if trimmed := strings.TrimSpace(event.ToolName); trimmed != "" {
			state.Message.Name = trimmed
		}
		if trimmed := strings.TrimSpace(event.ToolCallID); trimmed != "" {
			state.Message.ToolCallID = trimmed
			c.tools[trimmed] = state
		}
		status := strings.TrimSpace(event.Status)
		if status == "" {
			status = "pending"
		}
		state.Message.Running = uiBoolPtr(false)
		state.Message.Approval = &UIToolApproval{
			ApprovalID: strings.TrimSpace(event.ApprovalID),
			ShortID:    event.ShortID,
			Status:     status,
			CanApprove: true,
		}
		return []UIMessage{cloneToolStreamMessage(state.Message)}

	case "tool_call_end":
		state := c.findToolState(event.ToolCallID, event.ToolName)
		if state == nil {
			state = &uiToolStreamState{
				Message: UIMessage{
					ID:         c.nextMessageID(),
					Type:       UIMessageTool,
					Name:       strings.TrimSpace(event.ToolName),
					Input:      event.Input,
					ToolCallID: strings.TrimSpace(event.ToolCallID),
				},
			}
		}
		if event.Input != nil {
			state.Message.Input = event.Input
		}
		if len(event.Metadata) > 0 {
			state.Message.Metadata = cloneUIMetadata(event.Metadata)
		}
		applyToolResultToUIMessage(&state.Message, event.Output)
		if state.Message.ToolCallID != "" && !isBackgroundToolStillRunning(state.Message) {
			delete(c.tools, state.Message.ToolCallID)
		}
		return []UIMessage{cloneToolStreamMessage(state.Message)}

	case "attachment_delta":
		if len(event.Attachments) == 0 {
			return nil
		}
		return []UIMessage{{
			ID:          c.nextMessageID(),
			Type:        UIMessageAttachments,
			Attachments: append([]UIAttachment(nil), event.Attachments...),
		}}

	default:
		return nil
	}
}

func (c *UIMessageStreamConverter) nextMessageID() int {
	id := c.nextID
	c.nextID++
	return id
}

func (c *UIMessageStreamConverter) findToolState(toolCallID, toolName string) *uiToolStreamState {
	if trimmed := strings.TrimSpace(toolCallID); trimmed != "" {
		if state, ok := c.tools[trimmed]; ok {
			return state
		}
		// An explicit but unknown tool_call_id means this is a new call,
		// not a continuation of an in-flight one. Falling back to a
		// name-based match here would merge unrelated calls of the same
		// tool (e.g. three sequential `search` invocations) into one UI
		// message, which is exactly what we want to avoid.
		return nil
	}

	normalizedName := strings.TrimSpace(toolName)
	for _, state := range c.tools {
		if strings.TrimSpace(state.Message.Name) == normalizedName {
			return state
		}
	}
	return nil
}

func cloneToolStreamMessage(message UIMessage) UIMessage {
	clone := message
	if len(message.Progress) > 0 {
		clone.Progress = append([]any(nil), message.Progress...)
	}
	if len(message.Metadata) > 0 {
		clone.Metadata = cloneUIMetadata(message.Metadata)
	}
	return clone
}

func cloneUIMetadata(metadata map[string]any) map[string]any {
	if len(metadata) == 0 {
		return nil
	}
	clone := make(map[string]any, len(metadata))
	for key, value := range metadata {
		clone[key] = value
	}
	return clone
}
