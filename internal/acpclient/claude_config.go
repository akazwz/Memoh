package acpclient

import (
	"context"
	"errors"
	"fmt"
	"path"

	"github.com/memohai/memoh/internal/workspace/bridge"
)

// claudeManagedSettings is written to the managed config dir's settings.json
// for Claude Code sessions. The explicit "ask" rule outranks the CLI's
// built-in auto-allow for "safe" read-only commands (pwd, ls, ...), so every
// Bash invocation reaches the permission prompt and therefore Memoh's tool
// approval — Memoh policy stays the single authority over what runs unasked.
// (Verified live by TestACPLivePoolClaudeBashApproval.)
//
// There is deliberately NO fs "deny" rule. A prior version denied
// Read/Edit/Write("/**") on the belief that single-leading-slash patterns
// anchor to the settings dir — they don't: Claude Code anchors them to the
// PROJECT ROOT (an absolute path needs "//"). So the rule never matched the
// managed config it claimed to protect, AND it would have denied the agent its
// own workspace if it were honored at all — which it isn't: claude-agent-acp
// routes file ops through the ACP client fs capability, which does not consult
// settings deny rules (TestACPLivePoolClaudeWriteApproval proves a gated write
// still lands). The REAL fs boundary is Memoh's permission-scope validation
// (client.validatePermissionScope), which cancels any read/write/edit that
// resolves outside the workspace root.
var claudeManagedSettings = []byte(`{
  "permissions": {
    "ask": [
      "Bash"
    ]
  }
}
`)

// WriteClaudeManagedSettings writes the managed Claude Code settings into the
// given HOME directory via the workspace bridge (the CLI reads
// $HOME/.claude/settings.json).
func WriteClaudeManagedSettings(ctx context.Context, client *bridge.Client, homeDir string) error {
	return WriteClaudeManagedSettingsDir(ctx, client, path.Join(homeDir, ".claude"))
}

// WriteClaudeManagedSettingsDir writes the managed Claude Code settings into
// an explicit config directory (the CLAUDE_CONFIG_DIR layout, where
// settings.json sits directly inside the directory).
func WriteClaudeManagedSettingsDir(ctx context.Context, client *bridge.Client, configDir string) error {
	if client == nil {
		return errors.New("workspace bridge client is required")
	}
	if err := client.Mkdir(ctx, configDir); err != nil {
		return fmt.Errorf("create Claude settings dir: %w", err)
	}
	if err := client.WriteFile(ctx, path.Join(configDir, "settings.json"), claudeManagedSettings); err != nil {
		return fmt.Errorf("write Claude settings: %w", err)
	}
	return nil
}
