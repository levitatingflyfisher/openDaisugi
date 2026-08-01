"""Tests for signed release manifests (roadmap Stage 7: release-artifact signing)."""

from __future__ import annotations

from pathlib import Path

from opendaisugi.release import (
    ReleaseVerification,
    build_manifest,
    canonicalize_manifest,
    sha256_file,
    sign_manifest,
    verify_artifacts,
    verify_manifest_signature,
    verify_release,
)
from opendaisugi.signing import TrustedSignerRegistry, generate_keypair


def _write(p: Path, content: bytes) -> Path:
    p.write_bytes(content)
    return p


def test_sha256_file_known_vector(tmp_path):
    f = _write(tmp_path / "a.txt", b"hello\n")
    assert sha256_file(f) == ("5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03")


def test_build_manifest_hashes_and_sorts(tmp_path):
    _write(tmp_path / "z.whl", b"wheel")
    _write(tmp_path / "a.tar.gz", b"sdist")
    manifest = build_manifest(
        [tmp_path / "z.whl", tmp_path / "a.tar.gz"],
        version="0.39.0",
        created_at="2026-07-26T00:00:00Z",
    )
    assert manifest["version"] == "0.39.0"
    assert manifest["signature"] is None
    names = [a["name"] for a in manifest["artifacts"]]
    assert names == ["a.tar.gz", "z.whl"]  # sorted by name
    assert manifest["artifacts"][1]["size"] == len(b"wheel")
    assert manifest["artifacts"][1]["sha256"] == sha256_file(tmp_path / "z.whl")


def test_canonicalize_excludes_signature_and_is_stable(tmp_path):
    _write(tmp_path / "a.whl", b"x")
    m = build_manifest([tmp_path / "a.whl"], version="1", created_at="t")
    before = canonicalize_manifest(m)
    m2 = dict(m)
    m2["signature"] = "AAAA"
    assert canonicalize_manifest(m2) == before  # signature not part of signed body


def test_sign_and_verify_roundtrip_and_tamper(tmp_path):
    priv, pub = generate_keypair()
    _write(tmp_path / "a.whl", b"payload")
    m = build_manifest([tmp_path / "a.whl"], version="0.39.0", created_at="t")
    signed = sign_manifest(m, priv, signer="release-bot")
    assert signed["signer"] == "release-bot"
    assert verify_manifest_signature(signed, pub) is True

    tampered = dict(signed)
    tampered["version"] = "9.9.9"
    assert verify_manifest_signature(tampered, pub) is False


def test_verify_artifacts_detects_mismatch_and_missing(tmp_path):
    _write(tmp_path / "a.whl", b"orig")
    _write(tmp_path / "b.whl", b"keep")
    m = build_manifest([tmp_path / "a.whl", tmp_path / "b.whl"], version="1", created_at="t")

    assert verify_artifacts(m, tmp_path) == []  # all present + matching

    _write(tmp_path / "a.whl", b"TAMPERED")  # content changed
    (tmp_path / "b.whl").unlink()  # removed
    bad = verify_artifacts(m, tmp_path)
    assert set(bad) == {"a.whl", "b.whl"}


def test_verify_release_happy_path(tmp_path):
    priv, pub = generate_keypair()
    _write(tmp_path / "a.whl", b"payload")
    m = build_manifest([tmp_path / "a.whl"], version="0.39.0", created_at="t")
    signed = sign_manifest(m, priv, signer="release-bot")

    reg = TrustedSignerRegistry(path=tmp_path / "reg.json", _entries={})
    reg.add("release-bot", pub)

    v = verify_release(signed, artifact_dir=tmp_path, registry=reg, signer_names=["release-bot"])
    assert isinstance(v, ReleaseVerification)
    assert v.ok is True
    assert v.signature_ok is True
    assert v.artifact_mismatches == []


def test_verify_release_rejects_tampered_artifact(tmp_path):
    priv, pub = generate_keypair()
    _write(tmp_path / "a.whl", b"payload")
    m = build_manifest([tmp_path / "a.whl"], version="0.39.0", created_at="t")
    signed = sign_manifest(m, priv, signer="release-bot")
    reg = TrustedSignerRegistry(path=tmp_path / "reg.json", _entries={})
    reg.add("release-bot", pub)

    _write(tmp_path / "a.whl", b"TAMPERED")  # signature still valid, artifact is not
    v = verify_release(signed, artifact_dir=tmp_path, registry=reg, signer_names=["release-bot"])
    assert v.ok is False
    assert v.signature_ok is True
    assert v.artifact_mismatches == ["a.whl"]


def test_verify_release_rejects_unknown_signer(tmp_path):
    priv, _pub = generate_keypair()
    _write(tmp_path / "a.whl", b"payload")
    m = build_manifest([tmp_path / "a.whl"], version="0.39.0", created_at="t")
    signed = sign_manifest(m, priv, signer="release-bot")
    reg = TrustedSignerRegistry(path=tmp_path / "reg.json", _entries={})  # empty — nobody trusted

    v = verify_release(signed, artifact_dir=tmp_path, registry=reg, signer_names=["release-bot"])
    assert v.ok is False
    assert v.signature_ok is False
