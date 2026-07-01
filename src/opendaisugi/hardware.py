"""Best-effort hardware detection + size-based local-model recommendation.

Powers ``daisugi setup``: detect what the box can run, then recommend a local
model *sized* to it. The recommendation is a transparent budget→size heuristic,
NOT a baked-in model-id table — the model-family pick is unverified and drifts
release-to-release (see the local-model research), so we recommend a size class
+ quantization + the llamafile runtime, name candidate families as examples, and
mark the result ``provisional`` until a qualification run on the actual box
confirms it can emit valid envelopes.

Detection never raises: an unprobeable machine yields ``ram_gb=None`` /
``vram_gb=0.0`` and a zero budget rather than an exception.
"""

from __future__ import annotations

import logging
import os
import platform
import shutil
import subprocess
from dataclasses import dataclass, field

_log = logging.getLogger("opendaisugi.hardware")


@dataclass(frozen=True)
class HardwareProfile:
    system: str
    arch: str
    cpu_count: int
    ram_gb: float | None
    vram_gb: float
    gpu_name: str | None
    unified_memory: bool  # Apple silicon: GPU shares system RAM

    @property
    def has_discrete_gpu(self) -> bool:
        return self.vram_gb > 0 and not self.unified_memory

    @property
    def model_budget_gb(self) -> float:
        """Memory available to a model, with headroom for the runtime + KV cache.

        Discrete GPU → 80% of VRAM (the model must fit in VRAM). CPU-only or
        Apple unified memory → 60% of system RAM (leave room for the OS and the
        rest of the workload). Unknown RAM → 0.0 (caller treats as 'undetected').
        """
        if self.has_discrete_gpu:
            return round(self.vram_gb * 0.8, 1)
        if self.ram_gb:
            return round(self.ram_gb * 0.6, 1)
        return 0.0


@dataclass(frozen=True)
class ModelRecommendation:
    size_class: str          # human label, e.g. "~8B"
    params_b_max: int        # upper bound of the param class, in billions
    quant: str               # e.g. "Q4_K_M"
    runtime: str             # "llamafile"
    est_download_gb: float
    candidate_families: list[str] = field(default_factory=list)
    rationale: str = ""
    provisional: bool = True  # NEVER asserted-best; promote only after qualification


# --- probes (monkeypatched in tests; each is best-effort and never raises) ---

def _detect_ram_gb() -> float | None:
    try:
        import psutil  # type: ignore

        return round(psutil.virtual_memory().total / 1e9, 1)
    except Exception:
        pass
    try:  # Linux
        with open("/proc/meminfo") as fh:
            for line in fh:
                if line.startswith("MemTotal:"):
                    return round(int(line.split()[1]) * 1024 / 1e9, 1)
    except Exception:
        pass
    try:  # POSIX fallback (Linux/macOS)
        return round(os.sysconf("SC_PAGE_SIZE") * os.sysconf("SC_PHYS_PAGES") / 1e9, 1)
    except (ValueError, OSError, AttributeError):
        return None


def _detect_gpu() -> tuple[float, str | None]:
    """Return (vram_gb, gpu_name). (0.0, None) when no discrete GPU is found."""
    try:
        import torch  # type: ignore

        if torch.cuda.is_available() and torch.cuda.device_count() > 0:
            props = torch.cuda.get_device_properties(0)
            return round(props.total_memory / 1e9, 1), torch.cuda.get_device_name(0)
    except Exception:
        pass
    smi = shutil.which("nvidia-smi")
    if smi:
        try:
            out = subprocess.run(
                [smi, "--query-gpu=memory.total,name", "--format=csv,noheader,nounits"],
                capture_output=True, text=True, timeout=5,
            )
            if out.returncode == 0 and out.stdout.strip():
                first = out.stdout.strip().splitlines()[0]
                mem_mib, name = (p.strip() for p in first.split(",", 1))
                return round(float(mem_mib) / 1024, 1), name
        except (OSError, ValueError, subprocess.SubprocessError):
            pass
    return 0.0, None


def detect_hardware() -> HardwareProfile:
    system = platform.system()
    arch = platform.machine()
    cpu = os.cpu_count() or 1
    ram = _detect_ram_gb()
    vram, gpu = _detect_gpu()
    unified = system == "Darwin" and arch in ("arm64", "aarch64")
    return HardwareProfile(
        system=system, arch=arch, cpu_count=cpu,
        ram_gb=ram, vram_gb=vram, gpu_name=gpu, unified_memory=unified,
    )


# Budget (GB) → (size label, param-class upper bound B, approx GGUF download GB).
# Ascending; first row whose threshold the budget does NOT exceed is chosen.
_TIERS: tuple[tuple[float, str, int, float], ...] = (
    (3.0, "≤1B", 1, 0.8),
    (6.0, "~3B", 3, 2.2),
    (12.0, "~8B", 8, 5.0),
    (24.0, "~14B", 14, 9.0),
    (float("inf"), "~32B", 32, 20.0),
)


def recommend_model(profile: HardwareProfile) -> ModelRecommendation:
    budget = profile.model_budget_gb
    for threshold, label, params, dl in _TIERS:
        if budget < threshold:
            size_class, params_b_max, est = label, params, dl
            break
    else:  # pragma: no cover - inf sentinel guarantees a match
        size_class, params_b_max, est = _TIERS[-1][1], _TIERS[-1][2], _TIERS[-1][3]

    where = (
        f"{profile.vram_gb:g}GB VRAM ({profile.gpu_name})" if profile.has_discrete_gpu
        else f"{profile.ram_gb:g}GB RAM (CPU inference — expect slower generation; "
             f"favor the smaller end and a low context size)" if profile.ram_gb
             else "undetected memory (treating conservatively)"
    )
    rationale = (
        f"budget ~{budget:.0f}GB from {where}. Recommending a {size_class}-class "
        f"instruct model at {('Q4_K_M')}. This is provisional — qualify it on YOUR box "
        f"(run the candidate against the real envelope schema and check the pass rate) "
        f"before trusting it as Tier-1; the model family is your pick, not a verified default."
    )
    families = ["Qwen2.5", "Gemma", "Llama", "Phi"] if params_b_max >= 3 else ["Qwen2.5", "Gemma"]
    return ModelRecommendation(
        size_class=size_class,
        params_b_max=params_b_max,
        quant="Q4_K_M",
        runtime="llamafile",
        est_download_gb=est,
        candidate_families=families,
        rationale=rationale,
        provisional=True,
    )
