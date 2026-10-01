# coppice end-to-end checks

These checks use coppice the way a person does. They start a real server,
open the web floor in a real browser, and drive the TUI in a real
terminal. The Go tests in `internal/` check each part alone. This suite
checks that the parts still fit together.

No real agent, model or upstream is involved. Each harness is `bash`,
`fake-trust.sh`, which draws claude's recorded folder trust screen from
`testdata/screens/claude/blocked-10.txt`, or `fake-foreman.sh`, which
stands in for a claude foreman: it writes an invented transcript under a
scratch `CLAUDE_CONFIG_DIR`, names it from inside its pane, and answers
each line typed into it with a reply that names `e2e shell`. Gate facts
come from `rpc.mjs`, which speaks the socket protocol the way a gate hook
does.

## Run it

You need Go, `pkg-config` with libghostty-vt (see `scripts/toolchain.sh`),
Node 22 and bash.

```sh
cd harness/coppice/e2e
npm ci                                      # playwright, pinned in package-lock.json
npx playwright install chromium             # once per playwright version
./run.sh                                    # web and tui; or ./run.sh web, ./run.sh tui
```

`run.sh` builds coppice into the scratch directory. To test a coppice you
built already, set `COPPICE_BIN=/path/to/coppice`.

Everything goes into `E2E_DIR`, which defaults to
`${XDG_CACHE_HOME:-$HOME/.cache}/coppice-e2e`. Use real disk: on some
systems `/tmp` is RAM. The directory holds a scratch `HOME`, the XDG
directories, the server's socket (`run/c.sock`), its data directory, the
server and web logs, and `shots/`. `run.sh` clears it when a run starts
and keeps it after the run, so you can read it. The socket path must stay
under the 108-byte limit of a unix socket, so keep `E2E_DIR` short.

The suite never touches your own coppice server or config. Every call
names the scratch socket and data directory, `COPPICE_NO_AUTOSTART=1` is
set, and `HOME` and the XDG directories point into `E2E_DIR`.

## What it checks

`run.sh` starts the server and makes three panes: `e2e shell` and `e2e ask`
run bash, and `e2e trust` runs `fake-trust.sh` as a `claude` pane. The
`claude` table itself runs `fake-foreman.sh`.
Then `rpc.mjs` holds an undoable gate ask for `git push origin main` on
`e2e ask`. So the floor has two panes that need you.

`tui.sh` runs first. It starts the coppice TUI inside a coppice pane of
100x36, so the server's own virtual terminal (libghostty-vt) is the screen
and `coppice pane read` returns the text on it. At 100 columns the TUI has
no room for a window beside the rail, so Space opens a peek.

| Check | What passes |
|---|---|
| roster | the rail lists `e2e shell`, `e2e ask` and `e2e trust` |
| needs | the header says 2 need you |
| the peek on the held ask | shift-tab and Space show `Deny is the default.` and `y allow once  t allow for this task  n deny` |
| the peek on the trust screen | shift-tab and Space show `Claude asks to trust this folder.` and its keys |

Each screen it read is saved as `shots/tui-*.txt`.

`web.sh` then serves the floor on `127.0.0.1:18490` (set `E2E_PORT` to
change the port), and `web.mjs` checks it in a 1440x900 window:

| Check | What passes |
|---|---|
| the floor loads and signs in | the page opens with the token in `#t=` and draws the roster |
| the roster lists every pane | all three labels show in `#roster` |
| a tile shows the pane live and takes typed keys | typing `echo typed-in-a-tile` and Enter into the shell's window shows the command and its output there |
| a gate ask shows its ask bar | the `e2e ask` window shows the summary, Deny and Allow |
| the trust screen shows both answers | the `e2e trust` window shows Trust this folder and Not now. A click on Trust this folder makes coppice move the cursor to yes and press Enter; the fake then draws a prompt box, the bar goes, and the pane does not end |
| at 1440 the chat bar sits at the foot of the rail | the bar is in the rail, and no chat and no agents fold show on the desk |
| the floor starts the fake foreman | `floor.foreman` with the `claude` harness, and no sentence: the chat holds the tracked foreman's replies |
| a phone-sized page shows the roster in its fold | at 390x844 a tap on the agents fold shows the roster, and the page does not scroll sideways |
| the phone chat shows the fake foreman's reply | the reply that names `e2e shell` shows in the chat |
| home reads from the top | the facts line, the cards that need you, the fold, then the chat; the chat bar ends at the bottom of the screen |
| a sentence shows as sent, then read | a sentence sent from the bar shows as `sent`, then as `read` once the fake foreman's reply comes |
| a chip opens the agent's sheet | a tap on the `e2e shell` chip slides its sheet in; `echo typed-in-a-sheet` and Enter show there; a swipe right goes back with no reload, and the page never scrolls sideways |
| the page raised no script errors | no uncaught error and no `console.error` |

The floor has fewer windows than panes, so a check clicks a pane's roster
row when no window shows it. Each check saves a screenshot to
`shots/web-*.png`, and a failed one saves `shots/web-FAIL-<check>.png`.

## In CI

The `e2e` job in `.github/workflows/clients.yml` builds the release
tarball with `scripts/release.sh`, smoke-tests it with
`scripts/smoke-tarball.sh`, and runs `run.sh` against the tarball's
coppice. It uploads the screenshots, the TUI screens and the logs as the
`e2e` artifact.
