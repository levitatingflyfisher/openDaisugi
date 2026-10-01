"""The ML pack: the catalog, the lock, the bundle tar, the worker protocol
and the caller (opendaisugi.pack)."""

from __future__ import annotations

import hashlib
import io
import json
import os
import struct
import subprocess
import sys
import tarfile
from pathlib import Path

import pytest

from opendaisugi.pack import catalog, client, ustar, worker

REPO = Path(__file__).resolve().parent.parent
WORKER = REPO / "src" / "opendaisugi" / "pack" / "worker.py"
SYSPY = "/usr/bin/python3"


# ---------------------------------------------------------------------------
# The catalog and the lock
# ---------------------------------------------------------------------------


def test_default_catalog_names_the_packs():
    cat = catalog.load()
    names = [p["name"] for p in cat["packs"]]
    assert names == ["train", "train-cuda", "vla-ref", "vla-ref-cuda"]
    assert cat["protocol"] == worker.PROTOCOL
    py = cat["python"]
    assert py["file"].startswith("cpython-3.12.15+20261001-x86_64-unknown-linux-gnu")
    assert len(py["sha256"]) == 64
    for p in cat["packs"]:
        if p["gpu"]:
            assert "lock" not in p
        else:
            assert catalog.lock_text(cat, p)


def test_vla_ref_lock_is_a_copy_of_the_export_requirements():
    cat = catalog.load()
    vla = catalog.find(cat, "vla-ref")
    want = (REPO / "clients" / "vla" / "requirements.txt").read_text(encoding="utf-8")
    assert catalog.lock_text(cat, vla) == want


def test_parse_lock_joins_lines_and_keeps_hashes():
    text = (
        "# a comment\n"
        "Foo_Bar==1.0 \\\n"
        "    --hash=sha256:" + "a" * 64 + " \\\n"
        "    --hash=sha256:" + "b" * 64 + "\n"
        "    # via baz\n"
        "torch==2.14.1+cpu \\\n"
        "    --hash=sha256:" + "c" * 64 + "\n"
    )
    reqs = catalog.parse_lock(text)
    assert reqs == [
        catalog.Req("foo-bar", "1.0", ("a" * 64, "b" * 64)),
        catalog.Req("torch", "2.14.1+cpu", ("c" * 64,)),
    ]


@pytest.mark.parametrize(
    "line",
    [
        "foo==1.0\n",
        "foo>=1.0 --hash=sha256:" + "a" * 64 + "\n",
        "foo==1.0 ; sys_platform == 'win32' --hash=sha256:" + "a" * 64 + "\n",
        "foo==1.0 --hash=md5:abc\n",
    ],
)
def test_parse_lock_refuses_a_line_it_cannot_pin(line):
    with pytest.raises(catalog.LockError):
        catalog.parse_lock(line)


def test_wheel_key_and_names():
    assert catalog.normalize("Foo_Bar.baz") == "foo-bar-baz"
    assert catalog.wheel_key("torch-2.14.1+cpu-cp312-cp312-manylinux_2_28_x86_64.whl") == (
        "torch",
        "2.14.1+cpu",
    )
    assert catalog.wheel_key("Foo_Bar-1.0-1-py3-none-any.whl") == ("foo-bar", "1.0")
    assert catalog.wheel_key("notawheel.tar.gz") is None


# ---------------------------------------------------------------------------
# The bundle tar
# ---------------------------------------------------------------------------


def test_ustar_writes_the_same_bytes_and_tarfile_reads_them():
    files = [("b.txt", b"two"), ("wheels/" + "x" * 95 + ".whl", b"one")]
    a = ustar.write(files)
    assert a == ustar.write(list(reversed(files)))
    assert len(a) % 512 == 0
    with tarfile.open(fileobj=io.BytesIO(a)) as t:
        got = {m.name: t.extractfile(m).read() for m in t.getmembers()}
        assert all(m.mtime == 0 and m.uid == 0 and m.mode == 0o644 for m in t.getmembers())
    assert got == dict(files)


def test_ustar_long_names_go_in_a_pax_header():
    # A wheel name can be longer than the ustar name field.
    long = "wheels/" + "y" * 120 + "-1.0-py3-none-any.whl"
    files = [(long, b"long"), ("a.txt", b"a")]
    data = ustar.write(files)
    with tarfile.open(fileobj=io.BytesIO(data)) as t:
        got = {m.name: t.extractfile(m).read() for m in t.getmembers()}
    assert got == dict(files)
    assert ustar.read(data) == sorted(files)
    assert hashlib.sha256(data).hexdigest() == PAX_VECTOR


PAX_VECTOR = "4fd858c9e651dd835f78a85eff780cabdb930685c815c6a734f6c50551747c08"


def test_ustar_read_gives_files_back_and_refuses_escapes(tmp_path):
    files = [("a/b.txt", b"x"), ("c.whl", b"yy")]
    assert ustar.read(ustar.write(files)) == sorted(files)
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w", format=tarfile.USTAR_FORMAT) as t:
        info = tarfile.TarInfo("../evil")
        t.addfile(info, io.BytesIO(b""))
    with pytest.raises(ValueError):
        ustar.read(buf.getvalue())


# ---------------------------------------------------------------------------
# The worker protocol
# ---------------------------------------------------------------------------


def _start(tmp_path: Path, test_jobs: bool = True) -> subprocess.Popen:
    env = {"PATH": "/usr/bin:/bin", "HOME": str(tmp_path)}
    if test_jobs:
        env["DAISUGI_PACK_TEST_JOBS"] = "1"
    return subprocess.Popen(
        [SYSPY, "-I", str(WORKER), "--pack", "t"],
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        env=env,
    )


def _ask(p: subprocess.Popen, job: str, args: list[str]) -> bytes:
    body = json.dumps({"job": job, "args": args}).encode()
    p.stdin.write(struct.pack(">I", len(body)) + body)
    p.stdin.flush()


def test_worker_ready_line_progress_result_and_clean_exit(tmp_path):
    p = _start(tmp_path)
    ready = json.loads(p.stdout.readline())
    assert ready["ready"] == "daisugi-pack-1" and ready["pack"] == "t"
    _ask(p, "echo", ["one", "two"])
    p.stdin.close()
    out = p.stdout.read().decode().splitlines()
    assert p.wait(10) == 0
    # The echo job also prints to stdout; that text goes to stderr, so the
    # protocol stream holds only the reply lines.
    assert [json.loads(x) for x in out] == [
        {"progress": "one"},
        {"progress": "two"},
        {"result": {"args": ["one", "two"]}},
    ]
    assert b"echo: one" in p.stderr.read()


def test_worker_unknown_job_and_bad_request_are_errors_not_exits(tmp_path):
    p = _start(tmp_path)
    p.stdout.readline()
    _ask(p, "nope", [])
    body = b"[1, 2]"
    p.stdin.write(struct.pack(">I", len(body)) + body)
    p.stdin.close()
    out = [json.loads(x) for x in p.stdout.read().decode().splitlines()]
    assert p.wait(10) == 0
    assert out == [
        {"error": "no job named nope in this worker"},
        {"error": 'a request is a JSON object with a string "job" and a list "args"'},
    ]


def test_worker_test_jobs_need_the_switch(tmp_path):
    p = _start(tmp_path, test_jobs=False)
    p.stdout.readline()
    _ask(p, "echo", [])
    p.stdin.close()
    out = [json.loads(x) for x in p.stdout.read().decode().splitlines()]
    p.wait(10)
    assert out == [{"error": "no job named echo in this worker"}]


@pytest.mark.parametrize("raw", [struct.pack(">I", 0), struct.pack(">I", 10) + b"abc"])
def test_worker_lost_framing_exits_2(tmp_path, raw):
    p = _start(tmp_path)
    p.stdout.readline()
    p.stdin.write(raw)
    p.stdin.close()
    assert p.wait(10) == 2


def test_train_job_needs_a_base_model(tmp_path):
    p = _start(tmp_path, test_jobs=False)
    p.stdout.readline()
    _ask(p, "train", ["--jsonl", "x", "--output", "y"])
    p.stdin.close()
    out = [json.loads(x) for x in p.stdout.read().decode().splitlines()]
    p.wait(10)
    assert out == [{"error": "the train job needs --base-model; daisugi lora train fills it in"}]


# ---------------------------------------------------------------------------
# The caller
# ---------------------------------------------------------------------------


def _fake_pack(tmp_path: Path, worker_text: str | None = None) -> Path:
    d = tmp_path / "packs" / "t"
    (d / "venv" / "bin").mkdir(parents=True)
    os.symlink(SYSPY, d / "venv" / "bin" / "python")
    (d / "worker").mkdir()
    (d / "worker" / "daisugi_pack_worker.py").write_text(
        worker_text if worker_text is not None else WORKER.read_text()
    )
    return d


def test_run_job_streams_progress_and_returns_the_result(tmp_path):
    d = _fake_pack(tmp_path)
    seen: list[str] = []
    env = {"PATH": "/usr/bin:/bin", "DAISUGI_PACK_TEST_JOBS": "1"}
    got = client.run_job(d, "t", "echo", ["a", "b"], seen.append, env=env)
    assert got == client.Outcome(0, {"args": ["a", "b"]}, None)
    assert seen == ["a", "b"]


def test_run_job_error_reply(tmp_path):
    d = _fake_pack(tmp_path)
    got = client.run_job(d, "t", "nope", [], lambda s: None, env={"PATH": "/usr/bin:/bin"})
    assert got == client.Outcome(1, None, "pack t: nope: no job named nope in this worker")


def test_run_job_dead_worker_is_one_line(tmp_path):
    d = _fake_pack(tmp_path)
    env = {"PATH": "/usr/bin:/bin", "DAISUGI_PACK_TEST_JOBS": "1"}
    got = client.run_job(d, "t", "die", ["5"], lambda s: None, env=env)
    assert got == client.Outcome(1, None, "pack t: the worker exited 5: die: asked to exit")


def test_run_job_refuses_another_protocol(tmp_path):
    d = _fake_pack(
        tmp_path,
        'import json,sys\nprint(json.dumps({"ready": "daisugi-pack-0"}), flush=True)\n'
        "sys.stdin.read()\n",
    )
    got = client.run_job(d, "t", "echo", [], lambda s: None, env={"PATH": "/usr/bin:/bin"})
    assert got.code == 1
    assert got.error == (
        "pack t: the worker speaks daisugi-pack-0, not daisugi-pack-1. "
        "Install it again: daisugi pack install t --force"
    )


def test_run_job_not_a_reply_line(tmp_path):
    d = _fake_pack(
        tmp_path,
        'import json,sys\nprint(json.dumps({"ready": "daisugi-pack-1"}), flush=True)\n'
        "sys.stdin.buffer.read(4)\nprint('hello', flush=True)\nsys.stdin.read()\n",
    )
    got = client.run_job(d, "t", "echo", [], lambda s: None, env={"PATH": "/usr/bin:/bin"})
    assert got == client.Outcome(
        1, None, "pack t: the worker wrote a line that is not a reply: hello"
    )


def test_worker_module_is_standalone():
    # The worker runs in the pack's own Python, where opendaisugi is not
    # installed: it must import nothing from the project.
    text = WORKER.read_text()
    assert "opendaisugi" not in "".join(
        ln for ln in text.splitlines(keepends=True) if ln.lstrip().startswith(("import", "from"))
    )
    assert sys.executable  # the test itself runs in the project's venv


# ---------------------------------------------------------------------------
# install, status, remove, list, bundle (with the fake assets)
# ---------------------------------------------------------------------------

ASSETS = REPO / "clients" / "fixtures" / "pack" / "assets"


def _ctx(tmp_path: Path, port: int = 1):
    from opendaisugi.pack import manage

    text = (ASSETS / "catalog.json").read_text().replace("{PORT}", str(port))
    (tmp_path / "cat").mkdir(exist_ok=True)
    (tmp_path / "cat" / "catalog.json").write_text(text)
    (tmp_path / "cat" / "fake.lock").write_bytes((ASSETS / "fake.lock").read_bytes())
    out: list[str] = []
    err: list[str] = []
    ctx = manage.Ctx(
        data_dir=tmp_path / "data",
        cat=catalog.load(tmp_path / "cat" / "catalog.json"),
        out=out.append,
        err=err.append,
        env={"PATH": "/usr/bin:/bin", "HOME": str(tmp_path)},
    )
    return ctx, out, err


def _bundle_dir(tmp_path: Path) -> Path:
    import shutil

    b = tmp_path / "bundle"
    (b / "wheels").mkdir(parents=True)
    shutil.copy(ASSETS / "cpython-fake-x86_64-linux.tar.gz", b)
    for w in (ASSETS / "wheels").iterdir():
        shutil.copy(w, b / "wheels")
    return b


def test_install_offline_status_run_remove(tmp_path):
    from opendaisugi.pack import manage

    ctx, out, err = _ctx(tmp_path)
    assert manage.install(ctx, "train", offline=str(_bundle_dir(tmp_path))) == 0, err
    d = tmp_path / "data" / "packs" / "train"
    assert out[-2] == "Self-test: fakepkg 1.0."
    assert (d / "python" / "bin" / "python3").is_file()
    assert os.readlink(d / "python" / "bin" / "python") == "python3"
    assert (d / "python" / "share" / "fake" / ("d" * 60) / ("f" * 40 + ".txt")).is_file()
    assert not (d / "bundle").exists()
    m = json.loads((d / "manifest.json").read_text())
    assert m["packages"] == ["fake-long-" + "n" * 80 + "==1.0", "fakedep==2.0", "fakepkg==1.0"]
    assert m["source"] == "bundle"
    out.clear()
    assert manage.status(ctx, None) == 0
    assert out[0] == f"train: installed in {d}" and out[-1] == "  ok"
    out.clear()
    assert manage.install(ctx, "train", offline=None) == 0
    assert out == [f"The pack train is already installed in {d}."]
    (d / "worker" / "lora_train.py").write_text("changed")
    out.clear()
    assert manage.status(ctx, "train") == 1
    assert "  problem: the worker file lora_train.py was changed" in out
    out.clear()
    assert manage.remove(ctx, "train") == 0
    assert out == [f"Removed the pack train ({d})."] and not d.exists()
    assert (tmp_path / "data" / "packs").is_dir()


def test_offline_tarball_mismatch_names_the_file_and_leaves_nothing(tmp_path):
    from opendaisugi.pack import manage

    ctx, out, err = _ctx(tmp_path)
    b = _bundle_dir(tmp_path)
    tb = b / "cpython-fake-x86_64-linux.tar.gz"
    tb.write_bytes(tb.read_bytes() + b"x")
    assert manage.install(ctx, "train", offline=str(b)) == 1
    assert err[0].startswith("Hash mismatch: cpython-fake-x86_64-linux.tar.gz has sha256 ")
    assert not (tmp_path / "data" / "packs" / "train").exists()


def test_offline_wheel_mismatch_names_the_file(tmp_path):
    from opendaisugi.pack import manage

    ctx, out, err = _ctx(tmp_path)
    b = _bundle_dir(tmp_path)
    w = b / "wheels" / "fakedep-2.0-py3-none-any.whl"
    w.write_bytes(w.read_bytes() + b"x")
    assert manage.install(ctx, "train", offline=str(b)) == 1
    assert err[0].startswith("Hash mismatch: wheels/fakedep-2.0-py3-none-any.whl has sha256 ")
    assert not (tmp_path / "data" / "packs" / "train").exists()


def test_list_unknown_and_gpu(tmp_path):
    from opendaisugi.pack import manage

    ctx, out, err = _ctx(tmp_path)
    assert manage.list_packs(ctx) == 0
    assert (
        out[0]
        == f"{'train':<14}{'not installed':<15}The pack of the golden cases: two tiny wheels."
    )
    assert out[1].startswith(f"{'train-cuda':<14}{'needs a GPU':<15}")
    err.clear()
    assert manage.install(ctx, "nope", offline=None) == 2
    assert err == ["No pack named nope.", "See: daisugi pack list"]
    err.clear()
    assert manage.install(ctx, "train-cuda", offline=None) == 1
    assert err == ["The pack train-cuda needs a GPU build, which this release does not carry."]


def _serve_fake(tmp_path: Path):
    """The fake tarball and a PEP 503 index of the fake wheels, on a
    loopback port; the caller shuts it down."""
    import http.server
    import threading

    class H(http.server.SimpleHTTPRequestHandler):
        def __init__(self, *a, **kw):
            super().__init__(*a, directory=str(tmp_path / "srv"), **kw)

        def log_message(self, *a):
            pass

    srv = tmp_path / "srv"
    (srv / "python").mkdir(parents=True)
    (srv / "python" / "cpython-fake-x86_64-linux.tar.gz").write_bytes(
        (ASSETS / "cpython-fake-x86_64-linux.tar.gz").read_bytes()
    )
    for w in (ASSETS / "wheels").iterdir():
        name = catalog.normalize(w.name.split("-")[0])
        (srv / "simple" / name).mkdir(parents=True)
        (srv / "simple" / name / w.name).write_bytes(w.read_bytes())
        (srv / "simple" / name / "index.html").write_text(
            f'<html><body><a href="{w.name}">{w.name}</a></body></html>'
        )
    httpd = http.server.ThreadingHTTPServer(("127.0.0.1", 0), H)
    threading.Thread(target=httpd.serve_forever, daemon=True).start()
    return httpd


def test_bundle_tar_then_install_from_it(tmp_path):
    from opendaisugi.pack import manage

    httpd = _serve_fake(tmp_path)
    try:
        ctx, out, err = _ctx(tmp_path, httpd.server_address[1])
        tar = tmp_path / "fake-pack.tar"
        assert manage.bundle(ctx, "train", str(tar)) == 0, err
        names = [n for n, _ in ustar.read(tar.read_bytes())]
        assert names == [
            "cpython-fake-x86_64-linux.tar.gz",
            "wheels/fake_long_" + "n" * 80 + "-1.0-py3-none-any.whl",
            "wheels/fakedep-2.0-py3-none-any.whl",
            "wheels/fakepkg-1.0-py3-none-any.whl",
        ]
        assert not (tmp_path / "fake-pack.tar.partial").exists()
    finally:
        httpd.shutdown()
    assert manage.install(ctx, "train", offline=str(tar)) == 0, err


# ---------------------------------------------------------------------------
# The VLA oracle is one module
# ---------------------------------------------------------------------------


def test_vla_cases_use_the_oracle_module_and_its_noise_matches_the_fixture():
    # Read as text: importing the script would change sys.path.
    src = (REPO / "clients" / "vla_cases.py").read_text()
    assert "noise = vla_oracle.noise" in src and "def noise(" not in src
    assert "def load_policy(" not in (REPO / "clients" / "vla_cases.py").read_text()
    from opendaisugi.pack import vla_oracle

    fx = json.loads((REPO / "clients" / "fixtures" / "vla" / "process.json").read_text())
    for n in fx["noise"]:
        got = vla_oracle.noise(n["seed"])
        assert [float(v) for v in got.ravel()[:64]] == n["head"]


def test_vla_chunk_case_errors_are_plain():
    from opendaisugi.pack import vla_oracle

    with pytest.raises(vla_oracle.CaseError, match="the input has no policy"):
        vla_oracle.chunk_from_case({})
    with pytest.raises(vla_oracle.CaseError, match="not 2x2x3"):
        vla_oracle.decode_image(
            {
                "height": 2,
                "width": 2,
                "rgb_zlib": __import__("base64")
                .b64encode(__import__("zlib").compress(b"abc"))
                .decode(),
            }
        )


# ---------------------------------------------------------------------------
# The command line
# ---------------------------------------------------------------------------


def test_cli_pack_list_reads_the_catalog_named_in_the_environment(tmp_path):
    from typer.testing import CliRunner

    from opendaisugi.cli import app

    _ctx(tmp_path)
    env = {"OPENDAISUGI_PACK_CATALOG": str(tmp_path / "cat" / "catalog.json")}
    r = CliRunner().invoke(app, ["pack", "list", "--data-dir", str(tmp_path / "d")], env=env)
    assert r.exit_code == 0, r.output
    assert r.output.splitlines()[0].startswith("train         not installed  ")


def test_cli_lora_train_without_the_pack(tmp_path):
    from typer.testing import CliRunner

    from opendaisugi.cli import app

    env = {"OPENDAISUGI_LORA_TRAIN": "pack"}
    argv = ["lora", "train", "--jsonl", "x.jsonl", "--output", "out"]
    argv += ["--base-model", "m", "--data-dir", str(tmp_path / "d")]
    r = CliRunner().invoke(app, argv, env=env)
    assert r.exit_code == 1
    assert r.output.splitlines() == [
        "The pack train is not installed.",
        "Install it: daisugi pack install train",
    ]


def test_vla_cases_chunk_through_the_pack(tmp_path):
    # A fake daisugi that checks the input file and answers one chunk.
    hf = tmp_path / "hf"
    for repo, rev in [
        ("lerobot--smolvla_base", "d9f33c94a60fb382c90dea2164c96845bd955e28"),
        ("HuggingFaceTB--SmolVLM2-500M-Video-Instruct", "7b375e1b73b11138ff12fe22c8f2822d8fe03467"),
    ]:
        (hf / "hub" / f"models--{repo}" / "snapshots" / rev).mkdir(parents=True)
    fake = tmp_path / "daisugi"
    fake.write_text(
        "#!/usr/bin/python3\n"
        "import json, sys\n"
        "assert sys.argv[1:5] == ['pack', 'run', 'vla-ref', 'vla-chunk'], sys.argv\n"
        "case = json.load(open(sys.argv[6]))\n"
        "assert case['image']['height'] == 240 and case['task'] == 'pick up the block'\n"
        "assert case['policy'].endswith('d9f33c94a60fb382c90dea2164c96845bd955e28')\n"
        "print(json.dumps({'actions': [[0.5] * 6] * 50}, indent=2))\n"
    )
    fake.chmod(0o755)
    out = tmp_path / "chunk.json"
    r = subprocess.run(
        [sys.executable, str(REPO / "clients" / "vla_cases.py"), "--part", "chunk"]
        + ["--pack", str(fake), "--out", str(out)],
        env={"PATH": "/usr/bin:/bin", "HF_HOME": str(hf), "HOME": str(tmp_path)},
        capture_output=True,
        text=True,
    )
    assert r.returncode == 0, r.stderr
    got = json.loads(out.read_text())
    assert got["seed"] == 0 and got["actions"] == [[0.5] * 6] * 50


def test_train_job_names_the_exception_not_a_trailing_progress_bar(tmp_path):
    # A progress bar closes after the traceback, so the last line is not
    # the reason; the job names the last exception line.
    d = _fake_pack(tmp_path)
    (d / "worker" / "lora_train.py").write_text(
        "import sys\n"
        "print('Traceback (most recent call last):', file=sys.stderr)\n"
        "print('ValueError: boom', file=sys.stderr)\n"
        "print('  0%|          | 0/1 [00:12<?, ?it/s]', file=sys.stderr)\n"
        "sys.exit(1)\n"
    )
    seen: list[str] = []
    got = client.run_job(
        d, "t", "train", ["--base-model", "m"], seen.append, env={"PATH": "/usr/bin:/bin"}
    )
    assert got.error == "pack t: train: the trainer exited 1: ValueError: boom"
    assert seen[-1].strip().startswith("0%")


# ---------------------------------------------------------------------------
# A system pack (the AUR package daisugi-ml lays one out)
# ---------------------------------------------------------------------------


def test_a_system_pack_is_used_when_the_user_has_none(tmp_path):
    from opendaisugi.pack import manage

    ctx, out, err = _ctx(tmp_path)
    r = subprocess.run(
        [SYSPY, str(REPO / "scripts" / "system-pack.py"), "train", str(tmp_path / "sys")]
        + ["--python", SYSPY, "--catalog", str(tmp_path / "cat" / "catalog.json")],
        capture_output=True,
        text=True,
    )
    assert r.returncode == 0, r.stderr
    ctx.system_dir = tmp_path / "sys"
    s = tmp_path / "sys" / "train"
    assert json.loads((s / "manifest.json").read_text())["source"] == "system"
    assert manage.list_packs(ctx) == 0
    assert out[0].startswith(f"{'train':<14}{'system':<15}")
    out.clear()
    assert manage.status(ctx, None) == 0
    assert out == [f"train: provided by the system in {s}", "  ok"]
    out.clear()
    assert manage.run(ctx, "train", "selftest", ["json"]) == 0
    assert json.loads("\n".join(out))["imports"] == {"json": "2.0.9"}
    out.clear()
    assert manage.remove(ctx, "train") == 0
    assert out == ["Nothing to remove: the pack train is not installed."]
    assert s.is_dir()
    (s / "worker" / "vla_oracle.py").write_text("changed")
    out.clear()
    assert manage.status(ctx, "train") == 1
    assert out[1:] == [
        "  problem: the worker file vla_oracle.py was changed",
        "  fix: reinstall the system package that provides it",
    ]


def test_a_read_only_system_pack_runs_and_nothing_is_written(tmp_path):
    """As the AUR package daisugi-ml leaves it: compiled at package time
    and read-only at run time. list, status and a job all work, and the
    tree under it is the same after them."""
    from opendaisugi.pack import manage

    ctx, out, err = _ctx(tmp_path)
    r = subprocess.run(
        [SYSPY, str(REPO / "scripts" / "system-pack.py"), "train", str(tmp_path / "sys")]
        + ["--python", SYSPY, "--catalog", str(tmp_path / "cat" / "catalog.json")],
        capture_output=True,
        text=True,
    )
    assert r.returncode == 0, r.stderr
    sys_dir = tmp_path / "sys"
    subprocess.run([SYSPY, "-m", "compileall", "-q", str(sys_dir / "train" / "worker")], check=True)

    def tree():
        return sorted(
            (str(p.relative_to(sys_dir)), p.stat().st_mtime_ns, p.is_symlink() or p.stat().st_size)
            for p in sys_dir.rglob("*")
        )

    paths = [p for p in sys_dir.rglob("*") if not p.is_symlink()] + [sys_dir]
    for p in paths:
        p.chmod(p.stat().st_mode & ~0o222)
    try:
        before = tree()
        ctx.system_dir = sys_dir
        assert manage.list_packs(ctx) == 0
        assert manage.status(ctx, "train") == 0
        assert manage.run(ctx, "train", "selftest", ["json"]) == 0, err
        assert tree() == before
    finally:
        for p in paths:
            p.chmod(p.stat().st_mode | 0o700)


def test_the_release_s_pack_bundle_step(tmp_path):
    # scripts/pack-bundle.sh, which release.sh runs with PACK_BUNDLES set,
    # driven here by the Python daisugi and the fake catalog.
    httpd = _serve_fake(tmp_path)
    try:
        _ctx(tmp_path, httpd.server_address[1])
        wrapper = tmp_path / "daisugi"
        wrapper.write_text(f'#!/bin/sh\nexec {sys.executable} -m opendaisugi.cli "$@"\n')
        wrapper.chmod(0o755)
        env = {
            "PATH": "/usr/bin:/bin",
            "HOME": str(tmp_path),
            "OPENDAISUGI_PACK_CATALOG": str(tmp_path / "cat" / "catalog.json"),
        }
        r = subprocess.run(
            ["bash", str(REPO / "scripts" / "pack-bundle.sh"), str(wrapper), "9.9.9"]
            + [str(tmp_path / "dist"), "train"],
            env=env,
            capture_output=True,
            text=True,
        )
    finally:
        httpd.shutdown()
    arch = os.uname().machine
    assert r.returncode == 0, r.stderr
    assert r.stdout == f"opendaisugi-pack-train-9.9.9-linux-{arch}.tar\n"
    tar = tmp_path / "dist" / r.stdout.strip()
    assert [n for n, _ in ustar.read(tar.read_bytes())][0] == "cpython-fake-x86_64-linux.tar.gz"
