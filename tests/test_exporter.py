"""Tests for the Prometheus metrics exporter (`daisugi metrics`).

The exporter emits the Prometheus text exposition format (0.0.4) — the thing a
corporation's own Prometheus/Grafana/Datadog scrapes — over stdlib http.server,
with no new dependency and no bundled Grafana. It reads the same read_raw()
numbers the dashboard shows, so display and export never disagree.
"""

import threading
import urllib.request

from opendaisugi.exporter import make_metrics_server, render_prometheus


def _seed_gateway(data_dir, *, input_tokens, output_tokens):
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


def test_empty_data_dir_still_exposes_up_and_is_well_formed(tmp_path):
    text = render_prometheus(tmp_path)
    # a scrape always yields at least the health series
    assert "daisugi_up 1" in text
    assert text.endswith("\n")  # exposition must end with a newline
    # no store -> no store series (a scraper simply sees no such metric)
    assert "daisugi_gateway" not in text
    # every emitted sample line has a preceding TYPE line for its metric
    _assert_types_precede_samples(text)


def test_gateway_numbers_are_exported(tmp_path):
    _seed_gateway(tmp_path, input_tokens=1000, output_tokens=117)
    _seed_gateway(tmp_path, input_tokens=1000, output_tokens=117)
    text = render_prometheus(tmp_path)
    assert "daisugi_gateway_frontier_tokens_saved_total 2234" in text
    assert "# TYPE daisugi_gateway_frontier_tokens_saved_total counter" in text
    assert "daisugi_gateway_turns_total 2" in text
    _assert_types_precede_samples(text)


def test_http_server_serves_metrics(tmp_path):
    srv = make_metrics_server(tmp_path, host="127.0.0.1", port=0)
    port = srv.server_address[1]
    t = threading.Thread(target=srv.serve_forever, daemon=True)
    t.start()
    try:
        with urllib.request.urlopen(f"http://127.0.0.1:{port}/metrics", timeout=5) as resp:
            assert resp.status == 200
            assert "version=0.0.4" in resp.headers.get("Content-Type", "")
            body = resp.read().decode()
        assert "daisugi_up 1" in body
    finally:
        srv.shutdown()
        t.join(timeout=5)


def _assert_types_precede_samples(text: str) -> None:
    typed = set()
    for line in text.splitlines():
        if line.startswith("# TYPE "):
            typed.add(line.split()[2])
        elif line and not line.startswith("#"):
            name = line.split("{")[0].split(" ")[0]
            assert name in typed, f"sample {name!r} has no # TYPE line before it"
