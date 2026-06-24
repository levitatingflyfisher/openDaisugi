"""PathwayBundle — content-addressed, signed pathway transport unit (v0.25+).

A bundle is the on-disk representation of a CompiledPathway suitable for
exchange between opendaisugi instances. Bundles are:

- **Content-addressed** — ``bundle_hash`` is sha256 of the canonical-JSON
  encoding of (pathway, publisher, published_at). Two instances publishing
  the same pathway with the same metadata produce the same hash; the
  registry / git tree de-duplicates by filename.
- **Signed** — ``signature`` is an ed25519 signature over the canonical
  bundle bytes (machinery from v0.15). The publisher's public key must
  appear in the consumer's trusted-signers list or the bundle is refused.
- **Forward-compatible** — ``bundle_format_version`` lets future versions
  add fields without breaking existing clients. Pydantic ``extra='allow'``
  silently ignores unknown fields.

The transport layer (git in v0.25, optional HTTP in some future v0.26+)
moves bundle YAML files between instances. The bundle format itself is
the contract.
"""
from __future__ import annotations

import hashlib
import json
from typing import Any

from pydantic import BaseModel, ConfigDict, Field

from opendaisugi.pathway import CompiledPathway


class PathwayBundle(BaseModel):
    """A self-contained, signed pathway suitable for git/HTTP transport."""

    model_config = ConfigDict(extra="allow")

    bundle_format_version: int = 1
    pathway: CompiledPathway
    structure_signature: str = Field(
        description="From v0.24's plan_structure_signature(pathway.plan_template); "
                    "denormalized onto the bundle so consumers can index without "
                    "deserializing the full pathway."
    )
    publisher: str = Field(
        description="Human-readable publisher id (e.g. 'alice@laptop', "
                    "'build-server-2'). Cosmetic — trust gates "
                    "on the ed25519 signing key, not this string."
    )
    published_at: float
    bundle_hash: str = Field(
        description="sha256(canonical-JSON(pathway, publisher, published_at)). "
                    "Content-addresses the bundle for git filename + de-dup."
    )
    signature_b64: str | None = Field(
        default=None,
        description="ed25519 signature (base64) over the canonical-JSON of "
                    "(pathway, publisher, published_at). None for unsigned "
                    "bundles, which trusted-signer consumers refuse."
    )
    signer_pubkey_b64: str | None = Field(
        default=None,
        description="Base64-encoded ed25519 public key matching "
                    "``signature_b64``. Consumer looks this up in their "
                    "TrustedSignerRegistry."
    )


def _canonical_payload(
    pathway: CompiledPathway, publisher: str, published_at: float
) -> bytes:
    """Canonical-JSON encoding for hashing and signing.

    Sorted keys, stable separators, str-fallback for non-JSON-native types
    (e.g. ``float``-precision dates). Result is bytes ready for sha256
    or ed25519.sign().
    """
    body = {
        "pathway": pathway.model_dump(mode="json"),
        "publisher": publisher,
        "published_at": published_at,
    }
    return json.dumps(body, sort_keys=True, separators=(",", ":"),
                      default=str).encode("utf-8")


def compute_bundle_hash(
    pathway: CompiledPathway, publisher: str, published_at: float
) -> str:
    """Content-address a bundle. Stable: same inputs → same hash."""
    return hashlib.sha256(
        _canonical_payload(pathway, publisher, published_at)
    ).hexdigest()


def pathway_to_bundle(
    pathway: CompiledPathway,
    *,
    publisher: str,
    published_at: float | None = None,
    private_key_b64: str | None = None,
    public_key_b64: str | None = None,
) -> PathwayBundle:
    """Serialize a pathway into a transport-ready bundle.

    Pass ``private_key_b64`` + ``public_key_b64`` (the v0.15 ed25519
    keypair shape — see ``opendaisugi.signing.generate_keypair``) to
    produce a signed bundle. Trust-gating consumers will accept it iff
    ``public_key_b64`` appears in their ``TrustedSignerRegistry``.
    Omit both for an unsigned bundle (refused by trust-gating consumers).
    """
    import time as _time
    if published_at is None:
        published_at = _time.time()
    if not pathway.structure_signature:
        from opendaisugi.distiller import plan_structure_signature
        try:
            structure_sig = plan_structure_signature(pathway.plan_template)
        except Exception:
            structure_sig = ""
    else:
        structure_sig = pathway.structure_signature
    bundle_hash = compute_bundle_hash(pathway, publisher, published_at)
    signature_b64: str | None = None
    if private_key_b64 is not None:
        if public_key_b64 is None:
            raise ValueError(
                "pathway_to_bundle: signing requires both private_key_b64 "
                "and public_key_b64 (the latter goes into the bundle for "
                "consumer verification)"
            )
        from opendaisugi.signing import sign_bytes
        payload = _canonical_payload(pathway, publisher, published_at)
        signature_b64 = sign_bytes(payload, private_key_b64)
    return PathwayBundle(
        bundle_format_version=1,
        pathway=pathway,
        structure_signature=structure_sig,
        publisher=publisher,
        published_at=published_at,
        bundle_hash=bundle_hash,
        signature_b64=signature_b64,
        signer_pubkey_b64=public_key_b64 if signature_b64 else None,
    )


class UntrustedSignerError(Exception):
    """Bundle's signer is not in the consumer's trusted-signers registry."""


class InvalidSignatureError(Exception):
    """Bundle's signature does not verify against its claimed public key."""


class UnsignedBundleError(Exception):
    """Bundle has no signature and the consumer requires signed bundles."""


def bundle_to_pathway(
    bundle: PathwayBundle,
    *,
    trusted_pubkey_b64s: set[str] | None = None,
    require_signed: bool = True,
) -> CompiledPathway:
    """Verify a bundle and return its pathway.

    - If ``require_signed`` (default), raises ``UnsignedBundleError`` when
      the bundle has no signature.
    - If ``trusted_pubkey_b64s`` is provided, raises
      ``UntrustedSignerError`` when the signer's pubkey is not in the set.
    - If signature is present, verifies it cryptographically; raises
      ``InvalidSignatureError`` on mismatch.

    Pass ``require_signed=False`` and ``trusted_pubkey_b64s=None`` for
    unsigned dev-mode round-trips. Production consumers should leave both
    at their defaults.
    """
    if bundle.signature_b64 is None:
        if require_signed:
            raise UnsignedBundleError(
                f"bundle {bundle.bundle_hash[:12]} is unsigned; refusing"
            )
        return bundle.pathway
    if bundle.signer_pubkey_b64 is None:
        raise InvalidSignatureError(
            f"bundle {bundle.bundle_hash[:12]} carries a signature but "
            f"no signer pubkey to verify against"
        )
    if trusted_pubkey_b64s is not None and \
            bundle.signer_pubkey_b64 not in trusted_pubkey_b64s:
        raise UntrustedSignerError(
            f"bundle {bundle.bundle_hash[:12]} signed by "
            f"{bundle.signer_pubkey_b64[:16]}…; not in trusted-signers list"
        )
    from opendaisugi.signing import verify_bytes
    payload = _canonical_payload(
        bundle.pathway, bundle.publisher, bundle.published_at,
    )
    if not verify_bytes(payload, bundle.signature_b64, bundle.signer_pubkey_b64):
        raise InvalidSignatureError(
            f"bundle {bundle.bundle_hash[:12]} signature does not verify"
        )
    return bundle.pathway
