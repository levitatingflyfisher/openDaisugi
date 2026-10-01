"""The pinned Moonshine and Parakeet models: where they live and how they arrive.

A curated model (tiny, small or medium streaming English) lives in the user
cache, $XDG_CACHE_HOME/opendaisugi/models/moonshine/<name>-streaming-en/<rev>
when XDG_CACHE_HOME is an absolute path, else under ~/.cache. Never /tmp and
never ~/.opendaisugi. ensure_moonshine fetches the files that are missing or
fail their sha256 through _model_fetch, and says so in one line with the
model's size. The Go and Rust daisugi do the same, with the same line.

The Parakeet model lives in $XDG_CACHE_HOME/opendaisugi/models/parakeet/
<dir> the same way. ensure_parakeet fetches the pinned F16 file, makes the
Q4_K file from it with parakeet-quantize, checks that file against its own
pin, and deletes the F16 file (ruling VO-16).
"""

from __future__ import annotations

import os
import subprocess
from collections.abc import Callable
from pathlib import Path

from opendaisugi._model_fetch import fetch, sha256_file
from opendaisugi.voice import pins


def cache_root() -> Path:
    base = os.environ.get("XDG_CACHE_HOME", "")
    root = Path(base) if os.path.isabs(base) else Path.home() / ".cache"
    return root / "opendaisugi" / "models"


def moonshine_dir(name: str) -> Path:
    return cache_root() / "moonshine" / f"{name}-streaming-en" / pins.MOONSHINE_REVISION


def moonshine_url(name: str, file: str) -> str:
    base = os.environ.get(pins.MOONSHINE_BASE_URL_ENV) or pins.MOONSHINE_BASE_URL
    return f"{base}/{name}-streaming-en/{pins.MOONSHINE_REVISION}/{file}"


def moonshine_size_mb(name: str) -> int:
    """The model's bytes in megabytes (10**6), rounded half up."""
    total = sum(f.size for f in pins.MOONSHINE_MODELS[name])
    return (total + 500_000) // 1_000_000


def _matches(path: Path, sha256: str) -> bool:
    try:
        return path.is_file() and sha256_file(path) == sha256
    except OSError:
        return False


def ensure_moonshine(name: str, say: Callable[[str], None]) -> Path:
    """The directory of a curated model, every file in it checked.

    Files that are missing or fail their pin are fetched, after one line
    through ``say``. Raises _model_fetch.FetchVerificationError when a
    download does not match its pin, and OSError when one fails.
    """
    d = moonshine_dir(name)
    stale = [f for f in pins.MOONSHINE_MODELS[name] if not _matches(d / f.name, f.sha256)]
    if not stale:
        return d
    say(
        f"Fetching the Moonshine {name} model ({moonshine_size_mb(name)} MB) into {d}. "
        "This happens once."
    )
    for f in stale:
        fetch(moonshine_url(name, f.name), f.sha256, d / f.name, size=f.size, notice=False)
    return d


class QuantizeError(RuntimeError):
    """parakeet-quantize is missing, failed, or made a file that fails its pin."""


QUANTIZE_TIMEOUT_S = 1800.0


def parakeet_dir(name: str) -> Path:
    return cache_root() / "parakeet" / pins.PARAKEET_MODELS[name].dir


def parakeet_url(file: str) -> str:
    base = os.environ.get(pins.PARAKEET_BASE_URL_ENV) or pins.PARAKEET_BASE_URL
    return f"{base}/{file}"


def _mb(size: int) -> int:
    return (size + 500_000) // 1_000_000


def ensure_parakeet(name: str, quantizer: Path, say: Callable[[str], None]) -> Path:
    """The Q4_K file of a curated Parakeet model, checked against its pin.

    When it is missing or fails its pin: one line through ``say``, the F16
    file fetched (FetchVerificationError or OSError as for Moonshine), the
    Q4_K file made by ``quantizer`` and checked (QuantizeError), and the
    F16 file deleted.
    """
    m = pins.PARAKEET_MODELS[name]
    d = parakeet_dir(name)
    out = d / m.quantized.name
    if _matches(out, m.quantized.sha256):
        return out
    say(
        f"Fetching the Parakeet {name} model ({_mb(m.source.size)} MB) into {d}, then making "
        f"its {_mb(m.quantized.size)} MB Q4_K file. This happens once."
    )
    src = d / m.source.name
    fetch(parakeet_url(m.source.name), m.source.sha256, src, size=m.source.size, notice=False)
    if not (quantizer.is_file() and os.access(quantizer, os.X_OK)):
        raise QuantizeError(
            f"{pins.PARAKEET_QUANTIZE_BINARY} is not beside {pins.PARAKEET_BINARY}. Build both "
            "with clients/go/scripts/native.sh --parakeet, then try again."
        )
    part = out.with_name(out.name + ".part")
    try:
        proc = subprocess.run(
            [str(quantizer), str(src), str(part), pins.PARAKEET_QUANT],
            stdin=subprocess.DEVNULL,
            capture_output=True,
            timeout=QUANTIZE_TIMEOUT_S,
        )
    except subprocess.TimeoutExpired as exc:
        part.unlink(missing_ok=True)
        raise QuantizeError(
            f"{pins.PARAKEET_QUANTIZE_BINARY} did not finish in {QUANTIZE_TIMEOUT_S:g} seconds."
        ) from exc
    if proc.returncode != 0:
        part.unlink(missing_ok=True)
        lines = proc.stderr.decode("utf-8", errors="replace").strip().splitlines()
        last = lines[-1].strip()[:200] if lines else ""
        raise QuantizeError(f"{pins.PARAKEET_QUANTIZE_BINARY} exited {proc.returncode}: {last}")
    if not _matches(part, m.quantized.sha256):
        part.unlink(missing_ok=True)
        raise QuantizeError(
            f"The Q4_K file {pins.PARAKEET_QUANTIZE_BINARY} made did not match its pinned sha256 "
            "and was deleted. Set voice_model to the path of a Parakeet GGUF file instead."
        )
    part.rename(out)
    src.unlink(missing_ok=True)
    return out
