"""Resolve the bundled skill directory and install it into a harness path.

The package ships exactly one real skill directory. We symlink that directory
into each harness's discovery path so there is a single source of truth that
auto-updates on `uv add --upgrade`. The only case that cannot symlink is a
zipimport install (package left zipped, no real filesystem path); there we
copy the tree out instead.
"""
from __future__ import annotations

import importlib.resources as _ir
import shutil
from collections.abc import Callable
from pathlib import Path


def resolve_skill_dir() -> Path:
    """Return the real on-disk path of the bundled skill directory.

    Raises FileNotFoundError if the traversable has no real filesystem path
    (zipimport) — callers that need a path use SkillInstaller, which falls
    back to materializing a copy.
    """
    ref = _ir.files("opendaisugi").joinpath("skills", "opendaisugi-checklist")
    path = Path(str(ref))
    if not path.is_dir():
        raise FileNotFoundError(f"skill dir not a real path: {ref!r}")
    return path


class SkillInstaller:
    """Install the skill dir into a target path: symlink, or copy as fallback."""

    def __init__(
        self,
        source: Path,
        *,
        materialize: Callable[[Path], None] | None = None,
    ) -> None:
        self._source = source
        self._materialize = materialize

    def link(self, target: Path) -> Path:
        """Create (idempotently) a symlink target -> source, or a copy.

        If the source is a real directory, symlink. Otherwise materialize a
        copy at target via the injected materialize callable (or importlib's
        as_file in production).
        """
        target.parent.mkdir(parents=True, exist_ok=True)

        if self._source.is_dir():
            if target.is_symlink() and target.resolve() == self._source.resolve():
                return target  # already correct
            _clear(target)
            target.symlink_to(self._source, target_is_directory=True)
            return target

        # zipimport fallback: copy the tree out.
        _clear(target)
        if self._materialize is not None:
            self._materialize(target)
        else:  # production: copy from the importlib traversable
            ref = _ir.files("opendaisugi").joinpath("skills", "opendaisugi-checklist")
            with _ir.as_file(ref) as real:
                shutil.copytree(real, target)
        return target


def _clear(target: Path) -> bool:
    """Remove an existing target (symlink, file, or directory). Symlink-safe:
    unlinks a symlink rather than following it. Returns True if it removed
    something, False if the target was absent.
    """
    if target.is_symlink() or target.exists():
        if target.is_dir() and not target.is_symlink():
            shutil.rmtree(target)
        else:
            target.unlink()
        return True
    return False
