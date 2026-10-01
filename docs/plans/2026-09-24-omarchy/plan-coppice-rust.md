# Stage I plan: coppice in Rust

Written 2026-10-01. The map for the Rust port of coppice, the Go floor in
`harness/coppice`. The owner wants a Go coppice and a Rust coppice that
check each other. So the Rust port speaks the same wire as Go, and a
driver replays the same cases against both and counts every difference.
Go is the reference: where the two differ, the Rust port is wrong, or Go
is wrong and gets a fix and a ruling. Rulings go in
`clients/ADJUDICATIONS.md` with a `CP-R-n` prefix.

## Where the port lives

`harness/coppice-rs/` is its own cargo crate, with its own `Cargo.lock`
and its own pins (`PINS.md` there). It is not part of `clients/rust`: that
crate is the daisugi port and links Z3, and coppice links neither Z3 nor
tree-sitter. The binary is `coppice-rs` while the port is partial, so it
can never be taken for the shipped `coppice`. It takes the same command
line as Go for what it has. Since slice 6b it carries all of it, and the
binary is named `coppice`.

## The Go packages, in dependency order

Lines are Go lines without tests. "Pinned by" names what fixes the
behaviour today: Go tests, testdata, or the protocol corpus in
`testdata/protocol`.

| # | Package | Lines | Owns | Pinned by |
|---|---|---|---|---|
| 1 | `proto` | 1,247 | The wire: one JSON object per line, the error enum, the native and JSON-RPC 2.0 framings, frames, the state event and its checks, the report extras, the corpus matcher. | proto tests, all 9 corpus files, `testdata/events` |
| 2 | `textwidth` | 137 | Cell width of a string, and `Printable` (control runes out, cut to N runes). | textwidth tests |
| 3 | `config` | 380 | `coppice.toml`: the harness tables, the default, plugins, keys, projects, gateway, voice. | config tests, `testdata/keys.json` |
| 4 | `layout` | 846 | The workspace, tab, pane and task tree, its ids, and `layout.json`. | layout tests, `tasks.jsonl`, `scope.jsonl` |
| 5 | `state` | 328 | The state store and the merge of the sources (gate, headless, process, manifest, operator). | state tests, `testdata/events` |
| 6 | `vt` | 349 | The only package that calls libghostty-vt: feed, resize, plain text, the viewport cells. | vt tests, `testdata/vt` |
| 7 | `pane` | 878 | The grid over a vt terminal, a pty process, the frame diff per client, the adapter interfaces. | pane tests, `testdata/vt` |
| 8 | `worktree` | 159 | A task's git worktree: add, dirty check, remove. | worktree tests |
| 9 | `detect` | 969 | The screen manifests per harness and the evaluator that reads a screen. | detect tests, `testdata/screens` |
| 10 | `adapters` | 4,088 | The headless harnesses: claude stream-json, codex, opencode, pi, sprig. | adapter tests, `testdata/adapters` |
| 11 | `web` | 3,954 | The phone server: TLS, tokens, the ask channel, the floor page, voice. | web tests, `testdata/phone`, `e2e/web.sh` |
| 12 | `server` | 10,489 | The daemon: the socket, roles by SO_PEERCRED, every verb, attach and flow control, tasks, asks and holds, the foreman, facts, ended panes and restart, projects, plugins, voice. | server tests, all 9 corpus files, `testdata/facts`, `testdata/events` |
| 13 | `plugins` | 875 | Plugin manifests, the policy runner, the shared view library. | plugins tests |
| 14 | `attach` | 1,117 | The client side of attach: the status line, keys, the leave key. | attach tests, `testdata/attach`, `testdata/keys.json` |
| 15 | `tiles` | 240 | The tile model shared with the web floor. | tiles tests |
| 16 | `tmuxmirror` | 710 | The tmux mirror of the floor. | tmuxmirror tests, `testdata/tmux` |
| 17 | `tui` | 7,253 | The terminal floor: the rail, windows, the prompt line, voice keys. | tui tests, `testdata/words.json`, `e2e/tui.sh` |
| 18 | `cli` | 3,375 | The command line: the client verbs, `server start/stop/status`, attach, stdio, remote. | cli tests |
| 19 | `cmd/coppice`, `toolchain`, `testhome`, `boundary` | 179 | The entry point and test helpers. | |

About 37,600 lines in all. The server is the largest part by far, so it is
split across slices by what each verb needs.

## Terminal emulation

The Rust port links the same pinned libghostty-vt as Go, through its C API,
from the native prefix that `clients/go/scripts/native.sh --print-prefix`
names. The prefix and `scripts/toolchain.sh` (what the coppice CI job
builds) both pin ghostty `b0c421fcd2e290629d4285c181b52fe2f2095f06` with
zig 0.16.0, so both ports run the same engine. A screen is then the same
text on both sides by construction, which is what lets the compare check
`pane.read` text exactly.

- `build.rs` reads the prefix from `COPPICE_GHOSTTY_PREFIX` and links
  `lib/libghostty-vt.a` statically. It hard-codes no path. CI points it at
  the cached native prefix.
- The extern block is written by hand, for only the C calls `vt.go` makes
  through go-libghostty, checked against the headers in the prefix. No
  bindgen: it would be one more crate and a libclang at build time.
- No Rust terminal crate. A Rust VT emulator (vte, alacritty_terminal,
  wezterm-term) would parse escapes its own way, so the two ports would
  disagree on screens for reasons that are not bugs in either. That defeats
  the point of a cross-check.

## Crates

Every crate must earn its place: license, provenance, size. The lock file
pins each one, and builds run `--offline --locked`.

- `serde_json` and `serde` (MIT or Apache-2.0, the serde-rs project, by
  dtolnay): JSON in and out. Already in `clients/rust`.
- `libc` (MIT or Apache-2.0, the rust-lang project): the pty, SO_PEERCRED,
  flock, signals, `/proc`. Already in `clients/rust`.
- `toml` (MIT or Apache-2.0, the toml-rs project): `coppice.toml`, which Go
  reads with BurntSushi/toml. Both follow TOML 1.0, so a hand-written
  reader is the larger risk.

No async runtime: one thread per connection and per pane, as Go uses one
goroutine each. No pty crate: `openpty`, `setsid` and `TIOCSCTTY` through
libc are a few lines.

## The comparison

`clients/coppice_compare.py` (stdlib only, so uv fetches nothing) replays
the corpus. For each file it starts a fresh Go server and a fresh Rust
server, each with its own scratch HOME, socket, data dir and start dir,
`PATH=/usr/bin:/bin`, and a `coppice.toml` with no plugins and no gateway.
A fresh pair per file keeps pane and task ids the same on both sides even
when one side refuses a file. Each server starts as `--socket S --data-dir
D server start --foreground`, as a child in its own process group, and
stops with SIGTERM, then SIGKILL. Any process left with that socket in its
`COPPICE_SOCK` is killed and counted as a leak.

Each case passes two gates:

- **Gate A:** each reply matches the corpus line, with the matcher in
  `tests/floor/protocol_match.py`.
- **Gate B:** the full Go reply equals the full Rust reply, after each
  server's own paths become placeholders and the values that differ by
  process (`pid`, `uptime_s`, `ts`, `quiet_for`, `ended_at`) are masked.
  Anything else masked is a ruling with its own `CP-R-n`.

A case is agree, disagree, refused (the Rust server says it has no such
command) or go-fail (the Go reply fails the corpus line, a fault in Go or
the corpus). A slice is done when its files show 0 disagree, 0 go-fail, 0
leaks, and every refused case is ruled.

Only slice 1 has a ready corpus. The later slices add their own cases
before their code: request and reply cases go in a new directory,
`harness/coppice-rs/cases/<slice>/`, in the corpus format, and the driver
replays them the same way. They do not go in `testdata/protocol`, because
the Go conformance test replays every file there and must not change with
this port. Cases that are not request and reply (a screen, a file tree, a
CLI run) use the testdata that already pins Go, named per slice below.

## Slices

Each slice is one job. Each ends with a compare at 0 disagreements on its
cases, and with the cases it does not cover listed as refused, each with a
ruling.

1. **The server core.** Built 2026-10-01: all 70 corpus cases agree, none
   refused; what it leaves to later slices is ruled in CP-R-3 to CP-R-12. `proto` (codec, both framings, frames, the state
   event), `textwidth`, `config` (load and path), `layout`, `state`, `vt`
   over FFI, `pane` (grid, pty, frame diff; the headless interfaces only),
   and of `server`: the socket at 0600 in a 0700 directory, the start lock,
   the uid check and roles by SO_PEERCRED and `/proc`, `hello`, the guard,
   notes, `agent.allow` and `agent.deny` over the gate's ask files, the
   pane, tab and workspace verbs, attach with flow control and view-only,
   events, tasks without worktrees, `pane.report_state`, the agent verbs
   for pty panes, and `server.status`/`server.stop`. Of `cli`: `server
   start`, `stop` and `status`. Cases: all 9 corpus files.
2. **What the floor knows about a pane.** Built 2026-10-01: all 307
   cases of the four suites agree (the 70 protocol cases, 106 in
   `cases/facts/`, 112 built from `testdata/screens` and 19 from
   `testdata/events`), and so do the 20 event views; none refused. What
   it leaves to later slices is ruled in CP-R-13 to CP-R-18, and CP-R-3,
   CP-R-4 and CP-R-11 are narrowed. `detect` and the manifest tick,
   `pane.explain`, the ready-prompt rules and `pane.trust` (`ready.go`),
   asks held for a foreman, the foreman and `floor.talk`, subagents
   (`pane.report_child`), and the facts and stack (`facts.go`,
   `facts_stack.go`, `floor.facts`). Cases: `testdata/screens`,
   `testdata/events`, `testdata/facts`, and new cases in
   `cases/facts/`. The screens and events suites are built at run time
   from the Go testdata, so no file is added there; `testdata/facts` is
   replayed by the Rust unit tests of the transcript reader.
3. **Ended panes and restart.** Built 2026-10-02: all 408 cases agree
   (the 100 in `cases/ended/` and one more in `cases/facts/` among
   them), and so do the 25 event views and 9 tree views; none refused.
   CP-R-6 and CP-R-7 are retired, CP-R-4 is narrowed, and what the
   slice leaves is ruled in CP-R-19 to CP-R-23. `ended.go`,
   `restart.go`, `projects.go`, `worktree` for task worktrees,
   `layout.json` and `recent-dirs.json`. Cases: data dirs before and
   after a restart, compared as file trees (the driver's `#!tree` and
   `#!restart`), and new cases in `cases/ended/` (`pane.forget`,
   `pane.resume`, `project.list`, `task.create` with a worktree in a
   scratch repo that `#!repo` makes per side).
4. **Headless harnesses.** Built 2026-10-02: all 552 cases agree (128
   in `cases/headless/` and 15 in the adapters suite among them), and so
   do the 35 event views and 17 tree views; none refused. CP-R-3, CP-R-17
   and CP-R-20 are retired, CP-R-19 is narrowed, and what the slice
   leaves is ruled in CP-R-24 to CP-R-28. Before it, CP-R-23 was fixed
   in Go and Rust together. `adapters` (claude, codex, opencode, pi,
   sprig), `pane.fork` for real, `agent.prompt` and `agent.wait` on a
   headless pane. Cases: `testdata/adapters` with its fake harnesses
   (the driver's adapters suite), and new cases in `cases/headless/`
   with fake pi and opencode harnesses beside them.
5. **The command line.** Built 2026-10-02: all 916 cases agree (301 in
   `cases/cli/` and 63 in the keys suite among them), and so do the 48
   event views and 17 tree views; none refused. CP-R-1, CP-R-4 and CP-R-9
   are retired, CP-R-12 is narrowed, and what the slice leaves is ruled
   in CP-R-29 to CP-R-32. `cli` (every command but the floor on a
   terminal and `coppice web`), `attach`, `tmuxmirror`, the plugin loader
   and runner with the plugin role, and the voice supervisor. Cases: the
   stdout, stderr and exit code of each command against a scratch server
   (the driver's `cli` request), attach on a pseudo terminal (`pty`), the
   tmux mirror on a scratch tmux server, `testdata/attach` and
   `testdata/tmux` through the Rust unit tests, and `testdata/keys.json`
   (the keys suite).
6. **The screens.** `tui`, `tiles` and `web`. Cases: `testdata/words.json`,
   `testdata/phone`, and `harness/coppice/e2e` (`tui.sh`, `web.sh`) run
   against each binary. Split in two jobs:
   - **6a, tiles and the web floor.** Built 2026-10-02: all 395 cases in
     `cases/web` and their 13 event views agree, and so does the full
     compare, run in parts (1311 cases, 61 event views, 17 tree views);
     none refused. `e2e/run.sh web` passes against each binary. `tiles`
     with Go's tests, `web` (the HTTP and websocket server, the guard
     and the ban list, the /proc/net/tcp peer check, the event ring,
     views, voice, ntfy push, the local CA and TLS, the QR code) with the
     page embedded from the Go tree, `coppice web` and its subcommands,
     and `web.json` autostart in `server start`. The driver's web suite
     drives each side's web server on scratch loopback ports with raw
     HTTP and websocket clients. CP-R-12 is narrowed to the floor on a
     terminal, and the slice is ruled in CP-R-33 to CP-R-40. Three crates
     come in, at clients/rust's versions: rustls, rustls-pki-types and
     ring (CP-R-36).
   - **6b, the floor on a terminal.** Built 2026-10-02: all 230 cases
     in `cases/tui`, their 3 event views and 2 tree views agree, and so
     does the full compare, run in parts (1541 cases, 64 event views, 19
     tree views); none refused. `tui` (the rail, the peek, the prompt
     line and its words, live windows, the mouse, the key table and
     footer, Recent, the picker, `floor.json`, the talk key and voice,
     the first run), with Go's tests that need no server and a set of
     its Run tests against an in-process server, `testdata/words.json`
     held by a unit test. The driver reads the floor as a screen: each
     side's floor runs in a pane of a second scratch server and a
     `screen` request reads it through that server's libghostty-vt.
     `e2e/run.sh tui` passes against each binary, run by hand. CP-R-12 is retired,
     CP-R-40 narrowed, and the slice is ruled in CP-R-41 to CP-R-45.

After slice 6 the Rust binary is named `coppice`, as Go's is (the Cargo
package keeps `coppice-rs`). `scripts/install.sh` and
`scripts/release.sh` build either one: `COPPICE_PORT=rust` picks the
Rust coppice, and Go stays the default. Stage I is built.
