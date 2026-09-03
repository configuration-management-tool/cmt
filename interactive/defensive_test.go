// SPDX-License-Identifier: BSD-3-Clause
//
// Copyright (c) 2026, the configuration-management-tool/cmt authors

package interactive

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/configuration-management-tool/cmt/manifest"
)

// The pipe-constructor error paths in dialLocalSession and
// dialSSHSession cannot be reached by calling those functions: the
// underlying Cmd and Session are freshly made and untouched, which is
// the only state in which StdinPipe and friends succeed. They are not
// dead code — each one returns, and the SSH ones close the session and
// the dialed client on the way out — but nothing had ever executed
// them, so a mistake in that cleanup would have sat there indefinitely.
//
// These tests reach them through the package-level seams the two files
// now call, restoring each one with t.Cleanup. No test in this package
// calls t.Parallel, so swapping a package variable is not racing a
// concurrent test.

var errPipe = errors.New("interactive: injected pipe failure")

func TestDialLocalSessionPipeFailures(t *testing.T) {
	for _, tc := range []struct {
		name  string
		patch func(t *testing.T)
	}{
		{"stdin", func(t *testing.T) {
			orig := cmdStdinPipe
			cmdStdinPipe = func(*exec.Cmd) (io.WriteCloser, error) { return nil, errPipe }
			t.Cleanup(func() { cmdStdinPipe = orig })
		}},
		{"stdout", func(t *testing.T) {
			orig := cmdStdoutPipe
			cmdStdoutPipe = func(*exec.Cmd) (io.ReadCloser, error) { return nil, errPipe }
			t.Cleanup(func() { cmdStdoutPipe = orig })
		}},
		{"stderr", func(t *testing.T) {
			orig := cmdStderrPipe
			cmdStderrPipe = func(*exec.Cmd) (io.ReadCloser, error) { return nil, errPipe }
			t.Cleanup(func() { cmdStderrPipe = orig })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.patch(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			sess, err := dialLocalSession(ctx, "localhost", "true")
			if !errors.Is(err, errPipe) {
				t.Fatalf("dialLocalSession error = %v, want %v", err, errPipe)
			}
			// The session must not be handed back half-built: driveHost
			// would dereference stdin/stdout on it.
			if sess != nil {
				t.Fatalf("dialLocalSession returned a session alongside an error: %+v", sess)
			}
		})
	}
}

// TestClassifyLocalWait covers every outcome hostSession.wait reports,
// including the last one — a Wait failure that is not an *exec.ExitError.
// That case is reachable in principle (an I/O error unrelated to the
// process's own exit) but not through driveHost's single-Wait-call
// usage, so this is the only thing that ever runs it.
func TestClassifyLocalWait(t *testing.T) {
	// A real *exec.ExitError, rather than a hand-built one: ExitCode()
	// reads the underlying ProcessState, so a zero-valued ExitError
	// would panic and a fake would prove nothing about the real type.
	realExit := exec.Command("sh", "-c", "exit 7").Run()
	var ee *exec.ExitError
	if !errors.As(realExit, &ee) {
		t.Fatalf("setup: `exit 7` gave %T (%v), want *exec.ExitError", realExit, realExit)
	}

	other := errors.New("i/o error unrelated to the process exit")
	cancelled := context.Canceled

	for _, tc := range []struct {
		name         string
		werr, ctxErr error
		code         int
		running      bool
		wantErr      error
	}{
		// ctxErr wins even when Wait also failed: the process was killed
		// by exec.CommandContext, so werr describes our own signal.
		{"context cancelled beats a wait error", realExit, cancelled, -1, true, nil},
		{"context cancelled, no wait error", nil, cancelled, -1, true, nil},
		{"clean exit", nil, nil, 0, false, nil},
		{"non-zero exit", realExit, nil, 7, false, nil},
		{"wait error that is not an ExitError", other, nil, -1, false, other},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, running, err := classifyLocalWait(tc.werr, tc.ctxErr)
			if code != tc.code || running != tc.running || !errors.Is(err, tc.wantErr) {
				t.Fatalf("classifyLocalWait(%v, %v) = (%d, %v, %v), want (%d, %v, %v)",
					tc.werr, tc.ctxErr, code, running, err, tc.code, tc.running, tc.wantErr)
			}
		})
	}
}

func TestDialSSHSessionPipeFailures(t *testing.T) {
	for _, tc := range []struct {
		name  string
		patch func(t *testing.T)
	}{
		{"stdin", func(t *testing.T) {
			orig := sshStdinPipe
			sshStdinPipe = func(*ssh.Session) (io.WriteCloser, error) { return nil, errPipe }
			t.Cleanup(func() { sshStdinPipe = orig })
		}},
		{"stdout", func(t *testing.T) {
			orig := sshStdoutPipe
			sshStdoutPipe = func(*ssh.Session) (io.Reader, error) { return nil, errPipe }
			t.Cleanup(func() { sshStdoutPipe = orig })
		}},
		{"stderr", func(t *testing.T) {
			orig := sshStderrPipe
			sshStderrPipe = func(*ssh.Session) (io.Reader, error) { return nil, errPipe }
			t.Cleanup(func() { sshStderrPipe = orig })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestSSHServer(t)
			tc.patch(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// A real handshake and a real exec channel first: the seam
			// fires only after the session exists, which is the state
			// whose cleanup is under test.
			sess, err := dialSSHSession(ctx, sshHostSpec(srv), sshGroup(srv), "true")
			if !errors.Is(err, errPipe) {
				t.Fatalf("dialSSHSession error = %v, want %v", err, errPipe)
			}
			if sess != nil {
				t.Fatalf("dialSSHSession returned a session alongside an error: %+v", sess)
			}
		})
	}
}

// The ssh-agent path is the one piece of this package whose coverage
// depended on the machine running the tests rather than on the tests
// themselves: sshAgentAuthMethod reads $SSH_AUTH_SOCK, so a developer
// laptop with an agent covered the dial-and-succeed branch while a CI
// runner without one covered the empty-socket branch, and neither
// covered both. The gate is 100%, so it passed locally and failed in
// Actions at 99.7% with no test having changed.
//
// The socket this test serves itself pins the third state, so the
// result no longer depends on what happens to be running around the
// tests.

// TestSSHAgentAuthMethodConnected adds the one branch ssh_test.go's
// TestSSHAgentAuthMethod cannot reach: a socket that actually accepts.
// That test covers the two refusals (no $SSH_AUTH_SOCK, and one pointing
// at nothing), which is every branch a machine WITHOUT an agent runs --
// and on a machine with one, the dial succeeds instead and the refusals
// were the ones going unrun. Either way the package's coverage depended
// on whether an agent happened to be running around the tests, which is
// how this repository passed its own 100% gate locally and failed it in
// Actions at 99.7% with no test having changed.
func TestSSHAgentAuthMethodConnected(t *testing.T) {
	if am := agentSocket(t); am == nil {
		t.Fatal("sshAgentAuthMethod reported no agent while a listener was accepting")
	}
}

// TestBuildSSHClientConfigUsesAgent covers the append in
// buildSSHClientConfig that only runs when an agent answers — and, with
// no key and no password configured, it is also the only auth method
// present, so it is what keeps the function from returning its
// "no usable authentication" error.
func TestBuildSSHClientConfigUsesAgent(t *testing.T) {
	agentSocket(t)

	cfg, err := buildSSHClientConfig("someone", &manifest.SSHConfig{})
	if err != nil {
		t.Fatalf("buildSSHClientConfig with an agent available: %v", err)
	}
	if len(cfg.Auth) != 1 {
		t.Fatalf("cfg.Auth has %d methods, want exactly the agent's one", len(cfg.Auth))
	}
}

// agentSocket points $SSH_AUTH_SOCK at a Unix socket this test listens
// on, and returns what sshAgentAuthMethod makes of it. The listener
// accepts and closes: net.Dial is all sshAgentAuthMethod does before
// handing the connection to agent.NewClient, so nothing here needs to
// speak the agent protocol — and pretending to would be testing
// x/crypto/ssh/agent rather than this package.
func agentSocket(t *testing.T) ssh.AuthMethod {
	t.Helper()

	// Kept short deliberately: a Unix socket path is capped near 104
	// bytes on darwin, and the per-subtest temp directory names here
	// are long enough to make that a real risk rather than a note.
	dir, err := os.MkdirTemp("", "ag")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listening on %s: %v", sock, err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	// Registered in this order deliberately: t.Cleanup runs LIFO, so the
	// listener is closed FIRST and the wait for the accept loop second.
	// The other way round deadlocks -- the goroutine only returns once
	// Accept fails, which only happens once the listener is closed -- and
	// it deadlocks silently, as a test binary that never finishes rather
	// than one that fails.
	t.Cleanup(func() { <-done })
	t.Cleanup(func() { ln.Close() })

	t.Setenv("SSH_AUTH_SOCK", sock)
	am, ok := sshAgentAuthMethod()
	if !ok {
		t.Fatalf("sshAgentAuthMethod found no agent on %s", sock)
	}
	return am
}
