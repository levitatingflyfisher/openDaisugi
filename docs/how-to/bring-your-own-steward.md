# Bring your own steward

The floor's steward can be a harness you already like, with its own memory.
coppice gives that harness the same four tools our chat steward has, so it
can still start workers through the warden and see the worker table.

Our chat steward stays the default for `[steward] mode = "chat"`. This page
is for the other mode, `pane`, which is the default when the file says
nothing.

## 1. Pick the harness

The steward is the harness named by the `harness` field of the
`floor.steward` request, or else the `default` harness in `coppice.toml`.
The first thing you say to the floor starts it.

```toml
default = "claude"
```

## 2. Give it a memory

A harness with no memory forgets at its next restart. Install a memory
extension yourself, in that harness. Two examples for Pi, both other
people's projects under their own licences, which you read before you
install them:

- pi-optchat, an endless chat for Pi with a summary tree:
  <https://github.com/jonaslsaa/pi-optchat>
- pi-endless: <https://github.com/catoncat/pi-endless>

coppice copies no code from them and bundles none. It only starts the
harness you chose.

## 3. The four tools

When the harness can take an MCP server, coppice starts it with the server
`steward` already mounted. The tools are:

| Tool | What it does |
|---|---|
| `spawn` | Start a worker on a project with a brief. It returns at once with a label. The worker runs under the warden, like any node of the delegation tree. Its report comes back to the steward as a line that starts with the label in brackets, typed into the pane when the pane is idle. |
| `status` | List the workers: label, project, profile, model, start, state. |
| `zoom` | With a `label`, the newest 32 KiB of that worker's log, once it ended. With `id` and `n`, it answers in one line that this steward keeps its own memory. |
| `date` | The same one line: the floor keeps no log of this chat. |

`spawn` needs an envelope file for the project at
`<config dir>/envelopes/<project>.json`, as in chat mode.

## Which harnesses can mount it

| Harness | How | Status |
|---|---|---|
| Claude Code | `--mcp-config` with the server inline | Mounted |
| OpenCode | `OPENCODE_CONFIG_CONTENT` with a local MCP server | Mounted |
| Pi | none: Pi has no MCP client of its own | Not mounted. coppice posts one note. Mount the server by hand (section 4) through an MCP extension for Pi |
| Others | none known to coppice | Not mounted. Mount by hand (section 4) |

The floor opens the standing socket for every pane steward, whatever the
harness. Only Claude Code and OpenCode get the server added for them.

If you already have a `steward` entry in your own MCP config, or an
`OPENCODE_CONFIG_CONTENT` in your own environment, the floor's one replaces
it for the steward pane. Name your own server something else.

The harness is chosen by the command in its `[harness.<name>]` table, so a
table named `main` that runs `claude` is mounted too.

## 4. Mount it by hand

For any other harness that speaks MCP, add this stdio server to its own
config:

```
coppice steward tools --standing
```

The command finds the floor's socket from `COPPICE_DATA_DIR`, which every
pane has. Use `coppice --data-dir DIR steward tools --standing` for another
data dir. The server answers only when the floor started the steward: a call
from any process outside the steward's own process tree is refused. A
harness that you start by hand, outside the floor, is not served.

Any process that the steward's own shell starts is in its process tree, so
it can call `spawn` too. A worker is still bounded by the project's
envelope, so this gives it no power the steward did not have.

The socket is `tools.sock` in the floor's log dir, mode 0600. It is not the
public socket, and it takes only these four tools.

A steward that was already running before coppice restarted is found again and
gets the socket, but coppice cannot add the MCP mount to a process that is
already running. Restart that steward, or mount `coppice steward tools
--standing` in it by hand.
