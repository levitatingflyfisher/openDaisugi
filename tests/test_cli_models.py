"""CLI: `daisugi models list|search|use|pin`."""

from __future__ import annotations

import json

import pytest
from typer.testing import CliRunner

import opendaisugi.model_catalog as mc
import opendaisugi.model_registry as mr
from opendaisugi.cli import app
from opendaisugi.model_registry import ModelRef

runner = CliRunner()
DESK = {"OPENDAISUGI_VOICE_HARDWARE": "16,8,0"}
SMALL = {"OPENDAISUGI_VOICE_HARDWARE": "4,2,0"}


def test_models_help():
    res = runner.invoke(app, ["models", "--help"])
    assert res.exit_code == 0
    for sub in ("list", "search", "use"):
        assert sub in res.output


def test_list_names_the_default_for_this_box(tmp_path):
    res = runner.invoke(app, ["models", "list", "--data-dir", str(tmp_path)], env=DESK)
    assert res.exit_code == 0, res.output
    lines = res.output.splitlines()
    assert lines[0] == "This box has 16 GB of RAM, so the default is ibm-granite/granite-4.1-3b."
    assert lines[1] == "In use: the default."
    assert "ibm-granite/granite-4.0-1b" in res.output
    assert "mistralai/Ministral-3-3B-Instruct-2512" in res.output
    assert "meta-llama/Llama-3.2-1B-Instruct" in res.output


def test_list_json_holds_only_the_neutral_fields(tmp_path):
    res = runner.invoke(app, ["models", "list", "--json", "--data-dir", str(tmp_path)], env=SMALL)
    assert res.exit_code == 0, res.output
    body = json.loads(res.output)
    assert body["default"] == "ibm-granite/granite-4.0-1b"
    assert body["in_use"] is None
    assert body["hardware"] == {"ram_gb": 4.0, "vram_gb": 0.0, "class": "weak"}
    assert body["models"] == mc.load()["models"]


def test_use_records_any_id_and_list_shows_it(tmp_path):
    for mid in ("Qwen/Qwen2.5-1.5B-Instruct", "/m/own.gguf", "acme/new-model"):
        res = runner.invoke(app, ["models", "use", mid, "--data-dir", str(tmp_path)])
        assert res.exit_code == 0, res.output
        assert res.output.startswith(f"The garden now uses {mid} (recorded in ")
        assert mc.in_use(tmp_path) == mid
    res = runner.invoke(app, ["models", "list", "--data-dir", str(tmp_path)], env=DESK)
    assert "In use: acme/new-model (recorded by daisugi models use)." in res.output


def test_use_refuses_only_a_blank_id(tmp_path):
    res = runner.invoke(app, ["models", "use", " ", "--data-dir", str(tmp_path)])
    assert res.exit_code == 2
    assert mc.in_use(tmp_path) is None


@pytest.fixture(autouse=True)
def _online(monkeypatch):
    # CI sets HF_HUB_OFFLINE for the whole run; these cases fake the API, so
    # they must not see it.
    monkeypatch.delenv("HF_HUB_OFFLINE", raising=False)


def _fake_fetch(rows, asked):
    def fetch(url):
        asked.append(url)
        return json.dumps(rows).encode()

    return fetch


def test_search_shows_results_in_the_same_fields(monkeypatch):
    asked: list[str] = []
    rows = [
        {"id": "Qwen/Qwen2.5-1.5B-Instruct", "safetensors": {"total": 1543714304}},
        {"id": "big/one", "safetensors": {"total": 70000000000}},
    ]
    monkeypatch.setattr(mc, "fetch", _fake_fetch(rows, asked))
    res = runner.invoke(
        app, ["models", "search", "qwen"], env={**DESK, "HF_ENDPOINT": "http://h:1"}
    )
    assert res.exit_code == 0, res.output
    assert asked == ["http://h:1" + mc.search_target("qwen")]
    assert "Qwen/Qwen2.5-1.5B-Instruct" in res.output
    assert "big/one" not in res.output  # over the 8B the desk default allows


def test_search_json(monkeypatch):
    rows = [{"id": "a/b", "safetensors": {"total": 1000}, "cardData": {"license": "mit"}}]
    monkeypatch.setattr(mc, "fetch", _fake_fetch(rows, []))
    res = runner.invoke(
        app,
        ["models", "search", "x", "--json", "--license", "mit", "--max-params", "0"],
        env={**SMALL, "HF_ENDPOINT": "http://h:1"},
    )
    assert res.exit_code == 0, res.output
    body = json.loads(res.output)
    assert body["query"] == "x"
    assert body["endpoint"] == "http://h:1"
    assert body["max_params_b"] == 0.0
    assert body["licenses"] == ["mit"]
    assert [tuple(r) for r in body["results"]] == [mc.ENTRY_KEYS]


def test_search_offline_says_so_in_one_line(monkeypatch):
    def offline(url):
        raise mc.Offline("refused")

    monkeypatch.setattr(mc, "fetch", offline)
    res = runner.invoke(app, ["models", "search", "x"], env={**DESK, "HF_ENDPOINT": "http://h:1"})
    assert res.exit_code == 1
    assert res.output == "Offline: the Hugging Face API at http://h:1 could not be reached.\n"


def test_pin_resolves_any_named_repo(monkeypatch):
    monkeypatch.setattr(
        mr,
        "resolve_pinned",
        lambda repo, **kw: ModelRef(repo_id=repo, filename="m.Q4_K_M.gguf", revision="abc123"),
    )
    res = runner.invoke(app, ["models", "pin", "anyone/x", "--suffix", ".gguf"])
    assert res.exit_code == 0, res.output
    assert "m.Q4_K_M.gguf" in res.output
    assert "abc123" in res.output
