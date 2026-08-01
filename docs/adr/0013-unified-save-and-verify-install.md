# ADR-0013 — One install that both saves and verifies, across harnesses

**Status:** Accepted. Implemented (Phase 4) — `daisugi install --gate --gateway` wires the
GATE and BASE_URL layers through the per-harness `Runtime` adapters, shadow-by-default, with
the Codex/Hermes/OpenClaw gaps surfaced honestly in the install output. Builds on ADR-0004
(layer, not harness), ADR-0007 (call-time gate), ADR-0012 (gateway reuse).

## Context

`daisugi install` (`install.py`) is already a mature per-harness installer: a `Runtime`
adapter per harness (Claude Code, Codex, Hermes, OpenClaw) wires the **skill**, the **MCP
server**, a passive **capture hook**, and **instructions**, through idempotent, backed-up,
safe-merging config writes. Two things it does *not* wire, and both are the point of this ADR:

1. **The fail-closed gate (ADR-0007) is never installed.** `daisugi gate settings` only
   *prints* a `PreToolUse` block for inline use (`claude --settings "$(daisugi gate settings)"`).
   No installer writes it into a harness config. So the *verify* half of the stack is
   opt-in-by-copy-paste, not installed.
2. **The gateway base_url (ADR-0012 / Phase 1) is never wired.** `daisugi gateway` only
   *prints* `ANTHROPIC_BASE_URL=…` for the user to export. So the *save* half is not installed
   either.

"One install that both saves and verifies" therefore does not exist yet. Online research into
the four harnesses' real config mechanisms (primary sources, 2026-08) also shows there is **no
single shared config surface** to lean on:

- **MCP** unifies cleanly — the inner server object is `{command, args, env}` everywhere; only
  the file/key differ. (Already done in `install.py`.)
- **The tool gate splits three ways.** Claude Code and Hermes both call an **external command**
  (same verifier, different deny dialect: exit-2 / `{permissionDecision:"deny"}` vs exit-2 /
  `{"action":"block"}`). OpenClaw's hook is an **in-process TypeScript plugin** — it needs a JS
  shim to shell out. **Codex has no external allow/deny gate at all** (only a sandbox + static
  Starlark rules); its only interception point is the MCP boundary.
- **base_url is per-harness *and* multi-wire.** Only Claude Code takes it via env var; the
  others require a config-file write. Worse, the *wire protocol* differs: Claude Code and
  OpenClaw can speak **Anthropic Messages** (what the gateway emits); Codex and a Hermes
  *custom* provider expect **OpenAI**. The gateway speaks Anthropic Messages only.

## Decision

Add two layers to the install system — **`GATE`** (the fail-closed verify hook) and
**`BASE_URL`** (the gateway save pointer) — driven from one canonical source (the verifier
command, the MCP definition, the proxy endpoint) through the existing per-harness `Runtime`
adapters. **Wire what is verifiable; encode the rest as an honest capability matrix rather than
faking it.**

**GATE layer.**
- **Claude Code** — merge `gate_settings_json`'s `PreToolUse` block into
  `~/.claude/settings.json` via the existing `_patch_claude_settings` safe-merge. **Shadow by
  default** (ADR-0007's shadow-by-default posture); `--enforce` is an explicit opt-in.
- **Hermes** — the same external verifier command wired as a `pre_tool_call` shell hook
  (exit-2 deny) in `~/.hermes/config.yaml`. (Needs a Hermes deny-dialect in the gate; a scoped
  follow-up if not shipped in the first pass.)
- **OpenClaw** — its hook is an in-process TS plugin; wiring it needs a JS shim that shells out
  to the verifier. **Documented, not auto-wired.**
- **Codex** — no external gate exists. Safety rides the **MCP boundary** (openDaisugi's MCP
  tools verify before acting). **Documented.**

**BASE_URL layer.**
- **Claude Code** — `ANTHROPIC_BASE_URL` in `~/.claude/settings.json`'s `env` (Anthropic wire ✓).
- **OpenClaw** — `baseUrl` + `api:"anthropic-messages"` in `openclaw.json` (Anthropic wire ✓),
  via the existing `_patch_openclaw_config`.
- **Codex / Hermes-custom** — OpenAI wire. The gateway does not emit OpenAI Messages, so
  pointing them at it would break them. **Not wired** — documented, and gated behind an
  OpenAI-wire gateway adapter (a deliberate follow-up, not this ADR).

One canonical source of truth (verifier command + MCP def + proxy endpoint); thin per-harness
serializers (the `Runtime.apply` adapters). Every write stays idempotent, backed-up, and
dry-run-able (existing machinery). The gate is shadow-first and never enforces without an
explicit flag.

## Consequences

- **`daisugi install` for Claude Code now wires the complete stack** — skill + MCP + capture +
  **gate (verify)** + **base_url (save)**. That is the "one install that both saves and
  verifies" ADR-0012's Phase 4 named, realized for the reference harness.
- **The honest gaps, stated in the install output and here**: Codex gets MCP + skill but **no
  external gate** (MCP-boundary safety only) and **no base_url** (wrong wire); OpenClaw gets MCP
  + base_url but its **gate needs a JS shim**; a Hermes *custom* base_url is OpenAI-wire, so it
  is not wired. The gateway's Anthropic-only wire is the single load-bearing limit on full
  cross-harness *save*; an OpenAI-wire adapter is the follow-up that lifts it.
- **Writing into a user's `settings.json` is delicate.** Mitigated exactly as the existing
  layers are: shadow-default for the gate, `--dry-run`, a backup before every write, and
  idempotent safe-merge — never a wholesale rewrite.
- **Switchyard composition is already available** (`daisugi gateway --upstream <endpoint>`); the
  installer's base_url layer points a harness at the local gateway regardless of what the
  gateway forwards to, so compose-with-a-trained-router needs no new install surface.

## Invariants preserved (VISION §invariants)

The gate stays **fail-closed** and **shadow-by-default** (ADR-0007) — install never enforces
without an explicit opt-in. The gateway stays **fail-open** (a saver). The layer **never drives
the harness** (ADR-0004): install only wires surfaces the harness itself invokes (its hook, its
MCP client, its base_url) — it does not interpose on the model loop.

## Alternatives considered

- **One shared config file across all harnesses.** Rejected: the research shows no shared
  surface across gate + base_url + MCP. One canonical source with per-harness serializers is the
  only honest shape.
- **Rewrite each harness's config wholesale.** Rejected: it clobbers user settings. Idempotent
  safe-merge + backup (already in `install.py`) is the standard.
- **Emit an OpenAI wire from the gateway now, to cover Codex and Hermes-custom base_url.**
  Deferred: a full Anthropic↔OpenAI streaming multiplexer is a separate build; faking Codex
  base_url support without it would hand the user a broken config.

## Build order

4A the GATE layer (Claude Code, shadow-default; the Hermes shell-hook dialect as a scoped
follow-up) → 4B the BASE_URL layer (Claude Code env + OpenClaw anthropic-messages) → the honest
per-harness capability matrix surfaced in `daisugi install` output and the how-to. The
OpenAI-wire gateway adapter and the OpenClaw JS gate shim are named follow-ups, not this phase.
