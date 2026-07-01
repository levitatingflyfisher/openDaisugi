"""v0.27.0 — Z3-backed tautology/contradiction detection on predicates."""
from __future__ import annotations

from opendaisugi.predicate import parse_expression
from opendaisugi.vacuity import check_vacuity, clear_vacuity_cache


def _expr(d):
    return parse_expression(d)


def test_tautology_detected():
    # forall_steps( equals(type,'shell') OR not_equals(type,'shell') ) — always true
    e = _expr({"op": "forall_steps", "pred": {
        "op": "or", "children": [
            {"op": "equals", "path": "type", "value": "shell"},
            {"op": "not_equals", "path": "type", "value": "shell"},
        ]}})
    assert check_vacuity(e) == "tautology"


def test_contradiction_detected():
    e = _expr({"op": "forall_steps", "pred": {
        "op": "and", "children": [
            {"op": "equals", "path": "type", "value": "shell"},
            {"op": "not_equals", "path": "type", "value": "shell"},
        ]}})
    assert check_vacuity(e) == "contradiction"


def test_nontrivial_predicate():
    e = _expr({"op": "forall_steps",
               "pred": {"op": "equals", "path": "type", "value": "shell"}})
    assert check_vacuity(e) == "non_trivial"


# v0.28.6 — L1: vacuity result is memoized per (expr, timeout). Verify()
# typically calls check_vacuity 3-5x per envelope; the cache turns
# repeated calls on structurally-identical predicates into a dict
# lookup instead of a Z3 SAT pair.


def test_vacuity_result_is_cached_on_repeat_call():
    """Same expr twice → second call returns the cached verdict without
    instantiating a Z3 solver. We probe by patching _compute_vacuity to
    raise on the second call; the cache hit must skip the recompute."""
    from opendaisugi import vacuity as vac_mod

    clear_vacuity_cache()
    e = _expr({"op": "forall_steps",
               "pred": {"op": "equals", "path": "type", "value": "shell"}})
    first = check_vacuity(e)
    assert first == "non_trivial"

    # Replace the compute path with a poison value; the cache hit must
    # bypass it. If the cache is broken, this raises.
    original = vac_mod._compute_vacuity

    def _poison(*a, **kw):
        raise AssertionError("cache miss — _compute_vacuity should not run on repeat")

    vac_mod._compute_vacuity = _poison
    try:
        second = check_vacuity(e)
    finally:
        vac_mod._compute_vacuity = original
    assert second == first


def test_vacuity_cache_keys_on_expr_shape_not_identity():
    """Two structurally-identical Expression instances must share a
    cache slot — the cache is keyed on model_dump_json, not Python id()."""
    from opendaisugi import vacuity as vac_mod

    clear_vacuity_cache()
    e1 = _expr({"op": "forall_steps",
                "pred": {"op": "equals", "path": "type", "value": "shell"}})
    e2 = _expr({"op": "forall_steps",
                "pred": {"op": "equals", "path": "type", "value": "shell"}})
    assert e1 is not e2  # different parser invocations
    check_vacuity(e1)

    original = vac_mod._compute_vacuity
    vac_mod._compute_vacuity = lambda *a, **kw: (_ for _ in ()).throw(
        AssertionError("equal exprs must hit cache"))
    try:
        check_vacuity(e2)
    finally:
        vac_mod._compute_vacuity = original


def test_vacuity_cache_keys_on_timeout_separately():
    """Different timeout values are SEPARATE cache entries — a longer
    budget can flip a 'non_trivial' (Z3 unknown) into a real verdict, so
    sharing a slot across budgets would be unsound."""
    from opendaisugi import vacuity as vac_mod

    clear_vacuity_cache()
    e = _expr({"op": "forall_steps",
               "pred": {"op": "equals", "path": "type", "value": "shell"}})
    check_vacuity(e, timeout_ms=500)

    # Different timeout → must miss the cache → recomputes.
    calls = {"n": 0}
    original = vac_mod._compute_vacuity

    def _counting(*a, **kw):
        calls["n"] += 1
        return original(*a, **kw)

    vac_mod._compute_vacuity = _counting
    try:
        check_vacuity(e, timeout_ms=200)
    finally:
        vac_mod._compute_vacuity = original
    assert calls["n"] == 1, "different timeout_ms must miss the cache"


def test_vacuity_cache_is_bounded():
    """LRU eviction at the configured size prevents a hostile caller
    from blowing memory by churning unique exprs."""
    from opendaisugi import vacuity as vac_mod

    clear_vacuity_cache()
    # Temporarily shrink the bound so the test is fast.
    original_max = vac_mod._VACUITY_CACHE_MAX
    vac_mod._VACUITY_CACHE_MAX = 4
    try:
        for i in range(10):
            check_vacuity(_expr({
                "op": "forall_steps",
                "pred": {"op": "equals", "path": "type", "value": f"shell_{i}"},
            }))
        assert len(vac_mod._VACUITY_CACHE) == 4
    finally:
        vac_mod._VACUITY_CACHE_MAX = original_max
        clear_vacuity_cache()
