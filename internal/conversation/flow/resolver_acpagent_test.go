package flow

import (
	"testing"

	acpagent "github.com/memohai/memoh/internal/acpagent"
)

func TestIsCodexStopRequest(t *testing.T) {
	for _, text := range []string{"/codex stop", "codex stop", "停止 codex", "停止codex", "退出 Codex"} {
		if !isCodexStopRequest(text) {
			t.Fatalf("isCodexStopRequest(%q) = false, want true", text)
		}
	}
	for _, text := range []string{"用 codex 修复测试", "继续", "stop"} {
		if isCodexStopRequest(text) {
			t.Fatalf("isCodexStopRequest(%q) = true, want false", text)
		}
	}
}

func TestCodexACPModelIDIsEmpty(t *testing.T) {
	if acpAgentModelID != "" {
		t.Fatalf("acpAgentModelID = %q, want empty so persistence skips model UUID parsing", acpAgentModelID)
	}
}

func TestParseCodexSlashCommand(t *testing.T) {
	cmd, ok, err := parseCodexSlashCommand(`/codex start --project /data/simple-web "新建 Go 服务"`)
	if !ok || err != nil {
		t.Fatalf("parseCodexSlashCommand() ok=%v err=%v", ok, err)
	}
	if cmd.Action != "start" || cmd.ProjectPath != "/data/simple-web" || cmd.Task != "新建 Go 服务" {
		t.Fatalf("unexpected command: %#v", cmd)
	}

	cmd, ok, err = parseCodexSlashCommand("/codex stop")
	if !ok || err != nil || cmd.Action != "stop" {
		t.Fatalf("unexpected stop command: cmd=%#v ok=%v err=%v", cmd, ok, err)
	}

	if _, ok, err = parseCodexSlashCommand("/codex deploy"); !ok || err == nil {
		t.Fatalf("expected unknown codex action error, ok=%v err=%v", ok, err)
	}

	if _, ok, err = parseCodexSlashCommand("/help"); ok || err != nil {
		t.Fatalf("non-codex command should not route, ok=%v err=%v", ok, err)
	}
}

func TestParseACPAgentSlashCommandUsesProfiles(t *testing.T) {
	svc := acpagent.NewService(nil, nil, acpagent.Profile{
		ID:          "gemini",
		DisplayName: "Gemini",
		Command:     "gemini",
		Args:        []string{"--acp"},
	})
	r := &Resolver{acpAgentService: svc}
	cmd, ok, err := r.parseACPAgentSlashCommand(`/gemini start -p /data/app "修复测试"`)
	if !ok || err != nil {
		t.Fatalf("parseACPAgentSlashCommand() ok=%v err=%v", ok, err)
	}
	if cmd.AgentID != "gemini" || cmd.AgentName != "Gemini" || cmd.ProjectPath != "/data/app" || cmd.Task != "修复测试" {
		t.Fatalf("unexpected command: %#v", cmd)
	}
}
