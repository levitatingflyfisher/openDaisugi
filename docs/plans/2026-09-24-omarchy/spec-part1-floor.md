# Part 1 spec: the floor people love

The binding authority for part 1 of the Omarchy roadmap. Plans and task briefs
argue from this file. Where a brief and this file disagree, this file wins.

The code map of the floor as it stood on 2026-09-24 is in the plan directory's
companion notes and in `harness/coppice/PROTOCOL.md`.

## Words

- **Agent**: one running loop. In code and in the protocol it is a pane.
- **Window**: a place on screen that shows one agent live and takes its keys.
  In code it is a tile or a slot.
- **Ended agent**: a pane record whose process is gone. It is not on the floor.
  It is in **Recent**.
- **Project**: a directory the owner works in. The floor knows a list of them.

## S1. Ended agents leave the floor

- `pane.list` returns live records only. A request with `ended: true` returns
  only ended records, newest first, each with `ended_at` and `exit_code`.
  `agent.list` follows the same rule.
- A record the operator closes with `pane.close` is removed from the layout at
  once. It does not go to Recent.
- A record whose process ends on its own, or that a restart leaves closed, goes
  to Recent. It keeps its label, cwd, harness, argv, task and harness session id.
- `pane.forget` takes `pane`, or `ended: true` for every ended record. It
  removes the records. A live pane is refused with `bad_request`.
- An ended record older than seven days is removed when the server starts and
  once an hour after.
- `pane.resume` takes an ended `pane`. It starts a new live pane with the same
  label, cwd, harness and task, and removes the ended record.
  - A headless pane with a harness session id resumes that session, as restart
    does today.
  - A pty pane resumes when its harness table in `coppice.toml` has
    `resume_args`, for example `["--resume", "{session}"]`, and the record has a
    harness session id. `{session}` is replaced by that id.
  - Otherwise the new pane starts fresh, and the reply says `resumed: false`.
- After a restart, `server.status` says how many ended records can resume.
- CLI: `coppice pane forget PANE`, `coppice pane forget --ended`,
  `coppice pane list --ended`, `coppice pane resume PANE`.
- Web and TUI: ended agents never fill a window and never draw in the rail. A
  "Recent" fold at the foot of the rail lists them with Resume and Forget, and
  "Resume all" and "Clear all" when there are any. The fold is closed by
  default and shows its count.
- When a pane ends on its own, the floor shows one line: `<label> ended (exit
  N)`. A non-zero exit draws amber and stays until the owner clicks it or opens
  Recent.

## S2. What the floor knows about each agent

Each `pane.list` row gains an optional `stack` object and optional `gate` and
`tokens` objects. A field the server does not know is absent, never guessed.

- `stack.loop`: the harness name.
- `stack.model`: the model the agent last used.
  - Claude: read from the `message.model` of the newest assistant entry in the
    transcript file whose path the gate hook reports.
  - sprig: read from the `model` of the newest assistant entry in its session
    tree.
  - Others: absent until their adapter reports one.
- `stack.router`: `switchyard`, `gateway` or `direct`, from the pane's
  `ANTHROPIC_BASE_URL` or `OPENAI_BASE_URL` compared with the configured gateway
  address. Absent when neither is set.
- `stack.daisugi`: `{mode: "enforcing"|"watching"|"off", armed: bool}`. The
  mode comes from the pane's gate hook reports. A harness with no gate hook is
  `off`. The disarm marker in the gate root sets `armed: false`.
- `gate`: the last verdict for this pane: `{decision: "allow"|"deny"|"ask",
  tool, clause, at}`. The gate hook sends it with every report. The server
  keeps the last one per pane. The `ask` object keeps its present fields.
- `tokens`: `{fresh, cache_read, cache_write, out}` summed over the agent's
  turns, from the same transcript or session tree.
- The gate hook in daisugi adds `transcript_path`, `verdict` and `mode` to the
  state reports it already sends to coppice. The server reads a transcript only
  from a path the pane's own hook reported, and reads only what it needs: the
  tail since the last read.
- A floor header row shows the floor-wide facts: the daisugi mode and armed
  state, whether the gateway answers, the number of agents working and needing
  you, and tokens today.

## S3. One click, a new agent

- `pane.create` with no `cwd` and no `cmd_argv` starts the default harness. The
  cwd is, in order: the `near` pane's cwd when the request names one, the most
  recent project, or the server's own start directory.
- The label is the harness name and the project's base name, with a number when
  the name is taken: `claude-trellis`, `claude-trellis-2`.
- `coppice.toml` gains `projects = ["/abs/path", ...]`. The server also keeps a
  list of the ten directories agents most recently started in, in its data dir.
  `project.list` returns both, pinned first, each with its base name.
- CLI: `coppice project add DIR`, `coppice project list`, `coppice project rm
  DIR`. `coppice new [PROJECT]` starts the default harness in a project, by
  name or path, or near the current directory.
- Web: a New button makes an agent in one click, near the selected agent. A
  project menu beside it lists projects and starts one in the chosen project.
  The full form stays behind a small "more" control.
- TUI: `n` makes a new agent near the selected row. `N` opens the project
  picker. Number keys in the picker choose.
- Rename in place: the label on a window or row is editable. `pane.rename`
  takes `pane` and `label`.

## S4. One screen

The floor is one screen at every width. Nothing opens a new page.

- **Layout, 900 px and wider, and TUI 120 columns and wider**: the header row;
  the rail on the left; windows fill the rest. Windows tile to fill the area,
  as many as fit at a readable size, not a fixed three. A readable size is a
  cell at least 7 px wide on the web and 80 columns in the TUI.
- **Start**: windows fill with agents that need you, then working agents, then
  the rest, in rail order. The keyboard starts on the first agent that needs
  you, or else on the first window, or else on the rail. It never starts on a
  prompt line.
- **Keys go to the selected window.** The selected window has a thick accent
  border and the words "typing here". Every key goes to its agent except the
  leave key, ctrl-space by default. After leave, the rail has the keys and the
  selected row has the border. In the TUI the hardware cursor sits at the
  selected agent's own cursor.
- **Put an agent in a window**: click its row or dot. Drag a row onto a window.
  Select a row and press a window's number, 1 to 9. A row shows its window's
  number. A window's header shows its agent's name, state and stack bar.
- **The rail is a tree**: tasks with their agents under them, folding. A task
  folds to one line with its state counts. Projects are a grouping when there
  are no tasks.
- **Kill**: a × on every row and window header, and `ctrl-w` in the rail. A
  live agent asks once: "Stop <label>? Enter stops it, Esc keeps it." A stopped
  agent's record goes away, as S1 says.
- **An agent that ends while you are in it** returns you to the floor with the
  S1 line. Nobody stays on a dead screen.
- **Overlays**: the views (tree graph, kanban, colony, shift log, grid, inbox)
  and the journal open as a panel over part of the windows area, with a close
  control and Esc. The rail and at least one window stay visible. The colony
  can be pinned as a strip above the windows.
- **Phone, under 600 px**: home is the overview: the colony strip and the agent
  list. Tapping an agent slides its window in from the right over most of the
  screen, with the overview still visible at the left edge. A bar at the bottom
  holds the prompt, the mic and a key drawer. Swiping right or tapping the edge
  goes back. Nothing navigates away from the page.

## S5. Voice with no setup

- The coppice server starts the voice server itself when `daisugi` is on PATH
  and the voice extra is installed, and stops it with itself. `coppice.toml`
  `[voice] enabled = false` turns this off. `url` points at another one.
- The web mic works with no flags. When voice cannot run, the mic button says
  why in one line and offers the fix, as S7 says.
- TUI: holding the talk key records while held, when the terminal reports key
  release. Otherwise a press starts and a second press stops. The recording
  comes from the first of `pw-record`, `parecord` or `arecord` on PATH. The text
  lands in the selected agent's input, not sent, so the owner can read it and
  press Enter.

## S6. Readable windows

- Web canvases draw at the device pixel ratio, so text is sharp.
- The cell size comes from the font's measured advance, not a fixed ratio.
- A window never draws text smaller than the readable size in S4. When the
  agent's terminal is wider than the window allows, the window scrolls
  sideways, or the agent is resized to the window when it is the only viewer.

## S7. Every message has a fix

- Every error or warning the floor shows carries an action, or says in words
  that the fix is on another machine or needs the owner.
- The server offers these actions: start the coppice server, start voice,
  resume an agent, forget ended agents. Setting the daisugi mode means
  rewriting each harness's hook settings, so it stays a command the owner
  runs, named in words. A page that is already signed in needs no sign-in
  link.
- A fix that needs a shell opens a shell agent in the right directory with the
  command typed and not run.
- A web test scans every message string in the client and fails when one names
  a `coppice ` or `daisugi ` command and has no action.
- When the page cannot reach the box, it keeps the last picture dimmed, says
  so, and retries.

## S8. The foreman

- Talking to the floor, by the tell bar or by voice, reaches the foreman agent.
  With no foreman, the first talk starts one with the default harness in its
  own directory, `$XDG_STATE_HOME/coppice/foreman` or
  `~/.local/state/coppice/foreman`, labelled `foreman`, and gives it the
  foreman page. The directory is outside every directory daisugi guards,
  because daisugi refuses every call made from inside a guarded one. The
  server tracks the foreman by the pane id it started, not by its label.
- The foreman can list projects, start an agent in a project, prompt it, read
  it, and stop it, through the existing verbs.

## Checks by hand

Each section ends when the owner's hand check in `RESUME.md` passes on the TUI
at 120 and 200 columns, the web at 1440 px, and the web at 390 px, using the
isolated rig in `~/opendaisugi-scratch/rig`. Claude agents run with a scratch
gate root. Tests never run a real harness.
