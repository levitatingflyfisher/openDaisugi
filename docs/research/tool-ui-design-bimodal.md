# Bimodal tool design — a reading list and a verdict

*Deep-research run, 2026-08-24. 104 agents, 22 sources, 24 claims that survived 3-vote
adversarial checking. This report separates what the evidence proved from what is my own
recommendation. Read the "Honest limits" section before you trust any ranking.*

Companion: [`ui-moodboard.html`](ui-moodboard.html) — the visual study.

---

## The verdict

Your theory holds, with one correction. The evidence supports **two well-served poles plus a
built scaffolding path** — not a bare "avoid the middle."

- **Infant-simple.** One affordance, near-zero learning. A child could use it.
- **Expert-instrument.** Minimal energy for maximum skill. A high-throughput channel between
  a trained hand and the tool. vim, a cockpit, Bloomberg.
- **The only good middle is scaffolding** — a designed on-ramp that carries a novice *into*
  the expert instrument. Not a watered-down third product.

"Avoid the mediocre middle" is the weakest-proven part of the thesis. The strong, well-evidenced
result is: build both poles well, then build the ramp between them. Bloomberg and Engelbart's NLS
did exactly this — they onboard beginners straight into a dense expert tool.

---

## The two poles, with proof

### Infant-simple
- **The Apple mouse.** PARC's mouse had three buttons, cost ~$300, and broke in two weeks. Apple
  cut it to one button "because learning to mouse is a feat in and of itself," required it to cost
  under $15, survive years, and work on Formica and jeans. They also invented the menu bar, pull-down
  menus, and the trash can. A fragile lab instrument became a mass tool. *(New Yorker, "Creation Myth")*
- **Physical car buttons.** On a track test of 12 cars at 110 km/h, a 2005 Volvo with physical
  buttons did four tasks in ~10 seconds. The touchscreen-everything MG Marvel R took 44.6 seconds —
  **~4× longer**. The "do it all on one screen" middle lost badly. *(Vi Bilägare)*

### Expert-instrument
- **Engelbart's NLS.** A five-key chorded keyset (31 combinations, one hand) while the other hand
  used the mouse. A verb-noun command language — type `dw` to Delete Word — built as "a training aid
  of sorts." The command codes *were* the scaffolding. *(dougengelbart.org)*
- **The Bloomberg Terminal.** A mnemonic, color-coded keyboard (yellow Index, green GO, red Cancel)
  and a terse command line: `VOD LN Equity GO`. Keyboard-maximalist throughput — and it was built for
  traders "who had no prior computer experience." Both poles in one product. *(Wikipedia)*
- **Acme (Rob Pike, Plan 9).** Mouse chording: hold button 1 to select, add button 2 to Cut, add
  button 3 to Paste. A dense multi-button idiom, built by a systems programmer, not a UX author.
- **Raskin's efficiency models.** *The Humane Interface* gives four measures — GOMS keystroke model,
  Fitts' law, Hick's law, Raskin's own — all for minimizing task time. This is how you treat an expert
  UI as a measurable channel. *(Note: the popular "habituation drives Raskin" reading was refuted here —
  don't lean on it.)*

### The middle that failed
- **Car touchscreens** (above): the 4× penalty.
- **The 2018 Hawaii false missile alert.** An operator picked the wrong item from an ambiguous
  dropdown and sent a real statewide "ballistic missile inbound, this is not a drill" alert. No
  confirmation, no clear labels. The cautionary tale for a powerful control with no guard rail.

---

## The justified middle: scaffolding

- **Progressive disclosure** (Nielsen/NN/g) is the canonical mechanism. Show the few important
  options first; reveal the rest on request. It serves novices *and* experts in one interface. This is
  the single strongest piece of evidence for "scaffolding, not a binary."
- **Kathy Sierra, *Badass: Making Users Awesome*.** Design for the user's growing competence, not the
  product's lovability. "People care about how awesome THEY are when they use your product." The clearest
  statement of building a path to mastery.
- **Bloomberg / NLS** onboard beginners *into* the expert tool via mnemonics and training-aid command
  codes — one interface with a ramp, not two products.

---

## Reading / watch list (practitioner-first)

**Verified in this run** (survived checking):

| Source | What it is | Why |
|---|---|---|
| 37signals, **Getting Real** (free at basecamp.com/gettingreal) | Practitioner manifesto | "Build Less," "Half, Not Half-Assed," "Start With No." The case for the simple pole. |
| Kathy Sierra, **Badass: Making Users Awesome** | Book | Scaffolding to expertise. The user, not the product, is the hero. |
| Jef Raskin, **The Humane Interface** | Book | Measure the expert channel (GOMS/Fitts/Hick). Modes are the enemy. |
| **folklore.org** (Andy Hertzfeld) | Story archive | Atkinson's QuickDraw craft under brutal constraint. How great tools get made. |
| Celia Hodent, **The Gamer's Brain** + celiahodent.com | Practitioner (ex-Epic/Fortnite UX) | Cognitive-science game UX from someone who shipped it. |
| Edd Coates, **Game UI Database** (gameuidatabase.com) | Live catalog | 1,800+ shipped game UIs, 76k+ screens. Raw reference. |
| Engelbart, **NLS / "Augmenting Human Intellect"** (dougengelbart.org) | Source + demo | The expert instrument as intellect-amplifier. |

**My own recommendations** (the research could *not* verify these in the corpus — offered on my
knowledge, not proven here): Steve Krug **Don't Make Me Think** (the fastest useful read), Don Norman
**The Design of Everyday Things** (affordances/signifiers; a bit academic), Alan Cooper **About Face** /
**The Inmates Are Running the Asylum**, Edward Tufte (information density — the expert-pole aesthetic),
Bruce Tognazzini's **First Principles** (asktog.com), Dieter Rams' ten principles (vitsoe.com), Ryan
Singer **Shape Up** (basecamp.com/shapeup), and Bret Victor's talks (**Inventing on Principle**). Treat
this second list as "worth your time, unverified," not as endorsed by the data.

---

## What this means for openDaisugi

The tiering we already built matches the evidence. The research turns three of your open design
questions into decisions:

1. **Two real poles.** Keep a genuinely zero-learning family tier (`daisugi dashboard` — one command,
   no config, no dependency) and an uncompromising expert tier (keyboard-maximalist, mnemonic commands,
   dense readout). Do not blend them into one mediocre surface.
2. **Build the ramp, Bloomberg-style.** The join should be a progressive-disclosure on-ramp *inside*
   the expert tool, not a separate beginner app: mnemonic commands that teach themselves (like NLS `dw`),
   on-screen key hints (like lazygit), advanced controls hidden until asked for. The `[live]/[cfg]/[planned]`
   tags and per-option one-liners we just shipped are a first rung of this.
3. **Heed the glass-cockpit paradox.** Aircraft with glass cockpits had a *lower* accident rate but a
   *higher* fatal-accident rate — added capability without matching training raises the ceiling and the
   floor at once. A denser power tier for a runtime-assurance tool has the same risk: **ship the training
   with the capability.** For us that means the expert tier must teach as it exposes (hints, `--explain`,
   dry-run), or an under-trained operator will do more damage faster. The Hawaii alert is the worst case.

---

## Honest limits

- **Subset only.** 24 of 94 extracted claims survived 3-vote checking. Absent ≠ wrong. The textbook
  rankings, most of the TUI mood-board entries (vim, emacs, htop, k9s, lazygit, tmux, Norton/Midnight
  Commander, Ableton, Eurorack), and Bret Victor, Tognazzini, Rams, Shape Up, Julie Zhuo, Bill Joy/vi,
  and Bellard all came back "not verified," not "refuted."
- **Thesis mapping is my argument.** The facts are solid; the labels ("expert pole," "the mediocre
  middle") are the synthesizer's framing, not claims the sources make.
- **Weakest element.** "Avoid the middle" rests on two data points (car touchscreens, feature-scope in
  Getting Real). The two-poles-plus-scaffolding read is much better evidenced.
- **One refutation.** The claim that *habituation* is the driver of Raskin's guidelines was refuted
  (1–2). Don't cite it.

---

## Sources
New Yorker "Creation Myth" · dougengelbart.org · Wikipedia (Bloomberg Terminal, Acme, Glass cockpit,
Ableton Live, 2018 Hawaii false missile alert) · nngroup.com/articles/progressive-disclosure ·
basecamp.com/gettingreal + /shapeup · en.wikipedia.org/wiki/Kathy_Sierra + oreilly.com (Badass) ·
folklore.org · celiahodent.com · gameuidatabase.com · asktog.com · vitsoe.com · vibilagare.se · bellard.org.
