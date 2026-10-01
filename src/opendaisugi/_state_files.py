"""How the gate opens, and writes, the files it keeps under its root
(GR-R-1, GR-R-2, GR-R-7 to GR-R-10).

Python twin of the Go package internal/statefile and the Rust module
gate::statefile. The three do the same thing and write the same words.
"""

from __future__ import annotations

import errno
import os
import secrets
import stat
import sys
import threading
from pathlib import Path

# The gate reads its own state through no link (GR-R-1, GR-R-2): the gate
# root, DISARMED, envelopes/, the envelope files, the mode markers and the
# files below. The root is opened as a directory without following a
# symlink at its last component (the path DOWN TO the root may follow
# links, a home directory on another mount for one), then each name below
# is opened by its fd with the same flags. A refused file reads as absent:
# DISARMED reads as not disarmed, an envelope as none, a marker as none.
# A refusal that is not a plain "missing" leaves one stderr note naming the
# file and the reason, once per file and reason in a process. The reasons
# are fixed words, the same in the Go and Rust gates, and the note ends
# with the fix: ``(fix: chmod 600 '<path>')`` and so on (GR-R-8). A reader can
# ask for the refusal as a third answer, beside opened and missing
# (``probe_*``), for the caller that must not read a refusal as an absence.
_NOTED: set[str] = set()
_NOTED_LOCK = threading.Lock()
_DIR_FLAGS = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC
_FILE_FLAGS = os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK | os.O_CLOEXEC


class StateWriteRefused(OSError):
    """A writer was refused a path the reader would refuse. The message is
    one line: ``<path>: <reason> (fix: ...)``."""


def quote(path: str) -> str:
    """``path`` in single quotes; a quote inside it is written as four
    characters, quote backslash quote quote. Always quoted, so the three
    ports write the same bytes."""
    return "'" + path.replace("'", "'\\''") + "'"


def fix_for(why: str, path: str, *, is_dir: bool) -> str:
    """What the owner does about a refusal (GR-R-8)."""
    q = quote(path)
    if why == "is a symbolic link":
        return f"remove the link {q}"
    if why in ("is not a directory", "is not a regular file"):
        return f"move {q} aside"
    if why == "is owned by another user":
        return f'chown "$(id -un)" {q}'
    if why == "has more than one hard link":
        return f"cp {q} {q}.new && mv {q}.new {q}"
    if why == "is writable by group or others":
        return f"chmod {'700' if is_dir else '600'} {q}"
    if why.startswith("is larger than "):
        return f"shorten or remove {q}"
    return f"check that this user can read {q}"


def refusal_text(path: str, why: str, *, is_dir: bool) -> str:
    """``<path>: <why> (fix: ...)``."""
    return f"{path}: {why} (fix: {fix_for(why, path, is_dir=is_dir)})"


def state_note(text: str) -> None:
    """One stderr line: ``daisugi gate: not reading <refusal text>``."""
    with _NOTED_LOCK:
        if text in _NOTED:
            return
        _NOTED.add(text)
    try:
        sys.stderr.write(f"daisugi gate: not reading {text}\n")
        sys.stderr.flush()
    except Exception:  # noqa: BLE001 - a note that cannot be written changes nothing
        pass


def _repair_note(path: str) -> None:
    """One stderr line when an operator command set a directory's mode."""
    try:
        sys.stderr.write(
            f"daisugi gate: repaired {path}: it was writable by group or others; now 700\n"
        )
        sys.stderr.flush()
    except Exception:  # noqa: BLE001 - a note that cannot be written changes nothing
        pass


def _why_not_opened(name: str, dir_fd: int | None, err: int | None = None) -> str | None:
    """Why an open of ``name`` failed, or None when it is plainly missing.
    ``err`` is the errno of that open. ENOENT with no-follow flags means the
    name did not exist when it was opened, and that stands: a writer may
    rename a file into place a moment later, and a second look at the name
    would then call a plain miss a refusal."""
    if err == errno.ENOENT:
        return None
    try:
        st = os.stat(name, dir_fd=dir_fd, follow_symlinks=False)
    except (FileNotFoundError, NotADirectoryError):
        return None
    except OSError:
        return "cannot be opened"
    if stat.S_ISLNK(st.st_mode):
        return "is a symbolic link"
    return "cannot be opened"


def _refuse(note_path: str, why: str, *, is_dir: bool) -> str:
    text = refusal_text(note_path, why, is_dir=is_dir)
    state_note(text)
    return text


def _open_dir(name: str, dir_fd: int | None, note_path: str) -> tuple[int | None, str | None]:
    """(fd, None) for the directory ``name`` (under ``dir_fd``, or a path
    for the root); (None, None) when it is missing; (None, refusal) when it
    is refused. The directory must be this user's and writable by no group
    and no other user."""
    try:
        fd = os.open(name, _DIR_FLAGS, dir_fd=dir_fd)
    except OSError as e:
        why = _why_not_opened(name, dir_fd, e.errno)
        if why is None:
            return None, None
        if why == "cannot be opened":
            try:
                if not stat.S_ISDIR(os.stat(name, dir_fd=dir_fd, follow_symlinks=False).st_mode):
                    why = "is not a directory"
            except OSError:
                pass
        return None, _refuse(note_path, why, is_dir=True)
    try:
        st = os.fstat(fd)
        if st.st_uid != os.geteuid():
            why = "is owned by another user"
        elif st.st_mode & 0o022:
            why = "is writable by group or others"
        else:
            return fd, None
    except OSError:
        why = "cannot be opened"
    os.close(fd)
    return None, _refuse(note_path, why, is_dir=True)


def open_state_dir(name: str, dir_fd: int | None, note_path: str) -> int | None:
    """``_open_dir`` without the refusal."""
    return _open_dir(name, dir_fd, note_path)[0]


def probe_state_dirs(root: Path, dirs: tuple[str, ...]) -> tuple[list[int] | None, str | None]:
    """The fds of the gate root and each directory below it, in order, with
    no refusal; or (None, None) when one is missing; or (None, refusal) when
    one is refused (the ones opened are closed). The caller closes the fds.
    The last is the directory itself."""
    base = str(Path(root))
    fds: list[int] = []
    rfd, why = _open_dir(base, None, base)
    if rfd is None:
        return None, why
    fds.append(rfd)
    rel = ""
    for d in dirs:
        rel = f"{rel}{d}/"
        dfd, why = _open_dir(d, fds[-1], os.path.join(base, rel.rstrip("/")))
        if dfd is None:
            for f in fds:
                os.close(f)
            return None, why
        fds.append(dfd)
    return fds, None


def open_state_dirs(root: Path, dirs: tuple[str, ...]) -> list[int] | None:
    """``probe_state_dirs`` without the refusal."""
    return probe_state_dirs(root, dirs)[0]


def probe_file_in(
    dir_fd: int, note_dir: str, name: str, *, max_bytes: int | None = None
) -> tuple[int | None, str | None]:
    """An fd for ``name`` in the directory ``dir_fd`` (called ``note_dir``
    in a note), as (fd, None); (None, None) when it is missing; (None,
    refusal) when it is refused, which also leaves a note (see above). The
    file must be a regular file this user owns, with one link, writable by
    no group and no other user (and at most ``max_bytes`` bytes, when
    given)."""
    note_path = os.path.join(note_dir, name)
    try:
        fd = os.open(name, _FILE_FLAGS, dir_fd=dir_fd)
    except OSError as e:
        why = _why_not_opened(name, dir_fd, e.errno)
        if why is None:
            return None, None
        return None, _refuse(note_path, why, is_dir=False)
    try:
        st = os.fstat(fd)
        if not stat.S_ISREG(st.st_mode):
            why = "is not a regular file"
        elif st.st_uid != os.geteuid():
            why = "is owned by another user"
        elif st.st_nlink != 1:
            why = "has more than one hard link"
        elif st.st_mode & 0o022:
            why = "is writable by group or others"
        elif max_bytes is not None and st.st_size > max_bytes:
            why = f"is larger than {max_bytes} bytes"
        else:
            return fd, None
    except OSError:
        why = "cannot be opened"
    os.close(fd)
    return None, _refuse(note_path, why, is_dir=False)


def open_file_in(
    dir_fd: int, note_dir: str, name: str, *, max_bytes: int | None = None
) -> int | None:
    """``probe_file_in`` without the refusal."""
    return probe_file_in(dir_fd, note_dir, name, max_bytes=max_bytes)[0]


def probe_state_file(
    root: Path, dirs: tuple[str, ...], name: str, *, max_bytes: int | None = None
) -> tuple[int | None, str | None]:
    """An fd for ``<root>/<dirs...>/<name>`` as (fd, None); (None, None)
    when it is missing; (None, refusal) when the root, a directory or the
    file is refused."""
    fds, why = probe_state_dirs(root, dirs)
    if fds is None:
        return None, why
    try:
        note_dir = os.path.join(str(Path(root)), *dirs)
        return probe_file_in(fds[-1], note_dir, name, max_bytes=max_bytes)
    finally:
        for f in fds:
            os.close(f)


def open_state_file(
    root: Path, dirs: tuple[str, ...], name: str, *, max_bytes: int | None = None
) -> int | None:
    """An fd for ``<root>/<dirs...>/<name>``, or None (see probe_file_in)."""
    return probe_state_file(root, dirs, name, max_bytes=max_bytes)[0]


def probe_state_text(root: Path, dirs: tuple[str, ...], name: str) -> tuple[str | None, str | None]:
    """The file's text as ``Path.read_text(encoding="utf-8")`` reads it
    (universal newlines, a strict decode) as (text, None); (None, None)
    when it is missing; (None, refusal) when it is refused."""
    fd, why = probe_state_file(root, dirs, name)
    if fd is None:
        return None, why
    with os.fdopen(fd, "r", encoding="utf-8") as f:
        return f.read(), None


def read_state_text(root: Path, dirs: tuple[str, ...], name: str) -> str | None:
    """``probe_state_text`` without the refusal."""
    return probe_state_text(root, dirs, name)[0]


# --- writers (GR-R-9) ---------------------------------------------------------
# A writer works from a directory fd: the root and each directory below it
# are opened without following a link, and the file is made by openat
# (O_CREAT | O_EXCL | O_NOFOLLOW, 0600) under a temp name nobody can
# predict, then renamed over its name inside the same directory. So a link
# that a concurrent write planted at a name is never written through. The
# gate's own files (checkpoint state, the ask channel) replace whatever
# stands at their name; the operator's files (DISARMED, an envelope, a
# graft rule) refuse a link or a non-file there, so the owner is told.


def _make_dirs(root: Path, dirs: tuple[str, ...], *, repair: bool) -> list[int]:
    """The fds of the root and each directory below it, made 0700 when
    missing. A link at or below the root, or a directory another user owns,
    raises StateWriteRefused. A directory that group or others can write is
    refused as well, as the reader refuses it (GR-R-8), unless ``repair``
    is set, which is the operator's own command: it sets every directory to
    0700 and says so on stderr when the mode was loose. The gate's own
    writers never repair and never chmod."""
    base = str(Path(root))
    Path(base).parent.mkdir(parents=True, exist_ok=True)
    fds: list[int] = []
    try:
        name, dir_fd, path = base, None, base
        for step in (None, *dirs):
            if step is not None:
                name, dir_fd, path = step, fds[-1], os.path.join(path, step)
            try:
                os.mkdir(name, 0o700, dir_fd=dir_fd)
            except FileExistsError:
                pass
            try:
                fd = os.open(name, _DIR_FLAGS, dir_fd=dir_fd)
            except OSError as e:
                why = _why_not_opened(name, dir_fd, e.errno) or "cannot be opened"
                if why == "cannot be opened" and not stat.S_ISDIR(
                    os.stat(name, dir_fd=dir_fd, follow_symlinks=False).st_mode
                ):
                    why = "is not a directory"
                raise StateWriteRefused(refusal_text(path, why, is_dir=True)) from None
            fds.append(fd)
            if os.fstat(fd).st_uid != os.geteuid():
                raise StateWriteRefused(refusal_text(path, "is owned by another user", is_dir=True))
            loose = bool(os.fstat(fd).st_mode & 0o022)
            if loose and not repair:
                raise StateWriteRefused(
                    refusal_text(path, "is writable by group or others", is_dir=True)
                )
            if repair:
                os.fchmod(fd, 0o700)
                if loose:
                    _repair_note(path)
    except BaseException:
        for f in fds:
            os.close(f)
        raise
    return fds


def make_state_dirs(root: Path, dirs: tuple[str, ...]) -> list[int]:
    """Public name of ``_make_dirs``. The caller closes the fds."""
    return _make_dirs(root, dirs, repair=True)


def write_state_file(
    root: Path, dirs: tuple[str, ...], name: str, text: str, *, replace_link: bool
) -> Path:
    """Write ``text`` to ``<root>/<dirs...>/<name>`` (0600) through the
    directory fd. With ``replace_link`` a link or other entry at the name is
    replaced; without it a link or a non-regular file there is refused
    (StateWriteRefused). Returns the path."""
    base = str(Path(root))
    fds = _make_dirs(root, dirs, repair=not replace_link)
    dfd = fds[-1]
    try:
        if not replace_link:
            try:
                st = os.stat(name, dir_fd=dfd, follow_symlinks=False)
            except FileNotFoundError:
                st = None
            if st is not None and not stat.S_ISREG(st.st_mode):
                why = "is a symbolic link" if stat.S_ISLNK(st.st_mode) else "is not a regular file"
                path = os.path.join(base, *dirs, name)
                raise StateWriteRefused(refusal_text(path, why, is_dir=False))
        flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW | os.O_CLOEXEC
        for _ in range(16):
            tmp = f".{name}.{secrets.token_hex(6)}.tmp"
            try:
                fd = os.open(tmp, flags, 0o600, dir_fd=dfd)
                break
            except FileExistsError:
                continue
        else:
            raise StateWriteRefused(
                refusal_text(os.path.join(base, *dirs, name), "cannot be opened", is_dir=False)
            )
        try:
            with os.fdopen(fd, "w", encoding="utf-8", newline="") as f:
                os.fchmod(f.fileno(), 0o600)
                f.write(text)
            os.rename(tmp, name, src_dir_fd=dfd, dst_dir_fd=dfd)
        except BaseException:
            try:
                os.unlink(tmp, dir_fd=dfd)
            except OSError:
                pass
            raise
    finally:
        for f in fds:
            os.close(f)
    return Path(base, *dirs, name)


def unlink_state_file(root: Path, dirs: tuple[str, ...], name: str) -> bool:
    """Remove ``<root>/<dirs...>/<name>`` through the directory fd. True when
    a file was removed; a missing file, directory or root is no error (False).
    A link at the name is removed as the link it is. Raises StateWriteRefused
    for a refused directory."""
    fds, why = _open_for_unlink(root, dirs)
    if fds is None:
        if why:
            raise StateWriteRefused(why)
        return False
    try:
        os.unlink(name, dir_fd=fds[-1])
    except FileNotFoundError:
        return False
    finally:
        for f in fds:
            os.close(f)
    return True


def _open_for_unlink(root: Path, dirs: tuple[str, ...]) -> tuple[list[int] | None, str | None]:
    """The directory fds for a removal: a missing one is quiet, a link or
    a directory another user owns is a refusal (no note: the caller says)."""
    base = str(Path(root))
    fds: list[int] = []
    path = base
    name, dir_fd = base, None
    for step in (None, *dirs):
        if step is not None:
            name, dir_fd, path = step, fds[-1], os.path.join(path, step)
        why = None
        try:
            fd = os.open(name, _DIR_FLAGS, dir_fd=dir_fd)
        except OSError as e:
            why = _why_not_opened(name, dir_fd, e.errno)
            if why is None:
                for f in fds:
                    os.close(f)
                return None, None
            if why == "cannot be opened":
                try:
                    if not stat.S_ISDIR(
                        os.stat(name, dir_fd=dir_fd, follow_symlinks=False).st_mode
                    ):
                        why = "is not a directory"
                except OSError:
                    pass
            why = refusal_text(path, why, is_dir=True)
        else:
            if os.fstat(fd).st_uid == os.geteuid():
                fds.append(fd)
                continue
            os.close(fd)
            why = refusal_text(path, "is owned by another user", is_dir=True)
        for f in fds:
            os.close(f)
        return None, why
    return fds, None


def verify_written(root: Path, dirs: tuple[str, ...], name: str) -> None:
    """Read a file the operator just wrote with the gate's reader. A refusal
    raises StateWriteRefused (one line), so the writer never claims success
    for a file the gate would not read (GR-R-9)."""
    fd, why = probe_state_file(root, dirs, name)
    if fd is None:
        path = os.path.join(str(Path(root)), *dirs, name)
        raise StateWriteRefused(why or refusal_text(path, "cannot be opened", is_dir=False))
    os.close(fd)


def refused_paths(root: Path) -> list[str]:
    """Every path under the gate root the gate would refuse to read, each
    as ``<path>: <reason> (fix: ...)`` (GR-R-8): the root; then, if it
    opens, DISARMED, and each of ``envelopes/``, ``grafts/`` and
    ``checkpoint-state/`` with every ``*.json`` file in it. A refused
    directory hides what is below it. What is missing is not listed."""
    out: list[str] = []
    fds, why = probe_state_dirs(root, ())
    if fds is None:
        return [why] if why else []
    for f in fds:
        os.close(f)
    _, why = probe_state_file(root, (), "DISARMED")
    if why:
        out.append(why)
    for sub in ("envelopes", "grafts", "checkpoint-state"):
        dfds, why = probe_state_dirs(root, (sub,))
        if dfds is None:
            if why:
                out.append(why)
            continue
        try:
            try:
                names = sorted(n for n in os.listdir(dfds[-1]) if n.endswith(".json"))
            except OSError:
                names = []
            for name in names:
                _, why = probe_file_in(dfds[-1], os.path.join(str(Path(root)), sub), name)
                if why:
                    out.append(why)
        finally:
            for f in dfds:
                os.close(f)
    return out
