package application

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"

	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
)

// SessionQueues is the application surface for user-facing queue operations.
// Items are transient and live in the configured memory or Redis runtime.
type SessionQueues struct {
	SteerSupported bool
	Steer          []sessionruntime.SteerItem
	FollowUp       []sessionruntime.FollowUpItem
}

func (s *Service) liveQueueRuntime() (*sessionruntime.Manager, error) {
	if s == nil || s.sessionManager == nil {
		return nil, sessionruntime.ErrLiveQueueUnavailable
	}
	return s.sessionManager, nil
}

func (s *Service) EnqueueSteer(ctx context.Context, botID, sessionID, invocationID string, payload []byte) (sessionruntime.SteerItem, error) {
	runtime, err := s.liveQueueRuntime()
	if err != nil {
		return sessionruntime.SteerItem{}, err
	}
	return runtime.EnqueueSteer(ctx, sessionruntime.Key{BotID: botID, SessionID: sessionID}, uuid.NewString(), invocationID, payload)
}

func (s *Service) EnqueueFollowUp(ctx context.Context, botID, sessionID, invocationID string, payload []byte) (sessionruntime.FollowUpItem, error) {
	runtime, err := s.liveQueueRuntime()
	if err != nil {
		return sessionruntime.FollowUpItem{}, err
	}
	item, err := runtime.EnqueueFollowUp(ctx, sessionruntime.Key{BotID: botID, SessionID: sessionID}, uuid.NewString(), invocationID, payload)
	if err != nil {
		return sessionruntime.FollowUpItem{}, err
	}
	if item.Status == sessionruntime.QueueAccepted {
		s.kickFollowUpIfIdle(ctx, botID, sessionID, item.EnqueuedDuringRunID)
	}
	return item, nil
}

func (s *Service) ListSessionQueues(ctx context.Context, botID, sessionID string) (SessionQueues, error) {
	runtime, err := s.liveQueueRuntime()
	if err != nil {
		return SessionQueues{}, err
	}
	steers, followUps, err := runtime.PendingQueues(ctx, sessionruntime.Key{BotID: botID, SessionID: sessionID}, 0)
	if err != nil {
		return SessionQueues{}, err
	}
	snapshot, err := runtime.Snapshot(ctx, botID, sessionID)
	if err != nil {
		return SessionQueues{}, err
	}
	return SessionQueues{Steer: steers, FollowUp: followUps, SteerSupported: sessionruntime.SteerRunAvailable(snapshot.CurrentRunView)}, nil
}

func (s *Service) ReorderSteer(ctx context.Context, botID, sessionID string, item, before sessionruntime.SteerPendingRef) ([]sessionruntime.SteerItem, error) {
	runtime, err := s.liveQueueRuntime()
	if err != nil {
		return nil, err
	}
	return runtime.ReorderSteer(ctx, sessionruntime.Key{BotID: botID, SessionID: sessionID}, item, before)
}

func (s *Service) ReorderFollowUp(ctx context.Context, botID, sessionID string, item, before sessionruntime.FollowUpPendingRef) ([]sessionruntime.FollowUpItem, error) {
	runtime, err := s.liveQueueRuntime()
	if err != nil {
		return nil, err
	}
	return runtime.ReorderFollowUp(ctx, sessionruntime.Key{BotID: botID, SessionID: sessionID}, item, before)
}

func (s *Service) UpdateSteer(ctx context.Context, botID, sessionID, itemID string, payload []byte) (sessionruntime.SteerItem, error) {
	runtime, err := s.liveQueueRuntime()
	if err != nil {
		return sessionruntime.SteerItem{}, err
	}
	return runtime.UpdateSteer(ctx, sessionruntime.Key{BotID: botID, SessionID: sessionID}, sessionruntime.SteerItemID(itemID), payload)
}

func (s *Service) UpdateFollowUp(ctx context.Context, botID, sessionID, itemID string, payload []byte) (sessionruntime.FollowUpItem, error) {
	runtime, err := s.liveQueueRuntime()
	if err != nil {
		return sessionruntime.FollowUpItem{}, err
	}
	key := sessionruntime.Key{BotID: botID, SessionID: sessionID}
	_, items, err := runtime.PendingQueues(ctx, key, 0)
	if err != nil {
		return sessionruntime.FollowUpItem{}, err
	}
	for _, item := range items {
		if string(item.ID) != itemID {
			continue
		}
		body := decodeFollowUpPayload(item.Payload)
		if body.Command != nil {
			text := QueuePayloadText(payload)
			body.Text = text
			body.Command.Query = text
			body.Command.ModelQuery = ""
			body.Command.UserVisibleText = text
			payload, err = json.Marshal(body)
			if err != nil {
				return sessionruntime.FollowUpItem{}, err
			}
		}
		// Routing and attachment metadata are immutable for a queued command.
		// The backend rechecks accepted status atomically with the edit, so a
		// concurrent claim/cancel/promotion still rejects this write.
		return runtime.UpdateFollowUp(ctx, key, item.ID, payload)
	}
	return sessionruntime.FollowUpItem{}, sessionruntime.ErrQueueNotPending
}

func (s *Service) CancelSteer(ctx context.Context, botID, sessionID, itemID string) error {
	runtime, err := s.liveQueueRuntime()
	if err != nil {
		return err
	}
	return runtime.CancelSteer(ctx, sessionruntime.Key{BotID: botID, SessionID: sessionID}, sessionruntime.SteerItemID(itemID))
}

func (s *Service) CancelFollowUp(ctx context.Context, botID, sessionID, itemID string) error {
	runtime, err := s.liveQueueRuntime()
	if err != nil {
		return err
	}
	return runtime.CancelFollowUp(ctx, sessionruntime.Key{BotID: botID, SessionID: sessionID}, sessionruntime.FollowUpItemID(itemID))
}

func (s *Service) PromoteFollowUpToSteer(ctx context.Context, botID, sessionID string, followUp sessionruntime.FollowUpPendingRef) (sessionruntime.PromoteFollowUpResult, error) {
	runtime, err := s.liveQueueRuntime()
	if err != nil {
		return sessionruntime.PromoteFollowUpResult{}, err
	}
	return runtime.PromoteFollowUpToSteer(ctx, sessionruntime.Key{BotID: botID, SessionID: sessionID}, followUp)
}
