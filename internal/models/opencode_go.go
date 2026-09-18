package models

import (
	"context"
	"net/http"
	"strings"

	opencodego "github.com/felinics/twilight/provider/opencode/go"
	"github.com/felinics/twilight/sdk"
	"github.com/google/uuid"
)

type modelSessionKey struct{}

// WithModelSession scopes provider session metadata to the owning conversation.
// Only providers that require it forward this value to the remote endpoint.
func WithModelSession(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, modelSessionKey{}, strings.TrimSpace(sessionID))
}

var openCodeGoRoutes = opencodego.New()

// ResolveModelClientType uses Twilight's routing table for multi-protocol
// providers, so reasoning and prompt caching follow the actual wire protocol.
func ResolveModelClientType(clientType, modelID string) string {
	if clientType != string(ClientTypeOpenCodeGo) {
		return clientType
	}
	// Twilight routes models outside its exception table to Completions and only
	// fails on an invalid route override, which Memoh never configures.
	protocol, _ := openCodeGoRoutes.ProtocolForModel(modelID)
	return string(protocol)
}

// newOpenCodeGoModel sends requests through Twilight's Go provider rather than a
// protocol adapter of Memoh's own, because the provider also adapts requests to
// how Go's routes behave. Claude-specific thinking configuration therefore does
// not apply; Go models only take the request's reasoning effort.
func newOpenCodeGoModel(cfg SDKModelConfig) *sdk.Model {
	// Memoh reads the wire protocol from the provider name for reasoning, prompt
	// caching, and media handling.
	name := ResolveModelClientType(cfg.ClientType, cfg.ModelID)
	return &sdk.Model{
		ID:       cfg.ModelID,
		Provider: newOpenCodeGoProvider(cfg.BaseURL, cfg.APIKey, cfg.HTTPClient, name),
		Type:     sdk.ModelTypeChat,
	}
}

func newOpenCodeGoProvider(baseURL, apiKey string, httpClient *http.Client, name string) sdk.Provider {
	opts := []opencodego.Option{
		opencodego.WithAPIKey(apiKey),
		opencodego.WithHTTPClient(httpClient),
		// The agent's HTTP clients do not add Memoh's User-Agent.
		opencodego.WithHeaders(map[string]string{"User-Agent": DefaultProviderUserAgent()}),
	}
	if baseURL != "" {
		opts = append(opts, opencodego.WithBaseURL(baseURL))
	}
	// Standalone jobs (memory extraction and probes) have no conversation owner.
	// Give each constructed model/job its own stable ID across tool steps.
	return &openCodeGoSessionProvider{Provider: opencodego.New(opts...), name: name, jobID: uuid.NewString()}
}

type openCodeGoSessionProvider struct {
	sdk.Provider
	name  string
	jobID string
}

func (p *openCodeGoSessionProvider) Name() string { return p.name }

func (p *openCodeGoSessionProvider) sessionContext(ctx context.Context) context.Context {
	sessionID, _ := ctx.Value(modelSessionKey{}).(string)
	if sessionID == "" {
		sessionID = p.jobID
	}
	return sdk.WithRequestHeaders(ctx, map[string]string{opencodego.SessionHeader: sessionID})
}

func (p *openCodeGoSessionProvider) DoGenerate(ctx context.Context, req sdk.Request) (sdk.ModelResult, error) {
	return p.Provider.DoGenerate(p.sessionContext(ctx), req)
}

func (p *openCodeGoSessionProvider) DoStream(ctx context.Context, req sdk.Request) (<-chan sdk.StreamPart, error) {
	return p.Provider.DoStream(p.sessionContext(ctx), req)
}

func (p *openCodeGoSessionProvider) TestModel(ctx context.Context, modelID string) (*sdk.ModelTestResult, error) {
	return p.Provider.TestModel(p.sessionContext(ctx), modelID)
}
