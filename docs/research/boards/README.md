# Field boards

A field board is a page in three parts: what the field built, with every screen
archived here; what survived verification; and what ours is, and should be. One
board per layer of the stack.

## Read them

Open `index.html` in a browser. Every page is static and every image is local,
so a clone is enough. Fonts load from Google Fonts when the network is there and
fall back to system fonts when it is not.

To serve them instead:

```
python3 -m http.server 7071 --directory docs/research/boards
```

then open http://127.0.0.1:7071/ .

## Make one

1. Write the research report first and commit it under `docs/research/`. The
   board cites it and never goes past it.
2. Make a directory here with `sources.json`: one entry per image with `slug`,
   `title`, `group` (vendor, oss, protocol, inspiration), `source_page`,
   `caption`, and one of `image_url`, `readme` (an `owner/repo` whose README
   holds the image, with an optional `pick` index), or nothing, in which case
   the page's social image is used.
3. Run `_shell/fetch.py <dir>`. It downloads each image into `img/`, checks it
   is a real image, shrinks anything wider than 1600 px, and writes
   `manifest.json` with the source, the direct URL, and the fetch date.
4. Write `verdicts.json`: rules that match a title and tag it steal, avoid,
   watch, unverified, or reference, with the reason.
5. Write `body.html`, starting with `<!-- title: ... -->`. Use the classes in
   `_shell/style.css`. Part one is the `#board` div the shell fills. Part two is
   the patterns. Part three is real screens first, then what should change.
6. Run `_shell/render.py`. It renders every board and the index.

## Rules

- Archive everything. A source that vanishes does not take the board with it.
  The repo is the record.
- Every tag traces to a report verdict. No tag from taste.
- Part three starts from the CLI as it runs today.
- Screens in part one belong to their makers and are here for study.
