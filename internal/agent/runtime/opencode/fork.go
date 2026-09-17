package opencode

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/felinics/memoh/internal/agent/runtime/external"
)

const metadataStoreIDKey = "opencode_store_id"
const metadataForkAnchorsKey = "opencode_fork_anchors"

var _ external.ThreadForker = (*Driver)(nil)

func (d *Driver) ForkThread(ctx context.Context, botID, botAgentID, sourceThreadID string, metadata map[string]any, lastTurnID string) (map[string]any, error) {
	if metadataString(metadata, metadataSessionIDKey) == "" {
		return nil, external.ErrThreadUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	cfg, client, _, launcher, err := d.resolve(ctx, botID, botAgentID)
	if err != nil {
		return nil, err
	}
	input := external.PromptInput{BotID: botID, BotAgentID: botAgentID, ThreadID: sourceThreadID, RuntimeMetadata: metadata}
	srv, err := startServer(ctx, client, launcher, cfg, input, "")
	if err != nil {
		return nil, unavailable(err)
	}
	defer srv.close()
	delta, err := forkNative(ctx, srv.api, metadata, lastTurnID)
	if err != nil {
		return nil, unavailable(err)
	}
	// Forks are independent native sessions in their source family's store.
	// Keep the store identity explicit across process restarts and nested forks;
	// ordinary threads still receive their own store. Soft-deleting a Memoh
	// thread never removes the native store needed by surviving branches.
	delta[metadataStoreIDKey] = firstNonEmpty(metadataString(metadata, metadataStoreIDKey), sourceThreadID)
	return delta, nil
}

func forkNative(ctx context.Context, api *apiClient, metadata map[string]any, lastTurnID string) (map[string]any, error) {
	sourceID := metadataString(metadata, metadataSessionIDKey)
	var messages []nativeMessage
	if err := api.call(ctx, http.MethodGet, "/session/"+url.PathEscape(sourceID)+"/message", nil, &messages); err != nil {
		return nil, err
	}
	aliases := forkAnchors(metadata)
	anchor := lastTurnID
	if mapped := aliases[anchor]; mapped != "" {
		anchor = mapped
	}
	end, err := forkBoundary(messages, anchor)
	if err != nil {
		return nil, err
	}
	body := map[string]any{}
	if end < len(messages) {
		body["messageID"] = messages[end].Info.ID
	}
	var fork struct {
		ID string `json:"id"`
	}
	if err := api.call(ctx, http.MethodPost, "/session/"+url.PathEscape(sourceID)+"/fork", body, &fork); err != nil {
		return nil, err
	}
	if fork.ID == "" || fork.ID == sourceID {
		return nil, errors.New("opencode returned an invalid fork")
	}
	var copied []nativeMessage
	if err := api.call(ctx, http.MethodGet, "/session/"+url.PathEscape(fork.ID)+"/message", nil, &copied); err != nil {
		return nil, err
	}
	if len(copied) != end {
		return nil, errors.New("opencode fork history does not match the requested boundary")
	}
	mapping := map[string]string{}
	last := ""
	for i, message := range copied {
		if message.Info.Role != messages[i].Info.Role {
			return nil, errors.New("opencode fork changed message order")
		}
		if message.Info.Role != "user" {
			continue
		}
		mapping[messages[i].Info.ID] = message.Info.ID
		last = message.Info.ID
	}
	for original, prior := range aliases {
		if next := mapping[prior]; next != "" {
			mapping[original] = next
		}
	}
	if last == "" {
		return nil, errors.New("opencode fork has no user anchor")
	}
	return map[string]any{metadataSessionIDKey: fork.ID, metadataMessageIDKey: last, metadataForkAnchorsKey: mapping}, nil
}

// OpenCode's fork boundary is exclusive, whereas Memoh includes the selected
// assistant round. Stop at the next user message, retaining every assistant
// tool step in the selected round. Never silently fork at head on a stale ID.
func forkBoundary(messages []nativeMessage, anchor string) (int, error) {
	if anchor == "" {
		return len(messages), nil
	}
	found := false
	for i, message := range messages {
		if message.Info.Role != "user" {
			continue
		}
		if found {
			return i, nil
		}
		if message.Info.ID == anchor {
			found = true
		}
	}
	if !found {
		return 0, errors.New("opencode fork anchor is missing")
	}
	return len(messages), nil
}

func forkAnchors(metadata map[string]any) map[string]string {
	out := map[string]string{}
	switch values := metadata[metadataForkAnchorsKey].(type) {
	case map[string]string:
		for k, v := range values {
			out[k] = v
		}
	case map[string]any:
		for k, v := range values {
			if s, ok := v.(string); ok {
				out[k] = s
			}
		}
	}
	return out
}
