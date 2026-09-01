# The Omarchy roadmap

Written 2026-09-24. The goal: openDaisugi is ready to use every day and ready to
publish for Omarchy. From one install to a floor of agents across about ten
projects takes one or two commands, and the owner can talk to it.

The goal has three parts. They run in this order: part 1, the audit half of
part 3, then part 2. The Go gate core in part 2 starts early, because it is the
largest latency win and the piece a single binary needs.

## Names

- **openDaisugi** is the whole project.
- **daisugi** is the part that checks agents: the gate, the verifier, the
  journal and the garden. Its modes on screen are enforcing, watching and off.
  The config value for watching is `shadow`.
- **the gate** names only the part that allows or denies a call.
- **coppice** is the floor. **sprig** is our loop. **Switchyard** is the router.
- On screen, an **agent** is one running loop, and a **window** shows one agent.
  The word pane stays in code and in the protocol.

## Rules that bind every plan here

- Never touch the owner's live coppice server, its socket or its data dir.
  Every trial runs with its own `--socket`, `--data-dir`, `XDG_CONFIG_HOME` and
  `XDG_DATA_HOME` on real disk.
- Never touch `~/.opendaisugi/gate`. Its default envelope is the fallback for
  the owner's real Claude sessions. Trials use `--root` on a scratch dir.
- Never run `daisugi install` in a trial. It edits the owner's `~/.claude`.
  A trial Claude pane uses `claude --settings "$(daisugi gate settings --root <scratch>)"`.
- Trial projects are scratch dirs, never the owner's real repos.
- Automated tests never run real claude, sprig, codex, pi or opencode, never own
  a TTY, and use no network but loopback. Live runs are manual checks only.
- Browser checks run from a uv script outside the project. Playwright is never a
  project dependency.
- Scratch goes on real disk under `~/opendaisugi-scratch/`, never `/tmp`.
- Commit by explicit path. No attribution lines. Never commit CLAUDE.md,
  docs/superpowers/ or uv.lock. Push only through the fold script, on the
  owner's word.
- Mostly Claude for live runs. sprig runs with the Claude backend too. OpenCode
  Go runs a few times: $3 per model per 5 hours, and Kimi and Qwen refuse some
  topics. minimax-m3 answers them.

## Part 1: the floor people love

Each item ends with a live check by hand in the TUI, the web floor at desktop
width, and the web floor at phone width.

- **1a. One or two commands.** `coppice` goes on PATH at install. `coppice`
  alone starts the server if needed and opens the floor. `coppice web` alone
  serves on loopback and opens the browser signed in. The phone setup stays
  under `coppice web serve`.
- **1b. What a thing is.** Agents, windows and tasks. A dead agent leaves the
  floor for Recent, with Resume where its loop saved a session, and Resume all
  after a restart. Recent entries expire after a week. Kill on every row.
  `coppice agent forget`, `pane list --ended`, `pane resume`.
- **1c. One click, a new agent.** Default loop, sensible directory, automatic
  name, rename in place. A project picker holds the owner's projects so ten
  projects are one keystroke each. A missing directory is named as such.
- **1d. One screen.** The overview, the windows and the tree share one screen.
  Windows fill at start with the agents that need you, then the busiest.
  Keyboard focus starts on the agent that needs you. Typing goes to the
  selected agent, and a border says which one. Click or drag an agent into a
  window, or press its number. Kanban, shift log and the journal are overlays.
  On a phone, an agent slides in over the overview and controls rise from the
  bottom.
- **1e. Voice with no setup.** The server starts speech to text when it is
  installed. In the TUI, hold a key to talk, with a press-to-start fallback.
- **1f. daisugi on every agent.** A gate mark with the last verdict and why.
  Tokens spent and saved. A journal overlay.
- **1g. The stack bar.** One bar per agent: loop, daisugi and its mode, router,
  model. Colour is health only. Words are the choice. An outline marks the open
  segment. A moving light shows where the turn is. Swap the model and router
  from the next turn, the daisugi mode now, and the loop by moving the
  conversation, with the replay cost shown first. The floor has its own header
  bar.
- **1h. Every error has a fix button.** A fix the server can do is a button.
  A fix that needs a shell opens a shell window with the command typed and not
  run. A test fails when a message names a command and has no action.
- **1i. Readable windows.** The web windows are sharp and sized right.
- **1j. The foreman.** Talking to the floor reaches a foreman agent that can
  start, direct and stop agents across projects.

## Part 3: the publication sweep

The audit runs early and changes nothing. Its findings become fixes in part 1
and rules for part 2.

- **3a. Machine details.** No owner paths, user names, email, host names, uid
  or `/run/user/1000` assumptions, `~/.local/bin/claude`, GPU or tailnet names.
  The scope is every commit the next fold publishes, and fixtures first.
- **3b. Dependencies.** List every dependency in Python, Go, Rust, npm and every
  model file. Remove what we can. Check each licence against MIT.
- **3c. OS assumptions.** Every path comes from XDG or a flag. No fixed shell,
  no fixed distro.
- **3d. Speed from the start.** Measure cold start of every command and every
  gate check. Set a budget and a test for each.

## Part 2: Go and Rust, the whole stack

Python stays the oracle. Every port proves itself against the same
conformance suite at process boundaries.

- **2a. The conformance suite.** Golden cases per component, as stdin to
  stdout and exit code, or request to response: the gate hook contract, the
  verifier, shell decomposition, the journal format, the gateway wire, the
  coppice protocol and the sprig session tree. Cases are synthetic, because the
  frozen verifier corpus holds real local paths and is never published.
  `clients/compare.py` and `docs/spec/conformance.md` grow into this.
- **2b. The Go gate core, early.** One static binary that answers the hook
  contract. It removes Python start-up from every tool call.
- **2c. Go, the rest of daisugi.** Journal, garden, gateway, router client,
  install and the CLI. With coppice and sprig already in Go, this is the whole
  stack in Go.
- **2d. Rust, the whole stack.** daisugi, coppice and sprig.
- **Full parity (owner ruling, 2026-09-26).** Python, Go and Rust ship as
  competing binaries that check each other for mistakes, correctness and
  speed. Anything a Go or Rust user would need Python for is ported to both,
  including the parts that use pathways (stage K in the part 2 plan).
- **2e. Proofs.** Lean stays the proof of the verifier core. There is no third
  full stack. Bend 2 is six days old, so it is not a target.

Rulings to make with measurements before 2c and 2d:

- **Z3.** Measure the share of cases that reach the Z3 stages. Then choose FFI
  behind a build flag, or a native decision procedure for the restricted
  predicate algebra.
- **libghostty-vt, tree-sitter-bash and the potion embedder** are not Go or
  Rust. For each, choose FFI, a native port, or a documented exception.

## Scale

Python source is about 57k lines, coppice about 34k lines of Go, sprig about
2.6k. The Go port adds about 45k lines. The Rust port adds about 95k. This is
many sessions of work. The ledger at `.superpowers/sdd/omarchy/progress.md`
holds where it stands.
