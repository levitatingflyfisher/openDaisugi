"""The detached auto-tend starts the absolute entry point, as install writes it.

A hook runs with the agent's PATH, which often lacks the directory daisugi is
in. A bare `daisugi` there fails, quietly, with no tend.
"""

from __future__ import annotations

import subprocess

from opendaisugi import hook


def test_auto_tend_starts_the_absolute_entry_point(tmp_path, fake_daisugi_exe, monkeypatch):
    seen: list[list[str]] = []
    monkeypatch.setattr(subprocess, "Popen", lambda argv, **kw: seen.append(argv))
    hook._spawn_detached_auto_tend(tmp_path)
    assert seen == [[fake_daisugi_exe, "hook", "auto-tend", "--data-dir", str(tmp_path)]]
    assert seen[0][0].startswith("/")
