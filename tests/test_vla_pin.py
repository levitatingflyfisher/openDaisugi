"""The default VLA model loads at a pinned commit; a model the user names does not.

mjcf_path is empty, so these tests need no MuJoCo.
"""

from __future__ import annotations


def _load_with_fakes(**kwargs):
    """Run _ensure_loaded against fake torch/transformers; return the two
    from_pretrained mocks."""
    import sys
    import types
    from unittest.mock import MagicMock, patch

    from opendaisugi.vla_executor import TransformersVLAExecutor

    proc, model = MagicMock(), MagicMock()
    model.to.return_value = model
    fake_transformers = types.ModuleType("transformers")
    fake_transformers.AutoProcessor = MagicMock()
    fake_transformers.AutoProcessor.from_pretrained = MagicMock(return_value=proc)
    fake_transformers.AutoModel = MagicMock()
    fake_transformers.AutoModel.from_pretrained = MagicMock(return_value=model)
    exe = TransformersVLAExecutor(mjcf_path="", **kwargs)
    with patch.dict(
        sys.modules, {"torch": types.ModuleType("torch"), "transformers": fake_transformers}
    ):
        exe._ensure_loaded()
    return (
        fake_transformers.AutoProcessor.from_pretrained,
        fake_transformers.AutoModel.from_pretrained,
    )


def test_the_default_model_loads_at_a_pinned_commit():
    from opendaisugi.vla_executor import SMOLVLA_REVISION

    assert len(SMOLVLA_REVISION) == 40
    for call in _load_with_fakes():
        assert call.call_args.kwargs["revision"] == SMOLVLA_REVISION


def test_a_model_the_user_names_stays_unpinned_unless_they_pass_a_revision():
    for call in _load_with_fakes(model_id="someone/else"):
        assert call.call_args.kwargs["revision"] is None
    for call in _load_with_fakes(model_id="someone/else", revision="abc123"):
        assert call.call_args.kwargs["revision"] == "abc123"
    for call in _load_with_fakes(revision="def456"):
        assert call.call_args.kwargs["revision"] == "def456"
