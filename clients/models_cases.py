"""Synthetic model-catalog cases: `daisugi models list|search|use`, run
through the Python oracle.

    uv run --no-sync python clients/models_cases.py [--out clients/fixtures/models] [--only NAME]

A case runs as a garden case does (clients/garden_cases.py): a scratch
HOME, the tree laid out, one command, and the exit code, stdout, stderr
and the tree after it recorded. The hardware the default is picked by
comes from OPENDAISUGI_VOICE_HARDWARE, never the real box.

`models search` never reaches the real Hugging Face API. HF_ENDPOINT
names a fake on 127.0.0.1 ({HF}) that answers GET requests from the
case's `hf` table, keyed by the exact request target, and logs each
target. A target with no recorded answer gets HTTP 597, and recording
fails loudly, so a request that differs by one byte shows up. {DEAD} is
a loopback port nothing listens on. Every id, path and answer is
synthetic.
"""

from __future__ import annotations

import argparse
import http.server
import json
import os
import shutil
import sys
import threading
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))

import garden_cases  # noqa: E402 - sibling module, run as a script
from garden_cases import PY_CLI, body_id, write_jsonl  # noqa: E402
from pathway_cases import REPO  # noqa: E402
from ports import PortPool, sub_number  # noqa: E402

FIXTURE_DIR = REPO / "clients" / "fixtures" / "models"
SCRATCH = Path(
    os.environ.get("DAISUGI_MODELS_SCRATCH")
    or Path.home() / "opendaisugi-scratch" / "models" / "runs"
)
CASE_VERSION = 1
CHOICE = ".opendaisugi/garden_model.json"
DESK = {"OPENDAISUGI_VOICE_HARDWARE": "16,8,0"}
SMALL = {"OPENDAISUGI_VOICE_HARDWARE": "4,2,0"}


def cmd_for(case: dict[str, Any], binary: str | None) -> list[str]:
    return PY_CLI if binary is None else [binary]


# ---------------------------------------------------------------------------
# The fake Hugging Face API
# ---------------------------------------------------------------------------


class FakeHF:
    """Answers GET <target> from a table keyed by the exact target."""

    def __init__(self, table: dict[str, dict[str, Any]], log: list[str]):
        outer = self
        self.table, self.log = table, log

        class H(http.server.BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def do_GET(self):  # noqa: N802 - http.server's name
                outer.log.append(self.path)
                ans = outer.table.get(self.path)
                if ans is None:
                    status, out = 597, b'{"error": "no recorded answer"}'
                else:
                    status, out = ans.get("status", 200), ans["body"].encode("utf-8")
                try:
                    self.send_response(status)
                    self.send_header("content-type", "application/json")
                    self.send_header("content-length", str(len(out)))
                    self.end_headers()
                    self.wfile.write(out)
                except (BrokenPipeError, ConnectionResetError):
                    pass

            def log_message(self, *a):
                pass

        self.httpd = http.server.ThreadingHTTPServer(("127.0.0.1", 0), H)
        self.httpd.daemon_threads = True
        self.port = self.httpd.server_address[1]
        self.thread = threading.Thread(target=self.httpd.serve_forever, daemon=True)

    def __enter__(self):
        self.thread.start()
        return self

    def __exit__(self, *a):
        self.httpd.shutdown()
        self.httpd.server_close()


def run_case(case: dict[str, Any], cmd: list[str], work: Path) -> dict[str, Any]:
    """garden_cases.run_case with the fake API up and HF_ENDPOINT on it.
    The targets asked are recorded as ``hf``; both ports become
    placeholders."""
    from opendaisugi.model_catalog import search_target

    table = {search_target(q): ans for q, ans in (case.get("hf") or {}).items()}
    log: list[str] = []
    pool = PortPool()
    dead = pool.take()
    with FakeHF(table, log) as hf:
        ports = {"{HF}": str(hf.port), "{DEAD}": str(dead)}
        env = {"HF_ENDPOINT": "http://127.0.0.1:{HF}", **(case.get("env") or {})}
        for k, p in ports.items():
            env = {n: v.replace(k, p) for n, v in env.items()}
        res = garden_cases.run_case({**case, "env": env}, cmd, work)
    pool.release()
    res["hf"] = log
    text = json.dumps(res)
    for k, p in sorted(ports.items(), key=lambda kv: -len(kv[1])):
        text = sub_number(text, p, k)
    return json.loads(text)


# ---------------------------------------------------------------------------
# Cases
# ---------------------------------------------------------------------------


def models(name: str, *argv: str, **kw: Any) -> dict[str, Any]:
    c: dict[str, Any] = {
        "kind": "cli",
        "name": name,
        "argv": ["models", *argv],
        "before": {},
        "unset": ["HF_HUB_OFFLINE"],
    }
    c.update(kw)
    return c


def choice(model: str, at: str = CHOICE) -> dict[str, Any]:
    return {at: {"text": json.dumps({"model": model}, indent=2) + "\n"}}


def listing(*rows: Any) -> dict[str, Any]:
    return {"body": json.dumps(list(rows))}


GRANITE = [
    {
        "id": "ibm-granite/granite-4.1-3b",
        "cardData": {"license": "apache-2.0"},
        "safetensors": {"total": 3402836480},
        "pipeline_tag": "text-generation",
        "tags": ["license:apache-2.0"],
    },
    {
        "id": "someone/granite-4.1-3b-GGUF",
        "cardData": {"license": "apache-2.0"},
        "gguf": {"total": 3402836480, "context_length": 131072, "architecture": "granite"},
        "tags": ["gguf", "license:apache-2.0"],
    },
    {
        "id": "ibm-granite/granite-4.1-30b",
        "cardData": {"license": "apache-2.0"},
        "safetensors": {"total": 28865728512},
        "pipeline_tag": "text-generation",
    },
    {"id": "acme/granite-tuned", "cardData": {"license": "other", "license_name": "acme-1"}},
    {
        "id": "acme/granite-mit",
        "cardData": {"license": ["mit", "apache-2.0"]},
        "safetensors": {"total": 1200000000},
        "tags": ["license:mit"],
        "pipeline_tag": "text-generation",
    },
    {"id": "acme/float-size", "safetensors": {"total": 3e9}, "cardData": {"license": "mit"}},
    {"id": "acme/bool-size", "safetensors": {"total": True}, "gguf": "not an object"},
    {"no": "id"},
    7,
    {"id": 9},
]
QWEN = [
    {
        "id": "Qwen/Qwen2.5-1.5B-Instruct",
        "cardData": {"license": "apache-2.0"},
        "safetensors": {"total": 1543714304},
        "pipeline_tag": "text-generation",
    },
    {
        "id": "Qwen/Qwen2.5-0.5B-Instruct-GGUF",
        "cardData": {"license": "apache-2.0"},
        "gguf": {"total": 494032768, "context_length": 32768},
        "tags": ["gguf"],
    },
]


def build_cases() -> list[dict[str, Any]]:
    C: list[dict[str, Any]] = []
    add = C.append

    # -- list -------------------------------------------------------------
    add(models("list desk", "list", env=DESK))
    add(models("list desk json", "list", "--json", env=DESK))
    add(models("list small", "list", env=SMALL))
    add(models("list small json", "list", "--json", env=SMALL))
    add(models("list gpu", "list", env={"OPENDAISUGI_VOICE_HARDWARE": "4,2,8"}))
    add(models("list gpu json", "list", "--json", env={"OPENDAISUGI_VOICE_HARDWARE": "4,2,8"}))
    add(models("list unknown ram", "list", env={"OPENDAISUGI_VOICE_HARDWARE": ",1,0"}))
    add(
        models(
            "list unknown ram json", "list", "--json", env={"OPENDAISUGI_VOICE_HARDWARE": ",1,0"}
        )
    )
    add(models("list fractional ram", "list", env={"OPENDAISUGI_VOICE_HARDWARE": "15.5,4,0"}))
    add(models("list just under", "list", env={"OPENDAISUGI_VOICE_HARDWARE": "7.9,8,5.9"}))
    add(models("list exactly at", "list", env={"OPENDAISUGI_VOICE_HARDWARE": "8,1,0"}))
    add(models("list gpu at 6", "list", env={"OPENDAISUGI_VOICE_HARDWARE": "2,1,6"}))
    add(models("list with a choice", "list", env=DESK, before=choice("acme/my-model")))
    add(
        models(
            "list with a choice json",
            "list",
            "--json",
            env=SMALL,
            before=choice("Qwen/Qwen2.5-1.5B-Instruct"),
        )
    )
    add(models("list bad choice", "list", env=DESK, before={CHOICE: {"text": "{bad"}}))
    add(
        models(
            "list choice not a string", "list", env=DESK, before={CHOICE: {"text": '{"model": 5}'}}
        )
    )
    add(models("list choice blank", "list", env=DESK, before={CHOICE: {"text": '{"model": "  "}'}}))
    add(models("list choice a list", "list", env=DESK, before={CHOICE: {"text": '["x"]'}}))
    add(
        models(
            "list choice non-ascii",
            "list",
            "--json",
            env=DESK,
            before=choice("acme/modèle"),
        )
    )
    add(
        models(
            "list data dir",
            "list",
            "--data-dir",
            "{HOME}/d",
            env=DESK,
            before=choice("acme/elsewhere", "d/garden_model.json"),
        )
    )
    add(models("list extra arg", "list", "x", env=DESK))
    add(models("list bad option", "list", "--nope", env=DESK))

    # -- use --------------------------------------------------------------
    add(models("use granite", "use", "ibm-granite/granite-4.1-3b"))
    add(models("use qwen", "use", "Qwen/Qwen2.5-1.5B-Instruct"))
    add(models("use a gguf path", "use", "/m/own.gguf"))
    add(models("use a relative path", "use", "./models/x.gguf"))
    add(models("use a bare name", "use", "my-model"))
    add(models("use non-ascii", "use", "acme/modèle"))
    add(models("use with spaces", "use", " acme/x "))
    add(models("use blank", "use", " "))
    add(models("use empty", "use", ""))
    add(models("use replaces", "use", "acme/new", before=choice("acme/old")))
    add(models("use data dir", "use", "acme/x", "--data-dir", "{HOME}/a/b"))
    add(models("use missing id", "use"))
    add(models("use two ids", "use", "a/b", "c/d"))

    # -- search -----------------------------------------------------------
    add(models("search desk", "search", "granite", env=DESK, hf={"granite": listing(*GRANITE)}))
    add(
        models(
            "search desk json",
            "search",
            "granite",
            "--json",
            env=DESK,
            hf={"granite": listing(*GRANITE)},
        )
    )
    add(models("search small", "search", "granite", env=SMALL, hf={"granite": listing(*GRANITE)}))
    add(
        models(
            "search any size",
            "search",
            "granite",
            "--max-params",
            "0",
            env=DESK,
            hf={"granite": listing(*GRANITE)},
        )
    )
    add(
        models(
            "search any size json",
            "search",
            "granite",
            "--max-params",
            "0",
            "--json",
            env=DESK,
            hf={"granite": listing(*GRANITE)},
        )
    )
    add(
        models(
            "search max 3.5",
            "search",
            "granite",
            "--max-params",
            "3.5",
            env=SMALL,
            hf={"granite": listing(*GRANITE)},
        )
    )
    add(
        models(
            "search max negative",
            "search",
            "granite",
            "--max-params=-1",
            env=DESK,
            hf={"granite": listing(*GRANITE)},
        )
    )
    add(
        models(
            "search licenses",
            "search",
            "granite",
            "--license",
            "mit",
            "--license",
            "acme-1",
            "--max-params",
            "0",
            env=DESK,
            hf={"granite": listing(*GRANITE)},
        )
    )
    add(
        models(
            "search licenses json",
            "search",
            "granite",
            "--license",
            "apache-2.0",
            "--json",
            env=DESK,
            hf={"granite": listing(*GRANITE)},
        )
    )
    add(
        models(
            "search limit 1",
            "search",
            "granite",
            "--limit",
            "1",
            env=DESK,
            hf={"granite": listing(*GRANITE)},
        )
    )
    add(models("search qwen", "search", "qwen", env=DESK, hf={"qwen": listing(*QWEN)}))
    add(
        models(
            "search qwen json", "search", "qwen", "--json", env=SMALL, hf={"qwen": listing(*QWEN)}
        )
    )
    q = "granite é/4 & x+y"
    add(models("search odd query", "search", q, env=DESK, hf={q: listing(*QWEN)}))
    add(models("search no results", "search", "zzz", env=DESK, hf={"zzz": listing()}))
    add(
        models("search no results json", "search", "zzz", "--json", env=DESK, hf={"zzz": listing()})
    )
    add(
        models(
            "search all filtered",
            "search",
            "granite",
            "--license",
            "none-such",
            env=DESK,
            hf={"granite": listing(*GRANITE)},
        )
    )
    add(
        models(
            "search status 500",
            "search",
            "x",
            env=DESK,
            hf={"x": {"status": 500, "body": '{"error":"busy"}'}},
        )
    )
    add(
        models(
            "search status 404",
            "search",
            "x",
            "--json",
            env=DESK,
            hf={"x": {"status": 404, "body": "nope"}},
        )
    )
    add(models("search not json", "search", "x", env=DESK, hf={"x": {"body": "<html>"}}))
    add(models("search an object", "search", "x", env=DESK, hf={"x": {"body": '{"id": "a/b"}'}}))
    add(models("search a string", "search", "x", env=DESK, hf={"x": {"body": '"a"'}}))
    add(models("search empty body", "search", "x", env=DESK, hf={"x": {"body": ""}}))
    add(
        models(
            "search offline",
            "search",
            "x",
            env={**DESK, "HF_ENDPOINT": "http://127.0.0.1:{DEAD}"},
        )
    )
    add(
        models(
            "search offline json",
            "search",
            "x",
            "--json",
            env={**DESK, "HF_ENDPOINT": "http://127.0.0.1:{DEAD}"},
        )
    )
    add(
        models(
            "search hf hub offline", "search", "x", env={**DESK, "HF_HUB_OFFLINE": "1"}, unset=[]
        )
    )
    add(
        models(
            "search hf hub offline yes",
            "search",
            "x",
            env={**DESK, "HF_HUB_OFFLINE": "Yes"},
            unset=[],
        )
    )
    add(
        models(
            "search hf hub offline 0",
            "search",
            "x",
            env={**DESK, "HF_HUB_OFFLINE": "0"},
            unset=[],
            hf={"x": listing(*QWEN)},
        )
    )
    add(
        models(
            "search trailing slash",
            "search",
            "x",
            env={**DESK, "HF_ENDPOINT": "http://127.0.0.1:{HF}/"},
            hf={"x": listing(*QWEN)},
        )
    )
    add(models("search limit 0", "search", "x", "--limit", "0", env=DESK))
    add(models("search limit 101", "search", "x", "--limit", "101", env=DESK))
    add(models("search limit word", "search", "x", "--limit", "many", env=DESK))
    add(models("search max word", "search", "x", "--max-params", "big", env=DESK))
    add(models("search missing query", "search", env=DESK))
    return C


def all_cases() -> list[dict[str, Any]]:
    cases = build_cases()
    names = [c["name"] for c in cases]
    dup = {n for n in names if names.count(n) > 1}
    if dup:
        raise SystemExit(f"duplicate case names: {sorted(dup)}")
    return cases


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--out", type=Path, default=FIXTURE_DIR)
    ap.add_argument(
        "--only", help="run only cases whose name holds this text; print, write nothing"
    )
    ap.add_argument("--fresh", action="store_true", help="rerun every case")
    args = ap.parse_args()
    out_dir: Path = args.out
    out_dir.mkdir(parents=True, exist_ok=True)
    SCRATCH.mkdir(parents=True, exist_ok=True)
    cases = all_cases()
    old: dict[str, dict[str, Any]] = {}
    if (out_dir / "cases.jsonl").exists() and not args.fresh:
        for ln in (out_dir / "cases.jsonl").read_text(encoding="utf-8").splitlines():
            if ln.strip():
                c = json.loads(ln)
                old[c["id"]] = c
    for i, c in enumerate(cases):
        if args.only and args.only not in c["name"]:
            continue
        prev = old.get(body_id(c))
        if prev is not None and not args.only:
            c["expect"] = prev["expect"]
            continue
        c["expect"] = run_case(c, cmd_for(c, None), SCRATCH / "gen" / f"{i:04d}")
        if "597" in c["expect"]["stdout"] + "\n".join(c["expect"]["stderr"]):
            raise SystemExit(f"{c['name']}: the fake API had no answer for {c['expect']['hf']}")
        if args.only:
            print(
                json.dumps({"name": c["name"], **c["expect"]}, indent=1, ensure_ascii=False)[:8000]
            )
        else:
            print(f"{i + 1:4d}/{len(cases)} exit={c['expect']['exit']} {c['name']}", flush=True)
    if args.only:
        return 0
    manifest = {"v": CASE_VERSION}
    manifest["cases.jsonl"] = write_jsonl(out_dir / "cases.jsonl", cases)
    (out_dir / "manifest.json").write_text(
        json.dumps(manifest, indent=1, sort_keys=True) + "\n", encoding="utf-8"
    )
    shutil.rmtree(SCRATCH / "gen", ignore_errors=True)
    from fixture_paths import leaks

    leaked = leaks()
    if leaked:
        raise SystemExit("a machine path reached the fixtures:\n" + "\n".join(leaked[:10]))
    print(f"wrote {len(cases)} cases to {out_dir}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
