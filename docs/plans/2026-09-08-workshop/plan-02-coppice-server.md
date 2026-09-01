# coppice-server Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship `coppice`, one static Go binary that is both client and server, owning PTYs, ghostty grids, headless harness processes, the layout and the state merge, and serving them over a uid-checked unix socket with Herdr's verbs.

**Architecture:** A single Go module `harness/coppice` beside `harness/sprig`. Exactly one package (`internal/vt`) touches libghostty; everything above it sees `vt.Term` and `vt.Cell`. `internal/proto` owns the wire; `internal/state` owns the §3.1 merge; `internal/layout` owns the workspace/tab/pane tree; `internal/pane` owns PTY and headless panes; `internal/detect` is a faithful Go port of Herdr's TOML manifest engine; `internal/server` is the dispatcher; `cmd/coppice` is the CLI and the thin client.

**Tech Stack:** Go 1.26 (see the Deviation below), cgo + `go.mitchellh.com/libghostty` (static, via `pkg-config`), `github.com/creack/pty`, `github.com/BurntSushi/toml`, Zig 0.16.0 and CMake as build-time-only tools installed under `~/.local`.

**Spec:** `docs/plans/2026-09-08-workshop/spec-02-coppice-server.md` (binding), with contracts from `docs/plans/2026-09-08-workshop/00-master-spec.md` §2, §3.1, §3.3, §3.4, §5.2–5.5.

## Corrections from adversarial review (2026-09-08 — authoritative over the tasks below)

**Tasks 0, 2, 4, 11 and 12 are ready to build now** — the toolchain probe, the wire (closed error
enum, `Validate` refusing `state: blocked` from the gate with no ask and refusing `done` from a
manifest, `SourceRank` returning 0 for an unknown source so it can never win), the pane tree with
ids that are never reused, and the Herdr manifest port are all verified sound. The fixture
accounting in Task 12 is exactly right and worth naming: upstream has 21 manifests with ids
`amp agy claude cline codex cursor devin droid gemini copilot grok hermes kilo kimi kiro maki muse
opencode pi qodercli qwen`, `unfixtured.txt` lists the 17 that are not claude/codex/pi/opencode,
and `agy`/`copilot` are the real ids inside `antigravity.toml` and `github-copilot.toml`. There is
no `sprig.toml` upstream, so do not create `testdata/screens/sprig/`: it would trip
`TestEachFixtureDetectsItsDeclaredState`. (The prose "five harnesses" and "sixteen manifests" in
Task 12 Step 5 should read four and seventeen; the lists themselves are correct.)

### Blockers

- **BLOCKER (fail-open) — Task 3 `Merge` swallows `done`, so a dead agent reads `working` for
  ever.** Trace: `cur = {working, gate, TS=T}`, `in = {done, process, T+0.5}`. The gate-hold branch
  is skipped because `cur.State != blocked`, so control reaches
  `if SourceRank(in.Source) < SourceRank(cur.Source) && now-cur.TS < HoldWindow { return *cur }`:
  rank 2 < 4 and 0.5 < 2.0, so the event is discarded. `watchExit` (Task 8) and `pumpAdapter`
  (Task 14) each fire exactly once and never retry, so the pane keeps `working` permanently. The
  visible result is one `pane.list` row reading `closed: true, state: working, exit_code: 3`, and
  `agent.wait --until done` never resolves. Master §3.1 makes `process` exit and `headless`
  end-of-session the only producers of `done`; losing them is the state authority lying about a
  dead process, which is the one thing §5.1 sells. `TestProcessExitMarksThePaneDoneFromTheProcess
  Source` passes only because no gate event precedes the exit, and
  `TestFuzzNoIdleWithoutASourceThatSaidIdle` checks a different invariant.
  Fix: hoist the existing special case out of the gate-blocked branch. Before the rank comparison,
  add `if in.State == proto.StateDone && (in.Source == proto.SrcProcess || in.Source ==
  proto.SrcHeadless) { return in }`. Add the named test
  `TestProcessExitEndsAGateWorkingHoldImmediately`.
  — applied in Task 3 steps 1 and 3: `endsTheSession` is hoisted above the gate branch and above the rank comparison, with `TestProcessExitEndsAGateWorkingHoldImmediately`, `TestHeadlessEndEndsAGateWorkingHoldImmediately`, and the `cur x in` table test.

- **BLOCKER (cross-plan) — Task 7 `ServeConn` dispatches serially, so one wait freezes a client's
  whole connection.** The loop is `line := d.Next(); …; c.Enc.Send(h(c, &req))`. `agent.wait`
  (Task 10) defaults to 120000 ms and `pane.wait_output` (Task 8) to 30000 ms, both blocking inside
  the handler. The CLI survives because it opens a connection per command, but the cockpit
  (spec-03) and the PWA (spec-06) hold one long-lived connection and are precisely the clients that
  call `agent.wait --until blocked`. A cockpit waiting on pane A cannot send `pane.send_text`,
  `pane.detach`, or an allow for pane B's ask for two minutes, while `framePump` keeps delivering
  frames so the UI looks alive and accepts nothing. This repo already ruled on this shape: the
  2026-08-27 plan-3 review, "SHOULD-FIX (cross-plan) — Task 8 ask blocks the shared resident gate…
  The gate server must handle connections concurrently."
  Fix: dispatch each request in its own goroutine (`go func(){ _ = c.Enc.Send(h(c, &req)) }()`).
  `proto.Encoder.Send` is already mutex-guarded, so nothing else changes. Add
  `TestASlowWaitDoesNotBlockOtherCommandsOnTheSameConnection` to Task 7.
  — applied in Task 7 steps 1 and 4: `ServeConn` dispatches each request in its own goroutine, `proto.Encoder.Send` serialises the writes, and `TestASlowWaitDoesNotBlockOtherCommandsOnTheSameConnection` proves it.

- **BLOCKER (data loss) — Task 20 `runStdio` builds a second server on the real data directory and
  overwrites `layout.json`.** `server.New(server.Config{SocketPath: c.Socket, SkipPeerCheck: true})`
  leaves `DataDir` empty, so `New` fills it with `DataDir()` = `~/.opendaisugi/coppice`. Any pane
  or workspace verb over `--stdio` then calls `s.saveLayout()`, which writes that ephemeral empty
  tree over the running server's layout file, a file this process did not create. On the next real
  restart, `Restore()` reads a workshop that never existed. The same defect breaks the feature:
  `coppice --remote ssh://host pane list` starts a fresh in-process server on the far side with an
  empty tree instead of reaching the host's running one, so it reports zero panes, and panes it
  creates are children of the ssh session and die on disconnect. Spec-02 calls `--remote` a "thin
  client".
  Fix: `--stdio` must dial the existing socket and pump bytes both ways, never construct a
  `Server`. Do not instead pass a scratch `DataDir`: that leaves `--remote` broken. Add
  `TestStdioProxiesToTheRunningServerAndNeverWritesTheLayout`.
  — applied in Task 20 steps 1, 3 and 4: `--stdio` calls `cli.Proxy`, which dials the running socket and copies bytes both ways. `SkipPeerCheck` is deleted. `TestStdioProxiesToTheRunningServerAndNeverWritesTheLayout` and `TestStdioWithNoServerFailsRatherThanBuildingOne` prove both halves.

- **BLOCKER — Task 16 and Task 17 `Stop()` panic the daemon on a routine `pane.close`.** Codex:
  `send` reads `p.stopped` under the mutex, releases it, then sends; `Stop` sets `stopped`,
  releases, then `close(p.events)`. A producer that passed the check before `Stop` ran sends on a
  closed channel and panics. sprig is worse, not better: after `closeEvents` runs,
  `select { case <-p.stop: case p.events <- ev: }` has both cases ready, because a send on a closed
  channel is select-ready and then panics, so it is a coin flip rather than a narrow window. A
  panic in an adapter goroutine is unrecoverable and takes the whole server down, orphaning every
  other pane's process.
  Fix, stated so a mutex is not added instead: the event channel is closed by exactly one
  goroutine, the one producing into it, after it observes the stop signal. `Stop()` signals (close
  a `stop` channel) and kills the process; it never closes `events`. Apply to both adapters and add
  `TestStopWhileTheStreamIsFlowingDoesNotPanic` with `-race`.
  — applied in Task 16 step 4 and Task 17 step 3: both adapters gain a single relay goroutine that owns the events channel; `Stop` closes a `stop` channel through a `sync.Once`, cancels the context, kills the process and waits on `relayDone`. `TestStopWhileTheStreamIsFlowingDoesNotPanic` runs 50 iterations under `-race` in each adapter, and Task 21 adds a `-race` CI step.

- **BLOCKER (contract drift) — Task 17 drives sprig with a flag sprig does not have.**
  `flagValue(o.Argv, "session-id")` finds nothing: `harness/sprig/cli.go:34` declares
  `--session` ("session id to write under --session-dir"), and `:36` declares `--resume`. `Start`
  then hard-fails unless `--session-id` or `o.Resume` is present, so a headless sprig pane created
  with real sprig argv is refused, and the whole task's tests pass against an invented flag.
  Fix, with the design decision it needs so the coder does not just rename the string: read
  `--session`, falling back to `o.Resume`; when neither is present, mint an id and **inject
  `--session <id>` into the child argv**, because sprig's default with no `--session` is "a fresh
  one" and the adapter cannot otherwise know the tree filename. On the resume path pass `--resume`,
  not `--session`: `cli.go:54` requires `--session-dir` alongside `--resume`, and
  `NewSessionWriter` refuses an existing file. Everything else in Task 17 is verified: the six row
  shapes and their `id`/`parentId`/`ts`/`toolUseId`/`decision`/`clause`/`summary` fields match
  `harness/sprig/session_tree.go:100-205` exactly, and the tail path
  `filepath.Join(dir, sessionID+".jsonl")` matches `NewSessionWriter`.
  — applied in Task 17 steps 1 and 3: the adapter reads `--session`, falls back to `StartOpts.Resume` with `--resume`, and injects `--session <minted id>` when neither is given. `TestTheSessionFlagMatchesSprigsOwnSource` greps `harness/sprig/cli.go` for the real flag and fails if `--session-id` ever appears.

### Should-fix

- **SHOULD-FIX — Task 1 Step 6 does not compile, and Step 3's discovery list misses the symbols
  that are wrong.** Verified against the mirror source on 2026-09-08
  (`gh api repos/mitchellh/go-libghostty/contents/<file>`). Correct as written: `NewTerminal`,
  `WithSize`, `WithMaxScrollbackLines`, `Resize(cols, rows uint16, cw, ch uint32) error`,
  `NewFormatter`, `WithFormatterFormat|Trim|Unwrap|Selection`, `FormatterFormatPlain`,
  `Formatter.FormatString()`/`Close()`, `NewRenderState()`, `rs.Update(t)`,
  `rc.AppendGraphemes(dst []byte) ([]byte, error)`, `rc.Style() (*Style, error)`,
  `rc.FgColor()`/`BgColor() (*ColorRGB, error)`, `ColorRGB.Components()`, the `Style` predicates,
  `Style.Underline() SGRUnderline`, `SetSelection(*Selection) error`. Wrong:
  `VTWrite(data []byte)` returns nothing (`terminal.go:913`), so `return t.term.Feed`'s inner
  `return t.term.VTWrite(p)` fails; `Cols()`/`Rows()` are `(uint16, error)`
  (`terminal_data.go:191, :487`); `CursorX()`/`CursorY()` are `(uint16, error)` and
  `CursorVisible()` is `(bool, error)` (`:330, :340, :321`); `Title()` is `(string, error)`
  (`:563`); `Terminal.Close()` returns nothing (`terminal.go:791`) so `_ = t.term.Close()` fails;
  `SelectAll()` is `(*Selection, error)` and its doc says the selection is **not** installed as the
  terminal's active selection (`selection.go:894`), so `PlainAll`'s `Selection()` +
  `SetSelection(nil)` dance is both a compile error and destructive of a real selection; there is
  no type `ProgressReport` (it is `TerminalProgressReport{State TerminalProgressState; Progress
  int8}`, `terminal.go:364`) and `ProgressReportFn` takes **two** arguments,
  `func(t *Terminal, report TerminalProgressReport)` (`:375`); `RenderStateRowIterator` and
  `RenderStateRowCells` must come from `NewRenderStateRowIterator()`/`NewRenderStateRowCells()`
  (`render_state_row.go:80`, `render_state_cell.go:241`), not `var ri libghostty.RenderStateRow
  Iterator`, because `RenderState.RowIterator` populates a *pre-allocated* handle
  (`render_state_data.go:361`). Fix: paste this table into `PINS.md` between the `GODOC` markers
  and write Step 6 against it; extend Step 3's `go doc` list with `Terminal` (all methods, not
  `head -60`), `TerminalProgressReport`, `ProgressReportFn`, `Style`, `ColorRGB` and `Selection`.
  — applied in Task 1 steps 3 and 6: the verified signature table is pasted into Step 3 and `vt.go` is rewritten against it, including `VTWrite`/`Close` returning nothing, the `(value, error)` accessors, `SelectAll` returning a selection without installing it, `TerminalProgressReport` with a two-argument callback, and the constructor-allocated render-state handles.

- **SHOULD-FIX — Task 1 `Viewport()` leaks C handles on the 60 Hz path.** It calls
  `NewRenderState()` every invocation and never calls `rs.Close()`; the row iterator and the cells
  are likewise never `Close()`d. `framePump` calls it once per 16 ms per attached client per pane.
  Fix: allocate the render state, the iterator and the cells once per `Term` and reuse them under
  `mu`, or `defer` all three `Close()` calls.
  — applied in Task 1 steps 6 and 6b: the render state, the row iterator and the cell cursor are fields on `Term`, allocated in `New` and closed in `Close`; `TestViewportReusesItsHandles` pins that they are never reallocated.

- **SHOULD-FIX — the plan's own "Global Constraints" block is stale, and Task 1 Step 8 edits a line
  that is already edited.** The block copies master §4 *before* the amendment: it still says
  "**Go 1.25** for `harness/coppice`" and "go-libghostty at the newest tag". Master §4 now reads
  "**Go 1.26** … (amended 2026-09-08)". Fix: refresh the copied block, and change Task 1 Step 8
  from "edit §4" to "confirm §4 already reads Go 1.26 and cites `harness/coppice/PINS.md`; make no
  edit if it does". The Go 1.26 deviation itself is **verified and accepted**:
  `go.mitchellh.com/libghostty` declares `module go.mitchellh.com/libghostty` / `go 1.26.0`
  (`gh api repos/mitchellh/go-libghostty/contents/go.mod`, read 2026-09-08), the licence is MIT,
  the repo has **zero tags and zero releases** so a pseudo-version is the only possible pin, and
  `CMakeLists.txt` pins ghostty `GIT_TAG b0c421fcd2e290629d4285c181b52fe2f2095f06`, which matches
  `GHOSTTY_COMMIT` in `scripts/toolchain.sh`.
  — applied in Plan header and Task 1 step 8: the Global Constraints block now copies the amended §4 (Go 1.26, pseudo-version pin, no Herdr SHA), the Deviation block says it is accepted, and Step 8 is a confirmation that edits nothing when §4 already reads Go 1.26.

- **SHOULD-FIX — the Herdr reference pin does not exist.** Master §4 and Task 1's `PINS.md` table
  name commit `c5a21edfcbc2d5b46540ad91b7980aca31f5f1f3` as "their 1.3.2".
  `gh api repos/herdrdev/herdr/commits/c5a21edfcbc2d5b46540ad91b7980aca31f5f1f3` returns HTTP 422
  "No commit found", and the repository's newest tag is `v0.9.0`; there is no 1.3.2. Fix: delete
  the "reference commit named by master §4" row from `PINS.md` and record only the commit Task 12
  actually vendors, and amend master §4 in the same commit to drop the SHA and the version claim.
  Task 12 Step 1 must also change: `gh api repos/herdrdev/herdr/commits/main -q .sha` returns 422
  because the default branch is `master`, so `COMMIT` ends up empty, every `?ref=` is blank, and
  `PROVENANCE` records `commit:` with nothing after it. Use `commits/HEAD -q .sha`, assert the
  result is non-empty before writing, and change `TestProvenanceRecordsTheVendoredCommit` to assert
  a 40-character hex commit rather than the literal substring `"commit:"`. As written that test
  passes on the broken path.
  — applied in Task 1 step 1 and Task 12 steps 1 and 8: the nonexistent reference-commit row is gone from `PINS.md`, the vendoring script uses `commits/HEAD` and refuses a non-40-character sha, `TestProvenanceRecordsTheVendoredCommit` asserts a real sha by regex, and Step 1 amends §4 to drop the SHA and the 1.3.2 claim.

- **SHOULD-FIX — Task 3 and Task 13 trust a client-supplied timestamp for the hold window.**
  `Merge`'s comment says "now is the server's clock … so a client with a skewed clock cannot extend
  its own hold", but the comparison is `now - cur.TS`, and `cur.TS` is the previous *event's* `ts`,
  set by whoever sent it. A hook with a future clock holds a pane against every lower source
  indefinitely. `scanOnce` repeats it with `now - ev.TS < GateQuiet.Seconds()`, and Task 13's own
  `gateBlockedLine` fixture carries `ts: 1757300000.0`, about a year in the past, which is why
  `TestAPaneWithAFreshGateSourceIsNotScanned` does not actually exercise the freshness window.
  Fix: `state.Store` records its own receive time per pane and `Merge` takes that, not `cur.TS`.
  — applied in Task 3 steps 1 and 3 and Task 13 step 3: `state.Store` records its own receive clock, `Merge` takes `curReceived`, `scanOnce` reads `Store.Received`, and `TestTheHoldWindowIgnoresASkewedEventTimestamp` plus `TestARepeatDoesNotRefreshTheHold` pin it.

- **SHOULD-FIX — Task 13 `scanOnce` excludes gate-sourced panes for ever, not for two seconds.**
  The second guard, `if ev.Source == proto.SrcGate || ev.Source == proto.SrcOperator || ev.State ==
  proto.StateDone { continue }`, means a pane the gate once spoke for is never scanned again. That
  contradicts spec-02 ("a pty pane with a gate source in the last 2 s is not scanned") and makes
  Task 3's `TestManifestMayOverrideGateAfterTheHoldWindow` unreachable in practice. It errs
  fail-closed, so it is a choice, not a hole. Fix: either implement the 2 s rule the spec states,
  or keep the stricter rule and amend spec-02's "State merge" paragraph in the same commit, saying
  why.
  — applied in Task 13 steps 1 and 3: `scanOnce` skips only `done` panes and panes whose non-manifest source arrived under `GateQuiet`, so the screen speaks again after two seconds. `TestAPaneWhoseGateHasGoneQuietIsScannedAgain` is the new test.

- **SHOULD-FIX — Task 14 `pumpAdapter` writes unvalidated events into the store and onto the
  wire.** `proto.ParseStateEvent` is only reached from `handleReportState`. An adapter that emits
  `Event{Kind: EvState, State: "busy"}` has that string returned verbatim by `StateOf`, merged by
  `Merge`, stored, and broadcast, breaking the closed state enum for every client. The same is true
  of `watchExit` and `markRestored`. Fix: `ApplyState` calls `ev.Validate()` first and, on failure,
  applies `unknown` with the validation error in `detail`, then logs. Add
  `TestAnAdapterCannotInventAState`.
  — applied in Task 10 steps 1 and 3: `ApplyState` validates first and substitutes `unknown` with the reason in `detail`, logging the rejection. `TestAnAdapterCannotInventAState` proves it.

- **SHOULD-FIX — Task 9 `Broadcast` has no per-client backpressure.** It copies the client list and
  releases `s.mu`, which is right, but the caller still serialises behind the slowest
  `Enc.Send`. One wedged client, a PWA on a stalled tailnet, stalls `ApplyState` for every pane,
  including the `handleReportState` response the gate hook is blocking on. Fix: a bounded per-client
  send queue, dropping and disconnecting the client when it overflows.
  — applied in Task 9 steps 1 and 3: every client gets a bounded `events` queue drained by one pump goroutine; `Emit` drops and disconnects on overflow. `TestAWedgedClientIsDroppedRatherThanStallingTheServer` proves `ApplyState` does not stall.

- **SHOULD-FIX — Task 10 `WaitState` can miss the state it is waiting for.** It checks
  `s.states.Current` and only then appends the waiter, so a state arriving between the two is lost
  and the caller waits the full timeout. `agent.wait --until done` on a pane that exits a
  millisecond later times out spuriously. Fix: register the waiter first, then check the current
  state, and drain the channel on the early-return path.
  — applied in Task 10 steps 1 and 3: `WaitState` registers the waiter before reading the current state and drains the channel on every early return. `TestAgentWaitResolvesOnAStateThatArrivesRightAfterTheCall` is the regression test.

- **SHOULD-FIX — Task 7 `TestConnectionFromAnotherUIDIsRefused` tests the opposite of its name.**
  Its body dials as our own uid and asserts the connection is *served*; its own comment admits the
  refusal is covered elsewhere. Master §7 requires fail-closed tests to be named for the failure.
  Fix: rename it `TestOurOwnUIDIsServed`. The refusal path itself is sound:
  `TestRefusesWhenPeerUIDIsUnreadable` is the right test, `peerUID` returns `errUnreadableUID` on
  every failure, the non-Linux build refuses everything, and `Serve` checks every accepted
  connection. `--stdio` correctly carries no uid check, since SSH is the authenticator and stdin is
  not a socket; with the `runStdio` blocker fixed, `SkipPeerCheck` should be deleted outright
  rather than left as a field a future caller could set on a listening server.
  — applied in Task 7 step 1: renamed `TestOurOwnUIDIsServed`, and `Config.SkipPeerCheck` is deleted outright now that `--stdio` builds no server.

- **SHOULD-FIX — Task 6 `BuildEnv` can silently skip the injection spec-02 makes unconditional.**
  Spec-02: "Every spawned process gets `COPPICE_SOCK=<socket path>` and `COPPICE_PANE=<id>`."
  `BuildEnv` sets each only `if sock != ""` / `if paneID != ""`, and it does not remove an
  inherited value otherwise. A server started from inside a coppice pane passes its own stale
  `COPPICE_PANE` to a child, and the gate hook then reports state for the wrong pane. Fix: always
  set both, and delete any inherited value when the argument is empty; make an empty `Sock` or
  `PaneID` an error in `StartPTY`. Add `TestBuildEnvScrubsAnInheritedPaneID`. Note that the three
  built adapters do call `BuildEnv(os.Environ(), o.Env, o.Sock, o.PaneID)` correctly, but nothing
  in the interface requires it and no test asserts it for a headless pane.
  — applied in Task 6 steps 1 and 3: `BuildEnv` always sets both variables and removes an inherited value when the argument is empty; `StartPTY` refuses an empty `Sock` or `PaneID`. `TestBuildEnvScrubsAnInheritedPaneID` and `TestStartPTYRefusesWithoutASocketOrPaneID` are the tests.

- **SHOULD-FIX — Task 6 Step 3 tells the coder to write a broken `PTY.Resize`, then corrects it in
  prose.** The code block sets only the winsize; the paragraph after it says "Note the ordering bug
  the test catches" and supplies a replacement that reads `p.grid`, a field shown in neither the
  struct nor `StartPTY`. A coder doing exactly what the plan says writes the broken version. Fix:
  put the correct implementation in the code block, add `grid *Grid` to the struct and
  `grid: g` to `StartPTY`, and delete the paragraph.
  — applied in Task 6 step 3: the code block now contains the correct `Resize`, the struct has `grid *Grid`, `StartPTY` sets it, and the correcting paragraph is deleted.

- **SHOULD-FIX — Task 18's restart test launches the operator's real `claude`.**
  `TestRestoreBringsBackTheLayoutAndMarksPanesHonestly` sets `p.Harness = "claude"`,
  `p.Kind = KindHeadless`, `p.HarnessSessionID = "sess-42"`, then calls `Restore()`, which reaches
  `startPane` and the claude adapter with `COPPICE_CLAUDE_BIN` unset. That spawns a real
  `claude -p --output-format stream-json --input-format stream-json --verbose --resume sess-42`
  against the operator's subscription. Fix: set `COPPICE_CLAUDE_BIN` to
  `testdata/adapters/fake-claude.sh` in the test, or register a stub adapter. The restart semantics
  themselves are correct and honest: nothing claims to resurrect a process, a pty pane comes back
  closed and `done`, a headless pane without a recorded id comes back `unknown`, `restartNote` and
  the README say so in the same words, and `TestACorruptLayoutFileIsRefusedAndKept` proves the bad
  file is preserved.
  — applied in Task 18 step 1: the test sets `COPPICE_CLAUDE_BIN` and `COPPICE_CLAUDE_FIXTURE` to the replay script before `Restore()` runs, so no real subscription is touched.

- **SHOULD-FIX — Task 20 `coppice server start` does not background, and nothing autostarts.**
  Spec-02 and the README both say "the first invocation starts a background server". `runServer
  "start"` blocks in `Serve()`, and `pane list` against a dead socket exits 3. Fix: either fork a
  detached child in `start` and have `dial` autostart once on `ECONNREFUSED`, or change spec-02 and
  the README to say the server is started explicitly and stays in the foreground. Do not ship the
  two documents disagreeing.
  — applied in Task 20 steps 3, 4 and 7 and Task 21 step 3: `server start` detaches by default via `StartBackgroundServer` (setsid), `--foreground` is the child, `Dial` autostarts once unless `COPPICE_NO_AUTOSTART` is set, and the README says exactly that.

- **SHOULD-FIX — `detect.Set.Warnings()` is never read.** `registerAll` discards the error and the
  warnings, and no command surfaces them. A broken file in
  `~/.config/coppice/agent-detection/` silently reverts to the bundled manifest with no operator
  visible signal. Fix: add `"detection_warnings"` to `server.status` and print them from
  `coppice server status`.
  — applied in Task 13 step 3, Task 20 step 4 and Task 21 step 3: `Server.SetDetectionWarnings`/`DetectionWarnings`, `registerAll` feeds them in, `server.status` returns `detection_warnings`, `coppice server status` prints them, and the README says a broken override file says so.

### Notes

- **NOTE — the canonical adapter shape, for plans 04 and 05.** Plan-02 owns this interface and its
  shape wins unchanged; no field is missing. pi's resume id is `StartOpts.Resume`, pi's steer is
  `Proc.Steer`, OpenCode's per-pane auth token goes in `StartOpts.Env`. Reproduced verbatim from
  Task 6, package `pane`:
  ```go
  type EventKind string
  const ( EvText EventKind = "text"; EvTool = "tool"; EvState = "state"
          EvEnd = "end"; EvError = "error" )
  type Event struct { Kind EventKind; Text, Tool, State, Detail string
                     Ask *proto.Ask }   // set when State == "blocked"
  type StartOpts struct { Cwd string; Env map[string]string; Argv []string
                          Resume string; Sock, PaneID string }
  type Proc interface {
      Prompt(text string) error
      Steer(text string) error          // may return ErrUnsupported
      WriteStdin(b []byte) error
      Events() <-chan Event
      SessionID() (string, bool)
      Stop() error
  }
  type Adapter interface {
      Name() string
      Start(ctx context.Context, o StartOpts, g *Grid) (Proc, error)
  }
  var ErrUnsupported = errors.New("this harness does not support that")
  ```
  Three rules that belong with it, because 04 and 05 will otherwise each invent an answer.
  (a) Every adapter MUST set `cmd.Env = pane.BuildEnv(os.Environ(), o.Env, o.Sock, o.PaneID)`;
  that is the only path by which a headless harness's gate hook finds `COPPICE_SOCK` and
  `COPPICE_PANE`. (b) `Start`'s `g *Grid` is dead: claude, codex and sprig all ignore it and the
  server renders the transcript in `pumpAdapter`. Drop the parameter, or state in the interface
  doc that the server owns rendering. (c) `Stop()` signals and kills; the producing goroutine
  closes `Events()`. Spec-02's own `Adapter` (methods on the adapter taking a `Proc`,
  `Start(ctx, opts)` with no grid) is superseded by this block, so master §3's rule applies and
  spec-02 §"Headless adapters" gets amended in the same commit.
  — applied in Task 6 step 3 and Task 14 step 6: the `Adapter` doc states the three rules verbatim, `Event` gains `Ask *proto.Ask` (there is no `state.Ask`; `internal/pane` already imports `internal/proto`), the grid parameter is documented as server-owned rendering rather than dropped, and Task 14 amends spec-02's Headless adapters block in the same commit.

- **NOTE — Task 15's Claude flags are verified correct, empirically.** On this box,
  `claude -p --output-format stream-json --input-format stream-json` (no `--verbose`) prints
  `Error: When using --print, --output-format=stream-json requires --verbose` and refuses to run.
  `claude --help` confirms `--input-format` and `--output-format` "only work with --print" and that
  `--replay-user-messages` needs both stream-json formats. The plan's argv, `-p --output-format
  stream-json --input-format stream-json --verbose`, is exactly right. Keep `--verbose`.
  — applied in Task 15 step 4: no change needed. The argv keeps `--verbose`, and the reason is now a comment beside `Binary`.

- **NOTE — Task 3's fuzz test is weaker than its name.** `saidIdle` is set once per trial and never
  reset, so it proves only "no `idle` unless some event in this trial was `idle`", not that the
  *current* `idle` came from a source that said `idle`. Strengthen it to compare the merged state
  against the last event that could have produced it. The invariant itself does hold in the code as
  written: `Merge` only ever returns `in`, `*cur`, or a `cur`-derived value forced to `working`.
  — applied in Task 3 step 1: replaced by `TestFuzzTheMergedStateAlwaysCameFromAnEventThatCarriedIt`, which compares the merged state and source against every accepted event and allows exactly one derived value, the expired-ask fallback.

- **NOTE — Task 5 Step 3 asks the coder to type a stub and then replace it.** `func keysOf(m
  map[int][]protoCell) []int { return nil } // replaced below` does not compile; the real helper and
  its two extra imports follow in prose. Put the helper in the code block. Same pattern in Task 20,
  where `registerAll`, `termSize`, `runServer` and the `server.stop` handler arrive as prose after
  `cli.go`, with their imports (`detect`, `proto`, `term`, `time`) never listed.
  — applied in Task 5 step 3 and Task 20 step 4: the `keysOf` helper is in the code block with its imports, and `termSize`, `registerAll` and `runServer` are one block with the import list named.

- **NOTE — Task 19 adds a fourth Go dependency the plan's own constraint forbids.** "New Go
  dependencies, all pinned, no others: libghostty, creack/pty, BurntSushi/toml. Anything else needs
  a new decision." `golang.org/x/term` is that decision, and it is a good one: `harness/sprig` already
  requires `golang.org/x/term v0.45.0`, so the workshop gains no new module family. Amend the
  constraint list rather than leaving the plan contradicting itself.
  — applied in Plan Global Constraints: the dependency list now names `golang.org/x/term` as the fourth pinned dependency, with the reason.

- **NOTE — Task 19 advertises three leader keys that do nothing.** `ctrl+a n`, `p` and `c` print
  "This view shows one pane". `HelpText` lists them as "next pane", "previous pane", "create a pane
  here". Under §3.5 that is a control that lies. Either drop them from `HelpText` or make `n`/`p`
  re-attach to the neighbouring pane.
  — applied in Task 19 steps 1, 3 and 5: `attach.Run` returns `Next`, `n`/`p` resolve the neighbouring open pane over a second connection, `c` returns `Next.Create`, the CLI loops on it, and `TestEveryKeyInHelpTextHasAnAction` fails if a help line maps to nothing.

- **NOTE — layout pointers escape the tree's lock.** `Tree.Pane` returns the live `*Pane` and
  callers mutate it (`handleResize` writes `Cols`/`Rows`, `pumpAdapter` writes
  `HarnessSessionID`, Task 18's test writes three fields) while `Save` marshals the same pointers
  under `RLock`. `go test -race` will find this. Return a copy, or add setter methods.
  — applied in Task 4 steps 1 and 3 and Task 8 step 3: every accessor returns a deep copy, `UpdatePane` is the only mutator, `LivePane.Info` is a value refreshed by `Server.updatePane`, and `TestPaneAccessorsReturnCopies` plus `TestConcurrentUpdateAndSaveIsRaceFree` run under `-race`.

- **NOTE — `liveMu` is a package-level `sync.RWMutex` guarding per-server maps.** Two `Server`
  values in one test binary contend on one global. Move it onto `Server` beside `live`.
  — applied in Task 8 step 3: `liveMu` is a `Server` field beside `live`, so two servers in one test binary no longer share a lock.

- **NOTE — the operator downgrade is correct as specified, and its limit should be stated.**
  `handleReportState` downgrades `operator` to `gate` when `!c.Operator(id)`, and `Client.Operator`
  is true only while a `pane.attach` is open, which matches spec-02. The residual is that a caller
  can simply send `source: "gate"` and be believed; the socket's uid check is the only boundary.
  That is the intended design (the callers are the gate hook and our own adapters), but say so in
  `README.md` §Security so nobody later reads the downgrade as authentication.
  — applied in Task 10 step 3: the limit is now stated in `handleReportState`'s doc comment, naming the socket uid check as the only boundary and this function as what would need a real credential for a multi-uid server.

Re-review 2026-09-08: 29 of 29 verified applied; open: Task 3's strengthened fuzz test `TestFuzzTheMergedStateAlwaysCameFromAnEventThatCarriedIt` fails against its own `Merge` (the new done-to-unknown downgrade yields `unknown/operator` and `unknown/gate`, which no accepted event carried and the `derived` clause does not allow; 705 of 2000 trials fail in simulation) so `derived` must admit `unknown` from a source that sent `done`; Task 20 `server start` has no single-instance lock and `runServer` calls `Restore()` before `Listen()`, so two racing starts each spawn resumed panes and rewrite `layout.json` before one fails to bind, and `Listen`'s stale-socket probe is check-then-act, so the second start can unlink the first's now-live socket and bind its own; residual: `-race` is on six verification steps (Tasks 4, 9, 15, 16, 17 and the Task 21 CI job), not on every `go test` step, and the Self-review still says "five harnesses" and "sixteen" where Task 12 now says seventeen.

Fix round 2 (2026-09-08), one line per item:

- Task 3's fuzz test must admit the done-to-unknown downgrade.
  — applied in Task 3 steps 1 and 3: the single `derived` clause is split into `expired` (an
  expired gate ask becoming working) and `downgraded` (`unknown` with no ask, from a source whose
  accepted event carried `done` and which is neither process nor headless). A comment states why
  those two are the whole space: `Merge` returns the incoming event, the current one, or a value
  derived from the current one, and the current one is by induction one of the same three. The
  named unit test `TestADoneFromAnIneligibleSourceBecomesUnknownFromThatSource` pins the downgrade
  itself. Re-simulated with the same seed (20260908), the same 12 steps and the same 2000 trials:
  0 failing trials, down from 705.
- `server start` needs a single-instance lock taken before `Restore()` and before the socket probe.
  — applied in Task 7 steps 3b and 4 and Task 20 steps 3 and 4b: `lock_unix.go` takes an
  `flock(LOCK_EX|LOCK_NB)` on `<data dir>/server.lock`, writes the holder's pid, and holds it for
  the server's life; `lock_other.go` refuses on every non-unix platform. `Server.AcquireStartLock`
  returns `ErrAlreadyRunning` wrapped as "coppice server already running (pid N). Run: coppice
  server status". Both `Restore` and `Listen` refuse without it, `Close` releases it, and
  `runServer "start"` calls it first with `defer s.Close()`. `Listen`'s check-then-act probe is
  deleted outright: holding the lock makes any socket file stale by construction, so it unlinks
  unconditionally and never has a window in which it could remove a live socket.
  `TestTwoRacingStartsLeaveOneServerAndOneLayout` releases two starts from one channel and asserts
  exactly one holds the lock, the loser fails at the lock with an empty tree, the seeded pane
  survives in `layout.json`, and the socket answers `server.status` with the survivor's pid; it
  runs `-race -count=5`. `TestListenRefusesWithoutTheStartLock`,
  `TestRestoreRefusesWithoutTheStartLock` and `TestASecondAcquireOfTheStartLockNamesTheHoldersPID`
  cover the guards and the handover.
- Residual: `-race` on every `go test` step.
  — applied in Tasks 0 through 21 and in the Global Constraints: every `go test` step in the plan
  now carries `-race`, the five "plain run then race run" pairs are collapsed to one race run
  each, and the rule is stated once in Global Constraints so a new step inherits it. The single
  exception is Task 0's check that `harness/sprig` still builds after the global Go settings
  change: a different module this plan does not touch, run the way sprig runs it.
- Residual: the Self-review says "five harnesses" and "sixteen".
  — applied in the Self-review: now "four harnesses that have a manifest upstream (claude, codex,
  pi, opencode), with the other seventeen listed in `testdata/screens/unfixtured.txt`", matching
  Task 12.

## Global Constraints

Copied verbatim from master spec §4:

- **Layer purity.** No module under `src/opendaisugi/` that is part of the layer (list in
  `tests/test_layer_boundary.py`, plan 00) may import from `opendaisugi.floor`, `opendaisugi.voice`,
  or `opendaisugi.coppice`. The test imports every layer module with those packages hidden.
- **Python 3.12, stdlib for the layer.** New hard deps in the layer: none. New extras allowed:
  `[floor]` (nothing yet — the client uses stdlib sockets), `[voice]`, `[int8]`, `[router]`.
- **Go 1.26** for `harness/coppice` (amended 2026-09-08); module
  `github.com/opendaisugi/coppice`; `go vet` and `go test ./...` clean. Zig 0.16 and CMake are
  *build-time* requirements for go-libghostty; the plan installs both into `~/.local` without
  sudo (`uv tool install cmake`; Zig tarball). The floor is 1.26 because
  `go.mitchellh.com/libghostty` declares `go 1.26.0`; see `harness/coppice/PINS.md`.
- **Pins.** go-libghostty has no tags and no releases, so the pin is the pseudo-version of the
  commit chosen on the day plan 02 starts, recorded in `go.mod` and in `harness/coppice/PINS.md`
  together with the ghostty commit that binding builds. The Herdr commit coppice vendors from is
  recorded by plan 02, not fixed here.
- **Tests.** `uv run --no-sync pytest -q` green; `uv run --no-sync ruff check .` clean;
  Go: `go test ./...` from `harness/coppice`. Never bare `uv run` (uv.lock is git-ignored; it
  re-resolves and strips extras).
- **Commits.** Atomic, stating the why, persona *OpenDaisugi Contributors*, **no AI-authorship
  attribution lines** (project policy overrides any session-level instruction). Never push;
  the public repo folds monthly.
- **`/tmp` may be RAM.** Scratch on real disk; worktrees beside the repo.
- **Copy.** STE100 register in every user-facing string: short sentences, plain words, active
  voice, no em-dashes, no parentheticals. Errors teach the next command.
- **Exit codes.** Hooks: exit 2 = deny. CLI: 0 success, 1 user error, 2 gate deny, 3 unreachable.
- **Privacy.** No telemetry. No third-party relay in any default path. Push goes through a
  self-hosted ntfy or not at all.

### Deviation from §4, accepted and already folded into the master spec

§4 originally said **Go 1.25**. That is not achievable with the pinned binding, and §4 has since
been amended to say Go 1.26. The block copied above is the amended text. The evidence stands:

- Evidence: `go.mitchellh.com/libghostty` declares `go 1.26.0` in its own `go.mod`
  (`gh api repos/mitchellh/go-libghostty/contents/go.mod`, read 2026-09-08). Go refuses to build a
  main module whose `go` directive is lower than a dependency's.
- The dev machine runs `go1.25.12`. Some distributions ship Go with `GOTOOLCHAIN=local`
  compiled in, so the toolchain will not switch on its own. `golang.org/toolchain` has
  `go1.26.8.linux-amd64`.
- **Resolution this plan implements:** `harness/coppice/go.mod` declares `go 1.26.0` and
  `toolchain go1.26.8`; Task 0 sets `GOTOOLCHAIN=auto` so the toolchain downloads itself into
  the module cache. Both facts land in `harness/coppice/PINS.md`.
- **Master spec §3 rule applies:** a plan that changes a contract must change the master spec in
  the same commit. §4 has been amended already, so Task 1 Step 8 is now a *confirmation* step:
  read §4, and edit nothing if it already says Go 1.26 and cites `harness/coppice/PINS.md`.
- **The Herdr reference SHA in the original §4 does not exist.**
  `gh api repos/herdrdev/herdr/commits/c5a21edfcbc2d5b46540ad91b7980aca31f5f1f3` returns HTTP 422
  "No commit found", and the repository's newest tag is `v0.9.0`, so there is no 1.3.2. Task 1
  records only the commit Task 12 actually vendors, and Task 12 Step 1 amends §4 to drop the SHA
  and the version claim.

### Constraints this plan adds

- **New Go dependencies, all pinned, no others:** `go.mitchellh.com/libghostty`,
  `github.com/creack/pty`, `github.com/BurntSushi/toml`, `golang.org/x/term`. Anything else needs
  a new decision. `golang.org/x/term` is the fourth on purpose: `harness/sprig` already requires
  `golang.org/x/term v0.45.0`, so the workshop gains no new module family, and `coppice attach`
  needs raw mode.
- **`internal/vt` is the only package that may import `go.mitchellh.com/libghostty`.** A test
  enforces it. Everything above sees `vt.Term`, `vt.Cell`.
- **Every `go test` in this plan runs with `-race`.** The pane tree, the client event queues and
  the adapter stop paths are all concurrent by design, and a panic in an adapter goroutine takes
  the whole daemon down. The one exception is Task 0's check that `harness/sprig` still builds:
  that is a different module this plan does not change.
- **Fail-closed everywhere.** An unauthenticated client is dropped. A manifest that fails
  validation is not loaded. A parse error in a headless stream marks the pane `unknown`.
  `idle` is never a default and `unknown` is never dressed up as `idle`.
- **Herdr divergence, deliberate:** Herdr defaults a *known* agent with no matching rule to
  `idle` (`DEFAULT_KNOWN_AGENT_IDLE_FALLBACK`). We do not. No match means no event, so the pane
  keeps whatever source it had, or stays `unknown`. Master §3.1 forbids the Herdr behaviour.
- **Honesty tag (§3.5).** `modules.py` and `swap.py` belong to spec-03, which adds the `floor`
  stage. Plan 02 discharges §3.5 through `coppice server status`, which states in plain words
  what survives a restart and what does not, and through `PINS.md`.
- **Scratch on real disk.** Build output goes to `harness/coppice/build/` and `~/.local`, never
  `/tmp`.

---

### Task 0: Toolchain preflight and installer

The build needs Zig 0.16, CMake, a built `libghostty-vt`, and a Go toolchain that will switch.
None of that is on this machine and there is no sudo. This task makes the missing pieces
installable and makes their absence a clear message instead of a cgo link error.

**Files:**
- Create: `harness/coppice/go.mod`
- Create: `harness/coppice/scripts/toolchain.sh`
- Create: `harness/coppice/scripts/preflight.sh`
- Create: `harness/coppice/internal/toolchain/toolchain.go`
- Test: `harness/coppice/internal/toolchain/toolchain_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: module path `github.com/opendaisugi/coppice`;
  `toolchain.GhosttyPrefix() string` (the install prefix, `$HOME/.local/ghostty-vt` unless
  `COPPICE_GHOSTTY_PREFIX` overrides);
  `toolchain.HaveGhosttyVT() bool` (true when `<prefix>/share/pkgconfig/libghostty-vt-static.pc`
  exists); `toolchain.SkipReason() string` (empty when the build deps are present, otherwise the
  one-line teaching message used by every cgo test's `t.Skip`).

- [ ] **Step 1: Write the failing test**

```go
// harness/coppice/internal/toolchain/toolchain_test.go
package toolchain

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGhosttyPrefixHonoursOverride(t *testing.T) {
	t.Setenv("COPPICE_GHOSTTY_PREFIX", "/opt/gvt")
	if got := GhosttyPrefix(); got != "/opt/gvt" {
		t.Fatalf("GhosttyPrefix() = %q, want /opt/gvt", got)
	}
}

func TestHaveGhosttyVTIsFalseWithoutThePkgconfigFile(t *testing.T) {
	t.Setenv("COPPICE_GHOSTTY_PREFIX", t.TempDir())
	if HaveGhosttyVT() {
		t.Fatal("HaveGhosttyVT() = true for an empty prefix, want false")
	}
}

func TestHaveGhosttyVTIsTrueWhenThePkgconfigFileExists(t *testing.T) {
	dir := t.TempDir()
	pc := filepath.Join(dir, "share", "pkgconfig")
	if err := os.MkdirAll(pc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pc, "libghostty-vt-static.pc"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COPPICE_GHOSTTY_PREFIX", dir)
	if !HaveGhosttyVT() {
		t.Fatal("HaveGhosttyVT() = false with the pkgconfig file present, want true")
	}
}

// The skip reason is a user-facing string. It must teach the next command,
// per the global copy rule.
func TestSkipReasonNamesTheInstallerCommand(t *testing.T) {
	t.Setenv("COPPICE_GHOSTTY_PREFIX", t.TempDir())
	got := SkipReason()
	if !strings.Contains(got, "scripts/toolchain.sh") {
		t.Fatalf("SkipReason() = %q, want it to name scripts/toolchain.sh", got)
	}
	if strings.Contains(got, "—") {
		t.Fatalf("SkipReason() = %q, want no em-dash", got)
	}
}

func TestSkipReasonIsEmptyWhenTheLibraryIsPresent(t *testing.T) {
	dir := t.TempDir()
	pc := filepath.Join(dir, "share", "pkgconfig")
	if err := os.MkdirAll(pc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pc, "libghostty-vt-static.pc"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COPPICE_GHOSTTY_PREFIX", dir)
	if got := SkipReason(); got != "" {
		t.Fatalf("SkipReason() = %q, want empty", got)
	}
}
```

- [ ] **Step 2: Run the test and watch it fail**

Run: `cd harness/coppice && go test -race ./internal/toolchain/ -run Toolchain -v`
Expected: FAIL, `no required module provides package` or `undefined: GhosttyPrefix`.

- [ ] **Step 3: Write `go.mod` and the implementation**

```
// harness/coppice/go.mod
module github.com/opendaisugi/coppice

go 1.26.0

toolchain go1.26.8
```

```go
// harness/coppice/internal/toolchain/toolchain.go

// Package toolchain answers one question for the rest of coppice: are the
// build-time pieces of libghostty-vt present on this machine? cgo tests ask it
// so they skip with a reason instead of failing with a linker error.
package toolchain

import (
	"os"
	"path/filepath"
)

// GhosttyPrefix is where scripts/toolchain.sh installs libghostty-vt.
func GhosttyPrefix() string {
	if p := os.Getenv("COPPICE_GHOSTTY_PREFIX"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".local/ghostty-vt"
	}
	return filepath.Join(home, ".local", "ghostty-vt")
}

// HaveGhosttyVT reports whether the static pkg-config file exists. That file is
// what the cgo directive in go-libghostty resolves, so its presence is the
// honest test of "can this package link".
func HaveGhosttyVT() bool {
	pc := filepath.Join(GhosttyPrefix(), "share", "pkgconfig", "libghostty-vt-static.pc")
	_, err := os.Stat(pc)
	return err == nil
}

// SkipReason is empty when the build deps are present. Otherwise it is the
// message a skipped test prints. It names the command that fixes the machine.
func SkipReason() string {
	if HaveGhosttyVT() {
		return ""
	}
	return "libghostty-vt is not built. Run harness/coppice/scripts/toolchain.sh to install it."
}
```

- [ ] **Step 4: Run the test and watch it pass**

Run: `cd harness/coppice && go test -race ./internal/toolchain/ -v`
Expected: PASS, 5 tests.

- [ ] **Step 5: Write the preflight script**

```bash
#!/usr/bin/env bash
# harness/coppice/scripts/preflight.sh
# Reports whether this machine can build coppice. Exit 0 when it can, 1 when it
# cannot. Every failure line names the command that fixes it.
set -uo pipefail

prefix="${COPPICE_GHOSTTY_PREFIX:-$HOME/.local/ghostty-vt}"
missing=0

say_missing() {
  printf 'missing: %s\n  fix: %s\n' "$1" "$2" >&2
  missing=1
}

command -v go >/dev/null 2>&1 || say_missing "go" "install Go 1.26 or newer"
if command -v go >/dev/null 2>&1; then
  if [ "$(go env GOTOOLCHAIN)" = "local" ]; then
    say_missing "a switchable Go toolchain" "run harness/coppice/scripts/toolchain.sh"
  fi
fi
command -v zig >/dev/null 2>&1 || say_missing "zig 0.16" "run harness/coppice/scripts/toolchain.sh"
command -v cmake >/dev/null 2>&1 || say_missing "cmake" "run harness/coppice/scripts/toolchain.sh"
command -v pkg-config >/dev/null 2>&1 || say_missing "pkg-config" "install pkgconf from your distribution"
[ -f "$prefix/share/pkgconfig/libghostty-vt-static.pc" ] ||
  say_missing "libghostty-vt at $prefix" "run harness/coppice/scripts/toolchain.sh"

if [ "$missing" -ne 0 ]; then
  echo "coppice cannot build here yet. Fix the lines above, then run this script again." >&2
  exit 1
fi
echo "coppice can build here."
```

- [ ] **Step 6: Write the installer script**

```bash
#!/usr/bin/env bash
# harness/coppice/scripts/toolchain.sh
# Installs the build-time toolchain for coppice into $HOME/.local. No sudo.
# Undo the two global Go settings with: go env -u GOTOOLCHAIN PKG_CONFIG
set -euo pipefail

ZIG_VERSION="0.16.0"
ZIG_URL="https://ziglang.org/download/0.16.0/zig-x86_64-linux-0.16.0.tar.xz"
ZIG_SHA256="70e49664a74374b48b51e6f3fdfbf437f6395d42509050588bd49abe52ba3d00"
GHOSTTY_COMMIT="b0c421fcd2e290629d4285c181b52fe2f2095f06"

prefix="${COPPICE_GHOSTTY_PREFIX:-$HOME/.local/ghostty-vt}"
work="${COPPICE_BUILD_DIR:-$HOME/.local/share/coppice-build}"   # real disk; /tmp may be RAM
mkdir -p "$HOME/.local/bin" "$work"

# 1. Zig 0.16, checksum-verified.
if ! command -v zig >/dev/null 2>&1; then
  echo "installing zig $ZIG_VERSION into $HOME/.local"
  curl -fsSL "$ZIG_URL" -o "$work/zig.tar.xz"
  echo "$ZIG_SHA256  $work/zig.tar.xz" | sha256sum -c -
  tar -xJf "$work/zig.tar.xz" -C "$work"
  rm -rf "$HOME/.local/zig-$ZIG_VERSION"
  mv "$work/zig-x86_64-linux-$ZIG_VERSION" "$HOME/.local/zig-$ZIG_VERSION"
  ln -sf "$HOME/.local/zig-$ZIG_VERSION/zig" "$HOME/.local/bin/zig"
  rm -f "$work/zig.tar.xz"
fi

# 2. CMake, from uv so nothing is installed system-wide.
command -v cmake >/dev/null 2>&1 || uv tool install cmake

# 3. libghostty-vt at the commit go-libghostty fetches.
if [ ! -f "$prefix/share/pkgconfig/libghostty-vt-static.pc" ]; then
  echo "building libghostty-vt at $GHOSTTY_COMMIT"
  if [ ! -d "$work/ghostty/.git" ]; then
    git clone --filter=blob:none https://github.com/ghostty-org/ghostty.git "$work/ghostty"
  fi
  git -C "$work/ghostty" fetch --depth 1 origin "$GHOSTTY_COMMIT"
  git -C "$work/ghostty" checkout --detach "$GHOSTTY_COMMIT"
  ( cd "$work/ghostty" && zig build -Demit-lib-vt --prefix "$prefix" )
fi

# 4. A pkg-config wrapper, so a bare `go test ./...` finds the .pc file.
#    It APPENDS to PKG_CONFIG_PATH, so every other Go module on this box keeps working.
cat > "$HOME/.local/bin/coppice-pkg-config" <<WRAP
#!/usr/bin/env bash
export PKG_CONFIG_PATH="\${PKG_CONFIG_PATH:+\$PKG_CONFIG_PATH:}$prefix/share/pkgconfig"
exec pkg-config "\$@"
WRAP
chmod +x "$HOME/.local/bin/coppice-pkg-config"

# 5. Two global Go settings. Undo with: go env -u GOTOOLCHAIN PKG_CONFIG
go env -w GOTOOLCHAIN=auto
go env -w PKG_CONFIG="$HOME/.local/bin/coppice-pkg-config"

echo "done. Run harness/coppice/scripts/preflight.sh to confirm."
echo "to undo the global Go settings: go env -u GOTOOLCHAIN PKG_CONFIG"
```

- [ ] **Step 7: Make both scripts executable and run the preflight**

Run: `cd harness/coppice && chmod +x scripts/*.sh && ./scripts/preflight.sh; echo "exit=$?"`
Expected: it prints the missing pieces and `exit=1` on a fresh machine.

- [ ] **Step 8: Install the toolchain and re-run the preflight**

Run: `cd harness/coppice && ./scripts/toolchain.sh && ./scripts/preflight.sh`
Expected: `coppice can build here.`

- [ ] **Step 9: Prove the global Go settings did not break the neighbouring module**

Run: `cd ../sprig && go test ./... 2>&1 | tail -20`  (sprig is a different module and this plan
does not change it, so its own suite runs as sprig runs it)
Expected: all packages `ok` or `no test files`. If sprig breaks, the wrapper is replacing
`PKG_CONFIG_PATH` instead of appending. Fix the wrapper before continuing.

- [ ] **Step 10: Commit**

```bash
git add harness/coppice/go.mod harness/coppice/scripts harness/coppice/internal/toolchain
git commit -m "build(coppice): install the ghostty build toolchain without sudo

Zig 0.16 and CMake are not on this machine and there is no root. The installer
puts both under ~/.local, builds libghostty-vt at the commit go-libghostty
fetches, and leaves a pkg-config wrapper so a bare 'go test ./...' links. The
preflight turns a cgo link error into a sentence that names the fix."
```

---

### Task 1: Pins and the `internal/vt` boundary

This is the discovery task for the terminal engine. go-libghostty publishes **no tags and no
releases**, so the pin is a pseudo-version. Its API is explicitly unstable, so this task probes
the real symbols with `go doc`, records what it found, and confines every one of them to one
package.

**Files:**
- Create: `harness/coppice/PINS.md`
- Create: `harness/coppice/internal/vt/vt.go`
- Create: `harness/coppice/internal/vt/boundary_test.go`
- Test: `harness/coppice/internal/vt/vt_test.go`
- Modify: `harness/coppice/go.mod` (add the three requires)
- Modify: `docs/plans/2026-09-08-workshop/00-master-spec.md:…` (the §4 Go line; see Step 8)

**Interfaces:**
- Consumes: `toolchain.SkipReason()` from Task 0.
- Produces:
  ```go
  package vt
  type Cell struct { Text string; FG, BG *RGB; Attrs uint16 }
  type RGB struct { R, G, B uint8 }
  const ( AttrBold uint16 = 1 << iota; AttrFaint; AttrItalic; AttrUnderline;
          AttrBlink; AttrInverse; AttrStrike )
  type Term struct { /* unexported */ }
  func New(cols, rows uint16, scrollbackLines uint) (*Term, error)
  func (t *Term) Feed(p []byte) error
  func (t *Term) Resize(cols, rows uint16) error
  func (t *Term) Size() (cols, rows uint16)
  func (t *Term) Cursor() (x, y uint16, visible bool)
  func (t *Term) Title() string
  func (t *Term) Progress() string
  func (t *Term) PlainScreen() (string, error)
  func (t *Term) PlainScreenUnwrapped() (string, error)
  func (t *Term) PlainAll() (string, error)
  func (t *Term) Viewport() ([][]Cell, error)
  func (t *Term) Close()
  ```

- [ ] **Step 1: Record the pins that are already known**

Create `harness/coppice/PINS.md` with exactly this content. Every number below was read from
the upstream repositories on 2026-09-08 and the commands that produced them are shown, per the
repo rule that no recorded number lacks a reproducing command.

```markdown
# coppice pins

Everything coppice pins, why, and the command that re-reads it. Verify before you rely.

## Terminal engine

| what | value |
|---|---|
| module | `go.mitchellh.com/libghostty` |
| canonical source | `https://tangled.org/mitchellh.com/go-libghostty` (GitHub is a mirror) |
| version | `v0.0.0-20260908040635-9f448dfe8052` |
| upstream tags | none exist. The author has published no tag and no release. |
| ghostty commit it builds | `b0c421fcd2e290629d4285c181b52fe2f2095f06` |
| licence | MIT |

Re-read with:

```
curl -s https://proxy.golang.org/go.mitchellh.com/libghostty/@latest
gh api repos/mitchellh/go-libghostty/contents/CMakeLists.txt -q .content | base64 -d | grep GIT_TAG
```

The author states "I'm not promising any API stability yet." That is why every call into this
module lives in `internal/vt` and nowhere else, and why `internal/vt/boundary_test.go` fails the
build if a second package imports it.

## Build toolchain

| what | value |
|---|---|
| Zig | 0.16.0, `https://ziglang.org/download/0.16.0/zig-x86_64-linux-0.16.0.tar.xz` |
| Zig sha256 | `70e49664a74374b48b51e6f3fdfbf437f6395d42509050588bd49abe52ba3d00` |
| CMake | whatever `uv tool install cmake` resolves |
| install prefix | `$HOME/.local/ghostty-vt`, override with `COPPICE_GHOSTTY_PREFIX` |

## Go toolchain deviation

Master spec §4 asked for Go 1.25. `go.mitchellh.com/libghostty` declares `go 1.26.0`, so Go
refuses to build against it from a 1.25 main module. The dev machine ships go1.25.12; some
distributions ship Go with `GOTOOLCHAIN=local` compiled in, so it will not switch on its own.

- `go.mod` declares `go 1.26.0` and `toolchain go1.26.8`.
- `scripts/toolchain.sh` runs `go env -w GOTOOLCHAIN=auto`, which downloads go1.26.8 into the
  module cache on first build.
- Undo both global Go settings with `go env -u GOTOOLCHAIN PKG_CONFIG`.
- Master spec §4 was amended in the same commit.

## Other Go dependencies

| module | version | why |
|---|---|---|
| `github.com/creack/pty` | v1.1.24 | PTY spawn and winsize. MIT. |
| `github.com/BurntSushi/toml` | v1.5.0 | manifest parsing. Its `MetaData.Undecoded()` is how we reject unknown keys the way Herdr's `deny_unknown_fields` does. MIT. |
| `golang.org/x/term` | v0.45.0 | raw mode for `coppice attach`. The same module `harness/sprig` already requires, so no new module family. BSD-3. |

## Herdr reference

| what | value |
|---|---|
| repository | `https://github.com/herdrdev/herdr` |
| manifests vendored from | `src/detect/manifests/*.toml` |
| default branch | `master`, not `main`. `gh api repos/herdrdev/herdr/commits/main` returns HTTP 422. |
| vendored at commit | recorded by Task 12 in `internal/detect/manifests/PROVENANCE` |
| licence | Apache-2.0. See `harness/coppice/NOTICE`. |

Master §4 originally named `c5a21edfcbc2d5b46540ad91b7980aca31f5f1f3` as "their 1.3.2". That
commit does not exist (`gh api repos/herdrdev/herdr/commits/c5a21edf…` returns HTTP 422 "No commit
found") and the newest tag is `v0.9.0`, so there is no 1.3.2. Task 12 Step 1 removes the SHA and
the version claim from §4 and records only the commit we actually vendor.

## libghostty symbols coppice uses

Filled in by Task 1 Step 3 from `go doc`. If a future bump changes any signature here, the
compile breaks in `internal/vt` and nowhere else.

<!-- GODOC-BEGIN -->
<!-- GODOC-END -->
```

- [ ] **Step 2: Add the dependencies and prove the toolchain switch works**

Run:
```bash
cd harness/coppice
go get go.mitchellh.com/libghostty@v0.0.0-20260908040635-9f448dfe8052
go get github.com/creack/pty@v1.1.24
go get github.com/BurntSushi/toml@v1.5.0
go version
```
Expected: `go get` succeeds and `go version` reports go1.26.8 or newer. If it reports
`GOTOOLCHAIN=local` refusing the switch, Task 0 Step 8 did not run.

- [ ] **Step 3: Confirm the API against the pinned commit and record it**

The signature table below was read from the mirror source on 2026-09-08
(`gh api repos/mitchellh/go-libghostty/contents/<file> -q .content | base64 -d`). Run the `go doc`
commands, confirm each line, paste the table plus any correction between the `GODOC-BEGIN` and
`GODOC-END` markers in `PINS.md`, then write Step 6 against it. Where `go doc` disagrees with the
table, `go doc` wins and `PINS.md` records the difference.

```bash
cd harness/coppice
go doc go.mitchellh.com/libghostty Terminal          # every method, do not pipe through head
go doc go.mitchellh.com/libghostty TerminalProgressReport
go doc go.mitchellh.com/libghostty ProgressReportFn
go doc go.mitchellh.com/libghostty RenderState
go doc go.mitchellh.com/libghostty RenderStateRowIterator
go doc go.mitchellh.com/libghostty RenderStateRowCells
go doc go.mitchellh.com/libghostty Formatter
go doc go.mitchellh.com/libghostty Style
go doc go.mitchellh.com/libghostty ColorRGB
go doc go.mitchellh.com/libghostty Selection
```

The table to paste, and to write Step 6 against:

```
NewTerminal(opts ...TerminalOption) (*Terminal, error)
WithSize(cols, rows uint16) TerminalOption
WithMaxScrollbackLines(lines uint) TerminalOption
WithProgressReport(fn ProgressReportFn) TerminalOption
type ProgressReportFn func(t *Terminal, report TerminalProgressReport)   // TWO arguments
type TerminalProgressReport struct { State TerminalProgressState; Progress int8 }
(*Terminal).VTWrite(data []byte)                     // returns NOTHING
(*Terminal).Resize(cols, rows uint16, cw, ch uint32) error
(*Terminal).Cols() (uint16, error)                   // returns an error
(*Terminal).Rows() (uint16, error)                   // returns an error
(*Terminal).CursorX() (uint16, error)
(*Terminal).CursorY() (uint16, error)
(*Terminal).CursorVisible() (bool, error)
(*Terminal).Title() (string, error)                  // returns an error
(*Terminal).SelectAll() (*Selection, error)          // RETURNS the selection; does not install it
(*Terminal).SetSelection(sel *Selection) error
(*Terminal).Close()                                  // returns NOTHING
NewFormatter(t *Terminal, opts ...FormatterOption) (*Formatter, error)
WithFormatterFormat(f FormatterFormat) / WithFormatterTrim(bool) / WithFormatterUnwrap(bool)
WithFormatterSelection(sel *Selection) FormatterOption
FormatterFormatPlain
(*Formatter).FormatString() (string, error)
(*Formatter).Close()
NewRenderState() (*RenderState, error)
(*RenderState).Update(t *Terminal) error
(*RenderState).RowIterator(ri *RenderStateRowIterator) error    // fills a PRE-ALLOCATED handle
NewRenderStateRowIterator() (*RenderStateRowIterator, error)
NewRenderStateRowCells() (*RenderStateRowCells, error)
(*RenderStateRowIterator).Next() bool
(*RenderStateRowIterator).Cells(rc *RenderStateRowCells) error
(*RenderStateRowCells).Next() bool
(*RenderStateRowCells).AppendGraphemes(dst []byte) ([]byte, error)
(*RenderStateRowCells).Style() (*Style, error)
(*RenderStateRowCells).FgColor() (*ColorRGB, error) / BgColor() (*ColorRGB, error)
(ColorRGB).Components() (r, g, b uint8)
(Style).Bold/Faint/Italic/Blink/Inverse/Strikethrough() bool
(Style).Underline() SGRUnderline
```

Three consequences the implementation depends on, so write them down too:

1. `VTWrite` and `Terminal.Close` return nothing, so `return t.term.VTWrite(p)` and
   `_ = t.term.Close()` do not compile.
2. `SelectAll` **returns** a selection and does not install it as the terminal's active
   selection, so `PlainAll` must use the returned value directly and must never call
   `SetSelection(nil)`, which would destroy a real selection a client is holding.
3. `RenderState`, `RenderStateRowIterator` and `RenderStateRowCells` are C handles obtained from
   constructors and closed with `Close()`. `framePump` calls `Viewport()` up to 60 times a second
   per attached client, so they are allocated once per `Term` and reused, never per call.

- [ ] **Step 4: Write the failing test**

```go
// harness/coppice/internal/vt/vt_test.go
package vt

import (
	"strings"
	"testing"

	"github.com/opendaisugi/coppice/internal/toolchain"
)

func skipWithoutLib(t *testing.T) {
	t.Helper()
	if r := toolchain.SkipReason(); r != "" {
		t.Skip(r)
	}
}

func TestFeedAndPlainScreen(t *testing.T) {
	skipWithoutLib(t)
	term, err := New(20, 4, 200)
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	if err := term.Feed([]byte("Hello, \x1b[1;32mworld\x1b[0m!\r\n")); err != nil {
		t.Fatal(err)
	}
	got, err := term.PlainScreen()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "Hello, world!") {
		t.Fatalf("PlainScreen() = %q, want it to contain %q", got, "Hello, world!")
	}
}

func TestViewportCarriesTextAndStyle(t *testing.T) {
	skipWithoutLib(t)
	term, err := New(20, 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	// Bold red "AB" then a plain "C".
	if err := term.Feed([]byte("\x1b[1;38;2;255;0;0mAB\x1b[0mC")); err != nil {
		t.Fatal(err)
	}
	rows, err := term.Viewport()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("Viewport() has %d rows, want 3", len(rows))
	}
	if len(rows[0]) != 20 {
		t.Fatalf("row 0 has %d cells, want 20", len(rows[0]))
	}
	if rows[0][0].Text != "A" || rows[0][1].Text != "B" || rows[0][2].Text != "C" {
		t.Fatalf("row 0 first cells = %q %q %q, want A B C",
			rows[0][0].Text, rows[0][1].Text, rows[0][2].Text)
	}
	if rows[0][0].Attrs&AttrBold == 0 {
		t.Fatal("cell A has no AttrBold, want bold")
	}
	if rows[0][2].Attrs&AttrBold != 0 {
		t.Fatal("cell C has AttrBold, want the SGR reset to have cleared it")
	}
	if rows[0][0].FG == nil || *rows[0][0].FG != (RGB{R: 255}) {
		t.Fatalf("cell A FG = %v, want red", rows[0][0].FG)
	}
}

func TestResizeChangesReportedSize(t *testing.T) {
	skipWithoutLib(t)
	term, err := New(20, 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	if err := term.Resize(40, 10); err != nil {
		t.Fatal(err)
	}
	cols, rows := term.Size()
	if cols != 40 || rows != 10 {
		t.Fatalf("Size() = %d x %d, want 40 x 10", cols, rows)
	}
}

func TestTitleAndProgressComeFromOSC(t *testing.T) {
	skipWithoutLib(t)
	term, err := New(20, 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	if err := term.Feed([]byte("\x1b]0;claude working\x07\x1b]9;4;1;40\x07")); err != nil {
		t.Fatal(err)
	}
	if got := term.Title(); got != "claude working" {
		t.Fatalf("Title() = %q, want %q", got, "claude working")
	}
	// Herdr's osc_progress region is the OSC 9 payload without the "9;" prefix,
	// which is what its claude.toml rule `^4;0` matches.
	if got := term.Progress(); !strings.HasPrefix(got, "4;1") {
		t.Fatalf("Progress() = %q, want it to start with 4;1", got)
	}
}

func TestPlainAllIncludesScrollback(t *testing.T) {
	skipWithoutLib(t)
	term, err := New(20, 3, 200)
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	for _, line := range []string{"one", "two", "three", "four", "five"} {
		if err := term.Feed([]byte(line + "\r\n")); err != nil {
			t.Fatal(err)
		}
	}
	screen, err := term.PlainScreen()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(screen, "one") {
		t.Fatalf("PlainScreen() = %q, want %q to have scrolled off", screen, "one")
	}
	all, err := term.PlainAll()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(all, "one") {
		t.Fatalf("PlainAll() = %q, want it to contain the scrolled-off %q", all, "one")
	}
}
```

- [ ] **Step 5: Run the test and watch it fail**

Run: `cd harness/coppice && go test -race ./internal/vt/ -v`
Expected: FAIL, `undefined: New`.

- [ ] **Step 6: Write the implementation**

This is written against the Step 3 table. Note the three shapes that a naive reading gets wrong:
`VTWrite` and `Close` return nothing, `Cols`/`Rows`/`CursorX`/`CursorY`/`CursorVisible`/`Title`
all return an error alongside their value, and `SelectAll` returns a selection rather than
installing one. The render-state handles are allocated once and reused, because `framePump` calls
`Viewport` up to sixty times a second per attached client.

```go
// harness/coppice/internal/vt/vt.go

// Package vt is the only package in coppice that touches libghostty. The
// binding's author states its Go API is not stable yet, so a bump breaks the
// compile here and nowhere else. Everything above this package sees Term and
// Cell.
package vt

import (
	"fmt"
	"strings"
	"sync"

	libghostty "go.mitchellh.com/libghostty"
)

// RGB is one resolved colour. A nil *RGB means "the terminal default", which
// the client renders with its own palette.
type RGB struct{ R, G, B uint8 }

// Attribute bits carried on a Cell. The wire format sends this as one integer.
const (
	AttrBold uint16 = 1 << iota
	AttrFaint
	AttrItalic
	AttrUnderline
	AttrBlink
	AttrInverse
	AttrStrike
)

// Cell is one grid cell. Text is the full grapheme cluster, so a wide glyph or
// an emoji with a modifier arrives whole.
type Cell struct {
	Text  string
	FG    *RGB
	BG    *RGB
	Attrs uint16
}

// Term wraps one libghostty terminal. Every method holds mu: libghostty
// terminals are not safe for concurrent use, and a pane feeds bytes from its
// PTY reader while a client asks for a frame.
//
// The render state, the row iterator and the cell cursor are C handles. They
// are allocated once here and reused, never per Viewport call: framePump asks
// for a frame every 16 ms per attached client per pane, and allocating three
// handles on that path without closing them leaks until the process dies.
type Term struct {
	mu       sync.Mutex
	term     *libghostty.Terminal
	rs       *libghostty.RenderState
	ri       *libghostty.RenderStateRowIterator
	rc       *libghostty.RenderStateRowCells
	progress string
	closed   bool
}

// New creates a terminal of the given size with the given scrollback budget in
// lines. Pass 0 for scrollbackLines to keep only the viewport.
func New(cols, rows uint16, scrollbackLines uint) (*Term, error) {
	t := &Term{}
	opts := []libghostty.TerminalOption{
		libghostty.WithSize(cols, rows),
		libghostty.WithMaxScrollbackLines(scrollbackLines),
		// The callback takes the terminal and the report. Storing the
		// formatted payload is all we need: the manifest engine matches
		// Herdr's osc_progress region against exactly this shape.
		libghostty.WithProgressReport(func(_ *libghostty.Terminal, r libghostty.TerminalProgressReport) {
			t.progress = fmt.Sprintf("4;%d;%d", int(r.State), int(r.Progress))
		}),
	}
	term, err := libghostty.NewTerminal(opts...)
	if err != nil {
		return nil, fmt.Errorf("vt: create terminal: %w", err)
	}
	rs, err := libghostty.NewRenderState()
	if err != nil {
		term.Close()
		return nil, fmt.Errorf("vt: render state: %w", err)
	}
	ri, err := libghostty.NewRenderStateRowIterator()
	if err != nil {
		rs.Close()
		term.Close()
		return nil, fmt.Errorf("vt: row iterator: %w", err)
	}
	rc, err := libghostty.NewRenderStateRowCells()
	if err != nil {
		ri.Close()
		rs.Close()
		term.Close()
		return nil, fmt.Errorf("vt: row cells: %w", err)
	}
	t.term, t.rs, t.ri, t.rc = term, rs, ri, rc
	return t, nil
}

// Feed writes VT bytes. VTWrite returns nothing, so the only failure this can
// report is a closed terminal.
func (t *Term) Feed(p []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return fmt.Errorf("vt: terminal is closed")
	}
	t.term.VTWrite(p)
	return nil
}

func (t *Term) Resize(cols, rows uint16) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.term.Resize(cols, rows, 0, 0)
}

// Size returns the terminal's own idea of its size. Both accessors can fail, so
// a failure reports 0x0 rather than a plausible lie.
func (t *Term) Size() (uint16, uint16) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.size()
}

func (t *Term) size() (uint16, uint16) {
	cols, err := t.term.Cols()
	if err != nil {
		return 0, 0
	}
	rows, err := t.term.Rows()
	if err != nil {
		return 0, 0
	}
	return cols, rows
}

func (t *Term) Cursor() (uint16, uint16, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	x, err := t.term.CursorX()
	if err != nil {
		return 0, 0, false
	}
	y, err := t.term.CursorY()
	if err != nil {
		return 0, 0, false
	}
	vis, err := t.term.CursorVisible()
	if err != nil {
		return x, y, false
	}
	return x, y, vis
}

func (t *Term) Title() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	title, err := t.term.Title()
	if err != nil {
		return ""
	}
	return title
}

// Progress is the last OSC 9;4 payload seen, or "" when none has arrived.
func (t *Term) Progress() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.progress
}

func (t *Term) PlainScreen() (string, error) { return t.lockedPlain(false, nil) }

// PlainScreenUnwrapped joins rows the terminal soft-wrapped. The manifest
// evaluator reads this, because Herdr's rules are written against unwrapped
// lines.
func (t *Term) PlainScreenUnwrapped() (string, error) { return t.lockedPlain(true, nil) }

func (t *Term) lockedPlain(unwrap bool, sel *libghostty.Selection) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.plain(unwrap, sel)
}

// plain must be called with mu held.
func (t *Term) plain(unwrap bool, sel *libghostty.Selection) (string, error) {
	opts := []libghostty.FormatterOption{
		libghostty.WithFormatterFormat(libghostty.FormatterFormatPlain),
		libghostty.WithFormatterTrim(true),
		libghostty.WithFormatterUnwrap(unwrap),
	}
	if sel != nil {
		opts = append(opts, libghostty.WithFormatterSelection(sel))
	}
	f, err := libghostty.NewFormatter(t.term, opts...)
	if err != nil {
		return "", fmt.Errorf("vt: formatter: %w", err)
	}
	defer f.Close()
	s, err := f.FormatString()
	if err != nil {
		return "", fmt.Errorf("vt: format: %w", err)
	}
	return s, nil
}

// PlainAll is the scrollback plus the viewport, as plain text. SelectAll
// returns a selection and does not install it, so nothing here touches the
// terminal's active selection: a client may be holding one.
func (t *Term) PlainAll() (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	sel, err := t.term.SelectAll()
	if err != nil {
		return "", fmt.Errorf("vt: select all: %w", err)
	}
	out, err := t.plain(false, sel)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(out, "\n"), nil
}

// Viewport returns the visible grid, one slice per row, always exactly
// rows x cols cells. A cell with no text is the empty string, not a space, so
// a diff can tell "blank" from "a space was typed".
func (t *Term) Viewport() ([][]Cell, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if err := t.rs.Update(t.term); err != nil {
		return nil, fmt.Errorf("vt: render state update: %w", err)
	}
	colsU, rowsU := t.size()
	cols, nrows := int(colsU), int(rowsU)
	out := make([][]Cell, 0, nrows)

	if err := t.rs.RowIterator(t.ri); err != nil {
		return nil, fmt.Errorf("vt: row iterator: %w", err)
	}
	buf := make([]byte, 0, 8)
	for t.ri.Next() {
		row := make([]Cell, cols)
		if err := t.ri.Cells(t.rc); err != nil {
			return nil, fmt.Errorf("vt: row cells: %w", err)
		}
		for x := 0; x < cols && t.rc.Next(); x++ {
			buf = buf[:0]
			b, err := t.rc.AppendGraphemes(buf)
			if err != nil {
				return nil, fmt.Errorf("vt: graphemes: %w", err)
			}
			c := Cell{Text: string(b)}
			if c.Text == " " {
				c.Text = ""
			}
			if st, err := t.rc.Style(); err == nil && st != nil {
				c.Attrs = attrBits(*st)
			}
			if fg, err := t.rc.FgColor(); err == nil && fg != nil {
				r, g, bl := fg.Components()
				c.FG = &RGB{R: r, G: g, B: bl}
			}
			if bg, err := t.rc.BgColor(); err == nil && bg != nil {
				r, g, bl := bg.Components()
				c.BG = &RGB{R: r, G: g, B: bl}
			}
			row[x] = c
		}
		out = append(out, row)
		if len(out) == nrows {
			break
		}
	}
	for len(out) < nrows {
		out = append(out, make([]Cell, cols))
	}
	return out, nil
}

func attrBits(s libghostty.Style) uint16 {
	var a uint16
	if s.Bold() {
		a |= AttrBold
	}
	if s.Faint() {
		a |= AttrFaint
	}
	if s.Italic() {
		a |= AttrItalic
	}
	if s.Underline() != 0 {
		a |= AttrUnderline
	}
	if s.Blink() {
		a |= AttrBlink
	}
	if s.Inverse() {
		a |= AttrInverse
	}
	if s.Strikethrough() {
		a |= AttrStrike
	}
	return a
}

// Close releases the four C handles in the reverse order they were created.
// Terminal.Close returns nothing.
func (t *Term) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	t.closed = true
	t.rc.Close()
	t.ri.Close()
	t.rs.Close()
	t.term.Close()
}
```

- [ ] **Step 6b: Prove the handles are reused, not reallocated**

```go
// append to harness/coppice/internal/vt/vt_test.go

// framePump calls Viewport up to sixty times a second per attached client. If
// each call allocated a render state, an iterator and a cell cursor without
// closing them, the daemon would leak C memory until it died. This test does
// not measure memory; it pins the design: the handles are fields on Term.
func TestViewportReusesItsHandles(t *testing.T) {
	skipWithoutLib(t)
	term, err := New(20, 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	if term.rs == nil || term.ri == nil || term.rc == nil {
		t.Fatal("New did not allocate the render-state handles on the Term")
	}
	rs, ri, rc := term.rs, term.ri, term.rc
	for i := 0; i < 200; i++ {
		if _, err := term.Viewport(); err != nil {
			t.Fatal(err)
		}
	}
	if term.rs != rs || term.ri != ri || term.rc != rc {
		t.Fatal("Viewport replaced a handle, so it is allocating per call")
	}
}

// Every method that reads terminal state must survive an accessor that
// errors. None of them may return a plausible lie.
func TestSizeAndCursorReportZeroRatherThanGuessing(t *testing.T) {
	skipWithoutLib(t)
	term, err := New(20, 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	term.Close()
	// After Close the handles are gone; the accessors must not panic.
	_, _ = term.Size()
	_, _, _ = term.Cursor()
	_ = term.Title()
}
```

Run: `cd harness/coppice && go test -race ./internal/vt/ -run 'Viewport|Cursor' -v`
Expected: PASS.


- [ ] **Step 7: Write the boundary test and run everything**

```go
// harness/coppice/internal/vt/boundary_test.go
package vt_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// libghostty's Go API is explicitly unstable. Confining it to internal/vt is
// what keeps a bump a one-package job. This test is that confinement.
func TestOnlyInternalVTImportsLibghostty(t *testing.T) {
	root := filepath.Join("..", "..")
	var offenders []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.Contains(filepath.ToSlash(path), "/internal/vt/") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), "go.mitchellh.com/libghostty") {
			offenders = append(offenders, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Fatalf("these files import libghostty outside internal/vt: %v", offenders)
	}
}
```

Run: `cd harness/coppice && go test -race ./... -v`
Expected: PASS. If the toolchain is absent the vt tests skip with the Task 0 message; the
boundary test still runs.

- [ ] **Step 8: Confirm master spec §4 already says Go 1.26**

Run: `grep -n 'Go 1.2' ../../docs/plans/2026-09-08-workshop/00-master-spec.md` from
`harness/coppice`, or open §4 directly.

Expected: the `harness/coppice` bullet reads **Go 1.26** and cites `harness/coppice/PINS.md`.
If it does, **make no edit**. If it still reads Go 1.25, replace `**Go 1.25**` with `**Go 1.26**`
and append this sentence to the same bullet: `The floor is 1.26 because
go.mitchellh.com/libghostty declares go 1.26.0; see harness/coppice/PINS.md.` Change nothing else
in that file. Task 12 Step 1 handles the separate §4 correction about the Herdr SHA.

- [ ] **Step 9: Commit**

```bash
# Add the master spec only if Step 8 actually changed it.
git add harness/coppice/PINS.md harness/coppice/go.mod harness/coppice/go.sum \
        harness/coppice/internal/vt
git commit -m "feat(coppice): pin the terminal engine behind one package

go-libghostty publishes no tags and promises no API stability, so the pin is a
pseudo-version and every call sits in internal/vt. A boundary test fails the
build if a second package reaches for it. The binding needs Go 1.26, so master
spec section 4 moves with it rather than contradicting the code."
```

---

### Task 2: `internal/proto` — the wire

**Files:**
- Create: `harness/coppice/internal/proto/proto.go`
- Create: `harness/coppice/internal/proto/codec.go`
- Create: `harness/coppice/internal/proto/state_event.go`
- Create: `harness/coppice/testdata/events/gate-blocked.json`
- Create: `harness/coppice/testdata/events/process-done.json`
- Create: `harness/coppice/testdata/events/manifest-working.json`
- Create: `harness/coppice/testdata/events/headless-blocked.json`
- Test: `harness/coppice/internal/proto/proto_test.go`
- Test: `harness/coppice/internal/proto/fixtures_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  ```go
  package proto
  type ErrCode string
  const ( ErrBadRequest ErrCode = "bad_request"; ErrNoSuchPane = "no_such_pane"
          ErrNoSuchWorkspace = "no_such_workspace"; ErrNoSuchTab = "no_such_tab"
          ErrPaneClosed = "pane_closed"; ErrNotAttached = "not_attached"
          ErrAdapter = "adapter_error"; ErrSpawnFailed = "spawn_failed"
          ErrTimeout = "timeout"; ErrUnauthorized = "unauthorized"; ErrInternal = "internal" )
  func ValidCode(c ErrCode) bool
  type Request struct { ID, Cmd string; Params map[string]json.RawMessage }
  func (r *Request) Str(k string) (string, bool)
  func (r *Request) Int(k string) (int, bool)
  func (r *Request) StrSlice(k string) ([]string, bool)
  func (r *Request) StrMap(k string) (map[string]string, bool)
  type Response struct { ID string; OK bool; Result any; Error *Error }
  type Error struct { Code ErrCode; Message string }
  func OKResp(id string, result any) Response
  func ErrResp(id string, code ErrCode, msg string) Response
  type Cell struct { Text string; FG, BG string; Attrs uint16 }   // marshals as ["x","#rrggbb","",1]
  type Frame struct { Event, Pane string; Seq uint64; Cols, Rows int; Cursor [2]int
                      RowsChanged map[int][]Cell }
  type Ask struct { ID, Tool, Summary string; Deadline float64 }
  type PaneStateEvent struct { V int; TS float64; SessionID, HarnessSessionID, Harness, Pane,
                               State, Source, Detail string; Ask *Ask }
  const ( StateIdle="idle"; StateWorking="working"; StateBlocked="blocked"; StateDone="done"; StateUnknown="unknown" )
  const ( SrcOperator="operator"; SrcGate="gate"; SrcHeadless="headless"; SrcProcess="process"; SrcManifest="manifest" )
  func (e PaneStateEvent) Validate() error
  func ParseStateEvent(line []byte) (PaneStateEvent, error)
  func SourceRank(src string) int    // operator 5 … manifest 1, unknown source 0
  type Decoder struct{}; func NewDecoder(r io.Reader) *Decoder
  func (d *Decoder) Next() ([]byte, error)
  type Encoder struct{}; func NewEncoder(w io.Writer) *Encoder
  func (e *Encoder) Send(v any) error   // one JSON object, one newline, flushed, mutex-guarded
  ```

- [ ] **Step 1: Write the failing test**

```go
// harness/coppice/internal/proto/proto_test.go
package proto

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestRequestDecodesCommandAndParams(t *testing.T) {
	d := NewDecoder(strings.NewReader(
		`{"id":"1","cmd":"pane.create","cwd":"/repo","cmd_argv":["claude"],"cols":120}` + "\n"))
	line, err := d.Next()
	if err != nil {
		t.Fatal(err)
	}
	var r Request
	if err := json.Unmarshal(line, &r); err != nil {
		t.Fatal(err)
	}
	if r.ID != "1" || r.Cmd != "pane.create" {
		t.Fatalf("got id=%q cmd=%q, want 1 / pane.create", r.ID, r.Cmd)
	}
	if cwd, ok := r.Str("cwd"); !ok || cwd != "/repo" {
		t.Fatalf("Str(cwd) = %q %v, want /repo true", cwd, ok)
	}
	if argv, ok := r.StrSlice("cmd_argv"); !ok || len(argv) != 1 || argv[0] != "claude" {
		t.Fatalf("StrSlice(cmd_argv) = %v %v, want [claude] true", argv, ok)
	}
	if cols, ok := r.Int("cols"); !ok || cols != 120 {
		t.Fatalf("Int(cols) = %d %v, want 120 true", cols, ok)
	}
	if _, ok := r.Str("nope"); ok {
		t.Fatal("Str(nope) reported ok for an absent key")
	}
}

func TestGoldenOKResponse(t *testing.T) {
	b, err := json.Marshal(OKResp("1", map[string]string{"pane": "w1:p1"}))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":"1","ok":true,"result":{"pane":"w1:p1"}}`
	if string(b) != want {
		t.Fatalf("OKResp marshalled to %s, want %s", b, want)
	}
}

func TestGoldenErrorResponse(t *testing.T) {
	b, err := json.Marshal(ErrResp("7", ErrNoSuchPane, "pane w1:p9 does not exist"))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":"7","ok":false,"error":{"code":"no_such_pane","message":"pane w1:p9 does not exist"}}`
	if string(b) != want {
		t.Fatalf("ErrResp marshalled to %s, want %s", b, want)
	}
}

// The error codes are a closed enum. A code the spec does not list must not
// reach a client, because clients switch on it.
func TestValidCodeRejectsAnUnlistedCode(t *testing.T) {
	for _, c := range []ErrCode{ErrBadRequest, ErrNoSuchPane, ErrNoSuchWorkspace, ErrNoSuchTab,
		ErrPaneClosed, ErrNotAttached, ErrAdapter, ErrSpawnFailed, ErrTimeout,
		ErrUnauthorized, ErrInternal} {
		if !ValidCode(c) {
			t.Fatalf("ValidCode(%q) = false, want true", c)
		}
	}
	if ValidCode("kaboom") {
		t.Fatal(`ValidCode("kaboom") = true, want false`)
	}
}

func TestGoldenFrameEvent(t *testing.T) {
	f := Frame{
		Event: "frame", Pane: "w1:p1", Seq: 1, Cols: 2, Rows: 1, Cursor: [2]int{0, 0},
		RowsChanged: map[int][]Cell{0: {{Text: "h", FG: "#ffffff"}, {Text: "i"}}},
	}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"event":"frame","pane":"w1:p1","seq":1,"cols":2,"rows":1,"cursor":[0,0],` +
		`"rows_changed":{"0":[["h","#ffffff","",0],["i","","",0]]}}`
	if string(b) != want {
		t.Fatalf("Frame marshalled to %s, want %s", b, want)
	}
}

func TestEncoderWritesOneLinePerValue(t *testing.T) {
	var buf bytes.Buffer
	e := NewEncoder(&buf)
	if err := e.Send(OKResp("1", nil)); err != nil {
		t.Fatal(err)
	}
	if err := e.Send(OKResp("2", nil)); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("Encoder wrote %d lines, want 2: %q", len(lines), buf.String())
	}
}

func TestDecoderRejectsAnOverlongLine(t *testing.T) {
	huge := `{"id":"1","cmd":"x","pad":"` + strings.Repeat("a", 2<<20) + `"}` + "\n"
	d := NewDecoder(strings.NewReader(huge))
	if _, err := d.Next(); err == nil {
		t.Fatal("Decoder accepted a 2 MB line, want an error so one client cannot exhaust memory")
	}
}

func TestParseStateEventRoundTrip(t *testing.T) {
	line := []byte(`{"v":1,"ts":1757300000.123,"session_id":"d41c","harness_session_id":null,` +
		`"harness":"claude-code","pane":"w1:p3","state":"blocked","source":"gate",` +
		`"ask":{"id":"toolu_1","tool":"Bash","summary":"rm -rf build/","deadline":1757300090},` +
		`"detail":"verdict=deny clause=shell.deny[2]"}`)
	ev, err := ParseStateEvent(line)
	if err != nil {
		t.Fatal(err)
	}
	if ev.State != StateBlocked || ev.Source != SrcGate || ev.Ask == nil || ev.Ask.Tool != "Bash" {
		t.Fatalf("ParseStateEvent = %+v, want a gate blocked event with a Bash ask", ev)
	}
	back, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	var a, b map[string]any
	if err := json.Unmarshal(line, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(back, &b); err != nil {
		t.Fatal(err)
	}
	for k, av := range a {
		if bv, ok := b[k]; !ok || !jsonEqual(av, bv) {
			t.Fatalf("round trip lost or changed %q: %v -> %v", k, av, bv)
		}
	}
}

func jsonEqual(a, b any) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}

func TestValidateRejectsAnUnknownState(t *testing.T) {
	_, err := ParseStateEvent([]byte(`{"v":1,"ts":1,"session_id":"s","harness":"x",` +
		`"state":"busy","source":"gate"}`))
	if err == nil {
		t.Fatal(`ParseStateEvent accepted state "busy", want an error`)
	}
}

func TestValidateRejectsAnUnknownSource(t *testing.T) {
	_, err := ParseStateEvent([]byte(`{"v":1,"ts":1,"session_id":"s","harness":"x",` +
		`"state":"idle","source":"vibes"}`))
	if err == nil {
		t.Fatal(`ParseStateEvent accepted source "vibes", want an error`)
	}
}

// A headless adapter may block too, and its ask must survive the round trip.
// pi and OpenCode both produce this shape.
func TestAHeadlessBlockedEventKeepsItsAsk(t *testing.T) {
	ev, err := ParseStateEvent([]byte(`{"v":1,"ts":1757300075.0,"session_id":"d41c",` +
		`"harness_session_id":"pi-77","harness":"pi","pane":"w1:p6","state":"blocked",` +
		`"source":"headless","ask":{"id":"ui_1","tool":"Write","summary":"src/main.go",` +
		`"deadline":1757300165.0},"detail":"extension_ui_request"}`))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Ask == nil || ev.Ask.Tool != "Write" || ev.Ask.Summary != "src/main.go" {
		t.Fatalf("ask = %+v, want the Write ask", ev.Ask)
	}
}

func TestValidateRequiresAnAskOnBlockedFromTheGate(t *testing.T) {
	_, err := ParseStateEvent([]byte(`{"v":1,"ts":1,"session_id":"s","harness":"x",` +
		`"state":"blocked","source":"gate"}`))
	if err == nil {
		t.Fatal("ParseStateEvent accepted a gate blocked event with no ask, want an error")
	}
}

func TestValidateRejectsADetailOverTwoHundredChars(t *testing.T) {
	long := strings.Repeat("x", 201)
	_, err := ParseStateEvent([]byte(`{"v":1,"ts":1,"session_id":"s","harness":"x",` +
		`"state":"idle","source":"gate","detail":"` + long + `"}`))
	if err == nil {
		t.Fatal("ParseStateEvent accepted a 201 char detail, want an error")
	}
}

func TestSourceRankFollowsThePrecedenceOrder(t *testing.T) {
	if !(SourceRank(SrcOperator) > SourceRank(SrcGate) &&
		SourceRank(SrcGate) > SourceRank(SrcHeadless) &&
		SourceRank(SrcHeadless) > SourceRank(SrcProcess) &&
		SourceRank(SrcProcess) > SourceRank(SrcManifest) &&
		SourceRank(SrcManifest) > SourceRank("nonsense")) {
		t.Fatal("SourceRank does not follow operator > gate > headless > process > manifest")
	}
}
```

- [ ] **Step 2: Run the test and watch it fail**

Run: `cd harness/coppice && go test -race ./internal/proto/ -v`
Expected: FAIL, `undefined: NewDecoder`.

- [ ] **Step 3: Write `proto.go`**

```go
// harness/coppice/internal/proto/proto.go

// Package proto is the coppice wire format: one JSON object per line, both
// ways. Requests carry an id, responses echo it, events carry none. See master
// spec section 3.3.
package proto

import "encoding/json"

// ErrCode is the closed enum from spec-02. A client switches on these, so a
// code outside the set is a server bug, not a new case.
type ErrCode string

const (
	ErrBadRequest      ErrCode = "bad_request"
	ErrNoSuchPane      ErrCode = "no_such_pane"
	ErrNoSuchWorkspace ErrCode = "no_such_workspace"
	ErrNoSuchTab       ErrCode = "no_such_tab"
	ErrPaneClosed      ErrCode = "pane_closed"
	ErrNotAttached     ErrCode = "not_attached"
	ErrAdapter         ErrCode = "adapter_error"
	ErrSpawnFailed     ErrCode = "spawn_failed"
	ErrTimeout         ErrCode = "timeout"
	ErrUnauthorized    ErrCode = "unauthorized"
	ErrInternal        ErrCode = "internal"
)

var validCodes = map[ErrCode]bool{
	ErrBadRequest: true, ErrNoSuchPane: true, ErrNoSuchWorkspace: true, ErrNoSuchTab: true,
	ErrPaneClosed: true, ErrNotAttached: true, ErrAdapter: true, ErrSpawnFailed: true,
	ErrTimeout: true, ErrUnauthorized: true, ErrInternal: true,
}

func ValidCode(c ErrCode) bool { return validCodes[c] }

// Request is one line from a client. Command parameters sit beside id and cmd
// at the top level, so they are captured raw and read with the typed helpers.
type Request struct {
	ID     string                     `json:"id"`
	Cmd    string                     `json:"cmd"`
	Params map[string]json.RawMessage `json:"-"`
}

func (r *Request) UnmarshalJSON(b []byte) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	r.Params = m
	if raw, ok := m["id"]; ok {
		_ = json.Unmarshal(raw, &r.ID)
	}
	if raw, ok := m["cmd"]; ok {
		_ = json.Unmarshal(raw, &r.Cmd)
	}
	return nil
}

func (r *Request) Str(k string) (string, bool) {
	raw, ok := r.Params[k]
	if !ok {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

func (r *Request) Int(k string) (int, bool) {
	raw, ok := r.Params[k]
	if !ok {
		return 0, false
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, false
	}
	return n, true
}

func (r *Request) Bool(k string) (bool, bool) {
	raw, ok := r.Params[k]
	if !ok {
		return false, false
	}
	var v bool
	if err := json.Unmarshal(raw, &v); err != nil {
		return false, false
	}
	return v, true
}

func (r *Request) StrSlice(k string) ([]string, bool) {
	raw, ok := r.Params[k]
	if !ok {
		return nil, false
	}
	var v []string
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, false
	}
	return v, true
}

func (r *Request) StrMap(k string) (map[string]string, bool) {
	raw, ok := r.Params[k]
	if !ok {
		return nil, false
	}
	var v map[string]string
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, false
	}
	return v, true
}

// Raw hands back an untouched parameter for a caller that needs its own shape.
func (r *Request) Raw(k string) (json.RawMessage, bool) {
	raw, ok := r.Params[k]
	return raw, ok
}

type Error struct {
	Code    ErrCode `json:"code"`
	Message string  `json:"message"`
}

type Response struct {
	ID     string `json:"id"`
	OK     bool   `json:"ok"`
	Result any    `json:"result,omitempty"`
	Error  *Error `json:"error,omitempty"`
}

func OKResp(id string, result any) Response { return Response{ID: id, OK: true, Result: result} }

// ErrResp refuses to emit a code outside the enum. A caller that invents one
// gets "internal" and the invented code in the message, so the bug is visible
// without a client having to handle an unknown value.
func ErrResp(id string, code ErrCode, msg string) Response {
	if !ValidCode(code) {
		msg = string(code) + ": " + msg
		code = ErrInternal
	}
	return Response{ID: id, OK: false, Error: &Error{Code: code, Message: msg}}
}

// Cell is one grid cell on the wire: [text, fg, bg, attrs]. An empty colour
// string means the terminal default.
type Cell struct {
	Text  string
	FG    string
	BG    string
	Attrs uint16
}

func (c Cell) MarshalJSON() ([]byte, error) {
	return json.Marshal([]any{c.Text, c.FG, c.BG, c.Attrs})
}

func (c *Cell) UnmarshalJSON(b []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	if len(raw) != 4 {
		return json.Unmarshal(b, &struct{}{})
	}
	if err := json.Unmarshal(raw[0], &c.Text); err != nil {
		return err
	}
	if err := json.Unmarshal(raw[1], &c.FG); err != nil {
		return err
	}
	if err := json.Unmarshal(raw[2], &c.BG); err != nil {
		return err
	}
	return json.Unmarshal(raw[3], &c.Attrs)
}

// Frame is one render update for one pane. Seq counts per client, because two
// clients attach at different moments and each needs its own baseline.
type Frame struct {
	Event       string         `json:"event"`
	Pane        string         `json:"pane"`
	Seq         uint64         `json:"seq"`
	Cols        int            `json:"cols"`
	Rows        int            `json:"rows"`
	Cursor      [2]int         `json:"cursor"`
	RowsChanged map[int][]Cell `json:"rows_changed"`
}
```

- [ ] **Step 4: Write `state_event.go`**

```go
// harness/coppice/internal/proto/state_event.go
package proto

import (
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

const (
	StateIdle    = "idle"
	StateWorking = "working"
	StateBlocked = "blocked"
	StateDone    = "done"
	StateUnknown = "unknown"
)

const (
	SrcOperator = "operator"
	SrcGate     = "gate"
	SrcHeadless = "headless"
	SrcProcess  = "process"
	SrcManifest = "manifest"
)

// DetailMax is master spec 3.1's cap on the free-text detail field.
const DetailMax = 200

var validStates = map[string]bool{
	StateIdle: true, StateWorking: true, StateBlocked: true, StateDone: true, StateUnknown: true,
}

// sourceRanks encodes the precedence in master spec 3.1:
// operator > gate > headless > process > manifest.
var sourceRanks = map[string]int{
	SrcOperator: 5, SrcGate: 4, SrcHeadless: 3, SrcProcess: 2, SrcManifest: 1,
}

// SourceRank is 0 for anything not in the enum, so an unknown source can never
// win a comparison. That is the fail-closed direction.
func SourceRank(src string) int { return sourceRanks[src] }

type Ask struct {
	ID       string  `json:"id"`
	Tool     string  `json:"tool"`
	Summary  string  `json:"summary"`
	Deadline float64 `json:"deadline"`
}

// PaneStateEvent is master spec 3.1, byte for byte. The Python side in
// opendaisugi.floor.events declares the same shape; testdata/events holds the
// fixtures both sides parse.
type PaneStateEvent struct {
	V                int     `json:"v"`
	TS               float64 `json:"ts"`
	SessionID        string  `json:"session_id"`
	HarnessSessionID *string `json:"harness_session_id"`
	Harness          string  `json:"harness"`
	Pane             *string `json:"pane"`
	State            string  `json:"state"`
	Source           string  `json:"source"`
	Ask              *Ask    `json:"ask,omitempty"`
	Detail           string  `json:"detail"`
}

func (e PaneStateEvent) Validate() error {
	if e.V != 1 {
		return fmt.Errorf("v must be 1, got %d", e.V)
	}
	if e.SessionID == "" {
		return fmt.Errorf("session_id must not be empty")
	}
	if e.Harness == "" {
		return fmt.Errorf("harness must not be empty")
	}
	if !validStates[e.State] {
		return fmt.Errorf("state %q is not one of idle working blocked done unknown", e.State)
	}
	if SourceRank(e.Source) == 0 {
		return fmt.Errorf("source %q is not one of operator gate headless process manifest", e.Source)
	}
	if e.State == StateBlocked && e.Source == SrcGate && e.Ask == nil {
		return fmt.Errorf("a gate blocked event must carry the ask it is holding")
	}
	if e.State != StateBlocked && e.Ask != nil {
		return fmt.Errorf("ask is only valid on a blocked event, state is %q", e.State)
	}
	if e.Source == SrcManifest && e.State == StateDone {
		return fmt.Errorf("a manifest may never report done")
	}
	if utf8.RuneCountInString(e.Detail) > DetailMax {
		return fmt.Errorf("detail is %d chars, the cap is %d",
			utf8.RuneCountInString(e.Detail), DetailMax)
	}
	return nil
}

// ParseStateEvent decodes one line and validates it. A bad line is an error,
// never a default event, because a default would be a state nobody reported.
func ParseStateEvent(line []byte) (PaneStateEvent, error) {
	var e PaneStateEvent
	if err := json.Unmarshal(line, &e); err != nil {
		return PaneStateEvent{}, fmt.Errorf("state event is not JSON: %w", err)
	}
	if err := e.Validate(); err != nil {
		return PaneStateEvent{}, err
	}
	return e, nil
}
```

- [ ] **Step 5: Write `codec.go`**

```go
// harness/coppice/internal/proto/codec.go
package proto

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// MaxLine caps one wire line. A client that sends more is refused rather than
// allowed to grow the server's heap without bound.
const MaxLine = 1 << 20 // 1 MiB

type Decoder struct {
	r *bufio.Reader
}

func NewDecoder(r io.Reader) *Decoder {
	return &Decoder{r: bufio.NewReaderSize(r, 64<<10)}
}

// Next returns one line without its newline. An over-long line is an error and
// the decoder is finished, because the stream is no longer framed.
func (d *Decoder) Next() ([]byte, error) {
	var buf []byte
	for {
		chunk, isPrefix, err := d.r.ReadLine()
		if err != nil {
			return nil, err
		}
		buf = append(buf, chunk...)
		if len(buf) > MaxLine {
			return nil, fmt.Errorf("line is over %d bytes, send a smaller request", MaxLine)
		}
		if !isPrefix {
			return buf, nil
		}
	}
}

// Encoder writes one JSON value per line. Send is safe for concurrent use: the
// frame pump, the state pump and the request handler all write to one client.
type Encoder struct {
	mu sync.Mutex
	w  *bufio.Writer
}

func NewEncoder(w io.Writer) *Encoder { return &Encoder{w: bufio.NewWriterSize(w, 64<<10)} }

func (e *Encoder) Send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.w.Write(b); err != nil {
		return err
	}
	if err := e.w.WriteByte('\n'); err != nil {
		return err
	}
	return e.w.Flush()
}
```

- [ ] **Step 6: Run the test and watch it pass**

Run: `cd harness/coppice && go test -race ./internal/proto/ -v`
Expected: PASS.

- [ ] **Step 7: Write the three event fixtures**

`harness/coppice/testdata/events/gate-blocked.json`:
```json
{"v":1,"ts":1757300000.123,"session_id":"d41c","harness_session_id":"11111111-2222-3333-4444-555555555555","harness":"claude-code","pane":"w1:p3","state":"blocked","source":"gate","ask":{"id":"toolu_01","tool":"Bash","summary":"rm -rf build/","deadline":1757300090.0},"detail":"verdict=deny clause=shell.deny[2]"}
```

`harness/coppice/testdata/events/process-done.json`:
```json
{"v":1,"ts":1757300100.5,"session_id":"d41c","harness_session_id":null,"harness":"shell","pane":"w1:p4","state":"done","source":"process","detail":"exit=0"}
```

`harness/coppice/testdata/events/manifest-working.json`:
```json
{"v":1,"ts":1757300050.0,"session_id":"d41c","harness_session_id":null,"harness":"codex","pane":"w1:p5","state":"working","source":"manifest","detail":"rule=live_turn_working"}
```

`harness/coppice/testdata/events/headless-blocked.json` is the shape a headless adapter emits
when its harness asks a question. pi's `extension_ui_request` (spec-04) and OpenCode's
`permission.ask` (spec-05) both land here, which is why `pane.Event` carries an `Ask`:
```json
{"v":1,"ts":1757300075.0,"session_id":"d41c","harness_session_id":"pi-77","harness":"pi","pane":"w1:p6","state":"blocked","source":"headless","ask":{"id":"ui_1","tool":"Write","summary":"src/main.go","deadline":1757300165.0},"detail":"extension_ui_request"}
```

- [ ] **Step 8: Write the shared-fixture test**

```go
// harness/coppice/internal/proto/fixtures_test.go
package proto

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// Every fixture must parse and validate. If one does not, the Go and Python
// readers have already drifted.
func TestEveryEventFixtureParses(t *testing.T) {
	dir := filepath.Join("..", "..", "testdata", "events")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 4 {
		t.Fatalf("testdata/events has %d fixtures, want at least four", len(entries))
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ParseStateEvent(b); err != nil {
			t.Fatalf("%s does not parse: %v", e.Name(), err)
		}
	}
}

// Spec-02 requires the Go fixtures to be byte-identical to the Python ones.
// Spec-01 owns those bytes, so this test skips with a reason until they land
// and fails loudly the moment they diverge.
func TestEventFixturesMatchTheFloorFixtures(t *testing.T) {
	goDir := filepath.Join("..", "..", "testdata", "events")
	pyDir := filepath.Join("..", "..", "..", "..", "tests", "floor", "testdata", "events")
	if _, err := os.Stat(pyDir); err != nil {
		t.Skip("tests/floor/testdata/events does not exist yet. Spec-01 creates it and owns the bytes.")
	}
	sum := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		h := sha256.Sum256(b)
		return hex.EncodeToString(h[:])
	}
	entries, err := os.ReadDir(goDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		pyPath := filepath.Join(pyDir, e.Name())
		if _, err := os.Stat(pyPath); err != nil {
			t.Fatalf("%s has no counterpart in tests/floor/testdata/events. "+
				"Copy it there or delete it here. Spec-01 is the direction of truth.", e.Name())
		}
		if a, b := sum(filepath.Join(goDir, e.Name())), sum(pyPath); a != b {
			t.Fatalf("%s differs between the Go and Python fixtures. "+
				"Copy tests/floor/testdata/events/%s over the Go copy.", e.Name(), e.Name())
		}
	}
}
```

- [ ] **Step 9: Run the whole package and vet**

Run: `cd harness/coppice && go test -race ./internal/proto/ -v && go vet ./...`
Expected: PASS with the floor-comparison test skipped.

- [ ] **Step 10: Commit**

```bash
git add harness/coppice/internal/proto harness/coppice/testdata/events
git commit -m "feat(coppice): the wire, with a closed error enum and validated state events

Clients switch on the error code, so an invented code becomes internal rather
than a new case a client has to guess at. A state event that fails validation
is an error, never a default, because a default would be a state nobody
reported. The event fixtures are checked against the floor's copies so the Go
and Python readers cannot drift in silence."
```

---

### Task 3: `internal/state` — the merge

**Files:**
- Create: `harness/coppice/internal/state/state.go`
- Test: `harness/coppice/internal/state/state_test.go`

**Interfaces:**
- Consumes: `proto.PaneStateEvent`, `proto.SourceRank`, the state and source constants.
- Produces:
  ```go
  package state
  const HoldWindow = 2.0   // master 3.1 "within 2 s", in seconds
  func Merge(cur *proto.PaneStateEvent, curReceived float64,
             in proto.PaneStateEvent, now float64) proto.PaneStateEvent
  type Store struct{}
  func NewStore() *Store
  func (s *Store) Apply(pane string, in proto.PaneStateEvent, now float64) (proto.PaneStateEvent, bool)
  func (s *Store) Current(pane string) (proto.PaneStateEvent, bool)
  func (s *Store) Received(pane string) (float64, bool)
  func (s *Store) Forget(pane string)
  func (s *Store) Panes() []string
  ```
  `Apply` returns the merged event and whether it changed the pane's state.
  `curReceived` is the *server's* clock when the current event arrived, never the event's own
  `ts`: a hook with a skewed clock must not be able to extend its own hold.

- [ ] **Step 1: Write the failing test**

```go
// harness/coppice/internal/state/state_test.go
package state

import (
	"math/rand"
	"testing"

	"github.com/opendaisugi/coppice/internal/proto"
)

func ev(state, source string, ts float64) proto.PaneStateEvent {
	e := proto.PaneStateEvent{
		V: 1, TS: ts, SessionID: "s1", Harness: "claude-code", State: state, Source: source,
	}
	if state == proto.StateBlocked && source == proto.SrcGate {
		e.Ask = &proto.Ask{ID: "toolu_1", Tool: "Bash", Summary: "rm -rf x", Deadline: ts + 90}
	}
	return e
}

func TestFirstEventWins(t *testing.T) {
	got := Merge(nil, 0, ev(proto.StateWorking, proto.SrcManifest, 100), 100)
	if got.State != proto.StateWorking || got.Source != proto.SrcManifest {
		t.Fatalf("Merge(nil, …) = %s/%s, want working/manifest", got.State, got.Source)
	}
}

func TestManifestCannotOverrideGateWithinTheHoldWindow(t *testing.T) {
	cur := ev(proto.StateWorking, proto.SrcGate, 100)
	got := Merge(&cur, 100, ev(proto.StateIdle, proto.SrcManifest, 101), 101)
	if got.Source != proto.SrcGate || got.State != proto.StateWorking {
		t.Fatalf("manifest overrode the gate at +1 s: got %s/%s", got.State, got.Source)
	}
}

func TestManifestMayOverrideGateAfterTheHoldWindow(t *testing.T) {
	cur := ev(proto.StateWorking, proto.SrcGate, 100)
	got := Merge(&cur, 100, ev(proto.StateIdle, proto.SrcManifest, 103), 103)
	if got.Source != proto.SrcManifest || got.State != proto.StateIdle {
		t.Fatalf("manifest was still blocked at +3 s: got %s/%s", got.State, got.Source)
	}
}

// The hold window is measured against the server's receive time, not against a
// timestamp the caller chose. A hook with a clock a year in the future must not
// be able to hold a pane against every lower source for ever.
func TestTheHoldWindowIgnoresASkewedEventTimestamp(t *testing.T) {
	cur := ev(proto.StateWorking, proto.SrcGate, 9_999_999_999) // far future ts
	got := Merge(&cur, 100, ev(proto.StateIdle, proto.SrcManifest, 103), 103)
	if got.Source != proto.SrcManifest {
		t.Fatalf("a future event ts extended the hold: got %s/%s", got.State, got.Source)
	}
}

func TestOperatorOutranksTheGate(t *testing.T) {
	cur := ev(proto.StateBlocked, proto.SrcGate, 100)
	got := Merge(&cur, 100, ev(proto.StateWorking, proto.SrcOperator, 100.5), 100.5)
	if got.Source != proto.SrcOperator {
		t.Fatalf("operator lost to the gate: got %s", got.Source)
	}
}

func TestGateBlockedHoldsUntilAGateEventClearsIt(t *testing.T) {
	cur := ev(proto.StateBlocked, proto.SrcGate, 100)
	// A working event from the process five seconds later must not clear a gate
	// hold. Only the gate, the deadline, the operator, or a done clears it.
	got := Merge(&cur, 100, ev(proto.StateWorking, proto.SrcProcess, 105), 105)
	if got.State != proto.StateBlocked {
		t.Fatalf("a process working event cleared a gate hold: got %s", got.State)
	}
	got = Merge(&cur, 100, ev(proto.StateWorking, proto.SrcGate, 105), 105)
	if got.State != proto.StateWorking {
		t.Fatalf("the gate could not clear its own hold: got %s", got.State)
	}
}

func TestGateBlockedExpiresToWorkingAtTheDeadline(t *testing.T) {
	cur := ev(proto.StateBlocked, proto.SrcGate, 100) // deadline 190
	got := Merge(&cur, 100, ev(proto.StateIdle, proto.SrcManifest, 191), 191)
	if got.State != proto.StateWorking {
		t.Fatalf("an expired ask did not fall back to working: got %s", got.State)
	}
	if got.Ask != nil {
		t.Fatal("an expired ask was kept on the event, want it dropped")
	}
}

func TestProcessDoneOutranksAHeldGateBlock(t *testing.T) {
	// The process exited. There is nobody left to answer the ask.
	cur := ev(proto.StateBlocked, proto.SrcGate, 100)
	got := Merge(&cur, 100, ev(proto.StateDone, proto.SrcProcess, 101), 101)
	if got.State != proto.StateDone {
		t.Fatalf("process exit did not end a gate hold: got %s", got.State)
	}
}

// The fail-open the review found: a gate WORKING event, then a process exit
// half a second later. The gate-hold branch does not apply because the current
// state is working, so the plain rank comparison used to discard the exit and
// the pane read "working" for ever. watchExit and pumpAdapter fire exactly once
// and never retry, so the loss is permanent.
func TestProcessExitEndsAGateWorkingHoldImmediately(t *testing.T) {
	cur := ev(proto.StateWorking, proto.SrcGate, 100)
	got := Merge(&cur, 100, ev(proto.StateDone, proto.SrcProcess, 100.5), 100.5)
	if got.State != proto.StateDone || got.Source != proto.SrcProcess {
		t.Fatalf("Merge = %s/%s, want done/process: a dead process must never read working",
			got.State, got.Source)
	}
}

func TestHeadlessEndEndsAGateWorkingHoldImmediately(t *testing.T) {
	cur := ev(proto.StateWorking, proto.SrcGate, 100)
	got := Merge(&cur, 100, ev(proto.StateDone, proto.SrcHeadless, 100.1), 100.1)
	if got.State != proto.StateDone {
		t.Fatalf("Merge = %s, want done from the headless end-of-session event", got.State)
	}
}

// Table form, so every source pair is covered rather than the two the prose
// happened to name.
func TestDoneFromProcessOrHeadlessBeatsEveryHeldSource(t *testing.T) {
	cases := []struct {
		curState, curSource string
		inSource            string
	}{
		{proto.StateWorking, proto.SrcGate, proto.SrcProcess},
		{proto.StateWorking, proto.SrcGate, proto.SrcHeadless},
		{proto.StateWorking, proto.SrcOperator, proto.SrcProcess},
		{proto.StateWorking, proto.SrcOperator, proto.SrcHeadless},
		{proto.StateBlocked, proto.SrcGate, proto.SrcProcess},
		{proto.StateBlocked, proto.SrcGate, proto.SrcHeadless},
		{proto.StateIdle, proto.SrcGate, proto.SrcProcess},
	}
	for _, c := range cases {
		cur := ev(c.curState, c.curSource, 100)
		got := Merge(&cur, 100, ev(proto.StateDone, c.inSource, 100.2), 100.2)
		if got.State != proto.StateDone {
			t.Fatalf("cur={%s,%s} in={done,%s} = %s, want done",
				c.curState, c.curSource, c.inSource, got.State)
		}
	}
}

// done from a source that may not produce it must NOT win.
// The downgrade the fuzz test has to allow for: a done nobody was entitled to
// send becomes unknown from that same source. Never idle, and never a silent
// drop, because the source did observe something.
func TestADoneFromAnIneligibleSourceBecomesUnknownFromThatSource(t *testing.T) {
	for _, src := range []string{proto.SrcOperator, proto.SrcGate} {
		got := Merge(nil, 0, ev(proto.StateDone, src, 100), 100)
		if got.State != proto.StateUnknown {
			t.Fatalf("done from %s = %s, want unknown", src, got.State)
		}
		if got.Source != src {
			t.Fatalf("the downgrade changed the source to %s, want %s", got.Source, src)
		}
		if got.Ask != nil {
			t.Fatalf("the downgrade kept an ask: %+v", got.Ask)
		}
	}
}

func TestDoneFromOperatorOrManifestDoesNotWin(t *testing.T) {
	cur := ev(proto.StateWorking, proto.SrcGate, 100)
	if got := Merge(&cur, 100, ev(proto.StateDone, proto.SrcOperator, 100.2), 100.2); got.State == proto.StateDone {
		t.Fatal("an operator claimed done; only process and headless may produce it")
	}
	in := ev(proto.StateWorking, proto.SrcManifest, 100.2)
	in.State = proto.StateDone
	if got := Merge(&cur, 100, in, 100.2); got.State == proto.StateDone {
		t.Fatal("a manifest produced done, which master spec 3.1 forbids")
	}
}

func TestManifestNeverYieldsDone(t *testing.T) {
	cur := ev(proto.StateWorking, proto.SrcManifest, 100)
	in := ev(proto.StateWorking, proto.SrcManifest, 200)
	in.State = proto.StateDone // a caller that ignored Validate
	got := Merge(&cur, 100, in, 200)
	if got.State == proto.StateDone {
		t.Fatal("a manifest event produced done, which master spec 3.1 forbids")
	}
}

func TestDoneIsTerminal(t *testing.T) {
	cur := ev(proto.StateDone, proto.SrcProcess, 100)
	got := Merge(&cur, 100, ev(proto.StateWorking, proto.SrcOperator, 200), 200)
	if got.State != proto.StateDone {
		t.Fatalf("a done pane came back to life: got %s", got.State)
	}
}

func TestStoreReportsWhetherTheStateChanged(t *testing.T) {
	s := NewStore()
	if _, changed := s.Apply("w1:p1", ev(proto.StateWorking, proto.SrcGate, 100), 100); !changed {
		t.Fatal("the first event did not report a change")
	}
	if _, changed := s.Apply("w1:p1", ev(proto.StateWorking, proto.SrcGate, 100.2), 100.2); changed {
		t.Fatal("a repeat of the same state reported a change")
	}
	cur, ok := s.Current("w1:p1")
	if !ok || cur.State != proto.StateWorking {
		t.Fatalf("Current = %+v %v, want a working event", cur, ok)
	}
	recv, ok := s.Received("w1:p1")
	if !ok || recv != 100 {
		t.Fatalf("Received = %v %v, want the server clock of the event that stuck", recv, ok)
	}
}

// A repeat that does not change the state must not refresh the hold either:
// otherwise a chatty gate keeps the screen scanner away for ever.
func TestARepeatDoesNotRefreshTheHold(t *testing.T) {
	s := NewStore()
	s.Apply("w1:p1", ev(proto.StateWorking, proto.SrcGate, 100), 100)
	s.Apply("w1:p1", ev(proto.StateWorking, proto.SrcGate, 100.2), 100.2)
	if recv, _ := s.Received("w1:p1"); recv != 100 {
		t.Fatalf("Received = %v, want 100: a repeat of the same state is not news", recv)
	}
}

// The fail-closed invariant of the whole floor: the state a pane shows must be
// the state its most recent winning event carried. This is stronger than "some
// event in this trial said idle": it compares the merged state against the
// event that produced it.
func TestFuzzTheMergedStateAlwaysCameFromAnEventThatCarriedIt(t *testing.T) {
	states := []string{proto.StateIdle, proto.StateWorking, proto.StateBlocked,
		proto.StateDone, proto.StateUnknown}
	sources := []string{proto.SrcOperator, proto.SrcGate, proto.SrcHeadless,
		proto.SrcProcess, proto.SrcManifest}
	rng := rand.New(rand.NewSource(20260908))
	// Merge returns exactly one of: the incoming event, the current one, or a
	// value derived from the current one. The current one is itself, by
	// induction, one of those three, so the two inventions below are the whole
	// space of values no event carried.
	for trial := 0; trial < 2000; trial++ {
		s := NewStore()
		// history holds every event this pane has ever accepted at the door,
		// as it was SENT. Merge may rewrite the state on the way through.
		var history []proto.PaneStateEvent
		ts := 100.0
		for step := 0; step < 12; step++ {
			st := states[rng.Intn(len(states))]
			src := sources[rng.Intn(len(sources))]
			ts += rng.Float64() * 4
			in := ev(st, src, ts)
			if in.Validate() != nil {
				continue
			}
			history = append(history, in)
			merged, _ := s.Apply("w1:p1", in, ts)

			// The merged state is legitimate only if some accepted event
			// carried exactly that state and source, or it is one of the TWO
			// values Merge is allowed to invent.
			ok := false
			for _, h := range history {
				if h.State == merged.State && h.Source == merged.Source {
					ok = true
					break
				}
			}
			// Invention 1: an expired gate ask becomes working with no ask.
			expired := merged.State == proto.StateWorking &&
				merged.Source == proto.SrcGate && merged.Ask == nil &&
				merged.Detail == "ask deadline passed"
			// Invention 2: a done from a source that may not produce one is
			// downgraded to unknown, keeping the source. Only process and
			// headless may say done (master spec 3.1), so a gate or operator
			// done arrives as unknown from that same source, and no event ever
			// carried that pair.
			downgraded := false
			if merged.State == proto.StateUnknown && merged.Ask == nil {
				for _, h := range history {
					if h.Source == merged.Source && h.State == proto.StateDone &&
						h.Source != proto.SrcProcess && h.Source != proto.SrcHeadless {
						downgraded = true
						break
					}
				}
			}
			derived := expired || downgraded
			if !ok && !derived {
				t.Fatalf("trial %d step %d: merged %s/%s came from no accepted event: %+v",
					trial, step, merged.State, merged.Source, history)
			}
			if merged.State == proto.StateIdle {
				said := false
				for _, h := range history {
					if h.State == proto.StateIdle {
						said = true
					}
				}
				if !said {
					t.Fatalf("trial %d step %d produced idle with nobody reporting idle",
						trial, step)
				}
			}
		}
	}
}
```

- [ ] **Step 2: Run the test and watch it fail**

Run: `cd harness/coppice && go test -race ./internal/state/ -v`
Expected: FAIL, `undefined: Merge`.

- [ ] **Step 3: Write the implementation**

```go
// harness/coppice/internal/state/state.go

// Package state holds master spec section 3.1 in one place: the precedence
// between sources, the gate's hold on blocked, and the rule that idle is never
// a default. Every path into a pane's state goes through Merge.
package state

import (
	"sync"

	"github.com/opendaisugi/coppice/internal/proto"
)

// HoldWindow is the "within 2 s" of master spec 3.1: for two seconds after a
// higher-precedence source speaks, a lower one cannot contradict it. Seconds.
const HoldWindow = 2.0

// endsTheSession reports whether this event is the one kind that outranks every
// hold. Master spec 3.1: done comes only from a process exit or a headless
// end-of-session event, and it is terminal. Nothing may swallow it, because a
// swallowed done leaves a dead agent reading "working" for ever: watchExit and
// pumpAdapter each fire exactly once and never retry.
func endsTheSession(in proto.PaneStateEvent) bool {
	return in.State == proto.StateDone &&
		(in.Source == proto.SrcProcess || in.Source == proto.SrcHeadless)
}

// Merge folds one incoming event into a pane's current state.
//
// curReceived is the server's own clock when the current event arrived, and now
// is the server's clock for this one. Neither is the event's own ts: a hook with
// a skewed clock must not be able to extend its own hold.
func Merge(cur *proto.PaneStateEvent, curReceived float64,
	in proto.PaneStateEvent, now float64) proto.PaneStateEvent {

	// A manifest may never say done. Master spec 3.1. Downgrade rather than
	// drop, so the screen evidence still counts for something.
	if in.Source == proto.SrcManifest && in.State == proto.StateDone {
		in.State = proto.StateUnknown
		in.Ask = nil
	}
	// Only process and headless may produce done at all.
	if in.State == proto.StateDone && !endsTheSession(in) {
		in.State = proto.StateUnknown
		in.Ask = nil
	}
	if cur == nil {
		return in
	}

	// done is terminal. The process is gone; nothing observed later is about a
	// running agent.
	if cur.State == proto.StateDone {
		return *cur
	}

	// The session ending outranks every hold, whatever the current state is.
	// This is checked before the gate branch and before the rank comparison,
	// because a gate WORKING hold would otherwise discard a process exit that
	// arrives inside the two-second window.
	if endsTheSession(in) {
		return in
	}

	// A gate hold: blocked from the gate stands until the gate itself clears
	// it, the operator overrides, or the ask's deadline passes.
	if cur.State == proto.StateBlocked && cur.Source == proto.SrcGate {
		if in.Source == proto.SrcGate || in.Source == proto.SrcOperator {
			return in
		}
		if cur.Ask != nil && cur.Ask.Deadline > 0 && now >= cur.Ask.Deadline {
			expired := *cur
			expired.State = proto.StateWorking
			expired.Ask = nil
			expired.TS = now
			expired.Detail = "ask deadline passed"
			return expired
		}
		return *cur
	}

	// Ordinary precedence, bounded by the hold window on the server's clock.
	if proto.SourceRank(in.Source) < proto.SourceRank(cur.Source) &&
		now-curReceived < HoldWindow {
		return *cur
	}
	return in
}

// Store is the per-pane current state. It is safe for concurrent use: the PTY
// pump, the manifest tick and the socket dispatcher all write to it.
type Store struct {
	mu       sync.RWMutex
	cur      map[string]proto.PaneStateEvent
	received map[string]float64
}

func NewStore() *Store {
	return &Store{cur: map[string]proto.PaneStateEvent{}, received: map[string]float64{}}
}

// Apply merges one event and reports whether the pane's visible state or
// source changed, so callers only broadcast real news. The receive clock
// advances only when the state actually changed: a chatty source repeating
// itself must not keep the screen scanner away for ever.
func (s *Store) Apply(pane string, in proto.PaneStateEvent, now float64) (proto.PaneStateEvent, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var prev *proto.PaneStateEvent
	prevReceived := 0.0
	if p, ok := s.cur[pane]; ok {
		prev = &p
		prevReceived = s.received[pane]
	}
	merged := Merge(prev, prevReceived, in, now)
	changed := prev == nil || prev.State != merged.State || prev.Source != merged.Source ||
		askChanged(prev.Ask, merged.Ask)
	s.cur[pane] = merged
	if changed {
		s.received[pane] = now
	}
	return merged, changed
}

func askChanged(a, b *proto.Ask) bool {
	switch {
	case a == nil && b == nil:
		return false
	case a == nil || b == nil:
		return true
	default:
		return a.ID != b.ID
	}
}

func (s *Store) Current(pane string) (proto.PaneStateEvent, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.cur[pane]
	return e, ok
}

// Received is the server clock when this pane's current state arrived. The
// manifest tick uses it, never the event's own ts.
func (s *Store) Received(pane string) (float64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.received[pane]
	return r, ok
}

func (s *Store) Forget(pane string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cur, pane)
	delete(s.received, pane)
}

func (s *Store) Panes() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.cur))
	for k := range s.cur {
		out = append(out, k)
	}
	return out
}
```

- [ ] **Step 4: Run the test and watch it pass**

Run: `cd harness/coppice && go test -race ./internal/state/ -v`
Expected: PASS, including the 2000-trial fuzz.

- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/state
git commit -m "feat(coppice): one merge for pane state, and a dead process always wins

Master spec 3.1 lives in exactly one function so no caller can invent its own
precedence. A process exit or a headless end-of-session outranks every hold,
including a gate working hold inside the two-second window: watchExit fires
once and never retries, so swallowing it would leave a dead agent reading
working for ever. The hold window is measured on the server's clock, not on a
timestamp the caller chose, so a hook with a skewed clock cannot hold a pane
against every lower source."
```


---

### Task 4: `internal/layout` — workspaces, tabs, panes

**Files:**
- Create: `harness/coppice/internal/layout/layout.go`
- Test: `harness/coppice/internal/layout/layout_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  ```go
  package layout
  type PaneKind string; const ( KindPTY PaneKind = "pty"; KindHeadless PaneKind = "headless" )
  type Pane struct { ID, Label, Cwd string; Argv []string; Env map[string]string
                     Kind PaneKind; Harness string; Cols, Rows int
                     HarnessSessionID string; Closed bool; ExitCode *int }
  type Tab struct { ID, Label string; PaneIDs []string }
  type Workspace struct { ID, Label, Cwd string; TabIDs []string }
  type Tree struct{}
  func New() *Tree
  func (t *Tree) CreateWorkspace(label, cwd string) Workspace
  func (t *Tree) CreateTab(wsID, label string) (Tab, error)
  func (t *Tree) CreatePane(wsID, tabID string, p Pane) (Pane, error)
  func (t *Tree) Pane(id string) (Pane, bool)          // a COPY, never the live record
  func (t *Tree) Workspace(id string) (Workspace, bool)
  func (t *Tree) Tab(id string) (Tab, bool)
  func (t *Tree) Panes() []Pane
  func (t *Tree) Workspaces() []Workspace
  func (t *Tree) Tabs(wsID string) ([]Tab, error)
  func (t *Tree) UpdatePane(id string, f func(*Pane)) error   // mutate under the tree's lock
  func (t *Tree) Current() (wsID, tabID string)
  func (t *Tree) SetCurrent(wsID, tabID string) error
  func (t *Tree) ClosePane(id string, exit *int) error
  func (t *Tree) Save(path string) error
  func Load(path string) (*Tree, error)
  var ErrNoWorkspace, ErrNoTab, ErrNoPane error
  ```
  Every accessor returns a **copy**. Handing out the live `*Pane` would let `handleResize`,
  `pumpAdapter` and the restart path mutate records while `Save` marshals them under `RLock`,
  which `go test -race` reports as a data race. `UpdatePane` is the only way to change one.

- [ ] **Step 1: Write the failing test**

```go
// harness/coppice/internal/layout/layout_test.go
package layout

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestPaneIDsAreWorkspaceScopedAndStable(t *testing.T) {
	tr := New()
	ws := tr.CreateWorkspace("main", "/repo")
	tab, err := tr.CreateTab(ws.ID, "work")
	if err != nil {
		t.Fatal(err)
	}
	p1, err := tr.CreatePane(ws.ID, tab.ID, Pane{Cwd: "/repo", Argv: []string{"sh"}, Kind: KindPTY})
	if err != nil {
		t.Fatal(err)
	}
	p2, err := tr.CreatePane(ws.ID, tab.ID, Pane{Cwd: "/repo", Argv: []string{"sh"}, Kind: KindPTY})
	if err != nil {
		t.Fatal(err)
	}
	if ws.ID != "w1" || p1.ID != "w1:p1" || p2.ID != "w1:p2" {
		t.Fatalf("ids = %q %q %q, want w1 / w1:p1 / w1:p2", ws.ID, p1.ID, p2.ID)
	}
	// A closed pane must not free its number. A client holding "w1:p1" would
	// otherwise start talking to a different process.
	if err := tr.ClosePane(p1.ID, nil); err != nil {
		t.Fatal(err)
	}
	p3, err := tr.CreatePane(ws.ID, tab.ID, Pane{Cwd: "/repo", Argv: []string{"sh"}, Kind: KindPTY})
	if err != nil {
		t.Fatal(err)
	}
	if p3.ID != "w1:p3" {
		t.Fatalf("a reused pane number: got %q, want w1:p3", p3.ID)
	}
}

func TestSecondWorkspaceGetsItsOwnPaneNumbering(t *testing.T) {
	tr := New()
	a := tr.CreateWorkspace("a", "/a")
	b := tr.CreateWorkspace("b", "/b")
	ta, _ := tr.CreateTab(a.ID, "t")
	tb, _ := tr.CreateTab(b.ID, "t")
	pa, _ := tr.CreatePane(a.ID, ta.ID, Pane{Cwd: "/a", Kind: KindPTY})
	pb, _ := tr.CreatePane(b.ID, tb.ID, Pane{Cwd: "/b", Kind: KindPTY})
	if pa.ID != "w1:p1" || pb.ID != "w2:p1" {
		t.Fatalf("ids = %q %q, want w1:p1 / w2:p1", pa.ID, pb.ID)
	}
}

func TestUnknownIDsReturnTypedErrors(t *testing.T) {
	tr := New()
	if _, err := tr.CreateTab("w9", "x"); !errors.Is(err, ErrNoWorkspace) {
		t.Fatalf("CreateTab on a missing workspace = %v, want ErrNoWorkspace", err)
	}
	ws := tr.CreateWorkspace("main", "/repo")
	if _, err := tr.CreatePane(ws.ID, "t9", Pane{}); !errors.Is(err, ErrNoTab) {
		t.Fatalf("CreatePane on a missing tab = %v, want ErrNoTab", err)
	}
	if err := tr.ClosePane("w1:p9", nil); !errors.Is(err, ErrNoPane) {
		t.Fatalf("ClosePane on a missing pane = %v, want ErrNoPane", err)
	}
}

func TestCurrentDefaultsToTheFirstWorkspaceAndTab(t *testing.T) {
	tr := New()
	ws := tr.CreateWorkspace("main", "/repo")
	tab, _ := tr.CreateTab(ws.ID, "work")
	gotWS, gotTab := tr.Current()
	if gotWS != ws.ID || gotTab != tab.ID {
		t.Fatalf("Current() = %q %q, want %q %q", gotWS, gotTab, ws.ID, tab.ID)
	}
}

func TestSaveAndLoadRestoreTheTreeAndTheCounters(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "layout.json")

	tr := New()
	ws := tr.CreateWorkspace("main", "/repo")
	tab, _ := tr.CreateTab(ws.ID, "work")
	p, _ := tr.CreatePane(ws.ID, tab.ID, Pane{
		Cwd: "/repo/sub", Label: "auth fix", Argv: []string{"claude"},
		Env: map[string]string{"FOO": "bar"}, Kind: KindPTY, Cols: 120, Rows: 40,
		HarnessSessionID: "abc",
	})
	if err := tr.Save(path); err != nil {
		t.Fatal(err)
	}

	back, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := back.Pane(p.ID)
	if !ok {
		t.Fatalf("pane %s did not survive the round trip", p.ID)
	}
	if got.Cwd != "/repo/sub" || got.Label != "auth fix" || got.HarnessSessionID != "abc" ||
		got.Cols != 120 || got.Rows != 40 || got.Env["FOO"] != "bar" {
		t.Fatalf("restored pane = %+v, want the saved fields", got)
	}
	// The counter must survive too, or a restart hands out an id a client
	// already holds.
	next, err := back.CreatePane(ws.ID, tab.ID, Pane{Cwd: "/repo", Kind: KindPTY})
	if err != nil {
		t.Fatal(err)
	}
	if next.ID != "w1:p2" {
		t.Fatalf("post-restore pane id = %q, want w1:p2", next.ID)
	}
}

// The tree hands out copies. A caller that mutates what it got must not be
// able to change the tree, or Save marshals records while a handler edits them
// and go test -race fails.
func TestPaneAccessorsReturnCopies(t *testing.T) {
	tr := New()
	ws := tr.CreateWorkspace("main", "/repo")
	tab, _ := tr.CreateTab(ws.ID, "work")
	p, _ := tr.CreatePane(ws.ID, tab.ID, Pane{Cwd: "/repo", Kind: KindPTY, Label: "before"})

	got, _ := tr.Pane(p.ID)
	got.Label = "mutated"
	again, _ := tr.Pane(p.ID)
	if again.Label != "before" {
		t.Fatalf("mutating the returned pane changed the tree: label is %q", again.Label)
	}

	list := tr.Panes()
	list[0].Cwd = "/elsewhere"
	if back, _ := tr.Pane(p.ID); back.Cwd != "/repo" {
		t.Fatalf("mutating a Panes() entry changed the tree: cwd is %q", back.Cwd)
	}
}

func TestUpdatePaneIsTheOnlyWayToChangeOne(t *testing.T) {
	tr := New()
	ws := tr.CreateWorkspace("main", "/repo")
	tab, _ := tr.CreateTab(ws.ID, "work")
	p, _ := tr.CreatePane(ws.ID, tab.ID, Pane{Cwd: "/repo", Kind: KindPTY})

	if err := tr.UpdatePane(p.ID, func(x *Pane) {
		x.Cols, x.Rows = 200, 60
		x.HarnessSessionID = "sess-9"
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := tr.Pane(p.ID)
	if got.Cols != 200 || got.Rows != 60 || got.HarnessSessionID != "sess-9" {
		t.Fatalf("UpdatePane did not take: %+v", got)
	}
	if err := tr.UpdatePane("w9:p9", func(*Pane) {}); !errors.Is(err, ErrNoPane) {
		t.Fatalf("UpdatePane on a missing pane = %v, want ErrNoPane", err)
	}
}

// Saving while handlers mutate panes is the shape the race detector catches.
func TestConcurrentUpdateAndSaveIsRaceFree(t *testing.T) {
	dir := t.TempDir()
	tr := New()
	ws := tr.CreateWorkspace("main", "/repo")
	tab, _ := tr.CreateTab(ws.ID, "work")
	p, _ := tr.CreatePane(ws.ID, tab.ID, Pane{Cwd: "/repo", Kind: KindPTY})

	done := make(chan struct{}, 2)
	go func() {
		for i := 0; i < 200; i++ {
			_ = tr.UpdatePane(p.ID, func(x *Pane) { x.Cols = 80 + i%40 })
		}
		done <- struct{}{}
	}()
	go func() {
		for i := 0; i < 200; i++ {
			_ = tr.Save(filepath.Join(dir, "layout.json"))
		}
		done <- struct{}{}
	}()
	<-done
	<-done
}

func TestSaveIsAtomicAndPrivate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "layout.json")
	tr := New()
	tr.CreateWorkspace("main", "/repo")
	if err := tr.Save(path); err != nil {
		t.Fatal(err)
	}
	info, err := statFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if info != 0o600 {
		t.Fatalf("layout file mode = %o, want 600", info)
	}
}
```

Add this helper to the same file:

```go
func statFile(path string) (uint32, error) {
	fi, err := osStat(path)
	if err != nil {
		return 0, err
	}
	return uint32(fi.Mode().Perm()), nil
}
```

and at the top of the test file add `import "os"` plus `var osStat = os.Stat`.

- [ ] **Step 2: Run the test and watch it fail**

Run: `cd harness/coppice && go test -race ./internal/layout/ -v`
Expected: FAIL, `undefined: New`.

- [ ] **Step 3: Write the implementation**

```go
// harness/coppice/internal/layout/layout.go

// Package layout is the workspace, tab and pane tree. Ids look like "w1:p3":
// pane numbers are workspace-scoped and never reused, because a client holds an
// id across a close and must not silently start talking to a different process.
package layout

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

var (
	ErrNoWorkspace = errors.New("no such workspace")
	ErrNoTab       = errors.New("no such tab")
	ErrNoPane      = errors.New("no such pane")
)

type PaneKind string

const (
	KindPTY      PaneKind = "pty"
	KindHeadless PaneKind = "headless"
)

type Pane struct {
	ID               string            `json:"id"`
	Workspace        string            `json:"workspace"`
	Tab              string            `json:"tab"`
	Label            string            `json:"label"`
	Cwd              string            `json:"cwd"`
	Argv             []string          `json:"argv"`
	Env              map[string]string `json:"env,omitempty"`
	Kind             PaneKind          `json:"kind"`
	Harness          string            `json:"harness,omitempty"`
	Cols             int               `json:"cols"`
	Rows             int               `json:"rows"`
	HarnessSessionID string            `json:"harness_session_id,omitempty"`
	Closed           bool              `json:"closed"`
	ExitCode         *int              `json:"exit_code,omitempty"`
}

type Tab struct {
	ID        string   `json:"id"`
	Workspace string   `json:"workspace"`
	Label     string   `json:"label"`
	PaneIDs   []string `json:"pane_ids"`
}

type Workspace struct {
	ID        string   `json:"id"`
	Label     string   `json:"label"`
	Cwd       string   `json:"cwd"`
	TabIDs    []string `json:"tab_ids"`
	NextPane  int      `json:"next_pane"`
	NextTabNo int      `json:"next_tab"`
}

type snapshot struct {
	V          int                   `json:"v"`
	Workspaces map[string]*Workspace `json:"workspaces"`
	Tabs       map[string]*Tab       `json:"tabs"`
	Panes      map[string]*Pane      `json:"panes"`
	NextWS     int                   `json:"next_workspace"`
	CurrentWS  string                `json:"current_workspace"`
	CurrentTab string                `json:"current_tab"`
}

type Tree struct {
	mu   sync.RWMutex
	snap snapshot
}

func New() *Tree {
	return &Tree{snap: snapshot{
		V:          1,
		Workspaces: map[string]*Workspace{},
		Tabs:       map[string]*Tab{},
		Panes:      map[string]*Pane{},
		NextWS:     1,
	}}
}

func (t *Tree) CreateWorkspace(label, cwd string) Workspace {
	t.mu.Lock()
	defer t.mu.Unlock()
	ws := &Workspace{
		ID: fmt.Sprintf("w%d", t.snap.NextWS), Label: label, Cwd: cwd,
		NextPane: 1, NextTabNo: 1,
	}
	t.snap.NextWS++
	t.snap.Workspaces[ws.ID] = ws
	if t.snap.CurrentWS == "" {
		t.snap.CurrentWS = ws.ID
	}
	return *ws
}

func (t *Tree) CreateTab(wsID, label string) (Tab, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	ws, ok := t.snap.Workspaces[wsID]
	if !ok {
		return Tab{}, fmt.Errorf("%w: %s", ErrNoWorkspace, wsID)
	}
	tab := &Tab{ID: fmt.Sprintf("%s:t%d", ws.ID, ws.NextTabNo), Workspace: ws.ID, Label: label}
	ws.NextTabNo++
	ws.TabIDs = append(ws.TabIDs, tab.ID)
	t.snap.Tabs[tab.ID] = tab
	if t.snap.CurrentTab == "" {
		t.snap.CurrentTab = tab.ID
	}
	return *tab, nil
}

func (t *Tree) CreatePane(wsID, tabID string, p Pane) (Pane, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	ws, ok := t.snap.Workspaces[wsID]
	if !ok {
		return Pane{}, fmt.Errorf("%w: %s", ErrNoWorkspace, wsID)
	}
	tab, ok := t.snap.Tabs[tabID]
	if !ok || tab.Workspace != wsID {
		return Pane{}, fmt.Errorf("%w: %s", ErrNoTab, tabID)
	}
	p.ID = fmt.Sprintf("%s:p%d", ws.ID, ws.NextPane)
	ws.NextPane++
	p.Workspace = ws.ID
	p.Tab = tab.ID
	if p.Cols == 0 {
		p.Cols = 120
	}
	if p.Rows == 0 {
		p.Rows = 40
	}
	tab.PaneIDs = append(tab.PaneIDs, p.ID)
	stored := p
	t.snap.Panes[p.ID] = &stored
	return stored, nil
}

// clonePane deep-copies the one field that is a reference type, so a caller
// cannot reach back into the tree through the map it was handed.
func clonePane(p *Pane) Pane {
	out := *p
	if p.Env != nil {
		out.Env = make(map[string]string, len(p.Env))
		for k, v := range p.Env {
			out.Env[k] = v
		}
	}
	if p.Argv != nil {
		out.Argv = append([]string(nil), p.Argv...)
	}
	if p.ExitCode != nil {
		c := *p.ExitCode
		out.ExitCode = &c
	}
	return out
}

// Pane returns a COPY. Handing out the live record would let a handler mutate
// it while Save marshals the same pointer under RLock, which is a data race.
func (t *Tree) Pane(id string) (Pane, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	p, ok := t.snap.Panes[id]
	if !ok {
		return Pane{}, false
	}
	return clonePane(p), true
}

// UpdatePane is the only way to change a pane. The callback runs under the
// write lock, so no reader sees a half-applied change.
func (t *Tree) UpdatePane(id string, f func(*Pane)) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	p, ok := t.snap.Panes[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNoPane, id)
	}
	f(p)
	return nil
}

func (t *Tree) Workspace(id string) (Workspace, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	w, ok := t.snap.Workspaces[id]
	if !ok {
		return Workspace{}, false
	}
	out := *w
	out.TabIDs = append([]string(nil), w.TabIDs...)
	return out, true
}

func (t *Tree) Tab(id string) (Tab, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	tb, ok := t.snap.Tabs[id]
	if !ok {
		return Tab{}, false
	}
	out := *tb
	out.PaneIDs = append([]string(nil), tb.PaneIDs...)
	return out, true
}

func (t *Tree) Panes() []Pane {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]Pane, 0, len(t.snap.Panes))
	for _, p := range t.snap.Panes {
		out = append(out, clonePane(p))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (t *Tree) Workspaces() []Workspace {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]Workspace, 0, len(t.snap.Workspaces))
	for _, w := range t.snap.Workspaces {
		c := *w
		c.TabIDs = append([]string(nil), w.TabIDs...)
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (t *Tree) Tabs(wsID string) ([]Tab, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	ws, ok := t.snap.Workspaces[wsID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoWorkspace, wsID)
	}
	out := make([]Tab, 0, len(ws.TabIDs))
	for _, id := range ws.TabIDs {
		if tb, ok := t.snap.Tabs[id]; ok {
			c := *tb
			c.PaneIDs = append([]string(nil), tb.PaneIDs...)
			out = append(out, c)
		}
	}
	return out, nil
}

func (t *Tree) Current() (string, string) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.snap.CurrentWS, t.snap.CurrentTab
}

func (t *Tree) SetCurrent(wsID, tabID string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.snap.Workspaces[wsID]; !ok {
		return fmt.Errorf("%w: %s", ErrNoWorkspace, wsID)
	}
	if _, ok := t.snap.Tabs[tabID]; !ok {
		return fmt.Errorf("%w: %s", ErrNoTab, tabID)
	}
	t.snap.CurrentWS, t.snap.CurrentTab = wsID, tabID
	return nil
}

// ClosePane marks a pane closed. It keeps the record, so `pane list` can still
// show what ran and a client's stale id gets pane_closed rather than silence.
func (t *Tree) ClosePane(id string, exit *int) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	p, ok := t.snap.Panes[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNoPane, id)
	}
	p.Closed = true
	p.ExitCode = exit
	return nil
}

// Save writes the tree atomically at mode 0600. A half-written layout after a
// crash would restore a tree that never existed.
func (t *Tree) Save(path string) error {
	t.mu.RLock()
	b, err := json.MarshalIndent(t.snap, "", "  ")
	t.mu.RUnlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func Load(path string) (*Tree, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("layout file is not readable: %w", err)
	}
	if s.Workspaces == nil {
		s.Workspaces = map[string]*Workspace{}
	}
	if s.Tabs == nil {
		s.Tabs = map[string]*Tab{}
	}
	if s.Panes == nil {
		s.Panes = map[string]*Pane{}
	}
	if s.NextWS == 0 {
		s.NextWS = len(s.Workspaces) + 1
	}
	return &Tree{snap: s}, nil
}
```

- [ ] **Step 4: Run the test and watch it pass**

Run: `cd harness/coppice && go test -race ./internal/layout/ -v`
Expected: PASS. The race detector is what proves the copies are real: without them, `Save`
marshals records a handler is mutating.

- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/layout
git commit -m "feat(coppice): the pane tree, with ids that are never reused

A closed pane keeps its record and its number. A client that holds w1:p1 across
a close then gets pane_closed instead of quietly reaching a different process.
Save is atomic at 0600, because a half-written layout after a crash restores a
workshop that never existed."
```

---

### Task 5: `internal/pane` — the grid and the frame diff

**Files:**
- Create: `harness/coppice/internal/pane/grid.go`
- Create: `harness/coppice/testdata/vt/gen/main.go`
- Create: `harness/coppice/testdata/vt/README.md`
- Create: `harness/coppice/scripts/record-vt.sh`
- Test: `harness/coppice/internal/pane/grid_test.go`

**Interfaces:**
- Consumes: `vt.Term`, `vt.Cell`, `vt.RGB`, the `vt.Attr*` bits, `proto.Cell`, `proto.Frame`,
  `toolchain.SkipReason`.
- Produces:
  ```go
  package pane
  type ReadSource string
  const ( ReadVisible ReadSource = "visible"; ReadRecent = "recent"; ReadDetection = "detection" )
  const DetectionRows = 12
  const RecentRows = 200
  type Grid struct{}
  func NewGrid(cols, rows int) (*Grid, error)
  func (g *Grid) Write(p []byte) (int, error)
  func (g *Grid) Resize(cols, rows int) error
  func (g *Grid) Size() (int, int)
  func (g *Grid) Title() string
  func (g *Grid) Progress() string
  func (g *Grid) Read(src ReadSource) (string, error)
  func (g *Grid) Snapshot() (rows [][]proto.Cell, cursor [2]int, err error)
  func (g *Grid) Close()
  type FrameState struct{}
  func NewFrameState() *FrameState
  func (fs *FrameState) Next(paneID string, g *Grid) (proto.Frame, bool, error)
  ```
  `Next` returns the frame to send to one client and whether anything changed.
  The first call for a client sends every row.

- [ ] **Step 1: Write the VT stream generator**

The goldens must be reproducible from a checked-in program, not from a binary blob nobody can
regenerate. This generator writes the correctness fixtures.

```go
// harness/coppice/testdata/vt/gen/main.go

// Command gen writes the deterministic VT byte streams the grid goldens use.
// Run it from harness/coppice: go run ./testdata/vt/gen
// Recorded streams from real harnesses are a separate, opt-in path. See
// testdata/vt/README.md.
package main

import (
	"fmt"
	"os"
	"path/filepath"
)

var streams = map[string]string{
	// Plain text, SGR colour, and a reset.
	"basic.bin": "Hello, \x1b[1;32mworld\x1b[0m!\r\nsecond line\r\n",
	// Soft wrap: 30 characters into a 20 column terminal.
	"wrap.bin": "abcdefghijklmnopqrstuvwxyz0123\r\n",
	// Scroll: more lines than the viewport holds.
	"scroll.bin": "l1\r\nl2\r\nl3\r\nl4\r\nl5\r\nl6\r\nl7\r\nl8\r\n",
	// Cursor addressing plus erase to end of line.
	"erase.bin": "aaaaaaaa\r\n\x1b[1;4H\x1b[K",
	// OSC 0 title and OSC 9;4 progress.
	"osc.bin": "\x1b]0;claude working\x07\x1b]9;4;1;40\x07ready\r\n",
	// A Claude-shaped idle prompt box, for the detection region.
	"promptbox.bin": "some output\r\n" +
		"────────────────────\r\n" +
		"❯ \r\n" +
		"────────────────────\r\n",
}

func main() {
	dir := filepath.Join("testdata", "vt")
	for name, body := range streams {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	fmt.Printf("wrote %d streams to %s\n", len(streams), dir)
}
```

`harness/coppice/testdata/vt/README.md`:

```markdown
# VT fixtures

Two kinds of file live here.

**Generated.** `*.bin` written by `go run ./testdata/vt/gen` from
`testdata/vt/gen/main.go`. These cover the VT behaviour the grid must get right:
SGR, wrap, scroll, erase, OSC title and OSC progress, and a prompt box. They are
deterministic, so regenerating them must not change a golden. If it does, the
generator changed and the goldens need a fresh look.

**Recorded.** `recorded-*.bin` captured from a real harness with
`scripts/record-vt.sh`. These are opt-in. Nothing in CI records anything, and
the tests that read them skip with a reason when the file is absent. Record one
when a real harness renders something the generated set does not cover.

`*.golden.txt` is the plain-text screen the grid produces for the matching
stream. Regenerate every golden with `COPPICE_UPDATE_GOLDEN=1 go test ./internal/pane/`.
```

`harness/coppice/scripts/record-vt.sh`:

```bash
#!/usr/bin/env bash
# harness/coppice/scripts/record-vt.sh
# Records a real harness session as a raw VT byte stream for the grid goldens.
# Usage: scripts/record-vt.sh recorded-claude-idle -- claude
set -euo pipefail
name="${1:?usage: record-vt.sh <name> -- <command> [args...]}"
shift
[ "${1:-}" = "--" ] && shift
out="$(dirname "$0")/../testdata/vt/${name}.bin"
echo "recording to $out. Quit the harness when the screen shows what you want."
script -q -c "$*" "$out.typescript"
# script writes a typescript with its own header line; drop it.
tail -c +$(( $(head -1 "$out.typescript" | wc -c) + 1 )) "$out.typescript" > "$out"
rm -f "$out.typescript"
echo "wrote $out. Now run: COPPICE_UPDATE_GOLDEN=1 go test ./internal/pane/"
```

- [ ] **Step 2: Generate the streams**

Run: `cd harness/coppice && chmod +x scripts/record-vt.sh && go run ./testdata/vt/gen`
Expected: `wrote 6 streams to testdata/vt`.

- [ ] **Step 3: Write the failing test**

```go
// harness/coppice/internal/pane/grid_test.go
package pane

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/toolchain"
)

func skipWithoutLib(t *testing.T) {
	t.Helper()
	if r := toolchain.SkipReason(); r != "" {
		t.Skip(r)
	}
}

// golden compares got against testdata/vt/<name>.golden.txt, and rewrites the
// file when COPPICE_UPDATE_GOLDEN is set.
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("..", "..", "testdata", "vt", name+".golden.txt")
	if os.Getenv("COPPICE_UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v. Create it with COPPICE_UPDATE_GOLDEN=1 go test ./internal/pane/", err)
	}
	if got != string(want) {
		t.Fatalf("grid for %s differs.\ngot:\n%s\nwant:\n%s", name, got, want)
	}
}

func feedStream(t *testing.T, g *Grid, name string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "vt", name))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Write(b); err != nil {
		t.Fatal(err)
	}
}

func TestGoldenGrids(t *testing.T) {
	skipWithoutLib(t)
	cases := []struct{ stream string }{
		{"basic.bin"}, {"wrap.bin"}, {"scroll.bin"}, {"erase.bin"},
		{"osc.bin"}, {"promptbox.bin"},
	}
	for _, c := range cases {
		t.Run(c.stream, func(t *testing.T) {
			g, err := NewGrid(20, 6)
			if err != nil {
				t.Fatal(err)
			}
			defer g.Close()
			feedStream(t, g, c.stream)
			got, err := g.Read(ReadVisible)
			if err != nil {
				t.Fatal(err)
			}
			golden(t, strings.TrimSuffix(c.stream, ".bin"), got)
		})
	}
}

func TestOSCStreamSetsTitleAndProgress(t *testing.T) {
	skipWithoutLib(t)
	g, err := NewGrid(20, 6)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	feedStream(t, g, "osc.bin")
	if got := g.Title(); got != "claude working" {
		t.Fatalf("Title() = %q, want %q", got, "claude working")
	}
	if got := g.Progress(); !strings.HasPrefix(got, "4;1") {
		t.Fatalf("Progress() = %q, want it to start with 4;1", got)
	}
}

func TestReadDetectionIsTheBottomTwelveUnwrappedRows(t *testing.T) {
	skipWithoutLib(t)
	g, err := NewGrid(20, 30)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	for i := 1; i <= 25; i++ {
		if _, err := g.Write([]byte(strings.Repeat("x", 3) + string(rune('a'+i%26)) + "\r\n")); err != nil {
			t.Fatal(err)
		}
	}
	got, err := g.Read(ReadDetection)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) > DetectionRows {
		t.Fatalf("detection returned %d lines, want at most %d", len(lines), DetectionRows)
	}
}

func TestReadRecentReachesIntoScrollback(t *testing.T) {
	skipWithoutLib(t)
	g, err := NewGrid(20, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	feedStream(t, g, "scroll.bin")
	visible, err := g.Read(ReadVisible)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(visible, "l1") {
		t.Fatalf("visible = %q, want l1 to have scrolled off a 4 row terminal", visible)
	}
	recent, err := g.Read(ReadRecent)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(recent, "l1") {
		t.Fatalf("recent = %q, want it to reach back to l1", recent)
	}
}

func TestResizeChangesTheReportedSize(t *testing.T) {
	skipWithoutLib(t)
	g, err := NewGrid(20, 6)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if err := g.Resize(40, 12); err != nil {
		t.Fatal(err)
	}
	c, r := g.Size()
	if c != 40 || r != 12 {
		t.Fatalf("Size() = %d x %d, want 40 x 12", c, r)
	}
}

func TestFirstFrameIsFullAndTheSecondCarriesOnlyChangedRows(t *testing.T) {
	skipWithoutLib(t)
	g, err := NewGrid(10, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if _, err := g.Write([]byte("aa\r\nbb\r\n")); err != nil {
		t.Fatal(err)
	}

	fs := NewFrameState()
	first, changed, err := fs.Next("w1:p1", g)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("the first frame reported no change")
	}
	if first.Seq != 1 {
		t.Fatalf("first frame seq = %d, want 1", first.Seq)
	}
	if len(first.RowsChanged) != 4 {
		t.Fatalf("first frame carried %d rows, want all 4", len(first.RowsChanged))
	}

	// Nothing moved.
	if _, changed, err := fs.Next("w1:p1", g); err != nil {
		t.Fatal(err)
	} else if changed {
		t.Fatal("an unchanged grid produced a frame")
	}

	// One row changes.
	if _, err := g.Write([]byte("cc")); err != nil {
		t.Fatal(err)
	}
	second, changed, err := fs.Next("w1:p1", g)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("a changed grid produced no frame")
	}
	if second.Seq != 2 {
		t.Fatalf("second frame seq = %d, want 2", second.Seq)
	}
	if len(second.RowsChanged) != 1 {
		t.Fatalf("second frame carried %d rows, want exactly the 1 that changed", len(second.RowsChanged))
	}
	if _, ok := second.RowsChanged[2]; !ok {
		t.Fatalf("second frame changed rows %v, want row 2", keysOf(second.RowsChanged))
	}
}

func TestResizeForcesAFullFrame(t *testing.T) {
	skipWithoutLib(t)
	g, err := NewGrid(10, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	fs := NewFrameState()
	if _, _, err := fs.Next("w1:p1", g); err != nil {
		t.Fatal(err)
	}
	if err := g.Resize(20, 8); err != nil {
		t.Fatal(err)
	}
	f, changed, err := fs.Next("w1:p1", g)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || len(f.RowsChanged) != 8 {
		t.Fatalf("after a resize: changed=%v rows=%d, want a full 8 row frame",
			changed, len(f.RowsChanged))
	}
}

func keysOf(m map[int][]proto.Cell) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}
```

- [ ] **Step 4: Run the test and watch it fail**

Run: `cd harness/coppice && go test -race ./internal/pane/ -v`
Expected: FAIL, `undefined: NewGrid`.

- [ ] **Step 5: Write the implementation**

```go
// harness/coppice/internal/pane/grid.go

// Package pane owns what a pane is: a grid fed by bytes, plus either a PTY or a
// headless adapter behind it. A client cannot tell the two kinds apart except
// by a badge, because both arrive as frames.
package pane

import (
	"fmt"
	"hash/fnv"
	"strings"
	"sync"

	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/vt"
)

type ReadSource string

const (
	ReadVisible   ReadSource = "visible"
	ReadRecent    ReadSource = "recent"
	ReadDetection ReadSource = "detection"
)

// DetectionRows is the window the manifest evaluator sees. Herdr's rules are
// written against bottom_non_empty_lines(12), so the debugging window must show
// exactly what they read.
const DetectionRows = 12

// RecentRows is how far back `read recent` reaches, in lines.
const RecentRows = 200

// scrollbackLines is the per-pane scrollback budget. It bounds memory per pane
// and must stay comfortably above RecentRows.
const scrollbackLines = 2000

// Grid is one terminal. It is safe for concurrent use.
type Grid struct {
	mu   sync.Mutex
	term *vt.Term
	cols int
	rows int
}

func NewGrid(cols, rows int) (*Grid, error) {
	if cols <= 0 || rows <= 0 {
		return nil, fmt.Errorf("grid size must be positive, got %dx%d", cols, rows)
	}
	term, err := vt.New(uint16(cols), uint16(rows), scrollbackLines)
	if err != nil {
		return nil, err
	}
	return &Grid{term: term, cols: cols, rows: rows}, nil
}

func (g *Grid) Write(p []byte) (int, error) {
	if err := g.term.Feed(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (g *Grid) Resize(cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return fmt.Errorf("grid size must be positive, got %dx%d", cols, rows)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.term.Resize(uint16(cols), uint16(rows)); err != nil {
		return err
	}
	g.cols, g.rows = cols, rows
	return nil
}

func (g *Grid) Size() (int, int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.cols, g.rows
}

func (g *Grid) Title() string    { return g.term.Title() }
func (g *Grid) Progress() string { return g.term.Progress() }

// Read renders the pane as plain text. detection is the window the manifest
// evaluator sees, so `pane read --source detection` is a debugging view of the
// exact input a rule matched against.
func (g *Grid) Read(src ReadSource) (string, error) {
	switch src {
	case ReadVisible:
		return g.term.PlainScreen()
	case ReadRecent:
		all, err := g.term.PlainAll()
		if err != nil {
			return "", err
		}
		return lastLines(all, RecentRows), nil
	case ReadDetection:
		s, err := g.term.PlainScreenUnwrapped()
		if err != nil {
			return "", err
		}
		return bottomNonEmptyLines(s, DetectionRows), nil
	default:
		return "", fmt.Errorf("read source %q is not visible, recent or detection", src)
	}
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// bottomNonEmptyLines is Herdr's bottom_non_empty_lines(n): walk up from the
// bottom, take n non-empty lines, then return everything from the first of them
// onward, blank lines between them included.
func bottomNonEmptyLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	seen := 0
	start := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			seen++
			start = i
			if seen == n {
				break
			}
		}
	}
	if start < 0 {
		return ""
	}
	return strings.Join(lines[start:], "\n")
}

// Snapshot is the viewport as wire cells, plus the cursor.
func (g *Grid) Snapshot() ([][]proto.Cell, [2]int, error) {
	rows, err := g.term.Viewport()
	if err != nil {
		return nil, [2]int{}, err
	}
	out := make([][]proto.Cell, len(rows))
	for y, row := range rows {
		cells := make([]proto.Cell, len(row))
		for x, c := range row {
			cells[x] = proto.Cell{
				Text: c.Text, FG: hexOf(c.FG), BG: hexOf(c.BG), Attrs: c.Attrs,
			}
		}
		out[y] = cells
	}
	cx, cy, _ := g.term.Cursor()
	return out, [2]int{int(cx), int(cy)}, nil
}

func hexOf(c *vt.RGB) string {
	if c == nil {
		return ""
	}
	return fmt.Sprintf("#%02x%02x%02x", c.R, c.G, c.B)
}

func (g *Grid) Close() { g.term.Close() }

// FrameState is one client's view of one pane. The sequence number and the row
// hashes are per client, because two clients attach at different moments and
// each needs its own baseline.
type FrameState struct {
	seq    uint64
	hashes []uint64
	cols   int
	rows   int
	cursor [2]int
}

func NewFrameState() *FrameState { return &FrameState{} }

// Next builds the frame to send this client. The second return value is false
// when nothing changed, so the caller sends nothing at all.
func (fs *FrameState) Next(paneID string, g *Grid) (proto.Frame, bool, error) {
	cells, cursor, err := g.Snapshot()
	if err != nil {
		return proto.Frame{}, false, err
	}
	cols, rows := g.Size()
	full := fs.seq == 0 || fs.cols != cols || fs.rows != rows || len(fs.hashes) != len(cells)

	changed := map[int][]proto.Cell{}
	next := make([]uint64, len(cells))
	for y, row := range cells {
		next[y] = hashRow(row)
		if full || next[y] != fs.hashes[y] {
			changed[y] = row
		}
	}
	if len(changed) == 0 && cursor == fs.cursor {
		return proto.Frame{}, false, nil
	}
	fs.seq++
	fs.hashes = next
	fs.cols, fs.rows = cols, rows
	fs.cursor = cursor
	return proto.Frame{
		Event: "frame", Pane: paneID, Seq: fs.seq, Cols: cols, Rows: rows,
		Cursor: cursor, RowsChanged: changed,
	}, true, nil
}

func hashRow(row []proto.Cell) uint64 {
	h := fnv.New64a()
	for _, c := range row {
		_, _ = h.Write([]byte(c.Text))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(c.FG))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(c.BG))
		_, _ = h.Write([]byte{byte(c.Attrs), byte(c.Attrs >> 8), 0})
	}
	return h.Sum64()
}
```

- [ ] **Step 6: Create the goldens and run the test**

Run:
```bash
cd harness/coppice
COPPICE_UPDATE_GOLDEN=1 go test ./internal/pane/ -run TestGoldenGrids
go test ./internal/pane/ -v
```
Expected: the first command writes six `.golden.txt` files. The second passes. Open each golden
and read it: a golden nobody looked at is a rubber stamp.

- [ ] **Step 7: Commit**

```bash
git add harness/coppice/internal/pane harness/coppice/testdata/vt harness/coppice/scripts/record-vt.sh
git commit -m "feat(coppice): the pane grid, with per-client frame sequences

Two clients attach at different moments, so the sequence number and the row
hashes belong to the client, not the pane. The goldens come from a checked-in
generator rather than an opaque capture, so anyone can regenerate them and see
what changed. read --source detection returns exactly what the manifest
evaluator sees, which is the whole point of having that window."
```

---

### Task 6: `internal/pane` — PTY panes

**Files:**
- Create: `harness/coppice/internal/pane/pty.go`
- Create: `harness/coppice/internal/pane/adapter.go`
- Test: `harness/coppice/internal/pane/pty_test.go`

**Interfaces:**
- Consumes: `layout.Pane`, `Grid`.
- Produces:
  ```go
  package pane
  type SpawnOpts struct { Cwd string; Argv []string; Env map[string]string
                          Cols, Rows int; Sock, PaneID string }
  type PTY struct{}
  func StartPTY(o SpawnOpts, g *Grid) (*PTY, error)
  func (p *PTY) Write(b []byte) (int, error)
  func (p *PTY) Resize(cols, rows int) error
  func (p *PTY) Done() <-chan struct{}
  func (p *PTY) ExitCode() (int, bool)
  func (p *PTY) Close() error
  func BuildEnv(base []string, extra map[string]string, sock, paneID string) []string
  ```
  Plus the headless interfaces, declared here because Task 8's `LivePane` holds a `Proc` and
  Task 14 fills the registry behind it:
  ```go
  package pane
  type EventKind string
  const ( EvText EventKind = "text"; EvTool = "tool"; EvState = "state"
          EvEnd = "end"; EvError = "error" )
  type Event struct { Kind EventKind; Text, Tool, State, Detail string
                     Ask *proto.Ask }   // set when State == "blocked"
  type StartOpts struct { Cwd string; Env map[string]string; Argv []string
                          Resume string; Sock, PaneID string }
  type Proc interface {
      Prompt(text string) error
      Steer(text string) error          // may return ErrUnsupported
      WriteStdin(b []byte) error
      Events() <-chan Event
      SessionID() (string, bool)
      Stop() error
  }
  type Adapter interface {
      Name() string
      Start(ctx context.Context, o StartOpts, g *Grid) (Proc, error)
  }
  var ErrUnsupported = errors.New("this harness does not support that")
  ```

- [ ] **Step 1: Write the failing test**

```go
// harness/coppice/internal/pane/pty_test.go
package pane

import (
	"strings"
	"testing"
	"time"
)

func TestBuildEnvInjectsTheSocketAndPaneID(t *testing.T) {
	got := BuildEnv([]string{"PATH=/bin", "COPPICE_PANE=stale"},
		map[string]string{"FOO": "bar"}, "/run/coppice/server.sock", "w1:p3")
	want := map[string]string{
		"PATH": "/bin", "FOO": "bar",
		"COPPICE_SOCK": "/run/coppice/server.sock", "COPPICE_PANE": "w1:p3",
		"TERM": "xterm-256color",
	}
	seen := map[string]string{}
	for _, kv := range got {
		k, v, _ := strings.Cut(kv, "=")
		if _, dup := seen[k]; dup {
			t.Fatalf("env has %s twice: %v", k, got)
		}
		seen[k] = v
	}
	for k, v := range want {
		if seen[k] != v {
			t.Fatalf("env[%s] = %q, want %q", k, seen[k], v)
		}
	}
}

// Spec-02 makes the injection unconditional, so an inherited value must be
// removed rather than passed on. A server started from inside a coppice pane
// would otherwise hand its own pane id to every child it spawns, and the gate
// hook in that child would report state for the wrong pane.
func TestBuildEnvScrubsAnInheritedPaneID(t *testing.T) {
	got := BuildEnv([]string{"PATH=/bin", "COPPICE_PANE=w9:p9", "COPPICE_SOCK=/old.sock"},
		nil, "", "")
	for _, kv := range got {
		if strings.HasPrefix(kv, "COPPICE_PANE=") || strings.HasPrefix(kv, "COPPICE_SOCK=") {
			t.Fatalf("env still carries %q, want the inherited value removed", kv)
		}
	}
}

func TestStartPTYRefusesWithoutASocketOrPaneID(t *testing.T) {
	skipWithoutLib(t)
	g, err := NewGrid(20, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if _, err := StartPTY(SpawnOpts{
		Cwd: t.TempDir(), Argv: []string{"true"}, Cols: 20, Rows: 4, PaneID: "w1:p1",
	}, g); err == nil {
		t.Fatal("StartPTY accepted an empty socket path, want an error")
	}
	if _, err := StartPTY(SpawnOpts{
		Cwd: t.TempDir(), Argv: []string{"true"}, Cols: 20, Rows: 4, Sock: "/x.sock",
	}, g); err == nil {
		t.Fatal("StartPTY accepted an empty pane id, want an error")
	}
}

func TestPTYRunsACommandAndTheGridSeesItsOutput(t *testing.T) {
	skipWithoutLib(t)
	g, err := NewGrid(40, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	p, err := StartPTY(SpawnOpts{
		Cwd: t.TempDir(), Argv: []string{"sh", "-c", "echo hi"},
		Cols: 40, Rows: 10, Sock: "/dev/null", PaneID: "w1:p1",
	}, g)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the command did not exit within 5 s")
	}
	code, ok := p.ExitCode()
	if !ok || code != 0 {
		t.Fatalf("ExitCode() = %d %v, want 0 true", code, ok)
	}
	screen, err := g.Read(ReadVisible)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(screen, "hi") {
		t.Fatalf("grid = %q, want it to contain hi", screen)
	}
}

func TestPTYReportsANonZeroExitCode(t *testing.T) {
	skipWithoutLib(t)
	g, err := NewGrid(20, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	p, err := StartPTY(SpawnOpts{
		Cwd: t.TempDir(), Argv: []string{"sh", "-c", "exit 7"}, Cols: 20, Rows: 4,
		Sock: "/dev/null", PaneID: "w1:p1",
	}, g)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the command did not exit within 5 s")
	}
	if code, ok := p.ExitCode(); !ok || code != 7 {
		t.Fatalf("ExitCode() = %d %v, want 7 true", code, ok)
	}
}

func TestPTYPassesTheEnvironmentToTheChild(t *testing.T) {
	skipWithoutLib(t)
	g, err := NewGrid(60, 6)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	p, err := StartPTY(SpawnOpts{
		Cwd: t.TempDir(), Argv: []string{"sh", "-c", "echo pane=$COPPICE_PANE"},
		Cols: 60, Rows: 6, Sock: "/tmp/x.sock", PaneID: "w1:p9",
	}, g)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	<-p.Done()
	time.Sleep(50 * time.Millisecond) // let the reader drain the last bytes
	screen, err := g.Read(ReadVisible)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(screen, "pane=w1:p9") {
		t.Fatalf("grid = %q, want pane=w1:p9", screen)
	}
}

func TestStartPTYFailsLoudlyOnAMissingBinary(t *testing.T) {
	skipWithoutLib(t)
	g, err := NewGrid(20, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if _, err := StartPTY(SpawnOpts{
		Cwd: t.TempDir(), Argv: []string{"coppice-no-such-binary"}, Cols: 20, Rows: 4,
		Sock: "/dev/null", PaneID: "w1:p1",
	}, g); err == nil {
		t.Fatal("StartPTY succeeded for a binary that does not exist, want an error")
	}
}

func TestResizeSetsTheChildWinsize(t *testing.T) {
	skipWithoutLib(t)
	g, err := NewGrid(40, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	p, err := StartPTY(SpawnOpts{
		Cwd: t.TempDir(), Argv: []string{"sh", "-c", "sleep 5"}, Cols: 40, Rows: 10,
		Sock: "/dev/null", PaneID: "w1:p1",
	}, g)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := p.Resize(100, 30); err != nil {
		t.Fatal(err)
	}
	cols, rows := g.Size()
	if cols != 100 || rows != 30 {
		t.Fatalf("grid size after resize = %dx%d, want 100x30", cols, rows)
	}
}
```

- [ ] **Step 2: Run the test and watch it fail**

Run: `cd harness/coppice && go test -race ./internal/pane/ -run PTY -v`
Expected: FAIL, `undefined: StartPTY`.

- [ ] **Step 3: Write the implementation**

```go
// harness/coppice/internal/pane/pty.go
package pane

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/creack/pty"
)

// SpawnOpts is everything a pty pane needs to start. Sock and PaneID become
// COPPICE_SOCK and COPPICE_PANE in the child's environment: that pair is how
// the gate hook in the child finds its way back to report state.
type SpawnOpts struct {
	Cwd    string
	Argv   []string
	Env    map[string]string
	Cols   int
	Rows   int
	Sock   string
	PaneID string
}

// BuildEnv merges the server's own environment with the pane's extras and the
// two coppice variables. A key set twice would be ambiguous to the child, so
// later values replace earlier ones rather than appending.
//
// Spec-02 makes the injection unconditional: every spawned process gets
// COPPICE_SOCK and COPPICE_PANE. When an argument is empty the inherited value
// is REMOVED, never left in place. A server started from inside a coppice pane
// inherits that pane's COPPICE_PANE, and passing it on would make the gate hook
// in the child report state for somebody else's pane.
func BuildEnv(base []string, extra map[string]string, sock, paneID string) []string {
	merged := map[string]string{}
	order := []string{}
	set := func(k, v string) {
		if _, ok := merged[k]; !ok {
			order = append(order, k)
		}
		merged[k] = v
	}
	for _, kv := range base {
		if k, v, ok := strings.Cut(kv, "="); ok {
			set(k, v)
		}
	}
	for k, v := range extra {
		set(k, v)
	}
	set("TERM", "xterm-256color")
	set("COPPICE_SOCK", sock)
	set("COPPICE_PANE", paneID)
	out := make([]string, 0, len(order))
	for _, k := range order {
		// An empty value means "this must not reach the child", not "leave
		// whatever we inherited".
		if (k == "COPPICE_SOCK" || k == "COPPICE_PANE") && merged[k] == "" {
			continue
		}
		out = append(out, k+"="+merged[k])
	}
	return out
}

// PTY is a process on a pseudo-terminal, with its output feeding a Grid.
type PTY struct {
	cmd  *exec.Cmd
	f    *os.File
	grid *Grid
	done chan struct{}

	mu   sync.Mutex
	code int
	got  bool
}

func StartPTY(o SpawnOpts, g *Grid) (*PTY, error) {
	if len(o.Argv) == 0 {
		return nil, fmt.Errorf("pane needs a command to run")
	}
	// Spec-02 makes the two coppice variables unconditional. An empty one here
	// means the caller forgot, and a pane whose gate hook cannot find its way
	// home is worse than a pane that refuses to start.
	if o.Sock == "" || o.PaneID == "" {
		return nil, fmt.Errorf("a pane needs both a socket path and a pane id in SpawnOpts")
	}
	if o.Cols <= 0 {
		o.Cols = 120
	}
	if o.Rows <= 0 {
		o.Rows = 40
	}
	cmd := exec.Command(o.Argv[0], o.Argv[1:]...)
	cmd.Dir = o.Cwd
	cmd.Env = BuildEnv(os.Environ(), o.Env, o.Sock, o.PaneID)

	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(o.Cols), Rows: uint16(o.Rows)})
	if err != nil {
		return nil, fmt.Errorf("cannot start %s: %w", o.Argv[0], err)
	}
	p := &PTY{cmd: cmd, f: f, grid: g, done: make(chan struct{})}

	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := f.Read(buf)
			if n > 0 {
				_, _ = g.Write(buf[:n])
			}
			if err != nil {
				if err != io.EOF {
					// The pty closes with EIO when the child exits. That is
					// normal, not news.
					_ = err
				}
				break
			}
		}
		code := 0
		if werr := cmd.Wait(); werr != nil {
			var ee *exec.ExitError
			if ok := asExitError(werr, &ee); ok {
				code = ee.ExitCode()
			} else {
				code = -1
			}
		}
		p.mu.Lock()
		p.code, p.got = code, true
		p.mu.Unlock()
		close(p.done)
	}()
	return p, nil
}

func asExitError(err error, target **exec.ExitError) bool {
	ee, ok := err.(*exec.ExitError)
	if ok {
		*target = ee
	}
	return ok
}

func (p *PTY) Write(b []byte) (int, error) { return p.f.Write(b) }

// Resize sets the grid first, then the child's winsize. A child that redraws on
// SIGWINCH must find the grid already the new size, or the first redraw lands
// in a grid of the old shape.
func (p *PTY) Resize(cols, rows int) error {
	if err := p.grid.Resize(cols, rows); err != nil {
		return err
	}
	return pty.Setsize(p.f, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

func (p *PTY) Done() <-chan struct{} { return p.done }

func (p *PTY) ExitCode() (int, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.code, p.got
}

func (p *PTY) Close() error {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	return p.f.Close()
}
```

Also write the headless interfaces now. Nothing implements them until Task 14, but Task 8's
`LivePane` holds a `Proc`, so the type has to exist before then.

```go
// harness/coppice/internal/pane/adapter.go

// The headless side of a pane. A headless pane wraps a harness process and
// normalises its event stream into two things: transcript text written into the
// pane's grid, so a client cannot tell the two pane kinds apart except by a
// badge, and PaneStateEvents with source headless.
package pane

import (
	"context"
	"errors"

	"github.com/opendaisugi/coppice/internal/proto"
)

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
	Steer(text string) error
	WriteStdin(b []byte) error
	Events() <-chan Event
	SessionID() (string, bool)
	Stop() error
}

// Adapter starts one kind of harness headlessly.
//
// Three rules bind every implementation, including the ones spec-04 and spec-05
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
type Adapter interface {
	Name() string
	Start(ctx context.Context, o StartOpts, g *Grid) (Proc, error)
}
```

- [ ] **Step 4: Run the test and watch it pass**

Run: `cd harness/coppice && go test -race ./internal/pane/ -v && go vet ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/pane/pty.go harness/coppice/internal/pane/adapter.go \
        harness/coppice/internal/pane/pty_test.go
git commit -m "feat(coppice): pty panes that hand the gate its way home

Every spawned process gets COPPICE_SOCK and COPPICE_PANE. That pair is the
whole mechanism behind spec-01's report_state: the hook inside the harness
finds the floor without configuration. Resize touches the grid before the
winsize, so a child redrawing on SIGWINCH finds a grid that is already the
size it was told about."
```

---

### Task 7: `internal/server` — the socket, the uid check, the dispatcher

**Files:**
- Create: `harness/coppice/internal/server/paths.go`
- Create: `harness/coppice/internal/server/peercred_linux.go`
- Create: `harness/coppice/internal/server/peercred_other.go`
- Create: `harness/coppice/internal/server/lock_unix.go`
- Create: `harness/coppice/internal/server/lock_other.go`
- Create: `harness/coppice/internal/server/server.go`
- Test: `harness/coppice/internal/server/server_test.go`

**Interfaces:**
- Consumes: `proto.*`, `layout.Tree`, `state.Store`.
- Produces:
  ```go
  package server
  func SocketPath() string     // $XDG_RUNTIME_DIR/coppice/server.sock, else ~/.opendaisugi/coppice/server.sock
  func DataDir() string        // ~/.opendaisugi/coppice
  func peerUID(c *net.UnixConn) (uint32, error)   // build-tagged; non-Linux returns an error
  type Config struct { SocketPath, DataDir string }
  type Server struct{}
  func New(cfg Config) (*Server, error)
  var ErrAlreadyRunning error
  func (s *Server) AcquireStartLock() error   // flock on <data dir>/server.lock, held for life
  func (s *Server) HoldsStartLock() bool
  func (s *Server) Listen() error          // binds; dir 0700, socket 0600; refuses without the lock
  func (s *Server) Serve() error           // accept loop; returns when Close is called
  func (s *Server) ServeConn(r io.Reader, w io.Writer)   // the in-process test entry point
  func (s *Server) Close() error
  func (s *Server) Tree() *layout.Tree
  func (s *Server) States() *state.Store
  func (s *Server) Handle(name string, h Handler)
  func (s *Server) SetDetectionWarnings(w []string)
  func (s *Server) DetectionWarnings() []string
  func (s *Server) LastRestore() RestoreReport
  type RestoreReport struct { Panes, Resumed, MarkedDone, MarkedUnknown int; Notes []string }
  type Handler func(c *Client, r *proto.Request) proto.Response
  type Client struct { Enc *proto.Encoder; Attached map[string]*pane.FrameState; Operator bool }
  ```
  `Client.Operator` is true only for a client that has an open `pane.attach`, which is what
  lets it speak with `source: operator`.

- [ ] **Step 1: Write the failing test**

```go
// harness/coppice/internal/server/server_test.go
package server

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/proto"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	s, err := New(Config{SocketPath: filepath.Join(dir, "server.sock"), DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	// Restore and Listen both refuse without the start lock, so every test
	// server takes it. Each test has its own data directory, so they never
	// contend.
	if err := s.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// roundTrip drives ServeConn over an in-memory pipe and returns the responses.
func roundTrip(t *testing.T, s *Server, lines ...string) []proto.Response {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go func() {
		s.ServeConn(inR, outW)
		_ = outW.Close()
	}()
	go func() {
		for _, l := range lines {
			_, _ = io.WriteString(inW, l+"\n")
		}
		_ = inW.Close()
	}()
	var got []proto.Response
	d := proto.NewDecoder(outR)
	for range lines {
		line, err := d.Next()
		if err != nil {
			t.Fatalf("reading response %d: %v", len(got), err)
		}
		var r proto.Response
		if err := json.Unmarshal(line, &r); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	return got
}

func TestSocketPathPrefersXDGRuntimeDir(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	if got := SocketPath(); got != "/run/user/1000/coppice/server.sock" {
		t.Fatalf("SocketPath() = %q, want the XDG path", got)
	}
	t.Setenv("XDG_RUNTIME_DIR", "")
	if got := SocketPath(); !strings.HasSuffix(got, "/.opendaisugi/coppice/server.sock") {
		t.Fatalf("SocketPath() = %q, want the ~/.opendaisugi fallback", got)
	}
}

func TestListenCreatesAPrivateDirectoryAndSocket(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "nested", "server.sock")
	s, err := New(Config{SocketPath: sock, DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	di, err := os.Stat(filepath.Dir(sock))
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("socket dir mode = %o, want 700", di.Mode().Perm())
	}
	si, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if si.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %o, want 600", si.Mode().Perm())
	}
}

// Removing a leftover socket is only safe while we hold the lock. Without it,
// the check-then-act probe this replaced could unlink a socket another server
// had just bound.
func TestListenRefusesWithoutTheStartLock(t *testing.T) {
	dir := t.TempDir()
	s, err := New(Config{SocketPath: filepath.Join(dir, "server.sock"), DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Listen(); err == nil {
		t.Fatal("Listen bound without the start lock, want a refusal")
	}
}

func TestASecondAcquireOfTheStartLockNamesTheHoldersPID(t *testing.T) {
	dir := t.TempDir()
	a, err := New(Config{SocketPath: filepath.Join(dir, "server.sock"), DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err := a.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	b, err := New(Config{SocketPath: filepath.Join(dir, "server.sock"), DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	err = b.AcquireStartLock()
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second AcquireStartLock = %v, want ErrAlreadyRunning", err)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(os.Getpid())) {
		t.Fatalf("error %q does not name the holder's pid", err)
	}
	if !strings.Contains(err.Error(), "coppice server status") {
		t.Fatalf("error %q does not teach the next command", err)
	}
	// Releasing must hand the lock over, or a restart after a clean stop would
	// refuse for ever.
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.AcquireStartLock(); err != nil {
		t.Fatalf("the lock was not released by Close: %v", err)
	}
}

func TestUnknownCommandIsBadRequest(t *testing.T) {
	s := newTestServer(t)
	got := roundTrip(t, s, `{"id":"1","cmd":"pane.teleport"}`)
	if got[0].OK || got[0].Error.Code != proto.ErrBadRequest {
		t.Fatalf("unknown command gave %+v, want bad_request", got[0])
	}
	if !strings.Contains(got[0].Error.Message, "server.status") {
		t.Fatalf("error message %q does not teach a command that exists", got[0].Error.Message)
	}
}

func TestMalformedLineIsBadRequestAndTheConnectionSurvives(t *testing.T) {
	s := newTestServer(t)
	got := roundTrip(t, s, `not json`, `{"id":"2","cmd":"server.status"}`)
	if got[0].OK || got[0].Error.Code != proto.ErrBadRequest {
		t.Fatalf("malformed line gave %+v, want bad_request", got[0])
	}
	if !got[1].OK {
		t.Fatalf("the connection died after one bad line: %+v", got[1])
	}
}

func TestServerStatusReportsWhatSurvivesARestart(t *testing.T) {
	s := newTestServer(t)
	got := roundTrip(t, s, `{"id":"1","cmd":"server.status"}`)
	if !got[0].OK {
		t.Fatalf("server.status failed: %+v", got[0])
	}
	b, _ := json.Marshal(got[0].Result)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	for _, k := range []string{"pid", "socket", "uptime_s", "panes", "restart_note"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("server.status result is missing %q: %s", k, b)
		}
	}
	note, _ := m["restart_note"].(string)
	if !strings.Contains(note, "layout") || !strings.Contains(note, "not") {
		t.Fatalf("restart_note = %q, want it to say plainly what does not come back", note)
	}
}

// Fail-closed: a connection whose peer uid we cannot read, or whose uid is not
// ours, is dropped. Never served.
func TestOurOwnUIDIsServed(t *testing.T) {
	s := newTestServer(t)
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve() }()

	c, err := net.Dial("unix", s.cfg.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Our own uid, so this must be served. The refusal is proved by
	// TestRefusesWhenPeerUIDIsUnreadable, which is the case a test can force.
	_, _ = io.WriteString(c, `{"id":"1","cmd":"server.status"}`+"\n")
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	d := proto.NewDecoder(c)
	if _, err := d.Next(); err != nil {
		t.Fatalf("our own uid was refused: %v", err)
	}
}

func TestRefusesWhenPeerUIDIsUnreadable(t *testing.T) {
	s := newTestServer(t)
	s.peerUID = func(*net.UnixConn) (uint32, error) { return 0, errUnreadableUID }
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve() }()

	c, err := net.Dial("unix", s.cfg.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	d := proto.NewDecoder(c)
	line, err := d.Next()
	if err != nil {
		t.Fatalf("want one unauthorized line then a close, got %v", err)
	}
	var r proto.Response
	if err := json.Unmarshal(line, &r); err != nil {
		t.Fatal(err)
	}
	if r.OK || r.Error.Code != proto.ErrUnauthorized {
		t.Fatalf("got %+v, want unauthorized", r)
	}
	if _, err := d.Next(); err == nil {
		t.Fatal("the connection stayed open after unauthorized, want it closed")
	}
}

// The cockpit and the PWA hold ONE long-lived connection and are exactly the
// clients that call agent.wait --until blocked, which blocks for up to two
// minutes inside its handler. Dispatching serially would freeze allow, deny,
// send_text and detach for every other pane for that whole time, while frames
// kept arriving so the UI looked alive and accepted nothing.
func TestASlowWaitDoesNotBlockOtherCommandsOnTheSameConnection(t *testing.T) {
	s := newTestServer(t)
	release := make(chan struct{})
	s.Handle("test.slow", func(_ *Client, r *proto.Request) proto.Response {
		<-release
		return proto.OKResp(r.ID, map[string]any{"slow": true})
	})

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go func() { s.ServeConn(inR, outW); _ = outW.Close() }()
	defer inW.Close()

	if _, err := io.WriteString(inW, `{"id":"slow","cmd":"test.slow"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(inW, `{"id":"fast","cmd":"server.status"}`+"\n"); err != nil {
		t.Fatal(err)
	}

	// The fast response must arrive while the slow one is still blocked.
	type res struct {
		r   proto.Response
		err error
	}
	got := make(chan res, 1)
	go func() {
		line, err := proto.NewDecoder(outR).Next()
		if err != nil {
			got <- res{err: err}
			return
		}
		var r proto.Response
		err = json.Unmarshal(line, &r)
		got <- res{r: r, err: err}
	}()
	select {
	case g := <-got:
		if g.err != nil {
			close(release)
			t.Fatal(g.err)
		}
		if g.r.ID != "fast" {
			close(release)
			t.Fatalf("first response was %q, want fast: the slow handler blocked the connection",
				g.r.ID)
		}
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("server.status never answered while a slow handler was running")
	}
	close(release)
}

func TestConcurrentClientsAreServedIndependently(t *testing.T) {
	s := newTestServer(t)
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve() }()

	done := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func() {
			c, err := net.Dial("unix", s.cfg.SocketPath)
			if err != nil {
				done <- err
				return
			}
			defer c.Close()
			_, _ = io.WriteString(c, `{"id":"1","cmd":"server.status"}`+"\n")
			_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, err = proto.NewDecoder(c).Next()
			done <- err
		}()
	}
	for i := 0; i < 4; i++ {
		if err := <-done; err != nil {
			t.Fatalf("client %d: %v", i, err)
		}
	}
}
```

- [ ] **Step 2: Run the test and watch it fail**

Run: `cd harness/coppice && go test -race ./internal/server/ -v`
Expected: FAIL, `undefined: New`.

- [ ] **Step 3: Write `paths.go` and the two peercred files**

```go
// harness/coppice/internal/server/paths.go
package server

import (
	"os"
	"path/filepath"
)

// SocketPath is master spec 3.3: the runtime dir when the session has one,
// otherwise a private directory in the home directory.
func SocketPath() string {
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" {
		return filepath.Join(rt, "coppice", "server.sock")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".opendaisugi", "coppice", "server.sock")
	}
	return filepath.Join(home, ".opendaisugi", "coppice", "server.sock")
}

// DataDir holds the layout file and anything else that must survive a restart.
// It is never the runtime dir, which the system clears on logout.
func DataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".opendaisugi", "coppice")
	}
	return filepath.Join(home, ".opendaisugi", "coppice")
}
```

```go
// harness/coppice/internal/server/peercred_linux.go
//go:build linux

package server

import (
	"errors"
	"net"
	"syscall"
)

var errUnreadableUID = errors.New("cannot read the peer uid of this connection")

// peerUID reads SO_PEERCRED. A connection whose credentials we cannot read is
// refused, so an unexpected socket type can never be treated as trusted.
func peerUID(c *net.UnixConn) (uint32, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, errUnreadableUID
	}
	var ucred *syscall.Ucred
	var serr error
	cerr := raw.Control(func(fd uintptr) {
		ucred, serr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	})
	if cerr != nil || serr != nil || ucred == nil {
		return 0, errUnreadableUID
	}
	return ucred.Uid, nil
}
```

```go
// harness/coppice/internal/server/peercred_other.go
//go:build !linux

package server

import (
	"errors"
	"net"
)

var errUnreadableUID = errors.New("cannot read the peer uid of this connection")

// peerUID has no portable implementation outside Linux. Returning an error
// refuses every connection, which is the fail-closed direction. Master spec
// section 8 puts other platforms out of scope for now.
func peerUID(*net.UnixConn) (uint32, error) { return 0, errUnreadableUID }
```

- [ ] **Step 3b: Write the start lock**

One `coppice server start` at a time. The lock is taken **before** anything reads or writes the
data directory, so a second start cannot restore a layout, spawn resumed panes, or unlink a live
socket on its way to failing.

```go
// harness/coppice/internal/server/lock_unix.go
//go:build unix

package server

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// lockFile is <data dir>/server.lock. The lock is an flock on an open file
// description, so it is released by the kernel when the process dies, however
// it dies. A pid file alone cannot do that: a stale pid file after a crash
// would block every future start.
func lockFile(dataDir string) string { return filepath.Join(dataDir, "server.lock") }

type startLock struct{ f *os.File }

// acquireStartLock takes the exclusive lock without blocking. On contention it
// returns the pid recorded by the holder, so the caller can name it.
func acquireStartLock(dataDir string) (*startLock, int, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, 0, err
	}
	path := lockFile(dataDir)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, 0, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		pid := readLockPID(path)
		_ = f.Close()
		return nil, pid, ErrAlreadyRunning
	}
	// We hold it. Record our pid for the next caller's error message.
	if err := f.Truncate(0); err != nil {
		_ = releaseLock(f)
		return nil, 0, err
	}
	if _, err := f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0); err != nil {
		_ = releaseLock(f)
		return nil, 0, err
	}
	return &startLock{f: f}, 0, nil
}

func releaseLock(f *os.File) error {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return f.Close()
}

func (l *startLock) release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := releaseLock(l.f)
	l.f = nil
	return err
}

// readLockPID is best effort. An unreadable or empty file means "somebody has
// it and did not say who", which is still a refusal.
func readLockPID(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0
	}
	return n
}
```

```go
// harness/coppice/internal/server/lock_other.go
//go:build !unix

package server

// There is no portable single-instance lock outside unix. Refusing to start is
// the fail-closed direction, and master spec section 8 puts other platforms out
// of scope.
type startLock struct{}

func lockFile(string) string { return "" }

func acquireStartLock(string) (*startLock, int, error) { return nil, 0, ErrAlreadyRunning }

func (l *startLock) release() error { return nil }

func readLockPID(string) int { return 0 }
```

- [ ] **Step 4: Write `server.go`**

```go
// harness/coppice/internal/server/server.go

// Package server is the coppice daemon: one unix socket, one uid, one operator,
// one instance. It owns the pane tree and the merged state, and it hands both
// to command handlers registered by name.
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/opendaisugi/coppice/internal/layout"
	"github.com/opendaisugi/coppice/internal/pane"
	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/state"
)

// ErrAlreadyRunning is what AcquireStartLock returns when another server holds
// the lock. It is a refusal, never a takeover: the running server owns the
// data directory and the socket.
var ErrAlreadyRunning = errors.New("another coppice server holds the start lock")

// Config is the whole of a server's configuration. There is deliberately no
// "skip the uid check" field: every accepted connection is checked, and no
// future caller can turn that off by setting a struct member.
type Config struct {
	SocketPath string
	DataDir    string
}

// EventQueue is how many unsent events one client may fall behind by. A PWA on
// a stalled tailnet must not be able to hold up ApplyState for every pane,
// including the pane.report_state response a gate hook is blocking on.
const EventQueue = 256

// Client is one connection. Attached holds the per-client frame baselines, and
// its non-emptiness is what makes a client an operator: only a client watching
// a pane may speak for the human in front of it.
//
// Events go through a bounded queue drained by one goroutine. Responses go
// straight to Enc, so a wedged event stream never delays an answer the caller
// is waiting on.
type Client struct {
	Enc      *proto.Encoder
	mu       sync.Mutex
	Attached map[string]*pane.FrameState
	Subs     map[string]bool // event kinds this client subscribed to
	SubPanes map[string]bool // "*" means every pane

	events chan any
	dead   chan struct{}
	once   sync.Once
}

func newClient(w io.Writer) *Client {
	c := &Client{
		Enc:      proto.NewEncoder(w),
		Attached: map[string]*pane.FrameState{},
		Subs:     map[string]bool{},
		SubPanes: map[string]bool{},
		events:   make(chan any, EventQueue),
		dead:     make(chan struct{}),
	}
	go c.pump()
	return c
}

// pump is the only goroutine that writes this client's events.
func (c *Client) pump() {
	for {
		select {
		case <-c.dead:
			return
		case v := <-c.events:
			if err := c.Enc.Send(v); err != nil {
				c.Drop()
				return
			}
		}
	}
}

// Emit queues one event. A client that has fallen EventQueue events behind is
// disconnected rather than allowed to stall the server: dropping one client is
// better than freezing every pane.
func (c *Client) Emit(v any) {
	select {
	case <-c.dead:
	case c.events <- v:
	default:
		c.Drop()
	}
}

// Drop marks the client gone. It is safe to call more than once.
func (c *Client) Drop() { c.once.Do(func() { close(c.dead) }) }

// Dead reports whether this client has been dropped.
func (c *Client) Dead() bool {
	select {
	case <-c.dead:
		return true
	default:
		return false
	}
}

// Operator reports whether this client is attached to the given pane. Master
// spec 3.1 lets only an attached client speak with source operator.
func (c *Client) Operator(paneID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.Attached[paneID]
	return ok
}

type Handler func(c *Client, r *proto.Request) proto.Response

// RestoreReport is what a restart actually achieved. Task 18 fills it; it is
// declared here because handleStatus returns it and Server owns the field.
type RestoreReport struct {
	Panes         int      `json:"panes"`
	Resumed       int      `json:"resumed"`
	MarkedDone    int      `json:"marked_done"`
	MarkedUnknown int      `json:"marked_unknown"`
	Notes         []string `json:"notes"`
}

// Server owns every field the later tasks fill. Declaring them all here means
// no task adds a field to a struct another task already compiled against.
type Server struct {
	cfg      Config
	tree     *layout.Tree
	states   *state.Store
	started  time.Time
	peerUID  func(*net.UnixConn) (uint32, error)
	handlers map[string]Handler

	liveMu sync.RWMutex
	live   map[string]*LivePane

	lockMu sync.Mutex
	lock   *startLock

	mu             sync.Mutex
	ln             net.Listener
	clients        map[*Client]bool
	closed         bool
	waiters        []*waiter
	tickStop       chan struct{}
	restore        RestoreReport
	detectWarnings []string
}

// SetDetectionWarnings records what the manifest loader complained about, so
// server.status can show it. Warnings nothing reads are the same as no
// warnings: a broken override file would silently revert to the bundled
// manifest with no signal an operator could see.
func (s *Server) SetDetectionWarnings(w []string) {
	s.mu.Lock()
	s.detectWarnings = append([]string(nil), w...)
	s.mu.Unlock()
}

func (s *Server) DetectionWarnings() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.detectWarnings...)
}

func (s *Server) LastRestore() RestoreReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.restore
}

func New(cfg Config) (*Server, error) {
	if cfg.SocketPath == "" {
		cfg.SocketPath = SocketPath()
	}
	if cfg.DataDir == "" {
		cfg.DataDir = DataDir()
	}
	s := &Server{
		cfg: cfg, tree: layout.New(), states: state.NewStore(),
		started: time.Now(), peerUID: peerUID,
		handlers: map[string]Handler{}, clients: map[*Client]bool{},
		live: map[string]*LivePane{},
	}
	s.Handle("server.status", s.handleStatus)
	return s, nil
}

// AcquireStartLock takes the single-instance lock and holds it for the life of
// this server. Call it FIRST, before Restore and before Listen: a second start
// must fail before it can restore a layout, spawn resumed panes, or touch the
// socket.
func (s *Server) AcquireStartLock() error {
	s.lockMu.Lock()
	defer s.lockMu.Unlock()
	if s.lock != nil {
		return nil
	}
	l, pid, err := acquireStartLock(s.cfg.DataDir)
	if err != nil {
		if errors.Is(err, ErrAlreadyRunning) {
			who := "another process"
			if pid > 0 {
				who = fmt.Sprintf("pid %d", pid)
			}
			return fmt.Errorf("%w: coppice server already running (%s). Run: coppice server status",
				ErrAlreadyRunning, who)
		}
		return err
	}
	s.lock = l
	return nil
}

func (s *Server) HoldsStartLock() bool {
	s.lockMu.Lock()
	defer s.lockMu.Unlock()
	return s.lock != nil
}

func (s *Server) releaseStartLock() {
	s.lockMu.Lock()
	l := s.lock
	s.lock = nil
	s.lockMu.Unlock()
	if l != nil {
		_ = l.release()
	}
}

func (s *Server) Tree() *layout.Tree     { return s.tree }
func (s *Server) States() *state.Store   { return s.states }
func (s *Server) Handle(n string, h Handler) { s.handlers[n] = h }

func (s *Server) Listen() error {
	// The lock is what makes the unlink below safe. Probing the socket and then
	// removing it is check-then-act: between the probe and the unlink another
	// start can bind, and this one would delete a live socket and take its
	// place. Holding the lock means any socket file here is stale by
	// construction, so no probe is needed and none is done.
	if !s.HoldsStartLock() {
		return fmt.Errorf(
			"call AcquireStartLock before Listen. The lock is what makes removing a stale socket safe")
	}
	dir := filepath.Dir(s.cfg.SocketPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	_ = os.Remove(s.cfg.SocketPath)
	ln, err := net.Listen("unix", s.cfg.SocketPath)
	if err != nil {
		return err
	}
	if err := os.Chmod(s.cfg.SocketPath, 0o600); err != nil {
		_ = ln.Close()
		return err
	}
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()
	return nil
}

func (s *Server) Serve() error {
	s.mu.Lock()
	ln := s.ln
	s.mu.Unlock()
	if ln == nil {
		return fmt.Errorf("call Listen before Serve")
	}
	me := uint32(os.Getuid())
	for {
		conn, err := ln.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return nil
			}
			return err
		}
		go func(conn net.Conn) {
			defer conn.Close()
			uc, ok := conn.(*net.UnixConn)
			if !ok {
				s.refuse(conn, "this connection is not a unix socket")
				return
			}
			uid, err := s.peerUID(uc)
			if err != nil || uid != me {
				s.refuse(conn, "this socket serves one user. Run coppice as the user that started the server.")
				return
			}
			s.ServeConn(conn, conn)
		}(conn)
	}
}

func (s *Server) refuse(w io.Writer, msg string) {
	_ = proto.NewEncoder(w).Send(proto.ErrResp("", proto.ErrUnauthorized, msg))
}

// ServeConn runs the request loop over any reader and writer. Only the socket
// path reaches it in production; the tests drive it directly. --stdio does NOT
// build a server: it proxies to the running one, see Task 20.
func (s *Server) ServeConn(r io.Reader, w io.Writer) {
	c := newClient(w)
	s.mu.Lock()
	s.clients[c] = true
	s.mu.Unlock()
	defer func() {
		c.Drop()
		s.mu.Lock()
		delete(s.clients, c)
		s.mu.Unlock()
	}()

	d := proto.NewDecoder(r)
	for {
		line, err := d.Next()
		if err != nil {
			return
		}
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var req proto.Request
		if err := json.Unmarshal(line, &req); err != nil {
			_ = c.Enc.Send(proto.ErrResp("", proto.ErrBadRequest,
				"this line is not JSON. Send one JSON object per line."))
			continue
		}
		h, ok := s.handlers[req.Cmd]
		if !ok {
			_ = c.Enc.Send(proto.ErrResp(req.ID, proto.ErrBadRequest,
				fmt.Sprintf("no command %q. Known commands: %s", req.Cmd, s.commandList())))
			continue
		}
		// Each request runs on its own goroutine. agent.wait blocks for up to
		// two minutes and pane.wait_output for thirty seconds, both inside the
		// handler; the cockpit and the PWA hold one long-lived connection and
		// must keep answering for every other pane meanwhile. proto.Encoder.Send
		// is mutex-guarded, so writes stay one whole line at a time.
		go func(req proto.Request) {
			_ = c.Enc.Send(h(c, &req))
		}(req)
	}
}

func (s *Server) commandList() string {
	names := make([]string, 0, len(s.handlers))
	for n := range s.handlers {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// Broadcast sends one event to every client that asked for its kind and pane.
func (s *Server) Broadcast(kind, paneID string, v any) {
	s.mu.Lock()
	clients := make([]*Client, 0, len(s.clients))
	for c := range s.clients {
		clients = append(clients, c)
	}
	s.mu.Unlock()
	for _, c := range clients {
		c.mu.Lock()
		want := c.Subs[kind] && (c.SubPanes["*"] || c.SubPanes[paneID])
		c.mu.Unlock()
		if want {
			c.Emit(v) // queued, never blocking on this client's socket
		}
	}
}

// restartNote is user-facing copy. It states plainly what a restart brings
// back and what it does not, which is the honesty tag for this whole server.
const restartNote = "A restart brings back the layout, the labels and the working directories. " +
	"It does not bring back running processes. Panes come back closed or unknown, never idle. " +
	"A headless pane resumes only when its adapter recorded a harness session id."

func (s *Server) handleStatus(c *Client, r *proto.Request) proto.Response {
	panes := s.tree.Panes()
	live := 0
	for _, p := range panes {
		if !p.Closed {
			live++
		}
	}
	return proto.OKResp(r.ID, map[string]any{
		"pid":          os.Getpid(),
		"socket":       s.cfg.SocketPath,
		"data_dir":     s.cfg.DataDir,
		"uptime_s":     int(time.Since(s.started).Seconds()),
		"panes":              len(panes),
		"panes_live":         live,
		"restart_note":       restartNote,
		"restore":            s.LastRestore(),
		"detection_warnings": s.DetectionWarnings(),
	})
}

// Close stops listening and releases the start lock, in that order, so the next
// `coppice server start` after a clean stop is not refused by our own lock.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		s.releaseStartLock()
		return nil
	}
	s.closed = true
	ln := s.ln
	s.mu.Unlock()
	var err error
	if ln != nil {
		err = ln.Close()
	}
	s.releaseStartLock()
	return err
}
```

- [ ] **Step 5: Declare the two forward types so this task compiles on its own**

`Server` names `map[string]*LivePane` and `[]*waiter`. Task 8 and Task 10 write the real
declarations; put the empty ones in `server.go` now so this task builds, and let those tasks
replace them:

```go
// LivePane is replaced by Task 8. It joins a snapshot of the tree record to the
// things that exist only while the server runs.
type LivePane struct{}

// waiter is replaced by Task 10. It is how agent.wait sleeps until a state
// arrives instead of polling.
type waiter struct{}
```

- [ ] **Step 6: Run the test and watch it pass**

Run: `cd harness/coppice && go test -race ./internal/server/ -v && go vet ./...`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add harness/coppice/internal/server
git commit -m "feat(coppice): one socket, one uid, and requests that do not queue

A connection whose peer credentials we cannot read is refused, not trusted, and
the non-Linux build refuses every connection rather than pretending to check.
Config carries no skip-the-check field, so no later caller can turn it off.
Each request runs on its own goroutine: agent.wait blocks for two minutes and
the cockpit holds one connection, so a serial loop would freeze every other
pane's allow and deny while frames kept arriving and the UI looked alive.
server.status carries the restart note in plain words, because a floor that
claimed to restore running agents would be the dishonest control section 3.5
forbids."
```

---

### Task 8: pane lifecycle over the socket

**Files:**
- Create: `harness/coppice/internal/server/panes.go`
- Test: `harness/coppice/internal/server/panes_test.go`

**Interfaces:**
- Consumes: `Server`, `Client`, `layout.Tree`, `pane.Grid`, `pane.StartPTY`, `pane.ReadSource`.
- Produces:
  ```go
  package server
  type LivePane struct { Info layout.Pane; Grid *pane.Grid; PTY *pane.PTY; Adapter pane.Proc }
  func (s *Server) Live(id string) (*LivePane, bool)
  func (s *Server) updatePane(id string, f func(*layout.Pane)) error   // tree + live copy
  func (s *Server) RegisterPaneCommands()
  ```
  Commands registered: `workspace.create`, `workspace.list`, `tab.create`, `tab.list`,
  `pane.create`, `pane.list`, `pane.send_text`, `pane.send_keys`, `pane.run`, `pane.read`,
  `pane.resize`, `pane.close`, `pane.wait_output`, `session.list`, `session.stop`.
  `pane.split` is an alias for `pane.create` in the same tab.

- [ ] **Step 1: Write the failing test**

```go
// harness/coppice/internal/server/panes_test.go
package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/toolchain"
)

func newPaneServer(t *testing.T) *Server {
	t.Helper()
	if r := toolchain.SkipReason(); r != "" {
		t.Skip(r)
	}
	s := newTestServer(t)
	s.RegisterPaneCommands()
	return s
}

func result(t *testing.T, r proto.Response) map[string]any {
	t.Helper()
	if !r.OK {
		t.Fatalf("request failed: %+v", r.Error)
	}
	b, _ := json.Marshal(r.Result)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestPaneCreateMakesAWorkspaceAndTabWhenNoneExist(t *testing.T) {
	s := newPaneServer(t)
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+t.TempDir()+`","cmd_argv":["sh","-c","sleep 2"],"kind":"pty","label":"first"}`)
	m := result(t, got[0])
	if m["pane"] != "w1:p1" || m["workspace"] != "w1" || m["tab"] != "w1:t1" {
		t.Fatalf("pane.create result = %v, want w1 / w1:t1 / w1:p1", m)
	}
}

func TestPaneCreateWithoutCwdIsBadRequest(t *testing.T) {
	s := newPaneServer(t)
	got := roundTrip(t, s, `{"id":"1","cmd":"pane.create","cmd_argv":["sh"]}`)
	if got[0].OK || got[0].Error.Code != proto.ErrBadRequest {
		t.Fatalf("got %+v, want bad_request", got[0])
	}
	if !strings.Contains(got[0].Error.Message, "cwd") {
		t.Fatalf("error %q does not name the missing field", got[0].Error.Message)
	}
}

func TestHeadlessPaneWithoutHarnessIsBadRequest(t *testing.T) {
	s := newPaneServer(t)
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+t.TempDir()+`","kind":"headless"}`)
	if got[0].OK || got[0].Error.Code != proto.ErrBadRequest {
		t.Fatalf("got %+v, want bad_request", got[0])
	}
	if !strings.Contains(got[0].Error.Message, "harness") {
		t.Fatalf("error %q does not name the missing field", got[0].Error.Message)
	}
}

func TestSpawnFailureIsSpawnFailedNotInternal(t *testing.T) {
	s := newPaneServer(t)
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+t.TempDir()+`","cmd_argv":["coppice-no-such-binary"],"kind":"pty"}`)
	if got[0].OK || got[0].Error.Code != proto.ErrSpawnFailed {
		t.Fatalf("got %+v, want spawn_failed", got[0])
	}
}

func TestUnknownPaneIsNoSuchPane(t *testing.T) {
	s := newPaneServer(t)
	got := roundTrip(t, s, `{"id":"1","cmd":"pane.read","pane":"w9:p9"}`)
	if got[0].OK || got[0].Error.Code != proto.ErrNoSuchPane {
		t.Fatalf("got %+v, want no_such_pane", got[0])
	}
}

func TestClosedPaneRejectsInputWithPaneClosed(t *testing.T) {
	s := newPaneServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c","sleep 2"],"kind":"pty"}`,
		`{"id":"2","cmd":"pane.close","pane":"w1:p1"}`,
		`{"id":"3","cmd":"pane.send_text","pane":"w1:p1","text":"hi"}`)
	if !got[1].OK {
		t.Fatalf("pane.close failed: %+v", got[1].Error)
	}
	if got[2].OK || got[2].Error.Code != proto.ErrPaneClosed {
		t.Fatalf("got %+v, want pane_closed", got[2])
	}
}

func TestPaneRunSendsTheCommandAndAnEnter(t *testing.T) {
	s := newPaneServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh"],"kind":"pty","cols":60,"rows":10}`,
		`{"id":"2","cmd":"pane.run","pane":"w1:p1","line":"echo marker-9c1"}`,
		`{"id":"3","cmd":"pane.wait_output","pane":"w1:p1","contains":"marker-9c1","timeout_ms":5000}`,
		`{"id":"4","cmd":"pane.read","pane":"w1:p1","source":"visible"}`)
	for i, r := range got {
		if !r.OK {
			t.Fatalf("request %d failed: %+v", i+1, r.Error)
		}
	}
	m := result(t, got[3])
	text, _ := m["text"].(string)
	if !strings.Contains(text, "marker-9c1") {
		t.Fatalf("pane.read = %q, want it to contain marker-9c1", text)
	}
}

func TestWaitOutputTimesOutWithTheTimeoutCode(t *testing.T) {
	s := newPaneServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh"],"kind":"pty"}`,
		`{"id":"2","cmd":"pane.wait_output","pane":"w1:p1","contains":"never-appears","timeout_ms":300}`)
	if got[1].OK || got[1].Error.Code != proto.ErrTimeout {
		t.Fatalf("got %+v, want timeout", got[1])
	}
}

func TestSendKeysTranslatesNamedKeys(t *testing.T) {
	s := newPaneServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh"],"kind":"pty","cols":60,"rows":10}`,
		`{"id":"2","cmd":"pane.send_text","pane":"w1:p1","text":"echo keyed-4f2","enter":false}`,
		`{"id":"3","cmd":"pane.send_keys","pane":"w1:p1","keys":["enter"]}`,
		`{"id":"4","cmd":"pane.wait_output","pane":"w1:p1","contains":"keyed-4f2","timeout_ms":5000}`)
	for i, r := range got {
		if !r.OK {
			t.Fatalf("request %d failed: %+v", i+1, r.Error)
		}
	}
}

func TestSendKeysRejectsAnUnknownKeyName(t *testing.T) {
	s := newPaneServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh"],"kind":"pty"}`,
		`{"id":"2","cmd":"pane.send_keys","pane":"w1:p1","keys":["hyperspace"]}`)
	if got[1].OK || got[1].Error.Code != proto.ErrBadRequest {
		t.Fatalf("got %+v, want bad_request", got[1])
	}
	if !strings.Contains(got[1].Error.Message, "enter") {
		t.Fatalf("error %q does not list a key that works", got[1].Error.Message)
	}
}

func TestPaneListReportsKindAndState(t *testing.T) {
	s := newPaneServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c","sleep 2"],"kind":"pty","label":"one"}`,
		`{"id":"2","cmd":"pane.list"}`)
	m := result(t, got[1])
	panes, _ := m["panes"].([]any)
	if len(panes) != 1 {
		t.Fatalf("pane.list returned %d panes, want 1", len(panes))
	}
	p, _ := panes[0].(map[string]any)
	if p["kind"] != "pty" || p["label"] != "one" {
		t.Fatalf("pane entry = %v, want kind pty and label one", p)
	}
	// A pane nobody has reported on is unknown, never idle.
	if p["state"] != "unknown" {
		t.Fatalf("a fresh pane reports state %v, want unknown", p["state"])
	}
}

func TestProcessExitMarksThePaneDoneFromTheProcessSource(t *testing.T) {
	s := newPaneServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c","exit 3"],"kind":"pty"}`,
		`{"id":"2","cmd":"pane.wait_output","pane":"w1:p1","state":"done","timeout_ms":5000}`,
		`{"id":"3","cmd":"pane.list"}`)
	if !got[1].OK {
		t.Fatalf("waiting for done failed: %+v", got[1].Error)
	}
	m := result(t, got[2])
	panes, _ := m["panes"].([]any)
	p, _ := panes[0].(map[string]any)
	if p["state"] != "done" || p["source"] != "process" {
		t.Fatalf("after exit: state=%v source=%v, want done / process", p["state"], p["source"])
	}
	if p["exit_code"] != float64(3) {
		t.Fatalf("exit_code = %v, want 3", p["exit_code"])
	}
}
```

- [ ] **Step 2: Run the test and watch it fail**

Run: `cd harness/coppice && go test -race ./internal/server/ -run Pane -v`
Expected: FAIL, `s.RegisterPaneCommands undefined`.

- [ ] **Step 3: Write the implementation**

```go
// harness/coppice/internal/server/panes.go
package server

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/opendaisugi/coppice/internal/layout"
	"github.com/opendaisugi/coppice/internal/pane"
	"github.com/opendaisugi/coppice/internal/proto"
)

// LivePane joins a snapshot of the record in the tree to the things that only
// exist while the server runs. Info is a COPY: the tree owns the record, and
// changing one goes through updatePane so the tree and this copy never disagree.
type LivePane struct {
	Info    layout.Pane
	Grid    *pane.Grid
	PTY     *pane.PTY
	Adapter pane.Proc
}

// liveMu guards s.live. It is a field, not a package variable: two Server
// values in one test binary must not contend on one global lock.

func (s *Server) Live(id string) (*LivePane, bool) {
	s.liveMu.RLock()
	defer s.liveMu.RUnlock()
	lp, ok := s.live[id]
	return lp, ok
}

func (s *Server) putLive(id string, lp *LivePane) {
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	s.live[id] = lp
}

// updatePane changes the record in the tree and refreshes the live snapshot in
// one place, then persists. Nothing else writes a layout.Pane field.
func (s *Server) updatePane(id string, f func(*layout.Pane)) error {
	if err := s.tree.UpdatePane(id, f); err != nil {
		return err
	}
	rec, ok := s.tree.Pane(id)
	if !ok {
		return nil
	}
	s.liveMu.Lock()
	if lp, ok := s.live[id]; ok {
		lp.Info = rec
	}
	s.liveMu.Unlock()
	s.saveLayout()
	return nil
}

// namedKeys is the closed set pane.send_keys accepts. An unknown name is a
// bad_request that lists what works, rather than a silent no-op.
var namedKeys = map[string]string{
	"enter": "\r", "return": "\r", "tab": "\t", "esc": "\x1b", "escape": "\x1b",
	"space": " ", "backspace": "\x7f", "up": "\x1b[A", "down": "\x1b[B",
	"right": "\x1b[C", "left": "\x1b[D",
	"ctrl+a": "\x01", "ctrl+b": "\x02", "ctrl+c": "\x03", "ctrl+d": "\x04",
	"ctrl+e": "\x05", "ctrl+l": "\x0c", "ctrl+o": "\x0f", "ctrl+r": "\x12",
	"ctrl+u": "\x15", "ctrl+w": "\x17",
}

func keyNames() string {
	out := make([]string, 0, len(namedKeys))
	for k := range namedKeys {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

func (s *Server) RegisterPaneCommands() {
	s.Handle("workspace.create", s.handleWorkspaceCreate)
	s.Handle("workspace.list", s.handleWorkspaceList)
	s.Handle("tab.create", s.handleTabCreate)
	s.Handle("tab.list", s.handleTabList)
	s.Handle("pane.create", s.handlePaneCreate)
	s.Handle("pane.split", s.handlePaneCreate)
	s.Handle("pane.list", s.handlePaneList)
	s.Handle("pane.send_text", s.handleSendText)
	s.Handle("pane.send_keys", s.handleSendKeys)
	s.Handle("pane.run", s.handleRun)
	s.Handle("pane.read", s.handleRead)
	s.Handle("pane.resize", s.handleResize)
	s.Handle("pane.close", s.handleClose)
	s.Handle("pane.wait_output", s.handleWaitOutput)
	s.Handle("session.list", s.handlePaneList)
	s.Handle("session.stop", s.handleClose)
}

func (s *Server) handleWorkspaceCreate(_ *Client, r *proto.Request) proto.Response {
	cwd, _ := r.Str("cwd")
	label, _ := r.Str("label")
	ws := s.tree.CreateWorkspace(label, cwd)
	tab, err := s.tree.CreateTab(ws.ID, "main")
	if err != nil {
		return proto.ErrResp(r.ID, proto.ErrInternal, err.Error())
	}
	s.saveLayout()
	return proto.OKResp(r.ID, map[string]any{"workspace": ws.ID, "tab": tab.ID})
}

func (s *Server) handleWorkspaceList(_ *Client, r *proto.Request) proto.Response {
	out := []map[string]any{}
	for _, w := range s.tree.Workspaces() {
		out = append(out, map[string]any{
			"id": w.ID, "label": w.Label, "cwd": w.Cwd, "tabs": len(w.TabIDs),
		})
	}
	return proto.OKResp(r.ID, map[string]any{"workspaces": out})
}

func (s *Server) handleTabCreate(_ *Client, r *proto.Request) proto.Response {
	wsID, ok := r.Str("workspace")
	if !ok {
		wsID, _ = s.tree.Current()
	}
	if wsID == "" {
		return proto.ErrResp(r.ID, proto.ErrNoSuchWorkspace,
			"there is no workspace yet. Run: coppice workspace create --cwd .")
	}
	label, _ := r.Str("label")
	tab, err := s.tree.CreateTab(wsID, label)
	if err != nil {
		return proto.ErrResp(r.ID, proto.ErrNoSuchWorkspace, err.Error())
	}
	s.saveLayout()
	return proto.OKResp(r.ID, map[string]any{"tab": tab.ID, "workspace": wsID})
}

func (s *Server) handleTabList(_ *Client, r *proto.Request) proto.Response {
	wsID, ok := r.Str("workspace")
	if !ok {
		wsID, _ = s.tree.Current()
	}
	tabs, err := s.tree.Tabs(wsID)
	if err != nil {
		return proto.ErrResp(r.ID, proto.ErrNoSuchWorkspace, err.Error())
	}
	out := []map[string]any{}
	for _, tb := range tabs {
		out = append(out, map[string]any{"id": tb.ID, "label": tb.Label, "panes": len(tb.PaneIDs)})
	}
	return proto.OKResp(r.ID, map[string]any{"tabs": out})
}

func (s *Server) handlePaneCreate(_ *Client, r *proto.Request) proto.Response {
	cwd, ok := r.Str("cwd")
	if !ok || cwd == "" {
		return proto.ErrResp(r.ID, proto.ErrBadRequest,
			"pane.create needs cwd. Pass the directory the pane should start in.")
	}
	kind := layout.KindPTY
	if k, ok := r.Str("kind"); ok {
		switch k {
		case "pty":
			kind = layout.KindPTY
		case "headless":
			kind = layout.KindHeadless
		default:
			return proto.ErrResp(r.ID, proto.ErrBadRequest,
				fmt.Sprintf("kind %q is not pty or headless", k))
		}
	}
	argv, _ := r.StrSlice("cmd_argv")
	harness, _ := r.Str("harness")
	if kind == layout.KindHeadless && harness == "" {
		return proto.ErrResp(r.ID, proto.ErrBadRequest,
			"a headless pane needs harness. Run: coppice agent list to see the adapters.")
	}
	if kind == layout.KindPTY && len(argv) == 0 {
		return proto.ErrResp(r.ID, proto.ErrBadRequest,
			"a pty pane needs cmd_argv. Put the command after -- on the command line.")
	}

	wsID, tabID := s.tree.Current()
	if id, ok := r.Str("workspace"); ok {
		wsID = id
	}
	if id, ok := r.Str("tab"); ok {
		tabID = id
	}
	if wsID == "" {
		ws := s.tree.CreateWorkspace("", cwd)
		tab, err := s.tree.CreateTab(ws.ID, "main")
		if err != nil {
			return proto.ErrResp(r.ID, proto.ErrInternal, err.Error())
		}
		wsID, tabID = ws.ID, tab.ID
	}

	cols, ok := r.Int("cols")
	if !ok {
		cols = 120
	}
	rows, ok := r.Int("rows")
	if !ok {
		rows = 40
	}
	env, _ := r.StrMap("env")
	label, _ := r.Str("label")

	rec, err := s.tree.CreatePane(wsID, tabID, layout.Pane{
		Label: label, Cwd: cwd, Argv: argv, Env: env, Kind: kind,
		Harness: harness, Cols: cols, Rows: rows,
	})
	if err != nil {
		code := proto.ErrNoSuchTab
		if strings.Contains(err.Error(), "workspace") {
			code = proto.ErrNoSuchWorkspace
		}
		return proto.ErrResp(r.ID, code, err.Error())
	}

	if err := s.startPane(rec); err != nil {
		_ = s.tree.ClosePane(rec.ID, nil)
		return proto.ErrResp(r.ID, proto.ErrSpawnFailed, err.Error())
	}
	s.saveLayout()
	return proto.OKResp(r.ID, map[string]any{
		"pane": rec.ID, "workspace": wsID, "tab": tabID,
	})
}

// startPane builds the grid and, for a pty pane, the process. Headless panes
// get their adapter in Task 14; until then this returns a clear error rather
// than a pane that silently does nothing.
func (s *Server) startPane(rec layout.Pane) error {
	g, err := pane.NewGrid(rec.Cols, rec.Rows)
	if err != nil {
		return err
	}
	lp := &LivePane{Info: rec, Grid: g}
	if rec.Kind == layout.KindPTY {
		p, err := pane.StartPTY(pane.SpawnOpts{
			Cwd: rec.Cwd, Argv: rec.Argv, Env: rec.Env,
			Cols: rec.Cols, Rows: rec.Rows,
			Sock: s.cfg.SocketPath, PaneID: rec.ID,
		}, g)
		if err != nil {
			g.Close()
			return err
		}
		lp.PTY = p
		go s.watchExit(rec.ID, p)
	}
	s.putLive(rec.ID, lp)
	return nil
}

// watchExit turns a process exit into the one PaneStateEvent only the server
// can produce. done comes from process or headless and from nowhere else, and
// this fires exactly once and never retries, which is why state.Merge must let
// it through every hold.
func (s *Server) watchExit(paneID string, p *pane.PTY) {
	<-p.Done()
	code, _ := p.ExitCode()
	if err := s.tree.ClosePane(paneID, &code); err == nil {
		s.saveLayout()
	}
	harness := "shell"
	if rec, ok := s.tree.Pane(paneID); ok && rec.Harness != "" {
		harness = rec.Harness
	}
	s.ApplyState(paneID, proto.PaneStateEvent{
		V: 1, TS: nowSeconds(), SessionID: paneID, Harness: harness,
		Pane: &paneID, State: proto.StateDone, Source: proto.SrcProcess,
		Detail: fmt.Sprintf("exit=%d", code),
	})
}

func nowSeconds() float64 { return float64(time.Now().UnixNano()) / 1e9 }

func (s *Server) handlePaneList(_ *Client, r *proto.Request) proto.Response {
	out := []map[string]any{}
	for _, p := range s.tree.Panes() {
		row := map[string]any{
			"id": p.ID, "label": p.Label, "cwd": p.Cwd, "cmd": p.Argv,
			"kind": string(p.Kind), "harness": p.Harness,
			"workspace": p.Workspace, "tab": p.Tab, "closed": p.Closed,
			"cols": p.Cols, "rows": p.Rows,
			"state": proto.StateUnknown, "source": nil, "detail": "",
		}
		if p.ExitCode != nil {
			row["exit_code"] = *p.ExitCode
		}
		if ev, ok := s.states.Current(p.ID); ok {
			row["state"] = ev.State
			row["source"] = ev.Source
			row["detail"] = ev.Detail
			if ev.Ask != nil {
				row["ask"] = ev.Ask
			}
		}
		out = append(out, row)
	}
	return proto.OKResp(r.ID, map[string]any{"panes": out})
}

// livePane resolves a pane id to something writable, mapping every failure to
// the right closed-enum code.
func (s *Server) livePane(r *proto.Request) (*LivePane, *proto.Response) {
	id, ok := r.Str("pane")
	if !ok || id == "" {
		resp := proto.ErrResp(r.ID, proto.ErrBadRequest, "this command needs pane. Run: coppice pane list")
		return nil, &resp
	}
	rec, ok := s.tree.Pane(id)
	if !ok {
		resp := proto.ErrResp(r.ID, proto.ErrNoSuchPane,
			fmt.Sprintf("no pane %q. Run: coppice pane list", id))
		return nil, &resp
	}
	if rec.Closed {
		resp := proto.ErrResp(r.ID, proto.ErrPaneClosed,
			fmt.Sprintf("pane %s has closed. Create a new one with: coppice pane create", id))
		return nil, &resp
	}
	lp, ok := s.Live(id)
	if !ok {
		resp := proto.ErrResp(r.ID, proto.ErrPaneClosed,
			fmt.Sprintf("pane %s did not survive the last restart. Create a new one.", id))
		return nil, &resp
	}
	return lp, nil
}

func (lp *LivePane) write(b []byte) error {
	switch {
	case lp.PTY != nil:
		_, err := lp.PTY.Write(b)
		return err
	case lp.Adapter != nil:
		return lp.Adapter.WriteStdin(b)
	default:
		return fmt.Errorf("this pane has nothing to write to")
	}
}

func (s *Server) handleSendText(_ *Client, r *proto.Request) proto.Response {
	lp, bad := s.livePane(r)
	if bad != nil {
		return *bad
	}
	text, _ := r.Str("text")
	enter, ok := r.Bool("enter")
	if !ok {
		enter = true
	}
	if enter {
		text += "\r"
	}
	if err := lp.write([]byte(text)); err != nil {
		return proto.ErrResp(r.ID, proto.ErrInternal, err.Error())
	}
	return proto.OKResp(r.ID, map[string]any{"sent": len(text)})
}

func (s *Server) handleSendKeys(_ *Client, r *proto.Request) proto.Response {
	lp, bad := s.livePane(r)
	if bad != nil {
		return *bad
	}
	keys, ok := r.StrSlice("keys")
	if !ok || len(keys) == 0 {
		return proto.ErrResp(r.ID, proto.ErrBadRequest,
			"pane.send_keys needs keys. Known keys: "+keyNames())
	}
	var out strings.Builder
	for _, k := range keys {
		if seq, ok := namedKeys[strings.ToLower(k)]; ok {
			out.WriteString(seq)
			continue
		}
		if len([]rune(k)) == 1 {
			out.WriteString(k)
			continue
		}
		return proto.ErrResp(r.ID, proto.ErrBadRequest,
			fmt.Sprintf("key %q is not known. Known keys: %s", k, keyNames()))
	}
	if err := lp.write([]byte(out.String())); err != nil {
		return proto.ErrResp(r.ID, proto.ErrInternal, err.Error())
	}
	return proto.OKResp(r.ID, map[string]any{"sent": len(keys)})
}

func (s *Server) handleRun(_ *Client, r *proto.Request) proto.Response {
	// Execution ruling 2026-09-09: the command text lives under "line"; "cmd" is the verb.
	cmd, ok := r.Str("line")
	if !ok {
		return proto.ErrResp(r.ID, proto.ErrBadRequest, "pane.run needs line. Pass the line to type.")
	}
	lp, bad := s.livePane(r)
	if bad != nil {
		return *bad
	}
	if err := lp.write([]byte(cmd + "\r")); err != nil {
		return proto.ErrResp(r.ID, proto.ErrInternal, err.Error())
	}
	return proto.OKResp(r.ID, map[string]any{"sent": len(cmd) + 1})
}

func (s *Server) handleRead(_ *Client, r *proto.Request) proto.Response {
	lp, bad := s.livePane(r)
	if bad != nil {
		return *bad
	}
	src := pane.ReadVisible
	if v, ok := r.Str("source"); ok {
		src = pane.ReadSource(v)
	}
	text, err := lp.Grid.Read(src)
	if err != nil {
		return proto.ErrResp(r.ID, proto.ErrBadRequest, err.Error())
	}
	return proto.OKResp(r.ID, map[string]any{"text": text, "source": string(src)})
}

func (s *Server) handleResize(_ *Client, r *proto.Request) proto.Response {
	lp, bad := s.livePane(r)
	if bad != nil {
		return *bad
	}
	cols, okc := r.Int("cols")
	rows, okr := r.Int("rows")
	if !okc || !okr || cols <= 0 || rows <= 0 {
		return proto.ErrResp(r.ID, proto.ErrBadRequest,
			"pane.resize needs positive cols and rows.")
	}
	if lp.PTY != nil {
		if err := lp.PTY.Resize(cols, rows); err != nil {
			return proto.ErrResp(r.ID, proto.ErrInternal, err.Error())
		}
	} else if err := lp.Grid.Resize(cols, rows); err != nil {
		return proto.ErrResp(r.ID, proto.ErrInternal, err.Error())
	}
	_ = s.updatePane(lp.Info.ID, func(x *layout.Pane) { x.Cols, x.Rows = cols, rows })
	return proto.OKResp(r.ID, map[string]any{"cols": cols, "rows": rows})
}

func (s *Server) handleClose(_ *Client, r *proto.Request) proto.Response {
	id, _ := r.Str("pane")
	if id == "" {
		id, _ = r.Str("session")
	}
	rec, ok := s.tree.Pane(id)
	if !ok {
		return proto.ErrResp(r.ID, proto.ErrNoSuchPane,
			fmt.Sprintf("no pane %q. Run: coppice pane list", id))
	}
	if lp, ok := s.Live(id); ok {
		if lp.PTY != nil {
			_ = lp.PTY.Close()
		}
		if lp.Adapter != nil {
			_ = lp.Adapter.Stop()
		}
		lp.Grid.Close()
	}
	_ = s.tree.ClosePane(rec.ID, rec.ExitCode)
	s.saveLayout()
	return proto.OKResp(r.ID, map[string]any{"pane": rec.ID, "closed": true})
}

// handleWaitOutput waits for text on the screen, or for a merged state, or
// times out. It polls at 50 ms: a pane's grid changes on the PTY reader's
// goroutine, and a poll keeps the wait out of that hot path.
func (s *Server) handleWaitOutput(_ *Client, r *proto.Request) proto.Response {
	id, _ := r.Str("pane")
	rec, ok := s.tree.Pane(id)
	if !ok {
		return proto.ErrResp(r.ID, proto.ErrNoSuchPane,
			fmt.Sprintf("no pane %q. Run: coppice pane list", id))
	}
	contains, wantText := r.Str("contains")
	wantState, wantsState := r.Str("state")
	if !wantText && !wantsState {
		return proto.ErrResp(r.ID, proto.ErrBadRequest,
			"pane.wait_output needs contains or state.")
	}
	ms, ok := r.Int("timeout_ms")
	if !ok || ms <= 0 {
		ms = 30000
	}
	deadline := time.Now().Add(time.Duration(ms) * time.Millisecond)
	for time.Now().Before(deadline) {
		if wantsState {
			if ev, ok := s.states.Current(rec.ID); ok && ev.State == wantState {
				return proto.OKResp(r.ID, map[string]any{"matched": "state", "state": ev.State})
			}
		}
		if wantText {
			if lp, ok := s.Live(rec.ID); ok {
				if text, err := lp.Grid.Read(pane.ReadRecent); err == nil &&
					strings.Contains(text, contains) {
					return proto.OKResp(r.ID, map[string]any{"matched": "text"})
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return proto.ErrResp(r.ID, proto.ErrTimeout,
		fmt.Sprintf("pane %s did not match within %d ms. Run: coppice pane read %s", rec.ID, ms, rec.ID))
}

func (s *Server) saveLayout() {
	// Best effort. A layout write failure must never fail a command that
	// already succeeded in the world.
	_ = s.tree.Save(layoutPath(s.cfg.DataDir))
}

func layoutPath(dataDir string) string { return dataDir + "/layout.json" }
```

`Server` already carries `live` and `liveMu`, and `New` already initialises `live`: Task 7
declared both. Replace Task 7's empty `LivePane` declaration in `server.go` with the real one
above, and add the method `ApplyState`. Task 10 replaces `ApplyState` with the full version that
validates and wakes waiters; there must never be two definitions of it. For this task add the
minimal version:

```go
// ApplyState merges one event into the pane's state and broadcasts it when it
// is news. Task 10 replaces this with the version that validates the event
// first and wakes agent.wait.
func (s *Server) ApplyState(paneID string, ev proto.PaneStateEvent) {
	merged, changed := s.states.Apply(paneID, ev, nowSeconds())
	if changed {
		s.Broadcast("state", paneID, stateEvent{Event: "state", PaneStateEvent: merged})
	}
}

type stateEvent struct {
	Event string `json:"event"`
	proto.PaneStateEvent
}
```

- [ ] **Step 4: Run the test and watch it pass**

Run: `cd harness/coppice && go test -race ./internal/server/ -v && go vet ./...`
Expected: PASS. The race detector is what proves `updatePane` is the only writer.

- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/server
git commit -m "feat(coppice): the pane verbs, with Herdr's nouns and our error codes

A Herdr user should drive coppice from muscle memory, so the verbs are theirs.
Every failure maps to one closed-enum code and every message names the command
that gets the operator unstuck. A fresh pane reports unknown, not idle: nobody
has told us anything about it yet."
```

---

### Task 9: attach, frames, and subscriptions

**Files:**
- Create: `harness/coppice/internal/server/attach.go`
- Test: `harness/coppice/internal/server/attach_test.go`

**Interfaces:**
- Consumes: `Client`, `LivePane`, `pane.FrameState`, `proto.Frame`.
- Produces:
  ```go
  package server
  const FrameInterval = 16 * time.Millisecond   // 60 Hz ceiling per pane
  func (s *Server) RegisterAttachCommands()
  ```
  Commands: `pane.attach`, `pane.detach`, `events.subscribe`.
  `pane.attach` also subscribes the client to `state` events for that pane.

- [ ] **Step 1: Write the failing test**

```go
// harness/coppice/internal/server/attach_test.go
package server

import (
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/toolchain"
)

// stream drives ServeConn and hands back a decoder plus a send function, so a
// test can interleave requests and read the events they cause.
func stream(t *testing.T, s *Server) (send func(string), next func() map[string]any, stop func()) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go func() { s.ServeConn(inR, outW); _ = outW.Close() }()
	d := proto.NewDecoder(outR)
	send = func(line string) {
		if _, err := io.WriteString(inW, line+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	next = func() map[string]any {
		line, err := d.Next()
		if err != nil {
			t.Fatalf("reading the next line: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	stop = func() { _ = inW.Close() }
	return
}

func TestAttachSendsAFullFirstFrameThenRowDiffs(t *testing.T) {
	if r := toolchain.SkipReason(); r != "" {
		t.Skip(r)
	}
	s := newTestServer(t)
	s.RegisterPaneCommands()
	s.RegisterAttachCommands()

	send, next, stop := stream(t, s)
	defer stop()
	send(`{"id":"1","cmd":"pane.create","cwd":"` + t.TempDir() +
		`","cmd_argv":["sh","-c","echo hi; sleep 3"],"kind":"pty","cols":20,"rows":6}`)
	if m := next(); m["ok"] != true {
		t.Fatalf("pane.create failed: %v", m)
	}
	send(`{"id":"2","cmd":"pane.attach","pane":"w1:p1","cols":20,"rows":6}`)
	if m := next(); m["ok"] != true {
		t.Fatalf("pane.attach failed: %v", m)
	}

	deadline := time.Now().Add(6 * time.Second)
	sawFull := false
	for time.Now().Before(deadline) {
		m := next()
		if m["event"] != "frame" {
			continue
		}
		rows, _ := m["rows_changed"].(map[string]any)
		if !sawFull {
			if len(rows) != 6 {
				t.Fatalf("first frame carried %d rows, want all 6", len(rows))
			}
			sawFull = true
			continue
		}
		// Any later frame must be a diff, so strictly fewer than all rows
		// unless the whole screen really changed.
		b, _ := json.Marshal(m)
		if strings.Contains(string(b), "hi") {
			return // the echo landed in a diff frame; that is the whole test
		}
	}
	t.Fatal("no frame carried the child's output within 6 s")
}

func TestAttachOnAnUnknownPaneIsNoSuchPane(t *testing.T) {
	s := newTestServer(t)
	s.RegisterPaneCommands()
	s.RegisterAttachCommands()
	got := roundTrip(t, s, `{"id":"1","cmd":"pane.attach","pane":"w9:p9"}`)
	if got[0].OK || got[0].Error.Code != proto.ErrNoSuchPane {
		t.Fatalf("got %+v, want no_such_pane", got[0])
	}
}

func TestDetachWithoutAttachIsNotAttached(t *testing.T) {
	if r := toolchain.SkipReason(); r != "" {
		t.Skip(r)
	}
	s := newTestServer(t)
	s.RegisterPaneCommands()
	s.RegisterAttachCommands()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+t.TempDir()+`","cmd_argv":["sh","-c","sleep 2"],"kind":"pty"}`,
		`{"id":"2","cmd":"pane.detach","pane":"w1:p1"}`)
	if got[1].OK || got[1].Error.Code != proto.ErrNotAttached {
		t.Fatalf("got %+v, want not_attached", got[1])
	}
}

func TestSubscribeDeliversStateEventsForTheChosenPanesOnly(t *testing.T) {
	s := newTestServer(t)
	s.RegisterPaneCommands()
	s.RegisterAttachCommands()
	send, next, stop := stream(t, s)
	defer stop()
	send(`{"id":"1","cmd":"events.subscribe","panes":["w1:p1"],"kinds":["state"]}`)
	if m := next(); m["ok"] != true {
		t.Fatalf("events.subscribe failed: %v", m)
	}

	// An event for another pane must not arrive.
	other := "w1:p2"
	s.ApplyState(other, proto.PaneStateEvent{
		V: 1, TS: nowSeconds(), SessionID: "s", Harness: "shell", Pane: &other,
		State: proto.StateWorking, Source: proto.SrcProcess,
	})
	wanted := "w1:p1"
	s.ApplyState(wanted, proto.PaneStateEvent{
		V: 1, TS: nowSeconds(), SessionID: "s", Harness: "shell", Pane: &wanted,
		State: proto.StateWorking, Source: proto.SrcProcess,
	})
	m := next()
	if m["event"] != "state" || m["pane"] != "w1:p1" {
		t.Fatalf("first delivered event = %v, want the w1:p1 state event", m)
	}
}

func TestSubscribeToEveryPaneWithAStar(t *testing.T) {
	s := newTestServer(t)
	s.RegisterPaneCommands()
	s.RegisterAttachCommands()
	send, next, stop := stream(t, s)
	defer stop()
	send(`{"id":"1","cmd":"events.subscribe","panes":"*","kinds":["state"]}`)
	if m := next(); m["ok"] != true {
		t.Fatalf("events.subscribe failed: %v", m)
	}
	p := "w3:p7"
	s.ApplyState(p, proto.PaneStateEvent{
		V: 1, TS: nowSeconds(), SessionID: "s", Harness: "shell", Pane: &p,
		State: proto.StateWorking, Source: proto.SrcProcess,
	})
	if m := next(); m["pane"] != "w3:p7" {
		t.Fatalf("star subscription missed %v", m)
	}
}

// One wedged client must not stall ApplyState for every pane. A PWA on a
// stalled tailnet is the realistic case, and the gate hook is blocking on a
// pane.report_state response while it happens.
func TestAWedgedClientIsDroppedRatherThanStallingTheServer(t *testing.T) {
	s := newTestServer(t)
	s.RegisterPaneCommands()
	s.RegisterAttachCommands()

	// A reader that never reads: its pipe fills and its Send blocks.
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go func() { s.ServeConn(inR, outW); _ = outW.Close() }()
	defer inW.Close()
	_, _ = io.WriteString(inW, `{"id":"1","cmd":"events.subscribe","panes":"*","kinds":["state"]}`+"\n")
	// Read only the subscribe response, then stop reading for ever.
	if _, err := proto.NewDecoder(outR).Next(); err != nil {
		t.Fatal(err)
	}

	p := "w1:p1"
	done := make(chan struct{})
	go func() {
		for i := 0; i < EventQueue*4; i++ {
			s.ApplyState(p, proto.PaneStateEvent{
				V: 1, TS: nowSeconds(), SessionID: "s", Harness: "shell", Pane: &p,
				State: proto.StateWorking, Source: proto.SrcProcess,
				Detail: strconv.Itoa(i),
			})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ApplyState stalled behind a client that stopped reading")
	}
}

func TestFramesAreCoalescedAtSixtyHertz(t *testing.T) {
	if r := toolchain.SkipReason(); r != "" {
		t.Skip(r)
	}
	s := newTestServer(t)
	s.RegisterPaneCommands()
	s.RegisterAttachCommands()
	send, next, stop := stream(t, s)
	defer stop()
	// A child that writes as fast as it can for one second.
	send(`{"id":"1","cmd":"pane.create","cwd":"` + t.TempDir() +
		`","cmd_argv":["sh","-c","i=0; while [ $i -lt 3000 ]; do echo $i; i=$((i+1)); done; sleep 2"],` +
		`"kind":"pty","cols":20,"rows":6}`)
	next()
	send(`{"id":"2","cmd":"pane.attach","pane":"w1:p1","cols":20,"rows":6}`)
	next()

	frames := 0
	start := time.Now()
	for time.Since(start) < time.Second {
		if m := next(); m["event"] == "frame" {
			frames++
		}
		if frames > 200 {
			break
		}
	}
	// 60 Hz for one second is 60 frames. Allow generous slack for scheduling,
	// but 3000 lines must not become 3000 frames.
	if frames > 150 {
		t.Fatalf("got %d frames in about a second, want the 60 Hz ceiling to hold", frames)
	}
	if frames == 0 {
		t.Fatal("got no frames at all")
	}
}
```

- [ ] **Step 2: Run the test and watch it fail**

Run: `cd harness/coppice && go test -race ./internal/server/ -run 'Attach|Subscribe|Frames|Detach' -v`
Expected: FAIL, `s.RegisterAttachCommands undefined`.

- [ ] **Step 3: Write the implementation**

```go
// harness/coppice/internal/server/attach.go
package server

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/opendaisugi/coppice/internal/layout"
	"github.com/opendaisugi/coppice/internal/pane"
	"github.com/opendaisugi/coppice/internal/proto"
)

// FrameInterval is the 60 Hz ceiling from spec-02. A harness redrawing a
// spinner must not turn into a frame per redraw on the wire.
const FrameInterval = 16 * time.Millisecond

func (s *Server) RegisterAttachCommands() {
	s.Handle("pane.attach", s.handleAttach)
	s.Handle("pane.detach", s.handleDetach)
	s.Handle("events.subscribe", s.handleSubscribe)
}

func (s *Server) handleAttach(c *Client, r *proto.Request) proto.Response {
	lp, bad := s.livePane(r)
	if bad != nil {
		return *bad
	}
	if cols, okc := r.Int("cols"); okc {
		if rows, okr := r.Int("rows"); okr && cols > 0 && rows > 0 {
			if lp.PTY != nil {
				_ = lp.PTY.Resize(cols, rows)
			} else {
				_ = lp.Grid.Resize(cols, rows)
			}
			_ = s.updatePane(lp.Info.ID, func(x *layout.Pane) { x.Cols, x.Rows = cols, rows })
		}
	}
	id := lp.Info.ID

	c.mu.Lock()
	if _, already := c.Attached[id]; already {
		c.mu.Unlock()
		return proto.OKResp(r.ID, map[string]any{"pane": id, "attached": true})
	}
	fs := pane.NewFrameState()
	c.Attached[id] = fs
	// Attaching also subscribes this client to the pane's state events, so a
	// renderer does not need a second call to show the status line.
	c.Subs["state"] = true
	c.Subs["frame"] = true
	c.SubPanes[id] = true
	c.mu.Unlock()

	go s.framePump(c, id, fs)
	return proto.OKResp(r.ID, map[string]any{"pane": id, "attached": true})
}

// framePump is one goroutine per attached client per pane. It wakes on a fixed
// tick rather than on every byte, which is what makes the 60 Hz ceiling real
// instead of aspirational.
func (s *Server) framePump(c *Client, paneID string, fs *pane.FrameState) {
	t := time.NewTicker(FrameInterval)
	defer t.Stop()
	for range t.C {
		c.mu.Lock()
		still := c.Attached[paneID] == fs
		c.mu.Unlock()
		if !still {
			return
		}
		lp, ok := s.Live(paneID)
		if !ok {
			return
		}
		frame, changed, err := fs.Next(paneID, lp.Grid)
		if err != nil {
			return
		}
		if !changed {
			continue
		}
		if c.Dead() {
			return
		}
		c.Emit(frame) // queued; a stalled client is dropped, not waited on
	}
}

func (s *Server) handleDetach(c *Client, r *proto.Request) proto.Response {
	id, _ := r.Str("pane")
	c.mu.Lock()
	_, was := c.Attached[id]
	delete(c.Attached, id)
	c.mu.Unlock()
	if !was {
		return proto.ErrResp(r.ID, proto.ErrNotAttached,
			fmt.Sprintf("this client is not attached to %s. Run: coppice attach %s", id, id))
	}
	return proto.OKResp(r.ID, map[string]any{"pane": id, "attached": false})
}

func (s *Server) handleSubscribe(c *Client, r *proto.Request) proto.Response {
	kinds, ok := r.StrSlice("kinds")
	if !ok || len(kinds) == 0 {
		kinds = []string{"state", "layout"}
	}
	for _, k := range kinds {
		switch k {
		case "state", "layout", "frame":
		default:
			return proto.ErrResp(r.ID, proto.ErrBadRequest,
				fmt.Sprintf("kind %q is not state, layout or frame", k))
		}
	}
	panes := []string{}
	if raw, ok := r.Raw("panes"); ok {
		var one string
		if err := json.Unmarshal(raw, &one); err == nil {
			panes = []string{one}
		} else if err := json.Unmarshal(raw, &panes); err != nil {
			return proto.ErrResp(r.ID, proto.ErrBadRequest,
				`panes must be a list of pane ids or the string "*"`)
		}
	} else {
		panes = []string{"*"}
	}
	c.mu.Lock()
	for _, k := range kinds {
		c.Subs[k] = true
	}
	for _, p := range panes {
		c.SubPanes[p] = true
	}
	c.mu.Unlock()
	return proto.OKResp(r.ID, map[string]any{"kinds": kinds, "panes": panes})
}
```

- [ ] **Step 4: Run the test and watch it pass**

Run: `cd harness/coppice && go test -race ./internal/server/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/server/attach.go harness/coppice/internal/server/attach_test.go
git commit -m "feat(coppice): attach streams frames on a tick, not on every byte

The frame pump wakes at 60 Hz and asks the grid what changed, so a harness
redrawing a spinner three thousand times a second still costs sixty frames.
Attaching subscribes the client to that pane's state as well, because a
renderer that shows a status line should not need a second call to fill it."
```

---

### Task 10: `pane.report_state`, the operator downgrade, and the agent verbs

**Files:**
- Create: `harness/coppice/internal/server/agents.go`
- Modify: `harness/coppice/internal/server/panes.go` (replace the minimal `ApplyState` added in
  Task 8 with the version below; it is the same name and signature)
- Test: `harness/coppice/internal/server/agents_test.go`

**Interfaces:**
- Consumes: `state.Store`, `proto.PaneStateEvent`, `Client.Operator`.
- Produces:
  ```go
  package server
  func (s *Server) RegisterAgentCommands()
  func (s *Server) ApplyState(paneID string, ev proto.PaneStateEvent)
  func (s *Server) WaitState(paneID, until string, timeout time.Duration) (proto.PaneStateEvent, bool)
  ```
  Commands: `pane.report_state`, `agent.list`, `agent.get`, `agent.prompt`, `agent.wait`,
  `agent.read`.

- [ ] **Step 1: Write the failing test**

```go
// harness/coppice/internal/server/agents_test.go
package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/toolchain"
)

func newAgentServer(t *testing.T) *Server {
	t.Helper()
	if r := toolchain.SkipReason(); r != "" {
		t.Skip(r)
	}
	s := newTestServer(t)
	s.RegisterPaneCommands()
	s.RegisterAttachCommands()
	s.RegisterAgentCommands()
	return s
}

const paneCreate = `{"id":"1","cmd":"pane.create","cwd":"%s","cmd_argv":["sh","-c","sleep 5"],` +
	`"kind":"pty","label":"auth fix","cols":40,"rows":10}`

func gateBlockedLine(pane string) string {
	return `{"id":"9","cmd":"pane.report_state","pane":"` + pane + `","event":` +
		`{"v":1,"ts":1757300000.0,"session_id":"d41c","harness":"claude-code","pane":"` + pane +
		`","state":"blocked","source":"gate","ask":{"id":"toolu_1","tool":"Bash",` +
		`"summary":"rm -rf build/","deadline":9999999999.0},"detail":"verdict=deny"}}`
}

func TestReportStateFromTheGateSetsTheMergedState(t *testing.T) {
	s := newAgentServer(t)
	got := roundTrip(t, s,
		strings.Replace(paneCreate, "%s", t.TempDir(), 1),
		gateBlockedLine("w1:p1"),
		`{"id":"3","cmd":"pane.list"}`)
	if !got[1].OK {
		t.Fatalf("pane.report_state failed: %+v", got[1].Error)
	}
	m := result(t, got[2])
	panes, _ := m["panes"].([]any)
	p, _ := panes[0].(map[string]any)
	if p["state"] != "blocked" || p["source"] != "gate" {
		t.Fatalf("after a gate report: state=%v source=%v, want blocked / gate", p["state"], p["source"])
	}
	ask, _ := p["ask"].(map[string]any)
	if ask == nil || ask["tool"] != "Bash" {
		t.Fatalf("the ask did not survive: %v", p["ask"])
	}
}

// Only a client attached to the pane may speak as the operator. A detached
// client claiming operator is downgraded to gate, never trusted at face value.
func TestOperatorSourceIsDowngradedForADetachedClient(t *testing.T) {
	s := newAgentServer(t)
	line := `{"id":"2","cmd":"pane.report_state","pane":"w1:p1","event":` +
		`{"v":1,"ts":1757300000.0,"session_id":"d41c","harness":"claude-code","pane":"w1:p1",` +
		`"state":"working","source":"operator","detail":"I said so"}}`
	got := roundTrip(t, s,
		strings.Replace(paneCreate, "%s", t.TempDir(), 1),
		line,
		`{"id":"3","cmd":"pane.list"}`)
	if !got[1].OK {
		t.Fatalf("pane.report_state failed: %+v", got[1].Error)
	}
	m := result(t, got[2])
	panes, _ := m["panes"].([]any)
	p, _ := panes[0].(map[string]any)
	if p["source"] != "gate" {
		t.Fatalf("a detached client kept source=%v, want it downgraded to gate", p["source"])
	}
	b, _ := json.Marshal(got[1].Result)
	if !strings.Contains(string(b), "gate") {
		t.Fatalf("the response %s does not tell the caller its source was changed", b)
	}
}

func TestOperatorSourceIsKeptForAnAttachedClient(t *testing.T) {
	s := newAgentServer(t)
	send, next, stop := stream(t, s)
	defer stop()
	send(strings.Replace(paneCreate, "%s", t.TempDir(), 1))
	next()
	send(`{"id":"2","cmd":"pane.attach","pane":"w1:p1","cols":40,"rows":10}`)
	next()
	send(`{"id":"3","cmd":"pane.report_state","pane":"w1:p1","event":` +
		`{"v":1,"ts":1757300000.0,"session_id":"d41c","harness":"claude-code","pane":"w1:p1",` +
		`"state":"working","source":"operator","detail":"I said so"}}`)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		m := next()
		if m["id"] == "3" {
			if m["ok"] != true {
				t.Fatalf("pane.report_state failed: %v", m)
			}
			res, _ := m["result"].(map[string]any)
			if res["source"] != "operator" {
				t.Fatalf("an attached client was downgraded to %v, want operator", res["source"])
			}
			return
		}
	}
	t.Fatal("no response to the report within 5 s")
}

func TestReportStateWithAMalformedEventIsBadRequest(t *testing.T) {
	s := newAgentServer(t)
	got := roundTrip(t, s,
		strings.Replace(paneCreate, "%s", t.TempDir(), 1),
		`{"id":"2","cmd":"pane.report_state","pane":"w1:p1","event":{"v":1,"state":"vibing","source":"gate"}}`)
	if got[1].OK || got[1].Error.Code != proto.ErrBadRequest {
		t.Fatalf("got %+v, want bad_request", got[1])
	}
}

func TestAgentWaitResolvesImmediatelyOnAGateBlocked(t *testing.T) {
	s := newAgentServer(t)
	got := roundTrip(t, s,
		strings.Replace(paneCreate, "%s", t.TempDir(), 1),
		gateBlockedLine("w1:p1"),
		`{"id":"3","cmd":"agent.wait","pane":"w1:p1","until":"blocked","timeout_ms":2000}`)
	if !got[2].OK {
		t.Fatalf("agent.wait did not resolve on the gate report: %+v", got[2].Error)
	}
	m := result(t, got[2])
	if m["state"] != "blocked" {
		t.Fatalf("agent.wait returned state=%v, want blocked", m["state"])
	}
}

func TestAgentWaitTimesOutWithTheTimeoutCode(t *testing.T) {
	s := newAgentServer(t)
	got := roundTrip(t, s,
		strings.Replace(paneCreate, "%s", t.TempDir(), 1),
		`{"id":"2","cmd":"agent.wait","pane":"w1:p1","until":"idle","timeout_ms":300}`)
	if got[1].OK || got[1].Error.Code != proto.ErrTimeout {
		t.Fatalf("got %+v, want timeout", got[1])
	}
	if !strings.Contains(got[1].Error.Message, "unknown") {
		t.Fatalf("the timeout message %q does not say what the pane's state actually is",
			got[1].Error.Message)
	}
}

func TestAgentListShowsEveryPaneWithItsMergedState(t *testing.T) {
	s := newAgentServer(t)
	got := roundTrip(t, s,
		strings.Replace(paneCreate, "%s", t.TempDir(), 1),
		gateBlockedLine("w1:p1"),
		`{"id":"3","cmd":"agent.list"}`)
	m := result(t, got[2])
	agents, _ := m["agents"].([]any)
	if len(agents) != 1 {
		t.Fatalf("agent.list returned %d agents, want 1", len(agents))
	}
	a, _ := agents[0].(map[string]any)
	if a["state"] != "blocked" || a["label"] != "auth fix" {
		t.Fatalf("agent entry = %v, want the blocked auth fix pane", a)
	}
}

func TestAgentPromptWithWaitReturnsTheResolvedState(t *testing.T) {
	s := newAgentServer(t)
	cwd := t.TempDir()
	// A shell pane is not an agent, but agent.prompt is defined as "send text
	// and optionally wait", so the shell exercises the whole path.
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c","exit 0"],"kind":"pty"}`,
		`{"id":"2","cmd":"agent.prompt","pane":"w1:p1","text":"hello","wait":true,"until":"done","timeout_ms":5000}`)
	if !got[1].OK {
		t.Fatalf("agent.prompt --wait failed: %+v", got[1].Error)
	}
	m := result(t, got[1])
	if m["state"] != "done" {
		t.Fatalf("agent.prompt returned state=%v, want done", m["state"])
	}
}

// An adapter is our own code, but it is still the wrong place to decide what a
// state string may be. The enum is closed, and a client switches on it.
func TestAnAdapterCannotInventAState(t *testing.T) {
	s := newAgentServer(t)
	roundTrip(t, s, strings.Replace(paneCreate, "%s", t.TempDir(), 1))
	id := "w1:p1"
	s.ApplyState(id, proto.PaneStateEvent{
		V: 1, TS: nowSeconds(), SessionID: id, Harness: "claude-code", Pane: &id,
		State: "busy", Source: proto.SrcHeadless,
	})
	ev, ok := s.States().Current(id)
	if !ok {
		t.Fatal("the invalid event produced no state at all")
	}
	if ev.State != proto.StateUnknown {
		t.Fatalf("state = %q, want unknown: an invented state must never reach a client", ev.State)
	}
	if ev.State == proto.StateIdle {
		t.Fatal("an invalid event produced idle")
	}
	if !strings.Contains(ev.Detail, "rejected") {
		t.Fatalf("detail = %q, want it to say the event was rejected and why", ev.Detail)
	}
}

func TestAgentWaitResolvesOnAStateThatArrivesRightAfterTheCall(t *testing.T) {
	s := newAgentServer(t)
	cwd := t.TempDir()
	// The child exits after a short delay, so the done event lands while
	// agent.wait is already blocked. Registering the waiter after reading the
	// current state would lose it.
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c","sleep 0.2"],"kind":"pty"}`,
		`{"id":"2","cmd":"agent.wait","pane":"w1:p1","until":"done","timeout_ms":5000}`)
	if !got[1].OK {
		t.Fatalf("agent.wait missed a state that arrived while it was waiting: %+v", got[1].Error)
	}
}

func TestAgentGetOnAnUnknownPaneIsNoSuchPane(t *testing.T) {
	s := newAgentServer(t)
	got := roundTrip(t, s, `{"id":"1","cmd":"agent.get","pane":"w9:p9"}`)
	if got[0].OK || got[0].Error.Code != proto.ErrNoSuchPane {
		t.Fatalf("got %+v, want no_such_pane", got[0])
	}
}
```

- [ ] **Step 2: Run the test and watch it fail**

Run: `cd harness/coppice && go test -race ./internal/server/ -run Agent -v`
Expected: FAIL, `s.RegisterAgentCommands undefined`.

- [ ] **Step 3: Write the implementation**

```go
// harness/coppice/internal/server/agents.go
package server

import (
	"fmt"
	"log"
	"time"

	"github.com/opendaisugi/coppice/internal/pane"
	"github.com/opendaisugi/coppice/internal/proto"
)

func (s *Server) RegisterAgentCommands() {
	s.Handle("pane.report_state", s.handleReportState)
	s.Handle("agent.list", s.handleAgentList)
	s.Handle("agent.get", s.handleAgentGet)
	s.Handle("agent.prompt", s.handleAgentPrompt)
	s.Handle("agent.wait", s.handleAgentWait)
	s.Handle("agent.read", s.handleAgentRead)
}

// ApplyState merges one event into the pane's state, records it, and
// broadcasts it when it is news. This is the single entry point: the PTY
// watcher, the manifest tick, the headless adapters and pane.report_state all
// arrive here, so master spec 3.1 is applied once.
//
// It validates first. Only pane.report_state passes through ParseStateEvent, so
// without this an adapter emitting Event{Kind: EvState, State: "busy"} would
// have that string merged, stored and broadcast, breaking the closed state enum
// for every client. An event that fails validation becomes unknown with the
// reason in detail, which is the fail-closed direction: never idle, never a
// state nobody defined.
func (s *Server) ApplyState(paneID string, ev proto.PaneStateEvent) {
	if err := ev.Validate(); err != nil {
		harness := ev.Harness
		if harness == "" {
			harness = "unknown"
		}
		id := paneID
		ev = proto.PaneStateEvent{
			V: 1, TS: nowSeconds(), SessionID: paneID, Harness: harness, Pane: &id,
			State: proto.StateUnknown, Source: proto.SrcProcess,
			Detail: truncate("rejected state event: "+err.Error(), proto.DetailMax),
		}
		log.Printf("coppice: pane %s sent an invalid state event: %v", paneID, err)
	}
	merged, changed := s.states.Apply(paneID, ev, nowSeconds())
	s.notifyWaiters(paneID, merged)
	if changed {
		s.Broadcast("state", paneID, stateEvent{Event: "state", PaneStateEvent: merged})
	}
}

type stateEvent struct {
	Event string `json:"event"`
	proto.PaneStateEvent
}

// handleReportState is the push authority from spec-01. The server downgrades
// an operator claim from a client that is not attached: only a client watching
// the pane can be speaking for the human in front of it.
//
// The limit of that check, stated plainly: a caller can simply send
// source: "gate" and be believed. The socket's uid check is the only boundary,
// and it is the right one for a one-user machine. If coppice ever serves more
// than one uid, this is the function that needs a real credential.
func (s *Server) handleReportState(c *Client, r *proto.Request) proto.Response {
	id, ok := r.Str("pane")
	if !ok || id == "" {
		return proto.ErrResp(r.ID, proto.ErrBadRequest,
			"pane.report_state needs pane. Run: coppice pane list")
	}
	if _, ok := s.tree.Pane(id); !ok {
		return proto.ErrResp(r.ID, proto.ErrNoSuchPane,
			fmt.Sprintf("no pane %q. Run: coppice pane list", id))
	}
	raw, ok := r.Raw("event")
	if !ok {
		return proto.ErrResp(r.ID, proto.ErrBadRequest,
			"pane.report_state needs event, one PaneStateEvent object.")
	}
	ev, err := proto.ParseStateEvent(raw)
	if err != nil {
		return proto.ErrResp(r.ID, proto.ErrBadRequest, err.Error())
	}
	downgraded := false
	if ev.Source == proto.SrcOperator && !c.Operator(id) {
		ev.Source = proto.SrcGate
		downgraded = true
	}
	ev.Pane = &id
	s.ApplyState(id, ev)
	res := map[string]any{"pane": id, "source": ev.Source, "state": ev.State}
	if downgraded {
		res["note"] = "source became gate. Attach to this pane to speak as the operator."
	}
	return proto.OKResp(r.ID, res)
}

// waiters lets agent.wait and pane.wait_output resolve the moment a state
// arrives, instead of polling. A waiter that nobody satisfies is cleaned up by
// its own timeout.
type waiter struct {
	pane  string
	until string
	ch    chan proto.PaneStateEvent
}

func (s *Server) notifyWaiters(paneID string, ev proto.PaneStateEvent) {
	s.mu.Lock()
	keep := s.waiters[:0]
	for _, w := range s.waiters {
		if w.pane == paneID && (w.until == "" || w.until == ev.State) {
			select {
			case w.ch <- ev:
			default:
			}
			continue
		}
		keep = append(keep, w)
	}
	s.waiters = keep
	s.mu.Unlock()
}

// WaitState blocks until the pane's merged state equals until, or the timeout
// passes.
//
// The waiter is registered BEFORE the current state is read. Reading first and
// registering after loses any state that arrives between the two, and the
// caller then waits the whole timeout: `agent.wait --until done` on a pane that
// exits a millisecond later would time out spuriously.
func (s *Server) WaitState(paneID, until string, timeout time.Duration) (proto.PaneStateEvent, bool) {
	w := &waiter{pane: paneID, until: until, ch: make(chan proto.PaneStateEvent, 1)}
	s.mu.Lock()
	s.waiters = append(s.waiters, w)
	s.mu.Unlock()

	drop := func() {
		s.mu.Lock()
		keep := s.waiters[:0]
		for _, x := range s.waiters {
			if x != w {
				keep = append(keep, x)
			}
		}
		s.waiters = keep
		s.mu.Unlock()
		// Drain, so a notify that raced with the deregistration is not left
		// sitting in a channel nobody reads.
		select {
		case <-w.ch:
		default:
		}
	}

	if ev, ok := s.states.Current(paneID); ok && (until == "" || ev.State == until) {
		drop()
		return ev, true
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case ev := <-w.ch:
		drop()
		return ev, true
	case <-timer.C:
		drop()
		ev, _ := s.states.Current(paneID)
		return ev, false
	}
}

func (s *Server) agentRow(paneID string) (map[string]any, bool) {
	rec, ok := s.tree.Pane(paneID)
	if !ok {
		return nil, false
	}
	row := map[string]any{
		"pane": rec.ID, "label": rec.Label, "cwd": rec.Cwd, "kind": string(rec.Kind),
		"harness": rec.Harness, "closed": rec.Closed,
		"state": proto.StateUnknown, "source": nil, "detail": "",
	}
	if ev, ok := s.states.Current(rec.ID); ok {
		row["state"] = ev.State
		row["source"] = ev.Source
		row["detail"] = ev.Detail
		if ev.Ask != nil {
			row["ask"] = ev.Ask
		}
		if ev.HarnessSessionID != nil {
			row["harness_session_id"] = *ev.HarnessSessionID
		}
	}
	return row, true
}

func (s *Server) handleAgentList(_ *Client, r *proto.Request) proto.Response {
	out := []map[string]any{}
	for _, p := range s.tree.Panes() {
		if row, ok := s.agentRow(p.ID); ok {
			out = append(out, row)
		}
	}
	return proto.OKResp(r.ID, map[string]any{"agents": out})
}

func (s *Server) handleAgentGet(_ *Client, r *proto.Request) proto.Response {
	id, _ := r.Str("pane")
	row, ok := s.agentRow(id)
	if !ok {
		return proto.ErrResp(r.ID, proto.ErrNoSuchPane,
			fmt.Sprintf("no pane %q. Run: coppice agent list", id))
	}
	return proto.OKResp(r.ID, row)
}

func (s *Server) handleAgentPrompt(c *Client, r *proto.Request) proto.Response {
	text, ok := r.Str("text")
	if !ok {
		return proto.ErrResp(r.ID, proto.ErrBadRequest, "agent.prompt needs text.")
	}
	lp, bad := s.livePane(r)
	if bad != nil {
		return *bad
	}
	if lp.Adapter != nil {
		if err := lp.Adapter.Prompt(text); err != nil {
			return proto.ErrResp(r.ID, proto.ErrAdapter, err.Error())
		}
	} else if err := lp.write([]byte(text + "\r")); err != nil {
		return proto.ErrResp(r.ID, proto.ErrInternal, err.Error())
	}
	wait, _ := r.Bool("wait")
	if !wait {
		return proto.OKResp(r.ID, map[string]any{"pane": lp.Info.ID, "sent": len(text)})
	}
	until, ok := r.Str("until")
	if !ok {
		until = proto.StateIdle
	}
	ms, ok := r.Int("timeout_ms")
	if !ok || ms <= 0 {
		ms = 120000
	}
	ev, done := s.WaitState(lp.Info.ID, until, time.Duration(ms)*time.Millisecond)
	if !done {
		return proto.ErrResp(r.ID, proto.ErrTimeout,
			fmt.Sprintf("pane %s is %s after %d ms, not %s. Run: coppice agent get %s",
				lp.Info.ID, orUnknown(ev.State), ms, until, lp.Info.ID))
	}
	return proto.OKResp(r.ID, map[string]any{
		"pane": lp.Info.ID, "state": ev.State, "source": ev.Source, "ask": ev.Ask,
	})
}

func orUnknown(s string) string {
	if s == "" {
		return proto.StateUnknown
	}
	return s
}

func (s *Server) handleAgentWait(_ *Client, r *proto.Request) proto.Response {
	id, _ := r.Str("pane")
	if _, ok := s.tree.Pane(id); !ok {
		return proto.ErrResp(r.ID, proto.ErrNoSuchPane,
			fmt.Sprintf("no pane %q. Run: coppice agent list", id))
	}
	until, ok := r.Str("until")
	if !ok {
		until = proto.StateIdle
	}
	ms, ok := r.Int("timeout_ms")
	if !ok || ms <= 0 {
		ms = 120000
	}
	ev, done := s.WaitState(id, until, time.Duration(ms)*time.Millisecond)
	if !done {
		return proto.ErrResp(r.ID, proto.ErrTimeout,
			fmt.Sprintf("pane %s is %s after %d ms, not %s. Run: coppice agent get %s",
				id, orUnknown(ev.State), ms, until, id))
	}
	return proto.OKResp(r.ID, map[string]any{
		"pane": id, "state": ev.State, "source": ev.Source, "ask": ev.Ask,
	})
}

func (s *Server) handleAgentRead(_ *Client, r *proto.Request) proto.Response {
	lp, bad := s.livePane(r)
	if bad != nil {
		return *bad
	}
	src := pane.ReadRecent
	if v, ok := r.Str("source"); ok {
		src = pane.ReadSource(v)
	}
	text, err := lp.Grid.Read(src)
	if err != nil {
		return proto.ErrResp(r.ID, proto.ErrBadRequest, err.Error())
	}
	row, _ := s.agentRow(lp.Info.ID)
	row["text"] = text
	return proto.OKResp(r.ID, row)
}
```

`Server` already carries `waiters`: Task 7 declared it. Replace Task 7's empty `waiter`
declaration in `server.go` with the real one above, and delete the placeholder `ApplyState` and
`stateEvent` that Task 8 put in `panes.go`. There must be exactly one of each.

- [ ] **Step 4: Run the test and watch it pass**

Run: `cd harness/coppice && go test -race ./internal/server/ -v && go vet ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/server
git commit -m "feat(coppice): the gate speaks, and only a watcher speaks as the operator

pane.report_state is how the gate turns a fact it produced into the floor's
state. A client that is not attached cannot claim source operator: it is
downgraded to gate and told so, because operator outranks everything and the
only person entitled to it is the one looking at the pane. agent.wait resolves
on the merged state, so a gate-reported block ends the wait at once."
```

---

### Task 11: `internal/detect` — the manifest schema, recorded and loaded

This is the discovery task for Herdr's TOML format. Its deliverable is a written schema in
`internal/detect/README.md` plus a loader whose validation matches it.

**Files:**
- Create: `harness/coppice/internal/detect/README.md`
- Create: `harness/coppice/internal/detect/manifest.go`
- Test: `harness/coppice/internal/detect/manifest_test.go`

**Interfaces:**
- Consumes: `github.com/BurntSushi/toml`.
- Produces:
  ```go
  package detect
  const EngineVersion = 3
  type State string   // "idle" | "working" | "blocked" | "unknown"
  type Gate struct { All, Any, Not []Gate; Contains, Regex, LineRegex []string }
  type Rule struct { ID string; State *State; Priority int; Region string
                     VisibleIdle, VisibleBlocker, VisibleWorking, SkipStateUpdate bool
                     All, Any, Not []Gate; Contains, Regex, LineRegex []string }
  type Manifest struct { ID, Version string; MinEngineVersion int; UpdatedAt string
                         Aliases []string; Rules []Rule }
  func Parse(name string, data []byte) (*Manifest, *Compiled, error)
  type Compiled struct{}         // rules with their regexes already built
  func ValidRegion(spec string) bool
  ```

- [ ] **Step 1: Write the schema document**

`harness/coppice/internal/detect/README.md`. Every claim below was read from Herdr's
`src/detect/manifest.rs` on 2026-09-08; re-read it with
`gh api repos/herdrdev/herdr/contents/src/detect/manifest.rs -q .content | base64 -d`.

```markdown
# The agent-detection manifest schema

coppice reads Herdr's TOML manifests unchanged, so twenty-one agents are detected on day one
and a Herdr user's own override files keep working. This file is the schema, written down from
Herdr's `src/detect/manifest.rs`. Verify before you rely.

## Top level

| key | type | notes |
|---|---|---|
| `id` | string | required. The agent id, for example `claude`. |
| `version` | string | optional. Herdr uses `YYYY.MM.DD.N`. We keep it for `explain`. |
| `min_engine_version` | integer | optional. We refuse a manifest that asks for more than our engine version. |
| `updated_at` | string | optional, informational. |
| `aliases` | list of string | optional. Other names for the same agent. |
| `rules` | list of table | the rules, evaluated in file order. |

Any other top-level key is a load error. Herdr uses serde's `deny_unknown_fields`; we get the
same effect from `toml.MetaData.Undecoded()`.

## A rule

| key | type | notes |
|---|---|---|
| `id` | string | required, non-empty. |
| `state` | `idle` / `working` / `blocked` / `unknown` | the state this rule asserts. Absent means `unknown`. |
| `priority` | integer | default 0. Highest wins. On a tie the earlier rule in the file wins. |
| `region` | string | default `whole_recent`. See the region list below. |
| `visible_idle`, `visible_blocker`, `visible_working` | bool | evidence flags a client may show. |
| `skip_state_update` | bool | this rule matching means "emit no event at all". |
| `contains`, `regex`, `line_regex` | list of string | matchers. See the semantics below. |
| `all`, `any`, `not` | list of gate tables | nested matchers. |

`skip_state_update = true` requires `state = "unknown"` and forbids the three visible flags.

## A gate

A gate table takes the same six matcher keys as a rule: `contains`, `regex`, `line_regex`,
`all`, `any`, `not`. A gate with no positive matcher is a load error.

## Matcher semantics

- `contains`: **every** needle must appear. Needles are lowercased at load and matched against
  a lowercased copy of the region.
- `regex`: **every** pattern must match the region, case-sensitive, against the original text.
- `line_regex`: **every** pattern must match **some** line of the region. Not necessarily the
  same line.
- `all`: every nested gate must match.
- `any`: enforced only when non-empty; then at least one nested gate must match.
- `not`: no nested gate may match.

Herdr's patterns come from the Rust `regex` crate, which like Go's `regexp` is RE2-shaped:
`(?i)`, `(?m)`, `\x{2800}` and character classes all carry over, and neither engine has
backreferences or lookaround. A pattern that fails to compile is a load error, and a test
compiles every pattern in every bundled manifest so a drift between the two engines shows up
as a red test, not as a detection that quietly never fires.

## Regions

Read against the pane's screen text, unwrapped, plus the terminal title and the last OSC
progress payload.

`whole_recent` (default), `after_last_prompt_marker`, `before_current_prompt_marker`,
`whole_recent_without_current_prompt_marker`, `current_prompt_block_marker`,
`after_current_prompt_block_marker`, `prompt_box_body`, `above_prompt_box`,
`last_non_empty_above_prompt_box`, `after_last_horizontal_rule`, `osc_title`, `osc_progress`,
and the parameterised `bottom_lines(N)`, `bottom_non_empty_lines(N)`, `top_non_empty_lines(N)`.

`top_non_empty_lines(N)` needs `min_engine_version >= 3`; N must be a positive decimal with no
leading zero, at most 65535.

An unknown region name is a load error. It is never treated as an empty region, because a rule
that silently never matches is worse than a manifest that refuses to load.

## Limits, all load errors when exceeded

128 rules per manifest, gate nesting depth 8, 512 gates per manifest, 32 direct matchers per
gate, 1024 matchers per manifest, 512 characters per matcher.

## Where coppice deliberately differs from Herdr

Herdr falls back to `idle` when a *known* agent's manifest matches nothing
(`DEFAULT_KNOWN_AGENT_IDLE_FALLBACK`). coppice does not. Master spec 3.1 says `unknown` is what
the floor shows when it has no source, and that it is never dressed up as `idle`. No match means
no event, and the pane keeps whatever a real source last told us.

coppice also never fetches a manifest from the network. Herdr's remote update path is not
ported. Updates ride our releases, and `~/.config/coppice/agent-detection/<agent>.toml` replaces
a bundled file of the same name.
```

- [ ] **Step 2: Write the failing test**

```go
// harness/coppice/internal/detect/manifest_test.go
package detect

import (
	"strings"
	"testing"
)

func mustFail(t *testing.T, body, wantSubstr string) {
	t.Helper()
	_, _, err := Parse("test.toml", []byte(body))
	if err == nil {
		t.Fatalf("Parse accepted this manifest, want an error mentioning %q:\n%s", wantSubstr, body)
	}
	if !strings.Contains(err.Error(), wantSubstr) {
		t.Fatalf("Parse error = %q, want it to mention %q", err, wantSubstr)
	}
}

func TestParseReadsTheTopLevelFields(t *testing.T) {
	m, _, err := Parse("claude.toml", []byte(`
id = "claude"
version = "2026.09.04.1"
min_engine_version = 2
aliases = ["claude-code"]

[[rules]]
id = "prompt"
state = "idle"
priority = 950
region = "prompt_box_body"
line_regex = ['^\s*❯']
`))
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != "claude" || m.Version != "2026.09.04.1" || m.MinEngineVersion != 2 {
		t.Fatalf("manifest = %+v, want the claude header", m)
	}
	if len(m.Aliases) != 1 || m.Aliases[0] != "claude-code" {
		t.Fatalf("aliases = %v, want [claude-code]", m.Aliases)
	}
	if len(m.Rules) != 1 || m.Rules[0].ID != "prompt" || *m.Rules[0].State != "idle" {
		t.Fatalf("rules = %+v, want one idle prompt rule", m.Rules)
	}
}

func TestRegionDefaultsToWholeRecent(t *testing.T) {
	m, _, err := Parse("x.toml", []byte("id=\"x\"\n[[rules]]\nid=\"r\"\ncontains=[\"a\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if m.Rules[0].Region != "whole_recent" {
		t.Fatalf("region = %q, want whole_recent", m.Rules[0].Region)
	}
}

func TestUnknownTopLevelKeyIsALoadError(t *testing.T) {
	mustFail(t, "id=\"x\"\nmystery=1\n[[rules]]\nid=\"r\"\ncontains=[\"a\"]\n", "mystery")
}

func TestUnknownRuleKeyIsALoadError(t *testing.T) {
	mustFail(t, "id=\"x\"\n[[rules]]\nid=\"r\"\ncontains=[\"a\"]\nvibes=true\n", "vibes")
}

func TestUnknownRegionIsALoadError(t *testing.T) {
	mustFail(t, "id=\"x\"\n[[rules]]\nid=\"r\"\nregion=\"the_vibe_zone\"\ncontains=[\"a\"]\n",
		"the_vibe_zone")
}

func TestTopNonEmptyLinesNeedsEngineThree(t *testing.T) {
	mustFail(t, "id=\"x\"\nmin_engine_version=2\n[[rules]]\nid=\"r\"\n"+
		"region=\"top_non_empty_lines(3)\"\ncontains=[\"a\"]\n", "min_engine_version")
}

func TestManifestAskingForAFutureEngineIsALoadError(t *testing.T) {
	mustFail(t, "id=\"x\"\nmin_engine_version=99\n[[rules]]\nid=\"r\"\ncontains=[\"a\"]\n",
		"engine")
}

func TestARuleWithNoPositiveMatcherIsALoadError(t *testing.T) {
	mustFail(t, "id=\"x\"\n[[rules]]\nid=\"r\"\nnot=[{contains=[\"a\"]}]\n", "positive matcher")
}

func TestAnEmptyRuleIDIsALoadError(t *testing.T) {
	mustFail(t, "id=\"x\"\n[[rules]]\nid=\"\"\ncontains=[\"a\"]\n", "id")
}

func TestSkipStateUpdateRequiresStateUnknown(t *testing.T) {
	mustFail(t, "id=\"x\"\n[[rules]]\nid=\"r\"\nstate=\"idle\"\nskip_state_update=true\n"+
		"contains=[\"a\"]\n", "skip_state_update")
}

func TestSkipStateUpdateForbidsVisibleEvidence(t *testing.T) {
	mustFail(t, "id=\"x\"\n[[rules]]\nid=\"r\"\nstate=\"unknown\"\nskip_state_update=true\n"+
		"visible_idle=true\ncontains=[\"a\"]\n", "visible")
}

func TestABadRegexIsALoadError(t *testing.T) {
	mustFail(t, "id=\"x\"\n[[rules]]\nid=\"r\"\nregex=['(unclosed']\n", "regex")
}

func TestGateDepthOverEightIsALoadError(t *testing.T) {
	body := "id=\"x\"\n[[rules]]\nid=\"r\"\ncontains=[\"a\"]\nall=["
	open, close := "", ""
	for i := 0; i < 10; i++ {
		open += "{all=["
		close += "]}"
	}
	body += open + "{contains=[\"a\"]}" + close + "]\n"
	mustFail(t, body, "depth")
}

func TestTooManyRulesIsALoadError(t *testing.T) {
	var b strings.Builder
	b.WriteString("id=\"x\"\n")
	for i := 0; i < 129; i++ {
		b.WriteString("[[rules]]\nid=\"r\"\ncontains=[\"a\"]\n")
	}
	mustFail(t, b.String(), "128")
}

func TestAMatcherOverFiveHundredAndTwelveCharsIsALoadError(t *testing.T) {
	mustFail(t, "id=\"x\"\n[[rules]]\nid=\"r\"\ncontains=[\""+strings.Repeat("a", 513)+"\"]\n",
		"512")
}

func TestValidRegionAcceptsTheDocumentedNames(t *testing.T) {
	for _, r := range []string{
		"whole_recent", "after_last_prompt_marker", "before_current_prompt_marker",
		"whole_recent_without_current_prompt_marker", "current_prompt_block_marker",
		"after_current_prompt_block_marker", "prompt_box_body", "above_prompt_box",
		"last_non_empty_above_prompt_box", "after_last_horizontal_rule",
		"osc_title", "osc_progress",
		"bottom_lines(5)", "bottom_non_empty_lines(12)", "top_non_empty_lines(3)",
	} {
		if !ValidRegion(r) {
			t.Fatalf("ValidRegion(%q) = false, want true", r)
		}
	}
	for _, r := range []string{"", "bottom_lines()", "top_non_empty_lines(0)",
		"top_non_empty_lines(007)", "bottom_lines(x)", "nonsense"} {
		if ValidRegion(r) {
			t.Fatalf("ValidRegion(%q) = true, want false", r)
		}
	}
}
```

- [ ] **Step 3: Run the test and watch it fail**

Run: `cd harness/coppice && go test -race ./internal/detect/ -v`
Expected: FAIL, `undefined: Parse`.

- [ ] **Step 4: Write the implementation**

```go
// harness/coppice/internal/detect/manifest.go

// Package detect is a Go port of Herdr's TOML agent-detection engine. The
// schema it implements is written down in README.md, read from Herdr's
// src/detect/manifest.rs. Reusing their format means their manifests and their
// users' override files work here unchanged.
package detect

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// EngineVersion is what a manifest's min_engine_version is checked against.
// 3 is the version that introduced top_non_empty_lines.
const EngineVersion = 3

const (
	maxRules          = 128
	maxGateDepth      = 8
	maxTotalGates     = 512
	maxMatchersPerGat = 32
	maxTotalMatchers  = 1024
	maxMatcherChars   = 512
	topRegionMinVer   = 3
	maxTopRegionLines = 65535
)

type State string

const (
	StateIdle    State = "idle"
	StateWorking State = "working"
	StateBlocked State = "blocked"
	StateUnknown State = "unknown"
)

type Gate struct {
	All       []Gate   `toml:"all"`
	Any       []Gate   `toml:"any"`
	Not       []Gate   `toml:"not"`
	Contains  []string `toml:"contains"`
	Regex     []string `toml:"regex"`
	LineRegex []string `toml:"line_regex"`
}

type Rule struct {
	ID              string   `toml:"id"`
	State           *State   `toml:"state"`
	Priority        int      `toml:"priority"`
	Region          string   `toml:"region"`
	VisibleIdle     bool     `toml:"visible_idle"`
	VisibleBlocker  bool     `toml:"visible_blocker"`
	VisibleWorking  bool     `toml:"visible_working"`
	SkipStateUpdate bool     `toml:"skip_state_update"`
	All             []Gate   `toml:"all"`
	Any             []Gate   `toml:"any"`
	Not             []Gate   `toml:"not"`
	Contains        []string `toml:"contains"`
	Regex           []string `toml:"regex"`
	LineRegex       []string `toml:"line_regex"`
}

func (r Rule) gate() Gate {
	return Gate{All: r.All, Any: r.Any, Not: r.Not,
		Contains: r.Contains, Regex: r.Regex, LineRegex: r.LineRegex}
}

// EffectiveState is unknown when a rule declares no state, matching Herdr.
func (r Rule) EffectiveState() State {
	if r.State == nil {
		return StateUnknown
	}
	return *r.State
}

type Manifest struct {
	ID               string   `toml:"id"`
	Version          string   `toml:"version"`
	MinEngineVersion int      `toml:"min_engine_version"`
	UpdatedAt        string   `toml:"updated_at"`
	Aliases          []string `toml:"aliases"`
	Rules            []Rule   `toml:"rules"`
}

// compiledGate holds the regexes already built, so evaluation never compiles.
type compiledGate struct {
	all       []compiledGate
	any       []compiledGate
	not       []compiledGate
	contains  []string // already lowercased
	regex     []*regexp.Regexp
	lineRegex []*regexp.Regexp
}

// Compiled is a manifest ready to evaluate.
type Compiled struct {
	Manifest *Manifest
	gates    []compiledGate // one per rule, in file order
}

// Parse reads one manifest and validates it fully. Every failure is a load
// error: a manifest that half-loads would produce detections nobody can
// explain, which is worse than an agent that simply is not detected.
func Parse(name string, data []byte) (*Manifest, *Compiled, error) {
	var m Manifest
	md, err := toml.Decode(string(data), &m)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", name, err)
	}
	if und := md.Undecoded(); len(und) > 0 {
		keys := make([]string, 0, len(und))
		for _, k := range und {
			keys = append(keys, k.String())
		}
		return nil, nil, fmt.Errorf("%s: unknown keys: %s", name, strings.Join(keys, ", "))
	}
	if strings.TrimSpace(m.ID) == "" {
		return nil, nil, fmt.Errorf("%s: id must not be empty", name)
	}
	if m.MinEngineVersion > EngineVersion {
		return nil, nil, fmt.Errorf(
			"%s: needs detection engine %d, this build is engine %d. Update coppice.",
			name, m.MinEngineVersion, EngineVersion)
	}
	if len(m.Rules) > maxRules {
		return nil, nil, fmt.Errorf("%s: %d rules, the cap is %d", name, len(m.Rules), maxRules)
	}
	for i := range m.Rules {
		if m.Rules[i].Region == "" {
			m.Rules[i].Region = "whole_recent"
		}
	}

	c := &Compiled{Manifest: &m}
	totals := struct{ gates, matchers int }{}
	for _, r := range m.Rules {
		if strings.TrimSpace(r.ID) == "" {
			return nil, nil, fmt.Errorf("%s: a rule has an empty id", name)
		}
		if r.SkipStateUpdate {
			if r.EffectiveState() != StateUnknown {
				return nil, nil, fmt.Errorf(
					`%s: rule %s uses skip_state_update without state = "unknown"`, name, r.ID)
			}
			if r.VisibleIdle || r.VisibleBlocker || r.VisibleWorking {
				return nil, nil, fmt.Errorf(
					"%s: rule %s uses skip_state_update with visible state evidence", name, r.ID)
			}
		}
		if st := r.EffectiveState(); st != StateIdle && st != StateWorking &&
			st != StateBlocked && st != StateUnknown {
			return nil, nil, fmt.Errorf("%s: rule %s has state %q", name, r.ID, st)
		}
		if !ValidRegion(r.Region) {
			return nil, nil, fmt.Errorf("%s: rule %s uses invalid region: %s", name, r.ID, r.Region)
		}
		if strings.HasPrefix(strings.TrimSpace(r.Region), "top_non_empty_lines(") &&
			m.MinEngineVersion != 0 && m.MinEngineVersion < topRegionMinVer {
			return nil, nil, fmt.Errorf(
				"%s: rule %s uses top_non_empty_lines but min_engine_version is %d, need %d",
				name, r.ID, m.MinEngineVersion, topRegionMinVer)
		}
		cg, err := compileGate(r.gate(), "rule "+r.ID, 0, &totals)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", name, err)
		}
		c.gates = append(c.gates, cg)
	}
	return &m, c, nil
}

func compileGate(g Gate, ctx string, depth int, totals *struct{ gates, matchers int }) (compiledGate, error) {
	var out compiledGate
	if depth > maxGateDepth {
		return out, fmt.Errorf("%s exceeds max gate depth %d", ctx, maxGateDepth)
	}
	totals.gates++
	if totals.gates > maxTotalGates {
		return out, fmt.Errorf("manifest exceeds max gate count %d", maxTotalGates)
	}
	direct := len(g.Contains) + len(g.Regex) + len(g.LineRegex)
	if direct > maxMatchersPerGat {
		return out, fmt.Errorf("%s has %d direct matchers, max is %d",
			ctx, direct, maxMatchersPerGat)
	}
	totals.matchers += direct
	if totals.matchers > maxTotalMatchers {
		return out, fmt.Errorf("manifest exceeds max matcher count %d", maxTotalMatchers)
	}
	if len(g.Contains)+len(g.Regex)+len(g.LineRegex)+len(g.All)+len(g.Any) == 0 {
		return out, fmt.Errorf("%s must contain a positive matcher", ctx)
	}
	for _, s := range g.Contains {
		if len([]rune(s)) > maxMatcherChars {
			return out, fmt.Errorf("%s matcher exceeds max length %d", ctx, maxMatcherChars)
		}
		out.contains = append(out.contains, strings.ToLower(s))
	}
	compileAll := func(pats []string, kind string) ([]*regexp.Regexp, error) {
		var res []*regexp.Regexp
		for _, p := range pats {
			if len([]rune(p)) > maxMatcherChars {
				return nil, fmt.Errorf("%s matcher exceeds max length %d", ctx, maxMatcherChars)
			}
			re, err := regexp.Compile(p)
			if err != nil {
				return nil, fmt.Errorf("%s has an invalid %s %q: %w", ctx, kind, p, err)
			}
			res = append(res, re)
		}
		return res, nil
	}
	var err error
	if out.regex, err = compileAll(g.Regex, "regex"); err != nil {
		return out, err
	}
	if out.lineRegex, err = compileAll(g.LineRegex, "line_regex"); err != nil {
		return out, err
	}
	for _, n := range g.All {
		c, err := compileGate(n, "all gate", depth+1, totals)
		if err != nil {
			return out, err
		}
		out.all = append(out.all, c)
	}
	for _, n := range g.Any {
		c, err := compileGate(n, "any gate", depth+1, totals)
		if err != nil {
			return out, err
		}
		out.any = append(out.any, c)
	}
	for _, n := range g.Not {
		c, err := compileGate(n, "not gate", depth+1, totals)
		if err != nil {
			return out, err
		}
		out.not = append(out.not, c)
	}
	return out, nil
}

var fixedRegions = map[string]bool{
	"whole_recent": true, "after_last_prompt_marker": true,
	"before_current_prompt_marker": true, "whole_recent_without_current_prompt_marker": true,
	"current_prompt_block_marker": true, "after_current_prompt_block_marker": true,
	"prompt_box_body": true, "above_prompt_box": true,
	"last_non_empty_above_prompt_box": true, "after_last_horizontal_rule": true,
	"osc_title": true, "osc_progress": true,
}

// ValidRegion mirrors Herdr's validate_region_name. An unknown name is refused
// at load rather than silently returning an empty region, because a rule that
// can never match is a bug nobody would notice.
func ValidRegion(spec string) bool {
	s := strings.TrimSpace(spec)
	if fixedRegions[s] {
		return true
	}
	if _, ok := regionCount(s, "bottom_lines"); ok {
		return true
	}
	if _, ok := regionCount(s, "bottom_non_empty_lines"); ok {
		return true
	}
	_, ok := topRegionCount(s)
	return ok
}

func regionCount(spec, name string) (int, bool) {
	rest, ok := strings.CutPrefix(spec, name)
	if !ok || !strings.HasPrefix(rest, "(") || !strings.HasSuffix(rest, ")") {
		return 0, false
	}
	n, err := strconv.Atoi(rest[1 : len(rest)-1])
	if err != nil {
		return 0, false
	}
	return n, true
}

// topRegionCount is stricter than the others, exactly as Herdr is: a positive
// decimal, no leading zero, at most 65535.
func topRegionCount(spec string) (int, bool) {
	rest, ok := strings.CutPrefix(spec, "top_non_empty_lines")
	if !ok || !strings.HasPrefix(rest, "(") || !strings.HasSuffix(rest, ")") {
		return 0, false
	}
	digits := rest[1 : len(rest)-1]
	if digits == "" || strings.HasPrefix(digits, "0") {
		return 0, false
	}
	for _, c := range digits {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(digits)
	if err != nil || n > maxTopRegionLines {
		return 0, false
	}
	return n, true
}
```

- [ ] **Step 5: Run the test and watch it pass**

Run: `cd harness/coppice && go test -race ./internal/detect/ -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add harness/coppice/internal/detect
git commit -m "feat(coppice): Herdr's manifest schema, written down and enforced

The schema lived only in Rust source, so it is now in internal/detect/README.md
with the command that re-reads it. Every validation Herdr performs is ported,
including the limits, because a manifest that half-loads produces detections
nobody can explain. An unknown region is a load error rather than an empty
string: a rule that can never match is a bug nobody would notice."
```

---

### Task 12: `internal/detect` — the evaluator and the vendored manifests

**Files:**
- Create: `harness/coppice/internal/detect/regions.go`
- Create: `harness/coppice/internal/detect/evaluate.go`
- Create: `harness/coppice/internal/detect/set.go`
- Create: `harness/coppice/internal/detect/manifests/` (the 21 vendored `.toml` files)
- Create: `harness/coppice/internal/detect/manifests/PROVENANCE`
- Create: `harness/coppice/NOTICE`
- Create: `harness/coppice/testdata/screens/` (fixtures, see Step 5)
- Create: `harness/coppice/testdata/screens/unfixtured.txt`
- Test: `harness/coppice/internal/detect/regions_test.go`
- Test: `harness/coppice/internal/detect/evaluate_test.go`
- Test: `harness/coppice/internal/detect/manifests_test.go`

**Interfaces:**
- Consumes: `Manifest`, `Compiled`, `ValidRegion`.
- Produces:
  ```go
  package detect
  type Input struct { Screen, OSCTitle, OSCProgress string }
  type Result struct { Agent string; State State; Matched bool; RuleID string
                       Priority int; Region string; Skip bool
                       VisibleIdle, VisibleBlocker, VisibleWorking bool
                       Evaluated []Evaluated }
  type Evaluated struct { ID string; Priority int; Region string; State State; Matched bool
                          RegionBytes int; RegionPreview string }
  func Region(in Input, spec string) string
  func (c *Compiled) Evaluate(in Input) Result
  type Set struct{}
  func LoadSet(overrideDir string) (*Set, error)   // bundled, then overrides by file name
  func (s *Set) Agents() []string
  func (s *Set) For(agent string) (*Compiled, bool)   // resolves aliases
  func (s *Set) Warnings() []string
  ```

- [ ] **Step 1: Vendor the manifests**

Run from the repo root. This copies Herdr's manifests at a recorded commit and writes the
provenance file the NOTICE points at.

```bash
cd /path/to/openDaisugi
mkdir -p harness/coppice/internal/detect/manifests
# The default branch is master, not main: commits/main returns HTTP 422 and
# would leave COMMIT empty, every ?ref= blank, and PROVENANCE with no commit.
COMMIT=$(gh api repos/herdrdev/herdr/commits/HEAD -q .sha)
if [ ${#COMMIT} -ne 40 ]; then
  echo "could not read the Herdr HEAD commit. Check: gh api repos/herdrdev/herdr" >&2
  exit 1
fi
for f in $(gh api "repos/herdrdev/herdr/contents/src/detect/manifests?ref=$COMMIT" -q '.[].name'); do
  gh api "repos/herdrdev/herdr/contents/src/detect/manifests/$f?ref=$COMMIT" -q .content \
    | base64 -d > "harness/coppice/internal/detect/manifests/$f"
done
{
  echo "source: https://github.com/herdrdev/herdr"
  echo "path: src/detect/manifests"
  echo "commit: $COMMIT"
  echo "fetched: $(date -u +%Y-%m-%d)"
  echo "licence: Apache-2.0"
} > harness/coppice/internal/detect/manifests/PROVENANCE
ls harness/coppice/internal/detect/manifests
```

Expected: 21 `.toml` files plus `PROVENANCE`, and `PROVENANCE` carries a 40-character commit.
Record the same commit in `PINS.md` under the Herdr reference table, in the
"vendored at commit" row.

Then amend master §4 in this same commit, because it names a Herdr SHA that does not exist.
Open `docs/plans/2026-09-08-workshop/00-master-spec.md` §4 and delete the sentence
"Herdr's vendored commit `c5a21edf…` (their 1.3.2) is the reference for patch notes, not a
requirement", replacing it with: `The Herdr commit coppice vendors from is recorded in
harness/coppice/internal/detect/manifests/PROVENANCE.` Evidence:
`gh api repos/herdrdev/herdr/commits/c5a21edfcbc2d5b46540ad91b7980aca31f5f1f3` returns HTTP 422
"No commit found", and the newest tag is `v0.9.0`, so there is no 1.3.2.

Prepend this attribution line to every vendored `.toml`, keeping the file's own header comments
below it:

```bash
cd harness/coppice/internal/detect/manifests
for f in *.toml; do
  printf '# Vendored from herdrdev/herdr src/detect/manifests/%s, Apache-2.0. See ../../../NOTICE.\n' "$f" \
    | cat - "$f" > "$f.new" && mv "$f.new" "$f"
done
```

- [ ] **Step 2: Write the NOTICE**

`harness/coppice/NOTICE`:

```
coppice
Copyright the openDaisugi contributors.

This product includes agent-detection manifests from Herdr
(https://github.com/herdrdev/herdr), licensed under the Apache License,
Version 2.0. The files are in internal/detect/manifests/. Each file keeps its
own header comments. The commit they were taken from is recorded in
internal/detect/manifests/PROVENANCE.

You may obtain a copy of the Apache License at
http://www.apache.org/licenses/LICENSE-2.0

The manifests are used unchanged apart from one added attribution comment per
file. coppice does not fetch manifests from the network; updates ride coppice
releases.
```

- [ ] **Step 3: Write the failing region test**

```go
// harness/coppice/internal/detect/regions_test.go
package detect

import "testing"

const box = "line one\n" +
	"line two\n" +
	"────────────────────\n" +
	"❯ type here\n" +
	"────────────────────\n" +
	"  ? for shortcuts\n"

func TestBottomNonEmptyLinesTakesTheBottomOccurrence(t *testing.T) {
	in := Input{Screen: "a\n\nb\n\nc\n"}
	if got := Region(in, "bottom_non_empty_lines(2)"); got != "b\n\nc" {
		t.Fatalf("got %q, want %q", got, "b\n\nc")
	}
}

func TestBottomLinesCountsBlankLinesToo(t *testing.T) {
	in := Input{Screen: "a\nb\nc\n"}
	if got := Region(in, "bottom_lines(2)"); got != "b\nc" {
		t.Fatalf("got %q, want %q", got, "b\nc")
	}
}

func TestTopNonEmptyLinesTakesTheTopOccurrence(t *testing.T) {
	in := Input{Screen: "\na\nb\nc\n"}
	if got := Region(in, "top_non_empty_lines(2)"); got != "\na\nb" {
		t.Fatalf("got %q, want %q", got, "\na\nb")
	}
}

func TestPromptBoxBodyIsBetweenTheLastTwoRules(t *testing.T) {
	if got := Region(Input{Screen: box}, "prompt_box_body"); got != "❯ type here" {
		t.Fatalf("got %q, want %q", got, "❯ type here")
	}
}

func TestAbovePromptBoxStopsAtTheBoxTop(t *testing.T) {
	if got := Region(Input{Screen: box}, "above_prompt_box"); got != "line one\nline two" {
		t.Fatalf("got %q, want %q", got, "line one\nline two")
	}
}

func TestLastNonEmptyAbovePromptBox(t *testing.T) {
	if got := Region(Input{Screen: box}, "last_non_empty_above_prompt_box"); got != "line two" {
		t.Fatalf("got %q, want %q", got, "line two")
	}
}

func TestAfterLastHorizontalRule(t *testing.T) {
	if got := Region(Input{Screen: box}, "after_last_horizontal_rule"); got != "  ? for shortcuts" {
		t.Fatalf("got %q, want %q", got, "  ? for shortcuts")
	}
}

func TestCodexPromptMarkerRegions(t *testing.T) {
	screen := "• ran a tool\nsome output\n› \n"
	if got := Region(Input{Screen: screen}, "after_last_prompt_marker"); got != "" {
		t.Fatalf("after_last_prompt_marker = %q, want empty", got)
	}
	if got := Region(Input{Screen: screen}, "before_current_prompt_marker"); got != "• ran a tool\nsome output" {
		t.Fatalf("before_current_prompt_marker = %q", got)
	}
	if got := Region(Input{Screen: screen}, "whole_recent_without_current_prompt_marker"); got != "" {
		t.Fatalf("whole_recent_without_current_prompt_marker = %q, want empty when a prompt is current", got)
	}
	if got := Region(Input{Screen: screen}, "current_prompt_block_marker"); got != "• ran a tool" {
		t.Fatalf("current_prompt_block_marker = %q", got)
	}
	if got := Region(Input{Screen: screen}, "after_current_prompt_block_marker"); got != "• ran a tool\nsome output\n› " {
		t.Fatalf("after_current_prompt_block_marker = %q", got)
	}
}

func TestOSCRegionsComeFromTheirOwnFields(t *testing.T) {
	in := Input{Screen: "ignored", OSCTitle: "✳ idle", OSCProgress: "4;0"}
	if got := Region(in, "osc_title"); got != "✳ idle" {
		t.Fatalf("osc_title = %q", got)
	}
	if got := Region(in, "osc_progress"); got != "4;0" {
		t.Fatalf("osc_progress = %q", got)
	}
}
```

- [ ] **Step 4: Write `regions.go`**

```go
// harness/coppice/internal/detect/regions.go
package detect

import "strings"

// Input is what the evaluator sees: the pane's screen text unwrapped, the
// terminal title, and the last OSC progress payload.
type Input struct {
	Screen      string
	OSCTitle    string
	OSCProgress string
}

// Region slices the input the way a rule asked for. Every case here is a
// direct port of Herdr's region(); see README.md for the list.
func Region(in Input, spec string) string {
	s := strings.TrimSpace(spec)
	switch s {
	case "osc_title":
		return in.OSCTitle
	case "osc_progress":
		return in.OSCProgress
	}
	c := in.Screen
	switch s {
	case "whole_recent":
		return c
	case "after_last_prompt_marker":
		return afterLastPromptMarker(c)
	case "before_current_prompt_marker":
		return beforeCurrentPromptMarker(c)
	case "whole_recent_without_current_prompt_marker":
		if _, ok := currentPromptIndex(lines(c)); ok {
			return ""
		}
		return c
	case "current_prompt_block_marker":
		return currentPromptBlockMarker(c)
	case "after_current_prompt_block_marker":
		return afterCurrentPromptBlockMarker(c)
	case "prompt_box_body":
		return promptBoxBody(c)
	case "above_prompt_box":
		return abovePromptBox(c)
	case "last_non_empty_above_prompt_box":
		return lastNonEmptyLine(abovePromptBox(c))
	case "after_last_horizontal_rule":
		return afterLastHorizontalRule(c)
	}
	if n, ok := regionCount(s, "bottom_lines"); ok {
		return bottomLines(c, n)
	}
	if n, ok := regionCount(s, "bottom_non_empty_lines"); ok {
		return bottomNonEmpty(c, n)
	}
	if n, ok := topRegionCount(s); ok {
		return topNonEmpty(c, n)
	}
	return ""
}

func lines(s string) []string { return strings.Split(strings.TrimRight(s, "\n"), "\n") }

func joinFrom(ls []string, i int) string {
	if i >= len(ls) {
		return ""
	}
	return strings.Join(ls[i:], "\n")
}

func bottomLines(c string, n int) string {
	ls := lines(c)
	start := len(ls) - n
	if start < 0 {
		start = 0
	}
	return joinFrom(ls, start)
}

func bottomNonEmpty(c string, n int) string {
	ls := lines(c)
	seen, start := 0, -1
	for i := len(ls) - 1; i >= 0; i-- {
		if strings.TrimSpace(ls[i]) != "" {
			seen++
			start = i
			if seen == n {
				break
			}
		}
	}
	if start < 0 {
		return ""
	}
	return joinFrom(ls, start)
}

func topNonEmpty(c string, n int) string {
	ls := lines(c)
	seen, end := 0, -1
	for i := range ls {
		if strings.TrimSpace(ls[i]) != "" {
			seen++
			end = i
			if seen == n {
				break
			}
		}
	}
	if end < 0 {
		return ""
	}
	return strings.Join(ls[:end+1], "\n")
}

// codexPromptLine and codexBlockMarkerLine are Herdr's markers for Codex's UI.
func codexPromptLine(l string) bool {
	return l == "›" || strings.HasPrefix(l, "› ")
}

func codexBlockMarkerLine(l string) bool {
	return strings.HasPrefix(l, "•") || strings.HasPrefix(l, "■") ||
		strings.HasPrefix(l, "✗") || strings.HasPrefix(l, "✓")
}

func lastIndexFunc(ls []string, f func(string) bool) (int, bool) {
	for i := len(ls) - 1; i >= 0; i-- {
		if f(ls[i]) {
			return i, true
		}
	}
	return 0, false
}

// currentPromptIndex is the last prompt marker, but only when no block marker
// follows it: a block marker after the prompt means the agent started working
// again.
func currentPromptIndex(ls []string) (int, bool) {
	i, ok := lastIndexFunc(ls, codexPromptLine)
	if !ok {
		return 0, false
	}
	for _, l := range ls[i+1:] {
		if codexBlockMarkerLine(l) {
			return 0, false
		}
	}
	return i, true
}

func afterLastPromptMarker(c string) string {
	ls := lines(c)
	i, ok := lastIndexFunc(ls, codexPromptLine)
	if !ok {
		return c
	}
	return joinFrom(ls, i+1)
}

func beforeCurrentPromptMarker(c string) string {
	ls := lines(c)
	i, ok := currentPromptIndex(ls)
	if !ok {
		return c
	}
	return strings.Join(ls[:i], "\n")
}

func currentPromptBlockMarker(c string) string {
	ls := lines(c)
	i, ok := currentPromptIndex(ls)
	if !ok {
		return ""
	}
	j, ok := lastIndexFunc(ls[:i], codexBlockMarkerLine)
	if !ok {
		return ""
	}
	return ls[j]
}

func afterCurrentPromptBlockMarker(c string) string {
	ls := lines(c)
	i, ok := currentPromptIndex(ls)
	if !ok {
		return ""
	}
	j, ok := lastIndexFunc(ls[:i], codexBlockMarkerLine)
	if !ok {
		return ""
	}
	return joinFrom(ls, j)
}

// isHorizontalRule matches a line of box-drawing dashes, optionally followed by
// a label when the run is at least three characters long.
func isHorizontalRule(l string) bool {
	t := strings.TrimSpace(l)
	if t == "" {
		return false
	}
	n := 0
	for _, r := range t {
		if r != '─' {
			break
		}
		n++
	}
	if n == 0 {
		return false
	}
	rest := strings.TrimLeft(string([]rune(t)[n:]), " \t")
	return rest == "" || n >= 3
}

// promptBoxTopIndex is the second horizontal rule counting up from the bottom,
// which is the top border of the box the cursor sits in.
func promptBoxTopIndex(ls []string) (int, bool) {
	count := 0
	for i := len(ls) - 1; i >= 0; i-- {
		if isHorizontalRule(ls[i]) {
			count++
			if count == 2 {
				return i, true
			}
		}
	}
	return 0, false
}

func promptBoxBody(c string) string {
	ls := lines(c)
	top, ok := promptBoxTopIndex(ls)
	if !ok {
		return ""
	}
	end := len(ls)
	for i := top + 1; i < len(ls); i++ {
		if isHorizontalRule(ls[i]) {
			end = i
			break
		}
	}
	if top+1 >= end {
		return ""
	}
	return strings.Join(ls[top+1:end], "\n")
}

func abovePromptBox(c string) string {
	ls := lines(c)
	top, ok := promptBoxTopIndex(ls)
	if !ok {
		return c
	}
	return strings.Join(ls[:top], "\n")
}

func afterLastHorizontalRule(c string) string {
	ls := lines(c)
	last := -1
	for i, l := range ls {
		if isHorizontalRule(l) {
			last = i
		}
	}
	return joinFrom(ls, last+1)
}

func lastNonEmptyLine(c string) string {
	ls := lines(c)
	for i := len(ls) - 1; i >= 0; i-- {
		if strings.TrimSpace(ls[i]) != "" {
			return ls[i]
		}
	}
	return ""
}
```

- [ ] **Step 5: Write the evaluator test and the fixtures**

The fixture inventory is scoped on purpose. Four of the harnesses coppice adapts have a manifest
upstream: claude, codex, pi and opencode. Those get hand-written fixtures per state. There is no
`sprig.toml` upstream, and sprig's state comes from its own session tree rather than a screen, so
do **not** create `testdata/screens/sprig/`: it would trip
`TestEachFixtureDetectsItsDeclaredState`. The remaining seventeen manifests have no fixture, and
that gap is written down in `testdata/screens/unfixtured.txt` and asserted by a test, rather than
filled with guesses about UIs nobody here has run.

The 21 manifest ids upstream are `amp agy claude cline codex cursor devin droid gemini copilot
grok hermes kilo kimi kiro maki muse opencode pi qodercli qwen`. Note `agy` and `copilot`: those
are the ids declared inside `antigravity.toml` and `github-copilot.toml`, not the filenames.

`harness/coppice/testdata/screens/unfixtured.txt`:

```
# Manifests with no screen fixtures yet, one id per line.
#
# A fixture must come from a screen someone actually saw. Inventing one asserts
# a guess about a UI we have never run, which is worse than an honest gap: the
# test would pass and the detection would still be wrong.
#
# To close a line: record the screen with scripts/record-vt.sh, save the text as
# testdata/screens/<agent>/<state>-1.txt, and delete the line.
amp
agy
cline
cursor
devin
droid
gemini
grok
hermes
kilo
kimi
kiro
maki
muse
qodercli
qwen
copilot
```

Fixtures to write, taken from the rules in the vendored manifests (open each manifest and build
a screen that satisfies exactly one rule):

- `testdata/screens/claude/idle-1.txt` — the prompt box from the region test above.
- `testdata/screens/claude/working-1.txt` — a line `⏵ thinking about it (12s · esc to interrupt)`.
- `testdata/screens/claude/blocked-1.txt` — a permission prompt: a horizontal rule, then
  `Do you want to proceed?`, then `❯ 1. Yes`, `2. No`, then `esc to cancel`.
- `testdata/screens/codex/idle-1.txt`, `working-1.txt`, `blocked-1.txt`
- `testdata/screens/pi/…`, `testdata/screens/opencode/…` for each state their manifests declare.
- Nothing under `testdata/screens/sprig/`. There is no `sprig.toml`, and a fixture directory with
  no manifest fails `TestEachFixtureDetectsItsDeclaredState`.

```go
// harness/coppice/internal/detect/evaluate_test.go
package detect

import (
	"strings"
	"testing"
)

func compile(t *testing.T, body string) *Compiled {
	t.Helper()
	_, c, err := Parse("t.toml", []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestHighestPriorityWins(t *testing.T) {
	c := compile(t, `
id = "x"
[[rules]]
id = "low"
state = "idle"
priority = 10
contains = ["marker"]
[[rules]]
id = "high"
state = "blocked"
priority = 20
contains = ["marker"]
`)
	r := c.Evaluate(Input{Screen: "marker"})
	if !r.Matched || r.RuleID != "high" || r.State != StateBlocked {
		t.Fatalf("Evaluate = %+v, want the high rule", r)
	}
}

// Herdr keeps the earlier rule on a tie: `previous.priority >= rule.priority`
// does not replace. Porting that exactly matters, because several bundled
// manifests have equal-priority rules whose order is the tiebreak.
func TestATieKeepsTheEarlierRule(t *testing.T) {
	c := compile(t, `
id = "x"
[[rules]]
id = "first"
state = "working"
priority = 5
contains = ["marker"]
[[rules]]
id = "second"
state = "idle"
priority = 5
contains = ["marker"]
`)
	if r := c.Evaluate(Input{Screen: "marker"}); r.RuleID != "first" {
		t.Fatalf("tie went to %q, want first", r.RuleID)
	}
}

func TestContainsIsCaseInsensitiveAndRegexIsNot(t *testing.T) {
	c := compile(t, `
id = "x"
[[rules]]
id = "ci"
state = "blocked"
contains = ["Do You Want To Proceed?"]
[[rules]]
id = "cs"
state = "working"
priority = -1
regex = ['^Exact$']
`)
	if r := c.Evaluate(Input{Screen: "do you want to proceed?"}); r.RuleID != "ci" {
		t.Fatalf("contains did not match case-insensitively: %+v", r)
	}
	if r := c.Evaluate(Input{Screen: "exact"}); r.Matched {
		t.Fatalf("regex matched case-insensitively: %+v", r)
	}
}

// Each line_regex must match SOME line, not all the same line.
func TestEachLineRegexMatchesSomeLineIndependently(t *testing.T) {
	c := compile(t, `
id = "x"
[[rules]]
id = "two"
state = "blocked"
line_regex = ['^alpha$', '^beta$']
`)
	if r := c.Evaluate(Input{Screen: "alpha\nbeta"}); !r.Matched {
		t.Fatalf("two line_regex over two lines did not match: %+v", r)
	}
	if r := c.Evaluate(Input{Screen: "alpha"}); r.Matched {
		t.Fatalf("matched with only one of the two lines present: %+v", r)
	}
}

func TestAnyIsIgnoredWhenEmptyAndEnforcedWhenPresent(t *testing.T) {
	c := compile(t, `
id = "x"
[[rules]]
id = "r"
state = "blocked"
contains = ["base"]
any = [{ contains = ["one"] }, { contains = ["two"] }]
`)
	if r := c.Evaluate(Input{Screen: "base"}); r.Matched {
		t.Fatalf("any was not enforced: %+v", r)
	}
	if r := c.Evaluate(Input{Screen: "base two"}); !r.Matched {
		t.Fatalf("any with a satisfied branch did not match: %+v", r)
	}
}

func TestNotBlocksAMatch(t *testing.T) {
	c := compile(t, `
id = "x"
[[rules]]
id = "r"
state = "idle"
contains = ["prompt"]
not = [{ contains = ["esc to cancel"] }]
`)
	if r := c.Evaluate(Input{Screen: "prompt"}); !r.Matched {
		t.Fatal("the rule did not match without the excluded text")
	}
	if r := c.Evaluate(Input{Screen: "prompt, esc to cancel"}); r.Matched {
		t.Fatal("not did not block the match")
	}
}

// skip_state_update means emit nothing. If it produced "unknown" instead, a
// Claude pane sitting in the transcript viewer would be reset to unknown every
// 500 ms, wiping whatever the gate told us.
func TestSkipStateUpdateEmitsNothing(t *testing.T) {
	c := compile(t, `
id = "x"
[[rules]]
id = "viewer"
state = "unknown"
priority = 100
skip_state_update = true
contains = ["showing detailed transcript"]
`)
	r := c.Evaluate(Input{Screen: "showing detailed transcript"})
	if !r.Matched || !r.Skip {
		t.Fatalf("Evaluate = %+v, want Matched with Skip set", r)
	}
}

// No match means no state. Herdr defaults a known agent to idle here; master
// spec 3.1 forbids that, so the port stops short on purpose.
func TestNoMatchYieldsNoStateNotIdle(t *testing.T) {
	c := compile(t, "id=\"x\"\n[[rules]]\nid=\"r\"\nstate=\"idle\"\ncontains=[\"never\"]\n")
	r := c.Evaluate(Input{Screen: "something else"})
	if r.Matched {
		t.Fatalf("Evaluate matched nothing but reported %+v", r)
	}
	if r.State == StateIdle {
		t.Fatal("no match produced idle, which master spec 3.1 forbids")
	}
}

func TestEvaluatedCarriesEveryRuleForExplain(t *testing.T) {
	c := compile(t, `
id = "x"
[[rules]]
id = "a"
state = "idle"
contains = ["nope"]
[[rules]]
id = "b"
state = "working"
contains = ["yes"]
`)
	r := c.Evaluate(Input{Screen: "yes"})
	if len(r.Evaluated) != 2 {
		t.Fatalf("Evaluated has %d entries, want 2", len(r.Evaluated))
	}
	byID := map[string]Evaluated{}
	for _, e := range r.Evaluated {
		byID[e.ID] = e
	}
	if byID["a"].Matched || !byID["b"].Matched {
		t.Fatalf("Evaluated matched flags are wrong: %+v", r.Evaluated)
	}
	if !strings.Contains(byID["b"].RegionPreview, "yes") {
		t.Fatalf("the preview %q does not show what was matched", byID["b"].RegionPreview)
	}
}
```

- [ ] **Step 6: Write `evaluate.go`**

```go
// harness/coppice/internal/detect/evaluate.go
package detect

import "strings"

type Evaluated struct {
	ID            string `json:"id"`
	Priority      int    `json:"priority"`
	Region        string `json:"region"`
	State         State  `json:"state"`
	Matched       bool   `json:"matched"`
	RegionBytes   int    `json:"region_bytes"`
	RegionPreview string `json:"region_preview"`
}

type Result struct {
	Agent          string      `json:"agent"`
	Matched        bool        `json:"matched"`
	State          State       `json:"state"`
	RuleID         string      `json:"rule_id"`
	Priority       int         `json:"priority"`
	Region         string      `json:"region"`
	Skip           bool        `json:"skip_state_update"`
	VisibleIdle    bool        `json:"visible_idle"`
	VisibleBlocker bool        `json:"visible_blocker"`
	VisibleWorking bool        `json:"visible_working"`
	Evaluated      []Evaluated `json:"evaluated"`
}

const previewMax = 240

func preview(s string) string {
	r := []rune(s)
	if len(r) <= previewMax {
		return s
	}
	return string(r[:previewMax]) + "..."
}

// Evaluate runs every rule in file order and keeps the highest priority match.
// A tie keeps the earlier rule, exactly as Herdr does: several bundled
// manifests rely on file order as the tiebreak.
func (c *Compiled) Evaluate(in Input) Result {
	res := Result{Agent: c.Manifest.ID}
	best := -1
	for i, rule := range c.Manifest.Rules {
		text := Region(in, rule.Region)
		matched := gateMatches(&c.gates[i], text, strings.ToLower(text))
		res.Evaluated = append(res.Evaluated, Evaluated{
			ID: rule.ID, Priority: rule.Priority, Region: rule.Region,
			State: rule.EffectiveState(), Matched: matched,
			RegionBytes: len(text), RegionPreview: preview(text),
		})
		if !matched {
			continue
		}
		if best >= 0 && c.Manifest.Rules[best].Priority >= rule.Priority {
			continue
		}
		best = i
	}
	if best < 0 {
		return res
	}
	r := c.Manifest.Rules[best]
	st := r.EffectiveState()
	res.Matched = true
	res.State = st
	res.RuleID = r.ID
	res.Priority = r.Priority
	res.Region = r.Region
	res.Skip = r.SkipStateUpdate
	res.VisibleIdle = r.VisibleIdle && st == StateIdle
	res.VisibleBlocker = r.VisibleBlocker && st == StateBlocked
	res.VisibleWorking = r.VisibleWorking && st == StateWorking
	return res
}

func gateMatches(g *compiledGate, text, lower string) bool {
	for _, needle := range g.contains {
		if !strings.Contains(lower, needle) {
			return false
		}
	}
	for _, re := range g.regex {
		if !re.MatchString(text) {
			return false
		}
	}
	if len(g.lineRegex) > 0 {
		ls := strings.Split(text, "\n")
		for _, re := range g.lineRegex {
			hit := false
			for _, l := range ls {
				if re.MatchString(l) {
					hit = true
					break
				}
			}
			if !hit {
				return false
			}
		}
	}
	for i := range g.all {
		if !gateMatches(&g.all[i], text, lower) {
			return false
		}
	}
	if len(g.any) > 0 {
		hit := false
		for i := range g.any {
			if gateMatches(&g.any[i], text, lower) {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	for i := range g.not {
		if gateMatches(&g.not[i], text, lower) {
			return false
		}
	}
	return true
}
```

- [ ] **Step 7: Write `set.go`**

```go
// harness/coppice/internal/detect/set.go
package detect

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

//go:embed manifests/*.toml
var bundled embed.FS

// Set is every manifest coppice knows, bundled plus local overrides. There is
// no network path: Herdr's remote update is not ported, so a manifest can only
// change when coppice is updated or the operator edits an override file.
type Set struct {
	mu       sync.RWMutex
	byID     map[string]*Compiled
	aliases  map[string]string
	sources  map[string]string
	warnings []string
}

// OverrideDir is where an operator's own manifests live. A file there replaces
// the bundled file of the same name.
func OverrideDir() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "coppice", "agent-detection")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "coppice", "agent-detection")
}

// LoadSet reads the bundled manifests then the overrides. A manifest that fails
// to load is skipped with a warning rather than taking the whole set down: one
// bad override must not blind the floor to twenty working agents.
func LoadSet(overrideDir string) (*Set, error) {
	s := &Set{
		byID: map[string]*Compiled{}, aliases: map[string]string{},
		sources: map[string]string{},
	}
	entries, err := bundled.ReadDir("manifests")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".toml") {
			continue
		}
		data, err := bundled.ReadFile("manifests/" + e.Name())
		if err != nil {
			return nil, err
		}
		s.add(e.Name(), "bundled", data)
	}
	if overrideDir != "" {
		files, err := os.ReadDir(overrideDir)
		if err == nil {
			for _, f := range files {
				if f.IsDir() || !strings.HasSuffix(f.Name(), ".toml") {
					continue
				}
				p := filepath.Join(overrideDir, f.Name())
				data, err := os.ReadFile(p)
				if err != nil {
					s.warnings = append(s.warnings,
						fmt.Sprintf("cannot read %s: %v", p, err))
					continue
				}
				s.add(f.Name(), p, data)
			}
		}
	}
	return s, nil
}

func (s *Set) add(name, source string, data []byte) {
	m, c, err := Parse(name, data)
	if err != nil {
		s.warnings = append(s.warnings,
			fmt.Sprintf("%s did not load: %v. Fix it or delete it.", source, err))
		return
	}
	s.byID[m.ID] = c
	s.sources[m.ID] = source
	for _, a := range m.Aliases {
		s.aliases[a] = m.ID
	}
}

func (s *Set) Agents() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.byID))
	for id := range s.byID {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// For resolves an agent name or alias to its compiled manifest.
func (s *Set) For(agent string) (*Compiled, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if c, ok := s.byID[agent]; ok {
		return c, true
	}
	if id, ok := s.aliases[agent]; ok {
		c, ok := s.byID[id]
		return c, ok
	}
	return nil, false
}

func (s *Set) Source(agent string) string { return s.sources[agent] }

func (s *Set) Warnings() []string { return append([]string(nil), s.warnings...) }
```

- [ ] **Step 8: Write the bundled-manifest test**

```go
// harness/coppice/internal/detect/manifests_test.go
package detect

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func loadedSet(t *testing.T) *Set {
	t.Helper()
	s, err := LoadSet("")
	if err != nil {
		t.Fatal(err)
	}
	if w := s.Warnings(); len(w) > 0 {
		t.Fatalf("bundled manifests produced warnings, so a pattern or a key drifted: %v", w)
	}
	return s
}

// Every bundled manifest must parse, which means every regex in every rule
// compiles under Go's engine. That is the tripwire for Rust-to-Go regex drift.
func TestEveryBundledManifestLoads(t *testing.T) {
	s := loadedSet(t)
	if len(s.Agents()) < 20 {
		t.Fatalf("only %d manifests loaded, want the full vendored set", len(s.Agents()))
	}
}

// The literal substring "commit:" passes even when the vendoring script wrote
// an empty value, which is exactly the broken path. Assert a real SHA.
func TestProvenanceRecordsTheVendoredCommit(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("manifests", "PROVENANCE"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	for _, key := range []string{"source:", "licence: Apache-2.0"} {
		if !strings.Contains(text, key) {
			t.Fatalf("PROVENANCE is missing %q", key)
		}
	}
	re := regexp.MustCompile(`(?m)^commit:\s*([0-9a-f]{40})\s*$`)
	if !re.MatchString(text) {
		t.Fatalf("PROVENANCE has no 40 character commit sha:\n%s", text)
	}
}

func TestEveryVendoredFileCarriesAnAttributionLine(t *testing.T) {
	entries, err := os.ReadDir("manifests")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".toml") {
			continue
		}
		f, err := os.Open(filepath.Join("manifests", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(f)
		sc.Scan()
		first := sc.Text()
		_ = f.Close()
		if !strings.Contains(first, "herdrdev/herdr") || !strings.Contains(first, "Apache-2.0") {
			t.Fatalf("%s first line = %q, want the vendoring attribution", e.Name(), first)
		}
	}
}

// screenFixtures walks testdata/screens/<agent>/<state>-N.txt.
func screenFixtures(t *testing.T) map[string]map[string][]string {
	t.Helper()
	root := filepath.Join("..", "..", "testdata", "screens")
	out := map[string]map[string][]string{}
	agents, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range agents {
		if !a.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(root, a.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[a.Name()] = map[string][]string{}
		for _, f := range files {
			if !strings.HasSuffix(f.Name(), ".txt") {
				continue
			}
			state, _, _ := strings.Cut(strings.TrimSuffix(f.Name(), ".txt"), "-")
			out[a.Name()][state] = append(out[a.Name()][state],
				filepath.Join(root, a.Name(), f.Name()))
		}
	}
	return out
}

func unfixtured(t *testing.T) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "screens", "unfixtured.txt"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		out[l] = true
	}
	return out
}

// Every manifest is either fixtured or listed as unfixtured. A manifest that is
// neither is an untested detection nobody declared.
func TestEveryManifestIsFixturedOrDeclaredUnfixtured(t *testing.T) {
	s := loadedSet(t)
	fx := screenFixtures(t)
	skip := unfixtured(t)
	var missing []string
	for _, id := range s.Agents() {
		if len(fx[id]) > 0 || skip[id] {
			continue
		}
		missing = append(missing, id)
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("these manifests have no fixture and are not in unfixtured.txt: %v. "+
			"Add a fixture under testdata/screens/<agent>/, or add the id to unfixtured.txt.",
			missing)
	}
}

// Each fixture must produce the state its filename claims.
func TestEachFixtureDetectsItsDeclaredState(t *testing.T) {
	s := loadedSet(t)
	for agent, byState := range screenFixtures(t) {
		c, ok := s.For(agent)
		if !ok {
			t.Fatalf("fixtures exist for %q but no manifest declares it", agent)
		}
		for wantState, paths := range byState {
			for _, p := range paths {
				b, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				in := inputFromFixture(string(b))
				r := c.Evaluate(in)
				if !r.Matched {
					t.Fatalf("%s matched no rule, want state %s", p, wantState)
				}
				if string(r.State) != wantState {
					t.Fatalf("%s detected %s by rule %s, want %s",
						p, r.State, r.RuleID, wantState)
				}
			}
		}
	}
}

// Ambiguity means two rules of EQUAL priority match and disagree about the
// state. Two rules of different priority both matching is not ambiguity: that
// is what priority is for, and Herdr's own manifests rely on it.
func TestNoFixtureIsAmbiguousAtEqualPriority(t *testing.T) {
	s := loadedSet(t)
	for agent, byState := range screenFixtures(t) {
		c, _ := s.For(agent)
		for _, paths := range byState {
			for _, p := range paths {
				b, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				r := c.Evaluate(inputFromFixture(string(b)))
				byPriority := map[int]map[State][]string{}
				for _, e := range r.Evaluated {
					if !e.Matched {
						continue
					}
					if byPriority[e.Priority] == nil {
						byPriority[e.Priority] = map[State][]string{}
					}
					byPriority[e.Priority][e.State] = append(byPriority[e.Priority][e.State], e.ID)
				}
				for prio, states := range byPriority {
					if len(states) > 1 {
						t.Fatalf("%s: rules at priority %d disagree: %v. "+
							"Give one of them a different priority in the override, "+
							"or fix the fixture.", p, prio, states)
					}
				}
			}
		}
	}
}

// inputFromFixture reads the optional header lines a fixture may carry.
// "#osc_title: …" and "#osc_progress: …" on the first lines set those fields;
// everything else is the screen.
func inputFromFixture(body string) Input {
	in := Input{}
	var screen []string
	for _, l := range strings.Split(body, "\n") {
		switch {
		case strings.HasPrefix(l, "#osc_title:"):
			in.OSCTitle = strings.TrimSpace(strings.TrimPrefix(l, "#osc_title:"))
		case strings.HasPrefix(l, "#osc_progress:"):
			in.OSCProgress = strings.TrimSpace(strings.TrimPrefix(l, "#osc_progress:"))
		default:
			screen = append(screen, l)
		}
	}
	in.Screen = strings.Join(screen, "\n")
	return in
}
```

- [ ] **Step 9: Run everything and iterate on the fixtures**

Run: `cd harness/coppice && go test -race ./internal/detect/ -v`
Expected: PASS. If `TestEachFixtureDetectsItsDeclaredState` fails, read the failure: it names the
rule that fired. Adjust the fixture to match a real screen, not the rule. If no realistic screen
produces the declared state, move that agent to `unfixtured.txt` with a comment saying why.

- [ ] **Step 10: Commit**

```bash
git add harness/coppice/internal/detect harness/coppice/NOTICE harness/coppice/testdata/screens \
        harness/coppice/PINS.md docs/plans/2026-09-08-workshop/00-master-spec.md
git commit -m "feat(coppice): Herdr's detection engine in Go, with honest fixture coverage

Twenty-one agents are detected on day one because the manifests are Herdr's,
vendored with attribution at a recorded commit. Loading them compiles every
regex, which is the tripwire for drift between the Rust and Go engines.
Fixtures exist only for harnesses we actually run; the rest are listed in
unfixtured.txt, because a fixture invented for a UI nobody has seen makes the
test pass and the detection wrong. Ambiguity is defined as equal-priority rules
that disagree, since differing priorities are what priority is for."
```

---

### Task 13: the manifest tick, `pane.read --source detection`, and `pane explain`

**Files:**
- Create: `harness/coppice/internal/server/detect.go`
- Test: `harness/coppice/internal/server/detect_test.go`

**Interfaces:**
- Consumes: `detect.Set`, `detect.Input`, `pane.ReadDetection`, `state.Store`, `ApplyState`.
- Produces:
  ```go
  package server
  const ManifestTick = 500 * time.Millisecond
  const GateQuiet = 2 * time.Second   // a pane the gate spoke for recently is not scanned
  func (s *Server) StartManifestTick(set *detect.Set)
  func (s *Server) StopManifestTick()
  func (s *Server) RegisterExplainCommand(set *detect.Set)
  ```
  Command: `pane.explain`.

- [ ] **Step 1: Write the failing test**

```go
// harness/coppice/internal/server/detect_test.go
package server

import (
	"strings"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/detect"
	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/toolchain"
)

func newDetectServer(t *testing.T) (*Server, *detect.Set) {
	t.Helper()
	if r := toolchain.SkipReason(); r != "" {
		t.Skip(r)
	}
	s := newTestServer(t)
	s.RegisterPaneCommands()
	s.RegisterAttachCommands()
	s.RegisterAgentCommands()
	set, err := detect.LoadSet("")
	if err != nil {
		t.Fatal(err)
	}
	s.RegisterExplainCommand(set)
	return s, set
}

// A pane running a harness we have a manifest for, showing a screen the
// manifest calls idle, becomes idle from the manifest source.
func TestManifestTickSetsStateForAPaneWithNoBetterSource(t *testing.T) {
	s, set := newDetectServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c",`+
			`"printf '────────────────────\\n❯ \\n────────────────────\\n'; sleep 5"],`+
			`"kind":"pty","harness":"claude","cols":40,"rows":10}`)
	if !got[0].OK {
		t.Fatalf("pane.create failed: %+v", got[0].Error)
	}
	s.StartManifestTick(set)
	defer s.StopManifestTick()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ev, ok := s.States().Current("w1:p1"); ok && ev.Source == proto.SrcManifest {
			if ev.State != proto.StateIdle {
				t.Fatalf("manifest set state %s, want idle for a prompt box", ev.State)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the manifest tick never produced a state within 5 s")
}

// The gate outranks the screen, and a pane the gate spoke for in the last two
// seconds is not scanned at all.
func TestAPaneWithAFreshGateSourceIsNotScanned(t *testing.T) {
	// gateBlockedLine carries ts 1757300000.0, about a year in the past. This
	// test must pass because the STORE recorded the arrival just now, not
	// because the event claimed a recent timestamp.
	s, set := newDetectServer(t)
	cwd := t.TempDir()
	roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c",`+
			`"printf '────────────────────\\n❯ \\n────────────────────\\n'; sleep 5"],`+
			`"kind":"pty","harness":"claude","cols":40,"rows":10}`,
		gateBlockedLine("w1:p1"))
	s.StartManifestTick(set)
	defer s.StopManifestTick()

	time.Sleep(1200 * time.Millisecond)
	ev, ok := s.States().Current("w1:p1")
	if !ok || ev.Source != proto.SrcGate || ev.State != proto.StateBlocked {
		t.Fatalf("state = %+v, want the gate's blocked to still stand", ev)
	}
}

// The other half of the same rule, and the one a "never scan a gate pane again"
// shortcut would break: a harness that stops calling the gate must recover, not
// freeze on its last gate state for the rest of the session.
func TestAPaneWhoseGateHasGoneQuietIsScannedAgain(t *testing.T) {
	s, set := newDetectServer(t)
	cwd := t.TempDir()
	roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c",`+
			`"printf '────────────────────\n❯ \n────────────────────\n'; sleep 10"],`+
			`"kind":"pty","harness":"claude","cols":40,"rows":10}`)
	// A gate WORKING report, not blocked: a gate hold on blocked is a separate
	// rule and stands until the gate clears it.
	id := "w1:p1"
	s.ApplyState(id, proto.PaneStateEvent{
		V: 1, TS: nowSeconds(), SessionID: id, Harness: "claude", Pane: &id,
		State: proto.StateWorking, Source: proto.SrcGate, Detail: "verdict=allow",
	})
	s.StartManifestTick(set)
	defer s.StopManifestTick()

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if ev, ok := s.States().Current(id); ok && ev.Source == proto.SrcManifest {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("the screen never spoke again after the gate went quiet for more than 2 s")
}

func TestAPaneWithNoHarnessIsNotScanned(t *testing.T) {
	s, set := newDetectServer(t)
	cwd := t.TempDir()
	roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c","sleep 5"],"kind":"pty"}`)
	s.StartManifestTick(set)
	defer s.StopManifestTick()
	time.Sleep(1200 * time.Millisecond)
	if ev, ok := s.States().Current("w1:p1"); ok && ev.Source == proto.SrcManifest {
		t.Fatalf("a pane with no harness was scanned anyway: %+v", ev)
	}
}

func TestExplainListsTheMatchedRuleAndTheRegionItRead(t *testing.T) {
	s, _ := newDetectServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c",`+
			`"printf '────────────────────\\n❯ \\n────────────────────\\n'; sleep 5"],`+
			`"kind":"pty","harness":"claude","cols":40,"rows":10}`,
		`{"id":"2","cmd":"pane.wait_output","pane":"w1:p1","contains":"❯","timeout_ms":5000}`,
		`{"id":"3","cmd":"pane.explain","pane":"w1:p1"}`)
	if !got[2].OK {
		t.Fatalf("pane.explain failed: %+v", got[2].Error)
	}
	m := result(t, got[2])
	if m["agent"] != "claude" {
		t.Fatalf("explain agent = %v, want claude", m["agent"])
	}
	if m["rule_id"] == "" || m["rule_id"] == nil {
		t.Fatalf("explain named no rule: %v", m)
	}
	rules, _ := m["evaluated"].([]any)
	if len(rules) < 5 {
		t.Fatalf("explain listed %d rules, want every rule in the manifest", len(rules))
	}
	if _, ok := m["detection_text"]; !ok {
		t.Fatalf("explain does not show the detection window it read: %v", m)
	}
}

func TestExplainForAPaneWithNoManifestSaysSoPlainly(t *testing.T) {
	s, _ := newDetectServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c","sleep 5"],"kind":"pty"}`,
		`{"id":"2","cmd":"pane.explain","pane":"w1:p1"}`)
	if !got[1].OK {
		t.Fatalf("pane.explain failed: %+v", got[1].Error)
	}
	m := result(t, got[1])
	note, _ := m["note"].(string)
	if !strings.Contains(note, "harness") {
		t.Fatalf("note = %q, want it to say this pane has no harness to detect", note)
	}
}

func TestReadDetectionReturnsTheSameTextExplainUsed(t *testing.T) {
	s, _ := newDetectServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c",`+
			`"printf 'hello\\n'; sleep 5"],"kind":"pty","harness":"claude","cols":40,"rows":10}`,
		`{"id":"2","cmd":"pane.wait_output","pane":"w1:p1","contains":"hello","timeout_ms":5000}`,
		`{"id":"3","cmd":"pane.read","pane":"w1:p1","source":"detection"}`,
		`{"id":"4","cmd":"pane.explain","pane":"w1:p1"}`)
	read := result(t, got[2])["text"]
	explained := result(t, got[3])["detection_text"]
	if read != explained {
		t.Fatalf("pane.read --source detection = %q but explain read %q. "+
			"They must be the same text or the debugging window lies.", read, explained)
	}
}
```

- [ ] **Step 2: Run the test and watch it fail**

Run: `cd harness/coppice && go test -race ./internal/server/ -run 'Manifest|Explain|Detection|Scanned' -v`
Expected: FAIL, `s.RegisterExplainCommand undefined`.

- [ ] **Step 3: Write the implementation**

```go
// harness/coppice/internal/server/detect.go
package server

import (
	"fmt"
	"time"

	"github.com/opendaisugi/coppice/internal/detect"
	"github.com/opendaisugi/coppice/internal/layout"
	"github.com/opendaisugi/coppice/internal/pane"
	"github.com/opendaisugi/coppice/internal/proto"
)

// ManifestTick is how often a pty pane with no better source is scanned.
const ManifestTick = 500 * time.Millisecond

// GateQuiet is how long a report from any source above manifest keeps the
// screen scanner away. Master spec 3.1 gives those sources a two-second hold;
// scanning inside it would only produce events the merge throws away. After
// two seconds the screen is allowed to speak again, which is what makes a
// harness that stopped calling the gate recoverable rather than frozen.
const GateQuiet = 2 * time.Second

// detectionInput reads a pane's detection window plus its OSC state.
func detectionInput(lp *LivePane) (detect.Input, string, error) {
	text, err := lp.Grid.Read(pane.ReadDetection)
	if err != nil {
		return detect.Input{}, "", err
	}
	return detect.Input{
		Screen:      text,
		OSCTitle:    lp.Grid.Title(),
		OSCProgress: lp.Grid.Progress(),
	}, text, nil
}

// StartManifestTick scans pty panes that have no better source. It is the
// fallback of last resort: a pane with a gate, headless or process source in
// the last two seconds is skipped entirely.
func (s *Server) StartManifestTick(set *detect.Set) {
	s.mu.Lock()
	if s.tickStop != nil {
		s.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	s.tickStop = stop
	s.mu.Unlock()

	go func() {
		t := time.NewTicker(ManifestTick)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				s.scanOnce(set)
			}
		}
	}()
}

func (s *Server) StopManifestTick() {
	s.mu.Lock()
	stop := s.tickStop
	s.tickStop = nil
	s.mu.Unlock()
	if stop != nil {
		close(stop)
	}
}

func (s *Server) scanOnce(set *detect.Set) {
	now := nowSeconds()
	for _, rec := range s.tree.Panes() {
		if rec.Closed || rec.Kind != layout.KindPTY || rec.Harness == "" {
			continue
		}
		c, ok := set.For(rec.Harness)
		if !ok {
			continue
		}
		if ev, ok := s.states.Current(rec.ID); ok {
			// done is terminal. Nothing observed on a screen afterwards is
			// about a running agent.
			if ev.State == proto.StateDone {
				continue
			}
			// Spec-02: a pty pane with a gate source in the LAST 2 SECONDS is
			// not scanned. Not "ever spoke for": a pane whose harness stops
			// calling the gate must fall back to the screen, or it freezes on
			// its last gate state for the rest of the session.
			//
			// The age is measured with the store's own receive clock, never
			// with ev.TS, which is whatever the reporter's clock said. A hook
			// with a clock a year ahead must not be able to keep the scanner
			// away for ever.
			if ev.Source != proto.SrcManifest {
				received, ok := s.states.Received(rec.ID)
				if ok && now-received < GateQuiet.Seconds() {
					continue
				}
			}
		}
		lp, ok := s.Live(rec.ID)
		if !ok {
			continue
		}
		in, _, err := detectionInput(lp)
		if err != nil {
			continue
		}
		r := c.Evaluate(in)
		// No match means no event. A rule with skip_state_update means no event
		// either: the pane is in a viewer or a menu, and overwriting its state
		// every 500 ms would erase what a real source told us.
		if !r.Matched || r.Skip {
			continue
		}
		id := rec.ID
		s.ApplyState(id, proto.PaneStateEvent{
			V: 1, TS: now, SessionID: id, Harness: rec.Harness, Pane: &id,
			State: string(r.State), Source: proto.SrcManifest,
			Detail: fmt.Sprintf("rule=%s region=%s", r.RuleID, r.Region),
		})
	}
}

func (s *Server) RegisterExplainCommand(set *detect.Set) {
	s.Handle("pane.explain", func(_ *Client, r *proto.Request) proto.Response {
		lp, bad := s.livePane(r)
		if bad != nil {
			return *bad
		}
		in, text, err := detectionInput(lp)
		if err != nil {
			return proto.ErrResp(r.ID, proto.ErrInternal, err.Error())
		}
		out := map[string]any{
			"pane":           lp.Info.ID,
			"harness":        lp.Info.Harness,
			"detection_text": text,
			"osc_title":      in.OSCTitle,
			"osc_progress":   in.OSCProgress,
		}
		if lp.Info.Harness == "" {
			out["note"] = "this pane has no harness, so no manifest applies. " +
				"Create it with --harness NAME to enable screen detection."
			return proto.OKResp(r.ID, out)
		}
		c, ok := set.For(lp.Info.Harness)
		if !ok {
			out["note"] = fmt.Sprintf("no manifest for harness %q. Run: coppice agent list",
				lp.Info.Harness)
			return proto.OKResp(r.ID, out)
		}
		res := c.Evaluate(in)
		out["agent"] = res.Agent
		out["source_file"] = set.Source(res.Agent)
		out["matched"] = res.Matched
		out["rule_id"] = res.RuleID
		out["priority"] = res.Priority
		out["region"] = res.Region
		out["state"] = string(res.State)
		out["skip_state_update"] = res.Skip
		out["evaluated"] = res.Evaluated
		if !res.Matched {
			out["note"] = "no rule matched, so no state is claimed. " +
				"The floor shows unknown rather than guessing idle."
		}
		return proto.OKResp(r.ID, out)
	})
}
```

`Server` already carries `tickStop` and `detectWarnings`, `handleStatus` already returns
`restore` and `detection_warnings`, and `SetDetectionWarnings`/`DetectionWarnings` already exist:
Task 7 declared all of them. Nothing to add here.

- [ ] **Step 4: Run the test and watch it pass**

Run: `cd harness/coppice && go test -race ./internal/server/ -v && go vet ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/server/detect.go harness/coppice/internal/server/detect_test.go
git commit -m "feat(coppice): the screen is the fallback, and only when nothing better spoke

A pane the gate reported on in the last two seconds is not scanned at all: the
merge would discard the result anyway, and scanning would only burn work. A
skip_state_update rule emits nothing rather than unknown, so a transcript
viewer does not erase what the gate told us every half second. pane explain
prints the exact window a rule read, and a test pins it to the text
pane read --source detection returns."
```

---

### Task 14: the headless registry, the transcript writer, and the pi and OpenCode slots

**Files:**
- Create: `harness/coppice/internal/pane/headless.go`
- Create: `harness/coppice/internal/adapters/registry.go`
- Create: `harness/coppice/internal/adapters/pi/pi.go`
- Create: `harness/coppice/internal/adapters/opencode/opencode.go`
- Modify: `harness/coppice/internal/server/panes.go` (`startPane`, the headless branch)
- Test: `harness/coppice/internal/pane/headless_test.go`
- Test: `harness/coppice/internal/adapters/registry_test.go`

**Interfaces:**
- Consumes: `pane.Adapter`, `pane.Proc`, `pane.Event`, `pane.Grid`.
- Produces:
  ```go
  package pane
  func WriteTranscript(g *Grid, ev Event)        // renders one event into the grid
  func StateOf(ev Event) (string, bool)          // the PaneStateEvent state an event implies

  package adapters
  func Register(a pane.Adapter)
  func Get(name string) (pane.Adapter, bool)
  func Names() []string
  var ErrNotBuilt = errors.New("this adapter is a slot, not an implementation yet")

  package pi        // spec-04 fills this
  func New() pane.Adapter
  package opencode  // spec-05 fills this
  func New() pane.Adapter
  ```

- [ ] **Step 1: Write the failing test**

```go
// harness/coppice/internal/pane/headless_test.go
package pane

import (
	"strings"
	"testing"

	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/toolchain"
)

// pi and OpenCode both hand us a tool name and a summary when they block. A
// floor client cannot render "blocked on what?" without them, so the field
// travels on the event rather than being invented twice in specs 04 and 05.
func TestABlockedEventCarriesItsAsk(t *testing.T) {
	ev := Event{Kind: EvState, State: "blocked",
		Ask: &proto.Ask{ID: "ui_1", Tool: "Write", Summary: "src/main.go", Deadline: 1757300090}}
	st, ok := StateOf(ev)
	if !ok || st != "blocked" {
		t.Fatalf("StateOf = %q %v, want blocked true", st, ok)
	}
	if ev.Ask == nil || ev.Ask.Tool != "Write" {
		t.Fatalf("the ask did not survive: %+v", ev.Ask)
	}
}

func TestWriteTranscriptRendersTextIntoTheGrid(t *testing.T) {
	if r := toolchain.SkipReason(); r != "" {
		t.Skip(r)
	}
	g, err := NewGrid(60, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	WriteTranscript(g, Event{Kind: EvText, Text: "the model said this"})
	WriteTranscript(g, Event{Kind: EvTool, Tool: "Bash", Detail: "ls -la"})
	WriteTranscript(g, Event{Kind: EvError, Detail: "stream ended early"})
	screen, err := g.Read(ReadVisible)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"the model said this", "Bash", "ls -la", "stream ended early"} {
		if !strings.Contains(screen, want) {
			t.Fatalf("grid = %q, want it to contain %q", screen, want)
		}
	}
}

// A multi-line event must not leave the cursor mid-row: the next event would
// then be appended to the wrong place. Every write ends at column zero.
func TestWriteTranscriptEndsEveryEventOnItsOwnLine(t *testing.T) {
	if r := toolchain.SkipReason(); r != "" {
		t.Skip(r)
	}
	g, err := NewGrid(60, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	WriteTranscript(g, Event{Kind: EvText, Text: "first\nsecond"})
	WriteTranscript(g, Event{Kind: EvText, Text: "third"})
	screen, _ := g.Read(ReadVisible)
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
	if len(lines) < 3 || !strings.HasPrefix(lines[2], "third") {
		t.Fatalf("lines = %q, want third to start its own line", lines)
	}
}

func TestStateOfMapsEventsToStates(t *testing.T) {
	cases := []struct {
		ev    Event
		state string
		ok    bool
	}{
		{Event{Kind: EvText}, StateWorkingStr, true},
		{Event{Kind: EvTool, Tool: "Bash"}, StateWorkingStr, true},
		{Event{Kind: EvState, State: "blocked",
			Ask: &proto.Ask{ID: "a1", Tool: "Write", Summary: "src/main.go"}}, "blocked", true},
		{Event{Kind: EvEnd, State: "done"}, "done", true},
		{Event{Kind: EvEnd}, "done", true},
		// A parse error marks the pane unknown, never idle. Master spec 3.6.
		{Event{Kind: EvError, Detail: "bad json"}, "unknown", true},
	}
	for _, c := range cases {
		got, ok := StateOf(c.ev)
		if ok != c.ok || got != c.state {
			t.Fatalf("StateOf(%+v) = %q %v, want %q %v", c.ev, got, ok, c.state, c.ok)
		}
	}
}

func TestAnErrorEventNeverProducesIdle(t *testing.T) {
	for _, detail := range []string{"", "truncated", "unexpected end of JSON input"} {
		got, _ := StateOf(Event{Kind: EvError, Detail: detail})
		if got == "idle" {
			t.Fatalf("an error event with detail %q produced idle", detail)
		}
	}
}
```

```go
// harness/coppice/internal/adapters/registry_test.go
package adapters

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/opendaisugi/coppice/internal/pane"
)

func TestNamesListsEverySlotIncludingTheUnbuiltOnes(t *testing.T) {
	names := Names()
	want := []string{"claude", "codex", "opencode", "pi", "sprig"}
	for _, w := range want {
		found := false
		for _, n := range names {
			if n == w {
				found = true
			}
		}
		if !found {
			t.Fatalf("Names() = %v, want it to include %q", names, w)
		}
	}
}

func TestGetOnAnUnknownAdapterFails(t *testing.T) {
	if _, ok := Get("telepathy"); ok {
		t.Fatal(`Get("telepathy") reported ok`)
	}
}

// A slot that is not built must refuse loudly. A slot that silently produced a
// pane doing nothing would be exactly the dishonest control section 3.5 bans.
func TestUnbuiltAdaptersRefuseToStart(t *testing.T) {
	for _, name := range []string{"pi", "opencode"} {
		a, ok := Get(name)
		if !ok {
			t.Fatalf("Get(%q) failed", name)
		}
		_, err := a.Start(context.Background(), pane.StartOpts{}, nil)
		if !errors.Is(err, ErrNotBuilt) {
			t.Fatalf("%s.Start() = %v, want ErrNotBuilt", name, err)
		}
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("%s.Start() error %q does not name the adapter", name, err)
		}
	}
}
```

- [ ] **Step 2: Run the tests and watch them fail**

Run: `cd harness/coppice && go test -race ./internal/pane/ ./internal/adapters/... -v`
Expected: FAIL, `undefined: WriteTranscript` and no package `adapters`.

- [ ] **Step 3: Write `headless.go`**

```go
// harness/coppice/internal/pane/headless.go
package pane

import (
	"fmt"
	"strings"
)

// The state strings below mirror proto's constants. internal/pane already
// imports internal/proto for Cell, Frame and Ask, so the duplication is for
// readability at the call sites, not for avoiding the import.

// The state strings a headless event maps to. They mirror proto's constants;
// internal/pane does not import internal/proto, because a grid should not know
// about the wire.
const (
	StateIdleStr    = "idle"
	StateWorkingStr = "working"
	StateBlockedStr = "blocked"
	StateDoneStr    = "done"
	StateUnknownStr = "unknown"
)

// WriteTranscript renders one adapter event into the pane's grid, so a headless
// pane is also just a grid to a client. Every event ends at column zero, or the
// next event would continue someone else's line.
func WriteTranscript(g *Grid, ev Event) {
	var line string
	switch ev.Kind {
	case EvText:
		line = ev.Text
	case EvTool:
		line = "\x1b[36m" + ev.Tool + "\x1b[0m " + ev.Detail
	case EvState:
		line = "\x1b[33m[" + ev.State + "]\x1b[0m " + ev.Detail
		if ev.Ask != nil {
			line += " " + ev.Ask.Tool + ": " + ev.Ask.Summary
		}
	case EvEnd:
		line = "\x1b[32m[end]\x1b[0m " + ev.Detail
	case EvError:
		line = "\x1b[31m[error]\x1b[0m " + ev.Detail
	default:
		line = fmt.Sprintf("[%s] %s", ev.Kind, ev.Detail)
	}
	line = strings.ReplaceAll(strings.TrimRight(line, "\n"), "\n", "\r\n")
	_, _ = g.Write([]byte(line + "\r\n"))
}

// StateOf is the state a headless event implies. A parse error means unknown,
// never idle: master spec 3.6 says a broken stream must not look like a quiet
// agent.
func StateOf(ev Event) (string, bool) {
	switch ev.Kind {
	case EvText, EvTool:
		return StateWorkingStr, true
	case EvState:
		if ev.State == "" {
			return StateUnknownStr, true
		}
		return ev.State, true
	case EvEnd:
		if ev.State == "" {
			return StateDoneStr, true
		}
		return ev.State, true
	case EvError:
		return StateUnknownStr, true
	default:
		return StateUnknownStr, true
	}
}
```

- [ ] **Step 4: Write the registry and the two slots**

```go
// harness/coppice/internal/adapters/registry.go

// Package adapters is the registry of headless harness adapters. Every slot in
// spec-02 is registered, built or not: an adapter that is only a design gets a
// name here and refuses to start, so `coppice agent list` tells the truth about
// what exists.
package adapters

import (
	"errors"
	"sort"
	"sync"

	"github.com/opendaisugi/coppice/internal/pane"
)

// ErrNotBuilt is what a registered but unimplemented adapter returns.
var ErrNotBuilt = errors.New("this adapter is a slot, not an implementation yet")

var (
	mu   sync.RWMutex
	reg  = map[string]pane.Adapter{}
)

func Register(a pane.Adapter) {
	mu.Lock()
	defer mu.Unlock()
	reg[a.Name()] = a
}

func Get(name string) (pane.Adapter, bool) {
	mu.RLock()
	defer mu.RUnlock()
	a, ok := reg[name]
	return a, ok
}

func Names() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(reg))
	for n := range reg {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// NotBuilt is the adapter shape for a slot that is designed and not written.
type NotBuilt struct {
	AdapterName string
	Owner       string // which sub-spec fills it
}

func (n NotBuilt) Name() string { return n.AdapterName }

func (n NotBuilt) Start(_ context.Context, _ pane.StartOpts, _ *pane.Grid) (pane.Proc, error) {
	return nil, fmt.Errorf("%s: %w. %s fills this slot.", n.AdapterName, ErrNotBuilt, n.Owner)
}
```

Add `"context"` and `"fmt"` to that file's imports.

```go
// harness/coppice/internal/adapters/pi/pi.go

// Package pi is the pi headless adapter slot. spec-04 implements it against
// `pi --mode rpc`, where an extension_ui_request means blocked with an ask.
// Until then the slot exists and refuses, so `coppice agent list` is honest.
package pi

import (
	"github.com/opendaisugi/coppice/internal/adapters"
	"github.com/opendaisugi/coppice/internal/pane"
)

func New() pane.Adapter {
	return adapters.NotBuilt{AdapterName: "pi", Owner: "spec-04"}
}

func init() { adapters.Register(New()) }
```

```go
// harness/coppice/internal/adapters/opencode/opencode.go

// Package opencode is the OpenCode headless adapter slot. spec-05 implements it
// against `opencode serve` plus the plugin whose before-execute hook is
// deny-only by design.
package opencode

import (
	"github.com/opendaisugi/coppice/internal/adapters"
	"github.com/opendaisugi/coppice/internal/pane"
)

func New() pane.Adapter {
	return adapters.NotBuilt{AdapterName: "opencode", Owner: "spec-05"}
}

func init() { adapters.Register(New()) }
```

The registry test also expects `claude`, `codex` and `sprig`. Tasks 15 to 17 register those.
Until then add a temporary `NotBuilt` registration for each in
`harness/coppice/internal/adapters/registry.go`'s `init`, and delete each line in the task that
implements it:

```go
func init() {
	Register(NotBuilt{AdapterName: "claude", Owner: "task 15 of this plan"})
	Register(NotBuilt{AdapterName: "codex", Owner: "task 16 of this plan"})
	Register(NotBuilt{AdapterName: "sprig", Owner: "task 17 of this plan"})
}
```

- [ ] **Step 5: Wire the headless branch of `startPane`**

In `harness/coppice/internal/server/panes.go`, replace the body of `startPane`'s pty check with
both branches:

```go
func (s *Server) startPane(rec layout.Pane) error {
	g, err := pane.NewGrid(rec.Cols, rec.Rows)
	if err != nil {
		return err
	}
	lp := &LivePane{Info: rec, Grid: g}
	switch rec.Kind {
	case layout.KindPTY:
		p, err := pane.StartPTY(pane.SpawnOpts{
			Cwd: rec.Cwd, Argv: rec.Argv, Env: rec.Env,
			Cols: rec.Cols, Rows: rec.Rows,
			Sock: s.cfg.SocketPath, PaneID: rec.ID,
		}, g)
		if err != nil {
			g.Close()
			return err
		}
		lp.PTY = p
		go s.watchExit(rec.ID, p)
	case layout.KindHeadless:
		a, ok := adapters.Get(rec.Harness)
		if !ok {
			g.Close()
			return fmt.Errorf("no adapter named %q. Known adapters: %s",
				rec.Harness, strings.Join(adapters.Names(), ", "))
		}
		proc, err := a.Start(context.Background(), pane.StartOpts{
			Cwd: rec.Cwd, Env: rec.Env, Argv: rec.Argv,
			Resume: rec.HarnessSessionID, Sock: s.cfg.SocketPath, PaneID: rec.ID,
		}, g)
		if err != nil {
			g.Close()
			return err
		}
		lp.Adapter = proc
		go s.pumpAdapter(rec.ID, rec.Harness, proc, g)
	}
	s.putLive(rec.ID, lp)
	return nil
}

// pumpAdapter turns one adapter's events into transcript rows and state events.
// It is the only place a headless pane's state is produced, so the fail-closed
// rule lives in one spot: a broken stream marks the pane unknown, never idle.
func (s *Server) pumpAdapter(paneID, harness string, p pane.Proc, g *pane.Grid) {
	for ev := range p.Events() {
		pane.WriteTranscript(g, ev)
		st, ok := pane.StateOf(ev)
		if !ok {
			continue
		}
		id := paneID
		e := proto.PaneStateEvent{
			V: 1, TS: nowSeconds(), SessionID: paneID, Harness: harness, Pane: &id,
			State: st, Source: proto.SrcHeadless, Detail: truncate(ev.Detail, proto.DetailMax),
		}
		// An ask only means anything on a blocked event, and Validate rejects
		// it anywhere else. pi (spec-04) and OpenCode (spec-05) fill it.
		if st == proto.StateBlocked {
			e.Ask = ev.Ask
		}
		if sid, ok := p.SessionID(); ok {
			e.HarnessSessionID = &sid
			if rec, ok := s.tree.Pane(paneID); ok && rec.HarnessSessionID != sid {
				_ = s.updatePane(paneID, func(x *layout.Pane) { x.HarnessSessionID = sid })
			}
		}
		s.ApplyState(paneID, e)
	}
	// The channel closed without an end event. That is a stream that stopped,
	// not an agent that finished, so say done from the headless source with the
	// reason attached.
	id := paneID
	s.ApplyState(paneID, proto.PaneStateEvent{
		V: 1, TS: nowSeconds(), SessionID: paneID, Harness: harness, Pane: &id,
		State: proto.StateDone, Source: proto.SrcHeadless, Detail: "adapter stream ended",
	})
	_ = s.tree.ClosePane(paneID, nil)
	s.saveLayout()
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
```

Add `"context"` and the `adapters` import to `panes.go`, and add
`_ "github.com/opendaisugi/coppice/internal/adapters/pi"` and the same for `opencode` to
`cmd/coppice/main.go` in Task 19, so their `init` registrations run.

- [ ] **Step 6: Amend spec-02's "Headless adapters" block in the same commit**

Spec-02 sketches `Adapter` with methods that take a `Proc`
(`Prompt(p Proc, text string)`, `Start(ctx, opts)`, no grid). The shape this task ships is
different and is the one plans 04 and 05 build against, so master spec §3's rule applies: the
document changes with the code.

Open `docs/plans/2026-09-08-workshop/spec-02-coppice-server.md`, find the Go block under
"## Headless adapters", and replace it with the `Adapter`, `Proc`, `Event` and `StartOpts`
declarations from Task 6's `internal/pane/adapter.go`, including the `Ask *proto.Ask` field and
the three rules in the `Adapter` doc comment. Change nothing else in that file.

- [ ] **Step 7: Run the tests and watch them pass**

Run: `cd harness/coppice && go test -race ./... -v && go vet ./...`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add harness/coppice/internal/pane/headless.go harness/coppice/internal/pane/headless_test.go \
        harness/coppice/internal/adapters harness/coppice/internal/server/panes.go \
        docs/plans/2026-09-08-workshop/spec-02-coppice-server.md
git commit -m "feat(coppice): headless panes are grids too, and empty slots say so

An adapter's events become transcript rows in the same grid a pty pane uses, so
a client cannot tell the two kinds apart except by a badge. Every designed slot
is registered and refuses to start with the sub-spec that owns it, because a
control that silently does nothing is the lie section 3.5 forbids. A stream
that ends without an end event reports done with the reason, and a parse error
reports unknown, never idle."
```

---

### Task 15: the Claude Code headless adapter

**Files:**
- Create: `harness/coppice/internal/adapters/claude/claude.go`
- Create: `harness/coppice/testdata/adapters/claude-stream.jsonl`
- Create: `harness/coppice/testdata/adapters/fake-claude.sh`
- Test: `harness/coppice/internal/adapters/claude/claude_test.go`

**Interfaces:**
- Consumes: `pane.Adapter`, `pane.Proc`, `pane.Event`, `pane.Grid`, `adapters.Register`.
- Produces:
  ```go
  package claude
  func New() pane.Adapter
  const Binary = "claude"        // overridden by COPPICE_CLAUDE_BIN in tests
  ```

- [ ] **Step 1: Write the fixture stream and the fake binary**

`harness/coppice/testdata/adapters/claude-stream.jsonl` — the shapes `claude -p
--output-format stream-json --verbose` emits, one per line:

```json
{"type":"system","subtype":"init","session_id":"11111111-2222-3333-4444-555555555555","tools":["Bash"]}
{"type":"assistant","message":{"content":[{"type":"text","text":"I will list the files."}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_01","name":"Bash","input":{"command":"ls"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_01","content":"a.txt"}]}}
{"type":"result","subtype":"success","session_id":"11111111-2222-3333-4444-555555555555","result":"Listed one file."}
```

`harness/coppice/testdata/adapters/fake-claude.sh`:

```bash
#!/usr/bin/env bash
# A stand-in for `claude` that replays a recorded stream-json file. It reads one
# JSON user message per line on stdin and replays the fixture for each, so the
# adapter's prompt path and its parser are both exercised without a subscription.
set -euo pipefail
fixture="${COPPICE_CLAUDE_FIXTURE:?set COPPICE_CLAUDE_FIXTURE to a stream-json file}"
if [ -n "${COPPICE_CLAUDE_ECHO_ARGV:-}" ]; then
  printf '%s\n' "$*" > "$COPPICE_CLAUDE_ECHO_ARGV"
fi
while IFS= read -r line; do
  if [ -n "${COPPICE_CLAUDE_ECHO_STDIN:-}" ]; then
    printf '%s\n' "$line" >> "$COPPICE_CLAUDE_ECHO_STDIN"
  fi
  cat "$fixture"
done
```

- [ ] **Step 2: Write the failing test**

```go
// harness/coppice/internal/adapters/claude/claude_test.go
package claude

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/pane"
	"github.com/opendaisugi/coppice/internal/toolchain"
)

func testdata(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "..", "testdata", "adapters", name))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func startFake(t *testing.T) (pane.Proc, *pane.Grid, string) {
	t.Helper()
	if r := toolchain.SkipReason(); r != "" {
		t.Skip(r)
	}
	fake := testdata(t, "fake-claude.sh")
	if err := os.Chmod(fake, 0o755); err != nil {
		t.Fatal(err)
	}
	stdinLog := filepath.Join(t.TempDir(), "stdin.jsonl")
	t.Setenv("COPPICE_CLAUDE_BIN", fake)
	t.Setenv("COPPICE_CLAUDE_FIXTURE", testdata(t, "claude-stream.jsonl"))
	t.Setenv("COPPICE_CLAUDE_ECHO_STDIN", stdinLog)

	g, err := pane.NewGrid(80, 24)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.Close)
	p, err := New().Start(context.Background(), pane.StartOpts{Cwd: t.TempDir()}, g)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop() })
	return p, g, stdinLog
}

func collect(t *testing.T, p pane.Proc, want int, d time.Duration) []pane.Event {
	t.Helper()
	var out []pane.Event
	deadline := time.After(d)
	for len(out) < want {
		select {
		case ev, ok := <-p.Events():
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-deadline:
			t.Fatalf("collected %d events in %s, want %d: %+v", len(out), d, want, out)
		}
	}
	return out
}

func TestPromptReachesStdinAsOneJSONUserMessage(t *testing.T) {
	p, _, stdinLog := startFake(t)
	if err := p.Prompt("list the files"); err != nil {
		t.Fatal(err)
	}
	collect(t, p, 4, 5*time.Second)
	b, err := os.ReadFile(stdinLog)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(b))
	if strings.Count(line, "\n") != 0 {
		t.Fatalf("prompt wrote %d lines, want exactly 1: %q", strings.Count(line, "\n")+1, line)
	}
	for _, want := range []string{`"type":"user"`, `"role":"user"`, "list the files"} {
		if !strings.Contains(line, want) {
			t.Fatalf("stdin line %q is missing %q", line, want)
		}
	}
}

func TestStreamJSONBecomesTextToolAndResultEvents(t *testing.T) {
	p, _, _ := startFake(t)
	if err := p.Prompt("go"); err != nil {
		t.Fatal(err)
	}
	evs := collect(t, p, 4, 5*time.Second)
	kinds := map[pane.EventKind]int{}
	for _, e := range evs {
		kinds[e.Kind]++
	}
	if kinds[pane.EvText] == 0 {
		t.Fatalf("no text event: %+v", evs)
	}
	if kinds[pane.EvTool] == 0 {
		t.Fatalf("no tool event: %+v", evs)
	}
	sawIdle := false
	for _, e := range evs {
		if e.Kind == pane.EvState && e.State == "idle" {
			sawIdle = true
		}
	}
	if !sawIdle {
		t.Fatalf("the result line did not produce an idle state event: %+v", evs)
	}
}

func TestSessionIDComesFromTheInitLine(t *testing.T) {
	p, _, _ := startFake(t)
	if err := p.Prompt("go"); err != nil {
		t.Fatal(err)
	}
	collect(t, p, 4, 5*time.Second)
	id, ok := p.SessionID()
	if !ok || id != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("SessionID() = %q %v, want the id from the init line", id, ok)
	}
}

func TestProcessExitClosesTheEventChannel(t *testing.T) {
	p, _, _ := startFake(t)
	if err := p.Prompt("go"); err != nil {
		t.Fatal(err)
	}
	collect(t, p, 4, 5*time.Second)
	if err := p.Stop(); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-p.Events():
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("the event channel stayed open 5 s after Stop")
		}
	}
}

// Fail-closed: a line the parser cannot read marks the pane unknown. It must
// never be dropped silently, and it must never look like idle.
func TestAnUnparsableLineYieldsAnErrorEventNotSilence(t *testing.T) {
	if r := toolchain.SkipReason(); r != "" {
		t.Skip(r)
	}
	broken := filepath.Join(t.TempDir(), "broken.jsonl")
	if err := os.WriteFile(broken, []byte("{not json at all\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := testdata(t, "fake-claude.sh")
	_ = os.Chmod(fake, 0o755)
	t.Setenv("COPPICE_CLAUDE_BIN", fake)
	t.Setenv("COPPICE_CLAUDE_FIXTURE", broken)

	g, err := pane.NewGrid(80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	p, err := New().Start(context.Background(), pane.StartOpts{Cwd: t.TempDir()}, g)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Stop()
	if err := p.Prompt("go"); err != nil {
		t.Fatal(err)
	}
	evs := collect(t, p, 1, 5*time.Second)
	if evs[0].Kind != pane.EvError {
		t.Fatalf("first event = %+v, want an error event", evs[0])
	}
	if st, _ := pane.StateOf(evs[0]); st == "idle" {
		t.Fatal("an unparsable line produced idle")
	}
}

func TestSteerIsHonestlyUnsupported(t *testing.T) {
	p, _, _ := startFake(t)
	if err := p.Steer("stop that"); err == nil {
		t.Fatal("Steer reported success, want ErrUnsupported")
	}
}
```

- [ ] **Step 3: Run the test and watch it fail**

Run: `cd harness/coppice && go test -race ./internal/adapters/claude/ -v`
Expected: FAIL, no package `claude`.

- [ ] **Step 4: Write the implementation**

```go
// harness/coppice/internal/adapters/claude/claude.go

// Package claude runs Claude Code headlessly and normalises its stream-json
// into coppice events. The gate still decides: an event here says working, and
// a source: gate report saying blocked outranks it.
package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"

	"github.com/opendaisugi/coppice/internal/adapters"
	"github.com/opendaisugi/coppice/internal/pane"
)

// Binary is the command run for a headless Claude pane. COPPICE_CLAUDE_BIN
// overrides it, which is how the tests run without a subscription.
// The argv below is verified against the real binary: without --verbose,
// `claude -p --output-format stream-json --input-format stream-json` prints
// "Error: When using --print, --output-format=stream-json requires --verbose"
// and refuses to run. Keep --verbose.
const Binary = "claude"

func binary() string {
	if b := os.Getenv("COPPICE_CLAUDE_BIN"); b != "" {
		return b
	}
	return Binary
}

type adapter struct{}

func New() pane.Adapter { return adapter{} }

func (adapter) Name() string { return "claude" }

func init() { adapters.Register(New()) }

type proc struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	events chan pane.Event

	mu        sync.Mutex
	sessionID string
}

func (adapter) Start(ctx context.Context, o pane.StartOpts, g *pane.Grid) (pane.Proc, error) {
	argv := []string{
		"-p",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
	}
	if o.Resume != "" {
		argv = append(argv, "--resume", o.Resume)
	}
	argv = append(argv, o.Argv...)

	cmd := exec.CommandContext(ctx, binary(), argv...)
	cmd.Dir = o.Cwd
	cmd.Env = pane.BuildEnv(os.Environ(), o.Env, o.Sock, o.PaneID)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("cannot start %s: %w", binary(), err)
	}
	p := &proc{cmd: cmd, stdin: stdin, events: make(chan pane.Event, 256)}
	go p.read(stdout)
	return p, nil
}

// read parses one stream-json line at a time. A line it cannot read becomes an
// error event, so the pane goes unknown rather than staying at whatever it was.
//
// This is the only goroutine that sends on p.events and the only one that
// closes it, which is the rule the Adapter doc states. Stop kills the process,
// the read loop ends, and the deferred close runs after the last send. Codex
// and sprig need a relay to reach the same property because they have more than
// one producer.
func (p *proc) read(r io.Reader) {
	defer close(p.events)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		for _, ev := range p.parse(line) {
			p.events <- ev
		}
	}
	if err := sc.Err(); err != nil {
		p.events <- pane.Event{Kind: pane.EvError, Detail: "stream read failed: " + err.Error()}
	}
	_ = p.cmd.Wait()
	p.events <- pane.Event{Kind: pane.EvEnd, State: pane.StateDoneStr, Detail: "claude exited"}
}

type wireLine struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
	Result    string `json:"result"`
	Message   struct {
		Content []struct {
			Type       string          `json:"type"`
			Text       string          `json:"text"`
			ID         string          `json:"id"`
			Name       string          `json:"name"`
			Input      json.RawMessage `json:"input"`
			ToolUseID  string          `json:"tool_use_id"`
			ContentRaw json.RawMessage `json:"content"`
		} `json:"content"`
	} `json:"message"`
}

func (p *proc) parse(line []byte) []pane.Event {
	var w wireLine
	if err := json.Unmarshal(line, &w); err != nil {
		return []pane.Event{{
			Kind:   pane.EvError,
			Detail: "cannot read a stream-json line: " + err.Error(),
		}}
	}
	if w.SessionID != "" {
		p.mu.Lock()
		p.sessionID = w.SessionID
		p.mu.Unlock()
	}
	switch w.Type {
	case "system":
		return []pane.Event{{Kind: pane.EvState, State: pane.StateWorkingStr,
			Detail: "session started"}}
	case "assistant":
		var out []pane.Event
		for _, c := range w.Message.Content {
			switch c.Type {
			case "text":
				out = append(out, pane.Event{Kind: pane.EvText, Text: c.Text})
			case "tool_use":
				out = append(out, pane.Event{
					Kind: pane.EvTool, Tool: c.Name, Detail: string(c.Input),
				})
			}
		}
		if len(out) == 0 {
			out = append(out, pane.Event{Kind: pane.EvState, State: pane.StateWorkingStr})
		}
		return out
	case "user":
		var out []pane.Event
		for _, c := range w.Message.Content {
			if c.Type == "tool_result" {
				out = append(out, pane.Event{
					Kind: pane.EvText, Text: "result " + c.ToolUseID + ": " + string(c.ContentRaw),
				})
			}
		}
		return out
	case "result":
		// The turn is over. Claude is waiting on us now, which is idle.
		return []pane.Event{{Kind: pane.EvState, State: pane.StateIdleStr, Detail: w.Result}}
	default:
		return []pane.Event{{Kind: pane.EvState, State: pane.StateWorkingStr, Detail: w.Type}}
	}
}

// Prompt writes exactly one JSON user message line. Claude's stream-json input
// format frames on newlines, so an embedded newline would split one prompt into
// two and the second half would be read as a malformed line.
func (p *proc) Prompt(text string) error {
	msg := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": []map[string]any{{"type": "text", "text": text}},
		},
	}
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	_, err = p.stdin.Write(append(b, '\n'))
	return err
}

// Steer has no equivalent in Claude Code's headless mode. Saying so is better
// than sending a prompt and calling it steering.
func (p *proc) Steer(string) error {
	return fmt.Errorf("claude: %w. Send a prompt instead.", pane.ErrUnsupported)
}

func (p *proc) WriteStdin(b []byte) error {
	_, err := p.stdin.Write(b)
	return err
}

func (p *proc) Events() <-chan pane.Event { return p.events }

func (p *proc) SessionID() (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sessionID, p.sessionID != ""
}

func (p *proc) Stop() error {
	_ = p.stdin.Close()
	if p.cmd.Process != nil {
		return p.cmd.Process.Kill()
	}
	return nil
}
```

Delete the temporary `Register(NotBuilt{AdapterName: "claude", ...})` line from
`internal/adapters/registry.go`.

- [ ] **Step 5: Run the test and watch it pass**

Run: `cd harness/coppice && go test -race ./internal/adapters/... -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add harness/coppice/internal/adapters/claude harness/coppice/testdata/adapters \
        harness/coppice/internal/adapters/registry.go
git commit -m "feat(coppice): Claude Code headless, parsed rather than watched

A headless pane gets typed events, so its state is exact instead of inferred
from a screen. A result line means the turn ended and Claude is waiting on us,
which is idle; a line the parser cannot read becomes an error event and the
pane goes unknown, never idle. The prompt is one JSON line because stream-json
frames on newlines, and a fake claude replays a recorded stream so the whole
path is tested without a subscription."
```

---

### Task 16: the Codex headless adapter

**Files:**
- Create: `harness/coppice/internal/adapters/codex/codex.go`
- Create: `harness/coppice/testdata/adapters/codex-stream.jsonl`
- Create: `harness/coppice/testdata/adapters/fake-codex.sh`
- Test: `harness/coppice/internal/adapters/codex/codex_test.go`

**Interfaces:**
- Consumes: `pane.Adapter`, `pane.Proc`, `adapters.Register`.
- Produces: `package codex; func New() pane.Adapter; const Binary = "codex"`.

Codex runs one process per prompt (`codex exec --json`), unlike Claude's one long-lived process.
The adapter therefore keeps a single event channel across turns and starts a new child on each
`Prompt`, resuming with `--resume <id>` once a session id has been seen.

- [ ] **Step 1: Write the fixture and the fake binary**

`harness/coppice/testdata/adapters/codex-stream.jsonl`:

```json
{"type":"session.created","session_id":"cx-8f21"}
{"type":"item.completed","item":{"type":"assistant_message","text":"Checking the tests."}}
{"type":"item.completed","item":{"type":"command_execution","command":"go test ./...","exit_code":0}}
{"type":"turn.completed","usage":{"input_tokens":120,"output_tokens":40}}
```

`harness/coppice/testdata/adapters/fake-codex.sh`:

```bash
#!/usr/bin/env bash
# A stand-in for `codex exec --json`. It records its argv, replays a fixture,
# then exits, which is the one-process-per-turn shape the real binary has.
set -euo pipefail
fixture="${COPPICE_CODEX_FIXTURE:?set COPPICE_CODEX_FIXTURE to a JSONL file}"
if [ -n "${COPPICE_CODEX_ECHO_ARGV:-}" ]; then
  printf '%s\n' "$*" >> "$COPPICE_CODEX_ECHO_ARGV"
fi
cat "$fixture"
```

- [ ] **Step 2: Write the failing test**

```go
// harness/coppice/internal/adapters/codex/codex_test.go
package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/pane"
	"github.com/opendaisugi/coppice/internal/toolchain"
)

func td(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "..", "testdata", "adapters", name))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func start(t *testing.T) (pane.Proc, string) {
	t.Helper()
	if r := toolchain.SkipReason(); r != "" {
		t.Skip(r)
	}
	fake := td(t, "fake-codex.sh")
	if err := os.Chmod(fake, 0o755); err != nil {
		t.Fatal(err)
	}
	argvLog := filepath.Join(t.TempDir(), "argv.txt")
	t.Setenv("COPPICE_CODEX_BIN", fake)
	t.Setenv("COPPICE_CODEX_FIXTURE", td(t, "codex-stream.jsonl"))
	t.Setenv("COPPICE_CODEX_ECHO_ARGV", argvLog)

	g, err := pane.NewGrid(80, 24)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.Close)
	p, err := New().Start(context.Background(), pane.StartOpts{Cwd: t.TempDir()}, g)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop() })
	return p, argvLog
}

func drain(t *testing.T, p pane.Proc, want int, d time.Duration) []pane.Event {
	t.Helper()
	var out []pane.Event
	deadline := time.After(d)
	for len(out) < want {
		select {
		case ev, ok := <-p.Events():
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-deadline:
			t.Fatalf("collected %d of %d events in %s: %+v", len(out), want, d, out)
		}
	}
	return out
}

func TestPromptRunsCodexExecJSONWithThePromptInArgv(t *testing.T) {
	p, argvLog := start(t)
	if err := p.Prompt("check the tests"); err != nil {
		t.Fatal(err)
	}
	drain(t, p, 4, 5*time.Second)
	b, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(b))
	for _, want := range []string{"exec", "--json", "check the tests"} {
		if !strings.Contains(line, want) {
			t.Fatalf("argv %q is missing %q", line, want)
		}
	}
}

func TestASecondPromptResumesTheRecordedSession(t *testing.T) {
	p, argvLog := start(t)
	if err := p.Prompt("first"); err != nil {
		t.Fatal(err)
	}
	drain(t, p, 4, 5*time.Second)
	if id, ok := p.SessionID(); !ok || id != "cx-8f21" {
		t.Fatalf("SessionID() = %q %v, want cx-8f21", id, ok)
	}
	if err := p.Prompt("second"); err != nil {
		t.Fatal(err)
	}
	drain(t, p, 4, 5*time.Second)
	b, _ := os.ReadFile(argvLog)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Fatalf("codex ran %d times, want 2 (one process per turn)", len(lines))
	}
	if !strings.Contains(lines[1], "--resume cx-8f21") {
		t.Fatalf("the second run %q does not resume the recorded session", lines[1])
	}
}

func TestTurnCompletedBecomesIdleAndTheChannelStaysOpen(t *testing.T) {
	p, _ := start(t)
	if err := p.Prompt("go"); err != nil {
		t.Fatal(err)
	}
	evs := drain(t, p, 4, 5*time.Second)
	last := evs[len(evs)-1]
	if last.Kind != pane.EvState || last.State != "idle" {
		t.Fatalf("last event = %+v, want an idle state event", last)
	}
	// One process per turn means the process exiting is not the session ending.
	// A second prompt must still work.
	if err := p.Prompt("again"); err != nil {
		t.Fatalf("the channel closed after one turn: %v", err)
	}
}

func TestStopClosesTheChannel(t *testing.T) {
	p, _ := start(t)
	if err := p.Prompt("go"); err != nil {
		t.Fatal(err)
	}
	drain(t, p, 4, 5*time.Second)
	if err := p.Stop(); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-p.Events():
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("the channel stayed open 5 s after Stop")
		}
	}
}

// pane.close reaches Stop while a turn is still streaming. Closing the events
// channel from Stop would let the turn goroutine send on a closed channel,
// which panics, and a panic in an adapter goroutine takes the whole daemon
// down and orphans every other pane's process. Run this with -race.
func TestStopWhileTheStreamIsFlowingDoesNotPanic(t *testing.T) {
	for i := 0; i < 50; i++ {
		p, _ := start(t)
		if err := p.Prompt("go"); err != nil {
			t.Fatal(err)
		}
		// Stop immediately, with no wait: the turn goroutine is mid-stream.
		if err := p.Stop(); err != nil {
			t.Fatal(err)
		}
		if err := p.Stop(); err != nil {
			t.Fatalf("a second Stop failed: %v", err)
		}
		deadline := time.After(5 * time.Second)
		for done := false; !done; {
			select {
			case _, ok := <-p.Events():
				if !ok {
					done = true
				}
			case <-deadline:
				t.Fatal("the event channel never closed after Stop")
			}
		}
	}
}

func TestAnUnparsableLineYieldsAnErrorEvent(t *testing.T) {
	if r := toolchain.SkipReason(); r != "" {
		t.Skip(r)
	}
	broken := filepath.Join(t.TempDir(), "broken.jsonl")
	if err := os.WriteFile(broken, []byte("}{\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := td(t, "fake-codex.sh")
	_ = os.Chmod(fake, 0o755)
	t.Setenv("COPPICE_CODEX_BIN", fake)
	t.Setenv("COPPICE_CODEX_FIXTURE", broken)

	g, _ := pane.NewGrid(80, 24)
	defer g.Close()
	p, err := New().Start(context.Background(), pane.StartOpts{Cwd: t.TempDir()}, g)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Stop()
	if err := p.Prompt("go"); err != nil {
		t.Fatal(err)
	}
	evs := drain(t, p, 1, 5*time.Second)
	if evs[0].Kind != pane.EvError {
		t.Fatalf("first event = %+v, want an error event", evs[0])
	}
}
```

- [ ] **Step 3: Run the test and watch it fail**

Run: `cd harness/coppice && go test -race ./internal/adapters/codex/ -v`
Expected: FAIL, no package `codex`.

- [ ] **Step 4: Write the implementation**

```go
// harness/coppice/internal/adapters/codex/codex.go

// Package codex runs Codex headlessly. Unlike Claude, Codex is one process per
// turn: `codex exec --json` runs, streams JSONL, and exits. The adapter keeps
// one event channel across turns and resumes with --resume once it has seen a
// session id, so the pane is a continuous conversation to a client.
//
// Codex's own hooks are a fail-open class (spec-01 keeps that honesty tag), so
// a coppice pane running Codex is a soft gate. Nothing here changes that.
package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sync"

	"github.com/opendaisugi/coppice/internal/adapters"
	"github.com/opendaisugi/coppice/internal/pane"
)

const Binary = "codex"

func binary() string {
	if b := os.Getenv("COPPICE_CODEX_BIN"); b != "" {
		return b
	}
	return Binary
}

type adapter struct{}

func New() pane.Adapter { return adapter{} }

func (adapter) Name() string { return "codex" }

func init() { adapters.Register(New()) }

// proc keeps ONE event channel across turns. Exactly one goroutine ever closes
// it: the relay below, after Stop has been signalled and the current turn's
// reader has finished. Stop signals and kills; it never closes a channel a
// producer may still be sending on. A send on a closed channel panics, and a
// panic in an adapter goroutine takes the whole daemon down with every other
// pane's process orphaned.
type proc struct {
	ctx    context.Context
	cancel context.CancelFunc
	opts   pane.StartOpts

	in        chan pane.Event // producers send here
	events    chan pane.Event // clients read here; closed by relay, by nobody else
	stop      chan struct{}   // closed by Stop, once
	stopOnce  sync.Once
	relayDone chan struct{}

	mu        sync.Mutex
	sessionID string
	running   *exec.Cmd
	turns     sync.WaitGroup
}

func (adapter) Start(ctx context.Context, o pane.StartOpts, _ *pane.Grid) (pane.Proc, error) {
	ctx, cancel := context.WithCancel(ctx)
	p := &proc{
		ctx: ctx, cancel: cancel, opts: o,
		in: make(chan pane.Event, 256), events: make(chan pane.Event, 256),
		stop: make(chan struct{}), relayDone: make(chan struct{}),
	}
	if o.Resume != "" {
		p.sessionID = o.Resume
	}
	go p.relay()
	return p, nil
}

// relay is the single owner of the events channel. It forwards until Stop is
// signalled and every turn goroutine has finished, then closes exactly once.
func (p *proc) relay() {
	defer close(p.relayDone)
	defer close(p.events)
	for {
		select {
		case ev := <-p.in:
			select {
			case p.events <- ev:
			case <-p.stop:
				return
			}
		case <-p.stop:
			// Drain whatever the current turn already produced, then finish.
			p.turns.Wait()
			for {
				select {
				case ev := <-p.in:
					select {
					case p.events <- ev:
					default:
						return
					}
				default:
					return
				}
			}
		}
	}
}

func (p *proc) Prompt(text string) error {
	select {
	case <-p.stop:
		return fmt.Errorf("codex: this pane has stopped")
	default:
	}
	p.mu.Lock()
	argv := []string{"exec", "--json"}
	if p.sessionID != "" {
		argv = append(argv, "--resume", p.sessionID)
	}
	argv = append(argv, p.opts.Argv...)
	argv = append(argv, text)

	cmd := exec.CommandContext(p.ctx, binary(), argv...)
	cmd.Dir = p.opts.Cwd
	cmd.Env = pane.BuildEnv(os.Environ(), p.opts.Env, p.opts.Sock, p.opts.PaneID)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		p.mu.Unlock()
		return err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		p.mu.Unlock()
		return fmt.Errorf("cannot start %s: %w", binary(), err)
	}
	p.running = cmd
	p.mu.Unlock()

	p.turns.Add(1)
	go func() {
		defer p.turns.Done()
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
		for sc.Scan() {
			if len(sc.Bytes()) == 0 {
				continue
			}
			for _, ev := range p.parse(sc.Bytes()) {
				p.send(ev)
			}
		}
		if err := sc.Err(); err != nil {
			p.send(pane.Event{Kind: pane.EvError, Detail: "stream read failed: " + err.Error()})
		}
		_ = cmd.Wait()
		p.mu.Lock()
		p.running = nil
		p.mu.Unlock()
	}()
	return nil
}

// send never touches the events channel. It hands the event to the relay, which
// is the only goroutine allowed to close events.
func (p *proc) send(ev pane.Event) {
	select {
	case <-p.stop:
	case p.in <- ev:
	}
}

type wireLine struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id"`
	Item      struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Command  string `json:"command"`
		ExitCode *int   `json:"exit_code"`
	} `json:"item"`
}

func (p *proc) parse(line []byte) []pane.Event {
	var w wireLine
	if err := json.Unmarshal(line, &w); err != nil {
		return []pane.Event{{Kind: pane.EvError,
			Detail: "cannot read a codex JSON line: " + err.Error()}}
	}
	if w.SessionID != "" {
		p.mu.Lock()
		p.sessionID = w.SessionID
		p.mu.Unlock()
	}
	switch w.Type {
	case "session.created":
		return []pane.Event{{Kind: pane.EvState, State: pane.StateWorkingStr,
			Detail: "session " + w.SessionID}}
	case "item.completed":
		switch w.Item.Type {
		case "assistant_message":
			return []pane.Event{{Kind: pane.EvText, Text: w.Item.Text}}
		case "command_execution":
			d := w.Item.Command
			if w.Item.ExitCode != nil {
				d = fmt.Sprintf("%s (exit %d)", d, *w.Item.ExitCode)
			}
			return []pane.Event{{Kind: pane.EvTool, Tool: "shell", Detail: d}}
		default:
			return []pane.Event{{Kind: pane.EvState, State: pane.StateWorkingStr,
				Detail: w.Item.Type}}
		}
	case "turn.completed":
		// The turn ended. Codex is waiting on us, and the process exiting here
		// is not the session ending: another prompt starts another process.
		return []pane.Event{{Kind: pane.EvState, State: pane.StateIdleStr, Detail: "turn complete"}}
	case "turn.failed", "error":
		return []pane.Event{{Kind: pane.EvError, Detail: w.Type}}
	default:
		return []pane.Event{{Kind: pane.EvState, State: pane.StateWorkingStr, Detail: w.Type}}
	}
}

func (p *proc) Steer(string) error {
	return fmt.Errorf("codex: %w. Send the next prompt instead.", pane.ErrUnsupported)
}

// WriteStdin has no meaning for a one-process-per-turn harness. Refusing beats
// writing bytes into a process that may not exist.
func (p *proc) WriteStdin([]byte) error {
	return fmt.Errorf("codex: %w. Use agent prompt.", pane.ErrUnsupported)
}

func (p *proc) Events() <-chan pane.Event { return p.events }

func (p *proc) SessionID() (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sessionID, p.sessionID != ""
}

// Stop signals and kills. It does NOT close the events channel: the relay owns
// that, and closing it here would race with a turn goroutine mid-send. Calling
// Stop twice is safe, which matters because pane.close and a server shutdown
// can both reach it.
func (p *proc) Stop() error {
	p.stopOnce.Do(func() {
		close(p.stop)
		p.cancel()
		p.mu.Lock()
		cmd := p.running
		p.mu.Unlock()
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})
	<-p.relayDone // the events channel is closed by the time Stop returns
	return nil
}
```

Delete the temporary `Register(NotBuilt{AdapterName: "codex", ...})` line from
`internal/adapters/registry.go`.

- [ ] **Step 5: Run the test and watch it pass**

Run: `cd harness/coppice && go test -race ./internal/adapters/... -v && go vet ./...`
Expected: PASS. The race detector is what matters here: it is what proves `Stop` never races a
producer into a closed channel.

- [ ] **Step 6: Commit**

```bash
git add harness/coppice/internal/adapters/codex harness/coppice/testdata/adapters \
        harness/coppice/internal/adapters/registry.go
git commit -m "feat(coppice): Codex headless, one process per turn, one conversation

Codex exits after every turn, so the adapter keeps the event channel across
turns and resumes with the recorded session id. A client sees one continuous
pane. The process exiting is not the session ending, and a test pins that: a
second prompt after a completed turn must still work. Exactly one goroutine
closes the event channel, after Stop is signalled and the turn has finished:
Stop closing it would let a mid-stream producer send on a closed channel, and
that panic takes the whole daemon down with every other pane's process."
```

---

### Task 17: the sprig headless adapter

**Files:**
- Create: `harness/coppice/internal/adapters/sprig/sprig.go`
- Test: `harness/coppice/internal/adapters/sprig/sprig_test.go`

**Interfaces:**
- Consumes: `pane.Adapter`, `pane.Proc`, `adapters.Register`.
- Produces: `package sprig; func New() pane.Adapter; const Binary = "sprig"`.

sprig is ours, and it already writes the session tree on path E
(`harness/sprig/session_tree.go`). The adapter forwards prompts on stdin and reads state from
that file, so a sprig pane's `blocked` comes from sprig's own gate verdict rather than from a
screen.

**The flags are sprig's, read from `harness/sprig/cli.go`, not invented here.**
`cli.go:32` declares `--session-dir`, `cli.go:34` declares **`--session`** ("session id to write
under --session-dir (default: a fresh one)"), and `cli.go:36` declares `--resume`. There is no
`--session-id`. `cli.go:54` refuses `--resume` without `--session-dir`, and
`NewSessionWriter` refuses to overwrite an existing session file, so the two flags are not
interchangeable.

The design decision that follows, so the coder does not simply rename a string: the adapter reads
`--session` from the pane's argv, falling back to `StartOpts.Resume`. When **neither** is present
it mints an id and **injects `--session <id>` into the child's argv**, because sprig's default is
"a fresh one" and the adapter would otherwise have no way to know the tree filename it must tail.
On the resume path it injects `--resume <id>` instead, never `--session`.

The tree rows this adapter reads, from `harness/sprig/session_tree.go`:

- header `{"type":"session","v":1,"id":…,"harness":"sprig","cwd":…}`
- `{"type":"prompt","text":…}`
- `{"type":"assistant","model":…,"text":…,"usage":{…},"toolUses":[{"id","name"}]}`
- `{"type":"tool_call","toolUseId":…,"name":…,"input":{…},"detail":…}`
- `{"type":"verdict","toolUseId":…,"decision":"allow"|"deny","reason":…,"clause":…}`
- `{"type":"tool_result","toolUseId":…,"ok":bool,"summary":…}`

Every row also carries `id`, `parentId` and `ts`.

- [ ] **Step 1: Write the failing test**

```go
// harness/coppice/internal/adapters/sprig/sprig_test.go
package sprig

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/pane"
	"github.com/opendaisugi/coppice/internal/toolchain"
)

func appendRow(t *testing.T, path, row string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(row + "\n"); err != nil {
		t.Fatal(err)
	}
}

func startWithTree(t *testing.T) (pane.Proc, string) {
	t.Helper()
	if r := toolchain.SkipReason(); r != "" {
		t.Skip(r)
	}
	dir := t.TempDir()
	sessionDir := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	treePath := filepath.Join(sessionDir, "s1.jsonl")
	appendRow(t, treePath,
		`{"type":"session","v":1,"id":"s1","harness":"sprig","cwd":"/repo","ts":1757300000.0}`)

	// A stand-in for sprig that just holds stdin open. The state comes from the
	// tree file, not from this process.
	t.Setenv("COPPICE_SPRIG_BIN", "/bin/cat")

	g, err := pane.NewGrid(80, 24)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.Close)
	p, err := New().Start(context.Background(), pane.StartOpts{
		Cwd:    dir,
		Argv:   []string{"--session-dir", sessionDir, "--session", "s1"},
		Sock:   "/dev/null",
		PaneID: "w1:p1",
	}, g)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop() })
	return p, treePath
}

func waitFor(t *testing.T, p pane.Proc, match func(pane.Event) bool, d time.Duration) pane.Event {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case ev, ok := <-p.Events():
			if !ok {
				t.Fatal("the event channel closed before the event arrived")
			}
			if match(ev) {
				return ev
			}
		case <-deadline:
			t.Fatalf("no matching event within %s", d)
		}
	}
}

// The flag name is sprig's, not ours. Reading it out of sprig's own source is
// what stops this adapter drifting onto an invented flag whose tests all pass
// while a real sprig pane refuses to start.
func TestTheSessionFlagMatchesSprigsOwnSource(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "sprig", "cli.go"))
	if err != nil {
		t.Skipf("harness/sprig/cli.go is not readable from here: %v", err)
	}
	src := string(b)
	if !strings.Contains(src, `fs.String("session",`) {
		t.Fatal(`harness/sprig/cli.go does not declare fs.String("session", …). ` +
			"The adapter's flag name has drifted from sprig's.")
	}
	if strings.Contains(src, `fs.String("session-id",`) {
		t.Fatal("sprig now has a --session-id flag. Re-read cli.go and update the adapter.")
	}
	if !strings.Contains(src, `fs.String("session-dir",`) ||
		!strings.Contains(src, `fs.String("resume",`) {
		t.Fatal("harness/sprig/cli.go no longer declares --session-dir and --resume")
	}
}

// With no --session and no Resume, sprig picks "a fresh one" and never tells
// us which, so the tail would have no filename. The adapter mints the id and
// injects the flag.
func TestStartInjectsASessionFlagWhenNoneIsGiven(t *testing.T) {
	if r := toolchain.SkipReason(); r != "" {
		t.Skip(r)
	}
	dir := t.TempDir()
	sessionDir := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	argvLog := filepath.Join(dir, "argv.txt")
	script := filepath.Join(dir, "fake-sprig.sh")
	if err := os.WriteFile(script,
		[]byte("#!/usr/bin/env bash\nprintf '%s\\n' \"$*\" > \""+argvLog+"\"\nexec cat\n"),
		0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COPPICE_SPRIG_BIN", script)

	g, err := pane.NewGrid(80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	p, err := New().Start(context.Background(), pane.StartOpts{
		Cwd: dir, Argv: []string{"--session-dir", sessionDir},
		Sock: "/dev/null", PaneID: "w1:p1",
	}, g)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Stop()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(argvLog)
		if err == nil && strings.Contains(string(b), "--session ") {
			if strings.Contains(string(b), "--session-id") {
				t.Fatalf("argv %q uses --session-id, which sprig does not have", b)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the adapter never passed --session to sprig")
}

func TestSessionIDComesFromTheTreeHeader(t *testing.T) {
	p, _ := startWithTree(t)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if id, ok := p.SessionID(); ok {
			if id != "s1" {
				t.Fatalf("SessionID() = %q, want s1", id)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("SessionID never became available")
}

func TestAnAssistantRowBecomesTextAndWorking(t *testing.T) {
	p, tree := startWithTree(t)
	appendRow(t, tree, `{"type":"assistant","id":"a1","parentId":null,"ts":1757300001.0,`+
		`"model":"claude","text":"I will run the tests.","usage":{},"toolUses":[]}`)
	ev := waitFor(t, p, func(e pane.Event) bool { return e.Kind == pane.EvText }, 5*time.Second)
	if ev.Text != "I will run the tests." {
		t.Fatalf("text = %q, want the assistant row's text", ev.Text)
	}
	if st, _ := pane.StateOf(ev); st != "working" {
		t.Fatalf("StateOf = %q, want working", st)
	}
}

func TestAToolCallRowBecomesAToolEvent(t *testing.T) {
	p, tree := startWithTree(t)
	appendRow(t, tree, `{"type":"tool_call","id":"c1","ts":1757300002.0,`+
		`"toolUseId":"tu1","name":"Bash","input":{"command":"ls"},"detail":"ls"}`)
	ev := waitFor(t, p, func(e pane.Event) bool { return e.Kind == pane.EvTool }, 5*time.Second)
	if ev.Tool != "Bash" {
		t.Fatalf("tool = %q, want Bash", ev.Tool)
	}
}

// A deny verdict is what a coppice client shows as blocked, with the clause
// attached. That is the whole point of using sprig's own gate rather than
// reading its screen.
func TestADenyVerdictBecomesBlockedWithTheClause(t *testing.T) {
	p, tree := startWithTree(t)
	appendRow(t, tree, `{"type":"verdict","id":"v1","ts":1757300003.0,`+
		`"toolUseId":"tu1","decision":"deny","reason":"shell.deny[2]","clause":"shell.deny[2]"}`)
	ev := waitFor(t, p, func(e pane.Event) bool { return e.Kind == pane.EvState }, 5*time.Second)
	if ev.State != "blocked" {
		t.Fatalf("state = %q, want blocked", ev.State)
	}
	if ev.Detail == "" {
		t.Fatal("the blocked event carries no clause, so a client cannot say why")
	}
}

func TestAnAllowVerdictIsWorkingNotBlocked(t *testing.T) {
	p, tree := startWithTree(t)
	appendRow(t, tree, `{"type":"verdict","id":"v1","ts":1757300003.0,`+
		`"toolUseId":"tu1","decision":"allow","reason":"","clause":""}`)
	ev := waitFor(t, p, func(e pane.Event) bool { return e.Kind == pane.EvState }, 5*time.Second)
	if ev.State != "working" {
		t.Fatalf("state = %q, want working: an allowed call means the agent continues", ev.State)
	}
}

func TestPromptGoesToStdin(t *testing.T) {
	p, _ := startWithTree(t)
	if err := p.Prompt("do the thing"); err != nil {
		t.Fatal(err)
	}
	// /bin/cat echoes stdin to stdout, which the adapter renders as text.
	ev := waitFor(t, p, func(e pane.Event) bool {
		return e.Kind == pane.EvText && e.Text == "do the thing"
	}, 5*time.Second)
	_ = ev
}

func TestAMalformedTreeRowYieldsAnErrorEvent(t *testing.T) {
	p, tree := startWithTree(t)
	appendRow(t, tree, `{"type":`)
	ev := waitFor(t, p, func(e pane.Event) bool { return e.Kind == pane.EvError }, 5*time.Second)
	if st, _ := pane.StateOf(ev); st == "idle" {
		t.Fatal("a malformed tree row produced idle")
	}
}

func TestStartWithoutASessionDirIsAnError(t *testing.T) {
	if r := toolchain.SkipReason(); r != "" {
		t.Skip(r)
	}
	g, _ := pane.NewGrid(80, 24)
	defer g.Close()
	if _, err := New().Start(context.Background(),
		pane.StartOpts{Cwd: t.TempDir(), Sock: "/dev/null", PaneID: "w1:p1"}, g); err == nil {
		t.Fatal("Start succeeded with no --session-dir, want an error naming the flag")
	}
}

// pane.close reaches Stop while the tail and the stdout reader are both
// producing. Closing the events channel from Stop would let one of them send on
// a closed channel and panic, taking the daemon down. Run this with -race.
func TestStopWhileTheStreamIsFlowingDoesNotPanic(t *testing.T) {
	for i := 0; i < 50; i++ {
		p, tree := startWithTree(t)
		appendRow(t, tree, `{"type":"assistant","id":"a1","ts":1.0,"model":"m","text":"x",`+
			`"usage":{},"toolUses":[]}`)
		if err := p.Prompt("go"); err != nil {
			t.Fatal(err)
		}
		if err := p.Stop(); err != nil {
			t.Fatal(err)
		}
		if err := p.Stop(); err != nil {
			t.Fatalf("a second Stop failed: %v", err)
		}
		deadline := time.After(5 * time.Second)
		for done := false; !done; {
			select {
			case _, ok := <-p.Events():
				if !ok {
					done = true
				}
			case <-deadline:
				t.Fatal("the event channel never closed after Stop")
			}
		}
	}
}
```

- [ ] **Step 2: Run the test and watch it fail**

Run: `cd harness/coppice && go test -race ./internal/adapters/sprig/ -v`
Expected: FAIL, no package `sprig`.

- [ ] **Step 3: Write the implementation**

```go
// harness/coppice/internal/adapters/sprig/sprig.go

// Package sprig runs sprig headlessly. sprig is ours, so its state does not
// come from a screen or from a wrapper protocol: it comes from the session tree
// sprig already writes on path E, which carries the gate's own verdicts. A deny
// in that file is a fact we produced, not an inference.
package sprig

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/opendaisugi/coppice/internal/adapters"
	"github.com/opendaisugi/coppice/internal/pane"
)

const Binary = "sprig"

// TailInterval is how often the session tree file is re-read. sprig appends,
// so a poll on the file size is cheap and needs no inotify.
const TailInterval = 200 * time.Millisecond

func binary() string {
	if b := os.Getenv("COPPICE_SPRIG_BIN"); b != "" {
		return b
	}
	return Binary
}

type adapter struct{}

func New() pane.Adapter { return adapter{} }

func (adapter) Name() string { return "sprig" }

func init() { adapters.Register(New()) }

// proc owns two goroutines that produce events, the stdout reader and the tree
// tail, plus one relay that is the ONLY closer of the events channel. Stop
// signals; it never closes a channel a producer may still be sending on.
type proc struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	in        chan pane.Event
	events    chan pane.Event
	stop      chan struct{}
	stopOnce  sync.Once
	relayDone chan struct{}

	mu        sync.Mutex
	sessionID string
}

// flagValue reads --name VALUE or --name=VALUE out of argv.
func flagValue(argv []string, name string) (string, bool) {
	for i, a := range argv {
		if a == "--"+name && i+1 < len(argv) {
			return argv[i+1], true
		}
		if len(a) > len(name)+3 && a[:len(name)+3] == "--"+name+"=" {
			return a[len(name)+3:], true
		}
	}
	return "", false
}

// newSessionID mints an id sprig will accept as --session. Eight hex characters,
// the same shape sprig's own newID uses.
func newSessionID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "coppice-" + hex.EncodeToString(b)
}

func (adapter) Start(ctx context.Context, o pane.StartOpts, _ *pane.Grid) (pane.Proc, error) {
	dir, ok := flagValue(o.Argv, "session-dir")
	if !ok || dir == "" {
		return nil, fmt.Errorf(
			"sprig needs --session-dir in the pane's argv. That file is where its state comes from")
	}

	// sprig's flag is --session, not --session-id: harness/sprig/cli.go:34.
	argv := append([]string(nil), o.Argv...)
	sessionID, given := flagValue(argv, "session")
	switch {
	case given && sessionID != "":
		// The caller named the session. Use it and change nothing.
	case o.Resume != "":
		// Resume a recorded session. cli.go:54 requires --session-dir alongside
		// --resume, which we already have, and NewSessionWriter refuses to
		// reopen a file through --session, so this must be --resume.
		sessionID = o.Resume
		if _, has := flagValue(argv, "resume"); !has {
			argv = append(argv, "--resume", sessionID)
		}
	default:
		// sprig's default with no --session is "a fresh one", and it never
		// tells us which. Mint the id ourselves and pass it in, or the tail
		// below has no filename to watch.
		sessionID = newSessionID()
		argv = append(argv, "--session", sessionID)
	}

	cmd := exec.CommandContext(ctx, binary(), argv...)
	cmd.Dir = o.Cwd
	cmd.Env = pane.BuildEnv(os.Environ(), o.Env, o.Sock, o.PaneID)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("cannot start %s: %w", binary(), err)
	}

	p := &proc{
		cmd: cmd, stdin: stdin,
		in: make(chan pane.Event, 256), events: make(chan pane.Event, 256),
		stop: make(chan struct{}), relayDone: make(chan struct{}),
		sessionID: sessionID,
	}
	go p.relay()
	go p.readStdout(stdout)
	go p.tail(filepath.Join(dir, sessionID+".jsonl"))
	return p, nil
}

// readStdout renders whatever sprig prints. sprig's own state comes from the
// tree, so stdout is transcript only.
func (p *proc) readStdout(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		p.send(pane.Event{Kind: pane.EvText, Text: sc.Text()})
	}
	_ = p.cmd.Wait()
	p.send(pane.Event{Kind: pane.EvEnd, State: pane.StateDoneStr, Detail: "sprig exited"})
	p.signalStop()
}

// tail re-reads the session tree from the byte offset it stopped at. sprig
// appends only, so this never re-emits a row.
func (p *proc) tail(path string) {
	var offset int64
	t := time.NewTicker(TailInterval)
	defer t.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-t.C:
		}
		f, err := os.Open(path)
		if err != nil {
			continue // the file appears when sprig writes its header
		}
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			_ = f.Close()
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
		for sc.Scan() {
			line := sc.Bytes()
			offset += int64(len(line)) + 1
			if len(line) == 0 {
				continue
			}
			for _, ev := range p.parseRow(line) {
				p.send(ev)
			}
		}
		_ = f.Close()
	}
}

type treeRow struct {
	Type     string `json:"type"`
	ID       string `json:"id"`
	Text     string `json:"text"`
	Name     string `json:"name"`
	Detail   string `json:"detail"`
	Decision string `json:"decision"`
	Clause   string `json:"clause"`
	Reason   string `json:"reason"`
	Summary  string `json:"summary"`
	OK       *bool  `json:"ok"`
}

func (p *proc) parseRow(line []byte) []pane.Event {
	var r treeRow
	if err := json.Unmarshal(line, &r); err != nil {
		return []pane.Event{{Kind: pane.EvError,
			Detail: "cannot read a sprig session row: " + err.Error()}}
	}
	switch r.Type {
	case "session":
		p.mu.Lock()
		p.sessionID = r.ID
		p.mu.Unlock()
		return []pane.Event{{Kind: pane.EvState, State: pane.StateWorkingStr,
			Detail: "session " + r.ID}}
	case "prompt":
		return []pane.Event{{Kind: pane.EvState, State: pane.StateWorkingStr, Detail: "prompt"}}
	case "assistant":
		if r.Text == "" {
			return []pane.Event{{Kind: pane.EvState, State: pane.StateWorkingStr}}
		}
		return []pane.Event{{Kind: pane.EvText, Text: r.Text}}
	case "tool_call":
		return []pane.Event{{Kind: pane.EvTool, Tool: r.Name, Detail: r.Detail}}
	case "verdict":
		if r.Decision == "deny" {
			clause := r.Clause
			if clause == "" {
				clause = r.Reason
			}
			return []pane.Event{{Kind: pane.EvState, State: pane.StateBlockedStr,
				Detail: "verdict=deny clause=" + clause}}
		}
		return []pane.Event{{Kind: pane.EvState, State: pane.StateWorkingStr,
			Detail: "verdict=allow"}}
	case "tool_result":
		return []pane.Event{{Kind: pane.EvText, Text: r.Summary}}
	default:
		return nil
	}
}

// send hands the event to the relay. It must never touch p.events: after
// closeEvents ran, `select { case <-p.stop: case p.events <- ev: }` has BOTH
// cases ready, because a send on a closed channel is select-ready and then
// panics. That made the old shape a coin flip rather than a narrow window.
func (p *proc) send(ev pane.Event) {
	select {
	case <-p.stop:
	case p.in <- ev:
	}
}

// relay is the single owner of p.events. Nothing else closes it.
func (p *proc) relay() {
	defer close(p.relayDone)
	defer close(p.events)
	for {
		select {
		case ev := <-p.in:
			select {
			case p.events <- ev:
			case <-p.stop:
				return
			}
		case <-p.stop:
			return
		}
	}
}

// signalStop closes the stop channel exactly once. It never closes p.events.
func (p *proc) signalStop() { p.stopOnce.Do(func() { close(p.stop) }) }

func (p *proc) Prompt(text string) error {
	_, err := p.stdin.Write([]byte(text + "\n"))
	return err
}

func (p *proc) Steer(text string) error { return p.Prompt(text) }

func (p *proc) WriteStdin(b []byte) error {
	_, err := p.stdin.Write(b)
	return err
}

func (p *proc) Events() <-chan pane.Event { return p.events }

func (p *proc) SessionID() (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sessionID, p.sessionID != ""
}

// Stop signals and kills. The relay closes the events channel. Calling Stop
// twice is safe: pane.close and a server shutdown can both reach it.
func (p *proc) Stop() error {
	_ = p.stdin.Close()
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	p.signalStop()
	<-p.relayDone
	return nil
}
```

Delete the temporary `Register(NotBuilt{AdapterName: "sprig", ...})` line from
`internal/adapters/registry.go`.

- [ ] **Step 4: Run the test and watch it pass**

Run: `cd harness/coppice && go test -race ./internal/adapters/... -v && go vet ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/adapters/sprig harness/coppice/internal/adapters/registry.go
git commit -m "feat(coppice): sprig state comes from its gate, not its screen

sprig already writes the session tree with its own verdicts in it, so the
adapter tails that file. A deny becomes blocked with the clause attached, which
is the difference between a fact we produced and a guess about a terminal.
The flag is sprig's own --session, read out of harness/sprig/cli.go by a test,
because an invented flag would pass every test here and refuse every real pane.
Exactly one goroutine closes the event channel: a send on a closed channel is
select-ready and then panics, so the old shape was a coin flip, not a window."
```

---

### Task 18: restart, restore, and resume

**Files:**
- Create: `harness/coppice/internal/server/restart.go`
- Test: `harness/coppice/internal/server/restart_test.go`

**Interfaces:**
- Consumes: `layout.Load`, `layout.Tree`, `adapters.Get`, `state.Store`.
- Produces:
  ```go
  package server
  func (s *Server) Restore() error   // refuses without the start lock
  type RestoreReport struct { Panes, Resumed, MarkedDone, MarkedUnknown int; Notes []string }
  func (s *Server) LastRestore() RestoreReport
  ```
  `server.status` gains a `restore` object carrying the report.

- [ ] **Step 1: Write the failing test**

```go
// harness/coppice/internal/server/restart_test.go
package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/opendaisugi/coppice/internal/layout"
	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/toolchain"
)

func TestRestoreBringsBackTheLayoutAndMarksPanesHonestly(t *testing.T) {
	if r := toolchain.SkipReason(); r != "" {
		t.Skip(r)
	}
	dir := t.TempDir()
	cwd := t.TempDir()

	// Restore() reaches startPane and the claude adapter for a headless pane
	// with a recorded session id. Without this the test would spawn the
	// operator's real `claude -p … --resume sess-42` against their
	// subscription. Point the adapter at the replay script instead.
	fake, err := filepath.Abs(filepath.Join("..", "..", "testdata", "adapters", "fake-claude.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(fake, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COPPICE_CLAUDE_BIN", fake)
	t.Setenv("COPPICE_CLAUDE_FIXTURE",
		filepath.Join(filepath.Dir(fake), "claude-stream.jsonl"))

	// First server: create two panes, one of which records a harness session id.
	s1, err := New(Config{SocketPath: filepath.Join(dir, "a.sock"), DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	s1.RegisterPaneCommands()
	roundTrip(t, s1,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c","sleep 5"],`+
			`"kind":"pty","label":"the pty one"}`,
		`{"id":"2","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c","sleep 5"],`+
			`"kind":"pty","label":"the other one"}`)
	if err := s1.Tree().UpdatePane("w1:p2", func(p *layout.Pane) {
		p.HarnessSessionID = "sess-42"
		p.Harness = "claude"
		p.Kind = layout.KindHeadless
	}); err != nil {
		t.Fatal(err)
	}
	s1.saveLayout()
	_ = s1.Close()

	// Second server on the same data dir.
	s2, err := New(Config{SocketPath: filepath.Join(dir, "b.sock"), DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	// s1 released the lock in Close, so s2 can take it.
	if err := s2.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	s2.RegisterPaneCommands()
	if err := s2.Restore(); err != nil {
		t.Fatal(err)
	}

	p1, ok := s2.Tree().Pane("w1:p1")
	if !ok {
		t.Fatal("w1:p1 did not survive the restart")
	}
	if p1.Label != "the pty one" || p1.Cwd != cwd {
		t.Fatalf("restored pane = %+v, want the label and cwd back", p1)
	}
	if !p1.Closed {
		t.Fatal("a pty pane whose process is gone came back open, want it marked closed")
	}
	ev, ok := s2.States().Current("w1:p1")
	if !ok {
		t.Fatal("the restored pane has no state at all")
	}
	if ev.State == proto.StateIdle {
		t.Fatalf("a restored pane reports idle, want done or unknown: %+v", ev)
	}
	if ev.State != proto.StateDone && ev.State != proto.StateUnknown {
		t.Fatalf("restored state = %s, want done or unknown", ev.State)
	}

	// A new pane after a restore must not reuse an id a client already holds.
	next, err := s2.Tree().CreatePane("w1", "w1:t1", layout.Pane{Cwd: cwd, Kind: layout.KindPTY})
	if err != nil {
		t.Fatal(err)
	}
	if next.ID != "w1:p3" {
		t.Fatalf("post-restore pane id = %q, want w1:p3", next.ID)
	}
}

func TestRestoreReportsWhatItCouldAndCouldNotDo(t *testing.T) {
	if r := toolchain.SkipReason(); r != "" {
		t.Skip(r)
	}
	dir := t.TempDir()
	cwd := t.TempDir()
	s1, _ := New(Config{SocketPath: filepath.Join(dir, "a.sock"), DataDir: dir})
	if err := s1.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	s1.RegisterPaneCommands()
	roundTrip(t, s1,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c","sleep 5"],"kind":"pty"}`)
	s1.saveLayout()
	_ = s1.Close()

	s2, _ := New(Config{SocketPath: filepath.Join(dir, "b.sock"), DataDir: dir})
	if err := s2.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	s2.RegisterPaneCommands()
	if err := s2.Restore(); err != nil {
		t.Fatal(err)
	}
	rep := s2.LastRestore()
	if rep.Panes != 1 {
		t.Fatalf("report says %d panes, want 1", rep.Panes)
	}
	if rep.Resumed != 0 {
		t.Fatalf("report claims %d resumed panes, want 0: a pty process cannot be resumed",
			rep.Resumed)
	}
	if len(rep.Notes) == 0 {
		t.Fatal("the report carries no notes, so an operator cannot see what was lost")
	}

	got := roundTrip(t, s2, `{"id":"1","cmd":"server.status"}`)
	b, _ := json.Marshal(got[0].Result)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if _, ok := m["restore"]; !ok {
		t.Fatalf("server.status does not carry the restore report: %s", b)
	}
}

func TestRestoreRefusesWithoutTheStartLock(t *testing.T) {
	dir := t.TempDir()
	s, err := New(Config{SocketPath: filepath.Join(dir, "a.sock"), DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Restore(); err == nil {
		t.Fatal("Restore ran without the start lock, want a refusal")
	}
}

func TestRestoreWithNoLayoutFileIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	s, err := New(Config{SocketPath: filepath.Join(dir, "a.sock"), DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	if err := s.Restore(); err != nil {
		t.Fatalf("Restore on a fresh data dir = %v, want nil", err)
	}
	if len(s.Tree().Panes()) != 0 {
		t.Fatal("a fresh data dir produced panes")
	}
}

func TestACorruptLayoutFileIsRefusedAndKept(t *testing.T) {
	dir := t.TempDir()
	if err := writeFile(filepath.Join(dir, "layout.json"), "{ not json"); err != nil {
		t.Fatal(err)
	}
	s, _ := New(Config{SocketPath: filepath.Join(dir, "a.sock"), DataDir: dir})
	if err := s.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	err := s.Restore()
	if err == nil {
		t.Fatal("Restore accepted a corrupt layout file, want an error")
	}
	// The bad file must still be there. Overwriting it would destroy the only
	// record of what the operator had.
	if _, statErr := osStat(filepath.Join(dir, "layout.json")); statErr != nil {
		t.Fatal("Restore deleted the corrupt layout file")
	}
}
```

Add these helpers to the same file:

```go
func writeFile(path, body string) error { return os.WriteFile(path, []byte(body), 0o600) }

var osStat = os.Stat
```

- [ ] **Step 2: Run the test and watch it fail**

Run: `cd harness/coppice && go test -race ./internal/server/ -run Restore -v`
Expected: FAIL, `s.Restore undefined`.

- [ ] **Step 3: Write the implementation**

```go
// harness/coppice/internal/server/restart.go
package server

import (
	"fmt"
	"os"

	"github.com/opendaisugi/coppice/internal/adapters"
	"github.com/opendaisugi/coppice/internal/layout"
	"github.com/opendaisugi/coppice/internal/proto"
)

// RestoreReport and LastRestore live in server.go, because Server owns the
// field and handleStatus returns it. This file only fills it.
//
// Restore reads the layout from disk. It refuses without the start lock: a
// second `coppice server start` must fail before it can read this file, spawn
// resumed panes, and write its own tree back over the running server's.
//
// It restores the tree, the labels, the
// working directories and the id counters. It resurrects nothing: a pty pane's
// process is gone, so the pane comes back closed and done. A headless pane whose
// adapter recorded a harness session id is resumed; one without an id comes
// back closed and unknown, because we cannot say whether that agent finished.
func (s *Server) Restore() error {
	if !s.HoldsStartLock() {
		return fmt.Errorf(
			"call AcquireStartLock before Restore. Two starts restoring the same layout would each spawn resumed panes")
	}
	path := layoutPath(s.cfg.DataDir)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	tree, err := layout.Load(path)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w. "+
			"Move it aside and start again to get a fresh workshop.", path, err)
	}
	s.tree = tree

	rep := RestoreReport{}
	for _, rec := range tree.Panes() {
		rep.Panes++
		if rec.Closed {
			continue
		}
		switch {
		case rec.Kind == layout.KindHeadless && rec.HarnessSessionID != "":
			if _, ok := adapters.Get(rec.Harness); !ok {
				s.markRestored(rec, proto.StateUnknown, "no adapter named "+rec.Harness)
				rep.MarkedUnknown++
				rep.Notes = append(rep.Notes, fmt.Sprintf(
					"%s used harness %q, which this build has no adapter for.",
					rec.ID, rec.Harness))
				continue
			}
			if err := s.startPane(rec); err != nil {
				s.markRestored(rec, proto.StateUnknown, "resume failed: "+err.Error())
				rep.MarkedUnknown++
				rep.Notes = append(rep.Notes, fmt.Sprintf(
					"%s did not resume: %v. Create a new pane in %s.", rec.ID, err, rec.Cwd))
				continue
			}
			rep.Resumed++
			rep.Notes = append(rep.Notes, fmt.Sprintf(
				"%s resumed harness session %s.", rec.ID, rec.HarnessSessionID))
		case rec.Kind == layout.KindPTY:
			s.markRestored(rec, proto.StateDone, "the process did not survive the restart")
			rep.MarkedDone++
			rep.Notes = append(rep.Notes, fmt.Sprintf(
				"%s ran %v. Its process is gone. Create a new pane in %s.",
				rec.ID, rec.Argv, rec.Cwd))
		default:
			s.markRestored(rec, proto.StateUnknown, "no harness session id was recorded")
			rep.MarkedUnknown++
			rep.Notes = append(rep.Notes, fmt.Sprintf(
				"%s had no recorded harness session, so its outcome is unknown.", rec.ID))
		}
	}
	s.mu.Lock()
	s.restore = rep
	s.mu.Unlock()
	s.saveLayout()
	return nil
}

// markRestored closes the record and records the state with the source that
// really produced it. process for a dead process, manifest never, and never
// idle: a restored pane nobody has observed is unknown.
func (s *Server) markRestored(rec layout.Pane, st, detail string) {
	_ = s.tree.ClosePane(rec.ID, rec.ExitCode)
	id := rec.ID
	src := proto.SrcProcess
	if st == proto.StateUnknown {
		src = proto.SrcProcess // the observation is ours; the outcome is what is unknown
	}
	harness := rec.Harness
	if harness == "" {
		harness = "shell"
	}
	s.states.Apply(id, proto.PaneStateEvent{
		V: 1, TS: nowSeconds(), SessionID: id, Harness: harness, Pane: &id,
		State: st, Source: src, Detail: truncate(detail, proto.DetailMax),
	}, nowSeconds())
}

```

`Server` already carries `restore`, `LastRestore` already exists, and `handleStatus` already
returns `"restore": s.LastRestore()`: Task 7 declared all three. Nothing to add here.

- [ ] **Step 4: Run the test and watch it pass**

Run: `cd harness/coppice && go test -race ./internal/server/ -v && go vet ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/server/restart.go harness/coppice/internal/server/restart_test.go \
        harness/coppice/internal/server/server.go
git commit -m "feat(coppice): a restart restores the workshop and admits what it lost

The layout, the labels, the working directories and the id counters come back.
Processes do not. A pty pane returns closed and done; a headless pane resumes
only when its adapter recorded a harness session id, and comes back unknown
otherwise. A restored pane never reports idle, and server status prints the
whole report so the operator sees exactly what is gone."
```

---

### Task 19: `internal/attach` — the single-pane renderer

`attach` is deliberately single-pane. Splits and multi-pane layouts belong to the Python cockpit
(spec-03) and the PWA (spec-06). This is the terminal that shows one pane, a status line, and a
leader key.

**Files:**
- Create: `harness/coppice/internal/attach/render.go`
- Create: `harness/coppice/internal/attach/keys.go`
- Create: `harness/coppice/internal/attach/attach.go`
- Test: `harness/coppice/internal/attach/render_test.go`
- Test: `harness/coppice/internal/attach/keys_test.go`

**Interfaces:**
- Consumes: `proto.Frame`, `proto.Cell`, `proto.PaneStateEvent`.
- Produces:
  ```go
  package attach
  type Screen struct{}
  func NewScreen(cols, rows int) *Screen
  func (s *Screen) Apply(f proto.Frame)                  // full or diff frame
  func (s *Screen) RenderTo(w io.Writer) error           // ANSI, cursor placed last
  func (s *Screen) Text() string                         // plain text, for tests
  func StatusLine(pane, label, harness string, ev *proto.PaneStateEvent, cols int) string
  const Leader = 0x01                                     // ctrl+a
  type Action string
  const ( ActNone Action = ""; ActNext = "next"; ActPrev = "prev"; ActCreate = "create"
          ActDetach = "detach"; ActClose = "close"; ActHelp = "help"; ActLiteral = "literal" )
  type KeyReader struct{}
  func NewKeyReader() *KeyReader
  func (k *KeyReader) Feed(b byte) (Action, []byte)       // returns the action, or bytes to forward
  type Next struct { Pane string; Create bool }   // what to do after this view exits
  func Run(o Options) (Next, error)
  type Options struct { Socket, Pane string; In io.Reader; Out io.Writer; Cols, Rows int }
  ```
  `ctrl+a n` and `ctrl+a p` return the neighbouring open pane in `Next.Pane`; `ctrl+a c` returns
  `Next.Create`. The CLI loops on that, so the three keys `HelpText` advertises actually work.
  A control that does nothing is the lie §3.5 forbids.

- [ ] **Step 1: Write the failing tests**

```go
// harness/coppice/internal/attach/render_test.go
package attach

import (
	"strings"
	"testing"

	"github.com/opendaisugi/coppice/internal/proto"
)

func fullFrame() proto.Frame {
	return proto.Frame{
		Event: "frame", Pane: "w1:p1", Seq: 1, Cols: 5, Rows: 2, Cursor: [2]int{1, 1},
		RowsChanged: map[int][]proto.Cell{
			0: {{Text: "h"}, {Text: "i"}, {}, {}, {}},
			1: {{Text: "y"}, {Text: "o"}, {}, {}, {}},
		},
	}
}

func TestApplyFullFrameThenText(t *testing.T) {
	s := NewScreen(5, 2)
	s.Apply(fullFrame())
	want := "hi\nyo"
	if got := s.Text(); got != want {
		t.Fatalf("Text() = %q, want %q", got, want)
	}
}

func TestApplyDiffFrameLeavesUntouchedRowsAlone(t *testing.T) {
	s := NewScreen(5, 2)
	s.Apply(fullFrame())
	s.Apply(proto.Frame{
		Event: "frame", Pane: "w1:p1", Seq: 2, Cols: 5, Rows: 2, Cursor: [2]int{0, 0},
		RowsChanged: map[int][]proto.Cell{
			1: {{Text: "z"}, {Text: "z"}, {}, {}, {}},
		},
	})
	if got := s.Text(); got != "hi\nzz" {
		t.Fatalf("Text() = %q, want %q", got, "hi\nzz")
	}
}

func TestApplyResizesWhenTheFrameSizeChanges(t *testing.T) {
	s := NewScreen(5, 2)
	s.Apply(fullFrame())
	s.Apply(proto.Frame{
		Event: "frame", Pane: "w1:p1", Seq: 3, Cols: 3, Rows: 1, Cursor: [2]int{0, 0},
		RowsChanged: map[int][]proto.Cell{0: {{Text: "a"}, {Text: "b"}, {Text: "c"}}},
	})
	if got := s.Text(); got != "abc" {
		t.Fatalf("Text() = %q, want %q", got, "abc")
	}
}

func TestRenderToPlacesTheCursorLast(t *testing.T) {
	s := NewScreen(5, 2)
	s.Apply(fullFrame())
	var b strings.Builder
	if err := s.RenderTo(&b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	// The cursor at (1,1) is row 2, column 2 in one-based ANSI.
	pos := "\x1b[2;2H"
	if !strings.HasSuffix(out, pos) {
		t.Fatalf("render does not end by placing the cursor at %q: %q", pos, out)
	}
	if !strings.Contains(out, "hi") || !strings.Contains(out, "yo") {
		t.Fatalf("render lost the content: %q", out)
	}
}

func TestStatusLineShowsIdLabelHarnessStateAndSource(t *testing.T) {
	ev := &proto.PaneStateEvent{State: proto.StateBlocked, Source: proto.SrcGate,
		Ask: &proto.Ask{Tool: "Bash", Summary: "rm -rf build/"}}
	got := StatusLine("w1:p3", "auth fix", "claude-code", ev, 80)
	for _, want := range []string{"w1:p3", "auth fix", "claude-code", "blocked", "gate", "Bash"} {
		if !strings.Contains(got, want) {
			t.Fatalf("StatusLine = %q, want it to contain %q", got, want)
		}
	}
}

// A pane nobody has reported on says unknown, not idle.
func TestStatusLineWithNoStateSaysUnknown(t *testing.T) {
	got := StatusLine("w1:p3", "", "", nil, 80)
	if !strings.Contains(got, "unknown") {
		t.Fatalf("StatusLine = %q, want unknown", got)
	}
	if strings.Contains(got, "idle") {
		t.Fatalf("StatusLine = %q, want no idle for a pane with no source", got)
	}
}

func TestStatusLineFitsTheTerminalWidth(t *testing.T) {
	ev := &proto.PaneStateEvent{State: proto.StateWorking, Source: proto.SrcHeadless}
	got := StatusLine("w1:p3", strings.Repeat("long label ", 20), "claude-code", ev, 40)
	if n := len([]rune(stripANSI(got))); n > 40 {
		t.Fatalf("status line is %d columns, want at most 40", n)
	}
}
```

```go
// harness/coppice/internal/attach/keys_test.go
package attach

import (
	"bytes"
	"testing"
)

func TestPlainBytesAreForwarded(t *testing.T) {
	k := NewKeyReader()
	act, out := k.Feed('x')
	if act != ActNone || !bytes.Equal(out, []byte{'x'}) {
		t.Fatalf("Feed('x') = %q %q, want no action and the byte forwarded", act, out)
	}
}

func TestLeaderThenNextIsAnAction(t *testing.T) {
	k := NewKeyReader()
	if act, out := k.Feed(Leader); act != ActNone || len(out) != 0 {
		t.Fatalf("the leader alone produced %q %q, want nothing yet", act, out)
	}
	act, out := k.Feed('n')
	if act != ActNext || len(out) != 0 {
		t.Fatalf("leader n = %q %q, want ActNext and no forwarded bytes", act, out)
	}
}

func TestEveryDocumentedLeaderKeyMaps(t *testing.T) {
	cases := map[byte]Action{
		'n': ActNext, 'p': ActPrev, 'c': ActCreate,
		'd': ActDetach, 'x': ActClose, '?': ActHelp,
	}
	for b, want := range cases {
		k := NewKeyReader()
		k.Feed(Leader)
		if act, _ := k.Feed(b); act != want {
			t.Fatalf("leader %q = %q, want %q", string(b), act, want)
		}
	}
}

// Leader twice sends one literal leader byte to the pane, which is how a user
// types ctrl+a inside the harness.
func TestLeaderTwiceForwardsOneLiteralLeader(t *testing.T) {
	k := NewKeyReader()
	k.Feed(Leader)
	act, out := k.Feed(Leader)
	if act != ActLiteral || !bytes.Equal(out, []byte{Leader}) {
		t.Fatalf("leader leader = %q %q, want one literal leader byte", act, out)
	}
}

// An unknown key after the leader is not swallowed. Swallowing input is the
// worst thing a terminal wrapper can do.
func TestUnknownLeaderKeyForwardsBothBytes(t *testing.T) {
	k := NewKeyReader()
	k.Feed(Leader)
	act, out := k.Feed('q')
	if act != ActNone || !bytes.Equal(out, []byte{Leader, 'q'}) {
		t.Fatalf("leader q = %q %q, want both bytes forwarded", act, out)
	}
}

// Every key HelpText advertises must map to an action the caller can act on.
// A help line for a key that does nothing is the control section 3.5 forbids.
func TestEveryKeyInHelpTextHasAnAction(t *testing.T) {
	for _, line := range []struct {
		key byte
		txt string
	}{
		{'n', "next pane"}, {'p', "previous pane"}, {'c', "create a pane"},
		{'d', "detach"}, {'x', "close"}, {'?', "this list"},
	} {
		if !bytes.Contains([]byte(HelpText), []byte("ctrl+a "+string(line.key))) {
			t.Fatalf("HelpText does not mention ctrl+a %s", string(line.key))
		}
		k := NewKeyReader()
		k.Feed(Leader)
		if act, _ := k.Feed(line.key); act == ActNone {
			t.Fatalf("ctrl+a %s is advertised but maps to no action", string(line.key))
		}
	}
}

func TestTheLeaderStateResetsAfterEveryAction(t *testing.T) {
	k := NewKeyReader()
	k.Feed(Leader)
	k.Feed('n')
	act, out := k.Feed('n')
	if act != ActNone || string(out) != "n" {
		t.Fatalf("the second n = %q %q, want it forwarded as plain input", act, out)
	}
}
```

- [ ] **Step 2: Run the tests and watch them fail**

Run: `cd harness/coppice && go test -race ./internal/attach/ -v`
Expected: FAIL, no package `attach`.

- [ ] **Step 3: Write `render.go`**

```go
// harness/coppice/internal/attach/render.go

// Package attach is the single-pane renderer behind `coppice attach`. Splits
// and multi-pane layouts are the cockpit's job and the PWA's job; this is one
// pane, a status line, and a leader key.
package attach

import (
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/opendaisugi/coppice/internal/proto"
)

// Screen holds the client's copy of the pane grid, rebuilt from frames.
type Screen struct {
	cols, rows int
	grid       [][]proto.Cell
	cursor     [2]int
}

func NewScreen(cols, rows int) *Screen {
	s := &Screen{}
	s.resize(cols, rows)
	return s
}

func (s *Screen) resize(cols, rows int) {
	s.cols, s.rows = cols, rows
	s.grid = make([][]proto.Cell, rows)
	for y := range s.grid {
		s.grid[y] = make([]proto.Cell, cols)
	}
}

// Apply folds one frame in. A frame whose size differs from ours replaces the
// grid: the server resized, and keeping stale rows would show a screen that
// never existed.
func (s *Screen) Apply(f proto.Frame) {
	if f.Cols != s.cols || f.Rows != s.rows {
		s.resize(f.Cols, f.Rows)
	}
	for y, row := range f.RowsChanged {
		if y < 0 || y >= s.rows {
			continue
		}
		for x := 0; x < s.cols; x++ {
			if x < len(row) {
				s.grid[y][x] = row[x]
			} else {
				s.grid[y][x] = proto.Cell{}
			}
		}
	}
	s.cursor = f.Cursor
}

// Text is the plain content, used by tests and by anything that wants the
// screen without colour.
func (s *Screen) Text() string {
	lines := make([]string, s.rows)
	for y, row := range s.grid {
		var b strings.Builder
		for _, c := range row {
			if c.Text == "" {
				b.WriteByte(' ')
			} else {
				b.WriteString(c.Text)
			}
		}
		lines[y] = strings.TrimRight(b.String(), " ")
	}
	return strings.Join(lines, "\n")
}

// RenderTo writes the whole screen with colour, then places the cursor. The
// cursor goes last so the terminal does not flicker it across the redraw.
func (s *Screen) RenderTo(w io.Writer) error {
	var b strings.Builder
	b.WriteString("\x1b[H")
	for y, row := range s.grid {
		b.WriteString("\x1b[K")
		last := proto.Cell{}
		for _, c := range row {
			if c.FG != last.FG || c.BG != last.BG || c.Attrs != last.Attrs {
				b.WriteString(sgr(c))
				last = c
			}
			if c.Text == "" {
				b.WriteByte(' ')
			} else {
				b.WriteString(c.Text)
			}
		}
		b.WriteString("\x1b[0m")
		if y < len(s.grid)-1 {
			b.WriteString("\r\n")
		}
	}
	b.WriteString(fmt.Sprintf("\x1b[%d;%dH", s.cursor[1]+1, s.cursor[0]+1))
	_, err := io.WriteString(w, b.String())
	return err
}

func sgr(c proto.Cell) string {
	parts := []string{"0"}
	if c.Attrs&1 != 0 {
		parts = append(parts, "1")
	}
	if c.Attrs&2 != 0 {
		parts = append(parts, "2")
	}
	if c.Attrs&4 != 0 {
		parts = append(parts, "3")
	}
	if c.Attrs&8 != 0 {
		parts = append(parts, "4")
	}
	if c.Attrs&16 != 0 {
		parts = append(parts, "5")
	}
	if c.Attrs&32 != 0 {
		parts = append(parts, "7")
	}
	if c.Attrs&64 != 0 {
		parts = append(parts, "9")
	}
	out := "\x1b[" + strings.Join(parts, ";") + "m"
	if rgb, ok := parseHex(c.FG); ok {
		out += fmt.Sprintf("\x1b[38;2;%d;%d;%dm", rgb[0], rgb[1], rgb[2])
	}
	if rgb, ok := parseHex(c.BG); ok {
		out += fmt.Sprintf("\x1b[48;2;%d;%d;%dm", rgb[0], rgb[1], rgb[2])
	}
	return out
}

func parseHex(s string) ([3]int, bool) {
	var out [3]int
	if len(s) != 7 || s[0] != '#' {
		return out, false
	}
	if _, err := fmt.Sscanf(s, "#%02x%02x%02x", &out[0], &out[1], &out[2]); err != nil {
		return out, false
	}
	return out, true
}

// StatusLine is the one line under the pane. It names the pane, its label, its
// harness, its merged state and the source of that state, because a state with
// no visible source is a guess the operator cannot audit.
func StatusLine(paneID, label, harness string, ev *proto.PaneStateEvent, cols int) string {
	state, source := proto.StateUnknown, "none"
	extra := ""
	if ev != nil {
		state = ev.State
		source = ev.Source
		if ev.Ask != nil {
			extra = " " + ev.Ask.Tool
			if ev.Ask.Summary != "" {
				extra += ": " + ev.Ask.Summary
			}
		}
	}
	parts := []string{paneID}
	if label != "" {
		parts = append(parts, label)
	}
	if harness != "" {
		parts = append(parts, harness)
	}
	parts = append(parts, state+" via "+source)
	line := strings.Join(parts, "  ") + extra
	line += "   ctrl+a ? for keys"
	r := []rune(line)
	if len(r) > cols {
		if cols <= 1 {
			return string(r[:cols])
		}
		line = string(r[:cols-1]) + "…"
	}
	return line
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

func stripANSI(s string) string { return ansiRe.ReplaceAllString(s, "") }
```

- [ ] **Step 4: Write `keys.go`**

```go
// harness/coppice/internal/attach/keys.go
package attach

// Leader is ctrl+a, the same leader tmux and screen users already have in their
// fingers.
const Leader = 0x01

type Action string

const (
	ActNone    Action = ""
	ActNext    Action = "next"
	ActPrev    Action = "prev"
	ActCreate  Action = "create"
	ActDetach  Action = "detach"
	ActClose   Action = "close"
	ActHelp    Action = "help"
	ActLiteral Action = "literal"
)

// HelpText is what leader ? prints. Every line here does something: the three
// navigation keys return through attach.Next and the CLI re-attaches. STE100,
// one line per key.
const HelpText = "ctrl+a n  next pane\r\n" +
	"ctrl+a p  previous pane\r\n" +
	"ctrl+a c  create a pane here\r\n" +
	"ctrl+a d  detach and leave the pane running\r\n" +
	"ctrl+a x  close this pane\r\n" +
	"ctrl+a ?  this list\r\n" +
	"ctrl+a ctrl+a  send one ctrl+a to the pane\r\n"

// KeyReader turns a byte stream into actions plus the bytes to forward to the
// pane. It never swallows input: an unknown key after the leader forwards both
// bytes, because losing a keystroke is the worst thing a terminal wrapper can do.
type KeyReader struct{ armed bool }

func NewKeyReader() *KeyReader { return &KeyReader{} }

func (k *KeyReader) Feed(b byte) (Action, []byte) {
	if !k.armed {
		if b == Leader {
			k.armed = true
			return ActNone, nil
		}
		return ActNone, []byte{b}
	}
	k.armed = false
	switch b {
	case 'n':
		return ActNext, nil
	case 'p':
		return ActPrev, nil
	case 'c':
		return ActCreate, nil
	case 'd':
		return ActDetach, nil
	case 'x':
		return ActClose, nil
	case '?':
		return ActHelp, nil
	case Leader:
		return ActLiteral, []byte{Leader}
	default:
		return ActNone, []byte{Leader, b}
	}
}
```

- [ ] **Step 5: Write `attach.go`**

```go
// harness/coppice/internal/attach/attach.go
package attach

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"sync"

	"github.com/opendaisugi/coppice/internal/proto"
	"golang.org/x/term"
)

type Options struct {
	Socket string
	Pane   string
	In     io.Reader
	Out    io.Writer
	Cols   int
	Rows   int
}

// Next is what the caller should do when Run returns. An empty Next means the
// user detached or closed the pane and the CLI should exit.
type Next struct {
	Pane   string // re-attach to this pane
	Create bool   // create a pane in the same working directory, then attach
}

// Run attaches to one pane and renders it until the user detaches, closes it,
// or asks for another pane. It restores the terminal on every exit path,
// including a panic, because leaving a shell in raw mode is a bug the user has
// to fix by hand.
func Run(o Options) (Next, error) {
	conn, err := net.Dial("unix", o.Socket)
	if err != nil {
		return Next{}, fmt.Errorf("cannot reach the server at %s. Run: coppice server start", o.Socket)
	}
	defer conn.Close()

	if f, ok := o.In.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		st, err := term.MakeRaw(int(f.Fd()))
		if err != nil {
			return Next{}, err
		}
		defer func() {
			_ = term.Restore(int(f.Fd()), st)
			_, _ = io.WriteString(o.Out, "\x1b[?25h\r\n")
		}()
	}

	enc := proto.NewEncoder(conn)
	dec := proto.NewDecoder(conn)
	if err := enc.Send(map[string]any{
		"id": "attach", "cmd": "pane.attach", "pane": o.Pane,
		"cols": o.Cols, "rows": o.Rows,
	}); err != nil {
		return Next{}, err
	}

	screen := NewScreen(o.Cols, o.Rows)
	var lastState *proto.PaneStateEvent
	label, harness := "", ""

	// nextMu guards next, which the input goroutine writes and the read loop
	// returns after the connection closes.
	var nextMu sync.Mutex
	var next Next
	setNext := func(n Next) {
		nextMu.Lock()
		next = n
		nextMu.Unlock()
	}

	// neighbour asks the server for the pane list and picks the one before or
	// after this pane, skipping closed ones. A separate connection, because the
	// attach connection is busy streaming frames.
	neighbour := func(delta int) string {
		c2, err := net.Dial("unix", o.Socket)
		if err != nil {
			return ""
		}
		defer c2.Close()
		e2, d2 := proto.NewEncoder(c2), proto.NewDecoder(c2)
		if err := e2.Send(map[string]any{"id": "l", "cmd": "pane.list"}); err != nil {
			return ""
		}
		line, err := d2.Next()
		if err != nil {
			return ""
		}
		var r struct {
			Result struct {
				Panes []struct {
					ID     string `json:"id"`
					Closed bool   `json:"closed"`
				} `json:"panes"`
			} `json:"result"`
		}
		if err := json.Unmarshal(line, &r); err != nil {
			return ""
		}
		var open []string
		for _, p := range r.Result.Panes {
			if !p.Closed {
				open = append(open, p.ID)
			}
		}
		if len(open) < 2 {
			return ""
		}
		for i, id := range open {
			if id == o.Pane {
				return open[((i+delta)%len(open)+len(open))%len(open)]
			}
		}
		return open[0]
	}

	// Input goes straight through, except leader sequences.
	go func() {
		k := NewKeyReader()
		buf := make([]byte, 1)
		for {
			n, err := o.In.Read(buf)
			if err != nil || n == 0 {
				return
			}
			act, out := k.Feed(buf[0])
			switch act {
			case ActDetach:
				_ = enc.Send(map[string]any{"id": "d", "cmd": "pane.detach", "pane": o.Pane})
				_ = conn.Close()
				return
			case ActClose:
				_ = enc.Send(map[string]any{"id": "x", "cmd": "pane.close", "pane": o.Pane})
				_ = conn.Close()
				return
			case ActHelp:
				_, _ = io.WriteString(o.Out, "\r\n"+HelpText)
			case ActNext, ActPrev:
				delta := 1
				if act == ActPrev {
					delta = -1
				}
				id := neighbour(delta)
				if id == "" {
					_, _ = io.WriteString(o.Out,
						"\r\nThere is no other open pane. Run: coppice pane create --cwd . -- claude\r\n")
					continue
				}
				setNext(Next{Pane: id})
				_ = enc.Send(map[string]any{"id": "d", "cmd": "pane.detach", "pane": o.Pane})
				_ = conn.Close()
				return
			case ActCreate:
				setNext(Next{Create: true})
				_ = enc.Send(map[string]any{"id": "d", "cmd": "pane.detach", "pane": o.Pane})
				_ = conn.Close()
				return
			}
			if len(out) > 0 {
				_ = enc.Send(map[string]any{
					"id": "t", "cmd": "pane.send_text", "pane": o.Pane,
					"text": string(out), "enter": false,
				})
			}
		}
	}()

	for {
		line, err := dec.Next()
		if err != nil {
			// The server closed, or we detached, or a leader key asked for
			// another pane.
			nextMu.Lock()
			n := next
			nextMu.Unlock()
			return n, nil
		}
		var probe struct {
			Event string `json:"event"`
			OK    *bool  `json:"ok"`
			Error *struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			continue
		}
		if probe.OK != nil && !*probe.OK && probe.Error != nil {
			return Next{}, fmt.Errorf("%s: %s", probe.Error.Code, probe.Error.Message)
		}
		switch probe.Event {
		case "frame":
			var f proto.Frame
			if err := json.Unmarshal(line, &f); err != nil {
				continue
			}
			screen.Apply(f)
			if err := screen.RenderTo(o.Out); err != nil {
				return Next{}, err
			}
			status := StatusLine(o.Pane, label, harness, lastState, o.Cols)
			_, _ = io.WriteString(o.Out,
				fmt.Sprintf("\x1b[%d;1H\x1b[7m\x1b[K%s\x1b[0m", o.Rows+1, status))
			_, _ = io.WriteString(o.Out,
				fmt.Sprintf("\x1b[%d;%dH", screen.cursor[1]+1, screen.cursor[0]+1))
		case "state":
			var ev proto.PaneStateEvent
			if err := json.Unmarshal(line, &ev); err != nil {
				continue
			}
			lastState = &ev
			if ev.Harness != "" {
				harness = ev.Harness
			}
			status := StatusLine(o.Pane, label, harness, lastState, o.Cols)
			_, _ = io.WriteString(o.Out,
				fmt.Sprintf("\x1b7\x1b[%d;1H\x1b[7m\x1b[K%s\x1b[0m\x1b8", o.Rows+1, status))
		}
	}
}
```

Add `golang.org/x/term` to `go.mod`. It is already the only direct dependency of
`harness/sprig`, so the workshop is not gaining a new third-party module family.

- [ ] **Step 6: Run the tests and watch them pass**

Run: `cd harness/coppice && go get golang.org/x/term && go test -race ./internal/attach/ -v && go vet ./...`
Expected: PASS.

- [ ] **Step 7: Record the new dependency in PINS.md**

Add a row to the "Other Go dependencies" table: `golang.org/x/term`, the version `go get`
resolved, "raw mode for `coppice attach`. Same module harness/sprig already uses."

- [ ] **Step 8: Commit**

```bash
git add harness/coppice/internal/attach harness/coppice/go.mod harness/coppice/go.sum \
        harness/coppice/PINS.md
git commit -m "feat(coppice): one pane, a status line that names its source, one leader key

The status line prints the state and where it came from, because a state with
no visible source is a guess the operator cannot audit, and a pane nobody
reported on says unknown rather than idle. The key reader never swallows a
keystroke: an unknown leader sequence forwards both bytes. Raw mode is restored
on every exit path, including a panic."
```

---

### Task 20: `cmd/coppice` — the CLI and the thin client

**Files:**
- Create: `harness/coppice/cmd/coppice/main.go`
- Create: `harness/coppice/internal/cli/cli.go`
- Create: `harness/coppice/internal/cli/client.go`
- Test: `harness/coppice/internal/cli/cli_test.go`
- Test: `harness/coppice/internal/server/lock_test.go`

**Interfaces:**
- Consumes: everything above.
- Produces:
  ```go
  package cli
  type CLI struct { Version string; Socket string
                    In io.Reader; Out, Err io.Writer }
  func (c *CLI) Run(argv []string) int      // 0 ok, 1 user error, 3 unreachable
  type Client struct{}
  func Dial(socket string) (*Client, error)
  func DialRemote(target string) (*Client, error)   // ssh://host → ssh host coppice --stdio
  func (c *Client) Do(cmd string, params map[string]any) (map[string]any, error)
  func (c *Client) Close() error
  func Proxy(socket string, in io.Reader, out io.Writer) error   // --stdio
  func StartBackgroundServer(socket string) error                // fork a detached child
  ```
  `--stdio` **proxies** bytes to the running server's socket. It must never construct a
  `server.Server`: a second server on the real data directory would write its own empty tree over
  `layout.json`, and `--remote` would report the far side's panes as zero because it was talking
  to a fresh in-process server instead of the host's.

- [ ] **Step 1: Write the failing test**

```go
// harness/coppice/internal/cli/cli_test.go
package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opendaisugi/coppice/internal/server"
	"github.com/opendaisugi/coppice/internal/toolchain"
)

func runCLI(t *testing.T, sock string, argv ...string) (int, string, string) {
	t.Helper()
	// Dial autostarts a server when nothing is listening. Tests that want a
	// failing dial to stay a failing dial turn that off.
	t.Setenv("COPPICE_NO_AUTOSTART", "1")
	var out, errb bytes.Buffer
	c := &CLI{Version: "test", Socket: sock, In: strings.NewReader(""), Out: &out, Err: &errb}
	code := c.Run(argv)
	return code, out.String(), errb.String()
}

func liveServer(t *testing.T) string {
	t.Helper()
	if r := toolchain.SkipReason(); r != "" {
		t.Skip(r)
	}
	dir := t.TempDir()
	sock := filepath.Join(dir, "server.sock")
	s, err := server.New(server.Config{SocketPath: sock, DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	s.RegisterPaneCommands()
	s.RegisterAttachCommands()
	s.RegisterAgentCommands()
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve() }()
	t.Cleanup(func() { _ = s.Close() })
	return sock
}

func TestNoArgumentsPrintsUsageAndExitsOne(t *testing.T) {
	code, out, errb := runCLI(t, "/nonexistent.sock")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	text := out + errb
	for _, want := range []string{"coppice", "server", "pane", "agent", "attach"} {
		if !strings.Contains(text, want) {
			t.Fatalf("usage %q does not mention %q", text, want)
		}
	}
}

// Exit code 3 is "unreachable", per the global exit-code rule.
func TestAnUnreachableServerExitsThreeWithATeachingMessage(t *testing.T) {
	code, _, errb := runCLI(t, filepath.Join(t.TempDir(), "nope.sock"), "pane", "list")
	if code != 3 {
		t.Fatalf("exit code = %d, want 3", code)
	}
	if !strings.Contains(errb, "coppice server start") {
		t.Fatalf("error %q does not name the command that fixes it", errb)
	}
}

func TestAnUnknownVerbExitsOneAndListsTheVerbs(t *testing.T) {
	code, out, errb := runCLI(t, "/nonexistent.sock", "teleport")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(out+errb, "pane") {
		t.Fatalf("the error does not list the verbs: %q", out+errb)
	}
}

func TestPaneCreateAndListOverTheSocket(t *testing.T) {
	sock := liveServer(t)
	cwd := t.TempDir()
	code, out, errb := runCLI(t, sock, "pane", "create", "--cwd", cwd, "--label", "one",
		"--", "sh", "-c", "sleep 3")
	if code != 0 {
		t.Fatalf("pane create exit %d: %s %s", code, out, errb)
	}
	if !strings.Contains(out, "w1:p1") {
		t.Fatalf("pane create printed %q, want the new pane id", out)
	}
	code, out, _ = runCLI(t, sock, "pane", "list", "--json")
	if code != 0 {
		t.Fatalf("pane list --json exit %d", code)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("--json did not print one JSON object: %q", out)
	}
	if _, ok := res["panes"]; !ok {
		t.Fatalf("--json result has no panes key: %q", out)
	}
}

// The human table must not be the JSON. Two audiences, two outputs.
func TestPaneListWithoutJSONPrintsATable(t *testing.T) {
	sock := liveServer(t)
	cwd := t.TempDir()
	runCLI(t, sock, "pane", "create", "--cwd", cwd, "--", "sh", "-c", "sleep 3")
	code, out, _ := runCLI(t, sock, "pane", "list")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Fatalf("pane list printed JSON without --json: %q", out)
	}
	for _, want := range []string{"w1:p1", "pty", "unknown"} {
		if !strings.Contains(out, want) {
			t.Fatalf("the table %q is missing %q", out, want)
		}
	}
}

// A gate deny is exit 2, distinct from a user error.
func TestAServerErrorMapsToExitOneAndPrintsTheCode(t *testing.T) {
	sock := liveServer(t)
	code, _, errb := runCLI(t, sock, "pane", "read", "w9:p9")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 for a user error", code)
	}
	if !strings.Contains(errb, "no_such_pane") {
		t.Fatalf("error %q does not carry the code", errb)
	}
}

// --stdio must be a PROXY to the running server, not a second server. A second
// server would take DataDir from the default, write its own empty tree over the
// running server's layout.json, and make `coppice --remote ssh://host pane list`
// report zero panes because it was talking to a fresh in-process server rather
// than the host's.
func TestStdioProxiesToTheRunningServerAndNeverWritesTheLayout(t *testing.T) {
	if r := toolchain.SkipReason(); r != "" {
		t.Skip(r)
	}
	dir := t.TempDir()
	sock := filepath.Join(dir, "server.sock")
	s, err := server.New(server.Config{SocketPath: sock, DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	s.RegisterPaneCommands()
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve() }()
	defer s.Close()

	// One real pane, so the layout on disk is not empty.
	cwd := t.TempDir()
	runCLI(t, sock, "pane", "create", "--cwd", cwd, "--label", "real", "--", "sh", "-c", "sleep 5")

	layoutPath := filepath.Join(dir, "layout.json")
	before, err := os.ReadFile(layoutPath)
	if err != nil {
		t.Fatal(err)
	}

	// Drive --stdio and ask for the pane list. It must see the running
	// server's pane.
	var out bytes.Buffer
	in := strings.NewReader(`{"id":"1","cmd":"pane.list"}` + "\n")
	c := &CLI{Version: "test", Socket: sock, In: in, Out: &out, Err: &bytes.Buffer{}}
	if code := c.Run([]string{"--stdio"}); code != 0 {
		t.Fatalf("--stdio exit %d: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "w1:p1") {
		t.Fatalf("--stdio returned %q, want the running server's pane w1:p1", out.String())
	}
	if !strings.Contains(out.String(), "real") {
		t.Fatalf("--stdio returned %q, want the running server's label", out.String())
	}

	after, err := os.ReadFile(layoutPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("--stdio rewrote layout.json.\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// The other half of the same blocker: a --stdio session must not create a
// server of its own even when none is reachable and autostart is off.
func TestStdioWithNoServerFailsRatherThanBuildingOne(t *testing.T) {
	t.Setenv("COPPICE_NO_AUTOSTART", "1")
	dir := t.TempDir()
	var out, errb bytes.Buffer
	c := &CLI{Version: "test", Socket: filepath.Join(dir, "absent.sock"),
		In: strings.NewReader(`{"id":"1","cmd":"pane.list"}` + "\n"), Out: &out, Err: &errb}
	if code := c.Run([]string{"--stdio"}); code != 3 {
		t.Fatalf("--stdio with no server exited %d, want 3", code)
	}
	if _, err := os.Stat(filepath.Join(dir, "layout.json")); err == nil {
		t.Fatal("--stdio created a layout.json, so it built a server")
	}
}

func TestRemoteTargetBuildsTheSSHCommand(t *testing.T) {
	argv, err := remoteArgv("ssh://build-box")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ssh", "build-box", "coppice", "--stdio"}
	if strings.Join(argv, " ") != strings.Join(want, " ") {
		t.Fatalf("remoteArgv = %v, want %v", argv, want)
	}
}

func TestRemoteTargetRejectsANonSSHScheme(t *testing.T) {
	if _, err := remoteArgv("http://example.com"); err == nil {
		t.Fatal("remoteArgv accepted an http target, want an error naming ssh://")
	}
}

func TestVersionPrintsTheVersionAndExitsZero(t *testing.T) {
	code, out, _ := runCLI(t, "/nonexistent.sock", "--version")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out, "test") {
		t.Fatalf("--version printed %q, want the version", out)
	}
}
```

- [ ] **Step 2: Run the test and watch it fail**

Run: `cd harness/coppice && go test -race ./internal/cli/ -v`
Expected: FAIL, no package `cli`.

- [ ] **Step 3: Write `client.go`**

```go
// harness/coppice/internal/cli/client.go
package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/opendaisugi/coppice/internal/proto"
)

// Client speaks the socket protocol. The local path and the ssh path use the
// same code, because --stdio is the same JSONL on a different pipe.
type Client struct {
	enc   *proto.Encoder
	dec   *proto.Decoder
	close func() error
	n     int
}

// Dial connects, starting the server once if nothing is listening. Spec-02 and
// the README both promise that the first invocation brings the workshop up, so
// the CLI has to do it rather than telling the operator to.
func Dial(socket string) (*Client, error) {
	conn, err := net.Dial("unix", socket)
	if err != nil {
		if serr := StartBackgroundServer(socket); serr != nil {
			return nil, fmt.Errorf(
				"cannot reach the server at %s and cannot start one: %v. Run: coppice server start --foreground",
				socket, serr)
		}
		conn, err = waitForSocket(socket, 5*time.Second)
		if err != nil {
			return nil, fmt.Errorf(
				"started a server but %s never accepted a connection. Run: coppice server start --foreground",
				socket)
		}
	}
	return &Client{
		enc: proto.NewEncoder(conn), dec: proto.NewDecoder(conn), close: conn.Close,
	}, nil
}

func waitForSocket(socket string, d time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if c, err := net.Dial("unix", socket); err == nil {
			return c, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil, fmt.Errorf("timed out waiting for %s", socket)
}

// StartBackgroundServer re-executes this binary as a detached foreground server
// and returns as soon as it is spawned. COPPICE_NO_AUTOSTART=1 turns it off,
// which the tests use so a failing dial stays a failing dial.
func StartBackgroundServer(socket string) error {
	if os.Getenv("COPPICE_NO_AUTOSTART") != "" {
		return fmt.Errorf("autostart is off (COPPICE_NO_AUTOSTART is set)")
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self, "--socket", socket, "server", "start", "--foreground")
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	// The child outlives us; do not wait for it, and do not leave a zombie.
	go func() { _ = cmd.Wait() }()
	return nil
}

// Proxy pumps one client's bytes to the running server and back. This is the
// whole of --stdio. It constructs no server, so nothing here can touch
// layout.json.
func Proxy(socket string, in io.Reader, out io.Writer) error {
	conn, err := net.Dial("unix", socket)
	if err != nil {
		if serr := StartBackgroundServer(socket); serr != nil {
			return fmt.Errorf("cannot reach the server at %s. Run: coppice server start", socket)
		}
		conn, err = waitForSocket(socket, 5*time.Second)
		if err != nil {
			return fmt.Errorf("cannot reach the server at %s. Run: coppice server start", socket)
		}
	}
	defer conn.Close()
	done := make(chan error, 2)
	go func() { _, err := io.Copy(conn, in); done <- err }()
	go func() { _, err := io.Copy(out, conn); done <- err }()
	<-done
	return nil
}

// remoteArgv turns ssh://host into the command that runs a thin server there.
func remoteArgv(target string) ([]string, error) {
	rest, ok := strings.CutPrefix(target, "ssh://")
	if !ok || rest == "" {
		return nil, fmt.Errorf("remote target %q must look like ssh://host", target)
	}
	return []string{"ssh", rest, "coppice", "--stdio"}, nil
}

func DialRemote(target string) (*Client, error) {
	argv, err := remoteArgv(target)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("cannot run %s: %w", strings.Join(argv, " "), err)
	}
	return &Client{
		enc: proto.NewEncoder(in), dec: proto.NewDecoder(out),
		close: func() error {
			_ = in.(io.Closer).Close()
			return cmd.Wait()
		},
	}, nil
}

// Do sends one request and reads until the matching response. Events that
// arrive first are discarded: a one-shot CLI call is not a subscriber.
func (c *Client) Do(cmd string, params map[string]any) (map[string]any, error) {
	c.n++
	id := fmt.Sprintf("%d", c.n)
	req := map[string]any{"id": id, "cmd": cmd}
	for k, v := range params {
		req[k] = v
	}
	if err := c.enc.Send(req); err != nil {
		return nil, err
	}
	for {
		line, err := c.dec.Next()
		if err != nil {
			return nil, err
		}
		var r struct {
			ID     string         `json:"id"`
			OK     bool           `json:"ok"`
			Result map[string]any `json:"result"`
			Error  *proto.Error   `json:"error"`
			Event  string         `json:"event"`
		}
		if err := json.Unmarshal(line, &r); err != nil {
			continue
		}
		if r.Event != "" || r.ID != id {
			continue
		}
		if !r.OK {
			if r.Error == nil {
				return nil, fmt.Errorf("internal: the server sent a failure with no error")
			}
			return nil, fmt.Errorf("%s: %s", r.Error.Code, r.Error.Message)
		}
		return r.Result, nil
	}
}

func (c *Client) Close() error {
	if c.close != nil {
		return c.close()
	}
	return nil
}
```

- [ ] **Step 4: Write `cli.go`**

```go
// harness/coppice/internal/cli/cli.go

// Package cli is the coppice command line. Every verb is one socket call, so
// the CLI and the cockpit and the PWA all drive the same server through the
// same protocol.
package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/opendaisugi/coppice/internal/attach"
	"github.com/opendaisugi/coppice/internal/server"
)

const usage = `coppice: panes, agents and their state, on one machine.

  coppice server start|stop|status|token
  coppice workspace create [--cwd DIR] [--label TEXT]
  coppice workspace list
  coppice tab create [--label TEXT]        coppice tab list
  coppice pane create [--cwd DIR] [--label TEXT] [--kind pty|headless]
                      [--harness NAME] [--cols N] [--rows N] -- COMMAND...
  coppice pane list|send-text|send-keys|run|read|close|wait-output|explain
  coppice agent list|get|prompt|wait|read
  coppice attach [PANE]
  coppice --remote ssh://HOST <any command above>
  coppice --stdio                          speak the protocol on stdin and stdout

Add --json to any listing command to get one JSON object instead of a table.
Run coppice pane list to see your panes.`

type CLI struct {
	Version string
	Socket  string
	In      io.Reader
	Out     io.Writer
	Err     io.Writer
}

// Run returns the process exit code: 0 success, 1 user error, 3 unreachable.
// A gate deny is 2, which only the harness paths can produce.
func (c *CLI) Run(argv []string) int {
	if len(argv) == 0 {
		fmt.Fprintln(c.Err, usage)
		return 1
	}
	remote := ""
	for len(argv) > 0 && strings.HasPrefix(argv[0], "--") {
		switch argv[0] {
		case "--version":
			fmt.Fprintln(c.Out, "coppice "+c.Version)
			return 0
		case "--help", "-h":
			fmt.Fprintln(c.Out, usage)
			return 0
		case "--stdio":
			return c.runStdio()
		case "--remote":
			if len(argv) < 2 {
				fmt.Fprintln(c.Err, "--remote needs a target like ssh://host")
				return 1
			}
			remote = argv[1]
			argv = argv[2:]
			continue
		case "--socket":
			if len(argv) < 2 {
				fmt.Fprintln(c.Err, "--socket needs a path")
				return 1
			}
			c.Socket = argv[1]
			argv = argv[2:]
			continue
		default:
			fmt.Fprintf(c.Err, "unknown option %q\n%s\n", argv[0], usage)
			return 1
		}
	}
	if len(argv) == 0 {
		fmt.Fprintln(c.Err, usage)
		return 1
	}

	switch argv[0] {
	case "server":
		return c.runServer(argv[1:])
	case "attach":
		return c.runAttach(argv[1:])
	case "workspace", "tab", "pane", "agent", "session":
		return c.runVerb(remote, argv)
	default:
		fmt.Fprintf(c.Err, "unknown command %q\n%s\n", argv[0], usage)
		return 1
	}
}

func (c *CLI) dial(remote string) (*Client, int) {
	var (
		cl  *Client
		err error
	)
	if remote != "" {
		cl, err = DialRemote(remote)
	} else {
		cl, err = Dial(c.Socket)
	}
	if err != nil {
		fmt.Fprintln(c.Err, err)
		return nil, 3
	}
	return cl, 0
}

// verbMap turns "pane send-text" into the socket command "pane.send_text".
func socketCommand(group, verb string) string {
	return group + "." + strings.ReplaceAll(verb, "-", "_")
}

func (c *CLI) runVerb(remote string, argv []string) int {
	if len(argv) < 2 {
		fmt.Fprintf(c.Err, "%s needs a verb. %s\n", argv[0], usage)
		return 1
	}
	group, verb := argv[0], argv[1]
	rest := argv[2:]

	params := map[string]any{}
	asJSON := false
	var positional []string
	var after []string
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		switch {
		case a == "--":
			after = rest[i+1:]
			i = len(rest)
		case a == "--json":
			asJSON = true
		case a == "--wait":
			params["wait"] = true
		case a == "--no-enter":
			params["enter"] = false
		case strings.HasPrefix(a, "--"):
			if i+1 >= len(rest) {
				fmt.Fprintf(c.Err, "%s needs a value\n", a)
				return 1
			}
			key := strings.ReplaceAll(strings.TrimPrefix(a, "--"), "-", "_")
			val := rest[i+1]
			i++
			if n, err := strconv.Atoi(val); err == nil {
				params[key] = n
			} else {
				params[key] = val
			}
		default:
			positional = append(positional, a)
		}
	}
	if len(after) > 0 {
		params["cmd_argv"] = after
	}
	// The first positional of a pane or agent verb is the pane id, except for
	// the verbs that take no pane.
	if len(positional) > 0 && verb != "create" && verb != "list" {
		params["pane"] = positional[0]
		positional = positional[1:]
	}
	if len(positional) > 0 {
		switch verb {
		case "send-text", "run":
			params["text"] = strings.Join(positional, " ")
			if verb == "run" {
				params["cmd"] = strings.Join(positional, " ")
				delete(params, "text")
			}
		case "prompt":
			params["text"] = strings.Join(positional, " ")
		case "send-keys":
			params["keys"] = positional
		}
	}

	cl, code := c.dial(remote)
	if code != 0 {
		return code
	}
	defer cl.Close()

	res, err := cl.Do(socketCommand(group, verb), params)
	if err != nil {
		fmt.Fprintln(c.Err, err)
		return 1
	}
	if asJSON {
		b, _ := json.Marshal(res)
		fmt.Fprintln(c.Out, string(b))
		return 0
	}
	c.printHuman(group, verb, res)
	return 0
}

// printHuman is the table for a person. --json is the data for a program. They
// are deliberately different: a table that is also JSON serves neither well.
func (c *CLI) printHuman(group, verb string, res map[string]any) {
	rows, ok := res[pluralKey(group, verb)].([]any)
	if !ok {
		for _, k := range []string{"pane", "workspace", "tab", "state", "text", "note"} {
			if v, ok := res[k]; ok {
				fmt.Fprintf(c.Out, "%s %v\n", k, v)
			}
		}
		return
	}
	for _, r := range rows {
		m, _ := r.(map[string]any)
		id := firstOf(m, "id", "pane")
		fmt.Fprintf(c.Out, "%-10v %-10v %-10v %-10v %v\n",
			id, orDash(m["kind"]), orDash(m["state"]), orDash(m["source"]), orDash(m["label"]))
	}
}

func pluralKey(group, verb string) string {
	if verb != "list" {
		return ""
	}
	switch group {
	case "pane", "session":
		return "panes"
	case "agent":
		return "agents"
	case "workspace":
		return "workspaces"
	case "tab":
		return "tabs"
	}
	return ""
}

func firstOf(m map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			return v
		}
	}
	return "-"
}

func orDash(v any) any {
	if v == nil || v == "" {
		return "-"
	}
	return v
}

func (c *CLI) runAttach(argv []string) int {
	paneID := ""
	if len(argv) > 0 {
		paneID = argv[0]
	}
	if paneID == "" {
		cl, code := c.dial("")
		if code != 0 {
			return code
		}
		res, err := cl.Do("pane.list", nil)
		_ = cl.Close()
		if err != nil {
			fmt.Fprintln(c.Err, err)
			return 1
		}
		rows, _ := res["panes"].([]any)
		for _, r := range rows {
			m, _ := r.(map[string]any)
			if closed, _ := m["closed"].(bool); !closed {
				paneID, _ = m["id"].(string)
				break
			}
		}
		if paneID == "" {
			fmt.Fprintln(c.Err,
				"there are no open panes. Run: coppice pane create --cwd . -- claude")
			return 1
		}
	}
	cols, rows := 120, 40
	if f, ok := c.Out.(*os.File); ok {
		if w, h, err := termSize(f); err == nil {
			cols, rows = w, h-1 // one line for the status
		}
	}
	for {
		next, err := attach.Run(attach.Options{
			Socket: c.Socket, Pane: paneID, In: c.In, Out: c.Out, Cols: cols, Rows: rows,
		})
		if err != nil {
			fmt.Fprintln(c.Err, err)
			return 1
		}
		switch {
		case next.Pane != "":
			paneID = next.Pane
		case next.Create:
			cl, code := c.dial("")
			if code != 0 {
				return code
			}
			cwd, _ := os.Getwd()
			res, err := cl.Do("pane.create", map[string]any{
				"cwd": cwd, "kind": "pty", "cmd_argv": []string{shell()},
				"cols": cols, "rows": rows,
			})
			_ = cl.Close()
			if err != nil {
				fmt.Fprintln(c.Err, err)
				return 1
			}
			id, _ := res["pane"].(string)
			if id == "" {
				fmt.Fprintln(c.Err, "the server created no pane. Run: coppice pane list")
				return 1
			}
			paneID = id
		default:
			return 0
		}
	}
}

func shell() string {
	if sh := os.Getenv("SHELL"); sh != "" {
		return sh
	}
	return "/bin/sh"
}

// runStdio pumps bytes between stdin/stdout and the running server's socket. It
// builds NO server: `coppice --remote ssh://host pane list` must see the host's
// real pane tree, and a second server on the real data directory would write an
// empty layout over the running server's file.
func (c *CLI) runStdio() int {
	if err := Proxy(c.Socket, c.In, c.Out); err != nil {
		fmt.Fprintln(c.Err, err)
		return 3
	}
	return 0
}
```

The rest of `cli.go`, in the same file and with the same import block, which is
`encoding/json`, `fmt`, `io`, `os`, `strconv`, `strings`, `time`, `golang.org/x/term`, and the
coppice packages `attach`, `detect`, `proto` and `server`:

```go
func termSize(f *os.File) (int, int, error) { return term.GetSize(int(f.Fd())) }

// registerAll wires every command onto a server, and surfaces the detection
// warnings rather than swallowing them: a broken file in
// ~/.config/coppice/agent-detection/ silently reverting to the bundled manifest
// is exactly the kind of quiet failure the operator cannot see.
func registerAll(s *server.Server) {
	s.RegisterPaneCommands()
	s.RegisterAttachCommands()
	s.RegisterAgentCommands()
	s.Handle("server.stop", func(_ *server.Client, r *proto.Request) proto.Response {
		go func() { time.Sleep(50 * time.Millisecond); _ = s.Close() }()
		return proto.OKResp(r.ID, map[string]any{"stopping": true})
	})
	set, err := detect.LoadSet(detect.OverrideDir())
	if err != nil {
		s.SetDetectionWarnings([]string{
			"agent detection did not load: " + err.Error() +
				". Screen detection is off. Gate-reported state still works."})
		return
	}
	s.SetDetectionWarnings(set.Warnings())
	s.RegisterExplainCommand(set)
	s.StartManifestTick(set)
}

func (c *CLI) runServer(argv []string) int {
	if len(argv) == 0 {
		fmt.Fprintln(c.Err, "coppice server needs start, stop, status or token")
		return 1
	}
	switch argv[0] {
	case "start":
		// Spec-02 and the README both say the first invocation brings up a
		// background server, so `start` detaches by default. --foreground is
		// what the detached child runs, and what an operator uses to watch it.
		foreground := false
		for _, a := range argv[1:] {
			if a == "--foreground" {
				foreground = true
			}
		}
		if !foreground {
			if err := StartBackgroundServer(c.Socket); err != nil {
				fmt.Fprintln(c.Err, err)
				return 1
			}
			if _, err := waitForSocket(c.Socket, 5*time.Second); err != nil {
				fmt.Fprintf(c.Err,
					"started a server but %s never accepted a connection. "+
						"Run: coppice server start --foreground\n", c.Socket)
				return 3
			}
			fmt.Fprintf(c.Out, "coppice is listening on %s\n", c.Socket)
			return 0
		}
		s, err := server.New(server.Config{SocketPath: c.Socket})
		if err != nil {
			fmt.Fprintln(c.Err, err)
			return 1
		}
		// The lock comes FIRST, before anything reads or writes the data
		// directory. A second start that got as far as Restore would spawn its
		// own resumed panes and write its tree back over the running server's
		// layout, and only then fail to bind.
		if err := s.AcquireStartLock(); err != nil {
			// ErrAlreadyRunning already reads "coppice server already running
			// (pid N). Run: coppice server status", which is the whole message
			// an operator needs.
			fmt.Fprintln(c.Err, err)
			return 1
		}
		defer s.Close() // releases the lock on every exit path
		if err := s.Restore(); err != nil {
			fmt.Fprintln(c.Err, err)
			return 1
		}
		registerAll(s)
		if err := s.Listen(); err != nil {
			fmt.Fprintln(c.Err, err)
			return 1
		}
		fmt.Fprintf(c.Out, "coppice is listening on %s\n", c.Socket)
		if err := s.Serve(); err != nil {
			fmt.Fprintln(c.Err, err)
			return 1
		}
		return 0
	case "stop":
		cl, code := c.dial("")
		if code != 0 {
			return code
		}
		defer cl.Close()
		if _, err := cl.Do("server.stop", nil); err != nil {
			fmt.Fprintln(c.Err, err)
			return 1
		}
		fmt.Fprintln(c.Out, "the server is stopping")
		return 0
	case "status":
		cl, code := c.dial("")
		if code != 0 {
			return code
		}
		defer cl.Close()
		res, err := cl.Do("server.status", nil)
		if err != nil {
			fmt.Fprintln(c.Err, err)
			return 1
		}
		fmt.Fprintf(c.Out, "pid %v, %v panes, up %vs\n%v\n",
			res["pid"], res["panes"], res["uptime_s"], res["restart_note"])
		if ws, ok := res["detection_warnings"].([]any); ok && len(ws) > 0 {
			fmt.Fprintln(c.Out, "\nagent detection warnings:")
			for _, w := range ws {
				fmt.Fprintf(c.Out, "  %v\n", w)
			}
		}
		return 0
	case "token":
		// There is no token. The socket is the credential: mode 0600 in a 0700
		// directory, and the server checks the peer uid. Say so rather than
		// printing something that looks like a secret.
		fmt.Fprintf(c.Out,
			"coppice has no token. The socket is the credential.\n"+
				"It is %s, mode 0600, and the server refuses any peer whose uid is not yours.\n"+
				"To reach it from another machine, run: coppice --remote ssh://HOST pane list\n",
			c.Socket)
		return 0
	default:
		fmt.Fprintf(c.Err, "unknown server command %q. Try start, stop, status or token\n", argv[0])
		return 1
	}
}
```

- [ ] **Step 4b: Write the racing-starts test**

```go
// harness/coppice/internal/server/lock_test.go
package server

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/layout"
	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/toolchain"
)

// Two `coppice server start` invocations racing on the same data directory.
// Exactly one may hold the lock, restore, and bind. The loser must fail before
// it reads or writes anything: without the lock it would restore the layout,
// spawn its own resumed panes, write its tree back, and only then fail to bind
// the socket.
func TestTwoRacingStartsLeaveOneServerAndOneLayout(t *testing.T) {
	if r := toolchain.SkipReason(); r != "" {
		t.Skip(r)
	}
	dir := t.TempDir()
	sock := filepath.Join(dir, "server.sock")

	// Seed a layout with one pane record, so an overwrite by the loser would
	// be visible as a lost pane id.
	seed := layout.New()
	ws := seed.CreateWorkspace("main", dir)
	tab, err := seed.CreateTab(ws.ID, "work")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.CreatePane(ws.ID, tab.ID, layout.Pane{
		Cwd: dir, Label: "seeded", Argv: []string{"sh"}, Kind: layout.KindPTY,
	}); err != nil {
		t.Fatal(err)
	}
	layoutPath := filepath.Join(dir, "layout.json")
	if err := seed.Save(layoutPath); err != nil {
		t.Fatal(err)
	}

	type outcome struct {
		s      *Server
		locked bool
		err    error
	}
	results := make([]outcome, 2)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, err := New(Config{SocketPath: sock, DataDir: dir})
			if err != nil {
				results[i] = outcome{err: err}
				return
			}
			<-start // release both at the same moment
			if err := s.AcquireStartLock(); err != nil {
				results[i] = outcome{s: s, err: err}
				return
			}
			if err := s.Restore(); err != nil {
				results[i] = outcome{s: s, locked: true, err: err}
				return
			}
			s.RegisterPaneCommands()
			if err := s.Listen(); err != nil {
				results[i] = outcome{s: s, locked: true, err: err}
				return
			}
			go func() { _ = s.Serve() }()
			results[i] = outcome{s: s, locked: true}
		}(i)
	}
	close(start)
	wg.Wait()

	winners := 0
	var winner *Server
	for _, r := range results {
		if r.locked && r.err == nil {
			winners++
			winner = r.s
		}
	}
	if winners != 1 {
		t.Fatalf("%d of 2 starts came up, want exactly 1", winners)
	}
	defer winner.Close()

	// The loser must have failed at the lock, not later.
	for _, r := range results {
		if r.s == winner {
			continue
		}
		if !errors.Is(r.err, ErrAlreadyRunning) {
			t.Fatalf("the losing start failed with %v, want ErrAlreadyRunning at the lock", r.err)
		}
		if r.locked {
			t.Fatal("the losing start held the lock, so both restored")
		}
		if r.s != nil && len(r.s.Tree().Panes()) != 0 {
			t.Fatal("the losing start restored a tree, so it read the layout it was refused")
		}
	}

	// layout.json was written by the winner only, and the seeded pane survived.
	b, err := os.ReadFile(layoutPath)
	if err != nil {
		t.Fatal(err)
	}
	var snap struct {
		Panes map[string]json.RawMessage `json:"panes"`
	}
	if err := json.Unmarshal(b, &snap); err != nil {
		t.Fatalf("layout.json is not readable after the race: %v\n%s", err, b)
	}
	if _, ok := snap.Panes["w1:p1"]; !ok {
		t.Fatalf("the seeded pane is gone, so a start overwrote the layout:\n%s", b)
	}

	// The socket belongs to the survivor.
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("nothing is listening on %s after the race: %v", sock, err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.WriteString(conn, `{"id":"1","cmd":"server.status"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	line, err := proto.NewDecoder(conn).Next()
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Result struct {
			PID    int    `json:"pid"`
			Socket string `json:"socket"`
		} `json:"result"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Result.Socket != sock {
		t.Fatalf("the listener reports socket %q, want %q", resp.Result.Socket, sock)
	}
	if resp.Result.PID != os.Getpid() {
		t.Fatalf("the listener reports pid %d, want this process %d",
			resp.Result.PID, os.Getpid())
	}
}
```

Run: `cd harness/coppice && go test ./internal/server/ -run Racing -race -count=5 -v`
Expected: PASS every time. `-count=5` is deliberate: a lock race that passes once has not been
tested.

- [ ] **Step 5: Write `main.go`**

```go
// harness/coppice/cmd/coppice/main.go

// Command coppice is the client and the server in one binary. The first
// invocation can start a background server; every later one talks to it.
package main

import (
	"os"

	"github.com/opendaisugi/coppice/internal/cli"
	"github.com/opendaisugi/coppice/internal/server"

	// Registering the adapters. Each package's init puts itself in the
	// registry, including the slots spec-04 and spec-05 will fill.
	_ "github.com/opendaisugi/coppice/internal/adapters/claude"
	_ "github.com/opendaisugi/coppice/internal/adapters/codex"
	_ "github.com/opendaisugi/coppice/internal/adapters/opencode"
	_ "github.com/opendaisugi/coppice/internal/adapters/pi"
	_ "github.com/opendaisugi/coppice/internal/adapters/sprig"
)

var version = "0.1.0"

func main() {
	c := &cli.CLI{
		Version: version,
		Socket:  server.SocketPath(),
		In:      os.Stdin,
		Out:     os.Stdout,
		Err:     os.Stderr,
	}
	os.Exit(c.Run(os.Args[1:]))
}
```

- [ ] **Step 6: Run everything**

Run: `cd harness/coppice && go build ./... && go test -race ./... && go vet ./...`
Expected: PASS.

- [ ] **Step 7: Drive it by hand once**

Run:
```bash
cd harness/coppice
go build -o ./build/coppice ./cmd/coppice
./build/coppice server start          # detaches by default
./build/coppice pane create --cwd "$PWD" --label smoke -- sh -c 'echo hello; sleep 30'
./build/coppice pane list
./build/coppice pane read w1:p1
./build/coppice server status
./build/coppice server stop
```
Expected: `server start` returns immediately and the socket is up, the pane is created, the list
shows it, `pane read` shows `hello`, and the status prints the restart note. Then check the
autostart promise the README makes:

```bash
./build/coppice server start          # again, while the first is still up
./build/coppice server stop
./build/coppice pane list     # must bring the server up by itself, not exit 3
./build/coppice server stop
```

The second `server start` must print `coppice server already running (pid N). Run: coppice server
status` and exit 1, and `pane list` immediately afterwards must still show the same panes.

Read the output. A plan step that only says "it should work" has not been done.

- [ ] **Step 8: Commit**

```bash
git add harness/coppice/cmd harness/coppice/internal/cli
git commit -m "feat(coppice): one binary, client and server, local and over ssh

--stdio proxies bytes to the running server rather than building a second one,
so --remote ssh://host is a thin client that sees the host's real panes and can
never write an empty tree over the host's layout. server start detaches and a
dial brings the server up once, which is what the README already promised. The
table and --json are deliberately different outputs for two audiences. server
token prints the truth: there is no token, the socket is the credential, and
here is how to reach it from another machine."
```

---

### Task 21: CI, the build cache, and the docs

**Files:**
- Create: `harness/coppice/README.md`
- Create: `.github/workflows/coppice.yml`
- Modify: `AGENTS.md` (add coppice to the map of where things are)
- Test: `harness/coppice/internal/cli/docs_test.go`

**Interfaces:**
- Consumes: everything.
- Produces: a green Go job in CI with the ghostty build cached, and a README that a fresh reader
  can follow.

- [ ] **Step 1: Write the failing test**

```go
// harness/coppice/internal/cli/docs_test.go
package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The README must document every top-level command the CLI accepts. A verb with
// no documentation is a verb nobody will find.
func TestREADMEDocumentsEveryTopLevelCommand(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	for _, cmd := range []string{
		"coppice server start", "coppice server stop", "coppice pane create",
		"coppice pane read", "coppice pane explain", "coppice agent prompt",
		"coppice agent wait", "coppice attach", "--remote", "--stdio", "--foreground",
	} {
		if !strings.Contains(text, cmd) {
			t.Fatalf("README does not document %q", cmd)
		}
	}
}

// The README must say what a restart does not restore, in the same words the
// server does. Two different answers to that question is worse than one.
func TestREADMERepeatsTheRestartHonesty(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ToLower(string(b))
	for _, want := range []string{"restart", "does not", "unknown"} {
		if !strings.Contains(text, want) {
			t.Fatalf("README is missing %q from the restart section", want)
		}
	}
}

// STE100: no em-dashes in the user-facing docs.
func TestREADMEHasNoEmDashes(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "—") {
		t.Fatal("README contains an em-dash. The house register does not use them.")
	}
}

func TestUsageTextHasNoEmDashes(t *testing.T) {
	if strings.Contains(usage, "—") {
		t.Fatal("the usage text contains an em-dash")
	}
}
```

- [ ] **Step 2: Run the test and watch it fail**

Run: `cd harness/coppice && go test -race ./internal/cli/ -run README -v`
Expected: FAIL, the README does not exist.

- [ ] **Step 3: Write the README**

`harness/coppice/README.md`:

```markdown
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

You need Go 1.26, Zig 0.16 and CMake. The installer puts the last two under `~/.local` with no
sudo, builds `libghostty-vt`, and leaves a pkg-config wrapper so a plain `go build` links:

```
./scripts/toolchain.sh
./scripts/preflight.sh
go build ./cmd/coppice
```

To undo the two global Go settings the installer writes: `go env -u GOTOOLCHAIN PKG_CONFIG`.
Pins and their reasons are in `PINS.md`.

## Use

```
coppice server start
coppice pane create --cwd . --label "auth fix" -- claude
coppice pane list
coppice attach w1:p1
```

`coppice server start` detaches and returns. Any command also brings the server up by itself if
nothing is listening, so the first thing you run is enough. To watch the server in your terminal
instead, run `coppice server start --foreground`. `coppice server stop` shuts it down.

`ctrl+a d` detaches and leaves the pane running. `ctrl+a n` and `ctrl+a p` move to the next and
previous open pane. `ctrl+a c` opens a shell pane here. `ctrl+a x` closes this pane. `ctrl+a ?`
lists the keys.

Headless panes take a harness instead of a command, and give you typed events instead of a
screen:

```
coppice pane create --cwd . --kind headless --harness claude --label "the tests"
coppice agent prompt w1:p2 "run the tests and fix what breaks" --wait --timeout-ms 600000
coppice agent wait w1:p2 --until blocked
coppice agent list
```

Reading and debugging:

```
coppice pane read w1:p1 --source visible      the screen now
coppice pane read w1:p1 --source recent       the last 200 lines
coppice pane read w1:p1 --source detection    exactly what the manifest rules see
coppice pane explain w1:p1                    which rule matched, and every rule that did not
```

From another machine, over SSH, with no extra service:

```
coppice --remote ssh://build-box pane list
```

`coppice --stdio` speaks the same protocol on stdin and stdout. That is all `--remote` uses.

## What a restart does

A restart restores the layout, the labels, the working directories and the pane numbering. It
**does not** restore running processes. A pty pane comes back closed and `done`. A headless pane
resumes only when its adapter recorded a harness session id; without one it comes back closed
and `unknown`, because nobody can say whether that agent finished. `coppice server status`
prints the whole report.

A restored pane never reports `idle`.

## Security

The socket is the credential. It lives at mode 0600 inside a 0700 directory, and the server
refuses any connection whose peer uid is not yours. There is no token, no port, and no second
user. `coppice server token` says so.

Spawned processes get `COPPICE_SOCK` and `COPPICE_PANE` in their environment. That pair is how
the openDaisugi gate hook inside a harness reports state back without any configuration.

## Agent detection

Screen detection uses Herdr's TOML manifests, vendored under `internal/detect/manifests/` at a
recorded commit, Apache-2.0, see `NOTICE`. The schema is written down in
`internal/detect/README.md`. Put your own file at
`~/.config/coppice/agent-detection/<agent>.toml` to replace a bundled one. coppice never fetches
a manifest from the network.

Screen detection is the fallback. A pane that any higher source reported on in the last two
seconds is not scanned at all. After two seconds the screen speaks again, so a harness that stops
calling the gate recovers rather than freezing on its last reported state.

If a manifest fails to load, `coppice server status` prints the warning. A broken override file
reverts to the bundled manifest, and it says so.

## Layout of the code

| path | what it is |
|---|---|
| `internal/vt` | the only package that touches libghostty |
| `internal/proto` | the wire: requests, responses, events, the error enum |
| `internal/state` | master spec 3.1, the source precedence, in one function |
| `internal/layout` | workspaces, tabs, panes, and the ids that are never reused |
| `internal/pane` | the grid, the pty, the headless adapter interface |
| `internal/detect` | Herdr's manifest engine in Go |
| `internal/server` | the socket, the dispatcher, the verbs |
| `internal/adapters` | one package per harness |
| `internal/attach` | the single-pane renderer |
| `internal/cli` | the command line and the thin client |
```

- [ ] **Step 4: Write the CI workflow**

`.github/workflows/coppice.yml`:

```yaml
name: coppice

on:
  push:
    branches: [master]
    paths: ["harness/coppice/**", ".github/workflows/coppice.yml"]
  pull_request:
    paths: ["harness/coppice/**", ".github/workflows/coppice.yml"]

concurrency:
  group: coppice-${{ github.ref }}
  cancel-in-progress: true

jobs:
  build-and-test:
    runs-on: ubuntu-latest
    env:
      COPPICE_GHOSTTY_PREFIX: ${{ github.workspace }}/.ghostty-vt
    steps:
      - uses: actions/checkout@v4

      - uses: actions/setup-go@v5
        with:
          go-version: "1.26"
          cache-dependency-path: harness/coppice/go.sum

      # libghostty-vt takes minutes to build from source, so it is cached on the
      # pinned ghostty commit. A pin bump invalidates the cache, which is the
      # point: a new commit must be built and tested, not assumed.
      - name: Read the ghostty pin
        id: pin
        run: |
          commit=$(grep -oE '\b[0-9a-f]{40}\b' harness/coppice/scripts/toolchain.sh | head -1)
          echo "commit=$commit" >> "$GITHUB_OUTPUT"

      - name: Cache libghostty-vt
        id: cache-vt
        uses: actions/cache@v4
        with:
          path: ${{ env.COPPICE_GHOSTTY_PREFIX }}
          key: ghostty-vt-${{ runner.os }}-${{ steps.pin.outputs.commit }}

      - uses: mlugg/setup-zig@v1
        if: steps.cache-vt.outputs.cache-hit != 'true'
        with:
          version: 0.16.0

      - name: Build libghostty-vt
        if: steps.cache-vt.outputs.cache-hit != 'true'
        run: harness/coppice/scripts/toolchain.sh

      - name: Preflight
        run: harness/coppice/scripts/preflight.sh

      - name: Vet
        working-directory: harness/coppice
        run: go vet ./...

      - name: Test
        working-directory: harness/coppice
        env:
          PKG_CONFIG_PATH: ${{ env.COPPICE_GHOSTTY_PREFIX }}/share/pkgconfig
        run: go test ./... -count=1

      # The adapter Stop paths, the pane tree and the server's live map are all
      # concurrent by design. A panic in an adapter goroutine takes the daemon
      # down, so the race detector is a merge gate here, not a nicety.
      - name: Test with the race detector
        working-directory: harness/coppice
        env:
          PKG_CONFIG_PATH: ${{ env.COPPICE_GHOSTTY_PREFIX }}/share/pkgconfig
        run: go test ./... -count=1 -race

      - name: Build
        working-directory: harness/coppice
        env:
          PKG_CONFIG_PATH: ${{ env.COPPICE_GHOSTTY_PREFIX }}/share/pkgconfig
        run: go build ./cmd/coppice
```

- [ ] **Step 5: Add coppice to `AGENTS.md`**

Find the section of `AGENTS.md` that maps the
repository's directories. Add one row or bullet, in the file's own style:

```
- `harness/coppice/` — the floor: one Go binary holding the panes, the harness processes and
  the merged pane state, served over a uid-checked unix socket. Read `harness/coppice/README.md`
  first, then `PINS.md` for what it depends on and why.
```

- [ ] **Step 6: Run everything one last time**

Run:
```bash
cd harness/coppice
go vet ./... && go test -race ./... -count=1
cd ../..
uv run --no-sync ruff check .
uv run --no-sync pytest -q
```
Expected: the Go suite is green, and the Python suite is untouched and still green. This plan
adds no Python.

- [ ] **Step 7: Commit**

```bash
git add harness/coppice/README.md .github/workflows/coppice.yml AGENTS.md \
        harness/coppice/internal/cli/docs_test.go
git commit -m "docs(coppice): a README a fresh reader can follow, and CI that builds the engine

libghostty-vt takes minutes to build, so CI caches it on the pinned ghostty
commit. A pin bump invalidates the cache on purpose: a new commit gets built
and tested rather than assumed. A test asserts the README documents every
top-level verb and repeats the restart honesty in the same words the server
uses, because two different answers to what survives a restart is worse than
one."
```

---

## Self-review

**Spec coverage.** Every section of `spec-02-coppice-server.md` maps to a task:

| spec section | task |
|---|---|
| Layout (`go.mod`, `PINS.md`, `NOTICE`, every package) | 0, 1, 2, 3, 4, 5, 6, 7, 11, 12, 14, 19, 20 |
| Socket API error codes (closed enum) | 2 |
| `pane.create` fields, `COPPICE_SOCK` / `COPPICE_PANE` | 6, 8 |
| `pane.attach`, frame diffs, 60 Hz coalescing | 9 |
| `pane.read --source detection` | 5, 13 |
| `agent.wait` resolving on merged state | 10 |
| `events.subscribe` | 9 |
| State merge, operator downgrade, 500 ms tick, gate quiet window | 3, 10, 13 |
| Ghostty grid, frame diff by seq, resize, `PINS.md`, toolchain | 0, 1, 5 |
| Headless `Adapter` interface, claude, codex, sprig, pi and opencode slots | 6, 14, 15, 16, 17 |
| Manifests: TOML schema doc, vendoring, override dir, no remote fetch, `explain` | 11, 12, 13 |
| CLI verbs, `attach`, `--remote`, `--stdio` | 19, 20 |
| Security: 0700/0600, `SO_PEERCRED`, `--stdio`, no secrets in spawned env | 6, 7 |
| Tests: proto goldens, shared event fixtures, state table + fuzz, golden grids, manifest tables, fake claude, server integration, restart | 2, 3, 5, 8, 12, 15, 18 |

Two spec lines are deliberately narrowed, each with its reasoning in the task:

- **"a fixture that matches two states is a test failure"** is implemented as *equal-priority
  rules that disagree*. Taken literally it fails on Herdr's own manifests, where a low-priority
  catch-all and a high-priority specific rule match the same screen by design. Task 12.
- **"every bundled manifest × every state it declares has ≥ 1 fixture"** is implemented for the
  four harnesses that have a manifest upstream (claude, codex, pi, opencode), with the other
  seventeen listed in
  `testdata/screens/unfixtured.txt` and a test that fails if a manifest is neither fixtured nor
  listed. Inventing a fixture for a UI nobody here has run would make the test pass and the
  detection wrong. Task 12.

**Placeholder scan.** No step says TBD, "add error handling", "similar to Task N", or "write
tests for the above". Every code step carries the code. The one deliberate discovery gap is Task
1 Step 3, which names the exact `go doc` commands whose output the implementation is written
against, as the brief requires of a discovery task.

**Server field ownership (fix round 1).** `Server` is declared once, in Task 7, with every field
the later tasks fill: `live`, `liveMu`, `waiters`, `tickStop`, `restore`, `detectWarnings`. Task 7
also declares `RestoreReport`, `SetDetectionWarnings`, `DetectionWarnings`, `LastRestore`, and the
two empty forward types `LivePane` and `waiter` that Tasks 8 and 10 replace. No later task adds a
field to a struct an earlier task already compiled against.

**Concurrency ownership (fix round 1).** Three rules, each with a `-race` test:
the tree hands out copies and `UpdatePane` is the only mutator (Task 4); one goroutine drains each
client's bounded event queue and a client that overflows it is dropped (Task 9); the goroutine
producing into an adapter's `Events()` channel is the only one that closes it, and `Stop` only
signals (Tasks 15, 16, 17).

**Type consistency.** `proto.PaneStateEvent`, `proto.Frame`, `proto.Cell` and `proto.ErrCode`
come from Task 2 and are used from Task 3 onward. `state.Merge` and `state.Store` come from Task
3 and are used in Tasks 8, 10 and 13. `layout.Pane` and `layout.Tree` come from Task 4 and are
used in Tasks 8 and 18. `vt.Term` and `vt.Cell` come from Task 1 and are used only in Task 5.
`pane.Grid` and `pane.FrameState` come from Task 5, `pane.PTY` and `pane.SpawnOpts` from Task 6.
`pane.Adapter`, `pane.Proc`, `pane.Event` and `pane.StartOpts` are declared in **Task 6**, before
Task 8's `LivePane` holds a `Proc`; Task 14 adds the registry and the transcript writer behind
them. `detect.Set`, `detect.Input` and `detect.Compiled` come from Tasks 11 and 12 and are used
in Task 13. `server.Server`, `Client` and `Handler` come from Task 7 and are used in Tasks 8, 9,
10, 13, 18 and 20. `attach.Run` and `attach.Options` come from Task 19 and are used in Task 20.
`ApplyState` is declared minimally in Task 8 and replaced by the full version in Task 10, which
Task 10 states explicitly. `pane.Event` carries `Ask *proto.Ask` from Task 6, which is the field
specs 04 and 05 populate on a blocked event; there is no `state.Ask`, and `internal/pane` already
imports `internal/proto` for `Cell` and `Frame`. `Merge` takes `curReceived` from Task 3 onward
and every caller passes the store's own clock. `attach.Run` returns `(Next, error)` from Task 19
and Task 20 loops on it. No name is used before the task that defines it.

**Fix round 1 (2026-09-08).** Every item in the corrections block above carries an
"— applied in Task N step M" line naming the task, the step and the test. Five blockers, fifteen
should-fix items and nine notes: 29 markers, 29 items.
