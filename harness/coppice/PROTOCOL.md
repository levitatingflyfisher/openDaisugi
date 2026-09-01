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

A reply echoes the id:

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
| `pane_closed` | the pane exists but has closed, so it cannot be written or read |
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
`pane` answer `no_such_pane` for an unknown id and `pane_closed` for a closed
one, unless the row says otherwise.

### Server

| verb | params | result |
|---|---|---|
| `server.status` | none | `protocol` number, `pid`, `socket`, `data_dir`, `uptime_s`, `panes` count, `panes_live` count, `restart_note` text, `restore` object with `panes`, `already_closed`, `resumed`, `marked_done`, `marked_unknown`, `notes` list, `detection_warnings` list |
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
| `pane.create` | `cwd` required; `kind` is `pty` or `headless`, default `pty`; `cmd_argv` list, required for `pty`; `harness` name, required for `headless`; `workspace` and `tab` ids, default the current ones; `cols` default 120; `rows` default 40; `env` object of strings; `label` | `pane` id, `workspace` id, `tab` id | `no_such_workspace`, `no_such_tab`, `spawn_failed`, `server_closed`. An unknown harness is `bad_request` |
| `pane.split` | same as `pane.create` | same as `pane.create` | same as `pane.create` |
| `pane.list` | none | `panes` list of rows. Each row has `id`, `label`, `cwd`, `cmd` list, `kind`, `harness`, `workspace`, `tab`, `closed`, `cols`, `rows`, `state`, `source`, `detail`; optional `exit_code`, `quiet_for` seconds, `ts`, `session_id`, `ask`, `parent_pane` | |
| `session.list` | same as `pane.list` | same as `pane.list` | |
| `pane.send_text` | `pane`; `text` required, `""` sends a bare Enter; `enter` default true | `sent` byte count | `internal` |
| `pane.send_keys` | `pane`; `keys` non-empty list of key names or single characters. `testdata/keys.json` lists the names | `sent` key count | `internal` |
| `pane.run` | `pane`; `line` required, sent with a trailing Enter | `sent` byte count | `internal` |
| `pane.read` | `pane`; `source` is `visible`, `recent` or `detection`, default `visible` | `text`, `source` | A pane whose process exited but was not closed still reads. Only `pane.close` frees the screen |
| `pane.resize` | `pane`; `cols` and `rows`, both positive | `cols`, `rows` | `internal` |
| `pane.close` | `pane`, or `session` | `pane` id, `closed` true | `no_such_pane` only. Closing a closed pane succeeds |
| `session.stop` | same as `pane.close` | same as `pane.close` | same as `pane.close` |
| `pane.wait_output` | `pane`; `contains` text or `state` name, at least one; `timeout_ms` default 30000 | `matched` is `text`, or `matched` is `state` with `state` and `closed` | `timeout`, `pane_closed` |
| `pane.fork` | `pane`; `label`, default the parent's label plus ` fork` | `pane` id of the new pane, `workspace`, `tab`, `parent_pane` | `adapter_error` when the pane is a pty pane, when its harness cannot fork, or when the harness has not reported a session id yet. `spawn_failed`, `server_closed`. The new pane is headless, in the parent's workspace and tab, with the parent's harness, cwd, env, argv and size. It starts by resuming the parent's session as a new session. Until its harness reports a session id of its own, it cannot be forked again, and a server restart repeats the fork from the parent's session. Its `pane.list` row carries `parent_pane` |
| `pane.explain` | `pane` | `pane`, `harness`, `detection_text`, `osc_title`, `osc_progress`, `detection_warnings`; optional `effective_state`, `effective_source`, `received_age_s`, `tick`, `note`, `agent`, `source_file`, `matched`, `rule_id`, `priority`, `region`, `manifest_state`, `skip_state_update`, `evaluated` | `internal`. The coppice binary registers this verb |

### Attach and events

| verb | params | result | errors |
|---|---|---|---|
| `pane.attach` | `pane`; `cols` and `rows`, both positive, resize the pane first; `view_only`, default false, makes the server ignore `cols` and `rows` | `pane` id, `attached` true | Attaching also subscribes this connection to `frame` and `state` for that pane. With `view_only` true, `pane.send_text`, `pane.send_keys`, `pane.run`, `pane.resize` and `agent.prompt` on this connection answer `not_attached` |
| `pane.detach` | `pane` | `pane` id, `attached` false | `not_attached` |
| `events.subscribe` | `kinds` list of `state`, `layout`, `frame`, default `state` and `layout`; `panes` is `"*"` or a non-empty list of pane ids, default `"*"` | `kinds`, `panes` | Subscribing to `frame` sends nothing until `pane.attach` starts a frame pump for that pane |
| `events.pause` | `pane` | `pane` id, `paused` true | `not_attached`. Stops `frame` events for that pane on this connection only. Once the reply is sent, no frame for the pane follows until `events.resume` |
| `events.resume` | `pane` | `pane` id, `paused` false | `not_attached`. The next frame is a full one. Deltas follow it |

### Agents

| verb | params | result | errors |
|---|---|---|---|
| `pane.report_state` | `pane`; `event` is one PaneStateEvent object, see Events | `pane`, `source`, `state`, `detail`; optional `ask`, `note` | A `source` of `operator` from a connection not attached to the pane becomes `gate`, and `note` says so |
| `agent.list` | none | `agents` list of rows. Each row has `pane`, `label`, `cwd`, `kind`, `harness`, `closed`, `state`, `source`, `detail`; optional `ts`, `session_id`, `ask`, `harness_session_id`, `parent_pane` | |
| `agent.get` | `pane` | one `agent.list` row | `no_such_pane` |
| `agent.prompt` | `pane`; `text` required; `wait` default false; `until` state name, default `idle`; `timeout_ms` default 120000 | without `wait`: `pane`, `sent`. With `wait`: `pane`, `state`, `source`, optional `ask` | `adapter_error`, `timeout` |
| `agent.wait` | `pane`; `until` default `idle`; `timeout_ms` default 120000 | `pane`, `state`, `source`, optional `ask` | `timeout` |
| `agent.read` | `pane`; `region` is `visible`, `recent` or `detection`, default `recent`. `source` is accepted as an older name for `region` | one `agent.list` row plus `text` and `region` | `pane_closed` when the screen is freed |

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
 "ask":{"id":"toolu_1","tool":"Bash","summary":"rm -rf build/","deadline":1757300090},
 "detail":"verdict=deny clause=shell.deny[2]"}
```

- `state` is one of `idle`, `working`, `blocked`, `done`, `unknown`.
- `source` is one of `operator`, `gate`, `headless`, `process`, `manifest`.
  The server keeps the highest source on top, in that order.
- `done` comes only from `process` or `headless`.
- `ask` is present only on `blocked`. A `gate` blocked event must carry it.
- `detail` is at most 200 characters.
- `harness_session_id` and `pane` may be `null`.

### `layout`

`events.subscribe` accepts `layout` as a kind. The server sends no `layout`
event today. A client that subscribes to it receives nothing.

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
