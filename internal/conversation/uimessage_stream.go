package conversation

import (
	"strings"

	"github.com/memohai/memoh/internal/streamevent"
	"github.com/memohai/memoh/internal/toolapproval"
)

type uiTextStreamState struct {
	ID      int
	Content string
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
	switch streamevent.Type(strings.ToLower(strings.TrimSpace(string(event.Type)))) {
	case streamevent.TextStart:
		c.text = &uiTextStreamState{ID: c.nextMessageID()}
		return nil

	case streamevent.TextDelta:
		if c.text == nil {
			c.text = &uiTextStreamState{ID: c.nextMessageID()}
		}
		c.text.Content += event.Delta
		return []UIMessage{{
			ID:      c.text.ID,
			Type:    UIMessageText,
			Content: c.text.Content,
		}}

	case streamevent.TextEnd:
		c.text = nil
		return nil

	case streamevent.ReasoningStart:
		c.reasoning = &uiTextStreamState{ID: c.nextMessageID()}
		return nil

	case streamevent.ReasoningDelta:
		if c.reasoning == nil {
			c.reasoning = &uiTextStreamState{ID: c.nextMessageID()}
		}
		c.reasoning.Content += event.Delta
		return []UIMessage{{
			ID:      c.reasoning.ID,
			Type:    UIMessageReasoning,
			Content: c.reasoning.Content,
		}}

	case streamevent.ReasoningEnd:
		c.reasoning = nil
		return nil

	case streamevent.ToolCallStart, streamevent.ToolCallInputStart:
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
		if trimmed := strings.TrimSpace(event.ToolCallID); trimmed != "" {
			state.Message.ToolCallID = trimmed
			c.tools[trimmed] = state
		}
		state.Message.Running = uiBoolPtr(true)
		c.text = nil
		return []UIMessage{cloneToolStreamMessage(state.Message)}

	case streamevent.ToolCallProgress:
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
		return []UIMessage{cloneToolStreamMessage(state.Message)}

	case streamevent.ToolApprovalRequest:
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
		if trimmed := strings.TrimSpace(event.ToolName); trimmed != "" {
			state.Message.Name = trimmed
		}
		if trimmed := strings.TrimSpace(event.ToolCallID); trimmed != "" {
			state.Message.ToolCallID = trimmed
			c.tools[trimmed] = state
		}
		status := toolapproval.NormalizedStatus(event.Status)
		state.Message.Running = uiBoolPtr(false)
		state.Message.Approval = &UIToolApproval{
			ApprovalID: strings.TrimSpace(event.ApprovalID),
			ShortID:    event.ShortID,
			Status:     status,
			CanApprove: toolapproval.CanApprove(status),
		}
		return []UIMessage{cloneToolStreamMessage(state.Message)}

	case streamevent.UserInputRequest:
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
		userInputID := strings.TrimSpace(event.UserInputID)
		if userInputID == "" {
			userInputID = stringFromAny(event.Metadata["user_input_id"])
		}
		state.Message.Running = uiBoolPtr(false)
		state.Message.UserInput = uiUserInputFromPayload(
			userInputID,
			event.ShortID,
			status,
			event.Metadata["ui_payload"],
			status == "pending",
		)
		return []UIMessage{cloneToolStreamMessage(state.Message)}

	case streamevent.ToolCallEnd:
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
		applyToolResultToUIMessage(&state.Message, event.Output)
		if state.Message.ToolCallID != "" && !isBackgroundToolStillRunning(state.Message) {
			delete(c.tools, state.Message.ToolCallID)
		}
		return []UIMessage{cloneToolStreamMessage(state.Message)}

	case streamevent.Attachment:
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
	return clone
}
