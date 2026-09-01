# Spec 05 — OpenCode as a loop, and the OpenCode gate plugin

**Master:** §3.4, §3.6 · **Size:** M · **Depends on:** 01, 02 (adapter slot)

## Purpose

Same two joins as pi. coppice drives OpenCode through its headless HTTP server; the plugin gates
every tool call through the deny-only before-execute hook.

## The crux

OpenCode's `tool.execute.before` hook can only deny (throw). People call that a limitation. For
a fail-closed gate it is the correct primitive: there is no way for the plugin to accidentally
grant anything, and "throw on anything but an explicit allow" is one line. The `permission.ask`
hook is the second signal: when OpenCode itself pauses for permission, we report `blocked` so the
floor shows it. We do not auto-resolve OpenCode's own prompts; that would be the plugin deciding.

## Files

```
harness/coppice/internal/adapters/opencode/adapter.go   # `opencode serve --port N`; HTTP + SSE
src/opendaisugi/harness_opencode/plugin/daisugi-gate.ts  # the plugin (package data)
src/opendaisugi/install.py                               # `--harness opencode`: materialise + register in opencode.json
src/opendaisugi/hook.py                                  # `--format opencode`
src/opendaisugi/modules.py
tests/test_install_opencode.py
tests/test_hook_format_opencode.py
tests/harness_opencode/test_plugin_block_logic.py
```

## Adapter (Go)

Start: `opencode serve --port <free port> --hostname 127.0.0.1` with `OPENCODE_SERVER_PASSWORD`
set to a per-pane random token (basic auth); wait for `/doc` to answer. Prompt: create a session
once (`POST /session`), then `POST /session/{id}/message` with the text (endpoint names read from
the server's OpenAPI at `/doc` in plan task 1 and pinned in the adapter). Events: SSE on
`/event`; map `session.status busy` → working, idle → idle, `permission.asked` → blocked with the
permission id and title, `session.error` → unknown. Steer = a follow-up message. Session id
recorded for resume (`opencode run --attach` is not needed; the adapter is the client).
Process exit → done.

## Plugin (TypeScript)

Location per OpenCode's plugin docs (plan task 1 confirms; expected
`~/.config/opencode/plugins/daisugi-gate.ts` or a `plugin` entry in `opencode.json`).

```ts
export const DaisugiGate = async ({ project, client, $, directory }) => ({
  "tool.execute.before": async (input, output) => {
    const v = await askGate({ tool: input.tool, args: output.args, cwd: directory, sessionID: input.sessionID });
    if (!v.allow) throw new Error(`openDaisugi gate: DENIED — ${v.reason}`);
  },
  "permission.ask": async (permission) => { reportState("blocked", permission); /* do not resolve */ },
  "event": async ({ event }) => { if (event.type === "session.idle") reportState("idle"); },
});
```

`askGate` is the same socket client as spec-04's, with `--format opencode`. Same fail-closed
table: allow only on `exit_code 0`; every other outcome throws. Timeout 5 s.

**Found while planning (2026-09-08):** `gate.py _outcome()` returns `exit_code 0` for every
format except `claude`, so any client that reads the exit code would always see allow. Plans
04 and 05 both fix this; the reviewer reconciles them into one change (a set of exit-code
formats), landed once.

## Install

`daisugi install --harness opencode`: writes the plugin file, adds it to the user `opencode.json`
`plugin` list if that is the documented mechanism (idempotent by path), prints the honest line
about the resident gate. Uninstall reverses both.

## Tests

- Adapter: fake OpenCode server (Go `httptest`) serving `/doc`, session create, message, and an
  SSE stream from a recorded fixture → expected state sequence; basic auth enforced.
- Plugin logic under node with a stub context and fake gate socket: allow → no throw; deny → throws
  with the reason; unreachable → throws with the start hint; `permission.ask` → one `blocked`
  report and no resolution call.
- `--format opencode` mapping parity with the Claude format.
- Install idempotence and uninstall.
- Live (skip without `opencode`): one gated tool call visible in the gate log.

## Out of scope

OpenCode Go (the paid capacity tier); OpenCode's own subagents; its web UI.
