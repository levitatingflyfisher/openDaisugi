"""`tiers setup --endpoint`: the HTTP backend is pinned, a failed probe says why,
and the served context window is read from llama-server's own fields.

The box that found the bug had a `claude` binary on PATH. Auto-detection chose
the claude-code backend, which ignores the endpoint, so every probe failed in
2 s and the run printed 0/3 with no reason. These tests put a stub `claude` on
PATH and serve the endpoint from a localhost server. No model, no network.
"""

from __future__ import annotations

import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest
from typer.testing import CliRunner

from opendaisugi.cli import app

runner = CliRunner()

VALID = json.dumps(
    {
        "generated_by": "model",
        "task": "t",
        "permissions": {
            "file_read": ["/work/**"],
            "file_write": [],
            "network": False,
            "shell": False,
            "shell_allowlist": [],
            "max_execution_time_s": 30,
            "max_output_size_mb": 10,
        },
        "invariants": [],
        "postconditions": [],
    }
)

LLAMA_MODELS = {
    "object": "list",
    "data": [
        {
            "id": "gemma",
            "object": "model",
            "meta": {"n_vocab": 262144, "n_ctx": 16384, "n_ctx_train": 262144},
        }
    ],
}


class Server:
    """A localhost server. `routes` maps a path to (status, body, content-type)."""

    def __init__(self, routes):
        self.routes = routes
        self.hits: dict[str, int] = {}
        self.bodies: list[bytes] = []
        outer = self

        class H(BaseHTTPRequestHandler):
            def _answer(self):
                outer.hits[self.path] = outer.hits.get(self.path, 0) + 1
                n = int(self.headers.get("content-length") or 0)
                if n:
                    outer.bodies.append(self.rfile.read(n))
                status, body = outer.routes.get(self.path, (404, "{}"))
                data = body if isinstance(body, bytes) else str(body).encode()
                self.send_response(status)
                self.send_header("content-type", "application/json")
                self.send_header("content-length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

            do_GET = do_POST = _answer

            def log_message(self, *a):
                pass

        self.httpd = ThreadingHTTPServer(("127.0.0.1", 0), H)
        self.url = f"http://127.0.0.1:{self.httpd.server_port}"
        threading.Thread(target=self.httpd.serve_forever, daemon=True).start()

    def close(self):
        self.httpd.shutdown()
        self.httpd.server_close()


def chat(message: dict, finish: str = "stop") -> tuple[int, str]:
    return 200, json.dumps({"choices": [{"message": message, "finish_reason": finish}]})


@pytest.fixture
def box(tmp_path, monkeypatch):
    """A box with a stub `claude` first on PATH and no backend chosen."""
    bin_dir = tmp_path / "bin"
    bin_dir.mkdir()
    claude = bin_dir / "claude"
    claude.write_text("#!/bin/sh\nexit 1\n")
    claude.chmod(0o755)
    monkeypatch.setenv("PATH", str(bin_dir))
    for key in ("OPENDAISUGI_LLM_BACKEND", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"):
        monkeypatch.delenv(key, raising=False)
    monkeypatch.setattr("opendaisugi.DEFAULT_DATA_DIR", tmp_path / "home")
    servers: list[Server] = []

    def serve(routes):
        s = Server(routes)
        servers.append(s)
        return s

    yield serve
    for s in servers:
        s.close()


def setup(tmp_path, *args):
    return runner.invoke(app, ["tiers", "setup", "--data-dir", str(tmp_path / "d"), *args])


def test_endpoint_ignores_claude_on_path(box, tmp_path):
    srv = box({"/v1/chat/completions": chat({"role": "assistant", "content": VALID})})
    res = setup(tmp_path, "--endpoint", srv.url + "/v1", "--model", "m")
    assert res.exit_code == 0, res.output
    assert srv.hits["/v1/chat/completions"] == 3
    assert srv.hits["/api/tags"] == 1
    assert "PASSED" in res.output and "3/3" in res.output


def test_endpoint_refuses_forced_claude_code(box, tmp_path, monkeypatch):
    monkeypatch.setenv("OPENDAISUGI_LLM_BACKEND", "claude-code")
    srv = box({"/v1/chat/completions": chat({"role": "assistant", "content": VALID})})
    res = setup(tmp_path, "--endpoint", srv.url + "/v1", "--model", "m")
    assert res.exit_code != 0
    assert "--endpoint cannot be used with the claude-code backend" in res.output
    assert srv.hits == {}


def test_failed_probe_reports_reason_and_all_errored_hint(box, tmp_path):
    srv = box({"/v1/chat/completions": (500, "boom")})
    res = setup(tmp_path, "--endpoint", srv.url + "/v1", "--model", "m")
    assert res.exit_code == 0, res.output
    assert "FAILED" in res.output and "boom" in res.output
    assert "ALL attempts errored" in res.output
    js = setup(tmp_path, "--endpoint", srv.url + "/v1", "--model", "m", "--json")
    outcomes = json.loads(js.stdout)["qualification"]["outcomes"]
    assert len(outcomes) == 3
    assert outcomes[0]["task"] and outcomes[0]["outcome"] == "error"
    assert "boom" in outcomes[0]["reason"]


def test_declined_and_cut_reasons(box, tmp_path):
    srv = box({"/v1/chat/completions": chat({"role": "assistant", "content": "nope"})})
    js = setup(tmp_path, "--endpoint", srv.url + "/v1", "--model", "m", "--json")
    kinds = {o["outcome"] for o in json.loads(js.stdout)["qualification"]["outcomes"]}
    assert kinds == {"declined"}
    cut = box(
        {
            "/v1/chat/completions": chat(
                {"role": "assistant", "content": "", "reasoning_content": "x"}, "length"
            )
        }
    )
    res = setup(tmp_path, "--endpoint", cut.url + "/v1", "--model", "m")
    assert "First failure (truncated)" in res.output


def test_endpoint_reports_served_context(box, tmp_path):
    srv = box(
        {
            "/v1/models": (200, json.dumps(LLAMA_MODELS)),
            "/v1/chat/completions": chat({"role": "assistant", "content": VALID}),
        }
    )
    res = setup(tmp_path, "--endpoint", srv.url + "/v1", "--model", "m")
    assert "Context window: 16384 tokens" in res.output
    assert "262144" not in res.output
    js = setup(tmp_path, "--endpoint", srv.url + "/v1", "--model", "m", "--json")
    assert json.loads(js.stdout)["qualification"]["context_window"] == 16384


def test_progress_goes_to_stderr_and_json_stdout_stays_clean(box, tmp_path):
    srv = box({"/v1/chat/completions": chat({"role": "assistant", "content": VALID})})
    ep = srv.url + "/v1"
    res = setup(tmp_path, "--endpoint", ep, "--model", "m", "--json")
    assert f"qualifying m at {ep}: 3 probes, up to 300 s each" in res.stderr
    assert "probe 1/3: valid" in res.stderr and "probe 3/3: valid" in res.stderr
    json.loads(res.stdout)  # one clean document
    bad = box({"/v1/chat/completions": (500, "boom")})
    res = setup(tmp_path, "--endpoint", bad.url + "/v1", "--model", "m")
    assert "probe 1/3: error: " in res.stderr and "boom" in res.stderr
    quiet = runner.invoke(
        app,
        [
            "-q",
            "tiers",
            "setup",
            "--data-dir",
            str(tmp_path / "d"),
            "--endpoint",
            ep,
            "--model",
            "m",
        ],
    )
    assert "qualifying" not in quiet.stderr and "probe 1/3" not in quiet.stderr


def test_endpoint_context_is_of_the_named_model(box, tmp_path):
    models = {
        "data": [
            {"id": "small", "meta": {"n_ctx": 4096}},
            {"id": "big", "meta": {"n_ctx": 32768}},
        ]
    }
    srv = box(
        {
            "/v1/models": (200, json.dumps(models)),
            "/v1/chat/completions": chat({"role": "assistant", "content": VALID}),
        }
    )
    for name, want in (("big", 32768), ("openai/big", 32768), ("other", 4096)):
        res = setup(tmp_path, "--endpoint", srv.url + "/v1", "--model", name)
        assert f"Context window: {want} tokens" in res.output, (name, res.output)


def test_remote_context_from_models_meta(box, tmp_path):
    srv = box({"/v1/models": (200, json.dumps(LLAMA_MODELS))})
    res = setup(tmp_path, "--remote", srv.url.removeprefix("http://"), "--kind", "openai")
    assert "16384" in res.output and "262144" not in res.output
    assert "/props" not in srv.hits


def test_remote_context_falls_back_to_props(box, tmp_path):
    srv = box(
        {
            "/v1/models": (
                200,
                json.dumps({"data": [{"id": "m", "meta": {"n_ctx_train": 262144}}]}),
            ),
            "/props": (200, json.dumps({"default_generation_settings": {"n_ctx": 8192}})),
        }
    )
    res = setup(tmp_path, "--remote", srv.url.removeprefix("http://"), "--kind", "openai")
    assert "8192" in res.output and "262144" not in res.output
    assert srv.hits["/props"] == 1


def test_remote_context_never_the_training_length(box, tmp_path):
    srv = box({"/v1/models": (200, json.dumps({"data": [{"id": "m", "n_ctx_train": 4096}]}))})
    res = setup(tmp_path, "--remote", srv.url.removeprefix("http://"), "--kind", "openai")
    assert "4096" not in res.output


TAGS = (200, json.dumps({"models": [{"name": "gemma4:latest"}]}))


def test_ollama_endpoint_gets_reasoning_effort_none(box, tmp_path):
    srv = box(
        {"/api/tags": TAGS, "/v1/chat/completions": chat({"role": "assistant", "content": VALID})}
    )
    res = setup(tmp_path, "--endpoint", srv.url + "/v1", "--model", "gemma4", "--json")
    assert res.exit_code == 0, res.output
    assert srv.hits["/api/tags"] == 1
    posted = [json.loads(b) for b in srv.bodies if b]
    assert posted and all(b.get("reasoning_effort") == "none" for b in posted)
    assert list(posted[0])[-1] == "reasoning_effort"
    doc = json.loads(res.stdout)
    assert doc["qualification"]["host_kind"] == "ollama"
    assert doc["recommendation"]["runtime"] == "ollama"
    keys = list(doc["qualification"])
    assert keys.index("wired") + 1 == keys.index("host_kind") < keys.index("outcomes")
    assert "host: ollama, thinking off (reasoning_effort none)" in res.stderr


def test_openai_endpoint_sends_no_reasoning_effort(box, tmp_path):
    srv = box({"/v1/chat/completions": chat({"role": "assistant", "content": VALID})})
    res = setup(tmp_path, "--endpoint", srv.url + "/v1", "--model", "m")
    assert res.exit_code == 0, res.output
    assert srv.hits["/api/tags"] == 1
    assert all("reasoning_effort" not in json.loads(b) for b in srv.bodies if b)
    assert "via llamafile" in res.output and "Host kind: openai" in res.output
    assert "thinking off" not in res.stderr


def test_ollama_endpoint_text_says_ollama_not_llamafile(box, tmp_path):
    srv = box(
        {"/api/tags": TAGS, "/v1/chat/completions": chat({"role": "assistant", "content": VALID})}
    )
    res = setup(tmp_path, "--endpoint", srv.url + "/v1", "--model", "gemma4")
    assert "via ollama" in res.output and "llamafile" not in res.output
    assert "Host kind: ollama (thinking off: reasoning_effort none)" in res.output


def test_kind_flag_overrides_the_question(box, tmp_path):
    srv = box(
        {"/api/tags": TAGS, "/v1/chat/completions": chat({"role": "assistant", "content": VALID})}
    )
    setup(tmp_path, "--endpoint", srv.url + "/v1", "--model", "m", "--kind", "openai")
    assert "/api/tags" not in srv.hits
    assert all("reasoning_effort" not in json.loads(b) for b in srv.bodies if b)
    srv2 = box({"/v1/chat/completions": chat({"role": "assistant", "content": VALID})})
    setup(tmp_path, "--endpoint", srv2.url + "/v1", "--model", "m", "--kind", "ollama")
    assert "/api/tags" not in srv2.hits
    assert all(json.loads(b)["reasoning_effort"] == "none" for b in srv2.bodies if b)


def test_kind_anthropic_does_not_apply_to_endpoint(box, tmp_path):
    srv = box({})
    res = setup(tmp_path, "--endpoint", srv.url + "/v1", "--model", "m", "--kind", "anthropic")
    assert res.exit_code == 2
    assert "--kind 'anthropic' does not apply to --endpoint" in res.output
    assert srv.hits == {}


def test_truncated_probe_is_named_and_advised(box, tmp_path):
    cut = box({"/v1/chat/completions": chat({"role": "assistant", "content": ""}, "length")})
    js = setup(tmp_path, "--endpoint", cut.url + "/v1", "--model", "m", "--json")
    outcomes = json.loads(js.stdout)["qualification"]["outcomes"]
    assert {o["outcome"] for o in outcomes} == {"truncated"}
    assert "spends the limit on thinking" in outcomes[0]["reason"]
    res = setup(tmp_path, "--endpoint", cut.url + "/v1", "--model", "m")
    assert "First failure (truncated)" in res.output
    assert "Every failed probe was truncated" in res.output
    assert "Not promoted" not in res.output and "ALL attempts errored" not in res.output


def test_wire_records_host_kind(box, tmp_path):
    srv = box(
        {"/api/tags": TAGS, "/v1/chat/completions": chat({"role": "assistant", "content": VALID})}
    )
    res = setup(tmp_path, "--endpoint", srv.url + "/v1", "--model", "gemma4", "--wire")
    assert res.exit_code == 0, res.output
    cfg = json.loads((tmp_path / "d" / "local_tier1.json").read_text())
    assert cfg == {"model": "gemma4", "base_url": srv.url + "/v1", "host_kind": "ollama"}
