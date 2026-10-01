"""The pane rule refuses an allow or deny sent to another coppice server.
With no XDG_RUNTIME_DIR, coppice's socket sits under the data home, so
OPENDAISUGI_HOME= and XDG_DATA_HOME= reach another server as HOME= does."""

from __future__ import annotations

import pytest

from opendaisugi.pane_rule import shell_denies_from_outside


@pytest.mark.parametrize("var", ["HOME", "OPENDAISUGI_HOME", "XDG_DATA_HOME", "XDG_RUNTIME_DIR"])
def test_an_assignment_that_moves_the_socket_is_refused(var):
    assert shell_denies_from_outside(f"{var}=/tmp/x coppice agent deny 3") is True


def test_a_plain_deny_is_not_refused():
    assert shell_denies_from_outside("coppice agent deny 3") is False
