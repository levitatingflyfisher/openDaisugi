"""Embedding-based clustering of repeated gateway asks.

``gateway_journal.summarize`` counts repeats by an exact (normalized) sha256
of the ask text — a real signal, but one that only fires on a byte-for-byte
re-ask. Paraphrased work ("summarize the auth module" vs "give me a summary
of the auth code") never collapses under that scheme, which is exactly the
case a later reuse tool most wants to see. This module is ADDITIVE: it does
not touch ``turn_signature`` / ``RepeatGroup`` / ``summarize`` at all, it
adds a second, coarser view over the same journal records.

Clustering reuses the same embedder and the same greedy, threshold-based,
single-pass centroid clustering the offline Distiller already uses (see
``opendaisugi.distiller._cluster_with_centroids``) — deliberately not a
fancier algorithm. The embedder itself is the one shared instance behind
``opendaisugi._search._get_model`` (the ``[search]`` extra's
sentence-transformers model), never a second embedding path.
"""

from __future__ import annotations

from collections import Counter
from collections.abc import Callable, Iterable
from dataclasses import dataclass
from typing import Any

from opendaisugi.gateway_journal import GatewayTurnRecord
from opendaisugi.pathway_store import DEFAULT_PATHWAY_THRESHOLD

EmbedFn = Callable[[list[str]], Any]


@dataclass(frozen=True)
class RepeatCluster:
    """A group of asks that paraphrase each other, seen more than once total.

    ``signatures`` and ``member_tasks`` are index-aligned, one entry per *distinct*
    ``turn_signature`` merged into the cluster (not one per occurrence) — ``count`` carries
    the total occurrence count separately.
    """

    representative_task: str
    signatures: list[str]
    count: int
    member_tasks: list[str]


def _lazy_embed(texts: list[str]) -> Any:
    """Embed ``texts`` with the repo's one shared sentence-transformer model.

    ``opendaisugi._search`` itself imports fine without the ``[search]``
    extra installed (its heavy import is deferred inside ``_get_model``), so
    the guard has to wrap the call that actually loads the model, not the
    module import — mirrors ``PathwayStore.find()``'s guard, not the
    (import-statement-only) one in ``Journal.search()``.
    """
    from opendaisugi._search import _get_model

    try:
        model = _get_model()
    except ImportError:
        raise ImportError(
            "Embedding-based repeat clustering requires the [search] extra: "
            "uv add 'opendaisugi[search]'  (or: pip install 'opendaisugi[search]')"
        ) from None
    return model.encode(texts, convert_to_numpy=True)


def cluster_repeats(
    records: Iterable[GatewayTurnRecord],
    *,
    threshold: float = DEFAULT_PATHWAY_THRESHOLD,
    embed: EmbedFn | None = None,
) -> list[RepeatCluster]:
    """Group repeated asks by embedding similarity, paraphrases included.

    Only records with a non-empty ``signature`` are considered — tool-loop
    continuations carry an empty signature and are excluded here exactly as
    they are from ``gateway_journal.summarize``'s exact-match repeats.

    Distinct task texts are deduped by signature before embedding (one
    embed call per distinct ask, not per occurrence), then greedily
    assigned to the first existing cluster whose centroid similarity is
    ``>= threshold``, else start a new cluster — the same simple approach
    ``opendaisugi.distiller._cluster_with_centroids`` uses. Only clusters
    whose total member count is > 1 are returned (a singleton is not a
    repeat), sorted by count descending.

    ``embed`` takes a list of strings and returns a 2D array/list of
    vectors, one row per input string, in order — inject a deterministic
    fake for tests. When omitted, the shared lazy-loaded repo embedder is
    used (requires the ``[search]`` extra; raises ``ImportError`` with an
    install hint if it is missing).
    """
    signed = [r for r in records if r.signature]
    if not signed:
        return []

    if embed is None:
        embed = _lazy_embed

    counts: Counter[str] = Counter(r.signature for r in signed)
    # r.task is the record's readable label; for every record that survives the signature
    # filter above it is identical to the ask the signature was hashed from (task and ask
    # only diverge on a tool-loop continuation, which is exactly the empty-signature case
    # already excluded) — see gateway.py's _latest_user_text / _new_user_text. So embedding
    # r.task here is embedding the ask itself, not some other governing-task text.
    task_by_sig: dict[str, str] = {r.signature: r.task for r in signed}
    # First-seen order keeps clustering (and therefore output) deterministic.
    distinct_sigs = list(dict.fromkeys(r.signature for r in signed))

    import numpy as np

    from opendaisugi._similarity import cosine_similarity
    from opendaisugi.distiller import _normalize_task_for_embedding

    # Strip skill-invocation preamble before embedding, same as the distiller — raw harness
    # task text routinely carries "/skill ..." boilerplate that would otherwise swamp the
    # real semantic content and risk false-merging unrelated asks. Display fields
    # (representative_task / member_tasks) keep the original, unstripped text.
    texts = [_normalize_task_for_embedding(task_by_sig[sig]) for sig in distinct_sigs]
    vecs = np.asarray(embed(texts), dtype=float)
    sig_to_vec = dict(zip(distinct_sigs, vecs, strict=True))

    cluster_members: list[list[str]] = []
    cluster_centroids: list[np.ndarray] = []
    for sig in distinct_sigs:
        vec = sig_to_vec[sig]
        best_idx = -1
        best_sim = -1.0
        for i, centroid in enumerate(cluster_centroids):
            sim = cosine_similarity(vec, centroid)
            if sim > best_sim:
                best_sim = sim
                best_idx = i
        if best_idx >= 0 and best_sim >= threshold:
            cluster_members[best_idx].append(sig)
            member_vecs = np.array([sig_to_vec[s] for s in cluster_members[best_idx]])
            cluster_centroids[best_idx] = member_vecs.mean(axis=0)
        else:
            cluster_members.append([sig])
            cluster_centroids.append(vec.copy())

    clusters: list[RepeatCluster] = []
    for members in cluster_members:
        total = sum(counts[sig] for sig in members)
        if total <= 1:
            continue
        representative = members[0]
        for sig in members[1:]:
            if counts[sig] > counts[representative]:
                representative = sig
        clusters.append(
            RepeatCluster(
                representative_task=task_by_sig[representative],
                signatures=list(members),
                count=total,
                member_tasks=[task_by_sig[sig] for sig in members],
            )
        )

    clusters.sort(key=lambda c: c.count, reverse=True)
    return clusters
