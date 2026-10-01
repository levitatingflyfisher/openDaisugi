"""The browser view (`daisugi dashboard --serve`) loads nothing from another host.

textual-serve's own page links a Google Fonts stylesheet. The served page is
ours: it takes the font from textual-serve's local static folder, so a page
view makes no request off this machine.
"""

from __future__ import annotations

import re
from pathlib import Path

import pytest

pytest.importorskip("textual")
pytest.importorskip("textual_serve")

from opendaisugi import tui  # noqa: E402

# A URL the browser would fetch: in href, src, url() or @import. The SVG
# xmlns attribute is a name, not a fetch, so it is not matched.
_FETCH = re.compile(
    r"""(?:href|src)\s*=\s*["']\s*(?:https?:)?//|url\(\s*["']?\s*(?:https?:)?//|@import"""
)


def test_the_served_page_fetches_nothing_from_another_host():
    page = (tui.WEB_TEMPLATES / "app_index.html").read_text(encoding="utf-8")
    assert not _FETCH.search(page), _FETCH.search(page)
    assert "googleapis" not in page
    assert "fonts/RobotoMono-VariableFont_wght.ttf" in page


def test_serve_hands_textual_serve_our_page(monkeypatch, tmp_path):
    seen: dict = {}

    class FakeServer:
        def __init__(self, command, **kwargs):
            seen.update(kwargs, command=command)

        def serve(self):
            seen["served"] = True

    import textual_serve.server

    monkeypatch.setattr(textual_serve.server, "Server", FakeServer)
    tui.serve(tmp_path)
    assert Path(seen["templates_path"]) == tui.WEB_TEMPLATES
    assert Path(seen["templates_path"]).is_absolute()
    assert seen["served"] is True
