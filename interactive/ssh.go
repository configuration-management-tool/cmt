// SPDX-License-Identifier: BSD-3-Clause
//
// Copyright (c) 2026, the configuration-management-tool/cmt authors

// This file drives a live, streaming SSH session for cmt's
// `interactive = true` commands directly against golang.org/x/crypto/ssh,
// instead of going through github.com/go-remoteexec/transport the way
// package connect (cmt's buffered path) does.
//
// Why not reuse go-remoteexec/transport.SSH? That type's only public
// surface is the buffered remoteexec.Connection interface —
// Exec(ctx, cmd, stdin) (Result, error), returning one Result only after
// the remote command finishes. Nothing in that interface exposes a live
// ssh.Session, or the dialed *ssh.Client underneath it, so there is no
// way to back a progressively-streamed, live-stdin session with it no
// matter how it's wrapped — the buffered shape is baked into the
// interface itself, not an accident of this one type.
//
// Because of that, this file re-implements a small amount of SSH
// auth-method construction — private key parsing, password auth,
// ssh-agent auth, and a host-key-checking callback — that already exists
// privately inside go-remoteexec/transport. That duplication is
// deliberate, not an oversight: at the time this was written,
// go-remoteexec/transport is under active, unrelated development
// elsewhere (SSH TTY support, WinRM.ExecArgv, become-wrapping changes),
// and this project should not collide with that in-flight work by
// reaching into it. Once go-remoteexec/transport grows a
// streaming/session primitive of its own — or exposes the *ssh.Client it
// dials — this file should be migrated onto that and this duplication
// deleted. See also the README's architecture section, which carries
// the same note for anyone landing here from the docs rather than the
// code.
package interactive

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/configuration-management-tool/cmt/connect"
	"github.com/configuration-management-tool/cmt/manifest"
)

// dialSSHSession opens a live SSH session on hostSpec (honoring group's
// optional ssh{} config, same field set connect/buildSSHConfig maps for
// the buffered path — see manifest.SSHConfig) and starts rawCmd on it.
func dialSSHSession(ctx context.Context, hostSpec string, group manifest.HostsGroup, rawCmd string) (*hostSession, error) {
	user, host, port, err := connect.ParseHost(hostSpec)
	if err != nil {
		return nil, err
	}
	cfg := group.SSH

	if user == "" && cfg != nil {
		user = cfg.User
	}
	if user == "" {
		user = "root"
	}
	if port == 0 {
		port = 22
		if cfg != nil && cfg.Port != 0 {
			port = cfg.Port
		}
	}

	clientCfg, err := buildSSHClientConfig(user, cfg)
	if err != nil {
		return nil, err
	}

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("interactive: dialing %s: %w", addr, err)
	}

	sshConn, chans, reqs, err := ssh.NewClientConn(conn, addr, clientCfg)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("interactive: ssh handshake with %s: %w", addr, err)
	}
	client := ssh.NewClient(sshConn, chans, reqs)

	sess, err := client.NewSession()
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("interactive: opening ssh session on %s: %w", addr, err)
	}

	// As with local.go's cmd.Std{in,out,err}Pipe calls, these three only
	// ever error on a session that's already had Start/Shell/Run called
	// or the corresponding Stdin/Stdout/Stderr field already set —
	// neither applies to sess here, so they cannot be reached through
	// this function's own call pattern. Each one closes the session AND
	// the client before returning, which is exactly the kind of cleanup
	// that goes wrong unnoticed when nothing ever runs it, so they are
	// called through the seams below and a test swaps those.
	stdin, err := sshStdinPipe(sess)
	if err != nil {
		sess.Close()
		client.Close()
		return nil, err
	}
	stdout, err := sshStdoutPipe(sess)
	if err != nil {
		sess.Close()
		client.Close()
		return nil, err
	}
	stderr, err := sshStderrPipe(sess)
	if err != nil {
		sess.Close()
		client.Close()
		return nil, err
	}

	if err := sess.Start(rawCmd); err != nil {
		sess.Close()
		client.Close()
		return nil, fmt.Errorf("interactive: starting %q on %s: %w", rawCmd, addr, err)
	}

	// Force-close the session/client promptly on context cancellation
	// (SIGINT, propagated as ctx by Runner.Run) so a sess.Wait() blocked
	// in the wait closure below returns instead of hanging — unlike
	// local.go's exec.CommandContext, golang.org/x/crypto/ssh has no
	// built-in context-cancellation hookup.
	go func() {
		<-ctx.Done()
		sess.Close()
		client.Close()
	}()

	return &hostSession{
		host:   hostSpec,
		stdin:  stdin,
		stdout: stdout,
		stderr: stderr,
		wait: func() (int, bool, error) {
			werr := sess.Wait()
			if ctx.Err() != nil {
				return -1, true, nil
			}
			if werr == nil {
				return 0, false, nil
			}
			var ee *ssh.ExitError
			if errors.As(werr, &ee) {
				return ee.ExitStatus(), false, nil
			}
			return -1, false, werr
		},
		close: func() error {
			sess.Close()
			return client.Close()
		},
	}, nil
}

// Seams for the session's three pipe constructors, so the cleanup each
// error path performs — closing the session AND the dialed client — is
// executed by a test rather than only inspected. Swapped with t.Cleanup
// restore, as interactive.go's isLocalHost is; no test in this package
// calls t.Parallel.
var (
	sshStdinPipe  = (*ssh.Session).StdinPipe
	sshStdoutPipe = (*ssh.Session).StdoutPipe
	sshStderrPipe = (*ssh.Session).StderrPipe
)

// buildSSHClientConfig maps a manifest.SSHConfig onto an
// *ssh.ClientConfig: private key (optionally passphrase-protected),
// ssh-agent (via $SSH_AUTH_SOCK, when set), and password auth, tried in
// that order — plus a host-key callback that defaults to
// InsecureIgnoreHostKey, matching manifest.SSHConfig.HostKeyCheck's own
// documented default-off (a manifest must opt in explicitly).
func buildSSHClientConfig(user string, cfg *manifest.SSHConfig) (*ssh.ClientConfig, error) {
	var auths []ssh.AuthMethod

	if cfg != nil && cfg.PrivateKey != "" {
		signer, err := loadPrivateKeySigner(cfg.PrivateKey, cfg.PrivateKeyPassphrase)
		if err != nil {
			return nil, err
		}
		auths = append(auths, ssh.PublicKeys(signer))
	}
	if am, ok := sshAgentAuthMethod(); ok {
		auths = append(auths, am)
	}
	if cfg != nil && cfg.Password != "" {
		auths = append(auths, ssh.Password(cfg.Password))
	}
	if len(auths) == 0 {
		return nil, fmt.Errorf("interactive: no SSH auth method configured (set ssh.private-key or ssh.password in the manifest, or run ssh-agent)")
	}

	hostKeyCallback := ssh.InsecureIgnoreHostKey() //nolint:gosec // explicit, documented opt-out default; see manifest.SSHConfig.HostKeyCheck
	if cfg != nil && cfg.HostKeyCheck {
		cb, err := knownhosts.New(knownHostsPath(cfg.KnownHostsFile))
		if err != nil {
			return nil, fmt.Errorf("interactive: loading known_hosts: %w", err)
		}
		hostKeyCallback = cb
	}

	timeout := 30 * time.Second
	if cfg != nil && cfg.ConnectTimeout > 0 {
		timeout = time.Duration(cfg.ConnectTimeout) * time.Second
	}

	return &ssh.ClientConfig{
		User:            user,
		Auth:            auths,
		HostKeyCallback: hostKeyCallback,
		Timeout:         timeout,
	}, nil
}

// knownHostsPath returns configured, or ~/.ssh/known_hosts when it's
// empty (matching common ssh(1)/go-remoteexec default-known-hosts
// behavior).
func knownHostsPath(configured string) string {
	if configured != "" {
		return configured
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home + "/.ssh/known_hosts"
}

func loadPrivateKeySigner(path, passphrase string) (ssh.Signer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("interactive: reading private key %s: %w", path, err)
	}
	if passphrase != "" {
		signer, err := ssh.ParsePrivateKeyWithPassphrase(data, []byte(passphrase))
		if err != nil {
			return nil, fmt.Errorf("interactive: parsing private key %s: %w", path, err)
		}
		return signer, nil
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		return nil, fmt.Errorf("interactive: parsing private key %s: %w", path, err)
	}
	return signer, nil
}

// sshAgentAuthMethod returns an ssh.AuthMethod backed by a running
// ssh-agent (via $SSH_AUTH_SOCK), or ok=false when no agent is
// reachable. The agent connection is deliberately kept open for the
// process lifetime rather than closed here: ssh.PublicKeysCallback calls
// back into it lazily during the handshake, so closing early would break
// that — acceptable for a short-lived CLI invocation.
func sshAgentAuthMethod() (ssh.AuthMethod, bool) {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil, false
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil, false
	}
	ag := agent.NewClient(conn)
	return ssh.PublicKeysCallback(ag.Signers), true
}
