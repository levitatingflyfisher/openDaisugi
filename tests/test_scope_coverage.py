"""A tree is in scope only under a covering /** glob (SG-2)."""

import pytest

from opendaisugi.verify import covers_everything, covers_tree


@pytest.mark.parametrize(
    ("d", "globs", "ok"),
    [
        ("/work/out", ["/work/**"], True),
        ("/work", ["/work/**"], True),
        ("/work/a/out", ["/work/*/out/**"], True),
        ("/work/out", ["/work/*.py"], False),
        ("/work/out", ["/work/out"], False),
        ("/work/out", ["/work/out/*"], False),
        ("/", ["/**"], True),
        ("/", ["**"], True),
        ("/etc", ["/work/**", "/home/**"], False),
        ("out", ["out/**"], True),
    ],
)
def test_covers_tree(d, globs, ok):
    assert covers_tree(d, globs) is ok


def test_covers_everything_is_exact():
    assert covers_everything(["/**"]) and covers_everything(["x", "**"])
    assert not covers_everything(["/**/*"]) and not covers_everything(["/home/**"])
