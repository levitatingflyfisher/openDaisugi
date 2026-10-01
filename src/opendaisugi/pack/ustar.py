"""The bundle tar: a POSIX ustar archive written the same way, byte for
byte, by the Python, Go and Rust binaries, and read back by them.

The writer sorts the files by name. Each header has mode 0644, uid and
gid 0, mtime 0, empty user and group names and type 0 (a regular file).
A name of more than 100 bytes is split at a "/" into the prefix field
(at most 155 bytes) and the name field (at most 100 bytes). A name that
cannot be split (a wheel's file name can be longer than 100 bytes) gets
a POSIX extended header before it: a type "x" entry named
"././@PaxHeader" holding one record, "LEN path=NAME\n", then the file's
own header with the first 100 bytes of the name. The archive ends with
two zero blocks and no padding to a record size.

The reader takes regular files (and skips directory entries) from any
ustar archive, reads the "path" of an extended header, and refuses any
other entry, a name that is absolute or holds "..", and an archive cut
short.
"""

from __future__ import annotations

import io
from collections.abc import Iterator
from pathlib import Path
from typing import BinaryIO

BLOCK = 512
_CHUNK = 1 << 20


def _octal(n: int, width: int) -> bytes:
    """n in octal, zero-padded to width - 1 digits, then a NUL."""
    return format(n, "o").rjust(width - 1, "0").encode("ascii") + b"\0"


def split_name(name: str) -> tuple[bytes, bytes]:
    """(prefix, name) for a ustar header, or ValueError."""
    raw = name.encode("utf-8")
    if len(raw) <= 100:
        return b"", raw
    for i in range(len(raw) - 1, 0, -1):
        if raw[i : i + 1] == b"/" and i <= 155 and len(raw) - i - 1 <= 100:
            return raw[:i], raw[i + 1 :]
    raise ValueError(f"a name too long for a ustar header: {name}")


PAX_NAME = b"././@PaxHeader"


def _block(prefix: bytes, short: bytes, size: int, kind: bytes) -> bytes:
    h = bytearray(BLOCK)
    h[0 : len(short)] = short
    h[100:108] = _octal(0o644, 8)
    h[108:116] = _octal(0, 8)
    h[116:124] = _octal(0, 8)
    h[124:136] = _octal(size, 12)
    h[136:148] = _octal(0, 12)
    h[148:156] = b" " * 8
    h[156:157] = kind
    h[257:263] = b"ustar\0"
    h[263:265] = b"00"
    h[345 : 345 + len(prefix)] = prefix
    h[148:156] = format(sum(h), "o").rjust(6, "0").encode("ascii") + b"\0 "
    return bytes(h)


def pax_record(key: str, value: str) -> bytes:
    """One extended header record: its own length in decimal, a space,
    key=value and a newline."""
    body = f" {key}={value}\n".encode("utf-8")
    n = len(body) + 1
    while True:
        rec = str(n).encode("ascii") + body
        if len(rec) == n:
            return rec
        n = len(rec)


def header(name: str, size: int) -> bytes:
    """The header block of a file, after an extended header when the name
    does not fit the ustar fields."""
    try:
        prefix, short = split_name(name)
    except ValueError:
        rec = pax_record("path", name)
        pax = _block(b"", PAX_NAME, len(rec), b"x") + rec + b"\0" * (-len(rec) % BLOCK)
        return pax + _block(b"", name.encode("utf-8")[:100], size, b"0")
    return _block(prefix, short, size, b"0")


def pax_path(body: bytes) -> str | None:
    """The "path" record of an extended header, else None."""
    path = None
    while body:
        sp = body.find(b" ")
        if sp <= 0:
            break
        try:
            n = int(body[:sp])
        except ValueError:
            break
        if n <= sp or n > len(body):
            break
        key, _, value = body[sp + 1 : n].rstrip(b"\n").partition(b"=")
        if key == b"path":
            path = value.decode("utf-8", errors="replace")
        body = body[n:]
    return path


def _sorted(names):
    return sorted(names, key=lambda n: n.encode("utf-8"))


def write(files: list[tuple[str, bytes]]) -> bytes:
    """The archive of (name, data) pairs, sorted by name."""
    buf = io.BytesIO()
    data = dict(files)
    for name in _sorted(data):
        buf.write(header(name, len(data[name])))
        buf.write(data[name])
        buf.write(b"\0" * (-len(data[name]) % BLOCK))
    buf.write(b"\0" * (2 * BLOCK))
    return buf.getvalue()


def write_file(out: Path, files: list[tuple[str, Path]]) -> None:
    """The archive of (name, source path) pairs written to out, one file
    at a time."""
    src = dict(files)
    with open(out, "wb") as fh:
        for name in _sorted(src):
            size = src[name].stat().st_size
            fh.write(header(name, size))
            with open(src[name], "rb") as f:
                while chunk := f.read(_CHUNK):
                    fh.write(chunk)
            fh.write(b"\0" * (-size % BLOCK))
        fh.write(b"\0" * (2 * BLOCK))


def _field(b: bytes) -> bytes:
    return b.split(b"\0", 1)[0]


def _exact(fh: BinaryIO, n: int) -> bytes:
    buf = fh.read(n)
    if len(buf) < n:
        raise ValueError("a tar archive cut short")
    return buf


def entries(fh: BinaryIO) -> Iterator[tuple[str, int]]:
    """(name, size) of each regular file. The caller reads exactly size
    bytes of the file from fh before the next step."""
    long_path = None
    while True:
        h = fh.read(BLOCK)
        if len(h) < BLOCK or h == b"\0" * BLOCK:
            return
        try:
            size = int(_field(h[124:136]).strip() or b"0", 8)
        except ValueError:
            raise ValueError("a tar header with a bad size") from None
        name = _field(h[0:100])
        if h[257:262] == b"ustar":
            prefix = _field(h[345:500])
            if prefix:
                name = prefix + b"/" + name
        text = name.decode("utf-8", errors="replace")
        kind = h[156:157]
        if kind in (b"x", b"g"):
            body = _exact(fh, size)
            _exact(fh, -size % BLOCK)
            if kind == b"x":
                long_path = pax_path(body)
            continue
        if long_path is not None:
            text, long_path = long_path, None
        if kind == b"5":
            _exact(fh, size + (-size % BLOCK))
            continue
        if kind not in (b"0", b"\0"):
            raise ValueError(f"a tar entry that is not a file: {text}")
        parts = text.split("/")
        if not text or text.startswith("/") or ".." in parts:
            raise ValueError(f"a name outside the bundle: {text}")
        yield text, size
        _exact(fh, -size % BLOCK)


def read(data: bytes) -> list[tuple[str, bytes]]:
    """The (name, data) pairs of the regular files, sorted by name."""
    fh = io.BytesIO(data)
    out = [(name, _exact(fh, size)) for name, size in entries(fh)]
    return sorted(out, key=lambda f: f[0].encode("utf-8"))


def extract(tar: Path, dest: Path) -> list[str]:
    """Write the archive's files under dest, one at a time; their names."""
    names = []
    with open(tar, "rb") as fh:
        for name, size in entries(fh):
            p = dest / name
            p.parent.mkdir(parents=True, exist_ok=True)
            with open(p, "wb") as out:
                left = size
                while left:
                    chunk = _exact(fh, min(left, _CHUNK))
                    out.write(chunk)
                    left -= len(chunk)
            names.append(name)
    return names
