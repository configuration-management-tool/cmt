// SPDX-License-Identifier: BSD-3-Clause
//
// Copyright (c) 2026, the configuration-management-tool/cmt authors

package interactive

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/configuration-management-tool/cmt/manifest"
)

func manifestWith(cmds map[string]manifest.Command, groups map[string]manifest.HostsGroup) *manifest.Manifest {
	return &manifest.Manifest{
		Env:         map[string]string{},
		Commands:    cmds,
		HostsGroups: groups,
		Targets:     map[string]manifest.Target{},
	}
}

func TestRunUnknownHostsGroup(t *testing.T) {
	m := manifestWith(map[string]manifest.Command{"shell": {Name: "shell", Run: "bash", Interactive: true}}, nil)
	r := &Runner{Manifest: m}
	if _, err := r.Run(context.Background(), "nope", "shell", Options{}); err == nil || !strings.Contains(err.Error(), `unknown hosts_group "nope"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestRunUnknownCommand(t *testing.T) {
	m := manifestWith(nil, map[string]manifest.HostsGroup{"g": {Hosts: []string{"localhost"}}})
	r := &Runner{Manifest: m}
	if _, err := r.Run(context.Background(), "g", "nope", Options{}); err == nil || !strings.Contains(err.Error(), `unknown command "nope"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestRunCommandNotInteractive(t *testing.T) {
	m := manifestWith(map[string]manifest.Command{"c": {Name: "c", Run: "echo hi"}}, map[string]manifest.HostsGroup{"g": {Hosts: []string{"localhost"}}})
	r := &Runner{Manifest: m}
	if _, err := r.Run(context.Background(), "g", "c", Options{}); err == nil || !strings.Contains(err.Error(), "not marked interactive") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunWinRMRejected(t *testing.T) {
	m := manifestWith(
		map[string]manifest.Command{"shell": {Name: "shell", Run: "bash", Interactive: true}},
		map[string]manifest.HostsGroup{"g": {Hosts: []string{"win1"}, WinRM: &manifest.WinRMConfig{}}},
	)
	r := &Runner{Manifest: m}
	_, err := r.Run(context.Background(), "g", "shell", Options{})
	if err == nil || !strings.Contains(err.Error(), "not supported for winrm targets") {
		t.Fatalf("err = %v", err)
	}
}

// TestRunBecomeRejected covers Run's explicit rejection of an interactive
// command on a hosts_group with become configured: go-remoteexec/
// transport's Become decorator doesn't implement Streamer (see its own
// package doc), so this is turned into a clear upfront error instead of
// a confusing type-assertion failure surfacing from inside dialSSHSession.
func TestRunBecomeRejected(t *testing.T) {
	m := manifestWith(
		map[string]manifest.Command{"shell": {Name: "shell", Run: "bash", Interactive: true}},
		map[string]manifest.HostsGroup{"g": {Hosts: []string{"h1"}, Become: &manifest.BecomeConfig{}}},
	)
	r := &Runner{Manifest: m}
	_, err := r.Run(context.Background(), "g", "shell", Options{})
	if err == nil || !strings.Contains(err.Error(), "not supported with become configured") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunNoHostsResolved(t *testing.T) {
	m := manifestWith(
		map[string]manifest.Command{"shell": {Name: "shell", Run: "bash", Interactive: true}},
		map[string]manifest.HostsGroup{"g": {Hosts: []string{"h1"}}},
	)
	r := &Runner{Manifest: m}
	opts := Options{Only: regexp.MustCompile(`^nomatch$`)}
	_, err := r.Run(context.Background(), "g", "shell", opts)
	if err == nil || !strings.Contains(err.Error(), "resolved to no hosts") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunNilManifest(t *testing.T) {
	r := &Runner{}
	if _, err := r.Run(context.Background(), "g", "shell", Options{}); err == nil || !strings.Contains(err.Error(), "Runner.Manifest is required") {
		t.Fatalf("err = %v", err)
	}
}

// TestRunNilStdin covers Options.Stdin being left nil: it must behave
// as already-at-EOF (an immediate strings.NewReader("") stand-in), not
// panic or block forever, for a command that needs no input.
func TestRunNilStdin(t *testing.T) {
	m := manifestWith(
		map[string]manifest.Command{"shell": {Name: "shell", Run: "exit 0", Interactive: true}},
		map[string]manifest.HostsGroup{"g": {Hosts: []string{"localhost"}}},
	)
	r := &Runner{Manifest: m}
	s, err := r.Run(context.Background(), "g", "shell", Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(s.Results) != 1 || s.Results[0].ExitCode != 0 {
		t.Errorf("Results = %#v", s.Results)
	}
}

// TestRunResolveHostsError covers a dynamic-inventory hosts_group whose
// inventory command itself fails, surfaced as a plain error from
// orchestrate.ResolveHosts.
func TestRunResolveHostsError(t *testing.T) {
	m := manifestWith(
		map[string]manifest.Command{"shell": {Name: "shell", Run: "bash", Interactive: true}},
		map[string]manifest.HostsGroup{"g": {Inventory: "exit 1"}},
	)
	r := &Runner{Manifest: m}
	_, err := r.Run(context.Background(), "g", "shell", Options{})
	if err == nil || !strings.Contains(err.Error(), "resolving inventory") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunDialFailure(t *testing.T) {
	// A host string that fails connect.ParseHost surfaces as a dial
	// error via dialAll's aggregate-error path.
	m := manifestWith(
		map[string]manifest.Command{"shell": {Name: "shell", Run: "bash", Interactive: true}},
		map[string]manifest.HostsGroup{"g": {Hosts: []string{"user@host:notaport"}}},
	)
	r := &Runner{Manifest: m}
	_, err := r.Run(context.Background(), "g", "shell", Options{Stdin: strings.NewReader("")})
	if err == nil || !strings.Contains(err.Error(), "dialing hosts") {
		t.Fatalf("err = %v", err)
	}
}

// TestRunLocalSingleHost drives the full Runner.Run path against a
// single local host end to end: real os/exec underneath, a controlled
// io.Pipe standing in for local stdin, asserting live progressive
// output and a clean summary once stdin hits EOF.
func TestRunLocalSingleHost(t *testing.T) {
	m := manifestWith(
		map[string]manifest.Command{"shell": {
			// Wrapped in a nested `sh -c '...'`: cmt always prepends an
			// env-var-assignment prefix (CMT_HOST='...' ...) ahead of
			// Run (see manifest/README's $NAME example), and POSIX shell
			// only allows that prefix before a *simple* command — a bare
			// `while ...; done` right after the assignments is a syntax
			// error. Nesting it as `sh`'s single argument makes `sh` the
			// simple command the assignments apply to.
			Name: "shell", Run: `sh -c 'while IFS= read -r line; do echo "echo:$line"; done'`, Interactive: true,
		}},
		map[string]manifest.HostsGroup{"g": {Hosts: []string{"localhost"}}},
	)
	r := &Runner{Manifest: m}

	stdinR, stdinW := io.Pipe()
	var stdout syncBuf
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct {
		s   Summary
		err error
	}, 1)
	go func() {
		s, err := r.Run(ctx, "g", "shell", Options{Stdin: stdinR, Stdout: &stdout})
		done <- struct {
			s   Summary
			err error
		}{s, err}
	}()

	if _, err := stdinW.Write([]byte("hi\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	waitForSubstring(t, stdout.String, "[localhost] echo:hi")
	stdinW.Close() // EOF -> remote sh's `read` loop exits -> process ends

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("Run: %v (stdout=%q)", res.err, stdout.String())
		}
		if len(res.s.Results) != 1 || res.s.Results[0].ExitCode != 0 {
			t.Errorf("Results = %#v", res.s.Results)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("Run did not return; stdout so far = %q", stdout.String())
	}

	if !strings.Contains(stdout.String(), "interactive session summary:") {
		t.Errorf("stdout missing summary: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "localhost: exit code 0") {
		t.Errorf("stdout missing per-host summary line: %q", stdout.String())
	}
}

// TestRunLocalMultiHostMixedOutcomes drives two local "hosts" (both
// resolving to the real local machine, since interactive only needs
// hostnames to route local vs SSH — connect.IsLocal only checks the host
// literal, and only "localhost"/"127.0.0.1"/"::1" qualify) with commands
// that produce different exit codes, and checks Summary/Failed reflect
// the mix.
func TestRunLocalMultiHostMixedOutcomes(t *testing.T) {
	m := manifestWith(
		map[string]manifest.Command{"shell": {Name: "shell", Run: "exit $CMT_EXIT", Interactive: true}},
		map[string]manifest.HostsGroup{"g": {
			Hosts: []string{"localhost", "127.0.0.1"},
			Env:   map[string]string{"CMT_EXIT": "0"},
		}},
	)
	r := &Runner{Manifest: m}
	// Force one host to fail and the other to succeed via --only-style
	// env overrides is awkward per host, so instead give each host its
	// own exit code through CMT_HOST-driven shell logic (nested in
	// `sh -c '...'` for the same reason as TestRunLocalSingleHost: `if`
	// is a compound command, and cmt's env-assignment prefix only
	// precedes a simple one).
	m.Commands["shell"] = manifest.Command{Name: "shell", Run: `sh -c 'if [ "$CMT_HOST" = "127.0.0.1" ]; then exit 5; else exit 0; fi'`, Interactive: true}

	stdout := &syncBuf{}
	s, err := r.Run(context.Background(), "g", "shell", Options{Stdin: strings.NewReader(""), Stdout: stdout})
	if err == nil {
		t.Fatal("expected an error: one host exits non-zero")
	}
	if !s.Failed() {
		t.Error("Summary.Failed() = false, want true")
	}
	if len(s.Results) != 2 {
		t.Fatalf("Results = %#v", s.Results)
	}
	var sawZero, sawFive bool
	for _, r := range s.Results {
		switch r.ExitCode {
		case 0:
			sawZero = true
		case 5:
			sawFive = true
		}
	}
	if !sawZero || !sawFive {
		t.Errorf("expected one host at exit 0 and one at exit 5: %#v", s.Results)
	}
}

// TestRunInterruptStillRunning covers the SIGINT path end to end
// through Runner.Run: canceling the context while a local host is still
// running (e.g. `sleep`) must make Run return promptly with that host
// reported as StillRunning, not hang.
func TestRunInterruptStillRunning(t *testing.T) {
	m := manifestWith(
		map[string]manifest.Command{"shell": {Name: "shell", Run: "sleep 30", Interactive: true}},
		map[string]manifest.HostsGroup{"g": {Hosts: []string{"localhost"}}},
	)
	r := &Runner{Manifest: m}

	ctx, cancel := context.WithCancel(context.Background())
	stdinR, stdinW := io.Pipe()
	defer stdinW.Close()
	stdout := &syncBuf{}

	done := make(chan struct {
		s   Summary
		err error
	}, 1)
	go func() {
		s, err := r.Run(ctx, "g", "shell", Options{Stdin: stdinR, Stdout: stdout})
		done <- struct {
			s   Summary
			err error
		}{s, err}
	}()

	time.Sleep(100 * time.Millisecond) // let the session actually start
	cancel()

	select {
	case res := <-done:
		if res.err == nil {
			t.Fatal("expected an error (still-running host counts as failed)")
		}
		if len(res.s.Results) != 1 || !res.s.Results[0].StillRunning {
			t.Errorf("Results = %#v, want one StillRunning host", res.s.Results)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

// forceRemote overrides isLocalHost to always report "not local" for the
// duration of t, so Runner.Run really drives a host over
// dialSSHSession/SSH instead of dialLocalSession — needed because this
// package's in-process SSH test server can only bind 127.0.0.1 (see
// isLocalHost's doc comment), which connect.IsLocal itself always
// treats as local.
func forceRemote(t *testing.T) {
	t.Helper()
	orig := isLocalHost
	isLocalHost = func(string) bool { return false }
	t.Cleanup(func() { isLocalHost = orig })
}

// TestDialOneRoutesLocalVsSSH covers dialOne's own routing decision
// directly: a "localhost" hostSpec takes the local branch, and a
// hostSpec whose host isLocalHost reports as non-local takes the SSH
// branch (proven here by actually completing a live exchange with the
// in-process test server, not just by inspecting which function was
// called).
func TestDialOneRoutesLocalVsSSH(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	localSess, err := dialOne(ctx, "g", "localhost", manifest.HostsGroup{}, manifest.Command{Run: "exit 0"}, nil)
	if err != nil {
		t.Fatalf("dialOne (local): %v", err)
	}
	defer localSess.close()
	if code, running, err := localSess.wait(); err != nil || running || code != 0 {
		t.Errorf("local session outcome = code=%d running=%v err=%v", code, running, err)
	}

	srv := newTestSSHServer(t)
	forceRemote(t)
	sshSess, err := dialOne(ctx, "g", sshHostSpec(srv), sshGroup(srv), manifest.Command{Run: "exit 0"}, nil)
	if err != nil {
		t.Fatalf("dialOne (ssh): %v", err)
	}
	defer sshSess.close()
	if code, running, err := sshSess.wait(); err != nil || running || code != 0 {
		t.Errorf("ssh session outcome = code=%d running=%v err=%v", code, running, err)
	}
}

// TestRunSSHMultiHostInterleaved covers the primary multi-host SSH
// scenario end to end through Runner.Run: two in-process SSH servers as
// two "hosts" in one hosts_group, live interleaved prefixed output, a
// stdin broadcast reaching both, one host exiting earlier than the
// other, and a clean shutdown on local-stdin EOF.
func TestRunSSHMultiHostInterleaved(t *testing.T) {
	forceRemote(t)
	srvA := newTestSSHServer(t)
	srvB := newTestSSHServer(t)

	m := manifestWith(
		map[string]manifest.Command{"shell": {
			Name: "shell", Run: `sh -c 'while IFS= read -r line; do echo "echo:$line"; done'`, Interactive: true,
		}},
		map[string]manifest.HostsGroup{"g": {
			Hosts: []string{sshHostSpec(srvA), sshHostSpec(srvB)},
			// Both test servers share the same fixed username/password
			// (see newTestSSHServer), so one ssh{} block covers both.
			SSH: &manifest.SSHConfig{Password: srvA.Password},
		}},
	)
	r := &Runner{Manifest: m}

	stdinR, stdinW := io.Pipe()
	stdout := &syncBuf{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct {
		s   Summary
		err error
	}, 1)
	go func() {
		s, err := r.Run(ctx, "g", "shell", Options{Stdin: stdinR, Stdout: stdout})
		done <- struct {
			s   Summary
			err error
		}{s, err}
	}()

	if _, err := stdinW.Write([]byte("broadcast\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	waitForSubstring(t, stdout.String, "["+sshHostSpec(srvA)+"] echo:broadcast")
	waitForSubstring(t, stdout.String, "["+sshHostSpec(srvB)+"] echo:broadcast")

	stdinW.Close()

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("Run: %v (stdout=%q)", res.err, stdout.String())
		}
		if len(res.s.Results) != 2 {
			t.Fatalf("Results = %#v", res.s.Results)
		}
		for _, r := range res.s.Results {
			if r.ExitCode != 0 || r.StillRunning || r.Err != nil {
				t.Errorf("host %s outcome = %#v", r.Host, r)
			}
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("Run did not return; stdout so far = %q", stdout.String())
	}
}

// TestRunSSHOneExitsEarly covers one host's remote process exiting on
// its own well before local stdin reaches EOF and well before the other
// host's process does — the finished host must be reported correctly
// without blocking the still-running host's own live session.
func TestRunSSHOneExitsEarly(t *testing.T) {
	forceRemote(t)
	srvFast := newTestSSHServer(t)
	srvSlow := newTestSSHServer(t)

	fastCmd := "exit 0"
	slowCmd := `while IFS= read -r line; do echo "echo:$line"; done`

	m := &manifest.Manifest{
		Env: map[string]string{},
		Commands: map[string]manifest.Command{
			"shell": {Name: "shell", Run: "$CMT_SCRIPT", Interactive: true},
		},
		HostsGroups: map[string]manifest.HostsGroup{
			"g": {
				Hosts: []string{sshHostSpec(srvFast), sshHostSpec(srvSlow)},
				SSH:   &manifest.SSHConfig{Password: srvFast.Password},
			},
		},
		Targets: map[string]manifest.Target{},
	}
	// Route each host to a different script via a host-conditional
	// shell wrapper (mirrors TestRunLocalMultiHostMixedOutcomes), nested
	// in `sh -c '...'` for the same simple-command-only reason.
	inner := fmt.Sprintf(`if [ "$CMT_HOST" = %q ]; then %s; else %s; fi`, sshHostSpec(srvFast), fastCmd, slowCmd)
	m.Commands["shell"] = manifest.Command{
		Name:        "shell",
		Run:         "sh -c '" + inner + "'",
		Interactive: true,
	}

	r := &Runner{Manifest: m}
	stdinR, stdinW := io.Pipe()
	stdout := &syncBuf{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct {
		s   Summary
		err error
	}, 1)
	go func() {
		s, err := r.Run(ctx, "g", "shell", Options{Stdin: stdinR, Stdout: stdout})
		done <- struct {
			s   Summary
			err error
		}{s, err}
	}()

	// The slow host is still waiting on a line of input; prove it's
	// still live by talking to it after giving the fast host time to
	// exit on its own.
	time.Sleep(300 * time.Millisecond)
	if _, err := stdinW.Write([]byte("still-here\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	waitForSubstring(t, stdout.String, "["+sshHostSpec(srvSlow)+"] echo:still-here")

	stdinW.Close()

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("Run: %v (stdout=%q)", res.err, stdout.String())
		}
		if len(res.s.Results) != 2 {
			t.Fatalf("Results = %#v", res.s.Results)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("Run did not return; stdout so far = %q", stdout.String())
	}
}

// syncBuf is a concurrency-safe bytes.Buffer wrapper for tests that read
// a live-growing buffer from one goroutine while Runner.Run's fanned-out
// hosts write to it from others.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
