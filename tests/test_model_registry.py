"""Model resolution from the Hugging Face Hub.

Resolve any named repo, pin it to an immutable commit revision, and never
guess a filename (list the repo first), so an automated fetch cannot 404
on a made-up path. Discovery lists repos under a configurable set of
orgs. Download is opt-in. Tested with an injected fake Hub API, no network.
"""

import types

import pytest

from opendaisugi.model_registry import (
    DISCOVERY_ORGS,
    DownloadNotAllowed,
    ModelRef,
    NoMatchingFile,
    discover_llamafiles,
    download_pinned,
    resolve_pinned,
)


class _FakeApi:
    def __init__(self, files=(), sha="deadbeefcafe", model_ids=()):
        self._files = list(files)
        self._sha = sha
        self._model_ids = list(model_ids)

    def list_repo_files(self, repo_id, revision=None):
        return list(self._files)

    def model_info(self, repo_id, revision=None):
        return types.SimpleNamespace(sha=self._sha)

    def list_models(self, **kwargs):
        return [types.SimpleNamespace(id=m) for m in self._model_ids]


def test_discovery_orgs_hold_the_catalog_orgs():
    for org in ("mozilla-ai", "ibm-granite", "mistralai", "google", "meta-llama"):
        assert org in DISCOVERY_ORGS


def test_resolve_accepts_any_named_repo():
    api = _FakeApi(files=["model-q4.llamafile"], sha="c0ffee")
    ref = resolve_pinned("anyone/x", api=api)
    assert ref.repo_id == "anyone/x"
    assert ref.filename == "model-q4.llamafile"
    assert ref.revision == "c0ffee"


def test_resolve_returns_real_filename_pinned_to_commit():
    api = _FakeApi(
        files=["README.md", "Qwen2.5-0.5B-Instruct.Q4_K_M.llamafile"], sha="abc123def456"
    )
    ref = resolve_pinned("mozilla-ai/Qwen2.5-0.5B-Instruct-llamafile", api=api)
    assert isinstance(ref, ModelRef)
    assert (
        ref.filename == "Qwen2.5-0.5B-Instruct.Q4_K_M.llamafile"
    )  # from list_repo_files, not guessed
    assert ref.revision == "abc123def456"  # pinned to the immutable commit
    assert ref.repo_id == "mozilla-ai/Qwen2.5-0.5B-Instruct-llamafile"


def test_resolve_honors_explicit_revision():
    api = _FakeApi(files=["m.llamafile"], sha="should-not-be-used")
    ref = resolve_pinned("mozilla-ai/x", revision="v1.2.3-tag", api=api)
    assert ref.revision == "v1.2.3-tag"


def test_resolve_raises_when_no_matching_file():
    api = _FakeApi(files=["README.md", "config.json"])  # no .llamafile
    with pytest.raises(NoMatchingFile):
        resolve_pinned("mozilla-ai/x", api=api)


def test_resolve_custom_suffix_for_gguf():
    api = _FakeApi(files=["model.Q4_K_M.gguf", "README.md"])
    ref = resolve_pinned("mozilla-ai/x", suffix=".gguf", api=api)
    assert ref.filename == "model.Q4_K_M.gguf"


def test_download_is_opt_in():
    ref = ModelRef(repo_id="mozilla-ai/x", filename="m.llamafile", revision="abc")
    calls = []
    fake_dl = lambda **kw: calls.append(kw) or "/tmp/m.llamafile"
    with pytest.raises(DownloadNotAllowed):
        download_pinned(ref, _downloader=fake_dl)  # default allow_download=False
    assert calls == []  # nothing fetched


def test_download_pins_revision_when_allowed():
    ref = ModelRef(repo_id="mozilla-ai/x", filename="m.llamafile", revision="abc123")
    captured = {}

    def fake_dl(**kw):
        captured.update(kw)
        return "/tmp/m.llamafile"

    path = download_pinned(ref, allow_download=True, _downloader=fake_dl)
    assert path == "/tmp/m.llamafile"
    assert captured["repo_id"] == "mozilla-ai/x"
    assert captured["filename"] == "m.llamafile"
    assert captured["revision"] == "abc123"  # pinned, not "main"


def test_discover_lists_repos_under_the_discovery_orgs():
    api = _FakeApi(model_ids=["mozilla-ai/a-llamafile", "other/b-llamafile"])
    assert discover_llamafiles(api=api) == ["mozilla-ai/a-llamafile"]
    assert discover_llamafiles(orgs=("other",), api=api) == ["other/b-llamafile"]
