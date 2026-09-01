"""Loading and pinning the committed bench corpora.

A bench prints the digest of the exact bytes it read. That is the whole reason
a `reproduce:` line is worth anything: rerun the command against the same
digest and you get the same rows.
"""

from __future__ import annotations

import hashlib
import json
import os
from dataclasses import dataclass
from pathlib import Path

from opendaisugi.bench.options import repo_root


class CorpusMissing(RuntimeError):
    """The corpus file is not there. The message names the next command."""


@dataclass(frozen=True)
class CorpusRef:
    path: Path
    rel: str
    sha256: str

    @property
    def short(self) -> str:
        return self.sha256[:8]


def corpus_dir() -> Path:
    """Where the committed corpora live.

    ``DAISUGI_BENCH_CORPUS`` overrides, so a copy on real disk can stand in.
    Otherwise it is ``<checkout>/bench/corpus``. An installed wheel has no
    checkout, so the benches say so rather than inventing an empty corpus.
    """
    override = os.environ.get("DAISUGI_BENCH_CORPUS")
    if override:
        return Path(override)
    root = repo_root()
    if root is None:
        raise CorpusMissing(
            "The bench corpora ship with the source, not the wheel.\n"
            "Run `daisugi bench` from a checkout, or set DAISUGI_BENCH_CORPUS "
            "to a copy of bench/corpus."
        )
    return root / "bench" / "corpus"


def corpus_ref(path: Path) -> CorpusRef:
    """Pin one corpus file by the sha256 of its exact bytes."""
    data = path.read_bytes()
    root = repo_root()
    try:
        rel = str(path.relative_to(root)) if root else str(path)
    except ValueError:
        rel = str(path)
    return CorpusRef(path=path, rel=rel, sha256=hashlib.sha256(data).hexdigest())


def resolve_corpus(default_rel: str, override: Path | None) -> CorpusRef:
    """The corpus a bench will read: ``--corpus PATH`` when given, else the default."""
    path = Path(override) if override is not None else corpus_dir() / default_rel
    if not path.is_file():
        raise CorpusMissing(
            f"No corpus at {path}.\n"
            f"The committed corpora live in bench/corpus of the checkout.\n"
            f"Pass --corpus PATH, or set DAISUGI_BENCH_CORPUS to that directory."
        )
    return corpus_ref(path)


def load_jsonl(ref: CorpusRef) -> list[dict]:
    """Every non-blank line of a JSONL corpus, in file order."""
    return [
        json.loads(line)
        for line in ref.path.read_text(encoding="utf-8").splitlines()
        if line.strip()
    ]


def load_json(ref: CorpusRef) -> dict:
    """A whole-file JSON corpus: the envelope, the vocabulary, the price table."""
    return json.loads(ref.path.read_text(encoding="utf-8"))
