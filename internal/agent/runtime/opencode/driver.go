// Package opencode implements the official OpenCode HTTP/SSE runtime.
package opencode

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/felinics/memoh/internal/agent/decision/approval"
	userinput "github.com/felinics/memoh/internal/agent/decision/input"
	"github.com/felinics/memoh/internal/agent/runtime/external"
	"github.com/felinics/memoh/internal/agent/runtime/opencode/opencodecfg"
	"github.com/felinics/memoh/internal/agent/runtime/toolmount"
	"github.com/felinics/memoh/internal/agentcredential"
	"github.com/felinics/memoh/internal/apperror"
	"github.com/felinics/memoh/internal/botagents"
	"github.com/felinics/memoh/internal/runtimekind"
	"github.com/felinics/memoh/internal/workspace/bridge"
)

const (
	RuntimeType          = string(runtimekind.OpenCode)
	metadataSessionIDKey = "opencode_session_id"
	metadataMessageIDKey = "opencode_message_id"
)

type BridgeSource interface {
	MCPClient(context.Context, string) (*bridge.Client, error)
	WorkspaceInfo(context.Context, string) (bridge.WorkspaceInfo, error)
}
type ApprovalService interface {
	approval.FlowService
	RegisterWaiter(string) func()
}

type Driver struct {
	bridges     BridgeSource
	agents      *botagents.Service
	credentials *agentcredential.Service
	approval    ApprovalService
	userInput   userinput.FlowService
	toolGateway toolmount.Gateway
	launchers   external.LauncherResolver
	logger      *slog.Logger
}

func NewDriver(bridges BridgeSource, agents *botagents.Service, credentials *agentcredential.Service, approvals ApprovalService, questions userinput.FlowService, gateway toolmount.Gateway, logger *slog.Logger) *Driver {
	return &Driver{bridges: bridges, agents: agents, credentials: credentials, approval: approvals, userInput: questions, toolGateway: gateway, logger: logger.With(slog.String("runtime", RuntimeType))}
}
func (*Driver) RuntimeType() string { return RuntimeType }

func unavailable(err error) error {
	return apperror.Wrap(apperror.CodeExternalRuntimeUnavailable, err, map[string]string{"runtime": RuntimeType})
}

func (d *Driver) resolve(ctx context.Context, botID, agentID string) (opencodecfg.Config, *bridge.Client, bridge.WorkspaceInfo, string, error) {
	var info bridge.WorkspaceInfo
	agent, err := d.agents.Get(ctx, botID, agentID)
	if err != nil {
		return opencodecfg.Config{}, nil, info, "", err
	}
	cfg, err := opencodecfg.ParseAgentConfig(agent.Metadata)
	if err != nil {
		return cfg, nil, info, "", unavailable(err)
	}
	if cfg.Auth == opencodecfg.AuthAPIKey {
		credential, credErr := d.credentials.ResolveForBotAgent(ctx, botID, agentID)
		if credErr != nil {
			return cfg, nil, info, "", external.CredentialError(credErr)
		}
		if credential.AuthKind != agentcredential.AuthKindOpenCodeAPIKey {
			return cfg, nil, info, "", external.CredentialError(agentcredential.ErrIncompatible)
		}
		cfg.ProviderID = credential.Secret["provider_id"]
		cfg.APIKey = credential.Secret["api_key"]
	}
	info, err = d.bridges.WorkspaceInfo(ctx, botID)
	if err != nil {
		return cfg, nil, info, "", unavailable(err)
	}
	if err = external.RequireContainerWorkspace(info, RuntimeType); err != nil {
		return cfg, nil, info, "", err
	}
	client, err := d.bridges.MCPClient(ctx, botID)
	if err != nil {
		return cfg, nil, info, "", unavailable(err)
	}
	launcher, err := d.resolveLauncher(ctx, botID)
	return cfg, client, info, launcher.Path, err
}

func (d *Driver) Prompt(ctx context.Context, input external.PromptInput) (external.PromptResult, error) {
	cfg, client, info, launcher, err := d.resolve(ctx, input.BotID, input.BotAgentID)
	if err != nil {
		return external.PromptResult{}, err
	}
	t := newTurn(input, d.approval, d.userInput, d.logger)
	defer t.close()
	mount, err := d.mountTools(ctx, client, info, input)
	if err != nil {
		return external.PromptResult{}, unavailable(err)
	}
	defer mount.Stop()
	unregister := toolmount.RegisterTurnSink(d.toolGateway.Contexts, input.BotID, input.ThreadID, input.RunID, t.emit)
	defer unregister()
	// Stop first aborts the native turn, then tears down the process. The
	// process must survive the caller cancellation long enough to do so.
	runtimeCtx, stop := context.WithCancel(context.WithoutCancel(ctx))
	defer stop()
	startupCtx, startupCancel := context.WithCancel(runtimeCtx)
	stopStartup := context.AfterFunc(ctx, startupCancel)
	srv, err := startServer(startupCtx, client, launcher, cfg, input, mount.URL)
	stopStartup()
	if err != nil {
		startupCancel()
		if ctx.Err() != nil {
			return t.result(false), nil
		}
		return t.result(false), unavailable(err)
	}
	defer startupCancel()
	defer srv.close()
	t.api = srv.api
	d.observeVersion(ctx, input.BotID, srv.version)
	err = t.run(ctx, cfg)
	unregister()
	t.close()
	result := t.result(err == nil && ctx.Err() == nil)
	if ctx.Err() != nil {
		return result, nil
	}
	if err != nil {
		return result, unavailable(err)
	}
	return result, nil
}

// Native sessions are reused only if the last user message matches the
// canonical committed anchor. An uncommitted/foreign native tail starts a
// fresh session from Memoh context instead of silently diverging.
func (t *turnRunner) ensureSession(ctx context.Context) (bool, error) {
	id := metadataString(t.input.RuntimeMetadata, metadataSessionIDKey)
	if id != "" && !t.input.ForceFreshRuntime {
		var messages []nativeMessage
		err := t.api.call(ctx, http.MethodGet, "/session/"+url.PathEscape(id)+"/message", nil, &messages)
		if err == nil {
			last := ""
			for _, msg := range messages {
				if msg.Info.Role == "user" {
					last = msg.Info.ID
				}
			}
			if last != "" && last == metadataString(t.input.RuntimeMetadata, metadataMessageIDKey) {
				t.sessionID = id
				return false, nil
			}
		} else {
			var apiErr *httpError
			if !errors.As(err, &apiErr) || apiErr.status != http.StatusNotFound {
				return false, err
			}
		}
	}
	var session struct {
		ID string `json:"id"`
	}
	if err := t.api.call(ctx, http.MethodPost, "/session", map[string]any{}, &session); err != nil {
		return false, err
	}
	if session.ID == "" {
		return false, errors.New("opencode returned an empty session ID")
	}
	t.sessionID = session.ID
	return true, nil
}

func promptBody(input external.PromptInput, cfg opencodecfg.Config, messageID string) (map[string]any, error) {
	text := input.Prompt
	if len(input.AttachmentReferences) > 0 {
		text += "\n\nAttached workspace files:\n" + strings.Join(input.AttachmentReferences, "\n")
	}
	parts := []map[string]any{{"type": "text", "text": text}}
	for _, image := range input.Images {
		parts = append(parts, map[string]any{"type": "file", "mime": image.MimeType, "url": "data:" + image.MimeType + ";base64," + base64.StdEncoding.EncodeToString(image.Data)})
	}
	body := map[string]any{"messageID": messageID, "parts": parts}
	if input.ContextMarkdown != "" {
		body["system"] = input.ContextMarkdown
	}
	if selected := firstNonEmpty(input.ModelID, cfg.Model); selected != "" {
		provider, model, ok := strings.Cut(selected, "/")
		if !ok || provider == "" || model == "" {
			return nil, errors.New("opencode model ID must include provider/model")
		}
		body["model"] = map[string]string{"providerID": provider, "modelID": model}
	}
	if input.ReasoningEffort != "" && input.ReasoningEffort != "default" {
		body["variant"] = input.ReasoningEffort
	}
	if agent := promptAgent(metadataString(input.RuntimeMetadata, "collaboration_mode"), cfg.Agent); agent != "" {
		body["agent"] = agent
	}
	if input.Command != "" {
		body["command"] = input.Command
		body["arguments"] = input.CommandArgs
		delete(body, "system") // Native commands read the turn's instructions file.
		body["parts"] = parts[1:]
		if selected := firstNonEmpty(input.ModelID, cfg.Model); selected != "" {
			body["model"] = selected
		}
		if len(input.AttachmentReferences) > 0 {
			body["arguments"] = input.CommandArgs + "\n\nAttached workspace files:\n" + strings.Join(input.AttachmentReferences, "\n")
		}
	}
	return body, nil
}

func (d *Driver) ModelCatalog(ctx context.Context, request external.ModelCatalogRequest) (external.ModelCatalog, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	cfg, client, _, launcher, err := d.resolve(ctx, request.BotID, request.BotAgentID)
	if err != nil {
		return external.ModelCatalog{}, err
	}
	input := external.PromptInput{BotAgentID: request.BotAgentID, ThreadID: uuid.Nil.String(), RuntimeMetadata: map[string]any{"project_path": request.ProjectPath}}
	srv, err := startServer(ctx, client, launcher, cfg, input, "")
	if err != nil {
		return external.ModelCatalog{}, unavailable(err)
	}
	defer srv.close()
	d.observeVersion(ctx, request.BotID, srv.version)
	catalog, err := readModelCatalog(ctx, srv.api, cfg, request)
	if err != nil {
		return external.ModelCatalog{}, unavailable(err)
	}
	sort.Slice(catalog.Models, func(i, j int) bool { return catalog.Models[i].ID < catalog.Models[j].ID })
	return catalog, nil
}

func nativeMessageID() string {
	return fmt.Sprintf("msg_%012x%s", time.Now().UnixMilli(), strings.ReplaceAll(uuid.NewString(), "-", "")[:14])
}
