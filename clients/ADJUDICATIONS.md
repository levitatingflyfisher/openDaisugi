# Adjudication log — cross-client disagreements and oracle findings

Every disagreement between a client and the oracle gets a dated entry: what
disagreed, which implementation is wrong (or what the spec failed to say),
and the resolution. Client authors: record here, then match the oracle;
oracle fixes are batched after the tournament.

## 2026-08-21 — pre-campaign findings (from fixture generation)

**F-1 (oracle, security-relevant): file scopes are right-anchored.**
`_path_matches_any` delegates to `PurePosixPath.match`, which matches
relative patterns from the right: `file_write: ["out.txt"]` admits
`/etc/cron.d/out.txt`; `["*.py"]` admits any `.py` anywhere. The head
allowlist was explicitly left-anchored for this exact reason
(`_head_allowed` docstring); file scopes were not. Frozen for the
tournament; fix queued for the post-wiring pass (changes verdicts →
corpus regenerates).

**F-2 (oracle, portability): glob behavior is Python-version-dependent.**
On 3.12, `PurePosixPath.match` treats mid-pattern `**` as a single-segment
`*`; 3.13 makes it recursive. The oracle's verdicts would silently change
on a Python upgrade, and `_match_glob`'s comment already claims the 3.13
behavior. Fix direction: implement matching in the oracle directly (no
pathlib delegation). Queued with F-1.

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

**F-3 — FIXED IN THE ORACLE.** `predicate_z3._compile_scalar`'s `NotEquals`
now branches numeric-vs-string exactly like `Equals`. All four clients
aligned. Pinned by `tests/test_predicate_notequals_numeric.py`.

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

### The heredoc-pipe conformance rule

`cmd <<EOF 2>&1 | tail` (a heredoc that is not the last token of a piped command)
is a tree-sitter parse error the oracle raises — and `mvdan/sh` rejects it too.
Rather than parse past a construct two independent real parsers refuse, Lean
fails closed at the pipe (an `hdTrail` flag), staying aligned with the reference.

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

**GT-5 (finding, not fixed): the verify client's head extraction splits
on Go whitespace.** `verify.extractShellHead` uses `strings.Fields`, and
several helpers use `strings.TrimSpace`. For a command holding `\x1c` to
`\x1f` the head differs from Python's `split()`. No corpus command holds
one. The gate does not use these for head extraction (it has its own,
with Python's whitespace set). Left for the verify client's owner.

**GT-6 (harness, fixed): two false disagreements from the fixture
itself.** A seeded session tree whose last line was torn with no newline
made the next appended line unparseable in a way the normalizer could not
mask; the seed now ends with a newline. And envelope files stored as JSON
objects had their keys sorted by the canonical case form, which changed
the order of a pydantic error message; envelopes are now stored as text.

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

**GT-9 (client, fixed, was fail-open by time): no native deadline.** An
envelope glob with nine `**` segments kept the Go matcher busy for 110 s;
the oracle denies at `--verify-timeout`. The native verifier now runs
under that budget and denies in the oracle's words (`gate internal error
(denied fail-closed): verifier exceeded the gate's inner time budget
(5.0s).`, tier permanent). An envelope with more than two `**` segments
in a file glob is handed to Python outright: the oracle's matcher is
exponential there, and a faster port would allow where the oracle times
out and denies.

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

**GT-11 (ORACLE, fixed in ebd4dcc and ported; found by the review): the floor rule misses a
relative path after `cd ..`.** With `cwd` `~/work`, `cd .. && cp a
.config/coppice/x` writes into `~/.config/coppice` and neither gate
denies it: `_next_cwd` refuses to follow a `cd` whose target holds `..`,
so the next command's relative words are not placed. Its tier is
permanent, but the hard-deny rule does not fire. Not fixed.

**GT-12 (ORACLE, open, reported by the review): a path through a
symlink with `..`.** With `cfg` a symlink to `~/.config` in `~/work`,
neither gate denies `cp a cfg/../cfg/coppice/x`. The review reports it
as a floor write. On re-derivation it may not be one: the kernel
resolves `cfg` to `~/.config`, then `..` to `~`, then looks for `~/cfg`,
which does not exist in the review's layout, so `cp` would fail; the
gate's resolved spelling (`realpath`) is the same `~/cfg/coppice/x`. It
becomes a real gap only when `~/cfg` also exists and points at the
floor. Kept open for the controller to confirm or close.

## 2026-09-24 — Go gate binary, review round 2

**GT-10 closed in the oracle.** 0423900 makes `_classify_file_redirect`
refuse a file redirect with more than one destination, reason
`ambiguous shell (a redirect with more than one target '...')`:
tree-sitter files the words after such a redirect as more targets, where
bash reads them as the command's head or arguments.
`ls | FOO=1 </work/i rm x`, `rm </dev/null -rf /work`, `cat <a b` and
`echo hi >out more` are now refused. The frozen corpus holds none of these
shapes: the new oracle still matches all 11,450 cases.

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

**GT-15 (shared, fixed in the oracle by 84d1999 and ported): the floor
rule was quadratic in a line's length.** `_shell_hit` appended the cwd
once per command even when it had not changed, so every redirect was
placed against every command's copy. A line of 400 redirects took 257 s,
long enough to pass the host's hook timeout, which fails open. The
oracle now keeps each distinct cwd once, and the Go rule does the same.
The Go gate also runs the three hard-deny rules under the
`--verify-timeout` budget and denies when it runs out. That is stricter
than the oracle, which has no deadline there.

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

**Ported without a finding: fb7acaa and b1ebcab.** The state report now
carries the transcript path (an absolute string only), the verdict
`{decision, tool, clause}` and the mode (`enforcing` or `watching`). The
Go gate always runs in the hook's own process, so like the in-process
oracle it names the transcript and the session. It never takes the
resident-server path, which omits both.

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

**Decided: no `file_read` branch in the floor rule.** coppice.toml names
harnesses, plugins, projects and the gateway; web.json holds paths and
the name of an env variable. Neither holds a secret, so a read of either
is harmless and left to the envelope. The secrets and their directories
are the pane rule's. An operator can put any value in a `[plugin.<id>]`
table; such a value is not guarded. The module and `_hit` doc comments
now say this plainly instead of implying reads are denied.

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

**A search rooted above a data directory: closed below, in part.** See
the next entry.

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

**C-3. `gate check` is the hook contract.** `daisugi gate check` passes
its arguments to `internal/gate` unchanged: the contract of `python -m
opendaisugi.gate`, which is what a hook runs. Python's typer `gate
check` differs (a default `--mode shadow`, no `--session`,
`--captures-root` or `--ask`); nothing installs a hook that calls it.

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

**C-5. No distillation question.** Python's `install` without `--yes`
asks once whether to distil in the background and writes `auto_tend`.
Distillation is not in the binary, so Go never asks. The cases seed
`auto_tend: false`, where Python does not ask either.

**C-6. No decomposition warning.** `gate init
--allow-shell-decomposition` warns when tree-sitter-bash is missing.
The binary's gate decomposes shell natively, so it never warns; the
oracle has tree-sitter installed and does not warn either.

**C-7. What is refused, not answered.** A refused run exits 2, says
why, and writes nothing. Three cases are refused today, all
`config.yaml` shapes outside the YAML reader: an unclosed flow sequence
(a YAML error, which Python reads as shadow), a tab, an anchor. Also
refused, with no case yet: tags, block scalars, several documents,
quoted scalars over several lines; an uninstall of a settings shape Python's reverse would raise on (its
exception text is printed and cannot be matched).

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

**GT-20 (port, fixed): a lone surrogate on stderr.** Python's stderr
writes a lone surrogate as `\ud800` (backslashreplace). The binary wrote
the raw bytes, and its child-to-parent frame turned them into U+FFFD. The
frame now carries bytes and the deny line is escaped as Python writes it.

**GT-21 (ruling): depth parity.** The oracle denies very deep inputs
through RecursionError (a decomposer chain of about 990 links, a symlink
chain resolved past the limit, a payload nested past 9,994 containers).
The binary counts the oracle's Python frames on every path that reaches
`decompose_command`, `realpath` and `json.loads`, and raises at the same
depth. A probe (`sitecustomize`) logs the oracle's frame depth at each
such call; over every native case the binary's depths match. Threshold
scans over chains, pipes, nested substitutions and symlink chains near
the limit show no mismatch.

**GT-22 (ruling): crash backstop.** Each call is decided in a child
process of the binary. A fatal error, a signal, a stack past 256 MB or a
child past 25 s becomes the format's deny contract, never an allow.
Tested with a 10,000-link command and a 30,000-link symlink chain.

**GT-23 (known limit): the glob matcher's step budget.** The oracle's
file-scope matcher is exponential in `**` segments and denies when it
runs past `--verify-timeout`. The binary counts matcher steps and denies
at 1.2 million steps per second of budget, calibrated on the reference
box (Python makes about 1.6 million a second there). On a box slower than
that, Python times out after fewer steps than the budget, so the binary
can allow where the oracle denies. This is the one known exception to
"never allow where Python denies", and only for globs with three or more
`**` segments on a slow box.

**GT-24 (known limit, denied undecided): a predicate regex that makes
Python warn.** `re` prints a FutureWarning (a nested set, `[[`) to stderr
with the oracle's own install path and source line. The binary cannot
know that path, so such an envelope is denied undecided. `llm_check`
predicates, which call a model, are denied undecided too.

**GT-25 (ruling): config.yaml.** With no `--mode`, and for
`verifier_client`, the binary reads config.yaml as PyYAML's `safe_load`
reads a plain subset (block mappings, one nested level, one-line scalars,
YAML 1.1 resolution: `on` is a boolean, `0x10` an int, dates are
timestamps) and validates every Config field with pydantic's lax rules,
so one bad field fails the whole load and the gate falls back to shadow
and python, as the oracle's callers do. YAML outside the subset is denied
undecided. 20,000 random configs: 0 differences from `load_config`.

**GT-26 (ruling): the verifier client table.** `verify_via` finds the
compiled clients from the checkout that holds the oracle's source. The
binary finds them from the checkout that holds the binary. When the two
differ (an installed binary, a checkout elsewhere), the binary may report
"not built" where the oracle dispatches, or the reverse. The verdict is
the conjunction either way, so a client can only tighten it.

**GT-27 (ruling): argparse.** The command line is parsed as Python 3.12's
argparse parses the oracle's parser, errors and usage wrapping included
(20,000 random argv lists: 0 differences). `--help` prints the help and
exits 0, as the oracle does; a host reads that exit as an allow, but no
hook passes `--help`. Help at a terminal width other than 80 is denied
undecided.

**GT-28 (harness): a fixture expectation that depends on the box.** The
"config names go" case expects the Go conformance client to be built
(`clients/go/conform`). Where it is not built, both gates report "not
built" and allow, and the fixture reads as stale.

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

**GT-24 (closed): warning regexes and `llm_check` are decided.** See GT-29
and GT-31.

**GT-26 and GT-28 (closed): see GT-33.**

**GT-27 (widened): help at any width.** The help text is formatted as
argparse's HelpFormatter formats it, at the width `COLUMNS` gives (else
80), with textwrap's chunking. 30,000 random argv lists at widths 1 to
200: 0 differences.

**GT-29 (oracle change, open to veto): the gate filters `re`'s set
warnings.** A predicate regex with a nested set (`[[`) or a set operator
made `re` print a FutureWarning on stderr, with the oracle's install path
and source line, which no other client can know. `gate.py` now filters
exactly those FutureWarnings ("Possible nested set", "Possible set
difference/intersection/union/symmetric difference"). Nothing else about
the regex changes. If this filter is vetoed, the binary must go back to
denying such envelopes undecided.

**GT-30 (ruling, stricter than the oracle): `--verify-timeout` under
1 s.** The oracle joins its verifier thread for the budget, so under 1 s
its answer depends on how fast the thread runs (at 0.3 s it usually
allows a simple read). The binary denies any budget under 1 s as the
oracle's timeout denies ("verifier exceeded the gate's inner time budget
(Xs)."). NaN and a budget too large for a C time_t raise the oracle's
own ValueError and OverflowError text. The binary is never looser here,
only stricter.

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

**GT-32 (oracle change): size caps.** A payload over 16 MiB is denied
with "hook payload is larger than 16777216 bytes; the gate does not read
it", and a shell command over 262,144 characters with "shell command is
longer than 262144 characters; the gate does not read it", in both gates.
The binary's parent reads at most 16 MiB plus one byte of stdin.

**GT-33 (oracle change): the gate finds a verifier client by variable
or PATH.** With `verifier_client` set to a compiled client, the oracle
found it in the checkout that held its source, and the binary in the
checkout that held the binary, so their answers depended on where each
sat (GT-26), and so did a fixture case (GT-28). Both now run the file
named by `OPENDAISUGI_<NAME>_CLIENT`, else `daisugi-conform-<name>` on
PATH, else report "not built". The bench keeps the checkout rule.

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

**GT-36 (port of an oracle fix): a line the floor rule cannot split is a
hit.** The oracle's 1234e6db (found by the Rust port): a decompose that
raises inside the floor rule, as the recursion limit does from the floor
rule's deeper stack, now counts as a hit, and a command whose words
shlex cannot split leaves the cwd unknown while the check goes on. The
binary mirrors both, with its depth parity. Four chain shapes (`&&`,
`;`, `||`, pipes) of 900 to 1,060 links into `~/.config/coppice`
through both gates: 644 calls, all denied, 0 differences. Before the
fix the window was 977 and 978 links for `&&`.

**GT-37 (harness): the llm_check cases get a 30 s verify budget.** With
the suite's 5 s, litellm's import alone ran out the budget on a loaded
box, so three cases read as a timeout there and as a refused call
elsewhere.

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

**RG-2 (Rust client, fixed): `shlex_split` escaped `$` and backtick inside
double quotes.** Python's `shlex.split` escapes only `"` and `\` there.
The split now runs against the CPython-generated shlex fixture.

**RG-3 (Rust client, fixed, was looser than the oracle): a redirect with
more than one target.** Now refused with the oracle's reason.

**RG-4 (harness, noted): home at the root.** With `HOME=/` pathlib gives
`/.config/coppice`; the Rust gate joins as pathlib does.

**RG-5 (ported): the secret-file door.** The pane rule's secret files and
`path_names_secret`'s placement are mirrored, the pane rule before the
floor rule.

**RG-6 (oracle finding, fail-open, fixed in 1234e6db): the floor rule
missed a line at the recursion limit.** Found by the Rust port:
`_shell_hit` read a RecursionError from `decompose_command` as no hit, and
its frames sit one deeper than the pane rule's, so at 977 `&&` links a
relative write into `~/.config/coppice` was allowed under an envelope that
allows relative writes. The oracle now reads a failed split as a hit, and
both ports mirror it.

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

**RG-13 (Go client, open, found by the Rust port): a model reply that
starts with a BOM.** `json.loads` of a str that starts with U+FEFF raises
"Unexpected UTF-8 BOM (decode using utf-8-sig): line 1 column 1 (char
0)" before it scans. The Go `LoadsPy` has no such check, so by reading
it words that llm_check reply "Expecting value: line 1 column 1 (char
0)". A deny either way; only the reason differs. The Rust port has the
check.

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

**RG-10 (fixed, privacy): no build-machine paths.** `tools/z3-build-env.sh`
maps this box's paths away in rustc, the C compiler and the zig c++
wrapper, and takes libc++ and libc++abi by pinned SHA-256.
`tools/check-paths.sh` finds none in either binary. (Since 2026-09-25 it
takes libc++ and libc++abi from the native prefix `native.sh` builds, for
the baseline CPU; see RG-16.)

**RG-11 (not decided): a `\N{...}` escape in a predicate regex, and a
regex repeat of more than 10,000 copies inside a vacuity check.** The gate
does not carry Unicode's name table, and Python builds a Z3 term of that
many parts. A regex search past 20 million matcher steps is not decided
either.

**RG-12 (ruling, as GT-30): `--verify-timeout` under 1 s is not decided.**

**RG-14 (ported): the `daisugi` CLI.** The Go binary's gate commands and
gate half of install, ported to Rust as `target/release/daisugi`, with
the same words for a gate hook, the same refusals and the same one line
for a command not carried. `gate check` is the Rust gate, its child
started as `daisugi gate check`. On the 234 CLI cases: 221 agree, 10 not
ported, 3 refused (YAML outside the reader), the Go binary's split.

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

**IN-2 (oracle and both binaries): `install --gate --enforce` needs a
registered envelope.** With no `*.json` in the gate root's `envelopes/`,
it exits 1, writes nothing, and says on stderr: `Enforce needs a policy
first. Run: daisugi gate init --workspace DIR for a starter envelope,
then this command again. Or install in shadow mode: daisugi install
--gate`. CLI cases hold the refusal, a dry run, an envelopes directory
with no `.json`, and a session envelope only.

**IN-3 (oracle and both binaries): `gate status` warns when a hook's
program is gone.** For each gate hook in the `daisugi gate check` form, in
`~/.claude/settings.json` and the working directory's, whose program is
an absolute path that is not there, status adds one stderr line naming
the file, the program and the fix (`--enforce` for an enforce hook), in
text and JSON output alike. Python's `os.path.exists`, Go's `os.Stat` and
Rust's `fs::metadata` all follow symlinks, so a dangling link counts as
gone.

**IN-4 (ruling): `--version` is the build's.** The binaries print the
version their build set (the release number, or the checkout's git
describe), not pyproject's, so `cli_compare.py` checks only that
`--version` prints one version line.

**RG-16 (fixed, portability): the Rust binaries run on any x86-64.**
`tools/z3-build-env.sh` compiles Z3 with `zig c++ -mcpu=baseline` and
links the baseline libc++ and libc++abi of the native prefix, the ones the
Go gate links. Rust's standard library links `libgcc_s` for unwinding on
glibc; it stays: a static glibc build would need glibc's NSS modules at run
time for the name lookup of an llm_check over https, and
`release.sh --rust` allows it and nothing more.

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

**PW-5 (ruling): `opendaisugi_version` is the build's.** The json, skill
and smtlib exports name the version of the program that wrote them. The
binary writes its build version (IN-4), and the cases compare that key's
shape, not its value.

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

**PW-7 (ruling): no `find` command.** The Python CLI has none, so neither
has the binary; `cmd/pathway-probe` is the test instrument that runs
`find` on many queries in one process.

**PW-8 (ruling): help text.** `daisugi pathways ... --help` prints the
binary's own help, as the gate commands do; no case compares typer's.

**PW-9 (finding, speed): `list` is 22.6 ms at p50 on 1,000 lexical
pathways, over the 20 ms target.** `find` is 15.4 ms. `list` validates every
row as `list_all` does (envelope, plan and all 4,096 floats of each
vector); the binary does not skip that to meet the target, since it would
then print a list where Python raises. Storing vectors as binary would
remove most of the cost, but that is a change to the oracle's schema.

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

**PW-12 (ruling): a refused skill delegation is worded by the binary.**
Every other violation message is the oracle's, checked on 570 synthetic
plans and the private corpus's verify cases. A delegation that subsumption
refuses is reported by the oracle with Z3's counterexample; the binary
reaches the same verdict through its own SMT-LIB text and gives its own
reason.

**PW-13 (ruling): how stderr is compared.** Exactly, except for two
Python forms that carry no fixed text: a traceback, where the binary's line
must name the same exception type (`pydantic_core._pydantic_core.ValidationError`,
`FileNotFoundError`, ...), and click's usage box, where the binary's
output must hold the boxed message.

**PW-14 (Go-side improvement): an overwrite is one transaction.** The
binary deletes the old row and writes the new one in one transaction, so
a failure of the write keeps the old row. With PW-10 the oracle no longer
reaches a failing write either.

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

**GD-2 (oracle change): a failed model call is one warning line.** The
distiller's warning printed instructor's `<failed_attempts>` dump, every
completion with fresh response ids and times. It now goes through
`translate_llm_error`, as every other call site: the first cause and the
count, for example `ValidationError: 1 validation error for
GeneralizedTemplate (3 attempts)`.

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

**GD-5 (ruling): potion vectors within 1e-6.** model2vec sums the rows of
its float32 table in float32; the binary sums in float64 from the same
table, as `find` does (PW-2). A distilled pathway's stored centroid is
therefore compared within 1e-6 per element under potion, and exactly
under lexical, whose vectors both sides build in float64 with the same
order of sums (`mean(axis=0)` adds rows one by one). A potion model that
cannot be loaded is worded by the binary (the warning after `tend:`); no
case covers it.

**GD-6 (ruling): near-threshold cosines.** Clustering and merge compare a
cosine with a threshold (`>=` and `<`). numpy takes the dot products and
norms from BLAS, whose order of sums is chosen per CPU (PW-4); the binary
sums in order. A cosine within the last bits of a threshold can land on
either side; the cases keep a margin from every threshold, and the fuzz
met no such case.

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

**GD-9 (ruling): a model's reply as a Python literal.** `decode_dict_text`
reads a reply or a string step with `ast.literal_eval` when `json.loads`
fails. The binary reads the literals a model writes (dicts, lists,
tuples, quoted strings with the common escapes, decimal numbers, `True`,
`False`, `None`) and nothing else; a rarer literal form (triple quotes,
bytes, sets, hex numbers, implicit concatenation) is read as not valid
JSON, where the oracle would read it.

**GD-10 (ruling): what prints nothing.** The oracle's own loggers carry a
NullHandler, so its log lines (an unparseable `DAISUGI_CLAUDE_ARGS`, a
re-embed skipped, a gateway line skipped, an invalid `local_tier1.json`)
print nothing; the binary prints nothing for them either. instructor's
`Max retries exceeded ...` and `API call failed on attempt N: ...` lines
and litellm's red "Give Feedback" banner (on stdout) do print, and the
binary prints them the same way.

**GD-11 (oracle change): ties in `created_at`.** Traces are read newest
first. With `ORDER BY created_at DESC` alone, equal times (auto-tend stamps
whole seconds) came back in no fixed order, and the order picks the
representative the model is shown. Both sides now read
`ORDER BY created_at DESC, rowid DESC`: of two traces in one second, the
later insert is the newer (case `tend same second`).

**GD-12 (ruling): what is normalized.** A run mints pathway, env, plan and
trace ids at random and stamps times; the cases rename minted ids by first
appearance, write a time near the run as its offset from the run's start
(to the minute), and hide durations (`in N.Ns`, `elapsed_s`, a verify's
`duration_ms`, litellm's `time taken`). Everything else is compared
byte for byte, stderr included.

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

**GD-14 (ruling): help text and the commands left out.** `--help` prints
the binary's own help, as for the gate and pathway commands (PW-8).
`registry pull-and-tend` needs the git pathway store and says it is not in
this binary yet, as do the other `hook` commands.

**GD-15 (ruling): ints of any size in a model's reply.** pydantic takes an
int of any size, so a reply may hold `max_execution_time_s: 10**20`. The
Go verifier read it into an `int` and failed, and tend refused after it
had made its stores. It now reads the max time as a big int, which the Z3
check finds out of range, as the oracle does (the envelope is
inconsistent; the run carries on). The ints the verifier does not judge
are kept as text, and a step's number past the float64 range stays a
number (cases `tend improvement huge max time`, `tend template huge int
in metadata`).

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

**PW-R-3 (as PW-3): the tokenizer is ported from its tables.**
`tools/gen_potion_tables.py` asks the `tokenizers` library (0.23.2)
about every code point and writes `src/pathways/potion/tables.rs`;
`--check` says whether the file is current. 0 disagreements on the
3,042 real-tokenizer texts and the tiny model's goldens.

**PW-R-4 (ruling, where Go refuses): a `?` in the database path is
read.** rusqlite opens the file with no URI flag, so `?` is part of the
name, as in Python's `sqlite3.connect`. The bundled SQLite is compiled
with `SQLITE_USE_URI`, which reads any name that starts with `file:` as a
URI; such a relative name is given as `./file:...`, the file Python
opens (case `list data dir file uri`).

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

**PW-R-8 (as PW-7 and PW-8): no `find` command, and the binary's own
help text.** `pathway-probe` (find, tokens, time) is the instrument, and
writes the Go probe's JSON lines field for field.

**PW-R-9 (as PW-4, PW-5, PW-13, PW-14): ties, the version, stderr and the
overwrite.** Norms are summed in numpy's pairwise order and any tied row
is accepted; `opendaisugi_version` is the build's; stderr is compared as
PW-13 says; an overwrite deletes and writes in one transaction.

**PW-R-10 (finding, speed): `list` meets the target.** A first build took
38 ms at p50 on 1,000 lexical pathways. The validator copied every string
and built each location eagerly, and the shared JSON object hashed each
key twice. The validator now consumes its input and builds a location
only for an error, and an object hashes its keys only past 16 of them
(`gate/pyjson.rs`); `list` is 21 ms and `find` 7 ms at p50. The gate and
CLI compares still agree after the object change.

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

**PW-R-12 (Go finding, closed): True equals 1.** Found while fixing
PW-R-11. The Go verifier's predicate evaluator compared values with
`reflect.DeepEqual`, so `True` did not equal `1` or `1.0` and `False` did
not equal `0`. Python's `==` holds both, in lists and dicts too. So an
enforced `not_equals` or `not_in_set` that the oracle finds violated
passed in Go: a fail-open, in import, the verifier and conform. The
evaluator now compares as Python does (`TestBoolAndNumberCompareAsPythonDoes`),
checked by seven cases in `verify_messages.jsonl` and three pathway
cases. Both Rust evaluators already compared this way (`py_eq`).

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

**GD-R-5 (as GD-5, GD-6, GD-9, GD-11, GD-12, GD-13, GD-14): the rest.**
Potion vectors agree within 1e-6; cosines and centroids are summed in
order; a model's Python literal is read by the Go subset
(`pathways::literal`); traces are read newest first with rowid breaking
a tie; the salvage workspace is a glob's fixed prefix; the normalized
fields are the cases' own; `--help` is the binary's own text, and
`registry pull-and-tend` and the other `hook` commands say they are not
in this binary yet.

**GD-R-6 (ruling): a reply's steps in the oracle's order.** `coerce_step`
runs over every item before the StepBase check, so an invalid registered
step after an item that is no step is the error pydantic reports. The
Rust reply model does the same (`pmodel::tests::a_reply_names_the_invalid_step_first`,
the oracle's text), and so does the Go reply model since `25e9dc9a`. The
case `tend claude reply names invalid step first` covers it on both
binaries. Stored input (not a model reply) still takes the older order in
both binaries; no case reaches that shape.

**GD-R-7 (ruling): big numbers the Rust garden holds.** A count is a
SQLite INTEGER, so a sum of two fits 128 bits; a failure ratio is the
exactly rounded quotient Python's int / int gives
(`garden::tests::int_division_is_correctly_rounded`); a merged count past
64 bits fails as sqlite3 fails (case `merge sum past 64 bits`). A
session's `captured_at` is ordered as a float, where Go orders an exact
rational: two ints past 2**53 that round to one float may list in
another order. No case reaches it.
