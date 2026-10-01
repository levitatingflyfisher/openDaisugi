"""`daisugi pack list | install | remove | status | bundle | run`.

A pack lives in DATA/packs/NAME/:

- python/      the pinned CPython, unpacked from its tarball
- venv/        a virtual environment made with that Python
- lock.txt     the pack's lock, as pip read it
- worker/      the worker and the files its jobs run, and pack.json
- manifest.json, written last: a pack without it is not installed

A system pack (SYSTEM_PACKS/NAME, laid out by scripts/system-pack.py for
a distribution package such as the AUR daisugi-ml) has the same worker/
and manifest.json, with the system's Python as venv/bin/python and no
pins of its own. A pack in the data dir comes first.

Every file is checked against a pin before it is used: the tarball
against the catalog's sha256, each wheel against the lock (pip
--require-hashes, and before pip for a bundle). A mismatch stops the
install, names the file, and removes the pack directory. The Python,
Go and Rust binaries print the same lines.
"""

from __future__ import annotations

import hashlib
import io
import json
import os
import platform
import shutil
import subprocess
import sys
import tarfile
import urllib.error
import urllib.request
from collections.abc import Callable
from dataclasses import dataclass
from pathlib import Path

from opendaisugi.pack import catalog, client, ustar
from opendaisugi.pack.worker import PROTOCOL

_SRC = Path(__file__).resolve().parent
WORKER_SOURCES = {
    client.WORKER_FILE: _SRC / "worker.py",
    "lora_train.py": _SRC.parent / "lora" / "train.py",
    "vla_oracle.py": _SRC / "vla_oracle.py",
}
PIP_FLAGS = [
    "--isolated",
    "--disable-pip-version-check",
    "--no-input",
    "--no-cache-dir",
    "--quiet",
    "--require-hashes",
    "--only-binary=:all:",
]


SYSTEM_PACKS = Path("/usr/lib/opendaisugi/packs")
SYSTEM_ENV = "OPENDAISUGI_SYSTEM_PACKS"


@dataclass
class Ctx:
    data_dir: Path
    cat: dict
    out: Callable[[str], None]
    err: Callable[[str], None]
    env: dict[str, str] | None = None
    system_dir: Path | None = None


class Stop(Exception):
    """An install step that failed: the lines to print, then exit 1."""

    def __init__(self, *lines: str) -> None:
        super().__init__(lines[0] if lines else "")
        self.lines = list(lines)


def pack_dir(ctx: Ctx, name: str) -> Path:
    return Path(ctx.data_dir) / "packs" / name


def sha256_bytes(b: bytes) -> str:
    return hashlib.sha256(b).hexdigest()


def sha256_file(p: Path) -> str:
    h = hashlib.sha256()
    with open(p, "rb") as f:
        while chunk := f.read(1 << 20):
            h.update(chunk)
    return h.hexdigest()


def this_platform() -> str:
    machine = {"amd64": "x86_64"}.get(platform.machine().lower(), platform.machine().lower())
    return f"{machine}-{sys.platform}"


def read_manifest(d: Path) -> dict | None:
    try:
        m = json.loads((d / "manifest.json").read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return None
    return m if isinstance(m, dict) else None


def find_pack(ctx: Ctx, name: str) -> tuple[Path, bool] | None:
    """(directory, from the system) of the pack a job runs in: the data
    dir's, else the system's, else None."""
    d = pack_dir(ctx, name)
    if read_manifest(d) is not None:
        return d, False
    if ctx.system_dir is not None:
        sd = Path(ctx.system_dir) / name
        if read_manifest(sd) is not None:
            return sd, True
    return None


def _lookup(ctx: Ctx, name: str, *, build: bool) -> dict | None:
    """The pack, or None after printing why (the caller exits with the
    code in ctx-less form: 2 unknown, 1 otherwise)."""
    p = catalog.find(ctx.cat, name)
    if p is None:
        ctx.err(f"No pack named {name}.")
        ctx.err("See: daisugi pack list")
        return None
    if build and p["gpu"]:
        ctx.err(f"The pack {name} needs a GPU build, which this release does not carry.")
        return None
    if build and this_platform() != ctx.cat["platform"]:
        ctx.err(f"The packs of this release are for {ctx.cat['platform']} only.")
        return None
    return p


def _unknown_code(ctx: Ctx, name: str) -> int:
    return 2 if catalog.find(ctx.cat, name) is None else 1


# ---------------------------------------------------------------------------
# list, status, remove
# ---------------------------------------------------------------------------


def list_packs(ctx: Ctx) -> int:
    for p in ctx.cat["packs"]:
        found = None if p["gpu"] else find_pack(ctx, p["name"])
        if p["gpu"]:
            state = "needs a GPU"
        elif found is None:
            state = "not installed"
        else:
            state = "system" if found[1] else "installed"
        ctx.out(f"{p['name']:<14}{state:<15}{p['summary']}")
    ctx.out("")
    ctx.out("Install one: daisugi pack install NAME")
    return 0


def problems(ctx: Ctx, p: dict, d: Path, m: dict, system: bool = False) -> list[str]:
    """What is wrong with an installed pack, in words. A system pack has
    no pins of its own to check."""
    found = []
    if not os.path.exists(d / "venv" / "bin" / "python"):
        found.append("the virtual environment has no python")
    for fname, want in (m.get("worker") or {}).items():
        f = d / "worker" / fname
        if not f.is_file() or sha256_file(f) != want:
            found.append(f"the worker file {fname} was changed")
    if m.get("protocol") != PROTOCOL:
        found.append(f"the worker speaks {m.get('protocol')}, not {PROTOCOL}")
    if system:
        return found
    if m.get("lock_sha256") != sha256_bytes(catalog.lock_text(ctx.cat, p).encode("utf-8")):
        found.append("this binary pins another lock")
    if (m.get("python") or {}).get("sha256") != ctx.cat["python"]["sha256"]:
        found.append("this binary pins another Python")
    return found


def status(ctx: Ctx, name: str | None) -> int:
    if name is not None:
        p = _lookup(ctx, name, build=False)
        if p is None:
            return 2
        names = [name]
    else:
        names = [p["name"] for p in ctx.cat["packs"] if not p["gpu"] and find_pack(ctx, p["name"])]
        if not names:
            ctx.out("No pack is installed. See: daisugi pack list")
            return 0
    bad = False
    for n in names:
        p = catalog.find(ctx.cat, n)
        found = None if p["gpu"] else find_pack(ctx, n)
        if found is None:
            ctx.out(f"{n}: not installed")
            bad = True
            continue
        d, system = found
        m = read_manifest(d)
        if system:
            ctx.out(f"{n}: provided by the system in {d}")
            found_problems = problems(ctx, p, d, m, system=True)
            for pr in found_problems:
                ctx.out(f"  problem: {pr}")
            if found_problems:
                ctx.out("  fix: reinstall the system package that provides it")
                bad = True
            else:
                ctx.out("  ok")
            continue
        ctx.out(f"{n}: installed in {d}")
        ctx.out(f"  python {m['python']['version']} ({m['python']['file']})")
        ctx.out(
            f"  {len(m['packages'])} packages from {m['lock']} (sha256 {m['lock_sha256'][:12]})"
        )
        found = problems(ctx, p, d, m)
        for pr in found:
            ctx.out(f"  problem: {pr}")
        if found:
            ctx.out(f"  fix: daisugi pack install {n} --force")
            bad = True
        else:
            ctx.out("  ok")
    return 1 if bad else 0


def remove(ctx: Ctx, name: str) -> int:
    if catalog.find(ctx.cat, name) is None:
        _lookup(ctx, name, build=False)
        return 2
    d = pack_dir(ctx, name)
    if not os.path.lexists(d):
        ctx.out(f"Nothing to remove: the pack {name} is not installed.")
        return 0
    shutil.rmtree(d)
    ctx.out(f"Removed the pack {name} ({d}).")
    return 0


# ---------------------------------------------------------------------------
# The steps install and bundle share
# ---------------------------------------------------------------------------


def fetch(url: str, size: int) -> bytes:
    """The body of url, at most size bytes, or Stop."""
    try:
        with urllib.request.urlopen(url, timeout=60) as r:
            body = r.read(size + 1)
    except urllib.error.HTTPError as e:
        raise Stop(f"Could not fetch {url}: HTTP {e.code}.") from None
    except (urllib.error.URLError, OSError, ValueError):
        raise Stop(f"Could not fetch {url}: no answer.") from None
    if len(body) > size:
        raise Stop(f"Could not fetch {url}: larger than its pinned size of {size} bytes.")
    return body


def check_hash(label: str, got: str, want: str) -> None:
    if got != want:
        raise Stop(
            f"Hash mismatch: {label} has sha256 {got}, the pin is {want}.",
            "Nothing was installed.",
        )


def python_tarball(ctx: Ctx, src: Path | None, ctx_label: str | None) -> bytes:
    """The pinned CPython tarball, from a bundle directory or its URL,
    checked against its pin."""
    py = ctx.cat["python"]
    if src is not None:
        f = src / py["file"]
        if not f.is_file():
            raise Stop(f"The bundle {ctx_label} has no {py['file']}.")
        data = f.read_bytes()
    else:
        ctx.out(f"Fetching {py['file']} ...")
        data = fetch(py["url"], int(py["size"]))
    check_hash(py["file"], sha256_bytes(data), py["sha256"])
    ctx.out(f"Checked {py['file']} (sha256 {py['sha256'][:12]}).")
    return data


def unpack_python(ctx: Ctx, data: bytes, dest: Path) -> None:
    """Unpack the tarball under dest. Every entry is a file, a directory or
    a symlink under python/; a symlink stays inside dest."""
    py = ctx.cat["python"]
    root = dest.resolve()
    try:
        tf = tarfile.open(fileobj=io.BytesIO(data), mode="r:gz")
        members = tf.getmembers()
    except (tarfile.TarError, OSError, EOFError):
        raise Stop(f"{py['file']} is not a gzip tar archive.") from None
    for m in members:
        name = m.name
        parts = name.split("/")
        if name.startswith("/") or ".." in parts or parts[0] != "python":
            raise Stop(f"{py['file']} holds an entry outside python/: {name}")
        target = dest / name
        if m.isdir():
            target.mkdir(parents=True, exist_ok=True)
        elif m.isfile():
            target.parent.mkdir(parents=True, exist_ok=True)
            with tf.extractfile(m) as f, open(target, "wb") as out:
                shutil.copyfileobj(f, out)
            os.chmod(target, 0o755 if m.mode & 0o111 else 0o644)
        elif m.issym():
            link = m.linkname
            where = os.path.normpath(os.path.join(root, os.path.dirname(name), link))
            if link.startswith("/") or not (where + "/").startswith(str(root) + "/"):
                raise Stop(f"{py['file']} holds a link outside the pack: {name}")
            target.parent.mkdir(parents=True, exist_ok=True)
            if os.path.lexists(target):
                os.remove(target)
            os.symlink(link, target)
        else:
            raise Stop(f"{py['file']} holds an entry that is not a file or a link: {name}")
    if not os.path.isfile(dest / "python" / "bin" / "python3"):
        raise Stop(f"{py['file']} has no python/bin/python3.")
    ctx.out(f"Unpacked Python {py['version']}.")


def check_wheels(reqs: list[catalog.Req], wheels: Path) -> None:
    """Each lock line has a wheel in the bundle, and every such wheel has a
    pinned hash."""
    files = sorted(os.listdir(wheels)) if wheels.is_dir() else []
    keyed: dict[tuple[str, str], list[str]] = {}
    for f in files:
        k = catalog.wheel_key(f)
        if k is not None:
            keyed.setdefault(k, []).append(f)
    for r in reqs:
        found = keyed.get((r.name, r.version), [])
        if not found:
            raise Stop(f"The bundle has no wheel for {r.name}=={r.version}.")
        for f in found:
            got = sha256_file(wheels / f)
            if got not in r.hashes:
                raise Stop(
                    f"Hash mismatch: wheels/{f} has sha256 {got}, which the lock does not pin.",
                    "Nothing was installed.",
                )


def index_flags(p: dict) -> list[str]:
    flags = ["--index-url", p["index_url"]]
    for e in p.get("extra_index_urls") or []:
        flags += ["--extra-index-url", e]
    return flags


def run_quiet(argv: list[str], env, what: str) -> None:
    """Run a step; on failure Stop with its last error lines."""
    try:
        r = subprocess.run(argv, capture_output=True, env=env, stdin=subprocess.DEVNULL)
    except OSError:
        raise Stop(f"{what}: cannot start {argv[0]}.") from None
    if r.returncode != 0:
        tail = [
            ln
            for ln in (r.stderr + r.stdout).decode("utf-8", errors="replace").splitlines()
            if ln.strip()
        ][-5:]
        raise Stop(f"{what} (exit {r.returncode}):", *[f"  {ln}" for ln in tail])


def _open_bundle(offline: str, scratch: Path) -> Path:
    """The bundle as a directory: offline itself, or its .tar unpacked
    into scratch."""
    src = Path(offline)
    if src.is_dir():
        return src
    if src.is_file():
        try:
            ustar.extract(src, scratch)
        except ValueError as e:
            raise Stop(f"The bundle {offline} is not readable: {e}.") from None
        return scratch
    raise Stop(f"The bundle {offline} is not a directory or a .tar file.")


# ---------------------------------------------------------------------------
# install
# ---------------------------------------------------------------------------


def install(ctx: Ctx, name: str, offline: str | None, force: bool = False) -> int:
    p = _lookup(ctx, name, build=True)
    if p is None:
        return _unknown_code(ctx, name)
    lock = catalog.lock_text(ctx.cat, p)
    lock_sha = sha256_bytes(lock.encode("utf-8"))
    d = pack_dir(ctx, name)
    m = read_manifest(d)
    if m is not None and not force:
        if m.get("lock_sha256") == lock_sha and not problems(ctx, p, d, m):
            ctx.out(f"The pack {name} is already installed in {d}.")
            return 0
        ctx.out(f"The pack {name} is installed from another pin; installing it again.")
    try:
        reqs = catalog.parse_lock(lock)
    except catalog.LockError as e:
        ctx.err(f"The lock {p['lock']} is not usable: {e}.")
        return 1
    if os.path.lexists(d):
        shutil.rmtree(d)
    d.mkdir(parents=True)
    try:
        _install_steps(ctx, p, d, lock, lock_sha, reqs, offline)
    except Stop as s:
        for ln in s.lines:
            ctx.err(ln)
        shutil.rmtree(d, ignore_errors=True)
        return 1
    ctx.out(f"Installed the pack {name}. Run a job: daisugi pack run {name} JOB")
    return 0


def _install_steps(ctx, p, d, lock, lock_sha, reqs, offline) -> None:
    name = p["name"]
    py = ctx.cat["python"]
    ctx.out(f"Installing the pack {name} in {d}.")
    src = _open_bundle(offline, d / "bundle") if offline is not None else None
    data = python_tarball(ctx, src, offline)
    unpack_python(ctx, data, d)
    del data
    run_quiet(
        [str(d / "python" / "bin" / "python3"), "-m", "venv", str(d / "venv")],
        ctx.env,
        "Could not make the virtual environment",
    )
    ctx.out("Made the virtual environment.")
    (d / "lock.txt").write_text(lock, encoding="utf-8")
    pip = [str(d / "venv" / "bin" / "python"), "-m", "pip", "install", *PIP_FLAGS]
    pip += ["-r", str(d / "lock.txt")]
    if src is not None:
        check_wheels(reqs, src / "wheels")
        pip += ["--no-index", "--find-links", str(src / "wheels")]
    else:
        pip += index_flags(p)
    ctx.out(f"Installing {len(reqs)} packages from {p['lock']} ...")
    run_quiet(pip, ctx.env, "pip could not install the lock")
    ctx.out(f"Installed {len(reqs)} packages.")
    if src is not None and src == d / "bundle":
        shutil.rmtree(src)
    w = d / "worker"
    w.mkdir()
    shas = {}
    for fname, source in WORKER_SOURCES.items():
        body = source.read_bytes()
        (w / fname).write_bytes(body)
        shas[fname] = sha256_bytes(body)
    info = (json.dumps({"name": name, "check": p.get("check") or []}, indent=2) + "\n").encode()
    (w / "pack.json").write_bytes(info)
    shas["pack.json"] = sha256_bytes(info)
    got = client.run_job(d, name, "selftest", [], lambda s: None, env=ctx.env)
    if got.code != 0:
        raise Stop(f"The self-test failed: {got.error}")
    imports = got.result.get("imports") or {}
    ctx.out("Self-test: " + ", ".join(f"{k} {v}".strip() for k, v in imports.items()) + ".")
    manifest = {
        "pack": name,
        "protocol": PROTOCOL,
        "python": {"version": py["version"], "file": py["file"], "sha256": py["sha256"]},
        "lock": p["lock"],
        "lock_sha256": lock_sha,
        "packages": [f"{r.name}=={r.version}" for r in reqs],
        "source": "bundle" if src is not None else "index",
        "worker": shas,
    }
    (d / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")


# ---------------------------------------------------------------------------
# bundle
# ---------------------------------------------------------------------------


def bundle(ctx: Ctx, name: str, out: str) -> int:
    p = _lookup(ctx, name, build=True)
    if p is None:
        return _unknown_code(ctx, name)
    try:
        reqs = catalog.parse_lock(catalog.lock_text(ctx.cat, p))
    except catalog.LockError as e:
        ctx.err(f"The lock {p['lock']} is not usable: {e}.")
        return 1
    target = Path(out)
    as_tar = target.name.endswith(".tar")
    work = target.parent / (target.name + ".partial") if as_tar else target
    if os.path.lexists(target) or (as_tar and os.path.lexists(work)):
        ctx.err(f"{out} already exists. Give a new path.")
        return 1
    work.mkdir(parents=True)
    try:
        _bundle_steps(ctx, p, reqs, work)
        if as_tar:
            files = [(ctx.cat["python"]["file"], work / ctx.cat["python"]["file"])]
            files += [("wheels/" + f, work / "wheels" / f) for f in os.listdir(work / "wheels")]
            try:
                ustar.write_file(target, files)
            except ValueError as e:
                raise Stop(f"Could not write {out}: {e}.") from None
            shutil.rmtree(work)
    except Stop as s:
        for ln in s.lines:
            ctx.err(ln)
        shutil.rmtree(work, ignore_errors=True)
        if as_tar and os.path.lexists(target):
            os.remove(target)
        return 1
    ctx.out(
        f"Bundled the pack {name} in {out}: Python {ctx.cat['python']['version']} "
        f"and {len(reqs)} wheels."
    )
    ctx.out(f"Install it with no network: daisugi pack install {name} --offline {out}")
    return 0


def _bundle_steps(ctx, p, reqs, work: Path) -> None:
    py = ctx.cat["python"]
    data = python_tarball(ctx, None, None)
    (work / py["file"]).write_bytes(data)
    tmp = work / ".python"
    tmp.mkdir()
    unpack_python(ctx, data, tmp)
    del data
    (tmp / "lock.txt").write_text(catalog.lock_text(ctx.cat, p), encoding="utf-8")
    pip = [str(tmp / "python" / "bin" / "python3"), "-m", "pip", "download", *PIP_FLAGS]
    pip += ["--no-deps", "-d", str(work / "wheels"), "-r", str(tmp / "lock.txt"), *index_flags(p)]
    ctx.out(f"Fetching {len(reqs)} wheels from {p['lock']} ...")
    run_quiet(pip, ctx.env, "pip could not fetch the lock's wheels")
    shutil.rmtree(tmp)
    check_wheels(reqs, work / "wheels")
    ctx.out(f"Checked {len(reqs)} wheels against {p['lock']}.")


# ---------------------------------------------------------------------------
# run, and the trainer through the train pack
# ---------------------------------------------------------------------------


def _installed_or_say(ctx: Ctx, name: str) -> Path | None:
    found = find_pack(ctx, name)
    if found is None:
        ctx.err(f"The pack {name} is not installed.")
        ctx.err(f"Install it: daisugi pack install {name}")
        return None
    return found[0]


def run(ctx: Ctx, name: str, job: str, args: list[str]) -> int:
    """One job in the pack's worker: progress on stderr, the result as
    JSON on stdout."""
    p = _lookup(ctx, name, build=False)
    if p is None:
        return 2
    d = _installed_or_say(ctx, name)
    if d is None:
        return 1
    got = client.run_job(d, name, job, args, ctx.err, env=ctx.env)
    if got.code != 0:
        ctx.err(got.error)
        return got.code
    ctx.out(json.dumps(got.result, indent=2))
    return 0


TRAIN_PACK = "train"


def lora_train(ctx: Ctx, args: list[str]) -> int:
    """The trainer's arguments, run by the train pack's train job."""
    d = _installed_or_say(ctx, TRAIN_PACK)
    if d is None:
        return 1
    got = client.run_job(d, TRAIN_PACK, "train", args, ctx.err, env=ctx.env)
    if got.code != 0:
        ctx.err(got.error)
        return got.code
    ctx.out(f"The adapter is in {got.result.get('adapter')}.")
    return 0
