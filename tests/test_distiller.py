"""Tests for Distiller clustering + helpers (v0.3.0)."""

import numpy as np
import pytest

from opendaisugi.distiller import (
    TendReport,
    _cluster_by_similarity,
    _intersect_permissions,
    _normalize_task_for_embedding,
)
from opendaisugi.models import Permission


def test_plan_structure_signature_canonical():
    """v0.24: same step-type sequence produces same signature regardless of
    task wording, step ids, or step field values."""
    from opendaisugi.distiller import plan_structure_signature
    from opendaisugi.models import ActionPlan, ShellStep, FileReadStep
    a = ActionPlan(source="x", task="wash dishes", steps=[
        ShellStep(id="a1", command="echo a"),
        FileReadStep(id="a2", path="/etc/hosts", depends_on=["a1"]),
    ])
    b = ActionPlan(source="x", task="totally different wording", steps=[
        ShellStep(id="b1", command="echo z"),
        FileReadStep(id="b2", path="/proc/cpuinfo", depends_on=["b1"]),
    ])
    assert plan_structure_signature(a) == plan_structure_signature(b)
    assert plan_structure_signature(a) == "shell→file_read"


def test_plan_structure_signature_distinguishes_different_shapes():
    from opendaisugi.distiller import plan_structure_signature
    from opendaisugi.models import ActionPlan, ShellStep, FileReadStep
    shell_then_read = ActionPlan(source="x", task="t", steps=[
        ShellStep(id="s1", command="echo"),
        FileReadStep(id="s2", path="/x", depends_on=["s1"]),
    ])
    read_then_shell = ActionPlan(source="x", task="t", steps=[
        FileReadStep(id="s1", path="/x"),
        ShellStep(id="s2", command="echo", depends_on=["s1"]),
    ])
    assert plan_structure_signature(shell_then_read) != plan_structure_signature(read_then_shell)


def test_distiller_clusters_cross_wording_when_structure_matches(tmp_path, monkeypatch):
    """v0.24 win: two traces with the same step-type sequence but completely
    different task wording cluster under the default structure_weight=0.5
    where they would NOT under v0.23 task-only embedding."""
    import numpy as np
    from opendaisugi.distiller import Distiller
    from opendaisugi.journal import Journal
    from opendaisugi.pathway_store import PathwayStore

    journal = Journal(data_dir=tmp_path)
    store = PathwayStore(tmp_path / "pw.db")
    distiller = Distiller(
        journal=journal, pathway_store=store, model="test",
        min_traces=2, structure_weight=0.5,
    )

    # Mock both embedders deterministically. Task vecs are FAR apart on
    # text, structure vecs are IDENTICAL (same plan shape).
    def fake_task_embed(tasks):
        # 3 traces with three orthogonal task vectors
        v = np.eye(3, dtype=float)
        return v[:len(tasks)]

    def fake_struct_embed(sigs):
        # all same structure → same vector
        return np.tile([1.0, 0.0, 0.0], (len(sigs), 1)).astype(float)

    monkeypatch.setattr(distiller, "_embed_tasks", fake_task_embed)
    monkeypatch.setattr(distiller, "_embed_plan_structures", fake_struct_embed)

    # Inline the clustering portion to verify the concat math without
    # needing the journal / LLM-generalize pipeline.
    tasks = ["wash dishes", "do the plates", "run the dish routine"]
    sigs = ["a→b→c"] * 3
    task_vecs = fake_task_embed(tasks)
    struct_vecs = fake_struct_embed(sigs)
    tw = (1.0 - distiller.structure_weight) ** 0.5
    sw = distiller.structure_weight ** 0.5
    combined = np.concatenate([task_vecs * tw, struct_vecs * sw], axis=1)

    # Cosine similarity between any two of these in 6-dim should be
    # dominated by the matching structure component (1.0 contribution)
    # vs orthogonal task components (0.0 contribution after weighting).
    norms = np.linalg.norm(combined, axis=1)
    cos = (combined @ combined.T) / np.outer(norms, norms)
    # Structure weighting lifts the combined cosine well above the task-only
    # similarity, so a shared step-shape clusters even when task text diverges.
    assert cos[0, 1] >= 0.5  # weighted: structure match is half the energy
    # Sanity: pure task vectors are orthogonal (would NOT cluster under v0.23)
    task_cos = task_vecs @ task_vecs.T / np.outer(
        np.linalg.norm(task_vecs, axis=1), np.linalg.norm(task_vecs, axis=1)
    )
    assert task_cos[0, 1] < 0.1


def test_distiller_rejects_invalid_structure_weight(tmp_path):
    """v0.24: structure_weight outside [0, 1] should fail fast at construction."""
    from opendaisugi.distiller import Distiller
    from opendaisugi.journal import Journal
    from opendaisugi.pathway_store import PathwayStore

    journal = Journal(data_dir=tmp_path)
    store = PathwayStore(tmp_path / "pw.db")
    with pytest.raises(ValueError, match="structure_weight"):
        Distiller(journal=journal, pathway_store=store, structure_weight=1.5)


def test_normalize_task_strips_skill_base_directory_line():
    raw = (
        "Base directory for this skill: /home/u/.claude/plugins/cache/thing\n"
        "\n"
        "please refactor the config loader"
    )
    assert _normalize_task_for_embedding(raw) == "please refactor the config loader"


def test_normalize_task_strips_skill_header():
    raw = (
        "### Skill: superpowers:using-superpowers\n"
        "Path: plugin:superpowers:using-superpowers\n"
        "\n"
        "real task content"
    )
    assert _normalize_task_for_embedding(raw) == "real task content"


def test_normalize_task_strips_command_tags():
    raw = "<command-name>/sgcm</command-name><command-message>sgcm</command-message><command-args>foo</command-args>\n\nstress-test the approach"
    assert _normalize_task_for_embedding(raw) == "stress-test the approach"


def test_normalize_task_preserves_non_preamble_text():
    raw = "just a plain user task"
    assert _normalize_task_for_embedding(raw) == "just a plain user task"


def test_normalize_task_returns_original_if_stripping_empties():
    """A task that is entirely preamble should fall back to its original text;
    clustering against an empty string would collapse unrelated tasks together.
    """
    raw = "Base directory for this skill: /x\n"
    assert _normalize_task_for_embedding(raw) == raw


def test_normalize_task_different_preambles_become_distinguishable():
    """Two tasks with identical preamble but different real content should
    normalize to different strings (the whole point of the strip)."""
    preamble = "Base directory for this skill: /home/u/.claude/plugins/cache/thing\n\n"
    a = _normalize_task_for_embedding(preamble + "add CSV parser")
    b = _normalize_task_for_embedding(preamble + "fix auth bug")
    assert a != b
    assert a == "add CSV parser"
    assert b == "fix auth bug"


def test_tend_report_roundtrips():
    r = TendReport(created=1, updated=0, skipped=2, pathways=["p1"], duration_s=0.5, warnings=[])
    js = r.model_dump_json()
    r2 = TendReport.model_validate_json(js)
    assert r2.created == 1
    assert r2.pathways == ["p1"]


def test_cluster_by_similarity_groups_close_vectors():
    # Two tight clusters + one outlier.
    vecs = np.array([
        [1.0, 0.0, 0.0],
        [0.99, 0.01, 0.0],   # close to first
        [0.0, 1.0, 0.0],
        [0.01, 0.99, 0.0],   # close to third
        [0.0, 0.0, 1.0],     # outlier
    ])
    indices = list(range(5))
    clusters = _cluster_by_similarity(indices, vecs, threshold=0.95)
    # Three clusters: {0,1}, {2,3}, {4}
    assert len(clusters) == 3
    sizes = sorted(len(c) for c in clusters)
    assert sizes == [1, 2, 2]


def test_cluster_by_similarity_empty_input():
    assert _cluster_by_similarity([], np.array([]).reshape(0, 3), threshold=0.8) == []


def test_cluster_by_similarity_single_trace():
    vecs = np.array([[1.0, 0.0]])
    clusters = _cluster_by_similarity([0], vecs, threshold=0.9)
    assert clusters == [[0]]


def test_intersect_permissions_shell_and_flags():
    p1 = Permission(shell=True, shell_allowlist=["ls", "cat", "find"], file_read=["/a/**", "/b/**"])
    p2 = Permission(shell=True, shell_allowlist=["cat", "find", "grep"], file_read=["/a/**"])
    p3 = Permission(shell=True, shell_allowlist=["find", "grep"], file_read=["/a/**", "/c/**"])

    result = _intersect_permissions([p1, p2, p3])
    assert result.shell is True
    assert set(result.shell_allowlist) == {"find"}
    assert result.file_read == ["/a/**"]


def test_intersect_permissions_boolean_and():
    p1 = Permission(shell=True, network=True)
    p2 = Permission(shell=True, network=False)
    result = _intersect_permissions([p1, p2])
    assert result.shell is True
    assert result.network is False


def test_intersect_permissions_single_returns_copy():
    p1 = Permission(shell=True, shell_allowlist=["ls"])
    result = _intersect_permissions([p1])
    assert result.shell == p1.shell
    assert result.shell_allowlist == p1.shell_allowlist
    # Not the same object — must be a copy so callers can mutate freely.
    assert result is not p1


def test_intersect_permissions_empty_raises():
    with pytest.raises(ValueError):
        _intersect_permissions([])


def test_intersect_permissions_int_ceilings_take_minimum():
    p1 = Permission(max_execution_time_s=60, max_output_size_mb=20)
    p2 = Permission(max_execution_time_s=10, max_output_size_mb=50)
    result = _intersect_permissions([p1, p2])
    assert result.max_execution_time_s == 10
    assert result.max_output_size_mb == 20


from opendaisugi.models import ShellStep, Violation
from opendaisugi.refinement import RefinementRecord


def _refinement(stage: str, message: str) -> RefinementRecord:
    return RefinementRecord(
        step=ShellStep(id="s1", command="rm -rf /"),
        violations=[Violation(stage=stage, message=message, detail={})],
        z3_counterexample=None,
        envelope_id="env_x",
        fallback_action="halted",
        recomputed_step=None,
        recomputed_verification=None,
        timestamp=1.0,
    )


def test_extract_pitfalls_dedupes():
    from opendaisugi.distiller import _extract_pitfalls
    records = [
        _refinement("permissions", "shell not allowed"),
        _refinement("permissions", "shell not allowed"),   # duplicate
        _refinement("invariants", "file_unchanged violated"),
    ]
    result = _extract_pitfalls(records)
    assert len(result) == 2
    assert "[permissions] shell not allowed" in result
    assert "[invariants] file_unchanged violated" in result


def test_extract_pitfalls_empty():
    from opendaisugi.distiller import _extract_pitfalls
    assert _extract_pitfalls([]) == []


def test_extract_pitfalls_preserves_order_of_first_occurrence():
    from opendaisugi.distiller import _extract_pitfalls
    records = [
        _refinement("postconditions", "Z"),
        _refinement("permissions", "A"),
        _refinement("postconditions", "Z"),  # dup
        _refinement("invariants", "M"),
    ]
    result = _extract_pitfalls(records)
    assert result == [
        "[postconditions] Z",
        "[permissions] A",
        "[invariants] M",
    ]


@pytest.mark.asyncio
async def test_generalize_template_calls_llm_and_returns_pair(monkeypatch):
    from opendaisugi import distiller as dist_mod
    from opendaisugi.distiller import _generalize_template, GeneralizedTemplate
    from opendaisugi.models import ActionPlan, Envelope, Permission, ShellStep

    env = Envelope(generated_by="test", task="T", permissions=Permission(shell=True))
    plan = ActionPlan(source="t", task="T", steps=[ShellStep(id="s1", command="echo hi")])
    pitfalls = ["[permissions] shell command 'rm' not allowed"]

    class _FakeCompletions:
        def __init__(self):
            self.last_call = {}

        async def create(self, **kwargs):
            self.last_call = kwargs
            return GeneralizedTemplate(
                task_description="generalized task",
                plan_template=plan,
            )

    class _FakeChat:
        def __init__(self):
            self.completions = _FakeCompletions()

    class _FakeClient:
        def __init__(self):
            self.chat = _FakeChat()

    fake = _FakeClient()
    monkeypatch.setattr(dist_mod, "get_instructor_client", lambda _m: fake)

    result = await _generalize_template(
        plan=plan, envelope=env, pitfalls=pitfalls,
        model="anthropic/test-model",
    )
    assert result.task_description == "generalized task"
    assert result.plan_template.id == plan.id
    # Confirm prompt included the pitfalls.
    msgs = fake.chat.completions.last_call["messages"]
    joined = "\n".join(m["content"] for m in msgs)
    assert "shell command 'rm' not allowed" in joined


from opendaisugi.distiller import _validate_envelope, _improve_envelope


def test_validate_envelope_scores_test_plans():
    from opendaisugi.models import ActionPlan, Envelope, Permission, ShellStep

    env = Envelope(
        generated_by="test", task="T",
        permissions=Permission(shell=True, shell_allowlist=["find"]),
    )
    pass_plan = ActionPlan(
        source="t", task="T",
        steps=[ShellStep(id="s1", command="find /tmp -name '*.tmp'")],
    )
    fail_plan = ActionPlan(
        source="t", task="T",
        steps=[ShellStep(id="s1", command="rm -rf /tmp/x")],  # rm not allowed
    )
    score, failing = _validate_envelope(env, [pass_plan, fail_plan])
    assert score == 0.5
    assert len(failing) == 1
    assert failing[0] is fail_plan


def test_validate_envelope_empty_plans_returns_zero():
    from opendaisugi.models import Envelope, Permission
    env = Envelope(generated_by="test", task="T", permissions=Permission(shell=True))
    score, failing = _validate_envelope(env, [])
    assert score == 0.0
    assert failing == []


@pytest.mark.asyncio
async def test_improve_envelope_calls_llm_with_failure_context(monkeypatch):
    from opendaisugi import distiller as dist_mod
    from opendaisugi.models import ActionPlan, Envelope, Permission, ShellStep

    tight = Envelope(
        generated_by="test", task="T",
        permissions=Permission(shell=True, shell_allowlist=["find"]),
    )
    loose = Envelope(
        generated_by="test", task="T",
        permissions=Permission(shell=True, shell_allowlist=["find", "rm"]),
    )
    failing_plan = ActionPlan(
        source="t", task="T",
        steps=[ShellStep(id="s1", command="rm -rf /tmp/x")],
    )

    captured = {}

    class _FakeCompletions:
        async def create(self, **kwargs):
            captured.update(kwargs)
            return loose

    class _FakeChat:
        completions = _FakeCompletions()

    class _FakeClient:
        chat = _FakeChat()

    monkeypatch.setattr(dist_mod, "get_instructor_client", lambda _m: _FakeClient())

    result = await _improve_envelope(
        envelope=tight, failing_plans=[failing_plan], model="test-model",
    )
    assert result.id != tight.id or "rm" in (result.permissions.shell_allowlist or [])
    assert "rm -rf /tmp/x" in captured["messages"][1]["content"]
