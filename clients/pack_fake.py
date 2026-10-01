"""The fake assets of the pack golden cases: a fake CPython tarball, two
tiny wheels, their hashed lock and a catalog that names them.

    uv run --no-sync python clients/pack_fake.py   # writes clients/fixtures/pack/assets

The fake CPython's python/bin/python3 is a shell script that runs the
system's /usr/bin/python3, so `-m venv` and pip are real, and the pack
needs nothing else. The tarball holds the entry kinds of the real one
(files, an executable, symlinks, and a name of more than 100 bytes
stored in the ustar prefix field). The files are committed: zlib output
differs between builds, and the hashes in the lock must not move.
"""

from __future__ import annotations

import base64
import gzip
import hashlib
import io
import json
import sys
import tarfile
import zipfile
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
ASSETS = REPO / "clients" / "fixtures" / "pack" / "assets"
PY_FILE = "cpython-fake-x86_64-linux.tar.gz"
PY_VERSION = "3.12.99"
LONG_PROJECT = "fake_long_" + "n" * 80
LONG_WHEEL = LONG_PROJECT + "-1.0-py3-none-any.whl"
LONG = "python/share/fake/" + "d" * 60 + "/" + "f" * 40 + ".txt"


def _sha(b: bytes) -> str:
    return hashlib.sha256(b).hexdigest()


def cpython_tarball() -> bytes:
    raw = io.BytesIO()
    with tarfile.open(fileobj=raw, mode="w", format=tarfile.USTAR_FORMAT) as t:

        def add(name: str, data: bytes = b"", mode: int = 0o644, link: str | None = None):
            info = tarfile.TarInfo(name)
            info.mtime = 0
            info.mode = mode
            if link is not None:
                info.type = tarfile.SYMTYPE
                info.linkname = link
                t.addfile(info)
            else:
                info.size = len(data)
                t.addfile(info, io.BytesIO(data))

        add(
            "python/bin/python3",
            b"#!/bin/sh\n# The fake CPython of the pack golden cases: the system's Python.\n"
            b'exec /usr/bin/python3 "$@"\n',
            0o755,
        )
        add("python/bin/python", link="python3")
        add("python/bin/python3.12", link="python3")
        add("python/lib/python3.12/README.txt", b"The fake standard library.\n")
        add(LONG, b"A name of more than 100 bytes.\n")
    out = io.BytesIO()
    with gzip.GzipFile(fileobj=out, mode="wb", mtime=0) as gz:
        gz.write(raw.getvalue())
    return out.getvalue()


def _record_hash(b: bytes) -> str:
    return "sha256=" + base64.urlsafe_b64encode(hashlib.sha256(b).digest()).rstrip(b"=").decode()


def wheel(name: str, version: str, init: str, requires: list[str]) -> bytes:
    info = f"{name}-{version}.dist-info"
    files = {
        f"{name}/__init__.py": init.encode(),
        f"{info}/METADATA": (
            f"Metadata-Version: 2.1\nName: {name}\nVersion: {version}\n"
            + "".join(f"Requires-Dist: {r}\n" for r in requires)
        ).encode(),
        f"{info}/WHEEL": b"Wheel-Version: 1.0\nGenerator: pack_fake\nRoot-Is-Purelib: true\n"
        b"Tag: py3-none-any\n",
    }
    record = "".join(f"{p},{_record_hash(b)},{len(b)}\n" for p, b in files.items())
    record += f"{info}/RECORD,,\n"
    files[f"{info}/RECORD"] = record.encode()
    out = io.BytesIO()
    with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED) as z:
        for p, b in files.items():
            zi = zipfile.ZipInfo(p, date_time=(1980, 1, 1, 0, 0, 0))
            zi.external_attr = 0o644 << 16
            z.writestr(zi, b)
    return out.getvalue()


WHEELS = {
    "fakepkg-1.0-py3-none-any.whl": (
        "fakepkg",
        "1.0",
        'import fakedep  # noqa: F401\n\n__version__ = "1.0"\n',
        ["fakedep==2.0"],
    ),
    "fakedep-2.0-py3-none-any.whl": ("fakedep", "2.0", '__version__ = "2.0"\n', []),
    # A file name longer than the ustar name field, as some real wheels
    # have: the bundle tar stores it in an extended header.
    LONG_WHEEL: (LONG_PROJECT, "1.0", "", []),
}


def catalog(py_sha: str, py_size: int) -> dict:
    return {
        "v": 1,
        "protocol": "daisugi-pack-1",
        "platform": "x86_64-linux",
        "python": {
            "version": PY_VERSION,
            "file": PY_FILE,
            "url": "http://127.0.0.1:{PORT}/python/" + PY_FILE,
            "sha256": py_sha,
            "size": py_size,
        },
        "packs": [
            {
                "name": "train",
                "gpu": False,
                "summary": "The pack of the golden cases: two tiny wheels.",
                "lock": "fake.lock",
                "index_url": "http://127.0.0.1:{PORT}/simple",
                "extra_index_urls": [],
                "check": ["fakepkg"],
            },
            {"name": "train-cuda", "gpu": True, "summary": "A GPU pack, listed only."},
        ],
    }


def main() -> int:
    (ASSETS / "wheels").mkdir(parents=True, exist_ok=True)
    tb = cpython_tarball()
    (ASSETS / PY_FILE).write_bytes(tb)
    lock = ["# The fake lock of the pack golden cases (clients/pack_fake.py)."]
    for fname, (name, version, init, req) in sorted(WHEELS.items()):
        w = wheel(name, version, init, req)
        (ASSETS / "wheels" / fname).write_bytes(w)
        lock.append(f"{name}=={version} \\\n    --hash=sha256:{_sha(w)}")
    (ASSETS / "fake.lock").write_text("\n".join(lock) + "\n", encoding="utf-8")
    (ASSETS / "catalog.json").write_text(
        json.dumps(catalog(_sha(tb), len(tb)), indent=2) + "\n", encoding="utf-8"
    )
    print(f"pack_fake: {ASSETS} ({len(tb)} byte tarball, {len(WHEELS)} wheels)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
