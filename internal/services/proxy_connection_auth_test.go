package services

import (
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"piccolod/internal/api"
)

// TestProxy_ConnectionAuth_DenyBlocksLANConnection is the plan §D16 step-6
// end-to-end test: a listener configured with a deny rule covering the local
// loopback should reject incoming connections at the L4 chain (before they
// reach the backend echo server). Per plan §D6 / §D14.
func TestProxy_ConnectionAuth_DenyBlocksLANConnection(t *testing.T) {
	hb, stop := startEchoBackend(t)
	defer stop()

	pm := NewProxyManager()
	defer pm.StopAll()

	public := getFreePort(t)
	ep := ServiceEndpoint{
		App:        "test",
		Name:       "echo",
		HostBind:   hb,
		PublicPort: public,
		Flow:       api.FlowTCP,
		Protocol:   api.ListenerProtocolRaw,
		ConnectionAuth: &api.ConnectionAuth{
			Default: "allow",
			Rules: []api.ConnectionAuthRule{
				// Loopback (127.0.0.0/8) is the source for our test dial.
				{Match: "127.0.0.0/8", Strategy: "deny"},
			},
		},
	}
	pm.StartListener(ep)
	time.Sleep(100 * time.Millisecond)

	// Dial — the L4 chain's connection_auth should deny, closing the conn
	// before the echo backend ever sees it.
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(public)), 2*time.Second)
	if err != nil {
		// If the kernel rejected the conn pre-handshake, that's also a deny.
		return
	}
	defer conn.Close()

	// Conn might appear "established" at the kernel because the listener
	// accepted before the L4 chain ran. Writing then reading should fail
	// promptly because the chain closed the conn.
	conn.SetDeadline(time.Now().Add(time.Second))
	if _, err := conn.Write([]byte("hello\n")); err != nil {
		// Write may fail on a half-closed conn — that's the deny path too.
		return
	}
	buf := make([]byte, 8)
	n, err := conn.Read(buf)
	if err == nil && n > 0 {
		t.Fatalf("expected conn to be denied, but received %d bytes: %q", n, buf[:n])
	}
	if err != nil && err != io.EOF {
		// Any error (EOF, closed-conn, deadline) confirms denial.
		return
	}

	// Verify the deny was recorded in metrics.
	snap := pm.l4Metrics.Snapshot()
	if len(snap.Denied) == 0 {
		t.Fatal("expected denied counter to be incremented for connection_auth")
	}
	foundConnAuth := false
	for k := range snap.Denied {
		if k.DenyReason == "connection_auth" {
			foundConnAuth = true
			break
		}
	}
	if !foundConnAuth {
		t.Errorf("expected denied with reason \"connection_auth\", got snapshot %+v", snap.Denied)
	}
}

// TestProxy_ConnectionAuth_AllowPasses verifies the symmetric case: a
// listener with default allow + no matching deny rule lets the conn through
// to the backend (regression guard against accidental fail-closed).
func TestProxy_ConnectionAuth_AllowPasses(t *testing.T) {
	hb, stop := startEchoBackend(t)
	defer stop()

	pm := NewProxyManager()
	defer pm.StopAll()

	public := getFreePort(t)
	ep := ServiceEndpoint{
		App:        "test",
		Name:       "echo",
		HostBind:   hb,
		PublicPort: public,
		Flow:       api.FlowTCP,
		Protocol:   api.ListenerProtocolRaw,
		ConnectionAuth: &api.ConnectionAuth{
			Default: "allow",
			Rules:   nil,
		},
	}
	pm.StartListener(ep)
	time.Sleep(100 * time.Millisecond)

	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(public)), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(time.Second))
	msg := []byte("hello\n")
	if _, err := conn.Write(msg); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, len(msg))
	if _, err := conn.Read(buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != string(msg) {
		t.Fatalf("echo mismatch: got %q, want %q", buf, msg)
	}
}
