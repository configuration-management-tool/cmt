// SPDX-License-Identifier: BSD-3-Clause
//
// Copyright (c) 2026, the configuration-management-tool/cmt authors

package manifest

import (
	"fmt"
	"os"

	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// ParseFile reads and decodes the HCL2 manifest at path.
func ParseFile(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("manifest: reading %s: %w", path, err)
	}
	return Parse(data, path)
}

// Parse decodes HCL2 manifest source (as read from filename, used only for
// diagnostics) into a validated Manifest.
func Parse(src []byte, filename string) (*Manifest, error) {
	parser := hclparse.NewParser()
	file, diags := parser.ParseHCL(src, filename)
	if diags.HasErrors() {
		return nil, fmt.Errorf("manifest: %s", diags)
	}
	// ParseHCL always backs a diagnostics-free file with an *hclsyntax.Body;
	// asserting directly (rather than guarding an "impossible" branch) keeps
	// every line here reachable from a real manifest.
	body := file.Body.(*hclsyntax.Body)

	m := &Manifest{
		Env:         map[string]string{},
		HostsGroups: map[string]HostsGroup{},
		Commands:    map[string]Command{},
		Targets:     map[string]Target{},
	}

	for _, blk := range body.Blocks {
		switch blk.Type {
		case "env":
			if len(blk.Labels) != 0 {
				return nil, fmt.Errorf("%s: env block takes no label", blk.TypeRange)
			}
			env, err := attrsToStringMap(blk.Body.Attributes)
			if err != nil {
				return nil, fmt.Errorf("manifest: env block: %w", err)
			}
			if err := checkUnknownBlocks(blk.Body.Blocks); err != nil {
				return nil, fmt.Errorf("manifest: env block: %w", err)
			}
			for k, v := range env {
				m.Env[k] = v
			}

		case "hosts_group":
			hg, err := decodeHostsGroup(blk)
			if err != nil {
				return nil, err
			}
			if _, exists := m.HostsGroups[hg.Name]; exists {
				return nil, fmt.Errorf("%s: duplicate hosts_group %q", blk.TypeRange, hg.Name)
			}
			m.HostsGroups[hg.Name] = hg

		case "command":
			cmd, err := decodeCommand(blk)
			if err != nil {
				return nil, err
			}
			if _, exists := m.Commands[cmd.Name]; exists {
				return nil, fmt.Errorf("%s: duplicate command %q", blk.TypeRange, cmd.Name)
			}
			m.Commands[cmd.Name] = cmd

		case "target":
			tgt, err := decodeTarget(blk)
			if err != nil {
				return nil, err
			}
			if _, exists := m.Targets[tgt.Name]; exists {
				return nil, fmt.Errorf("%s: duplicate target %q", blk.TypeRange, tgt.Name)
			}
			m.Targets[tgt.Name] = tgt

		default:
			return nil, fmt.Errorf("%s: unknown top-level block %q", blk.TypeRange, blk.Type)
		}
	}

	if err := m.Validate(); err != nil {
		return nil, err
	}
	return m, nil
}

// firstErr calls each fn in order, returning the first non-nil error (and
// stopping there). It lets a block decoder read every one of its
// attributes as one small table instead of repeating an
// "if err != nil { return }" per field.
func firstErr(fns ...func() error) error {
	for _, fn := range fns {
		if err := fn(); err != nil {
			return err
		}
	}
	return nil
}

func blockLabel(blk *hclsyntax.Block) (string, error) {
	if len(blk.Labels) != 1 || blk.Labels[0] == "" {
		return "", fmt.Errorf("%s: %s block requires exactly one non-empty label", blk.TypeRange, blk.Type)
	}
	return blk.Labels[0], nil
}

func decodeHostsGroup(blk *hclsyntax.Block) (HostsGroup, error) {
	name, err := blockLabel(blk)
	if err != nil {
		return HostsGroup{}, err
	}
	attrs := blk.Body.Attributes

	var hosts []string
	var hasHosts, hasInventory bool
	var inventory string
	env := map[string]string{}
	err = firstErr(
		func() (err error) { hosts, hasHosts, err = attrStringList(attrs, "hosts"); return },
		func() (err error) { inventory, hasInventory, err = attrString(attrs, "inventory"); return },
		func() error {
			e, has, err := attrStringMap(attrs, "env")
			if has {
				env = e
			}
			return err
		},
	)
	if err != nil {
		return HostsGroup{}, fmt.Errorf("hosts_group %q: %w", name, err)
	}

	if err := checkUnknownAttrs(attrs, "hosts", "inventory", "env"); err != nil {
		return HostsGroup{}, fmt.Errorf("hosts_group %q: %w", name, err)
	}
	switch {
	case !hasHosts && !hasInventory:
		return HostsGroup{}, fmt.Errorf("hosts_group %q: must set either \"hosts\" or \"inventory\"", name)
	case hasHosts && hasInventory:
		return HostsGroup{}, fmt.Errorf("hosts_group %q: \"hosts\" and \"inventory\" are mutually exclusive", name)
	case hasHosts && len(hosts) == 0:
		return HostsGroup{}, fmt.Errorf("hosts_group %q: \"hosts\" must not be empty", name)
	}

	if err := checkUnknownBlocks(blk.Body.Blocks, "ssh", "winrm", "become"); err != nil {
		return HostsGroup{}, fmt.Errorf("hosts_group %q: %w", name, err)
	}

	hg := HostsGroup{Name: name, Hosts: hosts, Inventory: inventory, Env: env}

	sshBlk, err := soleBlock(blk.Body.Blocks, "ssh")
	if err != nil {
		return HostsGroup{}, fmt.Errorf("hosts_group %q: %w", name, err)
	}
	if sshBlk != nil {
		cfg, err := decodeSSHConfig(sshBlk)
		if err != nil {
			return HostsGroup{}, fmt.Errorf("hosts_group %q: ssh block: %w", name, err)
		}
		hg.SSH = &cfg
	}

	winrmBlk, err := soleBlock(blk.Body.Blocks, "winrm")
	if err != nil {
		return HostsGroup{}, fmt.Errorf("hosts_group %q: %w", name, err)
	}
	if winrmBlk != nil {
		cfg, err := decodeWinRMConfig(winrmBlk)
		if err != nil {
			return HostsGroup{}, fmt.Errorf("hosts_group %q: winrm block: %w", name, err)
		}
		hg.WinRM = &cfg
	}

	becomeBlk, err := soleBlock(blk.Body.Blocks, "become")
	if err != nil {
		return HostsGroup{}, fmt.Errorf("hosts_group %q: %w", name, err)
	}
	if becomeBlk != nil {
		cfg, err := decodeBecomeConfig(becomeBlk)
		if err != nil {
			return HostsGroup{}, fmt.Errorf("hosts_group %q: become block: %w", name, err)
		}
		hg.Become = &cfg
	}

	return hg, nil
}

func decodeSSHConfig(blk *hclsyntax.Block) (SSHConfig, error) {
	attrs := blk.Body.Attributes
	var cfg SSHConfig
	err := firstErr(
		func() (err error) { cfg.User, _, err = attrString(attrs, "user"); return },
		func() (err error) { cfg.Port, _, err = attrInt(attrs, "port"); return },
		func() (err error) { cfg.Password, _, err = attrString(attrs, "password"); return },
		func() (err error) { cfg.PrivateKey, _, err = attrString(attrs, "private-key"); return },
		func() (err error) { cfg.PrivateKeyPassphrase, _, err = attrString(attrs, "passphrase"); return },
		func() (err error) { cfg.HostKeyCheck, _, err = attrBool(attrs, "host-key-check"); return },
		func() (err error) { cfg.KnownHostsFile, _, err = attrString(attrs, "known-hosts"); return },
		func() (err error) { cfg.TTY, _, err = attrBool(attrs, "tty"); return },
		func() (err error) { cfg.TempDir, _, err = attrString(attrs, "tmpdir"); return },
		func() (err error) { cfg.ConnectTimeout, _, err = attrInt(attrs, "connect-timeout"); return },
	)
	if err != nil {
		return cfg, err
	}
	if err := checkUnknownAttrs(attrs, "user", "port", "password", "private-key",
		"passphrase", "host-key-check", "known-hosts", "tty", "tmpdir", "connect-timeout"); err != nil {
		return cfg, err
	}
	if err := checkUnknownBlocks(blk.Body.Blocks); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func decodeWinRMConfig(blk *hclsyntax.Block) (WinRMConfig, error) {
	attrs := blk.Body.Attributes
	var cfg WinRMConfig
	err := firstErr(
		func() (err error) { cfg.User, _, err = attrString(attrs, "user"); return },
		func() (err error) { cfg.Port, _, err = attrInt(attrs, "port"); return },
		func() (err error) { cfg.Password, _, err = attrString(attrs, "password"); return },
		func() (err error) { cfg.SSL, _, err = attrBool(attrs, "ssl"); return },
		func() (err error) { cfg.SSLVerify, _, err = attrBool(attrs, "ssl-verify"); return },
		func() (err error) { cfg.CACert, _, err = attrString(attrs, "ca-cert"); return },
		func() (err error) { cfg.Transport, _, err = attrString(attrs, "transport"); return },
		func() (err error) { cfg.ClientCert, _, err = attrString(attrs, "client-cert"); return },
		func() (err error) { cfg.ClientKey, _, err = attrString(attrs, "client-key"); return },
		func() (err error) { cfg.ConnectTimeout, _, err = attrInt(attrs, "connect-timeout"); return },
		func() (err error) { cfg.TempDir, _, err = attrString(attrs, "tmpdir"); return },
		func() (err error) { cfg.Path, _, err = attrString(attrs, "path"); return },
	)
	if err != nil {
		return cfg, err
	}
	if err := checkUnknownAttrs(attrs, "user", "port", "password", "ssl", "ssl-verify",
		"ca-cert", "transport", "client-cert", "client-key", "connect-timeout", "tmpdir", "path"); err != nil {
		return cfg, err
	}
	if err := checkUnknownBlocks(blk.Body.Blocks); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func decodeBecomeConfig(blk *hclsyntax.Block) (BecomeConfig, error) {
	attrs := blk.Body.Attributes
	var cfg BecomeConfig
	err := firstErr(
		func() (err error) { cfg.Method, _, err = attrString(attrs, "method"); return },
		func() (err error) { cfg.User, _, err = attrString(attrs, "user"); return },
		func() (err error) { cfg.Password, _, err = attrString(attrs, "password"); return },
	)
	if err != nil {
		return cfg, err
	}
	if err := checkUnknownAttrs(attrs, "method", "user", "password"); err != nil {
		return cfg, err
	}
	if err := checkUnknownBlocks(blk.Body.Blocks); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func decodeCommand(blk *hclsyntax.Block) (Command, error) {
	name, err := blockLabel(blk)
	if err != nil {
		return Command{}, err
	}
	attrs := blk.Body.Attributes

	cmd := Command{Name: name}
	var hasRun, hasLocal bool
	err = firstErr(
		func() (err error) { cmd.Desc, _, err = attrString(attrs, "desc"); return },
		func() (err error) { cmd.Run, hasRun, err = attrString(attrs, "run"); return },
		func() (err error) { cmd.Local, hasLocal, err = attrString(attrs, "local"); return },
		func() (err error) { cmd.Serial, _, err = attrInt(attrs, "serial"); return },
		func() (err error) { cmd.Once, _, err = attrBool(attrs, "once"); return },
		func() (err error) { cmd.Interactive, _, err = attrBool(attrs, "interactive"); return },
	)
	if err != nil {
		return Command{}, fmt.Errorf("command %q: %w", name, err)
	}
	if err := checkUnknownAttrs(attrs, "desc", "run", "local", "serial", "once", "interactive"); err != nil {
		return Command{}, fmt.Errorf("command %q: %w", name, err)
	}
	if err := checkUnknownBlocks(blk.Body.Blocks, "upload"); err != nil {
		return Command{}, fmt.Errorf("command %q: %w", name, err)
	}

	uploadBlk, err := soleBlock(blk.Body.Blocks, "upload")
	if err != nil {
		return Command{}, fmt.Errorf("command %q: %w", name, err)
	}
	hasUpload := uploadBlk != nil
	if hasUpload {
		up, err := decodeUpload(uploadBlk)
		if err != nil {
			return Command{}, fmt.Errorf("command %q: upload block: %w", name, err)
		}
		cmd.Upload = &up
	}

	switch n := boolCount(hasRun, hasLocal, hasUpload); {
	case n == 0:
		return Command{}, fmt.Errorf("command %q: exactly one of \"run\", \"local\", or an \"upload\" block is required", name)
	case n > 1:
		return Command{}, fmt.Errorf("command %q: \"run\", \"local\", and \"upload\" are mutually exclusive", name)
	}
	if hasLocal && (cmd.Serial != 0 || cmd.Once) {
		return Command{}, fmt.Errorf("command %q: \"serial\"/\"once\" apply only to \"run\"/\"upload\" commands, not \"local\"", name)
	}
	if cmd.Interactive && !hasRun {
		return Command{}, fmt.Errorf("command %q: \"interactive\" is only valid alongside \"run\" (not \"local\" or an \"upload\" block)", name)
	}
	if cmd.Interactive && (cmd.Serial != 0 || cmd.Once) {
		return Command{}, fmt.Errorf("command %q: \"interactive\" is mutually exclusive with \"serial\"/\"once\" (an interactive command always runs on every resolved host at once)", name)
	}

	return cmd, nil
}

func boolCount(bs ...bool) int {
	n := 0
	for _, b := range bs {
		if b {
			n++
		}
	}
	return n
}

// decodeUpload decodes an upload{} block's own attributes. Its caller
// (decodeCommand, via soleBlock) has already rejected a labeled or
// duplicate upload block, so this need not re-check that.
func decodeUpload(blk *hclsyntax.Block) (Upload, error) {
	attrs := blk.Body.Attributes
	var up Upload
	var hasSrc, hasDst bool
	err := firstErr(
		func() (err error) { up.Src, hasSrc, err = attrString(attrs, "src"); return },
		func() (err error) { up.Dst, hasDst, err = attrString(attrs, "dst"); return },
		func() (err error) { up.Executable, _, err = attrBool(attrs, "executable"); return },
	)
	if err != nil {
		return Upload{}, err
	}
	if !hasSrc {
		return Upload{}, fmt.Errorf("%s: \"src\" is required", blk.TypeRange)
	}
	if !hasDst {
		return Upload{}, fmt.Errorf("%s: \"dst\" is required", blk.TypeRange)
	}
	if err := checkUnknownAttrs(attrs, "src", "dst", "executable"); err != nil {
		return Upload{}, err
	}
	if err := checkUnknownBlocks(blk.Body.Blocks); err != nil {
		return Upload{}, err
	}
	return up, nil
}

func decodeTarget(blk *hclsyntax.Block) (Target, error) {
	name, err := blockLabel(blk)
	if err != nil {
		return Target{}, err
	}
	attrs := blk.Body.Attributes
	commands, has, err := attrStringList(attrs, "commands")
	if err != nil {
		return Target{}, fmt.Errorf("target %q: %w", name, err)
	}
	if !has {
		return Target{}, fmt.Errorf("target %q: \"commands\" is required", name)
	}
	if len(commands) == 0 {
		return Target{}, fmt.Errorf("target %q: \"commands\" must not be empty", name)
	}
	if err := checkUnknownAttrs(attrs, "commands"); err != nil {
		return Target{}, fmt.Errorf("target %q: %w", name, err)
	}
	if err := checkUnknownBlocks(blk.Body.Blocks); err != nil {
		return Target{}, fmt.Errorf("target %q: %w", name, err)
	}
	return Target{Name: name, Commands: commands}, nil
}
