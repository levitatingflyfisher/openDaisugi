"""data_dir coercion — Daisugi accepts str or os.PathLike (v0.1.3 polish)."""

from __future__ import annotations

from pathlib import Path

from opendaisugi import Daisugi


def test_data_dir_accepts_str(tmp_path):
    d = Daisugi(data_dir=str(tmp_path / "dd"))
    assert d.data_dir == Path(tmp_path / "dd")


def test_data_dir_accepts_path(tmp_path):
    d = Daisugi(data_dir=tmp_path / "dd")
    assert d.data_dir == Path(tmp_path / "dd")


def test_data_dir_none_defaults_to_default_data_dir():
    # None resolves to the DEFAULT_DATA_DIR module attribute (redirected away
    # from the real ~/.opendaisugi by the autouse isolation fixture).
    import opendaisugi

    d = Daisugi(data_dir=None)
    assert d.data_dir == opendaisugi.DEFAULT_DATA_DIR


def test_data_dir_pathlike_object(tmp_path):
    class PL:
        def __init__(self, p):
            self._p = str(p)

        def __fspath__(self):
            return self._p

    d = Daisugi(data_dir=PL(tmp_path / "dd"))
    assert d.data_dir == Path(tmp_path / "dd")
