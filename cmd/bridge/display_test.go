package main

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
)

func TestDisplayGeometryDefaultsToFourThree(t *testing.T) {
	t.Setenv(displayGeometryEnv, "")

	if got := displayGeometry(); got != "1280x960" {
		t.Fatalf("displayGeometry() = %q, want 1280x960", got)
	}
}

func TestDisplayGeometryCanBeOverridden(t *testing.T) {
	t.Setenv(displayGeometryEnv, "1440x1080")

	if got := displayGeometry(); got != "1440x1080" {
		t.Fatalf("displayGeometry() = %q, want 1440x1080", got)
	}
}

func TestDisplayTCPReadyRequiresRFBNoneSecurity(t *testing.T) {
	addr := startFakeRFBServer(t, []byte{2}, false)

	if displayTCPReady(context.Background(), addr) {
		t.Fatal("displayTCPReady must reject VNC servers that do not offer None security")
	}
}

func TestDisplayTCPReadyAcceptsRFBNoneSecurity(t *testing.T) {
	addr := startFakeRFBServer(t, []byte{1}, true)

	if !displayTCPReady(context.Background(), addr) {
		t.Fatal("displayTCPReady should accept VNC servers that complete None security negotiation")
	}
}

func TestDisplayTCPReadyAcceptsRFB33NoneSecurity(t *testing.T) {
	addr := startFakeRFB33Server(t, 1)

	if !displayTCPReady(context.Background(), addr) {
		t.Fatal("displayTCPReady should accept RFB 3.3 VNC servers with None security")
	}
}

func TestDisplayTCPReadyRejectsRFB33VNCAuth(t *testing.T) {
	addr := startFakeRFB33Server(t, 2)

	if displayTCPReady(context.Background(), addr) {
		t.Fatal("displayTCPReady must reject RFB 3.3 VNC auth servers")
	}
}

func TestIsBrowserArgMatchesRealBrowserExecutables(t *testing.T) {
	t.Parallel()

	for _, arg := range []string{
		"chromium",
		"/usr/bin/chromium",
		"/usr/lib/chromium/chromium",
		"google-chrome-stable",
		"/opt/google/chrome/chrome",
	} {
		if !isBrowserArg(arg) {
			t.Fatalf("expected %q to be recognized as a browser executable", arg)
		}
	}
}

func startFakeRFBServer(t *testing.T, securityTypes []byte, acceptNone bool) string {
	t.Helper()
	if len(securityTypes) != 1 {
		t.Fatalf("fake RFB server only supports one security type, got %d", len(securityTypes))
	}
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen fake RFB server: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	errCh := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			errCh <- err
			return
		}
		defer func() { _ = conn.Close() }()

		if _, err := conn.Write([]byte("RFB 003.008\n")); err != nil {
			errCh <- err
			return
		}
		version := make([]byte, 12)
		if _, err := io.ReadFull(conn, version); err != nil {
			errCh <- err
			return
		}
		if _, err := conn.Write(append([]byte{1}, securityTypes...)); err != nil {
			errCh <- err
			return
		}
		if !acceptNone {
			errCh <- nil
			return
		}
		selection := []byte{0}
		if _, err := io.ReadFull(conn, selection); err != nil {
			errCh <- err
			return
		}
		result := make([]byte, 4)
		if selection[0] != 1 {
			binary.BigEndian.PutUint32(result, 1)
		}
		if _, err := conn.Write(result); err != nil {
			errCh <- err
			return
		}
		errCh <- nil
	}()
	t.Cleanup(func() {
		if err := <-errCh; err != nil && !isClosedNetworkError(err) {
			t.Fatalf("fake RFB server failed: %v", err)
		}
	})
	return listener.Addr().String()
}

func startFakeRFB33Server(t *testing.T, securityType uint32) string {
	t.Helper()
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen fake RFB 3.3 server: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	errCh := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			errCh <- err
			return
		}
		defer func() { _ = conn.Close() }()

		if _, err := conn.Write([]byte("RFB 003.003\n")); err != nil {
			errCh <- err
			return
		}
		version := make([]byte, 12)
		if _, err := io.ReadFull(conn, version); err != nil {
			errCh <- err
			return
		}
		response := make([]byte, 4)
		binary.BigEndian.PutUint32(response, securityType)
		_, err = conn.Write(response)
		errCh <- err
	}()
	t.Cleanup(func() {
		if err := <-errCh; err != nil && !isClosedNetworkError(err) {
			t.Fatalf("fake RFB 3.3 server failed: %v", err)
		}
	})
	return listener.Addr().String()
}

func isClosedNetworkError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, net.ErrClosed)
}

func TestIsBrowserArgRejectsShellCommandsContainingBrowserText(t *testing.T) {
	t.Parallel()

	for _, arg := range []string{
		"sh -lc command -v chromium",
		"--remote-debugging-port=9222",
		"/tmp/memoh-display-prepare.sh",
	} {
		if isBrowserArg(arg) {
			t.Fatalf("expected %q not to be recognized as a browser executable", arg)
		}
	}
}
