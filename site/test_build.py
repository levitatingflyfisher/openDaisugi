"""Tests for site/build.py.

Run: uv run --with pytest --with 'markdown-it-py>=3,<5' --with 'mdit-py-plugins>=0.4,<1' \
         pytest site/test_build.py -q
"""

import importlib.util
import re
from pathlib import Path

import pytest

SITE = Path(__file__).resolve().parent
PAGES = ["whitepaper.html", "yellow-paper.html", "start-here.html"]


def _load():
    spec = importlib.util.spec_from_file_location("site_build", SITE / "build.py")
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


@pytest.fixture(scope="module")
def built(tmp_path_factory):
    out = tmp_path_factory.mktemp("site")
    assert _load().main(["--out", str(out)]) == 0
    return out


def read(built, name):
    return (built / name).read_text(encoding="utf-8")


@pytest.mark.parametrize("name", PAGES)
def test_page_exists(built, name):
    assert (built / name).is_file()


@pytest.mark.parametrize("name", PAGES)
def test_title_nav_and_source_link(built, name):
    page = read(built, name)
    m = re.search(r"<title>(.+?)</title>", page)
    assert m and m.group(1).strip()
    assert 'aria-label="Site pages"' in page
    for target in ("index.html", "whitepaper.html", "yellow-paper.html", "start-here.html"):
        assert f'href="{target}"' in page
    assert re.search(
        r'<a href="https://github\.com/[^"]+/blob/master/docs/[^"]+\.md">Source on GitHub</a>', page
    )
    assert 'class="toc"' in page


@pytest.mark.parametrize("name", PAGES)
def test_no_external_requests(built, name):
    page = read(built, name)
    assert not re.search(r'\bsrc="(?:https?:)?//', page)
    assert not re.search(r'<link[^>]+href="(?:https?:)?//', page)
    assert "@import" not in page and "url(http" not in page
    css = (built / "assets" / "site.css").read_text(encoding="utf-8")
    assert "http://" not in css and "https://" not in css and "@import" not in css


@pytest.mark.parametrize("name", PAGES)
def test_template_adds_no_em_dash(built, name):
    page = read(built, name)
    shell = re.sub(r"<article>.*?</article>", "", page, flags=re.S)
    assert "—" not in shell
    assert "—" not in (built / "assets" / "site.css").read_text(encoding="utf-8")


def test_links_between_papers_become_site_links(built):
    page = read(built, "whitepaper.html")
    assert "docs/spec/yellow-paper.md" not in page
    assert 'href="yellow-paper.html"' in page
    assert "https://github.com/levitatingflyfisher/openDaisugi/blob/master/VISION.md" in page


def test_build_is_deterministic(built, tmp_path):
    _load().main(["--out", str(tmp_path)])
    for name in PAGES:
        assert read(built, name) == read(tmp_path, name)


def _loop_count(path):
    """The loop count of an animated WebP, or None for a still one."""
    b = path.read_bytes()
    i = b.find(b"ANIM")
    return None if i < 0 else int.from_bytes(b[i + 12 : i + 14], "little")


def test_moving_pictures_play_once_and_hold():
    """A reader can always read the final state: nothing on the page loops."""
    moving = [p for p in sorted((SITE / "assets").glob("*.webp")) if _loop_count(p) is not None]
    assert moving
    for p in moving:
        assert _loop_count(p) == 1, p.name
    css = (SITE / "assets" / "site.css").read_text(encoding="utf-8")
    assert "infinite" not in css
    assert not re.search(r"@keyframes [fg]\d+\{[^@]*100%\{opacity:0", css)


def _play_ms(path):
    """How long an animated WebP plays, from its frames."""
    b = path.read_bytes()
    i, ms = 12, 0
    while i < len(b):
        size = int.from_bytes(b[i + 4 : i + 8], "little")
        if b[i : i + 4] == b"ANMF":
            ms += int.from_bytes(b[i + 20 : i + 23], "little")
        i += 8 + size + (size & 1)
    return ms


def test_each_robot_picture_starts_as_a_drawing_and_loads_its_simulation_late():
    """The line drawing is in the markup. The simulation has no src until the script plays it."""
    index = (SITE / "index.html").read_text(encoding="utf-8")
    figs = re.findall(
        r'<figure id="fig-([a-z]+)" class="wide cycle" data-sim="assets/([^"]+)" data-ms="(\d+)">([\s\S]*?)</figure>',
        index,
    )
    assert [f[0] for f in figs] == ["delivery", "patrol", "many"]
    for name, sim, ms, body in figs:
        assert f'<svg id="dg-{name}" class="dg scene"' in body
        img = re.search(r"<img[^>]*>", body).group(0)
        assert " src=" not in img and 'alt="' in img
        assert _play_ms(SITE / "assets" / sim) == int(ms)
        assert f'href="assets/{sim}"' in body
    assert "<picture" not in index


def test_no_machine_details_on_the_public_pages():
    """No build timing, core count or memory size of anyone's machine."""
    for rel in ("site/index.html", "site/start-here.html", "site/CLAIMS.md", "docs/start-here.md"):
        text = (SITE.parent / rel).read_text(encoding="utf-8")
        assert not re.search(r"\b\d+[- ]?(?:cores?|threads)\b", text, re.I), rel
        assert not re.search(r"\b\d+ ?min(?:utes)? on\b", text, re.I), rel


def test_the_main_page_has_no_open_block():
    index = (SITE / "index.html").read_text(encoding="utf-8")
    assert "<h3>Open</h3>" not in index
    assert "still to build" not in index.lower()
    assert ">Roadmap</a>" in index


def test_every_section_closes_its_figures():
    index = (SITE / "index.html").read_text(encoding="utf-8")
    sections = re.findall(r'<section id="([^"]+)"([\s\S]*?)</section>', index)
    assert len(sections) >= 8
    for name, body in sections:
        assert body.count("<figure") == body.count("</figure>"), name


def test_each_tile_opens_a_picture_that_exists():
    """Six tiles, each a link to a section and a pointer to that section's picture."""
    index = (SITE / "index.html").read_text(encoding="utf-8")
    tiles = re.findall(r'<a class="tile" href="#([^"]+)"(?: data-zoom="([^"]+)")?>', index)
    assert len(tiles) == 6
    for section, fig in tiles:
        assert f'<section id="{section}"' in index, section
        if fig:
            assert f'<figure id="{fig}"' in index, fig
    # the robots tile goes to its section, where the pictures play
    assert dict(tiles)["robots"] == ""
    for ref in re.findall(r'<use href="#(dg-[^"]+)"', index):
        assert index.count(f'id="{ref}"') == 1, ref


def test_the_main_page_stays_short():
    """The page was 4,252 visible words before the rework. It stays under half."""
    import html as _html

    index = (SITE / "index.html").read_text(encoding="utf-8")
    body = re.sub(r"<(script|style|svg)[\s\S]*?</\1>", "", index)
    words = len(_html.unescape(re.sub(r"<[^>]+>", " ", body)).split())
    assert words < 2000, words


def test_no_render_sits_among_the_small_drawings():
    """The owner: a rendered picture among the line drawings breaks the page.

    The grid holds drawings only. A render shows only inside a robot
    picture that starts as a drawing.
    """
    index = (SITE / "index.html").read_text(encoding="utf-8")
    grid = re.search(r'<div class="map"[\s\S]*?</div>\n', index).group(0)
    assert "<img" not in grid and "<picture" not in grid
    assert grid.count('<svg class="thumb dg"') == 6
    for fig in re.findall(r"<figure[^>]*>[\s\S]*?</figure>", index):
        if "<img" in fig:
            assert re.match(r'<figure id="fig-[a-z]+" class="wide cycle"', fig), fig[:80]


HEADLINE = "Runtime assurance for agents and robots."
SECOND = "The model decides. Z3 proves it is allowed."


def test_the_first_screen():
    """The owner: runtime assurance is the big line. His sentence about the
    model and Z3 is the second line, in a smaller size, and the third line
    stays word for word. Each is one string, so a change is one edit here and
    one in the page."""
    index = (SITE / "index.html").read_text(encoding="utf-8")
    hero = re.search(r'<div class="col hero">[\s\S]*?</div>', index).group(0)
    assert hero.startswith(
        f'<div class="col hero">\n  <h1>{HEADLINE}</h1>\n  <p class="beat">{SECOND}</p>\n'
    )
    under = re.search(r'<p class="one">(.*?)</p>', hero).group(1)
    assert under == (
        "Every action is checked against your rules before it runs. Work that repeats compiles "
        "down to checked code, like a JIT compiler. No model call."
    )
    assert (
        "It checks the actions an agent takes and leaves the code the agent writes to other tools."
        in hero
    )
    assert hero.count('class="btn"') == 1 and hero.count('class="more"') == 1
    # the phrase is printed once on the first screen: no label over it, no echo in the facts row
    assert "kicker" not in hero
    assert hero.lower().count("runtime assurance") == 1
    assert re.search(r"<title>[^<]*runtime assurance for agents and robots</title>", index)
    desc = re.search(r'<meta name="description" content="([^"]*)">', index).group(1)
    assert desc.startswith("Runtime assurance for agents and robots") and "JIT compiler" in desc
    visible = re.sub(r"<(script|style|svg)[\s\S]*?</\1>", "", index[index.index("<main>") :])
    assert visible.count(HEADLINE) == 1 and visible.count(SECOND) == 1
    # the second line is set clearly smaller than the headline
    css = (SITE / "assets" / "site.css").read_text(encoding="utf-8")
    h1 = re.findall(r"\.hero h1\{font-size:clamp\([^,]+,[^,]+,([0-9.]+)rem\)", css)[-1]
    beat = re.findall(r"\.hero \.beat\{font-size:clamp\([^,]+,[^,]+,([0-9.]+)rem\)", css)[-1]
    assert float(beat) <= 0.65 * float(h1), (beat, h1)
    assert "formally verified" not in index.lower()


def test_the_arrows_drawing_reads_when_its_animation_never_starts():
    """The markup shows the final picture. The animation is an addition the script starts."""
    css = (SITE / "assets" / "site.css").read_text(encoding="utf-8")
    rules = re.findall(r"([^{}]+)\{([^{}]*animation:[^{}]*)\}", css)
    moving = [sel.strip() for sel, body in rules if "animation:none" not in body]
    assert moving and all(r.startswith(".anim.go ") for r in moving), moving
    index = (SITE / "index.html").read_text(encoding="utf-8")
    svg = re.search(r'<svg id="dg-anim" class="([^"]+)"', index).group(1)
    assert "go" not in svg.split()
    assert "classList.add('go')" in index


def test_no_pair_of_clipped_sentences_set_against_each_other():
    """The owner: no "X does this. Y does that." Each statement is one plain sentence.

    Two neighbouring sentences of five words or fewer are that shape. The
    owner's own line on the first screen is the one exception.
    """
    import html as _html

    index = (SITE / "index.html").read_text(encoding="utf-8")
    body = re.sub(r"<(script|style|svg|pre)[\s\S]*?</\1>", " ", index)
    found = []
    # \\b keeps <p from matching <picture and <li from matching <link
    for _tag, block in re.findall(r"<(p|h2|h3|li|figcaption|dd)\b[^>]*>([\s\S]*?)</\1>", body):
        text = _html.unescape(re.sub(r"<[^>]+>", "", block)).strip()
        if text == SECOND:
            continue
        sents = [x for x in re.split(r"(?<=[.])\s+", text) if x]
        for a, b in zip(sents, sents[1:], strict=False):
            if len(a.split()) <= 5 and len(b.split()) <= 5:
                found.append(f"{a} {b}")
    assert not found, found
