"""native.sh --print-jobs: the job count fits the machine.

Runs `native.sh --print-jobs` with fake meminfo files. Nothing is built.
"""

from __future__ import annotations

import os
import subprocess
from pathlib import Path

SCRIPT = Path(__file__).resolve().parent.parent / "clients" / "go" / "scripts" / "native.sh"


def _meminfo(tmp_path: Path, avail_gib: int) -> Path:
    path = tmp_path / "meminfo"
    path.write_text(f"MemTotal: 99999999 kB\nMemAvailable: {avail_gib * 1024 * 1024} kB\n")
    return path


def _run(tmp_path: Path, **env: str) -> subprocess.CompletedProcess[str]:
    full = {k: v for k, v in os.environ.items() if not k.startswith("DAISUGI_")}
    full["XDG_CACHE_HOME"] = str(tmp_path / "cache")
    full.update(env)
    return subprocess.run(
        ["bash", str(SCRIPT), "--print-jobs"],
        capture_output=True, text=True, env=full, check=False,
    )  # fmt: skip


def test_env_override_used_unchanged(tmp_path: Path) -> None:
    r = _run(tmp_path, DAISUGI_NATIVE_JOBS="7", DAISUGI_NPROC="2",
             DAISUGI_MEMINFO=str(_meminfo(tmp_path, 1)))  # fmt: skip
    assert (r.returncode, r.stdout.strip()) == (0, "7")


def test_bad_env_value_refused(tmp_path: Path) -> None:
    for bad in ("0", "-3", "abc", "2.5", ""):
        r = _run(tmp_path, DAISUGI_NATIVE_JOBS=bad)
        assert r.returncode == 1, bad
        assert r.stdout == ""
        assert len(r.stderr.strip().splitlines()) == 1
        assert f"'{bad}'" in r.stderr


def test_many_cores_much_memory(tmp_path: Path) -> None:
    r = _run(tmp_path, DAISUGI_NPROC="12", DAISUGI_MEMINFO=str(_meminfo(tmp_path, 33)))
    assert r.stdout.strip() == "12"


def test_memory_limits_jobs(tmp_path: Path) -> None:
    r = _run(tmp_path, DAISUGI_NPROC="4", DAISUGI_MEMINFO=str(_meminfo(tmp_path, 7)))
    assert r.stdout.strip() == "3"


def test_at_least_one_job(tmp_path: Path) -> None:
    r = _run(tmp_path, DAISUGI_NPROC="16", DAISUGI_MEMINFO=str(_meminfo(tmp_path, 1)))
    assert r.stdout.strip() == "1"


def test_unreadable_meminfo_falls_back_to_two(tmp_path: Path) -> None:
    r = _run(tmp_path, DAISUGI_NPROC="8", DAISUGI_MEMINFO=str(tmp_path / "missing"))
    assert (r.returncode, r.stdout.strip()) == (0, "2")
