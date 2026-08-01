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


class LLMNotConfigured(OpenDaisugiError):
    """The selected LLM backend cannot run on this machine (no key, no binary).

    Raised before any network call. Deterministic: retrying cannot help.
    """


class DecompositionError(OpenDaisugiError):
    """The decomposer could not produce a valid plan.

    ``plan`` carries the assembled plan when one was built (DAG or policy
    failure) so a caller can inspect or retry; ``None`` when the LLM call itself
    failed or returned nothing.
    """

    def __init__(self, message: str, *, plan=None) -> None:
        super().__init__(message)
        self.plan = plan


class NoStepsError(DecompositionError):
    """The prompt decomposed to zero steps: there is nothing to run."""


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


class LowStakesNotConfigured(OpenDaisugiError, ValueError):
    """Raised when stakes='low' is passed but no low_stakes_envelope is configured.

    The library deliberately refuses to silently use a permissive default; the
    caller must opt in by passing ``low_stakes_envelope=...`` or constructing the
    facade via ``Daisugi.with_default_low_stakes()``.

    Subclasses both ``OpenDaisugiError`` (so ``cli.main()``'s one error renderer
    catches it and shows a config message, not a bug traceback) and ``ValueError``
    (the original v0.1 type — kept for backward compatibility).
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
