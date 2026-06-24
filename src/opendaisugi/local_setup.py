"""Local Tier-1 qualification gate + persisted config wiring.

A hardware recommendation is only a hypothesis. Before a local model is wired as
Tier-1, ``qualify_local_model`` runs a battery of representative tasks through
the REAL provider path (``provider.generate_envelope`` — instructor Mode.JSON
against the local endpoint) and measures the rate of valid-``Envelope``
production. Only a model that clears the threshold is promoted; a flaky model is
rejected, which is the whole point — openDaisugi can't centrally validate every
coworker's box, so the per-box qualification run is what makes the recommendation
safe to trust.

``write_tier1_config`` / ``load_configured_tier1`` persist the qualified choice
so ``daisugi onboard`` / ``tend`` defer to it automatically.
"""

from __future__ import annotations

import json
import logging
from dataclasses import dataclass, field
from pathlib import Path
from typing import TYPE_CHECKING

from opendaisugi.models import Envelope

if TYPE_CHECKING:
    from opendaisugi.tier1 import Tier1Provider

_log = logging.getLogger("opendaisugi.local_setup")

# A small battery spanning envelope shapes (shell-delete, file-read, file-write).
# Kept short so qualification is fast even on slow CPU inference.
DEFAULT_PROBE_TASKS: tuple[str, ...] = (
    "Delete .tmp files older than 7 days in /var/log",
    "Read /data/sales.csv and print the row count",
    "List the running processes and save them to processes.txt",
)

_TIER1_CONFIG = "local_tier1.json"


@dataclass(frozen=True)
class QualificationResult:
    attempts: int
    valid: int
    pass_rate: float
    passed: bool
    threshold: float
    outcomes: list[tuple[str, str]] = field(default_factory=list)  # (task, valid|declined|error)


async def qualify_local_model(
    provider: "Tier1Provider",
    *,
    probe_tasks: "list[str] | tuple[str, ...] | None" = None,
    threshold: float = 0.8,
    repeats: int = 1,
) -> QualificationResult:
    """Run the probe battery through ``provider`` and decide whether to promote it.

    ``valid`` = the provider returned a real ``Envelope``; ``declined`` = it
    returned ``None`` (instructor parse failure / model flaked); ``error`` = the
    call raised. ``repeats`` samples each task N times so a *probabilistic* model
    is estimated, not asked once. Promotion requires ``pass_rate >= threshold``.
    """
    tasks = list(probe_tasks) if probe_tasks is not None else list(DEFAULT_PROBE_TASKS)
    outcomes: list[tuple[str, str]] = []
    valid = 0
    for _ in range(max(1, repeats)):
        for task in tasks:
            try:
                env = await provider.generate_envelope(task)
            except Exception as exc:  # a live endpoint hiccup is a failed attempt, not a crash
                _log.info("qualify: task %r errored: %s", task, exc)
                outcomes.append((task, "error"))
                continue
            if isinstance(env, Envelope):
                valid += 1
                outcomes.append((task, "valid"))
            else:
                outcomes.append((task, "declined"))
    attempts = len(tasks) * max(1, repeats)
    rate = round(valid / attempts, 2) if attempts else 0.0
    return QualificationResult(
        attempts=attempts,
        valid=valid,
        pass_rate=rate,
        passed=attempts > 0 and rate >= threshold,
        threshold=threshold,
        outcomes=outcomes,
    )


def tier1_config_path(data_dir: "str | Path") -> Path:
    return Path(data_dir) / _TIER1_CONFIG


def write_tier1_config(data_dir: "str | Path", *, model: str, base_url: str | None) -> Path:
    """Persist the qualified local model so the CLI wires it as Tier-1."""
    path = tier1_config_path(data_dir)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps({"model": model, "base_url": base_url}, indent=2) + "\n")
    return path


def load_configured_tier1(data_dir: "str | Path") -> "Tier1Provider | None":
    """Build a Tier-1 provider from a persisted local config, or None if absent/invalid."""
    path = tier1_config_path(data_dir)
    if not path.exists():
        return None
    try:
        cfg = json.loads(path.read_text())
    except (json.JSONDecodeError, OSError) as exc:
        _log.warning("ignoring invalid %s: %s", path, exc)
        return None
    model = cfg.get("model")
    if not model:
        return None
    from opendaisugi.tier1 import LiteLLMTier1Provider

    return LiteLLMTier1Provider(model=model, base_url=cfg.get("base_url"))
