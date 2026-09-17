package opencodecfg

import "testing"

func TestConfigurationBoundary(t *testing.T) {
	for _, metadata := range []map[string]any{
		{"auth": "workspace"},
		{"auth": "api_key", "model": "openrouter/vendor/model", "permission_mode": "default", "base_url": "https://example.com/v1"},
	} {
		if _, err := ParseAgentConfig(metadata); err != nil {
			t.Fatalf("valid config: %v", err)
		}
	}
	for _, metadata := range []map[string]any{
		{"auth": "oauth_token"},
		{"auth": "api_key", "permission_mode": true},
		{"auth": "workspace", "permission_mode": "bypassPermissions"},
		{"auth": "workspace", "model": "unqualified"},
		{"auth": "api_key", "base_url": "file:///data/key"},
		{"auth": "api_key", "base_url": "https://user@example.com"},
	} {
		if _, err := ParseAgentConfig(metadata); err == nil {
			t.Fatalf("invalid config accepted: %v", metadata)
		}
	}
}
