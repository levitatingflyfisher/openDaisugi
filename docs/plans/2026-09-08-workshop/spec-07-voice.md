# Spec 07 — The voice bridge

**Master:** §5.11, §3.6 (delivery rule) · **Size:** M · **Depends on:** 03 (delivery through a
PaneBackend), 06 (the record button)
**Prerequisite (plan task 0):** a box with a GPU that Python can see, or acceptance of CPU speed.

## Purpose

Record anywhere, transcribe on a box you own, land the text in the pane you chose. The phone and
the laptop are recorders. The transcriber is a commodity. The bridge is the product.

## The cruxes

- **Prompt, not send.** By default the text lands in the pane's prompt box on the client that
  asked, where the operator sees it before the loop does. Direct delivery is an explicit per-pane
  arm (`--arm`), because voice transcription errors are exactly the input a fail-closed system
  should not feed an agent unseen.
- **Cleanup is a pathway.** The optional polish pass is one gateway call with a fixed prompt on
  a local model. It is journaled like any other turn, so the garden can distil it into a frozen
  pathway; the bridge does not special-case it.
- **No global hotkey in v1.** Wayland hotkeys are the part that costs Handy months. The laptop
  push-to-talk is a tiny terminal program: hold `space` to record, release to send. Handy exists
  for people who want a global key.

## Files

```
src/opendaisugi/voice/__init__.py
src/opendaisugi/voice/engines.py        # Engine protocol; FasterWhisperEngine; ParakeetEngine (sherpa-onnx); pick_engine()
src/opendaisugi/voice/server.py         # stdlib http.server: POST /transcribe, POST /deliver, GET /health
src/opendaisugi/voice/cleanup.py        # gateway call with the fixed prompt; returns text; journaled
src/opendaisugi/voice/deliver.py        # to a pane: prompt-preview (default) | armed send; via PaneBackend
src/opendaisugi/voice/ptt.py            # laptop push-to-talk: sounddevice + Silero VAD; posts to /transcribe
src/opendaisugi/cli.py                  # `daisugi voice serve|ptt|arm|disarm`
src/opendaisugi/config.py               # voice: {engine, model, cleanup: bool, cleanup_model, server_url}
pyproject.toml                          # [voice] = faster-whisper>=1.1, sounddevice>=0.5, numpy ; [voice-parakeet] = sherpa-onnx>=1.10
harness/coppice/internal/web/static/    # record button (MediaRecorder → POST /transcribe → prompt box)
tests/voice/…                            # fixtures: 3 short WAVs with expected transcripts
```

## Interfaces

```python
class Engine(Protocol):
    name: str
    def transcribe(self, wav_16k_mono: bytes, *, language: str | None) -> Transcript   # text, segments, duration_s, rtf
def pick_engine(config) -> Engine   # faster-whisper on CUDA if available, else CPU; parakeet when configured and importable
```

`POST /transcribe` (multipart or raw `audio/webm|wav`, ≤ 60 s) → `{text, duration_s, rtf,
engine, cleaned: bool}`; `?cleanup=1` runs `cleanup.py`. `POST /deliver` `{pane, backend?, text,
mode: "preview"|"send"}` → `{delivered: "preview"|"sent"|"refused", reason?}`; `send` is refused
unless the pane is armed. Arming: `daisugi voice arm PANE [--for 30m]` writes an entry under
`~/.opendaisugi/voice/armed/` with an expiry; the floor shows an `armed` badge.

Audio handling: ffmpeg if present for webm → wav, else `av`/`soundfile` fallback via the
`[voice]` extra; 16 kHz mono. The server binds `127.0.0.1` by default; `--listen 0.0.0.0:7477`
for the tailnet, with the same bearer token as spec-06 (`coppice web token` value, read from the
0600 file) required on every request when not on loopback.

Cleanup prompt (fixed, versioned in `cleanup.py` as `CLEANUP_PROMPT_V1`): "Fix punctuation and
obvious transcription errors. Keep the meaning and the words. Return only the corrected text."
The call goes through `daisugi gateway`'s local endpoint when running, else directly to the
configured local backend; never to a paid model unless `cleanup_model` names one.

Push-to-talk: `daisugi voice ptt --pane PANE [--server URL]`. Holds the terminal in raw mode;
space down = record, space up = stop, transcribe, deliver (preview by default); `q` quits. Prints
the text and the delivery result on one line each.

## Tests

- Engine: the three fixture WAVs → expected transcripts with WER ≤ 0.15 on CPU tiny/base
  (a marker skips the GPU-sized model without CUDA).
- Server: `/transcribe` with a WAV fixture → text; oversize (61 s) → 413; bad token off-loopback
  → 401; `/deliver mode=send` on an unarmed pane → refused with the arm command; armed → sent
  through a fake backend; expiry honoured.
- Cleanup: a fake gateway transport returns a fixed correction; the journal has one turn tagged
  `voice.cleanup`.
- PTT: keypress simulation via a fake `sounddevice` stream; posts once per press/release.
- PWA button: Playwright records a synthetic `MediaRecorder` blob and sees the text in the box.

## Out of scope

Global hotkeys; speaker diarisation; streaming transcription; TTS.
