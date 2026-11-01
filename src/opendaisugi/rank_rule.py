"""The warden's rule that no agent records a ranking vote.

``daisugi rank record`` writes a vote into a ranking. A vote recorded as
the owner outweighs the model judges, so an agent that could record one
for its own attempt would decide its own ranking. The owner records picks
through the floor, as the operator. So the warden denies every shell line
that runs ``daisugi rank record``, before any envelope check, in every
mode, and no operator ask can turn that deny.

The line is read as ``owner_rule`` reads it: per simple command, the verb
as arguments of a ``daisugi`` command, with the word-order fallbacks that
module names.
"""

from __future__ import annotations

from typing import Any

from opendaisugi.owner_rule import PIVOT_VERBS, RANK_VERBS, runs_verb, words

REFUSAL = "only the owner records a ranking vote. Record it yourself."


def names_rank_record(text: str) -> bool:
    """True when ``text`` runs ``daisugi rank record``, or ``daisugi rank
    pivot`` with ``--apply`` (the pivot that changes work; without the flag
    it only prints a plan)."""
    if runs_verb(text, RANK_VERBS):
        return True
    return runs_verb(text, PIVOT_VERBS) and "--apply" in words(text)


def rank_record_hit(payload: dict[str, Any], record: dict[str, Any] | None) -> bool:
    """True when a shell call runs ``daisugi rank record``. Any error counts
    as a hit, so a broken check denies."""
    try:
        if record is None or record.get("step_type") != "shell":
            return False
        command = record.get("command")
        return isinstance(command, str) and names_rank_record(command)
    except Exception:  # noqa: BLE001 - fail closed
        return True
