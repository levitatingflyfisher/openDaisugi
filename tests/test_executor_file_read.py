"""FileReadExecutor — chunked read with size cap, error handling."""

import pytest

from opendaisugi.executor import FileReadExecutor
from opendaisugi.models import FileReadStep, ShellStep


def test_reads_existing_file(tmp_path):
    f = tmp_path / "data.txt"
    f.write_text("hello world")
    step = FileReadStep(id="s", path=str(f))

    result = FileReadExecutor().run(step, timeout_s=5, max_output_bytes=1024)

    assert result.rc == 0
    assert result.stdout == "hello world"
    assert result.timed_out is False


def test_truncates_at_max_output_bytes(tmp_path):
    f = tmp_path / "big.txt"
    f.write_text("x" * 5000)
    step = FileReadStep(id="s", path=str(f))

    result = FileReadExecutor().run(step, timeout_s=5, max_output_bytes=100)

    assert result.rc == 0
    assert result.stdout.startswith("x" * 100)
    assert "truncated" in result.stdout


def test_missing_file_returns_rc1(tmp_path):
    step = FileReadStep(id="s", path=str(tmp_path / "nope.txt"))

    result = FileReadExecutor().run(step, timeout_s=5, max_output_bytes=1024)

    assert result.rc == 1
    assert "no such" in result.stdout.lower()


def test_directory_path_returns_rc1(tmp_path):
    step = FileReadStep(id="s", path=str(tmp_path))

    result = FileReadExecutor().run(step, timeout_s=5, max_output_bytes=1024)

    assert result.rc == 1


def test_rejects_non_file_read_step():
    step = ShellStep(id="s", command="echo hi")
    with pytest.raises(TypeError):
        FileReadExecutor().run(step, timeout_s=5, max_output_bytes=1024)


# --------------------- symlink-escape guard (SGCM review EB-2) ---------------------

def _env_read(globs):
    from opendaisugi.models import Envelope, Permission
    return Envelope(generated_by="t", task="x", permissions=Permission(file_read=globs))


def test_file_read_rejects_inner_symlink_escape(tmp_path):
    from opendaisugi.executor import FileReadExecutor
    from opendaisugi.models import FileReadStep
    allowed = tmp_path / "allowed"; allowed.mkdir()
    secret = tmp_path / "secret.txt"; secret.write_text("TOP SECRET")
    (allowed / "link").symlink_to(secret)  # symlink INSIDE allowed → outside
    (allowed / "real.txt").write_text("legit")

    exe = FileReadExecutor()
    exe.configure_from_envelope(_env_read([str(allowed) + "/**"]))
    # escape via the inner symlink is refused
    r = exe.run(FileReadStep(id="s", path=str(allowed / "link")), timeout_s=2, max_output_bytes=1024)
    assert r.rc == 2 and "TOP SECRET" not in r.stdout
    # a legit in-tree read still works
    ok = exe.run(FileReadStep(id="s", path=str(allowed / "real.txt")), timeout_s=2, max_output_bytes=1024)
    assert ok.rc == 0 and "legit" in ok.stdout
