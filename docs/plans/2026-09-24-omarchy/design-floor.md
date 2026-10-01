# Design: the floor people love, the pass after part 1 (roadmap 1a to 1i, the phone)

Status: **Proposed**, 2026-10-08. Design only. Nothing here is built.

This file does not repeat `spec-part1-floor.md`. That spec is built: the ledger says
"PART 1: COMPLETE" (T1 to T8 merged, then the final-review fix batch). This file lists what
the owner would still meet in a park or at the desk, with file and line evidence, and the
design that closes each gap. Where this file and the spec disagree, the owner rules, and the
spec changes in the same commit as the code.

The ledger records **no run** of the owner's hand checks in `RESUME.md`. Several gaps below are
guesses from code until they are. The plan (`plan-floor.md`) makes them its last chunk and
also asks for a friction log from the first real week.

## The owner's verdicts, as tests for every chunk

1. Windows are live and typeable. A window is never a picture of an agent.
2. Overview and control are one screen. On a phone an agent slides in over the overview.
   Nothing is a separate page.
3. One click makes a new agent with defaults.
4. Voice works with no setup step.
5. The floor leads with live work. It is never "a settings page, not a coding interface"
   (field report, `docs/plans/2026-08-26-daisugi-interface-two-lenses.md:78-86`).
6. From a park bench, the owner runs all projects from the phone by chat. The foreman
   (`design-foreman.md`) is the chat. The phone page hosts it.

## The lens: two poles and a ramp

From `docs/research/tool-interface-doctrine.md`:

- **The phone is the infant-simple pole.** One text box and one mic, at the bottom, under the
  thumb. You say what you want. The foreman does it. The agents that need you rise to the top
  as cards with their answer buttons. No settings on the home.
- **The desktop is the expert instrument.** Dense windows, keys straight to the agent, number
  keys, the stack bar on every window, colour as health only.
- **The ramp is the same floor on both.** The phone's chat is also a window on the desktop.
  The phone's agent cards are the desktop's rail rows. Every key and verb is printed where it
  works (the footer, the bar hints, the menu). A person who starts on the phone meets the
  same names on the desktop.
- **Honesty.** A control that does nothing is not drawn as a control. Today the stack bar
  says "Swap is not built yet." (`stackbar.js:13`). Keep that rule: each chunk either builds
  a control or keeps the honest note.
- **Say what changed.** Every action ends in one line on the status row: what is now true.

## How Go and Rust share each change

The Rust coppice embeds the Go tree's web page byte for byte
(`harness/coppice-rs/PINS.md`, "The phone page"; `harness/coppice-rs/build.rs:116-139`). So
each change has one of three costs. Every chunk in the plan carries its tag.

| Tag | What changes | Go | Rust | Tests |
|---|---|---|---|---|
| **web** | `harness/coppice/internal/web/static/*` only | one copy | none, it embeds the same files | node `_tests/*.test.mjs` through `internal/web/js_test.go` (`TestJSUnitTestsPass`), and `e2e/web.mjs`, which CI runs on both binaries |
| **verb** | a protocol verb or event, server state | `internal/server`, `internal/web` | `src/server`, `src/web` | Go unit tests, Rust unit tests, new cases under `harness/coppice-rs/cases/<slice>/*.jsonl` replayed by `clients/coppice_compare.py` with 0 disagreements |
| **tui** | the terminal floor | `internal/tui` | `src/tui` | Go and Rust unit tests, `harness/coppice-rs/cases/tui/*.jsonl`, and `e2e/tui.sh`, which `.github/workflows/clients.yml:443-447` also runs on the Rust binary |

What the end-to-end suite checks today (`harness/coppice/e2e/README.md`): the TUI at 100x36
(roster, "2 need you", the peek on an ask and on the trust screen), and the web at 1440x900
(sign-in, roster, a typeable window, the ask bar, the trust bar, no script errors). At
390x844 it checks **only** that the roster shows and the page does not scroll sideways. Every
phone chunk below adds a real phone check to `e2e/web.mjs`.

---

## 1a. One or two commands

**Shipped.** `coppice` alone dials (and so starts) the server and opens the floor
(`internal/cli/cli.go:165-166`, `runFloor` at `:1087`). The first run asks at most one
question (`internal/tui/firstrun.go:22-50`). `coppice web` alone serves on loopback, signed
in, and opens the browser (`internal/cli/web_open.go:22-26`). `install.sh --link` puts
`coppice` on PATH.

**Gaps.**

- The phone needs four steps and a web console. `coppice web cert tailscale NAME`
  (`internal/cli/web.go:202-235`), then `coppice web serve --tls tailscale`, then turning on
  MagicDNS and HTTPS in the Tailscale admin console (`web.go:231`), then scanning the QR. The
  owner must know the MagicDNS name.
- The certificate expires in 90 days. "Renewal is yours." (`web.go:233`, `internal/web/tls.go:32`).
  In the park, an expired certificate is a dead floor with no fix button.
- `coppice web serve` listens on `:8443` on every interface by default (`web.go:268`), and its
  CA handout listens on `:8080` in plain HTTP on every interface (`web.go:273`). The token
  guards the floor, but a café or home LAN still sees the ports.

**Design: `coppice web serve --tls tailscale` does the whole job.** The roadmap keeps the
phone setup under `coppice web serve` (`ROADMAP.md`, 1a), so this is the existing flag made
complete, not a new verb. With `--tls tailscale` and no `--cert` or `--key`:

- It reads `tailscale status --json` for this machine's MagicDNS name and tailnet address.
  With no tailscale on PATH, it says so in one line and names `--tls localca` (the LAN path,
  as today).
- It runs `tailscale cert` for that name when the pair is missing or has less than 30 days
  left. The server repeats that check once a day while it serves, and the floor header shows
  a warn mark 7 days before expiry, with a Renew button.
- It listens on the tailnet address and loopback only, unless `--listen` is given. No CA
  handout port: a Tailscale certificate needs none.
- With `--persist` (which exists), the next `coppice` start serves the phone too.
- It prints the QR with the token in `#t=` (as today) and one line: what now listens where.
- The other `--tls` sources keep their present behaviour.

On screen the owner sees: one command, one QR, one line. Changes: `internal/cli/web.go`
(`webServeCommand`, `:266`), `internal/web/tls.go` (expiry read), `internal/web/server.go`
(daily check, header fact `phone_cert_days`). Rust: `src/cli/web.rs`, `src/web/serve.rs`,
`src/web/config.rs`. Tests: the fake `tailscale` in
`harness/coppice-rs/cases/web/fixtures/bin/tailscale` grows `status --json` and a dated
`cert`; new cases in `harness/coppice-rs/cases/web/phone.jsonl`. Owner question 1 decides
whether the tailnet-only bind is the default; the chunk builds it behind one switch.

## 1b. What a thing is

**Shipped.** Spec S1 in full: ended agents leave the floor for Recent, Resume, Resume all,
Clear all, the seven-day expiry, `pane list --ended`, `pane forget`, `pane resume`
(`internal/server/ended.go`, README "Recent").

**Gaps.**

- After a restart nothing tells the owner that agents can come back. `server.status` has a
  `resumable` count, but neither the web nor the TUI reads it (no match for `resumable` in
  `internal/web/static/*.js` or `internal/tui/*.go`). The Recent fold is closed by default.
  The owner sees an empty floor and thinks the work is gone.
- The roadmap names `coppice agent forget` (`ROADMAP.md:52`). The CLI has only
  `pane forget` (`internal/cli/verbs.go:78`). On screen the word is agent; in the CLI it is
  pane. A person who reads the floor and types `coppice agent forget` gets an error.

**Design.**

- After a start that found resumable records, the status row says once:
  `4 agents stopped when coppice restarted. [Resume all] [Show them]`. Resume all does what the
  Recent button does. Show them opens the fold. The TUI prints the same line with `R resume
  all` in the footer until a key is pressed. **web** + **tui**.
- `coppice agent forget|resume` and `coppice agent list --ended` become the same verbs as
  their `pane` forms. The help text lists the agent form first. **verb** (CLI only; Go
  `internal/cli/verbs.go`, Rust `src/cli/verbs.rs`; cases in `cases/cli/verbs.jsonl`).

## 1c. One click, a new agent

**Shipped.** New starts the default harness near the selected agent in one click
(`newpane.js:14-31`). The menu lists projects, pinned first. The TUI has `n` and `N`.
Labels are automatic (`claude-trellis`, `-2`). Rename in place.

**Gaps.**

- A pinned project that moved shows as a normal menu entry. `project.list` rows carry only
  `path`, `name` and `pinned` (`internal/server/projects.go:171-172`). The click then fails
  with the server's "gone" text (`panes.go:803`). The roadmap asks that a missing directory
  is named as such.
- The default directory falls through silently. With no `near`, a missing newest recent
  directory is skipped for the next one (`projects.go:175-185`). The agent starts somewhere
  the owner did not choose, and the status says only "Started claude-x." (`newpane.js:24`).
- Ten projects are not one keystroke each in the TUI picker: number keys reach 1 to 9.

**Design.**

- `project.list` rows gain `missing: true` when the directory does not exist. The menu draws
  such a row dim with "(missing)" and one action, "Remove from the list", which runs
  `project rm` through a new verb `project.remove` (operator only). **verb** + **web** + **tui**.
- The status line after New names the directory, and says why it was chosen when it was not
  the near agent's: `Started claude-glean in ~/src/glean (your last project).` **web** + **tui**
  (the reply already carries `cwd`; it gains `why: near|recent|pinned|start`). **verb**.
- In the picker, `0` is the tenth project. Each picker row shows its agents' state counts,
  `trellis  1 needs · 2 working`, so the picker is also a map of the ten projects. **tui** + **web**.

## 1d. One screen

**Shipped.** Header, rail, windows that fill the area at a readable size, start fill order
and start focus, keys to the selected window with a thick border and "typing here", the
rail tree, × kill with one confirm, drag, number keys, overlays for views and the journal,
the phone sheet over the overview (`dock.js:1-9`, `app.js:378-405`).

**Gaps.**

- **Two screens are still pages.** The full New form and Settings are screens of their own.
  `show()` hides the floor for them (`app.js:382-386`; the sections are
  `index.html:110-144`). On a phone they replace the overview. This breaks verdict 2.
- **The TUI under 120 columns has no window.** A window needs 80 columns
  (`internal/tui/render.go:39-42`) beside the rail, so at 100 columns Space opens a peek, which
  is a picture, not a typeable window (the e2e suite relies on this: `e2e/tui.sh:14-17`).
  A laptop split or a phone over SSH is often 90 to 119 columns.
- **The pinned colony strip halves the windows at 1440 px** (ledger, T4c parked).
- **Subtasks are not nested in the TUI tree** (ledger, T6 parked).
- **The tell bar sits at the foot of the rail** (`index.html:47-52`), under every agent row.
  On a phone with ten agents it is off screen. It is the foreman's input, so it belongs at the
  bottom edge of the screen, always.

**Design.**

- The New form and Settings open in the overlay panel the views already use (`#overlay`,
  `app.css:301-315`): at 900 px and wider, a panel over part of the windows; under that, a
  sheet that rises from the bottom. The routes `#/new` and `#/settings` stay as links that
  open the overlay, as `#/view/` already does (`app.js:5-6`). **web**.
- **Slim rail in the TUI.** From 88 to 119 columns the rail draws as a strip of 6 columns per
  row: the window number, the state dot, and two letters of the label. One window of at least
  80 columns then fits. The selected row's full name shows in the window header. Below 88
  columns the peek stays. **tui**.
- The colony strip takes at most 2 text lines of height and never removes a window; when it
  would, it draws as one line of dots. **web**.
- Subtasks nest under their task in the TUI tree, as on the web. **tui**.
- The tell bar becomes the **chat bar**: docked at the bottom of the screen on a phone, and at
  the bottom of the rail on the desktop, above Recent. See "The phone page".

## 1e. Voice with no setup

**Shipped.** The coppice server starts `daisugi voice serve` itself, restarts it once, and
says why when it is down (`internal/server/voice.go:53-60`, `:248-290`). The web mic works
with no flags. The TUI talk key records while held (kitty protocol) or press to start and
press to stop. The text lands in the input, not sent. The voice engine stays loaded between
clips (ledger, VOICE RESIDENT).

**Gaps.**

- **The fix text is for the Python daisugi only.** Every voice fix says
  `pip install 'opendaisugi[voice]'` (`voice.go:103-104`, `:261`, `:270`, `:385`;
  `messages.js:50-51`). The Go and Rust daisugi now serve voice with no Python (ledger,
  MOONSHINE: "no-setup voice serve in ports"), with the engines from the `opendaisugi-voice`
  package. A tarball user who follows the pip line installs a second daisugi.
- **The first start is a silent minute.** "The first start can take a minute while the model
  loads" (README "Voice"; `voice.go:61`). The page shows "starting" with no size, no progress
  and no end.
- **The phone mic needs a secure context.** Over plain HTTP from another machine the browser
  gives no microphone at all. The page does not say that this is why. `coppice web` is
  loopback, so the desktop is fine; the phone is not, unless it came through `--tls tailscale`.
- **The "voice extra" banner shows on every load** (ledger, T5 polish), before the owner has
  pressed Mic.
- The TUI hold-to-talk path has never had its hand check (ledger, T7).

**Design.**

- The supervisor asks the daisugi it found which kind it is, with `daisugi voice serve --help`
  output it already runs as a probe (`voice.go:264-271`), or better a small
  `daisugi voice status --json` that all three daisugi binaries answer the same way
  (`{"engine": ..., "model": ..., "model_bytes": ..., "fetched": bool, "kind": "python|go|rust"}`).
  The fix then names the right install line for that kind. **verb** (coppice, both ports)
  plus a daisugi change in Python, Go and Rust, with voice compare cases.
- While the first model is fetched, `voice.status` carries `fetch: {done, total}` and the mic
  says `Getting the speech model, 41 of 160 MB.`, then `Voice is ready.` **verb** + **web** + **tui**.
- When `window.isSecureContext` is false, the mic button says `The mic needs https here.`
  with the fix `coppice web serve --tls tailscale` named in words (the fix is on the box, not on the phone). **web**.
- The voice banner shows only after the first Mic press. **web**.
- Voice reaches the chat bar too: the mic beside the chat box fills the box, not sent, as it
  does for an agent.

## 1f. daisugi on every agent

**Shipped.** The gate mark with the last verdict and its clause (`stackbar.js:88-97`),
tokens summed from the transcript or session tree (`internal/server/facts.go`), the journal
overlay (`overlay.js`).

**Gaps.**

- **Tokens saved are not shown.** Nothing in `internal/server/facts*.go`, `stackbar.js` or
  `internal/tui/facts.go` reads or names a saving. The roadmap asks for "tokens spent and
  saved". The gateway is the only part that knows a saving.
- **The journal has no history.** "The server keeps no verdict history." (`overlay.js:17`).
  A page opened after a deny never sees it. The phone, which opens and closes all day, sees
  almost nothing.

**Design.**

- The server keeps the last 500 verdicts it was told, in a ring per floor, written to
  `<data dir>/journal/verdicts.jsonl` (0600, one file, trimmed at start). A new verb
  `floor.journal {pane?, since?, limit?}` returns them newest first. The overlay reads it;
  the `JOURNAL_SOURCE` note changes to say what it now lists. **verb** + **web** + **tui**
  (the TUI gets `j` for the journal over the windows).
- Tokens saved come from the gateway, not from a guess. The floor facts read the gateway's
  own savings figure where it reports one per caller. Where it reports none, the header shows
  spent only and no saved number. The chunk agent first checks what the gateway exports today
  (its status and `/metrics`); if it has no per-pane figure, the chunk adds one to the
  gateway in all three daisugi binaries, keyed by the pane id the pane's env carries. **verb**.

## 1g. The stack bar

**Shipped.** One bar per window: loop, daisugi and mode, router, model. Colour is health
only; words are the choice; an outline marks the open segment (`stackbar.js:44-86`). The
moving light (`light()`, `stackbar.js:99-107`). The floor header bar.

**Gap: no segment swaps anything.** `SWAP_NOTE = 'Swap is not built yet.'` and
`MODE_NOTE = 'Changing the mode from here is not built yet.'` (`stackbar.js:12-15`). The
roadmap asks: the daisugi mode now, the model and router from the next turn, the loop by
moving the conversation with the replay cost shown first.

**Design.** Each swap is honest about when it takes effect.

| Segment | What the owner does | What happens | Effect |
|---|---|---|---|
| daisugi mode | opens the segment, picks enforcing or watching | coppice writes a per-agent mode marker the gate reads on its next call: `<gate root>/agents/<pane>.mode`. Off is never offered here: turning the gate off stays a command with words. Enforcing to watching asks once: "Watch only? daisugi will log and not stop calls for <label>." | **now**, the next tool call |
| model | picks a model from the harness's own list | claude: coppice queues `/model <name>` through the ready queue, so it lands only at an idle prompt. sprig headless: the next `agent.prompt` carries `model`. Others: the segment keeps `SWAP_NOTE`. | **next turn** |
| router | picks direct, gateway or switchyard | the base URL is in the agent's env, which a running process cannot change. So this is a resume with new env: the bar first says `Restarts <label> and resumes its conversation.` with Resume and Keep. | **after a resume** |
| loop | picks another harness | a new agent of that harness in the same directory gets a handoff brief built from the transcript tail. The bar first shows the replay cost: `Moves about 38k tokens of conversation to codex.` Move and Keep. The old agent stays until the owner closes it. | **a new agent** |

The mode marker is a daisugi change in Python, Go and Rust: the gate reads the marker for
the calling pane (it already knows the pane from `COPPICE_PANE` and the caller check), and
the marker can only lower enforcing to watching or raise it back, never turn the gate off
and never change an envelope. Gate compare cases cover it. The coppice side is a new verb
`agent.set_mode {pane, mode}` (operator only, refused from panes like `agent.allow`).
**verb** + **web** + **tui**. The swap rows that are not built keep their honest note.

## 1h. Every error has a fix button

**Shipped.** Messages carry actions (`messages.js`); a shell fix opens a shell agent with the
command typed and not run; the scan tests in `_tests/messages.test.mjs:130` and `:160` fail a
client string that names a `coppice` or `daisugi` command with no action.

**Gaps.**

- The scan covers the web client only. Server text reaches the page through `f.fail(err)`
  and `toMessage`, and the TUI prints its own lines. Neither is scanned.
- The scan knows only `coppice ` and `daisugi `. The voice fix names `pip install`, and the
  phone fixes will name `tailscale`.
- Some fixes live on another machine: "turn on Tailscale on this phone", "the box is asleep".
  The page says it cannot reach the box (S7) but not what the owner can do from the phone.

**Design.**

- One shared list of fix words, `testdata/fixwords.json` (`coppice`, `daisugi`, `pip`,
  `tailscale`, `systemctl`), read by the node scan, by a new Go test over `internal/server`
  and `internal/tui` string literals, and by the Rust mirror of that test. A string that names
  one and has no action fails. **web** + **verb** + **tui**.
- The cannot-reach line on the phone says, in words, the two fixes that are on the phone:
  `Turn on Tailscale on this phone.` when the page was loaded from a `.ts.net` name, and
  `The box may be asleep or off.` with Retry. **web**.

## 1i. Readable windows

**Shipped.** Canvases draw at the device pixel ratio; the cell comes from the measured
advance; no cell is narrower than 7 px (`windows.js:5-7`, `cellFor` at `:37-45`).

**Gap: the phone.** The page never resizes the agent: "watches and never resizes"
(`pane.js:9-10`). A pane defaults to 120 by 40 (README "Use"). At the 7 px floor that is about
840 px of text in a 390 px sheet, so the owner scrolls sideways through Claude's screen to
read one answer.

**Design.** Two parts.

- **The reading view.** For an agent whose transcript the server already reads (claude, and
  sprig's session tree), the phone sheet opens on a reading view: the conversation as
  messages, wrapped to the phone, from the new `pane.messages` verb (see the phone page). A
  line at its top says `From the transcript. [Screen]`; one tap shows the live screen.
  Asks and the trust screen come from screen detection and gate reports, not from the
  transcript, so their bars draw over the reading view exactly as they do now. The compose bar
  stays: the view is typeable. For an agent with no transcript, the sheet opens on the screen.
- **Fit when alone.** `pane.attach` gains `fit: {cols, rows}`. The server resizes the agent to
  it only when no other client is attached, and puts the old size back when that client
  detaches or another attaches. The phone sends it for the screen view. The reply says
  `fitted: true|false`, and the sheet says `Sized to this phone.` or
  `Also open on the desktop, so not resized.` **verb** + **web**.

---

## The phone page

### What the owner sees

Home is one column. From the top: the facts line, the agents that need you as cards with
their answer buttons, a fold with every agent, then the chat with the foreman. The chat bar is
docked at the bottom, above the keyboard. Tapping an agent anywhere (a card, a row, a chip in
the chat) slides its sheet in from the right. The overview stays in sight at the left edge.
Swipe right or tap the edge to go back. Nothing loads a new page.

```
+--------------------------------------+
| coppice   2 need you · 5 working  ⚙ |  facts line; ⚙ opens settings as a sheet
+--------------------------------------+
| ! glean-import  needs you            |  needs-you cards, top of home
|   git push origin main               |
|   [ Deny ]              [ Allow ]    |
| ! claude-trellis  trust this folder? |
|   [ Not now ]   [ Trust this folder ]|
+--------------------------------------+
| ▸ 7 agents · 3 projects              |  fold; opens the rail tree in place
+--------------------------------------+
| you 14:02                            |
|  start the glean import fix and      |
|  check trellis's failing test        |
| foreman 14:02                        |
|  Started two agents:                 |
|  (glean-import ›) on the CSV parser  |  chip opens the agent's sheet
|  (claude-trellis ›) on test_sync     |
| work 14:09                           |
|  claude-trellis: test fixed, 1 file  |
|  changed. Waiting on you to push.    |
| foreman 14:09                        |
|  trellis is done. glean needs an     |
|  answer, above.                      |
+--------------------------------------+
| [ Tell the floor...      ] (mic) (>) |  chat bar, docked at the bottom
+--------------------------------------+
```

The sheet, slid in over the home:

```
+--+-----------------------------------+
|  | ‹ claude-trellis   ● working      |
|  | claude · enforcing · gateway ·    |  stack bar, wraps on a phone
|  |   opus-5-5 · ✓ Edit · 41k tok     |
|o | From the transcript.   [Screen]   |
|v |-----------------------------------|
|e | you                               |
|r |  fix test_sync, it fails on CI    |
|v | claude                            |
|i |  The test reads a clock. I made   |
|e |  it take a fake clock and ran the |
|w |  suite: 212 passed.               |
|  |  · ran pytest tests/test_sync.py  |  tool use as one dim line, no output
|  |-----------------------------------|
|  | [Keys] [ Type here...  ] (mic)(⏎) |  compose bar, as today
+--+-----------------------------------+
```

The left strip is the overview (`#sheet-edge`, `app.css:287-289`). The ask and trust bars
draw over the reading view when the agent needs you, as they do over the screen today.

### What the owner does

- Types or says one sentence in the chat bar. It goes to the foreman through `floor.talk`
  (`internal/server/foreman.go`), which starts the foreman if none runs. The sentence shows
  in the chat at once, marked `sent`, then `read` when the foreman's turn starts.
- Answers asks from the cards without opening anything.
- Opens an agent from a chip in the chat or from the fold, reads it, types to it, swipes back.
- Plumbing words in the chat bar need a leading slash: `/claude`, `/close docs`. A plain
  sentence is always talk. Today a sentence led by a harness name opens that harness with the
  rest as arguments, and a short sentence led by `close` closes an agent
  (`classifyTell`, `newpane.js:237-245`; `plumbingCommand`, `:274-293`). In a chat box under
  a thumb, "claude, can you check trellis" would start a new claude. The terminal floor's
  prompt line keeps its words. (Owner question 2.)

### Where the chat comes from

The chat must work before foreman M1 to M4 exist, and keep working after. So the page reads a
message list that does not care which foreman is behind it.

- **New verb `pane.messages {pane, since?, limit?}`.** It returns
  `{messages: [{id, at, role, text, tool?}], source: "transcript"|"log", more: bool}`, where
  `role` is `owner`, `agent`, `work`, `note` or `past`. It reads only a transcript path the
  pane's own hook reported, the same rule `facts.go` keeps for tokens, and only the tail
  since `since`. Text is cleaned of control characters and capped per message. Tool use is
  kept as its tool name and a one-line summary of its input (the command, the file path),
  never its output, because outputs can hold secrets. An event `messages` (subscribe kind
  `messages`, a bare name like `state` and `note`; ruled 2026-10-09) tells subscribers that new
  ones exist.
- **New verb `floor.chat {since?, limit?}`.** It is `pane.messages` on the tracked foreman,
  plus the owner's own lines. The server writes each `floor.talk` sentence to
  `<data dir>/chat/YYYY-MM-DD.jsonl` (0600) before it types it, so the owner's words show
  even while the foreman starts, and survive a foreman that ended. A foreman that ended and
  a new one started show as one chat with a `note` line between them: `A new foreman
  started.`
- **Source today:** the Claude transcript of the foreman pane (user entries are `owner`,
  assistant text blocks are `agent`). sprig's session tree maps the same way.
- **Source later:** when foreman M1 writes its own append-only log, `floor.chat` reads that
  log instead, and its kinds (`owner`, `work`, `note`, `past`) map one to one. The page does
  not change. `past` messages (imported chats) are hidden on the phone by default behind
  `Show imported chats`.
- `work` messages: when an agent the foreman started ends or reports, the server adds a
  `work` line that names it, so the chat carries the chips. This is the same report M3 adds
  as "[Name] report".

Both verbs are **verb** chunks, in Go and in Rust, with fixture transcripts under
`harness/coppice/testdata/transcripts/` (invented content, never a real transcript) and
compare cases under `harness/coppice-rs/cases/chat/*.jsonl`.

### Who may read the chat

The chat holds the owner's whole working life (design-foreman, M5). So:

- `floor.chat` is **operator only**: the owner's TUI, the CLI on the owner's socket, and the
  web page signed in with the owner's own token. It is refused for panes, plugins, views and
  unplaced peers, with the same refusal `floor.talk` gives (`internal/server/role.go`).
- `pane.messages` is operator only too, except that a pane may read its own messages.
- A named web token (`coppice web token`, `state.me` in `app.js`) gets neither until the owner
  rules otherwise. Its page shows `The chat is for the owner's own sign-in.`
- `<data dir>/chat/` and `<data dir>/journal/` join the gate's secret-read door
  (`src/opendaisugi/pane_rule.py:24-40` and its Go and Rust mirrors), as `voice/token` and the
  CA and TLS keys did. Without that, any agent could `cat` the chat.

### Privacy

The chat file and the transcripts never leave the box. The phone reads them over the same
token-guarded websocket it uses for everything else, through the tunnel in the next
section. Nothing is cached on the phone beyond the open page: the service worker policy
(`_tests/sw-policy.test.mjs`) must keep `/api` and the websocket out of its cache, and a new
test checks that no chat text is written to `localStorage`.

---

## The one screen on the desktop

The chat is a window too. It takes a window slot like an agent does, and it fills first when
the foreman has a message the owner has not seen. Typing in it goes to the chat line, not to
the foreman's raw terminal; the foreman's screen is one click away (`Screen` in its header).

```
+------------------------------------------------------------------------------------------------+
| coppice  enforcing 6 · gateway ok · 5 working · 2 need you · 1.2M tok, 310k saved   New ▾   ⚙ |
+-------------------+----------------------------------------+-----------------------------------+
| ▾ glean     1 · 1 | 1 foreman (chat)            typing here | 2 glean-import          needs you|
|  2 ! glean-import | claude · enforcing · direct · opus     | claude · enforcing · gateway ·    |
|  4 ● glean-docs   |----------------------------------------| opus-5-5 · ⧗ Bash git push · 88k  |
| ▾ trellis   0 · 2 | you 14:02                              |-----------------------------------|
|  3 ● claude-trel  |  start the glean import fix and check  | ╭ git push origin main          ╮ |
|  5 ● sprig-trel   |  trellis's failing test                | │ Deny is the default.          │ |
| ▾ foreman         | foreman 14:02                          | │ [Deny]  [Allow once] [For task]│ |
|  1 ● foreman      |  Started glean-import and              | ╰───────────────────────────────╯ |
|                   |  claude-trellis.                       | $ git status                      |
| ▸ Recent 3        | work 14:09                             | On branch import-fix              |
|-------------------|  claude-trellis: test fixed.           |                                   |
| Tell the floor    |                                        |                                   |
| [             ]🎤 | > _                                    |                                   |
+-------------------+----------------------------------------+-----------------------------------+
| keys: ctrl-space leave · 1-9 window · n new · N project · ctrl-w stop · j journal · ctrl-\ talk |
+------------------------------------------------------------------------------------------------+
```

The rail numbers match the windows. The selected window has the thick border and "typing
here". The footer is the ramp: every key it names works where the owner is. In the TUI the
same picture holds, with the chat window drawn from the same `floor.chat` verb and its own
input line, as the headless window does today (README "First run", the headless paragraph).

---

## Reaching the floor from outside the house

The rules: local-first, no accounts we run, no third party sees content. The floor must be
reachable from a phone on mobile data, with the box at home behind NAT.

| | Tailscale (in use) | A blind relay we run | Claude Code Remote Control |
|---|---|---|---|
| What it reaches | the whole floor: every agent, every harness, the chat, voice | the whole floor, once built | one Claude session per process, or one `claude remote-control` server; not sprig, codex, pi or the floor |
| Who sees content | nobody but the two ends: WireGuard, end to end. DERP relays forward sealed packets | nobody, if the page code is not served by the relay (see below) | Anthropic stores the session transcript while it is connected (docs: "the session transcript ... is stored on Anthropic servers") |
| Who sees metadata | Tailscale's coordination server: device names, keys, addresses, when they connect | the relay host: when and how much, per channel | Anthropic |
| Accounts | a Tailscale account (theirs, not ours); Headscale removes it, but we would run it | none for the owner; but someone must host the relay on a public address | a claude.ai subscription; API keys refused |
| Works with our stack | yes; `coppice web cert tailscale` and `--tls tailscale` exist (`web.go:202`, `tls.go:16`) | needs a new transport in coppice and a phone client | **no** when `ANTHROPIC_BASE_URL` points at the gateway or Switchyard: Remote Control refuses a non-Anthropic base URL (docs, "Remote Control is only available when using Claude via api.anthropic.com") |
| Inbound ports at home | none | none | none |
| Build cost | small: `web serve --tls tailscale` made complete (1a) | large: hearthSync's relay is built (`OpenHearth/hearthSync/docs/adr/0013-relay.md`); porch's end-to-end sealing is "designed, not implemented" (`OpenHearth/porch/docs/adr/0002-dumb-e2e-relay-not-baas.md`) | none |
| Leaks to watch | `tailscale cert` puts `<host>.<tailnet>.ts.net` in public Certificate Transparency logs | the relay's host and its domain tie the owner to the project | the owner's prompts and code in an Anthropic store |

The blind relay has one more problem. The phone needs the page's code. If the relay serves
it, the relay can change it and read everything, so it is not blind. It stays blind only if
the page comes from somewhere the owner trusts: an installed app, or a service worker pinned
on a first visit over the LAN. That is a real design, and it is the same one hearthSync
needs, but it is not small, and it makes the owner a seen party who runs a public server.

**Recommendation: Tailscale is the default.** It is already in use and already wired. It sees
no content. It needs no server we run and no inbound port. It carries everything the floor
does, voice included, because the phone gets a real HTTPS origin. The account is the owner's
own, with Tailscale, not one we run. `coppice web serve --tls tailscale` (1a) makes it one
command.

- Headscale is the documented answer for an owner who wants no third party at all, at the cost
  of running a coordination server. We document it and do not build for it.
- Claude Code Remote Control stays a fine tool for one Claude session. It is not a way to
  reach the floor. The docs say so in one line, so nobody turns off the gateway to get it.
- The blind relay stays a later option, tied to hearthSync. It is not on this plan.
- The CT-log leak is named in the serve output the first time it gets a certificate:
  `This name is now in public certificate logs: <name>.` A MagicDNS name with no personal
  words keeps that harmless.

---

## What stays

- The protocol words: pane stays in code and on the wire. On screen it is agent and window.
- The spec S1 to S8 behaviours. This design adds to them.
- Fail closed: a fact the server does not know is absent, never guessed.
- One web page for both binaries.
- The gate holds every ask. The foreman holds none (README, the foreman paragraph). The phone
  answers asks only through the operator connection.
- Tests never run a real harness, never own a TTY, and use loopback only.

## Owner answers, 2026-10-08

1. **Everything typed or said in the chat goes to the foreman.** Plumbing needs a leading
   slash. The terminal prompt line keeps its words.
2. **`--tls tailscale` binds to the tailnet and loopback only.** No LAN listen by default.
3. **Lowering one agent from enforcing to watching** is one tap plus a confirm. Turning the
   gate off stays a typed command.
4. **The reading view opens first on the phone** for agents with a transcript. The live screen
   is one tap away.
5. **Imported chats (`past`)** are hidden by default behind `Show imported chats`.
6. **The floor always starts** (owner, 2026-10-08). Broken saved state is moved aside to
   `<name>.broken-<UTC time>`, never deleted, and the floor starts fresh with one line that
   names where it went. A foreman that cannot open (for example an I/O error) shows its error
   in its own window, and the rest of the floor starts.
