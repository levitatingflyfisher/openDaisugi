"""The knn golden file is current. The Go and Rust tests read the same file."""

from __future__ import annotations

import importlib.util
import json
from pathlib import Path

_PATH = Path(__file__).resolve().parent.parent / "clients" / "knn_golden.py"
_spec = importlib.util.spec_from_file_location("knn_golden", _PATH)
assert _spec and _spec.loader
knn_golden = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(knn_golden)


def test_the_golden_file_matches_the_python_module() -> None:
    on_disk = json.loads(knn_golden.PATH.read_text())
    assert on_disk == json.loads(json.dumps(knn_golden.build())), (
        "run: uv run --no-sync python clients/knn_golden.py --write"
    )


def test_the_golden_file_covers_every_branch() -> None:
    data = json.loads(knn_golden.PATH.read_text())
    models = {c["expect"]["model"] for c in data["decide"]}
    assert {"claude-opus-4-8", "claude-haiku-4-5", "local-model"} <= models
    assert any(c["expect"]["fallback"] for c in data["decide"])
    assert {c["expect"] for c in data["label"]} == {"bad", "good", None}
