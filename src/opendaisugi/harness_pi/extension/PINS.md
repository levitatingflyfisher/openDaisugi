# pi RPC and extension API: pinned facts

Fetched 2026-09-13 from pi's own repository as raw markdown, so a future
re-fetch diffs cleanly against this file:

- Source A, extension API: https://raw.githubusercontent.com/earendil-works/pi/main/packages/coding-agent/docs/extensions.md
- Source B, RPC protocol: https://raw.githubusercontent.com/earendil-works/pi/main/packages/coding-agent/docs/rpc.md
- https://pi.dev/docs/latest/rpc renders the same content as Source B. The raw
  markdown is what this file quotes.

Do not re-derive any fact below from memory when `index.ts` or the Go adapter
is next touched. Re-read this file, or re-fetch the two sources above.

## tool_call handler (Source A, "tool_call")

- Fired after `tool_execution_start`, before the tool executes. It can block.
- `event.toolName`. Built-in tool names are lowercase: `"bash"`, `"read"`,
  `"write"`, `"edit"`. Claude Code uses TitleCase for its own built-ins. pi
  does not.
- The full built-in list (Source A, "Overriding Built-in Tools", line 2088
  of the fetched file): `read`, `bash`, `powershell`, `edit`, `write`,
  `grep`, `find`, `ls`. `bash` and `powershell` both take
  `{ command: string; timeout?: number }`. `read` takes
  `{ path: string; offset?: number; limit?: number }`. `write` and `edit`
  take `path`. The docs do not pin the parameter names of `grep`, `find`
  and `ls`, so hook.py leaves those three MCP-style: an envelope admits
  them by name in `mcp_allowlist`, and a re-fetch that pins their input
  shape can move them into `_PI_TOOL_TYPE_MAP`.
- The extension sends `session_id` only when `OPENDAISUGI_SESSION_ID` is
  set. Without it the gate consults the `default` envelope.
- `event.toolCallId`, `event.input`. The input is mutable in place. This
  extension does not change it.
- Return `{ block: true, reason?: string, terminate?: boolean }` to block.
  Return `undefined` or nothing to allow.
- `terminate` only applies to a blocked call.
- Error Handling section, quoted:
  "tool_call errors block the tool (fail-safe)".
  pi's own runtime treats a thrown error inside the handler as a block. The extension still returns an explicit block with a real reason. The
  runtime rule is a second line of defense.

## Lifecycle events used for state (Source A and Source B, "Event Types")

- `session_start {reason: "startup"|"reload"|"new"|"resume"|"fork", previousSessionFile?}`.
- `agent_start {}`: a low-level agent run begins.
- `agent_end {messages, willRetry}`: one low-level run ends. pi may still
  auto-retry, compact and retry, or continue with a queued follow-up.
- `agent_settled {}`: the full session-level run has settled. Nothing further
  happens on its own. Use this, not `agent_end`, for "the agent is idle now".
- `message_update {assistantMessageEvent: {type: "text_delta", contentIndex, delta}}`
  streams one token delta per event. `message_end` closes one message.
- `extension_error {extensionPath, event, error}` is on the RPC event stream.

## RPC commands used by the Go adapter (Source B, "Commands")

- Start: `pi --mode rpc [--provider <name>] [--model <pattern>] [--session-dir <path>] [--name <name>] [--no-session]`.
- Framing (Source B, "Framing"): strict JSONL with LF (`\n`) as the only record
  delimiter. Quoted: "Node `readline` is not protocol-compliant for RPC mode
  because it also splits on `U+2028` and `U+2029`, which are valid inside JSON
  strings." Go's `bufio.Scanner` with `ScanLines` splits on LF and CRLF only.
- `{"id"?, "type":"prompt","message":str,"images"?}` answers
  `{"type":"response","command":"prompt","success":bool}`. Quoted: "If the
  agent is streaming and no `streamingBehavior` is specified, the command
  returns an error." Valid values: `"steer"` (delivered after the current
  turn's tool calls, before the next LLM call) or `"followUp"` (delivered once
  the agent is idle).
- `{"type":"steer","message":str}` answers `{"type":"response","command":"steer","success":bool}`.
- `{"type":"get_state"}` answers `data.sessionId` (a short display id),
  `data.sessionFile` (the session's JSONL path), `data.sessionName`,
  `data.isStreaming`, and more.
- `{"type":"switch_session","sessionPath":str}` answers `data.cancelled: bool`.
  It resumes by file path. `sessionPath` is `get_state`'s `sessionFile`, never
  the bare `sessionId`. The short id makes the resume fail.

## Extension UI protocol (Source B, "Extension UI Requests")

- Request shape: `{"type":"extension_ui_request","id":str,"method":"select"|"confirm"|"input"|"editor"|"notify"|"setStatus"|"setWidget"|"setTitle"|"set_editor_text", ...}`.
- Only the dialog methods (`"select"`, `"confirm"`, `"input"`, `"editor"`)
  expect an `extension_ui_response`. The rest (`notify`, `setStatus`,
  `setWidget`, `setTitle`, `set_editor_text`) are fire-and-forget.
- `select`, `input` and `editor` carry a `title` field. `confirm` carries both
  `title` and `message`. A dialog may carry `timeout` in milliseconds; pi
  auto-resolves with a default value when the client does not answer in
  time, and a response after that point is stale. The Go adapter expires
  its pending dialog at that instant and sets the ask deadline from it.
  Note that no field is literally named `prompt` in any of them. A summary built from
  `title || message` covers every dialog method.
- Response shape: `{"type":"extension_ui_response","id":str, value|confirmed|cancelled:true}`.
  `select`, `input` and `editor` take `value`. `confirm` takes `confirmed`.

## Error handling and mode behavior (Source A)

- "Extension errors are logged, agent continues" applies to handlers other
  than `tool_call`.
- `ctx.mode` is `"rpc"` and `ctx.hasUI` is `true` in RPC mode. Dialog and
  notify methods work over the wire with no terminal.

## Extension discovery and reload (Source A, "Placement")

- Auto-discovered paths include `~/.pi/agent/extensions/*/index.ts` (global,
  subdirectory) and `.pi/extensions/*/index.ts` (project-local).
- Quoted: "Extensions in auto-discovered locations can be hot-reloaded with
  `/reload`." That is a manual slash command in an interactive session.
  `--mode rpc` has no slash commands. Restart the pi process there.
