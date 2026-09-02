// SPDX-License-Identifier: BSD-3-Clause
//
// Copyright (c) 2026, the configuration-management-tool/cmt authors

// Package connect turns a hosts_group's host strings and optional
// ssh/winrm/become configuration into live
// github.com/go-remoteexec/transport connections: local commands and
// literal "localhost"/"127.0.0.1"/"::1" hosts get transport.NewLocal(),
// everything else dials SSH by default (WinRM only when a group
// explicitly configures it), optionally wrapped in transport.Become.
//
// This mirrors the config-resolution pattern of
// go-puppet-bolt/bolt's sshtransport.go dial(): map this repo's own
// config shape onto go-remoteexec/transport's config structs, dial, then
// decorate with Become — go-remoteexec/transport itself owns the wire
// protocols (SSH/WinRM/sudo), so none of that is reimplemented here.
package connect

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	remoteexec "github.com/go-remoteexec/transport"

	"github.com/configuration-management-tool/cmt/manifest"
)

// dialSSHFunc and dialWinRMFunc are remoteexec.DialSSH/DialWinRM behind a
// variable so tests can substitute a fake dial outcome for Dial's
// post-dial logic (the Become wrap, the final return) without a real
// network call or an in-process SSH/WinRM server — go-remoteexec/
// transport already owns testing those wire protocols themselves.
var (
	dialSSHFunc = func(ctx context.Context, cfg remoteexec.SSHConfig) (remoteexec.Connection, error) {
		return remoteexec.DialSSH(ctx, cfg)
	}
	dialWinRMFunc = func(ctx context.Context, cfg remoteexec.WinRMConfig) (remoteexec.Connection, error) {
		return remoteexec.DialWinRM(ctx, cfg)
	}
)

// LocalHost is the pseudo-host name that always resolves to
// transport.NewLocal(), regardless of any hosts_group configuration.
// "127.0.0.1" and "::1" are also treated as local.
const LocalHost = "localhost"

// ParseHost splits a host string of the form "host", "user@host", or
// "user@host:port" into its parts. port is 0 when unspecified. An
// unbracketed IPv6 literal (more than one ':') is never split on ":" —
// there is no unambiguous way to tell a trailing ":port" apart from the
// address itself — so cmt's "host:port" form applies only to hostnames
// and IPv4 literals; an IPv6 target must omit the port (cmt has no
// bracket syntax for it).
func ParseHost(s string) (user, host string, port int, err error) {
	rest := s
	if i := strings.Index(rest, "@"); i >= 0 {
		user = rest[:i]
		rest = rest[i+1:]
	}
	host = rest
	if strings.Count(rest, ":") == 1 {
		i := strings.IndexByte(rest, ':')
		p, perr := strconv.Atoi(rest[i+1:])
		if perr != nil {
			return "", "", 0, fmt.Errorf("connect: invalid port in host %q: %w", s, perr)
		}
		host, port = rest[:i], p
	}
	if host == "" {
		return "", "", 0, fmt.Errorf("connect: empty host in %q", s)
	}
	return user, host, port, nil
}

// IsLocal reports whether host (as returned by ParseHost) refers to the
// local machine.
func IsLocal(host string) bool {
	switch host {
	case LocalHost, "127.0.0.1", "::1":
		return true
	default:
		return false
	}
}

// Dial resolves hostSpec into a live transport.Connection, honoring
// group's optional ssh/winrm/become configuration. hostSpec may be a
// plain host, "user@host", "user@host:port", or LocalHost.
func Dial(ctx context.Context, hostSpec string, group manifest.HostsGroup) (remoteexec.Connection, error) {
	user, host, port, err := ParseHost(hostSpec)
	if err != nil {
		return nil, err
	}
	if IsLocal(host) {
		return remoteexec.NewLocal(), nil
	}

	var conn remoteexec.Connection
	if group.WinRM != nil {
		conn, err = dialWinRMFunc(ctx, buildWinRMConfig(user, host, port, group.WinRM))
	} else {
		conn, err = dialSSHFunc(ctx, buildSSHConfig(user, host, port, group.SSH))
	}
	if err != nil {
		return nil, fmt.Errorf("connect: dialing %q: %w", hostSpec, err)
	}
	return applyBecome(conn, group.Become), nil
}

// applyBecome wraps conn in remoteexec.Become when b is set, otherwise
// returns conn unchanged. Split out from Dial so the decision is
// unit-testable without a network dial.
func applyBecome(conn remoteexec.Connection, b *manifest.BecomeConfig) remoteexec.Connection {
	if b == nil {
		return conn
	}
	return remoteexec.Become(conn, becomeConfig(*b))
}

// buildSSHConfig maps the host string's own user/port (highest priority)
// and the group's ssh{} block (fallback) onto a remoteexec.SSHConfig. It
// does no I/O, which keeps every mapping decision unit-testable without a
// network dial.
func buildSSHConfig(user, host string, port int, cfg *manifest.SSHConfig) remoteexec.SSHConfig {
	c := remoteexec.SSHConfig{Host: host, User: user, Port: port}
	if cfg != nil {
		if c.User == "" {
			c.User = cfg.User
		}
		if c.Port == 0 {
			c.Port = cfg.Port
		}
		c.Password = cfg.Password
		c.PrivateKeyFile = cfg.PrivateKey
		c.PrivateKeyPassphrase = cfg.PrivateKeyPassphrase
		c.HostKeyCheck = cfg.HostKeyCheck
		c.KnownHostsFile = cfg.KnownHostsFile
		c.TTY = cfg.TTY
		c.TempDir = cfg.TempDir
		if cfg.ConnectTimeout > 0 {
			c.Timeout = time.Duration(cfg.ConnectTimeout) * time.Second
		}
	}
	if c.User == "" {
		c.User = "root"
	}
	return c
}

// buildWinRMConfig maps the host string's own user/port (highest
// priority) and the group's winrm{} block onto a remoteexec.WinRMConfig.
// It does no I/O, for the same reason as buildSSHConfig.
func buildWinRMConfig(user, host string, port int, cfg *manifest.WinRMConfig) remoteexec.WinRMConfig {
	c := remoteexec.WinRMConfig{Host: host, User: user, Port: port}
	if cfg != nil {
		if c.User == "" {
			c.User = cfg.User
		}
		if c.Port == 0 {
			c.Port = cfg.Port
		}
		c.Password = cfg.Password
		c.SSL = cfg.SSL
		c.SSLVerify = cfg.SSLVerify
		c.CACert = cfg.CACert
		c.Transport = cfg.Transport
		c.ClientCert = cfg.ClientCert
		c.ClientKey = cfg.ClientKey
		c.TempDir = cfg.TempDir
		c.Path = cfg.Path
		if cfg.ConnectTimeout > 0 {
			c.ConnectTimeout = time.Duration(cfg.ConnectTimeout) * time.Second
		}
	}
	return c
}

func becomeConfig(b manifest.BecomeConfig) remoteexec.BecomeConfig {
	method := remoteexec.BecomeMethod(b.Method)
	if method == "" {
		method = remoteexec.BecomeSudo
	}
	return remoteexec.BecomeConfig{Method: method, User: b.User, Password: b.Password}
}
