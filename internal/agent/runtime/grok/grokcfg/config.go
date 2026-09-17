// Package grokcfg owns Grok Build's Bot Agent configuration contract.
package grokcfg

import (
	"errors"
	"strings"
)

type AuthMode string

const (
	AuthAPIKey AuthMode = "api_key"
	AuthOAuth  AuthMode = "oauth"
)

var ErrNotConfigured = errors.New("grok runtime is not configured for this Agent")

type Config struct {
	Auth           AuthMode
	Model          string
	PermissionMode string
}

func ParseAgentConfig(metadata map[string]any) (Config, error) {
	str := func(key string) string { value, _ := metadata[key].(string); return strings.TrimSpace(value) }
	cfg := Config{Auth: AuthMode(str("auth")), Model: str("model"), PermissionMode: str("permission_mode")}
	if (cfg.Auth != AuthAPIKey && cfg.Auth != AuthOAuth) || !ValidPermissionMode(cfg.PermissionMode) {
		return Config{}, ErrNotConfigured
	}
	return cfg, nil
}

func ValidPermissionMode(mode string) bool {
	switch mode {
	case "", "ask", "auto", "always-approve":
		return true
	default:
		return false
	}
}
