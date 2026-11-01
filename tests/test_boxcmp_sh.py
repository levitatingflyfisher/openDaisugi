"""scripts/boxcmp.sh: the Rust run of a compare skips the go_only cases.

A fake uv and a fake git on PATH record each call in a scratch HOME; the
build checkout holds two compare scripts, one that knows --skip-go-only
and one that does not. Nothing real runs.
"""

from __future__ import annotations

import subprocess
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
SCRIPT = REPO / "scripts" / "boxcmp.sh"


def test_the_rust_run_gets_skip_go_only_where_the_compare_knows_it(tmp_path):
    fakes = tmp_path / "fakes"
    fakes.mkdir()
    log = tmp_path / "uv.log"
    (fakes / "uv").write_text(f'#!/bin/sh\necho "$*" >> {log}\necho "12 agree"\n')
    (fakes / "git").write_text("#!/bin/sh\necho 'abc123 head'\n")
    for f in ("uv", "git"):
        (fakes / f).chmod(0o755)
    clients = tmp_path / "daisugi-ci" / "build" / "clients"
    clients.mkdir(parents=True)
    (clients / "fence_compare.py").write_text('ap.add_argument("--skip-go-only")\n')
    (clients / "tree_compare.py").write_text("ap.add_argument('--binary')\n")
    r = subprocess.run(  # noqa: S603 - the script under test
        ["/bin/bash", str(SCRIPT), "round9", "fence", "tree"],
        env={
            "HOME": str(tmp_path),
            "PATH": f"{fakes}:/usr/bin:/bin",
            "BOXCMP_SCRATCH": str(tmp_path / "dc"),
        },
        capture_output=True,
        text=True,
        timeout=30,
        check=False,
    )
    assert r.returncode == 0, r.stderr
    calls = log.read_text().splitlines()
    assert len(calls) == 4
    build = tmp_path / "daisugi-ci" / "build"
    assert (
        calls[0]
        == f"run --no-sync python clients/fence_compare.py --binary {build}/clients/go/daisugi"
    )
    assert calls[1] == (
        f"run --no-sync python clients/fence_compare.py --binary {build}/clients/rust/target/release/daisugi --skip-go-only"
    )
    assert (
        calls[3]
        == f"run --no-sync python clients/tree_compare.py --binary {build}/clients/rust/target/release/daisugi"
    )
    assert "== fence rust" in r.stdout and "12 agree" in r.stdout
