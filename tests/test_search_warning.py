"""The [search]-extra missing warning must fire through warnings.warn so it's
visible without any logging configuration — a plain `import warnings; warnings.warn`
user would see it, but a silent log.warning they wouldn't.
"""
from __future__ import annotations

import warnings
from unittest import mock

from opendaisugi.pathway_store import PathwayStore


def test_find_emits_user_warning_when_search_extra_missing(tmp_path):
    store = PathwayStore(tmp_path / "p.db")

    # Seed one pathway so the store isn't empty and we reach the embed call.
    import time

    from opendaisugi.models import (
        ActionPlan,
        Envelope,
        Permission,
        ShellStep,
    )
    from opendaisugi.pathway import CompiledPathway

    env = Envelope(generated_by="t", task="T", permissions=Permission(shell=True))
    plan = ActionPlan(source="t", task="T", steps=[ShellStep(id="s1", command="ls")])
    store.put(CompiledPathway(
        id="p1", task_description="T", task_embedding=[1.0],
        envelope=env, plan_template=plan, source_trace_ids=[], distilled_at=time.time(),
    ))

    with mock.patch.object(store, "_embed_query", side_effect=ImportError("no st")):
        with warnings.catch_warnings(record=True) as caught:
            warnings.simplefilter("always")
            result = store.find("anything")

    assert result is None
    user_warnings = [w for w in caught if issubclass(w.category, UserWarning)]
    assert user_warnings, "expected a UserWarning when [search] extra is missing"
    msg = str(user_warnings[0].message)
    assert "opendaisugi[search]" in msg
    assert "uv add" in msg


def test_find_warning_fires_at_most_once(tmp_path):
    """_warn_search_extra_missing_once guard works even with warnings.warn."""
    import opendaisugi.pathway_store as ps_mod
    ps_mod._search_extra_warned = False  # reset module-level guard

    store = PathwayStore(tmp_path / "p.db")
    import time

    from opendaisugi.models import (
        ActionPlan,
        Envelope,
        Permission,
        ShellStep,
    )
    from opendaisugi.pathway import CompiledPathway

    env = Envelope(generated_by="t", task="T", permissions=Permission(shell=True))
    plan = ActionPlan(source="t", task="T", steps=[ShellStep(id="s1", command="ls")])
    store.put(CompiledPathway(
        id="p1", task_description="T", task_embedding=[1.0],
        envelope=env, plan_template=plan, source_trace_ids=[], distilled_at=time.time(),
    ))

    with mock.patch.object(store, "_embed_query", side_effect=ImportError("no st")):
        with warnings.catch_warnings(record=True) as caught:
            warnings.simplefilter("always")
            store.find("a")
            store.find("b")
            store.find("c")

    user_warnings = [w for w in caught if issubclass(w.category, UserWarning)]
    assert len(user_warnings) == 1, "warning should fire exactly once per process"

    ps_mod._search_extra_warned = False  # clean up
