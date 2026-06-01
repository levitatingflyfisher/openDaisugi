"""Trustable, configurable resolution of open models from the Hugging Face Hub.

The principled alternative to a hardcoded, stale model-id table (per the
local-model research). A tool resolves a model by:

1. **Trusted-org allowlist** — only repos under a configured set of orgs are
   accepted (``DEFAULT_TRUSTED_ORGS``, overridable). An untrusted ``org/repo``
   raises :class:`UntrustedSource` before any fetch.
2. **List, never guess** — the filename comes from ``list_repo_files`` on the
   actual repo, so an automated fetch can't 404 on a hallucinated path (the exact
   bug that bit ``daisugi setup``'s first llamafile attempt).
3. **Pin to an immutable commit** — the resolved :class:`ModelRef` carries a
   ``revision`` (the repo's current commit SHA unless one is supplied), so a later
   download is reproducible and can't be swapped under you.
4. **Opt-in download** — :func:`download_pinned` refuses unless
   ``allow_download=True``; resolution itself touches no weights.

The Hub client is injectable (``api=`` / ``_downloader=``) so this is fully
unit-testable without network. ``huggingface_hub`` is imported lazily.
"""

from __future__ import annotations

import logging
from dataclasses import dataclass

_log = logging.getLogger("opendaisugi.model_registry")

# Configurable allowlist of Hugging Face orgs we'll resolve models from. Kept
# conservative and overridable per call; the research found llamafiles spread
# across many orgs, so a tool must allow extension, not assume one canonical org.
DEFAULT_TRUSTED_ORGS: tuple[str, ...] = (
    "mozilla-ai",          # official llamafile builds + engine
    "ggml-org",            # llama.cpp / GGUF reference org
    "Qwen",
    "google",              # Gemma
    "meta-llama",
    "microsoft",           # Phi
    "bartowski",           # widely-used community GGUF quantizer
    "lmstudio-community",
)

_LLAMAFILE_SUFFIX = ".llamafile"


class UntrustedSource(Exception):
    """The repo's org is not in the trusted allowlist."""


class NoMatchingFile(Exception):
    """No file in the repo matched the requested suffix."""


class DownloadNotAllowed(Exception):
    """download_pinned was called without allow_download=True."""


@dataclass(frozen=True)
class ModelRef:
    """An immutable, trust-pinned reference to a model file on the Hub."""

    repo_id: str
    filename: str | None
    revision: str
    suffix: str = _LLAMAFILE_SUFFIX


def _org_of(repo_id: str) -> str:
    return repo_id.split("/", 1)[0]


def is_trusted(repo_id: str, trusted_orgs: "tuple[str, ...] | list[str]" = DEFAULT_TRUSTED_ORGS) -> bool:
    """True iff ``repo_id``'s org is in the trusted allowlist."""
    return _org_of(repo_id) in set(trusted_orgs)


def _hub_api(api):
    if api is not None:
        return api
    from huggingface_hub import HfApi  # lazy

    return HfApi()


def resolve_pinned(
    repo_id: str,
    *,
    suffix: str = _LLAMAFILE_SUFFIX,
    trusted_orgs: "tuple[str, ...] | list[str]" = DEFAULT_TRUSTED_ORGS,
    revision: str | None = None,
    api=None,
) -> ModelRef:
    """Resolve a trusted repo to a concrete, commit-pinned :class:`ModelRef`.

    Refuses an untrusted org, picks a real file matching ``suffix`` from
    ``list_repo_files`` (never a guessed name), and pins ``revision`` to the
    repo's current commit SHA when one isn't supplied.
    """
    if not is_trusted(repo_id, trusted_orgs):
        raise UntrustedSource(
            f"{repo_id!r} is not under a trusted org {tuple(trusted_orgs)} — "
            f"add its org to trusted_orgs to allow it"
        )
    hub = _hub_api(api)
    files = list(hub.list_repo_files(repo_id, revision=revision))
    matches = sorted(f for f in files if f.endswith(suffix))
    if not matches:
        raise NoMatchingFile(f"no {suffix!r} file in {repo_id!r} (saw {len(files)} files)")
    pinned = revision or hub.model_info(repo_id).sha
    return ModelRef(repo_id=repo_id, filename=matches[0], revision=pinned, suffix=suffix)


def download_pinned(ref: ModelRef, *, allow_download: bool = False, _downloader=None) -> str:
    """Download the pinned file (opt-in). Returns the local path.

    Raises :class:`DownloadNotAllowed` unless ``allow_download=True`` — resolution
    and recommendation never pull weights implicitly. The download is pinned to
    ``ref.revision`` so it's reproducible.
    """
    if not allow_download:
        raise DownloadNotAllowed(
            "refusing to download weights implicitly; pass allow_download=True"
        )
    downloader = _downloader
    if downloader is None:
        from huggingface_hub import hf_hub_download  # lazy

        downloader = hf_hub_download
    return downloader(repo_id=ref.repo_id, filename=ref.filename, revision=ref.revision)


def discover_llamafiles(
    *,
    trusted_orgs: "tuple[str, ...] | list[str]" = DEFAULT_TRUSTED_ORGS,
    limit: int = 50,
    api=None,
) -> list[str]:
    """List repo ids tagged ``library=llamafile`` on the Hub, scoped to trusted orgs."""
    hub = _hub_api(api)
    try:
        models = hub.list_models(filter="llamafile", limit=limit)
    except Exception as exc:  # discovery is best-effort; never crash the caller
        _log.warning("model discovery failed: %s", exc)
        return []
    return [m.id for m in models if is_trusted(m.id, trusted_orgs)]
