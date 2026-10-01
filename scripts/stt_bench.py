"""Measure speech-to-text engines on real speech: LibriSpeech test-clean and
test-other. Not a test: it prints, it does not assert. The table it gives is
in docs/research/stt-2026-10.md ("Measured on this box (real speech)").

Three steps:

  prep   pick N utterances from each archive with a fixed seed, extract only
         those, and write 16 kHz mono 16-bit WAVs and a manifest.
  run    run one engine over one set and write one JSON line per utterance.
  score  read the JSON lines and print WER, latency and memory per engine.

Engines:

  fw:MODEL           faster-whisper, CPU int8, the model loaded once in this
                     process (the way the voice server keeps it). Needs the
                     [voice] extra and the model in the Hugging Face cache.
  cli:NAME           an external program, one process per clip, start-up
                     included. --cmd is its command line with {clip} in it.
                     A stderr line with load_ms=... decode_ms=... is read.
  resident:NAME      an external program that loads once and reads a list of
                     clips. --cmd has {list} in it; each stdout line is
                     PATH<TAB>DECODE_MS<TAB>TEXT.
  proto:NAME         an engine program run the way the voice server runs it:
                     one resident child on the daisugi-voice-1 protocol
                     (src/opendaisugi/voice/resident.py). --cmd is its
                     command line. The time is the whole round trip of one
                     clip, the frame out and the reply line back.

WER is the Open ASR Leaderboard's: Whisper's EnglishTextNormalizer on both
sides, then the word edits of all utterances over the words of all
references. The leaderboard also maps British spellings to American ones; no
copy of that map is on this box, so the map is empty here.

Needs the dev environment: transformers (for the normalizer), and
faster-whisper for the fw engines. ffmpeg must be on PATH for prep.

Usage: uv run --no-sync python scripts/stt_bench.py prep --archive A.tar.gz ...
"""

from __future__ import annotations

import argparse
import json
import random
import statistics
import subprocess
import tarfile
import time
import wave
from pathlib import Path

SEED = 20261001
DEFAULT_DIR = Path.home() / ".cache" / "opendaisugi" / "bench"


def _sets_in(archive: Path) -> tuple[str, dict[str, str], dict[str, str]]:
    """Return the set name, utterance id -> FLAC member, id -> reference."""
    flacs: dict[str, str] = {}
    refs: dict[str, str] = {}
    name = ""
    with tarfile.open(archive, "r:gz") as tf:
        for m in tf:
            parts = m.name.split("/")
            if len(parts) > 2 and not name:
                name = parts[1]
            if m.name.endswith(".flac"):
                flacs[Path(m.name).stem] = m.name
            elif m.name.endswith(".trans.txt"):
                f = tf.extractfile(m)
                assert f is not None
                for line in f.read().decode().splitlines():
                    uid, _, text = line.partition(" ")
                    refs[uid] = text
    return name, flacs, refs


def prep(archive: Path, n: int, out: Path) -> None:
    name, flacs, refs = _sets_in(archive)
    ids = sorted(flacs)
    chosen = sorted(random.Random(SEED).sample(ids, n))
    dest = out / name
    dest.mkdir(parents=True, exist_ok=True)
    want = {flacs[i]: i for i in chosen}
    with tarfile.open(archive, "r:gz") as tf:
        for m in tf:
            uid = want.get(m.name)
            if uid is None:
                continue
            f = tf.extractfile(m)
            assert f is not None
            subprocess.run(
                [
                    "ffmpeg",
                    "-nostdin",
                    "-loglevel",
                    "error",
                    "-y",
                    "-i",
                    "pipe:0",
                    "-ar",
                    "16000",
                    "-ac",
                    "1",
                    "-c:a",
                    "pcm_s16le",
                    str(dest / f"{uid}.wav"),
                ],
                input=f.read(),
                check=True,
            )
    with open(out / f"{name}.tsv", "w") as fh:
        for uid in chosen:
            wav = dest / f"{uid}.wav"
            with wave.open(str(wav)) as w:
                secs = w.getnframes() / w.getframerate()
            fh.write(f"{uid}\t{secs:.3f}\t{refs[uid]}\n")
    print(f"{name}: {len(chosen)} of {len(ids)} utterances, seed {SEED}, in {dest}")


def _manifest(out: Path, name: str) -> list[tuple[str, float, str, Path]]:
    rows = []
    for line in (out / f"{name}.tsv").read_text().splitlines():
        uid, secs, ref = line.split("\t")
        rows.append((uid, float(secs), ref, out / name / f"{uid}.wav"))
    return rows


def _maxrss_kb() -> int:
    import resource

    return resource.getrusage(resource.RUSAGE_SELF).ru_maxrss


def _children_maxrss_kb() -> int:
    """The largest peak RSS of any child process reaped so far."""
    import resource

    return resource.getrusage(resource.RUSAGE_CHILDREN).ru_maxrss


def _parse_timing(err: str) -> dict[str, float]:
    t: dict[str, float] = {}
    for tok in err.split():
        k, _, v = tok.partition("=")
        if k in ("load_ms", "decode_ms"):
            try:
                t[k] = float(v)
            except ValueError:
                pass
    return t


def run(engine: str, name: str, out: Path, results: Path, cmd: str, threads: int) -> None:
    rows = _manifest(out, name)
    kind, _, label = engine.partition(":")
    lines = []
    if kind == "fw":
        from faster_whisper import WhisperModel

        t0 = time.monotonic()
        model = WhisperModel(
            label, device="cpu", compute_type="int8", cpu_threads=threads, local_files_only=True
        )
        load_s = time.monotonic() - t0
        for uid, secs, ref, wav in rows:
            a = time.monotonic()
            segs, _ = model.transcribe(str(wav))
            text = " ".join(s.text.strip() for s in segs).strip()
            lines.append(
                {
                    "id": uid,
                    "secs": secs,
                    "ref": ref,
                    "hyp": text,
                    "wall_s": time.monotonic() - a,
                    "load_s": load_s,
                }
            )
        rss = _maxrss_kb()
        for d in lines:
            d["rss_kb"] = rss
    elif kind == "cli":
        for uid, secs, ref, wav in rows:
            a = time.monotonic()
            p = subprocess.Popen(
                cmd.format(clip=wav),
                shell=True,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                text=True,
            )
            stdout, stderr = p.communicate()
            wall = time.monotonic() - a
            if p.returncode != 0:
                raise SystemExit(f"{engine} failed on {uid}: {stderr.strip().splitlines()[-1:]}")
            t = _parse_timing(stderr)
            text = " ".join(x.strip() for x in stdout.splitlines() if x.strip())
            lines.append(
                {
                    "id": uid,
                    "secs": secs,
                    "ref": ref,
                    "hyp": text,
                    "wall_s": wall,
                    "load_s": t.get("load_ms", 0) / 1000,
                    "decode_s": t.get("decode_ms", 0) / 1000,
                }
            )
        rss = _children_maxrss_kb()
        for d in lines:
            d["rss_kb"] = rss
    elif kind == "resident":
        lst = results / f"{engine.replace(':', '-')}-{name}.list"
        lst.write_text("".join(f"{wav}\n" for *_, wav in rows))
        p = subprocess.Popen(
            cmd.format(list=lst),
            shell=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )
        stdout, stderr = p.communicate()
        if p.returncode != 0:
            raise SystemExit(f"{engine} failed: {stderr.strip()[-400:]}")
        by_path = {}
        for line in stdout.splitlines():
            path, ms, text = line.split("\t", 2)
            by_path[path] = (float(ms) / 1000, text.strip())
        load_s = _parse_timing(stderr).get("load_ms", 0) / 1000
        for uid, secs, ref, wav in rows:
            dec, text = by_path[str(wav)]
            lines.append(
                {
                    "id": uid,
                    "secs": secs,
                    "ref": ref,
                    "hyp": text,
                    "wall_s": dec,
                    "load_s": load_s,
                    "rss_kb": _children_maxrss_kb(),
                }
            )
    elif kind == "proto":
        import shlex

        from opendaisugi.voice import resident

        eng = resident.ResidentEngine(
            label,
            shlex.split(cmd),
            binary=label,
            unavailable=RuntimeError,
            loading=RuntimeError,
        )
        t0 = time.monotonic()
        eng.start()
        load_s = time.monotonic() - t0
        try:
            for uid, secs, ref, wav in rows:
                text, _dur, wall = eng.transcribe(wav.read_bytes())
                lines.append(
                    {
                        "id": uid,
                        "secs": secs,
                        "ref": ref,
                        "hyp": " ".join(text.split()),
                        "wall_s": wall,
                        "load_s": load_s,
                    }
                )
        finally:
            eng.stop()
        rss = _children_maxrss_kb()
        for d in lines:
            d["rss_kb"] = rss
    else:
        raise SystemExit(f"unknown engine kind {kind}")
    dest = results / f"{engine.replace(':', '-')}-{name}.jsonl"
    with open(dest, "w") as fh:
        for d in lines:
            d["engine"] = engine
            d["set"] = name
            d["threads"] = threads
            fh.write(json.dumps(d) + "\n")
    print(f"wrote {dest}")


def _edits(a: list[str], b: list[str]) -> int:
    prev = list(range(len(b) + 1))
    for i, x in enumerate(a, 1):
        cur = [i] + [0] * len(b)
        for j, y in enumerate(b, 1):
            cur[j] = min(prev[j] + 1, cur[j - 1] + 1, prev[j - 1] + (x != y))
        prev = cur
    return prev[-1]


def score(files: list[Path]) -> None:
    from transformers.models.whisper.english_normalizer import EnglishTextNormalizer

    norm = EnglishTextNormalizer({})
    print(
        "| engine | set | n | WER | median s | p90 s | median s, 4-6 s clips "
        "| p90 s, 4-6 s clips | load s | peak RSS MB |"
    )
    print("|---|---|---|---|---|---|---|---|---|---|")
    for f in files:
        ds = [json.loads(x) for x in f.read_text().splitlines()]
        errs = words = 0
        for d in ds:
            r = norm(d["ref"]).split()
            h = norm(d["hyp"]).split()
            errs += _edits(r, h)
            words += len(r)
        walls = sorted(d["wall_s"] for d in ds)
        p90 = walls[min(len(walls) - 1, int(round(0.9 * (len(walls) - 1))))]
        mid = [d["wall_s"] for d in ds if 4.0 <= d["secs"] <= 6.0]
        mid_s = f"{statistics.median(mid):.2f} (n={len(mid)})" if mid else "-"
        mids = sorted(mid)
        mid90 = (
            f"{mids[min(len(mids) - 1, int(round(0.9 * (len(mids) - 1))))]:.2f}" if mids else "-"
        )
        loads = [d.get("load_s", 0) for d in ds]
        rss = max(d.get("rss_kb", 0) for d in ds) / 1024
        print(
            f"| {ds[0]['engine']} | {ds[0]['set']} | {len(ds)} | {100 * errs / words:.2f}% | "
            f"{statistics.median(walls):.2f} | {p90:.2f} | {mid_s} | {mid90} | "
            f"{statistics.median(loads):.2f} | {rss:.0f} |"
        )


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    sub = ap.add_subparsers(dest="action", required=True)
    p = sub.add_parser("prep")
    p.add_argument("--archive", type=Path, action="append", required=True)
    p.add_argument("-n", type=int, default=100)
    p.add_argument("--out", type=Path, default=DEFAULT_DIR)
    r = sub.add_parser("run")
    r.add_argument("engine")
    r.add_argument("--set", required=True)
    r.add_argument("--out", type=Path, default=DEFAULT_DIR)
    r.add_argument("--results", type=Path, required=True)
    r.add_argument("--cmd", default="")
    r.add_argument("--threads", type=int, default=4)
    s = sub.add_parser("score")
    s.add_argument("files", type=Path, nargs="+")
    a = ap.parse_args()
    if a.action == "prep":
        for arc in a.archive:
            prep(arc, a.n, a.out)
    elif a.action == "run":
        a.results.mkdir(parents=True, exist_ok=True)
        run(a.engine, a.set, a.out, a.results, a.cmd, a.threads)
    else:
        score(a.files)


if __name__ == "__main__":
    main()
