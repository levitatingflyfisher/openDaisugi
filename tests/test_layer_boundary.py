"""ADR-0020: the layer stays importable alone.

No module under ``opendaisugi`` that belongs to the layer may import
``opendaisugi.floor``, ``opendaisugi.voice``, or ``opendaisugi.coppice`` —
not as a top-level import, not lazily inside a function, not conditionally
on a flag. The floor and the loop may depend on the layer in any direction
they like; the layer may never reach up.

The packages ``opendaisugi.floor``, ``.voice``, ``.coppice`` do not exist on
disk yet (spec-01 creates ``floor`` first). Both tests below are written so
they keep working once those packages exist: the runtime test poisons their
names in ``sys.modules`` rather than relying on ``ModuleNotFoundError``, and
the static test matches on dotted-name prefix, not on whether the path
currently resolves to a real file.
"""

from __future__ import annotations

import ast
import importlib
import sys
from pathlib import Path

# Everything else under opendaisugi/ is the layer. A literal list, not a
# config file, so adding a floor module to this exclusion is a visible diff
# in a code review, not a silent config change.
LAYER_EXCLUDED = {
    "opendaisugi.tui",
    "opendaisugi.tui_asks",
    "opendaisugi.tui_base",
    "opendaisugi.tui_sessions",
    "opendaisugi.tui_tree",
    "opendaisugi.tui_wiring",
    "opendaisugi.cli",
    "opendaisugi.cockpit",
    "opendaisugi.dashboard",
    "opendaisugi.start",
}
UPPER_PACKAGES = ("opendaisugi.floor", "opendaisugi.voice", "opendaisugi.coppice")

SRC_ROOT = Path("src")
PACKAGE_ROOT = SRC_ROOT / "opendaisugi"


def _is_upper(dotted: str) -> bool:
    """True if ``dotted`` is one of UPPER_PACKAGES or lives under one.

    Matches on ``pkg + "."`` prefix, not bare ``pkg`` prefix, so
    ``opendaisugi.floorplan`` (a hypothetical unrelated module) is never
    mistaken for something under ``opendaisugi.floor``.
    """
    return any(dotted == pkg or dotted.startswith(pkg + ".") for pkg in UPPER_PACKAGES)


def _iter_layer_module_paths() -> list[tuple[str, Path]]:
    """(dotted module name, source file) for every layer module on disk."""
    out: list[tuple[str, Path]] = []
    for path in sorted(PACKAGE_ROOT.rglob("*.py")):
        if "__pycache__" in path.parts:
            continue
        rel = path.relative_to(SRC_ROOT)
        parts = list(rel.parts)
        parts[-1] = parts[-1][:-3]  # strip ".py"
        if parts[-1] == "__init__":
            parts = parts[:-1]
        dotted = ".".join(parts)
        if dotted in LAYER_EXCLUDED or _is_upper(dotted):
            continue
        out.append((dotted, path))
    return out


def _layer_modules() -> list[str]:
    return [dotted for dotted, _ in _iter_layer_module_paths()]


def _resolve_relative(dotted: str, is_package: bool, level: int, module: str | None) -> str:
    """Absolute dotted target of a relative ``ast.ImportFrom``.

    ``dotted`` is the importing module's own dotted name; ``is_package`` is
    True when that module is a package's ``__init__.py`` (so ``dotted`` is
    itself the package name, not a name one level inside it); ``level`` is
    ``node.level`` (1 for ``from .``, 2 for ``from ..``, ...); ``module`` is
    ``node.module`` (``None`` for a bare ``from . import x``).

    A plain (non-package) module's own package is one level up from its
    dotted name already, so ``level=1`` resolves to ``dotted`` minus its
    last segment; a package's own name already *is* that level, so its
    ``level=1`` resolves to ``dotted`` unchanged. Each additional level
    strips one more trailing segment. ``module``, when given, is appended;
    otherwise the caller appends the imported name itself (the ``from .
    import floor`` form has no ``module`` — ``floor`` is one of ``names``).
    """
    parts = dotted.split(".")
    strip = level - 1 if is_package else level
    if strip:
        parts = parts[:-strip] if strip < len(parts) else []
    base = ".".join(parts)
    if module:
        return f"{base}.{module}" if base else module
    return base


def test_resolve_relative_against_synthetic_inputs():
    """Unit-tests the resolver in isolation, without touching real files."""
    assert _resolve_relative("opendaisugi.gate", False, 1, "floor") == "opendaisugi.floor"
    assert _resolve_relative("opendaisugi", True, 1, "floor") == "opendaisugi.floor"
    assert _resolve_relative("opendaisugi.sub.mod", False, 2, "floor") == "opendaisugi.floor"
    assert _resolve_relative("opendaisugi.gate", False, 1, None) == "opendaisugi"


def _is_dynamic_import_call(func: ast.expr) -> bool:
    """True if ``func`` is the callee of a string-form dynamic import.

    Matches ``importlib.import_module`` (an ``ast.Attribute`` with value
    name ``importlib`` and attr ``import_module``), a bare ``import_module``
    (e.g. imported via ``from importlib import import_module``), and the
    builtin ``__import__``.
    """
    if isinstance(func, ast.Attribute):
        return (
            func.attr == "import_module"
            and isinstance(func.value, ast.Name)
            and func.value.id == "importlib"
        )
    if isinstance(func, ast.Name):
        return func.id in {"import_module", "__import__"}
    return False


def test_upper_package_prefix_matching_is_exact():
    """Guards the _is_upper helper itself against a substring-prefix bug."""
    assert _is_upper("opendaisugi.floor")
    assert _is_upper("opendaisugi.floor.pane")
    assert _is_upper("opendaisugi.coppice.client")
    assert not _is_upper("opendaisugi.floorplan")
    assert not _is_upper("opendaisugi.gate")


def test_layer_modules_import_with_upper_packages_hidden(monkeypatch):
    """Hiding floor/voice/coppice must not break importing any layer module.

    ``sys.modules[name] = None`` makes ``import name`` (and
    ``from name import x``) raise ImportError even when the package exists
    on disk — the mechanism that lets this test keep enforcing the boundary
    once spec-01 creates the real ``opendaisugi.floor`` package.
    """
    modules = _layer_modules()
    assert len(modules) > 100, "the module walk found suspiciously few layer modules"
    for pkg in UPPER_PACKAGES:
        monkeypatch.setitem(sys.modules, pkg, None)
    for mod in modules:
        importlib.import_module(mod)  # must not raise


def test_layer_modules_have_no_static_upper_imports():
    """AST scan: no layer module names an upper package in any import, ever.

    ``ast.walk`` descends into function and method bodies, so a lazy,
    function-local ``import opendaisugi.floor`` is caught exactly like a
    top-level one — the rule is structural, not about when the code runs.

    A relative ``from . import floor`` or ``from .floor import Pane`` is
    resolved to its absolute dotted target via ``_resolve_relative`` before
    the ``_is_upper`` check, rather than skipped: the runtime test below
    cannot backstop a relative import once ``opendaisugi.floor`` exists,
    because ``importlib.import_module`` returns an already-cached module
    without re-executing its body, so a relative import inside it would
    never raise even with the upper package poisoned in ``sys.modules``.

    Beyond ``import``/``from ... import`` statements, this also flags
    string-form dynamic imports: a call to ``importlib.import_module(...)``,
    a bare ``import_module(...)``, or ``__import__(...)`` whose first
    positional argument is a string literal naming an upper package. More
    broadly still, it flags any string literal anywhere in a layer module
    that names an upper package — not just call arguments — because the
    target module name need not be a call argument at all:
    ``src/opendaisugi/__init__.py``'s ``_LAZY`` dict stores module names as
    plain string values, never passed to any call. A module name built by
    concatenation or ``str.format``/f-string at runtime is not a string
    literal and is not caught by any of this — that gap is acknowledged,
    not silently covered.
    """
    offenders: list[tuple[str, str]] = []
    module_paths = _iter_layer_module_paths()
    assert len(module_paths) > 100, "the module walk found suspiciously few layer modules"
    for dotted, path in module_paths:
        # Constant nodes already reported via the dynamic-import-call branch
        # are tracked by id so the generic string-literal branch below
        # doesn't add the same source occurrence to `offenders` a second
        # time. Reset per file: ``id()`` is a CPython memory address, and
        # once this file's `tree` is freed at the next loop iteration, a
        # later file's Constant nodes can be handed the same address —
        # carrying the set across files would let a stale id silently
        # suppress a genuine offender in a different module.
        reported_constant_ids: set[int] = set()
        is_package = path.name == "__init__.py"
        tree = ast.parse(path.read_text(), filename=str(path))
        for node in ast.walk(tree):
            if isinstance(node, ast.Import):
                for alias in node.names:
                    if _is_upper(alias.name):
                        offenders.append((dotted, alias.name))
            elif isinstance(node, ast.ImportFrom):
                if node.level:
                    target = _resolve_relative(dotted, is_package, node.level, node.module)
                    if node.module:
                        if _is_upper(target):
                            offenders.append((dotted, target))
                    else:
                        for alias in node.names:
                            candidate = f"{target}.{alias.name}" if target else alias.name
                            if _is_upper(candidate):
                                offenders.append((dotted, candidate))
                    continue
                if node.module and _is_upper(node.module):
                    offenders.append((dotted, node.module))
            elif isinstance(node, ast.Call) and _is_dynamic_import_call(node.func):
                if node.args and isinstance(node.args[0], ast.Constant):
                    arg = node.args[0]
                    if isinstance(arg.value, str) and _is_upper(arg.value):
                        offenders.append((dotted, arg.value))
                        reported_constant_ids.add(id(arg))
            elif isinstance(node, ast.Constant) and id(node) not in reported_constant_ids:
                if isinstance(node.value, str) and _is_upper(node.value):
                    offenders.append((dotted, node.value))
    assert offenders == [], f"layer modules importing upper packages: {offenders}"
