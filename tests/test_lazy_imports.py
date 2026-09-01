"""The capture hook fires on every tool call, so `import opendaisugi` must not
eagerly drag in the model client's HTTP stack (httpx). It loads lazily on
the first model call.
"""

from __future__ import annotations

import subprocess
import sys


def _fresh_import_check(snippet: str) -> subprocess.CompletedProcess:
    return subprocess.run([sys.executable, "-c", snippet], capture_output=True, text=True)


def test_import_opendaisugi_does_not_load_httpx():
    r = _fresh_import_check(
        "import opendaisugi, sys; "
        "assert 'httpx' not in sys.modules, 'httpx eagerly imported by opendaisugi'; "
        "print('ok')"
    )
    assert r.returncode == 0, r.stderr


def test_import_hook_and_model_client_do_not_load_httpx():
    r = _fresh_import_check(
        "from opendaisugi import hook, llm, llm_client, llm_check; import sys; "
        "assert 'httpx' not in sys.modules, 'httpx eagerly imported via hook path'; "
        "print('ok')"
    )
    assert r.returncode == 0, r.stderr
