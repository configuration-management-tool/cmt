// SPDX-License-Identifier: BSD-3-Clause
//
// Copyright (c) 2026, the configuration-management-tool/cmt authors

package interactive

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

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
// buildSSHClientConfig's "no auth method configured" error.
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

// TestBuildSSHClientConfigNoAuth covers the "no auth method available"
// error when neither a private key, a password, nor a reachable
// ssh-agent is configured.
func TestBuildSSHClientConfigNoAuth(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	if _, err := buildSSHClientConfig("u", &manifest.SSHConfig{}); err == nil {
		t.Fatal("expected an error with no auth method configured")
	}
	if _, err := buildSSHClientConfig("u", nil); err == nil {
		t.Fatal("expected an error with a nil SSHConfig and no ambient auth")
	}
}

// TestBuildSSHClientConfigPrivateKey covers both the plain and
// passphrase-protected private-key auth paths, plus the read/parse
// error paths.
func TestBuildSSHClientConfigPrivateKey(t *testing.T) {
	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir, "")

	cfg, err := buildSSHClientConfig("u", &manifest.SSHConfig{PrivateKey: keyPath})
	if err != nil {
		t.Fatalf("buildSSHClientConfig: %v", err)
	}
	if len(cfg.Auth) == 0 {
		t.Fatal("expected at least one auth method")
	}

	passphrasePath := writeTestPrivateKey(t, dir, "s3cret")
	cfg, err = buildSSHClientConfig("u", &manifest.SSHConfig{PrivateKey: passphrasePath, PrivateKeyPassphrase: "s3cret"})
	if err != nil {
		t.Fatalf("buildSSHClientConfig (passphrase): %v", err)
	}
	if len(cfg.Auth) == 0 {
		t.Fatal("expected at least one auth method")
	}

	// A missing key file.
	if _, err := buildSSHClientConfig("u", &manifest.SSHConfig{PrivateKey: dir + "/does-not-exist"}); err == nil {
		t.Fatal("expected an error for a missing private key file")
	}

	// A key file that isn't a valid key.
	badPath := dir + "/bad.key"
	if err := writeFile(badPath, "not a key"); err != nil {
		t.Fatal(err)
	}
	if _, err := buildSSHClientConfig("u", &manifest.SSHConfig{PrivateKey: badPath}); err == nil {
		t.Fatal("expected an error for an unparseable private key")
	}

	// A passphrase-protected key given the wrong passphrase.
	if _, err := buildSSHClientConfig("u", &manifest.SSHConfig{PrivateKey: passphrasePath, PrivateKeyPassphrase: "wrong"}); err == nil {
		t.Fatal("expected an error for the wrong passphrase")
	}
}

// TestBuildSSHClientConfigHostKeyCheck covers HostKeyCheck=true using a
// known_hosts file, including the "file does not exist" error path.
func TestBuildSSHClientConfigHostKeyCheck(t *testing.T) {
	dir := t.TempDir()
	khPath := dir + "/known_hosts"
	if err := writeFile(khPath, ""); err != nil {
		t.Fatal(err)
	}

	cfg, err := buildSSHClientConfig("u", &manifest.SSHConfig{
		Password: "x", HostKeyCheck: true, KnownHostsFile: khPath,
	})
	if err != nil {
		t.Fatalf("buildSSHClientConfig: %v", err)
	}
	if cfg.HostKeyCallback == nil {
		t.Fatal("expected a non-nil HostKeyCallback")
	}

	if _, err := buildSSHClientConfig("u", &manifest.SSHConfig{
		Password: "x", HostKeyCheck: true, KnownHostsFile: dir + "/does-not-exist",
	}); err == nil {
		t.Fatal("expected an error for a missing known_hosts file")
	}
}

// TestKnownHostsPathDefault covers the empty-KnownHostsFile fallback to
// ~/.ssh/known_hosts.
func TestKnownHostsPathDefault(t *testing.T) {
	if got := knownHostsPath("/explicit/path"); got != "/explicit/path" {
		t.Errorf("knownHostsPath(explicit) = %q", got)
	}
	if got := knownHostsPath(""); got == "" || !strings.HasSuffix(got, "/.ssh/known_hosts") {
		t.Errorf("knownHostsPath(\"\") = %q, want a ~/.ssh/known_hosts path", got)
	}
}

// TestKnownHostsPathHomeUnset covers knownHostsPath's own fallback when
// os.UserHomeDir itself fails (unix: an empty $HOME).
func TestKnownHostsPathHomeUnset(t *testing.T) {
	t.Setenv("HOME", "")
	if got := knownHostsPath(""); got != "" {
		t.Errorf("knownHostsPath(\"\") with no $HOME = %q, want empty", got)
	}
}

// TestBuildSSHClientConfigConnectTimeout covers the ConnectTimeout > 0
// override (the zero-value/library-default path is exercised by every
// other buildSSHClientConfig test, which all pass ConnectTimeout unset).
func TestBuildSSHClientConfigConnectTimeout(t *testing.T) {
	cfg, err := buildSSHClientConfig("u", &manifest.SSHConfig{Password: "x", ConnectTimeout: 7})
	if err != nil {
		t.Fatalf("buildSSHClientConfig: %v", err)
	}
	if cfg.Timeout != 7*time.Second {
		t.Errorf("Timeout = %v, want 7s", cfg.Timeout)
	}
}

// TestSSHAgentAuthMethod covers both branches of sshAgentAuthMethod: no
// $SSH_AUTH_SOCK set, and one pointing at something unreachable.
func TestSSHAgentAuthMethod(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	if _, ok := sshAgentAuthMethod(); ok {
		t.Error("expected ok=false with no SSH_AUTH_SOCK set")
	}

	t.Setenv("SSH_AUTH_SOCK", "/does/not/exist.sock")
	if _, ok := sshAgentAuthMethod(); ok {
		t.Error("expected ok=false with an unreachable SSH_AUTH_SOCK")
	}
}

// writeTestPrivateKey generates a fresh RSA key, PEM-encodes it
// (optionally passphrase-protected, using golang.org/x/crypto/ssh's own
// encoding so it round-trips through ssh.ParsePrivateKey(WithPassphrase)
// exactly as a real key file would), writes it to dir, and returns its
// path.
func writeTestPrivateKey(t *testing.T, dir, passphrase string) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating test private key: %v", err)
	}

	var block *pem.Block
	if passphrase != "" {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(key, "", []byte(passphrase))
	} else {
		block, err = ssh.MarshalPrivateKey(key, "")
	}
	if err != nil {
		t.Fatalf("marshaling test private key: %v", err)
	}

	path := filepath.Join(dir, "id_"+strconv.Itoa(len(passphrase))+"_test")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("writing test private key: %v", err)
	}
	return path
}

func writeFile(path, contents string) error {
	return os.WriteFile(path, []byte(contents), 0o600)
}
