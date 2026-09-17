// Package opencodecfg owns the OpenCode BotAgent configuration contract.
package opencodecfg

import (
	"errors"
	"net/url"
	"strings"
)

const (
	AuthAPIKey    = "api_key"
	AuthWorkspace = "workspace"
)

var ErrNotConfigured = errors.New("opencode runtime configuration is invalid")

type Config struct {
	Auth           string
	BaseURL        string
	Model          string
	Agent          string
	PermissionMode string
	ProviderID     string
	APIKey         string `json:"-"`
}

func ParseAgentConfig(metadata map[string]any) (Config, error) {
	for _, key := range []string{"auth", "base_url", "model", "agent", "permission_mode"} {
		if value, exists := metadata[key]; exists && value != nil {
			if _, ok := value.(string); !ok {
				return Config{}, ErrNotConfigured
			}
		}
	}
	get := func(key string) string { value, _ := metadata[key].(string); return strings.TrimSpace(value) }
	cfg := Config{Auth: get("auth"), BaseURL: get("base_url"), Model: get("model"), Agent: get("agent"), PermissionMode: get("permission_mode")}
	if cfg.Auth != AuthAPIKey && cfg.Auth != AuthWorkspace {
		return Config{}, ErrNotConfigured
	}
	switch cfg.PermissionMode {
	case "", "default", "allow", "inherit":
	default:
		return Config{}, ErrNotConfigured
	}
	if cfg.Model != "" {
		provider, model, ok := strings.Cut(cfg.Model, "/")
		if !ok || provider == "" || model == "" {
			return Config{}, ErrNotConfigured
		}
	}
	if cfg.BaseURL != "" {
		u, err := url.Parse(cfg.BaseURL)
		if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return Config{}, ErrNotConfigured
		}
	}
	return cfg, nil
}
