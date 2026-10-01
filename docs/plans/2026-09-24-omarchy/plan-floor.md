# Plan: the floor pass after part 1

Design: `docs/plans/2026-09-24-omarchy/design-floor.md`. Read it first. Where a brief and the
design disagree, the design wins until the owner rules.

Each chunk is sized for one agent, one to three days; each says its size. The order puts the phone with chat
first, then the way in from outside, then readability on the phone, then the desk.

## Rules for every chunk

- The rules in `ROADMAP.md` ("Rules that bind every plan here") bind every chunk. Never touch
  the owner's live coppice server, socket, data dir, `~/.opendaisugi`, `~/.claude`, `~/.codex`,
  `~/.local/bin` or tmux. Every trial names its own `--socket`, `--data-dir`, `HOME` and XDG
  dirs on real disk under `~/opendaisugi-scratch/`.
- Test first. Tests never run a real harness, never own a TTY, and use loopback only.
- One heavy job at a time. Builds and test runs go under
  `systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G`, with `-j2` or `-p 1`.
- Each chunk names its tag from the design: **web** (one copy; the Rust binary embeds the
  page), **verb** (Go and Rust, with compare cases), **tui** (Go and Rust).
- A **verb** chunk is done only when `clients/coppice_compare.py` reports 0 disagreements for
  its new cases and for every suite it touches, on both binaries.
- A **web** chunk is done only when `go test ./internal/web -run TestJSUnitTestsPass` passes and
  `e2e/run.sh web` passes on the Go and on the Rust binary.
- A **tui** chunk is done only when `e2e/run.sh tui` passes on both binaries.
- `PROTOCOL.md` and `README.md` change in the same commit as the behaviour they describe.
- STE in code comments, docs and on-screen text. No em dashes, and none of the banned words. On screen the
  words are agent and window.
- Commit by explicit path, in small commits. No attribution lines. No push.
- The floor always starts. Every chunk that loads saved state tests two cases: broken state
  is moved aside to `<name>.broken-<UTC time>` and the floor starts fresh with one line
  naming where it went; a foreman that fails to open (an I/O error) shows its error in its
  window while the rest of the floor starts. Owner answers are in `design-floor.md`.

---

### F1. The chat read path (verb)

Size: 3 days.

Design: "The phone page", "Where the chat comes from".

Files: Go `internal/server/facts.go` (share the transcript reader), new
`internal/server/messages.go`, `internal/server/foreman.go` (write the owner's lines to the
chat file), `internal/proto` (the event); Rust `src/server/facts.rs`, new
`src/server/messages.rs`, `src/server/foreman.rs`; fixtures
`harness/coppice/testdata/transcripts/` (invented content); cases
`harness/coppice-rs/cases/chat/*.jsonl`; `PROTOCOL.md`.

Acceptance:
- `pane.messages` returns `owner` and `agent` messages from a fixture Claude transcript and a
  fixture sprig session tree, oldest first, with `since` and `limit`.
- It refuses a pane whose hook reported no transcript path, with a message that says so.
- A path no hook reported is never read (a test points a report at another pane's file).
- Tool use shows as a tool name and a one-line input summary. A tool result never appears.
- Control characters are dropped and each message is capped.
- `floor.chat` returns the owner's `floor.talk` lines from `<data dir>/chat/` merged by time
  with the foreman's messages, and a `note` line when a new foreman started.
- The chat file is mode 0600.
- `floor.chat` is refused for a pane, a plugin, a view, an unplaced peer and a named web token.
  `pane.messages` is refused the same way, except a pane reading its own messages.
- `<data dir>/chat/` and `<data dir>/journal/` are in the gate's secret-read door in Python,
  Go and Rust (`src/opendaisugi/pane_rule.py` and its mirrors), with gate compare cases: a
  `cat` of a chat file from a gated pane is denied.
- A `messages` event (the bare event name, ruled 2026-10-09) reaches a subscribed client when the transcript grows.
- Compare: 0 disagreements, Go and Rust, for `cases/chat` and the `facts` and `ended` suites.

### F2. The phone home with chat (web)

Size: 2 to 3 days. Owner question 2 (slash plumbing): builds the slash rule by default,
behind one switch in `newpane.js`.

Needs F1. Design: "The phone page", 1d (chat bar).

Files: `internal/web/static/index.html`, `app.js`, `dock.js`, `newpane.js` (the chat bar,
slash plumbing), new `chat.js`, `app.css`, `_tests/chat.test.mjs`, `_tests/phone.test.mjs`;
`e2e/web.mjs`, new `e2e/fake-foreman.sh` (a bash harness that writes an invented Claude
transcript and reports its path through `rpc.mjs`).

Acceptance:
- Under 600 px, home shows from the top: the facts line, the needs-you cards with their
  buttons, the agents fold, the chat. The chat bar is docked at the bottom and stays above the
  on-screen keyboard.
- A sentence shows in the chat at once as `sent`, then `read`.
- A plain sentence is always talk. `/claude` and `/close NAME` are plumbing. A sentence led
  by `claude` is talk (a test pins the old trap: `classifyTell('claude can you check it')`).
- A chip in a `work` or foreman message opens that agent's sheet. Swipe right goes back. The
  page never reloads.
- No chat text is written to `localStorage` (a test spies on it). The service worker policy
  test still passes.
- New `e2e/web.mjs` checks at 390x844: the chat shows the fake foreman's reply; send a
  sentence and see it as `sent`; tap a chip, the sheet slides in, type into it, swipe back;
  no sideways page scroll; no script errors.
- At 1440 the desktop floor is unchanged except the tell bar's new place in the rail.

### F3. `web serve --tls tailscale` does the whole job: the way in from outside (verb + web)

Size: 2 days. Owner question 1 (tailnet-only bind): builds tailnet plus loopback by default;
`--listen` overrides it.

Design: 1a, "Reaching the floor from outside the house", 1e (secure context), 1h (phone-side
words).

Files: Go `internal/cli/web.go`, `internal/web/tls.go`, `internal/web/server.go`; Rust
`src/cli/web.rs`, `src/web/serve.rs`, `src/web/config.rs`; the fake
`harness/coppice-rs/cases/web/fixtures/bin/tailscale` (adds `status --json` and a dated
cert); cases `harness/coppice-rs/cases/web/phone.jsonl`; `internal/web/static/messages.js`,
`record.js`; README "Phone".

Acceptance:
- `coppice web serve --tls tailscale` with no `--cert` or `--key`, and the fake tailscale: gets a cert, listens on the tailnet address and
  loopback only (a case checks the listen address), persists, prints the QR and one line.
- With no tailscale on PATH: one line that says so and names `coppice web serve`, exit 1.
- A cert with under 30 days left is renewed at start. Under 7 days, `floor.facts` carries
  `phone_cert_days` and the header shows a warn mark with Renew.
- The first certificate prints the Certificate Transparency line.
- With `isSecureContext` false, the mic says `The mic needs https here.` and names the fix on
  the box.
- The cannot-reach line names the two fixes on the phone (Tailscale on, the box asleep).
- Compare: 0 disagreements for `cases/web`.

### F4. Readable agents on the phone: the reading view and fit-when-alone (verb + web)

Size: 3 days. Owner question 4 (reading view first): builds it first by default; one
constant in `dock.js` flips it.

Needs F1. Design: 1i.

Files: Go `internal/server/attach.go`, `panes.go`; Rust `src/server/attach.rs`, `panes.rs`;
cases `harness/coppice-rs/cases/attach/fit.jsonl`; `internal/web/static/pane.js`, `dock.js`,
`chat.js` (shared message list), `app.css`, `_tests/pane.test.mjs`; `e2e/web.mjs`.

Acceptance:
- An agent with a transcript opens on the reading view on a phone, with
  `From the transcript. [Screen]`. Screen shows the live canvas.
- An ask or trust bar draws over the reading view. Allow and Deny work from it.
- The compose bar types into the agent from the reading view.
- `pane.attach` with `fit` resizes the agent only when no other client is attached, and puts
  the old size back on detach. A second attach while fitted restores the size first.
- The reply's `fitted` drives the sheet line (`Sized to this phone.` or the desktop line).
- `e2e/web.mjs` at 390x844: the shell agent fitted to the phone draws with no sideways scroll;
  the trust agent's bar shows over its view.
- Compare: 0 disagreements for the new cases and the `web` suite.

### F5a. `daisugi voice status --json` in three languages (daisugi)

Size: 2 days. Design: 1e. No such command exists today in `src/opendaisugi/voice/`,
`clients/go/internal/voice/` or `clients/rust/src/voice/`.

Acceptance:
- `daisugi voice status --json` answers `{engine, model, model_bytes, fetched, kind}` and,
  during a fetch, `fetch {done, total}`, the same shape in Python, Go and Rust.
- Voice compare: 0 disagreements, Go and Rust, with new cases for each field.

### F5b. Voice with no setup, for every daisugi (verb + web + tui)

Size: 2 days. Needs F5a. Design: 1e.

Files: coppice Go `internal/server/voice.go`, Rust `src/server/voice.rs`; cases
`harness/coppice-rs/cases/cli/voice-*.jsonl`, `cases/web/voice*.jsonl`;
`internal/web/static/record.js`, `messages.js`; `internal/tui/voice.go`, `src/tui/voice.rs`.

Acceptance:
- The supervisor's fix names the install line for the daisugi kind it found. No `pip` line is
  offered to a Go or Rust daisugi (a case for each kind, using the `voicebin` fixture).
- During a model fetch, `voice.status` carries `fetch {done, total}`; the web mic and the TUI
  line show it; then `Voice is ready.`
- The voice banner shows only after the first Mic press.
- The chat bar's mic fills the chat box and does not send.
- Compare: 0 disagreements for the voice cases.

### F6. No screen is a page: New form and Settings as overlays (web)

Size: 1 to 2 days.

Design: 1d.

Files: `internal/web/static/index.html`, `app.js` (`show()`), `newpane.js`, `settings.js`,
`overlay.js`, `app.css`, `_tests/app.test.mjs`, `_tests/overlay.test.mjs`; `e2e/web.mjs`.

Acceptance:
- `#/new` and `#/settings` open over the floor. The rail and at least one window stay visible
  at 1440. On a phone they rise from the bottom over the home. Esc and × close them.
- `show()` never hides `#screen-roster` (a test).
- The pinned colony strip never removes a window at 1440x900 (a test on `tileCount` with the
  strip).
- `e2e/web.mjs`: open More from the New menu and create an agent; the floor stays drawn.

### F7. Agents come back, and projects tell the truth (verb + web + tui)

Size: 2 to 3 days.

Design: 1b, 1c.

Files: Go `internal/cli/verbs.go`, `internal/server/projects.go`, `panes.go`; Rust
`src/cli/verbs.rs`, `src/server/projects.rs`, `panes.rs`; cases `cases/cli/verbs.jsonl`,
`cases/ended/projects.jsonl`; `internal/web/static/rail.js`, `newpane.js`, `roster.js`;
`internal/tui/rail.go`, `recent.go`, `src/tui/rail.rs`, `recent.rs`.

Acceptance:
- After a restart with resumable records, the web and the TUI show the one line with Resume
  all and Show them. It goes after one use.
- `coppice agent forget|resume` and `agent list --ended` act as their `pane` forms.
- `project.list` rows carry `missing`. The menu and the picker draw them dim with
  "(missing)" and Remove. `project.remove` is operator only.
- The New reply carries `why`, and the status line says where and why.
- The TUI picker takes `0` for the tenth project. Picker rows show state counts.
- Compare: 0 disagreements for `cli` and `ended`.

### F8. Journal history and tokens saved (verb + web + tui)

Size: 2 to 3 days.

Design: 1f.

Files: Go `internal/server/agents.go`, new `internal/server/journal.go`, `facts_stack.go`;
Rust `src/server/agents.rs`, new `src/server/journal.rs`, `facts.rs`; cases
`cases/facts/journal.jsonl`; `internal/web/static/overlay.js`, `stackbar.js`;
`internal/tui/facts.go`, `src/tui/facts.rs`; the gateway in three daisugi binaries only if
the first step finds no per-pane saving.

Acceptance:
- First step, written in the report: what the gateway exports today, and whether it can name
  the pane.
- `floor.journal` returns the last verdicts newest first, across a server restart, capped at
  500. The file is 0600.
- The overlay reads it. `JOURNAL_SOURCE` says what it lists.
- The TUI opens the journal over the windows with `j` from the rail.
- The header shows saved tokens only from a gateway figure. No figure, no number (a test).
- Compare: 0 disagreements for `facts`.

### F9. The chat window on the desk (web + tui)

Size: 2 to 3 days.

Needs F1. Design: "The one screen on the desktop".

Files: `internal/web/static/floor.js`, `tiles.js`, `chat.js`, `_tests/floor.test.mjs`;
`internal/tui/render.go`, `run.go`, new `internal/tui/chat.go`; Rust `src/tui/render.rs`,
`run.rs`, new `src/tui/chat.rs`; `cases/tui/chat.jsonl`; `e2e/tui.sh`, `e2e/web.mjs`.

Acceptance:
- The foreman's window draws the chat, with its own input line; Enter sends `floor.talk`.
  `Screen` in its header shows the foreman's terminal.
- The chat window fills first when it holds an unseen foreman message.
- `e2e/tui.sh` at 120x36 and `e2e/web.mjs` at 1440x900 see the fake foreman's reply in the
  chat window.

### F10. The slim rail and nested subtasks (tui)

Size: 2 days.

Design: 1d.

Files: `internal/tui/render.go`, `rail.go`, `railtree.go`; Rust `src/tui/render.rs`,
`rail.rs`, `railtree.rs`; `cases/tui/rail.jsonl`, `cases/tui/windows.jsonl`; `e2e/tui.sh`.

Acceptance:
- From 88 to 119 columns the rail draws 6 columns per row and one window of 80 or more
  columns shows and takes keys.
- Below 88 columns the peek stays.
- Subtasks nest under their task.
- `e2e/tui.sh` gains a run at 100x36 that types into a window. The existing peek checks move
  to an 80x36 run.

### F11. The daisugi mode from the stack bar (verb, and daisugi in three languages)

Size: 2 to 3 days.

Design: 1g, mode row. Wait for the owner's answer to open question 3.

Files: the gate in Python, Go and Rust (read `<gate root>/agents/<pane>.mode` for the calling
pane), gate compare cases; coppice Go `internal/server/role.go`, new verb in
`internal/server/agents.go`; Rust `src/server/role.rs`, `agents.rs`; cases
`cases/facts/mode.jsonl`; `internal/web/static/stackbar.js`; `internal/tui/stackline.go`,
`src/tui/stackline.rs`.

Acceptance:
- The marker moves one agent between enforcing and watching, and nothing else. It cannot
  turn the gate off or change an envelope (gate cases).
- `agent.set_mode` is refused from a pane, a plugin and a view.
- Enforcing to watching asks once. The status line says the new mode.
- Gate compare and coppice compare: 0 disagreements.

### F12a. Model and router swaps (verb + web + tui)

Size: 2 to 3 days. Design: 1g, the model and router rows.

Files: Go `internal/server/ready.go` (queue `/model`), `ended.go` (resume with new env); Rust
`src/server/ready.rs`, `ended.rs`; cases `cases/ended/swap.jsonl`;
`internal/web/static/stackbar.js`; the TUI stack line in both ports.

Acceptance:
- Model on claude: `/model NAME` is queued and sent only at an idle prompt (fake harness).
  On headless sprig: the next `agent.prompt` carries the model. Other harnesses keep
  `SWAP_NOTE`.
- Router: the bar says it restarts and resumes, and asks first. After Resume the agent's env
  has the new base URL and `stack.router` shows it.
- Compare: 0 disagreements.

### F12b. Move the conversation to another loop (verb + web + tui)

Size: 2 to 3 days. Needs F12a. Design: 1g, the loop row.

Files: new Go `internal/server/handoff.go`, Rust `src/server/handoff.rs`; cases
`cases/ended/handoff.jsonl`; `internal/web/static/stackbar.js`; the TUI stack line in both
ports.

Acceptance:
- The bar shows the replay cost from the transcript's token count before anything moves.
- Move starts a new agent of the chosen harness in the same directory with a handoff brief
  built from the transcript tail. The old agent stays.
- The brief carries no tool output (the same rule as `pane.messages`).
- Compare: 0 disagreements.

### F13. Fixes scanned everywhere (web + verb + tui)

Size: 1 day.

Design: 1h.

Files: new `harness/coppice/testdata/fixwords.json`; `_tests/messages.test.mjs`; new Go test
over `internal/server` and `internal/tui` string literals; Rust mirror in `src/`.

Acceptance:
- One list of fix words read by all three scans.
- A planted string that names `tailscale` with no action fails each scan.
- Every existing string passes or gets its action in the same commit.

### F14. The chat reads the foreman's own log (verb)

Size: 1 day. Owner question 5 (imported chats): hidden by default behind a button.

After foreman M1 (log) and M3 (turn) exist. Design: "Source later".

Files: Go `internal/server/messages.go`, Rust `src/server/messages.rs`, cases `cases/chat/`.

Acceptance:
- With the M1 log present, `floor.chat` reads it, and its kinds map one to one.
- `past` messages are hidden on the phone behind `Show imported chats`.
- The page does not change (no `internal/web/static` diff).

### F15. The owner's hand check and the friction log

Size: 1 day for the agent; one week for the owner.

Last, and again after any chunk the owner wants to see.

- Rewrite `RESUME.md` "Part 1" into the floor checks for F1 to F14, by width: TUI 100 and 200
  columns, web 1440, web 390 on the owner's own phone, from the park over `--tls tailscale`.
- The owner runs them and keeps a friction log for one week of real use.
- The top three pains become the next chunks, before any new design.

---

## Order and dependencies

| Order | Chunk | Tag | Needs |
|---|---|---|---|
| 1 | F1 chat read path | verb | none |
| 2 | F2 phone home with chat | web | F1 |
| 3 | F3 `web serve --tls tailscale` | verb + web | none (can run beside F2) |
| 4 | F4 reading view, fit | verb + web | F1 |
| 5 | F5a voice status in daisugi | daisugi | none |
| 5 | F5b voice for every daisugi | verb + web + tui | F5a |
| 6 | F6 overlays, not pages | web | none |
| 7 | F7 Recent line, projects | verb + web + tui | none |
| 8 | F8 journal, saved tokens | verb + web + tui | none |
| 9 | F9 chat window on the desk | web + tui | F1 |
| 10 | F10 slim rail | tui | none |
| 11 | F11 mode from the bar | verb + daisugi | owner question 3 |
| 12 | F12a model, router | verb + web + tui | F11 for the bar shape |
| 12 | F12b move the loop | verb + web + tui | F12a |
| 13 | F13 fix scans | web + verb + tui | F3, F5b (their new strings) |
| 14 | F14 foreman log source | verb | foreman M1, M3 |
| 15 | F15 hand check, friction log | owner | F1 to F4 at least |

F1, F2 and F3 together put the chat on the owner's phone from outside the house. That is the
first hand check worth asking for.
