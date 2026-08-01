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
