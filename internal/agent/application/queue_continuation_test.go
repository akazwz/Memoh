package application

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/turn"
)

// newFollowUpTestService wires a Service whose live queue lives in a memory
// backend with one active run, while turn admission is scripted so a
// continuation can start without PostgreSQL.
func newFollowUpTestService(t *testing.T, runner *fakeRunner) (*Service, *scriptedAdmitter, *sessionruntime.MemoryBackend, sessionruntime.Key) {
	t.Helper()
	backend := sessionruntime.NewMemoryBackend()
	key := sessionruntime.Key{BotID: "bot", SessionID: "session"}
	_, _, err := backend.Update(context.Background(), key, func(snapshot sessionruntime.Snapshot, _ bool) (sessionruntime.Snapshot, bool, error) {
		snapshot.BotID, snapshot.SessionID = key.BotID, key.SessionID
		snapshot.CurrentRunView = &sessionruntime.CurrentRunView{
			RunID: "original-run", TurnID: "turn-1", Generation: "gen-1", OwnerID: "owner-1", Status: sessionruntime.RunStatusRunning,
		}
		return snapshot, true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	manager := sessionruntime.NewManager(backend, sessionruntime.Options{})
	t.Cleanup(func() { _ = manager.Close() })
	service, admitter := newAdmittedTurnTestService(runner)
	service.sessionManager = manager
	service.allowedTeam = "team1"
	return service, admitter, backend, key
}

func markFollowUpTestRunTerminal(t *testing.T, backend *sessionruntime.MemoryBackend, key sessionruntime.Key) {
	t.Helper()
	_, _, err := backend.Update(context.Background(), key, func(snapshot sessionruntime.Snapshot, _ bool) (sessionruntime.Snapshot, bool, error) {
		if snapshot.CurrentRunView != nil {
			snapshot.CurrentRunView.Status = sessionruntime.RunStatusCompleted
		}
		return snapshot, true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestEnqueueDeferredTurnStartsFollowUpWithOriginalCommand(t *testing.T) {
	for _, text := range []string{"later", "edited"} {
		t.Run(text, func(t *testing.T) {
			runner := &fakeRunner{chunks: []string{`{"type":"done"}`}}
			service, admitter, backend, key := newFollowUpTestService(t, runner)
			ctx := context.Background()
			cmd := turn.StartTurnCommand{
				TeamID: "team1", Mode: turn.ModeChat, BotID: key.BotID, ThreadID: key.SessionID,
				ChatID: "chat-42", RouteID: "route-7", ReplyTarget: "tg:1",
				Query: "later", UserVisibleText: "later", IdempotencyKey: "msg-1",
				Attachments: []turn.Attachment{{Type: "image", URL: "https://example.invalid/image.png"}},
			}
			if err := service.EnqueueDeferredTurn(ctx, cmd); err != nil {
				t.Fatalf("enqueue deferred turn: %v", err)
			}
			queues, err := service.ListSessionQueues(ctx, key.BotID, key.SessionID)
			if err != nil || len(queues.FollowUp) != 1 || QueuePayloadText(queues.FollowUp[0].Payload) != "later" {
				t.Fatalf("queued follow-up = %#v, %v", queues.FollowUp, err)
			}
			// The same platform message redelivered while still busy is one item.
			if err := service.EnqueueDeferredTurn(ctx, cmd); err != nil {
				t.Fatalf("replay deferred turn: %v", err)
			}
			if queues, err = service.ListSessionQueues(ctx, key.BotID, key.SessionID); err != nil || len(queues.FollowUp) != 1 {
				t.Fatalf("replayed follow-up queue = %#v, %v", queues.FollowUp, err)
			}

			if text == "edited" {
				// This is the text-only envelope used by HTTP queue editing.
				if _, err := service.UpdateFollowUp(ctx, key.BotID, key.SessionID, string(queues.FollowUp[0].ID), []byte(`{"text":"edited"}`)); err != nil {
					t.Fatal(err)
				}
			}

			markFollowUpTestRunTerminal(t, backend, key)
			service.startFollowUp(ctx, sessionruntime.TerminalRun{RunID: "original-run", BotID: key.BotID, SessionID: key.SessionID})

			if runner.gotReq.ReplyTarget != "tg:1" || runner.gotReq.RouteID != "route-7" || runner.gotReq.ChatID != "chat-42" || runner.gotReq.Query != text ||
				!reflect.DeepEqual(runner.gotReq.Attachments, cmd.Attachments) {
				t.Fatalf("continuation lost the original routing: %+v", runner.gotReq)
			}
			admitter.mu.Lock()
			inputs := append([]sessionruntime.AdmitInput(nil), admitter.inputs...)
			admitter.mu.Unlock()
			if len(inputs) != 1 || !strings.HasPrefix(inputs[0].InvocationID, "follow-up:") {
				t.Fatalf("continuation admission = %#v, want a queue-item retry identity", inputs)
			}
			queues, err = service.ListSessionQueues(ctx, key.BotID, key.SessionID)
			if err != nil || len(queues.FollowUp) != 0 {
				t.Fatalf("follow-up still pending after start: %#v, %v", queues.FollowUp, err)
			}
		})
	}
}

func TestEnqueueDeferredTurnWithoutActiveRunReportsNoActiveRun(t *testing.T) {
	backend := sessionruntime.NewMemoryBackend()
	manager := sessionruntime.NewManager(backend, sessionruntime.Options{})
	t.Cleanup(func() { _ = manager.Close() })
	service := &Service{sessionManager: manager}
	err := service.EnqueueDeferredTurn(context.Background(), turn.StartTurnCommand{
		TeamID: "team1", Mode: turn.ModeChat, BotID: "bot", ThreadID: "idle", Query: "hi",
	})
	if !errors.Is(err, sessionruntime.ErrQueueNoActiveRun) {
		t.Fatalf("idle session deferred enqueue error = %v, want %v", err, sessionruntime.ErrQueueNoActiveRun)
	}
}

func TestFollowUpCommandFromTextPayloadStartsOrdinaryChatTurn(t *testing.T) {
	service := &Service{allowedTeam: "team1"}
	textItem := sessionruntime.FollowUpItem{
		ID: "f1", BotID: "bot", SessionID: "session", Payload: []byte(`{"text":"hello"}`),
	}
	cmd, ok := service.followUpCommand(textItem)
	if !ok {
		t.Fatal("text payload was not accepted")
	}
	if cmd.TeamID != "team1" || !cmd.NoDefer || cmd.IdempotencyKey != "follow-up:f1" ||
		cmd.Mode != turn.ModeChat || cmd.BotID != "bot" || cmd.ChatID != "bot" || cmd.ThreadID != "session" || cmd.Query != "hello" {
		t.Fatalf("follow-up command = %+v", cmd)
	}
	if _, ok := (&Service{}).followUpCommand(textItem); ok {
		t.Fatal("text follow-up without a served team must fail closed")
	}
	if _, ok := service.followUpCommand(sessionruntime.FollowUpItem{ID: "f2", BotID: "bot", SessionID: "session", Payload: []byte(`{"text":"  "}`)}); ok {
		t.Fatal("empty text payload was accepted")
	}
	// A stored command must belong to the item's session.
	foreign, _ := encodeFollowUpCommand(turn.StartTurnCommand{BotID: "bot", ThreadID: "other", Query: "x"})
	if _, ok := service.followUpCommand(sessionruntime.FollowUpItem{ID: "f3", BotID: "bot", SessionID: "session", Payload: foreign}); ok {
		t.Fatal("command for another session was accepted")
	}
}

func TestFollowUpStartIsSingleFlightPerSession(t *testing.T) {
	runner := &fakeRunner{chunks: []string{`{"type":"done"}`}}
	service, _, backend, key := newFollowUpTestService(t, runner)
	ctx := context.Background()
	if _, err := service.EnqueueFollowUp(ctx, key.BotID, key.SessionID, "invoke-1", []byte(`{"text":"queued"}`)); err != nil {
		t.Fatal(err)
	}
	markFollowUpTestRunTerminal(t, backend, key)
	service.followUpStarts.Store(key.String(), &followUpStart{})
	service.startFollowUp(ctx, sessionruntime.TerminalRun{RunID: "original-run", BotID: key.BotID, SessionID: key.SessionID})
	if runner.gotReq.Query != "" {
		t.Fatalf("second starter ran while another was in flight: %+v", runner.gotReq)
	}
	queues, err := service.ListSessionQueues(ctx, key.BotID, key.SessionID)
	if err != nil || len(queues.FollowUp) != 1 || queues.FollowUp[0].Status != sessionruntime.QueueAccepted {
		t.Fatalf("follow-up should remain accepted: %#v, %v", queues.FollowUp, err)
	}
}

func TestFollowUpSchedulingRaces(t *testing.T) {
	for _, tc := range []struct {
		name        string
		pending     int
		duringDrain bool
	}{
		{"enqueue after terminal notification", 1, false},
		{"next terminal before prior drain returns", 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service, admitter, backend, key := newFollowUpTestService(t, &fakeRunner{chunks: []string{`{"type":"done"}`}})
			ctx := context.Background()
			for _, id := range []string{"first", "second"}[:tc.pending] {
				if _, err := service.EnqueueFollowUp(ctx, key.BotID, key.SessionID, id, []byte(`{"text":"queued"}`)); err != nil {
					t.Fatal(err)
				}
			}
			if tc.duringDrain {
				service.sessionRuntime = &terminalNotifyingAdmitter{scriptedAdmitter: admitter, terminal: func(ctx context.Context, h sessionruntime.RunHandle) {
					service.startFollowUp(ctx, sessionruntime.TerminalRun{RunID: h.RunID, BotID: key.BotID, SessionID: key.SessionID})
				}}
			}
			markFollowUpTestRunTerminal(t, backend, key)
			if tc.duringDrain {
				service.startFollowUp(ctx, sessionruntime.TerminalRun{RunID: "original-run", BotID: key.BotID, SessionID: key.SessionID})
			} else {
				service.kickFollowUpIfIdle(ctx, key.BotID, key.SessionID, "original-run")
			}
			deadline := time.Now().Add(2 * time.Second)
			for {
				queues, err := service.ListSessionQueues(ctx, key.BotID, key.SessionID)
				if err != nil {
					t.Fatal(err)
				}
				if len(queues.FollowUp) == 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("trigger lost: %d pending follow-ups", len(queues.FollowUp))
				}
				time.Sleep(time.Millisecond)
			}
			admitter.mu.Lock()
			started := len(admitter.inputs)
			admitter.mu.Unlock()
			if started != tc.pending {
				t.Fatalf("admitted %d runs, want %d", started, tc.pending)
			}
		})
	}
}

type terminalNotifyingAdmitter struct {
	*scriptedAdmitter
	terminal func(context.Context, sessionruntime.RunHandle)
}

func (a *terminalNotifyingAdmitter) FinishRunWithErrorCode(ctx context.Context, handle sessionruntime.RunHandle, status, message string) error {
	if err := a.scriptedAdmitter.FinishRunWithErrorCode(ctx, handle, status, message); err != nil {
		return err
	}
	// Force the valid schedule in which the terminal observer runs before
	// the handle closes and before the first starter has returned from drain.
	a.terminal(ctx, handle)
	return nil
}
