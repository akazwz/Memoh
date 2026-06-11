package flow

import (
	"strings"
	"testing"
	"time"

	agentpkg "github.com/memohai/memoh/internal/agent"
	"github.com/memohai/memoh/internal/conversation"
)

func TestRenderACPContextMarkdownIncludesDynamicRuntimeAndMemory(t *testing.T) {
	t.Parallel()

	got := renderACPContextMarkdown(acpContextRenderInput{
		Now:                     time.Date(2026, 6, 1, 9, 30, 0, 0, time.FixedZone("PDT", -7*3600)),
		Timezone:                "America/Los_Angeles",
		BotID:                   "bot-1",
		SessionID:               "session-1",
		AgentID:                 "codex",
		ProjectPath:             "/data/app",
		DisplayName:             "Alice",
		CurrentChannel:          "telegram",
		ConversationType:        "group",
		ConversationName:        "Dev Group",
		SourceChannelIdentityID: "identity-1",
		Attachments: []conversation.ChatAttachment{{
			Name: "spec.md",
			Path: "/data/uploads/spec.md",
			Mime: "text/markdown",
		}},
		// The real source — agentpkg.FSClient.LoadSystemFiles — returns exactly
		// this curated set (AGENTS.md / MEMORY.md / PROFILES.md / daily
		// memory). ACP must inject the whole set verbatim, like the native
		// pipeline does; a hardcoded allowlist here is precisely how AGENTS.md
		// once went missing. NOTES.md stands in for a newly added workspace
		// file: it must still surface, with a title derived from its name.
		Files: []agentpkg.SystemFile{
			{Filename: "AGENTS.md", Content: "Always run the linter before committing."},
			{Filename: "MEMORY.md", Content: "User prefers small patches."},
			{Filename: "PROFILES.md", Content: "Alice is the project owner."},
			{Filename: "NOTES.md", Content: "Deploys happen on Fridays."},
			{Filename: "memory/2026-06-01.md", Content: "Today we discussed ACP context."},
		},
	})

	for _, want := range []string{
		"# Memoh ACP Context",
		"Current time: 2026-06-01T09:30:00-07:00",
		"Timezone: America/Los_Angeles",
		"Bot ID: bot-1",
		"ACP agent: codex",
		"Workspace: /data/app",
		"Sender: Alice",
		"Conversation name: Dev Group",
		"name=spec.md",
		"## Agent Instructions",
		"Embedded excerpt from `/data/AGENTS.md`",
		"Always run the linter before committing.",
		"## Long-Term Memory",
		"User prefers small patches.",
		"## Profiles",
		"Alice is the project owner.",
		// A file the allowlist never knew about still reaches the agent, under
		// a title derived from its name.
		"## Notes",
		"Deploys happen on Fridays.",
		"## Daily Memory - 2026-06-01.md",
		"Today we discussed ACP context.",
		"This virtual resource is already embedded",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("context missing %q:\n%s", want, got)
		}
	}
}
