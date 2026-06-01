"""Custom exception hierarchy for opendaisugi."""
from __future__ import annotations


class OpenDaisugiError(Exception):
    """Base class for all opendaisugi errors."""


class TaskTooLongError(OpenDaisugiError):
    """Raised when a task (plus optional context) exceeds the char limit."""


class VerificationTimeout(OpenDaisugiError):
    """Raised when a Z3 check exceeds its allotted time budget."""


class EnvelopeGenerationError(OpenDaisugiError):
    """Raised when envelope generation fails after retries."""


class StepExecutionError(OpenDaisugiError):
    """Raised by executor on infrastructure failure (not a non-zero rc)."""


class ApprovalDeniedError(OpenDaisugiError):
    """Approval strategy refused the step."""


class SupervisorAborted(OpenDaisugiError):
    """Run was aborted (SIGINT, timeout, or abort signal)."""


class NotTerminalError(OpenDaisugiError):
    """TtyPromptStrategy invoked without a TTY."""


class IntegrityViolation(OpenDaisugiError):
    """Run completed but per-step receipts don't cover the steps that were
    supposed to run — signal of silent step-skipping. v0.18.0+."""


class LowStakesNotConfigured(ValueError):
    """Raised when stakes='low' is passed but no low_stakes_envelope is configured.

    The library deliberately refuses to silently use a permissive default; the
    caller must opt in by passing ``low_stakes_envelope=...`` or constructing the
    facade via ``Daisugi.with_default_low_stakes()``.
    """


class ModelLadderExhausted(EnvelopeGenerationError):
    """Raised when every model in a tiered-routing ladder fails."""

    def __init__(self, attempted: list[str], last_error: Exception) -> None:
        self.attempted = attempted
        self.last_error = last_error
        super().__init__(
            f"All models in ladder exhausted: {attempted}. "
            f"Last error ({type(last_error).__name__}): {last_error}"
        )


class StakesInheritanceWarning(UserWarning):
    """Emitted when stakes='low' is passed together with parent=; parent is ignored."""
