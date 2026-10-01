"""The pack catalog: the pinned Python and the packs, each with its lock
file of hashed requirements.

The catalog is packs/catalog.json in the repository, beside the lock
files it names. The binaries carry copies. OPENDAISUGI_PACK_CATALOG, read
by the command line only, names another catalog.json (a mirror, or the
fake catalog of the golden cases); its lock paths are relative to it.
"""

from __future__ import annotations

import json
import re
from dataclasses import dataclass
from pathlib import Path

CATALOG_ENV = "OPENDAISUGI_PACK_CATALOG"
_HERE = Path(__file__).resolve().parent
_REPO_PACKS = _HERE.parent.parent.parent / "packs"
_WHEEL_PACKS = _HERE / "packs"


def default_dir() -> Path:
    """packs/ in a checkout, else the copy a wheel install carries."""
    return _REPO_PACKS if (_REPO_PACKS / "catalog.json").is_file() else _WHEEL_PACKS


class LockError(ValueError):
    """A lock line that does not pin one version with sha256 hashes."""


@dataclass(frozen=True)
class Req:
    name: str  # normalized
    version: str
    hashes: tuple[str, ...]


def load(path: Path | None = None) -> dict:
    """The catalog, with "_dir" set to the directory its locks are in."""
    p = Path(path) if path else default_dir() / "catalog.json"
    cat = json.loads(p.read_text(encoding="utf-8"))
    cat["_dir"] = str(p.parent)
    return cat


def find(cat: dict, name: str) -> dict | None:
    for p in cat["packs"]:
        if p["name"] == name:
            return p
    return None


def lock_text(cat: dict, pack: dict) -> str:
    return (Path(cat["_dir"]) / pack["lock"]).read_text(encoding="utf-8")


def normalize(name: str) -> str:
    """The PEP 503 form of a project name."""
    return re.sub(r"[-_.]+", "-", name).lower()


_HASH = re.compile(r"^--hash=sha256:([0-9a-f]{64})$")


def parse_lock(text: str) -> list[Req]:
    """The requirements of a hashed lock: `name==version` and one or more
    `--hash=sha256:HEX`, with backslash continuations and # comments."""
    logical: list[str] = []
    cur = ""
    for raw in text.splitlines():
        line = raw.strip()
        if line.startswith("#"):
            continue
        line = line.split(" #", 1)[0].rstrip()
        if line.endswith("\\"):
            cur += line[:-1] + " "
            continue
        cur += line
        if cur.strip():
            logical.append(cur.strip())
        cur = ""
    if cur.strip():
        logical.append(cur.strip())
    out = []
    for ln in logical:
        words = ln.split()
        head, rest = words[0], words[1:]
        if "==" not in head or ";" in ln:
            raise LockError(f"not one pinned version: {ln[:80]}")
        name, version = head.split("==", 1)
        hashes = []
        for w in rest:
            m = _HASH.match(w)
            if not m:
                raise LockError(f"not a sha256 hash: {w[:80]}")
            hashes.append(m.group(1))
        if not hashes or not name or not version:
            raise LockError(f"no sha256 hash: {ln[:80]}")
        out.append(Req(normalize(name), version, tuple(hashes)))
    return out


def wheel_key(filename: str) -> tuple[str, str] | None:
    """(normalized name, version) of a wheel file name, else None."""
    if not filename.endswith(".whl"):
        return None
    parts = filename[: -len(".whl")].split("-")
    if len(parts) not in (5, 6):
        return None
    return normalize(parts[0]), parts[1]
