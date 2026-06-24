"""Offline distillation pipeline (v0.3.0).

Reads successful traces from the journal, clusters them by task-embedding
similarity, intersects envelope permissions, picks a representative plan,
LLM-generalizes into a reusable template, validates against held-out
traces, and writes CompiledPathway rows to the PathwayStore.
"""

from __future__ import annotations

import logging
import secrets
import time
from typing import TYPE_CHECKING

from pydantic import BaseModel

from opendaisugi.llm import get_instructor_client
from opendaisugi.models import ActionPlan, Envelope
from opendaisugi.permissions import intersect_permissions as _intersect_permissions
from opendaisugi.verify import verify as _verify

if TYPE_CHECKING:
    import numpy as np

    from opendaisugi.journal import Journal
    from opendaisugi.pathway import PathwayMatch
    from opendaisugi.pathway_store import PathwayStore

from opendaisugi._search import _MODEL_NAME as _EMBEDDING_MODEL_NAME
from opendaisugi.pathway_store import DEFAULT_PATHWAY_THRESHOLD

_log = logging.getLogger("opendaisugi.distiller")

# Pitfalls grow with refinement churn. Cap prevents unbounded prompt growth
# inside _generalize_template while preserving the earliest (most common)
# violations, which tend to be the most actionable.
_MAX_PITFALLS = 20

# Bumped when the embedding algorithm changes in a way that makes new
# vectors incomparable with old ones, even under the same model name.
_EMBEDDING_MODEL_VERSION = "3"


def plan_structure_signature(plan: "ActionPlan") -> str:
    """Canonical structure-only signature of an ActionPlan (v0.24+).

    Returns the topologically-ordered step types joined by ``→``. Two
    plans with the same step-type sequence produce identical signatures
    regardless of task wording, step ids, or step field values. The
    Distiller embeds this alongside task text so structurally identical
    work clusters even when teammates phrase the task differently.

    Topological sort is deterministic for sequential plans (the common
    case); for plans with parallel branches the order within a level
    is by step id, matching ``opendaisugi.dag.topological_order``.
    """
    from opendaisugi.dag import topological_order
    return "→".join(s.type for s in topological_order(plan))


import re as _re

# Skill-invocation boilerplate that bleeds into task strings when a Claude
# Code session opens with "/skill …". The preamble swamps semantic content
# during clustering (every "/some-skill" invocation looks alike), so we
# strip these shapes before embedding. Original task strings are untouched.
_PREAMBLE_PATTERNS: tuple[_re.Pattern[str], ...] = (
    _re.compile(
        r"^[ \t]*Base directory for this skill:[^\n]*\n?",
        _re.MULTILINE,
    ),
    _re.compile(
        r"^[ \t]*###[ \t]+Skill:[^\n]*\n?",
        _re.MULTILINE,
    ),
    _re.compile(
        r"^[ \t]*Path:[ \t]+(?:plugin|bundled):[^\n]*\n?",
        _re.MULTILINE,
    ),
    _re.compile(
        r"<command-name>[^<]*</command-name>"
        r"|<command-message>[^<]*</command-message>"
        r"|<command-args>[^<]*</command-args>",
    ),
)


def _normalize_task_for_embedding(task: str) -> str:
    """Strip skill-invocation preamble from a task string for embedding.

    Returns the original string unchanged if stripping would leave it empty
    (a task that is *entirely* preamble is still better clustered against
    its literal text than against the empty string).
    """
    stripped = task
    for pat in _PREAMBLE_PATTERNS:
        stripped = pat.sub("", stripped)
    stripped = stripped.strip()
    return stripped or task


class TendReport(BaseModel):
    """Summary of a distillation run."""
    created: int
    updated: int
    skipped: int
    pathways: list[str]
    duration_s: float
    warnings: list[str]


def _cluster_with_centroids(
    indices: list[int],
    vecs: np.ndarray,
    *,
    threshold: float,
) -> list[tuple[list[int], np.ndarray]]:
    """Agglomerative clustering by cosine similarity.

    Assigns each index to an existing cluster if the similarity of its
    vector to the cluster centroid exceeds ``threshold``; otherwise opens
    a new cluster. Returns ``(cluster_indices, centroid_vector)`` pairs so
    callers can reuse the centroid without re-embedding members.
    """
    import numpy as np

    if len(indices) == 0:
        return []

    clusters: list[list[int]] = []
    centroids: list[np.ndarray] = []

    for i in indices:
        v = vecs[i]
        v_norm = np.linalg.norm(v) or 1e-9
        v_unit = v / v_norm

        best_cluster = -1
        best_sim = -1.0
        for ci, centroid in enumerate(centroids):
            c_norm = np.linalg.norm(centroid) or 1e-9
            sim = float(np.dot(v_unit, centroid / c_norm))
            if sim > best_sim:
                best_sim = sim
                best_cluster = ci

        if best_sim >= threshold:
            clusters[best_cluster].append(i)
            members = clusters[best_cluster]
            centroids[best_cluster] = vecs[members].mean(axis=0)
        else:
            clusters.append([i])
            centroids.append(v.copy())

    return list(zip(clusters, centroids))


def _cluster_by_similarity(
    indices: list[int],
    vecs: np.ndarray,
    *,
    threshold: float,
) -> list[list[int]]:
    """Cluster indices by cosine similarity; return cluster membership only.

    Thin wrapper over :func:`_cluster_with_centroids` for callers that
    only need the grouping.
    """
    return [cluster for cluster, _ in _cluster_with_centroids(indices, vecs, threshold=threshold)]


def _extract_pitfalls(records: list) -> list[str]:
    """Deduplicate violation messages from refinement records, preserving order."""
    seen: set[str] = set()
    out: list[str] = []
    for rec in records:
        for v in rec.violations:
            msg = f"[{v.stage}] {v.message}"
            if msg not in seen:
                seen.add(msg)
                out.append(msg)
    return out


class GeneralizedTemplate(BaseModel):
    """LLM response shape for template generalization."""
    task_description: str
    plan_template: ActionPlan


_GENERALIZE_SYSTEM = (
    "You are a planner distilling multiple successful runs into a reusable template.\n"
    "Produce a generalized task description and a concrete plan template.\n"
    "The plan template must be a valid ActionPlan with REAL (not placeholder) values —\n"
    "concrete paths, commands, URLs. A later adapt_plan() step rewrites these to\n"
    "match specific tasks. Do not invent <placeholder> tokens."
)


async def _generalize_template(
    *,
    plan: ActionPlan,
    envelope: Envelope,
    pitfalls: list[str],
    model: str,
) -> GeneralizedTemplate:
    """Ask the LLM to generalize one representative plan into a template.

    The envelope constrains what the template must respect; pitfalls list
    failure modes to avoid. Returns a GeneralizedTemplate the caller stores
    on the CompiledPathway.
    """
    client = get_instructor_client(model)
    pitfall_block = (
        "\n".join(f"- {p}" for p in pitfalls) if pitfalls else "(none recorded)"
    )
    user = (
        f"Representative plan:\n{plan.model_dump_json(indent=2)}\n\n"
        f"Envelope constraints:\n{envelope.model_dump_json(indent=2)}\n\n"
        f"Known pitfalls from past rejections:\n{pitfall_block}\n\n"
        "Produce: a generalized task_description (broad enough for similar tasks) "
        "and a plan_template with concrete representative values."
    )
    return await client.chat.completions.create(
        model=model,
        response_model=GeneralizedTemplate,
        messages=[
            {"role": "system", "content": _GENERALIZE_SYSTEM},
            {"role": "user", "content": user},
        ],
        max_retries=2,
    )


def _validate_envelope(
    envelope: "Envelope",
    test_plans: list["ActionPlan"],
) -> tuple[float, list["ActionPlan"]]:
    """Verify each test plan against ``envelope``. Return (score, failing_plans).

    ``score`` is the fraction of plans that pass verification (0.0 if empty).
    ``failing_plans`` is the list of plans that did NOT pass, for use in the
    improvement pass.
    """
    if not test_plans:
        return 0.0, []
    failing: list[ActionPlan] = []
    passed = 0
    for plan in test_plans:
        result = _verify(plan, envelope)
        if result.ok:
            passed += 1
        else:
            failing.append(plan)
    return passed / len(test_plans), failing


_IMPROVE_SYSTEM = (
    "You are tightening an envelope that was too restrictive for some valid plans.\n"
    "Widen ONLY the specific permissions that blocked the failing plans.\n"
    "Do not loosen anything else. Return a revised Envelope."
)


async def _improve_envelope(
    *,
    envelope: "Envelope",
    failing_plans: list["ActionPlan"],
    model: str,
) -> "Envelope":
    """Ask LLM to widen envelope permissions that blocked valid test plans."""
    client = get_instructor_client(model)
    failures_block = "\n\n".join(
        f"Plan {p.id}:\n{p.model_dump_json(indent=2)}"
        for p in failing_plans
    )
    user = (
        f"Current envelope (too tight):\n{envelope.model_dump_json(indent=2)}\n\n"
        f"Plans that should have passed but were rejected:\n{failures_block}\n\n"
        "Return a revised envelope that passes these plans while keeping\n"
        "all other permissions tight."
    )
    return await client.chat.completions.create(
        model=model,
        response_model=Envelope,
        messages=[
            {"role": "system", "content": _IMPROVE_SYSTEM},
            {"role": "user", "content": user},
        ],
        max_retries=2,
    )


class Distiller:
    """Batch distillation: journal traces → compiled pathways."""

    def __init__(
        self,
        *,
        journal: "Journal",
        pathway_store: "PathwayStore",
        model: str = "anthropic/claude-sonnet-4-20250514",
        min_traces: int = 3,
        similarity_threshold: float = DEFAULT_PATHWAY_THRESHOLD,
        lookback_days: int = 30,
        validation_split: float = 0.6,
        structure_weight: float = 0.5,
    ) -> None:
        self.journal = journal
        self.pathway_store = pathway_store
        self.model = model
        self.min_traces = min_traces
        self.similarity_threshold = similarity_threshold
        self.lookback_days = lookback_days
        self.validation_split = validation_split
        # v0.24+: 0.0 = pure task-text clustering (v0.23 behavior),
        # 1.0 = pure structural clustering, 0.5 = balanced. Per-domain
        # tuning happens in v0.25+ Gardener fitness.
        if not 0.0 <= structure_weight <= 1.0:
            raise ValueError(
                f"structure_weight must be in [0, 1], got {structure_weight}"
            )
        self.structure_weight = structure_weight

    def _embed_tasks(self, tasks: list[str]) -> np.ndarray:
        """Embed task strings. Overridable in tests.

        Strips skill-invocation preamble before embedding so clusters reflect
        semantic task intent, not repeated boilerplate from ``/skill …`` opens.
        """
        from opendaisugi._search import _get_model
        normalized = [_normalize_task_for_embedding(t) for t in tasks]
        return _get_model().encode(normalized, convert_to_numpy=True)

    def _embed_plan_structures(self, signatures: list[str]) -> np.ndarray:
        """Embed plan-structure signatures (v0.24+). Overridable in tests.

        Each signature is a deterministic ``→``-joined step-type sequence
        produced by ``plan_structure_signature``. Fed through the same
        sentence-transformer model as task text — slightly out-of-
        distribution but stable enough for clustering at scale. None
        signatures (v0.23 traces missing the field) get the empty string,
        which keeps the embedding tensor shape consistent.
        """
        from opendaisugi._search import _get_model
        normalized = [s or "" for s in signatures]
        return _get_model().encode(normalized, convert_to_numpy=True)

    async def tend(self) -> TendReport:
        """Run the full distillation pipeline."""
        started = time.time()
        warnings: list[str] = []
        created = 0
        updated = 0
        skipped = 0
        pathway_ids: list[str] = []

        since = time.time() - (self.lookback_days * 86400)
        traces = self.journal.list_successful_traces(since=since)
        if len(traces) < self.min_traces:
            msg = (
                f"tend: only {len(traces)} successful trace(s) in the last "
                f"{self.lookback_days} days, below min_traces={self.min_traces}; "
                f"no pathways distilled."
            )
            _log.info(msg)
            return TendReport(
                created=0, updated=0, skipped=len(traces),
                pathways=[], duration_s=time.time() - started,
                warnings=[msg],
            )

        tasks = [t.task for t in traces]
        embed_started = time.time()
        task_vecs = self._embed_tasks(tasks)
        if self.structure_weight > 0:
            sigs = [t.structure_signature or "" for t in traces]
            struct_vecs = self._embed_plan_structures(sigs)
            # Weighted concatenation: distance over the concat = weighted
            # sum of per-component distances (under L2-normalized inputs).
            # Sentence-transformers returns L2-normalized vectors by default.
            import numpy as _np
            tw = (1.0 - self.structure_weight) ** 0.5
            sw = self.structure_weight ** 0.5
            vecs = _np.concatenate([task_vecs * tw, struct_vecs * sw], axis=1)
        else:
            vecs = task_vecs
        embed_s = time.time() - embed_started
        cluster_started = time.time()
        clustered = _cluster_with_centroids(
            list(range(len(traces))), vecs, threshold=self.similarity_threshold,
        )
        cluster_s = time.time() - cluster_started
        _log.info(
            "tend: embedded %d tasks in %.2fs; clustered into %d group(s) in %.2fs "
            "(sizes=%s, threshold=%.2f)",
            len(traces), embed_s, len(clustered), cluster_s,
            [len(c) for c, _ in clustered], self.similarity_threshold,
        )

        for cluster, _combined_centroid in clustered:
            if len(cluster) < self.min_traces:
                skipped += 1
                continue

            cluster_traces = [traces[i] for i in cluster]
            # Staleness check: if an existing pathway already covers this cluster
            # and no cluster trace is newer than the pathway, skip.
            existing = self._find_existing_covering(cluster_traces)
            if existing is not None and not self._cluster_has_new_traces(existing, cluster_traces):
                skipped += 1
                continue

            # Use the task-only centroid for the pathway — PathwayStore.find
            # embeds incoming queries at task-text-only dimensionality, so the
            # stored centroid must match. The structure component shaped the
            # cluster boundary, but pathway lookup stays text-similarity.
            task_centroid = task_vecs[cluster].mean(axis=0)
            pathway = await self._distill_cluster(cluster_traces, task_centroid, warnings)
            if pathway is None:
                skipped += 1
                continue
            self.pathway_store.put(pathway)
            pathway_ids.append(pathway.id)
            if existing is None:
                created += 1
            else:
                updated += 1

        return TendReport(
            created=created, updated=updated, skipped=skipped,
            pathways=pathway_ids, duration_s=time.time() - started,
            warnings=warnings,
        )

    async def _distill_cluster(
        self, cluster_traces: list, centroid: np.ndarray, warnings: list[str],
    ) -> "CompiledPathway | None":
        """Distill one cluster into a CompiledPathway, or None if validation fails hard."""
        from opendaisugi.pathway import CompiledPathway

        # Train/test split (chronological; newest traces in test set).
        split_idx = max(1, int(len(cluster_traces) * self.validation_split))
        train = cluster_traces[:split_idx]
        test = cluster_traces[split_idx:]

        # Load full records for train/test sets. A single corrupt YAML must
        # not kill the whole tend() run — skip+warn on parse failure.
        train_records = self._load_records(train, warnings)
        test_records = self._load_records(test, warnings) if test else []

        if not train_records:
            warnings.append("cluster skipped: no loadable train traces after errors")
            return None

        # Intersect permissions across train envelopes.
        intersected = _intersect_permissions([r.envelope.permissions for r in train_records])

        # Representative: most recent train plan.
        representative = train_records[-1]
        intersected_envelope = representative.envelope.model_copy(
            update={"permissions": intersected, "generated_by": "distilled"},
        )

        # Pitfalls: all refinement records for cluster sessions.
        pitfalls: list[str] = []
        for t in cluster_traces:
            if t.run_id:
                records = self.journal.get_refinements(t.run_id).records
                pitfalls.extend(_extract_pitfalls(records))
        # Dedupe preserving order, then cap to avoid unbounded prompt growth.
        seen: set[str] = set()
        pitfalls = [p for p in pitfalls if not (p in seen or seen.add(p))]
        if len(pitfalls) > _MAX_PITFALLS:
            pitfalls = pitfalls[:_MAX_PITFALLS] + [
                f"... ({len(pitfalls) - _MAX_PITFALLS} more pitfall(s) truncated)"
            ]

        # LLM generalization.
        try:
            generalized = await _generalize_template(
                plan=representative.plan,
                envelope=intersected_envelope,
                pitfalls=pitfalls,
                model=self.model,
            )
        except Exception as exc:
            warnings.append(f"cluster generalization failed: {exc}")
            return None

        # Validate against test set.
        test_plans = [r.plan for r in test_records]
        score, failing = _validate_envelope(intersected_envelope, test_plans)

        if score < 0.5 and failing:
            try:
                improved = await _improve_envelope(
                    envelope=intersected_envelope,
                    failing_plans=failing,
                    model=self.model,
                )
                new_score, _ = _validate_envelope(improved, test_plans)
                if new_score > score:
                    intersected_envelope = improved
                    score = new_score
                else:
                    warnings.append(
                        f"cluster improvement pass did not increase score ({score:.2f} → {new_score:.2f})"
                    )
            except Exception as exc:
                warnings.append(f"cluster improvement pass failed: {exc}")

        _log.info(
            "cluster distilled: size=%d train=%d test=%d pitfalls=%d score=%.2f",
            len(cluster_traces), len(train_records), len(test_records),
            len(pitfalls), score,
        )
        try:
            structure_sig = plan_structure_signature(generalized.plan_template)
        except Exception:
            structure_sig = None
        return CompiledPathway(
            id=f"pathway_{secrets.token_hex(4)}",
            task_description=generalized.task_description,
            task_embedding=centroid.tolist(),
            embedding_model=_EMBEDDING_MODEL_NAME,
            embedding_model_version=_EMBEDDING_MODEL_VERSION,
            envelope=intersected_envelope,
            plan_template=generalized.plan_template,
            source_trace_ids=[t.trace_id for t in cluster_traces],
            version=1,
            hit_count=0,
            distilled_at=time.time(),
            structure_signature=structure_sig,
        )

    def _load_records(self, trace_metas: list, warnings: list[str]) -> list:
        """Load TraceRecords for each metadata row, skipping failures with a warning."""
        records = []
        for t in trace_metas:
            try:
                records.append(self.journal.load_trace(t.trace_id))
            except Exception as exc:
                warnings.append(f"load_trace({t.trace_id}) failed: {exc}")
        return records

    def _find_existing_covering(self, cluster_traces: list) -> "CompiledPathway | None":
        """Look for a pathway whose source_trace_ids contain any cluster trace id."""
        existing = self.pathway_store.list_all()
        cluster_ids = {t.trace_id for t in cluster_traces}
        for p in existing:
            if cluster_ids & set(p.source_trace_ids):
                return p
        return None

    def _cluster_has_new_traces(
        self, pathway: "CompiledPathway", cluster_traces: list,
    ) -> bool:
        """True if any cluster trace isn't already in pathway.source_trace_ids."""
        known = set(pathway.source_trace_ids)
        return any(t.trace_id not in known for t in cluster_traces)


async def adapt_plan(
    match: "PathwayMatch",
    task: str,
    *,
    model: str,
    z3_timeout_ms: int,
) -> ActionPlan:
    """Adapt a pathway's plan template to a specific task via LLM.

    On LLM failure or verification failure, the unmodified template is
    returned — callers always get back a plan that satisfies the pathway's
    envelope, never a half-validated adaptation.
    """
    client = get_instructor_client(model)
    prompt = (
        "You are adapting a reusable plan template to a specific task.\n\n"
        f"Template plan (generalized):\n{match.pathway.plan_template.model_dump_json(indent=2)}\n\n"
        f"Template task: {match.pathway.task_description}\n"
        f"Specific task: {task}\n\n"
        "Produce an ActionPlan that follows the template's structure but is\n"
        "adapted to the specific task. Keep the same step types and general\n"
        "approach. Adjust paths, commands, URLs, and content to match the\n"
        "specific task."
    )
    try:
        adapted = await client.chat.completions.create(
            model=model,
            response_model=ActionPlan,
            messages=[
                {"role": "system", "content": "You adapt plan templates to specific tasks."},
                {"role": "user", "content": prompt},
            ],
            max_retries=2,
        )
    except Exception as exc:
        _log.warning("adapt_plan LLM call failed: %s — returning template", exc)
        return match.pathway.plan_template

    verification = _verify(adapted, match.pathway.envelope, z3_timeout_ms=z3_timeout_ms)
    if not verification.ok:
        _log.warning(
            "adapted plan failed verification (%d violations) — returning template",
            len(verification.violations),
        )
        return match.pathway.plan_template
    return adapted
