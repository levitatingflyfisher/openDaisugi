# Five ways to drive a model — and gate it

*How sprig can put an LLM behind its envelope. Built and, where marked, proven live
against `claude -p` (Claude Opus 4.5-era CLI, subscription auth, model haiku). This doc
corrects an earlier design call — that native tool-use needed the TS/Python SDK and was
"off the table" for a Go harness. It was wrong. Native tool-use is reachable four ways;
the real constraint is auth, not language.*

## The axis that matters is auth, not language

Native tool-use — the model returns a structured `tool_use`, you run it, you return a
`tool_result` — is a feature of the **Messages API wire protocol**. Any language speaks it
over HTTP. The blocker is not Go vs Rust vs TS. It is **how you authenticate**, which then
dictates **where the tools come from**:

- **On subscription** (via the `claude` CLI, no API key — the exposure-freeze path): tools
  come from built-ins, or from **MCP servers** you attach, and are gated by **PreToolUse
  hooks**.
- **On an API key** (direct wire, like oh-my-pi): you pass a `tools:[]` array yourself and
  run the loop.

Both give real native tool calls. Four of the five paths below stay on subscription.

## The five paths

| Path | Who runs the loop | Where tools come from | Gate point | Auth | Status |
|---|---|---|---|---|---|
| **A · text** | sprig | a prompt convention (fenced JSON) | in-process `Executor` | subscription | prior |
| **B · text-hardened** | sprig | forgiving parse + worked example + one retry | in-process `Executor` | subscription | shipped, tested |
| **C · MCP bridge** | `claude` | **sprig's local MCP server** (`sprig-mcp`) | sprig's MCP handler → `Executor` | subscription | **live-proven** |
| **D · hook-gated** | `claude` | `claude`'s own built-in tools | **PreToolUse hook** (`sprig-hook`, exit 2) | subscription | **live-proven** |
| **E · direct-API** | sprig | native `tools:[]` array (the oh-my-pi way) | in-process `Executor` | **API key** | **built, opt-in** (needs key) |

The unifying thread: **one `sprig.Gate` interface** powers the in-process loop (A/B), the
MCP handler (C), and the hook (D). The boundary is the same object everywhere; only the
transport changes.

## Proven live — the gate holds while Claude drives

**C · MCP bridge.** `claude` drives, emits a native `mcp__sprig__write` call, `sprig-mcp`
routes it through the gated `Executor`.
- allow-gate → file written on disk.
- deny-gate → the driver's `tool_result` comes back `isError` with `REFUSED by the gate` →
  no file.

**D · hook-gated.** `claude` uses its *own* `Write`, a PreToolUse hook rules on it first.
- allow-hook → file written.
- deny-hook (exit 2, under `bypassPermissions`) → `tool_result is_error` → no file.

**The gate reasons; it is not a constant.** One hook gate, given a task that does both a
`Write` and a `Bash`, allowed the `Write` (file appeared) and denied the `Bash` (`"bash is
outside the envelope"`, no file). It discriminates on Claude's **native** tool vocabulary.

**The hook closes the bash blind spot.** A name-only matcher cannot see a `Bash` command's
content. `sprig-hook` reads the full `tool_input`, so the gate sees the actual command —
the exact gap openDaisugi's shell-decomposition ADRs already solve.

## C vs D — the honest tradeoff

| | C · MCP bridge | D · hook-gated |
|---|---|---|
| Model uses | sprig's tools (must learn them) | its own tools (already fluent) |
| Execution runs in | **sprig's** process | Claude's process |
| To make it route through sprig | **suppress the built-ins** (`--disallowedTools`) or the model prefers its own | nothing — the hook fires on every call |
| Gate sees | sprig's vocabulary (`write`, `{path}`) — matched end to end | Claude's vocabulary (`Write`, `{file_path}`) — the envelope must speak it |
| Operational weight | heavier (a server + tool-list surgery) | lighter (one settings file) |

**A near fail-open, caught while debugging.** With `--permission-mode bypassPermissions`
alone, Claude kept its built-in `Write` and used it *instead of* `mcp__sprig__write` — the
file appeared, but sprig never ran and the gate never fired. A file on disk is not proof
the bridge worked. C requires suppressing the built-ins; every C proof here was
re-verified with `stream-json` to confirm the tool that actually fired was `mcp__sprig__*`.

## The operational configs (non-obvious, load-bearing)

- **C:** `claude -p --mcp-config cfg.json --strict-mcp-config --permission-mode
  bypassPermissions --allowedTools "mcp__sprig__*" --disallowedTools "Write Edit Bash …"`
- **D:** `claude -p --settings hook.json --permission-mode bypassPermissions`
  - The hook **must exit 2** to deny. A JSON `permissionDecision:"deny"` at exit 0 is a
    decision *inside* the permission flow, which `bypassPermissions` skips. Exit 2 is an
    unconditional block, so a deny holds even with prompts bypassed.

## The live task battery

Metric of record is the **disk side-effect** (objective). Tokens (input+output, from
`stream-json` usage) are clean for C and D; the text path (B) shells `claude -p` per turn
without aggregating usage, so it reports turns, not tokens — an honest gap. A is not
re-run: it shares B's text architecture and its delta is pinned by the hardening tests.

**Result: 9/9 tasks succeeded.** Every architecture completed every task. The `tools`
column is the proof each path routed where intended.

| Path | Task | Tool(s) that actually fired | Tokens in / out | Time |
|---|---|---|---|---|
| B·text | write | *(sprig loop — 4 msgs)* | — | 11.6s |
| C·mcp | write | `ToolSearch` → `mcp__sprig__write` | 63.7k / 798 | 15.0s |
| D·hook | write | `Write` | 56.8k / 289 | 7.5s |
| B·text | edit | *(sprig loop — 6 msgs)* | — | 14.6s |
| C·mcp | edit | `ToolSearch` → `mcp__sprig__read` → `mcp__sprig__edit` | 84.7k / 615 | 11.8s |
| D·hook | edit | `Read` → `Edit` | 86.0k / 645 | 12.3s |
| B·text | bash | *(sprig loop)* | — | 102.3s ⚠ |
| C·mcp | bash | `ToolSearch` → `mcp__sprig__bash` ×2 | 84.8k / 706 | 13.3s |
| D·hook | bash | `Bash` → `Read` | 86.0k / 645 | 11.6s |

**What the numbers actually say:**

- **Correctness ties at this size.** On tasks this simple every path finishes. Adherence
  differences don't surface here — the hardening's value shows on *drift*, which the unit
  tests exercise, not a 3-task happy path.
- **Tokens are dominated by Claude Code's OWN context (~57–86k input, mostly cached), not
  sprig's tools.** This is the honest ceiling: on subscription, C and D both pay Claude
  Code's baseline. A minimal harness cannot shrink a context it is *renting*. Real token
  minimization is fundamentally an **E** (direct-API) game — the pi/omp "sub-1k prompt"
  win only exists when you don't drive through Claude Code.
- **C vs D on tokens: within a few percent, no clear winner.** But **C carries a
  consistent overhead** — a `ToolSearch` call to discover the *deferred* MCP tools, once
  per session. D's built-ins are always present, so it skips that round-trip.
- **D was fastest and lightest; the text path was the least predictable** — B·text took
  **102s** on the bash task (vs ~12s elsewhere) and its `--json` summary didn't parse
  there, though the file was still written correctly.

**Verdict (for subscription; E is a separate world):**

- **D · hook-gated is the pragmatic default** — fastest, lightest ops, the model uses
  tools it's already fluent in, gated natively with no discovery overhead.
- **C · MCP bridge when sprig must *own* execution** — its own process, its own tool
  vocabulary, its own sandbox — paying tool-list surgery and one discovery round-trip.
- **B · text for portability** — any text model, zero Claude-Code features — but slowest
  and (structurally, re-sending history per turn) the token-heaviest.
- **On subscription, tokens are close to a red herring: every road pays Claude Code's
  tax.** The token thesis lives in E, which the exposure freeze keeps parked.

## Across other subscriptions (Codex, Gemini, …)

The verdict above is for Claude. Extend the question to *other* subscriptions and the
ranking flips, because the axis becomes what that provider's tool exposes:

- **B · text is the portability winner.** Any provider with a headless CLI that takes a
  prompt and returns text — `codex`, `gemini`, others — drops into sprig's existing `Model`
  interface as a backend. The gate stays in-process and provider-agnostic, so the boundary
  is *identical* everywhere. A `CodexModel`/`GeminiModel` is a small addition; the gate is
  untouched.
- **C · MCP works where the provider CLI speaks MCP + headless** (Claude Code, Codex, Gemini
  all do, to a point) — but you re-solve the built-in-suppression and permission-mode dance
  per CLI.
- **D · hook is Claude-only, and unsafe elsewhere.** PreToolUse hooks are Claude-Code
  specific, and **Codex's hooks fail open** (openDaisugi already classifies them "Soft") —
  the gate guarantee *breaks* on Codex. Gemini has no equivalent.
- **E · direct-API is a different axis** — per-provider *API keys*, not subscriptions.

**So: D for Claude specifically; B for a fleet of subscriptions.** The `Model` interface
already exists for exactly this — the portable move is a per-provider CLI backend behind the
same in-process gate.

## E · direct-API — built, opt-in

The oh-my-pi pattern: speak `anthropic-messages` directly with a `tools:[]` array, run the
loop, gate in-process. Now built (`APIModel`, `SPRIG_BACKEND=api`) with a deliberately tiny
system prompt — the one path that escapes Claude Code's rented context, so the one place the
token thesis actually pays off. It is **OFF by default**: `NewAPIModel` refuses without
`ANTHROPIC_API_KEY`, so it never activates unless you bring a key. The live
token-minimization number is owed once a key is provided (this environment has none).

**The wiring is proven over a real socket, without a key.** A contract-validating mock
`/v1/messages` (see `model_api_smoke_test.go`) drives E's *whole* loop — real `net/http`,
the native `tool_use`/`tool_result` exchange with id pairing, and the gate — to a disk
side-effect. The mock validates each request against Anthropic's documented contract
(model, `max_tokens`, the `tools[]` array with an object `input_schema`, well-formed content
blocks, and the tool_result→tool_use pairing) and rejects violations, so a green run means
E emits a conformant request and completes the loop. `ANTHROPIC_BASE_URL` (the standard SDK
env var) points the real binary at that mock — or at an Anthropic-compatible proxy, or the
openDaisugi gateway. What the mock cannot prove, and only a real key can: that
api.anthropic.com *itself* accepts the exact schema, and E's real minimal-prompt token number.

## The real envelope, end to end

The battery above used stand-in gates. Wired to the ACTUAL openDaisugi envelope (Z3-backed,
`python -m opendaisugi.gate --mode enforce` against a registered envelope), both native
paths discriminate live:

- **D (hook):** Claude's `Write` to a scoped path → ALLOWED (file on disk); `Bash` →
  `openDaisugi gate: DENIED — requires shell but envelope forbids it` (no file).
- **C (MCP):** `mcp__sprig__write` → ALLOWED; `mcp__sprig__bash` → DENIED (no file).
- Standalone, the same envelope also denies a `Write` to `/etc/passwd` (path scope).

**The integration finding: the envelope classifies by tool NAME, and sprig's lowercase
`write`/`bash`/`read`/`edit` are not in its map — it denies unknown names by default.**
Claude's native `Write`/`Bash` (the D path) match the envelope's vocabulary as-is; sprig's
own tools did not, so every call was refused until `DaisugiGate` learned to translate
(`write`→`Write`, …). The translation lives in `DaisugiGate` itself, which **A, B, and C all
route through** — so it is not C-specific; the text paths inherit the same fix. That gives
**D on Claude** a genuine edge (its vocabulary already *is* the envelope's, no mapping), and
it also **strengthens B for portability**: a text backend on Codex or Gemini would hit the
exact same unknown-name denial, already solved by the same one map. The envelope's
`tool_input` keys already accept both `path`/`file_path` and `cmd`/`command`, so only the
name needed mapping.

Operational note: the gate was import-dominated by the package init, not Z3 (ADR-0017);
with the resident gate a call is milliseconds. A cold call (no server running) is still
several seconds, so the harness's fail-closed-on-timeout posture still needs a gate
timeout that clears a cold start (raised to 30s) — a healthy-but-cold gate must not be
wrongly denied.

## What this does not yet show

- No hard adherence *rate* over many trials — the battery is a handful of tasks, not a
  statistical run.
- The battery gate is allow-all (measuring the harness, not the gate); gating is proven
  separately above.
- The real-envelope proof (above) covers a few tool types, not an exhaustive policy audit,
  and the envelope was a minimal hand-written one, not inferred from a real session.
- E (direct-API) is built and unit-tested but not run live — this box has no API key, so
  its actual minimal-prompt token number is still owed.

## Provenance

Live runs against `claude -p`, model haiku, 2026-08. Paths C and D verified with
`--output-format stream-json` to confirm which tool actually executed. Design rationale in
`harness-designs.md`; the research behind the choices in `research-synthesis.md`.
