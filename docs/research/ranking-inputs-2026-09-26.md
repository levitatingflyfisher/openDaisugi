# Ranking inputs: how to rank N attempts at one task

Date: 2026-09-26. Method: web research (every outside claim cites a page that was read) plus a
read of `OpenHearth/eloEngine`. Status: research input for a design. Nothing here is built in
daisugi.

## Question

The strategy notes ("Ranking is a primitive", "Human review at speed" in
`docs/plans/2026-09-24-omarchy/strategy-2026-09-26.md`) ask daisugi to rank N attempts at one
task. Hard constraints eliminate. Measured scores rank. Pairwise preferences from model judges
and the owner break ties. The system asks the owner only the most informative pair and stops
when it is confident. This note collects the outside evidence and the owner's prior art.

What exists in daisugi today: no ranking code. The nearest part is the garden A/B harness,
`src/opendaisugi/gardener/ab_test.py`. It compares two envelopes and returns pass or fail, with
an optional boolean `judge`. It does not rank.

## 1. Pairwise ranking models

- **Bradley-Terry (BT).** The chance that item m beats m' is a logistic function of the gap
  between their scores. The scores come from a maximum-likelihood fit over all votes at once
  [1].
- **Online Elo.** Elo updates after each game and assumes skill changes over time. So the result
  depends on game order. Chatbot Arena dropped online Elo for BT because models are static, so
  order must not matter. BT also gave tighter confidence intervals [2]. Attempts at a task are
  static too.
- **TrueSkill.** Each player has a mean mu and an uncertainty sigma. The leaderboard uses the
  conservative score mu minus 3 sigma, which the true skill exceeds with 99% belief. Matchmaking
  prefers pairs with a high chance of a draw [3].
- **How LMArena computes ranks and intervals.** The Arena paper fits BT by maximum likelihood.
  It tried two interval methods, the pivot bootstrap and "sandwich" robust standard errors, and
  chose sandwich intervals because they are smaller in large samples [1]. The 2023 blog
  bootstraps the BT fit to get intervals; models with few votes get wider intervals [2]. The
  current code is Arena-Rank, an Apache-2.0 Python package with BT, closed-form intervals,
  reweighting for models with few battles, and a style-control extension [4].
- **Crowd versus experts.** Arena crowd votes agreed with experts 72% to 83% of the time.
  Experts agreed with each other 79.4% to 89.8% [1]. A human pick is a strong signal, but not a
  perfect one.

## 2. Asking few human questions

- **Arena's rule.** It samples a pair with probability in proportion to how much one more vote
  would shrink that pair's standard error [1]. This spreads effort to tighten every estimate.
- **Active ranking.** Heckel et al. rank items by the chance that each beats a random other
  item. They cover top-k and full orders. Their algorithm only counts wins and decides when to
  stop, and it needs a number of comparisons that is optimal up to log factors. BT and Thurstone
  assumptions give "at most logarithmic gains" [5].
- **Just sort it.** Maystre and Grossglauser show that repeated noisy Quicksort under BT does as
  well as state-of-the-art active methods, and much better than random pairs, at a small
  fraction of the compute [6].
- **Information gain.** ASAP picks the pair with the highest expected information gain, with
  message passing to keep cost down. It reports the best score accuracy of the methods it tested
  [7].
- **Dueling bandits.** The learner only sees pairwise outcomes and must find the best arm. The
  winner concepts are Condorcet, Copeland and Borda; RUCB picks pairs by upper confidence bounds
  [8].

**The simplest practical method for daisugi.** N is small (2 to 8 attempts, so at most 28
pairs). The router, the garden and a parent sprig mostly need the best attempt plus a
confidence, not a full order. So: fit BT, find the leader, and ask the owner about the
leader-versus-challenger pair whose fitted win chance is closest to 0.5. Stop when the leader's
fitted chance against every survivor clears a threshold, or when the question budget runs out.
This is a top-1 case of the win-count-and-stop idea in [5]. It is my synthesis, not a published
algorithm, and its thresholds need a test on real data.

## 3. Model judges, and why tests come first

- **Biases.** MT-Bench names position, verbosity and self-enhancement bias. Strong judges still
  reach over 80% agreement with humans, which is the human-human level [9].
- **Position.** The MT-Bench fix: call the judge twice with the order swapped, and count a win
  only when both orders agree; otherwise call it a tie. GPT-4 was consistent in only 65.0% of
  swapped cases [10]. CodeJudgeBench finds that code judges are still order-sensitive and random
  [11].
- **Length.** Length-controlled AlpacaEval fits a regression on the judge's preference and sets
  the length gap to zero. This raised its Spearman correlation with Chatbot Arena from 0.94 to
  0.98 [12].
- **Self-preference.** Judges score their own outputs higher, and the bias grows linearly with
  the model's ability to recognize its own text [13]. Mitigation: use a judge from a different
  model family than the generator, and hide which model wrote which attempt.
- **Code-specific.** CodeJudgeBench: thinking models judge code much better, and pairwise judging
  beats scalar scores [11]. But execution is the stronger signal. CodeT runs candidates against
  generated tests and picks by agreement; it raised HumanEval pass@1 to 65.8%, an 18.8-point
  gain [14]. This supports the strategy's order: tests and proofs eliminate and rank first;
  judges only break ties among survivors.

## 4. Arena and best-of-N in coding agents

| Product | What it does | How the user sees it |
|---|---|---|
| Grok Build "Arena Mode" | Scores competing agent outputs against each other before review; up to 8 parallel agents [15] | A ranked list [15]. **Status not verified:** TestingCatalog saw only code traces [16]; DevOps.com (2026-05-15) says it is coming and not in the early beta [15]; a third-party blog (2026-05-16) says it shipped, scored on correctness, style and tests [17] |
| Cursor | Same prompt to several models, each in its own git worktree [18] | Side by side. Cursor's own blog says it "will also suggest which solution it believes is best" [18]; a third-party guide says `/best-of-n` leaves the pick to the user [19] |
| OpenAI Codex cloud | `codex cloud exec --attempts 1-4`, "best-of-N" [20] | The user reads the attempts and picks one by hand [21]; `codex apply` brings a diff local [20] |
| Claude Code | No best-of-N feature in the official docs. It has worktrees, agent view, and dynamic workflows that "cross-check" subagent results [22] | Not applicable |

No product found shows a pairwise "which is better" screen with active pair choice. Grok and
Cursor show ranked or side-by-side lists; Codex shows N tabs.

## 5. Prior art: OpenHearth eloEngine

Pure Dart, no dependencies, MIT (`OpenHearth/eloEngine/README.md`). Lilt and Mantle depend on it
(their `pubspec.yaml`).

**What it implements:**
- 15 algorithms (`lib/src/models.dart`, `AlgorithmId`). Three are online, updated in
  `record()`: Elo with K = 64/32/16 at 0/10/30 matches (`elo_math.dart`, `models.dart`
  `EloConfig`), Glicko-2 (`glicko2.dart`), and two-player TrueSkill ranked by mu minus 3 sigma
  (`trueskill.dart`). Twelve are batch over a win/tie matrix (`pairwise_matrix.dart`): BT by
  Zermelo iteration, Thurstone, SpringRank, HodgeRank, SerialRank, matrix factorization
  (`parametric_algorithms.dart`), and Borda, Copeland, PageRank, Markov, Schulze, Ranked Pairs
  (`batch_algorithms.dart`).
- Consensus = harmonic mean of rank-scores across enabled algorithms, plus mean pairwise Kendall
  tau between algorithms and a per-item `rankSpread` (`engine.dart`).
- Convergence: Kendall tau between the ranking now and 5 matches ago; converged when the last 5
  tau values are all at least 0.95 and at least max(N, 20) matches are in (`engine.dart`
  `_updateConvergence`, `EloConfig`).
- `nextMatch()`: a heuristic on Elo ratings only. Priority = uncertainty (1/(1+matches) per item)
  + closeness (expected score near 0.5) minus a recency penalty (`engine.dart`). It is not
  information gain.
- `EloMerge.combine`: merges per-person sessions by harmonic mean, arithmetic mean or minimum,
  with a per-item agreement score (`merge.dart`).
- Undo by replay, JSON round-trip, skip and tie outcomes (`engine.dart`, `models.dart`).

**What it does not have:**
- No confidence intervals, other than Glicko RD and TrueSkill sigma.
- The BT fit has no prior. An item with no wins gets strength 0 and ranks last, and the fit
  cannot place items across a disconnected comparison graph. With few human picks, both happen.
- `EloMatch` holds only `idA`, `idB`, `outcome`, `timestamp` (`models.dart`). There is no judge
  identity or weight, so it cannot tell the owner's pick from a model judge's.
- No per-judge cycle check. The only cycle measure is HodgeRank `cyclicMagnitude` over all
  matches pooled (`parametric_algorithms.dart`). Its `harmonicMagnitude` is the fraction of
  uncompared pairs, not the Hodge harmonic component.

**What could be ported:** Kendall tau; the Zermelo BT fit (add a prior); HodgeRank
`cyclicMagnitude` as a "this judge is inconsistent" signal, run per judge once matches carry a
judge id; the Elo `expectedScore` closeness term for pair choice; `EloMerge` minimum strategy
as a strict "every judge must like it" option. Do not port the max(N, 20) floor: it means 20
questions for 2 attempts. Do not port the 15-algorithm ensemble as the core; at N of 8 or less a
BT fit with bootstrap intervals is cheap and the ensemble adds little. Any port follows the full
parity ruling: Python oracle first, then Go and Rust.

## Recommendation

1. **Eliminate (fail closed).** Out of envelope, failed proof, failed required test: out. If
   every attempt is out, return none, never the least bad.
2. **Rank by measured scores**, lexicographically: tests passed, then cost (tokens, time, diff
   size).
3. **Break ties with BT** over pairwise votes, with a weak prior (pseudo-counts), and bootstrap
   intervals (cheap at N of 8 or less). A fit over all votes is order-free, so it replays the
   same result from the journal.
4. **Model judges:** count a win only when it holds under a position swap, else a tie [10]. Use
   a judge from a different family than the generators and hide the author [13].
5. **Ask the owner** the leader-versus-challenger pair nearest 0.5; stop at a confidence
   threshold or a budget (section 2).

**Open (owner decides):**
- **Weight of the owner's pick.** Options: (a) a veto that overrides judges; (b) a BT vote worth k
  judge votes; (c) a separate owner-only fit that judges only seed. Recommendation: (b) with a
  large k, and (a) for any direct contradiction.
- **Stop threshold and question budget.** Options: fixed (for example 0.9 and 3 questions) or
  per task class. Recommendation: fixed at first, then tune from the journal.
- **Where it lives.** A daisugi `rank` beside `verify`, or a plugin in coppice or sprig. The
  strategy places it in daisugi; sprig is on hold, so any sprig use is **Proposed**.

## Sources

1. https://arxiv.org/html/2403.04132 (Chatbot Arena paper)
2. https://www.lmsys.org/blog/2023-12-07-leaderboard/
3. https://www.microsoft.com/en-us/research/project/trueskill-ranking-system/
4. https://arena.ai/blog/arena-rank
5. https://arxiv.org/abs/1606.08842 (Heckel, Shah, Ramchandran, Wainwright)
6. https://proceedings.mlr.press/v70/maystre17a.html
7. https://arxiv.org/abs/2004.05691 (ASAP)
8. https://www.ijcai.org/proceedings/2018/0776.pdf (Advancements in Dueling Bandits)
9. https://arxiv.org/abs/2306.05685 (MT-Bench abstract)
10. https://arxiv.org/html/2306.05685 (MT-Bench, position swap, Table 2)
11. https://arxiv.org/abs/2507.10535 (CodeJudgeBench)
12. https://arxiv.org/abs/2404.04475 (Length-controlled AlpacaEval)
13. https://arxiv.org/abs/2404.13076 (self-preference and self-recognition)
14. https://arxiv.org/abs/2207.10397 (CodeT)
15. https://devops.com/xai-enters-the-coding-agent-race-with-grok-build/
16. https://www.testingcatalog.com/xai-tests-parralel-agents-and-arena-mode-for-grok-build/
17. https://codex.danielvaughan.com/2026/05/16/grok-build-vs-codex-cli-xai-parallel-agents-arena-mode-competitive-analysis/
18. https://cursor.com/blog/agent-best-practices
19. https://www.agentpatterns.ai/tools/cursor/agents-window/
20. https://learn.chatgpt.com/docs/developer-commands?surface=cli
21. https://www.linkedin.com/posts/embirico_codex-can-now-generate-multiple-responses-activity-7339384974241935360-CjZE
22. https://code.claude.com/docs/en/agents
