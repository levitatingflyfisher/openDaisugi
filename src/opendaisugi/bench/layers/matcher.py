"""`daisugi bench matcher`: the built embedders at their own thresholds.

This is `scripts/matcher_fpr.py` grown into a table. Each backend is scored at
the threshold it ships with. See ADR-0018, ADR-0019 and ADR-0021. Comparing
two embedders at one shared cosine number compares nothing, because their
score scales differ. False-positive rate comes from every distinct-task pair
in the corpus. Recall comes from the hand-written paraphrase pairs.

The bench never downloads. A package that is installed but whose model is not
cached reports absent with the fetch command, so a 30 s budget cannot turn
into a 90 MB pull.
"""

from __future__ import annotations

import contextlib
import importlib.util
import itertools
import os
import sys
import time
from pathlib import Path

from opendaisugi.bench.corpus import (
    CorpusMissing,
    CorpusRef,
    corpus_ref,
    load_jsonl,
    resolve_corpus,
)
from opendaisugi.bench.options import MATCHER_BACKENDS
from opendaisugi.bench.registry import BenchOpts, BenchSpec, register
from opendaisugi.bench.table import Column, Row, Table

CORPUS = "tasks.jsonl"
PARAPHRASES = "paraphrases.jsonl"

# The measured false-match rate of MiniLM at 0.55. Every other backend's
# shipped threshold was chosen to match it. See ADR-0018.
TARGET_FPR = 0.0256
THRESHOLDS: tuple[float, ...] = (
    0.15,
    0.20,
    0.25,
    0.30,
    0.35,
    0.40,
    0.45,
    0.50,
    0.55,
    0.59,
    0.65,
    0.70,
)

COLUMNS: tuple[Column, ...] = (
    Column("option", "option"),
    Column("threshold", "threshold", align="right"),
    Column("fpr", "FPR@thr", align="right"),
    Column("recall", "recall@thr", align="right"),
    Column("ms_embed", "ms/embed", align="right", volatile=True),
    Column("mb", "MB", align="right"),
)


@contextlib.contextmanager
def _offline():
    """Force every model loader offline for the duration. A bench never downloads.

    huggingface_hub reads ``HF_HUB_OFFLINE`` once at import and then serves
    the cached value from ``constants``, so the env var alone guards nothing
    once the package is loaded. Both are set here and both are restored.
    The int8 loader fetches its pinned files unless ``OPENDAISUGI_INT8_MODEL``
    names a directory, so that is pinned to the cache directory as well.
    """
    from opendaisugi._search import _int8_cache_dir

    # Import the hub before the env var changes. A first import inside the
    # guard would read the value the guard set and record offline as the
    # state to restore.
    constants = None
    prev_flag = None
    if importlib.util.find_spec("huggingface_hub") is not None:
        import huggingface_hub.constants as constants

        prev_flag = constants.HF_HUB_OFFLINE
    prev_env = os.environ.get("HF_HUB_OFFLINE")
    prev_int8 = os.environ.get("OPENDAISUGI_INT8_MODEL")
    os.environ["HF_HUB_OFFLINE"] = "1"
    if prev_int8 is None:
        os.environ["OPENDAISUGI_INT8_MODEL"] = str(_int8_cache_dir())
    if constants is not None:
        constants.HF_HUB_OFFLINE = True
    try:
        yield
    finally:
        if prev_env is None:
            os.environ.pop("HF_HUB_OFFLINE", None)
        else:
            os.environ["HF_HUB_OFFLINE"] = prev_env
        if prev_int8 is None:
            os.environ.pop("OPENDAISUGI_INT8_MODEL", None)
        else:
            os.environ["OPENDAISUGI_INT8_MODEL"] = prev_int8
        if constants is not None:
            constants.HF_HUB_OFFLINE = prev_flag


def build_embedder(name: str):
    """Construct one backend, or return None when it cannot run offline right now."""
    from opendaisugi import _search

    with _offline():
        try:
            if name == "lexical":
                return _search._LexicalEmbedder()
            if name == "potion":
                return _search._PotionEmbedder(_search.resolve_potion_model())
            if name == _search._MODEL_NAME:
                return _search._STEmbedder()
            if name == "int8":
                return _search._Int8Embedder()
        except Exception:  # noqa: BLE001
            return None
    return None


def absent_hint(name: str) -> str:
    """The next command for a backend the bench could not build.

    A missing package and an uncached model are different absences. The first
    needs a pip extra. The second needs the model fetched once, or a local
    directory named through the backend's env var.
    """
    from opendaisugi._search import resolve_potion_model

    spec = MATCHER_BACKENDS[name]
    if spec.package is not None and importlib.util.find_spec(spec.package) is None:
        return f"pip install '{spec.extra}'"
    if name == "potion":
        return (
            f"cache {resolve_potion_model()} once with matcher_model: potion, "
            f"or set OPENDAISUGI_POTION_MODEL=<local dir>"
        )
    if name == "int8":
        return (
            "fetch the pinned int8 files once with matcher_model: int8, "
            "or set OPENDAISUGI_INT8_MODEL=<local dir>"
        )
    return (
        f"run one search with matcher_model: {name} while online, or point HF_HOME "
        f"at a cache that holds sentence-transformers/{name}"
    )


def _threshold_for(name: str) -> float:
    from opendaisugi import _search
    from opendaisugi.pathway_store import DEFAULT_PATHWAY_THRESHOLD

    return {
        "lexical": _search._LEXICAL_THRESHOLD,
        "potion": _search._POTION_THRESHOLD,
        "int8": _search._INT8_THRESHOLD,
        _search._MODEL_NAME: DEFAULT_PATHWAY_THRESHOLD,
    }[name]


def _hf_snapshot_dir(model_id: str) -> Path:
    """This model's own directory in the HF cache, never the whole cache tree.

    Summing ``~/.cache/huggingface/hub`` would report the operator's entire
    model collection as each backend's size. That is a wrong number printed
    with confidence.
    """
    return (
        Path.home() / ".cache" / "huggingface" / "hub" / ("models--" + model_id.replace("/", "--"))
    )


def _cache_mb(name: str) -> float:
    """On-disk size of this backend's model files, in MB. Zero for the model-free floor."""
    from opendaisugi._search import _MODEL_NAME, _int8_cache_dir, resolve_potion_model

    if name == "potion":
        model = resolve_potion_model()
        root = Path(model) if Path(model).is_dir() else _hf_snapshot_dir(model)
    elif name == _MODEL_NAME:
        root = _hf_snapshot_dir(f"sentence-transformers/{_MODEL_NAME}")
    elif name == "int8":
        root = _int8_cache_dir()
    else:
        return 0.0
    if not root.exists():
        return 0.0
    # The HF cache keeps bytes under blobs/ and symlinks under snapshots/.
    # Counting both would report every model twice.
    total = sum(f.stat().st_size for f in root.rglob("*") if f.is_file() and not f.is_symlink())
    return round(total / (1024 * 1024), 1)


def fpr_table(embed, tasks: list[str], thresholds=THRESHOLDS) -> list[tuple[float, float]]:
    """Threshold and false-positive rate pairs over every distinct-task pair."""
    import numpy as np

    vecs = np.asarray(embed(tasks))
    idx = np.asarray(list(itertools.combinations(range(len(tasks)), 2)))
    scores = np.sum(vecs[idx[:, 0]] * vecs[idx[:, 1]], axis=1)
    return [(t, float((scores >= t).mean())) for t in thresholds]


def measure(
    embed,
    tasks: list[str],
    paras: list[dict],
    *,
    threshold: float = 0.0,
    task_by_id: dict[str, str] | None = None,
) -> dict:
    """Pair scores both ways, plus warm ms/embed.

    Distinct-task pairs give the false-positive rate. Paraphrase pairs give
    recall. Each paraphrase names the task it restates through ``of``, and
    ``task_by_id`` resolves that id. When it is not given, the committed task
    corpus is read. The raw score arrays are returned too, so a caller can
    re-score at any threshold without embedding twice.
    """
    import numpy as np

    if task_by_id is None:
        task_by_id = {r["id"]: r["task"] for r in load_jsonl(resolve_corpus(CORPUS, None))}

    vecs = np.asarray(embed(tasks))
    idx = np.asarray(list(itertools.combinations(range(len(tasks)), 2)))
    diff_scores = np.sum(vecs[idx[:, 0]] * vecs[idx[:, 1]], axis=1)

    left = [task_by_id[p["of"]] for p in paras]
    right = [p["task"] for p in paras]
    same_scores = np.sum(np.asarray(embed(left)) * np.asarray(embed(right)), axis=1)

    t0 = time.monotonic()
    embed(tasks)
    ms_embed = ((time.monotonic() - t0) * 1000) / max(1, len(tasks))

    return {
        "pairs_same": float(len(same_scores)),
        "pairs_diff": float(len(diff_scores)),
        "same_scores": same_scores,
        "diff_scores": diff_scores,
        "ms_embed": ms_embed,
        "recall": float((same_scores >= threshold).mean()),
        "fpr": float((diff_scores >= threshold).mean()),
    }


def _paraphrases_beside(ref: CorpusRef, task_by_id: dict[str, str]) -> tuple[CorpusRef, list[dict]]:
    """The paraphrase file next to the task corpus that was read, checked.

    Recall is computed from this file, so it is pinned beside the task file,
    and every paraphrase must restate a task the corpus holds. A foreign id
    is a corpus error with the fix in it, not a KeyError deep in numpy.
    """
    path = ref.path.with_name(PARAPHRASES)
    if not path.is_file():
        raise CorpusMissing(
            f"No paraphrase file at {path}.\n"
            f"The matcher bench reads {PARAPHRASES} from the directory of the task corpus.\n"
            f"Put one beside {ref.path.name}, or run without --corpus for the committed pair."
        )
    para_ref = corpus_ref(path)
    paras = load_jsonl(para_ref)
    unknown = sorted({p["of"] for p in paras if p["of"] not in task_by_id})
    if unknown:
        raise CorpusMissing(
            f"{path} restates task ids that {ref.path} does not hold: {', '.join(unknown)}.\n"
            f"Every 'of' in {PARAPHRASES} must be an id in the task corpus."
        )
    return para_ref, paras


def run(opts: BenchOpts) -> Table:
    ref = resolve_corpus(CORPUS, opts.corpus)
    task_rows = load_jsonl(ref)
    tasks = [r["task"] for r in task_rows]
    task_by_id = {r["id"]: r["task"] for r in task_rows}
    para_ref, paras = _paraphrases_beside(ref, task_by_id)
    rows: list[Row] = []
    for name in MATCHER_BACKENDS:
        embedder = build_embedder(name)
        if embedder is None:
            rows.append(Row(name=name, absent=absent_hint(name)))
            continue
        thr = _threshold_for(name)
        stats = measure(embedder.encode, tasks, paras, threshold=thr, task_by_id=task_by_id)
        rows.append(
            Row(
                name=name,
                cells={
                    "option": name,
                    "threshold": thr,
                    "fpr": round(stats["fpr"], 4),
                    "recall": round(stats["recall"], 4),
                    "ms_embed": stats["ms_embed"],
                    "mb": _cache_mb(name),
                },
            )
        )
    return Table(
        layer="matcher",
        columns=COLUMNS,
        rows=tuple(rows),
        corpus=ref,
        companions=(para_ref,),
        reproduce=f"uv run --no-sync daisugi bench matcher --corpus {ref.rel}",
        notes=(
            f"each backend is scored at its own shipped threshold, FPR-matched to {TARGET_FPR}",
            "the corpus path is repo-relative, so run the reproduce line from the checkout root",
            f"recall reads {PARAPHRASES} beside the task corpus; both digests are printed",
            "this bench never downloads; derive a new threshold with scripts/matcher_fpr.py",
            f"python {sys.version_info.major}.{sys.version_info.minor}, CPU only",
        ),
    )


register(BenchSpec(name="matcher", layer="matcher", corpus=CORPUS, columns=COLUMNS, run=run))
