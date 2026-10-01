"""The gate's rule that no agent changes the delegation tree.

An agent that could register an envelope could give itself any authority:
the gate checks its calls against whatever envelope is registered. An
agent that could start a child through the tree could name a wider
ancestor as the child's parent and get a child wider than itself. So the
verbs that write the tree or register an envelope are the operator's and
the starter's: ``daisugi gate register`` and ``daisugi gate init``,
``daisugi start`` (it registers a starter envelope for a new session and
starts a harness under it), and ``daisugi tree root``, ``spawn``, ``end``
and ``answer``. The gate denies a shell line that runs one of them, before
any envelope check, in every mode, and no operator ask can turn that deny.
``daisugi tree check`` and ``daisugi tree status`` read only and pass.

The line is read as ``owner_rule`` reads it: per simple command, the verbs
as arguments of a ``daisugi`` command. So ``daisugi status && npm start``
is not a hit: ``start`` is an argument of npm.
"""

from __future__ import annotations

from typing import Any

from opendaisugi.owner_rule import TREE_VERBS, runs_verb, shell_hit

REFUSAL = "only the operator or the starter changes the delegation tree. Run it yourself."


def names_tree_write(text: str) -> bool:
    """True when ``text`` runs a verb that registers an envelope or writes
    the tree."""
    return runs_verb(text, TREE_VERBS)


def tree_write_hit(payload: dict[str, Any], record: dict[str, Any] | None) -> bool:
    """True when a shell call runs a verb that writes the tree. Any error
    counts as a hit, so a broken check denies."""
    return shell_hit(record, TREE_VERBS)
