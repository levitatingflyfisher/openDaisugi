"""Loopback ports for the case harnesses.

A port found by binding port 0 and closing the socket at once is free only
for a moment: the next bind to port 0 can get the same number. Two roles in
one case then share a port, one server cannot listen, and the normalizer
maps the number to the wrong name. A PortPool keeps each port it gives out
bound until release(), so the kernel cannot give the number out again, and
the numbers it gives are distinct from each other and from `avoid`.
"""

from __future__ import annotations

import socket
from collections.abc import Iterable


class PortPool:
    def __init__(self, avoid: Iterable[int] = ()):
        self.avoid = {int(p) for p in avoid}
        self.held: list[socket.socket] = []

    def take(self) -> int:
        rejected: list[socket.socket] = []
        try:
            while True:
                s = socket.socket()
                s.bind(("127.0.0.1", 0))
                port = s.getsockname()[1]
                if port in self.avoid:
                    rejected.append(s)
                    continue
                self.held.append(s)
                self.avoid.add(port)
                return port
        finally:
            for s in rejected:
                s.close()

    def release(self) -> None:
        """Close the held sockets: call this just before the processes that
        must bind the ports start."""
        for s in self.held:
            s.close()
        self.held = []

    def __enter__(self) -> PortPool:
        return self

    def __exit__(self, *a: object) -> None:
        self.release()


_CLASH = ("address already in use", "address in use")


def bind_clash(result: object) -> bool:
    """True when a run's record says a server could not bind its port: the
    case then runs once more on new ports."""
    import json

    text = json.dumps(result).lower()
    return any(c in text for c in _CLASH)


def sub_number(text: str, number: str, name: str) -> str:
    """Replace each whole ``number`` in dumped JSON ``text`` with ``name``.

    The number must stand alone. A letter, a digit or ``_`` next to it makes
    it part of a word, such as a hex digest or the ``\\uXXXX`` escape of an
    em dash (``\\u2014``); replacing it there changes a hash or leaves text
    that is not JSON. A decimal point before it, or a decimal point and a
    digit after it, makes it part of a decimal, such as a duration.
    """
    import re

    return re.sub(rf"(?<![0-9A-Za-z_.]){re.escape(number)}(?![0-9A-Za-z_]|\.[0-9])", name, text)
