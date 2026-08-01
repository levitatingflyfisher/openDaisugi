# Harness design — the deep-research synthesis

*2026-08-25. Hand-written synthesis from **262 verified claims** in run `wf_9199d220-63e`
(109/110 agents; the automatic synthesizer failed its structured-output cap twice, so this
is written by hand from the journal). Confidence is high where a primary source + arXiv id is
named; "asserted" flags an opinion the corpus did not measure. This supersedes the earlier
quota-truncated addendum.*

## Verdict per angle

### 1. The minimal loop — the thesis holds, hard
- A working coding agent is **"an LLM + a conversation loop + a few tools + enough tokens"** —
  buildable in **under 400 lines of Go**, most of it boilerplate. Tools are wired as native
  tool-use JSON schemas; the model returns structured tool calls you parse and run.
- **pi** is the reference: exactly **read/write/edit/bash**, a **sub-1k-token system prompt**
  (~25% of which just teaches pi to read its own manual), four packages, a ~600-line TUI, and it
  ranked **#2 on TerminalBench** (Opus 4.5) *before* it even had compaction. OpenClaw is built on
  pi's core — the "SDK core" thesis, validated.
- Ronacher's counsel: **don't use an SDK/framework** (Vercel AI SDK etc.); drive the loop by hand
  until the space settles. Isolate failures in **sub-agents** (run to success, report only success
  back — keep failures out of the main context).
- **→ sprig matches this exactly:** 4 tools, minimal loop, no SDK. Confirmed.

### 2. Token minimization — ranked, measured
1. **Ship no MCP.** Eager tool-schema injection is the **"Tools Tax": ~10–60k tokens/turn**;
   schemas occupy **15–30KB before the first user message**; a heavy tool ≈ 1k tokens each. This is
   the single biggest win, and sprig already takes it (zero MCP; bash + skills instead).
2. **Lazy/tiered tool schemas** save **60–70%**; an intent-gated two-phase loader hit a **95% cut**
   (context utilization 24%→91%) — *but on a synthetic 120-tool benchmark, not live agents.*
3. **Compress tool *results*, not tool *calls*** (results −32%, calls no net gain — parse failures
   cascade). **Compaction** helps occupancy (steerable: 150k default / 50k min) but is **not free** —
   its summarization step itself costs tokens.
4. **Do NOT adopt novel encodings.** TOON collapses on nested data (**0%** one-shot on the nested
   case), is unsafe as a multi-turn default, and its prompt-tax can make it cost *more* than JSON
   (Qwen3-235B: 4715 vs 2772). TRON is defensible only with many structurally-similar tools.
   **Plain JSON had the best accuracy;** thinking-mode is the load-bearing variable for format
   robustness. → JSON tool calls + compressed results is the safe default. Confirmed my earlier read.

### 3. Many-agent orchestration & the game UI — the honest verdict
- **The status-visibility pain is real, and the answer in practice is tmux + notifications + state
  fusion — NOT a game.**
  - **Fleet** (nicknisi): a tmux dashboard + **three-layer state fusion** (hook signals ~0ms + JSONL
    `stop_reason` parse busy/ready + tmux `capture-pane` ~50ms, tiered polling, 7 states, a JSON
    envelope + scriptable CLI verbs). **This is exactly grove.**
  - **workmux / Gas Town**: tmux-window-per-worktree *is* the dashboard. Yegge runs **20–30
    instances** and says plainly: *"a better UI is anticipated but does not yet exist."* Even the
    most advanced orchestrator ships tmux.
- **The RTS/god's-eye idea is being productized but is asserted, not measured.** AgentCraft
  (`npx @idosal/agentcraft`) is a real RTS-style fleet UI (container isolation, mobile PWA, push).
  Its thesis — RTS unit-control transfers to agent supervision — is a *design bet*; the "200 units"
  figure is **marketing**, and an HN counter lands: *"management games always devolve into
  spreadsheets."*
- **What to steal vs skip** (this is grove's mandate, confirmed): steal the **resource readout**
  (tokens/context as "lumber/gold"), the **event log** for attention, and **glanceable per-agent
  state**. Skip the **spatial diorama** (untested eye-candy).
- **The honest counter you should hold:** **Ronacher deliberately does NOT run swarms** — an expert
  negative. Shared mutable state is the real barrier (hence **worktree-per-agent isolation**). So
  fleets are a power-user move; the minimal single agent (sprig) is the safer default.

### 3b. Client/server headless — a proven shape
- **opencode serve**: headless HTTP, the TUI is itself a client, **OpenAPI 3.1** at `/doc`, **SSE**
  at `/event`, binds `127.0.0.1:4096`, mDNS/CORS/auth, 60+ endpoints. This is grove's daemon shape
  when it grows one.
- **Notifications**: `hooks → ntfy` over a **Tailscale sidecar**; a lazy-start local daemon (zero
  standing process); **remote tap-Allow/Deny on a phone works today**, routed by the existing
  permission rules (only calls that need approval ping you).

### 4. Rust vs Go — nuanced, and Go wins for *this* builder
- **For agents to work *on*, Go is easier:** agents misparse Rust's `cargo test`; Go's explicit
  context passing and structural interfaces are easier for an LLM to reason about. And **Gas Town
  (a serious many-agent orchestrator) chose Go.**
- **For raw performance, Rust wins:** Codex rewrote TS→Rust (zero-dep binary, OS sandboxing, no GC,
  wire protocol); Ratatui used **30–40% less memory / 15% less CPU** than Bubbletea at 1000 pts/s;
  the strongest Rust argument is **unbounded long-session state + GC pause unpredictability** — Rust
  frees deterministically. Startup: Rust ~10ms, Go ~20ms, TS ~100ms, Python ~300ms.
- **Recommendation (explicit in the corpus): for a solo builder whose goal is development speed +
  a slick TUI + easy automation, Go/Bubble Tea.** Rust only if you hit long-session memory pressure
  or need the FFI/WASM embedding story.
- **→ sprig/grove in Go is validated** (claims: Go-easier-for-agents, Gas-Town-Go, Go-for-dev-speed),
  with the honest caveat that a very-long-running session would favor Rust.

### 5. The assurance boundary — validated, plus a real warning
- **AgentSpec** (arXiv 2503.18666): triggers + predicates + enforcement; **>90%** unsafe executions
  prevented; **ms** overhead; rules can be **LLM-auto-generated** (95.56% precision / 70.96% recall);
  domain-agnostic (code / embodied / autonomous-vehicle). This is openDaisugi's published peer.
- **The gate contract is exactly a PreToolUse hook: exit 0 = allow, exit 2 = deny.** DaisugiGate
  implements precisely this. Confirmed.
- **The gate *improves quality*, not only safety:** a precondition/scope gate adds **+3.6pp** success;
  removing a hallucination-rejection gate costs **−3.2pp**. The envelope is a capability, not a tax.
- **⚠️ The warning that matters most for sprig:** *PreToolUse hooks match on tool NAME only and
  cannot inspect Bash content* — so a gate that governs Write/Edit has a **blind spot for file
  writes done via shell (heredocs, redirects).* This is **exactly** the shell-decomposition problem
  openDaisugi's ADR-0010/0014 already solves. sprig's DaisugiGate delegates to openDaisugi, so it
  **inherits the fix** — but it is the reason a naive name-only gate is unsafe, and the reason the
  openDaisugi envelope (not a stock hook) is the right spine.

## Net, for the build
- **sprig / grove / weave in Go: validated.** The Go choice, the minimal loop, the tmux-style
  glanceable fleet readout, the exit-0/2 gate contract, and the envelope-as-differentiator are all
  supported by measured evidence.
- **Two honest counters to carry:** fleets are contested (Ronacher avoids them; the game UI is
  asserted, not measured — grove's minimap is right, the diorama is not); and Rust would beat Go on
  very long single sessions.
- **The strongest new selling point:** the gate **raises pass-rate (+3.6pp)**, and a name-only hook
  has a shell blind spot the openDaisugi envelope closes — so "own your safety" also means "own your
  accuracy," and the envelope is load-bearing, not optional chrome.

*Provenance: run `wf_9199d220-63e`, 262 verified claims (journal). Key arXiv: AgentSpec 2503.18666;
TOON/TRON encoding studies; MCP Tools-Tax measurements. Full claim dump preserved in the run journal.*
