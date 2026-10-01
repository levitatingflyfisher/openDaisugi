"""voice status: the engine, the model on disk, and a fetch under way."""

from __future__ import annotations

import os
import time
from pathlib import Path

import pytest

from opendaisugi.config import Config
from opendaisugi.voice import models, pins, status


@pytest.fixture(autouse=True)
def cache(tmp_path, monkeypatch):
    monkeypatch.setenv("XDG_CACHE_HOME", str(tmp_path / "cache"))
    monkeypatch.setenv("OPENDAISUGI_VOICE_HARDWARE", "16,8,0")


def _sparse(path: Path, size: int, age_s: float = 0.0) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with open(path, "wb") as f:
        f.truncate(size)
    then = time.time() - age_s
    os.utime(path, (then, then))


def _cfg(engine="moonshine", model="tiny") -> Config:
    return Config(voice_engine=engine, voice_model=model)


def test_nothing_on_disk():
    st = status.voice_status(_cfg())
    assert st["engine"] == "moonshine"
    assert st["fetched"] is False
    assert st["kind"] == "python"
    assert "fetch" not in st
    assert st["model_bytes"] == sum(f.size for f in pins.MOONSHINE_MODELS["tiny"])


def test_fetched_when_every_pinned_size_is_there():
    d = models.moonshine_dir("tiny")
    for f in pins.MOONSHINE_MODELS["tiny"]:
        _sparse(d / f.name, f.size)
    assert status.voice_status(_cfg())["fetched"] is True


def test_fresh_part_file_is_a_fetch_with_progress():
    d = models.moonshine_dir("tiny")
    big = max(pins.MOONSHINE_MODELS["tiny"], key=lambda f: f.size)
    _sparse(d / (big.name + ".part"), 1000)
    st = status.voice_status(_cfg())
    assert st["fetch"] == {"done": 1000, "total": st["model_bytes"]}


def test_go_style_part_name_counts():
    d = models.moonshine_dir("tiny")
    _sparse(d / "encoder.ort.part-ab12", 500)
    assert status.voice_status(_cfg())["fetch"]["done"] == 500


def test_stale_part_file_is_no_fetch():
    d = models.moonshine_dir("tiny")
    _sparse(d / "encoder.ort.part", 500, age_s=3600)
    assert "fetch" not in status.voice_status(_cfg())


def test_parakeet_fetch_counts_the_source_file():
    m = pins.PARAKEET_MODELS["v2"]
    _sparse(models.parakeet_dir("v2") / (m.source.name + ".part"), 4096)
    st = status.voice_status(_cfg("parakeet", "v2"))
    assert st["fetch"] == {"done": 4096, "total": m.source.size}
    assert st["model_bytes"] == m.quantized.size


def test_a_model_named_by_path(tmp_path):
    p = tmp_path / "ggml.bin"
    p.write_bytes(b"abc")
    st = status.voice_status(_cfg("whisper.cpp", str(p)))
    assert (st["model_bytes"], st["fetched"]) == (3, True)


def test_describe_lines():
    assert status.describe({"engine": "moonshine", "model": "tiny", "fetched": False}).endswith(
        "not on this box yet."
    )
    line = status.describe(
        {"engine": "moonshine", "fetch": {"done": 41_000_000, "total": 160_000_000}}
    )
    assert line == "Getting the moonshine model, 41 of 160 MB."
