# coppice

One static binary that owns the panes, the agents in them, and their state. It is the client
and the server: the first invocation starts a background server as you, and every later one
talks to it over a private socket.

coppice mirrors Herdr's verbs on purpose, so a Herdr user drives it from muscle memory. It
competes on one axis: **state comes from the gate, not from the screen.** When openDaisugi's
gate blocks a tool call, "blocked" is a fact we produced, with the tool name and the clause
attached. Screens are the fallback for agents with no hook, and the floor says `unknown` rather
than guessing `idle`.

Everything here was written by an AI assistant. Verify before you rely.

## Build

You need Go 1.26.8, Zig 0.16, CMake, pkg-config, uv, curl and git. `curl` and `git` ship on most
Linux boxes already; `uv` and `pkg-config` may not.

```
./scripts/toolchain.sh
./scripts/preflight.sh
../../scripts/install.sh
```

The repo root's `scripts/install.sh` builds coppice, sprig and the Go `daisugi`, and puts all three
in `$XDG_BIN_HOME`, or `~/.local/bin`. It builds its own pinned `libghostty-vt` (ReleaseFast,
baseline CPU) in the native prefix that `clients/go/scripts/native.sh` makes, so it does not need
the one `toolchain.sh` builds. With `--link` it links the builds instead, so a rebuild is live at
once. `scripts/install.sh` here only calls it. Then `daisugi install --gate` wires the gate, and
`coppice` opens the floor in the terminal, and `coppice web` opens it in the browser. The install
stamps each binary with the checkout's `git describe`; a plain `go build` reports the commit.

`toolchain.sh` installs Zig and CMake under `~/.local`, no sudo, and builds `libghostty-vt` from
source into `~/.local/ghostty-vt`. Override that destination with `COPPICE_GHOSTTY_PREFIX`. It
also writes a pkg-config wrapper at `~/.local/bin/coppice-pkg-config` and two global Go settings,
`GOTOOLCHAIN=auto` and `PKG_CONFIG=coppice-pkg-config`, so a plain `go build` links. Undo both
with `go env -u GOTOOLCHAIN PKG_CONFIG`.

There is one thing the installer cannot give you: pkg-config itself. If `preflight.sh` reports it
missing, install `pkgconf` or `pkg-config` from your distribution's own package manager. That one
step needs sudo; nothing else here does, as long as `COPPICE_GHOSTTY_PREFIX` points somewhere you
can already write.

Pins and their reasons, including the unix-only build, are in `PINS.md`.

## First run

Run `coppice` with no arguments. The first time, it looks for `claude`, `codex`, `pi`, `sprig`,
and `opencode` on PATH. One found means no question. Several found means one numbered question.
None found means one honest line: Codex Desktop, Cursor, and Antigravity are apps, and coppice
cannot own their panes. The answer goes into `~/.config/coppice/coppice.toml`, or under
`$XDG_CONFIG_HOME` when that is set. Edit that file and save; the next pane reads it.

```toml
default = "claude"

[harness.claude]
command = "claude"
args = []
state = "hooks"
resume_args = ["--resume", "{session}"]   # pane resume, see Recent below
```

The first run writes `resume_args` into the `claude` table. When the daisugi gate hook runs
in a Claude pane, it reports that pane's session id, so the pane resumes from Recent where
it stopped. With no hook, the pane starts fresh.

`gateway = "http://127.0.0.1:8787"` at the top of the file names the daisugi token-saving
gateway. That is also the address the floor uses when the key is absent. A pane whose
`ANTHROPIC_BASE_URL` or `OPENAI_BASE_URL` points there shows its router as `gateway`.
`gateway = ""` says there is no gateway.

On the floor, Enter opens the default harness in the current directory. From a shell,
`coppice claude` does the same and attaches; `coppice open claude --resume` is the long form,
and any words after the name are appended to the configured args for that one pane.

The floor is one screen: the rail of agents on the left and windows on the right, each window
one agent live. There are as many windows as fit at 80 columns each beside the rail: one at
120 columns, two at 200. When the floor opens, the windows fill with the agents that need you,
then the working ones, then the rest, and the keys start in the first agent that needs you, or
else the first window. On a screen too narrow for windows the keys start on the rail. They
never start on the prompt line.

At 100 columns or wider, a dim row under the top bar shows the floor's facts, the same ones the
web page's header shows: the agents by daisugi mode, the gateway, how many work, how many need
you, and the tokens used today. The working and needs-you counts come from the rail itself, so
the two never disagree.

The window that has the keys has a thick accent border and says `typing here · ctrl-space
leaves` in its header, and the terminal cursor sits at that agent's own cursor. Every key goes
to that agent as you type it, Enter, Esc, Tab, the arrows and `ctrl-c` too. The leave key
(`[keys] leave` in `coppice.toml`, `ctrl-space` by default), or a click on the rail, gives the
keys back to the rail; `ctrl-c` there quits. A click on another window moves the keys there. A
window attaches its agent at the window's own size and resizes it when the window changes, the
way tmux does; a new agent starts at its window's size. When the screen narrows, the typing
agent moves into a window that still shows. A double click on a window, or the prompt word
`zoom`, attaches an agent full screen: the typing one, or the row under the cursor. When the
terminal cursor would fall outside the window, it hides.

A headless agent (sprig, codex, opencode, and claude in stream-json mode) takes whole messages,
never raw keystrokes, so its window works differently. Its last line is its own input line: `This
agent takes whole messages. Type and press Enter.` while it holds nothing, or the line typed so
far, marked with `> `. Printable keys, Backspace, the left and right arrows and `ctrl-u` (clear)
edit that line; the terminal cursor sits in it, scrolled to stay on screen once the line is wider
than the window. Enter sends it as `agent.prompt` and clears it, unless a paste left another byte
already behind it: that Enter becomes a space instead, so the paste's own lines join with one
rather than run together. A refusal shows as the message line and the words typed stay, since
the agent never got them. The leave key gives the keys back to the rail, as it does for a pty
window; Esc does too, but only here - a pty window has no line of its own for Esc to leave, so
there it is just another key the pane gets. A voice clip's text lands in the line too, never on
the wire, so the owner reads it before Enter sends it. The window still shows the agent's recent
output above that line. A double click, the prompt word `zoom`, and `coppice attach` all attach a
pty full screen; on a headless pane none of them do, since attach only ever forwards raw
keystrokes. A double click or `zoom` there just gives its window the keys again, or says why not
when the screen has no room for one, and `coppice attach` on one prints one line pointing at the
floor or `coppice agent prompt` instead.

The rail is a tree. With tasks, each task has one header with its state counts, as `▾
trellis-fix  1 needs · 2 working`, and its agents under it. Agents with no task group by
project, the base name of their directory, after the task groups. A task with no live agent
has no header. Groups and agents keep the order they were made in, so neither a header nor a
row moves when a state changes; the bar's count, an amber header and `shift-tab` show who needs
you. Enter or Space on a header folds the group to that one line.

On the rail the keys are these, and the footer prints them, wrapped to fit. `Enter` goes in:
the row's agent fills a window and takes the keys, or on a narrow floor it attaches full screen.
`1` to `9` put the row's agent in that window; each row shows its window's number. `n` starts
the default harness near the selected row, in that row's directory, and `N` opens the project
picker, where a number or Enter starts one in that project. `r` renames the row on the prompt
line: it starts with the old label, Enter keeps the new one, and Esc keeps the old.
`ctrl-w` asks `Stop <label>? Enter stops it, Esc keeps it.` and Enter stops the agent. The ×
at the end of every row and window header asks the same. `shift-tab` goes to the next agent
that needs you. `Space` peeks at the row, or on a floor with windows fills one. `ctrl-t` opens
the prompt line, where Enter runs the line. `ctrl-\` records a voice clip for the row's agent
(see Voice); the footer names it when there is room. `Esc` goes up one level: it closes the tree, closes
the peek, lets the prompt go, or pops the rail out of one task. Up and Down move the cursor.
One click on a row fills a window and gives it the keys; press on a row and let go over a
window to drag the agent into that window. Other keys only say how to type a line.

An agent that ends on its own leaves the rail and its window, and the floor says `<label>
ended (exit N)`, or `ended (killed)` when a signal ended it. A non-zero exit draws amber and
stays until you click it or open Recent. When the agent had the keys, or was attached full
screen, the floor brings you back to the rail with the cursor on Recent, and says `<label>
ended. Keys paused.` for 750 ms: keys typed then are dropped, so type ahead meant for the agent
never acts on another one.
The Recent fold at the foot of the rail, closed by default, shows its count. Enter opens it: it
lists the ended agents with `Resume all` and `Clear all`. Enter on an ended row resumes it,
and `ctrl-w` forgets it. A window's header shows a second line with the agent's loop, model,
router, daisugi mode, last gate verdict and tokens, when the server knows them.

A task is the unit of work above the pane: a label, a parent or none, a directory or a git
worktree, and a model name or none. A task with children is a team. `coppice task create
--label gate-refactor --cwd . --worktree` adds a worktree at `<repo>-worktrees/gate-refactor`
on a branch of that name, and `coppice pane create --task t1 -- claude` opens a pane that runs
there. A task's state is the worst state under it, so a team shows `needs you` when any pane
in any of its tasks does. Press `ctrl-t` and type `tree` to see the tasks as a text tree in place
of the roster. Enter on a task pushes the roster into it: the roster shows only the panes of
that task and its children, and the bar names the path, as `coppice › review-team · 1 needs
you`. The count is the count inside the task. `Esc` pops back one level. A tree opened while
pushed shows only that task's subtree.

The floor has a foreman too: one agent you talk to about all your projects. Type a sentence at
the prompt line (`ctrl-t`), type or say it on the phone's tell bar, or run `coppice floor talk
TEXT`. If no foreman runs, the first sentence starts one: your default harness, labelled
`foreman`, in its own scratch directory `$XDG_STATE_HOME/coppice/foreman` (or
`~/.local/state/coppice/foreman`), which never counts as a project. That directory is outside the
coppice config and data directories on purpose: the daisugi gate refuses every call made from
inside those. The server tracks the foreman by its pane id, kept in `foreman.json` in the data
dir, and only you may give a pane the label `foreman`. Other panes can still type into the
foreman; that grants them nothing, and its page tells it such text is data, not your orders. The server pastes the foreman page into
it (or, for a harness without bracketed paste, types the line that tells it to run `coppice
skill foreman`), waits for that turn to end, and then types your sentence. Sentences that come
while it starts wait in one queue and go in order, so two talks never start two foremen. While
the foreman waits on a question, your sentences wait too, and the floor says so. When the
foreman has ended, the next sentence starts a new one and the floor prints one line that says
so. The foreman works through the same commands you have: `coppice project list`, `coppice new
PROJECT --no-attach`, `coppice agent prompt`, `coppice pane read` and `coppice pane close`, and it
reports one line per agent. The gate stays outside it: the floor's foreman holds no asks, and
every ask stays yours. Type `foreman claude` at the prompt line to start one with a harness of
your choice instead; the server starts it and gives it the page the same way. Type `foreman`
alone to see which pane it is, or that none runs yet. At most 32
sentences wait for it.

A task can have a foreman: a pane that hears the asks of the task's panes before you do. Push
into the task and type `foreman claude` at the prompt line to open one there, or run `coppice task set-foreman t1
--pane w1:p3`. An ask that can be undone goes to the nearest foreman first. The foreman gets a
note, and the row waits under WORKING as `waiting on review-team's foreman · 1m`. The foreman
may deny the ask. It can never allow one. If it does nothing for 120 seconds, the ask comes to
you in NEEDS YOU. The hold ends 30 seconds before the gate stops waiting, so with the gate's
default 90 second wait the foreman has 60 seconds. When the foreman's harness reports the end
of a turn, the server types one line into it with a hold id, the pane, and the get and deny
commands, never the ask's own text, and at most one line per turn. It never types into a
foreman that is working or blocked, nor into one that holds text someone typed without Enter,
nor before a turn ends after the last line someone submitted. A foreman whose harness never reports a turn end gets the note only. A pane may not move a task,
and a new foreman, a cleared one, or a move sends the old foreman's holds to you at once. An ask that cannot be undone comes to you at once, and the foreman still gets
the note. A foreman that is blocked or closed holds nothing.
`coppice task list --tree` prints the same tree. `coppice task close t1` closes every pane
under the task and removes its worktrees, and refuses when one has uncommitted changes unless
`--keep-worktree` is passed. Branches are never deleted.

## Use

```
coppice server start
coppice pane create --cwd . --label "auth fix" -- claude
coppice pane list
coppice attach w1:p1
```

Every command that talks to the server brings one up if nothing is listening, unless
`COPPICE_NO_AUTOSTART` is set. `coppice server start` itself detaches and returns once the new
server answers; running it again when one is already up is a no-op, not an error. To watch the
server in your terminal instead of detaching, run `coppice server start --foreground`.

`coppice attach` and `coppice server stop` are the two exceptions: neither autostarts, with or
without a pane id for attach. If nothing is listening, `coppice attach` tells you to start a
server first, and `coppice server stop` prints `no server is running` and exits 0 - its whole
purpose is making sure none is, the same reasoning that makes `server start` idempotent.

Add `--json` to any `server`, `workspace`, `tab`, `pane`, `agent` or `session` command to get one
JSON object instead of a table or a line. `pane list` prints panes as a table by default; `--json`
gives you the same data a script can parse. `coppice attach` renders a live terminal. It has no
`--json` flag: its first argument is always a pane id.

Global flags, before the verb: `--socket PATH` and `--data-dir PATH` override where coppice looks
for the running server and its state; `--version` prints the build and exits.

A detached server writes its own stdout and stderr to `<data-dir>/server.log`. Check that file
first when a start seems to fail silently.

coppice exits 0 on success. It exits 1 when the server or the CLI itself refuses the request. It
exits 3 when the server is unreachable, or closes the connection mid-call. Exit 2 is reserved for
a gate deny. The CLI never returns it today; only a harness path can.

Inside a pane, coppice takes the leave key. `ctrl-space` leaves the pane and keeps it running: back
to the rail from a typing window or a full-screen attach on the floor, back to the shell from
`coppice attach`. Pressed twice within a moment it sends one `ctrl-space` byte to the pane
instead. In a floor window, and on the rail, coppice also takes the talk key (`ctrl-\`), which
records a voice clip for the agent; see Voice. A full-screen attach and `coppice attach` pass it
to the harness. Every other key belongs to the harness. The status line under the pane names the key.
To pick another one, set `leave` in the `[keys]` table of `coppice.toml`. It takes one ctrl
key, as `ctrl-space`, `ctrl-]` or `ctrl-a`, and a name it cannot read stops the run with the
file name and the shape it wants.

```toml
[keys]
leave = "ctrl-]"
```

Headless panes take a harness instead of a command, and give you typed events instead of a
screen:

```
coppice pane create --cwd . --kind headless --harness claude --label "the tests"
coppice agent prompt w1:p2 "run the tests and fix what breaks" --wait --timeout 600000
coppice agent wait w1:p2 --until blocked
coppice agent list
```

`--wait`'s timeout flag is `--timeout`, in milliseconds, the same as `pane wait-output` and
`agent wait` use.

### Ready prompts

A prompt reaches an agent only when its input box is ready. A prompt is `pane send-text` with
its Enter, `agent prompt`, `pane run`, the phone's Enter button, and what the foreman sends. It applies to a
pty agent whose harness has a detection manifest with an idle rule, such as `claude`.

- While the agent starts or works, the server keeps the prompt in a queue, in order, and the
  reply says `queued until <label> is ready`. The prompt goes once the input box shows.
- While the agent waits on a question on its screen, such as Claude's folder trust screen or a
  permission ask, the server refuses the prompt and sends nothing, and says how to answer:
  `<label> is waiting on a question on its screen. Answer it first: open it.`, or on the trust
  screen `... Answer it first: coppice pane trust <pane>, or open it.` A prompt already in
  the queue waits until you answer.
- An agent that reads ready gets the prompt once it has stayed ready for 300 ms.
- A prompt may be 64 KiB, and a queue holds 16 prompts and 64 KiB at most. When the agent ends,
  or stays 10 minutes in one state other than working, the queue drops what it holds. The floor
  says how many prompts were not sent, and so does the sender: the pane that sent them gets a
  line typed into its prompt, and the phone or window that sent them gets the note. Prompts
  queued when the server stops are dropped.
- The text goes first, and the Enter goes 150 ms later on its own, since Claude reads a fast
  burst as a paste. When the text still sits in the input box after that, the server sends one
  more Enter, never more.
- Raw typing, a bare Enter and `pane send-keys` are your own keys. They are never queued or
  refused. `pane run` in a shell, or in any pane with no manifest, runs at once as before.
- An agent whose harness has no manifest, or a manifest that can never read idle, and a
  headless agent, get the prompt at once.

A new agent in a folder Claude has not seen shows Claude's trust screen. The agent reads `needs
you` with `asks to trust this folder`. Answer it in its window, on the phone, or on the floor's
peek: Trust this folder chooses yes, and Not now presses Esc, which ends Claude. `coppice pane
trust PANE` does the same from a shell. coppice never writes Claude's config to trust a folder.

Reading and debugging:

```
coppice pane read w1:p1 --source visible      the screen right now, the default
coppice pane read w1:p1 --source recent       the last 200 lines of scrollback
coppice pane read w1:p1 --source detection    the whole unwrapped screen, exactly what the manifest rules see
coppice pane explain w1:p1                    which rule matched, and every rule that did not
```

A pane you did not size yourself defaults to 120 columns by 40 rows. An unknown `--source` is a
plain error naming the three it accepts.

From another machine, over SSH, with no extra service:

```
coppice --remote ssh://build-box pane list
```

`coppice --stdio` speaks the same protocol on stdin and stdout. That is all `--remote` uses; it
is not wired for `coppice attach` yet, so attaching to a remote pane means SSHing in and running
`coppice attach` on that host directly.

## Command reference

Every verb the CLI takes, beyond the ones already shown above. `pane` and `agent` verbs take a
pane id as their first argument; add `--json` to a `server`, `workspace`, `tab`, `pane` or `agent`
command for one JSON object instead of a table or a line.

| command | what it does |
|---|---|
| `coppice server start\|stop\|status\|token` | bring up, shut down or inspect the server, or name its credential |
| `coppice workspace create [--cwd DIR] [--label TEXT]` | start a new workspace |
| `coppice workspace list` | list workspaces |
| `coppice tab create [--label TEXT] [--workspace ID]` | start a new tab |
| `coppice tab list [--workspace ID]` | list tabs |
| `coppice pane create [--cwd DIR] [--label TEXT] [--kind pty\|headless] [--harness NAME] [--cols N] [--rows N] -- COMMAND...` | start a pane |
| `coppice pane list [--ended]` | list live panes, or with `--ended`, Recent: panes that ended on their own or that a restart found closed |
| `coppice pane send-text PANE TEXT [--no-enter]` | type text into a pane; with Enter it is a prompt, see Ready prompts |
| `coppice pane trust PANE [--not-now]` | answer Claude's folder trust screen with yes; `--not-now` presses Esc, which ends Claude; only the operator may run it |
| `coppice pane send-keys PANE KEY...` | send named keys, see the server's `Known keys:` reply and `testdata/keys.json` for the list |
| `coppice pane run PANE LINE` | type a line and press enter; on a harness pane it is a prompt, see Ready prompts |
| `coppice pane read PANE [--source visible\|recent\|detection]` | read a pane's screen |
| `coppice pane close PANE` | close a pane and remove its record at once; it never lands in Recent |
| `coppice pane resize PANE --cols N --rows N` | resize a pane |
| `coppice pane wait-output PANE [--contains TEXT] [--state STATE] [--timeout MS]` | wait for text or a state |
| `coppice pane explain PANE` | show which manifest rule matched, and every rule that did not |
| `coppice pane fork PANE [--label TEXT]` | start a headless pane that resumes a copy of the pane's session |
| `coppice pane forget PANE\|--ended` | remove one Recent pane's record, or every one of them; a live pane refuses |
| `coppice pane resume PANE` | start a new pane from a Recent one - resuming its session when the harness allows it, fresh otherwise - and remove the old record |
| `coppice pane rename PANE LABEL...` | change a pane's label in place; works on a live pane and on a Recent one alike |
| `coppice new [PROJECT] [--no-attach]` | start the default harness in a project (by name or path), or here with no `PROJECT`, and attach; `--no-attach` prints the pane id instead |
| `coppice project add DIR` | pin a directory as a project |
| `coppice project list` | list pinned projects, then recently used directories not already pinned |
| `coppice project rm DIR` | unpin a directory |
| `coppice agent list` | list panes as agents, with their merged state |
| `coppice agent get PANE` | one pane's merged state |
| `coppice agent prompt PANE TEXT [--wait] [--until STATE] [--timeout MS]` | send a prompt, optionally waiting for a state |
| `coppice agent wait PANE [--until STATE] [--timeout MS]` | wait for a pane's state |
| `coppice agent read PANE [--region visible\|recent\|detection]` | one pane's merged state plus its screen |
| `coppice agent allow PANE ASK [--reason TEXT] [--confirm NAME] [--scope task]` | answer a gate ask with yes; a pane's own connection is refused. A permanent ask needs `--confirm` with the pane name. `--scope task` allows an undoable ask for the task |
| `coppice agent deny PANE ASK [--reason TEXT]` | answer a gate ask with no; a pane's own connection is refused unless the pane is the foreman that holds that ask now. The gate lets a pane run it plainly, since the server makes that check, but refuses it through `--remote`, `--socket` or a wrapper such as ssh, setsid or nohup, where the server would read it as the operator's |
| `coppice floor note TEXT` | print one line on the floor, in dim |
| `coppice floor talk TEXT` | say one sentence to the floor's foreman; starts the foreman when none runs; only the operator may run it |
| `coppice skill foreman` | print the page that makes a pane the foreman; it needs no server |
| `coppice task create --label NAME [--parent ID] [--cwd DIR] [--worktree] [--model NAME]` | record a task; with `--worktree`, add a git worktree beside the repo that holds `--cwd` |
| `coppice task list [--tree]` | list tasks with their bubbled state, or draw them as a text tree |
| `coppice task close ID [--keep-worktree]` | close a task, its descendants, their panes, and their worktrees |
| `coppice task move ID --parent ID` | put a task under another; an empty parent detaches it |
| `coppice task set-foreman ID --pane PANE` | name the pane that hears the task's undoable asks first; an empty pane clears it; only the operator may run it |
| `coppice` | open the floor: the rail of agents by task or project, live windows that take the keys, a peek on one pane, and attach, in this terminal |
| `coppice attach [PANE]` | render one pane live, in this terminal |
| `coppice HARNESS [ARGS...]` | open that harness in the current directory and attach; `HARNESS` is a name from the config file |
| `coppice open HARNESS [ARGS...]` | the long form of the line above |
| `coppice web [--no-open] [--listen ADDR]` | start the server if needed, serve the floor on this machine only, and open it in the browser, signed in |
| `coppice web cert init [--name NAME]... [--ip ADDR]... [--ca-dir DIR] [--ca-listen ADDR] [--qr url\|pem\|off]` | make the local CA if there is none, issue a server certificate, and print a QR that installs the CA on a phone |
| `coppice web cert show [--ca-dir DIR]` | print what the current certificate covers and when it expires |
| `coppice web cert tailscale NAME.TAILNET.ts.net [--dir DIR]` | ask tailscale for a Let's Encrypt certificate and write it where `web serve --tls tailscale` looks for it |
| `coppice web serve [--listen ADDR] [--tls tailscale\|localca\|files\|off] [--cert FILE --key FILE] [--ca-dir DIR] [--ca-listen ADDR] [--external-url URL] [--gate-root DIR] [--voice-url URL [--voice-token-file FILE]] [--ntfy URL --ntfy-topic NAME --ntfy-token-env VAR] [--persist\|--forget] [--web-push] [--qr]` | serve the phone client, minting a token on the first run |
| `coppice web token [--rotate] [--url URL] [--qr] [--listen ADDR]` | print the current bearer token, its sign-in URL, and a QR to scan |

`coppice web` puts the floor on a phone over HTTPS. From 900 px the page is one screen: the
rail on the left, and live windows filling the rest. The page makes as many windows as fit at a
readable size, a cell at least 7 px wide, up to nine, and only as many as there are agents. A
window whose agent is wider than it scrolls sideways; text never shrinks below that size, and
it draws at the screen's pixel ratio. Windows fill with the agents that need you first, then
the working ones, then the rest. The keys start on the first agent that needs you, else the
first window. The selected window has an accent border and says `typing here`: every key goes
to its agent until `ctrl-space`, which gives the keys to the rail. There the arrows or `j`/`k`
move the selected row, `Enter` or `ctrl-space` puts it in a window and types there, `1` to `9`
puts it in that window, and `n` denies its ask. A click on a row, a window header or a window
body does the same as `Enter`. Each row shows the number of its window. An agent that ends on
its own leaves its window with one line, such as `build ended (exit 3)`, amber and kept until
clicked when the exit is not zero; an ended agent never fills a window. A paste goes to the
pane as one text. The browser keeps a few keys a page
cannot take: `ctrl-w`, `ctrl-t` and `ctrl-n` act on the browser, and `ctrl-v` pastes, so those
four never reach a web tile. A key typed with alt or meta held, an AltGr character, and
text from an input method (IME) may not reach a window yet either. Use the terminal floor for
all of these.

The rail is a tree. Each task has its agents under it, and its child tasks below them.
Agents with no task group by project, the base name of their directory. With no task and
one project, the rail is a plain list. A click on a group line folds it to one line with its
counts, such as `2 ask ●1 ○3`, and the browser remembers the fold. Each row shows a state
dot (filled for working, a ring for idle), the name, the harness when the server knows it,
the window number, and an `×` on hover or focus. The `×`, or `Delete` on the rail, asks once:
`Stop night-notes? Enter stops it, Esc keeps it.` Stop closes the agent, and its record goes;
it does not go to Recent. `ctrl-w` does the same only in an installed app window, since a
browser tab keeps `ctrl-w` for itself. A row dragged onto a window puts its agent there. A
double click on a name, in a row or a window header, or `F2` on the selected row, renames it
in place: `Enter` saves and `Esc` keeps the old name. Recent, folded shut at the foot of the
list with its count, holds the agents that ended on their own, each with Resume and Forget,
and Resume all and Clear all; opening it clears the ended lines. `New` starts the default
harness near the selected agent in one click, and the new agent takes a window with the
keys. The `▾` beside it lists the projects, pinned first, and starts one there; `More...`
opens the full form.

Each window header has two lines: the name and the last verdict, then a stack bar with the
state word and the directory after it. The bar runs in steps: the loop (the harness), the
daisugi mode (`enforcing`, `watching` or `off`), the router (`gateway`, `switchyard` or
`direct`), and the model. The words say what the agent runs. The colour says only its health:
green works, amber is degraded (a disarmed gate, or a codex gate, whose hooks fail open), red
is failing (a gateway that does not answer), grey is off or not known. A deny is a verdict,
not the gate's health, so it shows only in the mark. A field the
server does not know reads `model ?`, never a guess. A click on a step opens its details under
the header, with an outline on the step: daisugi shows the mode, whether it is armed, and the
last verdict with its clause and a Journal button; the model shows its tokens; the router
shows the gateway and whether it answers. No step can be swapped from here yet, and each says
so. A light pulses on the loop while the agent works and on daisugi while an ask waits for
you. Beside the name, `✓`, `✕` or `⧗` (an ask) is the last verdict; its tip names the tool and
clause, on hover and while the mark has the focus, and a click opens the daisugi details. An agent with no stack shows its harness word instead.
Reply on an ask gives that window the keys, so the answer is typed into the agent there.

The bar at the top carries the floor facts, read every five seconds with a fresh pane list:
how many agents are in each daisugi mode, such as `daisugi · 2 enforcing · 1 off`, whether
the gateway answers, how many agents
work and need you, and tokens today. The views (Tree, Kanban, Colony, Shift log, Grid, Inbox)
and the Journal open as a panel over every window but the first column. The panel takes the
focus and gives it back when it closes; `×` closes it, and
so does `Esc` while the rail has the keys (in a window `Esc` is the agent's). An old
`#/view/...` link opens the same panel. The colony can be pinned as a strip above the
windows, one row of 45 px; at 1440 by 900 even that costs a row of windows. The journal lists gate verdicts.
The server keeps no verdict history, so it reads the shift log's events of the last two
hours and each last verdict the page saw, and a verdict made while an agent kept working
shows only if the page saw it as the last one; the panel says this.

Every error or warning carries a fix: a button that retries the connection, opens Settings,
resumes or forgets an ended agent, or opens a shell agent with a command typed and not run,
so you read it before you press `Enter`. A fix that cannot run from the page says it runs on
the computer that runs coppice. While the page cannot reach the box it keeps the last
picture, dimmed, and retries.

On a phone (under 600 px wide, or under 900 px wide and 500 px tall, which is a phone on
its side) home is the overview: the colony strip when the server lists the colony, the
agents that need you in a small queue at the top with Deny and Look, and the agent list
grouped as the rail groups it. A tap on an agent slides its sheet in from the right over
most of the screen. The overview stays live under it and in sight at the left edge. The
sheet shows the agent's live terminal at a readable size (a cell at least 7 px wide; a
wider terminal scrolls sideways), its stack bar and gate mark, and its ask with Deny and
Allow or the name field. An ask a harness holds itself, such as OpenCode's permission
prompt, gets the same controls: the page sends `agent.allow` or `agent.deny` to
coppice-server over its own connection, and the server answers the harness. A swipe right on the sheet, a tap on the edge, or the back arrow
goes back; on the terminal body a swipe right scrolls until the body is at its left end.
Nothing leaves the page, and the phone never resizes an agent.

The sheet's bottom bar holds Keys, a text field, Mic and Enter. The keyboard's own Enter
types the text into the agent's input and does not press Enter, so you can read it there
first. The Enter button types what the field holds, if anything, and presses Enter, in
one request. Keys opens a drawer with Esc, Tab, the four arrows, ctrl-c and Enter. A
headless agent has no terminal to type into, so it takes whole messages: on its sheet
either Enter sends the field as one message and there is no key drawer, and its window on
a wider page sends it no keys and has a line under it whose Enter sends the message. The
open sheet is a modal dialog, and it ends above an on-screen keyboard even where the
browser does not resize the page for one. The colony strip, pinned or on a phone, is one
row with a slot per agent; a tap on a slot opens that agent. The
repository's
`docs/how-to/phone.md` is the full guide: the certificate choice, signing in, and ntfy
push. `coppice web` never dials the coppice socket at the command line itself, so its
only exit codes are 0 and 1.

## What a restart does

> A restart brings back the layout, the labels and the working directories. It does not bring back running processes. Panes come back closed or unknown, never idle. A headless pane resumes only when its adapter recorded a harness session id.

`coppice server status` prints this same sentence back, along with what it found. A restored
pane never reports `idle`: closed or unknown are the only two honest answers for a process that
is not there to ask.

## Recent

A pane the operator closes with `pane close` is gone at once; it never sits around. A pane that
ends on its own, or that a restart finds closed, moves to Recent instead: `coppice pane list
--ended` lists it, newest first, with its exit code and when it ended. `coppice pane forget PANE`
or `--ended` removes a record from Recent for good. `coppice pane resume PANE` starts a fresh
pane in its place, resuming its session when the harness allows it (a headless harness that
recorded a session id, or a pty one whose `[harness.<name>]` table has `resume_args`) and starting
clean otherwise; either way it removes the old record. `coppice server status` says how many
Recent panes it could actually resume, in its `resumable` count. Nothing older than seven days
stays in Recent: the server clears it out when it starts and once an hour after.

## Projects

A project is a directory the operator works in. `coppice project add DIR` pins one in
`coppice.toml`'s `projects` list; `coppice project rm DIR` unpins it. `coppice project list`
shows the pinned ones first, then the ten directories a pane most recently started in that are
not already pinned - the server keeps that second list itself, in its data directory, so it
survives a restart. `add` and `rm` rewrite the whole config file through the same path `coppice
open` already uses to write a freshly discovered harness table: every field coppice itself knows
survives the rewrite, but a comment you hand-added, and any key coppice does not recognize, does
not - only the leading `# coppice config...` line and coppice's own fields are kept.

`coppice new [PROJECT]` starts the default harness - `coppice.toml`'s `default` line, or, with no
config file yet, the first harness found on PATH - in a project, and attaches, the same way
`coppice claude` does. `PROJECT` matches a project's base name first, then its path; with none
given, it opens in your own current directory, never a guess at "the most recent one". `--no-attach`
prints the new pane's id instead of attaching, the same as running on a pipe.

A `pane.create` with neither `cwd` nor `cmd_argv` - a "one-click new agent" request; no floor
sends one of these yet, but a later web/TUI button will - fills both in on its own: `cwd` becomes
the directory named by `near` (a pane id, when one is given), else the most recently started-in
directory that still exists on disk - walking the whole recent list, newest first, so a directory
just used wins even when it is also pinned, and a missing newest entry falls through to the next
recent one rather than straight to a pinned project - else the first pinned project that still
exists, else the directory the server itself was started in. The harness becomes
`coppice.toml`'s `default` line.

A label the request left out becomes `<harness>-<the directory's own name>`, numbered `-2`, `-3`
and on only against another pane still on the floor; a Recent one never forces the count up. This
is not only the one-click case above: `coppice new`, `coppice open`/`coppice claude`, and the
TUI's own Enter-with-no-selection all send an explicit harness and directory and no label, and all
get named this same way.

## Voice

Voice needs no setup. When `daisugi` is on PATH and has its voice extra
(`pip install 'opendaisugi[voice]'`), the coppice server starts `daisugi voice serve` itself. It
listens on a loopback port the server picks, checks a token the server writes to
`<data dir>/voice/token`, and logs to `<data dir>/voice/voice.log`. It stops with the server.
When it stops on its own, the server starts it once more; when it stops again, voice is down
until you retry. The first start can take a minute while the model loads.

In the TUI, the talk key records a clip for the selected agent: the agent whose window has the
keys, or else the row under the cursor. The key is `ctrl-\` unless `[keys] talk` names another
ctrl key. In a terminal that reports key release (the kitty keyboard protocol), hold the key while
you speak and let go to stop. Anywhere else, press it to start and press it again to stop; a line
says `recording… press ctrl-\ to stop`. `Esc` throws the clip away. The clip is recorded with the
first of `pw-record`, `parecord` or `arecord` on PATH, into the coppice data directory, and is
deleted once it is heard. The text lands in the agent's input line without Enter, so you read it
and press Enter yourself. When there is no recorder, no voice server, or no agent selected, one
line says why and what to do.

On the web floor the mic works with no flags. When voice cannot run, the line under it says why
and what fixes it, and a Retry button asks the server to start voice again. `coppice web serve
--voice-url URL` still points the mic at a voice server you run yourself. On this machine it
gets the web token; on another machine it needs `--voice-token-file FILE` with that server's
own token, or voice stays off and says so, since the web sign-in token never leaves this
machine. When voice needs daisugi's voice extra, the fix button opens a shell with
`pip install 'opendaisugi[voice]'` typed and not run.

```toml
[voice]
enabled = false                   # start no voice server
url = "http://127.0.0.1:7477"     # use this one; the server starts none
token_file = "/path/to/token"     # the token that server checks; default the web token file
args = ["--data-dir", "/some/dir"]  # added to daisugi voice serve

[keys]
talk = "ctrl-g"
```

## Security

The socket is the credential. It lives at mode 0600 inside a 0700 directory, and the server
refuses any connection whose peer uid is not yours. There is no token, no port, and no second
user. `coppice server token` says so, in more words, on a terminal.

Spawned processes get `COPPICE_SOCK` and `COPPICE_PANE` in their environment. That pair is how
the openDaisugi gate hook inside a harness reports state back without any configuration. They
also get `COPPICE_DATA_DIR`, the server's data directory as an absolute path, so the gate guards
the web token, the CA and TLS keys and the voice token there even under a custom `--data-dir`.
The server sets all three, over any value the pane's own env or the server's env holds.

This is Linux today. `lock_other.go` and `peercred_other.go` let the module compile on other
unixes, but a peer-credential check has no portable implementation outside Linux yet, so every
connection is refused there until one exists. The build itself is unix-only outright: the client
spawns its own detached child with a real process group, which has no Windows equivalent as
written.

## Agent detection

Screen detection uses Herdr's TOML manifests, vendored under `internal/detect/manifests/` at a
recorded commit, Apache-2.0, see `NOTICE`. The schema is written down in
`internal/detect/README.md`. coppice never fetches a manifest from the network.

Put your own file at `$XDG_CONFIG_HOME/coppice/agent-detection/<agent>.toml`, or
`~/.config/coppice/agent-detection/<agent>.toml` when that variable is unset, to replace a
bundled one. The file's own `id` field decides which bundled manifest it replaces, not its
filename; naming it after the agent is a convention, not a rule coppice enforces.

Screen detection is the fallback. A higher source outranks it: the gate, an operator, or an
attached client. A pane any of those reported on in the last two seconds, the `GateQuiet` window,
is not scanned at all. After two seconds the screen speaks again, so a harness that stops calling
the gate recovers rather than freezing on its last reported state.

If a manifest fails to load, `coppice server status` prints the warning. A broken override file
reverts to the bundled manifest, and it says so.

`COPPICE_CLAUDE_BIN`, `COPPICE_CODEX_BIN` and `COPPICE_SPRIG_BIN` override which binary a headless
pane spawns for that harness, in the daemon's own environment. Unset, each adapter runs its
harness's ordinary name off `PATH`.

## Layout of the code

| path | what it is |
|---|---|
| `cmd/coppice` | the built binary's own `main` |
| `internal/vt` | the only package that touches libghostty. cgo. |
| `internal/proto` | the wire: requests, responses, events, the error enum |
| `internal/state` | master spec 3.1, the source precedence, in one function |
| `internal/layout` | workspaces, tabs, panes, and the ids that are never reused |
| `internal/pane` | the grid, the pty, the headless adapter interface. cgo, through `internal/vt`. |
| `internal/detect` | Herdr's manifest engine in Go |
| `internal/server` | the socket, the dispatcher, the verbs. cgo, through `internal/pane`. |
| `internal/adapters` | one package per harness. cgo, through `internal/pane`. |
| `internal/attach` | the single-pane renderer. No cgo: it only ever reads frame diffs off the wire. |
| `internal/cli` | the command line and the thin client. cgo, through `internal/server`. |
| `internal/toolchain` | answers "is libghostty-vt built here", for every cgo test's own skip check |
| `internal/boundary` | the one test proving only `internal/vt` imports libghostty. No cgo, deliberately. |
| `scripts/` | `toolchain.sh` and `preflight.sh` |
| `testdata/` | fixtures: manifests' screens, vt goldens, fake harness scripts, wire events |

"cgo" above means the package cannot link without `scripts/toolchain.sh` having run, and its own
tests skip with a reason instead where that has not happened, unless `COPPICE_REQUIRE_TOOLCHAIN`
is set, which turns that skip into a failure. CI always sets it.

## What is not verified yet

- Codex's wire format and its `codex exec resume` argv are unverified on this box: no real
  `codex` binary has run against this adapter. The fixture in `testdata/adapters/` is invented
  from the published docs. The adapter accepts either of the two vocabularies it might turn out
  to speak, and fails loudly, naming the unknown type, rather than silently on anything else.
- The sprig adapter has likewise never run a real `sprig`. Its shape comes from reading
  `harness/sprig/cli.go`'s own source, and a test asserts the flag names it depends on have not
  drifted from that file.
- `internal/detect/manifests/claude.toml`'s `live_turn_working` rule cannot match a line ending
  `esc to interrupt)`: the regex's own alternation is missing that closing paren. This is a
  vendored Herdr file; see `NOTICE` and `internal/detect/manifests/PROVENANCE`. Fixing it means
  forking the vendored set at a recorded commit, not a quiet edit.
- A person ran `coppice attach` on a real terminal on 2026-09-10. The raw-mode, keystroke and
  detach path worked as intended. The panic-safe terminal restore still has no automated test.
  The alternate screen request was refused by that terminal, so whether `\x1b[?1049h` and
  `\x1b[?1049l` take effect depends on the terminal, not on this code: coppice writes both
  whenever stdout is a terminal, in the right place and the right order. That same run found a
  help overlay defect: the block landed on the status row and scrolled the terminal. This
  commit fixes it. `TestHelpNeverPaintsOnTheStatusRow` pins the fix: it asserts the help rows
  and the status row never overlap and the write carries no newline. The SIGWINCH resize path
  has an automated test too, against a real pty pair:
  `TestAttachAndResizeAgreeOnRowsFromOneTerminalHeight`. That test proves the wire values
  agree. It does not prove a person watching the screen sees the redraw happen correctly.
- `coppice attach` does not work over `--remote` yet. It dials a local unix socket directly.
- An ordinary connection's teardown, no `coppice server stop` involved, gives an in-flight
  handler up to 130 seconds to finish before reclaiming its goroutines and file descriptor. That
  bound only matters for a handler that never writes and never watches for the connection
  dying; `agent.wait` and `pane.wait_output` both do the latter and return within milliseconds
  regardless of how long the bound is.
- The frame stream coalesces onto a 60 Hz ceiling, `FrameInterval`, fixed at 16 ms. It does not
  distinguish a harness that redraws 50 times a second from one that redraws once. A frame can
  arrive before the `pane.attach` response that started the stream; a client dispatches on each
  message's own event name or request id, never on arrival order. `autoPanes` only ever demotes
  an automatic subscription to an explicit one, never the reverse.
- Two cross-language divergences plan 03's Python oracle still needs to settle: whether `pane`
  and `harness_session_id` type-check as optional the same way on both sides, and which clock
  the manifest hold window reads. Go uses the server's own receive clock. The Python side has
  used the event's own `ts`.
- The one real cross-language fixture guard, `TestEventFixturesMatchTheFloorFixtures`, has never
  run: it skips unconditionally until `tests/floor/testdata/events` exists. Plan 03's Python
  client is what creates that directory and owns the bytes in it.

## Contributing

`go vet ./...`, `gofmt -l .`, and `go test -race ./...` must all be clean before a change lands.
See `.github/workflows/coppice.yml` for the exact CI commands, including the libghostty-vt cache.
