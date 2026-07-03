# ADR-0004: openDaisugi is a layer; MCP is the control pathway

- **Status:** Accepted
- **Date:** 2026-07-02

## Context

There is constant gravity pulling a tool like this toward *becoming the agent* —
add a chat loop, own the model calls, manage the session, and suddenly you've
rebuilt Claude Code / Codex / Gas Town, badly, and you're competing with the thing
you were supposed to make safer. The market already has capable harnesses. What it
lacks is a *shared assurance substrate* those harnesses can call. That gap is the
opportunity; filling it requires staying out of the harness business.

## Decision

openDaisugi is a **layer**, not a harness. It gates actions; it does not drive the
model. Integration is standardized on two mechanisms:

1. **MCP is the universal control pathway.** `mcp_server.py` exposes verify /
   orchestrate / journal tools; any MCP-capable harness (Claude Code, Codex,
   Cursor, Pi…) uses the same surface. No per-harness reimplementation of the core.
2. **`install.py` provides per-harness adapters** — the thin glue that wires
   openDaisugi into a specific harness (Claude Code hooks/settings, Hermes config,
   generic MCP registration). New harness = new adapter, same core.

The three consumption surfaces (library API, `daisugi` CLI, MCP server) all funnel
through the same `verify` + `Supervisor`. A standalone **TUI**, if built, is a
*monitoring* surface (watch runs, browse the journal/pathways) — explicitly not a
coding loop.

## Consequences

- **Buys:** cross-harness reach with one core; a clear differentiator (assurance +
  budget + memory that harnesses don't provide); no head-to-head competition with
  the harnesses we integrate into.
- **Costs:** we depend on host harnesses for the driving loop and inherit their
  quirks (auth, tool-permission models, non-interactive gotchas). Some UX we might
  want (rich interactive approval) has to be negotiated through the host.
- **Forecloses:** owning the agent loop. If a genuine need for a standalone
  *driving* agent ever appears, it must be a separate product built *on* this
  layer — not folded into it.

## Alternatives considered

- **Build a full standalone agent/harness:** rejected — recreates existing
  harnesses, abandons the "complementary substrate" position, and competes with
  our own integration targets.
- **Deep bespoke integration per harness (no MCP):** rejected — O(harnesses) work
  and drift; MCP gives O(1) core + thin adapters.
