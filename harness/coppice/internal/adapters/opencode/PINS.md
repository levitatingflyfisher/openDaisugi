# OpenCode API pins

Captured on 2026-09-23 against `opencode` `1.18.32`, the binary at
`~/.local/bin/opencode`, and `@opencode-ai/plugin` `1.18.32`. The server ran
once, by hand, on a loopback port, with HOME and every XDG home under a
scratch directory. Each fact below names its source with a lead-in:

- Live: seen on the running server or in its SSE stream.
- From /doc: read from the server's live OpenAPI document.
- From the types: read from the plugin package's `dist/index.d.ts`.
- From the bundle: read from the server's JavaScript, which `strings` prints
  from the binary.

To check these pins after an upgrade, run
`uv run --no-sync python scripts/opencode_discover.py --scratch DIR`.
It prints each fact this file does not name. It never sends a prompt.

## Server: `opencode serve`

- Start: `opencode serve --hostname 127.0.0.1 --port <N>`.
- From the bundle: the serve command prints its listen line with
  `console.log`, so the line goes to stdout:
  `opencode server listening on http://127.0.0.1:<port>`. The port in the
  line is the port the server bound, read back from its TCP address.
- From the bundle: with `--port 0` the server tries port 4096 first. When
  4096 is taken, it binds a free port.
- Live: HTTP Basic auth. `OPENCODE_SERVER_PASSWORD` sets the password.
  `OPENCODE_SERVER_USERNAME` sets the user name, default `opencode`. A
  request with no auth or wrong auth gets `401` with
  `WWW-Authenticate: Basic realm="Secure Area"`.
- Live: `GET /global/health` answers `{"healthy": true, "version": "1.18.32"}`.
  This is the readiness check, and the one place the real version shows.
- Live: `GET /doc` is the OpenAPI 3.1 document, about 470 KB. Its
  `info.version` is `1.0.0`, not the OpenCode version.

## Session and message

- Live: `POST /session`, body `{"title"?: string, ...}`, every field
  optional. Answers `200` with a `Session`. Only `id` matters here, a `ses_`
  string.
- Live: `GET /session/{id}` answers `200` for a session that exists and
  `404` for one that does not. The adapter checks a resume id with it.
- From /doc: `POST /session/{id}/message`, body `{"parts": [...]}`, answers
  `200` with the finished assistant message, so it holds the request open
  for the whole turn. The adapter does not use it.
- Live: `POST /session/{id}/prompt_async`, the same body, answers `204` at
  once. The adapter sends every prompt this way. One text part is
  `{"type": "text", "text": "..."}`. With no `model` the session's default
  model runs.
- From /doc: `POST /permission/{requestID}/reply`, body
  `{"reply": "once" | "always" | "reject", "message"?: string}`, answers
  `200`. This answers OpenCode's own permission prompt.
- From /doc: `POST /question/{requestID}/reply`, body `{"answers": [[...]]}`,
  one list of labels per question in order, answers a pending `question`
  tool call. `POST /question/{requestID}/reject` answers it with no answer.
- From the bundle: `parsePatch` trims the patch, unwraps one heredoc, finds
  the first `*** Begin Patch` and `*** End Patch` lines, and reads the
  `*** Add File:`, `*** Update File:`, `*** Move to:` and `*** Delete File:`
  lines between them. The gate reads every such line in that range.

## Events: `GET /event`

Live: server-sent events. Each event is one `data: {json}` line and a blank
line. There is no `event:` field. The type is in the JSON. The first event
is `server.connected`. Each event is `{"id", "type", "properties"}`.

The types the adapter reads, and their `properties`, from /doc unless the
line says Live:

- `session.status`: `{sessionID, status: {type: "idle" | "retry" | "busy"}}`.
- `session.idle`: `{sessionID}`.
- `session.error`: `{sessionID?, error?}`. `sessionID` is optional.
- `permission.asked`: `{id, sessionID, permission, patterns: [...],
  metadata, always: [...], tool?: {messageID, callID}}`. `id` starts `per`.
- `permission.replied`: `{sessionID, requestID, reply}`.
- `question.asked`: `{id, sessionID, questions: [...], tool?}`. `id` starts
  `que`. `question.replied` and `question.rejected` carry `{sessionID,
  requestID}`.
- `session.created`: `{sessionID, info}`. `info.parentID` names the parent
  of a session that a task tool call starts.
- Live: `message.part.updated` is `{sessionID, part, time}`. A text part is
  `{"type": "text", "text", "time": {"start", "end"?}}`. The server sends it
  once with empty text, streams `message.part.delta` events, then sends it
  again with the full text and `time.end` set. The echo of the user's own
  prompt is a text part with no `time` at all.
- A tool part is `{"type": "tool", "callID", "tool", "state": {"status":
  "pending" | "running" | "completed" | "error", "input", ...}}`.
- Live: many other types arrive too, such as `session.updated`,
  `message.updated`, `session.diff`, `plugin.added` and `catalog.updated`.

## Built-in tool ids

Live, `GET /experimental/tool/ids` answered:

`invalid`, `question`, `bash`, `read`, `glob`, `grep`, `edit`, `write`,
`task`, `webfetch`, `todowrite`, `websearch`, `skill`, `apply_patch`

From the bundle: it also defines `lsp` and `plan_exit`. MCP tools arrive
under their own ids.

From the bundle, the tool arguments:

- `bash`: `{command, timeout?, workdir?, description?}`. `workdir` is the
  directory the command runs in.
- `read`: `{filePath, offset?, limit?}`.
- `write`: `{filePath, content}`. A relative `filePath` joins the session
  directory.
- `edit`: `{filePath, oldString, newString, replaceAll?}`.
- `glob`: `{pattern, path?}`. `grep`: `{pattern, path?, include?}`.
- `webfetch`: `{url, format, timeout?}`. `websearch`: `{query, ...}`.
- `apply_patch`: `{patchText}`. The patch names each file it touches.

## Plugin surface

From the types, the `@opencode-ai/plugin` `Hooks` interface keys:

`dispose`, `event`, `config`, `tool`, `auth`, `provider`, `chat.message`,
`chat.params`, `chat.headers`, `permission.ask`, `command.execute.before`,
`tool.execute.before`, `shell.env`, `tool.execute.after`,
`experimental.chat.messages.transform`, `experimental.chat.system.transform`,
`experimental.provider.small_model`, `experimental.session.compacting`,
`experimental.compaction.autocontinue`, `experimental.text.complete`,
`tool.definition`

- `Plugin = (input: PluginInput, options?) => Promise<Hooks>`.
  `PluginInput` carries `client`, `project`, `directory`, `worktree`,
  `serverUrl` and `$`.
- `"tool.execute.before"?: (input: {tool, sessionID, callID}, output:
  {args}) => Promise<void>`. Only `output.args` carries the arguments.
- `"permission.ask"?: (input: Permission, output: {status: "ask" | "deny" |
  "allow"}) => Promise<void>`. `Permission` is `{id, type, pattern?,
  sessionID, messageID, callID?, title, metadata, time: {created}}`, from
  `@opencode-ai/sdk`. Upstream reports that this hook does not fire: the
  permission pipeline publishes a `permission.asked` bus event and does not
  call the hook. See github.com/anomalyco/opencode issues #9229 and #7006.
  The generic `event` hook sees `permission.asked`.
- `event?: (input: {event}) => Promise<void>`. From the bundle: the server
  calls it for each bus event and does not await it.

From the bundle, how the server loads and calls a plugin:

- The config loader walks each config directory: the global one,
  `$XDG_CONFIG_HOME/opencode` or `~/.config/opencode`, each project
  `.opencode` directory, and `OPENCODE_CONFIG_DIR` when set. For each one it
  calls `ConfigPlugin.load(dir)`, which globs `{plugin,plugins}/*.{ts,js}`,
  and adds each file to the config's `plugin_origins`. It also adds each
  `plugin` entry of `opencode.json`. An entry that starts `file://`, `./` or
  `../` is a local path. Any other entry is an npm package the server
  installs.
- The `Plugin.state` service loads every entry of `plugin_origins` and
  pushes each plugin's hooks onto the list that `trigger` walks.
- For a module whose default export is not an object with `server`, the
  server calls every distinct exported value as a plugin function. An
  export that is not a function makes the whole file fail to load. A load
  error is logged and the server runs on without that plugin. So the gate
  plugin exports one value only: its default plugin function.
- A second loader, the `config-plugin` plugin, globs the same files and
  reads a default export of the form `{id, effect}` or `{id, setup}`. It
  drops any other file with no error. A plain default function is loaded by
  `Plugin.state` and dropped here, so it loads once.
- `trigger("tool.execute.before", ...)` awaits each plugin's hook in turn
  and does not catch what it throws. The throw fails the tool call before
  the tool runs, and the model sees the error text.
- The server fires `tool.execute.before` for every built-in tool, every MCP
  tool, the MCP resource tools, the `task` subagent call and code-mode child
  tools.
- `--pure` sets `OPENCODE_PURE=1`, and `OPENCODE_PURE` makes `Plugin.state`
  load no entry of `plugin_origins`. OpenCode then runs with no gate. The
  adapter refuses `--pure` in a pane's argv and removes `OPENCODE_PURE` from
  the server's environment.
