"""The set of benches, one per layer, in the order they were registered.

`BenchSpec.run` takes the options and returns a Table. That is the whole
contract, so a new layer is one module and one `register` call.
"""

from __future__ import annotations

from collections.abc import Callable
from dataclasses import dataclass, field
from pathlib import Path

from opendaisugi.bench.table import Column, Table


def _default_data_dir() -> Path:
    import opendaisugi

    return opendaisugi.DEFAULT_DATA_DIR


class BenchRefused(RuntimeError):
    """A bench declined to run as asked. The message names the next step.

    Distinct from a bug: the CLI prints this and exits 1, and lets any other
    exception crash with its traceback.
    """


@dataclass(frozen=True)
class BenchOpts:
    corpus: Path | None = None
    live: bool = False
    data_dir: Path = field(default_factory=_default_data_dir)


@dataclass(frozen=True)
class BenchSpec:
    name: str
    layer: str
    corpus: str
    columns: tuple[Column, ...]
    run: Callable[[BenchOpts], Table]


_SPECS: dict[str, BenchSpec] = {}


def register(spec: BenchSpec) -> None:
    """Add a bench. Registering the same layer again replaces it."""
    _SPECS[spec.layer] = spec


def get(layer: str) -> BenchSpec:
    _load_layers()
    return _SPECS[layer]


def layer_names() -> list[str]:
    _load_layers()
    return list(_SPECS)


def run_bench(layer: str, opts: BenchOpts) -> Table:
    return get(layer).run(opts)


def _load_layers() -> None:
    """Import the layer modules once so their `register` calls have run."""
    from opendaisugi.bench import layers  # noqa: F401
