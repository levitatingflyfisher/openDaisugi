"""Which hosts this box has, why a floor test skips, and what to run to fix it.

Every live floor test asks here instead of calling shutil.which itself. A
skipped run reports one consistent, teaching reason. Read-only: this module
starts nothing and installs nothing.
"""

from __future__ import annotations

import shutil
from dataclasses import dataclass
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]
MANIFEST_DIR = REPO_ROOT / "harness" / "coppice" / "internal" / "detect" / "manifests"
SCREEN_FIXTURE_DIR = REPO_ROOT / "harness" / "coppice" / "testdata" / "screens"


@dataclass(frozen=True)
class HostFact:
    """One host requirement: is it here, why not, and the command that fixes it."""

    name: str
    present: bool
    why_not: str
    fix: str


def host_facts() -> dict[str, HostFact]:
    """Probe the box once. Never raises, never starts a server."""
    return {
        "tmux": HostFact(
            "tmux",
            shutil.which("tmux") is not None,
            "tmux is not on PATH",
            "install tmux 3.2 or newer with your package manager",
        ),
        "herdr": HostFact(
            "herdr",
            shutil.which("herdr") is not None,
            "herdr is not on PATH",
            "install herdr from herdr.dev, then run herdr session list --json",
        ),
        "coppice": HostFact(
            "coppice",
            shutil.which("coppice") is not None,
            "coppice is not on PATH but builds into harness/coppice/build/coppice",
            "build it: cd harness/coppice && mkdir -p build && go build -o build/coppice ./cmd/coppice",
        ),
        "vendored_manifests": HostFact(
            "vendored_manifests",
            MANIFEST_DIR.is_dir(),
            "harness/coppice/internal/detect/manifests is not in the tree",
            "run plan 02 first, it vendors the manifests into the tree",
        ),
        "screen_fixtures": HostFact(
            "screen_fixtures",
            SCREEN_FIXTURE_DIR.is_dir(),
            "harness/coppice/testdata/screens is not in the tree",
            "run plan 02 first, it records the shared screen fixtures",
        ),
    }


def skip_reason(name: str) -> str | None:
    """None when the host is here. Otherwise one line that teaches the fix.

    Two sentences, no em-dash. This string lands in pytest's skip column,
    where an operator reads it.
    """
    facts = host_facts()
    fact = facts.get(name)
    if fact is None:
        return f"unknown host fact {name!r}. known facts: {', '.join(sorted(facts))}."
    if fact.present:
        return None
    return f"{fact.why_not}. {fact.fix}."
