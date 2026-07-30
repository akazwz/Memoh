package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	connectsdk "github.com/memohai/connect-it/sdk/go"
	"github.com/modelcontextprotocol/go-sdk/auth"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	mcpgw "github.com/memohai/memoh/internal/mcp"
)

const connectorCallTimeout = 60 * time.Second

type cachedSessionAuth struct {
	fingerprint string
	handler     auth.OAuthHandler
}

// Source exposes all enabled Connect-It connections for a bot as one
// aggregate MCP tool source.
type Source struct {
	logger  *slog.Logger
	service *Service

	mu    sync.Mutex
	auths map[string]cachedSessionAuth
}

func NewSource(log *slog.Logger, service *Service) *Source {
	if log == nil {
		log = slog.Default()
	}
	return &Source{
		logger:  log.With(slog.String("tool_source", "connect_it")),
		service: service,
		auths:   map[string]cachedSessionAuth{},
	}
}

func (s *Source) ListTools(ctx context.Context, session mcpgw.ToolSessionContext) ([]mcpgw.ToolDescriptor, error) {
	callCtx, cancel := context.WithTimeout(ctx, connectorCallTimeout)
	defer cancel()
	clientSession, ok, err := s.connect(callCtx, strings.TrimSpace(session.BotID))
	if err != nil || !ok {
		return []mcpgw.ToolDescriptor{}, err
	}
	defer func() { _ = clientSession.Close() }()

	result, err := clientSession.ListTools(callCtx, &sdkmcp.ListToolsParams{})
	if err != nil {
		return nil, err
	}
	tools := make([]mcpgw.ToolDescriptor, 0, len(result.Tools))
	for _, tool := range result.Tools {
		if tool == nil || strings.TrimSpace(tool.Name) == "" {
			continue
		}
		inputSchema, err := schemaObject(tool.InputSchema)
		if err != nil {
			s.logger.Warn(
				"skip connect-it tool with invalid input schema",
				slog.String("tool", tool.Name),
				slog.Any("error", err),
			)
			continue
		}
		tools = append(tools, mcpgw.ToolDescriptor{
			Name:        strings.TrimSpace(tool.Name),
			Description: strings.TrimSpace(tool.Description),
			InputSchema: inputSchema,
		})
	}
	return tools, nil
}

func (s *Source) CallTool(
	ctx context.Context,
	session mcpgw.ToolSessionContext,
	toolName string,
	arguments map[string]any,
) (map[string]any, error) {
	toolName = strings.TrimSpace(toolName)
	if toolName == "" {
		return nil, mcpgw.ErrToolNotFound
	}
	callCtx, cancel := context.WithTimeout(ctx, connectorCallTimeout)
	defer cancel()
	if err := mcpgw.ValidateRuntimeGuard(callCtx, session); err != nil {
		return nil, err
	}
	clientSession, ok, err := s.connect(callCtx, strings.TrimSpace(session.BotID))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, mcpgw.ErrToolNotFound
	}
	defer func() { _ = clientSession.Close() }()
	if arguments == nil {
		arguments = map[string]any{}
	}

	result, err := clientSession.CallTool(callCtx, &sdkmcp.CallToolParams{
		Name:      toolName,
		Arguments: arguments,
	})
	if err != nil {
		return nil, err
	}
	return callResultObject(result)
}

func (s *Source) connect(ctx context.Context, botID string) (*sdkmcp.ClientSession, bool, error) {
	if botID == "" || !s.service.Configured() {
		return nil, false, nil
	}
	connections, err := s.service.SessionConnections(ctx, botID)
	if err != nil {
		return nil, false, err
	}
	if len(connections) == 0 {
		return nil, false, nil
	}
	handler := s.authHandler(botID, connections)
	mcpClient := sdkmcp.NewClient(&sdkmcp.Implementation{
		Name:    "memoh-connectors",
		Version: "1.0.0",
	}, nil)
	clientSession, err := mcpClient.Connect(ctx, &sdkmcp.StreamableClientTransport{
		Endpoint:             s.service.client.MCPEndpoint(),
		OAuthHandler:         handler,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		return nil, false, err
	}
	return clientSession, true, nil
}

func (s *Source) authHandler(
	botID string,
	connections map[string]string,
) auth.OAuthHandler {
	fingerprint := connectionFingerprint(connections)
	s.mu.Lock()
	defer s.mu.Unlock()
	if cached, ok := s.auths[botID]; ok && cached.fingerprint == fingerprint {
		return cached.handler
	}
	handler := s.service.client.MCPAuthHandler(connectsdk.MCPSessionConfig{
		Connections: connections,
		TTL:         time.Hour,
	})
	s.auths[botID] = cachedSessionAuth{
		fingerprint: fingerprint,
		handler:     handler,
	}
	return handler
}

func connectionFingerprint(connections map[string]string) string {
	keys := make([]string, 0, len(connections))
	for alias := range connections {
		keys = append(keys, alias)
	}
	sort.Strings(keys)
	var fingerprint strings.Builder
	for _, alias := range keys {
		fingerprint.WriteString(alias)
		fingerprint.WriteByte(0)
		fingerprint.WriteString(connections[alias])
		fingerprint.WriteByte(0)
	}
	return fingerprint.String()
}

func schemaObject(schema any) (map[string]any, error) {
	if schema == nil {
		return map[string]any{"type": "object"}, nil
	}
	if object, ok := schema.(map[string]any); ok {
		return object, nil
	}
	payload, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	var object map[string]any
	if err := json.Unmarshal(payload, &object); err != nil {
		return nil, err
	}
	if object == nil {
		return nil, errors.New("input schema is not an object")
	}
	return object, nil
}

func callResultObject(result *sdkmcp.CallToolResult) (map[string]any, error) {
	if result == nil {
		return nil, nil
	}
	payload, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	var object map[string]any
	if err := json.Unmarshal(payload, &object); err != nil {
		return nil, err
	}
	return object, nil
}
