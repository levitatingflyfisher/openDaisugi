"""Tests for the live 'factory floor' view (`daisugi dashboard`).

The dashboard is a live-metrics *skin* over the same `detect_stages` map that
`daisugi modules` renders. It must: read the real local stores (journal,
pathway, gateway) with zero fabrication, stay read-only (no store is created as
a side effect of looking), degrade gracefully when a store is empty or absent,
and emit a JSON that is a strict superset of `wiring_json` so there is one
contract for a future exporter to read.
"""

import io
import json

from opendaisugi.dashboard import (
    collect_metrics,
    dashboard_json,
    render_dashboard,
    run_live,
)
from opendaisugi.modules import wiring_json


class FakeStream:
    """A stdout stand-in with a controllable isatty()."""

    def __init__(self, *, tty: bool) -> None:
        self._buf = io.StringIO()
        self._tty = tty

    def write(self, s: str) -> int:
        return self._buf.write(s)

    def flush(self) -> None:
        pass

    def isatty(self) -> bool:
        return self._tty

    def getvalue(self) -> str:
        return self._buf.getvalue()


def _seed_gateway(data_dir, *, input_tokens: int, output_tokens: int) -> None:
    """Append one downgraded turn to the gateway journal at ``data_dir``.

    ``summarize`` derives the headline "frontier tokens saved" from a downgraded
    turn's own input+output tokens (the work that ran on a cheaper model instead
    of the frontier), not from the per-record ``frontier_tokens_saved`` field —
    so seed the token counts and let the summary do the arithmetic.
    """
    from opendaisugi.gateway_journal import GatewayJournal, GatewayTurnRecord

    GatewayJournal(path=data_dir / "gateway" / "turns.jsonl").append(
        GatewayTurnRecord(
            created_at="2026-08-22T00:00:00+00:00",
            signature="sig",
            task="t",
            tier="frontier",
            requested_model="big",
            model="small",
            difficulty=0.2,
            downgraded=True,
            estimated=False,
            input_tokens=input_tokens,
            output_tokens=output_tokens,
            frontier_tokens_saved=0,
            actual_dollars=0.01,
            counterfactual_dollars=0.05,
        )
    )


def test_collect_metrics_empty_data_dir_degrades_not_raises(tmp_path):
    m = collect_metrics(tmp_path)
    assert isinstance(m, dict)
    for key in ("stores", "matcher", "router"):
        assert key in m and m[key], f"stage {key} has no gauge"
    # No store present -> a blank gauge, never a fabricated number.
    assert m["router"][0].value == "—"


def test_collect_metrics_is_read_only(tmp_path):
    # Looking must not conjure stores into being.
    collect_metrics(tmp_path)
    assert not (tmp_path / "journal").exists()
    assert not (tmp_path / "pathways.db").exists()
    assert not (tmp_path / "gateway").exists()


def test_router_gauge_reflects_seeded_gateway_journal(tmp_path):
    # Two downgraded turns, each 1000 in + 117 out -> 2 * 1117 = 2,234 frontier
    # tokens that ran on a cheaper model instead of the frontier.
    _seed_gateway(tmp_path, input_tokens=1000, output_tokens=117)
    _seed_gateway(tmp_path, input_tokens=1000, output_tokens=117)
    router = collect_metrics(tmp_path)["router"][0]
    assert "2,234" in router.value, router.value  # summarized frontier tokens saved
    assert "$" in router.detail  # dollars alongside
    assert "2/2 downgraded" in router.detail


def test_render_dashboard_ascii_reuses_module_map_and_has_flow(tmp_path):
    art = render_dashboard(tmp_path)
    assert "legend:" in art
    # a real stage title from detect_stages, not an invented one
    assert "matcher" in art
    # a live gauge label is surfaced under the stages
    assert "tokens saved" in art or "traces" in art


def test_unmetered_stage_states_why_it_is_blank(tmp_path):
    m = collect_metrics(tmp_path)
    gate = " ".join(g.detail for g in m.get("gate", []))
    ver = " ".join(g.detail for g in m.get("verifier", []))
    assert "no passive counter" in gate
    assert "benchmark" in ver


def test_dashboard_json_is_superset_of_wiring(tmp_path):
    wire = json.loads(wiring_json(tmp_path))
    dash = json.loads(dashboard_json(tmp_path))
    assert len(dash) == len(wire)
    wire_keys = set(wire[0])
    for stage in dash:
        assert wire_keys <= set(stage), "dashboard JSON dropped a wiring field"
        assert "metrics" in stage, "dashboard JSON must add a metrics key"


def test_run_live_non_tty_emits_exactly_one_frame(tmp_path):
    s = FakeStream(tty=False)
    # iterations is ignored when the stream is not a tty — one frame, then return.
    run_live(tmp_path, stream=s, iterations=99)
    out = s.getvalue()
    assert out.count("legend:") == 1


def test_run_live_tty_bounded_iterations(tmp_path):
    s = FakeStream(tty=True)
    slept: list[float] = []
    run_live(tmp_path, interval=0.0, iterations=3, stream=s, sleep=slept.append)
    assert s.getvalue().count("legend:") == 3
    # sleeps only *between* frames, never after the last
    assert len(slept) == 2
