package application

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/felinics/memoh/internal/agent/runtime/external"
)

func TestRuntimeRoundHeadFollowsCapturedNativeState(t *testing.T) {
	t.Parallel()

	failure := errors.New("codex turn failed")
	for _, tc := range []struct {
		name       string
		checkpoint external.CheckpointOutcome
		promptErr  error
		completed  bool
		published  bool
		reset      bool
	}{
		{"staged_completed", external.CheckpointStaged, nil, true, true, false},
		// Restoring the older snapshot would make the runtime forget a round the
		// user can see, so a captured abort or failure moves the head too.
		{"staged_aborted", external.CheckpointStaged, nil, false, true, false},
		{"staged_failed", external.CheckpointStaged, failure, false, true, false},
		{"declined_completed", external.CheckpointDeclined, nil, true, true, true},
		// ACP declines on every result; an unfinished round of it must keep the
		// head its warm session is fenced against.
		{"declined_aborted", external.CheckpointDeclined, nil, false, false, false},
		{"declined_failed", external.CheckpointDeclined, failure, false, false, false},
		{"none_completed", external.CheckpointNone, nil, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			messages := &recordingMessageService{}
			service := &Service{messageService: messages, logger: slog.New(slog.DiscardHandler)}
			err := service.persistRuntimeRound(
				context.Background(),
				ChatRequest{BotID: "bot-1", ThreadID: "session-1", RunID: "run-1", Query: "inspect"},
				"codex", "/data/app",
				external.PromptResult{Text: "partial", Checkpoint: tc.checkpoint},
				tc.promptErr, tc.completed, nil, nil,
			)
			if err != nil {
				t.Fatal(err)
			}
			publication := messages.roundOptions[len(messages.roundOptions)-1].AgentPublication
			if (publication != nil) != tc.published {
				t.Fatalf("publication = %#v", publication)
			}
			if publication != nil && (publication.RunID != "run-1" || publication.CheckpointReset != tc.reset) {
				t.Fatalf("publication = %#v", publication)
			}
		})
	}
}
