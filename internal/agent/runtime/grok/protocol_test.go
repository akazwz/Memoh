package grok

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/felinics/memoh/internal/agent/decision/approval"
	userinput "github.com/felinics/memoh/internal/agent/decision/input"
	"github.com/felinics/memoh/internal/agent/event"
	"github.com/felinics/memoh/internal/agent/runtime/external"
	"github.com/felinics/memoh/internal/agent/runtime/toolmount"
)

type eventSink struct {
	mu     sync.Mutex
	events []event.StreamEvent
}

func (s *eventSink) EmitStreamEvent(ev event.StreamEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
}

func testTurn(t *testing.T) (*turnSession, net.Conn, *eventSink) {
	t.Helper()
	client, agent := net.Pipe()
	sink := &eventSink{}
	d := NewDriver(nil, nil, nil, nil, nil, toolmount.Gateway{}, nil)
	turn := newTurnSession(context.Background(), d, external.PromptInput{BotID: "bot", BotAgentID: "agent", ThreadID: "thread", RunID: "run", CanRequestUserInput: true, Sink: sink}, client)
	turn.sessionID = "s1"
	t.Cleanup(func() { turn.closeDecisions(); _ = agent.Close(); _ = client.Close() })
	return turn, agent, sink
}

func TestProtocolIgnoresReplayAndWaitsForFinalNotification(t *testing.T) {
	turn, agent, _ := testTurn(t)
	peer := acp.NewConnection(func(_ context.Context, method string, _ json.RawMessage) (any, *acp.RequestError) {
		if method != "session/prompt" {
			return nil, acp.NewMethodNotFound(method)
		}
		for _, p := range []map[string]any{
			{"sessionId": "s1", "_meta": map[string]any{"isReplay": true}, "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "old"}}},
			{"sessionId": "other", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "wrong"}}},
			{"sessionId": "s1", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "final"}}},
		} {
			raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": p})
			if _, err := agent.Write(append(raw, '\n')); err != nil {
				return nil, acp.NewInternalError(nil)
			}
		}
		return map[string]any{"stopReason": "end_turn", "_meta": map[string]any{"promptId": "p1", "usage": map[string]any{"input_tokens": 11, "output_tokens": 7, "total_tokens": 18}}}, nil
	}, agent, agent)
	_ = peer
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	response, err := call[promptResponse](ctx, turn, "session/prompt", map[string]any{"sessionId": "s1", "prompt": []any{}})
	if err != nil {
		t.Fatal(err)
	}
	turn.mu.Lock()
	got := turn.text.String()
	turn.mu.Unlock()
	if got != "final" || response.Meta.Usage == nil || response.Meta.Usage.sdk().TotalTokens != 18 {
		t.Fatalf("terminal response raced output or lost usage: %q %#v", got, response)
	}
}

func TestNativePlanExitUpdatesNextTurnModeWithoutChangingPermission(t *testing.T) {
	turn, _, _ := testTurn(t)
	turn.input.RuntimeMetadata = map[string]any{"collaboration_mode": "plan", "permission_mode": "auto"}
	for _, raw := range []string{
		`{"sessionId":"s1","update":{"sessionUpdate":"current_mode_update","currentModeId":"default"}}`,
		`{"sessionId":"s1","_meta":{"isReplay":true},"update":{"sessionUpdate":"current_mode_update","currentModeId":"plan"}}`,
		`{"sessionId":"other","update":{"sessionUpdate":"current_mode_update","currentModeId":"plan"}}`,
		`{"sessionId":"s1","update":{"sessionUpdate":"current_mode_update","currentModeId":"unsupported"}}`,
	} {
		if err := turn.update(json.RawMessage(raw)); err != nil {
			t.Fatal(err)
		}
	}
	metadata := (&lease{t: turn}).metadata(turn.input)
	if metadata["collaboration_mode"] != "default" || metadata["grok_native_mode"] != "default" {
		t.Fatalf("native exit did not clear plan mode: %#v", metadata)
	}
	if _, changed := metadata["permission_mode"]; changed {
		t.Fatal("plan transition changed permission mode")
	}
}

type inputFlow struct {
	answer  userinput.Request
	pending userinput.CreatePendingInput
	waiting chan struct{}
	cancels int
}

func (s *inputFlow) CreatePending(_ context.Context, in userinput.CreatePendingInput) (userinput.Request, error) {
	if err := userinput.ValidateAskUserInput(in.Input); err != nil {
		return userinput.Request{}, err
	}
	s.pending = in
	return userinput.Request{ID: "input", ToolCallID: in.ToolCallID, ToolName: in.ToolName, Input: in.Input.(map[string]any), Status: userinput.StatusPending}, nil
}
func (*inputFlow) RegisterWaiter(string) func() { return func() {} }
func (s *inputFlow) WaitForRegisteredResponse(ctx context.Context, _ string) (userinput.Request, error) {
	if s.waiting != nil {
		close(s.waiting)
		<-ctx.Done()
		return userinput.Request{}, ctx.Err()
	}
	return s.answer, nil
}

func (s *inputFlow) Cancel(context.Context, userinput.CancelInput) (userinput.Request, error) {
	s.cancels++
	return userinput.Request{Status: userinput.StatusCanceled}, nil
}

func TestQuestionAnswersPreserveMultipleChoicesAndFreeText(t *testing.T) {
	turn, _, _ := testTurn(t)
	flow := &inputFlow{answer: userinput.Request{Status: userinput.StatusSubmitted, Result: map[string]any{"answers": []any{
		map[string]any{"question_id": "q1", "selected": []any{map[string]any{"id": "q1.o1", "label": "A"}, map[string]any{"id": "q1.o2", "label": "B"}}},
		map[string]any{"question_id": "q2", "text": "custom text"},
	}}}}
	turn.driver.userInput = flow
	out, err := turn.questions(context.Background(), json.RawMessage(`{"toolCallId":"ask","questions":[{"question":"Choose","multiSelect":true,"options":[{"label":"A"},{"label":"B"}]},{"question":"Explain"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out)
	if string(raw) != `{"annotations":{"Explain":{"notes":"custom text"}},"answers":{"Choose":["A","B"],"Explain":["Other"]},"outcome":"accepted"}` {
		t.Fatalf("answers: %s", raw)
	}
	if flow.pending.ProviderMetadata["run_id"] != "run" {
		t.Fatal("missing decision run ownership")
	}
}

func TestCloseCancelsPendingReverseRequestAndRejectsLateRequests(t *testing.T) {
	turn, _, _ := testTurn(t)
	flow := &inputFlow{waiting: make(chan struct{})}
	turn.driver.userInput = flow
	done := make(chan struct{})
	raw := json.RawMessage(`{"sessionId":"s1","toolCallId":"ask","questions":[{"question":"Continue?"}]}`)
	go func() { defer close(done); _, _ = turn.handle(context.Background(), "_x.ai/ask_user_question", raw) }()
	select {
	case <-flow.waiting:
	case <-time.After(time.Second):
		t.Fatal("input did not wait")
	}
	turn.closeDecisions()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reverse request leaked")
	}
	if flow.cancels != 1 {
		t.Fatalf("cancellations: %d", flow.cancels)
	}
	if _, err := turn.handle(context.Background(), "_x.ai/ask_user_question", raw); err == nil {
		t.Fatal("late reverse request admitted")
	}
}

type approvalFlow struct {
	selected    string
	approved    bool
	policyCalls int
	pending     approval.CreatePendingInput
}

func (s *approvalFlow) EvaluatePolicy(context.Context, approval.CreatePendingInput) (approval.Evaluation, error) {
	s.policyCalls++
	return approval.Evaluation{Decision: approval.DecisionBypass}, nil
}

func (s *approvalFlow) CreatePending(_ context.Context, in approval.CreatePendingInput) (approval.Request, error) {
	if _, ok := approval.OperationForTool(in.ToolName); !ok {
		return approval.Request{}, errors.New("unsupported operation")
	}
	s.pending = in
	return approval.Request{ID: "approval", Status: approval.StatusPending, ToolCallID: in.ToolCallID, ToolName: in.ToolName, ToolInput: in.ToolInput.(map[string]any)}, nil
}

func (*approvalFlow) Get(context.Context, string) (approval.Request, error) {
	return approval.Request{Status: approval.StatusPending}, nil
}

func (*approvalFlow) Reject(context.Context, string, string, string) (approval.Request, error) {
	return approval.Request{Status: approval.StatusRejected}, nil
}

func (s *approvalFlow) WaitForDecision(context.Context, string) (approval.Request, error) {
	status := approval.StatusRejected
	if s.approved {
		status = approval.StatusApproved
	}
	return approval.Request{Status: status, DecidedByUser: true, SelectedOptionID: s.selected}, nil
}
func (*approvalFlow) RegisterWaiter(string) func() { return func() {} }
func TestNativeApprovalRequiresDecisionAndReturnsExactOfferedOption(t *testing.T) {
	for _, tc := range []struct {
		id       string
		approved bool
		want     string
	}{{"allow-session", true, "allow-session"}, {"deny-once", false, "deny-once"}, {"invented", true, ""}} {
		turn, _, _ := testTurn(t)
		flow := &approvalFlow{selected: tc.id, approved: tc.approved}
		turn.driver.approval = flow
		response, err := turn.permission(context.Background(), json.RawMessage(`{"toolCall":{"toolCallId":"call","title":"Run shell command","rawInput":{"command":"pwd"}},"options":[{"optionId":"allow-session","kind":"allow_always","name":"Allow session"},{"optionId":"deny-once","kind":"reject_once","name":"Deny"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		outcome := response.(map[string]any)["outcome"].(map[string]any)
		if tc.want == "" {
			if outcome["outcome"] != "cancelled" {
				t.Fatalf("invented option accepted: %v", outcome)
			}
		} else if outcome["optionId"] != tc.want {
			t.Fatalf("wrong option: %v", outcome)
		}
		if flow.policyCalls != 0 || flow.pending.ToolName != "permission" {
			t.Fatal("native approval bypassed human review or used invalid operation")
		}
	}
}

type pipeProcess struct {
	net.Conn
	done chan struct{}
	once sync.Once
}

func (p *pipeProcess) Close() error {
	var err error
	p.once.Do(func() { err = p.Conn.Close(); close(p.done) })
	return err
}
func (p *pipeProcess) CloseStdin()           { _ = p.Close() }
func (p *pipeProcess) Done() <-chan struct{} { return p.done }
func (*pipeProcess) Err() error              { return nil }
func TestCancelWaitsForOriginalPromptAndFinalOutput(t *testing.T) {
	turn, agent, _ := testTurn(t)
	process := &pipeProcess{Conn: agent, done: make(chan struct{})}
	started := make(chan struct{})
	canceled := make(chan struct{})
	peer := acp.NewConnection(func(ctx context.Context, method string, _ json.RawMessage) (any, *acp.RequestError) {
		switch method {
		case "session/prompt":
			close(started)
			select {
			case <-canceled:
			case <-ctx.Done():
				return nil, acp.NewInternalError(nil)
			}
			raw := []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"last chunk"}}}}` + "\n")
			if _, err := agent.Write(raw); err != nil {
				return nil, acp.NewInternalError(nil)
			}
			return map[string]any{"stopReason": "cancelled"}, nil
		case "session/cancel":
			close(canceled)
			return nil, nil
		default:
			return nil, acp.NewMethodNotFound(method)
		}
	}, agent, agent)
	_ = peer
	lease := &lease{t: turn, proc: process}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		response, err := lease.prompt(ctx, []any{})
		if err == nil && response.StopReason != "cancelled" {
			err = errors.New("lost terminal response")
		}
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("prompt never started")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not finish")
	}
	turn.mu.Lock()
	defer turn.mu.Unlock()
	if turn.text.String() != "last chunk" {
		t.Fatal("cancel lost the final notification watermark")
	}
}

func TestPermissionModeIncludesAutoAndKeepsPlanSeparate(t *testing.T) {
	modes, err := permissionModes("auto")
	if err != nil || modes.CurrentModeID != "auto" || len(modes.AvailableModes) != 3 {
		t.Fatalf("permission modes = %+v, %v", modes, err)
	}
	if _, err := permissionModes("plan"); err == nil {
		t.Fatal("plan must use its independent control")
	}
}
