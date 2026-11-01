"""The read-only MCP server named pando."""

import asyncio
import json
import shutil
from pathlib import Path

from typer.testing import CliRunner

from opendaisugi.cli import app
from opendaisugi.pando import PandoStore
from opendaisugi.pando.mcp import build_pando_server

GOLDEN = Path(__file__).resolve().parents[1] / "clients" / "fixtures" / "pando" / "golden" / "out"


def test_server_has_three_read_tools_and_zooms():
    store = PandoStore.open(GOLDEN)
    mcp = build_pando_server(store)
    assert mcp.name == "pando"
    names = {t.name for t in asyncio.run(mcp.list_tools())}
    assert names == {"zoom", "date", "recall"}
    out = asyncio.run(mcp.call_tool("zoom", {"id": 0, "n": 1}))
    assert store.message(0).text in json.dumps(out, default=str)


def test_cli_reads_and_refuses_writes(tmp_path):
    runner = CliRunner()
    r = runner.invoke(app, ["pando", "append", "x", "--data-dir", str(tmp_path)])
    assert r.exit_code == 2
    r = runner.invoke(app, ["pando", "status", "--store", "all", "--data-dir", str(tmp_path)])
    assert r.exit_code == 2
    r = runner.invoke(app, ["pando", "status", "--store", "home", "--data-dir", str(tmp_path)])
    assert r.exit_code == 1
    base = tmp_path / "pando"
    base.mkdir()
    # A copy: a store that is a link is refused.
    shutil.copytree(GOLDEN, base / "work")
    r = runner.invoke(app, ["pando", "date", "0", "--data-dir", str(tmp_path)])
    assert r.exit_code == 0 and r.output.strip().endswith("Z")
    r = runner.invoke(app, ["pando", "stores", "--data-dir", str(tmp_path)])
    assert r.output.strip() == "work"


def test_a_running_server_sees_messages_logged_after_it_started(tmp_path):
    """Each call reads the store as it is now."""
    shutil.copytree(GOLDEN, tmp_path / "work")
    store = PandoStore.open(tmp_path / "work")
    n = store.count
    mcp = build_pando_server(store)
    row = {
        "i": n,
        "kind": "user",
        "text": "a line logged later",
        "size": 19,
        "date": "2099-01-01T00:00:00Z",
    }
    (tmp_path / "work" / "main" / "2099-01-01.jsonl").write_text(
        json.dumps(row) + "\n", encoding="utf-8"
    )
    out = asyncio.run(mcp.call_tool("zoom", {"id": n, "n": 1}))
    assert "a line logged later" in json.dumps(out, default=str)


def test_a_marked_process_reads_nothing_from_the_python_reader(tmp_path, monkeypatch):
    # Round 8b: the Python reader does not scope to a branch, so a worker or a
    # process in a branch gets nothing, never the whole store.
    base = tmp_path / "pando"
    base.mkdir()
    shutil.copytree(GOLDEN, base / "work")
    runner = CliRunner()
    for var, val in (("DAISUGI_PANDO_BRANCH", "trellis"), ("DAISUGI_PANDO_SKIP", "1")):
        monkeypatch.setenv(var, val)
        r = runner.invoke(app, ["pando", "zoom", "0", "1", "--data-dir", str(tmp_path)])
        assert r.exit_code == 1 and "branch" in r.output
        monkeypatch.delenv(var)
    r = runner.invoke(app, ["pando", "zoom", "0", "1", "--data-dir", str(tmp_path)])
    assert r.exit_code == 0


def test_the_closed_server_lists_no_tool(monkeypatch):
    from opendaisugi.pando import mcp as pm

    seen = []

    class Fake:
        def __init__(self, name):
            seen.append(name)

        def run(self):
            seen.append("run")

    import mcp.server.fastmcp as fm

    monkeypatch.setattr(fm, "FastMCP", Fake)
    pm.serve_closed()
    assert seen == ["pando", "run"]
