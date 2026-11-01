"""The regex of an MCP argument rule: one small subset, whole string.

A rule regex runs on untrusted text in a hook with a time budget. Python
(google-re2), Go ``regexp`` and Rust ``regex`` must give the same answer, and
they do not agree on the corners of their syntax. So the warden accepts one
small subset and refuses everything else with one reason
(``regex '<p>' is outside the supported subset``). Inside the subset the
three engines agree. After the check each port runs its own engine for a
whole-string match.

The subset:

- literals, and ``\\`` before any ASCII punctuation;
- ``.``, ``^``, ``$``;
- ASCII classes ``[...]`` with ranges and a leading ``^``. Inside a class:
  literals, escaped punctuation, ranges, and ``\\d \\w \\s``. A ``-`` is a
  literal only first or last. No ``--``, ``&&``, ``~~``, no nested ``[``, no
  ``[:name:]``;
- ``\\d \\w \\s \\D \\W \\S`` (ASCII meaning);
- groups ``(...)`` and ``(?:...)``, and ``|``;
- quantifiers ``* + ? {m} {m,} {m,n}`` (m and n up to 1000, no leading zero,
  m <= n) and their lazy ``?`` forms; a quantifier follows an atom, not
  ``^`` or ``$``, and never another quantifier;
- the flag group ``(?i)`` only at the very start.

Three limits keep the ports alike: nested repeat counts multiply to at most
1000, groups nest at most 50 deep, and the pattern's expanded size (repeat
counts multiplied in) is at most 2000.

This validator is ported exactly to Go (``verify/rulesubset.go``) and Rust
(``rulesubset.rs``). ``clients/fixtures/rule_regex_subset.json`` lists accept
and refuse patterns; all three ports' unit tests read it.
"""

from __future__ import annotations

import functools
from typing import Any

MAX_REPEAT = 1000
MAX_DEPTH = 50
MAX_SIZE = 2000

_PUNCT = frozenset("!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~")
_NOT_LITERAL = frozenset("\\.^$[]()|*+?{}")
_CLASS_ESCAPES = frozenset("dws")
_ALL_CLASS_ESCAPES = frozenset("dwsDWS")


class _Outside(Exception):
    """The pattern is outside the subset."""


class _Parser:
    def __init__(self, pattern: str) -> None:
        self.s = pattern
        self.n = len(pattern)
        self.pos = 0

    def peek(self, k: int = 0) -> str:
        i = self.pos + k
        return self.s[i] if i < self.n else ""

    def run(self) -> None:
        if self.s.startswith("(?i)"):
            self.pos = 4
        size, prod = self.alt(0)
        if self.pos != self.n:
            raise _Outside  # a stray ")"
        if size > MAX_SIZE or prod > MAX_REPEAT:
            raise _Outside

    def alt(self, depth: int) -> tuple[int, int]:
        if depth > MAX_DEPTH:
            raise _Outside
        size = 0
        prod = 1
        while True:
            s, p = self.seq(depth)
            size += s
            prod = max(prod, p)
            if self.peek() == "|":
                self.pos += 1
                size += 1
                continue
            return size, prod

    def seq(self, depth: int) -> tuple[int, int]:
        size = 0
        prod = 1
        while self.pos < self.n and self.peek() not in ("|", ")"):
            a_size, a_prod, quantifiable = self.atom(depth)
            if quantifiable and self.peek() in ("*", "+", "?", "{"):
                f_size, f_prod = self.quant()
                a_size *= f_size
                a_prod *= f_prod
                if self.peek() in ("*", "+", "?", "{"):
                    raise _Outside  # a second quantifier
            size += a_size
            prod = max(prod, a_prod)
        return size, prod

    def atom(self, depth: int) -> tuple[int, int, bool]:
        c = self.peek()
        if c == "\\":
            e = self.peek(1)
            if e in _ALL_CLASS_ESCAPES or e in _PUNCT:
                self.pos += 2
                return 1, 1, True
            raise _Outside
        if c == ".":
            self.pos += 1
            return 1, 1, True
        if c in ("^", "$"):
            self.pos += 1
            return 1, 1, False
        if c == "[":
            self.klass()
            return 1, 1, True
        if c == "(":
            if self.s.startswith("(?:", self.pos):
                self.pos += 3
            elif self.peek(1) == "?":
                raise _Outside
            else:
                self.pos += 1
            size, prod = self.alt(depth + 1)
            if self.peek() != ")":
                raise _Outside
            self.pos += 1
            return size, prod, True
        if c in _NOT_LITERAL:
            raise _Outside  # a quantifier without an atom, a bare ] } or {
        self.pos += 1
        return 1, 1, True

    def quant(self) -> tuple[int, int]:
        """Parse a quantifier and its lazy ``?``. Returns (size factor, product factor)."""
        c = self.peek()
        size, prod = 1, 1
        self.pos += 1
        if c == "{":
            m = self.number()
            hi: int | None = m
            if self.peek() == ",":
                self.pos += 1
                hi = self.number() if self.peek().isascii() and self.peek().isdigit() else None
            if self.peek() != "}":
                raise _Outside
            self.pos += 1
            if hi is not None and m > hi:
                raise _Outside
            prod = hi if hi is not None else m
            prod = prod if prod > 0 else 1
            size = hi if hi is not None else m + 1
            size = size if size > 0 else 1
        if self.peek() == "?":
            self.pos += 1
        return size, prod

    def number(self) -> int:
        start = self.pos
        while self.peek() != "" and self.peek().isascii() and self.peek().isdigit():
            self.pos += 1
        digits = self.s[start : self.pos]
        if not digits or len(digits) > 4 or (len(digits) > 1 and digits[0] == "0"):
            raise _Outside
        value = int(digits)
        if value > MAX_REPEAT:
            raise _Outside
        return value

    def single(self) -> tuple[bool, int]:
        """One class item: (is_class_escape, code point)."""
        c = self.peek()
        if c == "":
            raise _Outside
        if c == "\\":
            e = self.peek(1)
            if e in _CLASS_ESCAPES:
                self.pos += 2
                return True, 0
            if e in _PUNCT:
                self.pos += 2
                return False, ord(e)
            raise _Outside
        if c == "[":
            raise _Outside
        self.pos += 1
        return False, ord(c)

    def klass(self) -> None:
        self.pos += 1  # [
        if self.peek() == "^":
            self.pos += 1
        items = 0
        while True:
            c = self.peek()
            if c == "":
                raise _Outside
            if c == "]":
                if items == 0:
                    raise _Outside
                self.pos += 1
                return
            if (c == "&" and self.peek(1) == "&") or (c == "~" and self.peek(1) == "~"):
                raise _Outside
            if c == "-":
                if self.peek(1) == "-" or self.s[self.pos - 1] == "-":
                    raise _Outside
                if items == 0 or self.peek(1) == "]":
                    self.pos += 1
                    items += 1
                    continue
                raise _Outside
            is_cls, lo = self.single()
            items += 1
            if self.peek() == "-" and self.peek(1) != "]":
                if is_cls or self.peek(1) == "-" or self.s[self.pos - 1] == "-":
                    raise _Outside
                self.pos += 1
                hi_cls, hi = self.single()
                if hi_cls or hi < lo:
                    raise _Outside


def supported(pattern: str) -> bool:
    """True when ``pattern`` is inside the subset."""
    if any("\ud800" <= ch <= "\udfff" for ch in pattern):
        return False
    try:
        _Parser(pattern).run()
    except _Outside:
        return False
    return True


@functools.lru_cache(maxsize=256)
def _compile(pattern: str) -> Any:
    import re2  # noqa: PLC0415 - kept off the hook's import path until a rule runs

    options = re2.Options()
    options.log_errors = False
    return re2.compile(pattern, options)


def compiles(pattern: str) -> bool:
    """True when ``pattern`` is in the subset and RE2 compiles it."""
    if not supported(pattern):
        return False
    try:
        _compile(pattern)
    except Exception:  # noqa: BLE001 - any compile failure refuses the rule
        return False
    return True


def fullmatch(pattern: str, text: str) -> bool:
    """True when ``pattern`` matches all of ``text``. Raises when it does not compile."""
    return _compile(pattern).fullmatch(text) is not None


def invalid_reason(pattern: str) -> str:
    """The one reason text for a pattern outside the subset."""
    return f"regex '{pattern}' is outside the supported subset"
