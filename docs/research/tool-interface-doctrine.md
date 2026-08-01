# The Tool-Maker's Doctrine

*A field manual for designing tool interfaces — CLI, TUI, GUI. Written from a 2026-08-24
deep-research run, a mood-board study of legendary and terrible interfaces, the clig.dev
command-line guidelines, and hard-won convention. Opinionated on purpose. Claims marked
**[proven]** survived adversarial checking; the rest are argued craft.*

---

## 0. The one law

**A tool should sit at one of two poles, joined by a ramp.**

- **Infant-simple.** One obvious affordance. Near-zero learning. A child, or a tired parent,
  succeeds on the first try without reading anything.
- **Expert-instrument.** Minimal energy for maximum skill. A high-throughput channel between a
  trained hand and the work. vim, a cockpit, the Bloomberg Terminal.
- **The ramp** is the only honest middle: a designed path that carries a novice *into* the
  expert instrument. Not a watered-down third product.

The place tools die is the **mediocre middle** — too fiddly for a beginner, too slow for an
expert. Most bad software lives there. Aim for a pole. If you must serve both audiences, build
one instrument and a ramp into it, the way Bloomberg trains market-makers "with no prior computer
experience" **[proven]** and Engelbart's NLS used `dw` = Delete Word as "a training aid of
sorts" **[proven]**.

Correction to the naive theory: the evidence for "avoid the middle" is thinner than the evidence
for "two poles plus a ramp." Build both poles well and build the ramp. That is the strong claim.

---

## 1. Pole one — infant-simple

**Use when:** the user is occasional, stressed, non-technical, or the stakes of a wrong move are
high. Family tools. Onboarding. Anything a person touches once a month.

**The moves:**
- One default that is right for almost everyone. No decision to make.
- Delete options to add users. Apple cut PARC's 3-button mouse to 1 "because learning to mouse is
  a feat in and of itself," made it cost under $15 and survive years **[proven]**. Fewer buttons,
  more people.
- A physical or spatial affordance beats a screen. Car physical buttons did a task **~4× faster**
  than a touchscreen at 110 km/h **[proven]**. An elevator button needs no manual.
- The tool works before it is configured. Zero-config is the whole product for this pole.
- Say what happened in plain words. No jargon, no codes.

**The trap:** hiding real capability so far that the tool feels like a toy to anyone who returns.
That is why the ramp exists.

---

## 2. Pole two — expert-instrument

**Use when:** the user is daily, trained, and repeats the work thousands of times. Their time is
the scarce resource. Developers. Traders. Pilots. Musicians.

**The moves:**
- **Throughput over discoverability.** Optimize keystrokes-per-task, not first-run comfort. Treat
  the interface as a measurable channel — Raskin's *Humane Interface* gives GOMS, Fitts, and Hick
  models for exactly this **[proven]**.
- **A command line for the expert.** Terse mnemonic verbs beat menus at scale. Bloomberg's
  `VOD LN Equity GO` and k9s's `:pods` are the same idea 40 years apart.
- **Modes are power and danger.** vim turns the keyboard into verbs; that is peak throughput and a
  cliff for novices. If you use modes, make the current mode impossible to miss.
- **Spatial constancy is the interface.** A mixing console's 400 identical faders are learned by
  where the hand knows they are. Never move an expert's controls.
- **Color is data, not decoration.** htop encodes load in color you read at a glance. Use color to
  mean something; never to prettify.
- **Density is a feature here.** Tufte's information density is the expert-pole aesthetic. Show the
  whole state; trust the trained eye.

**The trap — the glass-cockpit paradox [proven]:** glass cockpits *lowered* the overall accident
rate but *raised* the chance a crash was fatal. Added capability raises the ceiling and the floor
at once. **Ship the training with the instrument**, or the expert tool hurts under-trained users
faster than the simple one did.

---

## 3. The ramp — scaffolding done right

The ramp carries a novice into the expert instrument without becoming the mediocre middle. Four
mechanisms, in order of preference:

1. **Progressive disclosure [proven].** Show the few important controls first; reveal the rest on
   request. One interface that serves the novice (fewer mistakes) and the expert (less to scan).
   The canonical mechanism.
2. **On-screen affordances that teach.** lazygit prints its keybindings along the bottom, always.
   Midnight Commander's F-key bar has not changed in 30 years because it never lies. The expert
   stops reading them; the novice never has to leave the tool to learn.
3. **Mnemonics as a training aid.** NLS's `dw`, Bloomberg's colored keys. The command *is* the
   lesson. The name teaches the verb.
4. **Make the user awesome, not the product lovable.** Kathy Sierra: "people care about how
   awesome THEY are when they use your product" **[proven]**. Design for the user's growing
   competence, not the product's charm. The ramp's job is to graduate people off it.

**The ramp must not become the middle.** A separate "beginner mode" that no one graduates from is
the middle wearing a disguise. Prefer one instrument with disclosure over two products.

---

## 4. The failure modes (name them to avoid them)

- **The mediocre middle.** Serves no one fully. The default failure. A 100-button TV remote.
- **Capability without training (glass cockpit).** More power, worse outcomes for the untrained.
- **The unconfirmed catastrophic control.** The 2018 Hawaii false missile alert: a dropdown put
  "real" one line from "drill," no confirmation, and a whole state got a "ballistic missile
  inbound" alert. A powerful control with no guard rail is a loaded gun.
- **Feature count mistaken for capability.** More buttons is not more power. It is more to scan.
- **Silent success and silent hang.** The tool that says nothing leaves the user with no model of
  what happened or whether it is stuck.

---

## 5. The laws (the working heuristics)

1. **Pick a pole.** If you can't say which pole you're at, you're in the middle.
2. **One right default beats ten options.** Every option is a tax on the novice.
3. **The tool works before it's configured.** Zero-config is a feature, not a TODO.
4. **Never move an expert's controls.** Spatial memory is the fastest interface.
5. **Color and position must mean something.** Decoration that carries no data is noise.
6. **Put the affordance on the screen.** If the user must remember it or read a manual, the ramp
   is missing.
7. **Confirm the irreversible; type-to-confirm the catastrophic.** Match the guard rail to the
   blast radius.
8. **Say what changed.** After any state change, tell the user the new state in one line.
9. **Ship the training with the capability.** A denser tool without a ramp raises the fatal-error
   rate.
10. **Honesty over flattery.** Never dress a preference that does nothing as a working control.
    Label what's live, what needs a restart, what's not wired yet. (openDaisugi's own lesson.)
11. **Graduate the user.** The ramp's success is measured by people leaving it for the instrument.
12. **Delight is a real feature.** `sl` chuffing a train across the screen earns loyalty a spec
    sheet never will. Spend it sparingly.

---

## 6. Medium layer — the command line (clig.dev, distilled)

The CLI is the expert pole's native form. The rules that matter:

**Streams and machines.**
- Primary output → **stdout**; all logs, status, and errors → **stderr**. So `tool | jq` works.
- Human-readable is the default. Add **`--json`** for machines and **`--plain`** (tabular, one
  record per line) for `grep`/`awk`.
- Detect a TTY (`sys.stdout.isatty()`). No color, no spinners, no pager when piped.

**Color and quiet.**
- Respect **`NO_COLOR`** (any value), **`TERM=dumb`**, and a **`--no-color`** flag.
- Use color sparingly and only to mean something.
- Offer **`-q/--quiet`** and **`-v/--verbose`**. Brief output on success — never silent.

**Help and discovery.**
- `-h`, `--help`, and `help <sub>` all work and match. Lead with the common flags and **real
  examples**. Accept `-h` at the end of any command.
- No-args (when args are required): print concise help + one example + "pass --help".
- On invalid input, suggest the likely fix; don't auto-run it.

**Errors.**
- Rewrite expected errors in human language with the next action. Most important line last.
- Unexpected errors: write the traceback to a file, print a short message + how to report it.
- Group repeated errors under one header. Signal over noise.

**Safety and interactivity.**
- Prompt only when stdin is a TTY. Support **`--no-input`**; fail with a helpful message when a
  prompt is needed but input is off.
- Confirm dangerous ops; type-the-name for catastrophic ones. Never take secrets via flags.
- **Ctrl-C exits now.** A second Ctrl-C skips cleanup.

**Config and convention.**
- Precedence: flag > env var > project config > user config (**XDG: `~/.config/app/`**) > system.
- Env vars: `UPPER_SNAKE`, single-line, don't commandeer POSIX names. Ask before editing a user's
  config file; if you must, use dated comments.
- Ship **`--version`**. Exit **0** on success, non-zero (mapped) on failure. Name the tool
  lowercase-with-dashes, memorable, no collisions.

**Robustness.**
- Print something within **100ms** so it never looks hung. Show progress for long work.
- Validate early, fail fast. Make operations idempotent and re-runnable (crash-only).
- Time out network calls with sane defaults.

---

## 7. Medium layer — the TUI

The mood board's lessons, made rules:

- **Print the keybindings on the screen** (lazygit, Midnight Commander's F-bar). The ramp lives in
  the chrome. Experts stop seeing them; novices never leave.
- **A command line is the expert's fast path** (k9s `:pods`, Bloomberg). Offer it alongside the
  keys.
- **Color encodes state** (htop). One glance = system model.
- **If you use modes, scream the mode** (vim's `-- INSERT --`). An invisible mode is a trap.
- **One flat surface over infinite depth** (Ableton's clip grid). A beginner taps; an expert
  plays.
- **Never let a dead control look live.** Disabled/planned options must read as such.
- **Refresh without reflow.** Live data updates in place; the layout must not jump under the eye.

---

## 8. Medium layer — the GUI

- **Signifiers, not guesses.** A control must look like what it does (Norman). Clickable looks
  clickable; disabled looks disabled.
- **The diegetic ideal.** The best readout disappears into the work (Dead Space's health on the
  character's spine). Less chrome, full information.
- **Physical-feel beats flat where stakes or speed are high.** The car-button result generalizes:
  a control you can find without looking wins.
- **Theme is a set, not a coat of paint.** Light and dark must each resolve as a whole; never
  define a color in only one theme.
- **Motion serves meaning or is cut.** Animate a state change or a spatial relationship; never
  decorate.

---

## 9. The grading rubric

Score any tool interface out of these. Below 4/6 and it's in the middle.

1. **Pole clarity** — can you name its pole in one word?
2. **Zero-to-first-success** — does a novice win the first try (infant) or is the ramp on-screen
   (expert)?
3. **Throughput** — for the expert path, keystrokes/clicks per real task.
4. **Honesty** — does every control do what it appears to? Are dead/pending controls marked?
5. **Guard rails** — do they match the blast radius (confirm → type-to-confirm)?
6. **State legibility** — after any action, does the user know the new state?

---

## 10. Applied — openDaisugi

openDaisugi is a runtime-assurance layer with a **family/zero-config tier** and a **power/expert
tier**. The doctrine says:

- **Family tier = infant pole.** `daisugi dashboard`: one command, zero deps, zero config, reads
  as plain language. It must work before anything is set. It already does; keep it pure.
- **Power tier = expert instrument + on-screen ramp.** The TUI/GUI should be dense, keyboard-first,
  with a command line and its keybindings printed in the chrome (lazygit model), color encoding
  live state (htop), and honest `[live]/[cfg]/[planned]` tags so no control lies. The per-option
  one-liners we ship are rung one of the ramp — keep building it *inside* the expert tool, not as
  a separate beginner app.
- **The CLI is the expert's native surface.** Hold it to §6: stdout/stderr split, `--json` +
  `--plain`, `NO_COLOR`/TTY, `--version`, `--quiet`, XDG config, confirmation on the destructive
  commands, actionable errors.
- **Heed the glass cockpit.** A runtime-assurance power tier that exposes enforce/shadow, swaps,
  and gates can hurt an under-trained operator faster. Ship the teaching with it: `--explain`,
  dry-run, on-screen "what this does," and honest tags. The Hawaii alert is the worst case for a
  gate that flips protection off without a guard rail.

**Grade openDaisugi against §9 before and after each interface change.** The board is the mirror;
this doctrine is the ruler.

---

## Provenance
Deep-research 2026-08-24 (Apple mouse, Bloomberg, NLS, car HMI 4×, glass-cockpit paradox,
progressive disclosure, Kathy Sierra, Raskin's models, Getting Real — all **[proven]**). Mood board
`ui-moodboard.html`. clig.dev command-line guidelines. Everything unmarked is argued craft, not
proven fact — hold it to that standard.
