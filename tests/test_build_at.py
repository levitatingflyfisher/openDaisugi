"""clients/scripts/build-at.sh: the cache key (clean tree: the HEAD sha; dirty
tree: no cache), the cache hit, the lock between two callers and --prune. A
fake builder stands in for go and cargo."""

from __future__ import annotations

import os
import subprocess
import time
from pathlib import Path

SCRIPT = Path(__file__).resolve().parent.parent / "clients" / "scripts" / "build-at.sh"


def git(repo: Path, *args: str) -> str:
    env = {
        "PATH": os.environ["PATH"],
        "HOME": str(repo),
        "GIT_CONFIG_NOSYSTEM": "1",
        "GIT_AUTHOR_NAME": "t",
        "GIT_AUTHOR_EMAIL": "t@example.invalid",
        "GIT_COMMITTER_NAME": "t",
        "GIT_COMMITTER_EMAIL": "t@example.invalid",
    }
    return subprocess.run(
        ["git", "-C", str(repo), *args], env=env, check=True, capture_output=True, text=True
    ).stdout.strip()


def make_repo(tmp: Path) -> Path:
    repo = tmp / "wt"
    (repo / "harness" / "coppice").mkdir(parents=True)
    (repo / "harness" / "coppice" / "a.go").write_text("package a\n")
    (repo / "other.txt").write_text("x\n")
    git(repo, "init", "-q", "-b", "main")
    git(repo, "add", ".")
    git(repo, "commit", "-q", "-m", "one")
    return repo


def make_builder(tmp: Path, delay: float = 0.0) -> Path:
    b = tmp / "builder.sh"
    b.write_text(
        f'#!/usr/bin/env bash\necho "$1" >> "{tmp}/builds.log"\necho "$4" >> "{tmp}/targets.log"\n'
        f"sleep {delay}\n"
        'printf "binary-%s-%s" "$1" "$(git -C "$2" rev-parse HEAD)" > "$3"\n'
    )
    b.chmod(0o755)
    return b


def env(tmp: Path, builder: Path, tk: str = "tk1") -> dict[str, str]:
    return {
        **os.environ,
        "BUILD_AT_CACHE": str(tmp / "cache"),
        "BUILD_AT_RS_TARGET": str(tmp / "rs"),
        "BUILD_AT_BUILDER": str(builder),
        "BUILD_AT_TOOLKEY": tk,
    }


def run(tmp: Path, builder: Path, *args: str) -> subprocess.CompletedProcess:
    return subprocess.run(
        ["bash", str(SCRIPT), *args], env=env(tmp, builder), capture_output=True, text=True
    )


def builds(tmp: Path) -> list[str]:
    p = tmp / "builds.log"
    return p.read_text().split() if p.exists() else []


def test_clean_tree_is_keyed_by_sha_and_hits_the_cache(tmp_path):
    repo = make_repo(tmp_path)
    b = make_builder(tmp_path)
    sha = git(repo, "rev-parse", "HEAD")
    r1 = run(tmp_path, b, "go", str(repo))
    assert r1.returncode == 0, r1.stderr
    path = Path(r1.stdout.strip())
    assert path == tmp_path / "cache" / sha / "go-tk1" / "coppice"
    assert path.read_text() == f"binary-go-{sha}"
    r2 = run(tmp_path, b, "go", str(repo))
    assert r2.stdout.strip() == str(path)
    assert builds(tmp_path) == ["go"]  # the second call built nothing


def test_go_and_rust_cache_apart(tmp_path):
    repo = make_repo(tmp_path)
    b = make_builder(tmp_path)
    g = run(tmp_path, b, "go", str(repo)).stdout.strip()
    r = run(tmp_path, b, "rust", str(repo)).stdout.strip()
    assert g != r and builds(tmp_path) == ["go", "rust"]


def test_change_outside_harness_coppice_keeps_the_key(tmp_path):
    repo = make_repo(tmp_path)
    b = make_builder(tmp_path)
    run(tmp_path, b, "go", str(repo))
    (repo / "other.txt").write_text("changed\n")
    (repo / "new.txt").write_text("new\n")
    r = run(tmp_path, b, "go", str(repo))
    assert "without the cache" not in r.stderr
    assert builds(tmp_path) == ["go"]


def test_new_commit_is_a_new_key(tmp_path):
    repo = make_repo(tmp_path)
    b = make_builder(tmp_path)
    run(tmp_path, b, "go", str(repo))
    (repo / "other.txt").write_text("changed\n")
    git(repo, "commit", "-q", "-am", "two")
    run(tmp_path, b, "go", str(repo))
    assert builds(tmp_path) == ["go", "go"]


def test_dirty_tree_builds_every_time_and_says_so(tmp_path):
    repo = make_repo(tmp_path)
    b = make_builder(tmp_path)
    (repo / "harness" / "coppice" / "a.go").write_text("package b\n")
    r1 = run(tmp_path, b, "go", str(repo))
    assert r1.returncode == 0, r1.stderr
    assert "without the cache" in r1.stderr
    sha = git(repo, "rev-parse", "HEAD")
    assert sha not in r1.stdout
    run(tmp_path, b, "go", str(repo))
    assert builds(tmp_path) == ["go", "go"]
    assert not (tmp_path / "cache" / sha).exists()


def test_untracked_file_under_harness_coppice_is_dirty(tmp_path):
    repo = make_repo(tmp_path)
    b = make_builder(tmp_path)
    (repo / "harness" / "coppice-rs").mkdir()
    (repo / "harness" / "coppice-rs" / "n.rs").write_text("fn main(){}\n")
    r = run(tmp_path, b, "rust", str(repo))
    assert "without the cache" in r.stderr


def test_two_callers_at_one_sha_build_once(tmp_path):
    repo = make_repo(tmp_path)
    b = make_builder(tmp_path, delay=0.6)
    procs = [
        subprocess.Popen(
            ["bash", str(SCRIPT), "go", str(repo)],
            env=env(tmp_path, b),
            stdout=subprocess.PIPE,
            text=True,
        )
        for _ in range(4)
    ]
    outs = [p.communicate()[0].strip() for p in procs]
    assert all(p.returncode == 0 for p in procs)
    assert len(set(outs)) == 1
    assert builds(tmp_path) == ["go"]


def test_two_shas_get_two_different_binaries_from_one_target_dir(tmp_path):
    repo = make_repo(tmp_path)
    b = make_builder(tmp_path)
    first = git(repo, "rev-parse", "HEAD")
    one = Path(run(tmp_path, b, "rust", str(repo)).stdout.strip())
    (repo / "harness" / "coppice" / "a.go").write_text("package b\n")
    git(repo, "commit", "-q", "-am", "two")
    second = git(repo, "rev-parse", "HEAD")
    two = Path(run(tmp_path, b, "rust", str(repo)).stdout.strip())
    assert one != two and one.read_text() != two.read_text()
    assert one.read_text().endswith(first)
    assert two.read_text().endswith(second)
    targets = set((tmp_path / "targets.log").read_text().split())
    assert targets == {str(tmp_path / "rs")}  # one shared target dir


def test_new_toolchain_key_is_a_new_binary(tmp_path):
    repo = make_repo(tmp_path)
    b = make_builder(tmp_path)

    def go(tk):
        return subprocess.run(
            ["bash", str(SCRIPT), "go", str(repo)],
            env=env(tmp_path, b, tk),
            capture_output=True,
            text=True,
        ).stdout.strip()

    a1, a2, b1 = go("tkA"), go("tkA"), go("tkB")
    assert a1 == a2 and a1 != b1
    assert builds(tmp_path) == ["go", "go"]


def test_prune_refuses_a_root_the_script_did_not_make(tmp_path):
    b = make_builder(tmp_path)
    foreign = tmp_path / "cache"
    victim = foreign / ("a" * 40)
    victim.mkdir(parents=True)
    os.utime(victim, (1, 1))
    r = run(tmp_path, b, "--prune", "1")
    assert r.returncode != 0 and "not pruning" in r.stderr
    assert victim.exists()


def test_prune_leaves_dirs_that_are_not_cache_entries(tmp_path):
    repo = make_repo(tmp_path)
    b = make_builder(tmp_path)
    run(tmp_path, b, "go", str(repo))  # makes the cache and its marker
    stray = tmp_path / "cache" / "important"
    stray.mkdir()
    os.utime(stray, (1, 1))
    assert run(tmp_path, b, "--prune", "1").returncode == 0
    assert stray.exists()


def test_cache_root_with_other_files_gets_no_marker(tmp_path):
    repo = make_repo(tmp_path)
    b = make_builder(tmp_path)
    (tmp_path / "cache").mkdir()
    (tmp_path / "cache" / "mine.txt").write_text("x")
    run(tmp_path, b, "go", str(repo))
    assert not (tmp_path / "cache" / ".build-at-cache").exists()


def test_dirty_builds_of_same_named_worktrees_do_not_share_a_path(tmp_path):
    b = make_builder(tmp_path)
    paths = []
    for side in ("x", "y"):
        base = tmp_path / side
        base.mkdir()
        repo = make_repo(base)
        (repo / "harness" / "coppice" / "a.go").write_text("package z\n")
        paths.append(run(tmp_path, b, "go", str(repo)).stdout.strip())
    assert paths[0] != paths[1]


def test_failed_dirty_build_leaves_no_temp_file(tmp_path):
    repo = make_repo(tmp_path)
    bad = tmp_path / "bad.sh"
    bad.write_text('#!/usr/bin/env bash\necho half > "$3"\nexit 1\n')
    bad.chmod(0o755)
    (repo / "harness" / "coppice" / "a.go").write_text("package z\n")
    r = run(tmp_path, bad, "go", str(repo))
    assert r.returncode != 0
    assert not list((tmp_path / "cache").rglob("coppice.tmp.*"))


def test_prune_removes_old_entries_only(tmp_path):
    repo = make_repo(tmp_path)
    b = make_builder(tmp_path)
    old = Path(run(tmp_path, b, "go", str(repo)).stdout.strip()).parent.parent
    (repo / "other.txt").write_text("changed\n")
    git(repo, "commit", "-q", "-am", "two")
    new = Path(run(tmp_path, b, "go", str(repo)).stdout.strip()).parent.parent
    ago = time.time() - 10 * 86400
    os.utime(old, (ago, ago))
    r = run(tmp_path, b, "--prune", "5")
    assert r.returncode == 0, r.stderr
    assert not old.exists() and new.exists(), r.stderr
