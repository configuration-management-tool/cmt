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
instead of a bespoke implementation.

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
  resolved host — `serial`/`once` apply only to `run`/`upload`, not `local`).

- **`target "name" { commands = [...] }`** — a named, ordered list of command
  names. Commands run in sequence; if any host fails a command (a transport
  error, or a nonzero exit code for `run`/`local`), the remaining commands in
  the target do not run (fail-fast, matching sup).

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
execution — matching `echo cmd | sup group cmd` in upstream sup.

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

**Deferred, not stubbed:** a true *interactive* `stdin` session — sup's
`stdin: true` live, keystroke-by-keystroke, multiplexed-output session across
several hosts at once. `remoteexec.Connection.Exec` returns one buffered
`Result` after a command finishes; it does not stream stdout/stderr live, so
a real interactive multi-host session would need a streaming extension this
project does not add. Piped (fully-buffered, known-in-advance) stdin works
today, as described above; an interactive terminal session does not.

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
transport. `cmd/cmt` covers flag parsing and runs one real end-to-end
`local` command through the actual CLI entrypoint.

## License

BSD-3-Clause — see [LICENSE](LICENSE).
