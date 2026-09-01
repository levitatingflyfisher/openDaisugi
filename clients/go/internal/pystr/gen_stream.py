"""Write testdata/stream.json: byte strings read the way the transcript
parsers read a file (open(path, encoding="utf-8"), one line at a time),
with the lines Python hands out before it raises and the error it raises.

    python clients/go/internal/pystr/gen_stream.py [--check]

--check writes nothing and exits 1 when the file differs from what it
would write.

Every input is synthetic. The file goes to a temporary directory.
"""

import hashlib
import json
import sys
import tempfile
from pathlib import Path

CHUNK = 8192


def inputs() -> list[list[tuple[bytes, int]]]:
    """Each input as parts: (bytes, times)."""
    out: list[list[tuple[bytes, int]]] = []
    line = b'{"a": 1}\n'
    bad = [
        b"\xff",
        b"\x80",
        b"\xc0\xaf",
        b"\xc1\xbf",
        b"\xc3(",
        b"\xc3",
        b"\xe2\x82",
        b"\xe2(",
        b"\xe2\x82(",
        b"\xe0\x80\x80",
        b"\xe0\xa0",
        b"\xed\xa0\x80",
        b"\xed\x9f\xbf",
        b"\xf0\x80\x80\x80",
        b"\xf0\x90\x80",
        b"\xf0\x90(",
        b"\xf0\x9f\x98(",
        b"\xf4\x90\x80\x80",
        b"\xf4\x8f\xbf\xbf",
        b"\xf5\x80\x80\x80",
        b"\xf8",
        b"\xfe\xff",
    ]
    for b in bad:
        out.append([(b, 1)])
        out.append([(line, 1), (b, 1)])
        out.append([(line, 1), (b, 1), (b"\n", 1), (line, 1)])
        out.append([(line, 1), (b"x", 1), (b, 1)])
    # Near the chunk boundaries, with a multi-byte code point cut by one.
    for base in (CHUNK, 2 * CHUNK):
        for off in range(-4, 5):
            pad = [(b"a" * 10 + b"\n", 1), (b"b", base + off - 11)]
            for b in (
                b"\xff",
                b"\xe2\x82\xac",
                b"\xe2\x82",
                b"\xf0\x9f\x98\x80",
                b"\xf0\x9f",
                b"\xc3",
            ):
                out.append([*pad, (b, 1), (b"\nz\n", 1)])
                out.append([*pad, (b, 1)])
    # Line ends before the error: LF, CRLF, a lone CR, and a CR at the end
    # of the decoded text.
    for sep in (b"\n", b"\r\n", b"\r"):
        out.append([(b"one" + sep + b"two" + sep + b"\xff", 1)])
    out.append([(b"x", CHUNK - 2), (b"\r\ny\n", 1), (b"z", CHUNK), (b"\xff", 1)])
    out.append([(b"x", CHUNK - 1), (b"\ry\n", 1), (b"z", CHUNK), (b"\xff", 1)])
    out.append([(b"x", CHUNK - 1), (b"\r", 1), (b"\xff", 1)])
    out.append([(b"line\n", 3000), (b"\xff", 1)])
    out.append([("é".encode(), 5000), (b"\n\xff", 1)])
    out.append([])
    out.append([("ok é ✓ \U0001f600\n".encode(), 1)])
    return out


def read(path: Path) -> tuple[list[str], str | None]:
    got: list[str] = []
    try:
        with open(path, encoding="utf-8") as f:
            for ln in f:
                got.append(ln)
    except UnicodeDecodeError as e:
        return got, str(e)
    return got, None


def main() -> None:
    cases = []
    with tempfile.TemporaryDirectory() as d:
        p = Path(d) / "t.bin"
        for parts in inputs():
            data = b"".join(b * n for b, n in parts)
            p.write_bytes(data)
            lines, err = read(p)
            text = "".join(lines).encode("utf-8")
            cases.append(
                {
                    "parts": [[b.hex(), n] for b, n in parts],
                    "lines_sha256": hashlib.sha256(text).hexdigest(),
                    "lines_len": len(text),
                    "error": err,
                }
            )
    out = Path(__file__).parent / "testdata" / "stream.json"
    text = json.dumps(cases, separators=(",", ":")).replace("},{", "},\n{") + "\n"
    if "--check" in sys.argv[1:]:
        if not out.exists() or out.read_text(encoding="utf-8") != text:
            print(f"{out} is not what the oracle writes now")
            sys.exit(1)
        return
    out.parent.mkdir(exist_ok=True)
    out.write_text(text, encoding="utf-8")
    print(f"wrote {len(cases)} cases to {out}")


if __name__ == "__main__":
    main()
