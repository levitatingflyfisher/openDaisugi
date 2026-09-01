"""Synthetic pathway cases: the pathway store, the matchers and the
`daisugi pathways` commands, run through the Python oracle.

    uv run --no-sync python clients/pathway_cases.py [--out clients/fixtures/pathways]

It writes, under clients/fixtures/pathways:

- potion-tiny/: a tiny model in model2vec's file format (config.json,
  tokenizer.json, model.safetensors): a small WordPiece vocabulary and an
  8-wide random table. Both sides load it; no test downloads a model.
- units.json: BLAKE2b digests, lexical tokens and vectors, and the tiny
  model's token ids and vectors, for a corpus of texts.
- find.jsonl: stores and queries. Each case is a set of rows, a
  matcher_model, the environment, and for each query the matched id and
  score (rounded to 6 places) and the stale-embedding warning.
- cases.jsonl: CLI cases in the shape of clients/cli_cases.py. A case is a
  file tree (a pathways.db is given as rows and laid out by Python's
  sqlite3), one `daisugi pathways ...` command, and what the oracle did:
  exit code, stdout, stderr and the tree after, each database dumped as
  its schema and rows.

Cases that need the real potion-base-8M model are written only when it is
already in the Hugging Face cache of this box (nothing is downloaded), and
are marked real_potion. Cases whose answer the oracle cannot give (the
MiniLM and int8 matchers, which the Go binary does not carry) are marked
go_only with the Go binary's expected refusal.

Every path in a case is fake: the scratch HOME is written {HOME}. The
cases hold no content from any real session and may be committed. Lines
are content-addressed and sorted, and a manifest pins each file's bytes.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import random
import re
import shutil
import sqlite3
import stat
import subprocess
import sys
from pathlib import Path
from typing import Any

REPO = Path(__file__).resolve().parent.parent
FIXTURE_DIR = REPO / "clients" / "fixtures" / "pathways"
SCRATCH = Path(
    os.environ.get("DAISUGI_PATHWAY_SCRATCH") or Path.home() / "opendaisugi-scratch" / "d1" / "runs"
)
sys.path.insert(0, str(REPO / "src"))
CASE_VERSION = 1
REAL_POTION = "minishlab/potion-base-8M"


def canonical_json(obj: Any) -> str:
    return json.dumps(obj, sort_keys=True, separators=(",", ":"), ensure_ascii=True)


def case_id(body: dict[str, Any]) -> str:
    return hashlib.sha256(canonical_json(body).encode("utf-8")).hexdigest()[:16]


def hf_cache() -> Path:
    return Path(
        os.environ.get("HF_HUB_CACHE")
        or Path(os.environ.get("HF_HOME") or Path.home() / ".cache" / "huggingface") / "hub"
    )


def real_potion_dir() -> Path | None:
    """The cached snapshot of the real model, or None. Never downloads."""
    snaps = hf_cache() / "models--minishlab--potion-base-8M" / "snapshots"
    if not snaps.is_dir():
        return None
    for d in sorted(snaps.iterdir()):
        if all((d / f).exists() for f in ("config.json", "tokenizer.json", "model.safetensors")):
            return d
    return None


# ---------------------------------------------------------------------------
# The tiny potion model
# ---------------------------------------------------------------------------

TINY_WORDS = (
    "build test deploy release lint format docs backup sync fetch parse render migrate index "
    "search clean install check report list show stats delete export import run make add fix "
    "update the a an and or of to in on for with by from at as is are be this that it its into "
    "via using use file files write read network shell step plan task pathway envelope verify "
    "data model table query server client cache log logs error errors config setup start stop "
    "python go rust node npm pip cargo git commit push pull branch merge rebase tag main master "
    "readme license notice version changelog database db sql json yaml toml csv html css "
    "cafe naive resume uber zoe noel garcon"
).split()
TINY_PIECES = "##s ##ing ##ed ##er ##es ##ly ##tion ##al ##a ##e ##i ##o ##u ##n ##t ##r".split()
TINY_CJK = "中 文 日 本 語 学 生 한 국".split()
TINY_OTHER = "σ ς ω α β γ δ λ π ß ø æ œ ı".split()


def build_tiny_model(out: Path) -> None:
    """Write the tiny model. Deterministic: the same bytes every run."""
    import numpy as np
    from safetensors.numpy import save_file
    from tokenizers import Tokenizer, models, normalizers, pre_tokenizers

    specials = ["[PAD]", "[UNK]", "[CLS]", "[SEP]", "[MASK]"]
    vocab: dict[str, int] = {}

    def add(tok: str) -> None:
        if tok not in vocab:
            vocab[tok] = len(vocab)

    for t in specials:
        add(t)
    for c in range(0x21, 0x7F):
        ch = chr(c)
        if ch.isupper():
            continue
        add(ch)
    for c in "abcdefghijklmnopqrstuvwxyz0123456789":
        add("##" + c)
    for t in TINY_WORDS + TINY_PIECES + TINY_CJK + TINY_OTHER:
        add(t)
    tok = Tokenizer(models.WordPiece(vocab, unk_token="[UNK]", max_input_chars_per_word=100))
    tok.normalizer = normalizers.BertNormalizer(
        clean_text=True, handle_chinese_chars=True, strip_accents=None, lowercase=True
    )
    tok.pre_tokenizer = pre_tokenizers.BertPreTokenizer()
    tok.add_special_tokens(specials)
    out.mkdir(parents=True, exist_ok=True)
    tok.save(str(out / "tokenizer.json"))
    rng = np.random.default_rng(7)
    table = rng.standard_normal((len(vocab), 8)).astype(np.float32)
    save_file({"embeddings": table}, str(out / "model.safetensors"))
    (out / "config.json").write_text(
        json.dumps({"model_type": "model2vec", "hidden_dim": 8, "normalize": True}) + "\n"
    )


# ---------------------------------------------------------------------------
# Texts
# ---------------------------------------------------------------------------

FIXED_TEXTS = [
    "",
    "   ",
    "build the release",
    "Build The RELEASE",
    "run the tests and fix the lint errors",
    "deploy deploy deploy",
    "a an the and or",
    "migrate_db_v2 to postgres_15",
    "Résumé for the café naïve Zoë garçon",
    "ÉCOLE Ü ß ẞ ﬁle",
    "İstanbul DİYARBAKIR ı",
    "ΣΟΦΟΣ σοφος ΟΔΥΣΣΕΥΣ",
    "中文日本語学生 한국어",
    "tab\tnew\nline\rcr\x0bvt\x0cff",
    "zero\u200bwidth\u200djoiner\ufeffbom",
    "nbsp\u00a0here\u2003em\u3000ideo",
    "ctrl\x00nul\x07bel\x1besc\x7fdel\x85nel",
    "emoji 🚀🔥 and 👩\u200d💻 ok",
    "fullwidth ＢＵＩＬＤ ｔｅｓｔ",
    "\u212a Kelvin Å Angstrom",
    "punct!!! (a) [b] {c} <d> 'e' \"f\" `g` ~h~ ^i^",
    "[CLS] literal [UNK] [SEP][MASK] tokens [PAD]",
    "x" * 101,
    "y" * 100,
    "supercalifragilisticexpialidocious antidisestablishmentarianism",
    "naïve",
    "ﬁ ﬂ ﬀ ﬃ",
    "ǅ ǈ ǋ ǲ",
    "combining e\u0301 a\u0300 o\u0308 n\u0303",
    "k\u0301\u0327 stacked\u0316\u0317 marks",
    "tab	separated	words",
    "path/to/file.py --flag=value && echo done || exit 1",
    "https://example.com/a?b=c#d",
    "12345 67.89 1e10 -0 +5",
    "a" * 3000,
    " ".join(["word"] * 700),
    "unicode ✓ ✗ → ← ⇒ ∀ ∃ ∈ ∉",
    "\U0001d400\U0001d401 math bold",
    "Hangul 가나다 힣",
    "การ thai",
    "العربية arabic",
    "surrogate-free 𝔘𝔫𝔦𝔠𝔬𝔡𝔢",
]

_WORDS = TINY_WORDS + ["deploying", "tests", "builder", "releases", "zzz", "qqq"]
_EXTRA = list("äöüéèêçñåøæœßıİΣσςЖжΩ中文한국🚀·—–…«»¿¡") + ["\u0301", "\u200b", "\t", "\n", "\x00", "\u00a0", "[CLS]", "[UNK]"]


def random_texts(n: int, seed: int) -> list[str]:
    rng = random.Random(seed)
    out = []
    for _ in range(n):
        parts = []
        for _ in range(rng.randint(0, 12)):
            r = rng.random()
            if r < 0.6:
                w = rng.choice(_WORDS)
                if rng.random() < 0.2:
                    w = w.upper()
                elif rng.random() < 0.2:
                    w = w.capitalize()
                parts.append(w)
            elif r < 0.8:
                parts.append(rng.choice(_EXTRA))
            elif r < 0.9:
                parts.append(rng.choice(".,;:!?-_/()[]{}'\"#$%&*+=<>@\\^`|~"))
            else:
                parts.append(str(rng.randint(0, 10**rng.randint(1, 8))))
        sep = rng.choice([" ", " ", " ", "", "  ", "-", "_"])
        out.append(sep.join(parts))
    return out


# ---------------------------------------------------------------------------
# Rows
# ---------------------------------------------------------------------------

_MEM_STORE = None


def put_row(p) -> dict[str, Any]:
    """The column values PathwayStore.put writes for a pathway."""
    from opendaisugi.pathway_store import PathwayStore

    global _MEM_STORE
    if _MEM_STORE is None:
        _MEM_STORE = PathwayStore(":memory:")
    with _MEM_STORE._connect() as con:
        con.execute("DELETE FROM pathways")
    _MEM_STORE.put(p)
    return _MEM_STORE._load_all_rows()[0]


def envelope(i: int, **perm):
    from opendaisugi.models import Envelope, Permission

    base = {"shell": True, "shell_allowlist": ["make", "pytest"], "file_read": ["/work/**"]}
    base.update(perm)
    return Envelope(id=f"env_{i:08x}", generated_by="test", task=f"task {i}", permissions=Permission(**base))


def plan(i: int, steps=None):
    from opendaisugi.models import ActionPlan, FileReadStep, ShellStep

    if steps is None:
        steps = [
            ShellStep(id="s1", command="make test"),
            FileReadStep(id="s2", path="/work/out.txt", depends_on=["s1"]),
        ]
    return ActionPlan(id=f"plan_{i:08x}", source="script", task=f"task {i}", steps=steps)


def pathway(i: int, task: str, emb, *, model="lexical-hash-v1", emb_version="3", env=None, pl=None, **kw):
    from opendaisugi.pathway import CompiledPathway

    return CompiledPathway(
        id=kw.pop("id", f"pw_{i:04d}"),
        task_description=task,
        task_embedding=list(emb),
        embedding_model=model,
        embedding_model_version=emb_version,
        envelope=env or envelope(i),
        plan_template=pl or plan(i),
        source_trace_ids=kw.pop("source_trace_ids", [f"t{i}a", f"t{i}b"]),
        distilled_at=kw.pop("distilled_at", 1_700_000_000.0 + i * 3600.5),
        **kw,
    )


def lexical(text: str) -> list[float]:
    from opendaisugi._search import _LexicalEmbedder

    return [float(x) for x in _LexicalEmbedder().encode([text])[0]]


def potion_vec(model_dir: Path | str, text: str) -> list[float]:
    from opendaisugi._search import _PotionEmbedder

    return [float(x) for x in _PotionEmbedder(str(model_dir)).encode([text])[0]]


def sparse(emb_json: str) -> Any:
    """A stored embedding text, kept small: a json.dumps list of floats
    is written as its length and nonzero entries; any other text as is."""
    try:
        v = json.loads(emb_json)
    except ValueError:
        return {"text": emb_json}
    if (
        isinstance(v, list)
        and all(type(x) is float for x in v)
        and json.dumps(v) == emb_json
        and len(v) > 16
    ):
        return {"n": len(v), "nz": [[i, x] for i, x in enumerate(v) if x != 0.0]}
    return {"text": emb_json}


def unsparse(spec: Any) -> str:
    if "text" in spec:
        return spec["text"]
    v = [0.0] * spec["n"]
    for i, x in spec["nz"]:
        v[i] = x
    return json.dumps(v)


def row_spec(row: dict[str, Any]) -> dict[str, Any]:
    out = dict(row)
    out["task_embedding_json"] = sparse(row["task_embedding_json"])
    return out


# ---------------------------------------------------------------------------
# Laying out a database and reading it back
# ---------------------------------------------------------------------------

LEGACY_SCHEMA = """
CREATE TABLE pathways (
    id TEXT PRIMARY KEY,
    task_description TEXT NOT NULL,
    task_embedding_json TEXT NOT NULL,
    envelope_json TEXT NOT NULL,
    plan_template_json TEXT NOT NULL,
    source_trace_ids_json TEXT NOT NULL,
    pitfalls_json TEXT NOT NULL DEFAULT '[]',
    validation_score REAL NOT NULL DEFAULT 0.0,
    version INTEGER NOT NULL DEFAULT 1,
    hit_count INTEGER NOT NULL DEFAULT 0,
    distilled_at REAL NOT NULL
);
"""


def lay_out_db(path: Path, spec: dict[str, Any], home: str = "") -> None:
    """Write a database with Python's sqlite3: the current schema (or the
    legacy one) and the rows given, column by column. {HOME} in a text
    value is the scratch HOME."""
    from opendaisugi.pathway_store import _SCHEMA

    path.parent.mkdir(parents=True, exist_ok=True)
    con = sqlite3.connect(path)
    con.executescript(LEGACY_SCHEMA if spec.get("schema") == "legacy" else _SCHEMA)
    for row in spec.get("rows", []):
        cols = list(row)
        vals = [unsparse(row[c]) if c == "task_embedding_json" else row[c] for c in cols]
        vals = [v.replace("{HOME}", home) if isinstance(v, str) and home else v for v in vals]
        con.execute(
            f"INSERT INTO pathways ({', '.join(cols)}) VALUES ({', '.join('?' * len(cols))})", vals
        )
    con.commit()
    con.close()


def dump_db(path: Path) -> dict[str, Any]:
    con = sqlite3.connect(f"file:{path}?mode=ro", uri=True)
    try:
        schema = [r[0] for r in con.execute("SELECT sql FROM sqlite_master ORDER BY name")]
        cur = con.execute("SELECT * FROM pathways ORDER BY rowid")
        cols = [d[0] for d in cur.description]
        rows = []
        for r in cur.fetchall():
            row = {}
            for c, v in zip(cols, r, strict=True):
                if c == "task_embedding_json" and isinstance(v, str):
                    v = sparse(v)
                elif isinstance(v, float):
                    v = {"float": repr(v)}
                row[c] = v
            rows.append(row)
    finally:
        con.close()
    return {"schema": schema, "rows": rows}


# ---------------------------------------------------------------------------
# CLI cases: trees, commands, normalization
# ---------------------------------------------------------------------------

_VERSION = re.compile(r"(opendaisugi_version[\"']?:? *\"?)([0-9A-Za-z.+_-]+)")
_ENV_ID = re.compile(r"\benv_[0-9a-f]{8}\b")
_PLAN_ID = re.compile(r"\bplan_[0-9a-f]{8}\b")


def norm_version(s: str) -> str:
    """opendaisugi_version is the build's own version; compare its shape."""
    return _VERSION.sub(lambda m: m.group(1) + "{VERSION}", s)


def norm_text(s: str, home: str) -> str:
    return norm_version(s.replace(home, "{HOME}"))


def lay_out(tree: dict[str, Any], home: Path) -> None:
    h = str(home)
    home.mkdir(parents=True)
    os.chmod(home, 0o755)
    for rel, spec in sorted(tree.items()):
        p = home / rel
        if "dir" in spec:
            p.mkdir(parents=True, exist_ok=True)
        elif "db" in spec:
            lay_out_db(p, spec["db"], h)
        elif "hex" in spec:
            p.parent.mkdir(parents=True, exist_ok=True)
            p.write_bytes(bytes.fromhex(spec["hex"]))
        else:
            p.parent.mkdir(parents=True, exist_ok=True)
            p.write_bytes(spec["text"].replace("{HOME}", h).encode("utf-8"))


def read_tree(home: Path) -> dict[str, Any]:
    h = str(home)
    out: dict[str, Any] = {}
    if not home.exists():
        return out
    for dirpath, dirnames, filenames in os.walk(home):
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
                    entry["text"] = norm_text(raw.decode("utf-8"), h)
                except UnicodeDecodeError:
                    entry["hex"] = raw.hex()
            out[rel] = entry
    return out


def norm_stderr(s: str, home: str) -> list[str]:
    """A traceback keeps only its last line; warnings keep their text."""
    lines = norm_text(s, home).splitlines()
    if "Traceback (most recent call last):" in lines:
        start = lines.index("Traceback (most recent call last):")
        exc = next((ln for ln in lines[start + 1 :] if ln and not ln.startswith(" ")), "")
        return lines[:start] + ["Traceback (most recent call last): ...", exc.split(":")[0]]
    return lines


def base_env(home: Path) -> dict[str, str]:
    return {
        "HOME": str(home),
        "PATH": "/usr/bin:/bin",
        "PYTHONPATH": str(REPO / "src"),
        "CUDA_VISIBLE_DEVICES": "",
        "LANG": "C.UTF-8",
        "NO_COLOR": "1",
        "COLUMNS": "100",
        "HF_HUB_OFFLINE": "1",
        "XDG_CACHE_HOME": str(home / ".cache"),
    }


def run_cli_case(case: dict[str, Any], cmd: list[str], work: Path) -> dict[str, Any]:
    if work.exists():
        shutil.rmtree(work)
    home = work / "home"
    lay_out(case.get("before") or {}, home)
    h = str(home)
    env = base_env(home)
    env.update({k: v.replace("{HOME}", h) for k, v in (case.get("env") or {}).items()})
    cwd = home / case.get("cwd", "")
    cwd.mkdir(parents=True, exist_ok=True)
    argv = [a.replace("{HOME}", h) for a in case["argv"]]
    old = os.umask(0o022)
    try:
        proc = subprocess.run(cmd + argv, capture_output=True, env=env, cwd=cwd, timeout=300, check=False)
    finally:
        os.umask(old)
    return {
        "exit": proc.returncode,
        "stdout": norm_text(proc.stdout.decode("utf-8", "replace"), h),
        "stderr": norm_stderr(proc.stderr.decode("utf-8", "replace"), h),
        "tree": read_tree(home),
    }


PY_CLI = [sys.executable, "-m", "opendaisugi.cli"]
DB = ".opendaisugi/pathways.db"


def build_cli_cases() -> list[dict[str, Any]]:
    from opendaisugi.models import (
        AgenticStep,
        CartesianMoveStep,
        FileWriteStep,
        GripperStep,
        Invariant,
        JointMoveStep,
        MCPStep,
        NetworkStep,
        Postcondition,
        ShellStep,
        SimulationResetStep,
        SkillStep,
        TaskStep,
        VLAStep,
    )
    from opendaisugi.pathway import PathwayParameter
    from opendaisugi.portability import export

    C: list[dict[str, Any]] = []

    def add(name: str, argv: list[str], *, before=None, go_refuses=False, **kw):
        c = {"name": name, "argv": argv, "before": before or {}}
        if go_refuses:
            c["go_refuses"] = True
        c.update(kw)
        C.append(c)

    def db(*rows, schema="current"):
        return {DB: {"db": {"schema": schema, "rows": [row_spec(r) for r in rows]}}}

    small = [0.6, 0.8, 0.0]
    p1 = pathway(1, "build the release", small)
    p2 = pathway(2, "run the tests", [1.0, 0.0, 0.0], hit_count=3, version=2)
    p3 = pathway(3, "Résumé für café — 中文 ✓", [0.0, 0.0, 1.0], hit_count=4)
    rich_steps = [
        ShellStep(id="s1", command="make test", metadata={"k": [1, 2.5, None, True], "é": "ü"}),
        FileWriteStep(id="s2", path="/work/o.txt", content="line1\nline2 \"q\"", depends_on=["s1"],
                      postcondition=Postcondition(type="file_exists", path="/work/o.txt")),
        NetworkStep(id="s3", url="https://example.com/x?y=1", headers={"A": "b"}),
        JointMoveStep(id="s4", joint_targets={"j1": 0.5, "j2": -1e-7}, duration_s=2.5e-05),
        CartesianMoveStep(id="s5", target_position=(1.0, 2.0, 1e16), target_orientation=(0, 0, 0, 1)),
        GripperStep(id="s6", action="close"),
        SimulationResetStep(id="s7", seed=42),
        VLAStep(id="s8", task="pick | place \"it\"", target_pose=(0.1, 0.2, 0.3)),
        TaskStep(id="s9", prompt="summarize the log", preferred_model="haiku"),
        AgenticStep(id="s10", prompt="fix it", workspace="/work", tools=["Read", "Bash"], max_turns=3),
        SkillStep(id="s11", skill_id="pdf-extract", skill_input={"n": 1}),
        MCPStep(id="s12", server="fs", tool="read", arguments={"path": "/work"}),
    ]
    rich_env = envelope(
        9, file_write=["/work/**"], network=True, network_hosts=["example.com"],
        mcp_allowlist=["fs/read"], velocity_limit=0.5, joint_limits={"j1": (-1.0, 1.0)},
    )
    rich_env.invariants = [Invariant(type="no_side_effects", description="nothing leaves /work")]
    rich_env.postconditions = [Postcondition(type="file_exists", path="/work/o.txt", expected=1)]
    rich = pathway(
        9, "a rich pathway ✓ with every step", [1e-7, 2.5e-05, 0.3, -0.0, 1e16], env=rich_env,
        pl=plan(9, rich_steps), hit_count=7, failure_count=1, last_activation_at=1_700_000_123.25,
        structure_signature="shell→file_write", parameters=[PathwayParameter(
            name="target", step_index=0, step_id="s1", field="command", head="make", observed=["make a", "make b"])],
    )
    lex = pathway(4, "deploy the docs site", lexical("deploy the docs site"))
    r1, r2, r3, rr, rl = (put_row(p) for p in (p1, p2, p3, rich, lex))

    # list
    add("list no store", ["pathways", "list"])
    add("list no store json", ["pathways", "list", "--json"])
    add("list empty db", ["pathways", "list"], before=db())
    add("list three", ["pathways", "list"], before=db(r1, r2, r3))
    add("list three json", ["pathways", "list", "--json"], before=db(r1, r2, r3))
    add("list lexical row", ["pathways", "list", "--json"], before=db(rl, r1))
    add("list rich", ["pathways", "list"], before=db(rr))
    add("list data dir", ["pathways", "list", "--data-dir", "data"], before={"data/pathways.db": db(r2)[DB]})
    add("list data dir eq", ["pathways", "list", "--data-dir={HOME}/d2", "--json"])
    add("list legacy schema", ["pathways", "list"], before=db(schema="legacy"))
    legacy_row = {k: r1[k] for k in ("id", "task_description", "task_embedding_json", "envelope_json",
                                     "plan_template_json", "source_trace_ids_json", "version", "hit_count",
                                     "distilled_at")}
    legacy_row["pitfalls_json"] = "[]"
    add("list legacy row", ["pathways", "list", "--json"], before=db(legacy_row, schema="legacy"))
    add("show legacy row", ["pathways", "show", "pw_0001"], before=db(legacy_row, schema="legacy"))
    add("list extra arg", ["pathways", "list", "extra"])
    add("list bad option", ["pathways", "list", "--nope"])
    bad_env = dict(r2, envelope_json='{"generated_by": "x"}')
    add("list invalid envelope", ["pathways", "list"], before=db(r1, bad_env))
    add("list envelope not json", ["pathways", "list"], before=db(dict(r1, envelope_json="{nope")))
    add("list embedding not json", ["pathways", "list"], before=db(dict(r1, task_embedding_json="[1.0,")))
    add("list embedding strings", ["pathways", "list"], before=db(dict(r1, task_embedding_json='["a"]')))
    add("list embedding ints", ["pathways", "show", "pw_0001", "--json"],
        before=db(dict(r1, task_embedding_json="[1, 0, 2]")))
    add("list hits float", ["pathways", "list"], before=db(dict(r1, hit_count=2.5)))
    add("list hits whole float", ["pathways", "list", "--json"], before=db(dict(r1, hit_count=2.0)))
    add("list distilled int", ["pathways", "list", "--json"], before=db(dict(r1, distilled_at=1700000000)))
    add("list params empty text", ["pathways", "list"], before=db(dict(r1, parameters_json="")))
    add("list traces not list", ["pathways", "list"], before=db(dict(r1, source_trace_ids_json='"t1"')))
    add("list signature null", ["pathways", "show", "pw_0001", "--json"],
        before=db(dict(r1, structure_signature=None)))
    add("list plan bad step", ["pathways", "list"],
        before=db(dict(r1, plan_template_json='{"id":"p","source":"s","task":"t","steps":[{"type":"nope","id":"x"}]}')))
    add("list plan missing id", ["pathways", "list"],
        before=db(dict(r1, plan_template_json='{"source":"s","task":"t","steps":[]}')))
    add("list summary too long", ["pathways", "list"],
        before=db(dict(r1, envelope_json=r1["envelope_json"].replace('"summary":null', '"summary":"' + "x" * 81 + '"'))))
    add("list velocity out of range", ["pathways", "list"],
        before=db(dict(r1, plan_template_json='{"id":"p","source":"s","task":"t","steps":[{"type":"joint_move","id":"x","joint_targets":{},"velocity_scale":1.5}]}')))
    add("list data dir is file", ["pathways", "list", "--data-dir", "f"], before={"f": {"text": "x"}})

    # show
    add("show rich", ["pathways", "show", "pw_0009"], before=db(r1, rr))
    add("show rich json", ["pathways", "show", "pw_0009", "--json"], before=db(r1, rr))
    add("show unicode", ["pathways", "show", "pw_0003"], before=db(r3))
    add("show unicode json", ["pathways", "show", "pw_0003", "--json"], before=db(r3))
    add("show lexical", ["pathways", "show", "pw_0004"], before=db(rl))
    add("show missing", ["pathways", "show", "pw_9999"], before=db(r1))
    add("show missing no store", ["pathways", "show", "nope"])
    add("show missing quote id", ["pathways", "show", "it's"], before=db(r1))
    add("show no arg", ["pathways", "show"])
    add("show two args", ["pathways", "show", "a", "b"])
    add("show first of dup-ish", ["pathways", "show", "pw_0002"], before=db(r1, r2, r3))

    # stats
    add("stats no store", ["pathways", "stats"])
    add("stats three", ["pathways", "stats"], before=db(r1, r2, r3))
    add("stats three json", ["pathways", "stats", "--json"], before=db(r1, r2, r3))
    add("stats float hits", ["pathways", "stats", "--json"], before=db(dict(r1, hit_count=1.5), r2))
    add("stats float hits text", ["pathways", "stats"], before=db(dict(r1, hit_count=1.5), r2))
    add("stats invalid rows", ["pathways", "stats"], before=db(bad_env))

    # delete
    add("delete one", ["pathways", "delete", "pw_0002"], before=db(r1, r2, r3))
    add("delete missing", ["pathways", "delete", "pw_0005"], before=db(r1))
    add("delete no store", ["pathways", "delete", "x"])
    add("delete invalid row", ["pathways", "delete", "pw_0002"], before=db(bad_env))
    add("delete no arg", ["pathways", "delete"])

    # export
    for fmt in ("json", "md", "mermaid", "smtlib"):
        add(f"export rich {fmt}", ["pathways", "export", "pw_0009", f"out/rich.{fmt}", "--format", fmt],
            before=db(r1, rr))
        add(f"export unicode {fmt}", ["pathways", "export", "pw_0003", "u.txt", f"--format={fmt}"],
            before=db(r3))
        add(f"export small {fmt}", ["pathways", "export", "pw_0001", "{HOME}/abs/x", "--format", fmt],
            before=db(r1))
    add("export skill", ["pathways", "export", "pw_0001", "s.md", "--format", "skill"], before=db(r1))
    add("export skill rich", ["pathways", "export", "pw_0009", "s.md"], before=db(rr))
    add("export skill unicode", ["pathways", "export", "pw_0003", "s.md"], before=db(r3))
    add("export unknown format", ["pathways", "export", "pw_0001", "x", "--format", "yaml"])
    add("export missing", ["pathways", "export", "pw_0007", "x.json", "--format", "json"], before=db(r1))
    add("export no args", ["pathways", "export"])
    add("export one arg", ["pathways", "export", "pw_0001"])
    add("export output dir", ["pathways", "export", "pw_0001", "adir", "--format", "json"],
        before=dict(db(r1), adir={"dir": True}))
    add("export dotted output", ["pathways", "export", "pw_0001", "./a//b/./c.md", "--format", "md"],
        before=db(r1))
    add("export overwrite", ["pathways", "export", "pw_0001", "x.md", "--format", "md"],
        before=dict(db(r1), **{"x.md": {"text": "old"}}))
    add("export cwd", ["pathways", "export", "pw_0001", "rel.md", "--format", "md"], before=db(r1), cwd="sub")
    md_edge = pathway(5, "t", [0.1], env=envelope(5, shell=False, shell_allowlist=[], file_read=[]),
                      pl=plan(5, [TaskStep(id="a", prompt=""), SkillStep(id="b", skill_id=""),
                                  MCPStep(id="c", server="", tool="")]),
                      model="", distilled_at=-1.5, source_trace_ids=[])
    add("export md edges", ["pathways", "export", "pw_0005", "e.md", "--format", "md"], before=db(put_row(md_edge)))
    add("export mermaid edges", ["pathways", "export", "pw_0005", "e.mmd", "--format", "mermaid"],
        before=db(put_row(md_edge)))
    add("export smtlib edges", ["pathways", "export", "pw_0005", "e.smt2", "--format", "smtlib"],
        before=db(put_row(md_edge)))

    # import
    def bundle(p, **extra) -> str:
        return export(p, "json") if not extra else json.dumps(
            {"opendaisugi_version": "0.43.0", "schema_version": 1, "pathway": p.model_dump(mode="json"), **extra},
            indent=2)

    good = pathway(6, "import me ✓", [0.25, 0.5], hit_count=2)
    good_b = export(good, "json")
    add("import json", ["pathways", "import", "b.json"], before={"b.json": {"text": good_b}})
    add("import json into rows", ["pathways", "import", "b.json"], before=dict(db(r1), **{"b.json": {"text": good_b}}))
    add("import duplicate", ["pathways", "import", "b.json"],
        before=dict(db(put_row(good)), **{"b.json": {"text": good_b}}))
    add("import overwrite", ["pathways", "import", "b.json", "--overwrite"],
        before=dict(db(r1, put_row(good)), **{"b.json": {"text": good_b}}))
    add("import overwrite new", ["pathways", "import", "b.json", "--overwrite"], before={"b.json": {"text": good_b}})
    add("import rich", ["pathways", "import", "r.json"], before={"r.json": {"text": export(rich, "json")}})
    deny = pathway(7, "denied", [1.0], env=envelope(7, shell=False, shell_allowlist=[]))
    add("import verify fails", ["pathways", "import", "d.json"], before={"d.json": {"text": export(deny, "json")}})
    add("import newer schema", ["pathways", "import", "n.json"],
        before={"n.json": {"text": json.dumps({"schema_version": 2, "pathway": {}})}})
    add("import not a bundle", ["pathways", "import", "x.txt"], before={"x.txt": {"text": "hello"}})
    add("import no pathway key", ["pathways", "import", "k.json"], before={"k.json": {"text": "{\"schema_version\": 1}"}})
    add("import invalid pathway", ["pathways", "import", "i.json"],
        before={"i.json": {"text": json.dumps({"pathway": {"id": "x"}})}})
    add("import bad json", ["pathways", "import", "j.json"], before={"j.json": {"text": "{\"pathway\": "}})
    add("import missing file", ["pathways", "import", "gone.json"])
    add("import leading space", ["pathways", "import", "w.json"], before={"w.json": {"text": "\n  " + good_b}})
    ints = json.loads(good_b)
    ints["pathway"]["task_embedding"] = [1, 2]
    ints["pathway"]["hit_count"] = 5.0
    ints["pathway"]["distilled_at"] = 1700000000
    add("import coerced", ["pathways", "import", "c.json"], before={"c.json": {"text": json.dumps(ints)}})
    add("import long task", ["pathways", "import", "l.json"],
        before={"l.json": {"text": export(pathway(8, "é" * 70 + " tail", [1.0]), "json")}})
    add("import skill", ["pathways", "import", "s.md"], before={"s.md": {"text": export(good, "skill")}})
    add("import skill rich", ["pathways", "import", "s.md"], before={"s.md": {"text": export(rich, "skill")}})
    add("import no arg", ["pathways", "import"])
    # What Python cannot store must not be stored: a lone surrogate in a
    # bound column or in the envelope or plan, and nesting past pydantic's
    # serialization depth.
    def with_text(old: str, new: str) -> dict[str, Any]:
        assert old in good_b
        return {"b.json": {"text": good_b.replace(old, new, 1)}}

    add("import surrogate task", ["pathways", "import", "b.json"],
        before=with_text("import me \\u2713", "x \\ud800 y"))
    add("import surrogate id", ["pathways", "import", "b.json"],
        before=with_text('"id": "pw_0006"', '"id": "pw_\\udc00"'))
    add("import surrogate command", ["pathways", "import", "b.json"],
        before=with_text('"command": "make test"', '"command": "make te\\ud800st"'))
    add("import surrogate envelope key", ["pathways", "import", "b.json"],
        before=with_text('"metadata": {}', '"metadata": {"k\\udfff": 1}'))
    add("import surrogate trace", ["pathways", "import", "b.json"], before=with_text('"t6a"', '"t\\ud800"'))
    add("import surrogate pair", ["pathways", "import", "b.json"],
        before=with_text("import me \\u2713", "x \\ud83d\\ude00 y"))
    for d in (196, 197, 253, 254, 300):
        add(f"import metadata depth {d}", ["pathways", "import", "b.json"],
            before=with_text('"metadata": {}', '"metadata": {"n": ' + "[" * d + "]" * d + "}"))
    add("import envelope expr depth 255", ["pathways", "import", "b.json"],
        before=with_text('"invariants": []', '"invariants": [{"type": "t", "description": "d", "enforce": false, "expr": '
                         + "[" * 255 + "]" * 255 + "}]"))
    nan = good_b.replace('"distilled_at": 1700021603.0', '"distilled_at": NaN')
    assert nan != good_b
    add("import overwrite unstorable keeps the old row", ["pathways", "import", "b.json", "--overwrite"],
        before=dict(db(put_row(good)), **{"b.json": {"text": nan}}))
    # NaN or an infinity in any number of the envelope or plan is invalid
    # (models.non_finite_error): SCHEMA_INCOMPATIBLE, nothing stored. It was
    # stored as null, which reads as no limit.
    def edited(fn) -> dict[str, Any]:
        data = json.loads(good_b)
        fn(data["pathway"])
        return {"b.json": {"text": json.dumps(data, indent=2)}}

    def perms(**kw):
        return lambda p: p["envelope"]["permissions"].update(kw)

    def step0(**kw):
        return lambda p: p["plan_template"]["steps"][0].update(kw)

    nan, inf = float("nan"), float("inf")
    add("import nan velocity", ["pathways", "import", "b.json"], before=edited(perms(velocity_limit=nan)))
    add("import infinity bound", ["pathways", "import", "b.json"],
        before=edited(perms(workspace_bounds=[[0, 0, 0], [1, -inf, 1]])))
    add("import two non-finite", ["pathways", "import", "b.json"],
        before=edited(perms(torque_limit=inf, joint_limits={"j": [nan, 1]})))
    add("import 1e999 velocity", ["pathways", "import", "b.json"],
        before={"b.json": {"text": json.dumps(json.loads(edited(perms(velocity_limit=7.25))["b.json"]["text"]))
                           .replace("7.25", "1e999")}})
    add("import nan string velocity", ["pathways", "import", "b.json"], before=edited(perms(velocity_limit="nan")))
    # In Python mode (json.loads, then model_validate) float(int) overflows:
    # a float_type error, not an infinity.
    add("import big int velocity", ["pathways", "import", "b.json"],
        before={"b.json": {"text": json.dumps(json.loads(edited(perms(velocity_limit=7.25))["b.json"]["text"]))
                           .replace("7.25", "1" + "0" * 400)}})
    add("import nan in expr", ["pathways", "import", "b.json"], before=edited(lambda p: p["envelope"].update(
        invariants=[{"type": "t", "description": "d", "enforce": False, "expr": {"op": "equals", "path": "x",
                                                                                "value": [1, nan]}}])))
    add("import nan step metadata", ["pathways", "import", "b.json"], before=edited(step0(metadata={"n": [1, nan]})))
    add("import nan joint target", ["pathways", "import", "b.json"], before=edited(lambda p: p["plan_template"].update(
        steps=[{"type": "joint_move", "id": "j", "joint_targets": {"a": inf}}])))
    add("import nan mcp argument", ["pathways", "import", "b.json"], before=edited(lambda p: p["plan_template"].update(
        steps=[{"type": "mcp", "id": "m", "server": "fs", "tool": "read", "arguments": {"x": nan}}])))
    add("import nan skill contract envelope", ["pathways", "import", "b.json"],
        before=edited(lambda p: p["plan_template"].update(steps=[{
            "type": "skill", "id": "k", "skill_id": "x",
            "contract_envelope": {"task": "t", "generated_by": "g", "permissions": {"velocity_limit": nan}}}])))
    add("import nan overwrite keeps the old row", ["pathways", "import", "b.json", "--overwrite"],
        before=dict(db(put_row(good)), **edited(perms(velocity_limit=nan))))
    vel = pathway(6, "import me ✓", [0.25, 0.5], hit_count=2,
                  env=envelope(6, velocity_limit=7.25))
    vel_skill = export(vel, "skill")
    assert vel_skill.count("7.25") == 1
    add("import nan skill", ["pathways", "import", "s.md"], before={"s.md": {"text": vel_skill.replace("7.25", ".nan")}})
    add("import inf skill", ["pathways", "import", "s.md"], before={"s.md": {"text": vel_skill.replace("7.25", "-.inf")}})
    # A stored row with such a number is invalid on read, as any invalid row.
    vel_row = put_row(vel)
    assert vel_row["envelope_json"].count("7.25") == 1
    nan_row = dict(vel_row, envelope_json=vel_row["envelope_json"].replace("7.25", "NaN"))
    add("list nan envelope row", ["pathways", "list"], before=db(r1, nan_row))
    add("show nan envelope row", ["pathways", "show", "pw_0006"], before=db(nan_row))
    add("stats nan envelope row", ["pathways", "stats"], before=db(nan_row))
    add("delete nan envelope row", ["pathways", "delete", "pw_0006"], before=db(nan_row))
    inf_plan = dict(r1, plan_template_json=r1["plan_template_json"].replace('"metadata":{}', '"metadata":{"x":Infinity}', 1))
    assert inf_plan != r1
    add("list infinity plan row", ["pathways", "list", "--json"], before=db(inf_plan))
    add("import big int", ["pathways", "import", "b.json"], before=with_text('"hit_count": 2', '"hit_count": 1180591620717411303424'))
    add("import crlf skill", ["pathways", "import", "s.md"],
        before={"s.md": {"text": export(good, "skill").replace("\n", "\r\n")}})
    add("import cr only json", ["pathways", "import", "b.json"], before={"b.json": {"text": good_b.replace("\n", "\r")}})
    add("import invalid utf8", ["pathways", "import", "b.json"],
        before={"b.json": {"hex": good_b.encode().replace(b"import me", b"import \xff me").hex()}})
    for t in ("0", "-1", "4294967296", "abc", " 7 ", "1_0"):
        add(f"import timeout {t}", ["pathways", "import", "b.json", "--z3-timeout-ms", t],
            before={"b.json": {"text": good_b}})
    for name, mut in [
        ("read outside", lambda b: b["pathway"]["plan_template"]["steps"][1].update(path="/etc/shadow")),
        ("write no perm", lambda b: b["pathway"]["plan_template"]["steps"].append(
            {"id": "s3", "depends_on": [], "type": "file_write", "path": "/work/x", "content": "x"})),
        ("network no perm", lambda b: b["pathway"]["plan_template"]["steps"].append(
            {"id": "s3", "depends_on": [], "type": "network", "url": "https://evil.test/"})),
        ("shell compound", lambda b: b["pathway"]["plan_template"]["steps"][0].update(command="make test; rm -rf /")),
        ("dep cycle", lambda b: b["pathway"]["plan_template"]["steps"][0].update(depends_on=["s2"])),
        ("dup step", lambda b: b["pathway"]["plan_template"]["steps"][1].update(id="s1")),
        ("max time 0", lambda b: b["pathway"]["envelope"]["permissions"].update(max_execution_time_s=0)),
        ("predicate violated", lambda b: b["pathway"]["envelope"].update(invariants=[
            {"type": "only_ls", "description": "d",
             "expr": {"op": "forall_steps", "pred": {"op": "equals", "path": "command", "value": "ls"}}}])),
    ]:
        data = json.loads(good_b)
        mut(data)
        add(f"import verify {name}", ["pathways", "import", "b.json"], before={"b.json": {"text": json.dumps(data)}})
    # The oracle's step dicts keep a cartesian_move's target_position and
    # target_orientation and a vla's target_pose as tuples, which equal no
    # list; and Python's == holds True == 1 == 1.0.
    robot = [{"id": "c1", "type": "cartesian_move", "target_position": [0.5, 0.5, 0.5],
              "target_orientation": [0, 0, 0, 1]},
             {"id": "v1", "type": "vla", "task": "x", "target_pose": [0.5, 0.5, 0.5]}]
    flagged = [{"id": "s1", "type": "shell", "command": "make test", "metadata": {"n": 1, "b": True, "l": [1, True]}}]
    for name, steps, pred in [
        ("tuple equals", robot, {"op": "equals", "path": "target_position", "value": [0.5, 0.5, 0.5]}),
        ("tuple not equals", robot, {"op": "not_equals", "path": "target_position", "value": [0.5, 0.5, 0.5]}),
        ("tuple orientation in set", robot, {"op": "in_set", "path": "target_orientation", "values": [[0, 0, 0, 1]]}),
        ("tuple pose not in set", robot, {"op": "not_in_set", "path": "target_pose", "values": [[0.5, 0.5, 0.5]]}),
        ("tuple length", robot, {"op": "length_range", "path": "target_position", "min": 3, "max": 3}),
        ("tuple exists", robot, {"op": "exists", "path": "target_position"}),
        ("true equals one", flagged, {"op": "not_equals", "path": "metadata.n", "value": True}),
        ("true in set", flagged, {"op": "not_in_set", "path": "metadata.b", "values": [1.0]}),
        ("list of true", flagged, {"op": "equals", "path": "metadata.l", "value": [True, 1.0]}),
    ]:
        data = json.loads(good_b)
        data["pathway"]["plan_template"]["steps"] = steps
        data["pathway"]["envelope"]["invariants"] = [
            {"type": "rule", "description": "d", "expr": {"op": "exists_step", "pred": pred}}]
        add(f"import verify {name}", ["pathways", "import", "b.json"], before={"b.json": {"text": json.dumps(data)}})
    add("list data dir file uri", ["pathways", "list", "--data-dir", "file:zz"], before=db(r1)
        and {"file:zz/pathways.db": db(r1)[DB]})
    add("pathways no command", ["pathways", "nope"])
    return C


# ---------------------------------------------------------------------------
# Find cases
# ---------------------------------------------------------------------------

_FIND_ORACLE = r"""
import json, sys, warnings
from pathlib import Path
import numpy as np
import opendaisugi.pathway_store as ps
from opendaisugi.pathway_store import PathwayStore
from opendaisugi._similarity import cosine_similarity_batch

def tie_ids(store, task, best):
    # Every row whose score is within 1e-12 of the best: which of them
    # wins is decided by BLAS rounding, which differs between CPUs.
    from opendaisugi._search import active_model_name
    ident = active_model_name()
    rows = [r for r in store._load_all_rows()
            if (not r["embedding_model"] and not r["embedding_model_version"])
            or (r["embedding_model"] == ident and r["embedding_model_version"] == "3")]
    q = store._embed_query(task)
    keep = [(r["id"], json.loads(r["task_embedding_json"])) for r in rows]
    keep = [(i, v) for i, v in keep if len(v) == len(q)]
    scores = cosine_similarity_batch(q, np.array([v for _, v in keep]))
    return sorted(i for (i, _), s in zip(keep, scores) if abs(float(s) - best) <= 1e-12)

store = PathwayStore(Path(sys.argv[1]))
out = []
for line in sys.stdin:
    q = json.loads(line)
    ps._stale_embeddings_warned = False
    with warnings.catch_warnings(record=True) as w:
        warnings.simplefilter("always")
        m = store.find(q["task"], threshold=q.get("threshold"))
    warn = [str(x.message) for x in w if issubclass(x.category, UserWarning)]
    got = {"id": m.pathway.id if m else None,
           "score": round(m.similarity, 6) if m else None,
           "warning": warn[0] if warn else ""}
    if m:
        ties = tie_ids(store, q["task"], m.similarity)
        if len(ties) > 1:
            got["tie_ids"] = ties
    out.append(got)
print(json.dumps(out))
"""


def run_find_oracle(case: dict[str, Any], work: Path) -> list[dict[str, Any]]:
    if work.exists():
        shutil.rmtree(work)
    home = work / "home"
    tree = {".opendaisugi/config.yaml": {"text": f"matcher_model: {case['matcher']}\n"}}
    lay_out(tree, home)
    dbp = home / "store.db"
    lay_out_db(dbp, {"rows": case["rows"]}, str(home))
    tiny = home / "potion-tiny"
    shutil.copytree(FIXTURE_DIR / "potion-tiny", tiny)
    env = base_env(home)
    env.update({k: v.replace("{HOME}", str(home)) for k, v in (case.get("env") or {}).items()})
    if case.get("real_potion"):
        env["HF_HUB_CACHE"] = str(hf_cache())
    qs = "".join(json.dumps({k: v for k, v in q.items() if k != "expect"}) + "\n" for q in case["queries"])
    proc = subprocess.run([sys.executable, "-c", _FIND_ORACLE, str(dbp)], input=qs.encode(), env=env,
                          capture_output=True, timeout=600, check=False)
    if proc.returncode != 0:
        raise SystemExit(f"find oracle failed for {case['name']}:\n{proc.stderr.decode()[-2000:]}")
    return json.loads(proc.stdout)


def build_find_cases(real: Path | None) -> list[dict[str, Any]]:
    tiny = FIXTURE_DIR / "potion-tiny"
    C: list[dict[str, Any]] = []
    texts = [
        "build the release", "run the unit tests", "deploy the docs site", "migrate the database",
        "fetch and parse the logs", "clean the cache", "write the changelog", "search the index",
        "backup the database to s3", "render the html report",
    ]
    queries = [
        "build the release", "Build the RELEASE please", "run tests", "deploy docs", "database migration",
        "parse logs", "totally unrelated words here", "", "the a an", "cache clean up", "html report render",
        "中文 release", "résumé build",
    ]

    def lex_rows(tasks, model="lexical-hash-v1", version="3", start=0):
        return [row_spec(put_row(pathway(start + i, t, lexical(t), model=model, emb_version=version)))
                for i, t in enumerate(tasks)]

    def qs(items, **extra):
        return [dict({"task": q}, **extra) for q in items]

    C.append({"name": "lexical basic", "matcher": "lexical", "rows": lex_rows(texts), "queries": qs(queries)})
    C.append({"name": "lexical empty store", "matcher": "lexical", "rows": [], "queries": qs(queries[:2])})
    C.append({"name": "minilm empty store", "matcher": "all-MiniLM-L6-v2", "rows": [], "queries": qs(queries[:1])})
    C.append({"name": "unknown matcher", "matcher": "bogus", "rows": lex_rows(texts[:3]), "queries": qs(queries[:2])})
    stale = lex_rows(texts[:8]) + lex_rows(texts[8:], model="all-MiniLM-L6-v2", start=8)
    C.append({"name": "lexical stale 20pct", "matcher": "lexical", "rows": stale, "queries": qs(queries[:3])})
    few = lex_rows(texts[:9]) + lex_rows(texts[9:], version="2", start=9)
    C.append({"name": "lexical stale 10pct", "matcher": "lexical", "rows": few, "queries": qs(queries[:2])})
    C.append({"name": "lexical all stale", "matcher": "lexical",
              "rows": lex_rows(texts[:3], model="minishlab/potion-base-8M"), "queries": qs(queries[:2])})
    legacy = lex_rows(texts[:4], model="", version="")
    dim = [row_spec(put_row(pathway(20, "build the release", [0.1] * 384, model="", emb_version="")))]
    C.append({"name": "lexical legacy wildcards and dim guard", "matcher": "lexical", "rows": dim + legacy,
              "queries": qs(queries[:4])})
    C.append({"name": "lexical only other dims", "matcher": "lexical", "rows": dim, "queries": qs(queries[:2])})
    C.append({"name": "lexical thresholds", "matcher": "lexical", "rows": lex_rows(texts),
              "queries": qs(["build release", "run the unit"], threshold=0.0) + qs(["build release"], threshold=0.99)})
    dup = lex_rows(["build the release", "build the release", "Build the release!"])
    C.append({"name": "lexical ties", "matcher": "lexical", "rows": dup, "queries": qs(queries[:3])})
    zero = [row_spec(put_row(pathway(30, "zero", [0.0] * 4096)))] + lex_rows(texts[:2], start=31)
    C.append({"name": "lexical zero vector row", "matcher": "lexical", "rows": zero, "queries": qs(["", "build"])})
    rnd = random.Random(5)
    many = lex_rows([" ".join(rnd.choice(TINY_WORDS) for _ in range(rnd.randint(2, 7))) for _ in range(60)])
    C.append({"name": "lexical sixty", "matcher": "lexical", "rows": many, "queries": qs(random_texts(40, 3))})
    C.append({"name": "lexical fixed texts", "matcher": "lexical", "rows": lex_rows(FIXED_TEXTS[:20]),
              "queries": qs(FIXED_TEXTS)})

    def tiny_rows(tasks, start=0):
        ident = "{HOME}/potion-tiny"
        return [row_spec(put_row(pathway(start + i, t, potion_vec(tiny, t), model=ident)))
                for i, t in enumerate(tasks)]

    tenv = {"OPENDAISUGI_POTION_MODEL": "{HOME}/potion-tiny"}
    C.append({"name": "tiny potion", "matcher": "potion", "env": tenv, "rows": tiny_rows(texts),
              "queries": qs(queries + FIXED_TEXTS)})
    C.append({"name": "tiny potion random", "matcher": "potion", "env": tenv, "rows": tiny_rows(random_texts(30, 9)),
              "queries": qs(random_texts(60, 10))})
    C.append({"name": "potion missing model", "matcher": "potion", "env": {"OPENDAISUGI_POTION_MODEL": "{HOME}/nope"},
              "rows": tiny_rows(texts[:3]), "queries": qs(queries[:2])})
    C.append({"name": "potion model is a file", "matcher": "potion",
              "env": {"OPENDAISUGI_POTION_MODEL": "{HOME}/.opendaisugi/config.yaml"},
              "rows": tiny_rows(texts[:3]), "queries": qs(queries[:1])})
    C.append({"name": "potion rows under lexical", "matcher": "lexical", "rows": tiny_rows(texts[:3]),
              "queries": qs(queries[:2])})
    C.append({"name": "lexical rows under potion", "matcher": "potion", "env": tenv, "rows": lex_rows(texts[:3]),
              "queries": qs(queries[:2])})
    go_only = [
        {"name": "minilm refused", "matcher": "all-MiniLM-L6-v2", "rows": lex_rows(texts[:2]),
         "queries": [{"task": "build", "expect": {"refused": True}}]},
        {"name": "int8 refused", "matcher": "int8", "rows": lex_rows(texts[:2]),
         "queries": [{"task": "build", "expect": {"refused": True}}]},
    ]
    for c in go_only:
        c["go_only"] = True
    C.extend(go_only)
    if real is not None:
        def real_rows(tasks):
            return [row_spec(put_row(pathway(i, t, potion_vec(real, t), model=REAL_POTION)))
                    for i, t in enumerate(tasks)]

        C.append({"name": "real potion", "matcher": "potion", "real_potion": True, "rows": real_rows(texts),
                  "queries": qs(queries + FIXED_TEXTS)})
        C.append({"name": "real potion random", "matcher": "potion", "real_potion": True,
                  "rows": real_rows(random_texts(30, 11)), "queries": qs(random_texts(80, 12))})
    return C


# ---------------------------------------------------------------------------
# Verify messages
# ---------------------------------------------------------------------------


def verify_case(plan: dict[str, Any], env: dict[str, Any], strict: bool | None = None) -> dict[str, Any]:
    """One verify() call as import makes it: the violations' stages, steps
    and messages, and the Z3 timeouts it kept as warnings."""
    from opendaisugi.models import ActionPlan, Envelope
    from opendaisugi.verify import verify

    p = ActionPlan.model_validate(plan)
    e = Envelope.model_validate(env)
    r = verify(p, e, z3_timeout_ms=500, strict=strict)
    # As text: the case file sorts keys, and a dict's key order is input.
    return {
        "plan": p.model_dump_json(),
        "envelope": e.model_dump_json(),
        "strict": strict,
        "expect": {
            "ok": r.ok,
            "violations": [[v.stage, v.detail.get("step"), v.message] for v in r.violations],
        },
    }


def build_verify_cases() -> list[dict[str, Any]]:
    """Every violation the verifier words, on synthetic plans, and a seeded
    set of random plans for the order and the cycle it names."""
    base_perm = {"shell": True, "shell_allowlist": ["make", "git", "sh", "bash", "python", "xargs", "cat"],
                 "file_read": ["/work/**"], "file_write": ["/work/out/**"], "network": True,
                 "network_hosts": ["example.com", "API.test"], "mcp_allowlist": ["fs/read", "db/*"]}

    def env(**kw):
        perm = dict(base_perm)
        perm.update(kw.pop("perm", {}))
        e = {"id": "env_v", "generated_by": "t", "task": "t", "permissions": perm}
        e.update(kw)
        return e

    def plan(*steps):
        return {"id": "plan_v", "source": "s", "task": "t", "steps": list(steps)}

    def sh(i, c, **kw):
        return {"id": i, "type": "shell", "command": c, **kw}

    C = []
    shells = ["make test", "rm -rf /", "make test; rm x", "sh -c 'rm x'", "sh -c 'make a'", "python -c 'print(1)'",
              "bash -c \"sh -c 'rm y'\"", "cat a > /etc/x", "cat < /etc/shadow", "make >/work/out/log",
              "git status && curl x", "xargs rm", "  ", "# comment", "FOO=1 make", "env rm x",
              "sh -c 'sh -c \"sh -c \\\"sh -c rm\\\"\"'", "make $(whoami)", "echo hi"]
    for policy in ("surface", "strict"):
        for dec in (False, True):
            for c in shells:
                C.append(verify_case(plan(sh("s1", c)), env(shell_interpreter_policy=policy,
                                                           perm={"shell_allow_decomposition": dec})))
    C.append(verify_case(plan(sh("s1", "make")), env(perm={"shell": False, "shell_allowlist": []})))
    for url in ("https://example.com/a", "https://EXAMPLE.com/x", "http://evil.test/", "file:///etc/passwd",
                "ftp://example.com/x", "//example.com", "https://api.test:8080/p", "notaurl"):
        C.append(verify_case(plan({"id": "n1", "type": "network", "url": url}), env()))
    C.append(verify_case(plan({"id": "n1", "type": "network", "url": "https://x.test"}), env(perm={"network": False})))
    for path in ("/work/a", "/etc/shadow", "/work/../etc/x", "relative"):
        C.append(verify_case(plan({"id": "r1", "type": "file_read", "path": path}), env()))
        C.append(verify_case(plan({"id": "w1", "type": "file_write", "path": path, "content": "x"}), env()))
    for server, tool in (("fs", "read"), ("fs", "write"), ("db", "q")):
        C.append(verify_case(plan({"id": "m1", "type": "mcp", "server": server, "tool": tool}), env()))
    for tools, ws in (([], "/work"), (["Read"], "/tmp"), (["Nope", "Bash"], "/work"), (["WebFetch", "Write"], "/work")):
        C.append(verify_case(plan({"id": "a1", "type": "agentic", "prompt": "p", "workspace": ws, "tools": tools}),
                             env(perm={"network": False, "file_write": []})))
    phys = {"stakes": "physical"}
    C.append(verify_case(plan({"id": "t1", "type": "task", "prompt": "p", "preferred_model": "haiku"},
                              {"id": "a1", "type": "agentic", "prompt": "p", "workspace": "/work", "tools": ["Read"]}),
                         env(**phys)))
    C.append(verify_case(plan({"id": "k1", "type": "skill", "skill_id": "pdf"}), env(stakes="high")))
    C.append(verify_case(plan({"id": "k1", "type": "skill", "skill_id": "pdf"}), env()))
    # DAG
    C.append(verify_case(plan(sh("a", "make"), sh("a", "make"), sh("b", "make"), sh("b", "make")), env()))
    C.append(verify_case(plan(sh("a", "make", depends_on=["zz", "b"]), sh("b", "make", depends_on=["yy"])), env()))
    C.append(verify_case(plan(sh("a", "make", depends_on=["c"]), sh("b", "make", depends_on=["a"]),
                              sh("c", "make", depends_on=["b"])), env()))
    C.append(verify_case(plan(sh("a", "make", depends_on=["a"])), env()))
    # Z3
    C.append(verify_case(plan(), env(perm={"shell": False})))
    C.append(verify_case(plan(), env(perm={"max_execution_time_s": 0})))
    C.append(verify_case(plan(), env(perm={"max_execution_time_s": 3601})))
    C.append(verify_case(plan(), env(perm={"file_write": []}, postconditions=[{"type": "file_exists", "path": "/o"}])))
    # Robotics
    rob = {"workspace_bounds": [[0, 0, 0], [1, 1, 1]], "velocity_limit": 0.5,
           "joint_limits": {"j2": [-1, 1], "j1": [-0.5, 0.5]}, "obstacles": [[[0.4, 0.4, 0.4], [0.6, 0.6, 0.6]]]}
    invs = [{"type": t, "description": "d"} for t in
            ("end_effector_in_workspace", "joint_limits_respected", "velocity_bounded", "no_obstacle_penetration")]
    C.append(verify_case(plan({"id": "c1", "type": "cartesian_move", "target_position": [2, 0.5, 0.5]},
                              {"id": "c2", "type": "cartesian_move", "target_position": [1, 1, 1]},
                              {"id": "v1", "type": "vla", "task": "x", "target_pose": [0.1, 5, 0.1]},
                              {"id": "j1", "type": "joint_move", "joint_targets": {"j3": 0.1, "j1": 0.9, "j2": 0.2},
                               "duration_s": 0.3},
                              {"id": "j2", "type": "joint_move", "joint_targets": {"j2": -0.2}, "velocity_scale": 0.25}),
                         env(perm=rob, invariants=invs)))
    C.append(verify_case(plan(), env(invariants=invs[:3])))
    # Predicates
    def inv(expr, t="rule", enforce=True):
        return {"type": t, "description": "d", "expr": expr, "enforce": enforce}
    exprs = [
        {"op": "forall_steps", "pred": {"op": "equals", "path": "command", "value": "ls"}},
        {"op": "forall_steps", "pred": {"op": "equals", "path": "command", "value": "make test"}},
        {"op": "and", "children": [{"op": "exists", "path": "x"}, {"op": "not", "child": {"op": "exists", "path": "x"}}]},
        {"op": "or", "children": [{"op": "exists", "path": "x"}, {"op": "not", "child": {"op": "exists", "path": "x"}}]},
        {"op": "alias", "name": "no_rm"},
        "x > 1 and x < 0", 5, 1.5, [1], True,
    ]
    for strict in (None, True, False):
        for e in exprs:
            C.append(verify_case(plan(sh("s1", "make test")), env(invariants=[inv(e)]), strict=strict))
            C.append(verify_case(plan(sh("s1", "make test")), env(postconditions=[
                {"type": "custom", "expr": e}]), strict=strict))
        C.append(verify_case(plan(sh("s1", "make test")), env(invariants=[inv(None, "custom_prop")]), strict=strict))
        C.append(verify_case(plan(sh("s1", "make test")), env(postconditions=[{"type": "odd"}]), strict=strict))
    # Python's == holds True == 1 == 1.0, in lists and dicts too.
    flagged = sh("s1", "make test", metadata={"n": 1, "b": True, "l": [1, True], "z": 0})
    for pred in [{"op": "not_equals", "path": "metadata.n", "value": True},
                 {"op": "not_in_set", "path": "metadata.b", "values": [1]},
                 {"op": "not_equals", "path": "metadata.l", "value": [1.0, 1]},
                 {"op": "not_equals", "path": "metadata.z", "value": False},
                 {"op": "equals", "path": "metadata.b", "value": 1.0},
                 {"op": "in_set", "path": "metadata", "values": [{"z": False, "l": [True, 1], "b": 1, "n": True}]},
                 {"op": "equals", "path": "metadata.n", "value": "1"}]:
        C.append(verify_case(plan(flagged), env(invariants=[inv({"op": "forall_steps", "pred": pred})])))
    # Random plans: order of violations and the cycle networkx names.
    rng = random.Random(17)
    pool = [
        lambda i: sh(i, rng.choice(shells)),
        lambda i: {"id": i, "type": "file_read", "path": rng.choice(["/work/a", "/etc/x"])},
        lambda i: {"id": i, "type": "file_write", "path": rng.choice(["/work/out/a", "/etc/x"]), "content": ""},
        lambda i: {"id": i, "type": "network", "url": rng.choice(["https://example.com", "http://x.test", "ftp://a"])},
        lambda i: {"id": i, "type": "mcp", "server": rng.choice(["fs", "db", "x"]), "tool": "read"},
    ]
    for _ in range(240):
        n = rng.randint(1, 7)
        ids = [rng.choice("abcdefgh") for _ in range(n)] if rng.random() < 0.2 else [f"s{k}" for k in range(n)]
        steps = []
        for k in range(n):
            st = rng.choice(pool)(ids[k])
            st["depends_on"] = [rng.choice(ids + ["zz"]) for _ in range(rng.randint(0, 3))] if rng.random() < 0.7 else []
            if rng.random() < 0.5:
                st = {"id": ids[k], "type": "task", "prompt": "p", "depends_on": st["depends_on"]}
            steps.append(st)
        C.append(verify_case(plan(*steps), env(shell_interpreter_policy=rng.choice(["surface", "strict"]))))
    for _ in range(150):
        n = rng.randint(2, 9)
        ids = [f"t{k}" for k in range(n)]
        rng.shuffle(ids)
        steps = [{"id": i, "type": "task", "prompt": "p",
                  "depends_on": [rng.choice(ids) for _ in range(rng.randint(0, 3))]} for i in ids]
        C.append(verify_case(plan(*steps), env()))
    return C


# ---------------------------------------------------------------------------
# Units
# ---------------------------------------------------------------------------


def build_units(real: Path | None) -> dict[str, Any]:
    from opendaisugi._search import _LEXICAL_STOPWORDS, _LEXICAL_TOKEN_RE
    from tokenizers import Tokenizer

    texts = FIXED_TEXTS + random_texts(300, 1)
    tokens = sorted({t for x in texts for t in _LEXICAL_TOKEN_RE.findall(x.lower())} | {"a" * 200, "_", "0"})
    blake = [[t, hashlib.blake2b(t.encode(), digest_size=8).hexdigest()] for t in tokens]
    lex = []
    for x in texts:
        v = lexical(x)
        lex.append({
            "text": x,
            "tokens": [t for t in _LEXICAL_TOKEN_RE.findall(x.lower()) if t not in _LEXICAL_STOPWORDS],
            "nz": [[i, y] for i, y in enumerate(v) if y != 0.0],
        })
    tiny = FIXTURE_DIR / "potion-tiny"
    tok = Tokenizer.from_file(str(tiny / "tokenizer.json"))
    ids = [{"text": x, "ids": e.ids} for x, e in zip(texts, tok.encode_batch_fast(texts, add_special_tokens=False),
                                                      strict=True)]
    vecs = [{"text": x, "vec": potion_vec(tiny, x)} for x in texts]
    return {"blake2b": blake, "lexical": lex, "tiny_tokens": ids, "tiny_vectors": vecs}


def real_token_texts() -> list[str]:
    """Texts for the real tokenizer's id check (compare time, not committed)."""
    return FIXED_TEXTS + random_texts(3000, 21)


# ---------------------------------------------------------------------------
# Writing
# ---------------------------------------------------------------------------


def write_jsonl(path: Path, items: list[dict[str, Any]]) -> dict[str, Any]:
    for it in items:
        it.pop("id", None)
        it["id"] = case_id(it)
    items.sort(key=lambda c: c["id"])
    data = "".join(canonical_json(c) + "\n" for c in items).encode("utf-8")
    path.write_bytes(data)
    return {"count": len(items), "sha256": hashlib.sha256(data).hexdigest()}


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--out", type=Path, default=FIXTURE_DIR)
    ap.add_argument("--only", help="run only CLI cases whose name holds this text; print, write nothing")
    args = ap.parse_args()
    out: Path = args.out
    out.mkdir(parents=True, exist_ok=True)
    build_tiny_model(out / "potion-tiny")
    real = real_potion_dir()
    SCRATCH.mkdir(parents=True, exist_ok=True)

    cases = build_cli_cases()
    names = [c["name"] for c in cases]
    dup = {n for n in names if names.count(n) > 1}
    if dup:
        raise SystemExit(f"duplicate case names: {sorted(dup)}")
    for i, c in enumerate(cases):
        if args.only and args.only not in c["name"]:
            continue
        c["expect"] = run_cli_case(c, PY_CLI, SCRATCH / "gen" / f"{i:04d}")
        if args.only:
            print(json.dumps({"name": c["name"], **c["expect"]}, indent=1, ensure_ascii=False)[:6000])
        else:
            print(f"{i + 1:4d}/{len(cases)} exit={c['expect']['exit']} {c['name']}", flush=True)
    if args.only:
        return 0

    finds = build_find_cases(real)
    for i, c in enumerate(finds):
        if c.get("go_only"):
            continue
        got = run_find_oracle(c, SCRATCH / "find" / f"{i:04d}")
        for q, g in zip(c["queries"], got, strict=True):
            q["expect"] = g
        print(f"find {i + 1}/{len(finds)} {c['name']}: {sum(1 for g in got if g['id'])}/{len(got)} matched",
              flush=True)

    manifest = {"v": CASE_VERSION, "real_potion_cases": real is not None}
    manifest["cases.jsonl"] = write_jsonl(out / "cases.jsonl", cases)
    manifest["find.jsonl"] = write_jsonl(out / "find.jsonl", finds)
    manifest["verify_messages.jsonl"] = write_jsonl(out / "verify_messages.jsonl", build_verify_cases())
    units = json.dumps(build_units(real), ensure_ascii=True, sort_keys=True, indent=0).encode()
    (out / "units.json").write_bytes(units + b"\n")
    manifest["units.json"] = {"sha256": hashlib.sha256(units + b"\n").hexdigest()}
    (out / "manifest.json").write_text(json.dumps(manifest, indent=1, sort_keys=True) + "\n", encoding="utf-8")
    shutil.rmtree(SCRATCH / "gen", ignore_errors=True)
    shutil.rmtree(SCRATCH / "find", ignore_errors=True)
    from fixture_paths import leaks

    leaked = leaks()
    if leaked:
        raise SystemExit("a machine path reached the fixtures:\n" + "\n".join(leaked[:10]))
    print(f"wrote {len(cases)} CLI cases, {len(finds)} find cases (real potion: {real is not None}) to {out}")
    return 0


if __name__ == "__main__":
    sys.path.insert(0, str(Path(__file__).resolve().parent))
    raise SystemExit(main())
