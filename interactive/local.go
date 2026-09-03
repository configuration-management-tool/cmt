// SPDX-License-Identifier: BSD-3-Clause
//
// Copyright (c) 2026, the configuration-management-tool/cmt authors

package interactive

import (
	"context"
	"fmt"

	remoteexec "github.com/go-remoteexec/transport"
)

// localNewSessionFunc is (*remoteexec.Local).NewSession behind a variable
// so a test can force its error return — today's implementation never
// actually fails (it only builds a struct, no I/O happens until Start),
// so this branch cannot be reached through dialLocalSession's own call
// pattern, but the seam keeps that error path exercised rather than only
// read, matching connect's own dialSSHFunc/dialWinRMFunc seams.
var localNewSessionFunc = (*remoteexec.Local).NewSession

// dialLocalSession starts rawCmd through a live github.com/go-remoteexec/
// transport Session on the local machine — the same transport.NewLocal()
// the buffered orchestrate path dials, just streaming (via NewSession)
// instead of buffered (via Exec).
//
// ctx cancellation (Runner.Run's context, tied to SIGINT by the caller)
// kills the process automatically: remoteexec's local Session is built on
// exec.CommandContext, which documents exactly that behavior, so no
// separate context-watcher goroutine is needed here the way ssh.go needs
// one for its network client/session.
func dialLocalSession(ctx context.Context, host, rawCmd string) (*hostSession, error) {
	sess, err := localNewSessionFunc(remoteexec.NewLocal(), ctx)
	if err != nil {
		return nil, fmt.Errorf("interactive: opening local session: %w", err)
	}

	// As with ssh.go's session pipe calls, these three only ever error on
	// a session that's already had Start called or the corresponding pipe
	// already requested — neither applies to a freshly opened sess here,
	// so they cannot be reached through this function's own call pattern.
	// Exercised through the same shared seams ssh.go uses (sessStdinPipe/
	// sessStdoutPipe/sessStderrPipe operate on remoteexec.Session, which
	// both the local and SSH sessions satisfy).
	stdin, err := sessStdinPipe(sess)
	if err != nil {
		sess.Close()
		return nil, err
	}
	stdout, err := sessStdoutPipe(sess)
	if err != nil {
		sess.Close()
		return nil, err
	}
	stderr, err := sessStderrPipe(sess)
	if err != nil {
		sess.Close()
		return nil, err
	}

	if err := sess.Start(rawCmd); err != nil {
		sess.Close()
		return nil, fmt.Errorf("interactive: starting local command %q: %w", rawCmd, err)
	}

	return &hostSession{
		host:   host,
		stdin:  stdin,
		stdout: stdout,
		stderr: stderr,
		wait: func() (int, bool, error) {
			rc, werr := sess.Wait()
			return classifyWait(rc, werr, ctx.Err())
		},
		close: func() error {
			return sess.Close()
		},
	}, nil
}
