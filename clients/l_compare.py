"""Run the stage-L cases through a daisugi binary (and its l-probe) and
compare them to the oracle.

    uv run --no-sync python clients/l_compare.py --binary PATH/daisugi \\
        [--probe PATH/l-probe] [--oracle] [--only NAME] [--skip NAME] \\
        [--verbose] [--max-refused N]

For every case in clients/fixtures/l (clients/l_cases.py writes them from
the oracle) it checks the exit code, stdout, stderr and the tree after,
as clients/k4_compare.py does. A probe case runs the l-probe; with no
--probe given, each probe case counts as not ported.

Values made from the clock or a random key are renamed in both results
(the case's `vary`), so the guard checks each with the oracle's own code
on the tree the binary left:

- every pathway bundle in a registry: its hash recomputed from its
  content equals the bundle_hash it carries and its file name, and its
  signature verifies under the key it names;
- every release manifest: its signature verifies under the key the case
  signed with, its created_at has isoformat's shape, and each artifact's
  SHA-256 and size are the file's;
- every keypair keygen wrote: the public key is the private key's;
- every pathway store and journal: Python still reads it
  (garden_compare.guard).

--oracle also reruns the Python side, so a stale fixture shows up.
--max-refused N fails the run when more than N cases are refused or not
ported, so a binary that carries L cannot fall back to refusing it.
"""

from __future__ import annotations

import argparse
import collections
import json
import re
import shutil
import sys
import time
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))

import garden_cases  # noqa: E402 - sibling module, run as a script
import l_cases  # noqa: E402 - sibling module (it patches garden_cases)
from cli_cases import as_daisugi  # noqa: E402
from fixture_paths import leaks  # noqa: E402
from garden_compare import before_tree, compare, guard  # noqa: E402
from l_cases import FIXTURE_DIR, SCRATCH, cmd_for, run  # noqa: E402
from pathway_compare import STDERR_CLASSES, diff  # noqa: E402

_ISO = re.compile(r"^20[0-9]{2}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{6})?Z$")


def _pub_of(priv_b64: str) -> str | None:
    import base64

    from cryptography.hazmat.primitives import serialization
    from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey

    try:
        k = Ed25519PrivateKey.from_private_bytes(base64.b64decode(priv_b64))
    except ValueError:
        return None
    raw = k.public_key().public_bytes(serialization.Encoding.Raw, serialization.PublicFormat.Raw)
    return base64.b64encode(raw).decode()


def l_guard(case: dict[str, Any], work: Path) -> list[str]:
    """The oracle's own checks of what the binary signed and hashed."""
    import yaml

    from opendaisugi.pathway_bundle import PathwayBundle, _canonical_payload, compute_bundle_hash
    from opendaisugi.release import verify_manifest_signature
    from opendaisugi.signing import verify_bytes

    home = work / "home"
    out: list[str] = []
    # Only a case whose output was renamed needs these checks; every other
    # case compares its bytes exactly (and may lay out broken bundles on
    # purpose).
    if not home.exists() or not case.get("vary"):
        return out
    laid = json.dumps(case.get("before") or {})
    argv = case["argv"]
    signer = None
    if "--private-key" in argv:
        kp = home / argv[argv.index("--private-key") + 1].replace("{HOME}/", "")
        signer = _pub_of(kp.read_text().strip()) if kp.exists() else None
    for p in sorted(home.rglob("*.yaml")):
        if p.parent.name != "pathways" or ".git" in p.parts or p.name in laid:
            # A bundle the case laid out (broken on purpose, some of them)
            # is compared byte for byte; the guard checks what the run made.
            continue
        try:
            b = PathwayBundle.model_validate(yaml.safe_load(p.read_text(encoding="utf-8")))
        except Exception:  # noqa: BLE001 - a file the case laid out broken
            continue
        h = compute_bundle_hash(b.pathway, b.publisher, b.published_at)
        if h != b.bundle_hash or p.stem != h:
            out.append(f"bundle {p.name}: hash {h} vs field {b.bundle_hash}")
        # The signature is checked under the key the case signed with (the
        # public key the bundle names may be another, on purpose).
        if b.signature_b64 and not verify_bytes(
            _canonical_payload(b.pathway, b.publisher, b.published_at),
            b.signature_b64,
            signer or "",
        ):
            out.append(f"bundle {p.name}: the signature does not verify")
    if argv[:2] == ["release", "sign"]:
        key = argv[argv.index("--key") + 1]
        kp = home / key.replace("{HOME}/", "")
        pub = _pub_of(kp.read_text().strip()) if kp.exists() else None
        for m in sorted(home.rglob("*.json")):
            try:
                man = json.loads(m.read_text(encoding="utf-8"))
            except ValueError:
                continue
            if not isinstance(man, dict) or "manifest_version" not in man:
                continue
            if pub is None or not verify_manifest_signature(man, pub):
                out.append(f"manifest {m.name}: the signature does not verify")
            if not _ISO.match(str(man.get("created_at"))):
                out.append(f"manifest {m.name}: created_at {man.get('created_at')!r}")
            for a in man.get("artifacts", []):
                hits = [f for f in home.rglob(a["name"]) if f.is_file()]
                import hashlib

                if not any(
                    hashlib.sha256(f.read_bytes()).hexdigest() == a["sha256"]
                    and f.stat().st_size == a["size"]
                    for f in hits
                ):
                    out.append(f"manifest {m.name}: artifact {a['name']} does not match a file")
    if argv[:2] == ["release", "keygen"]:
        for k in sorted(home.rglob("*.key")):
            pubf = k.with_suffix(".pub")
            if not pubf.exists() or _pub_of(k.read_text().strip()) != pubf.read_text().strip():
                out.append(f"keypair {k.name}: the public key is not the private key's")
    return out


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--binary", required=True)
    ap.add_argument("--probe")
    ap.add_argument("--oracle", action="store_true")
    ap.add_argument("--only")
    ap.add_argument("--skip")
    ap.add_argument("--verbose", action="store_true")
    ap.add_argument("--max-refused", type=int, default=None)
    args = ap.parse_args()
    SCRATCH.mkdir(parents=True, exist_ok=True)
    binary = as_daisugi(args.binary, SCRATCH)
    cases = [
        json.loads(ln)
        for ln in (FIXTURE_DIR / "cases.jsonl").read_text(encoding="utf-8").splitlines()
        if ln.strip()
    ]
    classes: collections.Counter[str] = collections.Counter()
    stale = guarded = 0
    for i, case in enumerate(cases):
        if args.only and args.only not in case["name"]:
            continue
        if args.skip and args.skip in case["name"]:
            continue
        if case["kind"] == "probe" and not args.probe:
            classes["not-ported"] += 1
            if args.verbose:
                print(f"not-ported: {case['name']} (no l-probe)")
            continue
        work = SCRATCH / "cmp" / f"{i:04d}"
        l_cases._CURRENT.clear()
        l_cases._CURRENT.update(case)
        before = before_tree(case, SCRATCH / "cmp" / f"{i:04d}b")
        got = run(case, cmd_for(case, binary, args.probe), work)
        want = {**case["expect"], "requests": []}
        cls, problems = compare({**case, "expect": want}, {**got, "requests": []}, before)
        if cls == "agree":
            problems = l_guard(case, work)
            if got["tree"] != before:
                guarded += 1
                gp = guard(binary, work)
                if gp:
                    # A store the case laid out broken is broken before the
                    # binary runs too; only a new failure counts.
                    base = SCRATCH / "cmp" / f"{i:04d}base"
                    garden_cases.lay_out(case.get("before") or {}, base / "home", time.time())
                    known = {q.split(":")[0] for q in guard(binary, base)}
                    gp = [p for p in gp if p.split(":")[0] not in known]
                    shutil.rmtree(base, ignore_errors=True)
                problems += gp
            if problems:
                cls = "disagree"
        if args.oracle:
            live = run(case, cmd_for(case, None), SCRATCH / "cmp" / f"{i:04d}py")
            if live != case["expect"]:
                stale += 1
                print(f"STALE fixture: {case['name']}: {diff(case['expect'], live)[:3]}")
        classes[cls] += 1
        if cls == "disagree":
            print(f"DISAGREE: {case['name']}")
            for p in problems[:12]:
                print(f"    {str(p)[:600]}")
        elif cls in ("refused", "not-ported") and args.verbose:
            print(f"{cls}: {case['name']} {str(problems[:1])[:300]}")
        shutil.rmtree(work, ignore_errors=True)
    shutil.rmtree(SCRATCH / "cmp", ignore_errors=True)
    print(
        f"\nl: {sum(classes.values())} cases: "
        + ", ".join(f"{k}={v}" for k, v in sorted(classes.items()))
    )
    print("stderr compared: " + ", ".join(f"{k}={v}" for k, v in sorted(STDERR_CLASSES.items())))
    print(f"guard: Python read back the trees of {guarded} cases the binary wrote")
    leaked = leaks()
    for hit in leaked[:20]:
        print(f"LEAK {hit}")
    print(f"machine paths in clients/fixtures: {len(leaked)}")
    refused = classes.get("refused", 0) + classes.get("not-ported", 0)
    over = args.max_refused is not None and refused > args.max_refused
    if over:
        print(f"{refused} cases refused or not ported; at most {args.max_refused} are ruled")
    return 1 if classes.get("disagree") or stale or leaked or over else 0


if __name__ == "__main__":
    raise SystemExit(main())
