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
	// three branches are structurally defensive rather than reachable
	// through this function's own call pattern; tested instead is
	// Start's own failure path (an unresolvable interpreter), which
	// *is* reachable in real use.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
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
			werr := cmd.Wait()
			if ctx.Err() != nil {
				return -1, true, nil
			}
			if werr == nil {
				return 0, false, nil
			}
			var ee *exec.ExitError
			if errors.As(werr, &ee) {
				return ee.ExitCode(), false, nil
			}
			// Reachable in principle (e.g. an I/O error unrelated to the
			// process's own exit), but not through this function's own
			// single-Wait-call usage in driveHost — left untested rather
			// than fault-injected for its own sake.
			return -1, false, werr
		},
		close: func() error {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			return nil
		},
	}, nil
}
