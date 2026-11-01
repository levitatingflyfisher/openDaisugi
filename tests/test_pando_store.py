"""The Python reader over a Pando R2 store (ADR-0023, PD-R-10)."""

from pathlib import Path

import pytest

from opendaisugi.pando import PandoStore, list_stores, store_dir

GOLDEN = Path(__file__).resolve().parents[1] / "clients" / "fixtures" / "pando" / "golden" / "out"


def test_store_dir_rules(tmp_path):
    assert store_dir(tmp_path, "work") == tmp_path / "pando" / "work"
    with pytest.raises(ValueError, match="one store at a time"):
        store_dir(tmp_path, "all")
    for bad in ["", "Work", "1x", "a/b", "x" * 33]:
        with pytest.raises(ValueError):
            store_dir(tmp_path, bad)


def test_list_stores_only_real_ones(tmp_path):
    (tmp_path / "pando" / "work" / "main").mkdir(parents=True)
    (tmp_path / "pando" / "home" / "main").mkdir(parents=True)
    (tmp_path / "pando" / "junk").mkdir(parents=True)
    assert list_stores(tmp_path) == ["home", "work"]


def test_reads_the_golden_store_like_go():
    s = PandoStore.open(GOLDEN)
    assert s.count == 164
    m = s.message(0)
    assert m.i == 0 and m.date.endswith("Z")
    assert s.zoom(0, 1) == m.text
    assert s.date(0) == m.date
    # The saved view, not a rebuild: the same 18 lines the Go fixture holds.
    assert len(s.view_lines()) == 18
    assert s.view_lines()[0][0] == "0+64"


def test_zoom_matches_the_go_probes():
    """Every probe of the golden fixture gives the text the Go package gave."""
    import json

    probes = json.loads((GOLDEN / "probes.json").read_text())
    s = PandoStore.open(GOLDEN)
    checked = 0
    for p in probes:
        if p["op"] == "zoom":
            if p.get("err"):
                with pytest.raises(ValueError):
                    s.zoom(p["id"], p["n"])
            else:
                assert s.zoom(p["id"], p["n"]) == p["out"], p
            checked += 1
        elif p["op"] == "date":
            if p.get("err"):
                with pytest.raises(ValueError):
                    s.date(p["id"])
            else:
                assert s.date(p["id"]) == p["out"], p
            checked += 1
    assert checked >= 5


def test_view_text_matches_the_go_render():
    s = PandoStore.open(GOLDEN)
    want = (GOLDEN / "view.txt").read_text()
    got = "<chat>\n" + "".join(f"{n}|{t}\n" for n, t in s.view_lines()) + "</chat>\n"
    assert got == want


def test_a_torn_last_line_is_skipped(tmp_path):
    d = tmp_path / "pando" / "work"
    (d / "main").mkdir(parents=True)
    (d / "main" / "2026-10-08.jsonl").write_text(
        '{"i":0,"kind":"user","text":"one","size":3,"date":"2026-10-08T00:00:00Z"}\n{"i":1,"kind"'
    )
    s = PandoStore.open(d)
    assert s.count == 1


def test_missing_store_says_where(tmp_path):
    with pytest.raises(FileNotFoundError, match="no store"):
        PandoStore.open(tmp_path / "pando" / "home")


def test_python_never_writes():
    assert not hasattr(PandoStore, "append")


def test_recall_ranks_by_words(tmp_path):
    d = tmp_path / "pando" / "work"
    (d / "main").mkdir(parents=True)
    lines = [
        '{"i":0,"kind":"user","text":"the index is sqlite","size":19,"date":"2026-10-08T00:00:00Z"}',
        '{"i":1,"kind":"user","text":"lunch was late","size":14,"date":"2026-10-08T00:00:01Z"}',
        '{"i":2,"kind":"user","text":"sqlite index rebuilds from files","size":32,"date":"2026-10-08T00:00:02Z"}',
    ]
    (d / "main" / "2026-10-08.jsonl").write_text("\n".join(lines) + "\n")
    hits = PandoStore.open(d).recall("sqlite index", 2)
    assert [h.id for h in hits] == ["0+1", "2+1"]
    assert all("lunch" not in h.line for h in hits)


def test_a_reader_sees_every_message_past_the_saved_view(tmp_path):
    """As Go's read only open: the view is the snapshot, and the messages
    past it are a raw tail read to the end of the log, stopping at the first
    id that does not follow on."""
    import json

    from opendaisugi.pando.store import PandoStore

    main = tmp_path / "main"
    main.mkdir()
    rows = [
        {"i": i, "kind": "user", "text": f"m{i}", "size": 2, "date": "2026-11-02T00:00:00Z"}
        for i in (0, 1, 2, 4)
    ]
    (main / "2026-11-02.jsonl").write_text(
        "".join(json.dumps(r) + "\n" for r in rows), encoding="utf-8"
    )
    (tmp_path / "view.json").write_text(
        '{"count":1,"batch":false,"lines":[[0,0]]}', encoding="utf-8"
    )
    s = PandoStore.open(tmp_path)
    assert s.count == 3
    assert s.not_in_view == 2
    assert "m2" in s.zoom(2, 1)


def _seed(root, store, text):
    import json

    d = root / "pando" / store / "main"
    d.mkdir(parents=True)
    row = {"i": 0, "kind": "user", "text": text, "size": len(text), "date": "2026-11-02T00:00:00Z"}
    (d / "2026-11-02.jsonl").write_text(json.dumps(row) + "\n", encoding="utf-8")
    return root / "pando" / store


def test_a_symlink_in_a_store_or_child_path_is_refused(tmp_path):
    """The reviewer's probe: children/<label> as a link to another store."""
    import os

    import pytest

    from opendaisugi.pando.store import PandoStore, child_dir, list_stores, store_dir

    _seed(tmp_path, "home", "a private home line")
    work = _seed(tmp_path, "work", "a work line")
    (work / "children").mkdir()
    os.symlink("../../home", work / "children" / "evil")
    with pytest.raises(ValueError, match="link"):
        child_dir(work, "evil")
    os.symlink("home", tmp_path / "pando" / "other")
    with pytest.raises(ValueError, match="link"):
        store_dir(tmp_path, "other")
    assert "other" not in list_stores(tmp_path)
    side = tmp_path / "pando" / "side"
    side.mkdir()
    os.symlink("../home/main", side / "main")
    with pytest.raises(ValueError, match="link"):
        PandoStore.open(side)


def test_the_mcp_zoom_reads_a_child_store(tmp_path):
    import pytest

    from opendaisugi.pando.store import PandoStore

    work = _seed(tmp_path, "work", "the parent line")
    _seed(tmp_path, "home", "a private home line")
    child = work / "children" / "w1"
    import json

    (child / "main").mkdir(parents=True)

    row = {
        "i": 0,
        "kind": "user",
        "text": "the worker's own words",
        "size": 22,
        "date": "2026-11-02T00:00:00Z",
    }
    (child / "main" / "2026-11-02.jsonl").write_text(json.dumps(row) + "\n", encoding="utf-8")
    s = PandoStore.open(work)
    assert "the worker's own words" in s.zoom_child("w1", 0, 1)
    for bad in ("../../home", "nobody"):
        with pytest.raises((ValueError, FileNotFoundError)):
            s.zoom_child(bad, 0, 1)


def test_a_linked_day_file_is_not_read(tmp_path):
    import os

    from opendaisugi.pando.store import PandoStore

    home = _seed(tmp_path, "home", "a private home line")
    work = tmp_path / "pando" / "work" / "main"
    work.mkdir(parents=True)
    os.symlink(home / "main" / "2026-11-02.jsonl", work / "2026-11-02.jsonl")
    assert PandoStore.open(work.parent).count == 0
