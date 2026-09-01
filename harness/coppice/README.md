# coppice

One static binary that owns the panes, the agents in them, and their state. It is the client
and the server: the first invocation starts a background server as you, and every later one
talks to it over a private socket.

coppice mirrors Herdr's verbs on purpose, so a Herdr user drives it from muscle memory. It
competes on one axis: **state comes from the gate, not from the screen.** When openDaisugi's
gate blocks a tool call, "blocked" is a fact we produced, with the tool name and the clause
attached. Screens are the fallback for agents with no hook, and the floor says `unknown` rather
than guessing `idle`.

Everything here was written by an AI assistant. Verify before you rely.

## Build

You need Go 1.26.8, Zig 0.16, CMake, pkg-config, uv, curl and git. `curl` and `git` ship on most
Linux boxes already; `uv` and `pkg-config` may not.

```
./scripts/toolchain.sh
./scripts/preflight.sh
go build ./cmd/coppice
```

`toolchain.sh` installs Zig and CMake under `~/.local`, no sudo, and builds `libghostty-vt` from
source into `~/.local/ghostty-vt`. Override that destination with `COPPICE_GHOSTTY_PREFIX`. It
also writes a pkg-config wrapper at `~/.local/bin/coppice-pkg-config` and two global Go settings,
`GOTOOLCHAIN=auto` and `PKG_CONFIG=coppice-pkg-config`, so a plain `go build` links. Undo both
with `go env -u GOTOOLCHAIN PKG_CONFIG`.

There is one thing the installer cannot give you: pkg-config itself. If `preflight.sh` reports it
missing, install `pkgconf` or `pkg-config` from your distribution's own package manager. That one
step needs sudo; nothing else here does, as long as `COPPICE_GHOSTTY_PREFIX` points somewhere you
can already write.

Pins and their reasons, including the unix-only build, are in `PINS.md`.

## Use

```
coppice server start
coppice pane create --cwd . --label "auth fix" -- claude
coppice pane list
coppice attach w1:p1
```

Every command that talks to the server brings one up if nothing is listening, unless
`COPPICE_NO_AUTOSTART` is set. `coppice server start` itself detaches and returns once the new
server answers; running it again when one is already up is a no-op, not an error. To watch the
server in your terminal instead of detaching, run `coppice server start --foreground`.

`coppice attach` and `coppice server stop` are the two exceptions: neither autostarts, with or
without a pane id for attach. If nothing is listening, `coppice attach` tells you to start a
server first, and `coppice server stop` prints `no server is running` and exits 0 - its whole
purpose is making sure none is, the same reasoning that makes `server start` idempotent.

Add `--json` to any `server`, `workspace`, `tab`, `pane`, `agent` or `session` command to get one
JSON object instead of a table or a line. `pane list` prints panes as a table by default; `--json`
gives you the same data a script can parse. `coppice attach` renders a live terminal. It has no
`--json` flag: its first argument is always a pane id.

Global flags, before the verb: `--socket PATH` and `--data-dir PATH` override where coppice looks
for the running server and its state; `--version` prints the build and exits.

A detached server writes its own stdout and stderr to `<data-dir>/server.log`. Check that file
first when a start seems to fail silently.

coppice exits 0 on success. It exits 1 when the server or the CLI itself refuses the request. It
exits 3 when the server is unreachable, or closes the connection mid-call. Exit 2 is reserved for
a gate deny. The CLI never returns it today; only a harness path can.

`ctrl+a d` detaches and leaves the pane running. `ctrl+a n` and `ctrl+a p` move to the next and
previous open pane. `ctrl+a c` opens a shell pane here. `ctrl+a x` closes this pane. `ctrl+a
ctrl+a` sends one literal ctrl+a to the pane itself. `ctrl+a ?` lists the keys.

Headless panes take a harness instead of a command, and give you typed events instead of a
screen:

```
coppice pane create --cwd . --kind headless --harness claude --label "the tests"
coppice agent prompt w1:p2 "run the tests and fix what breaks" --wait --timeout 600000
coppice agent wait w1:p2 --until blocked
coppice agent list
```

`--wait`'s timeout flag is `--timeout`, in milliseconds, the same as `pane wait-output` and
`agent wait` use.

Reading and debugging:

```
coppice pane read w1:p1 --source visible      the screen right now, the default
coppice pane read w1:p1 --source recent       the last 200 lines of scrollback
coppice pane read w1:p1 --source detection    the whole unwrapped screen, exactly what the manifest rules see
coppice pane explain w1:p1                    which rule matched, and every rule that did not
```

A pane you did not size yourself defaults to 120 columns by 40 rows. An unknown `--source` is a
plain error naming the three it accepts.

From another machine, over SSH, with no extra service:

```
coppice --remote ssh://build-box pane list
```

`coppice --stdio` speaks the same protocol on stdin and stdout. That is all `--remote` uses; it
is not wired for `coppice attach` yet, so attaching to a remote pane means SSHing in and running
`coppice attach` on that host directly.

## Command reference

Every verb the CLI takes, beyond the ones already shown above. `pane` and `agent` verbs take a
pane id as their first argument; add `--json` to a `server`, `workspace`, `tab`, `pane` or `agent`
command for one JSON object instead of a table or a line.

| command | what it does |
|---|---|
| `coppice server start\|stop\|status\|token` | bring up, shut down or inspect the server, or name its credential |
| `coppice workspace create [--cwd DIR] [--label TEXT]` | start a new workspace |
| `coppice workspace list` | list workspaces |
| `coppice tab create [--label TEXT] [--workspace ID]` | start a new tab |
| `coppice tab list [--workspace ID]` | list tabs |
| `coppice pane create [--cwd DIR] [--label TEXT] [--kind pty\|headless] [--harness NAME] [--cols N] [--rows N] -- COMMAND...` | start a pane |
| `coppice pane list` | list panes, with their state |
| `coppice pane send-text PANE TEXT [--no-enter]` | type text into a pane |
| `coppice pane send-keys PANE KEY...` | send named keys, see the server's `Known keys:` reply and `testdata/keys.json` for the list |
| `coppice pane run PANE LINE` | type a line and press enter |
| `coppice pane read PANE [--source visible\|recent\|detection]` | read a pane's screen |
| `coppice pane close PANE` | close a pane and free its grid |
| `coppice pane resize PANE --cols N --rows N` | resize a pane |
| `coppice pane wait-output PANE [--contains TEXT] [--state STATE] [--timeout MS]` | wait for text or a state |
| `coppice pane explain PANE` | show which manifest rule matched, and every rule that did not |
| `coppice agent list` | list panes as agents, with their merged state |
| `coppice agent get PANE` | one pane's merged state |
| `coppice agent prompt PANE TEXT [--wait] [--until STATE] [--timeout MS]` | send a prompt, optionally waiting for a state |
| `coppice agent wait PANE [--until STATE] [--timeout MS]` | wait for a pane's state |
| `coppice agent read PANE [--region visible\|recent\|detection]` | one pane's merged state plus its screen |
| `coppice` | open the floor: the roster grouped by who needs you, a peek on one pane, and attach, in this terminal |
| `coppice attach [PANE]` | render one pane live, in this terminal |
| `coppice web cert init [--name NAME]... [--ip ADDR]... [--ca-dir DIR] [--ca-listen ADDR] [--qr url\|pem\|off]` | make the local CA if there is none, issue a server certificate, and print a QR that installs the CA on a phone |
| `coppice web cert show [--ca-dir DIR]` | print what the current certificate covers and when it expires |
| `coppice web cert tailscale NAME.TAILNET.ts.net [--dir DIR]` | ask tailscale for a Let's Encrypt certificate and write it where `web serve --tls tailscale` looks for it |
| `coppice web serve [--listen ADDR] [--tls tailscale\|localca\|files\|off] [--cert FILE --key FILE] [--ca-dir DIR] [--ca-listen ADDR] [--external-url URL] [--gate-root DIR] [--voice-url URL] [--ntfy URL --ntfy-topic NAME --ntfy-token-env VAR] [--persist\|--forget] [--web-push] [--qr]` | serve the phone client, minting a token on the first run |
| `coppice web token [--rotate] [--url URL] [--qr] [--listen ADDR]` | print the current bearer token, its sign-in URL, and a QR to scan |

`coppice web` puts the floor on a phone over HTTPS. The repository's
`docs/how-to/phone.md` is the full guide: the certificate choice, signing in, and ntfy
push. `coppice web` never dials the coppice socket at the command line itself, so its
only exit codes are 0 and 1.

## What a restart does

> A restart brings back the layout, the labels and the working directories. It does not bring back running processes. Panes come back closed or unknown, never idle. A headless pane resumes only when its adapter recorded a harness session id.

`coppice server status` prints this same sentence back, along with what it found. A restored
pane never reports `idle`: closed or unknown are the only two honest answers for a process that
is not there to ask.

## Security

The socket is the credential. It lives at mode 0600 inside a 0700 directory, and the server
refuses any connection whose peer uid is not yours. There is no token, no port, and no second
user. `coppice server token` says so, in more words, on a terminal.

Spawned processes get `COPPICE_SOCK` and `COPPICE_PANE` in their environment. That pair is how
the openDaisugi gate hook inside a harness reports state back without any configuration.

This is Linux today. `lock_other.go` and `peercred_other.go` let the module compile on other
unixes, but a peer-credential check has no portable implementation outside Linux yet, so every
connection is refused there until one exists. The build itself is unix-only outright: the client
spawns its own detached child with a real process group, which has no Windows equivalent as
written.

## Agent detection

Screen detection uses Herdr's TOML manifests, vendored under `internal/detect/manifests/` at a
recorded commit, Apache-2.0, see `NOTICE`. The schema is written down in
`internal/detect/README.md`. coppice never fetches a manifest from the network.

Put your own file at `$XDG_CONFIG_HOME/coppice/agent-detection/<agent>.toml`, or
`~/.config/coppice/agent-detection/<agent>.toml` when that variable is unset, to replace a
bundled one. The file's own `id` field decides which bundled manifest it replaces, not its
filename; naming it after the agent is a convention, not a rule coppice enforces.

Screen detection is the fallback. A higher source outranks it: the gate, an operator, or an
attached client. A pane any of those reported on in the last two seconds, the `GateQuiet` window,
is not scanned at all. After two seconds the screen speaks again, so a harness that stops calling
the gate recovers rather than freezing on its last reported state.

If a manifest fails to load, `coppice server status` prints the warning. A broken override file
reverts to the bundled manifest, and it says so.

`COPPICE_CLAUDE_BIN`, `COPPICE_CODEX_BIN` and `COPPICE_SPRIG_BIN` override which binary a headless
pane spawns for that harness, in the daemon's own environment. Unset, each adapter runs its
harness's ordinary name off `PATH`.

## Layout of the code

| path | what it is |
|---|---|
| `cmd/coppice` | the built binary's own `main` |
| `internal/vt` | the only package that touches libghostty. cgo. |
| `internal/proto` | the wire: requests, responses, events, the error enum |
| `internal/state` | master spec 3.1, the source precedence, in one function |
| `internal/layout` | workspaces, tabs, panes, and the ids that are never reused |
| `internal/pane` | the grid, the pty, the headless adapter interface. cgo, through `internal/vt`. |
| `internal/detect` | Herdr's manifest engine in Go |
| `internal/server` | the socket, the dispatcher, the verbs. cgo, through `internal/pane`. |
| `internal/adapters` | one package per harness. cgo, through `internal/pane`. |
| `internal/attach` | the single-pane renderer. No cgo: it only ever reads frame diffs off the wire. |
| `internal/cli` | the command line and the thin client. cgo, through `internal/server`. |
| `internal/toolchain` | answers "is libghostty-vt built here", for every cgo test's own skip check |
| `internal/boundary` | the one test proving only `internal/vt` imports libghostty. No cgo, deliberately. |
| `scripts/` | `toolchain.sh` and `preflight.sh` |
| `testdata/` | fixtures: manifests' screens, vt goldens, fake harness scripts, wire events |

"cgo" above means the package cannot link without `scripts/toolchain.sh` having run, and its own
tests skip with a reason instead where that has not happened, unless `COPPICE_REQUIRE_TOOLCHAIN`
is set, which turns that skip into a failure. CI always sets it.

## What is not verified yet

- Codex's wire format and its `codex exec resume` argv are unverified on this box: no real
  `codex` binary has run against this adapter. The fixture in `testdata/adapters/` is invented
  from the published docs. The adapter accepts either of the two vocabularies it might turn out
  to speak, and fails loudly, naming the unknown type, rather than silently on anything else.
- The sprig adapter has likewise never run a real `sprig`. Its shape comes from reading
  `harness/sprig/cli.go`'s own source, and a test asserts the flag names it depends on have not
  drifted from that file.
- `internal/detect/manifests/claude.toml`'s `live_turn_working` rule cannot match a line ending
  `esc to interrupt)`: the regex's own alternation is missing that closing paren. This is a
  vendored Herdr file; see `NOTICE` and `internal/detect/manifests/PROVENANCE`. Fixing it means
  forking the vendored set at a recorded commit, not a quiet edit.
- A person ran `coppice attach` on a real terminal on 2026-09-10. The raw-mode, keystroke and
  detach path worked as intended. The panic-safe terminal restore still has no automated test.
  The alternate screen request was refused by that terminal, so whether `\x1b[?1049h` and
  `\x1b[?1049l` take effect depends on the terminal, not on this code: coppice writes both
  whenever stdout is a terminal, in the right place and the right order. That same run found a
  help overlay defect: the block landed on the status row and scrolled the terminal. This
  commit fixes it. `TestHelpNeverPaintsOnTheStatusRow` pins the fix: it asserts the help rows
  and the status row never overlap and the write carries no newline. The SIGWINCH resize path
  has an automated test too, against a real pty pair:
  `TestAttachAndResizeAgreeOnRowsFromOneTerminalHeight`. That test proves the wire values
  agree. It does not prove a person watching the screen sees the redraw happen correctly.
- `coppice attach` does not work over `--remote` yet. It dials a local unix socket directly.
- An ordinary connection's teardown, no `coppice server stop` involved, gives an in-flight
  handler up to 130 seconds to finish before reclaiming its goroutines and file descriptor. That
  bound only matters for a handler that never writes and never watches for the connection
  dying; `agent.wait` and `pane.wait_output` both do the latter and return within milliseconds
  regardless of how long the bound is.
- The frame stream coalesces onto a 60 Hz ceiling, `FrameInterval`, fixed at 16 ms. It does not
  distinguish a harness that redraws 50 times a second from one that redraws once. A frame can
  arrive before the `pane.attach` response that started the stream; a client dispatches on each
  message's own event name or request id, never on arrival order. `autoPanes` only ever demotes
  an automatic subscription to an explicit one, never the reverse.
- Two cross-language divergences plan 03's Python oracle still needs to settle: whether `pane`
  and `harness_session_id` type-check as optional the same way on both sides, and which clock
  the manifest hold window reads. Go uses the server's own receive clock. The Python side has
  used the event's own `ts`.
- The one real cross-language fixture guard, `TestEventFixturesMatchTheFloorFixtures`, has never
  run: it skips unconditionally until `tests/floor/testdata/events` exists. Plan 03's Python
  client is what creates that directory and owns the bytes in it.

## Contributing

`go vet ./...`, `gofmt -l .`, and `go test -race ./...` must all be clean before a change lands.
See `.github/workflows/coppice.yml` for the exact CI commands, including the libghostty-vt cache.
