"""The live 'factory floor': the module wiring with real throughput gauges.

``daisugi modules`` draws the static pipeline; this makes it live. Each stage
carries the numbers actually recorded on this box — traces journaled, pathways
reused, frontier tokens the router kept off the expensive model — polled from
the same local stores the rest of Daisugi reads. One numeric reader
(:func:`read_raw`) is the single source of truth — the display gauges
(:func:`collect_metrics`), the JSON, and the Prometheus exporter all derive from
it; the map itself is always :func:`detect_stages`, so nothing drifts.

Three rules keep it honest, the same instinct behind the modules view:

* **Read-only.** Looking never conjures a store into being — every read is
  gated on the file already existing, so a 2-second refresh loop writes nothing.
* **No fabrication.** A stage with no passive counter (the gate's per-call
  throughput, the verifier's latency) shows a blank gauge that *says why* it is
  blank, rather than an invented number or a silent gap.
* **Graceful.** A missing or unreadable store degrades to a dash, never a raise;
  a dashboard must not be the thing that falls over.
"""

from __future__ import annotations

import json
import time
from dataclasses import asdict, dataclass
from pathlib import Path
from typing import Callable, TextIO

from opendaisugi.modules import _STATE_KEY, detect_stages


@dataclass
class Gauge:
    """One live reading for a stage. ``value`` is pre-formatted for display."""

    label: str
    value: str
    detail: str = ""
    fraction: float | None = None  # 0..1 for a bar, or None when not a ratio

    def as_dict(self) -> dict:
        return asdict(self)


def _blank(label: str, why: str) -> Gauge:
    return Gauge(label, "—", why)


def _ratio(num: int, denom: int) -> float | None:
    return (num / denom) if denom else None


# --- raw store reads (ONE place; both the display gauges and the Prometheus
# exporter derive from these numbers, so there is a single source of truth) ---


@dataclass
class RawMetrics:
    """Numeric readings straight off the stores. ``None`` = store absent/unreadable."""

    journal_total: int | None = None
    journal_passed: int | None = None
    journal_failed: int | None = None
    pathway_count: int | None = None
    pathway_hits: int | None = None
    gateway_turns: int | None = None
    gateway_downgraded: int | None = None
    gateway_frontier_tokens_saved: int | None = None
    gateway_dollars_saved: float | None = None
    gateway_multiplier: float | None = None
    gateway_cache_hit_rate: float | None = None


def read_raw(data_dir: Path) -> RawMetrics:
    """Read every store once, read-only and best-effort. Never raises."""
    data_dir = Path(data_dir)
    raw = RawMetrics()

    if (data_dir / "journal" / "index.db").exists():
        try:
            from opendaisugi.journal import Journal

            j = Journal(data_dir=data_dir)
            try:
                st = j.stats()
            finally:
                j.close()
            raw.journal_total, raw.journal_passed, raw.journal_failed = (
                st.total,
                st.passed,
                st.failed,
            )
        except Exception:
            pass

    pdb = data_dir / "pathways.db"
    if pdb.exists():
        try:
            from opendaisugi.pathway_store import PathwayStore

            s = PathwayStore(pdb).stats()
            raw.pathway_count = int(s.get("count", 0) or 0)
            raw.pathway_hits = int(s.get("total_hits", 0) or 0)
        except Exception:
            pass

    try:
        from opendaisugi.gateway_journal import GatewayJournal, summarize

        recs = GatewayJournal(path=data_dir / "gateway" / "turns.jsonl").load()
        if recs:
            g = summarize(recs)
            raw.gateway_turns = g.turns
            raw.gateway_downgraded = g.downgraded_turns
            raw.gateway_frontier_tokens_saved = g.frontier_tokens_saved
            raw.gateway_dollars_saved = g.dollars_saved
            raw.gateway_multiplier = g.blended_multiplier
            raw.gateway_cache_hit_rate = g.cache_hit_rate
    except Exception:
        pass

    return raw


def collect_metrics(data_dir: Path) -> dict[str, list[Gauge]]:
    """Poll the live stores for per-stage gauges, keyed by ``Stage.key``.

    Read-only and best-effort: any store that is missing, empty, or unreadable
    yields a blank gauge carrying the reason. Never raises.
    """
    raw = read_raw(Path(data_dir))

    if raw.journal_total is None:
        stores = _blank("traces", "no journal yet — run `daisugi onboard`")
    else:
        stores = Gauge(
            "traces",
            f"{raw.journal_total:,}",
            f"{raw.journal_passed:,} passed · {raw.journal_failed:,} failed",
            fraction=_ratio(raw.journal_passed, raw.journal_total),
        )

    if raw.pathway_count is None:
        matcher = _blank("reuse hits", "no pathways yet — run `daisugi tend`")
        distill = _blank("pathways", "no pathways yet — run `daisugi tend`")
    else:
        matcher = Gauge(
            "reuse hits", f"{raw.pathway_hits:,}", f"{raw.pathway_count:,} pathway(s) stored"
        )
        distill = Gauge(
            "pathways", f"{raw.pathway_count:,}", f"{raw.pathway_hits:,} lifetime reuse hit(s)"
        )

    if raw.gateway_turns is None:
        router = _blank("tokens saved", "no gateway turns recorded yet")
    else:
        router = Gauge(
            "tokens saved",
            f"{raw.gateway_frontier_tokens_saved:,}",
            f"${raw.gateway_dollars_saved:,.2f} · {raw.gateway_multiplier:.2f}x · "
            f"cache {raw.gateway_cache_hit_rate:.0%} · "
            f"{raw.gateway_downgraded}/{raw.gateway_turns} downgraded",
            fraction=_ratio(raw.gateway_downgraded, raw.gateway_turns),
        )

    return {
        "harness": [Gauge("detected", "see map", "active harnesses marked ● above")],
        "gate": [_blank("throughput", "no passive counter yet")],
        "verifier": [_blank("latency", "benchmark-only — run `daisugi guard-cost`")],
        "matcher": [matcher],
        "distill": [distill],
        "router": [router],
        "stores": [stores],
    }


# --- renderers --------------------------------------------------------------


def _box_line(text: str, inner: int, g: dict[str, str]) -> str:
    return f"{g['v']} {text[:inner].ljust(inner)} {g['v']}"


def _gauge_text(gauge: Gauge, flow: str) -> str:
    core = f"{flow} {gauge.label} {gauge.value}"
    return f"{core}  ({gauge.detail})" if gauge.detail else core


def render_dashboard(
    data_dir: Path,
    *,
    width: int = 66,
    stages=None,
    metrics: dict[str, list[Gauge]] | None = None,
    pulse: int | None = None,
    plain: bool | None = None,
) -> str:
    """Render one live frame. ``stages`` may be pre-resolved (resolve the static
    wiring once, poll ``metrics`` each tick) — see :func:`run_live`."""
    from opendaisugi import console

    g = console.glyphs() if plain is None else (console.ASCII_BOX if plain else console.BOX)
    flow = g["flow"]
    if stages is None:
        stages = detect_stages(data_dir)
    if metrics is None:
        metrics = collect_metrics(data_dir)

    inner = width - 4
    out: list[str] = [
        f"openDaisugi — live floor   (data dir: {data_dir})",
        "",
        "  a task from your agent",
    ]
    n = len(stages)
    for i, st in enumerate(stages):
        lit = pulse is not None and (pulse % n) == i
        out.append(f"        {g['v']}")
        out.append(f"        {g['down']} {g['on']}" if lit else f"        {g['down']}")
        head = f"{g['tl']}{g['h']} {st.title} "
        head = head + g["h"] * max(0, width - len(head) - 1) + g["tr"]
        out.append(head)
        out.append(_box_line(st.role, inner, g))
        # module row(s), active first, wrapped — same glyphs as the static map
        line = "  "
        for m in st.modules:
            chunk = f"{g[_STATE_KEY[m.state]]} {m.name}   "
            if len(line) + len(chunk) > inner:
                out.append(_box_line(line, inner, g))
                line = "  "
            line += chunk
        if line.strip():
            out.append(_box_line(line, inner, g))
        # live gauge row(s)
        for gauge in metrics.get(st.key, []):
            out.append(_box_line("  " + _gauge_text(gauge, flow), inner, g))
        out.append(g["bl"] + g["h"] * (width - 2) + g["br"])
    out.append(f"        {g['v']}")
    out.append(f"        {g['down']}")
    out.append("  verified action runs  (or falls back / is refused)")
    out.append("")
    out.append(
        f"legend:  {g['on']} active   {g['avail']} available   "
        f"{g['off']} possible    {flow} live gauge (read-only)"
    )
    return "\n".join(out)


def dashboard_json(data_dir: Path) -> str:
    """The wiring JSON with a live ``metrics`` list added per stage.

    A strict superset of :func:`opendaisugi.modules.wiring_json` — one contract
    for a future Prometheus/OTel exporter to read, not a parallel schema.
    """
    stages = detect_stages(data_dir)
    metrics = collect_metrics(data_dir)
    out = []
    for st in stages:
        d = asdict(st)
        d["metrics"] = [g.as_dict() for g in metrics.get(st.key, [])]
        out.append(d)
    return json.dumps(out, indent=2)


def run_live(
    data_dir: Path,
    *,
    interval: float = 2.0,
    iterations: int | None = None,
    stream: TextIO | None = None,
    sleep: Callable[[float], None] = time.sleep,
    clear: bool = True,
) -> None:
    """Render the live floor to ``stream``, refreshing every ``interval`` seconds.

    When the stream is not a TTY (piped, redirected, captured), emit a single
    frame and return — a live loop only makes sense on a real terminal. The
    static wiring is resolved once; only :func:`collect_metrics` runs per tick.
    """
    import sys

    from opendaisugi import console

    stream = stream or sys.stdout
    if not stream.isatty():
        stream.write(render_dashboard(data_dir) + "\n")
        return

    stages = detect_stages(data_dir)  # resolve the map once (advisor: not per tick)
    i = 0
    while iterations is None or i < iterations:
        frame = render_dashboard(
            data_dir, stages=stages, metrics=collect_metrics(data_dir), pulse=i
        )
        if clear and not console.current().plain:
            stream.write("\x1b[H\x1b[2J")  # cursor home + clear screen
        stream.write(frame + "\n")
        stream.flush()
        i += 1
        if iterations is not None and i >= iterations:
            break
        sleep(interval)
