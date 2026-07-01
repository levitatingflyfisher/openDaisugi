"""Tests for pathway portability: export/import round-trips + inspection formats."""

from __future__ import annotations

import json
import time

import pytest
from typer.testing import CliRunner

from opendaisugi.cli import app
from opendaisugi.models import (
    ActionPlan,
    Envelope,
    Invariant,
    Permission,
    ShellStep,
)
from opendaisugi.pathway import CompiledPathway
from opendaisugi.pathway_store import PathwayStore
from opendaisugi.portability import (
    BUNDLE_SCHEMA_VERSION,
    PathwayImportError,
    export,
    import_pathway,
    parse_bundle,
)


def _pathway(id_: str = "pathway_abc12345") -> CompiledPathway:
    env = Envelope(
        generated_by="test",
        task="delete stale .tmp files",
        permissions=Permission(shell=True, shell_allowlist=["find"]),
        invariants=[Invariant(type="no_side_effects", description="read-only dry-run")],
    )
    plan = ActionPlan(
        source="tmpl",
        task="delete stale .tmp files",
        steps=[
            ShellStep(id="s1", command="find /tmp -name '*.tmp' -mtime +7"),
            ShellStep(id="s2", command="find /tmp -name '*.log'", depends_on=["s1"]),
        ],
    )
    return CompiledPathway(
        id=id_,
        task_description="delete stale .tmp files",
        task_embedding=[0.1, 0.2, 0.3],
        embedding_model="stub",
        envelope=env,
        plan_template=plan,
        source_trace_ids=["t1", "t2"],
        distilled_at=time.time(),
    )


def test_export_md_renders_orchestration_step_detail():
    # A pathway containing orchestration step types must not export blank details.
    from opendaisugi.models import MCPStep, SkillStep, TaskStep
    env = Envelope(generated_by="t", task="x", permissions=Permission())
    plan = ActionPlan(source="tmpl", task="x", steps=[
        TaskStep(id="t1", prompt="analyze the quarterly numbers"),
        SkillStep(id="k1", skill_id="tidy-inbox", depends_on=["t1"]),
        MCPStep(id="m1", server="github", tool="create_issue", depends_on=["k1"]),
    ])
    pw = CompiledPathway(
        id="pathway_orch1234", task_description="x", task_embedding=[0.1],
        embedding_model="stub", envelope=env, plan_template=plan,
        source_trace_ids=["t1"], distilled_at=time.time(),
    )
    md = export(pw, "md")
    assert "analyze the quarterly numbers" in md
    assert "tidy-inbox" in md
    assert "github/create_issue" in md


# ─────────────────── JSON round-trip ───────────────────


def test_export_json_is_valid_json_and_versioned():
    p = _pathway()
    text = export(p, "json")
    data = json.loads(text)
    assert data["schema_version"] == BUNDLE_SCHEMA_VERSION
    assert "opendaisugi_version" in data
    assert data["pathway"]["id"] == p.id


def test_json_round_trip_preserves_pathway():
    p = _pathway()
    text = export(p, "json")
    restored = parse_bundle(text)
    assert restored.id == p.id
    assert restored.envelope.permissions.shell_allowlist == ["find"]
    assert len(restored.plan_template.steps) == 2
    assert restored.source_trace_ids == ["t1", "t2"]


# ─────────────────── Skill round-trip ───────────────────


def test_export_skill_has_yaml_frontmatter_and_body():
    p = _pathway()
    text = export(p, "skill")
    assert text.startswith("---\n")
    assert "\n---\n" in text
    assert "name:" in text
    assert "daisugi:" in text
    # Body contains the task description as heading
    assert "# delete stale .tmp files" in text


def test_skill_round_trip_preserves_pathway():
    p = _pathway()
    text = export(p, "skill")
    restored = parse_bundle(text)
    assert restored.id == p.id
    assert restored.envelope.permissions.shell_allowlist == ["find"]
    assert restored.plan_template.steps[1].depends_on == ["s1"]


# ─────────────────── Inspection formats ───────────────────


def test_export_mermaid_renders_dag_and_permissions():
    text = export(_pathway(), "mermaid")
    assert "flowchart TD" in text
    assert "s1" in text and "s2" in text
    assert "s1 --> s2" in text
    assert "### Permissions" in text
    assert "shell: True" in text


def test_export_md_contains_envelope_and_plan_sections():
    text = export(_pathway(), "md")
    assert "# Pathway:" in text
    assert "## Envelope" in text
    assert "## Plan template" in text
    assert "no_side_effects" in text  # invariant
    assert "find /tmp" in text  # step


def test_export_smtlib_is_parseable_sexpr():
    text = export(_pathway(), "smtlib")
    assert ";; openDaisugi pathway proof artifact" in text
    assert "(declare-fun shell" in text or "(declare-const shell" in text
    assert "(check-sat)" in text


# ─────────────────── Import from disk ───────────────────


def test_import_pathway_verifies_and_stores(tmp_path):
    p = _pathway(id_="p_import_ok")
    src = tmp_path / "bundle.json"
    src.write_text(export(p, "json"))
    store = PathwayStore(tmp_path / "store.db")

    result = import_pathway(src, store)
    assert result.pathway.id == "p_import_ok"
    assert result.overwrote_existing is False
    assert len(store.list_all()) == 1


def test_import_skill_file_also_works(tmp_path):
    p = _pathway(id_="p_skill_import")
    src = tmp_path / "skill.md"
    src.write_text(export(p, "skill"))
    store = PathwayStore(tmp_path / "store.db")

    result = import_pathway(src, store)
    assert result.pathway.id == "p_skill_import"


def test_import_rejects_unverifiable_pathway(tmp_path):
    """Mutate the plan to reference a command outside the envelope allowlist."""
    p = _pathway(id_="p_bad")
    # Build a tampered bundle: envelope says allowlist=[find], plan says `rm`.
    data = json.loads(export(p, "json"))
    data["pathway"]["plan_template"]["steps"][0]["command"] = "rm -rf /tmp/*"
    src = tmp_path / "bad.json"
    src.write_text(json.dumps(data))

    store = PathwayStore(tmp_path / "store.db")
    with pytest.raises(PathwayImportError) as exc:
        import_pathway(src, store)
    assert exc.value.code == "VERIFICATION_FAILED"
    assert store.list_all() == []


def test_import_rejects_schema_newer_than_library(tmp_path):
    p = _pathway(id_="p_future")
    data = json.loads(export(p, "json"))
    data["schema_version"] = BUNDLE_SCHEMA_VERSION + 99
    src = tmp_path / "future.json"
    src.write_text(json.dumps(data))

    store = PathwayStore(tmp_path / "store.db")
    with pytest.raises(PathwayImportError) as exc:
        import_pathway(src, store)
    assert exc.value.code == "SCHEMA_INCOMPATIBLE"


def test_import_rejects_non_json_non_skill(tmp_path):
    src = tmp_path / "junk.txt"
    src.write_text("this is just prose, not a bundle")
    store = PathwayStore(tmp_path / "store.db")
    with pytest.raises(PathwayImportError) as exc:
        import_pathway(src, store)
    assert exc.value.code == "SCHEMA_INCOMPATIBLE"


def test_import_duplicate_id_rejected_without_overwrite(tmp_path):
    p = _pathway(id_="p_dup")
    src = tmp_path / "b.json"
    src.write_text(export(p, "json"))
    store = PathwayStore(tmp_path / "store.db")
    import_pathway(src, store)
    with pytest.raises(PathwayImportError) as exc:
        import_pathway(src, store)
    assert exc.value.code == "DUPLICATE_ID"


def test_import_overwrite_flag_replaces_existing(tmp_path):
    p = _pathway(id_="p_replace")
    src = tmp_path / "b.json"
    src.write_text(export(p, "json"))
    store = PathwayStore(tmp_path / "store.db")
    import_pathway(src, store)
    result = import_pathway(src, store, allow_overwrite=True)
    assert result.overwrote_existing is True
    assert len(store.list_all()) == 1


# ─────────────────── CLI surface ───────────────────


runner = CliRunner()


def _seed_store(tmp_path) -> CompiledPathway:
    store = PathwayStore(tmp_path / "pathways.db")
    p = _pathway(id_="p_cli_seed")
    store.put(p)
    return p


def test_cli_export_writes_skill_file(tmp_path):
    _seed_store(tmp_path)
    out = tmp_path / "out.md"
    r = runner.invoke(
        app,
        ["pathways", "export", "p_cli_seed", str(out),
         "--format", "skill", "--data-dir", str(tmp_path)],
    )
    assert r.exit_code == 0, r.output
    assert out.exists()
    assert "daisugi:" in out.read_text()


def test_cli_export_rejects_unknown_format(tmp_path):
    _seed_store(tmp_path)
    r = runner.invoke(
        app,
        ["pathways", "export", "p_cli_seed", str(tmp_path / "x"),
         "--format", "xml", "--data-dir", str(tmp_path)],
    )
    assert r.exit_code == 2
    assert "Unknown format" in r.output


def test_cli_export_unknown_pathway_exits_1(tmp_path):
    _seed_store(tmp_path)
    r = runner.invoke(
        app,
        ["pathways", "export", "nonexistent", str(tmp_path / "o.json"),
         "--format", "json", "--data-dir", str(tmp_path)],
    )
    assert r.exit_code == 1


def test_cli_import_roundtrip(tmp_path):
    # Seed store A, export, wipe store, re-import, confirm.
    store = PathwayStore(tmp_path / "pathways.db")
    store.put(_pathway(id_="p_roundtrip"))

    out = tmp_path / "pkg.json"
    r = runner.invoke(
        app,
        ["pathways", "export", "p_roundtrip", str(out),
         "--format", "json", "--data-dir", str(tmp_path)],
    )
    assert r.exit_code == 0, r.output

    # Wipe via the CLI so the delete path is exercised too.
    store.delete("p_roundtrip")
    assert store.list_all() == []

    r = runner.invoke(
        app,
        ["pathways", "import", str(out), "--data-dir", str(tmp_path)],
    )
    assert r.exit_code == 0, r.output
    assert len(PathwayStore(tmp_path / "pathways.db").list_all()) == 1
