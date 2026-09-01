"""`daisugi bench`: one front door for comparing the options within a layer.

Contracts make the layers independent, so the default is a table per layer,
not an n by n matrix. A cross-layer pair earns a row only where a real
coupling exists. Every table prints the command that reproduces it and the
digest of the corpus it read.
"""

from __future__ import annotations
