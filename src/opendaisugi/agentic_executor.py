"""AgenticExecutor — run a tool-using sub-agent inside the parent's envelope.

Roadmap Stage 2, ADR-0007 §3. The enforcement story is defense in depth,
and neither wall alone is it:

- **Static outer wall.** The ``--allowedTools`` list handed to the sub-agent
  is *computed* from the envelope: the step's requested tools intersected
  with the capabilities the envelope actually grants. A tool the envelope
  doesn't back never reaches the argv. String patterns, not proof — which is
  exactly why it is not the primary mechanism.
- **Dynamic inner wall.** The sub-agent runs under the call-time gate in
  enforce mode: every tool call it makes is synthesized into a one-step plan
  and proved inside the envelope before it runs. The gate's settings and the
  registered envelope live in a freshly created private root *outside the
  workspace* — supplied from outside anything the sub-agent can write.

A failed sub-agent (``is_error``, spawn failure, missing workspace) surfaces
as a failed step — never a swallowed one. The gate root's shadow log is the
action transcript; with ``capture=True`` every tool call is also mirrored
into passive-capture format, so a delegated run feeds the same
captures → to-trace → journal pipeline distillation already reads.
"""

from __future__ import annotations

import json
import shutil
import tempfile
import time
from pathlib import Path

from opendaisugi.claude_code_llm import call_claude_p_sync
from opendaisugi.executor import ExecutorResult, truncate_output
from opendaisugi.gate import gate_settings_json, register_envelope
from opendaisugi.models import AgenticStep, Envelope
from opendaisugi.verify import _AGENTIC_TOOL_CAPABILITIES


class _LastAgentic:
    def __init__(self, model: str | None = None, tokens: int | None = None,
                 cost_usd: float | None = None) -> None:
        self.model = model
        self.attempts = 1
        self.tokens = tokens
        self.cost_usd = cost_usd


class AgenticExecutor:
    """StepExecutor for :class:`~opendaisugi.models.AgenticStep`.

    Construction::

        AgenticExecutor(envelope=env, model="haiku", capture=True)

    ``envelope`` is the *caller's* envelope — the authorization ceiling. The
    executor re-derives the tool wall from it on every run (it does not
    trust the step), registers it for the gate, and never grants the
    sub-agent a capability the envelope lacks.
    """

    def __init__(self, *, envelope: Envelope, model: str = "haiku",
                 binary: str = "claude", capture: bool = True,
                 keep_gate_root: bool = True) -> None:
        self.envelope = envelope
        self.model = model
        self.binary = binary
        self.capture = capture
        self.keep_gate_root = keep_gate_root
        self.last = _LastAgentic()
        self.last_gate_root: Path | None = None

    def _derive_allowed_tools(self, step: AgenticStep) -> list[str]:
        perms = self.envelope.permissions
        allowed: list[str] = []
        for tool in step.tools:
            cap = _AGENTIC_TOOL_CAPABILITIES.get(tool)
            if cap is None:
                continue  # unknown tool: never forwarded
            if getattr(perms, cap):
                allowed.append(tool)
        return allowed

    def run(self, step, *, timeout_s: int, max_output_bytes: int) -> ExecutorResult:
        if not isinstance(step, AgenticStep):
            raise TypeError(
                f"AgenticExecutor got {type(step).__name__}; wire it under "
                f"the 'agentic' step type only"
            )
        started = time.time()
        self.last = _LastAgentic(model=self.model)

        def _fail(msg: str) -> ExecutorResult:
            return ExecutorResult(
                rc=1, stdout=truncate_output(msg, max_output_bytes),
                duration_ms=(time.time() - started) * 1000.0, timed_out=False,
            )

        workspace = Path(step.workspace)
        if not workspace.is_dir():
            return _fail(
                f"agentic workspace '{step.workspace}' does not exist or is "
                f"not a directory"
            )

        allowed = self._derive_allowed_tools(step)
        if not allowed:
            return _fail(
                f"no requested tool is backed by the envelope "
                f"(requested {step.tools!r}); nothing to delegate"
            )

        # The gate root is created OUTSIDE the workspace on purpose: the
        # sub-agent must not be able to rewrite its own hook configuration
        # or envelope mid-session.
        gate_root = Path(tempfile.mkdtemp(prefix="daisugi-agentic-gate-"))
        self.last_gate_root = gate_root
        register_envelope(self.envelope, root=gate_root)
        settings = gate_settings_json(
            mode="enforce", root=gate_root,
            captures_root=(gate_root / "captures") if self.capture else None,
        )

        extra_args: list[str] = [
            "--output-format", "json",
            "--settings", settings,
            "--allowedTools", " ".join(allowed),
        ]
        if step.max_turns is not None:
            extra_args += ["--max-turns", str(step.max_turns)]

        try:
            raw = call_claude_p_sync(
                step.prompt, timeout_s=float(timeout_s), model=self.model,
                binary=self.binary, cwd=str(workspace),
                extra_args=tuple(extra_args),
            )
        except Exception as exc:  # noqa: BLE001 — a dead sub-agent is a failed step
            self._cleanup(gate_root)
            return _fail(f"agentic sub-agent failed: {exc}")

        try:
            obj = json.loads(raw)
        except json.JSONDecodeError:
            self._cleanup(gate_root)
            return _fail(
                f"agentic sub-agent returned unparseable output: {raw[:300]!r}"
            )

        usage = obj.get("usage") if isinstance(obj.get("usage"), dict) else {}
        fields = ("input_tokens", "output_tokens",
                  "cache_creation_input_tokens", "cache_read_input_tokens")
        present = [usage.get(f) for f in fields if usage.get(f) is not None]
        self.last = _LastAgentic(
            model=self.model,
            tokens=sum(int(v) for v in present) if present else None,
            cost_usd=obj.get("total_cost_usd"),
        )

        result_text = str(obj.get("result", "") or "")
        if obj.get("is_error"):
            self._cleanup(gate_root)
            return _fail(f"agentic sub-agent reported is_error: {result_text[:500]}")

        return ExecutorResult(
            rc=0,
            stdout=truncate_output(result_text, max_output_bytes),
            duration_ms=(time.time() - started) * 1000.0,
            timed_out=False,
        )

    def _cleanup(self, gate_root: Path) -> None:
        if not self.keep_gate_root:
            shutil.rmtree(gate_root, ignore_errors=True)
