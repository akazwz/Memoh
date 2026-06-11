package flow

import (
	"strings"
	"testing"
	"time"

	agentpkg "github.com/memohai/memoh/internal/agent"
)

// TestACPContextInjectsEverySystemFile is the drift guard that AGENTS.md
// needed and never had: every file the native pipeline loads
// (agent.SystemFileNames — the single source of truth) must reach the ACP
// agent too. A name added to SystemFileNames that the ACP context builder
// fails to surface fails here, instead of silently leaving the agent without
// its instructions.
func TestACPContextInjectsEverySystemFile(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 1, 9, 30, 0, 0, time.UTC)
	names := agentpkg.SystemFileNames(now)
	if len(names) == 0 {
		t.Fatal("SystemFileNames returned nothing")
	}

	files := make([]agentpkg.SystemFile, len(names))
	for i, name := range names {
		files[i] = agentpkg.SystemFile{Filename: name, Content: "content-of-" + name}
	}

	sections := acpContextSystemFiles(files)
	if len(sections) != len(names) {
		t.Fatalf("acpContextSystemFiles produced %d sections for %d system files; every file must surface:\nnames=%v\nsections=%+v",
			len(sections), len(names), names, sections)
	}
	for _, name := range names {
		marker := "content-of-" + name
		found := false
		for _, section := range sections {
			if strings.Contains(section.Content, marker) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("system file %q was dropped from the ACP context (no section carried %q)", name, marker)
		}
	}
}

// TestACPContextSystemFileTitleFallback documents that an unrecognized file
// name still gets a readable heading rather than a raw filename.
func TestACPContextSystemFileTitleFallback(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"AGENTS.md":         "Agent Instructions",
		"CONTRIBUTING.md":   "Contributing",
		"design_notes.md":   "Design Notes",
		"API.md":            "API",
		"memory/2026-06.md": "", // handled by the daily-memory branch, not the fallback
	}
	for name, want := range cases {
		if want == "" {
			continue
		}
		files := []agentpkg.SystemFile{{Filename: name, Content: "body"}}
		sections := acpContextSystemFiles(files)
		if len(sections) != 1 {
			t.Fatalf("%s produced %d sections, want 1", name, len(sections))
		}
		if sections[0].Title != want {
			t.Fatalf("%s title = %q, want %q", name, sections[0].Title, want)
		}
	}
}
