// SPDX-License-Identifier: BSD-3-Clause
//
// Copyright (c) 2026, the configuration-management-tool/cmt authors

package interactive

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	remoteexec "github.com/go-remoteexec/transport"

	"github.com/configuration-management-tool/cmt/manifest"
)

// The pipe-constructor error paths in dialLocalSession and
// dialSSHSession cannot be reached by calling those functions: the
// underlying remoteexec.Session is freshly opened and untouched, which is
// the only state in which StdinPipe and friends succeed. They are not
// dead code — each one returns, and both close the session (and, for
// SSH, the dialed connection) on the way out — but nothing had ever
// executed them, so a mistake in that cleanup would have sat there
// indefinitely.
//
// These tests reach them through the sessStdinPipe/sessStdoutPipe/
// sessStderrPipe seams both local.go and ssh.go call (shared, since both
// now drive a remoteexec.Session identically), restoring each one with
// t.Cleanup. No test in this package calls t.Parallel, so swapping a
// package variable is not racing a concurrent test.

var errPipe = errors.New("interactive: injected pipe failure")

func TestDialLocalSessionPipeFailures(t *testing.T) {
	for _, tc := range []struct {
		name  string
		patch func(t *testing.T)
	}{
		{"stdin", func(t *testing.T) {
			orig := sessStdinPipe
			sessStdinPipe = func(remoteexec.Session) (io.WriteCloser, error) { return nil, errPipe }
			t.Cleanup(func() { sessStdinPipe = orig })
		}},
		{"stdout", func(t *testing.T) {
			orig := sessStdoutPipe
			sessStdoutPipe = func(remoteexec.Session) (io.Reader, error) { return nil, errPipe }
			t.Cleanup(func() { sessStdoutPipe = orig })
		}},
		{"stderr", func(t *testing.T) {
			orig := sessStderrPipe
			sessStderrPipe = func(remoteexec.Session) (io.Reader, error) { return nil, errPipe }
			t.Cleanup(func() { sessStderrPipe = orig })
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

func TestDialSSHSessionPipeFailures(t *testing.T) {
	for _, tc := range []struct {
		name  string
		patch func(t *testing.T)
	}{
		{"stdin", func(t *testing.T) {
			orig := sessStdinPipe
			sessStdinPipe = func(remoteexec.Session) (io.WriteCloser, error) { return nil, errPipe }
			t.Cleanup(func() { sessStdinPipe = orig })
		}},
		{"stdout", func(t *testing.T) {
			orig := sessStdoutPipe
			sessStdoutPipe = func(remoteexec.Session) (io.Reader, error) { return nil, errPipe }
			t.Cleanup(func() { sessStdoutPipe = orig })
		}},
		{"stderr", func(t *testing.T) {
			orig := sessStderrPipe
			sessStderrPipe = func(remoteexec.Session) (io.Reader, error) { return nil, errPipe }
			t.Cleanup(func() { sessStderrPipe = orig })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestSSHServer(t)
			tc.patch(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// A real handshake and a real session first: the seam fires
			// only after the session exists, which is the state whose
			// cleanup is under test.
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

// TestDialLocalSessionNewSessionFails covers localNewSessionFunc's own
// error return: today's real (*remoteexec.Local).NewSession never
// actually fails (it only builds a struct; no I/O happens until Start),
// so this branch cannot be reached through dialLocalSession's own call
// pattern — exercised through the seam instead, same reasoning as the
// pipe-failure tests above.
func TestDialLocalSessionNewSessionFails(t *testing.T) {
	orig := localNewSessionFunc
	localNewSessionFunc = func(*remoteexec.Local, context.Context) (remoteexec.Session, error) {
		return nil, errPipe
	}
	t.Cleanup(func() { localNewSessionFunc = orig })

	if _, err := dialLocalSession(context.Background(), "localhost", "true"); !errors.Is(err, errPipe) {
		t.Fatalf("dialLocalSession error = %v, want %v", err, errPipe)
	}
}

// nonStreamerConn is a minimal remoteexec.Connection that deliberately
// does NOT implement Streamer, standing in for what remoteexec.Become
// actually returns — used to exercise dialSSHSession's own defensive
// type-assertion, which Run's upfront become-config check (see
// TestRunBecomeRejected) means nothing reaches in practice today.
type nonStreamerConn struct{ closed bool }

func (c *nonStreamerConn) Exec(context.Context, string, io.Reader) (remoteexec.Result, error) {
	return remoteexec.Result{}, nil
}
func (c *nonStreamerConn) Put(context.Context, string, string, remoteexec.PutOptions) error {
	return nil
}
func (c *nonStreamerConn) Fetch(context.Context, string, string) error { return nil }
func (c *nonStreamerConn) Remove(context.Context, string) error        { return nil }
func (c *nonStreamerConn) TempPath(base string) string                 { return base }
func (c *nonStreamerConn) Close() error                                { c.closed = true; return nil }

func TestDialSSHSessionNotAStreamer(t *testing.T) {
	conn := &nonStreamerConn{}
	orig := dialSSHFunc
	dialSSHFunc = func(context.Context, remoteexec.SSHConfig) (remoteexec.Connection, error) {
		return conn, nil
	}
	t.Cleanup(func() { dialSSHFunc = orig })

	_, err := dialSSHSession(context.Background(), "user@host", manifest.HostsGroup{}, "true")
	if err == nil || !strings.Contains(err.Error(), "does not support streaming sessions") {
		t.Fatalf("err = %v", err)
	}
	if !conn.closed {
		t.Error("dialSSHSession did not close the connection on the not-a-Streamer path")
	}
}

// TestClassifyWait covers every outcome classifyWait reports, including
// the last one — a Wait failure that is not a context cancellation. That
// case is reachable in principle (a remoteexec.Session.Wait failure
// unrelated to the command's own exit) but not through driveHost's
// single-Wait-call usage, so this is the only thing that ever runs it.
func TestClassifyWait(t *testing.T) {
	other := errors.New("i/o error unrelated to the process exit")
	cancelled := context.Canceled

	for _, tc := range []struct {
		name         string
		rc           int
		werr, ctxErr error
		code         int
		running      bool
		wantErr      error
	}{
		// ctxErr wins even when Wait also failed: the session was force-
		// closed by ctx cancellation, so werr describes our own signal.
		{"context cancelled beats a wait error", 0, other, cancelled, -1, true, nil},
		{"context cancelled, no wait error", 0, nil, cancelled, -1, true, nil},
		{"clean exit", 0, nil, nil, 0, false, nil},
		{"non-zero exit", 7, nil, nil, 7, false, nil},
		{"wait error unrelated to context", 0, other, nil, -1, false, other},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, running, err := classifyWait(tc.rc, tc.werr, tc.ctxErr)
			if code != tc.code || running != tc.running || !errors.Is(err, tc.wantErr) {
				t.Fatalf("classifyWait(%d, %v, %v) = (%d, %v, %v), want (%d, %v, %v)",
					tc.rc, tc.werr, tc.ctxErr, code, running, err, tc.code, tc.running, tc.wantErr)
			}
		})
	}
}
