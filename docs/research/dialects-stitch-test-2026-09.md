# The Stitch test: do real policies compress into dialect words?

Date: 2026-09-27. Status: research result. Stage 1 of `docs/plans/2026-09-24-omarchy/design-dialects.md`
(section 11), parts (a) and (b), and the data half of part (c).

## Short answer

- **The words that pass the reuse bar are mostly encoding glue and shell habits, not policy.**
  Stitch finds a large held-out compression (22% to 57%), but most of it comes from the shape of
  our encoding (list cells, empty fields, `none` slots). Those numbers do not measure a dialect.
  Counting only domain words, 42 of 88 held-out envelopes use a learned predicate word, 77 use a
  learned plan word, and 17 use a learned permission shape.
- **Permission words lose most of their gain on held-out data.** The greedy itemset pass of the
  first test saves 18.3% in-sample but only **5.8%** on the newer 30% of envelopes, definitions
  counted. 6 of its 15 words pass the promotion bar (3 held-out uses, 2 distinct tasks).
- **The strongest finding is vocabulary sprawl in invariants.** The models used 199 distinct
  invariant type names; 169 of them occur once. Of the 81 type names in the held-out envelopes,
  **67 never occur in the older 70%**. No miner can reuse a word that the next envelope renames.
  The first job of a dialect is to fix that vocabulary, not to compress it.
- **One real policy word survives:** `keep_unchanged(target)` (the `file_unchanged` invariant
  with a glob target). Learned on old envelopes, it and its variant with a clean exit are used by
  14 of 88 held-out envelopes (16%). It is still opaque: no trace carries a checkable `expr`.

## Data

The owner's journal copy: 740 traces. The same filter as the first test (task text longer than
12 characters) keeps 321 traces and **294 distinct envelopes**. One program per envelope.

| Fact | Count |
|---|---|
| Envelopes, sorted by `created_at` | 294 |
| Train (older 70%) / test (newer 30%) | 206 / 88 |
| Span / cut | 2026-07-02 to 2026-08-27; cut between 2026-08-05 04:36:33Z and 04:36:54Z |
| Test envelopes whose task text also occurs in train | 3 |
| Envelopes whose recorded verification was a deny | 280 |
| Invariants / with an `expr` | 322 / **0** |
| Postconditions / with an `expr` | 315 / **0** |
| Plan steps: shell / file_write / network / file_read / task / skill | 2,885 / 487 / 338 / 299 / 59 / 3 |
| Simple shell commands after decomposition | 4,499 |
| Shell steps the decomposer refused (not literal) | 277 |
| Network steps with a URL that has a host | **0** (249 empty, 89 hold a search query) |

**Corpus rule.** Design section 9.6 says to mine only envelopes from traces that succeeded and
were not denied. 14 envelopes were not denied, and only 7 have a run that succeeded: too few
for any split. So this test mines all 294.
This is a policy-shape experiment only. A real miner must use the 9.6 rule, and the journal
needs more allowed traces before it can.

## Method

1. **Stitch.** Commit `350804b` of `mlb2251/stitch` (MIT), built once in release mode with the
   memory cap, as an offline tool in a scratch directory. It is not in the repo, the lock files or
   any binary. `compress` learns on the train file; `rewrite` applies the learned library to the
   test file. Every run was single-threaded under a 4 GiB scope.
2. **One intern table** for the whole corpus, built before the split. Short word-like tokens
   (`git`, `file_unchanged`) stay as tokens; every other literal (globs, paths, flags, numbers,
   text) becomes a symbol `L<n>`, decoded after the run. So `src/**` has the same symbol in train and test.
3. **Three corpora, encoded as trees.**
   - *preds*: each invariant as `(inv TYPE TARGET SCOPE)`, each postcondition as
     `(post TYPE PATH-OR-EXPECTED)`, sorted and deduplicated, joined in a list
     `(and ITEM (and ITEM … nil))`. Descriptions are dropped (free text).
   - *plans*: steps in order, as a list `(seq ITEM … nil)`. A shell step is decomposed by the
     repo's own `shell_decompose` into simple commands `(sh HEAD ARG…)` (first 5 arguments) and
     write redirects `(redir_w PATH)`. Also `(fw PATH)`, `(fr PATH)`, `(net METHOD HOST)`,
     `(task)`, `(skill ID)`.
   - *perms*: each permission set as a sorted list `(set ATOM … nil)` inside
     `(perm (shl …) (rd …) (wr …) (hosts …) (mcp …) (network on|off))`. This is a contrast only.
     Permission words belong to itemset mining (design 5.1).
4. **Scoring.** DreamCoder cost (primitive 100, application 1), computed by one script for both
   sides. "With definitions" subtracts the cost of every learned body. Held-out use of a word is
   the number of test programs whose rewrite contains it, with the count of distinct tasks, and of
   distinct tasks not seen in train. The **bar** is design 5.5, test 2: at least 3 test programs
   from at least 2 distinct tasks.
5. **Itemset baseline.** `scan.py`'s greedy pass, learned on train only, then applied to test.
6. **Labels.** A word is *glue* when its unfolded body holds only encoding tokens (`seq`, `and`,
   `set`, `nil`, `none`, field tags, holes). Otherwise it is a *domain* word.

## Numbers

### Compression

| Corpus | Settings | Train saving (defs counted) | Test saving (no defs) | Test saving (defs counted) |
|---|---|---|---|---|
| perms, itemsets (greedy) | 15 words | 18.3% | 12.4% | **5.8%** |
| preds, Stitch | arity 2, 10 words | 57.8% | 56.9% | 52.2% |
| preds, Stitch | arity 3, 20 words, no curried bodies | 61.9% | 65.3% | 56.7% |
| plans, Stitch | arity 2, 10 words | 26.4% | 22.8% | 22.2% |
| plans, Stitch | arity 3, 20 words, no curried bodies | 33.7% | 34.5% | 32.6% |
| perms, Stitch (contrast) | arity 2, 10 words | 40.3% | 39.1% | 36.9% |

### How many held-out envelopes use a learned word

For the three main Stitch runs (arity 3 for preds and plans, arity 2 for perms), out of 88:

| Corpus | Any word | A domain word | No word at all | No domain word |
|---|---|---|---|---|
| preds | 75 | 42 | 13 | 46 |
| preds, `keep_unchanged` family only | | 14 | | 74 |
| plans | 83 | 77 | 5 | 11 |
| perms (contrast) | 88 | 17 | 0 | 71 |

The "any word" column is mostly glue. The domain column is the one that matters.

Stitch's held-out numbers are close to its train numbers. That is not evidence of a dialect. It
is evidence that the encoding repeats itself. The share of train utility that comes from glue
words: preds about 51% (and the biggest "domain" word is mostly glue as well), plans about 43%
(and another 18% from `echo` banners), perms about 89%. The Stitch percentages should be read as
an upper bound on what any tree miner can claim over this encoding, not as a dialect's worth.

### Invariant reuse, item by item

| Fact | Count |
|---|---|
| Held-out invariants | 116 |
| ... whose type name occurs in train | 45 |
| ... equal to a train invariant (type, target and scope) | 28 |
| Distinct invariant type names in the whole corpus | 199 (169 used once) |
| Type names in test / never seen in train | 81 / 67 |
| Held-out invariants of type `file_unchanged` | 23 |

The near-synonyms of the design (`read_only`, `no_modifications`, `no_file_modification`,
`read_only_operations`) and of `file_unchanged` (`file_immutable`, `file_preservation`) are
different primitive tokens to Stitch. It cannot merge them.

## The words, decoded and named

`#0`, `#1`, `#2` are the word's parameters. "Train" is the number of the 206 old envelopes whose
final rewrite uses the word; "test" is the same count over the 88 held-out envelopes, with
distinct tasks in brackets. A use of a word built on another word counts for both. Names are
proposals.

### Policy words (preds)

| Proposed name | Decoded body | Train | Test (tasks) | Bar |
|---|---|---|---|---|
| `rule_then_clean_exit(kind, target)` | any invariant, then `exit_code 0` as the only postcondition | 67 | 22 (21) | pass |
| `keep_unchanged(target)` | invariant `file_unchanged` on `target`, then the rest of the list | 15 | 10 (10) | pass |
| `ends_clean` | last postcondition is `exit_code 0` | 36 | 12 (11) | pass |
| `unchanged_then_clean_exit(target)` | `file_unchanged(target)` and only postcondition `exit_code 0` | 25 | 4 (4) | pass |
| `opaque_rule_then_clean_exit(kind)` | one untargeted invariant of any type, then `exit_code 0` | 27 | 13 (12) | pass |
| `leaves_file(path)` | last postcondition is `file_exists path` | 30 | 4 (4) | pass |
| (clean exit before other postconditions) | `exit_code 0` followed by more postconditions | 7 | 1 (1) | fail |

Only `keep_unchanged(target)` is a policy word in the design's sense. `rule_then_clean_exit`
is a template: its parameters are an invariant's name and target. Its common targets are
`src/**`, `**`, `lib/**`, `**/*` and a named script. `opaque_rule_then_clean_exit(kind)` shows
the problem in one word: its parameter is the invariant's *name*, so Stitch learned "some rule
with a name we cannot read". Its held-out uses fill that parameter with names such as
`file_immutable` and `file_preservation`: synonyms of `file_unchanged` that Stitch cannot merge. The postcondition words are about outputs, not permission.

### Plan fragments (evidence for pathways, not for policy; design 4.4)

| Proposed name | Decoded body | Train | Test (tasks) | Bar |
|---|---|---|---|---|
| `announce(text)` | an `echo` banner inside a step run (two words, before and after) | 97 / 93 | 25 (25) / 31 (27) | pass |
| `enter_workshop` | `cd` to the workshop root directory | 74 | **0** | fail |
| `enter_dir(dir)` | `cd` to one other absolute directory (a session scratch directory) | 45 | 28 (20) | pass |
| `search(args)` | `grep …` | 100 | 27 (22) | pass |
| `search_lines(pattern, path)` | `grep -n …` | 59 | 17 (16) | pass |
| `peek_head(n)` | `head` inside a step run (two words) | 61 / 45 | 24 (17) / 13 (12) | pass |
| `peek_tail(n)` | `tail` inside a step run (two words) | 63 / 50 | 5 (5) / 3 (3) | pass |
| `discard_output` | write redirect to the null device | 70 | 12 (12) | pass |
| `recent_history(n)` | `git log --oneline …` | 49 | 4 (4) | pass |
| `hostless_fetch` | a network `GET` with no host | 40 | 44 (20) | pass |
| `commit(msg)` | `git commit … -m …` | 27 | 0 | fail |
| `run_pytest(args)` | `python -m pytest …` | 18 | 0 | fail |
| `write_then_read(path)` | a file write followed by a read of the same path | 28 | 0 | fail |

`enter_workshop` is the biggest domain word on train (6.8% of train utility) and is never used
on test: the raw held-out plans never `cd` there (74 old envelopes do, 0 new ones). The work
moved to other directories after the cut. This is the drift of design 9.4, across one cut. `hostless_fetch` is a data problem, not a word: every recorded network
step lacks a host.

### Glue words (encoding artifacts)

Examples: `(seq #2 (seq #1 #0))` (200 train envelopes, 81 held-out envelopes), `(and (inv #1 none none) #0)`,
`(set #1 (set #0 nil))`, and record shapes such as `(#1 (wr #0) (hosts nil) (mcp nil) (network off))`.
They pass the bar easily and mean nothing. A miner that reports them as words would pass the
design's reuse test 2 and fail the point of it.

### Permission words (itemsets, held-out)

| Proposed name | Atoms | Train | Test (tasks) | Bar |
|---|---|---|---|---|
| `read_only_inspection` | shell `cat find grep head tail` | 22 | 3 (3) | pass |
| `build_and_test` | shell `make npm pytest python` | 18 | 4 (4) | pass |
| `git_online` | shell `git`, network on | 28 | 13 (9) | pass |
| `search` | shell `find grep` | 27 | 16 (13) | pass |
| `code_forges` | hosts `f-droid.org github.com gitlab.com` | 9 | 8 (5) | pass |
| `browse_files` | `search` plus shell `cat ls` | 8 | 3 (3) | pass |
| `web_source_globs` | read `**/*.js **/*.json **/*.md **/*.ts` | 11 | 0 | fail |
| `source_tree_rw` | read and write `lib/** src/**`, shell `node` | 8 | 0 | fail |
| `tests_rw` | read and write `test/** tests/**` | 6 | 0 | fail |

The Stitch contrast on sorted lists shows the problem the design predicted:
`(set find (set #1 #0))` means "`find`, then whatever atom sorts next". A sorted list is not a
set.

## What the data can and cannot show

- **It cannot show what an invariant means.** All 322 invariants and all 315 postconditions are
  opaque. Stitch mines the shape `(inv file_unchanged #0 none)`; it cannot mine a definition,
  because there is none to mine. These invariants become mineable only after they have kernel
  definitions (design 2.2, 4.2).
- **It cannot measure behavioural reuse** (design 5.5, test 4). This is a count of syntactic
  matches after the fact, not a test that a model emits a word when offered it.
- **It cannot show over-grant** (design 5.5, test 5) for plans or predicates. That needs the
  permission words applied to envelopes and compared with the plans' use.
- **The sample is small and mostly denied.** 88 held-out envelopes over about three weeks
  (2026-08-05 to 2026-08-27); 280 of 294 envelopes were denied. Three held-out tasks repeat train tasks, so leakage is small; the
  "novel task" counts in the scoring files differ from the task counts by at most 2 per word.
- **The plan corpus has blind spots.** 277 shell steps did not decompose, and no network step
  carries a host, so host words cannot come from plans.
- **Compression depends on the encoding.** Any figure above is relative to one encoding and one
  cost model. The glue share shows that most of the Stitch number is the encoding.

## Recommendation for the Zig miner

What it must do that Stitch did not:

1. **Canonicalize names before it mines.** Cluster invariant types and their descriptions into
   a small fixed set (the design's 5.1 cluster step), so `read_only`, `no_modifications` and
   `no_file_modification` become one candidate. 67 of 81 held-out type names are new; without this
   step every miner fails on reuse.
2. **Mine over definitions, and merge by proof.** Once a word has a kernel body, compare bodies
   by Z3 equivalence (design 5.4), not by syntax.
3. **Treat sets as sets.** Use closed-itemset mining for permissions (and for `and` lists of
   invariants), not trees over sorted lists.
4. **Drop glue by construction.** Encode lists as sets or n-ary nodes the miner understands, and
   reject any candidate whose body has no domain token. Report glue share with every run.
5. **Score with a time split and drift.** Keep the 70/30 split, the 3-use / 2-task bar, cost with
   definitions counted, and a decay for words like `enter_workshop` that stop being used.
6. **Filter the corpus by design 9.6** (allowed and succeeded traces only) and refuse to run
   when too few remain. Today that is 7 envelopes.
7. **Abstract machine paths.** Replace absolute directories with a workspace variable before
   mining, so `cd <root>` words generalize across projects and never leak a path into a name.

What to copy from Stitch:

- the cost model (primitive 100, application 1) and "utility = cost saved minus definition
  cost", which makes runs comparable;
- the separate `rewrite` step, which applies a frozen library to new data. This is exactly the
  held-out test, and the Zig miner should keep it as its own command;
- the ban on single-task words (`--allow-single-task` is off by default);
- `--no-curried-bodies`, which removes most partial-application glue at no cost.

**Stop rule check** (design 11, Stage 1). Held-out permission compression is small but above
zero (5.8%). No invariant cluster has a definition yet, so none verifies a recorded plan. The
honest reading: permission words are weakly supported; policy words wait for part (c), a kernel
definition for `keep_unchanged(target)` and its synonyms, and for more allowed traces.
