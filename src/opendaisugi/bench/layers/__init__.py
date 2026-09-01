"""Importing this package registers every layer bench.

Each module calls `registry.register` at import. Adding a layer means adding a
module and one import line here. Nothing else knows the list. The pairs
compose the layers, so their module is imported here too and registers the
`pairs` index the same way.
"""

from __future__ import annotations

from opendaisugi.bench import pairs  # noqa: F401
from opendaisugi.bench.layers import (  # noqa: F401
    backend,
    gate_path,
    loop,
    matcher,
    router,
    verifier,
)
