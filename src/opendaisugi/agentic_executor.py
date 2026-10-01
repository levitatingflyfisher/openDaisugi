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

Two runtimes run the sub-agent. ``claude`` (the default) is ``claude -p``
with the gate as its PreToolUse hook. ``sprig`` is the ``sprig`` binary
(``DAISUGI_SPRIG`` names it) with the gate as its ``--gate-cmd``: the same
edge proof, private gate root, pinned session and tool wall, with the wall
handed over as ``--tools``. sprig runs Read, Write, Edit and Bash only; a
requested tool it has no tool for is left out of the wall.

A failed sub-agent (``is_error``, spawn failure, missing workspace) surfaces
as a failed step — never a swallowed one. The gate root's audit log is the
action transcript; with ``capture=True`` every tool call is also mirrored
into passive-capture format, so a delegated run feeds the same
captures → to-trace → journal pipeline distillation already reads. On the
sprig runtime the gate root also keeps sprig's own session file under
``sessions/``.
"""

from __future__ import annotations

import json
import os
import shutil
import signal
import subprocess
import sys
import tempfile
import time
from pathlib import Path

from opendaisugi.claude_code_llm import call_claude_p_sync
from opendaisugi.executor import ExecutorResult, truncate_output
from opendaisugi.gate import gate_settings_json, register_envelope
from opendaisugi.hook import _safe_session_id
from opendaisugi.models import AgenticStep, Envelope
from opendaisugi.verify import _AGENTIC_TOOL_CAPABILITIES

RUNTIMES = ("claude", "sprig")

# The host tools sprig has a tool for, and its name for each. Any other
# requested tool (Glob, Grep, MultiEdit, WebFetch, WebSearch) is left out
# of sprig's wall, never mapped onto a wider tool.
SPRIG_TOOLS = {"Read": "read", "Write": "write", "Edit": "edit", "Bash": "bash"}

# sprig splits --gate-cmd at whitespace (Go's strings.Fields), so a word
# that holds whitespace would reach the gate as two words. These are the
# characters Go counts as whitespace there.
_GO_SPACE = frozenset(
    "\t\n\v\f\r \x85\xa0\u1680\u2028\u2029\u202f\u205f\u3000"
    + "".join(chr(c) for c in range(0x2000, 0x200B))
)

# The fixed texts of a failed sprig run. The Go and Rust clients copy them.
SPRIG_SPACE_TEXT = (
    "the sprig gate command has a word with a space in it, and sprig splits that "
    "command at spaces. Use a TMPDIR path and a daisugi path with no spaces."
)


# How long the output is read after the timeout kill. A process that left
# sprig's group can hold a pipe open; the run does not wait for it past this.
SPRIG_DRAIN_S = 2


def _sprig_start_text(binary: str) -> str:
    return f"agentic sub-agent failed: cannot start sprig ({binary})"


def _sprig_timeout_text(timeout_s: int) -> str:
    return f"agentic sub-agent failed: sprig ran past {timeout_s}s and was killed"


def _sprig_exit_text(rc: int, stderr: bytes) -> str:
    if rc < 0:
        return f"agentic sub-agent failed: sprig was killed by signal {-rc}"
    lines = [ln.strip() for ln in stderr.decode("utf-8", "replace").split("\n") if ln.strip()]
    text = f"agentic sub-agent failed: sprig exited with code {rc}"
    return f"{text}: {lines[-1][:500]}" if lines else text


def _run_sprig(
    argv: list[str], *, stdin: str, cwd: str, timeout_s: int
) -> tuple[int, bytes, bytes, bool]:
    """Run sprig in its own process group; kill the whole group at the
    timeout, so a tool or gate process sprig started dies with it.
    Returns (exit code, stdout, stderr, timed out). Raises OSError when
    sprig cannot start."""
    proc = subprocess.Popen(
        argv,
        cwd=cwd,
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        start_new_session=True,
    )
    try:
        out, err = proc.communicate(stdin.encode("utf-8", "surrogateescape"), timeout=timeout_s)
    except subprocess.TimeoutExpired:
        try:
            os.killpg(proc.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        # A process that left the group can still hold a pipe: wait for the
        # rest of the output a short, fixed time, then stop reading.
        try:
            out, err = proc.communicate(timeout=SPRIG_DRAIN_S)
        except subprocess.TimeoutExpired:
            out, err = b"", b""
            for pipe in (proc.stdout, proc.stderr):
                if pipe is not None:
                    pipe.close()
            proc.wait()
        return proc.returncode, out, err, True
    return proc.returncode, out, err, False


class _LastAgentic:
    def __init__(
        self, model: str | None = None, tokens: int | None = None, cost_usd: float | None = None
    ) -> None:
        self.model = model
        self.attempts = 1
        self.tokens = tokens
        self.cost_usd = cost_usd


class AgenticExecutor:
    """StepExecutor for :class:`~opendaisugi.models.AgenticStep`.

    Construction::

        AgenticExecutor(envelope=env, model="haiku", capture=True)
        AgenticExecutor(envelope=env, runtime="sprig")

    ``envelope`` is the *caller's* envelope — the authorization ceiling. The
    executor re-derives the tool wall from it on every run (it does not
    trust the step), registers it for the gate, and never grants the
    sub-agent a capability the envelope lacks. ``runtime`` picks the
    sub-agent: ``claude`` or ``sprig``; ``sprig_binary`` defaults to
    ``DAISUGI_SPRIG``, else ``sprig`` on PATH.
    """

    def __init__(
        self,
        *,
        envelope: Envelope,
        model: str = "haiku",
        binary: str = "claude",
        capture: bool = True,
        keep_gate_root: bool = True,
        runtime: str = "claude",
        sprig_binary: str | None = None,
    ) -> None:
        if runtime not in RUNTIMES:
            raise ValueError(f"unknown agentic runtime {runtime!r}; choose from {list(RUNTIMES)}")
        self.envelope = envelope
        self.model = model
        self.binary = binary
        self.capture = capture
        self.keep_gate_root = keep_gate_root
        self.runtime = runtime
        self.sprig_binary = (
            sprig_binary if sprig_binary is not None else os.environ.get("DAISUGI_SPRIG", "sprig")
        )
        self.last = _LastAgentic()
        self.last_gate_root: Path | None = None

    def _derive_allowed_tools(self, step: AgenticStep, envelope: Envelope) -> list[str]:
        perms = envelope.permissions
        allowed: list[str] = []
        for tool in step.tools:
            cap = _AGENTIC_TOOL_CAPABILITIES.get(tool)
            if cap is None:
                continue  # unknown tool: never forwarded
            if self.runtime == "sprig" and tool not in SPRIG_TOOLS:
                continue  # sprig has no tool for it: left out, never mapped
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
        self.last_gate_root = None

        def _fail(msg: str) -> ExecutorResult:
            return ExecutorResult(
                rc=1,
                stdout=truncate_output(msg, max_output_bytes),
                duration_ms=(time.time() - started) * 1000.0,
                timed_out=False,
            )

        workspace = Path(step.workspace)
        if not workspace.is_dir():
            return _fail(
                f"agentic workspace '{step.workspace}' does not exist or is not a directory"
            )

        # The edge: the sub-agent's envelope must fit inside the caller's,
        # strict and fail closed, before anything starts. A step with no
        # child envelope restates the caller's own, which fits.
        from opendaisugi.tree import edge_ok

        edge = edge_ok(self.envelope, step.child_envelope or self.envelope)
        if not edge.holds or edge.child is None:
            return _fail("the child envelope is refused: " + "; ".join(edge.reasons))
        child = edge.child.model_copy(update={"parent_envelope": self.envelope.id})

        allowed = self._derive_allowed_tools(step, child)
        if not allowed:
            return _fail(
                f"no requested tool is backed by the envelope "
                f"(requested {step.tools!r}); nothing to delegate"
            )

        if self.runtime == "sprig":
            # sprig splits its gate command at whitespace: a gate program
            # or a gate root with a space in its path would break the pin,
            # so the step fails before anything is made or started. The
            # root's own name (mkdtemp) never holds one.
            words = [*self._sprig_gate_program(), tempfile.gettempdir()]
            if any(c in _GO_SPACE for w in words for c in w):
                return _fail(SPRIG_SPACE_TEXT)

        # The gate root is created OUTSIDE the workspace on purpose: the
        # sub-agent must not be able to rewrite its own hook configuration
        # or envelope mid-session.
        gate_root = Path(tempfile.mkdtemp(prefix="daisugi-agentic-gate-"))
        self.last_gate_root = gate_root
        # The executor, not the sub-agent, picks the session the envelope
        # binds to, and pins the gate to it: the sub-agent's payload cannot
        # name another session, and no call falls back to a default.
        session = "agentic-" + _safe_session_id(step.id)
        register_envelope(child, session_id=session, root=gate_root)

        if self.runtime == "sprig":
            return self._run_sprig_step(
                step,
                allowed,
                workspace,
                gate_root,
                session,
                timeout_s,
                _fail,
                started,
                max_output_bytes,
            )

        settings = gate_settings_json(
            mode="enforce",
            root=gate_root,
            captures_root=(gate_root / "captures") if self.capture else None,
            session=session,
        )

        extra_args: list[str] = [
            "--output-format",
            "json",
            "--settings",
            settings,
            "--allowedTools",
            " ".join(allowed),
        ]
        if step.max_turns is not None:
            extra_args += ["--max-turns", str(step.max_turns)]

        try:
            raw = call_claude_p_sync(
                step.prompt,
                timeout_s=float(timeout_s),
                model=self.model,
                binary=self.binary,
                cwd=str(workspace),
                extra_args=tuple(extra_args),
            )
        except Exception as exc:  # noqa: BLE001 — a dead sub-agent is a failed step
            self._cleanup(gate_root)
            return _fail(f"agentic sub-agent failed: {exc}")

        try:
            obj = json.loads(raw)
        except json.JSONDecodeError:
            self._cleanup(gate_root)
            return _fail(f"agentic sub-agent returned unparseable output: {raw[:300]!r}")

        usage = obj.get("usage") if isinstance(obj.get("usage"), dict) else {}
        fields = (
            "input_tokens",
            "output_tokens",
            "cache_creation_input_tokens",
            "cache_read_input_tokens",
        )
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

    @staticmethod
    def _sprig_gate_program() -> list[str]:
        """The program sprig runs as its gate: this oracle's hook entry.
        sprig runs it in the workspace with the sub-agent's environment, so
        -I (isolated) keeps that directory, PYTHONPATH and the user site off
        sys.path: a module the sub-agent writes there is never imported."""
        return [sys.executable, "-I", "-m", "opendaisugi.gate_client"]

    def _run_sprig_step(
        self,
        step,
        allowed,
        workspace,
        gate_root,
        session,
        timeout_s,
        _fail,
        started,
        max_output_bytes,
    ) -> ExecutorResult:
        # The gate is pinned by its own command (--root, --session), never
        # by the payload sprig sends: a payload that names another session
        # still meets this session's envelope.
        gate_words = [
            *self._sprig_gate_program(),
            "--mode",
            "enforce",
            "--root",
            str(gate_root),
            "--session",
            session,
        ]
        if self.capture:
            gate_words += ["--captures-root", str(gate_root / "captures")]
        wall: list[str] = []
        for tool in allowed:
            if SPRIG_TOOLS[tool] not in wall:
                wall.append(SPRIG_TOOLS[tool])
        argv = [
            self.sprig_binary,
            "--json",
            "--gate",
            "--gate-cmd",
            " ".join(gate_words),
            "--tools",
            ",".join(wall),
            "--model",
            self.model,
            "--session-dir",
            str(gate_root / "sessions"),
            "--session",
            session,
        ]
        if step.max_turns is not None:
            argv += ["--max-turns", str(step.max_turns)]
        argv.append("-")

        try:
            rc, out, err, timed_out = _run_sprig(
                argv, stdin=step.prompt, cwd=str(workspace), timeout_s=timeout_s
            )
        except OSError:
            self._cleanup(gate_root)
            return _fail(_sprig_start_text(self.sprig_binary))
        if timed_out:
            self._cleanup(gate_root)
            return _fail(_sprig_timeout_text(timeout_s))
        if rc != 0:
            self._cleanup(gate_root)
            return _fail(_sprig_exit_text(rc, err))

        raw = out.decode("utf-8", "replace")
        try:
            obj = json.loads(raw)
        except json.JSONDecodeError:
            self._cleanup(gate_root)
            return _fail(f"agentic sub-agent returned unparseable output: {raw[:300]!r}")
        answer = obj.get("answer") if isinstance(obj, dict) else None
        if not isinstance(answer, str):
            self._cleanup(gate_root)
            return _fail(f"agentic sub-agent reply has no answer: {raw[:300]!r}")

        usage = obj.get("usage") if isinstance(obj.get("usage"), dict) else {}
        counts = [
            v
            for f in (
                "input_tokens",
                "output_tokens",
                "cache_creation_input_tokens",
                "cache_read_input_tokens",
            )
            if isinstance(v := usage.get(f), int) and not isinstance(v, bool)
        ]
        self.last = _LastAgentic(
            model=self.model, tokens=sum(counts) if counts else None, cost_usd=None
        )
        return ExecutorResult(
            rc=0,
            stdout=truncate_output(answer, max_output_bytes),
            duration_ms=(time.time() - started) * 1000.0,
            timed_out=False,
        )

    def _cleanup(self, gate_root: Path) -> None:
        if not self.keep_gate_root:
            shutil.rmtree(gate_root, ignore_errors=True)
