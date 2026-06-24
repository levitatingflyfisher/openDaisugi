"""FileReadExecutor — chunked read with size cap, error handling."""

import pytest

from opendaisugi.executor import ExecutorResult, FileReadExecutor
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
