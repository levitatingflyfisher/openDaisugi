# Resume point — 2026-09-08, session paused on usage limit

**Execution (2026-09-08/09):** plan 00 COMPLETE (commits 667b384..67500fc). Plan 01 COMPLETE (commits 8a7ffdb..b02e37c: `opendaisugi.floor` contracts, `_state_report`, the gate reports state, Stop/Notification lifecycle events, `daisugi hook report`, `install --gate --report herdr|coppice`, floor stage in `daisugi modules`; suite 2856 passed). Next: plan 02 (coppice-server, Go). Rulings so far are listed under 'Execution rulings' below.

**State (2026-09-08, end of day):** all twelve plans written, adversarially reviewed, fix-rounded, and re-reviewed. READY TO BUILD: 00, 01, 03, 04, 05, 06, 07, 08, 09, 10 (10 carries one blocker in its corrections block: add `[int8]` to the dev extra, and fix the same gap for `[potion]`), 11. Plan 02: fix round 2 verified 4/4, ready to build. Every plan's corrections block is authoritative over its tasks. Next: execute per 'After the plans land' step 3.
The re-dispatch table below is now historical; keep it for the pointers.

## Re-dispatch table

Each agent: `general-purpose`, prompt = "Read docs/plans/2026-09-08-workshop/PLAN-WRITER-BRIEF.md
first and follow it exactly. Your sub-spec is <spec>. Write <plan>." plus the per-plan pointers
below. Run all twelve in one message so they work in parallel.

| plan | spec | model | extra pointers to read / fetch |
|---|---|---|---|
| plan-00-adr-0020-and-vision.md | spec-00 | sonnet | docs/adr/0004, docs/adr/README.md, VISION.md ≈60–80; check tests/test_adr_index.py exists |
| plan-01-floor-contracts-and-herdr-hook.md | spec-01 | sonnet | gate.py (_log_tree ≈572, _maybe_ask ≈137, _maybe_checkpoint ≈637, gate_and_contract ≈685, run_argv ≈1070), hook.py, gate_server.py, ask.py, session_tree.py, install.py 430–520, tests/test_gate*.py; keep the `_state_report.py` layer indirection |
| plan-02-coppice-server.md | spec-02 (XL) | opus | harness/sprig/* for Go style; WebFetch github.com/mitchellh/go-libghostty + pkg.go.dev for real API names; creack/pty; `gh api repos/herdrdev/herdr/contents/src/detect/manifests/claude.toml -q .content \| base64 -d` for the manifest schema; Zig 0.16 + CMake absent, no sudo → install to ~/.local (Zig tarball + sha256; `uv tool install cmake`); Go 1.25 present |
| plan-03-coppice-client-and-backends.md | spec-03 (L) | opus | tui.py, tui_base.py, tui_sessions.py (ask actions, action_steer, action_attach), tui_wiring.py, cockpit.py, swap.py, modules.py, config.py, cli.py ≈3168, tests/test_tui.py; spec-01/02 contracts; herdr.dev/docs/cli-reference; tmux verbs |
| plan-04-pi.md | spec-04 (M) | sonnet | docs/research/cockpit-sources-2026-08-27/pi_coding_agent__*.md, hook.py, gate_server.py, install.py ≈944–1040 and ≈1195; fetch pi.dev/docs/latest/rpc and raw extensions.md for tool_call signature; Node 22 present |
| plan-05-opencode.md | spec-05 (M) | sonnet | hook.py, gate_server.py, install.py adapters, spec-01/02/04; fetch opencode.ai/docs/plugins, /server, /sdk; pin endpoints from /doc |
| plan-06-phone.md | spec-06 (L) | opus | spec-02/03; fetch tailscale.com/kb/1153/enabling-https, docs.ntfy.sh/publish, github.com/mdp/qrterminal; visual-loop skill in the iss-skills repo (skills/visual-loop); localca fully specified (crypto/x509) |
| plan-07-voice.md | spec-07 (M) | sonnet | spec-03/06, gateway.py, gateway_journal.py, config.py, cli.py, pyproject extras; faster-whisper + sherpa-onnx APIs; a Pascal GPU (sm_61) crashes torch → CPU default; WAV fixtures via espeak-ng if present else skip-with-reason |
| plan-08-model-host-route.md | spec-08 (S) | sonnet | config.py ≈300–320, cli.py `setup`, gateway_asgi.py, gateway.py estimate_prefix_tokens ≈63, modules.py ≈187–200, tests/test_gateway*.py; fetch docs.ollama.com/api/anthropic-compatibility and /api for /api/tags, /api/show shapes |
| plan-09-switchyard.md | spec-09 (M) | sonnet | spec-08, gateway*.py, install.py _patch_claude_base_url ≈621, start.py _detach, cli.py gateway; fetch NVIDIA-NeMo/Switchyard README for TOML fields, /v1/messages, target header, binary/install |
| plan-10-int8-matcher.md | spec-10 (S) | sonnet | _search.py full, swap.py, modules.py, exceptions.py, scripts/matcher_fpr.py, tests/test_matcher_backend.py full, ADR-0018/0019 shape, pyproject; HF tree API for onnx/ file names + LFS sha256 |
| plan-11-layer-management.md | spec-11 (L) | opus | swap.py, modules.py, _search.py, pathway_store.py, gateway*.py, gate.py ≈174/≈200, clients/gate.py, docs/client-diversity.md, scripts/matcher_fpr.py, docs/harness/harness-comparison.md, tests/test_swap*/test_modules*; specs 03/09/10 Interfaces; bench/corpus/ concrete |

## After the plans land

1. Self-review each against its spec (coverage, placeholders, type consistency).
2. Adversarial review pass (the cockpit campaign's pattern: a fresh Opus reviewer per plan,
   corrections block prepended to the plan). Fail-open and data-loss findings are blockers.
3. Commit the plans. Then execute with superpowers:subagent-driven-development in the order
   00 → 01 → 02 → 03 → 06 → 07, with 08 → 09, 10, and 04/05 (after 02's adapter interface) as
   parallel lanes, 11 last. Sonnet builds, Fable/Opus reviews, TDD, no pushes (monthly fold).

## Execution rulings (running list; each with what it costs if wrong)

- **Build in-place on master, no worktree.** Repo convention (prior SDD ledgers did the same); master is unpushed until the monthly fold. Cost if wrong: a branch the user could have asked for.
- **`LAYER_EXCLUDED` lands exactly as spec-00 lists it; plan 03 adds `opendaisugi.cli`, `opendaisugi.tui_floor`, `opendaisugi.tui_grid` in the same commit that first imports the floor from them** (plan 03 calls those cockpit modules; the boundary test counts lazy imports). Plan 07's voice command in cli.py inherits the cli exclusion. Cost if wrong: one small extra edit in plan 03.
- **No separate final whole-branch review for plan 00**: the one batched task review covered the whole range and the re-review covered the fix commit. Cost if wrong: a missed cross-commit issue in a docs-and-tests-only plan.
- Deferred minors from plan 00 (for a later sweep, none load-bearing): the AST scan's Call branch is subsumed by the Constant branch; aliased `importlib` is not matched by the call helper; `LAYER_EXCLUDED` matches exact names only (matters if an excluded module becomes a package); `_resolve_relative` mis-slices for level 0 on a package (unreachable); two DeprecationWarnings come from `regex_to_z3.py`, pre-existing.

### Plan 01 rulings (2026-09-09)
- **Session-tree `state` entry type moved from Task 3 into Task 1** (its function needed it). Cost if wrong: none.
- **`ruff format --check .` fails on 86 pre-existing files at baseline**: the format rule binds new files and new lines; pre-existing files are not reformatted inside feature commits. Cost if wrong: CI's format step stays as it was.
- **State rows are bookkeeping, not activity**: `SessionIndex.list()` ignores `state` entries for `last_ts`/`entry_count`. Cost if wrong: a session whose only recent rows are state reports looks older in the cockpit roster.
- **§3.1 amendments (master spec edited in the same commits):** a `done` from `process`/`headless` is terminal and wins at once; a `gate` `blocked` must carry an ask; the 2 s precedence window holds back only a `manifest` incoming event, every other source is a fact; any `blocked` whose ask deadline passed reads as `working`. The Go port in plan 02 Task 3 MUST implement the same four rules (its plan text still describes the older window and a gate-only expiry). Cost if wrong: a stale headless idle could briefly override a fresh gate working; both come from the same process, so the newer is the truth.
- **Both validators type-check every field** (layer `_validate_hook_report_row` and floor `from_json` agree on strings, non-bool numbers, dict ask, ask field types); cross-check table pins them.
- **coppice preference is POSSIBLE, never ACTIVE, until a server answers**; every `--report` value writes `floor_report`; herdr row requires both hooks and says the CLI contract is unverified.
- **No separate final review for plan 00**; plan 01 had a full final review plus one fix wave (5 commits).

### Plan 01 carry-forwards (do these in the named plan)
- **Plan 03 first Python task:** `gate.py` builds the ask row with `decision.tool_name` unconverted; a payload with `tool_use_id` but no tool name yields `"tool": null`, which the stricter `from_json` now rejects. One-line fix: `"tool": str(decision.tool_name or "unknown")` at the ask-row site in `_report_blocked`. Also add `home=`/`which=` passthrough to `render_wiring()` so `tests/test_modules.py` stops reading the real settings.
- **Plan 02 Tasks 2/3:** port the four §3.1 amendments into `proto.Validate` and `state.Merge`; `pane.report_state` accepts exactly `{"id":"r","cmd":"pane.report_state","pane":…,"event":{…}}` and answers within the client's 200 ms total budget.
- **Plan 03/04:** the headless permission-prompt deadline is hard-coded to 90 s in `hook.py`; with any expired block reading `working`, a harness that dies mid-prompt shows `working` after 90 s. Revisit the constant when the floor reader ships.
- **Plan 03:** `LAYER_EXCLUDED` gains `opendaisugi.cli`, `opendaisugi.tui_floor`, `opendaisugi.tui_grid` in the commit that first imports the floor from them; construct `PaneStateEvent`s with correct types (no `__post_init__` type checks); `PaneInfo.cwd: str` vs `spawn(cwd: Path)` to decide on first implementation; the cockpit roster's age grouping and PaneStateEvent state are still two independent notions.
- Deferred, none load-bearing: context marshalling duplicated 3x in gate.py (`_report_ctx` helper); `_report_blocked` naming; the `remaining` wait-budget test; two older `h["command"]` sites in install.py; two `.bak` files per install; `swap.py` docstring; truncation test hard-codes `width - 4`; `Ask.deadline` annotated float may hold int.

### Plan 02 rulings (2026-09-10)

Plan 02, coppice-server, executed 2026-09-09 to 2026-09-10 over Tasks 0 to 21, commits
f958e58..43a1bc2, 90 commits, no attribution line in any. Every task passed an Opus or Sonnet review with two
verdicts and a scoped re-review; fix rounds ran one to three per task. The ledger with every
ruling is `.superpowers/sdd/plan-02-coppice-server/progress.md`, git-ignored. The rulings that
bind later plans:

- **State merge, both floors.** `done` is absorbing: a dead process or ended session never
  comes back; a resumed harness is a new pane. Go uses the server receive clock for the
  manifest hold window. A restored pane is `done` or `unknown` with source `process`, never
  `idle`. The attach status line applies `EffectiveState` at build time and repaints on a 1 s
  ticker while blocked, so an expired ask reads `working` everywhere. Cost if wrong: a
  dead agent shown working; the tests named in the ledger guard it.
- **Wire.** `pane.run` carries the command text under `line`, since `cmd` is the verb.
  `Client.Do` refuses params named `id` or `cmd`. `pane.create` after `Close` answers
  `pane_closed` with server-scoped copy until a `server_closed` code exists with its Python
  twin. State events are broadcast for every pane; clients filter on `pane`.
- **Lifecycle.** Startup order is `AcquireStartLock`, `registerAll`, `Listen`, `Restore`,
  `Serve`, so a resumed agent's gate hook finds the socket in the backlog. `Restore` refuses
  without the lock, with live panes, or after `Serve`. `Close` tears live panes down twice
  under two 2 s deadlines, before and after `waitForHandlers`; every `Close` caller waits on
  `closeDone`; `Close` is never called from inside a handler. A `Server` is single use. The
  peer-EOF path waits up to 130 s for handlers with a per-send 1 s write deadline, so a
  `--stdio` reply is never lost to teardown while a stuck peer still trips at 1 s.
- **Adapters.** codex shape for one-shot harnesses: one process per prompt, then `--resume`.
  A resume-started pane never mints a session under the id it was asked to resume. Adapters
  never emit `EvEnd`. Central registration lives in `internal/adapters/all`; nothing else
  imports an adapter package. Codex wire format and resume argv are unverified on this box.
- **CLI.** `--data-dir` pairs with `--socket`; the daemon logs to `<data-dir>/server.log`;
  `server start` is idempotent and exits 0; `server stop` waits for the lock, not the socket;
  autostart strips `COPPICE_PANE` and `COPPICE_SOCK`; `COPPICE_NO_AUTOSTART` non-empty means
  off; unknown flags exit 1; `--timeout` maps to `timeout_ms`; `--json` is universal;
  transport failures exit 3 with the ssh stderr tail; `attach --remote` is refused.
- **Attach.** `Options.Rows` is the terminal height and `Run` reserves the status row.
  Keystrokes are assembled into whole UTF-8 runes before `pane.send_text`. One `ReadKeys`
  channel serves every `Run`; `Run` joins its key goroutine and the CLI joins `watchResize`
  with a bounded dial, and leaves the alternate screen before any wait.
- **CI.** `COPPICE_REQUIRE_TOOLCHAIN` turns toolchain skips into failures; the workflow has no
  preflight step; `toolchain.sh` runs only on a cache miss; Go 1.26.8 pinned; the cache key
  folds the ghostty commit, the zig version and the script hash. The workflow has never run on
  a real runner.
- **Tests.** Never a real claude, codex or sprig binary; fake adapters via `COPPICE_*_BIN`;
  never a TTY; never a hand-made `/tmp` path; `-race` everywhere; every dial sets
  `COPPICE_NO_AUTOSTART`. Reports state only what was measured; a null RED is reported as a
  null, never manufactured.

### Plan 02 carry-forwards (do these in the named plan)

- **Plan 03, first Python task:** `merge()` gains the done-absorbing guard and the same
  optional `pane` and `harness_session_id` type checks Go enforces, each with an oracle row;
  land `tests/floor/testdata/events` so the Go cross-language fixture test stops skipping;
  note that Python uses `current.ts` where Go uses the receive clock for the manifest hold.
- **Plan 03, wire:** add `server_closed` to the shared error enum in both languages and make
  `pane.create` after `Close` use it.
- **Plan 03, cockpit:** the floor screen consumes `pane.list` rows keyed `pane` for agent
  verbs and `id` for pane verbs; the 16 ms frame interval and `autoPanes` demotion are
  documented only in a test comment, so the Python backend must not assume per-write frames.
- **A person at a terminal, once:** the raw-mode, alternate-screen and panic-restore branch of
  `coppice attach` has never been exercised by a human; the README says so.
- **Known open items, listed in the README's "What is not verified yet":** codex wire and
  resume argv; no real sprig ever run; `claude.toml` `live_turn_working` cannot match a line
  ending "esc to interrupt)"; the 130 s wedged-handler hold; `layout.Save` after a rename with
  a failing directory fsync is untested; Linux only in practice.
- **Detection window, final fix wave.** The evaluator sees the whole unwrapped plain screen
  and each rule's region slices it, as Herdr does; `pane.read --source detection` and
  `pane.explain` show exactly that text. spec-02's "bottom 12 unwrapped rows" sentence is
  amended with a dated note. Reason: 43 vendored rules ask for more than 12 lines. Cost if
  wrong: per-pane tick cost of rows times cols.
- **CLI exit codes, final fix wave.** Every `attach` form and `server stop` dial without
  autostart; a dial failure is exit 3 everywhere; `server stop` with nothing running says so
  and exits 0; the read deadline follows `timeout_ms` plus a margin.
- **Wire for plan 03, final fix wave.** `pane.list` and `agent.list` rows carry `ts` and
  `session_id` from the stored event; named keys include f1 to f12 and
  `testdata/keys.json` is the one vocabulary; `server_closed` exists in the Go enum.

### Plan 03 rulings (2026-09-11)

Plan 03, coppice client and backends, executed 2026-09-10 over Tasks 0 to 12 plus 2b,
commits 43a1bc2..9b2f7c1, 128 commits, no attribution line in any. Every task
passed an Opus or Sonnet review with two verdicts and a scoped re-review; fix rounds ran one
to three per task; a whole-branch Opus review closed with "ship-ready with fixes" and one fix
wave in two lanes on disjoint files. The ledger with every ruling is
`.superpowers/sdd/plan-03-coppice-client-and-backends/progress.md`, git-ignored, archived
under `~/opendaisugi-backups/`. The rulings that bind later plans:

- **The Python manifest evaluator is a faithful port, not a re-design.** Go's ASCII Perl
  classes are reproduced with `re.ASCII` plus explicit rewrites; `$` is end of text unless
  the `m` flag is set; loads fail closed on any malformed manifest; the recorded schema is
  pinned to the Go source by a sha256 drift file, regenerated with
  `scripts/floor_manifest_schema.py` whenever `internal/detect` changes. One recorded
  divergence: Go folds Unicode under `(?i)`, Python's `re.ASCII` does not; zero vendored
  manifests reach it. Cost if wrong: a screen rule matches on one floor and not the other.
- **`merge()` is Go's twin.** Done absorbing; a gate block without an ask is refused;
  only the manifest source is held by the receive-clock window; an expired block reads
  working; `ERROR_CODES` is bound to `proto.go` by a parsing test; the attribute bits of a
  cell are bound to `vt.go` the same way. Cost if wrong: two floors disagree about one pane.
- **Registry.** Auto order is coppice, herdr, tmux; a probe never starts a daemon; a named
  backend that is not available is refused, never downgraded; `wait_for_state` reads through
  `effective_state` and never turns an unreachable host into `done`: absence counts only
  after a poll that saw the pane. Cost if wrong: a script moves on from a pane that is alive.
- **The coppice backend dials the socket the config names.** `FloorConfig.coppice_socket`
  and `config.data_dir / "coppice"` reach `CoppiceBackend` through `build_backend`; every
  `daisugi coppice` verb takes `--socket` and `--data-dir`, so a second openDaisugi instance
  has its own floor. Exit codes: 0, 1 the server or the CLI said no, 3 unreachable; a dropped
  connection is 3; no verb ever prints a traceback. Cost if wrong: two instances share one
  floor, or a script reads a traceback as success.
- **Gate facts reach every backend.** The gate records the pane it runs in from
  `COPPICE_PANE`, `HERDR_PANE_ID`, `HERDR_PANE`, or `TMUX_PANE`; `floor/gate_states.py` reads
  the session tree's newest state entry per pane; the registry hands that reader to the tmux
  and herdr backends, so a gate block outranks a screen guess under master 3.1 and carries a
  session id the ask keys can answer. Herdr blocks from Herdr's own screen stay
  manifest-sourced with no ask. Cost if wrong: the day-one backend shows a guess where a
  fact belongs and the ask keys lie.
- **One contract suite for every backend.** `tests/floor/test_backend_contract.py` runs
  every row against tmux on a per-pid private socket, coppice through the sandbox server
  built from `harness/coppice`, and herdr when installed; it asserts the degrade half and the
  raise half of fail-closed, that a backend yielding frames implements attach and detach,
  that `close()` stops a running pane, that a failed poll never fabricates `done`, and that
  every key vocabulary is a subset of `testdata/keys.json`. Herdr has never run it on this
  box. Cost if wrong: a future backend passes while lying.
- **The floor screen never lies and never dies.** The roster never attaches and never
  resizes a pane; the roster poll runs one at a time at the operator's `--interval`, never
  resets the cursor or disarms an allow, and survives a raise; the ask keys refuse with a
  reason on a pane with no session id and the ramp hides them; a dead events pump says so;
  every backend call on both screens goes through one error helper; `:floor <name>` re-picks
  on the next resume; the attach screen pops itself at most once; the leave key twice sends
  one leave byte to the pane and only chords in `keys.json` are sent. A text-only backend refuses attach
  and names its own attach command. Cost if wrong: an operator closes the wrong agent.
- **Copy and hygiene.** STE100 in every new string and comment, no em-dashes, no
  parentheticals, comments describe what exists and never cite rounds or tasks; the
  formatter runs repo-wide once at the end of a plan so CI's format job is green at the fold.

### Plan 03 carry-forwards (do these in the named plan)

- **Plan 06 or 07, whichever next touches the gate reader.** `floor/gate_states.py` has no
  recency bound: a gate fact recorded for a tmux pane id that a restarted server has
  recycled shows on the new pane until the next gate write. Bound it to entries newer than
  the pane's own start when the backend can tell. (fix-wave-A-re-review.md, finding 5)
- **Plan 06 or 07, herdr.** The herdr backend drops a gate fact for a pane that has no
  `agent list` row, and `list_proven` costs a third round trip per poll; fold both into the
  first plan that runs the contract suite against a real herdr, which no box here has done.
  (findings 2 and 7)
- **Any plan, tests.** `tests/floor/coppice_sandbox.py`'s fallback to the source checkout
  for manifests only resolves inside a checkout; a wheel install has no manifests to test.
- **Any plan, cockpit.** `SessionsScreen`'s one second poll timer can race a growing number
  of screens in one pytest process; one transient `NoMatches` on `#alerts` was seen twice
  under load. The floor's own timers are now cancelled on unmount; give the sessions poll
  the same treatment if it flakes again.
- **Plan 11, ops.** Two implementers in one shared tree each lost an edit to the other's
  `git stash`; the ledger rule is now "never git stash", but parallel lanes belong in
  worktrees on real disk beside the repo.

### Plan 06 rulings (2026-09-11)

Plan 06, the phone PWA for coppice, executed 2026-09-10 to 2026-09-11 over Tasks 0 to 13,
commits 54975bd..691dc22, 88 commits, no attribution line in any. Every task passed an
Opus or Sonnet review with two verdicts and a scoped re-review; fix rounds ran one to three per
task; a whole-branch Opus review closed with "ship-ready with fixes" and one fix wave by a single
implementer. The ledger with every ruling is `.superpowers/sdd/plan-06-phone/progress.md`,
git-ignored, archived under `~/opendaisugi-backups/`. The rulings that bind later plans:

- **The web verbs live in `internal/cli/web.go`** and are switched from `cli.go`; the global
  `--socket` and `--data-dir` reach them. Every default path derives from the data dir through
  `internal/web/paths.go`: token, CA, TLS, and `web.json` under `<data-dir>/web/`, the gate root
  at `<data-dir>/../gate` resolved and named in the startup log. No home-directory lookup exists
  in `internal/web`. A second coppice instance gets its own phone by `--data-dir` and `--socket`.
- **`web.AutoStart` is called from `internal/cli`'s `serverStart` foreground branch**, never from
  `internal/server`, which does not import `internal/web`. A saved `web.json` with `enabled`
  starts the phone server with the coppice server; a config-read failure is a stderr warning
  and the panes still run; a failed `Serve` cancels its own context so the CA hand-off listener
  and the push watcher stop too. `LoadConfig` treats a missing file as off and a malformed file
  as an error, never as enabled.
- **The token is a 32-byte secret at `<data-dir>/web/token`, 0600**, checked in constant time
  as a Bearer header or as the `daisugi.bearer.<token>` websocket subprotocol, with a per-address
  ban of one minute after three failures. It never reaches a log line, a URL query, the saved
  config, the service worker cache, or a screenshot. The phone reads it once from the `#t=`
  hash and replaces the hash. Every refusal, the guard's included, is JSON with a sentence that
  says what to do next.
- **One websocket is one upstream socket connection, unrewritten.** The read limit is
  `MaxRequestBytes`, 1 MiB, mirroring `proto.MaxLine`. A dead upstream closes the browser side
  with a reason string the client matches character for character; the two reasons are pinned
  in PINS.md and by a Go test against `ws.go` and `app.js`. Upstream death retries with a
  backoff that grows to 30 s and resets on the first message, not on the handshake; a rejected
  token stops retrying and opens Settings populated.
- **A phone answers a gate ask only through the gate's own files.** `AskChannel.Answer` reads
  the ask first, refuses a decision outside allow and deny, an unknown ask id, and an ask with
  no nonce, then writes `answers/<SafeID>.json` at 0600 by rename with the live ask's nonce.
  `SafeID` is a byte-for-byte port of `ask.py._safe`, held by a shared fixture the Python
  conformance test also reads, including cases that separate strip-then-cut from cut-then-strip.
  A refusal from coppice-server is a 502 and is never painted as an empty roster; a non-object
  `result` on an ok reply is a refusal too.
- **ntfy is self-hosted or not at all.** One push per merged transition to `blocked`, debounced
  5 s per pane by an injected clock; the body carries the pane label and the ask summary, or the
  event's `detail`; the click link is the external URL plus the pane id. `Watch` reads the
  subscribe reply and returns a refusal rather than waiting forever; an unlisted pane costs one
  `pane.list` lookup. `/api/push/test` answers 409 unconfigured, 502 refused, 200 sent, and the
  Settings screen shows three distinct sentences. The topic never reaches a log line.
- **TLS has four sources**: `off` refuses any listen but loopback; `files`; `localca` issues an
  ECDSA P-256 CA and a leaf of at most 398 days and refuses to replace an unreadable CA naming
  the file; `tailscale` uses the pair `coppice web cert tailscale` writes. The plain-HTTP CA
  hand-off on `--ca-listen`, `:8080` by default, is documented under "What leaves the box" and
  can be narrowed or closed.
- **The shell ships with no bundler and no inline script**, a CSP of `'self'` only, a service
  worker whose single decision function `shouldCache` has Node tests and never caches `/api/*`
  or `/ws`, a SHELL list compared exactly to the embed in Go and in Node, and stale-while-
  revalidate so an installed phone picks up a new shell. Static routes are registered per
  embedded file, so a wrong-method API call is a 405 with an `Allow` header and no directory
  listing exists. One wrapper puts the security headers on every response including the mux's
  own 404 and 405.
- **The seven attribute bits** bold 1, faint 2, italic 4, underline 8, blink 16, inverse 32,
  strike 64 are pinned in PINS.md, `pins_test.go`, `grid.js`, and `grid.test.mjs`; `drawGrid`
  renders faint as reduced alpha, blink as nothing, inverse as swapped colours, strike as a
  line. `stateOf` reads the flat `pane.list` row; the fixture carries every key
  `handlePaneList` sets, `session_id` included. Prompt is `agent.prompt`; Steer is
  `pane.send_text`; the harness picker offers `claude`, `codex`, `sprig` by adapter name.
- **A cold deep link into a pane attaches and shows its ask**: `open` clears its state on a
  failed attach, `mountPane` listens for `coppice:open`, and the `coppice:panes` event refills
  the header and the ask box when the roster's first reply lands. The roster's first paint goes
  over `/api/panes` before the socket opens and never blames the token during the handshake;
  with no token stored it makes no request at all. A socket close detaches the pane so the next
  open re-attaches. Saving a token reconnects in place: the live socket is closed deliberately,
  no retry is scheduled with the old token, and the copy is "Saved. Reconnecting."
- **Node tests run as** `node --test "harness/coppice/internal/web/static/_tests/*.test.mjs"`
  from the repo root; a bare directory path does not work on Node 22. `js_test.go` runs that
  suite inside `go test` and requires `# fail 0` with a per-file floor. The Playwright smoke,
  `scripts/phone_smoke.py`, builds under `harness/coppice/build/`, keeps scratch under
  `harness/coppice/.smoke/`, drives an `sh` pane over `--tls off` and the local CA, masks the
  token and uses a fixed directory literal so all five 360x780 screenshots regenerate
  byte-identical from a clean checkout. The screenshot existence check runs unconditionally;
  only the live pass is gated on Playwright and `COPPICE_SMOKE=1`; CI does not run it.
- **Copy and hygiene**: comments cite no task, round, review, plan, spec, ruling, or "master
  spec"; the Go pins test parses PINS.md; feature-status rows say "since the October 2026 fold"
  with no version number; NOTICE carries ISC for `coder/websocket`, MIT for `qrterminal`, and
  BSD-3 for `rsc.io/qr`.
- **Process rulings that bind every later plan**: implementers commit by explicit path only and
  never `git stash`, `add -A`, `-a`, or reset what they did not stage; nobody kills a process they
  did not start; nothing runs in the background; parallel lanes only on disjoint files and, after
  the 2026-09-11 out-of-memory restart, at most one implementer and one reviewer at a time with
  `go test -p 1`; a reviewer that has written nothing after forty minutes is stopped and replaced
  with a tighter brief.

The frame note in the plan 02 carries above now reads 16 ms; the earlier "75 Hz" wording was
wrong.

### Plan 06 carry-forwards (do these in the named plan)

- **Plan 11**: the gate reader recency bound and the two herdr carries from plan 03 (ruled R19).
- **Plan 11**: `internal/cli` has no logger, so `serverStart` passes none into `AutoStart` and
  the phone server logs through `slog.Default()`; thread one when the CLI grows a logger.
- **Plan 11 or the next coppice pass**: the 5 s close-handshake wait reachable from websocket
  teardown on an unanswering peer; the one-byte band where the read limit passes and `Send`
  refuses; stdout and stderr interleaving on a partial tailscale failure; the 401 branch pushes a
  history entry rather than replacing it; the smoke driver copy left under `~/.cache/oh-visual-loop`;
  `Watch`'s 5 s ack timeout has no test; a hostile page on the phone can spend the operator's own
  three ban strikes against a guessed coppice address, self-limiting at one minute; the per-file
  JS test gate cannot see a symmetric deletion of `test(` calls, which needs a committed per-file
  expectation.
- **Plans 04 and 05**: add `pi` and `opencode` to the harness picker when their adapters build.

### Plan 07 rulings (2026-09-11)

Plan 07, the voice bridge, executed 2026-09-11 over Tasks 0 to 8, commits af39548..960145d,
47 commits, no attribution line in any. Every task passed a Sonnet or Opus review with two
verdicts and a scoped re-review; fix rounds ran one to two per task; a whole-branch Opus review
closed ship-ready with fixes and one fix wave of ten items, checked by the controller against the full
suite (3606 passed) rather than a separate re-review, to spare usage. The ledger with every ruling is
`.superpowers/sdd/plan-07-voice/progress.md`, git-ignored, archived under
`~/opendaisugi-backups/`. The rulings that bind later plans:

- **Prompt, not send, and the arm decision is made once.** `deliver()` in
  `opendaisugi.voice.deliver` takes a `backend_factory` callable and calls it only after its own
  arm check passes on the `send` path; `preview` never builds a backend; an unknown mode is
  refused; a refused send on a host with no pane backend is 403 naming the arm command, never
  503. The grant file is named by `urllib.parse.quote(pane_key, safe="")`, so two pane ids can
  never share a grant, and stores the original key; a grant expires by wall-clock comparison on
  every check; `disarm` on a never-armed pane returns False and leaves no file. No client posts
  `mode: send` today: the phone button and `daisugi voice ptt` both preview, and the how-to says
  so. Send goes through `registry.prompt_pane`, so a headless pane is prompted, not typed into.
- **The cleanup pass is journaled like any other turn.** `clean_transcript` records the real
  transcript as `task` and `ask` through `record_turn`, tagged `tier1-local` and priced 0.0, so
  the garden sees the true repeat signal. It returns `CleanupResult(text, cleaned, reason)`:
  cleanup off gives the raw text with `cleaned=False` and no reason; a usable correction gives
  `cleaned=True`; a transport error or an empty reply gives the raw text with a reason and
  journals nothing. The transport is `litellm.completion()` against the configured local model,
  constructed with keyword arguments only.
- **The voice server fails closed at every edge.** The bearer is required on every POST,
  loopback included, since any local process or any page open in a browser on this box can
  reach 127.0.0.1; compared with `hmac.compare_digest`, read from
  `config.data_dir / "coppice" / "web" / "token"` or `--token-file`; `build_server` refuses to
  start on any address without a readable token file, checked before the bind and again
  against the bound address; `/transcribe` and `/deliver` refuse a request carrying an `Origin`
  header or a `Sec-Fetch-Site` other than `same-origin` or `none`, and `/deliver` requires
  `Content-Type: application/json`; a catch-all in `do_POST` answers 500 with a sentence, bad
  WAV is a 400, a journal write failure returns the raw text, and an unreadable grant path
  reads as unarmed; the ban ledger locked and pruned; a 60 s clip ceiling and wire caps before
  any read; negative or chunked framing refused; `/health` unauthenticated liveness only; every
  error reply, 404 included, carries a `message` sentence naming the next step; `/transcribe`
  returns `text` and `raw_text` on every reply. `serve()` takes an `on_bound` hook so the CLI
  prints its banner only after a real bind. The coppice web proxy builds a fresh upstream
  request with only Content-Type copied and the bearer set, so the phone path passes.
- **Engines.** `pick_engine` has an explicit branch per known name: `EngineUnavailable` when a
  known engine's package or model files are missing, which the CLI maps to exit 3;
  `UnknownEngine`, a `ValueError`, for any other name, exit 1. faster-whisper runs on CPU by
  default and on CUDA only when asked and verified; Parakeet through sherpa-onnx is opt-in and
  its archive is about 460 MB, never downloaded by any step or test. The CLI has no eager
  faster-whisper check, so a Parakeet box starts without it. `voice_*` fields are flat on
  `Config` after the `floor` field.
- **Audio.** ffmpeg first, `av` for webm decode when ffmpeg is absent, a bare-numpy resample last;
  every unsupported case raises `AudioFormatUnsupported` naming ffmpeg; every path returns a
  valid 16 kHz mono WAV with a correct header, because ffmpeg on a pipe writes a placeholder
  header and the module repacks it; `wav_duration_s` counts frames read rather than trusting the
  header. The three speech fixtures are deterministic across runs.
- **Push-to-talk** is a tap toggle, not a hold: tap space to start recording, tap it again to stop
  and send, and the copy says so everywhere. It never opens a device or a TTY in tests; `sounddevice` is imported lazily; the
  default stream is an adapter that reads only what the driver has buffered, so a held key
  captures the whole press; an empty release posts nothing; the client always sends the bearer, and a
  missing token file is one line naming `coppice web token`; a refused connection names `daisugi voice serve`; `q`
  while recording stops the stream. `voice ptt` takes `--data-dir` like its siblings.
- **The phone's record button** is an ES module mounted from `pane.js`, posting only through
  `window.coppice.api` to the same-origin `/api/voice/transcribe`, which the coppice web server
  proxies to `web serve --voice-url` with the bearer from the token store; the CSP stays
  `'self'` and the phone holds one token. Unset, the proxy answers 409 naming `daisugi voice
  serve` and the flag; the body is capped at 8 MiB; the voice server's `message` sentence is
  copied into `error` so the phone shows it. The button holds one state, the click handler's
  idle gate is the one guard against a second recorder, the post happens after the recorder's
  `stop` event, an empty transcript never wipes a draft, and a non-empty prompt box is appended
  to. `--voice-url` must be an absolute http or https URL.
- **CLI and docs.** The four voice commands sit after `DEFAULT_DATA_DIR`; no traceback leaves the
  CLI: a port in use, a half-given TLS pair, a malformed `--server`, and `--for inf` each exit 1
  with a sentence; Ctrl-C exits 0; `arm` and `disarm` take `--json`; `modules.py`, `swap.py`, and
  `tui_wiring.py` all know the `voice_engine` stage. The how-to has a troubleshooting table that
  quotes the client and server strings verbatim, a "What leaves the box" section, `--data-dir`,
  the coppice socket path, the phone's microphone-permission rows, and the gateway journal under
  "What leaves the box"; parenthetical command names in table rows and module labels
  follow the file's pattern and are fine.
- **Process rulings**: no module or test docstring cites a spec, plan, task, ruling, or "master
  spec"; modules are hidden in tests with `monkeypatch.setitem(sys.modules, name, None)`, and a
  test that re-imports a package module restores the package attribute at teardown; the
  `voice_live` marker is enforced in the root `tests/conftest.py`; `uv pip install` into the
  project venv is how an extra reaches this box, since `uv run --no-sync` installs nothing; one
  implementer and one reviewer at a time with `go test -p 1`; the Python suite runs in halves.

### Plan 07 carry-forwards (do these in the named plan)

- **Plan 11**: the auth-failure ban ledger is now reachable from loopback, so one local caller
  with a stale token bans every loopback caller, the phone proxy and `ptt` included, for 60 s
  (self-healing, but a per-token or per-route ban would remove it); no test drives the fresh-box
  `RuntimeError` through `voice serve` to exit 3; `disarm` has no exception guard; a very long
  pane key could exceed a file-name limit after percent-encoding; `PaneRef.backend` defaults to
  the literal `unknown` when `/deliver` omits it; the negative Content-Length guard bounds a
  stalled read at 30 s rather than eliminating it; the invalid-TLS-pair path still leaks the
  bound socket; the proxy's drain-before-409 has no regression test.
- **A later plan, once a client wants direct send**: a `--send` flag on `ptt` or a phone-side
  toggle that posts `mode: send`, and the floor's `armed` badge the spec names.
- **Hardware**: the sounddevice adapter and the phone's record button on a real phone are
  untested on this box; the how-to and feature-status say so.

### Plan 05 done (2026-09-23)

Plan 05, the OpenCode adapter and gate plugin, was built and approved after four fix rounds. OpenCode 1.18.32 is installed at `~/.local/bin/opencode`. The gate hard-denies any edit to the global OpenCode config directory, a project `.opencode` plugin or tool directory, and any `opencode.json`. apply_patch is checked path by path. A typed prompt never approves an OpenCode permission prompt. Only an operator allow names it. Parked, for one later design of harness-config protection shared with pi: `OPENCODE_CONFIG_DIR`, a directory change the gate cannot follow such as `git -C`, a delete of a parent such as `rm -rf ~/.config`, a shell `daisugi install --uninstall` or `daisugi gate disarm`, the gate reading the plugin path from its own env, and a late plugin report that shows a stale ask again. False positives that fail closed: a grep or `gh --body` that names opencode.json, and `git stash -m opencode.json`. The OpenCode ask has no workspace, as for codex and pi, so it is always permanent.

Hand check:

No automated test runs a real OpenCode, so these steps confirm the parts only
a real run can show:

1. Run `daisugi install --harness opencode`. Check that
   `~/.config/opencode/plugins/daisugi-gate.ts` exists.
2. Run `daisugi start`, so the resident gate listens.
3. Start `opencode` in a scratch project and ask it to read one file.
4. Check the gate's shadow log under `~/.opendaisugi/gate/shadow/` for that
   Read call.
5. Set `gate_mode: enforce` with an envelope that does not allow `rm`, ask
   OpenCode to run `rm` on a scratch file, and see the tool call fail with
   the gate's reason.
6. Start a coppice pane with the opencode harness. See it reach idle, send it
   a prompt, and see it go working, then idle.
7. Make OpenCode ask for a permission it is configured to ask about. See the
   pane go blocked, answer `y` from the floor, and see it go on.

### Plan 09 done (2026-09-23)

Plan 09, NeMo Switchyard upstream of the gateway, was built and approved after two fix rounds. switchyard-server 0.3.0 is installed on PATH, built from NVIDIA-NeMo/Switchyard tag v0.3.0, commit 336196f. Every cloud tier forwards the operator's own login by default. A key is used only when `install --api-key-env NAME` names it, and the gateway refuses to start when that variable is unset. A turn counts as a saving only when both models have a price and the served one is cheaper on input and output, in external and rules mode alike. The child binds loopback only, dies with its gateway on Linux, and runs on its own per-port config, state and logs.

Parked notes: OpenAI-wire turns are routed but book no saving, because the price table has no gpt ids, and no start line says so yet. A stop leaves the per-port TOML and log behind. `install --router switchyard` still writes `~/.opendaisugi/switchyard.toml`, which no gateway loads; it serves only as a `--dry-run` target. The suite-wide use of pytest `tmp_path` lands in `/tmp`.

Hand check:

1. Run `daisugi install --gateway --router switchyard --efficient-model <a model your Ollama serves>`. The output must say the capable tier forwards your own login.
2. Run `switchyard-server --config ~/.opendaisugi/switchyard.toml --dry-run`. Expect `server OK: daisugi`.
3. Run `daisugi gateway`. Expect `router: switchyard`, `healthy on http://127.0.0.1:4000`, and the two auth lines.
4. In another shell, run `ANTHROPIC_BASE_URL=http://127.0.0.1:8787 claude`. Do one easy turn and one hard turn.
5. Run `daisugi router status`. Expect the child pid and port, `healthy`, who pays for each tier, and target rows. Cross-check with `curl -s http://127.0.0.1:4000/v1/stats`.
6. Stop the gateway with Ctrl-C. `daisugi router status` must show no running child.
7. Start a gateway, `kill -9` it, and confirm its child is gone too. Status labels the state file stale, and `daisugi router stop` clears it.

Only a live run can show whether the subscription login passes `forward_auth` end to end, and whether a Claude Code feature that needs a non-oauth `anthropic-beta` value breaks.
