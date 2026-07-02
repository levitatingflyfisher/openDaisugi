"""Tests for envelope inheritance verification (v0.1.2 Task 3).

Pure function `verify_inheritance(child, parent)` returns a list of Violations
where each Violation represents a way the child *relaxes* the parent. An
empty list means the child is a valid tightening.
"""

from __future__ import annotations

import pytest

from opendaisugi.inheritance import EnvelopeInheritanceError, verify_inheritance
from opendaisugi.models import Envelope, Invariant, Permission, Postcondition


def _env(**perm_kwargs) -> Envelope:
    """Build an envelope with a fully-specified Permission (override per test)."""
    base_perm = dict(
        file_read=[],
        file_write=[],
        network=False,
        network_hosts=[],
        shell=False,
        shell_allowlist=[],
        max_execution_time_s=30,
        max_output_size_mb=10,
    )
    base_perm.update(perm_kwargs)
    return Envelope(
        generated_by="test-model",
        task="test task",
        permissions=Permission(**base_perm),
    )


def test_no_violations_when_child_equals_parent():
    parent = _env(file_read=["/tmp/**"], network=True)
    child = _env(file_read=["/tmp/**"], network=True)
    assert verify_inheritance(child, parent) == []


def test_no_violations_when_child_strictly_tighter():
    parent = _env(
        file_read=["/tmp/**", "/var/log/**"],
        network=True,
        max_execution_time_s=60,
    )
    child = _env(
        file_read=["/tmp/**"],
        network=False,
        max_execution_time_s=30,
    )
    assert verify_inheritance(child, parent) == []


def test_file_read_relaxation_violation():
    parent = _env(file_read=["/tmp/**"])
    child = _env(file_read=["/tmp/**", "/etc/**"])
    violations = verify_inheritance(child, parent)
    assert len(violations) == 1
    assert violations[0].stage == "inheritance"
    assert "file_read" in violations[0].message
    assert "/etc/**" in violations[0].message


def test_file_write_relaxation_violation():
    parent = _env(file_write=["/tmp/**"])
    child = _env(file_write=["/tmp/**", "/var/**"])
    violations = verify_inheritance(child, parent)
    assert len(violations) == 1
    assert violations[0].stage == "inheritance"
    assert "file_write" in violations[0].message
    assert "/var/**" in violations[0].message


def test_network_relaxation_violation():
    parent = _env(network=False)
    child = _env(network=True)
    violations = verify_inheritance(child, parent)
    assert len(violations) == 1
    assert "network" in violations[0].message


def test_max_execution_time_increase_violation():
    parent = _env(max_execution_time_s=30)
    child = _env(max_execution_time_s=60)
    violations = verify_inheritance(child, parent)
    assert any("max_execution_time_s" in v.message for v in violations)
    assert any("60" in v.message and "30" in v.message for v in violations)


def test_max_output_size_increase_violation():
    parent = _env(max_output_size_mb=10)
    child = _env(max_output_size_mb=50)
    violations = verify_inheritance(child, parent)
    assert any("max_output_size_mb" in v.message for v in violations)


def test_shell_relaxation_violation():
    parent = _env(shell=False)
    child = _env(shell=True)
    violations = verify_inheritance(child, parent)
    # Must mention shell but not be the network violation
    assert any(
        "shell" in v.message and "network" not in v.message
        for v in violations
    )


def test_shell_allowlist_relaxation_violation():
    parent = _env(shell=True, shell_allowlist=["echo"])
    child = _env(shell=True, shell_allowlist=["echo", "rm"])
    violations = verify_inheritance(child, parent)
    assert any("shell_allowlist" in v.message and "rm" in v.message for v in violations)


def test_network_hosts_empty_child_with_restricted_parent_violation():
    parent = _env(network=True, network_hosts=["example.com"])
    child = _env(network=True, network_hosts=[])  # empty = "any" = relaxation
    violations = verify_inheritance(child, parent)
    assert any("network_hosts" in v.message for v in violations)


def test_network_hosts_child_subset_of_parent_no_violation():
    parent = _env(network=True, network_hosts=["a.com", "b.com"])
    child = _env(network=True, network_hosts=["a.com"])
    assert verify_inheritance(child, parent) == []


def test_network_hosts_parent_empty_means_child_anything():
    parent = _env(network=True, network_hosts=[])
    child = _env(network=True, network_hosts=["example.com"])
    assert verify_inheritance(child, parent) == []


def test_network_hosts_child_adds_host_violation():
    parent = _env(network=True, network_hosts=["a.com"])
    child = _env(network=True, network_hosts=["a.com", "b.com"])
    violations = verify_inheritance(child, parent)
    assert any("network_hosts" in v.message and "b.com" in v.message for v in violations)


def test_invariant_removal_violation():
    parent = Envelope(
        generated_by="t",
        task="t",
        permissions=Permission(),
        invariants=[
            Invariant(
                type="file_unchanged",
                target="/etc/passwd",
                description="do not modify /etc/passwd",
            )
        ],
    )
    child = Envelope(generated_by="t", task="t", permissions=Permission())
    violations = verify_inheritance(child, parent)
    assert any("invariants" in v.message for v in violations)


def test_invariant_addition_no_violation():
    parent = Envelope(generated_by="t", task="t", permissions=Permission())
    child = Envelope(
        generated_by="t",
        task="t",
        permissions=Permission(),
        invariants=[
            Invariant(
                type="file_unchanged",
                target="/etc/passwd",
                description="do not modify /etc/passwd",
            )
        ],
    )
    assert verify_inheritance(child, parent) == []


def test_postcondition_removal_violation():
    parent = Envelope(
        generated_by="t",
        task="t",
        permissions=Permission(),
        postconditions=[Postcondition(type="file_exists", path="/tmp/out")],
    )
    child = Envelope(generated_by="t", task="t", permissions=Permission())
    violations = verify_inheritance(child, parent)
    assert any("postconditions" in v.message for v in violations)


def test_postcondition_addition_no_violation():
    parent = Envelope(generated_by="t", task="t", permissions=Permission())
    child = Envelope(
        generated_by="t",
        task="t",
        permissions=Permission(),
        postconditions=[Postcondition(type="file_exists", path="/tmp/out")],
    )
    assert verify_inheritance(child, parent) == []


def test_parent_with_own_parent_violation():
    grandparent = _env()
    parent = _env()
    parent.parent_envelope = grandparent.id  # depth-2 not supported in v0.1.2
    child = _env()
    violations = verify_inheritance(child, parent)
    assert any(
        "parent_envelope" in v.message or "depth" in v.message
        for v in violations
    )


def test_multiple_violations_all_reported():
    parent = _env(network=False, max_execution_time_s=30, max_output_size_mb=10)
    child = _env(network=True, max_execution_time_s=60, max_output_size_mb=50)
    violations = verify_inheritance(child, parent)
    assert len(violations) >= 3
    messages = " ".join(v.message for v in violations)
    assert "network" in messages
    assert "max_execution_time_s" in messages
    assert "max_output_size_mb" in messages


def test_envelope_inheritance_error_carries_violations():
    parent = _env(network=False)
    child = _env(network=True)
    violations = verify_inheritance(child, parent)
    err = EnvelopeInheritanceError(violations)
    assert err.violations == violations
    assert "network" in str(err)


def test_envelope_inheritance_error_is_exception():
    assert issubclass(EnvelopeInheritanceError, Exception)


def test_all_violations_have_inheritance_stage():
    parent = _env(
        file_read=["/tmp/**"],
        file_write=["/tmp/**"],
        network=False,
        shell=False,
        shell_allowlist=["echo"],
        max_execution_time_s=30,
        max_output_size_mb=10,
    )
    child = _env(
        file_read=["/tmp/**", "/etc/**"],
        file_write=["/tmp/**", "/var/**"],
        network=True,
        shell=True,
        shell_allowlist=["echo", "rm"],
        max_execution_time_s=60,
        max_output_size_mb=100,
    )
    violations = verify_inheritance(child, parent)
    assert len(violations) > 0
    assert all(v.stage == "inheritance" for v in violations)


# --------------------- robotics/mcp/stakes tightening (SGCM review H1) ---------------------

def _envp(**perm_and_meta):
    from opendaisugi.models import Envelope, Permission
    stakes = perm_and_meta.pop("stakes", "medium")
    return Envelope(generated_by="t", task="x", stakes=stakes, permissions=Permission(**perm_and_meta))


def test_inheritance_rejects_relaxed_velocity():
    from opendaisugi.inheritance import verify_inheritance
    parent = _envp(velocity_limit=0.5, stakes="physical")
    child = _envp(velocity_limit=5.0, stakes="physical")  # 10x faster
    assert verify_inheritance(child, parent)  # non-empty → violations


def test_inheritance_rejects_expanded_workspace():
    from opendaisugi.inheritance import verify_inheritance
    parent = _envp(workspace_bounds=((0, 0, 0), (1, 1, 1)), stakes="physical")
    child = _envp(workspace_bounds=((0, 0, 0), (100, 100, 100)), stakes="physical")
    assert verify_inheritance(child, parent)


def test_inheritance_rejects_added_mcp_tool():
    from opendaisugi.inheritance import verify_inheritance
    parent = _envp(mcp_allowlist=[])          # deny all
    child = _envp(mcp_allowlist=["github/*"])  # added a tool
    assert verify_inheritance(child, parent)


def test_inheritance_rejects_stakes_downgrade():
    from opendaisugi.inheritance import verify_inheritance
    parent = _envp(stakes="physical")
    child = _envp(stakes="low")  # downgrade re-enables probabilistic primitives
    assert verify_inheritance(child, parent)


def test_inheritance_allows_genuine_tightening():
    from opendaisugi.inheritance import verify_inheritance
    parent = _envp(velocity_limit=5.0, workspace_bounds=((0, 0, 0), (10, 10, 10)),
                   mcp_allowlist=["github/*"], stakes="high")
    child = _envp(velocity_limit=2.0, workspace_bounds=((0, 0, 0), (5, 5, 5)),
                  mcp_allowlist=[], stakes="physical")
    assert not verify_inheritance(child, parent)
