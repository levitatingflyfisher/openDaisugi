"""Hardware detection + size-based local-model recommendation (v0.30).

Detection is best-effort and must never raise; recommendation is a transparent
size-budget heuristic (NOT a baked-in model-id table — the model-family pick is
unverified and drifts, so we recommend a size class + quant + llamafile command
and mark it provisional-until-qualified).
"""

import opendaisugi.hardware as hw
from opendaisugi.hardware import (
    HardwareProfile,
    ModelRecommendation,
    detect_hardware,
    recommend_model,
)

# ---- recommendation (pure) ----

def _profile(*, ram_gb=16.0, vram_gb=0.0, gpu_name=None, unified=False, system="Linux", arch="x86_64", cpu=8):
    return HardwareProfile(
        system=system, arch=arch, cpu_count=cpu,
        ram_gb=ram_gb, vram_gb=vram_gb, gpu_name=gpu_name, unified_memory=unified,
    )


def test_budget_uses_vram_for_discrete_gpu():
    p = _profile(ram_gb=64.0, vram_gb=24.0, gpu_name="RTX 4090")
    # discrete GPU → budget from VRAM (with headroom), not the big system RAM
    assert 18.0 <= p.model_budget_gb <= 20.0


def test_budget_uses_ram_for_cpu_only():
    p = _profile(ram_gb=16.0, vram_gb=0.0)
    assert 9.0 <= p.model_budget_gb <= 10.0


def test_budget_apple_unified_uses_ram():
    p = _profile(ram_gb=16.0, vram_gb=0.0, unified=True, system="Darwin", arch="arm64")
    assert 9.0 <= p.model_budget_gb <= 10.0


def test_recommend_scales_with_budget():
    small = recommend_model(_profile(ram_gb=4.0))     # budget ~2.4
    mid = recommend_model(_profile(ram_gb=16.0))      # budget ~9.6
    big = recommend_model(_profile(ram_gb=64.0, vram_gb=24.0, gpu_name="RTX 4090"))  # ~19.2
    assert small.params_b_max < mid.params_b_max < big.params_b_max


def test_recommendation_is_provisional_and_llamafile_and_hedged():
    rec = recommend_model(_profile(ram_gb=16.0))
    assert isinstance(rec, ModelRecommendation)
    assert rec.provisional is True                      # never asserted-best
    assert rec.runtime == "llamafile"                   # onefile default
    assert "qualif" in rec.rationale.lower()            # must say: qualify before trusting
    assert len(rec.candidate_families) >= 2             # examples, not a single winner
    assert rec.quant                                    # a concrete quant suggestion


def test_cpu_only_recommendation_flags_slowness():
    rec = recommend_model(_profile(ram_gb=16.0, vram_gb=0.0))
    assert "cpu" in rec.rationale.lower()


# ---- detection (best-effort, degradation paths) ----

def test_detect_never_raises_and_returns_profile():
    p = detect_hardware()
    assert isinstance(p, HardwareProfile)
    assert p.cpu_count >= 1


def test_detect_handles_no_gpu(monkeypatch):
    monkeypatch.setattr(hw, "_detect_gpu", lambda: (0.0, None))
    monkeypatch.setattr(hw, "_detect_ram_gb", lambda: 8.0)
    p = detect_hardware()
    assert p.vram_gb == 0.0 and p.gpu_name is None
    assert p.model_budget_gb > 0  # falls back to RAM


def test_detect_handles_missing_ram_probe(monkeypatch):
    monkeypatch.setattr(hw, "_detect_ram_gb", lambda: None)
    monkeypatch.setattr(hw, "_detect_gpu", lambda: (0.0, None))
    p = detect_hardware()              # must not raise even if RAM unknown
    assert p.ram_gb is None
    assert p.model_budget_gb == 0.0    # unknown budget, not a crash


def test_detect_gpu_path(monkeypatch):
    monkeypatch.setattr(hw, "_detect_gpu", lambda: (24.0, "RTX 4090"))
    monkeypatch.setattr(hw, "_detect_ram_gb", lambda: 64.0)
    p = detect_hardware()
    assert p.vram_gb == 24.0 and p.gpu_name == "RTX 4090"
    rec = recommend_model(p)
    assert rec.params_b_max >= 8
