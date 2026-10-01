#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.10"
# dependencies = []
# ///
"""Lay out a system pack: a pack that a distribution package provides with
the system's Python and its own Python packages, not a pinned CPython and
a venv.

    scripts/system-pack.py NAME DEST [--python /usr/bin/python]

DEST/NAME/ gets the worker files (from this checkout), worker/pack.json,
venv/bin/python as a link to the system's Python, and a manifest.json
whose "source" is "system". The daisugi binaries look for a pack in the
user's data dir first, then in the system packs dir (/usr/lib/opendaisugi/
packs). The AUR package daisugi-ml runs this in package(); it builds no
venv and installs nothing with pip.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
PROTOCOL = "daisugi-pack-1"
WORKER = {
    "daisugi_pack_worker.py": ROOT / "src" / "opendaisugi" / "pack" / "worker.py",
    "lora_train.py": ROOT / "src" / "opendaisugi" / "lora" / "train.py",
    "vla_oracle.py": ROOT / "src" / "opendaisugi" / "pack" / "vla_oracle.py",
}


def lay_out(name: str, dest: Path, python: str, check: list[str]) -> Path:
    d = dest / name
    (d / "worker").mkdir(parents=True, exist_ok=True)
    (d / "venv" / "bin").mkdir(parents=True, exist_ok=True)
    link = d / "venv" / "bin" / "python"
    if os.path.lexists(link):
        link.unlink()
    os.symlink(python, link)
    shas = {}
    for fname, src in WORKER.items():
        body = src.read_bytes()
        (d / "worker" / fname).write_bytes(body)
        shas[fname] = hashlib.sha256(body).hexdigest()
    info = (json.dumps({"name": name, "check": check}, indent=2) + "\n").encode()
    (d / "worker" / "pack.json").write_bytes(info)
    shas["pack.json"] = hashlib.sha256(info).hexdigest()
    manifest = {"pack": name, "protocol": PROTOCOL, "source": "system", "worker": shas}
    (d / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")
    return d


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("name")
    ap.add_argument("dest")
    ap.add_argument("--python", default="/usr/bin/python")
    ap.add_argument("--catalog", default=str(ROOT / "packs" / "catalog.json"))
    args = ap.parse_args()
    cat = json.loads(Path(args.catalog).read_text(encoding="utf-8"))
    pack = next((p for p in cat["packs"] if p["name"] == args.name), None)
    if pack is None or pack["gpu"]:
        print(f"system-pack: no CPU pack named {args.name}", file=sys.stderr)
        return 2
    d = lay_out(args.name, Path(args.dest), args.python, pack.get("check") or [])
    print(f"system-pack: {d}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
