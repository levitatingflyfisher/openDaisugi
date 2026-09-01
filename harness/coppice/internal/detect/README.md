# The agent-detection manifest schema

coppice reads Herdr's TOML manifests unchanged, so twenty-one agents are detected on day one
and a Herdr user's own override files keep working. This file is the schema, written down from
Herdr's `src/detect/manifest.rs`. Verify before you rely.

## Top level

| key | type | notes |
|---|---|---|
| `id` | string | required. The agent id, for example `claude`. |
| `version` | string | optional. Herdr uses `YYYY.MM.DD.N`. We keep it for `explain`. |
| `min_engine_version` | integer | optional. We refuse a manifest that asks for more than our engine version. |
| `updated_at` | string | optional, informational. |
| `aliases` | list of string | optional. Other names for the same agent. |
| `rules` | list of table | the rules, evaluated in file order. |

Any other top-level key is a load error. Herdr uses serde's `deny_unknown_fields`; we get the
same effect from `toml.MetaData.Undecoded()`.

Herdr also refuses a manifest with zero rules (`rules` must be non-empty). coppice's `Parse`
enforces the same: a manifest with `id` set and no `[[rules]]` tables is a load error.

## A rule

| key | type | notes |
|---|---|---|
| `id` | string | required, non-empty. |
| `state` | `idle` / `working` / `blocked` / `unknown` | the state this rule asserts. Absent means `unknown`. |
| `priority` | integer | default 0. Highest wins. On a tie the earlier rule in the file wins. |
| `region` | string | default `whole_recent`. See the region list below. |
| `visible_idle`, `visible_blocker`, `visible_working` | bool | evidence flags a client may show. |
| `skip_state_update` | bool | this rule matching means "emit no event at all". |
| `contains`, `regex`, `line_regex` | list of string | matchers. See the semantics below. |
| `all`, `any`, `not` | list of gate tables | nested matchers. |

`skip_state_update = true` requires `state = "unknown"` and forbids the three visible flags.

## A gate

A gate table takes the same six matcher keys as a rule: `contains`, `regex`, `line_regex`,
`all`, `any`, `not`. A gate with no positive matcher is a load error.

Herdr treats a gate nested inside `not` slightly more leniently: there, a gate that is itself
only a nested `not` list (`not = [{ not = [...] }]`) is accepted, because *something* to negate is
present even without a positive matcher. coppice does not carry that split - every gate, `not`-
nested or otherwise, needs a positive matcher - so this one shape is stricter here than in Herdr.
Unlikely to matter for a hand-written manifest, flagged for completeness.

## Matcher semantics

- `contains`: **every** needle must appear. Needles are lowercased at load and matched against
  a lowercased copy of the region.
- `regex`: **every** pattern must match the region, case-sensitive, against the original text.
- `line_regex`: **every** pattern must match **some** line of the region. Not necessarily the
  same line.
- `all`: every nested gate must match.
- `any`: enforced only when non-empty; then at least one nested gate must match.
- `not`: no nested gate may match.

Herdr's patterns come from the Rust `regex` crate, which like Go's `regexp` is RE2-shaped:
`(?i)`, `(?m)`, `\x{2800}` and character classes all carry over, and neither engine has
backreferences or lookaround. A pattern that fails to compile is a load error, and a test
compiles every pattern in every bundled manifest so a drift between the two engines shows up
as a red test, not as a detection that quietly never fires.

Two constructs in the real vendored manifests are Rust-only and are rewritten before compiling,
by `translateRustRegex` in `manifest.go`, rather than by hand-editing the vendored `.toml` (see
`NOTICE`): Rust's `\uXXXX` and `\u{XXXX}` unicode escapes both become Go's `\x{XXXX}`, and Rust's
derived-property class `\p{Alphabetic}` (from Unicode's `PropList.txt`, which Go's `regexp` does
not implement) becomes Go's general-category `\pL`. This runs on every `regex` and `line_regex`
pattern, bundled or override, so an operator's own override file does not need to know about the
substitution either. It is not a general Unicode-property translator - only the two forms above
are recognised - so a future re-vendor that introduces a different unsupported construct still
fails closed with a compile error naming the rule, not a silent, wrong translation. An escaped
backslash right before one of these constructs (`\\uABCD` in the pattern text: a literal
backslash, then the four plain characters `uABCD`) is left alone rather than mistranslated -
the translator counts the run of backslashes and only treats the trailing one as live when the
count is odd.

## Regions

Read against the pane's screen text, unwrapped, plus the terminal title and the last OSC
progress payload. That text is the whole unwrapped screen, not a fixed slice of it: `whole_recent`
is the whole thing, and every other region narrows it from there. There is no separate window
layered underneath `region` - `pane.read --source detection` and `pane.explain`'s
`detection_text` show the evaluator's exact input, unnarrowed, so a rule's own `region` is the
only thing standing between it and the whole screen. 2026-09-10 note, final fix wave, plan 02:
this used to be capped at the bottom 12 non-empty lines before any rule's own region ever ran.
14 rules across the 21 vendored manifests read past that cap: a `bottom_non_empty_lines(N)` with
N over 12, a `top_non_empty_lines` of any N (it reads from the true top of the screen, which a
bottom-12 cap always discards), or a prompt-box region (`prompt_box_body`, `above_prompt_box`,
`last_non_empty_above_prompt_box`), whose match target is not a fixed number of lines from either
edge. The fixed cap was ruled out. A `contains` or `regex` rule can now match a phrase still on
the viewport but no longer current, the same tradeoff Herdr makes.

`whole_recent` (default), `after_last_prompt_marker`, `before_current_prompt_marker`,
`whole_recent_without_current_prompt_marker`, `current_prompt_block_marker`,
`after_current_prompt_block_marker`, `prompt_box_body`, `above_prompt_box`,
`last_non_empty_above_prompt_box`, `after_last_horizontal_rule`, `osc_title`, `osc_progress`,
and the parameterised `bottom_lines(N)`, `bottom_non_empty_lines(N)`, `top_non_empty_lines(N)`.

`top_non_empty_lines(N)` needs `min_engine_version >= 3`; N must be a positive decimal with no
leading zero, at most 65535; `bottom_lines(N)` and `bottom_non_empty_lines(N)`'s `N` is likewise
digits-only - no leading `+`, `-`, or anything else non-numeric (`bottom_lines(0)` itself stays
valid, since Herdr's constraint here is "not negative," not "no leading zero").

An unknown region name is a load error. It is never treated as an empty region, because a rule
that silently never matches is worse than a manifest that refuses to load.

Line-splitting keeps every blank line the screen actually has, dropping only the single line
terminator at the very end - matching Herdr, not a plain right-trim. A screen ending
`"a\nb\nc\n\n\n"` has two real trailing blank lines below `c`; `bottom_lines(2)` on it returns
those two blank lines (`"\n"`), not an empty string. A captured terminal commonly has several
blank rows below the last real output, so trimming a whole run of trailing newlines instead of
just the one terminator would silently discard real screen content, not merely tidy up
whitespace.

## Limits, all load errors when exceeded

128 rules per manifest, gate nesting depth 8, 512 gates per manifest, 32 direct matchers per
gate, 1024 matchers per manifest, 512 characters per matcher.

## Where coppice deliberately differs from Herdr

Herdr falls back to `idle` when a *known* agent's manifest matches nothing
(`DEFAULT_KNOWN_AGENT_IDLE_FALLBACK`). coppice does not. Master spec 3.1 says `unknown` is what
the floor shows when it has no source, and that it is never dressed up as `idle`. No match means
no event, and the pane keeps whatever a real source last told us.

coppice also never fetches a manifest from the network. Herdr's remote update path is not
ported. Updates ride our releases, and a `.toml` file under
`~/.config/coppice/agent-detection/` replaces the bundled manifest with the **same `id`** -
matched by the `id` field the override file itself declares, not by the override's filename. A
file at any path, named anything, still overrides the right bundled entry as long as its `id`
matches.

## What loads one manifest, what slices a region, what evaluates, and what loads the set

`manifest.go` parses and validates a single manifest's bytes: `Parse(name, data)` returns a
`*Manifest` (the raw decoded fields) and a `*Compiled` (the same rules with every regex already
built, so evaluation never compiles a pattern at match time).

`regions.go`'s `Region(in, spec)` slices an `Input` (screen text, OSC title, OSC progress) the way
a rule's `region` field asked for; it is the Go port of Herdr's `region()`. `evaluate.go`'s
`(*Compiled).Evaluate(in)` runs every rule in file order against that `Input` and returns the
highest-priority match (ties keep the earlier rule), plus an `Evaluated` entry per rule for
`explain`-style tooling.

Reading the bundled and override files off disk, resolving aliases across manifests, and
collecting non-fatal warnings for `server.status` live in `set.go`'s `Set` / `LoadSet`. The
bundled manifests are the real vendored ones (`//go:embed manifests/*.toml`), and
`manifests_test.go` proves the whole set against them - every bundled manifest loads and compiles
its regexes, every manifest is either fixtured under `../../testdata/screens/` or listed in
`unfixtured.txt`, and every fixture detects the state its filename declares.
