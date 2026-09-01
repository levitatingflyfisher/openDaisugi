#!/usr/bin/env python3
"""Render every field board under docs/research/boards into a static page.

A board is a directory with body.html, an optional manifest.json written by
fetch.py, and an optional verdicts.json that tags each archived image with the
report's conclusion. The shell (style.css and board.js) is shared, so a board
is only its words, its mockups, and its archived images.

Usage: render.py            renders every board and the index
"""

import glob
import json
import os
import re

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
FONTS = (
    "https://fonts.googleapis.com/css2?family=Bricolage+Grotesque:opsz,wght@12..96,300..700"
    "&family=Instrument+Sans:ital,wght@0,400..700;1,400&family=JetBrains+Mono:wght@400;500;600&display=swap"
)


def head(title, shell):
    return f"""<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{title}</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link rel="stylesheet" href="{FONTS}">
<link rel="stylesheet" href="{shell}/style.css">
</head>
<body>
<div class="wrap">
"""


def foot(items, shell):
    return f"""
</div>
<script>window.ITEMS = {json.dumps(items, ensure_ascii=False)};</script>
<script src="{shell}/board.js"></script>
</body>
</html>
"""


def verdict(item, rules):
    if item.get("group") == "inspiration":
        return "reference", "reference", None
    text = f"{item.get('title', '')} {item.get('slug', '')}"
    for r in rules:
        if re.search(r["match"], text, re.I):
            return r["verdict"], r.get("label", r["verdict"]), r.get("note")
    return "reference", "reference", None


ORDER = {"vendor": 0, "protocol": 1, "oss": 2, "inspiration": 3}


def items_for(board):
    path = os.path.join(board, "manifest.json")
    if not os.path.exists(path):
        return []
    rules = []
    vpath = os.path.join(board, "verdicts.json")
    if os.path.exists(vpath):
        rules = json.load(open(vpath))
    out = []
    for it in json.load(open(path)):
        if not os.path.exists(os.path.join(board, it["file"])):
            continue
        cls, label, note = verdict(it, rules)
        it["verdict"], it["verdict_label"] = cls, label
        if note and not it.get("note"):
            it["note"] = note
        out.append(it)
    out.sort(key=lambda i: (ORDER.get(i.get("group"), 9), i.get("title", ""), i.get("slug", "")))
    return out


def title_of(body, fallback):
    m = re.match(r"\s*<!--\s*title:\s*(.+?)\s*-->", body)
    return m.group(1) if m else fallback


def render_board(board):
    body = open(os.path.join(board, "body.html")).read()
    name = os.path.basename(board)
    items = items_for(board)
    page = head(title_of(body, name), "../_shell") + body + foot(items, "../_shell")
    open(os.path.join(board, "index.html"), "w").write(page)
    return name, len(items)


def render_index(boards):
    body_path = os.path.join(ROOT, "index-body.html")
    if not os.path.exists(body_path):
        return
    body = open(body_path).read()
    page = head(title_of(body, "Field boards"), "_shell") + body + foot([], "_shell")
    open(os.path.join(ROOT, "index.html"), "w").write(page)


if __name__ == "__main__":
    done = []
    for body in sorted(glob.glob(os.path.join(ROOT, "*", "body.html"))):
        done.append(render_board(os.path.dirname(body)))
    render_index(done)
    for name, n in done:
        print(f"{name}: {n} images")
