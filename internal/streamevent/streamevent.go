// Package streamevent is the leaf vocabulary for agent stream events, shared
// by the in-process agent loop (internal/agent), the ACP client
// (internal/acpclient), and the conversation layer. It exists so the event
// vocabulary is one set of constants the compiler keeps consistent, instead
// of parallel string tables locked together by tests, and so packages below
// internal/agent (which already depends on conversation) can speak the same
// types without an import cycle.
package streamevent

import "encoding/json"

// Type identifies the kind of stream event.
type Type string

const (
	AgentStart          Type = "agent_start"
	TextStart           Type = "text_start"
	TextDelta           Type = "text_delta"
	TextEnd             Type = "text_end"
	ReasoningStart      Type = "reasoning_start"
	ReasoningDelta      Type = "reasoning_delta"
	ReasoningEnd        Type = "reasoning_end"
	ToolCallInputStart  Type = "tool_call_input_start"
	ToolCallStart       Type = "tool_call_start"
	ToolCallProgress    Type = "tool_call_progress"
	ToolCallEnd         Type = "tool_call_end"
	ToolApprovalRequest Type = "tool_approval_request"
	UserInputRequest    Type = "user_input_request"
	Attachment          Type = "attachment_delta"
	Reaction            Type = "reaction_delta"
	Speech              Type = "speech_delta"
	AgentEnd            Type = "agent_end"
	AgentAbort          Type = "agent_abort"
	Retry               Type = "retry"
	Progress            Type = "progress"
	Error               Type = "error"
)

// FileAttachment is a file emitted alongside agent output.
type FileAttachment struct {
	Type        string         `json:"type"`
	Base64      string         `json:"base64,omitempty"`
	Path        string         `json:"path,omitempty"`
	URL         string         `json:"url,omitempty"`
	PlatformKey string         `json:"platform_key,omitempty"`
	Mime        string         `json:"mime,omitempty"`
	Name        string         `json:"name,omitempty"`
	ContentHash string         `json:"content_hash,omitempty"`
	Size        int64          `json:"size,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

// ReactionItem represents an emoji reaction extracted from agent output.
type ReactionItem struct {
	Emoji string `json:"emoji"`
}

// SpeechItem represents a TTS request extracted from agent output.
type SpeechItem struct {
	Text string `json:"text"`
}

// Event is emitted by the agent during streaming.
type Event struct {
	Type           Type             `json:"type"`
	Delta          string           `json:"delta,omitempty"`
	ToolName       string           `json:"toolName,omitempty"`
	ToolCallID     string           `json:"toolCallId,omitempty"`
	ApprovalID     string           `json:"approvalId,omitempty"`
	UserInputID    string           `json:"userInputId,omitempty"`
	ShortID        int              `json:"shortId,omitempty"`
	Status         string           `json:"status,omitempty"`
	Input          any              `json:"input,omitempty"`
	Metadata       map[string]any   `json:"metadata,omitempty"`
	Progress       any              `json:"progress,omitempty"`
	Result         any              `json:"result,omitempty"`
	Attachments    []FileAttachment `json:"attachments,omitempty"`
	Reactions      []ReactionItem   `json:"reactions,omitempty"`
	Speeches       []SpeechItem     `json:"speeches,omitempty"`
	Messages       json.RawMessage  `json:"messages,omitempty"`
	Usage          json.RawMessage  `json:"usage,omitempty"`
	Reasoning      []string         `json:"reasoning,omitempty"`
	Error          string           `json:"error,omitempty"`
	Attempt        int              `json:"attempt,omitempty"`
	MaxAttempt     int              `json:"maxAttempt,omitempty"`
	RetryError     string           `json:"retryError,omitempty"`
	StepNumber     int              `json:"stepNumber,omitempty"`
	TotalSteps     int              `json:"totalSteps,omitempty"`
	ProgressStatus string           `json:"progressStatus,omitempty"`
}

// IsTerminal returns true for events that signal end of stream.
func (e Event) IsTerminal() bool {
	return e.Type == AgentEnd || e.Type == AgentAbort
}
