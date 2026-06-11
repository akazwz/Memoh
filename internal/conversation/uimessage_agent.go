package conversation

import (
	"strings"

	"github.com/memohai/memoh/internal/streamevent"
)

// UIStreamEventFromAgent converts an agent stream event into the UI message
// stream shape. It is the single agent-to-UI bridge — the local-channel
// WebSocket pipeline, trigger streams, and the native/ACP parity suite all
// share it, so attachment handling and field mapping cannot drift between
// pipelines (the previous per-pipeline copies had already diverged).
func UIStreamEventFromAgent(event streamevent.Event) UIMessageStreamEvent {
	attachments := make([]UIAttachment, 0, len(event.Attachments))
	for _, attachment := range event.Attachments {
		attachments = append(attachments, uiAttachmentFromAgent(attachment))
	}
	return UIMessageStreamEvent{
		Type:        event.Type,
		Delta:       event.Delta,
		ToolName:    event.ToolName,
		ToolCallID:  event.ToolCallID,
		Input:       event.Input,
		Output:      event.Result,
		Progress:    event.Progress,
		Attachments: attachments,
		Error:       event.Error,
		ApprovalID:  event.ApprovalID,
		UserInputID: event.UserInputID,
		ShortID:     event.ShortID,
		Status:      event.Status,
		Metadata:    event.Metadata,
	}
}

// uiAttachmentFromAgent is the superset of the former per-pipeline copies:
// the trigger pipeline used to drop BotID/StorageKey, so attachments streamed
// from scheduled runs lost their storage references.
func uiAttachmentFromAgent(attachment streamevent.FileAttachment) UIAttachment {
	result := UIAttachment{
		ID:          strings.TrimSpace(attachment.ContentHash),
		Type:        normalizeUIAttachmentType(attachment.Type, attachment.Mime),
		Path:        strings.TrimSpace(attachment.Path),
		URL:         strings.TrimSpace(attachment.URL),
		Name:        strings.TrimSpace(attachment.Name),
		ContentHash: strings.TrimSpace(attachment.ContentHash),
		Mime:        strings.TrimSpace(attachment.Mime),
		Size:        attachment.Size,
		Metadata:    attachment.Metadata,
	}
	if attachment.Metadata != nil {
		if botID, ok := attachment.Metadata["bot_id"].(string); ok {
			result.BotID = strings.TrimSpace(botID)
		}
		if storageKey, ok := attachment.Metadata["storage_key"].(string); ok {
			result.StorageKey = strings.TrimSpace(storageKey)
		}
	}
	return result
}
