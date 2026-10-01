"""huggingface_hub's telemetry is off in every Python path that can import it.

Unless HF_HUB_DISABLE_TELEMETRY is set when it is imported, huggingface_hub
fetches its agent list from the Hub (/api/agent-harnesses) and names the
agent that runs it in its User-Agent. The package sets it on import; the
pack worker, which runs by its path, sets it itself. A value the user set
is kept. The Go and Rust ports never send that header.
"""

import os
import re
import subprocess
import sys
from pathlib import Path

import pytest

REPO = Path(__file__).resolve().parents[1]
WORKER = REPO / "src" / "opendaisugi" / "pack" / "worker.py"


def _env(**extra: str) -> dict[str, str]:
    env = {k: v for k, v in os.environ.items() if k not in ("HF_HUB_DISABLE_TELEMETRY",)}
    env.update(extra)
    return env


def _run(code: str, env: dict[str, str], *flags: str) -> str:
    out = subprocess.run(
        [sys.executable, *flags, "-c", code],
        env=env,
        capture_output=True,
        text=True,
        check=True,
        timeout=120,
    )
    return out.stdout.strip()


def test_package_import_turns_telemetry_off():
    code = "import os, opendaisugi; print(os.environ.get('HF_HUB_DISABLE_TELEMETRY'))"
    assert _run(code, _env()) == "1"


def test_package_import_keeps_a_value_the_user_set():
    code = "import os, opendaisugi; print(os.environ.get('HF_HUB_DISABLE_TELEMETRY'))"
    assert _run(code, _env(HF_HUB_DISABLE_TELEMETRY="0")) == "0"


def test_package_import_leaves_the_implicit_token_alone():
    code = "import os, opendaisugi; print(os.environ.get('HF_HUB_DISABLE_IMPLICIT_TOKEN'))"
    env = _env()
    env.pop("HF_HUB_DISABLE_IMPLICIT_TOKEN", None)
    assert _run(code, env) == "None"


def test_huggingface_hub_sends_no_agent_name(tmp_path):
    pytest.importorskip("huggingface_hub")
    code = (
        "import opendaisugi\n"
        "from huggingface_hub import constants\n"
        "from huggingface_hub.utils import build_hf_headers\n"
        "ua = build_hf_headers(token=False)['user-agent']\n"
        "print(constants.HF_HUB_DISABLE_TELEMETRY, 'agent/' in ua)\n"
    )
    # CLAUDECODE=1 is what huggingface_hub reads to name Claude Code.
    env = _env(HF_HOME=str(tmp_path), HF_HUB_OFFLINE="1", CLAUDECODE="1")
    assert _run(code, env) == "True False"


def test_pack_worker_turns_telemetry_off_by_itself():
    # The worker runs as `python -I worker.py`: the package's __init__ never runs.
    code = (
        "import os, runpy\n"
        f"runpy.run_path({str(WORKER)!r}, run_name='worker')\n"
        "print(os.environ.get('HF_HUB_DISABLE_TELEMETRY'))\n"
    )
    assert _run(code, _env(), "-I") == "1"
    assert _run(code, _env(HF_HUB_DISABLE_TELEMETRY="false"), "-I") == "false"


def test_go_and_rust_never_fetch_the_agent_list_or_name_an_agent():
    roots = [REPO / "clients" / "go", REPO / "clients" / "rust" / "src"]
    hits = []
    for root in roots:
        for p in root.rglob("*"):
            if p.suffix not in (".go", ".rs") or not p.is_file():
                continue
            text = p.read_text(encoding="utf-8", errors="replace")
            if "agent-harnesses" in text or re.search(r"agent/\{|\"agent/\"", text):
                hits.append(str(p.relative_to(REPO)))
    assert hits == []
