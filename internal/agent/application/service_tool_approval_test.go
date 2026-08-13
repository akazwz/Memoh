package application

import (
	"context"
	"errors"
	"testing"

	toolapproval "github.com/memohai/memoh/internal/agent/decision/approval"
	"github.com/memohai/memoh/internal/bots"
)

type recordingToolApprovalPermissionChecker struct {
	permission string
	allow      bool
}

func (c *recordingToolApprovalPermissionChecker) HasBotPermission(_ context.Context, _, _, permission string) (bool, error) {
	c.permission = permission
	return c.allow, nil
}

func TestAuthorizeToolApprovalResponseMapsOperationPermission(t *testing.T) {
	t.Parallel()

	cases := []struct {
		operation  string
		permission string
	}{
		{operation: toolapproval.OperationRead, permission: bots.PermissionWorkspaceRead},
		{operation: toolapproval.OperationWrite, permission: bots.PermissionWorkspaceWrite},
		{operation: toolapproval.OperationExec, permission: bots.PermissionWorkspaceExec},
		{operation: toolapproval.OperationPermission, permission: bots.PermissionWorkspaceExec},
	}
	for _, tc := range cases {
		t.Run(tc.operation, func(t *testing.T) {
			t.Parallel()
			checker := &recordingToolApprovalPermissionChecker{allow: true}
			resolver := &Service{botPermissions: checker}
			err := resolver.authorizeToolApprovalResponse(context.Background(), toolapproval.Request{
				BotID:     "bot-1",
				Operation: tc.operation,
			}, ToolApprovalResponseInput{ActorUserID: "user-1"})
			if err != nil {
				t.Fatalf("authorizeToolApprovalResponse() error = %v", err)
			}
			if checker.permission != tc.permission {
				t.Fatalf("checked permission = %q, want %q", checker.permission, tc.permission)
			}
		})
	}
}

func TestAuthorizeToolApprovalResponseFailsClosed(t *testing.T) {
	t.Parallel()

	checker := &recordingToolApprovalPermissionChecker{allow: true}
	resolver := &Service{botPermissions: checker}
	for _, target := range []toolapproval.Request{
		{BotID: "bot-1", Operation: "unknown"},
		{Operation: toolapproval.OperationRead},
	} {
		if err := resolver.authorizeToolApprovalResponse(context.Background(), target, ToolApprovalResponseInput{ActorUserID: "user-1"}); !errors.Is(err, toolapproval.ErrForbidden) {
			t.Fatalf("authorizeToolApprovalResponse(%+v) error = %v, want forbidden", target, err)
		}
	}

	checker.allow = false
	if err := resolver.authorizeToolApprovalResponse(context.Background(), toolapproval.Request{
		BotID:     "bot-1",
		Operation: toolapproval.OperationWrite,
	}, ToolApprovalResponseInput{ActorUserID: "user-1"}); !errors.Is(err, toolapproval.ErrForbidden) {
		t.Fatalf("denied permission error = %v, want forbidden", err)
	}
}

func TestLegacyPermissionOptionID(t *testing.T) {
	t.Parallel()

	options := []toolapproval.PermissionOption{
		{ID: "allow-session", Kind: toolapproval.OptionKindAllowAlways},
		{ID: "deny-once", Kind: toolapproval.OptionKindRejectOnce},
		{ID: "allow-once", Kind: toolapproval.OptionKindAllowOnce},
	}
	if got, err := legacyPermissionOptionID(options, "approve"); err != nil || got != "allow-once" {
		t.Fatalf("legacy approve = %q, %v", got, err)
	}
	// Without allow_once, the sole allow option is the only possible grant, so
	// a binary approve selects it instead of dead-ending the request.
	if got, err := legacyPermissionOptionID(options[:2], "approve"); err != nil || got != "allow-session" {
		t.Fatalf("approve without allow_once = %q, %v, want allow-session", got, err)
	}
	ambiguous := []toolapproval.PermissionOption{
		{ID: "allow-session", Kind: toolapproval.OptionKindAllowAlways},
		{ID: "allow-project", Kind: toolapproval.OptionKindAllowAlways},
	}
	if got, err := legacyPermissionOptionID(ambiguous, "approve"); got != "" || !errors.Is(err, toolapproval.ErrOptionUnavailable) {
		t.Fatalf("ambiguous approve = %q, %v, want option-unavailable", got, err)
	}
	if got, err := legacyPermissionOptionID(options[2:], "reject"); got != "" || err != nil {
		t.Fatalf("reject without reject_once = %q, %v", got, err)
	}
	// Rejection is always safe: an ambiguous set of several reject_once
	// options degrades to the binary reject (cancelled outcome downstream)
	// instead of dead-ending the request.
	ambiguousReject := []toolapproval.PermissionOption{
		{ID: "reject-a", Kind: toolapproval.OptionKindRejectOnce},
		{ID: "reject-b", Kind: toolapproval.OptionKindRejectOnce},
	}
	if got, err := legacyPermissionOptionID(ambiguousReject, "reject"); got != "" || err != nil {
		t.Fatalf("ambiguous reject = %q, %v, want binary fallback", got, err)
	}
}
