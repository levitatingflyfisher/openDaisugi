"""Cold-start budgets (roadmap 3d): scripts/speed.py against docs/reference/speed-budgets.json.

Two kinds of test. The logic tests use fake numbers and cannot flake. The
live tests start real processes and compare the MEDIAN of several runs with
budget x factor; the factor is 3 by default (a slow shared runner), and
SPEED_STRICT=1 makes it 1 for a quiet local box. SPEED_FACTOR=n sets it
directly. The Go and Rust live tests run only when their binaries exist in
SPEED_BIN_DIR (build them with scripts/speed-build.sh); the Python ones
always run.
"""

from __future__ import annotations

import importlib.util
import json
import os
import sys
from pathlib import Path

import pytest

REPO = Path(__file__).resolve().parent.parent
_spec = importlib.util.spec_from_file_location("speed", REPO / "scripts" / "speed.py")
speed = importlib.util.module_from_spec(_spec)
sys.modules["speed"] = speed
_spec.loader.exec_module(speed)

BIN_DIR = Path(os.environ.get("SPEED_BIN_DIR", speed.DEFAULT_BIN_DIR))
RUNS = 5


def budgets() -> dict:
    return speed.load_budgets()


def factor() -> float:
    if os.environ.get("SPEED_FACTOR"):
        return float(os.environ["SPEED_FACTOR"])
    if os.environ.get("SPEED_STRICT") == "1":
        return 1.0
    return float(budgets()["ci_factor"])


def result(key: str, *runs: float, skipped: str = "", error: str = "") -> "speed.Result":
    return speed.Result(key, list(runs), skipped=skipped, error=error)


# --- the numbers ------------------------------------------------------------


def test_p95_is_nearest_rank():
    assert speed.p95([10.0]) == 10.0
    assert speed.p95([float(i) for i in range(1, 21)]) == 19.0  # 20 runs: rank 19
    assert speed.p95([float(i) for i in range(1, 6)]) == 5.0  # 5 runs: the max


def test_median_is_the_middle_run():
    assert result("a", 9.0, 1.0, 5.0, 100.0, 4.0).median_ms == 5.0


def test_budget_rule_is_twice_the_median_or_plus_five_ms():
    assert speed.suggest_budget(2.0) == 7  # the +5 ms floor: 7
    assert speed.suggest_budget(40.0) == 80  # twice
    assert speed.suggest_budget(350.0) == 700
    assert speed.suggest_budget(451.8) == 925  # 903.6, to a 25 ms step


# --- the budget file --------------------------------------------------------


def test_every_surface_has_a_budget_and_no_budget_is_stale():
    b = budgets()
    assert speed.unbudgeted(b) == []
    assert set(b["budgets_ms"]) == {s.key for s in speed.SURFACES}


def test_budgets_are_positive_whole_milliseconds():
    b = budgets()
    assert b["ci_factor"] >= 2
    assert all(isinstance(v, int) and v > 0 for v in b["budgets_ms"].values())


def test_relative_rules_name_real_surfaces():
    keys = {s.key for s in speed.SURFACES}
    rules = budgets()["relative"]
    assert rules, "the gate speed-up over Python is a rule worth keeping"
    for rule in rules:
        assert rule["fast"] in keys and rule["slow"] in keys
        assert 0 < rule["max_ratio"] < 1


def test_native_gate_budgets_sit_far_below_python():
    b = budgets()["budgets_ms"]
    py = b["daisugi-gate/py/gate-hook"]
    assert b["daisugi-gate/go/gate-hook"] * 4 <= py
    assert b["daisugi-gate/rs/gate-hook"] * 4 <= py


# --- the check --------------------------------------------------------------


def test_check_passes_within_budget_and_fails_over():
    b = {"budgets_ms": {"t/go/c": 10}, "relative": []}
    assert speed.check([result("t/go/c", 9.0, 10.0, 8.0)], b, 1.0) == []
    over = speed.check([result("t/go/c", 11.0, 12.0, 13.0)], b, 1.0)
    assert len(over) == 1 and "t/go/c" in over[0]


def test_check_factor_loosens_the_limit():
    b = {"budgets_ms": {"t/go/c": 10}, "relative": []}
    slow = [result("t/go/c", 25.0, 26.0, 27.0)]
    assert speed.check(slow, b, 1.0)
    assert speed.check(slow, b, 3.0) == []


def test_check_judges_the_median_not_one_slow_run():
    b = {"budgets_ms": {"t/go/c": 10}, "relative": []}
    assert speed.check([result("t/go/c", 5.0, 6.0, 500.0, 7.0, 6.0)], b, 1.0) == []


def test_check_skips_a_surface_with_no_binary_but_reports_errors():
    b = {"budgets_ms": {"t/go/c": 10}, "relative": []}
    assert speed.check([result("t/go/c", skipped="not built")], b, 1.0) == []
    assert speed.check([result("t/go/c", error="exit 1")], b, 1.0) == ["t/go/c: exit 1"]


def test_relative_rule_compares_two_medians():
    b = {
        "budgets_ms": {},
        "relative": [{"fast": "g/go/h", "slow": "g/py/h", "max_ratio": 0.5}],
    }
    py = result("g/py/h", 300.0, 310.0, 305.0)
    assert speed.check([result("g/go/h", 20.0, 22.0, 21.0), py], b, 1.0) == []
    assert speed.check([result("g/go/h", 200.0, 220.0, 210.0), py], b, 1.0)


# A fake gate with the real shapes: allow is {"continue": true} on stdout,
# deny is exit 2 with a DENIED line on stderr.
FAKE_GATE = """#!/bin/sh
in=$(cat)
case "$in" in
  *"rm -rf"*) echo "openDaisugi gate: DENIED - fake" >&2; exit 2 ;;
esac
echo '{"continue": true}'
"""


def fake_bin(tmp_path: Path, name: str, body: str) -> None:
    f = tmp_path / name
    f.write_text(body)
    f.chmod(0o755)


GATE_GO = [x for x in speed.SURFACES if x.key == "daisugi-gate/go/gate-hook"]


def test_a_gate_that_denies_the_fixed_payload_is_an_error_not_a_fast_run(tmp_path):
    fake_bin(
        tmp_path,
        "daisugi-gate-go",
        "#!/bin/sh\ncat >/dev/null\necho 'openDaisugi gate: DENIED - x' >&2\nexit 2\n",
    )
    out = speed.measure(GATE_GO, 2, tmp_path, tmp_path / "scratch")
    assert out[0].error and not out[0].runs_ms


def test_a_gate_that_fails_open_is_an_error(tmp_path):
    # Allows everything, even the deny probe: the envelope did not load.
    fake_bin(
        tmp_path, "daisugi-gate-go", "#!/bin/sh\ncat >/dev/null\necho '{\"continue\": true}'\n"
    )
    out = speed.measure(GATE_GO, 2, tmp_path, tmp_path / "scratch")
    assert out[0].error.startswith("the deny probe was not denied")
    assert out[0].runs_ms == []


def test_a_gate_with_the_real_shapes_is_timed(tmp_path):
    fake_bin(tmp_path, "daisugi-gate-go", FAKE_GATE)
    out = speed.measure(GATE_GO, 3, tmp_path, tmp_path / "scratch")
    assert out[0].error == "" and len(out[0].runs_ms) == 3


def test_the_deny_probe_really_is_denied_by_the_python_gate(tmp_path):
    s = [x for x in speed.SURFACES if x.key == "daisugi-gate/py/gate-hook"]
    out = speed.measure(s, 1, tmp_path, tmp_path / "scratch")
    assert out[0].error == "" and len(out[0].runs_ms) == 1


def test_runs_are_interleaved(tmp_path):
    log = tmp_path / "order.txt"
    for name in ("coppice-go", "coppice-rs"):
        fake_bin(tmp_path, name, f"#!/bin/sh\necho {name} >> {log}\n")
    s = [x for x in speed.SURFACES if x.key in ("coppice/go/version", "coppice/rs/version")]
    out = speed.measure(s, 3, tmp_path, tmp_path / "scratch")
    assert all(len(r.runs_ms) == 3 for r in out)
    assert log.read_text().split() == ["coppice-go", "coppice-rs"] * 4  # warm-up + 3


SPRIG_HELP = [x for x in speed.SURFACES if x.key == "sprig/rs/help"]


def test_sprig_help_exit_2_counts_only_with_help_text(tmp_path):
    fake_bin(tmp_path, "sprig-rs", "#!/bin/sh\necho 'Usage of sprig:' >&2\nexit 2\n")
    ok = speed.measure(SPRIG_HELP, 2, tmp_path, tmp_path / "scratch")
    assert ok[0].error == "" and len(ok[0].runs_ms) == 2
    fake_bin(tmp_path, "sprig-rs", "#!/bin/sh\necho 'boom' >&2\nexit 2\n")
    bad = speed.measure(SPRIG_HELP, 2, tmp_path, tmp_path / "scratch")
    assert "without help text" in bad[0].error


def test_exit_2_is_an_error_where_it_is_not_allowed(tmp_path):
    fake_bin(tmp_path, "coppice-go", "#!/bin/sh\necho 'Usage: x' >&2\nexit 2\n")
    s = [x for x in speed.SURFACES if x.key == "coppice/go/help"]
    assert speed.measure(s, 2, tmp_path, tmp_path / "scratch")[0].error.startswith("exit 2")


def test_is_built_needs_an_executable_file(tmp_path):
    f = tmp_path / "x"
    f.write_text("")
    assert not speed.is_built(f)
    f.chmod(0o755)
    assert speed.is_built(f)
    assert not speed.is_built(tmp_path)


def test_measure_uses_a_scratch_home_and_cleans_up(tmp_path):
    fake = tmp_path / "sprig-go"
    probe = tmp_path / "env.txt"
    fake.write_text(f"#!/bin/sh\nenv > {probe}\n")
    fake.chmod(0o755)
    s = [x for x in speed.SURFACES if x.key == "sprig/go/version"]
    scratch = tmp_path / "scratch"
    out = speed.measure(s, 2, tmp_path, scratch)
    assert out[0].error == "" and len(out[0].runs_ms) == 2
    env = dict(line.split("=", 1) for line in probe.read_text().splitlines() if "=" in line)
    for var in ("HOME", "XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"):
        assert env[var].startswith(str(scratch)), var
    assert list(scratch.iterdir()) == []  # the world is removed


def test_json_output_is_machine_readable():
    doc = speed.as_json([result("a/b/c", 1.0, 2.0, 3.0), result("d/e/f", skipped="x")], 3)
    json.dumps(doc)
    assert doc["results"]["a/b/c"]["median_ms"] == 2.0
    assert doc["results"]["d/e/f"] == {"skipped": "x"}


# --- live: real processes ---------------------------------------------------


def in_ci() -> bool:
    """On a CI runner the absolute native budgets are not checked: the runner
    is slower and noisier than the box they were measured on. The relative
    rules still are. SPEED_STRICT=1 turns the absolute checks back on."""
    return bool(os.environ.get("CI")) and os.environ.get("SPEED_STRICT") != "1"


def measure_live(keys: list[str], tmp_path: Path) -> list["speed.Result"]:
    surfaces = [s for s in speed.SURFACES if s.key in keys]
    assert len(surfaces) == len(keys)
    return speed.measure(surfaces, RUNS, BIN_DIR, tmp_path / "scratch")


def assert_within_budget(results: list["speed.Result"]) -> None:
    assert results and not any(r.skipped for r in results)  # nothing measured is no pass
    assert not [r.error for r in results if r.error]
    assert speed.check(results, budgets(), factor()) == []


def test_python_cold_start_within_budget(tmp_path):
    keys = [
        "daisugi/py/version",
        "daisugi-gate/py/gate-hook",
        "daisugi-gate/py-client/gate-hook",
    ]
    assert_within_budget(measure_live(keys, tmp_path))


def native_built(*names: str) -> bool:
    return all(speed.is_built(BIN_DIR / n) for n in names)


def binary_name(key: str) -> str:
    tool, impl, _ = key.split("/")
    return f"{tool}-{impl}"


@pytest.mark.parametrize("rule", budgets()["relative"], ids=lambda r: r["fast"])
def test_relative_rule_holds(rule, tmp_path):
    # Runs in CI too. It compares two medians from one interleaved run, so
    # machine speed cancels. A missing binary skips the rule; it never fails.
    if not native_built(binary_name(rule["fast"])):
        pytest.skip(f"{binary_name(rule['fast'])} is not in {BIN_DIR}")
    res = measure_live([rule["fast"], rule["slow"]], tmp_path)
    assert not [r.error for r in res if r.error]
    assert speed.check(res, {"budgets_ms": {}, "relative": [rule]}, 1.0) == []


@pytest.mark.skipif(
    in_ci(), reason="absolute native budgets are a local check (SPEED_STRICT=1 in CI)"
)
def test_every_built_native_command_within_budget(tmp_path):
    keys = [
        s.key
        for s in speed.SURFACES
        if s.impl in ("go", "rs") and native_built(f"{s.tool}-{s.impl}")
    ]
    if not keys:
        pytest.skip(f"no native binaries in {BIN_DIR}")
    assert_within_budget(measure_live(keys, tmp_path))
