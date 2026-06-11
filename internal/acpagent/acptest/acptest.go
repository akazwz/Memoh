// Package acptest provides reusable scaffolding for ACP integration tests: an
// in-process workspace bridge, bot/session fixtures, and a SCRIPTED fake ACP
// agent whose behavior is driven by directives embedded in the prompt text.
//
// The same scripted prompts can be replayed against a real codex-acp adapter
// (the live tests), so one scenario covers both the deterministic L1 harness
// (fake agent) and the L3 smoke (real agent) — only the agent backend differs.
//
// Directive grammar (one per line in the prompt):
//
//	SAY <text>          agent streams an assistant message chunk
//	THINK <text>        agent streams a reasoning chunk
//	EXEC <command>      agent runs a terminal command via the client callback
//	                    (exercises the terminal approval + execution path)
//	WRITE <path> <text> agent writes a file via the client callback
//	                    (exercises the write approval path)
//	HANG                agent blocks until the prompt is cancelled
//	END <text>          agent streams <text> and ends the turn (default if no
//	                    END is given: the turn ends after the last directive)
package acptest

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	acp "github.com/coder/acp-go-sdk"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/memohai/memoh/internal/bots"
	"github.com/memohai/memoh/internal/config"
	"github.com/memohai/memoh/internal/workspace/bridge"
	pb "github.com/memohai/memoh/internal/workspace/bridgepb"
	"github.com/memohai/memoh/internal/workspace/bridgesvc"
)

// scriptedAgentEnv gates RunScriptedAgentIfInvoked: the helper test only
// becomes an agent when the spawned script sets it.
const scriptedAgentEnv = "MEMOH_ACP_SCRIPTED_AGENT"

// fakeSessionID is the session id the scripted agent reports from NewSession.
const fakeSessionID = "acptest-scripted-session"

// BridgeClient starts an in-process workspace bridge server over bufconn and
// returns a connected client. The bridge serves real filesystem + exec
// operations rooted at root (a t.TempDir()), so terminal/write callbacks
// actually run.
func BridgeClient(t *testing.T, root string) *bridge.Client {
	t.Helper()
	listener := bufconn.Listen(16 * 1024 * 1024)
	server := grpc.NewServer(
		grpc.MaxRecvMsgSize(16*1024*1024),
		grpc.MaxSendMsgSize(16*1024*1024),
	)
	pb.RegisterContainerServiceServer(server, bridgesvc.New(bridgesvc.Options{
		DefaultWorkDir:    root,
		WorkspaceRoot:     root,
		DataMount:         config.DefaultDataMount,
		AllowHostAbsolute: true,
	}))
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})

	conn, err := grpc.NewClient("passthrough:///acptest-bridge",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return listener.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(16*1024*1024),
			grpc.MaxCallSendMsgSize(16*1024*1024),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return bridge.NewClientFromConn(conn)
}

// Workspace adapts a bridge client + info into the acpclient runner's
// workspace provider interface.
type Workspace struct {
	Client *bridge.Client
	Info   bridge.WorkspaceInfo
}

func (w Workspace) MCPClient(context.Context, string) (*bridge.Client, error) {
	return w.Client, nil
}

func (w Workspace) WorkspaceInfo(context.Context, string) (bridge.WorkspaceInfo, error) {
	return w.Info, nil
}

// BotGetter is a fixed-bot botGetter for the pool.
type BotGetter struct{ Bot bots.Bot }

func (g BotGetter) Get(context.Context, string) (bots.Bot, error) { return g.Bot, nil }

// EnabledACPBot builds a bot with the given ACP agent enabled in the given
// setup mode (self/api_key/oauth).
func EnabledACPBot(id, agentID, mode string, managed map[string]any) bots.Bot {
	if managed == nil {
		managed = map[string]any{}
	}
	return bots.Bot{
		ID: id,
		Metadata: map[string]any{
			"acp": map[string]any{
				"agents": map[string]any{
					agentID: map[string]any{
						"enabled":    true,
						"setup_mode": mode,
						"managed":    managed,
					},
				},
			},
		},
	}
}

// WriteScriptedAgentScript writes an executable shell stub at dir/name that
// re-execs the test binary into the helper test identified by testRunName,
// with the scripted-agent env set. Point the pool's LocalCommand at name
// (with dir on PATH) to use the scripted agent as the ACP process.
func WriteScriptedAgentScript(t *testing.T, dir, name, testRunName string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	script := fmt.Sprintf("#!/bin/sh\n%s=1 exec %s -test.run '^%s$' --\n",
		scriptedAgentEnv, shellArg(os.Args[0]), testRunName)
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // test stub must be executable.
		t.Fatal(err)
	}
	return path
}

// RunScriptedAgentIfInvoked runs the scripted agent and exits when the process
// was spawned as the agent (env set); otherwise it returns immediately so the
// wrapping helper test is a no-op in normal runs. Call it as the entire body
// of a helper test (TestXxxScriptedAgent).
func RunScriptedAgentIfInvoked() {
	if os.Getenv(scriptedAgentEnv) != "1" {
		return
	}
	agent := &scriptedAgent{}
	conn := acp.NewAgentSideConnection(agent, os.Stdout, os.Stdin)
	agent.conn = conn
	<-conn.Done()
	os.Exit(0)
}

type scriptedAgent struct {
	conn *acp.AgentSideConnection
}

func (*scriptedAgent) Authenticate(context.Context, acp.AuthenticateRequest) (acp.AuthenticateResponse, error) {
	return acp.AuthenticateResponse{}, nil
}

func (*scriptedAgent) Initialize(context.Context, acp.InitializeRequest) (acp.InitializeResponse, error) {
	return acp.InitializeResponse{
		ProtocolVersion:   acp.ProtocolVersionNumber,
		AgentCapabilities: acp.AgentCapabilities{LoadSession: false},
	}, nil
}

func (*scriptedAgent) Cancel(context.Context, acp.CancelNotification) error { return nil }

func (*scriptedAgent) CloseSession(context.Context, acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	return acp.CloseSessionResponse{}, nil
}

func (*scriptedAgent) ListSessions(context.Context, acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	return acp.ListSessionsResponse{}, nil
}

func (*scriptedAgent) NewSession(context.Context, acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	return acp.NewSessionResponse{SessionId: acp.SessionId(fakeSessionID)}, nil
}

func (*scriptedAgent) UnstableSetSessionModel(context.Context, acp.UnstableSetSessionModelRequest) (acp.UnstableSetSessionModelResponse, error) {
	return acp.UnstableSetSessionModelResponse{}, nil
}

func (*scriptedAgent) ResumeSession(context.Context, acp.ResumeSessionRequest) (acp.ResumeSessionResponse, error) {
	return acp.ResumeSessionResponse{}, nil
}

func (*scriptedAgent) SetSessionConfigOption(context.Context, acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	return acp.SetSessionConfigOptionResponse{}, nil
}

func (*scriptedAgent) SetSessionMode(context.Context, acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	return acp.SetSessionModeResponse{}, nil
}

func (a *scriptedAgent) Prompt(ctx context.Context, p acp.PromptRequest) (acp.PromptResponse, error) {
	stop := acp.StopReasonEndTurn
	var once sync.Once
	cancelled := make(chan struct{})

	for _, line := range strings.Split(promptText(p), "\n") {
		if ctx.Err() != nil {
			return acp.PromptResponse{StopReason: acp.StopReasonCancelled}, nil
		}
		verb, rest := splitDirective(line)
		switch verb {
		case "SAY", "END":
			_ = a.conn.SessionUpdate(ctx, acp.SessionNotification{
				SessionId: p.SessionId,
				Update:    acp.UpdateAgentMessageText(rest),
			})
		case "THINK":
			_ = a.conn.SessionUpdate(ctx, acp.SessionNotification{
				SessionId: p.SessionId,
				Update:    acp.UpdateAgentThoughtText(rest),
			})
		case "EXEC":
			a.runExec(ctx, p.SessionId, rest)
		case "WRITE":
			a.runWrite(ctx, p.SessionId, rest)
		case "PERMISSION":
			a.runPermission(ctx, p.SessionId, rest)
		case "HANG":
			once.Do(func() { close(cancelled) })
			select {
			case <-ctx.Done():
				return acp.PromptResponse{StopReason: acp.StopReasonCancelled}, nil
			case <-cancelled:
				<-ctx.Done()
				return acp.PromptResponse{StopReason: acp.StopReasonCancelled}, nil
			}
		}
	}
	if ctx.Err() != nil {
		stop = acp.StopReasonCancelled
	}
	return acp.PromptResponse{StopReason: stop}, nil
}

// runExec drives the terminal client callback (approval + real execution).
// On approval rejection the agent streams a marker and continues — the turn
// must survive a denied tool.
func (a *scriptedAgent) runExec(ctx context.Context, sessionID acp.SessionId, command string) {
	resp, err := a.conn.CreateTerminal(ctx, acp.CreateTerminalRequest{
		SessionId: sessionID,
		Command:   "sh",
		Args:      []string{"-c", command},
	})
	if err != nil {
		_ = a.conn.SessionUpdate(ctx, acp.SessionNotification{
			SessionId: sessionID,
			Update:    acp.UpdateAgentMessageText("[exec-rejected]"),
		})
		return
	}
	_, _ = a.conn.WaitForTerminalExit(ctx, acp.WaitForTerminalExitRequest{
		SessionId:  sessionID,
		TerminalId: resp.TerminalId,
	})
	_, _ = a.conn.ReleaseTerminal(ctx, acp.ReleaseTerminalRequest{
		SessionId:  sessionID,
		TerminalId: resp.TerminalId,
	})
	_ = a.conn.SessionUpdate(ctx, acp.SessionNotification{
		SessionId: sessionID,
		Update:    acp.UpdateAgentMessageText("[exec-done]"),
	})
}

// runWrite drives the write client callback (approval + real write).
func (a *scriptedAgent) runWrite(ctx context.Context, sessionID acp.SessionId, rest string) {
	path, content, _ := strings.Cut(rest, " ")
	_, err := a.conn.WriteTextFile(ctx, acp.WriteTextFileRequest{
		SessionId: sessionID,
		Path:      path,
		Content:   content + "\n",
	})
	marker := "[write-done]"
	if err != nil {
		marker = "[write-rejected]"
	}
	_ = a.conn.SessionUpdate(ctx, acp.SessionNotification{
		SessionId: sessionID,
		Update:    acp.UpdateAgentMessageText(marker),
	})
}

// runPermission asks the client for permission for an action that does NOT
// map onto a fs/terminal tool — the generic-permission path, which forces a
// user decision (ForceApproval) regardless of policy. The agent streams a
// marker reflecting the outcome so tests can assert both branches and that a
// denied permission does not kill the turn.
func (a *scriptedAgent) runPermission(ctx context.Context, sessionID acp.SessionId, desc string) {
	title := "Scripted permission: " + desc
	resp, err := a.conn.RequestPermission(ctx, acp.RequestPermissionRequest{
		SessionId: sessionID,
		ToolCall: acp.ToolCallUpdate{
			ToolCallId: "scripted-perm",
			Title:      &title,
			RawInput:   map[string]any{"description": desc},
		},
		Options: []acp.PermissionOption{
			{Kind: acp.PermissionOptionKindAllowOnce, Name: "Allow", OptionId: "allow"},
			{Kind: acp.PermissionOptionKindRejectOnce, Name: "Reject", OptionId: "reject"},
		},
	})
	marker := "[permission-denied]"
	if err == nil && resp.Outcome.Selected != nil && resp.Outcome.Selected.OptionId == "allow" {
		marker = "[permission-allowed]"
	}
	_ = a.conn.SessionUpdate(ctx, acp.SessionNotification{
		SessionId: sessionID,
		Update:    acp.UpdateAgentMessageText(marker),
	})
}

func promptText(p acp.PromptRequest) string {
	var sb strings.Builder
	for _, block := range p.Prompt {
		if block.Text != nil {
			sb.WriteString(block.Text.Text)
		}
	}
	return sb.String()
}

// splitDirective returns the leading verb and the remainder of a directive
// line, ignoring lines that are not recognized directives (e.g. the embedded
// context resource or history replay text the pool prepends).
func splitDirective(line string) (string, string) {
	line = strings.TrimSpace(line)
	verb, rest, _ := strings.Cut(line, " ")
	switch verb {
	case "SAY", "THINK", "EXEC", "WRITE", "HANG", "END", "PERMISSION":
		return verb, strings.TrimSpace(rest)
	default:
		return "", ""
	}
}

func shellArg(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// ReadScript reads a script file (test helper for asserting written files).
func ReadScript(t *testing.T, parts ...string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(parts...)) //nolint:gosec // reads under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}
