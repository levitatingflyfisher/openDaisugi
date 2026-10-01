"""The rail clock mask of clients/coppice_local.py: an age is masked, and so
is the stale-timestamp age (ts 1) that the rail cuts to five digits and an
ellipsis, with the tail it cuts. A normal value is left alone."""

from __future__ import annotations

import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT))

from clients import coppice_local as cl  # noqa: E402


def _mask(row: str) -> str:
    return cl.mask_clocks({"rows": [row]})["rows"][0]


def test_a_stale_timestamp_age_and_its_cut_tail_are_masked():
    row = "  › 1 foreman  claude  idle  497954… ×┃  claude"
    assert _mask(row) == "  › 1 foreman  claude  idle  {AGE}  {CUT} ×┃  claude"


def test_a_normal_age_and_a_stale_one_mask_to_the_same_row():
    stale = "  › 1 foreman  claude  idle  497954… ×┃  claude"
    normal = "  › 1 foreman  claude  idle  3s  some long tail cut short here… ×┃  claude"
    assert _mask(stale) == _mask(normal)


def test_a_short_number_that_is_not_an_age_cell_is_not_masked():
    row = "  › 1 foreman  claude  idle  1234… ×┃  claude"
    assert "{AGE}" not in _mask(row)
