// SPDX-License-Identifier: BSD-3-Clause
//
// Copyright (c) 2026, the configuration-management-tool/cmt authors

// This file drives a live, streaming SSH session for cmt's
// `interactive = true` commands through github.com/go-remoteexec/transport's
// Streamer/Session pair (added in v0.1.4), the same shared library
// package connect uses for the buffered path.
//
// It used to duplicate a small amount of SSH auth-method construction
// here directly against golang.org/x/crypto/ssh, because at the time this
// package was first written, go-remoteexec/transport only exposed the
// buffered Connection.Exec and had no live-session primitive to build on
// — and it was under active, unrelated development elsewhere, so this
// project deliberately avoided colliding with that in-flight work. Once
// go-remoteexec/transport grew Streamer/Session, that duplication was
// deleted in favor of what's below: dial through connect.BuildSSHConfig +
// remoteexec.DialSSH exactly like the buffered path, then open a live
// Session on the resulting connection instead of calling Exec.
package interactive

import (
	"context"
	"fmt"

	remoteexec "github.com/go-remoteexec/transport"

	"github.com/configuration-management-tool/cmt/connect"
	"github.com/configuration-management-tool/cmt/manifest"
)

// dialSSHFunc is remoteexec.DialSSH behind a variable — returning the
// Connection interface rather than *remoteexec.SSH directly, matching
// package connect's own dialSSHFunc seam, so a test can substitute a fake
// Connection that does not implement Streamer (exercising the "does not
// support streaming sessions" branch below) as well as a dial failure.
// go-remoteexec/transport already owns testing the SSH wire protocol and
// its streaming Session; this package's own tests exercise only the
// adapter logic here.
var dialSSHFunc = func(ctx context.Context, cfg remoteexec.SSHConfig) (remoteexec.Connection, error) {
	return remoteexec.DialSSH(ctx, cfg)
}

// dialSSHSession opens a live SSH session on hostSpec (honoring group's
// optional ssh{} config via the same connect.BuildSSHConfig mapping the
// buffered orchestrate path uses) and starts rawCmd on it.
func dialSSHSession(ctx context.Context, hostSpec string, group manifest.HostsGroup, rawCmd string) (*hostSession, error) {
	user, host, port, err := connect.ParseHost(hostSpec)
	if err != nil {
		return nil, err
	}
	cfg := connect.BuildSSHConfig(user, host, port, group.SSH)

	conn, err := dialSSHFunc(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("interactive: dialing %s: %w", hostSpec, err)
	}

	// Every real Connection this package ever dials here is a *remoteexec.SSH,
	// which always implements Streamer — Runner.Run already rejects winrm
	// and become-configured groups before dialSSHSession is ever reached
	// (see the checks in Run), so this assertion cannot fail through this
	// package's own call pattern. It stays a checked assertion rather than
	// a bare one so a future caller that reaches this function differently
	// gets a clear error instead of a panic.
	streamer, ok := conn.(remoteexec.Streamer)
	if !ok {
		conn.Close()
		return nil, fmt.Errorf("interactive: connection to %s does not support streaming sessions", hostSpec)
	}
	sess, err := streamer.NewSession(ctx)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("interactive: opening ssh session on %s: %w", hostSpec, err)
	}

	// As with local.go's session pipe calls, these three only ever error
	// on a session that's already had Start called or the corresponding
	// pipe already requested — neither applies to a freshly opened sess
	// here, so they cannot be reached through this function's own call
	// pattern. Each error path closes the session AND the connection,
	// which is exactly the kind of cleanup that goes wrong unnoticed when
	// nothing ever runs it, so they are called through the seams below
	// and a test swaps those.
	stdin, err := sessStdinPipe(sess)
	if err != nil {
		sess.Close()
		conn.Close()
		return nil, err
	}
	stdout, err := sessStdoutPipe(sess)
	if err != nil {
		sess.Close()
		conn.Close()
		return nil, err
	}
	stderr, err := sessStderrPipe(sess)
	if err != nil {
		sess.Close()
		conn.Close()
		return nil, err
	}

	if err := sess.Start(rawCmd); err != nil {
		sess.Close()
		conn.Close()
		return nil, fmt.Errorf("interactive: starting %q on %s: %w", rawCmd, hostSpec, err)
	}

	return &hostSession{
		host:   hostSpec,
		stdin:  stdin,
		stdout: stdout,
		stderr: stderr,
		wait: func() (int, bool, error) {
			rc, werr := sess.Wait()
			return classifyWait(rc, werr, ctx.Err())
		},
		close: func() error {
			sess.Close()
			return conn.Close()
		},
	}, nil
}

// Seams for the session's three pipe constructors, so the cleanup each
// error path performs — closing the session AND the dialed connection —
// is executed by a test rather than only inspected. Swapped with
// t.Cleanup restore, as interactive.go's isLocalHost is; no test in this
// package calls t.Parallel.
var (
	sessStdinPipe  = remoteexec.Session.StdinPipe
	sessStdoutPipe = remoteexec.Session.StdoutPipe
	sessStderrPipe = remoteexec.Session.StderrPipe
)
