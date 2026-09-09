package application

import (
	"context"

	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// recordingStepPersister records the history boundary shared by native and
// subagent step tests, with an optional persistence failure.
type recordingStepPersister struct {
	*recordingMessageService
	steps   []messagepkg.AgentStep
	stepErr error
}

func (s *recordingStepPersister) PersistAgentStep(_ context.Context, step messagepkg.AgentStep) ([]messagepkg.Message, error) {
	if s.stepErr != nil {
		return nil, s.stepErr
	}
	s.steps = append(s.steps, step)
	result := make([]messagepkg.Message, len(step.Messages))
	for i, input := range step.Messages {
		result[i] = messagepkg.Message{ID: "committed", Role: input.Role, BotID: input.BotID, SessionID: input.SessionID, Metadata: input.Metadata, Content: input.Content}
	}
	return result, nil
}

func (s *recordingStepPersister) PersistAgentReplacementStep(ctx context.Context, step messagepkg.AgentStep) ([]messagepkg.Message, error) {
	return s.PersistAgentStep(ctx, step)
}

func (*recordingStepPersister) FinalizeAgentReplacement(context.Context, string, messagepkg.TurnReplacement, string, string) error {
	return nil
}
