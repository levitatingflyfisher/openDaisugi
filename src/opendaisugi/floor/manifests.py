"""Evaluate Herdr's TOML screen manifests, a port of harness/coppice/internal/detect.

Master spec 5.5: coppice reuses Herdr's manifest format, so twenty-one agents
are detected on day one. Master spec 5.1: this is the fallback path. A gate
event outranks anything decided here, a manifest may never assert ``done``,
and a screen that matches nothing is ``unknown``, never a confident ``idle``.

This module ports three Go files under ``harness/coppice/internal/detect``,
against the schema ``opendaisugi.floor.manifest_schema`` records:

* ``manifest.go`` -- ``parse_manifest`` here, loading and validating one
  manifest's rules, gates, limits, and the two Rust-only regex constructs.
* ``regions.go`` -- the ``_region_text`` dispatcher and its helpers, slicing
  the pane's screen text, title, and OSC progress the way a rule's own
  ``region`` field asks for.
* ``evaluate.go`` -- ``_evaluate``, running every rule in file order and
  keeping the highest-priority match, ties won by the earlier rule.

A manifest that fails validation anywhere -- an unknown key at any depth, an
invalid state, a bad regex, a limit exceeded -- is dropped whole, the same
choice ``internal/detect/set.go``'s ``Set.add`` makes: half a manifest is
worse than none, and a broken override file must not take a working bundled
manifest down with it.

Region text is read against the whole unwrapped screen, title, and last OSC
progress payload coppice hands its own evaluator, exactly as
``internal/detect/README.md`` documents as of the 2026-09-10 fix wave. There
is no fixed-line window layered underneath a rule's own ``region``.

Where the manifests come from, in order: the vendored directory bundled in
this repository, then the user's own override directory. There is no
``COPPICE_MANIFESTS`` environment variable; this only resolves inside a
source checkout, matching ``manifest_schema.MANIFEST_DIR``.

Several regex constructs need rewriting before Python's ``re`` can compile
them the way Go's ``regexp`` does, or compile them at all: the braced
unicode escapes, the end-of-text anchor, the derived Unicode property
``\\p{Alphabetic}``, and the six Perl classes ``\\s`` ``\\S`` ``\\d`` ``\\D``
``\\w`` ``\\W``, which Python defines over Unicode and Go defines over
ASCII only. See ``translate_rust_regex`` for the detail Go's own
``translateRustRegex`` does not need, since it targets its own engine.
"""

from __future__ import annotations

import os
import re
import sys
import tomllib
import unicodedata
from collections.abc import Sequence
from dataclasses import dataclass
from functools import lru_cache
from pathlib import Path

from opendaisugi.exceptions import OpenDaisugiError
from opendaisugi.floor import manifest_schema as schema


class ManifestSchemaMismatch(OpenDaisugiError):
    """One manifest failed to load or validate against the recorded schema.

    Raised internally while parsing a single file; ``load_manifests``
    catches it and drops that file, the same fail-closed choice
    ``internal/detect/set.go``'s ``Set.add`` makes.
    """


# --- value types --------------------------------------------------------


@dataclass(frozen=True)
class Gate:
    """One gate: a rule's own matchers, or a nested all/any/not table.

    ``contains`` is already lowercased. ``regex`` and ``line_regex`` are
    already compiled, so evaluation never compiles a pattern at match time.
    """

    contains: tuple[str, ...] = ()
    regex: tuple[re.Pattern[str], ...] = ()
    line_regex: tuple[re.Pattern[str], ...] = ()
    all: tuple["Gate", ...] = ()
    any: tuple["Gate", ...] = ()
    not_gates: tuple["Gate", ...] = ()


@dataclass(frozen=True)
class Rule:
    id: str
    state: str
    priority: int
    region: str
    gate: Gate
    skip_state_update: bool = False
    visible_idle: bool = False
    visible_blocker: bool = False
    visible_working: bool = False


@dataclass(frozen=True)
class Manifest:
    agent: str
    rules: tuple[Rule, ...]
    source: Path
    aliases: tuple[str, ...] = ()


@dataclass(frozen=True)
class Match:
    """One classification: the winning rule's state, id, priority and
    region, and whether it asked for silence.

    evaluate.go's Result carries three distinct outcomes: no rule matched,
    a real rule matched, or a ``skip_state_update`` rule matched (which
    always asserts state ``"unknown"``, enforced at load). ``skip`` is what
    tells those last two apart, since both otherwise report state
    ``"unknown"``: a caller that wants "emit nothing" can check ``skip``
    directly instead of the old, ambiguous ``rule is None``.
    """

    state: str
    rule: str | None
    priority: int
    region: str | None
    skip: bool


# --- where the manifests live -------------------------------------------


def _override_dir() -> Path:
    """The operator's own override directory, mirroring set.go's OverrideDir."""
    xdg = os.environ.get("XDG_CONFIG_HOME")
    base = Path(xdg) if xdg else Path.home() / ".config"
    return base / "coppice" / "agent-detection"


def default_manifest_dirs() -> tuple[Path, ...]:
    """The bundled vendored directory, then the user's own override directory.

    Later wins: ``load_manifests`` lets a later directory replace an
    earlier agent by id. There is no ``COPPICE_MANIFESTS`` environment
    variable; the bundled directory is always ``manifest_schema.MANIFEST_DIR``.
    """
    return (schema.MANIFEST_DIR, _override_dir())


DEFAULT_MANIFEST_DIRS: tuple[Path, ...] = default_manifest_dirs()


# --- translateRustRegex, targeting Python's re instead of Go's regexp ----
#
# Go compiles every pattern with regexp.Compile, whose Perl classes and word
# boundary are ASCII: \s is [\t\n\f\r ] (no vertical tab), \d is [0-9], \w is
# [0-9A-Za-z_], and \b/\B use that same ASCII word set. Python's re on str
# patterns defines all four over Unicode by default. Compiling with re.ASCII
# closes most of the gap (\d, \w, \b) but not all of it: Python's own ASCII
# \s still includes the vertical tab, U+000B, which Go's does not. So this
# module textually rewrites \s, \S, \d, \D, \w, \W into Go's exact character
# sets before compiling, and still compiles with re.ASCII so \b and \B use
# Go's ASCII word set -- Python has no way to redefine a zero-width
# assertion's character set except through that flag. re.ASCII also narrows
# (?i) case folding to ASCII letters only, a real divergence from Go's own
# (?i), which folds full Unicode (confirmed directly: Go's regexp matches
# "É" against (?i)é). No vendored pattern relies on non-ASCII folding today.

_S_POS_MEMBERS = "\t\n\x0c\r "  # tab, newline, form feed, carriage return, space
_D_POS_MEMBERS = "0-9"
_W_POS_MEMBERS = "0-9A-Za-z_"


def _complement_member_string(included: list[tuple[int, int]]) -> str:
    """The bracket-class member text for every codepoint NOT in the given
    ranges, as explicit \\U escapes. Used for the negated Perl classes
    (\\S, \\D, \\W) inside an existing character class, where Python cannot
    nest a negated bracket expression the way Go's char-class algebra can."""
    included = sorted(included)
    parts: list[tuple[int, int]] = []
    prev_end = -1
    for lo, hi in included:
        if lo > prev_end + 1:
            parts.append((prev_end + 1, lo - 1))
        prev_end = max(prev_end, hi)
    if prev_end < sys.maxunicode:
        parts.append((prev_end + 1, sys.maxunicode))
    return "".join(f"\\U{a:08x}" if a == b else f"\\U{a:08x}-\\U{b:08x}" for a, b in parts)


_S_NEG_MEMBERS = _complement_member_string(
    [(0x09, 0x09), (0x0A, 0x0A), (0x0C, 0x0C), (0x0D, 0x0D), (0x20, 0x20)]
)
_D_NEG_MEMBERS = _complement_member_string([(0x30, 0x39)])
_W_NEG_MEMBERS = _complement_member_string([(0x30, 0x39), (0x41, 0x5A), (0x5F, 0x5F), (0x61, 0x7A)])


@lru_cache(maxsize=1)
def _alphabetic_class() -> str:
    """The Unicode Letter general category, as an explicit codepoint class.

    Go's regexp has no derived Unicode properties either, so manifest.go's
    translateRustRegex maps Rust's \\p{Alphabetic} to Go's own general
    category \\pL, not to the wider Alphabetic property. Python's re module
    has no \\p syntax at all, fixed or derived. This builds the same general
    category Go's \\pL names, as a bracket expression, so this evaluator
    matches Go's actual choice rather than Rust's original property --
    matching Go is what the shared conformance fixtures measure. Built once
    and cached: scanning every codepoint costs a few hundred milliseconds
    the first time a \\p{Alphabetic} pattern is translated.
    """
    ranges: list[tuple[int, int]] = []
    start: int | None = None
    for cp in range(sys.maxunicode + 1):
        is_letter = unicodedata.category(chr(cp))[0] == "L"
        if is_letter and start is None:
            start = cp
        elif not is_letter and start is not None:
            ranges.append((start, cp - 1))
            start = None
    if start is not None:
        ranges.append((start, sys.maxunicode))
    parts = [f"\\U{lo:08x}" if lo == hi else f"\\U{lo:08x}-\\U{hi:08x}" for lo, hi in ranges]
    return "[" + "".join(parts) + "]"


def _match_construct(pattern: str, pos: int) -> str | None:
    """The construct token starting at ``pos``, unTranslated, or None.

    Recognises the braced unicode escapes \\u{XXXX} and \\x{XXXX}, the
    end-of-text anchor \\z, the derived property \\p{Alphabetic}, and the
    six Perl classes \\s \\S \\d \\D \\w \\W. Anything else, including a
    \\p{...} this module does not recognise, is left for the caller to pass
    through unchanged, so an unsupported construct fails closed at
    ``re.compile`` rather than being silently mistranslated.
    """
    rest = pattern[pos:]
    for prefix in ("u{", "x{"):
        if rest.startswith(prefix):
            end = rest.find("}")
            if end > len(prefix) and all(
                c in "0123456789abcdefABCDEF" for c in rest[len(prefix) : end]
            ):
                return rest[: end + 1]
            return None
    if rest.startswith("p{Alphabetic}"):
        return "p{Alphabetic}"
    if rest[:1] in ("z", "s", "S", "d", "D", "w", "W"):
        return rest[0]
    return None


def _translate_construct_text(construct: str, in_class: bool) -> str:
    if construct.startswith(("u{", "x{")):
        value = int(construct[2:-1], 16)
        return f"\\U{value:08x}"
    if construct == "z":
        return "\\Z"
    if construct == "p{Alphabetic}":
        if in_class:
            raise ManifestSchemaMismatch(
                "uses \\p{Alphabetic} inside a character class, which cannot "
                "be expressed as a nested set"
            )
        return _alphabetic_class()
    if construct == "s":
        return _S_POS_MEMBERS if in_class else f"[{_S_POS_MEMBERS}]"
    if construct == "S":
        return _S_NEG_MEMBERS if in_class else f"[^{_S_POS_MEMBERS}]"
    if construct == "d":
        return _D_POS_MEMBERS if in_class else f"[{_D_POS_MEMBERS}]"
    if construct == "D":
        return _D_NEG_MEMBERS if in_class else f"[^{_D_POS_MEMBERS}]"
    if construct == "w":
        return _W_POS_MEMBERS if in_class else f"[{_W_POS_MEMBERS}]"
    if construct == "W":
        return _W_NEG_MEMBERS if in_class else f"[^{_W_POS_MEMBERS}]"
    raise AssertionError(f"unreachable construct {construct!r}")  # pragma: no cover


_FLAG_GROUP_LETTERS = frozenset("imsU")


def _pattern_has_multiline_flag(pattern: str) -> bool:
    """Whether a real, unescaped flag group outside a character class
    carries the m flag: bare (``(?m)``), combined with others in any
    order (``(?im)``, ``(?mi)``, ``(?sm)``), or the scoped form
    (``(?m:...)``). A literal ``(?m)`` inside a character class does not
    count, and neither does an escaped ``(``.

    Conservative and whole-pattern, matching Go's own unscoped-flag shape:
    one such group anywhere in the pattern disarms the $ rewrite for the
    whole pattern, not just the text after it or the scoped span alone.
    """
    i = 0
    n = len(pattern)
    in_class = False
    while i < n:
        ch = pattern[i]
        if ch == "\\":
            j = i
            while j < n and pattern[j] == "\\":
                j += 1
            if j >= n:
                break
            # An escaped character never opens or closes a class and is
            # never a flag group start; skip past exactly the one escaped
            # character on an odd run, or nothing extra on an even run,
            # the same backslash-counting rule translate_rust_regex uses.
            i = j + 1 if (j - i) % 2 == 1 else j
            continue
        if ch == "[" and not in_class:
            in_class = True
            i += 1
            continue
        if ch == "]" and in_class:
            in_class = False
            i += 1
            continue
        if ch == "(" and not in_class and pattern[i : i + 2] == "(?":
            j = i + 2
            start = j
            while j < n and pattern[j] in _FLAG_GROUP_LETTERS:
                j += 1
            if j < n and pattern[j] in (")", ":") and "m" in pattern[start:j]:
                return True
        i += 1
    return False


def translate_rust_regex(pattern: str) -> str:
    """Rewrite the regex constructs the vendored manifests use that Python's
    re module does not accept, or accepts with different semantics: the
    braced unicode escapes \\u{XXXX} and \\x{XXXX}, the end-of-text anchor
    \\z, the derived Unicode property \\p{Alphabetic}, the six Perl classes
    \\s \\S \\d \\D \\w \\W (rewritten into Go's own ASCII-only character
    sets rather than Python's Unicode ones), and the unescaped $ anchor.

    Each of the Perl classes is recognised both outside and inside a
    character class: outside, \\s becomes the bracket expression
    ``[\\t\\n\\f\\r ]``; inside an already-open class, it contributes just
    the member characters, since Python cannot nest one bracket expression
    inside another. \\p{Alphabetic} inside a class is refused instead,
    since a working positive member list is only half of what \\S/\\D/\\W
    need and Go's own scope for this construct is already narrow; see the
    module docstring.

    This is not a general Unicode-property translator: only \\p{Alphabetic}
    is recognised, matching Go's own translateRustRegex's scope, so a
    future re-vendor that introduces a different construct fails closed
    with a compile error naming the rule, not a silent, wrong translation.

    Every backslash-led construct above starts with a literal backslash, so
    an escaped backslash right in front of one must not be mistaken for
    the start of it: a run of N backslashes translates only when N is odd,
    exactly as manifest.go's translateRustRegex counts them. When N is
    even, the character right after the run is not part of any escape at
    all and is fed back to this same scanner, so a real "[" or "]" there
    still opens or closes a class instead of being swallowed as if it had
    been escaped.

    This same scanner also refuses lookahead and lookbehind at a real,
    unescaped group opening outside a class -- RE2 does not support them,
    and an override file written against Herdr's own RE2-shaped engine
    would not use them either -- and rewrites an unescaped, unclassed $ to
    \\Z when no real flag group in the pattern carries the m flag (see
    ``_pattern_has_multiline_flag``), since Go's $ without that flag means
    end of text, not end of text or just before a final newline the way
    Python's own default $ does.
    """
    has_multiline = _pattern_has_multiline_flag(pattern)
    out: list[str] = []
    i = 0
    n = len(pattern)
    in_class = False
    while i < n:
        ch = pattern[i]
        if ch == "\\":
            j = i
            while j < n and pattern[j] == "\\":
                j += 1
            run = pattern[i:j]
            if j >= n:
                out.append(run)
                i = j
                continue
            construct = _match_construct(pattern, j)
            if construct is None:
                if len(run) % 2 == 0:
                    # Every backslash is an escaped literal one, so
                    # whatever follows is not part of this escape at all:
                    # emit the run and let the main loop see that
                    # character fresh, so a real "[" or "]" right after an
                    # escaped backslash still opens or closes a class.
                    out.append(run)
                    i = j
                    continue
                # Not a construct this function knows: pass the backslash
                # run and exactly the one following character through
                # unchanged, the same way \b, \A, \[, and every ordinary
                # escape already work.
                out.append(run)
                out.append(pattern[j])
                i = j + 1
                continue
            if len(run) % 2 == 0:
                # Every backslash is an escaped literal one, so the
                # construct text is ordinary characters, not a live escape.
                out.append(run)
                out.append(construct)
                i = j + len(construct)
                continue
            out.append(run[:-1])
            out.append(_translate_construct_text(construct, in_class))
            i = j + len(construct)
            continue
        if ch == "[" and not in_class:
            in_class = True
            out.append(ch)
            i += 1
            if i < n and pattern[i] == "^":
                out.append(pattern[i])
                i += 1
            if i < n and pattern[i] == "]":
                out.append(pattern[i])
                i += 1
            continue
        if ch == "]" and in_class:
            in_class = False
            out.append(ch)
            i += 1
            continue
        if ch == "(" and not in_class:
            # A real, unescaped group opening: only here can it start a
            # lookahead or lookbehind. An escaped "(" never reaches this
            # branch (the backslash arm above consumes it whole), and one
            # inside a class is just a class member, not a group.
            if pattern[i : i + 3] in ("(?=", "(?!") or pattern[i : i + 4] in ("(?<=", "(?<!"):
                raise ManifestSchemaMismatch("uses lookaround, which RE2 does not support")
            out.append(ch)
            i += 1
            continue
        if ch == "$" and not in_class and not has_multiline:
            out.append("\\Z")
            i += 1
            continue
        out.append(ch)
        i += 1
    return "".join(out)


def _compile_pattern(pattern: str, ctx: str, kind: str) -> re.Pattern[str]:
    try:
        translated = translate_rust_regex(pattern)
    except ManifestSchemaMismatch as exc:
        raise ManifestSchemaMismatch(f"{ctx} has an invalid {kind} {pattern!r}: {exc}") from exc
    try:
        return re.compile(translated, re.ASCII)
    except re.error as exc:
        raise ManifestSchemaMismatch(f"{ctx} has an invalid {kind} {pattern!r}: {exc}") from exc


# manifest.go lowercases a contains needle, and evaluate.go lowercases the
# region text, with Go's strings.ToLower: a simple, per-rune mapping. Python's
# str.lower() follows full Unicode SpecialCasing instead, which for exactly
# one codepoint in the whole Unicode range -- U+0130, LATIN CAPITAL LETTER I
# WITH DOT ABOVE -- expands one character into two (a combining dot above
# survives the fold). Go's strings.ToLower drops that dot and produces plain
# "i". Verified by comparing strings.ToLower against str.lower() for every
# codepoint 0 to 0x10FFFF: this is the only one where a per-character
# str.lower() produces more than one character.
_GO_LOWER_OVERRIDES = {0x0130: "i"}


def _go_lower(s: str) -> str:
    """Lowercase the way Go's strings.ToLower does: per rune, never
    Python's whole-string Unicode case folding.

    Always per character, with no fast path for the common case: Python's
    whole-string str.lower() applies the Final_Sigma rule, folding a
    capital sigma at the end of a word to the final-form lowercase sigma
    (ς) instead of the regular one (σ) Go's simple per-rune fold
    always produces. That divergence needs no U+0130 in the string at all,
    so a fast path keyed on U+0130 alone reintroduces it. Calling
    ``str.lower()`` on one character at a time has no word context left to
    apply Final_Sigma with, which is why the per-character loop alone
    already agrees with Go on every codepoint from 0 to 0x10FFFF, checked
    directly against Go's own strings.ToLower.
    """
    return "".join(_GO_LOWER_OVERRIDES.get(ord(c), c.lower()) for c in s)


# --- regions.go: slicing the screen the way a rule's region asks for -----


def _lines(s: str) -> list[str]:
    # Only the single final terminator is stripped, matching Herdr: every
    # blank line the screen actually has, including trailing ones, stays.
    return s.removesuffix("\n").split("\n")


def _join_from(ls: list[str], i: int) -> str:
    if i >= len(ls):
        return ""
    return "\n".join(ls[i:])


def _bottom_lines(c: str, n: int) -> str:
    ls = _lines(c)
    start = max(len(ls) - n, 0)
    return _join_from(ls, start)


def _bottom_non_empty(c: str, n: int) -> str:
    ls = _lines(c)
    seen, start = 0, -1
    for i in range(len(ls) - 1, -1, -1):
        if ls[i].strip():
            seen += 1
            start = i
            if seen == n:
                break
    if start < 0:
        return ""
    return _join_from(ls, start)


def _top_non_empty(c: str, n: int) -> str:
    ls = _lines(c)
    seen, end = 0, -1
    for i, line in enumerate(ls):
        if line.strip():
            seen += 1
            end = i
            if seen == n:
                break
    if end < 0:
        return ""
    return "\n".join(ls[: end + 1])


def _codex_prompt_line(line: str) -> bool:
    return line == "›" or line.startswith("› ")


def _codex_block_marker_line(line: str) -> bool:
    return line.startswith(("•", "■", "✗", "✓"))


def _last_index(ls: list[str], pred) -> int | None:
    for i in range(len(ls) - 1, -1, -1):
        if pred(ls[i]):
            return i
    return None


def _current_prompt_index(ls: list[str]) -> int | None:
    """The last prompt marker, unless a block marker follows it.

    A block marker after the prompt means the agent started working again.
    """
    i = _last_index(ls, _codex_prompt_line)
    if i is None:
        return None
    for line in ls[i + 1 :]:
        if _codex_block_marker_line(line):
            return None
    return i


def _after_last_prompt_marker(c: str) -> str:
    ls = _lines(c)
    i = _last_index(ls, _codex_prompt_line)
    if i is None:
        return c
    return _join_from(ls, i + 1)


def _before_current_prompt_marker(c: str) -> str:
    ls = _lines(c)
    i = _current_prompt_index(ls)
    if i is None:
        return c
    return "\n".join(ls[:i])


def _current_prompt_block_marker(c: str) -> str:
    ls = _lines(c)
    i = _current_prompt_index(ls)
    if i is None:
        return ""
    j = _last_index(ls[:i], _codex_block_marker_line)
    if j is None:
        return ""
    return ls[j]


def _after_current_prompt_block_marker(c: str) -> str:
    ls = _lines(c)
    i = _current_prompt_index(ls)
    if i is None:
        return ""
    j = _last_index(ls[:i], _codex_block_marker_line)
    if j is None:
        return ""
    return _join_from(ls, j)


def _is_horizontal_rule(line: str) -> bool:
    """A line of box-drawing dashes, with a label allowed once the run is
    at least three characters long."""
    t = line.strip()
    if not t:
        return False
    n = 0
    for ch in t:
        if ch != "─":
            break
        n += 1
    if n == 0:
        return False
    rest = t[n:].lstrip(" \t")
    return rest == "" or n >= 3


def _prompt_box_top_index(ls: list[str]) -> int | None:
    """The second horizontal rule counting up from the bottom: the top
    border of the box the cursor sits in."""
    count = 0
    for i in range(len(ls) - 1, -1, -1):
        if _is_horizontal_rule(ls[i]):
            count += 1
            if count == 2:
                return i
    return None


def _prompt_box_body(c: str) -> str:
    ls = _lines(c)
    top = _prompt_box_top_index(ls)
    if top is None:
        return ""
    end = len(ls)
    for i in range(top + 1, len(ls)):
        if _is_horizontal_rule(ls[i]):
            end = i
            break
    if top + 1 >= end:
        return ""
    return "\n".join(ls[top + 1 : end])


def _above_prompt_box(c: str) -> str:
    ls = _lines(c)
    top = _prompt_box_top_index(ls)
    if top is None:
        return c
    return "\n".join(ls[:top])


def _after_last_horizontal_rule(c: str) -> str:
    ls = _lines(c)
    last = -1
    for i, line in enumerate(ls):
        if _is_horizontal_rule(line):
            last = i
    return _join_from(ls, last + 1)


def _last_non_empty_line(c: str) -> str:
    ls = _lines(c)
    for i in range(len(ls) - 1, -1, -1):
        if ls[i].strip():
            return ls[i]
    return ""


def _region_count(spec: str, name: str) -> int | None:
    """Parse ``name(N)``. N is ASCII digits only -- Herdr's count is an
    unsigned size, not a signed integer, so a leading "-" is refused, not
    silently parsed into a negative count. A leading zero is allowed:
    ``bottom_lines(0)`` is a legitimate, if useless, region."""
    if not spec.startswith(name):
        return None
    rest = spec[len(name) :]
    if not rest.startswith("(") or not rest.endswith(")"):
        return None
    digits = rest[1:-1]
    if not digits or not all("0" <= ch <= "9" for ch in digits):
        return None
    return int(digits)


def _top_region_count(spec: str) -> int | None:
    """Parse ``top_non_empty_lines(N)``. Stricter than the others: a
    positive decimal, no leading zero, at most the max."""
    if not spec.startswith("top_non_empty_lines"):
        return None
    rest = spec[len("top_non_empty_lines") :]
    if not rest.startswith("(") or not rest.endswith(")"):
        return None
    digits = rest[1:-1]
    if not digits or digits[0] == "0":
        return None
    if not all("0" <= ch <= "9" for ch in digits):
        return None
    n = int(digits)
    if n > schema.TOP_NON_EMPTY_LINES_MAX_N:
        return None
    return n


def valid_region(spec: str) -> bool:
    """An unknown region name is a load error, never an empty region: a
    rule that can never match is a bug nobody would notice."""
    s = spec.strip()
    if s in schema.FIXED_REGIONS:
        return True
    if _region_count(s, "bottom_lines") is not None:
        return True
    if _region_count(s, "bottom_non_empty_lines") is not None:
        return True
    return _top_region_count(s) is not None


def _region_text(spec: str, screen: str, title: str, osc_progress: str) -> str:
    """Slice the screen, title, or OSC progress the way ``spec`` asks for.

    Read against the whole unwrapped screen -- there is no fixed-line
    window underneath this; every region narrows the whole thing from
    here, or reads the title/progress fields directly.
    """
    s = spec.strip()
    if s == "osc_title":
        return title
    if s == "osc_progress":
        return osc_progress
    c = screen
    if s == "whole_recent":
        return c
    if s == "after_last_prompt_marker":
        return _after_last_prompt_marker(c)
    if s == "before_current_prompt_marker":
        return _before_current_prompt_marker(c)
    if s == "whole_recent_without_current_prompt_marker":
        if _current_prompt_index(_lines(c)) is not None:
            return ""
        return c
    if s == "current_prompt_block_marker":
        return _current_prompt_block_marker(c)
    if s == "after_current_prompt_block_marker":
        return _after_current_prompt_block_marker(c)
    if s == "prompt_box_body":
        return _prompt_box_body(c)
    if s == "above_prompt_box":
        return _above_prompt_box(c)
    if s == "last_non_empty_above_prompt_box":
        return _last_non_empty_line(_above_prompt_box(c))
    if s == "after_last_horizontal_rule":
        return _after_last_horizontal_rule(c)
    n = _region_count(s, "bottom_lines")
    if n is not None:
        return _bottom_lines(c, n)
    n = _region_count(s, "bottom_non_empty_lines")
    if n is not None:
        return _bottom_non_empty(c, n)
    n = _top_region_count(s)
    if n is not None:
        return _top_non_empty(c, n)
    return ""


# --- manifest.go: loading and validating one manifest ---------------------


class _Totals:
    """Manifest-wide gate and matcher counters, shared across every rule."""

    __slots__ = ("gates", "matchers")

    def __init__(self) -> None:
        self.gates = 0
        self.matchers = 0


def _validate_keys(raw: dict, allowed: frozenset, ctx: str) -> None:
    unknown = sorted(set(raw) - allowed)
    if unknown:
        raise ManifestSchemaMismatch(f"{ctx} uses keys the schema does not name: {unknown}")


def _as_str_list(value: object, ctx: str, key: str) -> list[str]:
    if value is None:
        return []
    if not isinstance(value, list) or not all(isinstance(v, str) for v in value):
        raise ManifestSchemaMismatch(f"{ctx} key {key!r} must be a list of strings")
    return value


def _as_bool(raw: dict, key: str, ctx: str) -> bool:
    """A present, non-bool value fails the load: no coercion, no default
    the way ``bool(1)`` would silently accept."""
    value = raw.get(key, False)
    if not isinstance(value, bool):
        raise ManifestSchemaMismatch(f"{ctx} {key} must be a boolean")
    return value


def _as_gate_list(value: object, ctx: str, key: str) -> list:
    """The raw all/any/not entries, type-checked before any len() call.

    A non-list value here (an int, say) must fail the load the same way a
    non-list contains/regex/line_regex does, not raise a bare TypeError out
    of the caller.
    """
    if value is None:
        return []
    if not isinstance(value, list):
        raise ManifestSchemaMismatch(f"{ctx} key {key!r} must be a list of tables")
    return value


def _compile_gate(raw: dict, ctx: str, depth: int, totals: _Totals) -> Gate:
    """Compile one gate: a rule's own matchers, or a nested table.

    The caller has already validated ``raw``'s keys against the right key
    set -- ``RULE_KEYS`` for a rule's own top-level matchers,
    ``MATCHER_KEYS`` for a nested gate table -- since a rule dict carries
    more keys than a bare gate does.
    """
    if depth > schema.MAX_GATE_DEPTH:
        raise ManifestSchemaMismatch(f"{ctx} exceeds max gate depth {schema.MAX_GATE_DEPTH}")
    totals.gates += 1
    if totals.gates > schema.MAX_TOTAL_GATES:
        raise ManifestSchemaMismatch(f"manifest exceeds max gate count {schema.MAX_TOTAL_GATES}")

    contains_raw = _as_str_list(raw.get("contains"), ctx, "contains")
    regex_raw = _as_str_list(raw.get("regex"), ctx, "regex")
    line_regex_raw = _as_str_list(raw.get("line_regex"), ctx, "line_regex")
    direct = len(contains_raw) + len(regex_raw) + len(line_regex_raw)
    if direct > schema.MAX_MATCHERS_PER_GATE:
        raise ManifestSchemaMismatch(
            f"{ctx} has {direct} direct matchers, max is {schema.MAX_MATCHERS_PER_GATE}"
        )
    totals.matchers += direct
    if totals.matchers > schema.MAX_TOTAL_MATCHERS:
        raise ManifestSchemaMismatch(
            f"manifest exceeds max matcher count {schema.MAX_TOTAL_MATCHERS}"
        )

    all_raw = _as_gate_list(raw.get("all"), ctx, "all")
    any_raw = _as_gate_list(raw.get("any"), ctx, "any")
    not_raw = _as_gate_list(raw.get("not"), ctx, "not")
    if direct + len(all_raw) + len(any_raw) == 0:
        raise ManifestSchemaMismatch(f"{ctx} must contain a positive matcher")

    for s in (*contains_raw, *regex_raw, *line_regex_raw):
        if len(s) > schema.MAX_MATCHER_CHARS:
            raise ManifestSchemaMismatch(
                f"{ctx} matcher exceeds max length {schema.MAX_MATCHER_CHARS}"
            )

    contains = tuple(_go_lower(s) for s in contains_raw)
    regex = tuple(_compile_pattern(p, ctx, "regex") for p in regex_raw)
    line_regex = tuple(_compile_pattern(p, ctx, "line_regex") for p in line_regex_raw)

    def _compile_nested(raw_list: object, kind: str) -> tuple[Gate, ...]:
        out = []
        for g in raw_list or []:
            if not isinstance(g, dict):
                raise ManifestSchemaMismatch(f"{ctx} > {kind} entries must be tables")
            _validate_keys(g, schema.MATCHER_KEYS, f"{ctx} > {kind}")
            out.append(_compile_gate(g, f"{ctx} > {kind}", depth + 1, totals))
        return tuple(out)

    return Gate(
        contains=contains,
        regex=regex,
        line_regex=line_regex,
        all=_compile_nested(all_raw, "all"),
        any=_compile_nested(any_raw, "any"),
        not_gates=_compile_nested(not_raw, "not"),
    )


def _compile_rule(
    raw: object, manifest_min_engine_version: int, index: int, totals: _Totals
) -> Rule:
    if not isinstance(raw, dict):
        raise ManifestSchemaMismatch(f"rule {index} must be a table")
    _validate_keys(raw, schema.RULE_KEYS, f"rule {index}")

    rule_id = raw.get("id")
    if not isinstance(rule_id, str) or not rule_id.strip():
        raise ManifestSchemaMismatch(f"rule {index} has an empty id")
    ctx = f"rule {rule_id}"

    state_raw = raw.get("state")
    if state_raw is None:
        state = schema.DEFAULT_RULE_STATE
    elif isinstance(state_raw, str):
        state = state_raw
    else:
        raise ManifestSchemaMismatch(f"{ctx} has a non-string state")
    if state not in schema.RULE_STATES:
        raise ManifestSchemaMismatch(f"{ctx} has state {state!r}")

    priority = raw.get("priority", schema.DEFAULT_PRIORITY)
    if not isinstance(priority, int) or isinstance(priority, bool):
        raise ManifestSchemaMismatch(f"{ctx} priority must be an integer")

    region = raw.get("region")
    if region is None or region == "":
        region = schema.DEFAULT_REGION
    elif not isinstance(region, str):
        raise ManifestSchemaMismatch(f"{ctx} region must be a string")
    if not valid_region(region):
        raise ManifestSchemaMismatch(f"{ctx} uses invalid region: {region}")
    if region.strip().startswith("top_non_empty_lines("):
        gated = (
            manifest_min_engine_version != 0
            and manifest_min_engine_version < schema.TOP_NON_EMPTY_LINES_MIN_ENGINE_VERSION
        )
        if gated:
            raise ManifestSchemaMismatch(
                f"{ctx} uses top_non_empty_lines but min_engine_version is "
                f"{manifest_min_engine_version}, need "
                f"{schema.TOP_NON_EMPTY_LINES_MIN_ENGINE_VERSION}"
            )

    skip = _as_bool(raw, "skip_state_update", ctx)
    visible_idle = _as_bool(raw, "visible_idle", ctx)
    visible_blocker = _as_bool(raw, "visible_blocker", ctx)
    visible_working = _as_bool(raw, "visible_working", ctx)
    if skip:
        if state != "unknown":
            raise ManifestSchemaMismatch(f'{ctx} uses skip_state_update without state = "unknown"')
        if visible_idle or visible_blocker or visible_working:
            raise ManifestSchemaMismatch(
                f"{ctx} uses skip_state_update with visible state evidence"
            )

    gate = _compile_gate(raw, ctx, 0, totals)
    return Rule(
        id=rule_id,
        state=state,
        priority=priority,
        region=region,
        gate=gate,
        skip_state_update=skip,
        visible_idle=visible_idle,
        visible_blocker=visible_blocker,
        visible_working=visible_working,
    )


def parse_manifest(path: Path, text: str) -> Manifest:
    """Read and fully validate one manifest. Every failure is a load
    error: a manifest that half-loads would produce detections nobody can
    explain, which is worse than an agent that is simply not detected."""
    try:
        doc = tomllib.loads(text)
    except tomllib.TOMLDecodeError as exc:
        raise ManifestSchemaMismatch(f"{path.name}: {exc}") from exc

    _validate_keys(doc, schema.MANIFEST_KEYS, path.name)

    manifest_id = doc.get("id")
    if not isinstance(manifest_id, str) or not manifest_id.strip():
        raise ManifestSchemaMismatch(f"{path.name}: id must not be empty")

    min_engine_version = doc.get("min_engine_version", 0)
    if not isinstance(min_engine_version, int) or isinstance(min_engine_version, bool):
        raise ManifestSchemaMismatch(f"{path.name}: min_engine_version must be an integer")
    if min_engine_version > schema.ENGINE_VERSION:
        raise ManifestSchemaMismatch(
            f"{path.name}: needs detection engine {min_engine_version}, "
            f"this build is engine {schema.ENGINE_VERSION}"
        )

    for key in ("version", "updated_at"):
        value = doc.get(key)
        if value is not None and not isinstance(value, str):
            raise ManifestSchemaMismatch(f"{path.name}: {key} must be a string")
    aliases = doc.get("aliases")
    if aliases is not None and (
        not isinstance(aliases, list) or not all(isinstance(a, str) for a in aliases)
    ):
        raise ManifestSchemaMismatch(f"{path.name}: aliases must be a list of strings")

    raw_rules = doc.get("rules")
    if not isinstance(raw_rules, list) or not raw_rules:
        raise ManifestSchemaMismatch(f"{path.name}: manifest must contain at least one rule")
    if len(raw_rules) > schema.MAX_RULES:
        raise ManifestSchemaMismatch(
            f"{path.name}: {len(raw_rules)} rules, the cap is {schema.MAX_RULES}"
        )

    totals = _Totals()
    rules = tuple(_compile_rule(r, min_engine_version, i, totals) for i, r in enumerate(raw_rules))
    return Manifest(agent=manifest_id, rules=rules, source=path, aliases=tuple(aliases or ()))


def load_manifests(dirs: Sequence[Path]) -> dict[str, Manifest]:
    """Load every ``*.toml`` file under ``dirs``, in order. Later directories
    override an earlier one's agent by id, mirroring ``set.go``'s bundled-
    then-override precedence.

    A manifest that fails to parse or validate is dropped whole and the
    rest still load: one broken file must not blind the fallback path to
    every other agent.
    """
    out: dict[str, Manifest] = {}
    for directory in dirs:
        if not directory.is_dir():
            continue
        for path in sorted(directory.glob("*.toml")):
            try:
                text = path.read_text(encoding="utf-8")
                manifest = parse_manifest(path, text)
            except (OSError, ManifestSchemaMismatch):
                continue
            out[manifest.agent] = manifest
    return out


# --- evaluate.go: running the rules -----------------------------------


def _gate_matches(gate: Gate, text: str, lower: str) -> bool:
    for needle in gate.contains:
        if needle not in lower:
            return False
    for pattern in gate.regex:
        if not pattern.search(text):
            return False
    if gate.line_regex:
        ls = text.split("\n")
        for pattern in gate.line_regex:
            if not any(pattern.search(line) for line in ls):
                return False
    for nested in gate.all:
        if not _gate_matches(nested, text, lower):
            return False
    if gate.any:
        if not any(_gate_matches(nested, text, lower) for nested in gate.any):
            return False
    for nested in gate.not_gates:
        if _gate_matches(nested, text, lower):
            return False
    return True


_NO_MATCH = Match(state="unknown", rule=None, priority=0, region=None, skip=False)


def _evaluate(manifest: Manifest, screen: str, title: str, osc_progress: str) -> Match:
    """Run every rule in file order and keep the highest-priority match.

    A tie keeps the earlier rule, exactly as Herdr does. A winning
    ``skip_state_update`` rule reports its own id, priority and region with
    ``skip=True`` and state ``"unknown"`` (the state ``skip_state_update``
    is required to declare); a true no-match reports ``_NO_MATCH``. Both
    carry state ``"unknown"``, matching evaluate.go's own Result, but only
    the skip case names a rule.
    """
    best: Rule | None = None
    for rule in manifest.rules:
        text = _region_text(rule.region, screen, title, osc_progress)
        if _gate_matches(rule.gate, text, _go_lower(text)):
            if best is not None and best.priority >= rule.priority:
                continue
            best = rule
    if best is None:
        return _NO_MATCH
    return Match(
        state=best.state,
        rule=best.id,
        priority=best.priority,
        region=best.region,
        skip=best.skip_state_update,
    )


def _resolve_agent(agent: str, manifests: dict[str, Manifest]) -> Manifest | None:
    """Resolve an agent id or alias to its manifest, mirroring set.go's
    Set.For: a real agent id always wins, even when some other manifest
    also declares it as an alias."""
    direct = manifests.get(agent)
    if direct is not None:
        return direct
    for manifest in manifests.values():
        if agent in manifest.aliases:
            return manifest
    return None


def classify(
    screen: str,
    *,
    title: str = "",
    osc_progress: str | None,
    agent_hint: str | None,
    manifests: dict[str, Manifest],
) -> Match:
    """Return the winning ``Match`` for one screen.

    With an ``agent_hint`` only that agent's manifest runs. Without one,
    every manifest runs. Agreement is judged on the answer, ``(state,
    rule, skip)``, not on the whole ``Match``: several vendored manifests
    share a rule id such as ``osc_title_working`` at different
    priorities, and that is agreement, not a disagreement to collapse.
    When every manifest that matches agrees on the answer, the returned
    ``Match`` is the highest-priority one among them, so its ``priority``
    and ``region`` are one manifest's own facts, not a merge or an
    average of several. Two manifests naming a genuinely different answer
    still collapse to ``_NO_MATCH``: an ambiguous screen is not evidence,
    and master spec 3.1 forbids inventing ``idle``.
    """
    osc = osc_progress or ""
    if agent_hint is not None:
        manifest = _resolve_agent(agent_hint, manifests)
        if manifest is None:
            return _NO_MATCH
        return _evaluate(manifest, screen, title, osc)
    hits: dict[tuple[str, str | None, bool], Match] = {}
    for manifest in manifests.values():
        result = _evaluate(manifest, screen, title, osc)
        if result.state == "unknown":
            continue
        key = (result.state, result.rule, result.skip)
        current = hits.get(key)
        if current is None or result.priority > current.priority:
            hits[key] = result
    if len(hits) == 1:
        return next(iter(hits.values()))
    return _NO_MATCH
