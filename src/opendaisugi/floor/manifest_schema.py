"""The Herdr agent-detection manifest schema, recorded as plain constants.

coppice's Go evaluator at harness/coppice/internal/detect implements Herdr's
TOML manifest format unchanged, so Herdr's manifests and a Herdr user's own
override files work here too. harness/coppice/internal/detect/README.md
writes that format down from Herdr's src/detect/manifest.rs, with a complete
implementation beside it in manifest.go, regions.go and evaluate.go. This
module records the same facts as importable Python constants, so a Python
port of the evaluator does not have to read Go source to know the schema,
and can refuse a key, gate or region name the Go side does not recognise
instead of silently matching nothing.

Nothing here is a guess. Every constant traces to a line in README.md or the
named Go source. tests/floor/test_manifest_schema.py walks the vendored
manifests and the Go source itself to prove that, and
scripts/floor_manifest_schema.py hashes the vendored manifests and the
README so a change on the Go side is noticed here even when nobody reads it
by hand.
"""

from __future__ import annotations

from pathlib import Path

# Where the manifests and the schema document live. This only resolves
# inside a source checkout of this repository: there is no
# COPPICE_MANIFESTS environment variable and no packaged-wheel fallback.
# Outside a checkout, MANIFEST_DIR and DETECT_README simply do not exist;
# tests.floor.hostfacts is where a caller checks that before reading them.
REPO_ROOT = Path(__file__).resolve().parents[3]
MANIFEST_DIR = REPO_ROOT / "harness" / "coppice" / "internal" / "detect" / "manifests"
DETECT_README = REPO_ROOT / "harness" / "coppice" / "internal" / "detect" / "README.md"

# The engine version this schema was recorded against. A manifest whose
# min_engine_version exceeds this is a load error. See manifest.go's Parse.
ENGINE_VERSION = 3

# --- Top level --------------------------------------------------------------
#
# Any key not in this set is a load error: Herdr uses serde's
# deny_unknown_fields, and coppice gets the same effect from
# toml.MetaData.Undecoded(). rules must be present and non-empty.
MANIFEST_KEYS = frozenset({"id", "version", "min_engine_version", "updated_at", "aliases", "rules"})

# --- A rule -------------------------------------------------------------
#
# The eight keys a rule carries beyond the six matcher/gate keys below.
RULE_ONLY_KEYS = frozenset(
    {
        "id",
        "state",
        "priority",
        "region",
        "visible_idle",
        "visible_blocker",
        "visible_working",
        "skip_state_update",
    }
)

# The six matcher/gate keys. A gate table takes exactly these six, whether
# it is nested under all/any/not or is a rule's own top-level matchers.
MATCHER_KEYS = frozenset({"contains", "regex", "line_regex", "all", "any", "not"})

# Every key a rule may declare.
RULE_KEYS = RULE_ONLY_KEYS | MATCHER_KEYS

DEFAULT_PRIORITY = 0
# On a match, the highest-priority rule wins. A tie keeps the earlier rule
# in the file: evaluate.go's Evaluate only replaces the current best when
# the new rule's priority is strictly greater than the best seen so far.
PRIORITY_TIE_BREAK = "earlier rule in the file wins"

# The four states a rule may assert. A rule with no state key asserts
# "unknown". See manifest.go's EffectiveState. This set is
# opendaisugi.floor.events.STATES minus "done": the detection engine never
# reports "done" on its own, only idle/working/blocked/unknown.
RULE_STATES = frozenset({"idle", "working", "blocked", "unknown"})
DEFAULT_RULE_STATE = "unknown"

# --- Matcher semantics --------------------------------------------------
#
# Read off README.md's "Matcher semantics" section and cross-checked
# against gateMatches in evaluate.go. Every pattern list is ANDed: every
# needle, every regex, every line_regex pattern must match. "any" is the
# only OR in the whole schema.
MATCHER_SEMANTICS = {
    "contains": {
        "case_sensitive": False,  # needle and region text are both lowercased before matching
        "quantifier": "all",  # every needle must appear
        "scope": "region",  # matched against the whole region text
    },
    "regex": {
        "case_sensitive": True,  # matched against the original, unlowered text
        "quantifier": "all",  # every pattern must match
        "scope": "region",
    },
    "line_regex": {
        "case_sensitive": True,
        "quantifier": "all",  # every pattern must match, but each may match a different line
        "scope": "some_line",  # not necessarily the same line as another line_regex pattern
    },
}

# all: every nested gate must match.
# any: enforced only when non-empty; then at least one nested gate must match.
# not: no nested gate may match.
GATE_COMBINATOR_SEMANTICS = {
    "all": "every nested gate must match",
    "any": "enforced only when non-empty; then at least one nested gate must match",
    "not": "no nested gate may match",
}

# A gate needs at least one of these five to be non-empty. A gate here
# means a rule's own matchers, or a nested all/any/not table. "not" alone
# does not count: a gate with only "not" is a load error. See
# compileGate's "must contain a positive matcher" check.
POSITIVE_MATCHER_KEYS = frozenset({"contains", "regex", "line_regex", "all", "any"})

# Herdr accepts one narrower shape here: a gate nested inside "not" that is
# itself only another "not" list, such as not = [{ not = [...] }]. Herdr
# allows it because something to negate is present even without a positive
# matcher. coppice does not carry that split: every gate, "not"-nested or
# otherwise, needs a positive matcher. See README.md's "A gate" section.
COPPICE_IS_STRICTER_THAN_HERDR_FOR_NOT_ONLY_GATES = True

# skip_state_update = true means "emit no event at all" when the rule
# matches. It requires state to be "unknown" or absent, and it forbids the
# three visible_* flags. See manifest.go's Parse.
SKIP_STATE_UPDATE_REQUIRES_STATE_UNKNOWN = True
SKIP_STATE_UPDATE_FORBIDS_VISIBLE_FLAGS = True

# --- Regions --------------------------------------------------------------
#
# Fixed region names, not parameterised. Twelve of the fifteen.
FIXED_REGIONS = frozenset(
    {
        "whole_recent",
        "after_last_prompt_marker",
        "before_current_prompt_marker",
        "whole_recent_without_current_prompt_marker",
        "current_prompt_block_marker",
        "after_current_prompt_block_marker",
        "prompt_box_body",
        "above_prompt_box",
        "last_non_empty_above_prompt_box",
        "after_last_horizontal_rule",
        "osc_title",
        "osc_progress",
    }
)

# Parameterised region name prefixes. Each takes one N in parentheses,
# e.g. bottom_lines(12). Three of the fifteen.
PARAMETERIZED_REGIONS = frozenset({"bottom_lines", "bottom_non_empty_lines", "top_non_empty_lines"})

DEFAULT_REGION = "whole_recent"

# An unknown region name is a load error, never treated as an empty region.
# A rule that silently never matches is worse than a manifest that refuses
# to load. See regions.go's Region and manifest.go's ValidRegion.
UNKNOWN_REGION_IS_LOAD_ERROR = True

# bottom_lines(N) and bottom_non_empty_lines(N) accept a leading zero:
# bottom_lines(0) is a legitimate, if useless, region. Only a non-digit
# character is refused, a leading "-" included, because Herdr's count is
# an unsigned size, not a signed integer. See manifest.go's regionCount.
BOTTOM_REGIONS_ALLOW_LEADING_ZERO = True

# top_non_empty_lines(N) is stricter: N must be a positive decimal with no
# leading zero, and it needs min_engine_version >= 3. See manifest.go's
# topRegionCount.
TOP_NON_EMPTY_LINES_ALLOWS_LEADING_ZERO = False
TOP_NON_EMPTY_LINES_MAX_N = 65535
TOP_NON_EMPTY_LINES_MIN_ENGINE_VERSION = 3

# manifest.go only enforces the top_non_empty_lines minimum engine version
# when the manifest actually declares one: `m.MinEngineVersion != 0 &&
# m.MinEngineVersion < topRegionMinVer`. A manifest with no
# min_engine_version key at all defaults to 0, so it is exempt from this
# check even when one of its rules uses top_non_empty_lines. README.md
# reads as an unconditional "needs min_engine_version >= 3". The code is
# not unconditional; this constant records the code, not the README line.
TOP_NON_EMPTY_LINES_ENGINE_CHECK_SKIPPED_WHEN_UNSET = True

# --- Load limits, all load errors when exceeded ---------------------------
MAX_RULES = 128
MAX_GATE_DEPTH = 8
MAX_TOTAL_GATES = 512
MAX_MATCHERS_PER_GATE = 32
MAX_TOTAL_MATCHERS = 1024
MAX_MATCHER_CHARS = 512

# --- Rust regex constructs Go's RE2 engine does not accept directly -------
#
# Two constructs appear in the vendored manifests that Go's regexp does not
# accept outright. manifest.go's translateRustRegex rewrites each pattern
# before compiling. This module does not port that translator. It only
# records what a Python port of the evaluator also has to handle. Python's
# re module has no \p{...} property syntax at all, unlike Go's regexp.
RUST_ONLY_REGEX_CONSTRUCTS = {
    "unicode_escape": (
        r"\uXXXX and \u{XXXX} in the vendored patterns become Go's \x{XXXX}. "
        "Seven of the 21 vendored manifests use one or the other, for "
        "braille-spinner ranges and for variation-selector alternatives."
    ),
    "derived_unicode_property": (
        r"\p{Alphabetic} becomes Go's general category \pL. Python's re "
        "module has no \\p{...} property syntax at all."
    ),
}
