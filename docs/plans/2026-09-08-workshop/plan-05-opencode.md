# OpenCode Adapter and Gate Plugin Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** coppice can drive OpenCode headlessly over its HTTP/SSE server as a supervision-grade pane, and every OpenCode tool call passes through a fail-closed openDaisugi gate plugin — whether OpenCode runs inside coppice, inside Herdr, or bare.

**Architecture:** A Go adapter (`internal/adapters/opencode`) speaks HTTP to `opencode serve`: it creates one session per pane, posts prompts as messages, and translates the `/event` SSE bus into `pane.Event`s. A TypeScript plugin (`daisugi-gate.ts`, loaded automatically from OpenCode's plugin directory — no `opencode.json` registration needed) intercepts `tool.execute.before` and asks the resident gate over its unix socket, throwing to deny (OpenCode's hook is deny-only by design). Python's `--format opencode` gains the exit-code host contract Claude Code already has — without it, an OpenCode deny would only ever reach unread JSON on stdout and the plugin would see exit 0 and allow anyway.

**Tech Stack:** Go 1.25 (`harness/coppice`, stdlib `net/http` + `bufio` SSE parsing, no new dependency). TypeScript run directly by Node 22's native type-stripping (no npm install, no compile step — `import type` erases cleanly even when the package it names is not installed). Python 3.12 stdlib (`opendaisugi.hook`, `opendaisugi.gate`, `opendaisugi.install`, `opendaisugi.modules`).

**Spec:** `docs/plans/2026-09-08-workshop/spec-05-opencode.md`. Master contracts: `docs/plans/2026-09-08-workshop/00-master-spec.md` §3.2 (`PaneBackend`/`Frame` context), §3.4 (headless adapters table), §3.6 (fail-closed restated for new joins). Sibling design reused for the socket-client shape: `docs/plans/2026-09-08-workshop/spec-04-pi.md`.

**Requires:** plan 00 (import-boundary test), plan 01 (`opendaisugi.floor`, `daisugi hook report`, `report_state` — the plugin's state-push path), plan 02 (`harness/coppice` exists: `go.mod` for module `github.com/opendaisugi/coppice`, and `internal/pane` exporting the `Adapter` interface, `StartOpts`, `Proc`, `Event`, `EventKind`, `Ask`, `ErrUnsupported` exactly as described in spec-02 §Headless adapters — quoted in Task 1 below). **Provides:** `internal/adapters/opencode` (Go), `src/opendaisugi/harness_opencode/plugin/daisugi-gate.ts`, `EXIT_CODE_FORMATS` in `hook.py`, `install_opencode_harness`/`uninstall_opencode_harness` in `install.py`, `daisugi install --harness opencode`.

## Corrections from adversarial review (2026-09-08 — authoritative over the tasks below)

**Task 1 and the `filePath` half of Task 5 are ready to build now** — the plan's own live
research beat its spec on every contested fact, confirmed independently against upstream:
plugins load from `.opencode/plugins/` and `~/.config/opencode/plugins/` (plural) and
"Files in these directories are automatically loaded at startup", with no `opencode.json` entry
needed (`anomalyco/opencode` `packages/web/src/content/docs/plugins.mdx`, "Use a plugin"); the
hook list contains **no `permission.ask`** — the permission events are `permission.asked` and
`permission.replied`, reached through the generic `event` hook (same file, "Permission Events");
`tool.execute.before(input, output)` denies by `throw`, and `output.args.filePath` is the real
key OpenCode's file tools use (the shipped `.env` protection example). `hook.py`'s path
extraction really does miss `filePath`, so that half of Task 5 is a genuine and correct fix. The
defects cluster in Tasks 2-7.

- **BLOCKER (cross-plan ruling 1) — Task 5 Step 4 is deleted; plan-04 lands the `_outcome()`
  fix.** Both plans independently rewrite `gate.py:459-519` for the same real fail-open, and the
  bug is worse than either says: `stdout_for_format` (`hook.py:49-73`) has no `opencode` case
  either, so a deny on this format returns `{"continue": true}` on stdout *and* `exit_code 0` —
  it fails open on both channels. **This plan's `EXIT_CODE_FORMATS` design is the one that wins**
  (a set, not a second literal branch), but it lands once, in
  `plan-04-pi.md` Task 3 Step 6, as `EXIT_CODE_FORMATS = frozenset({"claude", "pi",
  "opencode"})` — all three at once, so this plan touches no branch. Task 5 here keeps only:
  Step 3's `filePath` fallback in `_payload_to_record`, and tests that **assert membership**
  (`"opencode" in EXIT_CODE_FORMATS`) plus the end-to-end exit-2-on-deny behaviour through
  `gate_and_contract`. Delete Step 4 and its `_outcome()` listing. plan-04 also adds the tail
  guard this design still needs — any `fmt` in neither `EXIT_CODE_FORMATS` nor
  `STDOUT_BLOCK_FORMATS = frozenset({"hermes", "openclaw"})` denies with exit 2 rather than
  falling through to `{"continue": true}` — because `--format` is an unrestricted string
  (`gate.py:1019`, no `choices=`). If this plan lands first for any reason, it lands the
  identical three-element set plus the tail guard and plan-04 asserts membership: whichever
  lands first lands the whole set; the second adds nothing. — applied in Task 5 (header states
  the dependency on `plan-04-pi.md` Task 3; Step 3/Step 4 keep only the `filePath` fallback;
  Step 1's tests assert `"opencode" in EXIT_CODE_FORMATS` and the end-to-end exit-2 behaviour;
  Step 7 names the reconciliation if plan-04 lands with a narrower set).
- **BLOCKER (cross-plan ruling 2) — Tasks 2-4: the `internal/pane` contract quoted in Task 1 is
  not plan-02's.** Task 1 says "assumed contract" and offers to reconcile at `go build` time;
  do not. plan-02 has landed the real definitions at `plan-02-coppice-server.md` ≈3626-3681, with
  `Ask`/`PaneStateEvent` in `internal/state` at ≈1503-1520. **plan-02's shape is canonical,
  verbatim.** Delete Task 1's `package pane` listing, import plan-02's, and read plan-02's own
  claude adapter (≈9174) and codex adapter (≈9628) for how `Start(ctx, o, g *Grid)` and `EvText`
  coexist. The deltas that force a rewrite of Tasks 2-4's code and tests: `Proc` is a **6-method
  interface** (`Prompt`/`Steer`/`WriteStdin`/`Events`/`SessionID`/`Stop`), not
  `struct{ ID string }` — so `proc.Prompt(text)`, never `a.Prompt(p, text)`, and
  `Start` returns a `Proc` implementation, not a `pane.Proc{}` zero value on error;
  `Adapter` is `Name()` + `Start(ctx, StartOpts, *Grid) (Proc, error)` only, so
  `func (a *Adapter) Start(ctx, opts pane.StartOpts)` gains the grid parameter; fields are
  `Cwd`/`Argv`/`Resume`, not `Cwd`/`ExtraArgv`/`ResumeID` (`buildServeArgs(port,
  opts.ExtraArgv)` becomes `opts.Argv`); constants are `EvText`/`EvTool`/`EvState`/`EvEnd`/
  `EvError`, not `EventText`/…; `Ask` is `state.Ask`, not `pane.Ask`. **One amendment plan-02
  owes this plan and plan-04:** `pane.Event` has no `Ask` field, so a `blocked` event cannot
  carry the permission id and title spec-05 requires and master §3.1 mandates on a blocked
  event. plan-02 adds `Ask *state.Ask` to `pane.Event`, keeping `State string`. No import cycle:
  `internal/state` imports only `internal/proto`, so `pane` → `state` is safe. — applied in
  Tasks 3 and 4, corrected against the live file rather than this note's exact wording: as of
  this fix, `plan-02-coppice-server.md`'s `internal/pane/adapter.go` listing already carries
  `Ask *proto.Ask` on `Event` (`internal/pane` imports `internal/proto` directly for it, not
  `internal/state` — verified live in that file), so no further amendment to plan-02 is owed;
  Tasks 3-4 import `internal/proto` and use `proto.Ask`, `pane.EvText`/`EvTool`/`EvState`/
  `EvEnd`/`EvError`, `pane.StateIdleStr`/`StateWorkingStr`/`StateBlockedStr`/`StateDoneStr`/
  `StateUnknownStr`, `pane.StartOpts{Cwd,Env,Argv,Resume,Sock,PaneID}`, and `pane.Proc` as a
  6-method interface throughout; Task 4 also flips the already-registered
  `internal/adapters/opencode/opencode.go`'s `New()` from `adapters.NotBuilt` to the real
  adapter instead of declaring a second `New()`.
- **BLOCKER — Task 6 Step 3: `askGate` trusts any socket at the path, so a planted socket is a
  universal allow.** `gate_client.py:33-50` (`_socket_is_trustworthy`) exists for exactly this:
  it `os.lstat`s the path (never `exists()`, which follows symlinks) and refuses anything that is
  not a socket, not owned by `os.getuid()`, or not mode `0600` — its docstring names the attack
  ("a rogue process could otherwise plant a symlink to a socket it controls"). `daisugi-gate.ts`
  does none of that, so this client is strictly weaker than the Python one on the same socket:
  any local process that wins `gateSockPath()` answers `{"exit_code":0}` and every tool call is
  allowed. `OPENDAISUGI_GATE_SOCK` makes it easier still, since the plugin runs inside the
  process whose env the harness controls. Fix: add
  `function gateSocketTrustworthy(p: string): boolean` using `fs.lstatSync(p)` —
  `st.isSocket() && st.uid === process.getuid() && (st.mode & 0o777) === 0o600`, wrapped in
  try/catch returning false — and call it before `net.createConnection`; a false verdict returns
  the same "unreachable" deny. Add the test named for the failure:
  `askGate: a socket that is not private and ours denies`. plan-04 Task 4 carries the identical
  correction; write the helper once and keep the two copies textually identical. — applied in
  Task 6 Step 3 (`gateSocketTrustworthy`, called from `askGate` before any `net.createConnection`)
  and Step 1 (three dedicated tests: rejects a non-0600 socket, rejects a non-socket path, and
  the named failure test — a planted socket answering `exit_code 0` still denies).
- **BLOCKER — Tasks 6 and 7: `GATE_MODE` is baked into a file the agent can rewrite, and it
  beats `config.yaml`.** `resolve_gate_mode` (`gate.py:51-70`) is explicit: "the installed hook
  command passes `--mode` (the one thing the agent cannot rewrite), and `config.yaml` is
  user-writable — so config must never be able to override the flag, especially to flip an
  installed `enforce` down to `shadow`." Here the flag's value lives in
  `~/.config/opencode/plugins/daisugi-gate.ts` — a plain text file in a directory OpenCode
  itself reads and the agent can write — and it *wins* over `config.yaml`. One `sed` on that
  constant turns every deny into an allow. Fix: **omit `--mode` from the argv entirely** and let
  `resolve_gate_mode(None, root)` read `config.yaml` server-side, the same trust level without a
  higher-priority override. Blast radius, all authoritative: delete `GATE_MODE`,
  `_OPENCODE_MODE_PLACEHOLDER`, and the `.replace(...)` in `install_opencode_harness` (the
  plugin file becomes install-mode-independent, so idempotence gets simpler); keep or drop the
  `mode` parameter only as a `config.gate_mode` writer; and correct the install line, because
  with `--mode` omitted a fresh install resolves to **shadow** (`resolve_gate_mode` returns
  `"shadow"` on a missing or unreadable config), so "or every tool call will be denied" is false
  as written. Honest STE100 line: "OpenCode asks the gate in-process. The gate only watches
  until you set `gate_mode: enforce`. Start it with `daisugi start`." — applied in Task 6 Step 3
  (no `GATE_MODE`/`configMode`/`--mode` anywhere in the shipped plugin file) and Task 7 Steps 3-4
  (`install_opencode_harness` copies the file unmodified — no `mode` parameter, no
  `_OPENCODE_MODE_PLACEHOLDER`, no `.replace()`; the CLI prints the exact corrected STE100 line
  above instead of the false "or every tool call will be denied").
- **BLOCKER — Task 6 Step 3: `reportState` sends the gate-socket wire shape to the coppice
  socket, so no state event can ever land.** It connects to `COPPICE_SOCK` and writes
  `{"argv":["hook","report","--pane",…],"stdin_b64":…}`. Two contracts are crossed at once.
  coppice-server speaks master §3.3's request shape — plan-01 ≈448 sends
  `{"id":"r","cmd":"pane.report_state","pane":…,"event":…}` — and `hook report` is a **gate.sock**
  verb that plan-01 adds to `gate_server`'s dispatch (plan-01 ≈263), not a coppice command.
  coppice-server will answer `{"ok":false,"error":{"code":…}}` at best. Fix: match plan-04
  Task 5 — send `{"v":1,"argv":["hook","report","--pane",<pane>],"stdin_b64":…}` to the **gate
  socket** (`gateSockPath()`), and let plan-01's `report_state()` do the coppice/Herdr fan-out.
  The three `event`/`permission.ask` tests must then assert against a fake **gate** socket. —
  applied in Task 6 Step 3 (`reportState` sends `{"v":1,"argv":["hook","report","--pane",
  coppicePane],...}` to `gateSockPath()`, checked with the same `gateSocketTrustworthy` guard)
  and Step 1 (the `permission.ask` and `event` tests assert the request lands on the fake gate
  socket with that exact argv, and that `COPPICE_SOCK` is never dialed).
- **SHOULD-FIX — Task 6: `askGate` sends no `--root`, so the gate can read a different root than
  the socket it answered on.** `gate_client.py`'s `_root_from_argv` and plan-04's extension both
  derive `--root` from the socket path; this plugin omits it, so `run_argv` falls back to
  `DEFAULT_GATE_ROOT` even when `OPENDAISUGI_GATE_SOCK` points elsewhere — envelopes and shadow
  logs then come from a different directory than the server was started with. Fix: append
  `"--root", path.dirname(sockPath)`, matching plan-04 exactly. — applied in Task 6 Step 3
  (`askGate`'s argv includes `"--root", path.dirname(sockPath)`) and Step 1 (asserted verbatim
  in the request-shape test).
- **SHOULD-FIX — Task 6: the request omits `"v": 1`.** `gate_client.py:62` sends it and
  plan-04's extension sends it. `gate_server` ignores it today, so this is forward-compatibility,
  not a bug — but the three clients on one socket should be byte-identical in shape. — applied
  in Task 6 Step 3 (both `askGate` and `reportState` requests carry `"v": 1`) and Step 1 (the
  request-shape test asserts `req.v === 1`).
- **SHOULD-FIX — Task 6: no `--verify-timeout`, so the gate outlives the client's budget.** The
  plugin gives up at 5000 ms; the gate's default inner verify budget is 10.0 s (`gate.py:1021`).
  Every verify slower than 5 s becomes an "unreachable" deny instead of the verdict the gate was
  about to produce — fail-closed, but wrong-reasoned. Fix: append `"--verify-timeout", "4"`,
  mirroring `gate_settings_json`'s own `inner = min(verify_timeout_s, max(1.0, hook_timeout_s -
  5.0))` discipline. — applied in Task 6 Step 3 (`askGate`'s argv includes `"--verify-timeout",
  "4"`) and Step 1 (asserted verbatim in the request-shape test).
- **SHOULD-FIX — Task 7: name spec-05's `opencode.json` instruction as do-not-implement.** The
  spec says install "adds it to the user `opencode.json` `plugin` list if that is the documented
  mechanism". It is not: that array is npm packages only, and OpenCode runs `bun install` on its
  entries at startup, so a file path there is a broken startup, not a no-op. This plan's code is
  already right; add one line to Task 7 Step 3's docstring saying the spec line is wrong and
  must not be restored, so a later reader does not "fix" it back. — applied in Task 7 Step 3
  (a `CORRECTION` comment block above `install_opencode_harness` states this as fact and names
  why: OpenCode runs `bun install` on each `opencode.json` "plugin" entry at startup, so a local
  file path there is a broken startup, not a no-op).
- **SHOULD-FIX — Task 6: an unmapped OpenCode tool id can never be admitted by an envelope.**
  `TOOL_NAME_MAP` passes unknown ids through, `_classify_tool` returns `None` for anything not
  `mcp__`-prefixed, and the gate denies with "unrecognized tool". That is fail-closed and
  therefore acceptable, but it makes the envelope's `mcp_allowlist` unusable for OpenCode's MCP
  and plugin-registered tools, and it is the opposite of what plan-04 Task 3 does for pi (unknown
  → `mcp`, admissible by name). Fix, or state the asymmetry honestly in Task 6's docstring and in
  the `modules.py` note: either translate unmapped ids to `mcp__opencode__<id>` in the plugin, or
  say in one line that OpenCode's non-built-in tools are denied unconditionally in this version.
  — applied in Task 6 (the "state the asymmetry" option, not the `mcp__` translation, to keep
  this plan's scope bounded): `TOOL_NAME_MAP`'s own comment and PINS.md both say plainly that
  OpenCode's non-built-in tools are denied as "unrecognized tool" in this version, and name the
  asymmetry with pi's adapter by name.
- **SHOULD-FIX — Task 7 Step 5: the honesty note does not state the enforcement class.** The row
  reads "gate plugin installed". Master §3.5 and plan-04's own modules test both want the class
  visible. OpenCode's is the strongest of the set — in-process, deny-only, no fail-open timeout
  path — so say so: "in-process deny-only hook; fail-closed". — applied in Task 7 Step 5 (the
  `modules.py` row's note is exactly that string when installed) and the modules test asserts
  both substrings appear.
- **NOTE — the `Permission` type import is unverified.** Task 6 types the handler
  `(input: Permission)` and states as fact that `permission.ask` "is type-declared by
  `@opencode-ai/plugin`". Nothing in this review read that package's `.d.ts`, and upstream's
  public hook list does not contain the name. Runtime is unaffected (`import type` erases; an
  unknown hook key is inert), so this is a note, not a blocker — but per the brief an
  unverifiable fact stated as fact becomes a discovery step: have Task 1's script dump the
  `@opencode-ai/plugin` `.d.ts` hook keys into PINS.md, and type the handler `any` until it does.
  — applied in Task 1 Step 3 (`discover_plugin_hooks()` npm-installs the real package and reads
  its `Hooks` interface member names out of the shipped `index.d.ts`; PINS.md's own content,
  Step 4, records the verified list and the exact `tool.execute.before`/`permission.ask`/
  `Permission` shapes) and Task 6 Step 3 (`permission.ask`'s handler is typed `(input: any)`,
  not `Permission`, per this note's own instruction — kept even though Task 1 now verifies the
  type is real, since nothing calls this hook today and importing a type for it buys nothing).
- **NOTE — Task 4: `opts.Env` is appended after `OPENCODE_SERVER_PASSWORD`, so a pane env can
  silently break auth.** Later entries win in Go's `exec`. A pane whose env carries
  `OPENCODE_SERVER_PASSWORD` overrides the per-pane token and every client call 401s. Append
  `opts.Env` first, the token last. — applied in Task 4 Step 3: `opencodeEnv(o.Env, token)`
  copies `o.Env` and forces `OPENCODE_SERVER_PASSWORD` to the adapter's own token in that copy
  before it is ever passed to `pane.BuildEnv` as `extra` — the token wins regardless of
  `BuildEnv`'s own internal insertion order, tested directly
  (`TestOpencodeEnv_TokenAlwaysWinsOverPaneEnv`).
- **NOTE — Task 6's `permission.ask` registration is harmless and correctly reasoned.** Upstream
  lists only `permission.asked`/`permission.replied`; registering an unknown hook key costs
  nothing and the `event` filter is what actually fires. Keeping both, and never writing
  `output.status`, matches spec-05's crux. Leave it. — confirmed unchanged in Task 6 Step 3:
  `permission.ask` stays registered (typed `any`, per the NOTE above) alongside the `event`
  fallback, and neither ever assigns `output.status`.

Re-review 2026-09-08: 13 of 13 verified applied; open: none.

## Global Constraints

(Copied verbatim from `00-master-spec.md` §4 — binds every task below.)

- **Layer purity.** No module under `src/opendaisugi/` that is part of the layer (list in
  `tests/test_layer_boundary.py`, plan 00) may import from `opendaisugi.floor`, `opendaisugi.voice`,
  or `opendaisugi.coppice`. The test imports every layer module with those packages hidden.
- **Python 3.12, stdlib for the layer.** New hard deps in the layer: none. New extras allowed:
  `[floor]` (nothing yet — the client uses stdlib sockets), `[voice]`, `[int8]`, `[router]`.
- **Go 1.26** for `harness/coppice` (amended 2026-09-08: go-libghostty declares go 1.26.0; sprig stays on 1.25); module `github.com/opendaisugi/coppice`; `go vet` and
  `go test ./...` clean. Zig 0.16 and CMake are *build-time* requirements for go-libghostty; the
  plan installs both into `~/.local` without sudo (`uv tool install cmake`; Zig tarball).
- **Pins.** go-libghostty at the newest tag on the day plan 02 starts, recorded in `go.mod`
  and in `harness/coppice/PINS.md` together with the ghostty commit that binding builds.
  Herdr's vendored commit `c5a21edfcbc2d5b46540ad91b7980aca31f5f1f3` (their 1.3.2) is the
  reference for patch notes, not a requirement; the binding chooses the commit it fetches.
- **Tests.** `uv run --no-sync pytest -q` green; `uv run --no-sync ruff check .` clean;
  Go: `go test ./...` from `harness/coppice`. Never bare `uv run` (uv.lock is git-ignored; it
  re-resolves and strips extras).
- **Commits.** Atomic, stating the why, persona *OpenDaisugi Contributors*, **no AI-authorship
  attribution lines** (project policy overrides any session-level instruction). Never push;
  the public repo folds monthly.
- **`/tmp` is RAM.** Scratch on real disk; worktrees beside the repo.
- **Copy.** STE100 register in every user-facing string: short sentences, plain words, active
  voice, no em-dashes, no parentheticals. Errors teach the next command.
- **Exit codes.** Hooks: exit 2 = deny. CLI: 0 success, 1 user error, 2 gate deny, 3 unreachable.
- **Privacy.** No telemetry. No third-party relay in any default path. Push goes through a
  self-hosted ntfy or not at all.

**This plan's own binding facts, corrected against the review (superseding the paragraph that
used to sit here):**

- The wire request that actually reaches a real allow/deny is bare gate flags —
  `{"v":1,"argv":["--format","opencode","--root",R,"--verify-timeout","4"],"stdin_b64":…}` —
  matching what `gate_client.py`'s own `ask_server()` sends and what `run_argv`'s parser
  (`gate.py:1012-1045`) actually reads. `--mode` is **never** sent (see Task 6); the gate
  resolves it server-side from `config.yaml` via `resolve_gate_mode`. `["hook","record",…]`
  (this plan's own earlier text, and `spec-05-opencode.md`'s crux section) is not this path —
  `hook record` never blocks (`hook.py`'s `record_and_contract` docstring: "Never blocks").
- `daisugi hook report` is a *different*, narrower verb, reachable through the **same** socket
  with the argv shape `["hook","report","--pane",P]` (plan-01 ≈775-813's `hook_report_argv`,
  dispatched before the ordinary flag parser). It never denies; it validates, downgrades a
  claimed `gate`/`operator` source to `headless`, writes the session tree, and calls
  `opendaisugi._state_report.report_state()` — which does the *actual* coppice/Herdr fan-out
  from **its own** process environment, not the caller's. The plugin's job is only to reach
  the gate socket with this shape; it does not talk to `COPPICE_SOCK` directly (Task 6).
- `permission.ask` (the hook name `spec-05-opencode.md` shows) is type-declared by
  `@opencode-ai/plugin` (verified: Task 1 dumps the installed package's `Hooks` interface into
  PINS.md) but does not fire in shipping OpenCode (upstream anomalyco/opencode issues
  #9229/#7006: the active permission pipeline emits a bus event instead of calling
  `Plugin.trigger()`). The generic `event` hook filtered on `event.type === "permission.asked"`
  is the reliable signal. This plan registers both: `permission.ask` for the day it starts
  working, `event` as the signal that actually fires today. Neither ever writes
  `output.status` — the plugin observes, it does not decide.
- `harness/coppice/internal/pane`'s `Adapter`/`StartOpts`/`Proc`/`Event` are **plan-02's landed
  shapes** (`plan-02-coppice-server.md`, its own `internal/pane/adapter.go` listing —
  currently near line 4400, cited approximately because sibling plans are still being edited in
  parallel; grep `^type Proc interface` in that file for the exact line when acting on this
  plan), not this plan's earlier guess. `Proc` is a 6-method interface, not a struct; fields are
  `Cwd`/`Argv`/`Resume`/`Sock`/`PaneID`, not `Cwd`/`ExtraArgv`/`ResumeID`; `Event.Ask` is
  `*proto.Ask` (`internal/proto`, already imported by `internal/pane` for this field — verified
  live in the same file, not `state.Ask`); constants are `EvText`/`EvTool`/`EvState`/`EvEnd`/
  `EvError` and `pane.StateIdleStr`/`StateWorkingStr`/`StateBlockedStr`/`StateDoneStr`/
  `StateUnknownStr`. `harness/coppice/internal/adapters/opencode/opencode.go` **already exists**
  (plan-02 pre-registers the slot): `func New() pane.Adapter { return adapters.NotBuilt{
  AdapterName: "opencode", Owner: "spec-05"} }` plus `func init() { adapters.Register(New()) }`.
  This plan's job is to change that one function's body, not to create a second `New()`.
- `EXIT_CODE_FORMATS` is built **once**, by `plan-04-pi.md` Task 3 — both plans independently
  proposed the identical fix for the identical fail-open (`stdout_for_format` has no `opencode`
  case either, so a deny on this format was returning `{"continue": true}` *and* `exit_code 0`:
  it failed open on both channels). This plan does not touch `gate.py`'s `_outcome()`. See
  Task 5 for the resulting narrower scope and the explicit cross-plan dependency this creates.

---

### Task 1: The OpenCode API pin (discovery)

Unchanged in substance from this plan's first draft (the review's own words: "ready to build
now" — plugins load from `.opencode/plugins/` and `~/.config/opencode/plugins/` with no
`opencode.json` entry, the hook list has no `permission.ask` in the *event* sense, `permission
.asked`/`permission.replied` are the real bus events, `tool.execute.before` denies by throw, and
`output.args.filePath` is the real key for file tools). One addition responds to the review's
NOTE on the unverified `Permission` import: Step 3's script now also dumps the installed
`@opencode-ai/plugin` package's `Hooks` interface member names into PINS.md, so Task 6 types
against a recorded fact instead of an assertion.

**Files:**
- Create: `scripts/opencode_discover.py`
- Create: `harness/coppice/internal/adapters/opencode/PINS.md`
- Test: `tests/test_opencode_pins.py`

**Interfaces:**
- Produces: `harness/coppice/internal/adapters/opencode/PINS.md` (read by Tasks 2-7 for every
  endpoint path, field name, and tool id they use). `scripts/opencode_discover.py` — no
  importable symbols; a standalone refresh tool (`uv run --no-sync python
  scripts/opencode_discover.py`), matching `scripts/matcher_fpr.py`'s shape.

- [ ] **Step 1: Write the failing test**

```python
# tests/test_opencode_pins.py
"""The OpenCode API pin (spec-05 Task 1): a static fact file, not a live test.

Later tasks read PINS.md instead of re-deriving these facts, so this test only
checks the committed file — it never needs `opencode` or the network to pass.
"""

from __future__ import annotations

from pathlib import Path

PINS = (
    Path(__file__).resolve().parents[1]
    / "harness"
    / "coppice"
    / "internal"
    / "adapters"
    / "opencode"
    / "PINS.md"
)


def test_pins_file_exists():
    assert PINS.exists(), "run `uv run --no-sync python scripts/opencode_discover.py`"


def test_pins_records_the_server_surface():
    text = PINS.read_text(encoding="utf-8")
    for fact in (
        "OPENCODE_SERVER_PASSWORD",
        "OPENCODE_SERVER_USERNAME",
        "GET /doc",
        "POST /session",
        "POST /session/{id}/message",
        "GET /event",
        '"type": "text"',
    ):
        assert fact in text, f"missing pinned fact: {fact!r}"


def test_pins_records_the_tool_ids():
    text = PINS.read_text(encoding="utf-8")
    for tool_id in ("bash", "read", "write", "edit", "glob", "grep", "webfetch", "websearch"):
        assert tool_id in text, f"missing pinned tool id: {tool_id!r}"


def test_pins_records_the_permission_ask_caveat():
    text = PINS.read_text(encoding="utf-8")
    assert "permission.asked" in text
    assert "9229" in text or "7006" in text  # the upstream issue, so the caveat is checkable


def test_pins_records_the_plugin_hook_keys():
    """Responds to the review's NOTE: the Permission import was asserted, not
    verified. This asserts the pin file records the REAL hook key list, not
    a paraphrase."""
    text = PINS.read_text(encoding="utf-8")
    for hook_key in ("tool.execute.before", "permission.ask", "event", "tool.execute.after"):
        assert hook_key in text, f"missing pinned plugin hook key: {hook_key!r}"
```

- [ ] **Step 2: Run test to verify it fails**

Run: `uv run --no-sync pytest tests/test_opencode_pins.py -q`
Expected: FAIL — `PINS.md` does not exist yet.

- [ ] **Step 3: Write the discovery script**

```python
#!/usr/bin/env python3
# scripts/opencode_discover.py
"""Refresh harness/coppice/internal/adapters/opencode/PINS.md against a real,
locally-started `opencode serve`, and against the `@opencode-ai/plugin`
package's shipped type declarations. Not a test: it prints, it doesn't
assert (same shape as scripts/matcher_fpr.py).

Requires the `opencode` CLI on PATH (`npm install -g opencode-ai`) for the
server half, and `npm`/`node` on PATH for the plugin-hooks half. Either half
missing prints a teaching message and leaves that half of PINS.md as
whatever this plan committed — captured against opencode-ai 1.18.29 and
@opencode-ai/plugin 1.18.29 on 2026-09-08.

Usage: uv run --no-sync python scripts/opencode_discover.py
"""

from __future__ import annotations

import base64
import json
import os
import re
import secrets
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import urllib.request
from pathlib import Path

PINS_PATH = (
    Path(__file__).resolve().parents[1]
    / "harness"
    / "coppice"
    / "internal"
    / "adapters"
    / "opencode"
    / "PINS.md"
)


def _free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def _get(url: str, user: str, password: str) -> tuple[int, bytes]:
    req = urllib.request.Request(url)
    token = base64.b64encode(f"{user}:{password}".encode()).decode()
    req.add_header("Authorization", f"Basic {token}")
    try:
        with urllib.request.urlopen(req, timeout=5) as resp:
            return resp.status, resp.read()
    except urllib.error.HTTPError as exc:  # noqa: BLE001 — we want the status either way
        return exc.code, b""


def discover_server() -> dict | None:
    if shutil.which("opencode") is None:
        print(
            "opencode is not on PATH. Install it with `npm install -g opencode-ai` "
            "and re-run this script to refresh the server half of PINS.md."
        )
        return None
    port = _free_port()
    password = secrets.token_hex(8)
    proc = subprocess.Popen(
        ["opencode", "serve", "--port", str(port), "--hostname", "127.0.0.1"],
        env={"OPENCODE_SERVER_PASSWORD": password, "PATH": os.environ["PATH"]},
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    try:
        base = f"http://127.0.0.1:{port}"
        deadline = time.monotonic() + 15
        status = 0
        while time.monotonic() < deadline:
            status, _ = _get(f"{base}/doc", "opencode", password)
            if status == 200:
                break
            time.sleep(0.5)
        if status != 200:
            raise RuntimeError(f"opencode serve never became ready (last /doc status {status})")
        _, tool_ids_raw = _get(f"{base}/experimental/tool/ids", "opencode", password)
        tool_ids = json.loads(tool_ids_raw)
        _, version_raw = _get(f"{base}/doc", "opencode", password)
        doc = json.loads(version_raw)
        version = doc.get("info", {}).get("version", "unknown")
        return {"version": version, "tool_ids": tool_ids}
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()


def discover_plugin_hooks() -> dict | None:
    """npm-install @opencode-ai/plugin into a throwaway temp dir and read the
    `Hooks` interface member names straight out of its shipped `index.d.ts` —
    the review's NOTE: don't assert a hook exists, dump it."""
    if shutil.which("npm") is None:
        print("npm is not on PATH. Skipping the plugin-hooks half of PINS.md.")
        return None
    with tempfile.TemporaryDirectory() as td:
        try:
            subprocess.run(
                ["npm", "install", "--no-audit", "--no-fund", "@opencode-ai/plugin@latest"],
                cwd=td,
                check=True,
                capture_output=True,
                timeout=120,
            )
        except (subprocess.CalledProcessError, subprocess.TimeoutExpired) as exc:
            print(f"could not install @opencode-ai/plugin: {exc}. Skipping this half.")
            return None
        pkg = json.loads(
            Path(td, "node_modules", "@opencode-ai", "plugin", "package.json").read_text()
        )
        dts = Path(td, "node_modules", "@opencode-ai", "plugin", "dist", "index.d.ts").read_text()
    # The Hooks interface member names, e.g. `"tool.execute.before"?:` or `event?:`.
    hook_keys = sorted(set(re.findall(r'^\s*(?:"([\w.]+)"|(\w+))\??:', dts, re.M)))
    hook_keys = sorted({a or b for a, b in hook_keys})
    return {"version": pkg["version"], "hook_keys": hook_keys}


def render(server: dict | None, plugin: dict | None, *, previous: str = "") -> str:
    server = server or {
        "version": "1.18.29 (unrefreshed — opencode was not on PATH; see below)",
        "tool_ids": [
            "invalid",
            "question",
            "bash",
            "read",
            "glob",
            "grep",
            "edit",
            "write",
            "task",
            "webfetch",
            "todowrite",
            "websearch",
            "skill",
            "apply_patch",
        ],
    }
    plugin = plugin or {
        "version": "1.18.29 (unrefreshed — npm was not on PATH; see below)",
        "hook_keys": [
            "dispose",
            "event",
            "config",
            "tool",
            "auth",
            "provider",
            "chat.message",
            "chat.params",
            "chat.headers",
            "permission.ask",
            "command.execute.before",
            "tool.execute.before",
            "shell.env",
            "tool.execute.after",
            "experimental.chat.messages.transform",
            "experimental.chat.system.transform",
            "experimental.provider.small_model",
            "experimental.session.compacting",
            "experimental.compaction.autocontinue",
            "experimental.text.complete",
            "tool.definition",
        ],
    }
    tool_ids = ", ".join(f"`{t}`" for t in server["tool_ids"])
    hook_keys = ", ".join(f"`{k}`" for k in plugin["hook_keys"])
    return f"""# OpenCode API pins (spec-05 Task 1)

Captured live against `opencode-ai` {server["version"]} and `@opencode-ai/plugin`
{plugin["version"]}. Re-run `uv run --no-sync python scripts/opencode_discover.py`
to refresh after an OpenCode upgrade; this file is what Tasks 2-7 of
plan-05-opencode.md read instead of re-deriving these facts.

## Server (`opencode serve`)

- Start: `opencode serve --port <N> --hostname 127.0.0.1`.
- Auth: HTTP Basic. `OPENCODE_SERVER_PASSWORD` sets the password;
  `OPENCODE_SERVER_USERNAME` sets the username (default `opencode`). No auth
  or wrong auth on any endpoint returns `401` with
  `WWW-Authenticate: Basic realm="Secure Area"` -- that header, or a `200`, is
  how a readiness poll knows the port is accepting connections yet.
- `GET /doc` -- the live OpenAPI 3.1 document. Confirms the running version.

## Session + message

- `POST /session` -- body `{{"title"?: string, ...}}` (all fields optional).
  Returns a `Session` object; only `id` matters to this plan.
- `GET /session/{{id}}` -- `200` with the `Session` if it still exists (used to
  test a resume id before trusting it), else `404`.
- `POST /session/{{id}}/message` -- body `{{"parts": [...]}}`; `parts` is the
  only required field. One part: `{{"type": "text", "text": "..."}}`. `model`
  is optional -- omit it and the session's own configured default is used.

## Events (`GET /event`)

Server-sent events: `data: {{json}}\\n\\n` per line, no `event:` field (the
type lives in the JSON). First event is always `server.connected`;
`server.heartbeat` recurs. Relevant `type` values and their `properties`:

- `session.status` -- `{{sessionID, status: {{type: "idle"|"retry"|"busy"}}}}`.
- `session.idle` -- `{{sessionID}}`.
- `session.error` -- `{{sessionID, error: {{...}}}}`.
- `permission.asked` -- `{{id, sessionID, permission, patterns: [...],
  metadata, always, tool?: {{messageID, callID}}}}`. This is the reliable
  signal for a pending permission prompt (see the plugin caveat below).
- `message.part.updated` -- `{{sessionID, part: {{type, text, ...}}}}` for a
  text part; used for transcript text, not state.

## Built-in tool ids (`GET /experimental/tool/ids`, live-confirmed)

{tool_ids}

Of these, `hook.py`'s `_TOOL_TYPE_MAP` already recognizes the capitalized
Claude-style names for: bash->Bash, read->Read, write->Write, edit->Edit,
glob->Glob, grep->Grep, webfetch->WebFetch, websearch->WebSearch. The plugin
translates client-side (same pattern as `harness/sprig/daisugi_gate.go`'s
`gateVocabulary`) -- no `hook.py` classifier change needed for tool names.
`hook.py` DOES need one change: its path extraction doesn't recognize
`filePath`, which is the field OpenCode's `read`/`write`/`edit` tools use for
the target path per the `@opencode-ai/plugin` docs example (confirmed).
Unmapped ids (`question`, `task`, `todowrite`, `skill`, `apply_patch`,
`invalid`) pass through untranslated and are denied as "unrecognized tool" --
fail-closed, but it means the envelope's `mcp_allowlist` cannot admit
OpenCode's non-built-in tools in this version (Task 6's docstring says so).

## Plugin surface (`@opencode-ai/plugin`, dumped from its shipped `index.d.ts`)

Full `Hooks` interface member names, read directly out of the installed
package (not asserted from docs -- the review's NOTE, resolved):

{hook_keys}

- Local `.ts`/`.js` files in `~/.config/opencode/plugins/` (global) or
  `.opencode/plugins/` (project) load automatically. The `"plugin"` array in
  `opencode.json` is for **npm package** plugins only -- a local file needs no
  registration there.
- `Hooks["tool.execute.before"]?: (input: {{tool, sessionID, callID}}, output:
  {{args}}) => Promise<void>`. Throw to deny; there is no return-value block
  signal. `input` carries no `args` -- only `output.args` does.
- `Hooks["permission.ask"]?: (input: Permission, output: {{status: "ask" |
  "deny" | "allow"}}) => Promise<void>`. **Known not to fire** in shipping
  OpenCode (github.com/anomalyco/opencode issues #9229 and #7006: the active
  permission pipeline calls `PermissionNext.ask()`, which emits a bus event
  instead of calling `Plugin.trigger()`). Register it anyway (costs nothing,
  fixed for free the day upstream fixes it) but never touch `output.status`.
  Task 6 types this handler's input `any`, not `Permission` -- one further
  step of defense past this file's own verification, since a future
  OpenCode release is free to change or drop a hook nothing calls today.
- `Permission = {{id, type, pattern?, sessionID, messageID, callID?, title,
  metadata, time: {{created}}}}` (from `@opencode-ai/sdk`'s generated types,
  read the same way).
- `Hooks["event"]?: (input: {{event}}) => Promise<void>` -- the reliable
  signal for `permission.asked` today.
"""


def main() -> int:
    server = discover_server()
    plugin = discover_plugin_hooks()
    PINS_PATH.parent.mkdir(parents=True, exist_ok=True)
    PINS_PATH.write_text(render(server, plugin), encoding="utf-8")
    print(f"wrote {PINS_PATH}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
```

- [ ] **Step 4: Commit the checked-in pin file from this plan's own live capture**

The script above regenerates `PINS.md` when `opencode`/`npm` are installed. Commit the file now
with the facts already captured while writing this plan (both the live `opencode serve` capture
and the live `@opencode-ai/plugin` package inspection), so Step 2's test passes without network
access during plan execution. Write `harness/coppice/internal/adapters/opencode/PINS.md` with
exactly the content `render(None, None)` produces (the fallback branches use this plan's own
verified capture: `opencode-ai` 1.18.29's tool ids and `@opencode-ai/plugin` 1.18.29's `Hooks`
member names — `dispose`, `event`, `config`, `tool`, `auth`, `provider`, `chat.message`,
`chat.params`, `chat.headers`, `permission.ask`, `command.execute.before`,
`tool.execute.before`, `shell.env`, `tool.execute.after`, and the six `experimental.*`/
`tool.definition` keys listed in the function above).

- [ ] **Step 5: Run the test to verify it passes**

Run: `uv run --no-sync pytest tests/test_opencode_pins.py -q`
Expected: PASS (5 tests).

- [ ] **Step 6: Lint and commit**

Run: `uv run --no-sync ruff check scripts/opencode_discover.py tests/test_opencode_pins.py`

```bash
git add scripts/opencode_discover.py harness/coppice/internal/adapters/opencode/PINS.md tests/test_opencode_pins.py
git commit -m "opencode: pin the live API surface and the real plugin hook keys, not a paraphrase"
```

---

### Task 2: Go — the OpenCode HTTP client core

This task is unchanged by the review — `client.go` never references `internal/pane`; it is a
plain HTTP client tested entirely against `httptest.Server`. The review's BLOCKER on the
`internal/pane` contract lands in Task 4, where those types are actually used.

**Prerequisite check (run before writing any code in this task):**

```bash
test -f harness/coppice/go.mod && grep -q "module github.com/opendaisugi/coppice" harness/coppice/go.mod
```

If this fails, STOP: plan 02 has not landed yet. Teaching message: "harness/coppice does not
exist or is not the coppice module — run plan-02-coppice-server.md's tasks first."

**Files:**
- Create: `harness/coppice/internal/adapters/opencode/client.go`
- Test: `harness/coppice/internal/adapters/opencode/client_test.go`

**Interfaces:**
- Consumes: nothing from `internal/pane` — pure `net/http`.
- Produces: `newClient(baseURL, username, password string) *client`; `(*client).waitReady(ctx)
  error`; `(*client).createSession(ctx, title string) (string, error)`; `(*client)
  .sessionExists(ctx, id string) bool`; `(*client).postMessage(ctx, sessionID, text string)
  error`; `(*client).do(ctx, method, path string, body any) (*http.Response, error)` — Task 3
  and Task 4 call `do` directly for `/event`.

- [ ] **Step 1: Write the failing tests**

```go
// harness/coppice/internal/adapters/opencode/client_test.go
package opencode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClient_CreateSession_SendsBasicAuthAndReturnsID(t *testing.T) {
	var gotUser, gotPass string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, _ = r.BasicAuth()
		if r.URL.Path != "/session" || r.Method != http.MethodPost {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"id": "ses_abc123"})
	}))
	defer srv.Close()

	c := newClient(srv.URL, "opencode", "secret-token")
	id, err := c.createSession(context.Background(), "coppice pane")
	if err != nil {
		t.Fatalf("createSession: %v", err)
	}
	if id != "ses_abc123" {
		t.Fatalf("got id %q, want ses_abc123", id)
	}
	if gotUser != "opencode" || gotPass != "secret-token" {
		t.Fatalf("basic auth not sent: user=%q pass=%q", gotUser, gotPass)
	}
	if gotBody["title"] != "coppice pane" {
		t.Fatalf("title not sent: %v", gotBody)
	}
}

func TestClient_CreateSession_NonOKStatusIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"name":"UnknownError"}`))
	}))
	defer srv.Close()

	c := newClient(srv.URL, "opencode", "x")
	if _, err := c.createSession(context.Background(), "t"); err == nil {
		t.Fatal("want an error on a 500 response")
	}
}

func TestClient_SessionExists_TrueOn200FalseOn404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/session/ses_live" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := newClient(srv.URL, "opencode", "x")
	if !c.sessionExists(context.Background(), "ses_live") {
		t.Fatal("want true for an existing session")
	}
	if c.sessionExists(context.Background(), "ses_gone") {
		t.Fatal("want false for a missing session")
	}
}

func TestClient_PostMessage_SendsOneTextPart(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newClient(srv.URL, "opencode", "x")
	if err := c.postMessage(context.Background(), "ses_1", "say hi"); err != nil {
		t.Fatalf("postMessage: %v", err)
	}
	if gotPath != "/session/ses_1/message" {
		t.Fatalf("got path %q", gotPath)
	}
	parts, _ := gotBody["parts"].([]any)
	if len(parts) != 1 {
		t.Fatalf("want one part, got %v", gotBody)
	}
	part := parts[0].(map[string]any)
	if part["type"] != "text" || part["text"] != "say hi" {
		t.Fatalf("got part %v", part)
	}
}

func TestClient_WaitReady_SucceedsOn200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newClient(srv.URL, "opencode", "x")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.waitReady(ctx); err != nil {
		t.Fatalf("waitReady: %v", err)
	}
}

func TestClient_WaitReady_TimesOutWhenNeverReady(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := newClient(srv.URL, "opencode", "x")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := c.waitReady(ctx); err == nil {
		t.Fatal("want a timeout error")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd harness/coppice && go test ./internal/adapters/opencode/... -run TestClient -v`
Expected: FAIL — `client.go` (`newClient`, etc.) does not exist yet.

- [ ] **Step 3: Write the client**

```go
// harness/coppice/internal/adapters/opencode/client.go
// Package opencode drives `opencode serve` as a headless coppice pane
// (spec-05). See PINS.md for every endpoint path, field name, and status
// code this file assumes — captured live against opencode-ai 1.18.29.
package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// client is a thin HTTP client bound to one running `opencode serve`
// process. Every method here is a single request; SSE streaming lives in
// events.go (Task 3).
type client struct {
	baseURL    string
	username   string
	password   string
	httpClient *http.Client
}

func newClient(baseURL, username, password string) *client {
	return &client{
		baseURL:    baseURL,
		username:   username,
		password:   password,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *client) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("opencode adapter: encode request: %w", err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, r)
	if err != nil {
		return nil, fmt.Errorf("opencode adapter: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.SetBasicAuth(c.username, c.password)
	return c.httpClient.Do(req)
}

// waitReady polls GET /doc (PINS.md) until it answers 200 or ctx expires.
func (c *client) waitReady(ctx context.Context) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		resp, err := c.do(ctx, http.MethodGet, "/doc", nil)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("opencode adapter: server did not become ready: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

type sessionInfo struct {
	ID string `json:"id"`
}

// createSession calls POST /session (PINS.md: body {"title"?}, all fields
// optional; only "id" matters here).
func (c *client) createSession(ctx context.Context, title string) (string, error) {
	resp, err := c.do(ctx, http.MethodPost, "/session", map[string]string{"title": title})
	if err != nil {
		return "", fmt.Errorf("opencode adapter: create session: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("opencode adapter: create session: %s: %s", resp.Status, b)
	}
	var info sessionInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return "", fmt.Errorf("opencode adapter: decode session: %w", err)
	}
	return info.ID, nil
}

// sessionExists calls GET /session/{id} — used to test a resume id before
// trusting it (OpenCode persists sessions across server restarts).
func (c *client) sessionExists(ctx context.Context, id string) bool {
	resp, err := c.do(ctx, http.MethodGet, "/session/"+id, nil)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// postMessage calls POST /session/{id}/message with one text part (PINS.md:
// body {"parts":[{"type":"text","text":...}]}; model omitted so the
// session's own configured default agent/model is used).
func (c *client) postMessage(ctx context.Context, sessionID, text string) error {
	body := map[string]any{
		"parts": []map[string]string{{"type": "text", "text": text}},
	}
	resp, err := c.do(ctx, http.MethodPost, "/session/"+sessionID+"/message", body)
	if err != nil {
		return fmt.Errorf("opencode adapter: post message: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("opencode adapter: post message: %s: %s", resp.Status, b)
	}
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd harness/coppice && go build ./... && go test ./internal/adapters/opencode/... -run TestClient -v`
Expected: PASS (6 tests).

- [ ] **Step 5: Vet and commit**

Run: `cd harness/coppice && go vet ./internal/adapters/opencode/...`

```bash
git add harness/coppice/internal/adapters/opencode/client.go harness/coppice/internal/adapters/opencode/client_test.go
git commit -m "opencode adapter: HTTP client for session create, message post, readiness"
```

---

### Task 3: Go — SSE event parsing and state mapping

**BLOCKER 2 applied here and in Task 4.** Rewritten against plan-02's landed
`internal/pane/adapter.go` (verbatim, not this plan's earlier guess): `pane.Event`'s `Kind`
field takes `pane.EvText`/`EvTool`/`EvState`/`EvEnd`/`EvError` (not `EventText`/…), `State` is a
plain string set from `pane.StateWorkingStr`/`StateIdleStr`/`StateBlockedStr`/`StateUnknownStr`
(not a second enum), and a blocked event's ask travels on `Event.Ask *proto.Ask` — plan-02 has
already landed that field (`internal/proto`, imported by `internal/pane` for exactly this;
`proto.Ask{ID, Tool, Summary string; Deadline float64}`, the same shape this plan always used).

**Files:**
- Create: `harness/coppice/internal/adapters/opencode/events.go`
- Create: `harness/coppice/internal/adapters/opencode/testdata/session_lifecycle.sse`
- Test: `harness/coppice/internal/adapters/opencode/events_test.go`

**Interfaces:**
- Consumes: `github.com/opendaisugi/coppice/internal/pane` (`Event`, `EvText`/`EvTool`/
  `EvState`/`EvEnd`/`EvError`, `StateWorkingStr`/`StateIdleStr`/`StateBlockedStr`/
  `StateUnknownStr`) and `github.com/opendaisugi/coppice/internal/proto` (`Ask`) — both
  plan-02's landed packages.
- Produces: `parseSSELine(line string) (busEvent, bool)`; `translateEvent(ev busEvent,
  ourSessionID string, now func() float64) (pane.Event, bool)`; `streamEvents(r io.Reader,
  ourSessionID string, now func() float64, out chan<- pane.Event)` — Task 4 calls
  `streamEvents` in its own goroutine.

- [ ] **Step 1: Record the fixture**

```
# harness/coppice/internal/adapters/opencode/testdata/session_lifecycle.sse
#
# Real SSE captured from `opencode serve` 1.18.29 (PINS.md), session
# ses_f7e52d6e1ffePj7Cw2stUE5v9d, redacted of local paths. The
# permission.asked line is CONSTRUCTED from the pinned schema (PINS.md), not
# captured live — the prompt that produced this trace never reached a real
# tool call (no provider credentials in the capture environment).

data: {"id":"evt_1","type":"server.connected","properties":{}}

data: {"id":"evt_2","type":"session.created","properties":{"sessionID":"ses_f7e52d6e1ffePj7Cw2stUE5v9d","info":{"id":"ses_f7e52d6e1ffePj7Cw2stUE5v9d"}}}

data: {"id":"evt_3","type":"message.part.updated","properties":{"sessionID":"ses_f7e52d6e1ffePj7Cw2stUE5v9d","part":{"type":"text","text":"say hi"}}}

data: {"id":"evt_4","type":"session.status","properties":{"sessionID":"ses_f7e52d6e1ffePj7Cw2stUE5v9d","status":{"type":"busy"}}}

data: {"id":"evt_5","type":"permission.asked","properties":{"id":"per_1","sessionID":"ses_f7e52d6e1ffePj7Cw2stUE5v9d","permission":"bash","patterns":["rm -rf *"],"metadata":{},"always":[]}}

data: {"id":"evt_6","type":"session.error","properties":{"sessionID":"ses_f7e52d6e1ffePj7Cw2stUE5v9d","error":{"name":"UnknownError","data":{"message":"Model not found"}}}}

data: {"id":"evt_7","type":"session.status","properties":{"sessionID":"ses_f7e52d6e1ffePj7Cw2stUE5v9d","status":{"type":"idle"}}}

data: {"id":"evt_8","type":"session.idle","properties":{"sessionID":"ses_f7e52d6e1ffePj7Cw2stUE5v9d"}}

data: {"id":"evt_9","type":"server.heartbeat","properties":{}}

data: {"id":"evt_10","type":"session.status","properties":{"sessionID":"ses_OTHER_PANE","status":{"type":"busy"}}}
```

- [ ] **Step 2: Write the failing tests**

```go
// harness/coppice/internal/adapters/opencode/events_test.go
package opencode

import (
	"os"
	"testing"

	"github.com/opendaisugi/coppice/internal/pane"
)

const ourSession = "ses_f7e52d6e1ffePj7Cw2stUE5v9d"

func fixedNow() float64 { return 1000.0 }

func TestParseSSELine_DataLineDecodes(t *testing.T) {
	ev, ok := parseSSELine(`data: {"id":"evt_1","type":"session.idle","properties":{"sessionID":"s1"}}`)
	if !ok {
		t.Fatal("want ok=true for a data: line")
	}
	if ev.Type != "session.idle" {
		t.Fatalf("got type %q", ev.Type)
	}
}

func TestParseSSELine_BlankAndMalformedLinesAreSkipped(t *testing.T) {
	if _, ok := parseSSELine(""); ok {
		t.Fatal("want ok=false for a blank line")
	}
	if _, ok := parseSSELine("data: not json"); ok {
		t.Fatal("want ok=false for malformed JSON")
	}
}

func TestTranslateEvent_SessionStatusBusyIsWorking(t *testing.T) {
	ev, _ := parseSSELine(`data: {"id":"e","type":"session.status","properties":{"sessionID":"s1","status":{"type":"busy"}}}`)
	pe, ok := translateEvent(ev, "s1", fixedNow)
	if !ok || pe.Kind != pane.EvState || pe.State != pane.StateWorkingStr {
		t.Fatalf("got %+v ok=%v", pe, ok)
	}
}

func TestTranslateEvent_SessionIdleIsIdle(t *testing.T) {
	ev, _ := parseSSELine(`data: {"id":"e","type":"session.idle","properties":{"sessionID":"s1"}}`)
	pe, ok := translateEvent(ev, "s1", fixedNow)
	if !ok || pe.State != pane.StateIdleStr {
		t.Fatalf("got %+v ok=%v", pe, ok)
	}
}

func TestTranslateEvent_SessionErrorIsUnknown(t *testing.T) {
	ev, _ := parseSSELine(`data: {"id":"e","type":"session.error","properties":{"sessionID":"s1","error":{"name":"X"}}}`)
	pe, ok := translateEvent(ev, "s1", fixedNow)
	if !ok || pe.State != pane.StateUnknownStr {
		t.Fatalf("got %+v ok=%v", pe, ok)
	}
}

func TestTranslateEvent_PermissionAskedIsBlockedWithAsk(t *testing.T) {
	ev, _ := parseSSELine(`data: {"id":"e","type":"permission.asked","properties":{"id":"per_1","sessionID":"s1","permission":"bash","patterns":["rm -rf *"],"metadata":{},"always":[]}}`)
	pe, ok := translateEvent(ev, "s1", fixedNow)
	if !ok || pe.State != pane.StateBlockedStr || pe.Ask == nil {
		t.Fatalf("got %+v ok=%v", pe, ok)
	}
	if pe.Ask.ID != "per_1" || pe.Ask.Tool != "bash" || pe.Ask.Deadline != 1090.0 {
		t.Fatalf("got ask %+v", pe.Ask)
	}
}

func TestTranslateEvent_AnotherSessionsEventIsSkipped(t *testing.T) {
	ev, _ := parseSSELine(`data: {"id":"e","type":"session.idle","properties":{"sessionID":"ses_OTHER_PANE"}}`)
	if _, ok := translateEvent(ev, ourSession, fixedNow); ok {
		t.Fatal("an event for a different session must not translate")
	}
}

func TestTranslateEvent_ServerConnectedAndHeartbeatAreSkipped(t *testing.T) {
	ev, _ := parseSSELine(`data: {"id":"e","type":"server.connected","properties":{}}`)
	if _, ok := translateEvent(ev, ourSession, fixedNow); ok {
		t.Fatal("server.connected carries no session state and must be skipped")
	}
}

func TestStreamEvents_FixtureProducesExpectedSequence(t *testing.T) {
	f, err := os.Open("testdata/session_lifecycle.sse")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	out := make(chan pane.Event, 16)
	streamEvents(f, ourSession, fixedNow, out)
	close(out)

	var states []string
	for pe := range out {
		if pe.Kind == pane.EvState {
			states = append(states, pe.State)
		}
	}
	want := []string{
		pane.StateWorkingStr, pane.StateBlockedStr, pane.StateUnknownStr,
		pane.StateIdleStr, pane.StateIdleStr,
	}
	if len(states) != len(want) {
		t.Fatalf("got states %v, want %v", states, want)
	}
	for i := range want {
		if states[i] != want[i] {
			t.Fatalf("got states %v, want %v", states, want)
		}
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `cd harness/coppice && go test ./internal/adapters/opencode/... -run 'TestParseSSELine|TestTranslateEvent|TestStreamEvents' -v`
Expected: FAIL — `events.go` does not exist yet.

- [ ] **Step 4: Write the parser**

```go
// harness/coppice/internal/adapters/opencode/events.go
package opencode

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"

	"github.com/opendaisugi/coppice/internal/pane"
	"github.com/opendaisugi/coppice/internal/proto"
)

// busEvent is one line of OpenCode's /event SSE stream (PINS.md: `data:
// {json}\n\n`, no `event:` field — the type lives in the JSON body).
type busEvent struct {
	Type       string          `json:"type"`
	Properties json.RawMessage `json:"properties"`
}

// parseSSELine decodes one "data: {...}" line. Anything else (blank,
// comment, malformed JSON) returns ok=false — the caller skips it rather
// than surfacing a parse error as pane state (§3.6: a parse error marks the
// pane unknown only when it comes from the event STREAM breaking, not from
// one skippable line; see streamEvents below).
func parseSSELine(line string) (busEvent, bool) {
	const prefix = "data: "
	if !strings.HasPrefix(line, prefix) {
		return busEvent{}, false
	}
	var ev busEvent
	if err := json.Unmarshal([]byte(strings.TrimPrefix(line, prefix)), &ev); err != nil {
		return busEvent{}, false
	}
	return ev, true
}

type sessionScoped struct {
	SessionID string `json:"sessionID"`
}

type sessionStatusProps struct {
	SessionID string `json:"sessionID"`
	Status    struct {
		Type string `json:"type"`
	} `json:"status"`
}

type permissionAskedProps struct {
	ID         string   `json:"id"`
	SessionID  string   `json:"sessionID"`
	Permission string   `json:"permission"`
	Patterns   []string `json:"patterns"`
}

type textPartProps struct {
	SessionID string `json:"sessionID"`
	Part      struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"part"`
}

// translateEvent maps one bus event scoped to ourSessionID into a
// pane.Event, per PINS.md's pinned shapes. An event for a different
// session, or a type we don't map (including server.connected and
// server.heartbeat, which carry no session), returns ok=false — never
// surfaced as a state flap for traffic that isn't ours.
func translateEvent(ev busEvent, ourSessionID string, now func() float64) (pane.Event, bool) {
	var scope sessionScoped
	if json.Unmarshal(ev.Properties, &scope) != nil || scope.SessionID != ourSessionID {
		return pane.Event{}, false
	}
	switch ev.Type {
	case "session.status":
		var p sessionStatusProps
		if json.Unmarshal(ev.Properties, &p) != nil {
			return pane.Event{}, false
		}
		switch p.Status.Type {
		case "busy", "retry":
			return pane.Event{Kind: pane.EvState, State: pane.StateWorkingStr}, true
		case "idle":
			return pane.Event{Kind: pane.EvState, State: pane.StateIdleStr}, true
		}
		return pane.Event{}, false
	case "session.idle":
		return pane.Event{Kind: pane.EvState, State: pane.StateIdleStr}, true
	case "session.error":
		return pane.Event{Kind: pane.EvState, State: pane.StateUnknownStr, Detail: string(ev.Properties)}, true
	case "permission.asked":
		var p permissionAskedProps
		if json.Unmarshal(ev.Properties, &p) != nil {
			return pane.Event{}, false
		}
		return pane.Event{
			Kind:  pane.EvState,
			State: pane.StateBlockedStr,
			Ask: &proto.Ask{
				ID:       p.ID,
				Tool:     p.Permission,
				Summary:  strings.Join(p.Patterns, ", "),
				Deadline: now() + 90,
			},
		}, true
	case "message.part.updated":
		var p textPartProps
		if json.Unmarshal(ev.Properties, &p) != nil || p.Part.Type != "text" {
			return pane.Event{}, false
		}
		return pane.Event{Kind: pane.EvText, Text: p.Part.Text}, true
	default:
		return pane.Event{}, false
	}
}

// streamEvents reads Server-Sent Events from r line by line, translating
// and forwarding each one scoped to ourSessionID until r is exhausted. A
// line that fails to parse is skipped (parseSSELine's ok=false); a failure
// reading the stream itself sends one final EvError event so the pane goes
// unknown, never a silently-stuck idle (§3.6). Kind EvError carries no
// State — the caller (Task 4) has already sent a StateUnknownStr event on
// the same failure path when one is warranted; this is the raw stream error
// for the transcript/log, not a second state signal.
func streamEvents(r io.Reader, ourSessionID string, now func() float64, out chan<- pane.Event) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		ev, ok := parseSSELine(scanner.Text())
		if !ok {
			continue
		}
		if pe, ok := translateEvent(ev, ourSessionID, now); ok {
			out <- pe
		}
	}
	if err := scanner.Err(); err != nil {
		out <- pane.Event{Kind: pane.EvError, Detail: err.Error()}
	}
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd harness/coppice && go test ./internal/adapters/opencode/... -run 'TestParseSSELine|TestTranslateEvent|TestStreamEvents' -v`
Expected: PASS (9 tests).

- [ ] **Step 6: Vet and commit**

Run: `cd harness/coppice && go vet ./internal/adapters/opencode/...`

```bash
git add harness/coppice/internal/adapters/opencode/events.go harness/coppice/internal/adapters/opencode/events_test.go harness/coppice/internal/adapters/opencode/testdata/session_lifecycle.sse
git commit -m "opencode adapter: translate the /event SSE bus into pane state, plan-02's real contract"
```

---

### Task 4: Go — the full `pane.Adapter`

**BLOCKER 2, continued.** `pane.Proc` is a 6-method interface (`Prompt`/`Steer`/`WriteStdin`/
`Events`/`SessionID`/`Stop`), not a struct — so `Start` returns a type that implements it
directly (a `*proc`, mirroring the shape plan-02's own claude/codex/sprig adapters use), never a
`pane.Proc{}` zero value on error. `Adapter.Start` takes `(ctx, pane.StartOpts, *pane.Grid)`;
this adapter, like plan-02's other three, ignores the grid (plan-02's own corrections block:
"`Start`'s `g *Grid` is dead: claude, codex and sprig all ignore it and the server renders the
transcript in `pumpAdapter`"). `StartOpts` fields are `Cwd`/`Env`/`Argv`/`Resume`/`Sock`/
`PaneID`. This task also flips the ALREADY-EXISTING `internal/adapters/opencode/opencode.go`
(plan-02 pre-registers this slot as `NotBuilt`) from the stub to the real adapter — it does not
create a second `New()`.

**NOTE applied (opts.Env ordering):** every adapter must call `pane.BuildEnv(os.Environ(), env,
o.Sock, o.PaneID)` (plan-02's own rule, so `COPPICE_SOCK`/`COPPICE_PANE` reach the process — the
one path the plugin's `reportState`, Task 6, has to find them by). `BuildEnv`'s `extra` argument
overrides `base` but there is no later layer to override `extra` itself, so this task never
passes `o.Env` straight through: it copies `o.Env`, forces `OPENCODE_SERVER_PASSWORD` to this
pane's own generated token in the copy, and passes THAT as `extra` — a pane's env can carry
anything else, but never win the auth token race, regardless of insertion order inside
`BuildEnv`.

**Files:**
- Modify: `harness/coppice/internal/adapters/opencode/opencode.go` (flip `New()`'s body)
- Create: `harness/coppice/internal/adapters/opencode/adapter.go`
- Test: `harness/coppice/internal/adapters/opencode/adapter_test.go`

**Interfaces:**
- Consumes: `newClient`, `(*client).waitReady/createSession/sessionExists/postMessage/do`
  (Task 2); `streamEvents` (Task 3); `pane.Adapter`, `pane.StartOpts`, `pane.Proc`, `pane.Event`,
  `pane.BuildEnv`, `pane.ErrUnsupported` (plan-02, landed).
- Produces: `type Adapter struct{}` implementing `pane.Adapter`; `func (Adapter) Name() string`;
  `func (Adapter) Start(ctx, pane.StartOpts, *pane.Grid) (pane.Proc, error)` — this is the value
  `opencode.go`'s `New()` now returns, the adapter registry's `"opencode"` entry.

- [ ] **Step 1: Write the failing tests**

```go
// harness/coppice/internal/adapters/opencode/adapter_test.go
package opencode

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/pane"
)

var _ pane.Adapter = Adapter{}

func TestAdapter_Name(t *testing.T) {
	if (Adapter{}).Name() != "opencode" {
		t.Fatal("Name() must return \"opencode\" — it is the adapter registry key")
	}
}

func TestBuildServeArgs_AppendsExtraArgvVerbatim(t *testing.T) {
	got := buildServeArgs(4096, []string{"--mdns"})
	want := []string{"serve", "--port", "4096", "--hostname", "127.0.0.1", "--mdns"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestOpencodeEnv_TokenAlwaysWinsOverPaneEnv(t *testing.T) {
	env := opencodeEnv(map[string]string{"OPENCODE_SERVER_PASSWORD": "attacker-value", "OTHER": "x"}, "real-token")
	if env["OPENCODE_SERVER_PASSWORD"] != "real-token" {
		t.Fatalf("got %v, want the adapter's own token to win", env)
	}
	if env["OTHER"] != "x" {
		t.Fatalf("got %v, want unrelated pane env preserved", env)
	}
}

func TestAdapter_LiveLifecycle(t *testing.T) {
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skip("opencode not on PATH — install with `npm install -g opencode-ai` to run this test")
	}
	a := Adapter{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p, err := a.Start(ctx, pane.StartOpts{Cwd: t.TempDir()}, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	sid, ok := p.SessionID()
	if !ok || sid == "" {
		t.Fatal("want a non-empty OpenCode session id after Start")
	}
	if err := p.Prompt("say hi"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if err := p.Steer("and this too"); err != nil {
		t.Fatalf("Steer: %v", err)
	}
	if err := p.WriteStdin([]byte("x")); err != pane.ErrUnsupported {
		t.Fatalf("WriteStdin: got %v, want pane.ErrUnsupported", err)
	}

	select {
	case ev, open := <-p.Events():
		if !open {
			t.Fatal("events channel closed before any event arrived")
		}
		t.Logf("first event: %+v", ev)
	case <-time.After(15 * time.Second):
		t.Fatal("no event within 15s of prompting")
	}

	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd harness/coppice && go test ./internal/adapters/opencode/... -run 'TestAdapter|TestBuildServeArgs|TestOpencodeEnv' -v`
Expected: FAIL — `adapter.go` (`Adapter`, `buildServeArgs`, `opencodeEnv`) does not exist yet.

- [ ] **Step 3: Write the adapter**

```go
// harness/coppice/internal/adapters/opencode/adapter.go
package opencode

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/opendaisugi/coppice/internal/pane"
)

// Adapter drives `opencode serve` as a headless coppice pane (spec-05). It
// carries no state of its own — every running instance's state lives on the
// *proc value Start returns, exactly the shape pane.Proc as an interface
// implies (plan-02's landed contract, not a struct{ID string} this plan
// used to assume).
type Adapter struct{}

func (Adapter) Name() string { return "opencode" }

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("opencode adapter: pick a free port: %w", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("opencode adapter: generate random bytes: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// buildServeArgs is the pure part of Start's argv construction — a plain
// function so it is testable without spawning anything.
func buildServeArgs(port int, extraArgv []string) []string {
	args := []string{"serve", "--port", strconv.Itoa(port), "--hostname", "127.0.0.1"}
	return append(args, extraArgv...)
}

// opencodeEnv copies opts.Env and forces OPENCODE_SERVER_PASSWORD to this
// pane's own generated token. pane.BuildEnv's `extra` argument overrides
// `base` with no later layer to override `extra` itself — passing opts.Env
// straight through as `extra` would let a pane's own env win the auth race
// against the token this adapter just generated for it (a pane whose env
// carries OPENCODE_SERVER_PASSWORD would 401 every one of this adapter's
// own calls). Forcing it here, once, in the map BuildEnv treats as `extra`,
// is the only place the override can be guaranteed regardless of insertion
// order inside BuildEnv.
func opencodeEnv(optsEnv map[string]string, token string) map[string]string {
	env := make(map[string]string, len(optsEnv)+1)
	for k, v := range optsEnv {
		env[k] = v
	}
	env["OPENCODE_SERVER_PASSWORD"] = token
	return env
}

// proc is one running `opencode serve` process plus the OpenCode session
// this adapter created (or resumed) on it. It implements pane.Proc.
type proc struct {
	client    *client
	sessionID string
	events    chan pane.Event
	cmd       *exec.Cmd
}

// Start spawns `opencode serve` bound to a fresh port and a per-pane random
// basic-auth token, waits for it to answer, and creates (or resumes) one
// session. g (the pane's grid) is accepted and ignored, matching plan-02's
// claude/codex/sprig adapters — the server renders the transcript from
// Events(), not from an adapter writing into the grid directly. The
// process's lifetime is bound to ctx (exec.CommandContext): canceling ctx
// tears down the subprocess.
func (Adapter) Start(ctx context.Context, o pane.StartOpts, g *pane.Grid) (pane.Proc, error) {
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	token, err := randomHex(16)
	if err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(ctx, "opencode", buildServeArgs(port, o.Argv)...)
	cmd.Dir = o.Cwd
	cmd.Env = pane.BuildEnv(os.Environ(), opencodeEnv(o.Env, token), o.Sock, o.PaneID)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("opencode adapter: start `opencode serve`: %w", err)
	}

	c := newClient(fmt.Sprintf("http://127.0.0.1:%d", port), "opencode", token)
	readyCtx, readyCancel := context.WithTimeout(ctx, 10*time.Second)
	defer readyCancel()
	if err := c.waitReady(readyCtx); err != nil {
		cmd.Process.Kill()
		return nil, err
	}

	sessionID := ""
	if o.Resume != "" && c.sessionExists(ctx, o.Resume) {
		sessionID = o.Resume
	} else {
		sessionID, err = c.createSession(ctx, "coppice pane")
		if err != nil {
			cmd.Process.Kill()
			return nil, err
		}
	}

	p := &proc{client: c, sessionID: sessionID, events: make(chan pane.Event, 64), cmd: cmd}

	// Two goroutines, one channel owner. streamPaneEvents forwards
	// translated SSE events but never closes p.events — an SSE stream can
	// end (server restart, a network blip) before the process actually
	// exits, and §3.1 reserves "done" for process exit or an explicit
	// end-of-session event, never a manifest-style inference.
	// watchProcessExit is the one place that closes the channel, and only
	// after sending an EvEnd event ("Stop() signals and kills; the
	// producing goroutine closes Events()" — plan-02's own rule).
	go p.streamPaneEvents(ctx)
	go p.watchProcessExit()

	return p, nil
}

func (p *proc) streamPaneEvents(ctx context.Context) {
	resp, err := p.client.do(ctx, "GET", "/event", nil)
	if err != nil {
		p.events <- pane.Event{Kind: pane.EvError, Detail: err.Error()}
		return
	}
	defer resp.Body.Close()
	streamEvents(resp.Body, p.sessionID, func() float64 { return float64(time.Now().Unix()) }, p.events)
}

func (p *proc) watchProcessExit() {
	_ = p.cmd.Wait()
	p.events <- pane.Event{Kind: pane.EvEnd, State: pane.StateDoneStr, Detail: "opencode serve exited"}
	close(p.events)
}

func (p *proc) Prompt(text string) error {
	return p.client.postMessage(context.Background(), p.sessionID, text)
}

// Steer sends a follow-up message. OpenCode has no separate interrupt-and-
// redirect channel in this plan's scope (PINS.md does not pin one); a
// second message queues behind the first, which is a real but bounded
// behavior gap — noted, not solved, here. Mirrors codex's own adapter
// (plan-02): Steer(text) = Prompt(text).
func (p *proc) Steer(text string) error {
	return p.Prompt(text)
}

// WriteStdin has no OpenCode equivalent — the harness is driven over HTTP,
// not a process stdin pipe. pane.ErrUnsupported is the documented "honest
// no" for exactly this case (internal/pane/adapter.go's own doc comment).
func (p *proc) WriteStdin([]byte) error {
	return pane.ErrUnsupported
}

func (p *proc) Events() <-chan pane.Event { return p.events }

func (p *proc) SessionID() (string, bool) {
	return p.sessionID, p.sessionID != ""
}

// Stop signals and kills; watchProcessExit (already running) is the
// goroutine that closes Events() once the kill actually lands — plan-02's
// rule for every adapter's Stop.
func (p *proc) Stop() error {
	if p.cmd.Process != nil {
		return p.cmd.Process.Kill()
	}
	return nil
}

var _ = sync.Mutex{} // proc needs no mutex: every field is written once at
// construction (Start) and never mutated afterward; SessionID/Events/
// Prompt/Steer/WriteStdin/Stop all only ever READ p's fields.
```

- [ ] **Step 4: Flip the existing adapter slot from `NotBuilt` to the real adapter**

`harness/coppice/internal/adapters/opencode/opencode.go` already exists (plan-02 pre-registers
this slot). Modify its `New()` function — do not add a second `New()`:

```go
// harness/coppice/internal/adapters/opencode/opencode.go

// Package opencode is the OpenCode headless adapter slot, implemented
// against `opencode serve` (client.go, events.go) plus the plugin whose
// before-execute hook is deny-only by design (harness_opencode/plugin).
package opencode

import (
	"github.com/opendaisugi/coppice/internal/adapters"
	"github.com/opendaisugi/coppice/internal/pane"
)

func New() pane.Adapter {
	return Adapter{}
}

func init() { adapters.Register(New()) }
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd harness/coppice && go test ./internal/adapters/opencode/... -v && go test ./internal/adapters/... -v`
Expected: PASS. `TestAdapter_LiveLifecycle` either passes (with `opencode` installed) or SKIPs
with the printed reason — never fails for its absence. The second command's
`internal/adapters` registry test (plan-02) now sees a real `"opencode"` adapter, not a
`NotBuilt` slot.

- [ ] **Step 6: Vet and commit**

Run: `cd harness/coppice && go vet ./... && go build ./...`

```bash
git add harness/coppice/internal/adapters/opencode/opencode.go harness/coppice/internal/adapters/opencode/adapter.go harness/coppice/internal/adapters/opencode/adapter_test.go
git commit -m "opencode adapter: wire the HTTP client and event stream into plan-02's pane.Proc"
```

---

### Task 5: Python — the `filePath` fallback (narrowed by BLOCKER 1)

**Depends on: `plan-04-pi.md` Task 3.** That task creates `opendaisugi.hook.EXIT_CODE_FORMATS`
and rewrites `gate.py`'s `_outcome()` to branch on membership in it — both this plan and
plan-04 independently proposed the identical fix for the identical fail-open (`_outcome()` gave
every non-"claude" format `exit_code 0` on every verdict, and `stdout_for_format` has no
`opencode` case either, so a deny returned `{"continue": true}` *and* `exit_code 0` — it failed
open on both channels). The review's ruling: it lands once, in plan-04, as
`EXIT_CODE_FORMATS = frozenset({"claude", "pi", "opencode"})` (or, if this plan's own tests run
before plan-04's has landed, plan-04's Task 3 is written to widen whichever frozenset it finds
already declared — either order converges on the same three-element set). This task therefore
touches `_outcome()` nowhere. What remains is real and independent of that dependency: `hook.py`'s
path extraction genuinely does not recognize `filePath`, and that fix stands on its own.

**Files:**
- Modify: `src/opendaisugi/hook.py:186` (add the `filePath` fallback — the only production
  code change left in this task)
- Test: `tests/test_hook_format_opencode.py`

**Interfaces:**
- Consumes: `opendaisugi.hook.EXIT_CODE_FORMATS` (produced by `plan-04-pi.md` Task 3 — imported
  by this task's tests, not declared by it), `opendaisugi.gate.gate_and_contract`,
  `opendaisugi.gate.register_envelope`, `opendaisugi.hook._payload_to_record` (existing).
- Produces: nothing new — `_payload_to_record`'s widened path extraction is the only change,
  consumed by Task 6's plugin payloads and by whatever already calls `_payload_to_record`.

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_hook_format_opencode.py
"""filePath extraction for OpenCode's file tools (spec-05 Task 5, narrowed).

The exit-code host contract for --format opencode is plan-04-pi.md Task 3's
EXIT_CODE_FORMATS, not this file's — see this task's header for why. These
tests exercise that dependency (asserting membership and the resulting
end-to-end behaviour through gate_and_contract) without re-declaring it.
"""

from __future__ import annotations

import json

from opendaisugi.gate import gate_and_contract, register_envelope
from opendaisugi.hook import _payload_to_record
from opendaisugi.models import Envelope, Permission


def _envelope(**perm_kwargs) -> Envelope:
    perms = {"file_read": ["/allowed/**"], **perm_kwargs}
    return Envelope(generated_by="test", task="opencode gate test", permissions=Permission(**perms))


def test_filepath_key_is_extracted_for_opencode_read_tool():
    record = _payload_to_record(
        {"tool_name": "Read", "tool_input": {"filePath": "/tmp/proj/a.txt"}, "session_id": "s1"}
    )
    assert record is not None
    assert record["path"] == "/tmp/proj/a.txt"


def test_filepath_fallback_does_not_shadow_the_existing_path_key():
    record = _payload_to_record(
        {"tool_name": "Read", "tool_input": {"path": "/existing/x.txt"}, "session_id": "s1"}
    )
    assert record is not None
    assert record["path"] == "/existing/x.txt"


def test_opencode_is_in_exit_code_formats():
    """Cross-plan dependency check, not a design choice this task makes: if
    this fails, plan-04-pi.md Task 3 has not landed (or landed with a
    narrower set than the review's ruling) — run that task first."""
    from opendaisugi.hook import EXIT_CODE_FORMATS

    assert "opencode" in EXIT_CODE_FORMATS, (
        "EXIT_CODE_FORMATS is built by plan-04-pi.md Task 3, not here — "
        "run that task before this one's end-to-end tests can pass"
    )


def test_opencode_format_denies_via_exit_code_end_to_end(tmp_path):
    """Same cross-plan dependency, exercised through the real pipeline: a
    deny must reach the plugin as exit_code 2, not exit_code 0 with an
    unread JSON body — the fail-open this whole design exists to close."""
    register_envelope(_envelope(shell=False), root=tmp_path)
    payload = json.dumps(
        {"tool_name": "Bash", "tool_input": {"command": "rm -rf /"}, "session_id": "s1"}
    ).encode()
    out = gate_and_contract(payload, root=tmp_path, fmt="opencode", mode="enforce")
    assert out.exit_code == 2
    assert "DENIED" in out.stderr


def test_opencode_format_allows_via_exit_code_zero(tmp_path):
    register_envelope(_envelope(), root=tmp_path)
    payload = json.dumps(
        {"tool_name": "Read", "tool_input": {"filePath": "/allowed/a.txt"}, "session_id": "s1"}
    ).encode()
    out = gate_and_contract(payload, root=tmp_path, fmt="opencode", mode="enforce")
    assert out.exit_code == 0
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `uv run --no-sync pytest tests/test_hook_format_opencode.py -q`
Expected: the two `filepath` tests FAIL (the fallback doesn't exist yet); the three
`EXIT_CODE_FORMATS`/`gate_and_contract` tests fail or pass depending on whether
`plan-04-pi.md` Task 3 has already run in this checkout — either way, this task changes nothing
about that outcome, and its own commit does not depend on it (Step 6 runs `filepath`-only).

- [ ] **Step 3: Add the `filePath` fallback to `hook.py`**

In `src/opendaisugi/hook.py`, widen the path extraction in `_payload_to_record` (around line
186):

```python
    elif step_type in ("file_read", "file_write"):
        record["path"] = (
            inp.get("file_path") or inp.get("path") or inp.get("pattern") or inp.get("filePath") or ""
        )
```

- [ ] **Step 4: Run the filePath tests to verify they pass**

Run: `uv run --no-sync pytest tests/test_hook_format_opencode.py -k filepath -q`
Expected: PASS (2 tests) — independent of whether plan-04 Task 3 has landed.

- [ ] **Step 5: Run the full existing hook suite to confirm no regression**

Run: `uv run --no-sync pytest tests/test_hook.py -q`
Expected: PASS — a purely additive `or` clause changes no existing format's behavior.

- [ ] **Step 6: Lint and commit**

Run: `uv run --no-sync ruff check src/opendaisugi/hook.py tests/test_hook_format_opencode.py`

```bash
git add src/opendaisugi/hook.py tests/test_hook_format_opencode.py
git commit -m "hook: recognize OpenCode's filePath key so its file tools resolve a real path"
```

- [ ] **Step 7 (run once plan-04-pi.md Task 3 has landed, in either order): confirm the cross-plan tests pass**

Run: `uv run --no-sync pytest tests/test_hook_format_opencode.py -q`
Expected: 5 passed. If this still fails after plan-04 Task 3 has landed, its `EXIT_CODE_FORMATS`
literal does not include `"opencode"` — that is plan-04's regression, not this task's; widen the
literal there, per its own Step 6/8 reconciliation note, rather than re-adding an `_outcome()`
branch here.

---

### Task 6: TypeScript — the openDaisugi gate plugin

**BLOCKER 3, 4, 5 and the SHOULD-FIX items on `--root`/`"v":1`/`--verify-timeout` and the
unmapped-tool-id asymmetry all apply in this task.**

- BLOCKER 3: `askGate` must not trust any socket at the expected path — a planted socket
  answering `{"exit_code":0}` would otherwise be a universal allow. `gateSocketTrustworthy`
  (below) mirrors `gate_client.py:33-50`'s `_socket_is_trustworthy`: `fs.lstatSync` (never a
  symlink-following `existsSync`), refuse anything that is not a socket, not owned by
  `process.getuid()`, or not mode `0600`.
- BLOCKER 4: `GATE_MODE`/`--mode` are gone entirely. `resolve_gate_mode(None, root)`
  (`gate.py:51-70`) reads `config.yaml` server-side when `--mode` is omitted, the same trust
  level as a config-file read without a higher-priority file the agent can rewrite to override
  an installed `enforce`. A fresh install resolves to **shadow** (`resolve_gate_mode`'s own
  documented fallback), so the honest line says that, not "or every tool call will be denied".
- BLOCKER 5: `reportState` sends `{"argv":["hook","report","--pane",P],...}` to the **gate**
  socket (`gateSockPath()`), never to `COPPICE_SOCK` directly — `hook_report_argv` (plan-01
  ≈775-813) is the thing that calls `opendaisugi._state_report.report_state()` and does the
  actual coppice/Herdr fan-out, from ITS OWN process environment, not this plugin's payload.
- SHOULD-FIX: `askGate`'s argv gains `"--root", path.dirname(sockPath)` (so envelopes/shadow
  logs come from the same directory the socket answered on — `gate_client.py`'s own
  `_root_from_argv` derives it the same way) and `"--verify-timeout", "4"` (under the gate's
  10 s default inner budget but inside this plugin's 5 s client timeout, mirroring
  `gate_settings_json`'s own `inner = min(verify_timeout_s, max(1.0, hook_timeout_s - 5.0))`
  discipline); the request gains `"v": 1` (byte-identical in shape to `gate_client.py`'s own
  request and to plan-04's extension).
- SHOULD-FIX (unmapped tool ids): documented in this file's own comment, not translated to
  `mcp__opencode__<id>` — OpenCode's non-built-in tools are denied unconditionally as
  "unrecognized tool" in this version, fail-closed. Widening that is future work, not silently
  assumed.
- NOTE applied: `permission.ask`'s input is typed `any`, not `Permission` — Task 1's PINS.md now
  carries the verified `Hooks` key list and the `Permission` shape from the installed package,
  but the hook is confirmed dead code upstream (issues #9229/#7006), so this plugin does not
  import a type for a parameter nothing currently supplies real values through.

**Files:**
- Create: `src/opendaisugi/harness_opencode/plugin/daisugi-gate.ts`
- Test: `tests/harness_opencode/test_plugin_block_logic.test.ts`
- Modify: `pyproject.toml` (`artifacts` list, so the new file ships in a built wheel)

**Interfaces:**
- Produces: `gateSocketTrustworthy(p: string) -> boolean`; `askGate(payload, opts?) ->
  Promise<{allow: boolean; reason: string}>`; `reportState(state, detail?, sessionId?) -> void`;
  `DaisugiGate: Plugin` (default export). No `configMode`/`GATE_MODE` — Task 7 no longer
  templates anything into this file, so it is install-mode-independent.
- Consumes: the wire shape `gate_server.py`'s `_Handler.handle()` reads (confirmed at
  `src/opendaisugi/gate_server.py:21-35`) and the `["hook","report",...]` gate-socket dispatch
  (plan-01 ≈775-813).

- [ ] **Step 1: Write the failing tests**

```ts
// tests/harness_opencode/test_plugin_block_logic.test.ts
import { test } from "node:test";
import assert from "node:assert";
import * as fs from "node:fs";
import * as net from "node:net";
import * as os from "node:os";
import * as path from "node:path";
import {
  DaisugiGate,
  askGate,
  gateSocketTrustworthy,
} from "../../src/opendaisugi/harness_opencode/plugin/daisugi-gate.ts";

type FakeReply = { stdout?: string; stderr?: string; exit_code: number };

function startFakeGate(reply: FakeReply): { sockPath: string; close: () => void; requests: any[] } {
  const sockPath = path.join(os.tmpdir(), `fakegate-${process.pid}-${Math.random().toString(16).slice(2)}.sock`);
  const requests: any[] = [];
  const server = net.createServer((conn) => {
    let buf = "";
    conn.on("data", (d) => {
      buf += d.toString();
      if (buf.includes("\n")) {
        requests.push(JSON.parse(buf));
        conn.end(
          JSON.stringify({ v: 1, stdout: reply.stdout ?? "", stderr: reply.stderr ?? "", exit_code: reply.exit_code }) +
            "\n",
        );
      }
    });
  });
  server.listen(sockPath);
  fs.chmodSync(sockPath, 0o600);
  return {
    sockPath,
    close: () => {
      server.close();
      fs.rmSync(sockPath, { force: true });
    },
    requests,
  };
}

test("gateSocketTrustworthy: rejects a socket that is not mode 0600", () => {
  const fake = startFakeGate({ exit_code: 0 });
  try {
    fs.chmodSync(fake.sockPath, 0o644);
    assert.strictEqual(gateSocketTrustworthy(fake.sockPath), false);
  } finally {
    fake.close();
  }
});

test("gateSocketTrustworthy: rejects a path that is not a socket at all", () => {
  const filePath = path.join(os.tmpdir(), `not-a-socket-${process.pid}.txt`);
  fs.writeFileSync(filePath, "x", { mode: 0o600 });
  try {
    assert.strictEqual(gateSocketTrustworthy(filePath), false);
  } finally {
    fs.rmSync(filePath, { force: true });
  }
});

test("gateSocketTrustworthy: accepts a private socket we own", () => {
  const fake = startFakeGate({ exit_code: 0 });
  try {
    assert.strictEqual(gateSocketTrustworthy(fake.sockPath), true);
  } finally {
    fake.close();
  }
});

test("askGate: a planted socket that is not private and ours denies, even though it answers exit_code 0", async () => {
  const fake = startFakeGate({ exit_code: 0 });
  try {
    fs.chmodSync(fake.sockPath, 0o666); // world-writable: a planted socket, per the blocker
    const v = await askGate({ tool_name: "Bash" }, { sockPath: fake.sockPath });
    assert.strictEqual(v.allow, false);
    assert.match(v.reason, /unreachable/);
    assert.strictEqual(fake.requests.length, 0, "must never even connect to an untrusted socket");
  } finally {
    fake.close();
  }
});

test("askGate: exit_code 0 on a trusted socket allows", async () => {
  const fake = startFakeGate({ exit_code: 0 });
  try {
    const v = await askGate({ tool_name: "Read" }, { sockPath: fake.sockPath });
    assert.deepStrictEqual(v, { allow: true, reason: "" });
  } finally {
    fake.close();
  }
});

test("askGate: nonzero exit denies with stderr as the reason", async () => {
  const fake = startFakeGate({ exit_code: 2, stderr: "openDaisugi gate: DENIED — no envelope" });
  try {
    const v = await askGate({ tool_name: "Bash" }, { sockPath: fake.sockPath });
    assert.strictEqual(v.allow, false);
    assert.strictEqual(v.reason, "openDaisugi gate: DENIED — no envelope");
  } finally {
    fake.close();
  }
});

test("askGate: unreachable socket denies with the start hint", async () => {
  const v = await askGate({ tool_name: "Bash" }, { sockPath: "/nonexistent/gate.sock", timeoutMs: 500 });
  assert.strictEqual(v.allow, false);
  assert.match(v.reason, /run daisugi start/);
});

test("askGate: the request never carries --mode, and does carry v/root/verify-timeout", async () => {
  const fake = startFakeGate({ exit_code: 0 });
  try {
    await askGate({ tool_name: "Read" }, { sockPath: fake.sockPath });
    assert.strictEqual(fake.requests.length, 1);
    const req = fake.requests[0];
    assert.strictEqual(req.v, 1);
    assert.ok(!req.argv.includes("--mode"), "the gate must resolve mode server-side, not from the plugin");
    assert.deepStrictEqual(req.argv, [
      "--format",
      "opencode",
      "--root",
      path.dirname(fake.sockPath),
      "--verify-timeout",
      "4",
    ]);
  } finally {
    fake.close();
  }
});

test("tool.execute.before: allow does not throw", async () => {
  const fake = startFakeGate({ exit_code: 0 });
  process.env.OPENDAISUGI_GATE_SOCK = fake.sockPath;
  try {
    const hooks = await DaisugiGate({ directory: "/tmp/proj" } as any, undefined as any);
    await hooks["tool.execute.before"]!(
      { tool: "read", sessionID: "ses_1", callID: "call_1" },
      { args: { filePath: "/tmp/proj/a.txt" } },
    );
  } finally {
    delete process.env.OPENDAISUGI_GATE_SOCK;
    fake.close();
  }
});

test("tool.execute.before: deny throws with the gate's own reason", async () => {
  const fake = startFakeGate({ exit_code: 2, stderr: "openDaisugi gate: DENIED — shell head 'rm' not in allowlist" });
  process.env.OPENDAISUGI_GATE_SOCK = fake.sockPath;
  try {
    const hooks = await DaisugiGate({ directory: "/tmp/proj" } as any, undefined as any);
    await assert.rejects(
      hooks["tool.execute.before"]!({ tool: "bash", sessionID: "ses_1", callID: "call_2" }, { args: { command: "rm -rf /" } }),
      /DENIED — shell head 'rm' not in allowlist/,
    );
  } finally {
    delete process.env.OPENDAISUGI_GATE_SOCK;
    fake.close();
  }
});

test("tool.execute.before: unreachable gate throws with the start hint", async () => {
  process.env.OPENDAISUGI_GATE_SOCK = "/nonexistent/gate.sock";
  try {
    const hooks = await DaisugiGate({ directory: "/tmp/proj" } as any, undefined as any);
    await assert.rejects(
      hooks["tool.execute.before"]!({ tool: "read", sessionID: "ses_1", callID: "call_3" }, { args: {} }),
      /run daisugi start/,
    );
  } finally {
    delete process.env.OPENDAISUGI_GATE_SOCK;
  }
});

test("permission.ask: never writes output.status — the plugin does not decide", async () => {
  delete process.env.COPPICE_SOCK;
  delete process.env.COPPICE_PANE;
  const hooks = await DaisugiGate({ directory: "/tmp/proj" } as any, undefined as any);
  const output: { status?: string } = {};
  await hooks["permission.ask"]!(
    { id: "per_1", type: "bash", sessionID: "ses_1", messageID: "msg_1", title: "run rm -rf /" } as any,
    output as any,
  );
  assert.strictEqual(output.status, undefined);
});

test("reportState (via permission.ask): sends the hook-report shape to the GATE socket, not COPPICE_SOCK", async () => {
  const fake = startFakeGate({ exit_code: 0 });
  process.env.OPENDAISUGI_GATE_SOCK = fake.sockPath;
  process.env.COPPICE_SOCK = "/should/never/be/dialed.sock";
  process.env.COPPICE_PANE = "w1:p1";
  try {
    const hooks = await DaisugiGate({ directory: "/tmp/proj" } as any, undefined as any);
    await hooks["permission.ask"]!(
      { id: "per_1", type: "bash", sessionID: "ses_1", messageID: "msg_1", title: "run rm -rf /" } as any,
      {} as any,
    );
    await new Promise((r) => setTimeout(r, 100));
    const reportReq = fake.requests.find((r) => Array.isArray(r.argv) && r.argv[0] === "hook");
    assert.ok(reportReq, "no hook-report request reached the fake GATE socket");
    assert.deepStrictEqual(reportReq.argv, ["hook", "report", "--pane", "w1:p1"]);
    const payload = JSON.parse(Buffer.from(reportReq.stdin_b64, "base64").toString());
    assert.strictEqual(payload.state, "blocked");
    assert.strictEqual(payload.session_id, "ses_1");
    assert.strictEqual(payload.harness, "opencode");
  } finally {
    delete process.env.OPENDAISUGI_GATE_SOCK;
    delete process.env.COPPICE_SOCK;
    delete process.env.COPPICE_PANE;
    fake.close();
  }
});

test("event: session.idle and permission.asked report to the GATE socket and never throw", async () => {
  const fake = startFakeGate({ exit_code: 0 });
  process.env.OPENDAISUGI_GATE_SOCK = fake.sockPath;
  process.env.COPPICE_PANE = "w1:p1";
  try {
    const hooks = await DaisugiGate({ directory: "/tmp/proj" } as any, undefined as any);
    await hooks.event!({ event: { type: "session.idle", properties: { sessionID: "ses_1" } } } as any);
    await hooks.event!({
      event: { type: "permission.asked", properties: { id: "per_1", sessionID: "ses_1", permission: "bash", patterns: ["rm -rf *"] } },
    } as any);
    await new Promise((r) => setTimeout(r, 100));
    const reportReqs = fake.requests.filter((r) => Array.isArray(r.argv) && r.argv[0] === "hook");
    assert.strictEqual(reportReqs.length, 2);
  } finally {
    delete process.env.OPENDAISUGI_GATE_SOCK;
    delete process.env.COPPICE_PANE;
    fake.close();
  }
});
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `node --test tests/harness_opencode/test_plugin_block_logic.test.ts`
Expected: FAIL — `src/opendaisugi/harness_opencode/plugin/daisugi-gate.ts` does not exist yet.

- [ ] **Step 3: Write the plugin**

```ts
// src/opendaisugi/harness_opencode/plugin/daisugi-gate.ts
//
// openDaisugi's fail-closed gate for OpenCode (spec-05). Every tool call
// OpenCode is about to run passes through tool.execute.before, which can
// only deny (throw) — a deny-only hook can never accidentally grant
// anything. There is no --mode flag anywhere in this file: the gate
// resolves shadow/enforce itself, server-side, from config.yaml
// (gate.py's resolve_gate_mode) — a file this plugin cannot rewrite, unlike
// a constant baked into this one at install time.
import type { Plugin } from "@opencode-ai/plugin";
import * as fs from "node:fs";
import * as net from "node:net";
import * as os from "node:os";
import * as path from "node:path";

function gateSockPath(): string {
  return process.env.OPENDAISUGI_GATE_SOCK || path.join(os.homedir(), ".opendaisugi", "gate", "gate.sock");
}

// Mirrors gate_client.py's _socket_is_trustworthy (gate_client.py:33-50)
// byte for byte in intent: a private (0600, owned by us, a REAL socket
// entry — never a symlink target we didn't verify) file at p. lstat, never
// a symlink-following exists()/stat() — a rogue process could otherwise
// plant a symlink to a socket it controls. Any answer this socket gives,
// including exit_code 0, is worthless until this returns true.
export function gateSocketTrustworthy(p: string): boolean {
  try {
    const st = fs.lstatSync(p);
    return st.isSocket() && st.uid === process.getuid!() && (st.mode & 0o777) === 0o600;
  } catch {
    return false;
  }
}

// OpenCode's built-in tool ids (PINS.md, GET /experimental/tool/ids,
// live-confirmed on opencode-ai 1.18.29) translated to the names the gate's
// classifier already knows (hook.py's _TOOL_TYPE_MAP) — the same
// client-side translation sprig's DaisugiGate does for its own lowercase
// tool names (harness/sprig/daisugi_gate.go's gateVocabulary). An id with
// no entry here (question/task/todowrite/skill/apply_patch/invalid — see
// PINS.md) passes through unchanged and is denied as "unrecognized tool":
// fail-closed, but it means OpenCode's non-built-in tools cannot be
// admitted by an envelope's mcp_allowlist in this version. That asymmetry
// with pi's adapter (which maps an unknown id to an admissible mcp__ name)
// is a real, documented gap, not an oversight — widening it is future work.
const TOOL_NAME_MAP: Record<string, string> = {
  bash: "Bash",
  read: "Read",
  write: "Write",
  edit: "Edit",
  glob: "Glob",
  grep: "Grep",
  webfetch: "WebFetch",
  websearch: "WebSearch",
};

export type GateVerdict = { allow: boolean; reason: string };

// One request/response round trip to the resident gate over its unix
// socket, in the wire shape gate_server.py reads (gate_server.py:21-35):
// {"argv": [...], "stdin_b64": <payload>} -> {"stdout","stderr","exit_code"}.
// argv is bare gate flags read by run_argv's parser (gate.py:1012-1045) —
// no --mode (the gate resolves it itself), --root pinned to the socket's
// own directory (gate_client.py's _root_from_argv derives it the same
// way), --verify-timeout under this function's own client timeout. exit_code
// 0 is the ONLY allow; anything else (a real deny, a socket error, a
// timeout, a malformed reply, or an UNTRUSTED socket this function refuses
// to even connect to) is a deny with a reason that teaches the next
// command — there is no path from a broken or planted gate to an allow.
export function askGate(
  payload: Record<string, unknown>,
  opts: { sockPath?: string; timeoutMs?: number } = {},
): Promise<GateVerdict> {
  const sockPath = opts.sockPath ?? gateSockPath();
  const timeoutMs = opts.timeoutMs ?? 5000;
  const UNREACHABLE = "openDaisugi gate unreachable: run daisugi start";
  return new Promise((resolve) => {
    if (!gateSocketTrustworthy(sockPath)) {
      resolve({ allow: false, reason: UNREACHABLE });
      return;
    }
    let settled = false;
    const finish = (v: GateVerdict) => {
      if (!settled) {
        settled = true;
        resolve(v);
      }
    };
    const req =
      JSON.stringify({
        v: 1,
        argv: ["--format", "opencode", "--root", path.dirname(sockPath), "--verify-timeout", "4"],
        stdin_b64: Buffer.from(JSON.stringify(payload)).toString("base64"),
      }) + "\n";
    let sock: net.Socket;
    const timer = setTimeout(() => {
      sock?.destroy();
      finish({ allow: false, reason: UNREACHABLE });
    }, timeoutMs);
    sock = net.createConnection(sockPath);
    let buf = "";
    sock.on("connect", () => sock.write(req));
    sock.on("data", (d) => {
      buf += d.toString();
    });
    sock.on("end", () => {
      clearTimeout(timer);
      try {
        const reply = JSON.parse(buf);
        if (reply.exit_code === 0) finish({ allow: true, reason: "" });
        else finish({ allow: false, reason: reply.stderr || `openDaisugi gate: DENIED (exit ${reply.exit_code})` });
      } catch {
        finish({ allow: false, reason: UNREACHABLE });
      }
    });
    sock.on("error", () => {
      clearTimeout(timer);
      finish({ allow: false, reason: UNREACHABLE });
    });
  });
}

// Fire-and-forget state push. Reaches `daisugi hook report` over the SAME
// GATE socket askGate uses — NOT COPPICE_SOCK. hook_report_argv (plan-01)
// is the thing that validates this event, writes the session tree, and
// calls opendaisugi._state_report.report_state() to fan it out to
// coppice/Herdr from the GATE's own environment; this function's only job
// is to reach the gate with the right shape. Never blocks the agent
// (fire-and-forget, 1 s timeout) and never throws.
export function reportState(state: string, detail: Record<string, unknown> = {}, sessionId = ""): void {
  const coppicePane = process.env.COPPICE_PANE;
  if (!coppicePane) return; // no pane to report against — nothing to attach the event to
  const sockPath = gateSockPath();
  if (!gateSocketTrustworthy(sockPath)) return;
  try {
    const event = {
      v: 1,
      ts: Date.now() / 1000,
      session_id: sessionId,
      harness: "opencode",
      pane: coppicePane,
      state,
      source: "headless",
      detail: JSON.stringify(detail).slice(0, 200),
    };
    const req =
      JSON.stringify({
        v: 1,
        argv: ["hook", "report", "--pane", coppicePane],
        stdin_b64: Buffer.from(JSON.stringify(event)).toString("base64"),
      }) + "\n";
    const sock = net.createConnection(sockPath);
    const timer = setTimeout(() => sock.destroy(), 1000);
    sock.on("connect", () => sock.write(req));
    sock.on("data", () => {});
    sock.on("end", () => clearTimeout(timer));
    sock.on("error", () => clearTimeout(timer));
  } catch {
    // best-effort — a reporting failure must never disrupt the agent
  }
}

export const DaisugiGate: Plugin = async ({ directory }) => {
  return {
    "tool.execute.before": async (input, output) => {
      const verdict = await askGate({
        tool_name: TOOL_NAME_MAP[input.tool] ?? input.tool,
        tool_input: output.args,
        session_id: input.sessionID,
        cwd: directory,
        tool_use_id: input.callID,
      });
      if (!verdict.allow) {
        throw new Error(verdict.reason);
      }
    },
    // Type-declared by @opencode-ai/plugin (PINS.md dumps the real Hooks
    // keys) but known not to fire on some builds (anomalyco/opencode
    // #9229: the active permission pipeline emits a bus event instead of
    // calling Plugin.trigger()). Registered anyway (costs nothing) but
    // never touches `output.status` — the plugin observes OpenCode's own
    // permission prompts, it does not decide them. Typed `any`, not
    // `Permission`: nothing calls this today, so importing a type for it
    // buys nothing and one more upstream rename cannot break this file.
    "permission.ask": async (input: any) => {
      reportState("blocked", { id: input?.id, tool: input?.type, summary: input?.title }, input?.sessionID);
    },
    event: async ({ event }) => {
      const ev = event as { type: string; properties?: Record<string, unknown> };
      if (ev.type === "session.idle") {
        reportState("idle", {}, String(ev.properties?.sessionID ?? ""));
      }
      if (ev.type === "permission.asked") {
        const p = ev.properties as { id: string; sessionID: string; permission: string; patterns?: string[] };
        reportState("blocked", { id: p.id, tool: p.permission, summary: (p.patterns ?? []).join(", ") }, p.sessionID);
      }
    },
  };
};

export default DaisugiGate;
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `node --test tests/harness_opencode/test_plugin_block_logic.test.ts`
Expected: PASS (14 tests). Node 22's native TypeScript support strips `import type { Plugin }
from "@opencode-ai/plugin"` entirely at load time — this passes with no npm install and no
`package.json` in the plugin's directory.

- [ ] **Step 5: Add the plugin directory to the wheel's packaged artifacts**

`src/opendaisugi/install_assets/**/*` is the existing pattern (`pyproject.toml`) for shipping
non-Python files inside `src/opendaisugi/`; `harness_opencode/` is a sibling directory using
the file layout `spec-05-opencode.md` specifies, so it needs its own entry.

In `pyproject.toml`, under `[tool.hatch.build.targets.wheel]`:

```toml
artifacts = [
    "src/opendaisugi/skills/**/*.md",
    "src/opendaisugi/install_assets/**/*",
    "src/opendaisugi/harness_opencode/**/*",
    "src/opendaisugi/viz_dag_template.html",
]
```

- [ ] **Step 6: Commit**

```bash
git add src/opendaisugi/harness_opencode/plugin/daisugi-gate.ts tests/harness_opencode/test_plugin_block_logic.test.ts pyproject.toml
git commit -m "opencode plugin: trust-check the gate socket, drop the rewritable mode, fix report routing"
```

---

### Task 7: Python — `daisugi install --harness opencode`

**BLOCKER 4, continued.** No mode is baked into the plugin file — there is nothing left to
template, so `install_opencode_harness` copies the shipped file byte for byte. The honest
install line states the real default (shadow) instead of the false "or every tool call will be
denied". **SHOULD-FIX applied:** the `modules.py` row now names the enforcement class
("in-process deny-only hook; fail-closed"), and this task's docstring states as fact that
`spec-05-opencode.md`'s "adds it to the user `opencode.json` plugin list" instruction is wrong
and must not be restored.

**Files:**
- Modify: `src/opendaisugi/install.py` (new functions, added near the OpenClaw section)
- Modify: `src/opendaisugi/cli.py:3536-3577` (`install_cmd` signature), body after `home =
  Path.home()` (~line 3612)
- Modify: `src/opendaisugi/modules.py` (harness stage, ~lines 119-127)
- Test: `tests/test_install_opencode.py`
- Modify: `tests/test_modules.py` (one new test)

**Interfaces:**
- Consumes: nothing from `opendaisugi.hook`/`opendaisugi.gate` — this task only materializes
  the plugin file Task 6 wrote, unmodified.
- Produces: `install_opencode_harness(home: Path) -> Path`; `uninstall_opencode_harness(home:
  Path) -> list[Path]`; `daisugi install --harness opencode`.

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_install_opencode.py
"""daisugi install --harness opencode: materialize/remove the gate plugin (spec-05).

OpenCode auto-loads local .ts files from its plugins directory — no
opencode.json registration is needed (that key is for npm package plugins
only, per PINS.md; spec-05-opencode.md's own text saying otherwise is wrong
and must not be restored — see install_opencode_harness's docstring). The
plugin file carries no install-time mode template (BLOCKER 4): the gate
resolves shadow/enforce itself from config.yaml, so installing this harness
is a single, unconditional file copy.
"""

from __future__ import annotations

from pathlib import Path

import pytest
from typer.testing import CliRunner

from opendaisugi.cli import app
from opendaisugi.install import install_opencode_harness, uninstall_opencode_harness

runner = CliRunner()


def _plugin_path(home: Path) -> Path:
    return home / ".config" / "opencode" / "plugins" / "daisugi-gate.ts"


def _shipped_plugin_text() -> str:
    import importlib.resources as _ir

    return (
        _ir.files("opendaisugi")
        .joinpath("harness_opencode", "plugin", "daisugi-gate.ts")
        .read_text(encoding="utf-8")
    )


def test_install_copies_the_plugin_byte_for_byte(tmp_path):
    dest = install_opencode_harness(home=tmp_path)
    assert dest == _plugin_path(tmp_path)
    assert dest.read_text(encoding="utf-8") == _shipped_plugin_text()


def test_install_is_idempotent(tmp_path):
    p1 = install_opencode_harness(home=tmp_path)
    text1 = p1.read_text(encoding="utf-8")
    p2 = install_opencode_harness(home=tmp_path)
    assert p1 == p2
    assert p2.read_text(encoding="utf-8") == text1


def test_install_never_writes_through_a_preplanted_symlink(tmp_path):
    evil_target = tmp_path / "evil.txt"
    plugin_path = _plugin_path(tmp_path)
    plugin_path.parent.mkdir(parents=True)
    plugin_path.symlink_to(evil_target)
    install_opencode_harness(home=tmp_path)
    assert not evil_target.exists()
    assert not plugin_path.is_symlink()


def test_uninstall_removes_the_file(tmp_path):
    install_opencode_harness(home=tmp_path)
    removed = uninstall_opencode_harness(home=tmp_path)
    assert removed == [_plugin_path(tmp_path)]
    assert not _plugin_path(tmp_path).exists()


def test_uninstall_is_idempotent_when_never_installed(tmp_path):
    assert uninstall_opencode_harness(home=tmp_path) == []


def test_cli_install_harness_opencode_dry_run_writes_nothing(tmp_path, monkeypatch):
    monkeypatch.setattr("pathlib.Path.home", lambda: tmp_path)
    res = runner.invoke(app, ["install", "--harness", "opencode", "--dry-run"])
    assert res.exit_code == 0, res.output
    assert not _plugin_path(tmp_path).exists()


def test_cli_install_harness_opencode_installs_and_prints_the_honest_line(tmp_path, monkeypatch):
    monkeypatch.setattr("pathlib.Path.home", lambda: tmp_path)
    res = runner.invoke(app, ["install", "--harness", "opencode"])
    assert res.exit_code == 0, res.output
    assert _plugin_path(tmp_path).exists()
    assert "daisugi start" in res.output
    assert "gate_mode: enforce" in res.output
    # BLOCKER 4's false line must not come back:
    assert "every tool call will be denied" not in res.output


def test_cli_install_unknown_harness_is_a_user_error(tmp_path, monkeypatch):
    monkeypatch.setattr("pathlib.Path.home", lambda: tmp_path)
    res = runner.invoke(app, ["install", "--harness", "bogus"])
    assert res.exit_code == 1  # CLI user error (Global Constraints: 1, not the gate's 2)


def test_cli_install_harness_opencode_uninstall_removes_it(tmp_path, monkeypatch):
    monkeypatch.setattr("pathlib.Path.home", lambda: tmp_path)
    runner.invoke(app, ["install", "--harness", "opencode"])
    res = runner.invoke(app, ["install", "--harness", "opencode", "--uninstall"])
    assert res.exit_code == 0, res.output
    assert not _plugin_path(tmp_path).exists()
```

Add to `tests/test_modules.py`:

```python
def test_opencode_harness_reflects_plugin_install_and_names_its_class(tmp_path, monkeypatch):
    from opendaisugi.install import install_opencode_harness
    from opendaisugi.modules import ACTIVE, POSSIBLE, detect_stages

    monkeypatch.setattr("pathlib.Path.home", lambda: tmp_path)
    harness_stage = next(s for s in detect_stages(tmp_path) if s.key == "harness")
    before = next(m for m in harness_stage.modules if m.name == "opencode")
    assert before.state == POSSIBLE

    install_opencode_harness(home=tmp_path)
    harness_stage = next(s for s in detect_stages(tmp_path) if s.key == "harness")
    after = next(m for m in harness_stage.modules if m.name == "opencode")
    assert after.state == ACTIVE
    assert "deny-only" in after.note
    assert "fail-closed" in after.note
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `uv run --no-sync pytest tests/test_install_opencode.py -q`
Expected: FAIL — `install_opencode_harness`/`uninstall_opencode_harness` do not exist; the CLI
has no `--harness` option yet.

- [ ] **Step 3: Add `install_opencode_harness`/`uninstall_opencode_harness` to `install.py`**

Add near the end of the OpenClaw section (after `_patch_openclaw_base_url`, before the
"Uninstall helpers" comment block):

```python
# ---------------------------------------------------------------------------
# OpenCode (spec-05) — a single gate-plugin install, not the four-layer
# Runtime protocol above. OpenCode is a loop harness (master §2), not a full
# agent runtime with its own skill/MCP/instructions layers in this plan's
# scope: it gets one thing, the fail-closed gate plugin.
#
# CORRECTION (adversarial review, BLOCKER — do not restore this):
# spec-05-opencode.md's Install section says to add the plugin to the user
# opencode.json "plugin" list "if that is the documented mechanism". It is
# not: that array registers npm packages (OpenCode runs `bun install` on
# each entry at startup), so a local file path there is a broken startup,
# not a no-op. Local .ts/.js files in ~/.config/opencode/plugins/ load
# automatically — PINS.md, verified against the shipped docs. This
# function does not touch opencode.json, and it should stay that way.
#
# CORRECTION (adversarial review, BLOCKER — do not restore mode templating):
# an earlier version of this function baked --mode into the plugin file via
# a placeholder replace. gate.py's resolve_gate_mode is explicit that
# config.yaml must never be overridable by anything OTHER than the
# installed hook's own --mode flag, and a plain-text file in a directory
# the agent can write is exactly such an override — one sed on that
# constant would have turned every deny into an allow. The plugin now
# carries no mode at all; the gate decides, server-side.
# ---------------------------------------------------------------------------


def _opencode_plugin_dest(home: Path) -> Path:
    return home / ".config" / "opencode" / "plugins" / "daisugi-gate.ts"


def install_opencode_harness(home: Path) -> Path:
    """Materialize the openDaisugi gate plugin into OpenCode's global plugin
    directory (spec-05), unmodified. Idempotent: re-running overwrites with
    identical content.
    """
    import importlib.resources as _ir

    dest = _opencode_plugin_dest(home)
    dest.parent.mkdir(parents=True, exist_ok=True)
    src = _ir.files("opendaisugi").joinpath("harness_opencode", "plugin", "daisugi-gate.ts")
    text = src.read_text(encoding="utf-8")
    if dest.is_symlink():
        dest.unlink()  # never write THROUGH a pre-planted symlink (arbitrary file write)
    dest.write_text(text, encoding="utf-8")
    return dest


def uninstall_opencode_harness(home: Path) -> list[Path]:
    """Reverse :func:`install_opencode_harness`. Idempotent: absent is fine."""
    dest = _opencode_plugin_dest(home)
    if dest.is_symlink() or dest.exists():
        dest.unlink()
        return [dest]
    return []
```

- [ ] **Step 4: Wire `--harness` into `install_cmd`**

In `src/opendaisugi/cli.py`, add a new option to `install_cmd`'s signature (after the `runtime`
option, ~line 3547):

```python
harness: str | None = (
    typer.Option(
        None,
        "--harness",
        help="Install a single loop harness's gate plugin directly (opencode). "
        "Bypasses the four-layer runtime install above.",
    ),
)
```

Immediately after `home = Path.home()` (~line 3612), before the `try: runtimes = ...` block —
note the corrected honest line (BLOCKER 4: a fresh install resolves to shadow, per
`resolve_gate_mode`'s own documented fallback, not enforce, so the old "or every tool call will
be denied" was false):

```python
    if harness is not None:
        if harness != "opencode":
            typer.echo(f"Unknown --harness {harness!r}. Supported: opencode.", err=True)
            raise typer.Exit(code=1)  # CLI user error (Global Constraints: 1, not the gate's 2)
        from opendaisugi.install import install_opencode_harness, uninstall_opencode_harness

        if do_uninstall:
            removed = uninstall_opencode_harness(home=home)
            if removed:
                typer.echo("Removed the openDaisugi gate plugin for OpenCode:")
                for f in removed:
                    typer.echo(f"  {f}")
            else:
                typer.echo("OpenCode harness was not installed — nothing to remove.")
            return
        if dry_run:
            typer.echo("Would install the openDaisugi gate plugin for OpenCode.")
            return
        path = install_opencode_harness(home=home)
        typer.echo(f"Installed the openDaisugi gate plugin for OpenCode -> {path}")
        typer.echo("OpenCode asks the gate in-process. The gate only watches until you set")
        typer.echo("gate_mode: enforce. Start it with `daisugi start`.")
        return
```

- [ ] **Step 5: Add the honesty-tag row to `modules.py`, naming the enforcement class**

Add a helper near `_claude_hook_installed` (~line 51):

```python
def _opencode_plugin_installed() -> bool:
    """Is the openDaisugi gate plugin materialized in OpenCode's global plugin dir?"""
    return (Path.home() / ".config" / "opencode" / "plugins" / "daisugi-gate.ts").exists()
```

In the `harness` stage's module list (~lines 120-127), add a row after `harness("codex",
"codex")`. The note names the enforcement class (SHOULD-FIX: master §3.5 wants this visible,
not just "installed" — OpenCode's is the strongest in the set: in-process, deny-only, no
fail-open timeout path):

```python
(harness("codex", "codex"),)
(
    Module(
        "opencode",
        ACTIVE if _opencode_plugin_installed() else POSSIBLE,
        "in-process deny-only hook; fail-closed"
        if _opencode_plugin_installed()
        else "`daisugi install --harness opencode`",
    ),
)
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `uv run --no-sync pytest tests/test_install_opencode.py tests/test_modules.py -q`
Expected: PASS.

- [ ] **Step 7: Live smoke test — the plugin loads without crashing the server**

Add to `tests/test_install_opencode.py`:

```python
import base64
import os
import shutil
import subprocess
import time
import urllib.request

_HAS_OPENCODE = shutil.which("opencode") is not None


@pytest.mark.skipif(not _HAS_OPENCODE, reason="requires the opencode CLI on PATH")
def test_live_installed_plugin_does_not_crash_opencode_serve(tmp_path):
    install_opencode_harness(home=tmp_path)
    env = dict(os.environ, HOME=str(tmp_path), OPENCODE_SERVER_PASSWORD="test-token")
    proc = subprocess.Popen(
        ["opencode", "serve", "--port", "0", "--hostname", "127.0.0.1"],
        env=env,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
    )
    try:
        deadline = time.monotonic() + 15
        url = None
        while time.monotonic() < deadline and url is None:
            line = proc.stdout.readline()
            if "listening on" in line:
                url = line.strip().rsplit(" ", 1)[-1]
            if proc.poll() is not None:
                break
        assert url is not None, "opencode serve exited before logging its listen address"
        req = urllib.request.Request(f"{url}/doc")
        req.add_header(
            "Authorization", "Basic " + base64.b64encode(b"opencode:test-token").decode()
        )
        with urllib.request.urlopen(req, timeout=5) as resp:
            assert resp.status == 200
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()
```

Run: `uv run --no-sync pytest tests/test_install_opencode.py -q`
Expected: PASS if `opencode` is installed (proves the shipped plugin file has no syntax error
that crashes the server on load); SKIP with the printed reason otherwise. This test does not
verify a tool call reaches the gate — that needs a real model turn (a provider API key), which
this plan does not fake; verify that manually per the spec's own "Live" test note.

- [ ] **Step 8: Lint and commit**

Run: `uv run --no-sync ruff check src/opendaisugi/install.py src/opendaisugi/cli.py src/opendaisugi/modules.py tests/test_install_opencode.py tests/test_modules.py`

```bash
git add src/opendaisugi/install.py src/opendaisugi/cli.py src/opendaisugi/modules.py tests/test_install_opencode.py tests/test_modules.py
git commit -m "install: daisugi install --harness opencode, no rewritable mode, honest defaults"
```

---

## Final check: the full suite

- [ ] Run: `uv run --no-sync pytest -q`
- [ ] Run: `uv run --no-sync ruff check .`
- [ ] Run: `cd harness/coppice && go vet ./... && go test ./...`
- [ ] Run: `node --test tests/harness_opencode/*.test.ts`
- [ ] Run: `uv run --no-sync python scripts/opencode_discover.py` (confirms the discovery
      script still runs cleanly if `opencode`/`npm` happen to be installed in this environment;
      a "not on PATH" message for either half is an expected pass, not a failure)
- [ ] Confirm `tests/test_hook_format_opencode.py`'s `EXIT_CODE_FORMATS`/`gate_and_contract`
      tests pass — they depend on `plan-04-pi.md` Task 3 having landed (Task 5's header); if
      `plan-04` has not run yet in this checkout, those two tests fail honestly with the message
      Task 5 Step 7 names, and the rest of this plan's suite is unaffected.

Expected: everything green. If `harness/coppice`'s landed `internal/pane`/`internal/proto`
differ from what Tasks 3-4 cite (sibling plans are still being edited in parallel; re-grep the
exact line numbers before acting on this plan), the compiler error names exactly which
identifier to reconcile — the tests and behavior described in this plan do not change.
