"""The system dialect: named words with definitions in the kernel.

A word is a name for a kernel term. The checker never trusts the name: it
replaces the word by its definition (it unfolds the word) and checks the
kernel term with the ordinary predicate code.

This module holds one word, ``keep_unchanged(target)``, and the invariant
type names that models write for the same meaning (its synonyms). An
invariant that names a word in its ``type``, with its glob in ``target``,
unfolds to

    forall_steps(forall_writes(not_matches(path, R(target))))

where ``R`` is ``glob_regex``: a regex that matches every normalized path
the file-scope glob matcher (``verify._match_glob``) matches. So no step
writes a path inside ``target``: no ``file_write`` step and no shell write
redirect, and no file a known command writes through its operands (``cp``,
``mv``, ``tee``, ``sed -i``, ``rm`` and others; see ``write_paths``). A
missing target is ``**``, every path. The words do not see the work of an
interpreter, nor of a command ``write_paths`` does not know.

The base. At the gate a call has a working directory, and a model writes
most targets relative (``src/**``) while the gate sees absolute paths. So
``unfold`` takes a ``base`` (the call's cwd): a relative target is joined
to it (``resolve_target``), and each relative write path is resolved
against it (``write_paths.resolve_writes``). A target that starts with
``/`` or ``**`` is left as written, so the default ``**`` still names
every path. With no base (a plan verified outside the gate) nothing is
resolved.

Synonyms unfold to the same word, so they are one word; the tests prove
their unfoldings equal with Z3.

Identity. ``DIALECT_HASH`` is the first 16 hex digits of the SHA-256 of
the canonical JSON of the definitions (parameters, bodies, synonyms, the
version of ``glob_regex`` and the version of the write paths, with the
resolution against a base). Descriptions are not part of it. A change to
any definition is a new hash.

Audit first. Until the operator pins this hash in ``config.yaml``
(``dialect_enforce``), a word is evaluated and its would-deny is recorded
as a warning (``AUDIT_PREFIX``), and the verdict does not change. With the
pin equal to ``DIALECT_HASH`` the word is enforced. With any other pin,
every use of a word is a violation: the operator pinned definitions this
build does not have.

The Go and Rust clients read ``DIALECT_JSON`` through generated tables
(``clients/go/internal/verify/dialect_gen.go``,
``clients/rust/src/dialect_gen.rs``; ``clients/gen_dialect.py``).
"""

from __future__ import annotations

import hashlib
import json
import posixpath
from typing import Any

KEEP_UNCHANGED = "keep_unchanged"

# The glob translation is part of every definition's meaning, so its
# version is part of the hash.
GLOB_REGEX_VERSION = 1

# What a step writes is part of every definition's meaning too: version 1
# was the redirects; version 2 adds the operand writes, the unknown write
# set of a line that moves its cwd, and the resolution against a base.
WRITE_PATHS_VERSION = 2

# The limits of a supported target glob. Past them a target is refused.
MAX_GLOB_CHARS = 1024
MAX_GLOB_DOUBLE_STARS = 4
MAX_GLOB_STARS = 16

WORDS: dict[str, dict[str, Any]] = {
    KEEP_UNCHANGED: {
        "params": [{"name": "target", "sort": "glob", "default": "**"}],
        "body": {
            "op": "forall_steps",
            "pred": {
                "op": "forall_writes",
                "pred": {"op": "not_matches", "path": "path", "regex": "$target"},
            },
        },
    }
}

DESCRIPTIONS: dict[str, str] = {
    KEEP_UNCHANGED: (
        "no step writes a path inside target (a file glob; ** when absent): no "
        "file_write step, no shell write redirect, and no file a known command "
        "writes through its operands (cp, mv, tee, sed -i, rm and others). At "
        "the gate a relative target and relative writes are placed from the "
        "call's cwd. Interpreter work is not seen."
    )
}

# Invariant type names that mean "these files stay unchanged". The names
# come from the invariant types models wrote in the owner's journal.
SYNONYMS: dict[str, str] = {
    name: KEEP_UNCHANGED
    for name in (
        "file_content_preservation",
        "file_immutability",
        "file_immutable",
        "file_preservation",
        "file_unchanged",
        "files_not_modified",
        "filesystem_readonly",
        "immutable_source",
        "no_file_modification",
        "no_file_modifications",
        "no_file_mutations",
        "no_file_writes",
        "no_filesystem_mutation",
        "no_local_modification",
        "no_local_modifications",
        "no_modification",
        "no_modifications",
        "no_project_modification",
        "no_source_modification",
        "read_only",
        "read_only_access",
        "read_only_operation",
        "read_only_operations",
    )
}

AUDIT_PREFIX = "dialect audit: "


def _canonical(obj: Any) -> str:
    return json.dumps(obj, sort_keys=True, separators=(",", ":"), ensure_ascii=True)


DIALECT_JSON = _canonical(
    {
        "glob_regex": GLOB_REGEX_VERSION,
        "synonyms": SYNONYMS,
        "version": 1,
        "words": WORDS,
        "write_paths": WRITE_PATHS_VERSION,
    }
)
DIALECT_HASH = hashlib.sha256(DIALECT_JSON.encode("ascii")).hexdigest()[:16]


class UnsupportedGlob(ValueError):
    """A target glob outside the supported set. It is never evaluated."""


def word_for(type_name: str) -> str | None:
    """The word an invariant type names, or None."""
    if type_name in WORDS:
        return type_name
    return SYNONYMS.get(type_name)


_REGEX_SPECIALS = frozenset("\\.^$*+?{}[]|()")


def _escape(text: str) -> str:
    return "".join("\\" + c if c in _REGEX_SPECIALS else c for c in text)


def _segment(seg: str) -> str:
    out = []
    for c in seg:
        if c == "*":
            out.append("[^/]*")
        elif c == "?":
            out.append("[^/]")
        else:
            out.append(_escape(c))
    return "".join(out)


_ANY_SEGMENTS = "(?:/[^/]*)*"
_END = "\\Z"


def _rest(elems: list[str | None]) -> str:
    return "".join(_ANY_SEGMENTS if e is None else "/" + e for e in elems)


def _first(elems: list[str | None]) -> str:
    if not elems:
        return ""
    head, tail = elems[0], elems[1:]
    if head is not None:
        return head + _rest(tail)
    return "(?:" + _first(tail) + "|[^/]*" + _ANY_SEGMENTS + _rest(tail) + ")"


def glob_regex(glob: str) -> str:
    """A regex for ``re.search`` that matches exactly the normalized paths
    the file-scope matcher matches with this glob.

    The regex ends in ``\\Z`` (the end of the string), not ``$``, which in
    Python also matches before a final newline. Refused: an empty glob,
    ``[`` or ``]``, a glob longer than ``MAX_GLOB_CHARS``, more than
    ``MAX_GLOB_DOUBLE_STARS`` ``**`` segments or ``MAX_GLOB_STARS`` stars,
    and a ``/**`` glob whose prefix normalizes to ``.``.
    """
    if not isinstance(glob, str) or glob == "":
        raise UnsupportedGlob("the glob is empty")
    if len(glob) > MAX_GLOB_CHARS:
        raise UnsupportedGlob(f"the glob is longer than {MAX_GLOB_CHARS} characters")
    if "[" in glob or "]" in glob:
        raise UnsupportedGlob("character classes ([...]) are not supported")
    if glob.count("*") > MAX_GLOB_STARS:
        raise UnsupportedGlob(f"the glob has more than {MAX_GLOB_STARS} stars")
    if glob.endswith("/**"):
        raw = glob[:-3]
        # On a normalized path, "starts with /" is "one or more /segment",
        # and "is prefix or starts with prefix/" is "prefix, then zero or
        # more /segment". Both keep their anchors at the top level, where
        # the Z3 translation reads them.
        if raw == "":
            return "^" + _ANY_SEGMENTS[:-1] + "+" + _END
        prefix = posixpath.normpath(raw)
        if prefix == ".":
            raise UnsupportedGlob("a /** glob whose prefix is . is not supported")
        return "^" + _escape(prefix) + _ANY_SEGMENTS + _END
    segs = glob.split("/")
    if sum(1 for s in segs if s == "**") > MAX_GLOB_DOUBLE_STARS:
        raise UnsupportedGlob(f"the glob has more than {MAX_GLOB_DOUBLE_STARS} ** segments")
    elems: list[str | None] = [None if s == "**" else _segment(s) for s in segs]
    return "^(?:" + _first(elems) + ")" + _END


def _fill(body: Any, holes: dict[str, str]) -> Any:
    """Fill typed holes: a string that is exactly ``$name`` becomes the hole's
    value. A value is never read again as a template."""
    if isinstance(body, str):
        if body.startswith("$") and body[1:] in holes:
            return holes[body[1:]]
        return body
    if isinstance(body, list):
        return [_fill(x, holes) for x in body]
    if isinstance(body, dict):
        return {k: _fill(v, holes) for k, v in body.items()}
    return body


def target_of(word: str, target: str | None) -> str:
    """The target a use of ``word`` names: the given one, else the default."""
    if target:
        return target
    return WORDS[word]["params"][0]["default"]


def resolve_target(target: str, base: str | None) -> str:
    """The target glob placed from ``base``, an absolute normalized
    directory. No base, an absolute target, or one that starts with ``**``
    is returned as written.

    Raises UnsupportedGlob when the base holds a glob character, when the
    target starts with ``~``, and when a target that does not end in
    ``/**`` has a ``.`` or ``..`` segment (a ``/**`` target's prefix is
    normalized by ``glob_regex``).
    """
    if base is None or target.startswith("/") or target.startswith("**"):
        return target
    if any(c in base for c in "*?[]"):
        raise UnsupportedGlob("the working directory holds a glob character")
    if target.startswith("~"):
        raise UnsupportedGlob("a target that starts with ~ cannot be placed from the cwd")
    if not target.endswith("/**") and any(s in (".", "..") for s in target.split("/")):
        raise UnsupportedGlob("a relative target with a . or .. segment cannot be placed")
    return base.rstrip("/") + "/" + target


def _set_base(expr: Any, base: str | None) -> None:
    """Give every forall_writes in ``expr`` the base its writes resolve against."""
    from opendaisugi.predicate import ForallWrites

    if isinstance(expr, ForallWrites):
        expr._base = base
    for name in type(expr).model_fields if hasattr(type(expr), "model_fields") else ():
        value = getattr(expr, name)
        for child in value if isinstance(value, list) else [value]:
            if hasattr(child, "model_fields"):
                _set_base(child, base)


def unfold(word: str, target: str, base: str | None = None) -> Any:
    """The kernel term of ``word(target)``, a parsed Expression, placed
    from ``base`` (see ``resolve_target``).

    Raises UnsupportedGlob for a target outside the supported set.
    """
    from opendaisugi.predicate import parse_expression

    holes: dict[str, str] = {}
    for param in WORDS[word]["params"]:
        if param["sort"] == "glob":
            holes[param["name"]] = glob_regex(resolve_target(target, base))
    expr = parse_expression(_fill(WORDS[word]["body"], holes))
    if base is not None:
        _set_base(expr, base)
    return expr


def witness(regex: str, plan: Any, base: str | None = None) -> str:
    """Why a keep_unchanged unfolding is false over ``plan``: the first step
    in order that writes a matching path, or whose writes cannot be read."""
    import re

    from opendaisugi.predicate_z3 import _step_to_dict
    from opendaisugi.write_paths import step_write_paths

    for step in plan.steps:
        d = _step_to_dict(step)
        writes = step_write_paths(d, base)
        if writes is None:
            return f"step '{d.get('id')}' has write paths that cannot be read"
        for path in writes:
            if re.search(regex, path) is not None:
                return f"step '{d.get('id')}' writes '{path}'"
    return "no step names the write"


__all__ = [
    "AUDIT_PREFIX",
    "DESCRIPTIONS",
    "DIALECT_HASH",
    "DIALECT_JSON",
    "KEEP_UNCHANGED",
    "WRITE_PATHS_VERSION",
    "SYNONYMS",
    "UnsupportedGlob",
    "WORDS",
    "glob_regex",
    "resolve_target",
    "target_of",
    "unfold",
    "witness",
    "word_for",
]
