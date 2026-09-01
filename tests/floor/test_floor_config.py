"""The floor's config knob is LIVE, so it must actually reach disk.

``floor.backend`` is nested, and ``Config.model_copy(update={"floor.backend": …})``
writes nothing at all. A knob marked live that silently does nothing is the exact
dishonest control the honesty tags exist to prevent, so every assertion here reads
the file back.
"""

from __future__ import annotations

import socket

import pytest

from opendaisugi.config import Config, FloorConfig, load_config, resolved_config, save_config
from opendaisugi.modules import ACTIVE, AVAILABLE, POSSIBLE, detect_stages
from opendaisugi.swap import (
    STAGE_EFFECT,
    SWAP_KNOBS,
    apply_swap,
    effect_of,
    get_field,
    is_live,
    resolve_command,
    selected_label,
    set_field,
)


def _cfg_path(tmp_path):
    return tmp_path / "config.yaml"


def test_the_default_floor_is_auto_with_no_notify():
    floor = Config().floor
    assert isinstance(floor, FloorConfig)
    assert floor.backend == "auto"
    assert floor.notify_cmd is None
    assert floor.tmux_socket is None


def test_the_nested_shape_round_trips_through_yaml(tmp_path):
    cfg = Config(floor=FloorConfig(backend="tmux", notify_cmd="ntfy publish x"))
    save_config(cfg, _cfg_path(tmp_path))
    text = _cfg_path(tmp_path).read_text()
    assert "floor:" in text and "backend: tmux" in text
    assert load_config(_cfg_path(tmp_path)).floor.backend == "tmux"


def test_get_and_set_field_walk_a_dotted_path():
    cfg = Config()
    assert get_field(cfg, "floor.backend") == "auto"
    assert get_field(cfg, "gate_mode") == "shadow"
    updated = set_field(cfg, "floor.backend", "herdr")
    assert updated.floor.backend == "herdr"
    assert cfg.floor.backend == "auto", "set_field must not mutate the original"


def test_a_floor_swap_actually_reaches_disk(tmp_path):
    apply_swap("floor_backend", "tmux", config_path=_cfg_path(tmp_path))
    assert load_config(_cfg_path(tmp_path)).floor.backend == "tmux"


def test_a_floor_swap_leaves_the_rest_of_the_floor_alone(tmp_path):
    save_config(
        Config(floor=FloorConfig(backend="auto", notify_cmd="ntfy publish x")),
        _cfg_path(tmp_path),
    )
    apply_swap("floor_backend", "herdr", config_path=_cfg_path(tmp_path))
    floor = load_config(_cfg_path(tmp_path)).floor
    assert floor.backend == "herdr" and floor.notify_cmd == "ntfy publish x"


def test_selected_label_reads_the_nested_field(tmp_path):
    apply_swap("floor_backend", "coppice", config_path=_cfg_path(tmp_path))
    assert selected_label(load_config(_cfg_path(tmp_path)), "floor_backend") == "coppice"


def test_the_command_line_resolves_a_floor_backend_swap():
    assert resolve_command("floor_backend tmux") == ("floor_backend", "tmux")


def test_the_floor_backend_stage_is_live_and_covered():
    assert STAGE_EFFECT["floor_backend"] == "live"
    assert is_live("floor_backend") and effect_of("floor_backend") == "live"
    assert SWAP_KNOBS["floor_backend"].field == "floor.backend"
    assert SWAP_KNOBS["floor_backend"].kind == "setting"


def test_the_floor_report_stage_stays_cfg():
    assert STAGE_EFFECT["floor_report"] == "cfg"
    assert not is_live("floor_report") and effect_of("floor_report") == "cfg"


def test_every_floor_backend_option_has_a_short_description():
    for opt in SWAP_KNOBS["floor_backend"].options:
        assert opt.desc.strip() and len(opt.desc) <= 40
        assert opt.cost is False


def test_the_floor_backend_stage_names_three_backends_honestly(tmp_path):
    # home=/which= isolate this from whatever this box actually has
    # installed, the same way the herdr detection tests do.
    stage = next(
        s
        for s in detect_stages(tmp_path, home=tmp_path / "home", which=lambda _: None)
        if s.key == "floor_backend"
    )
    names = [m.name for m in stage.modules]
    assert names == ["coppice", "herdr", "tmux"]
    for module in stage.modules:
        assert module.state in (ACTIVE, AVAILABLE, POSSIBLE)
        assert module.note.strip()


def test_tmux_is_available_exactly_when_it_is_on_path(tmp_path):
    on = detect_stages(tmp_path, home=tmp_path / "home", which=lambda name: "/usr/bin/" + name)
    off = detect_stages(tmp_path, home=tmp_path / "home", which=lambda _: None)
    tmux_on = next(m for s in on if s.key == "floor_backend" for m in s.modules if m.name == "tmux")
    tmux_off = next(
        m for s in off if s.key == "floor_backend" for m in s.modules if m.name == "tmux"
    )
    assert tmux_on.state == AVAILABLE
    assert tmux_off.state == POSSIBLE


def test_coppice_is_available_on_path_and_active_once_the_socket_answers(tmp_path, monkeypatch):
    monkeypatch.delenv("XDG_RUNTIME_DIR", raising=False)
    home = tmp_path / "home"

    nothing = detect_stages(tmp_path, home=home, which=lambda _: None)
    coppice_nothing = next(
        m for s in nothing if s.key == "floor_backend" for m in s.modules if m.name == "coppice"
    )
    assert coppice_nothing.state == POSSIBLE

    on_path = detect_stages(tmp_path, home=home, which=lambda name: "/usr/bin/" + name)
    coppice_on_path = next(
        m for s in on_path if s.key == "floor_backend" for m in s.modules if m.name == "coppice"
    )
    assert coppice_on_path.state == AVAILABLE

    sock_dir = home / ".opendaisugi" / "coppice"
    sock_dir.mkdir(parents=True)
    sock_path = sock_dir / "server.sock"
    srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    srv.bind(str(sock_path))
    try:
        answering = detect_stages(tmp_path, home=home, which=lambda _: None)
        coppice_answering = next(
            m
            for s in answering
            if s.key == "floor_backend"
            for m in s.modules
            if m.name == "coppice"
        )
        assert coppice_answering.state == ACTIVE
        assert coppice_answering.note == "server running"
    finally:
        srv.close()


def test_coppice_socket_detection_matches_the_backends_own_path_formula(tmp_path, monkeypatch):
    """modules.py cannot import the floor, so its socket path formula is
    pinned here against floor.coppice_backend.default_socket_path directly."""
    from opendaisugi.floor.coppice_backend import default_socket_path

    monkeypatch.delenv("XDG_RUNTIME_DIR", raising=False)
    monkeypatch.setattr("pathlib.Path.home", lambda: tmp_path)
    real_path = default_socket_path()
    assert real_path == tmp_path / ".opendaisugi" / "coppice" / "server.sock"

    real_path.parent.mkdir(parents=True)
    srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    srv.bind(str(real_path))
    try:
        stage = next(s for s in detect_stages(tmp_path) if s.key == "floor_backend")
        coppice = next(m for m in stage.modules if m.name == "coppice")
        assert coppice.state == ACTIVE
    finally:
        srv.close()


def test_the_layer_modules_never_import_the_floor_package():
    """Master §4 layer purity, checked at the source level so an import cannot creep in."""
    import inspect

    from opendaisugi import config, modules, swap

    for module in (config, swap, modules):
        source = inspect.getsource(module)
        assert "opendaisugi.floor" not in source, f"{module.__name__} imports the floor"


def test_resolved_config_flattens_the_floor_instead_of_printing_a_repr(tmp_path):
    save_config(Config(floor=FloorConfig(backend="tmux")), _cfg_path(tmp_path))
    rows = {f.key: f for f in resolved_config(_cfg_path(tmp_path))}
    assert "floor.backend" in rows and rows["floor.backend"].value == "tmux"
    assert "floor" not in rows, "a nested model must never print as a Config repr"


@pytest.mark.parametrize("bad", ["screen", "zellij", ""])
def test_an_unknown_backend_is_refused_at_the_swap(tmp_path, bad):
    with pytest.raises(ValueError):
        apply_swap("floor_backend", bad, config_path=_cfg_path(tmp_path))
