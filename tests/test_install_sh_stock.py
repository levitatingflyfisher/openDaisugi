"""scripts/install.sh on a stock box: it fetches zig and cmake itself,
builds with GOTOOLCHAIN=auto when go is old, keeps compiler output in a
log, and builds the voice engines only with --voice.

The tests run the script against a PATH of stubs (no zig, no cmake, a fake
go) or against a copy of the scripts with stubbed toolchain.sh and
native.sh. Nothing is built, fetched or installed outside tmp_path.
"""

from __future__ import annotations

import shutil
import subprocess
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent

_REAL_TOOLS = [
    "bash", "env", "sed", "awk", "dirname", "head", "tail", "cat", "mkdir", "rm", "mv", "ln",
    "install", "uname", "tr", "cut", "sort", "basename", "chmod", "printf", "tee", "seq",
]  # fmt: skip
_STUB_TOOLS = ["xz", "gcc", "make", "pkg-config", "git", "curl", "python3", "strip", "strings"]


def _stock_box(tmp_path: Path, go_version: str = "go1.25.1") -> Path:
    """A PATH dir with the distro tools, a go of the given version, no zig, no cmake."""
    box = tmp_path / "box"
    box.mkdir()
    for tool in _REAL_TOOLS:
        real = shutil.which(tool)
        if real:
            (box / tool).symlink_to(real)
    for tool in _STUB_TOOLS:
        (box / tool).write_text("#!/bin/sh\nexit 0\n")
        (box / tool).chmod(0o755)
    go = box / "go"
    go.write_text(
        "#!/bin/sh\n"
        'case "$1 $2" in\n'
        f'  "env GOVERSION") echo {go_version} ;;\n'
        '  "env GOTOOLCHAIN") echo "${GOTOOLCHAIN:-local}" ;;\n'
        "esac\n"
        'if [ "$1" = build ]; then\n'
        '  echo "GOTOOLCHAIN=${GOTOOLCHAIN:-unset}" >> "$GO_LOG"\n'
        "  echo compiler noise line\n"
        '  out=""; while [ $# -gt 0 ]; do [ "$1" = -o ] && out="$2"; shift; done\n'
        '  printf "#!/bin/sh\\n" > "$out"\n'
        "fi\n"
        "exit 0\n"
    )
    go.chmod(0o755)
    return box


def _stub_tree(tmp_path: Path, native_fails: bool = False) -> Path:
    """A copy of the scripts with the heavy parts replaced by stubs."""
    root = tmp_path / "tree"
    (root / "scripts").mkdir(parents=True)
    for name in ("install.sh", "preflight.sh"):
        shutil.copy(REPO / "scripts" / name, root / "scripts" / name)
    tc = root / "harness" / "coppice" / "scripts" / "toolchain.sh"
    tc.parent.mkdir(parents=True)
    tools = tmp_path / "fetched"
    tc.write_text(
        "#!/bin/bash\n"
        f'if [ "$1" = --print-path ]; then echo "{tools}"; exit 0; fi\n'
        f'mkdir -p "{tools}"\n'
        f'printf "#!/bin/sh\\necho 0.16.0\\n" > "{tools}/zig"\n'
        f'printf "#!/bin/sh\\nexit 0\\n" > "{tools}/cmake"\n'
        f'chmod +x "{tools}/zig" "{tools}/cmake"\n'
        'echo "toolchain args: $*" >> "$STAGE_LOG"\n'
    )
    nat = root / "clients" / "go" / "scripts" / "native.sh"
    nat.parent.mkdir(parents=True)
    body = (
        'for i in $(seq 1 40); do echo "fail line $i"; done; exit 1'
        if native_fails
        else "echo native noise"
    )
    nat.write_text(
        "#!/bin/bash\n"
        'case "$1" in\n'
        f'  --print-prefix|--print-moonshine|--print-parakeet) echo "{tmp_path}/prefix"; exit 0 ;;\n'
        "  --print-key) echo key; exit 0 ;;\n"
        "esac\n"
        'echo "native args: $*" >> "$STAGE_LOG"\n'
        f"{body}\n"
    )
    for f in (tc, nat):
        f.chmod(0o755)
    return root


def _run(
    tmp_path: Path, root: Path, box: Path, *args: str, **env: str
) -> subprocess.CompletedProcess[str]:
    base = {
        "HOME": str(tmp_path / "home"),
        "PATH": str(box),
        "XDG_BIN_HOME": str(tmp_path / "bin"),
        "STAGE_LOG": str(tmp_path / "stages.txt"),
        "GO_LOG": str(tmp_path / "go.txt"),
    }
    base.update(env)
    (tmp_path / "home").mkdir(exist_ok=True)
    return subprocess.run(  # noqa: S603 - the script under test
        [str(box / "bash"), str(root / "scripts" / "install.sh"), *args],
        env=base, capture_output=True, text=True, timeout=60, check=False,
    )  # fmt: skip


def test_dry_run_on_a_stock_box_plans_to_fetch_zig_and_cmake(tmp_path):
    r = _run(tmp_path, REPO, _stock_box(tmp_path), INSTALL_DRY="1")
    assert r.returncode == 0, r.stderr
    line = next(ln for ln in r.stdout.splitlines() if "fetch zig 0.16.0 and cmake" in ln)
    assert line.endswith(": yes")
    assert "GOTOOLCHAIN=auto: yes" in r.stdout
    assert "voice engines (moonshine, parakeet): no" in r.stdout
    # Preflight does not name what install.sh fetches itself.
    assert "missing: zig" not in r.stderr
    assert "missing: cmake" not in r.stderr


def test_dry_run_with_go_126_needs_no_toolchain_switch(tmp_path):
    r = _run(tmp_path, REPO, _stock_box(tmp_path, "go1.26.0"), INSTALL_DRY="1")
    assert "GOTOOLCHAIN=auto: no" in r.stdout


def test_a_stock_box_still_needs_what_only_the_distro_has(tmp_path):
    box = _stock_box(tmp_path)
    (box / "pkg-config").unlink()
    r = _run(tmp_path, REPO, box, INSTALL_DRY="1")
    assert r.returncode == 1
    assert "missing: pkg-config" in r.stderr


def test_the_build_is_quiet_fetches_tools_and_skips_voice(tmp_path):
    root = _stub_tree(tmp_path)
    r = _run(tmp_path, root, _stock_box(tmp_path))
    assert r.returncode == 0, r.stderr
    stages = (tmp_path / "stages.txt").read_text()
    assert "toolchain args: --tools-only" in stages
    assert "native args: --moonshine" not in stages
    assert "native args: --parakeet" not in stages
    assert "noise" not in r.stdout
    assert "noise" not in r.stderr
    assert "GOTOOLCHAIN=auto" in (tmp_path / "go.txt").read_text()
    assert "scripts/install.sh --voice" in r.stdout
    assert "about 45 min on 4 cores" in r.stdout
    log = tmp_path / "home" / ".local" / "state" / "opendaisugi" / "install.log"
    assert str(log) in r.stdout
    assert "compiler noise line" in log.read_text()
    assert (tmp_path / "bin" / "coppice").exists()


def test_voice_flag_builds_the_voice_engines(tmp_path):
    root = _stub_tree(tmp_path)
    r = _run(tmp_path, root, _stock_box(tmp_path), "--voice")
    assert r.returncode == 0, r.stderr
    stages = (tmp_path / "stages.txt").read_text()
    assert "native args: --moonshine" in stages
    assert "native args: --parakeet" in stages
    assert "scripts/install.sh --voice" not in r.stdout


def test_toolchain_prints_its_path_and_no_dev_box_paths(tmp_path):
    tc = REPO / "harness" / "coppice" / "scripts" / "toolchain.sh"
    data = tmp_path / "data"
    r = subprocess.run(  # noqa: S603
        ["/bin/bash", str(tc), "--print-path"],
        env={"HOME": str(tmp_path), "PATH": "/usr/bin:/bin", "XDG_DATA_HOME": str(data)},
        capture_output=True, text=True, timeout=30, check=False,
    )  # fmt: skip
    assert r.returncode == 0
    zig, cmake = r.stdout.strip().split(":")
    assert zig == f"{data}/opendaisugi/zig-0.16.0"
    assert "cmake-" in cmake
    assert not data.exists()  # printing the path installs nothing
    text = tc.read_text(encoding="utf-8")
    assert ".local/ghostty-vt" not in text


def test_a_failed_stage_shows_the_last_30_log_lines_and_the_log_path(tmp_path):
    root = _stub_tree(tmp_path, native_fails=True)
    log = tmp_path / "my.log"
    r = _run(tmp_path, root, _stock_box(tmp_path), INSTALL_LOG=str(log))
    assert r.returncode == 1
    assert "fail line 40" in r.stderr
    assert "fail line 11" in r.stderr
    assert "fail line 10\n" not in r.stderr
    assert str(log) in r.stderr
    assert not (tmp_path / "bin" / "coppice").exists()


def test_a_go_older_than_121_stops_with_one_line_naming_the_version(tmp_path):
    """GOTOOLCHAIN=auto needs go 1.21. Debian 12 ships 1.19: stop early."""
    r = _run(tmp_path, REPO, _stock_box(tmp_path, "go1.19.8"), INSTALL_DRY="1")
    assert r.returncode == 1
    lines = [ln for ln in r.stderr.splitlines() if "go1.19.8" in ln or "go is 1.19" in ln]
    assert len(lines) == 1, r.stderr
    assert "1.21" in lines[0]
    assert "https://go.dev/dl" in lines[0]


def test_go_121_is_enough_to_fetch_a_newer_go(tmp_path):
    r = _run(tmp_path, REPO, _stock_box(tmp_path, "go1.21.0"), INSTALL_DRY="1")
    assert r.returncode == 0, r.stderr
    assert "GOTOOLCHAIN=auto: yes" in r.stdout


def test_a_stock_box_without_xz_is_told_to_install_it(tmp_path):
    box = _stock_box(tmp_path)
    (box / "xz").unlink()
    r = _run(tmp_path, REPO, box, INSTALL_DRY="1")
    assert r.returncode == 1
    assert "missing: xz (install the xz-utils package" in r.stderr


def test_the_voice_note_is_silent_when_the_engines_are_already_linked(tmp_path):
    root = _stub_tree(tmp_path)
    (tmp_path / "bin").mkdir()
    for name in ("moonshine-cli", "parakeet-cli"):
        (tmp_path / "bin" / name).write_text("#!/bin/sh\n")
    r = _run(tmp_path, root, _stock_box(tmp_path))
    assert r.returncode == 0, r.stderr
    assert "voice engines were not built" not in r.stdout
