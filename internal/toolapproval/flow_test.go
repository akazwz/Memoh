package toolapproval

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeFlowService struct {
	evaluation   Evaluation
	evalErr      error
	created      Request
	createErr    error
	decided      Request
	waitErr      error
	waitBlocks   bool
	rejectCalls  []string
	rejectResult Request
	rejectErr    error
}

func (f *fakeFlowService) EvaluatePolicy(context.Context, CreatePendingInput) (Evaluation, error) {
	return f.evaluation, f.evalErr
}

func (f *fakeFlowService) CreatePending(_ context.Context, input CreatePendingInput) (Request, error) {
	if f.createErr != nil {
		return Request{}, f.createErr
	}
	req := f.created
	if req.ID == "" {
		req.ID = "approval-1"
	}
	req.ToolCallID = input.ToolCallID
	req.ToolName = input.ToolName
	req.Status = StatusPending
	return req, f.createErr
}

func (f *fakeFlowService) Reject(_ context.Context, approvalID, _, reason string) (Request, error) {
	f.rejectCalls = append(f.rejectCalls, approvalID+":"+reason)
	if f.rejectErr != nil {
		return Request{}, f.rejectErr
	}
	rejected := f.rejectResult
	rejected.Status = StatusRejected
	if rejected.DecisionReason == "" {
		rejected.DecisionReason = reason
	}
	return rejected, nil
}

func (f *fakeFlowService) WaitForDecision(ctx context.Context, _ string) (Request, error) {
	if f.waitBlocks {
		<-ctx.Done()
		return Request{}, ctx.Err()
	}
	return f.decided, f.waitErr
}

func flowInput() FlowRequest {
	return FlowRequest{
		Input: CreatePendingInput{
			BotID:                        "bot-1",
			SessionID:                    "session-1",
			ToolCallID:                   "call-1",
			ToolName:                     "exec",
			RequestedByChannelIdentityID: "channel-1",
		},
		Interactive: true,
	}
}

func TestRunFlowPolicyBypass(t *testing.T) {
	t.Parallel()
	svc := &fakeFlowService{evaluation: Evaluation{Decision: DecisionBypass}}
	var emitted []Request
	flow := flowInput()
	flow.Emit = func(req Request) { emitted = append(emitted, req) }
	result, err := RunFlow(context.Background(), svc, flow)
	if err != nil || !result.Approved {
		t.Fatalf("RunFlow() = %+v, %v; want approved", result, err)
	}
	if len(emitted) != 0 {
		t.Fatalf("bypass emitted %d events, want 0", len(emitted))
	}
}

func TestRunFlowForceApprovalSkipsPolicyBypass(t *testing.T) {
	t.Parallel()
	// The policy would bypass this request (unknown tool names evaluate to
	// bypass); ForceApproval must still create a pending request and ask.
	svc := &fakeFlowService{
		evaluation: Evaluation{Decision: DecisionBypass},
		decided:    Request{ID: "approval-1", Status: StatusApproved},
	}
	var emitted []Request
	flow := flowInput()
	flow.ForceApproval = true
	flow.Emit = func(req Request) { emitted = append(emitted, req) }
	result, err := RunFlow(context.Background(), svc, flow)
	if err != nil || !result.Approved || !result.DecidedByUser {
		t.Fatalf("RunFlow() = %+v, %v; want user-approved despite bypass policy", result, err)
	}
	if len(emitted) != 2 {
		t.Fatalf("emitted %d events, want pending+approved (flow must not bypass)", len(emitted))
	}
}

func TestRunFlowNonInteractiveAutoRejects(t *testing.T) {
	t.Parallel()
	svc := &fakeFlowService{evaluation: Evaluation{Decision: DecisionNeedsApproval}}
	flow := flowInput()
	flow.Interactive = false
	flow.NonInteractiveReason = "no stream"
	result, err := RunFlow(context.Background(), svc, flow)
	if err != nil || result.Approved {
		t.Fatalf("RunFlow() = %+v, %v; want rejected", result, err)
	}
	if result.Status != StatusRejected || result.DecisionReason != "no stream" || result.DecidedByUser {
		t.Fatalf("result = %+v, want system rejection with reason", result)
	}
	if len(svc.rejectCalls) != 1 {
		t.Fatalf("reject calls = %v, want 1", svc.rejectCalls)
	}
}

func TestRunFlowApprovedEmitsPendingAndDecision(t *testing.T) {
	t.Parallel()
	svc := &fakeFlowService{
		evaluation: Evaluation{Decision: DecisionNeedsApproval},
		decided:    Request{ID: "approval-1", Status: StatusApproved},
	}
	var emitted []Request
	flow := flowInput()
	flow.Emit = func(req Request) { emitted = append(emitted, req) }
	result, err := RunFlow(context.Background(), svc, flow)
	if err != nil || !result.Approved || !result.DecidedByUser {
		t.Fatalf("RunFlow() = %+v, %v; want approved by user", result, err)
	}
	if len(emitted) != 2 || emitted[0].Status != StatusPending || emitted[1].Status != StatusApproved {
		t.Fatalf("emitted = %+v, want pending then approved", emitted)
	}
	if emitted[1].ToolCallID != "call-1" || emitted[1].ToolName != "exec" {
		t.Fatalf("decision snapshot lost request identity: %+v", emitted[1])
	}
}

func TestRunFlowEmptyDecisionStatusDefaultsToRejected(t *testing.T) {
	t.Parallel()
	svc := &fakeFlowService{
		evaluation: Evaluation{Decision: DecisionNeedsApproval},
		decided:    Request{ID: "approval-1", Status: "", DecisionReason: "because"},
	}
	result, err := RunFlow(context.Background(), svc, flowInput())
	if err != nil || result.Approved {
		t.Fatalf("RunFlow() = %+v, %v; want rejected", result, err)
	}
	if result.Status != StatusRejected || result.DecisionReason != "because" {
		t.Fatalf("result = %+v, want rejected/because", result)
	}
}

func TestRunFlowTimeoutRejectsAndEmits(t *testing.T) {
	t.Parallel()
	svc := &fakeFlowService{
		evaluation: Evaluation{Decision: DecisionNeedsApproval},
		waitBlocks: true,
	}
	var emitted []Request
	flow := flowInput()
	flow.WaitTimeout = 30 * time.Millisecond
	flow.Emit = func(req Request) { emitted = append(emitted, req) }
	result, err := RunFlow(context.Background(), svc, flow)
	if err != nil || result.Approved {
		t.Fatalf("RunFlow() = %+v, %v; want timeout rejection", result, err)
	}
	if result.Status != StatusRejected || result.DecisionReason != "tool approval timed out" || result.DecidedByUser {
		t.Fatalf("result = %+v, want timed-out system rejection", result)
	}
	if len(svc.rejectCalls) != 1 {
		t.Fatalf("reject calls = %v, want 1", svc.rejectCalls)
	}
	if len(emitted) != 2 || emitted[1].Status != StatusRejected {
		t.Fatalf("emitted = %+v, want pending then rejected", emitted)
	}
}

func TestRunFlowCallerCancellationPropagates(t *testing.T) {
	t.Parallel()
	svc := &fakeFlowService{
		evaluation: Evaluation{Decision: DecisionNeedsApproval},
		waitBlocks: true,
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	flow := flowInput()
	flow.WaitTimeout = time.Minute
	_, err := RunFlow(ctx, svc, flow)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunFlow() err = %v, want context.Canceled", err)
	}
	// Caller cancellation is not a timeout: the pending row must not be
	// auto-rejected here (the user may still decide via another surface).
	if len(svc.rejectCalls) != 0 {
		t.Fatalf("reject calls = %v, want 0 on caller cancellation", svc.rejectCalls)
	}
}

func TestNormalizedStatusAndCanApprove(t *testing.T) {
	t.Parallel()
	if NormalizedStatus("") != StatusPending || NormalizedStatus(" approved ") != "approved" {
		t.Fatal("NormalizedStatus defaulting broken")
	}
	if !CanApprove("") || !CanApprove("Pending") || CanApprove(StatusApproved) || CanApprove(StatusRejected) {
		t.Fatal("CanApprove semantics broken")
	}
}

func TestRequestMetadataShape(t *testing.T) {
	t.Parallel()
	meta := RequestMetadata(Request{ID: "a-1", ShortID: 7})
	if meta["approval_id"] != "a-1" || meta["short_id"] != 7 || meta["status"] != StatusPending || meta["can_approve"] != true {
		t.Fatalf("metadata = %#v", meta)
	}
	decided := RequestMetadata(Request{ID: "a-1", ShortID: 7, Status: StatusApproved})
	if decided["status"] != StatusApproved || decided["can_approve"] != false {
		t.Fatalf("decided metadata = %#v", decided)
	}
}

func TestRejectionMessage(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		result FlowResult
		want   string
	}{
		{
			name:   "user rejected with reason",
			result: FlowResult{Status: StatusRejected, DecidedByUser: true, DecisionReason: "too risky"},
			want:   "tool execution rejected by user: too risky",
		},
		{
			name:   "user rejected without reason",
			result: FlowResult{Status: StatusRejected, DecidedByUser: true},
			want:   "tool execution rejected by user",
		},
		{
			name:   "timeout reject is a system outcome",
			result: FlowResult{Status: StatusRejected, DecisionReason: "tool approval timed out"},
			want:   "tool execution was not approved: tool approval timed out",
		},
		{
			name:   "non-rejected terminal status without reason",
			result: FlowResult{Status: "expired"},
			want:   "tool execution was not approved: expired",
		},
		{
			name:   "no status no reason",
			result: FlowResult{},
			want:   "tool execution was not approved",
		},
	}
	for _, tc := range cases {
		if got := RejectionMessage(tc.result); got != tc.want {
			t.Fatalf("%s: RejectionMessage() = %q, want %q", tc.name, got, tc.want)
		}
	}
}
