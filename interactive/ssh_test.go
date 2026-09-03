// SPDX-License-Identifier: BSD-3-Clause
//
// Copyright (c) 2026, the configuration-management-tool/cmt authors

package interactive

import (
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/configuration-management-tool/cmt/manifest"
)

// sshGroup builds a manifest.HostsGroup wired to srv (password auth, no
// host-key checking — the test server's host key is freshly generated
// per test and never persisted, so there is nothing to check against).
func sshGroup(srv *testSSHServer) manifest.HostsGroup {
	return manifest.HostsGroup{
		SSH: &manifest.SSHConfig{Password: srv.Password},
	}
}

func sshHostSpec(srv *testSSHServer) string {
	return srv.User + "@" + srv.Addr
}

// TestDialSSHSessionEcho exercises dialSSHSession directly against the
// in-process test server: a real SSH handshake, a real "exec" channel,
// live stdin -> stdout streaming.
func TestDialSSHSessionEcho(t *testing.T) {
	srv := newTestSSHServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sess, err := dialSSHSession(ctx, sshHostSpec(srv), sshGroup(srv), `while IFS= read -r line; do echo "got:$line"; done`)
	if err != nil {
		t.Fatalf("dialSSHSession: %v", err)
	}
	defer sess.close()

	out := make(chan string, 1)
	go func() {
		buf := make([]byte, 256)
		var acc []byte
		for {
			n, err := sess.stdout.Read(buf)
			if n > 0 {
				acc = append(acc, buf[:n]...)
				if strings.Contains(string(acc), "got:hello") {
					out <- string(acc)
					return
				}
			}
			if err != nil {
				out <- string(acc)
				return
			}
		}
	}()

	if _, err := sess.stdin.Write([]byte("hello\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	select {
	case got := <-out:
		if !strings.Contains(got, "got:hello") {
			t.Errorf("output = %q, want it to contain %q", got, "got:hello")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for echoed output")
	}

	sess.stdin.Close()
	exitCode, stillRunning, err := sess.wait()
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if stillRunning {
		t.Error("stillRunning = true, want false")
	}
	if exitCode != 0 {
		t.Errorf("exitCode = %d, want 0", exitCode)
	}
}

// TestDialSSHSessionAuthFailure covers a bad password.
func TestDialSSHSessionAuthFailure(t *testing.T) {
	srv := newTestSSHServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	group := manifest.HostsGroup{SSH: &manifest.SSHConfig{Password: "wrong"}}
	if _, err := dialSSHSession(ctx, sshHostSpec(srv), group, "true"); err == nil {
		t.Fatal("expected an error for a wrong password")
	}
}

// TestDialSSHSessionUnreachable covers a connection failure (nothing
// listening).
func TestDialSSHSessionUnreachable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	group := manifest.HostsGroup{SSH: &manifest.SSHConfig{Password: "x"}}
	if _, err := dialSSHSession(ctx, "nobody@127.0.0.2:1", group, "true"); err == nil {
		t.Fatal("expected a dial error against an unreachable port")
	}
}

// TestDialSSHSessionExitCode covers a non-zero remote exit code being
// reported correctly.
func TestDialSSHSessionExitCode(t *testing.T) {
	srv := newTestSSHServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sess, err := dialSSHSession(ctx, sshHostSpec(srv), sshGroup(srv), "exit 7")
	if err != nil {
		t.Fatalf("dialSSHSession: %v", err)
	}
	defer sess.close()

	exitCode, stillRunning, err := sess.wait()
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if stillRunning {
		t.Error("stillRunning = true, want false")
	}
	if exitCode != 7 {
		t.Errorf("exitCode = %d, want 7", exitCode)
	}
}

// TestDialSSHSessionContextCancel covers the SIGINT path: canceling ctx
// force-closes the session/client so a blocked wait() returns promptly
// instead of hanging on a still-running remote command.
func TestDialSSHSessionContextCancel(t *testing.T) {
	srv := newTestSSHServer(t)
	ctx, cancel := context.WithCancel(context.Background())

	sess, err := dialSSHSession(ctx, sshHostSpec(srv), sshGroup(srv), "sleep 30")
	if err != nil {
		t.Fatalf("dialSSHSession: %v", err)
	}
	defer sess.close()

	cancel()

	done := make(chan struct{})
	var stillRunning bool
	var waitErr error
	go func() {
		_, stillRunning, waitErr = sess.wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("wait() did not return after context cancellation")
	}
	if !stillRunning {
		t.Errorf("stillRunning = false, want true")
	}
	if waitErr != nil {
		t.Errorf("err = %v, want nil for an intentional cancellation", waitErr)
	}
}

// TestDialSSHSessionParseHostError covers dialSSHSession's own
// connect.ParseHost error passthrough.
func TestDialSSHSessionParseHostError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := dialSSHSession(ctx, "user@host:notaport", manifest.HostsGroup{}, "true"); err == nil {
		t.Fatal("expected an error for a malformed host string")
	}
}

// TestDialSSHSessionNoAuthMethod covers dialSSHSession's passthrough of
// remoteexec.DialSSH's own "no auth method configured" error when the
// group's ssh{} block has no password, private key, or UseAgent set.
func TestDialSSHSessionNoAuthMethod(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	group := manifest.HostsGroup{SSH: &manifest.SSHConfig{}}
	if _, err := dialSSHSession(ctx, "user@127.0.0.1:1", group, "true"); err == nil {
		t.Fatal("expected an error with no SSH auth method configured")
	}
}

// TestDialSSHSessionUserAndPortFromConfig covers both the "hostSpec has
// no user@ prefix, fall back to the ssh{} block's user" branch and the
// "hostSpec has no :port, fall back to the ssh{} block's port" branch,
// proven by actually completing a real handshake using values pulled
// only from the manifest config, not the host string.
func TestDialSSHSessionUserAndPortFromConfig(t *testing.T) {
	srv := newTestSSHServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	host, portStr, err := net.SplitHostPort(srv.Addr)
	if err != nil {
		t.Fatalf("parsing test server addr: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parsing test server port: %v", err)
	}
	group := manifest.HostsGroup{SSH: &manifest.SSHConfig{User: srv.User, Port: port, Password: srv.Password}}

	// hostSpec is a bare host, no "user@" and no ":port" — both must
	// come from group.SSH.
	sess, err := dialSSHSession(ctx, host, group, "exit 0")
	if err != nil {
		t.Fatalf("dialSSHSession: %v", err)
	}
	defer sess.close()
	if code, running, err := sess.wait(); err != nil || running || code != 0 {
		t.Errorf("outcome = code=%d running=%v err=%v", code, running, err)
	}
}

// TestDialSSHSessionDefaultUserAndPort covers the "no host-string user
// and no ssh{} block at all" default-to-root, and the "no host-string
// port and no ssh{} block port" default-to-22 branches. Neither default
// will actually authenticate against the test server (whose fixed user
// is "testuser", and nothing listens on real port 22 here), so this
// only asserts an error occurs — the point is exercising the default
// selection code, not a successful session.
func TestDialSSHSessionDefaultUserAndPort(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	group := manifest.HostsGroup{SSH: &manifest.SSHConfig{Password: "whatever"}}
	if _, err := dialSSHSession(ctx, "127.0.0.1", group, "true"); err == nil {
		t.Fatal("expected an error (default user/port do not match any listener we control)")
	}
}

// TestDialSSHSessionNewSessionFails covers client.NewSession() itself
// failing — a server that authenticates fine but rejects every "session"
// channel (e.g. no shell access configured).
func TestDialSSHSessionNewSessionFails(t *testing.T) {
	srv := newTestSSHServerMode(t, sshServerRejectSession)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := dialSSHSession(ctx, sshHostSpec(srv), sshGroup(srv), "true"); err == nil {
		t.Fatal("expected an error when the server rejects the session channel")
	} else if !strings.Contains(err.Error(), "opening ssh session") {
		t.Errorf("err = %v", err)
	}
}

// TestDialSSHSessionStartFails covers sess.Start() itself failing — a
// server that accepts the session channel but closes it before handling
// any request.
func TestDialSSHSessionStartFails(t *testing.T) {
	srv := newTestSSHServerMode(t, sshServerCloseOnAccept)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := dialSSHSession(ctx, sshHostSpec(srv), sshGroup(srv), "true"); err == nil {
		t.Fatal("expected an error when the server closes the session channel immediately")
	} else if !strings.Contains(err.Error(), "starting") {
		t.Errorf("err = %v", err)
	}
}

// TestDialSSHSessionWaitNonExitError covers sess.wait()'s fallback
// branch for a Wait() failure that isn't *ssh.ExitError: a server that
// starts the command but tears the channel down without ever sending
// "exit-status" — the wire-level equivalent of a dropped connection.
func TestDialSSHSessionWaitNonExitError(t *testing.T) {
	srv := newTestSSHServerMode(t, sshServerCloseWithoutExitStatus)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sess, err := dialSSHSession(ctx, sshHostSpec(srv), sshGroup(srv), "true")
	if err != nil {
		t.Fatalf("dialSSHSession: %v", err)
	}
	defer sess.close()

	_, stillRunning, waitErr := sess.wait()
	if stillRunning {
		t.Error("stillRunning = true, want false (not a context cancellation)")
	}
	if waitErr == nil {
		t.Fatal("expected a non-nil, non-ExitError wait() error")
	}
}

// Private-key/passphrase/host-key-check/known_hosts/ssh-agent auth-method
// construction used to be tested directly here, against this package's
// own now-deleted buildSSHClientConfig/knownHostsPath/sshAgentAuthMethod.
// That logic lives in github.com/go-remoteexec/transport now (see ssh.go's
// header comment) — connect_test.go already covers every manifest.SSHConfig
// field reaching the right remoteexec.SSHConfig field (the mapping this
// package and the buffered path both depend on), and the tests below still
// exercise dialSSHSession end-to-end including a real password-auth
// handshake against the in-process test server.
