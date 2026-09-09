package native

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/google/jsonschema-go/jsonschema"

	agenttools "github.com/felinics/memoh/internal/agent/tool"
)

func TestStreamSteerInterruptsOnlyInvocation(t *testing.T) {
	for _, mode := range []string{"text", "reasoning", "headers", "consecutive", "retry", "checkpoint_failure"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			wake := make(chan struct{}, 1)
			started := make(chan int, 3)
			var calls, disconnected atomic.Int32
			var pending, continueAfter atomic.Bool
			var nextInputs []sdk.Message
			var checkpoints, starts, terminals int
			var steps []int
			var finalInput []sdk.Message
			interruptions := 1
			retryAttempts := 0
			if mode == "retry" {
				retryAttempts = 1
			}
			if mode == "consecutive" {
				interruptions = 2
			}
			provider := agentStreamTestProvider(func(ctx context.Context, params sdk.GenerateParams) (*sdk.StreamResult, error) {
				call := int(calls.Add(1))
				if mode == "retry" && call == 1 {
					return closedAgentTestStream(&sdk.ErrorPart{Error: errors.New("unexpected EOF")}), nil
				}
				call -= retryAttempts
				if call > interruptions {
					finalInput = cloneProviderMessages(params.Messages)
					return closedAgentTestStream(&sdk.TextDeltaPart{Text: "done"}, &sdk.FinishStepPart{FinishReason: sdk.FinishReasonStop}), nil
				}
				if mode == "headers" {
					started <- call
					<-ctx.Done()
					disconnected.Add(1)
					return nil, ctx.Err()
				}
				parts := make(chan sdk.StreamPart)
				go func() {
					defer close(parts)
					var part sdk.StreamPart = &sdk.TextDeltaPart{Text: fmt.Sprintf("partial-%d", call)}
					if mode == "reasoning" {
						part = &sdk.ReasoningDeltaPart{Text: "unfinished thinking"}
					}
					select {
					case parts <- part:
					case <-ctx.Done():
					}
					<-ctx.Done()
					disconnected.Add(1)
				}()
				return &sdk.StreamResult{Stream: parts}, nil
			})
			events := New(Deps{}).Stream(ctx, RunConfig{
				Model: &sdk.Model{ID: "mock", Provider: provider}, Messages: []sdk.Message{sdk.UserMessage("original")},
				SteerWake: wake, PendingSteer: func(context.Context) (bool, error) { return pending.Load(), nil },
				ContinueAfterFinal: &continueAfter, NextModelInputs: &nextInputs,
				OnSteer: func(_ context.Context, index int, _ *sdk.StepResult) error {
					if index != checkpoints {
						return fmt.Errorf("step %d, want %d", index, checkpoints)
					}
					checkpoints++
					if mode == "checkpoint_failure" {
						return errors.New("SECRET database diagnostic")
					}
					pending.Store(false)
					nextInputs = []sdk.Message{sdk.UserMessage(fmt.Sprintf("steer-%d", checkpoints))}
					continueAfter.Store(true)
					return nil
				},
			})
			for events != nil {
				select {
				case <-started:
					pending.Store(true)
					wake <- struct{}{}
				case e, ok := <-events:
					if !ok {
						events = nil
						continue
					}
					if e.Type == EventTextDelta && strings.HasPrefix(e.Delta, "partial-") || e.Type == EventReasoningDelta {
						pending.Store(true)
						wake <- struct{}{}
					}
					if e.Type == EventAgentStart {
						starts++
					}
					if e.Type == EventStepEnd {
						steps = append(steps, e.StepNumber)
					}
					if strings.Contains(e.Error, "SECRET") || (e.Type == EventError && mode != "checkpoint_failure" && mode != "retry") {
						t.Fatalf("unexpected public error: %+v", e)
					}
					if e.IsTerminal() {
						terminals++
						if mode != "checkpoint_failure" && e.Type != EventAgentEnd {
							t.Fatalf("steer terminated run: %+v", e)
						}
					}
				case <-ctx.Done():
					t.Fatal("steer failed to continue the blocked invocation")
				}
			}
			if mode == "checkpoint_failure" {
				if calls.Load() != 1 {
					t.Fatal("continued after failed persistence")
				}
				return
			}
			if calls.Load() != int32(interruptions+retryAttempts+1) || disconnected.Load() != int32(interruptions) || starts != 1 || terminals != 1 {
				t.Fatalf("calls=%d disconnected=%d starts=%d terminals=%d", calls.Load(), disconnected.Load(), starts, terminals)
			}
			for i, step := range steps {
				if step != i {
					t.Fatalf("step cursor: %v", steps)
				}
			}
			var transcript strings.Builder
			for _, message := range finalInput {
				transcript.WriteString(messageContentText(message))
				for _, part := range message.Content {
					if _, ok := part.(sdk.ReasoningPart); ok {
						t.Fatal("replayed unfinished provider reasoning")
					}
				}
			}
			text := transcript.String()
			if !strings.Contains(text, "original") || strings.Count(text, "steer-1") != 1 || mode == "consecutive" && strings.Count(text, "steer-2") != 1 {
				t.Fatalf("continuation input: %q", text)
			}
		})
	}
}

func TestSteerGatePreservesToolAndCommitBoundaries(t *testing.T) {
	for _, part := range []sdk.StreamPart{&sdk.ToolInputStartPart{}, &sdk.StreamToolCallPart{}, &sdk.FinishStepPart{}} {
		t.Run(fmt.Sprintf("%T", part), func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			g := &modelSteerGate{cancel: cancel, ready: make(chan struct{}, 1)}
			g.begin()
			g.observe(part)
			if g.interrupt() || ctx.Err() != nil {
				t.Fatal("interrupted tool/commit boundary")
			}
			g.begin()
			if !g.interrupt() || !errors.Is(context.Cause(ctx), errModelSteered) || g.observe(&sdk.FinishStepPart{}) {
				t.Fatal("next model invocation failed to fence late output")
			}
		})
	}
}

func TestSteerPreservesToolsAndEarlierInput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := make(chan context.Context, 1)
	release := make(chan struct{})
	wake := make(chan struct{}, 1)
	var calls, executions atomic.Int32
	var pending atomic.Bool
	var immediateInput atomic.Bool
	var nextInputs []sdk.Message
	var finalInput []sdk.Message
	provider := agentStreamTestProvider(func(ctx context.Context, params sdk.GenerateParams) (*sdk.StreamResult, error) {
		call := calls.Add(1)
		if call <= 2 {
			if call == 2 {
				for _, message := range params.Messages {
					if messageContentText(message) == "change direction" {
						immediateInput.Store(true)
					}
				}
			}
			return closedAgentTestStream(
				&sdk.StreamToolCallPart{ToolCallID: fmt.Sprintf("call-%d", call), ToolName: "held_tool", Input: map[string]any{}},
				&sdk.FinishStepPart{FinishReason: sdk.FinishReasonToolCalls},
			), nil
		}
		if call == 3 {
			parts := make(chan sdk.StreamPart, 1)
			parts <- &sdk.TextDeltaPart{Text: "after-tools"}
			go func() { <-ctx.Done(); close(parts) }()
			return &sdk.StreamResult{Stream: parts}, nil
		}
		finalInput = cloneProviderMessages(params.Messages)
		return closedAgentTestStream(&sdk.TextDeltaPart{Text: "done"}, &sdk.FinishStepPart{FinishReason: sdk.FinishReasonStop}), nil
	})
	a := New(Deps{})
	a.SetToolProviders([]agenttools.ToolProvider{staticToolProvider{tools: []sdk.Tool{{
		Name: "held_tool", Parameters: &jsonschema.Schema{Type: "object"},
		Execute: func(ctx *sdk.ToolExecContext, _ any) (any, error) {
			if executions.Add(1) > 1 {
				return "completed second tool result", nil
			}
			started <- ctx
			select {
			case <-release:
				return "completed tool result", nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	}}}})
	events := a.Stream(ctx, RunConfig{
		Model: &sdk.Model{ID: "mock", Provider: provider}, Messages: []sdk.Message{sdk.UserMessage("original")},
		SupportsToolCall: true, SteerWake: wake, NextModelInputs: &nextInputs,
		PendingSteer: func(context.Context) (bool, error) { return pending.Load(), nil },
		OnSteer: func(_ context.Context, index int, _ *sdk.StepResult) error {
			if index != 2 {
				return errors.New("must not preempt a tool")
			}
			nextInputs = []sdk.Message{sdk.UserMessage("second change")}
			pending.Store(false)
			return nil
		},
		OnStepCommitted: func(_ context.Context, index int, _ *sdk.StepResult) error {
			if index == 0 {
				nextInputs = []sdk.Message{sdk.UserMessage("change direction")}
				pending.Store(false)
			}
			return nil
		},
	})
	var releaseTimer <-chan time.Time
	var toolCtx context.Context
	for events != nil {
		select {
		case toolCtx = <-started:
			pending.Store(true)
			wake <- struct{}{}
			releaseTimer = time.After(25 * time.Millisecond)
		case <-releaseTimer:
			if toolCtx.Err() != nil {
				t.Fatal("steer cancelled the tool")
			}
			close(release)
			releaseTimer = nil
		case e, ok := <-events:
			switch {
			case !ok:
				events = nil
			case e.Type == EventError || e.Type == EventAgentAbort:
				t.Fatalf("tool continuation failed: %+v", e)
			case e.Type == EventTextDelta && e.Delta == "after-tools":
				pending.Store(true)
				wake <- struct{}{}
			}
		case <-ctx.Done():
			t.Fatal("tool continuation timed out")
		}
	}
	users, secondUsers, results := 0, 0, 0
	for _, message := range finalInput {
		if message.Role == sdk.MessageRoleUser && messageContentText(message) == "change direction" {
			users++
		}
		if message.Role == sdk.MessageRoleTool {
			results++
		}
		if message.Role == sdk.MessageRoleUser && messageContentText(message) == "second change" {
			secondUsers++
		}
	}
	if calls.Load() != 4 || executions.Load() != 2 || users != 1 || secondUsers != 1 || results != 2 || !immediateInput.Load() {
		t.Fatalf("calls=%d tools=%d steer inputs=%d,%d results=%d immediate=%v", calls.Load(), executions.Load(), users, secondUsers, results, immediateInput.Load())
	}
}
