package acptest

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/memohai/memoh/internal/workspace/bridge"
)

// ContainerBridgeClient builds (once) and starts the toolkit workspace image
// with the bridge listening on TCP, binds dataRoot to /data, and returns a
// connected bridge client. This is the REAL container backend the production
// runtime uses — the ACP agent (codex/claude) is installed in the image and
// runs inside the container, not on the host. Requires docker; callers gate
// on it.
//
// Set MEMOH_LIVE_ACP_CONTAINER_IMAGE to reuse a prebuilt image and skip the
// ~5 minute build.
func ContainerBridgeClient(t *testing.T, dataRoot string) *bridge.Client {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker is required for the container backend test: %v", err)
	}
	repoRoot := findRepoRoot(t)
	image := strings.TrimSpace(os.Getenv("MEMOH_LIVE_ACP_CONTAINER_IMAGE"))
	if image == "" {
		image = "memoh-toolkit-acp-bridge-live:local"
		runDockerCmd(t, repoRoot, 10*time.Minute,
			"build", "-f", "docker/Dockerfile.server",
			"--target", "toolkit-acp-bridge-live", "-t", image, ".")
	}

	args := []string{"run", "-d", "--rm", "-e", "BRIDGE_TCP_ADDR=:1455", "-p", "127.0.0.1::1455"}
	if strings.TrimSpace(dataRoot) != "" {
		args = append(args, "-v", dataRoot+":/data")
	}
	args = append(args, image)
	containerID := strings.TrimSpace(runDockerCmd(t, repoRoot, time.Minute, args...))
	if containerID == "" {
		t.Fatal("docker run did not return a container id")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, "docker", "rm", "-f", containerID).Run() //nolint:gosec // operator-controlled cleanup.
	})

	port := waitForDockerBridgePort(t, containerID)
	client, err := bridge.Dial(context.Background(), net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		t.Fatalf("dial bridge: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	waitForBridgeExec(t, client)
	return client
}

func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			if _, err := os.Stat(filepath.Join(dir, "docker", "Dockerfile.server")); err == nil {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("failed to locate repository root")
		}
		dir = parent
	}
}

func runDockerCmd(t *testing.T, workDir string, timeout time.Duration, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...) //nolint:gosec // fixed docker args assembled by test code.
	cmd.Dir = workDir
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("docker %s timed out: %v\n%s", strings.Join(args, " "), ctx.Err(), string(output))
	}
	if err != nil {
		t.Fatalf("docker %s failed: %v\n%s", strings.Join(args, " "), err, string(output))
	}
	return string(output)
}

func waitForDockerBridgePort(t *testing.T, containerID string) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		out, err := exec.CommandContext(ctx, "docker", "port", containerID, "1455/tcp").CombinedOutput() //nolint:gosec // inspects an operator-created container.
		cancel()
		if err == nil {
			fields := strings.Fields(strings.TrimSpace(string(out)))
			if len(fields) > 0 {
				if _, port, splitErr := net.SplitHostPort(fields[len(fields)-1]); splitErr == nil && port != "" {
					return port
				}
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	logs, _ := exec.CommandContext(ctx, "docker", "logs", containerID).CombinedOutput() //nolint:gosec // reads logs from an operator-created container.
	t.Fatalf("bridge port not published for container %s\n%s", containerID, string(logs))
	return ""
}

func waitForBridgeExec(t *testing.T, client *bridge.Client) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		result, err := client.Exec(ctx, "true", "/data", 5)
		cancel()
		if err == nil && result.ExitCode == 0 {
			return
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("exit code %d: %s%s", result.ExitCode, result.Stdout, result.Stderr)
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("bridge exec never became ready: %v", lastErr)
}
