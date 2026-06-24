"""Ed25519 signing + trusted-signer registry for skill contracts (v0.15.0).

Closes the v0.11.0 stub: contracts can now carry a real cryptographic
signature, and ``verify_delegation`` can reject a tampered contract via
UNSAT of ``signature valid ∧ payload matches``. A delegating agent that
trusts a set of public keys ("I trust the Robin-v1 distilled skill
signer") gets cryptographic assurance that the envelope it is proving
subsumption over is the one the skill author actually published.

Canonicalization is deterministic JSON over the contract minus its
signature/signer fields — so re-serializing the same contract always
produces the same bytes to sign. The registry is a plain JSON file keyed
by signer name → base64 public key, stored by default under
``~/.opendaisugi/trusted_signers.json``. Registry I/O is exposed so a CLI
or test harness can swap the location.

Runtime dependency: ``cryptography`` (via the ``[sign]`` extra). Import is
lazy so the rest of the library still works on a wheel built without it.
"""

from __future__ import annotations

import base64
import json
import logging
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from opendaisugi.contracts import Contract

_log = logging.getLogger("opendaisugi.signing")


class SigningUnavailable(RuntimeError):
    """Raised when signing is requested but ``cryptography`` is not installed."""


def _load_cryptography() -> Any:
    try:
        from cryptography.exceptions import InvalidSignature
        from cryptography.hazmat.primitives.asymmetric.ed25519 import (
            Ed25519PrivateKey,
            Ed25519PublicKey,
        )
    except ImportError as e:
        raise SigningUnavailable(
            "signing requires the [sign] extra: uv add 'opendaisugi[sign]'  (or: pip install opendaisugi[sign])"
        ) from e
    return Ed25519PrivateKey, Ed25519PublicKey, InvalidSignature


def canonicalize_contract(contract: Contract) -> bytes:
    """Return deterministic bytes for signing.

    Strips ``signature`` and ``signer`` before serialization so a contract
    signs its own body, not its signature. Uses ``sort_keys=True`` +
    ``separators=(",", ":")`` for canonical form.
    """
    body = contract.model_dump(mode="json", exclude={"signature", "signer"})
    return json.dumps(body, sort_keys=True, separators=(",", ":")).encode("utf-8")


def generate_keypair() -> tuple[str, str]:
    """Generate a fresh ed25519 keypair as (private_b64, public_b64).

    The private key is PKCS#8 DER; the public key is raw 32 bytes. Both
    are base64-encoded so they round-trip through JSON/CLI without issue.
    """
    from cryptography.hazmat.primitives import serialization

    Ed25519PrivateKey, _PubKey, _InvSig = _load_cryptography()
    priv = Ed25519PrivateKey.generate()
    priv_bytes = priv.private_bytes(
        encoding=serialization.Encoding.Raw,
        format=serialization.PrivateFormat.Raw,
        encryption_algorithm=serialization.NoEncryption(),
    )
    pub_bytes = priv.public_key().public_bytes(
        encoding=serialization.Encoding.Raw,
        format=serialization.PublicFormat.Raw,
    )
    return (
        base64.b64encode(priv_bytes).decode("ascii"),
        base64.b64encode(pub_bytes).decode("ascii"),
    )


def sign_bytes(payload: bytes, private_key_b64: str) -> str:
    """ed25519-sign arbitrary bytes; return base64 signature.

    General-purpose primitive used by ``sign_contract`` and (v0.25+) the
    ``PathwayBundle`` signing path. Caller is responsible for producing a
    canonical byte representation of whatever they want to sign.
    """
    Ed25519PrivateKey, _PubKey, _InvSig = _load_cryptography()
    priv = Ed25519PrivateKey.from_private_bytes(base64.b64decode(private_key_b64))
    return base64.b64encode(priv.sign(payload)).decode("ascii")


def verify_bytes(payload: bytes, signature_b64: str, public_key_b64: str) -> bool:
    """Verify a base64 signature over ``payload`` against a base64 pubkey.

    Returns False on any failure — bad encoding, wrong key, tampered
    payload. Never raises (except ``SigningUnavailable`` if the extra
    isn't installed). v0.25+.
    """
    _PrivKey, Ed25519PublicKey, InvalidSignature = _load_cryptography()
    try:
        pub = Ed25519PublicKey.from_public_bytes(base64.b64decode(public_key_b64))
        sig = base64.b64decode(signature_b64)
    except (ValueError, TypeError):
        return False
    try:
        pub.verify(sig, payload)
        return True
    except InvalidSignature:
        return False


def sign_contract(contract: Contract, private_key_b64: str) -> str:
    """Return a base64 signature over the canonicalized contract body."""
    Ed25519PrivateKey, _PubKey, _InvSig = _load_cryptography()
    priv = Ed25519PrivateKey.from_private_bytes(base64.b64decode(private_key_b64))
    sig = priv.sign(canonicalize_contract(contract))
    return base64.b64encode(sig).decode("ascii")


def verify_signature_raw(contract: Contract, public_key_b64: str) -> bool:
    """Verify ``contract.signature`` against ``public_key_b64``.

    Returns False on any failure — missing signature, malformed key,
    tampered body. Never raises (except ``SigningUnavailable`` if the
    extra isn't installed).
    """
    if contract.signature is None:
        return False
    _PrivKey, Ed25519PublicKey, InvalidSignature = _load_cryptography()
    try:
        pub = Ed25519PublicKey.from_public_bytes(base64.b64decode(public_key_b64))
        sig = base64.b64decode(contract.signature)
    except (ValueError, TypeError):
        return False
    try:
        pub.verify(sig, canonicalize_contract(contract))
        return True
    except InvalidSignature:
        return False


@dataclass
class TrustedSignerRegistry:
    """A JSON-backed map of signer name → base64 public key.

    The registry is the trust root for ``verify_delegation``: a caller
    passes signer names (via ``trusted_signers=``) and the registry
    resolves each to a public key for signature verification. Unknown
    signers are treated as untrusted — verification fails closed.
    """

    path: Path
    _entries: dict[str, str]

    @classmethod
    def load(cls, path: Path | str) -> "TrustedSignerRegistry":
        p = Path(path)
        if p.exists():
            entries = json.loads(p.read_text(encoding="utf-8"))
            if not isinstance(entries, dict):
                raise ValueError(f"{p}: expected dict, got {type(entries).__name__}")
            return cls(path=p, _entries=dict(entries))
        return cls(path=p, _entries={})

    def save(self) -> None:
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self.path.write_text(
            json.dumps(self._entries, sort_keys=True, indent=2),
            encoding="utf-8",
        )

    def add(self, name: str, public_key_b64: str) -> None:
        self._entries[name] = public_key_b64

    def remove(self, name: str) -> bool:
        return self._entries.pop(name, None) is not None

    def get(self, name: str) -> str | None:
        return self._entries.get(name)

    def names(self) -> list[str]:
        return sorted(self._entries)

    def verify(self, contract: Contract, signer_names: list[str]) -> bool:
        """Verify ``contract`` against any of ``signer_names`` in this registry.

        Returns True iff the contract's signature verifies under at least
        one named public key. Unknown names are silently skipped.
        """
        if contract.signature is None:
            return False
        for name in signer_names:
            pub = self._entries.get(name)
            if pub is None:
                _log.debug(
                    "signing.signer_unknown",
                    extra={"contract_id": contract.contract_id, "signer": name},
                )
                continue
            if verify_signature_raw(contract, pub):
                _log.info(
                    "signing.verify_ok",
                    extra={"contract_id": contract.contract_id, "signer": name},
                )
                return True
        _log.warning(
            "signing.verify_failed",
            extra={
                "contract_id": contract.contract_id,
                "attempted_signers": list(signer_names),
            },
        )
        return False


def default_registry_path() -> Path:
    return Path.home() / ".opendaisugi" / "trusted_signers.json"


__all__ = [
    "SigningUnavailable",
    "TrustedSignerRegistry",
    "canonicalize_contract",
    "default_registry_path",
    "generate_keypair",
    "sign_contract",
    "verify_signature_raw",
]
