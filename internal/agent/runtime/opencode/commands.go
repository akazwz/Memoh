package opencode

import (
	"context"
	"net/http"
	"time"

	"github.com/felinics/memoh/internal/agent/runtime/external"
)

func (d *Driver) controlServer(ctx context.Context, input external.PromptInput) (*server, error) {
	cfg, client, _, launcher, err := d.resolve(ctx, input.BotID, input.BotAgentID)
	if err != nil {
		return nil, err
	}
	srv, err := startServer(ctx, client, launcher, cfg, input, "")
	if err != nil {
		return nil, unavailable(err)
	}
	return srv, nil
}

func (d *Driver) Commands(ctx context.Context, input external.PromptInput) ([]external.Command, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	srv, err := d.controlServer(ctx, input)
	if err != nil {
		return nil, err
	}
	defer srv.close()
	commands, err := nativeCommands(ctx, srv.api)
	if err != nil {
		return nil, unavailable(err)
	}
	return commands, nil
}

func nativeCommands(ctx context.Context, api *apiClient) ([]external.Command, error) {
	commands := []external.Command{
		{Name: "compact", Kind: external.CommandOperation, I18nKey: "runtime.opencode.commands.compact"},
		{Name: "status", Kind: external.CommandRead, I18nKey: "runtime.opencode.commands.status"},
		{Name: "skills", Kind: external.CommandRead, I18nKey: "runtime.opencode.commands.skills"},
		{Name: "mcp", Kind: external.CommandRead, I18nKey: "runtime.opencode.commands.mcp"},
	}
	var native []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := api.call(ctx, http.MethodGet, "/command", nil, &native); err != nil {
		return nil, err
	}
	for _, command := range native {
		if _, reserved := external.FindCommand(commands, command.Name); reserved || command.Name == "plan" || command.Name == "" {
			continue
		}
		commands = append(commands, external.Command{Name: command.Name, Description: command.Description, Kind: external.CommandTurn})
	}
	return commands, nil
}

func (d *Driver) ReadCommand(ctx context.Context, input external.PromptInput) (external.CommandResult, error) {
	endpoint := ""
	switch input.Command {
	case "status":
		return external.CommandResult{Data: map[string]any{"session_id": metadataString(input.RuntimeMetadata, metadataSessionIDKey), "model": input.ModelID, "variant": input.ReasoningEffort}, Notice: "last_observed"}, nil
	case "skills":
		endpoint = "/skill"
	case "mcp":
		endpoint = "/mcp"
	default:
		return external.CommandResult{}, external.ErrCommandUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	srv, err := d.controlServer(ctx, input)
	if err != nil {
		return external.CommandResult{}, err
	}
	defer srv.close()
	var data any
	if err := srv.api.call(ctx, http.MethodGet, endpoint, nil, &data); err != nil {
		return external.CommandResult{}, unavailable(err)
	}
	return external.CommandResult{Data: data}, nil
}
