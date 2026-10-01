"""The conversion recipe for the Parakeet v2 model (ruling VO-16).

    uv run --no-sync python scripts/parakeet_convert.py CRISPASR_DIR NEMO OUT_F16.gguf

Runs CrispASR's own converter, models/convert-parakeet-to-gguf.py at the
pinned commit (clients/go/scripts/native.sh checks it against
clients/native/parakeet-cli/crispasr-files.sha256), on NVIDIA's original
parakeet-tdt-0.6b-v2.nemo, and writes the F16 GGUF. It prints the file's
sha256. That sha256 must equal pins.PARAKEET_MODELS["v2"].source.sha256,
the F16 file the voice server fetches; then parakeet-quantize, built by
native.sh --parakeet, makes the Q4_K file whose sha256 is pinned beside it.

The converter imports four packages this project does not depend on. This
script puts small stand-ins in their place; every weight it handles is the
converter's own code:

- gguf: GGUFWriter, a writer of the GGUF v3 layout that gguf-py writes
  (the header, the key-value pairs in the order they are added, the tensor
  infos, then the data, each part padded to 32 bytes). The byte match with
  the published F16 file is the check that it is the same layout.
- sentencepiece: the piece list, read from the tokenizer's protobuf
  through transformers' copy of the SentencePiece schema.
- librosa and zstandard: refused if called. The .nemo path of the
  converter needs neither (the mel filterbank is in the checkpoint).

Needs the dev environment (torch, numpy, transformers, PyYAML). It is run
once, by hand, under ~/opendaisugi-scratch/heavy.sh; nothing it makes is
published.
"""

from __future__ import annotations

import argparse
import enum
import hashlib
import importlib.util
import os
import struct
import sys
import types
from pathlib import Path

import numpy as np

# huggingface_hub reads this at import: unless set, it fetches its agent
# list from the Hub and names the agent in its User-Agent. A set value is kept.
os.environ.setdefault("HF_HUB_DISABLE_TELEMETRY", "1")

ALIGN = 32
T_UINT32, T_INT32, T_FLOAT32, T_BOOL, T_STRING, T_ARRAY = 4, 5, 6, 7, 8, 9


class GGMLQuantizationType(enum.IntEnum):
    F32 = 0
    F16 = 1
    Q8_0 = 8
    Q4_K = 12


def _pad(n: int) -> int:
    return (n + ALIGN - 1) // ALIGN * ALIGN


def _str(s: str) -> bytes:
    b = s.encode("utf-8")
    return struct.pack("<Q", len(b)) + b


def _scalar(t: int, v) -> bytes:
    if t == T_STRING:
        return _str(v)
    fmt = {T_UINT32: "<I", T_INT32: "<i", T_FLOAT32: "<f", T_BOOL: "<?"}[t]
    return struct.pack(fmt, v)


def _type_of(v) -> int:
    if isinstance(v, (str, bytes)):
        return T_STRING
    if isinstance(v, list):
        return T_ARRAY
    if isinstance(v, float):
        return T_FLOAT32
    if isinstance(v, bool):
        return T_BOOL
    if isinstance(v, int):
        return T_INT32
    raise TypeError(f"no GGUF type for {type(v)}")


class GGUFWriter:
    """Writes a GGUF v3 file in gguf-py's layout. Only what the converter calls."""

    def __init__(self, path: str, arch: str) -> None:
        self.path = path
        self.kv: list[tuple[str, bytes]] = []
        self.tensors: list[tuple[str, np.ndarray, int]] = []
        self.add_string("general.architecture", arch)

    def _add(self, key: str, t: int, v) -> None:
        self.kv.append((key, struct.pack("<I", t) + _scalar(t, v)))

    def add_name(self, v: str) -> None:
        self.add_string("general.name", v)

    def add_string(self, k: str, v: str) -> None:
        self._add(k, T_STRING, v)

    def add_uint32(self, k: str, v: int) -> None:
        self._add(k, T_UINT32, v)

    def add_int32(self, k: str, v: int) -> None:
        self._add(k, T_INT32, v)

    def add_float32(self, k: str, v: float) -> None:
        self._add(k, T_FLOAT32, v)

    def add_bool(self, k: str, v: bool) -> None:
        self._add(k, T_BOOL, v)

    def add_array(self, k: str, v: list) -> None:
        et = _type_of(v[0])
        body = struct.pack("<IIQ", T_ARRAY, et, len(v)) + b"".join(_scalar(et, x) for x in v)
        self.kv.append((k, body))

    def add_tensor(self, name: str, t: np.ndarray, raw_dtype=None) -> None:
        if raw_dtype is not None:
            raise ValueError("this recipe writes F16 and F32 only")
        t = np.ascontiguousarray(t)
        dtype = {np.dtype(np.float32): 0, np.dtype(np.float16): 1}[t.dtype]
        self.tensors.append((name, t, dtype))

    def write_header_to_file(self) -> None:
        pass

    def write_kv_data_to_file(self) -> None:
        pass

    def write_tensors_to_file(self) -> None:
        pass

    def close(self) -> None:
        out = bytearray(b"GGUF")
        out += struct.pack("<IQQ", 3, len(self.tensors), len(self.kv))
        for k, body in self.kv:
            out += _str(k) + body
        offset = 0
        for name, t, dtype in self.tensors:
            out += _str(name) + struct.pack("<I", t.ndim)
            out += b"".join(struct.pack("<Q", d) for d in reversed(t.shape))
            out += struct.pack("<IQ", dtype, offset)
            offset += _pad(t.nbytes)
        out += bytes(_pad(len(out)) - len(out))
        with open(self.path, "wb") as f:
            f.write(out)
            for _name, t, _dtype in self.tensors:
                f.write(t.tobytes())
                f.write(bytes(_pad(t.nbytes) - t.nbytes))


def _install_stand_ins() -> None:
    from transformers.utils import sentencepiece_model_pb2_new as pb

    gguf = types.ModuleType("gguf")
    gguf.GGUFWriter = GGUFWriter
    gguf.GGMLQuantizationType = GGMLQuantizationType
    gguf.quantize = None
    sys.modules["gguf"] = gguf

    class SentencePieceProcessor:
        def LoadFromSerializedProto(self, b: bytes) -> None:  # noqa: N802 - the real API's name
            m = pb.ModelProto()
            m.ParseFromString(b)
            self.pieces = [x.piece for x in m.pieces]

        def id_to_piece(self, i: int) -> str:
            return self.pieces[i]

        def get_piece_size(self) -> int:
            return len(self.pieces)

    spm = types.ModuleType("sentencepiece")
    spm.SentencePieceProcessor = SentencePieceProcessor
    sys.modules["sentencepiece"] = spm

    def refused(*_a, **_k):
        raise RuntimeError("this recipe does not provide this package")

    librosa = types.ModuleType("librosa")
    librosa.filters = types.SimpleNamespace(mel=refused)
    sys.modules["librosa"] = librosa
    zstandard = types.ModuleType("zstandard")
    zstandard.ZstdDecompressor = refused
    sys.modules["zstandard"] = zstandard


def sha256_file(path: Path) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as f:
        while chunk := f.read(1 << 20):
            h.update(chunk)
    return h.hexdigest()


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("crispasr", type=Path, help="the pinned CrispASR checkout")
    ap.add_argument("nemo", type=Path, help="parakeet-tdt-0.6b-v2.nemo")
    ap.add_argument("out", type=Path, help="the F16 GGUF to write")
    ap.add_argument("--extract-dir", type=Path, help="where the .nemo is unpacked (real disk)")
    a = ap.parse_args()
    # The checkout is native.sh's, which refuses any file it did not list.
    sys.dont_write_bytecode = True
    _install_stand_ins()
    conv_path = a.crispasr / "models" / "convert-parakeet-to-gguf.py"
    spec = importlib.util.spec_from_file_location("convert_parakeet", conv_path)
    assert spec is not None and spec.loader is not None
    conv = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(conv)
    conv.convert(a.nemo, a.out, quant=None, extract_dir=a.extract_dir, hf=None)
    print(f"sha256 {sha256_file(a.out)}  {a.out}")


if __name__ == "__main__":
    main()
