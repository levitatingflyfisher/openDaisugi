# Use Pando R2, the conversation memory

Pando R2 keeps what was said. A verbatim log, a summary tree and a view sit in
one dir on disk. `daisugi pando` reads and writes it. No server needs to run.

## Where it lives

A store is the dir `<data home>/pando/<store>`. The data home is
`$OPENDAISUGI_HOME`, else the XDG data dir, else `~/.opendaisugi`.

There are two named stores: `work` (the default) and `home`. Any other name
that matches `[a-z][a-z0-9-]{0,31}` works too. Stores are separate on purpose.
Every command names one store with `--store NAME`. There is no `--store all`
and no search across stores. A `home` message cannot reach a `work` view,
because no code reads two stores at once.

Every command also takes `--data-dir DIR`, the data home that holds `pando/`.

## Read it

```sh
daisugi pando stores                    # the stores that exist
daisugi pando status                    # messages, nodes to build, messages not in the view yet, view size
daisugi pando log --day 2026-10-30      # one message per line
daisugi pando view                      # the summary lines a turn would see
daisugi pando zoom 0 4                  # open the node that starts at message 0 and holds 4
daisugi pando zoom 3 1                  # message 3, word for word
daisugi pando date 3                    # when message 3 was logged (UTC)
daisugi pando recall "sqlite index"     # ids and one line each; then zoom an id
```

A read command takes no lock. It sees every message a writer has already
synced, tended or not. The view is the last one a writer saved. Messages past
it are not in the view yet: `status` and `tend --dry-run` count them, and
`zoom ID 1`, `log`, `date` and `recall` read them word for word.
A store that does not exist gives `no store NAME at PATH` and exit 1.

These commands are for you, cron and your harness's MCP config. The warden
refuses `daisugi pando` (any verb) in a shell line that a pane agent or a
worker runs, and every read or write of a file under `pando/`. A worker never
reads the memory of its parent.

## Write it

```sh
daisugi pando append "we chose sqlite"          # a user message
daisugi pando append --kind note -  < note.txt  # standard input
daisugi pando tend                              # build the summary nodes that are missing
daisugi pando tend --dry-run                    # list them; no model call
daisugi pando tend --profile desk --max 200     # an agent profile, at most 200 model calls
```

One process writes a store at a time. A second writer gets
`another process has this pando store open` and exit 1.

`tend` asks a model for each missing node, one call at a time, oldest first.
It prints `nodes to build: N` before it starts. `--dry-run` prints it too.

`--profile NAME` picks an agent profile, for example a local model. With no
profile it runs Claude Code (`claude -p`, haiku). That is a paid call for each
node. A store you import old chats into can need thousands of them. So with no
`--profile`, `tend` stops when it would make more than 25 calls. It prints the
most calls it would make and does nothing else. Then pick one:

- `--profile NAME` to use an agent profile you chose.
- `--yes` to accept the default and pay for every call.

`tend` waits 120 seconds for one call. A slow model needs more: set `timeout_s` in its
agent profile (see [agent-profiles.md](agent-profiles.md)).

`--max N` makes at most N model calls this run, oldest first, across the store
and its child stores. Every call counts, and so does a re-ask: when the model's
line is over the limit, `tend` asks again, up to 5 calls for one node. A node
that runs out of calls keeps its shortest line so far, cut to the limit, or a
clipped copy of its source if there is none. A run with `--max 25` or less
never stops to ask. Run `tend` again for the rest.

While it runs, `tend` prints one line to stderr for each node it builds: the
level, the index, the seconds and the model calls so far. `-q` hides them. A
node is written to disk, and synced, the moment it is built, so a run you kill
loses only the node in flight. `daisugi pando status` shows the number of
clipped nodes.

When it ends, `tend` prints `built: N, fell back to clipped: M`. A clipped
node holds the start of its source text, behind `[clipped] `, instead of a
summary: the model gave no line for it. When M is more than 0, `tend` adds one
line that says why and how to fix it. The usual cause is a thinking model that
spends the whole reply on reasoning, so the reply has no text. Raise
`max_tokens` in the agent profile (it is 1024 when the profile names none; see
[agent-profiles.md](agent-profiles.md)) or use a model that does not think. Setting `thinking: off` in the profile
also stops most thinking models from spending the reply on reasoning, and
makes each call much faster.
If the first 5 model calls of a run all fall back, `tend` stops with exit 1
and says so, so a model that cannot work fills the store with 5 clipped nodes
and not thousands.

Build the clipped nodes again once the model is fixed:

```sh
daisugi pando tend --redo-clipped               # the same --max, --profile, --yes and --dry-run
```

It prints `nodes to build again: N` first. Then, oldest first, it asks the model
again for each clipped node, and for each node above one, because a parent's
line is made from its two children. A new line is added for a node; the tree
keeps the last line it holds for a node, so no file is cut. `--max` counts
model calls here too, and a run with the default model over 25 calls asks for
`--profile` or `--yes` first. A node that still comes back clipped is left as
it was. A parent may not be built again in the same run when `--max` stops it;
the next `--redo-clipped` finds it.

The 25-call limit counts the model calls the run can make, not the nodes. It
prints that count as `model calls: at most M`, under `nodes to build again`.
The count is an upper bound: a parent whose child is built again may need no
model call when the new lines fit together, and the plan cannot know that
before it runs.

A model never sees the `[clipped] ` marker. When a parent is made with a
model, both lines go to it without their markers, and a parent that falls back
to a clipped copy has one marker. A parent made by joining two lines has one
marker when either line is clipped.

To let a slower model write only the upper levels, or to compare two models on
the same nodes, build again by level:

```sh
daisugi pando tend --redo-level 2 --profile muse   # every node at level 2 and above
```

`--redo-level N` builds again every node at level N and above, lowest level
first, then oldest. Level 0 is the node of a single message. The run adds a
new line for each node, as `--redo-clipped` does, and takes the same `--max`,
`--profile`, `--yes` and `--dry-run`. With both flags, the run builds the
union of the two sets. A node whose new line is the same as the old one is
left.

A redo run that stops, on a failed call or on `--max`, goes on where it
stopped. It leaves a file `tree/redo.json` in the store. The file holds the
set the run picked, the `--profile` it used, and how many node lines the tree
held when it began. The next run with the same flags and the same profile skips
a node that has a newer line than that and whose line is not clipped. The
`nodes to build again` count shows only the nodes left. A run that goes through
every store removes the file. A run with other flags or another profile replaces
it and starts over. To start over with the same flags, delete `tree/redo.json`
first. A run for other flags or another profile that finds nothing to build
leaves the file as it is. A child store that is locked and skipped keeps its
file. A node whose new line equals the old one gets no newer line, so a
resumed run calls the model for it again.

The rule cannot tell a redo write from a write of the ordinary `tend`. A node
that `tend` builds after the file was written is skipped by the resumed redo.
To catch it, delete `tree/redo.json` and run the redo again.

The line count follows the order of the day files in `tree/`. The clock must
not be behind the newest one. Before a redo on a copied store, compare
`ls tree/ | tail -1` with `date -u +%F`.

Run it from cron on a box with no coppice.

A message is never out of sight. A tended view covers every message, with a
summary line if nothing finer fits. A message logged since the last `tend` is
not in the view yet, but every read command still finds it.

## Let a harness read it (MCP)

`daisugi pando serve --store work` is a stdio MCP server named `pando`. It has
read tools: `zoom(id, n, offset?, size?, child?, branch?)`, `date(id, branch?)`,
`recall(query, k?)` and `view()`. With `branch` the id is inside that branch
(see "Branches" below). With `child` (a worker's label, as its report names it)
the id and n are inside that worker's child store. Each call reads the store as it is then, so a
server that runs for days sees what was logged since it started. It has no write tool.

`daisugi install` registers it for you: in `~/.claude.json` for Claude Code, in
`config.toml` for Codex and in `config.yaml` for Hermes (the pando layer; see
"Keep it live" below). To register it by hand in one session, use
`claude --mcp-config pando.json` from your own shell:

```json
{"mcpServers": {"pando": {"command": "daisugi", "args": ["pando", "serve", "--store", "work"]}}}
```

The entry sits in the user config, so every Claude Code under that home starts
the server, a steward worker and a model call of daisugi's own included. Those
processes carry `DAISUGI_PANDO_SKIP=1`. The server then lists no tool and
answers every call with "The memory is closed to this process". A worker gets
no part of the household memory; it keeps only its own child store.

Its name is `pando`, so `mcp__pando__recall` does not meet the pathway tool
`mcp__opendaisugi__recall`.

## Keep it live

The pando layer of `daisugi install` is on by default (`--only pando` writes
only it; `--uninstall` takes it out). It writes four things:

- The MCP server `pando` (above), for Claude Code, Codex and Hermes.
- Four Claude Code hooks. `SessionStart` puts the view in the context of a new
  chat. `UserPromptSubmit`, `Stop` and `SessionEnd` log the chat. The three
  logging hooks run in the background, so a store that a model call holds never
  slows a prompt.
- A Hermes plugin, for the Hermes root and for each profile. It gives the view
  on every turn, because Hermes does not keep what a plugin adds to a turn. The
  text is the same bytes until the memory changes.
- A systemd user service, `daisugi-pando.service`, that runs `daisugi pando
  follow`. Install enables and starts it for your real home, and uninstall
  stops and disables it. `install --dry-run` says so and does nothing. A unit
  file that you wrote yourself is backed up first and never removed.

Set the agent profile that builds the summary nodes with `daisugi pando
profile NAME`. With no profile, follow only logs, and the nodes wait.
`daisugi config show` does not list this profile yet.

### Which chats enter the memory

Only chats that you (or your spouse, through Hermes) have with an agent
directly, and the steward. A worker the steward started, and a model call that
daisugi or coppice makes for its own work (the summarizer, the shell-history
split, envelope generation, the Python `claude -p` backend), are not your
chats. They carry `DAISUGI_PANDO_SKIP=1`. The hooks, the `context` verb and the
MCP server give them nothing, and nothing they say is logged.

A worker runs in the project, where your own chats run too, so the directory
cannot tell the two apart. The session id does. Two things put a worker's
session on the skip list, `<data home>/pando/skip.sessions` (one id or
transcript path per line, append only):

- Its own hooks. A hook that runs with the marker still reads its event, and
  adds the `session_id` and the `transcript_path` to the list. Follow, the
  importer and the hooks then drop that session.
- Its spawner. When coppice starts a worker it sets `DAISUGI_PANDO_SKIPLIST`
  to the list. Each `claude -p` call below it gets a fresh session id,
  writes the id to the list first and passes `--session-id`. The list holds
  the id before the transcript exists, so no poll can take its first line.

A pane that an agent creates is a worker too. When a pane, a plugin or an
unplaced peer asks the server for a pane (the Hermes steward starting a Claude
pane, say), the pane gets both variables in its environment, and they stay on
its layout record, so a resume or a restart keeps them. A Claude pane of this
kind starts with a minted `--session-id` that is on the list first. A pane that
you start from the floor, the phone or a plain terminal is your chat and is not
marked. One edge: if you type `coppice pane create` in a shell inside a coppice
pane, coppice sees a pane as the caller, and the new pane is marked.

Follow also skips any Claude Code session whose directory is under the coppice
data dir. A worker's transcript stays in its own child store. A line that
follow logged before the session was listed stays in the log: the log only
grows. A hook adds a session to the list once, not on every turn. When a spawner names
its own list in `DAISUGI_PANDO_SKIPLIST` (coppice does so under a `--data-dir`
of its own), the hook lists the session there too.

Hermes `delegate_task` children are workers too. They are sessions in
`state.db` with `source = 'subagent'` (or a `_delegate_from` mark in
`model_config`), and follow and the importer log none of their rows. Known gap:
whether Hermes calls the plugin for a child's turns, and so gives the child the
view, is not checked yet. It needs one test on the box with a logging plugin on
a delegated task. Also, nothing here starts Hermes as an internal model call; if
that ever changes, the call needs the marker.

The steward is logged once. A steward that coppice runs itself (the chat mode)
is logged by coppice. A steward in a pane on Hermes is logged
by the live feed from Hermes' `state.db`: in pane mode coppice opens no chat and
writes nothing to the store. A Claude Code pane steward runs under the steward
dir, which follow reads: its own hooks log it, as an ordinary chat (user and
assistant lines, not steward lines), and follow dedups against them.

### What a new chat sees

The view lists the tree lines. After them comes "Not in the view yet": the
newest 20 messages that have no node yet, word for word, 300 bytes each. They
come from every chat in the store, so a line that your spouse wrote in
Mattermost can reach the model of the next chat you start, and a Hermes
turn. That is the one-tree rule. It is also why a worker gets no view.

Not wired: OpenCode and OpenClaw have no pando hooks or plugin. Codex gets the
MCP server only, because its hooks fail open and cannot add context.

## Worker transcripts

A worker that coppice starts keeps its whole transcript as a small store of
its own, `<store>/children/<worker label>/`, with the same layout and the same
512-byte node rule. The worker's report lands in the steward's log as one
`work` message. That message names the child store and its root ids. In
coppice, `zoom` with `child` set to the label goes down from there to the
word. From a shell: `daisugi pando zoom 0 1 --child LABEL`. `tend` builds the
child trees too. A child lives under the store that started it. A worker of a
branch keeps its transcript under that branch, `branches/<branch>/children/<worker>/`:
zoom it with `branch` and `child` (`--branch trellis --child trellis-1`).

## Branches: long-running agents

The Rust and Python ports lag the Go build for branches after round 8b fix 3 (the owner's claim rule, the lead clone); the Go `daisugi` and coppice are the ones to run for them.

A long-running agent, such as a project lead the steward starts, gets a
branch: its own part of the one tree. A branch is a store inside the store,
`<store>/branches/<label>/`, with the same layout. A lead's own long-running
agents are branches of its branch: `<store>/branches/trellis/branches/ui/`.
A path such as `trellis/ui` names a branch from the store. See
[ADR-0029](../adr/0029-pando-branches.md).

### Which chats go where

| Who | Where its chat goes | What it sees |
|---|---|---|
| You, your spouse (Hermes), the steward | the store | the store and every branch |
| A long-running agent (`DAISUGI_PANDO_BRANCH=trellis`) | its branch | its branch and every branch below it |
| A worker of a branch (`DAISUGI_PANDO_BRANCH` and `DAISUGI_PANDO_SKIP`) | its transcript, under the branch: `branches/trellis/children/<worker>/` | recall, zoom and date over the branch and below; no view |
| A worker with no branch (`DAISUGI_PANDO_SKIP`) | its transcript, `children/<worker>/` | nothing |

The steward starts a long-running agent with `spawn` and `long_running:
true` (and `branch`, which defaults to the project name). coppice opens a
Claude pane in the project with the brief as its first prompt. A long-running
agent can start another one below its own branch (`pane.create` with
`long_running` and `pando_branch`). A pane that you start yourself is at the
root, as before.

A branch's chat never goes into the log above it. The store's session map,
`<store>/branch.sessions`, names the sessions of each branch. Before a lead
starts, coppice writes a dir route for the lead's clone; the route alone
maps the lead's sessions, and no session id is written for it. The follow
service, your own hooks and the importer send a mapped session to its
branch.

The folder decides. A session whose working dir is under a lead's dir route
is that lead's branch, whatever else the map says. A lead's own hooks write
no id line for a session there: the route already maps it. They still write
one for a session outside the route, and the Hermes plugin writes one for a
Hermes session, which has no dir.

The marker (`DAISUGI_PANDO_BRANCH`) stays on a lead. It is not what routes
its chat; the folder does that. It is how the lead's own processes know
their branch: a fenced lead can read its branch and nothing above it, so it
cannot read the session map to learn its route. The marker scopes its view,
`recall` and `zoom` to its branch. coppice reads it too, from the pane's
record, to grant the branch in the fence and to place the lead's own
workers and leads below it. Without it, an unfenced lead would see the whole
store.

Your own chat's SessionStart claims its session for the store once: two
lines, the session id and its transcript path. A resume or a compact of the
same chat adds no line.

### Read a branch

```sh
daisugi pando branches                         # every branch: path, state, messages, newest line
daisugi pando view --branch trellis            # the branch's own view
daisugi pando zoom 0 4 --branch trellis/ui     # a node inside a branch
daisugi pando recall "epub parser"             # the store and every branch; a hit in a branch reads trellis:12+4
```

From the store, `recall` finds lines in every branch, and each hit names its
branch: `trellis:12+4` is the node `12+4` of the branch `trellis`. Zoom it
with `--branch trellis` (or `branch: "trellis"` in the MCP tool). Your view
(`context`, `view` and the steward's), ends with one line for each live branch: its label and the
summary of its newest root node.

A process in a branch sees its branch and everything below it, never the
store above it or a sibling branch. `branch` in its tools is a path below its
own. A bad, missing or linked marker gives a closed server and no view. Its
`daisugi pando` refuses `stores`, `append`, `tend`, `import`, `migrate`,
`follow` and `close`.

### Close a branch

```sh
daisugi pando close trellis
```

A closed branch keeps every message, and `recall` from above still finds it.
The view above no longer names it. A branch is never deleted. A message, a
worker's transcript or a Hermes row goes into a closed branch and leaves it
closed. Only a new long-running agent opens it again.

### Read and fix the session map

```sh
daisugi pando routes                 # every session or dir:PATH and its branch
daisugi pando unroute SESSION_ID     # take a line off; the next lines go to the store
```

Your own chats claim their sessions for the store at SessionStart (a line
`<session>` and `-`), so no branch can map them later. The map is trusted input. A line that would map a session the store already
holds is refused, and such a line is not followed if someone wrote it by
hand: a branch cannot take one of your chats. Lines already logged stay
where they are. Both verbs are refused for a process with a marker.

### A lead's clone

In a git repo, `spawn` with `long_running` starts the lead in a shared clone
of its own, `<repo>-worktrees/lead-<branch>`, on a branch of that name. Its
Claude project dir is its own, and its git never writes your repo. Bring its
work over from your own repo with `git fetch <clone>
lead-<branch>:lead-<branch>`; the spawn answer gives the line, and a later
clone of the same lead starts from that branch. A lead's own nested lead gets a
clone of the same repo of yours. coppice reads nothing of the parent's clone:
the nested lead starts from the parent's branch only when you have fetched it
into your repo with the line above, else from your repo's branch. The steward
is told to ask you to run that fetch first. Fetch the nested lead's work from
your repo the same way. coppice maps the clone's dir to the branch (`dir:<path>` in
`daisugi pando routes`): every Claude session whose working dir is under it, a
`claude -p` the lead runs included, goes to the branch. In a project that is
not a git repo, the lead runs in the project and the spawn answer says so. A
clone that cannot be made (a dir in the way, a git error) refuses the spawn.
Do not work in `<repo>-worktrees/lead-*` yourself. Run no git command inside
a lead's clone: its git config, hooks and aliases are the lead's, and git
would run them as you. The fetch above, run in your repo, runs nothing of the
clone's. A Claude chat you start inside the clone goes to the lead's branch,
not to your store, because its dir belongs to the branch.

### What the fence does with branches

The MCP scoping is a soft wall: a process that can read the files can read
past it. The Landlock fence is the hard wall. It shuts all of `pando/` to a
fenced agent. A fenced pane with a branch gets its branch dir read only
(`--allow`), so the kernel lets it read its branch and below and nothing else
of the memory: not the store's log or view, not a sibling branch, not a
listing of `branches/`. The fence cannot let a branch write, because the
data home is the warden's state. So a fenced agent's hooks only read, and the
follow service logs its chat from outside the fence.

The fence grants of `~/.claude` only Claude's config, `statsig` and the
login token, and of `~/.claude/projects` only the workspace's own dir. So a
fenced agent cannot read the Claude transcripts of your other projects, your
prompt history (`history.jsonl`), or other sessions' todos, plans and file
history. It can still read `~/.claude.json`.

What this does not cover:

- An unfenced agent can read every file of its user, the store above included.
- A fenced Hermes long-running agent cannot map its session from its plugin.
  Its rows go to the store until the session is mapped. coppice starts
  long-running agents on Claude only.
- A fenced lead in a git repo has a dir route, so a fork's new session goes to
  its branch at once. A fenced lead outside a git repo that is forked logs its
  first lines in the store until coppice learns its new session id.
- A fenced lead reads its repo's objects (its clone borrows them) and nothing
  else of your repo: not its config, hooks or refs.
- A sprig tree worker has no MCP client, so it gets no branch tools. Its
  transcript still goes under its branch.

## Bring old chats in

See [import-old-chats.md](import-old-chats.md). Add `--tend` (with `--profile`, `--max` and `--yes`) to build the summary nodes at once instead of running `tend` after. The same 25-call limit applies.

## Move the old coppice log

Before this change, coppice kept the log in `<data home>/foreman`. Run:

```sh
daisugi pando migrate
```

It copies `main/`, `tree/`, `view.json` and `import.json` to
`<data home>/pando/work`, opens the copy, checks it holds the same messages
and the same view, and then renames the old dir to `foreman.moved-<UTC stamp>`.
It deletes nothing. Stop coppice first: with the old dir open it says
`coppice has this chat open; stop it first` and copies nothing. A second run
says `already moved: PATH` and exits 0. The steward's own files (turns, calls,
failover, queue) stay in the moved dir, and coppice copies them back on its
next start.

## Python

The Python package `opendaisugi.pando` reads stores and serves the same MCP
tools. It never writes. It does not read branches: for a process with
`DAISUGI_PANDO_BRANCH` or `DAISUGI_PANDO_SKIP` it serves no tool and its read
verbs exit 1. `daisugi pando append`, `tend`, `import` and `migrate`
in the Python CLI print that and exit 2; use the Go or Rust binary.
