# Spec 00 — ADR-0020, the VISION revision, and the layer boundary test

**Master:** `00-master-spec.md` §1, §4 (layer purity), §5.1–5.3 · **Size:** S · **Depends on:** nothing

## Purpose

Make the expanded vision durable and mechanically enforced before any floor code exists. Three
deliverables: an ADR that supersedes ADR-0004 precisely, a VISION revision that names the three
parts, and a test that fails the moment any layer module reaches up.

## Why this comes first

A vision that lives only in a chat transcript is not a vision. ADR-0004's escape hatch ("a
separate product built on this layer") was the right instinct with the wrong emphasis; the
protective invariant is importability of the layer, not the absence of a loop. Writing that down
and testing it is what lets sub-projects 02–07 grow without the old fear of "becoming the
harness, badly."

## Deliverables

### D1. `docs/adr/0020-layer-floor-loop.md`

Status Accepted, date 2026-09-08. Sections: Context (the audit's convergence finding, sprig and
the cockpit as facts on the ground, the name coppice), Decision (three parts, one invariant),
Consequences, Alternatives considered (keep ADR-0004 as written; fold the loop into the layer;
build the floor as a separate repo). The Decision section must contain these sentences verbatim:

> openDaisugi is three parts. The **layer** (gate, verifier, journal, garden) is a library that
> imports nothing above it and runs alone in any harness. The **floor** (coppice) supervises
> panes and sources every state it shows. The **loop** is whatever harness runs in a pane; sprig
> is ours, the rest are rented. The invariant that ADR-0004 protected is restated: **the layer
> stays importable alone.** `tests/test_layer_boundary.py` enforces it.

Update `docs/adr/0004-layer-not-harness.md` status line to `Superseded in part by ADR-0020` and
add one row to `docs/adr/README.md`.

### D2. `VISION.md`

- Replace invariant 5 ("Layer, not harness") with: **"The layer stays importable alone.**
  openDaisugi may host a floor and own a loop, but the gate, verifier, journal, and garden import
  nothing above them and run in any harness without them. ([ADR-0020](docs/adr/0020-layer-floor-loop.md))"
- Add a short section after "What this is", titled **"Three parts"**, with the table from the
  master spec §1 (layer / floor / loop, owner, what must stay true) and the coppice name note.
- Do not touch the scorecard numbers; the v0.44.0 doc-refresh release owns those.

### D3. `tests/test_layer_boundary.py`

Two tests.

```python
LAYER_EXCLUDED = {  # everything else under opendaisugi/ is the layer
    "opendaisugi.tui", "opendaisugi.tui_base", "opendaisugi.tui_sessions", "opendaisugi.tui_tree",
    "opendaisugi.tui_wiring", "opendaisugi.cockpit", "opendaisugi.dashboard", "opendaisugi.start",
}
UPPER_PACKAGES = ("opendaisugi.floor", "opendaisugi.voice", "opendaisugi.coppice")

def test_layer_modules_import_with_upper_packages_hidden(monkeypatch):
    # sys.modules[name] = None makes `import name` raise ImportError
    for pkg in UPPER_PACKAGES: monkeypatch.setitem(sys.modules, pkg, None)
    for mod in _layer_modules(): importlib.import_module(mod)  # must not raise

def test_layer_modules_have_no_static_upper_imports():
    # ast scan: any `import opendaisugi.floor…` / `from opendaisugi.floor…` in a layer file fails,
    # including inside functions (lazy imports count — the rule is structural, not runtime).
```

`_layer_modules()` walks `src/opendaisugi/**.py`, maps to dotted names, and subtracts
`LAYER_EXCLUDED` plus anything under `UPPER_PACKAGES`. The excluded set is a *list in the test*,
not a config file, so adding a floor module is a visible diff.

The packages `opendaisugi.floor`, `.voice`, `.coppice` do not exist yet when this lands; the
test must pass with them absent and keep passing when spec-01 creates `floor`.

## Testing

- The two tests above.
- `tests/test_adr_index.py` (exists? if not, add a 5-line test) asserts every `docs/adr/00NN-*.md`
  has a row in `docs/adr/README.md`.

## Out of scope

Scorecard numbers, README status line, CHANGELOG — the v0.44.0 refresh.
