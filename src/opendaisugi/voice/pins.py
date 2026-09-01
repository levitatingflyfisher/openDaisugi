"""Pinned upstream facts for the voice engines. Recorded once here. Read everywhere else.

Recorded 2026-09-08. The faster-whisper facts come from the project README at
github.com/SYSTRAN/faster-whisper. They also come from a live probe on this box. The
probe ran tiny.en on cpu with int8. It generated three espeak-ng fixtures and resampled
them to 16 kHz mono with ffmpeg. The WER numbers below are exactly what that run
produced. They are not an estimate.

The sherpa-onnx facts come from the Parakeet-TDT-0.6B-v2 example at
k2-fsa.github.io/sherpa/onnx/pretrained_models/offline-transducer/nemo-transducer-models.html.
They also come from inspect.signature(sherpa_onnx.OfflineRecognizer.from_transducer) on
sherpa-onnx 1.13.7. That call confirmed the model_type keyword this repo's engine needs.

Re-verify these facts before bumping any dependency version. A later faster-whisper or
ctranslate2 release could change tiny.en's decode. That change could raise the observed
WER without being a regression.
"""

from __future__ import annotations

# A curated allowlist of the CPU-sized model names the CLI offers. Not the
# full set faster-whisper recognizes. See faster_whisper.utils._MODELS for
# the complete upstream list.
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
FASTER_WHISPER_TEST_MODEL = "tiny.en"  # roughly 78MB, HF repo Systran/faster-whisper-tiny.en

# Recorded from a real run. tiny.en on cpu with int8. espeak-ng 1.51.1 synthesized the
# fixtures. ffmpeg resampled them to 16 kHz mono.
FIXTURE_TRANSCRIPTS = {
    "fixture_a.wav": "the quick brown fox jumps over the lazy dog",
    "fixture_b.wav": "please remember to buy milk after work",
    "fixture_c.wav": "what is the weather like today",
}

# The word error rate that live run actually measured for each fixture.
FIXTURE_OBSERVED_WER = {
    "fixture_a.wav": 0.111,  # transcribed "jump" for "jumps"
    "fixture_b.wav": 0.000,
    "fixture_c.wav": 0.000,
}

# Per-fixture ceilings carry headroom over the observed WER for cross-machine and
# cross-version variance. Every ceiling stays at or under 0.15.
FIXTURE_MAX_WER = {
    "fixture_a.wav": 0.15,
    "fixture_b.wav": 0.10,
    "fixture_c.wav": 0.10,
}
FIXTURE_MEAN_MAX_WER = 0.15  # the observed mean of 0.037 clears this easily

PARAKEET_MODEL_ARCHIVE = "sherpa-onnx-nemo-parakeet-tdt-0.6b-v2-int8"
PARAKEET_MODEL_URL = (
    "https://github.com/k2-fsa/sherpa-onnx/releases/download/asr-models/"
    "sherpa-onnx-nemo-parakeet-tdt-0.6b-v2-int8.tar.bz2"
)
PARAKEET_FILES = ("encoder.int8.onnx", "decoder.int8.onnx", "joiner.int8.onnx", "tokens.txt")
PARAKEET_MODEL_TYPE = "nemo_transducer"
