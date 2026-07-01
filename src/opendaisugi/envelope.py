"""Envelope generation: system prompt, async generate_envelope(), calibration.

The ENVELOPE_SYSTEM_PROMPT is the most load-bearing artifact in v0.0.1 —
Hypothesis #1 ("can an LLM generate useful envelopes?") rests on it. It is
versioned with the package; changes are SemVer minor bumps.

Prompt versioning: bump ``ENVELOPE_PROMPT_VERSION`` below whenever the prompt
body materially changes. The envelope cache (v0.1.2+) uses this string as the
eviction marker — any entry generated under an older version is discarded on
cache init.
"""

from __future__ import annotations

import json
import logging
from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from fnmatch import fnmatch
from typing import TYPE_CHECKING, Any, Literal

if TYPE_CHECKING:
    from opendaisugi.journal import Journal  # noqa: F401
    from opendaisugi.pathway_store import PathwayStore  # noqa: F401
    from opendaisugi.refinement import RefinementRecord  # noqa: F401
    from opendaisugi.tier1 import Tier1Provider  # noqa: F401

from opendaisugi import llm as _llm
from opendaisugi.envelope_cache import EnvelopeCache, make_cache_key
from opendaisugi.exceptions import (
    EnvelopeGenerationError,
    LowStakesNotConfigured,
    ModelLadderExhausted,
    StakesInheritanceWarning,
    TaskTooLongError,
)
from opendaisugi.inheritance import EnvelopeInheritanceError, verify_inheritance
from opendaisugi.models import Envelope
from opendaisugi.pathway_store import DEFAULT_PATHWAY_THRESHOLD
from opendaisugi.thinking import ThinkingBudget, thinking_kwargs
from opendaisugi.z3_checks import check_envelope_self_consistency

_log = logging.getLogger("opendaisugi.envelope")

ENVELOPE_PROMPT_VERSION = "2026-04-18"  # bump: predicate-algebra DSL exposed in prompt

# Pydantic-generated schema, embedded verbatim so the model sees the exact
# structure it must produce. Built at import time — cheap.
_ENVELOPE_SCHEMA = json.dumps(Envelope.model_json_schema(), indent=2)


_PREDICATE_HINT = ""  # populated just below, after build_envelope_prompt_hint is defined


def build_envelope_prompt_hint() -> str:
    """Prompt snippet describing the predicate-algebra DSL for Invariant.expr.

    Fixed, closed vocabulary. Unknown operators fail at parse time rather
    than silently passing verification.
    """
    return """
Invariants and postconditions may include an `expr` field using the predicate algebra.
Primitives (the complete vocabulary — do not invent others):
    equals(path, value), not_equals(path, value),
    in_set(path, values), not_in_set(path, values),
    matches(path, regex), not_matches(path, regex),
    numeric_range(path, min, max),
    exists(path), is_empty(path),
    and(children), or(children), not(child), implies(a, b),
    forall_steps(pred), exists_step(pred), forall_outputs(pred),
    depends_on(step_id_a, step_id_b), before(step_id_a, step_id_b),
    alias(name, args),  # reference a named alias
    llm_check(rule),    # probabilistic; not allowed when stakes='physical'
Example:
    {"op": "forall_steps",
     "pred": {"op": "implies",
              "a": {"op": "equals", "path": "type", "value": "email_send"},
              "b": {"op": "not_equals", "path": "metadata.signature", "value": "Ada"}}}
"""


_PREDICATE_HINT = build_envelope_prompt_hint()


ENVELOPE_SYSTEM_PROMPT = f"""\
# Role

You generate safety envelopes (checkable specifications) for proposed tasks.
You do NOT execute anything. Your output constrains what an action-proposing
system is allowed to do when carrying out the task.

# Schema

Produce a single JSON object matching this schema exactly:

{_ENVELOPE_SCHEMA}

# Predicate Algebra (Invariant.expr / Postcondition.expr)
{_PREDICATE_HINT}

# Calibration Guidance

- Minimize permissions to what the task genuinely requires.
- Prefer specific paths over globs; prefer allowlists over denylists.
- Default to network=False and shell=False unless the task clearly needs them.
- Reference only paths the task actually mentions — never invent directories.
- Shell allowlists should contain the smallest set of commands that suffice.

# Few-Shot Examples

Example 1: filesystem read

Task: Read /data/sales.csv and print the row count.

Envelope:
{{
  "generated_by": "anthropic/claude-sonnet-4-20250514",
  "task": "Read /data/sales.csv and print the row count.",
  "permissions": {{
    "file_read": ["/data/sales.csv"],
    "file_write": [],
    "network": false,
    "shell": false,
    "shell_allowlist": [],
    "max_execution_time_s": 30,
    "max_output_size_mb": 10
  }},
  "invariants": [
    {{"type": "file_unchanged", "target": "/data/sales.csv", "description": "Input file must not be modified."}}
  ],
  "postconditions": [
    {{"type": "exit_code", "expected": 0}}
  ]
}}

Example 2: filesystem write with allowlist

Task: Generate a PNG chart of /data/sales.csv into out/sales.png.

Envelope:
{{
  "generated_by": "anthropic/claude-sonnet-4-20250514",
  "task": "Generate a PNG chart of /data/sales.csv into out/sales.png.",
  "permissions": {{
    "file_read": ["/data/sales.csv"],
    "file_write": ["out/*.png"],
    "network": false,
    "shell": false,
    "shell_allowlist": [],
    "max_execution_time_s": 30,
    "max_output_size_mb": 10
  }},
  "invariants": [
    {{"type": "file_unchanged", "target": "/data/sales.csv", "description": "Input must not be modified."}}
  ],
  "postconditions": [
    {{"type": "file_exists", "path": "out/sales.png"}},
    {{"type": "file_size_range", "path": "out/sales.png", "min": 100, "max": 10485760}}
  ]
}}

Example 3: shell with tight allowlist

Task: Delete .tmp files older than 7 days under /var/log.

Envelope:
{{
  "generated_by": "anthropic/claude-sonnet-4-20250514",
  "task": "Delete .tmp files older than 7 days under /var/log.",
  "permissions": {{
    "file_read": ["/var/log/**"],
    "file_write": ["/var/log/**"],
    "network": false,
    "shell": true,
    "shell_allowlist": ["find"],
    "max_execution_time_s": 60,
    "max_output_size_mb": 10
  }},
  "invariants": [],
  "postconditions": [
    {{"type": "exit_code", "expected": 0}}
  ]
}}

Example 4: composed task (network + file write)

Task: Download https://example.com/report.json and save it to /tmp/report.json.

Envelope:
{{
  "generated_by": "anthropic/claude-sonnet-4-20250514",
  "task": "Download https://example.com/report.json and save it to /tmp/report.json.",
  "permissions": {{
    "file_read": [],
    "file_write": ["/tmp/report.json"],
    "network": true,
    "shell": false,
    "shell_allowlist": [],
    "max_execution_time_s": 30,
    "max_output_size_mb": 10
  }},
  "invariants": [],
  "postconditions": [
    {{"type": "file_exists", "path": "/tmp/report.json"}}
  ]
}}

# Failure Modes to Avoid

Do NOT produce envelopes that:
(a) allow all permissions (e.g. network=True, shell=True, file_write=["/**"]);
(b) deny all permissions when the task clearly needs some (the result must be
    checkable AND useful — empty permissions for a task that needs I/O is a bug);
(c) reference paths or commands the task did not mention;
(d) include aspirational invariants without enforcement hooks (e.g. "no bugs",
    "code is correct") — invariants must be checkable at runtime.
"""


def _validate_task_length(
    *,
    task: str,
    context: str | None,
    max_task_chars: int,
) -> None:
    """Validate task shape before spending an LLM round-trip.

    Rejects empty tasks (whitespace-only counts as empty) and tasks whose
    combined task+context length exceeds ``max_task_chars``. v0.0.1 does
    not auto-summarize — the caller is responsible for keeping the combined
    payload within the budget. Summarization is a v0.1 feature per spec
    §"Input Constraints".
    """
    if not task or not task.strip():
        raise ValueError("Task must be a non-empty string.")
    combined = len(task) + len(context or "")
    if combined > max_task_chars:
        raise TaskTooLongError(
            f"Task + context is {combined} chars (limit: {max_task_chars}). "
            "Summarize before passing, or increase max_task_chars."
        )


_SUMMARIZE_INSTRUCTION = (
    "\n\nAlso produce a one-line `summary` field (<=80 chars) describing the task "
    "in plain English."
)


def _refinement_hints_block(records: "list[RefinementRecord]") -> str:
    """Build the 'Prior Rejections' section for a refinement-aware prompt.

    Collects unique violation messages across all records. When the same
    message appears in multiple records, keeps the most recent record's
    timestamp for ranking. Returns the top 10 unique messages (by recency)
    formatted for injection into the user message. Empty string when no
    records — caller appends unconditionally.
    """
    if not records:
        return ""

    # message -> newest record timestamp seen for that message
    newest_ts: dict[str, float] = {}
    # message -> stage (preserved from whichever record had it)
    stage_for: dict[str, str] = {}
    for rec in records:
        for v in rec.violations:
            key = v.message
            if key not in newest_ts or rec.timestamp > newest_ts[key]:
                newest_ts[key] = rec.timestamp
            stage_for.setdefault(key, v.stage)

    # Sort messages by newest timestamp descending; take top 10.
    top = sorted(newest_ts.items(), key=lambda kv: -kv[1])[:10]

    lines = [
        "",
        "## Prior Rejections",
        "",
        "Previous plans verified against envelopes for this task were rejected.",
        "Generate an envelope that prevents these violations:",
        "",
    ]
    for msg, _ts in top:
        lines.append(f"- [{stage_for[msg]}] {msg}")
    return "\n".join(lines)


def _lookup_refinements(
    journal: "Journal | None",
    cache_key: str,
) -> "list[RefinementRecord]":
    """Safe wrapper around journal.get_refinements_by_key.

    Returns [] on any failure — envelope generation must not break because
    refinement lookup is broken.
    """
    if journal is None:
        return []
    try:
        return journal.get_refinements_by_key(cache_key)
    except Exception as exc:  # journal failure is non-fatal
        import logging
        logging.getLogger("opendaisugi.envelope").warning(
            "refinement lookup failed: %s", exc,
        )
        return []


async def generate_envelope(
    task: str,
    *,
    context: str | None = None,
    parent: Envelope | None = None,
    summarize: bool = False,
    cache: EnvelopeCache | None = None,
    pathway_store: "PathwayStore | None" = None,
    pathway_threshold: float = DEFAULT_PATHWAY_THRESHOLD,
    journal: "Journal | None" = None,
    stakes: Literal["low", "medium", "high"] = "medium",
    low_stakes_envelope: Envelope | None = None,
    model: str | list[str] = "anthropic/claude-sonnet-4-20250514",
    thinking_budget: ThinkingBudget = "standard",
    tier1: "Tier1Provider | None" = None,
    max_retries: int = 3,
    max_task_chars: int = 4000,
) -> Envelope:
    """Generate a safety envelope for a task via LLM + instructor.

    ``model`` may be a single model string or a list of models forming an
    escalation ladder. When a list is provided, each rung is tried in order;
    escalation occurs on instructor parse exhaustion or Z3 self-consistency
    violation. ``TaskTooLongError`` always short-circuits the whole ladder.

    When ``summarize=True``, the user message includes a trailing instruction
    asking the LLM to populate the optional ``summary`` field. The system
    prompt is never mutated so downstream caches key stably.

    When ``cache`` is provided, the cache is consulted first (all rungs are
    checked up-front; the first hit wins and short-circuits the LLM). On a
    miss, the LLM result is stored under the key for the rung that actually
    succeeded.

    When ``parent`` is provided, the returned envelope's ``parent_envelope``
    is stamped with ``parent.id`` and ``verify_inheritance`` is run against
    ``parent``; any violations raise ``EnvelopeInheritanceError`` and the
    result is NOT cached.

    Raises:
        TaskTooLongError: if ``len(task) + len(context or "")`` exceeds
            ``max_task_chars``.
        EnvelopeGenerationError: on any upstream LLM failure for a bare-string
            ``model`` (preserves v0.1.2 backward-compat exception type).
        ModelLadderExhausted: when a list ``model`` is provided and every rung
            fails.
        EnvelopeInheritanceError: if ``parent`` is provided and the generated
            child envelope relaxes any parent constraint.
    """
    _validate_task_length(task=task, context=context, max_task_chars=max_task_chars)

    if stakes == "low":
        if low_stakes_envelope is None:
            raise LowStakesNotConfigured(
                "stakes='low' requires a configured envelope. Pass "
                "low_stakes_envelope=... or construct the facade via "
                "Daisugi.with_default_low_stakes()."
            )
        if parent is not None:
            import warnings
            warnings.warn(
                "stakes='low' ignores parent= (no fresh envelope is generated).",
                StakesInheritanceWarning,
                stacklevel=2,
            )
        return low_stakes_envelope.model_copy()

    # Normalise model into a ladder; remember whether caller passed bare str.
    _single_model = isinstance(model, str)
    ladder: list[str] = [model] if _single_model else list(model)
    if not ladder:
        raise ValueError("model must be a non-empty string or non-empty list of strings.")

    parent_id = parent.id if parent is not None else None

    # Build user message once — shared across all rungs (hints appended per-rung).
    user_content = f"Task: {task}"
    if context:
        user_content += f"\n\nContext:\n{context}"
    if summarize:
        user_content += _SUMMARIZE_INSTRUCTION

    # Per-rung key helper — used for both cache lookup and hint lookup.
    def _key_for(rung: str) -> str:
        return make_cache_key(
            task=task, context=context, model=rung,
            parent_envelope_id=parent_id, summarize=summarize,
            thinking_budget=thinking_budget,
        )

    # v0.3.0: check compiled-pathway store first.
    if pathway_store is not None and stakes != "high":
        try:
            match = pathway_store.find(task, threshold=pathway_threshold)
        except Exception as exc:
            _log.warning("pathway_store.find failed: %s", exc)
            match = None
        if match is not None:
            pathway_store.increment_hit(match.pathway.id)
            return match.pathway.envelope.model_copy(update={
                "generated_by": f"compiled-pathway:{match.pathway.id}",
            })

    # v0.4.0: Tier-1 local-model slot. Runs between the pathway check and the
    # Tier-2 ladder. Returning None or raising means "decline, fall through."
    # The returned envelope goes through the same Z3 self-consistency gate as
    # Tier-2 output; inconsistent envelopes are treated as a decline.
    if tier1 is not None and stakes != "high":
        first_rung = model if isinstance(model, str) else (model[0] if model else "")
        tier1_key = make_cache_key(
            task=task, context=context, model=first_rung,
            parent_envelope_id=parent_id, summarize=summarize,
            thinking_budget=thinking_budget,
            tier1_provider_name=tier1.name,
        )
        # Probe the cache under the tier1-aware key first — if a prior Tier-1
        # call already answered for this provider+task, reuse that envelope
        # instead of asking the adapter again.
        if cache is not None:
            cached_t1 = cache.get(
                task=task, context=context, model=first_rung,
                parent_envelope_id=parent_id, summarize=summarize,
                thinking_budget=thinking_budget,
                tier1_provider_name=tier1.name,
            )
            if cached_t1 is not None:
                if cached_t1.cache_key is None:
                    cached_t1.cache_key = tier1_key
                return cached_t1
        tier1_env: Envelope | None
        try:
            tier1_env = await tier1.generate_envelope(task, context=context)
        except Exception as exc:
            _log.warning("tier1 provider %r failed: %s — falling through", tier1.name, exc)
            tier1_env = None
        if tier1_env is not None:
            try:
                t1_violations = check_envelope_self_consistency(tier1_env)
            except Exception:
                t1_violations = []
            if t1_violations:
                _log.info(
                    "tier1 provider %r returned self-inconsistent envelope (%s) — falling through",
                    tier1.name, t1_violations[0].message,
                )
            else:
                tier1_env.generated_by = f"tier1:{tier1.name}"
                if parent is not None:
                    tier1_env.parent_envelope = parent.id
                    inh_violations = verify_inheritance(tier1_env, parent)
                    if inh_violations:
                        raise EnvelopeInheritanceError(inh_violations)
                tier1_env.cache_key = tier1_key
                if cache is not None:
                    cache.put(
                        tier1_env,
                        task=task, context=context, model=first_rung,
                        parent_envelope_id=parent_id, summarize=summarize,
                        thinking_budget=thinking_budget,
                        tier1_provider_name=tier1.name,
                    )
                return tier1_env

    # Cache scan: check every rung up-front; first hit short-circuits the LLM.
    # Cache-bust: if the first rung with a cache hit has refinements newer
    # than the cached entry, invalidate that entry and fall through to
    # generation. Subsequent rungs are unaffected (consistent: one stale
    # entry doesn't invalidate the whole ladder).
    if cache is not None and stakes != "high":
        for rung in ladder:
            cached = cache.get(
                task=task, context=context, model=rung,
                parent_envelope_id=parent_id, summarize=summarize,
                thinking_budget=thinking_budget,
            )
            if cached is None:
                continue
            rung_key = _key_for(rung)
            cached_at = cache.get_inserted_at(rung_key)
            refinements = _lookup_refinements(journal, rung_key)
            if (
                cached_at is not None
                and refinements
                and max(r.timestamp for r in refinements) > cached_at
            ):
                # Stale — bust this rung's entry and fall through.
                cache.invalidate(rung_key)
                break
            if cached.cache_key is None:
                cached.cache_key = rung_key
            return cached

    # Escalation loop.
    last_error: Exception | None = None
    last_cause: BaseException | None = None  # original exc for __cause__ chaining
    for rung in ladder:
        rung_key = _key_for(rung)
        refinements = _lookup_refinements(journal, rung_key)
        hints_block = _refinement_hints_block(refinements)
        rung_user_content = user_content + hints_block

        client = _llm.get_instructor_client(model=rung)
        extra = thinking_kwargs(rung, thinking_budget)
        try:
            env = await client.chat.completions.create(
                model=rung,
                max_retries=max_retries,
                response_model=Envelope,
                messages=[
                    {"role": "system", "content": ENVELOPE_SYSTEM_PROMPT},
                    {"role": "user", "content": rung_user_content},
                ],
                **extra,
            )
        except Exception as e:
            translated = _llm.translate_llm_error(e)
            if isinstance(translated, TaskTooLongError):
                raise translated from e
            last_error = translated
            last_cause = e
            continue

        # Z3 self-consistency check — treat any z3 exception (e.g. timeout) as
        # "no violations" so a z3 outage doesn't block all envelope generation.
        try:
            violations = check_envelope_self_consistency(env)
        except Exception:
            violations = []

        if violations:
            last_error = EnvelopeGenerationError(
                f"Model {rung!r} produced self-inconsistent envelope: "
                f"{violations[0].message}"
            )
            continue

        # Rung succeeded — run inheritance check (not rung-specific; always).
        if parent is not None:
            env.parent_envelope = parent.id
            inh_violations = verify_inheritance(env, parent)
            if inh_violations:
                raise EnvelopeInheritanceError(inh_violations)

        # v0.2.1: stamp the cache key used for this generation onto the envelope
        # itself so downstream (Supervisor, journal) can tag refinement records
        # without reconstructing the args.
        env.cache_key = rung_key

        if cache is not None:
            cache.put(
                env,
                task=task,
                context=context,
                model=rung,
                parent_envelope_id=parent_id,
                summarize=summarize,
                thinking_budget=thinking_budget,
            )

        return env

    # All rungs exhausted.
    if _single_model:
        # Backward-compat: bare-string callers get the raw EnvelopeGenerationError,
        # with __cause__ chained exactly as the original single-model path did.
        raise last_error from last_cause  # type: ignore[misc]
    raise ModelLadderExhausted(attempted=ladder, last_error=last_error)  # type: ignore[arg-type]


def _resolve_path(envelope: Envelope, dotted: str) -> Any:
    """Walk dotted attribute path (e.g. ``permissions.file_read``).

    Raises ``AttributeError`` with the full dotted path in the message so
    corpus typos surface quickly instead of producing a bare ``getattr``
    error against an internal Pydantic model.
    """
    cur: Any = envelope
    for part in dotted.split("."):
        try:
            cur = getattr(cur, part)
        except AttributeError as e:
            raise AttributeError(
                f"Cannot resolve dotted path {dotted!r}: "
                f"{type(cur).__name__} has no attribute {part!r}"
            ) from e
    return cur


def _check_assert(envelope: Envelope, assertion: dict[str, Any]) -> bool:
    """Evaluate a single shape assertion against an envelope.

    Supported assertion keys (one per assertion dict):
        equals: value        — exact equality
        contains_glob: glob  — value is a list; at least one element matches glob
        not_glob: glob       — value is a list; NO element matches glob (reject degenerate)
        not_empty: true      — value is a list with at least one element
        empty: true          — value is a list with zero elements

    Raises TypeError when a list-valued assertion is run against a scalar
    path — silent False would hide corpus authoring mistakes.
    """
    path = assertion["path"]
    value = _resolve_path(envelope, path)

    if "equals" in assertion:
        return value == assertion["equals"]

    # All remaining assertion types expect a list.
    for list_key in ("contains_glob", "not_glob", "not_empty", "empty"):
        if list_key in assertion:
            if not isinstance(value, list):
                raise TypeError(
                    f"Assertion {list_key!r} at path {path!r} expects a list, "
                    f"got {type(value).__name__}"
                )
            break

    if "contains_glob" in assertion:
        glob = assertion["contains_glob"]
        return any(fnmatch(v, glob) for v in value)
    if "not_glob" in assertion:
        glob = assertion["not_glob"]
        return not any(fnmatch(v, glob) for v in value)
    if "not_empty" in assertion:
        return len(value) > 0
    if "empty" in assertion:
        return len(value) == 0
    raise ValueError(f"Unknown assertion shape: {assertion!r}")


@dataclass
class CalibrationReport:
    total: int
    passed: int
    failures: list[str]            # corpus entry ids that failed at least one assert
    errors: dict[str, str]         # corpus entry id -> exception string (producer failures)

    @property
    def pass_rate(self) -> float:
        return self.passed / self.total if self.total > 0 else 0.0


async def run_calibration(
    entries: list[dict[str, Any]],
    *,
    produce: Callable[..., Awaitable[Envelope]],
) -> CalibrationReport:
    """Score each corpus entry against ``produce(task=...)``.

    ``produce`` is any async callable that takes a task string (and optional
    kwargs) and returns an Envelope. In tests it is a mock; in the real
    calibration run it is ``generate_envelope``.
    """
    total = len(entries)
    passed = 0
    failures: list[str] = []
    errors: dict[str, str] = {}

    for entry in entries:
        entry_id = entry["id"]
        try:
            envelope = await produce(task=entry["task"])
        except Exception as e:  # producer blew up — count as failure
            failures.append(entry_id)
            errors[entry_id] = str(e)
            continue

        if all(_check_assert(envelope, a) for a in entry.get("asserts", [])):
            passed += 1
        else:
            failures.append(entry_id)

    return CalibrationReport(total=total, passed=passed, failures=failures, errors=errors)
