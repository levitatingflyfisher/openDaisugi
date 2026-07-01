"""Tests for PathwayBundle (v0.25)."""
from __future__ import annotations

import pytest

from opendaisugi.models import (
    ActionPlan,
    Envelope,
    Permission,
    ShellStep,
)
from opendaisugi.pathway import CompiledPathway
from opendaisugi.pathway_bundle import (
    InvalidSignatureError,
    UnsignedBundleError,
    UntrustedSignerError,
    bundle_to_pathway,
    pathway_to_bundle,
)


def _pathway(structure_signature: str = "shell→file_read") -> CompiledPathway:
    env = Envelope(generated_by="t", task="t",
                   permissions=Permission(shell=True, shell_allowlist=["echo"]))
    plan = ActionPlan(source="t", task="t", steps=[
        ShellStep(id="s1", command="echo hi"),
    ])
    return CompiledPathway(
        id="pathway_test",
        task_description="test",
        task_embedding=[0.1] * 4,
        embedding_model="test-model",
        embedding_model_version="3",
        envelope=env,
        plan_template=plan,
        source_trace_ids=["t1", "t2", "t3"],
        distilled_at=1000.0,
        structure_signature=structure_signature,
    )


def test_unsigned_bundle_roundtrips_with_require_signed_false():
    p = _pathway()
    b = pathway_to_bundle(p, publisher="dev@home")
    assert b.signature_b64 is None
    assert b.signer_pubkey_b64 is None
    out = bundle_to_pathway(b, require_signed=False)
    assert out.id == p.id


def test_unsigned_bundle_refused_by_default():
    p = _pathway()
    b = pathway_to_bundle(p, publisher="dev@home")
    with pytest.raises(UnsignedBundleError):
        bundle_to_pathway(b)


def test_signed_bundle_roundtrips():
    pytest.importorskip("cryptography")
    from opendaisugi.signing import generate_keypair
    priv, pub = generate_keypair()
    p = _pathway()
    b = pathway_to_bundle(p, publisher="alice@team",
                          private_key_b64=priv, public_key_b64=pub)
    assert b.signature_b64 is not None
    assert b.signer_pubkey_b64 == pub
    # Verifies with matching trusted-signer set
    out = bundle_to_pathway(b, trusted_pubkey_b64s={pub})
    assert out.id == p.id


def test_signed_bundle_refused_by_untrusted_consumer():
    pytest.importorskip("cryptography")
    from opendaisugi.signing import generate_keypair
    priv, pub = generate_keypair()
    _, other_pub = generate_keypair()
    b = pathway_to_bundle(_pathway(), publisher="alice@team",
                          private_key_b64=priv, public_key_b64=pub)
    # Consumer trusts only `other_pub`, not Alice's pub
    with pytest.raises(UntrustedSignerError):
        bundle_to_pathway(b, trusted_pubkey_b64s={other_pub})


def test_tampered_bundle_fails_verification():
    pytest.importorskip("cryptography")
    from opendaisugi.signing import generate_keypair
    priv, pub = generate_keypair()
    b = pathway_to_bundle(_pathway(), publisher="alice@team",
                          private_key_b64=priv, public_key_b64=pub)
    # Mutate the publisher field after signing — signature no longer
    # covers the new bytes.
    b = b.model_copy(update={"publisher": "mallory@evil"})
    with pytest.raises(InvalidSignatureError):
        bundle_to_pathway(b, trusted_pubkey_b64s={pub})


def test_bundle_hash_is_content_addressed():
    p = _pathway()
    b1 = pathway_to_bundle(p, publisher="alice", published_at=1000.0)
    b2 = pathway_to_bundle(p, publisher="alice", published_at=1000.0)
    assert b1.bundle_hash == b2.bundle_hash
    # Different publisher → different hash
    b3 = pathway_to_bundle(p, publisher="bob", published_at=1000.0)
    assert b3.bundle_hash != b1.bundle_hash


def test_bundle_carries_structure_signature_for_indexing():
    p = _pathway(structure_signature="approach_dish→locate_rim→begin_scrub")
    b = pathway_to_bundle(p, publisher="alice")
    assert b.structure_signature == "approach_dish→locate_rim→begin_scrub"


def test_bundle_format_version_present_for_forward_compat():
    """Future v0.26+ bundles may add fields; this version field is the
    handshake. Pydantic extra='allow' lets unknown fields through."""
    b = pathway_to_bundle(_pathway(), publisher="alice")
    assert b.bundle_format_version == 1


def test_signing_requires_both_keys_or_neither():
    p = _pathway()
    with pytest.raises(ValueError, match="public_key_b64"):
        pathway_to_bundle(p, publisher="alice", private_key_b64="abc")


def test_signed_bundle_rejected_without_trusted_set():
    # M4: a signed bundle with no trusted_pubkey_b64s must be REJECTED — verifying
    # against its own embedded key proves nothing (any attacker key self-verifies).
    pytest.importorskip("cryptography")
    from opendaisugi.signing import generate_keypair
    priv, pub = generate_keypair()
    p = _pathway()
    b = pathway_to_bundle(p, publisher="evil@home", private_key_b64=priv, public_key_b64=pub)
    assert b.signature_b64 is not None
    with pytest.raises(UntrustedSignerError):
        bundle_to_pathway(b)  # trusted_pubkey_b64s=None default
    # with the real key trusted, it verifies
    assert bundle_to_pathway(b, trusted_pubkey_b64s={pub}).id == p.id
