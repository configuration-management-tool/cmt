// SPDX-License-Identifier: BSD-3-Clause
//
// Copyright (c) 2026, the configuration-management-tool/cmt authors

// Package manifest decodes cmt's HCL2 manifest format into a typed,
// validated in-memory model: global environment variables, named
// hosts groups (cmt's rename of sup's "network"), named commands, and
// named targets that sequence commands.
package manifest

import "fmt"

// Manifest is the fully decoded and validated contents of a cmt manifest
// file (cmt.hcl by default).
type Manifest struct {
	// Env holds global environment variables, from the top-level env{}
	// block. Every command's remote invocation is prefixed with these
	// (merged with, and overridden by, its hosts_group's own env).
	Env map[string]string

	// HostsGroups holds every hosts_group block, keyed by its label.
	HostsGroups map[string]HostsGroup

	// Commands holds every command block, keyed by its label.
	Commands map[string]Command

	// Targets holds every target block, keyed by its label.
	Targets map[string]Target
}

// HostsGroup is a named group of hosts a command can run against — cmt's
// rename of sup's "network" (chosen to avoid the networking connotation).
type HostsGroup struct {
	// Name is the block's label.
	Name string

	// Hosts is a static list of host strings ("host", "user@host", or
	// "user@host:port"). Mutually exclusive with Inventory.
	Hosts []string

	// Inventory, when set, is a local shell command whose stdout (one
	// host string per line) is resolved at run time instead of Hosts.
	Inventory string

	// Env holds hosts_group-level environment variables; these win over
	// Manifest.Env for the same key.
	Env map[string]string

	// SSH, WinRM, and Become configure how connections to this group's
	// hosts are made. At most one of SSH/WinRM should be set; when
	// neither is, SSH with default settings is used. All are optional.
	SSH    *SSHConfig
	WinRM  *WinRMConfig
	Become *BecomeConfig
}

// SSHConfig is a hosts_group's optional per-group SSH connection
// configuration (an `ssh {}` block). Any field left at its zero value
// falls back to go-remoteexec/transport's own default for that field
// (see SSHConfig in that package), except where noted.
type SSHConfig struct {
	User                 string
	Port                 int
	Password             string
	PrivateKey           string // path to a private key file
	PrivateKeyPassphrase string
	// HostKeyCheck defaults to false (off) when unset, matching
	// go-remoteexec/transport's own zero-value behavior — a manifest
	// must opt in explicitly with `host-key-check = true`.
	HostKeyCheck   bool
	KnownHostsFile string
	TTY            bool
	TempDir        string
	// ConnectTimeout is in seconds; 0 means "use the library default".
	ConnectTimeout int
}

// WinRMConfig is a hosts_group's optional per-group WinRM connection
// configuration (a `winrm {}` block). Its presence on a HostsGroup
// selects WinRM over the default SSH transport for that group's hosts.
type WinRMConfig struct {
	User     string
	Port     int
	Password string
	// SSL and SSLVerify default to false (off) when unset, matching
	// go-remoteexec/transport's own zero-value behavior.
	SSL            bool
	SSLVerify      bool
	CACert         string
	Transport      string
	ClientCert     string
	ClientKey      string
	ConnectTimeout int // seconds
	TempDir        string
	Path           string
}

// BecomeConfig is a hosts_group's optional privilege-escalation
// configuration (a `become {}` block).
type BecomeConfig struct {
	// Method is "sudo" (default), "su", or "doas".
	Method   string
	User     string
	Password string
}

// Command is a named shell action a target can sequence — a remote `run`,
// a `local` action on the invoking machine, or a file `upload`. Exactly
// one of Run, Local, or Upload is set.
type Command struct {
	Name string
	Desc string

	// Run, when set, is executed on every resolved host of the
	// hosts_group the command is invoked against.
	Run string

	// Local, when set, is executed once on the invoking machine
	// regardless of the hosts_group (sup's `local` semantics).
	Local string

	// Upload, when set, copies a local file/directory to every
	// resolved host.
	Upload *Upload

	// Serial caps concurrent hosts for a Run or Upload command; 0 (or
	// unset) means unlimited (fan out to every host at once).
	Serial int

	// Once, when true, runs a Run or Upload command on only the first
	// resolved host instead of every host.
	Once bool

	// Interactive, when true, changes *how* Run is executed: instead of
	// the buffered "run to completion, then return one Result per host"
	// path (package orchestrate), it opens a live, streaming session to
	// every resolved host at once and forwards cmt's own stdin to all of
	// them keystroke-by-keystroke, live (package interactive) — sup's
	// `stdin: true`. Only valid alongside Run (Local and Upload commands
	// have no interactive equivalent), and mutually exclusive with
	// Serial/Once (an interactive command always addresses every
	// resolved host at once; there is no way to run it "serially" or
	// "once" and still call it a live multi-host session). An
	// interactive command referenced from inside a target's Commands is
	// a parse-time error (see Validate) — sup's stdin sessions are
	// invoked standalone, not chained into a fail-fast sequence.
	Interactive bool
}

// Upload describes a file/directory copy command (a command's `upload {}`
// block).
type Upload struct {
	Src        string
	Dst        string
	Executable bool
}

// Target is a named, ordered list of command names run in sequence
// (fail-fast: a failure on any host stops the remaining commands).
type Target struct {
	Name     string
	Commands []string
}

// Validate checks cross-references within m: every command a target lists
// must exist. It is called automatically by Parse/ParseFile.
func (m *Manifest) Validate() error {
	for _, t := range m.Targets {
		for _, cmdName := range t.Commands {
			if cmd, ok := m.Commands[cmdName]; ok {
				if cmd.Interactive {
					return fmt.Errorf("manifest: target %q references interactive command %q: interactive commands cannot be chained inside a target, invoke them standalone (cmt HOSTS_GROUP %s)", t.Name, cmdName, cmdName)
				}
				continue
			}
			if _, ok := m.Targets[cmdName]; ok {
				return fmt.Errorf("manifest: target %q references %q, which is a target, not a command (targets cannot nest)", t.Name, cmdName)
			}
			return fmt.Errorf("manifest: target %q references unknown command %q", t.Name, cmdName)
		}
	}
	return nil
}

// ExpandCommands resolves a list of CLI-supplied command/target names into
// a flat, ordered list of command names: a name that matches a target
// expands to that target's command list (one level, matching sup); a name
// that matches a command passes through unchanged. An unknown name is an
// error.
func (m *Manifest) ExpandCommands(names []string) ([]string, error) {
	var out []string
	for _, n := range names {
		if t, ok := m.Targets[n]; ok {
			out = append(out, t.Commands...)
			continue
		}
		if _, ok := m.Commands[n]; ok {
			out = append(out, n)
			continue
		}
		return nil, fmt.Errorf("manifest: %q is not a known command or target", n)
	}
	return out, nil
}
