# Improving the daisugi interface — through two lenses

**Status:** mostly design-only. Shipped so far: part of W9 (backend auto-detection,
conditional embedder device, load-noise suppression, 2026-08-27); W3 and W4 in full
(plan 5, 2026-08-28 — see their sections below for what shipped and one correction
adversarial review caught); W5, W7, and W8 in the TUI (plan 4); W6's error rendering
(plan 1). The rest of W9, and W1/W2 as a *named, tested* surface distinct from what
plan 1/2 already shipped, remain design-only here.
**Date:** 2026-08-26
**Method:** the daisugi CLI and TUI were put through two lenses, in sequence:
- **The Raskin lens** — *The Humane Interface* (Jef Raskin). Modes cause errors; one
  gesture, one meaning; there is no beginner and no expert; your time and your work
  are sacred; each error message is a design defect.
- **The clig lens** — *Command Line Interface Guidelines* (clig.dev). Human-first
  output; a CLI is a conversation; behave under a pipe; never hang silently; the
  Basics are not optional.

Where the two lenses pull in different directions, the divergence is named at the end.

---

## Evidence (measured 2026-08-26; W3/W4 rows re-measured 2026-08-28 after plan 5)

| Probe | Result (2026-08-26) | After plan 5 (2026-08-28) | Lens reading |
|---|---|---|---|
| `daisugi` with no args | Dumps a 66-command rich table, colour, box-drawing; no examples, no "start here" | 11-line `console.say` start-here text, no box drawing: `daisugi start`, `daisugi orchestrate "…"`, `daisugi status`, `daisugi dashboard`, `daisugi help --all` | clig: bare-run should be concise, examples first |
| Total surface | **66 commands across 12 groups** | **10 visible top-level commands in 5 panels** (Start here / Run / Gate / Garden / Settings); 70 commands total across 12 groups still exist and still run — hidden, not deleted, and still `Did you mean`-suggested on a typo | Raskin: visibility failure + Hick's law; clig: hard to learn |
| Typo'd command | `Did you mean 'status'?` | Unchanged, and now confirmed to work for a *hidden* command too (`relaese` → `release`) — Click builds suggestions from `list_commands`, hidden included | Both: a real strength — keep it |
| Piped output | ANSI colour codes present in a non-TTY pipe | Shipped in plan 2 (`console.py`); `start`'s own step output routes through `console.say`/`note` so it inherits the same pipe-safety | clig: colour must be off when piped |
| `--plain` / `-q` | **0 / 0** across the CLI | Shipped in plan 2 (root flags); `start --dry-run` under `-q` still prints its step table (`say`, not `note` — the table is a result, not progress chatter) | clig: no machine-plain, no quiet mode |
| `--json` | Present at 25 sites (not universal) | Unchanged by this plan | clig: good, but inconsistent |
| `isatty` / `NO_COLOR` handling | Only 2 modules of the codebase | Unchanged by this plan (plan 2's `console.py` is the shared implementation both already use) | clig: TTY detection is not global |
| Cold start | **~0.8s just to print `--help`** (Z3 import); several seconds for a cold gate, silent | Unchanged by this plan; `start` itself starts the resident gate (plan 2) so the *next* gated call is fast, and only reports the gate-server step "done" once its socket actually appears (polled, not assumed) | clig: never silent >100ms. Raskin: do not waste time |
| Getting-started commands | **Three** rivals: `quickstart`, `setup`, `onboard` (plus `run`) | **One**: `daisugi start`. `quickstart` and `gate quickstart` are deleted (not aliased); `setup` moved under the hidden `tiers` group with a redirect stub at the old name (`setup`↔`tiers` aren't close enough for Click's own did-you-mean); `onboard`/`run` remain as hidden, still-runnable, still-suggested escape hatches, not rivals on the visible surface | Raskin: monotony violation; clig: ambiguous siblings |
| Verdict mode | `--mode enforce` flag **and** `gate_mode` config both set it | Unchanged by this plan (plan 1's `config.gate_mode` precedence); `start` defaults to shadow and only arms enforce on the explicit `--enforce` flag | Raskin: two gestures for one result; and shadow/enforce is a mode |
| Secrets on flags | None found | Unchanged | clig: a strength — keep it |
| `modules` map markers | Honest `●` / `○` / `·` + `[live]`/`[cfg]`/`[planned]` | Unchanged by this plan | Raskin: words and visible state — a strength |

---

## Findings by lens

### The Raskin lens sees
1. **The 66-command surface is a visibility failure.** A humane interface does not
   ask the operator to memorise a vocabulary or root around for a command. Hick's law
   says choosing among 66 top-level options is slow by construction. And there is no
   "expert" who has all 66 in habit — you know each command or you do not, one at a
   time.
2. **The verdict has a mode.** `shadow` observes and never blocks; `enforce` denies.
   The checked action is the same gesture; the result depends on a state (the mode)
   that is not at the operator's locus of attention. That is the exact shape that
   produces mode errors. It is also set two ways (flag and config) — one result, two
   gestures, which is a monotony violation.
3. **Three ways to begin.** `quickstart`, `setup`, and `onboard` all mean "get me
   going." Every duplicate is a decision the operator must stop and make.
4. **Silence wastes sacred time.** ~0.8s to show help and several seconds for a cold
   gate, with nothing on screen, breaks the second law: do not make the user wait.
5. **Strengths to protect:** the honest `●/○/·` markers, words not icons, did-you-mean,
   and shadow-by-default (a safe default) are all humane and should survive any change.

### The clig lens sees
1. **The bare run fails the bare-run test.** Running `daisugi` should print a concise
   description, one or two examples, and a pointer to `--help`. It prints the whole
   help table instead — no examples, no "start here."
2. **It does not behave under a pipe.** Colour leaks into non-TTY output; `NO_COLOR`
   and `isatty` are handled in only two modules; there is no `--plain` and no `-q`.
   A well-behaved part of a larger system turns these off when piped.
3. **It can hang silently.** ~0.8s to `--help` and a multi-second cold gate with no
   progress line fails the hanging test (nothing silent for >100ms).
4. **`--json` is not universal.** Good coverage, but a script cannot rely on it
   everywhere.
5. **Strengths to protect:** did-you-mean suggestions, no secrets on flags, and the
   `--force` guards on re-runs are all correct clig behaviour.

---

## Field report — first real use (2026-08-27)

The first time the TUI was actually opened, the reaction was that it *"felt like a
settings page with some weird command options I didn't understand."* This is the Raskin
visibility failure made concrete, and it points past the CLI to the TUI itself. The
dashboard leads with the module **wiring** — configuration toggles and swap knobs — not
the live work. An operator who opens it wants to see what their agent is doing and what
the gate just allowed or denied; configuration is a second-order need. Leading with
config, in unfamiliar vocabulary, is exactly why it read as a settings page.

---

## Field report 2 — the first real command (2026-08-27)

Running `daisugi orchestrate "Hello how are you doing?"` failed twice over, and the two
failures together demonstrate most of this plan at once.

1. **Wrong default backend.** It tried the Anthropic API, found no `ANTHROPIC_API_KEY`,
   and died — on a box whose keyless `claude-code` backend works. The right invocation,
   `daisugi orchestrate "…" --llm claude-code`, succeeds. The tool did not work before it
   was configured, and it did not detect the auth it actually had.
2. **Useless retries.** The missing-key error was retried three times, identically. An
   authentication error cannot succeed on retry; the attempts only tripled the noise.
3. **The error was a raw internal dump.** The user saw `<failed_attempts><generation
   number="1"><exception>…` — the retry machinery, not a sentence. No plain cause, no
   next step.
4. **Engine noise in the operator's face.** An HF-hub warning, a `BertModel LOAD REPORT`,
   and a full CUDA `sm_61` incompatibility lecture printed before anything useful. The
   known fix — `CUDA_VISIBLE_DEVICES=""` to run the embedder on CPU — is not
   baked in.
5. **A non-task is a crash, not a graceful answer.** "Hello how are you doing?" decomposed
   to zero steps and raised `DecompositionError: decomposition produced no steps`. The
   humane outcome is to notice it is not an orchestration task and say so, or answer it
   directly — not to raise an exception.

With `--llm claude-code` and the GPU forced off, a real task ("List three benefits of
local-first software") runs end to end and returns a synthesized answer with a routing and
budget summary. The engine is fine. The defaults and the messages are the defects.

---

## The plan — nine workstreams, worst first

Each names the lens that motivates it, the evidence, and the proposed change. None is
implemented here.

### Tier A — the Basics (cheap, high value)

**W1 · Behave under a pipe.** *(clig)*
Detect `isatty` once, globally. When stdout is not a TTY, or `NO_COLOR`/`TERM=dumb`
is set: no colour, no box-drawing, no spinners. Add a global `--plain` (machine-
readable, greppable) and `-q/--quiet`. Make `--json` universal, not per-command.
Evidence: ANSI in a pipe; `--plain 0`, `-q 0`; isatty in 2 modules only.

**W2 · Never be silent for more than 100ms.** *(clig + Raskin)*
Lazy-load Z3 so `--help` and argument errors are instant. During a cold gate, show a
one-line progress note on a TTY ("verifying… warming the checker"). Offer a persistent
gate so per-call latency approaches zero. Evidence: ~0.8s to `--help`, multi-second
cold gate, both silent. Relates to sprig's `--gate-timeout` work.

**W9 · Work out of the box: right backend, usable device, no env-var soup.** *(both + field report 2)*
Detect the available auth and pick the working backend automatically — no key plus a local
`claude` means `claude-code`, not a dead Anthropic call — with an explicit `--llm` or a
config value always winning. Choose the embedder's device *conditionally*: the GPU when it
can actually run, CPU only when it cannot (a Pascal GPU's torch build lacks `sm_61`, so the GPU
is genuinely unusable there) — a real check against `torch.cuda.get_arch_list()`, never a
blanket force to CPU. The model is already cached (`~/.cache/huggingface`); the repeated
`BertModel LOAD REPORT` and the HF-hub warning are verbose logging, not a re-download —
quiet them (offline after first fetch, transformers log level), do not "cache better."
Never retry a deterministic error (auth, config); rewrite it as one plain line with the fix.
Turn a zero-step decomposition into a plain answer, not an exception.

And dissolve the env-var soup. Environment variables are invisible accumulated state — set
once, forgotten, baffling when you return to a tool after a break (Raskin: a hidden
persistent mode; clig: persistent config belongs in a visible, versioned file). So:
auto-detection means the common case needs zero env vars; persistent choices live in a
config file you can open and read; env vars stay for transient overrides only; and daisugi
echoes what it resolved ("backend: claude-code · embedder: cpu — GPU unsupported") so the
state is never hidden. Evidence: the first `daisugi orchestrate` runs, and the operator's
note that accumulated env vars become a headache after a break.

**Shipped 2026-08-27** (commits ebd053c, f8a51e6, 4b6b27c): backend auto-detection (no key +
local `claude` → claude-code), conditional embedder device (GPU only when the torch build
supports it, else CPU — no blanket force, no `sm_61` crash), and load-noise suppression.
Bare `daisugi orchestrate "<task>"` now runs with no key, no flag, no env vars, and ~6
lines of pure-signal output (was ~30). **Shipped (plan 1):** deterministic errors are
never retried (`llm.preflight`), the instructor XML is gone (`translate_llm_error`), a
zero-step prompt is a plain answer with exit 0, `daisugi config` shows every setting with
its source, and every token-spending command echoes `backend · gate · data` once.

### Tier B — the big usability wins

**W3 · Collapse the 66-command surface into one obvious path.** *(both)*
Redesign the bare run to show ~5 real tasks with example invocations and a single
"start here," then `daisugi help --all` for the full set. Demote rarely-used commands
into their groups so the top level is short. This serves clig (concise bare-run,
learnable) and Raskin (lower Hick's-law load; stop asking the operator to memorise a
vocabulary). Evidence: 66 commands, 12 groups, no examples on the bare run.

**Shipped (plan 5):** the bare run (and `daisugi help`) is an 11-line `console.say`
start-here text — five real tasks with example invocations, no box drawing, ending in
`daisugi help --all`. The visible top level is **10** commands in five
`rich_help_panel`s (spec §8.2's exact list: `start, status, dashboard, orchestrate,
install, config, gate, pathways, journal, help`); the rest (12 groups, 23 more
top-level commands) is hidden but reachable and still suggested on a typo —
`help --all` lists everything, hidden included, walking the Click command tree
directly rather than Rich's own `--help` renderer (which never shows hidden
commands, and would have been a second, drifting source of truth).

**W4 · One way to begin.** *(both)*
Replace `quickstart` + `setup` + `onboard` with a single `daisugi start` that gets a
working shadow-mode gate over the current session in one gesture. Keep the others only
as documented aliases during a deprecation window, then remove them. Evidence: three
rival getting-started commands.

**Shipped (plan 5):** `daisugi start` — five reported steps (`harness`, `hook`,
`envelope`, `gate-server`, `view`), each `done`/`skipped`/`failed`/`would`;
`--dry-run` changes nothing (one `act` flag gates every writer); idempotent (a
second run skips what the first did); `--enforce` is the only way to arm, `--ask`
is refused without it. No deprecation-window aliases: `quickstart` and
`gate quickstart` are deleted outright and Click's own did-you-mean bridges the
typo (`quickstart`→`start` are close enough); `setup` moved under the hidden
`tiers` group, with a small stub left at the old top-level name that redirects
in words (`setup`/`tiers` are not close enough for did-you-mean to bridge on its
own). One correction from the original design: the installed gate hook is scoped
to `<cwd>/.claude/settings.json` and the registered envelope is keyed to that
directory, not the shared `default` one — a machine-global hook whose only
envelope belonged to one project would deny every *other* project's sessions the
moment `--enforce` was flipped (caught in adversarial review, BLOCKER B-1).

**W8 · Make the TUI an operator's instrument, not a settings page.** *(Raskin + field report)*
First real use (2026-08-27): the dashboard read as a settings page with unfamiliar
options. Make the default view the live gate multi-session view — actions streaming, each allowed or
denied with its reason, agents running — and demote wiring and config to a second view.
Give every knob a plain-language label and a one-line, on-screen explanation (the ramp),
so nothing is a weird option the operator does not understand. Evidence: the user's
first session.

**Shipped (plan 4):** `daisugi dashboard --tui` opens on the **sessions** view — every
session grouped by what the operator must do (`NEEDS YOU` / `WORKING` / `PARKED` /
`DONE`), its last action, verdict + clause, step count, `↑fresh ⟳read ✎write`, and age,
with a peek pane and an alerts strip. Two more views cycle with **Tab**
(sessions → tree → wiring) or open by `:sessions` / `:tree <id>` / `:wiring`; **wiring**
is the old swap floor, never first. Keys (same meaning on every screen where they exist):
`j/k` move · `a` allow (then `⏎`; a *destructive* would-deny needs a typed token, not a
reflex `⏎`) · `d` deny · `e` edit (denies and re-asks — an in-place `updatedInput` narrowing
is unverified on the harness, so it never becomes a silent allow of the original) · `r`
remember (writes a proposal nobody auto-applies) · `s` steer (a note, sprig only) · `t`
tree · `⏎` attach (the exact `claude --resume <id>`, copied, on path D). On the **tree**
view: `Ctrl+O` cycles filters (all → no tools → prompts → labeled → verdicts), `L` labels,
`⏎` opens the rewind menu (Restore conversation / workspace / both / Fork here / Never
mind — workspace only when a checkpoint is on the path; the menu names what it will NOT
restore). `: command` · `? keys` · `q quit`. Refresh is a 1 s mtime poll: cell values
update in place, membership/group changes rebuild. `--serve` streams the same app to a
browser via local textual-serve (interval forwarded).

### Tier C — honesty and teaching

**W5 · Make the mode visible, single-sourced, and echoed.** *(Raskin)*
The verdict mode cannot be fully abolished — observing before enforcing is load-
bearing safety. So apply the next humane remedy: make it a *visible* state, never a
hidden one. One source of truth (config), with the flag as a documented override and a
clear precedence. On every gate action, echo the current mode ("gate: ENFORCE") so the
state is always at the operator's locus of attention. This turns a hidden mode into a
shown one and removes the two-gestures-one-result duplication. Evidence: `--mode` and
`gate_mode` both set the verdict.

**Shipped (plan 4):** the TUI header shows `gate: MODE (source)` at all times on every
screen — `ENFORCE`/`SHADOW` from the installed hook's `--mode` else config, `DISARMED`
from the marker (`cockpit.header_state`); the mode indicator rides the shared base screen,
so a pushed view can never hide it. The cache rate says its source and marks a
gateway-blended estimate with `?`.

**W6 · Every error teaches.** *(Raskin + clig)*
Audit every error and every denial. Each states, in order: what was attempted, why it
failed (the envelope clause in plain words), and the one next action. No stack traces
to the human; a bug-report path when it is our fault. The gate's denial reason already
does part of this — systematise it across the CLI. Evidence: error-message audit not
yet done.

**Shipped (plan 1):** `cli.main()` renders every `OpenDaisugiError` as three lines;
`run`, `verify`, `tend`, `orchestrate` use `_fail(what, why, fix)`.

**W7 · Keep the data-dense, honest views; extend them.** *(Raskin)*
The `modules` and `dashboard` views already use words and honest `●/○/·`, `[live]`/
`[cfg]`/`[planned]` markers — protect that. Extend the same honesty everywhere a
control could look live but is not. Make `dashboard` the single "what is happening"
surface. Evidence: the markers are already good; the honesty should be uniform.

**Shipped (plan 4):** the `[live]/[cfg]/[planned]` effect tags survive on the wiring
screen (tested), and the honesty is uniform across the new views — `s steer` reads
`(not on this path)` on Claude, `e` edit is marked *narrowing unverified*, and attach
names exactly what each harness path can do rather than dressing a dead control as live.

---

## Sequencing

1. **W1, W2, W9** first — they are the Basics, they are cheap, and they make everything
   else measurable (a piped, quiet, non-hanging CLI that works out of the box is testable).
2. **W3, W4, W8** next — the largest felt improvement for a new operator (the TUI included).
3. **W5, W6, W7** as steady polish.

A natural checkpoint: after W1–W4, re-run both lenses' diagnostic tests and record the
before/after (E and character efficiency for the common flows; the bare-run, hanging,
script, and conversation tests).

---

## Where the two lenses diverge (named honestly)

- **On the 66 commands.** clig is content with a large CLI *if* it is well-structured
  (`noun verb`, consistent, no ambiguous siblings). Raskin is not — he would question
  whether a 66-command CLI should stay a CLI at all, versus a single unified
  instrument with one set of elementary operations and universal undo. This plan takes
  the clig-compatible path (collapse and structure) because it is reachable now, while
  noting Raskin's deeper claim: the real fix might be fewer *concepts*, not just fewer
  visible commands.
- **On modes.** Raskin's first remedy is *do not have modes*. clig has no objection to
  a `--mode` flag. W5 keeps the mode (safety requires it) but adopts Raskin's fallback
  — make it a visible, single-sourced state — rather than his ideal of abolition.
- **On colour and boxes.** clig says colour with intention, off when piped. Raskin says
  pack the screen with data and prefer clarity to decoration. They agree on "off when
  piped"; they differ on how much chrome is welcome on a TTY. This plan follows clig on
  the TTY and Raskin on density where they meet.

The two lenses agree on the important things: **collapse the surface, one way to
begin, behave under a pipe, never hang, and say what changed.** That agreement is where
the plan puts its weight.
