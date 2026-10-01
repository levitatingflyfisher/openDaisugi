"""Best-effort hardware detection + size-based local-model recommendation.

Powers ``daisugi tiers setup``: detect what the box can run, then recommend a local
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
from collections.abc import Mapping
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
    size_class: str  # human label, e.g. "~8B"
    params_b_max: int  # upper bound of the param class, in billions
    quant: str  # e.g. "Q4_K_M"
    runtime: str  # "llamafile"
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
    return detect_gpu_smi()


def detect_gpu_smi() -> tuple[float, str | None]:
    """(vram_gb, gpu_name) from nvidia-smi alone, with no torch import.
    (0.0, None) when there is none. The voice bridge reads this one."""
    smi = shutil.which("nvidia-smi")
    if smi:
        try:
            out = subprocess.run(
                [smi, "--query-gpu=memory.total,name", "--format=csv,noheader,nounits"],
                capture_output=True,
                text=True,
                timeout=5,
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
        system=system,
        arch=arch,
        cpu_count=cpu,
        ram_gb=ram,
        vram_gb=vram,
        gpu_name=gpu,
        unified_memory=unified,
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
        f"{profile.vram_gb:g}GB VRAM ({profile.gpu_name})"
        if profile.has_discrete_gpu
        else f"{profile.ram_gb:g}GB RAM (CPU inference — expect slower generation; "
        f"favor the smaller end and a low context size)"
        if profile.ram_gb
        else "undetected memory (treating conservatively)"
    )
    rationale = (
        f"budget ~{budget:.0f}GB from {where}. Recommending a {size_class}-class "
        f"instruct model at {('Q4_K_M')}. This is provisional — qualify it on YOUR box "
        f"(run the candidate against the real envelope schema and check the pass rate) "
        f"before trusting it as Tier-1; the model family is your pick, not a verified default."
    )
    families = (
        ["Granite", "Ministral", "Gemma", "Llama"] if params_b_max >= 3 else ["Granite", "Llama"]
    )
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


# The engine a box with no voice choice gets, by its hardware (ruling
# VO-17). Parakeet wants a desktop: 8 GB of RAM leaves about 2 GB for its
# model and runtime, and it decodes on 4 cores. Moonshine small wants
# about 0.5 GB.
PARAKEET_MIN_RAM_GB = 8.0
PARAKEET_MIN_CPUS = 4
MOONSHINE_MIN_RAM_GB = 2.0
# A test sets this to "RAM_GB,CPUS,VRAM_GB" (RAM_GB may be empty for
# unknown), so no test or case reads the real box. The voice engine
# choice and the model catalog's default read it; nothing else does.
HARDWARE_ENV = "OPENDAISUGI_VOICE_HARDWARE"

# The names the voice package's pins give these (a test checks they
# agree): the layer may not import the voice package (ADR-0020).
PARAKEET_DEFAULT_MODEL = "v2"
MOONSHINE_DEFAULT_MODEL = "small"
PARAKEET_BINARY = "parakeet-cli"
MOONSHINE_BINARY = "moonshine-cli"

_LABELS = {
    ("parakeet", PARAKEET_DEFAULT_MODEL): "Parakeet v2",
    ("moonshine", MOONSHINE_DEFAULT_MODEL): "Moonshine small",
    ("faster-whisper", "tiny.en"): "faster-whisper tiny.en",
}
# The program each engine of the tiers needs. faster-whisper is never
# skipped: it is a tier only where its package imports.
_NEEDS = {"parakeet": PARAKEET_BINARY, "moonshine": MOONSHINE_BINARY}


@dataclass(frozen=True)
class VoiceHardware:
    ram_gb: float | None
    cpus: int
    vram_gb: float


def detect_voice_hardware(env: Mapping[str, str] | None = None) -> VoiceHardware:
    """Total RAM, CPUs online and GPU memory, from the probe tiers setup uses.
    ``env`` (default os.environ) is read for HARDWARE_ENV only."""
    raw = (os.environ if env is None else env).get(HARDWARE_ENV)
    if raw:
        ram, cpus, vram = raw.split(",")
        return VoiceHardware(float(ram) if ram else None, int(cpus), float(vram))
    return VoiceHardware(_detect_ram_gb(), os.cpu_count() or 1, detect_gpu_smi()[0])


def hardware_order(hw: VoiceHardware) -> list[str]:
    """The tiers this hardware can run, best first: parakeet, moonshine, tiny."""
    ram = hw.ram_gb
    if ram is not None and ram >= PARAKEET_MIN_RAM_GB and hw.cpus >= PARAKEET_MIN_CPUS:
        return ["parakeet", "moonshine", "tiny"]
    if ram is not None and ram >= MOONSHINE_MIN_RAM_GB:
        return ["moonshine", "tiny"]
    return ["tiny"]


def _hardware_text(hw: VoiceHardware) -> str:
    ram = "an unknown amount of RAM" if hw.ram_gb is None else f"{hw.ram_gb:g} GB of RAM"
    cores = "1 core" if hw.cpus == 1 else f"{hw.cpus} cores"
    text = f"{ram} and {cores}"
    if hw.vram_gb > 0:
        text += f", and a GPU with {hw.vram_gb:g} GB (no GPU engine is built yet)"
    return text


def choose_engine(
    hw: VoiceHardware, installed: set[str], faster_whisper: bool
) -> tuple[str, str, str]:
    """The engine, model and one line for a box with no voice choice.

    The tiers come from the hardware. The tiny tier is faster-whisper
    tiny.en where its package imports, else Moonshine small (whisper.cpp
    has no model this project fetches). The first tier that is installed
    wins; with none installed, the first tier, whose engine then says what
    is missing. ``installed`` holds the engine names whose program is on
    PATH or whose package imports.
    """
    picks: list[tuple[str, str]] = []
    for tier in hardware_order(hw):
        if tier == "parakeet":
            pick = ("parakeet", PARAKEET_DEFAULT_MODEL)
        elif tier == "moonshine" or not faster_whisper:
            pick = ("moonshine", MOONSHINE_DEFAULT_MODEL)
        else:
            pick = ("faster-whisper", "tiny.en")
        if pick not in picks:
            picks.append(pick)
    chosen = next((p for p in picks if p[0] in installed), picks[0])
    line = (
        f"No voice engine is set, so voice uses {_LABELS[chosen]}: "
        f"this box has {_hardware_text(hw)}."
    )
    if chosen != picks[0]:
        line += f" {_LABELS[picks[0]]} would come first, but {_NEEDS[picks[0][0]]} is not on PATH."
    return chosen[0], chosen[1], line
