"""Branching: N children in parallel in their own worktrees, ranked, one card."""

from __future__ import annotations

import dataclasses
import json
import os
import subprocess
import time
from pathlib import Path

import pytest

from opendaisugi import rank, tree, tree_branch, tree_run
from tests.test_tree_run import _env, _root


def _repo(tmp_path: Path) -> Path:
    r = tmp_path / "repo"
    r.mkdir()
    subprocess.run(["git", "init", "-q", str(r)], check=True)
    (r / "a.txt").write_text("one\n")
    subprocess.run(["git", "-C", str(r), "add", "a.txt"], check=True)
    subprocess.run(
        ["git", "-C", str(r), "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "init"],
        check=True,
    )
    return r


def _args(d, g, cwd, **kw):
    base = dict(
        parent="top", envelope=_env(), profiles=None, profile_name=None, model="m",
        tokens=100, turns=2, parent_tokens_used=0, parent_turns_used=0, task="t", tools=[],
        max_turns=None, cwd=str(cwd), timeout_s=5, sprig_binary="sprig", environ={},
        attempts=2, why="operator", parallel=2, checks=[], judge=None, fence=None,
    )  # fmt: skip
    base.update(kw)
    return base


def test_read_request_takes_attempts_and_why():
    ok = {"task": "t", "envelope": {}, "tools": [], "attempts": 3, "why": "failed_tests"}
    assert tree_run.read_request(json.dumps(ok).encode())["attempts"] == 3
    one = {"task": "t", "envelope": {}, "tools": [], "attempts": 1}
    assert tree_run.read_request(json.dumps(one).encode())["attempts"] == 1
    for bad in (9, True, 0, "2", 2.5):
        doc = {"task": "t", "envelope": {}, "tools": [], "attempts": bad, "why": "operator"}
        with pytest.raises(ValueError, match="2 to 8"):
            tree_run.read_request(json.dumps(doc).encode())
    for why in (None, "because"):
        doc = {"task": "t", "envelope": {}, "tools": [], "attempts": 2}
        if why:
            doc["why"] = why
        with pytest.raises(ValueError, match="why must be one of"):
            tree_run.read_request(json.dumps(doc).encode())


def test_branching_needs_a_git_workspace(tmp_path):
    d, g = _root(tmp_path, tokens=1000, turns=10)
    out = tree_branch.run_branches(d, g, **_args(d, g, tmp_path))
    assert out.status == "refused" and "needs a git workspace" in out.reasons[0]
    assert not (d / "tree" / "branches").exists()


def test_branches_reserve_all_or_none(tmp_path):
    d, g = _root(tmp_path, tokens=150, turns=10)
    repo = _repo(tmp_path)
    started = []
    out = tree_branch.run_branches(
        d, g, **_args(d, g, repo, start=lambda *a, **k: started.append(k) or None)
    )
    assert out.status == "refused" and "2 branches of 100 each need 200" in out.reasons[0]
    assert "has 150 tokens left" in out.reasons[0]
    assert started == [] and not (d / "tree" / "branches").exists()
    assert [s for s in tree.read_tree(d).nodes if s != "top"] == []


def _fake_start(live, peak, fail=("3",)):
    def start(data_dir, root, *, session, cwd, **k):
        live.append(session)
        peak[0] = max(peak[0], len(live))
        (Path(cwd) / "a.txt").write_text(f"{session}\n")
        live.remove(session)
        bad = any(session.endswith(f) for f in fail)
        return tree_run.NodeRun(
            "failed" if bad else "started", session,
            answer=f"answer {session}", tokens_used=10 + len(session), turns_used=1,
        )  # fmt: skip

    return start


def test_branches_run_rank_and_open_a_card(tmp_path):
    d, g = _root(tmp_path, tokens=1000, turns=10)
    repo = _repo(tmp_path)
    sessions = [tree.mint_session("top", n) for n in range(3)]
    bad = sessions[2][-1]
    out = tree_branch.run_branches(
        d, g,
        **_args(d, g, repo, attempts=3, why="failed_tests", parallel=3,
                start=_fake_start([], [0], fail=(bad,))),
    )  # fmt: skip
    assert out.status == "ran" and out.leader_session in sessions[:2]
    assert [b["session"] for b in out.branches] == sessions
    assert [b["status"] for b in out.branches].count("failed") == 1
    assert out.ranking["eliminated"][0]["reasons"] == ["required test failed: the child ran"]
    cards = rank.read_cards(d)
    assert len(cards) == 1 and cards[0].id == out.choice_id
    t = tree.read_tree(d)
    assert all(t.nodes[s].state == "ended" for s in sessions)
    assert all(
        t.nodes[s].branch_of == out.ranking_id and t.nodes[s].why == "failed_tests"
        for s in sessions
    )
    rows = [json.loads(x) for x in (d / "tree" / "ledger.jsonl").read_text().splitlines()]
    assert [r["session"] for r in rows if r["event"] == "spawn"] == sessions
    assert [r["session"] for r in rows if r["event"] == "end"] == sessions
    first_end = next(i for i, r in enumerate(rows) if r["event"] == "end")
    assert all(r["event"] == "spawn" for r in rows[first_end - 3 : first_end])
    text = tree.status_text(tree.status_doc(d))
    assert f"branches of {out.ranking_id} (failed_tests): 3 started, 3 ended, 1 eliminated" in text


def test_branches_start_at_most_parallel_at_once(tmp_path):
    d, g = _root(tmp_path, tokens=1000, turns=10)
    repo = _repo(tmp_path)
    peak = [0]
    out = tree_branch.run_branches(
        d, g, **_args(d, g, repo, attempts=4, parallel=1, start=_fake_start([], peak, fail=()))
    )
    assert out.status == "ran" and peak[0] == 1


def test_a_required_check_eliminates(tmp_path):
    d, g = _root(tmp_path, tokens=1000, turns=10)
    repo = _repo(tmp_path)
    s = [tree.mint_session("top", n) for n in range(2)]
    checks = [{"name": "tests", "argv": ["sh", "-c", f"grep -q {s[1]} a.txt"], "required": True}]
    out = tree_branch.run_branches(
        d, g, **_args(d, g, repo, checks=checks, start=_fake_start([], [0], fail=()))
    )
    assert out.status == "ran" and out.leader_session == s[1]
    assert out.ranking["eliminated"] == [{"id": s[0], "reasons": ["required test failed: tests"]}]


def test_every_branch_failing_chooses_nothing(tmp_path):
    d, g = _root(tmp_path, tokens=1000, turns=10)
    repo = _repo(tmp_path)
    out = tree_branch.run_branches(
        d,
        g,
        **_args(
            d,
            g,
            repo,
            start=_fake_start(
                [],
                [0],
                fail=(
                    "0",
                    "1",
                    "2",
                    "3",
                    "4",
                    "5",
                    "6",
                    "7",
                    "8",
                    "9",
                    "a",
                    "b",
                    "c",
                    "d",
                    "e",
                    "f",
                ),
            ),
        ),
    )
    assert out.status == "failed" and out.choice_id is None and len(out.reasons) == 2
    assert rank.read_cards(d) == []


def test_prune_removes_only_closed_cards_worktrees(tmp_path):
    d, g = _root(tmp_path, tokens=1000, turns=10)
    repo = _repo(tmp_path)
    out = tree_branch.run_branches(d, g, **_args(d, g, repo, start=_fake_start([], [0], fail=())))
    assert out.status == "ran" and out.choice_id
    assert tree_branch.prune_branches(d) == []  # the card is open
    assert all(Path(b["worktree"]).exists() for b in out.branches)
    rank.append_rows(d, [{
        "choice_id": out.choice_id, "ranking_id": out.ranking_id, "event": "confirmed",
        "how": "drop", "ts": 1.0,
    }])  # fmt: skip
    assert tree_branch.prune_branches(d) == sorted(b["session"] for b in out.branches)
    assert not (d / "tree" / "branches").exists()


def _git(repo, *a):
    return subprocess.run(
        ["git", "-C", str(repo), "-c", "user.name=t", "-c", "user.email=t@t", *a],
        check=True, capture_output=True, text=True,
    ).stdout  # fmt: skip


def _ran(tmp_path):
    d, g = _root(tmp_path, tokens=1000, turns=10)
    repo = _repo(tmp_path)
    out = tree_branch.run_branches(d, g, **_args(d, g, repo, start=_fake_start([], [0], fail=())))
    assert out.status == "ran"
    return d, repo, out


def test_the_card_names_the_adopt_command(tmp_path):
    d, repo, out = _ran(tmp_path)
    view = rank.queue(d, rank.now(), "date")["cards"][0]
    assert view["adopt"] == f"daisugi tree adopt {out.leader_session}"
    assert view["adopt"] in rank.queue_text(rank.queue(d, rank.now(), "date"))


def test_adopt_applies_the_leaders_edits_and_nothing_before(tmp_path):
    d, repo, out = _ran(tmp_path)
    assert (repo / "a.txt").read_text() == "one\n"  # nothing is copied by itself
    res = tree_branch.adopt(d, out.leader_session)
    assert res.status == "adopted" and res.files == ["a.txt"]
    assert (repo / "a.txt").read_text() == f"{out.leader_session}\n"


def test_adopt_refuses_a_dirty_file_and_writes_nothing(tmp_path):
    d, repo, out = _ran(tmp_path)
    (repo / "a.txt").write_text("mine\n")
    res = tree_branch.adopt(d, out.leader_session)
    assert res.status == "refused" and "a.txt has uncommitted changes" in res.why
    assert (repo / "a.txt").read_text() == "mine\n"


def test_adopt_refuses_a_conflict_and_leaves_the_tree_as_it_was(tmp_path):
    d, repo, out = _ran(tmp_path)
    (repo / "a.txt").write_text("other\n")
    _git(repo, "commit", "-qam", "moved on")  # the parent changed the same line
    res = tree_branch.adopt(d, out.leader_session)
    assert res.status == "refused" and "do not apply cleanly" in res.why
    assert (repo / "a.txt").read_text() == "other\n"
    assert _git(repo, "status", "--porcelain") == ""


def test_adopt_unknown_session_and_a_session_with_no_change(tmp_path):
    d, repo, out = _ran(tmp_path)
    assert tree_branch.adopt(d, "nope").status == "refused"
    assert tree_branch.adopt(d, "../x").status == "refused"
    wt = Path(out.branches[0]["worktree"])
    _git(wt, "checkout", "-q", "--", "a.txt")
    assert tree_branch.adopt(d, out.branches[0]["session"]).status == "nothing"


def test_adopt_is_a_tree_write_verb_and_the_cli_exits_2_on_refusal(tmp_path):
    from opendaisugi.tree_rule import names_tree_write

    assert names_tree_write("daisugi tree adopt tabc")
    d, repo, out = _ran(tmp_path)
    (repo / "a.txt").write_text("mine\n")
    from typer.testing import CliRunner

    from opendaisugi.cli import app

    res = CliRunner().invoke(app, ["tree", "adopt", out.leader_session, "--data-dir", str(d)])
    assert res.exit_code == 2 and "uncommitted" in res.output


# --- the one starter, with the fake sprig ----------------------------------

from typer.testing import CliRunner  # noqa: E402

from opendaisugi.cli import app  # noqa: E402
from opendaisugi.tree_rule import names_tree_write  # noqa: E402
from tests.test_tree_run import _flag, _req, fake, fence  # noqa: E402, F401


def test_prune_branches_is_a_tree_write_verb():
    assert names_tree_write("daisugi tree prune-branches")
    assert not names_tree_write("daisugi tree status")


def test_tree_run_with_attempts_starts_real_children_in_worktrees(tmp_path, fake, monkeypatch):  # noqa: F811
    _, calls = fake
    d, g = _root(tmp_path, tokens=1000, turns=20)
    repo = _repo(tmp_path)
    monkeypatch.chdir(repo)
    res = CliRunner().invoke(
        app,
        ["tree", "run", "--parent", "top", "--profile", "default", "--tokens", "200"]
        + ["--turns", "5", "--parallel", "2", "--data-dir", str(d), "--root", str(g), "-"],
        input=_req(attempts=2, why="operator"),
    )
    assert res.exit_code == 0, res.output
    out = json.loads(res.stdout)
    assert out["status"] == "ran" and len(out["branches"]) == 2 and out["choice_id"]
    sessions = [b["session"] for b in out["branches"]]
    assert sorted(_flag(c["argv"], "--session") for c in calls()) == sorted(sessions)
    t = tree.read_tree(d)
    assert all(t.nodes[s].state == "ended" for s in sessions)
    assert t.left(t.nodes["top"], "tokens") == 1000 - 2 * 7  # each branch used 7 of 200
    res = CliRunner().invoke(app, ["tree", "status", "--data-dir", str(d)])
    assert "branches of " in res.output
    res = CliRunner().invoke(app, ["tree", "prune-branches", "--data-dir", str(d), "--json"])
    assert json.loads(res.output) == {"removed": []}  # the card is open


def test_tree_run_with_attempts_outside_git_starts_nothing(tmp_path, fake, monkeypatch):  # noqa: F811
    _, calls = fake
    d, g = _root(tmp_path, tokens=1000, turns=20)
    plain = tmp_path / "plain"
    plain.mkdir()
    monkeypatch.chdir(plain)
    res = CliRunner().invoke(
        app,
        ["tree", "run", "--parent", "top", "--profile", "default", "--tokens", "200"]
        + ["--turns", "5", "--data-dir", str(d), "--root", str(g), "-"],
        input=_req(attempts=2, why="operator"),
    )
    assert res.exit_code == 1 and calls() == []
    assert "needs a git workspace" in json.loads(res.stdout)["reasons"][0]


def test_a_parent_that_is_not_running_leaves_no_worktree_and_no_node(tmp_path):
    d, g = _root(tmp_path, tokens=1000, turns=10)
    repo = _repo(tmp_path)
    started = []
    out = tree_branch.run_branches(
        d, g, **_args(d, g, repo, parent="ghost", start=lambda *a, **k: started.append(k))
    )
    assert out.status == "refused" and "not a running node" in out.reasons[0]
    assert started == [] and not (d / "tree" / "branches").exists()
    assert [s for s in tree.read_tree(d).nodes if s != "top"] == []
    assert (
        subprocess.run(
            ["git", "-C", str(repo), "worktree", "list"], capture_output=True, text=True
        ).stdout.count("\n")
        == 1
    )


def test_a_race_on_a_later_branch_ends_the_earlier_ones(tmp_path):
    d, g = _root(tmp_path, tokens=1000, turns=10)
    sessions = [tree.mint_session("top", n) for n in range(2)]
    # the second session is taken before the reservation runs
    tree.spawn(d, g, _env(), parent="top", session=sessions[1], tokens=10, turns=1)
    res = tree.spawn_branches(
        d,
        g,
        _env(),
        parent="top",
        sessions=sessions,
        tokens=100,
        turns=2,
        branch_of="br_x",
        why="operator",
    )
    assert res.status == "refused"
    t = tree.read_tree(d)
    assert t.nodes[sessions[0]].state == "ended" and t.nodes[sessions[0]].tokens_used is None


# --- the parent never trusts what a child wrote (B1) -----------------------


def _victim(tmp_path):
    v = tmp_path / "victim"
    v.mkdir()
    subprocess.run(["git", "init", "-q", str(v)], check=True)
    (v / "secret.txt").write_text("keep me\n")
    (v / "a.txt").write_text("victim\n")
    _git(v, "add", "-A")
    _git(v, "commit", "-qm", "init")
    _git(v, "worktree", "add", "-q", "--detach", str(tmp_path / "victim-wt"), "HEAD")
    return v, tmp_path / "victim-wt"


def _tree_of(path):
    return subprocess.run(
        ["git", "-C", str(path), "status", "--porcelain"], capture_output=True, text=True
    ).stdout


def test_the_spawn_rows_carry_the_base_and_the_repository(tmp_path):
    d, repo, out = _ran(tmp_path)
    head = _git(repo, "rev-parse", "HEAD").strip()
    common = str((repo / ".git").resolve())
    rows = [json.loads(x) for x in (d / "tree" / "ledger.jsonl").read_text().splitlines()]
    spawns = [r for r in rows if r["event"] == "spawn"]
    assert spawns and all(r["base"] == head and r["repo"] == common for r in spawns)
    assert tree.read_tree(d).nodes[out.leader_session].base == head


def test_no_base_note_is_kept_in_the_worktree_git_dir(tmp_path):
    d, repo, out = _ran(tmp_path)
    wt = Path(out.branches[0]["worktree"])
    gd = Path(_git(wt, "rev-parse", "--path-format=absolute", "--git-dir").strip())
    assert not (gd / "daisugi-base").exists()


def test_a_base_note_that_names_an_output_file_writes_nothing(tmp_path):
    d, repo, out = _ran(tmp_path)
    wt = Path(out.branches[0]["worktree"])
    gd = Path(_git(wt, "rev-parse", "--path-format=absolute", "--git-dir").strip())
    target = tmp_path / "pwned"
    (gd / "daisugi-base").write_text(f"--output={target}\n")
    sess = out.branches[0]["session"]
    ref = tree_branch.branch_ref(d, sess)
    assert ref is not None
    tree_branch.branch_hash(wt, ref[0], ref[1], "x")
    tree_branch.diff_lines(wt, ref[0], ref[1])
    res = tree_branch.adopt(d, sess)
    assert res.status == "adopted"
    assert not target.exists()


def test_a_dash_base_is_never_read_as_an_option(tmp_path):
    from opendaisugi import branch_git

    d, repo, out = _ran(tmp_path)
    wt = Path(out.branches[0]["worktree"])
    ref = tree_branch.branch_ref(d, out.branches[0]["session"])
    target = tmp_path / "pwned2"
    assert branch_git.diff(wt, f"--output={target}", ref[1]) is None
    assert not target.exists()
    assert tree.is_object_id("a" * 40) and tree.is_object_id("b" * 64)
    assert not tree.is_object_id("a" * 39) and not tree.is_object_id("--output=x")
    assert not tree.is_object_id("A" * 40) and not tree.is_object_id("a" * 40 + "\n")


def test_a_redirected_dot_git_is_refused_and_no_repository_is_written(tmp_path):
    d, repo, out = _ran(tmp_path)
    v, vwt = _victim(tmp_path)
    wt = Path(out.branches[0]["worktree"])
    vgd = _git(vwt, "rev-parse", "--path-format=absolute", "--git-dir").strip()
    (wt / ".git").write_text(f"gitdir: {vgd}\n")
    before_index = (Path(vgd) / "index").read_bytes()
    before_main = (v / ".git" / "index").read_bytes()
    res = tree_branch.adopt(d, out.branches[0]["session"], cwd=str(v))
    assert res.status == "refused" and "no longer belongs to" in res.why
    assert (v / "secret.txt").read_text() == "keep me\n"
    assert _tree_of(v) == "" and _tree_of(vwt) == ""
    assert (Path(vgd) / "index").read_bytes() == before_index
    assert (v / ".git" / "index").read_bytes() == before_main
    # and the main git dir of the victim, which has no worktree entry
    (wt / ".git").write_text(f"gitdir: {v / '.git'}\n")
    res = tree_branch.adopt(d, out.branches[0]["session"])
    assert res.status == "refused" and "no longer belongs to" in res.why
    assert (v / ".git" / "index").read_bytes() == before_main
    # the hash and the line count read nothing from the stranger either
    ref = tree_branch.branch_ref(d, out.branches[0]["session"])
    assert tree_branch.diff_lines(wt, ref[0], ref[1]) == 0


def _index(gd):
    f = Path(gd) / "index"
    return f.read_bytes() if f.exists() else b""


def _read_all(d, repo, out, wt, sess):
    """Every read the parent makes of a branch worktree."""
    from opendaisugi import branch_git

    ref = tree_branch.branch_ref(d, sess)
    assert ref is not None
    tree_branch.diff_lines(wt, ref[0], ref[1])
    tree_branch.branch_hash(wt, ref[0], ref[1], "x")
    branch_git.diff(wt, ref[0], ref[1], timeout=60)
    return tree_branch.adopt(d, sess)


@pytest.mark.parametrize("form", ["file", "symlink"])
def test_dot_git_at_the_main_git_dir_is_refused_and_the_main_index_stays(tmp_path, form):
    sub = tmp_path / form
    sub.mkdir()
    d, repo, out = _ran(sub)
    wt = Path(out.branches[0]["worktree"])
    sess = out.branches[0]["session"]
    main = (repo / ".git").resolve()
    (wt / "b.txt").write_text("new\n")
    (wt / ".git").unlink()
    if form == "file":
        (wt / ".git").write_text(f"gitdir: {main}\n")
    else:
        (wt / ".git").symlink_to(main)
    before = _index(main)
    res = _read_all(d, repo, out, wt, sess)
    assert res.status == "refused" and "no longer belongs to" in res.why
    assert _index(main) == before
    assert _tree_of(repo) == ""


def test_dot_git_at_a_siblings_git_dir_is_refused_and_the_sibling_index_stays(tmp_path):
    d, repo, out = _ran(tmp_path)
    wt = Path(out.branches[0]["worktree"])
    sib = Path(out.branches[1]["worktree"])
    sess = out.branches[0]["session"]
    sgd = Path(_git(sib, "rev-parse", "--path-format=absolute", "--git-dir").strip())
    (wt / "evil.txt").write_text("evil\n")
    (wt / ".git").write_text(f"gitdir: {sgd}\n")
    before, status = _index(sgd), _tree_of(sib)
    res = _read_all(d, repo, out, wt, sess)
    assert res.status == "refused" and "no longer belongs to" in res.why
    assert _index(sgd) == before and _tree_of(sib) == status
    assert "evil.txt" not in _tree_of(sib)


def _arm(repo, tmp_path):
    """A repository config and a hook the child could have written. Returns
    the marker file they create when they run."""
    marker = tmp_path / "ran"
    script = tmp_path / "hook.sh"
    script.write_text(f"#!/bin/sh\necho x >> {marker}\n")
    script.chmod(0o755)
    _git(repo, "config", "core.fsmonitor", str(script))
    hook = repo / ".git" / "hooks" / "post-checkout"
    hook.write_text(f"#!/bin/sh\necho x >> {marker}\n")
    hook.chmod(0o755)
    return marker


def test_a_config_and_a_hook_the_child_wrote_do_not_run_under_the_parents_reads(tmp_path):
    d, repo, out = _ran(tmp_path)
    wt = Path(out.branches[0]["worktree"])
    sess = out.branches[0]["session"]
    marker = _arm(repo, tmp_path)
    subprocess.run(["git", "-C", str(wt), "status"], capture_output=True)
    assert marker.exists(), "the control: an unguarded git runs the child's fsmonitor"
    marker.unlink()
    res = _read_all(d, repo, out, wt, sess)
    assert res.status == "adopted"
    assert not marker.exists()


def test_a_hook_the_child_wrote_does_not_run_on_a_refused_adopt(tmp_path):
    d, repo, out = _ran(tmp_path)
    (repo / "a.txt").write_text("other\n")
    _git(repo, "commit", "-qam", "moved on")
    marker = _arm(repo, tmp_path)
    _git(repo, "checkout", "HEAD", "--", "a.txt")
    assert marker.exists(), "the control: an unguarded checkout runs the hook"
    marker.unlink()
    res = tree_branch.adopt(d, out.leader_session)
    assert res.status == "refused" and "do not apply cleanly" in res.why
    assert not marker.exists()


def test_adopt_into_another_repository_is_refused(tmp_path):
    d, repo, out = _ran(tmp_path)
    v, _ = _victim(tmp_path)
    res = tree_branch.adopt(d, out.leader_session, cwd=str(v))
    assert res.status == "refused" and (v / "a.txt").read_text() == "victim\n"
    assert _tree_of(v) == ""


def test_adopt_refuses_a_session_with_a_nul_byte_without_raising(tmp_path):
    d, repo, out = _ran(tmp_path)
    assert tree_branch.adopt(d, "a\0b").status == "refused"
    assert tree_branch.adopt(d, "default").status == "refused"


def test_the_judge_artifact_needs_the_recorded_base(tmp_path):
    from opendaisugi import rank_judge

    d, repo, out = _ran(tmp_path)
    wt = out.branches[0]["worktree"]
    ref = tree_branch.branch_ref(d, out.branches[0]["session"])
    good = rank.Attempt(
        id="x", content_hash="sha256:x", author="m", edge_proof="ok",
        where={"kind": "worktree", "path": wt, "base": ref[0], "repo": ref[1]},
    )  # fmt: skip
    assert "a.txt" in rank_judge.artifact(good)
    w: list[str] = []
    bare = dataclasses.replace(good, where={"kind": "worktree", "path": wt})
    assert rank_judge.artifact(bare, w) == "" and "no artifact" in w[0]
    target = tmp_path / "pwned3"
    evil = dataclasses.replace(
        good, where={"kind": "worktree", "path": wt, "base": f"--output={target}", "repo": ref[1]}
    )
    assert rank_judge.artifact(evil, w) == "" and not target.exists()
    v, vwt = _victim(tmp_path)
    (Path(wt) / ".git").write_text(
        "gitdir: " + _git(vwt, "rev-parse", "--path-format=absolute", "--git-dir").strip() + "\n"
    )
    assert rank_judge.artifact(good, w) == ""


# --- prune keeps what only adopt can copy (I2) -----------------------------


def test_a_single_survivor_is_not_pruned_until_it_ages_out(tmp_path):
    d, g = _root(tmp_path, tokens=1000, turns=10)
    repo = _repo(tmp_path)
    sessions = [tree.mint_session("top", n) for n in range(2)]
    out = tree_branch.run_branches(
        d, g, **_args(d, g, repo, start=_fake_start([], [0], fail=(sessions[1][-1],)))
    )
    assert out.status == "ran" and out.choice_id is None  # one survivor: no card
    leader = Path(next(b["worktree"] for b in out.branches if b["session"] == out.leader_session))
    assert tree_branch.prune_branches(d) == []
    assert leader.exists()
    later = rank.now() + 15 * 86400
    assert tree_branch.prune_branches(d, now=later) == sorted(sessions)
    assert not leader.exists()


def test_prune_leaves_a_young_cardless_branch_and_removes_a_closed_one(tmp_path):
    d, repo, out = _ran(tmp_path)
    assert tree_branch.prune_branches(d, now=rank.now() + 20 * 86400) == []  # open card


def test_attempts_one_point_zero_is_not_the_single_child():
    doc = {"task": "t", "envelope": {}, "tools": [], "attempts": 1.0, "why": "operator"}
    with pytest.raises(ValueError, match="2 to 8"):
        tree_run.read_request(json.dumps(doc).encode())


# --- a fenced branch child may write its worktree and nowhere else (I5) ----


def test_a_fenced_branch_child_is_granted_its_own_worktree_only(tmp_path, fake, fence, monkeypatch):
    _, calls = fake
    words, fence_calls = fence
    d, g = _root(tmp_path, tokens=1000, turns=20)
    repo = _repo(tmp_path)
    words = [*words, "--workspace", str(repo)]
    monkeypatch.chdir(repo)
    monkeypatch.setenv("DAISUGI_SPRIG_FENCE", json.dumps(words))
    res = CliRunner().invoke(
        app,
        ["tree", "run", "--parent", "top", "--profile", "default", "--tokens", "200"]
        + ["--turns", "5", "--parallel", "2", "--sprig-fence", "--data-dir", str(d)]
        + ["--root", str(g), "-"],
        input=_req(attempts=2, why="operator"),
    )
    assert res.exit_code == 0, res.output
    out = json.loads(res.stdout)
    wts = {b["session"]: b["worktree"] for b in out["branches"]}
    fenced = fence_calls()
    assert len(fenced) == 2
    common = str((repo / ".git").resolve())
    seen = set()
    for fargv in fenced:
        # The workspace is the branch's own worktree, never the project.
        ws = [fargv[i + 1] for i, a in enumerate(fargv) if a == "--workspace"]
        assert len(ws) == 1 and ws[0] in wts.values() and str(repo) not in ws
        sess = Path(ws[0]).name
        seen.add(sess)
        grants = [fargv[i + 1] for i, a in enumerate(fargv) if a == "--allow"]
        git_grants = [x for x in grants if x.startswith(common)]
        # The shared git directory is read only (the objects too); this
        # branch's own admin directory is the only writable place inside it.
        assert sorted(git_grants) == sorted([f"{common}:ro", f"{common}/worktrees/{sess}:rw"])
        assert not [x for x in grants if x.endswith(":rw") and "/tree/branches/" in x]
    assert seen == set(wts)


def _real_fence(tmp_path):
    import shutil

    exe = Path(__file__).resolve().parents[1] / "clients" / "go" / "daisugi"
    if not exe.exists() or not shutil.which("git"):
        pytest.skip("no built daisugi to fence with")
    ws = tmp_path / "probe"
    ws.mkdir()
    probe = subprocess.run(
        [str(exe), "fence", "--workspace", str(ws), "--", "true"], capture_output=True, text=True,
        env={"HOME": str(tmp_path / "probe-home"), "PATH": os.environ["PATH"]},
    )  # fmt: skip
    if probe.returncode != 0:
        pytest.skip("this kernel has no usable Landlock")
    return exe


def test_a_really_fenced_child_writes_its_worktree_and_no_sibling(tmp_path):
    exe = _real_fence(tmp_path)
    repo = _repo(tmp_path)
    data = tmp_path / "data"
    mine, sibling = (tree_branch.make_worktree(str(repo), data, s) for s in ("n-mine", "n-sib"))
    common = str((repo / ".git").resolve())
    home = tmp_path / "home"
    home.mkdir()
    loose = next(p for p in (repo / ".git" / "objects").glob("??/*"))
    before = loose.read_bytes()
    script = (
        f"echo x > {mine}/new.txt; echo mine=$?; "
        f"echo x > {sibling}/new.txt; echo sibling=$?; "
        f"echo x > {tmp_path}/outside.txt; echo outside=$?; "
        f"echo x >> {common}/config; echo config=$?; "
        f"mkdir -p {common}/hooks; echo x > {common}/hooks/pre-commit; echo hooks=$?; "
        f"echo x > {common}/objects/probe; echo objects=$?; "
        f"rm -f {loose}; echo objrm=$?; "
        f"echo x > {loose}; echo objwrite=$?; "
        f"echo x >> {common}/objects/info/alternates; echo alt=$?; "
        f"echo x > {common}/worktrees/n-sib/probe; echo sibgit=$?; "
        f"echo two > {mine}/a.txt; "
        f"git -C {mine} add -A; echo add=$?; "
        f"git -C {mine} -c user.name=t -c user.email=t@t commit -qm c; echo commit=$?"
    )
    words = tree_run.branch_fence(
        ["--harness", "sprig", "--workspace", str(repo)], str(mine), "n-mine", common
    )
    env = tree_run.branch_env({"HOME": str(home), "PATH": os.environ["PATH"]}, "n-mine", common)
    r = subprocess.run(
        [str(exe), "fence", *words, "--", "sh", "-c", script],
        capture_output=True, text=True, env=env,
    )  # fmt: skip
    got = dict(x.split("=") for x in r.stdout.split() if "=" in x)
    assert got == {
        "mine": "0", "sibling": "1", "outside": "1", "config": "1", "hooks": "1",
        "objects": "1", "objrm": "1", "objwrite": "1", "alt": "1", "sibgit": "1",
        "add": "0", "commit": "0",
    }, (r.stdout, r.stderr)  # fmt: skip
    assert (Path(mine) / "new.txt").exists() and not (Path(sibling) / "new.txt").exists()
    assert loose.read_bytes() == before and not (repo / ".git" / "objects" / "probe").exists()
    assert "x" not in (repo / ".git" / "config").read_text().splitlines()
    assert not (repo / ".git" / "hooks" / "pre-commit").exists()
    # The child's commit lives in its own store, and the parent still reads it.
    own = repo / ".git" / "worktrees" / "n-mine" / "objects"
    assert any(own.glob("??/*"))
    base = _git(repo, "rev-parse", "HEAD").strip()
    from opendaisugi import branch_git

    child_head = _git(mine, "rev-parse", "HEAD").strip()
    assert subprocess.run(
        ["git", "-C", str(repo), "cat-file", "-e", child_head], capture_output=True
    ).returncode != 0, "the child's commit is not in the shared store"  # fmt: skip
    # Until the store is folded away the parent reads nothing; after, it reads
    # the child's work as worktree files against the base.
    assert branch_git.diff(mine, base, common) is None
    assert branch_git.fold(common, mine, base)
    got_diff = branch_git.diff(mine, base, common)
    assert got_diff is not None and b"+two" in got_diff and b"+x" in got_diff
    for cmd in (["fsck"], ["gc", "-q"], ["log", "--all", "--oneline"], ["rev-list", "--all"]):
        assert subprocess.run(["git", "-C", str(repo), *cmd], capture_output=True).returncode == 0


def _committing_start(extra=None):
    """A start that does what a fenced child does: edit its worktree, then
    add and commit with the branch environment, so the objects land in the
    child's own store. ``extra(cwd, session, common, env)`` runs last."""

    def start(data_dir, root, *, session, cwd, **k):
        common = tree.read_tree(data_dir).nodes[session].repo
        env = tree_run.branch_env(dict(os.environ), session, common)
        assert env["GIT_OBJECT_DIRECTORY"].startswith(common)
        (Path(cwd) / "a.txt").write_text(f"{session}\n")
        (Path(cwd) / "new.txt").write_text("fresh\n")
        for cmd in (
            ["add", "-A"],
            ["-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "c"],
        ):
            subprocess.run(["git", "-C", cwd, *cmd], check=True, capture_output=True, env=env)
        (Path(cwd) / "a.txt").write_text(f"{session} again\n")
        subprocess.run(["git", "-C", cwd, "add", "a.txt"], check=True, capture_output=True, env=env)
        if extra is not None:
            extra(cwd, session, common, env)
        return tree_run.NodeRun(
            "started", session, answer=f"answer {session}", tokens_used=10, turns_used=1
        )

    return start


def _whole(repo):
    """The operator's repository passes the checks a child's own store broke."""
    for cmd in (["fsck"], ["gc", "-q"], ["log", "--all", "--oneline"], ["rev-list", "--all"]):
        r = subprocess.run(["git", "-C", str(repo), *cmd], capture_output=True, text=True)
        assert r.returncode == 0, (cmd, r.stderr)


def test_adopting_from_a_child_that_committed_leaves_the_operator_repository_whole(tmp_path):
    d, g = _root(tmp_path, tokens=1000, turns=10)
    repo = _repo(tmp_path)
    out = tree_branch.run_branches(d, g, **_args(d, g, repo, start=_committing_start()))
    assert out.status == "ran", out.reasons
    sess = out.leader_session
    _, common = tree_branch.branch_ref(d, sess)
    # while the branch worktrees exist
    _whole(repo)
    for b in out.branches:
        assert not (Path(common) / "worktrees" / b["session"] / "objects").exists()
    res = tree_branch.adopt(d, sess)
    assert res.status == "adopted", res.why
    assert (repo / "a.txt").read_text() == f"{sess} again\n"
    assert (repo / "new.txt").read_text() == "fresh\n"
    _whole(repo)
    _git(repo, "add", "-A")
    _git(repo, "commit", "-qm", "adopted")
    for b in out.branches:
        tree_branch.remove_worktree(Path(b["worktree"]), common)
    _whole(repo)


def test_a_store_that_was_not_folded_is_not_read(tmp_path):
    d, repo, out = _ran(tmp_path)
    sess = out.leader_session
    base, common = tree_branch.branch_ref(d, sess)
    wt = tree_branch.branches_dir(d) / sess
    (Path(common) / "worktrees" / sess / "objects").mkdir()
    assert tree_branch.adopt(d, sess).status == "refused"
    from opendaisugi import branch_git

    assert branch_git.diff(wt, base, common) is None
    assert branch_git.fold(common, wt, base)
    assert tree_branch.adopt(d, sess).status == "adopted"


def test_the_fold_follows_no_link_the_child_made(tmp_path):
    from opendaisugi import branch_git

    d, repo, out = _ran(tmp_path)
    sess = out.leader_session
    base, common = tree_branch.branch_ref(d, sess)
    wt = tree_branch.branches_dir(d) / sess
    gd = Path(common) / "worktrees" / sess
    _git(repo, "commit", "-q", "--allow-empty", "-m", "operator moves on")
    branch = _git(repo, "symbolic-ref", "--short", "HEAD").strip()
    tip = _git(repo, "rev-parse", "HEAD").strip()
    config = (Path(common) / "config").read_text()
    index = (Path(common) / "index").read_bytes()
    (gd / "objects").mkdir()
    (gd / "HEAD").write_text(f"ref: refs/heads/{branch}\n")
    (gd / "index").unlink()
    (gd / "index").symlink_to(Path(common) / "index")
    shutil_target = tmp_path / "keep"
    shutil_target.mkdir()
    (shutil_target / "HEAD").write_text("keep\n")
    (gd / "logs").mkdir(exist_ok=True)
    import shutil

    shutil.rmtree(gd / "logs")
    (gd / "logs").symlink_to(shutil_target)
    (gd / "HEAD.lock").write_text("x")
    (gd / "index.lock").mkdir()
    assert branch_git.fold(common, wt, base)
    assert _git(repo, "rev-parse", branch).strip() == tip
    assert (Path(common) / "config").read_text() == config
    assert (Path(common) / "index").read_bytes() == index
    assert (shutil_target / "HEAD").read_text() == "keep\n"
    assert (gd / "HEAD").read_text().strip() == base
    assert not (gd / "index").is_symlink() and not (gd / "objects").exists()
    assert tree_branch.adopt(d, sess).status == "adopted"
    _whole(repo)


def test_an_alternates_file_the_child_wrote_is_never_opened(tmp_path):
    import threading

    def fifo(cwd, session, common, env):
        info = Path(common) / "worktrees" / session / "objects" / "info"
        info.mkdir(parents=True, exist_ok=True)
        os.mkfifo(info / "alternates")
        # content no store holds, so a lookup that misses reads the alternates
        (Path(cwd) / "a.txt").write_text(f"{session} late\n")

    d, g = _root(tmp_path, tokens=1000, turns=10)
    repo = _repo(tmp_path)
    got = {}

    def go():
        got["out"] = tree_branch.run_branches(
            d, g, **_args(d, g, repo, start=_committing_start(fifo))
        )
        got["adopt"] = tree_branch.adopt(d, got["out"].leader_session)

    t = threading.Thread(target=go, daemon=True)
    t.start()
    t.join(20)
    assert not t.is_alive(), "a parent git call opened the child's alternates"
    assert got["adopt"].status == "adopted", got["adopt"].why


def test_a_process_the_child_left_is_stopped_before_the_parent_reads(tmp_path):
    """A child leaves a process in a session of its own, so the kill of its
    process group misses it. From a moment after the child exits, it would
    keep writing a config.worktree and a store into the branch's admin
    directory, and the index."""
    pids = tmp_path / "pids"

    def daemon(cwd, session, common, env):
        gd = Path(common) / "worktrees" / session
        loop = (
            f"echo $$ >> {pids}; sleep 0.3; i=0; while [ $i -lt 300 ]; do "
            f": > {gd}/config.worktree; mkdir -p {gd}/objects; "
            f"git -C {cwd} add -A >/dev/null 2>&1; sleep 0.01; i=$((i+1)); done"
        )
        subprocess.run(
            ["sh", "-c", f"setsid sh -c '{loop}' </dev/null >/dev/null 2>&1 &"], check=True
        )
        deadline = time.monotonic() + 5
        while not pids.exists() and time.monotonic() < deadline:
            time.sleep(0.01)

    d, g = _root(tmp_path, tokens=1000, turns=10)
    repo = _repo(tmp_path)
    try:
        out = tree_branch.run_branches(d, g, **_args(d, g, repo, start=_committing_start(daemon)))
        left = [int(x) for x in pids.read_text().split()]
        assert len(left) == 2
        for pid in left:
            with pytest.raises(ProcessLookupError):
                os.kill(pid, 0)
        assert out.status == "ran", out.reasons
        time.sleep(0.6)
        sess = out.leader_session
        _, common = tree_branch.branch_ref(d, sess)
        assert not (Path(common) / "worktrees" / sess / "config.worktree").exists()
        res = tree_branch.adopt(d, sess)
        assert res.status == "adopted", res.why
        _whole(repo)
    finally:
        for pid in [int(x) for x in pids.read_text().split()] if pids.exists() else []:
            try:
                os.kill(pid, 9)
            except OSError:
                pass


def test_a_non_fenced_branch_child_keeps_the_shared_store(tmp_path, fake, monkeypatch):
    d, repo, out = _ran(tmp_path)
    sess = out.branches[0]["session"]
    base, common = tree_branch.branch_ref(d, sess)
    assert tree_run.branch_env({"A": "b"}, sess, None) == {"A": "b"}
    assert not (Path(common) / "worktrees" / sess / "objects").exists()
    assert tree_branch.adopt(d, sess).status in ("adopted", "nothing")


def _nested(wt, tmp_path):
    """A repository inside the worktree whose own config runs a program as a
    clean filter. Returns the marker the program writes."""
    marker = tmp_path / "ran-nested"
    script = tmp_path / "clean.sh"
    script.write_text(f"#!/bin/sh\necho x >> {marker}\ncat\n")
    script.chmod(0o755)
    sub = wt / "sub"
    sub.mkdir()
    subprocess.run(["git", "init", "-q", str(sub)], check=True)
    (sub / "s.txt").write_text("aaaa\n")
    (sub / ".gitattributes").write_text("* filter=x\n")
    _git(sub, "add", "-A")
    _git(sub, "commit", "-qm", "n")
    _git(sub, "config", "filter.x.clean", str(script))
    (sub / "s.txt").write_text("bbbb\n")
    os.utime(sub / "s.txt", (1, 1))
    return marker


def test_a_nested_repository_the_child_made_runs_no_program_in_the_parent(tmp_path):
    d, repo, out = _ran(tmp_path)
    wt = Path(out.branches[0]["worktree"])
    sess = out.branches[0]["session"]
    marker = _nested(wt, tmp_path)
    base = _git(repo, "rev-parse", "HEAD").strip()
    # the child stages the nested repository itself, as a gitlink
    subprocess.run(["git", "-C", str(wt), "add", "sub"], capture_output=True, check=True)
    (wt / "sub" / "s.txt").write_text("cccc\n")
    os.utime(wt / "sub" / "s.txt", (2, 2))
    subprocess.run(["git", "-C", str(wt), "diff", "--name-only", base], capture_output=True)
    assert marker.exists(), "the control: an unguarded diff runs the nested repository's filter"
    marker.unlink()
    (wt / "sub" / "s.txt").write_text("dddd\n")
    os.utime(wt / "sub" / "s.txt", (5, 5))
    _read_all(d, repo, out, wt, sess)
    assert not marker.exists()


def test_a_config_worktree_the_child_wrote_is_refused_and_runs_nothing(tmp_path):
    d, repo, out = _ran(tmp_path)
    wt = Path(out.branches[0]["worktree"])
    sess = out.branches[0]["session"]
    _git(repo, "config", "extensions.worktreeConfig", "true")
    gd = Path(_git(wt, "rev-parse", "--path-format=absolute", "--git-dir").strip())
    marker = tmp_path / "ran-wtconfig"
    script = tmp_path / "clean.sh"
    script.write_text(f"#!/bin/sh\necho x >> {marker}\ncat\n")
    script.chmod(0o755)
    (gd / "config.worktree").write_text(f'[filter "x"]\n\tclean = {script}\n')
    (wt / ".gitattributes").write_text("* filter=x\n")
    (wt / "a.txt").write_text("changed\n")
    os.utime(wt / "a.txt", (3, 3))
    subprocess.run(["git", "-C", str(wt), "diff", "HEAD"], capture_output=True)
    assert marker.exists(), "the control: git runs the child's config.worktree"
    marker.unlink()
    res = _read_all(d, repo, out, wt, sess)
    assert res.status == "refused" and "no longer belongs to" in res.why
    assert not marker.exists()
    # a symlink counts too
    (gd / "config.worktree").unlink()
    (gd / "config.worktree").symlink_to(tmp_path / "nowhere")
    assert _read_all(d, repo, out, wt, sess).status == "refused"
    assert not marker.exists()


def _run_with_fence(tmp_path, fence_words, envelope=None, environ=None):
    d, g = _root(tmp_path, tokens=1000, turns=10)
    repo = _repo(tmp_path)
    if envelope is not None:
        (g / "envelopes").mkdir(parents=True, exist_ok=True)
        (g / "envelopes" / "default.json").write_text(json.dumps(envelope))
    started = []

    def start(*a, **kw):
        started.append(kw["session"])
        return tree_run.NodeRun("failed", kw["session"], reasons=["x"])

    out = tree_branch.run_branches(
        d, g,
        **_args(d, g, repo, start=start, fence=["fence", *fence_words], environ=environ or {}),
    )  # fmt: skip
    return repo, out, started, d


@pytest.mark.parametrize("form", ["rw", "dir", "workdir", "eq"])
def test_a_branch_refuses_to_start_when_another_grant_covers_the_repo(tmp_path, form):
    repo = tmp_path / "repo"
    g = {
        "rw": ["--allow", f"{repo}:rw"],
        "dir": ["--allow", f"{tmp_path}:rw"],
        "workdir": ["--workdir", str(repo / ".git" / "hooks")],
        "eq": [f"--allow={repo}/.git:rw"],
    }[form]
    r, out, started, d = _run_with_fence(tmp_path, ["--harness", "sprig", *g])
    assert out.status == "refused" and len(out.reasons) == 1 and "covers" in out.reasons[0]
    assert started == [] and not (d / "tree" / "branches").exists()


@pytest.mark.parametrize("form", ["tilde", "workdir-home", "link", "relative", "link-envelope"])
def test_a_grant_is_read_as_the_fence_reads_it(tmp_path, form):
    """``~``, a link and a relative path reach the repository as the fence
    expands them: ``~`` is HOME, a relative path lies under the child's
    worktree, and Landlock grants the place a link points to."""
    repo = tmp_path / "repo"
    link = tmp_path / "plink"
    link.symlink_to(repo)
    # the worktree is <data>/tree/branches/<session>, four levels under tmp_path
    g, envelope = {
        "tilde": (["--allow", "~/repo:rw"], None),
        "workdir-home": (["--workdir=~"], None),
        "link": (["--allow", f"{link}:rw"], None),
        "relative": (["--allow", "../../../../repo/.git:rw"], None),
        "link-envelope": ([], {"file_write": [f"{link}/.git/**"]}),
    }[form]
    r, out, started, d = _run_with_fence(
        tmp_path, ["--harness", "sprig", *g], envelope=envelope, environ={"HOME": str(tmp_path)}
    )
    assert out.status == "refused" and "covers" in out.reasons[0], out.reasons
    assert started == [] and not (d / "tree" / "branches").exists()


@pytest.mark.parametrize("grant", ["ro", "inside"])
def test_a_grant_that_does_not_reach_the_git_directory_is_no_refusal(tmp_path, grant):
    repo = tmp_path / "repo"
    g = ["--allow", f"{repo}:ro", "--allow", str(repo / "x")]
    if grant == "inside":
        g = ["--allow", f"{repo}/src:rw"]
    r, out, started, d = _run_with_fence(tmp_path, ["--harness", "sprig", *g])
    assert started


def test_a_branch_refuses_to_start_when_the_envelope_writes_over_the_repo(tmp_path):
    repo = tmp_path / "repo"
    r, out, started, d = _run_with_fence(
        tmp_path, ["--harness", "sprig"], {"permissions": {"file_write": [f"{repo}/**"]}}
    )
    assert out.status == "refused" and "covers" in out.reasons[0] and started == []


def test_the_cli_exits_2_when_a_grant_covers_the_repo(tmp_path, fake, fence, monkeypatch):  # noqa: F811
    words, _ = fence
    d, g = _root(tmp_path, tokens=1000, turns=20)
    repo = _repo(tmp_path)
    monkeypatch.chdir(repo)
    monkeypatch.setenv("DAISUGI_SPRIG_FENCE", json.dumps([*words, "--allow", f"{repo}:rw"]))
    res = CliRunner().invoke(
        app,
        ["tree", "run", "--parent", "top", "--profile", "default", "--tokens", "200"]
        + ["--turns", "5", "--parallel", "2", "--sprig-fence", "--data-dir", str(d)]
        + ["--root", str(g), "-"],
        input=_req(attempts=2, why="operator"),
    )
    assert res.exit_code == 2, res.output


@pytest.mark.parametrize("why", ["weak-ipc", "reftable"])
def test_the_cli_exits_2_on_weak_ipc_or_a_reftable_repository(
    tmp_path, fake, fence, monkeypatch, why
):  # noqa: F811
    words, _ = fence
    d, g = _root(tmp_path, tokens=1000, turns=20)
    if why == "reftable":
        repo = tmp_path / "repo"
        subprocess.run(["git", "init", "-q", "--ref-format=reftable", str(repo)], check=True)
        (repo / "a.txt").write_text("one\n")
        _git(repo, "add", "a.txt")
        _git(repo, "commit", "-qm", "init")
        extra = []
    else:
        repo = _repo(tmp_path)
        extra = ["--allow-weak-ipc"]
    monkeypatch.chdir(repo)
    monkeypatch.setenv("DAISUGI_SPRIG_FENCE", json.dumps([*words, *extra]))
    res = CliRunner().invoke(
        app,
        ["tree", "run", "--parent", "top", "--profile", "default", "--tokens", "200"]
        + ["--turns", "5", "--parallel", "2", "--sprig-fence", "--data-dir", str(d)]
        + ["--root", str(g), "-"],
        input=_req(attempts=2, why="operator"),
    )
    assert res.exit_code == 2, res.output
    assert not (d / "tree" / "branches").exists()


def test_a_planted_reftable_directory_is_folded_away(tmp_path):
    from opendaisugi import branch_git

    d, repo, out = _ran(tmp_path)
    sess = out.leader_session
    base, common = tree_branch.branch_ref(d, sess)
    wt = tree_branch.branches_dir(d) / sess
    gd = Path(common) / "worktrees" / sess
    (gd / "objects").mkdir()
    (gd / "reftable").mkdir()
    assert branch_git.fold(common, wt, base)
    assert not (gd / "objects").exists() and not (gd / "reftable").exists()
    assert tree_branch.adopt(d, sess).status == "adopted"


def _writable(tmp_path):
    """Give the test back the rights a child took, so the tmp dir can go."""
    for root, dirs, _ in os.walk(tmp_path):
        for x in dirs:
            p = os.path.join(root, x)
            if not os.path.islink(p):
                os.chmod(p, 0o755)


def _admin_of(cwd):
    return subprocess.run(
        ["git", "-C", cwd, "rev-parse", "--path-format=absolute", "--git-dir"],
        check=True, capture_output=True, text=True,
    ).stdout.strip()  # fmt: skip


def _left(kind):
    """What a hostile child leaves in its admin directory after it committed."""

    def extra(cwd, session, common, env):
        admin = _admin_of(cwd)
        if kind == "store":
            import shutil

            shutil.rmtree(os.path.join(admin, "objects"))
        elif kind == "admin":
            os.chmod(admin, 0o555)
        elif kind == "logs-refs":
            subprocess.run(
                ["git", "-C", cwd, "update-ref", "refs/worktree/keep", "HEAD"],
                check=True, capture_output=True, env=env,
            )  # fmt: skip
            for part in ("logs", "refs/worktree", "refs"):
                os.chmod(os.path.join(admin, part), 0o555)
        elif kind == "reftable":
            os.mkdir(os.path.join(admin, "reftable"))
            os.chmod(os.path.join(admin, "reftable"), 0o000)

    return extra


@pytest.mark.parametrize("kind", ["store", "admin", "logs-refs", "reftable"])
def test_a_fenced_branch_is_folded_whatever_the_child_left(tmp_path, kind):
    d, g = _root(tmp_path, tokens=1000, turns=10)
    repo = _repo(tmp_path)
    try:
        out = tree_branch.run_branches(
            d, g, **_args(d, g, repo, start=_committing_start(_left(kind)), fence=["fence"])
        )
        # right after the run, while the branch worktrees exist
        _whole(repo)
        assert out.status == "ran", out.reasons
        _, common = tree_branch.branch_ref(d, out.leader_session)
        for b in out.branches:
            assert b["status"] == "ran", b
            gd = Path(common) / "worktrees" / b["session"]
            for part in ("objects", "logs", "refs", "reftable"):
                assert not os.path.lexists(gd / part), (b["session"], part)
        assert tree_branch.adopt(d, out.leader_session).status == "adopted"
        _whole(repo)
    finally:
        _writable(tmp_path)


def test_a_branch_whose_fold_fails_is_removed_and_not_ranked(tmp_path, monkeypatch):
    from opendaisugi import branch_git

    d, g = _root(tmp_path, tokens=1000, turns=10)
    repo = _repo(tmp_path)

    def lock(cwd, session, common, env):
        admin = _admin_of(cwd)
        Path(admin, "locked").write_text("mine\n")
        os.chmod(admin, 0o555)

    real = branch_git.fold
    calls: list[str] = []

    def fold(r, wt, b, *a, **k):
        calls.append(Path(wt).name)
        return False if len(calls) == 1 else real(r, wt, b, *a, **k)

    monkeypatch.setattr(branch_git, "fold", fold)
    try:
        out = tree_branch.run_branches(
            d, g, **_args(d, g, repo, start=_committing_start(lock), fence=["fence"])
        )
        _whole(repo)
        bad = [b for b in out.branches if b["session"] == calls[0]]
        assert len(bad) == 1 and bad[0]["status"] == "failed"
        _, common = tree_branch.branch_ref(d, bad[0]["session"])
        assert not os.path.lexists(Path(common) / "worktrees" / bad[0]["session"])
        assert not os.path.lexists(bad[0]["worktree"])
        listed = _git(repo, "worktree", "list", "--porcelain")
        assert bad[0]["worktree"] not in listed
        assert bad[0]["session"] not in out.ranking["order"]
    finally:
        _writable(tmp_path)


def test_a_reftable_repository_refuses_a_fenced_branch_run_up_front(tmp_path):
    d, g = _root(tmp_path, tokens=1000, turns=10)
    repo = tmp_path / "repo"
    subprocess.run(["git", "init", "-q", "--ref-format=reftable", str(repo)], check=True)
    (repo / "a.txt").write_text("one\n")
    _git(repo, "add", "a.txt")
    _git(repo, "commit", "-qm", "init")
    started = []

    def start(*a, **kw):
        started.append(kw["session"])
        return tree_run.NodeRun("failed", kw["session"], reasons=["x"])

    out = tree_branch.run_branches(d, g, **_args(d, g, repo, start=start, fence=["fence"]))
    assert out.status == "refused" and out.usage, out
    assert len(out.reasons) == 1 and "reftable" in out.reasons[0]
    assert "\n" not in out.reasons[0]
    assert started == [] and not (d / "tree" / "branches").exists()
    assert tree.read_tree(d).parent_rows("top") == 0


@pytest.mark.parametrize("word", ["--allow-weak-ipc", "--allow-weak-ipc=1"])
def test_a_branch_run_refuses_allow_weak_ipc(tmp_path, word):
    r, out, started, d = _run_with_fence(tmp_path, ["--harness", "sprig", word])
    assert out.status == "refused" and out.usage, out
    assert len(out.reasons) == 1 and "--allow-weak-ipc" in out.reasons[0]
    assert started == [] and not (d / "tree" / "branches").exists()
    assert tree.read_tree(d).parent_rows("top") == 0


@pytest.mark.parametrize("word", ["--no-fence", "--no-fence=1"])
def test_a_branch_run_refuses_no_fence(tmp_path, word):
    """R5-2: a branch fence that holds --no-fence confines nothing."""
    r, out, started, d = _run_with_fence(tmp_path, ["--harness", "sprig", word])
    assert out.status == "refused" and out.usage, out
    assert len(out.reasons) == 1 and "--no-fence" in out.reasons[0]
    assert started == [] and not (d / "tree" / "branches").exists()
    assert tree.read_tree(d).parent_rows("top") == 0


def test_prune_takes_back_an_admin_dir_the_child_locked(tmp_path):
    """R5-1: the clean reclaims rights before it removes a worktree."""
    d, g = _root(tmp_path, tokens=1000, turns=10)
    repo = _repo(tmp_path)
    out = tree_branch.run_branches(d, g, **_args(d, g, repo, start=_fake_start([], [0], fail=())))
    assert out.status == "ran" and out.choice_id
    common = _git(repo, "rev-parse", "--path-format=absolute", "--git-common-dir").strip()
    admins = [Path(common) / "worktrees" / Path(b["worktree"]).name for b in out.branches]
    for a in admins:
        assert a.is_dir()
        a.chmod(0o555)
    rank.append_rows(d, [{
        "choice_id": out.choice_id, "ranking_id": out.ranking_id, "event": "confirmed",
        "how": "drop", "ts": 1.0,
    }])  # fmt: skip
    try:
        assert tree_branch.prune_branches(d) == sorted(b["session"] for b in out.branches)
        assert not any(a.exists() for a in admins)
    finally:
        for a in admins:
            if a.exists():
                a.chmod(0o755)
