package acpclient

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/memohai/memoh/internal/config"
	"github.com/memohai/memoh/internal/workspace/bridge"
	pb "github.com/memohai/memoh/internal/workspace/bridgepb"
	"github.com/memohai/memoh/internal/workspace/bridgesvc"
)

type testWorkspace struct {
	client *bridge.Client
	info   bridge.WorkspaceInfo
}

func (w testWorkspace) MCPClient(context.Context, string) (*bridge.Client, error) {
	return w.client, nil
}

func (w testWorkspace) WorkspaceInfo(context.Context, string) (bridge.WorkspaceInfo, error) {
	return w.info, nil
}

func TestRunnerRunLocalWorkspaceFakeAgent(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.MkdirAll(project, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "input.txt"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	client := newTestBridgeClient(t, root)
	agentPath := writeFakeAgentScript(t, root)
	runner := NewRunner(nil, testWorkspace{
		client: client,
		info: bridge.WorkspaceInfo{
			Backend:        bridge.WorkspaceBackendLocal,
			DefaultWorkDir: root,
		},
	})

	result, err := runner.Run(context.Background(), RunRequest{
		BotID:       "bot-1",
		Task:        "touch the project",
		ProjectPath: "/data/project",
		Command:     agentPath,
		Timeout:     10 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !strings.Contains(result.Text, "read: hello") {
		t.Fatalf("result text missing read content: %q", result.Text)
	}
	if !strings.Contains(result.Text, "term: terminal-ok") {
		t.Fatalf("result text missing terminal output: %q", result.Text)
	}
	if result.StopReason != string(acp.StopReasonEndTurn) {
		t.Fatalf("StopReason = %q, want %q", result.StopReason, acp.StopReasonEndTurn)
	}
	if got, err := os.ReadFile(filepath.Join(project, "output.txt")); err != nil { //nolint:gosec // test path is under t.TempDir.
		t.Fatalf("read output file: %v", err)
	} else if string(got) != "written by fake agent\n" {
		t.Fatalf("output file = %q", got)
	}
}

func TestRunnerRequiresACPCommand(t *testing.T) {
	root := t.TempDir()
	client := newTestBridgeClient(t, root)
	runner := NewRunner(nil, testWorkspace{
		client: client,
		info: bridge.WorkspaceInfo{
			Backend:        bridge.WorkspaceBackendLocal,
			DefaultWorkDir: root,
		},
	})

	_, err := runner.Run(context.Background(), RunRequest{
		BotID:   "bot-1",
		Task:    "fix tests",
		Timeout: 2 * time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "ACP command is required") {
		t.Fatalf("Run() error = %v, want missing command error", err)
	}
}

func TestRunnerStartSessionStreamsEvents(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.MkdirAll(project, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "input.txt"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	client := newTestBridgeClient(t, root)
	agentPath := writeFakeAgentScript(t, root)
	runner := NewRunner(nil, testWorkspace{
		client: client,
		info: bridge.WorkspaceInfo{
			Backend:        bridge.WorkspaceBackendLocal,
			DefaultWorkDir: root,
		},
	})

	var streamed strings.Builder
	sess, err := runner.StartSession(context.Background(), StartRequest{
		BotID:       "bot-1",
		ProjectPath: "/data/project",
		Command:     agentPath,
		Timeout:     10 * time.Second,
	}, EventSinkFunc(func(event StreamEvent) {
		if event.Type == StreamEventTextDelta {
			streamed.WriteString(event.Delta)
		}
	}))
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}
	defer func() { _ = sess.Close() }()

	result, err := sess.Prompt(context.Background(), "touch the project")
	if err != nil {
		t.Fatalf("Prompt() error = %v", err)
	}
	if result.StopReason != string(acp.StopReasonEndTurn) {
		t.Fatalf("StopReason = %q, want %q", result.StopReason, acp.StopReasonEndTurn)
	}
	if !strings.Contains(streamed.String(), "read: hello") {
		t.Fatalf("streamed text = %q", streamed.String())
	}
}

func TestRunnerRejectsNonLocalWorkspace(t *testing.T) {
	runner := NewRunner(nil, testWorkspace{
		info: bridge.WorkspaceInfo{
			Backend:        bridge.WorkspaceBackendContainer,
			DefaultWorkDir: "/data",
		},
	})
	_, err := runner.Run(context.Background(), RunRequest{BotID: "bot-1", Task: "fix tests"})
	if err == nil || !strings.Contains(err.Error(), ErrUnsupportedWorkspace.Error()) {
		t.Fatalf("Run() error = %v, want unsupported local workspace error", err)
	}
}

func TestRunnerMissingCommandIncludesStderr(t *testing.T) {
	root := t.TempDir()
	client := newTestBridgeClient(t, root)
	runner := NewRunner(nil, testWorkspace{
		client: client,
		info: bridge.WorkspaceInfo{
			Backend:        bridge.WorkspaceBackendLocal,
			DefaultWorkDir: root,
		},
	})
	_, err := runner.Run(context.Background(), RunRequest{
		BotID:   "bot-1",
		Task:    "fix tests",
		Command: "memoh-definitely-missing-acp-command",
		Timeout: 2 * time.Second,
	})
	if err == nil {
		t.Fatal("expected missing command error")
	}
	if !strings.Contains(err.Error(), "memoh-definitely-missing-acp-command") {
		t.Fatalf("missing command error did not include stderr command detail: %v", err)
	}
	if !strings.Contains(err.Error(), "not available") {
		t.Fatalf("missing command error is not actionable: %v", err)
	}
}

func TestResolvePathUnderRootRejectsEscapeAndSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "app"), 0o750); err != nil {
		t.Fatal(err)
	}
	rootEval, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ResolvePathUnderRoot(root, "/data/app"); err != nil {
		t.Fatalf("ResolvePathUnderRoot(/data/app) error = %v", err)
	} else if got != filepath.Join(rootEval, "app") {
		t.Fatalf("ResolvePathUnderRoot(/data/app) = %q, want %q", got, filepath.Join(rootEval, "app"))
	}
	if _, err := ResolvePathUnderRoot(root, "../escape"); err == nil {
		t.Fatal("expected relative parent escape to be rejected")
	}

	link := filepath.Join(root, "outside-link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := ResolvePathUnderRoot(root, filepath.Join(link, "file.txt")); err == nil {
		t.Fatal("expected symlink escape to be rejected")
	}
}

func newTestBridgeClient(t *testing.T, root string) *bridge.Client {
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
	go func() {
		_ = server.Serve(listener)
	}()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})

	conn, err := grpc.NewClient("passthrough:///acpclient-test",
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

func writeFakeAgentScript(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "fake-acp-agent.sh")
	script := fmt.Sprintf("#!/bin/sh\nMEMOH_ACP_FAKE_AGENT=1 exec %s -test.run '^TestFakeACPAgentHelper$' --\n", escapeShellArg(os.Args[0]))
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // test helper must be executable.
		t.Fatal(err)
	}
	return path
}

func TestFakeACPAgentHelper(_ *testing.T) {
	if os.Getenv("MEMOH_ACP_FAKE_AGENT") != "1" {
		return
	}
	agent := &fakeACPAgent{}
	conn := acp.NewAgentSideConnection(agent, os.Stdout, os.Stdin)
	agent.conn = conn
	<-conn.Done()
	os.Exit(0)
}

type fakeACPAgent struct {
	conn *acp.AgentSideConnection
	cwd  string
}

func (*fakeACPAgent) Authenticate(context.Context, acp.AuthenticateRequest) (acp.AuthenticateResponse, error) {
	return acp.AuthenticateResponse{}, nil
}

func (*fakeACPAgent) Initialize(context.Context, acp.InitializeRequest) (acp.InitializeResponse, error) {
	return acp.InitializeResponse{
		ProtocolVersion:   acp.ProtocolVersionNumber,
		AgentCapabilities: acp.AgentCapabilities{LoadSession: false},
	}, nil
}

func (*fakeACPAgent) Cancel(context.Context, acp.CancelNotification) error { return nil }

func (*fakeACPAgent) CloseSession(context.Context, acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	return acp.CloseSessionResponse{}, nil
}

func (*fakeACPAgent) ListSessions(context.Context, acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	return acp.ListSessionsResponse{}, nil
}

func (a *fakeACPAgent) NewSession(_ context.Context, p acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	a.cwd = p.Cwd
	return acp.NewSessionResponse{SessionId: acp.SessionId("fake-session")}, nil
}

func (a *fakeACPAgent) Prompt(ctx context.Context, p acp.PromptRequest) (acp.PromptResponse, error) {
	outputPath := filepath.Join(a.cwd, "output.txt")
	permission, err := a.conn.RequestPermission(ctx, acp.RequestPermissionRequest{
		SessionId: p.SessionId,
		ToolCall: acp.ToolCallUpdate{
			ToolCallId: acp.ToolCallId("write-output"),
			Title:      acp.Ptr("Write output file"),
			Kind:       acp.Ptr(acp.ToolKindEdit),
			Status:     acp.Ptr(acp.ToolCallStatusPending),
			Locations:  []acp.ToolCallLocation{{Path: outputPath}},
			RawInput:   map[string]any{"path": outputPath, "cwd": a.cwd},
		},
		Options: []acp.PermissionOption{
			{Kind: acp.PermissionOptionKindAllowOnce, Name: "Allow", OptionId: acp.PermissionOptionId("allow")},
			{Kind: acp.PermissionOptionKindRejectOnce, Name: "Reject", OptionId: acp.PermissionOptionId("reject")},
		},
	})
	if err != nil {
		return acp.PromptResponse{}, err
	}
	if permission.Outcome.Selected == nil {
		return acp.PromptResponse{StopReason: acp.StopReasonCancelled}, nil
	}

	read, err := a.conn.ReadTextFile(ctx, acp.ReadTextFileRequest{
		SessionId: p.SessionId,
		Path:      filepath.Join(a.cwd, "input.txt"),
	})
	if err != nil {
		return acp.PromptResponse{}, err
	}
	if _, err := a.conn.WriteTextFile(ctx, acp.WriteTextFileRequest{
		SessionId: p.SessionId,
		Path:      outputPath,
		Content:   "written by fake agent\n",
	}); err != nil {
		return acp.PromptResponse{}, err
	}

	term, err := a.conn.CreateTerminal(ctx, acp.CreateTerminalRequest{
		SessionId: p.SessionId,
		Command:   "printf",
		Args:      []string{"terminal-ok"},
		Cwd:       &a.cwd,
	})
	if err != nil {
		return acp.PromptResponse{}, err
	}
	if _, err := a.conn.WaitForTerminalExit(ctx, acp.WaitForTerminalExitRequest{SessionId: p.SessionId, TerminalId: term.TerminalId}); err != nil {
		return acp.PromptResponse{}, err
	}
	termOut, err := a.conn.TerminalOutput(ctx, acp.TerminalOutputRequest{SessionId: p.SessionId, TerminalId: term.TerminalId})
	if err != nil {
		return acp.PromptResponse{}, err
	}
	_, _ = a.conn.ReleaseTerminal(ctx, acp.ReleaseTerminalRequest{SessionId: p.SessionId, TerminalId: term.TerminalId})

	_ = a.conn.SessionUpdate(ctx, acp.SessionNotification{
		SessionId: p.SessionId,
		Update: acp.UpdateAgentMessageText(
			"read: " + strings.TrimSpace(read.Content) + " term: " + strings.TrimSpace(termOut.Output),
		),
	})
	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

func (*fakeACPAgent) ResumeSession(context.Context, acp.ResumeSessionRequest) (acp.ResumeSessionResponse, error) {
	return acp.ResumeSessionResponse{}, nil
}

func (*fakeACPAgent) SetSessionConfigOption(context.Context, acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	return acp.SetSessionConfigOptionResponse{}, nil
}

func (*fakeACPAgent) SetSessionMode(context.Context, acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	return acp.SetSessionModeResponse{}, nil
}
