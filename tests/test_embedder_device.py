"""The embedder must use the GPU only when this torch build can actually run it,
and fall back to CPU otherwise — never a blanket force, and never the
`sm_61 no kernel image` crash on an older GPU. See W9 in
docs/plans/2026-08-26-daisugi-interface-two-lenses.md.
"""

import pytest


def test_cpu_when_cuda_unavailable(monkeypatch):
    torch = pytest.importorskip("torch")
    from opendaisugi._search import _embedder_device

    monkeypatch.setattr(torch.cuda, "is_available", lambda: False)
    assert _embedder_device() == "cpu"


def test_gpu_when_capability_is_supported(monkeypatch):
    torch = pytest.importorskip("torch")
    from opendaisugi._search import _embedder_device

    monkeypatch.setattr(torch.cuda, "is_available", lambda: True)
    monkeypatch.setattr(torch.cuda, "get_device_capability", lambda *a, **k: (8, 6))
    monkeypatch.setattr(torch.cuda, "get_arch_list", lambda: ["sm_80", "sm_86", "sm_90"])
    assert _embedder_device() == "cuda"


def test_cpu_when_gpu_capability_unsupported(monkeypatch):
    # The GTX 1060 on this box: sm_61 is not in the torch build's arch list, so
    # running a kernel on it crashes. Fall back to CPU instead of forcing it globally.
    torch = pytest.importorskip("torch")
    from opendaisugi._search import _embedder_device

    monkeypatch.setattr(torch.cuda, "is_available", lambda: True)
    monkeypatch.setattr(torch.cuda, "get_device_capability", lambda *a, **k: (6, 1))
    monkeypatch.setattr(torch.cuda, "get_arch_list", lambda: ["sm_75", "sm_80", "sm_86", "sm_90"])
    assert _embedder_device() == "cpu"


def test_quiet_model_load_silences_then_restores_loggers():
    # The model load emits third-party chatter (transformers' LOAD REPORT, the HF-hub
    # warning) via loggers; quiet them for the operator, but restore levels after so we
    # don't globally mute transformers for the rest of the process.
    import logging

    from opendaisugi._search import _quiet_model_load

    tlog = logging.getLogger("transformers")
    tlog.setLevel(logging.WARNING)
    with _quiet_model_load():
        assert tlog.level >= logging.ERROR
    assert tlog.level == logging.WARNING


def test_quiet_model_load_swallows_load_time_warnings():
    import warnings

    from opendaisugi._search import _quiet_model_load

    with warnings.catch_warnings(record=True) as caught:
        warnings.simplefilter("always")
        with _quiet_model_load():
            warnings.warn("sm_61 is not compatible", UserWarning)
    assert caught == []
