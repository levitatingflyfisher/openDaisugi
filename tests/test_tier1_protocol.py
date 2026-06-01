"""Tests for the Tier1Provider protocol shape."""

from __future__ import annotations

from opendaisugi.tier1 import Tier1Provider


def test_custom_provider_satisfies_protocol() -> None:
    """A user class with the right shape passes the isinstance check."""

    class MyProvider:
        name = "mine"

        async def generate_envelope(
            self, task: str, *, context: str | None = None,
        ):
            return None

    assert isinstance(MyProvider(), Tier1Provider)


def test_missing_name_fails_protocol_check() -> None:
    """A class missing ``name`` is not a valid Tier1Provider."""

    class Broken:
        async def generate_envelope(self, task: str, *, context: str | None = None):
            return None

    assert not isinstance(Broken(), Tier1Provider)
