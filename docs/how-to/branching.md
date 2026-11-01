# Branch one task into several attempts

`daisugi tree run` can start more than one child on the same task. Each child
runs in its own git worktree, under the same proved edge and its own share of
the budget. When they have all ended, daisugi drops the ones that fail a hard
check, ranks the rest with `daisugi rank`, and goes on with the leader. A card
for the choice stays open for you to look at later.

Branching is in the Python, Go and Rust daisugi (see [feature status](../feature-status.md)).

## When to branch

Branch on a signal, not by habit. The request must say why. `why` is one of
`failed_verify`, `failed_tests`, `low_confidence` or `operator`. The reason is
written on every branch's row in the tree ledger.

## Ask for it

Put `attempts` (2 to 8) and `why` in the request that `tree run` reads on stdin:

```json
{"task": "fix the parser", "envelope": {}, "tools": ["Read", "Edit"], "attempts": 3, "why": "failed_tests"}
```

Then run it from a directory inside a git work tree:

```bash
daisugi tree run --parent top --profile default --tokens 200000 --turns 20 \
  --parallel 3 --check tests='pytest -q' --judge local/judge-9b -
```

- `--parallel K` is how many branches live at once (default 4, never more than `attempts`).
- `--check NAME=COMMAND` runs in each branch's worktree after it ends. Exit 0 is a pass. A
  failed check eliminates that branch. Checks come only from this flag, never from the child.
- `--judge PROFILE/MODEL` asks a model to compare the survivors, in both orders. The judge must
  be of another model family than every author. See `daisugi rank judge`.

## What it costs

Each branch reserves the full `--tokens` and `--turns` from the parent. The parent must have
`attempts` times that left. If it does not, daisugi starts nothing and makes no worktree:

```
the parent has 150 tokens left; 2 branches of 100 each need 200
```

## What comes back

The reply holds the usual `status`, `session` and `answer` of the leader, and also
`branches`, `leader`, `choice_id` and `ranking`. The status is `ran` when a branch is left,
`failed` when none is, and `refused` when the budget or a worktree was refused.

The branch worktrees are under `<data dir>/tree/branches/<session>`. The leader's edits stay
there. daisugi does not copy them into your working directory by itself. You take them with
`daisugi tree adopt SESSION`. The card names the command.

```bash
daisugi tree adopt n-0123456789ab   # apply that branch's edits to the repository it came from
```

`adopt` applies the branch's diff against the commit the branch started from, with
`git apply --3way`. The parent wrote that commit and the repository on the branch's row in the
tree ledger when it made the worktree. It never reads either from the worktree, which the child
can rewrite. Before each git call it checks that the worktree is still the linked worktree git
made for that branch: its `.git` is a regular file, and its git directory is
`<repository>/worktrees/<branch>`. The repository's own git directory, or the git directory of
another branch, does not pass. A worktree with a `config.worktree` file in its git directory does
not pass either: git reads that file when `extensions.worktreeConfig` is on, and the child can
write it. Every git call it makes passes `core.fsmonitor=false`, an empty hooks path, and
`diff.ignoreSubmodules=all`, and its diffs pass `--ignore-submodules=all`, so a config or a hook a
child wrote does not run, and a repository the child made inside its worktree is not asked for its
status. The parent adds files with `git add` but leaves out the nested repositories the index
already holds.
It refuses, in one line and with exit code 2, when a file the edits touch has uncommitted
changes in your tree, when the edits do not apply cleanly, or when the worktree no longer
belongs to its repository. A refusal leaves your tree as it was and writes to no other
repository. The one thing it may write is the branch's own worktree index: it marks the
branch's new files there so the diff shows them. Use `--cwd DIR` to apply to another tree. Use `daisugi rank pivot` to see what moving
to another branch would do.

A branch child that runs under the fence has its worktree as its workspace. It can use git
there. It can write its worktree and its own branch directory
(`<repository>/worktrees/<branch>`), and nothing else in the repository's git directory. That
directory is read only to it: the shared config, the hooks, the object store and the other
branches' directories. It cannot delete or replace your history. Git in the child writes new
objects to a store of its own in its branch directory (the starter sets `GIT_OBJECT_DIRECTORY`
and `GIT_ALTERNATE_OBJECT_DIRECTORIES`), and reads the shared store as an alternate.

When every branch child has exited, and before the parent reads any worktree, the run does two
things:

- It stops every process a child left behind. The run is the child subreaper while the children
  run, so a process that a child started in a session of its own comes back to the run when its
  parent ends. The run sends each one SIGTERM, then SIGKILL after 2 seconds, and waits for it.
  If one does not stop within 5 seconds more, the run reads nothing and refuses. A resident
  warden that the child's fence started is one of these processes: it stops at the end of the
  run, and the next fenced run starts a new one.
- It folds every branch, whatever the child left there. First it gives itself back the rights on
  the branch directory, in case the child removed them. Then it sets the branch's `HEAD` back to
  the commit the branch started from, sets the branch's index to that commit's tree, and deletes
  the branch's reflogs, its own refs, its store, and a lock or a `reftable` directory the child
  made. Git runs on the repository's git directory for this, not through the worktree's `.git`
  file. A link the child made in its branch directory is removed, not followed. The fold
  succeeds only when the reflogs, the refs and the store are gone and `HEAD` and the index name
  the start commit. A branch that does not fold is out of the ranking, and the run removes its
  worktree and its branch directory.

So a child's commits come back to the parent only as the files in its worktree. Every read, the
diff, the hash, `adopt`, `rank pivot` and the floor's card diff, compares those files with the
commit the branch started from. No parent git call reads the child's store, and a worktree whose
store is still there is refused as one that no longer belongs to its repository. Your repository
stays whole: `git gc`, `git fsck` and `git log --all` work while the branch worktrees exist, and
after an adopt you can commit as usual.

This holds only while no other grant of the fence covers the repository. A fenced branch run
refuses to start, in one line and with exit code 2, and spends no budget, when:

- an `--allow ...:rw`, a `--workdir`, or a `file_write` path of the envelope covers the
  repository's git directory or its root;
- the fence words hold `--allow-weak-ipc`. Without the socket fence, a child can ask the
  session bus to start a process that is not its descendant, and the run cannot stop that
  process;
- the repository keeps its refs in the reftable format (`extensions.refStorage` in the
  repository's config). The fold cannot rewrite those refs. Run the branches without a fence,
  or convert the repository with `git refs migrate --ref-format=files`.

Each grant is read as the fence reads it: `~` is your home, a relative path lies under the
branch's worktree, and a symbolic link counts as the place it points to. A run with no fence has
no such limit: the child is not confined.

## Read and clean up

```bash
daisugi tree status                 # a "branches of ..." line for each ranking; "leader" shows
                                    # once a card is open
daisugi rank queue                  # the open cards
daisugi rank show --choice CHOICE   # the votes and rows behind a card
daisugi tree prune-branches         # remove the worktrees of closed cards
```

`prune-branches` removes the worktrees of branches whose card is closed. It leaves a running
branch and the branches of an open card. An open card decays after 14 days, and then its
worktrees can go. A ranking with no card (one survivor, or none) keeps its worktrees for 14
days from the end of the branch: `adopt` is the only way a lone survivor's edits reach your
tree, so nothing removes them before then.

## The judge and its record

`daisugi rank judge ATTEMPTS.json` asks one judge, twice, for the next pair. A reply that is
not one line of `A`, `B` or `tie` is no vote. So is a transport error. `daisugi rank judges`
shows how often each judge agrees with your answers on cards. A judge under 0.5 agreement over
five answered cards is dropped. The others vote with a weight drawn from a Beta posterior of
their record, seeded from the ranking, so the same journal gives the same weights.

## Change the work after the fact

`daisugi rank pivot --choice CHOICE --to ATTEMPT` prints what a switch would do. With
`--apply` it undoes the later steps the ledger marks reversible, takes the chosen edits out of
the chosen worktree, and applies the other attempt's patch. If the undo would be dishonest it
prints a follow-up task and changes nothing. Both worktrees are read against the commit their
branch started from, so a child that committed changes nothing here. If the other patch does
not apply, every touched file goes back to that commit, the chosen edits are applied again,
and the command exits 1. Only you run it: the warden denies an agent's
`rank pivot --apply`.

## From the steward

The steward's `spawn` tool takes `attempts` (2 to 8) and `why` (one of the four words above).
It puts both in the `tree run` request. The hard checks come from the project's envelope file,
`~/.config/coppice/envelopes/<project>.json`, in a `checks` object of name and command:

```json
{"envelope": {}, "tokens": 50000, "turns": 30, "checks": {"tests": "pytest -q"}}
```

Each entry becomes a `--check NAME=COMMAND` flag. The model cannot add one. The worker's report
ends with `N branches ran; card <id> opened: <headline>`, and the steward's `status` ends with
`open cards: N`. The steward never answers a card.

