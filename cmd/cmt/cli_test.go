// SPDX-License-Identifier: BSD-3-Clause
//
// Copyright (c) 2026, the configuration-management-tool/cmt authors

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	remoteexec "github.com/go-remoteexec/transport"

	"github.com/configuration-management-tool/cmt/connect"
	"github.com/configuration-management-tool/cmt/manifest"
	"github.com/configuration-management-tool/cmt/orchestrate"
)

// fakeConn is a minimal remoteexec.Connection double for CLI-level tests
// that need a deterministic, non-network Dial.
type fakeConn struct {
	rc int
}

func (c fakeConn) Exec(_ context.Context, cmd string, stdin io.Reader) (remoteexec.Result, error) {
	var got string
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		got = string(b)
	}
	return remoteexec.Result{RC: c.rc, Stdout: "ran:" + cmd + "|stdin:" + got + "\n"}, nil
}
func (fakeConn) Put(context.Context, string, string, remoteexec.PutOptions) error { return nil }
func (fakeConn) Fetch(context.Context, string, string) error                      { return nil }
func (fakeConn) Remove(context.Context, string) error                             { return nil }
func (fakeConn) TempPath(base string) string                                      { return "/tmp/" + base }
func (fakeConn) Close() error                                                     { return nil }

func fakeDial(rc int) orchestrate.DialFunc {
	return func(context.Context, string, manifest.HostsGroup) (remoteexec.Connection, error) {
		return fakeConn{rc: rc}, nil
	}
}

func writeManifest(t *testing.T, dir, contents string) string {
	t.Helper()
	path := filepath.Join(dir, "cmt.hcl")
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const basicManifest = `
hosts_group "g" {
  hosts = ["h1", "h2"]
}
command "run1" {
  run = "do-it"
}
`

func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"--version"}, &stdout, &stderr, strings.NewReader(""), nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "cmt "+version) {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestRunHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"-h"}, &stdout, &stderr, strings.NewReader(""), nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stderr.String(), "HOSTS_GROUP") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestRunUnknownFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"--nope-such-flag"}, &stdout, &stderr, strings.NewReader(""), nil)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

func TestRunMissingPositionalArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"onlyonearg"}, &stdout, &stderr, strings.NewReader(""), nil)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "HOSTS_GROUP") {
		t.Errorf("expected usage text on stderr, got %q", stderr.String())
	}
}

func TestRunBadEnvFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"-e", "NOEQUALSSIGN", "g", "run1"}, &stdout, &stderr, strings.NewReader(""), nil)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "invalid -e/--env value") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestRunBadOnlyRegex(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"--only", "(", "g", "run1"}, &stdout, &stderr, strings.NewReader(""), nil)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "--only") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestRunBadExceptRegex(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"--except", "(", "g", "run1"}, &stdout, &stderr, strings.NewReader(""), nil)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "--except") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestRunManifestParseError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	dir := t.TempDir()
	code := run([]string{"-f", filepath.Join(dir, "does-not-exist.hcl"), "g", "run1"}, &stdout, &stderr, strings.NewReader(""), nil)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "cmt:") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

// TestRunUnknownCommandName covers run()'s own ExpandCommands call (added
// so an interactive=true command can be detected before the buffered
// path runs — see detectInteractiveCommand) failing for a name that
// matches neither a command nor a target.
func TestRunUnknownCommandName(t *testing.T) {
	dir := t.TempDir()
	path := writeManifest(t, dir, basicManifest)

	var stdout, stderr bytes.Buffer
	code := run([]string{"-f", path, "g", "nope"}, &stdout, &stderr, strings.NewReader(""), fakeDial(0))
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "not a known command or target") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestRunSuccess(t *testing.T) {
	dir := t.TempDir()
	path := writeManifest(t, dir, basicManifest)

	var stdout, stderr bytes.Buffer
	code := run([]string{"-f", path, "g", "run1"}, &stdout, &stderr, strings.NewReader(""), fakeDial(0))
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "[h1] ran:") || !strings.Contains(stdout.String(), "[h2] ran:") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestRunFailure(t *testing.T) {
	dir := t.TempDir()
	path := writeManifest(t, dir, basicManifest)

	var stdout, stderr bytes.Buffer
	code := run([]string{"-f", path, "g", "run1"}, &stdout, &stderr, strings.NewReader(""), fakeDial(1))
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "cmt:") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestRunDebugFlag(t *testing.T) {
	dir := t.TempDir()
	path := writeManifest(t, dir, basicManifest)

	var stdout, stderr bytes.Buffer
	code := run([]string{"-D", "-f", path, "g", "run1"}, &stdout, &stderr, strings.NewReader(""), fakeDial(0))
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "[debug] host=h1") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestRunDisablePrefixFlag(t *testing.T) {
	dir := t.TempDir()
	path := writeManifest(t, dir, basicManifest)

	var stdout, stderr bytes.Buffer
	code := run([]string{"--disable-prefix", "-f", path, "g", "run1"}, &stdout, &stderr, strings.NewReader(""), fakeDial(0))
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "[h1]") {
		t.Errorf("expected no prefix, got %q", stdout.String())
	}
}

func TestRunEnvOverrideFlag(t *testing.T) {
	dir := t.TempDir()
	path := writeManifest(t, dir, `
hosts_group "g" { hosts = ["h1"] }
command "run1" { run = "do-it" }
`)
	var stdout, stderr bytes.Buffer
	code := run([]string{"-e", "FOO=bar", "--env", "BAZ=qux", "-f", path, "g", "run1"}, &stdout, &stderr, strings.NewReader(""), fakeDial(0))
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "FOO='bar'") || !strings.Contains(stdout.String(), "BAZ='qux'") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestRunOnlyExceptFlags(t *testing.T) {
	dir := t.TempDir()
	path := writeManifest(t, dir, basicManifest)

	var stdout, stderr bytes.Buffer
	code := run([]string{"--only", "^h1$", "-f", path, "g", "run1"}, &stdout, &stderr, strings.NewReader(""), fakeDial(0))
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "[h2]") {
		t.Errorf("--only should have excluded h2: %q", stdout.String())
	}
}

func TestRunPipedStdin(t *testing.T) {
	dir := t.TempDir()
	path := writeManifest(t, dir, `
hosts_group "g" { hosts = ["h1"] }
command "run1" { run = "do-it" }
`)
	var stdout, stderr bytes.Buffer
	code := run([]string{"-f", path, "g", "run1"}, &stdout, &stderr, strings.NewReader("piped"), fakeDial(0))
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "stdin:piped") {
		t.Errorf("expected piped stdin to reach Exec: %q", stdout.String())
	}
}

func TestRunTargetExpansion(t *testing.T) {
	dir := t.TempDir()
	path := writeManifest(t, dir, `
hosts_group "g" { hosts = ["h1"] }
command "run1" { run = "step1" }
command "run2" { run = "step2" }
target "deploy" { commands = ["run1", "run2"] }
`)
	var stdout, stderr bytes.Buffer
	code := run([]string{"-f", path, "g", "deploy"}, &stdout, &stderr, strings.NewReader(""), fakeDial(0))
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "step1") || !strings.Contains(stdout.String(), "step2") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

// TestRunEndToEndLocal is cmd/cmt's required real, non-mocked end-to-end
// test: a manifest with a `local` command run through the actual CLI
// entrypoint using the real connect.Dial (which resolves to
// transport.NewLocal() — no network).
func TestRunEndToEndLocal(t *testing.T) {
	dir := t.TempDir()
	path := writeManifest(t, dir, `
hosts_group "g" { hosts = ["localhost"] }
command "greet" {
  local = "echo hello-from-cmt"
}
`)
	var stdout, stderr bytes.Buffer
	code := run([]string{"-f", path, "g", "greet"}, &stdout, &stderr, strings.NewReader(""), connect.Dial)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "hello-from-cmt") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

// syncBuf is a concurrency-safe bytes.Buffer wrapper: an interactive
// command's fanned-out hosts write to Stdout/Stderr concurrently, so a
// test reading it live (from a different goroutine, while run() is
// still executing) needs a writer/reader safe for that.
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

func waitForSubstring(t *testing.T, got func() string, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(got(), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q in %q", want, got())
}

// TestRunInteractiveEndToEndLocal is cmd/cmt's required real,
// non-mocked end-to-end test for the interactive=true feature: a
// manifest command with interactive=true, run through the actual CLI
// entrypoint (real os/exec underneath, via package interactive, over
// the local transport — no network, no fake Dial, matching
// TestRunEndToEndLocal's own local-only convention). A controlled
// io.Pipe stands in for a real terminal's stdin; the test writes to it
// and asserts the echoed reply shows up live (before the process has
// exited), then closes it (EOF) and waits for the CLI call to return
// with a clean exit code.
func TestRunInteractiveEndToEndLocal(t *testing.T) {
	dir := t.TempDir()
	path := writeManifest(t, dir, `
hosts_group "g" { hosts = ["localhost"] }
command "shell" {
  desc        = "Interactive shell on all hosts"
  run         = "sh -c 'while IFS= read -r line; do echo \"echo:$line\"; done'"
  interactive = true
}
`)
	stdinR, stdinW := io.Pipe()
	stdout := &syncBuf{}
	var stderr bytes.Buffer

	done := make(chan int, 1)
	go func() {
		done <- run([]string{"-f", path, "g", "shell"}, stdout, &stderr, stdinR, connect.Dial)
	}()

	if _, err := stdinW.Write([]byte("hello-interactive\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	waitForSubstring(t, stdout.String, "[localhost] echo:hello-interactive")
	stdinW.Close() // EOF -> the remote `read` loop exits -> the session ends

	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit code = %d, stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("run() did not return; stdout so far = %q", stdout.String())
	}

	if !strings.Contains(stdout.String(), "interactive session summary:") {
		t.Errorf("stdout missing summary: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "localhost: exit code 0") {
		t.Errorf("stdout missing per-host summary line: %q", stdout.String())
	}
}

// TestRunInteractiveCombinedWithOtherCommands covers
// detectInteractiveCommand's validation: an interactive command named
// alongside any other command on the same invocation is rejected before
// anything runs.
func TestRunInteractiveCombinedWithOtherCommands(t *testing.T) {
	dir := t.TempDir()
	path := writeManifest(t, dir, `
hosts_group "g" { hosts = ["localhost"] }
command "shell" {
  run         = "bash"
  interactive = true
}
command "other" { run = "echo hi" }
`)
	var stdout, stderr bytes.Buffer
	code := run([]string{"-f", path, "g", "shell", "other"}, &stdout, &stderr, strings.NewReader(""), connect.Dial)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "must be invoked alone") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

// TestRunInteractiveFailure covers runInteractive's error path (a
// failure surfaced from interactive.Runner.Run) mapping to exit code 1,
// via an unknown hosts_group.
func TestRunInteractiveFailure(t *testing.T) {
	dir := t.TempDir()
	path := writeManifest(t, dir, `
hosts_group "g" { hosts = ["localhost"] }
command "shell" {
  run         = "true"
  interactive = true
}
`)
	var stdout, stderr bytes.Buffer
	code := run([]string{"-f", path, "nope", "shell"}, &stdout, &stderr, strings.NewReader(""), connect.Dial)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "cmt:") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestEnvListToMap(t *testing.T) {
	e := envList{"A=1", "B=2=x"}
	m, err := e.toMap()
	if err != nil {
		t.Fatalf("toMap: %v", err)
	}
	if m["A"] != "1" || m["B"] != "2=x" {
		t.Errorf("toMap = %#v", m)
	}
	if got := e.String(); got != "A=1,B=2=x" {
		t.Errorf("String() = %q", got)
	}

	bad := envList{"NOEQUALS"}
	if _, err := bad.toMap(); err == nil {
		t.Fatal("expected an error for a value without '='")
	}
}

func TestReadPipedStdin(t *testing.T) {
	if got := readPipedStdin(strings.NewReader("hello"), false); string(got) != "hello" {
		t.Errorf("readPipedStdin = %q", got)
	}
	if got := readPipedStdin(strings.NewReader("hello"), true); got != nil {
		t.Errorf("readPipedStdin with isTTY=true should return nil, got %q", got)
	}
	if got := readPipedStdin(strings.NewReader(""), false); got != nil {
		t.Errorf("readPipedStdin with empty input should return nil, got %q", got)
	}
	if got := readPipedStdin(errReader{}, false); got != nil {
		t.Errorf("readPipedStdin with a read error should return nil, got %q", got)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

func TestIsTerminal(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "cmt-isterminal")
	if err != nil {
		t.Fatal(err)
	}
	if isTerminal(f) {
		t.Errorf("a regular file should not be reported as a terminal")
	}
	f.Close()
	if isTerminal(f) {
		t.Errorf("a closed file (Stat error) should be treated as not-a-terminal")
	}
}

func TestRunUsesRealFileStdin(t *testing.T) {
	// Exercises run()'s "stdin.(*os.File)" type-assertion branch with a
	// genuine *os.File (a regular file, so isTerminal is false and its
	// content is still read as piped input).
	dir := t.TempDir()
	stdinPath := filepath.Join(dir, "stdin.txt")
	if err := os.WriteFile(stdinPath, []byte("file-stdin"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(stdinPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	path := writeManifest(t, dir, `
hosts_group "g" { hosts = ["h1"] }
command "run1" { run = "do-it" }
`)
	var stdout, stderr bytes.Buffer
	code := run([]string{"-f", path, "g", "run1"}, &stdout, &stderr, f, fakeDial(0))
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "stdin:file-stdin") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestMain_smoke(t *testing.T) {
	// main() itself is glue (os.Exit(run(...))); this just documents
	// that fmt/version formatting used there is sane, without actually
	// invoking os.Exit from within the test binary.
	if !strings.HasPrefix(fmt.Sprintf("cmt %s", version), "cmt 0.") {
		t.Errorf("unexpected version format: %s", version)
	}
}
