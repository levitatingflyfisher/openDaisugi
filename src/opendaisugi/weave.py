"""weave: run a verified plan tree on the supervisor, with typed slots (ADR-0022).

``daisugi weave PLAN.json -e ENVELOPE`` runs an ``ActionPlan`` through the
same supervisor as ``daisugi run``: the whole plan is verified first, each
step is verified again before it runs, and each step that runs writes a
receipt. weave adds four things.

- **Typed slots (WV-1).** A step may declare typed ``outputs``; a later step
  names a slot in its ``inputs``. The runner reads the slot values from the
  producing step's output (one JSON object), checks each value's type, fills
  the later step, and the supervisor verifies the filled step again before
  it runs. A slot value changes data only: a filled path keeps its directory
  and a filled URL keeps its scheme and host (the K1 bind rule,
  ``pathway_params._capability_head``). No slot fills a command, a prompt or
  a program, so free text never flows from one step into another.
- **The router** gives each ``task`` step with no ``preferred_model`` its
  model, by the rule ``daisugi route`` uses, without the pathway store.
- **Resume (WV-2).** Each step that starts is marked in
  ``<data dir>/weave/sha256-<plan hash>.jsonl`` before its executor runs.
  With ``--resume``, a step with a succeeded receipt from an earlier run of
  the same plan file is skipped (its slots are read from that receipt); a
  ``file_read`` or ``network`` step runs again; a step that started and has
  no receipt at all may have run, so the run stops before it unless
  ``--rerun`` names it (a ``task`` step, which has no effect, runs again).
- **Agentic steps (WV-4)** run through ``AgenticExecutor`` (``claude -p``
  under the call-time gate).
- **Attempts.** A ``task`` step may carry ``attempts: N`` (2 to 8). The
  runner asks the model N times, ranks the answers with ``rank`` (an answer
  that fails or whose slots do not read is out), goes on with the leader,
  and records the choice as a card in the review queue. A choice never
  stops the run, except before a step that cannot be undone: that step goes
  to the operator's ask (a terminal prompt), even under ``--yes``.

The plan hash is the SHA-256 of the plan file's bytes.
"""

from __future__ import annotations

import hashlib
import json
import math
import os
import re
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from opendaisugi.models import ActionPlan
from opendaisugi.run_session import RunStatus, StepOutcome

SLOT_TYPES = ("path", "string", "number", "list[path]", "list[string]", "list[number]")
#: Step types whose output may fill slots: each gives one text output.
OUTPUT_KINDS = ("shell", "file_read", "network", "task")
#: The fields a slot may fill, by step type, and the slot types each takes.
#: A path keeps its directory and a URL its host; content is data.
INPUT_FIELDS: dict[str, dict[str, tuple[str, ...]]] = {
    "file_read": {"path": ("path",)},
    "file_write": {"path": ("path",), "content": SLOT_TYPES},
    "network": {"url": ("string",)},
}
#: Step kinds whose output is model text. Their slot values are tainted:
#: they may fill a path or a URL under the directory and host rules, never
#: a file's content, which a later step may run.
MODEL_KINDS = ("task", "agentic")
#: Step kinds that run again on resume: they change nothing.
RERUN_KINDS = ("file_read", "network")
#: A step kind that has no effect: started with no receipt, it runs again.
NO_EFFECT_KINDS = ("task",)
_SLOT_NAME = re.compile(r"[a-z][a-z0-9_]{0,31}")
MAX_TEXT = 4096
MAX_LIST = 64
MAX_INT = 2**53


MIN_ATTEMPTS = 2
MAX_ATTEMPTS = 8
_ATTEMPT_STEP_ID = re.compile(r"[A-Za-z0-9._-]{1,29}")


class WeaveError(ValueError):
    """A plan whose slots do not check; nothing has run."""


@dataclass
class SlotSpec:
    """Each step's declared outputs and inputs, from the plan file."""

    outputs: dict[str, dict[str, str]] = field(default_factory=dict)
    inputs: dict[str, dict[str, tuple[str, str]]] = field(default_factory=dict)
    attempts: dict[str, int] = field(default_factory=dict)


def plan_hash(raw: bytes) -> str:
    return "sha256:" + hashlib.sha256(raw).hexdigest()


def _ancestors(plan: ActionPlan, sid: str) -> set[str]:
    deps = {s.id: list(s.depends_on) for s in plan.steps}
    seen: set[str] = set()
    todo = list(deps.get(sid, []))
    while todo:
        d = todo.pop()
        if d in seen:
            continue
        seen.add(d)
        todo.extend(deps.get(d, []))
    return seen


def read_slots(raw_steps: list[Any], plan: ActionPlan) -> SlotSpec:
    """The slot declarations of a plan file's steps, checked. Raises
    WeaveError at the first problem."""
    spec = SlotSpec()
    by_id = {s.id: s for s in plan.steps}
    for raw, step in zip(raw_steps, plan.steps, strict=True):
        if not isinstance(raw, dict):
            continue
        sid = step.id
        outs = raw.get("outputs")
        if outs is not None:
            if not isinstance(outs, dict):
                raise WeaveError(f"step {sid}: outputs must be an object")
            if outs and step.type not in OUTPUT_KINDS:
                raise WeaveError(f"step {sid}: a {step.type} step cannot declare outputs")
            for name, typ in outs.items():
                if not _SLOT_NAME.fullmatch(name):
                    raise WeaveError(
                        f"step {sid}: a slot name is 1 to 32 of a-z 0-9 _, "
                        f"starting with a letter: {json.dumps(name)}"
                    )
                if typ not in SLOT_TYPES:
                    raise WeaveError(
                        f"step {sid}: slot {name} has type {json.dumps(typ)}; "
                        f"the types are {', '.join(SLOT_TYPES)}"
                    )
            spec.outputs[sid] = dict(outs)
    for raw, step in zip(raw_steps, plan.steps, strict=True):
        if not isinstance(raw, dict):
            continue
        sid = step.id
        ins = raw.get("inputs")
        if ins is None:
            continue
        if not isinstance(ins, dict):
            raise WeaveError(f"step {sid}: inputs must be an object")
        fields = INPUT_FIELDS.get(step.type, {})
        up = _ancestors(plan, sid)
        got: dict[str, tuple[str, str]] = {}
        for fname, ref in ins.items():
            if fname not in fields:
                raise WeaveError(
                    f"step {sid}: a slot cannot fill {step.type}.{fname}; a slot fills "
                    "data only (a file path, a file's content, a URL), never a command "
                    "or a prompt"
                )
            if not isinstance(ref, str) or "." not in ref:
                raise WeaveError(f"step {sid}: input {fname} must name a slot as STEP.SLOT")
            src, slot = ref.rsplit(".", 1)
            if src not in by_id:
                raise WeaveError(f"step {sid}: input {fname} names no step {json.dumps(src)}")
            if src not in up:
                raise WeaveError(
                    f"step {sid}: input {fname} names step {src}, which it does not depend on"
                )
            typ = spec.outputs.get(src, {}).get(slot)
            if typ is None:
                raise WeaveError(f"step {sid}: step {src} declares no slot {json.dumps(slot)}")
            if typ not in fields[fname]:
                raise WeaveError(
                    f"step {sid}: {step.type}.{fname} takes {' or '.join(fields[fname])}, "
                    f"not {typ} ({ref})"
                )
            if fname == "content" and by_id[src].type in MODEL_KINDS:
                raise WeaveError(
                    f"step {sid}: {step.type}.{fname} cannot take {ref}: {src} is a "
                    f"{by_id[src].type} step, and model text never fills a file's content "
                    "(it may fill a path or a URL)"
                )
            if fname in ("path", "url") and _head(step.type, getattr(step, fname)) is None:
                raise WeaveError(
                    f"step {sid}: {step.type}.{fname} {json.dumps(getattr(step, fname))} has no "
                    f"{'directory' if fname == 'path' else 'host'} for a slot to keep"
                )
            got[fname] = (src, slot)
        if got:
            spec.inputs[sid] = got
    for raw, step in zip(raw_steps, plan.steps, strict=True):
        if not isinstance(raw, dict) or raw.get("attempts") is None:
            continue
        sid = step.id
        n = raw["attempts"]
        if step.type != "task":
            raise WeaveError(f"step {sid}: only a task step runs attempts")
        if not isinstance(n, int) or isinstance(n, bool) or not MIN_ATTEMPTS <= n <= MAX_ATTEMPTS:
            raise WeaveError(
                f"step {sid}: attempts must be a whole number from {MIN_ATTEMPTS} to {MAX_ATTEMPTS}"
            )
        if not _ATTEMPT_STEP_ID.fullmatch(sid):
            raise WeaveError(
                f"step {sid}: a step with attempts needs an id of 1 to 29 of A-Z a-z 0-9 . _ -"
            )
        spec.attempts[sid] = n
    return spec


def _head(step_type: str, value: str) -> str | None:
    from opendaisugi.pathway_params import _capability_head

    return _capability_head(step_type, value)


# ---------------------------------------------------------------------------
# Reading slot values
# ---------------------------------------------------------------------------


def _check_value(typ: str, v: Any) -> bool:
    if typ.startswith("list["):
        inner = typ[5:-1]
        return isinstance(v, list) and len(v) <= MAX_LIST and all(_check_value(inner, x) for x in v)
    if typ == "number":
        if isinstance(v, bool):
            return False
        if isinstance(v, int):
            return -MAX_INT <= v <= MAX_INT
        return isinstance(v, float) and math.isfinite(v)
    if not isinstance(v, str) or len(v) > MAX_TEXT or "\x00" in v:
        return False
    if typ == "string":
        return True
    # path
    return (
        v.startswith("/")
        and ".." not in v.split("/")
        and not any(ord(c) < 32 or ord(c) == 127 for c in v)
    )


def collect(outputs: dict[str, str], stdout: str, *, fence: bool) -> dict[str, Any] | str:
    """The slot values in a step's output, or why there are none."""
    from opendaisugi.delegate import _strip_fence

    text = _strip_fence(stdout) if fence else stdout.strip()
    try:
        obj = json.loads(text)
    except (ValueError, RecursionError):
        return "the output is not one JSON object"
    if not isinstance(obj, dict):
        return "the output is not one JSON object"
    got: dict[str, Any] = {}
    for name, typ in outputs.items():
        if name not in obj:
            return f"the output has no slot {name}"
        if not _check_value(typ, obj[name]):
            return f"slot {name} is not a {typ}"
        got[name] = obj[name]
    return got


# ---------------------------------------------------------------------------
# The state file and resume
# ---------------------------------------------------------------------------


def state_path(data_dir: Path, digest: str) -> Path:
    return data_dir / "weave" / (digest.replace(":", "-") + ".jsonl")


@dataclass
class Prior:
    """What earlier runs of the same plan file left: the runs, the runs
    that marked each step started, each step's latest succeeded receipt,
    and every (run, step) with a receipt of any kind."""

    runs: list[str] = field(default_factory=list)
    started: dict[str, list[str]] = field(default_factory=dict)
    done: dict[str, tuple[str, str]] = field(default_factory=dict)
    receipted: set[tuple[str, str]] = field(default_factory=set)

    def unreceipted(self, sid: str) -> str | None:
        """The first run that marked ``sid`` started and holds no receipt
        for it: the step may have run there."""
        for run in self.started.get(sid, []):
            if (run, sid) not in self.receipted:
                return run
        return None


def read_prior(path: Path, journal: Any) -> Prior:
    """The runs named in the state file, the steps each started, and each
    step's latest succeeded receipt among those runs."""
    prior = Prior()
    try:
        text = path.read_text(encoding="utf-8")
    except FileNotFoundError:
        return prior
    for line in text.splitlines():
        try:
            rec = json.loads(line)
        except ValueError:
            continue
        if not isinstance(rec, dict):
            continue
        run, sid = rec.get("run"), rec.get("step")
        if not isinstance(run, str) or not isinstance(sid, str):
            continue
        if run not in prior.runs:
            prior.runs.append(run)
        marked = prior.started.setdefault(sid, [])
        if run not in marked:
            marked.append(run)
    for run in prior.runs:
        for r in journal.receipts_for_run(run):
            prior.receipted.add((run, r.step_id))
            ev = r.evidence if isinstance(r.evidence, dict) else {}
            if r.verify_result and ev.get("status") == "succeeded":
                out = ev.get("stdout")
                prior.done[r.step_id] = (run, out if isinstance(out, str) else "")
    return prior


def _outcome(sid: str, status: str, error: str | None) -> StepOutcome:
    from opendaisugi.supervisor import _now_iso

    return StepOutcome(
        step_id=sid,
        status=status,  # type: ignore[arg-type]
        approved_by=None,
        rc=None,
        stdout="",
        duration_ms=0.0,
        started_at=_now_iso(),
        error=error,
    )


class WeaveHook:
    """The supervisor hook: resume, slot filling, start marks, slot reading."""

    def __init__(
        self,
        spec: SlotSpec,
        *,
        state: Path,
        prior: Prior | None,
        rerun: set[str],
        plan: ActionPlan | None = None,
        envelope: Any = None,
        z3_timeout_ms: int = 500,
        data_dir: Path | None = None,
        digest: str = "",
        project: str = "",
    ) -> None:
        self.spec = spec
        self.state = state
        self.prior = prior
        self.rerun = rerun
        self.plan = plan
        self.envelope = envelope
        self.z3_timeout_ms = z3_timeout_ms
        # Each step as it runs: a filled step replaces its placeholder, so
        # the whole-plan checks see every value filled so far.
        self.current: dict[str, Any] = {s.id: s for s in plan.steps} if plan else {}
        self.slots: dict[str, dict[str, Any]] = {}
        self.skipped: dict[str, str] = {}
        self.filled: dict[str, dict[str, Any]] = {}
        self.data_dir = data_dir
        self.digest = digest
        self.project = project
        self.run_id: str | None = None
        # The choice each step with attempts made in this run.
        self.choices: dict[str, dict[str, Any]] = {}
        # Open cards this run picked up on resume, whose ``resumed`` row is
        # written once the run has an id.
        self.resumed_cards: list[tuple[str, str]] = []

    def _resume_verdict(self, step: Any) -> str:
        """run, skip, or ask, for a step on resume."""
        if self.prior is None or step.type in RERUN_KINDS:
            return "run"
        if step.id in self.prior.done:
            return "skip"
        if (
            self.prior.unreceipted(step.id) is not None
            and step.type not in NO_EFFECT_KINDS
            and step.id not in self.rerun
        ):
            return "ask"
        return "run"

    def prefetchable(self, step: Any) -> bool:
        return (
            not self.spec.attempts
            and step.id not in self.spec.inputs
            and self._resume_verdict(step) == "run"
        )

    def prepare(self, step: Any) -> Any:
        verdict = self._resume_verdict(step)
        if verdict == "skip":
            run, stdout = self.prior.done[step.id]  # type: ignore[union-attr]
            outs = self.spec.outputs.get(step.id)
            if outs:
                got = collect(outs, stdout, fence=step.type == "task")
                if isinstance(got, str):
                    return (
                        _outcome(step.id, "aborted", f"resume: the receipt in {run}: {got}"),
                        RunStatus.ABORTED,
                    )
                self.slots[step.id] = got
            self.skipped[step.id] = run
            self._open_card_of(step)
            return _outcome(step.id, "skipped", None)
        if verdict == "ask":
            run = self.prior.unreceipted(step.id)  # type: ignore[union-attr]
            return (
                _outcome(
                    step.id,
                    "aborted",
                    f"resume: step {step.id} ({step.type}) started in {run} and has no "
                    "receipt, so it may have run. Check it, then run again with "
                    f"--rerun {step.id}",
                ),
                RunStatus.ABORTED,
            )
        ins = self.spec.inputs.get(step.id)
        if not ins:
            return step
        update: dict[str, Any] = {}
        for fname, (src, slot) in ins.items():
            if src not in self.slots or slot not in self.slots[src]:
                return (
                    _outcome(
                        step.id, "rejected_halted", f"rejected: slot {src}.{slot} has no value"
                    ),
                    RunStatus.HALTED_BY_SIMPLEX,
                )
            value = self.slots[src][slot]
            if fname == "content":
                update[fname] = value if isinstance(value, str) else json.dumps(value)
                continue
            old = getattr(step, fname)
            if _head(step.type, value) != _head(step.type, old):
                what = "directory" if fname == "path" else "scheme and host"
                return (
                    _outcome(
                        step.id,
                        "rejected_halted",
                        f"rejected: slot {src}.{slot} would change the {what} of "
                        f"{step.type}.{fname}: {json.dumps(value)}",
                    ),
                    RunStatus.HALTED_BY_SIMPLEX,
                )
            update[fname] = value
        self.filled[step.id] = update
        return step.model_copy(update=update)

    def checked(self, step: Any) -> Any:
        """After a filled step passes the per-step verify: the per-step
        verify skips the plan-level checks (predicate invariants among
        them), and the whole-plan verify saw only the placeholder, so verify
        the plan again whole with every value filled so far."""
        if step.id not in self.filled or self.plan is None or self.envelope is None:
            return None
        from opendaisugi.verify import verify

        self.current[step.id] = step
        again = self.plan.model_copy(
            update={"steps": [self.current[s.id] for s in self.plan.steps]}
        )
        result = verify(again, self.envelope, z3_timeout_ms=self.z3_timeout_ms)
        if result.ok:
            return None
        first = result.violations[0].message if result.violations else "rejected"
        return (
            _outcome(step.id, "rejected_halted", f"rejected: the filled plan: {first}"),
            RunStatus.HALTED_BY_SIMPLEX,
        )

    def started(self, step: Any, run_id: str) -> str | None:
        """Mark the step started, synced; why not, when the mark failed."""
        self.run_id = run_id
        if self.resumed_cards and self.data_dir is not None:
            from opendaisugi import rank

            t = rank.now()
            rows = [
                {"choice_id": cid, "ranking_id": rid, "event": "resumed", "run_id": run_id, "ts": t}
                for cid, rid in self.resumed_cards
            ]
            try:
                rank.append_rows(self.data_dir, rows)
            except OSError as exc:
                return f"the resume of the open choice was not recorded: {exc}"
            self.resumed_cards = []
        line = json.dumps({"run": run_id, "step": step.id}) + "\n"
        try:
            self.state.parent.mkdir(parents=True, exist_ok=True)
            with self.state.open("a", encoding="utf-8") as fh:
                fh.write(line)
                fh.flush()
                os.fsync(fh.fileno())
        except OSError as exc:
            return f"the start mark was not written: {exc}"
        return None

    def finish(self, step: Any, outcome: StepOutcome) -> StepOutcome:
        outs = self.spec.outputs.get(step.id)
        if not outs or outcome.status != "succeeded":
            return outcome
        got = collect(outs, outcome.stdout, fence=step.type == "task")
        if isinstance(got, str):
            from dataclasses import replace

            return replace(outcome, status="failed", error=f"slot outputs: {got}")
        self.slots[step.id] = got
        return outcome

    # -- attempts ------------------------------------------------------------

    def below(self, sid: str) -> list[str]:
        """Every step that depends on ``sid``, in id order."""
        if self.plan is None:
            return []
        return sorted(s.id for s in self.plan.steps if sid in _ancestors(self.plan, s.id))

    def choose(self, step: Any, results: list[Any]) -> Any:
        """Rank a step's attempts, record the choice, and give the leader's
        result; a failed result when no attempt survived."""
        from opendaisugi import rank
        from opendaisugi.executor import ExecutorResult

        outs = self.spec.outputs.get(step.id)
        attempts = []
        for k, res in enumerate(results, 1):
            tests = [
                {
                    "name": "the call answered",
                    "required": True,
                    "result": "pass" if res.rc == 0 else "fail",
                }
            ]
            if outs:
                ok = res.rc == 0 and not isinstance(collect(outs, res.stdout, fence=True), str)
                tests.append(
                    {
                        "name": "the slot outputs read",
                        "required": True,
                        "result": "pass" if ok else "fail",
                    }
                )
            digest = hashlib.sha256(res.stdout.encode("utf-8", "surrogatepass")).hexdigest()
            attempts.append(
                {
                    "id": f"{step.id}#{k}",
                    "content_hash": "sha256:" + digest,
                    "author": res.model or "",
                    "tests": tests,
                    "where": {"kind": "output", "text": res.stdout},
                }
            )
        doc = {
            "ranking_id": self.ranking_id(step.id),
            "task": step.prompt,
            "project": self.project,
            "attempts": attempts,
            "comparisons": [],
        }
        r = rank.parse(doc)
        warnings: list[str] = []
        owner: list[tuple[str, str]] = []
        if self.data_dir is not None:
            owner = rank.owner_answers(
                self.data_dir, r.ranking_id, {a.id: a for a in r.attempts}, warnings
            )
        res = rank.fit(r, owner=owner, warnings=warnings)
        if res["status"] == "none_survived":
            why = "; ".join(f"{e['id']}: {', '.join(e['reasons'])}" for e in res["eliminated"])
            self.choices[step.id] = {
                "choice_id": None,
                "chosen": None,
                "status": res["status"],
                "recorded": False,
                "ranking": res,
            }
            first = results[0]
            return ExecutorResult(
                rc=1,
                stdout=f"attempts: none survived: {why}",
                duration_ms=first.duration_ms,
                timed_out=False,
                model=first.model,
            )
        cid = None
        recorded = False
        if self.data_dir is not None:
            t = rank.now()
            rows = rank.sweep(self.data_dir, t)
            if len(res["order"]) > 1:
                row = rank.opened_row(
                    r,
                    res,
                    now=t,
                    run={"run_id": self.run_id, "step": step.id, "downstream": self.below(step.id)},
                )
                cid = row["choice_id"]
                if all(c.id != cid for c in rank.read_cards(self.data_dir)):
                    rows.append(row)
                    recorded = True
            try:
                rank.append_rows(self.data_dir, rows)
            except OSError as exc:
                # No card, no review: the run stops here.
                first = results[0]
                return ExecutorResult(
                    rc=1,
                    stdout=f"attempts: the card was not written: {exc}",
                    duration_ms=first.duration_ms,
                    timed_out=False,
                    model=first.model,
                )
        self.choices[step.id] = {
            "choice_id": cid,
            "chosen": res["leader"],
            "status": res["status"],
            "recorded": recorded,
            "ranking": res,
        }
        return results[int(res["leader"].rsplit("#", 1)[1]) - 1]

    def ranking_id(self, sid: str) -> str:
        return f"weave:{self.digest.split(':')[-1][:16]}:{sid}"

    def _open_card_of(self, step: Any) -> None:
        """For a step with attempts that resume skips: the choice an earlier
        run of the plan left open, so the steps below it still ask."""
        if step.id not in self.spec.attempts or self.data_dir is None:
            return
        from opendaisugi import rank

        rid = self.ranking_id(step.id)
        t = rank.now()
        for card in reversed(rank.read_cards(self.data_dir)):
            if card.opened["ranking_id"] != rid:
                continue
            if (
                card.close is None
                and card.answer is None
                and rank.decay(card, self.data_dir, t) is None
            ):
                self.choices[step.id] = {
                    "choice_id": card.id,
                    "chosen": card.opened["chosen"],
                    "status": card.opened.get("status"),
                    "recorded": False,
                    "ranking": None,
                }
                self.resumed_cards.append((card.id, rid))
            return

    def open_choice_above(self, step: Any) -> str | None:
        """The first step (by id) this step depends on whose choice is open."""
        if self.plan is None:
            return None
        for anc in sorted(_ancestors(self.plan, step.id)):
            c = self.choices.get(anc)
            if c and c["choice_id"] and not c.get("answered"):
                return anc
        return None


def step_tier(step: Any, cwd: str) -> str:
    """``undoable`` for a step weave can take back or that changes nothing
    (a read, a get, a task, a write under the working directory), else
    ``permanent``."""
    if step.type in ("file_read", "network", "task"):
        return "undoable"
    if step.type == "file_write":
        root = os.path.abspath(cwd)
        path = os.path.join(root, step.path) if not os.path.isabs(step.path) else step.path
        p = os.path.normpath(path)
        if p == root or p.startswith(root.rstrip("/") + "/"):
            return "undoable"
    return "permanent"


class AttemptsExecutor:
    """The task executor. A step with attempts asks the model N times, each
    prompt naming its attempt, and the hook ranks the answers."""

    def __init__(self, make: Any, hook: WeaveHook) -> None:
        self.make = make
        self.hook = hook

    def run(self, step: Any, *, timeout_s: int, max_output_bytes: int) -> Any:
        n = self.hook.spec.attempts.get(step.id)
        if not n:
            return self.make(None).run(step, timeout_s=timeout_s, max_output_bytes=max_output_bytes)
        results = [
            self.make((k, n)).run(step, timeout_s=timeout_s, max_output_bytes=max_output_bytes)
            for k in range(1, n + 1)
        ]
        return self.hook.choose(step, results)


class ChoiceAsk:
    """The approval strategy in front of the default one: a step that cannot
    be undone, below a step whose choice is still open, goes to the
    operator's terminal ask, whatever ``--yes`` or the allowlist say. With
    no terminal it is denied."""

    def __init__(self, inner: Any, hook: WeaveHook, cwd: str) -> None:
        self.inner = inner
        self.hook = hook
        self.cwd = cwd

    def decide(self, step: Any, envelope: Any) -> Any:
        import sys

        from opendaisugi.approval import ApprovalDecision, TtyPromptStrategy

        src = self.hook.open_choice_above(step)
        if src is None or step_tier(step, self.cwd) == "undoable":
            return self.inner.decide(step, envelope)
        c = self.hook.choices[src]
        cid = c["choice_id"]
        if not (sys.stdin.isatty() and sys.stdout.isatty()):
            return ApprovalDecision(
                approved=False,
                approved_by="denied",
                reason=f"step {step.id} cannot be undone and the choice {cid} on step {src} "
                "is open; answer it in a terminal, or review it with daisugi rank queue "
                "and run again",
            )
        print(f"Step {src} kept {c['chosen']} of its attempts (card {cid}).")
        d = TtyPromptStrategy().decide(step, envelope)
        if d.approved and self.hook.data_dir is not None:
            from opendaisugi import rank

            rank.append_rows(
                self.hook.data_dir,
                [
                    {
                        "choice_id": cid,
                        "ranking_id": self.hook.ranking_id(src),
                        "event": "confirmed",
                        "how": "permanent_ask",
                        "ts": rank.now(),
                    }
                ],
            )
            c["answered"] = True
        return d


# ---------------------------------------------------------------------------
# The task step's prompt and model
# ---------------------------------------------------------------------------


def task_prompt(
    step: Any, outputs: dict[str, str] | None, attempt: tuple[int, int] | None = None
) -> str:
    """orchestrator._task_step_prompt, and for a step with outputs, the
    JSON object the runner reads its slots from; for one of a step's
    attempts, which attempt it is."""
    from opendaisugi.orchestrator import _task_step_prompt

    if not outputs:
        text = _task_step_prompt(step)
    else:
        keys = ", ".join(f"{k} ({t})" for k, t in outputs.items())
        text = (
            f"{step.prompt}\n\nAnswer with one JSON object and nothing else. "
            f"Its keys and their types: {keys}. A path is absolute."
        )
    if attempt is not None:
        text += f"\n\nThis is attempt {attempt[0]} of {attempt[1]}."
    return text


def route_task(prompt: str) -> str:
    """The model `daisugi route` gives a task with no pathway store."""
    from opendaisugi.routing import RouteAdvisor

    return RouteAdvisor(pathway_store=None, threshold=0.0).advise(prompt).model


__all__ = [
    "AttemptsExecutor",
    "ChoiceAsk",
    "INPUT_FIELDS",
    "MODEL_KINDS",
    "OUTPUT_KINDS",
    "SLOT_TYPES",
    "Prior",
    "SlotSpec",
    "WeaveError",
    "WeaveHook",
    "collect",
    "plan_hash",
    "read_prior",
    "read_slots",
    "step_tier",
    "route_task",
    "state_path",
    "task_prompt",
]
