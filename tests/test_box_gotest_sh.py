import os
import stat
import subprocess
from pathlib import Path

REPO = Path(__file__).resolve().parents[1]


def fake_go(dir_, failing_suite):
    go = dir_ / "go"
    go.write_text(
        "#!/bin/sh\n"
        'case "$PWD" in *' + failing_suite + ") printf 'FAIL\\tx\\n'; exit 1;; esac\n"
        "echo ok; exit 0\n"
    )
    go.chmod(go.stat().st_mode | stat.S_IEXEC)
    return go


def run(tmp_path, failing):
    go = fake_go(tmp_path, failing)
    env = {
        **os.environ,
        "GO": str(go),
        "TMPDIR": str(tmp_path / "t"),
        "BOX_GOTEST_LOG": str(tmp_path / "logs"),
    }
    return subprocess.run(
        [str(REPO / "scripts/box-gotest.sh")], cwd=REPO, env=env, text=True, capture_output=True
    )


def test_a_failing_suite_fails_the_script(tmp_path):
    r = run(tmp_path, "harness/coppice")
    assert r.returncode == 1
    assert "harness/coppice: FAIL" in r.stdout and "clients/go: ok" in r.stdout
    assert (tmp_path / "logs" / "harness-coppice.log").read_text().startswith("FAIL")


def test_all_green_exits_zero(tmp_path):
    r = run(tmp_path, "no-such-suite")
    assert r.returncode == 0 and r.stdout.count(": ok") == 3
