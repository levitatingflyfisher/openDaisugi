"""Pluggable step execution.

``StepExecutor`` is a runtime-checkable protocol so tests can assert a fake
satisfies it without subclassing. Implementations fall into three kinds:

- **Shell execution**: ``SubprocessExecutor`` runs commands for real with
  process-group teardown.
- **Test/dev doubles**: ``DryRunExecutor`` logs the intended command and
  returns rc=0 without executing; ``FakeExecutor`` uses a lookup table for
  deterministic tests.
- **Step-type specialists**: executors bound to a specific non-shell step
  kind (e.g. ``FileReadExecutor``). Each handles one ``ActionStep`` subclass
  and raises ``TypeError`` when given anything else.

Implementations live in this file alongside the fakes so callers get one
import path. Heavy stdlib imports (``subprocess``, ``signal``, ``os``) are at
module top — they are zero-cost.
"""

from __future__ import annotations

import os
import secrets
import signal
import socket
import subprocess
import time
import urllib.error
import urllib.request
from dataclasses import dataclass
from typing import Protocol, runtime_checkable

from opendaisugi.models import ActionPlan, ActionStep, FileReadStep, FileWriteStep, NetworkStep


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    """Surface 3xx as ``HTTPError`` instead of chasing the ``Location`` header."""

    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


@dataclass(frozen=True)
class ExecutorResult:
    rc: int
    stdout: str
    duration_ms: float
    timed_out: bool


@runtime_checkable
class StepExecutor(Protocol):
    def run(
        self,
        step: ActionStep,
        *,
        timeout_s: int,
        max_output_bytes: int,
    ) -> ExecutorResult: ...


class DryRunExecutor:
    """Logs a kind-aware dry-run line and returns rc=0.

    Message format dispatches on ``step.type`` (the Pydantic discriminator):
    shell prints the command repr, file_read prints the path, file_write
    prints the path plus UTF-8 byte count, network prints ``GET {url}``.
    """

    def run(
        self,
        step: ActionStep,
        *,
        timeout_s: int,
        max_output_bytes: int,
    ) -> ExecutorResult:
        match step.type:
            case "shell":
                msg = f"[dry-run] would shell: {step.command!r}"
            case "file_read":
                msg = f"[dry-run] would file_read: {step.path}"
            case "file_write":
                nbytes = len(step.content.encode("utf-8"))
                msg = f"[dry-run] would file_write: {step.path} ({nbytes} bytes)"
            case "network":
                msg = f"[dry-run] would network: GET {step.url}"
            case "task":
                msg = f"[dry-run] would task (delegate to LLM): {step.prompt!r}"
            case "skill":
                msg = f"[dry-run] would skill: {step.skill_id} input={step.skill_input!r}"
            case "mcp":
                msg = f"[dry-run] would mcp: {step.server}/{step.tool} args={step.arguments!r}"
            case _:  # pragma: no cover - unreachable; Pydantic rejects at parse time
                msg = f"[dry-run] unknown step kind: {step.type!r}"
        return ExecutorResult(rc=0, stdout=msg, duration_ms=0.0, timed_out=False)


class FakeExecutor:
    """Deterministic lookup-table executor for tests.

    ``mapping`` keys are kind-specific: shell uses ``step.command``,
    file_read/file_write use ``step.path``, network uses ``step.url``.
    Unknown keys raise ``KeyError`` unless ``default`` is supplied.
    """

    def __init__(
        self,
        mapping: dict[str, ExecutorResult] | None = None,
        *,
        default: ExecutorResult | None = None,
    ) -> None:
        self._mapping = dict(mapping or {})
        self._default = default

    @staticmethod
    def _key_for(step: ActionStep) -> str:
        match step.type:
            case "shell":
                return step.command
            case "file_read" | "file_write":
                return step.path
            case "network":
                return step.url
            case "task":
                return step.prompt
            case "skill":
                return step.skill_id
            case "mcp":
                return f"{step.server}/{step.tool}"
            case _:  # pragma: no cover - discriminator guards this
                raise TypeError(f"FakeExecutor: unknown step kind {step.type!r}")

    def run(
        self,
        step: ActionStep,
        *,
        timeout_s: int,
        max_output_bytes: int,
    ) -> ExecutorResult:
        key = self._key_for(step)
        if key in self._mapping:
            return self._mapping[key]
        if self._default is not None:
            return self._default
        raise KeyError(f"FakeExecutor has no result for key {key!r}")


class FileReadExecutor:
    """Reads ``step.path`` in chunks, truncating at ``max_output_bytes``."""

    def run(
        self,
        step: ActionStep,
        *,
        timeout_s: int,
        max_output_bytes: int,
    ) -> ExecutorResult:
        if not isinstance(step, FileReadStep):
            raise TypeError(
                f"FileReadExecutor cannot run step of type {type(step).__name__}"
            )
        start = time.monotonic()
        try:
            with open(step.path, "rb") as f:
                buf = bytearray()
                truncated = False
                while True:
                    chunk = f.read(min(64 * 1024, max_output_bytes - len(buf) + 1))
                    if not chunk:
                        break
                    buf.extend(chunk)
                    if len(buf) > max_output_bytes:
                        buf = buf[:max_output_bytes]
                        truncated = True
                        break
            stdout = buf.decode(errors="replace")
            if truncated:
                stdout += "\n... [truncated]"
            rc = 0
        except (FileNotFoundError, IsADirectoryError, PermissionError) as e:
            rc = 1
            stdout = f"{type(e).__name__}: {e}"
        duration_ms = (time.monotonic() - start) * 1000.0
        return ExecutorResult(rc=rc, stdout=stdout, duration_ms=duration_ms, timed_out=False)


class FileWriteExecutor:
    """Atomically writes ``step.content`` to ``step.path``.

    Strategy: write to a sibling tempfile in the target's parent directory,
    fsync, then ``os.rename`` over the target. The tempfile is opened with
    ``O_CREAT | O_EXCL | O_WRONLY | O_NOFOLLOW`` so a pre-existing symlink
    at the tempfile path cannot redirect the write. After rename, we
    resolve the final path's realpath and verify it still lives under the
    parent's realpath — defense-in-depth against symlink swaps in the
    parent dir after verify's glob check.
    """

    def run(
        self,
        step: ActionStep,
        *,
        timeout_s: int,
        max_output_bytes: int,
    ) -> ExecutorResult:
        if not isinstance(step, FileWriteStep):
            raise TypeError(
                f"FileWriteExecutor cannot run step of type {type(step).__name__}"
            )
        start = time.monotonic()
        parent = os.path.dirname(step.path) or "."
        tmp_path = ""
        try:
            # Reject symlinks at the target path before we do anything.
            # os.rename would replace the symlink atomically (POSIX rename
            # does not follow symlinks), but that still means a caller
            # believed they were writing to a whitelisted path that was
            # actually a link to somewhere else — treat it as an escape.
            if os.path.islink(step.path):
                duration_ms = (time.monotonic() - start) * 1000.0
                return ExecutorResult(
                    rc=2,
                    stdout=f"symlink at target rejected: {step.path}",
                    duration_ms=duration_ms,
                    timed_out=False,
                )
            os.makedirs(parent, exist_ok=True)
            tmp_name = f".daisugi-tmp-{secrets.token_hex(8)}-{os.path.basename(step.path)}"
            tmp_path = os.path.join(parent, tmp_name)
            flags = os.O_CREAT | os.O_EXCL | os.O_WRONLY | os.O_NOFOLLOW
            fd = os.open(tmp_path, flags, 0o644)
            try:
                data = step.content.encode("utf-8")
                bytes_written = os.write(fd, data)
                os.fsync(fd)
            finally:
                os.close(fd)
            os.rename(tmp_path, step.path)
            # tempfile has been consumed by rename; avoid cleanup below.
            tmp_path = ""

            # Defense-in-depth: confirm the final resolved path still lives
            # under the parent we wrote into. A symlink swap between the
            # rename and here would escape the verify-layer glob check.
            parent_real = os.path.realpath(parent)
            final_real = os.path.realpath(step.path)
            if not (
                final_real == parent_real
                or final_real.startswith(parent_real + os.sep)
            ):
                try:
                    os.unlink(final_real)
                except OSError:
                    pass
                duration_ms = (time.monotonic() - start) * 1000.0
                return ExecutorResult(
                    rc=2,
                    stdout=f"symlink escape detected: {step.path} -> {final_real}",
                    duration_ms=duration_ms,
                    timed_out=False,
                )

            duration_ms = (time.monotonic() - start) * 1000.0
            return ExecutorResult(
                rc=0,
                stdout=f"wrote {bytes_written} bytes to {step.path}",
                duration_ms=duration_ms,
                timed_out=False,
            )
        except OSError as e:
            if tmp_path:
                try:
                    os.unlink(tmp_path)
                except OSError:
                    pass
            duration_ms = (time.monotonic() - start) * 1000.0
            return ExecutorResult(
                rc=1,
                stdout=f"{type(e).__name__}: {e}",
                duration_ms=duration_ms,
                timed_out=False,
            )


class NetworkExecutor:
    """GET-only HTTP via stdlib ``urllib.request``. No redirect following.

    Builds a per-call opener with a custom ``HTTPRedirectHandler`` whose
    ``redirect_request`` returns ``None``, so urllib surfaces any 3xx as an
    ``HTTPError`` (``rc=1``) rather than chasing the ``Location`` header into
    a host the envelope may not allow. Response body is read into a bounded
    buffer and truncated with a suffix at ``max_output_bytes``.
    """

    def run(
        self,
        step: ActionStep,
        *,
        timeout_s: int,
        max_output_bytes: int,
    ) -> ExecutorResult:
        if not isinstance(step, NetworkStep):
            raise TypeError(
                f"NetworkExecutor cannot run step of type {type(step).__name__}"
            )
        start = time.monotonic()
        opener = urllib.request.build_opener(_NoRedirect)
        req = urllib.request.Request(
            step.url, headers=step.headers, method="GET",
        )
        try:
            with opener.open(req, timeout=timeout_s) as resp:
                raw = resp.read(max_output_bytes + 1)
            if len(raw) > max_output_bytes:
                stdout = raw[:max_output_bytes].decode("utf-8", errors="replace")
                stdout += "\n... [truncated]"
            else:
                stdout = raw.decode("utf-8", errors="replace")
            rc = 0
            timed_out = False
        except urllib.error.HTTPError as e:
            rc = 1
            try:
                body = e.read() or b""
            except Exception:
                body = b""
            if body:
                text = body.decode("utf-8", errors="replace")
                stdout = f"HTTP {e.code}: {e.reason}\n{text}"
            else:
                stdout = f"HTTP {e.code}: {e.reason}"
            timed_out = False
        except (socket.timeout, TimeoutError) as e:
            rc = 2
            stdout = f"{type(e).__name__}: {e}"
            timed_out = True
        except urllib.error.URLError as e:
            reason = e.reason
            # urllib wraps socket.timeout inside URLError on some code paths.
            if isinstance(reason, (socket.timeout, TimeoutError)) or (
                isinstance(reason, str) and "timed out" in reason.lower()
            ):
                rc = 2
                stdout = f"URLError: {reason}"
                timed_out = True
            else:
                rc = 2
                stdout = f"URLError: {reason}"
                timed_out = False

        duration_ms = (time.monotonic() - start) * 1000.0
        return ExecutorResult(
            rc=rc, stdout=stdout, duration_ms=duration_ms, timed_out=timed_out,
        )


class SubprocessExecutor:
    """Launches shell steps via ``subprocess.Popen(start_new_session=True)``.

    Signal discipline — SIGINT/timeout paths escalate SIGTERM → SIGKILL over
    a 2 s window, targeting the whole process group so shell grandchildren
    are reaped. ``subprocess.run(timeout=)`` is intentionally not used — it
    kills only the direct child and leaks grandchildren.
    """

    def run(
        self,
        step: ActionStep,
        *,
        timeout_s: int,
        max_output_bytes: int,
    ) -> ExecutorResult:
        cmd = step.command or ""
        start = time.monotonic()
        proc = subprocess.Popen(
            cmd,
            shell=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            start_new_session=True,
        )
        timed_out = False
        try:
            out_bytes, _ = proc.communicate(timeout=timeout_s)
        except subprocess.TimeoutExpired:
            timed_out = True
            out_bytes = self._teardown(proc)
        except KeyboardInterrupt:
            self._teardown(proc)
            raise
        duration_ms = (time.monotonic() - start) * 1000.0
        rc = proc.returncode if proc.returncode is not None else -1

        stdout = (out_bytes or b"").decode(errors="replace")
        if len(stdout.encode()) > max_output_bytes:
            stdout = stdout.encode()[:max_output_bytes].decode(errors="replace") + "\n... [truncated]"
        return ExecutorResult(
            rc=rc, stdout=stdout, duration_ms=duration_ms, timed_out=timed_out,
        )

    @staticmethod
    def _teardown(proc: subprocess.Popen) -> bytes:
        """Escalate SIGTERM → SIGKILL to the process group and drain output."""
        try:
            os.killpg(proc.pid, signal.SIGTERM)
        except ProcessLookupError:
            pass
        try:
            out_bytes, _ = proc.communicate(timeout=2)
            return out_bytes or b""
        except subprocess.TimeoutExpired:
            pass
        try:
            os.killpg(proc.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        out_bytes, _ = proc.communicate()
        return out_bytes or b""


def default_executors() -> dict[str, StepExecutor]:
    """Factory wiring each step kind to its default concrete executor.

    Returns a fresh mapping per call so the Supervisor owns its own executor
    instances and tests can freely swap entries without action-at-a-distance.
    """
    return {
        "shell": SubprocessExecutor(),
        "file_read": FileReadExecutor(),
        "file_write": FileWriteExecutor(),
        "network": NetworkExecutor(),
    }


def dry_run_executor_map(plan: "ActionPlan") -> dict[str, StepExecutor]:
    """Route every step kind in ``plan`` through one shared ``DryRunExecutor``.

    Nothing touches the shell, disk, or network — the Supervisor still verifies,
    journals, and integrity-checks against the dry-run results. Covers whatever
    step kinds the plan actually contains (including robot kinds), unlike a
    hard-coded shell/file/network map.
    """
    dry = DryRunExecutor()
    return {step.type: dry for step in plan.steps}
