"""The gate's rule that no agent records a ranking vote.

``daisugi rank record`` writes a vote into a ranking. A vote recorded as
the owner outweighs the model judges, so an agent that could record one
for its own attempt would decide its own ranking. The owner records picks
through the floor, as the operator. So the gate denies every shell line
that runs ``daisugi rank record``, before any envelope check, in every
mode, and no operator ask can turn that deny.

The rule reads words, not a parsed command. It first drops every
backslash and quote, so ``dai\\sugi`` and ``'dai'sugi`` read as
``daisugi``. A word is a run of the ASCII characters letters, digits and
``_./:=+@%,-``; every other character, any non-ASCII one included, ends a
word. The line is a hit when a word whose last path part is ``daisugi``
or ``opendaisugi``, or starts with ``opendaisugi.``, comes before a word
``rank``, which comes before a word ``record``. So a path head, an env or
``uv run`` wrapper, ``python -m opendaisugi`` and ``sh -c`` all hit, and
options between the words do not hide them. A line that names the three
words in that order for another reason, such as an echo, is denied too.

Words the shell builds at run time (a variable, a command substitution,
eval) are not seen. This rule is a second line: an agent's envelope should
not allow the ``daisugi`` shell head at all.
"""

from __future__ import annotations

from typing import Any

REFUSAL = "only the owner records a ranking vote. Record it yourself."

_WORD = frozenset("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_./:=+@%,-")


def _words(text: str) -> list[str]:
    for ch in ("\\", "'", '"'):
        text = text.replace(ch, "")
    out: list[str] = []
    cur: list[str] = []
    for ch in text:
        if ch in _WORD:
            cur.append(ch)
        elif cur:
            out.append("".join(cur))
            cur = []
    if cur:
        out.append("".join(cur))
    return out


def _is_head(word: str) -> bool:
    last = word.rsplit("/", 1)[-1]
    return last in ("daisugi", "opendaisugi") or last.startswith("opendaisugi.")


def names_rank_record(text: str) -> bool:
    """True when ``text`` names ``daisugi``, then ``rank``, then ``record``."""
    state = 0
    for w in _words(text):
        if state == 0 and _is_head(w):
            state = 1
        elif state == 1 and w == "rank":
            state = 2
        elif state == 2 and w == "record":
            return True
    return False


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
