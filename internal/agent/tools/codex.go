package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	sdk "github.com/memohai/twilight-ai/sdk"

	acpagent "github.com/memohai/memoh/internal/acpagent"
	"github.com/memohai/memoh/internal/acpclient"
	"github.com/memohai/memoh/internal/workspace/bridge"
)

const codexDelegateToolName = "codex_delegate"

type CodexWorkspace interface {
	bridge.Provider
	bridge.WorkspaceInfoProvider
}

type CodexTaskStarter interface {
	Start(ctx context.Context, input acpagent.StartInput) (acpagent.Snapshot, error)
}

type CodexProvider struct {
	logger      *slog.Logger
	workspace   bridge.WorkspaceInfoProvider
	taskStarter CodexTaskStarter
}

func NewCodexProvider(log *slog.Logger, workspace CodexWorkspace, taskStarter ...CodexTaskStarter) *CodexProvider {
	if log == nil {
		log = slog.Default()
	}
	var starter CodexTaskStarter
	if len(taskStarter) > 0 {
		starter = taskStarter[0]
	}
	return &CodexProvider{
		logger:      log.With(slog.String("tool", "codex_delegate")),
		workspace:   workspace,
		taskStarter: starter,
	}
}

func newCodexProviderWithStarter(log *slog.Logger, workspace bridge.WorkspaceInfoProvider, starter CodexTaskStarter) *CodexProvider {
	if log == nil {
		log = slog.Default()
	}
	return &CodexProvider{logger: log.With(slog.String("tool", "codex_delegate")), workspace: workspace, taskStarter: starter}
}

func (p *CodexProvider) Tools(_ context.Context, session SessionContext) ([]sdk.Tool, error) {
	if session.IsSubagent || p.taskStarter == nil {
		return nil, nil
	}
	sess := session
	return []sdk.Tool{{
		Name:        codexDelegateToolName,
		Description: `Start a Codex ACP child session in the bot's local workspace and delegate a code task to it. Use this when the user asks Codex to inspect, edit, build, test, or otherwise work on a code project. This starts quickly; Codex continues streaming follow-up output in the same conversation. After this tool returns successfully, immediately tell the user in plain language that the task has been handed to Codex, that the rest of this conversation will be handled by Codex, and that they can send /codex stop to return to Memoh.`,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task": map[string]any{
					"type":        "string",
					"description": "Natural-language coding task to send to Codex.",
				},
				"project_path": map[string]any{
					"type":        "string",
					"description": "Optional project directory under the local workspace root. Relative paths and /data/... are resolved inside the workspace.",
				},
				"mode": map[string]any{
					"type":        "string",
					"enum":        []string{"normal"},
					"description": "Execution mode. Only normal is supported in the first-stage integration.",
					"default":     "normal",
				},
				"attachments": map[string]any{
					"type":        "array",
					"description": "Optional workspace paths or references that Codex should consider.",
					"items":       map[string]any{"type": "string"},
				},
			},
			"required": []string{"task"},
		},
		Execute: func(execCtx *sdk.ToolExecContext, input any) (any, error) {
			return p.exec(execCtx.Context, sess, inputAsMap(input))
		},
	}}, nil
}

func (p *CodexProvider) exec(ctx context.Context, session SessionContext, args map[string]any) (any, error) {
	if strings.TrimSpace(session.BotID) == "" {
		return nil, errors.New("bot_id is required")
	}
	if p.workspace != nil {
		info, err := p.workspace.WorkspaceInfo(ctx, session.BotID)
		if err != nil {
			return nil, fmt.Errorf("resolve workspace: %w", err)
		}
		if !strings.EqualFold(info.Backend, bridge.WorkspaceBackendLocal) {
			return nil, fmt.Errorf("%w: backend %q is not supported", acpclient.ErrUnsupportedWorkspace, info.Backend)
		}
	}

	task := StringArg(args, "task")
	if task == "" {
		return nil, errors.New("task is required")
	}
	mode := StringArg(args, "mode")
	if mode == "" {
		mode = "normal"
	}
	if !strings.EqualFold(mode, "normal") {
		return nil, fmt.Errorf("unsupported codex mode %q: only normal is supported", mode)
	}

	attachments := codexStringSliceArg(args, "attachments")
	if len(attachments) > 0 {
		var b strings.Builder
		b.WriteString(task)
		b.WriteString("\n\nAttachments:\n")
		for _, attachment := range attachments {
			b.WriteString("- ")
			b.WriteString(attachment)
			b.WriteByte('\n')
		}
		task = b.String()
	}

	result, err := p.taskStarter.Start(ctx, acpagent.StartInput{
		AgentID:              acpagent.CodexAgentID,
		BotID:                session.BotID,
		ChatID:               session.ChatID,
		SessionID:            session.SessionID,
		ChannelIdentityID:    session.ChannelIdentityID,
		CurrentPlatform:      session.CurrentPlatform,
		ReplyTarget:          session.ReplyTarget,
		Task:                 task,
		ProjectPath:          StringArg(args, "project_path"),
		UserMessagePersisted: true,
	})
	if err != nil {
		return nil, err
	}
	return formatCodexStartResult(result), nil
}

func codexStringSliceArg(args map[string]any, key string) []string {
	if args == nil {
		return nil
	}
	raw, ok := args[key]
	if !ok || raw == nil {
		return nil
	}
	switch value := raw.(type) {
	case []string:
		out := make([]string, 0, len(value))
		for _, item := range value {
			if s := strings.TrimSpace(item); s != "" {
				out = append(out, s)
			}
		}
		return out
	case []any:
		out := make([]string, 0, len(value))
		for _, item := range value {
			if s := strings.TrimSpace(fmt.Sprintf("%v", item)); s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		if s := strings.TrimSpace(fmt.Sprintf("%v", raw)); s != "" {
			return []string{s}
		}
		return nil
	}
}

func formatCodexStartResult(result acpagent.Snapshot) map[string]any {
	agentName := firstNonEmpty(result.AgentName, acpagent.CodexAgentName)
	return map[string]any{
		"status":         "started",
		"task_id":        result.TaskID,
		"agent_id":       result.AgentID,
		"agent_name":     agentName,
		"session_id":     result.SessionID,
		"project_path":   result.ProjectPath,
		"acp_session_id": result.ACPSession,
		"message":        fmt.Sprintf("已将任务交给 %s。接下来这个会话将由 %s 和用户沟通；如果用户想切回 Memoh，可以发送 /codex stop。", agentName, agentName),
	}
}
