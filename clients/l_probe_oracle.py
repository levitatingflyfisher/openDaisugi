"""The oracle's side of the stage-L probe: one query of the library parts
no command reaches, answered as JSON on stdout.

    python clients/l_probe_oracle.py '<query JSON>'

The binary's side is cmd/l-probe; both take the same query and must print
the same answer. clients/l_probe_cases.py lists the queries. An exception
is answered as {"error": <type>, "msg": <text>}, so a case can hold what
the oracle raises. Every key and path is synthetic.
"""

from __future__ import annotations

import asyncio
import base64
import dataclasses
import json
import sys
import warnings
from typing import Any


def err(e: BaseException) -> dict[str, Any]:
    t = type(e)
    name = t.__name__ if t.__module__ == "builtins" else f"{t.__module__}.{t.__name__}"
    msg = str(e) if not name.endswith("ValidationError") else ""
    return {"error": name, "msg": msg}


def guarded(fn, *a, **k) -> Any:
    try:
        return fn(*a, **k)
    except Exception as e:  # noqa: BLE001 - the probe records what the oracle raises
        return err(e)


# -- signing ------------------------------------------------------------------


def op_b64(q: dict[str, Any]) -> Any:
    return [guarded(lambda s: {"hex": base64.b64decode(s).hex()}, s) for s in q["inputs"]]


def op_sign(q: dict[str, Any]) -> Any:
    from opendaisugi.signing import sign_bytes

    return [
        guarded(lambda c: {"sig": sign_bytes(c["payload"].encode("utf-8"), c["priv"])}, c)
        for c in q["cases"]
    ]


def op_verify(q: dict[str, Any]) -> Any:
    from opendaisugi.signing import verify_bytes

    def one(c: dict[str, Any]) -> Any:
        payload = bytes.fromhex(c["hex"]) if "hex" in c else c["payload"].encode("utf-8")
        return {"ok": verify_bytes(payload, c["sig"], c["pub"])}

    return [guarded(one, c) for c in q["cases"]]


def op_contract(q: dict[str, Any]) -> Any:
    import tempfile
    from pathlib import Path

    from opendaisugi.contracts import Contract
    from opendaisugi.signing import (
        TrustedSignerRegistry,
        canonicalize_contract,
        sign_contract,
        verify_signature_raw,
    )

    c = Contract.model_validate(q["contract"])
    out: dict[str, Any] = {"canonical": canonicalize_contract(c).decode("utf-8")}
    if "priv" in q:
        sig = guarded(sign_contract, c, q["priv"])
        out["signature"] = sig
        if isinstance(sig, str):
            c = c.model_copy(update={"signature": sig, "signer": q.get("signer")})
    out["verify_raw"] = {n: guarded(verify_signature_raw, c, pub) for n, pub in q["trust"].items()}
    with tempfile.TemporaryDirectory() as d:
        p = Path(d) / "trusted_signers.json"
        reg = TrustedSignerRegistry.load(p)
        for n, pub in q["trust"].items():
            reg.add(n, pub)
        reg.save()
        out["registry_file"] = p.read_text(encoding="utf-8")
        reg = TrustedSignerRegistry.load(p)
        out["names"] = reg.names()
        out["registry_verify"] = guarded(reg.verify, c, q["names"])
        if q.get("remove"):
            out["removed"] = [reg.remove(n) for n in q["remove"]]
            out["registry_verify_after"] = guarded(reg.verify, c, q["names"])
    return out


# -- bundles ------------------------------------------------------------------


def op_to_bundle(q: dict[str, Any]) -> Any:
    from opendaisugi.pathway import CompiledPathway
    from opendaisugi.pathway_bundle import compute_bundle_hash, pathway_to_bundle

    p = CompiledPathway.model_validate(q["pathway"])
    b = pathway_to_bundle(
        p,
        publisher=q["publisher"],
        published_at=q["published_at"],
        private_key_b64=q.get("priv"),
        public_key_b64=q.get("pub"),
    )
    return {
        "bundle": b.model_dump(mode="json"),
        "hash": compute_bundle_hash(p, q["publisher"], q["published_at"]),
    }


def op_from_bundle(q: dict[str, Any]) -> Any:
    from opendaisugi.pathway_bundle import PathwayBundle, bundle_to_pathway

    b = PathwayBundle.model_validate(q["bundle"])
    trusted = set(q["trusted"]) if q.get("trusted") is not None else None
    p = bundle_to_pathway(
        b, trusted_pubkey_b64s=trusted, require_signed=q.get("require_signed", True)
    )
    return {"pathway": p.model_dump(mode="json")}


# -- deeds --------------------------------------------------------------------


def op_rollback(q: dict[str, Any]) -> Any:
    from opendaisugi.deeds import rollback_run
    from opendaisugi.journal import Journal

    j = Journal(data_dir=q["data_dir"])
    try:
        return dataclasses.asdict(rollback_run(j, q["run_id"]))
    finally:
        j.close()


def op_touched(q: dict[str, Any]) -> Any:
    from opendaisugi.deeds import touched_files
    from opendaisugi.journal import Journal

    j = Journal(data_dir=q["data_dir"])
    try:
        view = touched_files(j, q["run_id"])
    finally:
        j.close()
    return [[k, dataclasses.asdict(v)] for k, v in view.items()]


def op_reverse(q: dict[str, Any]) -> Any:
    from opendaisugi.deeds import apply_reversal
    from opendaisugi.models import ReversalHandle

    apply_reversal(ReversalHandle.model_validate(q["handle"]))
    return {"ok": True}


# -- strata -------------------------------------------------------------------


def op_strata(q: dict[str, Any]) -> Any:
    from opendaisugi.models import ActionPlan, Envelope, Invariant
    from opendaisugi.strata import StrataStore, promote_constraint

    store = StrataStore()
    env = Envelope.model_validate(q["envelope"]) if "envelope" in q else None
    out = []
    ids: list[str] = []

    def sid(ref: Any) -> str:
        return ids[ref] if isinstance(ref, int) else ref

    for step in q["steps"]:
        kind = step["do"]
        try:
            if kind == "emit":
                s = store.emit(
                    step["kind"],
                    step["content"],
                    provenance=step.get("provenance", ""),
                    status=step.get("status", "open"),
                    tags=step.get("tags"),
                    pinned=step.get("pinned", False),
                )
                ids.append(s.id)
                out.append(s.model_dump(mode="json"))
            elif kind == "set_status":
                out.append(
                    store.set_status(sid(step["ref"]), step["status"]).model_dump(mode="json")
                )
            elif kind == "get":
                s = store.get(sid(step["ref"]))
                out.append(None if s is None else s.model_dump(mode="json"))
            elif kind == "repage":
                out.append(store.repage(sid(step["ref"])).model_dump(mode="json"))
            elif kind == "by_kind":
                out.append([s.model_dump(mode="json") for s in store.by_kind(step["kind"])])
            elif kind == "all":
                out.append([s.model_dump(mode="json") for s in store.all()])
            elif kind == "reconstruct":
                r = store.reconstruct_context(
                    budget=step.get("budget"), tags=step.get("tags"), query=step.get("query")
                )
                out.append(r.model_dump(mode="json"))
            elif kind == "to_json":
                out.append(store.to_json())
            elif kind == "roundtrip":
                store = StrataStore.from_json(store.to_json())
                out.append("ok")
            elif kind == "from_json":
                store = StrataStore.from_json(step["text"])
                ids = [s.id for s in store.all()]
                out.append([s.model_dump(mode="json") for s in store.all()])
            elif kind == "promote":
                inv = (
                    Invariant.model_validate(step["add_invariant"])
                    if "add_invariant" in step
                    else None
                )
                cand = Envelope.model_validate(step["candidate"]) if "candidate" in step else None
                wit = (
                    ActionPlan.model_validate(step["deny_witness"])
                    if "deny_witness" in step
                    else None
                )
                target = store.get(sid(step["ref"]))
                r = promote_constraint(
                    env,
                    target,
                    add_invariant=inv,
                    remove_file_write=step.get("remove_file_write"),
                    candidate=cand,
                    deny_witness=wit,
                )
                if r.ok:
                    env = r.envelope
                out.append(
                    {
                        "ok": r.ok,
                        "reason": r.reason,
                        "enforcement_proven": r.enforcement_proven,
                        "violations": [[v.stage, v.message] for v in r.violations],
                        "envelope": r.envelope.model_dump(mode="json"),
                        "status": target.status,
                    }
                )
            elif kind == "ledger":
                from opendaisugi.strata import RederivationLedger

                out.append(RederivationLedger(**step["fields"]).model_dump(mode="json"))
            else:
                out.append({"error": "unknown step", "msg": kind})
        except Exception as e:  # noqa: BLE001
            out.append(err(e))
    return out


# -- batch runs ---------------------------------------------------------------


def op_batch(q: dict[str, Any]) -> Any:
    from opendaisugi.batch import BatchDeclaration, rollback_result, run_batch, two_ledger_report
    from opendaisugi.journal import Journal
    from opendaisugi.models import Envelope

    decl = BatchDeclaration.model_validate(q["decl"])
    env = Envelope.model_validate(q["envelope"])
    j = Journal(data_dir=q["data_dir"])
    try:
        r = asyncio.run(run_batch(decl, env, journal=j, sample_k=q.get("sample_k")))
    finally:
        j.close()
    out = {
        "status": r.status,
        "reason": r.reason,
        "classification": dataclasses.asdict(r.classification) if r.classification else None,
        "proof": dataclasses.asdict(r.proof) if r.proof else None,
        "sample_ok": r.sample_ok,
        "executed": r.executed,
        "reversals": [h.model_dump(mode="json") for h in r.reversals],
        "rollback": dataclasses.asdict(r.rollback) if r.rollback else None,
        "ledger": r.ledger.model_dump(mode="json") if r.ledger else None,
    }
    if q.get("undo"):
        out["undo"] = dataclasses.asdict(rollback_result(r))
    if q.get("ledger_kwargs") is not None:
        out["ledger_kwargs"] = two_ledger_report(decl, **q["ledger_kwargs"]).model_dump(mode="json")
    return out


OPS = {
    "b64": op_b64,
    "sign": op_sign,
    "verify": op_verify,
    "contract": op_contract,
    "to_bundle": op_to_bundle,
    "from_bundle": op_from_bundle,
    "rollback": op_rollback,
    "touched": op_touched,
    "reverse": op_reverse,
    "strata": op_strata,
    "batch": op_batch,
}


def main(arg: str) -> None:
    q = json.loads(arg)
    with warnings.catch_warnings():
        warnings.simplefilter("ignore")
        try:
            out = OPS[q["op"]](q)
        except Exception as e:  # noqa: BLE001
            out = err(e)
    print(json.dumps(out, ensure_ascii=False, sort_keys=True))


if __name__ == "__main__":
    main(sys.argv[1])
