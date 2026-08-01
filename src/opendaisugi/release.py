"""Signed release manifests (roadmap Stage 7 — release-artifact signing).

The one honest gap named in the Stage-7 scorecard: pathway *bundles* are signed,
but *release artifacts* (the wheel/sdist a stranger downloads) are not. This
closes it **without inventing a second trust root** — it reuses the exact
ed25519 machinery and ``TrustedSignerRegistry`` that already sign contracts and
pathway bundles (``opendaisugi.signing``). One key system, one thing for a
skeptic to audit.

A release manifest is a small JSON document: a version, and for each artifact
its name, size, and SHA-256. The maintainer signs the canonicalized manifest
once; a downloader then verifies two independent things — the signature is by a
*trusted* signer, and each artifact on disk *still hashes to what the manifest
says*. Either failing fails the whole verification, closed.

This module ships the machinery, tests, and CLI. It deliberately does **not**
generate or store a private key, and does not cut a release — key generation is
a one-command step (``daisugi release keygen``) the release operator runs and
keeps the private half offline.
"""

from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from opendaisugi.signing import TrustedSignerRegistry, sign_bytes, verify_bytes

_CHUNK = 1 << 20  # 1 MiB streaming read — artifacts can be large.


def sha256_file(path: Path | str) -> str:
    """Streaming SHA-256 of a file, hex-encoded."""
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(_CHUNK), b""):
            h.update(chunk)
    return h.hexdigest()


def build_manifest(
    artifacts: list[Path | str],
    *,
    version: str,
    created_at: str,
) -> dict[str, Any]:
    """Build an unsigned manifest over ``artifacts`` (hashed, sorted by name).

    ``created_at`` is passed in (not read from the clock) so the manifest is
    reproducible and this function stays pure/testable.
    """
    entries = []
    for a in artifacts:
        p = Path(a)
        entries.append({"name": p.name, "sha256": sha256_file(p), "size": p.stat().st_size})
    entries.sort(key=lambda e: e["name"])
    return {
        "manifest_version": 1,
        "version": version,
        "created_at": created_at,
        "artifacts": entries,
        "signer": None,
        "signature": None,
    }


def canonicalize_manifest(manifest: dict[str, Any]) -> bytes:
    """Deterministic bytes for signing — the manifest minus its ``signature``.

    ``signer`` is retained (and thus signed) so the claimed identity is bound to
    the signature; only the signature field itself is excluded, exactly as
    ``canonicalize_contract`` excludes its own signature.
    """
    body = {k: v for k, v in manifest.items() if k != "signature"}
    return json.dumps(body, sort_keys=True, separators=(",", ":")).encode("utf-8")


def sign_manifest(manifest: dict[str, Any], private_key_b64: str, *, signer: str) -> dict[str, Any]:
    """Return a copy of ``manifest`` with ``signer`` set and a fresh signature."""
    signed = dict(manifest)
    signed["signer"] = signer
    signed["signature"] = None
    signed["signature"] = sign_bytes(canonicalize_manifest(signed), private_key_b64)
    return signed


def verify_manifest_signature(manifest: dict[str, Any], public_key_b64: str) -> bool:
    """True iff ``manifest['signature']`` is valid over the canonical body."""
    sig = manifest.get("signature")
    if not sig:
        return False
    return verify_bytes(canonicalize_manifest(manifest), sig, public_key_b64)


def verify_artifacts(manifest: dict[str, Any], artifact_dir: Path | str) -> list[str]:
    """Names of artifacts that are missing or whose on-disk hash mismatches.

    Empty list ⇒ every artifact in the manifest is present and unmodified.
    """
    d = Path(artifact_dir)
    bad: list[str] = []
    for entry in manifest.get("artifacts", []):
        p = d / entry["name"]
        if not p.exists() or sha256_file(p) != entry["sha256"]:
            bad.append(entry["name"])
    return bad


@dataclass
class ReleaseVerification:
    """The verdict of :func:`verify_release` — closed if either check fails."""

    ok: bool
    signature_ok: bool
    artifact_mismatches: list[str]
    signer: str | None
    reason: str


def verify_release(
    manifest: dict[str, Any],
    *,
    artifact_dir: Path | str,
    registry: TrustedSignerRegistry,
    signer_names: list[str],
) -> ReleaseVerification:
    """Verify a signed release: trusted signature **and** intact artifacts.

    Fails closed: an untrusted/invalid signature or any artifact mismatch makes
    ``ok`` False. The signature is checked against ``signer_names`` resolved
    through ``registry`` — an unknown signer is untrusted, never skipped.
    """
    signer = manifest.get("signer")
    signature_ok = False
    for name in signer_names:
        pub = registry.get(name)
        if pub is not None and verify_manifest_signature(manifest, pub):
            signature_ok = True
            break

    mismatches = verify_artifacts(manifest, artifact_dir)
    if not signature_ok:
        reason = "signature not verifiable under any trusted signer"
    elif mismatches:
        reason = f"artifact hash mismatch: {', '.join(mismatches)}"
    else:
        reason = "signature valid and all artifacts intact"
    return ReleaseVerification(
        ok=signature_ok and not mismatches,
        signature_ok=signature_ok,
        artifact_mismatches=mismatches,
        signer=signer,
        reason=reason,
    )


def load_manifest(path: Path | str) -> dict[str, Any]:
    """Load a manifest JSON file."""
    return json.loads(Path(path).read_text(encoding="utf-8"))


def dump_manifest(manifest: dict[str, Any], path: Path | str) -> None:
    """Write a manifest as pretty JSON (stable key order)."""
    Path(path).write_text(json.dumps(manifest, sort_keys=True, indent=2) + "\n", encoding="utf-8")


__all__ = [
    "ReleaseVerification",
    "build_manifest",
    "canonicalize_manifest",
    "dump_manifest",
    "load_manifest",
    "sha256_file",
    "sign_manifest",
    "verify_artifacts",
    "verify_manifest_signature",
    "verify_release",
]
