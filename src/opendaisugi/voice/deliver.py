"""Deliver transcribed text to a pane: preview by default, sent only when armed.

Preview never touches a pane. It reports the mode and leaves the actual
display to the caller, such as a client screen. Direct send is refused
unless the pane holds a current arm grant, and a refused send never calls
the backend factory or the injected send callable either.

deliver() takes an opaque pane and a zero-argument backend factory in place
of a built backend. It makes the one arm decision itself and calls the
factory only right after that decision says send, never before and never a
second time. Preview and a refused send never call the factory at all. This
keeps the decision to one place: a caller that checked armed status itself
before calling deliver, then handed deliver a backend built ahead of time,
could see the grant expire or a concurrent arm land in the gap between its
own check and deliver's. deliver carries no runtime import of any pane
backend package, so it loads and runs on its own no matter which pane
backend a given host has. The send callable it is given carries the actual
delivery: it takes the backend, the pane, and the text, in that order, and
returns a word describing what happened.

An arm grant is a file. The directory that holds grants is created at 0700,
and each grant's own file at 0600, so a grant readable only by its owner
never becomes readable by anyone else. A missing, corrupt, or expired grant
is always treated as unarmed. Checking never fails open.
"""

from __future__ import annotations

import json
import time
import urllib.parse
from collections.abc import Callable
from dataclasses import dataclass
from pathlib import Path
from typing import TYPE_CHECKING, Literal, Protocol

if TYPE_CHECKING:
    from opendaisugi.floor.backend import PaneBackend, PaneRef


class SendText(Protocol):
    def __call__(self, backend: "PaneBackend", pane: "PaneRef", text: str) -> str:
        """Deliver text to a pane through a backend. Returns a word naming what happened."""
        ...


@dataclass(frozen=True)
class ArmEntry:
    pane_key: str
    armed_at: float
    expires_at: float


def _armed_path(armed_dir: Path, pane_key: str) -> Path:
    """Map pane_key to its grant file, one to one.

    Percent-encoding every character that is not filesystem-safe, including
    / and :, makes the mapping injective: two different keys never land on
    the same file, and a key built to contain a path separator never
    reaches outside armed_dir.
    """
    encoded = urllib.parse.quote(pane_key, safe="")
    return armed_dir / f"{encoded}.json"


def arm(pane_key: str, *, minutes: float, armed_dir: Path, now: float | None = None) -> ArmEntry:
    """Grant pane_key a direct-send window of the given length in minutes.

    Creates armed_dir at 0700 and the grant file at 0600 if they do not
    already exist. Calling arm again for the same pane replaces its grant.
    """
    now = time.time() if now is None else now
    armed_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
    entry = ArmEntry(pane_key=pane_key, armed_at=now, expires_at=now + minutes * 60.0)
    path = _armed_path(armed_dir, pane_key)
    path.write_text(
        json.dumps(
            {
                "pane_key": entry.pane_key,
                "armed_at": entry.armed_at,
                "expires_at": entry.expires_at,
            }
        )
    )
    path.chmod(0o600)
    return entry


def disarm(pane_key: str, *, armed_dir: Path) -> bool:
    """Revoke pane_key's direct-send grant, if it has one.

    Returns True when a grant existed and was removed, False when the pane
    had no grant. A pane that was never armed is left with no file either way.
    """
    path = _armed_path(armed_dir, pane_key)
    if not path.exists():
        return False
    path.unlink()
    return True


def is_armed(pane_key: str, *, armed_dir: Path, now: float | None = None) -> bool:
    """Report whether pane_key currently holds a direct-send grant.

    A missing, unreadable, or expired grant file reports unarmed. A pane
    key long enough to overflow a file name reports unarmed too, rather
    than raising OSError. Checking never fails open.
    """
    now = time.time() if now is None else now
    path = _armed_path(armed_dir, pane_key)
    try:
        if not path.exists():
            return False
        data = json.loads(path.read_text())
        expires_at = float(data["expires_at"])
    except Exception:
        return False
    return expires_at > now


@dataclass(frozen=True)
class DeliverResult:
    delivered: Literal["preview", "sent", "refused"]
    reason: str | None = None


def deliver(
    pane: "PaneRef",
    text: str,
    backend_factory: Callable[[], "PaneBackend"],
    *,
    mode: Literal["preview", "send"] = "preview",
    armed_dir: Path,
    pane_key: str | None = None,
    now: float | None = None,
    send: SendText,
) -> DeliverResult:
    """Deliver text toward a pane, in preview mode or send mode.

    Checks mode first. Any value other than preview or send is refused, and
    backend_factory is never called.

    Preview always succeeds and never calls backend_factory or send. The
    caller shows the text itself; this only reports the mode.

    Send is refused unless the pane holds a current arm grant, and a
    refused send never calls backend_factory or send either. The refusal
    names the exact command that arms the pane. Once armed, deliver calls
    backend_factory exactly once, then calls send with the backend it
    returned, the pane, and the text, in that order, and reports sent.

    The arm key defaults to the pane's own id. Pass pane_key to use a
    different key.

    now is forwarded to the arming check. Leave it unset to check against
    real time.
    """
    if mode not in ("preview", "send"):
        return DeliverResult(
            delivered="refused",
            reason=f"unknown mode {mode!r}. Use preview or send.",
        )
    key = pane_key if pane_key is not None else pane.id
    if mode == "preview":
        return DeliverResult(delivered="preview")
    if not is_armed(key, armed_dir=armed_dir, now=now):
        return DeliverResult(
            delivered="refused",
            reason=f"pane not armed for direct send. Run: daisugi voice arm {key} --for 30m",
        )
    backend = backend_factory()
    send(backend, pane, text)
    return DeliverResult(delivered="sent")
