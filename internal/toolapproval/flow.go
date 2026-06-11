package toolapproval

import (
	"context"
	"errors"
	"strings"
	"time"
)

// DefaultWaitTimeout bounds how long the approval flow waits for a decision
// before rejecting the request as timed out.
const DefaultWaitTimeout = 10 * time.Minute

// FlowService is the subset of the approval service the shared flow needs.
// Both the native MCP gateway and the ACP client callbacks satisfy it with
// *Service.
type FlowService interface {
	EvaluatePolicy(ctx context.Context, input CreatePendingInput) (Evaluation, error)
	CreatePending(ctx context.Context, input CreatePendingInput) (Request, error)
	Reject(ctx context.Context, approvalID, actorID, reason string) (Request, error)
	WaitForDecision(ctx context.Context, approvalID string) (Request, error)
}

// FlowRequest parameterizes one run of the pending-approval state machine.
type FlowRequest struct {
	Input CreatePendingInput
	// Interactive reports whether a live stream can deliver the approval
	// prompt to a user. Non-interactive requests are created and immediately
	// rejected so the pending row never dangles.
	Interactive bool
	// NonInteractiveReason is the rejection reason recorded when Interactive
	// is false.
	NonInteractiveReason string
	// ForceApproval skips the policy evaluation entirely: the request always
	// requires an explicit user decision. Use for requests whose tool name is
	// synthetic (e.g. generic ACP permissions) and therefore meaningless to
	// the per-tool policy, which would otherwise bypass them.
	ForceApproval bool
	// WaitTimeout bounds the wait for a decision; zero means
	// DefaultWaitTimeout. Production callers should leave it zero — it exists
	// so tests can exercise the timeout path quickly.
	WaitTimeout time.Duration
	// Emit receives every user-visible state change (pending, then the
	// terminal snapshot). May be nil. Policy bypasses and non-interactive
	// auto-rejections are not emitted — there is no stream to show them on.
	Emit func(Request)
}

// FlowResult describes how the approval flow concluded.
type FlowResult struct {
	Approved bool
	// Status is the terminal status after defaulting (approved/rejected/...).
	Status string
	// DecisionReason carries the decider's (or the system's) reason.
	DecisionReason string
	// DecidedByUser is true only when a live decision arrived from a user;
	// system outcomes (policy bypass, non-interactive auto-reject, timeout)
	// leave it false so callers can distinguish "user said no" from "nobody
	// was there to ask".
	DecidedByUser bool
}

// RunFlow executes the pending-approval state machine shared by the native
// MCP gateway and the ACP client callbacks: evaluate policy, create a pending
// request, publish it, wait for the decision (rejecting on timeout), and
// publish the terminal snapshot.
func RunFlow(ctx context.Context, svc FlowService, flow FlowRequest) (FlowResult, error) {
	actorID := flow.Input.RequestedByChannelIdentityID
	if !flow.ForceApproval {
		eval, err := svc.EvaluatePolicy(ctx, flow.Input)
		if err != nil {
			return FlowResult{}, err
		}
		if eval.Decision == DecisionBypass {
			return FlowResult{Approved: true, Status: StatusApproved}, nil
		}
	}

	req, err := svc.CreatePending(ctx, flow.Input)
	if err != nil {
		return FlowResult{}, err
	}
	if !flow.Interactive {
		reason := strings.TrimSpace(flow.NonInteractiveReason)
		if reason == "" {
			reason = "tool execution requires approval, but this request is not attached to an interactive stream"
		}
		// Reject on a detached ctx for the same reason as the timeout path:
		// the pending row must not dangle if the caller is torn down here.
		rejectCtx, rejectCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer rejectCancel()
		rejected, rejectErr := svc.Reject(rejectCtx, req.ID, actorID, reason)
		if rejectErr != nil {
			return FlowResult{}, rejectErr
		}
		if recorded := strings.TrimSpace(rejected.DecisionReason); recorded != "" {
			reason = recorded
		}
		return FlowResult{Status: StatusRejected, DecisionReason: reason}, nil
	}

	emit := flow.Emit
	if emit == nil {
		emit = func(Request) {}
	}
	emit(req)

	waitTimeout := flow.WaitTimeout
	if waitTimeout <= 0 {
		waitTimeout = DefaultWaitTimeout
	}
	waitCtx, cancel := context.WithTimeout(ctx, waitTimeout)
	defer cancel()
	decided, err := svc.WaitForDecision(waitCtx, req.ID)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			// The caller's ctx is still live: the user simply never decided.
			// Reject on a detached ctx so the pending row cannot dangle even
			// if the caller is torn down mid-flight.
			rejectCtx, rejectCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer rejectCancel()
			rejected, rejectErr := svc.Reject(rejectCtx, req.ID, actorID, "tool approval timed out")
			if rejectErr != nil {
				return FlowResult{}, rejectErr
			}
			reason := strings.TrimSpace(rejected.DecisionReason)
			if reason == "" {
				reason = "tool approval timed out"
			}
			timeoutReq := req
			timeoutReq.Status = StatusRejected
			timeoutReq.DecisionReason = reason
			emit(timeoutReq)
			return FlowResult{Status: StatusRejected, DecisionReason: reason}, nil
		}
		return FlowResult{}, err
	}

	decisionReq := req
	if status := strings.TrimSpace(decided.Status); status != "" {
		decisionReq.Status = status
	} else {
		decisionReq.Status = StatusRejected
	}
	decisionReq.DecisionReason = decided.DecisionReason
	emit(decisionReq)
	// Only an explicit approve/reject counts as a user decision; other
	// terminal statuses a decided row can carry (expired, cancelled, a
	// concurrent waiter's timeout-reject) are system outcomes.
	decidedByUser := strings.TrimSpace(decided.Status) != "" &&
		(strings.EqualFold(decisionReq.Status, StatusApproved) || strings.EqualFold(decisionReq.Status, StatusRejected))
	return FlowResult{
		Approved:       strings.EqualFold(decisionReq.Status, StatusApproved),
		Status:         decisionReq.Status,
		DecisionReason: decisionReq.DecisionReason,
		DecidedByUser:  decidedByUser,
	}, nil
}

// NormalizedStatus defaults an empty status to pending.
func NormalizedStatus(status string) string {
	status = strings.TrimSpace(status)
	if status == "" {
		return StatusPending
	}
	return status
}

// CanApprove reports whether a request in the given status still accepts a
// decision.
func CanApprove(status string) bool {
	return strings.EqualFold(NormalizedStatus(status), StatusPending)
}

// RejectionMessage renders the agent-visible text for an unapproved flow
// result. It is shared by the native MCP gateway and the ACP client callbacks
// so both report the same — honest — outcome: "rejected by user" only when a
// live user decided (DecidedByUser), and "not approved" with the system
// reason (timeout, no interactive stream, expired) otherwise. Telling an
// agent a user rejected a request nobody saw sends it into retry loops.
func RejectionMessage(result FlowResult) string {
	reason := strings.TrimSpace(result.DecisionReason)
	if result.DecidedByUser {
		if reason == "" {
			return "tool execution rejected by user"
		}
		return "tool execution rejected by user: " + reason
	}
	if reason == "" && result.Status != "" && !strings.EqualFold(result.Status, StatusRejected) {
		reason = result.Status
	}
	if reason == "" {
		return "tool execution was not approved"
	}
	return "tool execution was not approved: " + reason
}

// RequestMetadata is the canonical wire payload describing an approval
// request. Every emitter (native gateway, ACP callbacks, stream resolver)
// must use it so the shape cannot drift.
func RequestMetadata(req Request) map[string]any {
	status := NormalizedStatus(req.Status)
	return map[string]any{
		"approval_id": req.ID,
		"short_id":    req.ShortID,
		"status":      status,
		"can_approve": CanApprove(status),
	}
}
