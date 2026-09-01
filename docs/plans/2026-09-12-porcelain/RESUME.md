# Resume point for the porcelain plans

## Plan 12 hand check

Run on 2026-09-13 against a build of `harness/coppice` at the plan 12 commits. The server ran on
its own socket, data directory and runtime directory under a scratch path on real disk, so the
operator's own server was never touched. The exact commands and their output are in
`.superpowers/sdd/plan-12-honest-state/run/hand-check.log` on the box that ran them.

Build: `cd harness/coppice && go build -o build/coppice ./cmd/coppice`. There is no Makefile.

| step | command | result |
|---|---|---|
| 1 | `coppice server start` | listening; exit 0 |
| 2 | `coppice pane create --cwd . -- sh -c 'sleep 60'` then `coppice pane list` | row `w1:p1 pty idle process sleeper`; `--json` shows `state: idle`, `source: process`, `detail: no output yet`, `quiet_for: 0.44` |
| 3 | `coppice pane create --cwd . -- no-such-harness` then `coppice pane list` | one line: `spawn_failed: no-such-harness is not on PATH. Install it or give a full path.`; exit 1; `pane list` still shows only `w1:p1` |
| 4a | `coppice attach w1:p1 < /dev/null` | `attach needs a terminal. Run it from a shell, not a pipe.`; exit 1 |
| 4b | `coppice attach w1:p1` in a terminal | not run. The agent that ran this check never owns a TTY. Left for the operator. |
| 5 | `coppice server status` | `pid 1681599` as an integer; `panes 1, 1 live`; exit 0 |
| 6 | `coppice server stop` | `stopped`; exit 0 |

What step 4b should show, once an operator runs it from a shell: the pane's screen on the
alternate screen, and the shell back on detach.

One rule learned on the way: `COPPICE_NO_AUTOSTART` also refuses an explicit detached
`coppice server start`. Start with `--foreground`, or leave the variable unset, when the point
is to start a server.

## Plan 14 hand check

The floor renders from the Go binary. The tests drive it over a pipe with scripted keys and never
own a terminal, so the one step left for an operator is to run bare `coppice` from a shell against
a server of your own: the roster appears, Space opens the peek, Esc closes it, Enter attaches the
row under the cursor, the leave key comes back to the roster, and ctrl-c leaves. Plan 18b made
Enter type into a tile when tiles show and made ctrl-space the leave key. `daisugi coppice floor`
execs the same binary. The Textual floor screen is gone; `:floor <backend>` in the cockpit is
still the swap alias.

## Plan 15 hand check

Terminal and phone now share one shape: the roster plus as many live tiles as the width
allows. The tests drive both over pipes and a fake DOM, so two hand steps remain. In a
terminal at least 120 columns wide, run bare `coppice`, press Space on a row and watch the
tile beside the roster go live, click a row and a tile, then type `swap 1 2`, `rotate`,
`lock`, `unlock`, `layout all`, and `reset` on the prompt. On the phone, open the floor at a
phone width, a tablet width, and a laptop width: no tile, one tile, two or three tiles, with
the ask bar amber on a blocked pane and Deny and Reply on it. Plan 18b changed two things
here: a terminal tile now resizes the pane it shows, and the phone width is a pane above a
dock. Since plan 17, `t` is text for the prompt, and Space or a click fills a tile.

## Plan 16 hand check

coppice now works before it is configured. Discovery, the one question, the config file, Enter
on an empty floor, and `coppice <harness>` are all driven over pipes and fake PATHs in the
tests, so the hand steps are the real ones. Move `~/.config/coppice/coppice.toml` aside and run
bare `coppice` in a terminal: with one harness on PATH there is no question, with several there
is one numbered question, and the three vocabulary lines follow. Open the file it wrote, then
press Enter on the empty floor and watch the default harness open here. From a shell, run
`coppice claude` and `coppice open claude --resume` and confirm both attach. Edit the file to
add `args`, then press Enter again without restarting the server and confirm the new args
apply. Plan 17 rewrote the second vocabulary line: it now says that ctrl-c quits.

## Plan 17 hand check

The roster answers to six keys and the footer prints them from one table. In a real terminal
open two panes and let one block on an ask. Press shift-tab and watch the cursor land on the
blocked row, then press it again and watch it stay inside NEEDS YOU. Press ctrl-t, type a word,
press Space and confirm the space lands in the prompt, then press Esc and confirm the prompt
clears. Press ctrl-w on a row with a tile and confirm the tile closes while the row stays. Press
ctrl-w on a row with no tile, read `close <label>? y/n`, press n. On a narrow floor press Enter
on a row, then ctrl-space once, and confirm the roster is back within a third of a second.
Attach again and press ctrl-space twice inside a shell pane running `cat -v` to see one `^@`
arrive. Plan 18b made ctrl-space the default leave key in place of ctrl-]. Set `leave = "ctrl-["`
in coppice.toml and confirm the next `coppice attach` refuses with the file path. ctrl-c quits
the floor. The web floor page keeps its own keys until plan 23.

## Plan 18 hand check

A task is the unit above the pane: a label, a parent, a model or none, and a worktree beside
the repo when asked. The tests build worktrees in temp repos and drive the tree view over a
pipe, so the hand steps run against your own server in a repo of your own. Run
`coppice task create --label gate-refactor --cwd . --worktree` and confirm the reply names
`<repo>-worktrees/gate-refactor` and `git worktree list` shows it. Run
`coppice pane create --task t1 -- claude` with no `--cwd` and confirm the pane's cwd in
`coppice pane list` is the worktree. Make one commit in that worktree, then
`coppice task list --tree` shows `+1` beside the task. Create a second task with
`--parent t1` and confirm `task move t1 --parent t2` refuses with a cycle. In the floor type
`tree`, move with the arrows, press Enter on the team and confirm the roster shows only its
panes and the bar names the task, then Esc clears the filter. Leave an uncommitted file in
the worktree and run `coppice task close t1`: it refuses and names `keep_worktree`. Run it
again with `--keep-worktree` and confirm the panes close and the directory stays. Start the
floor against a server built before this plan and confirm the roster still draws and the
message line names `task.list`.

## Plan 18b hand check

Every tile takes the keyboard. The tests drive the floor over a pipe through a recording
proxy and the web page through a fake DOM, so the hand steps are the real terminal and the
real phone. In a terminal at least 120 columns wide, run bare `coppice` with two panes open.
Click a tile: its header turns to reverse video and says `typing · ctrl-space leaves`, the
footer says `ctrl-space roster  click a tile to move`, and the roster dims. Type a line into a
claude pane and press Enter, and confirm it submits. Press ctrl-c inside the tile and confirm
the floor stays. Press ctrl-space and confirm the roster keys come back and the next letter
lands on the prompt. Press Enter on the other row and confirm its tile types. Double click a
tile, or type `zoom`, and confirm the full-screen attach; ctrl-space brings the tiles back at
their own size. Resize the terminal and confirm the harness in the tile redraws to fit. On a
laptop browser, click a tile and type into it, then press ctrl-space. On the phone, confirm
the page is one pane above the dock, the blocked chip is amber and first, a tap on a chip
opens its pane, a swipe left or right moves to the next or previous one, and the pane keeps
the size it had in the terminal. A message on the floor's message line takes one row from the
tiles, so it resizes every tiled pane by one row while it shows.

## Plan 19 hand check

Any pane can be the foreman. The tests run cat and sh in place of every harness, fake the
kernel's view of a peer in unit tests, and drive one real socket from a helper process inside
a pane, so the hand steps are a real harness, a real gate, and the real Claude Code hooks.
Rebuild the binary first, since `coppice skill foreman` prints the page the binary carries.
The hello, the notes, the allow verbs and the subagent rows need the new server too. Run
`coppice server stop`, then `coppice server start`. A restart closes the running processes.
A floor built from this plan still draws against an older server, with no notes and no
subagent rows.

1. Run `coppice skill foreman` with no server running. It prints the page and starts nothing.
2. Run bare `coppice`. Type `foreman` and confirm the message line says
   `no foreman. Type: foreman claude`. Type `foreman claude`. A pane labelled `floor` opens,
   the line `Run: coppice skill foreman. Follow it. You are the foreman of this floor. Say
   ready.` lands in it, and `coppice.toml` now holds `foreman = "floor"`. Wait for the pane to
   say ready. The floor sends the line only after the pane drew something and went idle, up to
   30 s. A question on the screen that claude's manifest reads as blocked stays blocked when
   the screen goes quiet, so no Enter reaches it, and the message line says `floor did not go
   idle in 30 s, so nothing was sent`. The claude manifest reads the trust question, the theme
   picker, the login choice, the security notes, the terminal setup question and the API key
   question as blocked, from their text. Open it in a folder claude does not trust yet and
   confirm that `coppice pane explain <pane>` names a `coppice_first_run_` rule. A start screen
   that names none is a gap in the manifest: note its text. Answer any question yourself, then
   type the line in the tile.
3. At the prompt type a sentence of five words or more, for example
   `open a pane named docs that lists the repo`. It goes to the foreman, and the foreman's
   tile fills when tiles show. As the foreman runs `coppice pane create` and the rest, each
   command shows in dim above the prompt, as in `floor › pane.create  docs  claude`. On the
   web floor the same lines show dim under the `floor` tile header. Each of the first three
   notes takes one row from the tiles, so every tiled pane resizes by one row as they arrive,
   the same as a message on the message line.
4. Ask the foreman to allow a pending ask. From its own shell, `coppice agent allow <pane>
   <ask>` must answer `unauthorized: a pane can propose. It cannot allow.` Try the same line
   with `COPPICE_PANE` unset in that shell, for example `env -u COPPICE_PANE coppice agent
   allow <pane> <ask>`: the server still refuses, since the kernel places the process inside
   the pane. With the gate armed in enforce mode, the gate denies the Bash call first with
   the same sentence, and a Write to `~/.opendaisugi/gate/answers/x.json` is denied the same
   way.
   With the phone server on, from the pane's shell, run
   `curl -k -X POST -H "Authorization: Bearer $(cat ~/.opendaisugi/coppice/web/token)"
   https://127.0.0.1:8443/api/ask/answer -d '{"tool_use_id":"<ask>","decision":"allow"}'` with
   the gate disarmed: the web server answers 403 with the same sentence. Arm the gate again and
   confirm it denies that line before it runs. A websocket from the pane that sends an
   `agent.allow` line gets `unauthorized` too.
5. From a shell outside every pane, run `coppice agent allow <pane> <ask>` for a real pending
   ask and confirm the gate lets that tool call through.
6. Install the report hooks with `daisugi install --gate --report coppice` and confirm
   `~/.claude/settings.json` has SubagentStart and SubagentStop entries that run
   `daisugi hook record --format claude --event subagent_start` and `subagent_stop`. In a
   claude pane, ask for work that starts a subagent. A row with the agent type, for example
   `Explore  working`, shows two spaces in under the pane. Enter, Space, ctrl-w and a click
   on it each say `a subagent lives inside its parent. Enter on the parent.` The row turns
   `done` when the subagent stops and leaves the roster about a minute later.
7. Known gap: the kernel check stops a process that stays in the pane's process tree or in
   its session. A process a daemon starts for the pane is outside both, and the server and the
   web server then see the operator. One word does it: `setsid -f`, `systemd-run --user`,
   `ssh localhost`, a tmux or screen server, or `at`. The gate rule is the second door, and it
   sees only the literal words of a tool call. `c=coppice; setsid -f $c agent allow ...`
   passes both doors. Treat the rule as a guard against a model that follows its page, not
   against a hostile process.

## Plan 20 hand check

Every ask now carries a tier. The tests fake the gate's ask file, the ntfy server and the
coppice socket, so the hand steps are a real gate in enforce mode, a real harness, a real
phone and a real ntfy. Rebuild the binary and restart the server with `coppice server stop`,
then `coppice server start`. The web server needs a restart too. The tier of a shell command
comes from the shell decomposition. Without the `opendaisugi[shell]` extra every shell ask is
permanent. Check with `uv run --no-sync python -c "from opendaisugi.effects import
shell_parser_available as f; print(f())"`. A write counts as inside the workspace only inside
`CLAUDE_PROJECT_DIR`, so under a harness that does not set it every write is permanent. An ask
never runs in the resident gate server: with `--ask` the hook runs the gate in its own process.
So the tier comes from the opendaisugi the hook's Python imports. Make sure that is this tree,
or every ask file carries no tier and reads as permanent.

1. Arm the gate in enforce mode with an envelope that denies shell and writes, and keep an
   operator present. In a claude pane labelled `gate-refactor`, ask for `git status`. On a
   terminal narrower than 120 columns, Space on the row opens the peek. On a wider one, Space
   fills a tile, so type `read <pane>` to open the peek. It reads `Deny is the default.` and
   `y allow once  t allow for this task  n deny`. Press `y`. The call runs.
2. Ask for `git push --force origin master`. The peek reads `n deny   to allow, type
   gate-refactor and enter`. Press `y`. The message line says `type the pane name to allow` and
   nothing is sent. Type `gate-refactr` and Enter. The message says it is not the pane name,
   nothing is allowed, and the foreman gets nothing. Press `n`. The call is denied.
3. Ask again for the force push. Type `gate-refactor` and Enter. The call runs, so do this in a
   scratch repo with a scratch remote.
4. Ask for a write inside the workspace. In the peek press `t`. The call runs and
   `daisugi gate proposals` lists one proposal with scope `task`. Nothing applies it.
5. Run `coppice agent allow <pane> <ask>` on a permanent ask. It answers
   `this cannot be undone. Type the pane name to allow: gate-refactor`. Run it again with
   `--confirm gate-refactor`. It passes.
6. On the floor page, a permanent ask shows Deny first, the tier line and a name field, and no
   Allow. An undoable ask shows Deny, Allow and Reply. On the phone roster, each blocked card
   shows Deny, Look, and Allow or the name field. The pane screen shows the same.
7. With `--ntfy` and `--external-url` set, block a pane on a permanent ask. The lock-screen card
   reads `gate-refactor needs you.` and `Wants ... Gate says no.`, with Deny then Look. Tap
   Deny. The gate denies. An undoable ask's card also shows Allow, which opens the pane screen.
   The action header format follows the ntfy docs and was never tried against a live ntfy.
   Note what the phone shows if a button is missing.
8. Answer an ask on the floor, then tap Deny on its old card, three times. Each tap answers 409,
   that ask is gone, and the phone is not banned.

## Plan 21 hand check

Plugins are views and policies. The tests fake the socket, gh, ntfy and the speak URL, and one
Go test runs a real policy process against a real socket. The hand steps are a real browser, a
real phone, a real gh and a real ntfy. Rebuild the binary and restart the server with
`coppice server stop`, then `coppice server start`. The server needs `python3` on its PATH for
the shipped policies. A policy that runs from a server started by hand in a shell inherits that
shell's environment, gh's login among it.

1. With no `plugins` key in `coppice.toml`, start the server. `ps` shows one
   `notify-ntfy.py` start and exit, and the server log says it is not set up. No other policy
   runs. `ls ~/.local/share/coppice/plugins` shows `_lib` and `notify-ntfy`, each mode 0700.
2. Open the floor page. The rail shows a `Tree` button. Fill two tiles, then press `Tree`. The
   address reads `#/view/tree?sel=<a>,<b>`, the tree draws the same lines as the terminal
   `tree` word, and the two panes are marked. Press Back. The tiles come back as they were.
3. Open `/plugins/tree/` on its own. It says to open it from the floor page. In the browser's
   tools, the tree frame has an opaque origin, reads no `coppice.token`, and a `fetch` to
   `/api/tasks` from its console answers 403 with `A view holds no token.` Click a pane line.
   The floor opens that pane.
4. In the terminal floor, type `tree`. The terminal tree opens as before. Add a view of your
   own in `~/.config/coppice/plugins/mine/` with `"kind": "view"`, list it with `tree` in
   `plugins`, restart, and type `mine`. The message names
   `https://127.0.0.1:<port>/#/view/mine`. With the web server off it says so instead.
5. Set `plugins = ["tree", "turn-budget"]` and `[plugin.turn-budget] budget = 2`. Restart.
   Prompt a pane three times. After the third turn the pane gets ctrl-c and the floor shows
   `turn-budget › <pane> paused: past 2 turns. Prompt it to continue.`
6. Copy `plugins/turn-budget` to `~/.config/coppice/plugins/turn-budget/` and add a
   `pane.close` call to the script. Restart. The server log shows the script's refusal:
   `plugin turn-budget did not ask for pane.close in its manifest`. The pane stays open.
7. Turn on `merge-on-green` in a scratch repo with an open pull request and passing checks.
   When the task's pane goes done, the pane gets `checks are green, merge`, and gh is never
   asked to merge by the policy. The pane's own gate asks about the merge.
8. Set `[plugin.notify-ntfy] url` and `topic` on a topic the web server does not use. Block a
   pane. One message arrives with `<label> needs you.` and `Wants ... Gate says no.` Unset the
   built-in `--ntfy` first, or the phone gets two.
9. Stop the server. `ps` shows no `*.py` policy and no process a policy started.

## Plan 22 hand check

Seven views ship on by default: tree, minimap, kanban, colony, shift-log, herdr-grid and inbox.
The tests run each view's pure module in node, drive the floor page against a stub, and run
the event ring against a fake socket. The hand steps are a real browser, a real server, and
real panes. Rebuild the binary and restart the server with `coppice server stop`, then
`coppice server start`. The web server needs a restart too. With a `plugins` list in
`coppice.toml`, add the new view ids to it, or remove the key to run the default list.

1. Open the floor page on a laptop-width window. The rail shows a button for each of the seven
   views, and the minimap draws at the foot of the rail, one dot per task and pane in the
   state colours. Click a pane dot. The focused tile shows that pane, and the address stays
   `#/roster`. Type into that tile. The pane gets the keys.
2. Press `Tree`. The text tree shows first. Press `Graph`. The graph draws the same tasks and
   panes, children below their parent. Drag to pan. Click a task node, and the address `sel`
   holds its panes. Double click a pane node. The pane screen opens.
3. Press `Kanban`. Each task sits in inbox, working, needs you or done, by its state. A card
   does not drag. Block a pane. Within three seconds its task moves to needs you.
4. Press `Colony`. Each pane is a dot in its state colour with a bar under it. Reload the view.
   No dot moves. Drag a box around two dots. The address `sel` holds those two.
5. Let two panes work for twenty minutes, then press `Shift log`. Each pane has a row with
   green cells where it worked. Block one pane, and deny a call through the gate. Its cells
   show amber and red. Reload the whole floor page. The rows come back from the server's ring.
   With the web server run on its own by `coppice web serve`, stop the coppice server while
   the shift log is open. The browser's tools show `/api/events` answering 503. The status
   line says the floor has no history from the server, and every cell with no mark draws
   as unknown, never as quiet. Start the coppice server. Within thirty seconds the ring
   answers again with a new `from`. Cells before that time stay unknown, and the status line
   says the server holds nothing from before then.
6. Press `Grid`. Every pane has a slot, blocked first, and the first nine show live text. The
   roster on the left shows the state word on each row. Start a tenth pane. Its slot says nine
   panes at most show live. Type into a live pane from its own terminal. The grid picture
   follows. Try to type into the grid. Nothing reaches the pane.
7. Press `Inbox`. Only tasks with a worktree have a row, the ones that need you first, each with
   its `+N` ahead count and no pull request number. Press `!`. The first row that needs you is
   selected and its pane draws live on the right.
8. With the grid open, press Back. The tiles come back and take keys at once. Open a pane the
   grid watched. The pane screen takes keys at once. Neither ever says the pane is view-only.
9. Open `/plugins/_lib/view.js` on its own. It loads. `/plugins/_lib/floor_client.py` answers
   404.

## Plan 23 hand check

On a phone, home is the attention queue or the quiet screen, above the dock. The tests drive
the page through a fake DOM, fake the ntfy server, and fake the coppice socket, so the hand
steps are a real phone, a real ntfy and a real server. Nothing below ran on a phone yet. Rebuild
the binary and restart the server with `coppice server stop`, then `coppice server start`. The
web server needs a restart too. Serve the page over the tailnet as the plan 06 notes say, with
`--ntfy`, `--ntfy-topic` and `--external-url` set to the address the phone uses. Open the PWA on
the phone from its home screen icon.

To make a fake ask, open a shell pane and note its id, `<id>`. Send the state line through
`coppice --stdio`, with `<now>` the output of `date +%s`:

```
echo '{"id":"1","cmd":"pane.report_state","pane":"<id>","event":{"session_id":"hand","harness":"shell","state":"blocked","source":"gate","ts":<now>,"ask":{"id":"hand_1","tool":"Bash","summary":"rm -rf build/","tier":"permanent","deadline":<now + 600>}}}' | coppice --stdio
```

The reply is `"ok":true` and `coppice pane list` shows the pane blocked. A Deny only goes
through when the gate root holds the ask, so also write
`~/.opendaisugi/gate/asks/hand_1.json` with `{"nonce":"hand","tier":"permanent"}`, or the
directory given to `coppice web serve --gate-root`. To end the ask, send the same line with
`"state":"working"` and no `ask`. The pane stays blocked until that line, because no gate
waits on this ask. Delete `asks/hand_1.json` and `answers/hand_1.json` under the gate root when
the check is done. These steps ran against a scratch server on 2026-09-23, with no web server
and no phone.

1. With no pane blocked, open the PWA. The dock shows `home` first, marked current. Home says
   `Nothing needs you.`, `N working. I will ping when one asks.`, and a last ask line. That
   line reads `last ask <age> ago`, `no ask in the last <span>`, or `last ask unknown` when the
   event ring does not answer. It never says there was no ask for a span the ring did not see.
2. Send the fake ask. Within a few seconds the lock-screen card arrives: `<label> needs you.`,
   `Wants rm -rf build/. Gate says no.`, with Deny then Look. Home shows `1 needs you`, the
   working count under it, and one card: who asks, `rm -rf build/` in code, the tier line,
   Deny first and big, Look, and a field for the pane name. The pane's dock chip is amber.
3. Lock the phone. Tap the card body. The PWA opens at `#/pane/<id>?ask=hand_1`, the pane
   shows above the dock, and the ask box is amber and in view. Tap Look on another card. The
   same happens. Send the working line, then open the old card. The status line says the ask
   is gone, and nothing is amber.
4. Send the fake ask again, with the ask file in place. Tap Deny on the lock-screen card. The
   web server log says it answered the ask, and `answers/hand_1.json` under the gate root says
   `deny`. Send the working line, then tap Deny on that card again. It answers 409 and the
   phone is not banned.
   In ntfy's own view of the message, the Deny action carries a deny token and never the
   token from `coppice web token`.
5. Send the working line. Home goes back to the quiet screen, and its last ask line gives the
   age of the fake ask.
6. On a phone that never used New, type `claude` in the Tell the floor bar and Send. Nothing
   opens, the status line points at New, and `claude` stays in the bar. Start one pane from
   New, then type `claude` and Send. A claude pane opens in that directory, and the status
   line names it. Give two panes the same label and type `close <label>`. Nothing closes, and
   the status line names both ids. Type `close <id>`. That pane closes. With no foreman, type
   a sentence. The status line says there is no foreman, and the sentence stays in the bar.
7. Make a foreman on the terminal floor with `foreman claude`. Type a sentence in the bar and
   Send. The foreman pane gets the sentence with Enter. Block the foreman with a fake ask and
   send again. The status line says the foreman is waiting on a question, and nothing is sent.
8. Tap Mic, say a sentence, tap Stop. The text fills the bar and nothing is sent. Send it by
   hand.
9. On a laptop, block two panes. The tab title reads `2 · coppice`, and `coppice` once none is
   blocked. The rail lists the views, then Layout and Lock, then the minimap at its foot. Drag
   a tile header onto the other tile. The two swap. Press Lock and start a drag. The drag
   does not start, nothing moves, and the status line says the layout is locked.
10. Narrow the laptop window below 600 px. The tiles go, home and the dock show. Widen it
    again. The tiles come back.

## Plan 24 hand check

The bridges run in tools the tests never start. The tests check pure cores: the Lua roster
core, the tmux plan and the control-mode parser against recorded transcripts, and the Herdr
manifest against the schema. The steps below are the rest. Rebuild the binary first, and
start a scratch server if you do not want your own panes resized. Every full attach sizes
its pane, and the last size sent wins.

1. Neovim. On a system where `nvim` is a flatpak alias, not a program on `PATH`,
   `go test ./internal/plugins` skips the Lua spec and says so. Run it by hand from
   `harness/coppice/plugins/nvim`:
   `flatpak run io.neovim.nvim --headless --clean -c "luafile tests/roster_spec.lua"`. It
   prints `roster spec: 14 passed, 0 failed` and exits 0. It ran that way under NVIM 0.12.5
   on 2026-09-23, and a headless smoke against a scratch server read the roster and opened a
   Space peek.
2. In a real Neovim, put `plugins/nvim` on the runtime path and call
   `require("coppice").setup({})`. Block a pane with the fake ask from the plan 23 check. Run
   `:Coppice`. The float heads with `coppice · 1 need you`, and the blocked row is first, in
   the error colour, with the ask after the label. Space opens a split with the pane's
   screen, and the split takes no edits. Run `:Coppice` again and press Enter on a row. A
   terminal buffer in a new tab attaches, and keys reach the pane. ctrl-space ends the job.
   Esc closes the float. With the server stopped, `:Coppice` says it cannot reach the server.
   Enter under the flatpak Neovim needs `coppice` inside the sandbox, so use a Neovim that
   runs on the host for this step.
3. tmux. Inside tmux, run `coppice tmux-mirror`. One window per open pane appears, named by
   its label, each running `coppice attach`. Your own windows keep their names. The status
   line on the right reads the roster count, and `quiet` when nothing needs you or works.
   Block a pane. Within a second the count reads `1 need you`. Close a pane with
   `coppice pane close`. Its window goes. Close a mirrored window yourself. It stays closed.
   Create a pane labelled `x'; kill-server; #{session_name}`. Its window takes that name as
   text, and tmux keeps running. Set the session's own status line first with
   `tmux set-option status-right mine`. ctrl-c stops the mirror, the windows stay, and the
   status line reads `mine` again. Start it again and close the terminal it runs in, or send
   SIGTERM to its process group. The status line reads `mine` again. Run two mirrors on one
   session and stop both. The status line reads `mine`, not a count. Runs on a private tmux
   3.7b server on 2026-09-23 did all of this except the blocked count. One of them found the
   session through `TMUX_PANE` with no `--session`. SIGINT, SIGHUP and SIGTERM to the group
   each put `mine` back, and so did two mirrors stopped one after the other.
4. Herdr is not on this box, so none of this has run. Install Herdr. Run
   `daisugi coppice herdr-bridge <pane>`. It prints the manifest path under
   `~/.config/herdr/agent-detection/` and a `herdr pane run` line. Run that line against a
   Herdr pane. The pane shows the coppice pane with the attach status line on the last row,
   and keys reach it. Block the coppice pane. Check whether Herdr marks its pane blocked.
   Herdr picks a manifest by the foreground process, and a new agent id may need a Herdr
   update, so it may not. Write down what Herdr does, run `herdr server
   reload-agent-manifests` and look again, and then set `verified` in the bridge and in
   `herdr_verbs.json` only for what held.

## Plan 25 hand check

A foreman's children are one level down, and a child's ask goes to its foreman first. The
tests fake every ask and never run a harness, so a real foreman working a real team is the
part left to a person. Nothing below ran yet. Rebuild the binary and restart the server with
`coppice server stop`, then `coppice server start`. Restart the web server too for step 5.

To make a fake undoable ask on a pane `<id>`, use the plan 23 line with `"tier":"undoable"`
and a deadline at least 200 seconds out, and write `asks/hand_1.json` under the gate root with
`{"nonce":"hand","tier":"undoable"}`. A hold ends 30 seconds before the ask's deadline, and the
gate's own default wait is 90 seconds, so a real gate ask with the default is held for 60
seconds, not 120.

1. Make the team by words. Open the floor with the config's foreman set. Say to the foreman:
   `make a task review-team with two shell panes under it, one child task each, and run
   date in each`. The page teaches `task create` and `pane create --task`, so it should need
   nothing more. Write down what it does. Count the foreman's turns from the first sentence to its report: each
   command it ran is one dim note. Write that number here. It is the first measure of what a
   team costs.
2. Type `tree`. The team and its two children show, each child with its pane. Enter on
   `review-team`. The bar reads `coppice › review-team · quiet` and the roster shows only the
   two panes. Esc pops back to every pane. Type `tree` while pushed. Only the team's subtree
   shows.
3. Push into `review-team` and type `foreman claude`, or any harness from the config. A pane
   labelled `review-team-foreman` opens in the task, gets the page once it is idle, and
   `coppice task list --json` shows it as the team's `foreman`. The floor's own foreman in the
   config file does not change. Send the fake undoable ask to one child pane. The child's row
   sits under WORKING as `waiting on review-team's foreman · now`, and the bar count stays at
   zero. If the team foreman is idle, one line appears at its prompt with the pane, the ask id,
   and the get and deny commands, and never the ask's text. No phone push comes, from the
   built-in publisher or from the notify plugins. After 120 seconds the row moves to NEEDS YOU, the count reads `1 needs you`, the
   foreman gets a second note, and the phone push comes then, once.
4. Send a new fake ask with id `hand_2` and write its ask file. Tell the team foreman in its
   tile to refuse it. It runs `coppice agent deny <id> hand_2`, the answer file under the gate
   root says `deny` with `denied by the foreman review-team-foreman`, and the row leaves
   WORKING's waiting line. Ask it to allow the next one. The server refuses:
   `a pane can propose. It cannot allow.` Send a fake ask with `"tier":"permanent"`. It goes to
   NEEDS YOU at once, and the foreman still gets the note.
5. On the floor page, the rail shows `review-team` above the panes. Click it. The rail shows
   only the team's panes under `coppice › review-team`, the queue count is the team's, and
   the tiles keep their panes. `‹ back` pops. A held ask shows as a working row with the
   waiting line, and the tab title leaves it out of its count.

6. With a held ask, run `:Coppice` in Neovim. The head leaves the held ask out of its count,
   and the row sorts as working. The spec covers this and ran 14 passed under the flatpak
   Neovim. The tmux mirror's status line counts a held ask as working too.

Record the turn count from step 1, and whether the foreman needed the task verbs, here when
the check runs.

## Plan 26 hand check

Every connection can carry a name, every allow and deny names who gave it, and the roster
says who is looking. The tests fake the browsers and the gate, so two real people on one floor
is the part left to a person. Nothing below ran yet. Rebuild the binary and restart the server
with `coppice server stop`, then `coppice server start`, so the phone server inside it runs the
new code.

1. Mint two names. Run `coppice web token --for alice` and `coppice web token --for bob`. Each
   prints `name`, a token, and a sign-in QR. Run `coppice web token list`. It prints `alice`
   and `bob` and no token. A second `--for alice` prints the same token until `--rotate`.
   `coppice web token --for "two words"` refuses with `names are one word, up to 32
   characters`.
2. Sign in as alice in one browser window and as bob in another, both wide enough for tiles. A
   phone gets no tiles, so it shows no looking line. Open the same pane in a tile in both
   windows. Alice's tile header reads `· bob looking`, and bob's reads `· alice looking`.
   Neither page shows its own name. The terminal floor row for that pane reads
   `· alice, bob looking` at its next poll. Close bob's window. Alice's header drops bob within
   a second.
3. Open a view on bob's floor page that watches the pane. Bob counts as looking while the view
   is open, since a view looks through the floor page.
4. Send the fake undoable ask from plan 23 to that pane and write its ask file. Allow it from
   alice's page. The answer file under the gate root carries `"by":"alice"` and
   `"whoFrom":"token"`. With a real gate ask in its place, the shadow journal line for that
   call carries `allowed_by` `alice` and `who_from` `token`.
5. Send a second fake ask. Deny it from the terminal with `coppice agent deny <id> <ask>`. The
   answer file carries `"by":"local"` and `"whoFrom":"none"`, since the CLI sends no name.
   The journal never holds an empty name.
6. Run `coppice web token --revoke bob`. Bob's open page keeps its websocket until it drops,
   since the token is checked when the socket opens and on each `/api` request. Reload bob's
   page. It stops at `That token is not accepted`. Alice's page works on.
7. Optional: run a standalone `coppice web serve` beside the server. An allow from its page
   still writes `token`, since its own answer route checked the token. The names on its
   websockets count as `socket`, since only the phone server inside the coppice server can
   vouch for a token on the socket.

Record here whether the header line was easy to see on the phone, and how long bob's tab took
to leave alice's header.
