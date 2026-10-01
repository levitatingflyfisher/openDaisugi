"""The AUR files in packaging/aur (ruling PK-R-15): the source package and
the -bin package install the same files, every package carries the
project's version, and daisugi-py installs the Python CLI as daisugi-py.

Each package() runs in bash against fake extracted tarballs; nothing is
built and makepkg is not needed.
"""

from __future__ import annotations

import json
import os
import re
import subprocess
import sys
import tomllib
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
AUR = ROOT / "packaging" / "aur"
VERSION = tomllib.loads((ROOT / "pyproject.toml").read_text())["project"]["version"]


def _pkgbuilds() -> list[Path]:
    return sorted(AUR.glob("*/PKGBUILD"))


def _var(pkgbuild: Path, name: str) -> str:
    r = subprocess.run(
        ["bash", "-c", f'source "$1" && printf %s "${{{name}}}"', "bash", str(pkgbuild)],
        capture_output=True,
        text=True,
        check=True,
    )
    return r.stdout


def _words(pkgbuild: Path, name: str) -> list[str]:
    """The elements of a PKGBUILD array (or the one value of a scalar), one
    per line, so a space inside an element is kept."""
    r = subprocess.run(
        [
            "bash",
            "-c",
            'source "$1" && printf "%s\\n" "${!2}"',
            "bash",
            str(pkgbuild),
            f"{name}[@]",
        ],
        capture_output=True,
        text=True,
        check=True,
    )
    return [w for w in r.stdout.splitlines() if w]


def _fake_tarballs(srcdir: Path, version: str) -> None:
    go = srcdir / f"opendaisugi-{version}-linux-x86_64"
    rs = srcdir / f"opendaisugi-rust-{version}-linux-x86_64"
    for d, files in (
        (go, ["coppice", "sprig", "daisugi", "LICENSE", "NOTICE-coppice", "NOTICE-sprig",
              "NOTICE-daisugi", "README"]),
        (rs, ["daisugi", "daisugi-gate", "LICENSE", "NOTICE-rust", "README"]),
    ):  # fmt: skip
        d.mkdir(parents=True)
        for f in files:
            (d / f).write_text(f"{d.name}/{f}\n")


def _package(pkgbuild: Path, func: str, tmp: Path) -> dict[str, str]:
    """{path under pkgdir with the license dir named PKG: content}."""
    srcdir, pkgdir = tmp / "src", tmp / "pkg"
    _fake_tarballs(srcdir, VERSION)
    env = {**os.environ, "srcdir": str(srcdir), "pkgdir": str(pkgdir)}
    subprocess.run(
        ["bash", "-ec", f'source "$1"; cd "$srcdir"; {func}', "bash", str(pkgbuild)],
        env=env,
        check=True,
        capture_output=True,
    )
    name = _var(pkgbuild, "pkgname").split()[0]
    out = {}
    for p in sorted(pkgdir.rglob("*")):
        if p.is_file():
            rel = str(p.relative_to(pkgdir)).replace(f"/{name}/", "/PKG/")
            out[rel] = p.read_text() + ("x" if os.access(p, os.X_OK) else "")
    return out


def test_every_package_carries_the_project_version():
    assert {p.parent.name for p in _pkgbuilds()} >= {
        "opendaisugi",
        "opendaisugi-bin",
        "daisugi-py",
        "daisugi-ml",
        "opendaisugi-voice",
    }
    for p in _pkgbuilds():
        assert _var(p, "pkgver") == VERSION, p


def test_source_and_bin_packages_install_the_same_files(tmp_path):
    src = _package(AUR / "opendaisugi" / "PKGBUILD", "package", tmp_path / "a")
    binp = _package(AUR / "opendaisugi-bin" / "PKGBUILD", "package", tmp_path / "b")
    assert src == binp
    bins = sorted(k for k in src if k.startswith("usr/bin/"))
    assert bins == [
        "usr/bin/coppice",
        "usr/bin/daisugi",
        "usr/bin/daisugi-gate-rs",
        "usr/bin/daisugi-rs",
        "usr/bin/sprig",
    ]
    # daisugi is the Go build; daisugi-rs and daisugi-gate-rs the Rust ones.
    assert src["usr/bin/daisugi"].startswith(f"opendaisugi-{VERSION}-linux-x86_64/daisugi")
    assert src["usr/bin/daisugi-rs"].startswith(
        f"opendaisugi-rust-{VERSION}-linux-x86_64/daisugi\n"
    )
    assert all(src[b].endswith("x") for b in bins)


def test_daisugi_py_installs_the_python_cli_as_daisugi_py():
    text = (AUR / "daisugi-py" / "PKGBUILD").read_text()
    assert 'mv "${pkgdir}/usr/bin/daisugi" "${pkgdir}/usr/bin/daisugi-py"' in text
    assert _var(AUR / "daisugi-py" / "PKGBUILD", "arch") == "any"
    deps = _var(AUR / "daisugi-py" / "PKGBUILD", "depends[*]").split()
    # Every runtime dependency of the wheel has an Arch package here.
    wanted = tomllib.loads((ROOT / "pyproject.toml").read_text())["project"]["dependencies"]
    arch = {"pyyaml": "python-yaml", "z3-solver": "python-z3-solver"}
    for spec in wanted:
        name = re.split(r"[<>=!~ \[]", spec, maxsplit=1)[0].lower()
        assert arch.get(name, f"python-{name}") in deps, name


def test_bin_sums_fail_closed_until_a_release():
    """The -bin package fails closed until a release fills its sums in."""
    sums = _var(AUR / "opendaisugi-bin" / "PKGBUILD", "sha256sums[*]").split()
    assert sums == ["0" * 64, "0" * 64]


def test_voice_packages_link_each_engine_into_usr_bin(tmp_path):
    """moonshine-cli and parakeet-cli live under /usr/lib/opendaisugi with
    a relative link in /usr/bin; ONNX Runtime stays private beside
    moonshine-cli, and parakeet-quantize beside parakeet-cli."""
    pkgbuild = AUR / "opendaisugi-voice" / "PKGBUILD"
    src = tmp_path / "src"
    for rel in [
        "moonshine-prefix/bin/moonshine-cli",
        "moonshine-prefix/lib/libonnxruntime.so.1",
        "moonshine-prefix/share/licenses/moonshine/LICENSE",
        "moonshine-prefix/share/licenses/onnxruntime/LICENSE",
        "moonshine-prefix/share/licenses/onnxruntime/ThirdPartyNotices.txt",
        "parakeet-prefix/bin/parakeet-cli",
        "parakeet-prefix/bin/parakeet-quantize",
        "parakeet-prefix/share/licenses/crispasr/LICENSE",
        "parakeet-prefix/share/licenses/ggml/LICENSE",
        "openDaisugi/LICENSE",
    ]:
        (src / rel).parent.mkdir(parents=True, exist_ok=True)
        (src / rel).write_text(rel)
    for name, bin_ in (("moonshine-cli", "moonshine"), ("parakeet-cli", "parakeet")):
        pkg = tmp_path / name
        subprocess.run(
            ["bash", "-ec", f'source "$1"; cd "$srcdir"; package_{name}', "bash", str(pkgbuild)],
            env={**os.environ, "srcdir": str(src), "pkgdir": str(pkg), "pkgname": name},
            check=True,
            capture_output=True,
        )
        link = pkg / "usr" / "bin" / name
        assert link.is_symlink()
        assert os.readlink(link) == f"../lib/opendaisugi/{bin_}/bin/{name}"
        assert (
            link.resolve() == (pkg / "usr" / "lib" / "opendaisugi" / bin_ / "bin" / name).resolve()
        )
    lib = tmp_path / "moonshine-cli" / "usr/lib/opendaisugi/moonshine/lib/libonnxruntime.so.1"
    assert lib.is_file()
    assert not (tmp_path / "moonshine-cli" / "usr/lib/libonnxruntime.so.1").exists()
    quant = tmp_path / "parakeet-cli" / "usr/lib/opendaisugi/parakeet/bin/parakeet-quantize"
    assert os.access(quant, os.X_OK)


def test_daisugi_ml_lays_out_a_compiled_read_only_ready_system_pack(tmp_path):
    """daisugi-ml's package(): the train pack under
    /usr/lib/opendaisugi/packs with its worker compiled, so a run writes
    nothing there; the pack's python is the system's."""
    src, pkg = tmp_path / "src", tmp_path / "pkg"
    src.mkdir()
    (src / "openDaisugi").symlink_to(ROOT)
    subprocess.run(
        [
            "bash",
            "-ec",
            'source "$1"; cd "$srcdir"; package',
            "bash",
            str(AUR / "daisugi-ml" / "PKGBUILD"),
        ],
        env={
            **os.environ,
            "srcdir": str(src),
            "pkgdir": str(pkg),
            "PATH": f"{Path(sys.executable).parent}:{os.environ['PATH']}",
        },
        check=True,
        capture_output=True,
    )
    d = pkg / "usr" / "lib" / "opendaisugi" / "packs" / "train"
    assert os.readlink(d / "venv" / "bin" / "python") == "/usr/bin/python"
    m = json.loads((d / "manifest.json").read_text())
    assert m["source"] == "system"
    for f in m["worker"]:
        assert (d / "worker" / f).is_file()
    pyc = sorted(p.name for p in (d / "worker" / "__pycache__").iterdir())
    assert any(n.startswith("lora_train.") and n.endswith(".pyc") for n in pyc)
    assert any(".opt-1." in n for n in pyc)


def _srcinfo(pkgbuild: Path) -> tuple[dict[str, list[str]], dict[str, dict[str, list[str]]]]:
    """The .SRCINFO beside pkgbuild: the pkgbase block, then one block per
    pkgname, each {key: values}."""
    base: dict[str, list[str]] = {}
    pkgs: dict[str, dict[str, list[str]]] = {}
    cur = base
    for line in (pkgbuild.parent / ".SRCINFO").read_text().splitlines():
        if line.startswith("pkgname = "):
            cur = pkgs.setdefault(line.split(" = ", 1)[1], {})
        elif " = " in line:
            k, v = line.strip().split(" = ", 1)
            cur.setdefault(k, []).append(v)
    return base, pkgs


def _array_literal(text: str, start: int) -> str:
    """The text of the bash array whose '(' is at text[start], to its
    matching ')'. Quotes are honoured: a ')' inside one does not close it."""
    quote = ""
    for j in range(start + 1, len(text)):
        c = text[j]
        if quote:
            quote = "" if c == quote else quote
        elif c in "'\"":
            quote = c
        elif c == ")":
            return text[start : j + 1]
    raise AssertionError("an array that never closes")


def _function_arrays(pkgbuild: Path, func: str) -> dict[str, list[str]]:
    """The depends, optdepends, provides and conflicts arrays that a
    package function sets, as makepkg --printsrcinfo lists them."""
    body = subprocess.run(
        ["bash", "-c", 'source "$1" && declare -f "$2"', "bash", str(pkgbuild), func],
        capture_output=True,
        text=True,
        check=True,
    ).stdout
    out: dict[str, list[str]] = {}
    for m in re.finditer(r"^\s*(depends|optdepends|provides|conflicts)=\(", body, re.MULTILINE):
        literal = _array_literal(body, m.end() - 1)
        r = subprocess.run(
            ["bash", "-c", f'a={literal}; printf "%s\\n" "${{a[@]}}"'],
            capture_output=True,
            text=True,
            check=True,
        )
        out[m.group(1)] = r.stdout.splitlines()
    return out


def test_every_srcinfo_matches_its_pkgbuild():
    """A .SRCINFO is written by makepkg --printsrcinfo; a PKGBUILD edit
    without a new one fails here. Checked against the PKGBUILD: in the
    pkgbase block, pkgver, pkgrel, depends, makedepends, optdepends,
    source and sha256sums; in each pkgname block, the depends, optdepends,
    provides and conflicts its package function sets, and no others."""
    for p in _pkgbuilds():
        base, pkgs = _srcinfo(p)
        assert base["pkgbase"] == [p.parent.name], p
        assert base["pkgver"] == [_var(p, "pkgver")], p
        assert base["pkgrel"] == [_var(p, "pkgrel")], p
        for key in ("depends", "makedepends", "optdepends", "source", "sha256sums"):
            assert base.get(key, []) == _words(p, key), (p, key)
        names = _words(p, "pkgname")
        assert list(pkgs) == names, p
        for name in names:
            func = "package" if len(names) == 1 else f"package_{name}"
            got = _function_arrays(p, func)
            for key in ("depends", "optdepends", "provides", "conflicts"):
                assert pkgs[name].get(key, []) == got.get(key, []), (p, name, key)
