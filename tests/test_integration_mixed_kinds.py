"""End-to-end integration: one plan exercising all four step kinds.

Covers v0.1.1 Ship-criteria #3: shell + file_read + file_write + network run
together through the real ``default_executors()`` registry, with dependencies
wired and the journal recording each kind's outcome.
"""

from __future__ import annotations

import threading
from http.server import BaseHTTPRequestHandler, HTTPServer

import pytest

from opendaisugi.executor import default_executors
from opendaisugi.models import (
    ActionPlan,
    Envelope,
    FileReadStep,
    FileWriteStep,
    NetworkStep,
    Permission,
    ShellStep,
)
from opendaisugi.run_session import RunStatus
from opendaisugi.supervisor import Supervisor


class _HelloHandler(BaseHTTPRequestHandler):
    def log_message(self, *args, **kwargs):
        pass  # silence stderr

    def do_GET(self):
        if self.path == "/hello":
            body = b"hi"
            self.send_response(200)
            self.send_header("Content-Type", "text/plain")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        else:
            self.send_response(404)
            self.send_header("Content-Length", "0")
            self.end_headers()


@pytest.fixture
def hello_server():
    httpd = HTTPServer(("127.0.0.1", 0), _HelloHandler)
    port = httpd.server_address[1]
    t = threading.Thread(target=httpd.serve_forever, daemon=True)
    t.start()
    try:
        yield port
    finally:
        httpd.shutdown()
        httpd.server_close()


async def test_mixed_kind_plan_runs_end_to_end(tmp_path, hello_server, monkeypatch):
    """A single plan with shell + file_write + file_read + network succeeds
    via the real default executors, with dependencies respected and the
    journal recording each kind."""
    monkeypatch.setenv("DAISUGI_APPROVE", "always")

    port = hello_server
    out_file = tmp_path / "out.txt"
    url = f"http://127.0.0.1:{port}/hello"

    plan = ActionPlan(
        source="test",
        task="mixed-kind integration",
        steps=[
            ShellStep(id="step_shell", command="echo hello"),
            FileWriteStep(
                id="step_write",
                path=str(out_file),
                content="written by daisugi",
            ),
            FileReadStep(
                id="step_read",
                path=str(out_file),
                depends_on=["step_write"],
            ),
            NetworkStep(id="step_net", url=url),
        ],
    )
    envelope = Envelope(
        generated_by="test",
        task="mixed-kind integration",
        permissions=Permission(
            shell=True,
            shell_allowlist=["echo"],
            file_read=[f"{tmp_path}/**"],
            file_write=[f"{tmp_path}/**"],
            network=True,
            network_hosts=["127.0.0.1"],
        ),
    )

    supervisor = Supervisor(executors=default_executors())
    session = await supervisor.run(plan, envelope)

    assert session.status == RunStatus.SUCCEEDED, session
    assert len(session.steps) == 4
    by_id = {s.step_id: s for s in session.steps}
    assert set(by_id) == {"step_shell", "step_write", "step_read", "step_net"}
    for outcome in session.steps:
        assert outcome.status == "succeeded", outcome

    assert "hello" in by_id["step_shell"].stdout
    assert "written by daisugi" in by_id["step_read"].stdout
    assert "hi" in by_id["step_net"].stdout

    assert out_file.exists()
    assert out_file.read_text() == "written by daisugi"
