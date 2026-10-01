"""Pinned upstream facts for the voice engines. Recorded once here. Read everywhere else.

Recorded 2026-09-08. The faster-whisper facts come from the project README at
github.com/SYSTRAN/faster-whisper. They also come from a live probe on this box. The
probe ran tiny.en on cpu with int8. It generated three espeak-ng fixtures and resampled
them to 16 kHz mono with ffmpeg. The WER numbers below are exactly what that run
produced. They are not an estimate.

Re-verify these facts before bumping any dependency version. A later faster-whisper or
ctranslate2 release could change tiny.en's decode. That change could raise the observed
WER without being a regression.
"""

from __future__ import annotations

from typing import NamedTuple

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

# The Hugging Face commit each model of the allowlist is fetched at. A
# commit fixes every file of the repo, the LFS digest of model.bin included.
# tiny.en's is the commit the observed WER below was measured on
# (2026-09-08); the others are each repo's main on 2026-10-08. "turbo" is
# the repo faster-whisper names (mobiuslabsgmbh/...), which Hugging Face
# now redirects to dropbox-dash/faster-whisper-large-v3-turbo.
FASTER_WHISPER_REVISIONS = {
    "tiny": "d90ca5fe260221311c53c58e660288d3deb8d356",
    "tiny.en": "0d3d19a32d3338f10357c0889762bd8d64bbdeba",
    "base": "ebe41f70d5b6dfa9166e2c581c45c9c0cfc57b66",
    "base.en": "3d3d5dee26484f91867d81cb899cfcf72b96be6c",
    "small": "536b0662742c02347bc0e980a01041f333bce120",
    "small.en": "d1d751a5f8271d482d14ca55d9e2deeebbae577f",
    "medium": "08e178d48790749d25932bbc082711ddcfdfbc4f",
    "medium.en": "a29b04bd15381511a9af671baec01072039215e3",
    "large-v2": "f0fe81560cb8b68660e564f55dd99207059c092e",
    "large-v3": "edaa852ec7e145841d8ffdb056a99866b5f0a478",
    "distil-large-v3": "c3058b475261292e64a0412df1d2681c06260fab",
    "turbo": "0a363e9161cbc7ed1431c9597a8ceaf0c4f78fcf",
}
# Bytes fetched for a model, for the one first-use line.
FASTER_WHISPER_SIZES = {"tiny.en": 78_090_594}

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

# whisper.cpp facts, recorded 2026-10-01 from examples/cli/cli.cpp at
# github.com/ggml-org/whisper.cpp (master). The binary is whisper-cli. -m names
# the ggml model file and -f the input clip. -l sets the language. Its default
# is "en", and "auto" detects it, which matches faster-whisper's language=None.
# -nt prints no timestamps, so each segment's text goes to stdout as it is.
# -np prints nothing else to stdout. Logs go to stderr. A missing model file
# exits 3. The clip goes through a temp file, not stdin, since older builds
# read no "-" input.
WHISPER_CPP_BINARY = "whisper-cli"
WHISPER_CPP_AUTO_LANGUAGE = "auto"
WHISPER_CPP_TIMEOUT_S = 120.0

# Moonshine facts, recorded 2026-10-01 (ruling VO-13). Moonshine AI (formerly
# Useful Sensors, US) publishes the models under the MIT license: the LICENSE
# of github.com/moonshine-ai/moonshine at v0.1.5 (commit 234f60fa) names every
# streaming model and every English model MIT, and the Hugging Face cards
# moonshine-ai/moonshine-streaming-{tiny,small,medium} say "license: mit".
# The runtime at that commit loads the quantized .ort files its own model
# catalog names on download.moonshine.ai, not the Hugging Face layout (which
# has no tokenizer.bin), so the files come from there, under the dated
# directory below. Each file's size and CRC32C matched the catalog's
# (core/moonshine-model-file-metadata.generated.cpp at that commit) when it
# was first downloaded; the sha256 below was taken then. The word-timestamp
# decoder (decoder_kv_with_attention.ort) is not fetched: moonshine-cli
# never asks for word timestamps.
#
# moonshine-cli is this repo's own program (clients/native/moonshine-cli), on
# Moonshine's C API: -m MODEL_DIR -a tiny|small|medium -f CLIP. At v0.1.5 the
# arch only chooses the streaming decoder, so a model directory named by
# path runs as MOONSHINE_DIR_ARCH. It prints each transcript line on stdout.
MOONSHINE_BINARY = "moonshine-cli"
MOONSHINE_TIMEOUT_S = 120.0
MOONSHINE_DEFAULT_MODEL = "small"
MOONSHINE_DIR_ARCH = "small"
MOONSHINE_BASE_URL = "https://download.moonshine.ai/model"
# A test moves the download host with this variable; nothing else reads it.
MOONSHINE_BASE_URL_ENV = "OPENDAISUGI_MOONSHINE_BASE_URL"
MOONSHINE_REVISION = "quantized_26_08_21"


class MoonshineFile(NamedTuple):
    name: str
    sha256: str
    size: int


MOONSHINE_MODELS: dict[str, tuple[MoonshineFile, ...]] = {
    "tiny": (
        MoonshineFile(
            "adapter.ort",
            "22ecc949e146c49667fda28d102d4e30749a107dc88a396292aa8f277ef1347c",
            1_319_664,
        ),
        MoonshineFile(
            "cross_kv.ort",
            "143a36667b8d05fd9d04e8c337b7ee121f37ef299aea6b3d82bdb3d3401950b4",
            1_287_544,
        ),
        MoonshineFile(
            "decoder_kv.ort",
            "8852553f312adb6c9aa4d17418015049b30f412209ee569d336548c0044627de",
            32_583_720,
        ),
        MoonshineFile(
            "encoder.ort",
            "a8414e1a5dedf9f2093d7680601dd8a9b0433e7020260eafe0e370ead91134ca",
            7_675_440,
        ),
        MoonshineFile(
            "frontend.model.ort",
            "5121b561417b638afce0c6c31b760e37c93cf97f80d9b0031aad1fe7b6f25d61",
            23_344,
        ),
        MoonshineFile(
            "frontend.weights.ort",
            "217da24ac6f522ebf02da8ef288e77d1ac68d50d4a6821433182e4fbf4204bbd",
            2_093_464,
        ),
        MoonshineFile(
            "streaming_config.json",
            "74fe5ddebd63b17caf59e8a3b18c17547ff7bce1642050edbb1c3962674f8950",
            509,
        ),
        MoonshineFile(
            "tokenizer.bin",
            "6884b35fd6377d4c4d32336a0bc152f36b64d1e45b6503683cdc238250a8472d",
            249_974,
        ),
    ),
    "small": (
        MoonshineFile(
            "adapter.ort",
            "c665f742364febad597cc9ac1e0b341ffbee0e24a1466e2f3bde95e6e4771762",
            2_870_368,
        ),
        MoonshineFile(
            "cross_kv.ort",
            "e2d3417144e9514055ebfefe8dcc4c0a55a55adcb8530435844c75c53e352bf6",
            5_356_536,
        ),
        MoonshineFile(
            "decoder_kv.ort",
            "1a05465b1dd955858dfcbee039c0020fb5dd982b0f5094c34e61735d518d771b",
            81_878_600,
        ),
        MoonshineFile(
            "encoder.ort",
            "2d4d973e91e8aca08c51e7e7efa28a46ab265b63d809d5294d18b86bcd85b993",
            44_148_576,
        ),
        MoonshineFile(
            "frontend.model.ort",
            "09b1210ae30dc5f0f3e45f0ebab914c254741323114f53fbbe5ae62cca35058f",
            26_944,
        ),
        MoonshineFile(
            "frontend.weights.ort",
            "7ef97521bd4bad3928f5bb6808586f4fcc6e92bd5990394112eed7d4052ec338",
            7_769_464,
        ),
        MoonshineFile(
            "streaming_config.json",
            "26f02b6afb22d60871a5efd85c3d38e569cc0ddb6c5eb6e93d3260152ae8a47a",
            512,
        ),
        MoonshineFile(
            "tokenizer.bin",
            "6884b35fd6377d4c4d32336a0bc152f36b64d1e45b6503683cdc238250a8472d",
            249_974,
        ),
    ),
    "medium": (
        MoonshineFile(
            "adapter.ort",
            "3f2a287def57cc094367a0eec3c4f5fc36a32ec420e86764b696920991b20281",
            3_651_296,
        ),
        MoonshineFile(
            "cross_kv.ort",
            "642f6e21cd305be79342207c6f9e6b681d469d55bc48c72b27b84846fb71fd1e",
            11_643_776,
        ),
        MoonshineFile(
            "decoder_kv.ort",
            "193bb366492b74fc4ad338c6778e8d8eb916aaa11b5aa264f9057f4db7759486",
            146_972_408,
        ),
        MoonshineFile(
            "encoder.ort",
            "12915e76ebac7dd287c5ea63965d06103a53ba1ce242a4a34f318f3958c60c37",
            94_705_376,
        ),
        MoonshineFile(
            "frontend.model.ort",
            "95768855c70c8251eeecc05fedf69999da1b8ab16f605c9f457fd3354b0ad6b5",
            28_720,
        ),
        MoonshineFile(
            "frontend.weights.ort",
            "5ac941f490cbe035b335b99a414cc393d62d4c6f9f2423495b286870d271d709",
            11_889_560,
        ),
        MoonshineFile(
            "streaming_config.json",
            "28e83b7a28e91472692a035e0dae3116422ae43aeb2bef5ed822c44ce89b88af",
            513,
        ),
        MoonshineFile(
            "tokenizer.bin",
            "6884b35fd6377d4c4d32336a0bc152f36b64d1e45b6503683cdc238250a8472d",
            249_974,
        ),
    ),
}

# Parakeet facts, recorded 2026-10-01 (rulings VO-16 and VO-17). The model is
# NVIDIA's Parakeet-TDT-0.6B v2 (CC-BY-4.0, huggingface.co/nvidia/
# parakeet-tdt-0.6b-v2), converted from NVIDIA's original .nemo by
# CrispASR's converter at the pinned commit (scripts/parakeet_convert.py
# is the recipe). That F16 GGUF is byte for byte the file
# cstr/parakeet-tdt-0.6b-v2-GGUF publishes, so the voice server fetches it
# from there, checks its sha256, makes the Q4_K file with parakeet-quantize
# (built beside parakeet-cli by native.sh --parakeet), checks that sha256
# too, and deletes the F16 file. Nothing is published by this project.
#
# parakeet-cli is this repo's own program (clients/native/parakeet-cli) on
# CrispASR's Parakeet code and the ggml CPU backend:
# -m MODEL.gguf --resident. It is English only, so no language is passed.
PARAKEET_BINARY = "parakeet-cli"
PARAKEET_QUANTIZE_BINARY = "parakeet-quantize"
PARAKEET_DEFAULT_MODEL = "v2"
# The repo's commit of 2026-07-11 (main on 2026-10-08); the F16 file is the
# same at every commit since 2026-05-20.
PARAKEET_REVISION = "8878172f3c45ba231fac8cfd4aff2542b13bc875"
PARAKEET_BASE_URL = (
    f"https://huggingface.co/cstr/parakeet-tdt-0.6b-v2-GGUF/resolve/{PARAKEET_REVISION}"
)
# A test moves the download host with this variable; nothing else reads it.
PARAKEET_BASE_URL_ENV = "OPENDAISUGI_PARAKEET_BASE_URL"
PARAKEET_QUANT = "q4_k"
# The machines whose parakeet-quantize result is pinned: the Q4_K sha256
# below was made on x86_64. No aarch64 result has been made yet (this
# project's build box has no aarch64 CPU and no emulator), so on aarch64
# the curated model is refused until one is made and pinned; a GGUF file
# named by path still runs. docs/how-to/voice.md says how to make it.
PARAKEET_QUANT_ARCHES = ("x86_64",)


class ParakeetFile(NamedTuple):
    name: str
    sha256: str
    size: int


class ParakeetModel(NamedTuple):
    dir: str
    source: ParakeetFile  # the F16 file that is fetched
    quantized: ParakeetFile  # the Q4_K file parakeet-quantize makes from it


PARAKEET_MODELS: dict[str, ParakeetModel] = {
    "v2": ParakeetModel(
        "tdt-0.6b-v2",
        ParakeetFile(
            "parakeet-tdt-0.6b-v2.gguf",
            "c82b001dcb0adecd36f7401e4b77c7257eb352462368b89cf3ee13184206d7f7",
            1_236_861_248,
        ),
        ParakeetFile(
            "parakeet-tdt-0.6b-v2-q4_k.gguf",
            "764c4e6738b0b38c53bbfea040f9e07425d6d742df58b18906053085aea46b1c",
            396_937_984,
        ),
    ),
}
