"""Synthetic stage-L cases: the git pathway registry (`daisugi registry
init|pull|publish|status|pull-and-tend`), batch proofs (`daisugi batch
prove`), release signing (`daisugi release keygen|sign|verify`), and
probes of the library parts no command reaches (the deeds ledger, the
strata store, batch runs, pathway bundles and the signing primitives),
run through the Python oracle.

    uv run --no-sync python clients/l_cases.py [--out clients/fixtures/l] [--only NAME]

Two kinds of case:

- cli: one command, run as garden_cases runs one: a scratch HOME, and the
  exit code, stdout, stderr and the tree after recorded.
- probe: one query of the library, the argv the oracle's script (PROBE)
  and the binary's instrument (cmd/l-probe) both take.

Git: a tree entry "gitbare" is a bare repository with the commits the
case names, "gitclone" a clone of one (with local commits and files of
its own). The harness runs git with a scrubbed environment: no system or
user config but the case's own (work/gitconfig), fixed author, committer
and dates, so a commit id is the same on every run. The command under
test gets the same variables, and drops every GIT_* one: the git it runs
reads its identity and default branch from the user config
XDG_CONFIG_HOME names (work/xdg/git/config). A tree reader summarizes every git
directory (its refs, its log, the files at HEAD and, for a work tree,
`git status --porcelain`) instead of reading its objects. Lines git
itself writes to stderr are left out on both sides: their wording
changes from one git version to the next.

Keys are synthetic ed25519 keys made from fixed seeds. A case whose
output holds values made from the clock or a random key (a bundle
published now, a manifest signed now, a key made by keygen) names them in
`vary`; those values are renamed by order of first appearance, and
l_compare.py checks each such value with the oracle's own code instead.

Every path, key and task is synthetic.
"""

from __future__ import annotations

import argparse
import base64
import hashlib
import json
import os
import re
import shutil
import stat
import subprocess
import sys
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))

import garden_cases  # noqa: E402 - sibling module, run as a script
import k2_cases  # noqa: E402, F401 - its normalizing of run ids, timings and receipts
from garden_cases import PY_CLI, fill_text, run_case, write_jsonl  # noqa: E402
from pathway_cases import REPO, envelope, pathway, put_row, row_spec  # noqa: E402

FIXTURE_DIR = REPO / "clients" / "fixtures" / "l"
SCRATCH = Path(
    os.environ.get("DAISUGI_L_SCRATCH") or Path.home() / "opendaisugi-scratch" / "l" / "runs"
)
CASE_VERSION = 1
CONFIG = ".opendaisugi/config.yaml"
LEX = {CONFIG: {"text": "matcher_model: lexical\n"}}
DB = ".opendaisugi/pathways.db"
REG = ".opendaisugi/registry"
GIT_DATE = "2026-01-01T00:00:00+0000"
GIT_ENV = {
    "GIT_CONFIG_NOSYSTEM": "1",
    "GIT_CONFIG_GLOBAL": "{HOME}/../gitconfig",
    "GIT_AUTHOR_NAME": "Case Author",
    "GIT_AUTHOR_EMAIL": "author@example.invalid",
    "GIT_COMMITTER_NAME": "Case Committer",
    "GIT_COMMITTER_EMAIL": "committer@example.invalid",
    "GIT_AUTHOR_DATE": GIT_DATE,
    "GIT_COMMITTER_DATE": GIT_DATE,
    "GIT_TERMINAL_PROMPT": "0",
    "GIT_CEILING_DIRECTORIES": "{HOME}/..",
}
GITCONFIG = "[init]\n\tdefaultBranch = main\n[advice]\n\tdetachedHead = false\n"
# The user config the command's own git reads (it drops every GIT_*
# variable, so the identity and branch cannot come from the environment).
USER_GITCONFIG = GITCONFIG + "[user]\n\tname = Case User\n\temail = user@example.invalid\n"

# ---------------------------------------------------------------------------
# Keys
# ---------------------------------------------------------------------------


def seed(name: str) -> bytes:
    return hashlib.sha256(b"opendaisugi-l-case-key:" + name.encode()).digest()


def keypair(name: str) -> tuple[str, str]:
    """(private_b64, public_b64) as signing.generate_keypair writes them:
    the raw 32-byte seed and the raw 32-byte public key, base64."""
    from cryptography.hazmat.primitives import serialization
    from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey

    k = Ed25519PrivateKey.from_private_bytes(seed(name))
    pub = k.public_key().public_bytes(
        encoding=serialization.Encoding.Raw, format=serialization.PublicFormat.Raw
    )
    return base64.b64encode(seed(name)).decode(), base64.b64encode(pub).decode()


def key_files(name: str, where: str = "keys") -> dict[str, Any]:
    priv, pub = keypair(name)
    return {
        f"{where}/{name}.key": {"text": priv + "\n", "mode": 0o600},
        f"{where}/{name}.pub": {"text": pub + "\n"},
    }


# ---------------------------------------------------------------------------
# Git in the tree
# ---------------------------------------------------------------------------


def git_env(home: Path) -> dict[str, str]:
    env = {k: v.replace("{HOME}", str(home)) for k, v in GIT_ENV.items()}
    env.update({"PATH": "/usr/bin:/bin", "HOME": str(home), "LANG": "C.UTF-8"})
    return env


def git(home: Path, *args: str, cwd: Path | None = None) -> str:
    p = subprocess.run(
        ["git", *args],
        cwd=cwd or home,
        env=git_env(home),
        capture_output=True,
        text=True,
        check=False,
    )
    if p.returncode != 0:
        raise SystemExit(f"git {' '.join(args)}: {p.stderr}")
    return p.stdout


def _write(path: Path, spec: Any, home: Path, t0: float) -> None:
    if spec is None:
        path.unlink()
        return
    path.parent.mkdir(parents=True, exist_ok=True)
    if isinstance(spec, str):
        spec = {"text": spec}
    if "db" in spec:
        garden_cases.lay_out_pathways(path, spec["db"], str(home), t0)
    elif "hex" in spec:
        path.write_bytes(bytes.fromhex(spec["hex"]))
    else:
        path.write_bytes(fill_text(spec["text"], str(home), t0).encode("utf-8"))
    if "mode" in spec:
        os.chmod(path, spec["mode"])


def _commit_all(home: Path, repo: Path, commits: list[dict[str, Any]], t0: float) -> None:
    for c in commits:
        for rel, spec in c["files"].items():
            _write(repo / rel, spec, home, t0)
        git(home, "add", "-A", cwd=repo)
        git(home, "commit", "-q", "--allow-empty", "-m", c["message"], cwd=repo)


def lay_out_bare(home: Path, path: Path, spec: dict[str, Any], t0: float) -> None:
    git(home, "init", "-q", "--bare", "--object-format=sha1", str(path))
    if spec.get("commits"):
        seedrepo = home.parent / "tmp" / f"seed-{path.name}"
        git(home, "init", "-q", "--object-format=sha1", str(seedrepo))
        _commit_all(home, seedrepo, spec["commits"], t0)
        git(home, "push", "-q", str(path), "main", cwd=seedrepo)
        shutil.rmtree(seedrepo)


def lay_out_clone(home: Path, path: Path, spec: dict[str, Any], t0: float) -> None:
    git(home, "clone", "-q", str(home / spec["of"]), str(path))
    _commit_all(home, path, spec.get("commits", []), t0)
    if spec.get("push"):
        git(home, "push", "-q", "origin", "main", cwd=path)
    if "origin" in spec:
        git(home, "remote", "set-url", "origin", fill_text(spec["origin"], str(home), t0), cwd=path)


def lay_out_receipts(data_dir: Path, receipts: list[dict[str, Any]], home: Path) -> None:
    """A journal holding the receipts given, each written by Journal.append_receipt."""
    from opendaisugi.journal import Journal
    from opendaisugi.models import Receipt, ReversalHandle

    j = Journal(data_dir=data_dir)
    try:
        for r in receipts:
            rev = r.get("reversal")
            if rev is not None:
                rev = json.loads(json.dumps(rev).replace("{HOME}", str(home)))
            j.append_receipt(
                Receipt(
                    step_id=r["step_id"],
                    run_id=r["run_id"],
                    timestamp=r["at"],
                    evidence={"rc": 0},
                    evidence_hash="h-" + r["step_id"],
                    verify_result=True,
                    effect_class=r.get("effect_class"),
                    reversibility=r.get("reversibility"),
                    reversal=ReversalHandle.model_validate(rev) if rev else None,
                )
            )
    finally:
        j.close()


_garden_lay_out = garden_cases.lay_out


def lay_out(tree: dict[str, Any], home: Path, t0: float) -> None:
    """garden_cases.lay_out, with bare repositories and their clones: the
    plain entries outside every clone first, then the bare repositories,
    then the clones, then the entries inside a clone."""
    clones = sorted(rel for rel, s in tree.items() if "gitclone" in s)

    def inside(rel: str) -> bool:
        return any(rel.startswith(c + "/") for c in clones)

    plain = {
        rel: s
        for rel, s in tree.items()
        if not ({"gitbare", "gitclone", "link", "receipts"} & set(s)) and not inside(rel)
    }
    _garden_lay_out(plain, home, t0)
    for rel, s in sorted(tree.items()):
        if "receipts" in s:
            lay_out_receipts(home / rel, s["receipts"], home)
    for rel, s in sorted(tree.items()):
        if "link" in s:
            (home / rel).parent.mkdir(parents=True, exist_ok=True)
            os.symlink(fill_text(s["link"], str(home), t0), home / rel)
    (home.parent / "gitconfig").write_text(GITCONFIG, encoding="utf-8")
    (home.parent / "xdg" / "git").mkdir(parents=True, exist_ok=True)
    (home.parent / "xdg" / "git" / "config").write_text(USER_GITCONFIG, encoding="utf-8")
    for rel, s in sorted(tree.items()):
        if "gitbare" in s:
            lay_out_bare(home, home / rel, s["gitbare"], t0)
    for rel in clones:
        lay_out_clone(home, home / rel, tree[rel]["gitclone"], t0)
    for rel, s in sorted(tree.items()):
        if inside(rel) and not ({"gitbare", "gitclone"} & set(s)):
            if "dir" in s:
                (home / rel).mkdir(parents=True, exist_ok=True)
            else:
                _write(home / rel, s, home, t0)


garden_cases.lay_out = lay_out


def _is_git_dir(p: Path) -> bool:
    return p.name == ".git" or (
        (p / "HEAD").is_file() and (p / "objects").is_dir() and (p / "refs").is_dir()
    )


def git_summary(home: Path, gd: Path) -> dict[str, Any]:
    """What a git directory holds, without its objects: the refs, the log
    of every ref (subject and author), the files at HEAD and, for a work
    tree, its status."""

    def g(*args: str) -> list[str]:
        p = subprocess.run(
            ["git", f"--git-dir={gd}", *args],
            cwd=gd.parent,
            env=git_env(home),
            capture_output=True,
            text=True,
            check=False,
        )
        return p.stdout.splitlines() if p.returncode == 0 else [f"(exit {p.returncode})"]

    out: dict[str, Any] = {
        # A clone's origin/HEAD is left out: whether git makes it (an empty
        # remote, say) changes from one git version to the next.
        "refs": [r for r in g("for-each-ref", "--format=%(refname)") if not r.endswith("/HEAD")],
        "log": g("log", "--all", "--format=%s|%an <%ae>|%cn <%ce>"),
        "head": g("ls-tree", "-r", "--name-only", "HEAD"),
    }
    if gd.name == ".git":
        p = subprocess.run(
            ["git", "status", "--porcelain", "--untracked-files=all"],
            cwd=gd.parent,
            env=git_env(home),
            capture_output=True,
            text=True,
            check=False,
        )
        out["status"] = p.stdout.splitlines()
        br = subprocess.run(
            ["git", "rev-parse", "--abbrev-ref", "HEAD@{upstream}"],
            cwd=gd.parent,
            env=git_env(home),
            capture_output=True,
            text=True,
            check=False,
        )
        out["upstream"] = br.stdout.strip() if br.returncode == 0 else None
    return out


def read_tree(home: Path) -> dict[str, Any]:
    """garden_cases.read_tree, with each git directory as its summary."""
    out: dict[str, Any] = {}
    if not home.exists():
        return out
    for dirpath, dirnames, filenames in os.walk(home):
        here = Path(dirpath)
        keep = []
        for d in sorted(dirnames):
            if here == home and d == ".cache":
                continue
            p = here / d
            rel = str(p.relative_to(home))
            if _is_git_dir(p):
                out[rel] = {
                    "dir": True,
                    "mode": stat.S_IMODE(os.lstat(p).st_mode),
                    "git": git_summary(home, p),
                }
                continue
            out[rel] = {"dir": True, "mode": stat.S_IMODE(os.lstat(p).st_mode)}
            keep.append(d)
        dirnames[:] = keep
        for name in sorted(filenames):
            p = here / name
            rel = str(p.relative_to(home))
            st = os.lstat(p)
            entry: dict[str, Any] = {"mode": stat.S_IMODE(st.st_mode)}
            if stat.S_ISLNK(st.st_mode):
                entry["link"] = os.readlink(p)
                out[rel] = entry
                continue
            raw = p.read_bytes()
            if raw.startswith(b"SQLite format 3\x00"):
                entry["db"] = garden_cases.dump_db(p)
            else:
                try:
                    entry["text"] = raw.decode("utf-8")
                except UnicodeDecodeError:
                    entry["hex"] = raw.hex()
            out[rel] = entry
    return out


garden_cases.read_tree = read_tree

# ---------------------------------------------------------------------------
# Normalizing
# ---------------------------------------------------------------------------

# The case being run, for normalize() (run_case does not pass it).
_CURRENT: dict[str, Any] = {}
_garden_invoke = garden_cases.invoke
_garden_normalize = garden_cases.normalize


def invoke(case, argv, env, cwd, work):
    _CURRENT.clear()
    _CURRENT.update(case)
    return _garden_invoke(case, argv, env, cwd, work)


garden_cases.invoke = invoke

# datetime.isoformat() of a UTC time with microseconds, as release sign
# writes it ("...:05.123456Z"); the fraction is dropped before the time
# is compared.
_ISOFRAC = re.compile(r"\b(20[0-9]{2}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})\.[0-9]{1,6}Z")
_VARY = {
    "sha": re.compile(r"\b[0-9a-f]{64}\b"),
    "short": re.compile(r"\bbundle [0-9a-f]{12}\b"),
    "commit": re.compile(r"\b[0-9a-f]{40}\b"),
    "sig": re.compile(r"[A-Za-z0-9+/]{86}=="),
    "key": re.compile(r"(?<![A-Za-z0-9+/])[A-Za-z0-9+/]{43}="),
    "stratum": re.compile(r"\bstratum_[0-9a-f]{8}\b"),
}
# Lines git writes to stderr itself.
_GIT_LINE = re.compile(
    r"^(Cloning into |done\.$|warning: You appear to have cloned an empty repository\.|"
    r"fatal: |remote: |To |From |hint: |Receiving objects|Resolving deltas|"
    r"Counting objects|Compressing objects|Writing objects|Total |Unpacking objects)"
)


def normalize(result: dict[str, Any], home: str, t0: float) -> dict[str, Any]:
    text = _ISOFRAC.sub(r"\1Z", json.dumps(result))
    result = _garden_normalize(json.loads(text), home, t0)
    if "stderr" in result:
        result["stderr"] = [ln for ln in result["stderr"] if not _GIT_LINE.match(ln)]
    text = json.dumps(result)
    # A value the case laid out itself (an existing bundle's hash or
    # signature) is kept as it is; only values made during the run vary.
    laid = json.dumps(_CURRENT.get("before") or {})
    for kind in _CURRENT.get("vary", []):
        seen: dict[str, str] = {}
        known = set(_VARY[kind].findall(laid))

        def rename(
            m: re.Match[str], seen: dict[str, str] = seen, kind: str = kind, known: set = known
        ) -> str:
            if m.group(0) in known:
                return m.group(0)
            if kind == "short":
                return "bundle {SHORT}"
            if m.group(0) not in seen:
                seen[m.group(0)] = "{" + kind.upper() + str(len(seen) + 1) + "}"
            return seen[m.group(0)]

        text = _VARY[kind].sub(rename, text)
    out = json.loads(text)
    if "sha" in _CURRENT.get("vary", []):
        # A file named by a hash made now sorts anywhere among the others:
        # the listings are compared as sets.
        for entry in (out.get("tree") or {}).values():
            g = entry.get("git") if isinstance(entry, dict) else None
            if g:
                for k in ("head", "status"):
                    if isinstance(g.get(k), list):
                        g[k] = sorted(g[k])
    return out


garden_cases.normalize = normalize

# ---------------------------------------------------------------------------
# The probe
# ---------------------------------------------------------------------------

PROBE = r"""
import json, sys, warnings
warnings.simplefilter("ignore")
sys.path.insert(0, sys.argv[2]) if len(sys.argv) > 2 else None
from l_probe_oracle import main
main(sys.argv[1])
"""


def cmd_for(case: dict[str, Any], binary: str | None, probe: str | None = None) -> list[str]:
    """The command a case runs: the oracle's when binary is None."""
    if case["kind"] == "probe":
        if probe is None:
            return [sys.executable, str(REPO / "clients" / "l_probe_oracle.py")]
        return [probe]
    return PY_CLI if binary is None else [binary]


def run(case: dict[str, Any], cmd: list[str], work: Path) -> dict[str, Any]:
    res = run_case(case, cmd, work)
    res.pop("requests", None)
    return res


# ---------------------------------------------------------------------------
# Case builders
# ---------------------------------------------------------------------------


def cli(name: str, argv: list[str], before=None, *, env=None, cwd="", vary=(), **kw):
    c: dict[str, Any] = {
        "kind": "cli",
        "name": name,
        "argv": argv,
        "before": before or {},
        "env": {**GIT_ENV, "XDG_CONFIG_HOME": "{HOME}/../xdg", **(env or {})},
        "no_claude": True,
    }
    if cwd:
        c["cwd"] = cwd
    if vary:
        c["vary"] = list(vary)
    c.update(kw)
    return c


def probe(name: str, query: dict[str, Any], before=None, *, vary=(), **kw):
    c: dict[str, Any] = {
        "kind": "probe",
        "name": name,
        "argv": [json.dumps(query, ensure_ascii=False)],
        "before": before or {},
        "no_claude": True,
    }
    if vary:
        c["vary"] = list(vary)
    c.update(kw)
    return c


# -- release ----------------------------------------------------------------


def manifest(files: dict[str, str], *, version="1.2.3", signer="rel", key="rel", **edit):
    """A signed manifest over artifacts {name: text}, made by the oracle's
    own functions, with a fixed created_at."""
    from opendaisugi.release import canonicalize_manifest
    from opendaisugi.signing import sign_bytes

    entries = sorted(
        (
            {
                "name": n,
                "sha256": hashlib.sha256(t.encode()).hexdigest(),
                "size": len(t.encode()),
            }
            for n, t in files.items()
        ),
        key=lambda e: e["name"],
    )
    m: dict[str, Any] = {
        "manifest_version": 1,
        "version": version,
        "created_at": "2020-02-03T04:05:06.070809Z",
        "artifacts": entries,
        "signer": signer,
        "signature": None,
    }
    if key is not None:
        m["signature"] = sign_bytes(canonicalize_manifest(m), keypair(key)[0])
    m.update(edit)
    return json.dumps(m, sort_keys=True, indent=2) + "\n"


def trust(**names: str) -> str:
    return json.dumps({n: keypair(k)[1] for n, k in names.items()}, sort_keys=True, indent=2)


def build_release_cases() -> list[dict[str, Any]]:
    cases = []
    add = cases.append
    ARTS = {"dist/a.whl": {"text": "wheel bytes\n"}, "dist/b.tar.gz": {"text": "sdist ☃ bytes\n"}}
    # keygen
    add(cli("keygen default", ["release", "keygen"], vary=("key",)))
    add(
        cli(
            "keygen out-dir name",
            ["release", "keygen", "--out-dir", "k/deep", "--name", "rk"],
            vary=("key",),
        )
    )
    add(
        cli(
            "keygen overwrites",
            ["release", "keygen", "--out-dir", "{HOME}/k"],
            {
                "k/release_signing.key": {"text": "old\n", "mode": 0o644},
                "k/release_signing.pub": {"text": "old\n"},
            },
            vary=("key",),
        )
    )
    add(
        cli(
            "keygen out-dir is a file",
            ["release", "keygen", "--out-dir", "f"],
            {"f": {"text": "x"}},
        )
    )
    add(cli("keygen extra arg", ["release", "keygen", "x"]))
    # sign
    signed = [
        "release",
        "sign",
        "dist/a.whl",
        "dist/b.tar.gz",
        "--version",
        "1.2.3",
        "--key",
        "keys/rel.key",
        "--signer",
        "rel",
    ]
    add(cli("sign two", signed, {**ARTS, **key_files("rel")}, vary=("sig",)))
    add(
        cli(
            "sign out",
            signed + ["-o", "out/m.json"],
            {**ARTS, **key_files("rel"), "out": {"dir": True}},
            vary=("sig",),
        )
    )
    add(
        cli("sign out dir missing", signed + ["--out", "nope/m.json"], {**ARTS, **key_files("rel")})
    )
    add(
        cli(
            "sign same name twice",
            [
                "release",
                "sign",
                "dist/a.whl",
                "other/a.whl",
                "--version",
                "v",
                "--key",
                "keys/rel.key",
                "--signer",
                "s",
            ],
            {**ARTS, "other/a.whl": {"text": "other\n"}, **key_files("rel")},
        )
    )
    add(
        cli(
            "sign same path twice",
            ["release", "sign", "dist/a.whl", "dist/b.tar.gz", "dist/a.whl"]
            + ["--version", "v", "--key", "keys/rel.key", "--signer", "s"],
            {**ARTS, **key_files("rel")},
        )
    )
    add(
        cli(
            "sign missing artifact",
            signed + ["dist/none.whl", "gone"],
            {**ARTS, **key_files("rel")},
        )
    )
    add(cli("sign missing key file", signed[:7] + ["keys/none.key", "--signer", "rel"], ARTS))
    for label, text in [
        ("short", base64.b64encode(b"x" * 16).decode()),
        ("long", base64.b64encode(b"x" * 64).decode()),
        ("bad padding", "abc"),
        ("one char over", "A" * 45),
        ("junk chars", "!!" + keypair("rel")[0].replace("=", "") + "!="),
        ("url alphabet", keypair("rel")[0].replace("+", "-").replace("/", "_")),
        ("inner space", keypair("rel")[0][:10] + " \t" + keypair("rel")[0][10:]),
        ("after padding", keypair("rel")[0] + "QUJD"),
        ("non-ascii", "é" + keypair("rel")[0]),
        ("empty", ""),
    ]:
        add(
            cli(
                f"sign key {label}",
                signed,
                {**ARTS, "keys/rel.key": {"text": text + "\n"}},
                vary=("sig",),
            )
        )
    add(
        cli(
            "sign no artifacts",
            ["release", "sign", "--version", "1", "--key", "k", "--signer", "s"],
        )
    )
    add(
        cli(
            "sign missing version",
            ["release", "sign", "dist/a.whl", "--key", "keys/rel.key", "--signer", "s"],
            ARTS,
        )
    )
    add(
        cli(
            "sign missing signer",
            ["release", "sign", "dist/a.whl", "--version", "1", "--key", "keys/rel.key"],
            ARTS,
        )
    )
    add(
        cli(
            "sign empty artifact",
            ["release", "sign", "e", "--version", "", "--key", "keys/rel.key", "--signer", ""],
            {"e": {"text": ""}, **key_files("rel")},
            vary=("sig",),
        )
    )
    # verify
    arts = {"a.whl": "wheel bytes\n", "b.tar.gz": "sdist ☃ bytes\n"}
    rel_files = {n: {"text": t} for n, t in arts.items()}
    reg = ".opendaisugi/trusted_signers.json"
    good = {**rel_files, "m.json": {"text": manifest(arts)}, reg: {"text": trust(rel="rel")}}
    add(cli("verify ok", ["release", "verify", "m.json"], good))
    add(cli("verify ok named signer", ["release", "verify", "m.json", "--signer", "rel"], good))
    add(
        cli(
            "verify ok second signer",
            ["release", "verify", "m.json", "--signer", "nobody", "--signer", "rel"],
            good,
        )
    )
    add(
        cli(
            "verify unknown signer only",
            ["release", "verify", "m.json", "--signer", "nobody"],
            good,
        )
    )
    add(
        cli(
            "verify artifact dir",
            ["release", "verify", "m.json", "--artifact-dir", "d"],
            {
                **{f"d/{n}": {"text": t} for n, t in arts.items()},
                "m.json": {"text": manifest(arts)},
                reg: {"text": trust(rel="rel")},
            },
        )
    )
    add(
        cli(
            "verify tampered artifact",
            ["release", "verify", "m.json"],
            {**good, "a.whl": {"text": "evil\n"}},
        )
    )
    add(
        cli(
            "verify missing artifact",
            ["release", "verify", "m.json"],
            {k: v for k, v in good.items() if k != "b.tar.gz"},
        )
    )
    add(
        cli(
            "verify wrong key",
            ["release", "verify", "m.json"],
            {**good, reg: {"text": trust(rel="other")}},
        )
    )
    add(
        cli(
            "verify untrusted and tampered",
            ["release", "verify", "m.json"],
            {**good, reg: {"text": trust(rel="other")}, "a.whl": {"text": "evil\n"}},
        )
    )
    add(
        cli(
            "verify tampered version",
            ["release", "verify", "m.json"],
            {**good, "m.json": {"text": manifest(arts).replace('"1.2.3"', '"9.9.9"')}},
        )
    )
    add(
        cli(
            "verify unsigned",
            ["release", "verify", "m.json"],
            {**good, "m.json": {"text": manifest(arts, key=None)}},
        )
    )
    add(
        cli(
            "verify no registry",
            ["release", "verify", "m.json"],
            rel_files | {"m.json": {"text": manifest(arts)}},
        )
    )
    add(
        cli("verify empty registry", ["release", "verify", "m.json"], {**good, reg: {"text": "{}"}})
    )
    add(
        cli(
            "verify registry flag",
            ["release", "verify", "m.json", "--registry", "r.json"],
            {**rel_files, "m.json": {"text": manifest(arts)}, "r.json": {"text": trust(rel="rel")}},
        )
    )
    add(
        cli(
            "verify registry not a dict",
            ["release", "verify", "m.json"],
            {**good, reg: {"text": "[1]"}},
        )
    )
    add(
        cli(
            "verify registry bad json",
            ["release", "verify", "m.json"],
            {**good, reg: {"text": "{"}},
        )
    )
    add(
        cli(
            "verify registry key not b64",
            ["release", "verify", "m.json"],
            {**good, reg: {"text": json.dumps({"rel": "@@@"})}},
        )
    )
    add(
        cli(
            "verify registry key wrong size",
            ["release", "verify", "m.json"],
            {**good, reg: {"text": json.dumps({"rel": base64.b64encode(b"k" * 31).decode()})}},
        )
    )
    add(
        cli(
            "verify registry value not str",
            ["release", "verify", "m.json", "--signer", "rel"],
            {**good, reg: {"text": json.dumps({"rel": 5})}},
        )
    )
    add(cli("verify manifest missing", ["release", "verify", "none.json"], good))
    add(
        cli(
            "verify manifest bad json",
            ["release", "verify", "m.json"],
            {**good, "m.json": {"text": "{nope"}},
        )
    )
    add(
        cli(
            "verify manifest a list",
            ["release", "verify", "m.json"],
            {**good, "m.json": {"text": "[]"}},
        )
    )
    add(
        cli(
            "verify manifest no artifacts key",
            ["release", "verify", "m.json"],
            {**good, "m.json": {"text": manifest(arts, artifacts=None)}},
        )
    )
    add(
        cli(
            "verify signature not a string",
            ["release", "verify", "m.json"],
            {**good, "m.json": {"text": manifest(arts, signature=7)}},
        )
    )
    add(cli("verify no manifest arg", ["release", "verify"]))
    return cases


# -- batch prove -----------------------------------------------------------


def _step(sid: str, kind: str, **f: Any) -> dict[str, Any]:
    return {"id": sid, "type": kind, **f}


def decl(steps, items, footprint, *, params=None, **extra) -> str:
    d = {
        "program": {"id": "plan_b0000001", "source": "script", "task": "batch", "steps": steps},
        "parameters": params or [],
        "items": items,
        "footprint": footprint,
        **extra,
    }
    return json.dumps(d)


def benv(**perm: Any) -> str:
    return envelope(7, **perm).model_dump_json()


def build_batch_cases() -> list[dict[str, Any]]:
    cases = []
    add = cases.append
    W = "{HOME}/out"
    wparam = [{"name": "p", "step_index": 0, "step_id": "w", "field": "path", "head": W}]
    wstep = [_step("w", "file_write", path=W + "/x.txt", content="hi")]
    items = [{"p": f"{W}/{n}.txt"} for n in ("a", "b", "c")]
    env_ok = benv(file_write=[W + "/**"])

    def pv(name, d, e=env_ok, before=None, argv_extra=()):
        tree = {"d.json": {"text": d}, "e.json": {"text": e}, **(before or {})}
        add(cli(name, ["batch", "prove", "d.json", "-e", "e.json", *argv_extra], tree))

    pv("prove three writes", decl(wstep, items, [W + "/*"], params=wparam))
    pv("prove long option", decl(wstep, items, [W + "/*"], params=wparam), argv_extra=())
    pv(
        "prove out of envelope",
        decl(wstep, items, [W + "/*"], params=wparam),
        benv(file_write=["/elsewhere/**"]),
    )
    pv("prove under-declared", decl(wstep, items, ["{HOME}/other/*"], params=wparam))
    pv("prove both unprovable", decl(wstep, items, [], params=wparam), benv())
    pv(
        "prove unbound hole",
        decl(wstep, [{"p": f"{W}/a.txt"}, {"q": "x"}], [W + "/*"], params=wparam),
    )
    pv(
        "prove head change",
        decl(
            wstep,
            [{"p": "{HOME}/elsewhere/a.txt"}],
            [W + "/*", "{HOME}/elsewhere/*"],
            params=wparam,
        ),
    )
    pv(
        "prove hole in a non-string field",
        decl(
            wstep,
            [{"p": "x"}],
            [W + "/*"],
            params=[
                {"name": "p", "step_index": 0, "step_id": "w", "field": "depends_on", "head": ""}
            ],
        ),
    )
    pv(
        "prove step index past the end",
        decl(wstep, [{"p": f"{W}/a.txt"}], [W + "/*"], params=[{**wparam[0], "step_index": 5}]),
    )
    pv("prove no items", decl(wstep, [], [W + "/*"], params=wparam))
    pv("prove no params fixed write", decl(wstep, [{}, {}], [W + "/*"]))
    pv(
        "prove read only",
        decl([_step("r", "file_read", path="{HOME}/in.txt")], [{}], []),
        benv(file_read=["{HOME}/**"]),
    )
    pv(
        "prove network",
        decl([_step("n", "network", url="https://example.invalid/x", method="GET")], [{}], []),
        benv(network=True, network_hosts=["example.invalid"]),
    )
    pv("prove shell refused", decl([_step("s", "shell", command="make")], [{}], []))
    pv(
        "prove shell and mcp refused",
        decl(
            [
                _step("s", "shell", command="make"),
                _step("m", "mcp", server="x", tool="t"),
                _step("w2", "file_write", path=W + "/y", content=""),
            ],
            [{}],
            [W + "/*"],
        ),
    )
    pv(
        "prove non-utf8 target",
        decl(wstep, [{}], [W + "/*"]),
        before={"out/x.txt": {"hex": "ff00fe"}},
    )
    pv(
        "prove utf8 target",
        decl(wstep, [{}], [W + "/*"]),
        before={"out/x.txt": {"text": "old ☃\n"}},
    )
    pv("prove directory target", decl(wstep, [{}], [W + "/*"]), before={"out/x.txt": {"dir": True}})
    pv(
        "prove symlink target",
        decl(wstep, [{}], [W + "/*"]),
        before={"out/real.txt": {"text": "real\n"}, "out/x.txt": {"link": "real.txt"}},
    )
    pv(
        "prove dotdot path",
        decl([_step("w", "file_write", path=W + "/../out/x.txt", content="")], [{}], [W + "/*"]),
    )
    pv(
        "prove relative path",
        decl([_step("w", "file_write", path="rel/x.txt", content="")], [{}], ["rel/*"]),
        benv(file_write=["rel/**"]),
    )
    pv(
        "prove star crosses no slash",
        decl([_step("w", "file_write", path=W + "/d/x.txt", content="")], [{}], [W + "/*"]),
    )
    pv(
        "prove duplicate writes",
        decl(wstep, [{"p": f"{W}/a.txt"}, {"p": f"{W}/a.txt"}], [W + "/*"], params=wparam),
    )
    pv(
        "prove acceptance and sample",
        decl(wstep, [{}], [W + "/*"], acceptance={"type": "file_exists"}, sample_k=0),
    )
    pv("prove bad json", "{not json")
    pv("prove missing program", json.dumps({"items": []}))
    pv("prove items not strings", decl(wstep, [{"p": 3}], [W + "/*"], params=wparam))
    pv("prove unknown step type", decl([_step("z", "teleport")], [{}], []))
    pv("prove envelope bad", decl(wstep, [{}], [W + "/*"]), "{}")
    pv("prove envelope yaml not json", decl(wstep, [{}], [W + "/*"]), "id: x\n")
    add(
        cli(
            "prove missing envelope option",
            ["batch", "prove", "d.json"],
            {"d.json": {"text": "{}"}},
        )
    )
    add(
        cli(
            "prove missing declaration file",
            ["batch", "prove", "none.json", "-e", "e.json"],
            {"e.json": {"text": env_ok}},
        )
    )
    add(
        cli(
            "prove missing envelope file",
            ["batch", "prove", "d.json", "-e", "none.json"],
            {"d.json": {"text": decl(wstep, [{}], [])}},
        )
    )
    add(cli("prove no args", ["batch", "prove"]))
    return cases


# -- registry --------------------------------------------------------------


def bundle_text(
    i: int,
    task: str,
    *,
    key: str | None = "pub1",
    publisher="team-a",
    at=1_600_000_000.25,
    tamper: dict | None = None,
    **pw,
) -> tuple[str, str]:
    """A bundle YAML file (name, text) as publish writes it, from the
    oracle's own functions, with a fixed published_at."""
    import yaml

    from opendaisugi.pathway_bundle import pathway_to_bundle

    p = pathway(i, task, [0.5, 0.0, -0.25, 1e-07], **pw)
    priv = pub = None
    if key is not None:
        priv, pub = keypair(key)
    b = pathway_to_bundle(
        p, publisher=publisher, published_at=at, private_key_b64=priv, public_key_b64=pub
    )
    d = b.model_dump(mode="json")
    for k, v in (tamper or {}).items():
        d[k] = v
    return f"pathways/{b.bundle_hash}.yaml", yaml.safe_dump(d, sort_keys=False)


def bare(*commits: dict[str, Any]) -> dict[str, Any]:
    return {"gitbare": {"commits": list(commits)}}


def commit(message: str, **files: Any) -> dict[str, Any]:
    return {"message": message, "files": files}


def local_store(*pws) -> dict[str, Any]:
    return {DB: {"db": {"rows": [row_spec(put_row(p)) for p in pws]}}}


def build_registry_cases() -> list[dict[str, Any]]:
    cases = []
    add = cases.append
    readme = commit("init", **{"README.md": "team registry\n"})
    n1, b1 = bundle_text(1, "deploy the café service")
    n2, b2 = bundle_text(2, "rotate the logs", key="pub2")
    n3, b3 = bundle_text(3, "unsigned task", key=None)
    n4, b4 = bundle_text(4, "tampered task", tamper={"publisher": "mallory"})
    n5, b5 = bundle_text(5, "wrong hash name")
    n6, b6 = bundle_text(
        6, "a long task " * 12 + "ünïcode ☃ 中文", publisher="pé", at=1_600_000_001.0
    )
    with_bundles = commit(
        "bundles",
        **{
            n1: b1,
            n2: b2,
            n3: b3,
            n4: b4,
            "pathways/zz-bad.yaml": "::: not yaml [",
            "pathways/zz-list.yaml": "- 1\n",
            "pathways/notes.txt": "x",
            n5.replace(n5[9:20], "00000000000"): b5,
            n6: b6,
        },
    )
    remote = {"remote.git": bare(readme, with_bundles)}
    trusted = {f"{REG}/.cache/trusted-signers.json": {"text": trust(a="pub1", b="pub2")}}
    only1 = {f"{REG}/.cache/trusted-signers.json": {"text": trust(a="pub1")}}
    clone = {REG: {"gitclone": {"of": "remote.git"}}}

    # init
    add(cli("init clones", ["registry", "init", "{HOME}/remote.git"], {"remote.git": bare(readme)}))
    add(cli("init empty remote", ["registry", "init", "{HOME}/remote.git"], {"remote.git": bare()}))
    add(
        cli(
            "init clone-to",
            ["registry", "init", "{HOME}/remote.git", "--clone-to", "deep/reg"],
            remote,
        )
    )
    add(cli("init already cloned", ["registry", "init", "{HOME}/remote.git"], {**remote, **clone}))
    add(
        cli(
            "init dir without git",
            ["registry", "init", "{HOME}/nope.git"],
            {f"{REG}/x": {"text": "x"}},
        )
    )
    add(cli("init ext refused", ["registry", "init", "ext::sh -c touch% {HOME}/pwned"]))
    add(cli("init fd refused", ["registry", "init", "fd::3"]))
    add(cli("init EXT upper", ["registry", "init", "EXT::sh -c true"]))
    add(cli("init missing remote", ["registry", "init", "{HOME}/nope.git"]))
    add(
        cli(
            "init file url",
            ["registry", "init", "file://{HOME}/remote.git", "--clone-to", "c"],
            remote,
        )
    )
    add(cli("init no url", ["registry", "init"]))
    # status
    add(cli("status plain", ["registry", "status"], {**remote, **clone}))
    add(cli("status json", ["registry", "status", "--json"], {**remote, **clone, **trusted}))
    add(
        cli(
            "status trusted only one",
            ["registry", "status", "--json"],
            {**remote, **clone, **only1},
        )
    )
    add(
        cli(
            "status relative path",
            ["registry", "status", "--repo-path", REG],
            {**remote, **clone, **trusted},
        )
    )
    add(
        cli(
            "status empty clone",
            ["registry", "status", "--json"],
            {"remote.git": bare(), REG: {"gitclone": {"of": "remote.git"}}},
        )
    )
    add(cli("status no repo", ["registry", "status"]))
    add(
        cli(
            "status not git",
            ["registry", "status", "--repo-path", "plain"],
            {"plain/x": {"text": "x"}},
        )
    )
    add(
        cli(
            "status in-repo trust only",
            ["registry", "status", "--json"],
            {
                "remote.git": bare(
                    readme,
                    commit("trust", **{"trusted-signers.json": trust(a="pub1")}),
                    with_bundles,
                ),
                **clone,
            },
        )
    )
    add(
        cli(
            "status trust file bad json",
            ["registry", "status", "--json"],
            {**remote, **clone, f"{REG}/.cache/trusted-signers.json": {"text": "{"}},
        )
    )
    add(
        cli(
            "status trust file a list",
            ["registry", "status", "--json"],
            {**remote, **clone, f"{REG}/.cache/trusted-signers.json": {"text": '["x"]'}},
        )
    )
    add(
        cli(
            "status trust values mixed",
            ["registry", "status", "--json"],
            {
                **remote,
                **clone,
                f"{REG}/.cache/trusted-signers.json": {
                    "text": json.dumps({"a": keypair("pub1")[1], "b": 3, "c": None})
                },
            },
        )
    )
    add(
        cli(
            "status cache exists",
            ["registry", "status", "--json"],
            {
                **remote,
                **clone,
                **trusted,
                f"{REG}/.cache/pathways.db": {
                    "db": {"rows": [row_spec(put_row(pathway(9, "cached one", [1.0])))]}
                },
            },
        )
    )
    # pull
    add(cli("pull untrusted", ["registry", "pull"], {**remote, **clone}))
    add(
        cli(
            "pull trusted",
            ["registry", "pull"],
            {**remote, REG: {"gitclone": {"of": "remote.git"}}, **trusted},
        )
    )
    add(
        cli(
            "pull fetches bundles",
            ["registry", "pull"],
            {
                "remote.git": bare(readme),
                "seed": {"gitclone": {"of": "remote.git", "commits": [with_bundles], "push": True}},
                REG: {"gitclone": {"of": "remote.git"}},
                **trusted,
            },
        )
    )
    add(
        cli(
            "pull allow unsigned",
            ["registry", "pull", "--allow-unsigned"],
            {**remote, **clone, **trusted},
        )
    )
    add(
        cli(
            "pull require signed flag",
            ["registry", "pull", "--require-signed"],
            {**remote, **clone, **only1},
        )
    )
    add(
        cli(
            "pull remote gone",
            ["registry", "pull"],
            {
                **remote,
                REG: {"gitclone": {"of": "remote.git", "origin": "{HOME}/gone.git"}},
                **trusted,
            },
        )
    )
    add(
        cli(
            "pull diverged",
            ["registry", "pull"],
            {
                "remote.git": bare(readme, with_bundles),
                REG: {
                    "gitclone": {
                        "of": "remote.git",
                        "commits": [commit("local", **{"local.txt": "x"})],
                    }
                },
                **trusted,
            },
        )
    )
    add(cli("pull no repo", ["registry", "pull"]))
    add(
        cli(
            "pull repo path",
            ["registry", "pull", "--repo-path", "r"],
            {
                **remote,
                "r": {"gitclone": {"of": "remote.git"}},
                "r/.cache/trusted-signers.json": {"text": trust(a="pub1")},
            },
        )
    )
    add(
        cli(
            "pull empty remote",
            ["registry", "pull"],
            {"remote.git": bare(), REG: {"gitclone": {"of": "remote.git"}}},
        )
    )
    add(
        cli(
            "pull cache has the id",
            ["registry", "pull"],
            {
                **remote,
                **clone,
                **trusted,
                f"{REG}/.cache/pathways.db": {
                    "db": {"rows": [row_spec(put_row(pathway(1, "older copy", [1.0])))]}
                },
            },
        )
    )
    # publish
    two = local_store(
        pathway(1, "deploy the café service", [0.5, 0.0]),
        pathway(2, "already signed", [1.0], structure_signature="x→y"),
    )
    pub_tree = {**remote, **clone, **two, **key_files("pub1")}
    PUB = [
        "registry",
        "publish",
        "pw_0001",
        "--private-key",
        "keys/pub1.key",
        "--public-key",
        "keys/pub1.pub",
    ]
    add(cli("publish pushes", PUB, pub_tree, vary=("sha", "short", "commit", "sig")))
    add(
        cli(
            "publish no push", PUB + ["--no-push"], pub_tree, vary=("sha", "short", "commit", "sig")
        )
    )
    add(
        cli(
            "publish publisher",
            PUB + ["--publisher", "héllo team", "--push"],
            pub_tree,
            vary=("sha", "short", "commit", "sig"),
        )
    )
    add(
        cli(
            "publish structure signature kept",
            ["registry", "publish", "pw_0002", *PUB[3:]],
            pub_tree,
            vary=("sha", "short", "commit", "sig"),
        )
    )
    add(
        cli(
            "publish to empty remote",
            PUB,
            {"remote.git": bare(), **clone, **two, **key_files("pub1")},
            vary=("sha", "short", "commit", "sig"),
        )
    )
    add(
        cli(
            "publish push fails",
            PUB,
            {
                **remote,
                REG: {"gitclone": {"of": "remote.git", "origin": "{HOME}/gone.git"}},
                **two,
                **key_files("pub1"),
            },
            vary=("sha", "short", "commit", "sig"),
        )
    )
    add(cli("publish unknown id", ["registry", "publish", "pw_9999", *PUB[3:]], pub_tree))
    add(cli("publish no local store", PUB, {**remote, **clone, **key_files("pub1")}))
    add(
        cli(
            "publish data dir",
            PUB + ["--data-dir", "dd"],
            {**remote, **clone, **key_files("pub1"), "dd/pathways.db": two[DB]},
            vary=("sha", "short", "commit", "sig"),
        )
    )
    add(cli("publish no repo", PUB, {**two, **key_files("pub1")}))
    add(cli("publish bad private key", PUB, {**pub_tree, "keys/pub1.key": {"text": "abc\n"}}))
    add(
        cli(
            "publish short private key",
            PUB,
            {**pub_tree, "keys/pub1.key": {"text": base64.b64encode(b"k" * 31).decode() + "\n"}},
        )
    )
    add(cli("publish key file missing", PUB, {**remote, **clone, **two}))
    add(cli("publish missing options", ["registry", "publish", "pw_0001"], pub_tree))
    add(cli("publish no id", ["registry", "publish"]))
    add(
        cli(
            "publish public key mismatched",
            PUB[:5] + ["--public-key", "keys/pub2.pub"],
            {**pub_tree, **key_files("pub2")},
            vary=("sha", "short", "commit", "sig"),
        )
    )
    # An inherited GIT_* variable reaches no git the registry runs: the
    # decoy repository and its hooks are left alone, and the clone is used.
    decoy = {
        "decoy.git": bare(commit("decoy", **{"d.txt": "decoy\n"})),
        "decoy": {"gitclone": {"of": "decoy.git"}},
        "hooks/pre-commit": {"text": "#!/bin/sh\ntouch {HOME}/pwned\n", "mode": 0o755},
        "hooks/post-checkout": {"text": "#!/bin/sh\ntouch {HOME}/pwned\n", "mode": 0o755},
    }
    hostile = {
        "GIT_DIR": "{HOME}/decoy/.git",
        "GIT_WORK_TREE": "{HOME}/decoy",
        "GIT_INDEX_FILE": "{HOME}/decoy-index",
        "GIT_CONFIG_COUNT": "1",
        "GIT_CONFIG_KEY_0": "core.hooksPath",
        "GIT_CONFIG_VALUE_0": "{HOME}/hooks",
        "GIT_SSH_COMMAND": "touch {HOME}/pwned-ssh",
        "GIT_AUTHOR_NAME": "Mallory",
    }
    add(
        cli(
            "status hostile git env",
            ["registry", "status", "--json"],
            {**remote, **clone, **trusted, **decoy},
            env=hostile,
        )
    )
    add(
        cli(
            "pull hostile git env",
            ["registry", "pull"],
            {
                "remote.git": bare(readme),
                "seed": {"gitclone": {"of": "remote.git", "commits": [with_bundles], "push": True}},
                REG: {"gitclone": {"of": "remote.git"}},
                **trusted,
                **decoy,
            },
            env=hostile,
        )
    )
    add(
        cli(
            "publish hostile git env",
            PUB,
            {**pub_tree, **decoy},
            env=hostile,
            vary=("sha", "short", "commit", "sig"),
        )
    )
    add(
        cli(
            "init hostile git env",
            ["registry", "init", "{HOME}/remote.git"],
            {**remote, **decoy},
            env=hostile,
        )
    )
    # A repo path with no .git of its own inside another repository: git
    # finds no repository there and never climbs to the one above.
    outer = {"outer": {"gitclone": {"of": "remote.git"}}, "outer/reg/x": {"text": "x\n"}}
    add(
        cli(
            "status inside another repo",
            ["registry", "status", "--json", "--repo-path", "outer/reg"],
            {**remote, **outer},
        )
    )
    add(
        cli(
            "publish inside another repo",
            PUB + ["--repo-path", "outer/reg"],
            {**remote, **outer, **two, **key_files("pub1")},
            vary=("sha", "short", "sig"),
        )
    )
    # pull-and-tend
    add(cli("pull and tend", ["registry", "pull-and-tend"], {**remote, **clone, **trusted, **LEX}))
    add(
        cli(
            "pull and tend data dir",
            ["registry", "pull-and-tend", "--data-dir", "dd"],
            {**remote, **clone, **trusted, **LEX},
        )
    )
    add(cli("pull and tend no repo", ["registry", "pull-and-tend"], LEX))
    return cases


def all_cases() -> list[dict[str, Any]]:
    try:
        from l_probe_cases import build_probe_cases
    except ImportError:

        def build_probe_cases() -> list[dict[str, Any]]:
            return []

    cases = (
        build_release_cases() + build_batch_cases() + build_registry_cases() + build_probe_cases()
    )
    names = [c["name"] for c in cases]
    dup = {n for n in names if names.count(n) > 1}
    if dup:
        raise SystemExit(f"duplicate case names: {sorted(dup)}")
    return cases


def kid(c: dict[str, Any]) -> str:
    return garden_cases.body_id(c)


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--out", type=Path, default=FIXTURE_DIR)
    ap.add_argument(
        "--only", help="run only cases whose name holds this text; print, write nothing"
    )
    ap.add_argument("--fresh", action="store_true", help="rerun every case")
    args = ap.parse_args()
    out: Path = args.out
    out.mkdir(parents=True, exist_ok=True)
    SCRATCH.mkdir(parents=True, exist_ok=True)
    cases = all_cases()
    old: dict[str, dict[str, Any]] = {}
    if (out / "cases.jsonl").exists() and not args.fresh:
        for ln in (out / "cases.jsonl").read_text(encoding="utf-8").splitlines():
            if ln.strip():
                c = json.loads(ln)
                old[kid(c)] = c
    for i, c in enumerate(cases):
        if args.only and args.only not in c["name"]:
            continue
        prev = old.get(kid(c))
        if prev is not None and not args.only:
            c["expect"] = prev["expect"]
        else:
            c["expect"] = run(c, cmd_for(c, None), SCRATCH / "gen" / f"{i:04d}")
        if args.only:
            print(
                json.dumps({"name": c["name"], **c["expect"]}, indent=1, ensure_ascii=False)[:12000]
            )
        else:
            print(f"{i + 1:4d}/{len(cases)} exit={c['expect']['exit']} {c['name']}", flush=True)
    if args.only:
        return 0
    manifest_ = {"v": CASE_VERSION}
    manifest_["cases.jsonl"] = write_jsonl(out / "cases.jsonl", cases)
    (out / "manifest.json").write_text(
        json.dumps(manifest_, indent=1, sort_keys=True) + "\n", encoding="utf-8"
    )
    shutil.rmtree(SCRATCH / "gen", ignore_errors=True)
    from fixture_paths import leaks

    leaked = leaks()
    if leaked:
        raise SystemExit("a machine path reached the fixtures:\n" + "\n".join(leaked[:10]))
    print(f"wrote {len(cases)} cases to {out}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
