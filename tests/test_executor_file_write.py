"""FileWriteExecutor — atomic write, symlink defense, parent auto-create."""

import os

import pytest

from opendaisugi.executor import FileWriteExecutor
from opendaisugi.models import FileWriteStep, ShellStep


def test_atomic_write_creates_file(tmp_path):
    target = tmp_path / "out.txt"
    step = FileWriteStep(id="s", path=str(target), content="hello")

    result = FileWriteExecutor().run(step, timeout_s=5, max_output_bytes=1024)

    assert result.rc == 0
    assert result.timed_out is False
    assert target.read_bytes() == b"hello"
    assert "5" in result.stdout
    assert str(target) in result.stdout


def test_autocreates_missing_parent_dir(tmp_path):
    target = tmp_path / "sub" / "nested" / "file.txt"
    step = FileWriteStep(id="s", path=str(target), content="deep")

    result = FileWriteExecutor().run(step, timeout_s=5, max_output_bytes=1024)

    assert result.rc == 0
    assert target.read_text() == "deep"


def test_symlink_at_target_rejected(tmp_path, tmp_path_factory):
    escape_dir = tmp_path_factory.mktemp("escape-sandbox")
    escape_target = str(escape_dir / "should-not-be-written")
    target = tmp_path / "sneaky.txt"
    os.symlink(escape_target, str(target))
    try:
        step = FileWriteStep(id="s", path=str(target), content="pwned")

        result = FileWriteExecutor().run(step, timeout_s=5, max_output_bytes=1024)

        assert result.rc == 2
        assert not os.path.exists(escape_target)
    finally:
        if os.path.islink(str(target)):
            os.unlink(str(target))


def test_existing_file_overwritten_atomically(tmp_path):
    target = tmp_path / "overwrite.txt"
    target.write_text("old")
    step = FileWriteStep(id="s", path=str(target), content="new")

    result = FileWriteExecutor().run(step, timeout_s=5, max_output_bytes=1024)

    assert result.rc == 0
    assert target.read_text() == "new"


def test_rejects_non_file_write_step():
    step = ShellStep(id="s", command="echo hi")
    with pytest.raises(TypeError):
        FileWriteExecutor().run(step, timeout_s=5, max_output_bytes=1024)


def test_oserror_returns_rc1(tmp_path):
    # Make the parent directory read-only so the tempfile open fails.
    if os.geteuid() == 0:
        pytest.skip("running as root bypasses directory mode bits")
    parent = tmp_path / "readonly"
    parent.mkdir()
    os.chmod(parent, 0o500)  # r-x only, no write
    try:
        step = FileWriteStep(id="s", path=str(parent / "file.txt"), content="data")
        result = FileWriteExecutor().run(step, timeout_s=5, max_output_bytes=1024)

        assert result.rc == 1
        assert result.stdout  # carries some error message
    finally:
        os.chmod(parent, 0o700)


# --------------------- symlink-escape guard (SGCM review EB-2) ---------------------

def _env_write(globs):
    from opendaisugi.models import Envelope, Permission
    return Envelope(generated_by="t", task="x", permissions=Permission(file_write=globs))


def test_file_write_rejects_ancestor_symlink_escape(tmp_path):
    from opendaisugi.executor import FileWriteExecutor
    from opendaisugi.models import FileWriteStep
    allowed = tmp_path / "allowed"; allowed.mkdir()
    outside = tmp_path / "outside"; outside.mkdir()
    (allowed / "sub").symlink_to(outside)  # symlink inside allowed → outside tree

    exe = FileWriteExecutor()
    exe.configure_from_envelope(_env_write([str(allowed) + "/**"]))
    r = exe.run(FileWriteStep(id="s", path=str(allowed / "sub" / "x.txt"), content="pwned"),
                timeout_s=2, max_output_bytes=1024)
    assert r.rc == 2
    assert not (outside / "x.txt").exists()  # write did NOT escape
    # legit in-tree write still works
    ok = exe.run(FileWriteStep(id="s", path=str(allowed / "y.txt"), content="fine"),
                 timeout_s=2, max_output_bytes=1024)
    assert ok.rc == 0 and (allowed / "y.txt").read_text() == "fine"
