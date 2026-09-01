"""Herdr as a pane backend, driven through its CLI.

Master spec 5.5 keeps Herdr first-class. A Herdr user gets exact `blocked`
from our gate on day one, and a coppice user can fall back to Herdr without
relearning a vocabulary.

Two facts are PINNED in `herdr_verbs.json` rather than hardcoded, because
Herdr's own CLI reference contradicted the sub-spec:

* There is no `herdr pane create`. A pane comes from `tab create`, or from
  `pane split`, then `pane run <pane_id> <command>`, then `pane rename` for
  the label.
* `--json` is documented on `session list`, `agent explain`, and a handful of
  others. It is NOT documented on `pane list` or `agent list`, the two this
  backend reads most. So every list call tries `--json` first and falls back
  to parsing the text table.

The pin carries `verified: false` until a box with `herdr` on it confirms
both. Read the pin, never the sub-spec, for the argv.

State source: `gate` only when `herdr agent explain <target> --json` names
`daisugi` at the PINNED authority path, compared exactly. Anything else is
`manifest`. A substring search would stamp Herdr's screen guess as a gate
fact, and master spec 3.1 ranks a gate fact above a screen guess, so this
fails closed: no field, no dict, no match, no `gate`.

A blocked row is the one exception. Herdr's agent list carries no Ask, and
PaneStateEvent refuses a gate source on a blocked state with no Ask. A block
Herdr attributes to daisugi is reported as manifest instead, with a detail
that says where it really came from, so the operator still learns it.

Herdr's `idle|working|blocked|unknown` map to ours 1:1. Our `done` maps to
Herdr's `unknown`, not `idle`: Herdr has no `done`, and `idle` in Herdr's UI
means "ready for work", which a pane whose process exited is not. The word
`done` rides along in `--message` so a Herdr user still sees it.
"""

from __future__ import annotations

import json
import shutil
import subprocess
import threading
import time
from collections.abc import Callable, Iterator
from dataclasses import dataclass
from pathlib import Path
from typing import Literal

from opendaisugi.floor import Frame, PaneInfo, PaneRef, PaneStateEvent, merge

_PIN = Path(__file__).with_name("herdr_verbs.json")
# Used only when the pin is unreadable. The pin is the source of truth.
_DEFAULT_ARGV = {
    "read": ["pane", "read"],
    "send_text": ["pane", "send-text"],
    "send_keys": ["pane", "send-keys"],
    "close": ["pane", "close"],
    "list_panes": ["pane", "list"],
    "list_agents": ["agent", "list"],
    "prompt": ["agent", "prompt"],
    "explain": ["agent", "explain"],
    "report_agent": ["pane", "report-agent"],
}
_PROBE_S = 0.5
_CALL_S = 10.0
_POLL_S = 1.0
_GATE_BLOCKED_DETAIL = "reported by herdr as gate-blocked"

# Our states map onto the four Herdr accepts on `pane report-agent --state`.
# `done` maps to `unknown`, never `idle`: `idle` in Herdr's UI reads as
# "ready for work", and a pane whose process exited is not that. The real
# word goes in `--message`.
STATE_OUT: dict[str, str] = {
    "idle": "idle",
    "working": "working",
    "blocked": "blocked",
    "done": "unknown",
    "unknown": "unknown",
}


@dataclass(frozen=True)
class HerdrVerbs:
    verified: bool
    source: str
    spawn_chain: tuple[str, ...]
    json_commands: frozenset[str]
    states: tuple[str, ...]
    read_sources: tuple[str, ...]
    pane_env_vars: tuple[str, ...]
    authority_field: tuple[str, ...]
    authority_value: str
    argv: dict[str, tuple[str, ...]]
    keys: dict[str, str]

    def verb(self, name: str) -> list[str]:
        """The pinned argv prefix for one operation. Never a literal in the code."""
        return list(self.argv[name])


def load_verbs(path: Path | None = None) -> HerdrVerbs:
    """Read the pinned Herdr CLI facts. An unreadable pin is an unverified one."""
    try:
        raw = json.loads((path or _PIN).read_text(encoding="utf-8"))
    except (OSError, ValueError):
        raw = {}
    return HerdrVerbs(
        verified=bool(raw.get("verified")),
        source=str(raw.get("source", "")),
        spawn_chain=tuple(raw.get("spawn_chain") or ("tab create", "pane run", "pane rename")),
        json_commands=frozenset(raw.get("json_commands") or ()),
        states=tuple(raw.get("states") or ("idle", "working", "blocked", "unknown")),
        read_sources=tuple(raw.get("read_sources") or ("visible", "recent", "detection")),
        pane_env_vars=tuple(raw.get("pane_env_vars") or ("HERDR_PANE_ID",)),
        authority_field=tuple(raw.get("authority_field") or ("authority", "source")),
        authority_value=str(raw.get("authority_value") or "daisugi"),
        argv={k: tuple(v) for k, v in (raw.get("argv") or _DEFAULT_ARGV).items()},
        keys=dict(raw.get("keys") or {}),
    )


class HerdrBackend:
    """Drive Herdr panes over its CLI. Master spec 3.2 says Herdr yields text, not frames."""

    name = "herdr"
    yields_frames = False

    def __init__(
        self,
        verbs: HerdrVerbs | None = None,
        *,
        timeout_s: float = _PROBE_S,
        gate_states: Callable[[], dict[str, PaneStateEvent]] | None = None,
    ) -> None:
        self.verbs = verbs or load_verbs()
        self._probe_s = timeout_s
        self._gate_states = gate_states or (lambda: {})
        # The backend's own idea of each pane's current state, merged
        # forward the same way tmux's own cache is: a fresh screen guess
        # never overwrites a still-live gate hold outright, it goes
        # through merge() first.
        self._current: dict[str, PaneStateEvent] = {}
        # The ts of the last gate event actually merged in for each pane, so
        # a gate_states() callback that keeps returning the same snapshot
        # does not get re-applied every poll.
        self._gate_seen: dict[str, float] = {}
        # subscribe() polls forever until this is set. close_connection()
        # is the only way to set it from outside the loop.
        self._stopped = False

    # --- process plumbing -------------------------------------------------
    def _run(self, args: list[str], *, timeout_s: float = _CALL_S) -> subprocess.CompletedProcess:
        exe = shutil.which("herdr")
        if exe is None:
            raise FileNotFoundError("herdr is not on PATH")
        return subprocess.run(
            [exe, *args], capture_output=True, text=True, timeout=timeout_s, check=False
        )

    def _list_json(self, verb: list[str]) -> tuple[object | None, str]:
        """Try `--json` when the pin says the verb takes it. Returns the parsed value and text.

        Master §3.2: list() may never raise. A gone herdr binary, or one
        that hangs past its budget, surfaces here as OSError or
        SubprocessError; caught and degraded to no rows, the same as a
        verb that simply exited nonzero.
        """
        try:
            key = " ".join(verb[:2])
            if key in self.verbs.json_commands:
                proc = self._run([*verb, "--json"])
                if proc.returncode == 0:
                    try:
                        return json.loads(proc.stdout), proc.stdout
                    except ValueError:
                        return None, proc.stdout
            proc = self._run(verb)
            return None, proc.stdout if proc.returncode == 0 else ""
        except (OSError, subprocess.SubprocessError):
            return None, ""

    def _pane_ids(self) -> set[str] | None:
        """The pane ids `pane list` reports right now, or None when the
        call itself failed, timed out, or exited nonzero.

        A failed call is not the same fact as a clean answer that happens
        to list no panes: only this method's own None tells them apart.
        subscribe()'s exit-detection reads this, not list(), since list()
        also calls _agent_states() internally to attach a state to each
        row, and asking twice a cycle is not needed to answer this one
        question.

        Falls back to the plain verb when a --json call itself fails, the
        same as _list_json does: a host whose herdr rejects --json must
        not read as permanently unreachable just because the pin says
        this verb takes it.
        """
        verb = self.verbs.verb("list_panes")
        key = " ".join(verb[:2])
        try:
            rows = None
            if key in self.verbs.json_commands:
                proc = self._run([*verb, "--json"])
                if proc.returncode == 0:
                    try:
                        parsed = json.loads(proc.stdout)
                    except ValueError:
                        parsed = None
                    rows = parsed if isinstance(parsed, list) else _parse_table(proc.stdout)
            if rows is None:
                proc = self._run(verb)
                if proc.returncode != 0:
                    return None
                rows = _parse_table(proc.stdout)
        except (OSError, subprocess.SubprocessError):
            return None
        ids = {str(row.get("pane_id") or row.get("id") or row.get("col0") or "") for row in rows}
        ids.discard("")
        return ids

    # --- PaneBackend ------------------------------------------------------
    def available(self) -> bool:
        """`herdr session list --json` exits 0 inside the probe budget. Never raises."""
        try:
            if shutil.which("herdr") is None:
                return False
            proc = self._run(["session", "list", "--json"], timeout_s=self._probe_s)
            return proc.returncode == 0
        except Exception:  # noqa: BLE001
            return False

    def spawn(
        self,
        *,
        cwd: Path,
        cmd: list[str],
        env: dict[str, str],
        label: str,
        kind: Literal["pty", "headless"] = "pty",
        harness: str | None = None,
    ) -> PaneRef:
        """Create a pane through the PINNED chain, then run the command in it.

        `harness` is part of the 3.2 protocol so one call site drives all
        three backends. Herdr picks its own agent kind from the process, so
        it is ignored.
        """
        create, run_verb, rename = (list(v.split()) for v in self.verbs.spawn_chain[:3])
        args = [*create, "--cwd", str(cwd)]
        if label:
            args += ["--label", label]
        for key, value in env.items():
            args += ["--env", f"{key}={value}"]
        proc = self._run(args)
        if proc.returncode != 0:
            raise RuntimeError(f"herdr {' '.join(create)}: {proc.stderr.strip()}")
        pane_id = _parse_pane_id(proc.stdout)
        if not pane_id:
            raise RuntimeError(
                f"herdr {' '.join(create)} printed no pane id: {proc.stdout.strip()!r}"
            )
        if cmd:
            self._run([*run_verb, pane_id, " ".join(cmd)])
        if label:
            self._run([*rename, pane_id, label])
        return PaneRef(backend=self.name, id=pane_id)

    def list(self) -> list[PaneInfo]:
        parsed, text = self._list_json(self.verbs.verb("list_panes"))
        rows = parsed if isinstance(parsed, list) else _parse_table(text)
        agents = self._agent_states()
        out: list[PaneInfo] = []
        for row in rows:
            pane_id = str(row.get("pane_id") or row.get("id") or row.get("col0") or "")
            if not pane_id:
                continue
            ref = PaneRef(self.name, pane_id)
            out.append(
                PaneInfo(
                    ref=ref,
                    label=str(row.get("label") or row.get("col1") or ""),
                    cwd=str(row.get("cwd") or ""),
                    cmd=list(row.get("cmd") or []),
                    kind="pty",
                    state=agents.get(pane_id),
                )
            )
        return out

    def list_proven(self) -> tuple[bool, list[PaneInfo]]:
        """Whether pane list truly answered, and what it said.

        _pane_ids() proves whether the call itself succeeded, the same
        check subscribe()'s own exit detection uses. list() is then read
        for the actual rows, a separate round trip: an empty answer from
        that second call, after _pane_ids() proved panes exist, is a
        failure of its own, not proof every pane exited in the instant
        between the two calls.
        """
        ids = self._pane_ids()
        if ids is None:
            return False, []
        rows = self.list()
        if ids and not rows:
            return False, []
        return True, rows

    def send_text(self, pane: PaneRef, text: str, *, enter: bool = True) -> None:
        self._run([*self.verbs.verb("send_text"), pane.id, text])
        if enter:
            self.send_keys(pane, ["enter"])

    def send_keys(self, pane: PaneRef, keys: list[str]) -> None:
        """Map our key names through the pin, then send them. A rejection raises.

        `_run` ignores the return code everywhere else, which is fine for a
        read. Here it is not: a key Herdr refuses would be silently dropped,
        and the contract suite's send-keys test would pass on a no-op.
        """
        mapped: list[str] = []
        for key in keys:
            name = self.verbs.keys.get(key.lower())
            if name is None and len(key) != 1:
                raise ValueError(
                    f"unknown key {key!r}. Known keys: {', '.join(sorted(self.verbs.keys))}, "
                    f"or one printable character."
                )
            mapped.append(name or key)
        proc = self._run([*self.verbs.verb("send_keys"), pane.id, *mapped])
        if proc.returncode != 0:
            raise RuntimeError(f"herdr rejected the keys {' '.join(mapped)}: {proc.stderr.strip()}")

    def read(
        self, pane: PaneRef, *, source: Literal["visible", "recent", "detection"] = "visible"
    ) -> str:
        """The pane's text from source, or the empty string when the host is gone."""
        if source not in self.verbs.read_sources:
            raise ValueError(
                f"herdr has no read source {source!r}. "
                f"Known sources: {', '.join(self.verbs.read_sources)}."
            )
        try:
            proc = self._run([*self.verbs.verb("read"), pane.id, "--source", source])
        except (OSError, subprocess.SubprocessError):
            return ""
        return proc.stdout if proc.returncode == 0 else ""

    def resize(self, pane: PaneRef, cols: int, rows: int) -> None:
        """A no-op. Herdr owns its layout, and its resize is directional, not sized."""
        return None

    def close(self, pane: PaneRef) -> None:
        """Close one pane. A host already gone means close() already achieved
        what it was asked for."""
        try:
            self._run([*self.verbs.verb("close"), pane.id])
        except (OSError, subprocess.SubprocessError):
            return None

    def report_state(self, pane: PaneRef, ev: PaneStateEvent) -> None:
        """Push our state to Herdr. Never raises: a vanished or wedged herdr
        binary degrades the same way close() does, since nothing here
        waits on this call's result or can act on a failure."""
        mapped = STATE_OUT.get(ev.state)
        if mapped is None:
            return
        args = [
            *self.verbs.verb("report_agent"),
            pane.id,
            "--source",
            self.verbs.authority_value,
            "--agent",
            ev.harness,
            "--state",
            mapped,
        ]
        # Herdr has no `done`. Say it in the message so a Herdr user still sees it.
        message = f"done: {ev.detail}".strip(": ") if ev.state == "done" else ev.detail
        if message:
            args += ["--message", message[:200]]
        try:
            self._run(args)
        except (OSError, subprocess.SubprocessError):
            return

    def prompt(
        self, pane: PaneRef, text: str, *, wait: bool = False, timeout_s: float = 60.0
    ) -> str:
        args = [*self.verbs.verb("prompt"), pane.id, text]
        if wait:
            args += ["--wait", "--timeout", str(int(timeout_s * 1000))]
        self._run(args, timeout_s=timeout_s + 5.0)
        return "sent"

    def subscribe(
        self, *, ready: threading.Event | None = None
    ) -> Iterator[PaneStateEvent | Frame]:
        """Poll `pane list` and `agent list` every second. Herdr has no event stream we can tail.

        Herdr's own states, `idle`, `working`, `blocked` and `unknown`,
        never include `done`: Herdr has no notion of a pane's process
        exiting. A pane that leaves `pane list` between two polls is a
        process fact all the same, the same one tmux emits for a pane
        that vanishes from `list-panes`. `_pane_ids()` is what reads
        that, not `list()`: `list()` also calls `_agent_states()`
        internally to attach a state to each row, so calling it here too
        would ask the host for agent states twice every cycle for no
        reason. A `pane list` poll that itself failed, timed out or
        exited nonzero, returns `None` from `_pane_ids()` and skips the
        rest of that cycle: an empty answer from a failed call is not
        proof every pane exited.

        close_connection() stops the loop; cleared at the top here, so a
        second call to subscribe() after a previous one was stopped starts
        polling again rather than exiting at once.

        ready, when given, is set once the first poll has actually
        reached herdr, not merely been attempted. The event is set from
        inside this generator, so it only fires while a caller is
        iterating it. Cleared at the top, the same as _stopped, so a
        caller that reuses one Event across two subscribe() calls never
        reads a leftover set flag as a fresh baseline.
        """
        self._stopped = False
        if ready is not None:
            ready.clear()
        seen: set[str] = set()
        last: dict[str, str] = {}
        signaled = False
        while not self._stopped:
            alive = self._pane_ids()
            if alive is None:
                time.sleep(_POLL_S)
                continue
            for pane_id in sorted(seen - alive):
                last.pop(pane_id, None)
                yield PaneStateEvent(
                    session_id="",
                    harness="unknown",
                    state="done",
                    source="process",
                    ts=time.time(),
                    pane=pane_id,
                    detail="the pane's process exited",
                )
            seen = alive
            for pane_id, ev in self._agent_states().items():
                if last.get(pane_id) != ev.state:
                    last[pane_id] = ev.state
                    yield ev
            if ready is not None and not signaled:
                ready.set()
                signaled = True
            time.sleep(_POLL_S)

    def close_connection(self) -> None:
        """Stop the poll loop subscribe() runs. Nothing else is held open.

        The loop notices within one poll interval of this call, plus
        however long whatever herdr call is already in flight takes to
        return. One cycle can make several calls, each up to _CALL_S
        (10.0s), so the worst case is well past a flat one poll interval.
        """
        self._stopped = True

    # --- state ------------------------------------------------------------
    def _authority_is_daisugi(self, pane_id: str) -> bool:
        """`agent explain --json` says who decided. Only the gate may speak as the gate.

        Walks the PINNED path and compares exactly. Never a substring search
        over the document: `daisugi` appears in a cwd like `/work/openDaisugi`,
        in a pane label, and in a command line, and matching any of those
        would stamp Herdr's screen guess with the second-highest rank in
        master spec 3.1. Missing field, wrong shape, or a different name all
        return False, so the state falls back to `manifest`.
        """
        parsed, _ = self._list_json([*self.verbs.verb("explain"), pane_id])
        node = parsed
        for key in self.verbs.authority_field:
            if not isinstance(node, dict) or key not in node:
                return False
            node = node[key]
        if isinstance(node, (dict, list)):
            return False
        return str(node).strip().casefold() == self.verbs.authority_value.casefold()

    def _agent_states(self) -> dict[str, PaneStateEvent]:
        """Herdr's own screen guess for each pane, merged behind whatever the
        gate itself already reported for that pane.

        Master 5.1 ranks a gate fact above a screen guess the same way on
        every backend. gate_states(), when it names a newer event for this
        pane than the last one already merged in, is folded in first,
        through the same merge() tmux's own cache uses; Herdr's own guess
        for this cycle is then merged on top of that, so a gate hold still
        outranks a stale or wrong screen read.
        """
        parsed, text = self._list_json(self.verbs.verb("list_agents"))
        rows = parsed if isinstance(parsed, list) else _parse_table(text)
        now = time.time()
        gate = self._gate_states()
        out: dict[str, PaneStateEvent] = {}
        for row in rows:
            pane_id = str(row.get("pane_id") or row.get("col0") or "")
            harness = str(row.get("agent") or row.get("col1") or "unknown")
            state = str(row.get("state") or row.get("col2") or "unknown").strip().lower()
            if not pane_id or state not in self.verbs.states:
                continue
            authored_by_daisugi = self._authority_is_daisugi(pane_id)
            if state == "blocked":
                # agent list carries no Ask id, tool, summary or deadline, so
                # a blocked row can never be a gate source: PaneStateEvent
                # requires an Ask whenever source is gate and state is
                # blocked. Report the honest screen fact instead, and say in
                # the detail when Herdr itself thinks the block is ours.
                source = "manifest"
                detail = (
                    _GATE_BLOCKED_DETAIL
                    if authored_by_daisugi
                    else f"herdr agent list says {source}"
                )
            else:
                source = "gate" if authored_by_daisugi else "manifest"
                detail = f"herdr agent list says {source}"
            event = PaneStateEvent(
                session_id="",
                harness=harness,
                state=state,
                source=source,
                ts=now,
                pane=pane_id,
                detail=detail,
            )
            prior = self._current.get(pane_id)
            gate_event = gate.get(pane_id)
            if gate_event is not None and gate_event.ts > self._gate_seen.get(pane_id, 0.0):
                self._gate_seen[pane_id] = gate_event.ts
                prior = merge(prior, gate_event, now=now)
            prior = merge(prior, event, now=now)
            self._current[pane_id] = prior
            out[pane_id] = prior
        return out


def _parse_pane_id(text: str) -> str:
    """Take a pane id off `tab create` output, whatever shape it prints."""
    for line in text.splitlines():
        line = line.strip()
        if not line:
            continue
        if ":" in line:
            key, _, value = line.partition(":")
            if "pane" in key.casefold():
                return value.strip()
        elif " " not in line:
            return line
    return ""


def _parse_table(text: str) -> list[dict]:
    """Fallback for a verb with no `--json`: split a whitespace table into col0..colN."""
    rows: list[dict] = []
    for line in text.splitlines():
        cells = [c for c in line.replace("\t", " ").split(" ") if c]
        if not cells or cells[0].casefold() in ("pane", "pane_id", "id"):
            continue
        rows.append({f"col{i}": cell for i, cell in enumerate(cells)})
    return rows
