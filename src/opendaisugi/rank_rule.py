"""The gate's rule that no agent records a ranking vote.

``daisugi rank record`` writes a vote into a ranking. A vote recorded as
the owner outweighs the model judges, so an agent that could record one
for its own attempt would decide its own ranking. The owner records picks
through the floor, as the operator. So the gate denies every shell line
that runs ``daisugi rank record``, before any envelope check, in every
mode, and no operator ask can turn that deny.

The line is read as ``owner_rule`` reads it: per simple command, the verb
as arguments of a ``daisugi`` command, with the word-order fallbacks that
module names.
"""

from __future__ import annotations

from typing import Any

from opendaisugi.owner_rule import RANK_VERBS, runs_verb, shell_hit

REFUSAL = "only the owner records a ranking vote. Record it yourself."


def names_rank_record(text: str) -> bool:
    """True when ``text`` runs ``daisugi rank record``."""
    return runs_verb(text, RANK_VERBS)


def rank_record_hit(payload: dict[str, Any], record: dict[str, Any] | None) -> bool:
    """True when a shell call runs ``daisugi rank record``. Any error counts
    as a hit, so a broken check denies."""
    return shell_hit(record, RANK_VERBS)
