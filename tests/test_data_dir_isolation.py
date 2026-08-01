"""No test may read or write the real ``~/.opendaisugi`` data directory.

A bare ``Daisugi()`` (no ``data_dir=``) historically pointed at
``Path.home() / ".opendaisugi"`` and would create ``envelope_cache.db`` and,
if it logged, real journal traces there — polluting the user's actual data
directory (this is how 400+ ``task: t`` smoke-test traces ended up in it). An
autouse fixture must redirect the default away from the real home for every
test; this proves it.
"""

from pathlib import Path

import opendaisugi
from opendaisugi import Daisugi


def test_default_data_dir_is_isolated_from_real_home():
    real = Path.home() / ".opendaisugi"
    dai = Daisugi()
    assert dai.data_dir != real, (
        f"bare Daisugi() points at the REAL data dir {real} — a test would "
        "pollute it. The autouse isolation fixture is not redirecting the default."
    )
    # And it must sit under the per-test tmp default the fixture installed.
    assert dai.data_dir == opendaisugi.DEFAULT_DATA_DIR
