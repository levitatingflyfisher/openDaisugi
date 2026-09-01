# The workshop: layer, floor, loop — master spec

**Date:** 2026-09-08 · **Status:** approved design, plans in this directory
**Supersedes in part:** ADR-0004 (layer, not harness) — by ADR-0020, written under plan 00
**Reads first:** `VISION.md`, `docs/adr/0004-layer-not-harness.md`, the 2026-09-05 dev-infra
audit (`research-notes/2026-09-05-dev-infra-audit/REPORT.md`, outside this repo), and
`docs/plans/2026-08-27-cockpit-spec.md` (the session tree, the ask protocol, the resident gate).

This document is the one place the *shared* decisions live: the three-part shape, the contracts
every sub-project builds on, the global constraints, and the reasoning behind the choices that
were actually contested. Each sub-project has its own spec (`spec-NN-*.md`) and plan
(`plan-NN-*.md`). A sub-spec never restates a contract from here; it cites the section.

Everything here was written by an AI assistant with the operator in the loop. Verify before you
rely.

---

## 1. The one idea, extended

openDaisugi's thesis is unchanged: **separate what is allowed from what is decided.** A black box
proposes; a checkable envelope disposes. That separation is the *layer*.

What changed on 2026-09-08 is the ambition around it. The operator wants the whole workshop —
the desk, the phone, the kitchen, one GPU box somewhere, many agents at once — and wants it to be
*ours*: swappable at every layer, honest about what is live, and never dependent on a vendor for
the part that matters. The 2026-09-05 audit found that every published agentic setup converges on
one shape (one always-on Linux box; every device a window into it; every agent a window inside
it) and that the only piece nobody ships is the gate. The rest is assembled from Tailscale, a
multiplexer, and a phone SSH client.

So the workshop is three parts:

| Part | What it is | Who owns the code | What must stay true |
|---|---|---|---|
| **The layer** | gate + verifier + journal + garden. `verify(plan ⊆ envelope)`, fail-closed. | ours, forever | importable alone; imports nothing above it |
| **The floor** | *coppice*: panes, state, prompt, steer, phone, voice. The shop floor. | ours, built to compete with Herdr and to *drive* Herdr | every state it shows is sourced, never guessed when a source exists |
| **The loop** | the harness in a pane: sprig, Claude Code, Codex, pi, OpenCode. | rented, except sprig | every tool call passes the gate; an unreachable gate blocks |

**Why this is not a betrayal of ADR-0004.** That ADR said: "If a genuine need for a standalone
*driving* agent ever appears, it must be a separate product built *on* this layer — not folded
into it." Sprig (2026-08) is that product. The cockpit with allow/deny/steer is already a floor.
The invariant that actually protected the thesis was never "no loop"; it was **"the gate stays a
portable library."** ADR-0020 states that invariant precisely, and plan 00 enforces it with an
import-boundary test. Everything above the layer may grow; nothing in the layer may reach up.

**The name.** A coppice is a stand of trees cut back so each stump sends up straight shoots.
Daisugi is a coppicing technique. The multiplexer of sprigs is a coppice. (`garden` was taken by
the pathway store; `grove` by the sprig line.)

---

## 2. The shape

```
  phone PWA ───┐                             ┌─ pi --mode rpc            (headless)
  laptop TUI ──┼── JSONL/unix socket ──▶ coppice-server (Go) ─┼─ claude -p stream-json   (headless)
  scripts/agents┘        ▲                  PTY + ghostty-vt   ├─ codex exec --json       (headless)
                         │                  state authority    ├─ opencode serve          (headless)
                         │                        ▲            └─ any command in a PTY    (interactive)
                    Herdr / tmux                  │ report-state (push)
                    (alternate pane backends)     │
                                       daisugi hook ─── gate.sock ─── resident gate
                                              ▲
                                              └── every tool call, every harness, fail-closed
```

Data flow, in one paragraph. A harness runs in a pane. Each tool call it proposes hits the gate
(hook, MCP, pi extension, OpenCode plugin, or sprig's in-process `Gate`). The gate decides, writes
the session tree, and — new — **reports state** (`working`, `blocked` with the pending ask, or
`idle`) to whatever floor is listening: coppice-server over its socket, Herdr via
`herdr pane report-agent`, or nobody. coppice-server owns the process and the pane grid, merges
the reported state with its own process observations (exit ⇒ `done`), falls back to screen
manifests only for agents that have no hook, and streams frames and state to every attached
client. Clients render, prompt, steer, allow, deny. The phone is a client. Voice is a client that
types.

---

## 3. Contracts (normative)

Sub-specs cite these by number. A plan that changes a contract must change this file in the same
commit.

### 3.1 PaneStateEvent

One JSON object per line. Emitted by the gate hook (Python), by coppice-server's process watcher
(Go), by headless adapters, and by the manifest fallback. Consumed by coppice-server, the cockpit,
and the Herdr bridge.

```json
{
  "v": 1,
  "ts": 1757300000.123,
  "session_id": "d41c…",            // daisugi session id (tree file stem)
  "harness_session_id": "…",        // the harness's own id when known, else null
  "harness": "claude-code",         // claude-code | codex | pi | opencode | sprig | shell | unknown
  "pane": "w1:p3",                  // coppice/Herdr/tmux pane id when known, else null
  "state": "blocked",               // idle | working | blocked | done | unknown
  "source": "gate",                 // gate | process | headless | manifest | operator
  "ask": {                          // present only when state == blocked and the gate holds an ask
    "id": "toolu_…", "tool": "Bash", "summary": "rm -rf build/", "deadline": 1757300090.0,
    "tier": "permanent"             // undoable | permanent; absent or unknown reads as permanent
  },
  "detail": "verdict=deny clause=shell.deny[2]"   // free text, ≤ 200 chars, for the peek pane
}
```

Rules. `source` precedence (`operator` > `gate` > `headless` > `process` > `manifest`) decides
only when the incoming event is from `manifest`: within 2 s, a manifest event may not override a
higher-precedence current event. Events from every other source (`operator`, `gate`, `headless`,
`process`) are facts and apply at once. A `gate` event with `state: blocked` is authoritative
until a `gate` event clears it or the ask's `deadline` passes (then `working`). Any `blocked`
whose ask deadline has passed reads as `working`. `done` comes only from `process` (exit) or
`headless` (end-of-session event); a manifest may never say `done`.
A `done` from `process` or `headless` is terminal: it wins at once, before the gate hold and
before the precedence window, because nothing re-sends it. A `gate` event with `state: blocked`
carries an ask; without one it is invalid.
The ask's `tier` says how careful an answer must be. The gate sets it from the call's effect
class. A denied call that reads, writes inside the workspace, does a network get, or runs tests is
`undoable`: one key allows it. Every other denied call is `permanent`: the operator types the pane
name to allow it. An allowed call is `silent` and never asks, so `silent` shows only in the shadow
log and the session tree. Every reader treats an ask with no tier, or a tier it does not know, as
`permanent`. A present tier that is not a string makes the event invalid.
`unknown` is what the floor shows when it has *no* source — it is never dressed up as `idle`.
A live pty pane always has a `process` source: `working` within five seconds of output, else
`idle`. `unknown` is reserved for a headless pane whose event stream failed to parse. Every
`pane.list` row for a pane that still has a screen carries `quiet_for`, seconds since the last
byte of output. A closed record has no screen and no `quiet_for`.

### 3.2 PaneBackend protocol (Python, `opendaisugi.floor.backend`)

```python
class PaneBackend(Protocol):
    name: str  # "coppice" | "herdr" | "tmux"

    def available(self) -> bool: ...  # binary/socket reachable; never raises
    def spawn(
        self,
        *,
        cwd: Path,
        cmd: list[str],
        env: dict[str, str],
        label: str,
        kind: Literal["pty", "headless"],
        harness: str | None = None,
        task: str | None = None,
    ) -> PaneRef:
        ...
        # harness is required when kind == "headless" (it names the adapter, §3.4); amended 2026-09-08
        # task is the id of the task the pane works for; coppice only, herdr and tmux raise
        # NotImplementedError; a coppice task with a worktree runs the pane there; amended 2026-09-21

    def list(self) -> list[PaneInfo]: ...  # id, label, cwd, cmd, state: PaneStateEvent|None
    def send_text(self, pane: PaneRef, text: str, *, enter: bool = True) -> None: ...
    def send_keys(
        self, pane: PaneRef, keys: list[str]
    ) -> None: ...  # "enter", "ctrl+c", "esc", "a"
    def read(
        self, pane: PaneRef, *, source: Literal["visible", "recent", "detection"] = "visible"
    ) -> str: ...
    def resize(self, pane: PaneRef, cols: int, rows: int) -> None: ...
    def close(self, pane: PaneRef) -> None: ...
    def subscribe(
        self,
    ) -> Iterator[PaneStateEvent | Frame]: ...  # blocking generator; Frame only from coppice
    def report_state(self, pane: PaneRef, ev: PaneStateEvent) -> None: ...  # push authority in

    # Tasks above panes; amended 2026-09-21. list_tasks never raises and is [] on herdr and tmux.
    # create_task and close_task raise NotImplementedError("<name> has no tasks. Use the coppice
    # backend.") on herdr and tmux.
    def list_tasks(self) -> list[TaskInfo]: ...
    def create_task(
        self,
        label: str,
        *,
        parent: str | None = None,
        cwd: str | None = None,
        worktree: bool = False,
        model: str | None = None,
    ) -> TaskInfo: ...
    def close_task(self, task: str, *, keep_worktree: bool = False) -> None: ...
```

`TaskInfo` is a frozen dataclass `(ref, label, parent, cwd, worktree, model, state, panes: tuple[str, ...])`;
amended 2026-09-21.

`PaneRef` is `(backend: str, id: str)`. `Frame` is `{pane, seq, cols, rows, cursor: (x, y),
rows_changed: {row_index: [cell...]}}` where a cell is `[text, fg, bg, attrs]`. Only the coppice
backend yields frames; Herdr and tmux yield text via `read()`.

The contract test suite (`tests/floor/test_backend_contract.py`) is parameterised over every
backend and **skips**, never fails, when a backend's `available()` is false. A backend that is
present and fails a contract test fails the suite.

### 3.3 coppice-server socket API (Go, `harness/coppice`)

Unix socket at `$XDG_RUNTIME_DIR/coppice/server.sock`, else `~/.opendaisugi/coppice/server.sock`,
mode 0600, directory 0700. JSONL both ways. Requests carry `id`; responses echo it. Events carry
no `id`.

```
→ {"id":"1","cmd":"pane.create","cwd":"/repo","cmd_argv":["claude"],"env":{},"label":"auth fix","kind":"pty"}
← {"id":"1","ok":true,"result":{"pane":"w1:p1"}}
→ {"id":"2","cmd":"pane.attach","pane":"w1:p1","cols":120,"rows":40}
← {"id":"2","ok":true}
← {"event":"frame","pane":"w1:p1","seq":1,"cols":120,"rows":40,"cursor":[0,0],"rows_changed":{...}}
← {"event":"state","pane":"w1:p1", ...PaneStateEvent... }
```

Commands (mirroring Herdr's verbs on purpose — see §5.5): `server.status`, `session.list`,
`session.stop`, `workspace.create|list`, `tab.create|list`, `pane.create|list|split|send_text|
send_keys|run|read|resize|close|wait_output|report_state|attach|detach`, `agent.list|get|prompt|
wait|read`, `events.subscribe`. Errors: `{"id":…,"ok":false,"error":{"code":"no_such_pane","message":"…"}}`.
Codes are a closed enum listed in spec-02.

### 3.4 Headless adapters

A headless pane wraps a harness process and normalises its event stream to (a) a text transcript
rendered into the pane grid and (b) PaneStateEvents with `source: headless`. Prompt delivery is
per harness:

| harness | start | prompt in | events out | blocked when |
|---|---|---|---|---|
| claude-code | `claude -p --output-format stream-json --input-format stream-json --verbose` | JSON user message on stdin | stream-json lines | our hook says so (`source: gate` wins) |
| codex | `codex exec --json` | argv / stdin per turn | JSONL | hook (Codex hooks are fail-open class — spec-01 keeps the honesty tag) |
| pi | `pi --mode rpc` | `{"type":"prompt","message":…}` | JSONL events; `extension_ui_request` ⇒ `blocked` with ask | `extension_ui_request` or our extension's block |
| opencode | `opencode serve` + `opencode run --attach` | HTTP | SSE | plugin `permission.ask` |
| sprig | `sprig --session-dir …` | stdin | session tree (path E) | `sprig.Gate` deny → ask |

### 3.5 Honesty tags

Every module in `daisugi modules` carries `ACTIVE | AVAILABLE | POSSIBLE` and every stage carries
`live | cfg | planned` (`swap.py`). New modules from this work must land with the honest tag on
the day they land, not after. A control that does nothing is a lie; mark it `planned`.

### 3.6 Fail-closed, restated for the new joins

- pi extension: any thrown error, timeout, or unreachable gate ⇒ `{block: true, reason}`.
- OpenCode plugin: the before-execute hook is deny-only by design; that is a feature, not a limit.
- Headless adapters: a parse error in the event stream marks the pane `unknown`, never `idle`.
- coppice-server: a client it cannot authenticate (socket peer uid ≠ server uid) is dropped.
- Voice bridge: text is delivered as a *prompt* the operator sees before the loop sees it, unless
  the pane was explicitly armed for direct delivery.

---

## 4. Global constraints (copied into every plan header)

- **Layer purity.** No module under `src/opendaisugi/` that is part of the layer (list in
  `tests/test_layer_boundary.py`, plan 00) may import from `opendaisugi.floor`, `opendaisugi.voice`,
  or `opendaisugi.coppice`. The test imports every layer module with those packages hidden.
- **Python 3.12, stdlib for the layer.** New hard deps in the layer: none. New extras allowed:
  `[floor]` (nothing yet — the client uses stdlib sockets), `[voice]`, `[int8]`, `[router]`.
- **Go 1.26** for `harness/coppice` (amended 2026-09-08: go-libghostty's own go.mod declares
  `go 1.26.0`; sprig stays on 1.25); module `github.com/opendaisugi/coppice`; `go vet` and
  `go test ./...` clean. The toolchain script sets `GOTOOLCHAIN=auto` so 1.26 downloads
  itself; the undo is recorded in `PINS.md`. Zig 0.16 and CMake are *build-time* requirements for go-libghostty; the
  plan installs both into `~/.local` without sudo (`uv tool install cmake`; Zig tarball).
- **Pins.** go-libghostty at the newest tag on the day plan 02 starts, recorded in `go.mod`
  and in `harness/coppice/PINS.md` together with the ghostty commit that binding builds.
  The Herdr commit coppice vendors from is recorded in
  harness/coppice/internal/detect/manifests/PROVENANCE.
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

---

## 5. The cruxes, with reasons

These are the choices that were argued. Each is stated as the decision, the alternative, why, and
what it costs if wrong.

### 5.1 The state authority is the gate, not the screen

**Decision.** `source: gate` outranks every inference. Manifests are a fallback for agents with no
hook.
**Alternative.** Herdr's approach: watch the bottom of the screen, match known prompts.
**Why.** Herdr *infers* "blocked" because it is outside the agent. We are the thing that blocks.
When the gate denies or holds an ask, "blocked" is not a guess, it is a fact we produced, with the
tool name and the clause attached. Screen heuristics break when a UI changes (Herdr's own docs:
"Detection is probabilistic"). A fact does not.
**Cost if wrong.** Agents without a hook (a bare shell, an unsupported harness) get the weaker
path. That is acceptable: the honest tag says `manifest`, and the floor shows `unknown` rather
than a confident `idle`.

### 5.2 Headless first, PTY too

**Decision.** Both pane kinds ship in the first coppice-server. Headless panes are the default for
spawn-from-the-floor; PTY panes are for attaching the harness's own UI.
**Alternative.** Headless only (no terminal emulation), or PTY only (Herdr's model).
**Why.** Headless is *structurally* better for supervision: events are typed, state is exact,
there is nothing to parse off a screen. But people live in the harness UIs — slash commands,
skills, the diff view. Parity with Herdr means the operator can keep every habit. And the grid is
also the substrate the manifest fallback needs. Ghostty made the PTY path cheap enough to include.
**Cost if wrong.** Two code paths for panes. Mitigated by one PaneStateEvent and one frame
format; the client cannot tell which kind it is looking at except by a badge.

### 5.3 Go server, Python client

**Decision.** coppice-server is Go, beside sprig. The cockpit, the CLI, the contracts, and every
policy decision are Python.
**Alternative.** Python daemon with cffi to libghostty-vt, or pyte.
**Why.** The server must outlive every client, hold PTYs, and push frames at terminal speed.
That is plumbing, and Go with one static binary is the right tool — it is how Herdr and tmux
are shaped. go-libghostty is Mitchell Hashimoto's own binding, static-linked, MIT. There is no
Python binding, and the C API is marked unstable, so writing one would be ours to chase. Policy
(what state means, what the gate said, what to show) stays in Python where the layer lives.
**Cost if wrong.** Two languages, one more build-time toolchain (Zig, CMake). Mitigated by the
sprig precedent: the Go line already exists, builds in CI, and has its own tests.

### 5.4 Steal the engine, not the architecture

**Decision.** Use libghostty-vt through go-libghostty for the grid. Pin the same ghostty commit
Herdr pins.
**Alternative.** pyte (pure Python), vt100/alacritty crates via FFI, or xterm.js in the browser.
**Why.** Ghostty is the most exercised VT implementation of its generation, and its author
maintains the Go binding. Herdr chose it and patched it; pinning the same commit makes their
patch notes our map. The instability of the C API is the binding author's problem before it is
ours; our pin plus golden-grid tests are the tripwire.
**Cost if wrong.** A bump becomes a task, not a routine. Acceptable.

### 5.5 Mirror Herdr's verbs, compete on state

**Decision.** coppice's CLI and socket commands use Herdr's nouns and verbs (`pane create`,
`agent prompt --wait`, `pane read --source detection`). Its manifests use Herdr's TOML format and
we vendor Herdr's manifest files (Apache-2.0) with attribution and a local override directory.
**Alternative.** Invent our own vocabulary.
**Why.** A Herdr user should be able to drive coppice from muscle memory, and a coppice user
should be able to fall back to Herdr as a backend without relearning. We compete on the one axis
where we are structurally better (§5.1), not on vocabulary. Reusing manifests means twenty-one
agents are detected on day one.
**Cost if wrong.** If Herdr renames things, we choose whether to follow. Fine.

### 5.6 The phone is a browser, push is ntfy

**Decision.** The phone client is a PWA served by coppice-server. Push notifications go through a
self-hosted ntfy reached by the ntfy Android app. Web Push is optional, off by default.
**Alternative.** A native app; Web Push by default.
**Why.** Web Push on Android transits Google's push service. A local-first workshop should not
put Google in the path of "your agent is blocked." ntfy is Apache-2.0, self-hosts in one binary,
and its app holds a websocket to *our* server. A PWA needs a secure context; `tailscale cert`
gives a Let's Encrypt certificate for the MagicDNS name, at the price of publishing the machine
name on a certificate-transparency ledger. If that crosses the operator's exposure line, the
fallback is the daemon-issued local CA with QR trust from the 2026-08 LAN-PWA verdict.
**Cost if wrong.** One more service on the VM. Push latency depends on the phone's battery policy
for the ntfy app.

### 5.7 Switchyard upstream, the gateway keeps its own router

**Decision.** Claude Code → daisugi gateway → NeMo Switchyard → providers. The gateway's rules
router stays the no-dependency default. Switchyard is `AVAILABLE` when installed, `ACTIVE` when
the gateway's upstream points at it.
**Alternative.** Let Switchyard replace the gateway router; or put the gateway behind it.
**Why.** The gateway is the *meter* and the *memory* (turn journal, answer reuse, cache-aware
prefix). Switchyard is a *chooser*. A chooser we do not own must sit where we can measure it, and
measurement is only honest if the meter sees the request before the choice. Keeping our rules
router means a fresh install routes with zero external binaries.
**Cost if wrong.** Two proxies in the path add latency (~ms). A double-translation bug between
Anthropic and OpenAI wire formats is possible; the conformance test in spec-09 pins both.

### 5.8 Per-layer benches, five cross-layer pairs

**Decision.** `daisugi bench <layer>` compares the main options *within* a layer over a fixed
corpus. Exactly five cross-layer pairs get a row: verifier × shell decomposition, matcher ×
distiller cluster quality, router × model set, loop × gate path (with its fail-open class),
backend model × envelope quality.
**Alternative.** An n×n matrix.
**Why.** The layer boundaries are contracts; a contract makes the two sides independent by
construction, so most cross-layer cells are theatre. The five pairs are where a real interaction
exists (shared input shape, shared threshold, shared wire format). A pair that surprises us gets
promoted to a row; nothing gets a row on speculation.
**Cost if wrong.** A missed interaction. The promotion rule bounds the damage.

### 5.9 Hot swap where the architecture permits, and say where it does not

**Decision.** Matcher, backend, router, verifier dispatch, and loop become `live`. Gate mode stays
`cfg`.
**Why.** The gate hook file belongs to the harness (`.claude/settings.json`, `hooks.json`). We do
not own its reload. Dressing that as live would be exactly the dishonest control §3.5 forbids.
Everything we own in-process can reload: the embedder (cache reset + lazy re-embed), the client
choice (per call), the gateway config (SIGHUP-style reload), the verifier client (dispatch by
name — but the Python oracle always runs and both must allow; a compiled client is a second
opinion, never a replacement, ruling 2026-09-08), and the loop (a pane is a process; swapping is
spawning).

### 5.10 int8 is its own identity

**Decision.** The int8 matcher gets identity `all-MiniLM-L6-v2-int8`, its own FPR-derived
threshold, and no claim of equivalence with MiniLM.
**Why.** Quantised embeddings are ~95% similar to the float ones, which is "close", not "same".
Provenance that says MiniLM when the vectors came from int8 would poison a store silently. The
matcher line already learned this with potion (ADR-0018, ADR-0019).

### 5.11 Voice: the transcriber is a commodity, the bridge is the product

**Decision.** `daisugi voice serve` accepts audio, transcribes with faster-whisper or Parakeet,
optionally cleans up through the gateway on a local model, and delivers text to a chosen pane as
a *prompt the operator sees*. Phone and laptop are just recorders.
**Why.** Handy, FUTO, and Wispr have solved recording, VAD, and hotkeys; rebuilding that is
months for nothing. What nobody ships is "record anywhere, transcribe on the box you own, land in
the pane you chose." The cleanup pass is itself a pathway the garden can distil.

### 5.12 Any self-hosted box, ours first

**Decision.** The model-host route is generic (`host:port`, Tailscale or LAN). The default is
the machine in front of you (llamafile or Ollama, CPU or whatever GPU is present). A remote
GPU box is the first remote we try, not a special case.
**Why.** Local-first means the zero-config path runs on the machine in front of you. A remote
host is a swap, not a requirement.

---

## 6. Sub-projects and order

| # | Spec | Delivers | Depends on | Lang | Size |
|---|---|---|---|---|---|
| 00 | `spec-00-adr-0020-and-vision.md` | ADR-0020, VISION §invariants + scorecard, import-boundary test | — | docs + 1 test | S |
| 01 | `spec-01-floor-contracts-and-herdr-hook.md` | `opendaisugi.floor` package: PaneStateEvent, PaneBackend, `daisugi hook … --report`, Herdr bridge | 00 | py | S |
| 02 | `spec-02-coppice-server.md` | `harness/coppice`: server, PTY + ghostty, headless adapters, manifests, socket API, CLI | 01 | go | XL |
| 03 | `spec-03-coppice-client-and-backends.md` | floor screen in the cockpit, `daisugi coppice`, coppice/Herdr/tmux backends, contract suite | 01, 02 | py | L |
| 04 | `spec-04-pi.md` | pi headless adapter (in 02's adapter slot) + `daisugi install --harness pi` gate extension | 01, 02 | go + ts | M |
| 05 | `spec-05-opencode.md` | OpenCode adapter + plugin | 01, 02 | go + ts | M |
| 06 | `spec-06-phone.md` | PWA client, ntfy push, `tailscale cert` or local CA | 02, 03 | go + js | L |
| 07 | `spec-07-voice.md` | `daisugi voice serve`, phone record button, laptop push-to-talk | 03 (delivery), 06 (button) | py | M |
| 08 | `spec-08-model-host-route.md` | `daisugi setup --remote`, probe, context floor, count-tokens shim | — | py | S |
| 09 | `spec-09-switchyard.md` | `daisugi install --router switchyard`, gateway upstream, per-model share | 08 | py | M |
| 10 | `spec-10-int8-matcher.md` | onnxruntime matcher, pinned model, threshold | — | py | S |
| 11 | `spec-11-layer-management.md` | `daisugi bench <layer>`, hot swap for 5 stages, verifier dispatch | 03, 09, 10 | py | L |

Critical path: 00 → 01 → 02 → 03 → 06 → 07. Independent lanes that can run in parallel with it:
08 → 09; 10; 04 and 05 after 02's adapter interface exists. 11 last, because it benches what the
others built.

Sub-projects 06 through 09 assume the workshop from the audit report exists (a Linux VM on the
desktop, Tailscale, the model server). Each of their plans starts with a "prerequisite check"
task that verifies the host and stops with a teaching message if it is absent.

---

## 7. Testing philosophy for this work

- **A backend contract is one suite, many backends.** If a test cannot run against all three
  pane backends it is not a contract test; it belongs to that backend's own tests.
- **Golden grids.** PTY rendering is tested by feeding recorded VT byte streams into a pane and
  comparing the grid to a golden text file. Recordings live under `harness/coppice/testdata/vt/`
  and are captured once from real harnesses with `script(1)`.
- **Manifest table tests.** Every vendored manifest gets at least one screen fixture per state
  it claims to detect.
- **Fail-closed tests are named for the failure.** `test_pi_extension_blocks_when_gate_unreachable`,
  not `test_error_handling`.
- **Live tests skip, never fail, when the host is absent**, and they say which host.
- **No benchmark claims a number that its script cannot reproduce.** `daisugi bench` prints the
  command that generated every table.

---

## 8. Out of scope, on purpose

- A native Android app. The PWA plus the ntfy app is the phone.
- Herdr Mobile compatibility beyond "run Herdr as the backend and it just works."
- Tailnet Lock, devcontainers, Forgejo (the audit's own culls).
- Multi-user coppice. One server, one uid, one operator.
- Replacing Tailscale. Ever.
