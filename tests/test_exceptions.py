"""Tests for opendaisugi.exceptions."""

import pytest

from opendaisugi.exceptions import (
    EnvelopeGenerationError,
    OpenDaisugiError,
    TaskTooLongError,
    VerificationTimeout,
)


def test_base_exception_is_exception():
    assert issubclass(OpenDaisugiError, Exception)


def test_task_too_long_error_inherits_base():
    assert issubclass(TaskTooLongError, OpenDaisugiError)


def test_verification_timeout_inherits_base():
    assert issubclass(VerificationTimeout, OpenDaisugiError)


def test_envelope_generation_error_inherits_base():
    assert issubclass(EnvelopeGenerationError, OpenDaisugiError)


def test_task_too_long_error_carries_message():
    err = TaskTooLongError("task is 5000 chars, limit 4000")
    assert "5000" in str(err)
    assert isinstance(err, OpenDaisugiError)


def test_can_catch_child_via_base():
    with pytest.raises(OpenDaisugiError):
        raise TaskTooLongError("oops")


def test_low_stakes_not_configured_is_value_error():
    from opendaisugi.exceptions import LowStakesNotConfigured
    assert issubclass(LowStakesNotConfigured, ValueError)


def test_model_ladder_exhausted_subclasses_envelope_error():
    from opendaisugi.exceptions import EnvelopeGenerationError, ModelLadderExhausted
    err = ModelLadderExhausted(
        attempted=["sonnet", "opus"],
        last_error=RuntimeError("boom"),
    )
    assert isinstance(err, EnvelopeGenerationError)
    assert err.attempted == ["sonnet", "opus"]
    assert isinstance(err.last_error, RuntimeError)
    assert "sonnet" in str(err)
    assert "opus" in str(err)
    assert "boom" in str(err)


def test_stakes_inheritance_warning_is_user_warning():
    from opendaisugi.exceptions import StakesInheritanceWarning
    assert issubclass(StakesInheritanceWarning, UserWarning)
