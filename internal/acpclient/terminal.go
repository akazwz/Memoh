package acpclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"unicode/utf8"

	acp "github.com/coder/acp-go-sdk"

	"github.com/memohai/memoh/internal/workspace/bridge"
	pb "github.com/memohai/memoh/internal/workspace/bridgepb"
)

const (
	defaultTerminalOutputLimit = 128 * 1024
	maxTerminalOutputLimit     = 1024 * 1024
	defaultTerminalTimeout     = int32(600)
)

type terminalManager struct {
	ctx         context.Context
	client      *bridge.Client
	root        string
	defaultCwd  string
	timeout     int32
	baseEnv     []string
	virtualRoot bool

	mu        sync.Mutex
	nextID    int
	terminals map[string]*terminal
}

type terminal struct {
	stream *bridge.ExecStream
	limit  int

	mu        sync.Mutex
	output    string
	truncated bool
	exitCode  *int
	signal    *string
	done      chan struct{}
	doneOnce  sync.Once
}

func newTerminalManager(ctx context.Context, client *bridge.Client, root, defaultCwd string, timeoutSeconds int32, baseEnv []string, virtualRoot bool) *terminalManager { //nolint:contextcheck // terminal streams must live for the ACP turn, not a single RPC callback.
	if timeoutSeconds <= 0 {
		timeoutSeconds = defaultTerminalTimeout
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return &terminalManager{
		ctx:         ctx,
		client:      client,
		root:        root,
		defaultCwd:  defaultCwd,
		timeout:     timeoutSeconds,
		baseEnv:     append([]string(nil), baseEnv...),
		virtualRoot: virtualRoot,
		terminals:   map[string]*terminal{},
	}
}

func (m *terminalManager) CreateTerminal(_ context.Context, p acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	cwd := m.defaultCwd
	if p.Cwd != nil && strings.TrimSpace(*p.Cwd) != "" {
		resolved, err := m.resolvePath(*p.Cwd)
		if err != nil {
			return acp.CreateTerminalResponse{}, err
		}
		cwd = resolved
	}
	command := buildShellCommand(p.Command, p.Args)
	if strings.TrimSpace(command) == "" {
		return acp.CreateTerminalResponse{}, errors.New("terminal command is required")
	}

	limit := defaultTerminalOutputLimit
	if p.OutputByteLimit != nil && *p.OutputByteLimit > 0 {
		limit = *p.OutputByteLimit
		if limit > maxTerminalOutputLimit {
			limit = maxTerminalOutputLimit
		}
	}

	env := append([]string(nil), m.baseEnv...)
	for _, item := range p.Env {
		name := strings.TrimSpace(item.Name)
		if name == "" {
			continue
		}
		env = append(env, name+"="+item.Value)
	}
	stream, err := m.client.ExecStreamWithEnv(m.ctx, command, cwd, m.timeout, env) //nolint:contextcheck // use the ACP turn context so terminal output survives the create RPC.
	if err != nil {
		return acp.CreateTerminalResponse{}, err
	}

	term := &terminal{stream: stream, limit: limit, done: make(chan struct{})}
	id := m.nextTerminalID()
	m.mu.Lock()
	m.terminals[id] = term
	m.mu.Unlock()

	go term.readLoop()
	return acp.CreateTerminalResponse{TerminalId: id}, nil
}

func (m *terminalManager) resolvePath(path string) (string, error) {
	if m.virtualRoot {
		return ResolvePathUnderVirtualRoot(m.root, path)
	}
	return ResolvePathUnderRoot(m.root, path)
}

func (m *terminalManager) KillTerminal(_ context.Context, p acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	term, err := m.get(p.TerminalId)
	if err != nil {
		return acp.KillTerminalResponse{}, err
	}
	term.kill("killed")
	return acp.KillTerminalResponse{}, nil
}

func (m *terminalManager) TerminalOutput(_ context.Context, p acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	term, err := m.get(p.TerminalId)
	if err != nil {
		return acp.TerminalOutputResponse{}, err
	}
	output, truncated, status := term.snapshot()
	if output == "" {
		output = "\n"
	}
	return acp.TerminalOutputResponse{Output: output, Truncated: truncated, ExitStatus: status}, nil
}

func (m *terminalManager) ReleaseTerminal(_ context.Context, p acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	term, err := m.remove(p.TerminalId)
	if err != nil {
		return acp.ReleaseTerminalResponse{}, err
	}
	term.kill("released")
	return acp.ReleaseTerminalResponse{}, nil
}

func (m *terminalManager) WaitForTerminalExit(ctx context.Context, p acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	term, err := m.get(p.TerminalId)
	if err != nil {
		return acp.WaitForTerminalExitResponse{}, err
	}
	select {
	case <-term.done:
	case <-ctx.Done():
		return acp.WaitForTerminalExitResponse{}, ctx.Err()
	}
	code, signal := term.exit()
	return acp.WaitForTerminalExitResponse{ExitCode: code, Signal: signal}, nil
}

func (m *terminalManager) killAll() {
	m.mu.Lock()
	terms := make([]*terminal, 0, len(m.terminals))
	for id, term := range m.terminals {
		terms = append(terms, term)
		delete(m.terminals, id)
	}
	m.mu.Unlock()
	for _, term := range terms {
		term.kill("closed")
	}
}

func (m *terminalManager) nextTerminalID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	return fmt.Sprintf("term-%d", m.nextID)
}

func (m *terminalManager) get(id string) (*terminal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	term := m.terminals[id]
	if term == nil {
		return nil, fmt.Errorf("terminal %q not found", id)
	}
	return term, nil
}

func (m *terminalManager) remove(id string) (*terminal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	term := m.terminals[id]
	if term == nil {
		return nil, fmt.Errorf("terminal %q not found", id)
	}
	delete(m.terminals, id)
	return term, nil
}

func (t *terminal) readLoop() {
	for {
		output, err := t.stream.Recv()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				sig := "stream_error"
				t.finish(nil, &sig)
			} else {
				code := 0
				t.finish(&code, nil)
			}
			return
		}
		switch output.GetStream() {
		case pb.ExecOutput_STDOUT, pb.ExecOutput_STDERR:
			t.appendOutput(string(output.GetData()))
		case pb.ExecOutput_EXIT:
			code := int(output.GetExitCode())
			t.finish(&code, nil)
			return
		}
	}
}

func (t *terminal) appendOutput(s string) {
	if s == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.output += s
	if t.limit > 0 && len(t.output) > t.limit {
		t.truncated = true
		t.output = safeUTF8Suffix(t.output, t.limit)
	}
}

func (t *terminal) snapshot() (string, bool, *acp.TerminalExitStatus) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var status *acp.TerminalExitStatus
	if t.exitCode != nil || t.signal != nil {
		status = &acp.TerminalExitStatus{ExitCode: t.exitCode, Signal: t.signal}
	}
	return t.output, t.truncated, status
}

func (t *terminal) exit() (*int, *string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.exitCode, t.signal
}

func (t *terminal) kill(signal string) {
	_ = t.stream.Close()
	t.finish(nil, &signal)
}

func (t *terminal) finish(code *int, signal *string) {
	t.doneOnce.Do(func() {
		t.mu.Lock()
		if code != nil {
			v := *code
			t.exitCode = &v
		}
		if signal != nil {
			v := *signal
			t.signal = &v
		}
		t.mu.Unlock()
		close(t.done)
	})
}

func safeUTF8Suffix(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	start := len(s) - maxBytes
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}
