"""v0.27.0 — alias registration + vacuity verdicts land in the RefinementLog."""
from __future__ import annotations

from opendaisugi.aliases import Alias, AliasRegistry
from opendaisugi.predicate import parse_expression


def test_alias_registration_records_provenance(tmp_path):
    from opendaisugi.journal import Journal
    j = Journal(data_dir=tmp_path)
    reg = AliasRegistry(refinement_sink=j)
    expr = parse_expression({"op": "forall_steps",
                             "pred": {"op": "equals", "path": "type", "value": "shell"}})
    reg.register(Alias(name="only_shell", expr=expr, tier="household"))
    records = j.get_provenance()
    assert any(r.detail.get("alias") == "only_shell"
               and r.detail.get("vacuity") == "non_trivial" for r in records)


def test_journal_write_failure_does_not_crash_register(monkeypatch, tmp_path):
    from opendaisugi.journal import Journal
    j = Journal(data_dir=tmp_path)
    monkeypatch.setattr(j, "write_provenance", lambda *a, **k: (_ for _ in ()).throw(IOError("disk")))
    reg = AliasRegistry(refinement_sink=j)
    expr = parse_expression({"op": "forall_steps",
                             "pred": {"op": "equals", "path": "type", "value": "shell"}})
    reg.register(Alias(name="ok", expr=expr, tier="household"))  # must NOT raise
