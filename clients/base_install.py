"""The oracle as the binaries are built: a base install of opendaisugi
with only the extras the Go and Rust daisugi carry.

The module map (`daisugi modules`, `daisugi dashboard`) and the view step
of `daisugi start` report which optional packages are installed. The
venv that records the fixtures has more extras than the binaries carry,
and a CI runner has another set again. So the oracle runs with these
packages hidden, as if they were not installed:

- sentence_transformers (the MiniLM matcher, the [search] extra),
- onnxruntime (the int8 matcher, the [int8] extra),
- faster_whisper and sherpa_onnx (the voice engines),
- textual (the interactive dashboard, the [tui] extra).

model2vec (potion) and tree_sitter_bash (shell decomposition) stay
visible: both binaries carry them. The oracle also runs as an installed
wheel does, with no checkout: the compiled verifier clients it would
look for under the repository are then not built.

PRELUDE is Python source that sets this up. An oracle shim runs it first,
then the CLI.
"""

from __future__ import annotations

HIDDEN = ("sentence_transformers", "onnxruntime", "faster_whisper", "sherpa_onnx", "textual")

PRELUDE = f"""
import importlib.util as _iu, sys as _sys
_HIDDEN = {HIDDEN!r}
_real_find_spec = _iu.find_spec
def _find_spec(name, *a, **k):
    if name.split(".")[0] in _HIDDEN:
        return None
    return _real_find_spec(name, *a, **k)
_iu.find_spec = _find_spec
class _Hide:
    def find_spec(self, name, path=None, target=None):
        if name.split(".")[0] in _HIDDEN:
            raise ModuleNotFoundError(f"No module named {{name!r}}", name=name)
        return None
_sys.meta_path.insert(0, _Hide())
import opendaisugi.bench.options as _options
_options.repo_root = lambda: None
"""

MAIN = """
from opendaisugi.cli import main
sys.argv = ["daisugi"] + sys.argv[1:]
main()
"""


def shim(body: str = "") -> str:
    """The source of an oracle shim: the prelude, then body, then the CLI."""
    return PRELUDE + "import sys\n" + body + MAIN
