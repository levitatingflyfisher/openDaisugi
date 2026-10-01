# Adjudication log — cross-client disagreements and oracle findings

Every disagreement between a client and the oracle gets a dated entry: what
disagreed, which implementation is wrong (or what the spec failed to say),
and the resolution. Client authors: record here, then match the oracle;
oracle fixes are batched after the tournament.

**Status and Retires when (added 2026-09-27).** Every ruling below ends with two lines,
after the `decisions.md` ledger of Imp (https://github.com/deepfates/imp/blob/main/decisions.md),
whose rulings each carry a "Status" and a "Retires when":

- `Status:` is `in force`, or `retired <date> by <what>`. A ruling that is in force but has
  lost one part says which part retired, and when.
- `Retires when:` names the event that would end the ruling, or says it does not retire.

A retired ruling stays in place, marked, so the history reads true. Dates come from the
rulings that replaced them, from commits made after the monthly fold (for example 761edcf2,
a30e2597, 843bf76b), or, where the change sits inside a monthly fold, from that fold's month
and commit. "Ruling" here means every numbered entry, and each bold-led entry of the
unnumbered 2026-09-24 sections.

## 2026-08-21 — pre-campaign findings (from fixture generation)

**F-1 (oracle, security-relevant): file scopes are right-anchored.**
`_path_matches_any` delegates to `PurePosixPath.match`, which matches
relative patterns from the right: `file_write: ["out.txt"]` admits
`/etc/cron.d/out.txt`; `["*.py"]` admits any `.py` anywhere. The head
allowlist was explicitly left-anchored for this exact reason
(`_head_allowed` docstring); file scopes were not. Frozen for the
tournament; fix queued for the post-wiring pass (changes verdicts →
corpus regenerates).

Status: retired 2026-08-21 by the oracle fix (F-1 / F-2 in RESOLUTIONS).
Retires when: retired (see Status).

**F-2 (oracle, portability): glob behavior is Python-version-dependent.**
On 3.12, `PurePosixPath.match` treats mid-pattern `**` as a single-segment
`*`; 3.13 makes it recursive. The oracle's verdicts would silently change
on a Python upgrade, and `_match_glob`'s comment already claims the 3.13
behavior. Fix direction: implement matching in the oracle directly (no
pathlib delegation). Queued with F-1.

Status: retired 2026-08-21 by the oracle fix (F-1 / F-2 in RESOLUTIONS).
Retires when: retired (see Status).

## 2026-08-21 — TypeScript client (`clients/ts/`)

**F-3 (oracle, latent crash bug): `NotEquals` always resolves a String Z3
variable, regardless of the predicate value's type.**
`predicate_z3._compile_scalar`'s `Equals` branch checks
`isinstance(expr.value, (int, float)) and not isinstance(expr.value, bool)`
and resolves a numeric Z3 var when true — `NotEquals` has no such check and
always calls `scope.resolve_string(...)`. A `not_equals` predicate authored
with a numeric or boolean `value` therefore compiles to
`z3.String(...) != z3.IntVal(...)` (or `BoolVal`) — a genuine Z3 sort
mismatch, which raises `Z3Exception`.

Where this lands differs by call site:
- `check_vacuity`'s caller (`verify._check_predicate_item`) wraps the call in
  `try/except Exception: vacuity_verdict = "non_trivial"` — the crash is
  swallowed, vacuity classification is skipped, and execution falls through
  to `evaluate_predicate` (ground eval, plain Python `!=`, no Z3, no crash).
  Verdict-invisible.
- `envelope_subsumes`'s `_compile_invariants` call has no such wrapper.
  A `SkillStep` contract envelope carrying a `not_equals` invariant with a
  numeric/boolean value would propagate `Z3Exception` out of
  `envelope_subsumes` → `verify_delegation` → `check_skill_delegations` →
  `verify()` (none of these wrap it either) → an **error verdict** at the
  conformance boundary (`_serve_one`'s outer `except Exception`).

Not reachable from the current corpus: all `not_equals` usages in the 303
verify cases compare strings (`type`, `metadata.signature`), and all 4
`SkillStep` cases carry empty invariants on both sides. Found by code
inspection while porting, not by a corpus mismatch.

**Resolution: matched, not fixed.** The TypeScript port (`predicateZ3.ts`)
throws for a `not_equals` with a numeric/boolean value, reproducing the sort
mismatch's *effect* (silently-non_trivial in vacuity via the same
try/except-style catch in `verify.ts`; propagates to an error verdict in
subsumption, uncaught, matching the oracle's call chain exactly). Filed
prominently since a fresh `not_equals`-with-numeric-value case would
currently error on both the oracle and this client — the Rust/Go/Lean
authors should match the same behavior rather than each independently
"fixing" the asymmetry in a different direction. Oracle fix direction (for
the post-tournament batch): make `NotEquals` branch on value type exactly
like `Equals` does.

Status: retired 2026-08-21 by the oracle fix (F-3 in RESOLUTIONS).
Retires when: retired (see Status).

## 2026-08-21 — Go client (`clients/go/`), decompose port (mvdan.cc/sh/v3 vs tree-sitter-bash)

All found by differential testing against the live oracle
(`clients/go/probe_gen.py`-style probing) while porting `shell_decompose.py`
to `mvdan.cc/sh/v3/syntax`. Every one below is matched in the Go client with
a code comment naming the entry; see `clients/go/internal/verify/decompose.go`.
Gate at time of writing (against the CURRENT, post-oracle-fix corpus —
see "RESOLUTIONS" below for the corpus regeneration): 11,033/11,089
(99.50%) decompose cases matched, 361/361 (100.00%) verify cases matched;
residual explained by G-4/G-4b (see below) and one uncharacterized `[`
interaction (see "Residual, not chased" at the end).

**G-1 (grammar gap, tree-sitter-bash): `<>` (RdrInOut, POSIX read-write
redirect) is not in tree-sitter-bash's grammar at all.** `exec 3<>f` is a
whole-file parse error in the oracle (`root.has_error`); mvdan parses it
correctly as an ordinary `RdrInOut` redirect (`3<>f`, no path classification
needed since fd 3 is just being opened, not written to/read from a literal
path in a way `_classify_file_redirect` would touch). Real bash accepts
`<>`; tree-sitter-bash is objectively narrower here. Matched by rejecting
any redirect with `Op == syntax.RdrInOut` outright.

Status: retired 2026-09-25 by the move of the Go client to tree-sitter-bash, the oracle's own grammar: mvdan/sh is gone (see "Go gate binary, standalone").
Retires when: retired (see Status).

**G-2 (grammar looseness, tree-sitter-bash): the assignment-word grammar
accepts a leading digit; POSIX (and mvdan) do not.** `1FOO=1 git` → oracle
heads `["git"]` (treats `1FOO=1` as a leading env-assignment and skips it);
mvdan puts `1FOO=1` in `CallExpr.Args[0]` (a plain argument — POSIX-invalid
assignment names are not assignments) so the naive port would report head
`"1FOO=1"`. Verified via probing the boundary precisely: `9=1`, `_FOO=1`,
`foo=1`, `123=1`, `a1=1`, `FOO+=1` are ALL treated as assignments by the
oracle (any run of `[A-Za-z0-9_]+`, optionally `+`, then `=`); `FOO-BAR=1`
and `FOO.BAR=1` are NOT (hyphen/dot break it — same as mvdan). Real bash
requires a non-digit-leading POSIX name here too, so mvdan (and real bash)
are correct; tree-sitter-bash's grammar is more permissive than the shell it
claims to parse. Matched by re-peeling leading Args against the empirically-
derived lenient pattern before falling back to mvdan's own (POSIX-strict)
`Assigns` split — see `looksLikeLenientAssignWord`.

Status: retired 2026-09-25 by the move of the Go client to tree-sitter-bash, the oracle's own grammar: mvdan/sh is gone (see "Go gate binary, standalone").
Retires when: retired (see Status).

**G-3 (grammar gap, tree-sitter-bash): `[ ... ]` (the bracket `test` utility)
gets a dedicated non-head-producing grammar node; mvdan parses it as an
ordinary command.** `[ -f x ]` → oracle `heads: []` (rejects with "no
command heads found" when it's the only command); `[ -f "$(mktemp)" ]` →
oracle `heads: ["mktemp"]` (arguments ARE still walked for substitutions,
just no head is contributed for `[` itself). `test -f x` (the word form) is
an ordinary command, head `"test"` — only the literal `[` spelling gets the
special treatment. mvdan has no equivalent node (`[` is just another
`CallExpr` with head `"["`), which is what real bash's own parser does too
(`[` is a regular external/builtin utility, not shell grammar) — tree-sitter
diverges from real bash here, not mvdan. Matched: when the (already-literal)
head is exactly `"["`, don't append a head/command, but keep walking Args.
By far the highest-volume single fix found (663 → 175 "heads differ" cases
in one change).

Status: retired 2026-09-25 by the move of the Go client to tree-sitter-bash, the oracle's own grammar: mvdan/sh is gone (see "Go gate binary, standalone").
Retires when: retired (see Status).

**G-4 — the tree-sitter-bash statement-fusion bug — FIXED IN THE ORACLE,
this section rewritten (not appended) to match. Originally documented here
as a client-side head-suppression workaround; superseded 2026-08-21 when
the oracle itself was fixed (see "RESOLUTIONS" below) to fail closed
instead of silently fusing. This section now describes the CURRENT state:
the oracle's fix, and this client's best-effort prediction of when the
oracle will now reject.**

**What the bug is.** tree-sitter-bash 0.25.1 can parse `c1\nd1` as ONE
`command` node (name `c1`, argument `d1`) without setting `has_error` — the
newline, which real bash treats as a statement terminator, gets silently
swallowed into the command's own span. Left unchecked this is fail-OPEN:
`d1` executes but never faces the permission allowlist, because the
oracle's walker never visits it as its own command. Certain multi-line
scripts combining a pipeline of ≥3 stages with a later redirect-bearing
statement reliably trigger it; bisected down from real corpus transcripts
(see the original probe transcripts in this file's git history / prior
revisions for the derivation).

**The oracle's fix (`shell_decompose._command_has_bare_newline`,
`src/opendaisugi/shell_decompose.py`, read-only reference — never
edited).** For every `command` node the oracle visits, it now checks
whether that node's own span contains a raw newline that is NOT inside a
multiline-legal child (`string`/`raw_string`/`ansi_c_string`/
`translated_string`/`command_substitution`/`process_substitution`/
`arithmetic_expansion`/`heredoc_body`/`heredoc_redirect`) and NOT a
backslash line-continuation. If so, it fails closed with reason
`"ambiguous shell (bare newline inside command — parser statement
fusion)"` instead of silently fusing. The corpus was regenerated under the
fixed oracle: 13,387 → 11,450 total cases (13,084 → 11,089 decompose, 303
→ 361 verify).

**Why this client can't just read the same signal off its own tree.**
mvdan parses `c1\nd1` CORRECTLY as two separate statements — it never
produces a fused `command` node, so there is no "node span with a bare
newline in it" to detect on this side. Matching the oracle therefore means
PREDICTING, from a correctly-parsed mvdan tree, which scripts tree-sitter
would have fused — an empirical, leaky approximation of an opaque GLR
parser's ambiguity resolution, not a direct port of the oracle's check.
This is `detectG4bFusion` in `decompose.go`; when it fires, `DecomposeCommand`
returns the identical rejection reason string the oracle now uses, rather
than suppressing individual heads the way the pre-fix version of this
client did.

Status: retired 2026-08 (monthly fold 2c98de9a) by the oracle's fusion repair: `shell_decompose.py` now rewrites a fused parse into its statements and fails closed only where the repair cannot resolve it, and the Go client's `detectG4bFusion` is gone (`clients/go/internal/verify/decompose_test.go`).
Retires when: retired (see Status).

**G-4b (this client's current best-effort prediction rule, bisected
against the live oracle across three rounds — see `decompose.go`'s
`detectG4bFusion` doc comment for the exact algorithm).** Requires a "T"
statement: some top-level statement, before the file's last one, that
contains a pipeline with ≥3 stages (recursively, through `&&`/`||`/a brace
block/a subshell — confirmed a ≥3-stage pipe hiding inside an `&&`/`||`
branch counts too). From T, walk forward one statement at a time; the walk
requires an unbroken chain of real-newline boundaries (a `;` anywhere in
the chain stops the walk with no trigger). At each statement S reached
while the chain is intact:
- **rule "bare"**: S is NOT itself a top-level pipeline (a plain command,
  or one wrapped in `&&`/`||`/a block/a subshell) and carries a redirect
  SOMEWHERE in it (any type, including a bare fd-dup like `2>&1`, anywhere
  in its subtree, not necessarily its own top-level `Redirs`). Fires
  immediately regardless of S's position — it does NOT need to be the
  file's last statement, and nothing after it matters.
- **rule "pipe-at-eof"**: S IS itself a top-level pipeline (≥2 stages) and
  carries a redirect somewhere in it, AND S is the file's LAST top-level
  statement. The identical pipeline+redirect shape anywhere OTHER than the
  true last statement does not fire.

**HONEST LIMIT — this rule is still a leaky over-approximation, proven
irreducible with real evidence, not merely imprecise at the margins.**
Direct oracle probing found a case (`e30a93242e2d8867`, a Porch-APK-unzip
transcript) where a letter-substituted MINIMAL REPRO of its exact T/(gap)/R
shape reliably fires under the live oracle, while the real, longer script
with the identical abstracted structure does NOT fire. This means
tree-sitter's GLR disambiguation is sensitive to context genuinely outside
what any local T/R/chain model can see — the same conclusion the Lean
client's independent, more extensive probing reached from a different
angle (see the Lean L-1 follow-up below: their differently-derived
two-condition sub-rule explains 87/116 of their own residual and
explicitly leaves the remaining 29 as evidence of "a second, distinct
triggering mechanism... not characterized"). Two independent
investigations, two different rule derivations, both hit a wall at
roughly the same place. Further surface-level bisection was judged
not to be productive and was stopped (rather than risk overfitting the
rule to whichever specific corpus samples happen to be sampled next).

**Current numbers (this client, full 11,450-case corpus, 2026-08-21):**
decompose 11,033/11,089 matched (99.50%); verify 361/361 (100.00%); total
11,394/11,450 (99.51%). Of the corpus's 602 total decompose rejections,
159 are the new "ambiguous shell" (G-4b) reason, 404 are pre-existing
"non-literal head/redirect target" rejections, and 39 are pre-existing
"malformed shell" parse errors — the latter two categories are handled
with zero mismatches (100%), confirming G-4b is the sole remaining gap.
Of the 159 true G-4b rejections: **127 correctly caught (79.9% recall)**,
**32 missed — this client wrongly ACCEPTS** (`detectG4bFusion` under-fires;
this is the dangerous direction for a runtime-assurance verifier, since a
false accept here means a real head hiding inside what tree-sitter fused
would slip past the permission check un-checked — prioritized reducing
this count across three refinement rounds: 148 → 82 → 75 → 50 → 32 missed,
tracked via `clients/go/analyze_mismatches.py --kind decompose`). Of the
~10,487 accepted decompose cases: **24 wrongly REJECTED (99.77%
precision)** — a false reject is a precision/availability cost, not a
safety cost, under this system's fail-closed design.

**Derivation history (three rounds, each validated against real corpus
deltas per case, not synthetic guessing alone):**
1. Converted the original head-suppression rule directly to a rejection
   (T = rightmost ≥3-stage pipe before the file's last statement; R = the
   last statement, required to be a ≥2-stage pipe with a redirect; all
   T..R boundaries newline). 11,296 → 11,362/11,450.
2. Relaxed R: it does not need to be a pipeline at all — a single bare
   command carrying a redirect triggers it too (real corpus case
   `0c29d7a8373aa4f6`). 11,362 → 11,367/11,450.
3. Made both T-detection and R/redirect-detection recursive through
   `&&`/`||`/blocks/subshells (real corpus case `1e20c0513b657b83`: the
   redirect lives on the LEFT branch of an `||` chain, not on R's own
   top-level `Redirs`). 11,367 → 11,382/11,450.
4. Split R into the two-sub-rule "bare" (fires anywhere after T, no EOF
   requirement) vs "pipe-at-eof" (only at the true last statement) model
   above, after direct oracle probing showed a bare redirect-bearing
   statement mid-script (not at EOF) still fires while a pipe-shaped one
   in the same position does not. 11,382 → 11,394/11,450.

Status: retired 2026-08 (monthly fold 2c98de9a) by the oracle's fusion repair: `shell_decompose.py` now rewrites a fused parse into its statements and fails closed only where the repair cannot resolve it, and the Go client's `detectG4bFusion` is gone (`clients/go/internal/verify/decompose_test.go`).
Retires when: retired (see Status).
**G-5 (grammar gap, tree-sitter-bash): no grammar production for bash's
`time` reserved word at all — it parses as an ordinary command whose
literal head is `"time"`.** `time a` / `time a b c` → one command, head
`"time"`, full argv text unsplit. `time a | b` → a TWO-stage pipeline
`["time a", "b"]` (the pipe is still a real pipeline boundary — `time`
doesn't "own" the whole pipeline the way real bash's `time` keyword does).
`time (sub)` → `"time"` alone with EMPTY argv (`(` isn't a valid bare-word
character, so the "time" command node ends right there), then the subshell
is recognized as ordinary structure and walked separately — confirmed via
two real corpus cases (`time (echo ... | timeout ... | head -N)` → oracle
heads include BOTH `"time"` and `"echo"`/`"timeout"`/`"head"` from inside).
`time { a; b; }` → swallows `{` as a literal argument character (`heads:
["time","b","}"]`, commands include the malformed fragment `"time { a"`) —
NOT reproduced (construct absent from the corpus; the general "flatten as a
bare word" model doesn't predict this specific swallowing and it wasn't
worth deriving further). Real bash's `time` is a genuine reserved word
affecting a whole pipeline; mvdan's `TimeClause` matches real bash. Matched
for the CallExpr/BinaryCmd(pipe/list)/Subshell shapes actually observed —
see `walkTimeWrapped`.

Status: retired 2026-09-25 by the move of the Go client to tree-sitter-bash, the oracle's own grammar: mvdan/sh is gone (see "Go gate binary, standalone").
Retires when: retired (see Status).

**G-6 (parser bug, tree-sitter-bash): a heredoc/herestring combined with
certain follow-on redirects or list operators is a whole-file parse error,
even though each ingredient alone parses fine.** Two independently-confirmed
shapes:
1. A pipeline stage carrying a heredoc redirect FOLLOWED (later in the same
   statement) by any other redirect: `cmd <<'EOF' 2>&1 | tail -N` → parse
   error. But: the heredoc alone piped (`cmd <<'EOF' | tail`) is fine; the
   extra redirect alone with no pipe (`cmd <<'EOF' 2>&1`) is fine; the extra
   redirect coming BEFORE the heredoc (`cmd 2>&1 <<'EOF' | tail`) is fine.
   Only heredoc-then-another-redirect-then-pipe fails. This was the single
   highest-value find by case count: 46 corpus cases, all real developer
   transcripts of the very common `python - <<'PY' 2>&1 | tail -N` /
   `... 2>&1 | tail` shape.
2. Two or more heredoc-bearing statements chained by `&&`/`||` — anywhere in
   the chain, not necessarily adjacent, and regardless of `&&` vs `||`:
   `a <<'A' && b <<'B'` → parse error, even though `a <<'A' && echo b` (one
   heredoc) is fine on its own. 3 corpus cases (multi-commit-message
   `git commit -F - <<'EOF' && ... && git commit -F - <<'EOF2'` scripts).
   A closely related shape — one heredoc followed by `;` on the SAME
   physical line as the heredoc's own opener (`cmd <<EOF ; other\nBODY\nEOF`)
   — also parse-errors in the oracle but does not appear in the corpus and
   was not chased (purely synthetic, found only while bisecting #2).
mvdan and real bash parse all of these correctly; tree-sitter-bash's
heredoc-body-boundary tracking evidently gets confused by a follow-on
redirect/list-operator in specific configurations. Matched: reject when a
pipeline's LEFT stage has a heredoc followed by another redirect
(`hasHeredocFollowedByAnotherRedirect`), and reject when an `&&`/`||` chain
contains 2+ heredoc-bearing statements anywhere in it
(`countAndOrHeredocStmts`).

Status: retired 2026-09-25 by the move of the Go client to tree-sitter-bash, the oracle's own grammar: mvdan/sh is gone (see "Go gate binary, standalone").
Retires when: retired (see Status).

**Residual, not chased (1 "expected=False got=True" case):** a script using
`[ "$pubs" \< "$src" ]` (backslash-escaped `<` for POSIX string comparison
inside a bracket test) parse-errors in the oracle; the Go client accepts it.
Single occurrence, not bisected to a general rule — plausibly a further
`[`-grammar interaction (see G-3) but not confirmed. Left as an honest gap
rather than guessed at.

## 2026-08-21 — Go client, predicate-algebra port (Python `re` vs Go RE2 regex dialect)

**G-7 (client-side regex-dialect gap, not an oracle/parser disagreement):
Python's `re` module accepts `\uXXXX` Unicode escapes inside a pattern;
Go's `regexp` (RE2) does not.** The corpus's one real-world regex predicate
(`no_impersonation`'s `not_matches` on `metadata.body`) is authored as
`(?i)(—|-)\s*Ada` (an em dash or hyphen, then optional space, then
"Ada", case-insensitive) — a literal `—` escape in the pattern text
(confirmed via hexdump of the corpus's canonical-JSON line: the raw bytes
are `\\u2014`, i.e. one JSON-escaping level around a literal backslash-u-
2014, not a pre-decoded em dash character). `regexp.Compile` on that string
fails outright: `error parsing regexp: invalid escape sequence: \`u``. An
initial Go port let that compile error propagate as a "predicate evaluation
error" violation, flipping the case's overall verdict from the oracle's
`ok: true` to `ok: false` — caught by corpus case `310b247bc84437db`.
**Fixed** (not merely matched — this is a Go-vs-Python language gap, not a
frozen oracle behavior to reproduce): `evaluate.go`'s `translatePyRegex`
rewrites `\uXXXX` to RE2's `\x{XXXX}` before compiling, in the ground-
evaluation path only (`evalScalar`'s `Matches`/`NotMatches`). The Z3
vacuity-classification path never compiles the regex at all (Matches/
NotMatches always lower to a free/soft Bool there — see the `compileScalar`
doc comment in `vacuity.go`), so it was never affected by this gap.

Status: in force.
Retires when: the Go verify evaluator reads regexes with `pyre`, the port of Python's `re` the gate uses, and drops `translatePyRegex`.

**On ordering (not an adjudication, a Go-side bug fixed during porting):**
the oracle appends a command's head to `heads`/`commands` the INSTANT it
visits the "command" node, before walking that node's children — including
its OWN leading assignments. `X=$(date) prog --flag` → oracle
`heads: ["prog", "date"]` (the head first, THEN the nested substitution
found while walking the assignment). An initial Go port that walked
`CallExpr.Assigns` before extracting the head produced `["date", "prog"]` —
caught by corpus case `f06b6ffac9daa60f`, not a parser disagreement at all,
just a traversal-order bug. Fixed in `walkCallExpr`.

## 2026-08-21 — Lean client (`clients/lean/`), independent confirmation of G-4 / G-4b

**L-1 (independent reproduction of G-4, and confirmation that G-4b's
characterized rule is real): the Lean subset parser's entire decompose
false-accept residual (132/13,084 cases, all "superset_heads" — our heads
are an order-preserving supersequence of the oracle's, never missing an
oracle head, only adding real extra ones the oracle silently dropped) is
the same tree-sitter-bash GLR span-fusion bug as G-4 above, reproduced
independently from a Lean recursive-descent parser rather than mvdan.**
Root-caused one case in detail (`03410d0fc920f364`, a 5-statement
multi-pipe transcript command) two ways:

1. Textual bisection: truncating the command to any prefix shorter than
   the full 5 statements makes the fusion disappear entirely (`5: True,
   4: False, 3: False, 2: False, 1: False` for "does any oracle
   `commands` entry contain an embedded literal newline"), confirming the
   fusion depends on content at the true end of the script.
2. **Cross-checked directly against G-4b's characterized rule (this
   file's Go section, above) and it matches exactly.** The script's 5
   statements are `cd` / `echo` / a 3-stage pipe (`flutter analyze | grep
   | head -3`, redirect `2>&1` on stage 1) / `echo` / a 5-stage pipe
   (`flutter test | tr | grep | grep -v | head -10`, redirect `2>&1` on
   stage 1, this is the script's true-last statement). Under G-4b: T =
   the 3-stage pipe (the rightmost ≥3-stage pipeline before the last
   statement), R = the 5-stage pipe (the last statement, ≥2 stages, has a
   redirect). Applying the rule ("for every statement strictly after T
   through R inclusive, drop that statement's first pipe stage's head;
   vanish a single-stage statement in that range entirely") to this
   script: the 4th statement (`echo`, single-stage, strictly between T
   and R) vanishes; R's first stage (`flutter`) is dropped, its other
   four stages' heads survive. Predicted heads: `cd, echo, flutter, grep,
   head, tr, grep, grep, head` (9) — **exactly** the oracle's actual
   expected list for this case. G-4b's rule is confirmed real, not an
   artifact of the Go implementation.

Went further than a single case: bucketed all 132 by the extra head
token(s) our parser reports beyond the oracle's expected list
(`clients/lean/analysis/bucket_fa.py`). Result:
40+ distinct buckets (`echo` ×8, `flutter` ×9, `ls` ×4, `git` ×2, `cd` ×1,
`sed`/`grep`/`find`/`timeout`/`uv`/`cat`/`magick`/… each ×1-3, several
"MANY" buckets of 4-9 simultaneously-fused heads), with no single dominant
token — the signature of a widespread structural artifact, not a
concentrated local bug. Also directly probed and RULED OUT three cheap
alternative hypotheses that would produce the same "extra real head"
signature: `time`/`coproc` as unhandled reserved words, and a bare
fd-prefix (`2>&1 cmd`) parsed as a literal head — the oracle handles all
of these correctly (`time ls | wc` → `('time', 'wc')`; `2>&1 cmd` →
`('cmd',)`), so none of the 132 are those.

**G-4b's rule not implemented in the Lean parser — a deliberate, effort-
calibrated decision, not an oversight.** G-4b is real (confirmed above:
its rule reproduces the oracle's exact 9-head expected list for
`03410d0fc920f364`, the one case checked in detail — not sampled beyond
that), and it is plausible it explains more of the 132 without having
been checked case-by-case, but implementing it is a genuinely different
kind of feature than anything else in `ShellDecompose.lean`: it requires
tracking top-level-statement identity and rightmost-qualifying-T search
across the WHOLE script, true-end-of-script adjacency (any trailing
statement un-triggers it), and newline-vs-`;` separator distinction at
the statement level — none of which the current single-pass recursive-
descent architecture (`parseList`/`parseAndOr`/`parsePipeline`) carries as
first-class state. Implementing it would mean a structural rework, not a
local fix. And the ceiling is modest even if fully implemented: Go's own
residual after shipping G-4b is still ~119/13,084 (multi-window and
`&&`/`||`-interacting fusions, G-4's harder core, are explicitly
unmatched by G-4b) — the same order of magnitude as this client's 132,
meaning the gate (zero false-accepts) stays unmet either way. Chasing it
further trades a large, risky parser rework for a partial, uncertain
reduction rather than a fix — declined; recorded as an honestly-unmet
gate rather than force-fit to zero. See `clients/lean/README.md`'s
scorecard.

Status: retired 2026-08-21 by the L-1 update below.
Retires when: retired (see Status).

---

## 2026-08-21 — RESOLUTIONS (post-tournament fix pass)

**G-4 — FIXED IN THE ORACLE (was fail-open).** The Go and Lean ports both
proved this is not a client bug but a real vulnerability in the oracle's own
decomposition: tree-sitter-bash 0.25.1 fuses a newline-separated statement
boundary into one `command` node (`c1\nd1` → command `c1` arg `d1`) WITHOUT
setting `has_error`, so a later head (`d1`) executes but is never checked
against the allowlist. Fix: `shell_decompose._command_has_bare_newline`
rejects any command node whose span carries a raw newline outside a
multiline-legal child (quote / heredoc / substitution) and outside a `\`
line-continuation — fail-closed on the ambiguous parse. 167 previously
head-dropping decompose cases now correctly reject. The Rust/TS clients (same
grammar, same latent bug) get the identical port; Go/Lean drop their G-4/G-4b
special-casing since the oracle no longer emits the fused acceptance.
Verified by `tests/test_shell_decompose_fusion.py` (6 cases: the fusion
rejects; quoted-newline, line-continuation, heredoc, and command-substitution
forms are untouched).

Status: retired 2026-08 (monthly fold 2c98de9a) by the oracle's fusion repair: `shell_decompose.py` now rewrites a fused parse into its statements and fails closed only where the repair cannot resolve it, and the Go client's `detectG4bFusion` is gone (`clients/go/internal/verify/decompose_test.go`).
Retires when: retired (see Status).

**F-3 — FIXED IN THE ORACLE.** `predicate_z3._compile_scalar`'s `NotEquals`
now branches numeric-vs-string exactly like `Equals`. All four clients
aligned. Pinned by `tests/test_predicate_notequals_numeric.py`.

Status: in force.
Retires when: does not retire.

**F-1 / F-2 — FIXED IN THE ORACLE.** `verify._match_glob` no longer
delegates to `PurePosixPath.match`; it is now a native, left-AND-right-
anchored, `/`-aware matcher (see verify.py for the canonical source): a
pattern must consume the WHOLE normalized path, so a relative pattern can
never match an absolute path by accident (closing the `file_write:
["out.txt"]` admitting `/etc/cron.d/out.txt` scope-escape from F-1), and
`**` recursively spans zero-or-more segments the same way on every Python
version (closing F-2's 3.12-vs-3.13 drift). `clients/fixtures/semantics.json`
regenerated: 7 `path_match` cases flipped, all TIGHTENINGS (nothing that
used to reject now accepts) — `/etc/passwd` vs `[passwd]` and `/etc/cron.d/job`
vs `[job]` (the exact F-1 exploit shape) T→F; `sub/dir/x.py`/`/abs/x.py` vs
`[*.py]` T→F; `/x` and `../x` vs `[./**]` T→F; `a/b/c/x.py` vs
`[a/**/x.py]` F→T (mid-pattern `**` is now recursive on every Python).
Corpus UNCHANGED (only fixture expectations moved — file-scope path
matching wasn't exercised by any existing corpus case in a way that
flipped). Go client re-aligned same day — see below.

Status: in force.
Retires when: does not retire.

## 2026-08-21 — Lean client, L-1 follow-up: re-aligned to the fail-closed oracle, a NEW narrower residual found

**L-1 update.** The RESOLUTIONS entry above shipped G-4's fix
(`_command_has_bare_newline`) and regenerated the corpus (13,387 →
11,450 cases; 13,084 → 11,089 decompose). Re-ran the Lean client against
it: `superset_heads` (both sides `ok=true`, different heads — the OLD
bucket's shape) dropped to 0. **First draft of this entry asserted this
meant the old 132-case bucket became matches; that was wrong and
unverified — corrected here after actually checking.** This client's
decompose logic for the affected shape (a multi-line multi-pipe script)
is unmodified: it always parsed each newline as an ordinary statement
separator and reported the fuller, correct head list — it never
"rejected" these inputs, so there was no rejection for the new oracle to
newly agree with. Spot-checked three commands recognizable from the old
bucket by content, re-fetched fresh from the regenerated corpus's parsed
JSON (not hand-retyped, to rule out transcription error) —
`310f48f3073fdbac` (the same Sundial flutter-test transcript this entry's
original write-up bisected as `03410d0fc920f364`), `5be4aea55225f77b`,
and `73fbafb2360b80a9` — and all three are **still mismatches** against
the current client build: client `ok=true` with the fuller head list,
oracle now `ok=false` (rejects instead of fusing). The mismatch
*mechanism* is unchanged; only its *shape* changed, from
"superset_heads" to "reject_miss." The 132→116 count change reflects
corpus resampling (11,089 vs. 13,084 decompose cases — not a strict
subset relationship), not cases being fixed. Confirmed by re-running
`clients/lean/analysis/classify.py` and
`clients/lean/analysis/false_accept_detail.py`, plus the direct spot
checks above.

**But a new, narrower 116-case false-accept residual appeared — not
predicted by the RESOLUTIONS entry's framing ("Go/Lean drop their
G-4/G-4b special-casing since the oracle no longer emits the fused
acceptance"), because this client never had G-4/G-4b special-casing to
drop.** It always treated each newline as an ordinary statement
separator (correct POSIX/real-bash semantics), so it accepts several
thousand multi-line multi-pipe scripts the old oracle also accepted. The
new `_command_has_bare_newline` detector is **structural and local**
(walks every `command` AST node, rejects if its own span embeds a raw,
un-exempted newline) rather than tied to whether the fusion visibly
changed the reported heads — so it now also fires on inputs where the old
oracle's fusion was *output-invisible* (the fused span happened not to
change which heads got reported), which the old, symptom-level "did the
heads list differ" check could never have revealed. All 116 confirmed to
carry the identical oracle rejection reason string, `"ambiguous shell
(bare newline inside command — parser statement fusion)"` (checked by
calling `shell_decompose.decompose_command` directly on each of the 116
command texts — read-only, no oracle code touched).

Systematic differential probing (20+ synthetic cases against the live
oracle, sweeping pipe-stage count 1–4, redirect presence/position on both
the triggering pipeline and the statement after the newline) found a
two-condition sub-rule: **a pipeline of ≥3 stages whose last stage
carries no redirect of its own, immediately followed across a raw newline
by a statement containing a redirect operator anywhere before its own
natural end, triggers the fusion.** Validated against the real corpus
(not just synthetic probes) by walking each of the 116 flagged cases' real
AST, flattening through nested `pipeline`/`redirected_statement` wrappers
to count true pipe-stage depth and detect a later redirect transitively:
**this sub-rule explains 87 of the 116.** The remaining **29 falsify it as
a complete characterization** — they include cases with a pipe-stage
count of only 1 or 2 and no downstream redirect detected by the
validation. That caveat matters: the validation's redirect-detection
flattens through `pipeline`/`redirected_statement` nesting only, not
through `list` (`;`/`&&`/`||`) nesting, so a redirect sitting past a
top-level separator inside the following statement is invisible to it.
The 29 are evidence of *either* a second, uncharacterized triggering
mechanism *or* an incomplete redirect-detection pass in the validation
itself — undetermined; not chased further either way, since the "don't
implement" conclusion below holds under both readings.

**Not implemented.** A rule validated for only 87/116 would still leave
the false-accept gate unmet, and the missing 29 are evidence the model is
incomplete, not merely imprecise at the margins — retrofitting a
partial, cross-newline-lookahead, nested-wrapper-counting heuristic into
`ShellDecompose.lean`'s single-pass recursive-descent architecture is
real, ongoing-maintenance-cost parser complexity for under one point of
corpus match rate (87/11,089), built on a characterization already known
to be wrong for a quarter of the cases it's meant to explain. Recorded as
an honestly unmet gate: false-accepts = 116 (all
`"ambiguous shell (bare newline inside command — parser statement
fusion)"`), mechanism majority-characterized (87/116) via a validated
two-condition sub-rule, remainder (29/116) not characterized, no
client-side fix attempted. See `clients/lean/README.md`'s scorecard for
the full numbers.

Status: in force (its numbers predate the oracle's fusion repair).
Retires when: the Lean client is measured again against the oracle's fusion repair.

## 2026-08-21 — Go client, re-aligned to the F-1/F-2 file-scope matcher fix

**F-1/F-2 ported.** `clients/go/internal/verify/pathmatch.go` rewritten
(not patched): `matchDoubleStar` + `pathlibMatch` + `parsePurePosixSegments`
deleted outright (the old right-anchored `PurePosixPath.match` port they
implemented no longer has an oracle counterpart to match), replaced by
`matchGlob`, a direct line-for-line translation of the oracle's new
`verify._match_glob` — same `/**`-suffix special case (root/`.`/prefix
branches) and the same recursive `matchFrom(pi, ti)` walk for the general
case (`**` spans zero-or-more segments via a `for k := ti; k <= len(...)`
loop; `*`/`?`/`[...]` stay within one segment via the existing
`fnmatchCase`). `posixSplitRoot`/`posixNormpath` (still needed, unchanged
semantics) and the public `PathMatchesAny(path, globs) bool` entry point
(same signature, same callers in `verify.go`) were kept as-is.

One porting note: the oracle's Python does `glob.split("/")` /
`norm.split("/")` directly with NO empty-component filtering — an absolute
path/pattern's leading `""` segment (from the leading `/`) is a real
element of the list and is exactly what makes an absolute pattern's
segment-count/position naturally fail to align against a relative path's
segments (and vice versa), with no separate "is-absolute" bookkeeping
needed. Go's `strings.Split` has the identical behavior on a leading
separator (`strings.Split("/a/b", "/")` → `["", "a", "b"]`), so the port
needed no adaptation here — worth noting explicitly since the OLD
`pathlibMatch` port carried an explicit `pathIsAbs bool` computed by
`parsePurePosixSegments` (which DID filter empties) precisely to make up
for pathlib's separate absolute/relative code paths; the new algorithm
needs no such flag at all, which is itself evidence the new matcher is
structurally simpler, not just differently anchored.

**Verified:** `clients/fixtures/semantics.json`'s regenerated 29
`path_match` cases (`TestFixturePathMatch`) all pass, including the two
F-1-exploit-shaped tightenings (`/etc/passwd` vs `[passwd]`,
`/etc/cron.d/job` vs `[job]`, both now correctly `false`) and the F-2
mid-pattern-`**`-recursion case (`a/b/c/x.py` vs `[a/**/x.py]`, now
correctly `true`). Full `go test ./...` green. Full corpus gate re-run:
**11,394/11,450 — unchanged** from before this fix (the coordinator's
prediction confirmed: the corpus itself doesn't exercise a case whose
verdict flips under the new matcher; only the fixture's expectations
moved). `go build`/`go vet`/`go mod verify` clean; no files touched
outside `clients/go/` and this file.

Status: in force.
Retires when: does not retire.

---

## Campaign: Lean decompose coverage (2026-08-21)

The Lean client's hand-rolled subset parser was the strictest of the five
(8,882/11,450), refusing ~2,418 commands the oracle accepts — false-rejects, the
safe direction, but the bulk of its disagreement. It was extended one construct
at a time (arithmetic, `${…}`, backticks, subshells, heredocs, the
`if`/`while`/`until`/`for`/`select` compounds, assignment-value substitutions,
the `!` prefix) behind `clients/gate.py`, which classifies every "Lean accepts,
oracle rejects" case and holds the genuinely-unsafe count at **zero**:

- **verified divergence** — Lean parses valid bash the oracle's tree-sitter
  can't, with a COMPLETE head list proven either by an un-fused reparse or by an
  independent parser (Go's `mvdan/sh`) accepting with Lean's exact heads. Safe.
- **fusion-unverified** — a fusion-class divergence where no reference could
  confirm completeness (complex `for`-loop scripts). Reported and spot-checked,
  not blindly trusted.
- **genuine** — Lean accepting a command the oracle rejects on its merits, with
  no corroboration. Gated: the change is blocked until it goes to zero.

Result: decompose 8,555 → **10,770 / 11,089** (97.1%); total **11,097 / 11,450**;
disagreements 2,534 → 353. Theorems stayed `sorry`-free (they range over the head
model, not the production scanner).

### L-2 — over-acceptance of a malformed `for` header (found by the gate)

Extending the `for` parser first accepted `for r $items in; do :; done` — the
`in` misplaced, a genuine shell syntax error tree-sitter (and the oracle) reject.
The lenient header scan treated `$items` and `in` as ordinary header words. The
gate flagged it as a genuine over-acceptance (the oracle rejects it on its
merits, no corroboration). **Resolution:** the header parse now requires `in`, a
separator, or `do` immediately after the loop variable; anything else fails
closed. This is the differential harness running in the OTHER direction — the
strict reference catching the lenient client.

Status: in force.
Retires when: does not retire.

### The heredoc-pipe conformance rule

`cmd <<EOF 2>&1 | tail` (a heredoc that is not the last token of a piped command)
is a tree-sitter parse error the oracle raises — and `mvdan/sh` rejects it too.
Rather than parse past a construct two independent real parsers refuse, Lean
fails closed at the pipe (an `hdTrail` flag), staying aligned with the reference.

Status: in force.
Retires when: the oracle's grammar accepts a heredoc followed by a redirect before a pipe.

## 2026-09-24 — Go gate binary (`clients/go/cmd/daisugi-gate`)

The gate binary is judged on more than the verifier's `(stage, step)`
multiset: reason text, tier and every log line are what an operator reads,
so all of them must match the Python gate. Every entry below was found by
running `clients/gate_compare.py`, or by the local-only decompose check
against the frozen corpus, and is settled.

**GT-1 (client, fixed): decompose refusal reasons used Go `%q`.** The
oracle writes `non-literal command head ('$CMD')` with Python repr; the Go
client wrote `("$CMD")`. Reasons are informative for the verify wire, so
the corpus never saw it, but the gate copies the reason into the violation
detail (`decomposition_refused`). Fixed in the client: `decompose.go`
quotes with `pyjson.Repr`. After the fix, every single-line corpus command
with no heredoc decomposes identically in both clients, reason included
(6,430 of 6,430).

Status: in force.
Retires when: does not retire.

**GT-2 (parser gap, delegated): multi-line, heredoc and herestring
commands split into different command texts.** mvdan/sh and
tree-sitter-bash agree on heads, reads and writes for these, but not on
each simple command's source text (`python3 -` against `python3`; a
herestring kept or dropped). The gate hands that text to the head check,
the interpreter parser, the tier classifier and the pane rule, so the
difference is not informative there. Among the corpus's decompose cases,
832 multi-line and 3 single-line (heredoc, herestring) commands differ
only in text, and 3 multi-line commands differ in accept or refuse. In 2
of those mvdan accepts where tree-sitter refuses, a fail-open direction
under decomposition. Resolution: the gate hands any command holding a newline,
a CR or `<<` to the Python gate, at every place it would decompose (the
verify path, the tier, the pane rule, the floor rule, the OpenCode rule).
A decomposition refused for a reason only mvdan gives (`unsupported shell
construct`) is handed over too.

Status: retired 2026-09-24 by GT-8, whose vetted grammar replaced its newline-and-heredoc rule; GT-8 itself retired 2026-09-25.
Retires when: retired (see Status).

**GT-3 (ruling): the two Z3 stages are decided by inspection.** Every call
the permission stage admits reaches `check_envelope_self_consistency` and
`check_plan_against_envelope`, so "delegate what needs Z3" would have sent
every allow to Python. With no postconditions the solver sees only ground
equalities over `shell`, `file_write` and `max_execution_time_s`:
unsatisfiable exactly when `shell` is false with a non-empty allowlist, or
the time bound is outside (0, 3600]. The gate decides those directly and
writes the oracle's violation (`"detail": {"unsat_core": "[]"}`, checked
against the oracle). Envelopes with invariants, postconditions or robotics
bounds, where Z3 does real work, go to Python.

Status: in force for the two ground checks; its hand-off of Z3 work to Python retired 2026-09-25 by the standalone Go gate, which links Z3.
Retires when: does not retire.

**GT-4 (semantics gap, delegated): Unicode text.** The hard-deny rules and
the tier classifier use Python `re` (`\w`, `\s`, `\b`), `str.isspace`,
`str.split`, `str.strip` and `casefold`, all Unicode-aware. RE2 and Go's
`strings.Fields` are not, and Go's `unicode.IsSpace` leaves out `\x1c` to
`\x1f`, which Python treats as whitespace. Resolution: any command, path,
URL, cwd or MCP argument string the rules read that holds non-ASCII text
or a control character other than tab, newline and CR goes to Python.
Where the port keeps a regex, Python's `\s` is spelled out as
`[\t\n\v\f\r \x1c-\x1f]`; the two lookahead patterns (`_OPENCODE_TEXT`,
`_JQ_ENV`) are rewritten without lookaround.

Status: retired 2026-09-25 by the standalone Go gate: the Unicode rules run on `pyre` and `pystr`.
Retires when: retired (see Status).

**GT-5 (finding, not fixed): the verify client's head extraction splits
on Go whitespace.** `verify.extractShellHead` uses `strings.Fields`, and
several helpers use `strings.TrimSpace`. For a command holding `\x1c` to
`\x1f` the head differs from Python's `split()`. No corpus command holds
one. The gate does not use these for head extraction (it has its own,
with Python's whitespace set). Left for the verify client's owner.

Status: in force (open).
Retires when: the Go verify client splits a head on Python's whitespace set.

**GT-6 (harness, fixed): two false disagreements from the fixture
itself.** A seeded session tree whose last line was torn with no newline
made the next appended line unparseable in a way the normalizer could not
mask; the seed now ends with a newline. And envelope files stored as JSON
objects had their keys sorted by the canonical case form, which changed
the order of a pydantic error message; envelopes are now stored as text.

Status: in force.
Retires when: does not retire.

## 2026-09-24 — Go gate binary, review round 1

A security review ran the gate binary against the oracle and found three
fail-open classes. Each is fixed below and pinned by a gate case, a Go
test, or the seeded fuzz (`clients/gate_compare.py --fuzz N --seed S`).

**GT-7 (client, fixed, was fail-open): extglob hid a command head.**
mvdan parses `@(...)`, `!(...)`, `+(...)`, `*(...)` and `?(...)` as a leaf
(`*syntax.ExtGlob`) and never walks its pattern text, so
`echo @(a|$(rm x)) ; ls` decomposed to `echo` and `ls`. tree-sitter-bash
has no extglob production and refuses the whole command as
`malformed shell (parse error)`. The verify client had the same bug
(`decompose.go` treated ExtGlob as a leaf); it now refuses with the
oracle's reason, and so does the gate.

Status: retired 2026-09-25 by the move of the Go client to tree-sitter-bash, the oracle's own grammar: mvdan/sh is gone (see "Go gate binary, standalone").
Retires when: retired (see Status).

**GT-8 (parser gap, delegated, was fail-open): single-line commands the
two parsers read differently.** The review fuzz found 119 accepted by Go
and refused by the oracle (`[ a '<' b ] && ls`, `{a,b} ; ls`,
`x#y 2>&1`, ...); the gate's own fuzz found three more shapes inside a
first, looser grammar: a numeric head (`5`, `-5`, `0x1`) is a number to
tree-sitter and refused; tree-sitter ends a command's text at its first
redirect (`ls >/work/o x` has text `ls`); and it can drop a whole command
(GT-10). A fuzz of random words inside that grammar, run against a
permissive envelope so that any oracle refusal is a deny, found the rest:
tree-sitter reads a head by context (after an assignment `a:b`, `a%b`,
`a@b`, `a?b` are parse errors; a head holding `=` can become an
assignment; a quoted head is non-literal), it reads a bare redirect
target like `-0` as an fd operation and refuses it, a lone `-` head
before a redirect is no command to it, and mvdan reads `let`, `nameref`
and the `declare` family as clauses with no head while tree-sitter does
the same to `unset`. Resolution: the gate decomposes natively only inside
a small vetted grammar (README, "The vetted shell grammar"): a plain head
of letters, digits, `_ . / -`, no builtin whose head either parser drops,
redirects only after every word, no bare target starting with `-`. Every
other command goes to Python at every place the gate decomposes. This
replaces GT-2's narrower newline-and-heredoc rule. The fuzz ran clean
after each narrowing (sizes in the final report of this round).

Status: retired 2026-09-25 with the vetted grammar, by the standalone Go gate.
Retires when: retired (see Status).

**GT-9 (client, fixed, was fail-open by time): no native deadline.** An
envelope glob with nine `**` segments kept the Go matcher busy for 110 s;
the oracle denies at `--verify-timeout`. The native verifier now runs
under that budget and denies in the oracle's words (`gate internal error
(denied fail-closed): verifier exceeded the gate's inner time budget
(5.0s).`, tier permanent). An envelope with more than two `**` segments
in a file glob is handed to Python outright: the oracle's matcher is
exponential there, and a faster port would allow where the oracle times
out and denies.

Status: in force for the native deadline; its hand-off of globs with more than two `**` segments to Python retired 2026-09-25 by GT-23 (closed).
Retires when: does not retire.

**GT-10 (ORACLE, fixed in 0423900, was fail-open; see round 2): tree-sitter drops a command that
starts with an assignment and a redirect.** `decompose_command("ls |
FOO=1 </work/i rm x")` returns commands `['ls']`, heads `('ls',)`: the
`rm x` command is gone, so its head is never checked. With
`shell_allowlist: ["ls"]`, `shell_allow_decomposition: true` and
`file_read: ["/work/**"]`, `verify()` passes `ls | FOO=1 </work/i rm -rf
/work`. mvdan keeps the command. The gate hands this shape to Python (it
is outside the vetted grammar), so today the gate allows it too. A
failing Python test is kept outside the repo for the controller; a
fail-closed fix would reject a decomposition whose commands do not cover
every command node, or re-derive heads from the node list.

Status: retired 2026-09-24 by the oracle fix 0423900 (GT-10 closed, round 2).
Retires when: retired (see Status).

**GT-11 (ORACLE, fixed in ebd4dcc and ported; found by the review): the floor rule misses a
relative path after `cd ..`.** With `cwd` `~/work`, `cd .. && cp a
.config/coppice/x` writes into `~/.config/coppice` and neither gate
denies it: `_next_cwd` refuses to follow a `cd` whose target holds `..`,
so the next command's relative words are not placed. Its tier is
permanent, but the hard-deny rule does not fire. Not fixed.

Status: retired 2026-09-24 by the oracle fix ebd4dcc (GT-11 closed, round 2).
Retires when: retired (see Status).

**GT-12 (ORACLE, open, reported by the review): a path through a
symlink with `..`.** With `cfg` a symlink to `~/.config` in `~/work`,
neither gate denies `cp a cfg/../cfg/coppice/x`. The review reports it
as a floor write. On re-derivation it may not be one: the kernel
resolves `cfg` to `~/.config`, then `..` to `~`, then looks for `~/cfg`,
which does not exist in the review's layout, so `cp` would fail; the
gate's resolved spelling (`realpath`) is the same `~/cfg/coppice/x`. It
becomes a real gap only when `~/cfg` also exists and points at the
floor. Kept open for the controller to confirm or close.

Status: in force (open).
Retires when: the controller confirms the gap or closes it.

## 2026-09-24 — Go gate binary, review round 2

**GT-10 closed in the oracle.** 0423900 makes `_classify_file_redirect`
refuse a file redirect with more than one destination, reason
`ambiguous shell (a redirect with more than one target '...')`:
tree-sitter files the words after such a redirect as more targets, where
bash reads them as the command's head or arguments.
`ls | FOO=1 </work/i rm x`, `rm </dev/null -rf /work`, `cat <a b` and
`echo hi >out more` are now refused. The frozen corpus holds none of these
shapes: the new oracle still matches all 11,450 cases.

Status: in force.
Retires when: does not retire.

**GT-13 (client, fixed, was looser than the oracle): the Go verify client
mirrors the multi-target refusal.** mvdan reads `cat <a b` correctly (b is
an argument), so the Go client accepted what the oracle now refuses.
Probing the new oracle gives the shape:

- A file or fd redirect (not a heredoc or herestring) is refused when a
  word of the command comes before it and a word comes after it.
- On the right of a pipe, it is also refused when an assignment comes
  before it (`ls | FOO=1 </x rm`).
- A redirect before every word is a prefix and is accepted (`<x cat y`,
  `FOO=1 </x rm y z`).

The quoted text is the redirect through the words that follow it, up to
the next redirect. `decompose.go` now refuses those shapes with the
oracle's reason. The same probing found a second looser shape: a
herestring after a file redirect (`cat 2>/dev/null <<< c`) is a parse
error to tree-sitter. It is refused too.

Conformance against the corpus is unchanged: 11,447 of 11,450 (the three
multi-line residuals). The gate was never looser here, because its vetted
grammar already takes a redirect only after every word and hands a
herestring to Python.

Status: retired 2026-09-25 by the move of the Go client to tree-sitter-bash, the oracle's own grammar: mvdan/sh is gone (see "Go gate binary, standalone").
Retires when: retired (see Status).

**GT-11 closed in the oracle, and ported.** ebd4dcc changes the floor
and OpenCode rules. A relative word now counts as a hit when both of
these hold:

- the cwd is unknown, because the gate cannot follow a `cd` or the call
  has no cwd;
- the word names a guarded directory by its last part (`coppice`,
  `opencode`).

The Go gate had the old rule, so it would have allowed
`cd .. && cp a .config/coppice/x` where the oracle now refuses it. The
same check is ported for words and redirects (`namesRootPart` in
`rules.go`), with gate cases for both rules.

Status: in force.
Retires when: does not retire.

## 2026-09-24 — Go gate binary, review round 3

**GT-14 (client, fixed, was looser than the oracle): fd words starting
with 0.** tree-sitter reads `00` or `01` before a redirect as a parse
error, and it reads a lone `0` there as an argument, which gives the
redirect more than one target. Either way the oracle refuses. The
grammar took any all-digit word as an fd, so the Go gate allowed
`ls 01> /work/b` and `cat < /work/a 0< /etc/shadow` where the oracle
denies: 228 of the re-review's 790-command redirect enumeration. An fd
starting with 0 is now outside the grammar. `10>`, `123>` and dup targets
such as `2>&0` and `2>&01` still agree. The enumeration is committed as
`clients/fixtures/gate/redirects.txt` and runs as gate cases under an
envelope that allows every head and path.

Status: retired 2026-09-25 with the vetted grammar, by the standalone Go gate. The redirect enumeration stays as gate cases.
Retires when: retired (see Status).

**GT-15 (shared, fixed in the oracle by 84d1999 and ported): the floor
rule was quadratic in a line's length.** `_shell_hit` appended the cwd
once per command even when it had not changed, so every redirect was
placed against every command's copy. A line of 400 redirects took 257 s,
long enough to pass the host's hook timeout, which fails open. The
oracle now keeps each distinct cwd once, and the Go rule does the same.
The Go gate also runs the three hard-deny rules under the
`--verify-timeout` budget and denies when it runs out. That is stricter
than the oracle, which has no deadline there.

Status: in force.
Retires when: does not retire.

**GT-16 (ORACLE, fixed in 50684bb, see GT-18): the tier has the same quadratic shape.**
`effects._shell_class` still appends the cwd once per command. A
400-segment line that writes outside the envelope costs the oracle 8.3 s
of tier work after the verdict is known. Because the cost is quadratic,
a line some 2.5 times as long would pass the installed 30 s hook timeout
before the gate answers, and that timeout fails open. The fix has no
visible effect on output, since a repeated cwd only repeats identical
checks and identical classes. Keep each cwd once, as 84d1999 does for
the floor rule. The Go port already does, so the Go gate answers such a
line in well under a second.

Status: retired 2026-09-24 by the oracle fix 50684bb (GT-16 closed, below).
Retires when: retired (see Status).

**Ported without a finding: fb7acaa and b1ebcab.** The state report now
carries the transcript path (an absolute string only), the verdict
`{decision, tool, clause}` and the mode (`enforcing` or `watching`). The
Go gate always runs in the hook's own process, so like the in-process
oracle it names the transcript and the session. It never takes the
resident-server path, which omits both.

Status: in force.
Retires when: does not retire.

**GT-17 (harness, fixed): five fail-opens that did not reproduce.** In one
34,000-command fuzz run, 5 native allows (3 with seed 11, 2 with seed 23)
met an oracle deny. That run shared the box with the corpus sweep and the
re-review harness. Neither seed reproduced alone, under the same load, or
in a later loaded run of the whole set, which was clean. The FUZZ lines
had not been kept, so the oracle's reasons were lost. The likely cause
is timing. The oracle's first verified call in a fresh process imports
z3 and networkx inside its --verify-timeout worker, and it runs 25 times
slower than later calls (0.26 s against 0.01 s unloaded). On a loaded
box that can exceed the budget and deny. The fuzz now warms its oracle
process with one verified call before the first comparison, and every
FUZZ line prints the oracle's reason.

Status: in force.
Retires when: does not retire.

**GT-16 closed in the oracle, and GT-18 ported: long lines of cds.**
50684bb makes the tier keep each distinct cwd once, which closes GT-16.
It also bounds the work in two ways. First, past `_MAX_CWDS = 32`
distinct directories, the floor rule and the tier stop following cd:
- the floor rule's cwd becomes unknown, so a relative word is then judged
  by the names-a-guarded-directory rule;
- the tier counts the line as lost, so its relative words are unknown.

Second, the floor rule works its guard roots out once per check. A
re-review measured the Go gate at 456 s on 1,000 `cd` segments, since
it followed every one of them. The Go rules and tier now carry the same
cap and the same once-per-check roots. Gate cases pin 400 and 1,000
distinct cds, and a floor word after 33 cds and after 31. The Go gate
also keeps each realpath answer for the length of one call. A long line
asks for the same few paths from each cwd, and the oracle assumes too
that the file system holds still between its own repeated lookups.

On one later case run, with the box at load 8 to 9 from other sessions'
coppice servers and a Rust-gate fuzz, one oracle call ran past its own
5 s `--verify-timeout` on a plain command. That call reported as a stale
fixture. One coppice report also missed its 0.2 s socket budget, and that
reported as a record difference. Both cases passed when run again. The
oracle's budgets are wall-clock, so on a starved box they decide
differently from run to run. A comparison run that shows such a
difference must be repeated before it is read as a finding. Case and fuzz
runs can now be kept apart with `DAISUGI_GATE_SCRATCH`. Two runs that
share one scratch directory empty each other's work directories, which
may also explain the five unreproduced fuzz differences above.

Status: in force.
Retires when: does not retire.

## 2026-09-24: pane_rule read gap, the CA key, TLS keys, and the voice token

**Ported, plus one self-found gap closed in both clients.** The web door
caught a read of the phone's bearer token (`web/token`) but not the
other secrets a coppice data directory holds: the local CA's own key and
the leaf key it signs (`web/ca/ca.key`, `web/ca/leaf.key`), a tailscale
certificate's key (`web/tls/tailscale.key`), and the voice server's
bearer token (`voice/token`). A pane that read the CA key could mint a
certificate the owner's phone already trusts, not just answer an ask.

`pane_rule._SECRET_FILE` (was `_WEB_TOKEN_FILE`) now matches all five,
each by the same directory and file names regardless of where the data
directory itself lives, the same way the web token was already matched:
no new mechanism, just a broadening of the existing substring check plus
`web_door`'s doc comment. Ported line for line to the Go client's
`secretFile` (was `webTokenFile`) in `clients/go/internal/gate/rules.go`.

**Self-found gap (fixed before this landed, not a separate finding):**
`web_door` is a plain substring match on the text as typed. For a shell
line that is enough, since the floor rule tracks `cd` and normalizes
each word anyway. It is not enough for a Read tool's path or an MCP
argument: `/coppice/web//ca/ca.key`, a `.` segment, or a bogus segment a
`..` undoes all read straight through, allowed by the envelope, denied
by nothing. `path_names_secret` (`pathNamesSecret` in Go) closes it: it
runs the same normalize-then-resolve pass `path_touches_asks` already
runs for the gate's own ask files, on a file read's path and on every
MCP argument string, before falling back to the plain substring check.
Confirmed with a failing test first (`web//ca/ca.key` read as allowed by
the envelope, not denied by anyone) then fixed.

A public certificate (`ca.crt`, `leaf.crt`, `tailscale.crt`), `meta.json`,
and `web.json` (which holds a path to a token file, not a token) are
deliberately left out and fall through to the envelope. A Read of one of
these now asserts `allow is True` (`Exit == 0` in Go), not merely "not
the pane refusal": a shell `cat` of the same file is still denied, by the
floor rule (the whole coppice data directory is the floor's own), and a
test that only checked the refusal text would not have told the two
apart.

Eighteen new `gate_cases.py` cases: the four new secrets by Read, shell
`cat`, a redirect, `cp` as source, from a neutral cwd and from inside the
data directory, an MCP argument, four respelled paths (a doubled slash,
a `.` segment, a `..`-undone segment, one each for Read and one for MCP),
and four public/config negative controls. Regenerated corpus: 1,130
cases (was 1,112), 0 disagreements against the Go binary with `--oracle`,
0 stale fixtures.

**Known, accepted gaps, reported rather than fixed here.** A `--ca-dir`
or `--voice-token-file` override that moves a secret outside its default
`web/...` or `voice/...` shape is not caught, the same way a program
that builds a path at run time is not caught. A shell line's own path
respelling (`cat .../web//ca/ca.key`) is backstopped by the floor rule's
`cd` tracking, with the floor's own refusal text rather than the pane
rule's, but only under the floor's default roots: with a custom
`--data-dir`, neither rule catches it. The coppice server's own pane
check is the first door for all three.

Status: in force.
Retires when: a moved secret (`--ca-dir`, `--voice-token-file`) is guarded; until then the accepted gaps stand.

**Reported, not fixed: Grep on the secret's own directory reads it in
full.** `Grep` is a `file_read` whose path is a search root, not a file,
so `{"pattern": ".", "path": "<data>/web/ca"}` (or the same with `cwd`
set there and no `path` at all) prints `ca.key` in full, and the same
holds for `<data>/voice` and the voice token. `_SECRET_FILE` only matches
a path ending in the secret's own file name, never a bare containing
directory, and `floor_config._hit` has no `file_read` branch at all
(its own doc comment claims it denies a read there; the code does not).
The old `web/token` check had the identical hole. A fix would need a
"does this directory hold a secret" check, which is a new kind of
question for this module, not a broadening of the one it already asks,
and it would also catch a `Grep` over any unrelated project's own
`web/` or `voice/` directory. Left for the orchestrator to weigh.

Status: retired 2026-09-24 by "Narrowed: a search of a secret's directory" (below).
Retires when: retired (see Status).

## 2026-09-24: pane_rule lets `coppice agent deny` through

**Ported, a rule change, not a finding.** The pane rule refused every
spelling of `coppice agent allow` and `coppice agent deny`, on the CLI and
on the wire. But the foreman page tells a task foreman to deny the asks it
holds, and coppice-server already lets a pane deny only an ask it holds
now (`foremanDeny`). So the gate refused the one deny the server would
take. A deny is not an allow, so the pane rule now finds only an allow:
`_ALLOW_TEXT`, `_WIRE_TEXT` and `_is_allow_words` in `pane_rule.py`, and
`allowText`, `wireText` and `isAllowWords` in
`clients/go/internal/gate/rules.go`, match `allow` alone. A deny falls to
the envelope, and then to the server.

Cases: `pane coppice deny opts` became `pane coppice allow opts` (still
the pane refusal), and two new cases, `pane coppice deny passes` and
`pane wire deny passes`, show a deny reaching the envelope. Regenerated
corpus: 1,132 cases.

Status: in force.
Retires when: does not retire.

## 2026-09-24: a secret's directory, a custom data dir, and a deny from outside the pane

**Narrowed: a search of a secret's directory (was fail-open).** The gap
reported above. The pane rule now asks a second question of a file read:
is its path a directory that holds a coppice secret? That set is each
data directory itself and its `web`, `web/ca`, `web/tls` and `voice`
directories, as typed, normalized and resolved (`path_holds_secret`,
`pathHoldsSecret`). It is anchored at known data directories, so an
unrelated project's own `web/` is not caught. The record cannot say
whether a search named a path (for `Grep` it falls back to `pattern`), so
a search tool is checked on its cwd too. Per tool name: `Glob` gets no
directory check, since it prints names and never contents and a secret's
name is no secret. `Read` and pi's `read` open the one file their path
names, so only the path is checked, not the cwd. `Grep`, `search`, and
any other `file_read` name are checked on path and cwd.

Status: in force.
Retires when: does not retire.

**Decided: no `file_read` branch in the floor rule.** coppice.toml names
harnesses, plugins, projects and the gateway; web.json holds paths and
the name of an env variable. Neither holds a secret, so a read of either
is harmless and left to the envelope. The secrets and their directories
are the pane rule's. An operator can put any value in a `[plugin.<id>]`
table; such a value is not guarded. The module and `_hit` doc comments
now say this plainly instead of implying reads are denied.

Status: in force.
Retires when: `coppice.toml` or `web.json` starts to hold a secret.

**Narrowed: a custom `--data-dir` (was fail-open for a shell respelling and
a directory search).** coppice now sets `COPPICE_DATA_DIR` on every pty
and headless pane (`pane.BuildEnv`), after the pane's own env, and
removes an inherited value when it has none. The server makes a relative
`--data-dir` absolute once. The gate adds that directory to the floor
roots, so the floor rule's cd tracking catches a respelled shell path
there with the floor refusal, and to the pane rule's secret directories.
It only adds a directory; the defaults stay. A relative value is
ignored in Python and delegated by Go, as a relative XDG home is. The
resident gate's own env is not the pane's, so `gate_client`, the pi
extension and the OpenCode plugin send the value on each request as
`coppice_data_dir`, and `resident_call` carries it. The OpenCode plugin's
pinned hash in coppice followed.

Why a pane cannot lower it: the hook runs in the harness's env, which the
agent cannot change mid-session. **Accepted limit:** an agent that writes
an `env` entry into a harness settings file could change the value for a
later session. That can only drop the custom directory, never the
defaults. Cost: the floor's substring and last-part checks now also
match the custom directory's path and last part, so a short or common
last part denies more than the default does.

Status: in force.
Retires when: does not retire.

**A search rooted above a data directory: closed below, in part.** See
the next entry.

Status: retired 2026-09-24 into the next entry, "a recursive search rooted above a data directory".
Retires when: retired (see Status).

**Narrowed: a deny from outside the pane (was fail-open, reported by the
coordinator).** coppice-server lets a pane deny only an ask it holds, but
it knows a pane only by the caller's process tree and session. A deny
sent through `--remote`, another `--socket`, a changed
`XDG_RUNTIME_DIR` or `HOME`, or under ssh, setsid, systemd-run, nohup,
at, batch, tmux, screen, script, sudo and the like reads as the
operator's and could refuse any ask. `shell_denies_from_outside`
(`shellDeniesFromOutside`) refuses a line that holds a deny, in the CLI
or wire form, and any of these. A wrapper word counts at the head of a
command, after a quote, or after a prefix such as env; ssh, setsid,
systemd-run, nohup, tmux, daemonize, start-stop-daemon and
`start_new_session` count anywhere. A plain `coppice agent deny`, with or
without `--reason`, still passes. Over-deny: a reason whose text starts
with a wrapper word, such as `--reason 'at noon'`, is refused.
**Accepted over-deny:** the coppice CLI dials its default socket and
does not read `COPPICE_SOCK`, so a foreman under a server started with
its own `--socket` can deny only with `--socket`, which is now refused.
Such a foreman cannot deny through the gate; the operator must. Codex
and sprig start the gate with the harness's own env (sprig sets no env
of its own; Codex runs `hooks.json` commands in its env, not checked in
Codex's source), so `COPPICE_DATA_DIR` reaches their hooks. The pane
rule's `mcp` branch does not run the shell checks, so an MCP shell tool
that carries an allow or a wrapped deny is left to the envelope's
deny-by-default `mcp_allowlist`; that predates this entry.
**Still open:** a per-call text rule cannot see a deny that a script
file sends, or one that reaches the server through a relay an earlier
call set up (a socat or `ssh -L` forward placed where a later plain call
dials). The server's peer check is the only door there.

Cases: 31 new (13 deny wrapper and pass cases, 10 Grep, 2 Glob, 1 Read,
5 with `COPPICE_DATA_DIR` set, unset, or relative); `pane coppice deny
passes` is now the plain form, and its old `--socket` form is `pane deny
through a socket`, now the pane refusal. Regenerated corpus: 1,163 cases,
0 disagreements against the Go binary with `--oracle`, 0 stale fixtures.

Status: in force.
Retires when: the coppice server's peer check covers a deny sent by a script file or through a relay; the per-call rule stays.

## 2026-09-24: a recursive search rooted above a data directory

**Narrowed (was fail-open), see round 1 below.** A `Grep` of `~`, `/`, `~/.opendaisugi`, or
the parent of a custom data dir reads every secret under it.
`search_above_secret_hit` (`searchAboveSecretHit`) refuses a file read
other than Glob and Read when its path is an ancestor of a data
directory, typed, normalized or resolved, with its own refusal: "this
search reaches coppice's secrets. Search a narrower directory." The cwd
counts only when the call names no `file_path`, `path` or `filePath`, so
a Grep of `/work/src` from `$HOME` still passes; Go reads this from a new
`record.HasPath`, set from the same keys. It runs after the pane and
floor rules, so their refusals win where both fire.

A shell line gets the same check, cheaply, off the decomposition: for
each simple command (after `VAR=` words) that is rg, ag, ack, rgrep, grep
or egrep or fgrep with `-r`/`-R` (also inside a short cluster), or find
with `-exec`, `-execdir`, `-ok` or `-okdir`, each non-option word is
placed from the cwd the line has reached, with each `cd` followed. A
search that names at most one such word is placed at its cwd too, so
`rg token` from `$HOME` is refused. Find's roots are its words before
the first expression word, `.` by default.

Over-deny: an option's value word (`rg -g x`) is placed as a path, and a
lone pattern of `/` or `~` counts as a root. **Left to the envelope:**
any other tree reader (`cp -r`, `tar`, `zip -r`, `find ... | xargs
cat`), a search behind `sh -c` or a wrapper that decomposition does not
open, a relative root after a `cd` the gate cannot follow, an MCP tool's
search, and a call with no cwd and no absolute path.

Cases: 21 new (`search ...`), with narrow, names-only and one-file
controls. Regenerated corpus: 1,184 cases, 0 disagreements against the
Go binary with `--oracle`, 0 stale fixtures.

Status: in force, widened by review rounds 1 and 2.
Retires when: does not retire.

## 2026-09-24: the search rule, review round 1

The rule moved to `src/opendaisugi/search_rule.py` (Go:
`clients/go/internal/gate/search.go`). A security review found fail-opens
inside its own scope. Each is fixed in both gates, with cases:

- **F1, value words.** A word after an option that may take a value
  (`-m 1`, `-t py`, `-g '*'`) is still placed and checked, but it no
  longer counts as a root. Options known to take none are listed per
  program (grep, rg, others); any other option is taken to take a value,
  so a misread only adds the cwd check. The first sure word is the
  pattern unless `-e`, `-f`, `--regexp`, `--file` or `--files` gives it.
  Every word, the pattern included, is checked for placement, so a root
  misread as the pattern is still seen.
- **F2.** A line the parser or the word splitter cannot read is refused
  when it names one of the search programs.
- **F3.** grep recurses on any word with `recurse`, any prefix of
  `--recursive` or `--dereference-recursive`, `-NUM` and `--depth`
  (ugrep as grep). ugrep, ug and git grep search the cwd unasked; zgrep
  joins the grep family.
- **F4.** A root with `*`, `?`, `[...]` or a `{a,b}` brace is expanded
  (braces up to 64 alternatives, more is refused) and matched part by
  part, with the gate's own matcher in both gates, against every prefix
  of every secret file, from `/` down. Dot files match `*` here, unlike
  the shell, so this over-denies `~/*`. A shell word whose glob can match
  a secret file itself is refused by the pane rule, for any program
  (`cat ~/.opendaisugi/*/*/*`).
- **F5.** find's `--`, `-O<n>` and `-D <word>` are skipped with `-H`,
  `-L` and `-P`.
- **F6.** A relative root, or the cwd, is refused once the cwd is
  unknown: a cd the gate cannot follow, a call with no absolute cwd, or
  a Grep with no path and no absolute cwd.
- **F7.** A root with `$` before a name, `{`, `(` or a special
  parameter, a backtick, or a `~` form the gate does not expand is
  refused. A `$` at the end of a word is a regex anchor and passes.
  xargs before a search makes its roots unknown, so it is refused.
- **F8.** Every path a file read names (`file_path`, `path`,
  `filePath`) is checked, by this rule and by the pane rule's directory
  check. A path that is not a string is refused.

The program counts as the first word, after `VAR=` words, or anywhere
after a program that runs the next command (env, sudo, timeout, nice,
xargs and similar), with a leading backslash dropped. A word that holds a
whole shell line with a search program in it, as for `sh -c`, is checked
as a line, two levels deep. `rg --files ~` stays refused.

Status: in force.
Retires when: does not retire.

**Left to the envelope, not closed:** any other program that reads a
tree (`cp -r`, `tar`, `zip -r`, `fd -x`, `find ... | xargs cat`,
`find -fprint`); a search program that is not first and not after a
listed prefix (`sudo -u bob` works, `busybox` does, a shell function
does not); a root that a symlink made by an earlier call points above a
data directory (`ln -s ~ h && rg -L token h`); an MCP tool's search (pi's
grep and find, filesystem servers), which the envelope's deny-by-default
`mcp_allowlist` gates; a search a script file runs. Over-deny: a pattern
of `/`, `~`, or a glob that matches from `/`, and a `.*` pattern from the
home directory.

Cases: 66 new (`search r1 ...`), each finding with its controls.
Regenerated corpus: 1,250 cases, 0 disagreements against the Go binary
with `--oracle`, 0 stale fixtures; no earlier case changed its verdict.

Status: in force.
Retires when: a rule covers these tree readers, or the owner leaves them to the envelope for good.

## 2026-09-24: the search rule, review round 2

Two fail-opens the coordinator's re-check found, both closed in both
gates with cases:

- A bash sequence brace in a root (`/home/use{r..r}`) was not expanded.
  `{a..e}`, `{1..9}` and a `..step` form now expand like bash. A
  zero-padded or mixed sequence, or any brace past 64 words, is not
  expanded: it counts as reaching a secret unless the plain absolute
  path before the brace cannot lead to one (a word with no `/` is
  judged by its cwd). So `rg token {…65 words…,/home/user}` is refused
  by the search rule itself, and `echo {1..100}` or `rg token
  /work/f{1..100}` is left to the envelope.
- A line nested deeper than the two `sh -c` levels the gate follows was
  passed. Past that depth, a word that still names a search program is
  refused.

Order: when the search rule refuses a line, the pane rule's glob check
leaves it, so the agent sees the search refusal, which names the fix,
not the pane refusal. A glob that names a secret file in a non-search
command (`cat ~/.opendaisugi/coppice/web/toke{m..o}`) is still the pane
rule's.

Cases: 14 new (`search r2 ...`). Regenerated corpus: 1,264 cases, 0
disagreements against the Go binary with `--oracle`, 0 stale fixtures.

Status: in force.
Retires when: does not retire.

## 2026-09-24: the Go `daisugi` CLI (gate commands and install --gate)

Cases: `clients/fixtures/cli/` (234, written by `clients/cli_cases.py`
from the Python CLI). `clients/cli_compare.py` runs the Go binary on each
in a fresh scratch HOME and compares exit code, output and the tree
after. Rulings and differences, each checked against the oracle:

**C-1. `install --gate` writes the gate layer only.** Python's
`install --gate` also writes the skill, MCP, capture and instruction
layers. The Go binary is standalone and carries none of those, so it
writes the gate layer alone and says so. The oracle for these cases is
the Python CLI with `DEFAULT_LAYERS` emptied; for `install --gate
--uninstall` it is Python's reverse with every non-gate step a no-op.
Plain `install` and `install --uninstall` without `--gate` are not in
the binary. One gap in that oracle: `CodexRuntime.reverse` strips the
MCP block from `config.toml` inline, which the shim does not switch off;
no case holds a `config.toml`.

Status: in force.
Retires when: the binaries carry the skill, MCP, capture and instruction layers.

**C-2. The hook command runs the Go binary.** Python writes
`<python> -m opendaisugi.gate_client --mode ... `; Go writes
`DAISUGI_GATE_HOOK=opendaisugi.gate '<daisugi>' gate check --mode ...`
with the same flags, timeout, matcher and `|| exit 2`. Both CLIs find a
gate hook by its words (fix round 1, below): Python's
`config.gate_hook_args` accepts the assignment as the Go entry point, so
Python reports, removes and refuses to duplicate a Go hook (cases `status hook go form`, `uninstall gate go
form`, `install gate claude go form`). One difference follows: Python
rewrites an old `-m opendaisugi.gate` hook in place to `gate_client`
when that is its only difference; the Go command never equals that
rewrite, so Go warns and leaves the old hook. Not looser: the old hook
still gates. The compare replaces either entry point with `{GATE}`.

Status: in force.
Retires when: does not retire.

**C-3. `gate check` is the hook contract.** `daisugi gate check` passes
its arguments to `internal/gate` unchanged: the contract of `python -m
opendaisugi.gate`, which is what a hook runs. Python's typer `gate
check` differs (a default `--mode shadow`, no `--session`,
`--captures-root` or `--ask`); nothing installs a hook that calls it.

Status: in force.
Retires when: does not retire.

**C-4. Oracle findings, fixed in both.** Each is fixed in Python, with
its test in `tests/test_gate_hook_words.py`, and in Go:
- `gate report` raised `AttributeError` on a shadow-log line that is
  JSON but not an object (`[1, 2]`). Both now skip such a line.
- `gate init --session a/b` checked `envelopes/a/b.json` for an existing
  envelope but wrote `envelopes/a_b.json`, so it overwrote a registered
  `a_b` envelope without `--force`. Both now check the file they write.
- When a runtime's apply raised (no `~/.claude` with `--runtime claude`,
  or `"hooks": []` in settings.json), `install()` kept the failure in a
  summary the CLI never printed, and the user read "All runtimes were
  already configured, nothing changed." with exit 0. Both now print
  `Failed: <runtime>: <why>. Nothing was written for it.` on stderr and
  exit 1. The reasons are each language's own, so stderr is not compared
  there.

Status: in force.
Retires when: does not retire.

**C-5. No distillation question.** Python's `install` without `--yes`
asks once whether to distil in the background and writes `auto_tend`.
Distillation is not in the binary, so Go never asks. The cases seed
`auto_tend: false`, where Python does not ask either.

Status: in force.
Retires when: the binaries' `install` asks the distillation question (the garden is in both binaries since 2026-09-26).

**C-6. No decomposition warning.** `gate init
--allow-shell-decomposition` warns when tree-sitter-bash is missing.
The binary's gate decomposes shell natively, so it never warns; the
oracle has tree-sitter installed and does not warn either.

Status: in force.
Retires when: does not retire.

**C-7. What is refused, not answered.** A refused run exits 2, says
why, and writes nothing. Three cases are refused today, all
`config.yaml` shapes outside the YAML reader: an unclosed flow sequence
(a YAML error, which Python reads as shadow), a tab, an anchor. Also
refused, with no case yet: tags, block scalars, several documents,
quoted scalars over several lines; an uninstall of a settings shape Python's reverse would raise on (its
exception text is printed and cannot be matched).

Status: retired 2026-10-07 by YM-1: both ports read these shapes as
PyYAML does (the uninstall refusal stays, under IN-1).
Retires when: retired (see Status).

**C-8. Fix round 1 (review of the Go CLI).**
- *Hooks by their words.* Python found a gate hook by the substring
  `opendaisugi.gate` and its mode by a regex over the whole command, so
  a foreign `opendaisugi.gateway-watch` hook stopped install, gave
  `gate status` a mode and was removed by uninstall, and a `--mode`
  inside a quoted path was read as the hook's. Both CLIs now split the
  command as the shell does and accept only the two entry points; the
  mode is the last `--mode` word after it. `daisugi hook record` is
  matched the same way. Cases: `status foreign hook`, `status quoted
  mode`, `install gate foreign hook`, `install gate codex foreign hook`,
  `uninstall gate foreign hooks`. With it two refusals are gone (a
  settings file with a gate hook and a numeric command, where Python's
  result hung on set order; non-ASCII next to `--mode`): a command that
  is not a string is simply not a gate hook, in both.
- *`install --ask` is not in the binary.* At the time its hook sent
  every call to the Python gate, which carried the operator hand-off.
  Since the native gate merged, `gate check` hands nothing to Python and
  answers `--ask` itself; `install --ask` is still refused until it is
  ported. The binary refuses it with its one line; the cases `install gate ask` and
  `install gate ask shadow` are marked as not ported.
- *Plan, prompt, plan again.* The binary decides every change before it
  prints the plan (to refuse early) and again after the prompt, so a
  file changed while the prompt waited is not overwritten from a stale
  read.
- *Atomic writes.* Every file is written to a temporary file in its
  directory and renamed over the old one; an existing file keeps its
  mode, an envelope is 0600 from the moment it exists, and a backup has
  its final mode before any content is in it. A `settings.json` (or an
  envelope, or the marker) that is a symlink is written through, as
  Python's `write_text` does, and the binary says so on stderr (`note:
  <path> is a symlink; wrote <file>.`); the compare drops that note. A
  harness extension file that is a symlink is replaced, as Python
  unlinks it first.
- *The invoked path.* The hook names the binary by the path it was run
  by, symlinks kept, so an upgrade behind `~/.local/bin/daisugi` leaves
  it working.
- *Harness text.* `install --harness` no longer points at `daisugi
  start`, which the binary does not carry: it says the extension asks
  the resident gate, which the binary does not serve yet. The compare
  drops that line on both sides.

Status: in force; its "harness text" item retired 2026-09-27 by G2-4: both binaries' `install --harness` now name `daisugi start` (57a937dc for Rust).
Retires when: `install --ask` is ported, the last refusal here.

**C-9. Fix round 2.**
- *Hand-written hooks.* Matching by words missed gate hooks the old
  substring caught: `uv run [opts] python -m ...`, `NAME=val` and `env`
  prefixes, python flags before `-m`, `-mopendaisugi.gate`, `sh -c
  '...'`, and `--mode enforce||exit 2`. Status then said shadow while an
  enforce hook ran. Both CLIs now read those forms (the rule is in
  `clients/go/README.md`), require a `python*|py` word before `-m` and a
  `daisugi` word before `gate check`, and call anything else holding the
  gate module an unknown gate hook: status mode `unknown`, install treats
  it as installed, uninstall leaves the runtime untouched, names the hook
  in "Failures (left untouched)" and exits 1. Cases: `status written *`,
  `uninstall written *`, `status unknown beside shadow`, `install gate
  beside unknown`. Existing cases that stood in `x -m` or `y -m` for a
  Python hook now use `python3 -m`.
- *A runtime that will fail.* The binary plans before it prints, so its
  "Detected runtimes" list marks a runtime it will fail on (`! Claude
  Code: <why>`) instead of ticking it. Python can only tick it; the
  compare reads both as the runtime's name.
- *Umask and backups.* The umask is read from `/proc/self/status`,
  never by setting it (except where `/proc` is missing). A backup that
  could not be written whole is removed.

Status: in force.
Retires when: does not retire.


## 2026-09-25 — Go gate binary, standalone (no Python at run time)

The binary no longer hands any call to Python. It parses shell with
tree-sitter-bash (the oracle's grammar, built from the same source
archive) and links Z3 5.1.0 statically. A call it cannot decide is denied
with exit 2 and a reason ("the Go gate cannot decide this call (...)").
GT-2, GT-4 and the vetted-grammar rule are retired: mvdan/sh is gone, and
the Unicode rules run on ports of Python's `re` (pyre) and `str`
(pystr). The entries below are what the move found.

**GT-19 (port, fixed): the scanner reads LC_CTYPE.** tree-sitter-bash's
external scanner splits words with `iswspace` and `iswalpha`. CPython sets
LC_CTYPE from the environment at startup and coerces the C locale to
C.UTF-8 (PEP 538), so under the oracle U+2029 after a non-ASCII letter
ends a word (`rm > -中 r` has two redirect targets and is refused).
The Go process ran in the C locale and read one word. The binary now sets
LC_CTYPE as CPython does before it parses, and exports the coerced value
to what it starts, as CPython does. Found by the non-ASCII fuzz.

Status: in force.
Retires when: does not retire.

**GT-20 (port, fixed): a lone surrogate on stderr.** Python's stderr
writes a lone surrogate as `\ud800` (backslashreplace). The binary wrote
the raw bytes, and its child-to-parent frame turned them into U+FFFD. The
frame now carries bytes and the deny line is escaped as Python writes it.

Status: in force.
Retires when: does not retire.

**GT-21 (ruling): depth parity.** The oracle denies very deep inputs
through RecursionError (a decomposer chain of about 990 links, a symlink
chain resolved past the limit, a payload nested past 9,994 containers).
The binary counts the oracle's Python frames on every path that reaches
`decompose_command`, `realpath` and `json.loads`, and raises at the same
depth. A probe (`sitecustomize`) logs the oracle's frame depth at each
such call; over every native case the binary's depths match. Threshold
scans over chains, pipes, nested substitutions and symlink chains near
the limit show no mismatch.

Status: in force.
Retires when: does not retire.

**GT-22 (ruling): crash backstop.** Each call is decided in a child
process of the binary. A fatal error, a signal, a stack past 256 MB or a
child past 25 s becomes the format's deny contract, never an allow.
Tested with a 10,000-link command and a 30,000-link symlink chain.

Status: in force.
Retires when: does not retire.

**GT-23 (known limit): the glob matcher's step budget.** The oracle's
file-scope matcher is exponential in `**` segments and denies when it
runs past `--verify-timeout`. The binary counts matcher steps and denies
at 1.2 million steps per second of budget, calibrated on the reference
box (Python makes about 1.6 million a second there). On a box slower than
that, Python times out after fewer steps than the budget, so the binary
can allow where the oracle denies. This is the one known exception to
"never allow where Python denies", and only for globs with three or more
`**` segments on a slow box.

Status: retired 2026-09-25 by GT-23 (closed): a step limit, not a time budget.
Retires when: retired (see Status).

**GT-24 (known limit, denied undecided): a predicate regex that makes
Python warn.** `re` prints a FutureWarning (a nested set, `[[`) to stderr
with the oracle's own install path and source line. The binary cannot
know that path, so such an envelope is denied undecided. `llm_check`
predicates, which call a model, are denied undecided too.

Status: retired 2026-09-25 by GT-29 and GT-31 (GT-24 closed).
Retires when: retired (see Status).

**GT-25 (ruling): config.yaml.** With no `--mode`, and for
`verifier_client`, the binary reads config.yaml as PyYAML's `safe_load`
reads a plain subset (block mappings, one nested level, one-line scalars,
YAML 1.1 resolution: `on` is a boolean, `0x10` an int, dates are
timestamps) and validates every Config field with pydantic's lax rules,
so one bad field fails the whole load and the gate falls back to shadow
and python, as the oracle's callers do. YAML outside the subset is denied
undecided. 20,000 random configs: 0 differences from `load_config`.

Status: in force.
Retires when: does not retire.

**GT-26 (ruling): the verifier client table.** `verify_via` finds the
compiled clients from the checkout that holds the oracle's source. The
binary finds them from the checkout that holds the binary. When the two
differ (an installed binary, a checkout elsewhere), the binary may report
"not built" where the oracle dispatches, or the reverse. The verdict is
the conjunction either way, so a client can only tighten it.

Status: retired 2026-09-25 by GT-33.
Retires when: retired (see Status).

**GT-27 (ruling): argparse.** The command line is parsed as Python 3.12's
argparse parses the oracle's parser, errors and usage wrapping included
(20,000 random argv lists: 0 differences). `--help` prints the help and
exits 0, as the oracle does; a host reads that exit as an allow, but no
hook passes `--help`. Help at a terminal width other than 80 is denied
undecided.

Status: in force; its deny at a terminal width other than 80 retired 2026-09-25 by GT-27 (widened).
Retires when: does not retire.

**GT-28 (harness): a fixture expectation that depends on the box.** The
"config names go" case expects the Go conformance client to be built
(`clients/go/conform`). Where it is not built, both gates report "not
built" and allow, and the fixture reads as stale.

Status: retired 2026-09-25 by GT-33.
Retires when: retired (see Status).

## 2026-09-25 — Go gate binary, round 2 (100% native)

Every call is now decided in the binary. The warning regexes, `llm_check`,
`--help` at any width and `--verify-timeout` under 1 s were the last calls
denied undecided in the fuzz. Five of the entries below change the Python
oracle as well as the binary; each of those has its own tests.

**GT-23 (closed, oracle change): a glob step limit, not a time budget.**
The matcher now stops after `GLOB_MATCH_STEP_LIMIT` (100,000) calls of its
inner matcher, in `verify._match_glob` and in the binary alike, and the
gate denies with `file glob '...' is too complex to match: more than
100000 steps`. No answer depends on the speed of the box, so the one
known "looser than the oracle" case is gone. Cases cover three, four and
five `**` segments under and over the limit. The Rust, TypeScript and
Lean conformance clients do not have this limit yet; a glob past it can
make them run long, or allow where the oracle now denies.

Status: in force.
Retires when: does not retire.

**GT-24 (closed): warning regexes and `llm_check` are decided.** See GT-29
and GT-31.

Status: in force as a record; its `llm_check` half now rests on LLM-12, since GT-31 retired.
Retires when: does not retire.

**GT-26 and GT-28 (closed): see GT-33.**

Status: in force as a record.
Retires when: does not retire.

**GT-27 (widened): help at any width.** The help text is formatted as
argparse's HelpFormatter formats it, at the width `COLUMNS` gives (else
80), with textwrap's chunking. 30,000 random argv lists at widths 1 to
200: 0 differences.

Status: in force.
Retires when: does not retire.

**GT-29 (oracle change, open to veto): the gate filters `re`'s set
warnings.** A predicate regex with a nested set (`[[`) or a set operator
made `re` print a FutureWarning on stderr, with the oracle's install path
and source line, which no other client can know. `gate.py` now filters
exactly those FutureWarnings ("Possible nested set", "Possible set
difference/intersection/union/symmetric difference"). Nothing else about
the regex changes. If this filter is vetoed, the binary must go back to
denying such envelopes undecided.

Status: in force (open to the owner's veto).
Retires when: the owner vetoes the filter (then the binaries deny such envelopes undecided again) or confirms it.

**GT-30 (ruling, stricter than the oracle): `--verify-timeout` under
1 s.** The oracle joins its verifier thread for the budget, so under 1 s
its answer depends on how fast the thread runs (at 0.3 s it usually
allows a simple read). The binary denies any budget under 1 s as the
oracle's timeout denies ("verifier exceeded the gate's inner time budget
(Xs)."). NaN and a budget too large for a C time_t raise the oracle's
own ValueError and OverflowError text. The binary is never looser here,
only stricter.

Status: in force.
Retires when: the oracle decides a budget under 1 s without depending on how fast its thread runs.

**GT-31 (port): `llm_check` is litellm's one Anthropic call.** The
backend is chosen as the oracle chooses it (`OPENDAISUGI_LLM_BACKEND`,
then `llm_backend` in `~/.opendaisugi/config.yaml`, then an Anthropic key
or token, then `claude` on PATH). On the litellm backend the binary sends
the byte-identical request litellm 1.100.0 sends (URL, headers, body, the
payload cut at 4,000 code points), reads the reply as litellm does, and
words every failure as litellm words it, secret redaction included (the
failure text is the deny reason). Tests run against a fake server and the
oracle side by side; 147 rows, 0 differences. Denied undecided: the
claude-code backend (it runs `claude -p`), a model that is not
`anthropic/...`, a proxy, CA bundle or `LITELLM_*` setting, and a reply
or network failure the port does not model. Known limit: litellm loads a
`.env` file found above its own install directory at import, which the
binary does not read. Such a file can only add settings; with a key the
oracle finds there and the binary does not, the binary denies with
"Missing Anthropic API Key" where the oracle may allow.

Oracle fix found by the port: after a failed call litellm printed a
"Give Feedback / Get Help" banner on stdout, which broke the reply body
for `--format hermes` and `--format openclaw`. `llm_check` now sets
`litellm.suppress_debug_info`.

Status: retired 2026-09-27 by LLM-12: `llm_check` goes through our own model client, and litellm is gone (with it the banner fix).
Retires when: retired (see Status).

**GT-32 (oracle change): size caps.** A payload over 16 MiB is denied
with "hook payload is larger than 16777216 bytes; the gate does not read
it", and a shell command over 262,144 characters with "shell command is
longer than 262144 characters; the gate does not read it", in both gates.
The binary's parent reads at most 16 MiB plus one byte of stdin.

Status: in force.
Retires when: does not retire.

**GT-33 (oracle change): the gate finds a verifier client by variable
or PATH.** With `verifier_client` set to a compiled client, the oracle
found it in the checkout that held its source, and the binary in the
checkout that held the binary, so their answers depended on where each
sat (GT-26), and so did a fixture case (GT-28). Both now run the file
named by `OPENDAISUGI_<NAME>_CLIENT`, else `daisugi-conform-<name>` on
PATH, else report "not built". The bench keeps the checkout rule.

Status: in force.
Retires when: does not retire.

**GT-34 (fixed, from the fail-closed review): the child's verdict.** A
`DAISUGI_GATE_CHILD` variable in the host's environment made the binary
act as its own child and print the child's frame, which the host read as
an allow. The child now sends its frame to the parent on an inherited
pipe (fd 3), with a nonce the parent put in the variable, and writes only
the format's deny to its own stdout and exit code. The parent decides
from the frame alone. A test sets the variable and expects a deny. The
parent parses the argv itself, so a crash denies in the right format for
`--form hermes` and for a repeated `--format`. The child's deadline is
the verify budget, plus the ask budget with `--ask`, plus 3 s (was a
fixed 25 s), so the rules before the verifier cannot outlive a short host
hook timeout.

Status: in force.
Retires when: does not retire.

**GT-35 (fixed, privacy): no build-machine paths in the binaries.** Z3's
assertions and tree-sitter compiled `__FILE__` as the absolute build path
under `$HOME`, and the Go build without `-trimpath` kept the checkout
and module cache paths (about 370 strings in each binary). `native.sh`
now maps the build directory to `/daisugi-build`, `scripts/build.sh`
builds with `-trimpath`, and `scripts/check-paths.sh` fails when
`strings` finds `$HOME`, the checkout or a Go cache path in a binary.
`native.sh` also checks `zig version` (0.16.0), compiles for the baseline
CPU, and uses an existing prefix only when its stamp and SHA-256 manifest
match. zig's libc++, libc++abi and libunwind have no published digest:
zig builds them on the box from its own tarball, whose digest
`toolchain.sh` checks, so the zig version pins them.

Status: in force.
Retires when: does not retire.

**GT-36 (port of an oracle fix): a line the floor rule cannot split is a
hit.** The oracle's 1234e6db (found by the Rust port): a decompose that
raises inside the floor rule, as the recursion limit does from the floor
rule's deeper stack, now counts as a hit, and a command whose words
shlex cannot split leaves the cwd unknown while the check goes on. The
binary mirrors both, with its depth parity. Four chain shapes (`&&`,
`;`, `||`, pipes) of 900 to 1,060 links into `~/.config/coppice`
through both gates: 644 calls, all denied, 0 differences. Before the
fix the window was 977 and 978 links for `&&`.

Status: in force.
Retires when: does not retire.

**GT-37 (harness): the llm_check cases get a 30 s verify budget.** With
the suite's 5 s, litellm's import alone ran out the budget on a loaded
box, so three cases read as a timeout there and as a refused call
elsewhere.

Status: in force.
Retires when: does not retire.

## 2026-09-25: Rust gate binary (`clients/rust`, `daisugi-gate`)

The Rust gate is a second port of the gate hook, run against the same gate
cases and fuzz as the Go gate. It is standalone: it never runs Python. It
links Z3 5.1.0 and tree-sitter-bash, reads envelopes with jiter, and ports
every stage the gate reaches. A call it cannot decide exactly as the
oracle would is denied with exit 2 and a reason. Its entries are numbered
RG-n, apart from the Go gate's GT-n (the first drafts of RG-1 to RG-6
were numbered GT-19 to GT-24).

**RG-1 (Go client, fixed, was fail-open): an integer past Python's digit
limit.** Found by the Rust port. `json.loads` refuses an integer of more
than 4,300 digits, so the oracle denies such a payload as not parseable;
the Go `pyjson` read it. Both `pyjson` ports now refuse it.

Status: in force.
Retires when: does not retire.

**RG-2 (Rust client, fixed): `shlex_split` escaped `$` and backtick inside
double quotes.** Python's `shlex.split` escapes only `"` and `\` there.
The split now runs against the CPython-generated shlex fixture.

Status: in force.
Retires when: does not retire.

**RG-3 (Rust client, fixed, was looser than the oracle): a redirect with
more than one target.** Now refused with the oracle's reason.

Status: in force.
Retires when: does not retire.

**RG-4 (harness, noted): home at the root.** With `HOME=/` pathlib gives
`/.config/coppice`; the Rust gate joins as pathlib does.

Status: in force.
Retires when: does not retire.

**RG-5 (ported): the secret-file door.** The pane rule's secret files and
`path_names_secret`'s placement are mirrored, the pane rule before the
floor rule.

Status: in force.
Retires when: does not retire.

**RG-6 (oracle finding, fail-open, fixed in 1234e6db): the floor rule
missed a line at the recursion limit.** Found by the Rust port:
`_shell_hit` read a RecursionError from `decompose_command` as no hit, and
its frames sit one deeper than the pane rule's, so at 977 `&&` links a
relative write into `~/.config/coppice` was allowed under an envelope that
allows relative writes. The oracle now reads a failed split as a hit, and
both ports mirror it.

Status: in force.
Retires when: does not retire.

**RG-7 (mirrored): the oracle's fix round of 2026-09-25.** The Rust gate
follows each change: a payload over 16 MiB and a shell command over
262,144 characters are denied unread (2781922f); the `**` glob matcher
stops after 100,000 steps with GlobTooComplex (df98e34e), a count the port
takes exactly from a memo over the recursion; a verifier client is found
by `OPENDAISUGI_<NAME>_CLIENT` or as `daisugi-conform-<name>` on PATH,
never by checkout (ce64bee9); `re`'s set warnings are dropped, so a
predicate regex with `[[` is decided (GT-29); an argv error denies hermes
and openclaw with their block body on stdout, with argparse's text on
stderr (2a333767, 10b86339); and checkpoint git calls run no program the
workspace repo names (c5d33ade). As in the Go gate, every git call of one
checkpoint, both `is_repo` calls included, shares one deadline: at most
5 s, and 2 s before the child's deadline, so a repo whose config blocks
git cannot turn an allowed call into a backstop deny (10b86339).

Status: in force.
Retires when: does not retire.

**RG-8 (ported): `llm_check`.** The call litellm 1.100.0 makes, after the
Go gate's `llm.go` (GT-31): the backend and key as the oracle reads them,
one POST, and each failure in litellm's words. An https URL goes over
rustls with the ring provider and trusts the system's root store (a
ruling: no webpki-roots, no OpenSSL), as the Go gate trusts Go's. A
reply whose text json.loads refuses fails with json.loads's own message,
the JSONDecodeError position counted in code points, and a reply nested
past 900 is not decided, both as the Go gate's `LoadsPy`. Not decided:
`claude -p`, a model or setting litellm routes another way, a network or
TLS failure other than a refused connection (litellm's text for those
is not modelled, as in the Go gate), and a reply the port does not read.
Checked against the oracle over a local fake server: 17 replies, the
same bytes and exit code from both gates.

Status: retired 2026-09-27 by LLM-12 and LLM-1 to LLM-8: the Rust `llm_check` is a call through our own model client.
Retires when: retired (see Status).

**RG-13 (Go client, open, found by the Rust port): a model reply that
starts with a BOM.** `json.loads` of a str that starts with U+FEFF raises
"Unexpected UTF-8 BOM (decode using utf-8-sig): line 1 column 1 (char
0)" before it scans. The Go `LoadsPy` has no such check, so by reading
it words that llm_check reply "Expecting value: line 1 column 1 (char
0)". A deny either way; only the reason differs. The Rust port has the
check.

Status: retired 2026-09 (monthly fold d59b8741): the Go `pyjson` reader now refuses a leading BOM with `json.loads`'s text (`clients/go/internal/pyjson/pydecode.go`).
Retires when: retired (see Status).

**RG-9 (fixed, from the fail-closed review): the child.** The parent reads
at most one byte past the payload cap, builds its deny before it reads,
and runs the child with a fresh nonce and a frame pipe on fd 3. The child
writes its verdict only to that pipe and the format's deny on its own
stdout and exit code, so a process that sets `DAISUGI_GATE_WORKER` with no
such pipe is a parent, and a child started by anyone else reads as a
deny. The deadline is the verify budget, plus the ask budget with `--ask`,
plus 3 s, below the host timeout the installer writes. Every backstop and
undecided deny takes the format argparse reads. The abort test switch is
gone; the test kills a real child.

Status: in force.
Retires when: does not retire.

**RG-10 (fixed, privacy): no build-machine paths.** `tools/z3-build-env.sh`
maps this box's paths away in rustc, the C compiler and the zig c++
wrapper, and takes libc++ and libc++abi by pinned SHA-256.
`tools/check-paths.sh` finds none in either binary. (Since 2026-09-25 it
takes libc++ and libc++abi from the native prefix `native.sh` builds, for
the baseline CPU; see RG-16.)

Status: in force; its pinned-SHA-256 libc++ and libc++abi retired 2026-09-25 by RG-16 (they come from the native prefix now).
Retires when: does not retire.

**RG-11 (not decided): a `\N{...}` escape in a predicate regex, and a
regex repeat of more than 10,000 copies inside a vacuity check.** The gate
does not carry Unicode's name table, and Python builds a Z3 term of that
many parts. A regex search past 20 million matcher steps is not decided
either.

Status: in force.
Retires when: the Rust gate decides these inputs.

**RG-12 (ruling, as GT-30): `--verify-timeout` under 1 s is not decided.**

Status: in force.
Retires when: the Rust gate decides a budget under 1 s as GT-30 does.

**RG-14 (ported): the `daisugi` CLI.** The Go binary's gate commands and
gate half of install, ported to Rust as `target/release/daisugi`, with
the same words for a gate hook, the same refusals and the same one line
for a command not carried. `gate check` is the Rust gate, its child
started as `daisugi gate check`. On the 234 CLI cases: 221 agree, 10 not
ported, 3 refused (YAML outside the reader), the Go binary's split.

Status: in force.
Retires when: does not retire.

## 2026-09-25: install paths and the missing hook program

**IN-1 (ruling, no oracle case): a hook for a mise install names mise's
shim.** A `daisugi` that mise installed runs from a versioned path under
mise's data directory (`MISE_DATA_DIR`, else `$XDG_DATA_HOME/mise`, else
`~/.local/share/mise`, as https://mise.jdx.dev/directories.html says). A
hook that names that path breaks when mise removes the version on
upgrade: enforce mode then denies every call, shadow mode stops seeing
them. So when `install --gate` runs from inside `<data>/installs/` (both
sides with symlinks resolved), the hook names `<shims>/daisugi`
(`MISE_SHIMS_DIR`, else `<data>/shims`) when that shim exists and `mise
which daisugi` names this same binary. Otherwise the hook keeps the
versioned path and install prints one line on stderr: `note: <path> is a
versioned mise install. Run daisugi install --gate again after mise
upgrades daisugi.` The Python CLI is not installed by mise, so it has no
such case; the Go (`install.HookPath`) and Rust (`install::hook_path`)
binaries follow this entry, each with a test that builds the layout and a
stand-in `mise`.

Status: in force.
Retires when: does not retire.

**IN-5 (ruling, no oracle case): on Omarchy the hook names the stub.**
Omarchy runs agents through a stub in `~/.local/bin` that execs `mise x
<tool> -- <bin>`, not through mise's shims. So the order is: mise's shim
as in IN-1; else a script named `daisugi` in `$XDG_BIN_HOME` (first) or
`~/.local/bin`, in a directory on PATH, holding a `mise x <tool> --
daisugi` (or `mise exec`) line, when `mise which daisugi` or `mise which
--tool <tool> daisugi` names this same binary; else the invoked path and
the reinstall note. Go and Rust write byte-identical hooks and notes for
a stub in either directory, on PATH or not, with a stand-in `mise` that
answers only for the stub's tool.

Status: in force.
Retires when: does not retire.

**IN-2 (oracle and both binaries): `install --gate --enforce` needs a
registered envelope.** With no `*.json` in the gate root's `envelopes/`,
it exits 1, writes nothing, and says on stderr: `Enforce needs a policy
first. Run: daisugi gate init --workspace DIR for a starter envelope,
then this command again. Or install in shadow mode: daisugi install
--gate`. CLI cases hold the refusal, a dry run, an envelopes directory
with no `.json`, and a session envelope only.

Status: in force.
Retires when: does not retire.

**IN-3 (oracle and both binaries): `gate status` warns when a hook's
program is gone.** For each gate hook in the `daisugi gate check` form, in
`~/.claude/settings.json` and the working directory's, whose program is
an absolute path that is not there, status adds one stderr line naming
the file, the program and the fix (`--enforce` for an enforce hook), in
text and JSON output alike. Python's `os.path.exists`, Go's `os.Stat` and
Rust's `fs::metadata` all follow symlinks, so a dangling link counts as
gone.

Status: in force.
Retires when: does not retire.

**IN-4 (ruling): `--version` is the build's.** The binaries print the
version their build set (the release number, or the checkout's git
describe), not pyproject's, so `cli_compare.py` checks only that
`--version` prints one version line.

Status: in force.
Retires when: does not retire.

**RG-16 (fixed, portability): the Rust binaries run on any x86-64.**
`tools/z3-build-env.sh` compiles Z3 with `zig c++ -mcpu=baseline` and
links the baseline libc++ and libc++abi of the native prefix, the ones the
Go gate links. Rust's standard library links `libgcc_s` for unwinding on
glibc; it stays: a static glibc build would need glibc's NSS modules at run
time for the name lookup of an llm_check over https, and
`release.sh --rust` allows it and nothing more.

Status: in force.
Retires when: does not retire.

## 2026-09-25: the Go pathway store and matchers (`daisugi pathways`)

Cases: `clients/fixtures/pathways/` (written by `clients/pathway_cases.py`
from the Python store, matchers and CLI). `clients/pathway_compare.py`
runs the Go binary and `cmd/pathway-probe` over them. Rulings, each
checked against the oracle:

**PW-1 (ruling): the torch and onnx matchers are not in the binary.** With
`matcher_model: all-MiniLM-L6-v2` (the default) or `int8`, the oracle runs
sentence-transformers or onnxruntime, or falls back to `lexical` when that
package is absent. The Go binary carries neither and refuses such a `find`
with a line naming `lexical` and `potion`; it never falls back, since a
fallback would stamp and compare rows under an identity the operator did
not choose. An empty store still answers nothing first, as in Python (case
`minilm empty store`). The refusal cases are `go_only` in `find.jsonl`.

Status: in force.
Retires when: the oracle's default matcher is lexical or potion and MiniLM and int8 leave it (rulings DS-3 and DS-4), or the binaries carry those matchers.

**PW-2 (ruling): potion fetches one pinned model.** Python's model2vec
downloads any Hugging Face id through `huggingface_hub`. The binary fetches
only `minishlab/potion-base-8M`, at one revision, each of its three files
checked against a SHA-256 in the source, into
`$XDG_CACHE_HOME/opendaisugi/models/potion-base-8M`, with one line on
stderr before the download. `OPENDAISUGI_POTION_MODEL` naming a local
directory is read as Python reads it; naming another Hugging Face id is
"not available", which `find` answers with no match, as Python answers a
load failure (ADR-0019: never a fallback). The identity stamped on rows is
the same string on both sides, so rows stay compatible.

Status: in force.
Retires when: does not retire.

**PW-3 (ruling): the tokenizer is ported from its tables, not its code.**
`BertNormalizer` and `BertPreTokenizer` decide per code point (removed,
a space, a CJK character padded with spaces, mapped, split, punctuation)
by Unicode tables of the `tokenizers` library's own version.
`gen_tables.py` writes those tables by asking the library about every
code point, so the port does not depend on Go's Unicode version. Checked
per code point on the whole range, then as ids on 3,042 texts with the real
`tokenizer.json` (0 disagreements). A `tokenizer.json` with truncation,
padding, another normalizer, pre-tokenizer or model, or an added token
with options, is refused at load.

Status: in force.
Retires when: does not retire.

**PW-4 (ruling, portability of the oracle): exact ties.** Two rows whose
cosine is equal in real numbers get scores that differ in the last bit,
and which is larger depends on the order of the sums. numpy sums a row's
squares pairwise (the binary does the same, and matches `np.linalg.norm`
bit for bit), but it takes the dot products and the query's norm from
OpenBLAS, whose kernel, and so whose order, is chosen per CPU. The oracle's
own winner in a tie therefore differs between machines. The oracle now
records every row within 1e-12 of the best (`tie_ids`), and a Go pick among
them agrees. Seed 7 of the lexical fuzz met two such ties in 2,000 queries
before the binary summed norms pairwise; after, the compare accepted no
tie the other way on any case or fuzz query.

Status: in force.
Retires when: does not retire.

**PW-5 (ruling): `opendaisugi_version` is the build's.** The json, skill
and smtlib exports name the version of the program that wrote them. The
binary writes its build version (IN-4), and the cases compare that key's
shape, not its value.

Status: in force.
Retires when: does not retire.

**PW-6 (ruling): refusals.** The binary exits 2 with a reason, before it
writes anything, on input it cannot yet read the Python way: a plan step
given as a string (`coerce_step` decodes it as JSON or a Python literal),
a BLOB column, a database path holding `?` (the driver reads it as
options), and a skill file whose frontmatter is not in the form
`yaml.safe_dump` writes (the reader accepts a reading only when dumping it
gives the same text back). One gap: a store whose table needs a migration
is migrated on open, as Python migrates it, before rows are read, so a
refusal on such a store leaves the migrated table. Two refusals remain
where Python admits: a step given as a string, and a number past 64 bits
inside a plan or envelope (the verifier's JSON reader takes neither).

Status: retired 2026-10-08. Its skill-frontmatter refusal retired
2026-10-07 by YM-1; its string-step refusal 2026-10-08 by RF-1 (what
stays refused there is listed in RF-1); its refusal of a number past 64
bits 2026-10-08 by RF-2; its BLOB and `?` refusals 2026-10-08 by RF-3.
Retires when: met (see Status).

**PW-7 (ruling): no `find` command.** The Python CLI has none, so neither
has the binary; `cmd/pathway-probe` is the test instrument that runs
`find` on many queries in one process.

Status: in force.
Retires when: the Python CLI gains a `find` command.

**PW-8 (ruling): help text.** `daisugi pathways ... --help` prints the
binary's own help, as the gate commands do; no case compares typer's.

Status: in force.
Retires when: does not retire.

**PW-9 (finding, speed): `list` is 22.6 ms at p50 on 1,000 lexical
pathways, over the 20 ms target.** `find` is 15.4 ms. `list` validates every
row as `list_all` does (envelope, plan and all 4,096 floats of each
vector); the binary does not skip that to meet the target, since it would
then print a list where Python raises. Storing vectors as binary would
remove most of the cost, but that is a change to the oracle's schema.

Status: in force (open).
Retires when: the Go `list` meets 20 ms at p50 (the Rust one does, PW-R-10).

### Fix round 1 (review of the pathway port)

**PW-10 (oracle finding, fixed in both): import stored what the store
could not read back.** The oracle admitted a plan nested deeper than
pydantic's JSON reader takes back (197 levels of metadata), and every
later `list` and `find` of that store failed; a lone surrogate in a text
column or in the envelope or plan, an int past 64 bits or a NaN time made
`put()` raise part way, after `--overwrite` had already deleted the old
row. `import_pathway` now refuses all of these as `UNSTORABLE`, with the
reason, before anything is deleted or written. The binary checks the same
things in the same order, the read-back with its own port of the reader,
and pydantic's serializer depth counted as pydantic counts it (an empty
list or dict is a leaf). A surrogate in a dict key is not an error in
pydantic: the key is written as three U+FFFD, and so it is by the binary.
The compare now runs Python's `list` and `find` on the store after every
import the binary made.

Status: in force.
Retires when: does not retire.

**PW-11 (oracle finding, fixed in both): a Z3 `unknown` admitted an
import.** `verify()` keeps a Z3 timeout as a warning, so with
`--z3-timeout-ms 1` an envelope with `max_execution_time_s: 0` was
admitted as consistent. Import now refuses with `VERIFICATION_TIMEOUT`
("verifier timed out (Z3 ... exceeded Nms); raise --z3-timeout-ms"). The
binary also read a Z3 error as a pass (so `-1`, which `(set-option
:timeout -1)` rejects, admitted the same envelope); an error is now a
violation. Both CLIs take `--z3-timeout-ms` from 1 to 4294967295 only, the
range Z3's unsigned parameter holds (the ruling: refuse at the flag, not
wrap as z3py's ctypes does). The gate path: Python's gate calls the same
`verify()`, so a Z3 `unknown` there is still a warning and the call is
allowed; this round does not change the gate. The Go gate decides those
two ground checks without Z3 and cannot meet `unknown`.

Status: in force for import; its note that the gate still allows on a Z3 `unknown` retired 2026-09-25 by PW-15.
Retires when: does not retire.

**PW-12 (ruling): a refused skill delegation is worded by the binary.**
Every other violation message is the oracle's, checked on 570 synthetic
plans and the private corpus's verify cases. A delegation that subsumption
refuses is reported by the oracle with Z3's counterexample; the binary
reaches the same verdict through its own SMT-LIB text and gives its own
reason.

Status: in force.
Retires when: the binaries word a refused delegation with Z3's counterexample.

**PW-13 (ruling): how stderr is compared.** Exactly, except for two
Python forms that carry no fixed text: a traceback, where the binary's line
must name the same exception type (`pydantic_core._pydantic_core.ValidationError`,
`FileNotFoundError`, ...), and click's usage box, where the binary's
output must hold the boxed message.

Status: in force.
Retires when: does not retire.

**PW-14 (Go-side improvement): an overwrite is one transaction.** The
binary deletes the old row and writes the new one in one transaction, so
a failure of the write keeps the old row. With PW-10 the oracle no longer
reaches a failing write either.

Status: in force.
Retires when: does not retire.

**PW-15 (ruling): the gate no longer allows on a Z3 `unknown`.** PW-11 left
the gate path a warning-and-allow ("this round does not change the gate").
`verify()` still keeps a Z3 timeout as a warning by its lenient library
default, but now raises it to a Violation when the effective mode is
strict, or the envelope's `stakes` is `physical` (physical forces this
even past an explicit `strict=False`): envelope self-consistency,
plan-vs-envelope, skill-delegation subsumption (`check_skill_delegations`;
a timeout there used to be an uncaught exception, not even a warning), and
the robotics trajectory checks (`check_plan_invariants`, on both `verify()`
and `verify_step`'s per-step hot path). The gate itself now denies any
call whose result still carries a Z3-timeout warning, whatever the
envelope's stakes, with reason "the verifier could not finish a check in
time; denied" plus the warning text; shadow mode logs it as a would-deny
like any other deny. `import_pathway` keeps its stable
`VERIFICATION_TIMEOUT` code for a strict/physical pathway too, reading the
new Violation the same way it already read the warning.

The Go live gate decides the two ground checks (self-consistency,
plan-vs-envelope) by inspection and never calls Z3 there (`maxTimeIn`
parses the time as a `big.Int`, no fallback). Its offline `internal/verify`
port does shell out to a real `z3 -in` for the same two checks and can see
`unknown`; it records that in a `Timeouts` field only
`internal/pathways/importer.go` reads, so the Go gate binary itself is
never exposed to this gap. The Rust live gate decides the same two checks
by inspection too, except that self-consistency falls back to a real
linked Z3 call when `max_execution_time_s` is not plain decimal text
(`self_consistency` in `clients/rust/src/gate/check.rs`). Envelope parsing
always gives decimal text for a JSON int, so no case reaches that call.
It used to treat `Check::Unknown` as `Check::Sat`. It now does what the
oracle does. At `high` or `physical` stakes the unknown is the stage's
violation ("z3: verifier timed out (envelope self-consistency check);
raise the Z3 timeout", detail `reason` and `z3_message`). At any other
stakes it is the warning "Z3 self-consistency check exceeded 500ms", the
later stages still run, and when they all pass the gate denies with "the
verifier could not finish a check in time; denied (...)" and no
violations, so the tier is permanent. A later stage's violation still
decides the call. `verify_record` carries the warnings through the
verifier-client dispatch, which never clears them. A test hook makes Z3
answer unknown (`a_z3_unknown_in_self_consistency_is_denied` in
`clients/rust/src/gate/tests.rs`); the Python gate, with
`check_envelope_self_consistency` made to raise `VerificationTimeout`,
gives the same reasons and details for the same four envelopes.
`gate_compare` over the Rust binary: 0 disagreements in 1288 cases.
The Go gate has no such path: `selfConsistent` ends in `maxTimeIn`, which
parses the time as a `big.Int` and treats text it cannot parse as out of
range, and no Z3 call in `internal/gate` is on the self-consistency or
plan-vs-envelope path. Rust's offline
`z3_checks.rs` port is native throughout, unlike Go's; it never calls Z3.
Vacuity, in both live gates, already folds a Z3 `unknown` into
`non_trivial`, same as the oracle, by design (unrelated to this ruling).
Both offline subsumption ports already fail closed on `unknown` (denied,
not allowed); neither live gate ever builds a `skill` step, so this never
reaches the live gates at all.

Status: in force; its account of the Go offline verifier's `z3 -in` process retired 2026-09-27 by VZ-1 (761edcf2).
Retires when: does not retire.

## 2026-09-26: the Go garden and distiller (`gardener`, `tend`, `hook auto-tend`, `distill-repeats`)

Cases: `clients/fixtures/garden/` (written by `clients/garden_cases.py`
from the Python CLI). `clients/garden_compare.py` runs the Go binary over
them. Model calls in the cases go to a fake `claude` first on PATH and a
fake Anthropic server on 127.0.0.1; both answer from the case's recorded
table, keyed by the SHA-256 of the exact request, so the binary must send
the oracle's request bytes to be answered at all. The fake server also
checks each credential sent (`x-api-key`, and `authorization` for
`ANTHROPIC_AUTH_TOKEN`): a value that is not the case's gets HTTP 596 and
fails the case. The fixture keeps only a name for it (`{KEY}`,
`Bearer {TOKEN}`), never the value. Rulings:

**GD-1 (oracle change): a claude-code structured call reads the CLI's JSON
envelope.** The instructor shim of the claude-code backend
(`ClaudeCodeInstructorClient`, which every structured call site uses) ran
`claude -p` for plain text, so a turn the CLI
itself failed (`is_error`, exit 0) was parsed as the model's prose and
re-asked. It now passes `--output-format json`, reads `result`, and raises
without a re-ask on `is_error` or on stdout that is not the envelope
(sprig reads the same envelope). The standalone `call_claude_p_structured`
is not changed. Both sides send the argv
`-p --model=haiku [DAISUGI_CLAUDE_ARGS] --output-format json` and the
same stdin.

Status: in force.
Retires when: does not retire.

**GD-2 (oracle change): a failed model call is one warning line.** The
distiller's warning printed instructor's `<failed_attempts>` dump, every
completion with fresh response ids and times. It now goes through
`translate_llm_error`, as every other call site: the first cause and the
count, for example `ValidationError: 1 validation error for
GeneralizedTemplate (3 attempts)`.

Status: in force.
Retires when: does not retire.

**GD-3 (oracle change): the distiller stores nothing a Z3 check did not
finish.** `verify()` keeps a Z3 `unknown` as a warning in lenient mode
(and, since PW-15, as a violation under strict mode or physical stakes).
The distiller stored a salvaged or generalized template on the warning and
counted a timed-out test plan as passing. Now either form drops the
template with "verifier timed out; raise the Z3 timeout" and fails the
test plan. The binary reads its verifier's `Timeouts` the same way; its
lenient verify never words the strict violation, and does not need to,
since the distiller treats both alike. The cases cannot reach a timeout
at the default 500 ms; Go and Python unit tests cover it.

Status: in force.
Retires when: does not retire.

**GD-4 (ruling): the litellm backend is Anthropic only, with litellm's
bytes.** The binary sends what litellm 1.100 sends for an `anthropic/`
model under instructor 1.16's JSON mode: compact JSON with non-ASCII kept,
`model` without the prefix, the system text plus instructor's schema
wrapper as one text block, and `max_tokens` from litellm's own table
(generated into `internal/llm/schemas_gen.go` by `gen_schemas.py`, which
`--check` keeps current; a model litellm does not know gets 4096). It
honours `ANTHROPIC_API_BASE`, `ANTHROPIC_BASE_URL`, `REQUEST_TIMEOUT` and
`DEFAULT_ANTHROPIC_CHAT_MAX_TOKENS` as litellm does. A model that is not
`anthropic/<name>` on the litellm backend is refused before anything is
written, with a line naming the two backends the binary carries, even
when the run would make no model call. HTTP
errors are worded as litellm words them for the statuses measured (400,
401, 403, 404, 408, 422, 429, 500, 502, 503, 529); another 4xx is worded
as BadRequestError and another 5xx as InternalServerError, which was not
measured. A connection that fails before any status is worded by the
binary. The key is sent only as `x-api-key` (or a bearer
`ANTHROPIC_AUTH_TOKEN`) and never printed; the case log redacts it.

Status: retired 2026-09-27 by LLM-1 to LLM-9: the litellm backend is gone, and the `api` backend sends the oracle's own bytes.
Retires when: retired (see Status).

**GD-5 (ruling): potion vectors within 1e-6.** model2vec sums the rows of
its float32 table in float32; the binary sums in float64 from the same
table, as `find` does (PW-2). A distilled pathway's stored centroid is
therefore compared within 1e-6 per element under potion, and exactly
under lexical, whose vectors both sides build in float64 with the same
order of sums (`mean(axis=0)` adds rows one by one). A potion model that
cannot be loaded is worded by the binary (the warning after `tend:`); no
case covers it.

Status: in force.
Retires when: does not retire.

**GD-6 (ruling): near-threshold cosines.** Clustering and merge compare a
cosine with a threshold (`>=` and `<`). numpy takes the dot products and
norms from BLAS, whose order of sums is chosen per CPU (PW-4); the binary
sums in order. A cosine within the last bits of a threshold can land on
either side; the cases keep a margin from every threshold, and the fuzz
met no such case.

Status: in force.
Retires when: does not retire.

**GD-7 (ruling): refusals.** The binary exits 2 with a reason, before it
writes anything, when a run would need to read what it cannot read the
Python way: a trace body not in the form `yaml.safe_dump` writes, a
refinement record whose step is not a dict of a registered type (the
oracle would raise mid-run), a stored row it refuses as `pathways` does
(PW-6), a gateway turn whose token counts are not ints, or a config file
its reader does not take. `tend` reads the journal and the store before it
creates anything, each opened read-only (a `file:` URI with `mode=ro`), so
an old journal or store is migrated only after every check has passed; a
column or table a migration would add reads as Python reads it after that
migration (cases `tend old journal minilm`, `tend old store minilm`).
`Prepare` reads every trace in the lookback window, not only those a
cluster uses. What the model replies is never a refusal: the run has
written by then. A reply pydantic takes is read, whatever the size of its
ints, and verify judges it (GD-15), unless it holds NaN or an infinity
(GD-16). A matcher the binary does not carry (PW-1) is
refused only when the run would load its embedder: a stale row to
re-embed, or at least `min_traces` traces. Below that the oracle never
loads it, and the binary runs as it does (case `tend minilm below min
traces`).

Status: in force.
Retires when: the binaries read each refused input the Python way.

**GD-8 (ruling): auto-tend converts only sessions that verify.** A
captured session whose trace would carry a violation is refused, the whole
command with it, before anything is written: the trace stores each
violation's `detail` and `suggested_remediation`, which the binary's
verifier does not word yet. A session that verifies is converted as the
oracle converts it (the same envelope, plan, YAML body and index row; the
verify time differs and is not compared). A record that is not a JSON
object, has no `step_type`, or holds a value of a type the conversion
would raise on is refused too. When a tend will follow the conversions,
its checks run before the first conversion, counting the conversions in,
so a tend the binary would refuse (a matcher it does not carry with
enough traces, a trace body it cannot read) refuses the whole run with
nothing written. The oracle would convert, then fail or warn in the tend
(cases `auto-tend default matcher three sessions`, `auto-tend then tend
unreadable trace`). One exception: a session whose plan holds NaN or an
infinity is skipped with its text, as the oracle skips it (GD-17).

Status: retired 2026-09-28 by F-7: the Go binary, and the Rust binary with stage F, convert a session whose trace carries a violation the verifier words, and refuse only one it does not word (C-14).
Retires when: the binaries' verifier words every violation detail (C-14).

**GD-9 (ruling): a model's reply as a Python literal.** `decode_dict_text`
reads a reply or a string step with `ast.literal_eval` when `json.loads`
fails. The binary reads the literals a model writes (dicts, lists,
tuples, quoted strings with the common escapes, decimal numbers, `True`,
`False`, `None`) and nothing else; a rarer literal form (triple quotes,
bytes, sets, hex numbers, implicit concatenation) is read as not valid
JSON, where the oracle would read it.

Status: in force.
Retires when: does not retire.

**GD-10 (ruling): what prints nothing.** The oracle's own loggers carry a
NullHandler, so its log lines (an unparseable `DAISUGI_CLAUDE_ARGS`, a
re-embed skipped, a gateway line skipped, an invalid `local_tier1.json`)
print nothing; the binary prints nothing for them either. instructor's
`Max retries exceeded ...` and `API call failed on attempt N: ...` lines
and litellm's red "Give Feedback" banner (on stdout) do print, and the
binary prints them the same way.

Status: in force; its instructor lines and litellm banner retired 2026-09-27 by LLM-10.
Retires when: does not retire.

**GD-11 (oracle change): ties in `created_at`.** Traces are read newest
first. With `ORDER BY created_at DESC` alone, equal times (auto-tend stamps
whole seconds) came back in no fixed order, and the order picks the
representative the model is shown. Both sides now read
`ORDER BY created_at DESC, rowid DESC`: of two traces in one second, the
later insert is the newer (case `tend same second`).

Status: in force.
Retires when: does not retire.

**GD-12 (ruling): what is normalized.** A run mints pathway, env, plan and
trace ids at random and stamps times; the cases rename minted ids by first
appearance, write a time near the run as its offset from the run's start
(to the minute), and hide durations (`in N.Ns`, `elapsed_s`, a verify's
`duration_ms`, litellm's `time taken`). Everything else is compared
byte for byte, stderr included.

Status: in force.
Retires when: does not retire.

**GD-13 (oracle change): a salvaged leaf's workspace.** The do-nothing
salvage delegates a divergent step to an `agentic` leaf, and verify
requires the leaf's workspace inside the envelope's `file_read` globs. The
workspace was always `.`, so under the usual absolute globs (`/work/**`)
every salvage failed. It is now the fixed prefix of a `file_read` glob:
the leading segments before the first one with a glob character
(`/work/**` gives `/work`, `**` and `./**` give `.`, `/**` gives `/`). A
glob with no glob character names one file and gives none. The first
prefix that the globs admit under verify's own matcher wins, a glob too
complex to match giving none. When no glob gives one, both sides skip
salvage with the warning `delegated salvage skipped: no file_read glob
gives the leaf a workspace; falling back to frozen generalization` (cases
`tend salvage absolute globs`, `tend salvage no workspace`).

Status: in force.
Retires when: does not retire.

**GD-14 (ruling): help text and the commands left out.** `--help` prints
the binary's own help, as for the gate and pathway commands (PW-8).
`registry pull-and-tend` needs the git pathway store and says it is not in
this binary yet, as do the other `hook` commands.

Status: in force for `hook list`; retired for `registry pull-and-tend` on
2026-09-28 (stage L, L-12), for Go and then for Rust. The garden case is
now `registry pull-and-tend no clone`, which both answer as the oracle does.
Retires when: `registry pull-and-tend` and the other `hook` commands are in the binaries.

**GD-15 (ruling): ints of any size in a model's reply.** pydantic takes an
int of any size, so a reply may hold `max_execution_time_s: 10**20`. The
Go verifier read it into an `int` and failed, and tend refused after it
had made its stores. It now reads the max time as a big int, which the Z3
check finds out of range, as the oracle does (the envelope is
inconsistent; the run carries on). The ints the verifier does not judge
are kept as text, and a step's number past the float64 range stays a
number (cases `tend improvement huge max time`, `tend template huge int
in metadata`).

Status: in force.
Retires when: does not retire.

**GD-16 (ruling): NaN and the infinities in a model's reply are
schema-invalid.** pydantic's float fields take NaN, Infinity and -Infinity
(and a number too large for a float, which reads as an infinity), and a
stored NaN dumps as null, which reads as no limit. So the distiller took a
reply with `"velocity_limit": NaN` and adopted it. Now, on both sides and
on both backends, a structured reply that validates is walked in its
`model_dump()` (Go: the validated value), and each number that is not
finite is a `finite_number` error at its place, in document order,
titled with the response model's name (`models.non_finite_error`, Go
`nonFinite` in `internal/llm`). The walk covers Any-typed data, such as
step metadata, and steps decoded from text. The reply then takes the
existing schema-invalid path: on claude-code, "claude -p output failed X
validation: ..." and a re-ask; on litellm, the check runs inside
instructor's own `model_validate_json` (a subclass that keeps the model's
name, docstring and schema text, and returns the model itself), so
instructor re-asks and gives up as for any schema error. The prompt and
schema bytes do not change. Case `tend improvement nan velocity` now
expects the refusal: three attempts, then "cluster improvement pass
failed". Unit tests on both sides pin the same texts for both backends.
This ruling covered model replies only. The other places Python reads an
envelope (the gate root and `gate register`, `pathways import`, the other
CLI commands that read an envelope file, and the stores that read back
their own rows) took NaN and the infinities too; a probe imported a
bundle with `"velocity_limit": NaN` and the store then held null, since
PW-10's NOT NULL check does not cover a nullable column. GD-17 closes
them all at the model level.

Status: in force; its instructor path on the litellm backend retired 2026-09-27 by LLM-9, which keeps the same check.
Retires when: does not retire.

**GD-17 (oracle change): an Envelope or ActionPlan with NaN or an infinity
is invalid everywhere.** `Envelope` and `ActionPlan` now carry an
after-validator that raises `non_finite_error` (GD-16) on the validated
model. So every reader refuses such a value: `model_validate_json`,
`model_validate`, `Envelope(**...)`, a store row, a bundle, a model's
reply, and a model that holds one (a `CompiledPathway`, a SkillStep's
`contract_envelope`). Nested, the errors take the outer model's title
and place (`envelope.permissions.velocity_limit` for CompiledPathway;
`steps.contract_envelope...` for a step, with no item index, as pydantic
places any error a step raises). The after-validator runs only when the
fields validate, so an envelope with another error reports that error
alone. The GD-16 reply texts do not change: for an Envelope reply the
same error is raised one step earlier, and `non_finite_error` still
checks the other reply models. The readers take these forms: JSON's
`NaN`, `Infinity`, `-Infinity` and a number too large for a float
(`-NaN` and `+Infinity` are invalid JSON); YAML's `.nan`, `.NaN`, `.NAN`
and `[-+].inf` in its three cases (PyYAML reads `NaN`, `inf` and `1e999`
as strings); and a string pydantic reads as a float (`"nan"`,
`" inf "`, `"-Infinity"`, `"1e999"`). In Python mode (`model_validate` after
`json.loads` or `yaml.safe_load`) an int too large for a float is a
`float_type` error instead ("Input should be a valid number"); Go's
pmodel now gives that error too, as Rust's already did (case `import big
int velocity`).
- **Gate:** an envelope in the gate root with such a number is an
  unreadable envelope: "gate I/O error (denied fail-closed): 1
  validation error for Envelope ...". Enforce denies. Shadow mode allows
  and records the would-deny, as for any unreadable envelope; that is
  the shadow contract and is not changed here. An MCP call whose
  arguments hold such a number makes the gate's plan invalid: "gate
  internal error (denied fail-closed): 1 validation error for ActionPlan
  steps.0.arguments.n ...", before the allowlist is checked. Go and Rust
  word both texts exactly (Go: `pmodel.Model.Finite`, and the MCP check
  in `record.stepFieldCheck`; Rust: `pydantic::non_finite`).
- **`gate register`:** refuses with one line on stderr, "not registered:
  <path> is not a valid envelope: <place>: <message>; ...", exit 1, and
  writes nothing (any ValidationError; it was a traceback). The binaries
  read YAML `.nan` and `.inf` now (they refused them as YAML they do not
  read) and refuse a non-finite number as invalid, exit 1. stderr is not
  compared on a non-zero exit, so their line is their own.
- **`pathways import`:** `parse_bundle` raises SCHEMA_INCOMPATIBLE with
  "the pathway is not valid: " and the ValidationError's text, for any
  invalid pathway (it was a traceback), and stores nothing. The binary
  prints the same text, a non-dict pathway's `model_type` error included.
- **Stored rows:** a row whose envelope or plan text holds such a number
  fails to read, as any invalid row does (a traceback naming the
  ValidationError); `envelope_cache.get` raises, so generation fails
  closed rather than adopting the envelope.
- **auto-tend:** a captured session whose plan holds such a number
  raises a ValidationError, a ValueError, so auto-tend prints "skipped
  <id>: <text>" and converts the other sessions. The binary words that
  error exactly and skips too, where GD-8 refuses the other conversion
  errors.
- **YAML in config.yaml:** the binaries' YAML reader is shared, so
  config.yaml's `.nan` and `.inf` now read as floats too, where the
  binaries refused them. In an int or bool field they are invalid, and
  a top-level one is truthy, so not an empty config, as in the oracle
  (cases `status cfg inf for int`, `status cfg nan for bool`, `status
  cfg top nan`, `status cfg top minus inf`).
- **Other readers:** no data file in the repo holds such a number in an
  envelope or plan. The conformance runners (`cmd/conform`, Rust
  `models.rs`) read their cases with `encoding/json` and serde_json,
  which have no NaN or Infinity literal, so they cannot take one.

Cases: the `non_finite` tag in the gate cases (enforce, shadow, hermes,
a session file, MCP arguments), `register nan ...`, `register yaml
.nan` and the rest, `import nan ...`, `list nan envelope row` and the
other row cases, and `auto-tend nan mcp argument`. The existing cases
`register invalid`, `register bad bool`, `register bad bounds`,
`register bad stakes`, `register summary long` and `import invalid
pathway` now expect the one-line refusal.

Status: in force.
Retires when: does not retire.

## 2026-09-26: the Rust pathway store and matchers (`daisugi pathways`)

The Rust `daisugi` now carries `pathways list|show|stats|delete|export|
import`, and `target/release/pathway-probe` runs `find`. It is a port of
the Go binary's D1 (PW-1 to PW-15), run against the same cases
(`clients/pathway_compare.py --binary ... --probe ...`). Where the Rust
binary rules as Go does, the entry says so; the rest are its own.

**PW-R-1 (as PW-1): the torch and onnx matchers are not in the binary.**
A `find` under `all-MiniLM-L6-v2` or `int8` is refused with the Go
binary's line, word for word, naming `lexical` and `potion`; an empty
store still answers nothing first.

Status: in force.
Retires when: the oracle's default matcher is lexical or potion and MiniLM and int8 leave it (rulings DS-3 and DS-4), or the binaries carry those matchers.

**PW-R-2 (as PW-2, and one transport ruling): potion fetches one pinned
model.** The same revision and the same three SHA-256 values as the Go
binary, the same cache directory, and the same one line on stderr before
a download. The SHA-256 is the crate's own (`gate/sha256.rs`, already in
the gate); no crate was added for it. The download goes over rustls with
the ring provider and the system's root store (as RG-8), follows the
Hugging Face `resolve/` redirects (https only, at most eight, a
relative `Location` joined to its host), and stops past the pinned size.
The tests use a local server; no test downloads the model. The three
cached files on the reference box hash to the pinned values.

Status: in force.
Retires when: does not retire.

**PW-R-3 (as PW-3): the tokenizer is ported from its tables.**
`tools/gen_potion_tables.py` asks the `tokenizers` library (0.23.2)
about every code point and writes `src/pathways/potion/tables.rs`;
`--check` says whether the file is current. 0 disagreements on the
3,042 real-tokenizer texts and the tiny model's goldens.

Status: in force.
Retires when: does not retire.

**PW-R-4 (ruling, where Go refuses): a `?` in the database path is
read.** rusqlite opens the file with no URI flag, so `?` is part of the
name, as in Python's `sqlite3.connect`. The bundled SQLite is compiled
with `SQLITE_USE_URI`, which reads any name that starts with `file:` as a
URI; such a relative name is given as `./file:...`, the file Python
opens (case `list data dir file uri`).

Status: in force.
Retires when: does not retire.

**PW-R-5 (ruling): refusals.** The binary exits 2 with a reason, before
anything is written, on input it cannot read the Python way: a plan step
given as a string, a BLOB column, a skill
file whose frontmatter is not in the form `yaml.safe_dump` writes ("...
is not in this binary yet.", as PW-6), and an `llm_check` predicate that
import would evaluate: Python asks a model there, and the binary asks a
model only for a gate call. (A number past 64 bits inside a plan or an
envelope was refused here too; since GD-R-3 it is read as pydantic reads
it.) The bundle is read, validated, verified and
checked storable before the store is opened, so every such refusal
leaves the tree as it was (Go's PW-6 gap, a store migrated before the
refusal, does not arise here for import).

Status: retired 2026-10-08 by RF-1: import reads a string step as
coerce_step reads it (case `import string steps`). Its refusal of a
number past 64 bits retired 2026-09-26 by GD-R-3, its `llm_check`
refusal 2026-10-02 by PG-1 (import asks the model as the oracle does; 3
cases), and its skill-frontmatter refusal 2026-10-07 by YM-1.
Retires when: met (see Status).

**PW-R-6 (as PW-11 and PW-15): import on the linked Z3.** The two ground
checks (envelope self-consistency, plan-vs-envelope) run on the linked
Z3 5.1.0 with `--z3-timeout-ms` (the typed API, as z3py's
`solver.set("timeout", ms)`). An `unknown` is the oracle's warning ("Z3
self-consistency check exceeded Nms"), or under strict mode or physical
stakes its violation ("verifier timed out (...); raise the Z3 timeout")
with that warning as `z3_message`; import refuses on either with
`VERIFICATION_TIMEOUT`, as `import_pathway` does. A Z3 error is a
violation. A test makes Z3 answer unknown
(`pathways::verify::tests::a_z3_unknown_is_a_timeout`). Skill-delegation
subsumption is the oracle's too: an `unknown` from its final check is the
oracle's `VerificationTimeout` ("Z3 subsumption check exceeded Nms"), a
warning, or under strict mode or physical stakes the violation "verifier
timed out (skill-delegation subsumption); raise the Z3 timeout", and
import refuses with `VERIFICATION_TIMEOUT`. Both binaries said
`VERIFICATION_FAILED` here before (a refused delegation); both refused, so
it was the wrong code, not a fail-open. Tests:
`pathways::verify::tests::a_z3_unknown_in_skill_subsumption_is_a_timeout`
(Rust) and `TestZ3UnknownInSkillSubsumptionIsATimeout` (Go), with a
forced unknown. An `unknown` on a glob axis (`_patterns_subsume`) is a
refused delegation in the oracle and in both binaries. The Rust
subsumption checks now take `--z3-timeout-ms` as `(set-option :timeout
ms)`, as the Go binary sends it (test:
`subsumption::time_limit_tests::the_time_limit_reaches_z3`). The Rust
offline verifier (the conformance client) has no such flag: it sets no
limit, and still reads an `unknown` as a refused delegation.

Status: in force.
Retires when: does not retire.

**PW-R-7 (as PW-12): a refused skill delegation is worded by the
binary.** Every other violation message is the oracle's: the offline
verifier (`verify.rs`, `z3_checks.rs`, `dag.rs`) now carries each
message, the DAG stage names the cycle networkx's `find_cycle` walk
names (ported, as the Go client ports it), joint limits and targets keep
their document order for the robotics messages, and the predicate stage
is the gate's (Python's `re`, pydantic's tagged union). Checked on the
570 plans of `verify_messages.jsonl`. Stage and step are unchanged, so
`conform` answers as before: 11,450 of 11,450 on the frozen corpus after
the change.

Status: in force.
Retires when: as PW-12.

**PW-R-8 (as PW-7 and PW-8): no `find` command, and the binary's own
help text.** `pathway-probe` (find, tokens, time) is the instrument, and
writes the Go probe's JSON lines field for field.

Status: in force.
Retires when: as PW-7.

**PW-R-9 (as PW-4, PW-5, PW-13, PW-14): ties, the version, stderr and the
overwrite.** Norms are summed in numpy's pairwise order and any tied row
is accepted; `opendaisugi_version` is the build's; stderr is compared as
PW-13 says; an overwrite deletes and writes in one transaction.

Status: in force.
Retires when: does not retire.

**PW-R-10 (finding, speed): `list` meets the target.** A first build took
38 ms at p50 on 1,000 lexical pathways. The validator copied every string
and built each location eagerly, and the shared JSON object hashed each
key twice. The validator now consumes its input and builds a location
only for an error, and an object hashes its keys only past 16 of them
(`gate/pyjson.rs`); `list` is 21 ms and `find` 7 ms at p50. The gate and
CLI compares still agree after the object change.

Status: in force.
Retires when: does not retire.

**PW-R-11 (ruling, and a Go finding, closed): a value compared over a
tuple field.** Found by the Rust port with a probe. The oracle's predicate stage
reads each step as `model_dump()`, which keeps `target_position`,
`target_orientation` and `target_pose` as tuples; `(1.0, 2.0, 3.0)`
equals no list and its `str()` is another text. A validated step here
holds a list. So an import whose plan has a Cartesian or VLA step with
such a field, under an enforced `equals` over it, was admitted by both
binaries where the oracle refuses with VERIFICATION_FAILED ("invariant
'rule' violated"): a fail-open. The Rust binary first refused such an import;
it now answers as the oracle does: the predicate stage reads each step as
its `model_dump()`, with those fields of a `cartesian_move` or `vla` step
as a tuple (`pyjson::Value::Tuple`) that equals no list and has the
list's length and the tuple's methods (test:
`pathways::verify::tests::a_tuple_field_compares_as_a_python_tuple`,
each answer the oracle's for the same plan).
The Go binary answers as the oracle does: its predicate evaluator holds
those fields of a `cartesian_move` or `vla` step as a tuple type, which
equals no list and has the list's length (`TestTupleFieldsCompareAsPythonTuples`).
The import, the verifier and conform all use that evaluator. Nine
pathway cases (`import verify tuple ...`, `import verify true ...`,
`import verify list of true`) check it: both binaries agree on all of
them. The gate never builds a `cartesian_move` or `vla` step (no gate
step builder names either type), so neither live gate is affected. The
Rust offline verifier (the conformance client) now holds a tuple there
too (`predicate::tuple_tests`); no corpus case reaches it. After these fixes `conform` matches the frozen
corpus in both clients, 11,450 of 11,450.

Status: retired 2026-09-26: closed. The Rust refusal it first ruled is gone, and both binaries answer as the oracle does.
Retires when: retired (see Status).

**PW-R-12 (Go finding, closed): True equals 1.** Found while fixing
PW-R-11. The Go verifier's predicate evaluator compared values with
`reflect.DeepEqual`, so `True` did not equal `1` or `1.0` and `False` did
not equal `0`. Python's `==` holds both, in lists and dicts too. So an
enforced `not_equals` or `not_in_set` that the oracle finds violated
passed in Go: a fail-open, in import, the verifier and conform. The
evaluator now compares as Python does (`TestBoolAndNumberCompareAsPythonDoes`),
checked by seven cases in `verify_messages.jsonl` and three pathway
cases. Both Rust evaluators already compared this way (`py_eq`).

Status: retired 2026-09-26: closed. The Go evaluator compares as Python does.
Retires when: retired (see Status).

## 2026-09-26: the Rust garden and distiller (`gardener`, `tend`, `hook auto-tend`, `distill-repeats`)

The Rust `daisugi` now carries the rest of the pathway life cycle, a port
of the Go binary's D2 (GD-1 to GD-17), run against the same cases
(`clients/garden_compare.py --binary clients/rust/target/release/daisugi`)
with the same fake `claude` and fake Anthropic server. Where the Rust
binary rules as Go does, the entry says so; the rest are its own.

**GD-R-1 (as GD-1, GD-2, GD-4, GD-10, GD-16): the model call.**
`llm` sends the oracle's request bytes on both backends: `claude -p
--model=haiku [DAISUGI_CLAUDE_ARGS] --output-format json` with the
prompt on stdin, and litellm's compact JSON body to the Anthropic
Messages API with the key only as `x-api-key` (or a bearer
`ANTHROPIC_AUTH_TOKEN`). The schema texts and litellm's max_tokens table
come from the libraries through `tools/gen_llm_schemas.py`, which reads
them with the Go generator's own functions and whose `--check` says
whether `src/llm/schemas_gen.rs` is current. A reply is validated in
pydantic's order, and a reply with NaN or an infinity is schema-invalid.
Failures are worded as `translate_llm_error` words them; instructor's
lines and litellm's banner are printed where the oracle prints them. A
model that is not `anthropic/<name>` on the litellm backend is refused
before anything is written, with the Go binary's line.

Status: in force for the `claude -p` call; its litellm half (litellm's body and `max_tokens` table, instructor's lines, litellm's banner) retired 2026-09-27 by LLM-1 to LLM-10.
Retires when: does not retire.

**GD-R-2 (ruling): the HTTPS call.** The POST is the binary's own, over
the rustls and ring already in the tree (no crate added): `connection:
close`, the body framed by its length, and `REQUEST_TIMEOUT` (litellm's
6000 s when unset) bounding the connect and every read and write. A
timeout is litellm's `Timeout` text; a connection that fails before any
status is worded by the binary (as GD-4) and never holds the key. A test
sends one request over TLS to a local CA's server
(`llm::http::tests::a_request_over_tls_and_a_timeout`). The claude
subprocess runs in a fresh `opendaisugi-claude-XXXXXXXX` directory
(mode 0700) under `$TMPDIR`, 120 s, then SIGTERM, two seconds, SIGKILL,
as `_terminate_and_reap` does.

Status: in force for the `claude` subprocess; its HTTPS POST (`REQUEST_TIMEOUT`, litellm's `Timeout` text) retired 2026-09-27 by LLM-8 and LLM-14.
Retires when: does not retire.

**GD-R-3 (as GD-3 and GD-15, and a ruling): the distiller stores only
what verifies.** Every template, salvaged or generalized, and every test
plan is verified by `pathways::verify` with the linked Z3 at 500 ms
(`distill::verify_for_store`). A violation, a Z3 error (a violation) and
a Z3 check that answered unknown each drop the cluster; NaN and the
infinities are invalid before verify sees them (GD-17). An int of any
size is read as pydantic reads it: the max time goes to Z3 as its exact
text, so `10**20` is inconsistent as in the oracle, and any other int
past 64 bits is held at the nearest 64-bit bound in the verifier's typed
copy, which keeps its order against every small number (cases `tend
improvement huge max time`, `tend template huge int in metadata`).
Ruling: a template this binary cannot verify (an `llm_check` predicate,
which the oracle would ask a model about, or a pattern the port does not
decide) fails with the reason "this binary cannot verify it: ..." and is
never stored. The Go binary drops it too, with its verifier's evaluation
error as the reason; the oracle would ask a model. No case reaches it;
the distiller's test covers it.

Status: in force.
Retires when: does not retire.

**GD-R-4 (as GD-7 and GD-8, and a ruling): refusals come before the first
write.** `tend` reads the config, the matcher, the model's backend, the
journal and the store first, the journal and the store opened read-only
(a column a later migration adds reads as its default, a missing table as
empty), and refuses there. `hook auto-tend` reads, converts, verifies and
YAML-dumps every session first, and runs tend's checks with its
conversions counted in. The refused cases are the Go binary's twelve,
the same names (a `--verbose` run of both binaries lists them). Ruling:
should a run still meet input it cannot read after its first write, it
stops with exit 2 and says it stopped after its first write, never
"Nothing was changed." No case reaches it.

Status: in force.
Retires when: does not retire.

**GD-R-5 (as GD-5, GD-6, GD-9, GD-11, GD-12, GD-13, GD-14): the rest.**
Potion vectors agree within 1e-6; cosines and centroids are summed in
order; a model's Python literal is read by the Go subset
(`pathways::literal`); traces are read newest first with rowid breaking
a tie; the salvage workspace is a glob's fixed prefix; the normalized
fields are the cases' own; `--help` is the binary's own text, and
the other `hook` commands say they are not in this binary yet
(`registry pull-and-tend` is in the binary since stage L).

Status: in force.
Retires when: does not retire.

**GD-R-6 (ruling): a reply's steps in the oracle's order.** `coerce_step`
runs over every item before the StepBase check, so an invalid registered
step after an item that is no step is the error pydantic reports. The
Rust reply model does the same (`pmodel::tests::a_reply_names_the_invalid_step_first`,
the oracle's text), and so does the Go reply model since `25e9dc9a`. The
case `tend claude reply names invalid step first` covers it on both
binaries. Since RF-1 (2026-10-08) stored input takes the same order: both
binaries read every plan through one coerce_step path.

Status: in force.
Retires when: does not retire.

**GD-R-7 (ruling): big numbers the Rust garden holds.** A count is a
SQLite INTEGER, so a sum of two fits 128 bits; a failure ratio is the
exactly rounded quotient Python's int / int gives
(`garden::tests::int_division_is_correctly_rounded`); a merged count past
64 bits fails as sqlite3 fails (case `merge sum past 64 bits`). A
session's `captured_at` is ordered as a float, where Go orders an exact
rational: two ints past 2**53 that round to one float may list in
another order. No case reaches it.

Status: in force.
Retires when: does not retire.

## 2026-09-26: the Go gateway and router (`gateway`, `gateway-report`, `router status|stop`, `route`, `install --gateway`)

Cases: `clients/fixtures/gateway/` (written by `clients/gateway_cases.py`
from the Python CLI). `clients/gateway_compare.py` runs the Go binary over
them. A proxy case starts the gateway as a real process on a free
loopback port in front of a fake upstream on 127.0.0.1, which answers from
the case's table keyed by the SHA-256 of the request path and the exact
body bytes (HTTP 597 when there is no answer, so a body one byte off shows
up). The client is a raw socket. A credential is recorded as a name,
never its value, and a case fails when the value reaches any record.
Switchyard cases put a fake `switchyard-server` first on PATH; it records
what it was started with and whether it outlived the gateway. Nothing
reaches a real host or model. Rulings:

**GW-1 (ruling): the gateway serves no answer from the stores.** The
brief for this stage reads as if the gateway answers repeats from the
answer store and from pathways. The oracle's proxy does not: it only
captures answers (opt-in, `--capture-answers`), and `recall` and
`recall_answer` are reached through the MCP tools, which a harness calls
before the model. The Go gateway does the same. `internal/recall` carries
both as a library, proven on the oracle's cases through
`cmd/recall-probe` (GW-11).

Status: in force.
Retires when: does not retire.

**GW-2 (ruling): the server's own log lines are not compared.** uvicorn
prints lifecycle lines on stderr, an access line per request on stdout
(with the client's port) and an "Exception in ASGI application" traceback
block, all in its own grammar (`INFO:     `, `ERROR:    `). The harness
drops lines of that grammar on both sides. The binary prints no access
log; a port it cannot bind is said in uvicorn's `ERROR:` form and exits 3,
as uvicorn exits.

Status: in force.
Retires when: does not retire.

**GW-3 (ruling): framing and the server's own identity are not
compared.** uvicorn adds its own `date` and `server` beside the ones the
upstream sent, and each side picks its own framing. `date`, `server`,
`transfer-encoding`, `content-length` and `connection` are not compared
in an answer; every other header is, with duplicates joined by ", " as
httpx lists them. Upstream, the headers compared are the credentials,
`anthropic-*`, `content-type`, `accept-encoding` and `x-*`; a
`user-agent` the client did not send is each transport's own.

Status: in force.
Retires when: does not retire.

**GW-4 (ruling, oracle changed): SIGTERM runs the exit cleanup, then ends
the process by the signal.** uvicorn drains, then re-raises SIGTERM. The
gateway command, when it started a Switchyard child, has its own SIGTERM
handler in place by then: it runs the same cleanup as its `finally` block
(stop the child, remove the state file, print the `switchyard:` line), once,
then restores the default action and sends SIGTERM to itself, so the
process still dies of it (exit -15). No state file stays behind, so
`router status` shows no stale child. The binary does the same: it drains,
stops the child, then ends by SIGTERM. SIGINT drains, runs the stop, and
exits 0. Case `switchyard stopped by sigterm` records it. Before this
ruling the cleanup did not run on SIGTERM, and the state file stayed until
`router stop`.

Status: in force.
Retires when: does not retire.

**GW-5 (ruling): nesting past the recursion limits.** CPython 3.12's
`json.loads` and `json.dumps` raise past about 10,000 levels (the C
recursion limit), and `_block_chars` past about 1,000 Python frames less
the server's own. The binary uses 9,990 and 960. The exact level is not
matched; cases 3,000 levels deep agree (routed, and the count_tokens shim
answering 0).

Status: in force.
Retires when: does not retire.

**GW-6 (ruling): no timeout.** The oracle's upstream client has
`timeout=None`. The binary sets none either, the dial included. A slow
upstream is waited for (case `buffered slow upstream`); a refused one, or
a body cut before it ends, is a 500 "Internal Server Error" as uvicorn
answers; a stream cut after the answer started drops the connection, and
neither journals the turn.

Status: in force.
Retires when: does not retire.

**GW-7 (ruling): the stream sniffer decodes each read on its own.** The
oracle decodes each chunk `aiter_bytes` yields with replacement
characters, so a UTF-8 character split across two network reads is kept
as U+FFFD in a captured answer. The binary decodes each read the same
way. Where reads split depends on the network; the case pins the split
with a pause, and both keep the same text.

Status: in force.
Retires when: does not retire.

**GW-8 (ruling): a journal record of other types is refused.**
`gateway-report` and `router status` read the turn journal the gateway
writes. A record whose tokens are not ints (or bools), whose dollars are
not floats, whose signature, task or tier is not a str, whose
`downgraded` is not a bool, or a switchyard record whose model is not a
str, is refused (exit 2, nothing changed): the oracle adds whatever is
there, with Python's mixed-type arithmetic, or raises. A line json.loads
raises on with something other than JSONDecodeError is refused too.

Status: in force.
Retires when: does not retire.

**GW-9 (ruling): a Switchyard TOML file outside the reader is refused.**
`--switchyard-config` is read by a reader for the TOML a deployment file
uses (comments, tables, bare, quoted and dotted keys, strings, integers,
floats, booleans, one-line arrays and inline tables). A file outside that
(or invalid) is refused, exit 2, and no child starts; the oracle prints
tomllib's error and exits 1 (case `switchyard own config not toml`).

Status: retired 2026-10-08 by RF-5.
Retires when: met (the binaries read every TOML file `tomllib` reads).

**GW-10 (ruling): install --gateway and the router config.** The binary
writes the gate and base_url layers (Claude Code's `ANTHROPIC_BASE_URL`,
Codex's model provider, OpenClaw's provider; Hermes is the gap Python
names). `--uninstall` reverses both layers this binary writes, as the
oracle's uninstall reverses every layer; the cases' oracle shim reverses
only those two. `--router` rewrites config.yaml as `save_config` does,
every field restated as pydantic dumps it. A file whose values the binary
cannot restate that way (YAML its reader does not read) is refused
before anything is written; a file pydantic rejects fails after the
harness writes, as the oracle fails.

Status: in force.
Retires when: does not retire.

**GW-11 (ruling): recall in Go.** `recall` serves a pathway's plan only
after it verifies against the caller's envelope in the linked Z3. A Z3
check that answered unknown is a miss here, where the oracle's lenient
verify keeps it as a warning and serves the plan: stricter, as GT-30.
Binding a typed pathway's holes asks a model; the binary does not, and
takes `bind_parameters`' own fallback, the frozen template, which the
oracle takes too whenever no model answers (the cases run with a fake
`claude` that fails). A cosine agrees within 1e-9 (PW-4).

Status: in force; its clause on binding (the frozen template, no model) retired 2026-09-27 by K1-7.
Retires when: does not retire.

**GW-12 (ruling): help text.** `--help` prints the binary's own help, as
for the other commands (PW-8, GD-14). `router` with no command prints the
binary's help and exits 2, as typer does with its own help.

Status: in force.
Retires when: does not retire.

**GW-13 (fix): sums of floats.** Python 3.12's `sum()` over floats is
compensated (Neumaier). `summarize`, `build_report`, the share table and
the reuse worklist sum dollars with it; the binary now uses the same
algorithm (`pyjson.SumFloats`), `distill-repeats` included, which had
added with plain `+=`.

Status: in force.
Retires when: does not retire.

## 2026-09-26: the Rust gateway and router (`gateway`, `gateway-report`, `router status|stop`, `route`, `install --gateway`)

The Rust `daisugi` now carries stage E, a port of the Go binary's
gateway (GW-1 to GW-13), run against the same cases
(`clients/gateway_compare.py --binary clients/rust/target/release/daisugi`)
with the same fake upstream, fake `switchyard-server` and fake `claude`.
`gateway-probe` and `recall-probe` are the Rust test instruments for the
fuzz and the recall cases. Where the Rust binary rules as Go does, the
entry says so; the rest are its own.

**GW-R-1 (as GW-1 to GW-13).** Every Go ruling holds for the Rust
binary. It refuses the same case, `report float tokens` (GW-8), and no
other (`switchyard own config not toml`, GW-9, agrees since RF-5). GW-13's
compensated sum is now also in the Rust `distill-repeats`, which added
dollars with a plain `+=`.

Status: in force.
Retires when: does not retire.

**GW-R-2 (ruling): the HTTP stack.** The tree has no async runtime, so
the gateway uses the standard library's sockets and a thread for each
client connection, with keep-alive, `Expect: 100-continue` and chunked
request bodies. A stream is written with chunked framing, each upstream
read at once; both sockets set TCP_NODELAY. The upstream client speaks
HTTP/1.1 over TCP or over rustls (ring, the system root store, loaded
once), with no timeout and no redirect, as GW-6. It keeps up to 8 idle
connections for each host. A kept connection that the server closed
while it was idle is dropped before use. A request is sent again on a
new connection only when writing it to a kept connection failed, never
after it was written whole, so a turn is never sent twice. Crates added:
miniz_oxide with adler2 (the gzip and deflate decoders, pure Rust), and
num-bigint with num-traits as direct dependencies (already in the tree
through jiter). All are in `NOTICE`.

Status: in force.
Retires when: does not retire.

**GW-R-3 (ruling, replaced 2026-09-26): the upstream calls go through the
proxy httpx picks.** This ruling first made a proxy variable refuse the
gateway, since the binary had no proxy client. It now has one
(`netproxy`, PX-1 to PX-8 below): the gateway starts, and each turn goes
through the proxy httpx would use, or fails with 500 where httpx cannot
build its client (a SOCKS or unknown proxy scheme), as the oracle does.
Only a proxy setting the binary does not read the httpx way (PX-5) still
refuses the gateway before anything starts.

Status: retired 2026-09-26 by PX-1 to PX-8: its first form (a proxy variable refuses the gateway) is gone. The text above is its replacement, which PX-5 now governs.
Retires when: retired (see Status).

**GW-R-4 (ruling): the surrogate block.** The Rust tree holds a lone
surrogate as a character of a reserved block, U+10F800 to U+10FFFF
(`gate::py::text`). A request body that holds a real character of that
block, or an escape that decodes to one (`\udbfe`, `\udbff`), is not
decided: it goes upstream untouched and is not journaled, as a body the
oracle cannot route. An encoded lone surrogate (UTF-8 surrogatepass, or
UTF-16) is routed as the oracle routes it. The SSE sniffer and the
stores do not make this check; such a character in an answer would be
written back as a surrogate escape. It is a private-use character of
plane 16, and no case or fuzz input holds one.

Status: in force.
Retires when: does not retire.

**GW-R-5 (ruling): the request path.** The app sees the path
percent-decoded, as uvicorn gives it: `/_reload`, the wire and the
count_tokens path are decided on it. The path sent upstream is quoted
as httpx quotes it (an existing `%XX` kept, space, `"`, `#`, `<`, `>`,
`?`, backtick, braces and non-ASCII as `%XX` of their UTF-8), the query
as httpx quotes a query. A request target with a byte outside visible
ASCII is answered 400. Go forwards Go's own reading of the path; no case
holds a `%` escape.

Status: in force.
Retires when: does not retire.

**GW-R-6 (as Go): a compressed body that does not decode.** A gzip or
deflate answer that is cut short, or whose gzip checksum or length is
wrong, is an error: a buffered turn is answered 500, a stream is
dropped, and neither is journaled, as in Go. httpx would pass on the
part it decoded from a gzip stream that ends early. No case holds one.

Status: in force.
Retires when: does not retire.

**GW-R-7 (ruling): the Switchyard child.** It is forked from a thread
held for the life of the process, with `setsid`, `PR_SET_PDEATHSIG`
SIGTERM, and a check that the parent is still the gateway, as the Go
binary starts it. Checked by hand with a stand-in child: its signal
mask is empty, it leads its own session, it stops on the gateway's
SIGTERM (the gateway then dies of SIGTERM, exit 143 in a shell, the
state file removed), and a gateway killed with SIGKILL takes it along.

Status: in force.
Retires when: does not retire.

**GW-R-8 (ruling): big token counts.** The meter, the journal and the
report hold token counts exactly (num-bigint), as Python's int. The
reuse worklist in `gateway-report` adds each turn's tokens in 128 bits,
as `distill-repeats` does, and refuses a count past that range (exit 2,
nothing changed). No case or fuzz input reaches it.

Status: in force.
Retires when: does not retire.

## 2026-09-26: the Go `daisugi` CLI, the everyday commands (`status`, `config`, `start`, `journal`)

Cases: the new cases in `clients/cli_cases.py` (167 of the 472 in
`clients/fixtures/cli/`), run through the garden runner: journals and
stores laid out by the oracle's own classes, a fake `claude` answering
recorded replies, every database dumped whole, stderr compared on every
case, and the garden guard (Python reads every journal the binary
wrote). A ruling that changes the binary's answer is stored in the case
as `go_expect`, derived from the oracle's answer by the generator, so the
compare stays exact. Rulings and differences:

**C-10. What is refused, not answered.** A refused run exits 2 with one
line and writes nothing. `start` without `--no-ui` or `--dry-run` (the
view, C-11); `journal search` under MiniLM or int8 (C-15); `journal
parse` when a split over `api` would go through a proxy setting the
binary does not read as httpx does (C-16); `journal ingest` of a file
that is neither in the form `yaml.safe_dump` writes nor JSON, or with `OPENDAISUGI_CONFORMANCE_RECORD`
set outside `--dry-run`; `journal ingest` and `journal replay --json`
when a stored result would carry a violation whose detail is not worded
(C-14); `config` and `status` on a `config.yaml` outside the YAML reader
(C-7) or with a key that is not a string (its `str()` is not kept); a
transcript value of a type where the oracle raises or prints a repr (a
tool name that is not a string, a Bash input that is not a mapping, a
Codex `arguments` that is not a string). Where Python itself fails on
such input it exits 2 or 1 with its own text; the binary does not guess
that text. Cases refused today: MiniLM and int8 search, a potion model that is
not there, a replay whose predicate violation has no worded detail, and
two `config` shapes.

Status: in force; its refusal of the split over `api` (C-16) retired 2026-09-27 by the api split (843bf76b); its refusal of a transcript that is not UTF-8 and of a transcript value where the oracle raises or prints a repr retired 2026-09-28 by F-3, for the Go binary and for the Rust binary.
Retires when: each refused shape is answered the Python way.

**C-11 (ruling): `start` launches nothing and has no view.** Python's
`start` detaches one program, `python -m opendaisugi.cli gate serve
--root <data>/gate`, with the environment it was given, then waits two
seconds for `gate.sock`; after the steps it opens the TUI or the live
view in process. The oracle cases run it with that spawn replaced by a
fake that logs the argv and makes the socket, and the views by a line on
stderr. The binary's hook runs `daisugi gate check`, which answers each
call in process and never asks a resident gate, so it starts none: the
step reads `gate-server  skipped  not needed: this binary's gate hook
answers each call in process` (with Python's text when a socket is
already there), and no socket is made. The view (`daisugi dashboard`) is
not in the binary: the step reads `view  skipped  the multi-session view
(daisugi dashboard) is not in this binary yet`, and `start` without
`--no-ui` or `--dry-run` is refused before any step runs. `start
--enforce --ask` writes the hook with `--ask`, which `gate check`
answers natively; `install --ask` stays refused (C-8) only because it is
not ported yet.

Status: retired. Its "`start` launches nothing" retired 2026-09-27 by G2-4 (Go) and C-R (Rust); the view retired for the Go binary 2026-09-28 by K4-5, and for the Rust binary 2026-09-28 by K4-R-1.
Retires when: the view (`daisugi dashboard`) is in the binaries (stage K4).

**C-12 (ruling): `status` and pathway reuse.** Python reports the
`[search]` extra as installed when `sentence_transformers` imports, and
then token savings as live when pathways exist. The binary reuses
pathways only under `lexical` and `potion`; under MiniLM (the default)
or int8 it reuses none, so it says `✗ matcher <key> is not in this
binary — pathways disabled; set matcher_model: lexical or potion`, and
`search_extra_installed` and `token_savings_ready` are false. Under
lexical or potion the two agree. The matcher is read from
`~/.opendaisugi/config.yaml`, as the pathway commands read it.

Status: in force.
Retires when: the oracle's default matcher is lexical or potion and MiniLM and int8 leave it (rulings DS-3 and DS-4), or the binaries carry those matchers.

**C-13 (ruling): `status` reads the GPU from nvidia-smi only.** The
oracle asks torch first (`total_memory / 1e9`), then nvidia-smi (MiB /
1024). The binary carries no torch, so it asks nvidia-smi only. With CUDA
visible to torch the two differ by about 7% and the budget can round to
another whole number. The cases hide CUDA from torch and put a fake
`nvidia-smi` first on PATH. The memory size comes from `/proc/meminfo`
(the oracle's environment has no psutil; psutil reads the same line); a
case that depends on it is `live`: the compare asks the oracle again.

Status: in force.
Retires when: the oracle stops asking torch for the GPU.

**C-14 (ruling): violation details.** A trace stores each violation's
`detail` and `suggested_remediation`. The binary's verifier now words
them for the permissions stage (shell metacharacters, with the
decomposition remediation and the refusal reason; heads, interpreters,
redirects, recursion depth; network scheme and host; file paths; MCP;
agentic steps; unknown types) and the DAG stage, and keeps `warnings` in
the oracle's order and words (an opaque skill, a tautology, a timeout).
Other stages' details (Z3, predicate, robotics, delegation) are not
worded: `journal ingest` refuses an episode that would be journaled
with one, and `replay --json` a replay that holds one. The text form of
`replay` prints stage and message only and is never refused.

Status: in force.
Retires when: the binaries' verifier words every stage's details.

**C-15 (ruling): `journal search`.** The binary carries the lexical and
potion matchers and refuses MiniLM and int8 before anything is made.
Scores are cosines as the oracle computes them (row norms summed in
numpy's pairwise order), but the dot product is a plain sum where numpy
calls BLAS, so two scores that differ in the last bits may order either
way. Equal scores are the larger issue: numpy's `argsort` on this box
uses a SIMD sort that does not keep order among ties, and its order
differs between CPUs. The binary keeps ties in `list_recent` order. The
cases never cut a group of equal scores with `--limit`.

Status: in force.
Retires when: the oracle's default matcher is lexical or potion and MiniLM and int8 leave it (rulings DS-3 and DS-4), or the binaries carry those matchers.

**C-16 (ruling): `journal parse` splits through both backends.** An
episode over `--max-tools` is split by one model call. On claude-code: one
`claude -p --model=haiku [DAISUGI_CLAUDE_ARGS]` call, the prompt on
stdin, 60 s, in a fresh directory, as the oracle makes it; every failure
keeps the episode whole. The oracle leaves its fresh directory in
`$TMPDIR`; the binary removes it. On `api`: one
`llm_client.complete(model, [system, user], json_object=True)` with the
`--model` value (default `anthropic/claude-sonnet-4-20250514`), with the
oracle's request bytes (`response_format` goes on the chat wire only),
the reply's text read by `json.loads` and then `body.get("subtasks",
[])`. The oracle makes no preflight, no re-ask and no cut check here, and
every failure fails the whole parse: no key, no wire for the model, an
HTTP status, an unreadable reply, a text that is not JSON (worded as
`json.loads` words it) or not an object (`'list' object has no attribute
'get'`) prints `Parse error: <text>`, exits 2 and writes no file. A
proxy setting the binary does not read as httpx does is refused before
any call, and a reply nested past 900 levels is refused. Cases: `journal
parse split api *`. The Rust binary splits the same way (C-R). (Until
2026-09-27 the split over the HTTP backend was refused.)

Status: in force; its refusal of the split over the HTTP backend retired 2026-09-27 by the api split (a30e2597, 843bf76b).
Retires when: does not retire.

**C-17. Silent warnings.** `status` logs a store or journal it cannot
read, and an invalid `local_tier1.json`, on a logger with no handler, so
nothing is printed; the binary prints nothing either. Found on the way:
`config.Load` read a path through a file (`ENOTDIR`) as unreadable YAML
and refused; `Path.exists()` is false there, so it is now the defaults,
as in Python (case `status data dir is a file`).

Status: in force.
Retires when: does not retire.

**C-18. Found, not fixed: an invariant that does not parse.** An
envelope invariant whose `expr` is not a predicate the oracle's models
read (for example `{"op": "not", "arg": ...}`, where `Not` takes
`child`) makes the oracle's `verify()` raise `ValidationError`, so
`journal replay` ends with a traceback, exit 1. The Go verifier answers
a predicate violation instead (`evaluation error: empty expression`),
and `replay` reports drift. Both refuse the call; neither allows it. The
envelope cannot be registered in that form (`gate register` validates
it), so it reaches a verifier only from a hand-written journal. It is a
verifier difference outside this stage, and no case holds it.

Status: in force (open).
Retires when: the Go verifier raises the oracle's `ValidationError`, as the Rust one does (C-R-1).

**C-R (ruling): the Rust `status`, `config`, `start` and `journal`.** The
Rust binary carries the four commands as the Go binary does, with C-10
to C-17 as written: on the 498 CLI cases both give 478 agree, 11 not
ported and 9 refused, the same cases in each class, and 0 disagreements.
`start` launches its own `gate serve` as G2-4 says, and the compare runs
it behind the same wrapper. Where the two ports differ:

- **C-R-1 (the oracle's answer): a verifier that raises.** The Rust
  verifier reads an envelope predicate with the oracle's models, so the
  C-18 invariant makes it raise the oracle's `ValidationError`: `journal
  replay` exits 1 naming that exception, with the same error lines
  Python prints, where Go reports drift. Checked by hand on a journal the
  oracle wrote; no case holds it.
  Status: in force.
  Retires when: C-18 retires.
- **C-R-2 (refused): ingest when the verifier raises.** `journal ingest`
  refuses an episode whose verify raises, before anything is written. An
  episode file cannot hold such an envelope (ingest infers it), so this
  guards against a verifier fault only.
  Status: in force.
  Retires when: does not retire.
- **C-R-3 (the same answer, found on the way): a child is waited for, not
  polled.** `status` asks nvidia-smi and `journal parse` asks `claude`,
  each with a time limit; the wait is on a thread with the limit on a
  channel, and a child past it is killed, as Go's context kill does.
  Status: in force.
  Retires when: does not retire.

Status: in force.
Retires when: does not retire.

## 2026-09-26: proxies for every HTTP client (Rust and Go)

Every HTTP client in the Rust and Go binaries now goes through the proxy
the environment names, as the oracle's own client for that call does:
the gateway's upstream, the model client (`tend` and the other model
callers), the gate's `llm_check`, the potion download and the Switchyard
health probe. Rust has one connector for all of them
(`clients/rust/src/netproxy.rs`); Go has `internal/netproxy`, which sets
`Proxy` for an http request and does the CONNECT itself in
`DialTLSContext`. The proof is the proxy cases in the gateway and garden
suites, against a fake proxy (`clients/fake_proxy.py`) on 127.0.0.1 that
records each CONNECT line with its headers, each request line with `host`
and `proxy-authorization`, and the TLS server name a client sends into a
tunnel. It never connects to a host a URL names.

**PX-1 (ruling, the brief overruled): no bypass for loopback.** The brief
asked for loopback to go direct. httpx, urllib and aiohttp have no such
rule: a request to 127.0.0.1 or localhost goes through the proxy unless
`NO_PROXY` names it, and so does the gateway's call to a Switchyard child
and the health probe. A bypass would disagree with the oracle, and the
fake proxy (itself on 127.0.0.1) would see nothing. Go's
`http.ProxyFromEnvironment` skipped loopback; that is gone.

Status: in force.
Retires when: does not retire.

**PX-2 (ruling): three rule sets, by the oracle's client.** All three
read the variables as `urllib.request.getproxies` does: any
`<scheme>_proxy` in any case, a name ending in lower-case `_proxy`
winning, an empty lower-case one removing the scheme, and `HTTP_PROXY`
dropped when `REQUEST_METHOD` is set.

| | httpx (gateway upstream, `llm_check`) | aiohttp through litellm (model client) | urllib (potion download, health probe) |
|---|---|---|---|
| `ALL_PROXY` | yes, after the scheme's own | yes, after the scheme's own | no |
| `NO_PROXY` | each entry a URL pattern: `*` anywhere drops every proxy; `x.com` matches it and its subdomains, `.x.com` only subdomains; an IP matches that address (`/16` is read as a path, so a CIDR matches only its network address); a port must match, and a default port is dropped from the URL first; `http://h` only that scheme | urllib's suffix match on the host alone | a whole value of `*`; else suffixes, a leading dot ignored, matched against the host and against `host:port` |
| https | CONNECT: `Host`, `Accept: */*`, credentials; any 2xx; IPv6 target bare | CONNECT: `Host` without port 443, aiohttp's `Accept`, `Accept-Encoding`, `User-Agent`, credentials; only 200; IPv6 in brackets | CONNECT: credentials, `Host`; only 200; IPv6 in brackets; the proxy URL's scheme ignored |
| credentials | when user or password is set | when user or password is set | when both are set |
| proxy scheme | http, https (TLS to the proxy); SOCKS and others fail (PX-3) | https is TLS to the proxy; any other is an HTTP proxy | always plain TCP for https |

The model client follows aiohttp, not httpx: the oracle's model call is
`litellm.acompletion`, whose transport is `LiteLLMAiohttpTransport`, and
the fake proxy saw aiohttp's own CONNECT headers. The gate's `llm_check`
calls `litellm.completion`, which is httpx.

(2026-09-27: retired by LLM-11. The model client is now our own httpx
client, so every model call follows the httpx column; the aiohttp column
and its code in both ports are gone.)

Status: in force for the httpx and urllib columns; the aiohttp column retired 2026-09-27 by LLM-11 (litellm removed).
Retires when: does not retire.

**PX-3 (ruling): SOCKS and unknown schemes.** Without the socksio
package, httpx raises `ImportError` when it builds a client with a SOCKS
proxy in its mounts, and `ValueError` (`Unknown scheme for proxy URL
URL('...')`) for a scheme it does not know, for every request, even one
the proxy would not carry. The oracle's gateway builds its client for
each turn, so it starts and answers each turn 500; so do the binaries.
The gate leaves such an `llm_check` undecided. aiohttp sends a SOCKS URL
a CONNECT as if it were an HTTP proxy, and so do the binaries; urllib
tunnels through any proxy URL with a CONNECT.

Status: in force; its aiohttp sentence retired 2026-09-27 by LLM-11.
Retires when: does not retire.

**PX-4 (ruling): error texts.** A refused CONNECT is `403 Forbidden` in
httpx (httpcore's `ProxyError`), which the gate's litellm words as
`litellm.InternalServerError: AnthropicException - 403 Forbidden. ...`.
Through aiohttp it is `403, message='Forbidden', url='<proxy URL>'`, the
credentials in the URL redacted by litellm's secret pattern
(`http://REDACTED@...`). aiohttp words a connection that fails with the
repr of an SSLContext, a memory address, so the binaries do not copy it
and no garden case has one; the gateway cases hold the unreachable
proxy (500 on every side). A tunnel whose TLS fails (the fake proxy has
no trusted certificate) is compared by what the proxy saw, not by the
error text.

Status: in force for the gateway and the TLS note; its model-call texts (litellm's and aiohttp's) retired 2026-09-27 by LLM-8 and LLM-11.
Retires when: does not retire.

**PX-5 (ruling): what is refused.** A proxy setting the binary does not
read the oracle's way is refused, never guessed: two variables whose
names differ only in case with different values, when the binary holds
the environment as a map (the order decides in Python); a `NO_PROXY`
entry with `%`; a proxy URL whose host or port does not parse; a
non-ASCII host sent through a proxy (httpx would encode it as IDNA). The
gateway then says `daisugi gateway with this proxy setting (...) is not
in this binary yet.` and exits 2 before anything starts; `tend` refuses
before it reads anything when the names clash, and otherwise fails the
model call with the binary's "this model call is not in this binary ..."
text (LLM-11; before, "only the claude-code backend ..."); the gate
leaves the `llm_check` undecided. No case holds one.

Status: narrowed 2026-10-07 by PX-9: the case clash is read in the
environment's order, a NO_PROXY entry with `%` and a proxy URL httpx
rejects are read as httpx reads them; what stays refused is listed in
PX-9.
Retires when: the binaries read each refused setting the oracle's way.

**PX-6 (ruling): litellm's cost map.** Importing litellm fetches its
model cost map from raw.githubusercontent.com over https, through
`HTTPS_PROXY` or `ALL_PROXY` when one is set, and falls back to the map
it ships when that fails. The two maps differ: under the shipped one the
model client asks for `max_tokens` 64000, under the fetched one 4096. So
a garden case that sets `HTTPS_PROXY` or `ALL_PROXY` makes no request
that reaches the model, and sets `LITELLM_LOCAL_MODEL_COST_MAP=True` so
the fetch does not go through the fake proxy. The binaries do not fetch
it. Found, not fixed: every other litellm case lets the oracle fetch the
map from GitHub, and its `max_tokens` depends on whether that works. (2026-09-27: retired by LLM-6 and LLM-11. litellm is gone, and so is
the cost map.)

Status: retired 2026-09-27 by LLM-6 and LLM-11: litellm and its cost map are gone.
Retires when: retired (see Status).

**PX-7 (ruling): the probe's count.** The gateway polls a Switchyard
child's `/health` until it answers, and each poll goes through the proxy;
how many polls happen is timing, so that case keeps each distinct proxy
record once.

Status: in force.
Retires when: does not retire.

**PX-8 (ruling): names in direct cases.** Go's resolver (`netgo`) reads
`/etc/hosts` but not the `myhostname` NSS module that answers
`*.localhost`, so a case whose request goes direct uses `localhost` or
`localhost.localdomain`. A name that only the proxy sees (`api.localhost`)
is never resolved by any side.

Status: in force.
Retires when: does not retire.

## 2026-09-26: fail-open and privacy fixes (security queue 1)

Each fix is in the oracle first, then in Go and Rust where they have the
code, with the cases the oracle wrote.

**SEC-1 (ruling): subsumption checks every field that grants authority.**
`envelope_subsumes` now also fails when the inner envelope's stakes are
lower than the outer's (low < medium < high < physical; a value a port
does not rank fails), when its `custom_step_allowlist` names a type the
outer's does not (exact names, as `verify` matches them), or when its
`max_execution_time_s` or `max_output_size_mb` is larger. A callee that
leaves a budget unset has the default (30 s, 10 MB), not its caller's.
Go reads `max_output_size_mb` as a number from its text (`10.0` and `1e1`
equal 10); Rust holds both budgets as `i64`, and a value out of range
refuses the envelope. `shell_interpreter_policy` is still read from the
outer only; a child that loosens it is not caught here.

Status: in force.
Retires when: does not retire.

**SEC-2 (ruling): a subsumption timeout denies, whoever calls.** The
oracle's `envelope_subsumes` returns a result that does not hold, marked
`timed_out`, where it raised `VerificationTimeout`, so `SafeSubagent` and
swarm deny it rather than let a raise escape. `check_skill_delegations`
makes a delegation timeout a violation in every mode and at every stakes
level (it was a warning under lenient mode). The violation text is the one
the three already shared: `verifier timed out (skill-delegation
subsumption); raise the Z3 timeout`. The timeout text is still kept where
each keeps it (Go `Timeouts`, Rust `Outcome.timeouts`, the oracle's
`z3_message`), so a pathway import still names VERIFICATION_TIMEOUT. The
lenient allow of an opaque skill (no contract envelope) is unchanged.

Status: in force.
Retires when: does not retire.

**SEC-3 (ruling): a payload's session id never selects an envelope.** In a
shared gate root, an unpinned call that named another session's id was
checked against that session's envelope, and the checked agent can start
a harness with any session id it likes; a pin with no envelope fell back
to `default`. Now: unpinned, every call is checked against `default`;
pinned (`--session`), against the pin's own envelope or none, which
denies. The payload's id is still what the shadow log, the session tree
and the reports are keyed by. `gate init --session S` prints a settings
command that carries `--session S`, quoted as `shlex.quote` does.

Status: in force.
Retires when: does not retire.

**SEC-4 (ruling): an operator's edit passes the whole gate again.** When
an operator allows a call with an `updatedInput`, the gate runs the edit
through `evaluate_call` again (pane rule, hard-deny rules, envelope). A
would-deny there is the decision: its reason after `the operator's edit
does not pass the gate: `, its tier and fields, `ask` true, the operator's
name kept, no edit, and no second ask. An edit that is not a JSON object
is denied with `the edit is not a JSON object`. An edit the envelope does
not admit is denied even though the operator allowed the original call:
the operator approved what they saw, not a new call. The gate cases now
hold a fake operator that answers the posted ask with its nonce.

Status: in force.
Retires when: does not retire.

**SEC-5 (ruling): no agent runs `daisugi rank record`.** A hard-deny rule,
before the envelope, in every mode, not turned by an ask; refusal `only the
owner records a ranking vote. Record it yourself.` The rule is stricter
than "as the owner": it denies every `rank record`, whatever `--judge`
says, since any vote an agent types is a vote it forged (model votes come
from `rank judge`). It reads words: backslashes and quotes are dropped,
then a word is a run of ASCII letters, digits and `_./:=+@%,-`, and any
other character, non-ASCII included, ends a word. A line hits when a word
whose last path part is `daisugi` or `opendaisugi`, or starts with
`opendaisugi.`, comes before `rank`, which comes before `record`. So an
`echo` of those words is denied too. Words the shell builds at run time
(variables, substitution, eval) are not seen.

Status: in force. Since 2026-10-01 the line is read per simple command
(SW-2); the word reading above is its fallback.
Retires when: does not retire.

**SEC-6 (note, for the grafts work): Codex shares `--format claude`.**
`_patch_codex_gate` reuses the Claude Code gate entry. That is right for a
deny (exit 2) and for an operator edit today: Codex honors `updatedInput`
only with `allow`, which is what the claude shape sends. A graft that
rewrites a call the envelope did not already allow needs its own
`--format codex`, since one shape cannot follow both hosts' rules. No
code change here.

Status: in force.
Retires when: a graft that rewrites a call gets its own `--format codex`.

**SEC-7 (ruling): the oracle does not phone home.** `import opendaisugi`
sets `LITELLM_LOCAL_MODEL_COST_MAP=True` (keeping a value the user set)
before any litellm import, so litellm uses the cost map it ships and never
fetches one. This closes the "found, not fixed" part of PX-6: every
litellm case now runs under the shipped map. The 23 litellm garden cases
were rerun (max_tokens 64000 for claude-sonnet-4-20250514, where the
fetched map gave 4096), and the Go and Rust max_tokens tables were
regenerated from the shipped map; their generators set the variable
before they import litellm.

Status: retired 2026-09-27 by LLM-15: litellm is gone, so nothing needs `LITELLM_LOCAL_MODEL_COST_MAP`.
Retires when: retired (see Status).

**SEC-8 (ruling): aliases (oracle only; Go and Rust carry no registry).**
Substitution is one pass per string, longest name first, and a value put
into a regex field is escaped. A system alias's name cannot be registered
again at any tier, in either order, and a household file cannot claim the
system tier. An error in the register-time vacuity check refuses the
register; a body with a typed placeholder or a reference to another alias
is deferred by rule.

Status: in force.
Retires when: does not retire.

## 2026-09-27: the resident gate in Go and Rust (`daisugi gate serve`)

Cases: `clients/fixtures/gate_server/` (200: 190 requests and 10
scenarios, written by `clients/gate_server_cases.py` from the Python
server). `clients/gate_server_compare.py --binary B --port go|rust`
starts the binary's `gate serve` as the cases start the oracle's and
compares the reply bytes, the shadow log, the session tree, the captures
mirror and every line sent to a fake coppice or a fake herdr. Rulings and
notes, each checked against the oracle:

**G2-1 (ported): the server.** Both binaries serve `<root>/gate.sock` as
`gate_server.py` does: the root made with its parents and set 0700, a
stale socket or file at the path unlinked (G2-9), the socket 0600, one JSON
request line per connection (read to its newline, to 4 MiB, or to the
client's half-close) and one reply line, `{"v": 1, "stdout", "stderr",
"exit_code"}`, as `json.dumps` writes it. The request is read as
`json.loads` reads bytes (a UTF-8 BOM dropped, an encoded lone surrogate
kept, 9,992 nested containers at most, as the oracle's request thread
allows), argv as `str()` of each item (a string's characters, an
object's keys), stdin with the lenient base64 decode. `hook report` is
served in the server process; every other request is decided in a child
of the binary, fed the request on stdin (argv words that exec cannot
carry, a NUL or a lone surrogate, go through), with the decision `gate
check` gives. A crash or a hang of the child is the format's deny
(`hermes` and `openclaw` get their block body), never a reply that
allows, and never ends the server. `--help` in argv ends the connection
with no reply, as the oracle's `SystemExit(0)` does. Ctrl-C prints a
newline, SIGTERM prints nothing; each removes the socket file and exits
0 (G2-9). A SIGINT the process was started with ignored stays ignored; a
SIGTERM does not, since the Go runtime does not keep it ignored. A
second server on a root a live server answers on exits 1 (G2-9). The
server drops `COPPICE_SOCK`, `COPPICE_PANE`,
`HERDR_PANE_ID` and `HERDR_PANE` from its own environment.

Status: in force.
Retires when: does not retire.

**G2-2 (ruling, stricter): a request is UTF-8.** `json.loads` on bytes
also reads UTF-16 and UTF-32, found by a BOM or by zero bytes. The
binaries read UTF-8 only (with or without the BOM), so a UTF-16 or UTF-32
request is a bad request. No client sends one. Case `utf16 half close`
holds the ruling as `port_expect`; with a newline the oracle's own read
already cuts such a request apart (case `utf16 newline`).

Status: in force.
Retires when: does not retire.

**G2-3 (ruling, Rust, as RG-12): a `--verify-timeout` that is not finite
is not decided over the socket either.** Cases `argv nan` and `argv
infinity` (argv items that are JSON numbers, whose `str()` is `nan` and
`inf`) hold the Rust deny as `rust_expect`. The Go binary answers them
as the oracle does.

Status: in force.
Retires when: RG-12 retires.

**G2-4 (ruling, replaces the gate-server step of C-11): the Go `start`
launches the resident gate.** pi and OpenCode ask the gate over the
socket, whatever the hook does, so `start` detaches the binary's own
`gate serve --root <data>/gate` (a session of its own, no stdin, stdout
or stderr, this environment) where Python detaches `python -m
opendaisugi.cli gate serve`, waits two seconds for the socket, and words
the step as Python does. The binary names itself by the path it was run
by. The compare runs `start` cases behind a wrapper script that logs that
launch and makes the socket file, as the oracle's fake launch does; the
case's `go_expect` names `{DAISUGI} gate serve --root R` where the oracle
names Python. The view stays out of the binary (C-11). The Rust binary
has `start` too (C-R), with the same launch; its `install --harness`
line is now Python's, word for word, and the compare checks that line.

Status: in force.
Retires when: does not retire.

**G2-5 (note): the caller is who the request names, checked.** As in the
oracle: the payload's session id never selects an envelope, only the
pin (SEC-3; cases `sec3 *`). `coppice_sock`, `coppice_pane`,
`herdr_pane` and `coppice_data_dir` count only as strings. A resident
call's event names no harness session id and no transcript path, and
its pane only when the claim is shaped like a coppice pane id. The report
goes to coppice only when the socket is an absolute path to a socket this
uid owns, the peer on the live connection is this uid (SO_PEERCRED), and
the hello's reply places the connection as the claimed pane; the hello
carries the client's real pid, read with SO_PEERCRED when the server
accepted it (`peer_pids`, `<client pid>` in the cases). Else herdr, when a
herdr pane is named. Never the server's own pane. The caller's data
directory is guarded beside the server's own `COPPICE_DATA_DIR`, never
instead of it.

Status: in force.
Retires when: does not retire.

**G2-6 (note): what is not the contract.** The oracle's server prints
argparse's usage for a bad argv and a traceback for a client that left
before its reply (a `BrokenPipeError`) on its own stderr; the binaries
print nothing per request. A root that cannot be served (a file, or a
directory where the socket goes) exits 1 after the serving line in all
three; the rest of stderr is each language's own (Python: a traceback).
The scenarios compare the first line and the Ctrl-C newline only.

Status: in force.
Retires when: does not retire.

**G2-7 (note): limits the oracle does not have.** A call's child is
bounded by the deadline `gate check`'s child has (the verify budget, the
ask budget with `--ask`, and 3 s), and past it the call is denied; the
oracle's server has no bound. At most 32 calls are decided at once; a
call past that waits. Neither changes a verdict the oracle gives in time.

Status: in force.
Retires when: does not retire.

**G2-8 (fixed): the backlog is the system's.** `socketserver` listened
with a queue of 5, so a burst of clients could find it full: a client
with a timeout set (pi's, gate_client's) then failed to connect at once
and read the gate as unreachable, a deny. The oracle now listens with
`socket.SOMAXCONN`, as the binaries listen with the system's backlog. The concurrent scenario's clients connect again on
a full queue, so the case records the verdicts, not the queue. The oracle
also reads a request with no timeout: a client that connects and sends
nothing holds a thread until it leaves. The binaries do the same.

Status: in force.
Retires when: does not retire.

**G2-9 (fixed): a socket is a running gate only when it answers.**
Ctrl-C left `gate.sock` behind, and `start` took any file at that path
for a running server, so after a Ctrl-C it started nothing, and pi and
OpenCode found the socket but no server: a deny, never an allow. Now
`start` and `gate serve` connect to the socket first (a one-second
timeout; the probe sends an empty request and reads the reply). A
connection, or a full queue, is a live gate: `start` says it is already
running, and `gate serve` prints `gate: a resident gate already answers
on <sock>; stop it first` and exits 1 before the serving line, the live
socket untouched. Anything else there (the socket of a dead server, a
regular file) is stale: `start` removes it when it acts, never on a dry
run, and starts a server; `gate serve` removes it and serves. The
server removes its own socket on Ctrl-C, SIGTERM and stop, and only
while the path still holds the socket it bound. Python, the Go binary
(`start` and `gate serve`) and the Rust binary (`gate serve`). The
`start` cases hold a real listener for a running gate (`live_gate`) and
add a stale file; the scenarios `second server`, `sigterm`, `sigterm
ignored` and `lifecycle` hold the rest.

Status: in force.
Retires when: does not retire.

## 2026-09-27: our own model client (litellm and instructor dropped)

The oracle's model calls went through litellm (`acompletion`, `completion`)
and instructor's JSON mode. Both are gone. One module,
`opendaisugi/llm_client.py`, now makes every model call over HTTP; the
claude CLI backend (`claude_code_llm.py`) is unchanged. The new module
defines the request bytes. Every fixture that records a model request was
made again from it, and the Go and Rust clients send the same bytes.

**LLM-1 (ruling): the oracle defines the request.** Dropping litellm and
instructor changes what the oracle sends, so the old bytes (litellm's body,
its per-model `max_tokens`, its banner, instructor's log lines) are not
kept. The garden, gate and llm_check fixtures were made again from the new
oracle; the binaries changed to match. The gateway sends no model call of
its own, so its fixtures did not change.

Status: in force.
Retires when: does not retire.

**LLM-2 (ruling): the model name picks the wire.** `anthropic/<m>` goes to
the Anthropic Messages API as `<m>`, and so does a bare name that starts
with `claude` (sent as is). `openai/<m>` goes to OpenAI-compatible chat
completions as `<m>`. `ollama/<m>` and `ollama_chat/<m>` go to Ollama's
OpenAI-compatible path as `<m>`. Any other name fails before a request:
`no model wire for '<name>': name it anthropic/<model>, openai/<model> or
ollama/<model>` (the name as Python's `repr` writes it). litellm also
routed a bare `gpt-4o`; the client does not guess.

Status: in force.
Retires when: does not retire.

**LLM-3 (ruling): URLs.** Each base has its trailing slashes removed.
Messages: the call's base URL, else the first non-empty of
`ANTHROPIC_API_BASE` and `ANTHROPIC_BASE_URL`, else
`https://api.anthropic.com`; `/v1/messages` is added unless the base
already ends with it (case matters). Chat completions for `openai/`: the
call's base, else `OPENAI_API_BASE`, else `OPENAI_BASE_URL`, else
`https://api.openai.com/v1`; `/chat/completions` is added unless present.
Ollama: the call's base, else `OLLAMA_API_BASE`, else
`http://localhost:11434`; kept when it ends with `/chat/completions`,
`/chat/completions` added when it ends with `/v1`, else
`/v1/chat/completions` added. The client reads no config file for URLs;
`llm_base_url` stays with the gateway and `tiers setup`.

Status: in force.
Retires when: does not retire.

**LLM-4 (ruling): credentials.** Messages: the call's API key, else a
non-empty `ANTHROPIC_API_KEY`, sent as `x-api-key`; else a non-empty
`ANTHROPIC_AUTH_TOKEN`, sent as `authorization: Bearer <token>`; else the
call fails before a request with `no ANTHROPIC_API_KEY or
ANTHROPIC_AUTH_TOKEN is set`. An empty `ANTHROPIC_API_KEY` counts as unset
(litellm sent no header for it). Chat completions: the call's key, else,
for `openai/` only, a non-empty `OPENAI_API_KEY`, sent as `authorization:
Bearer <key>`; else no credential header. Ollama gets only the call's key.

Status: in force.
Retires when: does not retire.

**LLM-5 (ruling): the request.** A POST with these headers: `accept:
application/json`, `accept-encoding: identity` (no compressed reply to
read), `content-type: application/json`, `user-agent: opendaisugi`, then
for Messages `anthropic-version: 2023-06-01`, then the credential. The body
is JSON with no spaces and every non-ASCII character escaped
(`json.dumps(..., separators=(",", ":"), ensure_ascii=True)`), so a lone
surrogate in a prompt cannot fail the encoding. Messages keys, in order:
`model`, `max_tokens`, `system` (every system message's text, joined by a
blank line; left out when there is none), `messages` (role and string
content), then `temperature` and `thinking` when the call gives them. Chat
keys: `model`, `messages` (the system message kept in place), then
`max_tokens`, `temperature`, `response_format` (`{"type":"json_object"}`
for a structured call or a JSON-mode plain call) and `reasoning_effort`,
each only when set.

Status: in force.
Retires when: does not retire.

**LLM-6 (ruling): `max_tokens`.** Messages: the call's value, else 8192
(plus the thinking budget when the call asks for thinking). Chat: the
call's value, else none. litellm's per-model table (64000 for most Claude
names, 4096 when its cost-map fetch worked) is gone, and with it PX-6.
llm_check still asks for 200; the delegating executor still derives its
cap from `max_output_bytes`.

Status: in force.
Retires when: does not retire.

**LLM-7 (ruling): the reply.** Any status from 200 to 299 is a reply; the
body is decoded as UTF-8 with replacement and read with `json.loads`.
Messages: an object whose `content` is a list; the text is the `text` of
each block whose `type` is `text`, joined; other blocks (thinking) are
skipped; `stop_reason` `max_tokens` means cut. Chat: an object whose
`choices` is a non-empty list with an object first, whose `message` is an
object whose `content` is a string or null (null is empty text);
`finish_reason` `length` means cut. Anything else is unreadable. Redirects
are not followed.

Status: in force.
Retires when: does not retire.

**LLM-8 (ruling): error texts, one per class.** No text holds an OS or
library message, so all three languages write the same line. Each text has
key-shaped tokens shortened (`sk-abcd...wxyz`) and any `user:pass@` in a
URL dropped.

| class | text |
|---|---|
| no credential | `no ANTHROPIC_API_KEY or ANTHROPIC_AUTH_TOKEN is set` |
| no wire | LLM-2 |
| a status outside 2xx | `the model server answered HTTP <status>: <body, first 500 characters>` |
| unreadable reply | `the model reply could not be read: <body, first 200 characters>` |
| connection failed, reset, TLS, bad URL | `could not reach the model server at <URL>` |
| timeout | `the model call to <URL> timed out` |
| the proxy refused the CONNECT | `the proxy refused the model call: <status> <reason>` |
| httpx cannot build a client for the proxy setting (PX-3) | `the model call could not start: <httpx's text>` |
| a cut structured reply | `The output is incomplete due to a max_tokens length limit.` |
| a structured call that never validated | `ValidationError: <first line of the first error> (<n> attempts)` |

Status: in force.
Retires when: does not retire.

**LLM-9 (ruling): structured output keeps instructor's texts.** The schema
text is instructor's JSON-mode text, byte for byte (its `dedent` template
around `json.dumps(schema, indent=2, ensure_ascii=False)`), added to the
first system message after a blank line, or sent as a new first system
message; then consecutive messages of one role are joined by a blank line.
A one-time check against instructor 1.16.0 matched every response model a
call site uses; `tests/test_llm.py` pins the template without instructor.
A call makes `max_retries + 1` attempts (instructor's default of 3 stays).
A reply is validated with `model_validate_json` and then `non_finite_error`
(GD-16). On a schema error the reply goes back as an assistant message,
followed by a user message `Correct your JSON ONLY RESPONSE, based on the
following errors:\n<the error>`. A cut reply fails at once. A transport or
HTTP error fails at once with its own text, even after a schema error;
instructor raised the first schema error there, which hid the real cause.

Status: in force.
Retires when: does not retire.

**LLM-10 (ruling): the client prints nothing.** litellm's "Give Feedback /
Get Help" banner on stdout and instructor's log lines on stderr are gone in
all three languages. A hook's stdout stays the verdict body.

Status: in force.
Retires when: does not retire.

**LLM-11 (ruling): proxies are httpx's.** Every model call reads the proxy
variables as httpx does (the httpx column of PX-2, PX-3 for SOCKS and
unknown schemes), since the client is httpx. The aiohttp column of PX-2
described litellm's model client and is retired, as is PX-6 (no cost map,
no `LITELLM_LOCAL_MODEL_COST_MAP`). The proxy cases in the garden were made
again: a refused CONNECT now carries httpx's CONNECT headers, and a SOCKS
proxy URL fails before any request.

Status: in force.
Retires when: does not retire.

**LLM-12 (ruling): the gate's llm_check.** It is a plain call through the
same client: the Messages or chat wire by `OPENDAISUGI_LLM_CHECK_MODEL`,
`temperature` 0, `max_tokens` 200, the reply's text read with `json.loads`
as before. A failure's reason is `error: llm_check call failed: <LLM-8
text>`. litellm's error mapping and its secret pattern are gone, and
settings named `LITELLM_*` or `MINIMUM_CUSTOM_KEY_LENGTH` no longer leave
a check undecided. `SSL_CERT_FILE` and `SSL_CERT_DIR` still do, since
httpx reads them.

Status: in force.
Retires when: does not retire.

**LLM-13 (ruling): the backend is named `api`.** The value of `--llm`,
`OPENDAISUGI_LLM_BACKEND` and `llm_backend` that selects our own HTTP
client is `api`, and auto-detection picks `api` where it picked
`litellm`. The rename is a clean break, with no fallback: the old name
`litellm`, from any rung and with surrounding whitespace stripped, is
refused with one line in all three languages, `The LLM backend 'litellm'
is now named 'api'. Use 'api'.` `--llm litellm` prints it and exits 2,
as any bad `--llm` value does. From the environment or `config.yaml`,
`resolve_backend` raises `LLMNotConfigured` with it: a command that
resolves the backend (the `backend:` note of `journal parse`) prints it
and exits 1, `tend` fails on it as on a missing key, and an `llm_check`
fails closed with it as the reason. `config` shows the value as set, since
it resolves nothing. Cases: `journal parse llm flag renamed`, `journal
parse renamed *`, `config env backend renamed`, `tend renamed backend*`,
`llm renamed backend*`. (Until 2026-09-27 this ruling kept the name
`litellm`.)

Status: in force; its first form, which kept the name `litellm`, retired 2026-09-27 by the rename (a30e2597).
Retires when: does not retire.

**LLM-14 (ruling): the timeout.** The call's timeout, else
`OPENDAISUGI_LLM_TIMEOUT` when it reads as a finite number above zero
(`float()` rules), else 600 seconds. httpx applies it to the connect and to
each read and write; the binaries apply it to the whole exchange. No case
depends on the difference. litellm's `REQUEST_TIMEOUT` is no longer read.

Status: in force.
Retires when: does not retire.

**LLM-15 (ruling): the `generate` extra holds httpx only.** litellm and
instructor are gone from every extra. The HTTP client needs httpx, which
was not a base dependency, so `[generate]` now lists it alone; the
claude-code backend needs nothing extra. Importing any module that calls a
model opens no socket and loads no httpx (`tests/test_lazy_imports.py`,
`tests/test_llm_client_offline_import.py`).

Status: in force.
Retires when: does not retire.

## 2026-09-27: the offline verifiers link Z3

**VZ-1 (ruling): no verifier runs a `z3` program.** The Go offline
verifier (`internal/verify`, which `conform` and every `daisugi` command
that verifies use) sent its Full-profile queries to a `z3 -in` process,
unless a binary linked Z3 and said so; `conform` did not, and the CI job
installed the `z3` command for its tests. It now sends the same SMT-LIB2
text to the Z3 5.1.0 it links, through Z3's command interpreter
(`Z3_eval_smtlib2_string`), as the gate and the Rust verifier already do.
A solver that fails is still a `z3` violation (fail closed). The spec's
"invoke a solver binary, never bind a solver API" now reads "hand the
SMT-LIB2 text to a solver that reads it" (`docs/spec/conformance.md`).
Checked with no `z3` on `PATH`: the Go verifier tests, and the frozen
corpus (11,450 cases) through the Go and the Rust `conform`, 0
disagreements each.

Status: in force.
Retires when: does not retire.

## 2026-09-27: stage K1 in Go (model-written envelopes, Tier-0, bind, compose)

**K1-1 (ruling): what carries K1 in Go.** The one command of stage K1 is
`daisugi generate-envelope`, now in the Go binary. The rest of
`generate_envelope` (the pathway lookup, the Tier-1 slot, the envelope
cache and its refinement hints, ladders, inheritance), `bind_parameters`
and `compose` have no command in the oracle yet: the orchestrator and the
MCP server call them (stages K2 and K3). They are the Go library
`internal/envgen`, proven query by query through `cmd/envelope-probe`
against a script that calls the oracle's functions
(`clients/k1_cases.py`, 180 cases). `tiers setup` (hardware sizing and
the Tier-1 qualification run) is onboarding and goes with K3.
`ClaudeCodeTier1Provider` is not ported: no config or command builds it.
The probe's composition runs each step through an echo executor; the real
executors are K2.

Status: in force.
Retires when: K2 and K3 put commands on these paths; their cases then run
them end to end.

**K1-2 (Go-side, fails closed): self-consistency that did not finish.**
`generate_envelope` treats any exception from
`check_envelope_self_consistency`, a Z3 `unknown` included, as "no
violations", and so accepts the envelope (a Tier-1 envelope and a Tier-2
rung alike). The port rejects such an envelope: the rung fails, or the
Tier-1 slot declines. The check is ground and ends well inside its 500 ms,
so no case reaches it; a Go test makes the check answer `unknown` and sees
the envelope refused.

The oracle now fails closed the same way: an exception from the check
fails the rung with the exception's text, and the Tier-1 slot declines
(`tests/test_envelope_tiered_routing.py`, `tests/test_envelope_tier1_routing.py`).

Status: retired 2026-09-27 by the oracle fix and its tests (named above).
Retires when: retired (see Status).

**K1-3 (ruling): bind and recall fail closed on a Z3 `unknown`.**
`bind_parameters` keeps a bound plan whose verify kept a Z3 timeout as a
warning, and `recall` serves it. The port gives the frozen template
instead, and `recall` then verifies that template the same strict way, a
miss if it too does not finish. This extends GW-11 to the bound plan.

Go tests make the verify answer `unknown` and see the template given and
the plan not served.

Status: in force.
Retires when: the oracle's bind and recall verify in strict mode.

**K1-4 (ruling): a binding into a field that is not a string.**
`apply_bindings` sets the bound value with `setattr`, which pydantic does
not validate: a hole whose field is a declared list, dict or literal
(`depends_on`, `metadata`, `type`) would take a string and leave a plan
that is not well formed. The port refuses such a binding and gives the
frozen template, as the oracle does for a field the step does not
declare. Both sides agree on every string field, which is every field the
distiller makes a hole of (`path`, `url`).

The oracle now refuses such a binding too: `apply_bindings` raises
`ValueError` for a field the step does not declare as `str` or
`str | None`, and `bind_parameters` gives the template. Four cases bind
into `depends_on`, `metadata`, `type` and `postcondition`.

Status: retired 2026-09-27 by the oracle fix and its tests (named above).
Retires when: retired (see Status).

**K1-5 (ruling): the order of a frozenset in an inheritance message.**
`verify_inheritance` names the invariants or postconditions a child drops
as `frozenset({...})`, whose order follows the string hash, which Python
seeds per process. With two or more members the oracle's text is not
stable from run to run. The port lists them sorted.

The oracle now lists them sorted too, in the same `frozenset({...})`
text. Two cases drop three invariants and two postconditions at once.

Status: retired 2026-09-27 by the oracle fix and its tests (named above).
Retires when: retired (see Status).

**K1-6 (ruling): a Tier-1 config whose model is not a string.**
`load_configured_tier1` builds a provider from any truthy `model`, named
after it (`http:5`), which then always declines. The port reads such a
`local_tier1.json` as not in the binary yet. With a `base_url` set, both
raise the oracle's `TypeError`.

Status: in force.
Retires when: the oracle validates `local_tier1.json`.

**K1-7 (ruling): recall binds with a model.** GW-11 left a typed pathway
on its frozen template in Go. `recall` now binds the holes with one model
call (`envgen.Bind`), with the request bytes the oracle sends, and then
verifies the plan against the caller's envelope. The recall cases record
the fake model's requests and compare them.

Status: in force.
Retires when: does not retire.

## 2026-09-27: stage K1 in Rust (model-written envelopes, Tier-0, bind, compose)

**K1-R-1 (ruling): what carries K1 in Rust.** The Rust binary carries
`daisugi generate-envelope`, and the rest of stage K1 as the library
`src/envgen`, proven query by query through the test instrument
`envelope-probe` on the same 186 cases and the same script as Go. It
follows K1-1, K1-3, K1-6 and K1-7 as the Go binary does: the same three
cases are refused, a bound plan whose verify did not finish gives the
template, a Tier-1 model that is not a string is not in the binary, and
`recall` binds a typed pathway with one model call. The self-consistency
check runs on the linked Z3 (`pathways::verify::self_consistent`), not on
the native test the gate uses, so a test can make it answer `unknown`.

Status: in force.
Retires when: K2 and K3 put commands on these paths, as K1-1.

**K1-R-2 (ruling, Go and Rust): composition's `ok` fails closed.** The
compose probe reports `ok` false when a Z3 check in verify did not finish,
where the oracle's lenient verify reports `ok` true with a warning. Both
ports do this; no case reaches it, since the checks are ground. It
extends GW-11 and K1-3 to a plan that calls pathways as skills.

Status: in force.
Retires when: the oracle's composition verifies in strict mode.


## 2026-09-27: harness fixes (distinct ports; the binary's file name)

**PORT-1 (harness): each role in a case has its own port.** The gateway
compare took two ports from two `free_port()` calls, each closed at once, and
then bound its fake upstream to port 0. The kernel could give out the same
number twice, so two roles shared a port: one server could not listen, and
the normalizer wrote `{UP}` where `{GW}` stood (CI saw `switchyard own config
key auth` exit 0 against 3). `clients/ports.py` now holds each port bound
until the process that needs it starts, and never gives out a number already
in use by the case. A case whose record shows a bind clash (another process
on the box took the port in the gap) runs once more.

Status: in force.
Retires when: does not retire.

**INS-1 (ruling, Go and Rust, all three readers): a hook is the gate only when
its program's file name is `daisugi`.** `install gateway idempotent` agreed
when the binary ran from `target/release/daisugi` and disagreed from a copy
named `go-daisugi` or `rs-daisugi`: the second install added a second Codex
gate hook. The cause is the hook reader, not the location. `gate_hook_kind`
reads `NAME=... <program> gate check ...` as the gate only when the
program's file name is `daisugi` (the "true marker" case already rules that
the `DAISUGI_GATE_HOOK` marker alone does not make a gate). A binary with
another file name writes a hook with its own path, and no reader then sees
that hook as the gate. Claude Code's `settings.json` did not get a second
hook, because `_patch_claude_gate` first skips a command equal to the one it
would write; Codex's `_patch_codex_gate` has only the reader test, so it
appends again. The CLI compare now runs the binary through a symlink named
`daisugi` in its scratch directory (`as_daisugi`), and Go and Rust keep the
symlink's path, so no case depends on where the build put the binary. The
gap for a user stays: a binary installed under another name writes a hook
that `gate status` does not report, that `install --gate --uninstall` does
not remove, and that a second install repeats in Codex.

Status: in force (the harness part); the user-facing gap retired 2026-09-27
by the reader fix below.
Retires when: does not retire (the harness part).

The fix is in the reader, not the writer that this ruling first named. A
writer that refused under another file name would stop a user who installed
the binary as, for example, `daisugi-go`; a reader that knows the hook by
its shape works for every name. `gate_hook_kind` (Python, Go and Rust) now
reads `[NAME=val ...] PROGRAM gate check ARGS` as the gate when the program
is named `daisugi`, as before, or when the marker
`DAISUGI_GATE_HOOK=opendaisugi.gate` is among the leading assignments and
ARGS hold each flag the installer always writes (`--mode`, `--root`,
`--format`, `--verify-timeout`). The marker alone is still not a gate: the
"true marker" case (`/bin/true gate check --mode enforce`) lacks three of
the flags and stays None. `gate_hook_program` follows the same rule, so
status warns when a renamed binary is gone. A `uv run ... daisugi gate check`
hook is read by name as before. Nine CLI cases cover it: two installs run
twice through a link named `daisugi-copy` (`prog`), a renamed hook read and
removed in Claude Code and Codex, and two shapes that are not the gate. All
three languages agree on all nine.

## 2026-09-27: stage K2 in Go (`run`, `orchestrate`: the supervisor, the orchestrator, the decomposer)

`clients/k2_cases.py` writes 116 cases from the oracle; `clients/k2_compare.py`
runs them. The Go binary agrees on 109; 7 are refused as ruled below, the tree
unchanged. Python read
back every journal and pathway store the binary wrote (99 cases).

**K2-1 (ruling, Go): the per-step rejection and the recompute fallback are
ported, and no case reaches them.** `Supervisor.run` re-checks each step with
`verify_step`, and on a rejection calls the envelope's fallback (halt, or
`tier2_recompute`: one model call for a replacement of the step's own type). The
per-step checks are a subset of the whole-plan verify that runs first, so from the
CLI a plan that passes the first also passes the second. Go tests reach these
paths with the check swapped: a halt writes one `refinement_log` row and runs
nothing; a recomputed step that passes runs in place of the rejected one; one that
fails halts the run.

Status: retired 2026-09-30.
Retires when: a CLI path makes the per-step check reject a step the whole-plan
verify admitted, and a case covers it. Retired: `daisugi weave` fills a slot
into a step the whole-plan verify admitted, and the filled step's per-step
check rejects it (WV-R-4). Four weave cases cover the halt and the recompute
fallback (one model call, a replacement that is itself rejected); Go and Rust
agree on all four.

**K2-2 (ruling, Go): a Z3 check that does not finish fails closed in every K2
verify.** The whole-plan verify before a run, the decomposed plan's verify, a
reused pathway's verify, the per-step check and a recomputed step's verify each
treat an unknown as a failure (the run is rejected and journaled with a `z3`
violation; the decomposition fails as out of policy), where the oracle's lenient
verify keeps it as a warning. It extends GW-11 and K1-3. No case reaches it, since
the checks are ground; Go tests do, with the verify swapped.

Status: in force.
Retires when: the oracle's supervisor and decomposer verify in strict mode.

**K2-3 (ruling, Go): YAML the binary does not read is refused.** `run` and
`orchestrate --envelope` read a file with the config reader, and then as
`yaml.safe_dump` writes YAML (`LoadDumped`, which accepts a reading only when
dumping it gives the same text back). A file in any other form, and a file PyYAML
rejects, is refused with exit 2 before anything is written: the binary does not
word PyYAML's errors (3 cases).

Status: retired 2026-10-07 by YM-1: both ports load the files as
`yaml.safe_load` does and word its errors (the 3 cases agree).
Retires when: retired (see Status).

**K2-4 (ruling, Go): `orchestrate --max-parallel` above 1 is not in this binary
yet.** The oracle then prefetches a level's steps through `asyncio.gather`, so the
order of outcomes and receipts can change from run to run (1 case).

Status: retired 2026-10-03 by MP-1: both ports run a level's independent
steps concurrently.
Retires when: retired (see Status).

**K2-5 (ruling, Go): a stale-embeddings warning from the pathway store is
refused before anything is written.** `PathwayStore.find` warns (a UserWarning,
whose text names the oracle's source file) when stored pathways were embedded
under another model. `orchestrate` reads the store without a write first and
refuses when find would warn (1 case).

Status: retired 2026-10-08 by RF-4: both ports print the warning (in the
form the compare reads) under PYTHONWARNINGS as Python reads it.
Retires when: met (see Status).

**K2-6 (ruling, Go): a refusal after the data directory is set up says so.** A
verifier warning this binary does not word, or a violation whose detail it does
not write, can only show after the facade has made the envelope cache, pathway
store and journal (and, with no envelope given, after one was generated). The
binary then exits 2 with "... The envelope cache, pathway store and journal were
made; no step ran." instead of "Nothing was changed." No case reaches it: a plan
that reaches the run has passed the same verify with no violation.

Status: in force.
Retires when: every verifier warning and violation detail is worded in Go.

**K2-7 (ruling, Go and oracle): no CLI path runs an agentic step.** Neither
`run` nor `orchestrate` wires an agentic executor (the oracle's
`default_executors` holds shell, file_read, file_write and network; the
orchestrator adds task, skill and mcp). An agentic step fails with "no executor
for kind 'agentic'" and makes no model call, so no case has the fake `claude`
answer one. The binary does not port `AgenticExecutor`.

Status: retired 2026-10-09 (SX-R-1). For `weave` it was retired for the
oracle 2026-09-30 and for the ports 2026-10-02 by PG-3; for `run` and
`orchestrate` 2026-10-09 by SX-R-1, when the oracle and both ports wired
`AgenticExecutor` there too (cases `run agentic step runs`, `run agentic
step sprig`, `run agentic dry run`, `orchestrate reuses delegated pathway`
and its sprig twin).

**K2-8 (ruling, Go): an envelope the binary cannot check the oracle's way is
refused before the run.** An invariant whose `expr` asks a model (`llm_check`),
and an enforced postcondition whose `expr` asks a model, names an alias, is not a
dict, or does not parse, are refused with exit 2 before anything is written (2
cases: an invariant on `run`, a postcondition on `orchestrate`). An envelope the
model wrote for `orchestrate` is checked the same way once it exists; that
refusal comes after the data directory is set up and is worded as K2-6. The three opaque types
(`exit_code`, `file_exists`, `file_size_range`) and predicate exprs run as in
`stage2.verify_completed_step`.

Status: narrowed 2026-10-02 by PG-1 and PG-2: the ports ask the model for
an `llm_check` and word an unresolved alias as the oracle does, in every
verify and at stage 2, and check a postcondition expr that is not a dict
as the oracle does. In force for an enforced invariant or postcondition
whose `expr` is a dict that does not parse as a predicate: the oracle
raises a ValidationError out of verify there (a traceback, exit 1), and
the ports refuse with exit 2 before anything is written (1 case, `run
postcondition expr does not parse`).
Retires when: does not retire.

**K2-9 (harness): typer.echo drops ANSI sequences on a stream that is not a
terminal.** A step's output with colour codes printed differently until the
binary did the same (click's `_ansi_re`). The JSON forms escape the characters
and were already equal.

Status: in force.
Retires when: does not retire.

## 2026-09-27: stage K2 in Rust (`run`, `orchestrate`: the supervisor, the orchestrator, the decomposer)

**K2-R-1 (ruling, Rust): what carries K2 in Rust, and what is its own.**
The Rust binary carries `daisugi run` and `daisugi orchestrate` (`src/supervise`,
`src/orchestrate`) and follows the Go binary's rulings K2-1 to K2-9: the same
116 K2 cases agree but the same 7 refused as ruled, by name. Three points are
the Rust port's own:

- YAML (K2-3). A plan or envelope file is read in the form `yaml.safe_dump`
  writes (`pathways::dumped`), else by the config reader's subset
  (`cli::yaml`, as `gate register` reads an envelope). Anything else, and a
  file PyYAML rejects, is refused with exit 2 before anything is written. The
  refused set is Go's.
- Swappable checks. The Go tests swap package variables; the Rust tests pass
  a verify function to `run_cmd_with`, `orchestrate::Options.check`,
  `decompose`, `Supervisor.verify_step` and the recompute `Hooks`. Only
  tests pass another.
- The network executor sends its GET over the binary's own HTTP/1.1 client
  (`netproxy::open`, no proxy, no redirect followed), as Go sends it over
  `net/http` with the proxy turned off. A refused connection is worded
  `URLError: [Errno N] <strerror>` in both. No case reaches a server that
  answers.

Status: in force; its K2-5 clause retired 2026-10-08 by RF-4.
Retires when: K2-3, K2-4, K2-5 and K2-8 retire for both ports.


## 2026-09-28: stage K3 in Go (`mcp serve`, `onboard`, `tiers setup`)

`clients/k3_cases.py` writes 207 cases from the oracle; `clients/k3_compare.py`
runs them. The Go binary agrees on 204; 3 are not ported, as ruled below.
Python read back every journal and pathway store the binary wrote.

**K3-1 (ruling, Go): the MCP server takes no SDK, and says what the oracle's
SDK says.** Per DS-6, `daisugi mcp serve` is a JSON-RPC 2.0 server over stdio
of its own (`internal/mcpwire`). It copies what the oracle's FastMCP (mcp
1.30.0) answers: lines read with universal newlines and bad UTF-8 replaced;
the SDK's error notification for a line that is not a request or
notification (a response included); -32602 for a request the SDK does not
take (an unknown method, bad params, anything but initialize or ping before
initialize); -32601 for `logging/setLevel`; code 0 for `prompts/get`; each
tool's argument pre-parse and validation with its pydantic text; the tool
result's text block (`pydantic_core.to_json`, indent 2) and structured
content. `serverInfo.version` is the oracle's SDK version, and the tools/list
result, the capabilities and the instructions come from the oracle
(`gen_tools.py`, whose `--check` fails when they drift).

Status: in force.
Retires when: the oracle's server drops the SDK; the binary then pins the
oracle's own version string.

**K3-2 (harness): an MCP case compares stdout, not stderr.** The oracle's
stderr is the SDK's log (rich text that depends on width and time), not the
protocol. The driver sends one line at a time and waits for each request's
reply; a line that gets no reply is followed by a ping, so every line's
effect is in stdout before the next is sent. An oracle server that exits
other than 0 fails the recording. A binary that does not carry the server
keeps its one stderr line, so the compare reads it as not ported.

Status: in force.
Retires when: does not retire.

**K3-3 (ruling, Go): an MCP request the binary does not answer the oracle's
way gets error -32000 "... is not in this binary yet.", and the server goes
on.** These are `resources/read`, `resources/subscribe`,
`resources/unsubscribe`, `completion/complete`, the `tasks/*` methods, a
task-augmented call, arguments that hold a lone surrogate or nest deeper
than the pre-parse reads, and tool input a tool cannot read the oracle's way
(a store or journal row it does not read, a verifier warning or violation
detail it does not word, a postcondition expr it does not evaluate, a
matcher it does not build, `OPENDAISUGI_MCP_RUN_TIMEOUT` set). No case
reaches one; a Go test does. The compare takes a refused request when every
line before it is the oracle's and the binary wrote no file the oracle did
not.

Status: in force, narrowed 2026-10-03 by MX-1 (`resources/read`, `resources/subscribe`, `resources/unsubscribe`, `completion/complete`, the `tasks/*` methods and a task-augmented call are answered); its postcondition half narrowed 2026-10-02 by PG-1 and PG-2: `verify_completed_step` and `verify_plan` now answer an `llm_check`, an unresolved alias and an expr that is not a dict as the oracle does (10 cases); a dict expr that does not parse and an evaluation error the binary does not word stay refused.
Retires when: each of these is ported.

**K3-4 (ruling, Go): stdin that closes while a tool call runs.** The oracle
cancels the call and writes no reply; which calls it cancels depends on
timing, so no case compares it. The binary reads and answers one request at
a time, so it answers every whole request it read before it exits 0. A last
line with no end is read as a line in both (1 case).

Status: in force.
Retires when: the binary cancels a call in flight when its input ends.

**K3-5 (ruling, Go): a Z3 check that does not finish fails closed in
`verify_plan`, `run_plan` and `recall`.** It extends K2-2 and GW-11:
`verify_plan` answers not ok with a `z3` violation, where the oracle's
lenient verify keeps a warning. No case reaches it; a Go test does.

Status: in force.
Retires when: the oracle's MCP tools verify in strict mode.

**K3-6 (ruling, Go): `recall` with a `z3_timeout_ms` of 0 or less, or above
2^31-1, is refused.** Z3 reads such a timeout as none, and the oracle passes
the value through. No case.

Status: in force.
Retires when: the binary gives Z3 the value as the oracle does.

**K3-7 (ruling, oracle and Go): `recall_answer` refuses a `max_age_seconds`
that is not finite.** The string "NaN" reaches the tool as a float NaN
(pydantic reads it), and every age check is then false: the oracle served
an answer ten million seconds old (fail open, the class of GD-16 and
GD-17). Both sides now raise "max_age_seconds must be a finite number, got
nan" (2 cases; a Python test).

Status: in force.
Retires when: does not retire.

**K3-8 (ruling, Go): initialize params are checked one level deep.** The
binary checks `protocolVersion` (str or int), that each known capability is
an object, and `clientInfo` (name and version are str). A deeper field of
the wrong type (for example `roots.listChanged: 5`) gets -32602 from the
oracle and is taken by the binary.

Status: in force.
Retires when: the binary checks the SDK's whole ClientCapabilities model.

**K3-9 (ruling, Go): `tiers setup` probes the box as the oracle does on
Linux, without torch.** It extends C-13: the GPU from `nvidia-smi` alone,
the CPU count from `/sys/devices/system/cpu/online`, the memory from
MemTotal. Every K3 case puts a fake `nvidia-smi` first on PATH, so no case
probes the box's GPU; the budget comes from the fake GPU, and the CPU count
and memory are placeholders. The CPU-only recommendation depends on the
box's memory, so a Go test holds the oracle's answers for fixed profiles.
`--remote` and `tiers stats` were not in this binary (2 cases); both are
ported now (TS-1, SR-1). The "all attempts errored" hint is unreachable
in both: the Tier-1 provider declines rather than raises.

Status: in force (the probe of the box); the `--remote` and `tiers stats`
half retired 2026-10-03.
Retires when: the probe reads the box another way.

**K3-10 (ruling, oracle and Go): onboard's embedder check reads the matcher
in effect.** The oracle refused when the `sentence_transformers` package was
absent, which says nothing about the matcher it will use. It now asks for
the matcher in effect, after the package-absence fallback to lexical, as
`status` does: only a key that nothing builds turns pathways off. A real run
then refuses with exit 2, a dry run prints a note, and `--allow-no-embedder`
goes on to tend, which raises (4 cases, Python tests). The binary does the
same for a key that nothing builds, and refuses a matcher the oracle builds
and it does not before anything is written (1 case: no config, so the
oracle's MiniLM).

Status: in force. The gap between the oracle's check and `status` is closed.
Retires when: the binary builds every matcher the oracle builds.

**K3-11 (ruling, Go): onboard reads everything before its first write.** The
binary finds, parses (with any split model calls) and verifies every
transcript, then writes the journal, the split cache, the traces and runs
tend. The oracle interleaves parse and ingest per transcript. The model
requests are the same, in the same order, and so is the tree; a refusal
(input the binary cannot read the oracle's way, such as a transcript that is
not UTF-8, which the oracle warns about) changes nothing. Transcripts with
the same mtime keep the oracle's directory-scan order, and the binary's name
order: no case holds a tie. The binary does not walk into a linked
directory.

Status: in force.
Retires when: the oracle sorts ties by path, and a transcript the binary
cannot read is worded as the oracle's warning.

**K3-12 (harness): onboard cases normalize the source hash and leave
clustering to the garden cases.** An ingested trace's id holds the SHA-256
of its transcript's path, which names the scratch HOME; each is written as
the path it stands for (`import-{SRC path}-`), so the hash is still checked.
A tend that clusters asks a model with a prompt that names the scratch HOME,
so the request key changes run to run; the real-run cases pass
`--min-traces 99`. By hand, the binary's clustering prompt equals the
oracle's once HOME is normalized.

Status: in force.
Retires when: the fake model keys requests by their normalized bytes.


## 2026-09-28: stage K3 in Rust (`mcp serve`, `onboard`, `tiers setup`)

The Rust binary carries `daisugi mcp serve` (`src/mcpwire`,
`src/cli/mcpcmd.rs`), `onboard` and `tiers setup`, and follows the Go
binary's rulings K3-1 to K3-12: the same 207 K3 cases, 204 agree, the same
3 not ported by name, 0 disagreements. The points below are the Rust
port's own.

**K3-R-1 (ruling, Rust): the server writes its own lines, and a panic is a
refusal.** The binary's commands buffer their output until they exit;
`mcp serve` writes each line to stdout and flushes it at once, so the
client reads each reply as it is made. A tool call that panics gets the
-32000 refusal of K3-3 ("... with input this binary does not handle") and
the server goes on, as the Go server recovers. No case reaches a panic.

Status: in force.
Retires when: does not retire.

**K3-R-2 (ruling, Rust): the strict-mode opaque predicate violation keeps
its detail in the verifier's result.** The predicate stage words every
violation's detail, but the verify the MCP tools and `run` share kept only
the message, so a result that held one was refused (1 K3 case, `mcp verify
high stakes`). It now keeps the detail of the `opaque_unrecognized`
violation, as the Go port does; every other predicate violation is still
refused where a caller writes it. The other compares do not change.

Status: retired 2026-10-02 by PG-1: the verifier keeps every predicate violation's detail, and the Go port words the same details.
Retires when: the verifier keeps every predicate violation's detail (done).

**K3-R-3 (ruling, Rust): onboard walks the transcript roots in name order
and does not read a file name that is not UTF-8.** The directories are
read with `read_dir`, whose order the platform decides; the binary sorts
each directory's entries by name, the Go binary's walk order, so
transcripts with the same mtime keep the order K3-11 names. A name that is
not UTF-8 is left out, where Python would find it with its bytes escaped.
No case holds one.

Status: in force.
Retires when: the binary reads such a name as Python does.


## 2026-09-28: stage K4 in Go (`modules`, `dashboard`, `metrics`)

`clients/k4_cases.py` writes 110 cases from the oracle; `clients/k4_compare.py`
runs them. The Go binary agrees on 105; 2 are not ported and 3 refused, as
ruled below. Python read back every journal and pathway store the binary
wrote. `start` now opens the view: the 3 `start view` cases of the cli
compare agree. An unknown long option now names the close matches click
names, in every Go command (no compare changed).

**K4-1 (ruling, Go): the map answers as a base install with the binary's
extras.** The module map reports which optional packages are installed, and
the venv that records the fixtures has more of them than the binaries carry.
The oracle runs under `clients/base_install.py`: `sentence_transformers`,
`onnxruntime`, `faster_whisper`, `sherpa_onnx` and `textual` are hidden, and
there is no checkout, so no compiled verifier client is built. The binary
answers the same: potion and the bash grammar are installed; MiniLM, int8,
the voice engines and the four compiled clients are not. It keeps the
oracle's words, so a row names the Python extra (`needs opendaisugi[search]`)
and the verifier row reads `python (in-process)`, though the binary's
verifier is its own.

Status: in force.
Retires when: the binary words its own rows, or carries the missing matchers.

**K4-2 (ruling, Go): under MiniLM or int8 the lexical rows are available.**
With the package absent the oracle falls back to the lexical matcher
(ADR-0019) and marks its two rows active. The binary refuses those matchers
(C-12, K3-10) and falls back to nothing, so the rows are available, as under
any other matcher. The 8 cases with the default matcher or int8 carry
`go_expect` with the two rows rewritten.

Status: in force.
Retires when: C-12 retires.

**K4-3 (ruling, Go): the Textual views are refused.** `dashboard --tui` and
`dashboard --serve` exit 2 with "... is not in this binary yet." (2 cases).
`--json` and `--once` are read first, as in the oracle, so `--json --tui`
answers.

Status: in force.
Retires when: the binary carries an interactive view.

**K4-4 (ruling, Go): input the oracle raises on or adds as it finds it is
refused before any write.** A `~/.claude/settings.json` whose hooks are not
the shapes the map reads (not an object at the top, an event that is not a
list of objects, a command that is not a string) is refused; the oracle
raises `AttributeError` or `TypeError` after it made the journal (1 case). A
gateway turn record of other types (GW-8) is refused (2 cases). A gateway
journal the oracle cannot read (not UTF-8, a line `json.loads` raises on)
is a blank gauge, as the oracle swallows the error. A pathway store whose
hit sum is inf ends with exit 1 and the oracle's `TypeError` named.

Status: in force.
Retires when: does not retire.

**K4-5 (ruling, Go): `start` opens the plain live view.** The view step reads
as the oracle's without the tui extra: `open the live view (daisugi
dashboard); install the tui extra for the full instrument`, `would`, or
`skipped` with `--no-ui`. After the steps it draws one frame when stdout is
not a terminal; on a terminal it draws a frame every 2 seconds until Ctrl-C,
which ends with "Aborted!" and exit 1, as click ends the oracle's. The cli
cases run the start oracle as a base install (K4-1) and the view cases with
PATH set to the case's bin directory and a lexical config. A binary without
`dashboard` (the cli compare probes `dashboard --help`) is held to C-11.

Status: in force.
Retires when: does not retire.

**K4-6 (ruling, Go): the /metrics endpoint.** A scrape compares the status,
Content-Type and body, and Content-Length on a 200. The binary answers
HTTP/1.1 where the oracle's `http.server` answers HTTP/1.0, sends no
`Server` header, and its 404 carries `Content-Length: 0`; each connection is
closed after its reply, as the oracle's is. The request target is matched as
sent, with no path cleaning. A method other than GET gets the oracle's 501
page. A scrape over a gateway record of other types answers 500 with no
numbers, where the oracle adds what is there. A port it cannot bind ends
with exit 1 and the `OSError` named.

Status: in force.
Retires when: does not retire.

**K4-7 (harness): the box stays out of the cases.** PATH holds only the
case's bin directory and its own fakes, since the map reports tmux, herdr,
coppice, opencode and switchyard-server on PATH. A coppice socket is made by
the harness before the command and removed after, with the directories made
for it. A serve case starts `metrics --serve` on a port from
`clients/ports.py`, waits until it listens, scrapes, writes the files a step
names, then sends SIGINT; the port is written `{MPORT}`. The terminal view is
not in the cases; a Go test drives it over a pty (box glyphs, the screen
cleared before each frame, the lit stage moving, Ctrl-C). An interval that
is negative, NaN or inf ends a terminal view with exit 1 after its first
frame, where the oracle's `time.sleep` raises.

Status: in force.
Retires when: does not retire.

**K4-8 (found, then fixed): the map wrote the journal.** The dashboard
module says reading never makes a store, but `detect_stages` called
`gather_status`, whose `Journal()` made `journal/index.db`. So `daisugi
modules`, `daisugi dashboard` and `daisugi status` made the journal on a
fresh box, and the dashboard's traces gauge then read 0, not blank. The
oracle now reads the journal through `journal.read_stats`, which opens
the index read-only: a missing journal is not made and reads as empty
(blank on the floor), and an index with no traces table reads as empty.
`read_raw` reads it the same way, so an old journal is not migrated
either. Go and Rust do the same; the K4, start-view and status fixtures
were regenerated.

Status: retired 2026-09-28 (fixed in the oracle, Go and Rust).
Retires when: retired.

**K4-9 (found, then fixed): `status` counted no pathways when the hit sum
was inf.** `gather_status` keeps `int(count)` when `int(hits)` raises, and
the hits stay 0; the Go and Rust `status` set the count to 0. Both now
keep the count. The K4 case `status inf hit sum keeps the count` holds
such a store (two REAL hit counts of 1e308).

Status: retired 2026-09-28 (fixed in Go and Rust).
Retires when: retired.


## 2026-09-28: `daisugi hook record` in Go and Rust

The floor-report hooks and the capture hooks `install` writes run
`daisugi hook record --format claude|hermes|openclaw [--event ...]`. Both
binaries refused it with exit 2, and in Claude Code a Stop hook that
exits 2 blocks the stop. Both now carry it (Go `internal/gate/hookrecord.go`
and `internal/cli/hookcmd.go`; Rust `src/gate/hookrecord.rs` and
`src/cli/hookcmd.rs`). `clients/k4_cases.py` holds 76 hook cases; both
binaries agree on all 76.

**HR-1 (ruling, Go and Rust): past the options, the contract and exit 0.**
Python swallows every error on the hook path. The binaries do the same,
and a panic in a write is recovered. A write the binary cannot finish,
or an input it does not model, is skipped: JSON its decoder does not
model, a session tree it cannot read, a value whose `str()` it does not
word. So where the oracle might still write, the binary may write less,
but stdout and the exit code never differ. The option errors keep the
oracle's exits: an `--event` it does not name ends with exit 1 and the
oracle's line, an unknown option with click's exit 2. One residual: the
Go runtime ends the process with exit 2 on a fatal error (out of memory,
a stack overflow) or on a panic in a goroutine other than the one the
command runs on, which the command cannot recover; a Rust stack overflow
ends the process on a signal. No input the cases hold reaches either.

Status: in force.
Retires when: does not retire.

**HR-2 (ruling, Go and Rust): the background tend trigger.** The trigger
reads the stamp and the consent as the oracle does and starts `daisugi
hook auto-tend --data-dir <dir>` from PATH in a session of its own, its
streams on /dev/null, never waited on. A config.yaml the binary cannot
read counts as no consent, where the oracle may find consent in it. The
unit tests replace the spawn, so no test starts a real `daisugi`.

Status: in force.
Retires when: the binaries read every config.yaml the oracle reads.

**HR-3 (harness): stdin, a listening coppice, a fake herdr.** A hook case
feeds stdin (text or hex). A case that names `coppice` gets a unix socket
in its HOME that listens, writes each line a client sends to
`coppice.log` in the tree and answers `{"ok": true}`, so a report that is
never sent shows up. A fake `herdr` in `.fakebin` logs its argv. Session
tree entry ids (8 random hex digits) are renamed by first appearance.

Status: in force.
Retires when: does not retire.

**HR-4 (ruling): the other `hook` subcommands stay refused.** The
installers write only `hook record` (the Claude capture, stop,
notification and subagent hooks, the Hermes hook and the OpenClaw plugin)
and `hook auto-tend` (cron, and the trigger above), which both binaries
carry. `hook report` is reached through gate.sock, which `gate serve`
answers. `hook list`, `hook to-trace` and the command-line `hook report`
are in no hook and stay refused with exit 2.

Status: retired. `hook list` and `hook to-trace` retired 2026-09-28 by stage F, for the Go binary and for the Rust binary; the command-line `hook report` retired 2026-10-02 by PG-6.
Retires when: the binaries carry them.


## 2026-09-28: stage K4 in Rust (`modules`, `dashboard`, `metrics`)

The Rust binary carries `daisugi modules`, `dashboard` and `metrics`
(`src/cli/modulescmd.rs`, `src/cli/dashboardcmd.rs`) and follows rulings
K4-1 to K4-7. The K4 compare now holds 193 cases (the 110 of stage K4, 7
cases for K4-8 and K4-9: 3 `status --json` and the map, the floor and the
exporter over an inf hit sum, and the 76 hook cases above): Rust and Go
each agree on 188, with the same 2 not ported (K4-3) and 3 refused
(K4-4), and 0 disagreements. The points below are the Rust port's
own.

**K4-R-1 (ruling, Rust): `start` opens the plain live view.** As K4-5 for
Go: the view step reads `would` (or `skipped` with `--no-ui`), and after
the steps `start` draws one frame when stdout is not a terminal, or a
frame every 2 seconds on a terminal until Ctrl-C ends it with "Aborted!"
and exit 1. The root `--plain` drops the box drawing. C-11 is retired for
Rust; the 3 `start view` cli cases agree.

Status: in force.
Retires when: does not retire.

**K4-R-2 (ruling, Rust): the /metrics server is the binary's own.** It
answers on a std TCP listener, one thread per connection, and closes each
connection after its reply, as K4-6 says for Go: HTTP/1.1, no `Server`
header, `Content-Length` on every reply. A request line it cannot read
gets no reply where `http.server` answers 400. No case sends one.

Status: in force.
Retires when: the server answers a request line it cannot read as
`http.server` does.

**K4-R-3 (harness, Rust): the terminal view is tested over a pty.** The
integration test `the_live_view_on_a_terminal` starts the binary with its
stdout on a pty (posix_openpt): box glyphs, the screen cleared before
each frame, the lit stage moving, and Ctrl-C ending it with exit 0.

Status: in force.
Retires when: does not retire.

**K4-R-5 (found and fixed, Rust): an overflowing hit sum read as 0.** The
SQLite that rusqlite bundles (3.45) answers NULL for a SUM of REAL hit
counts that overflows; the oracle's SQLite (3.50) and the Go driver's
answer inf. So the Rust pathway store read the sum of two hit counts of
1e308 as 0, and `dashboard` and `metrics` drew numbers where the oracle
raises (3 of the new K4 cases, `... inf hit sum`). `Store::stats` now sums
the hit counts as floats when SQLite's SUM is NULL over a table with
rows, which gives inf there. The pathway, K3, K4 and status compares do
not change otherwise.

Status: in force.
Retires when: the bundled SQLite sums as the oracle's does.

**K4-R-4 (ruling, Rust): an unknown long option names its close
matches.** As in Go, every Rust command adds click's "(Possible options:
...)" with the difflib matches of ratio 0.6 or more. No compare changed.

Status: in force.
Retires when: does not retire.

## 2026-09-28: two flaky Rust tests

**RF-1 (found and fixed, Rust): a bind in a test failed now and then.**
`servecmd::probe_tells_nothing_stale_and_live_apart` makes a directory and
binds a socket in it. `listen` set the process umask to 0177 around its
bind, and the umask is shared by every thread: a directory another test
thread made in that window got mode 0600, with no search bit, and the bind
in it failed with EACCES. The failure was not caught in the Rust test; the
mechanism was shown apart from it: a thread that sets the umask to 0177
around a bind while another makes directories left 842 of 20,000 of
those directories without a search bit. `listen` no longer changes the
umask, in Rust or Go. The root is chmod 0700 before the bind, as in the oracle, so no other
user can reach the socket before its own chmod to 0600. The lib tests
passed 20 runs in a row; `gate_server` 200 cases agree in Go and Rust.

Status: in force.
Retires when: does not retire.

**RF-2 (found and fixed, Rust): an exec in a test failed now and then.**
`a_mise_install_hooks_the_shim_and_status_warns_on_a_gone_binary` wrote
its fake executables (the installed copy, the shim, the fake mise and the
stub) from the test process and ran them at once. A test on another
thread that forked in that window carried the open write handle into its
child until the child's exec, and the exec of the fresh file then failed
with ETXTBSY ("Text file busy"). As with RF-1, the mechanism was shown
apart from the test: a script written and run at once while another
thread forks and execs failed with ETXTBSY 12 times in 3,000. A child
process now writes each file, so
the test process never holds a write handle to a file it runs. The CLI
tests passed 20 runs in a row. `src/orchestrate/tests.rs` writes and runs
a script the same way and is left as it is; it has not failed.

Status: in force.
Retires when: does not retire.

## 2026-09-28: stage L in Go (registry, batch prove, release, deeds, strata)

The Go binary carries `daisugi registry init|pull|publish|status|pull-and-tend`
(`internal/registry`, `internal/cli/registrycmd.go`), `daisugi batch prove`
(`internal/batch`, `internal/cli/batchcmd.go`) and `daisugi release
keygen|sign|verify` (`internal/signing`, `internal/cli/releasecmd.go`),
and the library parts no command reaches: the deed ledger
(`internal/deeds`), the strata store (`internal/strata`), batch runs, the
pathway bundle (`internal/bundle`) and contract signing. The L compare
holds 232 cases: 137 commands and 95 library probes. Go agrees on all
232, with none refused. Rust carries none of stage L and refuses all 232.

**L-1 (ruling): the library parts are proved through a probe.** No
command reaches `deeds.rollback_run`, `touched_files`, `apply_reversal`,
the strata store and `promote_constraint`, `batch.run_batch` and
`rollback_result`, `bundle_to_pathway`, the contract signing functions or
`TrustedSignerRegistry.verify`. `cmd/l-probe`, a test instrument that is
not shipped, answers one query of them as `clients/l_probe_oracle.py`
answers it, as K1 proves its library parts through `envelope-probe`.

Status: in force. Rust proves them through its own `l-probe`
(`src/bin/l_probe.rs`, `src/cli/lprobe.rs`).
Retires when: a command reaches these parts.

**L-2 (ruling): values made from the clock or a random key vary.** A
bundle published now has a hash, a file name, a signature and a commit id
no two runs share; a manifest signed now has its own `created_at` and
signature; keygen makes new keys. A case names these kinds in `vary`, and
they are renamed by order of first appearance in both results; a value
the case laid out itself is kept. `l_compare.py` then checks each with the
oracle's own code on the tree the binary left: a bundle's hash recomputed
from its content equals its `bundle_hash` and its file name, and its
signature verifies under the key the case signed with; a manifest
verifies, its `created_at` has isoformat's shape, and each artifact's
SHA-256 and size are the file's; a keygen public key is its private
key's.

Status: in force.
Retires when: does not retire.

**L-3 (ruling): a bundle in YAML the binary does not read is skipped.**
The oracle skips a bundle file that does not parse or validate. The
binary reads the YAML `yaml.safe_dump` writes, and more; a file outside
what it reads is skipped as unparseable. So a bundle is never admitted on
a reading the oracle might not share; a hand-edited bundle in other YAML
that the oracle admits is left out by the binary (stricter, never
looser). No case hits it.

Status: in force.
Retires when: the binary reads all the YAML the oracle's loader does.

**L-4 (ruling): keys in the oracle's format, read the oracle's way.** A
private key file holds the raw 32-byte ed25519 seed in base64, a public
key file the raw 32 bytes in base64, each with a newline, as
`generate_keypair` writes them (its docstring said PKCS#8; the code writes
the seed, and the docstring is fixed). Base64 is decoded as CPython's
lenient `b64decode`: characters outside the alphabet are skipped, a pad
that completes a quad ends the input, and the same `binascii.Error` texts
("Incorrect padding", "number of data characters (N) cannot be 1 more than
a multiple of 4") are raised. A 37-input table in the probe cases agrees.
Signatures are Go's `crypto/ed25519`; on 31 verify cases (small-order
keys, a key that is not a curve point, S of L or more, short and long
signatures) it answers as OpenSSL does under `cryptography`.

Status: in force.
Retires when: does not retire.

**L-5 (ruling): an exception the oracle raises.** The binary prints
`daisugi <command>: <type>: <text>` and exits 1, and the compare checks
that the type the oracle's traceback names is there (PW-13). For a key
that is not ASCII the oracle's traceback names the `UnicodeEncodeError` it
was handling, so the binary names it too.

Status: in force.
Retires when: does not retire.

**L-6 (ruling): git runs with the caller's environment.** As in the
oracle, `git -C <repo> ...` runs with the environment the command got,
its output captured (`registry init` lets git's output through, as the
oracle does). A `GIT_DIR` or `GIT_WORK_TREE` in that environment would
point git elsewhere, in both. The cases scrub git's environment
themselves: no system or user config but the case's own, a fixed
identity and fixed dates, and `GIT_CEILING_DIRECTORIES` at the scratch
root. Lines git writes to stderr are left out of the compare on both
sides: their wording changes from one git version to the next.

Status: retired 2026-09-28 by L-15: the oracle now drops every `GIT_*`
variable for each git child the registry starts, and the ports do too.
Retires when: retired (see Status).

**L-7 (oracle behavior, ported): trust comes only from the local
anchor.** The registry trusts the keys in `<clone>/.cache/trusted-signers.json`,
never the `trusted-signers.json` the remote sends. `--allow-unsigned`
takes unsigned bundles but refuses signed ones, since with no trust set a
signature proves nothing. A Go test publishes from one clone and pulls in
another: a remote trust file admits nothing; the local anchor admits the
bundle.

Status: in force.
Retires when: does not retire.

**L-8 (oracle behavior, ported): `registry pull` counts only what the
pull adds.** Opening the store already puts the bundles in the clone into
the cache and drops that count, so a first `registry pull` can print "0
new" and `registry status` then shows them cached. `registry status` also
makes the cache. The binary does the same.

Status: in force.
Retires when: the oracle counts what opening the store cached.

**L-9 (finding, oracle): two artifacts with one name.** `release sign`
over `dist/a.whl` and `other/a.whl` writes two entries named `a.whl`;
`release verify` checks both against one file, so the manifest can never
verify. Ported as it is (case `sign same name twice`).

Fixed in the oracle, then in Go and Rust: `build_manifest` raises
ValueError on two artifacts with one file name, and `release sign` checks
first, after the missing-artifact check: it prints `two artifacts share a
name: a.whl (the manifest names each artifact by its file name)` and exits
2 with nothing written. Cases `sign same name twice` and `sign same path
twice`.

Status: retired 2026-09-28 by the fix above.
Retires when: retired (see Status).

**L-10 (oracle behavior, ported): `registry init` refuses ext:: and fd::
by prefix, case-sensitive.** `EXT::...` reaches git, which refuses it
through `GIT_ALLOW_PROTOCOL` (https, http, ssh, git and file only); the
command then fails as git failed.

Status: in force.
Retires when: does not retire.

**L-11 (ruling): help text, and a group with no command.** `--help`
prints the binary's own help (PW-8). `daisugi registry`, `batch` or
`release` with no command prints the binary's own list of commands and
exits 0, where typer prints its help box and exits 2; no case compares
them.

Status: in force.
Retires when: does not retire.

**L-12 (ruling): the garden case for `pull-and-tend`.** The garden case
that held Go to refusing `registry pull-and-tend` is now `registry
pull-and-tend no clone`: the oracle raises ValueError, and Go does too.
Since 2026-09-28 Rust does too.

Status: in force.
Retires when: does not retire.

**L-13 (ruling, CI): the L compare guards both ports.** CI runs the L
compare with `--probe l-probe --max-refused 0` for Go and `--max-refused
232` for Rust, so Go cannot fall back to refusing stage L.

Status: retired 2026-09-28: Rust carries stage L, and CI runs the L
compare with `--probe l-probe --max-refused 0` for both.
Retires when: retired (see Status).

**L-14 (finding, oracle): the publishable check can never refuse.**
`GitPathwayStore.publish` refuses a pathway that is not marked
publishable, with `getattr(pathway, "publishable", True)`. CompiledPathway
has no such field, so the check always passes, and its message names
`daisugi pathways mark-publishable`, a command that does not exist. The
binary publishes every pathway, as the oracle does.

Ruling (the smaller change that keeps the gate honest): the check is
dropped. The design meant a `publishable` field, default False, set by a
`mark-publishable` command; neither was built, so the only opt-in that
exists is the explicit `daisugi registry publish <pathway-id>`, one named
pathway at a time, and nothing else (pull, tend, distillation) ever
publishes. A dead check that can never refuse claims a gate that is not
there; removing it and fixing the shipped registry doc
(`skills/opendaisugi-checklist/references/git-registry.md`) to say what
is true keeps the gate honest. A mark would add a schema column to every
store in three languages for an opt-in the command already is.

Status: retired 2026-09-28 by the ruling above.
Retires when: retired (see Status).

**L-15 (ruling, oracle changed): the registry runs git with no inherited
GIT_* variable, and never above the clone.** An inherited `GIT_DIR`,
`GIT_WORK_TREE`, `GIT_INDEX_FILE`, `GIT_CONFIG_COUNT`/`KEY_n`/`VALUE_n`,
`GIT_SSH_COMMAND` or `GIT_AUTHOR_*` could point the registry's git at
another repository, run hooks, or sign commits as someone else. Every git
child the registry starts (`git -C <clone> ...`, and `registry init`'s
`git clone`) now gets the caller's environment with every variable whose
name starts with `GIT_` dropped (`git_pathway_store.git_env`); `registry
init` then adds only `GIT_ALLOW_PROTOCOL`. No `GIT_CONFIG_NOSYSTEM` is set:
the oracle's behaviour does not need it, and the system config is the
administrator's. Config files still apply, so the identity of a publish
commit comes from the user's git config.

Beyond the finding, and deliberate: `git -C <clone>` also runs with
`GIT_CEILING_DIRECTORIES` set to the directory above the clone
(`str(Path(repo).resolve().parent)`; Go `filepath.EvalSymlinks` of the
absolute path, Rust `canonicalize`). git then looks for a repository in
the clone itself and never climbs: a repo path whose `.git` is gone,
inside another repository (a home directory under git, say), is no
longer a registry whose `publish` commits and pushes into the repository
above it. `status` there reads an empty `head_commit` and `publish` fails
at `git add`.

The L cases pass the harness's own `GIT_*` variables to the command, so
they also check that they are dropped; the command's git reads its
identity and default branch from the user config `XDG_CONFIG_HOME` names.
New cases: `status`, `pull`, `publish` and `init hostile git env` (a
decoy repository and hooks path the tree shows untouched, a `Mallory`
author the commit does not carry) and `status` and `publish inside another
repo`. Tests in Python, Go and Rust.

Status: in force.
Retires when: does not retire.

## 2026-09-28: stage L in Rust (registry, batch prove, release, deeds, strata)

The Rust `daisugi` carries what the Go binary carries for stage L:
`registry init|pull|publish|status|pull-and-tend` (`src/registry.rs`,
`src/cli/registrycmd.rs`), `batch prove` (`src/batch.rs`,
`src/cli/batchcmd.rs`), `release keygen|sign|verify` (`src/signing.rs`,
`src/cli/releasecmd.rs`), and the library parts no command reaches: the
deed ledger (`src/deeds.rs`), the strata store (`src/strata.rs`), batch
runs, pathway bundles (`src/bundle.rs`) and contract signing, proved
through its own `l-probe` (L-1). It follows L-1 to L-15. The L compare:
239 cases, Rust agrees on all 239, with none refused.

**L-R-1 (ruling): ed25519 through ring.** Signing and verifying use the
`ring` crate, already in the tree through rustls (now a direct
dependency, the locked 0.17.14). The key format is the oracle's (L-4),
read by a port of CPython's lenient `b64decode`. On the probe's 31 verify
cases (small-order keys, a key that is not a curve point, S of L or more,
short and long signatures) ring answers as OpenSSL does under
`cryptography`, and a unit test signs RFC 8032's first vector byte for
byte.

Status: in force.
Retires when: does not retire.

**L-R-2 (ruling): a bundle is read only in the form `yaml.safe_dump`
writes.** The registry reads a bundle file with the reader that admits
only the text `yaml.safe_dump` writes back byte for byte
(`pathways::dumped`); any other file is skipped as unparseable. The Go
binary reads more (its port of the loader's subset, then the same
safe_dump form). Both are stricter than the oracle, never looser (L-3);
no case tells them apart.

Status: in force.
Retires when: the binary reads all the YAML the oracle's loader does.

**L-R-3 (ruling): the probe's scratch file.** The contract probe saves and
reloads a trusted-signer registry file, as the oracle's does in a
temporary directory. The Rust probe makes that directory under its
working directory (the case's scratch HOME) and removes it, so nothing is
written to the system temporary directory.

Status: in force.
Retires when: does not retire.


## 2026-09-28: stage F in Go (the transcript parsers, ingest, `hook list`, `hook to-trace`)

`clients/f_cases.py` writes 199 cases from the oracle; `clients/f_compare.py`
runs them. The Go binary agrees on 198 and refuses 1, as ruled below.
Python read back every journal the binary wrote.

**F-1 (ruling): what stage F covers, and what was there before.** The
oracle's ingest is `ingest.py`, its parsers are the two in `parsers/`
(`claude-code` and `codex`, the only formats `get_parser` registers), the
decomposer is `decomposer.py`, and the evidence-inferred envelope is
`hook.infer_envelope` (ADR-0016). Before F, the Go binary carried `journal
parse` and `journal ingest` for both formats with the split through either
backend (C-14, C-16), `onboard` over the same parsers (K3), `hook
auto-tend` with the capture conversion (GD-8), and the decomposer, whose
one caller passes no skills and no MCP tools, so its inventory text is
fixed (K2). F adds `hook list` and `hook to-trace`, Python's answer where
the binary refused a transcript (F-3), a trace that carries a violation in
`hook to-trace` and `auto-tend` (F-7), and the cases. The oracle has no
parser for pi, OpenCode, Hermes or OpenClaw transcripts: `journal parse
--format pi` gives its unknown-format error and `onboard` warns "no
parser for harness", in both (cases `format *`, `onboard other harnesses`).

Status: in force.
Retires when: the oracle adds a parser; that parser is then stage F work.

**F-2 (ruling): a file read as Python's text reader reads it.** The
parsers and the capture commands open a file with `encoding="utf-8"` and
read it line by line. On bytes that are not UTF-8, Python raises
`UnicodeDecodeError` from the decode call that meets them. It decodes 8192
bytes at a time, with the bytes of a code point cut by the chunk before
held back, so the position in the error counts from the start of that
call's bytes, and the whole lines before that chunk are read first (an
error in them comes first). `pystr.DecodeStream` does the same; a table of
314 inputs written by the oracle's Python (`internal/pystr/gen_stream.py`,
`--check` in the CI generators job) proves the text and the lines.

Status: in force.
Retires when: does not retire.

**F-3 (ruling): the parse errors Python raises are answered.** `journal
parse` prints `Parse error: <text>` after the backend note and exits 2,
and `onboard` warns `parse failed for <path>: <text>` and goes on, for:
bytes that are not UTF-8 (F-2); an int past 4300 digits (the `ValueError`
`json.loads` raises, in a line or a Codex `arguments`); a tool name that
is not a str (`unhashable type`, or no attribute `startswith`); a tool
input that is not a dict where the parser calls `.get`; a Bash command of
another type (no `len()`, a list walked item by item, a dict's
`KeyError: 0`); an assistant content that is not iterable; a Codex
`arguments` that is not a str, a patch that is not a str, an action that
is not a dict, and a line `type` or function name that is unhashable. A
step value pydantic does not take (a path that is an int, a list or nan; a
split task that is not a str) is pydantic's error, raised when the steps
are built, after the split requests, as in Python. Other values are kept
and passed on as `str()` gives them (a Codex namespace or argv item that
is not a str, a WebSearch query that is a number).

Status: in force.
Retires when: does not retire.

**F-4 (ruling): a line nested too deep is refused.** Where Python raises
`RecursionError` on nesting depends on its C recursion limit and on the C
frames already on the stack, which differ by caller and by build: on the
reference box `json.loads` raises past 9,997 levels in a test program. The
binary refuses a line nested past 900 levels, before anything is written
(1 case).

Status: in force.
Retires when: the oracle's reader limits nesting at a fixed depth.

**F-5. model_dump in json mode.** An episode's step values go through
`model_dump(mode="json")`, which writes nan and the infinities as null
(pydantic's default), so an MCP argument that is NaN in the transcript is
null in the episodes file, in JSON and in YAML. The binary does the same.

Status: in force.
Retires when: does not retire.

**F-6 (ruling): `hook list` orders as `list.sort(reverse=True)` does.**
The key is `last_at or 0`. CPython reverses the list, sorts it stably and
reverses it again; below 64 sessions its sort is one run from `count_run`
extended by binary insertion, which fixes where a key that compares false
both ways (nan) lands, and the binary follows it exactly. From 64 sessions
on, a nan is refused; without one, any stable sort gives the same order.
Keys of two types that do not compare (a str and a number) raise
`TypeError`, as in Python; two lists are refused. A directory named
`*.jsonl` raises `IsADirectoryError`, a file that is not UTF-8
`UnicodeDecodeError`, a first or last row that is not an object
`AttributeError`, and an int past the digit limit `ValueError`: the binary
names the type and exits 1, as the oracle's traceback does. A captures
root that is a file lists nothing. The text form pads with Python's
format rules (code points) and prints `last_at` as `str()` gives it.

Status: in force.
Retires when: does not retire.

**F-7 (ruling): `hook to-trace`, and a trace that carries a violation.**
The command checks that the capture exists (`error: no capture at <path>`,
with the path as pathlib prints it, `..` kept), reads the decomposition
setting, and makes the journal before it reads the capture, as Python
does; so an exception the oracle raises on the capture (no records, a file
that is not UTF-8, a directory, a record that is not an object or has no
`step_type`, a plan that does not validate) leaves the journal made, and
the binary makes it too before it names the exception. A capture the
binary does not read the Python way is refused before anything is made. A
trace whose verify fails is journaled with its violations, worded as
`journal ingest` words them (C-14); one the verifier does not word is
refused. `auto-tend` shares this conversion: it now converts a session
whose trace carries a worded violation (GD-8 retired for Go), and skips a
session whose capture raises `ValueError` with its text, as Python's
`except ValueError` does. A record whose `step_type` is not a str adds
nothing, as in Python.

Status: in force.
Retires when: does not retire.

**F-8. `auto-tend` on a capture Python raises on.** The oracle's
`list_sessions` raises on a directory named `*.jsonl` or a file that is
not UTF-8, after `auto-tend` made the journal. The binaries used to skip
the directory and refuse the file; both now refuse both, before anything
is written. No case holds it.

Status: in force.
Retires when: auto-tend names the exception after making the journal.

**F-9 (harness): large files, and refusals where Python exits 2.** A
tree entry may be written as parts repeated a number of times, and a file
over 32 KiB, before or after, is compared by its size and SHA-256, so a
huge line costs the fixtures nothing. A refusal (exit 2, one line that
says nothing was changed, the tree as it was) is counted as refused even
where the oracle exits 2 with a parse error of its own.

Status: in force.
Retires when: does not retire.

**F-10. Found, not fixed: the Rust binary on the F cases.** Rust carries
`journal parse`, `journal ingest`, `onboard` and `auto-tend`, but not `hook
list` or `hook to-trace` (59 cases not ported). It refuses 59 more: 58
that the Go binary answers (F-2, F-3) and the deep line both refuse
(F-4). It disagrees on 6: it reads an int past 4300
digits as a number where Python raises (`claude long integer`, `codex long
integer argument`, `onboard long integer`), writes nan in an episodes file
where the oracle writes null (`claude mcp arguments nan`, F-5), and prints
its refusal of a split task that is not a str after the backend note, so
the refusal is not one clean line (`split reply task number`, `split reply
task null`). CI runs the F compare for Go only (`--max-refused 1`) until
Rust carries stage F.

Status: retired 2026-09-28 by stage F in Rust: the Rust binary agrees on
198 F cases and refuses the deep line (F-4), as the Go binary does.
Retires when: the Rust binary agrees on the F cases or refuses them cleanly.


## 2026-09-28: stage F in Rust (the transcript parsers, ingest, `hook list`, `hook to-trace`)

The Rust `daisugi` carries what the Go binary carries for stage F:
`src/gate/pymodel.rs` (`decode_stream`), `src/transcript`, `src/capture.rs`,
`src/cli/hookcapcmd.rs` and `journal_result` in `src/cli/journalparse.rs`.
It follows F-1 to F-9. The F compare: 199 cases, Rust agrees on 198 and
refuses 1, the deep line (F-4), as the Go binary does; CI runs it with
`--max-refused 1` for both. The six cases of F-10 agree: an int past 4300
digits is `json.loads`'s ValueError, NaN and the infinities are null in
the episodes file (F-5), and a split task that is not a str is pydantic's
error after the backend note. C-10's refusal of a transcript that is not
UTF-8 or holds a value the parsers raise on retires for Rust too.

**F-R-1 (ruling): the decode table is the Go client's.** `decode_stream`
is proved by `internal/pystr/testdata/stream.json`, the 314 inputs
`gen_stream.py` writes from the oracle's Python, read by a unit test from
the Go client's tree; `gen_stream.py --check` keeps that table current in
CI. The Rust binary keeps no copy of its own.

Status: in force.
Retires when: does not retire.

**F-R-2 (ruling): how `hook list` compares a captured_at.** Two ints
compare as big integers; an int and a finite float compare exactly
through the float's floor (an integral float as that integer); nan and
the infinities as Python orders them. From 64 sessions on the binary
sorts with the standard library's stable sort on the same comparison,
where Go uses `sort.SliceStable`; a nan there is refused, as in Go (F-6),
and so are keys that do not all compare (a str among numbers, a list, a
dict), where the Rust sort needs a total order and Python raises on the
first pair its merge meets.

Status: in force.
Retires when: does not retire.

**F-R-3 (ruling): the verifier raising inside a capture conversion.**
`hook to-trace` and `auto-tend` share `convert_capture`, which verifies
the plan with `journal_result`, as `journal ingest` does. Where the
binary's verifier raises on a plan (a shape the oracle's verify would
answer), the conversion is refused before anything is written, with the
session named, as `journal ingest` refuses such an episode. No case
reaches it.

Status: in force.
Retires when: the binary's verifier raises only where the oracle's does.

## Owner rulings in code, 2026-09-30: audit mode, lexical default

**AU-1 (ruling): the old mode word gets one line, then a refusal.**
`--mode audit|enforce` in all three clients. `--mode shadow`, in any form
argparse reads (`--mode=shadow`, `--mo shadow`), is refused by the parser
with `argument --mode: 'shadow' is now 'audit': use --mode audit` (exit 2,
so each host format denies, as for any bad argv). Typer's `gate check
--mode` says the same in its usage error. There is no alias.

Status: in force.
Retires when: does not retire.

**AU-2 (ruling): `install --shadow` is refused with one line.** The flag
pair is `--enforce/--audit`. `--shadow` is a hidden option that fails
with `Invalid value for '--shadow': shadow mode is now audit mode: use
--audit`. The Go and Rust parsers carry it as a hidden option too, so
click's suggestions stay the same.

Status: in force.
Retires when: does not retire.

**AU-3 (ruling): the log directory.** The gate writes `<root>/audit/`. The
report reads `<root>/shadow/` first, then `<root>/audit/`, one session's
file or every `*.jsonl` in each: the old directory holds the operator's
history, so it is read, never moved. Old records keep `"mode": "shadow"`;
no reader branches on the value. Nothing writes `<root>/shadow/` now.

Status: in force.
Retires when: the owner moves or deletes the old directory.

**AU-4 (ruling): a config value of `shadow`.** `gate_mode: shadow` in
config.yaml is an unknown mode, so it resolves to audit, as every unknown
value always has. The verdicts do not change, so no line is printed.

Status: in force.
Retires when: does not retire.

**AU-5 (ruling): a hook that still passes `--mode shadow`.** The gate now
refuses that argv, so the hook denies every call. `gate status` and
`config` count such a hook as one that may enforce (`unknown`), never as
audit, and `gate status` prints one line on stderr naming the file and the
fix (`daisugi install --gate --uninstall`, then `daisugi install --gate`).
`install --gate` does not rewrite such a hook; it warns, as for any gate
hook with a different mode.

Status: in force.
Retires when: does not retire.

**AU-6 (ruling): coppice reads both words.** A session-tree verdict whose
mode is `audit` is `watching`; `shadow` is `watching` too, because trees
already written, and any daisugi binary not yet rebuilt, still say it.
This is reading data, not a second name for the mode.

Status: in force.
Retires when: no session tree on disk says `shadow`.

**AU-7 (ruling): `gate audit` keeps its name.** `daisugi gate audit` runs
the adversarial corpus; it is not the audit-mode log. `gate report`
summarizes the audit log. The owner may rename one of them.

Status: in force.
Retires when: the owner renames `gate audit` or `gate report`.

**DS-3 in code.** The default `matcher_model` is `lexical` in all three
clients (Config, the saved defaults block, the status fallback). A config
that names `all-MiniLM-L6-v2` keeps meaning MiniLM in Python and keeps its
refusal in the ports. `daisugi tiers setup --matcher potion` sets potion in
one step; plain `tiers setup` says so. Cases that relied on the old default
now name MiniLM in config.yaml.

Status: in force.
Retires when: does not retire.

**H-1 (harness fix): a PID inside a `\uXXXX` escape.** The gateway and
garden harnesses replaced each child PID in the dumped JSON text; a PID
of 2026 or 2192 turned the escape of an ellipsis or an arrow into
`\u{PID1}`, and the Rust gateway compare crashed in CI. `ports.sub_number`
never replaces inside an escape.

Status: in force.
Retires when: does not retire.

**H-2 (harness fix): a request key over a minted id.** A request key
hashes the raw prompt; when the prompt holds an id minted at random
(auto-tend's distill prompt), the key differs on every run. The garden
compare then compares the normalized text and drops the key.

Status: in force.
Retires when: does not retire.

## 2026-09-30: dialect stage 0 (shell write paths in the algebra, the keep_unchanged words)

Owner rulings in force: DI-10 (expose each shell step's write paths to the
predicate algebra, with a quantifier), DI-1 (audit first, then enforce),
DI-2 (no shadowing), DI-9 (a word use needs a pinned dialect). Built in
Python, Go (verify, gate, daisugi) and Rust (conform, gate, daisugi).
Yellow paper §2.3.1 and §2.4.

**DL-1 (ruling): the field is derived, and it names only what the shell
opens.** `writes(s)` (`write_paths.py`) is a file_write step's path, and
for a shell step every literal write redirect the decomposition finds,
also inside the payloads of command-taking wrappers (`sh -c`, `xargs`,
`find -exec`, `env`), as the permission stage walks them. The null
device and the standard streams are left out, as the file_write check
leaves them out. Each path is normalized with `posixpath.normpath`, so
`./src/a` and `lib/../src/a` are `src/a`. The field is not stored in the
step record; only `forall_writes` reads it, so plans, journals and
conformance cases do not change. As first built, a command's operand
writes (`cp`, `mv`, `tee`, `sed -i`) and interpreter work were not in it.
DL-11 adds the operand writes of the known writers. Interpreter work
(`python`, `awk`, `sed` without `-i`, `sudo`), a command that is not a
known writer, MCP tools, skills and delegated agents stay out of the
field.

Status: in force, as DL-11 widens it.
Retires when: does not retire.

**DL-2 (ruling): an unknown write set is false, a misplaced quantifier an
error.** A shell step whose redirects cannot be read (the grammar refuses
it, the shell extra is missing, wrappers nest past depth 4) makes
`forall_writes` false. At the plan root, inside `forall_outputs` and
inside another `forall_writes` the quantifier raises (`forall_writes
must stand inside forall_steps or exists_step`), an evaluation error and
so a violation, never a silent pass.

Status: in force.
Retires when: does not retire.

**DL-3 (ruling): the symbolic encoding.** On a symbolic step (vacuity,
subsumption) `forall_writes(φ)` is `n ⟹ φ(w)`, with `w` the free path
`<prefix>__w__path` and `n` the free Bool `<prefix>__writes_nonempty`. It
is a tautology exactly when `φ` is, and never a contradiction, since a
step may write nothing. In Rust's SMT text `n` is a declared Bool, not a
soft node, so subsumption does not pin it.

Status: in force.
Retires when: does not retire.

**DL-4 (ruling): the pin is in the operator's config, not the envelope.**
DI-9 asks that an envelope using words pin its dialect by hash. An
envelope field is a schema change in three clients, a "hard" ruling that
waits for the owner. So in stage 0 the operator pins: `dialect_enforce`
in config.yaml names the dialect hash that is enforced. No pin: audit.
The pin equal to this build's hash: enforce. Any other pin: every word
use is a violation (`dialect_pin_mismatch`), because the operator pinned
definitions this build does not have. A config that cannot be read pins
nothing, as an unreadable `gate_mode` falls back to audit. `gate status`
shows the hash to pin. The model-written opaque invariants pin nothing
and are audited, which DI-1 asks for. The pin was weaker than the hook's
`--mode`, which sits in argv, while no rule protected the config.yaml
beside the gate root; DL-15 adds that rule. A command that removes the
data directory above the file is still not caught (DL-15).

Status: provisional.
Retires when: the owner confirms it, or envelope-level pins are built
(design stage 3).

**DL-5 (ruling): what is a word use.** An enforced invariant with no
`expr` whose `type` is a word or a synonym, with its `target` as the
argument; a missing or empty target is `**`. An `alias` reference in an
`expr` is unchanged: with no registry it is an unresolved definition, a
violation. A postcondition that names a word is not a word use. An
invariant with `enforce: false` is not audited. Under audit the opaque
handling stays exactly as before (strict stakes still reject the opaque
type), so the audit changes no verdict; under the pin the word replaces
it.

Status: provisional.
Retires when: the owner confirms it, or the envelope prompt switches to
words (design stage 3c).

**DL-6 (ruling): the glob translation.** `glob_regex(g)` matches exactly
the normalized paths the file-scope matcher (`verify._match_glob`)
matches with `g`: a `/**` suffix with its prefix taken literally after
normpath, `**` segments over zero or more segments, `*` and `?` inside
one segment. It ends in `\Z`, not `$`, which in Python also matches
before a final newline. Refused, and so a would-deny or a violation: an
empty glob, `[` or `]`, more than 1,024 characters, 16 stars or 4 `**`
segments, a `/**` glob whose prefix normalizes to `.`. `globs.json` holds
400 globs (11 refused) against 99 paths; each port's regex is equal to the oracle's
and matches the same paths. Go's RE2 and Rust's regex crate spell `\Z`
as `\z`; both clients translate it in one pass over the pattern (in Go
this also stops `\\u0041` being read as a `\u` escape).

Status: in force.
Retires when: does not retire.

**DL-7 (ruling): where a would-deny is recorded.** A word's would-deny is
a verify warning that starts with `dialect audit: `, in the form
`invariant 'T' is keep_unchanged('G'); WHY; enforcing would deny`, where
WHY names the first step and the path it writes. A conformance case
carries `expect.word_audit` only when a word would deny, so every other
case keeps its id; a verdict always carries it, and a case with the key
compares it exactly. The dialect case file carries the key always. The
gate's audit log adds `word_audit` only when non-empty. `gate report`
adds `word_would_deny` and `word_denied` and one `WORD` line per audit
line; `gate status` adds a dialect line and a `dialect` object. The
status count (`gate.word_would_deny`) skips a log it cannot read, where
the report raises.

Status: in force.
Retires when: does not retire.

**DL-8 (ruling): the synonyms.** 23 invariant type names from the owner's
journal copy (names only) that say "these files stay unchanged":
`file_unchanged`, `file_immutable`, `file_immutability`,
`file_preservation`, `file_content_preservation`, `immutable_source`,
`no_file_modification(s)`, `no_modification(s)`, `no_file_mutations`,
`no_file_writes`, `no_filesystem_mutation`, `filesystem_readonly`,
`files_not_modified`, `no_local_modification(s)`,
`no_project_modification`, `no_source_modification`, `read_only`,
`read_only_access`, `read_only_operation(s)`. Left out as too vague or
about something else: `no_mutation(s)`, `immutability`, `preservation`,
`read_only_analysis`, `read_only_verification`,
`no_unintended_file_modification`, and every name about the network,
git, deletion or data. Each synonym unfolds to the same term, and a test
proves the unfoldings equal with Z3.

Status: provisional.
Retires when: the owner reviews the list, or the stage 2 miner proposes
synonyms merged by proof.

**DL-9 (port): the frame depth of the write-path decompositions.** The
oracle decomposes a shell command again under `forall_writes`, deeper on
the stack than the permission stage (frame 17 for a hand-written
quantifier, 19 for a word, 15 for the witness, 2 more per wrapper
level), so a long `&&` chain can pass the permission stage and raise
RecursionError there. The Go gate passes those frame numbers, counting
`all(...)` generator frames; the Rust gate pushes a frame per Python
call. Gate cases scan chains of 960 to 980 links: 0 disagreements.
GT-21 holds.

Status: in force.
Retires when: does not retire.

**DL-10 (known limit): the conform clients, TS and Lean.** The Go and
Rust conform clients decompose with no stack bound, so for a chain near
the recursion limit their `word_audit` text can differ from the
oracle's; no case holds one. The Go gate's frame numbers assume the
python verifier client, as its permission stage already does (with
another client the oracle runs one frame deeper). The TypeScript and
Lean clients do not know `forall_writes` and fail closed on it.

Status: in force.
Retires when: a case shows the difference, or TS and Lean learn the
quantifier.

## 2026-09-30: dialect follow-ups and two CI fixes

**K3-13 (ruling): the K3 guard is 4.** CI ran the K3 compare with
`--max-refused 3`, and each port now refuses or does not port 4 cases.
The fourth is `onboard minilm config`, added with the lexical default: a
config that names `all-MiniLM-L6-v2` keeps its refusal in the ports
(DS-3). The other three are `setup remote` and `tiers stats` (K3-9), and
`setup matcher over a bad config`, where the oracle ends in a pydantic
traceback and the port refuses and rewrites nothing. The guard is 4 in
the workflow.

Status: in force, narrowed 2026-10-03: `setup remote` and `tiers stats`
are ported (TS-1, SR-1), and the guard is 2.
Retires when: the ports carry MiniLM.

**DL-11 (ruling): the operand writes.** `write_paths.WRITERS` names 15
commands whose operands are write paths: `cp`, `mv`, `ln`, `install`,
`tee`, `sed`, `truncate`, `dd`, `rsync`, `touch`, `mkdir`, `rm`, `rmdir`,
`unlink`, `shred`, each with closed flag sets (GNU spellings; a flag
outside the set, an abbreviated long flag, or a missing value makes the
write set unknown). A head is found after `NAME=value` words and by its
last path part (`/bin/cp`). The rules: the destination of `cp`, `ln`,
`install` and `rsync` is written, the last operand or the `-t` directory;
a plan cannot ask whether it is a directory, so each source also gives
`DEST/basename(SOURCE)`, unless `-T`. `mv` also writes its sources: they
are unlinked, which changes them (the brief said the destination only;
for `mv` that would miss the file it removes). Every operand of the other
commands is written, and of `install -d`. `sed` writes only with `-i`,
then each file after the script (the first operand unless `-e` or `-f`
gives it) and each backup `FILE+SUFFIX`; a suffix with `*` or `/` is
unknown. GNU reads `-ie` as the suffix `e`, and so does this. `dd` writes
its `of=`; any word that is not `KEY=VALUE` with a known key is unknown.
`rsync` writes no remote destination (`HOST:PATH`, `rsync://`), and with
`--remove-source-files` its local sources. Unknown: a writer whose text
holds a word the shell changes (`$`, a backtick, an unquoted glob or
brace, `~user`: `effects._unsafe_words`), a line `shlex` cannot split,
and a writer under `xargs` or `find -exec`, which add operands the line
does not show. The sanctioned sinks are left out, as for redirects. A
recursive command shows its top path only, so `keep_unchanged('src/*.py')`
does not see `rm -r src`; `src/**` does. Interpreters, commands not in the
table, MCP tools and delegated agents stay out of scope (DL-1).

Status: in force.
Retires when: does not retire.

**DL-12 (ruling): a line that moves its cwd.** A shell line whose
decomposition runs `cd`, `pushd` or `popd` and whose write list at that
level holds a relative path has unknown writes, so `forall_writes` is
false. The paths after a `cd` are relative to a directory the plan does
not state. A `cd` inside `sh -c '...'` counts for that payload only; a
`cd` inside `$(...)` counts for the whole line (the fail-closed side).

Status: in force.
Retires when: the write paths follow each `cd` the way the effects check
does.

**DL-13 (ruling): a word is placed from the call's cwd.** Envelopes hold
no workspace, and the file-scope matcher places no glob: a relative glob
never matches an absolute path. So `verify(..., dialect_base=b)` takes
the base, and the gate passes the call's cwd (an absolute path, after
normpath; any other value places nothing). A relative target is joined
to it (`dialect.resolve_target`); the unfolded `forall_writes` resolves
each relative write path against it (`write_paths.resolve_writes`), and
a write path that starts with `~` is then unknown. A target that starts
with `/` or `**` is left as written, so the default `**` still names
every path. Refused (a would-deny, or a violation under the pin): a cwd
that holds `*`, `?`, `[` or `]`; a target that starts with `~`; a target
that does not end in `/**` and has a `.` or `..` segment (a `/**`
target's prefix is normalized by `glob_regex`, so `./**` names the cwd).
Hand-written `forall_writes` terms are never placed. The conformance
case carries the base as `options.dialect_base`; `verify_via` gives it to
the oracle only, as it does the pin. Plan-time verification (`daisugi
run`, the journal) has no base and places nothing.

Status: in force.
Retires when: envelopes carry a workspace (design stage 3).

**DL-14 (ruling): the write paths are part of the dialect hash.** The
canonical dialect JSON gains `"write_paths": 2` (1 was the redirects of
stage 0). DL-11 to DL-13 change what every word means, and a pin must
name the definitions it enforces, so the hash is new: `108a89a256798dfc`
(it was `79cea76bfa77cf4b`). A config that pins the old hash now makes
every word use a violation, as for any foreign pin; `gate status` shows
the hash to pin.

Status: in force.
Retires when: does not retire.

**DL-15 (ruling): daisugi's own config.yaml is a hard-deny path.** The
floor's guard (`floor_config.Guard`) covers the config.yaml beside the
gate root in force and `~/.opendaisugi/config.yaml`, each as written and
as resolved, with its own refusal (`this is daisugi's own config. Edit it
yourself.`), in every mode, after the floor's own rule. As for the
floor, a write the gate can place there is denied, and so is a shell
line or an MCP argument that names the file, a read included; a Read
tool call is not. The floor's rule for a relative word after a `cd` the
gate cannot follow (or with no cwd, as for an MCP argument) denies a
word that holds a root's last part; for this guard that part would be
`config.yaml`, which would deny `cd .. && cat config.yaml` and an issue
body that mentions the name. So here the word must hold the file's
directory name and then `config.yaml` (`.opendaisugi/config.yaml`). Not
caught: a bare relative `config.yaml` after such a `cd`, from inside the
data directory, and a command that moves or removes a directory above
the file (`rm -r ~/.opendaisugi`, `mv` of the data dir), as for the
floor's directories. daisugi's own commands that an
agent can run and that rewrite config.yaml (`tiers setup`, `install
--router`) load the file and save it back, so they keep
`dialect_enforce` as it is.

Status: in force.
Retires when: the pin moves into the envelope (DI-9, design stage 3).

**DL-16 (ruling): `daisugi status` counts plans and calls.** The Trust
section adds `dialect would-denies: N plans in the journal, M calls in
the gate's audit log`, and the JSON adds `word_would_deny_plans` and
`word_would_deny_calls`. A plan counts when its trace's
`result.warnings` holds a line that starts with `dialect audit: `, once
per trace. Read-only; a missing journal is 0. A trace file that cannot
be read, is not UTF-8, does not load as YAML, or whose warnings are not
a list is skipped; a trace whose text does not hold `dialect` is not
loaded. The calls are `gate.word_would_deny` over `<data dir>/gate`
(DL-7). A plan is verified with no base (DL-13), so a plan whose steps
write absolute paths under a relative target counts low, as the calls
did before DL-13.

Status: in force.
Retires when: does not retire.

**DL-17 (port): what the ports read differently.** The Go and Rust ports
read a trace with their `safe_dump`-form reader, which refuses a text
`yaml.safe_dump` would not write; such a trace is skipped, where the
oracle may load it and count it. Only a hand-edited trace can differ. The
ports' `_unsafe_words` reads bytes, as the gate's effects port already
did. The Rust gate's `unsafe_words` and the Go gate's `unsafeWords` now
call the one copy in the conform packages, which the write paths use.

Status: in force.
Retires when: a case shows a difference.

**CP-1 (fix): coppice layout saves take turns.** Two saves at once could
marshal in one order and rename in the other, so an older tree replaced a
newer one on disk (the failing restore test on the CI runner lost a
forked pane). `layout.Tree.Save` holds a save mutex from the marshal to
the rename. The idempotent attach test reads to each response by id, as
the other attach tests do, since the first attach's frame can reach the
wire between the two responses. Each test passes 20 runs.

Status: in force.
Retires when: does not retire.

## 2026-09-30: router parts 0 and 4 (the large-read graft, the `delegate` tool, the weekly measure)

**RP-1 (ruling): the graft rule is a JSON file in the gate root.** Per
GR-5, rules live in `<gate root>/grafts/`, one per `*.json` file, read in
file-name order; the first rule whose state is `audit` or `active` is in
force, and `router status` names every other one with the reason. The
fields: `id` (a full match of `[A-Za-z0-9._-]{1,64}`), `version` (an
integer of 1 or more), `shape` (`deny_redirect`, the one shape built),
`state` (a string), `match.tool` (`Read`), `match.file_lines_over` (an
integer of 1 or more, default 350), `redirect.tool` (`delegate`, the
default), `worker.choose` (`router`, the default) and
`worker.allow_remote` (a bool, default false). Other keys are ignored. A
file that is not UTF-8 JSON (a BOM included, or a directory named
`x.json`), or whose value is not such a rule, is not used. With no file,
the gate is as before: no existing gate case changed. The design showed
YAML; the gate root holds JSON envelopes already, and each port reads JSON
the oracle's way. A `deny_redirect` rule can only narrow what runs, so the
proof at install (design-grafts.md, "The proof at install") has nothing to
prove for it. No graft installer is built: rules are data a person or a
bot writes, so GR-3 (refuse beside an unknown rewriting hook) is not
exercised yet. 2026-09-30: the installer is built (GR-R-1); the proof at
install is not, since a `deny_redirect` rule has nothing to prove.

Status: in force.
Retires when: the proof at install is built for a shape that widens what runs.

**RP-2 (ruling): what a matched read is.** The payload's tool name is
exactly `Read` and the decision's step type is `file_read` (not `Grep` or
`Glob`), under the `claude` format only (Codex uses it too). It is the one
host format where the delegate's tool name, `mcp__opendaisugi__delegate`,
is known: under pi an unknown tool is `pi/<name>`, and the names hermes,
openclaw and opencode give an MCP tool are not verified, so the gate could
not check the redirected call there. Only an allowed call with no would-deny is grafted,
so a graft never turns a deny into an allow, and a read an audit-mode gate
would deny is not grafted. The record's path (the hook's join with the
call's cwd) must be absolute. An integer `limit` (not a bool) with
`0 < limit <= threshold` is not matched: it is the way to exact text, and
the way out when the worker is down. A float or a string `limit` is no
escape. The file must measure (RP-3) with more lines than the threshold.

Status: in force.
Retires when: does not retire.

**RP-3 (ruling): the measure of a file.** The file is opened with
`O_RDONLY | O_NONBLOCK | O_CLOEXEC` and checked on the descriptor: a
regular file (a FIFO or a device never stalls the gate), at most
`MAX_DELEGATE_BYTES` = 524,288 bytes by `fstat` and while reading, no NUL
byte, strict UTF-8. Its lines are its `\n` bytes plus one for a last line
with no `\n`; `\r`, `\v`, U+0085 and the other breaks `str.splitlines`
takes do not count. A path that cannot be encoded or holds NUL "cannot be
opened". A symlink is followed. The gate and the tool use the one cap, so
the gate never sends the model to a tool that then refuses the file.

Status: in force.
Retires when: does not retire.

**RP-4 (ruling): the redirect verdict.** An active rule's redirect has
`allow` false, `would_deny` false and the allowed call's tier (silent);
its reason names `mcp__opendaisugi__delegate`, the file's lines, the
threshold and the limit escape. The audit record gains `graft`:
`rule_id`, `version`, `shape`, `state`, `lines`, `file_lines_over`, the
`worker` when there is one, `applied` and `why`. The redirect is not built
with `_deny` and is not a would-deny, so it stays out of the false-deny
count and never reaches an operator ask. It acts in both gate modes: audit
mode keeps its promise for safety verdicts, and the graft's own state
(`audit` records "would redirect", `active` redirects) decides a cost rule
(design-grafts.md, "Keep grafts out of the false-deny rate"). A redirected
read reaches neither the captures mirror nor a checkpoint; it did not run.

Overturned 2026-09-30 by the controller: an audit-mode gate never denies
for a graft, whatever the rule's state. A cost deny must never surprise an
audit-mode user. Such a read goes through, and the record says
`applied: false`, `why: "the gate is in audit mode: the read is not
denied"`, with the worker and the delegate check made as before, so the
record still shows what enforce mode would do. Only `graft big read
(audit)` changed.

Status: in force (as overturned).
Retires when: does not retire.

**RP-5 (ruling): no worker, no redirect, said once.** With no worker (no
local model, a remote worker without a grant, physical stakes, a model
with no wire or no key), or with an envelope that would deny the delegate
call itself (the gate checks it as RP-7's MCP call, with the file's path,
before it redirects), the read goes through as the gate decided it. The
audit record carries `graft.applied: false` and the router's reason; the
one place it is said to the operator is `daisugi router status` ("worker:
none. <reason>. With no worker, reads over the threshold go through the
normal gate."). The gate writes nothing to stderr on an allow: Claude
Code, pi and OpenCode ignore it there. A worker that is set up but does
not answer is not probed by the gate (the gate never calls a model): the
tool then answers `ok: false`, and the limit escape (RP-2) remains.

Status: in force.
Retires when: does not retire.

**RP-6 (ruling): the router's worker for a delegate call.** The candidate
is the local rung `daisugi tiers setup` records: `<data dir>/local_tier1.json`,
read as `load_configured_tier1` reads it (`openai/` added to a bare model
name when `base_url` is a string). The model's wire URL
(`llm_client.resolve_wire`, with the environment) decides where it runs.
Its host is read by a written-out rule, the same in each port: after
`://` up to the first `/`, `?` or `#`; after the last `@`; inside `[...]`,
or before the first `:`; lower-cased in ASCII only. `localhost`, `::1` and
a dotted IPv4 address in 127.0.0.0/8 with no leading zeros are local and
need no grant. Any other host is remote (GR-2): the file's text leaves the
machine, an effect at the permanent tier, so it needs the rule's
`allow_remote` and an envelope with `network: true` that names the host in
`network_hosts` (compared lower-cased in ASCII); an empty list, which
means any host, is not a grant. Under physical stakes there is no worker.
No config field was added: the worker is the model already qualified.

Status: in force.
Retires when: router part 1 or 2 makes one router interface for turns, steps and delegate calls.

**RP-7 (ruling): the gate checks the delegate call's effects.** An
allowed `mcp__opendaisugi__delegate` call (the MCP allowlist admitted
`opendaisugi/delegate`) must also pass as what it does. Its `path` must be
a string and absolute; the gate does not join it with the cwd, so the gate
and the tool never place the same text differently (`~` and `$` paths are
not absolute). Under physical stakes it is denied. `normpath(path)` must
pass the envelope as a `file_read`. When the router (RP-6, from the gate's
own environment and envelope) picks a remote worker, the worker's URL must
pass as a network step; that deny is permanent. The MCP allowlist alone
never grants the read. The server name is the one `daisugi install`
registers, `opendaisugi`; a server registered by hand under another name
is another server to the gate, admitted only by its own allowlist entry,
and its calls get no read check. A hard-deny rule that refuses a path in
an MCP argument (the config guard, DL-15) refuses the delegate call too,
where a `Read` of that path may pass: the delegate call is the stricter.

Status: in force.
Retires when: does not retire.

**RP-8 (ruling): the `delegate` tool.** `delegate(path, question,
mode="bulk_read")` on the `opendaisugi` server (RT-2). The refusals, in
order: a mode other than `bulk_read` (RT-1's code-write draft is not
built), a blank question, a path that is not absolute (then `normpath`),
the gate's default envelope unreadable, physical stakes, a file that does
not measure (RP-3), no worker (RP-6), a worker error, a reply that does
not read (RP-9). The tool reads the gate root's `default` envelope for the
stakes and the remote grant, since an MCP server does not know a session's
`--session` pin; the gate's check of the call (RP-7) uses the session's
own envelope. The result's keys are fixed: `ok`, `mode`, `path`, `reason`,
`worker`, `lines`, `answer`, `answer_cut`, `quotes`, `dropped`,
`untrusted`, `exact_text`.

Status: in force.
Retires when: code-write (RT-1) is built.

**RP-9 (ruling): the worker call and the quote check.** The system message
is `delegate.WORKER_SYSTEM`, copied verbatim into each port; the user
message is `Question: …`, the file's base name (never its path, which
names the machine) and the text between `<file>` and `</file>`.
`max_tokens` 2048, `json_object` on the chat wire, no temperature, a
120 s timeout. The reply loses one surrounding code fence and must be a
JSON object with a string `answer` (cut to 4,000 code points) and `quotes`
(absent is none; not a list is unreadable). Only the first 20 items are
read. A quote is dropped, and counted in `dropped`, when it is not a
string, is blank, is longer than 2,000 code points, or is not an exact
substring of the file's decoded text; a repeat of a kept quote is left out
and not counted. No line numbers are returned. The answer and quotes are
returned as data with the `untrusted` note, and are never spliced into a
command.

Status: in force.
Retires when: does not retire.

**RP-10 (ruling): the delegation journal.** `<data dir>/router/delegations.jsonl`,
made 0600, one line per call, refused or not. `worker_dollars` is 0.0 for
a local worker; for a remote one, the gateway's price for the model's name
after the first `/`, else null (unpriced). `frontier_tokens_kept` is
`max(0, ceil(file bytes / 4) − ceil(returned bytes / 4))`, the returned
bytes being the UTF-8 of the answer and the kept quotes (a lone surrogate
counts its 3 bytes); `frontier_dollars_kept` is that times $3 per million
at the cache-write rate (1.25), once. Both are marked `estimated: true`:
the later cache reads of the kept tokens are not counted, nor the extra
frontier turn that deny-and-redirect costs. `task_ok` is null: no source
labels a task yet (design-ranking.md), and `router status` shows it as
unknown.

Status: in force.
Retires when: a label source fills `task_ok`.

**RP-11 (ruling): each gateway turn records its time.** The turn record
gains `elapsed_ms`, its last key: the time from `prepare()` to the end of
the answer, rounded to 3 places; null for a turn recorded outside the
proxy (the voice cleanup). The gateway cases normalize it to `{MS}`.

Status: in force.
Retires when: does not retire.

**RP-12 (ruling): `router status` by week.** Three sources: the turn
journal, the delegation journal, and every gate audit log record (by file
name) that holds a `graft` object. A week is an ISO week in UTC, from a
strict `YYYY-MM-DDTHH:MM:SSZ` stamp (ASCII digits, a real date and time)
or from the floor of a Unix time in the years 1 to 9999; a record with
neither is skipped. An integer field counts only when its size is at most
2**53; a number counts only as a finite float (an integer too large for a
float is none). The newest 8 weeks are shown. `tokens_saved` is the turns'
frontier tokens saved plus the delegations' tokens kept; `dollars_saved`
is the turns' dollars saved plus the delegations' dollars kept, less the
worker dollars; `estimated` is set when any turn is estimated or any
delegation succeeded. `escalations` is 0 and the JSON says
`escalation_built: false`: router part 1 is not built. GR-6's measure,
billed cost per successful task, needs task labels (RP-10); until then the
week shows billed dollars beside quota tokens, and task outcomes as
passed, failed and unknown. The `delegate` section names the rule in
force, each rule not in force with the reason, each rule file not used,
and the worker, or why there is none (RP-5).

Status: in force.
Retires when: GR-6's promotion meter is built.

**RP-13 (port): what the ports read differently.** The Go gate hands a
call back (a deny that says the port cannot decide it) when a rule file or
`local_tier1.json` holds JSON its reader does not model. The Go and Rust
tools hand back a worker reply nested past 900 levels and an answer with a
lone surrogate; Rust also a file holding a character of its surrogate
block. Each port reads a journal as Python's `read_text` does, with
universal newlines: a lone `\r` ends a line.

Status: in force.
Retires when: a case shows a difference.

**RP-14 (ruling): the generators rerun named cases.** `k3_cases.py --redo
TEXT` and `gateway_cases.py --redo TEXT [--redo-in-expect] [--range A:B]`
rerun the cases a change touches and keep the rest, so no heavy run passes
about 7 minutes. The gateway normalizer keeps a stamp that is not a real
date as written.

Status: in force.
Retires when: does not retire.

**RP-15 (harness): fixed dates stay a year from the run.** The gateway
normalizer writes a time within an hour of the run as `{ISO}` or `{NOW}`.
The router measure cases held fixed stamps around 2026-09-30, so on that
day `router status measure` and its JSON form read differently by the
hour they ran. Their dates move 52 weeks back to 2025, which keeps each
weekday and ISO week number, so the weeks the cases test (a Sunday's last
second, a Monday's first) are the same weeks. A fixed date in a gateway
case goes in a past year; a time the case means as "now" is laid out
relative to the run.

Status: in force.
Retires when: does not retire.

## 2026-09-30: the graft installer (GR-3)

**GR-R-1 (ruling): `daisugi graft install|status|remove`.** A hidden
command group writes, shows and removes `<data dir>/gate/grafts/<id>.json`
(`--id`, default `big-read`; the rule id pattern of RP-1). `install` takes
`--state` (`audit`, the default, or `active`), `--file-lines-over` (an
integer of 1 or more, default 350) and `--allow-remote`, and writes the
full rule as `json.dumps(rule, indent=2)`, through a temporary file and a
rename. When the file already holds a rule with the same id, the version
is that rule's plus one; else 1. `remove` of a missing file says so and
exits 0. `status` names every rule file, the one in force, each file not
used with the reason, and every rival hook; `--json` gives the same.

Status: in force.
Retires when: does not retire.

**GR-R-2 (ruling): what a rival hook is (GR-3, the refusal half).** The
installer reads the PreToolUse hooks of four files, in order, each once:
`~/.claude/settings.json`, the working directory's `.claude/settings.json`
and `.claude/settings.local.json`, and `~/.codex/hooks.json`. A missing
file has no hooks. A hook is known when its command is the gate in a form
read (`config.gate_hook_kind` is `gate`) or the capture hook
(`config.is_record_hook`). Any other hook, whatever its matcher and
whatever its type, may rewrite a tool's input after the gate saw it, so
the gate could not vouch for the input that runs: install refuses with one
line that names the file and the hook's command as a JSON string (or the
hook as JSON when it has no string command), exits 1 and writes nothing.
A file that is not UTF-8 JSON (a BOM included) refuses too, since its
hooks cannot be checked. Entries of the wrong type are skipped, as the
harness skips them. The refusal never touches the gate; the post-call
input check on Claude Code, GR-3's detection half, is not built. 29 CLI
cases; Go and Rust agree on all.

Status: in force.
Retires when: the post-call input check is built, with GR-3.

**GR-R-3 (harness): the gate cases' scratch root must be short.** A gate
case's cut `detail` (the coppice report) holds the envelope's globs, which
name the run's scratch root. With a longer root the cut falls inside the
second copy of the root, and a machine path reaches the fixture. The case
`graft delegate call dotdot out` was written again from a short root
(`DAISUGI_GATE_SCRATCH`), which gives the fixture's text as it was.

Status: retired 2026-10-08. Python, Go and Rust write the same cut text
(each cuts at 200 characters after the path is normalized), so no port
had a bug: the harness did. Every gate, K2 and weave case root now has
one length, `fixture_paths.ROOT_LEN` (56), whatever the scratch
directory, and a root the gate cut short is written as `{ROOT-CUT}`. The
two dotdot cases were written again at that length. A scratch directory
too long for it stops the run with a message.
Retires when: retired.

## 2026-09-30: `daisugi weave` (ADR-0022, WV-1 to WV-5)

**WV-R-1 (ruling): the command and the plan file.** `daisugi weave PLAN -e
ENVELOPE` (hidden) takes `--data-dir`, `--yes`, `--json`, `--resume`,
`--rerun STEP` (repeatable) and `--max-parallel N`. PLAN is an `ActionPlan`
as JSON (WV-3, one format): the file's bytes decoded as UTF-8, `json.loads`,
a JSON object, the model. A failure prints "Tried to read the plan P." with
"It did not parse: " and the first line of the error, exit 2. The envelope
is read as `run` reads it. Output, exit codes (0, 1, 2, 130) and the journal
are `run`'s, with the additions below. The plan hash is `sha256:` and the
SHA-256 of the plan file's bytes, so a file written again in another form
is another plan.

Status: in force.
Retires when: does not retire.

**WV-R-2 (ruling): the slot declarations (WV-1).** A step may carry
`outputs` (`{name: type}`) and `inputs` (`{field: "STEP.SLOT"}`), keys the
`ActionPlan` model ignores, so a journaled plan is dumped as before. A name
is `[a-z][a-z0-9_]{0,31}`; the types are `path`, `string`, `number`,
`list[path]`, `list[string]` and `list[number]`. Only `shell`, `file_read`,
`network` and `task` steps declare outputs. A slot fills only
`file_read.path` and `file_write.path` (a `path`), `file_write.content` (any
type) and `network.url` (a `string`). It never fills a command, a prompt, a
program or an argument list, so free text never flows from one step into
another step's program or a later model. A slot value that came from a
model step (`task` or `agentic`, `weave.MODEL_KINDS`) is tainted
(controller ruling, 2026-09-30): it never fills `file_write.content`,
whatever its type, because a later `shell` or `agentic` step may run that
file, the injection path ADR-0022 names. It may fill a path or a URL, under
the directory and host rules below and in WV-R-4. The refusal is "step B:
file_write.content cannot take T.S: T is a task step, and model text never
fills a file's content (it may fill a path or a URL)". The taint is the
source step's kind, not the value, and it does not pass through a later
step: a `file_read` whose path a model chose reads bytes no model wrote.
`shell` is left out for the reason
`pathway_params` gives (verify does not check a command's operands). The
source step (the text before the last dot) must be an ancestor through
`depends_on`, and must declare the slot; the step's own path or URL must
have a directory or a host. Every check runs before anything is written:
"Tried to weave the plan P.", the reason, "Fix the plan and run again.",
exit 2.

Status: in force.
Retires when: the owner widens what a slot may fill, or lets model text into a file.

**WV-R-3 (ruling): reading a slot.** After a step with outputs succeeds, its
stdout, stripped (a `task` step's also loses one surrounding code fence, as
`delegate._strip_fence`), must be one JSON object that holds every declared
slot; other keys are ignored and flow nowhere. A `number` is an int of at
most 2**53 in size or a finite float, never a bool; a `string` is at most
4,096 code points with no NUL; a `path` is a `string` that starts with `/`,
has no `..` part and no control character; a list holds at most 64 items.
A step whose output does not read fails: its outcome is `failed` with
"slot outputs: " and the reason, its receipt says so, and the run stops.

Status: in force.
Retires when: does not retire.

**WV-R-4 (ruling): filling a step, and verifying it again.** Before a step's
per-step verify, each input is filled from the slot values already read. A
path must keep its directory and a URL its scheme and host
(`pathway_params._capability_head`, the K1 bind rule); a value that would
change them stops the run before the step, `rejected_halted` with "rejected:
slot A.S would change the directory of file_write.path: ...", status
`halted_by_simplex`, with no fallback. Content takes a string as it is and
any other value as `json.dumps`. The filled step then goes through the
supervisor's own per-step verify, and a rejection goes to the envelope's
fallback like any other (halt, or `tier2_recompute`). The per-step verify
skips the plan-level checks (predicate invariants and the strict-mode checks
that ride on them), and the first whole-plan verify saw only the
placeholder, so a filled step that passes its per-step verify is verified
again as part of the whole plan, with every value filled so far (the
supervisor hook's `checked`). A failure stops the run before the step:
`rejected_halted`, "rejected: the filled plan: " and the first violation,
with no fallback. Go and Rust fail a Z3 check that does not finish there
(K2-2); the oracle's lenient verify keeps it as a warning.

Status: in force.
Retires when: does not retire.

**WV-R-5 (ruling): the router for task steps (RT-3 as built).** A `task`
step with no `preferred_model` gets the model `daisugi route` gives its
prompt with no pathway store: `claude-haiku-4-5` when
`estimate_difficulty(prompt)` is under 0.5, else `claude-opus-4-8`. The call
is in process, not a `daisugi route --json` subprocess. Tier-0 reuse is left
out: it replaces a whole task, not a step. A step's own `preferred_model`
wins. The routed model is journaled with the plan and shown in the JSON's
`weave.models`.

Status: in force.
Retires when: router part 1 or 2 makes one router interface for turns, steps and delegate calls.

**WV-R-6 (ruling): the task executor.** A `task` step runs through
`DelegatingExecutor(json_mode=False)` with `orchestrator._task_step_prompt`;
a step with outputs instead gets its prompt and "Answer with one JSON object
and nothing else. Its keys and their types: n (number), .... A path is
absolute." There is no budget.

Status: in force.
Retires when: does not retire.

**WV-R-7 (ruling, provisional): resume (WV-2 as built).** Before each
step's executor runs, the runner appends `{"run": RUN, "step": STEP}` to
`<data dir>/weave/sha256-<hex>.jsonl` and syncs it, with or without
`--resume`. With `--resume`, the runs named there give their receipts from
the journal. In plan order: a `file_read` or `network` step runs again; a
step with a succeeded receipt (verify result true, evidence status
`succeeded`) is skipped, as the outcome `skipped` with no receipt in the new
run, and its slots are read again from that receipt's stdout; a step that
any run marked started with no receipt from that same run may have run
there, so the run stops before it (`aborted`, exit 130, "Check it, then run
again with --rerun STEP") unless `--rerun` names it; a `task` step in that
state runs again (it has no effect). The stop is not a terminal prompt:
`--rerun` is the one way on. An old mark with no receipt keeps asking until
the step succeeds once. A receipt whose status is `succeeded` but whose
verify result is false counts as a receipt, not a success: the step runs
again with no stop. A skipped step is not expected to have a receipt in the
run's integrity check. A state line that does not read is skipped. A start
mark that cannot be written stops the run before the step: `aborted`, "the
start mark was not written: " and the reason; the step does not run.

Status: in force.
Retires when: the owner confirms or overturns WV-2.

**WV-R-8 (ruling): parallel levels.** `--max-parallel N` is the
supervisor's `max_parallel` in the oracle. A step with inputs, or one that
resume would skip or stop at, is kept out of the prefetch; a prefetched step
is marked started before its executor runs. Under 1 is refused: "Error:
--max-parallel must be 1 or more.", exit 2. The ports keep K2-4 and refuse
above 1.

Status: in force.
Retires when: K2-4 retires.

**WV-R-9 (ruling): agentic steps (WV-4 as built).** The oracle wires
`AgenticExecutor(envelope=...)`: `claude -p` under the call-time gate, with
its tool wall from the caller's envelope. The sprig half of WV-4 waits for
sprig. Skill and MCP steps get no executor, as in `run`.

Status: in force.
Retires when: the owner confirms or overturns WV-4.

**WV-R-10 (port): what the ports refuse.** Go and Rust refuse, before
anything is written, `--max-parallel` above 1 (K2-4), a plan or state file
that is not UTF-8, and plan JSON their readers do not model (nesting past
900, an int past the digit limit). Of 120 weave cases each agrees on 119
and refuses the 1 ruled. (Until 2026-10-02 they also refused a plan with
an agentic step; PG-3 retired that.)
The text of a start mark's write error is each language's own; no case
reaches it.

Status: in force for the UTF-8 and JSON-reader refusals; its agentic half
retired 2026-10-02 by PG-3, its `--max-parallel` half 2026-10-03 by MP-1.
Retires when: the readers take the rest.

**WV-R-11 (harness): earlier runs in a case.** (Since 2026-10-09 a weave
case may also put a fake or a real sprig on PATH, SX-R-7.) A weave case may hold `pre`
commands, run with the same binary before its command, so a resume case
finds the receipts that binary wrote; and a `weave_marks` entry, a state
file with start marks for the laid-out plan file, named by that file's
hash. A plan hash is minted like a run id (`{PLANHASH1}`). The recompute
cases, and the list-of-paths case, put their paths outside the scratch
HOME, so the model request and the bytes written are the same on every run;
`weave_compare.py --oracle` reruns the oracle on all 108 (2026-09-30,
after the attempts cases) and finds none stale. A receipt's `evidence_hash` once read `{HASH BAD}` on a
Go run and agreed on three reruns. The cause was the harness, not the
binary: `run_case` names each port in the whole result text
(`ports.sub_number`) before the receipt check, and it named the number
wherever no digit touched it. A port's digits inside the receipt's hex
hash (between two letters), or after the decimal point of its
`duration_ms`, turned the hash or the evidence JSON into text with
`{PORT}` in it, and the check read `{HASH BAD}`. A port now counts only
when no letter, digit, `_` or decimal part touches it; a port right after
a JSON escape (`\n41235`) is no longer named either, and no fixture has
one. The weave compare passed 10 runs in a row for Go and for Rust after
the fix (2026-09-30).

Status: in force.
Retires when: does not retire.

## 2026-09-30: `daisugi rank` and the review queue (RK-10)

The design is `docs/plans/2026-09-24-omarchy/design-ranking.md`. These rulings
fix what the oracle (`src/opendaisugi/rank.py`) does, so Go and Rust can
follow them as a spec.

**RK-R-1 (ruling): the commands.** A hidden group `daisugi rank`: `fit FILE`,
`choose FILE`, `queue`, and `record pick CHOICE ATTEMPT` and `record drop
CHOICE`. Each takes `--data-dir` (default `~/.opendaisugi`) and `--json`;
`queue` also takes `--sort`. The fit is `rank fit FILE`, not a bare `rank
FILE`: with a bare form every mistyped verb would read as a file name. The
owner's two verbs live under `record`, so the gate's existing hard deny of
any shell line that names `daisugi`, `rank` and `record` in that order
(`rank_rule.py`, SEC queue) covers them unchanged in all three languages;
`rank queue`, `rank fit` and `rank choose` are not denied. Not built: `rank
judge` (the model-calling step; the swap rule lives in the fit), `rank
show`, the SQLite index of RK-6, the coppice cards view, and the pivot (a
pick prints what switching would do; it does not change the work).

Status: in force.
Retires when: the judge step, the cards view or the pivot is built.

**RK-R-2 (ruling): the attempts file.** The file's bytes decoded as UTF-8
and `json.loads`; a failure prints "Tried to read the attempts in P.", "It
did not parse: " and the first line of the error, exit 2. Then the checks,
in this order, each failure "Tried to rank the attempts in P.", the reason,
"Fix the file and run again.", exit 2: a JSON object; `ranking_id` 1 to 96
of `A-Z a-z 0-9 . _ : # -`; `task` and `project` strings when present;
`attempts` a list of 1 to 8; per attempt, in order: an object, `id` 1 to 32
of `A-Z a-z 0-9 . _ # -`, `content_hash` a string that is not empty,
`author` a string, `edge_proof` `"ok"`, `"failed"` or absent, the flags
`started_by_parent` and `ran_plan` true or false, `verify` (`ok` true or
false, `violations` a count), `gate` (`denies`, `override_allows`, counts),
`tests` (each `name`, `required` default false, `result` `pass`, `fail` or
`not_run`), `features` (each `name`, `required`, `green`), `cost` (`tokens`,
`wall_ms`, `diff_lines`, each a count, and `estimated`, a list naming them),
`previews.summary` a string; then no id twice; then `comparisons` (each
`shown` `ab` or `ba`, `outcome` `a`, `b`, `tie` or `skip`, the strings `a`,
`b`, `a_hash`, `b_hash`, `judge`, `pair_id`, and `a` not equal to `b`);
then `owner_constraints` (pairs of two known ids, not the same); then
`policy`: `threshold` a finite number from 0.5 to 1 (default 0.9), `prior`
from 0.01 to 100 (default 1.0), `bootstrap` an int from 1 to 10000 (default
1000), `judges` a list of strings. A count is an int from 0 to 2**53, never
a bool; a number is never a bool. A JSON null reads as absent for a
top-level key, an attempt's `author`, `edge_proof`, flags, `verify`, `gate`,
`where`, `previews` and `previews.summary`, and every `policy` key; a null
`tests`, `features`, `cost`, `cost.estimated`, a null in a test, a feature,
`verify` or `gate`, is refused as the wrong type. Unknown keys are ignored.
The texts are the oracle's.

Status: in force.
Retires when: does not retire.

**RK-R-3 (ruling): what eliminates.** Per attempt, the reasons in this
order: "edge proof failed: never ran" (`edge_proof` failed), else "edge
proof missing" (started by a parent, no proof); "verify failed: N
violations", else "verify missing" (`ran_plan` and no `verify`); each
required test, in file order, "required test failed: NAME" or "required
test not run: NAME"; each required feature not green, "required feature not
green: NAME". Gate denies do not eliminate. An attempt with operator allows
outside its envelope is kept with the warning "ID was let out of its
envelope N times by an operator allow; it is kept" (RK-8). If no attempt
survives, `status` is `none_survived`, `leader` and `confidence` are null,
and nothing is chosen.

Status: in force.
Retires when: does not retire.

**RK-R-4 (ruling): a vote survives the order swap.** The comparisons are
the file's, or, when the file has no `comparisons` key, the rows of
`<data dir>/journal/rankings/comparisons.jsonl` with the same `ranking_id`
(a row that does not read is counted in the warning "N rows of the
comparison journal did not read; skipped"). In order: a row naming an id
that is not an attempt is ignored ("a comparison by J names no attempt X;
ignored"); a row whose `a_hash` or `b_hash` is not that attempt's
`content_hash` is ignored ("... binds to a hash that changed; ignored").
The rest are grouped by (`judge`, `pair_id`). A group makes a vote only when
it holds exactly two rows over one pair, one shown `ab` and one `ba`
(otherwise "judge J call P does not hold the pair once in each order; no
vote"). A `skip` in either row is no vote. The same winner in both rows is a
win; any other pair of verdicts is a tie. A judge votes once on a pair: of
several, the vote with the smallest `pair_id` is kept ("judge J voted more
than once on X and Y; one vote kept"). Only votes between survivors of the
same quality tier reach the fit.

Status: in force.
Retires when: does not retire.

**RK-R-5 (ruling): the fit.** Quality tiers: survivors grouped by (optional
tests passed, optional features green), larger first; members listed in id
order. Inside a tier, Bradley-Terry by the MM iteration, members in id
order: wins_i = prior/2 + the votes' wins (a tie half each); games_ij =
votes between i and j; every s starts at 1; each round computes, for each i,
denom = prior/(s_i + 1) plus, for each j in id order with games_ij not 0,
games_ij/(s_i + s_j), then new_i = wins_i/denom, all from the last round's
s; it stops after the round whose largest |new - s| is under 1e-10, or after
500 rounds. Only `+ - * /`; each sum is a running total in the order given.
A strength is s/(s + 1), the chance to beat the reference. Every float is
rounded to 9 decimals (Python's `round`) before a test or a print.

Status: in force.
Retires when: does not retire.

**RK-R-6 (ruling): the seeded bootstrap.** The seed is the first 16 hex
digits of the SHA-256 of `json.dumps(doc, sort_keys=True,
separators=(",", ":"))` (ASCII), where doc holds `ranking_id`, `policy`
(bootstrap, judges, prior and threshold as floats) and `owner_constraints`
(the pairs in force before any check, as lists), the comparisons used (each
with its eight keys), and per attempt `id`, `content_hash`, `cost`,
`edge_proof`, `features`, `ran_plan`, `started_by_parent`, `tests` and
`verify` as read (a test as `name`, `required`, `result`; `cost.estimated`
without repeats, in the order tokens, wall_ms, diff_lines). The
generator is splitmix64 from the seed as a big-endian 64-bit number. For
each of B rounds, for each tier in order with 2 or more members and n votes,
n draws pick votes by `next() % n`, and the tier is refit; a tier with one
member, or with no votes, refits on none. A member's interval is the
sorted round strengths at indexes (5(B-1))//100 and (95(B-1))//100. X beats Y
in a round when round9(s_X) > round9(s_Y). The leader's `confidence` is the
share of rounds in which, for every other member of its tier, the leader
beats it or an owner constraint (through the chain of them) puts it first;
1.0 when its tier has no other member.

Status: in force.
Retires when: does not retire.

**RK-R-7 (ruling): the order, the label and the next pair.** Inside a tier
the members go by strength, larger first, then id. Walking that list, a
member starts a new class when the share of rounds in which the one before
it beats it is at least the threshold; otherwise it joins the class before.
Inside a class the members go by cost: tokens, wall_ms, diff_lines (an
unknown cost after every known one), then id. The tiers follow each other.
The owner's pairs among survivors (repeats dropped) apply, except that
every pair whose two ids lie on a cycle of them is left out ("the owner's
answers form a cycle among X, Y; none of them applied"); the order then
takes, again and again, the first id in the fitted order whose owner
predecessors are all placed. `decided_by` for each adjacent pair: `owner`
(an owner pair, or the fitted order reversed), else `quality` (another
tier), else `preference` (another class), else `cost` (the costs differ),
else `tie` (by id), a value the design does not list. The leader is the
first. `status`: `single` for one survivor (label the leader); `ranked`
when the leader wins an owner pair or `confidence` is at least the
threshold (label the leader, `stop` `confident`); else `provisional`
(label `no_label`). For `provisional`, `next` names, for the leader's
tier-mates by (|round9(P(leader beats X)) - 0.5|, the votes on the pair,
id), the first judge in `policy.judges` with no vote on that pair:
`{"pair": [leader, X], "judge": J, "why": "P(L beats X) is P, the nearest to
even"}` (P as Python prints the rounded float); with none, `stop` is
`judges_exhausted`. `stop` is `nothing_to_ask` for `single` and
`none_survived`. With no optional test or feature on any survivor and more
than one survivor, the warning "no optional test or feature separates the
attempts; only opinion ranks them". Exit codes: 0 `ranked` or `single`, 3
`provisional`, 1 `none_survived`, 2 bad input. The JSON is the oracle's
key order with `json.dumps(indent=2)`, `scores` in the final order, and a
`seed`.

Status: in force.
Retires when: does not retire.

**RK-R-8 (ruling): the choice record.** `<data dir>/journal/rankings/choices.jsonl`,
one `json.dumps` line per row, appended and synced, never edited. `rank
choose` writes, after the decay rows (RK-R-9), one `opened` row when two or
more attempts survived: `choice_id` (`ch_` and the first 12 hex digits of
the SHA-256 of the canonical JSON `[ranking_id, [[id, content_hash], ...]]`
over the survivors sorted), `ranking_id`, `project`, `task`, `event`,
`options` (`survivors` with their hashes in the final order, and
`eliminated`), `chosen` (the leader), `status`, `facts` (order,
decided_by, quality_tiers, confidence, cost, summary, where, run) and
`ts`. The same choice is never opened twice ("Choice C is already
recorded."). A close row is `confirmed` or `overridden` (with `pick`) with
`how` `answer`, `drop` or `permanent_ask`, or `ignored` with `how` `decay`,
`trigger` and `fired_at`. A card is the fold of its rows in file order: the
first valid `opened` row makes it, the first close closes it, and the first
owner answer, before or after a decay, is its label. Rows that do not read
or do not fit are skipped. When the attempts file has no
`owner_constraints` key, the fit reads the owner's answers for its
`ranking_id` from here: `confirmed` puts the chosen one before each other
survivor, `overridden` puts the pick before the chosen one; an answer whose
survivors' hashes no longer match is left out ("the owner's answer on C
binds to attempts that changed; ignored").

Status: in force.
Retires when: does not retire.

**RK-R-9 (ruling): switch cost, decay and the queue.** The switch cost of a
card, from the ledger only: the receipts of `facts.run.run_id` (and of each
run that resumed it, RK-R-16) for the steps in `facts.run.downstream`, read from `<data dir>/journal/index.db` opened
read-only (none when it is missing or does not read). A receipt whose
reversibility is not `none` or `reversible` makes it `follow_up_only` (it
fired at that receipt's time); so does an alternative set whose every
`where.path` is gone; else any `reversible` receipt makes it `costly`; else
`cheap`. Decay (provisional, the design's option b): an open card closes as
`ignored` at the first of `follow_up_only` and 7 days after `ts`. Reading
never writes: `rank queue` leaves a decayed card out and counts it; a writer
(`choose`, `record`) first appends the `ignored` row of each decayed card,
with the trigger (`time` or `follow_up_only`) and the time it fired. The
queue lists open cards with a headline, a why, the switch cost and
`decays_in_days` (whole days, rounded up, never a date), sorted by
`reversibility` (cheap first, then the oldest, then id; the default),
`impact` (the largest diff-line gap first, then the lowest confidence, then
id), `date` (newest first, then id) or `project` (then newest, then id).
Another `--sort` prints "Error: --sort must be reversibility, impact, date
or project." and exits 2.

Status: in force.
Retires when: the owner sets the decay time (open question 11).

**RK-R-10 (ruling): the owner's answer.** `record drop C` records
`confirmed` (`drop`); `record pick C X` records `confirmed` (`answer`) when X
is the chosen one, else `overridden` with `pick` X and prints what
switching would do (the switch cost to X). A card with an owner answer
refuses a second one ("It is answered already: E (H).", exit 1); a card
closed by decay takes an answer, since a late label is still the owner's.
No such choice: exit 1. X not a survivor: exit 2.

Status: in force.
Retires when: the pivot is built.

**RK-R-11 (harness): the rank cases.** `clients/rank_cases.py` writes
`clients/fixtures/rank`; `clients/rank_compare.py` runs them as the garden
compare does. Times are laid out relative to the run; a `receipts` tree
entry is a journal index with receipt rows at fixed run ids. Help texts and
click's own usage errors are left to the cli family: the oracle prints them
through rich.

Status: in force.
Retires when: does not retire.

**RK-R-12 (ruling): weave's attempts (the N-attempt form).** weave had no
N-attempt form, so this is the smallest one the design needs. A `task`
step may carry `attempts: N`, an int from 2 to 8 (a bool is not one). The
checks run with the slot checks, after them, before anything is written:
"step S: only a task step runs attempts", "step S: attempts must be a whole
number from 2 to 8", "step S: a step with attempts needs an id of 1 to 29 of
A-Z a-z 0-9 . _ -". The task executor asks the model N times, one after
another, each prompt weave's own and then "This is attempt K of N." It ranks
the answers with `rank`: ranking id `weave:` + the first 16 hex digits of
the plan hash + `:` + the step id; attempt ids `S#K`; each content hash
`sha256:` and the SHA-256 of the answer; author the model; the required test
"the call answered" (the call's rc is 0) and, for a step with outputs, "the
slot outputs read" (WV-R-3's reading, fence stripped); `where` `{"kind":
"output", "text": answer}`; no judges and no comparisons; the owner's
answers from the choice record. When no attempt survives, the step fails
(rc 1, "attempts: none survived: " and each id with its reasons): the run
never goes on with nothing. Otherwise the step's result is the leader's;
with two or more survivors the runner first appends the decay rows and then
opens a card (RK-R-8), with `facts.run` holding the run id, the step and
every step below it in id order. A card that cannot be written fails the
step ("attempts: the card was not written: " and the error): no card, no
review. With no votes the leader is the first survivor by id, `provisional`.
The JSON's `weave.choices` holds, per step, the choice id, the chosen
attempt, the status, whether this run recorded the card, and the ranking;
the text prints "attempts: chose X (status); card C" (or "; no card").

Status: in force.
Retires when: the judge step is built, or weave takes alternatives that differ by more than a re-ask.

**RK-R-13 (ruling): the never-block rule in weave.** A choice never stops the
run, except before a step that cannot be undone. weave's tier of a step
fails closed: `file_read`, `network` and `task` are undoable; a
`file_write` is undoable only when its path, made absolute against the
working directory and normalized as `os.path.normpath` does, is the
working directory or under it; every other step (shell, agentic, skill,
mcp, a write anywhere else) is permanent. Before a permanent step that
depends, directly or not, on a step whose choice is open in this run (the
first such step in id order), the approval goes to a strategy in front of
the default one, so neither `--yes` nor the allowlist passes it: on a
terminal it prints "Step T kept X of its attempts (card C)." and asks the
default terminal question, and a yes records the card `confirmed`
(`permanent_ask`); with no terminal it denies, "step S cannot be undone and
the choice C on step T is open; answer it in a terminal, or review it with
daisugi rank queue and run again", and the run is aborted (exit 130). This
is weave's ask, not the gate's: the gate's permanent-tier ask (coppice) is
not changed and does not show cards yet. Prefetch is off for a plan with
attempts.

Status: in force.
Retires when: the coppice ask carries the card.

**RK-R-14 (ruling): resume keeps the question.** When resume skips a step
with attempts, the runner looks up the newest card for its ranking id; if
that card has no close, no answer and no decay due, the steps below still
ask as in RK-R-13. Otherwise they run.

Status: in force.
Retires when: does not retire.

**RK-R-15 (harness): weave's ranking and choice ids.** A weave ranking id
holds the plan hash, and a plan that names a path under the scratch HOME
hashes differently on every run, so the weave cases mint `weave:<16 hex>:`
and `ch_<12 hex>` like a run id. The rank cases do not: their choice ids are
the same on every run.

Status: in force.
Retires when: does not retire.

## 2026-09-30: rank and executor follow-ups

**RK-R-16 (ruling): a resumed run counts in the switch cost.** When `weave
--resume` skips a step with attempts and picks up its open card (RK-R-14),
the run appends one row to `choices.jsonl` when its first step starts:
`choice_id`, `ranking_id`, `event` `resumed`, `run_id` (this run's id) and
`ts`. A row that cannot be written stops the step ("the resume of the open
choice was not recorded: " and the error). The card fold (RK-R-8) keeps,
per card, the `run_id` of each `resumed` row that reads as a string, once
each, in file order; a `resumed` row does not close the card and is not an
answer. The switch cost (RK-R-9) reads the receipts of `facts.run.run_id`
and of each resumed run, keeps those of the steps in
`facts.run.downstream`, and orders them by time and then step id; the rest
of RK-R-9 is unchanged. So a later step a resumed run ran on the choice
makes the card `costly`, and one it cannot undo makes it `follow_up_only`
and decays it. The weave case "attempts resumed run counts in the switch
cost" shows it.

Status: in force.
Retires when: does not retire.

**EX-R-1 (ruling): the shell step reads its output to EOF.** The shell
executor reads the merged stdout and stderr until EOF or until the output
cap (`max_output_size_mb`, unchanged). The reader keeps what it has read
as it goes. A background child the shell left can hold the pipe open after
the shell exits, so the wait for EOF is bounded by the step's own time
(`max_execution_time_s` from the step's start). Past it the executor kills
the process group and keeps the output read so far; `timed_out` stays
false when the shell itself exited in time. Before this, Python closed the
pipe while its reader was still reading (the close waited for EOF with no
bound, and the reader's result could be read before it was set), and Go
and Rust gave up two seconds after the exit and kept nothing. Unit tests
in all three languages: a late background writer is read to EOF, and a
writer that outlives the step keeps the output read so far.

Status: in force.
Retires when: does not retire.

**RF-3 (looked for, not reproduced, Rust): k3 "mcp run live failed step".**
The case disagreed once in about three Rust runs during the rank job. It
agreed in 10 runs of 10 in one capped run (2026-09-30, after EX-R-1), and
in the full k3 compare on both ports. Three causes were looked at. (1) The
shell executor gave up reading output 2 s after the exit and kept nothing
(fixed by EX-R-1); the case's step is `false`, which prints nothing, so
this could change only a step with output. (2) The supervisor's per-step
verify has a 500 ms Z3 budget (`Supervisor(z3_timeout_ms=500)`, the MCP
`run_plan` path); a cold or starved Z3 can answer unknown, which halts the
run with "rejected: Z3 returned unknown" instead of failing the step. This
is the likeliest cause, since it depends on load, and it can hit the oracle
too. (3) Time normalization rounds to the minute, so it needs 30 s of skew;
not likely. Nothing was changed for (2): a longer budget for one case would
hide a real timeout elsewhere.

Status: in force.
Retires when: the case disagrees again with the full output kept (the compare prints each DISAGREE with its problems).

## 2026-09-30: the delegation tree, steps 1 to 3 (edge_ok, the deadline, the tree ledger)

The design is `docs/plans/2026-09-24-omarchy/design-delegation-tree.md`.
The owner adopted every recommendation in its open questions (AT-1 to
AT-7 below). These rulings fix what the oracle (`src/opendaisugi/tree.py`,
`tree_rule.py`) does, so Go (`internal/tree`) and Rust (`src/tree.rs`)
follow them as a spec.

**AT-1 (owner): build before sprig.** Steps 1 to 3 of the recommendation do
not need sprig and are built. Step 4 (coppice: an envelope on a task, the
proof on `task.create` and `pane.create`) is not built; the adversarial
test of the session binding in coppice comes first. Step 5 (sprig's
`spawn` tool) waits for sprig.

Status: in force.
Retires when: the owner changes it.

**AT-2 (owner): where the budgets live.** Tokens and turns are estimates,
so they live in a tree ledger the starter keeps (option B). The deadline is
exact, so it is an envelope field (option A), proved per edge.

Status: in force.
Retires when: the owner changes it.

**AT-3 (owner): asks go to the nearest foreman, then the operator.** No
code here changes coppice's holds.

Status: in force.
Retires when: the owner changes it.

**AT-4 (owner): strict at every edge,** whatever the stakes.

Status: in force.
Retires when: the owner changes it.

**AT-5 (owner): keep the refusal under physical stakes.** A parent with
`stakes: physical` starts no agent through the tree (TR-R-6).

Status: in force.
Retires when: the owner changes it.

**AT-6 (owner): three narrower proposals, then the human.** TR-R-7.

Status: in force.
Retires when: the owner changes it.

**AT-7 (owner): sprig fleet's operator override** folds into coppice's rules
when sprig resumes. Nothing built.

Status: in force.
Retires when: the owner changes it.

**TR-R-1 (ruling): the deadline field.** `Envelope.deadline` is a number,
seconds since the Unix epoch, read as pydantic reads `float | None` (so `1`,
`true` and `"1900000000"` read as floats, and NaN or an infinity is
refused by the finite-number check). It is left out of the JSON schema a
model sees (`SkipJsonSchema`) and out of every dump when absent
(`exclude_if`), so prompts, traces and fixtures without one are unchanged;
the ports' models leave it out the same way (`OmitNone`, `omit_none`). A
child with no deadline under a parent with one takes the parent's: the
proof fills it in, and the registered child holds it (inherit, not refuse,
since the child is still bounded). A child's later deadline is refused.
The gate does not yet deny a call after the deadline: the deadline is
proved per edge, not enforced at call time. An `AgenticStep` may carry a
`child_envelope`, hidden and left out the same way.

Status: retired 2026-10-01 for its last part: the gate enforces the
deadline at call time (SW-4). The rest is in force.
Retires when: the gate enforces the deadline at call time.

**TR-R-2 (ruling): `edge_ok` and its parts.** `tree.edge_ok(parent,
child)` holds when no part below fails. Every failing part is reported, in
this order, one reason each (values printed by `json.dumps`): a missing
envelope ("envelope: the parent has no envelope", "envelope: the child
declares no envelope"; nothing else is checked); `stakes` (lower than the
parent's); `custom_step_allowlist` (the extra names, sorted);
`max_execution_time_s`, then `max_output_size_mb` (more than the parent's);
`deadline` (after the parent's); `shell_interpreter_policy` (looser, in
the order strict, surface, allow); `invariants`, then `postconditions` (the
parent's enforced ones the child lacks, compared by their whole dumped
value, `enforce` included, so a copy with `enforce: false` is dropped; the
missing types listed); the robot bounds (the first of workspace_bounds,
velocity_limit, torque_limit, each joint in the parent's order, and the
count of dropped obstacles); `shell_allow_decomposition`; `file_read`,
`file_write` and `mcp_allowlist` (a parent pattern of a shape the proof
cannot read, else the first child pattern that does not fit, each pattern
proved on its own by Z3, or "the proof for ... did not finish"); the
network (none in the parent, any host under a list, the extra hosts,
compared in lower case); `shell` (none in the parent), `shell_allowlist`
(each child head that is neither a parent head nor a parent head and a
space; a head with a metacharacter admits nothing and is skipped);
`shell_interpreter_policy` again when the parent is strict and the child
allows interpreters (sorted); and the child's enforced invariants with no
expr whose type is not one of the recognized opaque types. No reason names
a solver model, since Python, Go and Rust may find different witnesses; a
Python caller still gets Z3's counterexample on `EdgeResult`, for a parent
model to read.

Status: in force.
Retires when: does not retire.

**TR-R-3 (ruling): when Z3 decides the edge.** When every part above holds
and either envelope has an enforced invariant with an expr, the edge runs
`envelope_subsumes(parent, child, strict=True)`: a timeout is "proof: the
proof did not finish in N ms", a failure is "proof: the child admits a step
the parent does not". With no such invariant, the Z3 query over one shell
step reduces to the head rule of TR-R-2, which decides it exactly, so Z3
does not run: Z3's string solver took 1.4 to 2.3 s on a two-word head under
a one-word parent, close to the 2 s budget, which would make the edge
depend on load. The lenient paths of `check_skill_delegations` (an opaque
skill or a timeout as a warning) do not apply: an edge never calls it.

Status: in force.
Retires when: does not retire.

**TR-R-4 (ruling): the commands.** A hidden group `daisugi tree`: `check
PARENT CHILD [--json] [--z3-timeout-ms N]` (writes nothing; exit 0 holds, 1
refused, 2 bad input), `root ENVELOPE --session S [--tokens N] [--turns N]`
(the operator), `spawn ENVELOPE --parent P --session C [--tokens N]
[--turns N] [--z3-timeout-ms N]` (the starter; exit 0 started, 1 refused, 3
sent to the operator as an ask, 2 bad input), `end SESSION [--tokens-used
N] [--turns-used N]`, `answer ASK allow|deny` (the operator) and `status
[--json]`. Each takes `--data-dir` (default `~/.opendaisugi`); the writers
take `--root`, the gate root, default the data directory's `gate`. An
envelope file is JSON only: a missing file, text that is not UTF-8, JSON
that does not parse, a document that is not an object, and an envelope that
does not validate each exit 2 with "Tried to read the envelope in P.", the
reason and the fix. A session id is 1 to 64 of `A-Z a-z 0-9 . _ -`, no dot
at either end, never `default` or `no-session`, so it is its own envelope
file name; a count is 0 to 2**53. A refused spawn prints one line on stderr:
"not started: the edge from P to C is refused: " and every reason joined by
"; ". Help texts and click's usage errors are left to the CLI cases.

Status: in force.
Retires when: does not retire.

**TR-R-5 (ruling): the tree ledger.** `<data dir>/tree/ledger.jsonl`, one
`json.dumps` line per row, appended and synced, never edited; each writer
folds, checks and appends under an exclusive `flock` on
`tree/ledger.lock`, so two starters cannot reserve the same budget. Rows:
`root` (session, envelope_id, tokens, turns, deadline), `spawn` (parent,
session, envelope_id, tokens, turns, deadline, proved), `refused` (parent,
session, reasons), `ask` (ask_id, parent, session, tokens, turns, the child
envelope as proposed, reasons), `answer` (ask_id, allow or deny) and `end`
(session, tokens_used, turns_used); each has `ts`. The fold reads rows in
file order (lines split as `rank._read_rows` splits them); a row that does
not read or fit is skipped and counted: a root or spawn of a session that
is a node already, a spawn under a parent that is not running, a refused
row or an ask from a parent that is not a node, a second ask with one id,
an answer to no ask or a second answer, an end of a node that is not
running. A node's `left` on an axis is its budget less, for each child, the
child's reservation while it runs, or the smaller of its reservation and
its use once it ended (a child with no use recorded keeps its whole
reservation); a node with no budget has no `left`. A spawn under a parent
with a budget must ask an amount, at most `left`. The root's and the
spawn's registration go into the gate root after the row is appended, so a
failure in between leaves a node with no envelope, which the gate denies.
`end` refuses a node with running children and removes the ended node's
envelope file, so the gate denies its later calls. `status` lists the nodes
depth first in the order they were made, then the open asks.

Status: in force.
Retires when: does not retire.

**TR-R-6 (ruling): what a spawn refuses before the proof.** In this order:
a session id or count that does not read (exit 2); a parent that is not a
running node; a child session that is a node or has an envelope file
already; an open ask for the same child session (exit 1 each, no row).
Then the reasons, each recorded in the refused or ask row: a parent with
`stakes: physical` ("stakes: the parent's stakes are physical, so it cannot
start an agent", AT-5, with no edge check), else the edge's reasons (a
parent whose envelope file is missing or does not validate has no
envelope), then the budget reasons. `tree check` keeps no such refusal: it
only proves.

Status: in force.
Retires when: does not retire.

**TR-R-7 (ruling): three refused proposals, then the operator.** A parent's
count of refused proposals rises with each `refused` row and goes back to
0 on a spawn under it or on an answer to one of its asks. The proof always
runs first: a proposal that holds starts the child whatever the count.
When the count is 3 or more, a refused proposal is written as an `ask`
row, not a `refused` row, and the spawn exits 3 with the ask id and the
command the operator answers it with. The ask id is `ask_` and the first 12
hex digits of the SHA-256 of `json.dumps([parent, session, envelope, n],
sort_keys=True, separators=(",", ":"))`, where `envelope` is the child's
dump and `n` the number of asks before it. `answer deny` closes the ask.
`answer allow` registers the child as proposed, with the parent's envelope
id as its `parent_envelope` when the parent has one, and appends the
answer and a `spawn` row with `proved: false`. In this order the parent
must still run, the session must still be free, the budget must still fit,
the parent's stakes must not be physical (AT-5 holds under an allow too:
"stakes: the parent's stakes are physical, so it cannot start an agent.",
"Deny the ask."), and the child must fit inside the root of its branch (`edge_ok` from the
root's registered envelope; "The child does not fit inside the root R: "
and the reasons, "Deny the ask, or register the child as a root of its
own."), else it refuses and writes nothing. So an allow never widens the
tree past its root, and every node stays inside the root the operator set;
`status` marks such a node "operator allow, not proved", meaning not proved
against its parent. Under the root itself an allow can start only a child
whose ask came from the budget, since the root's proof is the parent's.

Status: in force.
Retires when: coppice carries the ask.

**TR-R-8 (ruling): `gate register --parent P`.** It proves the edge from
P's registered envelope (P may be `default`) to the file's envelope before
it registers, and adds no ledger row: budgets are the tree's. It needs
`--session`, a session id the tree takes, and a child session with no
envelope registered yet. A refusal is one line on stderr, "not registered:
" and the reason, exit 1, and nothing is written. The registered child
holds the parent's envelope id as `parent_envelope` and the deadline as
proved.

Status: in force.
Retires when: does not retire.

**TR-R-9 (ruling): who writes the tree.** The gate denies, before any
envelope check, in every mode, with no operator ask, a shell line that
names a daisugi word, then `gate`, then `register` or `init`; a daisugi
word, then `start`; or a daisugi word, then `tree`, then `root`, `spawn`,
`end` or `answer`, with `rank_rule`'s words (`tree_rule.py`). Each of these
registers an envelope: an agent that could run one could give itself any
authority, and one that could spawn could name a wider ancestor as its
child's parent. `gate init` registers the starter envelope; `start`
registers one for a new session and runs a harness under it. `tree check`,
`tree status` and `registry init` pass. As with the rank rule, a line that
names the words for another reason is denied too (`daisugi status && npm
start`): run the two apart. Not covered, and not in this job: `daisugi gate
disarm` switches the gate off and no rule denies an agent's run of it
today; nor does anything deny `gate arm`. The starter's
own process is not gated; sprig and coppice will spawn as the starter,
binding the parent from the caller's pinned session, not from an argument.
The session binding is the gate's existing pin: a payload never selects an
envelope, and a pinned session with no envelope of its own is denied,
never given `default` (SEC-3); the tree only adds that a child session is
named, never `default`.

Status: in force. Since 2026-10-01 the line is read per simple command
(SW-2), so `daisugi status && npm start` passes, and `gate disarm` and the
other verbs that change enforcement are denied (SW-1).
Retires when: does not retire.

**TR-R-10 (ruling): the agentic step's child envelope.** `AgenticExecutor`
proves `edge_ok(caller, step.child_envelope)` before anything starts and
fails the step with "the child envelope is refused: " and the reasons. A
step with no child envelope restates the caller's own, which the edge
proves at once. The tool wall comes from the child envelope, which is
registered under a session the executor picks (`agentic-` and the step id,
made safe), and the sub-agent's gate is pinned to it; nothing is
registered as `default` in its gate root. Since 2026-10-02 the ports do
the same (PG-3).

Status: in force, now for the oracle and both ports.
Retires when: does not retire.

**TR-R-11 (found and fixed, Rust): an invariant on the command in a
subsumption proof.** Rust's subsumption seeds the scope's `ctx__command`
as the shell admission's `ctx_command`, but the scope handed the invariant
compiler its own name, so Z3 met an undeclared constant, the check failed
with an error, and any skill delegation or edge with an enforced invariant
on `command` was refused in Rust and held in Python and Go. Fail closed,
but a disagreement. The scope now hands back the seeded name. The tree
cases with invariants show it.

Status: in force.
Retires when: does not retire.

**TR-R-12 (harness): the tree cases.** `clients/tree_cases.py` writes
`clients/fixtures/tree`; `clients/tree_compare.py` runs them as the rank
compare does. A grown tree is laid out as the commands write it: the
ledger with times relative to the run and the registered envelopes (0600
in a 0700 directory). Deadlines in the cases lie in 2030, outside the
normalizer's window. CI compares tree with `--max-refused 0`. 31 gate
cases (`tree rule ...`) cover TR-R-9.

Status: in force.
Retires when: does not retire.

## 2026-10-01: the owner's verbs, GR-8 in the ports, the deadline at call time

**SW-1 (ruling): the verbs that change how the gate enforces are the
operator's.** The gate denies, before any envelope check, in every mode,
with no operator ask, an agent's shell line that runs `daisugi gate
disarm` (turns the gate off), `gate arm` (turns it on again: the operator's
switch either way), `gate serve` (starts the resident gate, the process the
hooks trust for a verdict; an agent's own server could answer from an
environment it chose), `install` (writes the gate hook, sets its mode,
removes it with `--uninstall`; the whole verb, so `--dry-run` and
`--print-skill` are denied too), and `graft install` and `graft remove`
(write and remove the graft rules). Refusal: `only the operator changes how
the gate enforces. Run it yourself.` (`owner_rule.py`). The CLI was audited
command by command (`daisugi help --all`, and the callers of `disarm`,
`arm`, `register_envelope`, the hook writers and `save_config`). Already
denied: `gate register`, `gate init`, `start`, the tree verbs (TR-R-9) and
`rank record` (SEC-5). Considered and not denied: `gate check` (decides one
payload, changes nothing), `gate settings` (prints JSON), `gate status`,
`report`, `replay`, `audit` and `proposals` (read only); `tiers setup
--matcher` and `--wire` (rewrite config.yaml through `save_config`, which
keeps every setting it reads, `dialect_enforce` and `verifier_client`
included; the matcher and the model tier do not change a verdict); `voice
arm` and `voice disarm` (a pane's direct-send grant in the voice bridge,
not the gate); `coppice *` (an operator answer through the floor is the
pane rule's); `onboard`, `tend`, `registry`, `gateway`, `router stop`
(pathways, the proxy, the router: no verdict changes). `router label`
joins the list when it is built (SW-12). Since 2026-10-01 the file writes are closed too (SW-17).
Not covered when this ruling was written: the gate did not stop an
agent whose envelope allows the file writes from doing the same by file:
checked 2026-10-01 with `file_write: ["/**"]` and `touch` allowed, the
Python gate allows `touch <gate root>/DISARMED`, a Write to the marker, to
`<gate root>/grafts/*.json` and to `<gate root>/envelopes/default.json`,
and `rm` of a rule file. Only `config.yaml` beside the gate root is guarded
(the daisugi config rule). An envelope that grants writes under the data
directory grants the operator's switches; a write rule for the gate root is
the follow-on, for the owner to rule on (its cost is that every shell line
naming the gate root, reads included, would be denied).

Status: in force.
Retires when: does not retire.

**SW-2 (ruling): the owner's verbs are read per simple command.** The rank,
tree and gate-change rules share one reading (`owner_rule.runs_verb`). The
line goes through `shell_decompose`; each simple command (substitutions
included) is split with `shlex`. A word whose last path part is `daisugi`,
`daisugi-py` or `opendaisugi`, or starts with `opendaisugi.`, is a head,
wherever it stands among the words, so any path, `env`, `nohup`, `uv run`
and `python -m opendaisugi[.cli]` are seen. The head's arguments are the
later words that do not start with `-`. The first that names a top-level
command (`COMMANDS`, every command of the CLI, hidden ones included) is the
command; for `gate`, `graft`, `rank` and `tree`, the first later argument
that names one of the group's subcommands is the subcommand. Any other word
is skipped: the root takes no option with a value, so skipping can only
find more. A hit is a command, or a command and subcommand, in the rule's
verb list. A test pins the tables to the CLI. Four fallbacks read words in
order as SEC-5 does (a head word, then the verb's words): a line that does
not decompose or whose parse raises, a simple command `shlex` cannot split,
a simple command that holds a form `shlex` splits differently from bash (a
backslash-newline line continuation, `$'...'` or `$"..."`), and a word with
a character outside SEC-5's word set (the payload of `sh -c`, `bash -lc`, a
quoted message). The word reading now joins a backslash-newline before it
drops backslashes and quotes, so `daisugi gate dis\<newline>arm`, which
bash runs as `gate disarm`, is a hit; SEC-5's reading missed it. Without the
third fallback the per-command reading missed `daisugi $'rank' record` and
a line continuation inside the verbs, which SEC-5's reading caught; it was
found in review and closed the same day (`owner rule shlex gap` cases). So `daisugi status && npm start` and
`daisugi status; yarn start` pass; `npm test && daisugi start`, `x=$(daisugi
tree end kid)`, `sh -c 'daisugi gate disarm'` and `daisugi gate disarm; (`
are hits; `echo daisugi gate disarm` and `git commit -m 'daisugi start
now'` are hits too (the rule does not know what echo or git does with its
words). A verb built at run time (a variable, a command substitution
used as a word, `$'\x..'` escapes) is not seen. Changed from the word reading: `daisugi\trank\nrecord` runs
`daisugi rank` and then `record`, so it is no hit; `daisugi<NBSP>rank
record` names no daisugi command (the shell does not split at a no-break
space), so it is no hit.

Status: in force.
Retires when: does not retire.

**SW-3 (port): the frames of the owner rules.** `_decide` calls
`rank_record_hit`, which calls `shell_hit`, `runs_verb`, `_simple_commands`
and `decompose_command`: the ports open one frame for each, so a long
`&&` chain raises RecursionError at the oracle's depth. Measured with the
depth logs: the rules decompose at the same depth as the floor rules. The
13 `owner chain` cases (955 to 980 links) agree; at 979 links the floor
rule's own fallback is the hit.

Status: in force.
Retires when: does not retire.

**SW-4 (ruling): the deadline at call time.** After the hard-deny rules
and before the envelope, `_decide` denies a call when the envelope that
checks it has a `deadline` and the time is after it (equal passes):
`the envelope's deadline D (Unix seconds) has passed; it starts no new
work`, D as `json.dumps` writes the float. It is an ordinary deny: audit
mode allows it as a would-deny, its tier is permanent (no clause), and an
operator ask may allow it; an operator's edit is checked again and denied
past the deadline. Every path reaches it: each `apply_patch` path goes
through `_decide`, and the resident gate runs the same code (two
`gate_server` cases). The clock is `time.time()`, or
`DAISUGI_GATE_NOW` when that is 1 to 12 ASCII digits with at most a point
and 1 to 6 digits and is later than the real time. A pin can only add
denies; it never extends a deadline. Cases use deadlines in 2096 with pins
on each side.

Status: in force.
Retires when: does not retire.

**SW-5 (note): GR-8 was already in the ports.** The brief asked for the
operator-edit fix (Python 6a0ad499) in Go and Rust. It landed there with
the subsumption and session-binding work (Go `recheckEdit`, `ask.go`; Rust
`recheck_edit`, `ask.rs`), with the `operator edit *` gate cases. This job
adds two cases, an edit into `daisugi gate disarm` and an edit after the
deadline, and retires GR-8.

Status: in force.
Retires when: does not retire.

## 2026-10-01: the delegate's code write (RT-1)

**SW-6 (ruling): code_write returns a draft and writes nothing.** The
`delegate` MCP tool takes `mode` `bulk_read` (the default) or `code_write`;
any other mode is refused with `mode 'X' is not built; the modes are
bulk_read and code_write`. For a code write, `question` is the request.
The checks before the worker are the bulk read's (an absolute path, a
question, a readable default envelope, not physical stakes). The target
exists when `os.path.lexists` says so (so a dangling symlink exists); then
it is measured as a bulk read measures it (a regular UTF-8 text file of at
most 512 KiB, no NUL) and refused as one when it does not measure; else the
worker drafts it from the request alone, `lines` is null and nothing is
read. The worker gets `WRITER_SYSTEM` and the request, the file name and
the file text (or "The file does not exist yet. Write it whole."), with
`max_tokens` 8192 and a JSON object reply: `{"form": "file"|"diff",
"text": "..."}`. The reply is read as the bulk read's (one surrounding
fence stripped). Refusals, each a journal row: not the JSON object asked
for; `has no form of file or diff`; `has no draft text`; `the draft is
longer than 524288 characters`. A file draft applies. A diff against no
file does not apply: `there is no file to apply a diff to`.

Changed 2026-10-01 (SW-18): a code write opens its target without
following a symbolic link, so a link there, dangling or not, is refused
with `it is a symbolic link`.

Status: in force.
Retires when: does not retire.

**SW-7 (ruling): the diff applier.** `delegate.apply_diff` reads the diff's
text split at each newline, less one empty last line. Before the first
hunk, empty lines and lines that start with `---`, `+++`, `diff ` or
`index ` are skipped; any other line does not apply (`line N before the
first hunk is not a diff header`, N counting from 1). A line that starts
with `@@` starts a hunk, and the rest of it, the line numbers and counts
included, is not read: workers get them wrong, and the lines locate the
hunk. In a hunk a line that starts with a space is context, `-` removed,
`+` added, and an empty line is an empty context line. A line that starts
with `\` does not apply (`line N is a \ line (no newline at the end),
which the applier does not read; send a whole file instead`); any other
line does not apply (`line N is not a context, removed or added line`). No
hunk: `the diff has no hunk`. The file's lines are its text split at each
newline, so line endings are literal (`\r` stays in the line) and a last
newline gives an empty last line. Each hunk's old lines (context and
removed) must match the file's lines exactly once at or after where the
hunk before it ended: `hunk K has no context or removed lines, so it has
no place in the file`, `hunk K does not match the file`, `hunk K matches M
places in the file`. They are replaced by its new lines in memory. No
fuzz, no whitespace folding.

Status: in force.
Retires when: does not retire.

**SW-8 (ruling): the draft's result and its row.** The result has the bulk
read's keys and four more: `form`, `applies`, `apply_reason` (null when it
applies) and `draft`, the draft in a fence of backticks one longer than the
longest backtick run in it and at least three, with info string `diff` for
a diff and none for a file, and a newline before the closing fence when the
draft does not end with one. `untrusted` is `DRAFT_NOTE`; `answer`,
`exact_text` are null and `quotes` empty. A bulk read's result carries the
four keys as null. The journal row gains `form` and `applied` (null when
refused or for a bulk read; true for a file draft). A code write's row,
refused or not, has `estimated` false, `frontier_tokens_kept` and
`frontier_dollars_kept` null: a draft keeps no frontier tokens off the
context that could be estimated, since the frontier still writes the file.
Its worker tokens and billed cost are recorded as a bulk read's. The port
hands back a draft that holds a lone surrogate, as it does an answer.

Status: in force.
Retires when: does not retire.

**SW-9 (ruling): the gate's check of a code write.** `_decide_delegate_call`
checks a `code_write` call (the `mode` argument the string `code_write`) as
a read of the normalized path only when `os.path.lexists` says the target
exists; for a new file it skips the read. The network send to a remote
worker, the absolute-path rule and the physical-stakes refusal are as for
a bulk read; the send now runs in its own function (`_delegate_send`), and
the ports open a frame for it. Any other mode value is checked as a bulk
read. The write itself is not checked here: the tool writes nothing, and
the frontier's own Write or Edit goes through the gate.

Status: in force.
Retires when: does not retire.

## 2026-10-01: the promotion meter (GR-6, GR-7)

**SW-10 (ruling): the operator's label.** `daisugi router label SESSION
OUTCOME [--note TEXT] [--data-dir D] [--json]` appends one row to
`<data dir>/router/labels.jsonl` (the directory made as needed, the file
0600 when new): `{"at": ISO, "session": SESSION, "outcome": OUTCOME,
"note": TEXT or null}`. OUTCOME is `pass` or `fail`; any other is a usage
error, exit 2 (`Error: OUTCOME must be pass or fail.`). SESSION must be a
session id as the gate writes it, that is equal to `_safe_session_id` of
itself (`Error: SESSION must be a session id as the gate names it: 1 to 128
of A-Z a-z 0-9 . _ -, with no dot at either end.`, exit 2). A note over 500
characters is a usage error (`Error: --note must be at most 500
characters.`). A write that fails prints `Error: the label could not be
written to PATH.` and exits 1. Output: `labeled SESSION: OUTCOME`, or the
row as JSON with `--json`. The meter reads the rows that are JSON objects
with a `session` that passes the same test and an `outcome` of pass or
fail; the last row for a session wins, so a label can be corrected. A
session with no label does not count.

Status: in force.
Retires when: does not retire.

**SW-11 (ruling): no weave or rank verdict is keyed by a gate session.**
The brief asked the meter to read the verdict weave and rank record where a
session ran one. Checked: weave journals a run under its run id (`traces.run_id`,
`run_status`), and its agentic steps run their gate under the session
`agentic-` and the step id, in a temporary gate root (`agentic_executor.py`),
so none of it reaches the gate audit logs the meter reads, and no id is
shared. A rank attempt carries tests and features but no session field
(`rank.py`, `_RESULTS`), and a card's run is a weave run id. A join on
anything but an equal id would be a guess, so the meter reads only the
operator's labels. A verdict source keyed by the gate session (a harness
hook, or weave naming its gate sessions by run) is the follow-on.

Status: in force.
Retires when: weave, rank or a hook records a verdict under the gate's session id.

**SW-12 (ruling): `router label` is the operator's.** The gate denies an
agent's shell line that runs `daisugi router label` (SW-2's reading, the
group `router` with subcommands `label`, `status`, `stop`), with refusal
`only the operator labels a task's outcome. Label it yourself.` (the
label rule, after the gate change rule). As with SW-1, an envelope that allowed writes to
`<data dir>/router/labels.jsonl` let an agent append a label by file; since
2026-10-01 the gate state rule guards that file (SW-17).

Status: in force.
Retires when: does not retire.

**SW-13 (ruling): the arm of a session.** A graft rule may have the state
`trial`, which acts (it joins `audit` and `active`), and an optional
`"trial": {"seed": N}` (an integer from 0 to 2**53; anything else makes the
file a bad rule: `trial must be an object with an integer seed from 0 to
2**53`); the seed is 0 when absent. For a rule in `audit` or `trial`, every
graft record of a matched read carries `arm`: `graft` when the first 8 hex
digits of the SHA-256 of `SEED:ID:VERSION:SESSION` (UTF-8) read as an
integer are even, else `control`. SESSION is the safe session id the audit
file is named by: the pin, else the payload's. The arm is set as the graft
record is made, before the worker and envelope checks. In `_maybe_graft`,
after the envelope check: an audit rule only records; a trial rule only
records in the control arm (`the session is in the trial's control arm: the
read is not denied`); an audit-mode gate only records; else the read is
redirected. The meter reads the recorded arm and never recomputes it.

Status: in force.
Retires when: does not retire.

**SW-14 (ruling): a session's billed cost.** The meter prices a session from
its Claude Code transcript: the last `transcript_path` string among the
session's audit records (by file line) that is an absolute path. It is
opened as the delegate opens a file (read only, non-blocking,
close-on-exec, checked on the open descriptor), and read only when it is a regular file of at most 64 MiB;
else the cost is unknown. The bytes are decoded as UTF-8 with replacement
and split at each newline; each line that reads as a JSON object (other
lines, too-deep ones included, are skipped) with `type` `assistant`, a
`message` object and a `usage` object is a message. Messages with the same
`message.id` string are one message, priced from the last such line (Claude
Code writes one line per content block with the same usage); a line with no
id string is its own message. A token count is an integer from 0 to 2**53
(a bool, a float, a negative or larger number reads as 0): `input_tokens`,
`cache_read_input_tokens`, `cache_creation_input_tokens`, `output_tokens`.
Dollars are the gateway's list prices per million tokens for
`message.model` (input, cache read at 0.1 of input, cache write at 1.25,
output), summed in message order; a model the table does not name is priced
at the fallback (3.0, 15.0) and makes the cost estimated. Quota tokens are
the four counts summed. These are API list prices: on a subscription the
binding limit is the quota, shown beside them. Delegation rows carry no
session, so a remote worker's cost is not in a session's cost: when the
rule allows a remote worker the graft arm's cost is a lower bound and is
marked estimated. A local worker costs nothing, so the default is exact.

Status: in force.
Retires when: a delegation row carries the gate session.

**SW-15 (ruling): the trial table and what promotion would do.** With an
acting rule in `audit` or `trial`, `router status` adds a `trial` object
(JSON) and section (text); else `trial` is null and there is no section.
The trial's sessions are those with at least one graft audit record for the
rule (same id and version) with an arm of `graft` or `control`, in id
order; a session whose records name both arms is left out and counted
(`conflicting`). Per arm: `sessions`, `labeled`, `passed`, `failed`,
`unlabeled`, `cost_unknown` (labeled sessions with no readable transcript),
`billed_dollars` and `quota_tokens` (summed over labeled sessions with a
known cost, in id order), `estimated`, `success_rate` (passed / labeled, or
null), `dollars_per_success` and `quota_per_success` (the sums over passed,
null when nothing passed or a labeled cost is unknown). The verdict, first
that holds: the rule is in audit (`none`); either arm has fewer than 3
labeled sessions (`wait`); the graft arm's success rate is below the
control's (`retire`); a cost per success is null (`wait`); the graft arm's
cost per success is lower (`promote`); else `keep`. Nothing is changed:
the text says what promotion would do, and the operator does it. Money is
printed with 4 decimals, rates with 2.

Status: in force.
Retires when: does not retire.

**SW-16 (fix): the weekly `estimated` flag.** A week was marked estimated
when any delegation in it was ok. A code write's row is not an estimate, so
the week is now marked when a turn, or an ok delegation row whose own
`estimated` is true, is in it.

Status: in force.
Retires when: does not retire.

## 2026-10-01: the gate's own state (SW-17 to SW-20)

**SW-17 (ruling, controller): no agent writes the gate's own state.** The
gate denies, in every mode, with no operator ask that can turn it
(`pane_rule` true), every agent write it can place under a protected path
(`gate_state_rule.py`, after the label rule and before the deadline).
Reads stay allowed. Refusal, one line that names the protected path as
written: `the gate's own state is under DIR; only the operator writes
there.`

The data dirs are `~/.opendaisugi`, and the gate root's parent when the
gate root is named `gate`. A gate root with another name (the temporary
root of an agentic step, `daisugi-agentic-gate-*` in the temp directory)
protects only itself, so the temp directory is not a data dir. The
protected paths, in this order: the gate root in force (the disarm marker,
`envelopes/`, `grafts/`, `asks/`, `answers/`, `audit/`, `operator.json`);
then under each data dir `gate` (the default gate root), `router` (labels
and delegations, which the meter reads), `journal` (the traces and their
receipts, `index.db`, `rankings/choices.jsonl`), `tree` (the ledger),
`gateway` (`turns.jsonl`, which the weekly measure reads, and
`answers.jsonl`, which the gateway serves back), `weave` (the run files a
resume reads) and `envelope_cache.db` (the envelopes a run reuses).
`config.yaml` keeps its own rule, which also denies reads (DL-15). Not
protected: `pathways.db`, `sessions/`, `local_tier1.json`, `alerts.yaml`,
`split_cache.db`.

A write is placed as the config rule places a path: `~`, `$HOME` and the
XDG homes expanded, a relative path joined to the call's cwd, and the
typed and the `realpath` spelling both checked (so `..` and a symlinked
parent are followed). The protected paths are checked as written and as
resolved. A write hits when it lands under a protected path, or when a
protected path lies under it: `rm -r`, `mv` or `chmod -R` of the data dir
or of any directory above it. So `cp x ~` hits too (cp may replace its
destination), and so does a Write whose path is `~`.

The writes, by record:

- file_write (Write, Edit, MultiEdit, each `apply_patch` path): the path.
  `NotebookEdit` is not in the gate's classification map and is denied
  as an unknown tool before this rule.
- shell: the rule runs only when the line names a data dir, or a gate root
  not named `gate`: the path as text with a path boundary after it (end,
  `/`, white space, a quote, a backtick, `;&|()<>:,`), a word or redirect
  the floor reading places under it (each cd followed; after a cd the gate
  cannot follow, a relative word whose parts hold the data dir's last part,
  `.opendaisugi`), or a cwd under it. A sibling with the same prefix
  (`<data>2`) is not named. On such a line, the write paths are those
  `write_paths.step_write_paths` reads (redirects and the operands of its
  15 writers), plus every operand of `ln` and of `cp` with `-l`, `-s`,
  `--link` or `--symbolic-link`, since a link made now is a door to a
  later write (a hard link to an envelope, then a write to the link). The
  line is denied, naming the data dir it names, when `step_write_paths`
  cannot read its writes (a variable, a glob, a substitution, a cd before
  a relative write), when a write is relative and the call has no cwd, or
  when a simple command is neither one of the 15 writers nor a read-only
  command (`READ_ONLY`: `cat`, `ls`, `grep`, `head`, `tail`, `jq`, `wc`,
  `find`, `stat`, `diff`, `echo`, `cd` and 31 more; a `find` with
  `-delete`, `-exec`, `-execdir`, `-ok`, `-okdir`, `-fls` or `-fprint*` is
  not read-only). So `python`, `git`, `tar`, `curl`, `chmod`, `sh -c` and
  any command the gate does not know are denied on a line that names a
  data dir, a read included (`git -C ~/.opendaisugi log`), and follow
  today's rules elsewhere. `chmod` and `git` were not added to
  `write_paths.WRITERS`: that table feeds `forall_writes` and the dialect
  hash.
- mcp: each string argument placed as a path from the cwd (`/` with none),
  and the cwd itself; a hit is one under a protected path (a directory
  above one is not a hit here). An MCP tool's reads and writes look the
  same, so a read of the state through MCP, the delegate's included, is
  denied too.

Any error in the check is a hit, named as the gate root. The rule
decomposes a line no deeper than the floor rule (frame 13 for the names
pass and the write paths, 12 for the command scan), so it never raises
where the floor rule passes; an `sh -c` payload is decomposed deeper and
raises nearer its own limit (gate cases `gate state sh chain N`). The
ports open the oracle's frames; Go and Rust agree on all 157 `gate state`
cases and on the whole gate fixture (2,094). The two `daisugi config mv
dir` cases (`mv {ROOT}/data /tmp/x`) now deny: they move the gate root.

Cost if wrong: an agent cannot edit gate files the owner wanted it to, and
an unknown command on a line that names the data dir is denied; the
operator runs it. Reversible.

Status: in force.
Retires when: does not retire.

**SW-18 (ruling): a code write opens its target without following a
link.** The gate checks a code write's target as a read only when it
exists at hook time (SW-9). A parallel call could plant a symlink there
before the tool reads, and the tool would read and send what the link
names. `delegate.measure(path, follow=False)` adds `O_NOFOLLOW`; `ELOOP`
gives `it is a symbolic link`, so the tool refuses with `the file cannot
be delegated: it is a symbolic link`, journals the refusal, and calls no
worker. A link in a parent directory is still followed (a race there is
not closed). A bulk read still follows a link, since the gate checked its
read on the resolved path. The k3 case `code write link` now refuses; Go
`MeasureFileFollow` and Rust `measure_follow` do the same.

Status: in force.
Retires when: does not retire.

**SW-19 (cases): the 512 KiB draft limit.** `MAX_DRAFT_CHARS` counts
characters. Two k3 cases give a file draft that starts with one two-byte
character: at 524,288 characters (524,289 bytes) it applies; at 524,289
characters it is refused with `the draft is longer than 524288
characters`. A port that counted bytes would refuse the first. Go and
Rust agree. The two cases add about 3 MB to the k3 fixture.

Status: in force.
Retires when: does not retire.

**SW-20 (ruling): `graft install --state trial`.** `--state` takes
`audit`, `trial` or `active` (`Error: --state must be audit, trial or
active.`). With `trial` the rule file gets `"trial": {"seed": N}` after
`worker`, N from `--seed` (default 0). `--seed` without `--state trial` is
a usage error (`Error: --seed needs --state trial.`), as is a seed below 0
or above 2**53 (`Error: --seed must be from 0 to 2**53.`); both exit 2,
after the id and state checks and before the line check. A seed that is
not an integer is Click's usage error. The install line reads `state
trial, seed N`. `graft status` shows `state trial (seed N)` and a
`trial_seed` key on each JSON row (null unless the state is trial). The
gate denies an agent's run of it (SW-1). 10 cli cases (`graft install
trial ...`, `graft install seed without trial`, `graft status trial`), and
the status JSON cases gained the key; Go and Rust agree.

Status: in force.
Retires when: does not retire.

## 2026-10-01: stage H in Go and Rust (the voice bridge)

`daisugi voice serve`, `ptt`, `arm` and `disarm` in both binaries, with the
`voice-probe` instrument. `clients/voice_cases.py` writes 430 cases from the
oracle: 271 probes of the pure parts, 118 cli cases and 41 server cases
(raw HTTP to `voice serve`, a fake `whisper-cli`, a fake coppice socket in
the scratch directory, a fake upstream for cleanup). Go and Rust each agree
on all 430. The cases were written before they were run on a port, but
the ports were written before the cases ran against them: red was shown
after, on the Go binary of 2026-09-30, which has no voice verbs. There,
two `arm` cli cases were refused, and of the 59 cases whose name holds
`serve ` 42 disagreed and 16 were refused. Red was not shown for Rust.

**VO-0 (oracle extension): the whisper.cpp engine.** The oracle ran its
speech model in process (faster-whisper, or parakeet through sherpa-onnx),
so it had no engine command line to case, and the ports cannot import a
Python package. The plan names whisper.cpp. `engines.WhisperCppEngine`
runs `whisper-cli -m MODEL -f CLIP -l LANG -nt -np` with the clip in a
temp file (`daisugi-voice-*.wav`, removed after), `-l auto` when no
language is given, and joins stdout's stripped non-blank lines with a
space. A missing binary or model file is EngineUnavailable (exit 3 from
`voice serve`); a nonzero exit names the code and the last stderr line
(a 500 from the server). `voice_engine: whisper.cpp` names it, with
`voice_model` the ggml file; no config field was added. `prereq` counts
`whisper-cli` on PATH as a speech engine. The facts are pinned in
`pins.py` from the upstream CLI source. `modules` does not list it yet
(VO-8).

Status: in force.
Retires when: does not retire.

**VO-1 (ruling): faster-whisper in the ports.** A port cannot load it.
`voice serve` under `voice_engine: faster-whisper` (the default) exits 3
with `faster-whisper needs the Python build of daisugi. Set voice_engine:
whisper.cpp and voice_model to a ggml model file.` The sentence avoids
"not installed", so coppice does not show its pip hint for it. The prereq
probe reports `faster_whisper_available` false and `ok` only when
`whisper-cli` is on PATH (`port_expect` on 5 cases). No case starts the
oracle's server under faster-whisper: that loads a real model.

Amended 2026-10-01 (VO-13): the sentence is now `faster-whisper needs the
Python build of daisugi. Set voice_engine: moonshine to use Moonshine
instead.`, and it applies only where a config names a voice_model other
than the default `tiny.en` with faster-whisper (`port_expect` on 4 cases).
A config whose voice settings are unset or at their defaults runs
Moonshine (VO-13). The oracle now runs with faster_whisper hidden, so the
prereq cases need no `port_expect`.

Status: in force.
Retires when: does not retire.

**VO-2 (ruling): parakeet in the ports.** Refused the same way, naming
sherpa-onnx (`port_expect` on `pick parakeet`).

Status: retired 2026-10-01 by VO-11.
Retires when: the parakeet engine is removed (done).

**VO-3 (ruling): deliver through coppice only.** The oracle's
`pick_backend` tries coppice, then herdr, then tmux under `backend: auto`,
and a named one literally. The ports reach a pane only through coppice,
with the verbs the oracle uses (`server.status`, then `pane.list` for the
pane's kind, then `pane.send_text` with `enter: true`, or `agent.prompt`
with `wait: false` and `timeout_ms: 60000` for a headless pane), each on
its own connection with ids from 1. Under auto with no coppice answering,
and under a backend named herdr or tmux, the ports answer 503
`no_pane_backend` with their own sentence (`port_expect` on 2 cases). A
send never reaches a backend the oracle would not use; preview and an
unarmed send reach none.

Status: in force.
Retires when: herdr and tmux backends are ported.

**VO-4 (ruling): a clip that is not a WAV, with no ffmpeg.** The oracle
falls back to the av package. The ports have no decoder of their own and
answer 400 `bad_audio`. Stricter, never looser. The cases give garbage
bytes, which both sides refuse.

Status: in force.
Retires when: does not retire.

**VO-5 (ruling): the microphone.** The oracle records through
sounddevice (PortAudio). The ports record through `arecord` (alsa-utils),
started on the first space and stopped with SIGINT on the second, the
whole press read at once; with no `arecord` the session ends with one
sentence. The state machine (`run_ptt`, `record_and_send`) is cased with
scripted keys, streams and client answers; the recorder has a unit test
with a fake `arecord` in each port. Raw terminal mode and a real device
are not tested.

Status: in force.
Retires when: a hand check on a real microphone passes.

**VO-6 (ruling): JSON bodies that are not UTF-8.** `json.loads(bytes)`
reads UTF-16 and UTF-32. Rust reads them too; Go answers such a `/deliver`
body 500. No case sends one.

Status: in force.
Retires when: Go decodes UTF-16 and UTF-32.

**VO-7 (ruling): proxies in the ptt client.** urllib honours
`http_proxy`; the ports' client does not use a proxy. The voice server is
loopback or a tailnet address.

Status: in force.
Retires when: does not retire.

**VO-8 (follow-up): `modules` and whisper.cpp.** The module map lists
faster-whisper and parakeet only, on all three sides. Listing whisper.cpp
changes the cli and k4 fixtures and both ports' modules; it is left for a
change of its own.

Status: retired 2026-10-01. The voice engine stage now lists
faster-whisper, moonshine and whisper.cpp on all three sides, and no
parakeet. An engine is ACTIVE when it is the one the config means (VO-13:
with the voice settings unset or at their defaults, faster-whisper where
it imports, else Moonshine) and it is there (the package, or its binary on
PATH); AVAILABLE when it is there; POSSIBLE otherwise. 81 k4 cases and 3
cli cases changed. Red was shown on the binaries of the Moonshine commit,
built before this change: 81 k4 cases (Go and Rust) and 3 cli cases (Go)
disagreed. That red came after both ports' modulescmd edits were written,
not before: the order the rules ask for was not kept here. Go and Rust
then agreed with the oracle on every k4 and cli case they carry (k4: 188
agree, 2 not ported, 3 refused; cli: 563 agree, 9 not ported, 9 refused,
the same refusals as before), and `--oracle` found no stale fixture.
Retires when: the module map lists whisper.cpp (done).

**VO-10 (ruling): TLS is not cased.** `voice serve --tls-cert --tls-key`
is cased only for a half-given pair (exit 1 after the bind, both ports
agree). A full pair is not: Go loads it with `tls.LoadX509KeyPair`, Rust
with rustls's PEM reader, and neither path runs in a test here. An
unreadable or bad pair exits 1 with each port's own text, where the
oracle prints ssl's.

Status: in force.
Retires when: a case serves over TLS with a certificate the harness makes.

**VO-9 (cases): what the server cases pin.** The check order (411, then
429, then 401, then 404, then 403); a POST to an unknown path without a
token is 401; three bad tokens ban the address for every POST, a good
token included; the token file is read on every request (rotated, or
removed, mid run); a bearer token or token file that is not ASCII or not
UTF-8 drops the connection with no reply, as `compare_digest` raising
does; the text cap counts code points; Content-Length is read as `int()`
reads it (`1_0`, padded, Unicode digits); `parse_qs` decides `cleanup=1`;
http.server's own 501 page for other methods; a port already in use
(`[Errno 98] Address already in use`, exit 1); and the arm grant file name
is `quote(key, safe="")`. An `expires_at` of `"inf"`, `Infinity` or
`1e999` reads as armed on every side, as the oracle reads it.

Status: in force.
Retires when: does not retire.

## 2026-10-01: Moonshine in, sherpa-onnx out

**VO-11 (ruling): the parakeet engine is removed.** By owner choice of
defaults, sherpa-onnx is not used (docs/research/stt-2026-10.md), so the
engine that ran Parakeet on it is gone from Python (the engine
class, its pins, the `[voice-parakeet]` extra in pyproject.toml and
uv.lock, the config comment, the docs) and from both ports' refusal text.
`voice_engine: parakeet` is now an unknown engine on every side: pick_engine
raises UnknownEngine (exit 1 from `voice serve`) with one line, `voice_engine
parakeet was removed. Parakeet will return on a different runtime.` and then the valid names. The `pick parakeet` case lost
its `port_expect`, and a cli case `serve parakeet removed` was added. Red was
shown on the Go and Rust binaries of the stage H build: 8 of 431 cases
disagreed on each (the pins answer, the two parakeet cases, and the five
unknown-engine messages whose list of valid names changed). The module map
change is VO-8's.

Status: retired 2026-10-01 by VO-16: Parakeet returns on parakeet-cli
(CrispASR's Parakeet code and the ggml CPU backend, no other CrispASR
code). `voice_engine: parakeet` is a valid engine again on every side,
and REMOVED_ENGINES is gone.
Retires when: Parakeet returns on another runtime (done).

**VO-13 (ruling): the Moonshine engine.** Checked 2026-10-01, with sources:

- License. The LICENSE of github.com/moonshine-ai/moonshine at v0.1.5
  (commit 234f60faa0eb388b01cdf7e60aca232af37aefda) says the code, apart
  from core/third-party, is MIT, and that every streaming model and every
  English model is MIT; only the legacy non-streaming models for Arabic,
  Japanese, Korean, Mandarin, Spanish, Ukrainian and Vietnamese stay under
  the non-commercial Moonshine Community License. The Hugging Face cards of
  moonshine-ai/moonshine-streaming-tiny, -small and -medium carry
  `license: mit` in their metadata; those repos hold no LICENSE file of
  their own. The developer is Moonshine AI (formerly Useful Sensors, US).
- The C API is core/moonshine-c-api.h: `moonshine_load_transcriber_from_files`
  (a model directory and an architecture: TINY_STREAMING 2, SMALL_STREAMING
  4, MEDIUM_STREAMING 5), `moonshine_transcribe_without_streaming` (16 kHz
  float PCM in, lines of text out), `moonshine_free_transcriber` and
  `moonshine_error_to_string`. At this commit the architecture only chooses
  the streaming decoder, so a model directory named by path runs as small.
- The repo ships no command-line program for a WAV file on Linux (its C++
  example prints streaming events for a fixed file; the CLI transcriber is
  a Windows project). So `clients/native/moonshine-cli` is the smallest C
  program on the C API: `-m MODEL_DIR -a tiny|small|medium -f CLIP`, one
  transcript line per stdout line, exit 3 when the model does not load, 4
  for a clip that is not a 16 kHz mono 16-bit WAV, 5 when transcription
  fails. The engine stays an external process, as whisper.cpp is, and the
  three sides run it the same way: `moonshine-cli -m DIR -a ARCH -f
  TMP.wav`, stdout's stripped non-blank lines joined by a space, a nonzero
  exit named with the last stderr line. The models are English, so no
  language is passed.
- The build. `clients/go/scripts/native.sh --moonshine` builds it into a
  prefix of its own (`$XDG_CACHE_HOME/opendaisugi/native/moonshine-<stamp>`:
  bin/moonshine-cli, lib/libonnxruntime.so.1, the two licenses), so the
  Z3 prefix and its stamp are untouched. The Moonshine sources are pinned
  by commit (a blobless clone of core/). ONNX Runtime is Microsoft's
  prebuilt 1.23.2 CPU release, pinned by the SHA-256 GitHub publishes for
  the asset (1fa4dc... for x86_64, 7c63c7... for aarch64); its
  libonnxruntime.so.1.23.2 is byte for byte the copy Moonshine vendors.
  A source build of ONNX Runtime was not tried: it is far heavier than this
  box's memory rules allow. The upstream diarizer (cpp-annote, which
  compiles kaldi-native-fbank, copyright Xiaomi Corporation) and the
  ZipVoice text-to-speech engine (a k2-fsa model, with its reference clips
  embedded in a source file) are replaced by stubs, so no k2-fsa code or
  data is compiled in. Moonshine's other text-to-speech code (its own) is
  linked because the C API names it; moonshine-cli never calls it.
  install.sh links moonshine-cli onto PATH, and goes on without it, with
  one line, when the build fails. A build from nothing, in fresh build and
  prefix directories, gave the same moonshine-cli byte for byte.
- The models. The runtime at that commit loads the quantized .ort files its
  model catalog names on download.moonshine.ai, under
  `<name>-streaming-en/quantized_26_08_21/`, not the Hugging Face layout
  (no tokenizer.bin). So that is where they come from: eight files per
  model (the word-timestamp decoder is not fetched). Each file's size and
  CRC32C matched the catalog's
  (core/moonshine-model-file-metadata.generated.cpp at the commit) when it
  was first downloaded on this box; the SHA-256 taken then is the pin. That
  is trust on first use, checked against the vendor's own digest. tiny,
  small and medium are 45, 142 and 269 MB. medium was downloaded too,
  only to take its pins.
- Config. `voice_engine: moonshine`, with `voice_model` tiny, small or
  medium (a curated model, fetched once into
  `$XDG_CACHE_HOME/opendaisugi/models/moonshine/...` when XDG_CACHE_HOME is
  absolute, else `~/.cache`) or a model directory. Moonshine with no
  voice_model set means small. A curated model's fetch says one line,
  `Fetching the Moonshine small model (142 MB) into DIR. This happens
  once.`; a failure says `The Moonshine small model could not be fetched.
  Check the network, then run daisugi voice serve again.` (or, for a file
  that fails its pin, that it was deleted and the same command fetches it
  again). Exit 3. No sentence says "not installed", which coppice reads as
  a pip hint.
- No config. A config whose voice_engine and voice_model are unset or at
  their defaults (`faster-whisper` and `tiny.en`) means faster-whisper when
  its package imports, and otherwise Moonshine small, after `faster-whisper
  is not available, so voice uses Moonshine small.` The defaults count as
  no choice because save_config writes every field: a box that ever saved
  its config names them without choosing them, so "key absent" could not
  tell it from a fresh box. A port never imports faster-whisper, so the Go
  and Rust daisugi serve voice with no config, under coppice too.
  Moonshine with voice_model unset or `tiny.en` means small. A config that
  names faster-whisper with another model, or another model alone, is not
  changed: VO-1 holds there. The Python build's faster-whisper now loads its model
  from the cache first; only a model that is not there is fetched, after
  `Fetching the faster-whisper tiny.en model (78 MB). This happens once.`,
  at the commit pinned for tiny.en (the commit fixes model.bin's LFS
  digest); a failure names `daisugi voice serve` to retry.
- The ports fetch with the same pins, URLs and cache paths, and say the
  same lines. They do not resume a partial download: a stopped one starts
  again from nothing, where the oracle resumes its `.part` with a Range
  request. No case starts with a `.part`. `OPENDAISUGI_MOONSHINE_BASE_URL`
  moves the download host for the cases; every case runs with it pointing
  at a closed loopback port unless it serves a fake host.
- Cases: 55 new (the args, picking it, the pinned fetch through a fake host
  or a closed port, the generic fetch with a caller's digest, a fake
  moonshine-cli in probe, cli and server cases). No case loads a real
  model or reaches the real host; the oracle runs with faster_whisper
  hidden. Red: 64 of 482 cases disagreed on the Go binary of the parakeet
  commit, shown before the Go code was written; the same 64 disagreed on
  the Rust stage H binary, but that run came after the Rust code was
  written and built, so for Rust red did not come first. The change from
  "key absent" to "at the default" was cased first (10 of 486 cases red on
  both earlier binaries) but its port code was written before that red run
  too. Go and Rust then agreed on all 486, and `--oracle` found no stale
  fixture.
- Known limits. The ports do not resume a download, so on a slow link a
  first fetch that runs past coppice's three-minute health wait is killed
  and starts again from nothing next time. A port's first run says two
  lines (the engine notice and the fetch line). moonshine-cli is built by
  install.sh only; the AUR package and the release do not ship it yet, and
  its message names the repo's native.sh. The pins are trust on first use,
  checked against the vendor's own CRC32C.
- One real check, outside the cases, on 2026-10-01: each of the three
  sides ran `voice serve` with no config, an empty cache on real disk and
  the real host. Each said the two lines, fetched the eight files of
  Moonshine small, checked their pins, bound and answered `/health`. No
  clip was sent, so no model ran. The Python side first failed: the host
  answers Python-urllib's default User-Agent with 403. All three fetchers
  now send `User-Agent: opendaisugi`.

Amended 2026-10-01 (VO-16 to VO-20): moonshine-cli now runs as one
resident child, `moonshine-cli -m DIR -a ARCH --resident`, not once per
clip (O-1, VO-19), and runs one second of silence before each clip
(VO-20). A config with no voice choice now gets the engine the hardware
picks (VO-17), so the "No config" item above and its notice line are
replaced; the Moonshine small fallback holds where Parakeet is not
installed or the box is smaller.

Status: in force.
Retires when: does not retire.

**VO-12 (ruling): the default engine stays faster-whisper.** The brief:
make `moonshine` with `small` the default if Moonshine small is at least as
accurate as tiny.en and not slower on this box. It is neither. On six
espeak-ng clips (the three pinned fixtures and three longer sentences, CPU
only, `CUDA_VISIBLE_DEVICES=""`), Moonshine small's mean WER was 0.107
against 0.087 for tiny.en, and its wall time per clip 1.49 s (process
start, model load and decode; the decode alone 1.16 s) against 0.43 s for
tiny.en on a loaded model. The table is in docs/research/stt-2026-10.md
("Measured on this box"). `voice_engine` keeps its default,
`faster-whisper`, and `voice_model` keeps `tiny.en`. Where faster-whisper
cannot load (no `[voice]` extra, and every Go and Rust daisugi), a config
with the voice settings unset or at their defaults runs Moonshine small
(VO-13).

Amended 2026-10-01: measured again on real speech, 100 utterances each of
LibriSpeech test-clean and test-other (docs/research/stt-2026-10.md,
"Measured on this box (real speech)"). Moonshine small is now the more
accurate (WER 4.85% and 10.39% against 5.26% and 13.77% for tiny.en) but
still slower: 1.83 to 1.92 s for a 4 to 6 s clip, one process per clip,
against 0.42 to 0.45 s. The default does not change; the reason is now
speed alone.

Status: retired 2026-10-01 by VO-17: with no voice choice the engine now
follows the hardware, and faster-whisper tiny.en is the low tier only.
Retires when: a measurement with real voices, or Moonshine's streaming
API, shows Moonshine small at least as accurate as tiny.en and not slower
(overtaken by VO-17).

## 2026-10-01: CrispASR checked, real-speech bench, no new engine

**VO-14 (ruling, provisional): the curated voice defaults.** Until the
owner rules, the curated defaults leave out Phonon-1, NVIDIA
Canary-Qwen-2.5B and CrispASR's Qwen3-ASR and Qwen3-TTS backends. This
shapes defaults only; a user may still choose any model. No code changes
with this ruling.

Status: retired 2026-10-01 by owner ruling O-3, which makes it the
owner's ruling.
Retires when: the owner rules on the curated voice defaults (done).

**VO-15 (ruling): no engine replaces the default; CrispASR only as a
Parakeet-only build.** Two checks, both in docs/research.

CrispASR (github.com/CrispStrobe/CrispASR, MIT, commit 7e2b0307): the
full build is too large for the default, as Moonshine's tree was (VO-13).
It compiles OmniVoice prompt tables and pypinyin tables into its core
library, and its one library target links many model ports the voice
bridge does not use. A program
on its Parakeet API, built from five CrispASR source files and the ggml CPU
backend, has none of that: `strings` and `nm` find no such code, and no
download code. The maintainer says he is Christian Ströbele of Stuttgart,
Germany; that is self-stated. The GGUFs of `cstr/parakeet-tdt-0.6b-v2-GGUF`
and `cstr/phonon2-GGUF` are reproducible: CrispASR's own converter, run
with four small stand-ins for Python packages this box does not have, gave
the published F16 tensors byte for byte from NVIDIA's .nemo and from
Fermion's archive (one filterbank tensor within 4e-9, from a stand-in), and
its quantizer gave the published Parakeet Q8_0 byte for byte. The details
are in docs/research/stt-2026-10-pass2.md, "CrispASR provenance".

The bench (docs/research/stt-2026-10.md, "Measured on this box (real
speech)"): the brief's bar for a new default was a lower WER than tiny.en
on both sets and a median under about 1.0 s for a 5 s utterance on this
CPU. Parakeet v2 on CrispASR passes the first by far (WER 1.95% and 3.99%
for Q4_K) and fails the second: 1.22 to 1.30 s for a 4 to 6 s clip as one
process per clip, the shape a voice engine runs in, and 1.02 to 1.16 s
even with the model resident. Phonon-2 Q8_0 takes 1.12 s as one process
per clip and 0.85 to 0.89 s resident. It could not be the default anyway:
its weights are CC-BY-4.0, so attribution must ship with them, and its
retraining used SPGISpeech under Kensho's own terms, which the owner has
not ruled on. Kyutai stt-1b-en_fr was not built: on a CPU its Rust binary
loads the 1.98 GB bf16 weights as f32 (about 4.0 GB, plus 0.38 GB for the
codec), over the 4 GB cap of every heavy run here. faster-whisper base.en
beats tiny.en on both counts but puts Python in the engine path and cannot
run in the ports (VO-1).

So nothing is wired and no default changes. For the owner: a resident
engine (one process that keeps the model loaded) would bring Phonon-2
under 1.0 s, and maybe Parakeet v2 Q4_K; that is a change to how the voice
bridge runs engines, not a new engine.

Status: retired 2026-10-01 by VO-16. The owner set the bar for a 5 s
clip at about 1.2 s, with the engine resident (O-1); Parakeet v2 Q4_K
meets it (1.04 to 1.10 s, VO-16). The CrispASR half of this ruling holds
and moved into VO-21. The Phonon-2 sentences are history only: O-2
excludes it.
Retires when: an engine fit for the curated defaults beats tiny.en on WER on
both sets and answers a 5 s clip in under about 1.0 s on this CPU, in the
shape the voice bridge runs it (overtaken by VO-16).

## 2026-10-01: stage I, coppice in Rust, slice 1 (the server core)

`harness/coppice-rs` is the Rust coppice; the map and its six slices are
in `docs/plans/2026-09-24-omarchy/plan-coppice-rust.md`. Slice 1 is the
wire codec in both framings, the socket, the start lock and the uid
check, roles by SO_PEERCRED and the process tree, hello, the guard,
notes, `agent.allow` and `agent.deny` over the gate's ask files, the
pane, tab and workspace verbs over pty panes, attach with flow control
and view-only, events, tasks, `pane.report_state`, the agent verbs, and
`server start`, `stop`, `status` and `token`. Terminal emulation is the
same pinned libghostty-vt as Go's, through its C API.

`clients/coppice_compare.py` starts a fresh Go server and a fresh Rust
server per corpus file, each with its own scratch HOME, socket, data dir
and start dir, `PATH=/usr/bin:/bin`, and a `coppice.toml` of
`plugins = []`, `gateway = ""` and `[voice] enabled = false`. Gate A is the corpus matcher
(`tests/floor/protocol_match.py`); gate B is full reply equality after
the normalization CP-R-1 and CP-R-2 name. Red was shown first, on the
skeleton that answered `server.status` only (commit `05956534`): of 70
cases, 6 agreed, 39 disagreed and 25 were refused, and every file was
red. On the slice-1 server all 70 cases of all 9 files agree, none is
refused, and no pane process outlives its server. The Go code, its tests
and its corpus results were not touched. Gate B was shown to fire on its
own: a throwaway Rust build that changed only the not-attached message,
which the corpus masks with `"*"`, passed gate A and disagreed on
flow.jsonl cases 2 and 7 (the two cases that send that message) and
nowhere else.

**CP-R-1 (normalization): the known-commands list.** An unknown verb's
message ends with `Known commands: ` and the server's verbs. The Rust
list is shorter until every slice lands, so the driver replaces the tail
with `{COMMANDS}` on both sides. The words before it must match.

Status: retired 2026-10-02 by slice 5. The Rust server registers every
verb Go does, voice.start and voice.status last, so the whole message is
the same text in both, and the driver compares it whole.

**CP-R-2 (normalization): per-process values.** Each server's socket,
data dir, start dir, home and work dir become placeholders, and the
values of the keys `pid`, `uptime_s`, `ts`, `quiet_for` and `ended_at`
are masked wherever they appear. Every other value, every message and
every row's key set is compared.

Status: in force.
Retires when: does not retire.

**CP-R-3 (ruling): harness and headless panes are refused.** A
`pane.create` or `pane.split` that names a harness, or asks for a
headless pane, passes every check Go makes before the record is made and
is then refused with `bad_request` and one sentence that names the later
slice. The ready-prompt rules, screen detection, transcript facts and the
adapters all hang on a harness, so a harness pane here would act
differently from Go's. Refused, not loose: no pane starts. No corpus case
names a harness.

Status: narrowed 2026-10-01 by slice 2. A pty pane that names a harness
now starts, with screen detection and the ready-prompt rules; only a
headless pane, and pane.fork of one, is refused. Retired 2026-10-02 by
slice 4, which ports the five adapters, headless panes and pane.fork
(cases/headless and the adapters suite).

**CP-R-4 (ruling): verbs not in slice 1.** `pane.trust`, `pane.explain`,
`pane.forget`, `pane.resume`, `pane.report_child`, `floor.facts`,
`floor.talk`, `floor.foreman`, `project.list`, `voice.start` and
`voice.status` are not registered, so the Rust server answers them
`no command` and the driver counts such a case as refused. No corpus case
sends one.

Status: narrowed 2026-10-01 by slice 2: `pane.trust`, `pane.explain`,
`pane.report_child`, `floor.facts`, `floor.talk` and `floor.foreman` are
registered. `pane.forget`, `pane.resume`, `project.list`, `voice.start`
and `voice.status` are not. Narrowed again 2026-10-02 by slice 3:
`pane.forget`, `pane.resume` and `project.list` are registered
(cases/ended). Only `voice.start` and `voice.status` are left.
Retired 2026-10-02 by slice 5, which ports the voice supervisor
(cases/cli/voice-*.jsonl).

**CP-R-5 (ruling): no foreman.** `task.set_foreman` with an empty pane
clears the foreman, as Go does. Naming a live pane is refused with
`bad_request`, after Go's own no-task, no-pane and closed-pane checks:
held asks are not ported, and a foreman without them would let asks
through that Go holds. So no hold ever exists here, and `agent.deny` from
a pane is always the pane refusal.

Status: retired 2026-10-01 by slice 2, which ports holds, the foreman's
deny and floor.talk (cases/facts/holds.jsonl and foreman.jsonl).

**CP-R-6 (ruling): no task worktrees.** `task.create` with
`worktree: true` is refused after Go's needs-cwd check.

Status: retired 2026-10-02 by slice 3, which ports `worktree`
(cases/ended/worktree.jsonl).

**CP-R-7 (ruling): no restore.** `coppice-rs server start` refuses a data
dir that holds `layout.json`, since it cannot restore it and its first
save would write over it. The driver gives each server an empty data dir.

Status: retired 2026-10-02 by slice 3, which ports Restore
(cases/ended/restart.jsonl and resume.jsonl restart both servers on
their own data dirs).

**CP-R-8 (ruling): facts in slice 1.** A pane.list row carries `stack`
(the router, from the pane's `ANTHROPIC_BASE_URL` or `OPENAI_BASE_URL`
against the gateway) and `gate` (the last verdict a report carried), as
Go builds them. Transcripts, tokens and the model are not read: Go reads
them only for a Claude or sprig pane, which CP-R-3 refuses, so a slice-1
row never lacks a fact Go would show. The gateway is not dialed.

Status: retired 2026-10-01 by slice 2, which ports the transcript reader,
claims, floor.facts and the loopback dial (cases/facts/facts.jsonl). The
one part left is CP-R-17.

**CP-R-9 (ruling): no plugins.** The Rust server runs no plugin, so a
hello with `role: plugin` is `no plugin X is enabled.` The driver runs Go
with `plugins = []` too, so neither side starts a policy process.

Status: retired 2026-10-02 by slice 5, which ports the plugin loader, the
policy runner and the plugin role (cases/cli/plugins.jsonl and
plugins-default.jsonl). The scratch config still says `plugins = []`
for every file that does not test plugins.

**CP-R-10 (ruling): two text edges.** Go's `%q` uses `strconv.IsPrint`;
the Rust port uses a table of the non-printing ranges (controls, format
characters, separators other than space, private use, noncharacters), so
an unassigned code point may quote differently. A request line that is
not UTF-8 is read with one U+FFFD per bad sequence, where Go puts one per
bad byte. No case reaches either.

Status: in force.
Retires when: a case shows a difference, which is then fixed.

**CP-R-11 (ruling): events are not compared yet.** The corpus skips every
line with no id, so frames, `frame_gap`, state, note and presence events
are ported but not compared in slice 1.

Status: narrowed 2026-10-01 by slice 2. The driver now keeps every line
with no id, on the case connection and on a second connection a file can
open with `#!watch`, and compares them once per file through an event
view. The view leaves out two kinds of state event and one kind of frame,
whose order and count follow the pty and the tick, not the requests: a
state event from the process source, a state event from the manifest
source, and a frame that is not full. A full frame is compared with its
`seq` masked. The events of `protocol/flow.jsonl` are not compared at
all: it pauses its pane right after the attach, so whether the first full
frame goes out first is a race on either server (a Go-against-Go run
showed it). What the tick read and wrote is still compared: the verdict
by `pane.explain`, and the state, source and detail the tick applied by
`agent.get` once the tick's state has won (cases/facts/tick.jsonl).
Retires when: does not retire. The left-out kinds are timing.

Narrowed again 2026-10-02: the view compares the order of events within
each kind (frames among frames, notes among notes), not across kinds.
Events of different kinds come from different goroutines and threads, so
a note and a full frame can reach the connection in either order. CI on
737a2a10 showed it once in cases/facts/explain.jsonl (Go sent the note
first, Rust the frame), while all 307 cases agreed.

**CP-R-12 (ruling): the command line.** The binary is `coppice-rs` and
carries the server commands only. The floor and the client verbs refuse
with one line.

Status: narrowed 2026-10-02 by slice 5. The binary is still
`coppice-rs` and prints Go's usage text. It carries every command of Go's
command line but two: the floor on a terminal (`coppice` with no
arguments; on a pipe it prints the usage, as Go does) and `coppice web`.
Each of the two refuses with one line that names the last slice.
Narrowed again 2026-10-02 by slice 6a, which ports `coppice web` and
its subcommands. Only the floor on a terminal is left; it refuses with
one line that names the last slice.
Retired 2026-10-02 by slice 6b, which ports the floor on a terminal; the
Rust binary is named `coppice`.

## 2026-10-01: stage I, coppice in Rust, slice 2 (what the floor knows about a pane)

Slice 2 ports screen detection (`detect` and the manifest tick),
`pane.explain`, the ready-prompt rules and `pane.trust`, asks held for a
foreman, the foreman and `floor.talk`, subagents (`pane.report_child`),
and the facts and stack (the transcript reader, claims, `floor.facts`).
The 21 Herdr manifests are compiled in from the Go tree. The regex crate
(=1.13.1, std and unicode) is the one new crate.

`clients/coppice_compare.py` now replays four suites: the protocol
corpus; hand-written cases in `harness/coppice-rs/cases/facts/` (explain,
tick, holds, foreman-none, foreman, children, facts); a screens suite built at
run time from `harness/coppice/testdata/screens`, where each fixture is
drawn by a fake harness in a pane exactly its size and must get its
declared state and rule from `pane.explain`, then one prompt; and an
events suite built from `harness/coppice/testdata/events`, each fixture
reported for a pane of its own. Case files may set `coppice.toml` lines,
open an event watch and retry a read-only verb. A Go-against-Go run
agreed on every case and on every event view but flow.jsonl's (CP-R-11).
Red was shown first, on the slice-1 build: of 307 cases, 110 agreed (the
70 protocol cases among them), 168 disagreed and 29 were refused, and 6
of 20 event views disagreed. On the slice-2 build all 307 cases and all
20 event views agree, nothing is refused, and no pane process outlives
its server. The Go code, its tests and its corpus
results were not touched.

**CP-R-13 (normalization): values that follow a tick or a clock.** The
values of the keys `received_age_s`, `effective_state`,
`effective_source` and `tick` (pane.explain), `since` and `until` (a
hold's held field), and `at` (a gate verdict) are masked wherever they
appear. Each is read from the manifest tick, the process tick or the
clock at the moment of the request. `manifest_state`, `rule_id`,
`evaluated` and `detection_text`, which follow from the screen alone, are
compared exactly. The mask goes by key name in every reply, so a later
slice that adds a key with one of these names checks that it follows a
clock, or scopes the mask to its verb.

Status: in force.
Retires when: does not retire.

**CP-R-14 (ruling): the text of a manifest load error.** A manifest that
does not parse is skipped with a warning in `detection_warnings`. Go's
warning carries BurntSushi/toml's message and lists unknown keys by name;
Rust's carries the toml crate's message. The checks after a parse (an
empty id, the engine version, no rules, a bad region or state, a pattern
that does not compile, the caps) say the same words. Only an operator's
override file can fail to load, and no case has one.

Status: in force.
Retires when: a case with a broken override file shows a difference that
matters, which is then fixed.

**CP-R-15 (ruling): two regex engines.** Go compiles a manifest pattern
with its RE2-shaped regexp; Rust with the regex crate, after a rewrite to
Go's meaning: `\d`, `\s`, `\w` and `\b` and their capitals become Go's
ASCII classes, a `[` or a doubled `&`, `~` or `-` inside a class becomes
a literal, and `\p{Alphabetic}` becomes `\pL` as Go rewrites it. Left
over: the two engines' Unicode tables may differ by version, and a
construct no bundled manifest uses (`\Q...\E`, say) may load in one and
not the other. Every bundled pattern compiles in both, and every screen
fixture gets the same verdict and the same `evaluated` rows in both.

Status: in force.
Retires when: a case shows a difference, which is then fixed.

**CP-R-16 (ruling): which subagent goes first on a tie.** Past 32
subagents on one pane, the oldest is dropped. Go picks among equally old
ones in map order, which is random; Rust drops the one with the least
id. Only a 33rd subagent reported in the same nanosecond as another
reaches it.

Status: in force.
Retires when: does not retire. Go's choice is not deterministic.

**CP-R-17 (ruling): sprig session trees.** The transcript reader reads
both Claude transcripts and sprig session trees, and its tests replay
`harness/coppice/testdata/facts` for both kinds with Go's own numbers.
But only a headless sprig pane has a tree to read, and a headless pane
is refused (CP-R-3), so the server never reads one.

Status: retired 2026-10-02 by slice 4. A headless sprig pane's facts read
its session tree: cases/headless/sprig.jsonl lists a pane whose tree
holds a deny verdict and an assistant row with tokens, and both servers
show the same gate mark and tokens in pane.list and floor.facts.

**CP-R-18 (ruling): what the cases do not reach.** These paths are
ported but no case drives them, because each needs minutes or a second
pane connection in one file, or a live gate: a hold that ends after its
120 s, a prompt queue dropped after 10 min, the foreman's 30 s notes, a
transcript two panes claim, the read that marks a hung file refused
after 5 s, and a foreman's deny that succeeds (holds.jsonl reaches the
deny up to the gate's "no pending ask", since no gate wrote the ask
file). The
Rust unit tests cover the reader's own rules (a split message counted
once, a line not yet ended, a line over the cap, a path that is not a
regular file, the files a pane lets go of).

Status: in force.
Retires when: a case reaches each path, with shorter timings that both
servers would take as a flag.

## 2026-10-02: stage I, coppice in Rust, slice 3 (ended panes, restart, projects, task worktrees)

Slice 3 ports `ended.go` (`pane.forget`, `pane.resume`, the seven-day
sweep), `restart.go` (Restore and its report), `projects.go`
(`project.list`; the recent directories and the default cwd were already
ported), the `worktree` package, and `layout.json` loading with Go's
repairs. `coppice-rs server start` now restores a data dir as Go does,
after it listens.

`clients/coppice_compare.py` gains three directives. `#!repo` makes a
scratch git repo per side at `{WORK}/repo`, with a cleared environment
(scratch HOME, `PATH=/usr/bin:/bin`, `GIT_CONFIG_NOSYSTEM=1`). `#!tree`
compares the two data dirs as file trees. `#!restart` stops both servers
(SIGTERM, then the leak check) and starts them again on the same data
dirs, on fresh connections, with the names bound so far kept. Each
server now runs with `GIT_CONFIG_NOSYSTEM=1` and a git ceiling at the
scratch root, and the scratch root is resolved, since git names a
directory by its real path. Five files in `harness/coppice-rs/cases/ended`
hold 100 cases and 9 tree views. A Go-against-Go run agreed on all of
them. Red was shown first, on the slice-2 build: of the 56 cases it
reached, 18 agreed, 28 disagreed and 10 were refused, all 4 trees it
reached disagreed (recent-dirs.json was 0644 where Go makes it 0600), and
the restart and resume files failed at the start (CP-R-7). One more case
in cases/facts/holds.jsonl pins the task fold of a held ask (a blocked
pane whose ask a foreman holds folds as working), which the slice-2
build got wrong. On the slice-3 build all 408 cases, all 25 event views
and all 9 tree views agree, nothing is refused, and no pane process
outlives its server. The Go code, its tests and its corpus results were
not touched.

**CP-R-19 (normalization): the data dir as a file tree.** A tree view
maps each path under the data dir to its type, its mode bits, and its
text after the reply normalization (CP-R-2: each server's paths become
placeholders). Two values are masked: each `"ended_at"` in a file, the
clock's second as in a reply, and the pid at the start of `server.lock`.
Every other byte of `layout.json`, `recent-dirs.json` and any other file
is compared, so the saved shape, the key order and the indentation are
checked, not only the values.

Status: in force. Narrowed 2026-10-02 by slice 4: the temp file of a
save in flight (`.layout-N.json.tmp`) is left out of a tree view. A
Go-against-Go run once caught that file on one side only, in
cases/headless/claude-exit.jsonl's tree after its restart. What wrote it
was not found.
Retires when: does not retire.

**CP-R-20 (ruling): a headless record at a restart or a resume.** Go
resumes a headless record that carries a harness session id through its
adapter, at a restart and on `pane.resume`. This build has no adapters
(CP-R-3). At a restart such a record comes back closed and unknown, with
the note Go writes when an adapter refuses the start (`did not resume:`
and the CP-R-3 sentence); a harness Go has no adapter for gets Go's own
`no adapter named` note. `pane.resume` of a headless record answers
`bad_request` with the CP-R-3 sentence and leaves the record as it was,
as Go does when a start fails. No case reaches either, since no case
can make a headless pane.

Status: retired 2026-10-02 by slice 4. Both ports resume a headless
session through its adapter at a restart and on pane.resume, and write
the same restore report (cases/headless/claude-exit.jsonl and
opencode.jsonl restart with a live headless pane; claude-exit.jsonl and
pi.jsonl resume an ended one).

**CP-R-21 (ruling): text edges of a broken file or a missing git.** A
`layout.json` that does not parse is refused at start in both, with
`layout file is not readable:` and then the parser's own words, which
differ: Go's encoding/json against serde_json. Go also matches JSON keys
without regard to case and keeps a null `children` list as null, where
this build reads keys exactly and writes `[]`. Only a hand-edited file
reaches these. When git is not on PATH, the worktree refusal carries
`exec: "git": executable file not found in $PATH` in both, but another
spawn error carries the OS words, which differ. The checks after a parse
(the schema version, the counter repairs) say the same words and are
unit-tested.

Status: in force.
Retires when: a case with a broken file shows a difference that matters,
which is then fixed.

**CP-R-22 (ruling): what the slice-3 cases do not reach.** These paths
are ported but no case drives them: two `pane.resume` calls of one
record at once (the claim refuses the second), the hourly sweep and the
sweep of a record older than seven days (no case can move the clock),
the resume of the tracked foreman's record, an upstream that cannot be
set after a worktree add, a worktree that git cannot read at
`task.close`, and a detached repo (a unit test covers the last).

Status: in force.
Retires when: a case reaches each path, with a clock or timing that both
servers would take as a flag.

**CP-R-23 (ruling): a worktree made from inside another worktree.** A
task made with `worktree: true` and a cwd inside another task's worktree
gets its worktree beside that worktree (`feat-worktrees/again`), since
`git rev-parse --show-toplevel` names the worktree. At `task.close`,
the repo is found through the shared `.git` instead, so the path to
remove names the main repo (`repo-worktrees/again`), git answers `is not
a working tree`, and the close fails with `internal` after its panes
closed. This is a fault in Go. The Rust port keeps it, so the two agree
(cases/ended/worktree.jsonl case 22), and Go is not changed in this
slice.

Status: retired 2026-10-02. The cause: `task.close` found the main repo
through the shared `.git` and then built the path again as
`<main repo>-worktrees/<label>`, which is not where `task.create` put a
worktree made from inside another one. Both ports now remove the path
the task recorded, through the main repo (`worktree.RemoveAt` in Go,
`worktree::remove_at` in Rust), so the close succeeds whether or not the
outer task closed first. A Go server test, a Go worktree test and a Rust
unit test pin it, and case 22 now expects the close to succeed and case
23 an empty task list.

## 2026-10-02: stage I, coppice in Rust, slice 4 (headless harnesses)

Slice 4 ports `internal/adapters`: claude and pi (one long-lived process,
JSON lines both ways), codex and sprig (one process per prompt, in its
own process group, the sprig session tree tailed between turns) and
opencode (a loopback `opencode serve` with a random password, its HTTP
API and event stream, the asks it holds itself, the gate plugin check).
The server gains headless panes and the pump that turns adapter events
into grid rows and headless states, `pane.fork`, `agent.prompt` and
`pane.send_text` to an adapter, `agent.allow` and `agent.deny` of a
harness-held ask, the resume of a headless session at a restart and on
`pane.resume`, the adapter pids in role placement, and the sprig session
tree in the facts. Go's encoding/json error words are kept by a small
decoder (`godec.rs`): the scanner's syntax errors, the struct type
errors, and the raw text of a json.RawMessage field. No crate is added:
the HTTP client, SHA-256 and the decoder are written here.

The compare driver gains `#!env` (server environment, so each adapter
runs its fake through COPPICE_CLAUDE_BIN and the like) and `#!mask`
(one key masked in one file), copies the fake harnesses into the
fixtures, and puts the OpenCode gate plugin in each side's scratch HOME.
Seven files in `harness/coppice-rs/cases/headless` hold 128 cases, and a
new adapters suite replays each stream fixture of
`harness/coppice/testdata/adapters` in a headless pane (15 cases). The
fakes are Go's fake-claude.sh, fake-codex.sh and fake-sprig.sh, and two
new ones beside the cases: fake-pi.sh and fake-opencode.py (stdlib only;
it replays Go's captured OpenCode stream). Red was shown first, on the
slice-3 build: of 552 cases, 415 agreed (all 408 earlier ones among
them) and 137 disagreed; 9 of 35 event views and 8 of 17 tree views
disagreed. On the slice-4 build all 552 cases, 35 event views and 17
tree views agree, nothing is refused, and no pane or harness process
outlives its server. Go was not changed in this slice past CP-R-23.

**CP-R-24 (normalization): a minted sprig session id.** A sprig pane
started with no `--session` gets an id the adapter makes: `coppice-` and
8 random hex digits. The driver writes it as `coppice-{MINTED}` in every
reply, event and data-dir file (cases/headless/sprig.jsonl, the `mint`
pane).

Status: in force.
Retires when: does not retire. The id is random by design.

**CP-R-25 (normalization): an ask's deadline in a headless file.** pi
and opencode put the time an ask runs out (now plus 90 s, or a dialog's
own timeout) in the ask's `deadline`. pi.jsonl and opencode.jsonl mask
that key with `#!mask deadline`. No other file masks it: a reported
ask's deadline in the other suites comes from the request and is
compared.

Status: in force.
Retires when: does not retire.

**CP-R-26 (normalization): the done of an operator close.** When the
operator closes a headless claude, pi or opencode pane, Go's adapter
picks between sending its end event and dropping it with a select on two
ready channels, which Go picks at random. So the done the pump posts says
`<harness> exited` or `adapter stream ended`, and a Go-against-Go run
showed both. The event view writes either as `{STOPPED}` for a headless
done. A done from a harness that exited on its own (`claude exited:
exit=3`) is compared as it is. The Rust adapters always send the end
event.

Status: in force.
Retires when: Go sends the end event every time, or never.

**CP-R-27 (ruling): text edges of the adapters.** These differ, or may,
and no case reaches them: a type error inside an anonymous struct the
opencode translator decodes (the error is dropped, so only whether one
happened counts, and that matches); Go's net/http words for a transport
fault (a reset, a malformed response); bufio.Scanner's words for a read
error other than a line over 8 MiB; a pi dialog timeout with a
fraction of a millisecond (Go cuts it the same way, but the deadline is
masked anyway, CP-R-25); and a bare harness name resolved on a PATH entry
that is relative.

Status: in force.
Retires when: a case shows a difference, which is then fixed.

**CP-R-28 (ruling): what the slice-4 cases do not reach.** These paths
are ported but no case drives them, because each needs 90 s or more, a
harness that misbehaves in a way the fakes do not, or a verb no client
sends: an OpenCode ask that passes its deadline and the reject that
follows (and a reject that fails); the OpenCode event stream ending while
its server runs; a codex turn whose background job keeps its stdout open
(Go's ORPHAN fixture); a harness that ignores stop, so the drain's one
deadline ends it; the restart of a fork that never named its own session
(the fork is started again); a pi `switch_session` that fails; and a
Claude SubagentStart or SubagentStop line, which the unit tests cover.
An adapter's Steer is not ported, since no verb of Go's server calls it;
pi's steer flag on a prompt sent while pi streams is ported. The Rust unit tests run
the claude, sprig and pi adapters on their fakes and replay Go's
captured OpenCode stream through the translator.

Status: in force.
Retires when: a case reaches each path, with shorter timings that both
servers would take as a flag.

## 2026-10-02: stage I, coppice in Rust, slice 5 (the command line)

Slice 5 ports Go's `cli` package but the floor screens and `coppice web`:
the client verbs with their tables and `--json` output, `server
start|stop|status|token` with Go's status text, `task list --tree`,
`skill`, `project add|list|rm`, `open` and `new` with the first-run
config, `--stdio` and `--remote` over ssh, and autostart. It ports
`attach` (the status line, the held leave key, whole runes, mouse
reports, SIGWINCH), the tmux mirror over tmux control mode, the plugin
loader with the shipped plugins carried in the binary, the policy runner
and the plugin role in the server, and the voice supervisor with
`voice.status` and `voice.start`. The config is written back in
BurntSushi/toml's layout. No crate is added.

`clients/coppice_compare.py` runs local requests beside protocol ones
(`clients/coppice_local.py`): a run of the side's own binary (`cli`, with
the socket and data dir the driver names; `pty`, on a pseudo terminal
read by a small terminal model), tmux on a scratch socket, file reads,
writes, counts and listings, and the server log. `#!plugins` copies
plugin directories in, and the leak check now also finds any process that
names a side's scratch dir. A keys suite sends each key of
`testdata/keys.json` through `coppice pane send-keys`. Twelve files in
`harness/coppice-rs/cases/cli` hold 301 cases, with fakes beside them (an
ssh that runs the side's own binary, a pi on PATH, a daisugi that serves
voice, 31 plugin directories). A Go-against-Go run agreed on the 300 of
them there were then. tmux's own output (a window's start command, the
exit code of a lookup of an unset option) is compared only between the
two sides, never against a fixed line, so a runner's tmux version cannot
fail a case. Red
was shown first for each part on the build before it: of 241 CLI cases
on the slice-4 build, 52 agreed; of 61 keys cases, 2; of 21 plugin
cases, 5; of 36 voice cases, 7 agreed and 28 were refused. On the
slice-5 build all 916 cases, 48 event views and 17 tree views agree,
nothing is refused, and no process outlives its server.

**CP-R-29 (normalization): what a command prints.** A CLI run's stdout
and stderr are text, so the key masks of CP-R-2 and CP-R-13 also apply
inside them: a JSON pair `"pid":123` becomes `"pid":"{MASKED}"` and a
line `pid 123` becomes `pid {MASKED}`, for each masked key, and the
status line `up 3s` becomes `up {MASKED}s`. `coppice --version` prints
the build's own version, which differs by design, so a whole line
`coppice VERSION` becomes `coppice {VERSION}`. `{BIN}` stands for each
side's binary, which the tmux mirror's window commands name. The Rust
side's work dir is named `rs`, two letters as `go` is, so a path wraps at
the same column on a terminal in both.

Status: in force.
Retires when: does not retire.

**CP-R-30 (normalization): the voice server's URL.** The managed voice
server listens on a port the kernel picks, so cases/cli/voice-fake.jsonl
masks `url` (`#!mask url`). Every other key of voice.status is compared,
`pid` masked as ever.

Status: in force.
Retires when: does not retire.

**CP-R-31 (ruling): text edges of the command line.** These differ, or
may, and no case reaches them: the words of a TOML parse error (Go's
BurntSushi/toml against the toml crate) after `cannot read coppice.toml:`
and in voice's off reason; a local date, time or date-time in a
`[plugin.<id>]` table, which Go turns into the local zone when it writes
the file back and Rust writes as read, and an offset of zero, which Go
writes `Z` and Rust `+00:00`; a mixed array of tables and values in a
plugin table, which Go refuses to encode; the read deadline on a
`--remote` ssh pipe, which Go sets and Rust does not; the words for a
plugin file that leaves its directory through a link (Go's os.OpenRoot),
for a manifest path that is a directory, and for an ssh start that fails
other than not found; a `/health` that redirects (Go follows it, Rust
takes only a 200) and an https voice url (Go speaks TLS, Rust reads it as
not answering); and a policy run as a `.py` that cannot start, where Go
names the resolved interpreter. Each is a path an operator's own unusual
file or setup reaches.

Status: in force.
Retires when: a case shows a difference that matters, which is then fixed.

**CP-R-32 (ruling): what the slice-5 cases do not reach.** These paths
are ported but no case drives them, because each needs minutes, a
terminal resize, or a fault the fakes do not make: attach when the
server goes away (exit 3) and when stdin closes, the SIGWINCH resize, the
150 s read deadline and a remote ssh's stderr tail, a detached server
that never answers, a plugin given up on after five tries (about 15 s)
and the runner's kill two seconds after its stop, a voice server that
never answers within three minutes or loses its port twice, the mirror
run inside tmux with no `--session`, a mark tmux refuses, and a tmux that
stops answering for five seconds. Rust unit tests cover the key reader,
whole runes, the frame render, `testdata/attach/status-lines.json`, the
two tmux transcripts of `testdata/tmux`, the mirror's plan and status
line, the embedded plugin list, manifest errors and slog quoting.

Status: in force.
Retires when: a case reaches each path, with shorter timings that both
servers would take as a flag.

## 2026-10-02: stage I, coppice in Rust, slice 6a (tiles and the web floor)

Slice 6a ports `internal/tiles` with Go's tests, `internal/web` and the
`coppice web` commands: `coppice web` with no verb (the floor on
loopback, signed in), `web serve`, `web token` and `web cert
init|show|tailscale`, and the start of the phone server from `web.json`
in `server start`. The server is written here: an HTTP/1.1 server with
net/http's framing (a body of 2048 bytes or less gets a Content-Length, a
longer one goes chunked), Go's ServeMux routing and redirects, http.Error,
FileServer and ServeContent with ranges; the websocket with
coder/websocket v1.8.15's handshake checks, read limit and close words;
the token guard, the ban list and the view refusal; the /proc/net/tcp
peer check, so a pane process may not answer an ask through the web; the
event ring; view plugins and the shared library; voice to a `--voice-url`
or the server's own; ntfy push with the lock-screen deny token; the QR
code, a port of rsc.io/qr v0.2.0 and qrterminal v3.2.1; and the local CA,
whose certificates are laid out as Go's x509.CreateCertificate lays them
out, so one CA directory is read by either port. The page is the Go
tree's `internal/web/static`, embedded by `build.rs` with Go's embed rule
(no name that starts with `.` or `_`), one copy for both binaries, and a
unit test holds the list and the bytes to the tree. Go's flag package is
copied for these commands' flags and usage text.

`clients/coppice_compare.py` gains a web suite (`--suite web`, the files
of `harness/coppice-rs/cases/web`) and two local requests: `http` (one
raw HTTP/1.1 request, plain or TLS checked against a side's own CA,
compared by status, every header but Date, and the body) and `ws` (a
websocket client: the handshake, the reply frames, the close code and
reason). Each side gets two loopback ports from a scratch range (18600 to
18999) as `{ADDR}` and `{ADDR2}`; no case listens on another address. A
`cli` run with `"peer": true` runs the other side's binary on this side's
data dir: Go's `cert init` then Rust's `cert show` and a Rust server on
Go's leaf, and the reverse, both checked by a TLS client that trusts only
that CA. Fakes beside the cases: a `tailscale` on PATH (the real one is
never run), an ntfy server and a voice server (python3, stdlib only, in a
scratch pane), and a websocket probe that a pane runs. Red was shown
first: on the slice-5 build, 12 of 89 cases agreed, and every file that
serves failed at its start. Go against Go agreed on all of them. On the
slice-6a build all 395 web cases and 13 event views agree; the full
compare, run in parts, agrees on all 1311 cases (the 916 earlier ones
among them), 61 event views and 17 tree views, none refused, no leak.
`harness/coppice/e2e/run.sh web` (`web.sh` in headless Chromium, from a
scratch copy of `e2e/` with its pinned Playwright from the local npm
cache) passes all seven checks against each binary.

One fault in the Rust server came out: it left its socket file behind
when it closed, where Go's listener removes it, so a client that dialed
after a stop got `connection refused` from Rust and `no such file or
directory` from Go. Fixed in Rust; every earlier case still agrees.

**CP-R-33 (normalization): a line of Go's default logger.** `web serve`
and the phone server log through Go's default logger: the date and time
at the start of a line become `{TIME}`, and a client's address in it
(`remote=` or `addr=` with a loopback port) has its port masked, since
the kernel picked it. The CA hand-off logs from its own goroutine in Go,
so its line and the phone server's first line may come in either order;
the driver puts the hand-off's first, which is the order Go shows in
practice and the order Rust writes.

Status: in force.
Retires when: does not retire.

**CP-R-34 (normalization): a minted token.** A web token coppice mints is
43 random characters of unpadded base64url; after `token     `, `#t=` or
`Bearer ` the driver writes it as `{MINTED}`. A push card's deny token,
64 random hex digits after `Bearer `, becomes `{DENY}`. The cases pin the
operator's token in `{DATA}/web/token` wherever a token is printed or
drawn, so the sign-in lines and the QR codes are compared whole.

Status: in force.
Retires when: does not retire. Tokens are random by design.

**CP-R-35 (normalization): values in a web reply that follow the clock or
randomness.** The event ring's `from` (the time its subscription was
answered) is masked with `#!mask from`. A body that holds a masked value
(a pid, a `quiet_for`, a `ts`) is as long as that value, so its
Content-Length is masked with it. A PEM file's length follows its random
key and signature (`mask_headers` on the CA hand-off). A view file on
disk carries the mtime of its copy in Last-Modified, which is masked;
Date is left out. Every other header is compared.

Status: in force.
Retires when: does not retire.

**CP-R-36 (ruling): three crates for TLS and the local CA.** The phone
server serves TLS by default, and `cert init` makes P-256 keys and ECDSA
signatures. Writing TLS by hand is the larger risk, so the crate uses the
versions and features `clients/rust` already locks:

- `rustls` =0.23.45 (Apache-2.0 OR ISC OR MIT; the rustls project), with
  `default-features = false` and `ring`, `std`, `tls12`: the TLS server,
  and the client for an https ntfy or voice server. 2.0 MB of source.
- `ring` =0.17.14 (Apache-2.0 AND ISC; Brian Smith's ring, from
  BoringSSL): P-256 key generation, ECDSA signing, SHA-256, and rustls's
  crypto. 8.2 MB of source, most of it pregenerated assembly; its C is
  built by the `cc` crate with the system compiler.
- `rustls-pki-types` =1.15.1 (MIT OR Apache-2.0; the rustls project):
  the key and certificate types. 272 KB.

They bring, all locked at clients/rust's versions: rustls-webpki 0.103.15
(ISC), untrusted 0.9.0 (ISC), subtle 2.6.1 (BSD-3-Clause), zeroize 1.9.0,
once_cell 1.21.4, getrandom 0.2.17, cfg-if 1.0.5, and for the build cc
1.4.3, shlex 2.0.1 and find-msvc-tools 0.1.11 (each MIT OR Apache-2.0),
about 1.8 MB of source more. The windows crates in the lock are never
built on Linux. The certificates' DER, SHA-1 for the websocket accept
key, base64, PEM and the QR code are written here, not taken from crates
(rcgen and x509-parser were looked at and not used: rcgen writes a
subject in a way that may not match a Go-made CA's bytes). The binary
grows from 7.1 MB to 9.6 MB, the page's 400 KB among it.

Status: in force.
Retires when: does not retire.

**CP-R-37 (ruling): text edges of certificates and keys.** Go reads a
certificate with crypto/x509 and a key with ParsePKCS8PrivateKey, and its
error words come from encoding/asn1 (`asn1: structure error: tags don't
match ...`). This port reads only what it needs (the subject, the key id,
the validity) and says `x509: malformed certificate` or `<file> is not an
ECDSA key` where Go says something longer, and it may read a
certificate Go's stricter parser refuses. A key or certificate that is
not PEM, the missing files, the partial CA and the key that does not
match the CA say the same words in both, and each is a case. Loading a
`--tls files` pair goes through rustls, whose words for a broken pair
are not Go's crypto/tls words; only a hand-made pair reaches this.

Status: in force.
Retires when: a case with a broken key shows a difference that matters,
which is then fixed.

**CP-R-38 (ruling): HTTP, TLS and websocket edges.** These differ, or
may, and no case reaches them, or they are timing:

- Go's TLS server also offers HTTP/2 (ALPN h2) to a browser; this port
  serves HTTP/1.1 only. Each side's cipher suites are its own library's;
  both take TLS 1.2 and 1.3.
- Go reads each request with a 10 s header timeout and no idle timeout;
  this port gives every read of a connection 10 s.
- This port sends 100 Continue before it routes a request that waits for
  one; Go sends it when the handler first reads the body, so a refused
  request gets none from Go and the connection closes.
- A request with several ranges gets the whole body here (200); Go sends
  multipart/byteranges with a random boundary. A header name or value
  that Go's httpguts refuses is read here; and an absent Host on
  HTTP/1.0, a CONNECT, and Go's other ReadRequest refusals are not all
  ported (a bad version, a missing or malformed Host, a header line with
  no colon, a bad Content-Length and an unsupported Transfer-Encoding
  are, with Go's words).
- A content type no table names is sniffed by a subset of Go's
  DetectContentType (HTML, XML, PDF, PNG, text, binary). The mime table
  is Go's builtins, then the system's globs2, as Go loads them, so a file
  extension types the same on one box.
- The websocket is read in 20 ms slices on one thread, where Go reads and
  writes on two goroutines, so a line may reach the browser up to 20 ms
  later. A close reason over 123 bytes, an unknown opcode's words and a
  close from the browser in the middle of a fragmented message are not
  cased.
- An https voice server or ntfy server is checked against the system's
  CA bundle by rustls, not Go's crypto/x509; a redirect from either is
  not followed here.

Status: in force.
Retires when: a case shows a difference, which is then fixed.

**CP-R-39 (ruling): what the web cases do not reach.** These paths are
ported but no case drives them, because each needs a value only one side
can see, minutes, or something a case must not do: a lock-screen deny
token used on the answer route and its retired 409 (the token is random
and lives only in the push card the fake ntfy got), an ask a harness
holds on the answer route and in a push card, a ban lifting after its
minute, the event ring's caps (10,000 events, two hours) and its retry
after it loses the server, ntfy and voice over https, a clip over 8 MiB,
the expiry warning within 14 days, `coppice web` opening a browser (every
case passes `--no-open` or has no display), the default listen addresses
`:8443` and `:8080` (no case listens on every address), and the real
`tailscale` (a fake answers). Rust unit tests cover the QR encoder, the
DER and the dates, the ranges, the key ids and base64, the flag parser's
usage text, the ban list, the token rules, and a CA that issues two
leaves and keeps its root.

Status: in force.
Retires when: a case reaches each path, with a clock or a timing both
servers would take as a flag.

**CP-R-40 (ruling): the phone screenshots and the e2e.** `testdata/phone`
holds five reference screenshots of the page at 360x780 that no Go or
JavaScript test reads. Both binaries serve the same page bytes, so a
screenshot can differ only through what the server sends, which the web
suite compares; the e2e check of a 390x844 page (the roster shows and
the page does not scroll sideways) runs against each binary.
`testdata/words.json` is held by a Rust unit test, as Go's push test
holds it. The e2e is run by hand against each binary with
`run.sh web`; `tui.sh` waits for slice 6b, and CI runs the e2e against
the Go tarball only.

Status: narrowed 2026-10-02 by slice 6b: `run.sh tui` passes against
each binary, and the coppice-rs CI job runs it against the Rust one
(CP-R-45). The web half is still run by hand against the Rust binary.
Retires when: CI runs `run.sh web` against the Rust binary too.

## 2026-10-02: stage I, coppice in Rust, slice 6b (the floor on a terminal), and the rename

Slice 6b ports `internal/tui`, the floor on a terminal: the rail grouped
by task and then by project, with fold headers and their counts; the
peek, with the ask's answer keys by tier and the trust screen's keys; the
prompt line and its words (list, read, close, open, swap, rotate, reset,
lock, unlock, layout, tree, zoom, foreman, a view id alone, and talk to
the foreman); the tree view and the task path; live windows beside the
rail on a wide screen, each attached with input rights at its own size,
the typing window, a headless agent's own input line, and the start
fill; the mouse (rows, windows, the close mark, drags, the ended line);
the rail keys from one table that also prints the footer; the Recent
fold and the ended line; the project picker; `floor.json`; the talk key
with the kitty keyboard protocol and voice clips; the one-question first
run before the alternate screen; and `coppice` with no arguments in the
CLI, with a hand-off to attach for one pane full screen. attach gains a
caller's key channel, the end-on-exit rule, and a screen's lines and
cursor; config gains the talk key. No crate is added. Go's code and tests
are not changed.

The floor is compared as a screen. A case file in `harness/coppice-rs/
cases/tui` starts a second scratch server for each side (the alt server)
and runs that side's binary as the floor in one of its panes, at a fixed
size, against the case's own server by the explicit socket and data dir,
so the floor's own pane is not on the roster it draws. Keys and words go
in with `coppice pane send-keys` and `send-text` on the alt server's pane,
named as in `testdata/keys.json`. A new local request, `screen`, reads the
pane at a checkpoint through the alt server's own libghostty-vt, the same
engine both ports link: a view-only attach on a connection with no hello,
and the first full frame, as plain rows and the cursor. Three files hold
the checkpoints: `rail.jsonl` (100 by 36: the roster, the peek on an ask
and on the trust screen, the answer keys, the hint, folding, rename, the
stop question, the words, `floor.json` compared as a tree, the tree view
and the task path, the picker, the ended line and Recent, `n` and ctrl-w),
`windows.jsonl` (200 by 50: two live windows, the start fill, the leave
key, typing into a pane, the number keys, the layout words, a headless
pi's own input line on the fake pi, zoom into attach and back, the mouse
on the close mark, a row and a drag, the wheel) and `firstrun.jsonl` (no
harness, two harnesses with the numbered question, one harness, and the
config file each writes). Each file was read on Go alone first, screen
by screen, before the Rust side was compared. Red was shown first: on the 6a
build 35 of the 230 tui cases agreed (the setup requests and the steps
that only send keys) and 195 disagreed, both tree views among them, and
`e2e/run.sh tui` failed from its first check (no roster). On the 6b build all 230 tui
cases, 3 event views and 2 tree views agree, and the whole compare, run
in parts, agrees on 1541 cases, 64 event views and 19 tree views, none
refused, no leak. `e2e/run.sh tui` passes all six checks against the Go
and the Rust binary.

CP-R-12 is retired: the Rust binary carries every command of Go's
command line, the floor on a terminal last, and is named `coppice`, as
Go's is. CP-R-40 is narrowed.

**CP-R-41 (normalization): the clocks on a floor screen.** A `screen`
result masks what follows the clock: a row's age (`now`, `3s`, `4m`,
`2h`) as `{AGE}`, an ended row's age before `ago`, and a held row's wait
after its foreman's name. An age takes as many cells as its digits, so the
padding after a masked age on a row that ends with the close mark is cut
to one space, and the tail of such a row that the narrow rail cuts short
(it ends with the ellipsis) is masked as `{CUT}`. A screen is read once it
holds the checkpoint's text and two frames 400 ms apart are the same. A
checkpoint after a change the 2 s poll draws (a new agent, a stopped one,
an ended one, the facts row's counts) waits 2.5 s first, and one after a
pane printed waits 6.5 s, so the process state's five-second window has
closed on both sides.

Status: in force.
Retires when: does not retire. Ages are clocks.

**CP-R-42 (ruling): Go's tui tests in Rust.** The Rust unit tests carry
the Go tests that need no server (the model, the rail, render, the peek
and its tiers, the trust screen, subagents, holds, looking, views and the
shared tree fixture, the tree, `testdata/words.json`, the talk key's
state, the facts row, the windows and the mouse), and a set of the Run
tests against a Rust server in the same process (the first frame, ctrl-c
and the end of the keys, the hint and the prompt, the peek, the cursor,
close, talk with no default harness, the stop question, `floor.json`
across runs, a server that goes away, the start fill on a wide screen).
The rest of Go's Run tests, which drive a floor against a live server
through a recording proxy (typing, the windows' attach and resize
requests, the headless line, the pause after an agent ends, the
foreman's promotion, voice clips through a fake recorder and a fake
voice server), are covered by the screen compare where a checkpoint can
see them; `testdata/keys.json` is read by no Go tui test, and the cases
send keys by its names. No case reaches: a voice clip (it needs a
recorder and a microphone), the kitty keyboard protocol and the device
query, a terminal resize, a double click into attach, a paste into a
headless window, the key pause after a typing agent ends, the task
foreman's promotion (it waits up to 30 s for a harness), and `open
HARNESS` and talk, which start harnesses. An agent that ends before the
floor's 2 s poll has seen it live draws no ended line in either port, so
`rail.jsonl`'s short agent lives past a poll.

Status: in force.
Retires when: a case reaches each path.

**CP-R-43 (ruling): text and timing edges of the floor.** These differ,
or may, and no case shows a difference: Go's first-run question reads its
answer through a buffered reader, which can take keys typed after the
line, where Rust reads byte by byte; the prompt line holds bytes, and a
rune not yet whole draws as U+FFFD in Rust where Go writes the raw byte;
`floor.json` field names are matched exactly, where Go's JSON matches
them in any case; the Rust floor waits for server lines, side work and
keys in 20 ms slices where Go selects on all of them, and its key reader
buffers keys where Go's channel holds none, so a paste is seen as one a
little sooner; a call's i/o error words are the Rust library's.

Status: in force.
Retires when: a case shows a difference that matters, which is then fixed.

**CP-R-44 (ruling, a fault in both ports): a click on a close mark.** A
left press on the close mark of a window header or a live row asks
whether to stop the agent, and the question takes the next key. A
terminal in mode 1000 sends a release after the press, and the floor reads
that release as the next key, so the question is answered no at once: on
a real terminal a click on the close mark only flashes the question. Go's
tests send the press alone. Both ports do the same, and
`windows.jsonl` pins it (`close-released`).

Status: retired 2026-10-02. While the stop question (or the clear-all
question) is open, a mouse release or a motion report does nothing; a
key or a new press answers it, as before. The fix is in the rail's key
handler of each port, before the question takes its key. A Go test and a
Rust test send the press, then the release, then motion, and the
question stays open until a new press. `windows.jsonl` now expects the
question still on screen after the release (`close-released`), and Esc
to close it (`close-kept-2`); the two pre-fix builds fail that case. A
wheel report is still a press and still answers the question no.

**CP-R-45 (ruling): the end-to-end suite and the release.** `e2e/run.sh
tui` passes against each binary, run by hand, and a step of the
coppice-rs CI job now runs it against the Rust binary (added with this
slice, not yet run on GitHub); the e2e job runs `run.sh` (web and tui)
against the Go tarball. `scripts/install.sh` and
`scripts/release.sh` build either coppice: `COPPICE_PORT=rust` puts the
Rust one in place of the Go one under the name `coppice`, and Go stays the
default. The Rust binary links libgcc_s as well as libc, so its release
check allows that one library more, and its tarball carries
`harness/coppice-rs/NOTICE`. The Cargo package keeps the name
`coppice-rs`, the directory CI builds in. The release script's Rust
branch was checked by a build with its path maps (no build-machine path,
libc and libgcc_s only, `--version` says the given version); no release
was made, and `COPPICE_PORT=rust scripts/install.sh` is checked for its
syntax only, since it installs into the owner's bin directory.

Status: in force.
Retires when: the owner makes the Rust coppice the default.

## 2026-10-01: the resident engine, Parakeet v2, engines by hardware

**O-1 (owner ruling): the voice server keeps the speech engine loaded
between clips.** Each external engine runs as one resident child (VO-19).

Status: in force.
Retires when: the owner rules otherwise.

**O-2 (owner ruling): Phonon-2 is excluded.** Its retraining used
SPGISpeech under Kensho's restrictive terms. No code, doc or summary offers
it. The research record of it (docs/research/stt-2026-10.md and
stt-2026-10-pass2.md, and VO-15) stays as history.

Status: in force.
Retires when: the owner rules otherwise.

**O-3 (owner ruling): the curated voice defaults.** VO-14 was the
provisional form of this ruling; it is now the owner's. It shapes defaults
only: no model is blocked, warned about or labelled.

Status: in force.
Retires when: the owner rules otherwise.

**O-4 (owner ruling): the engine may differ by hardware.** A phone, a
desktop CPU and a large GPU may each get their own engine (VO-17).

Status: in force.
Retires when: the owner rules otherwise.

**VO-16 (ruling): Parakeet v2 on parakeet-cli, the desktop default.**

- The engine. `voice_engine: parakeet` runs `parakeet-cli -m MODEL.gguf
  --resident`, this repo's own program (clients/native/parakeet-cli) on
  CrispASR's Parakeet code and the ggml CPU backend (VO-21). `voice_model`
  is `v2` (the default, and what `tiny.en` means here) or the path of a
  Parakeet-TDT GGUF file. A missing file says `Parakeet model file missing:
  PATH. Set voice_model to v2, or to a Parakeet-TDT GGUF file.`; a missing
  program names `native.sh --parakeet`. The model is English, so no
  language is passed.
- The model. NVIDIA Parakeet-TDT-0.6B v2 (CC-BY-4.0; the attribution is in
  NOTICE and in the module map). The recipe, run once on this box with
  nothing published: NVIDIA's `parakeet-tdt-0.6b-v2.nemo` from
  huggingface.co/nvidia/parakeet-tdt-0.6b-v2 (SHA-256
  d99e39955c9d3d0350d8fb7c75e40c64a2b2eaeb003883d7c941fd2e8747b28c, 2.47 GB,
  deleted after) went through CrispASR's own
  `models/convert-parakeet-to-gguf.py` at the pinned commit, run by
  `scripts/parakeet_convert.py`, which puts a GGUF writer of gguf-py's
  layout and a SentencePiece piece reader in place of the packages this
  project does not depend on. The F16 file it wrote has SHA-256
  c82b001dcb0adecd36f7401e4b77c7257eb352462368b89cf3ee13184206d7f7: byte
  for byte the file `cstr/parakeet-tdt-0.6b-v2-GGUF` publishes. Then
  `parakeet-quantize F16 OUT q4_k` (CrispASR's crispasr-quantize, built by
  `native.sh --parakeet`) made the Q4_K file, SHA-256
  764c4e6738b0b38c53bbfea040f9e07425d6d742df58b18906053085aea46b1c
  (396,937,984 bytes); two runs gave the same bytes. The Q4_K file cstr
  publishes is a different file and is not used.
- How a box gets it. Because the F16 file is the published one, the voice
  server fetches it (1,236,861,248 bytes) from
  `huggingface.co/cstr/parakeet-tdt-0.6b-v2-GGUF/resolve/main` and checks
  its pin, runs `parakeet-quantize` (found beside the file that
  `parakeet-cli` on PATH names), checks the Q4_K pin, and deletes the F16
  file. One line says so first: `Fetching the Parakeet v2 model (1237 MB)
  into DIR, then making its 397 MB Q4_K file. This happens once.` A
  mismatch fails closed: a fetched file that fails its pin is deleted; a
  Q4_K file that fails its pin is deleted with a sentence that names
  voice_model as the way out. All three sides took this path once on this
  box, from a loopback host that served the converted file, and each made
  the pinned Q4_K file. The URL names `main`, not a commit: only a short
  revision (8878172f) was recorded, so a changed upstream file would fail
  its pin, not load. The cases reach the fetch only through a closed port
  or a 404 (fake bytes cannot match the pins).
- The bench (docs/research/stt-2026-10.md, "Measured again with the
  resident engines"): resident Parakeet v2 Q4_K answers a 4 to 6 s clip in
  a median of 1.10 s (test-clean) and 1.04 s (test-other), p90 1.21 s and
  1.24 s, with WER 2.09% and 3.94%. The owner's bar for the desktop default
  is a median under about 1.2 s for a 5 s clip, so Parakeet is the
  desktop default (VO-17).

Status: in force.
Retires when: does not retire.

**VO-17 (ruling): the engine a box with no voice choice gets follows its
hardware.** `engines.choose_engine`, the same in Go and Rust. Inputs: total
RAM, CPUs online and GPU memory, from the probe `tiers setup` uses (Python's
`hardware`, with the nvidia-smi part split out so no torch import happens;
Go's and Rust's setup probe), and which engines are installed. Tiers:
Parakeet v2 with at least 8 GB of RAM and 4 cores (that leaves about 2 GB
for its model and runtime on a box that also runs agents); Moonshine small
with at least 2 GB; else the tiny tier, which is faster-whisper tiny.en
where its package imports and Moonshine small otherwise. whisper.cpp is
never chosen: this project fetches no whisper.cpp model, so it needs a
voice_model path. The first tier whose program is on PATH (or whose
package imports) wins; with none, the first tier, whose engine then says
what is missing. Total RAM, not free RAM, so a server never changes engine
from one start to the next. One line says which and why, for example `No
voice engine is set, so voice uses Parakeet v2: this box has 15.5 GB of
RAM and 4 cores.`, and, when a tier was skipped, `Parakeet v2 would come
first, but parakeet-cli is not on PATH.` GPU memory only appears in the
line (`and a GPU with 6 GB (no GPU engine is built yet)`): no GPU engine is
built, and Voxtral Realtime, the large-GPU option, is in the research note
only, untested. `OPENDAISUGI_VOICE_HARDWARE=RAM_GB,CPUS,VRAM_GB` replaces
the probe; every case suite sets it, so no fixture depends on the box.
`daisugi modules` marks the chosen engine active the same way. This
replaces VO-13's "faster-whisper where it imports, else Moonshine small".
14 `choose` cases, 6 `pick no config hardware` cases.

Status: in force.
Retires when: a GPU engine is built, or a phone build picks its own.

**VO-18 (ruling): whisper.cpp stays one process per clip.** O-1 asks for a
resident child for every external engine, and for a wrapper over the C API
where an engine has no resident mode. whisper.cpp's own resident mode is
its HTTP server, not this protocol, and a wrapper needs whisper.cpp's
sources, which were not on this task's list of downloads. A wrapper built
from the copy inside CrispASR's tree would blur the provenance VO-21 rests
on. So `whisper-cli` runs as before (VO-0). whisper.cpp is never the
engine a box gets without a choice (VO-17).

Status: in force.
Retires when: a resident wrapper over whisper.cpp's C API is built from
ggml-org/whisper.cpp at a pinned commit.

**VO-19 (ruling): the resident protocol and lifecycle.** Defined once, in
`src/opendaisugi/voice/resident.py`; `clients/native/common/daisugi-native.h`
is the child side; Go (`internal/voice/resident.go`) and Rust
(`src/voice/resident.rs`) follow it. The child writes `{"ready":
"daisugi-voice-1"}` once its model is loaded, or `{"error": WHY}` and
exits 3. A request is a 4-byte big-endian length (1 to 64 MiB) and a 16 kHz
mono 16-bit WAV; a reply is one JSON line with a string `text` or `error`;
end of file means exit 0. In resident mode the child points fd 1 at stderr
and keeps the protocol on its own stream, and asks for SIGTERM when its
parent dies. The server starts the child and waits for it before the bind
(a failed start is exit 3 with `PROGRAM did not start: WHY`), sends one
clip at a time, restarts at once a child that dies after it was ready,
answers a clip that comes while a child loads with 503 `engine_loading`,
answers a child lost inside a clip (an end, a reply it cannot read, no
reply in 120 s, the write of the frame included, so a child that stops
reading its stdin cannot hold the clip lock) with 503 `engine_unavailable` and starts a new one, starts
a child that failed to load again only when the next clip comes (so a
broken engine never loops), passes an `error` reply on as the generic 500,
and on stop closes the child's stdin, waits 2 s and kills it. Cases: a fake
engine (`clients/fake_resident.py`) driven by a spec per start and per
clip; 17 `resident` probe cases (one load and many clips, a crash and the
restart, malformed replies, an error reply, a slow load within and past
the timeout, a load error, an exit before ready, a wrong ready line, clips
refused while it loads, a failed restart, a clip timeout, a child that
stops reading its stdin, odd text) and 16
`resident line` cases; 7 parakeet server cases. A server step may be sent
again while it answers `engine_loading`, and only its last answer is kept,
so a restart's timing never reaches a fixture; every case also counts the
fake's runs still alive once the server has stopped (always 0). 555 voice
cases (371 probe, 134 cli, 50 server), from 486. Red was shown on the Go
and Rust binaries built before this change: on each, 106 cases disagreed
and 1 was classed fail-open (`serve moonshine fails is 500`: the old
binary ran the new resident fake once per clip). For Go the red came
after the cases and before any Go code; for Rust before any Rust code.
Then all 554 agreed on both, and two full oracle runs wrote the same
bytes. The write-under-the-timeout case came last: the Rust binary built
before that fix disagreed on it (it waited out the fake's 30 s stall);
with the fix Go and Rust agree on all 555.

Status: in force.
Retires when: does not retire.

**VO-20 (ruling): Moonshine rests between clips.** Moonshine keeps one
Silero voice detector per process and never resets its state, so a
resident child heard each clip through the end of the clip before: on
LibriSpeech, resident Moonshine small scored 6.21% and 11.21% WER against
4.85% and 10.39% one process per clip, and 47 of 100 test-clean
transcripts changed (one gained a "Yeah" that was never said). Before each
clip, moonshine-cli now runs one second of silence through the same call
and drops its transcript. With that, WER is 4.81% and 10.70%, within the
sample's noise of the one-process figures. The C API offers no reset; the
silence is the smallest change that brought the numbers back.

Status: in force.
Retires when: Moonshine resets its detector between calls, or offers a
reset in its C API.

**VO-21 (ruling): the Parakeet-only CrispASR build.** `native.sh
--parakeet` checks out CrispASR (github.com/CrispStrobe/CrispASR) at
7e2b030780df5c9ad7ede6ac1935bc783b037d18 with only the 32 files of
`clients/native/parakeet-cli/crispasr-files.sha256` (the five sources, the
headers they include, the quantizer's three files, the converter and the
license), checks each file's SHA-256, and refuses any other file on disk.
It checks out CrispStrobe/ggml at 2f5a80d258c46e6ac8eee95f1328c0f58376d7ee,
the commit CrispASR's submodule pins, with only the top files, cmake/,
include/, the top of src/ and the CPU backend, without its LoongArch and
SpacemiT code (those files build only for those CPUs), and checks the tree against one SHA-256 of its sorted sha256sum
listing (aa6369fd...). That digest was taken here on the first checkout:
trust on first use, against the git commit. Both programs are built with
zig 0.16 for the baseline CPU with `-ffp-contract=off`; on x86_64 with
AVX2, FMA, F16C and BMI2, the ggml CPU backend alone also gets those
(the prefix stamp names which). The quantizer reaches the quantization
through `ggml_quantize_chunk`, which is in ggml-base (`quantize_q4_K` in
`ggml-quants.c`), compiled for the baseline CPU in every build; the ISA
flags reach only the CPU backend. So the same bytes are expected on every
x86_64 box. `check-binary.sh` must pass on both before
anything is installed: no string or symbol of a left-out model, Xiaomi's
tables, pinyin, Kaldi or CrispASR's download cache; no URL; no socket,
DNS, curl or TLS call; and `ldd` names only the C runtime. The quantizer
is checked with `--rules`, which skips the model-name strings and only
that: its per-model rules name other models' tensors as plain strings.
`ldd` of parakeet-cli names libc and libm only. The quantizer's output was
the same on two runs here and on the three end-to-end runs; it was not
checked on another box or on aarch64, where a different result fails the
Q4_K pin closed, and the way out is a voice_model path to a GGUF file.

Status: in force.
Retires when: does not retire.

## 2026-10-02: stage J, sprig in Rust

`harness/sprig-rs` is the Rust sprig: the four binaries the Go module
builds (`sprig`, `sprig-hook`, `sprig-mcp`, `weave`), with the loop, the
four tools, the executor and the process gate, the session tree and its
resume, the claude and API backends, the command line, the hook, the MCP
server and the workflow runner. It changes no behavior: it is checked
against the Go sprig, not the Python oracle, since sprig has no Python
side.

`clients/sprig_compare.py --go DIR --rust DIR` runs each case of
`clients/sprig_cases.py` on both sides, each in a scratch root of its own
with `PATH` set to the side's fakes and `/usr/bin:/bin`, and no other
variable but the case's own. The fakes: a `claude` that answers from the
case's script and records argv, cwd and stdin; a gate that rules by the
case's rules and records each payload; a local HTTP server for the API
that records each request. Compared per case: the exit code, stdout,
stderr, every fake call and request, and the files of the work dir and the
session dir after the run. The real-gate cases run only with
`--daisugi`, once per daisugi binary given, after `daisugi gate init
--workspace WORK` in the side's HOME.

Red was shown first, on the empty crate whose four binaries said "not
ported" (commit `0702fa0f`): all 177 cases refused. Go against Go agreed
on all 177 before any Rust code, which shows the masking below is whole.
Then all 177 agreed, and with the https and raw-byte cases added, all 185
(173 through fakes, 12 through the real Go and Rust daisugi), none
refused. Go's own tests were not changed and pass. No fault was found in
Go.

**SP-R-1 (normalization): per-process values.** Each side's root becomes
`{ROOT}`, the fake API's port `{PORT}` and a closed port `{DEAD}`. The
values of `id`, `parentId`, `toolUseId`, `leafId` and `session_id`, and a
minted session id in stderr and in a session file's name, are renamed by
first appearance (ID1, ID2, ...), so a broken parentId link still shows.
`ts` and `latencyMs` are masked. The API's `X-Opencode-Session` header
keeps only its shape, `sprig-` and 16 hex digits. Both sides are built
with one version string, so `--version` compares. Every other byte is
compared.

Status: in force.
Retires when: does not retire.

**SP-R-2 (ruling): the HTTP client past what sprig sets.** The Rust
client sends Host, User-Agent (Go's text), Content-Length and sprig's
four headers, and no `Accept-Encoding: gzip`, so no reply comes gzipped
for it to undo; the compare records the path, the body and sprig's four
headers. Not followed, and not compared: `HTTP_PROXY`, `HTTPS_PROXY` and
`NO_PROXY`; redirects (Go follows a 307 or 308, Rust reports the 3xx as
an `anthropic api` error); the words of a DNS failure, a TLS failure and
a URL that Go's url.Parse refuses for any reason but a missing scheme or
a bad port; a header value Go refuses. The compare covers http, https
through a scratch CA, chunked replies and a refused connection.

Status: in force.
Retires when: a case needs one of these, or the API backend leaves its
on-hold state.

**SP-R-3 (ruling): ported, not run in the compare.** The claude backend's
120 s timeout (both say `context deadline exceeded`); a session file whose
parentId links form a loop (both walk it without end, as Go does); a
session-file line of 16 MiB or more (both stop with `bufio.Scanner: token
too long`); a write to a closed stdout or stderr (both die of SIGPIPE).
`--gate-timeout` above 9223372036 seconds differs: Go's Duration
overflows and the gate times out at once, Rust waits that long. No
caller passes such a value.

Status: in force.
Retires when: does not retire.

**SP-R-4 (ruling): the crates.** rustls 0.23.45 (Apache-2.0 OR ISC OR
MIT, the rustls project, 2.0 MB of source), ring 0.17.14 (Apache-2.0 AND
ISC, from BoringSSL, 8.2 MB, its C and assembly built by its build
script) and rustls-pki-types 1.15.1 (MIT OR Apache-2.0), for https in the
API backend. With them come rustls-webpki 0.103.15 (ISC), untrusted 0.9.0
(ISC), subtle 2.6.1 (BSD-3-Clause), zeroize 1.9.0, getrandom 0.2.17,
once_cell 1.21.4, cfg-if 1.0.5 and libc 0.2.189 (MIT OR Apache-2.0), and
at build time only cc 1.4.3, shlex 2.0.1 and find-msvc-tools 0.1.11.
Each is the version and feature set `harness/coppice-rs/Cargo.lock` and
`clients/rust` already lock; no new crate came from the network. There
is no JSON crate: serde_json refuses a lone surrogate escape and bytes
that are not UTF-8, which Go turns into U+FFFD, and does not match keys
case-folded, so `src/json.rs` follows encoding/json by hand: its scanner
and error words, case-folded keys, the last duplicate key, slice reuse,
type errors with Go's field path, and its encoder's escapes and float
format. `NOTICE` and `PINS.md` list them.

Status: in force.
Retires when: does not retire.

**SP-R-5 (ruling): Go's own texts.** The errno texts, the signal names and
the code points `strconv.IsPrint` calls not printable were printed by Go
1.25.12 on linux/amd64 and are tables in `src/goerr.rs`; the JSON error
words and the flag package's words were taken from that Go too. The
compare ran here against Go 1.25.12. The CI job builds the Go sprig with
`GO_VERSION` 1.26.8, against `go 1.25.12` in go.mod; if a newer Go words
any of these differently, the compare there shows it. The job is added,
not yet run on GitHub.

Status: in force.
Retires when: the job has run green on GitHub.

**SP-R-6 (ruling): the version without a stamp.** Both install scripts
stamp the version (`-X main.version` for Go, `SPRIG_VERSION` for Rust).
Without one, Go reports the VCS revision its build records, cut to 12
characters with `-dirty`, and the Rust build script runs `git rev-parse
HEAD` and `git status --porcelain` for the same; the two may disagree on
what counts as dirty. Not compared.

Status: in force.
Retires when: does not retire.


## 2026-10-02: full parity, the model-asking checks and named definitions

The inventory of what a Go or Rust user still needed Python for is in
`docs/plans/2026-09-24-omarchy/plan-part2-ports.md` ("Full parity").
New cases: 32 k2 (`run llm_check *`, `run alias *`, `run postcondition
expr *`, `orch llm_check *`, `orch alias *`; two cases the ports refused
before now answered), 10 k3 (`mcp step llm_check
*`, `mcp step alias`, `mcp step expr not a dict`, `mcp verify llm_check
invariant`, `mcp verify alias invariant`), 3 pathway (`import llm_check
*`) and 5 gate cases (`llm claude *`). Red first: the binaries from before
this work refused or disagreed on every one of them (Go k2 16 disagree and
16 refused of 148; Rust 11 disagree and 15 refused of the 26 `llm_check`
k2 cases; Go 6 of 6 `mcp step llm_check` k3 cases refused; Rust 3 of 3
import cases refused; the Go gate denied all 5 `llm claude` cases
undecided). After: Go and Rust each agree on 142 k2 cases (6 not ported,
as ruled), 299 k3 cases (3 not ported and 1 refused, K3-13), all 161
pathway CLI cases and all 2,099 gate cases.

**PG-1 (ruling): `llm_check` in every verify.** The oracle's evaluator asks
the model for an `llm_check` wherever `verify` or
`stage2.verify_completed_step` runs: the gate, `run`, `orchestrate`,
`weave`, the MCP tools, `pathways import`, ingest and tend. Each port now
has one copy of `llm_check.run_llm_check`, which both the gate and the
commands call (Go `internal/llmcheck`, Rust `gate::llm`), on both
backends. Every command's verify goes through it, since the verifier is
shared; the asking is cased in the k2, k3, pathway and gate suites, not in
the f and garden suites (no ingest or tend case holds an `llm_check`).
The two backends:

- claude-code: `claude -p --model=haiku`, then `DAISUGI_CLAUDE_ARGS`, the
  prompt on stdin, in the neutral working directory, a 60 s limit, and the
  first `{` to the last `}` of stdout read as JSON or as a Python dict
  literal. A failed run is a plain "not satisfied" (the oracle catches
  `EnvelopeGenerationError` in `_invoke_model`), so the item is
  "violated", never an evaluation error.
- the HTTP backend: LLM-12 as before. Any failure is an errored result,
  and the item is an evaluation error whose text holds `error: llm_check
  call failed:`.

The payload is `json.dumps({"task": plan.task, "steps": [...]})` with each
step's keys in the order the binary read them; the fake model answers only
exact bytes, so every case checks it. The gate's claude-code branch, until
now denied undecided, runs the same code (5 gate cases). A call the port
does not make the oracle's way (`SSL_CERT_FILE`, `SSL_CERT_DIR`, a proxy
it does not read, a reply nested past 900) is undecided: the gate denies
it, a Rust command refuses it, and a Go command records an evaluation
error ending "is not in this binary yet" that a caller which stores the
violation refuses. The predicate stage's violations now carry the
oracle's detail (`{label: type}`, with `description` for "violated", and
the unresolved-alias detail), so `run` journals them instead of refusing.

Status: in force.
Retires when: does not retire.

**PG-2 (ruling): named definitions at every command.** No command of the
oracle builds an alias registry: only the library function
`integrations.hermes.envelope_from_yaml` passes one to `verify`. So at
every command an `alias` is unresolved, and the ports now say so in the
oracle's words: a top-level alias is the "references unresolved alias
'NAME'; pass an AliasRegistry via aliases= to verify()" violation with its
detail, and a nested one the evaluation error "unresolved alias reference
'NAME'; resolve aliases before evaluation" (the Go port quoted the name
with double quotes before). At stage 2, and in the MCP stage-2 tool, an
alias is that evaluation error. Cases: `run alias *`, `orch alias
postcondition`, `mcp step alias`, `mcp verify alias invariant`.

Status: in force.
Retires when: does not retire.

**PG-3 (ruling): agentic steps in `weave`, in the ports.** Go
(`supervise.Agentic`) and Rust (`supervise::agentic`) port
`AgenticExecutor` and `weave` wires it, as the oracle does (WV-R-9): the
workspace must be a directory; the child envelope (the caller's own when
the step names none) is proved inside the caller's with `edge_ok` and a
2,000 ms budget, and a refusal fails the step with "the child envelope is
refused: " and the reasons; the tool wall is the step's tools whose
capability the child grants; the child, with `parent_envelope` set, is
registered 0600 in `envelopes/agentic-<safe step id>.json` of a fresh 0700
gate root made by `mkdtemp` in `tempfile.gettempdir()`, outside the
workspace, and kept; the sub-agent runs as `claude -p --model=haiku`,
`DAISUGI_CLAUDE_ARGS`, `--output-format json --settings <gate settings>
--allowedTools <wall> [--max-turns N]`, in the workspace, the prompt on
stdin. The settings are `gate_settings_json` in enforce mode with
`--captures-root <root>/captures` and `--session agentic-<id>`; the hook
command runs the binary's own `gate check` where the oracle's runs
`python -m opendaisugi.gate_client`, as the hook `install --gate` writes.
Each failure is a failed step with the oracle's text: no workspace, no
backed tool, `claude -p` failed, output that is not JSON, `is_error`. A
reply that is JSON but no object, or a token count `int()` refuses,
raises out of the executor as in the oracle. `run` and `orchestrate` still
wire none (K2-7). Since 2026-10-09 the same executor has a sprig runtime
(`--agent sprig`, SX-R-2) and `run` and `orchestrate` wire it (SX-R-1). Cases: 11 new weave cases (`agentic *`). Red first: the
binaries from before refused all 12 agentic weave cases. After: Go and
Rust each agree on 119 of the 120 weave cases (1 refused, WV-R-10).

Status: in force.
Retires when: does not retire.

**PG-4 (harness): the sub-agent in the weave cases.** The weave suite's
fake `claude` keeps the garden fake's behavior for a call without
`--settings`. With `--settings` it is an agentic sub-agent: its key leaves
out the settings value (a fresh gate root and the gate's own program
differ run to run and side to side); its log entry holds the settings
with the program before ` --mode ` written as `<GATE>` and the root as
`{GATEROOT}`, the root's mode and the working directory; it runs each tool
call of its answer through the settings' PreToolUse command as the host
runs a hook, and logs each verdict (exit, stdout, stderr); and it logs the
envelopes registered in the gate root, with their modes and text. So a
case compares the inner wall itself: in `weave agentic runs` the gate
allows the read inside the workspace and `cat`, and denies `/etc/passwd`
and `rm`, the same way on all three sides.

Status: in force.
Retires when: does not retire.

**PG-5 (ruling): the alias registry, on a probe.** `aliases.AliasRegistry`
and the seven system aliases are ported (Go `internal/aliases`, Rust
`aliases`): registration with the system-name rule, the path check (a
`children` that is no list iterates as Python iterates it) and the Z3
vacuity check (a body that names an alias, or that parses only once a
typed placeholder is bound, is deferred; any other exception refuses the
register with its text), lookup by tier, and resolve with one-pass
substitution, longest name first, a regex field's text escaped as
`re.escape` escapes it, and the oracle's errors (`UnknownAliasError` as
the name's repr, `AliasCycleError`, missing arguments, a substituted body
pydantic refuses). No command builds a registry, here or in the oracle
(PG-2), so `alias-probe` measures it: 23 cases
(`clients/alias_cases.py`), all agreeing in Go and in Rust. The first Go
run disagreed on one: the vacuity compiler named an op it cannot compile
by its tag where the oracle names the class (`DependsOn`); both ports
now name the class, and quote an unresolved alias with the oracle's
single quotes. `integrations.hermes.envelope_from_yaml` and
`load_household_aliases`, which read YAML files and build envelopes, are
not ported: no binary reads an alias file.

Status: in force.
Retires when: a command of the oracle builds a registry (the ports then
wire this one).

**PG-6 (ruling): `daisugi verify` and the command-line `hook report`.**
Both ports carry them. `verify PLAN --envelope ENVELOPE [--json]` checks
its two paths as typer's `Path(exists=True, dir_okay=False, readable=True)`
does ("File 'p' does not exist.", "is a directory.", "is not readable."),
reads the plan and then the envelope as `yaml.safe_load` and the models
read them (YAML outside what the readers take is refused, K2-3), and
verifies leniently, as `verify(plan, envelope)` does: a Z3 check that does
not finish is a warning, not the violation `run` makes of it (K2-2). It
prints the oracle's lines, or `VerificationResult.model_dump(mode="json")`
with `--json`, and exits 0 or 1. `hook report [--pane P] [--root R]` reads
one event from stdin and runs the same parse, downgrade, check, session
tree append and delivery as the resident server's `hook report`, with the
pane identity read from the process's own environment, as the oracle's
command line reads it. The cases put the gate root's parent on a file, so
the best-effort tree append writes nothing; the tree is cased through
gate.sock in the gate_server suite. Cases: 13 `verify *` and 15 `hook
report *` CLI cases; red first, both binaries answered every one as not
in the binary. After: Go and Rust each agree on 591 of the 609 CLI cases,
with the same 9 not ported and 9 refused as before.

Status: in force.
Retires when: does not retire.

## 2026-10-02: the robotics executors in Go and Rust

**RB-R-1 (ruling): no command builds a robot executor; each port has a
probe.** The oracle's `run`, `orchestrate` and `weave` build the default
executors (shell, file_read, file_write, network, and task and agentic in
weave), so a robot step fails there with "no executor for kind
'joint_move'"; only a library caller hands `robotics_executors` or a VLA
executor to `Supervisor`. The ports do the same: `daisugi` still fails a
robot step that way, and the executors are a library part
(`internal/robotics`, `src/robotics`) that a Go or Rust caller hands to its
supervisor. The cases drive the library, as the alias cases do (PG-2), and
each port answers them with a `robot-probe` that is not shipped. Red first:
a probe that ran the cases through the default executors answered 77 of
the 79 cases wrongly (the 2 left were render error cases, which then had
no oracle). After: Go and Rust each agree on all 79.

Status: in force.
Retires when: a command of the oracle builds a robot executor (the ports
then wire theirs there).

**RB-R-2 (ruling): MuJoCo is pinned, linked dynamically, and wrapped by us.**
`clients/go/scripts/native.sh --mujoco` installs the official 3.12.0
release for Linux x86_64 (or aarch64), checked against the SHA-256 GitHub
publishes for the asset (a9367911... for x86_64), into a prefix of its own;
the Z3 prefix is never rebuilt for it. 3.12.0 is the version `uv.lock` pins
for the oracle's wheel, and the wheel's `libmujoco.so.3.12.0` is the same
file as the release's, byte for byte (sha256 bd3f702a...), so every side
runs the same library build. The release holds only a shared library, so
the builds with the `mujoco` build tag (Go) or Cargo feature (Rust) link it
dynamically, with no rpath: the loader finds it through `LD_LIBRARY_PATH`.
The default builds and the shipped binaries never link MuJoCo. The wrappers
are ours: cgo in Go, hand-written FFI in Rust, and one small C layer both
compile (`clients/native/mujoco/dmj.c`: loading with the parser's message,
flat views of the model and data fields, the contacts, and rendering), so
neither port needs generated bindings and both issue the same calls. To
write the expectations, the oracle runs with the mujoco wheel fetched by the
SHA-256 in `uv.lock`, unpacked on `PYTHONPATH` with `MUJOCO_GL=disable`;
the project's virtual environment is not changed, and CI does not need the
wheel.

Status: in force.
Retires when: the oracle moves to another MuJoCo (both pins move together).

**RB-R-3 (ruling): floats are compared exactly, the IK included.** Physics
is the same library on every side, so qpos, qvel, ctrl, body and site
positions and every float in a stdout are compared to the last bit, with
no tolerance. The IK's damped least-squares update goes through numpy in
the oracle, so the ports compute it in numpy's order: each entry of
J J^T as a chain of fused multiply-adds from zero (what OpenBLAS's kernels
do on a CPU with FMA), the 3 x 3 solve as OpenBLAS's left-looking LU
(getf2) with column-oriented triangular sweeps, and J^T x and the norm as
plain sums in order. Checked against numpy on 3,000 random systems (the
solve) and for 1 to 6 joints (J J^T and J^T x), then on every IK case. The
expectations were written on a CPU with FMA; an oracle run on a CPU without
it, or with another OpenBLAS kernel, may differ in the last bit and show the
cartesian cases as stale. Then this ruling is reopened, not loosened.

Status: in force.
Retires when: does not retire.

**RB-R-4 (ruling): rendering is compared between the ports.** No in-scope
oracle path renders: only `TransformersVLAExecutor._capture_image` does,
and it is the next job. The oracle's own renderer also needs PyOpenGL,
which is not installed here, and fetching it would be more than the pinned
MuJoCo download. So the 5 render cases carry no expectation: every probe
must draw the same RGB bytes (the SHA-256 of the image, no pixel
tolerance), an image of one color fails, and the two error cases (an image
larger than the offscreen buffer, a camera that does not exist) must give
the same error. The image is top row first, as the oracle's `Renderer`
returns it, drawn with its defaults (a 10,000-geom scene, font scale 150,
all categories). The context is EGL with no display: the first EGL device
that initializes, as MuJoCo's Python EGL context picks it, then the default
display, as `sample/record.cc` does. OSMesa is not built as a fallback:
this box has no libOSMesa to build or test it against, so a box with no
working EGL gets the renderer's error, and the other cases proceed. Both
ports drew the same bytes on this box (Mesa's EGL); CI installs Mesa's EGL.

Status: in force.
Retires when: an in-scope oracle path renders (the cases then get an
expectation, and the tolerance is ruled then).

**RB-R-5 (ruling): receipts are compared without their timing.** A
receipt's evidence holds `duration_ms`, and its `evidence_hash` covers it,
so neither can match across runs. The compare checks each receipt's hash
against the oracle's `compute_evidence_hash` of that receipt's own
evidence, then drops `duration_ms` and the hash, and compares the rest.
Run ids, trace ids, times and durations are dropped everywhere.

Status: in force.
Retires when: does not retire.

**RB-R-6 (ruling): the dish-wash kit becomes joint moves.** The kit
(`examples/dish-wash`) registers its own step types and wraps
`MuJoCoExecutor` in user code, which no port reads. The cases run what its
executor runs: each domain step as the `joint_move` it makes, with the
kit's targets (the scrub target's sine computed by the oracle when the case
is written) and its `settle_steps=200`, under a supervisor, for one and two
plates.

Status: in force.
Retires when: the ports read registered custom step types.

**RB-R-7 (ruling): the robotics violations carry their detail.** The
verifier's four robotics checks (workspace, joint limits, velocity,
obstacles) named their violations without the oracle's detail dict, so
`run`, `orchestrate` and a supervised probe refused a plan they rejected
("a violation whose detail this binary does not write yet") where the
oracle journals the rejection. Both ports now write the oracle's detail:
the invariant, the step, and the joint, target, range, bounds, velocities
or sample point. Red first in a unit test in each port; the case `run
rejected joint limit` now agrees.

Status: in force.
Retires when: does not retire.

## 2026-10-03: SmolVLA inference in Go and Rust

**VL-R-1 (ruling): the model and its backbone are Apache-2.0; the graphs
are 1.6 GB.** The model cards in the pinned snapshots say `license:
apache-2.0` for `lerobot/smolvla_base` and for its backbone
`HuggingFaceTB/SmolVLM2-500M-Video-Instruct`. The checkpoint's
`model.safetensors` is 865 MB (the VLM is stored in bfloat16). The three
FP32 graphs are 393 MB (vision), 796 MB (prefix) and 405 MB (denoise).
The project's virtual environment has torch and transformers but not
lerobot, and lerobot pins other versions of both, so the export runs in a
separate virtual environment (VL-R-2). ONNX Runtime is MIT.

Status: in force.
Retires when: the pinned revision changes (check the cards again then).

**VL-R-2 (ruling): the export is pinned and can be rebuilt.** Model
`lerobot/smolvla_base` at revision
`d9f33c94a60fb382c90dea2164c96845bd955e28`, backbone tokenizer from
`HuggingFaceTB/SmolVLM2-500M-Video-Instruct` at
`7b375e1b73b11138ff12fe22c8f2822d8fe03467`. The export environment is
`clients/vla/requirements.txt` (lerobot 0.6.1, torch 2.11.0 CPU, onnx
1.19.1, onnxruntime 1.23.2, mujoco 3.12.0, PyOpenGL 3.1.10, and every
dependency with its sha256), Python 3.12, opset 17. The recipe:

    uv venv VENV --python 3.12
    uv pip install --python VENV/bin/python --require-hashes \
      --index-strategy unsafe-best-match \
      --extra-index-url https://download.pytorch.org/whl/cpu \
      -r clients/vla/requirements.txt
    # fetch both repositories at the revisions above into HF_HOME, then
    for p in vision prefix denoise check assets; do
      CUDA_VISIBLE_DEVICES="" HF_HUB_OFFLINE=1 \
        VENV/bin/python clients/vla_export.py --part $p --out DIR
    done

`--part assets` copies the backbone's `tokenizer.json` (sha256
5ece781dc8d2b2f3e2f289ca0ae50b17cfc27dd27bfe7971bb8241e0b964331a) and the
checkpoint's normalizer stats as `stats.safetensors` (sha256
490ab239d96e263687c0b2e386a0afbc235a2eceb9857c36ed32f2f162a3e7c8) beside
the graphs. The executors check all five files against these pins before
they load any.

Each part runs in its own process under the 4 GB cap (peak 2.4 GB). The
graphs and their sha256:

- `vision.onnx` eae4a86506b2f8e36f4b3efa16133964feadbc6323ecbb14c61838e080b06664
- `prefix.onnx` ac391d4633dce409f050929adbe77c7a6af86dddfa0266aa4937e98b13ef3579
- `denoise.onnx` f2412f7254659dcb9cf0d00dc181f05cca1299609ab1ec25042fe400ff4bda66

They are kept in `~/.cache/opendaisugi/models/smolvla-onnx/`, never in
the repo. A second export of `vision.onnx` gave the same bytes. Two
earlier graphs from the same session (prefix a5f378e5..., denoise
3c3d4689...) are void: they kept bfloat16 weights, for which the CPU
provider has no kernels, and the prefix held a CumSum over a boolean
input, which ONNX does not allow. Neither loaded in ONNX Runtime.

`--part check` runs the three graphs with all three sessions open (peak
2.4 GB) and the Euler loop in numpy, against the PyTorch policy in FP32
on one fixed input and noise: largest absolute difference 1.8e-6 over
the 50x32 chunk (largest relative difference 3.3e-3, at values near
zero), `allclose(atol=1e-3, rtol=1e-3)` true. One chunk took 2.65 s in
ONNX Runtime (4 threads) and 2.57 s in PyTorch on this CPU.

The oracle is the FP32 PyTorch policy (the checkpoint's bfloat16 VLM
weights cast to FP32, which is exact), run by lerobot in the export
environment. The project's own `TransformersVLAExecutor` cannot load this
model: in the project's environment, transformers' `AutoProcessor` raises
`ValueError: Unrecognized processing class` and `AutoModel` raises
`ValueError: Unrecognized model ... Should have a model_type key in its
config.json` on the pinned snapshot. Stock lerobot runs the VLM in
bfloat16; that gap is not the basis of any tolerance.

Status: in force.
Retires when: the pinned revision or a pinned package changes (export
again and pin the new sha256 values).

**VL-R-3 (ruling): what stays outside the graphs.** The graphs hold the
networks only. The caller does the rest, as lerobot's processors and
`sample_actions` do:

- the task text gets a trailing newline if it has none, then the
  backbone's tokenizer gives at most 48 ids, padded on the right, with a
  mask of 1 for each real token;
- the camera image (RGB, top row first) is scaled to [0,1], resized
  bilinearly (no antialias, `align_corners=False`) to fit 512x512 with
  its aspect kept, and padded with 0 on the left and top; the graph maps
  [0,1] to [-1,1];
- the state is normalized with the checkpoint's mean and std
  (`(x - mean) / (std + 1e-8)`) and padded with zeros to 32;
- the prefix mask is 64 ones (image), the 48 token mask values, and one
  one (state);
- the 10 Euler steps: `dt = -0.1`, `time = 1 + step * dt` in float32,
  `x = x + dt * v`, starting from the caller's noise [1,50,32];
- the sine-cosine time embedding [1,720] (period 0.004 to 4.0), computed
  in float64 and cast to float32. lerobot computes it in float64, and
  the CPU provider has no float64 Cos;
- the action is the first 6 of the 32 dimensions, unnormalized with the
  checkpoint's mean and std (`x * std + mean`, with no epsilon).

The stats come from the checkpoint's normalizer file (the pre and post
processors name the same file, kept beside the graphs as
`stats.safetensors`). lerobot looks a feature up by its key
(`observation.state.mean`, `action.std`, ...) and leaves the value as it
is when the key is missing. In `smolvla_base` the file holds only
per-dataset action stats (`so100.buffer.action.mean` and the like), so
both steps leave the values unchanged. The ports do the same lookup, so a
fine-tuned checkpoint with real stats would be normalized.

The vision graph takes its patch positions as a constant. This is exact
only for a full 512x512 image with no padding mask, which the caller
always gives (padding is pixels, not a mask).

Status: in force.
Retires when: a newer export moves a step into a graph.

**VL-R-4 (ruling): three graphs, and FP32 only where a graph traces.** In
one graph with the vision encoder, the prefix export went over the 4 GB
cap once its weights were FP32. So the vision encoder and connector are
a graph of their own (`vision.onnx`, image to embeddings [1,64,960]), and
each part casts to FP32 only the weights its graph uses; the others stay
in bfloat16 and never reach the graph. Constant folding is off in the
export (ONNX Runtime folds when it loads the graph). The denoise export
uses zeros for the key and value cache: the shapes are static and the
cache is an input.

Status: in force.
Retires when: the export runs with a higher memory budget.

**VL-R-5 (ruling): the tokenizer is the oracle's, not the file's
pipeline.** lerobot loads the backbone's tokenizer with transformers'
`AutoTokenizer`, which gives a `GPT2Tokenizer` (the class named in
`tokenizer_config.json`). transformers 5 builds that class's own pipeline:
the ByteLevel pre-tokenizer, no normalizer, and no special tokens added.
It takes the vocabulary, the merges and the added tokens from
`tokenizer.json`, but not the file's pre-tokenizer, which runs a Digits
step before ByteLevel. The two disagree only where a digit split moves a
space: `"Ⅻ ½ ٣"` is `[173, 223, 121, 3351, 138, 16936, 113]` in the
oracle and `[173, 223, 121, 216, 16738, 216, 164, 113]` with the file's
pipeline. The ports follow the oracle. Both write the minimal byte-level
BPE by hand (added tokens first, longest match; the GPT-2 pattern written
out, as Go's regexp has no lookahead; merges by rank, leftmost first),
with the character classes generated from the tokenizers library
(`clients/go/internal/smolvla/gen_tables.py`). The text gets a newline
when it has none, the ids are cut on the left to 48 (the file's
`truncation_side` is left), and padded on the right with `<|im_end|>`
(id 2). Red first: with the file's pipeline, 1 of the 30 instructions in
`clients/fixtures/vla/tokens.json` differed; after, all 30 agree exactly
in Go and in Rust.

Status: in force.
Retires when: the oracle's tokenizer pipeline changes (generate the
fixture again).

**VL-R-6 (ruling): an action may differ from the oracle's by at most
1e-3.** Two FP32 runtimes do not add in the same order, so a port cannot
give the oracle's bits. Measured on this CPU: the three graphs in Python's
ONNX Runtime against the PyTorch policy, 1.8e-6 at most over a 50x6
chunk; the Go and Rust executors against the oracle on the golden chunk
(`clients/fixtures/vla/chunk.json`: a 240x320 test image, "pick up the
block", a 6-value state, noise seed 0), 2.4e-6 each. The tolerance is
1e-3 on every action value (`ChunkTolerance` in Go, `CHUNK_TOLERANCE` in
Rust), about 400 times the largest measured difference and the same as
Arm's validation of its SmolVLA export. The golden chunk, the replayed
runs of the closed loop and its first free run are held to it; the later
free runs are not (VL-R-7).

Status: in force.
Retires when: a measured difference comes near it (find the cause first;
never raise it to pass).

**VL-R-7 (ruling): the closed loop is held run by run, not as a free
trajectory.** In the pick-place scene (`tests/fixtures/mjcf/two_joint_arm.xml`,
"pick up the block", 5 runs of 25 actions, noise seeds 11 to 15), the
oracle loop (`clients/vla_cases.py --part loop`) records each run's state,
camera image, chunk and qpos after it. `clients/vla_compare.py` then holds
each port to:

- replay: each recorded run's own state and image, fed to the port's
  policy, gives the oracle's chunk within 1e-3. Measured: 3.7e-6 at most,
  in Go and in Rust, over all 5 runs;
- the first free run: chunk 1.7e-6, qpos 2.1e-7, the same image bytes;
- Go against Rust over all 5 free runs: the same chunks, qpos and image
  bytes, bit for bit (both run the same ONNX Runtime build in float32
  with no fused multiply-add, and the same MuJoCo build).

The later free runs against the oracle are reported, not held. The two
renderers draw the same bytes for the same state (checked on two fixed
poses), but after run 0 the qpos differs by 2e-7, which changes 6 bytes of
the next 240x320 image. The policy turns that into a chunk difference of
1e-3, and the loop carries it on: 1.5e-2, 4.7e-2 and 9.2e-2 in runs 2 to
4, with 1308 to 5979 image bytes differing. A pixel camera in a closed
loop amplifies a difference in the 7th digit; no tolerance on the free
trajectory would be honest. The case checks the plumbing (camera, state,
tokenizer, graphs, actions, physics), not task success: the base model is
not trained on this scene, the image is mostly dark (a headlight only),
and its gripper targets (about 2.0, sized for the SO100 arm) drive
`j_grip` far past its range.

Latency per chunk on this CPU (Intel i5-7300HQ, 4 cores, 4 threads each,
load average 0.7 to 3.4): Go 2.8 s and Rust 3.0 s on the loop's frames
(the replay runs: the three graphs and the 10 Euler steps, not the camera
or physics). The PyTorch oracle took 6.7 s on the same mostly dark frames,
but 2.6 s on the export check's random image and 3.2 s on the golden
chunk's patterned image, and ONNX Runtime from Python took 2.65 s on the
check's image. The oracle's time depends on its input, so these numbers
are not a speed comparison between the runtimes.

Status: in force.
Retires when: the oracle's own executor can run the loop (the project's
environment gains lerobot), or a scene with a trained task gives a
success measure to compare.

## 2026-10-03: the model catalog (`daisugi models list|search|use`)

**MC-R-1 (ruling): the catalog's entries, as checked.** Each id, license
and size was read from the Hugging Face API on 2026-10-03 (`cardData`,
`safetensors.total`, the `config.json` context length, and each GGUF
repo's file list). All six ids exist as named:

| id | params | license | context | GGUF repo |
|---|---|---|---|---|
| `ibm-granite/granite-4.1-3b` | 3402836480 | apache-2.0 | 131072 | `ibm-granite/granite-4.1-3b-GGUF` |
| `ibm-granite/granite-4.0-1b` | 1631750144 | apache-2.0 | 131072 | `ibm-granite/granite-4.0-1b-GGUF` |
| `mistralai/Ministral-3-3B-Instruct-2512` | 3849090048 | apache-2.0 | 262144 | `mistralai/Ministral-3-3B-Instruct-2512-GGUF` |
| `google/gemma-4-E2B-it` | 5123178051 | apache-2.0 | 131072 | `ggml-org/gemma-4-E2B-it-GGUF` |
| `meta-llama/Llama-3.2-3B-Instruct` | 3212749824 | Llama 3.2 Community License | 131072 | `bartowski/Llama-3.2-3B-Instruct-GGUF` |
| `meta-llama/Llama-3.2-1B-Instruct` | 1235814400 | Llama 3.2 Community License | 131072 | `bartowski/Llama-3.2-1B-Instruct-GGUF` |

The Gemma 4 small model is E2B. Its GGUF repo is the llama.cpp project's
(Q4_0, Q8_0, BF16); `unsloth/gemma-4-E2B-it-GGUF` also has Q4_K_M. The
Llama repos are gated, so their context length is the `context_length`
of the bartowski GGUF. Ministral's size counts its vision encoder, as
`safetensors.total` does. A size is an integer count everywhere; the
text shows it in billions to one decimal.

An entry under a custom licence shows that licence by its full name
("Llama 3.2 Community License"), not the Hub's short id, as every entry
shows its licence. Both defaults are under an OSI or permissive licence
(apache-2.0); a test in each language holds that (2026-10-09).

Status: in force.
Retires when: an entry changes (check its card again then).

**MC-R-2 (ruling): the default follows the voice probe.** The class of
a box is `capable` when its GPU has 6 GB or more, or its RAM is 8 GB or
more, else `weak`; the thresholds, the default per class and the default
search size per class are in `model_catalog.json`, which all three
languages read. The hardware is `detect_voice_hardware`:
`OPENDAISUGI_VOICE_HARDWARE` when set, else the probe `tiers setup` uses.
Cases always set it, so no case reads the box.

Status: in force.
Retires when: a setting replaces the hardware pick.

**MC-R-3 (ruling): the choice is a file of its own, kept as given.**
`daisugi models use ID` writes `<data-dir>/garden_model.json`
(`{"model": ID}`, indent 2, ASCII escapes), not `config.yaml`, so no
other command's config bytes change. The id is kept exactly as typed,
spaces included; only an id that is blank after stripping is refused
(exit 2, three lines). A file that does not read as an object with a
non-blank string `model` counts as no choice.

Status: in force.
Retires when: the choice moves into config.yaml.

**MC-R-4 (ruling): how search asks and reads.** The endpoint is
`HF_ENDPOINT` with trailing slashes stripped, else
`https://huggingface.co`. `HF_HUB_OFFLINE` is read as huggingface_hub
reads it (1, ON, YES or TRUE, any case) and then nothing is asked. The
request target is built in one fixed order: the query percent-encoded
(unreserved ASCII kept, every other UTF-8 byte as `%XX`), `limit=100`,
`sort=downloads`, `direction=-1`, then `expand%5B%5D=` for cardData,
gguf, pipeline_tag, safetensors and tags. Any 2xx is an answer. The
filters run on the answer: size is `safetensors.total`, else
`gguf.total`, integers only; a size filter of 0 or less shows any size,
and a model of unknown size shows only then. The license is `cardData.
license`, its `license_name` when it is `other`, else the first
`license:` tag. Three one-line errors, each exit 1: no answer (offline),
an HTTP status, and an answer that is not a JSON list. None carries the
operating system's error text, which differs between languages.

Status: in force.
Retires when: the API's model list changes shape.

**MC-R-5 (ruling): `models pin` stays in Python.** The old `daisugi
models REPO` (resolve to a commit-pinned file, `--pull` to download) is
`daisugi models pin REPO` and resolves any named repo. The org list in
`model_registry` is now only the scope of discovery. Go and Rust list
`models pin` as not in the binary.

Status: in force.
Retires when: a port carries a Hugging Face file download.

**MC-R-6 (ruling): the trainer's base model.** `python -m
opendaisugi.lora.train` uses `--base-model` when given, else the choice
`models use` recorded in `--data-dir` (default `~/.opendaisugi`), else the
default for this hardware (MC-R-2).

Status: retired 2026-10-03. The trainer is now `daisugi lora train` in
all three languages, which picks the base model the same way (PK-R-6).

**MC-R-7 (ruling): the group is hidden at the top level.** `models` is
an operational group like `tiers`, so the ten visible top-level commands
stay ten (SF-5); `help --all` lists it, and so do the binaries' help.
Bare `daisugi models` prints the group help and exits 2 in all three.
Help texts are not cases (as RK-R-11).

Status: in force.
Retires when: the top-level set is ruled again.

**MC-R-8 (ruling): the Llama notice.** A model trained from a Llama base
must carry Meta's "Built with Llama" notice. `docs/integrations.md` says
so once, where training is described.

Status: in force.
Retires when: no Llama model is in the catalog.

**MC-R-9 (ruling): what the wording sweep left as it was.** The user
docs, the help texts and the changelog no longer say why an engine or
model is not offered. These stay as written, as records or build sources
no user reads to run daisugi: `docs/research/`, the plans under
`docs/plans/`, `docs/superpowers/` and `docs/exploration/` (none says it
there), this file's earlier rulings, and the comments and the left-out
name list of the native builds (`clients/native/moonshine-cli`,
`clients/native/parakeet-cli`, `clients/go/scripts/native.sh`). The
owner may reword those too.

Status: retired 2026-10-07 by the owner's ruling. The comments and
left-out lists of `clients/native/moonshine-cli`,
`clients/native/parakeet-cli` and `clients/go/scripts/native.sh` now keep
only the technical facts: which code is left out, and that leaving it out
keeps the build small, offline and easy to audit. They give no origin,
country or model-ancestry reason. The plans under `docs/plans/` give
none. The name list that `check-binary.sh` checks is unchanged, so the
check works as before. `docs/research/`, `docs/superpowers/` and this
file's earlier rulings stay as dated records.
Retires when: retired.

**Verified:** 68 model cases (`clients/fixtures/models/`); Go and Rust
each agree on all 68. The 36 `tiers setup` cases of K3 were recorded
again for the new families and the new step text; both binaries agree
on 34, with the same one not ported and one refused as before. The CLI
suite: Go and Rust each 591 agree, 9 refused, 9 not ported, 0 disagree.

## 2026-10-03: the ML pack (`daisugi pack`, `daisugi lora train`)

**PK-R-1 (ruling): a pack is a child process, not an embedded Python.**
The gate core stays standalone Go and Rust (owner ruling 2026-10-03). The
parts that must stay Python (LoRA training, lerobot's SmolVLA policy) run
in a worker process that the binaries start in a pack's own Python
(`docs/research/ports-ml-2026-10-q4-packaging.md`). A pack is
`DATA/packs/NAME/`: `python/` (the pinned CPython, unpacked), `venv/` (made
with it), `lock.txt`, `worker/` (the worker, the files its jobs run, and
`pack.json`) and `manifest.json`, written last; a directory without the
manifest is not installed. The worker protocol, `daisugi-pack-1`, is
defined once, in `src/opendaisugi/pack/worker.py`: a ready line, then
requests framed as the voice engines frame a clip (four bytes of length,
then a JSON object `{"job", "args"}`), and replies as JSON lines
(`progress`, then one `result` or `error`). The worker keeps its own copy
of stdout and points file descriptor 1 at stderr, so a library's print
cannot break the reply stream. The jobs are `selftest`, `train` and
`vla-chunk`; `echo` and `die` exist only with `DAISUGI_PACK_TEST_JOBS=1`,
for the protocol's cases. A job that fails never ends the worker; a
worker that dies, or writes a line that is not a reply, is one line and
exit 1 in the caller. CUDA packs (`train-cuda`, `vla-ref-cuda`) are listed
as needing a GPU; `install` and `bundle` refuse them.

Status: in force.
Retires when: a port carries a trainer of its own.

**PK-R-2 (ruling): the pinned CPython.** python-build-standalone release
20261001, CPython 3.12.15, x86_64 glibc, the `install_only_stripped`
archive (34,289,539 bytes), sha256
`7bb1659e3235077b7f63d5b6eb6ce653c6fcd6c5041e9d5f73b42ce10421464d`, as the
release's `SHA256SUMS` lists it (fetched 2026-10-03; the same file lists
the research note's `install_only` hash, `0e56475e...`). The archive
holds 3485 files and 1049 symbolic links in POSIX ustar, 9 names in the
prefix field. The unpack takes files, directories and symbolic links
under `python/` only, and refuses a link that leaves the pack.

Status: in force.
Retires when: the pin moves (fetch `SHA256SUMS` again).

**PK-R-3 (ruling): the train lock, and how it was made.**
`packs/train.lock` pins 57 packages with every wheel's sha256: torch
2.14.1+cpu, transformers 5.18.0, peft 0.21.2, trl 1.14.1, datasets 5.0.1,
accelerate 1.15.0, numpy 2.5.3, pyarrow 25.0.1 and their dependencies.
No line has a marker and no NVIDIA or triton wheel is in it. It was made
once from `packs/train.in`, with uv by its full path and a scratch cache:

    UV_CACHE_DIR=SCRATCH UV_NO_CONFIG=1 ~/.local/bin/uv pip compile packs/train.in \
      --generate-hashes --python-version 3.12 --python-platform x86_64-manylinux_2_28 \
      --only-binary :all: --index-url https://pypi.org/simple \
      --extra-index-url https://download.pytorch.org/whl/cpu \
      --index-strategy unsafe-best-match \
      --custom-compile-command "see ruling PK-R-3" -o packs/train.lock

The install uses no uv: the pack's own pip runs with `--isolated
--no-cache-dir --require-hashes --only-binary=:all:` and the catalog's
index URLs (or `--no-index --find-links` for a bundle).

Status: in force.
Retires when: the lock is made again (record the new command here).

**PK-R-4 (ruling): the vla-ref lock is the export environment's.**
`packs/vla-ref.lock` is a byte copy of `clients/vla/requirements.txt`
(VL-R-2); a test checks the copy. It was not installed on this box: the
pinned snapshots of VL-R-2 are not here, and the brief asks for one real
install, of `train`. So `--only-binary=:all:` is not proven for every
line of it.

Status: in force.
Retires when: a vla-ref install runs (record its result here).

**PK-R-5 (ruling): the bundle, and what an offline install trusts.**
`pack bundle NAME OUT` fetches the CPython tarball (checked), unpacks it
in a scratch directory and runs its pip `download --no-deps
--require-hashes --only-binary=:all:` on the lock, then checks every lock
line has a wheel whose sha256 the lock pins. OUT is a directory, or a tar
when it ends in `.tar`. The tar is written the same way, byte for byte,
by the three binaries: ustar, names sorted, mode 0644, owner 0, mtime 0,
two zero blocks at the end. A name that does not fit the ustar fields
gets a POSIX extended header (type `x`, named `././@PaxHeader`, one
`path` record) before its own header, which holds the first 100 bytes of
the name: the real train bundle has a wheel name of 107 bytes
(`charset_normalizer-3.5.2-cp312-cp312-manylinux2014_x86_64.manylinux_2_17_x86_64.manylinux_2_28_x86_64.whl`).
`install --offline` checks the tarball and every wheel against the pins
in the binary (the catalog and the lock), never against a file in the
bundle, before pip sees them.

Status: in force.
Retires when: the bundle format changes.

**PK-R-6 (ruling): the garden's train step is `daisugi lora train`.**
No command trained before: the trainer was `python -m
opendaisugi.lora.train`, and `lora` was not in the binaries. Step 4 of
the brief is read as: the trainer becomes `daisugi lora train` in all
three languages. Its options are the trainer's. The base model is
`--base-model`, else the choice `models use` recorded in `--data-dir`,
else the default for this hardware (MC-R-6, retired into this). Go and
Rust run it in the train pack (the `train` job). Python runs it in its
own process when torch, transformers, peft, trl and datasets import, else
in the pack; `OPENDAISUGI_LORA_TRAIN=pack` sends it to the pack (the
golden cases set it). `lora export` stays in Python; the binaries list it
as not in the binary.

Status: in force.
Retires when: a port carries a trainer of its own.

**PK-R-7 (ruling): the VLA reference is one module, reached through the
pack.** The oracle of VL-R-2 (the policy loader, the noise, the
tokenizer and stats lookups, one chunk) moved from
`clients/vla_cases.py` and `clients/vla_export.py` into
`src/opendaisugi/pack/vla_oracle.py`; the two scripts import it, and every
pack carries a copy for the `vla-chunk` job. `clients/vla_cases.py --part
chunk --pack DAISUGI` writes the golden chunk through `DAISUGI pack run
vla-ref vla-chunk`, so the compare's oracle needs no separate export
environment for that part. The job's input is a JSON file with the
policy and backbone snapshot directories, the task, the state, the noise
seed and the image (height, width, RGB as zlib then base64). The loop
part still runs in the export environment. Not run for real here
(PK-R-4); a test drives the script against a fake daisugi, and the
oracle's noise is checked against `clients/fixtures/vla/process.json`.

Status: in force.
Retires when: the vla-ref pack is installed and the chunk is made through
it (compare it with `chunk.json` then).

**PK-R-8 (ruling): the one real install and train run.** On this box
(4-core i5-7300HQ, no GPU used, 4 GB memory cap):

- `pack bundle train` (Rust): the CPython and 57 wheels, a 351.5 MB tar,
  in about 70 s; the first try found the long wheel name of PK-R-5.
- `pack install train --offline` (Go): 65 s, 1.5 GB on disk; the
  self-test imported torch 2.14.1+cpu, transformers 5.18.0, peft 0.21.2,
  trl 1.14.1 and datasets 5.0.1. The Rust binary installed it again from
  the same bundle after each trainer fix, with no network.
- The base model is `ibm-granite/granite-4.0-1b` (3.1 GB downloaded once
  into a scratch `HF_HOME`), the smallest catalog model that downloads
  without a token: `meta-llama/Llama-3.2-1B-Instruct` is gated (and
  would carry the notice of MC-R-8).
- The run found three trainer faults, each fixed test first: trl 1.14
  calls the sequence limit `max_length`; transformers refuses bf16 with no
  GPU unless `use_cpu` is set; and trl's default chunked loss and its
  router loss read mixture-of-experts fields that the dense Granite 4
  config has with zero experts. The trainer now asks for the model's own
  loss (`nll`) and no router loss when the model has no experts.
- One step on 4 examples, batch 4, sequence limit 64, in bf16, through
  `daisugi lora train` in the Rust binary: 852 s in all (840 s in the
  trainer), loss 5.44, a 34 MB adapter (`adapter_model.safetensors`)
  and its checkpoint; the trainer's lines streamed on stderr and the
  binary printed where the adapter is. The same step run directly in the
  pack's Python before it took 846 s and gave the same loss.
- The run used one core for most of the step (bf16 on a CPU without
  native bf16 instructions), with a peak resident size of about 4 GB.

The pack, the bundle and the model were deleted afterwards.

Status: in force.
Retires when: the pins of PK-R-2 or PK-R-3 move (run it again).

**PK-R-9 (ruling): the AUR split package `daisugi-ml`.** The source
PKGBUILD (`packaging/aur/opendaisugi`) is now a split package:
`opendaisugi` (the binaries, with `daisugi-ml` as an optional dependency)
and `daisugi-ml` (`arch=any`), which depends on `python`,
`python-pytorch`, `python-transformers`, `python-peft`, `python-trl`,
`python-datasets` and `python-accelerate`, and lays out the train pack as
a system pack (PK-R-10) with `scripts/system-pack.py`. `package()`
builds no virtual environment and runs no pip. Of those names, only
`python-pytorch` (the Arch repositories) and `python-peft` (AUR) were
checked, by the research note; the other four were not. `vla-ref` has no
system pack (lerobot is not packaged for Arch). Not published.

Status: in force.
Retires when: the Arch package names are checked (then drop this note).

**PK-R-10 (ruling): system packs.** A system pack is
`/usr/lib/opendaisugi/packs/NAME/` (or the directory
`OPENDAISUGI_SYSTEM_PACKS` names; the command line reads it, and the
golden cases set it), with the same `worker/` and a `manifest.json` whose
`source` is `system`, and the system's Python as `venv/bin/python`. The
binaries use the data dir's pack first, then the system's. `list` shows
it as `system`; `status` checks its worker files and protocol only (it
has no pins of its own) and says to reinstall the system package on a
problem; `remove` never touches it.

Status: in force.
Retires when: a distribution ships the packs another way.

**PK-R-11 (ruling): the pack's golden cases.** `clients/pack_cases.py`
runs 24 cases through the oracle against a fake server: a fake CPython
tarball (with a symbolic link, an executable and a name in the prefix
field, as the real one has) whose `python3` runs `/usr/bin/python3`, so
`venv` and pip are real; three tiny wheels (one with a name longer than
100 bytes) and their hashed lock, committed under
`clients/fixtures/pack/assets` by `clients/pack_fake.py`. The cases need
`/usr/bin/python3` with venv and ensurepip, and without `datasets`. The
system Python's version is written as `{PYVER}`. Help texts are not cases
(as RK-R-11). Some steps hold this box's `/usr/bin/python3` (3.14) text
beyond its version: the trainer's traceback lines in the `lora train`
steps and `json` 2.0.9 in a `selftest json` step, so the cases are made
again on a box with another system Python. `clients/pack_compare.py --binary B` holds each step's exit
code, stdout and stderr, the tree after, and the paths the fake server
was asked, byte for byte.

Status: in force.
Retires when: the cases change shape.

**Verified:** 24 pack cases (`clients/fixtures/pack/`): Go and Rust each
agree on all 24. The Python unit tests (`tests/test_pack.py`,
`tests/test_lora_train_script.py`), the Go `internal/pack` tests and the
Rust `pack::` tests pass.

## 2026-10-03: the last parity rows, and CPU training

**PK-R-12 (ruling): a pack install, remove or bundle is the operator's.**
The gate's owner rule (`owner_rule.py`) did not list `pack` as a
command. So `daisugi pack install train` read as the top-level command
`install`, and the gate denied it with the wrong words ("only the
operator changes how the gate enforces"); `daisugi pack status gate
disarm` read the same way. Now `pack` is a command with its six
subcommands, and `pack install`, `pack remove` and `pack bundle` are
owner verbs with their own refusal: "only the operator installs, removes
or bundles a pack. Run it yourself." Each one fetches code from the
network, or changes the Python the binaries run as a worker, outside any
envelope permission an agent's `daisugi` allowlist gives. `pack list`,
`pack status`, `pack run` (a job in a pack the operator installed) and
`lora train` (heavy, but it only computes) stay with the envelope. The
rule is the same in the three languages, with 12 gate cases ("pack
rule").

Status: in force.
Retires when: the operator wants an agent to install packs (then drop
the verbs).

**PK-R-13 (ruling): CPU training runs in fp32 on every core; the
measurement is the owner's.** On a box with no GPU the trainer now loads
the model in float32, turns bf16 off, and sets torch's intra-op threads
to the cores this process may use (`os.sched_getaffinity`). Its first
output line says both ("training on the CPU in float32 with 4 threads");
the pack worker sends that line on as the first progress line. A GPU
keeps bf16 and torch's own thread count. The before-and-after
measurement of PK-R-8 was not run here: the step took 852 s, which is
longer than one foreground command may run under the memory rule, and
it cannot be split; the pack, the 351.5 MB bundle and the 3.1 GB model
were also deleted after PK-R-8. To measure it, with about 5 GB free:
`daisugi pack bundle train --out B`, `daisugi pack install train
--offline --bundle B`, then the same 4-example `daisugi lora train`
run as PK-R-8 (batch 4, sequence limit 64, `--base-model
ibm-granite/granite-4.0-1b`), and compare the time with 852 s.

Measured 2026-10-08 (PK-R-14). Status: in force.
Retires when: the trainer's CPU settings change.

**PK-R-14 (ruling): the CPU step, measured through the pack from Go and
Rust.** On this box (4-core i5-7300HQ), with the train pack built by
the Go `pack bundle train` (336 MB, 2 min 23 s) and installed by the
Rust `pack install train --offline` (17 min 20 s: the disk was under
heavy load from other work; PK-R-8 took 65 s), in a scratch HOME:

- Base model `ibm-granite/granite-4.0-350m` (the same Granite 4 family
  as the 1B; 352 M parameters). One step on the 4-example set of PK-R-8
  (batch 4, sequence limit 64, `--epochs 1 --grad-accum 1`). The first
  line both ports printed: "training on the CPU in float32 with 4
  threads".
- Go `daisugi lora train`, first run (model download, cold cache): the
  step took 57.9 s in the trainer, 7 min 24 s in all.
- Go, warm: 4.3 s in the trainer, 2 min 34 s in all. Rust, warm: 3.1 s
  in the trainer, 3 min 4 s in all. Loss 4.437 in every run. The time
  outside the step is the worker starting and the imports and the
  model load under the same disk load.
- The 1B model of PK-R-8 was not run: it has 1.63 billion parameters,
  6.5 GB in float32 before any activation, so it cannot fit the 4 GB
  cap every job here runs under (PK-R-8 fit only because bf16 halves
  the weights). On a CPU box with less than about 8 GB free, the fp32
  choice of PK-R-13 makes the 1B base model fail where bf16 ran in 852 s.

The pack, the bundle and the model were deleted afterwards.

Status: in force.
Retires when: the pins of PK-R-2 or PK-R-3 move, or PK-R-13 changes.

**TS-1 (ruling): `tiers stats`.** Both ports read the journal as
`accounting.tier_stats` does: the successful traces since the window's
start (the SQL filter on `created_at`), each trace's envelope
`generated_by` bucketed into tier 0, 1 or 2, a trace that does not load
skipped, and the per-tier token estimates, the hit rate and the tier-1
providers (ties in their first order). `_row_ts` reads a created_at as
`datetime.fromisoformat` does for the forms the journal writes and a few
more: a date, then `T` or a space, `HH:MM`, `:SS` and a fraction of 1 to 6
digits, then `Z` or an offset `+HH:MM`; a naive time is local time. Any
other text reads as no time, so the trace is not dropped by the second
check, where Python may parse a rarer ISO form. The journal never writes
one. 12 k3 cases ("tiers stats").

Status: in force.
Retires when: the journal writes another created_at form.

**SR-1 (ruling): `tiers setup --remote`.** Both ports probe a model host
as `model_host.probe` does (Ollama `/api/tags` and `/api/show`, an
OpenAI-compatible `/v1/models`, the Anthropic `/v1/messages` with the
probe model), record it in config.yaml as `record` does, and print
`describe_host`'s lines. Three limits:

- The probe's requests go out with each client's own `user-agent` and
  `accept-encoding` (httpx names itself and its decoders; the ports send
  `opendaisugi` and `identity`). The k3 normalization holds these two
  headers of a probe request as `{CLIENT}`; the method, path, body,
  `accept` and `content-type` are compared.
- A host answer Python reads with a type the ports do not hold the same
  way is refused, before anything is written: a `true` or `false` where
  Python's `isinstance(v, int)` counts it an int, an integer past 64 bits,
  a model name that is not a string, a port with digits of another script.
- `num_ctx` is read with an ASCII `\b` and ASCII spaces and digits; the
  oracle's pattern is Unicode-aware, so a Modelfile with a non-ASCII
  letter right before `num_ctx` can differ.

The fake server of the k3 cases now answers a GET by its path (the key is
the SHA-256 of `GET <path>`). 22 k3 cases ("setup remote").

Status: in force.
Retires when: the probe changes its wires.

**RP-1 (ruling): `gate replay`.** Both ports replay a captured session as
`gate.replay_captures` does: each line of the captures file decided in
audit terms against the envelope by the gate's own record check (the
data dir's gate root for the verifier client and the dialect pin), as the
record the audit log holds, then the report `gate report` builds, with
the oracle's own counts line and `DENY` lines. A capture whose fields are
not of the step's types is the deny the oracle's pydantic step raises; a
line that is JSON but not an object, a missing file, and an envelope that
does not load end as the oracle's traceback (its last line, exit 1). A
line in a JSON form this binary does not read is refused. The JSON report
holds each call's `elapsed_ms`; the compare holds it as 0. 19 cli cases
("replay"). The Go envelope reader does not take a JSON envelope or a YAML
flow sequence (3 cases refused, row 12 of the inventory); the Rust one
does.

Status: in force.
Retires when: the replay changes its record.

**HP-1 (ruling): `daisugi help`.** `help` prints the oracle's start-here
text. `help --all` prints the oracle's text for every command, grouped,
generated from the oracle by `clients/gen_help.py` (checked in CI) into
both ports, with the lines of the commands a port does not carry left
out: the list `HELP_NOT_PORTED` in `clients/cli_cases.py` and in each
port (`bench`, `conformance`, `coppice`, `gate audit`, `lora export`,
`models pin` at the time of writing; `viz` until VZ-1). The compare drops the same
lines from the oracle's text. 5 cli cases ("help").

Status: in force.
Retires when: the ports carry every command (then the list is empty).

**VZ-1 (ruling): `daisugi viz`.** Both ports render a distilled pathway's
plan as `viz.render_dag_html` does: the oracle's template (embedded; a Go
test checks the copy, Rust includes the file), the view model built from
the port's own verify, model sizing with the default ladder and the
plan's dependency levels (networkx's topological generations, now
`Levels` in Go and `levels` in Rust, which `topo_order` flattens), and
`</` escaped. The message counts the page's characters, as `len(html)`
does. Without an id it lists the pathways. 9 k3 cases ("viz").

Status: in force.
Retires when: the template or the view model changes shape.

**MP-1 (ruling): `--max-parallel` above 1.** `orchestrate` and `weave`
now run as the oracle's supervisor does in parallel mode: the steps in
dependency-level order (networkx's topological generations), and on
entering a level, its parallel-safe steps (shell, file, network, and task
steps under an unlimited budget; never a weave step with attempts, a slot
input, or one that does not run on resume) that pass their verify and
approval run at once, at most `--max-parallel` at a time, when there are
two or more. The loop then approves, receipts and halts in order, using
each prefetched result; a prefetched step the loop never reached gets its
outcome and receipt at the end. Go runs them on goroutines; Rust on
scoped threads, each executor giving a job that owns a copy of what it
needs (the task executor sizes the step first, asks with a copy of the
model client, and records the spend in order). Since steps run at once,
the k2 compare reads a case's model requests as a multiset ("unordered").
5 k2 cases ("orch max parallel") and 4 weave cases ("max parallel"). The
k2 refused guard drops to 5 and the weave guard to 0.

Status: in force.
Retires when: the supervisor's parallel mode changes.

**MX-1 (ruling): the MCP methods with no handler.** The oracle's server
registers no handler for `resources/subscribe`, `resources/unsubscribe`,
`completion/complete` or the `tasks/*` methods: the SDK checks the params
(-32602 when they do not validate) and answers -32601 "Method not found".
`resources/read` checks the `uri` as pydantic's `AnyUrl` and answers
error 0 "Unknown resource: URI" with the URL as it serializes. A tool
call's `task` (an object with an optional int `ttl`) is read and not used.
Both ports now answer these. A URI is normalized for the forms the ports
read (a scheme, a special scheme's host lowercased, its default port
dropped, an empty path made "/", `file:///...`, any other scheme kept);
a URI with `%`, a backslash, brackets, user info, a numeric host, a dot
segment, or a character outside printable ASCII, a `ttl` that is not an
int, and a `task` on another method stay refused with -32000 (K3-3).
33 k3 cases.

Status: in force.
Retires when: the ports parse URLs as the WHATWG parser does.

**IN-1 (ruling): every layer of `install`.** Both ports now write all
of `install.py`'s layers, in the order its `apply` writes them: the
skill, the MCP server, the capture hook and the pathway instructions for
each harness (Claude Code, Codex, Hermes, OpenClaw), then the gate and
gateway layers; `--uninstall` reverses all of them as `reverse` does. So
`install` with no flag, `--print-skill`, `--uninstall` alone,
`--allow-shell-decomposition`, `--ask` (with `--gate --enforce`), and
`--report herdr` and `coppice` (the floor-report and subagent hooks, and
`floor_report` in config.yaml) are answered, and an install that is not
`--yes` asks the auto-tend question after the confirm, as the oracle does.
The patched oracles of the cli suite (the gate-only install and
uninstall shims, and the dropped "layers come from the Python CLI" lines)
are gone: every install case runs the oracle as it is. Three limits:

- The skill. The oracle links the packaged skill directory into the
  harness; a binary has no package directory, so each port copies the
  files from itself (Go embeds a copy a test checks; Rust includes the
  package's files). The plan still says "Symlink", as the oracle's does.
  The compare reads the oracle's link as that copy (directories 0755,
  files 0644). A copy that is already there, file for file, is left as
  it is, and still listed, as `_link_skill` returns its target.
- A runtime whose apply raises part way (a Claude Code with no
  `~/.claude`: the capture hook's write raises after the skill and the
  MCP server are written) keeps what it wrote and lists none of it, as
  the oracle's does.
- A Hermes config.yaml in a YAML form the port's reader does not take
  (the oracle warns and skips one that does not parse) is refused (row
  12 of the inventory), and so is a hooks or MCP value whose shape the
  oracle's code raises on.

35 new cli cases ("layers ...", "uninstall layers ..."); 110 earlier
install cases run against the whole oracle now. The cli runner can run
earlier commands in a case's home first (`pre`).

Status: in force.
Retires when: install.py changes its layers.

**LX-1 (ruling): `lora export` in both ports.** `daisugi lora export
OUTPUT` writes the journal's successful traces as training JSONL, as
`lora.dataset.emit_jsonl` does: alpaca or chat form, `--days`,
`--min-task-chars` and `--system-prompt`, the envelope as
`model_dump_json()`, every line with `ensure_ascii`, and the summary JSON
on stdout. It makes the parent directory first and lists the traces
next, in the oracle's order. A trace whose YAML is gone or does not load
counts as `skipped_load_error`. A trace in a YAML form the port's reader
does not take is refused (row 12), and so is a task that is not text,
where the oracle raises. Nine k3 cases ("lora export ...").

Status: in force.
Retires when: `lora/dataset.py` changes.

**MP-2 (ruling): `models pin` in both ports, without `--pull`.** `daisugi
models pin REPO` asks the Hub what `resolve_pinned` asks through
huggingface_hub: the tree of `main` (`recursive=true&expand=false`), then
the model info for its commit. It prints the first matching file in
sorted order and the commit, in text or JSON, and the oracle's
`NoMatchingFile` line with exit 2. The ports refuse, before or in place
of a request:

- `--pull`. `hf_hub_download` writes the Hugging Face cache (blobs, a
  snapshot link, lock files) and draws progress bars on stderr; a binary
  does not write that cache.
- What huggingface_hub raises on: a repo id `validate_repo_id` rejects
  (the ports take ASCII ids only, since Python's `\w` takes other
  letters too), `HF_HUB_OFFLINE`, a Hub that cannot be reached, a status
  outside 2xx, a body that is not JSON.
- A tree with a next page (a `Link` header): the oracle asks again with
  backoff, and the ports do not follow pages.
- A tree entry or a model info that huggingface_hub's classes might
  raise on, or read with parts the ports do not model: a last commit, a
  security status, eval results, inference-provider mappings, and a date
  outside the Hub's `...Z` form. A real model info (dates, siblings,
  card data, safetensors, transformers info) is read.

31 models cases ("pin ..."); the fake Hub now answers a raw request
target and can send headers.

Status: retired 2026-10-08 by RF-7: the ports write the Hugging Face
cache, follow pages, and raise what huggingface_hub raises; the one
refusal left is listed there.
Retires when: met (see Status).

**MP-3 (ruling): the Hub's agent registry is not fetched.**
huggingface_hub 1.30 builds its User-Agent with `detect_agent()`, which
fetches `{HF_ENDPOINT}/api/agent-harnesses` once per HF_HOME (and caches
it there) unless `HF_HUB_DISABLE_TELEMETRY` is set. That request carries
nothing the command needs, and the ports do not make it. The models
cases set `HF_HUB_DISABLE_TELEMETRY=1` for both sides
(`models_cases.run_case`), so the requests compared are the ones `models
pin` makes for the user. The ports never change their output on it.

2026-10-08: the Python product now turns it off too. `import opendaisugi`
sets `HF_HUB_DISABLE_TELEMETRY=1` when it is unset (a value the user set,
"0" included, is kept), and the pack worker, which runs by its path, sets
it itself; so do the dev scripts that load Hub models. Python, Go and
Rust now send no agent name and never ask for the agent list.
`HF_HUB_DISABLE_IMPLICIT_TOKEN` is not set: it would drop a saved
`hf auth login` token, and a gated model the user chose would not
download. `tests/test_hf_telemetry_off.py` checks all of it, and that no
Go or Rust source names the agent list or an agent/ User-Agent part.

Status: in force.
Retires when: huggingface_hub stops making the request.

**YM-1 (ruling): YAML is read as `yaml.safe_load` reads it, in both
ports.** Go (`internal/pyyaml/full.go`) and Rust (`src/pyyaml`) carry a
translation of PyYAML 6's reader, scanner, parser, composer, resolver and
SafeConstructor, one method at a time, so the same text gives the same
value or raises the same exception with the same words (`str(exc)`, marks
and snippets included). Every reader goes through it: config.yaml (the
CLI and the gate), plan and envelope files (`run`, `orchestrate`, `gate
register`, `gate replay`), journal traces, an episodes file, skill
frontmatter, registry bundles and a Hermes config.yaml. Where the oracle
catches the error the ports print what it prints; where it does not, the
ports print the traceback's last line or refuse, as each caller's ruling
says. `clients/yaml_cases.py` records safe_load's answer for 2623 texts
(a fixed list and seeded mutations of it); both ports give the same
value or the same error on all of them, and refuse none.

What the result model does not hold is refused, never guessed: a
`!!binary` value (base64's errors are not modelled), a `!!set`,
`!!omap` or `!!pairs` value, a node that holds itself, a float or
timestamp key, nesting deep enough to reach Python's recursion limit, a
`\u` escape for a lone surrogate, a tag URI escape that is not UTF-8,
an `!!int` or `!!float` text Python's `int()` or `float()` might read
another way, and a date or a key that is not a str where the caller
needs JSON. A later error in the same text still wins, as in Python: a
refusal is kept until the whole load has run. Two refusals sit above the
loader: a config.yaml key that is not a str (`load_config` keeps it, and
`daisugi config` prints it among the unknown keys; the ports refuse the
file), and the cases (`config bool key`) that hold one.

Five cases that were refused now agree in both ports: k2 `run envelope
not yaml`, `run plan not yaml`, `orch envelope not yaml`; garden `tend
yaml not safe_dump form`, `auto-tend then tend unreadable trace`.

Status: in force.
Retires when: PyYAML's loader changes.

**PX-9 (ruling): proxy URLs and NO_PROXY entries are parsed as httpx
parses them.** Both ports carry a translation of httpx 0.28's
`_urlparse.urlparse` (Go `netproxy/urlparse.go`, Rust `netproxy.rs`):
the URL and authority patterns, `encode_host` (IPv4 and IPv6 checks,
percent-encoding of an ASCII name, IDNA for a non-ASCII one),
`normalize_port`, the path checks and `quote`. So a proxy URL or a
NO_PROXY entry gives the parts httpx gives, and one httpx rejects stops
the client with httpx's `InvalidURL` words, as the oracle's client does
(the gateway answers each turn with 500; the model call fails). A port
`int()` reads (`+8080`, ` 8080`) is read. A NO_PROXY entry holding `%`
is a pattern no host matches, as in httpx. URLPattern reads only the
scheme, host and port of an entry, so an entry with a path or userinfo
is read too.

The environment's order now reaches every reader: a reader handed a
map orders it by this process's environment (`OrderedFromMap`,
`ordered_vars`), so two names that differ only in case resolve as
`getproxies` resolves them. Only a map that does not come from the
process environment (a test's) is still refused when two such names
differ.

Still refused, never guessed (this retires the rest of PX-5):

- a port no socket takes (0, negative, above 65535; a decimal digit of
  another script is read since RF-6);
- a proxy host httpx sends percent-encoded (where its connection fails
  is not modelled), and an IPv6 address with a zone;
- an IDNA host outside the letters the ports encode: ASCII letters,
  digits and hyphens and the lower-case Latin-1 letters (U+00DF to
  U+00FF but U+00F7), all PVALID; a label already in `xn--` form; and a
  NO_PROXY entry whose host is in `xn--` form (URLPattern orders mounts
  by the decoded host's length);
- a request to a non-ASCII host through a proxy (the request line and
  Host header would need the IDNA form everywhere they are written).

New cases: gateway `env proxy port with a plus`, `port after a space`,
`port not a number`, `ipv4 with a leading zero`, `ipv6 not closed`,
`idna host not valid`, `no_proxy percent`, `no_proxy bad port`,
`no_proxy non-ascii`, `no_proxy with a path`; garden `tend api proxy
names that differ in case`, `port not a number`, `no_proxy bad port`.

Status: in force.
Retires when: httpx's URL parser changes.

## 2026-10-08: the Omarchy and AUR packages (prepared, not published)

**PK-R-15 (ruling): `daisugi` is the Go build, and DAISUGI_PORT hands
`install` to another port.** The owner's ruling of 2026-10-07: the
package installs `daisugi` as the Go build, with `daisugi-rs` and
`daisugi-py` beside it so the three can check each other, and
DAISUGI_PORT chooses the default. The gate's hot path must not gain an
exec. The mechanism: `install` in every port reads DAISUGI_PORT (`go`,
`rust` or `python`). When it names this port, or is unset, install runs
as before. When it names another port, install runs that port's binary
in place of this process, with the same arguments and root flags, and
DAISUGI_PORT_HOP=1 set. The binary must be beside this one, under its
fixed name (`daisugi`, `daisugi-rs`, `daisugi-py`). The port that runs
install writes the hook as it always did, with its own path: the Go and
Rust ports `<self> gate check ...` with the marker, the Python port
`python -m opendaisugi.gate_client ...`. So a hook runs the chosen port
directly, every reader already reads all three hook forms, and the hook
content stays at parity. One line and exit 2, nothing changed, when the
value is not a port, when the named binary is not beside this one, or
when a hand-over reached a binary of another port (DAISUGI_PORT_HOP set).
Only `install` reads the variable; every other command runs the binary
the user typed. `scripts/install.sh` keeps its own DAISUGI_PORT (go or
rust), which chooses the port it builds and installs as `daisugi`.

Tests: `TestPortHop`, `TestInstallHandsOverToDaisugiPort` (Go),
`cli::port::tests`, `install_hands_over_to_the_port_daisugi_port_names`
(Rust, the real exec), `tests/test_port.py` (Python, the real exec).

**PK-R-16 (ruling): `daisugi-py` is its own package, built from the
wheel.** Every runtime dependency of the wheel has an Arch package:
pydantic, networkx, yaml, typer, click, numpy and z3-solver in extra,
and tree-sitter (0.26.0) and tree-sitter-bash (0.25.1) in the AUR, the
versions the gate pins (checked 2026-10-08). So it is a package with
Arch dependencies, not a `uv tool install`. It is a separate PKGBUILD,
not a split of `opendaisugi`, so installing it builds no Go, Rust or Z3.
The console script `daisugi` is renamed `daisugi-py` in package(). Gap:
Arch's python-z3-solver is 4.16.0; the Go and Rust ports link Z3 5.1.0,
and the gate's cases were measured with 5.1.0.

**PK-R-17 (ruling): the Rust hook entry is `daisugi-gate-rs`.** The Go
module has its own `cmd/daisugi-gate` (not shipped), and the pi and
OpenCode extensions are called daisugi-gate, so the Rust one takes the
port suffix. The release tarballs keep their names (`daisugi`,
`daisugi-gate` in the Rust tarball); the packages rename at install.

**PK-R-18 (ruling): the source package's build tools.** `rust` joins the
makedepends (cargo builds daisugi-rs), `curl` is named (preflight.sh
needs it), `gcc-libs` joins depends (the Rust binaries link libgcc_s).
zig stays a pinned tarball in source=() even though Arch's extra has zig
0.16.0: native.sh needs that version exactly, and a later Arch zig would
stop the build. Z3 builds with `zig c++`, so no g++ is needed (Arch's gcc
has it anyway). prepare() runs `cargo fetch --locked` into
`$srcdir/cargo`, and build() sets CARGO_NET_OFFLINE, so build() needs no
network. build() unsets makepkg's CFLAGS, CXXFLAGS, CPPFLAGS, LDFLAGS and
RUSTFLAGS: z3-build-env.sh appends its path maps to them, and they would
reach `zig c++`. `pkgver` stays the project version (pyproject.toml); a
test holds every PKGBUILD to it, so a release bumps them together.

Status: in force.
Retires when: the owner changes the default binary or publishes packages
another way.

**PK-R-19 (ruling): the speech engines are two packages built as
native.sh builds them.** `packaging/aur/opendaisugi-voice` makes
`moonshine-cli` and `parakeet-cli` from the pinned sources: openDaisugi,
Moonshine, CrispASR and ggml by commit, ONNX Runtime and zig by sha256.
native.sh finds each in its build directory, checks it again and fetches
nothing; it builds exactly what `native.sh --moonshine` and `--parakeet`
build. check() runs `check-binary.sh` on both Parakeet programs, checks
each prefix's manifest, and runs each engine through a link (exit 2,
usage) to show it loads. Each engine lives in
`/usr/lib/opendaisugi/<engine>` with a relative link in `/usr/bin`:
moonshine-cli finds its private `libonnxruntime.so.1` through
`$ORIGIN/../lib`, which the loader resolves through the link (checked
with ldd in an Arch container), and every port finds `parakeet-quantize`
beside the resolved `parakeet-cli`. The private ONNX Runtime keeps clear
of Arch's `onnxruntime`. `!strip` keeps every file as native.sh's manifest
lists it. A package must run on any x86_64 CPU, so native.sh gains
`DAISUGI_PARAKEET_ISA` (auto, the default, as before; baseline;
x86-64-v3), and the package sets baseline: slower ggml code on a box with
AVX2 than a checkout build gives. No model is packaged: the first `voice
serve` fetches it and checks its sha256, as before. Built with makepkg
in an Arch container on 2026-10-08: moonshine-cli 11,063,134 bytes,
parakeet-cli 1,055,541 bytes; namcap warns only of no PIE and of
unused loader links.

Status: in force.
Retires when: the engines' pins move or the package layout changes.

**PK-R-20 (ruling): `daisugi-ml` is its own package, compiled and
read-only.** It was a split of the source package `opendaisugi`, so
building it built Go, Rust and Z3 first. It is now
`packaging/aur/daisugi-ml`, arch any, built from the checkout alone, and
`opendaisugi` is a single package. package() lays the train pack out
with `scripts/system-pack.py` and compiles the worker (plain and `-O`),
so a job never tries to write `__pycache__` under `/usr`. The
`pack remove` of a system pack touches only the data dir, as before. New
golden case `system-pack-read-only`: the system pack compiled and
read-only, then `pack list`, `pack status`, `pack run train selftest`,
`lora train`, an offline install from a bundle into the data dir, and
`pack status`; the tree is recorded after. Python, Go and Rust agree on
all 25 pack cases. `tests/test_pack.py` checks that nothing under the
read-only pack changes. The offline bundle path needs nothing from the
package layout: the Go and Rust binaries carry the catalog, the locks
and the worker, and the Python package carries them in the wheel
(`opendaisugi/pack/packs`).

Status: in force.
Retires when: the system pack layout changes.

**PK-R-21 (ruling): the release dry run, and what the packages carry.**
`pkgver` stays 0.43.0 in every PKGBUILD: it is the version in
`pyproject.toml`, and a release sets both at once (a test checks they
agree). It is not a stale number to bump alone. Each package dir carries
a `.SRCINFO` from `makepkg --printsrcinfo`; a test fails when a PKGBUILD
changes its version, depends, makedepends or sums without a new one.

The dry run on 2026-10-08 (no tag, no upload): `scripts/release.sh
0.50.0-dryrun --rust` used the cached Z3 (the Rust build took 2m45s).
makepkg ran in an Arch container (podman, rootless, `--nodeps`) for
`opendaisugi-bin` (from the two dry-run tarballs), `daisugi-py` and
`daisugi-ml` (from a local clone at a91d11c7); `opendaisugi-voice` was
built in PK-R-19. The installed `daisugi`, `daisugi-rs`, `coppice` and
`sprig` each print 0.50.0.dryrun on Arch. The source package
`opendaisugi` was not built: its build() runs release.sh in `$srcdir`,
which compiles Z3 twice from source, and this box's rules forbid that;
it waits for a box or a CI job with the time and memory.

namcap findings acted on: the Rust binaries link libgcc_s, which Arch
now ships as `libgcc` (split out of `gcc-libs`), so `opendaisugi` and
`opendaisugi-bin` depend on glibc and libgcc; `daisugi-ml` depends on
python-numpy (the VLA oracle imports it); `daisugi-py` lists
python-sounddevice (AUR) as optional for push-to-talk and its pkgdesc
no longer names the package. Accepted as they are: the Go binaries have
no PIE and no full RELRO (static Go links), the Rust binaries name the
loader they do not use, the -bin PKGBUILD names x86_64 (the only arch),
no Maintainer tag (the owner adds one when publishing), and namcap's
"may not be needed" and Python-module lines, which come from building
with `--nodeps` where the Python deps are not installed.

Status: in force.
Retires when: a release sets pkgver, or the package set changes.

## 2026-10-08: the ruled refusals retired (Go and Rust read what Python reads)

The owner asked why Go and Rust sometimes refuse input that Python
accepts. Python leans on libraries that read odd forms (json and
ast.literal_eval, sqlite3, PyYAML, tomllib, httpx, huggingface_hub); the
ports read the common forms by hand and exited 2 on the rest. These
entries record what each port now reads, and what it still refuses.

**RF-1 (ruling): a plan step given as a string is read as coerce_step
reads it.** Both ports decode a str step wherever a plan is read (a plan
file, a pathway store, a bundle, a trace, a model's reply, an MCP call):
JSON first, then a Python literal. Go `pyjson.LiteralEval` and Rust
`pathways::literal` translate Python 3.12's tokenizer (numbers, strings
with every prefix, escapes and implicit concatenation, brackets to 200
levels) and `ast.literal_eval`: dicts, lists, tuples, str, int of any
base and size, float, True, False and None. Each item goes through
coerce_step before the after-validator, so stored input now takes the
oracle's order too (GD-R-6), and `RefinementRecord.step` decodes a str
step as well. `clients/literal_cases.py` records decode_dict_text's
answer for 3,084 texts (a fixed list and seeded mutations); both ports
give the same dict or None on every one they answer
(`pmodel` tests in both ports).

Still refused (exit 2, nothing changed), and only where the step is used:

- a text whose tokens make Python's parser print a SyntaxWarning on
  stderr: an invalid escape such as `\d`, an octal escape above 0o377, a
  number run into a keyword, and any f-string;
- a decoded step that holds a set, bytes, a complex number, Ellipsis, a
  `\N{...}` escape or a key that is not a str;
- a decoded step that holds a tuple and fails its validation (the error
  would name the tuple, which the ports read as a list);
- a step whose "type" is a list or a dict: Python's registry lookup
  raises TypeError out of the model (a traceback). This held for a dict
  step too, which the ports read as no step before.

Where Python returns None and prints nothing, the ports may still refuse
on these grounds (74 of the 2,682 such texts in the corpus); they never
give another answer.

New cases: pathway `show plan string step ...` (7; `warns` and `set`
refused as above) and `import string steps`; CLI `verify string steps`,
`verify string step not a step`, `verify string step warns` (refused);
k2 `run string steps`; garden `tend string steps` (traces and the
model's reply). A ruled refusal in the pathway and CLI compares may now
be worded either way ("is not in this binary yet." or "Nothing was
changed.").

Status: in force.
Retires when: the ports print Python's SyntaxWarning text and model sets,
bytes and complex numbers.

**RF-2 (ruling): a number past 64 bits in a plan or an envelope is kept
as Python keeps an int.** Since GD-R-3 both ports hold such an int as
its exact text wherever pydantic keeps it (step metadata, a step's
`seed` or `max_actions`, `max_output_size_mb`, a postcondition's
`expected`, `min` or `max`), send `max_execution_time_s` to Z3 as its
text, and hold any other one at the nearest 64-bit bound in the
verifier's typed copy, which keeps its order against every number that
fits. What still refused was the violation such a number most often
makes: `Envelope is internally inconsistent` (and `Plan requirements
contradict envelope permissions`) had no detail in the ports, so `verify`,
`run` and `orchestrate` refused it as a violation they did not word
(K2-6). The oracle's detail is `{"unsat_core": str(solver.unsat_core())}`,
and with no tracked assertions z3 prints `[]`; both ports now write that,
as the gate already did. New cases: CLI `verify big ints` and `verify big
ints json`; pathway `show big ints`, `show big ints text`, `import big
ints`; k2 `run big ints`. All agree in both ports.

Status: in force.
Retires when: does not retire.

**RF-3 (ruling): a BLOB column and a database path holding `?` are read
as Python's sqlite3 reads them.** A path: `sqlite3.connect(path)` takes
the name as a plain file name (no URI, no options; the oracle passes no
`uri=True` except for its own read-only opens, which escape the path).
The Go driver read a `?` in its DSN as the start of options, so the Go
binary refused; it now opens every database as a `file:` URI of the
absolute path with each special character escaped (`sqlpath.DSN`), so
`?`, `#`, `%` and a leading `file:` are part of the name. The Rust binary
read `?` already (PW-R-4); its envelope cache no longer refuses one. A
BLOB: Python's sqlite3 gives bytes, and the pathway store's readers take
bytes as the oracle does. pydantic reads bytes for a str, an int or a
float (and `model_validate_json` for JSON) as their UTF-8 text and raises
a ValidationError when they are not UTF-8; `json.loads` reads bytes in
the encoding `json.detect_encoding` picks (UTF-8 with or without a BOM,
UTF-16, UTF-32, decoded with 'surrogatepass'; a failed decode is
UnicodeDecodeError); `find` admits a row whose provenance values are
both falsy (an empty BLOB too) and otherwise needs both to be the
current str values, so a BLOB name is stale. The harness lays out and
dumps a BLOB as `{"blob": hex}`, and its own read-only opens now escape
the path too (they opened another file for a path holding `?`).

Still refused: `tend` re-embedding a stale row whose id or task is not
text (the oracle passes the bytes to its embedder and to the UPDATE).

New cases: pathway `list blob ...` (7), `show blob every column`, `list
data dir question mark`, `file prefix`, `percent and hash` (with and
without a store), find `lexical blob columns`; k2 `run data dir with a
question mark` (the journal, the store and the envelope cache under it).

Status: in force.
Retires when: does not retire.

**RF-4 (ruling): the stale-embeddings warning is printed, not refused.**
`PathwayStore.find` warns once per process (a UserWarning) when 10% or
more of the store was embedded under another model. Python prints it on
stderr as `<file>:<line>: UserWarning: <text>` and the source line of
the frame it names, a frame of the oracle's own install (here
`concurrent/futures/thread.py`, since the orchestrator runs find in a
thread). Both ports print `UserWarning: <text>` at the first find of the
run (the envelope generation's, or the orchestrator's), and the k2
compare reads the oracle's stderr in that form (`k2_cases._warnings`).
PYTHONWARNINGS is read as `warnings._processoptions` reads it: comma
separated `action:message:category:module:lineno`, the action by prefix,
the message a case-blind prefix, the last matching option first. Still
refused (exit 2, nothing changed): an invalid option (Python prints a
note at start), the `error` action for the warning (it becomes an
exception the orchestrator logs and swallows), a filter that names a
module or a line, a category that is not a builtin, and a filter that
shows a category beyond UserWarning (Python then shows the
DeprecationWarnings its libraries raise, which a port cannot know). New
cases: k2 `orch stale embeddings warns`, `default filter`, `message
filter`, and `error filter` and `all warnings` (refused); `orch stale
embeddings warn` (PYTHONWARNINGS=ignore) now agrees. This covered
`orchestrate` only; RF-11 extends it to every other caller of find.

Status: in force.
Retires when: the ports model where Python's own libraries warn.

**RF-5 (ruling): a Switchyard config is read as tomllib reads it.** Both
ports carry a translation of Python 3.12's tomllib (`_parser.py` and
`_re.py`), one function at a time (Go `switchyard/toml.go`, Rust
`switchyard/toml.rs`): every statement rule, the flags that forbid a
second declaration, multi-line basic and literal strings with their
escapes and line-ending backslash, arrays of tables, multi-line arrays
with comments, inline tables, dates, times and date-times (to the
microsecond, with Python's repr and str of each), ints of any size and
floats with underscores, inf and nan. The brief preferred a library;
none was in go.sum or Cargo.lock, and BurntSushi/toml and the `toml`
crate differ from tomllib at the edges (an int past 64 bits, key order
inside arrays of tables, TOML 1.1 forms behind an environment variable),
so the ports translate tomllib, as YM-1 translates PyYAML, and add no
dependency. The file is read as `read_text` reads it: UTF-8 (else the
oracle's UnicodeDecodeError path) with universal newlines, also when the
gateway copies the user's file for the child. A file tomllib rejects is
the oracle's SwitchyardConfigError (exit 1). The repr of a value in an
error (`names target datetime.date(1979, 5, 27), which is missing`) and
str() of a value (`api_key_env` given as a date-time) are Python's; a
`llm_client` that is another hashable type is no client, as `dict.get`
finds none. `clients/toml_cases.py` records tomllib's answer on 3,052
texts (a fixed list and seeded mutations); both ports give the same
document or the same error class on every one. New gateway cases:
`switchyard own config full toml` (CRLF, multi-line and literal strings,
dates, arrays of tables), `key env a date`, `big int`, `target named by
a date`, `not utf-8`; `not toml` now agrees.

Status: in force.
Retires when: tomllib changes.

**RF-6 (ruling): what stays of PX-9's refusals.** A proxy port written
in the decimal digits of another script (Unicode Nd, as Python 3.12's
unicodedata has them: fullwidth, Arabic-Indic and the rest) is read as
`int()` reads it, so it is the port it names (case `env proxy port in
fullwidth digits`, both ports). An "Invalid port" error quotes the port
as the URL wrote it, as httpx does (`Invalid port: '８x'`), not the digits
read from it (fix round 1, S8; a unit test in each port). The rest of
PX-9's list stays refused,
each for a reason outside what a port can model the oracle's way on any
box:

- a port of 0, below 0 or above 65535: httpx passes it on, and where the
  connection fails, or which port it reaches, is decided by the C
  library's getaddrinfo (glibc folds 70000 to 4464 and answers -1 with
  EAI_SERVICE), not by Python;
- a proxy host httpx sends percent-encoded, and an IPv6 address with a
  zone: the name goes to the system resolver or to an interface of this
  box;
- an IDNA host outside the letters the ports encode, a label already in
  `xn--` form, and a NO_PROXY entry in `xn--` form: httpx uses the `idna`
  package (IDNA 2008 with its own tables and contextual rules), which
  neither Go's nor Rust's standard library carries; a full port of it is
  more than this brief's day;
- a request to a non-ASCII host through a proxy: every place the ports
  write the request line and Host header would need that IDNA form.

Status: in force.
Retires when: the ports carry the `idna` package's tables, or httpx's
URL parser changes.

**RF-7 (ruling): `models pin` does what huggingface_hub does, `--pull`
included.** Both ports (Go `cli/modelspin.go` and `cli/hubclient.go`,
Rust `cli/modelspin.rs`):

- validate the repo id as `validate_repo_id` does, with Python's `\w`
  (a letter or number of any script, `str.isalnum`, or "_"), so a
  non-ASCII id is asked for (its bytes percent-encoded, as httpx sends
  them) and an id it rejects raises HFValidationError;
- raise what huggingface_hub raises, as an uncaught traceback with exit
  1 whose first exception is the one Python names first:
  OfflineModeIsEnabled under HF_HUB_OFFLINE, httpcore's ConnectError for
  a Hub that cannot be reached, httpx's HTTPStatusError for a status of
  400 or more (hf_raise_for_status raises from it), json's
  JSONDecodeError for a body that is not JSON, and the KeyError or
  TypeError of a tree entry or model info its classes cannot build;
- follow the tree's next pages (the Link header's `rel="next"`), the
  first with `session.get` and each next one with `http_backoff`: a 429
  or 5xx is asked again up to five times, waiting 1, 2, 4, 8 and 8
  seconds, or the Retry-After seconds plus one, and each wait is logged
  on stderr in huggingface_hub's words;
- with `--pull`, write the cache as `hf_hub_download` lays it out:
  HF_HUB_CACHE, HUGGINGFACE_HUB_CACHE or <HF_HOME>/hub; a HEAD of the
  file's resolve URL at the pinned commit (X-Repo-Commit, X-Linked-Etag
  or ETag, X-Linked-Size or Content-Length); the blob downloaded to a
  `<etag>.<hex>.incomplete` file and moved to `blobs/<etag>` with a new
  file's mode; `snapshots/<commit>/<file>` a relative symlink to it;
  `refs/<revision>` only when the revision is not the commit (here it
  always is, so no ref); `.locks/<repo folder>/<etag>.lock` (mode 0664)
  and `CACHEDIR.TAG`. No telemetry header is sent (MP-3). Progress goes
  to stderr only when stderr is a terminal (huggingface_hub draws none
  there in the cases). The compare checks the tree (symlinks by their
  target) and then has Python find each pulled file with
  `hf_hub_download(..., local_files_only=True)` under HF_HUB_OFFLINE.

Still refused, never guessed: a model info date `parse_datetime` might
read another way (it uses strptime), a tree entry with its last commit
or a security status, a model info with eval results or
inference-provider mappings, a redirect, a file in Xet storage, a HEAD
answer huggingface_hub would retry or fall back to the cache on (a 429 or
5xx, a missing commit, etag or size, a 404 that names the commit), a
network error on a next page (http_backoff retries it in httpx's words),
and a "$" in a cache path (expandvars).

New models cases: `pin paged tree two pages`, `rate limited`, `retry
after`, `pin non-ascii repo`, `pin repo ends in a mark`, `pin repo of
digits of another script`, `pin pull writes the cache`, `pin pull json`,
`pin pull lfs`; `pin pull` now answers its HEAD with 404. 107 cases: 106
agree in both ports, 1 refused (`pin info bad date`); CI's ceiling drops
from 12 to 1. The fake Hub answers HEAD, a sequence of answers per
target, and writes its own port into a header.

Status: in force.
Retires when: does not retire.

## 2026-10-09: fix round 1 (review of the refusal rows)

**RF-8 (fix, B1): a literal int past 4300 decimal digits does not
decode.** Python 3.12 reads at most `sys.int_info.default_max_str_digits`
(4300) decimal digits into an int: a longer decimal literal is a
SyntaxError, so `decode_dict_text` gives None and the step stays a str
(JSON says the same through json.loads). The literal readers had no
limit, so `verify` passed a plan the oracle rejects. Both now apply it
(underscores do not count; a run of zeros is 0). A hex, octal or binary
literal has no limit at parse, but its decimal text past 4300 digits
cannot be printed (str() raises), so such a value is refused where a
step uses it. New cases: CLI `verify string step int too long`, `verify
json step int too long`; eight corpus texts (after the seeded mutants,
so the rest of the corpus is unchanged).

Status: in force.
Retires when: Python's int limit changes.

**RF-9 (fix, S6 and S7): tomllib's words, and its ValueError past 4300
digits.** `tomllib.loads("n = " + "1" * 4301)` raises int()'s ValueError
("Exceeds the limit (4300 digits) for integer string conversion: value
has 4301 digits; use sys.set_int_max_str_digits() to increase the
limit"), not a TOMLDecodeError. The Switchyard readers catch only
OSError, UnicodeDecodeError and TOMLDecodeError, so the oracle exits 1
with a traceback whose first exception is ValueError. Both ports now
raise the same (Go `TOMLValueError`, Rust `TomlDecodeError::is_value_error`)
and print that traceback; hex, octal and binary ints have no limit, as in
int(text, 0). The corpus generator records `{"exc": "ValueError", "msg":
...}` for such a text, and now records `str(exc)` of every error, so the
3,056 texts check tomllib's message with its line and column, not only
the class (RF-5 said the words match; now the corpus proves it). New
gateway case: `switchyard own config int too long`.

Status: in force.
Retires when: tomllib or Python's int limit changes.

**RF-10 (fix, S1 to S5): `models pin` and `--pull` on the network.**
Amends RF-7.

- A redirect is not followed, by either port (Go's client had followed
  up to 10, to any host, for GET and HEAD; Rust followed none). Python
  follows the tree's and model info's redirects, and for `--pull` reads
  the HEAD's metadata off a redirect to another host (a CDN) and follows
  one to the same host. A port that gets a 3xx refuses, before anything
  is written. New cases: `pin tree redirects`, `pin pull head
  redirects` (both refused; CI models ceiling 1 to 3).
- `--pull` from huggingface.co refuses every LFS file and every Xet file,
  which is every GGUF: the Hub answers their resolve URL with a redirect
  to its CDN. It works against a Hub or mirror that serves the file at
  its resolve URL. This is the ruled state, said plainly in
  docs/client-diversity.md.
- Only a refused connection (ECONNREFUSED) is printed as httpcore's
  ConnectError ("[Errno 111] Connection refused"). Any other network
  failure (a name that does not resolve, TLS, a connection that breaks
  or times out) is refused: huggingface_hub names it in httpx's words,
  which the ports do not carry. A unit test in each port (a closed
  connection is not a ConnectError).
- The download is written to the `.incomplete` file as it arrives (Go
  io.Copy, Rust `llm::http::request_to`), so a file of gigabytes costs a
  buffer, not its size. Python does not hash the file, so neither port
  does; the size is counted.
- After the cache is touched, Python raises what http_get raises and the
  ports now do the same: a size that differs from the HEAD's is OSError
  ("Consistency check failed: file should be of size 10 but has size 9
  (...)"), a status of 400 or more is httpx's HTTPStatusError, both exit
  1 with the temporary file removed (as `_download_to_tmp_and_move`
  does), the folders, lock file and CACHEDIR.TAG left as Python leaves
  them. New cases: `pin pull size differs`, `pin pull download not
  found`. A refusal after a write (a body cut short, which
  huggingface_hub resumes; a redirect on the GET; a disk error) no longer
  says "Nothing was changed.": it says the Hugging Face cache may hold
  folders and a lock file from this pull.
- A Retry-After in non-ASCII digits (str.isdigit reads every script) or
  of more than 9 digits is refused (Go's Atoi had wrapped to a negative
  wait). An etag or commit of "." or ".." is refused before any write.
- As in Python, and so not refused: a next-page Link that points back
  loops for ever, and the bearer token goes to whatever host a next-page
  Link names.

Status: in force.
Retires when: the ports follow redirects by huggingface_hub's rules and
read Xet storage.

**RF-11 (fix, S9): the stale-embeddings warning in every caller of
find.** Python's `PathwayStore.find` warns in every caller, once per
process. Checked in the oracle: `route` (RouteAdvisor.advise),
`distill-repeats` (rank_reuse_candidates, one find per repeated ask),
and the MCP server's `find_pathway`, `recall` (gateway_recall.recall)
and `envelope_for` (envelope generation looks the task up first) tools
all reach it; `tend` re-embeds stale rows and
does not call find (the Go file named `tendcmd.go` holds
`distill-repeats`). Both ports now print `UserWarning: <text>` once in
each, with RF-4's PYTHONWARNINGS reading. `route` and `distill-repeats`
refuse a filter RF-4 does not model, before they print or write
anything. The MCP server's stderr is its log, which no client reads and
the k3 compare does not record: there a filter not modelled prints
nothing, not a refusal, which would end a tool call over a log line. The
gateway and garden compares now read a warning as k2 does
(`file:line: UserWarning: text` and its source line become
`UserWarning: text`). New cases: gateway `route stale store`, `route
stale store json` (both ports failed first); garden `repeats stale
store`. Unit tests in Go for the MCP server's once-only warning.

Status: in force.
Retires when: the ports model where Python's own libraries warn (RF-4).

## 2026-10-09: fix round 2 (re-review 1 of the refusal rows)

**RF-12 (fix, B2): a YAML int past 4300 decimal digits does not load.**
PyYAML's `construct_yaml_int` calls `int(value)` (after it drops `_` and
the sign), which raises ValueError past 4300 decimal digits. The plan or
envelope then does not parse: exit 2, "It did not parse: Exceeds the
limit (4300 digits) for integer string conversion: value has N digits;
use sys.set_int_max_str_digits() to increase the limit". Both YAML
readers (Go `pyyaml.yamlInt`, Rust `pyyaml::yaml_int`) had no limit, so
`verify` passed a plan or envelope the oracle rejects. Both now raise
the same ValueError. Hex, octal and binary go through `int(v, base)`,
which has no limit at parse; a base-60 part that long is already
refused. A plan file in JSON is read as YAML, so it is covered too. New
CLI cases: `verify yaml plan int too long` (and `underscores`), `verify
json plan int too long`, `verify yaml envelope int too long` (both ports
failed first), `verify yaml plan int at the limit` and `verify yaml
plan hex int long` (both agreed before and after); five YAML corpus
texts.

Status: in force.
Retires when: Python's int limit changes.

**RF-13 (fix, B3): numbers compare as Python compares them.** Python's
`==` compares two ints exactly and an int with a float exactly. Go's
verifier decoded every step number to a float64 and compared those, so
two ints past 2^53 that round to the same float64 were equal: `equals`
and `in_set` allowed a plan Python denies (`metadata.n` 2^53+1 against
2^53; 2^64+5 against 2^64), and `not_equals` denied what Python allows.
Go now keeps an int that no float64 holds exactly as its exact text
(`exactNumbers`), and `pyEqual` compares by Python's rules (`pyNumCmp`:
big ints, a float against an int by exact rational value, NaN equal to
nothing). Fields pydantic declares as float on robot steps are read as
floats in the evaluator's step (pydantic rounds an int there).
`numeric_range` is `min <= float(val) <= max` as in Python: a bool
counts, an int is rounded, an int past the float range raises
OverflowError. The stage-2 `exit_code` handler is `rc == expected` by
the same rule (it had truncated a float rc: 3.5 met an expected 3). The
Z3 literal of such an int is exact. The Rust verify path already
compared exactly (`gate::predicate`); its older evaluator
(`crate::predicate`, used by stage 2 on completed steps, the conform
client and subsumption) compared as f64 and now compares ints exactly
too; it holds ints only to 64 bits, so stage 2 refuses to compare an
int past 64 bits (a violation in `verify_completed_step`, a refusal in
the CLI's stage-2 check), which fails closed. New CLI cases: 14 `verify
big int ...` rows (2^53+1 against 2^53, 2^64+5 against 2^64 in `equals`
and `in_set`, the negatives, `not_equals` and `not_in_set`, and int
against float pairs); Go failed 11 of them first, Rust none. Thirteen
conformance verify cases in `clients/fixtures/dialect` (ints to 64 bits,
which every client's JSON reader holds): the old Rust conform client
failed 10 of them, Go none (its conform shares the fixed package). Unit
tests in both ports.

Found by the same search: a `length_range` bound with a fractional part
(`min: 2.5`) is pydantic's ValidationError, a traceback out of verify. Go
cut it to an int and verified (2.5 became 2, so a plan passed that the
oracle stops); Rust's older parser read it as no bound and its verify
path then refused with a many-line message. Both now treat it as an
expr they do not read and refuse in one line, before anything runs, as
K2-8 rules. New CLI cases `verify length range fractional min` and
`max` (ruled refusals; a probe of the old Go binary printed "Verification: OK" on the min case).

Status: in force.
Retires when: never (Python's numeric tower).

## 2026-10-09: fix round 3 (re-review 2 and the subsumption numerals)

**RF-14 (fix, B4 and the subsumption numerals): Z3 numerals and stage-2
ints are exact.** Python's subsumption and vacuity build Z3 numerals with
`_z3_lit`: an int is `z3.IntVal(n)` (exact) and a float `z3.RealVal(f)`,
which Z3 reads from `str(f)`, the shortest digits (1e30 is 10^30, not the
float's binary value); a `numeric_range` bound is a pydantic float, so
2^53+1 there is already 2^53 in Python too.

- Rust `z3_bridge` printed every number through f64 (`as_f64`), and a
  whole float with all its binary digits. So two ints past 2^53 got one
  numeral, and a skill's contract could be proved inside a caller's
  envelope it is not inside (verify's skill delegation; the delegation
  tree's `edge_ok` for `tree check|spawn` and `gate register --parent`).
  Now an int to 64 bits is its exact text, and a float its shortest
  digits. An int past 64 bits, which the typed copy reads as a float, is
  refused: CLI verify with a skill contract refuses before anything runs
  (past i64, where `to_serde` clamps), and `edge_ok` adds the reason
  "proof: an int past 64 bits, which this binary does not prove over
  exactly" (fails closed; Python may prove the edge).
- Go kept any int a float64 held exactly as a float64,
  and printed it with its shortest digits (2^60 as 1152921504606847000).
  Go now keeps every int past 2^53 as its exact text.
- B4, Rust stage 2 (MCP `verify_completed_step`, the run supervisor):
  `to_serde` clamps an int past i64, and the guard looked only past u64,
  so 2^63+5 equalled 2^63-1 and 2^64-1 read as 9.2e18 in
  `numeric_range`. Stage 2 now holds every int to 64 bits exactly, signed
  or not (`to_serde_u64`), and refuses one past 64 bits. Go's stage 2
  reads the step through `exactNumbers`, so it holds them exactly.

Cases: 7 conformance verify rows with a skill contract (the old Rust
conform failed 4, the old Go conform 1: 2^60 against 1152921504606847000);
7 k3 `mcp step big int` rows (old Rust failed 6; now Rust agrees on 6 and
refuses the one past 64 bits; k3 CI ceiling 2 to 3); 4 tree `check numbers
past 2^53` rows (all agree before and after: a child keeps the parent's
invariants word for word, so its own numbers cannot widen it there); 10
CLI `verify delegation` rows (both ports refuse each, before and after:
the delegation's unverified-invariant warning is not worded by the ports,
an older ruling). Unit tests: Rust numerals, Go numerals (the old Go
failed).

Status: in force.
Retires when: the Rust typed copy holds ints of any size.
## 2026-10-09: sprig as an agentic executor (Python, Go, Rust)

The owner ruled on 2026-10-07 that sprig becomes an agentic executor now,
in all three languages, and that Go and Rust stop refusing an agentic
step. These entries record the rulings for this chunk and how each port
copies the oracle.

**SX-R-1 (ruling): the runtime is an `--agent claude|sprig` option on
`weave`, `run` and `orchestrate`, default `claude`.** It is a setting of
the executor, not a step field, so plans, dumps, the schemas and the
pathway cases do not change. Another value exits 2 with one line,
`Invalid --agent 'pi'; choose from ['claude', 'sprig'].`, checked first in
the command body (after the option parser's own checks). `run` and
`orchestrate` now wire `AgenticExecutor` with the caller's envelope, as
`weave` does, so a reused delegated pathway runs its agentic leaves.
Cases: weave `agent flag bad value`; k2 `run agentic step runs`, `run
agentic step sprig`, `run agentic dry run`, `orchestrate reuses delegated
pathway` and its sprig twin. `run --dry-run` covers an agentic step: `DryRunExecutor` prints
`[dry-run] would agentic: <prompt repr> in <workspace> tools=<tools
repr>` and starts nothing. K2-7 retires with this entry. The owner flips
the default to sprig after the dogfood runs.

Status: in force.
Retires when: the owner changes the default or the option.

**SX-R-2 (ruling): the sprig runtime keeps every guarantee of PG-3.** The
workspace check, the edge proof, the child registered 0600 in a fresh
0700 gate root in `tempfile.gettempdir()`, the session
`agentic-<safe step id>` and the failure texts before the sub-agent starts
are the claude runtime's. sprig then runs as `<DAISUGI_SPRIG or sprig>
--json --gate --gate-cmd "<gate> --mode enforce --root R --session S
--captures-root R/captures" --tools <wall> --model haiku --session-dir
R/sessions --session S [--max-turns N] -`, in the workspace, the prompt on
stdin, in a process group of its own. `<gate>` is the program the hook
runs: `<python> -m opendaisugi.gate_client` in the oracle, `<self> gate
check` in the ports, with no shell, no marker word and no `|| exit 2`
(sprig denies on any non-zero exit). The gate is pinned by the command;
a payload that names another session meets the pinned session's
envelope. sprig's session file stays in the kept gate root.

Status: in force.
Retires when: does not retire.

**SX-R-3 (ruling): the wall on sprig.** The step's tools that the child
backs, then Read, Write, Edit and Bash mapped to read, write, edit and
bash, once each, in the step's order. A tool sprig has no tool for (Glob,
Grep, MultiEdit, WebFetch, WebSearch) is left out, never mapped onto a
wider tool such as bash. When none is left the step fails with the same
"no requested tool is backed by the envelope ... nothing to delegate" and
nothing starts.

Status: in force.
Retires when: sprig gains a tool for one of the left-out host tools.

**SX-R-4 (ruling): a gate command word with whitespace fails the step.**
sprig splits `--gate-cmd` at whitespace (Go's `strings.Fields`). When the
gate program's path or the temp directory holds a character Go counts as
whitespace, the step fails before anything is made or started with
"the sprig gate command has a word with a space in it, and sprig splits
that command at spaces. Use a TMPDIR path and a daisugi path with no
spaces." No quoting change to sprig.

Status: in force.
Retires when: sprig takes its gate command as separate words.

**SX-R-5 (ruling): the texts of a failed sprig run.** Each is a failed
step (rc 1), written once in the oracle and copied by the ports:
`agentic sub-agent failed: cannot start sprig (<binary>)`, `... sprig ran
past <N>s and was killed` (the whole process group gets SIGKILL at the
step timeout), `... sprig was killed by signal <n>`, `... sprig exited
with code <rc>` with `: <last non-blank stderr line, 500 characters>`
when there is one, `agentic sub-agent returned unparseable output:
<repr of the first 300 characters>`, and `agentic sub-agent reply has no
answer: <repr>` for JSON that is not an object or has no str `answer`.
No odd reply raises out of the executor, unlike the claude runtime.
Tokens are the sum of the int values in `usage`; `cost_usd` stays None.

Status: in force.
Retires when: does not retire.

**SX-R-6 (ruling): sprig's `--tools`, `--model` and `--json` usage.**
`--tools LIST` (comma list of read, write, edit and bash; default all
four) sets the tools the prompt and the API request offer; a call to
another tool is refused as an unknown tool; an empty list or another
name exits 2 with one line. `--model M` goes to `claude -p` as
`--model=M` (default haiku) and is used over SPRIG_MODEL on the API.
`--json` adds `model` (the model asked for) and `usage`: the four token
counts of all turns added up, under the Messages API names; a count that
adds up to zero is left out, since sprig's Usage holds no "not reported"
mark. Go and Rust sprig agree on 193 sprig cases (8 new).

Status: in force.
Retires when: does not retire.

**SX-R-7 (harness): the fake sprig and the real one.** The agentic fakes
moved to `clients/agentic_fakes.py`, which the weave and K2 suites both
import (weave_cases imports k2_cases, so the fakes could not live in
weave_cases without an import cycle). A case's `"sprig": "fake"` puts
FAKE_SPRIG on PATH: it logs the argv with the gate program as `<GATE>`
and the gate root as `{GATEROOT}`, the cwd, the sprig variables, the
prompt and the registered envelopes, sends each scripted call to the
`--gate-cmd` words as sprig does, logs each verdict, and prints the
scripted reply. `"sprig": "real"` runs the sprig binary given with
`--sprig BIN` (weave_cases, weave_compare, k2_compare) against the fake
claude; the compares skip those cases without it. The fake claude's key
now reads the prompt with HOME and any agentic gate root masked, since a
real sprig puts tool results that name them in its prompt; no earlier
case had either in a prompt, so no key changed. Weave cases `sprig
agentic *` (18 kind A, 2 kind B: `real read`, `real deny`). Red first:
master's Go and Rust binaries disagreed on exactly the 20 new weave and
5 new k2 cases. After: Go and Rust each agree on 143 of 143 weave cases
and 161 of 164 k2 cases (3 not ported as ruled), run once with the Go
sprig as the real sprig and once with sprig-rs; the CLI compare stays at
0 disagreements.

Status: in force.
Retires when: does not retire.

**SX-R-8 (ruling, Go and Rust): how the ports run sprig.** Go
(`supervise.runSprig`, `runGroup`) and Rust (`supervise::agentic_sprig`)
start sprig with its own process group (Setpgid, `process_group(0)`),
the run's environment, and the workspace as cwd; a bare name is looked up
on the run's PATH, and a name with a slash is used as given. As in the
oracle's `communicate(timeout=...)`, the run ends when sprig has exited
and its pipes are closed; a process sprig left that still holds a pipe at
the step timeout makes it a timeout, and the group is killed. The gate
root's name comes from each port's own mkdtemp (it is masked in every
case). The wall check for whitespace uses Go's `unicode.IsSpace` and
Rust's `char::is_whitespace`, the same set the oracle names
(`_GO_SPACE`). The ports compute no token count: no compared output
shows the executor's meter.

Status: in force.
Retires when: does not retire.

**SX-R-9 (ruling, fix rounds 1 and 2): the Python gate never imports from
the agent's working directory.** Both runtimes run the oracle's gate in the
sub-agent's workspace: Claude Code runs a hook in the session's
directory, and sprig runs its `--gate-cmd` in its own cwd. `python -m`
puts the cwd first on `sys.path`, so a sub-agent allowed to write its
workspace could plant `opendaisugi/gate_client.py` there and allow every
later call (shown with a planted module that prints and exits 0). The
gate program is now `<python> -I -m opendaisugi.gate_client` in
`gate_settings_json` (the claude hook of an agentic step, `daisugi gate
settings`, and the hooks `daisugi install --gate` writes for Claude Code
and Codex) and in the sprig gate command. Round 1 used `-P`, which drops
only the implicit cwd entry; the agent's environment reaches the gate on
both paths, and an empty or relative PYTHONPATH entry (`.`, `:/x`, `/x:`,
`/x::/y`) still put the workspace on `sys.path`, where a planted
`gate_client.py` or `sitecustomize.py` ran. `-I` (isolated) drops the
cwd, every PYTHON* variable and the user site; the interpreter's own
site-packages, an editable install's `.pth` among them, still resolve the
installed gate. A re-install brings an older hook (no flag, `-P`, or the
old `opendaisugi.gate` module) up to today's command in place, for Claude
Code and for Codex. An older hook that differs in mode, root or python is
left alone, as before, and the warning now says it runs without `-I` and
how to replace it, when a gate hook there runs the Python module
(`-m opendaisugi.gate...`) without `-I`; Go (`install/isolated.go`) and
Rust (`cli/install.rs`) add the same words on the same test (cases
`install gate other mode`, `install gate old module`, `install gate beside
unknown`). Other places that start
`python -m opendaisugi...`: `daisugi start` spawns `-m opendaisugi.cli
gate serve` and the TUI spawns `-m opendaisugi.cli dashboard`, both in the
operator's directory, not an agent's; the pi and OpenCode extensions talk
to the gate socket and start no Python. The ports run `<absolute self>
gate check`, which loads nothing from the cwd. The compare harnesses mask
the gate program (`{GATE}`, `<GATE>`), so no recorded case changed;
`norm_command` reads the `-I -m` form. Tests: tests/test_gate_cwd.py
(both runtimes with a planted gate and sitecustomize under no PYTHONPATH
and the four forms above, the hook command, the installed gate found under
`-I`, the upgrades from no flag and from `-P`, the warning), red before
each fix. Not covered (parked by the coordinator): a workspace whose write
scope holds the gate's own code (an editable `src/` or a venv's
site-packages) can still change the gate.

Status: in force.
Retires when: does not retire.

**SX-R-10 (ruling, fix round 1): the read after the timeout kill is
bounded, and a leftover pipe ends as a timeout.** After the group kill at
the step timeout, all three read the rest of sprig's output for at most
`SPRIG_DRAIN_S` (2 s), then stop: a process that left the group (setsid)
and holds a pipe no longer keeps the step past its timeout. A process
sprig leaves in its group that holds a pipe after sprig exits makes the
run a timeout, as SX-R-8 says. Tests in all three: sprig answers, starts
`sleep 60` on its stdout (in the group, and with setsid), and exits 0; the
step fails with "sprig ran past Ns and was killed" within the timeout
plus the drain. CI now runs the two real-sprig weave cases in the
sprig-rs job, each daisugi with each sprig; `weave_compare --sprig` fails
when no real-sprig case ran.

Status: in force.
Retires when: does not retire.

## 2026-10-09: the data home (OPENDAISUGI_HOME, XDG_DATA_HOME)

**DH-R-1 (ruling): one rule picks the data directory in every language.**
When no flag names it, the data directory is `$OPENDAISUGI_HOME` when that
is usable; else `$XDG_DATA_HOME/opendaisugi` when XDG_DATA_HOME is usable
and `~/.opendaisugi` does not exist; else `~/.opendaisugi`. A value is
usable when, after a leading `~` or `~/` is replaced by the home directory,
it is an absolute path; an empty, relative or `~user` value is ignored and
the next rule applies (fix round 1: a quoted `~/od` or a relative value
would have put the gate root under the hook's cwd, the agent's workspace,
where the guard did not look). The XDG spec says the same of a relative XDG
path. An existing install keeps its directory; nothing is moved. One helper holds the rule in each language and module:
`opendaisugi/datahome.py`, `clients/go/internal/datahome`,
`clients/rust/src/datahome.rs`, `harness/coppice/internal/datahome` (the
foreman's directory now calls it, with its behaviour unchanged) and
`harness/coppice-rs/src/datahome.rs`. The pi extension and the OpenCode
plugin apply the same rule to find the gate socket (OPENDAISUGI_GATE_SOCK
still wins). sprig names no data directory, so it has no helper. An
installed gate hook bakes an absolute `--root`; the Python installers
refuse a relative one (the Go and Rust CLIs already refuse a HOME that is
not absolute). Every default that named `~/.opendaisugi` (the gate root,
captures, config.yaml, the registry, the trusted signers, the coppice data
directory and fallback socket, every `--data-dir`) now asks the helper. The
compare harnesses run with no OPENDAISUGI_HOME and no XDG_DATA_HOME, so no
recorded case changed; the cli cases `data home ...` run each branch of the
rule, and each kind of unusable value (`~/od`, `od`, `./od`, empty), in all
three.

Status: in force.
Retires when: does not retire.

**DH-R-2 (ruling, gate): the gate guards every place the data home can
be.** The gate's own-state rule (`gate_state_rule.data_dirs`), the floor
rule's coppice data directories (`floor_config.coppice_data_dirs` and
`_roots`) and daisugi's config guard (`floor_config.daisugi_config_roots`)
named only `~/.opendaisugi`. With the data home moved by the environment,
an agent could have written the moved gate state or config.yaml. Each list
now holds `~/.opendaisugi`, `$OPENDAISUGI_HOME` and
`$XDG_DATA_HOME/opendaisugi` (each when set), with no exists check:
`datahome.guarded_data_dirs`. `floor_config._VARS` also expands
`$OPENDAISUGI_HOME`, so a shell line that names the moved home through the
variable is placed. Each change only adds denies; with neither variable
set nothing changes. Go (`runner.guardedDataDirs`) and Rust
(`Runner::guarded_data_dirs`) make the same change; the 15 gate cases
`data home ...` hold it, including the pane rule's secret reads (web token,
CA key) under a moved coppice data directory. Every existing gate case kept
its recorded answer; gate, cli and models compares: 0 disagreements, Go and
Rust, twice. Tests: tests/test_gate_state_guard.py,
tests/test_gate_daisugi_config_guard.py, tests/test_gate_floor_config.py,
red before the change.

Fix round 1: the pane rule's `_OTHER_SERVER` also refuses a leading
`OPENDAISUGI_HOME=` or `XDG_DATA_HOME=` assignment, as it refuses `HOME=`
and any `XDG_RUNTIME_DIR`: with no runtime dir, coppice's fallback socket
sits under the data home, so either moves an allow or deny to another
server. Gate cases `pane deny data home`, `pane deny xdg data home`, `pane
deny home`; `data home od tilde`, `od relative`, `od dot relative`, `od
empty`, `xdg tilde`, `xdg relative` hold the usable-value rule. Only
denies are added.

Status: in force.
Retires when: does not retire.
