"""Python ``re`` subset → Z3 regex translator (v0.11.0).

Makes the ``Matches`` predicate-algebra operator compile to real Z3 regex
terms instead of a Python-evaluated bool wrapped in z3.BoolVal. When the
authored regex falls outside the supported subset, ``translate`` raises
``UnsupportedRegexError`` and the caller falls back to a soft Z3 Bool
variable whose truth value is resolved at Stage 2 by Python ``re``. Every
pattern the translator accepts is reasoned about symbolically by Z3.

Supported:
    literals, ``.``, character classes ``[abc]``, ranges ``[a-z]``, negation
    ``[^…]``, alternation ``|``, quantifiers ``*`` ``+`` ``?`` ``{n}``
    ``{n,m}``, groups ``(...)``, anchors ``^`` and ``$``, category
    escapes ``\\d`` ``\\w`` ``\\s`` (and negated forms).

Unsupported (raise ``UnsupportedRegexError``):
    lookaround, backreferences, inline flags ``(?i)``, word boundaries
    ``\\b``, named groups.

Search vs full-match: Python ``re.search`` does substring matching. Z3
``InRe`` is full-string. The translator wraps the compiled regex with
``Star(AllChar)`` padding unless anchored, preserving search semantics.
"""

from __future__ import annotations

import sre_parse
from sre_constants import (  # type: ignore[import-untyped]
    ANY,
    ASSERT,
    ASSERT_NOT,
    AT,
    AT_BEGINNING,
    AT_BEGINNING_STRING,
    AT_BOUNDARY,
    AT_END,
    AT_END_STRING,
    AT_NON_BOUNDARY,
    BRANCH,
    CATEGORY,
    CATEGORY_DIGIT,
    CATEGORY_NOT_DIGIT,
    CATEGORY_NOT_SPACE,
    CATEGORY_NOT_WORD,
    CATEGORY_SPACE,
    CATEGORY_WORD,
    GROUPREF,
    IN,
    LITERAL,
    MAX_REPEAT,
    MIN_REPEAT,
    NEGATE,
    NOT_LITERAL,
    RANGE,
    SUBPATTERN,
)

import z3


class UnsupportedRegexError(ValueError):
    """Authored regex uses a feature the Z3 translator does not support."""


_NEWLINE = chr(0x0A)

# Alphabet for ``.`` and negated character classes. Pre-v0.28.4 this was
# Range(0x20, 0x7e) — printable ASCII without newline. That was deliberately
# narrow for Z3 state-size reasons, but produced a soundness bug at
# subsumption: an outer regex that explicitly bounded to ASCII (e.g.
# ``[ -~]+``) was treated as IDENTICAL to inner's ``.+`` even though the
# inner Python regex admits non-ASCII (``é``, ``中``, …) that the outer
# would reject — Z3 returned ``unsat`` (subsumed), reality had a
# non-ASCII counterexample. Closed in v0.28.4 by widening to the
# Basic Multilingual Plane minus the newline (BMP is Z3's full Unicode
# string-sort range — supplementary plane codepoints 0x10000–0x10FFFF
# silently disappear if requested). This covers Latin-extended, CJK,
# IPA, math symbols — every common authoring char a Python regex would
# match on. Trade-off: Z3 returns ``unknown`` more often on wide-alphabet
# string-theory checks, but ``unknown`` fails closed via
# ``VerificationTimeout``, so incompleteness is recoverable; unsoundness
# was not.
_ANY_CHAR_LO = chr(0x00)
_ANY_CHAR_HI = chr(0xFFFF)


def _any_char() -> z3.ReRef:
    # ``.`` in Python ``re`` excludes ONLY newline by default. We match that
    # exactly: BMP Unicode minus ``\n``.
    return z3.Intersect(
        z3.Range(_ANY_CHAR_LO, _ANY_CHAR_HI),
        z3.Complement(z3.Re(_NEWLINE)),
    )


def _digit() -> z3.ReRef:
    return z3.Range("0", "9")


def _word() -> z3.ReRef:
    return z3.Union(
        z3.Range("a", "z"),
        z3.Range("A", "Z"),
        z3.Range("0", "9"),
        z3.Re("_"),
    )


def _space() -> z3.ReRef:
    return z3.Union(z3.Re(" "), z3.Re("\t"), z3.Re("\n"), z3.Re("\r"))


def _complement_char(r: z3.ReRef) -> z3.ReRef:
    # "Any printable ASCII char not in r." Z3's Intersect+Complement is
    # the sound encoding.
    return z3.Intersect(_any_char(), z3.Complement(r))


def _category_regex(cat):
    if cat == CATEGORY_DIGIT:
        return _digit()
    if cat == CATEGORY_NOT_DIGIT:
        return _complement_char(_digit())
    if cat == CATEGORY_WORD:
        return _word()
    if cat == CATEGORY_NOT_WORD:
        return _complement_char(_word())
    if cat == CATEGORY_SPACE:
        return _space()
    if cat == CATEGORY_NOT_SPACE:
        return _complement_char(_space())
    raise UnsupportedRegexError(f"unsupported category {cat!r}")


def _node_to_regex(op, arg) -> z3.ReRef:
    if op == LITERAL:
        return z3.Re(chr(arg))
    if op == NOT_LITERAL:
        return _complement_char(z3.Re(chr(arg)))
    if op == ANY:
        return _any_char()
    if op == IN:
        # arg is list of (sub_op, sub_arg). First element may be NEGATE.
        items = list(arg)
        negate = bool(items and items[0][0] == NEGATE)
        if negate:
            items = items[1:]
        pieces: list[z3.ReRef] = []
        for sub_op, sub_arg in items:
            if sub_op == LITERAL:
                pieces.append(z3.Re(chr(sub_arg)))
            elif sub_op == RANGE:
                lo, hi = sub_arg
                pieces.append(z3.Range(chr(lo), chr(hi)))
            elif sub_op == CATEGORY:
                pieces.append(_category_regex(sub_arg))
            else:
                raise UnsupportedRegexError(
                    f"unsupported char-class element {sub_op!r}"
                )
        if not pieces:
            raise UnsupportedRegexError("empty character class")
        unioned = pieces[0] if len(pieces) == 1 else z3.Union(*pieces)
        return _complement_char(unioned) if negate else unioned
    if op == BRANCH:
        _, alts = arg
        pieces = [_pattern_to_regex(alt) for alt in alts]
        if not pieces:
            raise UnsupportedRegexError("empty branch")
        return pieces[0] if len(pieces) == 1 else z3.Union(*pieces)
    if op in (MAX_REPEAT, MIN_REPEAT):
        lo, hi, body = arg
        inner = _pattern_to_regex(body)
        if hi == sre_parse.MAXREPEAT:
            if lo == 0:
                return z3.Star(inner)
            if lo == 1:
                return z3.Plus(inner)
            return z3.Concat(*([inner] * lo), z3.Star(inner))
        if lo == 0 and hi == 1:
            return z3.Option(inner)
        if lo == hi:
            return z3.Concat(*([inner] * lo)) if lo > 0 else z3.Re("")
        return z3.Loop(inner, lo, hi)
    if op == SUBPATTERN:
        # (group_num_or_None, add_flags, del_flags, pattern)
        _, add_flags, del_flags, body = arg
        if add_flags or del_flags:
            raise UnsupportedRegexError(
                "inline regex flags (?i) / (?m) not supported"
            )
        return _pattern_to_regex(body)
    if op == CATEGORY:
        return _category_regex(arg)
    if op == AT:
        # Anchors handled at top level; a stray AT inside a group is fine
        # only if it matches trivially — this path is rarely hit.
        if arg in (AT_BEGINNING, AT_END, AT_BEGINNING_STRING, AT_END_STRING):
            return z3.Re("")
        raise UnsupportedRegexError(
            f"anchor {arg!r} (word boundaries / non-start-end) not supported"
        )
    if op in (ASSERT, ASSERT_NOT):
        raise UnsupportedRegexError("lookahead/lookbehind not supported")
    if op == GROUPREF:
        raise UnsupportedRegexError("backreferences not supported")
    raise UnsupportedRegexError(f"unsupported regex op {op!r}")


def _pattern_to_regex(pattern) -> z3.ReRef:
    pieces: list[z3.ReRef] = []
    for op, arg in pattern:
        if op == AT:
            # Only ^/$ anchors are tolerated — boundary matchers (\b, \B)
            # have no Z3 regex equivalent without a full lexer context.
            if arg in (AT_BEGINNING, AT_END, AT_BEGINNING_STRING, AT_END_STRING):
                continue
            raise UnsupportedRegexError(
                f"regex anchor {arg!r} (word boundary / non-start-end) not supported"
            )
        pieces.append(_node_to_regex(op, arg))
    if not pieces:
        return z3.Re("")
    if len(pieces) == 1:
        return pieces[0]
    return z3.Concat(*pieces)


def _peel_anchors(pattern):
    """Return (anchor_start, anchor_end, body) after stripping ^/$ at ends."""
    items = list(pattern)
    anchor_start = False
    anchor_end = False
    if items and items[0][0] == AT and items[0][1] in (
        AT_BEGINNING,
        AT_BEGINNING_STRING,
    ):
        anchor_start = True
        items = items[1:]
    if items and items[-1][0] == AT and items[-1][1] in (
        AT_END,
        AT_END_STRING,
    ):
        anchor_end = True
        items = items[:-1]
    return anchor_start, anchor_end, items


def translate(pattern: str) -> z3.ReRef:
    """Translate a Python regex string into a Z3 regex (``z3.ReRef``).

    The returned regex has ``re.search`` semantics: unanchored patterns are
    padded with ``Star(AllChar)`` so ``z3.InRe(s, translate(p))`` is true
    iff ``re.search(p, s)`` would be (modulo unsupported features).
    """
    try:
        parsed = sre_parse.parse(pattern)
    except Exception as e:
        raise UnsupportedRegexError(f"sre_parse failed on {pattern!r}: {e}") from e

    # Reject inline flags like ``(?i)``. ``parsed.state.flags`` encodes
    # both Python's default flags (UNICODE=32 for str patterns) and any
    # user-specified inline flags; subtract the baseline so only the
    # genuinely-inline bits remain.
    _DEFAULT_FLAGS = sre_parse.parse("").state.flags
    inline_flags = getattr(parsed, "state", parsed).flags & ~_DEFAULT_FLAGS
    if inline_flags:
        raise UnsupportedRegexError(
            f"inline regex flags {inline_flags!r} (e.g. (?i), (?m)) not supported"
        )

    anchor_start, anchor_end, body_items = _peel_anchors(parsed)
    body = _pattern_to_regex(body_items)

    left = z3.Re("") if anchor_start else z3.Star(_any_char())
    right = z3.Re("") if anchor_end else z3.Star(_any_char())
    return z3.Concat(left, body, right)


__all__ = ["translate", "UnsupportedRegexError"]
