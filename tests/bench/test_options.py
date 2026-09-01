"""The pinned facts must stay true. Every recorded command and class cites a
document that still says it. A fact nobody re-reads rots silently."""

import sys

from opendaisugi.bench.options import (
    GATE_PATHS,
    LOOPS,
    MATCHER_BACKENDS,
    VERIFIER_CLIENTS,
    build_hint,
    repo_root,
)


def _read(rel: str) -> str:
    root = repo_root()
    assert root is not None, "these tests need a checkout, not an installed wheel"
    return (root / rel).read_text(encoding="utf-8")


def test_every_client_build_step_is_verbatim_in_its_readme():
    for spec in VERIFIER_CLIENTS.values():
        if not spec.build_steps:
            continue
        readme = _read(spec.readme)
        for step in spec.build_steps:
            assert step in readme, f"{spec.name}: {step!r} is not in {spec.readme}"


def test_every_client_argv_appears_in_the_diversity_doc():
    doc = _read("docs/client-diversity.md")
    for spec in VERIFIER_CLIENTS.values():
        if spec.name == "python":
            continue
        assert " ".join(spec.argv) in doc, f"{spec.name} argv is not in docs/client-diversity.md"


def test_python_client_needs_no_build_and_runs_this_interpreter():
    py = VERIFIER_CLIENTS["python"]
    assert py.build_steps == ()
    assert py.argv == (sys.executable, "-m", "opendaisugi.conformance")


def test_matcher_backends_match_the_search_fallback_table():
    from opendaisugi._search import _FALLBACK_PACKAGE

    recorded = {
        name: (spec.package, spec.extra)
        for name, spec in MATCHER_BACKENDS.items()
        if spec.package is not None
    }
    assert recorded == dict(_FALLBACK_PACKAGE)


def test_lexical_needs_no_package_so_it_can_never_be_absent():
    assert MATCHER_BACKENDS["lexical"].package is None
    assert MATCHER_BACKENDS["lexical"].extra is None


def test_every_gate_path_quote_is_still_in_its_source():
    for spec in GATE_PATHS.values():
        text = _read(spec.source)
        assert spec.quote in text, f"{spec.name}: {spec.quote!r} is gone from {spec.source}"


def test_fail_open_classes_and_evidence_are_from_the_closed_sets():
    for spec in GATE_PATHS.values():
        assert spec.fail_open_class in {"hard", "soft", "deny-only", "advisory"}
        assert spec.evidence in {"measured", "designed"}


def test_codex_is_recorded_as_the_fail_open_path():
    assert GATE_PATHS["codex-hooks"].fail_open_class == "soft"


def test_every_loop_names_an_install_command_and_a_real_gate_path():
    for spec in LOOPS.values():
        assert spec.install.strip()
        assert spec.gate_path in GATE_PATHS


def test_build_hint_is_a_runnable_one_liner():
    assert build_hint(VERIFIER_CLIENTS["go"]) == (
        "cd clients/go && go build -o conform ./cmd/conform"
    )


def test_every_client_profile_is_from_the_closed_set_and_lean_is_core():
    """The Lean client implements the Core profile by design. Its README says
    so. A bench that scored it as a Full client would read a profile gap as a
    verifier bug, or worse, hide it."""
    for spec in VERIFIER_CLIENTS.values():
        assert spec.profile in {"core", "full"}, spec.name
    assert VERIFIER_CLIENTS["lean"].profile == "core"
    assert "Core" in _read(VERIFIER_CLIENTS["lean"].readme)
    assert all(s.profile == "full" for n, s in VERIFIER_CLIENTS.items() if n != "lean")
