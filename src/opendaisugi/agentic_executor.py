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

import contextlib
import dataclasses
import fcntl
import json
import os
import selectors
import shutil
import signal
import subprocess
import sys
import tempfile
import threading
import time
from collections.abc import Callable, Mapping
from pathlib import Path
from typing import Any

from opendaisugi import agent_profiles
from opendaisugi.claude_code_llm import call_claude_p_sync
from opendaisugi.executor import ExecutorResult, truncate_output
from opendaisugi.gate import gate_settings_json
from opendaisugi.hook import _safe_session_id
from opendaisugi.models import AgenticStep, Envelope
from opendaisugi.verify import _AGENTIC_TOOL_CAPABILITIES, agentic_tool_granted

RUNTIMES = ("claude", "sprig")

# The host tools sprig has a tool for, and its name for each. Any other
# requested tool (Glob, Grep, MultiEdit, WebFetch, WebSearch) is left out
# of sprig's wall, never mapped onto a wider tool.
SPRIG_TOOLS = {
    "Read": "read",
    "Write": "write",
    "Edit": "edit",
    "Bash": "bash",
    "Spawn": "spawn",
}

#: The descriptors a child reads its key and the profile bundle from.
KEY_FD = 3
SPAWN_KEYS_FD = 4

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
#: How long kill_groups waits after SIGTERM before SIGKILL.
KILL_GRACE_S = 2
#: The most a child may write on stdout, and on stderr.
OUTPUT_CAP = 16 << 20


def _sprig_start_text(binary: str) -> str:
    return f"agentic sub-agent failed: cannot start sprig ({binary})"


SPRIG_TOO_BIG_TEXT = "agentic sub-agent failed: sprig wrote more than 16 MiB"


def _sprig_timeout_text(timeout_s: int) -> str:
    return f"agentic sub-agent failed: sprig ran past {timeout_s}s and was killed"


def _sprig_exit_text(rc: int, stderr: bytes) -> str:
    if rc < 0:
        return f"agentic sub-agent failed: sprig was killed by signal {-rc}"
    lines = [ln.strip() for ln in stderr.decode("utf-8", "replace").split("\n") if ln.strip()]
    text = f"agentic sub-agent failed: sprig exited with code {rc}"
    return f"{text}: {lines[-1][:500]}" if lines else text


# The child starter for descriptors at fixed numbers: Popen keeps a
# passed descriptor at the parent's number, so this small program, run as
# `python -I -c`, moves each one to its place, sets the parent-death
# signal, and becomes the child. argv: "SRC:DST,...", the parent pid,
# then the child's argv.
_PLACE_FDS = (
    "import ctypes,os,sys\n"
    "try:\n"
    "    ctypes.CDLL(None).prctl(1, 15, 0, 0, 0)\n"
    "except Exception:\n"
    "    pass\n"
    "if os.getppid() != int(sys.argv[2]):\n"
    "    os._exit(1)\n"
    "for pair in filter(None, sys.argv[1].split(',')):\n"
    "    src, dst = (int(x) for x in pair.split(':'))\n"
    "    os.dup2(src, dst)\n"
    "    os.close(src)\n"
    "os.execv(sys.argv[3], sys.argv[3:])\n"
)


def _pipe_fds(fds: Mapping[int, bytes]) -> tuple[list[tuple[int, int]], list[threading.Thread]]:
    """One pipe per descriptor, its read end moved above 30 (close on
    exec until the child starter places it), its bytes written by a thread
    so a large payload never blocks the start."""
    moves, writers = [], []
    for dst, data in sorted(fds.items()):
        r, w = os.pipe()
        high = fcntl.fcntl(r, fcntl.F_DUPFD_CLOEXEC, 30)
        os.close(r)

        def write(w: int = w, data: bytes = data) -> None:
            try:
                _write_all(w, data)
            except OSError:
                pass
            finally:
                os.close(w)

        t = threading.Thread(target=write, daemon=True)
        t.start()
        writers.append(t)
        moves.append((high, dst))
    return moves, writers


def _write_all(fd: int, data: bytes) -> None:
    view = memoryview(data)
    while view:
        view = view[os.write(fd, view) :]


def _run_sprig(
    argv: list[str],
    *,
    stdin: str,
    cwd: str,
    timeout_s: int,
    env: Mapping[str, str] | None = None,
    fds: Mapping[int, bytes] | None = None,
    on_start: Callable[[int], None] | None = None,
    branch: Callable[[], list[int]] | None = None,
) -> tuple[int, bytes, bytes, bool]:
    """Run sprig in its own process group, with the parent-death signal
    (SIGTERM, so sprig can kill its tools' groups) set. ``on_start`` gets
    its pid, which is its group. When it exits, fails, writes more than
    OUTPUT_CAP or runs past the timeout, ``branch()`` groups and then its
    own are killed (kill_groups).
    ``env`` is its whole environment (None inherits this one); ``fds``
    puts bytes on descriptors at fixed numbers, each a pipe.
    Returns (exit code, stdout, stderr, timed out). Raises OSError when
    sprig cannot start."""
    path = (env if env is not None else os.environ).get("PATH")
    binary = shutil.which(argv[0], path=path) if "/" not in argv[0] else argv[0]
    if binary is None or not os.access(binary, os.X_OK) or os.path.isdir(binary):
        raise FileNotFoundError(argv[0])
    moves, _ = _pipe_fds(fds or {})
    place = ",".join(f"{src}:{dst}" for src, dst in moves)
    argv = [sys.executable, "-I", "-c", _PLACE_FDS, place, str(os.getpid()), binary, *argv[1:]]
    try:
        proc = subprocess.Popen(
            argv,
            cwd=cwd,
            env=dict(env) if env is not None else None,
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            start_new_session=True,
            pass_fds=tuple(src for src, _ in moves),
        )
    finally:
        for src, _ in moves:
            os.close(src)
    if on_start is not None:
        on_start(proc.pid)
    threading.Thread(
        target=_feed, args=(proc.stdin, stdin.encode("utf-8", "surrogateescape")), daemon=True
    ).start()
    out, err, timed_out, big = _collect(proc, timeout_s, branch)
    if big:
        raise _TooBig
    return proc.returncode, out, err, timed_out


def _collect(
    proc: subprocess.Popen, timeout_s: int, branch: Callable[[], list[int]] | None
) -> tuple[bytes, bytes, bool, bool]:
    """Read the child's stdout and stderr, at most OUTPUT_CAP each, until
    both end. The kill (MX-R-8): when the child exits, writes too much or
    runs past the timeout, the recorded groups of its running descendants
    and then its own group are killed (kill_groups). Output a process
    outside the group still holds is read until the timeout, and after a
    kill for SPRIG_DRAIN_S more. Returns (stdout, stderr, timed out, too
    big)."""
    out_buf, err_buf = bytearray(), bytearray()
    bufs = {proc.stdout.fileno(): out_buf, proc.stderr.fileno(): err_buf}
    sel = selectors.DefaultSelector()
    for fd in bufs:
        os.set_blocking(fd, False)
        sel.register(fd, selectors.EVENT_READ)
    deadline = time.monotonic() + timeout_s
    stop_at: float | None = None
    exited_kill = timed_out = big = False

    def kill() -> None:
        kill_groups([*(branch() if branch is not None else []), proc.pid], reap=proc)

    try:
        while sel.get_map():
            now = time.monotonic()
            if stop_at is not None and now >= stop_at:
                break
            if stop_at is None and (big or now >= deadline):
                timed_out = not big
                kill()
                stop_at = time.monotonic() + SPRIG_DRAIN_S
            elif not exited_kill and proc.poll() is not None:
                exited_kill = True
                kill()
            for key, _ in sel.select(0.02):
                try:
                    chunk = os.read(key.fd, 65536)
                except BlockingIOError:
                    continue
                if not chunk:
                    sel.unregister(key.fd)
                elif len(bufs[key.fd]) + len(chunk) > OUTPUT_CAP:
                    big = True
                else:
                    bufs[key.fd] += chunk
        if not exited_kill and stop_at is None:
            # Both pipes ended before the child did: wait for it, to the
            # timeout.
            while proc.poll() is None and time.monotonic() < deadline:
                time.sleep(0.02)
            timed_out = proc.poll() is None
            kill()
        proc.wait()
    finally:
        sel.close()
        for pipe in (proc.stdout, proc.stderr):
            with contextlib.suppress(OSError):
                pipe.close()
    return bytes(out_buf), bytes(err_buf), timed_out, big


def _feed(pipe: Any, data: bytes) -> None:
    try:
        pipe.write(data)
    except (BrokenPipeError, OSError):
        pass
    finally:
        with contextlib.suppress(OSError):
            pipe.close()


class _TooBig(Exception):
    """A child wrote more than OUTPUT_CAP on stdout or stderr."""


def kill_groups(groups: list[int], *, reap: Any = None) -> None:
    """Kill these process groups, in order (MX-R-8): SIGTERM to each, so a
    sprig can kill the groups of its own tools, then SIGKILL to each one
    still there after KILL_GRACE_S. Never a group of 1 or less, and never
    this process's own group. ``reap`` is the child to wait on, so its
    exit is seen."""
    mine = os.getpgrp()
    targets = [g for g in dict.fromkeys(groups) if g > 1 and g != mine]

    def signal_all(sig: int) -> list[int]:
        left = []
        for g in targets:
            try:
                os.killpg(g, sig)
                left.append(g)
            except (ProcessLookupError, PermissionError):
                pass
        return left

    alive = signal_all(signal.SIGTERM)
    end = time.monotonic() + KILL_GRACE_S
    while alive and time.monotonic() < end:
        if reap is not None:
            reap.poll()
        still = []
        for g in alive:
            try:
                os.killpg(g, 0)
                still.append(g)
            except (ProcessLookupError, PermissionError):
                pass
        alive = still
        if alive:
            time.sleep(0.02)
    targets = alive
    signal_all(signal.SIGKILL)


class _LastAgentic:
    def __init__(
        self, model: str | None = None, tokens: int | None = None, cost_usd: float | None = None
    ) -> None:
        self.model = model
        self.attempts = 1
        self.tokens = tokens
        self.cost_usd = cost_usd


#: The session of the root an executor's tree mode makes: the caller's
#: envelope and budget. The depth-1 child is its only child.
ROOT_SESSION = "root"

_USAGE_FIELDS = (
    "input_tokens",
    "output_tokens",
    "cache_creation_input_tokens",
    "cache_read_input_tokens",
)


def sprig_gate_program() -> list[str]:
    """The program sprig runs as its gate: this oracle's hook entry.
    sprig runs it in the workspace with the sub-agent's environment, so
    -I (isolated) keeps that directory, PYTHONPATH and the user site off
    sys.path: a module the sub-agent writes there is never imported."""
    return [sys.executable, "-I", "-m", "opendaisugi.gate_client"]


def tree_cmd_base() -> list[str]:
    """The command sprig's spawn tool runs: this oracle's one starter,
    `tree run`, isolated as the gate program is."""
    return [sys.executable, "-I", "-m", "opendaisugi.cli", "tree", "run"]


def has_go_space(words: list[str]) -> bool:
    return any(c in _GO_SPACE for w in words for c in w)


def sprig_gate_words(spawn: bool) -> list[str]:
    """The words sprig splits at whitespace that are not the tree's own
    paths: the gate program, TMPDIR, and the tree command with spawn."""
    words = [*sprig_gate_program(), tempfile.gettempdir()]
    if spawn:
        words += tree_cmd_base()
    return words


def sprig_wall(tools: list[str], envelope: Envelope) -> list[str]:
    """sprig's --tools for a step's host tools: each one the envelope
    backs and sprig has a tool for, in the step's order, once."""
    wall: list[str] = []
    for tool in tools:
        if tool not in SPRIG_TOOLS or _AGENTIC_TOOL_CAPABILITIES.get(tool) is None:
            continue
        if agentic_tool_granted(tool, envelope.permissions) and SPRIG_TOOLS[tool] not in wall:
            wall.append(SPRIG_TOOLS[tool])
    return wall


def child_env(
    environ: Mapping[str, str], profiles: agent_profiles.Profiles, *, sprig: bool
) -> dict[str, str]:
    """A child's environment (MX-R-3): no variable the table names as a
    key_env and no ``*_API_KEY`` or ``*_TOKEN``, for every child; a sprig
    child also loses every ambient key and redirect. A sprig child gets
    its key on a descriptor; a ``claude -p`` child uses its own login."""
    drop = set(profiles.key_names())
    if sprig:
        drop |= set(agent_profiles.AMBIENT)
    return {k: v for k, v in environ.items() if k not in drop and not agent_profiles.is_key_name(k)}


def sprig_start(
    *,
    sprig_binary: str,
    wall: list[str],
    model: str,
    gate_root: Path,
    data_dir: Path,
    session: str,
    profile: agent_profiles.Profile,
    profiled: bool,
    profiles: agent_profiles.Profiles,
    environ: Mapping[str, str],
    capture: bool,
    max_turns: int | None,
    max_tokens: int | None,
) -> tuple[list[str], dict[int, bytes], dict[str, str] | None]:
    """The argv, descriptors and environment of a sprig child: one builder
    for every starter, so a child starts the same whoever starts it. The
    gate is pinned by its own command (--root, --session), never by the
    payload sprig sends."""
    gate_words = [
        *sprig_gate_program(),
        "--mode",
        "enforce",
        "--root",
        str(gate_root),
        "--session",
        session,
    ]
    if capture:
        gate_words += ["--captures-root", str(gate_root / "captures")]
    argv = [
        sprig_binary,
        "--json",
        "--gate",
        "--gate-cmd",
        " ".join(gate_words),
        "--tools",
        ",".join(wall),
        "--model",
        model,
    ]
    fds: dict[int, bytes] = {}
    spawn = "spawn" in wall
    if profiled:
        argv += ["--wire", profile.wire]
        if profile.wire != "claude-code":
            argv += ["--base-url", profile.base_url or agent_profiles.DEFAULT_BASE[profile.wire]]
    if profile.key:
        argv += ["--key-fd", str(KEY_FD)]
        fds[KEY_FD] = profile.key.encode("utf-8", "surrogateescape")
    names = profiles.key_names()
    if (profiled or spawn) and names:
        argv += ["--scrub-env", ",".join(names)]
    if spawn:
        tree_cmd = [*tree_cmd_base(), "--data-dir", str(data_dir), "--root", str(gate_root)]
        argv += ["--spawn-keys-fd", str(SPAWN_KEYS_FD), "--tree-cmd", " ".join(tree_cmd)]
        fds[SPAWN_KEYS_FD] = profiles.bundle()
    argv += ["--session-dir", str(gate_root / "sessions"), "--session", session]
    if max_turns is not None:
        argv += ["--max-turns", str(max_turns)]
    if max_tokens is not None:
        argv += ["--max-tokens", str(max_tokens)]
    argv.append("-")
    return argv, fds, child_env(environ, profiles, sprig=True)


class SprigOutcome:
    """How a sprig child ended: the failure text, or its answer and the
    --json object it printed."""

    def __init__(self, error: str | None, answer: str | None = None, obj: Any = None) -> None:
        self.error = error
        self.answer = answer
        self.obj: dict[str, Any] = obj if isinstance(obj, dict) else {}


def run_sprig_child(
    argv: list[str],
    *,
    task: str,
    cwd: str,
    timeout_s: int,
    env: dict[str, str] | None,
    fds: dict[int, bytes],
    on_start: Callable[[int], None] | None = None,
    branch: Callable[[], list[int]] | None = None,
) -> SprigOutcome:
    """Run a sprig child and read its --json reply; every failure is one
    of the fixed texts the Go and Rust clients copy."""
    run_kw: dict[str, Any] = {}
    if env is not None:
        run_kw["env"] = env
    if fds:
        run_kw["fds"] = fds
    if on_start is not None:
        run_kw["on_start"] = on_start
    if branch is not None:
        run_kw["branch"] = branch
    try:
        rc, out, err, timed_out = _run_sprig(
            argv, stdin=task, cwd=cwd, timeout_s=timeout_s, **run_kw
        )
    except _TooBig:
        return SprigOutcome(SPRIG_TOO_BIG_TEXT)
    except OSError:
        return SprigOutcome(_sprig_start_text(argv[0]))
    if timed_out:
        return SprigOutcome(_sprig_timeout_text(timeout_s))
    if rc != 0:
        return SprigOutcome(_sprig_exit_text(rc, err))
    raw = out.decode("utf-8", "replace")
    try:
        obj = json.loads(raw)
    except json.JSONDecodeError:
        return SprigOutcome(f"agentic sub-agent returned unparseable output: {raw[:300]!r}")
    answer = obj.get("answer") if isinstance(obj, dict) else None
    if not isinstance(answer, str):
        return SprigOutcome(f"agentic sub-agent reply has no answer: {raw[:300]!r}")
    return SprigOutcome(None, answer, obj)


def _meter_tokens(obj: dict[str, Any], *, ints_only: bool) -> int | None:
    usage = obj.get("usage") if isinstance(obj.get("usage"), dict) else {}
    if ints_only:
        counts = [
            v
            for f in _USAGE_FIELDS
            if isinstance(v := usage.get(f), int) and not isinstance(v, bool)
        ]
        return sum(counts) if counts else None
    present = [usage.get(f) for f in _USAGE_FIELDS if usage.get(f) is not None]
    return sum(int(v) for v in present) if present else None


class AgenticExecutor:
    """StepExecutor for :class:`~opendaisugi.models.AgenticStep`.

    Construction::

        AgenticExecutor(envelope=env, model="haiku", capture=True)
        AgenticExecutor(envelope=env, runtime="sprig", tokens=50_000)

    ``envelope`` is the *caller's* envelope — the authorization ceiling. The
    executor re-derives the tool wall from it on every run (it does not
    trust the step), and never grants the sub-agent a capability the
    envelope lacks. ``runtime`` picks the sub-agent: ``claude`` or
    ``sprig``; ``sprig_binary`` defaults to ``DAISUGI_SPRIG``, else
    ``sprig`` on PATH.

    Every step runs as a delegation tree (tree mode): a fresh private
    directory holds ``gate/`` (the gate root) and ``tree/`` (the ledger),
    so the gate guards both. Its root is the caller's envelope with the
    budget ``tokens`` and ``turns`` (none by default); the sub-agent is
    the root's child under the session ``agentic-<step id>``, started by
    the one starter (tree_run.run_node) on sprig.
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
        profiles: Any = None,
        environ: Mapping[str, str] | None = None,
        tokens: int | None = None,
        turns: int | None = None,
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
        self.tokens = tokens
        self.turns = turns
        self.last = _LastAgentic()
        self.last_gate_root: Path | None = None
        # The root starter reads each profile's key variable here, once.
        self.environ = dict(os.environ if environ is None else environ)
        self.profiles = agent_profiles.resolve(profiles, self.environ)

    def _derive_allowed_tools(
        self, step: AgenticStep, envelope: Envelope, runtime: str | None = None
    ) -> list[str]:
        runtime = runtime or self.runtime
        perms = envelope.permissions
        allowed: list[str] = []
        for tool in step.tools:
            cap = _AGENTIC_TOOL_CAPABILITIES.get(tool)
            if cap is None:
                continue  # unknown tool: never forwarded
            if runtime == "sprig" and tool not in SPRIG_TOOLS:
                continue  # sprig has no tool for it: left out, never mapped
            if runtime == "claude" and tool == "Spawn":
                continue  # a claude -p node is a leaf: it starts no children
            if agentic_tool_granted(tool, perms):
                allowed.append(tool)
        return allowed

    def run(self, step, *, timeout_s: int, max_output_bytes: int) -> ExecutorResult:
        if not isinstance(step, AgenticStep):
            raise TypeError(
                f"AgenticExecutor got {type(step).__name__}; wire it under "
                f"the 'agentic' step type only"
            )
        from opendaisugi import tree

        started = time.time()
        self.last = _LastAgentic(model=self.model)
        self.last_gate_root = None
        profile, why = self.profiles.lookup(step.profile)

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
        if profile is None:
            return _fail(f"agentic sub-agent failed: {why}")
        model = step.preferred_model or profile.model or self.model
        self.last = _LastAgentic(model=model)
        # A profile on another wire runs on sprig, whatever --agent says.
        runtime = "sprig" if profile.wire != "claude-code" else self.runtime

        # The edge: the sub-agent's envelope must fit inside the caller's,
        # strict and fail closed, before anything starts. A step with no
        # child envelope restates the caller's own, which fits.
        edge = tree.edge_ok(self.envelope, step.child_envelope or self.envelope)
        if not edge.holds or edge.child is None:
            return _fail("the child envelope is refused: " + "; ".join(edge.reasons))

        allowed = self._derive_allowed_tools(step, edge.child, runtime)
        if not allowed:
            return _fail(
                f"no requested tool is backed by the envelope "
                f"(requested {step.tools!r}); nothing to delegate"
            )

        # sprig splits its gate command at whitespace: a gate program or a
        # tree directory with a space in its path would break the pin, so
        # the step fails before anything is made or started. The tree's own
        # name (mkdtemp) never holds one.
        if runtime == "sprig" and has_go_space(sprig_gate_words("Spawn" in allowed)):
            return _fail(SPRIG_SPACE_TEXT)

        # The tree lives OUTSIDE the workspace on purpose: the sub-agent
        # must not be able to rewrite its hook configuration, its envelope
        # or the ledger mid-session. gate/ and tree/ under one directory,
        # so the gate guards both.
        data_dir = Path(tempfile.mkdtemp(prefix="daisugi-agentic-"))
        gate_root = data_dir / "gate"
        # Private like the directory mkdtemp made: no other user reads the
        # envelopes, the audit log or the ledger.
        gate_root.mkdir(mode=0o700)
        (data_dir / "tree").mkdir(mode=0o700)
        self.last_gate_root = gate_root
        # The executor, not the sub-agent, picks the session the envelope
        # binds to, and pins the gate to it: the sub-agent's payload cannot
        # name another session, and no call falls back to a default.
        session = "agentic-" + _safe_session_id(step.id)
        tree.make_root(
            data_dir, gate_root, self.envelope, ROOT_SESSION, tokens=self.tokens, turns=self.turns
        )
        try:
            if runtime == "sprig":
                res = self._run_sprig_node(
                    step,
                    workspace,
                    data_dir,
                    gate_root,
                    session,
                    timeout_s,
                    model,
                    _fail,
                    max_output_bytes,
                )
            else:
                res = self._run_claude_node(
                    step,
                    allowed,
                    workspace,
                    data_dir,
                    gate_root,
                    session,
                    timeout_s,
                    model,
                    _fail,
                    max_output_bytes,
                )
        finally:
            tree.end_subtree(data_dir, gate_root, session)
            with contextlib.suppress(tree.TreeError):
                tree.end(data_dir, gate_root, ROOT_SESSION, tokens_used=0, turns_used=0)
        if res.rc != 0:
            self._cleanup(data_dir)
            return res
        return dataclasses.replace(res, duration_ms=(time.time() - started) * 1000.0)

    def _run_sprig_node(
        self,
        step,
        workspace,
        data_dir,
        gate_root,
        session,
        timeout_s,
        model,
        _fail,
        max_output_bytes,
    ) -> ExecutorResult:
        from opendaisugi import tree_run

        node = tree_run.run_node(
            data_dir,
            gate_root,
            parent=ROOT_SESSION,
            session=session,
            envelope=step.child_envelope or self.envelope,
            profiles=self.profiles,
            profile_name=step.profile,
            model=model,
            tokens=self.tokens,
            turns=self.turns,
            parent_tokens_used=None,
            parent_turns_used=None,
            task=step.prompt,
            tools=list(step.tools),
            max_turns=step.max_turns,
            cwd=str(workspace),
            timeout_s=timeout_s,
            sprig_binary=self.sprig_binary,
            environ=self.environ,
            default_model=self.model,
            capture=self.capture,
        )
        if node.status in ("refused", "asked"):
            return _fail("the child envelope is refused: " + "; ".join(node.reasons))
        if node.status != "started":
            return _fail(node.reasons[0])
        self.last = _LastAgentic(
            model=model, tokens=_meter_tokens(node.obj, ints_only=True), cost_usd=None
        )
        return ExecutorResult(
            rc=0,
            stdout=truncate_output(node.answer or "", max_output_bytes),
            duration_ms=0.0,
            timed_out=False,
        )

    def _run_claude_node(
        self,
        step,
        allowed,
        workspace,
        data_dir,
        gate_root,
        session,
        timeout_s,
        model,
        _fail,
        max_output_bytes,
    ) -> ExecutorResult:
        from opendaisugi import tree

        sp = tree.spawn(
            data_dir,
            gate_root,
            step.child_envelope or self.envelope,
            parent=ROOT_SESSION,
            session=session,
            tokens=self.tokens,
            turns=self.turns,
            profile=step.profile if step.profile not in (None, agent_profiles.DEFAULT) else None,
            model=model,
        )
        if sp.status != "started":
            return _fail("the child envelope is refused: " + "; ".join(sp.reasons))

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
        max_turns = step.max_turns
        if self.turns is not None:
            max_turns = self.turns if max_turns is None else min(max_turns, self.turns)
        if max_turns is not None:
            extra_args += ["--max-turns", str(max_turns)]

        claude_kw: dict[str, Any] = {}
        claude_kw["env"] = child_env(self.environ, self.profiles, sprig=False)
        try:
            raw = call_claude_p_sync(
                step.prompt,
                timeout_s=float(timeout_s),
                model=model,
                binary=self.binary,
                cwd=str(workspace),
                extra_args=tuple(extra_args),
                **claude_kw,
            )
        except Exception as exc:  # noqa: BLE001 — a dead sub-agent is a failed step
            return _fail(f"agentic sub-agent failed: {exc}")

        try:
            obj = json.loads(raw)
        except json.JSONDecodeError:
            return _fail(f"agentic sub-agent returned unparseable output: {raw[:300]!r}")
        tokens = _meter_tokens(obj, ints_only=False)
        self.last = _LastAgentic(model=model, tokens=tokens, cost_usd=obj.get("total_cost_usd"))

        result_text = str(obj.get("result", "") or "")
        if obj.get("is_error"):
            return _fail(f"agentic sub-agent reported is_error: {result_text[:500]}")
        own = tree.reported_tokens(obj.get("usage"))
        turns = obj.get("num_turns")
        turns = turns if isinstance(turns, int) and not isinstance(turns, bool) else None
        tree.end(
            data_dir,
            gate_root,
            session,
            tokens_used=own,
            turns_used=None if turns is None else max(0, min(turns, tree.MAX_COUNT)),
        )
        return ExecutorResult(
            rc=0,
            stdout=truncate_output(result_text, max_output_bytes),
            duration_ms=0.0,
            timed_out=False,
        )

    # Kept for callers of the old names.
    _sprig_gate_program = staticmethod(sprig_gate_program)
    _tree_cmd = staticmethod(tree_cmd_base)

    def _cleanup(self, data_dir: Path) -> None:
        if not self.keep_gate_root:
            shutil.rmtree(data_dir, ignore_errors=True)
