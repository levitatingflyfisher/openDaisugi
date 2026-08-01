"""Prometheus metrics exporter — the corporate observability tier.

Corporations that adopt a tool for observability expect to scrape it with the
stack they already run. So this exposes openDaisugi's numbers in the Prometheus
text exposition format (version 0.0.4) — the lingua franca every Prometheus,
Grafana, Datadog, and VictoriaMetrics scraper reads — at ``/metrics``.

Two deliberate choices keep it cheap and clean:

* **No new dependency.** The exposition text is hand-built and served over
  stdlib ``http.server``; we do not pull ``prometheus_client``. The scraper only
  cares about the wire format, not our libraries.
* **No bundled Grafana.** We ship the numbers; the company points its own
  Grafana at them. An MIT tool exposing a metrics endpoint that a separate
  Grafana scrapes carries no AGPL obligation (process/network boundary).

Every number comes from :func:`opendaisugi.dashboard.read_raw`, the same
read-only reader the dashboard shows — so the exported series and the on-screen
gauges can never disagree. A store that is absent simply emits no series for it
(the honest Prometheus behaviour), and ``daisugi_up`` is always present so a
scrape never comes back empty. OTLP/OpenTelemetry push is a future addition; the
pull endpoint is the broadest-compatibility first step.
"""

from __future__ import annotations

from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path

from opendaisugi.dashboard import RawMetrics, read_raw

_CONTENT_TYPE = "text/plain; version=0.0.4; charset=utf-8"


def _fmt(value: float | int) -> str:
    if isinstance(value, bool):  # bool is an int subclass — keep 0/1
        return "1" if value else "0"
    if isinstance(value, int):
        return str(value)
    return repr(float(value))


def _metric(lines: list[str], name: str, mtype: str, help_text: str, samples) -> None:
    lines.append(f"# HELP {name} {help_text}")
    lines.append(f"# TYPE {name} {mtype}")
    for labels, value in samples:
        label_str = ""
        if labels:
            inner = ",".join(f'{k}="{v}"' for k, v in labels.items())
            label_str = "{" + inner + "}"
        lines.append(f"{name}{label_str} {_fmt(value)}")


def render_prometheus(data_dir: Path) -> str:
    """Render the current metrics as Prometheus text exposition (0.0.4)."""
    raw: RawMetrics = read_raw(Path(data_dir))
    lines: list[str] = []

    _metric(lines, "daisugi_up", "gauge", "1 if the daisugi exporter responded.", [(None, 1)])

    if raw.journal_total is not None:
        _metric(
            lines,
            "daisugi_journal_traces",
            "gauge",
            "Verified traces recorded in the journal, by outcome.",
            [
                ({"status": "passed"}, raw.journal_passed),
                ({"status": "failed"}, raw.journal_failed),
                ({"status": "total"}, raw.journal_total),
            ],
        )

    if raw.pathway_count is not None:
        _metric(
            lines,
            "daisugi_pathways",
            "gauge",
            "Distilled pathways stored.",
            [(None, raw.pathway_count)],
        )
        _metric(
            lines,
            "daisugi_pathway_reuse_hits_total",
            "counter",
            "Lifetime pathway reuse hits.",
            [(None, raw.pathway_hits)],
        )

    if raw.gateway_turns is not None:
        _metric(
            lines,
            "daisugi_gateway_turns_total",
            "counter",
            "Gateway turns routed.",
            [(None, raw.gateway_turns)],
        )
        _metric(
            lines,
            "daisugi_gateway_downgraded_turns_total",
            "counter",
            "Gateway turns routed to a cheaper model than requested.",
            [(None, raw.gateway_downgraded)],
        )
        _metric(
            lines,
            "daisugi_gateway_frontier_tokens_saved_total",
            "counter",
            "Tokens kept off the frontier model by routing.",
            [(None, raw.gateway_frontier_tokens_saved)],
        )
        _metric(
            lines,
            "daisugi_gateway_dollars_saved",
            "gauge",
            "Estimated dollars saved by routing (best-effort).",
            [(None, raw.gateway_dollars_saved)],
        )
        _metric(
            lines,
            "daisugi_gateway_blended_multiplier",
            "gauge",
            "Blended cost multiplier versus all-frontier routing.",
            [(None, raw.gateway_multiplier)],
        )
        _metric(
            lines,
            "daisugi_gateway_cache_hit_rate",
            "gauge",
            "Share of input tokens served from cache (0..1).",
            [(None, raw.gateway_cache_hit_rate)],
        )

    return "\n".join(lines) + "\n"


def make_metrics_server(data_dir: Path, host: str = "127.0.0.1", port: int = 9188) -> HTTPServer:
    """Build (but do not start) an HTTP server that serves ``/metrics``.

    Port 0 binds an ephemeral port (read it back from ``server_address``).
    """
    dd = Path(data_dir)

    class _Handler(BaseHTTPRequestHandler):
        def do_GET(self) -> None:  # noqa: N802 - http.server's required name
            if self.path.rstrip("/") in ("", "/metrics"):
                body = render_prometheus(dd).encode("utf-8")
                self.send_response(200)
                self.send_header("Content-Type", _CONTENT_TYPE)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)
            else:
                self.send_response(404)
                self.end_headers()

        def log_message(self, *args) -> None:  # silence per-request stderr logging
            pass

    return HTTPServer((host, port), _Handler)


def serve_metrics(data_dir: Path, *, host: str = "127.0.0.1", port: int = 9188) -> None:
    """Serve ``/metrics`` until interrupted (the pull endpoint a scraper hits)."""
    make_metrics_server(data_dir, host=host, port=port).serve_forever()
