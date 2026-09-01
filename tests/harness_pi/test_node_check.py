from __future__ import annotations

import subprocess

from tests.harness_pi._node_check import node_status


def test_reports_ok_when_node_version_meets_minimum(monkeypatch):
    monkeypatch.setattr("shutil.which", lambda name: "/usr/bin/node")
    monkeypatch.setattr(
        "subprocess.run",
        lambda *a, **k: subprocess.CompletedProcess(a[0], 0, stdout="v22.22.2\n"),
    )
    ok, detail = node_status(min_major=22)
    assert ok is True
    assert detail == "v22.22.2"


def test_reports_absent_when_node_not_on_path(monkeypatch):
    monkeypatch.setattr("shutil.which", lambda name: None)
    ok, detail = node_status()
    assert ok is False
    assert "not on PATH" in detail


def test_reports_too_old_when_major_below_minimum(monkeypatch):
    monkeypatch.setattr("shutil.which", lambda name: "/usr/bin/node")
    monkeypatch.setattr(
        "subprocess.run",
        lambda *a, **k: subprocess.CompletedProcess(a[0], 0, stdout="v18.19.0\n"),
    )
    ok, detail = node_status(min_major=22)
    assert ok is False
    assert "older than required 22" in detail


def test_this_box_actually_has_a_usable_node():
    """Not mocked. Confirms the extension tests run here instead of
    skipping. If this fails on a dev box, install Node 22 or newer."""
    ok, detail = node_status()
    assert ok, f"Node 22+ required for pi extension tests: {detail}"
