"""Resumable, sha256-verified downloads for on-demand model files.

Generic infrastructure. The int8 matcher of ADR-0021 is the first caller.
It fetches a small quantized ONNX model and its tokenizer on first use.
Nothing here is specific to int8. A file at ``dest`` that already matches
the expected hash short-circuits to zero network calls. A hash mismatch
after a full download deletes the file and raises, so a caller never sees
a corrupt or truncated file at ``dest``, and the next call re-fetches clean.
"""

from __future__ import annotations

import hashlib
import http.client
import logging
import urllib.error
import urllib.request
from pathlib import Path

_CHUNK = 1 << 20  # 1 MiB
_log = logging.getLogger("opendaisugi._model_fetch")


class FetchVerificationError(OSError):
    """The sha256 of a downloaded file did not match after the fetch.

    The file and any partial ``.part`` file are deleted before this is
    raised. The next ``fetch()`` call starts clean instead of reusing a
    corrupt file.
    """


def sha256_file(path: Path) -> str:
    """Streaming SHA-256 of a file, hex-encoded."""
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(_CHUNK), b""):
            h.update(chunk)
    return h.hexdigest()


def fetch(url: str, sha256: str, dest: Path, *, size: int | None = None) -> Path:
    """Return ``dest``. Download it first when it is absent or hash-mismatched.

    Resumes a partial download at ``<dest>.part`` with an HTTP Range request
    when one exists. A transfer that stops short of ``size`` keeps the
    ``.part`` and raises a plain ``OSError``; the next call resumes it.
    Verifies the sha256 of a complete file before it renames the file into
    place. Logs one notice that names the file and its size before the first
    network call this process makes for it. The notice never fires at
    import, only when a caller needs the file.
    """
    dest = Path(dest)
    dest.parent.mkdir(parents=True, exist_ok=True)
    if dest.exists() and sha256_file(dest) == sha256:
        return dest
    if dest.exists():
        dest.unlink()  # stale or corrupt: start clean, never reuse it

    part = dest.with_name(dest.name + ".part")
    resume_from = part.stat().st_size if part.exists() else 0
    size_note = f" of about {size // 1024 // 1024}MB" if size else ""
    _log.warning("fetching %s%s from %s ...", dest.name, size_note, url)

    req = urllib.request.Request(url)
    if resume_from:
        req.add_header("Range", f"bytes={resume_from}-")
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            mode = "ab" if resume_from and resp.status == 206 else "wb"
            if mode == "wb":
                resume_from = 0
            with open(part, mode) as f:
                try:
                    while chunk := resp.read(_CHUNK):
                        f.write(chunk)
                except http.client.IncompleteRead as exc:
                    f.write(exc.partial)  # keep every byte that arrived
                    raise
    except (urllib.error.URLError, http.client.HTTPException, TimeoutError) as exc:
        # One exception type for every network failure. Bytes already in
        # the .part stay for a resume. This module has no caller-specific
        # env var to name. A caller that has one catches this OSError and
        # adds that guidance itself.
        raise OSError(f"could not download {url}: {exc}") from exc

    got = part.stat().st_size
    if size is not None and got < size:
        raise OSError(f"download of {url} stopped at {got} of {size} bytes. The next call resumes.")
    actual = sha256_file(part)
    if actual != sha256:
        part.unlink(missing_ok=True)
        raise FetchVerificationError(
            f"{url} downloaded but its sha256 did not match. "
            f"Expected {sha256}, got {actual}. Deleted the file; the next call re-fetches."
        )
    part.rename(dest)
    return dest
