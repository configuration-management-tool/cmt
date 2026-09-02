// SPDX-License-Identifier: BSD-3-Clause
//
// Copyright (c) 2026, the configuration-management-tool/cmt authors

package interactive

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// waitForSubstring polls got for want to appear, failing t after a
// bounded timeout — used instead of a fixed sleep so these tests aren't
// flaky under load while still failing fast when something's actually
// wrong.
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

// TestLocalSessionEchoesStdinProgressively drives dialLocalSession
// directly against a real `sh` process (a tiny "read a line, echo it
// back" loop) fed by an io.Pipe under the test's own control, and
// asserts the echoed output shows up before the process exits — i.e.
// that output really streams live rather than only appearing once
// Wait() returns.
func TestLocalSessionEchoesStdinProgressively(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sess, err := dialLocalSession(ctx, "localhost", `while IFS= read -r line; do echo "got:$line"; done`)
	if err != nil {
		t.Fatalf("dialLocalSession: %v", err)
	}

	var out bytes.Buffer
	var mu sync.Mutex
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := sess.stdout.Read(buf)
			if n > 0 {
				mu.Lock()
				out.Write(buf[:n])
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	if _, err := sess.stdin.Write([]byte("hello\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	waitForSubstring(t, func() string { mu.Lock(); defer mu.Unlock(); return out.String() }, "got:hello")

	if _, err := sess.stdin.Write([]byte("world\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	waitForSubstring(t, func() string { mu.Lock(); defer mu.Unlock(); return out.String() }, "got:world")

	// Closing stdin (EOF) makes the read loop's `read -r` fail and the
	// shell exit on its own.
	sess.stdin.Close()

	exitCode, stillRunning, err := sess.wait()
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if stillRunning {
		t.Error("stillRunning = true, want false (process exited on its own)")
	}
	if exitCode != 0 {
		t.Errorf("exitCode = %d, want 0", exitCode)
	}
	sess.close()
}

// TestLocalSessionProcessExitsWithoutReadingStdin covers a process that
// exits on its own without ever consuming stdin — the session must still
// end cleanly and report the real exit code, without requiring the
// caller to have sent (or closed) any stdin first.
func TestLocalSessionProcessExitsWithoutReadingStdin(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sess, err := dialLocalSession(ctx, "localhost", "exit 3")
	if err != nil {
		t.Fatalf("dialLocalSession: %v", err)
	}
	defer sess.close()

	exitCode, stillRunning, err := sess.wait()
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if stillRunning {
		t.Error("stillRunning = true, want false")
	}
	if exitCode != 3 {
		t.Errorf("exitCode = %d, want 3", exitCode)
	}
}

// TestLocalSessionContextCancelKillsProcess covers the SIGINT path: a
// canceled context should kill a still-running local process promptly,
// with wait() reporting stillRunning instead of hanging.
func TestLocalSessionContextCancelKillsProcess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	sess, err := dialLocalSession(ctx, "localhost", "sleep 30")
	if err != nil {
		t.Fatalf("dialLocalSession: %v", err)
	}
	defer sess.close()

	cancel()

	done := make(chan struct{})
	var exitCode int
	var stillRunning bool
	go func() {
		exitCode, stillRunning, err = sess.wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("wait() did not return after context cancellation")
	}
	if !stillRunning {
		t.Errorf("stillRunning = false, want true (process was killed by ctx cancellation)")
	}
	if err != nil {
		t.Errorf("err = %v, want nil for an intentional cancellation", err)
	}
	_ = exitCode
}

// TestLocalSessionUnknownCommandExitsNonZero confirms an sh-syntax-valid
// but nonexistent command is an ordinary non-zero exit, not a start
// error (sh itself starts fine and merely reports "command not found").
func TestLocalSessionUnknownCommandExitsNonZero(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess, err := dialLocalSession(ctx, "localhost", "this-is-not-a-real-command-xyz")
	if err != nil {
		t.Fatalf("dialLocalSession: %v", err)
	}
	defer sess.close()
	exitCode, stillRunning, waitErr := sess.wait()
	if waitErr != nil {
		t.Fatalf("wait: %v", waitErr)
	}
	if stillRunning {
		t.Error("stillRunning = true, want false")
	}
	if exitCode == 0 {
		t.Error("exitCode = 0, want non-zero for an unknown command")
	}
}

// TestLocalSessionStartError covers dialLocalSession's cmd.Start()
// error path itself: with PATH emptied, exec.Cmd can't resolve "sh" at
// all, so Start (not just the eventual exit code) fails.
func TestLocalSessionStartError(t *testing.T) {
	t.Setenv("PATH", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := dialLocalSession(ctx, "localhost", "true"); err == nil {
		t.Fatal("expected an error with PATH emptied (sh cannot be resolved)")
	}
}
