# Spec 02 — coppice-server (Go)

**Master:** §2, §3.1, §3.3, §3.4, §5.2–5.5 · **Size:** XL · **Depends on:** 01 (for the event
shape; the Go side re-declares the same JSON, tested against the Python fixtures)

## Purpose

One static Go binary, `coppice`, that is both client and server. The first invocation starts a
background server as the user; the server owns PTYs, ghostty grids, headless harness processes,
the layout, and the state merge; clients attach over the socket (locally, over SSH, or through
the PWA in spec-06). It mirrors Herdr's verbs so a Herdr user is at home, and it competes on the
one axis where it is structurally better: state comes from the gate.

## The cruxes (see master §5.2–5.5 for the arguments)

- Both pane kinds. `pty` for interactive harness UIs; `headless` for supervision-grade streams.
- State merge lives here, in one place, by the §3.1 precedence rules. Manifests are the fallback.
- Detach survives anything. Server restart restores layout and cwds, resumes where an adapter
  recorded a harness session id, resurrects nothing else. Say so in `coppice server status`.
- Ghostty via go-libghostty, statically linked. Pin recorded in `PINS.md`.

## Layout

Amended 2026-09-10, task 21: this block now matches the tree at HEAD, not the plan's original
draft. `go.mod` reads `go 1.26.0` / `toolchain go1.26.8`, not `go 1.25` (`PINS.md`'s "Go toolchain
deviation" section explains why); `README.md`, `internal/cli/`, `internal/toolchain/`,
`internal/boundary/` and `scripts/` were not in the original draft at all.

```
harness/coppice/
  go.mod                         module github.com/opendaisugi/coppice; go 1.26.0, toolchain go1.26.8
  README.md                      what a fresh reader needs: build, use, security, what is not verified yet
  PINS.md                        go-libghostty version, ghostty commit it builds, Herdr reference commit
  NOTICE                         Apache-2.0 attribution for vendored Herdr manifests
  scripts/                       toolchain.sh (installs Zig/CMake/libghostty-vt), preflight.sh
  cmd/coppice/main.go            the built binary's own main; hands straight to internal/cli
  internal/cli/                  verb parsing, the socket client, --stdio proxy, `coppice attach`'s CLI wiring
  internal/proto/                request/response/event types; error code enum; JSONL codec
  internal/server/               listener (peer-uid check), dispatcher, subscriptions, state file
  internal/layout/               workspace → tab → pane tree; ids "w1:p3"; persist/restore
  internal/pane/                 the grid, the pty, the headless Adapter interface + registry
  internal/adapters/claude/      claude -p stream-json (in + out)
  internal/adapters/codex/       codex exec --json
  internal/adapters/pi/          (spec-04 fills; interface + skeleton here)
  internal/adapters/opencode/    (spec-05 fills; interface + skeleton here)
  internal/adapters/sprig/       sprig, one process per prompt, session tree tail
  internal/state/                PaneStateEvent (Go struct), merge() per §3.1, per-pane current state
  internal/detect/               TOML manifest loader + evaluator; vendored manifests; override dir
  internal/detect/manifests/     *.toml from Herdr (Apache-2.0), one NOTICE line each
  internal/attach/               terminal renderer for `coppice attach` (single pane, status line, leader key)
  internal/toolchain/            answers "is libghostty-vt built here", for every cgo test's own skip check
  internal/boundary/             the one test proving only internal/vt imports libghostty
  internal/vt/                   the only package that touches libghostty
  testdata/vt/                   recorded VT streams + golden grids
  testdata/screens/              manifest fixtures: <agent>/<state>-N.txt
  testdata/events/               PaneStateEvent fixtures shared with tests/floor (symlink or copy + checksum test)
  testdata/adapters/             fake harness scripts (fake-claude.sh, fake-codex.sh, fake-sprig.sh)
```

## Socket API (normative details for §3.3)

Error codes (closed enum): `bad_request`, `no_such_pane`, `no_such_workspace`, `no_such_tab`,
`pane_closed`, `server_closed`, `not_attached`, `adapter_error`, `spawn_failed`, `timeout`,
`unauthorized`, `internal`.

`pane.create` fields: `cwd` (required), `cmd_argv` (required for pty; for headless it is the
adapter name in `harness` plus optional extra argv), `env` (map), `label`, `kind` (`pty` |
`headless`), `harness` (adapter name; required when headless), `cols`/`rows` (default 120×40),
`workspace`, `tab` (default: current). Result `{pane, workspace, tab}`.

Every spawned process gets `COPPICE_SOCK=<socket path>` and `COPPICE_PANE=<id>` in its env. That
is how spec-01's `report_state` finds its way back.

`pane.attach` streams `frame` events (first frame full, then `rows_changed` diffs) and `state`
events for that pane; the client sends `pane.resize` on terminal resize; `pane.detach` stops the
stream. Frames are coalesced at 60 Hz maximum per pane.

`pane.read --source detection` returns the whole screen, unwrapped, exactly what the manifest
evaluator sees. This is the debugging window; the Python floor evaluator uses it too. See the
Manifests section below for the 2026-09-10 note on this.

`agent.wait` fields: `pane`, `until` (state), `timeout_ms`. Resolves on the merged state, so a
gate-reported `blocked` resolves it immediately.

`events.subscribe` fields: `panes` (list or `*`), `kinds` (`state` | `layout` | `frame`).

## State merge

`internal/state` implements §3.1 exactly. Inputs: `pane.report_state` (source as given by the
caller; the server *downgrades* any `operator` source arriving from a non-attached client to
`gate` — only an attached client may speak as operator), process exit (`process`/`done`),
adapter events (`headless`), manifest evaluation on a 500 ms tick for pty panes whose current
source is `manifest` or `unknown` (a pty pane with a gate source in the last 2 s is not scanned).
`unknown` is the state of a pane with no source; `idle` is never a default.

## Ghostty grid

Per pty pane: one go-libghostty terminal sized to the pane. Bytes from the PTY feed the terminal;
the render state yields rows of cells `{text, fg, bg, attrs}`; the frame diff is computed by
comparing row hashes to the last frame sent to *that client* (per-client seq). `read visible`
formats the screen as plain text via the binding's formatter; `read recent` includes the last 200
scrollback rows. Resize goes to both the terminal and the PTY winsize.

Build: `PINS.md` records the go-libghostty tag chosen on day one and the ghostty commit it
fetches. Build-time deps are Zig 0.16 and CMake; `harness/coppice/scripts/toolchain.sh` installs
both into `~/.local` without sudo (Zig tarball from ziglang.org with sha256; `uv tool install
cmake`). CI caches the built library.

## Headless adapters

```go
// ErrUnsupported is what an adapter returns for a verb its harness has no
// equivalent for. It is not an error condition; it is an honest "no".
var ErrUnsupported = errors.New("this harness does not support that")

type EventKind string

const (
    EvText  EventKind = "text"
    EvTool  EventKind = "tool"
    EvState EventKind = "state"
    EvEnd   EventKind = "end"
    EvError EventKind = "error"
)

// Event is one normalised thing a harness said.
//
// Ask is the pending question when State is "blocked". pi's extension_ui_request
// (spec-04) and OpenCode's permission.ask (spec-05) both carry a tool name and a
// summary, and a floor client cannot render "blocked on what?" without them, so
// the field lives here rather than being invented twice.
type Event struct {
    Kind   EventKind
    Text   string
    Tool   string
    State  string // idle | working | blocked | done | unknown, for EvState and EvEnd
    Detail string
    Ask    *proto.Ask
}

type StartOpts struct {
    Cwd    string
    Env    map[string]string
    Argv   []string // extra argv beyond the adapter's own
    Resume string   // a harness session id to resume, when one was recorded
    Sock   string
    PaneID string
}

// Proc is one running headless harness.
type Proc interface {
    Prompt(text string) error
    Steer(text string) error // may return ErrUnsupported
    WriteStdin(b []byte) error
    // Events must return the SAME channel on every call: pumpAdapter's drain
    // step (fix round 1, item 2) calls it again after Stop(), and a
    // different channel there would silently stop draining the one it was
    // already reading.
    Events() <-chan Event
    SessionID() (string, bool)
    Stop() error
}

// Adapter starts one kind of harness headlessly.
//
// Four rules bind every implementation, including the ones spec-04 and spec-05
// write, so they do not each invent an answer:
//
//  1. Set cmd.Env = BuildEnv(os.Environ(), o.Env, o.Sock, o.PaneID). That is the
//     only path by which a headless harness's gate hook finds COPPICE_SOCK and
//     COPPICE_PANE.
//  2. The SERVER owns rendering. g is passed for an adapter that wants to write
//     raw VT bytes itself; claude, codex and sprig all ignore it, because
//     pumpAdapter renders their events into the same grid. Ignoring it is the
//     expected case.
//  3. Stop() signals and kills. The goroutine producing into Events() is the
//     one that closes it, after it observes the stop signal. Closing it from
//     Stop races a producer into a closed channel, and that panic takes the
//     whole daemon down.
//  4. EvEnd is terminal, and closing Events() afterward is the adapter's own
//     job: the pump only falls back to calling Stop() and draining (bounded)
//     when an adapter that already sent one keeps its channel open anyway -
//     that is a safety net, not a substitute for the adapter closing its own
//     stream. Two things every implementation, spec-04's and spec-05's most
//     of all, must also get right on the way there: emit one Event per
//     message, not one per streamed token delta - the pump renders one grid
//     row per Event it sees, so a token-by-token harness must coalesce its
//     own deltas before calling in; and know that Text is written into the
//     grid RAW, unescaped, so the pump's own "[end]"/"[error]" marker lines
//     are not authoritative to a reader unless the adapter's model text
//     cannot itself produce that literal text, or the adapter escapes its
//     own model text before handing it over - pick one and say which in the
//     adapter's own doc comment.
type Adapter interface {
    Name() string
    Start(ctx context.Context, o StartOpts, g *Grid) (Proc, error)
}
```

Adapter events become (a) transcript text appended to the pane's grid through a plain writer (so
headless panes are also just grids to a client) and (b) PaneStateEvents with `source: headless`.
The claude adapter: `claude -p --output-format stream-json --input-format stream-json --verbose`,
prompt = one JSON user message line on stdin; `result` event ⇒ `idle`; process exit ⇒ `done`.
The codex adapter: `codex exec --json` per turn, one process per prompt, resume via
`codex exec --resume <id>` when available. The sprig adapter tails the session tree file (path E,
`harness/sprig/session_tree.go` shape) for state and forwards stdin prompts.

## Manifests

TOML in Herdr's schema (plan task: copy the schema from the vendored files, document it in
`internal/detect/README.md`). Bundled `manifests/*.toml` vendored from Herdr's `src/detect/
manifests/` at a recorded commit, each file keeping its header comment plus one attribution line.
Override directory `~/.config/coppice/agent-detection/<agent>.toml` replaces the bundled file of
the same name. No remote fetch of manifests (Herdr's remote update is off the table; updates ride
our releases). Evaluation reads the whole unwrapped screen plus the terminal title and the last
OSC progress sequence, and each rule's own `region` narrows that screen the way Herdr's own rules
expect. `explain` output (`coppice pane explain <id>`) lists which rule matched.

2026-09-10 note, final fix wave, plan 02: this section originally said evaluation reads the
bottom 12 unwrapped rows. The final review found 43 vendored rules across the 21 manifests ask
for more than 12 lines, so the fixed window was ruled out in favor of the whole screen, with each
rule's own region doing the narrowing instead.

## CLI

```
coppice server start|stop|status|token
coppice workspace create [--cwd] [--label]     coppice workspace list
coppice tab create …                            coppice tab list
coppice pane create [--cwd] [--label] [--kind pty|headless] [--harness NAME] -- <argv>
coppice pane list [--json]   send-text   send-keys   run   read [--source]   close   wait-output   explain
coppice agent list | get | prompt <pane> <text> [--wait] [--timeout MS] | wait <pane> [--until S] | read
coppice attach [<pane>]                          full-screen single-pane renderer; leader ctrl+a; n/p/c/d/x/?
coppice --remote ssh://host                      thin client: ssh host coppice --stdio
```

`attach` is deliberately single-pane: splits and multi-pane layouts are the Python cockpit's and
the PWA's job. The status line shows pane id, label, harness, merged state, and its source.

## Security

- Socket dir 0700, socket 0600; server checks `SO_PEERCRED` uid == own uid; else `unauthorized`
  and close.
- `--stdio` mode (for `--remote` over SSH) speaks the same JSONL on stdin/stdout, no auth beyond
  SSH.
- Spawned env never includes the server's own secrets; the server has none.

## Tests

- `proto`: golden JSON for every request/response/event; the PaneStateEvent fixtures in
  `testdata/events/` are byte-identical to `tests/floor/testdata/events/` (a test checks).
- `state`: table tests for every §3.1 rule; a fuzz test that no sequence yields `idle` without a
  source.
- `pane/grid`: golden grids from `testdata/vt/*.bin` (recorded with `script -q -c '<harness> …'`)
  → `*.golden.txt`; resize test; frame-diff test (only changed rows in the diff).
- `detect`: every bundled manifest × every state it declares has ≥ 1 fixture; a fixture that
  matches *two* states is a test failure (ambiguity).
- `adapters/claude`: a fake `claude` script emitting recorded stream-json → events; prompt line
  reaches stdin; exit ⇒ done.
- `server`: integration test spawning `sh -c 'echo hi; sleep 1'` in a pty pane, attaching, seeing
  `hi` in a frame, then `done`.
- Restart: persist, kill, start, layout and cwds restored, panes marked `done`/`unknown` honestly.

## Out of scope

Multi-pane rendering in Go; remote manifest updates; multi-user; Windows.
