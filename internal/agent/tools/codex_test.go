package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	acpagent "github.com/memohai/memoh/internal/acpagent"
	"github.com/memohai/memoh/internal/workspace/bridge"
)

type stubCodexStarter struct {
	input acpagent.StartInput
	err   error
}

func (s *stubCodexStarter) Start(_ context.Context, input acpagent.StartInput) (acpagent.Snapshot, error) {
	s.input = input
	if s.err != nil {
		return acpagent.Snapshot{}, s.err
	}
	return acpagent.Snapshot{
		TaskID:      "task-1",
		AgentID:     acpagent.CodexAgentID,
		AgentName:   acpagent.CodexAgentName,
		SessionID:   firstNonEmpty(input.SessionID, input.ChatID),
		ProjectPath: "/workspace/app",
		ACPSession:  "acp-session-1",
		Status:      "running",
	}, nil
}

type stubWorkspaceInfo struct {
	info bridge.WorkspaceInfo
	err  error
}

func (w stubWorkspaceInfo) WorkspaceInfo(context.Context, string) (bridge.WorkspaceInfo, error) {
	return w.info, w.err
}

func TestCodexProviderExecDelegatesTask(t *testing.T) {
	starter := &stubCodexStarter{}
	provider := newCodexProviderWithStarter(nil, stubWorkspaceInfo{
		info: bridge.WorkspaceInfo{Backend: bridge.WorkspaceBackendLocal, DefaultWorkDir: "/workspace"},
	}, starter)

	out, err := provider.exec(context.Background(), SessionContext{BotID: "bot-1"}, map[string]any{
		"task":         "fix tests",
		"project_path": "app",
		"attachments":  []any{"logs/test.txt"},
	})
	if err != nil {
		t.Fatalf("exec() error = %v", err)
	}
	if starter.input.BotID != "bot-1" || starter.input.ProjectPath != "app" {
		t.Fatalf("StartInput = %+v", starter.input)
	}
	if starter.input.AgentID != acpagent.CodexAgentID {
		t.Fatalf("StartInput.AgentID = %q, want %q", starter.input.AgentID, acpagent.CodexAgentID)
	}
	if !strings.Contains(starter.input.Task, "fix tests") || !strings.Contains(starter.input.Task, "logs/test.txt") {
		t.Fatalf("StartInput.Task = %q", starter.input.Task)
	}
	payload := out.(map[string]any)
	if payload["status"] != "started" || payload["task_id"] != "task-1" {
		t.Fatalf("output = %#v", payload)
	}
	if !strings.Contains(payload["message"].(string), "接下来这个会话将由 Codex") {
		t.Fatalf("message = %q", payload["message"])
	}
}

func TestCodexProviderRejectsUnsupportedModeAndWorkspace(t *testing.T) {
	provider := newCodexProviderWithStarter(nil, stubWorkspaceInfo{
		info: bridge.WorkspaceInfo{Backend: bridge.WorkspaceBackendLocal, DefaultWorkDir: "/workspace"},
	}, &stubCodexStarter{})
	if _, err := provider.exec(context.Background(), SessionContext{BotID: "bot-1"}, map[string]any{
		"task": "fix tests",
		"mode": "plan",
	}); err == nil {
		t.Fatal("expected unsupported mode error")
	}

	provider = newCodexProviderWithStarter(nil, stubWorkspaceInfo{
		info: bridge.WorkspaceInfo{Backend: bridge.WorkspaceBackendContainer, DefaultWorkDir: "/data"},
	}, &stubCodexStarter{})
	if _, err := provider.exec(context.Background(), SessionContext{BotID: "bot-1"}, map[string]any{"task": "fix tests"}); err == nil {
		t.Fatal("expected non-local workspace error")
	}
}

func TestCodexProviderPropagatesStarterError(t *testing.T) {
	want := errors.New("missing codex-acp")
	provider := newCodexProviderWithStarter(nil, stubWorkspaceInfo{
		info: bridge.WorkspaceInfo{Backend: bridge.WorkspaceBackendLocal, DefaultWorkDir: "/workspace"},
	}, &stubCodexStarter{err: want})
	if _, err := provider.exec(context.Background(), SessionContext{BotID: "bot-1"}, map[string]any{"task": "fix tests"}); !errors.Is(err, want) {
		t.Fatalf("exec() error = %v, want %v", err, want)
	}
}
