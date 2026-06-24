"""Canonical step metadata vocabulary (v0.9.0).

Parses docs/step-vocabulary.md into a structured CanonicalKeys map at
import time. Provides assert_step_matches_vocabulary() as a lazy check:
unknown keys warn but don't raise, unknown step types are accepted.
"""

from __future__ import annotations

import re
import warnings
from pathlib import Path

CanonicalKeys = dict[str, set[str]]

_DOC_PATH = Path(__file__).resolve().parent.parent.parent / "docs" / "step-vocabulary.md"


def load_canonical_keys(path: Path | None = None) -> CanonicalKeys:
    """Parse docs/step-vocabulary.md into {step_type: {canonical_key, ...}}."""
    doc = path or _DOC_PATH
    if not doc.exists():
        return {}
    text = doc.read_text(encoding="utf-8")
    result: CanonicalKeys = {}
    current: str | None = None
    header_re = re.compile(r"^##\s*`([a-zA-Z_][a-zA-Z0-9_]*)`")
    key_re = re.compile(r"^\s*-\s*`([a-zA-Z_][a-zA-Z0-9_]*)\s*:")
    for line in text.splitlines():
        m = header_re.match(line)
        if m:
            current = m.group(1)
            result.setdefault(current, set())
            continue
        if current is None:
            continue
        k = key_re.match(line)
        if k:
            result[current].add(k.group(1))
    return result


_CANONICAL: CanonicalKeys | None = None


def _cached() -> CanonicalKeys:
    global _CANONICAL
    if _CANONICAL is None:
        _CANONICAL = load_canonical_keys()
    return _CANONICAL


def assert_step_matches_vocabulary(step: dict) -> None:
    """Warn (not raise) if a step's metadata carries non-canonical keys."""
    keys = _cached()
    step_type = step.get("type")
    if step_type not in keys:
        return
    canonical = keys[step_type]
    metadata = step.get("metadata") or {}
    extras = [k for k in metadata if k not in canonical]
    if extras:
        warnings.warn(
            f"step type '{step_type}' has non-canonical metadata keys: {extras}. "
            f"Expected vocabulary: {sorted(canonical)}.",
            UserWarning,
            stacklevel=2,
        )


__all__ = ["CanonicalKeys", "assert_step_matches_vocabulary", "load_canonical_keys"]
