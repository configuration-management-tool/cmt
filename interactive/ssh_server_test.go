// SPDX-License-Identifier: BSD-3-Clause
//
// Copyright (c) 2026, the configuration-management-tool/cmt authors

package interactive

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os/exec"
	"testing"

	"golang.org/x/crypto/ssh"
)

// testSSHServer is a minimal, in-process SSH server, listening on
// 127.0.0.1, used only to exercise dialSSHSession end to end against a
// real (if tiny) implementation of the wire protocol — not a mock of the
// ssh package's internals. It accepts a fixed username/password, and for
// every "exec" request runs the command line through /bin/sh -c on this
// test machine, wiring stdin/stdout/stderr straight to the SSH channel.
//
// This mirrors the pattern go-remoteexec/transport's own tests use for
// the same reason: testing against a real, local SSH server exercises
// the actual protocol path (auth, channel setup, exec requests,
// exit-status reporting) instead of asserting against invented
// expectations about how golang.org/x/crypto/ssh behaves internally.
// sshServerMode selects a testSSHServer's misbehavior, for tests that
// need dialSSHSession to observe a specific failure from an otherwise
// real SSH server rather than from a mock:
//   - sshServerNormal: accept sessions and run "exec" requests through
//     /bin/sh -c, as a real, well-behaved sshd would (the default).
//   - sshServerRejectSession: reject every "session" channel outright,
//     so client.NewSession() itself fails (e.g. a server configured
//     with no shell access at all).
//   - sshServerCloseOnAccept: accept the "session" channel but close it
//     immediately without handling any request, so sess.Start fails.
//   - sshServerCloseWithoutExitStatus: accept and start the process,
//     but tear down the channel without ever sending "exit-status" —
//     the wire-level equivalent of a connection dropping mid-command,
//     so sess.Wait returns something other than *ssh.ExitError.
type sshServerMode int

const (
	sshServerNormal sshServerMode = iota
	sshServerRejectSession
	sshServerCloseOnAccept
	sshServerCloseWithoutExitStatus
)

type testSSHServer struct {
	Addr     string
	User     string
	Password string
}

func newTestSSHServer(t *testing.T) *testSSHServer {
	t.Helper()
	return newTestSSHServerMode(t, sshServerNormal)
}

func newTestSSHServerMode(t *testing.T, mode sshServerMode) *testSSHServer {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating test host key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatalf("ssh.NewSignerFromKey: %v", err)
	}

	srv := &testSSHServer{User: "testuser", Password: "testpass"}

	config := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if c.User() == srv.User && string(pass) == srv.Password {
				return nil, nil
			}
			return nil, fmt.Errorf("auth failed")
		},
	}
	config.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	srv.Addr = ln.Addr().String()

	go func() {
		for {
			nConn, err := ln.Accept()
			if err != nil {
				return
			}
			go handleSSHConn(nConn, config, mode)
		}
	}()

	return srv
}

func handleSSHConn(nConn net.Conn, config *ssh.ServerConfig, mode sshServerMode) {
	sConn, chans, reqs, err := ssh.NewServerConn(nConn, config)
	if err != nil {
		return
	}
	defer sConn.Close()
	go ssh.DiscardRequests(reqs)
	for newCh := range chans {
		if newCh.ChannelType() != "session" || mode == sshServerRejectSession {
			newCh.Reject(ssh.UnknownChannelType, "unsupported channel type")
			continue
		}
		go handleSSHSession(newCh, mode)
	}
}

func handleSSHSession(newCh ssh.NewChannel, mode sshServerMode) {
	ch, requests, err := newCh.Accept()
	if err != nil {
		return
	}
	defer ch.Close()

	if mode == sshServerCloseOnAccept {
		return // defer ch.Close() above fires immediately, no requests handled
	}

	for req := range requests {
		switch req.Type {
		case "exec":
			cmdLine := parseExecPayload(req.Payload)
			req.Reply(true, nil)
			if mode == sshServerCloseWithoutExitStatus {
				return // ch.Close() via defer, no exit-status ever sent
			}
			runShellOverSSHChannel(ch, cmdLine)
			return
		default:
			if req.WantReply {
				req.Reply(false, nil)
			}
		}
	}
}

// parseExecPayload decodes an SSH "exec" channel request's payload: a
// single SSH string (uint32 length prefix + bytes) holding the command
// line.
func parseExecPayload(payload []byte) string {
	if len(payload) < 4 {
		return ""
	}
	n := binary.BigEndian.Uint32(payload[:4])
	if uint64(n) > uint64(len(payload)-4) {
		return ""
	}
	return string(payload[4 : 4+n])
}

// runShellOverSSHChannel runs cmdLine through /bin/sh -c, wired to ch.
//
// Deliberately uses cmd.StdinPipe() rather than assigning ch directly to
// cmd.Stdin: with a plain non-*os.File Stdin, exec.Cmd.Wait() blocks
// until *its own* internal stdin-copying goroutine reaches EOF — which
// here would mean waiting for the SSH client to close its stdin, even
// for a child process (like `exit 7`) that never reads stdin at all.
// Real sshd has no such quirk; StdinPipe (whose docs promise Wait
// doesn't wait on it) plus a detached copy goroutine avoids it here too.
func runShellOverSSHChannel(ch ssh.Channel, cmdLine string) {
	cmd := exec.Command("sh", "-c", cmdLine)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		sendExitStatus(ch, 127)
		return
	}
	cmd.Stdout = ch
	cmd.Stderr = ch.Stderr()

	if err := cmd.Start(); err != nil {
		sendExitStatus(ch, 127)
		return
	}
	go func() {
		io.Copy(stdin, ch)
		stdin.Close()
	}()

	code := 0
	if err := cmd.Wait(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			code = 1
		}
	}
	sendExitStatus(ch, code)
}

func sendExitStatus(ch ssh.Channel, code int) {
	type exitStatusMsg struct{ Status uint32 }
	ch.SendRequest("exit-status", false, ssh.Marshal(exitStatusMsg{Status: uint32(code)}))
}
