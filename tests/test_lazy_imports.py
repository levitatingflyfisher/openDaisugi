"""The capture hook fires on every tool call, so `import opendaisugi` must not
eagerly drag in litellm/instructor (~2.4s). Those load lazily on first LLM use.
"""
from __future__ import annotations

import subprocess
import sys


def _fresh_import_check(snippet: str) -> subprocess.CompletedProcess:
    return subprocess.run([sys.executable, "-c", snippet], capture_output=True, text=True)


def test_import_opendaisugi_does_not_load_litellm():
    r = _fresh_import_check(
        "import opendaisugi, sys; "
        "assert 'litellm' not in sys.modules, 'litellm eagerly imported by opendaisugi'; "
        "print('ok')"
    )
    assert r.returncode == 0, r.stderr


def test_import_hook_does_not_load_instructor():
    r = _fresh_import_check(
        "from opendaisugi import hook; import sys; "
        "assert 'instructor' not in sys.modules, 'instructor eagerly imported via hook path'; "
        "print('ok')"
    )
    assert r.returncode == 0, r.stderr
