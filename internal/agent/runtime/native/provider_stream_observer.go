package native

import (
	"context"

	sdk "github.com/felinics/twilight/sdk"
)

type providerStreamEventObserver struct {
	sdk.Provider
	observe func(StreamEvent)
	steer   *modelSteerGate
}

func modelWithProviderStreamEventObserver(model *sdk.Model, observe func(StreamEvent), steer *modelSteerGate) *sdk.Model {
	if model == nil || model.Provider == nil || (observe == nil && steer == nil) {
		return model
	}
	observed := *model
	provider := model.Provider
	// A final-steer continuation reuses the model from the preceding call.
	// Replace our observer instead of nesting another stream/notification loop.
	for {
		previous, ok := provider.(providerStreamEventObserver)
		if !ok {
			break
		}
		provider = previous.Provider
	}
	observed.Provider = providerStreamEventObserver{Provider: provider, observe: observe, steer: steer}
	return &observed
}

func (p providerStreamEventObserver) DoStream(ctx context.Context, params sdk.GenerateParams) (*sdk.StreamResult, error) {
	// Every provider call begins a fresh attempt. For ordinary multi-step runs
	// the previous step has already consumed or checkpointed its timings; for a
	// retry this discards the failed attempt before replacement parts arrive.
	if p.observe != nil {
		p.observe(StreamEvent{Type: EventRetry})
	}
	p.steer.begin()
	result, err := p.Provider.DoStream(ctx, params)
	if err != nil || result == nil || result.Stream == nil {
		return result, err
	}

	source := result.Stream
	observed := make(chan sdk.StreamPart)
	result.Stream = observed
	go func() {
		defer close(observed)
		for {
			var part sdk.StreamPart
			var ok bool
			select {
			case part, ok = <-source:
				if !ok {
					return
				}
			case <-ctx.Done():
				return
			}
			if !p.steer.observe(part) {
				return
			}
			if event, ok := providerPartTimingEvent(part); ok && p.observe != nil {
				p.observe(event)
			}
			select {
			case observed <- part:
			case <-ctx.Done():
				return
			}
		}
	}()
	return result, nil
}

func providerPartTimingEvent(part sdk.StreamPart) (StreamEvent, bool) {
	switch p := part.(type) {
	case *sdk.ReasoningStartPart:
		return StreamEvent{Type: EventReasoningStart}, true
	case *sdk.ReasoningDeltaPart:
		return StreamEvent{Type: EventReasoningDelta, Delta: p.Text}, true
	case *sdk.ReasoningEndPart:
		return StreamEvent{Type: EventReasoningEnd}, true
	case *sdk.TextStartPart:
		return StreamEvent{Type: EventTextStart}, true
	case *sdk.TextDeltaPart:
		return StreamEvent{Type: EventTextDelta, Delta: p.Text}, true
	case *sdk.TextEndPart:
		return StreamEvent{Type: EventTextEnd}, true
	case *sdk.ToolInputStartPart:
		return StreamEvent{Type: EventToolCallInputStart}, true
	case *sdk.StreamToolCallPart:
		return StreamEvent{Type: EventToolCallStart}, true
	case *sdk.ToolProgressPart:
		return StreamEvent{Type: EventToolCallProgress}, true
	case *sdk.ToolApprovalRequestPart:
		return StreamEvent{Type: EventToolApprovalRequest}, true
	case *sdk.AbortPart:
		return StreamEvent{Type: EventAgentAbort}, true
	default:
		return StreamEvent{}, false
	}
}
