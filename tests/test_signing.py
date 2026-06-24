"""Ed25519 contract signing + trusted-signer registry tests (v0.15.0)."""

from __future__ import annotations

from pathlib import Path

import pytest

from opendaisugi.contracts import Contract, verify_delegation
from opendaisugi.models import Envelope, Permission
from opendaisugi.signing import (
    TrustedSignerRegistry,
    canonicalize_contract,
    generate_keypair,
    sign_contract,
    verify_signature_raw,
)


def _unsigned_contract(skill_id: str = "echo-skill") -> Contract:
    return Contract(
        contract_id=f"c_{skill_id}",
        skill_id=skill_id,
        version="0.1.0",
        envelope=Envelope(
            generated_by=skill_id,
            task="skill task",
            permissions=Permission(shell=True, shell_allowlist=["echo"]),
        ),
        guarantees=["safe"],
    )


def test_generate_keypair_returns_distinct_b64_strings():
    priv, pub = generate_keypair()
    assert isinstance(priv, str) and isinstance(pub, str)
    assert priv != pub
    assert len(priv) > 20 and len(pub) > 20


def test_canonicalize_excludes_signature_and_signer():
    c = _unsigned_contract()
    unsigned_bytes = canonicalize_contract(c)
    c.signature = "AAAA"
    c.signer = "alice"
    signed_bytes = canonicalize_contract(c)
    assert unsigned_bytes == signed_bytes


def test_sign_then_verify_roundtrip():
    priv, pub = generate_keypair()
    c = _unsigned_contract()
    c.signature = sign_contract(c, priv)
    assert verify_signature_raw(c, pub) is True


def test_verify_fails_on_tampered_body():
    priv, pub = generate_keypair()
    c = _unsigned_contract()
    c.signature = sign_contract(c, priv)
    c.skill_id = "evil-skill"
    assert verify_signature_raw(c, pub) is False


def test_verify_fails_with_wrong_public_key():
    priv1, _pub1 = generate_keypair()
    _priv2, pub2 = generate_keypair()
    c = _unsigned_contract()
    c.signature = sign_contract(c, priv1)
    assert verify_signature_raw(c, pub2) is False


def test_verify_on_unsigned_contract_returns_false():
    _priv, pub = generate_keypair()
    c = _unsigned_contract()
    assert verify_signature_raw(c, pub) is False


def test_verify_on_malformed_signature_returns_false():
    _priv, pub = generate_keypair()
    c = _unsigned_contract()
    c.signature = "not-valid-base64-!!!"
    assert verify_signature_raw(c, pub) is False


def test_registry_load_nonexistent_path_returns_empty(tmp_path: Path):
    reg = TrustedSignerRegistry.load(tmp_path / "missing.json")
    assert reg.names() == []


def test_registry_roundtrip_save_load(tmp_path: Path):
    p = tmp_path / "trust.json"
    reg = TrustedSignerRegistry.load(p)
    _priv, pub = generate_keypair()
    reg.add("robin-v1", pub)
    reg.save()

    reloaded = TrustedSignerRegistry.load(p)
    assert reloaded.names() == ["robin-v1"]
    assert reloaded.get("robin-v1") == pub


def test_registry_remove_returns_true_for_known_name(tmp_path: Path):
    reg = TrustedSignerRegistry.load(tmp_path / "r.json")
    reg.add("a", "AAAA")
    assert reg.remove("a") is True
    assert reg.remove("a") is False


def test_registry_rejects_non_dict_json(tmp_path: Path):
    p = tmp_path / "bad.json"
    p.write_text("[]", encoding="utf-8")
    with pytest.raises(ValueError):
        TrustedSignerRegistry.load(p)


def test_registry_verify_against_known_signer(tmp_path: Path):
    priv, pub = generate_keypair()
    reg = TrustedSignerRegistry.load(tmp_path / "r.json")
    reg.add("robin-v1", pub)
    c = _unsigned_contract()
    c.signature = sign_contract(c, priv)
    assert reg.verify(c, ["robin-v1"]) is True


def test_registry_verify_unknown_signer_returns_false(tmp_path: Path):
    priv, pub = generate_keypair()
    reg = TrustedSignerRegistry.load(tmp_path / "r.json")
    reg.add("robin-v1", pub)
    c = _unsigned_contract()
    c.signature = sign_contract(c, priv)
    assert reg.verify(c, ["unknown-signer"]) is False


def test_registry_verify_multiple_signers_any_match(tmp_path: Path):
    """verify() is a disjunction: any named signer matching is enough."""
    priv_a, pub_a = generate_keypair()
    _priv_b, pub_b = generate_keypair()
    reg = TrustedSignerRegistry.load(tmp_path / "r.json")
    reg.add("a", pub_a)
    reg.add("b", pub_b)
    c = _unsigned_contract()
    c.signature = sign_contract(c, priv_a)
    assert reg.verify(c, ["b", "a"]) is True


def test_delegation_accepts_signed_contract_with_trusted_registry(tmp_path: Path):
    """End-to-end: signed contract + matching registry ⇒ delegation allowed."""
    priv, pub = generate_keypair()
    reg = TrustedSignerRegistry.load(tmp_path / "r.json")
    reg.add("robin-v1", pub)

    caller = Envelope(
        generated_by="orchestrator",
        task="orchestrate",
        permissions=Permission(shell=True, shell_allowlist=["echo", "ls", "pytest"]),
    )
    c = _unsigned_contract()
    c.signer = "robin-v1"
    c.signature = sign_contract(c, priv)

    d = verify_delegation(
        caller, c,
        trusted_signers=["robin-v1"],
        signer_registry=reg,
    )
    assert d.signature_valid is True
    assert d.allowed is True


def test_delegation_rejects_tampered_signed_contract(tmp_path: Path):
    """Body modification after signing ⇒ signature fails ⇒ delegation rejected."""
    priv, pub = generate_keypair()
    reg = TrustedSignerRegistry.load(tmp_path / "r.json")
    reg.add("robin-v1", pub)

    caller = Envelope(
        generated_by="orchestrator",
        task="orchestrate",
        permissions=Permission(shell=True, shell_allowlist=["echo"]),
    )
    c = _unsigned_contract()
    c.signature = sign_contract(c, priv)
    c.skill_id = "malicious-rebind"

    d = verify_delegation(
        caller, c,
        trusted_signers=["robin-v1"],
        signer_registry=reg,
    )
    assert d.signature_valid is False
    assert d.allowed is False


def test_delegation_unsigned_contract_still_works_without_trust_config():
    """Unsigned contract: signature_valid is None; delegation decided on subsumption."""
    caller = Envelope(
        generated_by="orchestrator",
        task="orchestrate",
        permissions=Permission(shell=True, shell_allowlist=["echo"]),
    )
    c = _unsigned_contract()
    d = verify_delegation(caller, c)
    assert d.signature_valid is None
    assert d.allowed is True
