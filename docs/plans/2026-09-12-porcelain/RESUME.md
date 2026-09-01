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
row under the cursor, ctrl+a d comes back to the roster, and q leaves. `daisugi coppice floor`
execs the same binary. The Textual floor screen is gone; `:floor <backend>` in the cockpit is
still the swap alias.

## Plan 15 hand check

Terminal and phone now share one shape: the roster plus as many live tiles as the width
allows. The tests drive both over pipes and a fake DOM, so two hand steps remain. In a
terminal at least 120 columns wide, run bare `coppice`, press Space on a row and watch the
tile beside the roster go live, click a row and a tile, then type `swap 1 2`, `rotate`,
`lock`, `unlock`, `layout all`, and `reset` on the prompt. On the phone, open the floor at a
phone width, a tablet width, and a laptop width: no tile, one tile, two or three tiles, with
the ask bar amber on a blocked pane and Deny and Reply on it. A tile never resizes the pane
it shows. `t` opens a tile at an empty prompt; the six keys of plan 17 will settle the map.
