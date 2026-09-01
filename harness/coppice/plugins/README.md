# coppice plugins

A plugin is a directory with a `manifest.json`. There are two kinds.

- A **view** is a page. The web server serves it at `/plugins/<id>/`, and
  the floor page opens it in a frame at `#/view/<id>`.
- A **policy** is a process. The server starts it, it listens to events on
  the socket, and it runs only the verbs its manifest names.

The shipped plugins live in this directory and ship inside the binary.
Your own plugins live in `$XDG_CONFIG_HOME/coppice/plugins/<id>/`, which
is `~/.config/coppice/plugins/<id>/` on most boxes. A directory of yours
may not take the id of a shipped plugin. The server refuses it, names the
shipped plugin, and runs the shipped one. Change a shipped plugin's
settings with its `[plugin.<id>]` table instead.

## Turn plugins on and off

`coppice.toml` lists the ids that run:

```toml
plugins = ["tree", "notify-ntfy", "turn-budget"]

[plugin.turn-budget]
budget = 25
```

- With no `plugins` key, the default list runs: every shipped view, and
  `notify-ntfy`.
- `plugins = []` runs no plugin.
- A list runs what it names and nothing else. To add a policy, list the
  views you want too.
- A `[plugin.<id>]` table sets that plugin's settings. They lie over the
  `config` in its manifest.

The server prints each plugin that does not load, and why, when it starts.

## The shipped set

| id | kind | on by default | what it does |
|---|---|---|---|
| `tree` | view | yes | The task tree, the same lines the terminal floor draws, and a graph of it. A button switches between the two. |
| `minimap` | view | yes | The task tree shrunk to dots. It sits at the foot of the floor page rail. A dot click fills a tile. |
| `kanban` | view | yes | Tasks as cards in four columns: inbox, working, needs you, done. The columns are states the server owns, so a card has no drag. |
| `colony` | view | yes | Every pane as a forager on a field, in its state colour, with a bar for the time since its last state. Drag a box to select. |
| `shift-log` | view | yes | Rows are panes and cells are five minutes of the last eighty. A cell shows working, an ask, or a deny. The floor page reads the last two hours from the web server's own event ring. A cell the ring does not cover, and no event marked, is drawn as unknown. |
| `herdr-grid` | view | yes | A roster with the state word on each row, and a slot for every pane. The first nine slots show the pane live. |
| `inbox` | view | yes | One row per task with a worktree, the ones that need you first, with its ahead count. `!` jumps to the first row that needs you. The selected row's pane shows live. |
| `notify-ntfy` | policy | yes | Posts a message to your ntfy topic when a pane becomes blocked. It does nothing until `[plugin.notify-ntfy]` sets `url` and `topic`. |
| `notify-lockscreen` | policy | no | Posts an ntfy card with a Look button. |
| `notify-voice` | policy | no | Speaks the label and the ask through `speak_url`, or leaves a note that voice is not set up. |
| `merge-on-green` | policy | no | Asks a done pane to merge once its pull request checks pass. |
| `close-quiet` | policy | no | Closes a pane that stays idle for `idle_minutes`. The worktree stays. |
| `turn-budget` | policy | no | Sends ctrl-c to a pane past `budget` turns. |

A view runs in a sandboxed frame and holds no token. The floor page posts
it the panes, the tasks, the recent events and the selection. A view may
post back three things: open a pane, set the selection, and watch up to
nine panes. The floor attaches a watched pane view-only, which never
resizes it and may not type into it, and posts the view the text of each
frame. Every shipped view imports `../_lib/view.js`, which the web server
serves at `/plugins/_lib/view.js`.

The three policies that act on panes ship off. Each one closes a pane,
stops a pane, or types into a pane, and you choose when that happens.

`notify-ntfy` does the same job as the built-in publisher that
`coppice web serve --ntfy` sets up. Set one of the two. With both set,
each block posts twice. Only the built-in publisher can put a Deny button
on a lock screen card, because only the server can mint a token that
denies one ask and nothing else. That is why `notify-lockscreen` carries
Look and no Deny.

## Bridges

A bridge puts the floor inside a tool you already use. The tool stays as
it is. None of the three can allow or deny an ask. Each one runs
`coppice --socket <socket> attach <pane>` when you open a pane, so your
keys reach the pane, and a full attach sizes the pane to its own window.
When two attaches show one pane, the last size sent wins.

| tool | what it needs | one command to start |
|---|---|---|
| Neovim | Neovim 0.10 or newer, and `nvim/` on its runtime path. See `nvim/README.md`. | `:Coppice` |
| tmux | tmux on `PATH`. Run it inside tmux, or pass `--session NAME`. | `coppice tmux-mirror` |
| Herdr | Herdr, and the `coppice` program on its `PATH`. | `daisugi coppice herdr-bridge <pane>` |

**Neovim.** `nvim/` is a client you install in Neovim. The binary does not
carry it. `:Coppice` opens the roster in a float. Space shows a pane's
screen in a split, read-only. Enter opens a terminal buffer that attaches.
Neovim that runs inside a coppice pane is placed as that pane.

**tmux.** `coppice tmux-mirror` drives tmux in control mode. It keeps one
window per open pane in one session, and each window runs `coppice attach`
on its pane. It sets that session's `status-right` to the roster count,
such as `2 need you · 3 working`, and to `quiet` when no pane needs you or
works. It marks each window it creates with the window option
`@coppice_pane`, and it renames and kills only the windows with that mark.
It kills the window of a pane that closes. A window you close, or leave
with the attach key, stays closed until the mirror starts again. When tmux
refuses a change to one window, because the window went first, the mirror
prints it and tries again on the next second. ctrl-c stops the mirror. Its
windows stay, and the session's status line goes back to what it was
before. A hang-up or a SIGTERM puts it back too. The session keeps its own
status line in the option `@coppice_saved_status` while a mirror runs, so
a second mirror on the same session puts back the right one. Each window
runs attach with no shell. `--tmux-socket PATH` points the mirror at a
tmux server on another socket.

**Herdr.** `daisugi coppice herdr-bridge <pane>` writes one file,
`agent-detection/coppice.toml` under Herdr's config directory, and prints
the `herdr pane run` line that runs attach in a Herdr pane. The file holds
detection rules that read the attach status line on the last row. It never
replaces a file there that is not its own. This bridge is not verified.
Herdr picks a manifest by the foreground process of a pane, and its
documentation says a new agent needs a Herdr update before Herdr knows it.
So Herdr may never apply these rules. The rules also need the whole state
word on the status line, so a pane narrower than its fields reads as
unknown. attach makes each run of spaces in a label one space, so a label
cannot hide the real state word. A label that reads like a state, such as
`blocked via gate`, can still fake that state in Herdr's read, and an
agent can set a label. The attach status line still shows the state and its source to
anyone who looks at the pane.

## The manifest

```json
{
  "id": "turn-budget",
  "kind": "policy",
  "title": "Turn budget",
  "about": "It pauses a pane with ctrl-c once it runs past a set number of turns.",
  "run": "turn-budget.py",
  "listens": ["state"],
  "needs": ["pane.send_keys", "floor.note"],
  "config": {"budget": 40}
}
```

| field | kind | meaning |
|---|---|---|
| `id` | both | Lower case letters, digits and hyphens. It starts with a letter and matches the directory name. |
| `kind` | both | `view` or `policy`. |
| `title` | both | The name the rail shows. |
| `about` | both | One line that says what the plugin does. |
| `page` | view | The html file, inside the directory. |
| `ring` | view | `true` asks the floor page to post the web server's ring of recent state events, with `from`, the time since which the ring holds every one. A view that draws no history leaves it off. |
| `run` | policy | The program, inside the directory. A `.py` file runs under `python3`. Anything else runs as it is and needs its execute bit. |
| `listens` | policy | The event kinds it may subscribe to: `state`, `layout`, `note`, `child`. |
| `needs` | policy | The verbs it may run, as `PROTOCOL.md` names them. |
| `config` | policy | Its settings. The runner passes them with your table laid over them. |

The loader refuses a manifest with an unknown field, a file outside the
directory, or a link that leads out of it.

## The rules

- **The `coppice.` prefix is reserved.** Only a plugin the binary carries
  may use an id that starts with `coppice.`.
- **A policy can propose. It cannot allow.** A manifest that asks for
  `agent.allow` or `agent.deny` does not load. The server refuses those
  verbs to every plugin connection anyway, and it refuses `pane.report_state`
  and `pane.report_child`, since a false state could open a blocked pane to
  typed keys.
- **A policy holds only its `needs`.** The server knows each policy it
  started by its process, through the kernel. A connection from the policy,
  or from any process under it, runs only those verbs, whatever it says in
  `hello`. A policy you start by hand, outside the server, is an operator
  connection. Send `hello` with `role` `plugin` from it to hold it to the
  manifest.
- **A view holds nothing and is given data.** A view runs in a frame
  sandboxed with scripts only, so it has an opaque origin. It cannot read
  the floor page's storage or its token. Every request it sends to `/api`
  or `/ws` carries `Origin: null`, and the server refuses it. The floor
  page reads the tasks and panes over its own connection and posts them
  into the frame with `postMessage`, with the panes the tiles showed as
  `sel`.
- **A view may ask for two things.** It may post
  `{"type": "open-pane", "pane": "<id>"}` or
  `{"type": "set-sel", "sel": ["<id>", ...]}` to `window.parent`. The floor
  page checks each pane against its pane list and acts on nothing else.
- **Every mark in a view means a state the socket reports.** A view draws
  what the floor page posts, and nothing it guesses.
- **An agent never edits the floor's config.** The gate denies every write
  it can place under `~/.config/coppice`, `~/.local/share/coppice` and
  `~/.opendaisugi/coppice`, and every shell line that names them, before
  any envelope check. It expands `~`, `$HOME` and the XDG homes, follows
  each `cd`, and normalizes `./`, `//` and `../`. It answers `this is the
  floor's own config. Edit it yourself.` A relative write after a `cd` the
  gate cannot follow is left to its tier, which is permanent. A plugin is
  something you add by hand.
- **A note names its plugin.** A `floor.note` from a policy leads with the
  plugin id and names no pane.

## How a policy runs

The server starts each enabled policy once it listens. The environment
carries:

- `COPPICE_SOCKET` and `COPPICE_SOCK`: the socket to dial.
- `COPPICE_PLUGIN`: the plugin id.
- `COPPICE_PLUGIN_CONFIG`: the settings, as one JSON object.

A shipped policy runs from a fresh copy of its files in
`$XDG_DATA_HOME/coppice/plugins/<id>/`, mode 0700, written on each start.
Your own policy runs where it is.

A policy that exits 0 is finished and does not start again. Exit 0 only
when there is nothing to do, such as a notifier with no settings. When the
socket closes, exit with another code: the server drops a client that falls
far behind on events, and the runner then starts the policy again. A policy
that fails starts again after a wait that doubles from one second to thirty.
After five failures in a row, each shorter than a minute, the server stops
trying and logs a line that names the plugin. When the server stops, every
policy stops with it, and so does each process a policy started.

A policy speaks the socket protocol in `PROTOCOL.md`. The shipped ones use
`_lib/floor_client.py`, which is stdlib Python:

```python
from floor_client import FloorClient, config

client = FloorClient()
client.hello()
client.subscribe(["state"])
while True:
    ev = client.next_event()
    if ev and ev.get("state") == "blocked":
        client.note(f"{ev['pane']} needs you")
```

Each shipped script starts with `#!/usr/bin/env python3` and a PEP 723
block with `dependencies = []`, so it runs on a box with no uv.

## Test a plugin

The tests in `tests/floor/test_example_policies.py` and
`tests/floor/test_example_notifiers.py` show the way. `tests/floor/plugin_fakes.py`
has three parts:

- `FakeFloor` listens on a unix socket, answers each verb from a table,
  sends a list of state events after the subscribe, and records every
  request the plugin sent.
- `run_plugin` runs a script against that socket with the environment the
  server would give it.
- `fake_gh` writes a `gh` for the front of `PATH` that logs its arguments
  and answers from a script.

Assert the exact requests the plugin sent, and that none is outside its
`needs`. A test never reaches a real ntfy, a real voice engine or a real
`gh`: point the plugin at a loopback server the test starts.

A view is plain html and a module script with no inline code, since the
content policy refuses inline script and style. It listens for `message`
events from `window.parent` whose `data.type` is `coppice.data`, and reads
`tasks`, `panes`, `sel` and `events` from them. `events` holds the
newest hundred `state`, `note`, `child` and `layout` events the floor page
heard, oldest first. A view is never given a pane's screen. Test its pure functions with
`node --test`, as `internal/web/static/_tests/views.test.mjs` tests the
tree view.
