# ADR-0006: `claude -p` as a keyless LLM backend (stopgap)

- **Status:** Accepted
- **Date:** 2026-07-02

## Context

openDaisugi needs an LLM for envelope generation, decomposition, synthesis,
LLMCheck, and task-step execution. Requiring an `ANTHROPIC_API_KEY` excludes users
who have a **Claude Code subscription but no API key** — a large share of the
target audience (people already living in Claude Code). We need a path that works
for them, gives real (not made-up) cost/token accounting, and doesn't fork the
whole codebase.

## Decision

Support a pluggable backend (`llm.py`): `litellm` (API-key) or **`claude-code`**,
which shells out to `claude -p` (`claude_code_llm.py`). Selected via
`OPENDAISUGI_LLM_BACKEND=claude-code` or `--llm claude-code`. Use
`claude -p --output-format json` to read Claude Code's *own* accounting for exact
`total_cost_usd` + token usage. Treat this as a **stopgap**: the Claude Agent SDK
(same subscription/auth, but amortizes context across calls) is the eventual
upgrade for anything cost- or latency-sensitive.

## Consequences

- **Buys:** the full pipeline runs with no API key; cost/budget numbers are exact,
  not heuristic. Removes the single biggest onboarding barrier for Claude Code users.
- **Costs:** every call is a subprocess that reloads Claude Code's full system
  prompt (cache-creation tokens dominate — ~2¢/call floor even for a one-liner),
  and it inherits Claude Code's environment quirks. Two of these bit us and are
  now handled: **project-context contamination** (runs in a neutral CWD so no
  `CLAUDE.md` leaks in) and **tool-permission prompts** (forward flags via
  `DAISUGI_CLAUDE_ARGS`, e.g. `--dangerously-skip-permissions`, or steps that need
  a tool will fail). Argv is built injection-safe (`--model=` + `--` separator).
- **Forecloses:** nothing — it's an additive backend behind the same interface, so
  moving to the Agent SDK later is a backend swap, not a rewrite.

## Alternatives considered

- **Require an API key (litellm only):** rejected — excludes the core audience.
- **Claude Agent SDK now:** deferred, not rejected — it's the right long-term
  answer (context amortization, real token streams), but `claude -p
  --output-format json` already delivers exact cost, so the SDK wasn't needed to
  ship keyless operation. Tracked as the successor.
