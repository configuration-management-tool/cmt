// SPDX-License-Identifier: BSD-3-Clause
//
// Copyright (c) 2026, the configuration-management-tool/cmt authors

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
	"strings"

	"github.com/configuration-management-tool/cmt/interactive"
	"github.com/configuration-management-tool/cmt/manifest"
	"github.com/configuration-management-tool/cmt/orchestrate"
)

// version is cmt's own release version (not the manifest schema
// version — there is only one).
const version = "0.1.0"

const usageText = `cmt [OPTIONS] HOSTS_GROUP COMMAND [COMMAND2 ...]

cmt runs named shell commands, from an HCL2 manifest, against a named
group of hosts (a "hosts_group") in parallel. A COMMAND that matches a
target expands to that target's command list.

Options:
  -f FILE            manifest path (default "cmt.hcl")
  -e, --env KEY=VAL   set/override a manifest env var (repeatable)
  --only REGEXP       keep only resolved hosts matching REGEXP
  --except REGEXP     drop resolved hosts matching REGEXP
  -D, --debug         print a per-host result summary after each command
  --disable-prefix     do not prefix output lines with "[host] "
  --version            print the version and exit
  -h, --help           print this help and exit
`

// envList collects repeated -e/--env KEY=VAL flag values.
type envList []string

func (e *envList) String() string { return strings.Join(*e, ",") }
func (e *envList) Set(v string) error {
	*e = append(*e, v)
	return nil
}

func (e envList) toMap() (map[string]string, error) {
	out := make(map[string]string, len(e))
	for _, kv := range e {
		i := strings.IndexByte(kv, '=')
		if i < 0 {
			return nil, fmt.Errorf("invalid -e/--env value %q: want KEY=VAL", kv)
		}
		out[kv[:i]] = kv[i+1:]
	}
	return out, nil
}

// run is cmt's testable entry point: it parses args, loads the
// manifest, and executes the requested commands, writing output to
// stdout/stderr and reading piped input from stdin. It returns the
// process exit code.
func run(args []string, stdout, stderr io.Writer, stdin io.Reader, dial orchestrate.DialFunc) int {
	fs := flag.NewFlagSet("cmt", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usageText) }

	manifestPath := fs.String("f", "cmt.hcl", "manifest path")
	var envs envList
	fs.Var(&envs, "e", "set/override a manifest env var KEY=VAL (repeatable)")
	fs.Var(&envs, "env", "set/override a manifest env var KEY=VAL (repeatable)")
	onlyPattern := fs.String("only", "", "keep only resolved hosts matching this regexp")
	exceptPattern := fs.String("except", "", "drop resolved hosts matching this regexp")
	var debug bool
	fs.BoolVar(&debug, "D", false, "print a per-host result summary after each command")
	fs.BoolVar(&debug, "debug", false, "print a per-host result summary after each command")
	disablePrefix := fs.Bool("disable-prefix", false, `do not prefix output lines with "[host] "`)
	showVersion := fs.Bool("version", false, "print the version and exit")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	if *showVersion {
		fmt.Fprintf(stdout, "cmt %s\n", version)
		return 0
	}

	positional := fs.Args()
	if len(positional) < 2 {
		fs.Usage()
		return 2
	}
	hostsGroup, commands := positional[0], positional[1:]

	envOverrides, err := envs.toMap()
	if err != nil {
		fmt.Fprintln(stderr, "cmt:", err)
		return 2
	}

	var onlyRe, exceptRe *regexp.Regexp
	if *onlyPattern != "" {
		onlyRe, err = regexp.Compile(*onlyPattern)
		if err != nil {
			fmt.Fprintln(stderr, "cmt: --only:", err)
			return 2
		}
	}
	if *exceptPattern != "" {
		exceptRe, err = regexp.Compile(*exceptPattern)
		if err != nil {
			fmt.Fprintln(stderr, "cmt: --except:", err)
			return 2
		}
	}

	m, err := manifest.ParseFile(*manifestPath)
	if err != nil {
		fmt.Fprintln(stderr, "cmt:", err)
		return 1
	}

	// ExpandCommands is called here (and, redundantly but harmlessly,
	// again inside orchestrate.Runner.Run below for the non-interactive
	// path) purely so an interactive=true command can be detected and
	// routed to package interactive before any buffered-path machinery
	// runs — see detectInteractiveCommand.
	cmdNames, err := m.ExpandCommands(commands)
	if err != nil {
		fmt.Fprintln(stderr, "cmt:", err)
		return 1
	}
	interactiveCmd, isInteractive, err := detectInteractiveCommand(m, cmdNames)
	if err != nil {
		fmt.Fprintln(stderr, "cmt:", err)
		return 1
	}
	if isInteractive {
		return runInteractive(m, hostsGroup, interactiveCmd, envOverrides, onlyRe, exceptRe, *disablePrefix, stdout, stderr, stdin)
	}

	isTTY := false
	if f, ok := stdin.(*os.File); ok {
		isTTY = isTerminal(f)
	}

	opts := orchestrate.Options{
		EnvOverrides:  envOverrides,
		Only:          onlyRe,
		Except:        exceptRe,
		DisablePrefix: *disablePrefix,
		StdinData:     readPipedStdin(stdin, isTTY),
		Stdout:        stdout,
		Stderr:        stderr,
	}

	runner := &orchestrate.Runner{Manifest: m, Dial: dial}
	results, runErr := runner.Run(context.Background(), hostsGroup, commands, opts)

	if debug {
		for _, r := range results {
			fmt.Fprintf(stderr, "[debug] host=%s cmd=%s kind=%s rc=%d err=%v\n", r.Host, r.Cmd, r.Kind, r.Out.RC, r.Err)
		}
	}

	if runErr != nil {
		fmt.Fprintln(stderr, "cmt:", runErr)
		return 1
	}
	return 0
}

// detectInteractiveCommand looks for an interactive=true command among
// cmdNames (already target-expanded — manifest.Manifest.Validate
// already rejects an interactive command referenced *inside* a target,
// so the only way one appears here is a direct `cmt group cmdname` on
// the command line). It is an error for an interactive command to be
// combined with any other command on the same invocation: a live
// keystroke-forwarding session is inherently a single, standalone,
// blocking thing — there is only one local stdin to give it, and
// nothing sensible to buffer-run before or after it in the same
// process.
func detectInteractiveCommand(m *manifest.Manifest, cmdNames []string) (name string, isInteractive bool, err error) {
	var found []string
	for _, n := range cmdNames {
		if cmd, ok := m.Commands[n]; ok && cmd.Interactive {
			found = append(found, n)
		}
	}
	if len(found) == 0 {
		return "", false, nil
	}
	if len(cmdNames) != 1 {
		return "", false, fmt.Errorf("interactive command %q must be invoked alone (cmt HOSTS_GROUP %s), not combined with other commands", found[0], found[0])
	}
	return found[0], true, nil
}

// runInteractive drives one interactive=true command through package
// interactive instead of the buffered orchestrate path. It wires
// SIGINT (via signal.NotifyContext) to the context passed into
// interactive.Runner.Run, so Ctrl-C cleanly force-closes every open
// session rather than leaving cmt hung waiting on a live command.
func runInteractive(m *manifest.Manifest, hostsGroup, cmdName string, envOverrides map[string]string, onlyRe, exceptRe *regexp.Regexp, disablePrefix bool, stdout, stderr io.Writer, stdin io.Reader) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	runner := &interactive.Runner{Manifest: m}
	_, runErr := runner.Run(ctx, hostsGroup, cmdName, interactive.Options{
		EnvOverrides:  envOverrides,
		Only:          onlyRe,
		Except:        exceptRe,
		DisablePrefix: disablePrefix,
		Stdin:         stdin,
		Stdout:        stdout,
		Stderr:        stderr,
	})
	if runErr != nil {
		fmt.Fprintln(stderr, "cmt:", runErr)
		return 1
	}
	return 0
}

// readPipedStdin reads stdin fully when isTTY is false, so its content
// can be replayed (via a fresh reader) to every host a `run` command
// reaches. It returns nil when stdin is a terminal (interactive use, no
// piped input to forward) or empty.
func readPipedStdin(stdin io.Reader, isTTY bool) []byte {
	if isTTY {
		return nil
	}
	data, err := io.ReadAll(stdin)
	if err != nil || len(data) == 0 {
		return nil
	}
	return data
}

// isTerminal reports whether f is an interactive character device. A
// Stat failure is treated as "not a terminal" (safe default: attempt to
// read piped input rather than silently drop it).
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
