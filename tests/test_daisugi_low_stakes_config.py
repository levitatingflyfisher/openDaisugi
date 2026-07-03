"""Facade-level low-stakes configuration (v0.1.3)."""
from __future__ import annotations

import pytest

from opendaisugi import DEFAULT_LOW_STAKES_ENVELOPE, Daisugi
from opendaisugi.exceptions import LowStakesNotConfigured


def test_default_daisugi_has_no_low_stakes():
    d = Daisugi()
    assert d._low_stakes_envelope is None


def test_with_default_low_stakes_classmethod():
    d = Daisugi.with_default_low_stakes()
    assert d._low_stakes_envelope is DEFAULT_LOW_STAKES_ENVELOPE


def test_with_default_low_stakes_forwards_kwargs(tmp_path):
    d = Daisugi.with_default_low_stakes(data_dir=tmp_path / "dd", cache=False)
    assert d._low_stakes_envelope is DEFAULT_LOW_STAKES_ENVELOPE
    assert d.data_dir == tmp_path / "dd"
    assert d.cache is None


def test_explicit_low_stakes_envelope(sample_envelope):
    d = Daisugi(low_stakes_envelope=sample_envelope)
    assert d._low_stakes_envelope is sample_envelope


@pytest.mark.asyncio
async def test_daisugi_stakes_low_without_config_raises(tmp_path):
    d = Daisugi(data_dir=tmp_path / "dd", cache=False)
    with pytest.raises(LowStakesNotConfigured):
        await d.generate_envelope(task="x", stakes="low")


@pytest.mark.asyncio
async def test_daisugi_with_default_low_stakes_returns_default(tmp_path):
    d = Daisugi.with_default_low_stakes(data_dir=tmp_path / "dd", cache=False)
    env = await d.generate_envelope(task="x", stakes="low")
    assert env.id == DEFAULT_LOW_STAKES_ENVELOPE.id


@pytest.mark.asyncio
async def test_daisugi_explicit_low_stakes_envelope_used(tmp_path, sample_envelope):
    d = Daisugi(
        data_dir=tmp_path / "dd", cache=False,
        low_stakes_envelope=sample_envelope,
    )
    env = await d.generate_envelope(task="x", stakes="low")
    assert env.id == sample_envelope.id
