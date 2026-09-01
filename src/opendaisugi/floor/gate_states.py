"""Read the session store's own gate state entries back out for a backend.

gate.py writes one PaneStateEvent per call into the session tree, as a
`state` entry. This module is the reader that makes those entries
reachable: `gate_states_reader` returns a callable a backend can call on
every poll, handing back the newest `state` entry per pane id across
every session file. The reader applies no recency bound of its own, so a
gate fact for a pane stands until a newer one for that same pane id
replaces it. The 2-second window in events.py's merge() is a separate
rule that only holds back an incoming manifest event; it does not age
out a gate event.
"""

from __future__ import annotations

import json
from collections.abc import Callable
from pathlib import Path

from opendaisugi.floor.events import PaneStateEvent
from opendaisugi.session_tree import SessionTree


def gate_states_reader(data_dir: Path) -> Callable[[], dict[str, PaneStateEvent]]:
    """Build a reader over `<data_dir>/sessions`. Never raises, ever.

    A missing sessions directory, a session file that rotates out from
    under the read, or a state entry with no pane or a malformed body are
    all the same fact to a caller: nothing usable there this poll. Only a
    pane id ties an event to a specific pane in the roster, so an entry
    with no pane is skipped rather than guessed at. A session file torn
    by a killed writer raises ValueError or TypeError out of its own
    entries() call, not only OSError: a non-numeric timestamp on one line
    fails a plain float conversion, and a write torn mid multibyte
    character fails UTF-8 decoding entirely. Either one skips the whole
    file for this poll rather than raising past this reader.
    """
    sessions_dir = Path(data_dir) / "sessions"

    def read() -> dict[str, PaneStateEvent]:
        out: dict[str, PaneStateEvent] = {}
        try:
            paths = list(sessions_dir.glob("*.jsonl"))
        except OSError:
            return out
        for path in paths:
            try:
                entries = SessionTree(path).entries()
            except (OSError, ValueError, TypeError):
                continue
            for entry in entries:
                if entry.type != "state":
                    continue
                pane = entry.data.get("pane")
                if not pane or not isinstance(pane, str):
                    continue
                try:
                    row = json.dumps({**entry.data, "ts": entry.ts})
                    ev = PaneStateEvent.from_json(row)
                except (ValueError, TypeError):
                    continue
                prior = out.get(pane)
                if prior is None or ev.ts > prior.ts:
                    out[pane] = ev
        return out

    return read
