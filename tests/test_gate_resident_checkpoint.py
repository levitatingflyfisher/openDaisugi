"""Checkpoints are off inside the fence (FN-R-36); the bound snapshot stays (FN-R-27, FN-R-34).

The resident gate runs outside the fence. A checkpoint would make it run
git on config the fenced agent writes. So a --checkpoints call from a
fenced session is denied with one line, and the server takes no checkpoint
for it. The bound snapshot itself (git never chooses the tree, a git dir
that leads out is refused, a git worktree is taken when its link checks
out) stays for unfenced use, and is tested here by calling it directly.
The transcript a resident payload names is read only when it is a regular
file under ~/.claude/projects (FN-R-29).
"""

from __future__ import annotations

import base64
import json
import os
import socket
import subprocess
import threading
import time
from pathlib import Path

import pytest

from opendaisugi import fence_runs
from opendaisugi.gate import register_envelope, starter_envelope
from opendaisugi.gate_server import SOCK_NAME, serve

SECRET = "the-secret-the-fence-denies\n"


def _git(cwd: Path, *args: str) -> str:
    r = subprocess.run(["git", "-C", str(cwd), *args], check=True, capture_output=True, text=True)
    return r.stdout


def _repo(path: Path) -> Path:
    path.mkdir(parents=True, exist_ok=True)
    _git(path, "init", "-q")
    _git(path, "config", "user.email", "t@t")
    _git(path, "config", "user.name", "t")
    (path / "a.txt").write_text("one\n")
    _git(path, "add", "a.txt")
    _git(path, "commit", "-q", "-m", "init")
    return path


def _prompt(path: Path, uuid: str = "u1") -> Path:
    path.parent.mkdir(parents=True, exist_ok=True)
    row = {
        "type": "user",
        "uuid": uuid,
        "parentUuid": None,
        "sessionId": "s1",
        "timestamp": "2026-10-23T10:00:00Z",
        "message": {"role": "user", "content": "hi"},
    }
    path.write_text(json.dumps(row) + "\n")
    return path


@pytest.fixture
def home(tmp_path: Path, monkeypatch) -> Path:
    h = tmp_path / "home"
    (h / ".claude" / "projects" / "p").mkdir(parents=True)
    monkeypatch.setattr(
        fence_runs, "transcript_root", lambda: str(h / ".claude" / "projects"), raising=False
    )
    return h


@pytest.fixture
def root(tmp_path: Path) -> Path:
    r = tmp_path / "gate"
    register_envelope(starter_envelope(tmp_path), root=r)
    return r


@pytest.fixture
def server(root: Path):
    ready, stop = threading.Event(), threading.Event()
    t = threading.Thread(
        target=serve, args=(root,), kwargs={"ready": ready, "stop": stop}, daemon=True
    )
    t.start()
    assert ready.wait(5), "server did not start"
    yield root / SOCK_NAME
    stop.set()
    t.join(5)


def _call(sock: Path, cwd: str, transcript: str) -> dict:
    body = {
        "session_id": "s1",
        "tool_name": "Bash",
        "tool_input": {"command": "ls"},
        "cwd": cwd,
        "transcript_path": transcript,
    }
    req = {
        "v": 1,
        "argv": ["--mode", "enforce", "--checkpoints"],
        "stdin_b64": base64.b64encode(json.dumps(body).encode()).decode(),
    }
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as s:
        s.settimeout(30)
        s.connect(str(sock))
        s.sendall(json.dumps(req).encode() + b"\n")
        buf = b""
        while not buf.endswith(b"\n"):
            chunk = s.recv(65536)
            assert chunk, "no reply"
            buf += chunk
    return json.loads(buf)


def _refs(repo: Path) -> list[str]:
    out = _git(repo, "for-each-ref", "--format=%(refname)", "refs/daisugi")
    return [ln for ln in out.splitlines() if ln]


def _blobs(repo: Path) -> set[str]:
    out = subprocess.run(
        ["git", "-C", str(repo), "cat-file", "--batch-all-objects", "--batch"],
        check=True,
        capture_output=True,
    ).stdout
    return {SECRET} if SECRET.encode() in out else set()


def _snap(ws: Path, cwd: Path | None = None):
    """The bound snapshot, called directly: the unfenced use it stays for."""
    from opendaisugi.checkpoints import snapshot

    return snapshot(cwd or ws, session_id="s1", entry_id="e1", workspace=str(ws))


def _refused(ws: Path) -> bool:
    from opendaisugi.checkpoints import NotAGitRepo

    if fence_runs.git_dir_refusal(str(ws)) is None:
        return False
    with pytest.raises(NotAGitRepo):
        _snap(ws)
    return True


def test_a_fenced_checkpoint_call_is_denied(root, server, home, tmp_path):
    ws = _repo(tmp_path / "ws")
    fence_runs.register(root, os.getpid(), str(ws))
    t = _prompt(home / ".claude" / "projects" / "p" / "t.jsonl")
    reply = _call(server, str(ws), str(t))
    assert reply["exit_code"] == 2, reply
    assert reply["stderr"].strip().splitlines()[-1].endswith("checkpoints are off inside the fence")
    assert _refs(ws) == [], "the unfenced server took a checkpoint for a fenced session"


def test_a_fenced_checkpoint_call_with_no_cwd_is_denied(root, server, home, tmp_path):
    ws = _repo(tmp_path / "ws")
    fence_runs.register(root, os.getpid(), str(ws))
    reply = _call(server, "", "")
    assert reply["exit_code"] == 2, reply
    assert "checkpoints are off inside the fence" in reply["stderr"]


def test_an_unfenced_checkpoint_call_is_unchanged(root, server, home, tmp_path):
    ws = _repo(tmp_path / "ws")
    t = _prompt(home / ".claude" / "projects" / "p" / "t.jsonl")
    reply = _call(server, str(ws), str(t))
    assert reply["exit_code"] == 2, reply
    assert "runs under no fence run" in reply["stderr"]


def test_core_worktree_cannot_move_the_tree(root, server, home, tmp_path):
    other = tmp_path / "other"
    other.mkdir()
    (other / "id_key").write_text(SECRET)
    ws = _repo(tmp_path / "ws")
    _git(ws, "config", "core.worktree", str(other))
    _snap(ws)
    refs = _refs(ws)
    assert len(refs) == 1, "the bound workspace was checkpointed"
    names = _git(ws, "ls-tree", "-r", "--name-only", refs[0]).split()
    assert "a.txt" in names and "id_key" not in names
    assert _blobs(ws) == set(), "a file outside the workspace was hashed"


def test_a_gitfile_denies_the_call(root, server, home, tmp_path):
    other = _repo(tmp_path / "other")
    ws = tmp_path / "ws"
    ws.mkdir()
    (ws / ".git").write_text(f"gitdir: {other / '.git'}\n")
    assert _refused(ws)
    assert _refs(other) == []


@pytest.mark.parametrize("shape", ["link", "commondir", "objects-link", "fanout-link"])
def test_a_git_dir_that_leads_out_denies_the_call(root, server, home, tmp_path, shape):
    other = _repo(tmp_path / "other")
    ws = _repo(tmp_path / "ws")
    g = ws / ".git"
    if shape == "link":
        os.rename(g, tmp_path / "moved.git")
        g.symlink_to(tmp_path / "moved.git")
    elif shape == "commondir":
        (g / "commondir").write_text(str(other / ".git") + "\n")
    elif shape == "objects-link":
        os.rename(g / "objects", tmp_path / "moved-objects")
        (g / "objects").symlink_to(tmp_path / "moved-objects")
    else:
        (tmp_path / "far").mkdir()
        (g / "objects" / "ab").symlink_to(tmp_path / "far")
    assert _refused(ws), shape
    assert _refs(other) == []


def test_no_walk_up_to_a_parent_repo(root, server, home, tmp_path):
    parent = _repo(tmp_path / "parent")
    ws = parent / "ws"
    ws.mkdir()
    (ws / "b.txt").write_text("two\n")
    from opendaisugi.checkpoints import NotAGitRepo

    with pytest.raises(NotAGitRepo):
        _snap(ws)
    assert _refs(parent) == [], "the parent repo was checkpointed"


def test_a_subdir_cwd_snapshots_the_bound_workspace(root, server, home, tmp_path):
    ws = _repo(tmp_path / "ws")
    (ws / "sub").mkdir()
    (ws / "sub" / "c.txt").write_text("three\n")
    _snap(ws, ws / "sub")
    refs = _refs(ws)
    assert len(refs) == 1
    names = _git(ws, "ls-tree", "-r", "--name-only", refs[0]).split()
    assert {"a.txt", "sub/c.txt"} <= set(names)


def test_a_link_that_leaves_the_workspace_is_not_copied(root, server, home, tmp_path):
    other = tmp_path / "other"
    other.mkdir()
    (other / "id_key").write_text(SECRET)
    ws = _repo(tmp_path / "ws")
    (ws / "key").symlink_to(other / "id_key")
    (ws / "dir").symlink_to(other)
    _snap(ws)
    refs = _refs(ws)
    assert len(refs) == 1
    names = _git(ws, "ls-tree", "-r", "--name-only", refs[0]).split()
    assert "key" not in names and "dir" not in names and "a.txt" in names
    assert _blobs(ws) == set()


def test_a_transcript_outside_the_projects_dir_is_ignored(home, tmp_path):
    t = _prompt(tmp_path / "elsewhere" / "t.jsonl")
    assert fence_runs.resident_transcript(str(t)) is None
    # A link under the projects dir that leads out is outside too.
    link = home / ".claude" / "projects" / "p" / "l.jsonl"
    link.symlink_to(t)
    assert fence_runs.resident_transcript(str(link)) is None
    inside = _prompt(home / ".claude" / "projects" / "p" / "t.jsonl")
    assert fence_runs.resident_transcript(str(inside)) is not None


def test_a_fifo_transcript_is_ignored_at_once(home):
    fifo = home / ".claude" / "projects" / "p" / "t.jsonl"
    os.mkfifo(fifo)
    t0 = time.monotonic()
    assert fence_runs.resident_transcript(str(fifo)) is None
    assert time.monotonic() - t0 < 3, "the read waited on a FIFO"


# A git worktree workspace (W-1): its .git is a gitfile. git's own link must
# check out, by realpath: the gitfile names <common>/worktrees/<name>, that
# dir's gitdir file names this workspace's .git, its commondir is <common>.


def _worktree(tmp_path: Path) -> tuple[Path, Path]:
    main = _repo(tmp_path / "main")
    wt = tmp_path / "wt"
    _git(main, "worktree", "add", "-q", "--detach", str(wt))
    return main, wt


def test_a_git_worktree_takes_a_checkpoint_and_restores_it(root, server, home, tmp_path):
    from opendaisugi.checkpoints import restore

    main, wt = _worktree(tmp_path)
    (wt / "w.txt").write_text("in the worktree\n")
    _snap(wt)
    refs = _refs(wt)
    assert len(refs) == 1, "the worktree was checkpointed"
    names = _git(wt, "ls-tree", "-r", "--name-only", refs[0]).split()
    assert {"a.txt", "w.txt"} <= set(names)
    (wt / "a.txt").write_text("changed\n")
    (wt / "w.txt").unlink()
    restore(wt, ref=refs[0], session_id="s1", entry_id="back")
    assert (wt / "a.txt").read_text() == "one\n"
    assert (wt / "w.txt").read_text() == "in the worktree\n"
    assert (main / "a.txt").read_text() == "one\n"
    assert not (main / "w.txt").exists()


@pytest.mark.parametrize("shape", ["other-worktree", "no-back-link", "back-link-elsewhere",
                                   "commondir-elsewhere", "not-under-worktrees"])
def test_a_forged_worktree_gitfile_denies_the_call(root, server, home, tmp_path, shape):
    main, wt = _worktree(tmp_path)
    other = _repo(tmp_path / "other")
    ws = tmp_path / "ws"
    ws.mkdir()
    (ws / "b.txt").write_text("two\n")
    admin = main / ".git" / "worktrees" / "forged"
    if shape == "other-worktree":
        # The real worktree's admin dir: its gitdir names wt/.git, not ws/.git.
        target = main / ".git" / "worktrees" / "wt"
    elif shape == "not-under-worktrees":
        target = tmp_path / "loose"
        target.mkdir()
        (target / "gitdir").write_text(str(ws / ".git") + "\n")
        (target / "commondir").write_text(str(main / ".git") + "\n")
        (target / "HEAD").write_text(_git(main, "rev-parse", "HEAD"))
    else:
        admin.mkdir()
        (admin / "HEAD").write_text(_git(main, "rev-parse", "HEAD"))
        if shape != "no-back-link":
            back = wt / ".git" if shape == "back-link-elsewhere" else ws / ".git"
            (admin / "gitdir").write_text(str(back) + "\n")
        common = other / ".git" if shape == "commondir-elsewhere" else Path("../..")
        (admin / "commondir").write_text(str(common) + "\n")
        target = admin
    (ws / ".git").write_text(f"gitdir: {target}\n")
    assert _refused(ws), shape
    assert _refs(main) == [] and _refs(other) == []


def test_a_worktree_runs_no_filter_or_fsmonitor_from_the_common_config(
    root, server, home, tmp_path
):
    main, wt = _worktree(tmp_path)
    mark = tmp_path / "marks"
    mark.mkdir()
    mon = tmp_path / "fsmonitor.sh"
    mon.write_text(f"#!/bin/sh\ntouch {mark}/fsmonitor\n")
    mon.chmod(0o755)
    _git(main, "config", "filter.x.clean", f"sh -c 'touch {mark}/clean; cat'")
    _git(main, "config", "filter.x.required", "true")
    _git(main, "config", "core.fsmonitor", str(mon))
    (wt / ".gitattributes").write_text("*.txt filter=x\n")
    (wt / "c.txt").write_text("three\n")
    _snap(wt)
    assert len(_refs(wt)) == 1
    assert sorted(os.listdir(mark)) == [], "a filter or fsmonitor from the common config ran"
