"""rank pivot: undo through the ledger, apply the alternative's patch, or
print the follow-up."""

from __future__ import annotations

import json
import subprocess
import time
from pathlib import Path

import pytest
from typer.testing import CliRunner

from opendaisugi import rank, rank_pivot
from opendaisugi.cli import app
from opendaisugi.journal import Journal
from opendaisugi.models import Receipt, ReversalHandle


def _git(*args: str, cwd: Path) -> None:
    subprocess.run(
        ["git", "-c", "user.name=t", "-c", "user.email=t@t", *args],
        cwd=cwd, check=True, capture_output=True,
    )  # fmt: skip


def _card(dd: Path, *, where=None, downstream=("s1", "s2", "s3"), run="run_a"):
    d = rank.rankings_dir(dd)
    d.mkdir(parents=True, exist_ok=True)
    row = {
        "choice_id": "ch_000000000001",
        "ranking_id": "r0",
        "event": "opened",
        "options": {
            "survivors": [{"id": "a", "content_hash": "h-a"}, {"id": "b", "content_hash": "h-b"}],
            "eliminated": [],
        },
        "chosen": "a",
        "status": "provisional",
        "facts": {"run": {"run_id": run, "downstream": list(downstream)}, "where": where or {}},
        "ts": time.time(),
    }
    (d / "choices.jsonl").write_text(json.dumps(row) + "\n")


def _receipts(dd: Path, revs: list[str], files: Path | None = None) -> None:
    j = Journal(data_dir=dd)
    for k, rev in enumerate(revs, start=1):
        handle = None
        if rev == "reversible" and files is not None:
            f = files / f"f{k}.txt"
            f.write_text("new")
            handle = ReversalHandle(kind="file_write", path=str(f), prior_existed=False)
        j.append_receipt(
            Receipt(
                step_id=f"s{k}",
                run_id="run_a",
                timestamp=float(k),
                evidence_hash="x",
                verify_result=True,
                effect_class="file_write",
                reversibility=rev,
                reversal=handle,
            )  # fmt: skip
        )
    j.close()


def test_plan_lists_the_reversible_receipts_newest_first(tmp_path):
    dd = tmp_path / "dd"
    _receipts(dd, ["reversible", "none", "reversible"])
    _card(dd)
    p = rank_pivot.plan(rank.read_cards(dd)[0], dd, "b")
    assert p["cost"] == "costly" and [u["step_id"] for u in p["undo"]] == ["s3", "s1"]
    assert p["skipped"] == []


def test_plan_writes_nothing(tmp_path):
    dd = tmp_path / "dd"
    _receipts(dd, ["reversible"])
    _card(dd)
    before = sorted(str(p) for p in dd.rglob("*"))
    rank_pivot.plan(rank.read_cards(dd)[0], dd, "b")
    assert sorted(str(p) for p in dd.rglob("*")) == before


def test_plan_refuses_the_chosen_and_an_unknown_attempt(tmp_path):
    dd = tmp_path / "dd"
    _card(dd)
    card = rank.read_cards(dd)[0]
    for bad in ("a", "z"):
        with pytest.raises(rank.RankError, match="not another attempt"):
            rank_pivot.plan(card, dd, bad)


def test_follow_up_only_applies_nothing(tmp_path):
    dd = tmp_path / "dd"
    _receipts(dd, ["irreversible"])
    _card(dd)
    card = rank.read_cards(dd)[0]
    p = rank_pivot.plan(card, dd, "b")
    assert p["cost"] == "follow_up_only"
    out = rank_pivot.apply(p, dd)
    assert out["applied"] is False and "Follow-up" in out["follow_up"]


def test_apply_undoes_then_stops_at_the_first_patch_failure_and_says_so(tmp_path):
    dd = tmp_path / "dd"
    files = tmp_path / "files"
    files.mkdir()
    _receipts(dd, ["reversible", "none", "reversible"], files)
    _card(
        dd,
        where={
            "a": {"kind": "worktree", "path": str(tmp_path / "wa"), "base": "a" * 40, "repo": "/r"},
            "b": {"kind": "worktree", "path": str(tmp_path / "wb"), "base": "a" * 40, "repo": "/r"},
        },
    )
    for w in ("wa", "wb"):
        (tmp_path / w).mkdir()
        (tmp_path / w / ".git").write_text(f"gitdir: /r/worktrees/{w}\n")
    card = rank.read_cards(dd)[0]
    calls = []

    def git(argv, **kw):
        calls.append(argv)
        name = Path(argv[argv.index("-C") + 1]).name

        class R:
            returncode = 1 if "apply" in argv else 0
            stdout = (
                b"/r\n"
                if "--git-common-dir" in argv
                else f"/r/worktrees/{name}\n".encode()
                if "--git-dir" in argv
                else b""
            )
            stderr = b"conflict"

        return R()

    out = rank_pivot.apply(rank_pivot.plan(card, dd, "b"), dd, git=git)
    assert out["applied"] is False and "conflict" in out["why"]
    assert any("apply" in c for c in calls)
    assert out["undone"] == ["s3", "s1"] and not (files / "f1.txt").exists()


def _two_worktrees(tmp_path):
    repo = tmp_path / "repo"
    repo.mkdir()
    _git("init", "-q", ".", cwd=repo)
    (repo / "a.txt").write_text("one\n")
    _git("add", "a.txt", cwd=repo)
    _git("commit", "-qm", "init", cwd=repo)
    wa, wb = tmp_path / "wa", tmp_path / "wb"
    _git("worktree", "add", "-q", "--detach", str(wa), "HEAD", cwd=repo)
    _git("worktree", "add", "-q", "--detach", str(wb), "HEAD", cwd=repo)
    dd = tmp_path / "dd"
    ref = {
        "base": subprocess.run(
            ["git", "-C", str(repo), "rev-parse", "HEAD"], capture_output=True, text=True
        ).stdout.strip(),
        "repo": str(repo / ".git"),
    }
    _card(
        dd,
        where={
            "a": {"kind": "worktree", "path": str(wa), **ref},
            "b": {"kind": "worktree", "path": str(wb), **ref},
        },
        downstream=(),
    )
    return dd, wa, wb


def test_apply_moves_a_patch_when_both_branches_edited_the_same_file(tmp_path):
    dd, wa, wb = _two_worktrees(tmp_path)
    (wa / "a.txt").write_text("chosen\n")  # the chosen worktree keeps its own edits
    (wa / "new_a.txt").write_text("only in a\n")
    (wb / "a.txt").write_text("alternative\n")
    card = rank.read_cards(dd)[0]
    out = rank_pivot.apply(rank_pivot.plan(card, dd, "b"), dd)
    assert out["applied"] is True, out
    assert (wa / "a.txt").read_text() == "alternative\n"
    assert not (wa / "new_a.txt").exists()


def test_a_failed_patch_puts_the_chosen_edits_back(tmp_path):
    dd, wa, wb = _two_worktrees(tmp_path)
    (wa / "a.txt").write_text("chosen\n")
    (wb / "a.txt").write_text("alternative\n")
    card = rank.read_cards(dd)[0]
    real = subprocess.run
    seen = []

    def git(argv, **kw):
        if "--3way" in argv:
            seen.append(argv)

            class R:
                returncode = 1
                stdout = b""
                stderr = b"conflict"

            return R()
        return real(argv, **kw)

    out = rank_pivot.apply(rank_pivot.plan(card, dd, "b"), dd, git=git)
    assert out["applied"] is False and "conflict" in out["why"] and "are back" in out["why"]
    assert (wa / "a.txt").read_text() == "chosen\n" and seen


def test_cli_pivot_prints_a_plan_and_apply_needs_the_flag(tmp_path):
    dd = tmp_path / "dd"
    _receipts(dd, ["reversible"])
    _card(dd)
    res = CliRunner().invoke(
        app,
        [
            "rank",
            "pivot",
            "--choice",
            "ch_000000000001",
            "--to",
            "b",
            "--data-dir",
            str(dd),
            "--json",
        ],
    )
    assert res.exit_code == 0, res.output
    assert json.loads(res.output)["plan"]["cost"] == "costly"
    res = CliRunner().invoke(
        app, ["rank", "pivot", "--choice", "ch_nope", "--to", "b", "--data-dir", str(dd)]
    )
    assert res.exit_code == 1 and "no choice" in res.output.lower()


def _commit(w, msg):
    subprocess.run(
        ["git", "-C", str(w), "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qam", msg],
        check=True, capture_output=True,
    )  # fmt: skip


def test_a_branch_that_committed_is_still_read_against_its_base(tmp_path):
    dd, wa, wb = _two_worktrees(tmp_path)
    (wa / "a.txt").write_text("chosen\n")
    _commit(wa, "the chosen child committed")  # HEAD moved off the base
    (wb / "a.txt").write_text("alternative\n")
    _commit(wb, "so did the other")
    card = rank.read_cards(dd)[0]
    out = rank_pivot.apply(rank_pivot.plan(card, dd, "b"), dd)
    assert out["applied"] is True, out
    assert (wa / "a.txt").read_text() == "alternative\n"


def test_a_real_conflict_is_reported_and_the_chosen_tree_is_left_as_it_was(tmp_path):
    dd, wa, wb = _two_worktrees(tmp_path)
    (wa / "a.txt").write_text("chosen\n")
    (wa / "only_a.txt").write_text("only in a\n")
    (wb / "a.txt").write_text("alternative\n")
    card = rank.read_cards(dd)[0]
    plan = rank_pivot.plan(card, dd, "b")
    real = subprocess.run

    def git(argv, **kw):
        r = real(argv, **kw)
        if "-R" in argv and "apply" in argv:
            # a concurrent edit lands between the two applies, so the
            # alternative's patch meets a line it cannot merge
            (wa / "a.txt").write_text("a clash\n")
        return r

    out = rank_pivot.apply(plan, dd, git=git)
    assert out["applied"] is False and "git apply failed" in out["why"]
    assert "are back" in out["why"]
    unmerged = real(
        ["git", "-C", str(wa), "diff", "--name-only", "--diff-filter=U"],
        capture_output=True, text=True,
    ).stdout  # fmt: skip
    assert unmerged == "" and "<<<<<<<" not in (wa / "a.txt").read_text()
    assert (wa / "a.txt").read_text() == "chosen\n"
    assert (wa / "only_a.txt").read_text() == "only in a\n"


def test_a_failed_patch_does_not_remove_a_file_the_chosen_tree_already_had(tmp_path):
    dd, wa, wb = _two_worktrees(tmp_path)
    (wa / ".git_ignore_probe").write_text("")
    # An ignored file in the chosen tree that the alternative adds by force.
    exclude = subprocess.run(
        ["git", "-C", str(wa), "rev-parse", "--git-path", "info/exclude"],
        capture_output=True, text=True,
    ).stdout.strip()  # fmt: skip
    ex = Path(exclude) if Path(exclude).is_absolute() else wa / exclude
    ex.parent.mkdir(parents=True, exist_ok=True)
    ex.write_text("x.log\n")
    (wa / "a.txt").write_text("chosen\n")
    (wa / "x.log").write_text("the chosen tree's own\n")
    (wb / "x.log").write_text("the alternative's\n")
    _git("add", "-f", "x.log", cwd=wb)
    card = rank.read_cards(dd)[0]
    out = rank_pivot.apply(rank_pivot.plan(card, dd, "b"), dd)
    assert out["applied"] is False, out
    assert (wa / "x.log").read_text() == "the chosen tree's own\n"
    assert (wa / "a.txt").read_text() == "chosen\n"


def test_a_config_and_a_hook_the_child_wrote_do_not_run_under_a_pivot(tmp_path):
    dd, wa, wb = _two_worktrees(tmp_path)
    repo = tmp_path / "repo"
    marker = tmp_path / "ran"
    script = tmp_path / "hook.sh"
    script.write_text(f"#!/bin/sh\necho x >> {marker}\n")
    script.chmod(0o755)
    _git("config", "core.fsmonitor", str(script), cwd=repo)
    hook = repo / ".git" / "hooks" / "post-checkout"
    hook.write_text(f"#!/bin/sh\necho x >> {marker}\n")
    hook.chmod(0o755)
    subprocess.run(["git", "-C", str(wa), "status"], capture_output=True)
    _git("checkout", "HEAD", "--", "a.txt", cwd=wa)
    assert marker.exists(), "the control: an unguarded git runs the child's config and hook"
    marker.unlink()
    (wa / "a.txt").write_text("chosen\n")
    (wb / "a.txt").write_text("alternative\n")
    card = rank.read_cards(dd)[0]
    plan = rank_pivot.plan(card, dd, "b")
    real = subprocess.run

    def git(argv, **kw):
        r = real(argv, **kw)
        if "-R" in argv and "apply" in argv:
            (wa / "a.txt").write_text("a clash\n")  # forces the restore path
        return r

    out = rank_pivot.apply(plan, dd, git=git)
    assert out["applied"] is False
    assert not marker.exists()


def test_dot_git_at_a_siblings_git_dir_is_refused_by_a_pivot(tmp_path):
    dd, wa, wb = _two_worktrees(tmp_path)
    repo = tmp_path / "repo"
    sgd = subprocess.run(
        ["git", "-C", str(wb), "rev-parse", "--path-format=absolute", "--git-dir"],
        capture_output=True, text=True,
    ).stdout.strip()  # fmt: skip
    (wa / "evil.txt").write_text("evil\n")
    (wa / ".git").write_text(f"gitdir: {sgd}\n")
    before = (Path(sgd) / "index").read_bytes()
    card = rank.read_cards(dd)[0]
    out = rank_pivot.apply(rank_pivot.plan(card, dd, "b"), dd)
    assert out["applied"] is False and "no longer belongs" in out["why"]
    assert (Path(sgd) / "index").read_bytes() == before
    assert repo.exists()


def test_a_staged_clash_reaches_the_three_way_merge_and_leaves_no_new_file_behind(tmp_path):
    dd, wa, wb = _two_worktrees(tmp_path)
    (wa / "a.txt").write_text("chosen\n")
    (wb / "a.txt").write_text("alternative\n")
    (wb / "only_b.txt").write_text("only in b\n")  # the alternative's own new file
    card = rank.read_cards(dd)[0]
    plan = rank_pivot.plan(card, dd, "b")
    real = subprocess.run
    reached = []

    def git(argv, **kw):
        r = real(argv, **kw)
        if "-R" in argv and "apply" in argv:
            # a concurrent edit, staged, lands between the two applies: the
            # alternative's patch now meets a line it can only merge
            (wa / "a.txt").write_text("a clash\n")
            _git("add", "a.txt", cwd=wa)
        if "--3way" in argv:
            reached.append(r.returncode)
        return r

    out = rank_pivot.apply(plan, dd, git=git)
    assert reached == [1], "the apply must reach the merge and fail there"
    assert out["applied"] is False and "are back" in out["why"]
    assert (wa / "a.txt").read_text() == "chosen\n"
    assert not (wa / "only_b.txt").exists()
    unmerged = real(
        ["git", "-C", str(wa), "diff", "--name-only", "--diff-filter=U"],
        capture_output=True, text=True,
    ).stdout  # fmt: skip
    assert unmerged == ""


def test_a_nested_repository_the_child_made_runs_no_program_under_a_pivot(tmp_path):
    import os

    dd, wa, wb = _two_worktrees(tmp_path)
    marker = tmp_path / "ran-nested"
    script = tmp_path / "clean.sh"
    script.write_text(f"#!/bin/sh\necho x >> {marker}\ncat\n")
    script.chmod(0o755)
    sub = wa / "sub"
    sub.mkdir()
    _git("init", "-q", ".", cwd=sub)
    (sub / "s.txt").write_text("aaaa\n")
    (sub / ".gitattributes").write_text("* filter=x\n")
    _git("add", "-A", cwd=sub)
    _git("commit", "-qm", "n", cwd=sub)
    _git("config", "filter.x.clean", str(script), cwd=sub)
    _git("add", "sub", cwd=wa)
    (sub / "s.txt").write_text("bbbb\n")
    os.utime(sub / "s.txt", (1, 1))
    ref = subprocess.run(
        ["git", "-C", str(tmp_path / "repo"), "rev-parse", "HEAD"], capture_output=True, text=True
    ).stdout.strip()
    subprocess.run(["git", "-C", str(wa), "diff", "--name-only", ref], capture_output=True)
    assert marker.exists(), "the control: an unguarded diff runs the nested filter"
    marker.unlink()
    os.utime(sub / "s.txt", (7, 7))
    (sub / "s.txt").write_text("cccc\n")
    (wa / "a.txt").write_text("chosen\n")
    (wb / "a.txt").write_text("alternative\n")
    card = rank.read_cards(dd)[0]
    out = rank_pivot.apply(rank_pivot.plan(card, dd, "b"), dd)
    assert out["applied"] is True, out
    assert not marker.exists()


def test_a_dangling_symlink_the_chosen_tree_had_is_kept_by_a_failed_pivot(tmp_path):
    dd, wa, wb = _two_worktrees(tmp_path)
    (wa / "a.txt").write_text("chosen\n")
    (wa / "dangling").symlink_to(tmp_path / "nowhere")
    (wb / "a.txt").write_text("alternative\n")
    (wb / "dangling").write_text("the alternative's\n")
    _git("add", "-f", "dangling", cwd=wb)
    exclude = subprocess.run(
        ["git", "-C", str(wa), "rev-parse", "--git-path", "info/exclude"],
        capture_output=True, text=True,
    ).stdout.strip()  # fmt: skip
    ex = Path(exclude) if Path(exclude).is_absolute() else wa / exclude
    ex.parent.mkdir(parents=True, exist_ok=True)
    ex.write_text("dangling\n")
    card = rank.read_cards(dd)[0]
    plan = rank_pivot.plan(card, dd, "b")
    real = subprocess.run

    def git(argv, **kw):
        r = real(argv, **kw)
        if "-R" in argv and "apply" in argv:
            (wa / "a.txt").write_text("a clash\n")
            _git("add", "a.txt", cwd=wa)
        return r

    out = rank_pivot.apply(plan, dd, git=git)
    assert out["applied"] is False, out
    assert (wa / "dangling").is_symlink()


def test_a_pivot_reads_a_child_that_committed_into_its_own_store(tmp_path):
    import os

    dd, wa, wb = _two_worktrees(tmp_path)
    repo = tmp_path / "repo"
    from opendaisugi import tree_run

    common = str((repo / ".git").resolve())
    env = {**os.environ}
    for w in (wa, wb):
        env2 = tree_run.branch_env(env, w.name, common)
        (w / "a.txt").write_text(f"{w.name} edit\n")
        (w / f"new_{w.name}.txt").write_text("n\n")
        subprocess.run(["git", "-C", str(w), "add", "-A"], check=True, env=env2)
        subprocess.run(
            ["git", "-C", str(w), "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "c"],
            check=True, env=env2, capture_output=True,
        )  # fmt: skip
    assert any((repo / ".git" / "worktrees" / "wa" / "objects").glob("??/*"))
    card = rank.read_cards(dd)[0]
    # A store that was not folded away is not read.
    out = rank_pivot.apply(rank_pivot.plan(card, dd, "b"), dd)
    assert out["applied"] is False and "no longer belongs" in out["why"], out
    from opendaisugi import branch_git

    base = subprocess.run(
        ["git", "-C", str(repo), "rev-parse", "HEAD"], capture_output=True, text=True
    ).stdout.strip()
    for w in (wa, wb):
        assert branch_git.fold(common, w, base)
    out = rank_pivot.apply(rank_pivot.plan(card, dd, "b"), dd)
    assert out["applied"] is True, out
    assert (wa / "a.txt").read_text() == "wb edit\n"
    assert (wa / "new_wb.txt").exists() and not (wa / "new_wa.txt").exists()
    # Every blob the pivot staged is in the shared store, and the repository
    # stays whole.
    staged = subprocess.run(
        ["git", "-C", str(wa), "ls-files", "-s"], capture_output=True, text=True, check=True
    ).stdout.split("\n")
    for line in filter(None, staged):
        oid = line.split()[1]
        assert subprocess.run(
            ["git", "--git-dir", common, "cat-file", "-e", oid], capture_output=True
        ).returncode == 0, line  # fmt: skip
    for cmd in (["fsck"], ["gc", "-q"], ["log", "--all", "--oneline"], ["rev-list", "--all"]):
        assert subprocess.run(["git", "-C", str(repo), *cmd], capture_output=True).returncode == 0
