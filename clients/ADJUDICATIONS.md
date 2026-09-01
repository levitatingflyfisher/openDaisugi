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

Status: in force.
Retires when: the binaries' YAML reader takes these shapes.

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

Status: in force.
Retires when: the binary reads a string step and a number past 64 bits the Python way.

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

Status: in force; its refusal of a number past 64 bits retired 2026-09-26 by GD-R-3.
Retires when: the binary reads a string step and an import's `llm_check` predicate the Python way.

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
binaries. Stored input (not a model reply) still takes the older order in
both binaries; no case reaches that shape.

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

Status: in force.
Retires when: the binaries read every TOML file `tomllib` reads.

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
binary. It refuses the same two cases, `report float tokens` (GW-8) and
`switchyard own config not toml` (GW-9), and no other. GW-13's
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

Status: in force.
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

Status: in force.
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

Status: in force.
Retires when: the Go YAML reader covers the YAML a user writes by hand and words
PyYAML's parse errors.

**K2-4 (ruling, Go): `orchestrate --max-parallel` above 1 is not in this binary
yet.** The oracle then prefetches a level's steps through `asyncio.gather`, so the
order of outcomes and receipts can change from run to run (1 case).

Status: in force.
Retires when: the binary runs a level's independent steps concurrently and the
compare reads receipts as a set.

**K2-5 (ruling, Go): a stale-embeddings warning from the pathway store is
refused before anything is written.** `PathwayStore.find` warns (a UserWarning,
whose text names the oracle's source file) when stored pathways were embedded
under another model. `orchestrate` reads the store without a write first and
refuses when find would warn (1 case).

Status: in force.
Retires when: the binary prints the warning as Python's warnings module does.

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

Status: retired for the oracle 2026-09-30; in force for `run` and
`orchestrate`, which still wire none.
Retires when: a CLI command wires an agentic executor. `daisugi weave` wires
`AgenticExecutor` in the oracle (WV-R-9); the ports do not port it and
refuse a plan with an agentic step (WV-R-10).

**K2-8 (ruling, Go): an envelope the binary cannot check the oracle's way is
refused before the run.** An invariant whose `expr` asks a model (`llm_check`),
and an enforced postcondition whose `expr` asks a model, names an alias, is not a
dict, or does not parse, are refused with exit 2 before anything is written (2
cases: an invariant on `run`, a postcondition on `orchestrate`). An envelope the
model wrote for `orchestrate` is checked the same way once it exists; that
refusal comes after the data directory is set up and is worded as K2-6. The three opaque types
(`exit_code`, `file_exists`, `file_size_range`) and predicate exprs run as in
`stage2.verify_completed_step`.

Status: in force.
Retires when: the binary evaluates `llm_check` and resolves aliases as the oracle
does.

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

Status: in force.
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

Status: in force.
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
`--remote` and `tiers stats` are not in this binary yet (2 cases). The
"all attempts errored" hint is unreachable in both: the Tier-1 provider
declines rather than raises.

Status: in force.
Retires when: the binary carries `--remote` and `tiers stats`.

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

Status: in force.
Retires when: the verifier keeps every predicate violation's detail.

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

Status: in force for the command-line `hook report` in both binaries; `hook list` and `hook to-trace` retired 2026-09-28 by stage F, for the Go binary and for the Rust binary.
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

Status: in force.
Retires when: the ports carry MiniLM, `--remote` and `tiers stats`.

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

Status: in force.
Retires when: the gate cases normalize a cut root.

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
anything is written, a plan with an agentic step (K2-7 holds in the ports),
`--max-parallel` above 1 (K2-4), a plan or state file that is not UTF-8, and
plan JSON their readers do not model (nesting past 900, an int past the
digit limit). Of 108 weave cases each agrees on 106 and refuses the 2
ruled.
The text of a start mark's write error is each language's own; no case
reaches it.

Status: in force.
Retires when: a port wires an agentic executor, or runs a level's steps concurrently.

**WV-R-11 (harness): earlier runs in a case.** A weave case may hold `pre`
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
card, from the ledger only: the receipts of `facts.run.run_id` for the steps
in `facts.run.downstream`, read from `<data dir>/journal/index.db` opened
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
