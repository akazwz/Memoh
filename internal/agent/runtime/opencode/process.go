package opencode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/felinics/memoh/internal/agent/runtime/agentprocess"
	"github.com/felinics/memoh/internal/agent/runtime/external"
	"github.com/felinics/memoh/internal/agent/runtime/opencode/opencodecfg"
	"github.com/felinics/memoh/internal/workspace/bridge"
)

const containerPath = "/data/.memoh/deps/bin:/opt/memoh/toolkit/bin:/usr/local/bin:/usr/bin:/bin"

type server struct {
	api       *apiClient
	version   string
	proc      *agentprocess.Process
	transport *http.Transport
	cancel    context.CancelFunc
	cleanup   func()
}

func (s *server) close() {
	s.cancel()
	s.transport.CloseIdleConnections()
	_ = s.proc.Close()
	if s.cleanup != nil {
		s.cleanup()
	}
}

func stateRoot(agentID, threadID string) (string, error) {
	for _, id := range []string{agentID, threadID} {
		if _, err := uuid.Parse(id); err != nil {
			return "", errors.New("opencode state requires agent and thread UUIDs")
		}
	}
	return path.Join("/data/.memoh/opencode", agentID, threadID), nil
}

func launchConfig(cfg opencodecfg.Config, input external.PromptInput, mcpURL string) map[string]any {
	config := map[string]any{"autoupdate": false, "share": "disabled", "server": map[string]any{"mdns": false}}
	if cfg.Model != "" {
		config["model"] = cfg.Model
	}
	if cfg.Auth == opencodecfg.AuthAPIKey {
		options := map[string]any{"apiKey": cfg.APIKey}
		if cfg.BaseURL != "" {
			options["baseURL"] = cfg.BaseURL
		}
		config["provider"] = map[string]any{cfg.ProviderID: map[string]any{"options": options}}
		config["enabled_providers"] = []string{cfg.ProviderID}
	}
	// Permissions are applied to the session each turn. Putting the preset
	// in global config would also rewrite the native plan agent's restrictions.
	if mcpURL != "" {
		config["mcp"] = map[string]any{"memoh": map[string]any{"type": "remote", "url": mcpURL, "oauth": false, "timeout": 900000}}
	}
	return config
}

func startServer(ctx context.Context, client *bridge.Client, launcher string, cfg opencodecfg.Config, input external.PromptInput, mcpURL string) (*server, error) {
	root, err := stateRoot(input.BotAgentID, firstNonEmpty(metadataString(input.RuntimeMetadata, metadataStoreIDKey), input.ThreadID))
	if err != nil {
		return nil, err
	}
	cwd := firstNonEmpty(metadataString(input.RuntimeMetadata, "project_path"), "/data")
	launch := launchConfig(cfg, input, mcpURL)
	cleanup := func() {}
	if input.Command != "" && input.ContextMarkdown != "" {
		// /command has no system field. Native instructions preserve Memoh's
		// system context without altering command arguments or templates.
		directory := path.Join(root, "instructions")
		if err := client.Mkdir(ctx, directory); err != nil {
			return nil, err
		}
		filename := path.Join(directory, uuid.NewString()+".md")
		if err := client.WriteFile(ctx, filename, []byte(input.ContextMarkdown)); err != nil {
			return nil, err
		}
		cleanup = func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = client.DeleteFile(ctx, filename, false)
		}
		launch["instructions"] = []string{filename}
	}
	transferred := false
	defer func() {
		if !transferred {
			cleanup()
		}
	}()
	config, err := json.Marshal(launch)
	if err != nil {
		return nil, err
	}
	password := uuid.NewString() + uuid.NewString()
	env := []string{"HOME=/data", "PATH=" + containerPath, "XDG_DATA_HOME=" + root + "/data", "XDG_STATE_HOME=" + root + "/state", "XDG_CACHE_HOME=/data/.cache", "XDG_CONFIG_HOME=/data/.config", "OPENCODE_SERVER_PASSWORD=" + password, "OPENCODE_DISABLE_AUTOUPDATE=true", "OPENCODE_CONFIG_CONTENT=" + string(config)}
	if cfg.Auth == opencodecfg.AuthAPIKey {
		auth, err := json.Marshal(map[string]any{cfg.ProviderID: map[string]string{"type": "api", "key": cfg.APIKey}})
		if err != nil {
			return nil, err
		}
		env = append(env, "OPENCODE_AUTH_CONTENT="+string(auth))
	}
	if cfg.Auth == opencodecfg.AuthWorkspace {
		// Explicit workspace auth may read a user-managed native login; runtime
		// session databases remain isolated from that shared authentication file.
		file, readErr := client.ReadFile(ctx, "/data/.local/share/opencode/auth.json", 0, 0)
		if readErr == nil {
			env = append(env, "OPENCODE_AUTH_CONTENT="+file.GetContent())
		} else if !errors.Is(readErr, bridge.ErrNotFound) {
			return nil, readErr
		}
	}
	processCtx, cancel := context.WithCancel(ctx)
	proc, err := agentprocess.Start(processCtx, client, shellQuote(launcher)+" serve --hostname 127.0.0.1 --port 0", cwd, env)
	if err != nil {
		cancel()
		return nil, err
	}
	address := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(proc)
		scanner.Buffer(make([]byte, 4096), 1<<20)
		for scanner.Scan() {
			const prefix = "opencode server listening on "
			if strings.HasPrefix(scanner.Text(), prefix) {
				select {
				case address <- strings.TrimSpace(strings.TrimPrefix(scanner.Text(), prefix)):
				default:
				}
			}
		}
	}()
	transport := &http.Transport{DialContext: client.DialContext, ResponseHeaderTimeout: 0, IdleConnTimeout: 30 * time.Second}
	srv := &server{proc: proc, cancel: cancel, transport: transport, cleanup: cleanup}
	transferred = true
	startupCtx, stop := context.WithTimeout(ctx, 60*time.Second)
	defer stop()
	select {
	case base := <-address:
		u, parseErr := url.Parse(base)
		if parseErr != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.User != nil || u.Path != "" {
			srv.close()
			return nil, errors.New("opencode returned an invalid listener address")
		}
		srv.api = &apiClient{http: &http.Client{Transport: transport}, baseURL: base, password: password, directory: cwd}
		var health struct {
			Healthy bool   `json:"healthy"`
			Version string `json:"version"`
		}
		if err := srv.api.call(startupCtx, http.MethodGet, "/global/health", nil, &health); err != nil {
			srv.close()
			return nil, err
		}
		if !health.Healthy {
			srv.close()
			return nil, errors.New("opencode is not healthy")
		}
		srv.version = health.Version
		return srv, nil
	case <-proc.Done():
		srv.close()
		return nil, fmt.Errorf("opencode exited during startup: %w", proc.Err())
	case <-startupCtx.Done():
		srv.close()
		return nil, startupCtx.Err()
	}
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }
func metadataString(metadata map[string]any, key string) string {
	v, _ := metadata[key].(string)
	return strings.TrimSpace(v)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}
