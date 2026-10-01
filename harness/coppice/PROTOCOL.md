# The coppice socket protocol

Version: 1

This page is the wire contract between a coppice server and any client. The
Go server in `internal/server` implements it. The corpus in
`testdata/protocol` proves it, one request and one expected reply per case.
A client in any language that replays that corpus and passes speaks this
protocol. `server.status` reports the same version number under `protocol`.

## Transport and auth

- The server listens on one unix socket. The default path comes from
  `coppice server status`.
- The socket file has mode `0600` and its directory has mode `0700`.
- On every accepted connection the server reads the peer uid. A peer whose
  uid is not the server's own uid, or whose uid cannot be read, gets one
  `unauthorized` error line and the connection closes. No request from that
  peer is read.
- `coppice --stdio` speaks the same protocol on stdin and stdout. It proxies
  to the running server. It is not a second server.
- There is no token, no password and no TLS on the socket. The uid check is
  the whole of the auth. The socket serves one user on one machine.

## Roles

A connection is the operator, a pane or a plugin. A pane may propose. A
plugin holds only the verbs its manifest names. Only the operator may
allow an ask. A foreman pane may deny an ask it holds, and no other.

- On Linux the server reads the peer pid from the socket and places it.
  The peer is a pane when a process on its parent chain, itself included,
  is a pane's child process or the server with the peer below it. It is
  also a pane when its session is a pty pane's session, or the server's own
  session when the server leads that session. The server itself is the
  operator: the phone server runs inside it.
- On Linux a peer the server cannot place may not run the allow verbs. It
  may run every other verb.
- A client may send `hello` with `role` `pane` and a `pane` id. That makes
  the connection a pane on any system. A later `hello` that says operator
  changes nothing. On Linux a `hello` that says operator from a pane process
  changes nothing either. On a system with no peer pid, the `hello` alone
  decides.
- The `coppice` CLI sends `hello` as a pane first on every connection when
  `COPPICE_PANE` is set.
- The web server places its own clients. For a client on this box it finds
  the client's socket in `/proc/net/tcp` and `/proc/net/tcp6`, finds the
  processes that hold it through `/proc/<pid>/fd`, and names them in
  `hello` as `peer_pids`, or sends `peer_unknown` when it finds none. The
  server places each named pid the way it places a socket peer. A named pid
  or `peer_unknown` can only take rights away, never give them. The web
  server sends a hello first on the upstream of every websocket. For a
  local client it carries these keys, and the web server sends them before
  it writes an answer for a local client of `/api/ask/answer`. A client on
  another host, such as the phone over a tailnet, is not placed. Its hello
  carries only the name its token gives. See Names.
- A relay on this box in front of the web server hides every client behind
  itself: `tailscale serve`, caddy or another reverse proxy, or an
  `ssh -L` tunnel. The web server then finds the relay's process, which is
  no pane, so every client through it reads as the operator, a pane's
  client included. It is the same case as a process a daemon starts for
  the pane.
- A pane connection gets `unauthorized` with the message
  `a pane can propose. It cannot allow.` for `agent.allow`, `agent.deny`,
  and `pane.report_state` with `source` `operator`. It gets the same
  refusal for `pane.send_keys`, `pane.send_text`, `pane.run` and
  `agent.prompt` to a pane whose state is `blocked`, since a key there
  answers the harness's own question. A prompt to a pane under the Ready
  prompts rules gets that section's refusal instead, which names the pane. A pane connection gets the refusal
  for `pane.report_state` and `pane.report_child` about any pane but its
  own, and about every pane when the server cannot name its pane.
- One exception: `agent.deny` from a pane connection passes when the
  server names the pane, the connection is no plugin, the kernel placed
  it, and that pane is the foreman that holds that exact ask of that exact
  pane now. See Holds. `agent.allow` from a pane is always refused.
- A pane, a plugin, or a peer the server cannot place gets `unauthorized`
  with `only the operator names a foreman.` for `task.set_foreman`, and
  with `only the operator talks to the floor's foreman.` for `floor.talk`.
  It gets `only the operator reads the machines.` for `floor.machines`.
  It gets `only the operator sets an agent's daisugi mode.` for
  `agent.set_mode`. It gets `only the operator sets an agent's model.` for
  `agent.set_model` and for an `agent.prompt` that carries `model`, and
  `only the operator sets an agent's router.` for `agent.set_router` and
  for a `pane.resume` that carries `router`. It gets `only the operator
  moves an agent's conversation.` for `agent.move`.
  It gets `only the operator names a foreman.` for `floor.foreman`, and
  `only the operator names a pane foreman.` for a pane label `foreman`,
  and for a `pane.resume` of a record labelled `foreman`.
  See The floor's foreman.
- A pane, a plugin, or a peer the server cannot place gets `unauthorized`
  with `a pane may rename only itself. The operator renames any pane.`
  for a `pane.rename` of any pane but its own. People find their agents
  by label, so one agent may not relabel another.
- A pane, or a peer the server cannot place, gets the pane refusal for
  `task.move`, since a move can put a task under a foreman the operator did
  not choose.
- The chat and the transcripts hold the owner's whole working life. A
  pane, a plugin, or a peer the server cannot place gets `unauthorized`
  with `only the operator talks to the floor's foreman.` for `floor.chat`.
  It gets `a pane may read only its own messages. The operator reads any
  pane's.` for `pane.messages`, except a pane's own connection reading its
  own pane: a connection whose role is pane with that pane's id, which is
  no plugin and no peer the kernel could not place, the same rule as for a
  `transcript_path`. A connection whose `hello` said `name_from` `token`
  and gave a name, that is a named web token, gets `unauthorized` with
  `the chat is for the owner's own sign-in.` for both verbs. The owner's
  own web token carries no name and reads both.
- The kernel places a peer by its process tree and session, and a pane's
  pid counts only while it is that pane's own process: a pty pane whose
  process ended drops out at once, and a pid the kernel gave to a new
  process does not count, told by its start time (field 22 of
  `/proc/<pid>/stat`, kept when the pane spawned). A process that leaves
  its pane's session and tree (`setsid -f`, `systemd-run`, a daemon) is
  seen as the operator and could read the chat. The gate's rule denies
  the chat verbs on the wire from a gated agent, as it denies
  `agent.allow`, as typed and with JSON string escapes decoded, with the
  reason `the chat and an agent's messages are the operator's to read.`;
  a command built at run time passes it. A cgroup per pane
  is the follow-up that closes this.
- A pane, a plugin or a peer the server cannot place gets `unauthorized`
  with `that sprig session belongs to another pane. Only the operator
  resumes or forks it.` for a `pane.create` of a sprig pane whose resume
  names a session tree another pane's record names (live or ended,
  compared by real path), the flags read as sprig's own parser reads them:
  `-name` or `--name`, `=value` or the next word, the last value winning,
  up to `--`; the adapter strips every such spelling and passes its own, and for a `pane.fork` of another pane's
  sprig pane. The new pane would read that tree as its own messages.
- Each pane names the root pid of every process the server starts for it:
  the pty child, the headless harness, and each turn of a harness that
  runs one process per turn. A descendant of any of them is that pane.
- Every other verb a pane connection runs, except `hello`,
  `pane.report_state`, `pane.report_child`, `events.subscribe`,
  `events.pause`, `events.resume`, `floor.note` and `floor.notes`, is
  recorded as a `note` before it runs.
- The kernel check stops a process that stays in the pane's process tree or
  in its session. A process that a daemon starts for the pane is outside
  both, and the server sees the operator. One shell word does it:
  `setsid -f`, `systemd-run --user`, `ssh localhost`, a tmux or screen
  server, or `at`. The gate's rule is the second door. It sees only the
  literal words of a tool call, so a command built at run time passes it.

### Names

A connection can carry a name. A name is a label for a person, never a
right. It does not change the role, and it does not change who may allow.

- `hello` with `name` names an operator connection. The name rule is
  `names are one word, up to 32 characters`. A name holds only ASCII
  letters, digits, `-` and `_`. Any other name gets `bad_request` with that
  rule as the message.
- A name sticks. A later `hello` may repeat it. A different name gets
  `bad_request` with `this connection is <name>.`
- A pane connection is `pane:<id>`, and a plugin connection is
  `plugin:<id>`, whatever name its hello gave. The server reads this from
  the role each time, so a named connection that becomes a pane is that
  pane.
- `name_from` is `token` or `socket`, default `socket`. The phone server
  sends `hello` with `name_from` `token` first on the upstream of every
  websocket, with the name its token carries, or no name for the
  operator's own token. After that hello, the connection takes no name
  from a later `hello`: one that tries gets `bad_request` with
  `this connection takes its name from its token.` So a browser cannot
  name itself. The phone server forwards no browser line until the server
  accepts that hello. A refusal, no reply in 5 seconds, or a closed
  upstream closes the websocket.
- A socket name is a label any operator process on this box can set. A
  token name is bound to the token it was minted for. On the socket, the
  server counts a name as a token name only when the peer is the server
  process itself, where the phone server runs. A standalone
  `coppice web serve` is another process, so the names on its websockets
  count as socket names. Its `/api/ask/answer` writes the answer file
  itself, after it checked the token, so an answer from its page still
  says `token`.
- The socket serves one uid. A connection from another uid is refused
  before it can send `hello`.
- The `hello` reply carries `name` when the connection has one.
- `agent.allow` and `agent.deny` write the connection's name into the
  answer file as `by`, and how the server knows it as `whoFrom`: `token`,
  `socket`, or `pane` for a foreman. A pane name is as weak as a socket
  name when the pane was named in a `hello`: an operator process can say
  `role` `pane` with any pane id. Such a claim only takes rights away. A
  pane the server could not name by id writes `pane` alone. The gate journals `allowed_by` or
  `denied_by` with `who_from`. A connection with no name writes `local`
  from `none`, so the journal never holds an empty name. The phone's
  `/api/ask/answer` writes the name its token carries, from `token`.
- `by` and `whoFrom` in the answer file are what its writer says. Any
  process of this uid can write that file, the same boundary the nonce
  has.
- The web server checks a token when a websocket opens and on each `/api`
  request. A websocket that is already open stays open after its token is
  revoked or rotated, until it drops. A reload then gets the refusal.
- `coppice web token --for NAME` mints a token for NAME. `coppice web token
  list` prints the names that hold one. `coppice web token --revoke NAME`
  retires one.

### Plugins

A plugin policy is a process the server starts. `plugins/README.md` says
how to write one.

- The server starts each enabled policy in a session of its own, with
  `COPPICE_SOCKET`, `COPPICE_SOCK`, `COPPICE_PLUGIN` and
  `COPPICE_PLUGIN_CONFIG` in its environment. It records the policy's pid
  before the policy can dial.
- On Linux a peer is that plugin when a process on its parent chain,
  itself included, is the policy, or when its session is the policy's
  session. The walk checks for a policy before it reaches the server, so a
  policy never reads as a pane.
- After a policy exits, its pid stays a plugin until no process is left in
  its session. A process that left the policy's group but stayed in the
  session is still that plugin. The runner kills the whole session when a
  policy exits. When the server dies without a stop, the kernel kills each
  policy, but a process left in its session is not killed. The next server
  has no record of that session and places such a process as the operator.
- A plugin connection runs only the verbs in its manifest's `needs`. Any
  other verb gets `unauthorized` with the message
  `plugin <id> did not ask for <verb> in its manifest`. `hello` is always
  free. `events.subscribe` is free for the kinds in `listens`, and any
  other kind gets `plugin <id> does not listen to <kind> in its manifest`.
  `events.pause` and `events.resume` are free when `listens` is not empty.
- A plugin never runs `agent.allow` or `agent.deny`. Those get
  `a plugin can propose. It cannot allow.` A manifest that asks for either
  does not load. A plugin never runs `pane.report_state` or
  `pane.report_child`. It gets the pane refusal for typed keys to a
  `blocked` pane, the same as a pane.
- A `floor.note` from a plugin leads with the plugin id and names no pane.
  Every verb a plugin runs that changes something, such as `pane.close`,
  is recorded as a `note`. The read verbs are not.
- A plugin the server no longer runs holds no verb at all.
- `hello` with `role` `plugin` and `plugin` set to an enabled id names the
  plugin. From a policy the server started it must name that same plugin.
  From any other connection it narrows that connection to the plugin's
  verbs. It never widens a connection, and a pane may not send it. A
  process an operator starts by hand, outside the server, is an operator
  connection until it sends this hello.

## Framing

Both directions carry one JSON value per line, terminated by `\n`. A line is
at most 1 MiB. A longer line ends the connection.

A connection speaks one of two framings. It starts native. The first line
that carries `"jsonrpc": "2.0"` switches the connection to JSON-RPC 2.0 for
the rest of its life. There is no way back. A native request whose reply is
still pending at the moment of the switch is answered in native form, the
form it was sent in. A JSON array whose members carry `"jsonrpc": "2.0"`
switches the connection too, and then gets the JSON-RPC batch refusal.

### Native

A request is one object with a string `id`, a string `cmd`, and the command
parameters as further top-level keys:

```
{"id":"1","cmd":"pane.read","pane":"w1:p1","source":"recent"}
```

A reply echoes the id. The requests on one connection run at the same time,
so their replies may come in any order, and a client matches each reply to
its request by the id:

```
{"id":"1","ok":true,"result":{"text":"...","source":"recent"}}
{"id":"1","ok":false,"error":{"code":"no_such_pane","message":"no pane \"w9\". Run: coppice pane list"}}
```

An event has no `id`. It carries `event` and its own fields:

```
{"event":"state","v":1,"ts":1757300000.5,"pane":"w1:p1","state":"working",...}
```

A line that is not JSON gets a `bad_request` reply with an empty id. A `cmd`
the server does not know gets a `bad_request` reply that lists every known
command. A JSON array gets a `bad_request` reply. Nothing runs without a
reply.

### JSON-RPC 2.0

A request is one object with `"jsonrpc": "2.0"`, an `id`, a `method` and an
optional `params` object:

```
{"jsonrpc":"2.0","id":7,"method":"pane.read","params":{"pane":"w1:p1","source":"recent"}}
```

- `method` is the native `cmd`. The names map one to one.
- `params` holds the native top-level parameters. It must be an object or
  absent. An array is refused.
- `id` must be a string or a number. It comes back with the same type.

A reply:

```
{"jsonrpc":"2.0","id":7,"result":{"text":"...","source":"recent"}}
{"jsonrpc":"2.0","id":7,"error":{"code":-32000,"message":"no pane \"w9\". Run: coppice pane list","data":{"code":"no_such_pane"}}}
```

An event is a notification. `method` is the native `event` name and `params`
carries every other field:

```
{"jsonrpc":"2.0","method":"state","params":{"v":1,"ts":1757300000.5,"pane":"w1:p1","state":"working",...}}
```

Rules on a JSON-RPC connection:

| line | reply |
|---|---|
| a request with no `id`, or `"id": null` | error `-32600`, id `null`, message `coppice does not accept notifications, send an id` |
| a JSON array | error `-32600`, id `null`. coppice does not accept batches |
| a line that is not JSON | error `-32700`, id `null` |
| a `method` the server does not know | error `-32601`, `data.code` is `bad_request`, the message lists every known command |
| an object without `"jsonrpc": "2.0"` | error `-32600`. After the switch, every line must be JSON-RPC |
| any native error | error `-32000`, `data.code` is the native code, `message` is the native message |

Nothing runs without a reply.

## Ids

- A request id is chosen by the client. The server echoes it and never reads
  it for meaning.
- A pane id, a tab id and a workspace id are opaque strings such as `w1:p3`.
  They are stable for the life of the server's data directory. A restart
  keeps them. A pane id is never reused, even when the pane failed to start.

## Errors

The native error code is a closed enum. A client switches on it. A code
outside this list is a server bug. The server refuses to emit one and
reports `internal` instead, with the invented code in the message.

| code | meaning |
|---|---|
| `bad_request` | the request is missing a field, or a field has a value the server does not accept |
| `no_such_pane` | no pane has that id |
| `no_such_workspace` | no workspace has that id, or no workspace exists yet |
| `no_such_tab` | no tab has that id |
| `pane_closed` | the pane ended on its own, or a restart found nothing behind it, and the record is still in Recent (`ended: true`); it cannot be written or read this way. An operator `pane.close` removes the record instead, so a stale id after that is `no_such_pane`, not this |
| `server_closed` | the server is shutting down and will not start a new pane |
| `not_attached` | this connection is not attached to that pane |
| `adapter_error` | a headless adapter refused or failed the call |
| `spawn_failed` | the pane's process could not start |
| `timeout` | the wait ended before the condition was met, or the client went away |
| `unauthorized` | the peer uid is not the server's uid |
| `internal` | the server failed in a way it cannot name better |

Every message is a plain sentence a person can act on. Many end with the
command to run next.

## Verbs

`params` names the fields a request carries. `result` names the fields a
success reply carries. A field marked optional may be absent from a reply.
Every verb can answer `bad_request` for a malformed request. Verbs that take
`pane` answer `no_such_pane` for an id nothing knows any more - whether it
never existed, or an operator `pane.close`, a `pane.forget`, a successful
`pane.resume`, or the seven-day sweep removed it - and `pane_closed` for a
pane that is still on record as ended, unless the row says otherwise.

### Server

| verb | params | result |
|---|---|---|
| `server.status` | none | `protocol` number, `pid`, `socket`, `data_dir`, `uptime_s`, `panes` count, `panes_live` count, `resumable` count of ended records `pane.resume` would actually resume rather than start fresh, `restart_note` text, `restore` object with `panes`, `already_closed`, `resumed`, `marked_done`, `marked_unknown`, `resumable` (how many of the records that restart marked ended are still in Recent and could still resume; it falls as they are resumed or forgotten, and it is the only count the restart line reads), `notes` list, `detection_warnings` list |
| `server.stop` | none | `stopping` true. The reply is sent before the server stops. The coppice binary registers this verb |

### Workspaces and tabs

| verb | params | result | errors |
|---|---|---|---|
| `workspace.create` | `cwd`, `label` | `workspace` id, `tab` id of its first tab | `internal` |
| `workspace.list` | none | `workspaces` list of `id`, `label`, `cwd`, `tabs` count | |
| `tab.create` | `workspace` id, default the current one; `label` | `tab` id, `workspace` id | `no_such_workspace` |
| `tab.list` | `workspace` id, default the current one | `tabs` list of `id`, `label`, `panes` count | `no_such_workspace` |

### Panes

| verb | params | result | errors |
|---|---|---|---|
| `pane.create` | `cwd`, required unless `task` supplies it or the one-click case below fills it in; `kind` is `pty` or `headless`, default `pty`; `cmd_argv` list, required for `pty` unless `harness` names a `[harness.<name>]` table in the config file, in which case the server builds argv from that table's `command` and `args` and then the request's optional `args` list; `harness` name, required for `headless`; `near` a pane id, used only to fill in `cwd` below; `workspace` and `tab` ids, default the current ones; `cols` default 120; `rows` default 40; `env` object of strings; `label`, see below; `task` id of the task the pane works for; `fence` is `on` or `off`, the pane's own setting for `daisugi fence` (see The fence below), and anything else is `bad_request`. With `task`, `cwd` may be absent: a task with a worktree runs the pane there, and a different `cwd` is `bad_request`; a task without one lends its own `cwd` when the request has none. **A label the request left out**, whenever `harness` and `cwd` both end up known - explicit or defaulted, `pty` or `headless` - becomes `<harness>-<the cwd's base name>`, or `-2`, `-3` and on when a live pane already has that exact label; an ended pane's label never forces the count up. This is not only the one-click case: `coppice new`, `coppice open`/`coppice claude`, and the TUI's own Enter-with-no-selection all send an explicit `harness` and `cwd` and no `label`, and all get this same naming. **One click, a new agent**: a `pty` request with neither `cwd` nor `cmd_argv` fills both in on top of that. `cwd` becomes, in order, `near`'s own cwd; the directories a pane most recently started in, newest first, the whole list walked for the first that still exists on disk - a directory just used wins here even when it is also pinned, and a missing newest entry falls through to the next recent one, never straight to a pinned project; the pinned projects, in file order, the first that still exists; or this server's own start directory. A `harness` this same request also left out becomes `coppice.toml`'s `default` line | `pane` id, `workspace` id, `tab` id, `cwd` the directory the pane started in, `label` its label, and for the one-click case `why`: `near`, `recent`, `pinned` or `start`, the step that chose `cwd`. A client says where and why on its status line | `no_such_workspace`, `no_such_tab`, `spawn_failed`, `server_closed`. An unknown harness is `bad_request`. An unknown task is `bad_request`: `no task <id>. Run: coppice task list`. An unknown `near` is `no_such_pane`. A `pty` pane with `harness` and no `cmd_argv` whose name has no table in the config file is `bad_request`: `no harness named "<name>" in <path>`. The one-click case with no `default` line in the config file is `bad_request`: `no default harness in <path>` |
| `pane.split` | same as `pane.create` | same as `pane.create` | same as `pane.create` |
| `pane.list` | `scope` task id, optional; `ended` bool, default false | `path` list of labels from the root, `floor`, down to the scope; `panes` list of rows. With no `ended`, only live records list. With `ended: true`, only ended records list, newest first (by `ended_at`, then by id), each carrying `ended_at` (unix seconds) and `exit_code` (`null` when unknown) - see Ended panes. With `scope`, only the panes of that task and its descendants are listed, live or ended, whichever `ended` asked for. Each row has `id`, `label`, `cwd`, `cmd` list, `kind`, `harness`, `workspace`, `tab`, `closed`, `cols`, `rows` (the size a live pane runs at now, see Fit), `state`, `source`, `detail`; optional `quiet_for` seconds, `ts`, `session_id`, `ask`, `held` while a foreman holds the ask, see Holds, `parent_pane`, `task`, `children` list of the pane's subagents, each with `id`, `label`, `state` and `ts`, oldest first. A done subagent leaves the list 60 seconds after it stopped, and an ended pane has none. `looking` lists the names of the people attached to the pane now, sorted, see `presence`; `stack`, `gate` and `tokens`, see Agent facts; `fenced` true on a live fenced pane and `workspace_changed`, see The fence | `bad_request` when `scope` names no task: `no task <id>. Run: coppice task list` |
| `session.list` | same as `pane.list` | same as `pane.list` | |
| `pane.send_text` | `pane`; `text` required, `""` sends a bare Enter; `enter` default true | `sent` byte count. A prompt that waits, see Ready prompts: `sent` 0, `queued` its place from 1, `note` `queued until <label> is ready` | `internal`. `bad_request` for a prompt to a pane that waits on a question, and past the queue cap, see Ready prompts |
| `pane.trust` | `pane`; `trust` default true | `pane` id, `trusted` bool | Answers Claude Code's folder trust screen. `trust` true moves the cursor to `Yes, I trust this folder` and presses Enter, each key its own write. `trust` false presses Esc, which ends Claude. `bad_request` `<label> does not ask to trust a folder now.` when the pane's screen shows no trust screen. Only the operator runs it: a pane or a plugin gets `unauthorized` `only the operator trusts a folder.` The server never writes Claude's own config to trust a folder |
| `pane.ack_changed` | `cwd` absolute directory | `cwd` resolved, `acked` list of the files the hold named | Ends the hold a fenced run's change to a watched file put on new starts in `cwd`; see The fence. `bad_request` `pane.ack_changed takes cwd, an absolute directory.` Only the operator runs it: a pane or a plugin gets `unauthorized` `only the operator acks a changed workspace.` |
| `pane.send_keys` | `pane`; `keys` non-empty list of key names or single characters. `testdata/keys.json` lists the names | `sent` key count | `internal` |
| `pane.run` | `pane`; `line` required, sent with a trailing Enter | `sent` byte count. On a harness pane under Ready prompts, the line is a prompt: a line that waits answers `sent` 0, `queued`, `note` | `internal`. On a harness pane under Ready prompts, `bad_request` when the pane waits on a question, even for an empty line, and past the queue cap |
| `pane.read` | `pane`; `source` is `visible`, `recent` or `detection`, default `visible` | `text`, `source` | A pane whose process exited but was not closed still reads: `pane.close`, `pane.forget`, a successful `pane.resume`, or the seven-day sweep are what free the screen |
| `pane.resize` | `pane`; `cols` and `rows`, both positive | `cols`, `rows` | `internal` |
| `pane.close` | `pane`, or `session` | `pane` id, `closed` true | `no_such_pane` only. Removes the record at once, live or already ended - it never lands in Recent. Closing an id already removed is `no_such_pane`, not a repeatable success any more |
| `session.stop` | same as `pane.close` | same as `pane.close` | same as `pane.close` |
| `pane.wait_output` | `pane`; `contains` text or `state` name, at least one; `timeout_ms` default 30000 | `matched` is `text`, or `matched` is `state` with `state` and `closed` | `timeout`, `no_such_pane`, `pane_closed` |
| `pane.fork` | `pane`; `label`, default the parent's label plus ` fork` | `pane` id of the new pane, `workspace`, `tab`, `parent_pane` | `adapter_error` when the pane is a pty pane, when its harness cannot fork, or when the harness has not reported a session id yet. `spawn_failed`, `server_closed`. The new pane is headless, in the parent's workspace and tab, with the parent's harness, cwd, env, argv and size. It starts by resuming the parent's session as a new session. Until its harness reports a session id of its own, it cannot be forked again, and a server restart repeats the fork from the parent's session. Its `pane.list` row carries `parent_pane` |
| `pane.rename` | `pane`; `label`, required and non-empty once cleaned | `pane` id, `label` (cleaned) | `bad_request` when `label` is missing, empty, or nothing but whitespace and control characters. `no_such_pane` for an unknown `pane`. `unauthorized` when a pane renames a pane that is not its own. Works on a live pane and on an ended record alike |
| `pane.explain` | `pane` | `pane`, `harness`, `detection_text`, `osc_title`, `osc_progress`, `detection_warnings`; optional `effective_state`, `effective_source`, `received_age_s`, `tick`, `note`, `agent`, `source_file`, `matched`, `rule_id`, `priority`, `region`, `manifest_state`, `skip_state_update`, `evaluated` | `internal`. The coppice binary registers this verb |
| `pane.forget` | `pane` id of one ended record, or `ended: true` for every ended record, not both | with `pane`: `pane` id, `forgot` true. With `ended: true`: `forgot` count | `bad_request` when neither `pane` nor `ended: true` is given, when both are, or when `pane` names a live pane: `pane <id> is still live. Close it first`. `no_such_pane` for an unknown `pane` |
| `pane.resume` | `pane` id of one ended record; `cols` and `rows`, optional, the new pane's size, default the ended record's own; `router`, optional, `direct` or `gateway`, sets the base URL in the new pane's env (see `agent.set_router`) | `pane` id of the new live pane, `workspace`, `tab`, `resumed` bool, and `router` when one was given | `bad_request` when `cols` or `rows` is given and not positive, when `router` is not `direct` or `gateway` or no base URL is built for the record's harness, when `pane` names a live pane, or when a `pane.resume` for the same id is already in flight: `pane <id> is already resuming.`. `no_such_pane` for an unknown `pane`, including one another concurrent `pane.resume` for the same id just finished replacing. `spawn_failed`, `server_closed`. See Ended panes for the resume rule |

Every pane label is cleaned on the server before it is stored, by
`pane.create`, `pane.split`, `pane.fork`, `pane.rename` and `pane.resume`
alike: control characters and Unicode format characters (zero-width and
bidi marks) are dropped, the label is cut to 64 runes, and spaces around
it are trimmed. Every screen draws labels, so none carries an escape code
or poses as another label. A pane, a plugin or a peer the server cannot
place gets `unauthorized` with `another live pane has that label. Only
the operator gives two panes one name.` for a `pane.create`,
`pane.split` or `pane.rename` to the exact label of another live pane,
and the foreman refusal for any final label that reads as `foreman`,
whether the request named it or the server made it. A `harness` name in
`pane.create` must be at most 32 letters, digits, `.`, `_` or `-`,
starting with a letter or digit, or the request is `bad_request`. A task label from `task.create` is cleaned the
same way as a pane label.

### The fence

A pane's harness can start under `daisugi fence`: a Landlock ruleset the
kernel enforces on the harness and every process it starts. It reads and
writes the workspace (the pane's `cwd`), the parts of the harness's own
directory the harness writes, and a private temp dir; it reads the system and
the harness's config, connects only to this server's socket and the gate's,
and reaches nothing else under HOME. The gate's state is never writable. The
network is not limited.

Whether a pane is fenced: the pane's own `fence` (`on` or `off`) first, then
`fence` in `coppice.toml` (`"auto"`, `"on"` or `"off"`). The default is
`off`: the fence is opt-in (FN-R-33). Any other value or TOML shape (a bare
word, a boolean, a number) is a config error: the server does not start,
and it prints `cannot read <path>: fence must be "on", "off" or "auto", not
<value>.` A file that does not parse while it sets fence is refused the
same way, with the parse error's first line (FN-R-37).
Under `auto`, a pane is fenced while the gate is on: a gate root it may use (the
one beside the data dir, or the gate of a data dir openDaisugi may pick under
the user's home) holds `envelopes/` and no `DISARMED` marker. Only a missing
`envelopes/` reads as off; one that cannot be read reads as on. A pane with no
harness name, or the `shell` harness, is the operator's own and is fenced only
when its own `fence` is `on`. The pane's `fence` is saved in `layout.json`
(`"fence"`, omitted when empty). The saved `argv` never holds the fence: the
server builds the wrapped command at each start, for a `pty` pane, a headless
adapter, a resume, a fork and a restore alike.

A pane, a plugin or a peer the server cannot place may not set `fence` to
`off` on `pane.create` or `pane.split`: `unauthorized`, `only the operator
turns a pane's fence off.` When such a caller may be fenced (a fenced pane, a
pane the server cannot name, a plugin, an unplaced peer), whatever it
creates, splits, forks or resumes is saved with `fence` `on`, a shell or a
custom `cmd_argv` included. Such a caller may use `pane.send_keys`,
`pane.send_text`, `pane.run` and `agent.prompt` only on a fenced pane:
`unauthorized`, `a fenced pane may type only into a fenced pane.` For a fenced
pane the server drops `HOME`, `TMPDIR`, the XDG variables, `OPENDAISUGI_HOME`
and every `DAISUGI_FENCE*` from the request's `env`, so its own values stand.

The wrapper is `daisugi fence [--harness NAME] --workspace CWD --gate-root
ROOT --tmpdir <data dir>/fence-tmp/<pane>-<16 hex> --coppice-socket SOCK
[--workdir DIR] --`, with `daisugi` from `COPPICE_DAISUGI_BIN` or PATH. The
pane id's `:` and `/` read `_` in the temp dir's name, and each start gets a
fresh random suffix, so no run finds an earlier run's files; the server
removes it when the pane ends. A sprig pane's `--session-dir` is a `--workdir`, checked like the
workspace. A fenced `claude` pane also gets `--settings` with a JSON
document: the gate hook (`DAISUGI_GATE_HOOK=opendaisugi.gate '<daisugi>' gate
check --root '<root>' --format claude --verify-timeout 10 --ask --ask-timeout
90 || exit 2`, matcher `*`, timeout 105, `onFailure` `block`),
`disableAllHooks` false and a `ConfigChange` hook `exit 2`; after the program
on a `pty` pane, first on a headless one. A fenced pane whose `daisugi` is
missing fails to start (`spawn_failed`). One whose kernel has no usable
Landlock starts, prints why and how to turn the fence off, and ends with exit
code 1. The harness never runs bare by accident.

When a fenced pane starts and ends, the server hashes the files in its `cwd`
that run code later outside any fence: `.mcp.json`, `.envrc`, `.gitmodules`,
`opencode.json`, `opencode.jsonc`, the `.claude/`, `.codex/` and `.opencode/`
trees, and `.git/config` and `.git/hooks/*`. When `.git` is a file (a
worktree, a submodule) it hashes that file, the `config` and `hooks/*` of the
git dir it names, and those of that dir's `commondir`. A link is hashed as
its target text; past 4096 entries the list ends in `(more files than the
watch reads)`. A file added, removed or changed is listed on the pane's
record (`"workspace_changed"` in `layout.json`, omitted when empty), on its
`pane.list` row as `workspace_changed`, which the rail shows as `workspace
config changed: <files>`, and in a note: `workspace config changed: a fenced
run changed <files> in <cwd>. Check them, then run: coppice pane ack-changed
<cwd>`. A live fenced pane's row carries `fenced` true.

The change also holds the workspace, in `<data dir>/workspace-holds.json`
keyed by the resolved `cwd`, so it outlives the pane's record and a restart.
A hold covers its workspace and every dir under it, by path parts (`/ws`
holds `/ws/sub`, not `/wsx`). Each start check first hashes again every live
fenced pane whose `cwd` is the start dir or above it, and holds a change since
its last look at once, while that pane still runs; a pane's end holds only
what no start saw. While it holds, every new start there or under it
(`pane.create`, `pane.split`, `pane.fork`, `pane.resume`, a restore) fails
with `workspace config changed: a fenced run changed <files> in <held>. Check
them, then run: coppice pane ack-changed <held>`, where `<held>` is the held
workspace. A pane that runs there goes on. `pane.ack_changed` with
`cwd` (an absolute directory) ends the hold and answers `cwd` (resolved) and
`acked` (the files it held on, `[]` when none). Only the operator runs it: a
pane or a plugin gets `unauthorized` `only the operator acks a changed
workspace.`

The foreman's sprig and claude turns and the compactor run under the same
fence when the gate is on or `fence` is `"on"`: the workspace is the turn's
directory, the work dir is `--allow`ed read and write, and a key pipe is
`--keep-fd 3`. A worker's `daisugi tree run` runs bare, since it writes the
worker's gate root (`<work>/gate`); its sprig starts under the fence instead.
The server passes the prefix in `DAISUGI_SPRIG_FENCE`, a JSON list: `daisugi
fence --workspace <worker dir> --coppice-socket SOCK --gate-root <work>/gate`.
The tree run adds `--allow <work>/sprig-sessions:rw`, a `--keep-fd` for each
key pipe and `--`. So the gate root is no write grant of the worker's, the
fence starts its resident gate, and sprig keeps its sessions in
`<work>/sprig-sessions`; the worker's record carries `"fenced": true` and its
`session_file` is there. A spawn tool under the fence fails the node. The
server also puts `--sprig-fence` before the closing `-` of the tree run, so a
`daisugi` that cannot apply the fence refuses the run (an unknown option, or
the variable missing) and the worker does not start.

### Ready prompts

A text prompt reaches an agent only when its input box is ready. A
prompt is `pane.send_text` with non-empty `text` and `enter` true,
`agent.prompt` to a `pty` pane, or `pane.run`. `pane.run` promises a
line with Enter, not a line now, so on such a pane it takes the same
queue. An empty `pane.run` line is still an Enter, so it is refused on a
question too. The rules apply to a `pty` pane whose
harness has a manifest with at least one `idle` rule, while the manifest
tick runs. A manifest with no `idle` rule could never say the input box
is ready.

- The server reads the pane's screen against its manifest at once.
- When the screen shows a question, such as Claude's folder trust screen
  or a permission ask, or another source says the pane is `blocked`, the
  server refuses with `bad_request`
  `<label> is waiting on a question on its screen. Answer it first: open it.`
  and sends nothing. On Claude's trust screen the fix names the command:
  `<label> is waiting on a question on its screen. Answer it first: coppice
  pane trust <pane>, or open it.` This refusal is the same for the operator and for a pane
  connection. For 5 s after `pane.trust` trusted the folder, the trust
  screen may still show while Claude draws its next screen. A prompt then
  waits in the queue and is not refused.
- `pane.trust` takes one answer at a time for a pane. A second call while
  one runs, or within 5 s of the last answer, is `bad_request` `<label>
  has an answer to its trust screen already. Wait a moment and look at it
  again.` It is also `bad_request` when the screen shows no cursor mark
  on its options: the server never guesses which line is chosen. A drop
  of the pane's queued prompts does not end that 5 s; only the pane's
  end does.
- When the screen reads `idle`, no prompt waits for the pane, and the
  screen still reads `idle` at each look for 300 ms, the server sends the
  prompt now.
- Otherwise the pane is starting or working, and the prompt waits in the
  pane's queue, in order. The reply says `queued until <label> is ready`.
  The queue sends its oldest prompt once the screen has read `idle` for
  300 ms, and the next one after the pane takes it. A prompt that waits
  while the pane shows a question stays in the queue until the owner
  answers the question. While the server cannot read the pane's screen
  against its manifest, the queue holds and sends nothing.
- One prompt may be 64 KiB at most. Past it the prompt is `bad_request`
  `the prompt is <n> bytes. A prompt may be 65536 bytes at most.`, whether it
  would go now or wait. A queue holds 16 prompts and 64 KiB at most. Past either cap a
  prompt is `bad_request` `<label> has <n> prompts, <b> bytes, waiting
  already. A queue holds 16 prompts and 65536 bytes. Wait until it is
  ready, then send again.`
- The queue drops what it holds when the pane ends, and when the pane
  stays 10 minutes in one state other than `working`. The 10 minutes
  start again each time the state changes and each time a prompt goes,
  and time spent `working` does not count. A drop posts one note that
  names the pane by its label, even after `pane.close`, and says how
  many prompts were not sent. Each sender hears of it too: a sender that
  still waits on the reply, as `agent.prompt` with `wait`, gets the drop
  as its error and nothing more; one whose wait ended before the drop is
  told as any other sender; a
  pane that sent a prompt gets the note with `to` set to it, and the same
  line, starting `coppice: `, typed into its own prompt through its own
  queue; a client connection still open gets the `note` event.
- Prompts queued when the server stops are dropped with no note.
- A prompt goes as its text, then, 150 ms later, an Enter as its own
  write. A harness such as Claude Code reads a burst that ends in CR as a
  paste and keeps the CR in its input box. When the input box still holds
  the start of the text 800 ms after the Enter, the server sends one more
  Enter. It never sends more than one.
- Raw text (`enter` false), a bare Enter (`text` `""`) and
  `pane.send_keys` are the owner's own keys. They are never queued or
  refused by these rules.
- A pane whose harness has no manifest or no `idle` rule, a pane with no
  harness, and a `headless` pane get a prompt at once, as before.

### Ended panes

A live pane's process is running, or its adapter has not ended its session.
Once that stops being true, one of two things happens:

- **The operator asked for it**, through `pane.close` (or `task.close`,
  which closes its panes the same way). The record is removed from the
  tree at once. It never appears anywhere again, under any `pane.list` or
  `agent.list` call, and a later verb against that id is `no_such_pane`.
- **It ended on its own** - the process exited, a headless adapter ended
  its own session, or a server restart found no process behind the
  record. The record stays, marked `closed`, with `ended_at` (unix
  seconds) and `exit_code` (when known). `pane.list` and `agent.list`
  leave it out of their default reply and list it only with `ended: true`,
  newest first. Its grid stays readable through `pane.read` and
  `agent.read` until something removes the record. `state` reads `done`,
  `source` `process`, even for a record a restart found already closed and
  so never itself observed ending: the record's own `closed`, `exit_code`
  and `ended_at` are what `done` is built from then, with `ts` set to
  `ended_at` (the pane's real end time), not invented from nothing and not
  stamped with the moment the row happens to be read.

Three things remove an ended record: `pane.forget`, a successful
`pane.resume`, and the seven-day sweep, which runs once when the server
starts (on whatever a restart just marked ended, and whatever was already
old enough) and once an hour after, dropping any ended record whose
`ended_at` is more than seven days old.

`pane.resume` starts a new pane - a new id, never the old one - with the
ended record's label, cwd, env, harness and task, then removes the ended
record, but only once the new pane has actually started:

- A **headless** pane with a recorded harness session id resumes that
  session, the same way a server restart does. A pane that was itself a
  pending fork (see `pane.fork`) carries that forward, so the new pane
  repeats the fork rather than resuming the borrowed session directly.
- A **pty** pane resumes when its harness has a `resume_args` entry in
  `coppice.toml` (`[harness.<name>]`, alongside `command` and `args`) and
  the record has a harness session id. The new pane's command line is
  `command`, then `args`, then `resume_args` with every `{session}`
  replaced by that id, then the pane's own extra args from `pane.create`'s
  `args` list (a task prompt, say) - `resume_args = ["--resume",
  "{session}"]`, for example.
- **Otherwise**, the new pane starts fresh from the ended record's own
  argv, and the reply says `resumed: false`.

A headless pane's harness session id comes from its adapter's own
`SessionID()` report, recorded the first time it differs from the id the
pane started with (`internal/server/panes.go`'s `pumpAdapter`). Nothing in
this build writes one for a pty pane; the resume_args mechanism above is
ready for the day something does. `server.status`'s `resumable` count is
honest about this: it counts only an ended record `pane.resume` would
actually resume, not merely one that ended.

If a new pane fails to start, the ended record it was trying to replace is
left exactly as it was, so a caller can fix whatever was wrong and try
again without losing it.

A `pane.resume` for an id another `pane.resume` is still in flight for is
refused, `bad_request "pane <id> is already resuming."`, rather than each
starting its own new pane from the same ended record. A later call for the
same id, once the first has finished, either finds the same refusal (still
in flight) or `no_such_pane` (already resumed and removed) - never a
second live pane from one ended record.

### Projects

A project is a directory the operator works in. `coppice.toml`'s `projects`
list pins some; the server also remembers, in its data directory, the ten
directories a pane most recently started in.

| verb | params | result | errors |
|---|---|---|---|
| `project.remove` | `path` of a pinned or recent project | `removed` the path taken out. The path leaves the pinned list in `coppice.toml` and the recent list. The path matches a list entry as sent, cleaned, or with its links resolved, so a hand-edited `/src/x/` pin can go. `bad_request` when the path is in neither, when `path` is missing, or when it is not absolute. Rewrites `coppice.toml` and drops its comments. Operator only: a pane or a plugin gets `unauthorized` ("only the operator edits the project list."), and a named web token gets `unauthorized` ("the project list is for the owner's own sign-in.") | |
| `project.list` | none | `projects` list of `path`, `name` (the path's base name), `pinned` bool, `missing` bool (true when the directory is gone; a client draws such a row dim and offers `project.remove`). Every pinned project first, in `coppice.toml` order, then every recently started-in directory not already pinned, newest first. `default` is the harness name `coppice.toml`'s `default` line gives, or `""` when it gives none: a client that starts an agent in a chosen project sends this as `harness`, since only a create with no `cwd` fills the harness in | |

`pane.create`'s one-click case (above) reads its `cwd` default from the raw
recent-directories list, not this row's own de-duplicated "recent" half:
being pinned does not cost a directory its place as the most recently used
one.

### Voice

The server starts `daisugi voice serve` itself, once the socket listens,
when `daisugi` is on PATH and `daisugi voice serve --help` exits 0. It
binds a loopback port the server picks, checks a bearer token file the
server writes at `<data dir>/voice/token` (mode 0600), and writes its output
to `<data dir>/voice/voice.log`. The server stops it when it stops. When it
dies after it answered `/health`, the server starts it once more; when it
dies again, voice is down until `voice.start`. `coppice.toml` changes this:

```toml
[voice]
enabled = false          # start no voice server
url = "http://127.0.0.1:7477"   # use this one instead; the server starts none
token_file = "/path/to/token"   # the token that server checks; default the web token file
args = ["--data-dir", "/some/dir"]  # added after the flags the server passes
```

Voice is `off`, with the reason in `voice.status`, when `coppice.toml` does
not read, when `url` names a machine other than this one and there is no
`token_file` (the web token never leaves this machine), and when `args` sets
`--host`, `--listen`, `--port` or `--token-file`, which the server sets
itself.

| verb | params | result | errors |
|---|---|---|---|
| `voice.status` | none | `state`: `idle` (not started), `starting`, `ready`, `down`, or `off`. `ready` bool. `url`: the voice server's base URL while starting or ready, else `""`. `token_file`: the file whose token goes in `Authorization: Bearer` to that URL. `managed`: false when `url` came from `[voice] url`. `reason`: one sentence that says why voice cannot run now, `""` when ready. `fix`: what the owner does about it, a clause with no full stop, for a client to end with its own way to try again. `command`: a shell line that fixes it, for a client to type and not run, or `""`; today only `pip install 'opendaisugi[voice]'` or its `--upgrade` form, and only for a Python daisugi (a Go or Rust daisugi, found with `daisugi voice status --json`, gets a fix with no `command`, because pip would install a second daisugi). `fetch`: `{done, total}` in bytes, only while `state` is `starting` and the voice process is getting its speech model; the server asks `daisugi voice status --json` about once a second to fill it. `pid` while a managed process runs. It never waits on the voice process; for `[voice] url` it asks that server's `/health` for at most one second | |
| `voice.start` | none | as `voice.status`. Starts voice when it is idle or down, with its one restart back, and does nothing when it is starting, ready or off. It returns at once, usually with `state` `starting` | |

A client posts audio to `<url>/transcribe` with the token from
`token_file`, as `daisugi voice serve` documents. The web server's
`/api/voice/transcribe` does this for the page, and the floor's talk key
does it for the TUI.

### Speech out

The foreman can answer aloud. `[voice] speak_url` in `coppice.toml` names a
speech server with an OpenAI-style `POST /v1/audio/speech`; `speak_model` and
`speak_voice` are sent to it (defaults `kokoro` and `af_heart`, what
Kokoro-FastAPI expects), and `speak_profile` names an operator agent profile
whose key goes to it as `Authorization: Bearer`. The key is read from
`daisugi agent-profiles bundle --fd 3` for each request and is in no reply,
no log and no file. The address takes no user name, password, query or
fragment, and must be `http` or `https` with a host. An address that ends
with `/audio/speech` is used as it is, one that ends with `/v1` gets
`/audio/speech`, and any other gets `/v1/audio/speech`. The port, when there
is one, is digits. The request uses no proxy, and a redirect is a failure;
it is not followed. The key goes only over `https`, or over plain `http` to
`localhost`, a loopback address, `100.64.0.0/10`, `fd7a:115c:a1e0::/48` or a
name that ends in `.ts.net`; to any other host `speech.say` is refused
(`adapter_error`) before anything is sent. The 2 MiB limit counts the bytes
of the body after any chunking is removed.

| verb | params | result | errors |
|---|---|---|---|
| `speech.status` | none | `mode`: `server` when a valid `speak_url` is set, else `browser`. For `server`: `url` (the scheme, host and path of the endpoint, nothing else), `model`, `voice`, and `profile` when `speak_profile` is set. For `browser` with an address that is not valid: `line`, one sentence with the fix | `unauthorized` from a pane, a plugin, a peer the server cannot place, or a named web token (as `floor.chat` refuses one): `only the operator reads aloud.` |
| `speech.say` | `text`: one sentence, at most 2000 bytes | `audio`: the audio, base64; `bytes`: its length; `type`: its media type (`audio/mpeg` when the server names none that looks like audio). The server asks for `mp3` | `bad_request`: no text, text over 2000 bytes, or no valid `speak_url`. `timeout`: the speech server took longer than 10 s for the whole exchange. `adapter_error`: it did not answer, answered a status other than 2xx, stopped sending, sent more than 2 MiB, or sent no audio; the profile is not in the agent profiles, or the profiles cannot be read. Each message names the host and what to fix. `unauthorized` as for `speech.status` |

The web server (`coppice web serve`, and the one the coppice server starts)
turns a `speech.say` answer into a binary websocket frame, and only on the
socket whose request it answers. A page that never sends `speech.say` never
gets one. The frame is:

- 4 bytes: the big-endian length N of the header;
- N bytes: the header, `{"event":"speech","id":<the request id, a string>,"type":"<media type>"}`;
- the rest: the audio bytes.

A reply is changed this way only when it answers a `speech.say` request that
the same socket sent (the web server keeps those ids, and no other verb can
make it send a frame), it is `ok`, its `id` is a string, and
its `result` holds `audio` that decodes from base64 to exactly `bytes` bytes.
Every other line, a `speech.say` failure included, goes to the page as the
text line it is. The 4 MiB limit on a reply line holds, so one sentence is at
most 2 MiB of audio. The page asks for one sentence at a time, in order,
and plays them with WebAudio. When the speech server fails, the page speaks
the rest of that reply with the browser's voice and shows the message.

`floor.machines` lists a `speak_url` on a machine as `speak`, a probe like
`voice`.

### Tasks

A task is the unit of work above the pane: a label, a parent or none, a
directory or a git worktree, and a model name or none. A task with children
is a team. Ids are `t1`, `t2`, and so on. A task's `state` is the worst
state among its panes and its children, in the order `blocked`, `working`,
`idle`, `unknown`, `done`. A task with nothing under it has state `""`.

| verb | params | result | errors |
|---|---|---|---|
| `task.create` | `label` required; `parent` task id; `cwd`; `worktree` bool, default false; `model` | `task` id, `worktree` path or `""` | `bad_request` when `label` is missing, when `parent` names no task, when `worktree` is true and `cwd` is missing, when the repo is on no branch, when `cwd` is not inside a git repo: `worktree needs a git repo. <cwd> is not inside one.`, or when `label` is not a name git can use: `task names use lowercase letters, digits, and dashes`. With `worktree` true the server runs `git worktree add -b <label> <repo>-worktrees/<label>` beside the repo that holds `cwd`, on a branch whose upstream is the branch the repo was on. A failed add stores nothing |
| `task.list` | `scope` task id, optional | `path` list of labels from the root, `floor`, down to the scope; `tasks` list of rows. With `scope`, only that task and its descendants are listed. Each row has `id`, `label`, `parent`, `cwd`, `worktree`, `model`, `foreman` pane id or `""`, `state`, `panes` list of pane ids; `ahead` commit count when the task has a worktree and git can count | `bad_request` when `scope` names no task |
| `task.close` | `task` required; `keep_worktree` bool, default false | `task` id, `closed` true, `panes` list of the pane ids it closed | `bad_request` when `task` names no task. With `keep_worktree` false every worktree in the task and its descendants is checked first; one with uncommitted changes refuses the whole call before anything closes: `task <id> has uncommitted changes in <path>. Commit them, or pass keep_worktree: true.` Then the panes close, then the worktrees are removed, then the records go. A removal git refuses after the panes closed is `internal`, names the path, and ends with the same sentence; the records stay, and a second call with `keep_worktree` true finishes the close. Branches are never deleted. With `keep_worktree` true the worktrees stay on disk |
| `task.move` | `task` required; `parent` task id, `""` or absent detaches | `task` id, `parent` | `bad_request` when either id names no task, or when `parent` is the task itself or one of its descendants |
| `task.set_foreman` | `task` required; `pane` id, `""` or absent clears | `task` id, `foreman` pane id or `""` | `unauthorized` from any connection but the operator. `bad_request` when `task` names no task or `pane` is closed. `no_such_pane` when `pane` names no pane |

#### Holds

A task's `foreman` is a pane that hears the asks of the task's panes, and
of every descendant task's panes, before the operator does. The nearest
foreman up the chain of tasks wins.

- When a pane's merged state becomes `blocked` with an ask, the server
  looks for the nearest foreman. It holds the ask only when the ask's tier
  is `undoable`, and the foreman pane is open, is not blocked, and is not
  the asking pane. Otherwise the ask goes to the operator at once.
- The server writes a `note` from the asking pane, with `to` the foreman,
  that names the pane, the ask id, the summary, and the deny command. It
  writes the note for a permanent ask too, with no hold.
- Each hold has a short id the server makes, `h1`, `h2` and so on, carried
  as `held.hold`. `agent.deny` and `agent.allow` accept it in `ask` for the
  pane that holds it, in place of the ask id.
- While the foreman is idle, the server types one line into it for each
  hold: the hold id, the pane id, the seconds left, `coppice agent get
  <pane>` and `coppice agent deny <pane> <hold>`. It names the ask id too,
  only when the id is 1 to 64 letters, digits, `_` or `-`. It reads the
  foreman's state again just before the send. The server types only into
  a foreman whose idle ends a turn: an idle from `gate` or `headless`. An
  idle from `process` only says the screen went quiet, and one from
  `manifest` only says the prompt box shows, which it also does with a
  half-typed line in it. A foreman with no turn-ending source gets the note
  and never typed bytes. A foreman that is working or blocked gets no
  bytes. Input with `pane.send_text`, `pane.send_keys`, `pane.run` or
  `agent.prompt` marks the foreman, and a marked foreman gets no bytes.
  The mark clears only when the last input ended with Enter, a carriage
  return or a line feed, and a turn ended after it: a `working` state
  followed by a `gate` or `headless` idle. Input that does not end with
  Enter sets the mark again and keeps it, whatever turns end, since it
  still sits in the input line. The server never clears that text. The server's own line
  counts as input too, so a foreman gets at most one line per turn, and a
  second hold waits for the next turn to end. The
  line never carries the ask's summary, which the asking pane wrote. The
  write runs apart from the event that started it, and a hold counts as
  told only once a write succeeds. The server types on its own authority,
  so the typing rule for panes does not apply.
- A hold lasts 120 seconds, and ends 30 seconds before the ask's own
  deadline when that comes first. A deadline 30 seconds away or less gives
  no hold. While it lasts, the `state` event and the `pane.list` row carry
  `held`: `hold` the hold id, `by` the foreman pane, `foreman_label`, `task` the task that
  named the foreman, `task_label`, `ask`, `since` and `until`. The plan
  named this mark `held_by`. It is `held.by`, so the facts about one hold
  travel together.
- The hold ends when the foreman or the operator answers the ask, when the
  pane's state is no longer that ask, when the pane closes, when the
  foreman closes, when the foreman blocks on an ask of its own, when
  `task.set_foreman` or `task.move` changes the pane's nearest foreman, or
  at `until`. When the ask still waits after the hold ends, the server
  sends the pane's `state` event again with no `held`, and the foreman gets
  a note. A floor moves the ask to its needs-you list then.
- An ask that went to the operator is never held again.
- A foreman pane that closes is cleared from every task that names it.

### Attach and events

| verb | params | result | errors |
|---|---|---|---|
| `pane.attach` | `pane`; `cols` and `rows`, both positive, resize the pane first; `view_only`, default false, makes the server ignore `cols`, `rows` and `fit`; `fit`, optional, an object with `cols` from 1 to 1000 and `rows` from 1 to 500: size the pane to this client only while no other client is attached (see Fit) | `pane` id, `attached` true; `fitted`, a bool, when the request had `fit` | `bad_request` when `fit` is not an object with whole `cols` from 1 to 1000 and `rows` from 1 to 500. Attaching also subscribes this connection to `frame` and `state` for that pane. With `view_only` true, `pane.send_text`, `pane.send_keys`, `pane.run`, `pane.resize` and `agent.prompt` on this connection answer `not_attached` |
| `pane.detach` | `pane` | `pane` id, `attached` false | `not_attached` |
| `events.subscribe` | `kinds` list of `state`, `layout`, `frame`, `note`, `child`, `presence`, `messages`, default `state` and `layout`; `panes` is `"*"` or a non-empty list of pane ids, default `"*"` | `kinds`, `panes` | Subscribing to `frame` sends nothing until `pane.attach` starts a frame pump for that pane |
| `events.pause` | `pane` | `pane` id, `paused` true | `not_attached`. Stops `frame` events for that pane on this connection only. Once the reply is sent, no frame for the pane follows until `events.resume` |
| `events.resume` | `pane` | `pane` id, `paused` false | `not_attached`. The next frame is a full one. Deltas follow it |

### Fit

`pane.attach` with `fit` sizes the pane to the client's screen, for a phone that
would otherwise scroll sideways through a wide terminal. The server resizes the
pane to `fit` only when no other client is attached to it, a view included, and
the reply says `fitted` true. Otherwise it changes nothing and `fitted` is
false. A fit changes only the running terminal. The pane's record, and so
`layout.json`, keeps the owner's size, so a crash or a restart never resumes
an agent at the phone's size, and `pane.list` reports the size the pane runs
at now. The server puts the owner's size back when the fitted client detaches
or its connection ends, and when any other client attaches. A `pane.resize`
from any client ends the fit: its size wins and is not undone. The server then
refuses to fit the client whose fit the resize ended: its next `pane.attach`
with `fit` answers `fitted` false and changes nothing, until the client sends
`pane.detach` (or its connection ends) and attaches fresh. The presence event
the resize sends would otherwise make a phone refit at once and undo the
resize. A client that
sends `pane.detach` right after `pane.attach` must wait for the attach reply.
The server runs the two at the same time, so a detach that arrives first may
or may not find the pane attached. A
second `pane.attach` with `fit` from the fitted client moves the size and
keeps the first old size. A `view_only` attach never fits.

### Floor

| verb | params | result | errors |
|---|---|---|---|
| `hello` | `role` is `operator`, `pane` or `plugin`, default `operator`; `pane` id; `plugin` id, needed with role `plugin`; `peer_pids` list of pids a web server serves on this connection; `peer_unknown` true when a web server could not find its local client; `name`, a name for the connection; `name_from`, `token` or `socket` | `role`, the role the connection has now; `allow`, whether it may run `agent.allow` and `agent.deny`; optional `pane`; optional `name`; for a plugin, `plugin` and `needs` | `bad_request` for any other role, for role `plugin` with no `plugin` or with an id that is not enabled, for a hello that names another plugin than the one the connection is, for `peer_pids` that is not a list of numbers, for a name that breaks the name rule, for a name that changes the connection's name, or for any other `name_from`. `unauthorized` for role `plugin` from a pane. See Roles and Names |
| `floor.note` | `text` required. `pane` id, used only on an operator connection | `text` as recorded: control runes and bytes that are not UTF-8 dropped, cut at 200 runes | `bad_request` for empty text. On a pane connection the note names that pane and its text leads with the pane's label and ` › ` |
| `floor.notes` | none | `notes` list of the last 200 notes, oldest first, each the `note` event object, with `to` when the note has an addressee | |
| `floor.foreman` | `harness` name, optional, default the `default` harness | `pane` the new foreman's id; `label`; `started` true. The foreman gets its page as with `floor.talk`. See The floor's foreman | `bad_request` while the foreman runs: `the foreman <label> (<id>) runs already. Close it first, or talk to it.`, and for no default harness as with `floor.talk`. `unauthorized` from any connection but the operator |
| `floor.talk` | `text` required, one sentence for the floor's foreman; `act` optional, a client id for the sentence: 1 to 64 letters, digits, `.`, `:`, `-` or `_` (a UUID from a sender) | `pane` the foreman's id; `label`; `started` true when this talk started the foreman; `queued` how many sentences wait, this one included; optional `note`, one line, when the old foreman had ended or the foreman waits on a question. In chat mode, a repeat of a known `act` logs nothing and queues no turn: the reply has `message` (`log/N`, the last message that carries the `act`), `queued` (the turn's place, 0 when it has started), `label` and `already` true. The reply comes at once, before the words are typed. See The floor's foreman | `bad_request` for empty text, for a bad `act` (also one that is not a string), for a known `act` sent with other words than it first carried (`act <id> was used for other words. Send a new id for a new sentence.`), for a full queue, and when no foreman runs and `coppice.toml` names no `default` harness: `no default harness in <path>, so no foreman can start. Run coppice open HARNESS once to set one.` `unauthorized` from any connection but the operator. A refusal from `pane.create` when the foreman cannot start |
| `floor.chat` | `since`, optional, the `id` of the last message the client has; `limit`, optional, default 100; below 1 counts as 100 and above 500 as 500 | `messages` list, oldest first, each with `id`, `at`, `role`, `text`, optional `tool` and `pane`, and on each `owner` line `read` bool; `source` `transcript`; `more` bool. See The chat. In chat mode (`[foreman] mode = "chat"`): `past`, optional bool, shows imported `past` lines (default off); `id` is `log/<i>`; `role` can also be `work` or `past`; a tool use has `tool` set to its name and no result; `source` is `log` | `unauthorized` from any connection but the operator, and for a named web token. A chat file or a foreman's transcript that cannot be read is left out with one note, and `more` is true. `internal` only when a damaged file cannot be moved aside |
| `floor.facts` | none | `daisugi` object: `mode`, `armed`, and `enforcing`, `watching` and `off`, the number of live harness panes in each mode, and `installed`, true when the Claude Code or Codex settings in the home hold a daisugi gate hook, and optional `hook_root`, the gate root such a hook names with `--root` when it is not this server's gate root (then the gate reads its mode markers elsewhere and `agent.set_mode` does nothing; a hook with no `--root` is not checked); `working` and `needing_you`, the number of live panes whose state is `working` and `blocked`, where a blocked pane whose ask a foreman holds counts as working, as every floor counts it from `pane.list`; `tokens_today`, a `tokens` object; optional `gateway` object with `url` and optional `answers` bool; optional `foreman` object with `pane` and `label`, the floor's tracked foreman while it runs; optional `foreman_turn` object with `ran_on`, the name of the turn account the chat foreman's last turn ran on (`Claude` for Claude Code), once a turn has run (after a restart, from the saved state read when the chat opens), see the chat foreman's failover; optional `phone_cert_days`, the whole days, rounded down, left on the phone server's tailscale certificate (the one a running `coppice web serve --tls tailscale` names in `web/serving.json`, else, when web.json is enabled under `--tls tailscale`, its `cert_file` or the default pair; nothing for a phone server that neither runs nor is saved), present only under 7 days and negative once it has run out. See Agent facts | |
| `floor.machines` | none | `tailscale`, `ok`, `absent` (no `tailscale` on `PATH`) or `off` (it does not answer within 3 s, or its state is not `Running`); `machines` list, empty unless `ok`, this box first and then the peers by name, each with `name` (the MagicDNS short name), `online`, `os`, `self`, optional `last_seen` (as tailscale prints it, absent when tailscale has none), `profiles` list of `{name, address, answers, answers_ms, key_set}` and optional `voice` and `speak` `{address, answers, answers_ms}`; `line`, one plain sentence with the fix, when `tailscale` is not `ok`; `profiles_line` when the operator's agent profiles could not be read. See The machines view | `unauthorized` from a pane, a plugin, or a peer the server cannot place: `only the operator reads the machines.` |
| `floor.journal` | `pane`, optional, keeps one agent's verdicts; `since`, optional, a unix time in seconds, keeps verdicts made after it (a value that is not a number is `bad_request`); `limit`, optional, below 1 or above 500 counts as 500 | `entries` list of the last verdicts a gate or an agent's loop report told the server, newest first, 100 per agent and 500 in all (the oldest go first), each `{at, pane, decision, tool, clause}` with `mode` when the report named one. `at` is unix seconds on the server clock. The list stays across a restart. A pane gets only its own entries; a plugin and a peer the server cannot place get none, even if their hello names a pane; an operator or a view gets all | `bad_request` for a `since` that is not a number |

#### The floor's foreman

The floor's foreman is the pane the server started as the foreman. The
server tracks it by its pane id, which it keeps in `foreman.json` in the
data dir, so the id outlives a restart. The label `foreman` is only for
people to read: the server never finds the foreman by its label. A pane,
a plugin or a peer the server cannot place gets `unauthorized` with `only
the operator names a pane foreman.` for `pane.create`, `pane.split`,
`pane.fork` and `pane.rename` with the label `foreman`, in any case and
with spaces around it, and for `pane.resume` of a record labelled
`foreman`, since a resume keeps the label.

When the operator resumes the tracked foreman's ended record with
`pane.resume`, the new pane becomes the tracked foreman, so the next
`floor.talk` goes to it and no second foreman starts. A resumed session
already had its page. A pane that starts fresh gets the page again. A
record labelled `foreman` that is not the tracked foreman, because a talk
started a new one first, resumes labelled `foreman-old`, so two panes
never read as the foreman.

- With no live foreman, `floor.talk` starts one: `pane.create` with the
  `default` harness, `kind` `pty`, the label `foreman`, and `cwd` the
  foreman's own directory, made if missing: `$XDG_STATE_HOME/coppice/foreman`,
  else `~/.local/state/coppice/foreman`. The daisugi gate refuses every
  call made from inside the coppice config and data directories, so the
  foreman's directory is never inside them. It is scratch, not a project.
  It never joins the recent directories, and a one-click `pane.create`
  `near` the foreman does not start there. No config key names the
  foreman: `floor.facts` names the tracked one while it runs.
- `floor.foreman` starts the foreman the same way with a harness the
  request names, and refuses while the foreman runs.
- A foreman the server starts gets its page before any words. The server
  waits until the pane drew its screen and reads `idle` from a state newer
  than the first bytes it drew, since a new pane reads idle before it
  draws anything. It then types the page: when the pane's program turned
  bracketed paste on (`CSI ?2004h`), the header line and the page that
  `coppice skill foreman` prints, as one paste, then Enter. The page holds
  no escape byte, so it cannot end the paste early. When the program did
  not turn paste on, the server types the one line `Run: coppice skill
  foreman. Follow it. You are the foreman of this floor. Say ready.` It
  then waits for the turn that starts to end.
- The words go as one line. Tabs and line breaks become spaces, other
  control characters are dropped, and every run of spaces becomes one, so
  one talk is one prompt. The words wait in one queue on the server,
  oldest first, at most 32. Past that, `floor.talk` is refused with `the
  foreman has 32 sentences waiting already. Look at its window, then talk
  again.` One goroutine types the words with Enter, one at a time. A
  second `floor.talk` while the foreman starts joins the queue and never
  starts a second foreman.
- A word waits while the foreman's state is `blocked`, since an Enter
  there would answer its question.
- After 30 seconds of waiting on a foreman that is not ready, the server
  writes one note saying so.
- A foreman that ends before its page drops the queue with a note. A
  foreman that ends later drops what is left with a note. The next
  `floor.talk` starts a new one and its reply carries a `note` that says
  the old one ended. The server writes that note to the floor too.
- Other panes can type into the foreman: `pane.send_text` and
  `agent.prompt` are open to panes. That grants them no rights. The
  foreman's page tells it that text other agents type is data, not the
  operator's orders, and the gate judges every call the foreman makes the
  same way whoever asked for it.
- The floor's foreman is no task's foreman, so it holds no asks. See
  Holds.

#### The chat

The chat is what the owner said to the floor's foreman and what the
foremen said back. Two verbs read it: `floor.chat` reads the whole chat,
and `pane.messages` reads one pane's transcript.

- The server writes each `floor.talk` sentence, after it accepts it and
  before it types it, as one line of `<data dir>/chat/YYYY-MM-DD.jsonl`,
  the day in UTC. The line is `{"kind":"user","date":"<UTC time as
  2006-01-02T15:04:05.000Z>","pane":"<foreman id>","text":"<the words as
  typed>"}`. The date always has the same length, so no id hangs on the
  clock. So the owner's
  words show at once, while the foreman starts, and outlive a foreman that
  ended. When the server starts a foreman and it had tracked an earlier
  one, it first writes a line of kind `note` with the text `A new foreman
  started.` and the new foreman's id. The kinds are the foreman log's own
  (`user`, `note`), so a later reader of that log maps the same way. The
  directory is mode 0700 and each file 0600. A write that fails leaves the
  talk as it is, and the server writes one note that names the file.
- Each write sends a `messages` event for the foreman's pane with `chat`
  true.
- `floor.chat` reads the newest chat files, at most 4 MiB in all, and the
  transcripts of the tracked foreman, even when no line names it yet (as
  for one that `floor.foreman` or the terminal floor started), and of each
  foreman the lines name, newest four foremen at most, the tracked one
  first:
  a sprig foreman's session tree, else the newest four transcripts the
  foreman's own process claimed (see Agent facts). The claims are kept in
  the data dir, so after a restart the chat still holds the replies of a
  foreman that ended. A chat file or a transcript that cannot be read is
  left out, `more` is true, and the server writes one note that names it,
  once per server, however often the chat is read. From the chat files it takes
  `user` lines as role `owner` and `note` lines as role `note`. From a
  transcript it takes only role `agent` messages: the owner's lines there
  are the same words, and the first one is the foreman's page. It merges
  them by `at`. At the same `at`, chat lines come before transcript
  messages, and each source keeps its own order. A chat line's `id` is
  `chat/<day>/<byte offset of its line>`. A transcript message's `id` is
  `<pane>/<n>/<its pane.messages id>`, where `n` numbers the foreman's
  claimed transcripts, oldest 0. Each message carries `pane`, the
  foreman's id.
- Each `owner` line carries `read`. It is true when that foreman's
  transcripts record the same words as an owner message (each run of white
  space read as one space) whose `at` is at or after the line's `at`: the
  foreman's turn for that sentence has started. The chat lines of each
  foreman are matched oldest first, and each recorded message reads one
  line at most, in order. So a sentence typed while an earlier turn runs,
  which the harness has not taken yet, stays `false`, and an older message
  with the same words never reads a new line. A line whose foreman's
  transcript is not read is `false`. Other panes can type into the
  foreman, so an agent that types the owner's exact words while the
  owner's line waits can make that line read early; it cannot read the
  chat to learn them. A line that never reached the foreman can take the
  mark of a later line with the same words.
- A chat file with a complete line that is not a JSON object is damaged.
  The server moves it to `<name>.broken-<UTC time as 20060102T150405Z>`,
  mode 0600, never deletes it, and writes one note that names where it
  went. The chat starts fresh: the reply holds the other files, and the
  next talk makes a new file for that day. A last line with no line feed
  is half written and is left out. Before its first write to a file, the
  server moves such a line to `<name>.broken-<UTC time>` (0600), syncs
  that copy and its directory to disk, and only then cuts the line from
  the file, with one note that names where it went: no byte is ever lost.
  A file that cannot be read for any other reason moves nowhere. A
  damaged file that cannot be moved aside stays where it is: the reply
  holds the rest, `more` is true, and one note names the file.
- `since`, `limit` and `more` work the same in both verbs. The reply holds
  the messages after the one whose `id` is `since`, or all the messages
  read when `since` is absent or names none of them. Of those it keeps the
  newest `limit`. `more` is true when messages older than the first one
  kept were left out: by `limit`, or, with no `since` found, because the
  read did not reach the start of a file.
- The desk draws the chat as the foreman's window, from `floor.chat` and
  the pane `floor.facts` names under `foreman`. Both floors, the web page
  and the terminal, read the newest messages, show the owner's lines as
  `you` and the replies as `foreman`, and send the line typed in that
  window with `floor.talk`, never as keys to the foreman's pane. A reply
  is a message with role `agent` and no `tool`. The floor counts the
  newest reply's `id` as seen once a window has drawn it. No verb or field
  is new for this.

### Agents

| verb | params | result | errors |
|---|---|---|---|
| `agent.allow` | `pane`; `ask` id of the gate's ask; `reason`; `confirm`, the pane name, needed for a permanent ask; `scope` `once` or `task`, default `once` | `pane`, `ask`, `decision` `allow` | `unauthorized` on a pane connection. `bad_request` when the pane never reported that ask in a blocked state, or its deadline has passed, or when no ask is pending under that id, or for an allow with any other `scope`. Then the permanent rule: `unauthorized` with `this cannot be undone. Type the pane name to allow: <name>` when the ask is permanent and `confirm` is not the pane name, and `unauthorized` for `scope` `task` on a permanent ask. The pane name is its label, or its id when it has no label. The ask is undoable only when both the tier the pane reported and the tier in the gate's ask file say `undoable`. A missing tier on either side is permanent. `scope` `task` rides in the answer file, and the gate records it as a proposed envelope edit that nothing applies. Any live ask of the pane may be answered, not only the one its state shows now. The server keeps its record of asks in memory only. After a server restart, an ask raised before it can be answered here only once the pane reports that ask again. `internal` when the answer cannot be written. The answer is the same file the phone writes in the gate directory. The phone's answer route holds the same rule: for an allow it reads the tier and the pane name from `agent.list`, and an ask no pane shows is gone |
| `agent.deny` | `pane`; `ask`; `reason` | `pane`, `ask`, `decision` `deny` | the role refusal and the checks that the ask exists, the same as `agent.allow`. A deny never needs `confirm`, and it ignores `scope`. The foreman that holds the ask may deny it from its pane; the reason is then `denied by the foreman <label>` |
| `pane.report_state` | `pane`; `event` is one PaneStateEvent object, see Events. The event may also carry the gate report fields `transcript_path`, `verdict` and `mode`, see Agent facts | `pane`, `source`, `state`, `detail`; optional `ask`, `note`; optional `transcript`, the refusal of a `transcript_path` the server does not take (see Agent facts) | A `source` of `operator` from a connection not attached to the pane becomes `gate`, and `note` says so. `bad_request` for a gate report field of the wrong type, a `transcript_path` that is not absolute, a `mode` other than `enforcing` or `watching`, or a `verdict` whose `decision` is not `allow`, `deny` or `ask` |
| `pane.report_child` | `pane`; `child` id of a subagent inside the pane's harness; `state` is `working` or `done`; `label`, cleaned and cut the same way as a note | `pane`, `child`, `state` | `bad_request` when `child` is missing or `state` is anything else. A subagent is read only: no verb acts on it. A report sends a `child` event. A pane connection may send it, and it makes no note |
| `agent.list` | none | `agents` list of rows. Each row has `pane`, `label`, `cwd`, `kind`, `harness`, `closed`, `state`, `source`, `detail`; optional `ts`, `session_id`, `ask`, `harness_session_id`, `parent_pane` | |
| `agent.get` | `pane` | one `agent.list` row | `no_such_pane` |
| `agent.prompt` | `pane`; `text` required; `model`, optional, a plain model name for a headless sprig pane: this turn and the ones after it run on it; `wait` default false; `until` state name, default `idle`; `timeout_ms` default 120000 | without `wait`: `pane`, `sent`. A prompt that waits, see Ready prompts, adds `queued` and `note` and has `sent` 0. With `wait`: `pane`, `state`, `source`, optional `ask`. A prompt that waits is sent first, within `timeout_ms`, and then the wait for `until` starts | `adapter_error`, `timeout`. `bad_request` for a prompt to a pty pane that waits on a question, and past the queue cap, see Ready prompts |
| `agent.wait` | `pane`; `until` default `idle`; `timeout_ms` default 120000 | `pane`, `state`, `source`, optional `ask` | `timeout` |
| `agent.set_mode` | `pane`; `mode` is `enforcing` or `watching` | `pane`, `mode` | Sets one agent's daisugi mode from its next tool call. `watching` writes the marker `<gate root>/agents/<floor>/<pane>.mode` (the text `watching` and a newline; file 0600, directories 0700, written to a new file and renamed into place). `<floor>` is the first 16 hex digits of the SHA-256 of the server's socket path, the `COPPICE_SOCK` its panes get: pane ids start at `w1:p1` on every floor, and floors whose data dirs share a parent share a gate root, so each floor keeps its own directory. `enforcing` removes it. coppice makes, writes and sweeps markers only in real directories: a symlink or a file where `agents/` or the floor's directory belongs gets `internal` `could not set the daisugi mode of <label>: <path> is not a directory: it is a symlink or a file. Move it aside`, and a start leaves such a directory alone and says so in a note. The gate reads the marker for the calling pane of the calling floor only (`COPPICE_PANE` and `COPPICE_SOCK`, or a resident request's `coppice_pane` and `coppice_sock`), and it can only lower an `enforce` hook to `audit`: it never turns the gate off and never changes an envelope. The marker goes when the agent ends or its record goes, and a server start removes every marker in its own floor's directory, so each agent of an earlier run starts enforcing; another floor's directory is left alone. `bad_request` `mode must be enforcing or watching. Turning the gate off stays a command: daisugi gate disarm` for any other mode, `off` included; `bad_request` `<label> runs no harness, so no gate checks it.`; `no_such_pane`; `pane_closed` for an ended agent; `not_attached` on a connection attached to the pane view only, before any other check; `internal` when the marker cannot be written. `unauthorized` from a pane, a plugin, or a peer the server cannot place: `only the operator sets an agent's daisugi mode.` |
| `agent.set_model` | `pane`; `model`, a plain name: letters, digits and `. _ : / - [ ]`, 1 to 64 characters, starting with a letter or digit | `pane`, `model`, `when`, and for a queued swap `queued` and `note` | Swaps one agent's model, and says when. On a `claude` pty pane it sends `/model NAME` through the ready queue (see Ready prompts): `when` is `now` when the pane was ready, or `queued` with the queue place and the note, and the line goes only at an idle prompt. A pane with no verdict from the screen reader (no manifest, or no manifest tick yet) waits in the queue too: the line is never typed until the ready queue reads an idle prompt. On a headless `sprig` pane the turns after the call run with `--model NAME`, after the pane's own args, until the agent ends or the pane is resumed: `when` is `next turn`. The record keeps the name (`model` in the pane record), so a resume or a restart of that pane runs its turns on it again. Any other harness gets `bad_request` `Swapping the model of <label> is not built for <harness> yet.` `bad_request` `model must be a plain name: letters, digits and . _ : / - [ ], up to 64 characters.` for any other name; `no_such_pane`; `pane_closed`; `not_attached` on a connection attached to the pane view only, before any other check. `unauthorized` from a pane, a plugin, or a peer the server cannot place: `only the operator sets an agent's model.` |
| `agent.set_router` | `pane`; `router` is `direct` or `gateway` | `pane` (the new pane's id), `workspace`, `tab`, `resumed`, `router` | Stops the agent, keeps its record as ended, and resumes its conversation in a new pane whose env sets the base URL: `ANTHROPIC_BASE_URL` for a `claude` pane. `direct` sets the vendor's own address (`https://api.anthropic.com`), never a missing key, since a pane's process also inherits the server's env. `gateway` sets the gateway's address from `coppice.toml`. What routes behind the gateway, `gateway` or `switchyard`, is the gateway's own setting, so a pane picks `gateway` and `stack.router` then names that setting. The new pane has a new id and no watching marker: it starts enforcing. `bad_request` `<label> has no conversation to resume yet, so a restart would lose it. Wait for its first reply, then swap.` when the record has no harness session id, or a pty harness has no `resume_args`; the agent is not stopped. `bad_request` `router must be direct or gateway.`; `bad_request` `no gateway is set. Add gateway = "http://127.0.0.1:8787" to coppice.toml.`; `bad_request` `the gateway at <url> does not answer. Start it, then swap.` for `gateway` when a loopback gateway accepts no connection, before the agent stops; `bad_request` `Swapping the router of <label> is not built for <harness> yet.`; `no_such_pane`; `pane_closed`; `not_attached` as for `agent.set_model`; `spawn_failed` when the new pane cannot start: the swap then tries one plain `pane.resume` with the old env. If that works, the reply says `The router of <label> did not change: <why> The agent runs again on its old router, as pane <id>.`; if it fails too, `<label> was stopped and its router did not change. It is in the ended list. To start it again, run: coppice pane resume <id>.` and the ended record stays; `internal` when the old process does not stop in 10 s. `unauthorized` from a pane, a plugin, or a peer the server cannot place: `only the operator sets an agent's router.` |
| `agent.move` | `pane`; `harness`, a harness name as in `coppice.toml` other than the pane's own; `preview`, optional bool | with `preview`: `from` (the pane's harness), `harness`, `tokens`, `messages`, `trimmed`, and `conversation` when the transcript counted usage. Without: the same, and `pane` (the new agent), `workspace`, `tab`, `when` (`now`, `queued` or `typed`: `typed` is a terminal agent with no screen rule, whose brief went in at once), and for a queued brief `queued` and `note` | Moves the conversation to another loop. It reads the pane's transcript the way `pane.messages` does and builds a brief from its tail: a preface, then the newest owner and agent texts that fit 12000 runes and 40 messages, oldest first. A row with a tool name is left out whole, so the brief carries no tool output and no tool input. `tokens` is the brief's size, about four bytes to a token. `conversation` is the transcript's own count of the conversation, input read fresh plus cache writes, each once; `trimmed` is true when older messages were left out. With `preview` nothing starts, so a client shows the cost first. Without it, a new agent of `harness` starts in the same directory, workspace and tab (a terminal agent when `coppice.toml` has `[harness.NAME]`, else a headless one when an adapter is built), and the brief goes to it as a prompt; a terminal agent takes it through the ready queue, as `agent.prompt` does. The old agent stays and is not touched. If the brief cannot go, the new agent is removed and the reply is an error. `bad_request` `<label> already runs on <harness>.`; `<label> (<id>) has no transcript to move. Its hook reported none.`; `<label> has no conversation to move yet. Wait for its first reply.`; `no harness named "<name>" in <path>. ...`; `Swapping the loop of <label> is not built for a pane with no harness yet.`; a bad harness name; `no_such_pane`; `pane_closed` for an ended pane; `not_attached` as for `agent.set_model`; `server_closed`; `spawn_failed`; `unauthorized` from a pane, a plugin, or a peer the server cannot place: `only the operator moves an agent's conversation.`; `unauthorized` from a named web token, preview included: `the chat is for the owner's own sign-in.` |
| `agent.read` | `pane`; `region` is `visible`, `recent` or `detection`, default `recent`. `source` is accepted as an older name for `region` | one `agent.list` row plus `text` and `region` | `pane_closed` when the screen is freed |
| `pane.messages` | `pane`; `since` and `limit` as for `floor.chat` | `messages` list, oldest first, each with `id`, `at`, `role` (`owner` or `agent`), `text`, optional `tool`; `source` `transcript`; `more` bool. See Agent facts | `no_such_pane`. `bad_request` with `<label> (<id>) has no transcript to read. Its hook reported none.` when the server reads no file for the pane. `unauthorized` as in Roles. `internal` when the file cannot be read |

### Agent facts

What the floor knows about each agent beyond its state. A field the server
does not know is absent, never guessed.

A gate hook adds three optional fields to the event it sends with
`pane.report_state`. The server keeps them apart from the PaneStateEvent:
no `state` event and no row carries them as sent.

- `transcript_path`: the absolute path of the harness's own transcript file.
  The server reads it only when all of these hold:
  - The report comes from a process inside the pane: the kernel placed
    the connection's peer (SO_PEERCRED, then its parent chain in /proc)
    in this pty pane, and it is no plugin. A `hello` that says it is the
    pane does not count. From anywhere else the reply's `transcript` says
    `only a process inside the pane names its transcript.`
  - The event's `harness` is `claude-code` or `claude`, and the pane's
    record names the same harness.
  - The path ends in `.jsonl` and lies under the `projects` directory of
    the Claude config directory the pane sees: its `CLAUDE_CONFIG_DIR`, or
    `$HOME/.claude`, taken from the pane's env over the server's.
  The path's real path, every symlink resolved, must lie under the real
  `projects` directory too. The real path is then the pane's claim, for
  good: the server keeps every claim in `<data dir>/transcripts.json`
  (0600) across restarts. A path the pane claimed before is taken again
  with no more checks. `pane.resume` of a pane hands its claims to the
  new pane, since the resumed session writes on in the same file. Else
  the transcript is refused, and only the transcript: the rest of the
  report (the state, the ask, the verdict, the session id) still counts,
  and the reply's `transcript` names the refusal, when:
  - another pane claimed that real path first, live or ended, the ended
    foreman included, before or after a restart: `another pane named that
    transcript first. A pane reads only its own.`;
  - its first record, the first `user` or `assistant` entry in its first
    64 KiB, has no `timestamp` or no `cwd`: `that transcript's first record
    has no time or no working directory. A pane reads only its own.`; a
    file of 64 KiB or more with no such record in them: `that transcript's
    first line is too long to check. A pane reads only its own.`; a file
    with no record yet passes, since a hook may report it before its first
    line;
  - that record's time is more than a second before the pane's start:
    `that transcript began before the pane started. A pane reads only its
    own.`;
  - that record's `cwd` is not the pane's working directory or under it,
    such as the owner's own Claude session in another terminal: `that
    transcript began in another directory. A pane reads only its own.`
    A pane that works in `/` or in its home dir takes only a `cwd` that is
    that directory itself, since every directory is under it;
  - the pane claimed 72 transcripts already: `this pane named too many
    transcripts. Start a new pane.`
  Every read resolves the claimed path again: when its real path is no
  longer the claim (a directory on the way was swapped for a symlink), the
  read fails with `that transcript's path now leads to another file. A
  pane reads only its own.`
  A claims file that does not parse moves aside as a chat file does. One
  that cannot be read refuses every new claim until a restart reads it.
  The server never follows a symlink at the path, never reads a path that
  is not a regular file, and reads only the bytes added since its last
  read, at most 4 MiB at a time. A line longer than that is skipped.
- `verdict`: `{decision, tool, clause}`. `decision` is `allow`, `deny` or
  `ask`. `tool` and `clause` are strings, cut at 200 runes. In watching
  mode `decision` is what the harness was told, and `clause` names what an
  enforcing gate would have denied. `model` in a row is cut the same way.
- `mode`: `enforcing` or `watching`. daisugi's config value `audit` is
  `watching`, and `enforce` is `enforcing`. (A session tree line written
  before audit mode was renamed says `shadow`; it is also `watching`.)
- `harness_session_id`, a PaneStateEvent field, is also kept: when the
  pane's own connection reports it for a live pty pane, and the event's
  `harness` names the pane's own harness (`claude-code` counts as
  `claude`), the server stores it as the record's harness session id, so
  `pane.resume` can resume that session. An id of anything but letters, digits, `.`, `_`, `:` and `-`, or
  one led by `-` or longer than 128 bytes, is not stored.

`pane.messages` reads the same file the facts read, under the same rules:
for a Claude pane the transcript its own process claimed last (after a
restart, its newest claim), for a sprig pane its session tree. A path no
process inside the pane named is never read, and a path another pane
claimed first is not this pane's. A pane reading its own messages gets
the first-record checks above again, and their refusal, so a file it
claimed empty and filled from an older one is not its own. The read takes the
last 4 MiB of the file and skips the first line when the read starts inside
it. A missing file reads as no messages.

- Claude: a `user` entry gives role `owner` messages, an `assistant` entry
  gives role `agent` messages. `message.content` is a string, or a list of
  blocks: a `text` block is one message, a `tool_use` block is one message
  with `tool` its name, and every other block (`thinking`, `tool_result`,
  an image) is left out. An entry is left out whole when it carries
  `isMeta`, `isSidechain` or `isCompactSummary` true, or a
  `toolUseResult` key. An owner text that starts with `<local-command-`,
  `<bash-` or `<command-` is the record of a command and its output, and
  is left out. `at` is the entry's `timestamp`.
- sprig: a `prompt` entry gives an `owner` message, an `assistant` entry
  with `text` an `agent` message, and a `tool_call` entry an `agent`
  message with `tool` its `name`. `at` is the entry's `ts`.
- A tool's message `text` is a one-line summary of its input: the first
  string, of `command`, `file_path`, `path`, `pattern`, `url`, `query` and
  `description`, that is not empty, else sprig's `detail`. Each run of
  white space is one space, and the line is cut at 200 runes with `…`. A
  tool's output never appears, since output can hold secrets.
- A message's text loses its control characters (below U+0020 but the
  line feed, U+007F, and U+0080 to U+009F), the Unicode format characters
  (Cf: bidi marks, zero-width characters), and U+2028 and U+2029. A tab
  becomes a space. It is
  trimmed of white space at both ends and cut at 2000 runes with `…`. A
  message with no text and no tool is left out.
- `id` is the byte offset of the message's line in the file, as a decimal
  string. A line that gives more than one message names the second
  `<offset>.1`, the third `<offset>.2`, and so on. `at` is unix seconds, 0
  when the entry has no time the server can read.

A `pane.list` row carries:

- `stack`, an object:
  - `loop`: the pane's harness.
  - `model`: the model the agent last used. Claude: `message.model` of the
    newest assistant entry in the reported transcript, not `<synthetic>`.
    sprig: `model` of the newest assistant entry in its session tree, the
    file `<--session-dir>/<harness session id>.jsonl` its adapter tails.
  - `router`: `gateway`, `switchyard` or `direct`. The server compares the
    pane's `ANTHROPIC_BASE_URL` and `OPENAI_BASE_URL`, its own env over the
    server's, with the gateway address by scheme, host and port, every
    loopback name the same host. The gateway address is `gateway` in
    `coppice.toml`, or `http://127.0.0.1:8787`, where `daisugi gateway`
    listens by default, when the key is absent. `gateway = ""` means there
    is no gateway. Then a base URL at `https://api.anthropic.com` or
    `https://api.openai.com` is `direct`, and any other leaves `router`
    absent. A pane pointed at the
    gateway is `switchyard` when `gateway_router: switchyard` is in
    daisugi's `config.yaml` beside the gate root, else `gateway`. The
    gateway reads that key only when it starts, and its `--router` flag
    overrides it. Absent when neither variable is set.
  - `daisugi`: `{mode, armed}` on a live pane with a harness. `mode` is
    the mode its gate hook last reported, or for a sprig pane the mode of
    the newest verdict in its session tree, else `off`. `armed` is false
    while the disarm marker `DISARMED` is in the gate root. `marker`
    `watching` is there while the agent's mode marker says watching (see
    `agent.set_mode`); `mode` says what the gate last reported, so it can
    say `enforcing` until the agent's next tool call. `marker` is `refused`
    when the gate would refuse a path on the way to the marker: the gate
    root as a symlink, or a root, `agents/` or floor directory that another
    user owns or group or others can write, or a marker that is a link or
    shared. The gate then lowers nothing and, for the root or a directory,
    denies every call. `markerNote` is the gate's one line with the fix
    (`<path>: <reason> (fix: chmod 700 '<path>')`). The server checks these
    paths with the same rules as the gate (GR-R-1), by fd from the root down,
    so `watching` is never shown for a marker the gate would not read.
- `gate`: `{decision, tool, clause, at}`, the last verdict. `at` is the
  unix second the server took a gate hook's report, or for a sprig pane
  the `ts` of the verdict entry in its session tree, never later than now.
  The newer one wins.
- `tokens`: `{fresh, cache_read, cache_write, out}` summed over every
  transcript the pane reported. Claude: `input_tokens`,
  `cache_read_input_tokens`, `cache_creation_input_tokens` and
  `output_tokens` of each assistant message, a message Claude splits over
  several lines counted once. sprig: `fresh`, `cacheRead`, `cacheWrite`
  and `out` of each assistant entry. Absent until one file was read.

The server reads a transcript when a client asks for a row or for
`floor.facts`, at most once a second for each pane, and not for an ended
pane. The read runs apart from the request, one at a time for each pane.
The request waits at most 50 ms for a read it started, then answers with
the facts it already has, so a file whose read hangs never holds up
`pane.list` or `floor.facts`. A path whose read takes longer than 5
seconds is never read again. A pane reads up to 8 files. It remembers
where it stopped in up to 64 more, so a file used again is read on, not
counted twice. Past that, a file's tokens stay in the pane's sum and its
claim is released.

`floor.facts` is the floor's header row. `daisugi.mode` is the least
guarded mode among the live harness panes, `off` below `watching` below
`enforcing`, and `off` when there is none, so the header never claims more
than every agent has. `tokens_today` sums the whole `tokens` of each pane
the server had a report or a token change from today, local time. A pane
that `pane.forget` removed no longer counts. `gateway.answers` is whether a
TCP connection to the gateway address opens within 300 ms. The server
dials only a loopback address and sends no request, since the gateway
passes almost every request on to the model provider. It remembers the
answer for 5 seconds. `answers` is absent for an address that is not
loopback.

`floor.facts` carries `saved_today`, `{tokens}`, only when the gateway has
a figure for this floor: the sum of `frontier_tokens_saved` over the turns
of today, local time, that name a pane of this floor, in the gateway's turn
journal (`gateway/turns.jsonl` in the daisugi data dir, beside the gate
root). A turn names its pane in a `pane` field. A gateway that writes no
`pane` field gives no figure, because the floor will not guess which pane a
saving belongs to. The gateway writes `pane` and `floor` when a pane's base
URL carries the prefix `/pane/<floor>/<pane>`; coppice puts it there when it
starts a pane whose own env sets `ANTHROPIC_BASE_URL` or `OPENAI_BASE_URL` to
the gateway, and only when the gateway says it takes the prefix (`GET /_caps` on
a loopback gateway answers `{"pane_prefix": true}`; coppice asks once for each
gateway address, with a 500 ms limit, keeps a yes, and asks again after 5 s
on a no; an older gateway has no such route, so its panes keep the plain URL),
ahead of any path the URL has, with `<floor>` the floor key of
`agent.set_mode` and `<pane>` the pane's id at that start. The record in the
pane's tree keeps the plain URL, so a resume stamps its new id. A base URL a
pane only inherits from the server is not stamped. A record that names a
floor counts only on that floor; one with no `floor` counts by its pane. With no journal, one that cannot be read, or no turn
today that names a pane here, the key is absent and a client shows no
number.

The journal of verdicts keeps the last 500 verdicts: each `verdict` object
of a `pane.report_state` report, with the pane, the time and the report's
mode, and each verdict a poll reads from a sprig pane's session tree (with
the tree's own time; a verdict not newer than the newest one the journal
holds for that pane is skipped, so a restart adds none twice). The file is
`journal/verdicts.jsonl` under the data dir, mode 0600, one JSON object a
line. The first time the server reads or adds a verdict after it starts, it
cuts the file back to 500 lines. A file that does not parse moves to
`verdicts.jsonl.broken-<UTC time>` with a note, and the journal starts
fresh.

### The machines view

`floor.machines` is a read-only view of the boxes on the tailnet that serve
the floor. It runs `tailscale status --json` once per call, with a 3 s limit,
and starts nothing else on the tailnet. Adding a box and the access rules
stay in Tailscale's own console.

A profile points at a machine when the host of its `base_url` is the
machine's short or full MagicDNS name, or one of its tailnet addresses. A
profile whose host is a loopback name points at this box, unless a machine
claims the address. The profile table is the operator's, read with
`daisugi agent-profiles list --json`: one row for each valid profile with its
name, wire, base URL, model and `key_set`. The command reads whether a key
variable is set and nothing else about it, so no key and no variable name
enters the reply. `key_set` is false only when the profile names a key
variable that is not set; a profile that needs no key reads as set. The
server runs it in the foreman's neutral `compactor` directory, never the
bundle command. A profile with no base URL is not on any machine. `address` is the host and port as the profile writes them
(the port is 80 or 443 for a URL with none). `answers` is whether a TCP
connection opens within 1 s, and `answers_ms` is how long it took, `null`
when it did not. When the host is a MagicDNS name the server connects to the
machine's first IPv4 tailnet address, so the check needs no DNS. A
`[voice] url` on a machine is listed the same way under `voice`, and a
`[voice] speak_url` under `speak`. No request
is sent over any connection. The server probes nothing else.


## Events

An event reaches a connection only when the connection subscribed to its
kind and its pane. `pane.attach` subscribes to `frame` and `state` for the
attached pane. `events.subscribe` widens that.

### `frame`

One render update for one attached pane. `seq` counts per connection and
starts at 1. The first frame carries every row and `"full": true`. A later
frame carries only the rows that changed and has no `full` key. A cell is
`[text, fg, bg, attrs]`. An empty colour means the terminal default.

```
{"event":"frame","pane":"w1:p1","seq":1,"cols":2,"rows":1,"cursor":[0,0],
 "rows_changed":{"0":[["h","#ffffff","",0],["i","","",0]]},"full":true}
```

`cursor` is `[column, row]`. Frames are sent at most every 16 ms per pane.
A client applies a full frame to an empty grid of `cols` by `rows`. A
client applies a delta on top of the frame before it. The `full` flag is
the only rule a client needs: a full frame follows an attach, a change of
size, `events.resume` and every `frame_gap`, and `seq` may skip numbers
after a gap. Frames and state events for one pane travel on separate paths
and may arrive in either order relative to each other.

### `frame_gap`

Each attached pane on a connection has a buffer of 64 frames. When the
client has not read them, the server drops the frames that do not fit and
counts them. When a frame fits again, the server sends one `frame_gap`
first, then a full frame. `dropped` is the number of frames the client did
not get. State events are not affected by a gap. They keep their own queue
of 256, and a client that falls that far behind on state events is
disconnected.

```
{"event":"frame_gap","pane":"w1:p1","dropped":37}
```

### `state`

One PaneStateEvent, sent when a pane's merged state changes. The same shape
is what `pane.report_state` accepts under `event`.

```
{"event":"state","v":1,"ts":1757300000.5,"session_id":"w1:p1",
 "harness_session_id":null,"harness":"claude-code","pane":"w1:p1",
 "state":"blocked","source":"gate",
 "ask":{"id":"toolu_1","tool":"Bash","summary":"rm -rf build/","deadline":1757300090,
        "tier":"permanent"},
 "detail":"verdict=deny clause=shell.deny[2]"}
```

- `state` is one of `idle`, `working`, `blocked`, `done`, `unknown`.
- `source` is one of `operator`, `gate`, `headless`, `process`, `manifest`.
  The server keeps the highest source on top, in that order.
- `done` comes only from `process` or `headless`.
- A live `pty` pane's `process` fact is `working`, detail `output in the
  last 5 s`, while its screen took bytes in the last 5 seconds, and
  `idle` after, detail `no output yet` or `no output for 5 s`. The server
  looks once a second, and again at each `pane.list`, `agent.list`,
  `agent.get`, `task.list`, `floor.facts` and `pane.wait_output` for a
  state, so such a read sees the fact as of its own moment: never `no
  output yet` after output that is on the screen. A read can so send the
  pane's `state` event a moment before the next look would have.
- A short redraw is no work while the gate last said idle (CP-R-56). A pane
  whose last gate fact was `idle` keeps its `idle` when output starts after
  a quiet spell, until the burst has gone on for 1 s after the first write a
  look saw. A clock or a status line that redraws a few bytes does not make
  the pane `working`. A gate `working` or `blocked`, and a pane that never
  had a gate fact, are not held.
- The talk queue takes a standing hook idle (CP-R-57), and only the talk
  queue. When a hook idle (source `gate` or `headless`, as Stop reports it)
  is at least 3 s old with no hook fact and no submitted input since, and the
  merged state is `working` from the process source only, the queue counts
  the pane as `idle` and sends its page or words. State events and reads do
  not change.
- `ask` is present only on `blocked`. A `gate` blocked event must carry it.
- `ask.tier` is `undoable` or `permanent`. An ask with no tier, or a tier
  the server does not know, reads as `permanent`, and the server sends it
  that way. A tier that is not a string makes the event invalid.
- `detail` is at most 200 characters.
- `harness_session_id` and `pane` may be `null`.
- `held` is present only while a foreman holds the ask. See Holds. Only
  the server sets it. `pane.report_state` never reads it.
- `who` is present only on an `operator` event. It is the name of the
  connection that sent the report. Only the server sets it. The server
  drops a `who` that a report carries. A `who` on any other source makes
  the event invalid.

### `presence`

Who looks at one pane, sent when a person attaches to it or leaves it.
`looking` lists the names of the named operator connections attached to
the pane now, sorted, each name once. A view-only attach counts, so a view
on the floor page counts for the page's own name. A connection with no
name, a pane and a plugin do not show. An empty list means nobody named
looks.

```
{"event":"presence","pane":"w1:p1","looking":["alice","bob"]}
```

### `note`

One line the floor prints in dim: a verb a pane ran, or a `floor.note`.
`pane` names the pane it came from and is absent for an operator note.
`to`, when present, names the pane the note is for, apart from its author:
a hold note comes from the asking pane and is for its foreman.

```
{"event":"note","text":"floor › pane.create  docs  pi","pane":"w1:p1","ts":1757300000.5}
```

### `child`

One subagent of a pane started or stopped. `label` is the subagent's type
as its harness names it.

```
{"event":"child","pane":"w1:p1","child":"a1","state":"working","label":"Explore","ts":1757300000.5}
```

### `messages`

New messages may exist for a pane: `pane.messages` would read more, or,
with `chat` true, `floor.chat` would. The event carries no text. A client
that wants the messages asks for them. The server sends it when it writes
a chat line, and when it sees a transcript file the server reads change
in size or time, or a pane get or change its file. It looks at each such
file every half second, from the first `events.subscribe` that names
`messages` until the server stops. `chat` is true when the pane is the
floor's tracked foreman.

```
{"event":"messages","pane":"w1:p1","chat":true,"ts":1757300000.5}
```

### `layout`

`events.subscribe` accepts `layout` as a kind. The server sends no `layout`
event today. A client that subscribes to it receives nothing.

## ACP adjacency

The Agent Client Protocol, ACP, is JSON-RPC 2.0 between an editor and an
agent. coppice is not an ACP agent and not an ACP client. Four ACP methods
have a coppice verb beside them, and a client that knows ACP can drive a
pane through these verbs over the JSON-RPC framing.
`testdata/protocol/acp-adjacent.jsonl` sends each one.

| ACP method | coppice verb | what differs |
|---|---|---|
| `session/new` | `pane.create` | ACP starts an agent session. coppice starts a process in a pane, in a `cwd`, with an argv or a named harness. |
| `session/prompt` | `pane.send_text`, or `agent.prompt` | ACP streams the turn back as updates. coppice sends the text and replies at once. `agent.prompt` with `wait` replies when the pane reaches the `until` state. |
| `session/cancel` | `pane.send_keys` with `["ctrl+c"]` | ACP cancels the turn. coppice sends the key, and the harness decides what it means. |
| `session/request_permission` | a `state` event with `state` `blocked` and an `ask`; the reply is `agent.allow` or `agent.deny` | ACP asks the client, and the client answers. coppice sends the ask as an event, and only an operator may answer it. |

Allow and deny keep every rule in Roles and in `agent.allow`, whatever
door they come through. A connection the server places as a pane gets
`unauthorized` for both. An ACP client that runs on the agent side, inside
a pane, is placed as a pane, so it can never allow. An allow or a deny for
an ask the pane never raised is `bad_request`. A permanent ask needs
`confirm`, the pane name, and a missing tier reads as permanent.

The gap. ACP has no fail-closed tier on a permission request: it has no
`undoable` and no `permanent`, and no rule that a missing tier is the
stricter one. ACP has no `frame` stream either. A client that knows only
ACP sees text and permission requests, and never the screen of the pane.
coppice does not carry ACP's own method names on the wire. A client maps
them itself.

## Versioning

- This page carries `Version: 1` near its top. `server.status` returns
  `protocol: 1`. The two numbers are one number.
- A change that adds a verb, an event, a result field or an optional
  parameter keeps the version. A client must ignore fields it does not know.
- A change that removes or renames a verb, an event, a field or an error
  code, or changes a field's type, raises the version.
- The native envelope is stable. JSON-RPC framing is an addition beside it,
  not a replacement.

## The corpus

`testdata/protocol/*.jsonl` holds request and expected-reply pairs.
`testdata/protocol/README.md` states the file format and the matching rules.
`internal/proto/conformance_test.go` replays every file against a real
server. A client in another language replays the same files with the same
rules. A `#skip` line in a corpus file is a listed gap, never a pass.

## The foreman's tools in pane mode

When `floor.talk` or `floor.foreman` starts a pane foreman, or a resumed
foreman is followed, the server opens the standing socket
`<log dir>/tools.sock` (mode 0600, once, until the server closes). If the
harness command is `claude` or `opencode`, it also starts the pane with the
`foreman` MCP server mounted (`--mcp-config`, or `OPENCODE_CONFIG_CONTENT`). The socket speaks the same one-line protocol as
the socket of a chat turn and serves the same four verbs. The peer must
descend from the pane foreman's process; any other peer gets a `refused`
error. `zoom` also takes `offset` and `size` (bytes, whole numbers, at most
32 KiB a page; refused with a `label`). A reply that is not the whole text ends with a line
`[bytes A-B of N. More: offset B]` or `[bytes A-B of N. End.]`, and a cut
never splits a UTF-8 rune. A node that is not in the chat answers with an
error that ends with `The root nodes are <id+n>, ...` (or `The chat is
empty.`). With no chat open, `zoom` with `id` and `n` and `date` answer one plain
line that the foreman keeps its own memory, and no call is logged. A worker's
report is typed into the pane when it is idle, and shown as a `note` in the
chat files. A `pi` foreman gets a note that says to mount `coppice foreman tools
--standing` by hand. Worker reports wait in a queue of their own (16, the
oldest dropped with a note) and do not count against the 32 of `floor.talk`.

## The foreman in chat mode

With `[foreman] mode = "chat"` the foreman is one chat that never ends, not
a pane. `floor.talk` logs the owner's words as a `user` message, queues a
turn and answers at once with `{mode: "chat", message: "log/<i>", queued,
label}`; the reply to the owner comes through `floor.chat` and the
`messages` event. `floor.foreman` is refused: there is no pane to start. At
most 32 turns wait; past that `floor.talk` answers `bad_request`. A log that
cannot be opened is an `internal` error with a plain reason in each
`floor.talk` and `floor.chat`.

The chat opens even when its files are damaged (FM-R-68 and FM-R-69). The
tree and the view come from the log, so only the log is irreplaceable. A tree
line that is not a node, or is past the end of the log, is copied word for
word to `tree/<day>.jsonl.broken-<UTC time>` (0600), the tree file is
rewritten without it (temp file, fsync, rename), and the nodes it held are
built again, in id order, before any node of a later message. A `view.json`
that cannot be used is copied alone to `view.json.broken-<UTC time>` and the
view is rebuilt by replay over the tree (the save of the rebuilt view replaces
the bad file; an open that fails first leaves it in place, so the next open
repairs it again); a missing `view.json` is rebuilt the same way, without a
copy; if that fails, the whole history moves
aside as for a bad `main/` line. Each open that repaired anything adds one
`note` to the chat that names the copies. A bad line in `main/` still moves the
whole dir to `<dir>.broken-<UTC time>` and starts a new chat. The reads
(`coppice foreman log|view|zoom|date`) never repair or write: they skip a
bad tree line, build the view in memory, and say so on stderr.

Each turn the server listens on `<log dir>/turns/t-<n>.sock`, a socket in
the turn dir of this data dir (mode 0700; the turn's cwd and its system
prompt file are there too), that serves only the foreman's four tools to
the turn's own process tree. It is
not a second public socket: one JSON line in (`{"tool", "args"}`), one out
(`{"ok", "result" | "error"}`), then close. The server checks SO_PEERCRED,
refuses every peer that does not descend from the process the turn
started, and removes the socket when the turn ends.

### Importing old chats (FM-R-71 to FM-R-85)

Old Claude Code chats (`~/.claude/projects/*/*.jsonl`) and Codex chats
(`~/.codex/sessions/**/rollout-*.jsonl`) can join the foreman's memory, but only
when the owner chooses. Nothing is read into the chat before that.

`floor.import` is operator only (a pane or a plugin gets "only the operator
imports old chats into the foreman."), and needs chat mode. Its optional
`action` is `status` (the default), `progress` (the state, `done` and `total`
only: it starts no scan), `accept`, `decline`, `pause` or `resume`;
`accept` also takes `from` (`claude`, `codex` or `all`), `since` (`YYYY-MM-DD`)
and `tools` (bool, default false). Every action answers
`{state, offered, paused, done, total, queued, sessions, messages, estimating}`.
`state` is `off` (not chosen), `declined`, `running`, `paused` or `done`.
`sessions` and `messages` are what could be imported now; `estimating` is true
while the first scan of the transcripts runs (the reply does not wait for it).

When the chat opens and old chats exist and the owner has not chosen, the
server logs one `note` that offers the import, once (`offered` in
`import.json`). Declining is remembered, and the command and the verb still
work later.

The queue and the choice are in `<log dir>/import.json` (0600). The compactor
loop imports one message at a time, as kind `past`, dated when it was said,
only when no live node is pending and no turn runs or waits. A live message is
therefore compacted before any import node that was queued earlier, no model call
starts for an import node while a turn runs, and
`coppice foreman import --pause` takes hold at the next message. Each message
carries an act id (`imp:claude:<uuid>`, or `imp:<source>:<session>:<line>`), so
a crash, a restart or a second import never logs one twice. Thinking is never
imported; tool calls and their results only with `tools`. The floor's chat
leaves `past` messages out unless `floor.chat` gets `past: true`.
