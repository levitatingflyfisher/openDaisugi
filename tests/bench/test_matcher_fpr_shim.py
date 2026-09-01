"""scripts/matcher_fpr.py is the command the ADRs cite for the shipped
thresholds. It must keep working end to end on a journal, not only accept
the flag, and it must measure through the bench so the two cannot drift."""

import sqlite3
import subprocess
import sys

from opendaisugi.bench.options import repo_root

SCRIPT = "scripts/matcher_fpr.py"


def _run(*args: str) -> subprocess.CompletedProcess:
    root = repo_root()
    assert root is not None
    return subprocess.run(
        [sys.executable, str(root / SCRIPT), *args],
        cwd=root,
        capture_output=True,
        text=True,
        timeout=180,
        env={**__import__("os").environ, "HF_HUB_OFFLINE": "1"},
        check=False,
    )


def test_a_missing_journal_says_so_and_exits_clean(tmp_path):
    proc = _run("--journal", str(tmp_path / "nope.db"))
    assert proc.returncode == 0, proc.stderr[-500:]
    assert "no journal at" in proc.stdout


def test_the_shim_scores_the_lexical_floor_on_a_real_journal_file(tmp_path):
    db = tmp_path / "index.db"
    con = sqlite3.connect(str(db))
    con.execute("CREATE TABLE traces (id INTEGER PRIMARY KEY, task TEXT)")
    con.executemany(
        "INSERT INTO traces (task) VALUES (?)",
        [
            ("add a pytest test for the shell decomposer",),
            ("bump the docker base image to bookworm",),
            ("summarise the last ten journal traces",),
            ("fix the flaky gateway streaming test",),
            ("add a pytest test for the shell decomposer",),
            ("x",),
        ],
    )
    con.commit()
    con.close()
    proc = _run("--journal", str(db))
    assert proc.returncode == 0, proc.stderr[-800:]
    assert "4 distinct usable tasks" in proc.stdout
    assert "6 distinct-task pairs" in proc.stdout
    assert "=== lexical ===" in proc.stdout
    assert "thr=0.25" in proc.stdout
    assert "closest to target FPR 0.0256" in proc.stdout
    assert "=== int8 === skipped:" in proc.stdout or "=== int8 ===\n" in proc.stdout


def test_the_shim_prints_the_module_thresholds_and_the_module_rates(tmp_path):
    """The shim must measure through the bench module. Its printed thresholds
    are the module's THRESHOLDS, and its lexical rate at each one equals what
    fpr_table computes in this process on the same tasks."""
    import re

    from opendaisugi.bench.layers.matcher import THRESHOLDS, build_embedder, fpr_table

    tasks = [
        "add a pytest test for the shell decomposer",
        "bump the docker base image to bookworm",
        "summarise the last ten journal traces",
        "fix the flaky gateway streaming test",
    ]
    db = tmp_path / "index.db"
    con = sqlite3.connect(str(db))
    con.execute("CREATE TABLE traces (id INTEGER PRIMARY KEY, task TEXT)")
    con.executemany("INSERT INTO traces (task) VALUES (?)", [(t,) for t in tasks])
    con.commit()
    con.close()
    proc = _run("--journal", str(db))
    assert proc.returncode == 0, proc.stderr[-800:]
    block = proc.stdout.split("=== lexical ===", 1)[1].split("===", 1)[0]
    printed = [
        (float(m.group(1)), float(m.group(2)))
        for m in re.finditer(r"thr=(\d+\.\d+)\s+fpr=(\d+\.\d+)", block)
    ]
    expected = fpr_table(build_embedder("lexical").encode, sorted(tasks), THRESHOLDS)
    assert [t for t, _ in printed] == list(THRESHOLDS)
    assert [round(f, 4) for _, f in printed] == [round(f, 4) for _, f in expected]
    assert "->" not in proc.stdout
