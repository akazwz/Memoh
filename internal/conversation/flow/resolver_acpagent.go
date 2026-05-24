package flow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	acpagent "github.com/memohai/memoh/internal/acpagent"
	"github.com/memohai/memoh/internal/channel"
	commandpkg "github.com/memohai/memoh/internal/command"
	"github.com/memohai/memoh/internal/conversation"
)

// ACP agents are external child agents, not configured Memoh chat model rows.
// Leave model_id empty so message persistence does not try to parse it as a UUID.
const acpAgentModelID = ""

type acpAgentSlashCommand struct {
	AgentID     string
	AgentName   string
	Action      string
	Task        string
	ProjectPath string
}

func (r *Resolver) routeACPAgentMessage(ctx context.Context, req conversation.ChatRequest) (bool, error) {
	if r == nil || r.acpAgentService == nil {
		return false, nil
	}
	sessionID := firstNonEmpty(req.SessionID, req.ChatID)
	if cmd, ok, err := r.parseACPAgentSlashCommand(req.Query); ok {
		if err != nil {
			text := acpAgentCommandUsage(cmd)
			r.publishACPAgentControlMessage(ctx, req, sessionID, text)
			return true, nil
		}
		switch cmd.Action {
		case "start":
			return true, r.startACPAgentFromCommand(ctx, req, sessionID, cmd)
		case "stop":
			r.acpAgentService.Stop(req.BotID, sessionID)
			text := fmt.Sprintf("已停止 %s 子会话，后续消息会回到 Memoh Agent。", cmd.AgentName)
			r.publishACPAgentControlMessage(ctx, req, sessionID, text)
			return true, nil
		default:
			text := acpAgentCommandUsage(cmd)
			r.publishACPAgentControlMessage(ctx, req, sessionID, text)
			return true, nil
		}
	}

	active, ok := r.acpAgentService.ActiveSnapshot(req.BotID, sessionID)
	if !ok {
		return false, nil
	}
	if isACPAgentStopRequest(req.Query, active) {
		r.acpAgentService.Stop(req.BotID, sessionID)
		text := fmt.Sprintf("已停止 %s 子会话，后续消息会回到 Memoh Agent。", active.AgentName)
		r.publishACPAgentControlMessage(ctx, req, sessionID, text)
		return true, nil
	}
	text := acpAgentPromptWithAttachments(req.Query, req.Attachments)
	if strings.TrimSpace(text) == "" {
		return true, errors.New("message text or attachments required")
	}
	_, err := r.acpAgentService.Send(ctx, acpagent.SendInput{
		BotID:                req.BotID,
		ChatID:               firstNonEmpty(req.ChatID, req.BotID),
		SessionID:            sessionID,
		ChannelIdentityID:    firstNonEmpty(req.SourceChannelIdentityID, req.UserID),
		CurrentPlatform:      req.CurrentChannel,
		ReplyTarget:          req.ReplyTarget,
		ConversationType:     req.ConversationType,
		Text:                 text,
		UserMessagePersisted: false,
	})
	if err != nil {
		return true, err
	}
	return true, nil
}

func (r *Resolver) startACPAgentFromCommand(ctx context.Context, req conversation.ChatRequest, sessionID string, cmd acpAgentSlashCommand) error {
	if r == nil || r.acpAgentService == nil {
		return errors.New("ACP agent service is not configured")
	}
	if active, ok := r.acpAgentService.ActiveSnapshot(req.BotID, sessionID); ok {
		text := fmt.Sprintf("%s 子会话已经在运行。后续消息会继续发送给 %s；发送 /%s stop 可以切回 Memoh。", active.AgentName, active.AgentName, active.AgentID)
		r.publishACPAgentControlMessage(ctx, req, sessionID, text)
		return nil
	}
	task := strings.TrimSpace(cmd.Task)
	if task == "" {
		task = fmt.Sprintf("请接管这个会话，等待用户的下一条指令。先简短告诉用户 %s 已准备好。", cmd.AgentName)
	}
	task = acpAgentPromptWithAttachments(task, req.Attachments)
	handoff := fmt.Sprintf("已启动 %s ACP 子会话。接下来这个会话将由 %s 和你沟通；发送 /%s stop 可以切回 Memoh。", cmd.AgentName, cmd.AgentName, cmd.AgentID)
	_, err := r.acpAgentService.Start(ctx, acpagent.StartInput{
		AgentID:              cmd.AgentID,
		BotID:                req.BotID,
		ChatID:               firstNonEmpty(req.ChatID, req.BotID),
		SessionID:            sessionID,
		ChannelIdentityID:    firstNonEmpty(req.SourceChannelIdentityID, req.UserID),
		CurrentPlatform:      req.CurrentChannel,
		ReplyTarget:          req.ReplyTarget,
		ConversationType:     req.ConversationType,
		Task:                 task,
		ProjectPath:          cmd.ProjectPath,
		HandoffText:          handoff,
		UserMessagePersisted: true,
	})
	if err != nil {
		return err
	}
	r.persistACPAgentControlMessage(ctx, req, handoff)
	return nil
}

func (r *Resolver) publishACPAgentControlMessage(ctx context.Context, req conversation.ChatRequest, sessionID, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	r.publishBackgroundAgentStream(req.BotID, sessionID, map[string]any{"type": "start"})
	r.publishBackgroundAgentStream(req.BotID, sessionID, map[string]any{
		"type": "message",
		"data": conversation.UIMessage{
			ID:      0,
			Type:    conversation.UIMessageText,
			Content: text,
		},
	})
	r.publishBackgroundAgentStream(req.BotID, sessionID, map[string]any{"type": "end"})
	r.persistACPAgentControlMessage(ctx, req, text)
}

func (r *Resolver) persistACPAgentControlMessage(ctx context.Context, req conversation.ChatRequest, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	messages := []conversation.ModelMessage{
		{
			Role:    "user",
			Content: conversation.NewTextContent(req.Query),
		},
		{
			Role:    "assistant",
			Content: conversation.NewTextContent(text),
		},
	}
	req.RawQuery = firstNonEmpty(req.RawQuery, req.Query)
	if err := r.storeRound(context.WithoutCancel(ctx), req, messages, acpAgentModelID); err != nil {
		r.logger.Warn("persist ACP agent control message failed",
			slog.String("bot_id", req.BotID),
			slog.String("session_id", req.SessionID),
			slog.Any("error", err),
		)
	}
}

func (r *Resolver) persistACPAgentCompletion(ctx context.Context, event acpagent.Completion) {
	text := strings.TrimSpace(event.Text)
	if text == "" {
		return
	}
	req := conversation.ChatRequest{
		BotID:                   event.BotID,
		ChatID:                  firstNonEmpty(event.ChatID, event.BotID),
		SessionID:               event.SessionID,
		SourceChannelIdentityID: event.ChannelIdentityID,
		UserID:                  event.ChannelIdentityID,
		CurrentChannel:          event.CurrentPlatform,
		ReplyTarget:             event.ReplyTarget,
		ConversationType:        event.ConversationType,
		Query:                   event.Prompt,
		RawQuery:                event.Prompt,
		UserMessagePersisted:    event.UserMessagePersisted,
	}
	messages := make([]conversation.ModelMessage, 0, 2)
	if !event.UserMessagePersisted {
		messages = append(messages, conversation.ModelMessage{
			Role:    "user",
			Content: conversation.NewTextContent(event.Prompt),
		})
	}
	messages = append(messages, conversation.ModelMessage{
		Role:    "assistant",
		Content: acpAgentTextContent(text, event.AgentID, event.AgentName),
	})
	if err := r.storeRound(context.WithoutCancel(ctx), req, messages, acpAgentModelID); err != nil {
		r.logger.Warn("persist ACP agent completion failed",
			slog.String("bot_id", event.BotID),
			slog.String("session_id", event.SessionID),
			slog.Any("error", err),
		)
	}
	if r.outboundFn != nil &&
		strings.TrimSpace(event.CurrentPlatform) != "" &&
		strings.TrimSpace(event.ReplyTarget) != "" &&
		!strings.EqualFold(strings.TrimSpace(event.CurrentPlatform), channel.ChannelTypeLocal.String()) {
		if err := r.outboundFn(ctx, event.BotID, event.CurrentPlatform, event.ReplyTarget, text); err != nil {
			r.logger.Warn("deliver ACP agent completion failed",
				slog.String("bot_id", event.BotID),
				slog.String("platform", event.CurrentPlatform),
				slog.String("reply_target", event.ReplyTarget),
				slog.Any("error", err),
			)
		}
	}
}

func acpAgentTextContent(text, agentID, agentName string) json.RawMessage {
	return conversation.NewPartsContent([]conversation.ContentPart{{
		Type:     "text",
		Text:     text,
		Metadata: acpAgentUIMetadata(agentID, agentName),
	}})
}

func acpAgentUIMetadata(agentID, agentName string) map[string]any {
	agentID = strings.ToLower(strings.TrimSpace(firstNonEmpty(agentID, acpagent.CodexAgentID)))
	agentName = firstNonEmpty(agentName, acpagent.CodexAgentName)
	return map[string]any{
		"source":   acpagent.SourceACPAgent,
		"agent_id": agentID,
		"agent":    agentName,
	}
}

func isACPAgentStopRequest(text string, active acpagent.Snapshot) bool {
	normalized := strings.ToLower(strings.TrimSpace(text))
	if normalized == "" {
		return false
	}
	agentID := strings.ToLower(strings.TrimSpace(active.AgentID))
	agentName := strings.ToLower(strings.TrimSpace(active.AgentName))
	if agentID == "" {
		agentID = acpagent.CodexAgentID
	}
	names := []string{agentID}
	if agentName != "" && agentName != agentID {
		names = append(names, agentName)
	}
	for _, name := range names {
		spacedStop := "停止 " + name
		joinedStop := "停止" + name
		spacedExit := "退出 " + name
		joinedExit := "退出" + name
		switch normalized {
		case "/" + agentID + " stop", name + " stop", "stop " + name, spacedStop, joinedStop, spacedExit, joinedExit:
			return true
		}
		if strings.Contains(normalized, spacedStop) ||
			strings.Contains(normalized, joinedStop) ||
			strings.Contains(normalized, spacedExit) ||
			strings.Contains(normalized, joinedExit) {
			return true
		}
	}
	return false
}

func (r *Resolver) parseACPAgentSlashCommand(text string) (acpAgentSlashCommand, bool, error) {
	cmdText := commandpkg.ExtractCommandText(text)
	if strings.TrimSpace(cmdText) == "" {
		return acpAgentSlashCommand{}, false, nil
	}
	parsed, err := commandpkg.Parse(cmdText)
	if err != nil {
		return acpAgentSlashCommand{}, false, nil
	}
	if r == nil || r.acpAgentService == nil {
		return acpAgentSlashCommand{}, false, nil
	}
	profile, ok := r.acpAgentService.Profile(parsed.Resource)
	if !ok {
		return acpAgentSlashCommand{}, false, nil
	}
	cmd := acpAgentSlashCommand{
		AgentID:   profile.ID,
		AgentName: profile.DisplayName,
	}
	action := strings.TrimSpace(parsed.Action)
	if action == "" {
		return cmd, true, errors.New("missing ACP agent action")
	}
	switch action {
	case "start":
		projectPath, task := parseACPAgentStartArgs(parsed.Args)
		cmd.Action = action
		cmd.ProjectPath = projectPath
		cmd.Task = task
		return cmd, true, nil
	case "stop":
		cmd.Action = action
		return cmd, true, nil
	default:
		return cmd, true, errors.New("unknown ACP agent action")
	}
}

func parseACPAgentStartArgs(args []string) (string, string) {
	taskParts := make([]string, 0, len(args))
	projectPath := ""
	for i := 0; i < len(args); i++ {
		arg := strings.TrimSpace(args[i])
		if arg == "" {
			continue
		}
		switch {
		case arg == "--project" || arg == "-p":
			if i+1 < len(args) {
				projectPath = strings.TrimSpace(args[i+1])
				i++
			}
		case strings.HasPrefix(arg, "--project="):
			projectPath = strings.TrimSpace(strings.TrimPrefix(arg, "--project="))
		default:
			taskParts = append(taskParts, arg)
		}
	}
	return projectPath, strings.TrimSpace(strings.Join(taskParts, " "))
}

func acpAgentCommandUsage(cmd acpAgentSlashCommand) string {
	agentID := firstNonEmpty(cmd.AgentID, acpagent.CodexAgentID)
	agentName := firstNonEmpty(cmd.AgentName, acpagent.CodexAgentName)
	return fmt.Sprintf("用法：/%s start [--project <path>] [任务]\n停止 %s：/%s stop", agentID, agentName, agentID)
}

func acpAgentPromptWithAttachments(query string, attachments []conversation.ChatAttachment) string {
	text := strings.TrimSpace(query)
	if len(attachments) == 0 {
		return text
	}
	var b strings.Builder
	if text != "" {
		b.WriteString(text)
		b.WriteString("\n\n")
	}
	b.WriteString("Attachments:\n")
	for _, att := range attachments {
		value := firstNonEmpty(att.Path, att.URL, att.ContentHash, att.PlatformKey, att.Name)
		if value == "" {
			value = strings.TrimSpace(att.Type)
		}
		if value == "" {
			continue
		}
		b.WriteString("- ")
		b.WriteString(value)
		if att.Mime != "" {
			b.WriteString(" (")
			b.WriteString(att.Mime)
			b.WriteString(")")
		}
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String())
}

func isCodexStopRequest(text string) bool {
	return isACPAgentStopRequest(text, acpagent.Snapshot{AgentID: acpagent.CodexAgentID, AgentName: acpagent.CodexAgentName})
}

func parseCodexSlashCommand(text string) (acpAgentSlashCommand, bool, error) {
	svc := acpagent.NewService(nil, nil)
	r := &Resolver{acpAgentService: svc}
	return r.parseACPAgentSlashCommand(text)
}
