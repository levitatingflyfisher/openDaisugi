"""Record a sha256 pin over the vendored Herdr manifests and their schema doc.

opendaisugi.floor.manifest_schema records the Herdr manifest TOML schema as
Python constants, read by hand off harness/coppice/internal/detect/README.md
and its Go implementation. That reading does not re-derive itself from the
files on every run. This script is the drift check: it hashes every
vendored manifest and the Go README, and --check compares those hashes
against the last recorded pin. A hash mismatch means the Go side changed
and a human needs to re-read the schema and update manifest_schema.py by
hand; this script does not rewrite the constants for you.

Usage:
    uv run --no-sync python scripts/floor_manifest_schema.py           # record the pin
    uv run --no-sync python scripts/floor_manifest_schema.py --check   # exit 1 if the pin is stale
"""

from __future__ import annotations

import argparse
import hashlib
import json
import sys
from datetime import UTC, datetime
from pathlib import Path

from opendaisugi.floor.manifest_schema import DETECT_README, MANIFEST_DIR

PIN = (
    Path(__file__).resolve().parents[1]
    / "src"
    / "opendaisugi"
    / "floor"
    / "manifest_sources_pin.json"
)

# recorded_at is excluded from the --check comparison: it moves every day
# and says nothing about drift.
_COMPARED_KEYS = ("readme_sha256", "manifest_sha256")


def _sha256(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def compute_pin(manifest_dir: Path, readme_path: Path) -> dict:
    """Hash every manifest under manifest_dir and the README at readme_path."""
    manifests = {p.name: _sha256(p) for p in sorted(manifest_dir.glob("*.toml"))}
    return {
        "recorded_at": datetime.now(UTC).date().isoformat(),
        "readme_sha256": _sha256(readme_path),
        "manifest_sha256": manifests,
    }


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true", help="Compare, do not write.")
    args = parser.parse_args(argv)

    if not MANIFEST_DIR.is_dir():
        print(f"no manifests under {MANIFEST_DIR}", file=sys.stderr)
        print("run plan 02 first, it vendors herdr's manifests into the tree", file=sys.stderr)
        return 1
    if not DETECT_README.is_file():
        print(f"{DETECT_README} is missing", file=sys.stderr)
        return 1

    pin = compute_pin(MANIFEST_DIR, DETECT_README)
    if args.check:
        try:
            current = json.loads(PIN.read_text(encoding="utf-8"))
        except (OSError, ValueError):
            current = {}
        stale = any(current.get(k) != pin.get(k) for k in _COMPARED_KEYS)
        if stale:
            print(f"{PIN} is stale.", file=sys.stderr)
            print("run: uv run --no-sync python scripts/floor_manifest_schema.py", file=sys.stderr)
            return 1
        return 0

    PIN.write_text(json.dumps(pin, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    print(f"recorded {len(pin['manifest_sha256'])} manifests and the detect README to {PIN}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
