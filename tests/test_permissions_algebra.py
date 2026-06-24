"""Tests for opendaisugi.permissions.intersect_permissions (v0.4.1)."""

from __future__ import annotations

import pytest

from opendaisugi.models import Permission
from opendaisugi.permissions import intersect_permissions


def test_empty_raises() -> None:
    with pytest.raises(ValueError):
        intersect_permissions([])


def test_single_returns_copy() -> None:
    p = Permission(shell=True, network=True)
    result = intersect_permissions([p])
    assert result == p
    assert result is not p  # deep copy


def test_bool_fields_and() -> None:
    a = Permission(shell=True, network=True)
    b = Permission(shell=True, network=False)
    result = intersect_permissions([a, b])
    assert result.shell is True
    assert result.network is False


def test_list_fields_intersect_sorted() -> None:
    a = Permission(shell=True, file_read=["/a", "/b", "/c"])
    b = Permission(shell=True, file_read=["/b", "/c", "/d"])
    result = intersect_permissions([a, b])
    assert result.file_read == ["/b", "/c"]


def test_int_fields_minimum() -> None:
    a = Permission(shell=True, max_execution_time_s=60)
    b = Permission(shell=True, max_execution_time_s=30)
    c = Permission(shell=True)  # no ceiling set
    result = intersect_permissions([a, b, c])
    assert result.max_execution_time_s == 30


def test_distiller_backward_compat_alias() -> None:
    """The distiller's original name must still resolve for any existing callers."""
    from opendaisugi.distiller import _intersect_permissions
    assert _intersect_permissions is intersect_permissions
