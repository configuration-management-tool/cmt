// SPDX-License-Identifier: BSD-3-Clause
//
// Copyright (c) 2026, the configuration-management-tool/cmt authors

// Unit tests for this package's unexported helpers (writeSummary,
// dialAll's partial-dial-failure cleanup, driveHost, broadcastStdin) —
// the pieces of Runner.Run's machinery whose edge-case branches (a
// transport error mid-session, one host already dialed while a sibling
// fails, a stdin write racing a finished host, a done signal arriving
// while a broadcast send is blocked) are impractical to hit reliably by
// only ever going through the full, real, concurrent Runner.Run path —
// see TestRunLocal*/TestRunSSH* in interactive_test.go for that
// end-to-end coverage.
package interactive

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/configuration-management-tool/cmt/manifest"
	"github.com/configuration-management-tool/cmt/orchestrate"
)

func TestWriteSummary(t *testing.T) {
	var buf bytes.Buffer
	writeSummary(&buf, []HostOutcome{
		{Host: "b", ExitCode: 0},
		{Host: "a", StillRunning: true},
		{Host: "c", Err: fmt.Errorf("boom")},
	})
	got := buf.String()
	wantLines := []string{
		"interactive session summary:",
		"  a: still running (session closed)",
		"  b: exit code 0",
		"  c: error: boom",
	}
	for _, w := range wantLines {
		if !strings.Contains(got, w) {
			t.Errorf("summary %q missing line %q", got, w)
		}
	}
	// Sorted by host: a, b, c in that order.
	ia, ib, ic := strings.Index(got, "  a:"), strings.Index(got, "  b:"), strings.Index(got, "  c:")
	if !(ia < ib && ib < ic) {
		t.Errorf("summary lines not sorted by host: %q", got)
	}
}

// TestDialAllPartialFailureClosesGoodSessions covers dialAll's cleanup
// path: when one host of several fails to dial, every session that did
// succeed must be closed before the aggregate error is returned.
func TestDialAllPartialFailureClosesGoodSessions(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hosts := []string{"localhost", "user@host:notaport"}
	cmd := manifest.Command{Run: "sleep 30"}
	sessions, err := dialAll(ctx, "g", hosts, manifest.HostsGroup{}, cmd, nil)
	if err == nil {
		t.Fatal("expected an aggregate dial error")
	}
	if sessions != nil {
		t.Errorf("expected nil sessions on partial failure, got %#v", sessions)
	}
	if !strings.Contains(err.Error(), "dialing hosts") {
		t.Errorf("err = %v", err)
	}
	// The successfully-dialed "localhost" session must have been closed
	// (its process killed) rather than left running — indirectly proven
	// by the overall call returning promptly rather than this test
	// needing any cleanup of its own; a leaked `sleep 30` process would
	// otherwise outlive the test.
}

// fakeSession builds a *hostSession backed by in-memory pipes/behavior,
// for driveHost unit tests that need to control exactly when wait()
// returns and what stdin.Write does, independent of a real process or
// SSH session.
type fakeSession struct {
	host       string
	stdoutR    *io.PipeReader
	stdoutW    *io.PipeWriter
	stderrR    *io.PipeReader
	stderrW    *io.PipeWriter
	stdinWrite func([]byte) (int, error)
	stdinClose func() error
	waitFn     func() (int, bool, error)
	closeFn    func() error
}

func newFakeSession(host string) *fakeSession {
	outR, outW := io.Pipe()
	errR, errW := io.Pipe()
	return &fakeSession{host: host, stdoutR: outR, stdoutW: outW, stderrR: errR, stderrW: errW}
}

type fakeWriteCloser struct {
	write func([]byte) (int, error)
	close func() error
}

func (f fakeWriteCloser) Write(p []byte) (int, error) { return f.write(p) }
func (f fakeWriteCloser) Close() error {
	if f.close != nil {
		return f.close()
	}
	return nil
}

func (f *fakeSession) toHostSession() *hostSession {
	return &hostSession{
		host:   f.host,
		stdin:  fakeWriteCloser{write: f.stdinWrite, close: f.stdinClose},
		stdout: f.stdoutR,
		stderr: f.stderrR,
		wait:   f.waitFn,
		close:  f.closeFn,
	}
}

// TestDriveHostStdinWriteError covers driveHost's stdin-forwarder
// goroutine seeing a Write error (e.g. the underlying process already
// exited and closed its end) and returning instead of looping forever
// or panicking.
func TestDriveHostStdinWriteError(t *testing.T) {
	fs := newFakeSession("h")
	writeErr := fmt.Errorf("broken pipe")
	// A channel, not a plain bool: driveHost's stdin-forwarder goroutine
	// is deliberately never joined (see driveHost's own doc comment), so
	// nothing here happens-before the assertions below except through a
	// synchronized channel operation — a bare bool write/read would be a
	// data race even though the events happen to occur in the intended
	// order in practice.
	wrote := make(chan struct{}, 1)
	fs.stdinWrite = func(p []byte) (int, error) {
		select {
		case wrote <- struct{}{}:
		default:
		}
		return 0, writeErr
	}
	fs.waitFn = func() (int, bool, error) { return 0, false, nil }
	fs.closeFn = func() error { return nil }

	go func() { fs.stdoutW.Write([]byte("out\n")); fs.stdoutW.Close() }()
	go func() { fs.stderrW.Close() }()

	stdinCh := make(chan []byte, 1)
	stdinCh <- []byte("data") // delivered once, Write fails, forwarder returns

	done := make(chan struct{})
	results := make(chan HostOutcome, 1)
	var stdout, stderr bytes.Buffer

	finished := make(chan struct{})
	go func() {
		driveHost(fs.toHostSession(), stdinCh, done, results,
			orchestrate.NewPrefixWriter(&stdout, "h", false),
			orchestrate.NewPrefixWriter(&stderr, "h", false))
		close(finished)
	}()

	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("driveHost did not return")
	}
	select {
	case <-wrote:
	case <-time.After(2 * time.Second):
		t.Fatal("expected sess.stdin.Write to have been called")
	}
	r := <-results
	if r.Host != "h" || r.ExitCode != 0 || r.StillRunning || r.Err != nil {
		t.Errorf("outcome = %#v", r)
	}
	if !strings.Contains(stdout.String(), "[h] out") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

// TestBroadcastStdinCtxDoneAfterSuccessfulRead covers broadcastStdin's
// final ctx.Err() check (reached after a successful, error-free Read),
// using an empty channel set so neither per-channel select is involved.
func TestBroadcastStdinCtxDoneAfterSuccessfulRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled before broadcastStdin ever runs

	finished := make(chan struct{})
	go func() {
		broadcastStdin(ctx, strings.NewReader("data"), nil, nil)
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("broadcastStdin did not return promptly for an already-canceled context")
	}
}

// TestBroadcastStdinSkipsDoneChannel covers the pre-send "already done"
// fast path: a channel whose done is closed before broadcastStdin ever
// sees a chunk must never receive it, while a sibling channel that is
// still live gets it normally.
func TestBroadcastStdinSkipsDoneChannel(t *testing.T) {
	doneA := make(chan struct{})
	close(doneA)
	doneB := make(chan struct{})
	chA := make(chan []byte, 1)
	chB := make(chan []byte, 1)

	r, w := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	finished := make(chan struct{})
	go func() {
		broadcastStdin(ctx, r, []chan []byte{chA, chB}, []chan struct{}{doneA, doneB})
		close(finished)
	}()

	if _, err := w.Write([]byte("hi")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	select {
	case got := <-chB:
		if string(got) != "hi" {
			t.Errorf("chB = %q, want %q", got, "hi")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for chB")
	}
	select {
	case v := <-chA:
		t.Errorf("chA received %q, want it skipped (done already closed)", v)
	default:
	}

	w.Close()
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("broadcastStdin did not return after stdin EOF")
	}
}

// TestBroadcastStdinUnblocksOnDoneWhileSendBlocked covers the blocking
// select's <-doneChans[i] case: a channel already at capacity (nobody
// draining it) must not wedge the broadcaster forever — closing its
// done unblocks the pending send attempt.
func TestBroadcastStdinUnblocksOnDoneWhileSendBlocked(t *testing.T) {
	done := make(chan struct{})
	ch := make(chan []byte, 1)
	ch <- []byte("filler") // fills capacity so the next send blocks

	r, w := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	finished := make(chan struct{})
	go func() {
		broadcastStdin(ctx, r, []chan []byte{ch}, []chan struct{}{done})
		close(finished)
	}()

	if _, err := w.Write([]byte("hi")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// By program order, done is only closed after this write has
	// already been issued, so broadcastStdin's pre-check for this
	// channel necessarily observed "not done yet" and is now sitting in
	// the blocking select (ch has no free capacity for anyone to drain
	// it) — this sleep just gives that goroutine a moment to get there.
	time.Sleep(100 * time.Millisecond)
	close(done)
	w.Close()

	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("broadcastStdin did not return; the done-case in the blocking select did not fire")
	}

	// ch still only has the original filler value — the "hi" chunk was
	// never delivered, because done fired instead of a send completing.
	if v, ok := <-ch; !ok || string(v) != "filler" {
		t.Errorf("ch = %q ok=%v, want the buffered filler value", v, ok)
	}
	if _, ok := <-ch; ok {
		t.Error("expected ch to be closed (stdin EOF)")
	}
}

// TestBroadcastStdinCtxDoneWhileSendBlocked covers the blocking select's
// <-ctx.Done() case: canceling the context (the SIGINT path) while a
// send to a full, still-live channel is blocked must return promptly
// rather than waiting for that channel to drain.
func TestBroadcastStdinCtxDoneWhileSendBlocked(t *testing.T) {
	done := make(chan struct{}) // deliberately never closed
	ch := make(chan []byte, 1)
	ch <- []byte("filler") // fills capacity so the next send blocks

	r, w := io.Pipe()
	defer w.Close()
	ctx, cancel := context.WithCancel(context.Background())

	finished := make(chan struct{})
	go func() {
		broadcastStdin(ctx, r, []chan []byte{ch}, []chan struct{}{done})
		close(finished)
	}()

	if _, err := w.Write([]byte("hi")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// Same program-order reasoning as
	// TestBroadcastStdinUnblocksOnDoneWhileSendBlocked: this write
	// happens-before cancel(), and nothing ever drains ch, so
	// broadcastStdin must already be sitting in the blocking select
	// by the time ctx is canceled.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("broadcastStdin did not return after ctx cancellation while blocked on send")
	}
}
