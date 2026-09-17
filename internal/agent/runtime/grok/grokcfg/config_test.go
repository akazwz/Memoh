package grokcfg

import "testing"

func TestPermissionModes(t *testing.T) {
	for _, mode := range []string{"", "ask", "auto", "always-approve"} {
		if _, err := ParseAgentConfig(map[string]any{"auth": "oauth", "permission_mode": mode}); err != nil {
			t.Errorf("valid mode %q: %v", mode, err)
		}
	}
	for _, mode := range []string{"plan", "yolo", "unrecognized"} {
		if ValidPermissionMode(mode) {
			t.Errorf("accepted non-permission mode %q", mode)
		}
	}
}
