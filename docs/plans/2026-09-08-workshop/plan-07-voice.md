# The voice bridge — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development
> (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship `daisugi voice serve` (transcribe + optional cleanup + deliver-to-pane over
HTTP), `daisugi voice ptt` (laptop push-to-talk), `daisugi voice arm|disarm` (the direct-send
grant), and the phone's record button — so a voice clip recorded anywhere lands as text in a
chosen pane's prompt box, previewed by the operator before the loop sees it.

**Architecture:** A stdlib `http.server` process (`voice/server.py`) owns three endpoints:
`GET /health`, `POST /transcribe` (audio in, text out, via a pluggable `Engine`), `POST
/deliver` (text out to a pane, through the §3.2 `PaneBackend` contract, preview by default,
send only when the pane is armed). Two thin clients post to it: the laptop's push-to-talk
terminal program (`voice/ptt.py`) and the phone PWA's record button (a small vanilla-JS
module dropped into spec-06's static assets). An optional cleanup pass
(`voice/cleanup.py`) runs one fixed-prompt local-model call and self-journals it as a
repeatable `voice.cleanup` turn, so the garden can distil it like any other repeated ask.

**Tech Stack:** Python 3.12, stdlib `http.server` (no ASGI/httpx for this surface — see Task
5's module docstring), `faster-whisper` (CTranslate2, CPU int8 by default) with `sherpa-onnx`
(NeMo Parakeet-TDT) as an opt-in second engine, `litellm` (already a hard dependency) for the
cleanup call, `sounddevice` for the laptop mic, plain JS (no framework) for the record button.

**Spec:** `docs/plans/2026-09-08-workshop/spec-07-voice.md` (master:
`docs/plans/2026-09-08-workshop/00-master-spec.md` §5.11, §3.6). Depends on spec-03 (the
`PaneBackend` protocol and `registry.pick_backend`, from `opendaisugi.floor`) and spec-06 (the
phone PWA and its bearer-token convention) — per the master's critical path (00→01→02→03→06→07)
those exist by the time this plan runs. This plan decouples from them wherever a real API
surface isn't yet fixed (Task 4's `deliver.py` treats `PaneBackend`/`PaneRef` as
`TYPE_CHECKING`-only and duck-typed; Task 5's `server.py` falls back to a local stand-in
`PaneRef` shape when `opendaisugi.floor` is not yet installed, and takes its backend through
an injected `backend_factory`), and calls out the one place it cannot fully decouple (Task
5's bearer-token file path, which spec-06 does not itself pin — flagged there as a cross-plan
assumption).

## Corrections from adversarial review (2026-09-08 — authoritative over the tasks below)

**Tasks 0, 1, 2, 4, 5, 6, 7, 8 are ready to build now** — the pinned faster-whisper and
sherpa-onnx facts check out against upstream (below), the audio-normalization fallback chain
never fails silently, `pick_engine`'s CPU/CUDA choice is opt-in-and-verified rather than
auto-switched, and the delivery rule in Tasks 4–5 is genuinely fail-closed: `/deliver
mode=send` on an unarmed pane is refused before the backend is ever touched
(`deliver.py:1746`, `is_armed` checked first), an arm grant expires by wall-clock comparison
on every check (no caching), and `VoiceRequestHandler._authorized()` runs inside `do_POST`
*before* dispatch, so the bearer-token check applies to `/deliver` exactly as it does to
`/transcribe` — a bad or missing token off-loopback is 401 either way, and `build_server()`
goes further than the spec requires by refusing to even bind off-loopback without a readable
token file. `ptt.py` and `record.js` both only ever call `mode="preview"`; nothing in this
plan defaults to `send`. No path was found that delivers unseen text to a loop.

- **SHOULD-FIX — Decision 1 (plan header + Task 3) re-decides master §5.11's crux on a
  factual error.** The master spec is explicit: the cleanup call is "journaled like any other
  turn, so the garden can distil it into a frozen pathway... the bridge does not special-case
  it" (§5.11), and a voice transcript is the operator's own prompt text — no more private than
  a typed one, which the journal already records unconditionally. Decision 1 inverts this: it
  justifies routing the cleanup call around `Gateway.prepare`/`finish` and hardcoding
  `task="voice.cleanup"`/`ask="voice.cleanup"` (`cleanup.py:1483`) by claiming "the repo's own
  posture is that raw content only ever enters a store opt-in (`capture_answers`, off by
  default)". That is not what `capture_answers` gates. `capture_answers` (`gateway_pipeline.py`
  `Gateway.finish`, ~line 130) gates the *response* text going into the answer-reuse store. The
  *task*/*ask* fields — the raw human prompt itself — are written to `turns.jsonl`
  unconditionally, on every turn, whenever journalling is on at all:
  `gateway_pipeline.py:87-88` (`task = _latest_user_text(body); ask = _new_user_text(body)`)
  feeds straight into `record_turn(task=prepared.task, ask=prepared.ask)` with no opt-in gate.
  So a typed prompt already gets exactly the treatment the master says a voice transcript
  deserves; Decision 1 gives voice transcripts *stricter* privacy than typed prompts get today,
  which is the special case §5.11 forbids, built on a claim about `capture_answers` that is
  false. It also breaks the reuse signal it claims to serve: `gateway_cluster`/
  `gateway_distill` rank repeat clusters by clustering real, distinct `task` text (paraphrase
  included) to find out *what* is being asked repeatedly; collapsing every cleanup call — no
  matter its content — into one identical literal string produces a single meaningless
  mega-cluster instead of the genuine repeat signal a frozen pathway would be built from. Fix:
  have `clean_transcript()` journal the real transcript as `task`/`ask` (still tagged
  `tier="tier1-local"` and priced at `(0.0, 0.0)`, exactly as now) through the normal
  `record_turn` path, so it is journaled like any other turn per §5.11. If transcript privacy
  is wanted beyond what a typed prompt already gets, that is a new, explicitly-named opt-out
  config field (mirroring `capture_answers`'s own opt-in shape) — not a silent substitution
  justified by misdescribing an existing control.
- **SHOULD-FIX — Task 0's Parakeet-TDT archive size is wrong by ~25%.** Stated three times as
  "~600 MB" (`spec-07-voice.md`'s own header via this plan's "Recorded facts" section, the
  `pyproject.toml` `voice-parakeet` comment, and `pins.py`'s module docstring). Verified via
  `gh api repos/k2-fsa/sherpa-onnx/releases/tags/asr-models`:
  `sherpa-onnx-nemo-parakeet-tdt-0.6b-v2-int8.tar.bz2` is 482,468,385 bytes (~460 MB / 460
  MiB), not ~600 MB. Fix: change all three occurrences to "~460 MB" (or state the exact byte
  count from the release asset). Every other faster-whisper and sherpa-onnx fact this plan
  states was verified against upstream and is accurate: `WhisperModel(model_size_or_path: str,
  device="cpu"|"cuda"|"auto", compute_type=...)` and `.transcribe(audio: Union[str, BinaryIO,
  np.ndarray], language=None) -> Tuple[Iterable[Segment], TranscriptionInfo]` match
  `faster_whisper/transcribe.py` exactly; `Segment.start/.end/.text` match; `decode_audio()` in
  `faster_whisper/audio.py` passes a `BinaryIO` straight to `av.open()`, confirming a bare
  `io.BytesIO` needs no temp file; `tiny.en`/`base.en` are real entries in `utils.py`'s
  `_MODELS`. `sherpa_onnx.OfflineRecognizer.from_transducer(encoder, decoder, joiner, tokens,
  ..., model_type: str = "transducer", ...)` and the NeMo-specific
  `model_type="nemo_transducer"` both match the installed package's signature and the official
  Parakeet Python example (`python-api-examples/offline-nemo-parakeet-decode-file.py`) and CLI
  docs verbatim, down to `create_stream()` → `stream.accept_waveform(sample_rate, audio)` →
  `decode_stream(stream)` → `stream.result.text`; and `encoder.int8.onnx`/`decoder.int8.onnx`/
  `joiner.int8.onnx`/`tokens.txt` are the archive's real member names.
- **SHOULD-FIX — no task registers the voice bridge's honesty tag.** Master §3.5 requires every
  new module to land with its true `ACTIVE | AVAILABLE | POSSIBLE` tag in `modules.py` "on the
  day they land, not after" — the exact pattern the matcher backends already use
  (`src/opendaisugi/modules.py:88-105`: `ACTIVE` when a backend is both selected and its
  package is present, `AVAILABLE` when installed but not chosen, `POSSIBLE` when the package is
  missing). This plan's nine tasks never touch `modules.py`, so `daisugi modules`/`daisugi
  dashboard` will keep reporting the voice bridge as if it does not exist even after Task 7
  ships `daisugi voice serve`. Fix: add a step (Task 7 is the natural place, since it already
  wires the CLI) that registers a `voice` module list mirroring the matcher-backend pattern —
  `faster-whisper` `ACTIVE` when `config.voice_engine == "faster-whisper"` and importable else
  `AVAILABLE`/`POSSIBLE` by package presence, `parakeet` `AVAILABLE` when `sherpa-onnx` is
  importable else `POSSIBLE` — with a test asserting the tag flips with `voice_engine` and
  package presence, the same way the existing matcher tests do.
- **NOTE — `/health` is unauthenticated even off-loopback.** Spec-07 says the bearer token is
  "required on every request when not on loopback," and `do_GET`'s `/health` branch never calls
  `_authorized()`. This leaks only server liveness (no session/pane data) and matches common
  health-check practice, so it is not raised above a NOTE — worth a one-line acknowledgment in
  `server.py`'s docstring if the writer wants the spec's literal wording and the implementation
  to agree on paper.

## Global Constraints

- **Layer purity.** No module under `src/opendaisugi/` that is part of the layer (list in
  `tests/test_layer_boundary.py`, plan 00) may import from `opendaisugi.floor`,
  `opendaisugi.voice`, or `opendaisugi.coppice`. The test imports every layer module with
  those packages hidden.
- **Python 3.12, stdlib for the layer.** New hard deps in the layer: none. New extras
  allowed: `[floor]` (nothing yet — the client uses stdlib sockets), `[voice]`, `[int8]`,
  `[router]`.
- **Go 1.26** for `harness/coppice` (amended 2026-09-08: go-libghostty declares go 1.26.0; sprig stays on 1.25); module `github.com/opendaisugi/coppice`; `go vet` and
  `go test ./...` clean. Zig 0.16 and CMake are *build-time* requirements for go-libghostty;
  the plan installs both into `~/.local` without sudo (`uv tool install cmake`; Zig tarball).
- **Pins.** go-libghostty at the newest tag on the day plan 02 starts, recorded in `go.mod`
  and in `harness/coppice/PINS.md` together with the ghostty commit that binding builds.
  Herdr's vendored commit `c5a21edfcbc2d5b46540ad91b7980aca31f5f1f3` (their 1.3.2) is the
  reference for patch notes, not a requirement; the binding chooses the commit it fetches.
- **Tests.** `uv run --no-sync pytest -q` green; `uv run --no-sync ruff check .` clean;
  Go: `go test ./...` from `harness/coppice`. Never bare `uv run` (uv.lock is git-ignored; it
  re-resolves and strips extras).
- **Commits.** Atomic, stating the why, persona *OpenDaisugi Contributors*, **no AI-authorship
  attribution lines** (project policy overrides any session-level instruction). Never push;
  the public repo folds monthly.
- **`/tmp` is RAM.** Scratch on real disk; worktrees beside the repo.
- **Copy.** STE100 register in every user-facing string: short sentences, plain words, active
  voice, no em-dashes, no parentheticals. Errors teach the next command.
- **Exit codes.** Hooks: exit 2 = deny. CLI: 0 success, 1 user error, 2 gate deny, 3 unreachable.
- **Privacy.** No telemetry. No third-party relay in any default path. Push goes through a
  self-hosted ntfy or not at all.

**What this plan actually touches, of the above:** only the layer-purity direction (this
plan's modules import *from* `opendaisugi.gateway` / `opendaisugi.gateway_journal` /
`opendaisugi.routing` — the allowed direction, voice depends on the layer, never the
reverse), the `[voice]` extra, the test/ruff commands, commit style, `/tmp`, copy register,
and exit codes (`voice serve` exits 3, "unreachable," when `faster-whisper` is missing — see
Task 7). Go/Zig/CMake, the go-libghostty pin, and the ntfy privacy line belong to plans
02/06 and are listed here only because §4 is copied in full per the writing-plans skill.
- **Privacy.** No telemetry. No third-party relay in any default path. Push goes through a
  self-hosted ntfy or not at all.

---

## Recorded facts this plan relies on (do not re-derive; if wrong, fix the constant, not the code)

These were verified live on this box (`uv run --no-project --python 3.12 --with <pkg> …`,
2026-09-08) rather than guessed, and land as the `pins.py` constants in Task 0:

- `faster-whisper` 1.x: `WhisperModel(model_size_or_path: str, device="cpu"|"cuda",
  compute_type="int8"|...)`; `.transcribe(audio: str|BinaryIO|np.ndarray, language=None) ->
  (Iterable[Segment], TranscriptionInfo)`; `Segment` has `.start .end .text`. Valid model
  names include `tiny.en`, `base.en`, … A bare `io.BytesIO` object is accepted directly (no
  temp file needed).
- Three espeak-ng-synthesized, ffmpeg-resampled-to-16kHz fixtures, transcribed with
  `tiny.en`/cpu/int8: `"the quick brown fox jumps over the lazy dog"` → WER 0.111 (`"jump"`
  for `"jumps"`); `"please remember to buy milk after work"` → WER 0.000; `"what is the
  weather like today"` → WER 0.000. Mean 0.037, well inside the spec's 0.15 ceiling.
  Deterministic across two runs (same text and WER both times).
- `sherpa-onnx` 1.13.7: `sherpa_onnx.OfflineRecognizer.from_transducer(encoder, decoder,
  joiner, tokens, model_type="transducer", ...)` — a NeMo Parakeet-TDT model needs
  `model_type="nemo_transducer"` (confirmed both from the k2-fsa docs' CLI example
  `--model-type=nemo_transducer` and from `inspect.signature` on the installed package,
  which accepts a `model_type` keyword). `recognizer.create_stream()` →
  `stream.accept_waveform(sample_rate, waveform_float32_neg1_to_1)` →
  `recognizer.decode_stream(stream)` → `stream.result.text`.
- The Parakeet-TDT-0.6B-v2 int8 archive is `sherpa-onnx-nemo-parakeet-tdt-0.6b-v2-int8.tar.bz2`
  at `github.com/k2-fsa/sherpa-onnx/releases/download/asr-models/…`, containing
  `encoder.int8.onnx`, `decoder.int8.onnx`, `joiner.int8.onnx`, `tokens.txt`. ~600 MB — never
  auto-downloaded (this box treats `/tmp` as RAM and the model is an opt-in engine).

## Decisions this plan makes where the spec leaves a gap (stated, not hidden)

1. **Cleanup always self-journals `task="voice.cleanup"`, and never routes through a running
   gateway process.** The spec's prose ("through the gateway's local endpoint when running,
   else directly to the local backend") would, on the gateway path, journal the *raw
   transcript* as the turn's task — the repo's own posture is that raw content only ever
   enters a store opt-in (`capture_answers`, off by default). Tagging every cleanup call with
   the literal string `"voice.cleanup"` keeps the transcript out of `turns.jsonl` entirely,
   and is *more* useful for distillation: every cleanup call collapses into one repeat
   cluster by construction, which is exactly the reuse signal the garden looks for.
2. **The cleanup transport is `litellm.completion()`**, not a new HTTP client. `litellm` is
   already a hard dependency of `opendaisugi` (pyproject.toml), and `tier1.py`'s
   `LiteLLMTier1Provider` already establishes the pattern (a local OpenAI-compatible endpoint
   via `base_url` + a bare or `openai/`-prefixed model string). No new dependency, no new
   wire format.
3. **`Config` gets flat, `voice_`-prefixed fields**, not a nested `VoiceConfig`. The file's own
   convention for a namespaced setting is a prefix (`gateway_local_model`, `matcher_model`,
   `gate_mode`), and `resolved_config()` prints one row per `Config.model_fields` member — a
   nested model would print an unreadable blob and break "every setting, with its source."
4. **The bearer-token file path is a plan-07 constant, not a fact read from spec-06** (which
   does not itself pin one). `TOKEN_PATH = ~/.opendaisugi/coppice/web-token` is stated in
   Task 5 as a cross-plan assumption, overridable with `--token-file`, and every test in this
   plan passes an explicit path so the suite never depends on the guess being right.
5. **Audio normalization prefers `ffmpeg`** (present on this box; a correct, anti-aliased
   resampler and a webm/opus decoder in one) **and falls back to `av`** (PyAV, `[voice]`
   extra) **for webm decode only** when `ffmpeg` is absent. A bare-numpy linear-interpolation
   resample is the last-resort path for a WAV at the wrong sample rate with neither tool
   present — measurably worse (WER rose from 0.111 to 0.222 on the "quick brown fox" fixture
   in a live check) but never silently wrong, and documented as degraded in `audio.py`.
6. **The PWA record button ships as a standalone `record.js`** with its own Playwright test
   harness page, not a patch to spec-06's `app.js`/`index.html` (which do not exist in this
   repo yet). The file documents its one-line integration point in a header comment.

---

### Task 0: Prerequisites, pinned facts, and the extras that gate everything else

The spec's own header says: "Prerequisite (plan task 0): a box with a GPU that Python can
see, or acceptance of CPU speed." CPU always works; the only hard stop is `faster-whisper`
itself not being installed. This task also records the facts above as code (never re-guessed
by a later task) and adds the `pyproject.toml` extras every later task needs.

**Files:**
- Modify: `pyproject.toml` (add `[voice]`, `[voice-parakeet]` extras; add `opendaisugi[voice]`
  to `dev`; register the `voice_live` pytest marker)
- Create: `src/opendaisugi/voice/__init__.py`
- Create: `src/opendaisugi/voice/prereq.py`
- Create: `src/opendaisugi/voice/pins.py`
- Create: `tests/voice/__init__.py`
- Create: `tests/voice/conftest.py`
- Test: `tests/voice/test_prereq.py`
- Test: `tests/voice/test_pins.py`

**Interfaces:**
- Produces (consumed by every later task):
  - `opendaisugi.voice.prereq.PrereqResult` — `faster_whisper_available: bool`,
    `ffmpeg_available: bool`, `espeak_available: bool`, property `ok: bool`.
  - `opendaisugi.voice.prereq.check_voice_prereqs() -> PrereqResult`
  - `opendaisugi.voice.pins.FASTER_WHISPER_MODEL_NAMES: frozenset[str]`
  - `opendaisugi.voice.pins.FASTER_WHISPER_TEST_MODEL: str` (`"tiny.en"`)
  - `opendaisugi.voice.pins.FIXTURE_TRANSCRIPTS: dict[str, str]` (filename → expected text)
  - `opendaisugi.voice.pins.FIXTURE_MAX_WER: dict[str, float]` (per-filename ceiling)
  - `opendaisugi.voice.pins.FIXTURE_MEAN_MAX_WER: float`
  - `opendaisugi.voice.pins.PARAKEET_MODEL_ARCHIVE: str`, `PARAKEET_MODEL_URL: str`,
    `PARAKEET_FILES: tuple[str, ...]`, `PARAKEET_MODEL_TYPE: str` (`"nemo_transducer"`)
  - `tests.voice.conftest.requires_faster_whisper` — a `pytest.mark.skipif` decorator object.

- [ ] **Step 1: Add the `[voice]` / `[voice-parakeet]` extras and the `voice_live` marker**

Edit `pyproject.toml`. Find the `potion = [...]` block (currently `pyproject.toml:57-60`,
right after its explanatory comment starting `# v0.45: the torch-free pathway-reuse
embedder`) and insert a new `voice` block immediately after it, then a `voice-parakeet`
block right after the following `mcp = [...]` block (`pyproject.toml:61-63`):

```toml
# v0.45: the voice bridge (spec-07) — faster-whisper is CTranslate2 (no torch), so it
# runs on a CPU-only box or one whose GPU can't safely take torch workloads. `av`
# (PyAV) is the webm/opus decode fallback when the `ffmpeg` binary is absent; a WAV
# already at 16 kHz mono, or with ffmpeg present, never touches it. See ADR-0019's
# torch-free precedent — same reasoning, a different codec.
voice = [
    "faster-whisper>=1.1",
    "sounddevice>=0.5",
    "numpy>=1.26",
    "av>=13.0",
]
# v0.45: the opt-in second engine (NeMo Parakeet-TDT via sherpa-onnx). Its model is a
# ~600MB download the voice bridge never fetches automatically; see pins.py.
voice-parakeet = [
    "sherpa-onnx>=1.10",
]
```

In the `dev` list, add `"opendaisugi[voice]"` (not `voice-parakeet` — that one stays
opt-in even for contributors, matching the model-download rule):

```toml
dev = [
    "pytest>=8.0",
    "pytest-cov>=4.1",
    "pytest-asyncio>=0.23",
    "ruff>=0.14",
    "build>=1.2",
    "twine>=5.0",
    "opendaisugi[search]",
    "opendaisugi[mcp]",
    "opendaisugi[shell]",
    "opendaisugi[sign]",
    "opendaisugi[gateway]",
    "opendaisugi[tui]",
    "opendaisugi[voice]",
]
```

In `[tool.pytest.ini_options] markers`, add:

```toml
    "voice_live: transcribes with a real speech-to-text engine; requires DAISUGI_VOICE_LIVE=1",
```

- [ ] **Step 2: Write the failing prereq test**

```python
# tests/voice/test_prereq.py
from __future__ import annotations

from opendaisugi.voice.prereq import PrereqResult, check_voice_prereqs


def test_check_voice_prereqs_returns_a_result_with_the_three_flags():
    result = check_voice_prereqs()
    assert isinstance(result, PrereqResult)
    assert isinstance(result.faster_whisper_available, bool)
    assert isinstance(result.ffmpeg_available, bool)
    assert isinstance(result.espeak_available, bool)


def test_ok_is_true_only_when_faster_whisper_is_available():
    assert PrereqResult(
        faster_whisper_available=True, ffmpeg_available=False, espeak_available=False
    ).ok
    assert not PrereqResult(
        faster_whisper_available=False, ffmpeg_available=True, espeak_available=True
    ).ok
```

- [ ] **Step 3: Run it to confirm it fails**

Run: `uv run --no-sync pytest tests/voice/test_prereq.py -q`
Expected: FAIL — `ModuleNotFoundError: No module named 'opendaisugi.voice'`

- [ ] **Step 4: Write `src/opendaisugi/voice/__init__.py` and `prereq.py`**

```python
# src/opendaisugi/voice/__init__.py
"""The voice bridge (spec-07): record anywhere, transcribe on this box, land the
text in a pane. See docs/plans/2026-09-08-workshop/spec-07-voice.md."""
```

```python
# src/opendaisugi/voice/prereq.py
"""Host-capability check for the voice bridge.

Only `faster-whisper` is a hard requirement — everything else (ffmpeg, espeak-ng) makes a
feature better (real anti-aliased resampling; a real-speech test fixture) rather than being
load-bearing. `check_voice_prereqs()` never raises; the caller (the CLI) decides what to do
with a missing flag."""

from __future__ import annotations

import importlib
import shutil
from dataclasses import dataclass


def _importable(name: str) -> bool:
    try:
        importlib.import_module(name)
    except ImportError:
        return False
    return True


@dataclass(frozen=True)
class PrereqResult:
    faster_whisper_available: bool
    ffmpeg_available: bool
    espeak_available: bool

    @property
    def ok(self) -> bool:
        """True only when the hard requirement (a working speech engine) is met."""
        return self.faster_whisper_available


def check_voice_prereqs() -> PrereqResult:
    return PrereqResult(
        faster_whisper_available=_importable("faster_whisper"),
        ffmpeg_available=shutil.which("ffmpeg") is not None,
        espeak_available=(shutil.which("espeak-ng") or shutil.which("espeak")) is not None,
    )
```

- [ ] **Step 5: Run it to confirm it passes**

Run: `uv run --no-sync pytest tests/voice/test_prereq.py -q`
Expected: PASS

- [ ] **Step 6: Write the failing pins test**

```python
# tests/voice/test_pins.py
from __future__ import annotations

from opendaisugi.voice import pins


def test_faster_whisper_test_model_is_a_recognized_name():
    assert pins.FASTER_WHISPER_TEST_MODEL in pins.FASTER_WHISPER_MODEL_NAMES


def test_parakeet_url_matches_the_archive_name():
    assert pins.PARAKEET_MODEL_URL.endswith(pins.PARAKEET_MODEL_ARCHIVE + ".tar.bz2")


def test_fixture_ceilings_cover_every_fixture_and_stay_above_the_recorded_value():
    assert set(pins.FIXTURE_MAX_WER) == set(pins.FIXTURE_TRANSCRIPTS)
    for name, ceiling in pins.FIXTURE_MAX_WER.items():
        assert 0.0 < ceiling <= 0.5, f"{name}: {ceiling} is not a sane per-fixture ceiling"
    assert 0.0 < pins.FIXTURE_MEAN_MAX_WER <= 0.5


def test_parakeet_model_type_is_the_nemo_variant():
    # Confirmed live: sherpa_onnx.OfflineRecognizer.from_transducer(model_type=...)
    # needs "nemo_transducer" for a NeMo Parakeet-TDT export — plain "transducer"
    # (the parameter's own default) is for icefall/k2-style exports and will not
    # decode a Parakeet model's duration head correctly.
    assert pins.PARAKEET_MODEL_TYPE == "nemo_transducer"
```

- [ ] **Step 7: Run it to confirm it fails**

Run: `uv run --no-sync pytest tests/voice/test_pins.py -q`
Expected: FAIL — `ModuleNotFoundError: No module named 'opendaisugi.voice.pins'`

- [ ] **Step 8: Write `pins.py`**

```python
# src/opendaisugi/voice/pins.py
"""Pinned upstream facts for the voice engines — recorded once, read everywhere else.

Recorded 2026-09-08. faster-whisper facts: github.com/SYSTRAN/faster-whisper's README, plus
a live probe on this box (`uv run --no-project --python 3.12 --with faster-whisper --with
numpy python …`) that generated three espeak-ng fixtures, resampled them to 16 kHz mono with
ffmpeg, and transcribed them with tiny.en/cpu/int8 — the WER numbers below are exactly what
that run produced, not an estimate. sherpa-onnx facts: k2-fsa.github.io/sherpa/onnx/
pretrained_models/offline-transducer/nemo-transducer-models.html (the Parakeet-TDT-0.6B-v2
CLI example) plus `inspect.signature(sherpa_onnx.OfflineRecognizer.from_transducer)` on
sherpa-onnx 1.13.7, confirming the `model_type` keyword this repo's engine needs.

Re-verify before bumping any of these — a later faster-whisper/ctranslate2 release changing
tiny.en's decode could raise the observed WER without necessarily being a regression.
"""

from __future__ import annotations

FASTER_WHISPER_MODEL_NAMES = frozenset(
    {
        "tiny",
        "tiny.en",
        "base",
        "base.en",
        "small",
        "small.en",
        "medium",
        "medium.en",
        "large-v2",
        "large-v3",
        "distil-large-v3",
        "turbo",
    }
)
FASTER_WHISPER_TEST_MODEL = "tiny.en"  # ~78MB, HF repo Systran/faster-whisper-tiny.en

# Recorded from a real run, tiny.en/cpu/int8, espeak-ng 1.51.1 synthesis resampled to
# 16 kHz mono with ffmpeg. Per-fixture ceilings carry headroom over the observed value
# (0.111 / 0.000 / 0.000) for cross-machine/cross-version variance; the mean ceiling is
# the number spec-07 states (0.15) and the observed mean (0.037) clears it easily.
FIXTURE_TRANSCRIPTS = {
    "fixture_a.wav": "the quick brown fox jumps over the lazy dog",
    "fixture_b.wav": "please remember to buy milk after work",
    "fixture_c.wav": "what is the weather like today",
}
FIXTURE_MAX_WER = {
    "fixture_a.wav": 0.20,  # observed 0.111 ("jump" for "jumps")
    "fixture_b.wav": 0.10,  # observed 0.000
    "fixture_c.wav": 0.10,  # observed 0.000
}
FIXTURE_MEAN_MAX_WER = 0.15  # the spec's stated ceiling; observed mean 0.037

PARAKEET_MODEL_ARCHIVE = "sherpa-onnx-nemo-parakeet-tdt-0.6b-v2-int8"
PARAKEET_MODEL_URL = (
    "https://github.com/k2-fsa/sherpa-onnx/releases/download/asr-models/"
    "sherpa-onnx-nemo-parakeet-tdt-0.6b-v2-int8.tar.bz2"
)
PARAKEET_FILES = ("encoder.int8.onnx", "decoder.int8.onnx", "joiner.int8.onnx", "tokens.txt")
PARAKEET_MODEL_TYPE = "nemo_transducer"
```

- [ ] **Step 9: Run it to confirm it passes**

Run: `uv run --no-sync pytest tests/voice/test_pins.py -q`
Expected: PASS

- [ ] **Step 10: Write `tests/voice/__init__.py` and `conftest.py`**

```python
# tests/voice/__init__.py
```

```python
# tests/voice/conftest.py
"""Shared fixtures/markers for the voice test suite."""

from __future__ import annotations

import pytest

from opendaisugi.voice.prereq import check_voice_prereqs

requires_faster_whisper = pytest.mark.skipif(
    not check_voice_prereqs().faster_whisper_available,
    reason="faster-whisper not installed — pip install 'opendaisugi[voice]'",
)
```

- [ ] **Step 11: Run the whole voice suite and ruff, then commit**

Run: `uv run --no-sync pytest tests/voice/ -q && uv run --no-sync ruff check src/opendaisugi/voice pyproject.toml`
Expected: PASS, clean

```bash
git add pyproject.toml src/opendaisugi/voice/__init__.py src/opendaisugi/voice/prereq.py \
        src/opendaisugi/voice/pins.py tests/voice/__init__.py tests/voice/conftest.py \
        tests/voice/test_prereq.py tests/voice/test_pins.py
git commit -m "voice: prerequisite check, pinned upstream facts, and the [voice] extra

Records faster-whisper's real API shape and observed WER, and sherpa-onnx's
Parakeet-TDT construction, from live probes rather than guesses — later voice
tasks read these constants instead of re-deriving them."
```

---

### Task 1: Audio normalization and deterministic speech fixtures

**Files:**
- Create: `src/opendaisugi/voice/audio.py`
- Create: `tests/voice/fixtures.py`
- Test: `tests/voice/test_audio.py`
- Test: `tests/voice/test_fixtures.py`

**Interfaces:**
- Consumes: nothing from Task 0 except `pins.FIXTURE_TRANSCRIPTS` (fixtures.py only).
- Produces:
  - `opendaisugi.voice.audio.AudioFormatUnsupported(RuntimeError)`
  - `opendaisugi.voice.audio.to_wav_16k_mono(raw: bytes, content_type: str = "") -> bytes`
  - `opendaisugi.voice.audio.wav_duration_s(wav_bytes: bytes) -> float`
  - `opendaisugi.voice.audio.write_wav_16k_mono(samples: "numpy.ndarray") -> bytes`
  - `tests.voice.fixtures.has_speech_synthesis() -> bool`
  - `tests.voice.fixtures.generate_fixtures(out_dir: Path) -> dict[str, str | None]`
    (value is `None` when the tone fallback ran — no real speech, callers must not score WER)

- [ ] **Step 1: Write the failing audio tests**

```python
# tests/voice/test_audio.py
from __future__ import annotations

import io
import shutil
import subprocess
import wave

import numpy as np
import pytest

from opendaisugi.voice.audio import (
    AudioFormatUnsupported,
    to_wav_16k_mono,
    wav_duration_s,
    write_wav_16k_mono,
)


def _make_wav(*, sr: int, n_samples: int, channels: int = 1) -> bytes:
    t = np.arange(n_samples) / sr
    tone = (np.sin(2 * np.pi * 440 * t) * 10000).astype(np.int16)
    if channels == 2:
        tone = np.repeat(tone, 2)
    buf = io.BytesIO()
    with wave.open(buf, "wb") as w:
        w.setnchannels(channels)
        w.setsampwidth(2)
        w.setframerate(sr)
        w.writeframes(tone.tobytes())
    return buf.getvalue()


def test_wav_duration_s_matches_frame_count_over_sample_rate():
    wav_bytes = _make_wav(sr=16000, n_samples=32000)
    assert wav_duration_s(wav_bytes) == pytest.approx(2.0, abs=1e-3)


def test_write_wav_16k_mono_roundtrips_through_wav_duration_s():
    samples = np.zeros(16000, dtype=np.float64)
    wav_bytes = write_wav_16k_mono(samples)
    assert wav_bytes[:4] == b"RIFF"
    assert wav_duration_s(wav_bytes) == pytest.approx(1.0, abs=1e-3)


def test_to_wav_16k_mono_passes_through_a_wav_already_at_16k_mono():
    wav_bytes = _make_wav(sr=16000, n_samples=16000)
    out = to_wav_16k_mono(wav_bytes, "audio/wav")
    assert wav_duration_s(out) == pytest.approx(1.0, abs=1e-3)
    with wave.open(io.BytesIO(out), "rb") as w:
        assert w.getframerate() == 16000
        assert w.getnchannels() == 1


@pytest.mark.skipif(shutil.which("ffmpeg") is None, reason="ffmpeg not on PATH")
def test_to_wav_16k_mono_resamples_a_22050hz_wav_via_ffmpeg():
    wav_bytes = _make_wav(sr=22050, n_samples=22050 * 2, channels=2)
    out = to_wav_16k_mono(wav_bytes, "audio/wav")
    with wave.open(io.BytesIO(out), "rb") as w:
        assert w.getframerate() == 16000
        assert w.getnchannels() == 1
    assert wav_duration_s(out) == pytest.approx(2.0, abs=0.05)


def test_to_wav_16k_mono_falls_back_to_numpy_resample_without_ffmpeg(monkeypatch):
    monkeypatch.setattr("opendaisugi.voice.audio.shutil.which", lambda name: None)
    wav_bytes = _make_wav(sr=8000, n_samples=8000)
    out = to_wav_16k_mono(wav_bytes, "audio/wav")
    with wave.open(io.BytesIO(out), "rb") as w:
        assert w.getframerate() == 16000
    assert wav_duration_s(out) == pytest.approx(1.0, abs=1e-3)


def test_to_wav_16k_mono_rejects_a_non_16_bit_wav():
    buf = io.BytesIO()
    with wave.open(buf, "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(1)  # 8-bit — unsupported
        w.setframerate(16000)
        w.writeframes(b"\x00" * 16000)
    with pytest.raises(AudioFormatUnsupported):
        to_wav_16k_mono(buf.getvalue(), "audio/wav")


@pytest.mark.skipif(shutil.which("ffmpeg") is None, reason="ffmpeg not on PATH")
def test_to_wav_16k_mono_decodes_webm_via_ffmpeg():
    wav_bytes = _make_wav(sr=16000, n_samples=16000)
    proc = subprocess.run(
        [
            "ffmpeg",
            "-y",
            "-hide_banner",
            "-loglevel",
            "error",
            "-i",
            "pipe:0",
            "-f",
            "webm",
            "pipe:1",
        ],
        input=wav_bytes,
        capture_output=True,
        timeout=15,
    )
    assert proc.returncode == 0, proc.stderr
    out = to_wav_16k_mono(proc.stdout, "audio/webm")
    assert wav_duration_s(out) == pytest.approx(1.0, abs=0.1)


def test_to_wav_16k_mono_raises_a_teaching_error_for_webm_with_no_decoder(monkeypatch):
    monkeypatch.setattr("opendaisugi.voice.audio.shutil.which", lambda name: None)
    with pytest.raises(AudioFormatUnsupported, match="ffmpeg"):
        to_wav_16k_mono(b"not a real webm file", "audio/webm")
```

- [ ] **Step 2: Run it to confirm it fails**

Run: `uv run --no-sync pytest tests/voice/test_audio.py -q`
Expected: FAIL — `ModuleNotFoundError: No module named 'opendaisugi.voice.audio'`

- [ ] **Step 3: Write `audio.py`**

```python
# src/opendaisugi/voice/audio.py
"""Audio normalization: any uploaded or recorded clip → 16 kHz mono 16-bit PCM WAV bytes.

`ffmpeg` (when on PATH) is the primary path for both webm/opus decode and resampling — it is
a correct, anti-aliased resampler and needs no Python decoder library. `av` (PyAV, the
`[voice]` extra's bundled ffmpeg libraries) is the fallback for webm decode when the `ffmpeg`
binary itself is absent. A WAV already at 16 kHz mono needs no conversion at all. A WAV at
another rate with neither ffmpeg nor av available falls back to a plain numpy
linear-interpolation resample — audibly worse (no anti-aliasing; a live check on this box
raised one fixture's WER from 0.111 to 0.222) but never silently wrong, and never the primary
path: `ffmpeg` is common enough that this only matters on a stripped-down host.
"""

from __future__ import annotations

import io
import shutil
import subprocess
import wave

import numpy as np


class AudioFormatUnsupported(RuntimeError):
    """The clip cannot be normalized on this host — the message names the fix."""


def wav_duration_s(wav_bytes: bytes) -> float:
    with wave.open(io.BytesIO(wav_bytes), "rb") as w:
        return w.getnframes() / float(w.getframerate())


def write_wav_16k_mono(samples: np.ndarray, *, sample_rate: int = 16000) -> bytes:
    """Pack float or int samples into 16 kHz mono 16-bit PCM WAV bytes."""
    buf = io.BytesIO()
    with wave.open(buf, "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(sample_rate)
        clipped = np.clip(samples, -32768, 32767).astype(np.int16)
        w.writeframes(clipped.tobytes())
    return buf.getvalue()


def _is_wav(raw: bytes) -> bool:
    return len(raw) >= 12 and raw[:4] == b"RIFF" and raw[8:12] == b"WAVE"


def _read_wav_mono_samples(raw: bytes) -> tuple[int, np.ndarray]:
    with wave.open(io.BytesIO(raw), "rb") as w:
        sr = w.getframerate()
        channels = w.getnchannels()
        sampwidth = w.getsampwidth()
        frames = w.readframes(w.getnframes())
    if sampwidth != 2:
        raise AudioFormatUnsupported(
            f"unsupported sample width ({sampwidth * 8}-bit) — only 16-bit PCM WAV is supported"
        )
    samples = np.frombuffer(frames, dtype=np.int16).astype(np.float64)
    if channels > 1:
        samples = samples.reshape(-1, channels).mean(axis=1)
    return sr, samples


def _resample_linear(samples: np.ndarray, orig_sr: int, target_sr: int) -> np.ndarray:
    if orig_sr == target_sr:
        return samples
    duration = len(samples) / orig_sr
    n_target = max(1, int(round(duration * target_sr)))
    orig_t = np.arange(len(samples)) / orig_sr
    target_t = np.arange(n_target) / target_sr
    return np.interp(target_t, orig_t, samples)


def _ffmpeg_to_wav_16k_mono(raw: bytes) -> bytes:
    proc = subprocess.run(
        [
            "ffmpeg",
            "-hide_banner",
            "-loglevel",
            "error",
            "-i",
            "pipe:0",
            "-ar",
            "16000",
            "-ac",
            "1",
            "-f",
            "wav",
            "pipe:1",
        ],
        input=raw,
        capture_output=True,
        timeout=30,
    )
    if proc.returncode != 0:
        raise AudioFormatUnsupported(
            f"ffmpeg could not decode this clip: {proc.stderr.decode(errors='replace')[:200]}"
        )
    return proc.stdout


def _av_to_wav_16k_mono(raw: bytes) -> bytes:
    try:
        import av
    except ImportError as exc:
        raise AudioFormatUnsupported(
            "cannot decode this audio format: neither the ffmpeg binary nor the 'av' "
            "package is available. Install ffmpeg, or run: pip install 'opendaisugi[voice]'"
        ) from exc
    container = av.open(io.BytesIO(raw))
    resampler = av.AudioResampler(format="s16", layout="mono", rate=16000)
    chunks: list[np.ndarray] = []
    for frame in container.decode(audio=0):
        for out_frame in resampler.resample(frame):
            chunks.append(out_frame.to_ndarray().flatten())
    container.close()
    merged = np.concatenate(chunks).astype(np.float64) if chunks else np.zeros(0)
    return write_wav_16k_mono(merged)


def to_wav_16k_mono(raw: bytes, content_type: str = "") -> bytes:
    """Normalize any supported audio clip to 16 kHz mono 16-bit PCM WAV bytes."""
    if _is_wav(raw):
        sr, samples = _read_wav_mono_samples(raw)
        if sr == 16000:
            return write_wav_16k_mono(samples)
        if shutil.which("ffmpeg"):
            return _ffmpeg_to_wav_16k_mono(raw)
        return write_wav_16k_mono(_resample_linear(samples, sr, 16000))
    if shutil.which("ffmpeg"):
        return _ffmpeg_to_wav_16k_mono(raw)
    return _av_to_wav_16k_mono(raw)
```

- [ ] **Step 4: Run the audio tests to confirm they pass**

Run: `uv run --no-sync pytest tests/voice/test_audio.py -q`
Expected: PASS (the two `ffmpeg`-gated tests skip only if `ffmpeg` is truly absent — it is
present on this box, so they run)

- [ ] **Step 5: Write the failing fixtures tests**

```python
# tests/voice/test_fixtures.py
from __future__ import annotations

import wave

from tests.voice.fixtures import generate_fixtures, has_speech_synthesis
from opendaisugi.voice import pins


def test_generate_fixtures_writes_three_16k_mono_wavs(tmp_path):
    transcripts = generate_fixtures(tmp_path)
    assert set(transcripts) == set(pins.FIXTURE_TRANSCRIPTS)
    for name in pins.FIXTURE_TRANSCRIPTS:
        path = tmp_path / name
        assert path.exists()
        with wave.open(str(path), "rb") as w:
            assert w.getframerate() == 16000
            assert w.getnchannels() == 1
            assert w.getnframes() > 0


def test_generate_fixtures_returns_real_transcripts_when_speech_synthesis_available(tmp_path):
    if not has_speech_synthesis():
        import pytest

        pytest.skip("espeak-ng and/or ffmpeg not on PATH")
    transcripts = generate_fixtures(tmp_path)
    assert transcripts == pins.FIXTURE_TRANSCRIPTS


def test_generate_fixtures_returns_none_transcripts_on_the_tone_fallback(tmp_path, monkeypatch):
    monkeypatch.setattr("tests.voice.fixtures.has_speech_synthesis", lambda: False)
    transcripts = generate_fixtures(tmp_path)
    assert all(v is None for v in transcripts.values())
    assert set(transcripts) == set(pins.FIXTURE_TRANSCRIPTS)
```

- [ ] **Step 6: Run it to confirm it fails**

Run: `uv run --no-sync pytest tests/voice/test_fixtures.py -q`
Expected: FAIL — `ModuleNotFoundError: No module named 'tests.voice.fixtures'`

- [ ] **Step 7: Write `fixtures.py`**

```python
# tests/voice/fixtures.py
"""Deterministic speech fixtures for the voice engine tests.

espeak-ng (formant synthesis, offline, no model download) is the TTS this repo can rely on
for a real-speech fixture without a network call. When espeak-ng (or espeak) AND ffmpeg are
both on PATH, `generate_fixtures()` synthesizes three short clips with known transcripts and
resamples them through `opendaisugi.voice.audio.to_wav_16k_mono` — the exact path production
code uses. When either tool is missing, it falls back to three numpy sine-wave tones: NOT
speech. Callers MUST treat a `None` transcript as "no WER assertion possible here, skip with
reason" — scoring a WER against a tone would be a meaningless number, worse than skipping.
"""

from __future__ import annotations

import shutil
import subprocess
import wave
from pathlib import Path

import numpy as np

from opendaisugi.voice import pins
from opendaisugi.voice.audio import to_wav_16k_mono


def _espeak_binary() -> str | None:
    return shutil.which("espeak-ng") or shutil.which("espeak")


def has_speech_synthesis() -> bool:
    """True only when a real-speech fixture can be produced: a TTS binary AND ffmpeg
    (ffmpeg is required here, not merely preferred — see audio.py's module docstring on
    why the numpy fallback is measurably worse; fixtures should never use it)."""
    return _espeak_binary() is not None and shutil.which("ffmpeg") is not None


def generate_fixtures(out_dir: Path) -> dict[str, str | None]:
    """Write the three fixtures.FIXTURE_TRANSCRIPTS filenames into out_dir as 16 kHz mono
    WAVs. Returns {filename: expected_transcript}, or {filename: None} for every entry when
    the tone fallback ran."""
    out_dir.mkdir(parents=True, exist_ok=True)
    if has_speech_synthesis():
        return _generate_speech_fixtures(out_dir)
    return _generate_tone_fixtures(out_dir)


def _generate_speech_fixtures(out_dir: Path) -> dict[str, str | None]:
    binary = _espeak_binary()
    assert binary is not None  # has_speech_synthesis() already checked this
    result: dict[str, str | None] = {}
    for name, text in pins.FIXTURE_TRANSCRIPTS.items():
        raw_path = out_dir / f"_raw_{name}"
        subprocess.run(
            [binary, "-w", str(raw_path), text], check=True, capture_output=True, timeout=10
        )
        wav_bytes = to_wav_16k_mono(raw_path.read_bytes(), "audio/wav")
        (out_dir / name).write_bytes(wav_bytes)
        raw_path.unlink(missing_ok=True)
        result[name] = text
    return result


def _generate_tone_fixtures(out_dir: Path) -> dict[str, str | None]:
    result: dict[str, str | None] = {}
    for i, name in enumerate(pins.FIXTURE_TRANSCRIPTS, start=1):
        freq = 220.0 * i
        sr = 16000
        duration_s = 2.0
        t = np.arange(int(sr * duration_s)) / sr
        samples = (np.sin(2 * np.pi * freq * t) * 10000).astype(np.int16)
        with wave.open(str(out_dir / name), "wb") as w:
            w.setnchannels(1)
            w.setsampwidth(2)
            w.setframerate(sr)
            w.writeframes(samples.tobytes())
        result[name] = None
    return result
```

- [ ] **Step 8: Run the fixtures tests to confirm they pass**

Run: `uv run --no-sync pytest tests/voice/test_fixtures.py -q`
Expected: PASS

- [ ] **Step 9: Run the whole voice suite and ruff, then commit**

Run: `uv run --no-sync pytest tests/voice/ -q && uv run --no-sync ruff check src/opendaisugi/voice tests/voice`
Expected: PASS, clean

```bash
git add src/opendaisugi/voice/audio.py tests/voice/fixtures.py tests/voice/test_audio.py \
        tests/voice/test_fixtures.py
git commit -m "voice: audio normalization (ffmpeg/av/numpy) and deterministic speech fixtures

Fixtures dogfood the same to_wav_16k_mono() production code uses, and fall
back to non-speech tones (never a fabricated transcript) when espeak-ng or
ffmpeg is missing."
```

---

### Task 2: `Config.voice_*` fields, the `Engine` protocol, and both engines

**Files:**
- Modify: `src/opendaisugi/config.py:96` (insert new fields after `pathway_store_backend`)
- Create: `src/opendaisugi/voice/engines.py`
- Test: `tests/voice/test_engines.py`
- Test: `tests/test_config.py` (append; find this file first — it holds `Config` round-trip
  tests already)

**Interfaces:**
- Consumes: `pins.FASTER_WHISPER_TEST_MODEL`, `pins.PARAKEET_FILES`, `pins.PARAKEET_MODEL_TYPE`
  (Task 0); `to_wav_16k_mono`/`wav_duration_s` (Task 1, used inside `transcribe()`);
  `tests.voice.fixtures.generate_fixtures`/`has_speech_synthesis` (Task 1, WER test only).
- Produces (consumed by Task 3's `cleanup.py` uses `Config` only; Task 5's `server.py` uses
  all of the below):
  - `opendaisugi.voice.engines.Transcript` — `text: str`, `segments: list[dict]`,
    `duration_s: float`, `rtf: float`
  - `opendaisugi.voice.engines.Engine` (Protocol) — `name: str`,
    `transcribe(self, wav_16k_mono: bytes, *, language: str | None = None) -> Transcript`
  - `opendaisugi.voice.engines.EngineUnavailable(RuntimeError)`
  - `opendaisugi.voice.engines.FasterWhisperEngine(model: str, *, device: str, compute_type: str)`
  - `opendaisugi.voice.engines.ParakeetEngine(model_dir: Path)`
  - `opendaisugi.voice.engines.pick_engine(config: Config) -> Engine`
  - `Config.voice_engine: str`, `Config.voice_model: str`, `Config.voice_device: str`,
    `Config.voice_compute_type: str`, `Config.voice_cleanup: bool`,
    `Config.voice_cleanup_model: str | None`, `Config.voice_cleanup_base_url: str | None`,
    `Config.voice_server_url: str`

- [ ] **Step 1: Add the `voice_*` fields to `Config`**

In `src/opendaisugi/config.py`, after line 96 (`pathway_store_backend: str = "sqlite"  #
sqlite | git`) and before the two blank lines that precede `def default_config()`, insert:

```python
# Voice bridge (spec-07). voice_device stays "cpu" until the operator opts in:
# pick_engine() only actually uses CUDA when this says "cuda" AND ctranslate2
# reports a visible device — this box's GPU has crashed other torch-based
# inference under CUDA before (ADR-0019), so CUDA is opt-in-and-verified, never
# auto-detected-and-switched-to.
voice_engine: str = "faster-whisper"  # faster-whisper | parakeet
voice_model: str = "tiny.en"  # a faster-whisper model id, or (parakeet) a local model dir
voice_device: str = "cpu"  # cpu | cuda
voice_compute_type: str = "int8"
# The optional cleanup pass (spec-07's cruxes): off by default, and it never
# touches a paid model unless voice_cleanup_model names one.
voice_cleanup: bool = False
voice_cleanup_model: str | None = None
voice_cleanup_base_url: str | None = None
voice_server_url: str = "http://127.0.0.1:7477"
```

- [ ] **Step 2: Write the failing config test**

Find `tests/test_config.py` and append:

```python
def test_voice_defaults_are_cpu_safe_and_cleanup_is_off():
    config = default_config()
    assert config.voice_engine == "faster-whisper"
    assert config.voice_device == "cpu"
    assert config.voice_cleanup is False
    assert config.voice_cleanup_model is None
    assert config.voice_server_url == "http://127.0.0.1:7477"


def test_voice_model_default_matches_the_pinned_faster_whisper_test_model():
    from opendaisugi.voice import pins

    assert default_config().voice_model == pins.FASTER_WHISPER_TEST_MODEL


def test_voice_config_roundtrips_through_yaml(tmp_path):
    path = tmp_path / "config.yaml"
    config = default_config().model_copy(
        update={"voice_cleanup": True, "voice_cleanup_model": "ollama/llama3.2:3b"}
    )
    save_config(config, path)
    loaded = load_config(path)
    assert loaded.voice_cleanup is True
    assert loaded.voice_cleanup_model == "ollama/llama3.2:3b"
```

(If `tests/test_config.py` does not already import `default_config`, `save_config`,
`load_config` at module scope, add them to its existing `from opendaisugi.config import …`
line rather than re-importing locally — match the file's existing style.)

- [ ] **Step 3: Run it to confirm it fails**

Run: `uv run --no-sync pytest tests/test_config.py -k voice -q`
Expected: FAIL — `AttributeError: 'Config' object has no attribute 'voice_engine'` (and the
`pins` import will also fail until Step 1 above and Task 0 are both in place — Task 0 is a
prior task, so only the `Config` fields are new here)

- [ ] **Step 4: Run it to confirm it passes**

Run: `uv run --no-sync pytest tests/test_config.py -k voice -q`
Expected: PASS (the `Config` fields from Step 1 are enough)

- [ ] **Step 5: Write the failing engines tests**

```python
# tests/voice/test_engines.py
from __future__ import annotations

import sys
import types
from pathlib import Path

import pytest

from opendaisugi.config import Config
from opendaisugi.voice import engines, pins
from tests.voice.conftest import requires_faster_whisper
from tests.voice.fixtures import generate_fixtures, has_speech_synthesis


def _word_error_rate(ref: str, hyp: str) -> float:
    def norm(s: str) -> list[str]:
        return "".join(c.lower() if c.isalnum() or c.isspace() else " " for c in s).split()

    r, h = norm(ref), norm(hyp)
    d = [[0] * (len(h) + 1) for _ in range(len(r) + 1)]
    for i in range(len(r) + 1):
        d[i][0] = i
    for j in range(len(h) + 1):
        d[0][j] = j
    for i in range(1, len(r) + 1):
        for j in range(1, len(h) + 1):
            cost = 0 if r[i - 1] == h[j - 1] else 1
            d[i][j] = min(d[i - 1][j] + 1, d[i][j - 1] + 1, d[i - 1][j - 1] + cost)
    return d[len(r)][len(h)] / max(1, len(r))


@requires_faster_whisper
def test_pick_engine_defaults_to_faster_whisper_on_cpu(monkeypatch):
    captured = {}

    class FakeWhisperModel:
        def __init__(self, model, device, compute_type):
            captured["model"] = model
            captured["device"] = device
            captured["compute_type"] = compute_type

    monkeypatch.setattr("faster_whisper.WhisperModel", FakeWhisperModel, raising=False)
    engine = engines.pick_engine(Config())
    assert isinstance(engine, engines.FasterWhisperEngine)
    assert captured["device"] == "cpu"
    assert captured["model"] == pins.FASTER_WHISPER_TEST_MODEL


@requires_faster_whisper
def test_faster_whisper_engine_falls_back_to_cpu_when_cuda_unavailable(monkeypatch):
    monkeypatch.setattr(engines, "_cuda_available", lambda: False)
    captured = {}

    class FakeWhisperModel:
        def __init__(self, model, device, compute_type):
            captured["device"] = device

    monkeypatch.setattr("faster_whisper.WhisperModel", FakeWhisperModel, raising=False)
    engines.FasterWhisperEngine("tiny.en", device="cuda", compute_type="int8")
    assert captured["device"] == "cpu"


def test_pick_engine_parakeet_raises_when_sherpa_onnx_not_installed(monkeypatch):
    import builtins

    real_import = builtins.__import__

    def fake_import(name, *args, **kwargs):
        if name == "sherpa_onnx":
            raise ImportError("simulated: sherpa_onnx not installed")
        return real_import(name, *args, **kwargs)

    monkeypatch.setattr(builtins, "__import__", fake_import)
    with pytest.raises(engines.EngineUnavailable, match="pip install"):
        engines.ParakeetEngine(Path("/nonexistent"))


def test_parakeet_raises_when_model_files_are_missing(monkeypatch, tmp_path):
    monkeypatch.setitem(sys.modules, "sherpa_onnx", types.SimpleNamespace(OfflineRecognizer=None))
    with pytest.raises(engines.EngineUnavailable, match="Download and extract"):
        engines.ParakeetEngine(tmp_path)


@requires_faster_whisper
@pytest.mark.voice_live
def test_faster_whisper_transcribes_fixtures_within_the_recorded_wer_ceiling(tmp_path):
    if not has_speech_synthesis():
        pytest.skip("espeak-ng and/or ffmpeg not on PATH — no real speech fixture to score")
    transcripts = generate_fixtures(tmp_path)
    engine = engines.FasterWhisperEngine(
        pins.FASTER_WHISPER_TEST_MODEL, device="cpu", compute_type="int8"
    )
    total_wer = 0.0
    for name, expected in transcripts.items():
        wav_bytes = (tmp_path / name).read_bytes()
        result = engine.transcribe(wav_bytes)
        wer = _word_error_rate(expected, result.text)
        assert wer <= pins.FIXTURE_MAX_WER[name], f"{name}: wer {wer:.3f}, got {result.text!r}"
        total_wer += wer
    assert total_wer / len(transcripts) <= pins.FIXTURE_MEAN_MAX_WER
```

- [ ] **Step 6: Run it to confirm it fails**

Run: `uv run --no-sync pytest tests/voice/test_engines.py -q`
Expected: FAIL — `ModuleNotFoundError: No module named 'opendaisugi.voice.engines'`

- [ ] **Step 7: Write `engines.py`**

```python
# src/opendaisugi/voice/engines.py
"""Speech-to-text engines: a small Protocol plus two adapters.

FasterWhisperEngine (CTranslate2, via the `faster-whisper` package) is the default — CPU
int8 unless `Config.voice_device == "cuda"` AND ctranslate2 actually reports a CUDA device.
CUDA is opt-in-and-verified, never auto-detected-and-switched-to: this workshop's own box has
a GPU that has crashed other torch-based inference under CUDA before (ADR-0019's potion
matcher note), and faster-whisper's CUDA path is untested on it.

ParakeetEngine (sherpa-onnx, NeMo Parakeet-TDT) is opt-in and needs a pre-extracted model
directory — see `pins.PARAKEET_MODEL_URL`. It is never auto-downloaded: the archive is
~600 MB and this box treats `/tmp` as RAM."""

from __future__ import annotations

import io
import time
import wave
from dataclasses import dataclass
from pathlib import Path
from typing import TYPE_CHECKING, Protocol

from opendaisugi.voice import pins
from opendaisugi.voice.audio import wav_duration_s

if TYPE_CHECKING:
    from opendaisugi.config import Config


@dataclass(frozen=True)
class Transcript:
    text: str
    segments: list[dict]
    duration_s: float
    rtf: float  # wall time / duration_s; > 1 means slower than real time


class Engine(Protocol):
    name: str

    def transcribe(self, wav_16k_mono: bytes, *, language: str | None = None) -> Transcript: ...


class EngineUnavailable(RuntimeError):
    """The configured engine's package, or its model files, are not available on this host."""


def _cuda_available() -> bool:
    """Best-effort, never raises — mirrors hardware.py's detection style."""
    try:
        import ctranslate2

        return ctranslate2.get_cuda_device_count() > 0
    except Exception:
        return False


class FasterWhisperEngine:
    name = "faster-whisper"

    def __init__(
        self,
        model: str = pins.FASTER_WHISPER_TEST_MODEL,
        *,
        device: str = "cpu",
        compute_type: str = "int8",
    ) -> None:
        try:
            from faster_whisper import WhisperModel
        except ImportError as exc:
            raise EngineUnavailable(
                "faster-whisper is not installed. Install it with: pip install 'opendaisugi[voice]'"
            ) from exc
        resolved_device = device
        if device == "cuda" and not _cuda_available():
            resolved_device = "cpu"
        self._model = WhisperModel(model, device=resolved_device, compute_type=compute_type)
        self.model_name = model
        self.device = resolved_device

    def transcribe(self, wav_16k_mono: bytes, *, language: str | None = None) -> Transcript:
        duration_s = wav_duration_s(wav_16k_mono)
        start = time.monotonic()
        segments_iter, _info = self._model.transcribe(io.BytesIO(wav_16k_mono), language=language)
        segments = [{"start": s.start, "end": s.end, "text": s.text.strip()} for s in segments_iter]
        elapsed = time.monotonic() - start
        text = " ".join(s["text"] for s in segments).strip()
        rtf = elapsed / duration_s if duration_s > 0 else 0.0
        return Transcript(text=text, segments=segments, duration_s=duration_s, rtf=rtf)


class ParakeetEngine:
    name = "parakeet"

    def __init__(self, model_dir: Path) -> None:
        try:
            import sherpa_onnx
        except ImportError as exc:
            raise EngineUnavailable(
                "sherpa-onnx is not installed. Install it with: "
                "pip install 'opendaisugi[voice-parakeet]'"
            ) from exc
        missing = [f for f in pins.PARAKEET_FILES if not (model_dir / f).exists()]
        if missing:
            raise EngineUnavailable(
                f"Parakeet model files missing in {model_dir}: {', '.join(missing)}. "
                f"Download and extract: {pins.PARAKEET_MODEL_URL}"
            )
        self._recognizer = sherpa_onnx.OfflineRecognizer.from_transducer(
            encoder=str(model_dir / "encoder.int8.onnx"),
            decoder=str(model_dir / "decoder.int8.onnx"),
            joiner=str(model_dir / "joiner.int8.onnx"),
            tokens=str(model_dir / "tokens.txt"),
            model_type=pins.PARAKEET_MODEL_TYPE,
            num_threads=1,
            sample_rate=16000,
            feature_dim=80,
            decoding_method="greedy_search",
            provider="cpu",
        )
        self.model_dir = model_dir

    def transcribe(self, wav_16k_mono: bytes, *, language: str | None = None) -> Transcript:
        import numpy as np

        duration_s = wav_duration_s(wav_16k_mono)
        with wave.open(io.BytesIO(wav_16k_mono), "rb") as w:
            raw = w.readframes(w.getnframes())
        samples = np.frombuffer(raw, dtype=np.int16).astype(np.float32) / 32768.0
        start = time.monotonic()
        stream = self._recognizer.create_stream()
        stream.accept_waveform(16000, samples)
        self._recognizer.decode_stream(stream)
        elapsed = time.monotonic() - start
        text = stream.result.text.strip()
        rtf = elapsed / duration_s if duration_s > 0 else 0.0
        segments = [{"start": 0.0, "end": duration_s, "text": text}]
        return Transcript(text=text, segments=segments, duration_s=duration_s, rtf=rtf)


def pick_engine(config: "Config") -> Engine:
    """faster-whisper on CPU by default; CUDA only when `config.voice_device == "cuda"`
    AND a CUDA device is actually visible. Parakeet only when `config.voice_engine ==
    "parakeet"`, gated on the package and the model files both being present."""
    if config.voice_engine == "parakeet":
        return ParakeetEngine(Path(config.voice_model))
    return FasterWhisperEngine(
        config.voice_model, device=config.voice_device, compute_type=config.voice_compute_type
    )
```

- [ ] **Step 8: Run the fast (non-`voice_live`) engine tests to confirm they pass**

Run: `uv run --no-sync pytest tests/voice/test_engines.py -q -m "not voice_live"`
Expected: PASS

- [ ] **Step 9: Run the live WER test once, by hand, to confirm the recorded ceilings hold**

Run: `DAISUGI_VOICE_LIVE=1 uv run --no-sync pytest tests/voice/test_engines.py -q -m voice_live`
Expected: PASS (this downloads `tiny.en` from Hugging Face on first run if it is not already
cached — that is expected and matches how the recorded facts in Task 0 were produced)

- [ ] **Step 10: Run the whole voice suite and ruff, then commit**

Run: `uv run --no-sync pytest tests/voice/ tests/test_config.py -q -m "not voice_live" && uv run --no-sync ruff check src/opendaisugi/voice tests/voice src/opendaisugi/config.py`
Expected: PASS, clean

```bash
git add src/opendaisugi/config.py src/opendaisugi/voice/engines.py tests/voice/test_engines.py \
        tests/test_config.py
git commit -m "voice: Config.voice_* fields and the Engine protocol (faster-whisper, parakeet)

CUDA is opt-in-and-verified in pick_engine(), never auto-detected — this box's
GPU has a history of crashing torch-based inference under CUDA. The WER test
against the espeak-ng fixtures is gated behind DAISUGI_VOICE_LIVE=1, matching
the existing smoke/calibration marker convention."
```

---

### Task 3: The cleanup pass — a fixed prompt, self-journaled, never priced against a paid model

**Files:**
- Create: `src/opendaisugi/voice/cleanup.py`
- Test: `tests/voice/test_cleanup.py`

**Interfaces:**
- Consumes: `Config.voice_cleanup`, `Config.voice_cleanup_model`, `Config.voice_cleanup_base_url`,
  `Config.data_dir` (Task 2); `opendaisugi.gateway.{RouteDecision, _PRICES_PER_MTOK,
  measure_turn}`, `opendaisugi.gateway_journal.{GatewayJournal, record_turn}`,
  `opendaisugi.routing.estimate_difficulty` (existing layer modules — read-only, no changes).
- Produces (consumed by Task 5's `server.py`):
  - `opendaisugi.voice.cleanup.CLEANUP_PROMPT_V1: str`
  - `opendaisugi.voice.cleanup.CleanupTransport` (Protocol) —
    `complete(self, *, model: str, system: str, user: str) -> tuple[str, dict[str, int]]`
  - `opendaisugi.voice.cleanup.LiteLLMCleanupTransport(base_url: str | None = None, timeout_s: float = 30.0)`
  - `opendaisugi.voice.cleanup.clean_transcript(text: str, *, config: Config, transport: CleanupTransport | None = None, journal: GatewayJournal | None = None) -> str`

- [ ] **Step 1: Write the failing cleanup tests**

```python
# tests/voice/test_cleanup.py
from __future__ import annotations

from opendaisugi.config import Config
from opendaisugi.gateway_journal import GatewayJournal
from opendaisugi.voice.cleanup import CLEANUP_PROMPT_V1, clean_transcript


class FakeTransport:
    def __init__(self, corrected_text: str) -> None:
        self.corrected_text = corrected_text
        self.calls: list[tuple[str, str, str]] = []

    def complete(self, *, model: str, system: str, user: str) -> tuple[str, dict[str, int]]:
        self.calls.append((model, system, user))
        return self.corrected_text, {"input_tokens": 42, "output_tokens": 7}


def test_clean_transcript_returns_original_when_cleanup_is_disabled(tmp_path):
    config = Config(
        data_dir=tmp_path, voice_cleanup=False, voice_cleanup_model="ollama/llama3.2:3b"
    )
    result = clean_transcript(
        "hello  world", config=config, transport=FakeTransport("Hello, world.")
    )
    assert result == "hello  world"


def test_clean_transcript_returns_original_when_no_model_is_configured(tmp_path):
    config = Config(data_dir=tmp_path, voice_cleanup=True, voice_cleanup_model=None)
    result = clean_transcript(
        "hello world", config=config, transport=FakeTransport("Hello, world.")
    )
    assert result == "hello world"


def test_clean_transcript_calls_the_transport_with_the_fixed_prompt(tmp_path):
    config = Config(data_dir=tmp_path, voice_cleanup=True, voice_cleanup_model="ollama/llama3.2:3b")
    transport = FakeTransport("Hello, world.")
    result = clean_transcript("hello  world", config=config, transport=transport)
    assert result == "Hello, world."
    assert transport.calls == [("ollama/llama3.2:3b", CLEANUP_PROMPT_V1, "hello  world")]


def test_clean_transcript_falls_back_to_the_original_on_an_empty_correction(tmp_path):
    config = Config(data_dir=tmp_path, voice_cleanup=True, voice_cleanup_model="ollama/llama3.2:3b")
    result = clean_transcript("hello world", config=config, transport=FakeTransport("   "))
    assert result == "hello world"


def test_clean_transcript_journals_one_turn_tagged_voice_cleanup_priced_at_zero(tmp_path):
    config = Config(data_dir=tmp_path, voice_cleanup=True, voice_cleanup_model="ollama/llama3.2:3b")
    journal = GatewayJournal(path=tmp_path / "gateway" / "turns.jsonl")

    clean_transcript(
        "first thing said",
        config=config,
        transport=FakeTransport("First thing said."),
        journal=journal,
    )
    clean_transcript(
        "second thing said",
        config=config,
        transport=FakeTransport("Second thing said."),
        journal=journal,
    )

    records = journal.load()
    assert len(records) == 2
    assert all(r.task == "voice.cleanup" for r in records)
    # Same repeatable ask every time -> the garden can cluster every cleanup
    # call into one repeat group, exactly what a reuse pathway needs.
    assert records[0].signature == records[1].signature
    # Never a phantom saving, and never priced at the (3.0, 15.0) fallback rate
    # that would otherwise depress the gateway's blended multiplier.
    assert records[0].downgraded is False
    assert records[0].actual_dollars == 0.0
    assert records[0].counterfactual_dollars == 0.0
    # The transcript itself never enters the journal.
    for r in records:
        assert "first thing" not in r.task
        assert "second thing" not in r.task


def test_clean_transcript_uses_the_default_journal_under_config_data_dir(tmp_path):
    config = Config(data_dir=tmp_path, voice_cleanup=True, voice_cleanup_model="ollama/llama3.2:3b")
    clean_transcript("hello world", config=config, transport=FakeTransport("Hello, world."))
    journal = GatewayJournal(path=tmp_path / "gateway" / "turns.jsonl")
    assert len(journal.load()) == 1
```

- [ ] **Step 2: Run it to confirm it fails**

Run: `uv run --no-sync pytest tests/voice/test_cleanup.py -q`
Expected: FAIL — `ModuleNotFoundError: No module named 'opendaisugi.voice.cleanup'`

- [ ] **Step 3: Write `cleanup.py`**

```python
# src/opendaisugi/voice/cleanup.py
"""The optional cleanup pass: one fixed-prompt local-model call, self-journaled.

Decision (spec-07's cruxes say "journaled like any other turn, so the garden can distil it");
this module journals it *itself*, always tagged with the literal task "voice.cleanup", and
never through a running gateway process's own auto-journaling. Two reasons: (1) the
transcript never enters `turns.jsonl` this way — the repo's own posture on raw content
(`capture_answers`, opt-in and off by default) says a transcript belongs there even less;
(2) every cleanup call collapses into one repeat signature by construction, which is exactly
the distillation signal the garden looks for (see `gateway_distill.py`).

The transport is `litellm.completion()` — `litellm` is already a hard dependency of
opendaisugi, and this mirrors `tier1.LiteLLMTier1Provider`'s shape for a local
OpenAI-compatible endpoint (Ollama/llamafile) exactly. No new dependency, no new wire format.

Pricing: `measure_turn()` is called with an explicit price override for the cleanup model at
(0.0, 0.0) — without it, `price_turn()`'s fallback rate (Sonnet-priced) would book real
dollars for a local call and silently depress the gateway's blended multiplier across every
other recorded turn. `Gateway.__post_init__` (gateway_pipeline.py) does the same override for
`local_model`; this mirrors it rather than routing this call *through* that class, because a
cleanup call has no "requested model" to route away from — it is always local, by design,
never a decision.
"""

from __future__ import annotations

from pathlib import Path
from typing import Protocol

from opendaisugi.config import Config
from opendaisugi.gateway import _PRICES_PER_MTOK, RouteDecision, measure_turn
from opendaisugi.gateway_journal import GatewayJournal, record_turn
from opendaisugi.routing import estimate_difficulty

CLEANUP_PROMPT_V1 = (
    "Fix punctuation and obvious transcription errors. Keep the meaning and "
    "the words. Return only the corrected text."
)


class CleanupTransport(Protocol):
    def complete(self, *, model: str, system: str, user: str) -> tuple[str, dict[str, int]]:
        """One blocking call. Returns (corrected_text, usage), usage carrying
        input_tokens/output_tokens (0 when the backend does not report them)."""
        ...


class LiteLLMCleanupTransport:
    """The real transport: one `litellm.completion()` call. `base_url` set means a local
    OpenAI-compatible endpoint (Ollama/llamafile/a running `daisugi gateway`) — mirrors
    `LiteLLMTier1Provider`'s auto-prefix (a bare model name needs `openai/` to route to a
    `base_url` instead of a cloud provider)."""

    def __init__(self, *, base_url: str | None = None, timeout_s: float = 30.0) -> None:
        self.base_url = base_url
        self.timeout_s = timeout_s

    def complete(self, *, model: str, system: str, user: str) -> tuple[str, dict[str, int]]:
        import litellm

        call_model = model
        if self.base_url is not None and "/" not in call_model:
            call_model = f"openai/{call_model}"
        kwargs: dict = {}
        if self.base_url is not None:
            kwargs["base_url"] = self.base_url
        resp = litellm.completion(
            model=call_model,
            messages=[
                {"role": "system", "content": system},
                {"role": "user", "content": user},
            ],
            timeout=self.timeout_s,
            **kwargs,
        )
        text = resp.choices[0].message.content or ""
        usage = getattr(resp, "usage", None) or {}
        if isinstance(usage, dict):
            input_tokens = int(usage.get("prompt_tokens", 0) or 0)
            output_tokens = int(usage.get("completion_tokens", 0) or 0)
        else:
            input_tokens = int(getattr(usage, "prompt_tokens", 0) or 0)
            output_tokens = int(getattr(usage, "completion_tokens", 0) or 0)
        return text, {"input_tokens": input_tokens, "output_tokens": output_tokens}


def _default_journal(config: Config) -> GatewayJournal:
    return GatewayJournal(path=Path(config.data_dir) / "gateway" / "turns.jsonl")


def _journal_cleanup(*, model: str, usage: dict[str, int], journal: GatewayJournal) -> None:
    decision = RouteDecision(
        tier="tier1-local",
        model=model,
        requested_model=model,
        difficulty=estimate_difficulty(CLEANUP_PROMPT_V1),
        downgraded=False,
        reason="voice cleanup: a fixed local pass, never routed",
    )
    saving = measure_turn(decision, usage, prices={**_PRICES_PER_MTOK, model: (0.0, 0.0)})
    record = record_turn(decision, saving, task="voice.cleanup", ask="voice.cleanup")
    journal.append(record)


def clean_transcript(
    text: str,
    *,
    config: Config,
    transport: CleanupTransport | None = None,
    journal: GatewayJournal | None = None,
) -> str:
    """Polish a transcript through one fixed-prompt local-model call.

    Returns `text` unchanged when cleanup is off, or when no model is configured — cleanup
    never touches a paid model unless `config.voice_cleanup_model` names one, and it never
    silently falls back to the cloud."""
    if not config.voice_cleanup or not config.voice_cleanup_model:
        return text
    model = config.voice_cleanup_model
    transport = transport or LiteLLMCleanupTransport(base_url=config.voice_cleanup_base_url)
    corrected, usage = transport.complete(model=model, system=CLEANUP_PROMPT_V1, user=text)
    _journal_cleanup(model=model, usage=usage, journal=journal or _default_journal(config))
    cleaned = corrected.strip()
    return cleaned or text
```

- [ ] **Step 4: Run it to confirm it passes**

Run: `uv run --no-sync pytest tests/voice/test_cleanup.py -q`
Expected: PASS

- [ ] **Step 5: Run the whole voice suite and ruff, then commit**

Run: `uv run --no-sync pytest tests/voice/ -q -m "not voice_live" && uv run --no-sync ruff check src/opendaisugi/voice/cleanup.py tests/voice/test_cleanup.py`
Expected: PASS, clean

```bash
git add src/opendaisugi/voice/cleanup.py tests/voice/test_cleanup.py
git commit -m "voice: the cleanup pass, self-journaled as a zero-priced, repeatable turn

Tags every call task='voice.cleanup' so the transcript never enters
turns.jsonl and every cleanup call collapses into one repeat cluster for the
garden. Prices the local model at (0,0) explicitly — bypassing that would
silently book Sonnet-rate dollars against a local call."
```

---

### Task 4: Delivery — preview by default, armed send otherwise

**Files:**
- Create: `src/opendaisugi/voice/deliver.py`
- Test: `tests/voice/test_deliver.py`

**Interfaces:**
- Consumes: nothing from earlier tasks (deliberately decoupled — see module docstring).
  `PaneBackend`/`PaneRef` (master §3.2, `opendaisugi.floor.backend`, delivered by spec-01/03)
  are referenced only under `TYPE_CHECKING`, never imported at runtime by this module.
- Produces (consumed by Task 5's `server.py`):
  - `opendaisugi.voice.deliver.ArmEntry` — `pane_key: str`, `armed_at: float`, `expires_at: float`
  - `opendaisugi.voice.deliver.arm(pane_key: str, *, minutes: float, armed_dir: Path, now: float | None = None) -> ArmEntry`
  - `opendaisugi.voice.deliver.disarm(pane_key: str, *, armed_dir: Path) -> None`
  - `opendaisugi.voice.deliver.is_armed(pane_key: str, *, armed_dir: Path, now: float | None = None) -> bool`
  - `opendaisugi.voice.deliver.DeliverResult` — `delivered: Literal["preview", "sent", "refused"]`, `reason: str | None`
  - `opendaisugi.voice.deliver.deliver(pane, text: str, backend, *, mode: Literal["preview", "send"] = "preview", armed_dir: Path, pane_key: str | None = None) -> DeliverResult`
    — `pane` and `backend` are duck-typed: `pane` needs `.backend`/`.id` attributes
    (matches §3.2's `PaneRef`); `backend` needs `.send_text(pane, text)` (matches §3.2's
    `PaneBackend.send_text`).

- [ ] **Step 1: Write the failing deliver tests**

```python
# tests/voice/test_deliver.py
from __future__ import annotations

from dataclasses import dataclass

import pytest

from opendaisugi.voice.deliver import DeliverResult, arm, deliver, disarm, is_armed


@dataclass(frozen=True)
class FakePaneRef:
    """Matches the shape of opendaisugi.floor.backend.PaneRef (backend: str, id: str)
    without importing it — deliver.py is decoupled from the floor package on purpose."""

    backend: str
    id: str


class FakeBackend:
    def __init__(self) -> None:
        self.send_text_calls: list[tuple[FakePaneRef, str]] = []

    def send_text(self, pane, text, *, enter: bool = True) -> None:
        self.send_text_calls.append((pane, text))


def test_deliver_preview_mode_never_calls_the_backend(tmp_path):
    backend = FakeBackend()
    pane = FakePaneRef(backend="coppice", id="w1:p1")
    result = deliver(pane, "hello", backend, mode="preview", armed_dir=tmp_path)
    assert result == DeliverResult(delivered="preview")
    assert backend.send_text_calls == []


def test_deliver_send_is_refused_when_the_pane_is_not_armed(tmp_path):
    backend = FakeBackend()
    pane = FakePaneRef(backend="coppice", id="w1:p1")
    result = deliver(pane, "hello", backend, mode="send", armed_dir=tmp_path)
    assert result.delivered == "refused"
    assert "daisugi voice arm w1:p1" in result.reason
    assert backend.send_text_calls == []


def test_deliver_send_succeeds_once_the_pane_is_armed(tmp_path):
    backend = FakeBackend()
    pane = FakePaneRef(backend="coppice", id="w1:p1")
    arm("w1:p1", minutes=30, armed_dir=tmp_path)
    result = deliver(pane, "hello", backend, mode="send", armed_dir=tmp_path)
    assert result == DeliverResult(delivered="sent")
    assert backend.send_text_calls == [(pane, "hello")]


def test_arm_grant_expires(tmp_path):
    entry = arm("w1:p1", minutes=1, armed_dir=tmp_path, now=1_000.0)
    assert entry.expires_at == pytest.approx(1_060.0)
    assert is_armed("w1:p1", armed_dir=tmp_path, now=1_030.0)
    assert not is_armed("w1:p1", armed_dir=tmp_path, now=1_100.0)


def test_disarm_revokes_the_grant(tmp_path):
    arm("w1:p1", minutes=30, armed_dir=tmp_path)
    assert is_armed("w1:p1", armed_dir=tmp_path)
    disarm("w1:p1", armed_dir=tmp_path)
    assert not is_armed("w1:p1", armed_dir=tmp_path)


def test_disarm_on_a_pane_that_was_never_armed_does_not_raise(tmp_path):
    disarm("never:armed", armed_dir=tmp_path)  # must not raise


def test_is_armed_treats_a_corrupt_entry_file_as_unarmed(tmp_path):
    tmp_path.mkdir(exist_ok=True)
    (tmp_path / "coppice_w1_p1.json").write_text("not json")
    assert not is_armed("w1:p1", armed_dir=tmp_path)


def test_arm_creates_the_armed_dir_at_0700_and_the_entry_at_0600(tmp_path):
    armed_dir = tmp_path / "armed"
    arm("w1:p1", minutes=30, armed_dir=armed_dir)
    assert oct(armed_dir.stat().st_mode)[-3:] == "700"
    entry_files = list(armed_dir.glob("*.json"))
    assert len(entry_files) == 1
    assert oct(entry_files[0].stat().st_mode)[-3:] == "600"
```

- [ ] **Step 2: Run it to confirm it fails**

Run: `uv run --no-sync pytest tests/voice/test_deliver.py -q`
Expected: FAIL — `ModuleNotFoundError: No module named 'opendaisugi.voice.deliver'`

- [ ] **Step 3: Write `deliver.py`**

```python
# src/opendaisugi/voice/deliver.py
"""Deliver transcribed text to a pane: preview by default, sent only when armed.

Direct delivery is exactly the input a fail-closed system should not feed an agent unseen
(master §3.6) — voice transcription errors included. `deliver()` never imports a concrete
`PaneBackend`; it only calls the method the §3.2 protocol promises (`send_text`), and it
treats `pane` as an opaque object with `.backend`/`.id` (matching §3.2's `PaneRef`). This
keeps this module free of any runtime dependency on `opendaisugi.floor` — the caller (Task 5's
server.py) resolves and injects a real backend and a real pane reference.

An arm grant is an authorization: the directory is 0700, the entry file 0600, and an expired
or unreadable entry is always treated as unarmed — never fails open."""

from __future__ import annotations

import json
import time
from dataclasses import dataclass
from pathlib import Path
from typing import TYPE_CHECKING, Literal

if TYPE_CHECKING:
    from opendaisugi.floor.backend import PaneBackend, PaneRef


@dataclass(frozen=True)
class ArmEntry:
    pane_key: str
    armed_at: float
    expires_at: float


def _armed_path(armed_dir: Path, pane_key: str) -> Path:
    safe = pane_key.replace("/", "_").replace(":", "_")
    return armed_dir / f"{safe}.json"


def arm(pane_key: str, *, minutes: float, armed_dir: Path, now: float | None = None) -> ArmEntry:
    """Grant `pane_key` direct-send for `minutes`. Creates `armed_dir` at 0700 and the entry
    file at 0600 if they do not already exist."""
    now = time.time() if now is None else now
    armed_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
    entry = ArmEntry(pane_key=pane_key, armed_at=now, expires_at=now + minutes * 60.0)
    path = _armed_path(armed_dir, pane_key)
    path.write_text(
        json.dumps(
            {"pane_key": entry.pane_key, "armed_at": entry.armed_at, "expires_at": entry.expires_at}
        )
    )
    path.chmod(0o600)
    return entry


def disarm(pane_key: str, *, armed_dir: Path) -> None:
    _armed_path(armed_dir, pane_key).unlink(missing_ok=True)


def is_armed(pane_key: str, *, armed_dir: Path, now: float | None = None) -> bool:
    """A missing, corrupt, or expired entry is unarmed — never fails open."""
    now = time.time() if now is None else now
    path = _armed_path(armed_dir, pane_key)
    if not path.exists():
        return False
    try:
        data = json.loads(path.read_text())
    except (OSError, ValueError):
        return False
    return float(data.get("expires_at", 0.0)) > now


@dataclass(frozen=True)
class DeliverResult:
    delivered: Literal["preview", "sent", "refused"]
    reason: str | None = None


def deliver(
    pane: "PaneRef",
    text: str,
    backend: "PaneBackend",
    *,
    mode: Literal["preview", "send"] = "preview",
    armed_dir: Path,
    pane_key: str | None = None,
) -> DeliverResult:
    """Deliver `text` toward `pane` through `backend`.

    `preview` always succeeds without touching the backend — the caller (the client UI) is
    responsible for actually showing the text; this only reports the mode. `send` is refused
    unless the pane is armed, and the refusal names the exact arm command.

    The default arm key is the bare `pane.id` (not a "backend:id" composite) — this matches
    `daisugi voice arm PANE` / `daisugi coppice list`'s own bare pane ids exactly, so the
    command a refusal prints is one the operator can paste verbatim. `registry.pick_backend`
    resolves one backend per invocation, so a bare id is unambiguous in practice; pass
    `pane_key` explicitly to disambiguate if that ever stops being true."""
    key = pane_key if pane_key is not None else pane.id
    if mode == "preview":
        return DeliverResult(delivered="preview")
    if not is_armed(key, armed_dir=armed_dir):
        return DeliverResult(
            delivered="refused",
            reason=f"pane not armed for direct send. Run: daisugi voice arm {key} --for 30m",
        )
    backend.send_text(pane, text)
    return DeliverResult(delivered="sent")
```

- [ ] **Step 4: Run it to confirm it passes**

Run: `uv run --no-sync pytest tests/voice/test_deliver.py -q`
Expected: PASS

- [ ] **Step 5: Run the whole voice suite and ruff, then commit**

Run: `uv run --no-sync pytest tests/voice/ -q -m "not voice_live" && uv run --no-sync ruff check src/opendaisugi/voice/deliver.py tests/voice/test_deliver.py`
Expected: PASS, clean

```bash
git add src/opendaisugi/voice/deliver.py tests/voice/test_deliver.py
git commit -m "voice: deliver() with preview-by-default and an armed-only send grant

deliver.py has no runtime import of opendaisugi.floor — pane and backend are
duck-typed against the §3.2 PaneBackend contract, so this module tests and
loads independently of which pane backend plan-03 ships."
```

---

### Task 5: The voice server — `/health`, `/transcribe`, `/deliver`

**Files:**
- Create: `src/opendaisugi/voice/server.py`
- Test: `tests/voice/test_server.py`

**Interfaces:**
- Consumes: `pick_engine`/`EngineUnavailable` (Task 2); `to_wav_16k_mono`/`AudioFormatUnsupported`/
  `wav_duration_s` (Task 1); `clean_transcript` (Task 3); `deliver.deliver`/`deliver.is_armed`
  (Task 4); `Config` (existing).
- Produces (consumed by Task 6's `ptt.py` — as an HTTP client, not an import — and Task 7's
  `cli.py`):
  - `opendaisugi.voice.server.TOKEN_PATH: Path` (the cross-plan assumption; see the plan
    header's Decision 4)
  - `opendaisugi.voice.server.MAX_AUDIO_SECONDS: float` (60.0)
  - `opendaisugi.voice.server.VoiceServer(ThreadingHTTPServer)` — constructed via
    `build_server(...)`
  - `opendaisugi.voice.server.build_server(host: str, port: int, *, config: Config, engine, token_file: Path, armed_dir: Path, backend_factory: Callable[[], "PaneBackend"], tls_cert: Path | None = None, tls_key: Path | None = None) -> VoiceServer`
  - `opendaisugi.voice.server.serve(*, host: str = "127.0.0.1", port: int = 7477, config: Config, token_file: Path = TOKEN_PATH, armed_dir: Path | None = None, tls_cert: Path | None = None, tls_key: Path | None = None) -> None`
    (blocking; builds the engine via `pick_engine(config)`, resolves the backend via
    `opendaisugi.floor.registry.pick_backend`, calls `serve_forever()`)

- [ ] **Step 1: Write the failing server tests (health, loopback auth bypass, off-loopback refusal)**

```python
# tests/voice/test_server.py
from __future__ import annotations

import http.client
import json
import threading
import wave
from dataclasses import dataclass
from io import BytesIO

import numpy as np
import pytest

from opendaisugi.config import Config
from opendaisugi.voice import server as voice_server
from opendaisugi.voice.deliver import arm
from opendaisugi.voice.engines import Transcript


@dataclass(frozen=True)
class FakePaneRef:
    backend: str
    id: str


class FakeEngine:
    name = "fake"

    def __init__(self, text: str = "hello from the fixture") -> None:
        self.text = text
        self.calls: list[bytes] = []

    def transcribe(self, wav_16k_mono: bytes, *, language: str | None = None) -> Transcript:
        self.calls.append(wav_16k_mono)
        return Transcript(text=self.text, segments=[], duration_s=1.0, rtf=0.1)


class FakeBackend:
    name = "fake"

    def __init__(self) -> None:
        self.sent: list[tuple[FakePaneRef, str]] = []

    def send_text(self, pane, text, *, enter: bool = True) -> None:
        self.sent.append((pane, text))


def _make_wav(seconds: float = 1.0, sr: int = 16000) -> bytes:
    n = int(sr * seconds)
    samples = (np.sin(2 * np.pi * 440 * np.arange(n) / sr) * 10000).astype(np.int16)
    buf = BytesIO()
    with wave.open(buf, "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(sr)
        w.writeframes(samples.tobytes())
    return buf.getvalue()


@pytest.fixture
def running_server(tmp_path):
    engine = FakeEngine()
    backend = FakeBackend()
    config = Config(data_dir=tmp_path)
    token_file = tmp_path / "token"
    token_file.write_text("s3cr3t")
    server = voice_server.build_server(
        "127.0.0.1",
        0,
        config=config,
        engine=engine,
        token_file=token_file,
        armed_dir=tmp_path / "armed",
        backend_factory=lambda: backend,
    )
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    yield server, engine, backend, token_file, tmp_path
    server.shutdown()
    thread.join(timeout=5)


def _conn(server) -> http.client.HTTPConnection:
    host, port = server.server_address[:2]
    return http.client.HTTPConnection(host, port, timeout=5)


def test_health_returns_ok_without_a_token(running_server):
    server, *_ = running_server
    conn = _conn(server)
    conn.request("GET", "/health")
    resp = conn.getresponse()
    assert resp.status == 200
    assert json.loads(resp.read()) == {"ok": True}


def test_transcribe_from_loopback_needs_no_token(running_server):
    server, engine, _backend, _token_file, _tmp = running_server
    conn = _conn(server)
    conn.request("POST", "/transcribe", body=_make_wav(), headers={"Content-Type": "audio/wav"})
    resp = conn.getresponse()
    assert resp.status == 200
    payload = json.loads(resp.read())
    assert payload["text"] == "hello from the fixture"
    assert payload["engine"] == "fake"
    assert payload["cleaned"] is False
    assert len(engine.calls) == 1
```

- [ ] **Step 2: Run it to confirm it fails**

Run: `uv run --no-sync pytest tests/voice/test_server.py -q`
Expected: FAIL — `ModuleNotFoundError: No module named 'opendaisugi.voice.server'`

- [ ] **Step 3: Write `server.py`**

```python
# src/opendaisugi/voice/server.py
"""stdlib HTTP server for the voice bridge — POST /transcribe, POST /deliver, GET /health.

`http.server` rather than the `[gateway]` extra's httpx/uvicorn ASGI stack: this answers
three endpoints for one local surface, not a wire-protocol proxy, so pulling in an async web
stack here would be extra weight for no benefit.

Binds 127.0.0.1 by default. `--listen 0.0.0.0:PORT` (the tailnet) requires the same
bearer-token file `coppice web token` writes (spec-06). spec-06 does not itself pin that
path, so TOKEN_PATH below is a cross-plan assumption — override with `--token-file` if
plan-06 lands at a different path. Off-loopback with no readable token file REFUSES TO
START rather than serve unauthenticated (fail-closed, master §3.6)."""

from __future__ import annotations

import ipaddress
import json
import logging
import ssl
import time
from dataclasses import dataclass
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import TYPE_CHECKING, Callable
from urllib.parse import parse_qs, urlsplit

from opendaisugi.config import Config
from opendaisugi.voice import deliver as deliver_mod
from opendaisugi.voice.audio import AudioFormatUnsupported, to_wav_16k_mono, wav_duration_s
from opendaisugi.voice.cleanup import clean_transcript
from opendaisugi.voice.engines import EngineUnavailable

try:
    # The real thing, once plan-01/03 land (master's critical path: 00→01→02→03→06→07).
    from opendaisugi.floor.backend import PaneRef
except ImportError:  # pragma: no cover - exercised only before plan-01/03 land

    @dataclass(frozen=True)
    class PaneRef:  # local stand-in, identical shape to opendaisugi.floor.backend.PaneRef
        backend: str
        id: str


if TYPE_CHECKING:
    from opendaisugi.floor.backend import PaneBackend

_log = logging.getLogger("opendaisugi.voice.server")

MAX_AUDIO_SECONDS = 60.0
MAX_CONTENT_LENGTH_BYTES = 30_000_000  # a wire-size ceiling, checked before any decode
TOKEN_PATH = Path.home() / ".opendaisugi" / "coppice" / "web-token"
_BAN_WINDOW_S = 60.0
_BAN_THRESHOLD = 3


def _read_token(token_file: Path) -> str | None:
    try:
        return token_file.read_text(encoding="utf-8").strip() or None
    except OSError:
        return None


def _is_loopback(address: str) -> bool:
    try:
        return ipaddress.ip_address(address).is_loopback
    except ValueError:
        return False


def _extract_multipart_audio(body: bytes, content_type: str) -> tuple[bytes, str]:
    """Minimal multipart/form-data extraction: the first file part's bytes and its own
    Content-Type. No `cgi` module (deprecated in 3.12, removed in 3.13) — a MediaRecorder
    upload has exactly one part, so a boundary split is enough."""
    marker = "boundary="
    idx = content_type.find(marker)
    if idx == -1:
        raise ValueError("multipart request is missing a boundary")
    boundary = content_type[idx + len(marker) :].strip().strip('"').encode()
    for part in body.split(b"--" + boundary):
        if b"Content-Disposition" not in part:
            continue
        header_end = part.find(b"\r\n\r\n")
        if header_end == -1:
            continue
        headers = part[:header_end].decode("latin-1")
        payload = part[header_end + 4 :]
        if payload.endswith(b"\r\n"):
            payload = payload[:-2]
        part_type = "application/octet-stream"
        for line in headers.splitlines():
            if line.lower().startswith("content-type:"):
                part_type = line.split(":", 1)[1].strip()
        return payload, part_type
    raise ValueError("multipart request has no file part")


class VoiceRequestHandler(BaseHTTPRequestHandler):
    server_version = "opendaisugi-voice/1"

    def log_message(self, fmt: str, *args) -> None:  # route through logging, not stderr
        _log.info("%s - %s", self.client_address[0], fmt % args)

    def _authorized(self) -> bool:
        server: VoiceServer = self.server  # type: ignore[assignment]
        if _is_loopback(self.client_address[0]):
            return True
        token = _read_token(server.token_file)
        header = self.headers.get("Authorization", "")
        supplied = header[len("Bearer ") :].strip() if header.startswith("Bearer ") else ""
        ok = bool(token) and supplied == token
        if not ok:
            server.record_auth_failure(self.client_address[0])
        return ok

    def _send_json(self, status: int, payload: dict) -> None:
        body = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self) -> None:
        if urlsplit(self.path).path == "/health":
            self._send_json(200, {"ok": True})
            return
        self._send_json(404, {"error": "not_found"})

    def do_POST(self) -> None:
        server: VoiceServer = self.server  # type: ignore[assignment]
        parsed = urlsplit(self.path)
        if server.is_banned(self.client_address[0]):
            self._send_json(429, {"error": "too_many_failed_tokens"})
            return
        if not self._authorized():
            self._send_json(401, {"error": "unauthorized"})
            return
        if parsed.path == "/transcribe":
            want_cleanup = parse_qs(parsed.query).get("cleanup", ["0"])[0] == "1"
            self._handle_transcribe(server, want_cleanup=want_cleanup)
        elif parsed.path == "/deliver":
            self._handle_deliver(server)
        else:
            self._send_json(404, {"error": "not_found"})

    def _handle_transcribe(self, server: "VoiceServer", *, want_cleanup: bool) -> None:
        length = int(self.headers.get("Content-Length", "0"))
        if length > MAX_CONTENT_LENGTH_BYTES:
            self._send_json(413, {"error": "payload_too_large"})
            return
        body = self.rfile.read(length)
        content_type = self.headers.get("Content-Type", "application/octet-stream")
        try:
            if content_type.startswith("multipart/form-data"):
                audio_bytes, inner_type = _extract_multipart_audio(body, content_type)
            else:
                audio_bytes, inner_type = body, content_type
            wav_bytes = to_wav_16k_mono(audio_bytes, inner_type)
        except (AudioFormatUnsupported, ValueError) as exc:
            self._send_json(400, {"error": "bad_audio", "message": str(exc)})
            return
        duration_s = wav_duration_s(wav_bytes)
        if duration_s > MAX_AUDIO_SECONDS:
            self._send_json(413, {"error": "audio_too_long", "max_seconds": MAX_AUDIO_SECONDS})
            return
        try:
            transcript = server.engine.transcribe(wav_bytes)
        except EngineUnavailable as exc:
            self._send_json(503, {"error": "engine_unavailable", "message": str(exc)})
            return
        text = transcript.text
        cleaned = False
        if want_cleanup:
            polished = clean_transcript(text, config=server.config)
            cleaned = polished != text
            text = polished
        self._send_json(
            200,
            {
                "text": text,
                "duration_s": transcript.duration_s,
                "rtf": transcript.rtf,
                "engine": server.engine.name,
                "cleaned": cleaned,
            },
        )

    def _handle_deliver(self, server: "VoiceServer") -> None:
        length = int(self.headers.get("Content-Length", "0"))
        try:
            payload = json.loads(self.rfile.read(length) or b"{}")
        except json.JSONDecodeError:
            self._send_json(400, {"error": "bad_json"})
            return
        pane_id = payload.get("pane")
        text = payload.get("text", "")
        mode = payload.get("mode", "preview")
        backend_name = payload.get("backend")
        if not pane_id or mode not in ("preview", "send"):
            self._send_json(400, {"error": "bad_request"})
            return
        backend = server.backend_factory()
        pane = PaneRef(backend=backend_name or backend.name, id=pane_id)
        result = deliver_mod.deliver(pane, text, backend, mode=mode, armed_dir=server.armed_dir)
        status = 200 if result.delivered != "refused" else 403
        self._send_json(status, {"delivered": result.delivered, "reason": result.reason})


class VoiceServer(ThreadingHTTPServer):
    def __init__(
        self,
        address: tuple[str, int],
        *,
        config: Config,
        engine,
        token_file: Path,
        armed_dir: Path,
        backend_factory: Callable[[], "PaneBackend"],
        tls_cert: Path | None = None,
        tls_key: Path | None = None,
    ) -> None:
        super().__init__(address, VoiceRequestHandler)
        self.config = config
        self.engine = engine
        self.token_file = token_file
        self.armed_dir = armed_dir
        self.backend_factory = backend_factory
        self._auth_failures: dict[str, list[float]] = {}
        if tls_cert is not None or tls_key is not None:
            if tls_cert is None or tls_key is None:
                raise ValueError("--tls-cert and --tls-key must both be given, or neither")
            ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
            ctx.load_cert_chain(certfile=str(tls_cert), keyfile=str(tls_key))
            self.socket = ctx.wrap_socket(self.socket, server_side=True)

    def record_auth_failure(self, address: str) -> None:
        now = time.time()
        hits = [t for t in self._auth_failures.get(address, []) if now - t < _BAN_WINDOW_S]
        hits.append(now)
        self._auth_failures[address] = hits

    def is_banned(self, address: str) -> bool:
        now = time.time()
        hits = [t for t in self._auth_failures.get(address, []) if now - t < _BAN_WINDOW_S]
        self._auth_failures[address] = hits
        return len(hits) >= _BAN_THRESHOLD


def _is_loopback_host(host: str) -> bool:
    return host == "localhost" or _is_loopback(host)


def build_server(
    host: str,
    port: int,
    *,
    config: Config,
    engine,
    token_file: Path,
    armed_dir: Path,
    backend_factory: Callable[[], "PaneBackend"],
    tls_cert: Path | None = None,
    tls_key: Path | None = None,
) -> VoiceServer:
    """Construct (but do not start) a VoiceServer. Refuses off-loopback without a readable
    token file — fail-closed, never serves unauthenticated."""
    if not _is_loopback_host(host) and _read_token(token_file) is None:
        raise RuntimeError(
            f"refusing to listen on {host} without a token: {token_file} is missing or "
            f"unreadable. Run `coppice web token` first, pass --token-file, or bind "
            f"127.0.0.1 only."
        )
    return VoiceServer(
        (host, port),
        config=config,
        engine=engine,
        token_file=token_file,
        armed_dir=armed_dir,
        backend_factory=backend_factory,
        tls_cert=tls_cert,
        tls_key=tls_key,
    )


def serve(
    *,
    host: str = "127.0.0.1",
    port: int = 7477,
    config: Config,
    token_file: Path = TOKEN_PATH,
    armed_dir: Path | None = None,
    tls_cert: Path | None = None,
    tls_key: Path | None = None,
) -> None:
    """Build the real engine and backend, then serve forever (blocking)."""
    from opendaisugi.floor.registry import pick_backend
    from opendaisugi.voice.engines import pick_engine

    engine = pick_engine(config)
    resolved_armed_dir = armed_dir or (config.data_dir / "voice" / "armed")
    server = build_server(
        host,
        port,
        config=config,
        engine=engine,
        token_file=token_file,
        armed_dir=resolved_armed_dir,
        backend_factory=lambda: pick_backend(config),
        tls_cert=tls_cert,
        tls_key=tls_key,
    )
    server.serve_forever()
```

- [ ] **Step 4: Run the two written tests to confirm they pass**

Run: `uv run --no-sync pytest tests/voice/test_server.py -q`
Expected: PASS

- [ ] **Step 5: Add and pass the auth, oversize, and refuse-to-start tests**

Append to `tests/voice/test_server.py`:

```python
def test_transcribe_off_loopback_with_a_bad_token_is_401(running_server, monkeypatch):
    server, *_ = running_server
    monkeypatch.setattr(voice_server, "_is_loopback", lambda address: False)
    conn = _conn(server)
    conn.request(
        "POST",
        "/transcribe",
        body=_make_wav(),
        headers={"Content-Type": "audio/wav", "Authorization": "Bearer wrong"},
    )
    resp = conn.getresponse()
    assert resp.status == 401
    resp.read()


def test_transcribe_off_loopback_with_the_right_token_succeeds(running_server, monkeypatch):
    server, _engine, _backend, token_file, _tmp = running_server
    monkeypatch.setattr(voice_server, "_is_loopback", lambda address: False)
    conn = _conn(server)
    token = token_file.read_text().strip()
    conn.request(
        "POST",
        "/transcribe",
        body=_make_wav(),
        headers={"Content-Type": "audio/wav", "Authorization": f"Bearer {token}"},
    )
    resp = conn.getresponse()
    assert resp.status == 200
    resp.read()


def test_three_bad_tokens_ban_the_address_for_a_minute(running_server, monkeypatch):
    server, *_ = running_server
    monkeypatch.setattr(voice_server, "_is_loopback", lambda address: False)
    conn = _conn(server)
    for _ in range(3):
        conn.request(
            "POST",
            "/transcribe",
            body=_make_wav(),
            headers={"Content-Type": "audio/wav", "Authorization": "Bearer wrong"},
        )
        conn.getresponse().read()
        conn = _conn(server)
    conn.request(
        "POST",
        "/transcribe",
        body=_make_wav(),
        headers={"Content-Type": "audio/wav", "Authorization": "Bearer wrong"},
    )
    resp = conn.getresponse()
    assert resp.status == 429
    resp.read()


def test_transcribe_accepts_a_multipart_form_upload(running_server):
    server, engine, _backend, _token_file, _tmp = running_server
    boundary = "----daisugiTestBoundary"
    wav_bytes = _make_wav()
    body = (
        (
            f"--{boundary}\r\n"
            f'Content-Disposition: form-data; name="audio"; filename="clip.wav"\r\n'
            f"Content-Type: audio/wav\r\n\r\n"
        ).encode()
        + wav_bytes
        + f"\r\n--{boundary}--\r\n".encode()
    )
    conn = _conn(server)
    conn.request(
        "POST",
        "/transcribe",
        body=body,
        headers={"Content-Type": f"multipart/form-data; boundary={boundary}"},
    )
    resp = conn.getresponse()
    assert resp.status == 200
    payload = json.loads(resp.read())
    assert payload["text"] == "hello from the fixture"
    assert engine.calls[-1][:4] == b"RIFF"


def test_oversize_audio_is_413(running_server):
    server, *_ = running_server
    conn = _conn(server)
    oversized = _make_wav(seconds=61.0)
    conn.request("POST", "/transcribe", body=oversized, headers={"Content-Type": "audio/wav"})
    resp = conn.getresponse()
    assert resp.status == 413
    resp.read()


def test_deliver_send_on_an_unarmed_pane_is_refused_with_the_arm_command(running_server):
    server, _engine, backend, _token_file, _tmp = running_server
    conn = _conn(server)
    body = json.dumps({"pane": "p1", "text": "hi", "mode": "send"}).encode()
    conn.request("POST", "/deliver", body=body, headers={"Content-Type": "application/json"})
    resp = conn.getresponse()
    assert resp.status == 403
    payload = json.loads(resp.read())
    assert payload["delivered"] == "refused"
    assert "daisugi voice arm" in payload["reason"]
    assert backend.sent == []


def test_deliver_send_on_an_armed_pane_reaches_the_backend(running_server):
    server, _engine, backend, _token_file, tmp = running_server
    arm("p1", minutes=30, armed_dir=tmp / "armed")
    conn = _conn(server)
    body = json.dumps({"pane": "p1", "text": "hi", "mode": "send"}).encode()
    conn.request("POST", "/deliver", body=body, headers={"Content-Type": "application/json"})
    resp = conn.getresponse()
    assert resp.status == 200
    payload = json.loads(resp.read())
    assert payload["delivered"] == "sent"
    assert len(backend.sent) == 1
    sent_pane, sent_text = backend.sent[0]
    assert sent_pane.backend == "fake"
    assert sent_pane.id == "p1"
    assert sent_text == "hi"


def test_build_server_refuses_off_loopback_without_a_readable_token_file(tmp_path):
    with pytest.raises(RuntimeError, match="refusing to listen"):
        voice_server.build_server(
            "0.0.0.0",
            0,
            config=Config(data_dir=tmp_path),
            engine=FakeEngine(),
            token_file=tmp_path / "no-such-token",
            armed_dir=tmp_path / "armed",
            backend_factory=lambda: FakeBackend(),
        )
```

Note on `PaneRef`: `server.py`'s module-level `try/except ImportError` around `from
opendaisugi.floor.backend import PaneRef` means every test above runs whether or not
`opendaisugi.floor` (spec-01/03) is installed yet — the local fallback dataclass has the
identical `(backend: str, id: str)` shape, so `/deliver` behaves the same either way. Once
plan-01/03 land (the critical path is 00→01→02→03→06→07), the `try` branch quietly takes
over and this is exercised against the real type instead. This is the one place in this
plan where a real cross-plan type is used directly (Task 4's `deliver.py` stays fully
decoupled via `TYPE_CHECKING`); the fallback exists so that decision does not also block
this file's own test suite from running standalone.

- [ ] **Step 6: Run the full server test file to confirm everything passes**

Run: `uv run --no-sync pytest tests/voice/test_server.py -q`
Expected: PASS

- [ ] **Step 7: Run the whole voice suite and ruff, then commit**

Run: `uv run --no-sync pytest tests/voice/ -q -m "not voice_live" && uv run --no-sync ruff check src/opendaisugi/voice/server.py tests/voice/test_server.py`
Expected: PASS, clean

```bash
git add src/opendaisugi/voice/server.py tests/voice/test_server.py
git commit -m "voice: the stdlib HTTP server — /health, /transcribe, /deliver

Off-loopback with no readable token file refuses to start rather than serve
unauthenticated. TOKEN_PATH is a documented cross-plan assumption about
spec-06's coppice-web-token file, overridable with --token-file."
```

---

### Task 6: Push-to-talk — the laptop terminal client

**Files:**
- Create: `src/opendaisugi/voice/ptt.py`
- Test: `tests/voice/test_ptt.py`

**Interfaces:**
- Consumes: nothing from earlier voice tasks except conceptually talking to Task 5's server
  over HTTP (no import — `HttpVoiceClient` is a thin `urllib` wrapper, decoupled by design).
- Produces (consumed by Task 7's `cli.py`):
  - `opendaisugi.voice.ptt.VoiceClient` (Protocol) — `transcribe(wav_bytes: bytes) -> dict`,
    `deliver(pane: str, text: str, *, mode: str) -> dict`
  - `opendaisugi.voice.ptt.HttpVoiceClient(server_url: str, *, timeout_s: float = 30.0)`
  - `opendaisugi.voice.ptt.record_and_send(frames: list[bytes], *, pane: str, client: VoiceClient, sample_rate: int = 16000) -> tuple[str, dict]`
  - `opendaisugi.voice.ptt.run_ptt(pane: str, *, client: VoiceClient, keys: Iterable[str], open_stream: Callable[[], "RecordStream"] | None = None, chunk_frames: int = 1600, print_fn=print) -> list[tuple[str, dict]]`
  - `opendaisugi.voice.ptt.main_loop(pane: str, *, server_url: str, stdin=None) -> None`
    (the real terminal loop — thin, wraps `run_ptt` with real termios/stdin)

- [ ] **Step 1: Write the failing ptt tests**

```python
# tests/voice/test_ptt.py
from __future__ import annotations

import io
import wave

from opendaisugi.voice.ptt import main_loop, record_and_send, run_ptt


class FakeStream:
    def __init__(self, chunks: list[bytes]) -> None:
        self.chunks = list(chunks)
        self.started = False
        self.stopped = False

    def start(self) -> None:
        self.started = True

    def stop(self) -> None:
        self.stopped = True

    def read(self, frames: int) -> tuple[bytes, bool]:
        if not self.chunks:
            return b"", False
        chunk = self.chunks.pop(0)
        return chunk, bool(self.chunks)


class FakeClient:
    def __init__(self, text: str = "hello world") -> None:
        self.text = text
        self.transcribe_calls: list[bytes] = []
        self.deliver_calls: list[tuple[str, str, str]] = []

    def transcribe(self, wav_bytes: bytes) -> dict:
        self.transcribe_calls.append(wav_bytes)
        return {"text": self.text}

    def deliver(self, pane: str, text: str, *, mode: str) -> dict:
        self.deliver_calls.append((pane, text, mode))
        return {"delivered": "preview"}


def test_record_and_send_packages_frames_as_a_16k_mono_wav():
    client = FakeClient()
    text, result = record_and_send([b"\x00\x00" * 100], pane="w1:p1", client=client)
    assert text == "hello world"
    assert result == {"delivered": "preview"}
    wav_bytes = client.transcribe_calls[0]
    assert wav_bytes[:4] == b"RIFF"
    with wave.open(io.BytesIO(wav_bytes), "rb") as w:
        assert w.getframerate() == 16000
        assert w.getnchannels() == 1
    assert client.deliver_calls == [("w1:p1", "hello world", "preview")]


def test_run_ptt_posts_once_per_press_release_cycle():
    client = FakeClient()
    stream = FakeStream([b"\x00\x01" * 800])
    results = run_ptt("w1:p1", client=client, keys=[" ", " ", "q"], open_stream=lambda: stream)
    assert len(client.transcribe_calls) == 1
    assert len(client.deliver_calls) == 1
    assert stream.started and stream.stopped
    assert results == [("hello world", {"delivered": "preview"})]


def test_run_ptt_handles_two_press_release_cycles():
    client = FakeClient()
    streams = [FakeStream([b"\x00\x01" * 400]), FakeStream([b"\x00\x02" * 400])]
    it = iter(streams)
    results = run_ptt(
        "w1:p1", client=client, keys=[" ", " ", " ", " ", "q"], open_stream=lambda: next(it)
    )
    assert len(client.transcribe_calls) == 2
    assert len(results) == 2
    assert all(s.started and s.stopped for s in streams)


def test_run_ptt_quits_immediately_on_q_without_recording():
    client = FakeClient()
    results = run_ptt("w1:p1", client=client, keys=["q"], open_stream=lambda: FakeStream([]))
    assert results == []
    assert client.transcribe_calls == []


def test_main_loop_reads_from_a_fake_non_tty_stdin_and_quits(monkeypatch):
    calls: list[tuple[str, str, str]] = []

    class FixedClient(FakeClient):
        def deliver(self, pane, text, *, mode):
            calls.append((pane, text, mode))
            return {"delivered": "preview"}

    monkeypatch.setattr("opendaisugi.voice.ptt.HttpVoiceClient", lambda url: FixedClient())
    monkeypatch.setattr(
        "opendaisugi.voice.ptt._default_stream", lambda: FakeStream([b"\x00\x00" * 100])
    )
    fake_stdin = io.StringIO("  q")  # not a real tty -> main_loop must skip termios entirely
    main_loop("w1:p1", server_url="http://127.0.0.1:7477", stdin=fake_stdin)
    assert calls == [("w1:p1", "hello world", "preview")]
```

- [ ] **Step 2: Run it to confirm it fails**

Run: `uv run --no-sync pytest tests/voice/test_ptt.py -q`
Expected: FAIL — `ModuleNotFoundError: No module named 'opendaisugi.voice.ptt'`

- [ ] **Step 3: Write `ptt.py`**

```python
# src/opendaisugi/voice/ptt.py
"""Laptop push-to-talk: hold space to record, release to transcribe and deliver.

No global hotkey (spec-07's cruxes: that is the part that costs Handy months). This only
reads keys while its own terminal has focus, in raw mode. `sounddevice` is imported lazily,
only when a press actually starts recording — importing it at module load time can raise
`OSError` on a headless box with no PortAudio device, and this module's own tests (and any
`--help` invocation) must not depend on a working audio device.

The core logic (`run_ptt`) takes its keypresses and its audio stream as parameters, so it is
fully unit-testable without a terminal or a microphone; `main_loop` is the thin, effectively
untestable wrapper that supplies the real ones."""

from __future__ import annotations

import io
import json
import wave
from typing import Callable, Iterable, Protocol
from urllib import request as urllib_request


class RecordStream(Protocol):
    def start(self) -> None: ...
    def stop(self) -> None: ...
    def read(self, frames: int) -> tuple[bytes, bool]:
        """Return (chunk, more_available)."""
        ...


class VoiceClient(Protocol):
    def transcribe(self, wav_bytes: bytes) -> dict: ...
    def deliver(self, pane: str, text: str, *, mode: str) -> dict: ...


class HttpVoiceClient:
    """stdlib `urllib` client for the voice server's /transcribe and /deliver."""

    def __init__(self, server_url: str, *, timeout_s: float = 30.0) -> None:
        self.server_url = server_url.rstrip("/")
        self.timeout_s = timeout_s

    def transcribe(self, wav_bytes: bytes) -> dict:
        req = urllib_request.Request(
            f"{self.server_url}/transcribe",
            data=wav_bytes,
            method="POST",
            headers={"Content-Type": "audio/wav"},
        )
        with urllib_request.urlopen(req, timeout=self.timeout_s) as resp:
            return json.loads(resp.read())

    def deliver(self, pane: str, text: str, *, mode: str) -> dict:
        body = json.dumps({"pane": pane, "text": text, "mode": mode}).encode()
        req = urllib_request.Request(
            f"{self.server_url}/deliver",
            data=body,
            method="POST",
            headers={"Content-Type": "application/json"},
        )
        with urllib_request.urlopen(req, timeout=self.timeout_s) as resp:
            return json.loads(resp.read())


def record_and_send(
    frames: list[bytes],
    *,
    pane: str,
    client: "VoiceClient",
    sample_rate: int = 16000,
) -> tuple[str, dict]:
    """Package recorded 16-bit mono PCM frames as a WAV, transcribe, and deliver (preview by
    default — `run_ptt`/`main_loop` never pass `mode="send"`; arming a pane for direct send is
    a separate, explicit `daisugi voice arm` step). Returns (text, deliver_result)."""
    buf = io.BytesIO()
    with wave.open(buf, "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(sample_rate)
        w.writeframes(b"".join(frames))
    result = client.transcribe(buf.getvalue())
    text = result.get("text", "")
    deliver_result = client.deliver(pane, text, mode="preview")
    return text, deliver_result


def _default_stream() -> "RecordStream":
    import sounddevice as sd  # lazy: raises OSError on a headless box otherwise

    return sd.RawInputStream(samplerate=16000, channels=1, dtype="int16")


def run_ptt(
    pane: str,
    *,
    client: "VoiceClient",
    keys: Iterable[str],
    open_stream: Callable[[], "RecordStream"] | None = None,
    chunk_frames: int = 1600,
    print_fn: Callable[[str], None] = print,
) -> list[tuple[str, dict]]:
    """Drive one push-to-talk session from a sequence of keypresses — real terminal input in
    production (`main_loop`), a fixed list in tests. Space starts/stops a recording; `q`
    quits. Returns the (text, deliver_result) pairs produced, one per full press/release
    cycle."""
    open_stream = open_stream or _default_stream
    results: list[tuple[str, dict]] = []
    recording = False
    stream: "RecordStream | None" = None
    frames: list[bytes] = []
    for ch in keys:
        if ch == "q":
            break
        if ch == " " and not recording:
            recording = True
            frames = []
            stream = open_stream()
            stream.start()
        elif ch == " " and recording:
            recording = False
            more = True
            while more:
                chunk, more = stream.read(chunk_frames)
                if chunk:
                    frames.append(chunk)
            stream.stop()
            text, result = record_and_send(frames, pane=pane, client=client)
            print_fn(text)
            print_fn(str(result.get("delivered", "?")))
            results.append((text, result))
    return results


def main_loop(pane: str, *, server_url: str, stdin=None) -> None:
    """The real interactive loop: raw terminal mode on a real tty, plain char-by-char
    reading otherwise (a fake stdin in a test, or a pipe). Delegates all decision logic to
    `run_ptt`."""
    import sys

    stdin = stdin if stdin is not None else sys.stdin
    client = HttpVoiceClient(server_url)
    is_tty = bool(getattr(stdin, "isatty", lambda: False)())
    old_settings = None
    if is_tty:
        import termios
        import tty

        fd = stdin.fileno()
        old_settings = termios.tcgetattr(fd)
        tty.setraw(fd)
    try:

        def _keys():
            while True:
                ch = stdin.read(1)
                if not ch:
                    return
                yield ch

        run_ptt(pane, client=client, keys=_keys())
    finally:
        if is_tty and old_settings is not None:
            import termios

            termios.tcsetattr(stdin.fileno(), termios.TCSADRAIN, old_settings)
```

- [ ] **Step 4: Run it to confirm it passes**

Run: `uv run --no-sync pytest tests/voice/test_ptt.py -q`
Expected: PASS

- [ ] **Step 5: Run the whole voice suite and ruff, then commit**

Run: `uv run --no-sync pytest tests/voice/ -q -m "not voice_live" && uv run --no-sync ruff check src/opendaisugi/voice/ptt.py tests/voice/test_ptt.py`
Expected: PASS, clean

```bash
git add src/opendaisugi/voice/ptt.py tests/voice/test_ptt.py
git commit -m "voice: push-to-talk — space to record, release to transcribe and deliver

run_ptt() takes its keys and its audio stream as parameters so the press/
release logic is fully unit-tested without a terminal or a microphone;
main_loop() is the thin real-tty wrapper around it."
```

---

### Task 7: `daisugi voice serve|ptt|arm|disarm`, and the how-to doc

**Files:**
- Modify: `src/opendaisugi/cli.py` (add `voice_app` alongside the other hidden Typer
  sub-apps, e.g. near `hook_app`/`gate_app` around line 300-320)
- Create: `docs/how-to/voice.md`
- Test: `tests/test_cli_voice.py`

**Interfaces:**
- Consumes: `opendaisugi.voice.prereq.check_voice_prereqs` (Task 0); `opendaisugi.voice.server.serve`/
  `TOKEN_PATH` (Task 5); `opendaisugi.voice.ptt.main_loop` (Task 6); `opendaisugi.voice.deliver.arm`/
  `disarm` (Task 4); `opendaisugi.config.load_config`, `Config.voice_server_url`.
- Produces: `daisugi voice serve|ptt|arm|disarm` on the CLI (no further consumers within this
  plan — this is the outermost task before the PWA button).

- [ ] **Step 1: Write the failing CLI tests**

```python
# tests/test_cli_voice.py
from __future__ import annotations

import time

from typer.testing import CliRunner

from opendaisugi.cli import app
from opendaisugi.voice.deliver import is_armed

runner = CliRunner()


def test_voice_help_lists_the_four_subcommands():
    result = runner.invoke(app, ["voice", "--help"])
    assert result.exit_code == 0, result.output
    for name in ("serve", "ptt", "arm", "disarm"):
        assert name in result.output


def test_voice_arm_then_disarm(tmp_path):
    result = runner.invoke(
        app, ["voice", "arm", "w1:p1", "--for", "30m", "--data-dir", str(tmp_path)]
    )
    assert result.exit_code == 0, result.output
    assert is_armed("w1:p1", armed_dir=tmp_path / "voice" / "armed")

    result = runner.invoke(app, ["voice", "disarm", "w1:p1", "--data-dir", str(tmp_path)])
    assert result.exit_code == 0, result.output
    assert not is_armed("w1:p1", armed_dir=tmp_path / "voice" / "armed")


def test_voice_arm_accepts_hours(tmp_path):
    before = time.time()
    result = runner.invoke(
        app, ["voice", "arm", "w1:p1", "--for", "2h", "--data-dir", str(tmp_path)]
    )
    assert result.exit_code == 0, result.output
    assert is_armed("w1:p1", armed_dir=tmp_path / "voice" / "armed", now=before + 3600)
    assert not is_armed("w1:p1", armed_dir=tmp_path / "voice" / "armed", now=before + 3 * 3600)


def test_voice_serve_exits_3_when_faster_whisper_is_missing(monkeypatch):
    import opendaisugi.voice.prereq as prereq_mod

    monkeypatch.setattr(
        prereq_mod,
        "check_voice_prereqs",
        lambda: prereq_mod.PrereqResult(
            faster_whisper_available=False, ffmpeg_available=True, espeak_available=True
        ),
    )
    result = runner.invoke(app, ["voice", "serve"])
    assert result.exit_code == 3
    assert "pip install 'opendaisugi[voice]'" in result.output
```

- [ ] **Step 2: Run it to confirm it fails**

Run: `uv run --no-sync pytest tests/test_cli_voice.py -q`
Expected: FAIL — `AssertionError` (no `voice` command group registered yet; typer prints
"No such command 'voice'")

- [ ] **Step 3: Wire the `voice_app` into `cli.py`**

Find the block that defines `hook_app`/`gate_app` (around line 300-320 — see
`app.add_typer(gate_app, name="gate", rich_help_panel="Gate")`) and add, immediately after
the `gate_app` block:

```python
voice_app = typer.Typer(
    name="voice",
    help="The voice bridge: record anywhere, transcribe on this box, land the text in a pane.",
    no_args_is_help=True,
)
app.add_typer(voice_app, name="voice", hidden=True)


def _parse_minutes(spec: str) -> float:
    """'30m' -> 30.0, '2h' -> 120.0, '90' -> 90.0 (a bare number means minutes)."""
    spec = spec.strip().lower()
    if spec.endswith("m"):
        return float(spec[:-1])
    if spec.endswith("h"):
        return float(spec[:-1]) * 60.0
    return float(spec)


@voice_app.command("serve")
def voice_serve_cmd(
    host: str = typer.Option("127.0.0.1", "--host", help="Bind address."),
    port: int = typer.Option(7477, "--port", help="Bind port."),
    listen: str = typer.Option(
        None, "--listen", help="host:port for the tailnet, e.g. 0.0.0.0:7477. Needs a token file."
    ),
    token_file: Path = typer.Option(
        None, "--token-file", help="Defaults to the coppice web token."
    ),
    tls_cert: Path = typer.Option(None, "--tls-cert"),
    tls_key: Path = typer.Option(None, "--tls-key"),
    data_dir: Path = typer.Option(DEFAULT_DATA_DIR, "--data-dir", help="Daisugi data directory."),
) -> None:
    """Run the voice bridge: POST /transcribe, POST /deliver, GET /health."""
    from opendaisugi.config import load_config
    from opendaisugi.voice import server as voice_server
    from opendaisugi.voice.prereq import check_voice_prereqs

    result = check_voice_prereqs()
    if not result.faster_whisper_available:
        typer.echo("faster-whisper is not installed.", err=True)
        typer.echo("The voice bridge needs it to transcribe audio.", err=True)
        typer.echo("Fix: pip install 'opendaisugi[voice]'", err=True)
        raise typer.Exit(code=3)
    if listen:
        host, _, port_str = listen.partition(":")
        port = int(port_str)
    config = load_config().model_copy(update={"data_dir": data_dir})
    resolved_token_file = token_file or voice_server.TOKEN_PATH
    typer.echo(f"opendaisugi voice  →  listening on http://{host}:{port}")
    try:
        voice_server.serve(
            host=host,
            port=port,
            config=config,
            token_file=resolved_token_file,
            tls_cert=tls_cert,
            tls_key=tls_key,
        )
    except RuntimeError as exc:
        typer.echo(str(exc), err=True)
        raise typer.Exit(code=3)


@voice_app.command("ptt")
def voice_ptt_cmd(
    pane: str = typer.Option(..., "--pane", help="The pane id to deliver text to."),
    server: str = typer.Option(None, "--server", help="Defaults to voice_server_url from config."),
) -> None:
    """Laptop push-to-talk: hold space to record, release to transcribe and deliver."""
    from opendaisugi.config import load_config
    from opendaisugi.voice.ptt import main_loop

    config = load_config()
    server_url = server or config.voice_server_url
    typer.echo(f"push-to-talk on {pane}  ({server_url})  —  space to record, q to quit")
    main_loop(pane, server_url=server_url)


@voice_app.command("arm")
def voice_arm_cmd(
    pane: str = typer.Argument(..., help="The pane id to grant direct-send to."),
    for_: str = typer.Option("30m", "--for", help="How long, e.g. 30m or 2h."),
    data_dir: Path = typer.Option(DEFAULT_DATA_DIR, "--data-dir", help="Daisugi data directory."),
) -> None:
    """Grant PANE direct-send for a time window. Without this, delivered text only previews."""
    from opendaisugi.voice.deliver import arm

    minutes = _parse_minutes(for_)
    entry = arm(pane, minutes=minutes, armed_dir=data_dir / "voice" / "armed")
    until = time.strftime("%H:%M:%S", time.localtime(entry.expires_at))
    typer.echo(f"{pane} armed for {minutes:.0f}m (until {until}).")


@voice_app.command("disarm")
def voice_disarm_cmd(
    pane: str = typer.Argument(..., help="The pane id to revoke direct-send from."),
    data_dir: Path = typer.Option(DEFAULT_DATA_DIR, "--data-dir", help="Daisugi data directory."),
) -> None:
    """Revoke PANE's direct-send grant. Delivered text goes back to preview-only."""
    from opendaisugi.voice.deliver import disarm

    disarm(pane, armed_dir=data_dir / "voice" / "armed")
    typer.echo(f"{pane} disarmed.")
```

Add `import time` to `cli.py`'s existing top-of-file import block if it is not already there
(check first — several commands already print timestamps).

- [ ] **Step 4: Run it to confirm it passes**

Run: `uv run --no-sync pytest tests/test_cli_voice.py -q`
Expected: PASS

- [ ] **Step 5: Write `docs/how-to/voice.md`**

```markdown
# Record anywhere, land it in a pane

`daisugi voice` turns any recording — your phone in the kitchen, your laptop's mic — into
text in the pane you chose. You always see the text before the pane's agent does, unless you
explicitly arm that pane.

## Turn it on

```bash
uv add 'opendaisugi[voice]'
daisugi voice serve
```

This starts a server on `http://127.0.0.1:7477` with three endpoints: `GET /health`, `POST
/transcribe`, `POST /deliver`. It downloads the `tiny.en` speech model on first use (about
78 MB) unless you already have it cached.

## From the laptop

```bash
daisugi voice ptt w1:p1
```

Hold `space` to record, release to transcribe and preview it in pane `w1:p1`. Press `q` to
quit.

## From the phone

Open the phone PWA (see `docs/how-to/phone.md`), open a pane, and tap the record button next
to the prompt box. The same preview rule applies.

## Direct send, if you want it

By default, delivered text goes to the pane's prompt box for you to read and send yourself.
To let it go straight in:

```bash
daisugi voice arm w1:p1 --for 30m
```

Direct send stays off until you run this, and it expires on its own. Revoke it early with
`daisugi voice disarm w1:p1`.

## Cleaning up the transcript

Off by default. Turn it on in `~/.opendaisugi/config.yaml`:

```yaml
voice_cleanup: true
voice_cleanup_model: ollama/llama3.2:3b
```

This runs one fixed-prompt call on the model you name — never a paid model unless you name
one. It never changes what you said, only fixes punctuation and obvious transcription slips.

## CPU and GPU

`voice_device` stays `cpu` by default. Setting it to `cuda` only takes effect if a CUDA
device is actually visible; otherwise it silently runs on CPU. This is deliberate: some GPUs
have crashed other local model inference under CUDA, and a working CPU path beats a broken
GPU one every time.
```

- [ ] **Step 6: Run the whole test suite touched by this task and ruff, then commit**

Run: `uv run --no-sync pytest tests/test_cli_voice.py tests/voice/ -q -m "not voice_live" && uv run --no-sync ruff check src/opendaisugi/cli.py`
Expected: PASS, clean

```bash
git add src/opendaisugi/cli.py docs/how-to/voice.md tests/test_cli_voice.py
git commit -m "voice: wire daisugi voice serve|ptt|arm|disarm into the CLI

serve exits 3 (unreachable) with a teaching message when faster-whisper is
not installed, matching the coppice backend's own exit-3 convention."
```

---

### Task 8: The phone's record button

**Files:**
- Create: `harness/coppice/internal/web/static/record.js`
- Create: `tests/voice/fixtures/record_button_harness.html`
- Test: `tests/voice/test_record_button.py`

**Interfaces:**
- Consumes: nothing from this plan's Python code — a standalone browser module.
- Produces: `initRecordButton(container, opts)` (JS, global function; also exported as a
  CommonJS module for the test harness to `require`, and left as a bare global for a real
  page's `<script>` tag to call directly) where
  `opts = { serverUrl: string, token: string, onText: (text: string) => void, onError?: (message: string) => void }`.
  This is the documented one-line integration point for spec-06's Pane screen (not modified
  by this plan — that file does not exist in this repo yet).

- [ ] **Step 1: Write `record.js`**

```javascript
// harness/coppice/internal/web/static/record.js
//
// The voice bridge's record button (spec-07). Self-contained: touches nothing else on the
// page. spec-06's Pane screen is expected to call this once, next to its Prompt/Steer
// buttons:
//
//   initRecordButton(document.getElementById("prompt-actions"), {
//     serverUrl: settings.voiceServerUrl,
//     token: settings.token,
//     onText: (text) => { promptBox.value = text; },
//     onError: (message) => { showToast(message); },
//   });

function initRecordButton(container, opts) {
  const button = document.createElement("button");
  button.type = "button";
  button.className = "voice-record-button";
  button.textContent = "\u{1F3A4} Record";
  container.appendChild(button);

  let recorder = null;
  let chunks = [];
  let recording = false;

  button.addEventListener("click", async () => {
    if (!recording) {
      try {
        const stream = await navigator.mediaDevices.getUserMedia({ audio: true });
        chunks = [];
        recorder = new MediaRecorder(stream);
        recorder.addEventListener("dataavailable", (e) => {
          if (e.data && e.data.size > 0) chunks.push(e.data);
        });
        recorder.addEventListener("stop", () => onStop(stream));
        recorder.start();
        recording = true;
        button.textContent = "⏹ Stop";
      } catch (err) {
        if (opts.onError) opts.onError(String(err));
      }
    } else {
      recorder.stop();
      recording = false;
      button.textContent = "\u{1F3A4} Record";
    }
  });

  async function onStop(stream) {
    stream.getTracks().forEach((t) => t.stop());
    const blob = new Blob(chunks, { type: "audio/webm" });
    try {
      const resp = await fetch(opts.serverUrl.replace(/\/$/, "") + "/transcribe", {
        method: "POST",
        headers: { Authorization: "Bearer " + opts.token },
        body: blob,
      });
      if (!resp.ok) {
        if (opts.onError) opts.onError("transcribe failed: " + resp.status);
        return;
      }
      const data = await resp.json();
      opts.onText(data.text || "");
    } catch (err) {
      if (opts.onError) opts.onError(String(err));
    }
  }

  return button;
}

if (typeof module !== "undefined" && module.exports) {
  module.exports = { initRecordButton };
}
```

- [ ] **Step 2: Write the test harness page**

```html
<!-- tests/voice/fixtures/record_button_harness.html -->
<!DOCTYPE html>
<html>
  <body>
    <div id="container"></div>
    <div id="result"></div>
    <script src="/record.js"></script>
    <script>
      window.__lastText = null;
      window.__lastError = null;
      initRecordButton(document.getElementById("container"), {
        serverUrl: "http://localhost:9999",
        token: "test-token",
        onText: (text) => {
          window.__lastText = text;
          document.getElementById("result").textContent = text;
        },
        onError: (msg) => {
          window.__lastError = msg;
        },
      });
    </script>
  </body>
</html>
```

- [ ] **Step 3: Write the failing Playwright test**

```python
# tests/voice/test_record_button.py
from __future__ import annotations

import functools
import http.server
import shutil
import threading

import pytest

playwright_sync = pytest.importorskip(
    "playwright.sync_api",
    reason="playwright not installed — pip install playwright && playwright install chromium",
)

from pathlib import Path

RECORD_JS = (
    Path(__file__).resolve().parents[2]
    / "harness"
    / "coppice"
    / "internal"
    / "web"
    / "static"
    / "record.js"
)
HARNESS_HTML = Path(__file__).resolve().parent / "fixtures" / "record_button_harness.html"

# Overrides getUserMedia/MediaRecorder before any page script runs — headless Chromium has
# no real microphone, and this test only needs to prove the wiring (click -> POST
# /transcribe -> onText), not real audio capture.
_FAKE_MEDIA_JS = """
navigator.mediaDevices = navigator.mediaDevices || {};
navigator.mediaDevices.getUserMedia = async () => ({
  getTracks: () => [{ stop: () => {} }],
});
window.MediaRecorder = class {
  constructor(stream) { this._listeners = {}; }
  addEventListener(name, fn) { this._listeners[name] = fn; }
  start() {}
  stop() {
    if (this._listeners.dataavailable) {
      this._listeners.dataavailable({ data: new Blob(["x"]) });
    }
    if (this._listeners.stop) this._listeners.stop();
  }
};
"""


@pytest.fixture
def static_server(tmp_path):
    root = tmp_path / "static"
    root.mkdir()
    (root / "record.js").write_bytes(RECORD_JS.read_bytes())
    (root / "record_button_harness.html").write_bytes(HARNESS_HTML.read_bytes())
    handler = functools.partial(http.server.SimpleHTTPRequestHandler, directory=str(root))
    httpd = http.server.ThreadingHTTPServer(("127.0.0.1", 0), handler)
    thread = threading.Thread(target=httpd.serve_forever, daemon=True)
    thread.start()
    host, port = httpd.server_address[:2]
    yield f"http://{host}:{port}"
    httpd.shutdown()
    thread.join(timeout=5)


def test_record_button_posts_a_recording_and_fills_in_the_text(static_server):
    from playwright.sync_api import sync_playwright

    with sync_playwright() as p:
        if shutil.which("chromium") is None and not p.chromium.executable_path:
            pytest.skip("no Chromium executable for Playwright on this host")
        browser = p.chromium.launch()
        page = browser.new_page()
        page.add_init_script(_FAKE_MEDIA_JS)
        page.route(
            "**/transcribe",
            lambda route: route.fulfill(
                status=200,
                content_type="application/json",
                body='{"text": "hello from the fixture"}',
            ),
        )
        page.goto(f"{static_server}/record_button_harness.html")
        page.click("button.voice-record-button")  # start
        page.wait_for_timeout(50)
        page.click("button.voice-record-button")  # stop -> triggers dataavailable+stop -> POST
        page.wait_for_function("window.__lastText !== null", timeout=5000)
        assert page.evaluate("window.__lastText") == "hello from the fixture"
        browser.close()
```

- [ ] **Step 4: Run it**

Run: `uv run --no-sync pytest tests/voice/test_record_button.py -q`
Expected: SKIP with reason "playwright not installed" if `playwright` is not installed in
this environment (expected on a fresh checkout — this test is intentionally not part of the
`dev` extra, matching the `smoke`/`calibration`/`voice_live` opt-in convention). If
`playwright` and a Chromium build are both present (`pip install playwright && playwright
install chromium`), it runs for real and must PASS.

- [ ] **Step 5: Run the whole voice suite and ruff, then commit**

Run: `uv run --no-sync pytest tests/voice/ -q -m "not voice_live"`
Expected: PASS (with the record-button test skipping if Playwright is absent)

```bash
git add harness/coppice/internal/web/static/record.js tests/voice/fixtures/record_button_harness.html \
        tests/voice/test_record_button.py
git commit -m "voice: the phone record button (record.js) and its Playwright smoke test

Standalone with its own test harness page rather than a patch to spec-06's
app.js/index.html, which do not exist in this repo yet — the integration
point (initRecordButton) is documented in the file's header comment."
```

---

## Post-plan check (run once, after all nine tasks)

```bash
uv run --no-sync pytest -q -m "not voice_live and not smoke and not calibration"
uv run --no-sync ruff check .
uv run --no-sync ruff format --check .
```

Expected: everything green. The three opt-in marker families (`voice_live`, `smoke`,
`calibration`) are unaffected by this plan except for the one new `voice_live` test added in
Task 2 — run it once by hand with `DAISUGI_VOICE_LIVE=1` to confirm the recorded WER ceilings
still hold on the machine you're building on.
