"""GitPathwayStore — git-backed shared pathway registry (v0.25+).

A registry is a git repo with this layout:

    opendaisugi-registry/
    ├── trusted-signers.json       # JSON: {name → public_key_b64}
    ├── pathways/
    │   ├── <bundle_hash>.yaml     # signed PathwayBundle, one per file
    │   └── ...
    └── README.md                  # optional team conventions

GitPathwayStore subclasses PathwayStore. The sqlite layer remains the
local cache; ``pull()`` populates it from bundles in the local git clone;
``publish()`` writes a signed bundle, commits it, and pushes. Existing
``Daisugi(pathway_store=...)`` integrations work unchanged when handed a
GitPathwayStore — ``find()`` and ``put()`` retain their PathwayStore
semantics.
"""
from __future__ import annotations

import json
import logging
import subprocess
from pathlib import Path
from typing import Any

import yaml

from opendaisugi.pathway import CompiledPathway
from opendaisugi.pathway_bundle import (
    InvalidSignatureError,
    PathwayBundle,
    UnsignedBundleError,
    UntrustedSignerError,
    bundle_to_pathway,
    pathway_to_bundle,
)
from opendaisugi.pathway_store import PathwayStore

_log = logging.getLogger("opendaisugi.git_pathway_store")


def _git(repo: Path, *args: str, check: bool = True) -> subprocess.CompletedProcess:
    """Run a git subcommand in ``repo``. Raises CalledProcessError on
    non-zero exit when ``check=True`` (the default).
    """
    return subprocess.run(
        ["git", "-C", str(repo), *args],
        capture_output=True, text=True, check=check,
    )


class GitPathwayStore(PathwayStore):
    """A pathway store backed by a local clone of a shared git registry.

    Inherits the local SQLite cache from PathwayStore; layers git transport
    on top. ``pull()`` syncs new pathway bundles from the remote into the
    local cache after signature verification. ``publish()`` writes a
    signed bundle to the working tree, commits, and pushes.

    Constructed from an already-cloned repo path. Use the ``daisugi
    registry init`` CLI subcommand to clone for the first time.
    """

    PATHWAYS_SUBDIR = "pathways"
    TRUSTED_SIGNERS_FILE = "trusted-signers.json"

    def __init__(
        self,
        *,
        repo_path: str | Path,
        cache_db_path: str | Path | None = None,
        private_key_b64: str | None = None,
        public_key_b64: str | None = None,
        publisher: str = "opendaisugi-instance",
        require_signed: bool = True,
        offline_ok: bool = True,
        trusted_signers_path: str | Path | None = None,
    ) -> None:
        self.repo_path = Path(repo_path)
        if not self.repo_path.exists():
            raise ValueError(
                f"GitPathwayStore: repo_path {self.repo_path} does not exist; "
                f"clone it first with `daisugi registry init`"
            )
        self.private_key_b64 = private_key_b64
        self.public_key_b64 = public_key_b64
        self.publisher = publisher
        self.require_signed = require_signed
        self.offline_ok = offline_ok

        # Local cache: defaults to an in-repo cache file so it's
        # gitignore-able and operators don't accidentally commit it.
        if cache_db_path is None:
            cache_db_path = self.repo_path / ".cache" / "pathways.db"
            Path(cache_db_path).parent.mkdir(parents=True, exist_ok=True)
        super().__init__(cache_db_path)

        # SECURITY: the trust anchor must NOT be the in-repo trusted-signers.json —
        # `git pull` updates that file from the same remote whose bundles it gates,
        # so an attacker who can push adds their own key + a malicious bundle and it
        # verifies (circular trust). Default to a LOCAL, git-untracked file next to
        # the cache; pass ``trusted_signers_path`` for an explicit out-of-band anchor.
        self._trusted_signers_override = (
            Path(trusted_signers_path) if trusted_signers_path is not None else None
        )
        self._local_trust_default = Path(cache_db_path).parent / self.TRUSTED_SIGNERS_FILE

        # Materialize any bundles already in the local clone (no pull yet).
        self._materialize_local_bundles()

    @property
    def _pathways_dir(self) -> Path:
        return self.repo_path / self.PATHWAYS_SUBDIR

    @property
    def _trusted_signers_path(self) -> Path:
        """The LOCAL, out-of-band trust anchor — never the in-repo (pulled) file."""
        return self._trusted_signers_override or self._local_trust_default

    def _load_trusted_signers(self) -> set[str]:
        """Load the trusted-signers JSON file from the LOCAL anchor.

        Returns the set of base64 public keys. Empty set if the file
        doesn't exist or is unparseable (caller's risk: with no trusted
        signers and ``require_signed=True``, every pull is rejected).
        """
        if not self._trusted_signers_path.exists():
            # If the (remote-controlled) in-repo file exists but no local anchor is
            # configured, warn — trust is NOT taken from it, so signed bundles will
            # be rejected until a local anchor is set up.
            if (self.repo_path / self.TRUSTED_SIGNERS_FILE).exists():
                _log.warning(
                    "git_pathway_store.in_repo_trust_ignored",
                    extra={"hint": "trusted-signers.json in the registry is remote-"
                                   "controlled and is NOT used as a trust anchor; "
                                   "set trusted_signers_path to a local file"},
                )
            return set()
        try:
            data = json.loads(self._trusted_signers_path.read_text(encoding="utf-8"))
        except json.JSONDecodeError:
            _log.warning(
                "git_pathway_store.trusted_signers_unparseable",
                extra={"path": str(self._trusted_signers_path)},
            )
            return set()
        if not isinstance(data, dict):
            return set()
        return {v for v in data.values() if isinstance(v, str)}

    def pull(self) -> int:
        """``git pull`` then materialize any newly-available bundles into
        the local cache. Returns the count of newly-materialized pathways.

        Failures during the network operation are tolerated when
        ``offline_ok=True`` (the default) — the local clone stays
        readable even when the remote is unreachable. Verification
        failures (untrusted signer, invalid signature) skip individual
        bundles with a warning; they don't fail the whole pull.
        """
        try:
            _git(self.repo_path, "pull", "--ff-only", check=True)
        except subprocess.CalledProcessError as exc:
            if not self.offline_ok:
                raise
            _log.warning(
                "git_pathway_store.pull_failed",
                extra={"error": exc.stderr.strip()[:200]},
            )
        return self._materialize_local_bundles()

    def _materialize_local_bundles(self) -> int:
        """Walk the local pathways/ dir; insert any not-yet-cached bundles
        into the SQLite cache. Skips bundles that fail signature
        verification with a warning."""
        if not self._pathways_dir.exists():
            return 0
        trusted = self._load_trusted_signers()
        existing_ids = {p.id for p in self.list_all()}
        new_count = 0
        for bundle_path in sorted(self._pathways_dir.glob("*.yaml")):
            try:
                raw = yaml.safe_load(bundle_path.read_text(encoding="utf-8"))
                bundle = PathwayBundle.model_validate(raw)
            except Exception as exc:
                _log.warning(
                    "git_pathway_store.bundle_unparseable",
                    extra={"path": str(bundle_path), "error": str(exc)[:200]},
                )
                continue
            try:
                pathway = bundle_to_pathway(
                    bundle,
                    trusted_pubkey_b64s=trusted if self.require_signed else None,
                    require_signed=self.require_signed,
                )
            except (UnsignedBundleError, UntrustedSignerError, InvalidSignatureError) as exc:
                _log.warning(
                    "git_pathway_store.bundle_refused",
                    extra={"path": str(bundle_path), "reason": type(exc).__name__},
                )
                continue
            if pathway.id in existing_ids:
                continue
            self.put(pathway)
            existing_ids.add(pathway.id)
            new_count += 1
        return new_count

    def publish(
        self,
        pathway: CompiledPathway,
        *,
        commit_message: str | None = None,
        push: bool = True,
    ) -> str:
        """Sign and write a bundle for ``pathway``, commit, optionally push.

        Returns the bundle hash. Refuses (raises ValueError) if
        ``private_key_b64`` was not provided at construction. Pushes to
        the configured remote when ``push=True`` (default); set ``push=False``
        for review-flow setups that prefer a separate manual push.
        """
        if self.private_key_b64 is None:
            raise ValueError(
                "GitPathwayStore.publish requires private_key_b64 at "
                "construction; the bundle must be signed."
            )
        if not getattr(pathway, "publishable", True):
            raise ValueError(
                f"pathway {pathway.id} is not marked publishable; run "
                f"`daisugi pathways mark-publishable {pathway.id}` first"
            )
        bundle = pathway_to_bundle(
            pathway,
            publisher=self.publisher,
            private_key_b64=self.private_key_b64,
            public_key_b64=self.public_key_b64,
        )
        self._pathways_dir.mkdir(parents=True, exist_ok=True)
        bundle_path = self._pathways_dir / f"{bundle.bundle_hash}.yaml"
        bundle_path.write_text(
            yaml.safe_dump(bundle.model_dump(mode="json"), sort_keys=False),
            encoding="utf-8",
        )
        rel = bundle_path.relative_to(self.repo_path)
        _git(self.repo_path, "add", str(rel))
        msg = commit_message or (
            f"publish pathway {pathway.id} (bundle {bundle.bundle_hash[:12]})"
        )
        _git(self.repo_path, "commit", "-m", msg)
        if push:
            try:
                _git(self.repo_path, "push", check=True)
            except subprocess.CalledProcessError as exc:
                if not self.offline_ok:
                    raise
                _log.warning(
                    "git_pathway_store.push_failed",
                    extra={"error": exc.stderr.strip()[:200]},
                )
        # Also cache locally so find() sees the new pathway immediately.
        self.put(pathway)
        return bundle.bundle_hash

    def status(self) -> dict[str, Any]:
        """Return registry diagnostic info for the CLI's ``registry status``."""
        try:
            commit = _git(self.repo_path, "rev-parse", "HEAD",
                          check=False).stdout.strip()
        except FileNotFoundError:
            commit = "(git binary missing)"
        bundle_files = list(self._pathways_dir.glob("*.yaml")) \
            if self._pathways_dir.exists() else []
        cached = self.list_all()
        return {
            "repo_path": str(self.repo_path),
            "head_commit": commit,
            "bundle_files": len(bundle_files),
            "cached_pathways": len(cached),
            "trusted_signers": len(self._load_trusted_signers()),
            "publisher": self.publisher,
            "signing_configured": self.private_key_b64 is not None,
        }
