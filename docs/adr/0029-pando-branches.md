# ADR-0029: Pando branches: one tree, shaped like an org chart

- **Status:** Accepted (owner rulings of 2026-11-06)
- **Date:** 2026-11-06
- **Builds on:** [ADR-0023](0023-pando-r2-in-daisugi.md) (Pando R2 in daisugi) and its
  child stores (PD-R-11).

## Context

Round 7 made the store live: every chat the owner has with an agent goes into one store as it
is said. A worker the steward starts is not the owner's chat. It carries
`DAISUGI_PANDO_SKIP`, gets no view, and keeps its transcript in a child store.

A long-running agent is neither. A project lead that the steward starts and that runs for days
is not the owner's chat, and it is not a short worker. The owner ruled (2026-11-06):

> The long runner memory should be visible to those above them, it should be part of the
> tree ... the worker can access everything below a certain point but not above it.

## Decision

1. **A branch is a store inside a store.** A long-running agent gets a branch:
   `<store>/branches/<label>/`, with the same layout as a store (`main/`, `tree/`,
   `view.json`, `lock`) and a `branch.json` that marks it and holds its state (`open` or
   `closed`). A lead's own long-running agents are branches of its branch:
   `<store>/branches/trellis/branches/ui/`. A path names a branch from the store:
   `trellis/ui`. A path has at most 8 levels; each label is a child label
   (`[A-Za-z0-9][A-Za-z0-9._-]{0,63}`).
2. **The marker names the branch.** `DAISUGI_PANDO_BRANCH=<path>` in a process's environment
   is its branch. With no marker, the process is at the root of the store, as before.
3. **Live messages go to the branch, never above it.** The hooks of a process with the marker
   log into its branch and give the branch's view. The session map,
   `<store>/branch.sessions` (one `id<TAB>branch` line, append only, the first line for an id
   wins), tells the follow service, the owner's own hooks and the importer which sessions
   belong to a branch. coppice writes a lead's session id there before the lead starts
   (`--session-id`), so even the first line of its transcript goes to the branch. The Hermes
   plugin passes its session id to `daisugi pando context`, which maps `hermes:<id>`.
4. **A process sees its branch and everything below it.** `serve`, `context`, `hook` and the
   read verbs of a process with the marker treat its branch as the store. `recall` searches
   the branch and every branch below it; `zoom` and `date` take `branch`, a path below the
   caller's own. `..`, `.`, empty parts, absolute paths and symbolic links at any level are
   refused. A marker that is bad, a link, or names a branch that is not there gives a closed
   server and no view. It never falls back to the store above.
5. **Anyone above sees down.** From the root (the owner's chats, the steward), `recall`
   searches the store and every branch, and each hit names its branch
   (`trellis:12+4`). `zoom` and `date` take `branch`. The view ends with one line for each
   live branch right below it: the label and the summary of its newest root node. A closed
   branch keeps its messages and stays searchable; the view no longer names it.
6. **Short workers.** A worker with a branch carries both markers. It gets `recall`, `zoom`
   and `date` over its branch and below, no `view` tool, and no view pushed. Its hooks log
   nothing. Its transcript is a child of the branch: `<branch>/children/<worker>/`. A worker
   with no branch keeps today's closed memory.
7. **Spawning.** The steward's `spawn` tool takes `branch` and `long_running`.
   `long_running: true` starts a Claude pane in the project with the brief as its first
   prompt and the marker in its environment (the branch defaults to the project name). A
   long-running agent may start another below its own branch (`pane.create` with
   `long_running` and `pando_branch`); any other pane an agent starts is a worker of the
   caller's branch. The marker rides on the layout record, so a resume, a restore and a fork
   keep it. Operator panes are unchanged: they are at the root.
8. **Tend and follow build branch nodes** in the background, with the same summarizer and
   rules as the store.
9. **PD-R-2 stands.** A branch lives inside one store. No code reads two stores.

## The fence and the soft wall

The scoping in the MCP server is a soft wall. A process that can read the store files can read
past it. What closes the files is the Landlock fence.

The fence shuts all of `<data home>/pando/` to a fenced agent (ADR-0028 and the fence how-to).
For a fenced pane with a branch, coppice adds `--allow <branch dir>`, read only. Checked on this
kernel (scratch HOME, 2026-11-06): the fenced process reads its branch and the branches below
it; it cannot read the store's `view.json` or `main/`, cannot list `pando/work`,
`branches/` or `children/`, cannot read a sibling branch or a worker child of the store, and
`branches/trellis/../other` is shut too. The same holds when the data home is a symbolic link.
The lead's own pando commands were run inside the same fence: `pando serve` answers recall,
zoom and view from the branch and refuses `branch: ".."`; `pando context` gives the branch view;
`pando hook` SessionStart prints it and Stop returns at once (about 10 ms) and writes nothing.
No file under `pando/` changed during the fenced run.

The fence cannot give a branch write access: the data home is the warden's state, and
`--allow <branch>:rw` is refused. So a fenced agent's own hooks only read. Its chat is logged
by the follow service and by coppice, outside the fence. Its session must be on the map before
it starts; coppice does this for the Claude panes it starts, and for a session it learns later
(a resume or a fork).

**The raw chats (fixes 1 and 2).** The fence used to grant `~/.claude` read only as a whole, so
a fenced lead could read every Claude transcript under `~/.claude/projects`, which are the
owner's chats. The fence now grants of `~/.claude` only Claude's config (settings, CLAUDE.md,
agents, commands, skills, hooks, output-styles, plugins, keybindings, local), `statsig`
and the login token, and of `projects/` only the workspace's own dir. The owner's prompt history
(`history.jsonl`), other sessions' state, file history, todos, plans, debug logs, shell
snapshots, session env and IDE locks stay shut. Checked on this kernel with a scratch HOME: a
fenced `claude -p` runs a Bash tool turn and a Write tool turn without them (the rewind file
history and shell snapshots of a fenced session are then not saved). A workspace whose Claude
project dir the fence cannot name (a path over 200 bytes or not ASCII) is refused, unless
`--allow ~/.claude/projects:rw` names the whole dir.

What a fenced lead can still read of the owner's: `~/.claude.json` (the owner's name and email
in `oauthAccount`, every project path, per-project settings, the last session id of each
project), the login token (`.credentials.json`, read and write), and the config named above.
The copies of `~/.claude.json` in `~/.claude/backups` stay shut. The last session ids cannot be
used to take a chat: see "The owner claims its own sessions".

**Git: a lead works in a clone (fix 3).** A linked worktree keeps its objects, refs, config
and hooks in the owner's `<repo>/.git`, and git in the worktree must write that dir. Landlock
cannot cut `config` and `hooks` out of it, and a lead runs for days, so a lead could set
`core.fsmonitor` or a hook that the owner's next `git status` runs outside the fence (the
workspace watch hashes them only when a pane starts and ends, too late for a lead). So a lead
gets `git clone --shared` of the repo instead (see "A lead runs in its own clone"). The fence
grants the clone read and write and the object stores it borrows read only, and nothing else of
the owner's repo. coppice names those stores on the owner's side with `git rev-parse
--git-path objects` (so a project that is a linked worktree resolves to its main repo's store)
and keeps the repo and its store on the lead's record. A lead's nested lead clones that recorded
repo, never a dir the caller names, and coppice reads nothing of the parent's clone at all: the
nested clone starts from `origin/lead-<parent>` when the owner has already fetched the parent's
branch into the repo (`git fetch <parent clone> lead-<parent>:lead-<parent>`), else from the
repo's branch. The steward's spawn answer tells the steward to ask the owner to run that fetch
before a lead starts a nested lead. So every lead is granted only the owner's repo's store, and
no path inside a lead-written tree is ever asked about, fetched from or granted; that is why J6
is closed for nested leads. The store must be the repo's
`<common dir>/objects` with no link on the way, checked when the lead is made and again when its
pane starts; a link refuses the spawn or the pane, never a silent skip. Checked on this kernel: a
parent that points its clone's `objects` at `~/.ssh` gets a nested lead whose git works and that
cannot read `~/.ssh` (that check ran a fetch from the parent's clone, which coppice no longer
runs; the grant is the same). A store git cannot name refuses the spawn.
The grant is the whole store, dangling objects included. Checked on this
kernel: inside the fence `git add`, `commit`, `switch -c`, `switch`, `branch -D`, `config` and
`gc` in the clone all succeed, and a write to `<repo>/.git/config`, to `hooks/`, to
`refs/heads/main` or into `objects/`, and a read of the config or of the owner's files, are all
denied (also checked: a lead of a linked-worktree project, and a nested lead whose clone borrows
its parent's). Outside, the owner brings the work over with `git fetch <clone>
lead-<branch>:lead-<branch>` from the owner's own repo, which runs nothing of the clone's. The
clone's git config, hooks and aliases are the lead's: the owner runs no git command inside it. The fence
no longer grants a linked worktree's git dirs: a fenced pane in a linked worktree cannot run
git, as before fix 2. A `--shared` clone borrows the repo's objects: a history rewrite plus
`git gc` in the owner's repo can break it.

coppice's own git calls (making a lead's clone, a task's worktree) run with
`-c core.hooksPath=/dev/null -c core.fsmonitor=false`, so no hook or fsmonitor command of a repo
an agent wrote runs as the owner. A `filter.<x>.smudge` in such a repo's config is not covered.

**A lead runs in its own clone.** Its Claude project dir is named from its working dir. A lead
in the owner's project dir would share that dir, and the owner's chats in that project, with the
owner. So in a git repo `spawn` with `long_running` starts the lead in a shared clone of its
own, `<repo>-worktrees/lead-<branch>`, on a branch of that name, made once and used again. The name keeps a-z and 0-9 and
writes any other character as a dash and two hex digits, so `trellis/ui`, `trellis.ui` and
`trellis-ui` get three clones. A new clone starts from the lead's branch when the owner's repo
has one (fetched from an earlier lead), else from the repo's current branch. A clone that
cannot be made, or a dir in the way that is not a clone of the repo, refuses the spawn: the lead
never runs in the owner's dir in its place. A lead's own nested lead (`pane.create` with
`long_running`) gets a clone and a dir route the same way. A pane with a dir route gets no
minted session id: the dir route covers every session in it. coppice also maps that dir to the
branch (`dir:<path>` on the session map), so every Claude session whose cwd is under it, a
`claude -p` the lead runs or a model call, goes to the branch, never the store above. A project
that is not a git repo runs the lead in the project itself, and the spawn answer says so.

**The owner claims its own sessions.** The owner's unmarked SessionStart hook writes
`<session>\t-` on the map: the session is the store's. A lead's hooks carry the marker, so an
unmarked session is the owner's: its claim wins over a branch line written for it before (a
lead that read the id from `~/.claude.json`, say). A session whose cwd is under a branch's dir
route is that branch's, decided once for the whole event: no hook claims its id or its
transcript path, so the hooks and the follow service log all of it into the branch, a Claude
session the owner starts inside a lead's clone included. coppice maps an id a pane reports only
when the pane has no dir route and the id's transcript is in the pane's own Claude project dir.
Do not work in `<repo>-worktrees/lead-*` yourself. Soft wall: an unfenced lead that drops its
marker writes into the store as the owner.

**The session map is trusted input.** A line that would map a session the store already holds
is refused, and such a line is not followed if it is there: a forged SessionStart or
`context --session` from a branch cannot take the owner's chat. The owner lists the map with
`daisugi pando routes` and takes a line off with `daisugi pando unroute ID`; both are refused
under any marker.

**A closed branch stays closed.** A message, a worker's transcript or a Hermes row goes into a
closed branch without opening it. Only a new long-running agent opens it again.

What the wall does not stop:

- An unfenced agent can read any file its user can read, the store above it included. The
  scoping is then only the MCP server's and the warden's (the warden refuses `daisugi pando`
  and file reads under `pando/` in an agent's shell line).
- A fenced Hermes long-running agent cannot write the session map from its plugin, and coppice
  cannot mint a Hermes session id. Its rows go to the store above until the session is mapped.
  Today coppice starts long-running agents on Claude only.
- A lead's worktree dir route covers a fork's new session too. A lead outside a git repo has
  no dir route: a fenced fork of it can have its first lines logged in the store above until
  coppice learns its new session id.
- A fenced lead in a project that is not a git repo shares its Claude project dir with the
  owner's chats in that project, and can read them.
- A sprig tree worker has no MCP client, so the read-only branch tools reach only workers that
  are Claude Code panes. A sprig worker carries the marker, and its transcript goes under the
  branch.

## Consequences

- **Buys:** a long runner's memory is part of the one tree, visible from above, and closed
  above and to the side. The root view says what each branch is doing in one line.
- **Costs:** a session map to keep, a third marker variable, and a recursion in tend, follow
  and recall. Recall from the root reads every branch on each call.
- **Forecloses:** a branch that sees its parent. A lead that needs a fact from above gets it in
  its brief.

## Rulings (PD-R-14 to PD-R-17, in `clients/ADJUDICATIONS.md`)
