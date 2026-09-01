"""The stage-L probe cases: queries of the library parts no command
reaches (clients/l_probe_oracle.py answers them for the oracle, cmd/l-probe
for the binary). clients/l_cases.py runs them with its other cases.

Every key, path and task is synthetic.
"""

from __future__ import annotations

import base64
import json
from typing import Any

from l_cases import keypair, probe
from pathway_cases import envelope, pathway

L = 2**252 + 27742317777372353535851937790883648493


def _b64(b: bytes) -> str:
    return base64.b64encode(b).decode()


def b64_inputs() -> list[str]:
    good = keypair("pub1")[0]
    return [
        good,
        good.rstrip("="),
        good + "=",
        good + "==",
        good + "===",
        good + "\n",
        " " + good,
        good[:10] + "\n" + good[10:],
        good[:10] + "*" + good[10:],
        good.replace("+", "-").replace("/", "_"),
        good + "QUJD",
        good + "QQ==QUJD",
        "QQ==",
        "QQ=",
        "QQ",
        "Q",
        "QUI=",
        "QUI",
        "QUJD",
        "=QUJD",
        "Q=Q=",
        "QU=I=",
        "QUJ=D",
        "====",
        "",
        "   ",
        "!!!",
        "AB==CD==",
        "é",
        "QUJDé",
        "A" * 5,
        "A" * 9,
        "A" * 13 + "==",
        "YQ==YQ==",
        "Y=Q==",
        "YQ=\n=",
        "Y\tQ=\r\n=",
    ]


def sign_cases() -> list[dict[str, Any]]:
    good = keypair("pub1")[0]
    return [
        {"priv": good, "payload": "hello"},
        {"priv": good, "payload": ""},
        {"priv": good, "payload": "ünïcode ☃ 中文"},
        {"priv": keypair("pub2")[0], "payload": "hello"},
        {"priv": good.rstrip("="), "payload": "x"},
        {"priv": "!" + good, "payload": "x"},
        {"priv": _b64(b"k" * 31), "payload": "x"},
        {"priv": _b64(b"k" * 33), "payload": "x"},
        {"priv": _b64(b"k" * 64), "payload": "x"},
        {"priv": "", "payload": "x"},
        {"priv": "abc", "payload": "x"},
        {"priv": "é", "payload": "x"},
        {"priv": _b64(bytes(32)), "payload": "x"},
    ]


def verify_cases() -> list[dict[str, Any]]:
    from opendaisugi.signing import sign_bytes

    priv, pub = keypair("pub1")
    _, pub2 = keypair("pub2")
    sig = sign_bytes(b"hello", priv)
    raw = base64.b64decode(sig)
    r, s = raw[:32], int.from_bytes(raw[32:], "little")
    s_plus_l = (s + L).to_bytes(32, "little")
    small_order = [
        bytes(32),
        (1).to_bytes(32, "little"),
        bytes.fromhex("ecffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f"),
        bytes.fromhex("c7176a703d4dd84fba3c0b760d10670f2a2053fa2c39ccc64ec7fd7792ac037a"),
        bytes.fromhex("26e8958fc2b227b045c3f489f2ef98f0d5dfac05d3c63339b13802886d53fc05"),
    ]
    cases: list[dict[str, Any]] = [
        {"payload": "hello", "sig": sig, "pub": pub},
        {"payload": "hellO", "sig": sig, "pub": pub},
        {"payload": "hello", "sig": sig, "pub": pub2},
        {"payload": "hello", "sig": sig.rstrip("="), "pub": pub},
        {"payload": "hello", "sig": sig, "pub": pub.rstrip("=")},
        {"payload": "hello", "sig": sig + "\n", "pub": " " + pub},
        {"payload": "hello", "sig": sig[:-4], "pub": pub},
        {"payload": "hello", "sig": _b64(raw + b"\0"), "pub": pub},
        {"payload": "hello", "sig": _b64(raw[:63]), "pub": pub},
        {"payload": "hello", "sig": _b64(r + s_plus_l), "pub": pub},
        {"payload": "hello", "sig": "", "pub": pub},
        {"payload": "hello", "sig": sig, "pub": ""},
        {"payload": "hello", "sig": "!!", "pub": pub},
        {"payload": "hello", "sig": sig, "pub": "abc"},
        {"payload": "hello", "sig": sig, "pub": _b64(b"p" * 31)},
        {"payload": "hello", "sig": sig, "pub": _b64(b"p" * 33)},
        {"payload": "hello", "sig": "é", "pub": pub},
        {"payload": "hello", "sig": sig, "pub": "é"},
        {"payload": "hello", "sig": _b64(bytes(64)), "pub": pub},
        {"hex": "00ff80", "sig": sign_bytes(bytes.fromhex("00ff80"), priv), "pub": pub},
    ]
    for p in small_order:
        cases.append({"payload": "hello", "sig": _b64(bytes(64)), "pub": _b64(p)})
        cases.append({"payload": "hello", "sig": _b64(p + bytes(32)), "pub": _b64(p)})
    # A key that is not a curve point.
    cases.append({"payload": "hello", "sig": sig, "pub": _b64(bytes([2]) + bytes(31))})
    return cases


def contract(**kw: Any) -> dict[str, Any]:
    c = {
        "contract_id": "c1",
        "skill_id": "skill.write",
        "envelope": json.loads(envelope(3, file_write=["/work/**"]).model_dump_json()),
    }
    c.update(kw)
    return c


def pw_dict(i: int, task: str, **kw: Any) -> dict[str, Any]:
    return json.loads(pathway(i, task, [0.5, -0.25, 1e-07, 3.0], **kw).model_dump_json())


def build_probe_cases() -> list[dict[str, Any]]:
    cases = []
    add = cases.append
    add(probe("b64 decode table", {"op": "b64", "inputs": b64_inputs()}))
    add(probe("sign bytes", {"op": "sign", "cases": sign_cases()}))
    add(probe("verify bytes", {"op": "verify", "cases": verify_cases()}))
    p1, p2 = keypair("pub1"), keypair("pub2")
    trust = {"alice": p1[1], "bob": p2[1]}
    add(
        probe(
            "contract sign and verify",
            {
                "op": "contract",
                "contract": contract(),
                "priv": p1[0],
                "signer": "alice",
                "trust": trust,
                "names": ["bob", "alice"],
            },
        )
    )
    add(
        probe(
            "contract unknown names",
            {
                "op": "contract",
                "contract": contract(),
                "priv": p1[0],
                "trust": trust,
                "names": ["carol"],
            },
        )
    )
    add(
        probe(
            "contract signer bob only",
            {
                "op": "contract",
                "contract": contract(),
                "priv": p1[0],
                "trust": trust,
                "names": ["bob"],
            },
        )
    )
    add(
        probe(
            "contract unsigned",
            {"op": "contract", "contract": contract(), "trust": trust, "names": ["alice"]},
        )
    )
    add(
        probe(
            "contract remove signer",
            {
                "op": "contract",
                "contract": contract(),
                "priv": p1[0],
                "trust": trust,
                "names": ["alice"],
                "remove": ["alice", "nobody"],
            },
        )
    )
    add(
        probe(
            "contract full fields",
            {
                "op": "contract",
                "contract": contract(
                    version="2.0",
                    input_schema={"b": 1, "a": [1.5, None, "é"]},
                    output_schema={"z": {"y": True}},
                    guarantees=["no network", "ünïcode"],
                    created_at="2026-01-01",
                    signature="ignored",
                    signer="someone",
                ),
                "priv": p2[0],
                "trust": trust,
                "names": ["bob"],
            },
        )
    )
    add(
        probe(
            "contract carries a bad signature",
            {
                "op": "contract",
                "contract": contract(signature="!!!!"),
                "trust": trust,
                "names": ["alice", "bob"],
            },
        )
    )
    add(
        probe(
            "contract bad private key",
            {
                "op": "contract",
                "contract": contract(),
                "priv": "abc",
                "trust": trust,
                "names": ["alice"],
            },
        )
    )
    add(
        probe(
            "contract invalid",
            {"op": "contract", "contract": {"contract_id": "x"}, "trust": {}, "names": []},
        )
    )
    # bundles
    pw = pw_dict(1, "deploy the café ☃ service")
    add(
        probe(
            "to bundle unsigned",
            {"op": "to_bundle", "pathway": pw, "publisher": "team", "published_at": 1600000000.5},
        )
    )
    add(
        probe(
            "to bundle signed",
            {
                "op": "to_bundle",
                "pathway": pw,
                "publisher": "tëam",
                "published_at": 1600000000.123456789,
                "priv": p1[0],
                "pub": p1[1],
            },
        )
    )
    add(
        probe(
            "to bundle int time",
            {
                "op": "to_bundle",
                "pathway": pw,
                "publisher": "t",
                "published_at": 1600000000,
                "priv": p1[0],
                "pub": p1[1],
            },
        )
    )
    add(
        probe(
            "to bundle priv without pub",
            {
                "op": "to_bundle",
                "pathway": pw,
                "publisher": "t",
                "published_at": 1.0,
                "priv": p1[0],
            },
        )
    )
    add(
        probe(
            "to bundle pub without priv",
            {"op": "to_bundle", "pathway": pw, "publisher": "t", "published_at": 1.0, "pub": p1[1]},
        )
    )
    add(
        probe(
            "to bundle signature kept",
            {
                "op": "to_bundle",
                "pathway": pw_dict(2, "x", structure_signature="kept"),
                "publisher": "t",
                "published_at": 2.5,
            },
        )
    )
    add(
        probe(
            "to bundle odd floats",
            {
                "op": "to_bundle",
                "pathway": {
                    **pw,
                    "distilled_at": 1e-05,
                    "last_activation_at": 1e22,
                    "task_embedding": [1e16, -0.0, 5e-324, 0.1],
                },
                "publisher": "t",
                "published_at": 0.0001,
            },
        )
    )
    add(
        probe(
            "to bundle parameters",
            {
                "op": "to_bundle",
                "pathway": {
                    **pw,
                    "parameters": [
                        {
                            "name": "p",
                            "step_index": 1,
                            "step_id": "s2",
                            "field": "path",
                            "head": "/work",
                            "observed": ["/work/a", "/work/b"],
                        }
                    ],
                },
                "publisher": "t",
                "published_at": 3.0,
                "priv": p2[0],
                "pub": p2[1],
            },
        )
    )
    add(
        probe(
            "to bundle bad key",
            {
                "op": "to_bundle",
                "pathway": pw,
                "publisher": "t",
                "published_at": 1.0,
                "priv": "abc",
                "pub": p1[1],
            },
        )
    )
    add(
        probe(
            "to bundle shell plan",
            {
                "op": "to_bundle",
                "pathway": {
                    **pw,
                    "plan_template": {
                        "id": "plan_00000009",
                        "source": "llm",
                        "task": "t",
                        "steps": [
                            {"id": "a", "type": "shell", "command": "ls"},
                            {
                                "id": "b",
                                "type": "file_write",
                                "path": "/w/x",
                                "content": "y",
                                "depends_on": ["a"],
                            },
                            {
                                "id": "c",
                                "type": "network",
                                "url": "https://e.invalid",
                                "depends_on": ["a"],
                            },
                        ],
                    },
                },
                "publisher": "t",
                "published_at": 4.0,
            },
        )
    )
    from l_probe_oracle import op_to_bundle

    signed = op_to_bundle(
        {
            "pathway": pw,
            "publisher": "team",
            "published_at": 1600000000.5,
            "priv": p1[0],
            "pub": p1[1],
        }
    )["bundle"]
    unsigned = op_to_bundle({"pathway": pw, "publisher": "team", "published_at": 1600000000.5})[
        "bundle"
    ]
    fb = {"op": "from_bundle"}
    add(probe("from bundle trusted", {**fb, "bundle": signed, "trusted": [p1[1]]}))
    add(probe("from bundle untrusted", {**fb, "bundle": signed, "trusted": [p2[1]]}))
    add(probe("from bundle no trust set", {**fb, "bundle": signed, "trusted": None}))
    add(
        probe(
            "from bundle no trust set not required",
            {**fb, "bundle": signed, "trusted": None, "require_signed": False},
        )
    )
    add(probe("from bundle unsigned required", {**fb, "bundle": unsigned, "trusted": [p1[1]]}))
    add(
        probe(
            "from bundle unsigned allowed",
            {**fb, "bundle": unsigned, "trusted": None, "require_signed": False},
        )
    )
    add(
        probe(
            "from bundle tampered publisher",
            {**fb, "bundle": {**signed, "publisher": "mallory"}, "trusted": [p1[1]]},
        )
    )
    add(
        probe(
            "from bundle tampered task",
            {
                **fb,
                "bundle": {**signed, "pathway": {**signed["pathway"], "task_description": "evil"}},
                "trusted": [p1[1]],
            },
        )
    )
    add(
        probe(
            "from bundle tampered hash only",
            {**fb, "bundle": {**signed, "bundle_hash": "0" * 64}, "trusted": [p1[1]]},
        )
    )
    add(
        probe(
            "from bundle sig without key",
            {**fb, "bundle": {**signed, "signer_pubkey_b64": None}, "trusted": [p1[1]]},
        )
    )
    add(
        probe(
            "from bundle garbage sig",
            {**fb, "bundle": {**signed, "signature_b64": "!!"}, "trusted": [p1[1]]},
        )
    )
    add(
        probe(
            "from bundle extra fields",
            {
                **fb,
                "bundle": {**signed, "future": {"x": 1}, "bundle_format_version": 2},
                "trusted": [p1[1]],
            },
        )
    )
    add(
        probe(
            "from bundle missing field",
            {
                **fb,
                "bundle": {k: v for k, v in signed.items() if k != "publisher"},
                "trusted": [p1[1]],
            },
        )
    )
    add(
        probe(
            "from bundle bad pathway",
            {**fb, "bundle": {**signed, "pathway": {"id": "x"}}, "trusted": [p1[1]]},
        )
    )
    add(
        probe(
            "from bundle time as int",
            {**fb, "bundle": {**signed, "published_at": 1600000000}, "trusted": [p1[1]]},
        )
    )
    # deeds
    d = "{HOME}/data"
    add(probe("rollback a run", {"op": "rollback", "data_dir": d, "run_id": "r1"}, deeds_tree()))
    add(
        probe(
            "rollback another run", {"op": "rollback", "data_dir": d, "run_id": "r2"}, deeds_tree()
        )
    )
    add(
        probe(
            "rollback unknown run",
            {"op": "rollback", "data_dir": d, "run_id": "none"},
            deeds_tree(),
        )
    )
    add(probe("rollback empty journal", {"op": "rollback", "data_dir": d, "run_id": "r1"}))
    add(probe("touched a run", {"op": "touched", "data_dir": d, "run_id": "r1"}, deeds_tree()))
    add(
        probe("touched another run", {"op": "touched", "data_dir": d, "run_id": "r2"}, deeds_tree())
    )
    add(
        probe(
            "reverse restore",
            {
                "op": "reverse",
                "handle": {
                    "kind": "file_write",
                    "path": "{HOME}/w/a.txt",
                    "prior_existed": True,
                    "prior_content": "before ☃\n",
                },
            },
            {"w/a.txt": {"text": "after"}},
        )
    )
    add(
        probe(
            "reverse restore none content",
            {
                "op": "reverse",
                "handle": {
                    "kind": "file_write",
                    "path": "{HOME}/w/new/a.txt",
                    "prior_existed": True,
                },
            },
        )
    )
    add(
        probe(
            "reverse delete and dirs",
            {
                "op": "reverse",
                "handle": {
                    "kind": "file_write",
                    "path": "{HOME}/w/x/y/a.txt",
                    "prior_existed": False,
                    "created_dirs": ["{HOME}/w/x/y", "{HOME}/w/x"],
                },
            },
            {"w/x/y/a.txt": {"text": "new"}},
        )
    )
    add(
        probe(
            "reverse dirs not empty",
            {
                "op": "reverse",
                "handle": {
                    "kind": "file_write",
                    "path": "{HOME}/w/x/a.txt",
                    "prior_existed": False,
                    "created_dirs": ["{HOME}/w/x"],
                },
            },
            {"w/x/a.txt": {"text": "new"}, "w/x/b.txt": {"text": "keep"}},
        )
    )
    add(
        probe(
            "reverse already gone",
            {
                "op": "reverse",
                "handle": {
                    "kind": "file_write",
                    "path": "{HOME}/w/gone.txt",
                    "prior_existed": False,
                    "created_dirs": ["{HOME}/w/none"],
                },
            },
        )
    )
    add(
        probe(
            "reverse bad kind",
            {"op": "reverse", "handle": {"kind": "shell", "path": "x", "prior_existed": False}},
        )
    )
    # strata
    for c in strata_cases():
        add(c)
    # batch runs
    for c in batch_run_cases():
        add(c)
    return cases


def deeds_tree() -> dict[str, Any]:
    H = "{HOME}/w"
    rec = [
        {
            "run_id": "r1",
            "step_id": "s1",
            "at": 1.0,
            "effect_class": "file_write",
            "reversibility": "reversible",
            "reversal": {
                "kind": "file_write",
                "path": f"{H}/a.txt",
                "prior_existed": True,
                "prior_content": "a0\n",
            },
        },
        {
            "run_id": "r1",
            "step_id": "s2",
            "at": 2.0,
            "effect_class": "file_write",
            "reversibility": "reversible",
            "reversal": {
                "kind": "file_write",
                "path": f"{H}/new/b.txt",
                "prior_existed": False,
                "created_dirs": [f"{H}/new"],
            },
        },
        {
            "run_id": "r1",
            "step_id": "s3",
            "at": 3.0,
            "effect_class": "file_write",
            "reversibility": "reversible",
            "reversal": {
                "kind": "file_write",
                "path": f"{H}/a.txt",
                "prior_existed": True,
                "prior_content": "a1\n",
            },
        },
        {
            "run_id": "r1",
            "step_id": "s4",
            "at": 4.0,
            "effect_class": "shell",
            "reversibility": "irreversible",
        },
        {
            "run_id": "r1",
            "step_id": "s5",
            "at": 5.0,
            "effect_class": "file_read",
            "reversibility": "none",
        },
        {
            "run_id": "r1",
            "step_id": "s6",
            "at": 6.0,
            "effect_class": "file_write",
            "reversibility": "irreversible",
        },
        {"run_id": "r1", "step_id": "s7", "at": 7.0, "effect_class": None, "reversibility": None},
        {
            "run_id": "r2",
            "step_id": "s1",
            "at": 1.5,
            "effect_class": "file_write",
            "reversibility": "reversible",
            "reversal": {"kind": "file_write", "path": f"{H}/c.txt", "prior_existed": False},
        },
    ]
    return {
        "data": {"receipts": rec},
        "w/a.txt": {"text": "a2\n"},
        "w/new/b.txt": {"text": "b\n"},
        "w/c.txt": {"text": "c\n"},
    }


def strata_cases() -> list[dict[str, Any]]:
    env = json.loads(
        envelope(5, file_write=["/work/**", "/tmp/**"], file_read=["/work/**"]).model_dump_json()
    )
    base = [
        {
            "do": "emit",
            "kind": "fact",
            "content": "The build uses make",
            "provenance": "log",
            "tags": ["build"],
        },
        {
            "do": "emit",
            "kind": "hypothesis",
            "content": "The cache is stale",
            "tags": ["cache", "build"],
        },
        {"do": "emit", "kind": "constraint", "content": "Never write to /tmp", "tags": ["fs"]},
        {"do": "emit", "kind": "goal", "content": "Fix the flaky test", "pinned": True},
        {"do": "emit", "kind": "fact", "content": "CACHE lives in /var", "tags": ["cache"]},
    ]
    cases = []

    def sc(name: str, steps: list[dict[str, Any]], **q: Any) -> None:
        cases.append(probe(name, {"op": "strata", "steps": steps, **q}, vary=("stratum",)))

    sc(
        "strata emit and read",
        base
        + [
            {"do": "all"},
            {"do": "by_kind", "kind": "fact"},
            {"do": "get", "ref": 1},
            {"do": "get", "ref": "stratum_00000000"},
            {"do": "repage", "ref": 2},
            {"do": "repage", "ref": "nope"},
        ],
    )
    sc(
        "strata set status",
        base
        + [
            {"do": "set_status", "ref": 1, "status": "ruled_out"},
            {"do": "set_status", "ref": "nope", "status": "open"},
            {"do": "set_status", "ref": 0, "status": "bogus"},
            {"do": "all"},
        ],
    )
    sc(
        "strata emit bad kind",
        [
            {"do": "emit", "kind": "rumor", "content": "x"},
            {"do": "emit", "kind": "fact", "content": "y", "status": "maybe"},
            {"do": "all"},
        ],
    )
    for b in (None, 0, 1, 2, 3, 10, -1):
        sc(
            f"strata reconstruct budget {b}",
            base
            + [
                {"do": "set_status", "ref": 1, "status": "ruled_out"},
                {"do": "reconstruct", "budget": b},
            ],
        )
    sc(
        "strata reconstruct tags",
        base
        + [
            {"do": "reconstruct", "budget": 3, "tags": ["cache"]},
            {"do": "reconstruct", "budget": 3, "tags": ["build", "cache"]},
        ],
    )
    sc(
        "strata reconstruct query",
        base
        + [
            {"do": "reconstruct", "budget": 3, "query": "cache"},
            {"do": "reconstruct", "budget": 3, "query": "CACHE"},
            {"do": "reconstruct", "budget": 3, "query": ""},
        ],
    )
    sc(
        "strata reconstruct unicode query",
        [
            {"do": "emit", "kind": "fact", "content": "STRASSE ǅ İstanbul"},
            {"do": "emit", "kind": "fact", "content": "other"},
            {"do": "reconstruct", "budget": 2, "query": "straße"},
            {"do": "reconstruct", "budget": 1, "query": "i̇stanbul"},
            {"do": "reconstruct", "budget": 1, "query": "ǆ"},
        ],
    )
    sc(
        "strata pinned over budget",
        base
        + [
            {"do": "emit", "kind": "constraint", "content": "c2"},
            {"do": "reconstruct", "budget": 1},
        ],
    )
    sc(
        "strata roundtrip",
        base
        + [
            {"do": "roundtrip"},
            {"do": "all"},
            {"do": "emit", "kind": "fact", "content": "after"},
            {"do": "to_json"},
        ],
    )
    sc("strata to json", base[:2] + [{"do": "to_json"}])
    sc(
        "strata from json",
        [
            {
                "do": "from_json",
                "text": json.dumps(
                    {
                        "seq": 7,
                        "strata": [
                            {"id": "stratum_aaaaaaaa", "kind": "fact", "content": "x", "seq": 7}
                        ],
                    }
                ),
            },
            {"do": "emit", "kind": "fact", "content": "next"},
            {"do": "to_json"},
        ],
    )
    sc(
        "strata from bad json",
        [
            {"do": "from_json", "text": "{"},
            {"do": "from_json", "text": json.dumps({"seq": "x", "strata": []})},
            {"do": "all"},
        ],
    )
    sc(
        "strata ledger",
        [
            {
                "do": "ledger",
                "fields": {
                    "output_tokens_without_store": 900,
                    "output_tokens_with_store": 1200,
                    "rederived_facts": 2,
                },
            }
        ],
    )
    # promotion
    W = {"type": "file_write"}
    witness = {
        "id": "plan_w0000001",
        "source": "script",
        "task": "w",
        "steps": [{"id": "w", "type": "file_write", "path": "/tmp/x", "content": "y"}],
    }
    ok_witness = {
        **witness,
        "steps": [{"id": "w", "type": "file_write", "path": "/work/x", "content": "y"}],
    }
    sc(
        "strata promote remove glob",
        base
        + [
            {"do": "promote", "ref": 2, "remove_file_write": ["/tmp/**"]},
            {"do": "promote", "ref": 2, "remove_file_write": ["/work/**"]},
        ],
        envelope=env,
    )
    sc(
        "strata promote with witness",
        base
        + [{"do": "promote", "ref": 2, "remove_file_write": ["/tmp/**"], "deny_witness": witness}],
        envelope=env,
    )
    sc(
        "strata promote witness not denied",
        base
        + [
            {
                "do": "promote",
                "ref": 2,
                "remove_file_write": ["/tmp/**"],
                "deny_witness": ok_witness,
            },
            {"do": "get", "ref": 2},
        ],
        envelope=env,
    )
    sc(
        "strata promote a fact",
        base + [{"do": "promote", "ref": 0, "remove_file_write": ["/tmp/**"]}],
        envelope=env,
    )
    sc(
        "strata promote nothing",
        base
        + [
            {"do": "promote", "ref": 2},
            {"do": "promote", "ref": 2, "remove_file_write": ["/nothere/**"]},
        ],
        envelope=env,
    )
    sc(
        "strata promote invariant",
        base
        + [
            {
                "do": "promote",
                "ref": 2,
                "add_invariant": {"type": "no_secrets", "description": "no secrets"},
            }
        ],
        envelope=env,
    )
    sc(
        "strata promote loosening candidate",
        base
        + [
            {
                "do": "promote",
                "ref": 2,
                "candidate": {**env, "permissions": {**env["permissions"], "file_write": ["/**"]}},
            }
        ],
        envelope=env,
    )
    sc(
        "strata promote tightening candidate",
        base
        + [
            {
                "do": "promote",
                "ref": 2,
                "candidate": {
                    **env,
                    "permissions": {**env["permissions"], "file_write": ["/work/**"]},
                },
                "deny_witness": witness,
            }
        ],
        envelope=env,
    )
    sc(
        "strata promote shell off",
        base
        + [
            {
                "do": "promote",
                "ref": 2,
                "candidate": {**env, "permissions": {**env["permissions"], "shell": False}},
            }
        ],
        envelope=env,
    )
    del W
    return cases


def batch_run_cases() -> list[dict[str, Any]]:
    # Relative paths (the probe runs in HOME): the declaration's JSON, whose
    # length the ledger counts, then does not depend on where HOME is.
    W = "out"
    wparam = [{"name": "p", "step_index": 0, "step_id": "w", "field": "path", "head": W}]

    def decl(steps, items, footprint, params=None, **extra):
        return {
            "program": {"id": "plan_b0000001", "source": "script", "task": "batch", "steps": steps},
            "parameters": params if params is not None else wparam,
            "items": items,
            "footprint": footprint,
            **extra,
        }

    wstep = [{"id": "w", "type": "file_write", "path": W + "/x.txt", "content": "new ☃\n"}]
    items = [{"p": f"{W}/{n}.txt"} for n in ("a", "b", "c")]
    env = json.loads(
        envelope(8, file_write=[W + "/**"], file_read=["in.txt", "missing.txt"]).model_dump_json()
    )
    base = {"op": "batch", "envelope": env, "data_dir": "{HOME}/data"}
    APPROVE = {"DAISUGI_APPROVE": "always"}
    cases = []

    def bc(name, q, before=None, env_=APPROVE):
        cases.append(probe(name, {**base, **q}, before, env=env_))

    bc("batch run three", {"decl": decl(wstep, items, [W + "/*"])})
    bc(
        "batch run three undo",
        {"decl": decl(wstep, items, [W + "/*"]), "undo": True},
        {"out/b.txt": {"text": "old b\n"}},
    )
    bc("batch run sample zero", {"decl": decl(wstep, items, [W + "/*"], sample_k=0)})
    bc("batch run sample override", {"decl": decl(wstep, items, [W + "/*"]), "sample_k": 5})
    bc(
        "batch run shell refused",
        {"decl": decl([{"id": "s", "type": "shell", "command": "true"}], [{}], [], params=[])},
    )
    bc("batch run unprovable", {"decl": decl(wstep, items, ["else/*"])})
    bc(
        "batch run irreversible target",
        {"decl": decl(wstep, items, [W + "/*"])},
        {"out/b.txt": {"hex": "ff"}},
    )
    bc(
        "batch run acceptance exists",
        {"decl": decl(wstep, items, [W + "/*"], acceptance={"type": "file_exists"})},
    )
    bc(
        "batch run acceptance nonempty fails",
        {
            "decl": decl(
                [{"id": "w", "type": "file_write", "path": W + "/x.txt", "content": ""}],
                items,
                [W + "/*"],
                acceptance={"type": "file_nonempty"},
            )
        },
    )
    bc(
        "batch run acceptance other path",
        {
            "decl": decl(
                wstep, items, [W + "/*"], acceptance={"type": "file_exists", "path": "missing"}
            )
        },
    )
    bc(
        "batch run acceptance unknown",
        {"decl": decl(wstep, items, [W + "/*"], acceptance={"type": "vibes"})},
    )
    bc(
        "batch run acceptance rc",
        {"decl": decl(wstep, items, [W + "/*"], acceptance={"type": "rc_zero"})},
    )
    bc(
        "batch run read and write",
        {
            "decl": decl(
                wstep + [{"id": "r", "type": "file_read", "path": "in.txt", "depends_on": ["w"]}],
                items,
                [W + "/*"],
            )
        },
        {"in.txt": {"text": "in\n"}},
    )
    bc(
        "batch run read fails halts",
        {
            "decl": decl(
                wstep
                + [{"id": "r", "type": "file_read", "path": "missing.txt", "depends_on": ["w"]}],
                items,
                [W + "/*"],
                sample_k=0,
            )
        },
    )
    bc(
        "batch run new dirs",
        {
            "decl": decl(
                [{"id": "w", "type": "file_write", "path": W + "/d1/x.txt", "content": "x"}],
                [{"p": f"{W}/d1/a.txt"}, {"p": f"{W}/d2/b.txt"}],
                [W + "/**"],
                params=[{**wparam[0], "head": W + "/d1"}],
            ),
            "undo": True,
        },
    )
    bc(
        "batch run ledger kwargs",
        {
            "decl": decl(wstep, items[:1], [W + "/*"]),
            "ledger_kwargs": {
                "output_tokens_saved": 5000,
                "calls_saved": 3,
                "tokens_per_call": 100,
                "spec_input_injected": 10,
            },
        },
    )
    bc("batch run no approval env", {"decl": decl(wstep, items[:1], [W + "/*"])}, env_={})
    bc("batch run bad decl", {"decl": {"program": {}}})
    return cases
