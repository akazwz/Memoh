package opencode

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/felinics/memoh/internal/agent/runtime/external"
)

var _ external.Compactor = (*Driver)(nil)

func (d *Driver) Compact(ctx context.Context, input external.PromptInput) (external.CompactionResult, error) {
	if metadataString(input.RuntimeMetadata, metadataSessionIDKey) == "" {
		return external.CompactionResult{}, external.ErrThreadUnavailable
	}
	srv, err := d.controlServer(ctx, input)
	if err != nil {
		return external.CompactionResult{}, err
	}
	defer srv.close()
	metadata, err := compactNative(ctx, srv.api, input)
	if err != nil {
		return external.CompactionResult{}, unavailable(err)
	}
	return external.CompactionResult{RuntimeMetadata: metadata}, nil
}

func compactNative(ctx context.Context, api *apiClient, input external.PromptInput) (map[string]any, error) {
	id := metadataString(input.RuntimeMetadata, metadataSessionIDKey)
	endpoint := "/session/" + url.PathEscape(id)
	var before []nativeMessage
	if err := api.call(ctx, http.MethodGet, endpoint+"/message", nil, &before); err != nil {
		return nil, err
	}
	if len(before) == 0 {
		return nil, external.ErrThreadUnavailable
	}
	lastUser := nativeMessage{}
	for _, message := range before {
		if message.Info.Role == "user" {
			lastUser = message
		}
	}
	if lastUser.Info.ID != metadataString(input.RuntimeMetadata, metadataMessageIDKey) {
		return nil, errors.New("opencode compaction anchor does not match committed history")
	}
	provider, model := lastUser.Info.Model.ProviderID, lastUser.Info.Model.ModelID
	if input.ModelID != "" {
		var ok bool
		provider, model, ok = strings.Cut(input.ModelID, "/")
		if !ok {
			return nil, errors.New("invalid opencode compaction model")
		}
	}
	if provider == "" || model == "" {
		return nil, errors.New("opencode compaction model is unavailable")
	}
	requestCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- api.call(requestCtx, http.MethodPost, endpoint+"/summarize", map[string]any{"providerID": provider, "modelID": model, "auto": false}, nil)
	}()
	select {
	case err := <-done:
		if err != nil {
			return nil, err
		}
	case <-ctx.Done():
		abortCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer stop()
		_ = api.call(abortCtx, http.MethodPost, endpoint+"/abort", nil, nil)
		select {
		case <-done:
		case <-abortCtx.Done():
		}
		return nil, ctx.Err()
	}
	var after []nativeMessage
	if err := api.call(ctx, http.MethodGet, endpoint+"/message", nil, &after); err != nil {
		return nil, err
	}
	prior := map[string]bool{}
	for _, message := range before {
		prior[message.Info.ID] = true
	}
	completed, anchor := false, ""
	for _, message := range after {
		if message.Info.Role == "user" {
			anchor = message.Info.ID
		}
		if prior[message.Info.ID] {
			continue
		}
		if message.Info.Error != nil {
			return nil, errors.New("opencode compaction failed")
		}
		if message.Info.Role == "assistant" && string(message.Info.Summary) == "true" && message.Info.Finish != "" {
			completed = true
		}
	}
	if !completed || anchor == "" {
		return nil, errors.New("opencode did not complete compaction")
	}
	return map[string]any{metadataSessionIDKey: id, metadataMessageIDKey: anchor}, nil
}
