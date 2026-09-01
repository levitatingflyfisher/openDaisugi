# Plan 06: The phone — a PWA served by coppice-server, push through ntfy

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** From the phone in the kitchen, over a tailnet or a LAN and with nothing of Google's in the default path, see every coppice pane and its sourced state, open one, read its grid, prompt or steer it, allow or deny a gate ask, and be told by ntfy when a pane blocks.

**Architecture:** A new Go package `harness/coppice/internal/web` runs inside the coppice server process. It serves an embedded plain-JS PWA over HTTPS and one websocket endpoint. The websocket is not a new protocol: each browser connection opens one unix-socket connection to coppice-server and pipes the §3.3 JSONL through unchanged, so a phone client is the same kind of client as a terminal one and the bytes are the same bytes. Everything the package needs from coppice-server arrives through a single `Dialer` interface, so every test in this plan runs against an in-process fake JSONL server and the plan builds without waiting for plan 02's internals. TLS comes from one of four sources: a `tailscale cert` pair, a local CA this package generates with `crypto/x509`, explicit files, or plain HTTP that refuses any address that is not loopback. Push is an HTTP POST to a self-hosted ntfy on each merged transition to `blocked`.

**Tech Stack:** Go 1.25 (`crypto/x509`, `crypto/tls`, `log/slog`, `embed`, `net/http` method patterns), `github.com/coder/websocket`, `github.com/mdp/qrterminal/v3`, plain ES-module JavaScript with no bundler and no framework, Node 22 `node --test` for the JS unit tests, Playwright through the repo's `visual-loop` skill for the screenshot pass, Python 3.12 stdlib for the smoke orchestrator.

**Spec:** `docs/plans/2026-09-08-workshop/spec-06-phone.md` (master: `docs/plans/2026-09-08-workshop/00-master-spec.md` §5.6, §3.1, §3.3, §3.6, §4).

**Requires:** plan 02 (`harness/coppice` module, the unix socket, the §3.3 command set) and, at run time, plan 03's floor only in the sense that the PWA mirrors what the TUI floor does. **Provides:** `harness/coppice/internal/web`, `coppice web serve|token|cert`, `docs/how-to/phone.md`, `scripts/phone_smoke.py`.

## Corrections from adversarial review (2026-09-08 — authoritative over the tasks below)

**Tasks 0, 1, 3, 6, 9 and 12 are ready to build now.** Five of the six things this plan was
hunted hardest for came back clean at the point they were aimed at. Two carve-outs, both below:
the `kind` premise ruling 5 rests on is wrong (should-fix), and the answer-writing path is sound
while the client that would reach it never surfaces the ask (blocker, task 10). (a) *Every* route
carries the token: `New` registers `/ws`
through `s.guard`, `routes()` guards all four `/api/*` paths, and only the static shell is open,
which ruling 11 states on purpose. The ban is correct for all three of its tests (window filter,
lift at 60 s, per-address), and it holds against a *good* token during a ban. (b) `--tls off` is
refused off loopback on the only path that reaches it: `RequireLoopback` is inside
`TLSOptions.Resolve()`'s `TLSOff` case and `Serve` resolves before it listens, with a named test
covering `:8443`, `0.0.0.0`, a tailnet address and `[::]`. (c) The allow/deny path is sound.
`AskChannel.Answer` reads `asks/<SafeID>.json` first, refuses when the file is missing, when the
nonce is missing, and when the decision is not `allow` or `deny`, writes `nonce` from the ask,
creates `answers/` at `0700`, writes the file at `0600`, and lands it by rename so the answer's
mtime is later than the ask's. That is exactly what `src/opendaisugi/ask.py:237-245` honours. **A
forged allow is not possible through this path.** (d) The CA hand-off serves only the CA:
`CACertHandler` is a bare `HandlerFunc` wired straight into `http.Server.Handler` with no mux, and
it 404s every path that is not `/ca.crt`. The chain itself is proved by a real handshake
(`TestARealTLSHandshakeSucceedsWithTheCAInTheRootPool`), not by a certificate that merely parses,
and the leaf stays inside the browsers' 398-day ceiling. (e) ntfy carries the pane label, the ask summary and a
click URL, never a grid or a transcript. The defects are elsewhere, and two of them are blockers.

- **BLOCKER — Task 10 `stateOf` reads a shape `pane.list` does not return, so every pane is
  `unknown` and Allow/Deny never appears.** `stateOf(paneInfo)` takes `paneInfo.state` and then
  reads `st.state`, `st.source`, `st.harness`, `st.ts`, `st.ask` off it, i.e. it expects the
  *nested* `PaneInfo.state: PaneStateEvent` of master §3.2 and spec-01. Plan 02's
  `handlePaneList` (plan-02-coppice-server.md, `func (s *Server) handlePaneList`) returns a **flat**
  row: `{"id","label","cwd","cmd","kind","harness","workspace","tab","closed","cols","rows",
  "state","source","detail"}` plus `"ask"` when one exists. So `st` is the *string* `"blocked"`,
  `st.state` is `undefined`, `stateChip` maps that to `unknown`, `sortPanes` never puts a blocked
  pane first, `paneRows` never sets `askSummary`, and `pane.js`'s `open()` calls
  `setAsk(stateOf(info))` and so hides the Allow/Deny box on the pane the operator just tapped.
  The box only ever appears if a *fresh* `state` event happens to arrive after the tap, and a gate
  `blocked` is one shot. The plan is inconsistent with itself here too: `pane.js`'s `onEvent` calls
  `stateOf({ state: msg })` to re-nest a flat wire event. Fix: both shapes are flat and use the
  same field names, so make `stateOf` read them directly — `state`, `source`, `harness`, `ts`,
  `ask` off the object it is given — and change `onEvent` to call `stateOf(msg)` with no wrapper.
  Same bullet, second half: plan 02's `pane.list` row carries **no `ts`**, so after this fix
  `paneRows`' `age` is still always `null` and the age spec-06 asks for on a roster card never
  paints on first load. Either plan 02 adds `"ts": ev.TS` to the row (preferred, one line beside
  `row["detail"]`), or task 10 states that the age is blank until the pane emits a live event.
  Add a `roster.test.mjs` case that feeds a real flat `pane.list` row and asserts
  `state === 'blocked'` and a non-empty `askSummary`. — applied in Task 10 step 1 (the fixture), step 2 (the roster and pane cases), step 5 (`paneRows`) and step 6 (`stateOf`, `askFor`, `onEvent`); the `ts` line in Task 0 step 3.

- **BLOCKER — Task 2 `Call` reports a server refusal as success, and Task 4 `handlePanes` turns
  that into an empty roster.** `Call` returns `(msg, nil)` for any reply that echoes the id,
  including `{"id":…,"ok":false,"error":{…}}`. `handlePanes` then does
  `result, _ := msg["result"].(map[string]any); if result == nil { result = map[string]any{"panes":
  []any{}} }` and writes **200** with an empty list, so the PWA paints "No panes yet. Tap New to
  start one." on top of a `no_such_workspace` or an `internal` error. That is the move master §3.1
  forbids in as many words ("`unknown` is what the floor shows when it has *no* source — it is
  never dressed up as `idle`") and §5.1 names as the cost of guessing. The plan's own JS `rpc()`
  already checks `msg.ok === false` and rejects, so the same protocol is read two ways in one
  package. Fix at `Call`, which covers both callers: after matching the id, return an error when
  `msg["ok"] != true`, carrying `msg["error"]["code"]` and `["message"]`. `handlePanes` then
  reaches its existing `writeErr(w, http.StatusBadGateway, …)` branch and the roster shows the
  refusal. `Publisher.Watch`'s `refresh()` gets the same benefit for free. Add
  `TestCallReturnsAnErrorWhenTheServerRefuses` beside the existing `Call` tests. — applied in Task 2 step 2 (the test) and step 4 (`Call` plus `RefusedError`), and Task 4 step 2 (`TestAPIPanesSurfacesARefusalInsteadOfAnEmptyRoster`) and step 5 (`handlePanes`).

- **SHOULD-FIX — Task 5 `CADir.Init` silently replaces the CA the phone already trusts.** `Init`
  treats *every* `loadCA()` failure as "there is no CA yet" and calls `createCA`, which `writePEM`s
  over `ca.crt` and `ca.key`. `loadCA` fails on an absent file, an unreadable file, a non-PEM file,
  an unparseable certificate and a non-ECDSA key. So one corrupt byte in `ca.key` makes the next
  `cert init` mint a new root with no message, and ruling 7's promise ("the phone never has to
  install a CA twice") is broken exactly when the operator can least afford it.
  `TestReInitKeepsTheSameCAAndReissuesOnlyTheLeaf` only walks the happy path. Fix: `Init` must
  distinguish the two cases. Create only when `ca.crt` is absent; when it is present but will not
  load, return an error naming the bad file and telling the operator to move the `ca` directory
  aside if they really want a new root. Add
  `TestInitRefusesRatherThanReplacingACAItCannotRead`. — applied in Task 5 step 1 (the test) and step 3 (`Init` creates only when `ca.crt` is absent).

- **SHOULD-FIX — Task 8 has no `--gate-root`, so a non-default data directory makes every
  Allow and Deny a 409.** `webServeCommand` hardcodes `Gate: web.AskChannel{Root:
  web.DefaultGateRoot()}` and `DefaultGateRoot` hardcodes `~/.opendaisugi/gate`. The real root is
  `<data_dir>/gate`, and `data_dir` is a configurable field (`src/opendaisugi/config.py:27`); the
  terminal view reads it that way (`src/opendaisugi/tui_sessions.py:302`,
  `self.app.data_dir / "gate"`). The failure is closed, not open — the answer file lands where
  nothing reads it and `wait_answer` times out into a deny — but the operator sees a button that
  never works. Fix: add `--gate-root` to `web serve` with `DefaultGateRoot()` as the default,
  carry it in `Config` as `gate_root`, and name the resolved path in the startup log line. — applied in Task 8 step 1 (`TestGateRootIsConfigurableAndSaved`), step 3 (`Config.GateRoot`, `Serve` resolves and logs it) and step 4 (the flag).

- **SHOULD-FIX — Task 8 `--forget` rewrites the whole config from flag defaults.**
  `webServeCommand` builds `cfg` from the flags and only then handles `--forget`, so
  `coppice web serve --forget` saves `web.json` with `Enabled: false` **and** every ntfy setting
  reset to its flag default. An operator who ran `--persist` with `--ntfy` loses that
  configuration by turning autostart off. Fix: for `--forget`, load the saved config, set
  `Enabled = false`, save that. Add a test that `--persist` with ntfy followed by `--forget`
  followed by `--persist` still carries the ntfy URL. — applied in Task 8 step 1 (`TestForgetKeepsEverySettingExceptEnabled`) and step 4 (`--forget` loads the saved config).

- **SHOULD-FIX — Task 8 `TestAutoStartDoesNothingWhenTheConfigIsAbsentOrDisabled` asserts
  nothing.** It calls `AutoStart`, checks `err == nil`, and calls `stop()`. It never checks that no
  listener came up, which is the whole claim in its name, so it passes on the bug it exists to
  catch. Fix: put a fixed loopback port in the disabled config, then assert nothing is listening on
  it after a short wait, and assert the same for the absent-config case. — applied in Task 8 step 1 (`assertNothingListening`, plus an enabled case so the port is proved bindable).

- **SHOULD-FIX — Task 9 `connect()` retries a rejected token forever and bans the operator's own
  phone.** The `close` handler is `setTimeout(connect, 2000)` with no backoff and no attention to
  *why* the socket closed. A stale token therefore produces a 401 every 2 s: three inside six
  seconds trips `BanFailures`, the address serves 60 s of 429, and the fourth retry after the ban
  lifts re-bans it immediately. The operator then pastes the correct token, reloads, and still gets
  429 with "Reconnecting." on screen and nothing that says why. Fix: on `close`, probe
  `GET /api/token/check` once; on 401 stop retrying and show "That token is not accepted. Run
  coppice web token and scan the QR again."; on 429 wait 60 s; otherwise back off from 2 s to a
  cap. (`docs/how-to/phone.md`'s troubleshooting row also quotes "Not connected", which is the
  `rpc` message, not the string this path shows.) — applied in Task 9 step 4 (`afterClose`) and Task 13 step 1 (the troubleshooting table now quotes the strings this path shows).

- **SHOULD-FIX — Task 11 ships a Settings control that does nothing, which ruling 10 forbids two
  tasks earlier.** `settings.js` writes `localStorage['coppice.url']` and reads it back into the
  field, but nothing else ever reads it: `connect()` uses `wsUrl(location.origin)` and `api()`
  uses relative paths. The **Server** field is decoration. Master §3.5 is the rule the plan itself
  cites to refuse `--web-push`. Fix: either make the field real (store an origin and have
  `connect()`/`api()` use it, which also needs CORS on the server, so it is not free), or delete
  the field and its label and say in the how-to that the app always talks to the origin it was
  served from. Deleting is the cheaper honest answer. — applied in Task 9 step 4 (the field is gone from `index.html`), Task 11 step 4 (`mountSettings` shows the origin as a fact) and Task 13 step 1.

- **SHOULD-FIX — Ruling 5's stated reason is wrong on both contracts it cites; the conclusion
  survives on a different one.** Ruling 5 says "`pane.list` (§3.2 `PaneInfo`) carries no pane
  `kind`". Two errors. First, on the question of which document is right: **spec-01 is right.**
  Master §3.2's `# id, label, cwd, cmd, state: PaneStateEvent|None` is an illustrative gloss on a
  Protocol signature, not a field list; spec-01 spells the dataclass out as
  `PaneInfo: ref, label, cwd, cmd, kind: str, state`. Master §3.2 is incomplete, not contradictory,
  and should be read that way. Second, and more to the point, the PWA never touches §3.2's Python
  `PaneBackend` at all — it reads §3.3's socket `pane.list`, and plan 02's `handlePaneList` row
  carries `"kind": string(p.Kind)` explicitly. So `kind` is available to the client. The ruling's
  *conclusion* (Prompt is `agent.prompt` for a pane of either kind) is still correct, on plan 02's
  own grounds: its `agent.prompt` test says "A shell pane is not an agent, but agent.prompt is
  defined as 'send text…'". Fix: rewrite ruling 5 to cite that semantic, drop the claim about a
  missing `kind`, and keep `pane.test.mjs`'s "whatever the pane is" cases as they are. — applied in the Rulings section (ruling 5 rewritten), Task 10 step 2 (the test comment) and step 6 (the module comment).

- **NOTE — Task 4's `safe_id_cases.json` has two wrong expected values.** Step 1 tells you to
  verify against the real `_safe` and fix, which is the right instruction; here are the two so it
  can be done without a second run. `"../../etc/passwd"` produces `_.._etc_passwd`, not
  `_.._.._etc_passwd` (the regex replaces each `/` with one `_`, then `.strip(".")` removes the
  leading `..`). The 136-character case produces 128 characters ending `…0123456789012345678901234567`,
  not the 130 characters written. Every other case is correct as written. The Go port's order
  (replace, `Trim(".")`, cut to 128, then `"none"`) matches `src/opendaisugi/ask.py:42` exactly, and
  both sides produce pure ASCII before the cut, so byte-slicing at 128 in Go equals character-
  slicing at 128 in Python. — applied in Task 4 step 1 (both values corrected against the real `_safe`).

- **NOTE — Task 6's `/../ca.key` case does not test what it says.** `http.Get(ts.URL + "/../ca.key")`
  resolves the reference against the base URL in the *client* and sends `GET /ca.key`, so the
  traversal never reaches the handler. The handler is safe anyway, because it compares
  `r.URL.Path` for exact equality with `/ca.crt`. To test the real thing, dial the listener and
  write the request line by hand, or drop the case rather than let it claim a property it does not
  check. — applied in Task 6 step 1 (`rawRequestPath` dials the listener and writes the request line).

- **NOTE — Task 7 can publish more than the how-to admits.** `OnState` falls back to `ev.Detail`
  when there is no ask summary, and master §3.1 gives `detail` as free text up to 200 characters
  (`verdict=deny clause=shell.deny[2]`). Task 13's "What leaves the box" section says only "the
  pane label and the ask summary go to the ntfy server you named". Either add `detail` to that
  sentence or drop the fallback and publish the fixed "blocked, and the gate gave no detail". — applied in Task 13 step 1 ("What leaves the box" now names `detail` and gives an example).

- **NOTE — ruling 1's unrewritten pipe hands a token holder the whole socket, and nothing says
  so.** `handleWS` copies every line through untouched, so a browser with the token can issue any
  §3.3 command, including `pane.create` with arbitrary `cmd_argv` in an arbitrary `cwd` (code
  execution as the operator) and `pane.report_state` with `source: "gate"` (blanking a real
  blocked state). Plan 02's `handleReportState` comment — "Everything else the caller says about
  its own source is taken at face value, because the callers are the gate and our own adapters" —
  stops being true the moment this plan puts that socket on a tailnet. This is the intended
  design and spec-06 says the token is the inner wall, so it is not a defect. It is undocumented.
  Add one sentence to ruling 1 and one line to `docs/how-to/phone.md`: the token is equivalent to
  a shell on the box, so treat it like an SSH key and rotate it with `coppice web token --rotate`
  if a phone is lost. — applied in the Rulings section (ruling 1) and Task 13 step 1 ("Treat the token like an SSH key").

- **NOTE — Task 0 step 3 needs no plan-02 edit.** Plan 02's `handleAttach` already reads
  `cols`/`rows` with `if cols, okc := r.Int("cols"); okc { … }` and resizes only when both are
  present and positive, which is exactly ruling 4's requirement. Take the "record that in PINS.md
  and move on" branch; do not add `attach_nosize_test.go` to plan 02 unless the helper names it
  assumes already exist. — applied in Task 0 step 3 (the attach test is gone; the step now
  confirms and records, and spends its one plan-02 edit on the `ts` line instead).

Re-review 2026-09-08: 9 of 9 verified applied; open: none.

## Global Constraints

Copied verbatim from master spec §4. Every task's requirements implicitly include this section.

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

## Rulings this plan makes

These are decisions the spec left open. They bind every task below. Each is stated so a reviewer can reject the ruling rather than discover it in the code.

1. **The websocket is a socket client, not a second protocol.** `/ws` opens one unix-socket connection per browser connection and copies JSONL lines through in both directions with no rewriting. "Parity with the unix socket" is therefore structural, and the parity test asserts byte identity in both directions. The consequence is deliberate and must be said out loud: because nothing is rewritten, whoever holds the token can issue any §3.3 command, including `pane.create` with any argv in any directory. **The token is equivalent to a shell on the box.** Treat it like an SSH key, and rotate it with `coppice web token --rotate` if a phone is lost. `docs/how-to/phone.md` says the same thing to the operator.
2. **Everything upstream goes through one `Dialer` interface.** `internal/web` never imports plan 02's `internal/server`, `internal/proto`, or `internal/pane`. It talks to a socket. This keeps the whole plan buildable and testable against a fake JSONL server.
3. **Browsers cannot set `Authorization` on a WebSocket.** The token therefore also travels as a `Sec-WebSocket-Protocol` offer of `daisugi.bearer.<token>`, alongside the plain name `daisugi.v1`. The server negotiates only `daisugi.v1`, so the response never echoes the token. Tokens are `base64.RawURLEncoding` (no `=` padding) precisely so every character is a legal protocol-token character.
4. **The phone never resizes a pane.** It attaches without a size and zooms instead. A phone that resized the pane would wreck the same pane in the operator's terminal. Task 0 makes `cols`/`rows` optional on `pane.attach` if plan 02 required them.
5. **Prompt is `agent.prompt`, Steer is `pane.send_text`.** Master §3.3 has no `agent.steer`, so steering is raw text with no turn boundary. Prompt is `agent.prompt` on a pane of either kind, on plan 02's own reading of the verb: its `agent.prompt` test says "a shell pane is not an agent, but `agent.prompt` is defined as send text". The client is not choosing between two commands and does not need to know the pane's kind to pick one. (`kind` *is* on the wire, in plan 02's `pane.list` row and in spec-01's `PaneInfo` dataclass; master §3.2's field gloss is incomplete, not contradictory. The New screen sends `kind` because `pane.create` takes it.)
6. **Allow and deny go through the gate's own file protocol.** §3.3 has no ask-answering command, so the web package writes `<data dir>/gate/answers/<id>.json` exactly as `src/opendaisugi/ask.py` defines it, echoing the nonce from the ask. `SafeID` is a byte-exact port of `ask.py`'s `_safe`, held to it by a fixture file both languages read.
7. **The local CA uses ECDSA P-256, and `cert init` is re-runnable.** P-256 keeps the CA PEM near 600 bytes, which is a QR a phone can actually scan. Re-running `cert init` after the box changes address reuses the existing CA and reissues only the leaf, so the phone never has to install a CA twice.
8. **The CA reaches the phone over plain HTTP on a second port.** A phone that trusts nothing cannot fetch the CA over the HTTPS the CA is for. `coppice web serve --tls localca` runs a second listener that serves exactly `GET /ca.crt` and nothing else, and the terminal QR encodes that URL. `--qr pem` encodes the certificate itself for anyone who prefers it.
9. **`--tls off` exists and refuses any address that is not loopback.** It is how the Playwright pass gets a real secure context (`http://127.0.0.1` is one; HTTPS with an untrusted certificate is not, and service workers do not register there). Refusing non-loopback is a fail-closed test, not a convenience.
10. **Web Push is not built.** `--web-push` exits 1 with a message that names ntfy. Master §3.5 forbids a control that does nothing; a flag that teaches is honest, a flag that silently no-ops is not. `docs/feature-status.md` carries the same claim.
11. **Static assets carry no token.** The shell must load before the operator has pasted anything. Every `/api/*` path and the `/ws` upgrade carry one.

## File map

```
harness/coppice/
  go.mod                                    Modify: + coder/websocket, + qrterminal/v3
  PINS.md                                   Modify: + a "Web / phone" section (task 0)
  internal/web/token.go                     the bearer token on disk
  internal/web/ban.go                       three bad tokens in a minute is a minute's ban
  internal/web/upstream.go                  Dialer, Session, Call: the socket client
  internal/web/server.go                    Options, Server, the auth guard, the mux
  internal/web/ws.go                        /ws: one websocket = one socket connection
  internal/web/api.go                       /api/token/check, /api/panes, /api/ask/answer, /api/push/test
  internal/web/ask.go                       SafeID + AskChannel: the gate's answer file
  internal/web/tls.go                       four TLS sources and the 14-day expiry warning
  internal/web/localca.go                   CA and leaf from crypto/x509
  internal/web/cahttp.go                    the plain-HTTP /ca.crt hand-off
  internal/web/push.go                      ntfy on every merged transition to blocked
  internal/web/config.go                    web.json, Serve, AutoStart
  internal/web/static.go                    embed + the static handler
  internal/web/static/                      the PWA (index.html, app.js, grid.js, ...)
  internal/web/static/_tests/               node --test unit tests (the _ keeps them out of embed)
  internal/web/static/_icongen/main.go      writes the two PNG icons (run once)
  cmd/coppice/web.go                        coppice web serve|token|cert
  testdata/phone/                           the checked-in 360x780 screenshots
scripts/phone_smoke.py                      the smoke orchestrator
scripts/phone_smoke.mjs                     the Playwright driver
tests/test_ask_safe_id_conformance.py       Python half of the SafeID fixture check
docs/how-to/phone.md                        the setup, both cert paths, the exposure note
```

---

### Task 0: Prerequisites, pins, and the facts this plan is not allowed to guess

**Files:**
- Create: `harness/coppice/internal/web/pins_test.go`
- Create: `harness/coppice/internal/web/doc.go`
- Modify: `harness/coppice/PINS.md` (created by plan 02; append a new section at the end)
- Modify: `harness/coppice/internal/server/` (created by plan 02; one line in `handlePaneList`, one test)

**Interfaces:**
- Consumes: nothing.
- Produces: the recorded facts every later task reads. `harness/coppice/PINS.md` gains a `## Web / phone (plan 06)` section holding: the websocket module path and version, the qrterminal module path and version, the `tailscale cert` flag names, the frame `attrs` bit encoding, the **flat** `pane.list` row shape, the flat `state` event shape, and the refusal shape. Tasks 2, 4, 7 and 10 read those three shapes from here rather than from memory. `internal/web/doc.go` declares `package web` so the package exists for later tasks. Plan 02's `pane.list` row gains `ts`.

- [ ] **Step 1: Check the host and stop with a teaching message if it is not ready**

Run this. Every line must pass. Where a line fails, the message tells you what to do; do that first, or mark this plan's live tests skipped for the stated reason.

```bash
cd /mnt/tera/working/programming/openDaisugi

go version | grep -q 'go1\.2[5-9]' \
  || echo "STOP: Go 1.25 or newer is required. Install it, then re-run."

test -f harness/coppice/go.mod \
  || echo "STOP: harness/coppice does not exist. Run plan 02 (spec-02-coppice-server.md) first."

( cd harness/coppice && go test ./... >/dev/null 2>&1 ) \
  || echo "STOP: harness/coppice tests are not green. Fix plan 02 before starting plan 06."

node --version | grep -qE '^v(2[2-9]|[3-9][0-9])' \
  || echo "SKIP-REASON: node 22+ is absent. Tasks 10 to 12 mark their JS and Playwright tests skipped."

command -v tailscale >/dev/null \
  || echo "SKIP-REASON: tailscale is absent. The tailscale TLS source is documented but not exercised on this box."

command -v magick >/dev/null \
  || echo "SKIP-REASON: ImageMagick is absent. Task 12 reads the per-size PNGs instead of a contact sheet."
```

Write the SKIP-REASON lines you saw into the commit message for this task. A later task that says "skip with reason" quotes them.

- [ ] **Step 2: Record the upstream facts in `PINS.md`**

Gather the four facts. Do not guess any of them; each command below produces the value.

```bash
cd /mnt/tera/working/programming/openDaisugi/harness/coppice

# 1 + 2: the two module versions this plan pins.
go list -m -versions github.com/coder/websocket | tr ' ' '\n' | tail -1
go list -m -versions github.com/mdp/qrterminal/v3 | tr ' ' '\n' | tail -1

# 3: the tailscale cert flags. If tailscale is absent, record the documented
#    flags and mark them unverified, naming the source URL.
tailscale cert --help 2>&1 | head -30 || echo "tailscale absent"

# 4: the attrs encoding a frame cell carries, from plan 02's grid.
grep -rn 'attrs\|Attrs' internal/pane/grid.go | head -20
```

Append this section to `harness/coppice/PINS.md`, filling in the values you just read. Where a value came from documentation rather than from this box, say so on the line.

```markdown
## Web / phone (plan 06)

| What | Value | How it was fixed |
|---|---|---|
| websocket library | `github.com/coder/websocket` v1.8.15 | Zero dependencies, MIT, maintained successor to nhooyr/websocket. `Accept` refuses cross-origin by default, which is the CSRF wall we want. |
| terminal QR | `github.com/mdp/qrterminal/v3` v3.2.1 | Named by spec-06. `GenerateWithConfig` with `HalfBlocks: true` fits a phone-scannable code in a terminal. |
| `tailscale cert` flags | `--cert-file=<path> --key-file=<path> <name>.<tailnet>.ts.net` | `tailscale cert --help` on this box. Verified: yes/no. Source when unverified: https://tailscale.com/kb/1080/cli |
| Let's Encrypt validity | 90 days, renewal is the operator's | https://tailscale.com/kb/1153/enabling-https |
| frame cell `attrs` | bold 1, italic 2, underline 4, inverse 8 | Read from `internal/pane/grid.go`. If that file did not define them, plan 06 defines them here and plan 02 adopts these values. |
| `pane.attach` size | `cols` and `rows` are optional; absent means "attach, do not resize" | Ruling 4. `handleAttach` already reads them with `r.Int("cols")` and resizes only when both are present and positive. |
| `pane.list` row shape | **flat**: `id, label, cwd, cmd, kind, harness, workspace, tab, closed, cols, rows, state, source, detail`, plus `exit_code` when the process exited, plus `ask` when the gate holds one, plus `ts` after step 3 | Read from `handlePaneList` in `internal/server`. The state is a **string on the row**, not a nested `PaneStateEvent`. Master §3.2's `state: PaneStateEvent|None` describes the Python `PaneBackend`, not this wire. |
| `state` event shape | `{"event":"state", ...PaneStateEvent...}` flattened: `v, ts, session_id, harness_session_id, harness, pane, state, source, ask, detail` | Read from `stateEvent` in `internal/server`. Same field names as a `pane.list` row, so one reader serves both. |
| refusal shape | `{"id":…,"ok":false,"error":{"code":…,"message":…}}` | `proto.ErrResp`. `code` is the closed enum in spec-02. |
```

Replace the version numbers with whatever the two `go list` lines actually printed, and set "Verified: yes/no" honestly.

- [ ] **Step 3: Confirm `pane.attach` already accepts a missing size, and give `pane.list` a `ts`**

First, confirm the attach side. Plan 02's `handleAttach` reads the size with
`if cols, okc := r.Int("cols"); okc { ... }` and resizes only when both are present and positive,
which is exactly what ruling 4 needs. Check it, then record it in `PINS.md`. Do not add a test to
plan 02 for this; it already behaves.

```bash
cd /mnt/tera/working/programming/openDaisugi/harness/coppice
grep -n 'r.Int("cols")' internal/server/*.go
```

Second, the one line plan 02 owes this plan. Its `handlePaneList` builds a flat row and copies
`state`, `source`, `detail` and `ask` off the merged event, but not `ts`. Without it a roster card
can never show a pane's age on first load, which spec-06 asks for. Add one line beside
`row["detail"]` in `handlePaneList`:

```go
		if ev, ok := s.states.Current(p.ID); ok {
			row["state"] = ev.State
			row["source"] = ev.Source
			row["detail"] = ev.Detail
			row["ts"] = ev.TS
			if ev.Ask != nil {
				row["ask"] = ev.Ask
			}
		}
```

and this test beside plan 02's other `handlePaneList` tests, using that file's own helpers:

```go
// append to harness/coppice/internal/server/pane_list_test.go

// A roster card shows how long a pane has been in its state. The age has to
// come from somewhere, and the merged event is the only thing that knows it.
func TestPaneListCarriesTheEventTimestamp(t *testing.T) {
	s, pane := newServerWithPane(t)
	s.states.Report(proto.PaneStateEvent{
		V: 1, TS: 1757300000.5, SessionID: "s1", Harness: "claude-code",
		Pane: &pane, State: proto.StateWorking, Source: "gate",
	})
	row := firstPaneRow(t, s)
	if row["ts"] != 1757300000.5 {
		t.Fatalf("pane.list row ts is %v, want the merged event's 1757300000.5", row["ts"])
	}
}
```

Adapt `newServerWithPane` and `firstPaneRow` to plan 02's actual helper names; if it has none,
build the row through the same path its existing `handlePaneList` test uses.

- [ ] **Step 4: Write the failing pins test**

```go
// harness/coppice/internal/web/pins_test.go
package web

import (
	"os"
	"strings"
	"testing"
)

// PINS.md is the plan's memory. Every fact a later task reads is written
// there once, by task 0, from a command that printed it. A later task that
// guessed instead would be a plan defect, so this test fails the moment the
// record is missing.
func TestPinsRecordsEveryWebFact(t *testing.T) {
	raw, err := os.ReadFile("../../PINS.md")
	if err != nil {
		t.Fatalf("read PINS.md: %v", err)
	}
	pins := string(raw)
	if !strings.Contains(pins, "## Web / phone (plan 06)") {
		t.Fatal("PINS.md has no 'Web / phone (plan 06)' section")
	}
	for _, want := range []string{
		"github.com/coder/websocket",
		"github.com/mdp/qrterminal/v3",
		"--cert-file",
		"--key-file",
		"bold 1, italic 2, underline 4, inverse 8",
		"attach, do not resize",
		// The shape task 10's client reads. Getting this wrong makes every
		// pane show as unknown and hides Allow and Deny, so it is written
		// down once, from the handler, before any client code exists.
		"**flat**",
		"not a nested `PaneStateEvent`",
		`"error":{"code":`,
	} {
		if !strings.Contains(pins, want) {
			t.Errorf("PINS.md does not record %q", want)
		}
	}
}
```

- [ ] **Step 5: Run the test and watch it fail**

```bash
cd /mnt/tera/working/programming/openDaisugi/harness/coppice && go test ./internal/web/ -run TestPinsRecordsEveryWebFact -v
```

Expected: FAIL. Either the package does not build yet, or the section is missing.

- [ ] **Step 6: Create the package and finish the PINS section**

```go
// harness/coppice/internal/web/doc.go

// Package web serves the phone: an installable PWA, one websocket that is a
// coppice socket client, and ntfy push when a pane blocks.
//
// The socket coppice-server listens on is bound to the operator's uid and
// nothing else can reach it. HTTPS has no such wall, so this package adds
// two of its own: a bearer token on every request, and TLS from a
// certificate the phone already trusts. The tailnet or the LAN is the outer
// wall. Both, not either.
//
// Nothing here imports plan 02's server internals. Everything upstream goes
// through Dialer, so the whole package is testable against a fake JSONL
// server and a phone client is exactly the kind of client a terminal is.
package web
```

Write the `## Web / phone (plan 06)` section into `PINS.md` with the values from step 2.

- [ ] **Step 7: Run the test and watch it pass**

```bash
cd /mnt/tera/working/programming/openDaisugi/harness/coppice && go test ./internal/web/ -run TestPinsRecordsEveryWebFact -v
```

Expected: PASS.

- [ ] **Step 8: Commit**

```bash
cd /mnt/tera/working/programming/openDaisugi
git add harness/coppice/PINS.md harness/coppice/internal/web/doc.go harness/coppice/internal/web/pins_test.go harness/coppice/internal/server/
git commit -m "coppice/web: pin the phone's upstream facts before any code reads them

Seven facts decide how this package behaves and none of them are ours: the
two library versions, the flags tailscale cert takes, the bit values a frame
cell's attrs field uses, and the three wire shapes the client reads. The
shapes matter most. A pane.list row is flat, with the state as a string on
the row, not the nested PaneStateEvent master 3.2 describes for the Python
backend; a client that read the wrong one would show every pane as unknown
and hide Allow and Deny. Recording all seven from commands and handlers that
printed them keeps every later task from guessing. pane.list also gains ts,
because a roster card cannot show an age it was never told."
```

---

### Task 1: The bearer token, and a minute's ban for the address that guesses

**Files:**
- Create: `harness/coppice/internal/web/token.go`
- Create: `harness/coppice/internal/web/ban.go`
- Test: `harness/coppice/internal/web/token_test.go`
- Test: `harness/coppice/internal/web/ban_test.go`

**Interfaces:**
- Consumes: `package web` (task 0).
- Produces:
  - `const TokenBytes = 32`
  - `var ErrNoToken error`
  - `type TokenStore struct{ Path string }`
  - `func DefaultTokenPath() string`
  - `func (s TokenStore) Load() (string, error)`
  - `func (s TokenStore) Mint() (string, error)`
  - `func (s TokenStore) Verify(candidate string) bool`
  - `const BanFailures = 3`, `const BanWindow = time.Minute`, `const BanDuration = time.Minute`
  - `type Banlist struct{ ... }`
  - `func NewBanlist(now func() time.Time) *Banlist`
  - `func (b *Banlist) Banned(addr string) bool`
  - `func (b *Banlist) Fail(addr string)`

- [ ] **Step 1: Write the failing tests**

```go
// harness/coppice/internal/web/token_test.go
package web

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func newStore(t *testing.T) TokenStore {
	t.Helper()
	return TokenStore{Path: filepath.Join(t.TempDir(), "coppice", "web-token")}
}

// The failure this names: a box with no token file must not be an open box.
func TestVerifyRefusesEverythingWhenNoTokenHasBeenMinted(t *testing.T) {
	s := newStore(t)
	if s.Verify("anything") {
		t.Fatal("an absent token file authorized a request")
	}
	if s.Verify("") {
		t.Fatal("an empty candidate authorized a request")
	}
}

// The failure this names: an empty token file must not match an empty header.
func TestVerifyRefusesWhenTheTokenFileIsEmpty(t *testing.T) {
	s := newStore(t)
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.Path, []byte("   \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s.Verify("") || s.Verify("   ") {
		t.Fatal("an empty token file authorized a request")
	}
	if _, err := s.Load(); err != ErrNoToken {
		t.Fatalf("Load on an empty file returned %v, want ErrNoToken", err)
	}
}

func TestMintWritesAPrivateFileInAPrivateDirectory(t *testing.T) {
	s := newStore(t)
	if _, err := s.Mint(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode is %o, want 600", fi.Mode().Perm())
	}
	di, err := os.Stat(filepath.Dir(s.Path))
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("token directory mode is %o, want 700", di.Mode().Perm())
	}
}

// A token also rides a Sec-WebSocket-Protocol header, where "=" is illegal.
// Raw base64url keeps every character legal there.
func TestMintedTokenIsBase64URLWithNoPadding(t *testing.T) {
	s := newStore(t)
	tok, err := s.Mint()
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`).MatchString(tok) {
		t.Fatalf("token %q is not 43 characters of unpadded base64url", tok)
	}
}

func TestMintRotatesAndTheOldTokenStopsWorking(t *testing.T) {
	s := newStore(t)
	first, err := s.Mint()
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Mint()
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("Mint returned the same token twice")
	}
	if s.Verify(first) {
		t.Fatal("the rotated-out token still authorizes")
	}
	if !s.Verify(second) {
		t.Fatal("the current token does not authorize")
	}
}
```

```go
// harness/coppice/internal/web/ban_test.go
package web

import (
	"testing"
	"time"
)

func fixedClock(t *time.Time) func() time.Time { return func() time.Time { return *t } }

func TestBanlistBansAfterThreeBadTokensInOneMinute(t *testing.T) {
	now := time.Unix(1_757_300_000, 0)
	b := NewBanlist(fixedClock(&now))
	b.Fail("10.0.0.7")
	now = now.Add(10 * time.Second)
	b.Fail("10.0.0.7")
	if b.Banned("10.0.0.7") {
		t.Fatal("two failures banned an address")
	}
	now = now.Add(10 * time.Second)
	b.Fail("10.0.0.7")
	if !b.Banned("10.0.0.7") {
		t.Fatal("three failures inside a minute did not ban the address")
	}
	if b.Banned("10.0.0.8") {
		t.Fatal("the ban leaked to another address")
	}
}

func TestBanlistForgetsFailuresOlderThanAMinute(t *testing.T) {
	now := time.Unix(1_757_300_000, 0)
	b := NewBanlist(fixedClock(&now))
	b.Fail("10.0.0.7")
	b.Fail("10.0.0.7")
	now = now.Add(61 * time.Second)
	b.Fail("10.0.0.7")
	if b.Banned("10.0.0.7") {
		t.Fatal("failures a minute apart accumulated into a ban")
	}
}

func TestBanLiftsAfterSixtySeconds(t *testing.T) {
	now := time.Unix(1_757_300_000, 0)
	b := NewBanlist(fixedClock(&now))
	for i := 0; i < BanFailures; i++ {
		b.Fail("10.0.0.7")
	}
	now = now.Add(59 * time.Second)
	if !b.Banned("10.0.0.7") {
		t.Fatal("the ban lifted early")
	}
	now = now.Add(2 * time.Second)
	if b.Banned("10.0.0.7") {
		t.Fatal("the ban did not lift after a minute")
	}
}
```

- [ ] **Step 2: Run the tests and watch them fail**

```bash
cd /mnt/tera/working/programming/openDaisugi/harness/coppice && go test ./internal/web/ -run 'Token|Ban' -v
```

Expected: FAIL, `undefined: TokenStore`, `undefined: NewBanlist`.

- [ ] **Step 3: Write `token.go`**

```go
// harness/coppice/internal/web/token.go
package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// TokenBytes is the entropy behind one web token. Encoded with
// base64.RawURLEncoding it is 43 characters, and every one of them is a legal
// RFC 7230 token character. That matters: a browser cannot set an
// Authorization header on a WebSocket, so the same string also rides a
// Sec-WebSocket-Protocol offer, where "=" padding would be illegal.
const TokenBytes = 32

// ErrNoToken says nobody has minted a token on this box yet.
var ErrNoToken = errors.New("no web token")

// TokenStore is the one bearer token, 0600 inside a 0700 directory.
type TokenStore struct{ Path string }

// DefaultTokenPath is ~/.opendaisugi/coppice/web-token.
func DefaultTokenPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "web-token"
	}
	return filepath.Join(home, ".opendaisugi", "coppice", "web-token")
}

// Load returns the stored token. A missing, unreadable, or blank file is
// ErrNoToken, never an empty string a caller might compare against.
func (s TokenStore) Load() (string, error) {
	raw, err := os.ReadFile(s.Path)
	if err != nil {
		return "", ErrNoToken
	}
	tok := strings.TrimSpace(string(raw))
	if tok == "" {
		return "", ErrNoToken
	}
	return tok, nil
}

// Mint writes a fresh token and replaces any existing one.
func (s TokenStore) Mint() (string, error) {
	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	buf := make([]byte, TokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	tok := base64.RawURLEncoding.EncodeToString(buf)
	tmp, err := os.CreateTemp(dir, ".web-token-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return "", err
	}
	if _, err := tmp.WriteString(tok); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), s.Path); err != nil {
		return "", err
	}
	return tok, nil
}

// Verify compares in constant time and fails closed. No file, a blank file,
// or a blank candidate never authorizes.
func (s TokenStore) Verify(candidate string) bool {
	if candidate == "" {
		return false
	}
	stored, err := s.Load()
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(stored)) == 1
}
```

- [ ] **Step 4: Write `ban.go`**

```go
// harness/coppice/internal/web/ban.go
package web

import (
	"sync"
	"time"
)

const (
	// BanFailures bad tokens inside BanWindow from one address earn
	// BanDuration of silence. Spec-06: "three bad tokens from one address in
	// a minute is a 60 s ban".
	BanFailures = 3
	BanWindow   = time.Minute
	BanDuration = time.Minute
)

// Banlist counts bad tokens per client address. A ban holds even when a good
// token arrives during it: an address that just guessed three times is not
// trusted for a minute, and the operator's own phone can wait.
type Banlist struct {
	mu     sync.Mutex
	now    func() time.Time
	fails  map[string][]time.Time
	banned map[string]time.Time
}

func NewBanlist(now func() time.Time) *Banlist {
	if now == nil {
		now = time.Now
	}
	return &Banlist{now: now, fails: map[string][]time.Time{}, banned: map[string]time.Time{}}
}

// Banned reports whether addr is serving a ban, and clears one that expired.
func (b *Banlist) Banned(addr string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	until, ok := b.banned[addr]
	if !ok {
		return false
	}
	if !b.now().Before(until) {
		delete(b.banned, addr)
		return false
	}
	return true
}

// Fail records one bad token from addr.
func (b *Banlist) Fail(addr string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	recent := b.fails[addr][:0]
	for _, t := range b.fails[addr] {
		if now.Sub(t) < BanWindow {
			recent = append(recent, t)
		}
	}
	recent = append(recent, now)
	b.fails[addr] = recent
	if len(recent) >= BanFailures {
		b.banned[addr] = now.Add(BanDuration)
		delete(b.fails, addr)
	}
}
```

- [ ] **Step 5: Run the tests and watch them pass**

```bash
cd /mnt/tera/working/programming/openDaisugi/harness/coppice && go test ./internal/web/ -run 'Token|Ban' -v && go vet ./internal/web/
```

Expected: PASS, vet clean.

- [ ] **Step 6: Commit**

```bash
cd /mnt/tera/working/programming/openDaisugi
git add harness/coppice/internal/web/token.go harness/coppice/internal/web/ban.go harness/coppice/internal/web/token_test.go harness/coppice/internal/web/ban_test.go
git commit -m "coppice/web: the bearer token, and a minute's ban for the guesser

The unix socket is bound to the operator's uid and nothing else can reach it.
HTTPS has no such wall, so the phone needs one of its own. The token is 32
random bytes as unpadded base64url, because a browser cannot set an
Authorization header on a WebSocket and the same string has to survive a
Sec-WebSocket-Protocol offer. Verify fails closed on a missing or blank file,
and three bad tokens from one address buy that address a minute of silence."
```

---

### Task 2: The socket client every request rides on

**Files:**
- Create: `harness/coppice/internal/web/upstream.go`
- Test: `harness/coppice/internal/web/fakeserver_test.go`
- Test: `harness/coppice/internal/web/upstream_test.go`

**Interfaces:**
- Consumes: `package web` (task 0).
- Produces:
  - `const MaxLineBytes = 4 << 20`
  - `type Dialer interface{ Dial(ctx context.Context) (io.ReadWriteCloser, error) }`
  - `type UnixDialer struct{ Path string }` with `Dial`
  - `func DefaultSocketPath() string`
  - `type Session struct{ ... }`
  - `func Open(ctx context.Context, d Dialer) (*Session, error)`
  - `func (s *Session) Send(line []byte) error`
  - `func (s *Session) Lines() <-chan []byte`
  - `func (s *Session) Err() error`
  - `func (s *Session) Close() error`
  - `func Call(ctx context.Context, d Dialer, req map[string]any) (map[string]any, error)` — errors on `ok:false`
  - `type RefusedError struct{ Code, Message string }` with `Error() string`
  - test helper `func newFakeServer(t *testing.T, reply func(req map[string]any) []map[string]any) (*fakeServer, Dialer)`

- [ ] **Step 1: Write the fake JSONL server the whole plan tests against**

```go
// harness/coppice/internal/web/fakeserver_test.go
package web

import (
	"bufio"
	"encoding/json"
	"net"
	"path/filepath"
	"sync"
	"testing"
)

// fakeServer is a coppice-server that only speaks JSONL. Every test in this
// package runs against it, so the web package never needs plan 02's
// internals to be finished, and the parity claim ("the browser's bytes are
// the socket's bytes") is checkable at the byte level.
type fakeServer struct {
	mu       sync.Mutex
	received [][]byte // exactly the bytes each request line arrived as
	path     string
}

// newFakeServer listens on a unix socket and answers each request line with
// whatever reply returns. A nil reply answers nothing, which is what an
// event-only subscription looks like.
func newFakeServer(t *testing.T, reply func(req map[string]any) []map[string]any) (*fakeServer, Dialer) {
	t.Helper()
	f := &fakeServer{path: filepath.Join(t.TempDir(), "server.sock")}
	ln, err := net.Listen("unix", f.path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(conn, reply)
		}
	}()
	return f, UnixDialer{Path: f.path}
}

func (f *fakeServer) serve(conn net.Conn, reply func(map[string]any) []map[string]any) {
	defer conn.Close()
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), MaxLineBytes)
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		f.mu.Lock()
		f.received = append(f.received, line)
		f.mu.Unlock()
		var req map[string]any
		if json.Unmarshal(line, &req) != nil || reply == nil {
			continue
		}
		for _, msg := range reply(req) {
			body, _ := json.Marshal(msg)
			conn.Write(append(body, '\n'))
		}
	}
}

func (f *fakeServer) Received() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]byte, len(f.received))
	copy(out, f.received)
	return out
}

// echoOK answers any request with {"id":…,"ok":true,"result":{"cmd":<cmd>}}.
func echoOK(req map[string]any) []map[string]any {
	return []map[string]any{{"id": req["id"], "ok": true, "result": map[string]any{"cmd": req["cmd"]}}}
}
```

- [ ] **Step 2: Write the failing upstream tests**

```go
// harness/coppice/internal/web/upstream_test.go
package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSessionSendsOneLinePerMessage(t *testing.T) {
	f, d := newFakeServer(t, echoOK)
	s, err := Open(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Send([]byte(`{"id":"1","cmd":"server.status"}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case line := <-s.Lines():
		if !bytes.Contains(line, []byte(`"ok":true`)) {
			t.Fatalf("reply was %s", line)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no reply within 2 s")
	}
	got := f.Received()
	if len(got) != 1 || string(got[0]) != `{"id":"1","cmd":"server.status"}` {
		t.Fatalf("the server saw %q", got)
	}
}

// The failure this names: a full 120x40 frame is roughly 150 KB of JSON, and
// bufio.Scanner's default 64 KB buffer would cut it in half and hand the
// browser a broken line.
func TestSessionDoesNotTruncateAOneHundredKilobyteLine(t *testing.T) {
	big := strings.Repeat("x", 100*1024)
	_, d := newFakeServer(t, func(req map[string]any) []map[string]any {
		return []map[string]any{{"id": req["id"], "ok": true, "result": big}}
	})
	s, err := Open(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Send([]byte(`{"id":"1","cmd":"pane.read"}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case line := <-s.Lines():
		var msg map[string]any
		if err := json.Unmarshal(line, &msg); err != nil {
			t.Fatalf("the reply line did not survive whole: %v", err)
		}
		if msg["result"] != big {
			t.Fatal("the 100 KB payload came back changed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no reply within 5 s")
	}
}

func TestCallMatchesTheReplyByIdAndIgnoresEventsInBetween(t *testing.T) {
	_, d := newFakeServer(t, func(req map[string]any) []map[string]any {
		return []map[string]any{
			{"event": "state", "pane": "w1:p1", "state": "working"},
			{"id": "not-yours", "ok": true},
			{"id": req["id"], "ok": true, "result": map[string]any{"panes": []any{}}},
		}
	})
	msg, err := Call(context.Background(), d, map[string]any{"cmd": "pane.list"})
	if err != nil {
		t.Fatal(err)
	}
	if msg["ok"] != true {
		t.Fatalf("Call returned %v", msg)
	}
}

// The failure this names: a refusal returned as a success becomes an empty
// roster painted on top of a real error, which is exactly the guessing
// master spec 5.1 refuses. The code has to reach the caller intact.
func TestCallReturnsAnErrorWhenTheServerRefuses(t *testing.T) {
	_, d := newFakeServer(t, func(req map[string]any) []map[string]any {
		return []map[string]any{{"id": req["id"], "ok": false, "error": map[string]any{
			"code": "no_such_workspace", "message": `no workspace "w9". Run: coppice workspace list`,
		}}}
	})
	msg, err := Call(context.Background(), d, map[string]any{"cmd": "pane.list"})
	if err == nil {
		t.Fatal("Call reported a refusal as success")
	}
	if msg != nil {
		t.Fatalf("Call returned a body alongside the error: %v", msg)
	}
	var refused *RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("Call returned %T, want *RefusedError", err)
	}
	if refused.Code != "no_such_workspace" {
		t.Errorf("code is %q", refused.Code)
	}
	if !strings.Contains(refused.Message, "coppice workspace list") {
		t.Errorf("the server's teaching message was dropped: %q", refused.Message)
	}
}

// A refusal with no error body must still be an error, not a success with
// nothing in it.
func TestCallTreatsAnEmptyRefusalAsAnError(t *testing.T) {
	_, d := newFakeServer(t, func(req map[string]any) []map[string]any {
		return []map[string]any{{"id": req["id"], "ok": false}}
	})
	if _, err := Call(context.Background(), d, map[string]any{"cmd": "pane.list"}); err == nil {
		t.Fatal("an ok:false with no error body was reported as success")
	}
}

// The failure this names: with no server there is nothing to report, and a
// caller must get an error rather than an invented empty success.
func TestCallFailsWhenTheSocketIsNotThere(t *testing.T) {
	d := UnixDialer{Path: filepath.Join(t.TempDir(), "nothing.sock")}
	if _, err := Call(context.Background(), d, map[string]any{"cmd": "pane.list"}); err == nil {
		t.Fatal("Call succeeded with no server listening")
	}
}

func TestSessionCloseStopsTheReader(t *testing.T) {
	_, d := newFakeServer(t, echoOK)
	s, err := Open(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	select {
	case _, ok := <-s.Lines():
		if ok {
			t.Fatal("Lines yielded after Close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Lines did not close within 2 s of Close")
	}
}
```

- [ ] **Step 3: Run the tests and watch them fail**

```bash
cd /mnt/tera/working/programming/openDaisugi/harness/coppice && go test ./internal/web/ -run 'Session|Call' -v
```

Expected: FAIL, `undefined: Open`, `undefined: Call`, `undefined: UnixDialer`.

- [ ] **Step 4: Write `upstream.go`**

```go
// harness/coppice/internal/web/upstream.go
package web

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// MaxLineBytes bounds one JSONL line in either direction. A full 120x40 frame
// is roughly 150 KB of JSON, so bufio.Scanner's 64 KB default would split
// frames and hand the browser half a line. 4 MiB leaves room for a scrollback
// read as well.
const MaxLineBytes = 4 << 20

// Dialer is the only thing this package knows about coppice-server. Tests
// supply a fake; the binary supplies a unix socket.
type Dialer interface {
	Dial(ctx context.Context) (io.ReadWriteCloser, error)
}

// UnixDialer dials the coppice-server socket.
type UnixDialer struct{ Path string }

func (d UnixDialer) Dial(ctx context.Context) (io.ReadWriteCloser, error) {
	var dl net.Dialer
	return dl.DialContext(ctx, "unix", d.Path)
}

// DefaultSocketPath is master spec 3.3's path: the runtime dir when there is
// one, the data dir otherwise.
func DefaultSocketPath() string {
	if run := os.Getenv("XDG_RUNTIME_DIR"); run != "" {
		return filepath.Join(run, "coppice", "server.sock")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "coppice-server.sock"
	}
	return filepath.Join(home, ".opendaisugi", "coppice", "server.sock")
}

// Session is one client's connection to coppice-server. One websocket owns
// exactly one Session, which is what makes a browser client the same kind of
// client as a terminal one.
type Session struct {
	up    io.ReadWriteCloser
	lines chan []byte
	done  chan struct{}
	mu    sync.Mutex
	err   error
	once  sync.Once
}

func Open(ctx context.Context, d Dialer) (*Session, error) {
	up, err := d.Dial(ctx)
	if err != nil {
		return nil, err
	}
	s := &Session{up: up, lines: make(chan []byte, 64), done: make(chan struct{})}
	go s.read()
	return s, nil
}

func (s *Session) read() {
	defer close(s.lines)
	sc := bufio.NewScanner(s.up)
	sc.Buffer(make([]byte, 0, 64*1024), MaxLineBytes)
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		select {
		case s.lines <- line:
		case <-s.done:
			return
		}
	}
	s.setErr(sc.Err())
}

func (s *Session) setErr(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.mu.Unlock()
}

// Err is the reader's first error, if any.
func (s *Session) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Send writes one JSONL line. The newline is ours; the caller passes the
// object bytes exactly as they should reach the server.
func (s *Session) Send(line []byte) error {
	if len(line) > MaxLineBytes {
		return errors.New("that request line is too long")
	}
	out := make([]byte, 0, len(line)+1)
	out = append(out, line...)
	out = append(out, '\n')
	_, err := s.up.Write(out)
	return err
}

// Lines yields one element per JSONL line from the server, newline stripped.
// It closes when the connection ends.
func (s *Session) Lines() <-chan []byte { return s.lines }

func (s *Session) Close() error {
	var err error
	s.once.Do(func() {
		close(s.done)
		err = s.up.Close()
	})
	return err
}

var callSeq atomic.Uint64

// Call runs one request over a fresh connection and returns the reply that
// echoes its id. The /api handlers use it; the hot path is the websocket, so
// a connection per call is the right trade for a handler that runs rarely.
func Call(ctx context.Context, d Dialer, req map[string]any) (map[string]any, error) {
	s, err := Open(ctx, d)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	id := "api-" + strconv.FormatUint(callSeq.Add(1), 10)
	out := make(map[string]any, len(req)+1)
	for k, v := range req {
		out[k] = v
	}
	out["id"] = id
	body, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	if err := s.Send(body); err != nil {
		return nil, err
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, errors.New("coppice-server did not answer in 5 s")
		case line, ok := <-s.Lines():
			if !ok {
				return nil, errors.New("coppice-server closed the connection")
			}
			var msg map[string]any
			if json.Unmarshal(line, &msg) != nil {
				continue
			}
			if msg["id"] != id {
				continue
			}
			if ok, _ := msg["ok"].(bool); !ok {
				return nil, refusal(msg)
			}
			return msg, nil
		}
	}
}

// RefusedError is a coppice-server refusal, carried out to the caller with
// its closed-enum code intact.
type RefusedError struct {
	Code    string
	Message string
}

func (e *RefusedError) Error() string {
	if e.Code == "" {
		return "coppice-server refused the request"
	}
	return e.Code + ": " + e.Message
}

// refusal turns {"ok":false,"error":{"code","message"}} into an error.
// Returning that reply as a success is how a floor ends up showing an empty
// roster on top of a real failure, which is the guessing master spec 5.1
// exists to refuse.
func refusal(msg map[string]any) error {
	body, _ := msg["error"].(map[string]any)
	code, _ := body["code"].(string)
	message, _ := body["message"].(string)
	if code == "" && message == "" {
		return &RefusedError{Code: "internal", Message: "coppice-server refused and said nothing"}
	}
	return &RefusedError{Code: code, Message: message}
}
```

- [ ] **Step 5: Run the tests and watch them pass**

```bash
cd /mnt/tera/working/programming/openDaisugi/harness/coppice && go test ./internal/web/ -v && go vet ./internal/web/
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
cd /mnt/tera/working/programming/openDaisugi
git add harness/coppice/internal/web/upstream.go harness/coppice/internal/web/upstream_test.go harness/coppice/internal/web/fakeserver_test.go
git commit -m "coppice/web: one Dialer is everything the phone knows about the server

Routing the phone through the same unix socket a terminal uses makes parity
structural rather than a promise, and it keeps this package free of plan 02's
internals so it can be built and tested on its own. The scanner buffer is
4 MiB on purpose: a full 120x40 frame is about 150 KB of JSON and the 64 KB
default would hand the browser half a line. Call turns ok:false into an
error carrying the server's own code, because a refusal returned as a
success becomes an empty roster painted over a real failure, and guessing
like that is what master 5.1 exists to refuse."
```

---

### Task 3: The server, the guard, and `/ws`

**Files:**
- Create: `harness/coppice/internal/web/server.go`
- Create: `harness/coppice/internal/web/ws.go`
- Modify: `harness/coppice/go.mod` (add `github.com/coder/websocket` at the version task 0 pinned)
- Test: `harness/coppice/internal/web/ws_test.go`

**Interfaces:**
- Consumes: `TokenStore`, `Banlist`, `Dialer`, `Session`, `Open`, `MaxLineBytes` (tasks 1, 2).
- Produces:
  - `type Options struct { Dial Dialer; Tokens TokenStore; Log *slog.Logger; Now func() time.Time }`
  - `type Server struct{ ... }`
  - `func New(opts Options) (*Server, error)`
  - `func (s *Server) Handler() http.Handler`
  - `func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc`
  - `func clientAddr(r *http.Request) string`
  - `const WSSubprotocol = "daisugi.v1"`, `const WSBearerPrefix = "daisugi.bearer."`
  - `func bearerFrom(r *http.Request) string`

Later tasks add fields to `Options`: `Gate AskChannel` in task 4, `Push *Publisher` in task 7.

- [ ] **Step 1: Write the failing tests**

```go
// harness/coppice/internal/web/ws_test.go
package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func newTestServer(t *testing.T, d Dialer) (*httptest.Server, string) {
	t.Helper()
	store := TokenStore{Path: filepath.Join(t.TempDir(), "coppice", "web-token")}
	tok, err := store.Mint()
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{Dial: d, Tokens: store})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, tok
}

func wsAddr(base string) string { return "ws" + strings.TrimPrefix(base, "http") + "/ws" }

// The failure this names: an upgrade with no token must not become a socket.
func TestWSUpgradeWithoutATokenIsRefused(t *testing.T) {
	_, d := newFakeServer(t, echoOK)
	ts, _ := newTestServer(t, d)
	_, resp, err := websocket.Dial(context.Background(), wsAddr(ts.URL), nil)
	if err == nil {
		t.Fatal("an unauthenticated upgrade succeeded")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status was %v, want 401", resp)
	}
}

func TestWSUpgradeWithABearerHeaderSucceeds(t *testing.T) {
	_, d := newFakeServer(t, echoOK)
	ts, tok := newTestServer(t, d)
	h := http.Header{}
	h.Set("Authorization", "Bearer "+tok)
	c, _, err := websocket.Dial(context.Background(), wsAddr(ts.URL), &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatalf("upgrade with a good token: %v", err)
	}
	c.Close(websocket.StatusNormalClosure, "")
}

// A browser cannot set Authorization on a WebSocket, so the token rides a
// subprotocol offer. The negotiated protocol must be the safe name, never
// the one carrying the secret.
func TestWSUpgradeWithTheSubprotocolTokenDoesNotEchoTheToken(t *testing.T) {
	_, d := newFakeServer(t, echoOK)
	ts, tok := newTestServer(t, d)
	c, _, err := websocket.Dial(context.Background(), wsAddr(ts.URL), &websocket.DialOptions{
		Subprotocols: []string{WSSubprotocol, WSBearerPrefix + tok},
	})
	if err != nil {
		t.Fatalf("upgrade with a subprotocol token: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")
	if got := c.Subprotocol(); got != WSSubprotocol {
		t.Fatalf("negotiated %q, want %q", got, WSSubprotocol)
	}
}

// The failure this names: after three guesses the address must be silenced,
// not merely told "no" a fourth time.
func TestWSUpgradeFromABannedAddressIsRefusedWith429(t *testing.T) {
	_, d := newFakeServer(t, echoOK)
	ts, _ := newTestServer(t, d)
	for i := 0; i < BanFailures; i++ {
		websocket.Dial(context.Background(), wsAddr(ts.URL), &websocket.DialOptions{
			Subprotocols: []string{WSSubprotocol, WSBearerPrefix + "wrong"},
		})
	}
	_, resp, err := websocket.Dial(context.Background(), wsAddr(ts.URL), &websocket.DialOptions{
		Subprotocols: []string{WSSubprotocol, WSBearerPrefix + "wrong"},
	})
	if err == nil {
		t.Fatal("a banned address still upgraded")
	}
	if resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status was %v, want 429", resp)
	}
}

// The parity claim, at the byte level: what the browser sends is what the
// socket receives, and what the socket answers is what the browser reads.
func TestWSIsByteIdenticalToTheUnixSocketInBothDirections(t *testing.T) {
	f, d := newFakeServer(t, func(req map[string]any) []map[string]any {
		return []map[string]any{{"id": req["id"], "ok": true, "result": map[string]any{"pane": "w1:p1"}}}
	})
	ts, tok := newTestServer(t, d)
	c, _, err := websocket.Dial(context.Background(), wsAddr(ts.URL), &websocket.DialOptions{
		Subprotocols: []string{WSSubprotocol, WSBearerPrefix + tok},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	sent := `{"id":"7","cmd":"pane.create","cwd":"/repo","kind":"pty"}`
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, []byte(sent)); err != nil {
		t.Fatal(err)
	}
	typ, got, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if typ != websocket.MessageText {
		t.Fatalf("message type was %v, want text", typ)
	}
	var msg map[string]any
	if err := json.Unmarshal(got, &msg); err != nil {
		t.Fatalf("reply was not one JSON object: %v", err)
	}
	if msg["id"] != "7" {
		t.Fatalf("reply id was %v, want 7", msg["id"])
	}
	recv := f.Received()
	if len(recv) != 1 || string(recv[0]) != sent {
		t.Fatalf("the socket saw %q, want %q", recv, sent)
	}
}

// The failure this names: another page must not be able to open the
// operator's panes through the browser's own credentials.
func TestWSRejectsAnUpgradeFromAnotherOrigin(t *testing.T) {
	_, d := newFakeServer(t, echoOK)
	ts, tok := newTestServer(t, d)
	h := http.Header{}
	h.Set("Authorization", "Bearer "+tok)
	h.Set("Origin", "https://evil.example")
	_, resp, err := websocket.Dial(context.Background(), wsAddr(ts.URL), &websocket.DialOptions{HTTPHeader: h})
	if err == nil {
		t.Fatal("a cross-origin upgrade succeeded")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status was %v, want 403", resp)
	}
}

func TestWSClosingTheBrowserClosesTheSocket(t *testing.T) {
	_, d := newFakeServer(t, echoOK)
	ts, tok := newTestServer(t, d)
	c, _, err := websocket.Dial(context.Background(), wsAddr(ts.URL), &websocket.DialOptions{
		Subprotocols: []string{WSSubprotocol, WSBearerPrefix + tok},
	})
	if err != nil {
		t.Fatal(err)
	}
	c.Close(websocket.StatusNormalClosure, "")
	// The handler goroutine must return. Give it a moment, then assert the
	// server accepts a fresh connection, which it cannot do if the old one
	// wedged the handler.
	time.Sleep(200 * time.Millisecond)
	c2, _, err := websocket.Dial(context.Background(), wsAddr(ts.URL), &websocket.DialOptions{
		Subprotocols: []string{WSSubprotocol, WSBearerPrefix + tok},
	})
	if err != nil {
		t.Fatalf("a second connection after a close: %v", err)
	}
	c2.Close(websocket.StatusNormalClosure, "")
}
```

- [ ] **Step 2: Add the websocket dependency and run the tests to watch them fail**

```bash
cd /mnt/tera/working/programming/openDaisugi/harness/coppice
go get github.com/coder/websocket@v1.8.15   # use the version PINS.md records
go test ./internal/web/ -run TestWS -v
```

Expected: FAIL, `undefined: New`, `undefined: WSSubprotocol`.

- [ ] **Step 3: Write `server.go`**

```go
// harness/coppice/internal/web/server.go
package web

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// Options is everything the phone server needs. Dial is the only door to
// coppice-server; Tokens is the only wall in front of it.
type Options struct {
	Dial   Dialer
	Tokens TokenStore
	Log    *slog.Logger
	Now    func() time.Time
}

// Server is the HTTP surface: static assets with no token, /ws and /api with
// one.
type Server struct {
	opts Options
	bans *Banlist
	mux  *http.ServeMux
}

func New(opts Options) (*Server, error) {
	if opts.Dial == nil {
		return nil, errors.New("web: no dialer. Pass Options.Dial")
	}
	if opts.Tokens.Path == "" {
		return nil, errors.New("web: no token store. Pass Options.Tokens")
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	s := &Server{opts: opts, bans: NewBanlist(opts.Now), mux: http.NewServeMux()}
	s.mux.HandleFunc("/ws", s.guard(s.handleWS))
	return s, nil
}

func (s *Server) Handler() http.Handler { return s.mux }

// clientAddr is the host part of RemoteAddr. No supported path puts a proxy
// in front of this server, so a forwarding header is never trusted: honoring
// one would let a caller pick its own ban bucket.
func clientAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// guard fails closed. A banned address, a missing token, or a wrong token
// never reaches the handler, and every refusal is one log line.
func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		addr := clientAddr(r)
		if s.bans.Banned(addr) {
			s.opts.Log.Warn("web: refused a banned address", "addr", addr, "path", r.URL.Path)
			http.Error(w, "Too many bad tokens. Wait one minute.", http.StatusTooManyRequests)
			return
		}
		if !s.opts.Tokens.Verify(bearerFrom(r)) {
			s.bans.Fail(addr)
			s.opts.Log.Warn("web: refused a bad token", "addr", addr, "path", r.URL.Path)
			http.Error(w, "Bad token. Run coppice web token to see the current one.", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}
```

- [ ] **Step 4: Write `ws.go`**

```go
// harness/coppice/internal/web/ws.go
package web

import (
	"context"
	"net/http"
	"strings"

	"github.com/coder/websocket"
)

const (
	// WSSubprotocol is the only name the server ever negotiates.
	WSSubprotocol = "daisugi.v1"
	// WSBearerPrefix carries the token in a second subprotocol offer,
	// because the browser WebSocket API cannot set request headers.
	WSBearerPrefix = "daisugi.bearer."
)

// bearerFrom reads the token from Authorization, then from a
// Sec-WebSocket-Protocol offer. Terminal clients use the header; browsers
// use the offer.
func bearerFrom(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	for _, raw := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, part := range strings.Split(raw, ",") {
			part = strings.TrimSpace(part)
			if strings.HasPrefix(part, WSBearerPrefix) {
				return strings.TrimPrefix(part, WSBearerPrefix)
			}
		}
	}
	return ""
}

// handleWS gives one browser one socket connection and copies JSONL lines
// through untouched in both directions. Nothing here parses a request: the
// browser speaks the same protocol a terminal speaks, so parity is the shape
// of the code rather than a claim about it.
//
// Subprotocols lists only WSSubprotocol, so the handshake response never
// echoes the token back. Accept refuses a cross-origin upgrade by default,
// and that default stays: no other page may drive the operator's panes.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols: []string{WSSubprotocol},
	})
	if err != nil {
		s.opts.Log.Warn("web: upgrade refused", "err", err)
		return
	}
	defer c.CloseNow()
	c.SetReadLimit(MaxLineBytes)

	sess, err := Open(r.Context(), s.opts.Dial)
	if err != nil {
		s.opts.Log.Warn("web: coppice-server is not reachable", "err", err)
		c.Close(websocket.StatusInternalError, "coppice-server is not reachable")
		return
	}
	defer sess.Close()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	go func() {
		defer cancel()
		for line := range sess.Lines() {
			if err := c.Write(ctx, websocket.MessageText, line); err != nil {
				return
			}
		}
	}()

	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		if err := sess.Send(data); err != nil {
			return
		}
	}
}
```

- [ ] **Step 5: Run the tests and watch them pass**

```bash
cd /mnt/tera/working/programming/openDaisugi/harness/coppice && go test ./internal/web/ -v && go vet ./internal/web/
```

Expected: PASS. If `TestWSRejectsAnUpgradeFromAnotherOrigin` fails, `AcceptOptions.OriginPatterns` or `InsecureSkipVerify` has been set somewhere; remove it.

- [ ] **Step 6: Commit**

```bash
cd /mnt/tera/working/programming/openDaisugi
git add harness/coppice/go.mod harness/coppice/go.sum harness/coppice/internal/web/server.go harness/coppice/internal/web/ws.go harness/coppice/internal/web/ws_test.go
git commit -m "coppice/web: one websocket is one socket connection, nothing rewritten

The phone gets the same protocol the terminal gets, which is why the parity
test can assert byte identity in both directions instead of comparing
behaviour. The token rides a Sec-WebSocket-Protocol offer because the browser
WebSocket API cannot set headers, and the server negotiates only the safe
name so the handshake never echoes the secret back. Cross-origin upgrades
stay refused: another page must not be able to drive the operator's panes."
```

---

### Task 4: `/api`, and answering an ask through the gate's own files

**Files:**
- Create: `harness/coppice/internal/web/ask.go`
- Create: `harness/coppice/internal/web/api.go`
- Create: `harness/coppice/internal/web/testdata/safe_id_cases.json`
- Modify: `harness/coppice/internal/web/server.go` (add `Gate AskChannel` to `Options`, register the API routes)
- Test: `harness/coppice/internal/web/ask_test.go`
- Test: `harness/coppice/internal/web/api_test.go`
- Test: `tests/test_ask_safe_id_conformance.py`

**Interfaces:**
- Consumes: `Server`, `Options`, `guard`, `writeJSON`, `writeErr`, `Call`, `Dialer` (tasks 2, 3).
- Produces:
  - `var ErrNoAsk error`, `var ErrBadDecision error`
  - `func SafeID(raw string) string`
  - `type AskChannel struct{ Root string }`
  - `func DefaultGateRoot() string`
  - `func (a AskChannel) Answer(toolUseID, decision, reason string) error`
  - `Options.Gate AskChannel`
  - routes `GET /api/token/check`, `GET /api/panes`, `POST /api/ask/answer`. `/api/panes` answers 502 with `{"error":"server refused: <message>","code":<code>}` when coppice-server refuses, never 200 with an empty list.

- [ ] **Step 1: Write the shared SafeID fixture**

```json
{
  "_comment": "Inputs and the exact file stem ask.py's _safe() produces for each. Go's web.SafeID and Python's opendaisugi.ask._safe both read this file. They must agree byte for byte or the phone writes an answer the gate never reads.",
  "cases": [
    {"in": "toolu_01ABCdef", "out": "toolu_01ABCdef"},
    {"in": "call/with/slashes", "out": "call_with_slashes"},
    {"in": "../../etc/passwd", "out": "_.._etc_passwd"},
    {"in": "...", "out": "none"},
    {"in": "", "out": "none"},
    {"in": ".hidden.", "out": "hidden"},
    {"in": "a b\tc", "out": "a_b_c"},
    {"in": "id-with.dots_and-dashes", "out": "id-with.dots_and-dashes"},
    {"in": "unicode-éè", "out": "unicode-__"},
    {"in": "0123456789012345678901234567890123456789012345678901234567890123456789012345678901234567890123456789012345678901234567890123456789ABCDEF", "out": "01234567890123456789012345678901234567890123456789012345678901234567890123456789012345678901234567890123456789012345678901234567"}
  ]
}
```

The input on the last line is 136 characters and the output is 128: the cut happens after the
substitution and the dot strip, and both sides are pure ASCII by then, so Go's byte slice equals
Python's character slice.

Verify each expected value against the real Python before trusting it:

```bash
cd /mnt/tera/working/programming/openDaisugi
uv run --no-sync python -c "
import json
from opendaisugi.ask import _safe
cases = json.load(open('harness/coppice/internal/web/testdata/safe_id_cases.json'))['cases']
for c in cases:
    got = _safe(c['in'])
    print('OK ' if got == c['out'] else 'MISMATCH ', repr(c['in']), '->', repr(got), 'expected', repr(c['out']))
"
```

Fix any expected value the real `_safe` disagrees with. The Python is the reference; the fixture records what it does.

- [ ] **Step 2: Write the failing tests**

```go
// harness/coppice/internal/web/ask_test.go
package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeAsk builds an ask file the way src/opendaisugi/ask.py's post_ask does.
func writeAsk(t *testing.T, root, toolUseID, nonce string) {
	t.Helper()
	dir := filepath.Join(root, "asks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{
		"toolUseId": toolUseID,
		"nonce":     nonce,
		"postedAt":  float64(time.Now().Unix()),
		"deadline":  float64(time.Now().Add(90 * time.Second).Unix()),
		"toolName":  "Bash",
		"detail":    "rm -rf build/",
	})
	if err := os.WriteFile(filepath.Join(dir, SafeID(toolUseID)+".json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSafeIDMatchesEveryPythonFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/safe_id_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Cases []struct{ In, Out string } `json:"cases"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Cases) == 0 {
		t.Fatal("the fixture file has no cases")
	}
	for _, c := range doc.Cases {
		if got := SafeID(c.In); got != c.Out {
			t.Errorf("SafeID(%q) = %q, want %q", c.In, got, c.Out)
		}
	}
}

func TestAnswerWritesTheFileThePythonGateWillHonor(t *testing.T) {
	root := t.TempDir()
	writeAsk(t, root, "toolu_01ABC", "deadbeef")
	a := AskChannel{Root: root}
	if err := a.Answer("toolu_01ABC", "allow", "allowed from the phone"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "answers", "toolu_01ABC.json")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("answer mode is %o, want 600", fi.Mode().Perm())
	}
	di, err := os.Stat(filepath.Join(root, "answers"))
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("answers directory mode is %o, want 700", di.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if body["nonce"] != "deadbeef" {
		t.Fatalf("nonce is %v, want the ask's deadbeef", body["nonce"])
	}
	if body["decision"] != "allow" || body["toolUseId"] != "toolu_01ABC" {
		t.Fatalf("answer body is %v", body)
	}
	if _, ok := body["updatedInput"]; !ok {
		t.Fatal("answer body has no updatedInput key")
	}
}

// The failure this names: with no ask on disk there is no nonce to echo, and
// ask.py retires both files the moment it sees any answer. Writing one the
// gate is going to reject would burn the ask cycle for nothing, so write
// nothing at all.
func TestAnswerWritesNothingWhenThereIsNoAsk(t *testing.T) {
	root := t.TempDir()
	a := AskChannel{Root: root}
	if err := a.Answer("toolu_gone", "allow", "too late"); err != ErrNoAsk {
		t.Fatalf("Answer returned %v, want ErrNoAsk", err)
	}
	if _, err := os.Stat(filepath.Join(root, "answers", "toolu_gone.json")); !os.IsNotExist(err) {
		t.Fatal("Answer wrote a file with no ask to answer")
	}
}

// The failure this names: only allow and deny are decisions. Anything else
// must not reach the gate's directory.
func TestAnswerRefusesADecisionThatIsNeitherAllowNorDeny(t *testing.T) {
	root := t.TempDir()
	writeAsk(t, root, "toolu_01ABC", "deadbeef")
	a := AskChannel{Root: root}
	for _, bad := range []string{"", "yes", "ALLOW", "maybe"} {
		if err := a.Answer("toolu_01ABC", bad, ""); err != ErrBadDecision {
			t.Errorf("Answer(%q) returned %v, want ErrBadDecision", bad, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "answers", "toolu_01ABC.json")); !os.IsNotExist(err) {
		t.Fatal("a bad decision still wrote an answer file")
	}
}

func TestAnswerRefusesAnAskWithNoNonce(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "asks")
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "toolu_x.json"), []byte(`{"toolUseId":"toolu_x"}`), 0o600)
	if err := (AskChannel{Root: root}).Answer("toolu_x", "deny", ""); err != ErrNoAsk {
		t.Fatalf("Answer returned %v, want ErrNoAsk", err)
	}
}

// The failure this names: a tool_use id is attacker-influenced text. It must
// never steer a write out of the answers directory.
func TestAnswerNeverWritesOutsideTheAnswersDirectory(t *testing.T) {
	root := t.TempDir()
	writeAsk(t, root, "../../etc/passwd", "deadbeef")
	if err := (AskChannel{Root: root}).Answer("../../etc/passwd", "deny", ""); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "answers"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != SafeID("../../etc/passwd")+".json" {
		t.Fatalf("answers holds %v", entries)
	}
}
```

```go
// harness/coppice/internal/web/api_test.go
package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func apiServer(t *testing.T, d Dialer, gateRoot string) (string, string, *http.Client) {
	t.Helper()
	store := TokenStore{Path: filepath.Join(t.TempDir(), "coppice", "web-token")}
	tok, err := store.Mint()
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{Dial: d, Tokens: store, Gate: AskChannel{Root: gateRoot}})
	if err != nil {
		t.Fatal(err)
	}
	ts := newHTTPTestServer(t, s)
	return ts, tok, &http.Client{}
}

// The failure this names: an /api path that answers without a token is an
// open door to every pane on the box.
func TestEveryAPIPathRefusesAMissingToken(t *testing.T) {
	_, d := newFakeServer(t, echoOK)
	base, _, hc := apiServer(t, d, t.TempDir())
	for _, p := range []string{"/api/token/check", "/api/panes"} {
		resp, err := hc.Get(base + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s without a token returned %d, want 401", p, resp.StatusCode)
		}
	}
	resp, err := hc.Post(base+"/api/ask/answer", "application/json", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("POST /api/ask/answer without a token returned %d, want 401", resp.StatusCode)
	}
}

func TestAPIPanesProxiesPaneList(t *testing.T) {
	_, d := newFakeServer(t, func(req map[string]any) []map[string]any {
		if req["cmd"] != "pane.list" {
			return []map[string]any{{"id": req["id"], "ok": false}}
		}
		return []map[string]any{{"id": req["id"], "ok": true, "result": map[string]any{
			"panes": []any{map[string]any{"id": "w1:p1", "label": "auth fix"}},
		}}}
	})
	base, tok, hc := apiServer(t, d, t.TempDir())
	req, _ := http.NewRequest("GET", base+"/api/panes", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	panes, _ := body["panes"].([]any)
	if len(panes) != 1 {
		t.Fatalf("body was %v", body)
	}
}

// The failure this names: a 200 with an empty list would make the PWA print
// "No panes yet. Tap New to start one." on top of a real server error, and
// the operator would go looking for a bug that is not there.
func TestAPIPanesSurfacesARefusalInsteadOfAnEmptyRoster(t *testing.T) {
	_, d := newFakeServer(t, func(req map[string]any) []map[string]any {
		return []map[string]any{{"id": req["id"], "ok": false, "error": map[string]any{
			"code": "internal", "message": "the layout store is unreadable",
		}}}
	})
	base, tok, hc := apiServer(t, d, t.TempDir())
	req, _ := http.NewRequest("GET", base+"/api/panes", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", resp.StatusCode)
	}
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	if body["code"] != "internal" {
		t.Errorf("the server's code was dropped: %v", body["code"])
	}
	msg, _ := body["error"].(string)
	if !strings.HasPrefix(msg, "server refused: ") || !strings.Contains(msg, "layout store") {
		t.Errorf("the client would show %q", msg)
	}
	if _, hasPanes := body["panes"]; hasPanes {
		t.Error("a refusal came back carrying a pane list")
	}
}

func TestAPIAskAnswerAllowsAPendingAsk(t *testing.T) {
	_, d := newFakeServer(t, echoOK)
	gate := t.TempDir()
	writeAsk(t, gate, "toolu_01ABC", "deadbeef")
	base, tok, hc := apiServer(t, d, gate)
	req, _ := http.NewRequest("POST", base+"/api/ask/answer",
		bytes.NewReader([]byte(`{"tool_use_id":"toolu_01ABC","decision":"allow","reason":"fine"}`)))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

// The error has to teach: the operator is looking at a phone and needs to
// know why the button did nothing.
func TestAPIAskAnswerReturns409AndSaysWhyWhenTheAskIsGone(t *testing.T) {
	_, d := newFakeServer(t, echoOK)
	base, tok, hc := apiServer(t, d, t.TempDir())
	req, _ := http.NewRequest("POST", base+"/api/ask/answer",
		bytes.NewReader([]byte(`{"tool_use_id":"toolu_gone","decision":"allow"}`)))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status %d, want 409", resp.StatusCode)
	}
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	msg, _ := body["error"].(string)
	if msg == "" {
		t.Fatal("409 carried no message")
	}
}
```

Add the small helper both files use:

```go
// harness/coppice/internal/web/httptest_test.go
package web

import (
	"net/http/httptest"
	"testing"
)

func newHTTPTestServer(t *testing.T, s *Server) string {
	t.Helper()
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts.URL
}
```

```python
# tests/test_ask_safe_id_conformance.py
"""One format, two implementations, one fixture set.

The phone answers a gate ask by writing the file ``ask.py`` reads. If Go's
``SafeID`` and Python's ``_safe`` ever disagree about the file stem, the
phone writes an answer nobody ever looks at and the operator sees a button
that silently does nothing. This is the conformance pattern the verifier
clients already use, applied to a two-line function that would otherwise
drift unnoticed.
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from opendaisugi.ask import _safe

FIXTURES = (
    Path(__file__).resolve().parents[1]
    / "harness"
    / "coppice"
    / "internal"
    / "web"
    / "testdata"
    / "safe_id_cases.json"
)


def _cases() -> list[dict[str, str]]:
    return json.loads(FIXTURES.read_text(encoding="utf-8"))["cases"]


def test_the_fixture_file_exists_and_has_cases():
    assert FIXTURES.exists(), f"missing {FIXTURES}"
    assert len(_cases()) >= 8


@pytest.mark.parametrize("case", _cases(), ids=lambda c: repr(c["in"]))
def test_python_safe_matches_the_go_fixture(case):
    assert _safe(case["in"]) == case["out"]
```

- [ ] **Step 3: Run the tests and watch them fail**

```bash
cd /mnt/tera/working/programming/openDaisugi/harness/coppice && go test ./internal/web/ -run 'SafeID|Answer|API' -v
cd /mnt/tera/working/programming/openDaisugi && uv run --no-sync pytest tests/test_ask_safe_id_conformance.py -q
```

Expected: Go FAILs with `undefined: SafeID`; Python FAILs on the missing fixture, or passes once step 1 wrote it.

- [ ] **Step 4: Write `ask.go`**

```go
// harness/coppice/internal/web/ask.go
package web

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	// ErrNoAsk means the gate is not holding an ask under that id any more.
	ErrNoAsk = errors.New("no pending ask")
	// ErrBadDecision means the caller sent something that is not a decision.
	ErrBadDecision = errors.New("decision must be allow or deny")
)

var unsafeIDChars = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// SafeID is the Go port of src/opendaisugi/ask.py's _safe, in its order:
// replace every character outside [A-Za-z0-9._-] with "_", strip leading and
// trailing dots, cut to 128 characters, then fall back to "none". The two
// must agree exactly, because the file stem is the only thing joining an
// answer to its ask. testdata/safe_id_cases.json holds both languages to it.
func SafeID(raw string) string {
	s := unsafeIDChars.ReplaceAllString(raw, "_")
	s = strings.Trim(s, ".")
	if len(s) > 128 {
		s = s[:128]
	}
	if s == "" {
		return "none"
	}
	return s
}

// AskChannel writes into the gate's ask directory. Master spec 3.3 has no
// command for answering an ask, so the phone uses the file protocol the gate
// already defines, exactly as tui_sessions.py does from the terminal.
type AskChannel struct{ Root string }

// DefaultGateRoot is ~/.opendaisugi/gate, the directory ask.py owns.
func DefaultGateRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "gate"
	}
	return filepath.Join(home, ".opendaisugi", "gate")
}

// Answer records one decision, echoing the nonce from the ask it answers.
//
// It fails closed in three ways, and each matters. An unknown decision
// writes nothing. A missing ask writes nothing, because ask.py retires both
// files the instant it sees any answer, so an answer it will reject burns
// the ask cycle for no reason. An ask with no nonce writes nothing, for the
// same reason.
func (a AskChannel) Answer(toolUseID, decision, reason string) error {
	if decision != "allow" && decision != "deny" {
		return ErrBadDecision
	}
	id := SafeID(toolUseID)
	raw, err := os.ReadFile(filepath.Join(a.Root, "asks", id+".json"))
	if err != nil {
		return ErrNoAsk
	}
	var ask struct {
		Nonce string `json:"nonce"`
	}
	if json.Unmarshal(raw, &ask) != nil || ask.Nonce == "" {
		return ErrNoAsk
	}
	body, err := json.Marshal(map[string]any{
		"toolUseId":    toolUseID,
		"decision":     decision,
		"reason":       reason,
		"updatedInput": nil,
		"nonce":        ask.Nonce,
	})
	if err != nil {
		return err
	}
	dir := filepath.Join(a.Root, "answers")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".answer-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Rename, so a reader never sees half a file, and so the answer's mtime
	// is later than the ask's. wait_answer rejects an answer older than the
	// ask it claims to answer.
	return os.Rename(tmp.Name(), filepath.Join(dir, id+".json"))
}
```

- [ ] **Step 5: Write `api.go` and register the routes**

```go
// harness/coppice/internal/web/api.go
package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

func (s *Server) routes() {
	s.mux.HandleFunc("GET /api/token/check", s.guard(s.handleTokenCheck))
	s.mux.HandleFunc("GET /api/panes", s.guard(s.handlePanes))
	s.mux.HandleFunc("POST /api/ask/answer", s.guard(s.handleAskAnswer))
}

// handleTokenCheck answers only for a good token. The guard did the work;
// reaching this line is the answer.
func (s *Server) handleTokenCheck(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handlePanes is the roster's first paint, before the websocket is open.
//
// A refusal is never flattened into an empty list. "No panes yet" and
// "coppice-server refused" are different facts, and painting the first over
// the second is exactly what master spec 5.1 calls the cost of guessing.
func (s *Server) handlePanes(w http.ResponseWriter, r *http.Request) {
	msg, err := Call(r.Context(), s.opts.Dial, map[string]any{"cmd": "pane.list"})
	var refused *RefusedError
	switch {
	case errors.As(err, &refused):
		s.opts.Log.Warn("web: pane.list refused", "code", refused.Code, "message", refused.Message)
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": "server refused: " + refused.Message,
			"code":  refused.Code,
		})
		return
	case err != nil:
		s.opts.Log.Warn("web: pane.list failed", "err", err)
		writeErr(w, http.StatusBadGateway, "coppice-server did not answer. Run coppice server status.")
		return
	}
	result, _ := msg["result"].(map[string]any)
	if result == nil {
		result = map[string]any{"panes": []any{}}
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleAskAnswer(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ToolUseID string `json:"tool_use_id"`
		Decision  string `json:"decision"`
		Reason    string `json:"reason"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "The request body is not JSON.")
		return
	}
	if body.Reason == "" {
		body.Reason = "answered from the phone"
	}
	err := s.opts.Gate.Answer(body.ToolUseID, body.Decision, body.Reason)
	switch {
	case errors.Is(err, ErrBadDecision):
		writeErr(w, http.StatusBadRequest, "Send decision allow or deny.")
	case errors.Is(err, ErrNoAsk):
		writeErr(w, http.StatusConflict, "That ask is gone. The gate timed out, or another client answered it.")
	case err != nil:
		s.opts.Log.Error("web: could not write the answer", "err", err)
		writeErr(w, http.StatusInternalServerError, "The answer could not be written. Check that ~/.opendaisugi/gate exists.")
	default:
		s.opts.Log.Info("web: answered an ask", "decision", body.Decision, "tool_use_id", body.ToolUseID)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}
```

In `server.go`, add the field and the call:

```go
type Options struct {
	Dial   Dialer
	Tokens TokenStore
	Gate   AskChannel
	Log    *slog.Logger
	Now    func() time.Time
}
```

and, in `New`, after the `/ws` registration:

```go
	if opts.Gate.Root == "" {
		s.opts.Gate = AskChannel{Root: DefaultGateRoot()}
	}
	s.routes()
```

- [ ] **Step 6: Run the tests and watch them pass**

```bash
cd /mnt/tera/working/programming/openDaisugi/harness/coppice && go test ./internal/web/ -v && go vet ./internal/web/
cd /mnt/tera/working/programming/openDaisugi && uv run --no-sync pytest tests/test_ask_safe_id_conformance.py -q && uv run --no-sync ruff check .
```

Expected: PASS on both sides, ruff clean.

- [ ] **Step 7: Commit**

```bash
cd /mnt/tera/working/programming/openDaisugi
git add harness/coppice/internal/web/ask.go harness/coppice/internal/web/api.go harness/coppice/internal/web/server.go harness/coppice/internal/web/ask_test.go harness/coppice/internal/web/api_test.go harness/coppice/internal/web/httptest_test.go harness/coppice/internal/web/testdata/safe_id_cases.json tests/test_ask_safe_id_conformance.py
git commit -m "coppice/web: allow and deny from the phone, and a refusal that stays a refusal

Master 3.3 has no command for answering an ask, so the phone writes the same
file the terminal view writes and echoes the nonce from the ask. Answer fails
closed three ways, and the reason is the same each time: ask.py retires both
files the moment it sees any answer, so an answer the gate will reject would
burn the ask cycle. One fixture file holds Go's SafeID and Python's _safe to
the same file stem, because a disagreement there is a button that silently
does nothing. /api/panes now reports a refusal as a refusal: an empty roster
painted over a real server error sends the operator hunting a bug that is
not there."
```

---

### Task 5: TLS from four sources, and a local CA that a phone can install once

**Files:**
- Create: `harness/coppice/internal/web/tls.go`
- Create: `harness/coppice/internal/web/localca.go`
- Test: `harness/coppice/internal/web/tls_test.go`
- Test: `harness/coppice/internal/web/localca_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `type TLSSource string` with `TLSTailscale`, `TLSLocalCA`, `TLSFiles`, `TLSOff`
  - `type TLSOptions struct { Source TLSSource; CertFile, KeyFile, CADir, Listen string }`
  - `func (o TLSOptions) Resolve() (certFile, keyFile string, err error)`
  - `func LoadLeaf(certFile string) (*x509.Certificate, error)`
  - `const ExpiryWarnWindow = 14 * 24 * time.Hour`
  - `func ExpiryWarning(c *x509.Certificate, now time.Time) string`
  - `func RequireLoopback(listen string) error`
  - `func DefaultTLSDir() string`
  - `const CAValidity`, `const LeafValidity`
  - `type CAMeta struct{ Version int; Names, IPs []string; Created, NotAfter time.Time }`
  - `type CADir struct{ Path string }`
  - `func DefaultCADir() string`
  - `func (d CADir) Init(names []string, ips []net.IP, now time.Time) (CAMeta, error)` — creates only when `ca.crt` is absent; errors rather than replacing a CA it cannot read
  - `func (d CADir) CAPEM() ([]byte, error)`
  - `func (d CADir) Meta() (CAMeta, error)`
  - `func (d CADir) LeafPaths() (certFile, keyFile string)`

- [ ] **Step 1: Write the failing tests**

```go
// harness/coppice/internal/web/localca_test.go
package web

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func initCA(t *testing.T) (CADir, CAMeta) {
	t.Helper()
	d := CADir{Path: filepath.Join(t.TempDir(), "ca")}
	meta, err := d.Init([]string{"localhost", "box.tail1234.ts.net"}, []net.IP{net.ParseIP("127.0.0.1")}, time.Now())
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	return d, meta
}

func TestInitWritesTheDocumentedLayoutWithPrivateModes(t *testing.T) {
	d, _ := initCA(t)
	for name, want := range map[string]os.FileMode{
		"ca.crt":    0o644,
		"ca.key":    0o600,
		"leaf.crt":  0o644,
		"leaf.key":  0o600,
		"meta.json": 0o600,
	} {
		fi, err := os.Stat(filepath.Join(d.Path, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if fi.Mode().Perm() != want {
			t.Errorf("%s mode is %o, want %o", name, fi.Mode().Perm(), want)
		}
	}
	di, err := os.Stat(d.Path)
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Errorf("ca directory mode is %o, want 700", di.Mode().Perm())
	}
}

func TestTheLeafChainsToTheCAAndCarriesEveryNameAndAddress(t *testing.T) {
	d, _ := initCA(t)
	caPEM, err := d.CAPEM()
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("ca.crt is not a PEM certificate")
	}
	certFile, _ := d.LeafPaths()
	leaf, err := LoadLeaf(certFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "localhost",
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		t.Fatalf("the leaf does not verify against its own CA: %v", err)
	}
	if err := leaf.VerifyHostname("box.tail1234.ts.net"); err != nil {
		t.Errorf("the second name is missing: %v", err)
	}
	found := false
	for _, ip := range leaf.IPAddresses {
		if ip.Equal(net.ParseIP("127.0.0.1")) {
			found = true
		}
	}
	if !found {
		t.Error("127.0.0.1 is not in the leaf's addresses")
	}
}

// The honest proof: a real handshake, not just a chain that parses.
func TestARealTLSHandshakeSucceedsWithTheCAInTheRootPool(t *testing.T) {
	d, _ := initCA(t)
	certFile, keyFile := d.LeafPaths()
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "hello from the box")
	}))
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{pair}}
	ts.StartTLS()
	defer ts.Close()

	caPEM, _ := d.CAPEM()
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	resp, err := hc.Get(ts.URL)
	if err != nil {
		t.Fatalf("handshake against our own CA failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello from the box" {
		t.Fatalf("body was %q", body)
	}
}

// Re-running cert init after the box changes address must not ask the
// operator to install a second CA on the phone.
func TestReInitKeepsTheSameCAAndReissuesOnlyTheLeaf(t *testing.T) {
	d, _ := initCA(t)
	before, err := d.CAPEM()
	if err != nil {
		t.Fatal(err)
	}
	certFile, _ := d.LeafPaths()
	leafBefore, _ := os.ReadFile(certFile)

	if _, err := d.Init([]string{"localhost"}, []net.IP{net.ParseIP("10.0.0.5")}, time.Now()); err != nil {
		t.Fatal(err)
	}
	after, _ := d.CAPEM()
	if string(before) != string(after) {
		t.Fatal("re-init replaced the CA the phone already trusts")
	}
	leafAfter, _ := os.ReadFile(certFile)
	if string(leafBefore) == string(leafAfter) {
		t.Fatal("re-init did not reissue the leaf")
	}
	leaf, _ := LoadLeaf(certFile)
	if err := leaf.VerifyHostname("10.0.0.5"); err != nil {
		t.Errorf("the new address is missing from the reissued leaf: %v", err)
	}
}

func TestTheLeafDoesNotOutliveThreeHundredNinetyEightDays(t *testing.T) {
	d, _ := initCA(t)
	certFile, _ := d.LeafPaths()
	leaf, err := LoadLeaf(certFile)
	if err != nil {
		t.Fatal(err)
	}
	if leaf.NotAfter.Sub(leaf.NotBefore) > LeafValidity+2*time.Hour {
		t.Fatalf("leaf validity is %v, over the browser limit", leaf.NotAfter.Sub(leaf.NotBefore))
	}
}

// The failure this names: treating every load failure as "there is no CA
// yet" means one corrupt byte in ca.key silently mints a new root, and every
// phone in the house stops trusting the box with nothing on screen to say
// why. Re-init keeps the CA or it refuses. It never replaces it.
func TestInitRefusesRatherThanReplacingACAItCannotRead(t *testing.T) {
	d, _ := initCA(t)
	before, err := d.CAPEM()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.Path, "ca.key"), []byte("not a key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Init([]string{"localhost"}, nil, time.Now()); err == nil {
		t.Fatal("Init replaced a CA it could not read")
	} else if !strings.Contains(err.Error(), "will not load") {
		t.Errorf("the error does not say what happened: %v", err)
	}
	after, _ := d.CAPEM()
	if string(before) != string(after) {
		t.Fatal("ca.crt was overwritten, so every phone that trusted it is now locked out")
	}
}

// The failure this names: a certificate for nothing at all is a certificate
// no browser will accept, and the operator should hear that now.
func TestInitRefusesWithNoNameAndNoAddress(t *testing.T) {
	d := CADir{Path: filepath.Join(t.TempDir(), "ca")}
	if _, err := d.Init(nil, nil, time.Now()); err == nil {
		t.Fatal("Init accepted an empty name and address list")
	}
}

func TestMetaRecordsWhatTheLeafCovers(t *testing.T) {
	d, meta := initCA(t)
	read, err := d.Meta()
	if err != nil {
		t.Fatal(err)
	}
	if len(read.Names) != 2 || read.Names[0] != "localhost" {
		t.Fatalf("meta names are %v", read.Names)
	}
	if read.NotAfter.Before(meta.Created) {
		t.Fatal("meta.NotAfter is before meta.Created")
	}
}
```

```go
// harness/coppice/internal/web/tls_test.go
package web

import (
	"crypto/x509"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResolveFilesNeedsBothPaths(t *testing.T) {
	_, _, err := TLSOptions{Source: TLSFiles, CertFile: "only.crt"}.Resolve()
	if err == nil {
		t.Fatal("--tls files accepted a cert with no key")
	}
	if !strings.Contains(err.Error(), "--key") {
		t.Fatalf("the error does not name the missing flag: %v", err)
	}
}

func TestResolveTailscaleTeachesTheCertCommandWhenTheFilesAreMissing(t *testing.T) {
	dir := t.TempDir()
	_, _, err := TLSOptions{Source: TLSTailscale, CertFile: filepath.Join(dir, "a.crt"), KeyFile: filepath.Join(dir, "a.key")}.Resolve()
	if err == nil {
		t.Fatal("--tls tailscale accepted missing files")
	}
	if !strings.Contains(err.Error(), "tailscale cert --cert-file") {
		t.Fatalf("the error does not teach the command: %v", err)
	}
}

func TestResolveLocalCATeachesCertInitWhenThereIsNoCA(t *testing.T) {
	_, _, err := TLSOptions{Source: TLSLocalCA, CADir: filepath.Join(t.TempDir(), "ca")}.Resolve()
	if err == nil {
		t.Fatal("--tls localca accepted a missing CA")
	}
	if !strings.Contains(err.Error(), "coppice web cert init") {
		t.Fatalf("the error does not teach the command: %v", err)
	}
}

func TestResolveLocalCAFindsTheLeafOnceInitHasRun(t *testing.T) {
	d, _ := initCA(t)
	certFile, keyFile := TLSOptions{Source: TLSLocalCA, CADir: d.Path}.mustResolve(t)
	wantCert, wantKey := d.LeafPaths()
	if certFile != wantCert || keyFile != wantKey {
		t.Fatalf("resolved %q %q, want %q %q", certFile, keyFile, wantCert, wantKey)
	}
}

func TestExpiryWarningFiresInsideFourteenDaysAndIsSilentOutside(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	near := &x509.Certificate{NotAfter: now.Add(10 * 24 * time.Hour)}
	far := &x509.Certificate{NotAfter: now.Add(40 * 24 * time.Hour)}
	if msg := ExpiryWarning(near, now); msg == "" || !strings.Contains(msg, "10 days") {
		t.Fatalf("warning for a 10-day certificate was %q", msg)
	}
	if msg := ExpiryWarning(far, now); msg != "" {
		t.Fatalf("warning for a 40-day certificate was %q", msg)
	}
}

// The failure this names: plain HTTP on a tailnet address would put every
// pane on the box on the wire in the clear. --tls off exists for loopback
// and must refuse anything else.
func TestPlainHTTPIsRefusedOnAnyAddressThatIsNotLoopback(t *testing.T) {
	for _, listen := range []string{":8443", "0.0.0.0:8443", "100.64.1.2:8443", "[::]:8443"} {
		if err := RequireLoopback(listen); err == nil {
			t.Errorf("--tls off accepted %q", listen)
		}
	}
	for _, listen := range []string{"127.0.0.1:8443", "localhost:8443", "[::1]:8443"} {
		if err := RequireLoopback(listen); err != nil {
			t.Errorf("--tls off refused loopback %q: %v", listen, err)
		}
	}
}
```

Add the small test helper `mustResolve`:

```go
// harness/coppice/internal/web/tls_helper_test.go
package web

import "testing"

func (o TLSOptions) mustResolve(t *testing.T) (string, string) {
	t.Helper()
	certFile, keyFile, err := o.Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return certFile, keyFile
}
```

- [ ] **Step 2: Run the tests and watch them fail**

```bash
cd /mnt/tera/working/programming/openDaisugi/harness/coppice && go test ./internal/web/ -run 'TLS|CA|Expiry|Loopback|Init|Leaf|Meta|Resolve' -v
```

Expected: FAIL, `undefined: CADir`, `undefined: TLSOptions`.

- [ ] **Step 3: Write `localca.go`**

```go
// harness/coppice/internal/web/localca.go
package web

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

const (
	// CAValidity is ten years. The operator installs this once on a phone and
	// should not be asked again.
	CAValidity = 10 * 365 * 24 * time.Hour
	// LeafValidity is 398 days, the browsers' ceiling for a server
	// certificate. Staying under it keeps the local path behaving like the
	// public one.
	LeafValidity = 398 * 24 * time.Hour
)

// CAMeta is what the leaf currently covers. cert show reads it so the
// operator does not have to parse a certificate to answer "which addresses?"
type CAMeta struct {
	Version  int       `json:"version"`
	Names    []string  `json:"names"`
	IPs      []string  `json:"ips"`
	Created  time.Time `json:"created"`
	NotAfter time.Time `json:"not_after"`
}

// CADir is ~/.opendaisugi/coppice/ca: one CA the phone installs once, and one
// leaf reissued whenever the box changes address.
type CADir struct{ Path string }

// DefaultCADir is ~/.opendaisugi/coppice/ca.
func DefaultCADir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "ca"
	}
	return filepath.Join(home, ".opendaisugi", "coppice", "ca")
}

func (d CADir) file(name string) string { return filepath.Join(d.Path, name) }

// LeafPaths are the server certificate and key --tls localca serves.
func (d CADir) LeafPaths() (string, string) { return d.file("leaf.crt"), d.file("leaf.key") }

// CAPEM is the certificate the phone installs.
func (d CADir) CAPEM() ([]byte, error) { return os.ReadFile(d.file("ca.crt")) }

func (d CADir) Meta() (CAMeta, error) {
	raw, err := os.ReadFile(d.file("meta.json"))
	if err != nil {
		return CAMeta{}, err
	}
	var m CAMeta
	err = json.Unmarshal(raw, &m)
	return m, err
}

func randomSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}

func writeFileMode(path string, body []byte, mode os.FileMode) error {
	if err := os.WriteFile(path, body, mode); err != nil {
		return err
	}
	// WriteFile only applies mode when it creates the file, so an existing
	// file keeps whatever it had. Say it again.
	return os.Chmod(path, mode)
}

func writePEM(path, blockType string, der []byte, mode os.FileMode) error {
	return writeFileMode(path, pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der}), mode)
}

func (d CADir) loadCA() (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certPEM, err := os.ReadFile(d.file("ca.crt"))
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err := os.ReadFile(d.file("ca.key"))
	if err != nil {
		return nil, nil, err
	}
	certBlock, _ := pem.Decode(certPEM)
	keyBlock, _ := pem.Decode(keyPEM)
	if certBlock == nil || keyBlock == nil {
		return nil, nil, errors.New("the CA files are not PEM")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	anyKey, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	key, ok := anyKey.(*ecdsa.PrivateKey)
	if !ok {
		return nil, nil, errors.New("the CA key is not an ECDSA key")
	}
	return cert, key, nil
}

// createCA mints the root the phone installs once. P-256 rather than RSA on
// purpose: the certificate lands near 670 bytes of PEM, and a QR of it is
// still narrow enough to draw in a terminal. An RSA-2048 root would be twice
// that and the code would be a wall nobody can scan.
func (d CADir) createCA(now time.Time) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "openDaisugi coppice local CA", Organization: []string{"openDaisugi"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(CAValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}
	// SubjectKeyId is left empty on purpose: for a CA template Go derives it
	// from the public key.
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	if err := writePEM(d.file("ca.crt"), "CERTIFICATE", der, 0o644); err != nil {
		return nil, nil, err
	}
	if err := writePEM(d.file("ca.key"), "PRIVATE KEY", keyDER, 0o600); err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	return cert, key, err
}

// Init makes the CA if there is none, keeps it if there is, and always
// reissues the leaf for the given names and addresses. Keeping the CA is the
// whole point: the operator installs it on the phone once, and a change of
// address must not undo that.
func (d CADir) Init(names []string, ips []net.IP, now time.Time) (CAMeta, error) {
	if len(names) == 0 && len(ips) == 0 {
		return CAMeta{}, errors.New("give at least one --name or one --ip")
	}
	if err := os.MkdirAll(d.Path, 0o700); err != nil {
		return CAMeta{}, err
	}
	if err := os.Chmod(d.Path, 0o700); err != nil {
		return CAMeta{}, err
	}

	// Create only when there is nothing there. Every other loadCA failure is
	// a CA that exists and will not read: a truncated key, a file someone
	// edited, the wrong key type. Minting a new root over it would break the
	// one promise this design makes, that the phone installs a certificate
	// once, and it would break it silently at the worst moment.
	var (
		caCert *x509.Certificate
		caKey  *ecdsa.PrivateKey
		err    error
	)
	if _, statErr := os.Stat(d.file("ca.crt")); errors.Is(statErr, os.ErrNotExist) {
		caCert, caKey, err = d.createCA(now)
		if err != nil {
			return CAMeta{}, err
		}
	} else {
		caCert, caKey, err = d.loadCA()
		if err != nil {
			return CAMeta{}, fmt.Errorf(
				"the CA in %s will not load: %w. The phone already trusts it, so this "+
					"command will not replace it. Move %s aside to start a new one, then "+
					"install the new CA on every phone", d.Path, err, d.Path)
		}
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return CAMeta{}, err
	}
	serial, err := randomSerial()
	if err != nil {
		return CAMeta{}, err
	}
	cn := ""
	if len(names) > 0 {
		cn = names[0]
	} else {
		cn = ips[0].String()
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn, Organization: []string{"openDaisugi"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(LeafValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              names,
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		return CAMeta{}, err
	}
	leafKeyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		return CAMeta{}, err
	}
	certFile, keyFile := d.LeafPaths()
	if err := writePEM(certFile, "CERTIFICATE", der, 0o644); err != nil {
		return CAMeta{}, err
	}
	if err := writePEM(keyFile, "PRIVATE KEY", leafKeyDER, 0o600); err != nil {
		return CAMeta{}, err
	}

	meta := CAMeta{Version: 1, Names: names, Created: now.UTC(), NotAfter: tmpl.NotAfter.UTC()}
	for _, ip := range ips {
		meta.IPs = append(meta.IPs, ip.String())
	}
	body, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return CAMeta{}, err
	}
	if err := writeFileMode(d.file("meta.json"), body, 0o600); err != nil {
		return CAMeta{}, err
	}
	return meta, nil
}
```

- [ ] **Step 4: Write `tls.go`**

```go
// harness/coppice/internal/web/tls.go
package web

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// TLSSource is where the server's certificate comes from.
type TLSSource string

const (
	// TLSTailscale reads the pair tailscale cert wrote. Let's Encrypt issues
	// it, so the machine name lands on a public certificate-transparency
	// ledger. docs/how-to/phone.md says so before the operator chooses.
	TLSTailscale TLSSource = "tailscale"
	// TLSLocalCA reads the pair coppice web cert init wrote. Nothing about
	// the box leaves it.
	TLSLocalCA TLSSource = "localca"
	// TLSFiles reads an explicit pair.
	TLSFiles TLSSource = "files"
	// TLSOff serves plain HTTP and refuses any address that is not loopback.
	// It exists so a browser on this box gets a real secure context, which
	// HTTPS with an untrusted certificate does not give.
	TLSOff TLSSource = "off"
)

// ExpiryWarnWindow is how long before expiry the server starts complaining.
// A tailscale certificate lives 90 days and nothing renews it for you.
const ExpiryWarnWindow = 14 * 24 * time.Hour

// DefaultTLSDir is ~/.opendaisugi/coppice/tls, where coppice web cert
// tailscale puts the pair it asks tailscale for.
func DefaultTLSDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "tls"
	}
	return filepath.Join(home, ".opendaisugi", "coppice", "tls")
}

type TLSOptions struct {
	Source   TLSSource
	CertFile string
	KeyFile  string
	CADir    string
	Listen   string
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Resolve returns the certificate and key to serve, or an error that teaches
// the command which produces them.
func (o TLSOptions) Resolve() (string, string, error) {
	switch o.Source {
	case TLSOff:
		return "", "", RequireLoopback(o.Listen)
	case TLSFiles:
		if o.CertFile == "" || o.KeyFile == "" {
			return "", "", fmt.Errorf("--tls files needs both --cert and --key")
		}
		if !exists(o.CertFile) || !exists(o.KeyFile) {
			return "", "", fmt.Errorf("--cert or --key does not exist. Check the paths")
		}
		return o.CertFile, o.KeyFile, nil
	case TLSTailscale:
		if o.CertFile == "" || o.KeyFile == "" {
			return "", "", fmt.Errorf("--tls tailscale needs --cert and --key. " +
				"Run tailscale cert --cert-file <path> --key-file <path> <name>.<tailnet>.ts.net")
		}
		if !exists(o.CertFile) || !exists(o.KeyFile) {
			return "", "", fmt.Errorf("no tailscale certificate at %s. "+
				"Run tailscale cert --cert-file %s --key-file %s <name>.<tailnet>.ts.net",
				o.CertFile, o.CertFile, o.KeyFile)
		}
		return o.CertFile, o.KeyFile, nil
	case TLSLocalCA:
		dir := o.CADir
		if dir == "" {
			dir = DefaultCADir()
		}
		certFile, keyFile := CADir{Path: dir}.LeafPaths()
		if !exists(certFile) || !exists(keyFile) {
			return "", "", fmt.Errorf("no local certificate in %s. Run coppice web cert init", dir)
		}
		return certFile, keyFile, nil
	default:
		return "", "", fmt.Errorf("unknown --tls %q. Use tailscale, localca, files, or off", o.Source)
	}
}

// LoadLeaf parses the first certificate in a PEM file.
func LoadLeaf(certFile string) (*x509.Certificate, error) {
	raw, err := os.ReadFile(certFile)
	if err != nil {
		return nil, err
	}
	for {
		block, rest := pem.Decode(raw)
		if block == nil {
			return nil, fmt.Errorf("%s holds no PEM certificate", certFile)
		}
		if block.Type == "CERTIFICATE" {
			return x509.ParseCertificate(block.Bytes)
		}
		raw = rest
	}
}

// ExpiryWarning is the line to log when a certificate is close to running
// out, and the empty string when it is not. Renewal is the operator's, so
// the warning has to arrive before the phone stops working.
func ExpiryWarning(c *x509.Certificate, now time.Time) string {
	left := c.NotAfter.Sub(now)
	if left > ExpiryWarnWindow {
		return ""
	}
	if left <= 0 {
		return "The certificate has expired. Renew it, then restart coppice web serve."
	}
	return fmt.Sprintf("The certificate expires in %d days. Renew it before then.", int(left.Hours()/24))
}

// RequireLoopback refuses plain HTTP anywhere a second machine could reach.
// Every pane on the box is on the other side of this listener.
func RequireLoopback(listen string) error {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("--listen %q is not host:port", listen)
	}
	if host == "" {
		return fmt.Errorf("--tls off needs a loopback address. Use --listen 127.0.0.1:8443")
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("--tls off serves plain HTTP, so it only listens on loopback. "+
			"Use --listen 127.0.0.1:8443, or pick --tls localca to reach %s", host)
	}
	return nil
}
```

- [ ] **Step 5: Run the tests and watch them pass**

```bash
cd /mnt/tera/working/programming/openDaisugi/harness/coppice && go test ./internal/web/ -v && go vet ./internal/web/
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
cd /mnt/tera/working/programming/openDaisugi
git add harness/coppice/internal/web/tls.go harness/coppice/internal/web/localca.go harness/coppice/internal/web/tls_test.go harness/coppice/internal/web/localca_test.go harness/coppice/internal/web/tls_helper_test.go
git commit -m "coppice/web: four TLS sources, and a local CA the phone installs once

tailscale cert is the easy path and it publishes the machine name on a
public transparency ledger, so the operator has to be able to choose the
other one. The local CA is ECDSA P-256 because that keeps the certificate
near 600 bytes and a QR of it is one a phone camera can actually read. Init
keeps an existing CA and reissues only the leaf, so changing address does not
mean installing a root on the phone twice. Plain HTTP refuses any address a
second machine could reach, because every pane on the box is behind it."
```

---

### Task 6: `coppice web cert`, the CA QR, and the plain-HTTP hand-off

**Files:**
- Create: `harness/coppice/internal/web/cahttp.go`
- Create: `harness/coppice/internal/web/qr.go`
- Create: `harness/coppice/cmd/coppice/web.go`
- Modify: `harness/coppice/cmd/coppice/main.go` (created by plan 02; register the `web` subcommand)
- Modify: `harness/coppice/go.mod` (add `github.com/mdp/qrterminal/v3` at the pinned version)
- Test: `harness/coppice/internal/web/cahttp_test.go`
- Test: `harness/coppice/cmd/coppice/web_test.go`

**Interfaces:**
- Consumes: `CADir`, `DefaultCADir`, `DefaultTLSDir`, `CAMeta`, `LoadLeaf` (task 5).
- Produces:
  - `func CACertHandler(caPEM []byte) http.Handler`
  - `func ServeCACert(ctx context.Context, listen string, caPEM []byte, log *slog.Logger) error`
  - `func WriteQR(w io.Writer, text string)`, `const MaxQRColumns = 120`
  - `func CACertURL(host, caListen string) string`
  - in `package main`: `func webCommand(args []string) int`, `func webCertCommand(args []string) int`

- [ ] **Step 1: Write the failing tests**

```go
// harness/coppice/internal/web/cahttp_test.go
package web

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// The CA certificate is public by nature, so this one path carries no token.
// Nothing else may ride along with it.
func TestCACertHandlerServesTheCertificateAndNothingElse(t *testing.T) {
	ts := httptest.NewServer(CACertHandler([]byte("-----BEGIN CERTIFICATE-----\nAAA\n-----END CERTIFICATE-----\n")))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/ca.crt")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-x509-ca-cert" {
		t.Errorf("content type is %q", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "coppice-ca.crt") {
		t.Errorf("content disposition is %q, so the phone will not save it as a file", cd)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "BEGIN CERTIFICATE") {
		t.Errorf("body was %q", body)
	}

	for _, path := range []string{"/", "/api/panes", "/ca.key"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s returned %d, want 404", path, resp.StatusCode)
		}
	}

	// A traversal has to be written by hand. http.Get resolves "/../ca.key"
	// against the base URL in the client and sends "GET /ca.key", so going
	// through the client would test nothing and claim it had.
	raw := rawRequestPath(t, ts.Listener.Addr().String(), "/../ca.key")
	if !strings.HasPrefix(raw, "HTTP/1.1 404") && !strings.HasPrefix(raw, "HTTP/1.1 400") {
		t.Errorf("a hand-written traversal got %q", strings.SplitN(raw, "\r\n", 2)[0])
	}
}

// rawRequestPath dials the listener and writes the request line itself, so
// the path reaches the handler exactly as typed.
func rawRequestPath(t *testing.T, addr, path string) string {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n", path)
	body, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestCACertURLPointsAtTheHandOffPort(t *testing.T) {
	if got := CACertURL("192.168.1.20", ":8080"); got != "http://192.168.1.20:8080/ca.crt" {
		t.Fatalf("CACertURL gave %q", got)
	}
	if got := CACertURL("box.tail1234.ts.net", "0.0.0.0:9000"); got != "http://box.tail1234.ts.net:9000/ca.crt" {
		t.Fatalf("CACertURL gave %q", got)
	}
}

func TestWriteQRDrawsBlocks(t *testing.T) {
	var sb strings.Builder
	WriteQR(&sb, "http://127.0.0.1:8080/ca.crt")
	out := sb.String()
	if !strings.ContainsAny(out, "█▀▄") {
		t.Fatalf("the QR had no block characters: %q", out)
	}
	if len(strings.Split(strings.TrimRight(out, "\n"), "\n")) < 10 {
		t.Fatal("the QR is too short to be a QR")
	}
}

func qrWidth(text string) int {
	var sb strings.Builder
	WriteQR(&sb, text)
	widest := 0
	for _, line := range strings.Split(strings.TrimRight(sb.String(), "\n"), "\n") {
		if n := utf8.RuneCountInString(line); n > widest {
			widest = n
		}
	}
	return widest
}

// The failure this names: a QR wider than the terminal wraps, and a wrapped
// QR is not a QR. Byte count is not the property that matters here; drawn
// width is. The URL code is small. The PEM code is the one at risk, and if
// it ever stops fitting, --qr pem has to go rather than print a scramble.
func TestBothQRCodesFitATerminal(t *testing.T) {
	if w := qrWidth("http://192.168.1.20:8080/ca.crt"); w > MaxQRColumns {
		t.Errorf("the URL QR is %d columns wide, over the %d budget", w, MaxQRColumns)
	}
	d, _ := initCA(t)
	pem, err := d.CAPEM()
	if err != nil {
		t.Fatal(err)
	}
	// An ECDSA P-256 root is roughly 670 bytes of PEM, which lands near 113
	// columns with the two-module quiet zone on each side.
	if w := qrWidth(string(pem)); w > MaxQRColumns {
		t.Errorf("the CA PEM QR is %d columns wide, over the %d budget. "+
			"Either shrink the certificate or drop --qr pem", w, MaxQRColumns)
	}
}
```

```go
// harness/coppice/cmd/coppice/web_test.go
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

var (
	buildOnce sync.Once
	binPath   string
	buildErr  error
)

// coppiceBin builds the real binary once and hands every CLI test its path.
// Testing a CLI through its own binary is the only way to be sure the
// subcommand is actually reachable, rather than that a function exists.
func coppiceBin(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "coppice-bin-")
		if err != nil {
			buildErr = err
			return
		}
		binPath = filepath.Join(dir, "coppice")
		cmd := exec.Command("go", "build", "-o", binPath, "./cmd/coppice")
		cmd.Dir = "../.."
		out, err := cmd.CombinedOutput()
		if err != nil {
			buildErr = err
			binPath = string(out)
		}
	})
	if buildErr != nil {
		t.Fatalf("go build ./cmd/coppice: %v\n%s", buildErr, binPath)
	}
	return binPath
}

// run executes the binary with a private HOME so nothing touches the
// operator's real ~/.opendaisugi.
func run(t *testing.T, home string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(coppiceBin(t), args...)
	cmd.Env = append(os.Environ(), "HOME="+home, "XDG_RUNTIME_DIR=")
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run %v: %v", args, err)
	}
	return string(out), code
}

func TestWebCertInitCreatesTheCAAndPrintsAQR(t *testing.T) {
	home := t.TempDir()
	out, code := run(t, home, "web", "cert", "init", "--name", "localhost", "--ip", "127.0.0.1")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	for _, name := range []string{"ca.crt", "ca.key", "leaf.crt", "leaf.key", "meta.json"} {
		if _, err := os.Stat(filepath.Join(home, ".opendaisugi", "coppice", "ca", name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if !strings.ContainsAny(out, "█▀▄") {
		t.Errorf("cert init printed no QR:\n%s", out)
	}
	if !strings.Contains(out, "/ca.crt") {
		t.Errorf("cert init did not print the CA download URL:\n%s", out)
	}
}

// The failure this names: an operator who has not run init must be told the
// command, not shown a stack trace.
func TestWebCertShowTeachesInitWhenThereIsNoCA(t *testing.T) {
	out, code := run(t, t.TempDir(), "web", "cert", "show")
	if code != 1 {
		t.Fatalf("exit %d, want 1: %s", code, out)
	}
	if !strings.Contains(out, "coppice web cert init") {
		t.Errorf("the message does not teach the command:\n%s", out)
	}
}

func TestWebCertShowListsWhatTheLeafCovers(t *testing.T) {
	home := t.TempDir()
	run(t, home, "web", "cert", "init", "--name", "localhost", "--ip", "127.0.0.1")
	out, code := run(t, home, "web", "cert", "show")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	for _, want := range []string{"localhost", "127.0.0.1", "expires"} {
		if !strings.Contains(out, want) {
			t.Errorf("cert show did not mention %q:\n%s", want, out)
		}
	}
}

// The flags are ours, not tailscale's defaults, because we never verified
// what tailscale names a file when you do not tell it.
func TestWebCertTailscalePassesExplicitCertAndKeyPaths(t *testing.T) {
	home := t.TempDir()
	fake := filepath.Join(t.TempDir(), "bin")
	os.MkdirAll(fake, 0o755)
	script := "#!/bin/sh\necho \"$@\" > \"" + filepath.Join(home, "argv.txt") + "\"\n" +
		"for a in \"$@\"; do case \"$a\" in --cert-file=*) echo cert > \"${a#--cert-file=}\";; --key-file=*) echo key > \"${a#--key-file=}\";; esac; done\n"
	if err := os.WriteFile(filepath.Join(fake, "tailscale"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(coppiceBin(t), "web", "cert", "tailscale", "box.tail1234.ts.net")
	cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+fake+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	argv, err := os.ReadFile(filepath.Join(home, "argv.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--cert-file=", "--key-file=", "box.tail1234.ts.net"} {
		if !strings.Contains(string(argv), want) {
			t.Errorf("tailscale was called as %q, missing %q", argv, want)
		}
	}
}
```

- [ ] **Step 2: Add the QR dependency and run the tests to watch them fail**

```bash
cd /mnt/tera/working/programming/openDaisugi/harness/coppice
go get github.com/mdp/qrterminal/v3@v3.2.1   # use the version PINS.md records
go test ./internal/web/ -run 'CACert|QR' -v
go test ./cmd/coppice/ -run TestWebCert -v
```

Expected: FAIL, `undefined: CACertHandler`, and the CLI test fails because `web` is not a subcommand.

- [ ] **Step 3: Write `cahttp.go` and `qr.go`**

```go
// harness/coppice/internal/web/cahttp.go
package web

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// CACertHandler serves exactly one file. A phone that trusts nothing yet
// cannot fetch the CA over the HTTPS the CA is for, so the hand-off is plain
// HTTP on a second port. A CA certificate is public by design, which is why
// this path carries no token, and why it must serve nothing else: the key
// beside it is not public.
func CACertHandler(caPEM []byte) http.Handler {
	body := append([]byte(nil), caPEM...)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ca.crt" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/x-x509-ca-cert")
		w.Header().Set("Content-Disposition", `attachment; filename="coppice-ca.crt"`)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Write(body)
	})
}

// CACertURL is what the terminal QR encodes. Scanning it opens the phone's
// browser on a download, which is the step Android's certificate installer
// needs: it reads a file, not a screenful of text.
func CACertURL(host, caListen string) string {
	_, port, err := net.SplitHostPort(caListen)
	if err != nil || port == "" {
		port = "8080"
	}
	return fmt.Sprintf("http://%s:%s/ca.crt", host, port)
}

// ServeCACert runs the hand-off listener until ctx ends.
func ServeCACert(ctx context.Context, listen string, caPEM []byte, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	srv := &http.Server{
		Addr:              listen,
		Handler:           CACertHandler(caPEM),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	log.Info("web: serving the CA certificate", "listen", listen, "path", "/ca.crt")
	err := srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
```

```go
// harness/coppice/internal/web/qr.go
package web

import (
	"io"

	"github.com/mdp/qrterminal/v3"
)

// MaxQRColumns is the drawn width a QR must stay inside. A wrapped QR is not
// a QR, and 120 columns is what a terminal on a laptop reliably has.
const MaxQRColumns = 120

// WriteQR draws a QR code with half blocks, so it fits a normal terminal at
// a size a phone camera can still read. Error correction stays low on
// purpose: less redundancy is a smaller code, and the operator is holding a
// phone six inches from the screen, not reading a poster.
func WriteQR(w io.Writer, text string) {
	qrterminal.GenerateWithConfig(text, qrterminal.Config{
		Level:          qrterminal.L,
		Writer:         w,
		HalfBlocks:     true,
		BlackChar:      qrterminal.BLACK_BLACK,
		WhiteBlackChar: qrterminal.WHITE_BLACK,
		WhiteChar:      qrterminal.WHITE_WHITE,
		BlackWhiteChar: qrterminal.BLACK_WHITE,
		QuietZone:      2,
	})
}
```

- [ ] **Step 4: Write `cmd/coppice/web.go` with the `cert` subcommand**

```go
// harness/coppice/cmd/coppice/web.go
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/opendaisugi/coppice/internal/web"
)

const webUsage = `coppice web - the phone client

  coppice web cert init [--name NAME]... [--ip ADDR]... [--ca-dir DIR] [--ca-listen ADDR] [--qr url|pem|off]
      Make the local CA if there is none, then issue a server certificate.
      Prints a QR the phone scans to install the CA.
  coppice web cert show
      Print what the current certificate covers and when it expires.
  coppice web cert tailscale <name>.<tailnet>.ts.net [--dir DIR]
      Ask tailscale for a Let's Encrypt certificate and write it where
      coppice web serve --tls tailscale looks for it.
`

func webCommand(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, webUsage)
		return 1
	}
	switch args[0] {
	case "cert":
		return webCertCommand(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: coppice web %s\n\n%s", args[0], webUsage)
		return 1
	}
}

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func webCertCommand(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, webUsage)
		return 1
	}
	switch args[0] {
	case "init":
		return webCertInit(args[1:])
	case "show":
		return webCertShow(args[1:])
	case "tailscale":
		return webCertTailscale(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: coppice web cert %s\n\n%s", args[0], webUsage)
		return 1
	}
}

func webCertInit(args []string) int {
	fs := flag.NewFlagSet("cert init", flag.ContinueOnError)
	var names, ips stringList
	fs.Var(&names, "name", "a DNS name the phone will use. Repeatable.")
	fs.Var(&ips, "ip", "an address the phone will use. Repeatable.")
	caDir := fs.String("ca-dir", web.DefaultCADir(), "where the CA lives")
	caListen := fs.String("ca-listen", ":8080", "the plain-HTTP port that hands the CA to the phone")
	qrKind := fs.String("qr", "url", "url, pem, or off")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	var parsed []net.IP
	for _, raw := range ips {
		ip := net.ParseIP(raw)
		if ip == nil {
			fmt.Fprintf(os.Stderr, "%q is not an address. Use --ip 192.168.1.20.\n", raw)
			return 1
		}
		parsed = append(parsed, ip)
	}
	d := web.CADir{Path: *caDir}
	meta, err := d.Init(names, parsed, time.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	fmt.Printf("CA        %s\n", filepath.Join(*caDir, "ca.crt"))
	fmt.Printf("server    %s\n", filepath.Join(*caDir, "leaf.crt"))
	fmt.Printf("covers    %s\n", strings.Join(append(append([]string{}, meta.Names...), meta.IPs...), " "))
	fmt.Printf("expires   %s\n", meta.NotAfter.Format(time.RFC3339))

	host := "127.0.0.1"
	if len(meta.Names) > 0 {
		host = meta.Names[0]
	} else if len(meta.IPs) > 0 {
		host = meta.IPs[0]
	}
	url := web.CACertURL(host, *caListen)
	fmt.Printf("\nInstall the CA on the phone. Scan this, save the file, then open it.\n%s\n\n", url)
	switch *qrKind {
	case "off":
	case "pem":
		pem, err := d.CAPEM()
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			return 1
		}
		web.WriteQR(os.Stdout, string(pem))
	default:
		web.WriteQR(os.Stdout, url)
	}
	fmt.Printf("\nStart the server with: coppice web serve --tls localca --ca-listen %s\n", *caListen)
	return 0
}

func webCertShow(args []string) int {
	fs := flag.NewFlagSet("cert show", flag.ContinueOnError)
	caDir := fs.String("ca-dir", web.DefaultCADir(), "where the CA lives")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	d := web.CADir{Path: *caDir}
	meta, err := d.Meta()
	if err != nil {
		fmt.Fprintf(os.Stderr, "No local CA in %s. Run coppice web cert init.\n", *caDir)
		return 1
	}
	certFile, _ := d.LeafPaths()
	leaf, err := web.LoadLeaf(certFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "The certificate in %s will not parse. Run coppice web cert init.\n", *caDir)
		return 1
	}
	fmt.Printf("CA        %s\n", filepath.Join(*caDir, "ca.crt"))
	fmt.Printf("covers    %s\n", strings.Join(append(append([]string{}, meta.Names...), meta.IPs...), " "))
	fmt.Printf("expires   %s\n", leaf.NotAfter.Format(time.RFC3339))
	if warn := web.ExpiryWarning(leaf, time.Now()); warn != "" {
		fmt.Printf("warning   %s\n", warn)
	}
	return 0
}

func webCertTailscale(args []string) int {
	fs := flag.NewFlagSet("cert tailscale", flag.ContinueOnError)
	dir := fs.String("dir", web.DefaultTLSDir(), "where to write the pair")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "Give the MagicDNS name. Example: coppice web cert tailscale box.tail1234.ts.net")
		return 1
	}
	name := fs.Arg(0)
	if err := os.MkdirAll(*dir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	certFile := filepath.Join(*dir, name+".crt")
	keyFile := filepath.Join(*dir, name+".key")
	// Both paths are explicit. What tailscale names a file when you do not
	// tell it is not something we verified, so we never rely on it.
	cmd := exec.Command("tailscale", "cert",
		"--cert-file="+certFile, "--key-file="+keyFile, name)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "tailscale cert failed. Turn on MagicDNS and HTTPS in the admin console, then try again.\n")
		return 1
	}
	fmt.Printf("cert      %s\nkey       %s\n", certFile, keyFile)
	fmt.Printf("\nStart the server with:\n  coppice web serve --tls tailscale --cert %s --key %s\n", certFile, keyFile)
	fmt.Printf("Let's Encrypt issues this and it lasts 90 days. Renewal is yours.\n")
	return 0
}
```

- [ ] **Step 5: Register `web` in the CLI dispatch**

Find plan 02's subcommand dispatch and add one case.

```bash
cd /mnt/tera/working/programming/openDaisugi/harness/coppice
grep -n 'case "server"\|switch ' cmd/coppice/main.go | head
```

Add, beside the other cases:

```go
	case "web":
		os.Exit(webCommand(args[1:]))
```

If plan 02 built a map of handlers instead of a switch, add the entry `"web": webCommand` in the same shape as its neighbours. Record the file and line you edited in this task's commit message.

- [ ] **Step 6: Run the tests and watch them pass**

```bash
cd /mnt/tera/working/programming/openDaisugi/harness/coppice
go test ./internal/web/ ./cmd/coppice/ -v && go vet ./...
```

Expected: PASS. If `TestWebCertInitCreatesTheCAAndPrintsAQR` writes into your real home, the `HOME=` override did not reach the binary; check `run`.

- [ ] **Step 7: Commit**

```bash
cd /mnt/tera/working/programming/openDaisugi
git add harness/coppice/go.mod harness/coppice/go.sum harness/coppice/internal/web/cahttp.go harness/coppice/internal/web/qr.go harness/coppice/internal/web/cahttp_test.go harness/coppice/cmd/coppice/web.go harness/coppice/cmd/coppice/main.go harness/coppice/cmd/coppice/web_test.go
git commit -m "coppice/web: cert init, and a QR that actually gets the CA onto a phone

A phone that trusts nothing cannot fetch the CA over the HTTPS the CA is
for, so the hand-off is one plain-HTTP path on a second port serving exactly
ca.crt. The QR encodes that URL rather than the certificate text, because
Android's installer wants a file and a camera app hands you a screenful of
base64. --qr pem is there for anyone who prefers the other way. cert
tailscale always passes --cert-file and --key-file, because what tailscale
names a file on its own is not something we verified."
```

---

### Task 7: ntfy on every merged transition to `blocked`

**Files:**
- Create: `harness/coppice/internal/web/push.go`
- Modify: `harness/coppice/internal/web/server.go` (add `Push *Publisher` to `Options`)
- Modify: `harness/coppice/internal/web/api.go` (register `POST /api/push/test`)
- Test: `harness/coppice/internal/web/push_test.go`

**Interfaces:**
- Consumes: `Dialer`, `Open`, `Call`, `Session` (task 2); `Server`, `Options`, `guard`, `writeJSON`, `writeErr` (tasks 3, 4).
- Produces:
  - `type Ask struct { ID, Tool, Summary string; Deadline float64 }`
  - `type StateEvent struct { Pane, Harness, State, Source, Detail string; TS float64; Ask *Ask }`
  - `const PushDebounce = 5 * time.Second`
  - `type PushConfig struct { BaseURL, Topic, TokenEnv, ClickBase string }`
  - `type Publisher struct{ ... }`
  - `func NewPublisher(cfg PushConfig, hc *http.Client, now func() time.Time, log *slog.Logger) (*Publisher, error)`
  - `func (p *Publisher) OnState(ev StateEvent, label string)`
  - `func (p *Publisher) Test(ctx context.Context) error`
  - `func (p *Publisher) Watch(ctx context.Context, d Dialer) error`
  - `func asciiHeader(s string, max int) string`
  - `Options.Push *Publisher`
  - route `POST /api/push/test`

- [ ] **Step 1: Write the failing tests**

```go
// harness/coppice/internal/web/push_test.go
package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type capturedPush struct {
	mu   sync.Mutex
	reqs []struct {
		Path, Title, Click, Auth, Body string
	}
}

func (c *capturedPush) add(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reqs = append(c.reqs, struct{ Path, Title, Click, Auth, Body string }{
		r.URL.Path, r.Header.Get("Title"), r.Header.Get("Click"), r.Header.Get("Authorization"), string(body),
	})
}

func (c *capturedPush) len() int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.reqs) }

func newNtfy(t *testing.T) (*capturedPush, string) {
	t.Helper()
	cap := &capturedPush{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.add(r)
		w.WriteHeader(200)
	}))
	t.Cleanup(ts.Close)
	return cap, ts.URL
}

func newPublisher(t *testing.T, base string, now *time.Time, tokenEnv string) *Publisher {
	t.Helper()
	p, err := NewPublisher(PushConfig{
		BaseURL:   base,
		Topic:     "coppice",
		TokenEnv:  tokenEnv,
		ClickBase: "https://box.tail1234.ts.net:8443",
	}, http.DefaultClient, func() time.Time { return *now }, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func blockedEvent(pane string) StateEvent {
	return StateEvent{Pane: pane, Harness: "claude-code", State: "blocked", Source: "gate",
		Ask: &Ask{ID: "toolu_1", Tool: "Bash", Summary: "rm -rf build/"}}
}

func TestPublishesOnceWhenAPaneBecomesBlocked(t *testing.T) {
	cap, base := newNtfy(t)
	now := time.Unix(1_757_300_000, 0)
	p := newPublisher(t, base, &now, "")
	p.OnState(StateEvent{Pane: "w1:p1", State: "working"}, "auth fix")
	p.OnState(blockedEvent("w1:p1"), "auth fix")
	if cap.len() != 1 {
		t.Fatalf("%d publishes, want 1", cap.len())
	}
	got := cap.reqs[0]
	if got.Path != "/coppice" {
		t.Errorf("posted to %q, want /coppice", got.Path)
	}
	if got.Title != "auth fix needs you" {
		t.Errorf("title was %q", got.Title)
	}
	if got.Body != "rm -rf build/" {
		t.Errorf("body was %q", got.Body)
	}
	if got.Click != "https://box.tail1234.ts.net:8443/#/pane/w1:p1" {
		t.Errorf("click was %q", got.Click)
	}
	if got.Auth != "" {
		t.Errorf("an unauthenticated ntfy got an Authorization header: %q", got.Auth)
	}
}

func TestDoesNotPublishWhileThePaneStaysBlocked(t *testing.T) {
	cap, base := newNtfy(t)
	now := time.Unix(1_757_300_000, 0)
	p := newPublisher(t, base, &now, "")
	p.OnState(blockedEvent("w1:p1"), "auth fix")
	now = now.Add(time.Hour)
	p.OnState(blockedEvent("w1:p1"), "auth fix")
	p.OnState(blockedEvent("w1:p1"), "auth fix")
	if cap.len() != 1 {
		t.Fatalf("%d publishes, want 1: repeat blocked events are not new news", cap.len())
	}
}

func TestPublishesAgainAfterBlockedClearsAndReturns(t *testing.T) {
	cap, base := newNtfy(t)
	now := time.Unix(1_757_300_000, 0)
	p := newPublisher(t, base, &now, "")
	p.OnState(blockedEvent("w1:p1"), "auth fix")
	p.OnState(StateEvent{Pane: "w1:p1", State: "working"}, "auth fix")
	now = now.Add(30 * time.Second)
	p.OnState(blockedEvent("w1:p1"), "auth fix")
	if cap.len() != 2 {
		t.Fatalf("%d publishes, want 2", cap.len())
	}
}

// The failure this names: a pane that flaps blocked, working, blocked inside
// five seconds must not buzz the phone twice.
func TestDebouncesARepeatBlockWithinFiveSeconds(t *testing.T) {
	cap, base := newNtfy(t)
	now := time.Unix(1_757_300_000, 0)
	p := newPublisher(t, base, &now, "")
	p.OnState(blockedEvent("w1:p1"), "auth fix")
	p.OnState(StateEvent{Pane: "w1:p1", State: "working"}, "auth fix")
	now = now.Add(2 * time.Second)
	p.OnState(blockedEvent("w1:p1"), "auth fix")
	if cap.len() != 1 {
		t.Fatalf("%d publishes, want 1 inside the %v debounce", cap.len(), PushDebounce)
	}
}

func TestSendsTheBearerTokenFromTheNamedEnvironmentVariable(t *testing.T) {
	cap, base := newNtfy(t)
	t.Setenv("COPPICE_NTFY_TOKEN", "tk_secret")
	now := time.Unix(1_757_300_000, 0)
	p := newPublisher(t, base, &now, "COPPICE_NTFY_TOKEN")
	p.OnState(blockedEvent("w1:p1"), "auth fix")
	if cap.len() != 1 {
		t.Fatal("no publish")
	}
	if cap.reqs[0].Auth != "Bearer tk_secret" {
		t.Fatalf("Authorization was %q", cap.reqs[0].Auth)
	}
}

func TestNonAsciiTitleAndBodyAreSanitized(t *testing.T) {
	cap, base := newNtfy(t)
	now := time.Unix(1_757_300_000, 0)
	p := newPublisher(t, base, &now, "")
	ev := blockedEvent("w1:p1")
	ev.Ask.Summary = "rm -rf \u2014 build/\nsecond line"
	p.OnState(ev, "caf\u00e9 fix")
	got := cap.reqs[0]
	for _, s := range []string{got.Title, got.Body} {
		for _, r := range s {
			if r < 0x20 || r > 0x7e {
				t.Fatalf("%q still carries a non-ASCII or control rune %q", s, r)
			}
		}
	}
}

func TestLabelFallsBackToThePaneId(t *testing.T) {
	cap, base := newNtfy(t)
	now := time.Unix(1_757_300_000, 0)
	p := newPublisher(t, base, &now, "")
	p.OnState(blockedEvent("w1:p1"), "")
	if cap.reqs[0].Title != "w1:p1 needs you" {
		t.Fatalf("title was %q", cap.reqs[0].Title)
	}
}

// Push is a courtesy, not a permission. An ntfy that is down or refusing
// must never take the floor down with it.
func TestAPushFailureNeverStopsTheServer(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer ts.Close()
	now := time.Unix(1_757_300_000, 0)
	p := newPublisher(t, ts.URL, &now, "")
	p.OnState(blockedEvent("w1:p1"), "auth fix")     // must not panic
	p.OnState(StateEvent{Pane: "w1:p1", State: "working"}, "auth fix")
	now = now.Add(30 * time.Second)
	p.OnState(blockedEvent("w1:p1"), "auth fix")     // still tries
	if err := p.Test(context.Background()); err == nil {
		t.Fatal("Test reported success against a 500")
	}
}

func TestWatchTurnsSocketStateEventsIntoPushes(t *testing.T) {
	cap, base := newNtfy(t)
	_, d := newFakeServer(t, func(req map[string]any) []map[string]any {
		switch req["cmd"] {
		case "pane.list":
			return []map[string]any{{"id": req["id"], "ok": true, "result": map[string]any{
				"panes": []any{map[string]any{"id": "w1:p1", "label": "auth fix"}}}}}
		case "events.subscribe":
			return []map[string]any{
				{"id": req["id"], "ok": true},
				{"event": "state", "pane": "w1:p1", "state": "blocked", "source": "gate", "harness": "claude-code",
					"ask": map[string]any{"id": "toolu_1", "tool": "Bash", "summary": "rm -rf build/"}},
			}
		}
		return []map[string]any{{"id": req["id"], "ok": true}}
	})
	now := time.Unix(1_757_300_000, 0)
	p := newPublisher(t, base, &now, "")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go p.Watch(ctx, d)
	deadline := time.Now().Add(3 * time.Second)
	for cap.len() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if cap.len() != 1 {
		t.Fatalf("%d publishes from one socket state event", cap.len())
	}
	if !strings.Contains(cap.reqs[0].Title, "auth fix") {
		t.Errorf("Watch did not look the label up: title %q", cap.reqs[0].Title)
	}
}
```

Add the API test for the test button:

```go
// append to harness/coppice/internal/web/api_test.go
func TestPushTestReturns409WhenPushIsNotConfigured(t *testing.T) {
	_, d := newFakeServer(t, echoOK)
	base, tok, hc := apiServer(t, d, t.TempDir())
	req, _ := http.NewRequest("POST", base+"/api/push/test", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status %d, want 409", resp.StatusCode)
	}
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	if msg, _ := body["error"].(string); !strings.Contains(msg, "--ntfy") {
		t.Fatalf("the message does not teach the flag: %q", body["error"])
	}
}
```

- [ ] **Step 2: Run the tests and watch them fail**

```bash
cd /mnt/tera/working/programming/openDaisugi/harness/coppice && go test ./internal/web/ -run 'Push|Publish|Watch|Debounce|Label|Ascii|Bearer' -v
```

Expected: FAIL, `undefined: NewPublisher`.

- [ ] **Step 3: Write `push.go`**

```go
// harness/coppice/internal/web/push.go
package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// PushDebounce is how long one pane stays quiet after it buzzes the phone. A
// pane that flaps blocked, working, blocked is one event to a human.
const PushDebounce = 5 * time.Second

// Ask mirrors the ask block of a PaneStateEvent (master spec 3.1).
type Ask struct {
	ID       string  `json:"id"`
	Tool     string  `json:"tool"`
	Summary  string  `json:"summary"`
	Deadline float64 `json:"deadline"`
}

// StateEvent is the part of a PaneStateEvent push cares about.
type StateEvent struct {
	Pane    string  `json:"pane"`
	Harness string  `json:"harness"`
	State   string  `json:"state"`
	Source  string  `json:"source"`
	Detail  string  `json:"detail"`
	TS      float64 `json:"ts"`
	Ask     *Ask    `json:"ask"`
}

// PushConfig is a self-hosted ntfy. There is no other option and no default
// server: master spec 4 says push goes through a self-hosted ntfy or not at
// all.
type PushConfig struct {
	BaseURL   string // https://ntfy.example
	Topic     string
	TokenEnv  string // the name of the environment variable holding the token
	ClickBase string // the PWA's external URL, for the notification's click target
}

// Publisher turns merged state into notifications. It holds the last state
// it saw per pane, so it can tell a transition from a repeat.
type Publisher struct {
	cfg  PushConfig
	hc   *http.Client
	now  func() time.Time
	log  *slog.Logger
	mu   sync.Mutex
	last map[string]string
	sent map[string]time.Time
}

func NewPublisher(cfg PushConfig, hc *http.Client, now func() time.Time, log *slog.Logger) (*Publisher, error) {
	if cfg.BaseURL == "" || cfg.Topic == "" {
		return nil, errors.New("push needs --ntfy URL and --ntfy-topic NAME")
	}
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.Default()
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	cfg.ClickBase = strings.TrimRight(cfg.ClickBase, "/")
	return &Publisher{cfg: cfg, hc: hc, now: now, log: log,
		last: map[string]string{}, sent: map[string]time.Time{}}, nil
}

// asciiHeader makes a string safe for an HTTP header value: printable ASCII
// only, one line, bounded. ntfy reads Title and Click as headers, and a
// stray newline there would split the request.
func asciiHeader(s string, max int) string {
	var b strings.Builder
	lastSpace := false
	for _, r := range s {
		if r < 0x20 || r > 0x7e {
			r = ' '
		}
		if r == ' ' {
			if lastSpace {
				continue
			}
			lastSpace = true
		} else {
			lastSpace = false
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if len(out) > max {
		out = strings.TrimSpace(out[:max-3]) + "..."
	}
	return out
}

// OnState records one merged state and publishes when a pane has just become
// blocked. Everything else is quiet on purpose.
func (p *Publisher) OnState(ev StateEvent, label string) {
	p.mu.Lock()
	was := p.last[ev.Pane]
	p.last[ev.Pane] = ev.State
	if ev.State != "blocked" || was == "blocked" {
		p.mu.Unlock()
		return
	}
	now := p.now()
	if last, ok := p.sent[ev.Pane]; ok && now.Sub(last) < PushDebounce {
		p.mu.Unlock()
		return
	}
	p.sent[ev.Pane] = now
	p.mu.Unlock()

	if label == "" {
		label = ev.Pane
	}
	body := "blocked, and the gate gave no detail"
	if ev.Ask != nil && ev.Ask.Summary != "" {
		body = ev.Ask.Summary
	} else if ev.Detail != "" {
		body = ev.Detail
	}
	click := ""
	if p.cfg.ClickBase != "" {
		click = p.cfg.ClickBase + "/#/pane/" + ev.Pane
	}
	if err := p.publish(context.Background(), label+" needs you", body, click); err != nil {
		// A notification that did not arrive is not a reason to change what
		// the floor believes. Log it and carry on.
		p.log.Warn("web: ntfy publish failed", "pane", ev.Pane, "err", err)
	}
}

// Test publishes one message so the operator can prove the topic works from
// the Settings screen.
func (p *Publisher) Test(ctx context.Context) error {
	return p.publish(ctx, "coppice test", "Push works. This came from your own server.", p.cfg.ClickBase)
}

func (p *Publisher) publish(ctx context.Context, title, body, click string) error {
	url := p.cfg.BaseURL + "/" + p.cfg.Topic
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader([]byte(asciiHeader(body, 500))))
	if err != nil {
		return err
	}
	req.Header.Set("Title", asciiHeader(title, 100))
	if click != "" {
		req.Header.Set("Click", click)
	}
	if p.cfg.TokenEnv != "" {
		if tok := os.Getenv(p.cfg.TokenEnv); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	resp, err := p.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("ntfy answered %d", resp.StatusCode)
	}
	return nil
}

// Watch subscribes to state events on the socket and feeds OnState. Labels
// come from pane.list, which is where they live; a pane the list does not
// know is titled by its id rather than guessed at.
func (p *Publisher) Watch(ctx context.Context, d Dialer) error {
	labels := map[string]string{}
	refresh := func() {
		msg, err := Call(ctx, d, map[string]any{"cmd": "pane.list"})
		if err != nil {
			return
		}
		result, _ := msg["result"].(map[string]any)
		panes, _ := result["panes"].([]any)
		for _, raw := range panes {
			item, _ := raw.(map[string]any)
			id, _ := item["id"].(string)
			label, _ := item["label"].(string)
			if id != "" {
				labels[id] = label
			}
		}
	}
	refresh()

	sess, err := Open(ctx, d)
	if err != nil {
		return err
	}
	defer sess.Close()
	sub, _ := json.Marshal(map[string]any{
		"id": "push-sub", "cmd": "events.subscribe",
		"panes": "*", "kinds": []string{"state"},
	})
	if err := sess.Send(sub); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case line, ok := <-sess.Lines():
			if !ok {
				return errors.New("coppice-server closed the event stream")
			}
			var envelope struct {
				Event string `json:"event"`
			}
			if json.Unmarshal(line, &envelope) != nil || envelope.Event != "state" {
				continue
			}
			var ev StateEvent
			if json.Unmarshal(line, &ev) != nil || ev.Pane == "" {
				continue
			}
			if _, known := labels[ev.Pane]; !known {
				refresh()
			}
			p.OnState(ev, labels[ev.Pane])
		}
	}
}
```

- [ ] **Step 4: Wire the publisher into `Options` and add the test route**

In `server.go`, add the field:

```go
type Options struct {
	Dial   Dialer
	Tokens TokenStore
	Gate   AskChannel
	Push   *Publisher
	Log    *slog.Logger
	Now    func() time.Time
}
```

In `api.go`, add the route inside `routes()` and the handler:

```go
	s.mux.HandleFunc("POST /api/push/test", s.guard(s.handlePushTest))
```

```go
func (s *Server) handlePushTest(w http.ResponseWriter, r *http.Request) {
	if s.opts.Push == nil {
		writeErr(w, http.StatusConflict,
			"Push is off. Restart the server with --ntfy URL and --ntfy-topic NAME.")
		return
	}
	if err := s.opts.Push.Test(r.Context()); err != nil {
		writeErr(w, http.StatusBadGateway, "ntfy did not accept the message. Check the URL and the token.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
```

- [ ] **Step 5: Run the tests and watch them pass**

```bash
cd /mnt/tera/working/programming/openDaisugi/harness/coppice && go test ./internal/web/ -v && go vet ./internal/web/
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
cd /mnt/tera/working/programming/openDaisugi
git add harness/coppice/internal/web/push.go harness/coppice/internal/web/push_test.go harness/coppice/internal/web/server.go harness/coppice/internal/web/api.go harness/coppice/internal/web/api_test.go
git commit -m "coppice/web: ntfy on the transition to blocked, not on every event

The interesting moment is the edge, so the publisher holds the last merged
state per pane and only speaks when a pane has just become blocked, with a
five second floor per pane so a flapping pane does not buzz twice. Titles and
bodies are scrubbed to printable ASCII because ntfy reads them as headers and
a stray newline would split the request. A push that fails is logged, never
retried into a loop and never allowed to change what the floor believes: a
notification is a courtesy, not a permission."
```

---

### Task 8: `coppice web serve`, `coppice web token`, and the honest `--web-push`

**Files:**
- Create: `harness/coppice/internal/web/config.go`
- Modify: `harness/coppice/cmd/coppice/web.go` (add `serve` and `token`)
- Modify: `harness/coppice/internal/server/` (one call to `web.AutoStart`; find the file in step 5)
- Test: `harness/coppice/internal/web/config_test.go`
- Test: `harness/coppice/cmd/coppice/web_serve_test.go`

**Interfaces:**
- Consumes: `Options`, `New`, `Server.Handler`, `TokenStore`, `AskChannel`, `TLSOptions`, `LoadLeaf`, `ExpiryWarning`, `CADir`, `ServeCACert`, `NewPublisher`, `Publisher.Watch`, `WriteQR`, `UnixDialer`, `DefaultSocketPath` (tasks 1 to 7).
- Produces:
  - `type Config struct { Enabled bool; Listen, TLS, CertFile, KeyFile, CADir, CAListen, Ntfy, NtfyTopic, NtfyTokenEnv, ExternalURL, GateRoot string }`
  - `func DefaultConfigPath() string`
  - `func LoadConfig(path string) (Config, error)`
  - `func SaveConfig(path string, c Config) error`
  - `func Serve(ctx context.Context, c Config, opts Options) error`
  - `func AutoStart(ctx context.Context, opts Options) (func(), error)`
  - in `package main`: `func webServeCommand(args []string) int`, `func webTokenCommand(args []string) int`, `func signInURL(base, listen, tlsSource string) string`, `func printSignIn(token, externalURL, listen, tlsSource string)`

- [ ] **Step 1: Write the failing tests**

```go
// harness/coppice/internal/web/config_test.go
package web

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadConfigTreatsAMissingFileAsOff(t *testing.T) {
	c, err := LoadConfig(filepath.Join(t.TempDir(), "web.json"))
	if err != nil {
		t.Fatalf("a missing config is not an error: %v", err)
	}
	if c.Enabled {
		t.Fatal("a missing config enabled the phone server")
	}
}

func TestSaveConfigWritesAPrivateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "coppice", "web.json")
	if err := SaveConfig(path, Config{Enabled: true, Listen: ":8443", TLS: "localca"}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("web.json mode is %o, want 600", fi.Mode().Perm())
	}
	back, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !back.Enabled || back.Listen != ":8443" || back.TLS != "localca" {
		t.Fatalf("round trip gave %+v", back)
	}
}

// The failure this names: an operator who never asked for a phone server
// must not get a listener when the coppice server starts. Checking only that
// AutoStart returned nil would pass on exactly the bug this test exists to
// catch, so it names a port and proves nothing came up on it.
func TestAutoStartDoesNothingWhenTheConfigIsAbsentOrDisabled(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	_, d := newFakeServer(t, echoOK)
	store := TokenStore{Path: filepath.Join(home, "web-token")}
	if _, err := store.Mint(); err != nil {
		t.Fatal(err)
	}
	const addr = "127.0.0.1:18447"

	// No config file at all.
	stop, err := AutoStart(context.Background(), Options{Dial: d, Tokens: store})
	if err != nil {
		t.Fatal(err)
	}
	assertNothingListening(t, addr, "with no web.json")
	stop()

	// A config that names a port but is switched off.
	if err := SaveConfig(DefaultConfigPath(), Config{Enabled: false, Listen: addr, TLS: "off"}); err != nil {
		t.Fatal(err)
	}
	stop, err = AutoStart(context.Background(), Options{Dial: d, Tokens: store})
	if err != nil {
		t.Fatal(err)
	}
	assertNothingListening(t, addr, "with enabled false")
	stop()

	// Switched on, the same port answers. Without this the two checks above
	// would pass on a port nothing could ever bind.
	if err := SaveConfig(DefaultConfigPath(), Config{Enabled: true, Listen: addr, TLS: "off"}); err != nil {
		t.Fatal(err)
	}
	stop, err = AutoStart(context.Background(), Options{Dial: d, Tokens: store})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	waitForPort(t, addr)
}

func TestServeOverPlainLoopbackAnswersTheTokenCheck(t *testing.T) {
	home := t.TempDir()
	_, d := newFakeServer(t, echoOK)
	store := TokenStore{Path: filepath.Join(home, "web-token")}
	tok, err := store.Mint()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := Config{Enabled: true, Listen: "127.0.0.1:18443", TLS: "off"}
	go Serve(ctx, cfg, Options{Dial: d, Tokens: store, Gate: AskChannel{Root: home}})
	waitForPort(t, "127.0.0.1:18443")

	if code := getStatus(t, "http://127.0.0.1:18443/api/token/check", tok); code != 200 {
		t.Fatalf("token check returned %d", code)
	}
	if code := getStatus(t, "http://127.0.0.1:18443/api/token/check", "wrong"); code != 401 {
		t.Fatalf("a bad token returned %d, want 401", code)
	}
}

// The failure this names: plain HTTP on anything but loopback would put
// every pane on the box on the wire in the clear.
func TestServeRefusesPlainHTTPOffLoopback(t *testing.T) {
	home := t.TempDir()
	_, d := newFakeServer(t, echoOK)
	store := TokenStore{Path: filepath.Join(home, "web-token")}
	store.Mint()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := Serve(ctx, Config{Enabled: true, Listen: "0.0.0.0:18444", TLS: "off"},
		Options{Dial: d, Tokens: store})
	if err == nil {
		t.Fatal("Serve accepted plain HTTP on 0.0.0.0")
	}
}
```

Add the two small helpers:

```go
// harness/coppice/internal/web/serve_helper_test.go
package web

import (
	"net"
	"net/http"
	"testing"
	"time"
)

func waitForPort(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("nothing listening on %s after 5 s", addr)
}

// assertNothingListening gives a listener that should not exist half a
// second to appear, then fails if it did.
func assertNothingListening(t *testing.T, addr, when string) {
	t.Helper()
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			t.Fatalf("something is listening on %s %s", addr, when)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func getStatus(t *testing.T, url, token string) int {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}
```

```go
// harness/coppice/cmd/coppice/web_serve_test.go
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Master spec 3.5: a control that does nothing is a lie. Web Push is not
// built, so the flag says so and names the path that is.
func TestWebPushFlagRefusesAndNamesNtfy(t *testing.T) {
	out, code := run(t, t.TempDir(), "web", "serve", "--web-push", "--tls", "off", "--listen", "127.0.0.1:18445")
	if code != 1 {
		t.Fatalf("exit %d, want 1: %s", code, out)
	}
	if !strings.Contains(out, "ntfy") {
		t.Errorf("the message does not name ntfy:\n%s", out)
	}
	if !strings.Contains(out, "not built") {
		t.Errorf("the message does not say the feature is not built:\n%s", out)
	}
}

func TestWebServeRefusesPlainHTTPOffLoopbackFromTheCLI(t *testing.T) {
	out, code := run(t, t.TempDir(), "web", "serve", "--tls", "off", "--listen", "0.0.0.0:18446")
	if code != 1 {
		t.Fatalf("exit %d, want 1: %s", code, out)
	}
	if !strings.Contains(out, "127.0.0.1") && !strings.Contains(out, "localca") {
		t.Errorf("the message teaches neither loopback nor localca:\n%s", out)
	}
}

func TestWebTokenMintsOnFirstRunAndPrintsTheURL(t *testing.T) {
	home := t.TempDir()
	out, code := run(t, home, "web", "token", "--url", "https://box.tail1234.ts.net:8443")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if _, err := os.Stat(filepath.Join(home, ".opendaisugi", "coppice", "web-token")); err != nil {
		t.Fatalf("no token file: %v", err)
	}
	if !strings.Contains(out, "https://box.tail1234.ts.net:8443/#t=") {
		t.Errorf("token did not print a scannable URL:\n%s", out)
	}
	if !strings.ContainsAny(out, "█▀▄") {
		t.Errorf("token printed no QR:\n%s", out)
	}
}

// The failure this names: an https URL printed for a plain-HTTP listener is
// a QR that cannot connect, and the operator has no way to see why.
func TestTheSignInURLFollowsTheTLSSource(t *testing.T) {
	home := t.TempDir()
	// --persist writes web.json before it tries to listen, and both of these
	// refuse to listen for a stated reason, so each records a TLS source and
	// exits without leaving a server behind.
	run(t, home, "web", "serve", "--tls", "off", "--listen", "0.0.0.0:8443", "--persist")
	out, _ := run(t, home, "web", "token", "--qr=false")
	if !strings.Contains(out, "http://") || strings.Contains(out, "https://") {
		t.Fatalf("with --tls off saved, the sign-in URL was not plain http:\n%s", out)
	}
	run(t, home, "web", "serve", "--tls", "localca", "--listen", "0.0.0.0:8443", "--persist")
	out, _ = run(t, home, "web", "token", "--qr=false")
	if !strings.Contains(out, "https://") {
		t.Fatalf("with --tls localca saved, the sign-in URL was not https:\n%s", out)
	}
}

// The failure this names: an operator who set up push and then turned
// autostart off would find their ntfy settings gone, with nothing having
// said so.
func TestForgetKeepsEverySettingExceptEnabled(t *testing.T) {
	home := t.TempDir()
	run(t, home, "web", "serve", "--tls", "localca", "--listen", "0.0.0.0:8443", "--persist",
		"--ntfy", "https://ntfy.box.local", "--ntfy-topic", "coppice",
		"--ntfy-token-env", "COPPICE_NTFY_TOKEN")

	saved := filepath.Join(home, ".opendaisugi", "coppice", "web.json")
	body, err := os.ReadFile(saved)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "ntfy.box.local") {
		t.Fatalf("--persist did not save the ntfy URL:\n%s", body)
	}

	run(t, home, "web", "serve", "--forget")
	body, err = os.ReadFile(saved)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "ntfy.box.local") {
		t.Fatalf("--forget threw away the ntfy URL:\n%s", body)
	}
	if !strings.Contains(string(body), `"enabled": false`) {
		t.Fatalf("--forget did not turn autostart off:\n%s", body)
	}
}

func TestGateRootIsConfigurableAndSaved(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "elsewhere", "gate")
	run(t, home, "web", "serve", "--tls", "localca", "--listen", "0.0.0.0:8443",
		"--persist", "--gate-root", root)
	body, err := os.ReadFile(filepath.Join(home, ".opendaisugi", "coppice", "web.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "elsewhere") {
		t.Fatalf("--gate-root was not saved:\n%s", body)
	}
}

func TestWebTokenShowsTheSameTokenUntilRotated(t *testing.T) {
	home := t.TempDir()
	first, _ := run(t, home, "web", "token", "--qr=false")
	second, _ := run(t, home, "web", "token", "--qr=false")
	if first != second {
		t.Fatal("a second coppice web token minted a new token instead of showing the current one")
	}
	third, _ := run(t, home, "web", "token", "--qr=false", "--rotate")
	if third == second {
		t.Fatal("--rotate did not change the token")
	}
}

// The wiring assertion: AutoStart is only useful if the coppice server
// actually calls it, and no unit test in package web can see that.
func TestTheCoppiceServerCallsWebAutoStart(t *testing.T) {
	matches, err := filepath.Glob("../../internal/server/*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range matches {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), "web.AutoStart(") {
			return
		}
	}
	t.Fatal("no file in internal/server calls web.AutoStart, so web.enabled would never start anything")
}
```

- [ ] **Step 2: Run the tests and watch them fail**

```bash
cd /mnt/tera/working/programming/openDaisugi/harness/coppice
go test ./internal/web/ -run 'Config|AutoStart|Serve' -v
go test ./cmd/coppice/ -v
```

Expected: FAIL, `undefined: LoadConfig`, and unknown subcommands.

- [ ] **Step 3: Write `config.go`**

```go
// harness/coppice/internal/web/config.go
package web

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Config is the persisted phone server. Enabled is what makes the coppice
// server start it, so there is one process to keep alive instead of two.
type Config struct {
	Enabled      bool   `json:"enabled"`
	Listen       string `json:"listen"`
	TLS          string `json:"tls"`
	CertFile     string `json:"cert_file"`
	KeyFile      string `json:"key_file"`
	CADir        string `json:"ca_dir"`
	CAListen     string `json:"ca_listen"`
	Ntfy         string `json:"ntfy"`
	NtfyTopic    string `json:"ntfy_topic"`
	NtfyTokenEnv string `json:"ntfy_token_env"`
	ExternalURL  string `json:"external_url"`
	// GateRoot is <data_dir>/gate. data_dir is configurable
	// (src/opendaisugi/config.py), and the terminal view already reads it
	// that way, so a box with a non-default data directory would otherwise
	// have every Allow and Deny land where nothing reads it.
	GateRoot string `json:"gate_root"`
}

// DefaultConfigPath is ~/.opendaisugi/coppice/web.json.
func DefaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "web.json"
	}
	return filepath.Join(home, ".opendaisugi", "coppice", "web.json")
}

// LoadConfig reads the file. No file means the phone server is off, which is
// the right default for a box nobody has set up yet.
func LoadConfig(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return Config{}, err
	}
	return c, nil
}

func SaveConfig(path string, c Config) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// Serve runs the phone server until ctx ends.
func Serve(ctx context.Context, c Config, opts Options) error {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if c.Listen == "" {
		c.Listen = ":8443"
	}
	if c.GateRoot != "" {
		opts.Gate = AskChannel{Root: c.GateRoot}
	}
	if opts.Gate.Root == "" {
		opts.Gate = AskChannel{Root: DefaultGateRoot()}
	}
	source := TLSSource(c.TLS)
	if source == "" {
		source = TLSLocalCA
	}
	certFile, keyFile, err := TLSOptions{
		Source: source, CertFile: c.CertFile, KeyFile: c.KeyFile,
		CADir: c.CADir, Listen: c.Listen,
	}.Resolve()
	if err != nil {
		return err
	}

	if certFile != "" {
		leaf, err := LoadLeaf(certFile)
		if err != nil {
			return err
		}
		if warn := ExpiryWarning(leaf, time.Now()); warn != "" {
			opts.Log.Warn("web: " + warn)
		}
	}

	if c.Ntfy != "" && c.NtfyTopic != "" {
		pub, err := NewPublisher(PushConfig{
			BaseURL: c.Ntfy, Topic: c.NtfyTopic,
			TokenEnv: c.NtfyTokenEnv, ClickBase: c.ExternalURL,
		}, nil, nil, opts.Log)
		if err != nil {
			return err
		}
		opts.Push = pub
		go func() {
			if err := pub.Watch(ctx, opts.Dial); err != nil && ctx.Err() == nil {
				opts.Log.Warn("web: the push watcher stopped", "err", err)
			}
		}()
	}

	if source == TLSLocalCA && c.CAListen != "" && c.CAListen != "off" {
		dir := c.CADir
		if dir == "" {
			dir = DefaultCADir()
		}
		caPEM, err := (CADir{Path: dir}).CAPEM()
		if err != nil {
			return err
		}
		go func() {
			if err := ServeCACert(ctx, c.CAListen, caPEM, opts.Log); err != nil && ctx.Err() == nil {
				opts.Log.Warn("web: the CA hand-off stopped", "err", err)
			}
		}()
	}

	s, err := New(opts)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              c.Listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12},
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	opts.Log.Info("web: serving the phone",
		"listen", c.Listen, "tls", string(source), "gate_root", opts.Gate.Root)
	if source == TLSOff {
		err = srv.ListenAndServe()
	} else {
		err = srv.ListenAndServeTLS(certFile, keyFile)
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// AutoStart is what the coppice server calls once its socket is up. A config
// that is absent or disabled starts nothing and reports no error: the phone
// is opt-in.
func AutoStart(ctx context.Context, opts Options) (func(), error) {
	c, err := LoadConfig(DefaultConfigPath())
	if err != nil {
		return func() {}, err
	}
	if !c.Enabled {
		return func() {}, nil
	}
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		if err := Serve(ctx, c, opts); err != nil && ctx.Err() == nil {
			log := opts.Log
			if log == nil {
				log = slog.Default()
			}
			log.Error("web: the phone server stopped", "err", err)
		}
	}()
	return cancel, nil
}
```

- [ ] **Step 4: Add `serve` and `token` to `cmd/coppice/web.go`**

Extend `webUsage` and the `webCommand` switch, then add the two commands.

```go
// add to webUsage
//   coppice web serve [--listen :8443] [--tls tailscale|localca|files|off] [--cert F --key F]
//                     [--ca-dir DIR] [--ca-listen ADDR] [--external-url URL]
//                     [--gate-root DIR]
//                     [--ntfy URL --ntfy-topic NAME --ntfy-token-env VAR] [--persist|--forget]
//       Serve the phone client. Mints a token on the first run and prints it.
//   coppice web token [--rotate] [--url URL] [--qr]
//       Print the current token, its sign-in URL, and a QR to scan.

	case "serve":
		return webServeCommand(args[1:])
	case "token":
		return webTokenCommand(args[1:])
```

```go
func webServeCommand(args []string) int {
	fs := flag.NewFlagSet("web serve", flag.ContinueOnError)
	listen := fs.String("listen", ":8443", "address to serve the phone client on")
	tlsSource := fs.String("tls", "localca", "tailscale, localca, files, or off")
	certFile := fs.String("cert", "", "certificate file for --tls files or --tls tailscale")
	keyFile := fs.String("key", "", "key file for --tls files or --tls tailscale")
	caDir := fs.String("ca-dir", web.DefaultCADir(), "where the local CA lives")
	caListen := fs.String("ca-listen", ":8080", "plain-HTTP port that hands the CA to the phone, or off")
	externalURL := fs.String("external-url", "", "the URL the phone uses, for notification links")
	ntfy := fs.String("ntfy", "", "your ntfy server, for push")
	ntfyTopic := fs.String("ntfy-topic", "", "the ntfy topic to publish to")
	ntfyTokenEnv := fs.String("ntfy-token-env", "", "name of the environment variable holding the ntfy token")
	gateRoot := fs.String("gate-root", web.DefaultGateRoot(), "the gate's ask directory, <data dir>/gate")
	webPush := fs.Bool("web-push", false, "not built. Use ntfy.")
	persist := fs.Bool("persist", false, "remember these settings and start with the coppice server")
	forget := fs.Bool("forget", false, "stop starting with the coppice server, then exit")
	qr := fs.Bool("qr", true, "print the sign-in QR on start")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *webPush {
		fmt.Fprintln(os.Stderr,
			"Web push is not built. It is marked planned in docs/feature-status.md.\n"+
				"Push goes through your own ntfy. Start again with --ntfy URL --ntfy-topic NAME.")
		return 1
	}

	cfg := web.Config{
		Enabled: true, Listen: *listen, TLS: *tlsSource,
		CertFile: *certFile, KeyFile: *keyFile, CADir: *caDir, CAListen: *caListen,
		Ntfy: *ntfy, NtfyTopic: *ntfyTopic, NtfyTokenEnv: *ntfyTokenEnv,
		ExternalURL: *externalURL, GateRoot: *gateRoot,
	}
	if *forget {
		// Turning autostart off must not also forget the ntfy settings the
		// operator saved. Edit the file that is there rather than writing a
		// fresh one out of flag defaults.
		saved, err := web.LoadConfig(web.DefaultConfigPath())
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			return 1
		}
		if saved.Listen == "" {
			saved = cfg // nothing was ever saved, so record what was asked for
		}
		saved.Enabled = false
		if err := web.SaveConfig(web.DefaultConfigPath(), saved); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			return 1
		}
		fmt.Println("The phone server will not start with the coppice server.")
		return 0
	}
	if *persist {
		if err := web.SaveConfig(web.DefaultConfigPath(), cfg); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			return 1
		}
		fmt.Printf("Saved %s. The coppice server will start the phone server.\n", web.DefaultConfigPath())
	}

	store := web.TokenStore{Path: web.DefaultTokenPath()}
	tok, err := store.Load()
	if err != nil {
		// A first run with no token should hand the operator a working
		// phone, not a refusal. Minting one keeps every request
		// authenticated, which is the part that matters.
		tok, err = store.Mint()
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			return 1
		}
		fmt.Println("Minted a new web token.")
	}
	if *qr {
		printSignIn(tok, *externalURL, *listen, *tlsSource)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err = web.Serve(ctx, cfg, web.Options{
		Dial:   web.UnixDialer{Path: web.DefaultSocketPath()},
		Tokens: store,
		Gate:   web.AskChannel{Root: cfg.GateRoot},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	return 0
}

// signInURL builds the address the phone opens. The scheme follows the TLS
// source, because printing an https URL for a plain-HTTP listener hands the
// operator a QR that cannot connect.
func signInURL(base, listen, tlsSource string) string {
	if base != "" {
		return strings.TrimRight(base, "/")
	}
	scheme := "https"
	if tlsSource == string(web.TLSOff) {
		scheme = "http"
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "localhost"
	}
	if scheme == "http" {
		host = "127.0.0.1" // --tls off only ever listens on loopback
	}
	_, port, splitErr := net.SplitHostPort(listen)
	if splitErr != nil || port == "" {
		port = "8443"
	}
	return scheme + "://" + host + ":" + port
}

func printSignIn(token, externalURL, listen, tlsSource string) {
	url := signInURL(externalURL, listen, tlsSource) + "/#t=" + token
	fmt.Printf("token     %s\nsign in   %s\n\nScan this on the phone.\n", token, url)
	web.WriteQR(os.Stdout, url)
}

func webTokenCommand(args []string) int {
	fs := flag.NewFlagSet("web token", flag.ContinueOnError)
	rotate := fs.Bool("rotate", false, "mint a new token and retire the old one")
	url := fs.String("url", "", "the URL the phone uses")
	qr := fs.Bool("qr", true, "print the sign-in QR")
	listen := fs.String("listen", ":8443", "the port the phone connects to")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	store := web.TokenStore{Path: web.DefaultTokenPath()}
	tok, err := store.Load()
	if err != nil || *rotate {
		tok, err = store.Mint()
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			return 1
		}
	}
	// The saved config knows which scheme the server is actually serving.
	// Without one, https is the right guess: every source but --tls off is
	// https, and --tls off is the loopback development path.
	cfg, _ := web.LoadConfig(web.DefaultConfigPath())
	if *qr {
		printSignIn(tok, *url, *listen, cfg.TLS)
	} else {
		fmt.Printf("token     %s\nsign in   %s\n", tok, signInURL(*url, *listen, cfg.TLS)+"/#t="+tok)
	}
	return 0
}
```

Add the imports `cmd/coppice/web.go` now needs: `context`, `net`, `os/signal`, `syscall`.

- [ ] **Step 5: Call `AutoStart` from the coppice server**

```bash
cd /mnt/tera/working/programming/openDaisugi/harness/coppice
grep -rn 'func .*Serve\|func .*Run\|Accept()' internal/server/*.go | head
```

In the function that owns the accept loop, immediately after the listener is accepting, add:

```go
	stopWeb, err := web.AutoStart(ctx, web.Options{
		Dial:   web.UnixDialer{Path: sockPath},
		Tokens: web.TokenStore{Path: web.DefaultTokenPath()},
		Gate:   web.AskChannel{Root: web.DefaultGateRoot()},
		Log:    log,
	})
	if err != nil {
		log.Warn("web: the phone server did not start", "err", err)
	} else {
		defer stopWeb()
	}
```

Use the server package's own names for `ctx`, `sockPath`, and `log`. Import `"github.com/opendaisugi/coppice/internal/web"`. Record the file and line in the commit message.

- [ ] **Step 6: Run the tests and watch them pass**

```bash
cd /mnt/tera/working/programming/openDaisugi/harness/coppice && go test ./... && go vet ./...
```

Expected: PASS. If `TestServeOverPlainLoopbackAnswersTheTokenCheck` fails on a busy port, change the port in the test rather than the code.

- [ ] **Step 7: Commit**

```bash
cd /mnt/tera/working/programming/openDaisugi
git add harness/coppice/internal/web/config.go harness/coppice/internal/web/config_test.go harness/coppice/internal/web/serve_helper_test.go harness/coppice/cmd/coppice/web.go harness/coppice/cmd/coppice/web_serve_test.go harness/coppice/internal/server/
git commit -m "coppice/web: one process to keep alive, and a flag that admits it is not built

web.json holds the phone server's settings and the coppice server starts it,
so an operator keeps one process alive rather than two. The first serve mints
a token and prints it as a QR: refusing to start would be fail-closed in
letter and useless in practice, and a minted token still authenticates every
request. --gate-root exists because data_dir is configurable and the terminal
view already reads it that way; hardcoding the default would make every Allow
on a non-standard box land where nothing reads it. --forget edits the saved
config rather than writing one out of flag defaults, so turning autostart off
does not also throw away the ntfy settings. --web-push exits 1 and names
ntfy, because master 3.5 says a control that does nothing is a lie and a flag
that teaches is the honest shape."
```

---

### Task 9: The PWA shell: embed, the static handler, the service worker, the icons

**Files:**
- Create: `harness/coppice/internal/web/static.go`
- Create: `harness/coppice/internal/web/static/index.html`
- Create: `harness/coppice/internal/web/static/app.css`
- Create: `harness/coppice/internal/web/static/app.js`
- Create: `harness/coppice/internal/web/static/sw.js`
- Create: `harness/coppice/internal/web/static/sw-policy.js`
- Create: `harness/coppice/internal/web/static/manifest.webmanifest`
- Create: `harness/coppice/internal/web/static/icons/icon-192.png` (generated)
- Create: `harness/coppice/internal/web/static/icons/icon-512.png` (generated)
- Create: `harness/coppice/internal/web/static/_icongen/main.go`
- Modify: `harness/coppice/internal/web/server.go` (register `/`)
- Test: `harness/coppice/internal/web/static_test.go`

**Interfaces:**
- Consumes: `Server`, `New` (task 3).
- Produces:
  - `func StaticHandler() (http.Handler, error)`
  - `func StaticFiles() fs.FS`
  - in `static/app.js`: `export function bearerProtocols(token)`, `export function route(hash)`, `window.coppice` with `rpc(cmd, fields)`, `screen(name)`
  - in `static/sw-policy.js`: `export const SHELL`, `export function shouldCache(path)`

- [ ] **Step 1: Write the failing tests**

```go
// harness/coppice/internal/web/static_test.go
package web

import (
	"encoding/json"
	"image"
	_ "image/png"
	"io"
	"io/fs"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func staticServer(t *testing.T) string {
	t.Helper()
	_, d := newFakeServer(t, echoOK)
	store := TokenStore{Path: filepath.Join(t.TempDir(), "web-token")}
	store.Mint()
	s, err := New(Options{Dial: d, Tokens: store})
	if err != nil {
		t.Fatal(err)
	}
	return newHTTPTestServer(t, s)
}

// The shell has to load before the operator has pasted anything, so this is
// the one part of the surface with no token on it.
func TestTheAppShellLoadsWithoutAToken(t *testing.T) {
	base := staticServer(t)
	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET / returned %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "screen-roster") {
		t.Fatalf("index.html has no roster screen:\n%s", body)
	}
}

func TestTheManifestAndServiceWorkerAreServedCorrectly(t *testing.T) {
	base := staticServer(t)
	resp, err := http.Get(base + "/manifest.webmanifest")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/manifest+json") {
		t.Errorf("manifest content type is %q", ct)
	}
	var m map[string]any
	json.NewDecoder(resp.Body).Decode(&m)
	if m["start_url"] != "/" || m["display"] != "standalone" {
		t.Errorf("manifest is %v", m)
	}

	sw, err := http.Get(base + "/sw.js")
	if err != nil {
		t.Fatal(err)
	}
	defer sw.Body.Close()
	if sw.StatusCode != 200 {
		t.Fatalf("GET /sw.js returned %d, so the worker cannot take the root scope", sw.StatusCode)
	}
}

func TestStaticSetsNoCacheNoSniffAndAContentSecurityPolicy(t *testing.T) {
	base := staticServer(t)
	resp, err := http.Get(base + "/app.js")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Cache-Control") != "no-cache" {
		t.Errorf("Cache-Control is %q", resp.Header.Get("Cache-Control"))
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("X-Content-Type-Options is %q", resp.Header.Get("X-Content-Type-Options"))
	}
	csp := resp.Header.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'self'", "base-uri 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q is missing %q", csp, want)
		}
	}
	if strings.Contains(csp, "unsafe-inline") {
		t.Errorf("CSP allows inline script or style: %q", csp)
	}
}

// The failure this names: an inline handler or style would force
// unsafe-inline into the policy, and the policy is the reason a stray script
// cannot read the operator's token.
func TestNoStaticFileUsesInlineScriptOrStyle(t *testing.T) {
	raw, err := fs.ReadFile(StaticFiles(), "index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(raw)
	for _, bad := range []string{"onclick=", "onload=", "<style>", "javascript:"} {
		if strings.Contains(html, bad) {
			t.Errorf("index.html contains %q, which the CSP forbids", bad)
		}
	}
}

func TestEveryFileTheShellNeedsIsEmbedded(t *testing.T) {
	for _, name := range []string{
		"index.html", "app.css", "app.js", "grid.js", "roster.js", "pane.js",
		"newpane.js", "settings.js", "sw.js", "sw-policy.js",
		"manifest.webmanifest", "icons/icon-192.png", "icons/icon-512.png",
	} {
		if _, err := fs.Stat(StaticFiles(), name); err != nil {
			t.Errorf("%s is not embedded: %v", name, err)
		}
	}
}

func TestTheTestsDirectoryIsNotShipped(t *testing.T) {
	if _, err := fs.Stat(StaticFiles(), "_tests"); err == nil {
		t.Fatal("the JS unit tests were embedded into the binary")
	}
	if _, err := fs.Stat(StaticFiles(), "_icongen"); err == nil {
		t.Fatal("the icon generator was embedded into the binary")
	}
}

func TestTheIconsAreRealPNGsAtTheDeclaredSizes(t *testing.T) {
	for name, want := range map[string]int{"icons/icon-192.png": 192, "icons/icon-512.png": 512} {
		f, err := StaticFiles().Open(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		cfg, format, err := image.DecodeConfig(f)
		f.Close()
		if err != nil || format != "png" {
			t.Fatalf("%s is not a PNG: %v", name, err)
		}
		if cfg.Width != want || cfg.Height != want {
			t.Errorf("%s is %dx%d, want %dx%d", name, cfg.Width, cfg.Height, want, want)
		}
	}
}
```

For now `grid.js`, `roster.js`, `pane.js`, `newpane.js`, and `settings.js` may be one-line placeholder modules that export nothing; tasks 10 and 11 fill them. Create them as empty ES modules in this task so the embed test is honest about the file list from the start.

- [ ] **Step 2: Run the tests and watch them fail**

```bash
cd /mnt/tera/working/programming/openDaisugi/harness/coppice && go test ./internal/web/ -run 'Static|Shell|Manifest|Icon|Inline|Embedded|Shipped' -v
```

Expected: FAIL, `undefined: StaticFiles`.

- [ ] **Step 3: Write the icon generator and run it once**

```go
// harness/coppice/internal/web/static/_icongen/main.go
// Run once: go run ./internal/web/static/_icongen/main.go
// The leading underscore keeps this directory out of ./... and out of embed.
package main

import (
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"log"
	"os"
	"path/filepath"
	"strconv"
)

// A coppice is a stump that sends up straight shoots. Three shoots of
// different heights on a stool, in flat colour: legible at 48 pixels, and
// nothing here needs a font.
func icon(size int) *image.RGBA {
	ground := color.RGBA{0x12, 0x18, 0x1a, 0xff}
	shoot := color.RGBA{0x8f, 0xb9, 0x96, 0xff}
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	draw.Draw(img, img.Bounds(), &image.Uniform{ground}, image.Point{}, draw.Src)

	unit := float64(size) / 32
	rect := func(x, y, w, h float64) {
		r := image.Rect(int(x*unit), int(y*unit), int((x+w)*unit), int((y+h)*unit))
		draw.Draw(img, r, &image.Uniform{shoot}, image.Point{}, draw.Src)
	}
	// the stool
	rect(7, 24, 18, 3)
	// three shoots
	rect(9, 12, 3, 12)
	rect(14.5, 6, 3, 18)
	rect(20, 10, 3, 14)
	return img
}

func main() {
	dir := "internal/web/static/icons"
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Fatal(err)
	}
	for _, size := range []int{192, 512} {
		name := filepath.Join(dir, "icon-"+strconv.Itoa(size)+".png")
		f, err := os.Create(name)
		if err != nil {
			log.Fatal(err)
		}
		if err := png.Encode(f, icon(size)); err != nil {
			log.Fatal(err)
		}
		f.Close()
		log.Printf("wrote %s", name)
	}
}
```

```bash
cd /mnt/tera/working/programming/openDaisugi/harness/coppice
mkdir -p internal/web/static/icons
go run ./internal/web/static/_icongen/main.go internal/web/static/icons
```

- [ ] **Step 4: Write the shell files**

```html
<!-- harness/coppice/internal/web/static/index.html -->
<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
<meta name="theme-color" content="#12181a">
<title>coppice</title>
<link rel="manifest" href="/manifest.webmanifest">
<link rel="icon" href="/icons/icon-192.png">
<link rel="stylesheet" href="/app.css">
<script type="module" src="/app.js"></script>
</head>
<body>
<header id="bar">
  <button id="back" class="ghost" hidden>Back</button>
  <h1 id="title">Panes</h1>
  <button id="go-new" class="ghost">New</button>
  <button id="go-settings" class="ghost">Settings</button>
</header>

<p id="status" role="status"></p>

<section id="screen-roster">
  <ul id="roster" class="cards"></ul>
  <p id="roster-empty" class="empty" hidden>No panes yet. Tap New to start one.</p>
</section>

<section id="screen-pane" hidden>
  <div id="pane-head">
    <span id="pane-label"></span>
    <span id="pane-chip" class="chip"></span>
    <span id="pane-source" class="muted"></span>
  </div>
  <div id="ask" hidden>
    <p id="ask-summary"></p>
    <button id="allow">Allow</button>
    <button id="deny">Deny</button>
  </div>
  <div id="canvas-wrap">
    <canvas id="grid" width="360" height="480"></canvas>
    <pre id="grid-text" class="visually-hidden" aria-live="off"></pre>
  </div>
  <div id="compose">
    <textarea id="text" rows="2" placeholder="Type, then Prompt or Steer"></textarea>
    <button id="prompt">Prompt</button>
    <button id="steer">Steer</button>
    <button id="keys-toggle" class="ghost">Keys</button>
  </div>
  <div id="keys" hidden></div>
</section>

<section id="screen-new" hidden>
  <label for="new-cwd">Directory</label>
  <input id="new-cwd" list="recent-cwds" autocomplete="off">
  <datalist id="recent-cwds"></datalist>
  <label for="new-label">Label</label>
  <input id="new-label" autocomplete="off">
  <label for="new-kind">Kind</label>
  <select id="new-kind">
    <option value="pty">pty, the harness UI</option>
    <option value="headless">headless, supervised</option>
  </select>
  <label for="new-command">Command</label>
  <input id="new-command" value="claude" autocomplete="off">
  <label for="new-harness">Harness</label>
  <select id="new-harness">
    <option value="claude-code">claude-code</option>
    <option value="codex">codex</option>
    <option value="pi">pi</option>
    <option value="opencode">opencode</option>
    <option value="sprig">sprig</option>
  </select>
  <button id="create">Create</button>
</section>

<section id="screen-settings" hidden>
  <p class="muted" id="set-origin"></p>
  <label for="set-token">Token</label>
  <input id="set-token" autocomplete="off">
  <button id="save-settings">Save</button>
  <button id="forget-token" class="ghost">Forget token</button>
  <button id="test-push">Test push</button>
  <p class="muted">Push goes through your own ntfy. Web push is not built.</p>
</section>
</body>
</html>
```

```css
/* harness/coppice/internal/web/static/app.css */
:root {
  color-scheme: dark;
  --bg: #12181a; --fg: #e8eae7; --muted: #98a3a0; --line: #26312f;
  --blocked: #d98b4a; --working: #8fb996; --idle: #7f8c8a; --done: #5d6a68; --unknown: #b06868;
}
* { box-sizing: border-box; }
body { margin: 0; background: var(--bg); color: var(--fg);
  font: 16px/1.4 ui-monospace, SFMono-Regular, Menlo, monospace; }
#bar { display: flex; gap: .5rem; align-items: center; padding: .6rem .8rem;
  border-bottom: 1px solid var(--line); position: sticky; top: 0; background: var(--bg); }
#bar h1 { font-size: 1rem; margin: 0; flex: 1; }
button { min-height: 44px; min-width: 44px; padding: 0 .8rem; border: 1px solid var(--line);
  background: #1b2422; color: var(--fg); border-radius: 6px; font: inherit; }
button.ghost { background: transparent; }
#status { margin: .4rem .8rem; color: var(--muted); min-height: 1.2em; }
.cards { list-style: none; margin: 0; padding: .4rem .8rem; display: grid; gap: .5rem; }
.card { border: 1px solid var(--line); border-radius: 8px; padding: .6rem .8rem; }
.card .row { display: flex; gap: .5rem; align-items: baseline; flex-wrap: wrap; }
.card .ask { color: var(--blocked); margin: .3rem 0 0; }
.chip { border-radius: 999px; padding: .1rem .5rem; font-size: .8rem; border: 1px solid currentColor; }
.chip-blocked { color: var(--blocked); } .chip-working { color: var(--working); }
.chip-idle { color: var(--idle); } .chip-done { color: var(--done); } .chip-unknown { color: var(--unknown); }
.muted { color: var(--muted); }
.empty { padding: 2rem .8rem; color: var(--muted); }
#canvas-wrap { overflow: hidden; touch-action: none; background: #0b0f10; }
canvas { display: block; }
#compose { display: flex; gap: .4rem; padding: .5rem .8rem; align-items: flex-end; flex-wrap: wrap; }
#compose textarea { flex: 1 1 60%; min-width: 10rem; background: #1b2422; color: var(--fg);
  border: 1px solid var(--line); border-radius: 6px; font: inherit; padding: .4rem; }
#keys { display: flex; gap: .4rem; padding: 0 .8rem .6rem; flex-wrap: wrap; }
#ask { padding: .6rem .8rem; border-bottom: 1px solid var(--line); }
#pane-head { display: flex; gap: .5rem; align-items: baseline; padding: .5rem .8rem; flex-wrap: wrap; }
section label { display: block; margin: .8rem .8rem .2rem; color: var(--muted); }
section input, section select { width: calc(100% - 1.6rem); margin: 0 .8rem; min-height: 44px;
  background: #1b2422; color: var(--fg); border: 1px solid var(--line); border-radius: 6px; font: inherit; padding: 0 .5rem; }
section > button { margin: .8rem; }
.visually-hidden { position: absolute; width: 1px; height: 1px; overflow: hidden; clip-path: inset(50%); }
```

```js
// harness/coppice/internal/web/static/sw-policy.js

// The shell is cached so the app opens with no network. Pane content never
// is: a stale frame is worse than an empty one, and a cached ask is a lie
// about what the gate is holding right now.
export const SHELL = [
  '/', '/app.css', '/app.js', '/grid.js', '/roster.js', '/pane.js',
  '/newpane.js', '/settings.js', '/manifest.webmanifest', '/icons/icon-192.png',
];

export function shouldCache(path) {
  return SHELL.includes(path);
}
```

```js
// harness/coppice/internal/web/static/sw.js
import { SHELL, shouldCache } from './sw-policy.js';

const CACHE = 'coppice-shell-v1';

self.addEventListener('install', (e) => {
  e.waitUntil(caches.open(CACHE).then((c) => c.addAll(SHELL)).then(() => self.skipWaiting()));
});

self.addEventListener('activate', (e) => {
  e.waitUntil(
    caches.keys()
      .then((keys) => Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k))))
      .then(() => self.clients.claim()),
  );
});

self.addEventListener('fetch', (e) => {
  const url = new URL(e.request.url);
  if (e.request.method !== 'GET' || url.origin !== self.location.origin || !shouldCache(url.pathname)) {
    return; // straight to the network, and never stored
  }
  e.respondWith(caches.match(e.request).then((hit) => hit || fetch(e.request)));
});
```

```json
{
  "name": "coppice",
  "short_name": "coppice",
  "start_url": "/",
  "scope": "/",
  "display": "standalone",
  "background_color": "#12181a",
  "theme_color": "#12181a",
  "icons": [
    { "src": "/icons/icon-192.png", "sizes": "192x192", "type": "image/png", "purpose": "any" },
    { "src": "/icons/icon-512.png", "sizes": "512x512", "type": "image/png", "purpose": "any maskable" }
  ]
}
```

```js
// harness/coppice/internal/web/static/app.js

// The shell: one websocket, one request map, four screens. Every screen is a
// section in index.html; routing is the hash, so the back button works and a
// notification can link straight at a pane.

export const WS_SUBPROTOCOL = 'daisugi.v1';
export const WS_BEARER_PREFIX = 'daisugi.bearer.';

// A browser cannot set headers on a WebSocket, so the token rides a second
// subprotocol offer. The server negotiates the first name only.
export function bearerProtocols(token) {
  return [WS_SUBPROTOCOL, WS_BEARER_PREFIX + token];
}

// route turns a location hash into a screen and its argument.
export function route(hash) {
  const h = (hash || '').replace(/^#/, '');
  if (h.startsWith('/pane/')) return { screen: 'pane', pane: decodeURIComponent(h.slice('/pane/'.length)) };
  if (h === '/new') return { screen: 'new' };
  if (h === '/settings') return { screen: 'settings' };
  return { screen: 'roster' };
}

const state = {
  token: null,
  ws: null,
  seq: 0,
  pending: new Map(),
  panes: [],
  onEvent: null,
};

function status(msg) {
  const el = document.getElementById('status');
  if (el) el.textContent = msg || '';
}

// rpc sends one §3.3 request and resolves with the reply that echoes its id.
function rpc(cmd, fields = {}) {
  return new Promise((resolve, reject) => {
    if (!state.ws || state.ws.readyState !== WebSocket.OPEN) {
      reject(new Error('Not connected. Check the token in Settings.'));
      return;
    }
    const id = 'w' + ++state.seq;
    state.pending.set(id, { resolve, reject });
    state.ws.send(JSON.stringify({ id, cmd, ...fields }));
    setTimeout(() => {
      if (state.pending.delete(id)) reject(new Error(cmd + ' timed out'));
    }, 15000);
  });
}

const RETRY_MIN_MS = 2000;
const RETRY_MAX_MS = 30000;
let retryMs = RETRY_MIN_MS;
let giveUp = false;

// afterClose decides whether reconnecting is worth doing. Retrying a rejected
// token every two seconds trips the server's own ban after three tries, and
// the operator's phone then spends a minute at 429 with "Reconnecting." on
// screen and nothing that says why. So ask once what happened.
async function afterClose() {
  let probe = null;
  try {
    probe = await fetch('/api/token/check', {
      headers: { Authorization: 'Bearer ' + state.token },
    });
  } catch {
    probe = null;   // the box is unreachable, which is a normal retry
  }
  if (probe && probe.status === 401) {
    giveUp = true;
    status('That token is not accepted. Run coppice web token and scan the QR again.');
    return;
  }
  if (probe && probe.status === 429) {
    status('Too many bad tokens. Waiting one minute.');
    setTimeout(connect, 60000);
    return;
  }
  status('Reconnecting.');
  setTimeout(connect, retryMs);
  retryMs = Math.min(RETRY_MAX_MS, retryMs * 2);
}

function connect() {
  if (giveUp) return;
  if (!state.token) { status('No token. Open Settings and paste one.'); return; }
  // Task 11 replaces these three lines with the tested wsUrl() helper, once
  // settings.js exists to hold it.
  const url = new URL(location.origin);
  url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:';
  url.pathname = '/ws';
  const ws = new WebSocket(url.toString(), bearerProtocols(state.token));
  state.ws = ws;
  ws.addEventListener('open', () => {
    status('');
    retryMs = RETRY_MIN_MS;
    rpc('events.subscribe', { panes: '*', kinds: ['state', 'layout'] }).catch(() => {});
    window.dispatchEvent(new CustomEvent('coppice:open'));
  });
  ws.addEventListener('message', (e) => {
    let msg;
    try { msg = JSON.parse(e.data); } catch { return; }
    if (msg.id && state.pending.has(msg.id)) {
      const { resolve, reject } = state.pending.get(msg.id);
      state.pending.delete(msg.id);
      if (msg.ok === false) reject(new Error((msg.error && msg.error.message) || 'refused'));
      else resolve(msg.result || {});
      return;
    }
    if (state.onEvent) state.onEvent(msg);
  });
  ws.addEventListener('close', afterClose);
}

async function api(path, options = {}) {
  const headers = Object.assign({ Authorization: 'Bearer ' + state.token }, options.headers || {});
  const resp = await fetch(path, Object.assign({}, options, { headers }));
  const body = await resp.json().catch(() => ({}));
  if (!resp.ok) throw new Error(body.error || ('HTTP ' + resp.status));
  return body;
}

function show(name) {
  for (const s of ['roster', 'pane', 'new', 'settings']) {
    document.getElementById('screen-' + s).hidden = s !== name;
  }
  document.getElementById('back').hidden = name === 'roster';
}

window.coppice = { rpc, api, state, show, status, route, connect };

function boot() {
  const fromHash = /^#t=([A-Za-z0-9_-]{43})$/.exec(location.hash);
  if (fromHash) {
    localStorage.setItem('coppice.token', fromHash[1]);
    history.replaceState(null, '', '#/roster');
  }
  state.token = localStorage.getItem('coppice.token');

  document.getElementById('back').addEventListener('click', () => { location.hash = '#/roster'; });
  document.getElementById('go-new').addEventListener('click', () => { location.hash = '#/new'; });
  document.getElementById('go-settings').addEventListener('click', () => { location.hash = '#/settings'; });
  window.addEventListener('hashchange', () => window.dispatchEvent(new CustomEvent('coppice:route')));

  if ('serviceWorker' in navigator) {
    navigator.serviceWorker.register('/sw.js', { type: 'module' }).catch(() => {});
  }
  connect();
  window.dispatchEvent(new CustomEvent('coppice:route'));
}

if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', boot);
else boot();
```

Create the five modules tasks 10 and 11 fill, each as a real file so the embed list is honest today:

```js
// harness/coppice/internal/web/static/grid.js
// Filled by task 10.
export const ATTR = { BOLD: 1, ITALIC: 2, UNDERLINE: 4, INVERSE: 8 };
```

```js
// harness/coppice/internal/web/static/roster.js
// Filled by task 10.
```

```js
// harness/coppice/internal/web/static/pane.js
// Filled by task 10.
```

```js
// harness/coppice/internal/web/static/newpane.js
// Filled by task 11.
```

```js
// harness/coppice/internal/web/static/settings.js
// Filled by task 11.
```

- [ ] **Step 5: Write `static.go` and register `/`**

```go
// harness/coppice/internal/web/static.go
package web

import (
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"strings"
)

// The directive has no all: prefix on purpose. Go leaves out entries whose
// name starts with _ or ., which is what keeps static/_tests and
// static/_icongen out of the binary.
//
//go:embed static
var staticFS embed.FS

// StaticFiles is the shipped PWA, rooted at static/.
func StaticFiles() fs.FS {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	return sub
}

// csp keeps every script and style in a file. Nothing inline means a stray
// injected script has nowhere to run, which matters on a page that holds the
// operator's token.
const csp = "default-src 'self'; connect-src 'self'; img-src 'self' data:; " +
	"script-src 'self'; style-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// StaticHandler serves the shell. There is no token on this path: the shell
// has to load before the operator has pasted one.
func StaticHandler() (http.Handler, error) {
	if err := mime.AddExtensionType(".webmanifest", "application/manifest+json"); err != nil {
		return nil, err
	}
	files := http.FileServerFS(StaticFiles())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/_") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", csp)
		w.Header().Set("Referrer-Policy", "no-referrer")
		files.ServeHTTP(w, r)
	}), nil
}
```

In `New`, after `s.routes()`:

```go
	static, err := StaticHandler()
	if err != nil {
		return nil, err
	}
	s.mux.Handle("/", static)
```

- [ ] **Step 6: Run the tests and watch them pass**

```bash
cd /mnt/tera/working/programming/openDaisugi/harness/coppice && go test ./internal/web/ -v && go vet ./...
```

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
cd /mnt/tera/working/programming/openDaisugi
git add harness/coppice/internal/web/static.go harness/coppice/internal/web/static_test.go harness/coppice/internal/web/static/ harness/coppice/internal/web/server.go
git commit -m "coppice/web: the app shell, embedded, with no bundler and no inline script

One binary carries the whole client, so there is nothing to build and nothing
to serve from anywhere else. The content policy forbids inline script and
style, which is why every handler lives in a module file: the page holds the
operator's token and a stray injected script must have nowhere to run. The
service worker caches the shell and only the shell. A cached frame would be a
stale picture of a live pane, and a cached ask would be a lie about what the
gate is holding right now."
```

---

### Task 10: The Roster and the Pane screen

**Files:**
- Modify: `harness/coppice/internal/web/static/grid.js`
- Modify: `harness/coppice/internal/web/static/roster.js`
- Modify: `harness/coppice/internal/web/static/pane.js`
- Modify: `harness/coppice/internal/web/static/app.js` (import and mount the two screens)
- Create: `harness/coppice/internal/web/static/_tests/fixtures/pane_list.json`
- Create: `harness/coppice/internal/web/static/_tests/grid.test.mjs`
- Create: `harness/coppice/internal/web/static/_tests/roster.test.mjs`
- Create: `harness/coppice/internal/web/static/_tests/pane.test.mjs`
- Create: `harness/coppice/internal/web/js_test.go`

**Interfaces:**
- Consumes: `window.coppice.rpc`, `window.coppice.api`, `window.coppice.show`, `window.coppice.status`, `route`, `bearerProtocols` (task 9).
- Produces:
  - `grid.js`: `export const ATTR`, `export function newGrid(cols, rows)`, `export function applyFrame(grid, frame)`, `export function gridToText(grid)`, `export function drawGrid(ctx, grid, cellPx)`
  - `roster.js`: `export function sortPanes(panes)`, `export function stateChip(state)`, `export function ageLabel(seconds)`, `export function paneRows(raw, nowSeconds)`, `export function render(panes)`, `export function mountRoster()`
  - `pane.js`: `export function attachCommand(pane)`, `export function promptCommand(pane, text)`, `export function steerCommand(pane, text)`, `export function keysCommand(pane, key)`, `export const KEYS`, `export function stateOf(row)` (reads plan 02's **flat** shape), `export function askFor(st)`, `export function open(paneId)`, `export function close()`, `export function onEvent(msg)`, `export function mountPane()`

- [ ] **Step 1: Write the fixture the client is held to**

This is a literal `pane.list` reply from plan 02's `handlePaneList`, with the `ts` task 0 added.
Three rows on purpose: one working, one blocked with an ask, and one the state store has never
seen, which is where `source` is `null` and there is no `ts` at all. Every roster and pane
assertion below runs against this file rather than against a shape someone remembered.

```json
{
  "id": "2",
  "ok": true,
  "result": {
    "panes": [
      {
        "id": "w1:p1", "label": "auth fix", "cwd": "/repo", "cmd": ["claude"],
        "kind": "pty", "harness": "claude-code",
        "workspace": "w1", "tab": "t1", "closed": false,
        "cols": 120, "rows": 40,
        "state": "working", "source": "gate", "detail": "", "ts": 1757300000.0
      },
      {
        "id": "w1:p2", "label": "db migration", "cwd": "/repo", "cmd": ["codex"],
        "kind": "headless", "harness": "codex",
        "workspace": "w1", "tab": "t1", "closed": false,
        "cols": 120, "rows": 40,
        "state": "blocked", "source": "gate",
        "detail": "verdict=deny clause=shell.deny[2]", "ts": 1757300030.0,
        "ask": {
          "id": "toolu_01ABC", "tool": "Bash",
          "summary": "rm -rf build/", "deadline": 1757300090.0
        }
      },
      {
        "id": "w1:p3", "label": "shell", "cwd": "/repo", "cmd": ["sh"],
        "kind": "pty", "harness": "shell",
        "workspace": "w1", "tab": "t1", "closed": false,
        "cols": 120, "rows": 40,
        "state": "unknown", "source": null, "detail": ""
      }
    ]
  }
}
```

Check it against the handler before trusting it:

```bash
cd /mnt/tera/working/programming/openDaisugi/harness/coppice
sed -n '/func (s \*Server) handlePaneList/,/^}/p' internal/server/*.go
```

Every key in the fixture must appear in that function, and every key that function always sets
must appear in the fixture. If plan 02 has since added a field, add it here too.

- [ ] **Step 2: Write the failing JS tests**

```js
// harness/coppice/internal/web/static/_tests/grid.test.mjs
import test from 'node:test';
import assert from 'node:assert/strict';
import { ATTR, newGrid, applyFrame, gridToText } from '../grid.js';

test('the attrs bits match what PINS.md records', () => {
  assert.equal(ATTR.BOLD, 1);
  assert.equal(ATTR.ITALIC, 2);
  assert.equal(ATTR.UNDERLINE, 4);
  assert.equal(ATTR.INVERSE, 8);
});

test('a first full frame fills the grid', () => {
  const frame = {
    pane: 'w1:p1', seq: 1, cols: 3, rows: 2, cursor: [1, 0],
    rows_changed: { 0: [['h', '', '', 0], ['i', '', '', 0], [' ', '', '', 0]], 1: [['o', '', '', 0], ['k', '', '', 0], [' ', '', '', 0]] },
  };
  const g = applyFrame(null, frame);
  assert.equal(gridToText(g), 'hi\nok');
  assert.deepEqual(g.cursor, [1, 0]);
});

test('a later frame changes only the rows it names', () => {
  let g = applyFrame(null, {
    cols: 2, rows: 2, cursor: [0, 0],
    rows_changed: { 0: [['a', '', '', 0], ['b', '', '', 0]], 1: [['c', '', '', 0], ['d', '', '', 0]] },
  });
  g = applyFrame(g, { cols: 2, rows: 2, cursor: [0, 1], rows_changed: { 1: [['x', '', '', 0], ['y', '', '', 0]] } });
  assert.equal(gridToText(g), 'ab\nxy');
});

// The failure this names: a frame that names a row outside the grid must not
// throw and blank the whole pane.
test('a row index outside the grid is dropped, not thrown', () => {
  const g = applyFrame(null, { cols: 2, rows: 1, cursor: [0, 0], rows_changed: { 0: [['a', '', '', 0], ['b', '', '', 0]], 9: [['z', '', '', 0]] } });
  assert.equal(gridToText(g), 'ab');
});

test('a resize starts a fresh grid rather than mixing two sizes', () => {
  let g = applyFrame(null, { cols: 2, rows: 1, cursor: [0, 0], rows_changed: { 0: [['a', '', '', 0], ['b', '', '', 0]] } });
  g = applyFrame(g, { cols: 4, rows: 1, cursor: [0, 0], rows_changed: { 0: [['w', '', '', 0], ['x', '', '', 0], ['y', '', '', 0], ['z', '', '', 0]] } });
  assert.equal(g.cols, 4);
  assert.equal(gridToText(g), 'wxyz');
});
```

```js
// harness/coppice/internal/web/static/_tests/roster.test.mjs
import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { sortPanes, stateChip, ageLabel, paneRows } from '../roster.js';

const REPLY = JSON.parse(readFileSync(new URL('./fixtures/pane_list.json', import.meta.url), 'utf8'));
const PANES = REPLY.result.panes;
const NOW = 1757300060;

// These four run against plan 02's literal pane.list reply. The shape is
// flat: state is a string on the row, not a nested PaneStateEvent. Reading
// the nested shape would make every pane unknown, put no blocked pane first,
// and leave askSummary empty, so the roster would look calm while the gate
// was holding an ask.
test('a real pane.list row reads as the state it says it is', () => {
  const rows = paneRows(PANES, NOW);
  const blocked = rows.find((r) => r.id === 'w1:p2');
  assert.equal(blocked.state, 'blocked');
  assert.equal(blocked.harness, 'codex');
  assert.equal(blocked.source, 'gate');
  assert.equal(blocked.label, 'db migration');
});

test('a real blocked row carries its ask summary to the card', () => {
  const blocked = paneRows(PANES, NOW).find((r) => r.id === 'w1:p2');
  assert.equal(blocked.askSummary, 'rm -rf build/');
  assert.notEqual(blocked.askSummary, '');
});

test('a real pane.list puts the blocked pane at the top', () => {
  const order = sortPanes(paneRows(PANES, NOW)).map((r) => r.id);
  assert.equal(order[0], 'w1:p2');
  assert.deepEqual(order, ['w1:p2', 'w1:p1', 'w1:p3']);
});

test('a row the state store has never seen shows unknown with no age', () => {
  const rows = paneRows(PANES, NOW);
  const fresh = rows.find((r) => r.id === 'w1:p3');
  assert.equal(fresh.state, 'unknown');
  assert.equal(fresh.age, null);              // no ts on the row, so no age is invented
  assert.equal(stateChip(fresh.state).word, 'unknown');
  const working = rows.find((r) => r.id === 'w1:p1');
  assert.equal(working.age, 60);              // NOW minus the row's ts
});

test('blocked panes come first and the rest keep their order', () => {
  const panes = [
    { id: 'a', state: 'working' },
    { id: 'b', state: 'idle' },
    { id: 'c', state: 'blocked' },
    { id: 'd', state: 'working' },
  ];
  assert.deepEqual(sortPanes(panes).map((p) => p.id), ['c', 'a', 'd', 'b']);
});

// Master 3.1: unknown is what the floor shows when it has no source, and it
// is never dressed up as idle.
test('an unknown or missing state reads as unknown, never idle', () => {
  assert.equal(stateChip('unknown').word, 'unknown');
  assert.equal(stateChip(undefined).word, 'unknown');
  assert.equal(stateChip('nonsense').word, 'unknown');
});

// Colour alone is not a signal. Every chip carries the word too.
test('every chip carries a word as well as a class', () => {
  for (const s of ['blocked', 'working', 'idle', 'done', 'unknown']) {
    const chip = stateChip(s);
    assert.equal(chip.word, s);
    assert.ok(chip.cls.includes('chip-' + s));
  }
});

test('ages read in the largest useful unit', () => {
  assert.equal(ageLabel(4), '4s');
  assert.equal(ageLabel(125), '2m');
  assert.equal(ageLabel(7300), '2h');
  assert.equal(ageLabel(200000), '2d');
  assert.equal(ageLabel(-5), '0s');
});
```

```js
// harness/coppice/internal/web/static/_tests/pane.test.mjs
import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { attachCommand, promptCommand, steerCommand, keysCommand, KEYS, stateOf, askFor } from '../pane.js';

const PANES = JSON.parse(
  readFileSync(new URL('./fixtures/pane_list.json', import.meta.url), 'utf8'),
).result.panes;
const blockedRow = PANES.find((p) => p.id === 'w1:p2');
const workingRow = PANES.find((p) => p.id === 'w1:p1');

// One state event exactly as plan 02's stateEvent type puts it on the wire:
// the envelope key plus a flattened PaneStateEvent, same field names as a
// pane.list row.
const stateEvent = {
  event: 'state', v: 1, ts: 1757300030.0, session_id: 'd41c', harness_session_id: null,
  harness: 'codex', pane: 'w1:p2', state: 'blocked', source: 'gate',
  detail: 'verdict=deny clause=shell.deny[2]',
  ask: { id: 'toolu_01ABC', tool: 'Bash', summary: 'rm -rf build/', deadline: 1757300090.0 },
};

// The failure this names: reading pane.list as a nested PaneStateEvent makes
// stateOf return unknown for every pane, and the Allow and Deny box never
// appears on the pane the operator just tapped. A gate blocked is one shot,
// so there is no second event to rescue it.
test('stateOf reads plan 02 flat pane.list row', () => {
  const st = stateOf(blockedRow);
  assert.equal(st.state, 'blocked');
  assert.equal(st.source, 'gate');
  assert.equal(st.harness, 'codex');
  assert.equal(st.ts, 1757300030.0);
  assert.equal(st.ask.id, 'toolu_01ABC');
});

test('stateOf reads a state event the same way, with no wrapper', () => {
  const st = stateOf(stateEvent);
  assert.equal(st.state, 'blocked');
  assert.equal(st.ask.summary, 'rm -rf build/');
  assert.equal(st.harness, 'codex');
});

test('the Allow and Deny box opens on a real blocked row and carries the ask id', () => {
  const ask = askFor(stateOf(blockedRow));
  assert.equal(ask.visible, true);
  assert.equal(ask.summary, 'rm -rf build/');
  assert.equal(ask.askId, 'toolu_01ABC');
});

test('the Allow and Deny box stays shut on a pane that is not blocked', () => {
  assert.equal(askFor(stateOf(workingRow)).visible, false);
  assert.equal(askFor(stateOf(undefined)).visible, false);
  // Blocked with no ask on the row is still not something to answer.
  assert.equal(askFor(stateOf({ state: 'blocked' })).visible, false);
});


// Ruling 4: the same pane is on the operator's terminal. The phone watches
// and zooms; it never reshapes.
test('attach never sends a size', () => {
  const req = attachCommand({ id: 'w1:p1' });
  assert.equal(req.cmd, 'pane.attach');
  assert.equal(req.pane, 'w1:p1');
  assert.equal('cols' in req, false);
  assert.equal('rows' in req, false);
});

// Ruling 5: plan 02 defines agent.prompt as "send text", and a shell pane
// answers it too, so there is one command and no branch to get wrong.
test('prompt is agent.prompt whatever the pane is', () => {
  for (const pane of [{ id: 'w1:p1' }, { id: 'w1:p2', kind: 'headless' }, { id: 'w1:p3', kind: 'pty' }]) {
    const req = promptCommand(pane, 'run the tests');
    assert.equal(req.cmd, 'agent.prompt');
    assert.equal(req.pane, pane.id);
    assert.equal(req.text, 'run the tests');
  }
});

test('steer is raw text with no turn boundary', () => {
  const req = steerCommand({ id: 'w1:p1' }, 'stop, do the other thing');
  assert.equal(req.cmd, 'pane.send_text');
  assert.equal(req.enter, true);
});

test('the keys drawer sends the four names the backends map', () => {
  assert.deepEqual(KEYS, ['enter', 'esc', 'ctrl+c', 'tab']);
  assert.deepEqual(keysCommand({ id: 'w1:p1' }, 'ctrl+c'), { cmd: 'pane.send_keys', pane: 'w1:p1', keys: ['ctrl+c'] });
});
```

```go
// harness/coppice/internal/web/js_test.go
package web

import (
	"os/exec"
	"strings"
	"testing"
)

// The client's pure logic is JavaScript, so its unit tests are Node's. This
// runs them inside go test when node is here and skips with the reason when
// it is not, which is the rule for every live test in this work.
func TestJSUnitTestsPass(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not on PATH, so the client's JS unit tests did not run")
	}
	out, err := exec.Command("node", "--test", "static/_tests/").CombinedOutput()
	if err != nil {
		t.Fatalf("node --test failed:\n%s", out)
	}
	if !strings.Contains(string(out), "pass ") {
		t.Fatalf("node --test ran no tests:\n%s", out)
	}
}
```

- [ ] **Step 3: Run the tests and watch them fail**

```bash
cd /mnt/tera/working/programming/openDaisugi/harness/coppice/internal/web && node --test static/_tests/
```

Expected: FAIL, `sortPanes is not exported`, `attachCommand is not exported`.

- [ ] **Step 4: Write `grid.js`**

```js
// harness/coppice/internal/web/static/grid.js

// The pane grid, painted on a canvas. A frame is master spec 3.3's shape:
// the first one after an attach carries every row, and the rest carry only
// the rows that changed.

// The bit values PINS.md records for a cell's attrs field.
export const ATTR = { BOLD: 1, ITALIC: 2, UNDERLINE: 4, INVERSE: 8 };

const BLANK = [' ', '', '', 0];

export function newGrid(cols, rows) {
  return {
    cols, rows, cursor: [0, 0],
    cells: Array.from({ length: rows }, () => Array.from({ length: cols }, () => BLANK)),
  };
}

// applyFrame folds one frame into the grid and returns it. A frame for a
// different size starts a fresh grid rather than mixing two shapes, and a row
// index outside the grid is dropped: a malformed frame must not blank a pane
// the operator is reading.
export function applyFrame(grid, frame) {
  let g = grid;
  if (!g || g.cols !== frame.cols || g.rows !== frame.rows) g = newGrid(frame.cols, frame.rows);
  const changed = frame.rows_changed || {};
  for (const key of Object.keys(changed)) {
    const y = Number(key);
    if (!Number.isInteger(y) || y < 0 || y >= g.rows) continue;
    const row = changed[key] || [];
    for (let x = 0; x < g.cols; x++) g.cells[y][x] = row[x] || BLANK;
  }
  if (Array.isArray(frame.cursor)) g.cursor = [frame.cursor[0] | 0, frame.cursor[1] | 0];
  return g;
}

// gridToText is what the hidden mirror in the page shows. A canvas is opaque
// to a screen reader and to a test, and this is the fix for both.
export function gridToText(grid) {
  return grid.cells
    .map((row) => row.map((c) => (c && c[0]) || ' ').join('').replace(/\s+$/, ''))
    .join('\n');
}

export function drawGrid(ctx, grid, cellPx) {
  const w = Math.max(4, Math.round(cellPx * 0.6));
  const h = Math.max(6, Math.round(cellPx));
  ctx.canvas.width = grid.cols * w;
  ctx.canvas.height = grid.rows * h;
  ctx.font = h - 2 + 'px ui-monospace, Menlo, monospace';
  ctx.textBaseline = 'top';
  ctx.fillStyle = '#0b0f10';
  ctx.fillRect(0, 0, ctx.canvas.width, ctx.canvas.height);
  for (let y = 0; y < grid.rows; y++) {
    for (let x = 0; x < grid.cols; x++) {
      const [text, fg, bg, attrs] = grid.cells[y][x] || BLANK;
      const inverse = (attrs & ATTR.INVERSE) !== 0 || (grid.cursor[0] === x && grid.cursor[1] === y);
      const paper = inverse ? (fg || '#e8eae7') : (bg || '#0b0f10');
      const ink = inverse ? (bg || '#0b0f10') : (fg || '#e8eae7');
      ctx.fillStyle = paper;
      ctx.fillRect(x * w, y * h, w, h);
      if (text && text !== ' ') {
        ctx.fillStyle = ink;
        ctx.font = ((attrs & ATTR.BOLD) ? 'bold ' : '') + ((attrs & ATTR.ITALIC) ? 'italic ' : '')
          + (h - 2) + 'px ui-monospace, Menlo, monospace';
        ctx.fillText(text, x * w, y * h);
      }
      if (attrs & ATTR.UNDERLINE) {
        ctx.fillStyle = ink;
        ctx.fillRect(x * w, y * h + h - 1, w, 1);
      }
    }
  }
}
```

- [ ] **Step 5: Write `roster.js`**

```js
// harness/coppice/internal/web/static/roster.js
import { stateOf, askFor } from './pane.js';

// The roster: every pane, blocked first, each with a word as well as a
// colour. Master 3.1 says unknown is what the floor shows when it has no
// source, so an unknown state stays unknown here and never softens to idle.

const ORDER = { blocked: 0, working: 1, unknown: 2, idle: 3, done: 4 };
const WORDS = ['blocked', 'working', 'idle', 'done', 'unknown'];

export function sortPanes(panes) {
  return panes
    .map((p, i) => [p, i])
    .sort((a, b) => {
      const d = (ORDER[a[0].state] ?? 5) - (ORDER[b[0].state] ?? 5);
      return d !== 0 ? d : a[1] - b[1];
    })
    .map((e) => e[0]);
}

export function stateChip(state) {
  const word = WORDS.includes(state) ? state : 'unknown';
  return { word, cls: 'chip chip-' + word };
}

export function ageLabel(seconds) {
  const s = Math.max(0, Math.floor(seconds));
  if (s < 60) return s + 's';
  if (s < 3600) return Math.floor(s / 60) + 'm';
  if (s < 86400) return Math.floor(s / 3600) + 'h';
  return Math.floor(s / 86400) + 'd';
}

function card(pane) {
  const chip = stateChip(pane.state);
  const li = document.createElement('li');
  li.className = 'card';
  li.tabIndex = 0;
  li.dataset.pane = pane.id;

  const row = document.createElement('div');
  row.className = 'row';
  const label = document.createElement('strong');
  label.textContent = pane.label || pane.id;
  const chipEl = document.createElement('span');
  chipEl.className = chip.cls;
  chipEl.textContent = chip.word;
  const harness = document.createElement('span');
  harness.className = 'muted';
  harness.textContent = pane.harness || 'unknown';
  const age = document.createElement('span');
  age.className = 'muted';
  age.textContent = pane.age === null ? '' : ageLabel(pane.age);
  row.append(label, chipEl, harness, age);
  li.append(row);

  if (pane.state === 'blocked' && pane.askSummary) {
    const ask = document.createElement('p');
    ask.className = 'ask';
    ask.textContent = pane.askSummary;
    li.append(ask);
  }
  li.addEventListener('click', () => { location.hash = '#/pane/' + encodeURIComponent(pane.id); });
  return li;
}

export function render(panes) {
  const list = document.getElementById('roster');
  const empty = document.getElementById('roster-empty');
  list.replaceChildren(...sortPanes(panes).map(card));
  empty.hidden = panes.length > 0;
}

// paneRows turns plan 02's flat pane.list rows into what a card needs. The
// row's `ts` is the merged event's timestamp, added to handlePaneList in
// task 0; a pane the state store has never seen has none, and its age stays
// blank rather than reading as zero seconds old.
export function paneRows(raw, nowSeconds) {
  return (raw || []).map((p) => {
    const st = stateOf(p);
    return {
      id: p.id,
      label: p.label || p.id,
      state: st.state,
      source: st.source,
      harness: st.harness,
      askSummary: askFor(st).summary,
      age: st.ts ? Math.max(0, nowSeconds - st.ts) : null,
    };
  });
}

export function mountRoster() {
  const refresh = async () => {
    try {
      const result = await window.coppice.rpc('pane.list');
      window.coppice.state.panes = result.panes || [];
      render(paneRows(window.coppice.state.panes, Date.now() / 1000));
    } catch (e) {
      window.coppice.status(e.message);
    }
  };
  // A busy box produces state events faster than a phone should ask for a
  // pane list. Coalesce them: one round trip every half second is plenty
  // for a roster a human is reading.
  let timer = null;
  const soon = () => {
    if (timer) return;
    timer = setTimeout(() => { timer = null; refresh(); }, 500);
  };
  window.addEventListener('coppice:open', refresh);
  window.addEventListener('coppice:route', () => {
    if (window.coppice.route(location.hash).screen === 'roster') refresh();
  });
  window.addEventListener('coppice:state', soon);
  return refresh;
}
```

- [ ] **Step 6: Write `pane.js`**

```js
// harness/coppice/internal/web/static/pane.js
import { applyFrame, gridToText, drawGrid } from './grid.js';

// One pane, full width. The phone watches and zooms; it never resizes, so
// attach carries no size (ruling 4). Prompt is agent.prompt on a pane of
// either kind, because plan 02 defines that verb as "send text" and a shell
// pane answers it too, so there is no branch to get wrong (ruling 5).

export const KEYS = ['enter', 'esc', 'ctrl+c', 'tab'];

export function attachCommand(pane) {
  return { cmd: 'pane.attach', pane: pane.id };
}

export function promptCommand(pane, text) {
  return { cmd: 'agent.prompt', pane: pane.id, text };
}

export function steerCommand(pane, text) {
  return { cmd: 'pane.send_text', pane: pane.id, text, enter: true };
}

export function keysCommand(pane, key) {
  return { cmd: 'pane.send_keys', pane: pane.id, keys: [key] };
}

// stateOf reads the state off a coppice-server object. Both shapes that
// carry one are FLAT and use the same field names, so one reader serves
// both: a `pane.list` row ({id, label, ..., state, source, detail, harness,
// ts, ask}) and a `state` event ({event, v, ts, pane, state, source, ask,
// detail, harness}). PINS.md records both, from plan 02's handlePaneList and
// its stateEvent type.
//
// Master §3.2's `state: PaneStateEvent|None` describes the Python
// PaneBackend, not this wire. Reading the nested shape here would make
// `st.state` undefined for every pane, paint the whole roster unknown, and
// hide Allow and Deny on the pane the operator just tapped.
export function stateOf(row) {
  if (!row) return { state: 'unknown', source: '', harness: 'unknown', ts: 0, ask: null };
  return {
    state: row.state || 'unknown',
    source: row.source || '',
    harness: row.harness || 'unknown',
    ts: row.ts || 0,
    ask: row.ask || null,
  };
}

// askFor decides what the Allow and Deny box shows. Pure, so the decision
// the operator depends on is testable against a real pane.list row rather
// than only through the DOM.
export function askFor(st) {
  if (st.state !== 'blocked' || !st.ask) return { visible: false, summary: '', askId: '' };
  return {
    visible: true,
    summary: st.ask.summary || st.ask.tool || 'blocked',
    askId: st.ask.id || '',
  };
}

let current = null;   // { id }
let grid = null;
let scale = 1;
const pointers = new Map();
let pinchStart = 0;
let scaleStart = 1;

function repaint() {
  if (!grid) return;
  const canvas = document.getElementById('grid');
  drawGrid(canvas.getContext('2d'), grid, 14 * scale);
  document.getElementById('grid-text').textContent = gridToText(grid);
}

function setAsk(st) {
  const box = document.getElementById('ask');
  const chip = document.getElementById('pane-chip');
  chip.textContent = st.state;
  chip.className = 'chip chip-' + st.state;
  document.getElementById('pane-source').textContent = st.source ? 'source ' + st.source : '';
  const ask = askFor(st);
  document.getElementById('ask-summary').textContent = ask.summary;
  box.dataset.askId = ask.askId;
  box.hidden = !ask.visible;
}

async function answer(decision) {
  const askId = document.getElementById('ask').dataset.askId;
  if (!askId) return;
  try {
    await window.coppice.api('/api/ask/answer', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ tool_use_id: askId, decision, reason: 'answered from the phone' }),
    });
    window.coppice.status(decision === 'allow' ? 'Allowed.' : 'Denied.');
    document.getElementById('ask').hidden = true;
  } catch (e) {
    window.coppice.status(e.message);
  }
}

async function send(build) {
  const box = document.getElementById('text');
  const text = box.value.trim();
  if (!current || !text) return;
  const req = build(current, text);
  const { cmd, ...fields } = req;
  try {
    await window.coppice.rpc(cmd, fields);
    box.value = '';
    window.coppice.status('');
  } catch (e) {
    window.coppice.status(e.message);
  }
}

// close detaches whatever pane is open. Leaving a pane attached while the
// operator is on another screen keeps frames streaming to a phone that is
// not showing them, which costs the battery the whole feature exists to
// save.
export function close() {
  if (!current) return;
  const id = current.id;
  current = null;
  grid = null;
  window.coppice.rpc('pane.detach', { pane: id }).catch(() => {});
}

export async function open(paneId) {
  if (current && current.id === paneId) return;   // a repeat route event must not blank the grid
  if (current) close();
  current = { id: paneId };
  grid = null;
  scale = 1;
  const info = (window.coppice.state.panes || []).find((p) => p.id === paneId) || { id: paneId };
  document.getElementById('pane-label').textContent = info.label || paneId;
  setAsk(stateOf(info));
  const req = attachCommand(current);
  const { cmd, ...fields } = req;
  try {
    await window.coppice.rpc(cmd, fields);
  } catch (e) {
    window.coppice.status(e.message);
  }
}

export function onEvent(msg) {
  if (msg.event === 'frame' && current && msg.pane === current.id) {
    grid = applyFrame(grid, msg);
    repaint();
    return;
  }
  if (msg.event === 'state') {
    // A state event is already flat. Wrapping it would break stateOf the
    // same way reading pane.list as nested does.
    if (current && msg.pane === current.id) setAsk(stateOf(msg));
    window.dispatchEvent(new CustomEvent('coppice:state'));
  }
}

export function mountPane() {
  document.getElementById('prompt').addEventListener('click', () => send(promptCommand));
  document.getElementById('steer').addEventListener('click', () => send(steerCommand));
  document.getElementById('allow').addEventListener('click', () => answer('allow'));
  document.getElementById('deny').addEventListener('click', () => answer('deny'));

  const drawer = document.getElementById('keys');
  drawer.replaceChildren(...KEYS.map((key) => {
    const b = document.createElement('button');
    b.textContent = key;
    b.addEventListener('click', () => {
      if (!current) return;
      const { cmd, ...fields } = keysCommand(current, key);
      window.coppice.rpc(cmd, fields).catch((e) => window.coppice.status(e.message));
    });
    return b;
  }));
  document.getElementById('keys-toggle').addEventListener('click', () => { drawer.hidden = !drawer.hidden; });

  // Pinch to zoom the grid. The pane keeps its size on the server; only the
  // picture changes.
  const wrap = document.getElementById('canvas-wrap');
  wrap.addEventListener('pointerdown', (e) => {
    pointers.set(e.pointerId, e);
    if (pointers.size === 2) {
      const [a, b] = [...pointers.values()];
      pinchStart = Math.hypot(a.clientX - b.clientX, a.clientY - b.clientY);
      scaleStart = scale;
    }
  });
  wrap.addEventListener('pointermove', (e) => {
    if (!pointers.has(e.pointerId)) return;
    pointers.set(e.pointerId, e);
    if (pointers.size === 2 && pinchStart > 0) {
      const [a, b] = [...pointers.values()];
      const now = Math.hypot(a.clientX - b.clientX, a.clientY - b.clientY);
      scale = Math.min(3, Math.max(0.4, scaleStart * (now / pinchStart)));
      repaint();
    }
  });
  const drop = (e) => { pointers.delete(e.pointerId); if (pointers.size < 2) pinchStart = 0; };
  wrap.addEventListener('pointerup', drop);
  wrap.addEventListener('pointercancel', drop);

  window.addEventListener('coppice:route', () => {
    const r = window.coppice.route(location.hash);
    if (r.screen === 'pane') open(r.pane);
    else close();
  });
}
```

- [ ] **Step 7: Mount both screens in `app.js`**

Add the imports and the wiring at the top of `boot()`:

```js
import { mountRoster } from './roster.js';
import { mountPane, onEvent as paneEvent } from './pane.js';
```

and inside `boot()`, before `connect()`:

```js
  mountRoster();
  mountPane();
  state.onEvent = paneEvent;
  window.addEventListener('coppice:route', () => {
    const r = route(location.hash);
    show(r.screen);
    document.getElementById('title').textContent =
      { roster: 'Panes', pane: 'Pane', new: 'New pane', settings: 'Settings' }[r.screen];
  });
```

- [ ] **Step 8: Run the tests and watch them pass**

```bash
cd /mnt/tera/working/programming/openDaisugi/harness/coppice/internal/web && node --test static/_tests/
cd /mnt/tera/working/programming/openDaisugi/harness/coppice && go test ./... && go vet ./...
```

Expected: PASS on both.

- [ ] **Step 9: Commit**

```bash
cd /mnt/tera/working/programming/openDaisugi
git add harness/coppice/internal/web/static/ harness/coppice/internal/web/js_test.go
git commit -m "coppice/web: the roster and the pane, read from the shape the server sends

stateOf reads plan 02's flat pane.list row, where the state is a string on
the row. Master 3.2's nested PaneStateEvent describes the Python backend, not
this wire, and a client that read it that way would show every pane as
unknown and hide Allow and Deny on the pane the operator just tapped. A gate
blocked is one shot, so there would be no second event to rescue it. The
tests run against a literal pane.list reply for that reason. Attach sends no
size, because the same pane is open in the operator's terminal and a phone
that reshaped it would wreck the screen they are reading. Prompt is
agent.prompt on a pane of either kind, on plan 02's own reading of the verb.
The hidden text mirror under the canvas is there for screen readers, and it
is also the only way a test can read a canvas."
```

---

### Task 11: The New and Settings screens

**Files:**
- Modify: `harness/coppice/internal/web/static/newpane.js`
- Modify: `harness/coppice/internal/web/static/settings.js`
- Modify: `harness/coppice/internal/web/static/app.js` (mount both)
- Create: `harness/coppice/internal/web/static/_tests/newpane.test.mjs`
- Create: `harness/coppice/internal/web/static/_tests/settings.test.mjs`

**Interfaces:**
- Consumes: `window.coppice.rpc`, `window.coppice.api`, `window.coppice.state`, `window.coppice.status` (task 9).
- Produces:
  - `newpane.js`: `export function createCommand(form)`, `export function recentCwds(panes, stored)`, `export function mountNew()`
  - `settings.js`: `export function wsUrl(origin)`, `export function tokenFromHash(hash)`, `export function mountSettings()`

- [ ] **Step 1: Write the failing JS tests**

```js
// harness/coppice/internal/web/static/_tests/newpane.test.mjs
import test from 'node:test';
import assert from 'node:assert/strict';
import { createCommand, recentCwds } from '../newpane.js';

test('a pty pane sends the command line as argv', () => {
  const req = createCommand({ cwd: '/repo', label: 'auth fix', kind: 'pty', command: 'claude --resume', harness: 'claude-code' });
  assert.deepEqual(req, {
    cmd: 'pane.create', cwd: '/repo', label: 'auth fix', kind: 'pty',
    env: {}, cmd_argv: ['claude', '--resume'],
  });
});

test('a headless pane sends the harness name instead of argv', () => {
  const req = createCommand({ cwd: '/repo', label: '', kind: 'headless', command: 'ignored', harness: 'codex' });
  assert.equal(req.kind, 'headless');
  assert.equal(req.harness, 'codex');
  assert.deepEqual(req.cmd_argv, []);
});

// The failure this names: a pane created in the wrong directory does real
// work in the wrong repository.
test('a blank directory is refused rather than defaulted', () => {
  assert.throws(() => createCommand({ cwd: '  ', kind: 'pty', command: 'claude' }), /directory/i);
});

test('recent directories put the live panes first and keep ten', () => {
  const panes = [{ cwd: '/a' }, { cwd: '/b' }, { cwd: '/a' }];
  const stored = ['/b', '/c', '/d'];
  assert.deepEqual(recentCwds(panes, stored), ['/a', '/b', '/c', '/d']);
  const many = Array.from({ length: 30 }, (_, i) => '/dir' + i);
  assert.equal(recentCwds([], many).length, 10);
});
```

```js
// harness/coppice/internal/web/static/_tests/settings.test.mjs
import test from 'node:test';
import assert from 'node:assert/strict';
import { wsUrl, tokenFromHash } from '../settings.js';

test('the websocket follows the page scheme', () => {
  assert.equal(wsUrl('https://box.tail1234.ts.net:8443'), 'wss://box.tail1234.ts.net:8443/ws');
  assert.equal(wsUrl('http://127.0.0.1:8443'), 'ws://127.0.0.1:8443/ws');
});

// The failure this names: storing junk from the hash would leave the app
// permanently unable to connect with no way to tell why.
test('only a well formed token is taken from the hash', () => {
  const good = 'A'.repeat(43);
  assert.equal(tokenFromHash('#t=' + good), good);
  assert.equal(tokenFromHash('#t=short'), null);
  assert.equal(tokenFromHash('#/roster'), null);
  assert.equal(tokenFromHash(''), null);
  assert.equal(tokenFromHash('#t=' + 'A'.repeat(42) + '='), null);
});
```

- [ ] **Step 2: Run the tests and watch them fail**

```bash
cd /mnt/tera/working/programming/openDaisugi/harness/coppice/internal/web && node --test static/_tests/
```

Expected: FAIL, `createCommand is not exported`, `wsUrl is not exported`.

- [ ] **Step 3: Write `newpane.js`**

```js
// harness/coppice/internal/web/static/newpane.js

// Start a pane from the kitchen. The directory is the one field that must
// not have a default: a pane started in the wrong repository does real work
// in the wrong place.

const MAX_RECENT = 10;

export function createCommand(form) {
  const cwd = (form.cwd || '').trim();
  if (!cwd) throw new Error('Pick a directory first.');
  const req = {
    cmd: 'pane.create',
    cwd,
    label: (form.label || '').trim(),
    kind: form.kind === 'headless' ? 'headless' : 'pty',
    env: {},
  };
  if (req.kind === 'headless') {
    req.harness = form.harness;
    req.cmd_argv = [];
  } else {
    req.cmd_argv = (form.command || 'sh').trim().split(/\s+/);
  }
  return req;
}

// recentCwds is the picker's list: the directories panes are running in
// today, then the ones this phone used before.
export function recentCwds(panes, stored) {
  const out = [];
  for (const cwd of [...(panes || []).map((p) => p.cwd), ...(stored || [])]) {
    if (cwd && !out.includes(cwd)) out.push(cwd);
  }
  return out.slice(0, MAX_RECENT);
}

function readStored() {
  try { return JSON.parse(localStorage.getItem('coppice.cwds') || '[]'); } catch { return []; }
}

export function mountNew() {
  const kind = document.getElementById('new-kind');
  const command = document.getElementById('new-command');
  const harness = document.getElementById('new-harness');
  const sync = () => {
    command.hidden = kind.value === 'headless';
    harness.hidden = kind.value !== 'headless';
  };
  kind.addEventListener('change', sync);
  sync();

  window.addEventListener('coppice:route', () => {
    if (window.coppice.route(location.hash).screen !== 'new') return;
    const list = document.getElementById('recent-cwds');
    list.replaceChildren(...recentCwds(window.coppice.state.panes, readStored()).map((cwd) => {
      const o = document.createElement('option');
      o.value = cwd;
      return o;
    }));
  });

  document.getElementById('create').addEventListener('click', async () => {
    let req;
    try {
      req = createCommand({
        cwd: document.getElementById('new-cwd').value,
        label: document.getElementById('new-label').value,
        kind: kind.value,
        command: command.value,
        harness: harness.value,
      });
    } catch (e) {
      window.coppice.status(e.message);
      return;
    }
    const { cmd, ...fields } = req;
    try {
      const result = await window.coppice.rpc(cmd, fields);
      const stored = recentCwds([{ cwd: req.cwd }], readStored());
      localStorage.setItem('coppice.cwds', JSON.stringify(stored));
      location.hash = '#/pane/' + encodeURIComponent(result.pane);
    } catch (e) {
      window.coppice.status(e.message);
    }
  });
}
```

- [ ] **Step 4: Write `settings.js`**

```js
// harness/coppice/internal/web/static/settings.js

// Server, token, and one button that proves push works. Nothing here talks
// to anything but this box.

export function wsUrl(origin) {
  const u = new URL(origin);
  u.protocol = u.protocol === 'https:' ? 'wss:' : 'ws:';
  u.pathname = '/ws';
  u.search = '';
  u.hash = '';
  return u.toString();
}

// tokenFromHash reads the token a scanned QR puts in the fragment. A token
// that is not exactly 43 characters of unpadded base64url is junk, and
// storing junk would leave the app unable to connect with nothing on screen
// to explain why.
export function tokenFromHash(hash) {
  const m = /^#t=([A-Za-z0-9_-]{43})$/.exec(hash || '');
  return m ? m[1] : null;
}

export function mountSettings() {
  const token = document.getElementById('set-token');

  window.addEventListener('coppice:route', () => {
    if (window.coppice.route(location.hash).screen !== 'settings') return;
    // The app always talks to the origin it was served from, so this is a
    // fact, not a field. An editable server box would need CORS on the
    // server to mean anything, and a control that does nothing is what
    // ruling 10 refuses two tasks earlier.
    document.getElementById('set-origin').textContent = 'Server ' + location.origin;
    token.value = localStorage.getItem('coppice.token') || '';
  });

  document.getElementById('save-settings').addEventListener('click', () => {
    const tok = token.value.trim();
    if (tok && !/^[A-Za-z0-9_-]{43}$/.test(tok)) {
      window.coppice.status('That token is not the right shape. Run coppice web token.');
      return;
    }
    localStorage.setItem('coppice.token', tok);
    window.coppice.status('Saved. Reload to reconnect.');
  });

  document.getElementById('forget-token').addEventListener('click', () => {
    localStorage.removeItem('coppice.token');
    token.value = '';
    window.coppice.status('Token forgotten. Scan the QR again.');
  });

  document.getElementById('test-push').addEventListener('click', async () => {
    try {
      await window.coppice.api('/api/push/test', { method: 'POST' });
      window.coppice.status('Sent. Watch the ntfy app.');
    } catch (e) {
      window.coppice.status(e.message);
    }
  });
}
```

- [ ] **Step 5: Mount both in `app.js` and use `tokenFromHash`**

Add the imports:

```js
import { mountNew } from './newpane.js';
import { mountSettings, tokenFromHash, wsUrl } from './settings.js';
```

Make three edits in `app.js`.

First, replace the four lines in `boot()` that begin `const fromHash = /^#t=` with the tested function:

```js
  const scanned = tokenFromHash(location.hash);
  if (scanned) {
    localStorage.setItem('coppice.token', scanned);
    history.replaceState(null, '', '#/roster');
  }
```

Second, mount the two new screens on the line after `mountPane();`:

```js
  mountNew();
  mountSettings();
```

Third, use the tested URL builder in `connect()`. Replace its three `const url = new URL(...)` lines with one call, so the function the tests cover is the function that runs:

```js
  const ws = new WebSocket(wsUrl(location.origin), bearerProtocols(state.token));
```

- [ ] **Step 6: Run the tests and watch them pass**

```bash
cd /mnt/tera/working/programming/openDaisugi/harness/coppice/internal/web && node --test static/_tests/
cd /mnt/tera/working/programming/openDaisugi/harness/coppice && go test ./... && go vet ./...
```

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
cd /mnt/tera/working/programming/openDaisugi
git add harness/coppice/internal/web/static/
git commit -m "coppice/web: start a pane from the kitchen, and keep the token honest

The directory field has no default on purpose: a pane started in the wrong
repository does real work in the wrong place, so a blank one is refused
rather than guessed. The token from a scanned QR is checked against its own
shape before it is stored, because junk in localStorage would leave the app
unable to connect with nothing on screen to explain why."
```

---

### Task 12: The Playwright pass and the checked-in screenshots

**Files:**
- Create: `scripts/phone_smoke.py`
- Create: `scripts/phone_smoke.mjs`
- Create: `harness/coppice/testdata/phone/` (the PNGs, committed)
- Create: `harness/coppice/internal/web/smoke_test.go`
- Modify: `.gitignore` (ignore `harness/coppice/.smoke/`)

**Interfaces:**
- Consumes: the `coppice` binary, `coppice web cert init`, `coppice web token`, `coppice web serve`, `coppice server start`, `coppice pane create`.
- Produces: `harness/coppice/testdata/phone/roster.png`, `pane.png`, `new.png`, `settings.png`, `https-roster.png`.

- [ ] **Step 1: Set up Playwright through the repo's visual-loop skill**

```bash
bash /mnt/tera/working/programming/iss-skills/skills/visual-loop/scripts/web-setup.sh
```

That installs Playwright and Chromium into `~/.cache/oh-visual-loop`. If it fails, record the reason: task 12's tests skip and say so, and the rest of the plan still stands.

- [ ] **Step 2: Write the Playwright driver**

```js
// scripts/phone_smoke.mjs
//
// Drives the coppice PWA on a phone-sized viewport and shoots one PNG per
// screen. Copied to ~/.cache/oh-visual-loop by phone_smoke.py, because ES
// module resolution ignores NODE_PATH and playwright lives there.
//
// usage: node phone_smoke.mjs <baseUrl> <token> <outDir> <paneLabel> [--insecure]
import { chromium } from 'playwright';
import { mkdirSync } from 'node:fs';

const [baseUrl, token, outDir, paneLabel] = process.argv.slice(2);
const insecure = process.argv.includes('--insecure');
if (!baseUrl || !token || !outDir) {
  console.error('usage: node phone_smoke.mjs <baseUrl> <token> <outDir> <paneLabel> [--insecure]');
  process.exit(2);
}
mkdirSync(outDir, { recursive: true });

const prefix = insecure ? 'https-' : '';
const browser = await chromium.launch();
const context = await browser.newContext({
  viewport: { width: 360, height: 780 },
  deviceScaleFactor: 2,
  ignoreHTTPSErrors: insecure,
});
const page = await context.newPage();
page.on('console', (m) => console.log('[page]', m.type(), m.text()));

const fail = async (msg) => {
  await page.screenshot({ path: `${outDir}/${prefix}failure.png` });
  console.error('FAIL:', msg);
  await browser.close();
  process.exit(1);
};

// Sign in the way a scanned QR does.
await page.goto(`${baseUrl}/#t=${token}`, { waitUntil: 'load', timeout: 20000 });
await page.waitForTimeout(1500);

if (!insecure) {
  // ready resolves only for an ACTIVE worker. getRegistration would resolve
  // for one that is still installing, or one whose install rejected because
  // cache.addAll failed, so it would pass on exactly the failure this pass
  // exists to catch.
  const active = await page.evaluate(() => Promise.race([
    navigator.serviceWorker.ready.then(() => true),
    new Promise((r) => setTimeout(() => r(false), 10000)),
  ]));
  if (!active) await fail('the service worker never became active over http://127.0.0.1');
}

await page.waitForSelector(`#roster .card:has-text("${paneLabel}")`, { timeout: 20000 })
  .catch(() => fail(`the roster never showed a pane labelled ${paneLabel}`));
await page.screenshot({ path: `${outDir}/${prefix}roster.png` });

if (insecure) {
  await browser.close();
  console.log('https pass ok');
  process.exit(0);
}

await page.click(`#roster .card:has-text("${paneLabel}")`);
await page.waitForSelector('#screen-pane:not([hidden])', { timeout: 10000 });
await page.waitForTimeout(1500);

await page.fill('#text', 'echo hello-from-phone');
await page.click('#prompt');

const seen = await page.waitForFunction(
  () => document.getElementById('grid-text').textContent.includes('hello-from-phone'),
  null, { timeout: 20000 },
).then(() => true).catch(() => false);
if (!seen) await fail('the echo never appeared in the pane grid');
await page.screenshot({ path: `${outDir}/pane.png` });

await page.goto(`${baseUrl}/#/new`);
await page.waitForSelector('#screen-new:not([hidden])');
await page.fill('#new-cwd', '/tmp');
await page.screenshot({ path: `${outDir}/new.png` });

await page.goto(`${baseUrl}/#/settings`);
await page.waitForSelector('#screen-settings:not([hidden])');
await page.waitForTimeout(300);
await page.screenshot({ path: `${outDir}/settings.png` });

await browser.close();
console.log('smoke ok');
```

- [ ] **Step 3: Write the orchestrator**

```python
# scripts/phone_smoke.py
"""Drive the coppice phone client end to end and shoot one PNG per screen.

Two passes. The first runs over http://127.0.0.1, which browsers treat as a
secure context, so the service worker really registers and the whole flow is
exercised. The second runs over the local CA to prove the certificate path
loads at all; Chromium is told to accept it rather than to trust it, because
trusting a root in the browser's own store needs certutil and the honest
proof of the chain is the Go handshake test in internal/web.

Usage: uv run --no-sync python scripts/phone_smoke.py [--keep]
Exit codes: 0 ok, 1 failed, 3 a prerequisite is missing.
"""

from __future__ import annotations

import argparse
import json
import os
import shutil
import signal
import socket
import subprocess
import sys
import time
from pathlib import Path

REPO = Path(__file__).resolve().parents[1]
COPPICE = REPO / "harness" / "coppice"
WORK = COPPICE / ".smoke"  # real disk, never /tmp, which is RAM here
SHOTS = COPPICE / "testdata" / "phone"
HARNESS_HOME = Path.home() / ".cache" / "oh-visual-loop"
HTTP_PORT = 18450
HTTPS_PORT = 18451
CA_PORT = 18452
LABEL = "smoke shell"


def need(cond: object, msg: str) -> None:
    if not cond:
        print(msg, file=sys.stderr)
        sys.exit(3)


def wait_port(port: int, seconds: float = 15.0) -> None:
    end = time.time() + seconds
    while time.time() < end:
        with socket.socket() as s:
            s.settimeout(0.3)
            if s.connect_ex(("127.0.0.1", port)) == 0:
                return
        time.sleep(0.1)
    print(f"nothing listening on 127.0.0.1:{port}", file=sys.stderr)
    sys.exit(1)


def run(argv: list[str], env: dict[str, str], check: bool = True) -> str:
    proc = subprocess.run(argv, env=env, capture_output=True, text=True)
    if check and proc.returncode != 0:
        print(" ".join(argv), file=sys.stderr)
        print(proc.stdout + proc.stderr, file=sys.stderr)
        sys.exit(1)
    return proc.stdout + proc.stderr


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--keep", action="store_true", help="leave the work directory in place")
    args = ap.parse_args()

    need(shutil.which("go"), "go is not on PATH. Install Go 1.25.")
    need(shutil.which("node"), "node is not on PATH. Install Node 22.")
    need(
        (HARNESS_HOME / "node_modules" / "playwright").exists(),
        "playwright is not installed. Run bash iss-skills/skills/visual-loop/scripts/web-setup.sh",
    )

    WORK.mkdir(parents=True, exist_ok=True)
    SHOTS.mkdir(parents=True, exist_ok=True)
    home = WORK / "home"
    home.mkdir(exist_ok=True)
    binary = WORK / "coppice"
    run(
        ["go", "build", "-o", str(binary), "./cmd/coppice"], env={**os.environ, "PWD": str(COPPICE)}
    )

    env = {**os.environ, "HOME": str(home), "XDG_RUNTIME_DIR": str(WORK / "run")}
    (WORK / "run").mkdir(exist_ok=True)

    procs: list[subprocess.Popen] = []
    try:
        run([str(binary), "server", "start"], env)
        run(
            [
                str(binary),
                "pane",
                "create",
                "--cwd",
                str(REPO),
                "--label",
                LABEL,
                "--kind",
                "pty",
                "--",
                "sh",
            ],
            env,
        )
        run(
            [
                str(binary),
                "web",
                "cert",
                "init",
                "--name",
                "localhost",
                "--ip",
                "127.0.0.1",
                "--ca-listen",
                f":{CA_PORT}",
            ],
            env,
        )
        token_out = run(
            [str(binary), "web", "token", "--qr=false", "--url", f"http://127.0.0.1:{HTTP_PORT}"],
            env,
        )
        token = next(
            line.split()[1] for line in token_out.splitlines() if line.startswith("token ")
        )

        shutil.copy(REPO / "scripts" / "phone_smoke.mjs", HARNESS_HOME / "phone_smoke.mjs")
        driver = str(HARNESS_HOME / "phone_smoke.mjs")

        # Pass one: plain HTTP on loopback, a real secure context.
        procs.append(
            subprocess.Popen(
                [
                    str(binary),
                    "web",
                    "serve",
                    "--tls",
                    "off",
                    "--listen",
                    f"127.0.0.1:{HTTP_PORT}",
                    "--qr=false",
                ],
                env=env,
            )
        )
        wait_port(HTTP_PORT)
        first = subprocess.run(
            ["node", driver, f"http://127.0.0.1:{HTTP_PORT}", token, str(SHOTS), LABEL],
            capture_output=True,
            text=True,
        )
        print(first.stdout + first.stderr)
        if first.returncode != 0:
            return 1

        # Pass two: the local CA, to prove the certificate path loads.
        procs.append(
            subprocess.Popen(
                [
                    str(binary),
                    "web",
                    "serve",
                    "--tls",
                    "localca",
                    "--listen",
                    f"127.0.0.1:{HTTPS_PORT}",
                    "--ca-listen",
                    f":{CA_PORT}",
                    "--qr=false",
                ],
                env=env,
            )
        )
        wait_port(HTTPS_PORT)
        second = subprocess.run(
            [
                "node",
                driver,
                f"https://127.0.0.1:{HTTPS_PORT}",
                token,
                str(SHOTS),
                LABEL,
                "--insecure",
            ],
            capture_output=True,
            text=True,
        )
        print(second.stdout + second.stderr)
        if second.returncode != 0:
            return 1

        sheet = Path(
            "/mnt/tera/working/programming/iss-skills/skills/visual-loop/scripts/contact-sheet.sh"
        )
        if sheet.exists() and shutil.which("magick"):
            subprocess.run(
                [
                    "bash",
                    str(sheet),
                    str(SHOTS / "montage.png"),
                    *[str(p) for p in sorted(SHOTS.glob("*.png")) if p.name != "montage.png"],
                ],
                check=False,
            )
            print(f"read {SHOTS / 'montage.png'}")
        else:
            print(f"read the PNGs in {SHOTS}")
        return 0
    finally:
        for p in procs:
            p.send_signal(signal.SIGTERM)
            try:
                p.wait(timeout=5)
            except subprocess.TimeoutExpired:
                p.kill()
        subprocess.run([str(binary), "server", "stop"], env=env, capture_output=True)
        if not args.keep:
            shutil.rmtree(WORK, ignore_errors=True)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
```

- [ ] **Step 4: Write the Go gate that skips with a reason**

```go
// harness/coppice/internal/web/smoke_test.go
package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The Playwright pass drives a real browser against a real server. It costs
// seconds and needs Node and Chromium, so it runs on request and skips with
// its reason otherwise, which is the rule for every live test in this work.
func TestPhoneSmoke(t *testing.T) {
	if os.Getenv("COPPICE_SMOKE") != "1" {
		t.Skip("set COPPICE_SMOKE=1 to run the Playwright pass")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not on PATH, so the Playwright pass did not run")
	}
	home, _ := os.UserHomeDir()
	if _, err := os.Stat(filepath.Join(home, ".cache", "oh-visual-loop", "node_modules", "playwright")); err != nil {
		t.Skip("playwright is not installed. Run iss-skills/skills/visual-loop/scripts/web-setup.sh")
	}
	cmd := exec.Command("uv", "run", "--no-sync", "python", "scripts/phone_smoke.py")
	cmd.Dir = "../../../.."
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("phone_smoke.py failed:\n%s", out)
	}
}

// The screenshots are the eyeballing record. If they are gone, nobody has
// looked at this client in a while.
func TestTheCheckedInScreenshotsExist(t *testing.T) {
	for _, name := range []string{"roster.png", "pane.png", "new.png", "settings.png", "https-roster.png"} {
		if _, err := os.Stat(filepath.Join("..", "..", "testdata", "phone", name)); err != nil {
			t.Errorf("%s is missing. Run uv run --no-sync python scripts/phone_smoke.py", name)
		}
	}
}
```

- [ ] **Step 5: Run the smoke pass and look at the pictures**

```bash
cd /mnt/tera/working/programming/openDaisugi
echo "harness/coppice/.smoke/" >> .gitignore
uv run --no-sync python scripts/phone_smoke.py
```

Now open `harness/coppice/testdata/phone/montage.png` (or the four PNGs) and read them. Judge, at 360x780:

- nothing clipped at the right edge, no horizontal scrollbar
- every button at least 44 pixels tall and reachable with a thumb
- the state chip readable as a word, not only a colour
- the blocked card visibly first in the roster
- the canvas not spilling past the viewport before a pinch

Fix what you see, re-run, and look again. Do not move on with "the screenshots were generated"; move on when you have read them.

- [ ] **Step 6: Commit**

```bash
cd /mnt/tera/working/programming/openDaisugi
git add .gitignore scripts/phone_smoke.py scripts/phone_smoke.mjs harness/coppice/testdata/phone/ harness/coppice/internal/web/smoke_test.go
git commit -m "coppice/web: a browser drives the phone client, and the pictures are kept

The first pass runs over http://127.0.0.1 because browsers treat loopback as
a secure context and HTTPS with an untrusted certificate does not, so this is
the only way to see the service worker actually register. The second pass
runs over the local CA to prove that path loads; it tells Chromium to accept
the certificate rather than to trust it, and the honest proof of the chain
stays the Go handshake test. The four screenshots are committed so the next
person can see what the client looked like without running anything."
```

---

### Task 13: `docs/how-to/phone.md`, and saying honestly what is built

**Files:**
- Create: `docs/how-to/phone.md`
- Modify: `docs/README.md` (add the how-to to the guide list, beside `how-to/gate.md`)
- Modify: `docs/feature-status.md` (one row)
- Modify: `CHANGELOG.md` (one entry under `## Unreleased`)

**Interfaces:**
- Consumes: every command from tasks 6 and 8.
- Produces: the operator-facing guide.

- [ ] **Step 1: Write the how-to**

Write `docs/how-to/phone.md` with these sections, in this order. Copy is STE100: short sentences, active voice, no em-dashes, no parenthetical asides. Every command below must be one you have actually run in an earlier task.

````markdown
# Use the phone

See every pane, open one, prompt it, and answer the gate, from a phone in the
kitchen. No app store, no relay, and nothing of Google's in the default path.

## Before you start

- coppice-server runs on the box. Check with `coppice server status`.
- The phone reaches the box. A tailnet is the usual way. A LAN works too.
- You have picked a certificate path. Read the next section before you choose.

## Which certificate path

A phone will not install a web app, and will not run a service worker, over
plain HTTP. So the box needs a certificate the phone trusts. There are two
honest ways to get one.

| | `tailscale` | `localca` |
|---|---|---|
| Who issues it | Let's Encrypt, through Tailscale | your own box |
| What leaves the box | your machine name, onto a public certificate ledger | nothing |
| Phone setup | none | install one certificate, once |
| Renewal | yours, every 90 days | ten years |
| Works off the tailnet | no | yes, on any LAN |

Pick `tailscale` if publishing `<machine>.<tailnet>.ts.net` on a public ledger
is fine with you. Pick `localca` if it is not.

## Path A: tailscale

1. Open the Tailscale admin console. Go to the DNS page.
2. Turn on MagicDNS.
3. Under HTTPS Certificates, choose Enable HTTPS.
4. Acknowledge the notice. Your machine names go into a public ledger. Do not
   turn this on if a machine name is sensitive.
5. On the box, ask for the certificate:

```bash
coppice web cert tailscale box.tail1234.ts.net
```

That runs `tailscale cert` with explicit `--cert-file` and `--key-file` paths
and writes the pair under `~/.opendaisugi/coppice/tls/`.

6. Start the server:

```bash
coppice web serve --tls tailscale \
  --cert ~/.opendaisugi/coppice/tls/box.tail1234.ts.net.crt \
  --key  ~/.opendaisugi/coppice/tls/box.tail1234.ts.net.key \
  --external-url https://box.tail1234.ts.net:8443
```

Let's Encrypt certificates last 90 days. Renewal is yours. The server warns
in its log for the last 14 days.

## Path B: the local CA

1. Make the CA and the server certificate. Name every address the phone will
   use:

```bash
coppice web cert init --name box.local --ip 192.168.1.20
```

2. Scan the QR it prints. The phone opens a download of `ca.crt` from the
   box on port 8080.
3. On the phone, open Settings, then Security and privacy, then Encryption
   and credentials, then Install a certificate, then CA certificate. Choose
   Install anyway, then pick the file you downloaded.
4. Android shows a standing notice that the network may be monitored. That is
   what a user-installed certificate authority always does.
5. Start the server:

```bash
coppice web serve --tls localca --external-url https://192.168.1.20:8443
```

Re-run `coppice web cert init` whenever the box changes address. It keeps the
same CA and issues a new server certificate, so you never install a
certificate on the phone twice.

## Sign in

```bash
coppice web token
```

That prints the token, a sign-in URL, and a QR. Scan the QR on the phone. The
app stores the token and clears it from the address bar. To retire a token,
run `coppice web token --rotate` and scan the new QR.

Three bad tokens from one address earn that address a minute of silence.

**Treat the token like an SSH key.** The phone speaks the same protocol your
terminal speaks, with nothing filtered, so anyone holding the token can start
a pane running any command in any directory. That is what makes the phone
useful and it is also what it costs. Rotate the token if you lose the phone.

Install the app from the browser menu, with Add to home screen.

The app always talks to the address it was served from. Settings shows that
address; there is no box to change it. To point a phone at a different box,
open that box's own URL.

## Push

Push goes through your own ntfy. Nothing else.

1. Run ntfy on the box or on the tailnet. One binary, Apache-2.0.
2. Install the ntfy Android app. Subscribe it to your topic.
3. Put the ntfy token in an environment variable and name it:

```bash
export COPPICE_NTFY_TOKEN=tk_yourtoken
coppice web serve --tls localca \
  --ntfy https://ntfy.box.local --ntfy-topic coppice \
  --ntfy-token-env COPPICE_NTFY_TOKEN \
  --external-url https://192.168.1.20:8443
```

The server publishes when a pane becomes blocked, with the pane label in the
title, the ask in the body, and a link to that pane. A pane stays quiet for
five seconds after it speaks.

Test it from the phone. Open Settings, then Test push.

Web push is not built. It would route your notifications through your browser
vendor's push service, which is what ntfy exists to avoid here. See
`docs/feature-status.md`.

## Keep it running

```bash
coppice web serve --tls localca --persist \
  --external-url https://192.168.1.20:8443 \
  --ntfy https://ntfy.box.local --ntfy-topic coppice \
  --ntfy-token-env COPPICE_NTFY_TOKEN
```

`--persist` writes `~/.opendaisugi/coppice/web.json`. The coppice server then
starts the phone server with itself, so there is one process to keep alive.
`--forget` undoes it.

## What leaves the box

- With `tailscale`: your machine name, on a public certificate ledger.
- With `localca`: nothing.
- With push: the pane label goes to the ntfy server you named, plus the ask
  summary, or the gate's `detail` line when there is no ask. That detail can
  name a tool and a clause, for example `verdict=deny clause=shell.deny[2]`.
  Run your own ntfy and that is your box too.
- Nothing else. No telemetry, no relay, no accounts. No grid and no
  transcript ever leaves through push.

## When it does not work

| What you see | What to do |
|---|---|
| The browser says the certificate is not trusted | Install the CA. Run `coppice web cert show` to see what the certificate covers, then re-run `cert init` with the address you are actually using. |
| That token is not accepted. Run coppice web token and scan the QR again. | The token on the phone is stale. Do exactly that. The app stops retrying on purpose, so it does not get your own address banned. |
| No token. Open Settings and paste one. | Nothing is stored yet. Scan the QR from `coppice web token`. |
| Too many bad tokens. Waiting one minute. | Three wrong tokens came from your address. Wait, then scan the QR. |
| Reconnecting. | The box is unreachable. Check the tailnet or the LAN. The app backs off to thirty seconds and keeps trying. |
| Add to home screen is missing | The page is not a secure context. Fix the certificate first. |
| server refused: … | coppice-server answered and said no. The message is its own. Run `coppice server status` and `coppice pane list`. |
| The roster is empty | There really are no panes. A refusal is never shown this way; it says `server refused`. |
| Allow does nothing, and you see That ask is gone | The gate timed out, or the answer is landing in the wrong directory. Check the `gate_root` in the server's start-up log against your data directory, and set `--gate-root` if they differ. |
| Push never arrives | Check the ntfy app is subscribed to the topic. Then press Test push in Settings and read the message it gives you. |
````

- [ ] **Step 2: Link it from the docs hub**

In `docs/README.md`, in the How-to guides list, add the line after the `how-to/gate.md` entry:

```markdown
- **[Use the phone](how-to/phone.md)** — the PWA coppice serves: both certificate paths, the token QR, and ntfy push.
```

- [ ] **Step 3: Say what is built, and what is not**

Append one row to the table in `docs/feature-status.md`:

```markdown
| Phone client (`coppice web serve`) | v0.45.0 | Experimental | The PWA coppice-server embeds: roster, pane grid, prompt, steer, allow, deny, and ntfy push on the transition to blocked. TLS from `tailscale cert`, a local CA, or files. Web Push is planned, not built: `--web-push` exits 1 and names ntfy. Driven by a Playwright pass, not yet used on a phone for a week. |
```

Add one entry under `## Unreleased` in `CHANGELOG.md`:

```markdown
- **The phone: a PWA coppice-server serves, and ntfy push.** `coppice web serve`
  puts the floor on a phone over a tailnet or a LAN. One websocket per browser
  is one unix-socket connection to coppice-server, so the phone speaks exactly
  the protocol the terminal speaks. A bearer token guards `/ws` and `/api`, and
  three bad tokens from one address buy that address a minute of silence. TLS
  comes from `tailscale cert`, from a local CA the box generates and a QR
  installs on the phone, or from explicit files. Push goes through a
  self-hosted ntfy on every merged transition to `blocked`. Web Push is not
  built; `--web-push` says so and names ntfy. See `docs/how-to/phone.md`.
```

- [ ] **Step 4: Check the whole plan is green**

```bash
cd /mnt/tera/working/programming/openDaisugi
uv run --no-sync pytest -q
uv run --no-sync ruff check .
cd harness/coppice && go test ./... && go vet ./...
cd internal/web && node --test static/_tests/
```

Expected: all green. Every claim in `docs/how-to/phone.md` must match a command that exists; run each one once against a scratch HOME if you are not sure.

- [ ] **Step 5: Commit**

```bash
cd /mnt/tera/working/programming/openDaisugi
git add docs/how-to/phone.md docs/README.md docs/feature-status.md CHANGELOG.md
git commit -m "docs: how to put the floor on a phone, and what that costs

The certificate choice is the whole decision, so it is a table before it is
a procedure: tailscale is easy and puts your machine name on a public ledger,
the local CA costs one install on the phone and publishes nothing. The what
leaves the box section says both, plainly, and the feature-status row says
Web Push is planned rather than shipped, because a control that does nothing
is a lie and so is a row that implies one."
```

---

## Notes for the reviewer

- **Task 0 makes exactly one edit outside this plan's own package:** `"ts": ev.TS` on plan 02's `pane.list` row, so a roster card can show a pane's age on first load. `pane.attach` needs no edit; plan 02's `handleAttach` already treats a missing size as "do not resize". If plan 02 has not run yet, task 0 stops with a teaching message instead.
- **`internal/web` imports nothing from plan 02.** Every upstream call goes through `Dialer`, so this plan is buildable and testable on its own and the parity claim is structural. The price is that plan 02's wire shapes are facts this plan has to write down rather than types it can import, which is what the `pane.list`, `state` event and refusal rows in `PINS.md` are for. Task 10's fixture is the same discipline in JavaScript.
- **The token is a shell on the box.** Ruling 1's unrewritten pipe is deliberate, and it means a token holder can `pane.create` any command anywhere. Ruling 1 and the how-to both say so.
- **Web Push is a refusal, not a stub.** Task 8 ships `--web-push` as an exit-1 with a message. Implementing RFC 8291 payload encryption would be its own plan; a VAPID-only wake-and-fetch design is the cheaper follow-on if the operator ever wants it.
- **The `localca` Playwright pass proves the page loads, not that the chain is trusted.** Chromium is told to accept the certificate. The trust proof is `TestARealTLSHandshakeSucceedsWithTheCAInTheRootPool` in task 5.
- **`--tls off` is new.** The spec does not name it. It exists because a service worker will not register on HTTPS with an untrusted certificate, and it refuses any address that is not loopback.

