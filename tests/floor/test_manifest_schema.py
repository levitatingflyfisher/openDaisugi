"""The recorded Herdr manifest schema is read off the Go source, not guessed.

Two things must stay true as the Go side evolves. Every key, gate and region a
vendored manifest actually uses must be one opendaisugi.floor.manifest_schema
names. And every constant that module records must match the Go source it was
read from, not just itself. A sha256 pin over the vendored manifests and the
Go README catches a byte the Go side changed even when nobody edits the
schema module by hand; scripts/floor_manifest_schema.py is the drift check
that compares the tree against that pin.
"""

from __future__ import annotations

import importlib.util
import json
import re
import subprocess

import pytest

from opendaisugi.floor import manifest_schema as schema
from tests.floor import hostfacts

DETECT_DIR = hostfacts.REPO_ROOT / "harness" / "coppice" / "internal" / "detect"
MANIFEST_GO = DETECT_DIR / "manifest.go"
SCRIPT = hostfacts.REPO_ROOT / "scripts" / "floor_manifest_schema.py"
PIN = hostfacts.REPO_ROOT / "src" / "opendaisugi" / "floor" / "manifest_sources_pin.json"


def _load_script_module():
    spec = importlib.util.spec_from_file_location("floor_manifest_schema_script", SCRIPT)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def _struct_toml_tags(go_text: str, struct_name: str) -> set[str]:
    match = re.search(rf"type {struct_name} struct \{{(.*?)\n\}}", go_text, re.S)
    assert match, f"type {struct_name} struct not found in manifest.go"
    return set(re.findall(r'toml:"([^"]+)"', match.group(1)))


def _top_level_const_int(go_text: str, name: str) -> int:
    match = re.search(rf"^const {name} = (\d+)$", go_text, re.M)
    assert match, f"top-level const {name} not found in manifest.go"
    return int(match.group(1))


def _const_block(go_text: str) -> str:
    match = re.search(r"const \(\n(.*?)\n\)\n", go_text, re.S)
    assert match, "const (...) block not found in manifest.go"
    return match.group(1)


def _const_int(const_block: str, name: str) -> int:
    # Anchored to one line inside the const block, not searched across the
    # whole file: a bare substring search would also match name as a prefix
    # of a longer constant, or match a comparison site that happens to carry
    # an "=" nearby, instead of the declaration itself.
    match = re.search(rf"^\s*{name}\s*=\s*(\d+)\s*$", const_block, re.M)
    assert match, f"const {name} not found in the const block"
    return int(match.group(1))


def _fixed_regions(go_text: str) -> set[str]:
    match = re.search(r"var fixedRegions = map\[string\]bool\{(.*?)\n\}", go_text, re.S)
    assert match, "fixedRegions map not found in manifest.go"
    return set(re.findall(r'"([^"]+)":\s*true', match.group(1)))


def _parameterized_region_names(go_text: str) -> set[str]:
    names = set(re.findall(r'regionCount\(s, "([^"]+)"\)', go_text))
    top = re.search(r'CutPrefix\(spec, "(top_non_empty_lines)"\)', go_text)
    assert top, "top_non_empty_lines prefix not found in manifest.go"
    names.add(top.group(1))
    return names


def _region_known(region: str) -> bool:
    if region in schema.FIXED_REGIONS:
        return True
    for prefix in schema.PARAMETERIZED_REGIONS:
        if region.startswith(prefix + "(") and region.endswith(")"):
            digits = region[len(prefix) + 1 : -1]
            if digits.isdigit():
                return True
    return False


def _walk_nested_gates(gate: dict, manifest_name: str, rule_id: str) -> None:
    for combinator in ("all", "any", "not"):
        for nested in gate.get(combinator, []):
            for key in nested:
                assert key in schema.MATCHER_KEYS, (
                    f"{manifest_name} rule {rule_id} uses unrecorded gate key {key!r}"
                )
            _walk_nested_gates(nested, manifest_name, rule_id)


# --- the schema module resolves the same directory hostfacts does --------


def test_manifest_dir_agrees_with_hostfacts():
    assert schema.MANIFEST_DIR == hostfacts.MANIFEST_DIR


def test_detect_readme_lives_beside_the_manifests():
    assert schema.DETECT_README.parent == schema.MANIFEST_DIR.parent


# --- the schema module matches the Go source, not just itself ------------


def test_the_schema_module_matches_the_go_struct_tags():
    reason = hostfacts.skip_reason("vendored_manifests")
    if reason:
        pytest.skip(reason)
    go_text = MANIFEST_GO.read_text(encoding="utf-8")
    assert _struct_toml_tags(go_text, "Manifest") == schema.MANIFEST_KEYS
    assert _struct_toml_tags(go_text, "Rule") == schema.RULE_KEYS
    assert _struct_toml_tags(go_text, "Gate") == schema.MATCHER_KEYS


def test_the_schema_module_matches_the_go_regions():
    reason = hostfacts.skip_reason("vendored_manifests")
    if reason:
        pytest.skip(reason)
    go_text = MANIFEST_GO.read_text(encoding="utf-8")
    assert _fixed_regions(go_text) == schema.FIXED_REGIONS
    assert _parameterized_region_names(go_text) == schema.PARAMETERIZED_REGIONS
    assert len(schema.FIXED_REGIONS) + len(schema.PARAMETERIZED_REGIONS) == 15


def test_the_schema_module_matches_the_go_limits():
    reason = hostfacts.skip_reason("vendored_manifests")
    if reason:
        pytest.skip(reason)
    go_text = MANIFEST_GO.read_text(encoding="utf-8")
    assert _top_level_const_int(go_text, "EngineVersion") == schema.ENGINE_VERSION
    block = _const_block(go_text)
    assert _const_int(block, "maxRules") == schema.MAX_RULES
    assert _const_int(block, "maxGateDepth") == schema.MAX_GATE_DEPTH
    assert _const_int(block, "maxTotalGates") == schema.MAX_TOTAL_GATES
    assert _const_int(block, "maxMatchersPerGate") == schema.MAX_MATCHERS_PER_GATE
    assert _const_int(block, "maxTotalMatchers") == schema.MAX_TOTAL_MATCHERS
    assert _const_int(block, "maxMatcherChars") == schema.MAX_MATCHER_CHARS
    assert _const_int(block, "topRegionMinVer") == schema.TOP_NON_EMPTY_LINES_MIN_ENGINE_VERSION
    assert _const_int(block, "maxTopRegionLines") == schema.TOP_NON_EMPTY_LINES_MAX_N


# --- two places the README reads one way and the code does another -------


def test_the_readme_code_disagreements_are_recorded_not_dropped():
    assert schema.TOP_NON_EMPTY_LINES_ENGINE_CHECK_SKIPPED_WHEN_UNSET is True
    assert schema.BOTTOM_REGIONS_ALLOW_LEADING_ZERO is True
    assert schema.TOP_NON_EMPTY_LINES_ALLOWS_LEADING_ZERO is False


# --- every vendored manifest fits inside the recorded schema -------------


def test_every_vendored_manifest_uses_only_recorded_keys_gates_and_regions():
    reason = hostfacts.skip_reason("vendored_manifests")
    if reason:
        pytest.skip(reason)
    import tomllib

    files = sorted(schema.MANIFEST_DIR.glob("*.toml"))
    assert len(files) == 21, f"{len(files)} manifests on disk, want the 21 vendored"

    checked = 0
    for path in files:
        doc = tomllib.loads(path.read_text(encoding="utf-8"))
        for key in doc:
            assert key in schema.MANIFEST_KEYS, f"{path.name} uses unrecorded key {key!r}"
        for rule in doc.get("rules", []):
            for key in rule:
                assert key in schema.RULE_KEYS, (
                    f"{path.name} rule {rule.get('id')} uses unrecorded key {key!r}"
                )
            state = rule.get("state")
            if state is not None:
                assert state in schema.RULE_STATES, (
                    f"{path.name} rule {rule.get('id')} uses unrecorded state {state!r}"
                )
            region = rule.get("region", schema.DEFAULT_REGION)
            assert _region_known(region), (
                f"{path.name} rule {rule.get('id')} uses unrecorded region {region!r}"
            )
            _walk_nested_gates(rule, path.name, rule.get("id", "?"))
            checked += 1
    assert checked > 0, "no rules were checked, the manifest walk found nothing"


# --- the drift-check pin ---------------------------------------------------


def test_the_pin_is_committed():
    assert PIN.exists(), "run: uv run --no-sync python scripts/floor_manifest_schema.py"


def test_the_pin_names_every_vendored_manifest_and_the_readme():
    reason = hostfacts.skip_reason("vendored_manifests")
    if reason:
        pytest.skip(reason)
    pin = json.loads(PIN.read_text(encoding="utf-8"))
    names = {p.name for p in schema.MANIFEST_DIR.glob("*.toml")}
    assert set(pin["manifest_sha256"]) == names
    assert pin["readme_sha256"]
    assert pin["recorded_at"]


def test_the_pin_matches_the_tree():
    reason = hostfacts.skip_reason("vendored_manifests")
    if reason:
        pytest.skip(reason)
    proc = subprocess.run(
        ["uv", "run", "--no-sync", "python", str(SCRIPT), "--check"],
        capture_output=True,
        text=True,
        cwd=hostfacts.REPO_ROOT,
    )
    assert proc.returncode == 0, proc.stderr


def test_check_ignores_the_recorded_at_date(tmp_path, monkeypatch):
    """A date stamp is not drift. --check must compare the hashes, not the day."""
    module = _load_script_module()
    src = tmp_path / "manifests"
    src.mkdir()
    (src / "x.toml").write_text('id = "x"\n', encoding="utf-8")
    readme = tmp_path / "README.md"
    readme.write_text("placeholder\n", encoding="utf-8")
    pin = tmp_path / "pin.json"
    monkeypatch.setattr(module, "MANIFEST_DIR", src)
    monkeypatch.setattr(module, "DETECT_README", readme)
    monkeypatch.setattr(module, "PIN", pin)

    assert module.main([]) == 0
    stored = json.loads(pin.read_text())
    stored["recorded_at"] = "1999-01-01"
    pin.write_text(json.dumps(stored, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    assert module.main(["--check"]) == 0


def test_check_catches_a_real_change_after_the_pin_was_recorded(tmp_path, monkeypatch):
    module = _load_script_module()
    src = tmp_path / "manifests"
    src.mkdir()
    (src / "x.toml").write_text('id = "x"\n', encoding="utf-8")
    readme = tmp_path / "README.md"
    readme.write_text("placeholder\n", encoding="utf-8")
    pin = tmp_path / "pin.json"
    monkeypatch.setattr(module, "MANIFEST_DIR", src)
    monkeypatch.setattr(module, "DETECT_README", readme)
    monkeypatch.setattr(module, "PIN", pin)

    assert module.main([]) == 0
    assert module.main(["--check"]) == 0

    (src / "x.toml").write_text('id = "x"\nversion = "2"\n', encoding="utf-8")
    assert module.main(["--check"]) == 1


def test_main_fails_closed_when_the_manifest_dir_is_missing(tmp_path, monkeypatch):
    module = _load_script_module()
    monkeypatch.setattr(module, "MANIFEST_DIR", tmp_path / "nope")
    monkeypatch.setattr(module, "DETECT_README", tmp_path / "README.md")
    monkeypatch.setattr(module, "PIN", tmp_path / "pin.json")
    assert module.main([]) == 1
