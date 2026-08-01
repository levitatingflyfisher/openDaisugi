"""Tests for the module-wiring view (`daisugi modules`)."""

import json

from opendaisugi.modules import (
    ACTIVE,
    detect_stages,
    render_wiring,
    wiring_json,
)


def test_detect_stages_covers_the_pipeline(tmp_path):
    stages = detect_stages(tmp_path)
    keys = {s.key for s in stages}
    # The load-bearing stages of the pipeline must all appear.
    for k in ("harness", "gate", "verifier", "matcher", "router", "distill", "stores"):
        assert k in keys, f"stage {k} missing"


def test_every_stage_names_at_least_one_module(tmp_path):
    for s in detect_stages(tmp_path):
        assert s.modules, f"stage {s.key} has no modules"
        assert all(m.name for m in s.modules)


def test_verifier_is_single_impl_today_not_falsely_swappable(tmp_path):
    # Honesty check: only the python verifier is runtime today, so the verifier
    # stage must NOT advertise itself as a live swap-point.
    verifier = next(s for s in detect_stages(tmp_path) if s.key == "verifier")
    active = [m for m in verifier.modules if m.state == ACTIVE]
    assert [m.name for m in active] == ["python (in-process)"]
    assert not verifier.swappable


def test_render_is_ascii_with_legend_and_flow(tmp_path):
    art = render_wiring(tmp_path, plain=False)
    assert "module wiring" in art
    assert "legend:" in art
    assert "● active" in art and "○ available" in art and "· possible" in art
    # the pipeline flows top to bottom
    assert "a task from your agent" in art
    assert "verified action runs" in art
    # a swap-point count line is present
    assert "planned (not wired yet)" in art


def test_json_output_is_valid_and_structured(tmp_path):
    data = json.loads(wiring_json(tmp_path))
    assert isinstance(data, list) and data
    assert {"key", "title", "role", "modules"} <= set(data[0])


def test_map_marks_live_cfg_planned(tmp_path):
    from opendaisugi.modules import render_wiring

    art = render_wiring(tmp_path, plain=False)
    assert "[live]" in art and "[cfg]" in art and "[planned]" in art
    for line in art.splitlines():
        if line.startswith("┌─ shell decomposition"):
            assert "[live]" in line  # the running code reads it now
        if line.startswith("┌─ verifier"):
            assert "[planned]" in line  # nothing dispatches to it yet
        if line.startswith("┌─ gate"):
            assert "[cfg]" in line  # a real choice, needs reinstall
