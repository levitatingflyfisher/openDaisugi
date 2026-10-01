"""The knn router on the command line: route --knn, route feedback, router status,
the router choice, and the pieces view. The lexical embedder stands in for a model."""

from __future__ import annotations

import json
from pathlib import Path

import pytest
from typer.testing import CliRunner

from opendaisugi import route_knn as rk
from opendaisugi._search import _get_model
from opendaisugi.cli import app
from opendaisugi.config import Config, save_config
from opendaisugi.modules import ACTIVE, AVAILABLE, detect_stages

runner = CliRunner()
OPUS = "claude-opus-4-8"
HAIKU = "claude-haiku-4-5"
ID = "lexical-hash-v1"
TASK = "rename the helper function in the parser module"


@pytest.fixture(autouse=True)
def _lexical(monkeypatch, tmp_path):
    """The lexical matcher in a throwaway default data dir."""
    monkeypatch.setattr("opendaisugi.DEFAULT_DATA_DIR", tmp_path / "default")
    save_config(Config(matcher_model="lexical"), tmp_path / "default" / "config.yaml")
    from opendaisugi import _search

    _search.reload()
    yield
    _search.reload()


def _seed(data_dir: Path, model: str, outcome: str, n: int, text: str = TASK) -> None:
    store = rk.OutcomeStore(data_dir / "gateway" / "routing.db")
    vec = [float(x) for x in _get_model().encode([text], convert_to_numpy=True)[0]]
    vec = rk.unpack_vec(rk.pack_vec(vec))
    for _ in range(n):
        store.add(
            conversation="c",
            ts=0.0,
            task_sha="s",
            embedder=ID,
            vec=vec,
            model=model,
            cost=0.0,
            latency_ms=None,
            outcome=outcome,
        )


def test_route_knn_shows_the_choice_and_its_neighbours(tmp_path) -> None:
    _seed(tmp_path, HAIKU, rk.GOOD, 4)
    res = runner.invoke(app, ["route", TASK, "--data-dir", str(tmp_path), "--knn"])
    assert res.exit_code == 0, res.output
    assert "knn:        claude-haiku-4-5; 4 neighbours, 4 known;" in res.output
    assert "threshold:  0.25" in res.output
    assert "    t1  1.00  claude-haiku-4-5  good" in res.output


def test_route_knn_json_adds_a_knn_key(tmp_path) -> None:
    _seed(tmp_path, HAIKU, rk.GOOD, 4)
    res = runner.invoke(app, ["route", TASK, "--data-dir", str(tmp_path), "--knn", "--json"])
    data = json.loads(res.output)
    assert data["knn"]["model"] == HAIKU
    assert data["knn"]["fallback"] is False
    assert data["knn"]["neighbours"][0] == {
        "turn_id": "t4",
        "similarity": 1.0,
        "model": HAIKU,
        "outcome": "good",
    }
    plain = json.loads(
        runner.invoke(app, ["route", TASK, "--data-dir", str(tmp_path), "--json"]).output
    )
    assert "knn" not in plain


def test_route_knn_with_no_table_says_why(tmp_path) -> None:
    res = runner.invoke(app, ["route", TASK, "--data-dir", str(tmp_path), "--knn"])
    assert res.exit_code == 0, res.output
    assert "0 of 0 neighbours have a known outcome (need 3); the rules router decides" in res.output
    assert not (tmp_path / "gateway" / "routing.db").exists()  # a read books nothing


def test_route_feedback_sets_an_outcome(tmp_path) -> None:
    _seed(tmp_path, HAIKU, rk.UNKNOWN, 1)
    res = runner.invoke(app, ["route", "feedback", "t1", "bad", "--data-dir", str(tmp_path)])
    assert res.exit_code == 0, res.output
    assert "turn t1: outcome set to bad" in res.output
    store = rk.OutcomeStore(tmp_path / "gateway" / "routing.db")
    assert store.recent(ID)[0].outcome == rk.BAD


def test_route_feedback_refuses_an_unknown_turn(tmp_path) -> None:
    res = runner.invoke(app, ["route", "feedback", "t9", "good", "--data-dir", str(tmp_path)])
    assert res.exit_code == 1
    assert "no booked turn 't9'" in res.output


def test_route_feedback_wants_good_or_bad(tmp_path) -> None:
    res = runner.invoke(app, ["route", "feedback", "t1", "maybe", "--data-dir", str(tmp_path)])
    assert res.exit_code == 2
    res = runner.invoke(app, ["route", "feedback", "--data-dir", str(tmp_path)])
    assert res.exit_code == 2


def test_a_plain_task_with_extra_words_is_a_usage_error(tmp_path) -> None:
    res = runner.invoke(app, ["route", "some task", "extra", "--data-dir", str(tmp_path)])
    assert res.exit_code == 2


def test_router_status_names_knn_and_lists_booked_turns(tmp_path) -> None:
    save_config(Config(matcher_model="lexical", gateway_router="knn"), tmp_path / "config.yaml")
    _seed(tmp_path, HAIKU, rk.GOOD, 2)
    _seed(tmp_path, OPUS, rk.BAD, 1)
    res = runner.invoke(app, ["router", "status", "--data-dir", str(tmp_path)])
    assert res.exit_code == 0, res.output
    assert "configured router: knn" in res.output
    assert "knn outcomes: 3 booked (good 2, bad 1, unknown 0)" in res.output
    assert "    t3  claude-opus-4-8  bad" in res.output
    data = json.loads(
        runner.invoke(app, ["router", "status", "--data-dir", str(tmp_path), "--json"]).output
    )
    assert data["knn"]["booked"] == 3


def test_router_status_without_knn_has_no_knn_section(tmp_path) -> None:
    save_config(Config(matcher_model="lexical"), tmp_path / "config.yaml")
    res = runner.invoke(app, ["router", "status", "--data-dir", str(tmp_path)])
    assert "knn outcomes" not in res.output


def test_install_accepts_knn_as_a_router(tmp_path, monkeypatch) -> None:
    home = tmp_path / "home"
    (home / ".claude").mkdir(parents=True)
    monkeypatch.setenv("HOME", str(home))
    res = runner.invoke(app, ["install", "--dry-run", "--gateway", "--router", "knn"])
    assert res.exit_code == 0, res.output
    assert "Router: set gateway_router to knn" in res.output


def test_install_still_refuses_an_unknown_router(tmp_path, monkeypatch) -> None:
    home = tmp_path / "home"
    (home / ".claude").mkdir(parents=True)
    monkeypatch.setenv("HOME", str(home))
    res = runner.invoke(app, ["install", "--dry-run", "--gateway", "--router", "nope"])
    assert res.exit_code == 1
    assert "choose one of: rules, switchyard, knn, model, off" in res.output


def test_the_module_map_has_a_knn_row(tmp_path) -> None:
    def knn_row(router: str):
        save_config(
            Config(matcher_model="lexical", gateway_router=router), tmp_path / "config.yaml"
        )
        stage = next(s for s in detect_stages(tmp_path, home=tmp_path) if s.key == "router")
        return next(m for m in stage.modules if m.name == "knn")

    assert knn_row("rules").state == AVAILABLE
    assert knn_row("knn").state == ACTIVE
