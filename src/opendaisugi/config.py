"""User configuration for opendaisugi.

Loads from and saves to ``~/.opendaisugi/config.yaml``. The Daisugi facade
constructor kwargs override whatever is loaded from disk — config.yaml is
a default source, not an authoritative one.
"""

from __future__ import annotations

from collections.abc import Callable
from pathlib import Path

import yaml
from pydantic import BaseModel, Field


class Config(BaseModel):
    """Typed config with sensible defaults for every field."""

    model: str = "anthropic/claude-sonnet-4-20250514"
    max_task_chars: int = 4000
    z3_timeout_ms: int = 500
    data_dir: Path = Field(default_factory=lambda: Path.home() / ".opendaisugi")
    # Background distillation consent (Phase A). None = never asked (distinct
    # from an explicit no); True = distil repeated tasks in the background;
    # False = declined. Distillation only ever affects *efficiency* (the guard
    # enforces safety regardless), so it is safe to automate once consented.
    auto_tend: bool | None = None


def default_config() -> Config:
    """Return a Config populated entirely from field defaults."""
    return Config()


def load_config(path: Path | None = None) -> Config:
    """Load config from ``path`` (default: ``~/.opendaisugi/config.yaml``).

    Returns ``default_config()`` when the file does not exist. Unknown keys
    in the YAML file are silently ignored so that a config written by a
    newer version of opendaisugi still loads on an older version.
    """
    if path is None:
        path = Path.home() / ".opendaisugi" / "config.yaml"
    if not path.exists():
        return default_config()

    raw = yaml.safe_load(path.read_text()) or {}
    known = {f for f in Config.model_fields}
    filtered = {k: v for k, v in raw.items() if k in known}
    return Config(**filtered)


def save_config(config: Config, path: Path | None = None) -> None:
    """Write ``config`` to ``path`` as YAML, creating parent dirs if needed.

    ``Path`` values are serialized as strings. No atomic-write ceremony —
    config.yaml is user-editable and written rarely.
    """
    if path is None:
        path = Path.home() / ".opendaisugi" / "config.yaml"
    path.parent.mkdir(parents=True, exist_ok=True)
    data = config.model_dump(mode="json")
    # Pydantic serializes Path to str in mode="json"; yaml.safe_dump is fine with it.
    path.write_text(yaml.safe_dump(data, sort_keys=True))


def auto_tend_enabled(config: Config) -> bool:
    """True only when the user has explicitly consented to background distillation.

    Unasked (None) and declined (False) both mean "do not auto-tend" — consent
    is opt-in, never assumed.
    """
    return config.auto_tend is True


def ensure_auto_tend_consent(
    config: Config,
    ask: Callable[[], bool],
    *,
    path: Path | None = None,
) -> Config:
    """Ask once whether to distil in the background, persist the answer, return it.

    If ``config.auto_tend`` is already set (True or False), returns ``config``
    unchanged without calling ``ask`` — the question is asked exactly once, ever.
    Otherwise ``ask()`` is invoked (a ``() -> bool`` the caller wires to a prompt),
    the choice is written to ``config`` on disk, and the updated Config returned.
    """
    if config.auto_tend is not None:
        return config
    decided = config.model_copy(update={"auto_tend": bool(ask())})
    save_config(decided, path)
    return decided
