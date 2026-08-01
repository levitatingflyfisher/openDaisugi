"""F-1/F-2 (surfaced by the multi-client campaign): file scopes were
right-anchored via PurePosixPath.match, so a relative glob admitted any
suffix-matching path — including absolute ones. An authored envelope with
file_write=['out.txt'] admitted a plan writing /etc/cron.d/out.txt: a
fail-open scope escape of the same class as G-4. The matcher is now
left-anchored and /-aware, implemented natively (no pathlib delegation, so
it is also no longer Python-version-dependent — F-2)."""

from opendaisugi.models import ActionPlan, Envelope, FileWriteStep, Permission
from opendaisugi.verify import _path_matches_any, verify


def test_relative_glob_does_not_admit_absolute_path():
    # the F-1 fail-open: a bare relative filename must not match an absolute path
    assert not _path_matches_any("/etc/cron.d/out.txt", ["out.txt"])
    assert not _path_matches_any("/etc/passwd", ["passwd"])
    assert not _path_matches_any("/abs/x.py", ["*.py"])


def test_authored_envelope_rejects_absolute_escape_end_to_end():
    env = Envelope(
        generated_by="human-authored", task="write a local file", stakes="low",
        permissions=Permission(file_write=["out.txt"]),
    )
    plan = ActionPlan(
        source="test", task="t",
        steps=[FileWriteStep(id="s1", path="/etc/cron.d/out.txt", content="evil")],
    )
    result = verify(plan, env)
    assert not result.ok  # previously ok=True — the scope escape
    assert any(v.stage == "permissions" for v in result.violations)


def test_star_stays_within_one_segment():
    assert _path_matches_any("x.py", ["*.py"])          # single segment: matches
    assert not _path_matches_any("sub/x.py", ["*.py"])  # * does not cross '/'


def test_legitimate_scopes_still_match():
    assert _path_matches_any("out/summary.md", ["./out/**"])
    assert _path_matches_any("notes.md", ["./**"])
    assert _path_matches_any("out.txt", ["out.txt"])
    assert _path_matches_any("/tmp/x", ["/tmp/**"])
    assert _path_matches_any("/etc/passwd", ["/**"])       # root everything
    assert _path_matches_any("a/b/c/x.py", ["a/**/x.py"])  # ** spans segments
    assert not _path_matches_any("out/sub/x.txt", ["out/*"])
