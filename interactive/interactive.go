// SPDX-License-Identifier: BSD-3-Clause
//
// Copyright (c) 2026, the configuration-management-tool/cmt authors

// Package interactive implements cmt's true live interactive multi-host
// stdin session: sup's `stdin: true` mode, an `interactive = true`
// command block in cmt's own manifest schema. Keystrokes read from cmt's
// own stdin are forwarded live to every resolved host's remote (SSH) or
// local process, and every host's stdout/stderr streams back live,
// prefixed per host, as it is produced.
//
// This is a different execution model from package orchestrate's
// buffered path: orchestrate drives every host through
// remoteexec.Connection.Exec, which returns one Result only after a
// command finishes — there is no way to observe output progressively
// through that interface, and no way to feed it input the command
// hasn't already been given up front. This package instead drives each
// host directly: os/exec for a local target, golang.org/x/crypto/ssh for
// an SSH target (see ssh.go for why SSH is driven directly here instead
// of through go-remoteexec/transport). Both packages share the same
// host-resolution and env-layering rules (see the orchestrate.ResolveHosts/
// MergeEnv/WithBuiltins/RenderEnv calls below) — only "how the command is
// actually driven" differs.
//
// Scope: local and SSH targets only. A WinRM-configured hosts_group is a
// clear error from Runner.Run (see the WinRM check there), not a silent
// best-effort attempt — this project's convention is to say plainly what
// is supported and what is deferred rather than half-work something.
package interactive

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/configuration-management-tool/cmt/connect"
	"github.com/configuration-management-tool/cmt/manifest"
	"github.com/configuration-management-tool/cmt/orchestrate"
)

// Options configures one Runner.Run invocation.
type Options struct {
	// EnvOverrides are applied last (highest priority), e.g. from the
	// CLI's -e/--env flags — same meaning as orchestrate.Options.
	EnvOverrides map[string]string

	// Only, when set, keeps only resolved hosts whose name matches.
	Only *regexp.Regexp
	// Except, when set, drops resolved hosts whose name matches.
	Except *regexp.Regexp

	// DisablePrefix turns off the "[host] " output line prefix.
	DisablePrefix bool

	// Stdin is read progressively and broadcast live to every host's
	// session. Required (a nil Stdin behaves as already-at-EOF: every
	// host's remote stdin is closed immediately, so only commands that
	// need no input at all make sense with it).
	Stdin io.Reader

	// Stdout and Stderr receive prefixed, live per-host output, plus
	// the final summary (see Summary). Default to io.Discard if nil.
	Stdout, Stderr io.Writer
}

// HostOutcome is how one host's interactive session ended.
type HostOutcome struct {
	Host string

	// ExitCode is the remote/local process's exit code. Meaningful only
	// when StillRunning is false and Err is nil.
	ExitCode int

	// StillRunning is true when the session was force-closed (SIGINT,
	// i.e. the Run context was canceled) before the process exited on
	// its own — there is no real exit code to report in that case.
	//
	// Local stdin reaching EOF does *not* by itself set this: closing a
	// host's remote stdin only signals EOF downstream (see the package
	// doc comment) and Run keeps waiting for that host's process to
	// actually exit in response, exactly like a real terminal
	// multiplexer would. StillRunning therefore only ever comes from an
	// interrupted Run.
	StillRunning bool

	// Err holds a dial error or a non-exit transport failure. nil even
	// when ExitCode is non-zero (that is a normal command failure, not
	// a cmt-level error).
	Err error
}

// Summary is every host's HostOutcome from one Runner.Run call.
type Summary struct {
	Results []HostOutcome
}

// Failed reports whether any host in the summary should be treated as a
// failure: a dial/transport error, a still-running (interrupted) host,
// or a non-zero exit code.
func (s Summary) Failed() bool {
	for _, r := range s.Results {
		if r.Err != nil || r.StillRunning || r.ExitCode != 0 {
			return true
		}
	}
	return false
}

// writeSummary prints one line per host, hosts sorted for determinism.
func writeSummary(w io.Writer, results []HostOutcome) {
	sorted := append([]HostOutcome(nil), results...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Host < sorted[j].Host })

	fmt.Fprintln(w, "interactive session summary:")
	for _, r := range sorted {
		switch {
		case r.Err != nil:
			fmt.Fprintf(w, "  %s: error: %v\n", r.Host, r.Err)
		case r.StillRunning:
			fmt.Fprintf(w, "  %s: still running (session closed)\n", r.Host)
		default:
			fmt.Fprintf(w, "  %s: exit code %d\n", r.Host, r.ExitCode)
		}
	}
}

// Runner runs one interactive command against a resolved hosts_group.
type Runner struct {
	Manifest *manifest.Manifest

	// Inventory resolves a hosts_group's dynamic `inventory` command.
	// Defaults to orchestrate.RunLocalInventory if nil, exactly like
	// orchestrate.Runner.Inventory.
	Inventory orchestrate.InventoryFunc
}

// hostSession is a live, driven session on one host — either a local
// os/exec process (local.go) or an SSH session (ssh.go). Both
// constructors return one of these so the rest of this file need not
// care which.
type hostSession struct {
	host   string
	stdin  io.WriteCloser
	stdout io.Reader
	stderr io.Reader

	// wait blocks until the process exits (or the session is force
	// closed via context cancellation) and reports the outcome.
	wait func() (exitCode int, stillRunning bool, err error)

	// close releases every resource this session holds (process,
	// pipes, SSH session/client). Safe to call once the session is no
	// longer needed; also invoked from a context-cancellation watcher
	// to force a hung wait() to return.
	close func() error
}

// Run resolves group's hosts (exactly as orchestrate.Runner.Run does —
// see orchestrate.ResolveHosts), opens a live session to each
// concurrently, forwards ctx's cancellation and opts.Stdin to all of
// them, streams every host's output live to opts.Stdout/Stderr, and
// returns a Summary once every host's session has ended (process exit,
// or a canceled ctx force-closing whatever is still running).
//
// It writes the summary to opts.Stdout and returns a non-nil error when
// Summary.Failed() — mirroring how orchestrate.Runner.Run reports a
// multi-host failure as an error to its caller.
func (r *Runner) Run(ctx context.Context, groupName, cmdName string, opts Options) (Summary, error) {
	if r.Manifest == nil {
		return Summary{}, fmt.Errorf("interactive: Runner.Manifest is required")
	}
	group, ok := r.Manifest.HostsGroups[groupName]
	if !ok {
		return Summary{}, fmt.Errorf("interactive: unknown hosts_group %q", groupName)
	}
	cmd, ok := r.Manifest.Commands[cmdName]
	if !ok {
		return Summary{}, fmt.Errorf("interactive: unknown command %q", cmdName)
	}
	if !cmd.Interactive {
		return Summary{}, fmt.Errorf("interactive: command %q is not marked interactive = true", cmdName)
	}
	if group.WinRM != nil {
		return Summary{}, fmt.Errorf("interactive: interactive mode is not supported for winrm targets (hosts_group %q)", groupName)
	}

	inv := r.Inventory
	if inv == nil {
		inv = orchestrate.RunLocalInventory
	}
	hosts, err := orchestrate.ResolveHosts(ctx, group, inv, opts.Only, opts.Except)
	if err != nil {
		return Summary{}, err
	}
	if len(hosts) == 0 {
		return Summary{}, fmt.Errorf("interactive: hosts_group %q resolved to no hosts", groupName)
	}

	stdout := orchestrate.SyncWriterOrDiscard(opts.Stdout)
	stderr := orchestrate.SyncWriterOrDiscard(opts.Stderr)
	stdin := opts.Stdin
	if stdin == nil {
		stdin = strings.NewReader("")
	}

	env := orchestrate.MergeEnv(r.Manifest.Env, group.Env, opts.EnvOverrides)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	sessions, err := dialAll(ctx, groupName, hosts, group, cmd, env)
	if err != nil {
		return Summary{}, err
	}

	summary := driveAll(ctx, sessions, stdin, stdout, stderr, opts.DisablePrefix)
	writeSummary(stdout, summary.Results)

	if summary.Failed() {
		return summary, fmt.Errorf("interactive: session failed on one or more hosts")
	}
	return summary, nil
}

// dialAll opens a live session to every host concurrently. If any host
// fails to dial, every session that did succeed is closed and a single
// aggregate error is returned — an interactive session either starts
// live on every resolved host or not at all, no silent partial start.
func dialAll(ctx context.Context, groupName string, hosts []string, group manifest.HostsGroup, cmd manifest.Command, env map[string]string) ([]*hostSession, error) {
	sessions := make([]*hostSession, len(hosts))
	dialErrs := make([]error, len(hosts))

	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Add(1)
		go func(i int, h string) {
			defer wg.Done()
			sess, err := dialOne(ctx, groupName, h, group, cmd, env)
			if err != nil {
				dialErrs[i] = fmt.Errorf("%s: %w", h, err)
				return
			}
			sessions[i] = sess
		}(i, h)
	}
	wg.Wait()

	var failed []string
	for _, e := range dialErrs {
		if e != nil {
			failed = append(failed, e.Error())
		}
	}
	if len(failed) > 0 {
		for _, s := range sessions {
			if s != nil {
				s.close()
			}
		}
		return nil, fmt.Errorf("interactive: dialing hosts: %s", strings.Join(failed, "; "))
	}
	return sessions, nil
}

// isLocalHost is connect.IsLocal behind a variable purely so tests can
// force dialOne's local-vs-SSH routing decision independent of what
// loopback addresses actually happen to be bindable in the test
// environment: this package's in-process SSH test server can only bind
// 127.0.0.1 (macOS, unlike Linux, does not auto-bind the rest of
// 127.0.0.0/8 without an explicit interface alias), and connect.IsLocal
// treats "127.0.0.1" as local by cmt's own documented, deliberate
// convention (see its doc comment) — so a test exercising "Runner.Run
// really drives this host over SSH, live, end to end" needs to override
// this, the same test-seam pattern package connect itself uses for
// dialSSHFunc/dialWinRMFunc.
var isLocalHost = connect.IsLocal

func dialOne(ctx context.Context, groupName, hostSpec string, group manifest.HostsGroup, cmd manifest.Command, env map[string]string) (*hostSession, error) {
	user, host, _, err := connect.ParseHost(hostSpec)
	if err != nil {
		return nil, err
	}
	full := orchestrate.RenderEnv(orchestrate.WithBuiltins(env, groupName, hostSpec, user)) + cmd.Run

	if isLocalHost(host) {
		return dialLocalSession(ctx, hostSpec, full)
	}
	return dialSSHSession(ctx, hostSpec, group, full)
}

// driveAll drives every session concurrently to completion (process
// exit, or the context being canceled) and returns the collected
// Summary. It also broadcasts stdin (see broadcastStdin) to every
// session live as it's read.
func driveAll(ctx context.Context, sessions []*hostSession, stdin io.Reader, stdout, stderr io.Writer, disablePrefix bool) Summary {
	stdinChans := make([]chan []byte, len(sessions))
	doneChans := make([]chan struct{}, len(sessions))
	for i := range sessions {
		stdinChans[i] = make(chan []byte, 16)
		doneChans[i] = make(chan struct{})
	}

	results := make(chan HostOutcome, len(sessions))
	var wg sync.WaitGroup
	for i, sess := range sessions {
		wg.Add(1)
		go func(i int, sess *hostSession) {
			defer wg.Done()
			driveHost(sess, stdinChans[i], doneChans[i], results,
				orchestrate.NewPrefixWriter(stdout, sess.host, disablePrefix),
				orchestrate.NewPrefixWriter(stderr, sess.host, disablePrefix))
		}(i, sess)
	}

	// The stdin-broadcasting goroutine is deliberately not joined: a
	// real terminal's os.Stdin.Read blocks until the next keystroke or
	// EOF, with no portable way to cancel it from here. Once every host
	// is done (this function's wg.Wait, below), the CLI returns and the
	// process exits right behind it, so an outstanding blocked read is
	// harmless in real use. Tests use a closable/cancelable Stdin (an
	// io.Pipe, or a context that's already canceled) to verify the
	// clean-shutdown paths directly instead of relying on this.
	go broadcastStdin(ctx, stdin, stdinChans, doneChans)

	wg.Wait()
	close(results)

	var summary Summary
	for r := range results {
		summary.Results = append(summary.Results, r)
	}
	return summary
}

// driveHost runs one host's session to completion: pumps stdout/stderr
// live to their prefixed writers, pumps broadcast stdin chunks to the
// session's stdin, waits for the process to exit (or the session to be
// force-closed), and reports the outcome on results.
func driveHost(sess *hostSession, stdinCh <-chan []byte, done chan<- struct{}, results chan<- HostOutcome, stdout, stderr *orchestrate.PrefixWriter) {
	var pumpWG sync.WaitGroup
	pumpWG.Add(2)
	go func() {
		defer pumpWG.Done()
		io.Copy(stdout, sess.stdout)
		stdout.Flush()
	}()
	go func() {
		defer pumpWG.Done()
		io.Copy(stderr, sess.stderr)
		stderr.Flush()
	}()

	// Both the stdin-forwarder goroutine below (on the stdinCh-closed
	// path) and the main flow after sess.wait() (a few lines down) may
	// try to close sess.stdin — a real, observed race under
	// golang.org/x/crypto/ssh's sessionStdin.Close (it is not safe to
	// call concurrently with itself), and not guaranteed idempotent for
	// local os/exec pipes either. closeStdinOnce makes it just that:
	// once, however many of the two call it.
	var stdinCloseOnce sync.Once
	closeStdin := func() { stdinCloseOnce.Do(func() { sess.stdin.Close() }) }

	// Pumps stdinCh -> sess.stdin. Not joined before reporting the
	// outcome (see driveAll's comment on why the top-level stdin reader
	// isn't joined either): once the host is done, close(done) below
	// stops the broadcaster from sending this session any *new* data,
	// but this goroutine may still be sitting idle on an empty,
	// not-yet-closed channel — harmless, and it unblocks the moment the
	// broadcaster does close it (global stdin EOF) or the process was
	// killed out from under sess.stdin.Write.
	go func() {
		for chunk := range stdinCh {
			if _, err := sess.stdin.Write(chunk); err != nil {
				return
			}
		}
		closeStdin()
	}()

	exitCode, stillRunning, err := sess.wait()
	close(done) // tell the broadcaster to stop targeting this host
	closeStdin()
	pumpWG.Wait() // stdout/stderr fully drained before reporting
	sess.close()

	results <- HostOutcome{Host: sess.host, ExitCode: exitCode, StillRunning: stillRunning, Err: err}
}

// broadcastStdin reads stdin progressively and fans each chunk read out
// to every still-live session's channel, skipping (not blocking on) a
// channel whose done has already fired. It closes every stdinCh at
// EOF/read-error, signaling each per-host forwarder to close that
// session's remote stdin in turn (see driveHost) — or returns without
// closing them if ctx is canceled first (the sessions are being torn
// down anyway; see the ssh.go/local.go context watchers).
func broadcastStdin(ctx context.Context, stdin io.Reader, stdinChans []chan []byte, doneChans []chan struct{}) {
	buf := make([]byte, 4096)
	for {
		n, err := stdin.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			for i := range stdinChans {
				select {
				case <-doneChans[i]:
					continue
				default:
				}
				select {
				case stdinChans[i] <- chunk:
				case <-doneChans[i]:
				case <-ctx.Done():
					return
				}
			}
		}
		if err != nil {
			for _, ch := range stdinChans {
				close(ch)
			}
			return
		}
		if ctx.Err() != nil {
			return
		}
	}
}
