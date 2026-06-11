package acpclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/memohai/memoh/internal/acpprofile"
	"github.com/memohai/memoh/internal/workspace/bridge"
	pb "github.com/memohai/memoh/internal/workspace/bridgepb"
)

const (
	stderrTailLimit      = 8 * 1024
	defaultContainerPath = "/opt/memoh/toolkit/bin:/usr/local/bin:/usr/bin:/bin"
	containerToolkitBin  = "/opt/memoh/toolkit/bin"
	noProjectWorkDirPart = "/.memoh/acp-work/no-project/"
)

var (
	commandResolveWindow = 5 * time.Second
	commandResolveDelay  = 200 * time.Millisecond
)

type WorkspaceBackend string

const (
	WorkspaceBackendLocal     WorkspaceBackend = "local"
	WorkspaceBackendContainer WorkspaceBackend = "container"
)

type SetupMode string

const (
	SetupModeAPIKey SetupMode = "api_key"
	SetupModeOAuth  SetupMode = "oauth"
	SetupModeSelf   SetupMode = "self"
)

type processOptions struct {
	Backend   WorkspaceBackend
	AgentID   string
	SetupMode SetupMode
	Env       []string
	// WorkspaceRoot is the real (host) workspace root for local backends. It is
	// used to point Codex at a bot-scoped CODEX_HOME so BYOK credentials stay
	// isolated from the user's real ~/.codex.
	WorkspaceRoot string
	NoTimeout     bool
}

type bridgeProcess struct {
	stream  *bridge.ExecStream
	stdin   *io.PipeWriter
	stdout  *io.PipeReader
	tail    *stderrTail
	done    chan struct{}
	env     []string
	cleanup func()
	once    sync.Once
}

func startBridgeProcess(ctx context.Context, client *bridge.Client, command string, args []string, workDir string, timeout time.Duration, opts processOptions) (*bridgeProcess, error) {
	if client == nil {
		return nil, errors.New("workspace bridge client is required")
	}
	command = strings.TrimSpace(command)
	if command == "" {
		return nil, errors.New("ACP command is required")
	}
	if strings.Contains(filepath.ToSlash(workDir), noProjectWorkDirPart) {
		if err := client.Mkdir(ctx, workDir); err != nil {
			return nil, fmt.Errorf("prepare ACP cwd: %w", err)
		}
	}
	timeoutSeconds := int32(timeout.Seconds())
	if opts.NoTimeout {
		timeoutSeconds = -1
	} else if timeoutSeconds <= 0 {
		timeoutSeconds = int32(DefaultRunTimeout.Seconds())
	}

	env, cleanup, err := prepareProcessEnv(ctx, client, workDir, opts)
	if err != nil {
		return nil, err
	}

	resolvedCommand, err := resolveCommand(ctx, client, command, workDir, env, opts.Backend)
	if err != nil {
		if cleanup != nil {
			cleanup()
		}
		return nil, err
	}

	shellCommand := buildShellCommand(resolvedCommand, args)
	execStream, err := client.ExecStreamWithEnv(ctx, shellCommand, workDir, timeoutSeconds, env)
	if err != nil {
		if cleanup != nil {
			cleanup()
		}
		return nil, err
	}

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	proc := &bridgeProcess{
		stream:  execStream,
		stdin:   stdinW,
		stdout:  stdoutR,
		tail:    &stderrTail{},
		done:    make(chan struct{}),
		env:     append([]string(nil), env...),
		cleanup: cleanup,
	}

	go func() {
		defer func() { _ = stdinR.Close() }()
		buf := make([]byte, 32*1024)
		for {
			n, readErr := stdinR.Read(buf)
			if n > 0 {
				if sendErr := execStream.SendStdin(buf[:n]); sendErr != nil {
					_ = stdoutW.CloseWithError(sendErr)
					return
				}
			}
			if readErr != nil {
				return
			}
		}
	}()

	go func() {
		defer close(proc.done)
		for {
			output, recvErr := execStream.Recv()
			if recvErr != nil {
				if !errors.Is(recvErr, io.EOF) {
					_ = stdoutW.CloseWithError(recvErr)
				} else {
					_ = stdoutW.Close()
				}
				return
			}
			switch output.GetStream() {
			case pb.ExecOutput_STDOUT:
				if _, err := stdoutW.Write(output.GetData()); err != nil {
					_ = stdoutW.CloseWithError(err)
					return
				}
			case pb.ExecOutput_STDERR:
				proc.tail.append(output.GetData())
			case pb.ExecOutput_EXIT:
				_ = stdoutW.Close()
				return
			}
		}
	}()

	return proc, nil
}

func prepareProcessEnv(ctx context.Context, client *bridge.Client, workDir string, opts processOptions) ([]string, func(), error) {
	// Per-agent containment behavior comes from the profile registry, never
	// from agent-ID conditionals: a new agent with known shapes onboards by
	// declaration only (architecture doc §5.6).
	profile, _ := acpprofile.Lookup(opts.AgentID)

	if opts.Backend == WorkspaceBackendLocal {
		env, err := prepareLocalProcessEnv(opts, profile)
		if err != nil {
			return nil, nil, err
		}
		// Managed settings (Claude-style ask/deny pins) are written into the
		// scoped local config dir so the host's real config — permission
		// allow rules, hooks, MCP servers — never leaks into a managed
		// session. Self mode keeps the host config by design.
		if profile.ManagedSettings == acpprofile.ManagedSettingsClaude && normalizeSetupMode(opts.SetupMode) != SetupModeSelf {
			if err := WriteClaudeManagedSettingsDir(ctx, client, path.Join(dataMountPath, profile.LocalIsolationDir)); err != nil {
				return nil, nil, fmt.Errorf("prepare managed settings: %w", err)
			}
		}
		return env, nil, nil
	}

	mode := normalizeSetupMode(opts.SetupMode)

	env := withoutEnvKeys(opts.Env, "HOME", "PATH", "CODEX_HOME")
	switch mode {
	case SetupModeAPIKey, SetupModeOAuth:
		tempHomeDir := "/tmp/memoh-acp/" + uuid.NewString()
		homeDir := tempHomeDir
		if profile.ContainerHomeStrategy == acpprofile.ContainerHomePersistent {
			homeDir = dataMountPath
		}
		env = append(env, "HOME="+homeDir, "PATH="+defaultContainerPath)

		if err := client.Mkdir(ctx, homeDir); err != nil {
			return nil, nil, fmt.Errorf("prepare ACP HOME: %w", err)
		}
		// Cleanup intentionally derives a fresh background ctx with its own
		// short deadline: the parent ctx is usually already cancelled by the
		// time we tear down the ACP HOME, but we still want to issue rm -rf.
		// Defined before the settings write so a write failure can reclaim the
		// freshly-created HOME instead of orphaning it.
		cleanup := func() { //nolint:contextcheck // cleanup uses independent background ctx by design.
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if homeDir == tempHomeDir {
				_, _ = client.ExecWithEnv(cleanupCtx, "rm -rf "+escapeShellArg(homeDir), workDir, 5, env)
			}
		}
		// The managed HOME doubles as the config dir for settings-pinned
		// agents; local backends get the same isolation via the scoped
		// config-dir env above.
		if profile.ManagedSettings == acpprofile.ManagedSettingsClaude {
			if err := WriteClaudeManagedSettings(ctx, client, homeDir); err != nil {
				cleanup()
				return nil, nil, fmt.Errorf("prepare managed settings: %w", err)
			}
		}

		return env, cleanup, nil
	case SetupModeSelf:
		// Self mode deliberately writes no managed settings/env (the agent runs
		// against config the operator supplied out of band). For a settings-
		// pinned agent (Claude) that means no ask:[Bash] containment pin — which
		// is NOT a reachable bypass: self mode also injects no credentials
		// (managedProcessEnv returns nil for self), so a container self-mode
		// Claude with no operator-baked config can't authenticate and never runs
		// a tool. If the operator DID bake in their own config+creds, self mode
		// respecting that config (permissions included) is the contract. The
		// managed (api_key/oauth) path above is what production uses and it does
		// pin ask:[Bash].
		env = withoutEnvKeys(opts.Env, "HOME", "PATH", "CODEX_HOME")
		env = append(env, "PATH="+defaultContainerPath)
		if profile.ContainerHomeStrategy == acpprofile.ContainerHomePersistent {
			env = append(env, "HOME="+dataMountPath)
		}
		return env, nil, nil
	default:
		return nil, nil, fmt.Errorf("unsupported ACP setup mode %q", mode)
	}
}

// prepareLocalProcessEnv builds the managed env overrides for a local (desktop)
// workspace. Local agents run as host processes that inherit the host
// environment (the bridge appends our entries onto os.Environ), so we only add
// the BYOK overrides and never touch HOME/PATH. Self mode returns no overrides
// so the host's own agent login is used as-is.
func prepareLocalProcessEnv(opts processOptions, profile acpprofile.Profile) ([]string, error) {
	mode := normalizeSetupMode(opts.SetupMode)
	if mode == SetupModeSelf {
		return nil, nil
	}
	env := append([]string(nil), opts.Env...)
	if profile.LocalIsolationEnv != "" {
		// The agent reads its config from this env var; point it at the
		// bot-scoped managed dir under the workspace root so BYOK credentials
		// don't clobber the user's real host config. If the root is unknown
		// we must fail loudly: silently skipping the override would leak BYOK
		// credentials into the host config.
		root := strings.TrimSpace(opts.WorkspaceRoot)
		if root == "" {
			return nil, fmt.Errorf("local %s BYOK requires a workspace root for %s isolation", strings.TrimSpace(opts.AgentID), profile.LocalIsolationEnv)
		}
		env = withoutEnvKeys(env, profile.LocalIsolationEnv)
		env = append(env, profile.LocalIsolationEnv+"="+filepath.Join(root, filepath.FromSlash(profile.LocalIsolationDir)))
	}
	if len(env) == 0 {
		return nil, nil
	}
	return env, nil
}

func normalizeSetupMode(mode SetupMode) SetupMode {
	switch SetupMode(strings.ToLower(strings.TrimSpace(string(mode)))) {
	case SetupModeOAuth:
		return SetupModeOAuth
	case SetupModeSelf:
		return SetupModeSelf
	default:
		return SetupModeAPIKey
	}
}

func resolveCommand(ctx context.Context, client *bridge.Client, command, workDir string, env []string, backend WorkspaceBackend) (string, error) {
	command = strings.TrimSpace(command)
	resolved, lastResult, err := resolveCommandOnce(ctx, client, command, workDir, env, backend)
	if err != nil || resolved != "" || backend != WorkspaceBackendContainer {
		if resolved != "" || err != nil {
			return resolved, err
		}
		return "", commandNotAvailableError(command, lastResult, backend)
	}

	deadline := time.Now().Add(commandResolveWindow)
	for time.Now().Before(deadline) {
		timer := time.NewTimer(commandResolveDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		case <-timer.C:
		}

		resolved, result, err := resolveCommandOnce(ctx, client, command, workDir, env, backend)
		if result != nil {
			lastResult = result
		}
		if err != nil {
			return "", err
		}
		if resolved != "" {
			return resolved, nil
		}
	}
	return "", commandNotAvailableError(command, lastResult, backend)
}

func resolveCommandOnce(ctx context.Context, client *bridge.Client, command, workDir string, env []string, backend WorkspaceBackend) (string, *bridge.ExecResult, error) {
	command = strings.TrimSpace(command)
	if !isPlainCommand(command) {
		return command, nil, nil
	}

	if strings.Contains(command, "/") {
		result, err := checkCommand(ctx, client, "test -x "+escapeShellArg(command), workDir, env)
		if err != nil {
			return "", nil, fmt.Errorf("check ACP command %q: %w", command, err)
		}
		if result.ExitCode == 0 {
			return command, result, nil
		}
		return "", result, nil
	}

	result, err := checkCommand(ctx, client, "command -v "+escapeShellArg(command)+" >/dev/null 2>&1", workDir, env)
	if err != nil {
		return "", nil, fmt.Errorf("check ACP command %q: %w", command, err)
	}
	if result.ExitCode == 0 {
		return command, result, nil
	}
	lastResult := result

	if backend == WorkspaceBackendContainer {
		toolkitCommand := containerToolkitBin + "/" + command
		toolkitResult, err := checkCommand(ctx, client, "test -x "+escapeShellArg(toolkitCommand), workDir, env)
		if err != nil {
			return "", nil, fmt.Errorf("check ACP command %q: %w", command, err)
		}
		lastResult = toolkitResult
		if toolkitResult.ExitCode == 0 {
			return toolkitCommand, toolkitResult, nil
		}
	}

	return "", lastResult, nil
}

func checkCommand(ctx context.Context, client *bridge.Client, check, workDir string, env []string) (*bridge.ExecResult, error) {
	return client.ExecWithEnv(ctx, check, workDir, 10, env)
}

func commandNotAvailableError(command string, result *bridge.ExecResult, backend WorkspaceBackend) error {
	detail := ""
	if result != nil {
		detail = strings.TrimSpace(result.Stderr)
		if detail == "" {
			detail = strings.TrimSpace(result.Stdout)
		}
	}
	if detail != "" {
		detail = ": " + detail
	}
	if backend == WorkspaceBackendLocal {
		return fmt.Errorf("ACP command %q is not available to the workspace process%s. Install the ACP agent command and restart Memoh Desktop/local server so PATH is inherited", command, detail)
	}
	return fmt.Errorf("ACP command %q is not available in the workspace PATH or %s%s. Install it in the workspace or rebuild the Memoh workspace runtime with %s available", command, containerToolkitBin, detail, containerToolkitBin)
}

func isPlainCommand(command string) bool {
	command = strings.TrimSpace(command)
	if command == "" {
		return false
	}
	return !strings.ContainsAny(command, " \t\n'\"\\$&;|<>*?()[]{}!`")
}

// exited reports whether the process's output stream has terminated (process
// exit or transport death). Cheap, non-blocking.
func (p *bridgeProcess) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func (p *bridgeProcess) Read(b []byte) (int, error) {
	return p.stdout.Read(b)
}

func (p *bridgeProcess) Write(b []byte) (int, error) {
	return p.stdin.Write(b)
}

func (p *bridgeProcess) Close() error {
	p.once.Do(func() {
		if p.stdin != nil {
			_ = p.stdin.Close()
		}
		if p.stdout != nil {
			_ = p.stdout.Close()
		}
		if p.stream != nil {
			_ = p.stream.Close()
		}
		if p.cleanup != nil {
			p.cleanup()
		}
	})
	select {
	case <-p.done:
	case <-time.After(2 * time.Second):
	}
	return nil
}

func withoutEnvKeys(env []string, keys ...string) []string {
	if len(env) == 0 {
		return nil
	}
	blocked := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		blocked[key] = struct{}{}
	}
	out := make([]string, 0, len(env))
	for _, item := range env {
		key, _, ok := strings.Cut(item, "=")
		if !ok {
			out = append(out, item)
			continue
		}
		if _, skip := blocked[key]; skip {
			continue
		}
		out = append(out, item)
	}
	return out
}

func (p *bridgeProcess) errorWithStderr(err error) error {
	if err == nil {
		err = io.EOF
	}
	if strings.TrimSpace(p.tail.String()) == "" {
		select {
		case <-p.done:
		case <-time.After(250 * time.Millisecond):
		}
	}
	stderr := strings.TrimSpace(p.tail.String())
	if stderr == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, stderr)
}

type stderrTail struct {
	mu  sync.Mutex
	buf string
}

func (t *stderrTail) append(data []byte) {
	if t == nil || len(data) == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf += string(data)
	if len(t.buf) > stderrTailLimit {
		t.buf = t.buf[len(t.buf)-stderrTailLimit:]
	}
}

func (t *stderrTail) String() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.buf
}

func escapeShellArg(value string) string {
	if value == "" {
		return "''"
	}
	if !strings.ContainsAny(value, " \t\n'\"\\$&;|<>*?()[]{}!`") {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func buildShellCommand(command string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, escapeShellArg(strings.TrimSpace(command)))
	for _, arg := range args {
		parts = append(parts, escapeShellArg(arg))
	}
	return strings.Join(parts, " ")
}
