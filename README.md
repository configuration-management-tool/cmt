# cmt

`cmt` is a super-simple deployment tool: it runs named shell commands, defined
in a manifest, against named groups of hosts in parallel. It is a from-scratch,
pure-Go (`CGO_ENABLED=0`) reimplementation of the idea behind
[pressly/sup](https://github.com/pressly/sup) ("Stack Up") — full credit to
that project for the design this borrows — but `cmt` is **not** Supfile-compatible.
The manifest language is [HCL2](https://github.com/hashicorp/hcl) instead of
YAML, a group of hosts is called a `hosts_group` instead of a `network` (to
avoid the networking connotation), and connections (SSH, WinRM, local exec,
privilege escalation) are handled by the shared library
[`github.com/go-remoteexec/transport`](https://github.com/go-remoteexec/transport)
instead of a bespoke implementation — with one deliberate exception, package
`interactive`'s live SSH sessions; see
[Architecture: buffered vs. streaming execution](#architecture-buffered-vs-streaming-execution)
below.

## Install

```sh
go install github.com/configuration-management-tool/cmt/cmd/cmt@latest
```

or build from a clone of this repository:

```sh
git clone https://github.com/configuration-management-tool/cmt
cd cmt
go build -o cmt ./cmd/cmt
```

## Manifest

By default `cmt` reads `cmt.hcl` in the current directory (override with `-f`).

```hcl
env {
  NAME  = "api"
  IMAGE = "example/api"
}

hosts_group "production" {
  hosts = ["api1.example.com", "api2.example.com"]
  env = { DEPLOY_ENV = "prod" }
}

hosts_group "staging" {
  inventory = "curl http://example.com/latest/meta-data/hostname"
}

command "restart" {
  desc   = "Restart example container"
  run    = "sudo docker restart example"
  serial = 2
}

command "build" {
  desc = "Build image"
  run  = "sudo docker build -t image:latest ."
  once = true
}

command "prepare" {
  desc  = "Prepare to upload"
  local = "npm run build"
}

command "upload" {
  desc = "Upload dist files"
  upload {
    src = "./dist"
    dst = "/tmp/"
  }
}

command "shell" {
  desc        = "Interactive shell on all hosts"
  run         = "bash"
  interactive = true
}

target "deploy" {
  commands = ["build", "prepare", "upload", "restart"]
}
```

### Blocks

- **`env { ... }`** — global environment variables, exported (as `KEY='VAL'`
  shell-quoted assignments prefixed onto the command line) for every command.
  A `hosts_group`'s own `env` overrides these for its hosts; `-e`/`--env` on
  the CLI overrides both.

  Because these are a *shell prefix* (`NAME='api' ... your-command`, all one
  line), they only reach a **child process's** environment — ordinary POSIX
  shell semantics, not a `cmt` quirk. `run = "echo hello $NAME"` prints
  nothing: `$NAME` is expanded while the outer shell parses the line,
  before the assignment takes effect. To use a value inside the same
  command line, defer expansion to a nested shell: `run = "sh -c 'echo
  hello $NAME'"` (note the single quotes, so the *outer* parse leaves
  `$NAME` untouched). A script file that reads `$NAME` normally, invoked
  via `run = "./deploy.sh"`, is unaffected — this only bites literal
  variable references written directly on the `run =`/`local =` line.
- **`hosts_group "name" { ... }`** — a named set of hosts. Either:
  - `hosts = [...]` — a static list. Each entry is `host`, `user@host`, or
    `user@host:port`. The literal hosts `localhost`, `127.0.0.1`, and `::1`
    always resolve to a local (no-network) connection, regardless of any
    `ssh`/`winrm`/`become` block.
  - `inventory = "shell command"` — a dynamic list: the command runs locally
    and its stdout, one host per non-empty line, becomes the host list.

  `hosts` and `inventory` are mutually exclusive; exactly one is required.

  Optional nested blocks configure how `cmt` connects to this group's hosts
  (all fields optional; see [`manifest/manifest.go`](manifest/manifest.go)
  for the authoritative field list):

  - `ssh { user, port, password, private-key, passphrase, host-key-check,
    known-hosts, tty, tmpdir, connect-timeout }` — SSH is the default
    transport for any non-local host, even with no `ssh` block at all.
  - `winrm { user, port, password, ssl, ssl-verify, ca-cert, transport,
    client-cert, client-key, connect-timeout, tmpdir, path }` — used only
    when a `winrm` block is present (a deliberate value-add over sup, which
    has no Windows transport; free here since
    `go-remoteexec/transport` already speaks WS-Management).
  - `become { method, user, password }` — privilege escalation
    (`method` is `sudo` (default), `su`, or `doas`).

- **`command "name" { ... }`** — exactly one of:
  - `run = "shell command"` — runs on every resolved host of the
    `hosts_group` the command is invoked against.
  - `local = "shell command"` — runs once, locally, regardless of the
    `hosts_group` (sup's `local` semantics — this is *not* per host).
  - `upload { src, dst, executable? }` — copies `src` to `dst` on every
    resolved host (`executable` chmods the remote file `+x`; POSIX hosts
    only, ignored under WinRM).

  Optional: `desc` (documentation only), `serial = N` (cap concurrent hosts;
  0/unset = unlimited fan-out), `once = true` (run on only the first
  resolved host — `serial`/`once` apply only to `run`/`upload`, not `local`),
  `interactive = true` — see below.

  **`interactive = true`** changes *how* a `run` command is executed:
  instead of the buffered "run to completion, then return output" path,
  `cmt` opens a live session to every resolved host at once and forwards
  `cmt`'s own stdin to all of them keystroke-by-keystroke, live — sup's
  `stdin: true` (see [Interactive sessions](#interactive-sessions) below).
  Validated at parse time:
  - Only valid alongside `run` — a `local` or `upload` command has no
    interactive equivalent, and setting `interactive = true` on either is
    a manifest error.
  - Mutually exclusive with `serial`/`once` — an interactive command
    always addresses every resolved host at once; there is no meaningful
    way to run it "serially" or "once" and still call it a live
    multi-host session.
  - An interactive command referenced from inside a `target`'s `commands`
    list is a manifest error — sup's `stdin: true` mode is invoked
    standalone (`cmt HOSTS_GROUP COMMANDNAME`), not chained into a
    fail-fast sequence of other commands.

  The CLI adds one more rule, checked at run time rather than parse time
  (a manifest alone can't know what a given invocation combines): naming
  an interactive command alongside any other command on the same command
  line (`cmt group shell other`) is also rejected — there is only one
  local stdin to give a live session, and nothing sensible to
  buffer-run before or after it in the same process.

- **`target "name" { commands = [...] }`** — a named, ordered list of command
  names. Commands run in sequence; if any host fails a command (a transport
  error, or a nonzero exit code for `run`/`local`), the remaining commands in
  the target do not run (fail-fast, matching sup). An `interactive = true`
  command cannot appear in this list — see above.

## CLI usage

```
cmt [OPTIONS] HOSTS_GROUP COMMAND [COMMAND2 ...]
```

A `COMMAND` that names a `target` expands to that target's command list.

| Flag | Description |
|---|---|
| `-f FILE` | manifest path (default `cmt.hcl`) |
| `-e`, `--env KEY=VAL` | set/override a manifest env var (repeatable) |
| `--only REGEXP` | keep only resolved hosts matching `REGEXP` |
| `--except REGEXP` | drop resolved hosts matching `REGEXP` |
| `-D`, `--debug` | print a per-host result summary after each command |
| `--disable-prefix` | don't prefix output lines with `[host] ` |
| `--version` | print the version and exit |
| `-h`, `--help` | print usage and exit |

```sh
cmt production build prepare upload restart
cmt production deploy                        # target expands to its commands
cmt production restart --only 'api1\.'        # only api1.example.com
echo 'some input' | cmt production restart    # piped stdin reaches every host's command
```

### Piped stdin

When `cmt`'s own stdin is not a terminal, it is read fully and replayed (a
fresh copy per host) as the standard input of every `run` command's remote
execution — matching `echo cmd | sup group cmd` in upstream sup. This is
the *non*-interactive path (package `orchestrate`); see below for
`interactive = true` commands, which read stdin live instead.

### Interactive sessions

`cmt group commandname`, where `commandname` names an `interactive = true`
command, opens a live session to every host `group` resolves to, all at
once: keystrokes typed at `cmt`'s own stdin are forwarded live to every
host's remote (or local) process as they're typed, and every host's
stdout/stderr streams back live, interleaved and prefixed `[host] ` as it's
produced — not buffered until the command exits.

```sh
cmt production shell     # bash, live, on every host production resolves to
```

The session for a given host ends when that host's process exits (its exit
code is recorded; `cmt` stops forwarding it further stdin), or when `cmt`'s
own stdin reaches EOF (every host's remote stdin is closed in turn, and
`cmt` then waits for each process to actually exit in response — closing
stdin is a signal, not a kill), or on Ctrl-C (every open session is
force-closed immediately). Once every host's session has ended, `cmt`
prints a per-host summary (exit code, or "still running" for a host that
was still live when Ctrl-C force-closed it) and exits non-zero if any host
failed.

Scope: local and SSH hosts only, for now. A `hosts_group` with a `winrm {}`
block is a clear error for an interactive command ("interactive mode is not
supported for winrm targets"), not a silent best-effort attempt — see
[Supported / deferred](#supported--deferred).

## Supported / deferred

**Supported:**

- Hosts groups: static `hosts` lists and dynamic `inventory` commands.
- Global and per-group environment variables, plus `-e`/`--env` CLI
  overrides; builtin `$CMT_HOSTS_GROUP`/`$CMT_HOST`/`$CMT_USER` variables.
- Commands: `run` (remote), `local` (runs once, not per host), `upload`.
- `serial` (bounded concurrency) and `once` (first-host-only).
- Targets: named, ordered, fail-fast command sequences.
- Host filtering via `--only`/`--except`.
- Piped, non-interactive stdin forwarded to every host's `run` command.
- SSH (default transport) and WinRM (opt-in), plus `sudo`/`su`/`doas`
  privilege escalation, all via `github.com/go-remoteexec/transport`.
- A true *interactive* `stdin` session (`interactive = true`) — sup's
  `stdin: true` live, keystroke-by-keystroke, multiplexed-output session
  across several hosts at once — for **local and SSH** hosts (see
  [Interactive sessions](#interactive-sessions) above and
  [Architecture](#architecture-buffered-vs-streaming-execution) below).

**Deferred, not stubbed:** interactive sessions (`interactive = true`)
against a **WinRM**-configured `hosts_group`, or a `hosts_group` with a
**`become {}`** block. `package interactive` only drives local and SSH
targets, streaming (neither `WinRM` nor a `remoteexec.Become`-wrapped
connection implements the streaming primitive it needs — see the
architecture section below); either is a clear error at run time rather
than a silent best-effort/degraded attempt.

## Architecture: buffered vs. streaming execution

`cmt` has two execution paths, in two packages, for two different needs:

- **`orchestrate`** (the default, for `run`/`local`/`upload` commands) drives
  every host through `remoteexec.Connection.Exec(ctx, cmd, stdin) (Result,
  error)` — one buffered `Result` returned only after the command finishes.
  This is the right shape for "run this, then tell me what happened," and it
  is entirely backed by `github.com/go-remoteexec/transport`, which owns the
  SSH/WinRM/local wire protocols and privilege-escalation wrapping.

- **`interactive`** (for `interactive = true` commands) needs the opposite
  shape: observe output as it's produced, and keep feeding input for as long
  as the session is open. `remoteexec.Connection`'s buffered `Exec` cannot do
  that no matter how it's wrapped, so `package interactive` instead uses
  `github.com/go-remoteexec/transport`'s `Streamer`/`Session` pair (added in
  v0.1.4): `remoteexec.NewLocal()` for a local target, `remoteexec.DialSSH`
  for an SSH one, each type-asserted to `Streamer` and opened via
  `NewSession(ctx)` instead of `Exec`. `connect.BuildSSHConfig` (the same
  mapping the buffered path uses) resolves a `hosts_group`'s `ssh{}` config
  either way, so the two paths never diverge on what a manifest's connection
  settings mean.

  `Streamer` is implemented by `Local` and `SSH`; `WinRM` deliberately is
  not (WS-Management's poll-based Send/Receive shell protocol doesn't map
  onto continuous streaming), and neither does a `remoteexec.Become`-wrapped
  connection (its sudo/su/doas marker-based success detection needs
  real-time stream scanning, not a slice of a buffered string) — both are
  explicit run-time errors from `Runner.Run`, not a silent best-effort
  attempt, matching the WinRM-deferred note above.

  Both packages still share the same host-resolution and env-layering rules
  (`orchestrate.ResolveHosts`/`MergeEnv`/`WithBuiltins`/`RenderEnv`, and
  `connect`'s host-string parsing) — only "how the command is actually
  driven" differs between them.

## Development

```sh
go build ./...
go vet ./...
go test -race ./...
```

`manifest` and `connect` are pure unit tests (HCL parsing; host-string and
SSH/WinRM/`become` config-shape mapping) plus one real, non-mocked
integration test each through `transport.NewLocal()` — no network, no
in-process SSH/WinRM server (that protocol is already tested inside
`go-remoteexec/transport` itself). `orchestrate` is tested against a fake
in-memory `transport.Connection`, exercising `serial` batching, `once`,
target fail-fast, env layering, and host filtering without any real
transport. `cmd/cmt` covers flag parsing and runs real end-to-end `local`
and `interactive` commands through the actual CLI entrypoint.

`interactive`, unlike `orchestrate`, *is* tested against a real (if tiny)
in-process SSH server — `golang.org/x/crypto/ssh` supports the server side
directly, the same pattern `go-remoteexec/transport`'s own tests use for
the same reason: real keystroke-by-keystroke, multi-host, live behavior is
impractical to fake convincingly and easy to get subtly wrong by asserting
against invented expectations instead. Its local path is exercised against
real `os/exec` processes with a controlled `io.Pipe`/`bytes.Reader` standing
in for a terminal's stdin — no TTY simulation. 95.3% coverage; the small
remaining gap is a handful of explicitly-documented, structurally-defensive
branches (a pipe-setup call erroring on an already-started process/session)
that aren't reachable through this package's own single-call usage pattern
without contrived fault injection.

## License

BSD-3-Clause — see [LICENSE](LICENSE).
