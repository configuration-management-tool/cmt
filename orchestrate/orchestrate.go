// SPDX-License-Identifier: BSD-3-Clause
//
// Copyright (c) 2026, the configuration-management-tool/cmt authors

// Package orchestrate fans a manifest target's commands out across a
// hosts_group's resolved hosts: bounded concurrency (serial), run-once
// (once), sequential fail-fast target execution, env rendering, and
// output prefixing.
//
// It depends only on the remoteexec.Connection interface and a caller
// supplied DialFunc — never on package connect directly — so it can be
// exercised in tests with a fake in-memory Connection and no network or
// subprocess involved.
package orchestrate

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"sync"

	remoteexec "github.com/go-remoteexec/transport"

	"github.com/configuration-management-tool/cmt/manifest"
)

// DialFunc resolves one host specification (a hosts_group's host string,
// a dynamic-inventory line, or LocalHost) into a live connection. It has
// the same shape as connect.Dial; production code wires that in, tests
// inject a fake.
type DialFunc func(ctx context.Context, hostSpec string, group manifest.HostsGroup) (remoteexec.Connection, error)

// InventoryFunc runs a hosts_group's dynamic `inventory` command and
// returns its resolved host list, one per non-empty output line.
// Production code defaults to runLocalInventory (below); tests inject a
// fake to stay hermetic.
type InventoryFunc func(ctx context.Context, cmd string) ([]string, error)

// LocalHost is the pseudo-host DialFunc receives for every `local`
// command and for resolving dynamic inventory — see connect.LocalHost,
// which this deliberately matches so wiring Dial = connect.Dial "just
// works".
const LocalHost = "localhost"

// HostResult is the outcome of running one command on one host (or, for
// a `local` command, the single synthetic LocalHost "host").
type HostResult struct {
	Host string
	Cmd  string // the command's manifest name
	Kind string // "run", "local", or "upload"
	Out  remoteexec.Result
	Err  error // set on dial/exec/put failure; nil even if Out.RC != 0
}

// Failed reports whether this host's step should be treated as a
// failure: a transport error, or (for run/local) a non-zero exit code.
// Upload has no exit code, so only a transport error fails it.
func (r HostResult) Failed() bool {
	if r.Err != nil {
		return true
	}
	if r.Kind == "upload" {
		return false
	}
	return r.Out.RC != 0
}

// Options configures one Runner.Run invocation.
type Options struct {
	// EnvOverrides are applied last (highest priority), e.g. from the
	// CLI's -e/--env flags.
	EnvOverrides map[string]string

	// Only, when set, keeps only resolved hosts whose name matches.
	Only *regexp.Regexp
	// Except, when set, drops resolved hosts whose name matches.
	Except *regexp.Regexp

	// DisablePrefix turns off the "[host] " output line prefix.
	DisablePrefix bool

	// StdinData, when non-nil, is streamed as stdin to every `run`
	// command's Exec call (a fresh reader per host, since a stream can
	// only be consumed once — see the cmd/cmt package for how it is
	// populated from a piped, non-TTY stdin).
	StdinData []byte

	// Stdout and Stderr receive prefixed per-host command output.
	// Default to io.Discard if nil.
	Stdout, Stderr io.Writer
}

// Runner executes manifest commands and targets against resolved hosts.
type Runner struct {
	Manifest *manifest.Manifest

	// Dial resolves a host specification to a connection. Required.
	Dial DialFunc

	// Inventory resolves a hosts_group's dynamic `inventory` command.
	// Defaults to RunLocalInventory if nil.
	Inventory InventoryFunc
}

// Run resolves names (command and/or target names — a target name
// expands to its command list) and runs them, in order, against
// groupName's hosts_group, stopping at the first command with any
// failed host (fail-fast). It returns every HostResult produced before
// stopping, and a non-nil error describing the first failure (a dial
// error, an unknown name, or a failed host).
func (r *Runner) Run(ctx context.Context, groupName string, names []string, opts Options) ([]HostResult, error) {
	if r.Dial == nil {
		return nil, fmt.Errorf("orchestrate: Runner.Dial is required")
	}
	group, ok := r.Manifest.HostsGroups[groupName]
	if !ok {
		return nil, fmt.Errorf("orchestrate: unknown hosts_group %q", groupName)
	}
	cmdNames, err := r.Manifest.ExpandCommands(names)
	if err != nil {
		return nil, err
	}

	hosts, err := r.resolveHosts(ctx, group, opts)
	if err != nil {
		return nil, err
	}

	env := mergeEnv(r.Manifest.Env, group.Env, opts.EnvOverrides)
	// Fanned-out hosts write to the same Stdout/Stderr concurrently
	// (bounded only by a command's `serial`, unlimited by default), so
	// both need a writer safe for concurrent use — an io.Writer
	// generally is not (e.g. *bytes.Buffer, or even *os.File writing
	// more than fits one syscall).
	out := syncWriterOrDiscard(opts.Stdout)
	errOut := syncWriterOrDiscard(opts.Stderr)

	var all []HostResult
	for _, cmdName := range cmdNames {
		cmd := r.Manifest.Commands[cmdName]
		results := r.runCommand(ctx, groupName, cmd, hosts, env, opts, out, errOut)
		all = append(all, results...)
		if failed := firstFailure(results); failed != nil {
			return all, fmt.Errorf("orchestrate: command %q failed on host %q: %s", cmdName, failed.Host, failureMessage(*failed))
		}
	}
	return all, nil
}

func failureMessage(r HostResult) string {
	if r.Err != nil {
		return r.Err.Error()
	}
	return fmt.Sprintf("exit code %d", r.Out.RC)
}

func firstFailure(results []HostResult) *HostResult {
	for i := range results {
		if results[i].Failed() {
			return &results[i]
		}
	}
	return nil
}

// runCommand runs one command across hosts (or once, locally) and
// returns every HostResult.
func (r *Runner) runCommand(ctx context.Context, groupName string, cmd manifest.Command, hosts []string, env map[string]string, opts Options, out, errOut io.Writer) []HostResult {
	switch {
	case cmd.Local != "":
		res := r.execOne(ctx, LocalHost, manifest.HostsGroup{}, "local:"+cmd.Name, "local", cmd.Local, groupName, env, opts, out, errOut)
		return []HostResult{res}

	case cmd.Upload != nil:
		targets := selectHosts(hosts, cmd.Once)
		return r.fanOut(ctx, groupName, cmd, targets, env, opts, out, errOut, func(ctx context.Context, host string, group manifest.HostsGroup) HostResult {
			return r.uploadOne(ctx, host, group, cmd, out)
		})

	default: // cmd.Run != ""
		targets := selectHosts(hosts, cmd.Once)
		return r.fanOut(ctx, groupName, cmd, targets, env, opts, out, errOut, func(ctx context.Context, host string, group manifest.HostsGroup) HostResult {
			return r.execOne(ctx, host, group, cmd.Name, "run", cmd.Run, groupName, env, opts, out, errOut)
		})
	}
}

// selectHosts applies `once` (keep only the first host).
func selectHosts(hosts []string, once bool) []string {
	if once && len(hosts) > 1 {
		return hosts[:1]
	}
	return hosts
}

// fanOut runs fn across targets with concurrency bounded by cmd.Serial
// (0 = unlimited).
func (r *Runner) fanOut(ctx context.Context, groupName string, cmd manifest.Command, targets []string, env map[string]string, opts Options, out, errOut io.Writer, fn func(context.Context, string, manifest.HostsGroup) HostResult) []HostResult {
	group := r.Manifest.HostsGroups[groupName]
	results := make([]HostResult, len(targets))
	limit := cmd.Serial
	if limit <= 0 || limit > len(targets) {
		limit = len(targets)
	}
	if limit == 0 {
		return results
	}

	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i, host := range targets {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, host string) {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = fn(ctx, host, group)
		}(i, host)
	}
	wg.Wait()
	return results
}

func (r *Runner) execOne(ctx context.Context, host string, group manifest.HostsGroup, cmdName, kind, rawCmd, groupName string, env map[string]string, opts Options, out, errOut io.Writer) HostResult {
	conn, err := r.Dial(ctx, host, group)
	if err != nil {
		return HostResult{Host: host, Cmd: cmdName, Kind: kind, Err: err}
	}
	defer conn.Close()

	user := parseHostUser(host)
	full := renderEnv(withBuiltins(env, groupName, host, user)) + rawCmd

	var stdin io.Reader
	if opts.StdinData != nil {
		stdin = strings.NewReader(string(opts.StdinData))
	}
	res, err := conn.Exec(ctx, full, stdin)
	hr := HostResult{Host: host, Cmd: cmdName, Kind: kind, Out: res, Err: err}
	writePrefixed(out, host, res.Stdout, opts.DisablePrefix)
	writePrefixed(errOut, host, res.Stderr, opts.DisablePrefix)
	return hr
}

func (r *Runner) uploadOne(ctx context.Context, host string, group manifest.HostsGroup, cmd manifest.Command, out io.Writer) HostResult {
	conn, err := r.Dial(ctx, host, group)
	if err != nil {
		return HostResult{Host: host, Cmd: cmd.Name, Kind: "upload", Err: err}
	}
	defer conn.Close()

	err = conn.Put(ctx, cmd.Upload.Src, cmd.Upload.Dst, remoteexec.PutOptions{
		Executable:   cmd.Upload.Executable,
		MkdirParents: true,
	})
	if err == nil {
		writePrefixed(out, host, fmt.Sprintf("uploaded %s to %s\n", cmd.Upload.Src, cmd.Upload.Dst), false)
	}
	return HostResult{Host: host, Cmd: cmd.Name, Kind: "upload", Err: err}
}

// resolveHosts resolves group's static or dynamic host list, then
// applies opts.Only/Except.
func (r *Runner) resolveHosts(ctx context.Context, group manifest.HostsGroup, opts Options) ([]string, error) {
	var hosts []string
	if group.Inventory != "" {
		inv := r.Inventory
		if inv == nil {
			inv = RunLocalInventory
		}
		resolved, err := inv(ctx, group.Inventory)
		if err != nil {
			return nil, fmt.Errorf("orchestrate: hosts_group %q: resolving inventory: %w", group.Name, err)
		}
		hosts = resolved
	} else {
		hosts = append(hosts, group.Hosts...)
	}

	out := hosts[:0:0]
	for _, h := range hosts {
		if opts.Only != nil && !opts.Only.MatchString(h) {
			continue
		}
		if opts.Except != nil && opts.Except.MatchString(h) {
			continue
		}
		out = append(out, h)
	}
	return out, nil
}

// localInventoryExec runs cmd through transport.NewLocal(). It is a
// variable, rather than inlined into RunLocalInventory, purely so a test
// can substitute a transport-error outcome (a real local shell has no
// portable way to force one) — production code never reassigns it.
var localInventoryExec = func(ctx context.Context, cmd string) (remoteexec.Result, error) {
	local := remoteexec.NewLocal()
	defer local.Close()
	return local.Exec(ctx, cmd, nil)
}

// RunLocalInventory is the default InventoryFunc: it runs cmd through a
// local shell (transport.NewLocal()) and splits its stdout into
// non-empty, trimmed lines.
func RunLocalInventory(ctx context.Context, cmd string) ([]string, error) {
	res, err := localInventoryExec(ctx, cmd)
	if err != nil {
		return nil, err
	}
	if res.RC != 0 {
		return nil, fmt.Errorf("inventory command exited %d: %s", res.RC, strings.TrimSpace(res.Stderr))
	}
	var hosts []string
	sc := bufio.NewScanner(strings.NewReader(res.Stdout))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" {
			hosts = append(hosts, line)
		}
	}
	return hosts, nil
}

// mergeEnv layers global < group < overrides, later keys winning.
func mergeEnv(layers ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, l := range layers {
		for k, v := range l {
			out[k] = v
		}
	}
	return out
}

// withBuiltins returns a copy of env with cmt's builtin CMT_* variables
// added (these always win over a same-named manifest env var, so a
// manifest cannot accidentally shadow them).
func withBuiltins(env map[string]string, groupName, host, user string) map[string]string {
	out := make(map[string]string, len(env)+3)
	for k, v := range env {
		out[k] = v
	}
	out["CMT_HOSTS_GROUP"] = groupName
	out["CMT_HOST"] = host
	out["CMT_USER"] = user
	return out
}

// renderEnv renders env deterministically as a shell `KEY='VAL' ...`
// prefix (empty string if env is empty).
func renderEnv(env map[string]string) string {
	if len(env) == 0 {
		return ""
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(shellQuote(env[k]))
		b.WriteByte(' ')
	}
	return b.String()
}

// syncWriter serializes concurrent writes to an underlying io.Writer
// that may not be safe for concurrent use on its own.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// syncWriterOrDiscard wraps w for safe concurrent use, or returns
// io.Discard if w is nil.
func syncWriterOrDiscard(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return &syncWriter{w: w}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// parseHostUser extracts the user portion of a "user@host[:port]" host
// spec, without erroring (an orchestrate-local, minimal duplicate of
// connect.ParseHost's user-splitting kept deliberately tiny so this
// package never imports connect).
func parseHostUser(hostSpec string) (user string) {
	if i := strings.Index(hostSpec, "@"); i >= 0 {
		return hostSpec[:i]
	}
	return ""
}

// writePrefixed writes text to w, prefixing every non-empty line with
// "[host] " unless disabled. A no-op for empty text.
func writePrefixed(w io.Writer, host, text string, disablePrefix bool) {
	if text == "" {
		return
	}
	if disablePrefix {
		io.WriteString(w, text)
		return
	}
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	for _, line := range lines {
		fmt.Fprintf(w, "[%s] %s\n", host, line)
	}
}
