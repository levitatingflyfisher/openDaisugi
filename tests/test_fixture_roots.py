"""A case's fixture text does not depend on the length of the scratch path.

The gate cuts a record's detail and verdict clause at 200 characters, and
YAML folds a long line at a space. Both happen after the path is in the
text, so a longer scratch directory used to move the cut or the fold and
leave a machine path in a fixture (GR-R-3). Every case root now has one
length, and a root the gate cut short is written as {ROOT-CUT}.
"""

import json
import shutil
import sys
import tempfile
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "clients"))

from fixture_paths import LEAK, ROOT_LEN, fixed_root  # noqa: E402
from gate_cases import DETAIL_MAX, FIXTURE_DIR, _norm_value, python_gate_cmd, run_case  # noqa: E402

DOTDOT = "graft delegate call dotdot out"


def test_fixed_root_has_one_length():
    a = fixed_root(Path("/s"), "g/0001")
    b = fixed_root(Path("/mnt/a/longer/scratch/dir"), "c/p0001")
    assert len(str(a)) == len(str(b)) == ROOT_LEN
    assert str(a).startswith("/s/g/0001_")


def test_fixed_root_refuses_a_path_too_long():
    with pytest.raises(SystemExit, match="Set the suite's DAISUGI_"):
        fixed_root(Path("/" + "x" * ROOT_LEN), "g/0001")


def test_a_root_cut_short_is_written_as_a_marker():
    short = "/home/user/s/" + "_" * (ROOT_LEN - 13)
    long = "/mnt/a/much/longer/scratch/dir/" + "_" * (ROOT_LEN - 31)
    seen = set()
    for root in (short, long):
        text = f"verdict=deny clause=read {'x' * 40} '{root}/etc/passwd' not in ['{root}/work/**']"
        text = text[:DETAIL_MAX]
        got = _norm_value(text, root)
        assert not LEAK.search(got)
        seen.add(got)
    assert len(seen) == 1
    assert next(iter(seen)).endswith("{ROOT-CUT}")


def test_a_string_not_cut_keeps_its_ending():
    root = "/home/user/s/" + "_" * (ROOT_LEN - 13)
    assert _norm_value("ls /h", root) == "ls /h"
    assert _norm_value("x" * (DETAIL_MAX - 1) + "/", root) == "x" * (DETAIL_MAX - 1) + "/"


def test_dotdot_case_matches_its_fixture_under_a_long_scratch_path():
    """The oracle, run from a short and from a long scratch directory,
    writes the committed fixture for the case that once failed only from a
    long one."""
    case = next(
        json.loads(ln)
        for ln in (FIXTURE_DIR / "cases.jsonl").read_text(encoding="utf-8").splitlines()
        if ln.strip() and json.loads(ln)["name"] == DOTDOT
    )
    top = Path(tempfile.mkdtemp(prefix="fr"))
    try:
        room = ROOT_LEN - len(str(top)) - len("/g/0000") - 2
        if room < 8:
            pytest.skip("the temp directory is too long for a long scratch path")
        for base in (top / "s", top / ("L" * room)):
            got, _ = run_case(case, python_gate_cmd(), fixed_root(base, "g/0000"))
            assert not LEAK.search(json.dumps(got))
            for field in ("exit", "audit", "tree", "coppice", "stderr"):
                assert got[field] == case["expect"][field], (base, field)
    finally:
        shutil.rmtree(top, ignore_errors=True)
