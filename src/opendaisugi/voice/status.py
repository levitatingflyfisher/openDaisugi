"""What `daisugi voice status` reports: the engine, its model, and a fetch.

The answer is read from disk, never from a running server, so a client can
ask while `voice serve` is still fetching the model. A fetch shows as a
fresh partial file beside a pinned file: `<name>.part` (Python) or
`<name>.part-<hex>` (Go and Rust). A partial file not written for
FRESH_S seconds belongs to a fetch that stopped, and shows as no fetch.
The Go and Rust daisugi answer the same shape with kind "go" or "rust".
"""

from __future__ import annotations

import os
import time
from pathlib import Path
from typing import TYPE_CHECKING, Any

from opendaisugi.voice import engines, models, pins

if TYPE_CHECKING:
    from opendaisugi.config import Config

KIND = "python"
FRESH_S = 30.0


def _size(path: Path) -> int | None:
    return path.stat().st_size if path.is_file() else None


def _part_sizes(path: Path, now: float) -> tuple[int, bool]:
    """The largest partial file of ``path`` and whether any is fresh."""
    best, fresh = 0, False
    try:
        names = os.listdir(path.parent)
    except OSError:
        return 0, False
    for n in names:
        if n == path.name + ".part" or n.startswith(path.name + ".part-"):
            try:
                st = (path.parent / n).stat()
            except OSError:
                continue
            best = max(best, st.st_size)
            fresh = fresh or now - st.st_mtime <= FRESH_S
    return best, fresh


def _progress(
    dirpath: Path, files: list[tuple[str, int]], now: float, also: list[str] | None = None
) -> dict[str, int] | None:
    """{done, total} over the pinned ``files`` when a fetch is under way.

    ``also`` names files whose fresh partial copy counts as a fetch too (the
    file parakeet-quantize is writing).
    """
    done, total, active = 0, 0, False
    for name, size in files:
        total += size
        if _size(dirpath / name) == size:
            done += size
            continue
        part, fresh = _part_sizes(dirpath / name, now)
        done += min(part, size)
        active = active or fresh
    for name in also or []:
        active = active or _part_sizes(dirpath / name, now)[1]
    return {"done": done, "total": total} if active else None


def _path_bytes(p: Path) -> tuple[int, bool]:
    """The bytes of a model named by path: a file, or a directory's files."""
    if p.is_file():
        return p.stat().st_size, True
    if p.is_dir():
        return sum(c.stat().st_size for c in p.iterdir() if c.is_file()), True
    return 0, False


def voice_status(config: Config, *, now: float | None = None) -> dict[str, Any]:
    now = time.time() if now is None else now
    engine, model = engines.resolve_engine(config, say=lambda _line: None)
    out: dict[str, Any] = {"engine": engine, "model": model, "model_bytes": 0, "fetched": False}
    fetch: dict[str, int] | None = None
    if engine == "moonshine" and model in pins.MOONSHINE_MODELS:
        d = models.moonshine_dir(model)
        pinned = [(f.name, f.size) for f in pins.MOONSHINE_MODELS[model]]
        out["model_bytes"] = sum(s for _, s in pinned)
        out["fetched"] = all(_size(d / n) == s for n, s in pinned)
        fetch = _progress(d, pinned, now)
    elif engine == "parakeet" and model in pins.PARAKEET_MODELS:
        m = pins.PARAKEET_MODELS[model]
        d = models.parakeet_dir(model)
        out["model_bytes"] = m.quantized.size
        out["fetched"] = _size(d / m.quantized.name) == m.quantized.size
        if not out["fetched"]:
            fetch = _progress(d, [(m.source.name, m.source.size)], now, [m.quantized.name])
    elif engine == "faster-whisper":
        out["model_bytes"] = pins.FASTER_WHISPER_SIZES.get(model, 0)
    elif engine in ("moonshine", "parakeet", "whisper.cpp"):
        out["model_bytes"], out["fetched"] = _path_bytes(Path(model))
    out["kind"] = KIND
    if fetch is not None:
        out["fetch"] = fetch
    return out


def _mb(n: int) -> int:
    return (n + 500_000) // 1_000_000


def describe(st: dict[str, Any]) -> str:
    """The one line `daisugi voice status` prints without --json."""
    if "fetch" in st:
        f = st["fetch"]
        return f"Getting the {st['engine']} model, {_mb(f['done'])} of {_mb(f['total'])} MB."
    what = f"The {st['engine']} {st['model']} model"
    if st["fetched"]:
        return f"{what} is on this box ({_mb(st['model_bytes'])} MB)."
    return f"{what} is not on this box yet."
