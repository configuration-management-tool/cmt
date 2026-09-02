// SPDX-License-Identifier: BSD-3-Clause
//
// Copyright (c) 2026, the configuration-management-tool/cmt authors

package connect

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	remoteexec "github.com/go-remoteexec/transport"

	"github.com/configuration-management-tool/cmt/manifest"
)

func TestParseHost(t *testing.T) {
	tests := []struct {
		in       string
		wantUser string
		wantHost string
		wantPort int
		wantErr  bool
	}{
		{in: "host", wantHost: "host"},
		{in: "user@host", wantUser: "user", wantHost: "host"},
		{in: "user@host:2222", wantUser: "user", wantHost: "host", wantPort: 2222},
		{in: "host:2222", wantHost: "host", wantPort: 2222},
		{in: "user@host:notaport", wantErr: true},
		{in: "@", wantErr: true}, // empty host
		{in: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			user, host, port, err := ParseHost(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseHost(%q): expected an error", tt.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseHost(%q): %v", tt.in, err)
			}
			if user != tt.wantUser || host != tt.wantHost || port != tt.wantPort {
				t.Errorf("ParseHost(%q) = (%q, %q, %d), want (%q, %q, %d)",
					tt.in, user, host, port, tt.wantUser, tt.wantHost, tt.wantPort)
			}
		})
	}
}

func TestIsLocal(t *testing.T) {
	for _, h := range []string{"localhost", "127.0.0.1", "::1"} {
		if !IsLocal(h) {
			t.Errorf("IsLocal(%q) = false, want true", h)
		}
	}
	for _, h := range []string{"example.com", "10.0.0.1", ""} {
		if IsLocal(h) {
			t.Errorf("IsLocal(%q) = true, want false", h)
		}
	}
}

func TestDialLocal(t *testing.T) {
	for _, host := range []string{"localhost", "127.0.0.1", "::1", "user@localhost", "localhost:22"} {
		conn, err := Dial(context.Background(), host, manifest.HostsGroup{})
		if err != nil {
			t.Fatalf("Dial(%q): %v", host, err)
		}
		defer conn.Close()
		res, err := conn.Exec(context.Background(), "echo hi", nil)
		if err != nil {
			t.Fatalf("Exec on %q: %v", host, err)
		}
		if strings.TrimSpace(res.Stdout) != "hi" {
			t.Errorf("Exec on %q: stdout = %q", host, res.Stdout)
		}
	}
}

func TestDialBadHostString(t *testing.T) {
	if _, err := Dial(context.Background(), "user@host:notaport", manifest.HostsGroup{}); err == nil {
		t.Fatal("expected an error for a malformed host string")
	}
}

func TestBuildSSHConfig(t *testing.T) {
	// Host string user/port win over the group's ssh{} config.
	c := buildSSHConfig("stringuser", "h", 2200, &manifest.SSHConfig{
		User: "cfguser", Port: 22, Password: "pw", PrivateKey: "/key", PrivateKeyPassphrase: "phrase",
		HostKeyCheck: true, KnownHostsFile: "/kh", TTY: true, TempDir: "/tmp2", ConnectTimeout: 5,
	})
	if c.User != "stringuser" || c.Port != 2200 {
		t.Errorf("host-string user/port should win: %+v", c)
	}
	if c.Password != "pw" || c.PrivateKeyFile != "/key" || c.PrivateKeyPassphrase != "phrase" ||
		!c.HostKeyCheck || c.KnownHostsFile != "/kh" || !c.TTY || c.TempDir != "/tmp2" ||
		c.Timeout != 5*time.Second {
		t.Errorf("ssh config fields not mapped: %+v", c)
	}

	// No host-string user/port: the group's config fills them in.
	c = buildSSHConfig("", "h", 0, &manifest.SSHConfig{User: "cfguser", Port: 22})
	if c.User != "cfguser" || c.Port != 22 {
		t.Errorf("expected group config to fill in user/port: %+v", c)
	}

	// No group config at all: default user is "root", port passes through.
	c = buildSSHConfig("", "h", 0, nil)
	if c.User != "root" {
		t.Errorf("expected default user root, got %q", c.User)
	}

	// A zero ConnectTimeout leaves the library's own default (0 = "use
	// library default") alone.
	c = buildSSHConfig("u", "h", 0, &manifest.SSHConfig{})
	if c.Timeout != 0 {
		t.Errorf("expected Timeout 0 (library default), got %v", c.Timeout)
	}
}

func TestBuildWinRMConfig(t *testing.T) {
	c := buildWinRMConfig("stringuser", "h", 5987, &manifest.WinRMConfig{
		User: "cfguser", Port: 5986, Password: "pw", SSL: true, SSLVerify: true, CACert: "/ca",
		Transport: "basic", ClientCert: "/cc", ClientKey: "/ck", ConnectTimeout: 10,
		TempDir: `C:\Temp`, Path: "/wsman2",
	})
	if c.User != "stringuser" || c.Port != 5987 {
		t.Errorf("host-string user/port should win: %+v", c)
	}
	if c.Password != "pw" || !c.SSL || !c.SSLVerify || c.CACert != "/ca" || c.Transport != "basic" ||
		c.ClientCert != "/cc" || c.ClientKey != "/ck" || c.ConnectTimeout != 10*time.Second ||
		c.TempDir != `C:\Temp` || c.Path != "/wsman2" {
		t.Errorf("winrm config fields not mapped: %+v", c)
	}

	c = buildWinRMConfig("", "h", 0, &manifest.WinRMConfig{User: "cfguser", Port: 5986})
	if c.User != "cfguser" || c.Port != 5986 {
		t.Errorf("expected group config to fill in user/port: %+v", c)
	}

	c = buildWinRMConfig("u", "h", 0, nil)
	if c.User != "u" || c.Password != "" {
		t.Errorf("expected passthrough with nil cfg: %+v", c)
	}

	c = buildWinRMConfig("u", "h", 0, &manifest.WinRMConfig{})
	if c.ConnectTimeout != 0 {
		t.Errorf("expected ConnectTimeout 0 (library default), got %v", c.ConnectTimeout)
	}
}

func TestBecomeConfig(t *testing.T) {
	c := becomeConfig(manifest.BecomeConfig{Method: "su", User: "root", Password: "pw"})
	if c.Method != remoteexec.BecomeSudo && c.Method != "su" {
		t.Errorf("unexpected method mapping: %+v", c)
	}
	if c.Method != "su" || c.User != "root" || c.Password != "pw" {
		t.Errorf("becomeConfig = %+v", c)
	}

	// An empty Method defaults to sudo.
	c = becomeConfig(manifest.BecomeConfig{User: "root"})
	if c.Method != remoteexec.BecomeSudo {
		t.Errorf("expected default method sudo, got %q", c.Method)
	}
}

// TestDialSSHUnreachable exercises the real ssh dial path end to end
// (host/port/cfg mapping -> remoteexec.DialSSH) against a loopback
// address nothing listens on, so it fails fast with "connection
// refused" — no network access and no in-process SSH server required
// (go-remoteexec/transport owns testing its own wire protocol).
func TestDialSSHUnreachable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	group := manifest.HostsGroup{SSH: &manifest.SSHConfig{ConnectTimeout: 2}}
	_, err := Dial(ctx, "nobody@127.0.0.2:1", group)
	if err == nil {
		t.Fatal("expected a dial error against an unreachable port")
	}
	if !strings.Contains(err.Error(), "connect: dialing") {
		t.Errorf("error = %q, want a %q prefix", err.Error(), "connect: dialing")
	}
}

// TestDialWinRMUnreachable is TestDialSSHUnreachable's WinRM counterpart.
func TestDialWinRMUnreachable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	group := manifest.HostsGroup{WinRM: &manifest.WinRMConfig{Port: 1, ConnectTimeout: 2}}
	_, err := Dial(ctx, "127.0.0.2", group)
	if err == nil {
		t.Fatal("expected a dial error against an unreachable port")
	}
	if !strings.Contains(err.Error(), "connect: dialing") {
		t.Errorf("error = %q, want a %q prefix", err.Error(), "connect: dialing")
	}
}

// fakeConn is a minimal remoteexec.Connection double used only to drive
// Dial's post-dial success path (see TestDialSuccessPath) without a real
// network call.
type fakeConn struct{}

func (fakeConn) Exec(context.Context, string, io.Reader) (remoteexec.Result, error) {
	return remoteexec.Result{}, nil
}
func (fakeConn) Put(context.Context, string, string, remoteexec.PutOptions) error { return nil }
func (fakeConn) Fetch(context.Context, string, string) error                     { return nil }
func (fakeConn) Remove(context.Context, string) error                            { return nil }
func (fakeConn) TempPath(base string) string                                     { return "/tmp/" + base }
func (fakeConn) Close() error                                                    { return nil }

// TestDialSuccessPath exercises Dial's post-dial logic (the Become wrap,
// the final return) by substituting dialSSHFunc/dialWinRMFunc for the
// duration of the test — Dial's own SSH/WinRM branches are otherwise
// identical in shape to TestDialSSHUnreachable/TestDialWinRMUnreachable,
// which already exercise a real (fast-failing) dial; this test only
// covers what a successful dial does next, without standing up an
// in-process SSH/WinRM server (go-remoteexec/transport tests its own
// wire protocols).
func TestDialSuccessPath(t *testing.T) {
	origSSH, origWinRM := dialSSHFunc, dialWinRMFunc
	t.Cleanup(func() { dialSSHFunc, dialWinRMFunc = origSSH, origWinRM })

	dialSSHFunc = func(context.Context, remoteexec.SSHConfig) (remoteexec.Connection, error) {
		return fakeConn{}, nil
	}
	dialWinRMFunc = func(context.Context, remoteexec.WinRMConfig) (remoteexec.Connection, error) {
		return fakeConn{}, nil
	}

	conn, err := Dial(context.Background(), "example.com", manifest.HostsGroup{
		Become: &manifest.BecomeConfig{Method: "sudo", User: "root"},
	})
	if err != nil {
		t.Fatalf("Dial (ssh): %v", err)
	}
	if _, ok := conn.(fakeConn); ok {
		t.Errorf("expected Dial to wrap the connection in Become")
	}

	conn, err = Dial(context.Background(), "example.com", manifest.HostsGroup{
		WinRM: &manifest.WinRMConfig{},
	})
	if err != nil {
		t.Fatalf("Dial (winrm): %v", err)
	}
	if _, ok := conn.(fakeConn); !ok {
		t.Errorf("expected the fake connection back unwrapped (no become configured)")
	}
}

// TestApplyBecome covers Dial's post-dial Become-wrapping decision
// directly (Dial itself can only reach it after a live SSH/WinRM dial,
// which this repo's tests deliberately don't stand up — see the package
// doc comment on why that protocol testing belongs to
// go-remoteexec/transport, not here).
func TestApplyBecome(t *testing.T) {
	base := remoteexec.NewLocal()

	if got := applyBecome(base, nil); got != remoteexec.Connection(base) {
		t.Errorf("applyBecome with nil BecomeConfig should return conn unchanged")
	}

	wrapped := applyBecome(base, &manifest.BecomeConfig{Method: "sudo", User: "root"})
	if wrapped == remoteexec.Connection(base) {
		t.Errorf("applyBecome with a BecomeConfig should wrap conn, not return it unchanged")
	}
}
