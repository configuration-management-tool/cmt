// SPDX-License-Identifier: BSD-3-Clause
//
// Copyright (c) 2026, the configuration-management-tool/cmt authors

package interactive

import (
	"context"
	"errors"
	"os/exec"
)

// dialLocalSession starts rawCmd through /bin/sh -c on the local
// machine, live: cmd.StdinPipe/StdoutPipe/StderrPipe wired straight into
// the returned hostSession, exactly matching connect.LocalHost's
// no-network local execution used by the buffered orchestrate path
// (transport.NewLocal), just streaming instead of buffered.
//
// ctx cancellation (Runner.Run's context, tied to SIGINT by the caller)
// kills the process automatically: exec.CommandContext documents exactly
// that behavior, so no separate context-watcher goroutine is needed here
// the way ssh.go needs one for its network client/session.
func dialLocalSession(ctx context.Context, host, rawCmd string) (*hostSession, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", rawCmd)

	// StdinPipe/StdoutPipe/StderrPipe only ever error if the
	// corresponding Cmd.Std{in,out,err} field is already set, or the
	// command has already started — neither is true here (cmd is fresh,
	// and these three calls all happen before Start below), so these
	// three branches cannot be reached through this function's own call
	// pattern. They are still gone through when they fire, so they are
	// called through the seams below and exercised by swapping those,
	// the way interactive.go's isLocalHost already is. The alternative
	// was leaving three error returns that nothing has ever executed.
	stdin, err := cmdStdinPipe(cmd)
	if err != nil {
		return nil, err
	}
	stdout, err := cmdStdoutPipe(cmd)
	if err != nil {
		return nil, err
	}
	stderr, err := cmdStderrPipe(cmd)
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	return &hostSession{
		host:   host,
		stdin:  stdin,
		stdout: stdout,
		stderr: stderr,
		wait: func() (int, bool, error) {
			return classifyLocalWait(cmd.Wait(), ctx.Err())
		},
		close: func() error {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			return nil
		},
	}, nil
}

// Seams for the three pipe constructors, so their error returns can be
// executed by a test rather than only read. Package-level and swapped
// with t.Cleanup restore, matching interactive.go's isLocalHost; no test
// in this package calls t.Parallel, so the swap is not racing anything.
var (
	cmdStdinPipe  = (*exec.Cmd).StdinPipe
	cmdStdoutPipe = (*exec.Cmd).StdoutPipe
	cmdStderrPipe = (*exec.Cmd).StderrPipe
)

// classifyLocalWait turns one cmd.Wait() outcome into the triple
// hostSession.wait reports. It takes both errors as arguments rather
// than reading them from a closure so that every branch — including
// the last, a Wait failure that is not an *exec.ExitError — can be
// exercised directly. That case is reachable in principle (an I/O error
// unrelated to the process's own exit) but not through driveHost's
// single-Wait-call usage, so calling it here is the only way it is ever
// run.
//
// ctxErr is checked FIRST and on its own: when the context is cancelled
// the process was killed by exec.CommandContext, so werr describes the
// kill rather than anything the command did, and reporting an exit code
// from it would be reporting our own signal as the command's result.
func classifyLocalWait(werr, ctxErr error) (int, bool, error) {
	if ctxErr != nil {
		return -1, true, nil
	}
	if werr == nil {
		return 0, false, nil
	}
	var ee *exec.ExitError
	if errors.As(werr, &ee) {
		return ee.ExitCode(), false, nil
	}
	return -1, false, werr
}
