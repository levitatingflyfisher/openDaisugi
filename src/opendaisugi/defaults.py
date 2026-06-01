"""Library-shipped default envelopes.

``DEFAULT_LOW_STAKES_ENVELOPE`` is the permissive sandbox-grade envelope the
CLI uses when ``--stakes low`` is passed without ``--low-stakes-envelope``, and
that ``Daisugi.with_default_low_stakes()`` injects into the facade. It is
intentionally NOT the library default for ``Daisugi()`` — callers must opt in.
"""
from __future__ import annotations

from opendaisugi.models import Envelope, FallbackStrategy, Permission

DEFAULT_LOW_STAKES_ENVELOPE: Envelope = Envelope(
    id="env_default_low_stakes",
    generated_by="opendaisugi-library-default",
    task="<default low-stakes envelope>",
    permissions=Permission(
        file_read=["**"],
        file_write=["/tmp/**", "./out/**"],
        network=False,
        network_hosts=[],
        shell=False,
        shell_allowlist=[],
        max_execution_time_s=30,
        max_output_size_mb=10,
    ),
    invariants=[],
    postconditions=[],
    fallback=FallbackStrategy(),
    summary="Default low-stakes envelope (dev/sandbox use)",
)
