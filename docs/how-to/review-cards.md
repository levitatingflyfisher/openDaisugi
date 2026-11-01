# Review the choices daisugi made for you

When daisugi ranks attempts and goes on with the leader, it opens a card. The card says what
it kept, why, and what switching would cost. You answer it later. This page shows how, from the
terminal, and from the floor.

## From the floor

Open the **Cards** view in the rail, or press the `N to review` badge in the garden. Each card
shows what daisugi kept, why, what switching would cost, and how many days are left before it
decays. That is level one, and it shows no code. Tick **See the code** for level two: the diff
of each attempt, read from its worktree. Sort by easiest to undo, widest change, newest or
project.

Three buttons answer a card. **Keep** and **Drop** both record that you kept the choice.
**Switch to X** records your pick. Only you can press them: a pane, a lock-screen link and a
view cannot. An answer also removes the branch worktrees of the closed card.

The floor runs `daisugi rank queue` in the data directory named by `COPPICE_DAISUGI_DATA_DIR`,
or daisugi's own default. Cards from the steward's branches are in the steward's work
directory, so point the variable there to see them.

## From the terminal

The commands below are the same path without the floor.

## See the open cards

```bash
daisugi rank queue                    # open cards, cheapest switch first
daisugi rank queue --sort impact      # the widest diff gap first
daisugi rank queue --json             # the same, for a program
```

A card shows a headline, the reason, the switch cost, and how many days it has left. After 14
days an unanswered card closes as kept. A card whose switch needs a follow-up task closes at
once. Reading a card writes nothing.

If the card came from a branched `tree run`, it also names the command that takes the kept
edits into your tree (`daisugi tree adopt SESSION`).

## See the rows behind a card

```bash
daisugi rank show --choice ch_0123456789ab
daisugi rank show --ranking br_0123456789ab --json
```

This lists the judges' votes, the choice rows, the card and your earlier answers. It reads only.

## Answer it

```bash
daisugi rank record drop ch_0123456789ab        # keep what daisugi kept
daisugi rank record pick ch_0123456789ab b      # prefer attempt b
daisugi rank record pick ch_0123456789ab b --apply   # prefer b and change the work
```

An answer stands. A pick records your preference first and changes nothing else. Your
preference counts against every judge on that pair: a judge that agreed with you gains weight in
later rankings, and one that did not loses it. See `daisugi rank judges`.

## Change the work after you picked

`daisugi rank pivot --choice C --to B` prints what the switch would do. Add `--apply` to do it.
A pivot undoes only the later steps the ledger marks reversible, newest first. It then takes the
chosen edits out of the chosen worktree and applies the other attempt's patch. If a step ran
that cannot be undone, or the other attempt's place is gone, it prints a follow-up task and
changes nothing. Only you run `--apply`: the warden denies an agent's `rank pivot --apply` and
`rank record`.

## Take a branch's edits

```bash
daisugi tree adopt SESSION
```

See [Branch one task into several attempts](branching.md).

## Where the floor finds cards

The floor shows the cards of the data directory it is set to and the cards of the steward's
work directory, where the steward's branching spawns open theirs. A card shows once, and it
names the directory it came from. Your answer goes to that directory, and so does the
clean-up of its worktrees.

## Tasks with an envelope

A task with an envelope starts only fenced Claude panes. Any other pane in that task does not
start: it would have no hook the server wrote, and it would be judged by the `default`
envelope. The steward's own pane in such a task is one case.
