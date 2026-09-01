"""`daisugi bench loop`: nine gate cases per loop, in that loop's vocabulary.

The cases are a contract, not a live run: nine tool calls against one
committed envelope, replayed with the tool names and input keys each loop
delivers. That is the axis `docs/harness/harness-comparison.md` found to
matter. The envelope classifies by tool name, so a loop whose vocabulary the
gate does not speak has every call refused until something translates.

These cases are ours. They are not the 3x3 battery in docs/harness, which
used stand-in gates and measured the harness rather than the gate.

A loop with no tool for a case counts as not applicable and prints as such. A
loop whose vocabulary is not pinned yet prints absent with its install
command. Nothing here spawns a harness.
"""

from __future__ import annotations

from pathlib import Path

from opendaisugi.bench.corpus import (
    CorpusMissing,
    CorpusRef,
    corpus_ref,
    load_json,
    load_jsonl,
    resolve_corpus,
)
from opendaisugi.bench.layers.gate_path import battery_envelope_ref
from opendaisugi.bench.options import GATE_PATHS, LOOPS, loop_is_installed
from opendaisugi.bench.registry import BenchOpts, BenchSpec, register
from opendaisugi.bench.table import Column, Row, Table
from opendaisugi.gate import evaluate_call
from opendaisugi.models import Envelope

CORPUS = "loop-vocabulary-cases.jsonl"
VOCABULARY = "loop-vocabulary.json"

COLUMNS: tuple[Column, ...] = (
    Column("option", "option"),
    Column("installed", "installed"),
    Column("gate_cases", "gate cases"),
    Column("gate_path", "gate path"),
    Column("fail_open", "fail-open class"),
    Column("source", "source"),
)


def payload_format(loop: str, vocab: dict) -> str:
    """The gate's `--format` for this loop's payloads. Claude's unless the vocabulary says."""
    entry = vocab.get(loop) or {}
    return str(entry.get("fmt") or "claude")


def payload_for(loop: str, case: dict, vocab: dict) -> dict | None:
    """The hook payload this loop would deliver for one case.

    None when the loop has no tool for that case, or when its vocabulary is
    not pinned yet. A guessed name would score a loop we have never seen.
    """
    entry = vocab.get(loop) or {}
    names, keys = entry.get("names"), entry.get("keys")
    if not names or not keys:
        return None
    tool = names.get(case["tool"])
    if not tool:
        return None
    tool_input = {keys.get(k, k): v for k, v in case["input"].items()}
    return {"session_id": f"bench-{loop}", "tool_name": tool, "tool_input": tool_input}


def run_vocabulary_cases(
    loop: str, cases: list[dict], vocab: dict, envelope: Envelope
) -> tuple[int, int, int]:
    """(passed, applicable, not_applicable) for one loop over the cases.

    Each payload is judged under the loop's own gate format, the one its hook
    or extension passes to the gate, so a lowercase pi name classifies the
    way it does in production.
    """
    fmt = payload_format(loop, vocab)
    passed = applicable = not_applicable = 0
    for case in cases:
        payload = payload_for(loop, case, vocab)
        if payload is None:
            not_applicable += 1
            continue
        applicable += 1
        decision = evaluate_call(payload, envelope, mode="enforce", fmt=fmt)
        if decision.allow is (case["expect"] == "allow"):
            passed += 1
    return passed, applicable, not_applicable


def _vocabulary_beside(ref: CorpusRef) -> CorpusRef:
    """The vocabulary file in the directory of the case corpus that was read."""
    path = Path(ref.path).with_name(VOCABULARY)
    if not path.is_file():
        raise CorpusMissing(
            f"No {VOCABULARY} at {path}.\n"
            f"The loop bench reads {VOCABULARY} from the directory of the case corpus.\n"
            f"Put one beside {ref.path.name}, or run without --corpus for the committed pair."
        )
    return corpus_ref(path)


def run(opts: BenchOpts) -> Table:
    ref = resolve_corpus(CORPUS, opts.corpus)
    cases = load_jsonl(ref)
    vocab_ref = _vocabulary_beside(ref)
    vocab = load_json(vocab_ref)
    envelope_ref = battery_envelope_ref()
    envelope = Envelope.model_validate(load_json(envelope_ref))
    rows: list[Row] = []
    for name, spec in LOOPS.items():
        entry = vocab.get(name) or {}
        if not entry.get("names"):
            rows.append(Row(name=name, absent=f"{entry.get('note', 'not pinned')}: {spec.install}"))
            continue
        installed = loop_is_installed(spec)
        if opts.live and not installed:
            rows.append(Row(name=name, absent=spec.install))
            continue
        passed, applicable, na = run_vocabulary_cases(name, cases, vocab, envelope)
        cell = f"{passed}/{applicable}" + (f", {na} n/a" if na else "")
        rows.append(
            Row(
                name=name,
                cells={
                    "option": name,
                    "installed": "yes" if installed else "no",
                    "gate_cases": cell,
                    "gate_path": spec.gate_path,
                    "fail_open": GATE_PATHS[spec.gate_path].fail_open_class,
                    "source": "replay",
                },
            )
        )
    return Table(
        layer="loop",
        columns=COLUMNS,
        rows=tuple(rows),
        corpus=ref,
        companions=(vocab_ref, envelope_ref),
        reproduce=f"uv run --no-sync daisugi bench loop --corpus {ref.rel}",
        notes=(
            "these cases are ours; they are not the 3x3 battery in docs/harness, which used "
            "stand-in gates and measured the harness rather than the gate",
            "the corpus path is repo-relative, so run the reproduce line from the checkout root",
            "the cases replay recorded payload shapes; nothing here spawns a harness",
            "n/a means the loop has no tool for that case, not that the case passed",
            "each loop is judged under its own gate format; pi's lowercase names classify "
            "only under --format pi",
            "sprig's row uses the names its DaisugiGate translates to before we see them",
        ),
    )


register(BenchSpec(name="loop", layer="loop", corpus=CORPUS, columns=COLUMNS, run=run))
