"""The gate's writers make what its reader accepts, and follow no link the
reader refuses (GR-R-7 to GR-R-10).

The refusal result is a third answer beside "opened" and "missing"; the
note ends with the fix; a writer works from a directory fd, so a link that
a concurrent write planted at a name is replaced (the gate's own files) or
refused (the operator's files), never written through.
"""

from __future__ import annotations

import os
from pathlib import Path

import pytest

from opendaisugi import _state_files as sf


@pytest.fixture(autouse=True)
def _fresh_notes():
    sf._NOTED.clear()
    yield
    sf._NOTED.clear()


@pytest.fixture
def root(tmp_path: Path) -> Path:
    r = tmp_path / "gate"
    r.mkdir(mode=0o700)
    r.chmod(0o700)
    return r


# ---- the fix is the end of every note ------------------------------------------


def test_the_note_ends_with_the_fix_for_a_group_writable_file(root, capsys):
    f = root / "DISARMED"
    f.write_text("x")
    f.chmod(0o666)
    fd, refusal = sf.probe_state_file(root, (), "DISARMED")
    assert fd is None
    want = f"{root}/DISARMED: is writable by group or others (fix: chmod 600 '{root}/DISARMED')"
    assert refusal == want
    assert capsys.readouterr().err == f"daisugi gate: not reading {want}\n"


def test_the_fix_for_a_group_writable_directory_is_chmod_700(root):
    root.chmod(0o775)
    fd, refusal = sf.probe_state_file(root, (), "DISARMED")
    assert fd is None
    assert refusal == f"{root}: is writable by group or others (fix: chmod 700 '{root}')"


def test_the_fix_for_a_link_is_to_remove_it(root, tmp_path):
    (root / "envelopes").symlink_to(tmp_path)
    _, refusal = sf.probe_state_file(root, ("envelopes",), "default.json")
    assert (
        refusal == f"{root}/envelopes: is a symbolic link (fix: remove the link '{root}/envelopes')"
    )


def test_a_path_with_a_quote_is_quoted_the_same_way_in_all_three_ports(tmp_path):
    r = tmp_path / "it's"
    r.mkdir()
    r.chmod(0o775)
    _, refusal = sf.probe_state_file(r, (), "DISARMED")
    assert refusal is not None
    assert refusal.endswith(f"(fix: chmod 700 '{tmp_path}/it'\\''s')")


# ---- refused is not missing ------------------------------------------------------


def test_missing_is_not_refused(root):
    fd, refusal = sf.probe_state_file(root, (), "DISARMED")
    assert (fd, refusal) == (None, None)
    fd, refusal = sf.probe_state_file(root, ("envelopes",), "default.json")
    assert (fd, refusal) == (None, None)


def test_an_opened_file_has_no_refusal(root):
    (root / "DISARMED").write_text("x")
    (root / "DISARMED").chmod(0o600)
    fd, refusal = sf.probe_state_file(root, (), "DISARMED")
    assert fd is not None and refusal is None
    os.close(fd)


# ---- the dir-fd writer ----------------------------------------------------------------


def test_the_gates_own_writer_replaces_a_planted_link_and_writes_nothing_through_it(root, tmp_path):
    victim = tmp_path / "victim"
    victim.write_text("keep")
    (root / "checkpoint-state").mkdir(mode=0o700)
    (root / "checkpoint-state" / "s1.json").symlink_to(victim)
    sf.write_state_file(
        root, ("checkpoint-state",), "s1.json", '{"prompt": "p"}', replace_link=True
    )
    assert victim.read_text() == "keep"
    got = root / "checkpoint-state" / "s1.json"
    assert not got.is_symlink()
    assert got.read_text() == '{"prompt": "p"}'
    assert got.stat().st_mode & 0o777 == 0o600


def test_the_gates_own_writer_does_not_trip_on_a_planted_temp_name(root, tmp_path):
    victim = tmp_path / "victim"
    victim.write_text("keep")
    d = root / "checkpoint-state"
    d.mkdir(mode=0o700)
    for n in range(8):
        (d / f".s1.json.{n}.tmp").symlink_to(victim)
    sf.write_state_file(root, ("checkpoint-state",), "s1.json", "{}", replace_link=True)
    assert victim.read_text() == "keep"
    assert (d / "s1.json").read_text() == "{}"


def test_an_operator_writer_refuses_a_link_at_the_name(root, tmp_path):
    victim = tmp_path / "victim"
    victim.write_text("keep")
    (root / "DISARMED").symlink_to(victim)
    with pytest.raises(sf.StateWriteRefused) as e:
        sf.write_state_file(root, (), "DISARMED", "x\n", replace_link=False)
    assert (
        str(e.value)
        == f"{root}/DISARMED: is a symbolic link (fix: remove the link '{root}/DISARMED')"
    )
    assert victim.read_text() == "keep"


def test_a_writer_refuses_a_symlinked_directory(root, tmp_path):
    (root / "envelopes").symlink_to(tmp_path)
    with pytest.raises(sf.StateWriteRefused):
        sf.write_state_file(root, ("envelopes",), "default.json", "{}", replace_link=False)
    assert not (tmp_path / "default.json").exists()


def test_a_writer_makes_the_directories_private_whatever_the_umask(tmp_path):
    old = os.umask(0o002)
    try:
        r = tmp_path / "home" / "gate"
        sf.write_state_file(r, ("envelopes",), "default.json", "{}", replace_link=False)
        (r / "envelopes").chmod(0o775)
        sf.write_state_file(r, ("envelopes",), "default.json", "{}", replace_link=False)
    finally:
        os.umask(old)
    assert r.stat().st_mode & 0o777 == 0o700
    assert (r / "envelopes").stat().st_mode & 0o777 == 0o700
    assert (r / "envelopes" / "default.json").stat().st_mode & 0o777 == 0o600


def test_mkdir_private_does_not_chmod_through_a_link(tmp_path):
    from opendaisugi.gate import _mkdir_private

    real = tmp_path / "real"
    real.mkdir()
    real.chmod(0o755)
    (tmp_path / "link").symlink_to(real)
    _mkdir_private(tmp_path / "link")
    assert real.stat().st_mode & 0o777 == 0o755


# ---- the commands say so, and exit non-zero ------------------------------------


def _cli(*args: str):
    from typer.testing import CliRunner

    from opendaisugi.cli import app

    return CliRunner().invoke(app, list(args))


def test_gate_disarm_fails_loudly_over_a_planted_link(root, tmp_path):
    victim = tmp_path / "victim"
    victim.write_text("keep")
    (root / "DISARMED").symlink_to(victim)
    res = _cli("gate", "disarm", "--root", str(root))
    assert res.exit_code == 1
    lines = [ln for ln in res.output.splitlines() if ln.strip()]
    assert lines == [
        f"gate NOT disarmed: {root}/DISARMED: is a symbolic link (fix: remove the link '{root}/DISARMED')"
    ]
    assert victim.read_text() == "keep"


def test_gate_disarm_succeeds_and_the_gate_is_disarmed(root):
    from opendaisugi.gate import is_disarmed

    res = _cli("gate", "disarm", "--root", str(root))
    assert res.exit_code == 0
    assert is_disarmed(root)


def test_gate_register_fails_loudly_over_a_linked_envelopes_dir(root, tmp_path):
    import json as _json

    from opendaisugi.gate import starter_envelope

    (root / "envelopes").symlink_to(tmp_path)
    env = tmp_path / "env.json"
    env.write_text(starter_envelope(Path("/work")).model_dump_json())
    res = _cli("gate", "register", str(env), "--root", str(root))
    assert res.exit_code == 1
    assert "not registered: " in res.output and "is a symbolic link" in res.output
    assert not (tmp_path / "default.json").exists()
    del _json


def _physical_default(data: Path, mode: int) -> None:
    from opendaisugi.gate import register_envelope
    from opendaisugi.models import Envelope

    gate = data / "gate"
    register_envelope(
        Envelope(generated_by="t", task="t", stakes="physical", permissions={}), root=gate
    )
    (gate / "envelopes" / "default.json").chmod(mode)


def test_a_refused_default_envelope_stops_the_delegate(tmp_path):
    from opendaisugi.delegate import run_delegate

    f = tmp_path / "a.txt"
    f.write_text("x\n" * 400)
    _physical_default(tmp_path / "d", 0o666)
    res = run_delegate(str(f), "what is in it?", "bulk_read", data_dir=tmp_path / "d")
    assert res.ok is False
    assert res.reason == "the gate's default envelope cannot be read"


def test_a_registered_physical_default_still_stops_the_delegate(tmp_path):
    from opendaisugi.delegate import run_delegate

    f = tmp_path / "a.txt"
    f.write_text("x\n" * 400)
    _physical_default(tmp_path / "d", 0o600)
    res = run_delegate(str(f), "what is in it?", "bulk_read", data_dir=tmp_path / "d")
    assert res.reason == "the delegate is refused under physical stakes"


def test_a_missing_default_envelope_does_not_stop_the_delegate_at_the_envelope(tmp_path):
    from opendaisugi.delegate import run_delegate

    f = tmp_path / "a.txt"
    f.write_text("x\n" * 400)
    res = run_delegate(str(f), "what is in it?", "bulk_read", data_dir=tmp_path / "d")
    assert res.reason != "the gate's default envelope cannot be read"


# ---- gate status lists each refused path ----------------------------------------


def test_gate_status_lists_each_refused_path(root, tmp_path):
    from opendaisugi.gate import disarm, register_envelope, starter_envelope

    register_envelope(starter_envelope(Path("/work")), root=root)
    register_envelope(starter_envelope(Path("/work")), root=root, session_id="s1")
    disarm(root)
    (root / "DISARMED").chmod(0o666)
    (root / "envelopes" / "s1.json").chmod(0o664)
    res = _cli("gate", "status", "--root", str(root))
    refused = [ln for ln in res.output.splitlines() if ln.startswith("refused: ")]
    assert refused == [
        f"refused: {root}/DISARMED: is writable by group or others (fix: chmod 600 '{root}/DISARMED')",
        f"refused: {root}/envelopes/s1.json: is writable by group or others"
        f" (fix: chmod 600 '{root}/envelopes/s1.json')",
    ]
    js = _cli("gate", "status", "--root", str(root), "--json")
    import json

    assert json.loads(js.output.splitlines()[0])["refused"] == [ln[9:] for ln in refused]


def test_gate_status_json_has_no_refused_key_when_nothing_is_refused(root):
    import json

    from opendaisugi.gate import register_envelope, starter_envelope

    register_envelope(starter_envelope(Path("/work")), root=root)
    js = _cli("gate", "status", "--root", str(root), "--json")
    assert "refused" not in json.loads(js.output.splitlines()[0])
    assert "refused: " not in _cli("gate", "status", "--root", str(root)).output


def test_gate_status_names_a_refused_root_and_nothing_below_it(tmp_path):
    r = tmp_path / "gate"
    r.mkdir()
    r.chmod(0o775)
    res = _cli("gate", "status", "--root", str(r))
    refused = [ln for ln in res.output.splitlines() if ln.startswith("refused: ")]
    assert refused == [f"refused: {r}: is writable by group or others (fix: chmod 700 '{r}')"]


def test_a_deny_for_a_refused_envelope_says_so_and_a_missing_one_does_not(root, tmp_path):
    import json

    from opendaisugi.gate import run_argv

    read = json.dumps(
        {
            "session_id": "s1",
            "tool_name": "Read",
            "tool_input": {"file_path": "/work/a"},
            "cwd": "/w",
        }
    ).encode()
    argv = ["--mode", "enforce", "--root", str(root), "--format", "claude"]
    assert "no envelope registered" in run_argv(argv, read).stderr
    (root / "envelopes").mkdir(mode=0o700)
    (root / "envelopes" / "default.json").write_text("{}")
    (root / "envelopes" / "default.json").chmod(0o666)
    out = run_argv(argv, read)
    assert out.exit_code == 2
    assert "the gate refused its envelope state: " in out.stderr
    assert "is writable by group or others (fix: chmod 600 " in out.stderr


# ---- GR r1 M1: arm removes the marker through the root's directory fd ------------


def test_arm_through_a_symlinked_root_says_so_and_removes_nothing(tmp_path):
    from typer.testing import CliRunner

    from opendaisugi.cli import app

    real = tmp_path / "real"
    real.mkdir(mode=0o700)
    (real / "DISARMED").write_text("x")
    link = tmp_path / "gate"
    link.symlink_to(real)
    res = CliRunner().invoke(app, ["gate", "arm", "--root", str(link)])
    assert res.exit_code == 1, res.output
    assert "gate NOT armed:" in res.stderr and "symbolic link" in res.stderr
    assert (real / "DISARMED").exists()


def test_arm_removes_the_marker_of_a_plain_root(root):
    from opendaisugi.gate import arm, disarm, is_disarmed

    disarm(root)
    assert is_disarmed(root)
    arm(root)
    assert not is_disarmed(root)
    arm(root)  # a missing marker is no error


# ---- GR r1 M2: the gate's own writers never repair a root the reader refuses -----


def test_the_gates_own_writer_refuses_a_group_writable_root_and_keeps_its_mode(root):
    root.chmod(0o775)
    with pytest.raises(sf.StateWriteRefused) as exc:
        sf.write_state_file(root, ("checkpoint-state",), "s1.json", "{}", replace_link=True)
    assert f"{root}: is writable by group or others (fix: chmod 700 '{root}')" in str(exc.value)
    assert (root.stat().st_mode & 0o777) == 0o775
    assert not (root / "checkpoint-state").exists()


def test_the_gates_own_writer_refuses_a_group_writable_subdirectory(root):
    (root / "asks").mkdir(mode=0o700)
    (root / "asks").chmod(0o770)
    with pytest.raises(sf.StateWriteRefused):
        sf.write_state_file(root, ("asks",), "a.json", "{}", replace_link=True)
    assert ((root / "asks").stat().st_mode & 0o777) == 0o770


def test_an_operator_writer_repairs_the_mode_and_says_so(root, capsys):
    root.chmod(0o775)
    sf.write_state_file(root, (), "DISARMED", "x\n", replace_link=False)
    assert (root.stat().st_mode & 0o777) == 0o700
    err = capsys.readouterr().err
    assert err == f"daisugi gate: repaired {root}: it was writable by group or others; now 700\n"
