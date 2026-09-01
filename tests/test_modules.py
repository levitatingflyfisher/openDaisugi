"""Tests for the module-wiring view (`daisugi modules`)."""

import json

from opendaisugi.modules import (
    ACTIVE,
    AVAILABLE,
    POSSIBLE,
    detect_stages,
    render_wiring,
    wiring_json,
)


def _write_hooks(home, *, stop=False, notification=False):
    """Materialize a minimal ~/.claude/settings.json with the given
    floor-report hooks present (or not) — mirrors what
    `_patch_claude_report_hooks` (install.py) actually writes."""
    claude_dir = home / ".claude"
    claude_dir.mkdir(parents=True, exist_ok=True)
    hooks = {}
    if stop:
        hooks["Stop"] = [
            {
                "hooks": [
                    {
                        "type": "command",
                        "command": "daisugi hook record --format claude --event stop",
                    }
                ]
            }
        ]
    if notification:
        hooks["Notification"] = [
            {
                "hooks": [
                    {
                        "type": "command",
                        "command": "daisugi hook record --format claude --event notification",
                    }
                ]
            }
        ]
    (claude_dir / "settings.json").write_text(json.dumps({"hooks": hooks}))


def test_detect_stages_covers_the_pipeline(tmp_path):
    stages = detect_stages(tmp_path)
    keys = {s.key for s in stages}
    # The load-bearing stages of the pipeline must all appear.
    for k in (
        "harness",
        "gate",
        "verifier",
        "matcher",
        "router",
        "distill",
        "stores",
        "voice_engine",
    ):
        assert k in keys, f"stage {k} missing"


def test_every_stage_names_at_least_one_module(tmp_path):
    for s in detect_stages(tmp_path):
        assert s.modules, f"stage {s.key} has no modules"
        assert all(m.name for m in s.modules)


# --- the voice engine stage: faster-whisper ACTIVE by selection + package,
# parakeet AVAILABLE by package presence alone ------------------------------


def test_voice_stage_marks_faster_whisper_active_when_selected_and_installed(tmp_path):
    # faster-whisper is installed in this dev/test env, and it is the config
    # default, so it is the running engine, not merely an installed option.
    stage = next(s for s in detect_stages(tmp_path) if s.key == "voice_engine")
    fw = {m.name: m for m in stage.modules}["faster-whisper"]
    assert fw.state == ACTIVE


def test_voice_stage_marks_faster_whisper_possible_when_the_package_is_missing(
    tmp_path, monkeypatch
):
    import sys

    monkeypatch.setitem(sys.modules, "faster_whisper", None)
    stage = next(s for s in detect_stages(tmp_path) if s.key == "voice_engine")
    fw = {m.name: m for m in stage.modules}["faster-whisper"]
    assert fw.state == POSSIBLE


def test_voice_stage_marks_parakeet_possible_when_sherpa_onnx_is_missing(tmp_path):
    # sherpa-onnx is not installed in this dev/test env.
    stage = next(s for s in detect_stages(tmp_path) if s.key == "voice_engine")
    parakeet = {m.name: m for m in stage.modules}["parakeet"]
    assert parakeet.state == POSSIBLE


def test_voice_stage_marks_parakeet_available_when_sherpa_onnx_is_importable(tmp_path, monkeypatch):
    import importlib.util

    real_find_spec = importlib.util.find_spec

    def _present(name, *a, **k):
        if name == "sherpa_onnx":
            return object()
        return real_find_spec(name, *a, **k)

    monkeypatch.setattr(importlib.util, "find_spec", _present)
    stage = next(s for s in detect_stages(tmp_path) if s.key == "voice_engine")
    parakeet = {m.name: m for m in stage.modules}["parakeet"]
    assert parakeet.state == AVAILABLE
    # Never ACTIVE from this stage alone: a present package says nothing
    # about whether the model directory itself is there too.
    assert parakeet.state != ACTIVE


def test_voice_stage_tag_flips_with_voice_engine_selection(tmp_path):
    from opendaisugi.config import Config, save_config

    save_config(Config(voice_engine="parakeet"), tmp_path / "config.yaml")
    stage = next(s for s in detect_stages(tmp_path) if s.key == "voice_engine")
    by_name = {m.name: m for m in stage.modules}
    # Selecting parakeet never promotes it past AVAILABLE without the
    # package (still missing here), and drops faster-whisper to AVAILABLE
    # even though its package is installed, since it is no longer selected.
    assert by_name["parakeet"].state == POSSIBLE
    assert by_name["faster-whisper"].state == AVAILABLE


def test_verifier_clients_are_only_active_after_a_successful_dispatch(tmp_path, monkeypatch):
    # A binary on disk is not proof it works, so presence alone is AVAILABLE.
    monkeypatch.setattr("opendaisugi.verifier_dispatch.last_dispatch", lambda root=None: {})
    verifier = next(s for s in detect_stages(tmp_path) if s.key == "verifier")
    active = [m.name for m in verifier.modules if m.state == ACTIVE]
    assert active == ["python (in-process)"]


def test_an_unbuilt_verifier_client_is_possible_with_its_build_command(tmp_path, monkeypatch):
    from opendaisugi.bench import options

    monkeypatch.setattr(options, "client_is_built", lambda spec: spec.probe is None)
    verifier = next(s for s in detect_stages(tmp_path) if s.key == "verifier")
    by_name = {m.name: m for m in verifier.modules}
    assert by_name["rust"].state == POSSIBLE
    assert "cargo build" in by_name["rust"].note


def test_a_client_that_answered_the_gate_under_this_data_dir_is_active(tmp_path, monkeypatch):
    """The map reads the dispatch record from the data dir it describes, not
    from the operator's home."""
    from opendaisugi.bench import options
    from opendaisugi.verifier_dispatch import _record_dispatch

    monkeypatch.setattr(options, "client_is_built", lambda spec: True)
    _record_dispatch(tmp_path / "gate", "go", True, "")
    verifier = next(s for s in detect_stages(tmp_path) if s.key == "verifier")
    by_name = {m.name: m for m in verifier.modules}
    assert by_name["go (mvdan)"].state == ACTIVE
    assert by_name["rust"].state == AVAILABLE


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
            assert "[live]" in line  # dispatched per verify call
        if line.startswith("┌─ gate"):
            assert "[cfg]" in line  # a real choice, needs reinstall


def test_floor_stage_reports_herdr_and_coppice_honestly(tmp_path):
    # home=/which= isolate this from whatever the real developer box has
    # actually installed (whole-branch review, minor 4) — this repo's own
    # workflow runs `daisugi install --gate --report herdr` for real, so a
    # bare detect_stages(tmp_path) would read the box's real
    # ~/.claude/settings.json instead of a clean fixture.
    stages = detect_stages(tmp_path, home=tmp_path / "home", which=lambda _: None)
    floor = next(s for s in stages if s.key == "floor_report")
    names = {m.name for m in floor.modules}
    assert {"herdr", "coppice", "none"} <= names
    coppice = next(m for m in floor.modules if m.name == "coppice")
    # This stage reports the report path, which nothing reads yet. Whether
    # the coppice binary is built or installed anywhere is the
    # floor_backend stage's own claim to make, not this one's.
    assert coppice.state == POSSIBLE
    assert "not wired yet" in coppice.note
    assert "server built" not in coppice.note


def test_floor_stage_shows_coppice_still_possible_once_the_preference_is_recorded(tmp_path):
    """Recording the coppice preference in floor_report must not render
    ACTIVE. This stage reports which hook writes state; it never checks a
    socket. The floor_backend stage checks the socket instead."""
    from opendaisugi.config import load_config, save_config

    cfg_path = tmp_path / "config.yaml"
    save_config(load_config(cfg_path).model_copy(update={"floor_report": "coppice"}), cfg_path)
    # home=/which= isolate this the same way (whole-branch review, minor 4).
    stages = detect_stages(tmp_path, home=tmp_path / "home", which=lambda _: None)
    floor = next(s for s in stages if s.key == "floor_report")
    coppice = next(m for m in floor.modules if m.name == "coppice")
    assert coppice.state == POSSIBLE
    # Recording the preference must not change the note: this stage
    # reports the report path, not the config field.
    assert "not wired yet" in coppice.note
    assert "server built" not in coppice.note


def test_floor_herdr_active_when_both_hooks_installed(tmp_path):
    home = tmp_path / "home"
    _write_hooks(home, stop=True, notification=True)
    stages = detect_stages(tmp_path, home=home)
    floor = next(s for s in stages if s.key == "floor_report")
    herdr = next(m for m in floor.modules if m.name == "herdr")
    assert herdr.state == ACTIVE
    # Whole-branch review, minor 8: "hooks installed" alone overstated it —
    # the Herdr CLI contract this module shells out to (_state_report.py's
    # docstring) is UNVERIFIED against a real Herdr process.
    assert herdr.note == "hooks installed; Herdr CLI contract unverified"


def test_floor_herdr_available_when_binary_present_but_no_hooks(tmp_path):
    home = tmp_path / "home"
    home.mkdir()
    stages = detect_stages(tmp_path, home=home, which=lambda name: "/usr/bin/herdr")
    floor = next(s for s in stages if s.key == "floor_report")
    herdr = next(m for m in floor.modules if m.name == "herdr")
    assert herdr.state == AVAILABLE


def test_floor_herdr_possible_when_neither_hooks_nor_binary(tmp_path):
    home = tmp_path / "home"
    home.mkdir()
    stages = detect_stages(tmp_path, home=home, which=lambda name: None)
    floor = next(s for s in stages if s.key == "floor_report")
    herdr = next(m for m in floor.modules if m.name == "herdr")
    assert herdr.state == POSSIBLE


def test_floor_herdr_requires_both_hooks_not_just_one(tmp_path):
    """Finding 1(c): ACTIVE needs Stop AND Notification, not either alone;
    the detail says plainly that only one of the two is wired."""
    home = tmp_path / "home"
    _write_hooks(home, stop=True, notification=False)
    stages = detect_stages(tmp_path, home=home, which=lambda name: None)
    floor = next(s for s in stages if s.key == "floor_report")
    herdr = next(m for m in floor.modules if m.name == "herdr")
    assert herdr.state != ACTIVE
    assert "one of two hooks installed" in herdr.note


def test_floor_none_row_active_only_when_no_herdr_hooks(tmp_path):
    """Finding 2: the coppice preference no longer flips the 'none' row —
    it is ACTIVE purely on the absence of herdr hooks."""
    from opendaisugi.config import load_config, save_config

    cfg_path = tmp_path / "config.yaml"
    save_config(load_config(cfg_path).model_copy(update={"floor_report": "coppice"}), cfg_path)
    home = tmp_path / "home"
    home.mkdir()
    stages = detect_stages(tmp_path, home=home, which=lambda name: None)
    floor = next(s for s in stages if s.key == "floor_report")
    none_row = next(m for m in floor.modules if m.name == "none")
    assert none_row.state == ACTIVE  # coppice recorded, but nothing actually listens

    _write_hooks(home, stop=True, notification=True)
    stages = detect_stages(tmp_path, home=home, which=lambda name: None)
    floor = next(s for s in stages if s.key == "floor_report")
    none_row = next(m for m in floor.modules if m.name == "none")
    assert none_row.state != ACTIVE  # herdr is actually listening now


def test_render_wiring_does_not_truncate_any_stage_description(tmp_path):
    """Finding 1(d): the floor stage's role line was 67 chars against a
    62-char inner width, silently dropping 'done'. Guard every stage."""
    width = 66
    inner = width - 4
    # home=/which= isolate both calls below from the real developer box, so
    # neither reads the real ~/.claude/settings.json or PATH.
    stages = detect_stages(tmp_path, home=tmp_path / "home", which=lambda _: None)
    for st in stages:
        assert len(st.role) <= inner, (
            f"{st.key}: role is {len(st.role)} chars, truncates at {inner}"
        )
    art = render_wiring(
        tmp_path, width=width, plain=True, home=tmp_path / "home", which=lambda _: None
    )
    lines = art.splitlines()
    floor_idx = next(i for i, line in enumerate(lines) if "floor report" in line)
    # the role line immediately follows the box header
    assert "done" in lines[floor_idx + 1]


def test_render_wiring_passes_home_and_which_through_to_detect_stages(tmp_path, monkeypatch):
    """render_wiring() used to call detect_stages(data_dir) with no home=/
    which= of its own, so a caller could not isolate render_wiring() from
    the real developer box the way detect_stages() itself already
    supports."""
    calls = []

    def _fake_detect_stages(data_dir, *, home=None, which=None):
        calls.append((data_dir, home, which))
        return []

    monkeypatch.setattr("opendaisugi.modules.detect_stages", _fake_detect_stages)
    sentinel_home = tmp_path / "home"

    def sentinel_which(_name):
        return None

    render_wiring(tmp_path, home=sentinel_home, which=sentinel_which, plain=True)
    assert calls == [(tmp_path, sentinel_home, sentinel_which)]


def test_wiring_json_passes_home_and_which_through_to_detect_stages(tmp_path, monkeypatch):
    """wiring_json() used to call detect_stages(data_dir) with no home=/
    which= of its own, so a caller could not isolate it from the real
    developer box the way render_wiring() already could."""
    calls = []

    def _fake_detect_stages(data_dir, *, home=None, which=None):
        calls.append((data_dir, home, which))
        return []

    monkeypatch.setattr("opendaisugi.modules.detect_stages", _fake_detect_stages)
    sentinel_home = tmp_path / "home"

    def sentinel_which(_name):
        return None

    wiring_json(tmp_path, home=sentinel_home, which=sentinel_which)
    assert calls == [(tmp_path, sentinel_home, sentinel_which)]


def test_coppice_socket_present_reads_the_given_env_not_the_real_one(tmp_path, monkeypatch):
    """`_coppice_socket_present` must isolate the same way
    `default_socket_path(env=...)` already does, so a test never reads the
    real XDG_RUNTIME_DIR."""
    import socket as socket_mod

    from opendaisugi.modules import _coppice_socket_present

    real_runtime = tmp_path / "real-runtime-must-be-ignored"
    real_runtime.mkdir()
    monkeypatch.setenv("XDG_RUNTIME_DIR", str(real_runtime))

    fixture_runtime = tmp_path / "fixture-runtime"
    (fixture_runtime / "coppice").mkdir(parents=True)
    sock_path = fixture_runtime / "coppice" / "server.sock"
    srv = socket_mod.socket(socket_mod.AF_UNIX, socket_mod.SOCK_STREAM)
    srv.bind(str(sock_path))
    try:
        assert _coppice_socket_present(env={"XDG_RUNTIME_DIR": str(fixture_runtime)}) is True
        # The real XDG_RUNTIME_DIR (monkeypatched above) has no socket in it,
        # and must never be read once env names a different one.
        assert _coppice_socket_present(env={}) is False
    finally:
        srv.close()


def test_detect_stages_floor_backend_check_uses_the_given_env_not_the_real_one(
    tmp_path, monkeypatch
):
    """`detect_stages(env=...)` must reach the coppice socket check the same
    way `home=`/`which=` already reach the rest of this function."""
    import socket as socket_mod

    real_runtime = tmp_path / "real-runtime-must-be-ignored"
    real_runtime.mkdir()
    monkeypatch.setenv("XDG_RUNTIME_DIR", str(real_runtime))

    home = tmp_path / "home"
    coppice_dir = home / ".opendaisugi" / "coppice"
    coppice_dir.mkdir(parents=True)
    sock_path = coppice_dir / "server.sock"
    srv = socket_mod.socket(socket_mod.AF_UNIX, socket_mod.SOCK_STREAM)
    srv.bind(str(sock_path))
    try:
        # env={} means no XDG_RUNTIME_DIR was seen, so the home-based path
        # applies. Without env threaded through, this would read the real
        # (monkeypatched) XDG_RUNTIME_DIR instead and never find the socket.
        stages = detect_stages(tmp_path, home=home, which=lambda _: None, env={})
        floor_backend = next(s for s in stages if s.key == "floor_backend")
        coppice = next(m for m in floor_backend.modules if m.name == "coppice")
        assert coppice.state == ACTIVE
    finally:
        srv.close()


def test_backend_stage_shows_the_recorded_remote_host(tmp_path):
    from opendaisugi.config import Config, save_config

    save_config(
        Config(
            llm_base_url="http://box:11434",
            llm_host_kind="ollama",
            llm_host_model="qwen3-coder:latest",
            llm_context_window=32768,
        ),
        tmp_path / "config.yaml",
    )
    backend = next(s for s in detect_stages(tmp_path) if s.key == "backend")
    host_module = next((m for m in backend.modules if "box:11434" in m.name), None)
    assert host_module is not None
    assert "ollama" in host_module.name
    assert "qwen3-coder:latest" in host_module.name
    assert "32k" in host_module.name
    # AVAILABLE, not ACTIVE: a recorded host is not a harness that routes
    # through it right now.
    assert host_module.state == AVAILABLE


def test_backend_stage_has_no_host_line_when_none_is_recorded(tmp_path):
    # A regression guard: the five fixed modules stay the only ones.
    backend = next(s for s in detect_stages(tmp_path) if s.key == "backend")
    assert len(backend.modules) == 5


def test_backend_stage_marks_auto_active_when_nothing_names_a_backend(tmp_path):
    def _which(name):
        return None

    backend = next(s for s in detect_stages(tmp_path, which=_which, env={}) if s.key == "backend")
    by_name = {m.name: m for m in backend.modules}
    assert by_name["auto"].state == "active"
    assert "litellm" in by_name["auto"].note
    assert all(m.state != "active" for n, m in by_name.items() if n != "auto")


def test_backend_stage_marks_the_file_choice_active(tmp_path):
    from opendaisugi.config import load_config, save_config

    save_config(
        load_config(tmp_path / "config.yaml").model_copy(update={"llm_backend": "ollama"}),
        tmp_path / "config.yaml",
    )
    backend = next(s for s in detect_stages(tmp_path, env={}) if s.key == "backend")
    by_name = {m.name: m for m in backend.modules}
    assert by_name["ollama"].state == "active"
    assert by_name["auto"].state != "active"


# --- pi: the gate extension is a real, installable adapter row ---------------


def test_pi_harness_row_is_active_when_extension_installed(tmp_path, monkeypatch):
    monkeypatch.setattr("pathlib.Path.home", lambda: tmp_path)
    ext = tmp_path / ".pi" / "agent" / "extensions" / "daisugi-gate"
    ext.mkdir(parents=True)
    (ext / "index.ts").write_text("// installed")
    stages = detect_stages(tmp_path)
    harness_stage = next(s for s in stages if s.key == "harness")
    pi_module = next(m for m in harness_stage.modules if m.name == "pi")
    assert pi_module.state == ACTIVE


def test_pi_harness_row_is_available_when_pi_present_but_not_installed(tmp_path, monkeypatch):
    monkeypatch.setattr("pathlib.Path.home", lambda: tmp_path)
    (tmp_path / ".pi").mkdir()
    stages = detect_stages(tmp_path)
    harness_stage = next(s for s in stages if s.key == "harness")
    pi_module = next(m for m in harness_stage.modules if m.name == "pi")
    assert pi_module.state == AVAILABLE


def test_pi_harness_row_is_possible_when_pi_not_detected(tmp_path, monkeypatch):
    monkeypatch.setattr("pathlib.Path.home", lambda: tmp_path)
    stages = detect_stages(tmp_path)
    harness_stage = next(s for s in stages if s.key == "harness")
    pi_module = next(m for m in harness_stage.modules if m.name == "pi")
    assert pi_module.state == POSSIBLE


def test_pi_rows_honor_the_home_parameter_over_path_home(tmp_path, monkeypatch):
    other = tmp_path / "other"
    ext = other / ".pi" / "agent" / "extensions" / "daisugi-gate"
    ext.mkdir(parents=True)
    (ext / "index.ts").write_text("// installed")
    monkeypatch.setattr("pathlib.Path.home", lambda: tmp_path)
    stages = detect_stages(tmp_path, home=other)
    harness_stage = next(s for s in stages if s.key == "harness")
    assert next(m for m in harness_stage.modules if m.name == "pi").state == ACTIVE


def test_gate_stage_names_pis_in_process_enforcement_class(tmp_path, monkeypatch):
    monkeypatch.setattr("pathlib.Path.home", lambda: tmp_path)
    ext = tmp_path / ".pi" / "agent" / "extensions" / "daisugi-gate"
    ext.mkdir(parents=True)
    (ext / "index.ts").write_text("// installed")
    stages = detect_stages(tmp_path)
    gate_stage = next(s for s in stages if s.key == "gate")
    pi_gate = next(m for m in gate_stage.modules if "pi" in m.name)
    assert pi_gate.state == ACTIVE
    assert "exit-2" in pi_gate.note or "fail-open" in pi_gate.note
