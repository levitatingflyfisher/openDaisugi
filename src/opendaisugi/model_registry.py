"""Resolution of open models from the Hugging Face Hub.

The principled alternative to a hardcoded, stale model-id table (per the
local-model research). A tool resolves a model by:

1. **Any named repo** - the operator may name any repo; nothing is refused.
   Discovery (``discover_llamafiles``) lists repos under a configurable set
   of orgs (``DISCOVERY_ORGS``).
2. **List, never guess** - the filename comes from ``list_repo_files`` on the
   actual repo, so an automated fetch can't 404 on a hallucinated path (the exact
   bug that bit ``daisugi setup``'s first llamafile attempt).
3. **Pin to an immutable commit** - the resolved :class:`ModelRef` carries a
   ``revision`` (the repo's current commit SHA unless one is supplied), so a later
   download is reproducible and can't be swapped under you.
4. **Opt-in download** - :func:`download_pinned` refuses unless
   ``allow_download=True``; resolution itself touches no weights.

The Hub client is injectable (``api=`` / ``_downloader=``) so this is fully
unit-testable without network. ``huggingface_hub`` is imported lazily.
"""

from __future__ import annotations

import logging
from dataclasses import dataclass

_log = logging.getLogger("opendaisugi.model_registry")

# The orgs discovery lists repos from. Configurable per call.
DISCOVERY_ORGS: tuple[str, ...] = (
    "mozilla-ai",  # llamafile builds + engine
    "ggml-org",  # llama.cpp / GGUF reference org
    "ibm-granite",
    "mistralai",
    "google",
    "meta-llama",
    "bartowski",  # community GGUF quantizer
    "lmstudio-community",
)

_LLAMAFILE_SUFFIX = ".llamafile"


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


def _hub_api(api):
    if api is not None:
        return api
    from huggingface_hub import HfApi  # lazy

    return HfApi()


def resolve_pinned(
    repo_id: str,
    *,
    suffix: str = _LLAMAFILE_SUFFIX,
    revision: str | None = None,
    api=None,
) -> ModelRef:
    """Resolve any named repo to a concrete, commit-pinned :class:`ModelRef`.

    Picks a real file matching ``suffix`` from ``list_repo_files`` (never a
    guessed name), and pins ``revision`` to the repo's current commit SHA
    when one isn't supplied.
    """
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
    orgs: "tuple[str, ...] | list[str]" = DISCOVERY_ORGS,
    limit: int = 50,
    api=None,
) -> list[str]:
    """List repo ids tagged ``library=llamafile`` on the Hub, under ``orgs``."""
    hub = _hub_api(api)
    try:
        models = hub.list_models(filter="llamafile", limit=limit)
    except Exception as exc:  # discovery is best-effort; never crash the caller
        _log.warning("model discovery failed: %s", exc)
        return []
    scope = set(orgs)
    return [m.id for m in models if _org_of(m.id) in scope]
