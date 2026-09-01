# Spec 04 — pi as a loop, and the pi gate extension

**Master:** §3.4, §3.6, §5.2 · **Size:** M · **Depends on:** 01, 02 (adapter slot)
**Research:** `docs/research/cockpit-sources-2026-08-27/pi_coding_agent__mario_zechner___badlogi.md`

## Purpose

Two joins. First, coppice drives pi headless through its RPC mode (JSONL on stdin/stdout), so a
pi pane is a supervision-grade pane: typed events, exact state, `extension_ui_request` becomes a
coppice ask. Second, every pi tool call passes the gate through a pi extension that answers the
`tool_call` event, fail-closed, so pi is gated whether it runs in coppice, in Herdr, or bare.

## The crux

pi has no MCP and no hooks file; its extension API *is* the integration surface, and its
`tool_call` handler can return `{block: true, reason}`. That is a native, in-process gate hook,
better than Claude Code's exit-2 convention. The fail-closed rule is therefore trivial to state
and easy to test: **any path that does not receive an explicit allow from the gate blocks.**
An extension that cannot reach the gate does not "degrade to allow"; it blocks with the reason
"gate unreachable" and the command to start it.

## Files

```
harness/coppice/internal/adapters/pi/adapter.go        # `pi --mode rpc`; prompt/steer/follow_up; events → state
harness/coppice/internal/adapters/pi/testdata/          # recorded RPC transcripts
src/opendaisugi/harness_pi/extension/index.ts           # the extension source (shipped as package data)
src/opendaisugi/harness_pi/extension/package.json       # keywords ["pi-package"]; pi.extensions = ["index.ts"]
src/opendaisugi/install.py                              # `--harness pi`: materialise into ~/.pi/agent/extensions/daisugi-gate/
src/opendaisugi/hook.py                                 # `--format pi` payload mapping
src/opendaisugi/modules.py                              # harness stage: pi ACTIVE/AVAILABLE
tests/test_install_pi.py
tests/test_hook_format_pi.py
tests/harness_pi/test_extension_block_logic.py          # runs the TS via node against a fake gate socket
```

## Adapter (Go)

Start: `pi --mode rpc --session-dir <coppice state>/pi/<pane>` (plus `--provider/--model` from
StartOpts when set). Prompt: `{"id":…, "type":"prompt","message":…}`. Steer:
`{"type":"steer",…}`; follow-up: `{"type":"follow_up",…}`. Resume: `switch_session` with the
recorded session id (SessionID() reads it from `get_state`).

Events → PaneStateEvent (`source: headless`): `agent_start` → working; `agent_end` /
`agent_settled` → idle; `extension_ui_request` with method in `{select, confirm, input, editor}`
→ `blocked` with `Ask{id: request.id, tool: method, summary: request.title or prompt,
deadline: now+90s}`; the pane's `agent.prompt` while blocked answers it by sending
`extension_ui_response` with the text (for `confirm`, `y`/`yes` → true). `extension_error` →
`unknown` with detail. Text deltas (`message_update`) are appended to the grid transcript.
Process exit → `done`.

## Extension (TypeScript)

```ts
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
export default function (pi: ExtensionAPI) {
  pi.on("tool_call", async (event, ctx) => {
    const verdict = await askGate(event, ctx, { timeoutMs: 5000 });   // {allow:boolean, reason:string}
    if (!verdict.allow) return { block: true, reason: verdict.reason };
  });
  pi.on("session_start", (e, ctx) => reportState("idle", ctx));
  pi.on("agent_start", (e, ctx) => reportState("working", ctx));
  pi.on("agent_end", (e, ctx) => reportState("idle", ctx));
}
```

`askGate` connects to the resident gate socket (`~/.opendaisugi/gate/gate.sock`, or
`OPENDAISUGI_GATE_SOCK`) with node's `net`, sends one JSONL request in the shape
`gate_server.py` reads: `{"argv": [<the gate's own flags: --mode, --format pi, --root>], "stdin_b64": <payload>}`
(**corrected 2026-09-08:** the earlier `["hook","record",…]` shape targets the passive capture
command, which never blocks; the plan pins the real argv from `gate.py _build_parser`),
where payload is `{session_id, tool_name, tool_input, cwd}` from the event and ctx. Reply
`exit_code 0` → allow; `2` → block with `stderr` as reason; anything else, a timeout, a socket
error, a parse error → block with `"openDaisugi gate unreachable: run daisugi start"`. The socket
absent → same. There is no `daisugi gate` CLI fallback from the extension: a cold in-process gate
is 0.7 s and the extension's budget is 5 s, so the fallback *could* work, but two code paths to
"allow" is one too many for a fail-closed component; the resident gate is the one path.

`reportState` sends the §3.1 event through the same socket as `{"argv":["hook","report",…]}`
(spec-01 adds `hook report` as a thin subcommand) and never blocks the agent (fire and forget,
1 s timeout).

Tool name mapping (`hook.py --format pi`): `bash` → shell; `read`/`write`/`edit` → file with
`path`; anything else → the tool name as an MCP-style tool with its input, denied unless the
envelope names it. Same classification the Claude format uses; only the field names differ.

## Install

`daisugi install --harness pi [--gate enforce|shadow]`: writes the extension directory into
`~/.pi/agent/extensions/daisugi-gate/` (pi hot-reloads extensions from there), records the
install in the ledger, and prints the honest line: "pi is gated in-process. Start the resident
gate with `daisugi start` or every tool call will block." Uninstall removes the directory. The
cross-tenant uninstall ref-count residual (install.py ≈1307) applies here too; the plan touches
it only to add pi to the ref-count set if that fix has landed, else leaves a comment.

## Tests

- Adapter: a fake `pi` script replaying `testdata/*.jsonl` → the expected state sequence; a prompt
  reaches stdin as the exact JSON; `extension_ui_request` → blocked with the ask id; answering
  sends `extension_ui_response` with that id.
- Extension logic: run `index.ts` under node with a stub `pi` object and a fake gate socket:
  allow → handler returns undefined; deny → `{block:true, reason}`; socket absent → block with the
  unreachable reason; slow socket (6 s) → block within 5.5 s.
- `--format pi` mapping: bash/read/write/edit/other → the same classes the Claude format yields
  for equivalent calls (parametrised against `_classify_tool`).
- Install idempotence: two installs, one directory, same content; uninstall removes it.
- Live (skip without `pi` on PATH): spawn a pi headless pane in coppice with the extension
  installed, prompt "run ls", see a `tool_call` reach the gate log.

## Out of scope

pi themes/prompts/skills packaging; pi's own subagent extension; oh-my-pi's built-in MCP.
