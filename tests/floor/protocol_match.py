"""The matching language of the coppice protocol corpus, in Python.

This file is the Python copy of
harness/coppice/internal/proto/conformance_match.go. The two are kept
structurally parallel: the same constants, a Matcher with the same two
methods, the same helper functions in the same order, and the same rules
in the same order inside `_match`. When one changes, the other changes the
same way. harness/coppice/testdata/protocol/README.md states the rules in
prose.
"""

from __future__ import annotations

import json
from typing import Any

# SKIP_MARKER starts an expected line that names a case the server does not
# implement yet. A replay logs it as a gap. It never counts as a pass.
SKIP_MARKER = "#skip"

# WILDCARD, as a whole string value in an expected line, matches any present
# value.
WILDCARD = "*"

# ID_NAME, as a whole string value in an expected line, matches the id the
# request carried.
ID_NAME = "$id"


class Mismatch(Exception):
    """The path and the reason of the first mismatch."""


class Matcher:
    """Holds the values that expected lines bound with "$name". One Matcher
    serves one corpus file, because a file is replayed on one connection and
    its bindings carry across cases."""

    def __init__(self) -> None:
        self.bindings: dict[str, Any] = {}

    def match(self, expected: Any, actual: Any, req_id: Any) -> None:
        """Compare actual against expected and return when they agree.
        The rules, in order:

        - An expected string "*" matches any present value.
        - An expected string "$id" matches only req_id, the id the request
          carried.
        - Any other expected string "$name" matches any present value and
          binds it under name, replacing an older binding of the same name.
        - An expected object matches when every key it lists is present in
          the actual object and matches. Extra keys in the actual object
          are allowed.
        - An expected array matches an actual array of the same length
          whose elements match one to one.
        - Every other value matches only an equal JSON value.

        Raises Mismatch naming the path of the first mismatch.
        """
        self._match("", expected, actual, req_id)

    def _match(self, path: str, expected: Any, actual: Any, req_id: Any) -> None:
        if isinstance(expected, str):
            if expected == WILDCARD:
                return
            if expected == ID_NAME:
                if not _same_json(actual, req_id):
                    raise Mismatch(
                        f"{_at(path)}: want the request id {_json_of(req_id)}, "
                        f"got {_json_of(actual)}"
                    )
                return
            name = _binding_name(expected)
            if name is not None:
                self.bindings[name] = actual
                return
            if not isinstance(actual, str) or actual != expected:
                raise Mismatch(f"{_at(path)}: want {_json_of(expected)}, got {_json_of(actual)}")
            return
        if isinstance(expected, dict):
            if not isinstance(actual, dict):
                raise Mismatch(f"{_at(path)}: want an object, got {_json_of(actual)}")
            for key, ev in expected.items():
                if key not in actual:
                    raise Mismatch(f"{_at(path)}: key {key!r} is missing")
                self._match(_join(path, key), ev, actual[key], req_id)
            return
        if isinstance(expected, list):
            if not isinstance(actual, list):
                raise Mismatch(f"{_at(path)}: want an array, got {_json_of(actual)}")
            if len(actual) != len(expected):
                raise Mismatch(f"{_at(path)}: want {len(expected)} elements, got {len(actual)}")
            for i, ev in enumerate(expected):
                self._match(_join(path, str(i)), ev, actual[i], req_id)
            return
        if not _same_json(expected, actual):
            raise Mismatch(f"{_at(path)}: want {_json_of(expected)}, got {_json_of(actual)}")

    def substitute(self, v: Any) -> Any:
        """Return v with every string "$name" replaced by the value bound
        under name. A name with no binding is an error, so a corpus file
        cannot send a placeholder to the server by mistake. "$id" is never
        bound, and "*" is left as it is."""
        if isinstance(v, str):
            if v == ID_NAME:
                raise ValueError(f'"{v}" is never bound. A request line may not use it.')
            name = _binding_name(v)
            if name is None:
                return v
            if name not in self.bindings:
                raise ValueError(f'"{v}" is not bound. An expected line must bind it first.')
            return self.bindings[name]
        if isinstance(v, dict):
            return {k: self.substitute(ev) for k, ev in v.items()}
        if isinstance(v, list):
            return [self.substitute(ev) for ev in v]
        return v


def _binding_name(s: str) -> str | None:
    """The name when s is a "$name" placeholder other than "$id", else None."""
    if len(s) < 2 or s[0] != "$" or s == ID_NAME:
        return None
    return s[1:]


def request_id(req: Any) -> Any:
    """The id a decoded request line carries, or None when the line is not
    an object or has no id key. match compares "$id" against it."""
    if not isinstance(req, dict):
        return None
    return req.get("id")


def has_id(line: Any) -> bool:
    """Whether a decoded line is an object with an id key. A reply always
    has one, even when its value is null. An event or a notification has
    none, and a replay skips it while it waits for a reply."""
    return isinstance(line, dict) and "id" in line


def _same_json(a: Any, b: Any) -> bool:
    return _json_of(a) == _json_of(b)


def _json_of(v: Any) -> str:
    """One canonical text per JSON value, the way Go's json.Marshal gives
    one. Go decodes every number as float64 and prints 7.0 as 7, so an
    integral float here prints as an integer too. A bool never becomes a
    number, because json.dumps writes true, not 1."""
    try:
        return json.dumps(_normalize(v), sort_keys=True, separators=(",", ":"))
    except (TypeError, ValueError):
        return repr(v)


def _normalize(v: Any) -> Any:
    if isinstance(v, bool):
        return v
    if isinstance(v, float) and v.is_integer():
        return int(v)
    if isinstance(v, dict):
        return {k: _normalize(x) for k, x in v.items()}
    if isinstance(v, list):
        return [_normalize(x) for x in v]
    return v


def _at(path: str) -> str:
    return path if path else "top level"


def _join(path: str, key: str) -> str:
    return key if path == "" else f"{path}.{key}"
