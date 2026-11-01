#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.11"
# dependencies = [
#   "markdown-it-py>=3,<5",
#   "mdit-py-plugins>=0.4,<1",
# ]
# ///
"""Render the repo's papers into static pages that match site/index.html.

    uv run site/build.py            # writes the pages next to index.html
    uv run site/build.py --out DIR  # writes them to DIR (plus a copy of assets)

The output is deterministic: no timestamps, no network, sorted everywhere.
"""

from __future__ import annotations

import argparse
import html
import posixpath
import re
import shutil
import sys
from pathlib import Path

from markdown_it import MarkdownIt
from mdit_py_plugins.footnote import footnote_plugin

SITE = Path(__file__).resolve().parent
ROOT = SITE.parent
REPO = "https://github.com/levitatingflyfisher/openDaisugi"

# source (repo-relative) -> (output name, nav label)
PAGES = {
    "docs/whitepaper.md": ("whitepaper.html", "Whitepaper"),
    "docs/spec/yellow-paper.md": ("yellow-paper.html", "Yellow paper"),
    "docs/start-here.md": ("start-here.html", "Start here"),
}

NAV = [
    ("index.html", "Home"),
    ("whitepaper.html", "Whitepaper"),
    ("yellow-paper.html", "Yellow paper"),
    ("start-here.html", "Start here"),
    ("index.html#try", "Install"),
]

TEMPLATE = """<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{title}</title>
<meta name="description" content="{description}">
<link rel="stylesheet" href="assets/site.css">
<script>document.documentElement.className+=" js"</script>
</head>
<body>
<div class="vert" lang="ja" aria-hidden="true">台杉</div>
<header class="top"><div class="col">
  <a class="mark" href="index.html" style="text-decoration:none;color:inherit"><span class="seal" lang="ja" aria-hidden="true">台杉</span><span>openDaisugi</span></a>
  <nav class="sc" aria-label="Site pages">
    {nav}
  </nav>
</div></header>
<main class="paper">
<nav class="toc" aria-label="Contents">
<p class="cap sc">Contents</p>
<ol>
{toc}
</ol>
</nav>
<article>
<p class="srcline"><a href="{src_url}">Source on GitHub</a></p>
{body}
</article>
</main>
<footer><div class="col">MIT license. This page loads only its own files. <a href="index.html">Back to the overview</a>.</div></footer>
</body>
</html>
"""


def slug(text: str, seen: dict[str, int]) -> str:
    base = re.sub(r"[^a-z0-9]+", "-", text.lower()).strip("-") or "section"
    n = seen.get(base, 0)
    seen[base] = n + 1
    return base if n == 0 else f"{base}-{n + 1}"


def fix_href(href: str, src: str) -> str:
    """Map a link in a source doc to a site link or a GitHub link."""
    if re.match(r"^[a-z][a-z0-9+.-]*:", href, re.I) or href.startswith("#"):
        return href
    path, _, frag = href.partition("#")
    if not path:
        return href
    target = posixpath.normpath(posixpath.join(posixpath.dirname(src), path))
    suffix = f"#{frag}" if frag else ""
    if target in PAGES:
        return PAGES[target][0] + suffix
    if target.startswith(".."):
        return REPO
    is_dir = path.endswith("/") or (ROOT / target).is_dir()
    kind = "tree" if is_dir else "blob"
    return f"{REPO}/{kind}/master/{target.rstrip('/')}{suffix}"


def make_md(src: str, toc: list, seen: dict[str, int]) -> MarkdownIt:
    md = MarkdownIt("commonmark", {"html": False, "linkify": False}).enable("table")
    md.use(footnote_plugin)

    def core(state):
        toks = state.tokens
        for i, t in enumerate(toks):
            if t.type == "heading_open":
                inline = toks[i + 1]
                text = "".join(
                    c.content for c in inline.children or [] if c.type in ("text", "code_inline")
                )
                hid = slug(text, seen)
                t.attrSet("id", hid)
                if t.tag in ("h2", "h3"):
                    toc.append((hid, text, t.tag))
            if t.type == "inline":
                for c in t.children or []:
                    if c.type == "link_open":
                        c.attrSet("href", fix_href(c.attrGet("href") or "", src))

    md.core.ruler.push("site_links_and_ids", core)

    def table_open(self, tokens, idx, options, env):
        return (
            '<div class="scroll" tabindex="0" role="region" aria-label="Table">'
            + self.renderToken(tokens, idx, options, env)
        )

    def table_close(self, tokens, idx, options, env):
        return self.renderToken(tokens, idx, options, env) + "</div>"

    md.add_render_rule("table_open", table_open)
    md.add_render_rule("table_close", table_close)
    return md


def render(src: str) -> tuple[str, str, str]:
    text = (ROOT / src).read_text(encoding="utf-8")
    toc: list = []
    md = make_md(src, toc, {})
    body = md.render(text)
    m = re.search(r"<h1[^>]*>(.*?)</h1>", body, re.S)
    title = html.unescape(re.sub(r"<[^>]+>", "", m.group(1))) if m else src
    h2 = [t for t in toc if t[2] == "h2"]
    items = h2 if len(h2) >= 3 else toc
    toc_html = "\n".join(
        f'<li><a href="#{i}">{html.escape(t, quote=False)}</a></li>' for i, t, _ in items
    )
    desc = ""
    p = re.search(r"<p><em>(.*?)</em></p>", body, re.S) or re.search(r"<p>(.*?)</p>", body, re.S)
    if p:
        desc = re.sub(r"\s+", " ", html.unescape(re.sub(r"<[^>]+>", "", p.group(1)))).strip()
    return title, desc, body, toc_html


def page(src: str, out_name: str) -> str:
    title, desc, body, toc_html = render(src)
    nav = "\n    ".join(
        f'<a href="{h}"' + (' aria-current="page"' if h == out_name else "") + f">{l}</a>"
        for h, l in NAV
    )
    return TEMPLATE.format(
        title=html.escape(title + " | openDaisugi", quote=True),
        description=html.escape(desc, quote=True),
        nav=nav,
        toc=toc_html,
        body=body.rstrip("\n"),
        src_url=f"{REPO}/blob/master/{src}",
    )


def main(argv: list[str]) -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--out", type=Path, default=SITE, help="output directory")
    args = ap.parse_args(argv)
    out = args.out.resolve()
    out.mkdir(parents=True, exist_ok=True)
    if out != SITE:
        shutil.copytree(SITE / "assets", out / "assets", dirs_exist_ok=True)
        shutil.copy2(SITE / "index.html", out / "index.html")
    for src, (name, _label) in sorted(PAGES.items()):
        (out / name).write_text(page(src, name), encoding="utf-8", newline="\n")
        print(f"wrote {out / name}")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
