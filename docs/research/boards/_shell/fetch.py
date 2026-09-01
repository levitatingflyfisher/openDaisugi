#!/usr/bin/env python3
"""Archive the images a field board cites.

Reads sources.json in a board directory, downloads each image into img/,
verifies it is a real image, shrinks anything wider than 1600 px, and writes
manifest.json with the source page, the direct URL, and the fetch date. The
copy in the repo is the record. A source that vanishes later does not take
the board with it.

Usage: fetch.py BOARD_DIR
"""

import datetime
import json
import os
import re
import subprocess
import sys
import urllib.parse

from PIL import Image

UA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/128 Safari/537.36"
MAX_W = 1600
BADGE = re.compile(r"shields\.io|badge|/logo|sponsor|avatar|\.svg$", re.I)
IMG_EXT = re.compile(r"\.(png|jpe?g|gif|webp)(\?|$)", re.I)


def curl(url, out=None, timeout=40):
    cmd = ["curl", "-sL", "-A", UA, "--max-time", str(timeout), url]
    if out:
        cmd += ["-o", out]
    r = subprocess.run(cmd, capture_output=True)
    return r.returncode == 0, r.stdout


def readme_images(repo):
    ok, body = curl(f"https://raw.githubusercontent.com/{repo}/HEAD/README.md")
    if not ok or not body:
        return []
    text = body.decode("utf-8", "replace")
    found = re.findall(r"!\[[^\]]*\]\(([^)\s]+)", text)
    found += re.findall(r'<img[^>]+src="([^"]+)"', text)
    found += re.findall(r"<source[^>]+src=\"([^\"]+)\"", text)
    urls = []
    for u in found:
        if u.startswith("http"):
            urls.append(u)
        else:
            urls.append(f"https://raw.githubusercontent.com/{repo}/HEAD/{u.lstrip('./')}")
    good = [u for u in urls if IMG_EXT.search(u) and not BADGE.search(u)]
    return good or [u for u in urls if IMG_EXT.search(u)]


def og_image(page):
    ok, body = curl(page)
    if not ok:
        return None
    m = re.search(r'property="og:image"\s+content="([^"]+)"', body.decode("utf-8", "replace"))
    if not m:
        m = re.search(r'content="([^"]+)"\s+property="og:image"', body.decode("utf-8", "replace"))
    return urllib.parse.urljoin(page, m.group(1)) if m else None


def shrink(path):
    im = Image.open(path)
    fmt = im.format
    w, h = im.size
    if w <= MAX_W or fmt == "GIF":
        return w, h
    nh = round(h * MAX_W / w)
    im = im.convert("RGB") if fmt in ("JPEG",) else im
    im = im.resize((MAX_W, nh), Image.LANCZOS)
    im.save(path, format=fmt, optimize=True, **({"quality": 85} if fmt == "JPEG" else {}))
    return MAX_W, nh


def main(board):
    src = json.load(open(os.path.join(board, "sources.json")))
    imgdir = os.path.join(board, "img")
    os.makedirs(imgdir, exist_ok=True)
    manifest, failed = [], []
    today = datetime.date.today().isoformat()
    for it in src:
        url = it.get("image_url")
        if not url and it.get("readme"):
            cands = readme_images(it["readme"])
            url = cands[it.get("pick", 0)] if len(cands) > it.get("pick", 0) else None
        if not url and it.get("source_page"):
            url = og_image(it["source_page"])
        if not url:
            failed.append((it["slug"], "no image url"))
            continue
        ext = (IMG_EXT.search(url) or [None, "png"])[1].lower().replace("jpeg", "jpg")
        out = os.path.join(imgdir, f"{it['slug']}.{ext}")
        ok, _ = curl(url, out)
        try:
            if not ok or os.path.getsize(out) < 8000:
                raise ValueError("small or failed")
            Image.open(out).verify()
            w, h = shrink(out)
        except Exception as e:
            if os.path.exists(out):
                os.remove(out)
            failed.append((it["slug"], f"{e}: {url}"))
            continue
        rec = {k: it[k] for k in ("slug", "title", "group", "source_page", "caption") if k in it}
        rec.update(
            {
                "file": f"img/{os.path.basename(out)}",
                "image_url": url,
                "fetched": today,
                "width": w,
                "height": h,
            }
        )
        if it.get("license"):
            rec["license"] = it["license"]
        manifest.append(rec)
    json.dump(
        manifest, open(os.path.join(board, "manifest.json"), "w"), indent=1, ensure_ascii=False
    )
    print(f"{len(manifest)} archived, {len(failed)} failed")
    for slug, why in failed:
        print(f"  FAIL {slug}: {why}")


if __name__ == "__main__":
    main(sys.argv[1])
