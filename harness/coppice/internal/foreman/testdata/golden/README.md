# Foreman golden fixtures

These files are the Go reference's output for one fixed message script.
A port (Rust first) plays `script.json` and must write every file under
`out/` byte for byte. Regenerate them only on purpose:

    go test ./internal/foreman/ -run TestGoldenFixtures -update

## Input: script.json

- `config`: `node_bytes` (tree line cap), `view_max` and `view_min` (the
  sawtooth bounds, in bytes of view lines), `split_bytes` (message cap).
- `messages`: in order. For each one, set the clock to `date`, then append
  `text` as a message of `kind`.
- `probes`: run after the last message. `zoom` with `id` and `n`, or `date`
  with `id`. Write `out` (the result) or `err: true`.

## Output: out/

| File | What it holds |
|---|---|
| `main/YYYY-MM-DD.jsonl` | The log. One message per line: `{"i","kind","text","size","date"}`. The file is the UTC day of `date`. |
| `tree/YYYY-MM-DD.jsonl` | The tree. One node per line: `{"l","i","text","size"}`, in build order. The file is the UTC day of the clock when the node was built. |
| `view.json` | `{"count","batch","lines"}`. `lines` is the view as `[l, i]` pairs, oldest first. |
| `views.jsonl` | After each script message: `{"step","size","view"}`, where `view` is view.json as it was then. |
| `view.txt` | The rendered view. |
| `probes.json` | The probes with their results. |

## The rules a port must copy

JSON: field order as above, no spaces, one value per line ending in `\n`.
`<`, `>` and `&` are not escaped. Go escapes U+2028 and U+2029, so the
script has neither. `size` is the UTF-8 byte length of `text`.
`probes.json` and `script.json` are indented by two spaces.

Dates: UTC, `YYYY-MM-DDTHH:MM:SSZ`.

Append: replace invalid UTF-8 with U+FFFD, then split. No filter changes
the text: it is logged word for word. Each part is a message with the next
id. A cut is the longest prefix of at most N bytes that ends on a rune
boundary (a rune wider than N goes whole). An empty text is one empty
message.

Build: after a message is logged, queue its level-0 node, then while the
node is a right child (odd `i`), queue its parent. Build the queue in
order. A level-0 node is the message text when it fits in `node_bytes`,
else the summarizer's line. A parent is `left + "\n" + right` when that
fits, else the summarizer's line. Clip every line to `node_bytes`.

The fake summarizer of the fixtures:

- Compress: `"c "` + the first 60 bytes of the text.
- Merge: `"m "` + the first 40 bytes of left + `" | "` + the first 40 bytes of right.

(Each "first N bytes" is the rune-safe cut above.)

View: after the nodes are built, each new message adds the line `[0, i]`.
Size is the sum of the rendered lines. If size > `view_max`, set `batch`.
While `batch`: stop with `batch` false once size <= `view_min`; else merge
the most due pair, or stop (keeping `batch`) if none can merge.

Most due: a pair is two adjacent lines at one level `l`, the left with an
even `i` and the right with `i + 1`, whose parent node is built. Its due is
`(T - last) / 2^l`, where `T` is the message count and `last` is the id of
the pair's last message. Compare exactly, as rationals. The highest due
merges; among equals, the oldest pair.

Render: each line is `id+n|text` and `\n`, where `id` is the first message,
`n` is `2^l`, and each `\r\n`, `\n` or `\r` in the text is one space. A node
not built yet shows `(not summarized yet: zoom it)`. The view is the lines
between `<chat>\n` and `</chat>\n`.

Zoom `(id, n)`: `n` is a power of two, `id` a multiple of `n`, and
`id + n` at most the count. For `n = 1` it is the message text; else the
two halves as rendered lines.
