package agent

import "github.com/memohai/memoh/internal/streamevent"

// The stream event vocabulary lives in internal/streamevent so packages that
// cannot import internal/agent (it depends on conversation) still share the
// exact same types and constants. These aliases keep the agent package API
// unchanged.

// StreamEventType identifies the kind of stream event.
type StreamEventType = streamevent.Type

const (
	EventAgentStart          = streamevent.AgentStart
	EventTextStart           = streamevent.TextStart
	EventTextDelta           = streamevent.TextDelta
	EventTextEnd             = streamevent.TextEnd
	EventReasoningStart      = streamevent.ReasoningStart
	EventReasoningDelta      = streamevent.ReasoningDelta
	EventReasoningEnd        = streamevent.ReasoningEnd
	EventToolCallInputStart  = streamevent.ToolCallInputStart
	EventToolCallStart       = streamevent.ToolCallStart
	EventToolCallProgress    = streamevent.ToolCallProgress
	EventToolCallEnd         = streamevent.ToolCallEnd
	EventToolApprovalRequest = streamevent.ToolApprovalRequest
	EventUserInputRequest    = streamevent.UserInputRequest
	EventAttachment          = streamevent.Attachment
	EventReaction            = streamevent.Reaction
	EventSpeech              = streamevent.Speech
	EventAgentEnd            = streamevent.AgentEnd
	EventAgentAbort          = streamevent.AgentAbort
	EventRetry               = streamevent.Retry
	EventProgress            = streamevent.Progress
	EventError               = streamevent.Error
)

// StreamEvent is emitted by the agent during streaming.
type StreamEvent = streamevent.Event
