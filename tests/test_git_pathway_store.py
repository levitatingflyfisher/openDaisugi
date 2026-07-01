"""End-to-end tests for GitPathwayStore (v0.25)."""
from __future__ import annotations

import json
import subprocess
from pathlib import Path

import pytest

from opendaisugi.models import (
    ActionPlan,
    Envelope,
    Permission,
    ShellStep,
)
from opendaisugi.pathway import CompiledPathway

pytest.importorskip("cryptography")  # signing path required for most tests


def _bare_repo(tmp_path: Path) -> Path:
    """Create a bare git repo to act as the team registry's remote."""
    bare = tmp_path / "registry.git"
    subprocess.run(["git", "init", "--bare", str(bare)], check=True,
                   capture_output=True)
    return bare


def _clone(remote: Path, local: Path) -> None:
    subprocess.run(["git", "clone", str(remote), str(local)], check=True,
                   capture_output=True)


def _initial_commit(repo: Path, *, trusted_signers: dict[str, str] | None = None) -> None:
    """Seed the repo with trusted-signers.json + an empty pathways dir
    + an initial commit. Real registries run this once at setup.
    """
    (repo / "pathways").mkdir(exist_ok=True)
    (repo / "pathways" / ".gitkeep").write_text("")
    (repo / "trusted-signers.json").write_text(
        json.dumps(trusted_signers or {}, indent=2),
    )
    subprocess.run(["git", "-C", str(repo), "add", "."], check=True,
                   capture_output=True)
    # Use a real-looking author so commits succeed in fresh test envs
    subprocess.run([
        "git", "-C", str(repo), "-c", "user.email=test@example.com",
        "-c", "user.name=test", "commit", "-m", "init",
    ], check=True, capture_output=True)
    subprocess.run(["git", "-C", str(repo), "push"], check=True,
                   capture_output=True)


def _pathway() -> CompiledPathway:
    env = Envelope(generated_by="t", task="t",
                   permissions=Permission(shell=True, shell_allowlist=["echo"]))
    plan = ActionPlan(source="t", task="wash plates", steps=[
        ShellStep(id="s1", command="echo hi"),
    ])
    return CompiledPathway(
        id="pathway_demo",
        task_description="wash a plate",
        task_embedding=[0.1] * 4,
        embedding_model="test", embedding_model_version="3",
        envelope=env, plan_template=plan,
        source_trace_ids=["t1"], distilled_at=1000.0,
        structure_signature="shell",
    )


def test_publish_and_pull_roundtrip(tmp_path: Path):
    """Publisher A publishes; consumer B pulls and finds the pathway."""
    from opendaisugi.git_pathway_store import GitPathwayStore
    from opendaisugi.signing import generate_keypair

    priv_a, pub_a = generate_keypair()
    bare = _bare_repo(tmp_path)
    repo_a = tmp_path / "a"
    repo_b = tmp_path / "b"
    _clone(bare, repo_a)
    # Configure committer identity so 'git commit' doesn't refuse in CI
    subprocess.run(["git", "-C", str(repo_a), "config", "user.email", "a@test"],
                   check=True)
    subprocess.run(["git", "-C", str(repo_a), "config", "user.name", "a"],
                   check=True)
    _initial_commit(repo_a, trusted_signers={"alice": pub_a})

    _clone(bare, repo_b)
    subprocess.run(["git", "-C", str(repo_b), "config", "user.email", "b@test"],
                   check=True)
    subprocess.run(["git", "-C", str(repo_b), "config", "user.name", "b"],
                   check=True)
    subprocess.run(["git", "-C", str(repo_b), "pull"], check=True,
                   capture_output=True)

    a_store = GitPathwayStore(
        repo_path=repo_a,
        private_key_b64=priv_a, public_key_b64=pub_a,
        publisher="alice@dev",
    )
    bundle_hash = a_store.publish(_pathway())
    assert bundle_hash

    # Trust anchor is LOCAL / out-of-band (SGCM H5) — never the in-repo file.
    b_trust = tmp_path / "b_trusted_signers.json"
    b_trust.write_text(json.dumps({"alice": pub_a}))
    b_store = GitPathwayStore(repo_path=repo_b, trusted_signers_path=b_trust)
    new_count = b_store.pull()
    assert new_count == 1
    found = b_store.list_all()
    assert len(found) == 1
    assert found[0].id == "pathway_demo"


def test_pull_refuses_bundle_from_untrusted_signer(tmp_path: Path):
    """Untrusted-signer bundle is skipped on pull, not raised."""
    from opendaisugi.git_pathway_store import GitPathwayStore
    from opendaisugi.signing import generate_keypair

    priv_alice, pub_alice = generate_keypair()
    _, pub_other = generate_keypair()  # only this one is trusted
    bare = _bare_repo(tmp_path)
    repo_a = tmp_path / "a"
    repo_b = tmp_path / "b"
    _clone(bare, repo_a)
    subprocess.run(["git", "-C", str(repo_a), "config", "user.email", "a@test"],
                   check=True)
    subprocess.run(["git", "-C", str(repo_a), "config", "user.name", "a"],
                   check=True)
    # Only `other` is in the trusted-signers file; alice is NOT
    _initial_commit(repo_a, trusted_signers={"other": pub_other})

    a_store = GitPathwayStore(
        repo_path=repo_a,
        private_key_b64=priv_alice, public_key_b64=pub_alice,
        publisher="alice@dev",
    )
    a_store.publish(_pathway())

    _clone(bare, repo_b)
    b_store = GitPathwayStore(repo_path=repo_b)
    new_count = b_store.pull()
    assert new_count == 0
    assert b_store.list_all() == []


def test_publish_refuses_without_private_key(tmp_path: Path):
    """Publishing requires a signing key at construction."""
    from opendaisugi.git_pathway_store import GitPathwayStore

    bare = _bare_repo(tmp_path)
    repo = tmp_path / "a"
    _clone(bare, repo)
    subprocess.run(["git", "-C", str(repo), "config", "user.email", "a@test"],
                   check=True)
    subprocess.run(["git", "-C", str(repo), "config", "user.name", "a"],
                   check=True)
    _initial_commit(repo)

    store = GitPathwayStore(repo_path=repo)  # no key
    with pytest.raises(ValueError, match="private_key_b64"):
        store.publish(_pathway())


def test_status_returns_diagnostics(tmp_path: Path):
    from opendaisugi.git_pathway_store import GitPathwayStore

    bare = _bare_repo(tmp_path)
    repo = tmp_path / "a"
    _clone(bare, repo)
    subprocess.run(["git", "-C", str(repo), "config", "user.email", "a@test"],
                   check=True)
    subprocess.run(["git", "-C", str(repo), "config", "user.name", "a"],
                   check=True)
    _initial_commit(repo)
    store = GitPathwayStore(repo_path=repo, publisher="alice@dev")
    s = store.status()
    assert s["publisher"] == "alice@dev"
    assert s["signing_configured"] is False
    assert "head_commit" in s


def test_offline_ok_tolerates_pull_failure(tmp_path: Path):
    """When offline_ok=True, an unreachable remote doesn't fail the call."""
    from opendaisugi.git_pathway_store import GitPathwayStore

    bare = _bare_repo(tmp_path)
    repo = tmp_path / "a"
    _clone(bare, repo)
    subprocess.run(["git", "-C", str(repo), "config", "user.email", "a@test"],
                   check=True)
    subprocess.run(["git", "-C", str(repo), "config", "user.name", "a"],
                   check=True)
    _initial_commit(repo)

    # Break the remote
    subprocess.run(["git", "-C", str(repo), "remote", "set-url", "origin",
                    "/nonexistent/path/to/nowhere.git"], check=True)
    store = GitPathwayStore(repo_path=repo, offline_ok=True)
    # pull() warns but doesn't raise; returns 0 new
    assert store.pull() == 0


def test_in_repo_trusted_signers_file_is_not_a_trust_anchor(tmp_path: Path):
    # H5: an attacker who can push to the registry adds their key to the in-repo
    # trusted-signers.json — but that file must NOT be used as a trust anchor
    # (it's pulled from the same remote it authenticates). With no LOCAL anchor,
    # the attacker's signed bundle is rejected.
    from opendaisugi.git_pathway_store import GitPathwayStore
    from opendaisugi.signing import generate_keypair

    priv_evil, pub_evil = generate_keypair()
    bare = _bare_repo(tmp_path)
    repo_a = tmp_path / "a"; repo_b = tmp_path / "b"
    _clone(bare, repo_a)
    subprocess.run(["git", "-C", str(repo_a), "config", "user.email", "a@test"], check=True)
    subprocess.run(["git", "-C", str(repo_a), "config", "user.name", "a"], check=True)
    # The registry itself declares the attacker trusted (remote-controlled file).
    _initial_commit(repo_a, trusted_signers={"evil": pub_evil})
    a_store = GitPathwayStore(repo_path=repo_a, private_key_b64=priv_evil,
                              public_key_b64=pub_evil, publisher="evil@dev")
    a_store.publish(_pathway())

    _clone(bare, repo_b)
    subprocess.run(["git", "-C", str(repo_b), "config", "user.email", "b@test"], check=True)
    subprocess.run(["git", "-C", str(repo_b), "config", "user.name", "b"], check=True)
    # Consumer configures NO local anchor → the in-repo "evil is trusted" is ignored.
    b_store = GitPathwayStore(repo_path=repo_b)
    b_store.pull()
    assert b_store.list_all() == []  # attacker's self-trusted bundle rejected
