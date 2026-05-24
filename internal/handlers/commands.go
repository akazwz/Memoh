package handlers

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/memohai/memoh/internal/accounts"
	"github.com/memohai/memoh/internal/bots"
	"github.com/memohai/memoh/internal/command"
)

type CommandManifestHandler struct {
	registry       *command.ManifestRegistry
	botService     *bots.Service
	accountService *accounts.Service
	logger         *slog.Logger
}

func NewCommandManifestHandler(log *slog.Logger, registry *command.ManifestRegistry, botService *bots.Service, accountService *accounts.Service) *CommandManifestHandler {
	if log == nil {
		log = slog.Default()
	}
	return &CommandManifestHandler{
		registry:       registry,
		botService:     botService,
		accountService: accountService,
		logger:         log.With(slog.String("handler", "commands")),
	}
}

func (h *CommandManifestHandler) Register(e *echo.Echo) {
	e.GET("/commands", h.ListCommands)
}

type CommandListResponse struct {
	Commands []command.CommandManifest `json:"commands"`
}

// ListCommands godoc
// @Summary List available slash commands
// @Description Returns UI-facing slash command manifests for the current bot/session/scope.
// @Tags commands
// @Param bot_id query string false "Bot ID"
// @Param session_id query string false "Session ID"
// @Param scope query string false "Client scope, e.g. web, desktop, im, local_chat"
// @Success 200 {object} CommandListResponse
// @Failure 400 {object} ErrorResponse
// @Failure 403 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /commands [get].
func (h *CommandManifestHandler) ListCommands(c echo.Context) error {
	channelIdentityID, err := RequireChannelIdentityID(c)
	if err != nil {
		return err
	}

	botID := strings.TrimSpace(c.QueryParam("bot_id"))
	sessionID := strings.TrimSpace(c.QueryParam("session_id"))
	scope := strings.TrimSpace(c.QueryParam("scope"))
	if scope == "" {
		scope = "web"
	}
	if botID == "" && sessionID != "" {
		return echo.NewHTTPError(http.StatusBadRequest, "bot id is required when session id is provided")
	}
	var botMetadata map[string]any
	if botID != "" {
		bot, err := AuthorizeBotAccess(c.Request().Context(), h.botService, h.accountService, channelIdentityID, botID)
		if err != nil {
			return err
		}
		botMetadata = bot.Metadata
	}
	if h.registry == nil {
		return c.JSON(http.StatusOK, CommandListResponse{Commands: []command.CommandManifest{}})
	}

	commands, err := h.registry.Commands(c.Request().Context(), command.ManifestRequest{
		BotID:       botID,
		SessionID:   sessionID,
		Scope:       scope,
		BotMetadata: botMetadata,
	})
	if err != nil {
		h.logger.Error("list command manifests failed", slog.Any("error", err))
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to list commands")
	}
	if commands == nil {
		commands = []command.CommandManifest{}
	}
	return c.JSON(http.StatusOK, CommandListResponse{Commands: commands})
}
