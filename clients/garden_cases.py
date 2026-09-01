"""Synthetic garden cases: `daisugi gardener ...`, `tend`, `hook auto-tend`
and `distill-repeats`, run through the Python oracle.

    uv run --no-sync python clients/garden_cases.py [--out clients/fixtures/garden] [--only NAME]

A case is a file tree, one command and, for the commands that call a
model, a table of recorded model answers. The runner lays the tree out as
a scratch HOME, runs the command there and records what it did: exit
code, stdout, stderr and the tree after it. Every database in the tree is
dumped whole: each table's schema and rows, and its user_version.

Model calls never reach a real model. The claude-code backend runs a fake
`claude` first on PATH; the litellm backend talks to a fake Anthropic
server on 127.0.0.1. Both answer from the case's table, keyed by the
SHA-256 of the exact request: for `claude`, the argv after the program and
the stdin; for the server, the request body. A request with no recorded
answer fails loudly (exit 97, HTTP 597), so a request that differs by one
byte shows up. The request bytes are kept in the fixture. The fake server
checks that each credential sent (x-api-key, authorization) is the case's
own, answers 596 when it is not, and keeps only a name for it, never its
value.

Times: a tree names times relative to the moment it is laid out
({"now": -86400} in a row, {NOWF:-86400} or {ISO:-86400} in text). Output
is normalized the same way on both sides: a time near now is written as
its offset from the start of the run, rounded to the minute; pathway, env
and plan ids a run mints at random, and trace ids, are renamed by order of
first appearance.

Every path in a case is fake: the scratch HOME is written {HOME}. The
cases hold no content from any real session and may be committed. Lines
are content-addressed and sorted, and a manifest pins each file's bytes.
"""

from __future__ import annotations

import argparse
import hashlib
import http.server
import json
import os
import re
import shutil
import sqlite3
import stat
import subprocess
import sys
import threading
import time
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))

from pathway_cases import (  # noqa: E402 - sibling module, run as a script
    FIXTURE_DIR as PATHWAY_FIXTURES,
)
from pathway_cases import (
    REPO,
    canonical_json,
    case_id,
    envelope,
    pathway,
    put_row,
    row_spec,
    unsparse,
)

FIXTURE_DIR = REPO / "clients" / "fixtures" / "garden"
SCRATCH = Path(
    os.environ.get("DAISUGI_GARDEN_SCRATCH") or Path.home() / "opendaisugi-scratch" / "d2" / "runs"
)
CASE_VERSION = 1
PY_CLI = [sys.executable, "-m", "opendaisugi.cli"]
DB = ".opendaisugi/pathways.db"
DAY = 86400.0

# ---------------------------------------------------------------------------
# Laying out a tree
# ---------------------------------------------------------------------------

_NOWF = re.compile(r"\{NOWF:(-?[0-9.]+)\}")
_ISO = re.compile(r"\{ISO:(-?[0-9.]+)\}")


def iso(t: float) -> str:
    return datetime.fromtimestamp(t, tz=timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def fill_text(s: str, home: str, t0: float) -> str:
    s = s.replace("{HOME}", home)
    s = _NOWF.sub(lambda m: repr(t0 + float(m.group(1))), s)
    return _ISO.sub(lambda m: iso(t0 + float(m.group(1))), s)


def fill_value(v: Any, home: str, t0: float) -> Any:
    if isinstance(v, dict) and set(v) == {"now"}:
        return t0 + float(v["now"])
    if isinstance(v, str):
        return fill_text(v, home, t0)
    return v


def lay_out_pathways(path: Path, spec: dict[str, Any], home: str, t0: float) -> None:
    from opendaisugi.pathway_store import _SCHEMA

    path.parent.mkdir(parents=True, exist_ok=True)
    con = sqlite3.connect(path)
    if spec.get("legacy"):
        # The table as the first release made it: none of the later
        # columns, and the two a later release drops.
        con.executescript(
            "CREATE TABLE pathways (id TEXT PRIMARY KEY, task_description TEXT NOT NULL, "
            "task_embedding_json TEXT NOT NULL, envelope_json TEXT NOT NULL, plan_template_json TEXT NOT NULL, "
            "source_trace_ids_json TEXT NOT NULL, pitfalls_json TEXT NOT NULL DEFAULT '[]', "
            "validation_score REAL NOT NULL DEFAULT 0.0, version INTEGER NOT NULL DEFAULT 1, "
            "hit_count INTEGER NOT NULL DEFAULT 0, distilled_at REAL NOT NULL);"
        )
    else:
        con.executescript(_SCHEMA)
    legacy_cols = {"id", "task_description", "task_embedding_json", "envelope_json", "plan_template_json",
                   "source_trace_ids_json", "version", "hit_count", "distilled_at"}
    for row in spec.get("rows", []):
        if spec.get("legacy"):
            row = {k: v for k, v in row.items() if k in legacy_cols}
        cols = list(row)
        vals = [unsparse(row[c]) if c == "task_embedding_json" else fill_value(row[c], home, t0) for c in cols]
        con.execute(f"INSERT INTO pathways ({', '.join(cols)}) VALUES ({', '.join('?' * len(cols))})", vals)
    con.commit()
    con.close()


def lay_out_journal(data_dir: Path, spec: dict[str, Any], home: str, t0: float) -> None:
    """A journal as Journal writes it: each trace through Journal.log with a
    fixed verification result, then its run columns and refinements."""
    from opendaisugi.journal import Journal
    from opendaisugi.models import ActionPlan, Envelope, VerificationResult
    from opendaisugi.refinement import RefinementRecord

    j = Journal(data_dir=data_dir)
    try:
        for t in spec.get("traces", []):
            env = Envelope.model_validate(t["envelope"])
            pl = ActionPlan.model_validate(t["plan"])
            res = VerificationResult(ok=t.get("ok", True), violations=t.get("violations", []), duration_ms=1.5,
                                     envelope_id=env.id, plan_id=pl.id)
            tid = j.log(
                task=fill_text(t["task"], home, t0),
                envelope=env,
                plan=pl,
                result=res,
                trace_id=t["id"],
                created_at=fill_text(t["created_at"], home, t0),
            )
            if "run_status" in t or "run_id" in t:
                j._con.execute(
                    "UPDATE traces SET run_id = ?, run_status = ? WHERE id = ?",
                    (t.get("run_id"), t.get("run_status"), tid),
                )
            if "signature" in t:
                j._con.execute("UPDATE traces SET structure_signature = ? WHERE id = ?", (t["signature"], tid))
        for r in spec.get("refinements", []):
            rec = RefinementRecord.model_validate(r["record"])
            j._con.execute(
                "INSERT INTO refinement_log (session_id, record_json, inserted_at, cache_key) VALUES (?, ?, ?, ?)",
                (r["session"], rec.model_dump_json(), t0 + r.get("at", 0.0), rec.cache_key),
            )
        for sid, tid in spec.get("converted", []):
            j.mark_session_converted(sid, tid, converted_at=t0 - 3600)
        if spec.get("downgrade"):
            # A journal as an early release left it: user_version 1, none of
            # the columns and tables the later migrations add.
            j._con.executescript(
                "DROP INDEX IF EXISTS idx_traces_structure;"
                "DROP INDEX IF EXISTS idx_refinement_cache_key;"
                "ALTER TABLE traces DROP COLUMN structure_signature;"
                "ALTER TABLE traces DROP COLUMN run_id;"
                "ALTER TABLE traces DROP COLUMN run_status;"
                "ALTER TABLE traces DROP COLUMN failed_step_id;"
                "ALTER TABLE traces DROP COLUMN total_duration_ms;"
                "ALTER TABLE refinement_log DROP COLUMN cache_key;"
                "DROP TABLE receipts; DROP TABLE hook_conversions; DROP TABLE provenance_log;"
                "PRAGMA user_version = 1;"
            )
        for path in spec.get("remove_yaml", []):
            (data_dir / "journal" / "traces" / f"{path}.yaml").unlink()
        for name, text in spec.get("yaml_text", {}).items():
            (data_dir / "journal" / "traces" / f"{name}.yaml").write_text(text)
    finally:
        j.close()


def lay_out(tree: dict[str, Any], home: Path, t0: float) -> None:
    h = str(home)
    home.mkdir(parents=True)
    os.chmod(home, 0o755)
    for rel, spec in sorted(tree.items()):
        p = home / rel
        if "dir" in spec:
            p.mkdir(parents=True, exist_ok=True)
        elif "db" in spec:
            lay_out_pathways(p, spec["db"], h, t0)
        elif "journal" in spec:
            lay_out_journal(p, spec["journal"], h, t0)
        elif "hex" in spec:
            p.parent.mkdir(parents=True, exist_ok=True)
            p.write_bytes(bytes.fromhex(spec["hex"]))
        else:
            p.parent.mkdir(parents=True, exist_ok=True)
            p.write_bytes(fill_text(spec["text"], h, t0).encode("utf-8"))
        if "mode" in spec:
            os.chmod(p, spec["mode"])


# ---------------------------------------------------------------------------
# Reading a tree back
# ---------------------------------------------------------------------------


def _cell(v: Any) -> Any:
    if isinstance(v, float):
        return {"float": repr(v)}
    if isinstance(v, bytes):
        return {"hex": v.hex()}
    return v


def dump_db(path: Path) -> dict[str, Any]:
    con = sqlite3.connect(f"file:{path}?mode=ro", uri=True)
    try:
        schema = [r[0] for r in con.execute("SELECT sql FROM sqlite_master ORDER BY name")]
        names = [r[0] for r in con.execute(
            "SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name")]
        tables = {}
        for name in names:
            cur = con.execute(f'SELECT * FROM "{name}" ORDER BY rowid')
            cols = [d[0] for d in cur.description]
            rows = []
            for r in cur.fetchall():
                row = {}
                for c, v in zip(cols, r, strict=True):
                    if c == "task_embedding_json" and isinstance(v, str):
                        from pathway_cases import sparse

                        v = sparse(v)
                    else:
                        v = _cell(v)
                    row[c] = v
                rows.append(row)
            tables[name] = rows
        seq = [list(r) for r in con.execute("SELECT name, seq FROM sqlite_sequence ORDER BY name")] \
            if "sqlite_sequence" in [r[0] for r in con.execute("SELECT name FROM sqlite_master")] else []
        uv = con.execute("PRAGMA user_version").fetchone()[0]
    finally:
        con.close()
    return {"schema": schema, "tables": tables, "user_version": uv, "sequence": seq}


def read_tree(home: Path) -> dict[str, Any]:
    out: dict[str, Any] = {}
    if not home.exists():
        return out
    for dirpath, dirnames, filenames in os.walk(home):
        dirnames[:] = [d for d in dirnames if not (Path(dirpath) == home and d == ".cache")]
        for name in sorted(dirnames) + sorted(filenames):
            p = Path(dirpath) / name
            rel = str(p.relative_to(home))
            st = os.lstat(p)
            if stat.S_ISDIR(st.st_mode):
                out[rel] = {"dir": True, "mode": stat.S_IMODE(st.st_mode)}
                continue
            entry: dict[str, Any] = {"mode": stat.S_IMODE(st.st_mode)}
            raw = p.read_bytes()
            if raw.startswith(b"SQLite format 3\x00"):
                entry["db"] = dump_db(p)
            else:
                try:
                    entry["text"] = raw.decode("utf-8")
                except UnicodeDecodeError:
                    entry["hex"] = raw.hex()
            out[rel] = entry
    return out


# ---------------------------------------------------------------------------
# Normalization
# ---------------------------------------------------------------------------

_TIME = re.compile(r"(?<![0-9.])1[0-9]{9}(?:\.[0-9]+)?(?![0-9])")
_ISOTIME = re.compile(r"\b(20[0-9]{2}-[0-9]{2}-[0-9]{2})T([0-9]{2}:[0-9]{2}:[0-9]{2})Z")
_DUR = re.compile(r"\bin [0-9]+\.[0-9]s\b")
_ELAPSED = re.compile(r'(elapsed_s\\?"?: )[0-9.]+')
_SKIPPED_AGO = re.compile(r"last run [0-9]+s ago")
_TOOK = re.compile(r"time taken=[0-9.]+ seconds")
_DURATION_YAML = re.compile(r"(duration_ms: )[0-9.e+-]+")
_DURATION_CELL = re.compile(r'("duration_ms": \{"float": ")[^"]*(")')
_MINTED = [
    (re.compile(r"\bpathway_[0-9a-f]{8}\b"), "pathway"),
    (re.compile(r"\benv_[0-9a-f]{8}\b"), "env"),
    (re.compile(r"\bplan_[0-9a-f]{8}\b"), "plan"),
    (re.compile(r"\b20[0-9]{2}-[0-9]{2}-[0-9]{2}-[0-9a-f]{8}\b"), "trace"),
    (re.compile(r"\bchatcmpl-[0-9a-f-]{36}\b"), "chatcmpl"),
]
_TMP = re.compile(r"opendaisugi-claude-[A-Za-z0-9_]+")


def _offset(v: float, t0: float) -> str | None:
    off = v - t0
    if -400 * DAY <= off <= 5 * DAY:
        return f"{round(off / 60) * 60:+d}"
    return None


def normalize(result: dict[str, Any], home: str, t0: float) -> dict[str, Any]:
    """The run's output with {HOME}, times near t0 and minted ids made
    stable. Applied to the canonical JSON text of the whole result, so a
    value is renamed the same way wherever it appears."""
    text = json.dumps(result, sort_keys=False, ensure_ascii=True)
    text = text.replace(json.dumps(home)[1:-1], "{HOME}")
    text = _TMP.sub("opendaisugi-claude-{TMP}", text)

    def time_sub(m: re.Match[str]) -> str:
        o = _offset(float(m.group(0)), t0)
        return m.group(0) if o is None else "{NOW" + o + "}"

    def iso_sub(m: re.Match[str]) -> str:
        v = datetime.strptime(m.group(1) + m.group(2), "%Y-%m-%d%H:%M:%S").replace(tzinfo=timezone.utc).timestamp()
        o = _offset(v, t0)
        return m.group(0) if o is None else "{ISO" + o + "}"

    text = _TIME.sub(time_sub, text)
    text = _ISOTIME.sub(iso_sub, text)
    text = _DUR.sub("in {DUR}s", text)
    text = _ELAPSED.sub(r"\1{ELAPSED}", text)
    text = _SKIPPED_AGO.sub("last run {AGO}s ago", text)
    text = _TOOK.sub("time taken={TOOK} seconds", text)
    # How long a verify() took: a timing, not a result.
    text = _DURATION_YAML.sub(r"\1{MS}", text)
    text = _DURATION_CELL.sub(r"\1{MS}\2", text)
    for pat, kind in _MINTED:
        seen: dict[str, str] = {}

        def mint(m: re.Match[str], seen: dict[str, str] = seen, kind: str = kind) -> str:
            if m.group(0) not in seen:
                seen[m.group(0)] = "{" + kind.upper() + str(len(seen) + 1) + "}"
            return seen[m.group(0)]

        text = pat.sub(mint, text)
    return json.loads(text)


def norm_stderr(s: str) -> list[str]:
    """A traceback keeps only its last line; everything else its text."""
    lines = s.splitlines()
    if "Traceback (most recent call last):" in lines:
        start = lines.index("Traceback (most recent call last):")
        exc = next((ln for ln in lines[start + 1 :] if ln and not ln.startswith(" ")), "")
        return lines[:start] + ["Traceback (most recent call last): ...", exc.split(":")[0]]
    return lines


# ---------------------------------------------------------------------------
# The fake model
# ---------------------------------------------------------------------------

FAKE_CLAUDE = r'''#!{PYTHON}
import hashlib, json, os, sys, time
argv = sys.argv[1:]
data = sys.stdin.buffer.read()
key = hashlib.sha256(json.dumps(argv).encode() + b"\0" + data).hexdigest()
log = os.environ["FAKE_MODEL_LOG"]
with open(log, "a", encoding="utf-8") as f:
    f.write(json.dumps({"kind": "claude", "argv": argv, "stdin": data.decode("utf-8", "surrogateescape"), "key": key}) + "\n")
table = json.load(open(os.environ["FAKE_MODEL_TABLE"], encoding="utf-8"))
ans = table.get(key)
if ans is None:
    sys.stderr.write("fake claude: no recorded answer for " + key + "\n")
    sys.exit(97)
if ans.get("sleep"):
    time.sleep(ans["sleep"])
sys.stdout.write(ans.get("stdout", ""))
sys.stderr.write(ans.get("stderr", ""))
sys.exit(ans.get("exit", 0))
'''


class FakeServer:
    """A fake Anthropic Messages API: the answer for a request body's
    SHA-256, from the case's table."""

    def __init__(self, table: dict[str, Any], log: list[dict[str, Any]]):
        self.table = table
        self.log = log
        # The credentials the case gives the command; run_case sets them.
        self.key: str | None = None
        self.token: str | None = None
        outer = self

        class H(http.server.BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def do_POST(self):  # noqa: N802 - http.server's name
                n = int(self.headers.get("content-length", 0))
                body = self.rfile.read(n)
                key = hashlib.sha256(body).hexdigest()
                headers, key_ok = outer.redact(self.headers)
                entry = {"kind": "http", "path": self.path, "key": key, "body": body.decode("utf-8"),
                         "headers": headers}
                if not key_ok:
                    entry["key_ok"] = False
                outer.log.append(entry)
                ans = outer.table.get(key)
                if not key_ok:
                    status, out = 596, b'{"error": "the credential is not the case\'s"}'
                elif ans is None:
                    status, out = 597, b'{"error": "no recorded answer"}'
                else:
                    if ans.get("sleep"):
                        time.sleep(ans["sleep"])
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

    def redact(self, headers) -> tuple[dict[str, str], bool]:
        """The headers the fixture keeps, each credential replaced by a
        name, and whether every credential sent is the case's own. The
        credential values never reach a fixture."""
        out: dict[str, str] = {}
        ok = True
        for k, v in headers.items():
            name = k.lower()
            if name == "x-api-key":
                good = self.key is not None and v == self.key
                out[name] = "{KEY}" if good else "{WRONG KEY}"
                ok &= good
            elif name == "authorization":
                good = self.token is not None and v == "Bearer " + self.token
                out[name] = "Bearer {TOKEN}" if good else "{WRONG AUTHORIZATION}"
                ok &= good
            elif name in ("anthropic-version", "content-type"):
                out[name] = v
        return out, ok

    def __enter__(self):
        self.thread.start()
        return self

    def __exit__(self, *a):
        self.httpd.shutdown()
        self.httpd.server_close()


def claude_key(argv: list[str], stdin: str) -> str:
    return hashlib.sha256(json.dumps(argv).encode() + b"\0" + stdin.encode("utf-8")).hexdigest()


# ---------------------------------------------------------------------------
# Running a case
# ---------------------------------------------------------------------------


def base_env(home: Path, work: Path) -> dict[str, str]:
    return {
        "HOME": str(home),
        "PATH": f"{work / 'bin'}:/usr/bin:/bin",
        "PYTHONPATH": str(REPO / "src"),
        "CUDA_VISIBLE_DEVICES": "",
        "LANG": "C.UTF-8",
        "NO_COLOR": "1",
        "COLUMNS": "100",
        "HF_HUB_OFFLINE": "1",
        "XDG_CACHE_HOME": str(home / ".cache"),
        "TMPDIR": str(work / "tmp"),
    }


def run_case(case: dict[str, Any], cmd: list[str], work: Path, raw: list | None = None) -> dict[str, Any]:
    """Run one case with cmd (the oracle's or the binary's). Returns the
    normalized result and the model requests it made; raw, when given,
    gets the requests as they were made."""
    if work.exists():
        shutil.rmtree(work)
    home = work / "home"
    (work / "bin").mkdir(parents=True)
    (work / "tmp").mkdir()
    t0 = time.time()
    lay_out(case.get("before") or {}, home, t0)
    h = str(home)
    table = case.get("model") or {}
    (work / "table.json").write_text(json.dumps(table), encoding="utf-8")
    if not case.get("no_claude"):
        fake = work / "bin" / "claude"
        fake.write_text(FAKE_CLAUDE.replace("{PYTHON}", sys.executable), encoding="utf-8")
        fake.chmod(0o755)
    log_path = work / "model.log"
    log_path.touch()
    env = base_env(home, work)
    env["FAKE_MODEL_LOG"] = str(log_path)
    env["FAKE_MODEL_TABLE"] = str(work / "table.json")
    http_log: list[dict[str, Any]] = []
    with FakeServer(table, http_log) as srv:
        env["ANTHROPIC_API_BASE"] = f"http://127.0.0.1:{srv.port}"
        env.update({k: v.replace("{HOME}", h).replace("{PORT}", str(srv.port))
                    for k, v in (case.get("env") or {}).items()})
        for k in case.get("unset") or []:
            env.pop(k, None)
        srv.key, srv.token = env.get("ANTHROPIC_API_KEY"), env.get("ANTHROPIC_AUTH_TOKEN")
        cwd = home / case.get("cwd", "")
        cwd.mkdir(parents=True, exist_ok=True)
        argv = [a.replace("{HOME}", h) for a in case["argv"]]
        old = os.umask(0o022)
        try:
            proc = subprocess.run(cmd + argv, capture_output=True, env=env, cwd=cwd,
                                  timeout=case.get("timeout", 300), check=False)
        finally:
            os.umask(old)
    requests = [json.loads(ln) for ln in log_path.read_text(encoding="utf-8").splitlines() if ln.strip()]
    requests += http_log
    if raw is not None:
        raw.extend(requests)
    result = {
        "exit": proc.returncode,
        "stdout": proc.stdout.decode("utf-8", "replace"),
        "stderr": norm_stderr(proc.stderr.decode("utf-8", "replace")),
        "tree": read_tree(home),
        "requests": [{k: v for k, v in r.items() if k != "headers" or r["kind"] == "http"} for r in requests],
    }
    return normalize(result, h, t0)


def claude_envelope(text: str, *, is_error: bool = False) -> str:
    """stdout of `claude -p --output-format json`, in the shape the real CLI
    writes (as sprig recorded it)."""
    return json.dumps({
        "type": "result", "subtype": "error_during_execution" if is_error else "success",
        "is_error": is_error, "duration_ms": 1200, "duration_api_ms": 1100, "num_turns": 1,
        "result": text, "session_id": "00000000-0000-4000-8000-000000000000", "total_cost_usd": 0.0012,
        "usage": {"input_tokens": 12, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0,
                  "output_tokens": 7},
    })


def api_message(text: str, *, stop: str = "end_turn") -> str:
    return json.dumps({
        "id": "msg_01", "type": "message", "role": "assistant", "model": "claude-sonnet-4-20250514",
        "content": [{"type": "text", "text": text}], "stop_reason": stop, "stop_sequence": None,
        "usage": {"input_tokens": 12, "output_tokens": 7},
    })


def make_answer(spec: dict[str, Any]) -> tuple[str, dict[str, Any]]:
    """(the request kind it answers, the fake's answer) for a reply spec."""
    if "claude" in spec:
        return "claude", {"stdout": claude_envelope(spec["claude"])}
    if "claude_is_error" in spec:
        return "claude", {"stdout": claude_envelope(spec["claude_is_error"], is_error=True)}
    if "claude_raw" in spec:
        return "claude", {k: spec[k] for k in ("stdout", "stderr", "exit", "sleep") if k in spec} | {
            "stdout": spec["claude_raw"]}
    if "http" in spec:
        return "http", {"body": api_message(spec["http"], stop=spec.get("stop", "end_turn"))}
    if "http_status" in spec:
        return "http", {"status": spec["http_status"], "body": spec["body"]}
    if "http_sleep" in spec:
        return "http", {"sleep": spec["http_sleep"], "body": api_message("{}")}
    raise ValueError(spec)


def record_replies(case: dict[str, Any], work: Path) -> None:
    """Answer the case's requests in order with its reply specs: run the
    oracle, answer the first request with no answer yet, and run again,
    until every request has an answer or the specs run out."""
    table: dict[str, Any] = {}
    replies = list(case["replies"])
    while True:
        case["model"] = table
        raw: list[dict[str, Any]] = []
        run_case(case, PY_CLI, work, raw)
        missing = [r for r in raw if r["key"] not in table]
        if not missing or not replies:
            break
        kind, ans = make_answer(replies.pop(0))
        if kind != missing[0]["kind"]:
            raise SystemExit(f"{case['name']}: a {kind} reply for a {missing[0]['kind']} request")
        table[missing[0]["key"]] = ans
    if replies:
        print(f"WARNING {case['name']}: {len(replies)} reply spec(s) never asked for", flush=True)
    case["model"] = table


# ---------------------------------------------------------------------------
# Garden cases
# ---------------------------------------------------------------------------


def build_garden_cases() -> list[dict[str, Any]]:
    C: list[dict[str, Any]] = []

    def add(name: str, argv: list[str], *, before=None, **kw):
        c = {"name": name, "argv": argv, "before": before or {}}
        c.update(kw)
        C.append(c)

    def db(*rows):
        return {DB: {"db": {"rows": list(rows)}}}

    def row(i, emb=(1.0, 0.0, 0.0), *, env=None, **kw):
        """A row with its times given relative to now."""
        times = {k: kw.pop(k) for k in ("distilled_at", "last_activation_at") if k in kw}
        times.setdefault("distilled_at", -1 * DAY - i * 60)
        r = row_spec(put_row(pathway(i, f"task {i}", list(emb), env=env, **kw)))
        for k, v in times.items():
            r[k] = {"now": v}
        return r

    fresh = -1 * DAY
    old = -40 * DAY
    rows = [
        row(1, hit_count=2, failure_count=1, distilled_at=old),                       # grace
        row(2, hit_count=2, failure_count=3, distilled_at=fresh),                     # failure 0.60
        row(3, hit_count=5, failure_count=5, distilled_at=fresh, last_activation_at=fresh),  # 0.50 kept
        row(4, hit_count=9, failure_count=0, distilled_at=old, last_activation_at=old),     # stale
        row(5, hit_count=9, failure_count=0, distilled_at=old),                        # stale via distilled_at
        row(6, hit_count=9, failure_count=1, distilled_at=old, last_activation_at=-2 * DAY),  # kept
        row(7, hit_count=0, failure_count=5, distilled_at=fresh),                     # failure 1.00
        row(8, hit_count=4, failure_count=1, distilled_at=-29.9 * DAY),               # kept, not stale
        row(9, hit_count=6, failure_count=0, distilled_at=-31 * DAY, last_activation_at=0.0),  # stale 31d
        row(10, hit_count=3, failure_count=2, distilled_at=-300.5 * DAY, last_activation_at=-100.2 * DAY),
    ]
    add("status no store", ["gardener", "status"])
    add("status empty", ["gardener", "status"], before=db())
    add("status rows", ["gardener", "status"], before=db(*rows))
    add("status rows json", ["gardener", "status", "--json"], before=db(*rows))
    add("status data dir", ["gardener", "status", "--data-dir", "d", "--json"],
        before={"d/pathways.db": db(*rows[:3])[DB]})
    add("status data dir is file", ["gardener", "status", "--data-dir", "f"], before={"f": {"text": "x"}})
    add("status invalid row", ["gardener", "status"],
        before=db(dict(rows[0], envelope_json='{"generated_by": "x"}')))
    add("status float hits", ["gardener", "status", "--json"], before=db(dict(rows[0], hit_count=2.5)))
    add("status whole float hits", ["gardener", "status", "--json"], before=db(dict(rows[0], hit_count=4.0)))
    add("status big counts", ["gardener", "status"], before=db(dict(rows[0], hit_count=2**62, failure_count=2**62)))
    add("status bad option", ["gardener", "status", "--nope"])
    add("status extra arg", ["gardener", "status", "x"])

    add("prune no store", ["gardener", "prune"])
    add("prune defaults", ["gardener", "prune"], before=db(*rows))
    add("prune defaults json", ["gardener", "prune", "--json"], before=db(*rows))
    add("prune dry run", ["gardener", "prune", "--dry-run"], before=db(*rows))
    add("prune dry run json", ["gardener", "prune", "--dry-run", "--json"], before=db(*rows))
    add("prune tight", ["gardener", "prune", "--max-idle-days", "1.5", "--max-failure-ratio", "0.1",
                        "--min-activations", "0"], before=db(*rows))
    add("prune loose", ["gardener", "prune", "--max-idle-days=1000", "--max-failure-ratio=1",
                        "--min-activations=100"], before=db(*rows))
    add("prune negative ratio", ["gardener", "prune", "--max-failure-ratio", "-1", "--json"], before=db(*rows))
    add("prune nan idle", ["gardener", "prune", "--max-idle-days", "nan", "--json"], before=db(*rows))
    add("prune inf idle", ["gardener", "prune", "--max-idle-days", "inf"], before=db(*rows))
    add("prune neg inf idle", ["gardener", "prune", "--max-idle-days", "-inf", "--min-activations", "-3"],
        before=db(*rows))
    add("prune underscore float", ["gardener", "prune", "--max-idle-days", " 3_0 "], before=db(*rows))
    add("prune bad float", ["gardener", "prune", "--max-idle-days", "abc"], before=db(*rows))
    add("prune bad int", ["gardener", "prune", "--min-activations", "1.5"], before=db(*rows))
    add("prune missing value", ["gardener", "prune", "--min-activations"], before=db(*rows))
    add("prune zero rows", ["gardener", "prune", "--json"], before=db())
    add("prune invalid row", ["gardener", "prune"],
        before=db(rows[1], dict(rows[3], plan_template_json="{nope")))
    add("prune big counts", ["gardener", "prune", "--json"],
        before=db(dict(rows[1], hit_count=2**63 - 1, failure_count=2**63 - 1)))
    add("prune future row", ["gardener", "prune", "--max-idle-days", "0", "--json"],
        before=db(row(11, hit_count=9, distilled_at=2 * DAY), row(12, hit_count=9, distilled_at=-1)))

    # merge
    a = [1.0, 0.0, 0.0]
    near = [0.97, 0.2431, 0.0]   # cos ~0.9700 with a
    mid = [0.9, 0.4359, 0.0]     # cos ~0.90 with a
    other = [0.0, 1.0, 0.0]
    m_rows = [
        row(1, a, hit_count=3),
        row(2, near, hit_count=5),
        row(3, other, hit_count=1),
        row(4, mid, hit_count=1),
        row(5, a, hit_count=5, distilled_at=-5 * DAY),
    ]
    add("merge no store", ["gardener", "merge"])
    add("merge empty", ["gardener", "merge", "--json"], before=db())
    add("merge basic", ["gardener", "merge"], before=db(*m_rows))
    add("merge basic json", ["gardener", "merge", "--json"], before=db(*m_rows))
    add("merge dry run", ["gardener", "merge", "--dry-run"], before=db(*m_rows))
    add("merge dry run json", ["gardener", "merge", "--dry-run", "--json"], before=db(*m_rows))
    add("merge low threshold", ["gardener", "merge", "--similarity", "0.5", "--json"], before=db(*m_rows))
    add("merge threshold 0.85", ["gardener", "merge", "--similarity=0.85"], before=db(*m_rows))
    add("merge threshold 1", ["gardener", "merge", "--similarity", "1"], before=db(*m_rows))
    add("merge threshold -1", ["gardener", "merge", "--similarity", "-1", "--json"], before=db(*m_rows))
    add("merge threshold nan", ["gardener", "merge", "--similarity", "nan", "--json"], before=db(*m_rows))
    add("merge bad float", ["gardener", "merge", "--similarity", "x"], before=db(*m_rows))
    # winner rules
    tie_rows = [row(1, a, hit_count=2, distilled_at=-3 * DAY), row(2, a, hit_count=2, distilled_at=-2 * DAY),
                row(3, a, hit_count=2, distilled_at=-2 * DAY)]
    add("merge tie newer wins", ["gardener", "merge", "--json"], before=db(*tie_rows))
    add("merge tie equal times", ["gardener", "merge", "--json"], before=db(tie_rows[2], tie_rows[1]))
    add("merge sources union", ["gardener", "merge"],
        before=db(row(1, a, hit_count=4, failure_count=1, source_trace_ids=["t9", "t1", "t1"]),
                  row(2, a, hit_count=1, failure_count=2, source_trace_ids=["t0", "t9", "é", "Z"])))
    # compatibility
    add("merge incompatible allowlist", ["gardener", "merge", "--json"],
        before=db(row(1, a), row(2, a, env=envelope(2, shell_allowlist=["make"]))))
    add("merge compatible sorted lists", ["gardener", "merge", "--json"],
        before=db(row(1, a, env=envelope(1, file_read=["/a/**", "/b/**"])),
                  row(2, a, env=envelope(2, file_read=["/a/**", "/b/**"]))))
    add("merge unsorted list is incompatible", ["gardener", "merge", "--json"],
        before=db(row(1, a, env=envelope(1, file_read=["/b/**", "/a/**"])),
                  row(2, a, env=envelope(2, file_read=["/b/**", "/a/**"]))))
    add("merge duplicate list is incompatible", ["gardener", "merge", "--json"],
        before=db(row(1, a, env=envelope(1, shell_allowlist=["make", "make"])),
                  row(2, a, env=envelope(2, shell_allowlist=["make", "make"]))))
    add("merge mcp is incompatible", ["gardener", "merge", "--json"],
        before=db(row(1, a, env=envelope(1, mcp_allowlist=["fs/read"])),
                  row(2, a, env=envelope(2, mcp_allowlist=["fs/read"]))))
    add("merge ceilings", ["gardener", "merge", "--json"],
        before=db(row(1, a, env=envelope(1, max_execution_time_s=60)),
                  row(2, a, env=envelope(2, max_execution_time_s=60)),
                  row(3, a, env=envelope(3, max_execution_time_s=30))))
    add("merge decomposition flag", ["gardener", "merge", "--json"],
        before=db(row(1, a, env=envelope(1, shell_allow_decomposition=True)),
                  row(2, a, env=envelope(2, shell_allow_decomposition=True)),
                  row(3, a, env=envelope(3, shell_allow_decomposition=False))))
    add("merge robotics is incompatible", ["gardener", "merge", "--json"],
        before=db(row(1, a, env=envelope(1, velocity_limit=0.5)), row(2, a, env=envelope(2, velocity_limit=0.5))))
    # identity and width
    add("merge other model skipped", ["gardener", "merge", "--json"],
        before=db(row(1, a), row(2, a, model="all-MiniLM-L6-v2"), row(3, a, emb_version="2")))
    add("merge width mismatch skipped", ["gardener", "merge", "--json"],
        before=db(row(1, a), row(2, [1.0, 0.0])))
    add("merge zero vectors", ["gardener", "merge", "--json"],
        before=db(row(1, [0.0, 0.0, 0.0]), row(2, [0.0, 0.0, 0.0])))
    add("merge chain", ["gardener", "merge", "--json"],
        before=db(row(1, a, hit_count=1), row(2, a, hit_count=3), row(3, a, hit_count=2), row(4, a, hit_count=9)))
    add("merge chain text", ["gardener", "merge"],
        before=db(row(1, a, hit_count=1), row(2, a, hit_count=3), row(3, a, hit_count=2), row(4, a, hit_count=9)))
    add("merge sum past 64 bits", ["gardener", "merge"],
        before=db(row(1, a, hit_count=2**62), row(2, a, hit_count=2**62)))
    add("merge invalid row", ["gardener", "merge"], before=db(row(1, a), dict(row(2, a), task_embedding_json={"text": "[1,"})))
    add("merge unicode ids", ["gardener", "merge"],
        before=db(row(1, a, id="pw_é", hit_count=2), row(2, a, id="pw ✓", hit_count=1)))

    # run
    run_rows = rows + [row(20, a, hit_count=9, distilled_at=fresh), row(21, a, hit_count=7, distilled_at=fresh)]
    add("run no store", ["gardener", "run"])
    add("run rows", ["gardener", "run"], before=db(*run_rows))
    add("run rows json", ["gardener", "run", "--json"], before=db(*run_rows))
    add("run dry run", ["gardener", "run", "--dry-run"], before=db(*run_rows))
    add("run dry run json", ["gardener", "run", "--dry-run", "--json"], before=db(*run_rows))
    add("run data dir", ["gardener", "run", "--data-dir", "{HOME}/dd"], before={"dd/pathways.db": db(*run_rows)[DB]})

    # watch
    stamp = ".opendaisugi/.gardener-last-run"
    add("watch first", ["gardener", "watch"], before=db(*run_rows))
    add("watch recent stamp", ["gardener", "watch"], before=dict(db(*run_rows), **{stamp: {"text": "{NOWF:-100}"}}))
    add("watch old stamp", ["gardener", "watch"], before=dict(db(*run_rows), **{stamp: {"text": "{NOWF:-7200}"}}))
    add("watch force", ["gardener", "watch", "--force"],
        before=dict(db(*run_rows), **{stamp: {"text": "{NOWF:-100}"}}))
    add("watch dry run", ["gardener", "watch", "--dry-run"], before=db(*run_rows))
    add("watch min interval", ["gardener", "watch", "--min-interval", "60"],
        before=dict(db(*run_rows), **{stamp: {"text": "{NOWF:-100}"}}))
    add("watch bad stamp", ["gardener", "watch"], before=dict(db(*run_rows), **{stamp: {"text": "yesterday\n"}}))
    add("watch spaced stamp", ["gardener", "watch"], before=dict(db(*run_rows), **{stamp: {"text": "  {NOWF:-50}\n"}}))
    add("watch no store", ["gardener", "watch", "--data-dir", "{HOME}/fresh"])
    add("watch bad interval", ["gardener", "watch", "--min-interval", "1.5"])
    add("watch invalid row", ["gardener", "watch"], before=db(dict(rows[0], envelope_json="{")))

    add("gardener no such command", ["gardener", "nope"])
    return C


# ---------------------------------------------------------------------------
# Writing
# ---------------------------------------------------------------------------


def body_id(case: dict[str, Any]) -> str:
    """A case's id: its body, without what the oracle recorded."""
    return case_id({k: v for k, v in case.items() if k not in ("id", "expect", "model")})


def write_jsonl(path: Path, items: list[dict[str, Any]]) -> dict[str, Any]:
    for it in items:
        it.pop("id", None)
        it["id"] = body_id(it)
    items.sort(key=lambda c: c["id"])
    data = "".join(canonical_json(c) + "\n" for c in items).encode("utf-8")
    path.write_bytes(data)
    return {"count": len(items), "sha256": hashlib.sha256(data).hexdigest()}


# ---------------------------------------------------------------------------
# Tend cases
# ---------------------------------------------------------------------------

JDIR = ".opendaisugi"
CONFIG = ".opendaisugi/config.yaml"


def t_env(i: int, **perm: Any) -> dict[str, Any]:
    base: dict[str, Any] = {"shell": True, "shell_allowlist": ["make", "pytest"], "file_read": ["/work/**"]}
    base.update(perm)
    return {"id": f"env_{i:08x}", "generated_by": "test", "task": f"task {i}", "permissions": base}


def t_plan(i: int, steps: list[dict[str, Any]]) -> dict[str, Any]:
    return {"id": f"plan_{i:08x}", "source": "script", "task": f"task {i}", "steps": steps}


def sh(sid: str, cmd: str, deps=()) -> dict[str, Any]:
    return {"id": sid, "type": "shell", "command": cmd, "depends_on": list(deps)}


def rd(sid: str, path: str, deps=()) -> dict[str, Any]:
    return {"id": sid, "type": "file_read", "path": path, "depends_on": list(deps)}


def trace(i: int, task: str, steps, *, hours: float, env=None, **kw) -> dict[str, Any]:
    t = {"id": f"t{i:02d}", "task": task, "envelope": env or t_env(i), "plan": t_plan(i, steps),
         "created_at": "{ISO:%d}" % int(-hours * 3600)}
    t.update(kw)
    return t


def template(task: str, steps, **extra) -> str:
    return json.dumps({"task_description": task,
                       "plan_template": {"id": "plan_tmpl0001", "source": "distilled", "task": task, "steps": steps,
                                         **extra}})


def build_tend_cases() -> list[dict[str, Any]]:
    C: list[dict[str, Any]] = []
    lexical = {CONFIG: {"text": "matcher_model: lexical\n"}}

    def add(name: str, argv: list[str], *, traces=None, refinements=None, extra=None, config=None, **kw):
        before: dict[str, Any] = dict(config if config is not None else lexical)
        if traces is not None:
            before[JDIR] = {"journal": {"traces": traces, "refinements": refinements or [],
                                        **(kw.pop("journal_extra", {}))}}
        before.update(extra or {})
        c = {"name": name, "argv": argv, "before": before}
        c.update(kw)
        C.append(c)

    build = [sh("s1", "make test"), rd("s2", "/work/out.txt", ["s1"])]
    same = [trace(k, f"build the release {k}", build, hours=k + 1) for k in range(1, 4)]
    tmpl_ok = template("build the release", [sh("a", "make test"), rd("b", "/work/out.txt", ["a"])])
    claude = {"env": {"OPENDAISUGI_LLM_BACKEND": "claude-code"}}
    api = {"env": {"ANTHROPIC_API_KEY": "sk-test-000000000000000000000000"}}

    add("tend no journal", ["tend"])
    add("tend no journal dry run", ["tend", "--dry-run"])
    add("tend below min traces", ["tend"], traces=same[:2])
    add("tend min traces flag", ["tend", "--min-traces", "2"], traces=same[:2], replies=[{"claude": tmpl_ok}], **claude)
    add("tend claude creates", ["tend"], traces=same, replies=[{"claude": tmpl_ok}], **claude)
    add("tend claude dry run", ["tend", "--dry-run"], traces=same, replies=[{"claude": tmpl_ok}], **claude)
    add("tend litellm creates", ["tend"], traces=same, replies=[{"http": tmpl_ok}], **api)
    add("tend litellm other model", ["tend", "--model", "anthropic/claude-haiku-4-5"], traces=same,
        replies=[{"http": tmpl_ok}], **api)
    add("tend auto picks claude", ["tend"], traces=same, replies=[{"claude": tmpl_ok}])
    add("tend config picks claude", ["tend"], traces=same, replies=[{"claude": tmpl_ok}],
        config={CONFIG: {"text": "matcher_model: lexical\nllm_backend: claude-code\n"}}, **api)
    # typed holes: the read path varies under one directory
    holes = [trace(k, f"build the release {k}", [sh("s1", "make test"), rd("s2", f"/work/out{k}.txt", ["s1"])],
                   hours=k + 1) for k in range(1, 5)]
    add("tend typed holes", ["tend"], traces=holes, replies=[{"claude": tmpl_ok}], **claude)
    # divergent shell commands: salvaged, no model
    div = [trace(k, f"build the release {k}", [sh("s1", cmd), rd("s2", "/work/out.txt", ["s1"])], hours=k + 1)
           for k, cmd in enumerate(["make test", "make lint", "pytest -q", "make docs", "make a", "make b", "make c"],
                                   start=1)]
    # The envelope's glob is absolute: the leaf's workspace is its prefix.
    add("tend salvage absolute globs", ["tend"], traces=div[:4], **claude)
    lit = [dict(t, envelope=t_env(k, file_read=["/work/out.txt"])) for k, t in enumerate(div, start=1)]
    add("tend salvage no workspace", ["tend"], traces=lit[:4], replies=[{"claude": tmpl_ok}], **claude)
    wide = {"file_read": ["**"]}
    divw = [dict(t, envelope=t_env(k, **wide)) for k, t in enumerate(div, start=1)]
    add("tend salvage", ["tend"], traces=divw[:4], **claude)
    add("tend salvage many variants", ["tend"], traces=divw, **claude)
    rdiv = [trace(k, f"build the release {k}", [sh("s1", "make test"), rd("s2", p, ["s1"])], hours=k + 1)
            for k, p in enumerate(["/work/a/x", "/work/b/x", "/etc/y"], start=1)]
    add("tend salvage path dirs", ["tend"], traces=[dict(t, envelope=t_env(k, **wide)) for k, t in
                                                    enumerate(rdiv, start=1)], **claude)
    # model failures, claude-code
    add("tend claude bad json", ["tend"], traces=same,
        replies=[{"claude": "not json"}, {"claude": "still {not json}"}, {"claude": "{'a': }"}], **claude)
    add("tend claude schema invalid then good", ["tend"], traces=same,
        replies=[{"claude": json.dumps({"task_description": 5})}, {"claude": tmpl_ok}], **claude)
    # GD-R-6: coerce_step runs over every item before the StepBase check,
    # so the invalid "shell" step (no command) is the error, not the
    # "nope" item that comes before it and is no step at all.
    add("tend claude reply names invalid step first", ["tend"], traces=same, replies=[
        {"claude": json.dumps({"task_description": "build it", "plan_template": {
            "source": "s", "task": "t", "steps": [{"type": "nope", "id": "x"}, {"type": "shell", "id": "a"}]}})},
        {"claude": tmpl_ok}], **claude)
    add("tend claude python literal", ["tend"], traces=same, replies=[{"claude": "Sure: {'task_description': "
        "'build it', 'plan_template': {'source': 's', 'task': 't', 'steps': [{'id': 'a', 'type': 'shell', "
        "'command': 'make test', 'depends_on': []}]}} done"}], **claude)
    add("tend claude string steps", ["tend"], traces=same, replies=[{"claude": json.dumps({
        "task_description": "build it", "plan_template": {"source": "s", "task": "t", "steps": [
            "{'type': 'shell', 'id': 'a', 'command': 'make test'}", json.dumps(rd("b", "/work/o", ["a"]))]}})}],
        **claude)
    add("tend claude is_error", ["tend"], traces=same, replies=[{"claude_is_error": "Credit balance is too low"}],
        **claude)
    add("tend claude exit 1", ["tend"], traces=same,
        replies=[{"claude_raw": "", "stderr": "boom é\n" + "x" * 600, "exit": 1}], **claude)
    add("tend claude not the envelope", ["tend"], traces=same, replies=[{"claude_raw": tmpl_ok}], **claude)
    add("tend claude extra args", ["tend"], traces=same, replies=[{"claude": tmpl_ok}],
        env={"OPENDAISUGI_LLM_BACKEND": "claude-code", "DAISUGI_CLAUDE_ARGS": "--max-turns 2 --append-system-prompt 'be brief'"})
    add("tend claude bad extra args", ["tend"], traces=same, replies=[{"claude": tmpl_ok}],
        env={"OPENDAISUGI_LLM_BACKEND": "claude-code", "DAISUGI_CLAUDE_ARGS": "--x 'unclosed"})
    add("tend claude missing", ["tend"], traces=same, no_claude=True, env={"OPENDAISUGI_LLM_BACKEND": "claude-code"})
    add("tend no backend at all", ["tend"], traces=same, no_claude=True)
    add("tend claude timeout", ["tend"], traces=same, replies=[{"claude_raw": "", "sleep": 125}],
        timeout=600, slow=True, **claude)
    # model failures, litellm
    add("tend litellm bad json then good", ["tend"], traces=same, replies=[{"http": "no"}, {"http": tmpl_ok}], **api)
    add("tend litellm bad json thrice", ["tend"], traces=same,
        replies=[{"http": "no"}, {"http": "{\"task_description\": 1}"}, {"http": ""}], **api)
    for st in (500, 401, 429, 503, 400):
        add(f"tend litellm http {st}", ["tend"], traces=same, replies=[
            {"http_status": st, "body": json.dumps({"type": "error", "error": {"type": "api_error", "message": "boom"}})}],
            **api)
    add("tend litellm http 500 after bad json", ["tend"], traces=same, replies=[
        {"http": "{}"}, {"http_status": 500, "body": "{\"type\":\"error\"}"}], **api)
    add("tend litellm timeout", ["tend"], traces=same, replies=[{"http_sleep": 3}],
        env={"ANTHROPIC_API_KEY": "sk-test-000000000000000000000000", "REQUEST_TIMEOUT": "1"})
    add("tend litellm cut", ["tend"], traces=same, replies=[{"http": tmpl_ok[:40], "stop": "max_tokens"}], **api)
    add("tend litellm string steps", ["tend"], traces=same, replies=[{"http": json.dumps({
        "task_description": "build it", "plan_template": {"source": "s", "task": "t", "steps": [
            json.dumps(sh("a", "make test"))]}})}], **api)
    add("tend litellm base url", ["tend"], traces=same, replies=[{"http": tmpl_ok}],
        env={"ANTHROPIC_API_KEY": "sk-test-000000000000000000000000", "ANTHROPIC_BASE_URL": "http://127.0.0.1:{PORT}"},
        unset=["ANTHROPIC_API_BASE"])
    add("tend litellm no key", ["tend"], traces=same, no_claude=True, env={"OPENDAISUGI_LLM_BACKEND": "litellm"})
    add("tend litellm model not anthropic", ["tend", "--model", "gpt-4o"], traces=same, go_refuses=True, **api)
    # templates that do not verify, and the improvement pass
    add("tend template does not verify", ["tend"], traces=same, replies=[{"claude": template(
        "build", [sh("a", "rm -rf /")])}], **claude)
    add("tend template with a cycle", ["tend"], traces=same, replies=[{"claude": template(
        "build", [sh("a", "make test", ["b"]), sh("b", "make test", ["a"])])}], **claude)
    narrow = [trace(k, f"build the release {k}", build, hours=k + 1,
                    env=t_env(k, shell_allowlist=["pytest"]) if k == 1 else None) for k in range(1, 4)]
    wide_env = json.dumps(dict(t_env(90, shell_allowlist=["make", "pytest"]), generated_by="improved"))
    add("tend improvement adopted", ["tend"], traces=narrow,
        replies=[{"claude": tmpl_ok}, {"claude": wide_env}], **claude)
    add("tend improvement no better", ["tend"], traces=narrow,
        replies=[{"claude": tmpl_ok}, {"claude": json.dumps(t_env(91, shell_allowlist=["pytest"]))}], **claude)
    add("tend improvement fails", ["tend"], traces=narrow,
        replies=[{"claude": tmpl_ok}, {"claude": "no"}, {"claude": "no"}], **claude)
    add("tend improvement litellm", ["tend"], traces=narrow,
        replies=[{"http": tmpl_ok}, {"http": wide_env}], **api)
    # An int past 64 bits is a valid int to pydantic; verify finds the
    # envelope inconsistent (max time over 3600) and the run carries on.
    big_env = json.dumps(dict(t_env(92, shell_allowlist=["make", "pytest"]), generated_by="improved"))
    big_env = big_env.replace('"shell": true', '"shell": true, "max_execution_time_s": 100000000000000000000')
    add("tend improvement huge max time", ["tend"], traces=narrow,
        replies=[{"claude": tmpl_ok}, {"claude": big_env}], **claude)
    add("tend template huge int in metadata", ["tend"], traces=same, replies=[{"claude": template(
        "build the release", [dict(sh("a", "make test"), metadata={"n": 10**400}),
                              rd("b", "/work/out.txt", ["a"])])}], **claude)
    # GD-16: NaN in a reply is schema-invalid, so every attempt is refused
    # and the improvement fails. The fake answers a repeated prompt (the
    # second re-ask) as it answered it first, so two replies cover three.
    nan_env = big_env.replace("100000000000000000000", "30").replace(
        '"shell": true', '"shell": true, "velocity_limit": NaN')
    add("tend improvement nan velocity", ["tend"], traces=narrow,
        replies=[{"claude": tmpl_ok}] + [{"claude": nan_env}] * 2, **claude)
    add("tend litellm auth token", ["tend"], traces=same, replies=[{"http": tmpl_ok}],
        env={"ANTHROPIC_AUTH_TOKEN": "tok-test-00000000000000000000", "OPENDAISUGI_LLM_BACKEND": "litellm"})
    # existing pathways
    def existing(srcs, **kw):
        return {".opendaisugi/pathways.db": {"db": {"rows": [row_spec(put_row(pathway(
            1, "build the release", [1.0, 0.0], source_trace_ids=srcs, **kw)))]}}}
    add("tend covered no new traces", ["tend"], traces=same, extra=existing(["t01", "t02", "t03"]), **claude)
    add("tend covered new trace updates", ["tend"], traces=same, extra=existing(["t01", "t02"]),
        replies=[{"claude": tmpl_ok}], **claude)
    add("tend reembeds stale rows", ["tend"], traces=same[:1], extra=existing(["zz"]), **claude)
    add("tend reembeds legacy rows", ["tend"], traces=same[:1], extra=existing(["zz"], model="", emb_version=""),
        **claude)
    def legacy(srcs):
        spec = existing(srcs)
        spec[".opendaisugi/pathways.db"]["db"]["legacy"] = True
        return spec
    add("tend old store", ["tend"], traces=same[:1], extra=legacy(["zz"]), **claude)
    add("tend old store minilm", ["tend"], traces=same[:1], extra=legacy(["zz"]), config={}, go_refuses=True)
    add("tend invalid store row", ["tend"], traces=same, extra={".opendaisugi/pathways.db": {"db": {"rows": [dict(
        row_spec(put_row(pathway(1, "x", [1.0], source_trace_ids=["q"]))), envelope_json="{}")]}}}, **claude)
    # the journal's content
    add("tend missing yaml", ["tend"], traces=same + [trace(4, "build the release 4", build, hours=5)],
        journal_extra={"remove_yaml": ["t04"]}, replies=[{"claude": tmpl_ok}], **claude)
    add("tend invalid yaml envelope", ["tend"], traces=same, journal_extra={"yaml_text": {
        "t01": "id: t01\ncreated_at: x\ntask: t\nenvelope:\n  task: t\nplan: {}\nresult: {}\n"}},
        replies=[{"claude": tmpl_ok}], **claude)
    add("tend yaml not safe_dump form", ["tend"], traces=same, journal_extra={"yaml_text": {
        "t01": "{id: t01}\n"}}, go_refuses=True, **claude)
    runs = [trace(k, f"build the release {k}", build, hours=k + 1, run_id=f"run{k}", run_status="succeeded")
            for k in range(1, 4)]
    runs.append(trace(4, "build the release 4", build, hours=5, run_id="run4", run_status="failed"))
    runs.append(trace(5, "build the release 5", build, hours=6, ok=False))
    rec = {"step": sh("s1", "rm x"), "violations": [
        {"stage": "permissions", "message": "shell head 'rm' not in allowlist"},
        {"stage": "z3", "message": "é unicode ✓"}], "z3_counterexample": None, "envelope_id": "env_x",
        "fallback_action": "halted", "timestamp": 1.5}
    many = [dict(rec, violations=[{"stage": "s", "message": f"m{k}"} for k in range(25)])]
    add("tend pitfalls", ["tend"], traces=runs, refinements=[
        {"session": "run1", "record": rec}, {"session": "run2", "record": rec, "at": 1.0},
        {"session": "run3", "record": many[0], "at": 2.0}], replies=[{"claude": tmpl_ok}], **claude)
    # Same-second traces: the later insert lists first, so it is the
    # representative and its task names the pathway.
    ties = [trace(k, f"build the release {k}", build, hours=2) for k in (2, 1, 3)]
    add("tend same second", ["tend"], traces=ties, replies=[{"claude": tmpl_ok}], **claude)
    # An old journal: the binary reads it before it migrates it, and a
    # refusal leaves it as it was.
    add("tend old journal", ["tend"], traces=same, journal_extra={"downgrade": True},
        replies=[{"claude": tmpl_ok}], **claude)
    add("tend old journal minilm", ["tend"], traces=same, journal_extra={"downgrade": True}, config={},
        go_refuses=True)
    add("tend lookback", ["tend", "--lookback-days", "1"], traces=same + [
        trace(k, f"build the release {k}", build, hours=24 * 3 + k) for k in range(4, 7)], **claude)
    net = {"id": "n1", "type": "network", "url": "https://docs.example.com/deploy", "depends_on": []}
    two = same + [trace(k, f"deploy the docs site {k}", [net], hours=k + 10,
                        env=t_env(k, network=True, network_hosts=["docs.example.com"])) for k in range(4, 7)]
    add("tend two clusters", ["tend"], traces=two, replies=[{"claude": tmpl_ok}, {"claude": template(
        "deploy the docs", [dict(net, id="a")])}], **claude)
    add("tend unicode task", ["tend"], traces=[trace(k, f"Résumé café ✓ build {k}", build, hours=k + 1)
                                                 for k in range(1, 4)], replies=[{"claude": tmpl_ok}], **claude)
    add("tend skill preamble", ["tend"], traces=[trace(k, f"Base directory for this skill: /x\n### Skill: y\n"
                                                     f"<command-name>/b</command-name>build the release {k}", build,
                                                     hours=k + 1) for k in range(1, 4)],
        replies=[{"claude": tmpl_ok}], **claude)
    add("tend unknown matcher", ["tend"], traces=same, config={CONFIG: {"text": "matcher_model: bogus\n"}})
    add("tend minilm", ["tend"], traces=same, config={}, go_refuses=True)
    # Below min_traces with no stale row the oracle never loads the
    # embedder, so the binary runs too.
    add("tend minilm below min traces", ["tend"], traces=same[:2], config={})
    add("tend potion tiny", ["tend"], traces=same, replies=[{"claude": tmpl_ok}], potion=True,
        config={CONFIG: {"text": "matcher_model: potion\n"}},
        env={"OPENDAISUGI_LLM_BACKEND": "claude-code", "OPENDAISUGI_POTION_MODEL": "{HOME}/potion-tiny"},
        extra={"potion-tiny/" + f: {"hex": (PATHWAY_FIXTURES / "potion-tiny" / f).read_bytes().hex()}
               for f in ("config.json", "tokenizer.json", "model.safetensors")})
    add("tend bad min traces", ["tend", "--min-traces", "x"])
    add("tend tier1 invalid", ["tend"], extra={".opendaisugi/local_tier1.json": {"text": "{nope"}})
    add("tend data dir", ["tend", "--data-dir", "{HOME}/dd"], extra={"dd/journal/x": {"text": ""}})
    return C


def same_build_traces(ahead: bool = False) -> list[dict[str, Any]]:
    """Two traces of one build. ahead lays them out in the next hours, so
    the newest trace (the representative the model is shown) has fixed
    ids, not ones a run mints."""
    build = [sh("s1", "make test"), rd("s2", "/work/out.txt", ["s1"])]
    return [trace(k, f"build the release {k}", build, hours=-k if ahead else k + 1) for k in range(1, 3)]


def build_autotend_cases() -> list[dict[str, Any]]:
    C: list[dict[str, Any]] = []
    on = {CONFIG: {"text": "matcher_model: lexical\nauto_tend: true\n"}}
    claude = {"OPENDAISUGI_LLM_BACKEND": "claude-code"}

    def cap(*records: dict[str, Any], at: float = 1_700_000_000.0) -> dict[str, Any]:
        lines = []
        for k, r in enumerate(records):
            lines.append(json.dumps(dict({"captured_at": at + k, "tool": "Bash"}, **r)))
        return {"text": "\n".join(lines) + "\n"}

    def caps(**sessions):
        return {f".opendaisugi/captures/{sid}.jsonl": v for sid, v in sessions.items()}

    def add(name, argv, before, **kw):
        c = {"name": name, "argv": ["hook", "auto-tend"] + argv, "before": before}
        c.update(kw)
        C.append(c)

    make = {"step_type": "shell", "command": "make test"}
    read = {"step_type": "file_read", "path": "/work/src/a.py"}
    add("auto-tend consent off", [], {CONFIG: {"text": "matcher_model: lexical\n"}}, env=claude)
    add("auto-tend consent off force", ["--force"], {CONFIG: {"text": "matcher_model: lexical\n"}}, env=claude)
    add("auto-tend no captures", [], dict(on), env=claude)
    add("auto-tend recent stamp", [], dict(on, **{".opendaisugi/.hook-auto-tend-last-run": {"text": "{NOWF:-60}"}}),
        env=claude)
    add("auto-tend recent stamp force", ["--force", "--skip-distill"],
        dict(on, **{".opendaisugi/.hook-auto-tend-last-run": {"text": "{NOWF:-60}"}}, **caps(a=cap(make))), env=claude)
    add("auto-tend old stamp", ["--skip-distill"],
        dict(on, **{".opendaisugi/.hook-auto-tend-last-run": {"text": "{NOWF:-7200}"}}, **caps(a=cap(make))),
        env=claude)
    add("auto-tend min interval", ["--min-interval", "30", "--skip-distill"],
        dict(on, **{".opendaisugi/.hook-auto-tend-last-run": {"text": "{NOWF:-60}"}}, **caps(a=cap(make))),
        env=claude)
    add("auto-tend converts", ["--skip-distill"], dict(on, **caps(
        s1=cap(make, read, at=1_700_000_100.0),
        s2=cap({"step_type": "file_write", "path": "/work/out/r.txt"}, {"step_type": "network",
               "url": "https://API.Example.com/v1?q=1"}, at=1_700_000_300.0),
        s3=cap({"step_type": "mcp", "mcp_server": "fs", "mcp_tool": "read", "arguments": {"p": "/x"}},
               {"step_type": "task"}, at=1_700_000_200.0),
        empty={"text": "\n\n"},
        bad={"text": "{not json\n" + json.dumps(dict(make, captured_at=5)) + "\n"},
    )), env=claude)
    # A captured MCP call whose arguments hold NaN or an infinity makes an
    # invalid plan (models.non_finite_error): that session is not converted.
    add("auto-tend nan mcp argument", ["--skip-distill"], dict(on, **caps(
        n=cap({"step_type": "mcp", "mcp_server": "fs", "mcp_tool": "read", "arguments": {"p": float("nan")}}),
        ok=cap(make, at=1_700_000_100.0),
    )), env=claude)
    add("auto-tend relative paths", ["--skip-distill"], dict(on, **caps(
        r=cap({"step_type": "file_read", "path": "a.txt"}, {"step_type": "file_read", "path": "sub//dir/./b.txt"},
              {"step_type": "file_write", "path": "/"}, {"step_type": "file_read", "path": ""},
              {"step_type": "shell", "command": "  FOO=1 pytest -q  "}, {"step_type": "shell", "command": "# note"}),
    )), env=claude)
    add("auto-tend decomposition", ["--skip-distill", "--allow-shell-decomposition"], dict(on, **caps(
        d=cap({"step_type": "shell", "command": "cd /repo && make test > /repo/out.log"},
              {"step_type": "shell", "command": "timeout 30 git fetch"}),
    )), env=claude)
    add("auto-tend decomposition from config", ["--skip-distill"], {
        CONFIG: {"text": "matcher_model: lexical\nauto_tend: true\nshell_allow_decomposition: true\n"},
        **caps(d=cap({"step_type": "shell", "command": "make a | tee /work/log"}))}, env=claude)
    # apply_patch is never admitted by name, so its trace carries a
    # violation, which the binary refuses to write (GD-8).
    add("auto-tend apply_patch violation", ["--skip-distill"], dict(on, **caps(
        p=cap({"step_type": "mcp", "mcp_server": "opencode", "mcp_tool": "apply_patch"}))), env=claude,
        go_refuses=True)
    add("auto-tend compound without decomposition", ["--skip-distill"], dict(on, **caps(
        d=cap({"step_type": "shell", "command": "make a && make b"}))), env=claude, go_refuses=True)
    add("auto-tend already converted", ["--skip-distill"], dict(on, **caps(a=cap(make), b=cap(make, at=1_700_000_500.0)),
        **{".opendaisugi": {"journal": {"traces": [], "converted": [["a", "t-old"]]}}}), env=claude)
    add("auto-tend then tend below min", [], dict(on, **caps(a=cap(make, read), b=cap(make, read, at=1_700_000_500.0))),
        env=claude)
    # Laid out an hour ahead, so the newest trace (the representative the
    # model is shown) is one with fixed ids, not the one this run mints.
    old = [trace(k, f"captured session old{k}", [sh("s0", "make test"), rd("s1", "/work/src/a.py", ["s0"])],
                 hours=-k, env=t_env(k, shell_allowlist=["make"], file_read=["/work/src/**"]))
           for k in range(1, 3)]
    add("auto-tend then tend distills", [], dict(on, **caps(new=cap(make, read)),
        **{".opendaisugi": {"journal": {"traces": old}}}), env=claude,
        replies=[{"claude": template("captured session", [sh("a", "make test"), rd("b", "/work/src/a.py", ["a"])])}])
    add("auto-tend default matcher one session", [], {CONFIG: {"text": "auto_tend: true\n"}, **caps(a=cap(make))},
        env=claude)
    add("auto-tend default matcher three sessions", [], {CONFIG: {"text": "auto_tend: true\n"}, **caps(
        a=cap(make), b=cap(make, at=1_700_000_500.0), c=cap(make, at=1_700_000_900.0))}, env=claude, go_refuses=True)
    add("auto-tend then tend unreadable trace", [], dict(on, **caps(a=cap(make)), **{".opendaisugi": {"journal": {
        "traces": same_build_traces(ahead=True), "yaml_text": {"t01": "{id: t01}\n"}}}}), env=claude,
        go_refuses=True)
    add("auto-tend then tend fails", [], {CONFIG: {"text": "matcher_model: bogus\nauto_tend: true\n"},
                                          **caps(a=cap(make))}, env=claude)
    add("auto-tend captures root", ["--captures-root", "{HOME}/caps", "--skip-distill"],
        dict(on, **{"caps/z.jsonl": cap(make)}), env=claude)
    add("auto-tend data dir", ["--data-dir", "{HOME}/dd", "--force", "--skip-distill"], dict(on, **caps(a=cap(make))),
        env=claude)
    # Commands the binary does not carry: one line, exit 2, nothing written.
    for argv in (["registry", "pull-and-tend", "--repo-path", "{HOME}/reg"], ["hook", "list"]):
        C.append({"name": "not in binary " + " ".join(argv[:2]), "argv": argv, "before": dict(on),
                  "go_refuses": True, "env": claude})
    return C


def build_repeats_cases() -> list[dict[str, Any]]:
    from pathway_cases import lexical

    C: list[dict[str, Any]] = []
    lex = {CONFIG: {"text": "matcher_model: lexical\n"}}
    turns_path = ".opendaisugi/gateway/turns.jsonl"

    def turn(task: str, *, sig: str | None = None, tokens=(100, 10, 0, 0), dollars=0.01, **extra) -> str:
        rec = {"created_at": "2020-01-01T00:00:00Z", "signature": hashlib.sha256(task.encode()).hexdigest()[:16]
               if sig is None else sig, "task": task, "tier": "t2", "requested_model": "m", "model": "m",
               "difficulty": 0.5, "downgraded": False, "estimated": False, "input_tokens": tokens[0],
               "output_tokens": tokens[1], "frontier_tokens_saved": 0, "actual_dollars": dollars,
               "counterfactual_dollars": dollars, "cache_read_tokens": tokens[2], "cache_creation_tokens": tokens[3]}
        rec.update(extra)
        return json.dumps(rec)

    def add(name, argv, lines=None, extra=None, **kw):
        before = dict(lex)
        if lines is not None:
            before[turns_path] = {"text": "".join(ln + "\n" for ln in lines)}
        before.update(extra or {})
        c = {"name": name, "argv": ["distill-repeats"] + argv, "before": before}
        c.update(kw)
        C.append(c)

    base = [turn("summarize the auth module"), turn("summarize the auth module", tokens=(50, 5, 1000, 20)),
            turn("run the unit tests"), turn("run the unit tests"), turn("run the unit tests please"),
            turn("deploy the docs site", dollars=1.5), turn("deploy the docs site", dollars=2.25),
            turn("one-off question about cats"), turn("", sig=""), turn("continuation", sig="")]
    add("repeats no journal", [])
    add("repeats all unsigned", [], [turn("a", sig=""), turn("b", sig="")])
    add("repeats none repeated", [], [turn("alpha beta"), turn("gamma delta")])
    add("repeats ranked", [], base)
    add("repeats top 1", ["--top", "1"], base)
    add("repeats top negative", ["--top", "-1"], base)
    add("repeats top zero", ["--top", "0"], base)
    add("repeats bad lines", [], base[:4] + ["{nope", "[1, 2]", "\"text\"", json.dumps({"task": "x"})])
    add("repeats long unicode task", [], [turn("Résumé  café\t✓ " * 12), turn("Résumé  café\t✓ " * 12)])
    add("repeats ties", [], [turn("aa bb", tokens=(5, 5, 0, 0), dollars=1.0), turn("aa bb", tokens=(5, 5, 0, 0)),
                             turn("cc dd", tokens=(5, 5, 0, 0), dollars=3.0), turn("cc dd", tokens=(5, 5, 0, 0))])
    add("repeats big numbers", [], [turn("x y", tokens=(10**12, 10**12, 7, 1), dollars=1234567.891),
                                    turn("x y", tokens=(3, True, 0, 0), dollars=0)])
    add("repeats skill preamble", [], [turn("Base directory for this skill: /s\nbuild the release"),
                                       turn("### Skill: q\nbuild the release")])
    reusable = {".opendaisugi/pathways.db": {"db": {"rows": [row_spec(put_row(pathway(
        1, "run the unit tests", lexical("run the unit tests"))))]}}}
    add("repeats reusable", [], base, extra=reusable)
    add("repeats data dir", ["--data-dir", "{HOME}/dd"], extra={"dd/gateway/turns.jsonl": {"text": "\n".join(base)}})
    add("repeats float tokens", [], [turn("x y", tokens=(1.5, 0, 0, 0)), turn("x y")], go_refuses=True)
    add("repeats unknown matcher", [], base, extra={CONFIG: {"text": "matcher_model: bogus\n"}})
    add("repeats bad top", ["--top", "x"], base)
    return C


def all_cases() -> list[dict[str, Any]]:
    cases = build_garden_cases() + build_tend_cases() + build_autotend_cases() + build_repeats_cases()
    names = [c["name"] for c in cases]
    dup = {n for n in names if names.count(n) > 1}
    if dup:
        raise SystemExit(f"duplicate case names: {sorted(dup)}")
    return cases


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--out", type=Path, default=FIXTURE_DIR)
    ap.add_argument("--only", help="run only cases whose name holds this text; print, write nothing")
    ap.add_argument("--skip-slow", action="store_true", help="leave out the cases that wait out a timeout")
    ap.add_argument("--fresh", action="store_true", help="rerun every case, not only new or changed ones")
    args = ap.parse_args()
    out: Path = args.out
    out.mkdir(parents=True, exist_ok=True)
    SCRATCH.mkdir(parents=True, exist_ok=True)
    cases = [c for c in all_cases() if not (args.skip_slow and c.get("slow"))]
    old: dict[str, dict[str, Any]] = {}
    if (out / "cases.jsonl").exists() and not args.fresh:
        for ln in (out / "cases.jsonl").read_text(encoding="utf-8").splitlines():
            if ln.strip():
                c = json.loads(ln)
                old[c["id"]] = c
    cache_path = SCRATCH / "gen-cache.jsonl"
    if cache_path.exists() and not args.fresh:
        for ln in cache_path.read_text(encoding="utf-8").splitlines():
            if ln.strip():
                c = json.loads(ln)
                old.setdefault(c["id"], c)
    for i, c in enumerate(cases):
        if args.only and args.only not in c["name"]:
            continue
        prev = old.get(body_id(c))
        if prev is not None and not args.only:
            c["expect"] = prev["expect"]
            if "model" in prev:
                c["model"] = prev["model"]
            continue
        if c.get("replies"):
            record_replies(c, SCRATCH / "gen" / f"{i:04d}")
        c["expect"] = run_case(c, PY_CLI, SCRATCH / "gen" / f"{i:04d}")
        if any(r.get("key_ok") is False for r in c["expect"]["requests"]):
            raise SystemExit(f"{c['name']}: the oracle sent a credential that is not the case's")
        if not args.only:
            with cache_path.open("a", encoding="utf-8") as fh:
                fh.write(json.dumps({"id": body_id(c), "expect": c["expect"], **({"model": c["model"]}
                                                                                 if "model" in c else {})}) + "\n")
        if args.only:
            print(json.dumps({"name": c["name"], **c["expect"]}, indent=1, ensure_ascii=False)[:8000])
        else:
            print(f"{i + 1:4d}/{len(cases)} exit={c['expect']['exit']} {c['name']}", flush=True)
    if args.only:
        return 0
    manifest = {"v": CASE_VERSION}
    manifest["cases.jsonl"] = write_jsonl(out / "cases.jsonl", cases)
    (out / "manifest.json").write_text(json.dumps(manifest, indent=1, sort_keys=True) + "\n", encoding="utf-8")
    shutil.rmtree(SCRATCH / "gen", ignore_errors=True)
    cache_path.unlink(missing_ok=True)
    from fixture_paths import leaks

    leaked = leaks()
    if leaked:
        raise SystemExit("a machine path reached the fixtures:\n" + "\n".join(leaked[:10]))
    print(f"wrote {len(cases)} cases to {out}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
