"""rank: elimination, quality, the Bradley-Terry fit with its seeded
bootstrap, owner constraints, cost, and the choice record with its cards."""

from __future__ import annotations

import json
import time
from pathlib import Path

import pytest
from typer.testing import CliRunner

from opendaisugi import rank
from opendaisugi.cli import app
from opendaisugi.rank_rule import names_rank_record


def _att(aid: str, **kw) -> dict:
    return {"id": aid, "content_hash": f"h-{aid}", **kw}


def _cmp(judge: str, pair: str, a: str, b: str, ab: str, ba: str) -> list[dict]:
    """A judge's two calls on one pair: the pair shown a-first, then b-first.
    ``ab`` and ``ba`` name the winner by attempt id, or "tie" / "skip"."""

    def out(w: str, first: str) -> str:
        if w in ("tie", "skip"):
            return w
        return "a" if w == first else "b"

    return [
        {
            "a": a,
            "b": b,
            "a_hash": f"h-{a}",
            "b_hash": f"h-{b}",
            "shown": "ab",
            "outcome": out(ab, a),
            "judge": judge,
            "pair_id": pair,
        },
        {
            "a": b,
            "b": a,
            "a_hash": f"h-{b}",
            "b_hash": f"h-{a}",
            "shown": "ba",
            "outcome": out(ba, b),
            "judge": judge,
            "pair_id": pair,
        },
    ]


def _doc(attempts: list[dict], comps=None, owner=None, **policy) -> dict:
    d: dict = {"ranking_id": "r1", "attempts": attempts}
    if comps is not None:
        d["comparisons"] = comps
    if owner is not None:
        d["owner_constraints"] = owner
    d["policy"] = {"bootstrap": 200, **policy}
    return d


def _fit(doc: dict) -> dict:
    return rank.fit(rank.parse(doc))


@pytest.mark.parametrize(
    ("doc", "want"),
    [
        ({"attempts": []}, "ranking_id"),
        ({"ranking_id": "r", "attempts": []}, "1 to 8"),
        ({"ranking_id": "r", "attempts": [_att("a")] * 2}, "used twice"),
        ({"ranking_id": "r", "attempts": [{"id": "a b", "content_hash": "x"}]}, "id must be"),
        ({"ranking_id": "r", "attempts": [{"id": "a"}]}, "content_hash"),
        (
            {"ranking_id": "r", "attempts": [_att("a", tests=[{"name": "t", "result": "ok"}])]},
            "result must be",
        ),
        ({"ranking_id": "r", "attempts": [_att("a", cost={"tokens": -1})]}, "whole number"),
        ({"ranking_id": "r", "attempts": [_att("a", cost={"tokens": True})]}, "whole number"),
        ({"ranking_id": "r", "attempts": [_att("a")], "policy": {"threshold": 0.2}}, "threshold"),
        ({"ranking_id": "r", "attempts": [_att("a")], "policy": {"bootstrap": 0}}, "bootstrap"),
        (
            {"ranking_id": "r", "attempts": [_att("a")], "owner_constraints": [["a", "z"]]},
            "no attempt",
        ),
    ],
)
def test_parse_refuses(doc, want):
    with pytest.raises(rank.RankError, match=want):
        rank.parse(doc)


def test_elimination_reasons_in_order():
    res = _fit(
        _doc(
            [
                _att(
                    "a",
                    started_by_parent=True,
                    ran_plan=True,
                    tests=[
                        {"name": "t1", "required": True, "result": "fail"},
                        {"name": "t2", "required": True, "result": "not_run"},
                    ],
                    features=[{"name": "f", "required": True, "green": False}],
                ),
                _att("b", edge_proof="failed", verify={"ok": False, "violations": 2}),
            ]
        )
    )
    assert res["status"] == "none_survived"
    assert res["leader"] is None
    assert res["eliminated"] == [
        {
            "id": "a",
            "reasons": [
                "edge proof missing",
                "verify missing",
                "required test failed: t1",
                "required test not run: t2",
                "required feature not green: f",
            ],
        },
        {"id": "b", "reasons": ["edge proof failed: never ran", "verify failed: 2 violations"]},
    ]
    assert rank.EXIT[res["status"]] == 1


def test_single_survivor_is_the_label():
    res = _fit(
        _doc([_att("a"), _att("b", tests=[{"name": "t", "required": True, "result": "fail"}])])
    )
    assert (res["status"], res["leader"], res["label"], res["confidence"]) == (
        "single",
        "a",
        "a",
        1.0,
    )


def test_quality_tiers_decide_before_votes():
    q = [{"name": "o", "required": False, "result": "pass"}]
    res = _fit(_doc([_att("a"), _att("b", tests=q)], _cmp("j", "p1", "a", "b", "a", "a")))
    assert res["quality_tiers"] == [["b"], ["a"]]
    assert res["order"] == ["b", "a"]
    assert res["decided_by"] == ["quality"]
    assert res["status"] == "ranked"


def test_votes_count_only_when_both_orders_agree():
    agree = _cmp("j1", "p1", "a", "b", "b", "b") + _cmp("j2", "p2", "a", "b", "b", "b")
    res = _fit(_doc([_att("a"), _att("b")], agree))
    assert res["order"] == ["b", "a"]
    assert res["scores"]["b"]["strength"] > res["scores"]["a"]["strength"]
    split = _cmp("j1", "p1", "a", "b", "a", "b")
    res2 = _fit(_doc([_att("a"), _att("b")], split))
    assert res2["scores"]["a"]["strength"] == res2["scores"]["b"]["strength"]


def test_a_stale_hash_is_no_vote():
    comps = _cmp("j1", "p1", "a", "b", "b", "b")
    comps[0]["b_hash"] = "old"
    res = _fit(_doc([_att("a"), _att("b")], comps))
    assert any("hash that changed" in w for w in res["warnings"])
    assert res["scores"]["a"]["votes"] == 0


def test_one_vote_per_judge_per_pair():
    comps = _cmp("j", "p1", "a", "b", "b", "b") + _cmp("j", "p0", "a", "b", "a", "a")
    res = _fit(_doc([_att("a"), _att("b")], comps))
    assert any("more than once" in w for w in res["warnings"])
    assert res["scores"]["a"]["votes"] == 1
    # The vote of the smaller call id (p0) is kept.
    assert res["order"][0] == "a"


def test_the_fit_is_deterministic_and_replays():
    comps = _cmp("j1", "p1", "a", "b", "a", "a") + _cmp("j2", "p2", "b", "c", "b", "b")
    doc = _doc([_att("a"), _att("b"), _att("c")], comps, judges=["j1", "j2", "j3"])
    assert _fit(doc) == _fit(json.loads(json.dumps(doc)))
    res = _fit(doc)
    assert res["seed"] == rank.seed_of(rank.parse(doc), rank.parse(doc).comparisons, [])
    assert 0.0 <= res["confidence"] <= 1.0


def test_no_votes_is_provisional_and_cost_orders():
    res = _fit(
        _doc(
            [_att("a", cost={"tokens": 900}), _att("b", cost={"tokens": 100}), _att("c")],
            judges=["j"],
        )
    )
    assert res["status"] == "provisional"
    assert res["order"] == ["b", "a", "c"]
    assert res["decided_by"] == ["cost", "cost"]
    assert res["label"] == "no_label"
    assert res["next"]["judge"] == "j" and res["next"]["pair"][0] == "b"
    assert rank.EXIT[res["status"]] == 3


def test_judges_exhausted():
    res = _fit(_doc([_att("a"), _att("b")], _cmp("j", "p", "a", "b", "a", "b"), judges=["j"]))
    assert res["status"] == "provisional"
    assert (res["next"], res["stop"]) == (None, "judges_exhausted")


def test_owner_constraint_moves_the_order_and_labels():
    comps = []
    for k in range(4):
        comps += _cmp(f"j{k}", f"p{k}", "a", "b", "a", "a")
    res = _fit(_doc([_att("a"), _att("b")], comps, owner=[["b", "a"]]))
    assert res["order"] == ["b", "a"]
    assert res["decided_by"] == ["owner"]
    assert (res["status"], res["label"]) == ("ranked", "b")


def test_an_owner_cycle_applies_none():
    res = _fit(_doc([_att("a"), _att("b"), _att("c")], owner=[["a", "b"], ["b", "c"], ["c", "a"]]))
    assert res["owner_constraints"] == []
    assert any("cycle" in w for w in res["warnings"])


def test_a_disconnected_graph_still_fits():
    comps = _cmp("j", "p1", "a", "b", "a", "a") + _cmp("j", "p2", "c", "d", "c", "c")
    res = _fit(_doc([_att(x) for x in "abcd"], comps))
    assert all(0.0 < res["scores"][x]["strength"] < 1.0 for x in "abcd")


def test_splitmix_known_values():
    g = rank.SplitMix64(0)
    assert [g.next() for _ in range(3)] == [
        0xE220A8397B1DCDAF,
        0x6E789E6AA1B965F4,
        0x06C45D188009454F,
    ]


def test_the_warden_rule_covers_the_owner_verbs():
    assert names_rank_record("daisugi rank record pick ch_000000000000 a")
    assert names_rank_record("daisugi rank record drop ch_000000000000")
    assert not names_rank_record("daisugi rank queue --json")
    assert not names_rank_record("daisugi rank choose a.json")
    assert not names_rank_record("daisugi rank fit a.json")


# ---------------------------------------------------------------------------
# The CLI: choose, queue, record, decay
# ---------------------------------------------------------------------------


def _write(p: Path, doc: dict) -> Path:
    p.write_text(json.dumps(doc), encoding="utf-8")
    return p


def _run(*args: str):
    return CliRunner().invoke(app, ["rank", *args])


def test_fit_and_queue_create_nothing(tmp_path):
    f = _write(tmp_path / "a.json", _doc([_att("a"), _att("b")]))
    dd = tmp_path / "dd"
    res = _run("fit", str(f), "--data-dir", str(dd), "--json")
    assert res.exit_code == 3, res.output
    res = _run("queue", "--data-dir", str(dd))
    assert res.exit_code == 0 and "No open cards." in res.output
    assert not dd.exists()


def test_choose_opens_a_card_once_and_drop_confirms(tmp_path):
    f = _write(tmp_path / "a.json", _doc([_att("a", cost={"diff_lines": 3}), _att("b")]))
    dd = tmp_path / "dd"
    res = _run("choose", str(f), "--data-dir", str(dd), "--json")
    assert res.exit_code == 3, res.output
    out = json.loads(res.output)
    cid = out["choice_id"]
    assert out["recorded"] is True and cid.startswith("ch_")
    again = json.loads(_run("choose", str(f), "--data-dir", str(dd), "--json").output)
    assert again["recorded"] is False and again["choice_id"] == cid
    q = json.loads(_run("queue", "--data-dir", str(dd), "--json").output)
    assert [c["choice_id"] for c in q["cards"]] == [cid]
    card = q["cards"][0]
    assert card["chosen"] == "a" and card["alternatives"] == ["b"]
    assert card["switch"]["cost"] == "cheap"
    res = _run("record", "drop", cid, "--data-dir", str(dd))
    assert res.exit_code == 0 and "confirmed" in res.output
    q = json.loads(_run("queue", "--data-dir", str(dd), "--json").output)
    assert q["cards"] == []
    res = _run("record", "pick", cid, "b", "--data-dir", str(dd))
    assert res.exit_code == 1 and "answered already" in res.output


def test_pick_overrides_and_feeds_the_next_fit(tmp_path):
    comps = []
    for k in range(3):
        comps += _cmp(f"j{k}", f"p{k}", "a", "b", "a", "a")
    doc = _doc([_att("a"), _att("b")], comps)
    f = _write(tmp_path / "a.json", doc)
    dd = tmp_path / "dd"
    cid = json.loads(_run("choose", str(f), "--data-dir", str(dd), "--json").output)["choice_id"]
    res = _run("record", "pick", cid, "b", "--data-dir", str(dd))
    assert res.exit_code == 0, res.output
    assert "switch from a to b" in res.output
    res = _run("record", "pick", cid, "z", "--data-dir", str(dd))
    assert res.exit_code == 1  # answered already
    out = json.loads(_run("fit", str(f), "--data-dir", str(dd), "--json").output)
    assert out["order"] == ["b", "a"] and out["label"] == "b"


def test_a_card_decays_after_fourteen_days_and_a_writer_records_it(tmp_path):
    dd = tmp_path / "dd"
    d = rank.rankings_dir(dd)
    d.mkdir(parents=True)
    old = time.time() - 15 * 86400
    row = {
        "choice_id": "ch_000000000001",
        "ranking_id": "r0",
        "event": "opened",
        "options": {
            "survivors": [{"id": "a", "content_hash": "h-a"}, {"id": "b", "content_hash": "h-b"}],
            "eliminated": [],
        },
        "chosen": "a",
        "status": "provisional",
        "facts": {},
        "ts": old,
    }
    (d / "choices.jsonl").write_text(json.dumps(row) + "\n", encoding="utf-8")
    before = (d / "choices.jsonl").read_text()
    q = json.loads(_run("queue", "--data-dir", str(dd), "--json").output)
    assert q == {"cards": [], "decayed": 1}
    assert (d / "choices.jsonl").read_text() == before
    res = _run("record", "pick", "ch_000000000001", "b", "--data-dir", str(dd))
    assert res.exit_code == 0, res.output
    rows = [json.loads(x) for x in (d / "choices.jsonl").read_text().splitlines()]
    assert [r["event"] for r in rows] == ["opened", "ignored", "overridden"]
    assert rows[1]["trigger"] == "time" and rows[1]["fired_at"] == old + 14 * 86400


def test_a_gone_alternative_is_follow_up_only(tmp_path):
    dd = tmp_path / "dd"
    f = _write(
        tmp_path / "a.json",
        _doc([_att("a"), _att("b", where={"kind": "worktree", "path": str(tmp_path / "gone")})]),
    )
    res = _run("choose", str(f), "--data-dir", str(dd), "--json")
    assert res.exit_code == 3, res.output
    q = json.loads(_run("queue", "--data-dir", str(dd), "--json").output)
    assert q == {"cards": [], "decayed": 1}


def _card_with_run(dd: Path, rows_after: list[dict] | None = None) -> None:
    d = rank.rankings_dir(dd)
    d.mkdir(parents=True, exist_ok=True)
    row = {
        "choice_id": "ch_000000000002",
        "ranking_id": "r0",
        "event": "opened",
        "options": {
            "survivors": [{"id": "a", "content_hash": "h-a"}, {"id": "b", "content_hash": "h-b"}],
            "eliminated": [],
        },
        "chosen": "a",
        "status": "provisional",
        "facts": {"run": {"run_id": "run_a", "step": "t", "downstream": ["w", "x"]}},
        "ts": time.time(),
    }
    lines = [row, *(rows_after or [])]
    (d / "choices.jsonl").write_text("".join(json.dumps(r) + "\n" for r in lines))


def _receipts(dd: Path, rows: list[tuple[str, str, str, float]]) -> None:
    import sqlite3

    db = dd / "journal" / "index.db"
    db.parent.mkdir(parents=True, exist_ok=True)
    con = sqlite3.connect(db)
    con.execute(
        "CREATE TABLE receipts (run_id TEXT, step_id TEXT, reversibility TEXT, timestamp REAL)"
    )
    con.executemany("INSERT INTO receipts VALUES (?, ?, ?, ?)", rows)
    con.commit()
    con.close()


def test_switch_cost_counts_the_runs_that_resumed_the_card(tmp_path):
    dd = tmp_path / "dd"
    _receipts(dd, [("run_b", "w", "reversible", 10.0), ("run_c", "x", "irreversible", 20.0)])
    _card_with_run(dd)
    (card,) = rank.read_cards(dd)
    assert card.resumed == []
    assert rank.switch_cost(card, dd)["cost"] == "cheap"
    resumed = {"choice_id": "ch_000000000002", "ranking_id": "r0", "event": "resumed"}
    _card_with_run(dd, [{**resumed, "run_id": "run_b", "ts": 1.0}])
    (card,) = rank.read_cards(dd)
    assert card.resumed == ["run_b"]
    sc = rank.switch_cost(card, dd)
    assert sc["cost"] == "costly" and sc["undo_steps"] == 1
    _card_with_run(
        dd,
        [
            {**resumed, "run_id": "run_b", "ts": 1.0},
            {**resumed, "run_id": "run_b", "ts": 2.0},
            {**resumed, "run_id": 7, "ts": 2.0},
            {**resumed, "run_id": "run_c", "ts": 3.0},
        ],
    )
    (card,) = rank.read_cards(dd)
    assert card.resumed == ["run_b", "run_c"]
    sc = rank.switch_cost(card, dd)
    assert sc["cost"] == "follow_up_only" and sc["fired_at"] == 20.0


def test_show_reads_votes_and_choices_and_creates_nothing(tmp_path):
    dd = tmp_path / "dd"
    f = _write(
        tmp_path / "a.json",
        _doc([_att("a"), _att("b")], comps=_cmp("j1", "p1", "a", "b", "a", "a")),
    )
    out = json.loads(_run("choose", str(f), "--data-dir", str(dd), "--json").output)
    cid = out["choice_id"]
    res = _run("show", "--choice", cid, "--data-dir", str(dd), "--json")
    assert res.exit_code == 0, res.output
    doc = json.loads(res.output)
    assert doc["ranking_id"] == "r1" and [c["event"] for c in doc["choices"]] == ["opened"]
    assert doc["card"]["choice_id"] == cid
    before = sorted(p.name for p in dd.rglob("*"))
    _run("show", "--ranking", "r1", "--data-dir", str(dd))
    assert sorted(p.name for p in dd.rglob("*")) == before


def test_show_refuses_two_selectors_and_names_a_missing_ranking(tmp_path):
    dd = tmp_path / "dd"
    assert _run("show", "--ranking", "r1", "--choice", "ch_x", "--data-dir", str(dd)).exit_code == 2
    res = _run("show", "--ranking", "nope", "--data-dir", str(dd))
    assert res.exit_code == 1 and "No ranking nope." in res.output
    assert not dd.exists()


def test_tau_b_handles_ties():
    assert rank.kendall_tau_b(["a", "b", "c"], ["a", "b", "c"]) == 1.0
    assert rank.kendall_tau_b(["a", "b", "c"], ["c", "b", "a"]) == -1.0
    assert rank.kendall_tau_b([["a", "b"], "c"], ["a", "b", "c"]) == pytest.approx(0.816496581)
    assert rank.kendall_tau_b(["a"], ["a"]) is None


def test_cyclic_magnitude_needs_a_loop():
    def v(w, lo):
        return rank.Vote(i=w, j=lo, win=1.0, judge="j")

    assert rank.cyclic_magnitude([v("a", "b"), v("b", "c")]) is None
    assert rank.cyclic_magnitude([v("a", "b"), v("b", "c"), v("c", "a")]) == pytest.approx(1.0)
    assert rank.cyclic_magnitude([v("a", "b"), v("b", "c"), v("a", "c")]) == pytest.approx(0.0)


def test_a_judge_under_the_floor_is_dropped(tmp_path):
    dd = tmp_path / "dd"
    for n in range(5):
        comps = _cmp("j-bad", f"p{n}", "a", "b", "a", "a")
        doc = {**_doc([_att("a"), _att("b")], comps=comps), "ranking_id": f"r{n}"}
        f = _write(tmp_path / f"{n}.json", doc)
        cid = json.loads(_run("choose", str(f), "--data-dir", str(dd), "--json").output)[
            "choice_id"
        ]
        assert _run("record", "pick", cid, "b", "--data-dir", str(dd)).exit_code == 0
        # the votes live in the journal, as rank judge writes them
        rank.append_comparisons(dd, [{**c, "ranking_id": f"r{n}"} for c in comps])
    out = json.loads(_run("judges", "--data-dir", str(dd), "--json").output)
    t = out["judges"]["j-bad"]
    assert t["cards"] == 5 and t["agree"] == 0.0 and t["dropped"] is True
    assert (t["agreed"], t["disagreed"]) == (0, 5)
    assert "j-bad" not in out["weights"]


def test_judges_reads_nothing_into_existence(tmp_path):
    res = _run("judges", "--data-dir", str(tmp_path / "dd"), "--json")
    assert res.exit_code == 0 and json.loads(res.output)["judges"] == {}
    assert not (tmp_path / "dd").exists()


def test_beta_sample_is_deterministic_and_in_range():
    a = rank.beta_sample(3, 2, rank.SplitMix64(7))
    assert a == rank.beta_sample(3, 2, rank.SplitMix64(7)) and 0.0 < a < 1.0
    assert rank.beta_sample(1, 1, rank.SplitMix64(7)) == rank.beta_sample(1, 1, rank.SplitMix64(7))


def test_weights_follow_the_posterior_and_replay_exactly():
    trust = {
        "good": {"agreed": 40, "disagreed": 2, "dropped": False},
        "poor": {"agreed": 6, "disagreed": 6, "dropped": False},
        "cut": {"agreed": 0, "disagreed": 5, "dropped": True},
    }
    w1 = rank.judge_weights(trust, "ab12")
    assert w1 == rank.judge_weights(trust, "ab12") and "cut" not in w1
    assert w1["good"] > 1.4 and 0.0 < w1["poor"] < 2.0
    assert rank.judge_weights(trust, "cd34") != w1


def _doc_two_judges() -> dict:
    comps = _cmp("j1", "p1", "a", "b", "a", "a") + _cmp("j2", "p2", "a", "b", "b", "b")
    return _doc([_att("a"), _att("b")], comps=comps)


def test_fit_uses_weights_and_names_them():
    res = rank.fit(rank.parse(_doc_two_judges()), weights={"j1": 1.9, "j2": 0.1})
    assert res["judge_weights"] == {"j1": 1.9, "j2": 0.1}
    assert res["order"][0] == "a"
    res2 = rank.fit(rank.parse(_doc_two_judges()), weights={"j1": 0.1, "j2": 1.9})
    assert res2["scores"]["b"]["strength"] > res2["scores"]["a"]["strength"]
    assert res["scores"]["a"]["strength"] > res["scores"]["b"]["strength"]
    assert "judge_weights" not in rank.fit(rank.parse(_doc_two_judges()))


def test_a_dropped_judge_has_no_vote():
    res = rank.fit(rank.parse(_doc_two_judges()), weights={"j1": 1.0})
    assert res["order"][0] == "a" and res["scores"]["a"]["votes"] == 1


def test_beta_sample_values_are_pinned_for_the_ports():
    assert rank.beta_sample(3, 2, rank.SplitMix64(7)) == 0.582930293
    assert rank.beta_sample(1, 1, rank.SplitMix64(7)) == 0.389829748
    trust = {
        "good": {"agreed": 40, "disagreed": 2, "dropped": False},
        "poor": {"agreed": 6, "disagreed": 6, "dropped": False},
    }
    assert rank.judge_weights(trust, "ab12") == {"good": 1.921783876, "poor": 0.589813668}


def test_a_card_decays_after_fourteen_days_not_seven(tmp_path):
    def card(age_days):
        dd = tmp_path / f"dd{age_days}"
        _card_with_run(dd)
        rows = [
            json.loads(x)
            for x in (rank.rankings_dir(dd) / "choices.jsonl").read_text().splitlines()
        ]
        rows[0]["ts"] = time.time() - age_days * 86400
        (rank.rankings_dir(dd) / "choices.jsonl").write_text(
            "".join(json.dumps(r) + "\n" for r in rows)
        )
        return dd, rank.read_cards(dd)[0]

    dd, young = card(10)
    assert rank.decay(young, dd, time.time()) is None
    assert rank.queue(dd, time.time(), "date")["cards"][0]["decays_in_days"] == 4
    dd, old = card(15)
    assert rank.decay(old, dd, time.time())["trigger"] == "time"
    assert rank.DECAY_SECONDS == 14 * 86400
