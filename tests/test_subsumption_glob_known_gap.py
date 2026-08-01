"""KNOWN GAP — the envelope↔envelope Z3 glob encoding diverges from the concrete
runtime matcher, and this test pins the divergence so it cannot silently regress or
be silently fixed.

``subsumption._glob_to_z3`` encodes a single-``*`` glob as ``And(PrefixOf, SuffixOf)``
over a *free, un-normalized* string, so a single ``*`` crosses ``/`` and ``..`` is not
collapsed. ``verify._path_matches_any`` — the matcher the executor gate actually uses —
normalizes with ``posixpath.normpath`` and matches with ``PurePosixPath.match``, where a
single ``*`` does **not** cross ``/``. They disagree.

Impact (why this is real but does NOT block Stage 9):
- The divergence makes ``_patterns_subsume`` under-approximate ``¬outer_ok``, so
  ``envelope_subsumes`` can incorrectly *hold* — a **false formal claim**, the
  fail-open edge of ``check_skill_delegations``.
- It is NOT an unauthorized write: the concrete gate (``_path_matches_any``) still
  denies the actual effect at runtime. So the damage is a wrong *proof*, not a wrong
  *action*.
- Stage 9's ``batch.prove_footprint`` deliberately avoids this encoding and proves the
  resolved write-set with the concrete matcher instead
  (``tests/test_batch.py::test_prove_footprint_uses_concrete_matcher_not_z3_glob``).

The surgical fix (its own commit, after Stage 9): classify the diverging glob shapes as
*unsupported* in ``subsumption._glob_unsupported`` so a mid-path single-star becomes a
fail-closed violation instead of a permissive ``And(PrefixOf, SuffixOf)``. Expect it to
light up existing subsumption tests — which is exactly why it is not bundled here. When
that lands, this test's ``holds`` assertion flips and the test must be updated to assert
the closed behavior.
"""

from __future__ import annotations

from opendaisugi.models import Envelope, Permission
from opendaisugi.subsumption import envelope_subsumes
from opendaisugi.verify import _path_matches_any


def _env(write_globs: list[str]) -> Envelope:
    return Envelope(
        generated_by="test",
        task="glob-gap",
        permissions=Permission(file_write=write_globs),
    )


def test_known_gap_glob_z3_diverges_from_concrete_matcher():
    sub_path = "/x/sub/c.txt"
    single_star = "/x/*.txt"

    # The concrete runtime gate DENIES the sub-path (single * does not cross /).
    assert _path_matches_any(sub_path, [single_star]) is False

    # …but subsumption's Z3 glob encoding BLESSES an inner envelope that admits it.
    outer = _env([single_star])
    inner = _env([sub_path])
    result = envelope_subsumes(outer, inner)

    # This is the documented fail-open: a false formal claim, not an unauthorized
    # write. When the surgical fix lands, this flips to ``is False`` and this test
    # (and its docstring) must be updated to assert the closed behavior.
    assert result.holds is True, (
        "Divergence appears CLOSED — if this was the intended fix, update this "
        "known-gap test to assert the fail-closed behavior."
    )
