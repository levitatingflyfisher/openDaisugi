"""Agent profiles: the operator's table of where a child agent runs.

A profile names a wire (``claude-code``, ``anthropic`` or ``openai``), an
optional base URL, an optional default model and an optional ``key_env``:
the name of the variable that holds the key. The table lives in
``config.yaml`` under ``agent_profiles``; it is the operator's, never a
model's. A plan step or a parent agent names a profile, never a URL or a
key, so a parent cannot send a key or a workspace anywhere the operator
did not list.

The root starter reads each ``key_env`` once (:func:`resolve`). From then
on a key moves only through an inherited pipe: the child's own key on
descriptor 3, and for a child that may start children, the resolved table
with its keys (the bundle) on descriptor 4. No process below the root
reads the config or a key variable.

The built-in profile ``default`` is the run's own runtime as before:
``claude -p`` or sprig, picked by ``--agent``, with no key and no URL.
No profile is limited by a model's origin; the model id is free text.
"""

from __future__ import annotations

import json
import re
from collections.abc import Mapping
from dataclasses import dataclass
from typing import Any

DEFAULT = "default"
WIRES = ("claude-code", "anthropic", "openai")
FIELDS = ("wire", "base_url", "model", "key_env")
#: The server a wire talks to when the profile names none.
DEFAULT_BASE = {"anthropic": "https://api.anthropic.com", "openai": "https://api.openai.com/v1"}
#: The variables that can hold a provider key or point a key elsewhere. A
#: profiled sprig child gets none of them: the profile alone decides.
AMBIENT = (
    "ANTHROPIC_API_KEY",
    "ANTHROPIC_AUTH_TOKEN",
    "OPENAI_API_KEY",
    "SPRIG_API_KEY",
    "ANTHROPIC_BASE_URL",
    "SPRIG_BACKEND",
    "SPRIG_MODEL",
)

_NAME = re.compile(r"[A-Za-z0-9._-]{1,64}")
_VAR = re.compile(r"[A-Za-z_][A-Za-z0-9_]*")


def is_key_name(name: str) -> bool:
    """A variable name that may hold a credential: ``*_API_KEY`` or
    ``*_TOKEN``. No child of the executor gets one (MX-R-3)."""
    return name.endswith("_API_KEY") or name.endswith("_TOKEN")


def valid_name(name: str) -> bool:
    return bool(_NAME.fullmatch(name))


@dataclass(frozen=True)
class Profile:
    name: str
    wire: str
    base_url: str | None = None
    model: str | None = None
    key_env: str | None = None
    #: The key read from key_env, or None (no key_env, or it is not set).
    key: str | None = None

    def entry(self) -> dict[str, Any]:
        """The profile as the bundle carries it."""
        return {
            "wire": self.wire,
            "base_url": self.base_url,
            "model": self.model,
            "key_env": self.key_env,
            "key": self.key,
        }


BUILTIN = Profile(DEFAULT, "claude-code")


def _check(name: str, doc: Any) -> str | None:
    """Why the table entry ``name`` is not a profile, or None."""
    if name == DEFAULT:
        return "default is built in"
    if not valid_name(name):
        return "its name is not 1 to 64 of A-Z a-z 0-9 . _ -"
    if not isinstance(doc, dict):
        return "it is not a mapping"
    extra = sorted(str(k) for k in doc if k not in FIELDS)
    if extra:
        return f"it has an unknown field {extra[0]}"
    if doc.get("wire") not in WIRES:
        return "wire must be claude-code, anthropic or openai"
    for f in ("base_url", "model", "key_env"):
        if doc.get(f) is not None and not isinstance(doc[f], str):
            return f"{f} must be text"
    if doc.get("key_env") is not None and not _VAR.fullmatch(doc["key_env"]):
        return "key_env must be a variable name"
    if doc["wire"] == "claude-code":
        if doc.get("base_url") is not None:
            return "the claude-code wire takes no base_url"
        if doc.get("key_env") is not None:
            return "the claude-code wire takes no key_env"
    return None


@dataclass
class Profiles:
    """The resolved table: the valid profiles with their keys read, and
    why each other string-named entry is not a profile."""

    valid: dict[str, Profile]
    invalid: dict[str, str]

    def names(self) -> list[str]:
        return sorted([DEFAULT, *self.valid])

    def key_names(self) -> list[str]:
        """Every key_env the table names, sorted: no child gets them."""
        return sorted({p.key_env for p in self.valid.values() if p.key_env})

    def lookup(self, name: str | None) -> tuple[Profile | None, str | None]:
        """The profile a step names (None is the built-in), or why not."""
        if name is None or name == DEFAULT:
            return BUILTIN, None
        if name in self.invalid:
            return None, f"agent profile '{name}' is not valid: {self.invalid[name]}"
        p = self.valid.get(name)
        if p is None:
            return None, (
                f"no agent profile named '{name}'; the profiles are {', '.join(self.names())}"
            )
        if p.key_env and p.key is None:
            return None, f"agent profile '{name}' names {p.key_env}, which is not set"
        return p, None

    def bundle(self) -> bytes:
        """The table with its keys, for a child that may start children."""
        doc = {DEFAULT: BUILTIN.entry(), **{n: p.entry() for n, p in self.valid.items()}}
        return json.dumps(doc, sort_keys=True, separators=(",", ":")).encode("utf-8")


def operator_bundle(profiles: Profiles) -> tuple[bytes, list[str]]:
    """The bundle an operator-side root starter hands to ``tree run``, and
    one line for each profile left out of it. An invalid profile, and a
    profile whose key_env is not set, are left out: a child that asked for
    one would only find it without a key. The lines name the profile and
    the variable, never a key."""
    notes: list[str] = []
    kept: dict[str, Profile] = {}
    for name in sorted({*profiles.valid, *profiles.invalid}):
        if name in profiles.invalid:
            notes.append(f"agent profile '{name}' is left out: {profiles.invalid[name]}")
            continue
        prof = profiles.valid[name]
        if prof.key_env and prof.key is None:
            notes.append(f"agent profile '{name}' is left out: {prof.key_env} is not set")
            continue
        kept[name] = prof
    doc = {DEFAULT: BUILTIN.entry(), **{n: q.entry() for n, q in kept.items()}}
    return json.dumps(doc, sort_keys=True, separators=(",", ":")).encode("utf-8"), notes


def list_rows(profiles: Profiles) -> list[dict[str, Any]]:
    """One row for each valid profile, by name, for ``agent-profiles list``:
    no key and no key variable name. ``key_set`` is False only when the
    profile names a key variable that is not set, so a profile that needs
    no key reads as set."""
    return [
        {
            "name": n,
            "wire": p.wire,
            "base_url": p.base_url,
            "model": p.model,
            "key_set": p.key_env is None or p.key is not None,
        }
        for n, p in sorted(profiles.valid.items())
    ]


def resolve(raw: Any, environ: Mapping[str, str]) -> Profiles:
    """Read the table ``raw`` (config.yaml's ``agent_profiles``) and each
    key_env from ``environ``, once. An entry with a name that is not text
    is left out. An empty key counts as not set."""
    valid: dict[str, Profile] = {}
    invalid: dict[str, str] = {}
    if isinstance(raw, dict):
        for name, doc in raw.items():
            if not isinstance(name, str):
                continue
            why = _check(name, doc)
            if why is not None:
                invalid[name] = why
                continue
            key_env = doc.get("key_env")
            key = environ.get(key_env) if key_env else None
            valid[name] = Profile(
                name,
                doc["wire"],
                doc.get("base_url"),
                doc.get("model"),
                key_env,
                key or None,
            )
    return Profiles(valid, invalid)


def from_bundle(data: bytes) -> Profiles:
    """The table a starter got on its keys descriptor. A bundle that does
    not read is an empty table: only the built-in profile is there."""
    try:
        doc = json.loads(data.decode("utf-8"))
    except (UnicodeDecodeError, ValueError, RecursionError):
        doc = None
    valid: dict[str, Profile] = {}
    if isinstance(doc, dict):
        for name, e in doc.items():
            if name == DEFAULT or not isinstance(e, dict) or not valid_name(name):
                continue
            if e.get("wire") not in WIRES:
                continue
            vals = {f: e.get(f) if isinstance(e.get(f), str) else None for f in (*FIELDS, "key")}
            valid[name] = Profile(
                name, e["wire"], vals["base_url"], vals["model"], vals["key_env"], vals["key"]
            )
    return Profiles(valid, {})


def configured_table() -> Any:
    """config.yaml's ``agent_profiles`` as written, or None. A config that
    does not read has no table: the run keeps the built-in profile."""
    from opendaisugi.config import _read_raw
    from opendaisugi.datahome import data_home

    try:
        return _read_raw(data_home() / "config.yaml").get("agent_profiles")
    except (OSError, ValueError):
        return None


def config_text(raw: Any) -> str:
    """The table as ``daisugi config`` shows it: the profile names, never
    a field, so no key name or URL is on that surface."""
    if raw is None or raw == {}:
        return "none"
    if not isinstance(raw, dict):
        return "not a mapping"
    return ", ".join(sorted(str(k) for k in raw))
