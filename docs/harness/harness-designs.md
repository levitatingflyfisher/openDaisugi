# Three opinionated harness designs — a Pi competitor with a proven spine

*2026-08-24. Bets, not a survey. Informed by `research-notes/2026-08-23-harness-meta.md`
(the landscape) + `…-skills-souls-shorthand.md` (the DSL/envelope thread) + the
tool-interface-design doctrine (`docs/research/tool-interface-doctrine.md`). A design-focused
deep-research (`wu83vibdh`) is running; this will be refined against it. Honesty first: per the
landscape's own verdict, building a harness from scratch is "a weekend education, not a strategy."
So the pitch is not "dethrone Claude Code." It is: **rent your harness, own your safety** — a
minimal, automatable harness whose one non-negotiable edge is openDaisugi's runtime envelope, and
which is a switchable openDaisugi module, take-it-or-leave-it.*

## The name

The daisugi metaphor is already the product: one trunk (`大杉`), many straight shoots harvested
again and again. A harness that raises many agent shoots from one trunk **is** daisugi made literal.
Working name: **`sprig`** (one minimal shoot) for the core, **`grove`** for the fleet. Pick later.

## The through-line (true of all three)

1. **The envelope is the moat, not the chrome.** Every proposed tool call is verified against an
   openDaisugi envelope *before* it runs, Z3-backed, fail-closed. This is the one thing pi, opencode,
   and Grok Build do **not** have, and it is the published-peer idea (AgentSpec, ICSE 2026: a formal
   grammar where "if the language has no DROP TABLE, the agent can't drop a table"). Rent the loop;
   own the boundary.
2. **Pi-minimal core.** The loop is `read/write/edit/bash` + the model, system prompt + tool defs
   held under ~1k tokens (pi's measured floor; the harness that sends ~3× less context won Databricks
   on pass-rate *and* cost). Everything past that must earn its tokens.
3. **Automatable by construction (clig.dev).** A `-p/--print` headless mode, `--json`, stdout for
   the answer / stderr for logs, real exit codes, `-` for stdin. If it can't run in cron and a pipe,
   it isn't done.
4. **Portable identity.** Reads `AGENTS.md` and `SKILL.md` (the won standards) so a skill written
   today runs here too. The harness is disposable; your workflow layer isn't.

---

## Design A — `sprig`: the pin (out-pi pi)

**The bet:** the smallest possible correct loop, plus the envelope. No TUI, no subagents, no MCP.
A single Rust binary that is faster and *provably safer* than pi at the same token floor.

**Pole (doctrine):** *infant-simple to invoke.* `sprig "add a test for parse_url"` just works — zero
config, one obvious affordance. The expert surface is flags, not chrome.

**Shape:**
- The loop: stream from the model, parse tool calls, **gate each call** (`verify(call, envelope)`),
  execute or refuse with the proven reason, feed the result back. That's it.
- Four tools. Bash is the universal escape hatch (Zechner's lesson: a CLI script + README beats a
  13–18k-token MCP; `sprig` ships *zero* MCP and lets bash + skills do the rest at ~225 tokens).
- Envelope: in-process check (no sidecar latency) via the openDaisugi core. Fail-closed: an
  unverifiable call is refused, not allowed.
- Automatable: `sprig -p "…" --json | jq`, exit 0/nonzero, `--envelope path` to pin the boundary.

**Token story:** ~1k harness overhead; the envelope lives *outside* the token stream (it's a check,
not a prompt), so safety costs ~0 context occupancy — the exact advantage over stuffing "be careful"
into the system prompt.

**Honest tradeoff:** boring on purpose. No fleet, no game. It's the thing you script and trust. If
pi already feels right and you only wish it couldn't `rm -rf` your repo, this is the whole pitch.

**Doctrine grade:** pole✓ throughput✓ (pipe + flags) honesty✓ guard-rails✓✓ (the envelope *is* the
guard rail) state✓. The infant pole done right.

---

## Design B — `grove`: the fleet (the many-agent instrument)

**The bet:** the pain point the landscape names is *status visibility across many agents*. Solve
that. A headless daemon runs N `sprig` shoots concurrently — each in its own git worktree/branch
(Grok Build's trick) — and a keyboard-first TUI supervises the fleet like an expert instrument.

**Pole (doctrine):** *expert-instrument with the ramp on screen.* This is the power tier. Keys
printed in the chrome (lazygit), a `:` command line (k9s/Bloomberg), color that encodes state (htop).

**The client/server split (opencode's proven pattern):**
- `grove serve` — headless daemon, holds all sessions; survives disconnects; reachable over
  Tailscale/SSH. Push via hooks → ntfy when an agent needs you.
- Thin clients attach: the TUI, `grove -p` for scripts, a phone browser. Reconnect and the fleet is
  still there.

**The "video-game-informed" UI — the honest verdict (doctrine + to be checked by research):**
the value of games here is **glanceable state encoding, not gameplay.** Steal the *readout*, not the
animation:
- A **fleet minimap** (RTS/`htop` hybrid): one row/tile per agent, color = state — `thinking` (dim),
  `blocked-on-gate` (amber: the envelope refused a call, needs your ruling), `awaiting-review` (cyan:
  a diff is ready), `running` (green), `done`/`failed`. The whole fleet's health in one screen — the
  thing tmux can't give you.
- **Hotkey to warp** to any agent's transcript/diff (StarCraft control-groups: number keys jump to
  units). Tab cycles "needs-you" agents only.
- **The gate as a game event.** When an agent hits the envelope, it *pauses as a unit* and flashes
  amber — you approve/deny/edit-envelope from the fleet view. This is the differentiator turned into
  the core interaction: you supervise *decisions*, the envelope supervises *safety*.
- **Cut the eye-candy.** No real-time animated factory belts, no isometric world. Doctrine law:
  capability without training (glass-cockpit) and delight-that-slows-experts are failure modes. A
  live animated map is slower to read than colored rows. Ship the minimap; skip the diorama. *(The
  research is explicitly asked to confirm or overturn this.)*

**Token story:** each shoot is a `sprig` (minimal); sub-agent context isolation means the fleet's
total context is N small windows, not one bloated one — the measured way many-agent scales without
the harness tax compounding.

**Honest tradeoff:** more to build, and a real risk of building a beautiful cockpit nobody out-flies
tmux with. Mitigation: the minimap must beat `tmux + ntfy` on the *one* job (which agents need me
now?) or it's chrome. Grade it against that before shipping.

**Doctrine grade target:** the expert pole; the ramp is the Footer + the `:` line + color state — the
same rubric the openDaisugi dashboard just passed 6/6.

---

## Design C — `weave`: the conductor (automation-first, workflow-as-data)

**The bet:** the most *automatable* harness is one you drive with a tiny declarative workflow DSL —
a formal surface the model proposes in and the machine checks (Anka: a purpose DSL hit 99.9% parse /
+40pp over Python because "the space of valid code it can generate is so much smaller"). Workflows
become data: a DAG of steps, each an agent action, each gated by the envelope.

**Pole (doctrine):** *infant-simple to run a workflow, expert to author one.* `weave run ship-pr`
is one command; the `ship-pr.weave` file is the expert surface.

**Shape:**
- A minimal workflow grammar (`step`, `parallel`, `gate`, `on-fail`) — the same shape as openDaisugi's
  own Workflow tool, but as a first-class, checkable harness primitive on disk. The grammar *is* a
  safety boundary: a step that isn't in the language can't run.
- Deterministic control flow (loops, fan-out, retries) in the harness; the model fills the leaves.
  This is the patio11 lifecycle: human defines the surface, agent implements/populates, human
  ratifies — with the envelope as the ratifier.
- Automation-first: `weave run pipeline.weave -p --json`, exit codes per step, resumable, cron-able.
  The "video game" is optional — a live DAG/factory view of the pipeline executing (Factorio-as-
  *diagram*, not as game), which here is genuinely useful because a pipeline **is** a flow graph.

**Token story:** the workflow shell spends *zero* model tokens — control flow is code. The model is
called only at leaves, each with a minimal `sprig` context. Cheapest of the three per unit of work.

**Honest tradeoff:** a DSL is a commitment; get the grammar wrong and it's a straitjacket (the
Microsoft finding: niche DSLs start <20% cold, need 3–5 canonical examples + a validation loop). Keep
it tiny and example-driven, or don't ship it.

---

## The synthesis — one product, three layers

These are not rivals; they stack, and the honest MVP order is **A → C → B**:

- **`sprig` (A)** is the atom: one proven, minimal, automatable agent. Build first; it's the whole
  thing for the infant tier and the unit for everything else.
- **`weave` (C)** wraps many sprigs in deterministic, checkable workflows — the automation win, and
  it needs only A + a small grammar.
- **`grove` (B)** is the expert cockpit over live sprigs/weaves — the most build, the most risk, the
  coolest demo. Build last, and only if the minimap beats `tmux + ntfy` at "who needs me now."

The through-line in all three is the envelope. That is the openDaisugi-modular part: `sprig` is
"another switchable module" — you can run the loop with the gate on (proven) or off (just pi), and
you can point the gate at any envelope source. Take it or leave it.

## Rust vs Go — my prior (research to confirm)

Lean **Rust**: it's where the serious minimal harnesses already live (Codex, Grok Build, claw-code),
single-binary distribution is trivial, `ratatui` + `tokio` are mature, and the openDaisugi verifier
has a Rust conformance client already — an in-process envelope check without a language boundary.
**Go's** counter-pull is real for Design B specifically: goroutines + Bubble Tea (Charm) + Wish
(serve a TUI over SSH) make the many-agent daemon and its clients markedly cheaper to build, and
opencode chose that stack for exactly this. Provisional call: **Rust for `sprig`/`weave` (A, C);
re-open the question for `grove` (B)** where Go's concurrency + SSH-serve story may win. The research
prompt asks this head-to-head.

## What's next
1. Deep-research (`wu83vibdh`) lands → refine the token-checklist, the Rust/Go call, and the
   game-UI verdict against measured evidence.
2. TDD-prototype **`sprig` (Design A)** first — the minimal loop + the in-process envelope gate — in
   the chosen language, superpowers TDD, atomic commits. It's the smallest thing that proves the
   whole thesis: rent the loop, own the boundary.

---

## Research addendum (deep-research `wu83vibdh`, 2026-08-24)

**Honesty first:** the run was **quota-truncated** — the synthesis step and ~23 verification
agents died to a session limit, so there is **no merged report, just 15 verified claims**, skewed
toward token-encoding. The loop/language/fleet angles mostly died. Decisions below lean on these
survivors + the harness-meta file + the doctrine, not a full synthesis.

**Token-minimization checklist (ranked, measured):**
1. **No MCP.** Eager tool-schema injection is the "Tools Tax": **~10k–60k tokens/turn** in typical
   multi-server setups (arXiv 2604.21816). sprig already ships zero MCP — this is the biggest win.
2. **Compress tool *results*, not tool *calls*.** Compact encodings save on schemas (−23%) and
   results (−32%) but give **no net saving on tool calls** — parse failures cascade (arXiv 2605.29676).
3. **Compaction for long runs.** Auto-summarize older context at a threshold (default 150k, min 50k),
   steerable via custom instructions (Anthropic docs). Add as an opt-in loop step.
4. ⚠️ **Novel encodings (TOON/TRON) are a trap for generation.** The instructional prompt-tax can
   *reverse* the savings (TOON cost Qwen3-235B **4715 vs JSON's 2772 tokens**). TRON is better (up to
   27% at ≤14pp accuracy) but still net-negative for some models. **Don't adopt naively;** JSON tool
   calls + compressed results is the safe default.

**Fleet UI verdict — CONFIRMED "readout, not gameplay."** `nicknisi/fleet` (a real tool) derives
per-agent status by **state-fusion**: hook signals (~0ms) + JSONL `stop_reason` parsing
(`tool_use`=busy vs `end_turn`=ready) + tmux `capture-pane` scraping (~50ms), on **tiered polling**
(500ms status, 5s scraping, 10s git, zero subprocesses on keypress). This is exactly `grove`'s
minimap done right — glanceable state, no animation. **Build the state-fusion minimap; skip the
diorama.** Baseline to beat: tmux + ntfy.

**Client/server (grove daemon) — use opencode's proven shape:** headless HTTP server (the TUI is
itself a client), a machine-readable **OpenAPI 3.1** contract at `/doc`, and live updates over
**SSE** at `/event` (opencode docs). Multi-client + Tailscale/phone falls out of this.

**Assurance boundary — DECIDED and BUILT.** AgentSpec (arXiv 2503.18666 + 2606.26057): the clean
gate is **out-of-process, on the only path, fail-closed at request+system levels, externalized
signed policy (P1–P4)**; ms overhead; >90% unsafe executions prevented. This *is* openDaisugi's
PreToolUse-hook contract → shipped as `DaisugiGate` (subprocess, exit-0=allow, else deny).

**Rust vs Go — decision stands (the head-to-head died to quota).** Go for `sprig` (built, tested,
fast iteration, goroutines for `grove`, an openDaisugi Go client exists, opencode precedent);
re-open for `grove` only if a Rust/Ratatui fleet view proves worth it. Both toolchains are on the box.

**Net:** `sprig` (Design A) is built end-to-end and proves the thesis. `grove`/`weave` are now
de-risked by measured precedent (fleet state-fusion, opencode serve/SSE). The one unbuilt leaf is
the model backend — deferred by the quota limit, not by uncertainty.
