"""Sanity test that the top-level opendaisugi package re-exports the public API."""


def test_public_api_exports():
    import opendaisugi

    assert hasattr(opendaisugi, "__version__")
    assert hasattr(opendaisugi, "verify")
    assert hasattr(opendaisugi, "Envelope")
    assert hasattr(opendaisugi, "ActionPlan")
    assert hasattr(opendaisugi, "ActionStep")
    assert hasattr(opendaisugi, "Permission")
    assert hasattr(opendaisugi, "Postcondition")
    assert hasattr(opendaisugi, "Invariant")
    assert hasattr(opendaisugi, "VerificationResult")
    assert hasattr(opendaisugi, "Violation")
    assert hasattr(opendaisugi, "OpenDaisugiError")
    assert hasattr(opendaisugi, "VerificationTimeout")


def test_verify_works_from_top_level():
    from opendaisugi import (
        ActionPlan,
        Envelope,
        Permission,
        ShellStep,
        verify,
    )

    env = Envelope(
        generated_by="test",
        task="test",
        permissions=Permission(shell=True, shell_allowlist=["echo"]),
    )
    plan = ActionPlan(
        source="test",
        task="test",
        steps=[ShellStep(id="s1", command="echo hi")],
    )
    result = verify(plan, env)
    assert result.ok is True


def test_public_week2_exports_are_importable():
    from opendaisugi import (
        Config,
        Daisugi,
        generate_envelope,
    )
    assert Config is not None
    assert Daisugi is not None
    assert callable(generate_envelope)


def test_all_list_contains_week2_exports():
    import opendaisugi
    assert "Config" in opendaisugi.__all__
    assert "Daisugi" in opendaisugi.__all__
    assert "generate_envelope" in opendaisugi.__all__


def test_config_helpers_are_importable():
    from opendaisugi import Config, load_config, save_config
    assert callable(load_config)
    assert callable(save_config)
    # save_config should accept a Config and a path — round-trip smoke test.
    import tempfile
    from pathlib import Path as _P
    cfg = Config()
    with tempfile.TemporaryDirectory() as d:
        p = _P(d) / "c.yaml"
        save_config(cfg, p)
        loaded = load_config(p)
        assert loaded == cfg


def test_calibration_helpers_are_importable():
    from opendaisugi import CalibrationReport, run_calibration
    assert callable(run_calibration)
    # CalibrationReport is a dataclass — check it constructs.
    report = CalibrationReport(total=0, passed=0, failures=[], errors={})
    assert report.pass_rate == 0.0


def test_journal_public_exports():
    from opendaisugi import Journal, JournalStats, ReplayResult, TraceRecord
    assert Journal is not None
    assert JournalStats is not None
    assert ReplayResult is not None
    assert TraceRecord is not None


def test_all_list_contains_journal_exports():
    import opendaisugi
    for name in ("Journal", "JournalStats", "ReplayResult", "TraceRecord"):
        assert name in opendaisugi.__all__


def test_v010_supervisor_exports():
    from opendaisugi import (
        RunStatus,
        Supervisor,
    )
    assert Supervisor is not None
    assert RunStatus.SUCCEEDED.value == "succeeded"


def test_v010_all_list_contains_supervisor_exports():
    import opendaisugi
    for name in (
        "Supervisor", "RunSession", "RunStatus", "StepOutcome",
        "StepExecutor", "SubprocessExecutor", "DryRunExecutor",
        "FakeExecutor", "ExecutorResult",
        "ApprovalStrategy", "ApprovalDecision",
    ):
        assert name in opendaisugi.__all__, f"{name} missing from __all__"


# --- v0.1.2 Daisugi(cache=...) facade integration --------------------------


def test_daisugi_default_constructs_cache(tmp_path):
    from opendaisugi import Daisugi, EnvelopeCache

    d = Daisugi(data_dir=tmp_path)
    assert isinstance(d.cache, EnvelopeCache)
    assert d.cache.stats() == {"entries": 0, "evicted_on_init": 0}


def test_daisugi_cache_false_disables(tmp_path):
    from opendaisugi import Daisugi

    d = Daisugi(data_dir=tmp_path, cache=False)
    assert d.cache is None


def test_daisugi_accepts_injected_cache(tmp_path):
    from opendaisugi import Daisugi, EnvelopeCache

    custom = EnvelopeCache(tmp_path / "custom.db", prompt_version="custom-v")
    d = Daisugi(data_dir=tmp_path, cache=custom)
    assert d.cache is custom


def test_daisugi_cache_db_lives_in_data_dir(tmp_path):
    from opendaisugi import Daisugi

    d = Daisugi(data_dir=tmp_path)
    expected = tmp_path / "envelope_cache.db"
    # Trigger creation by touching stats (which opens the connection).
    d.cache.stats()
    assert expected.exists()


async def test_daisugi_generate_envelope_uses_cache(tmp_path, mock_llm_client):
    """Two facade-level generate_envelope calls hit cache on the second."""
    from opendaisugi import Daisugi
    from opendaisugi.models import Envelope, Permission

    mock_llm_client.set_next_envelope(Envelope(
        generated_by="t", task="t", permissions=Permission(),
    ))
    d = Daisugi(data_dir=tmp_path)
    await d.generate_envelope("identical task")
    await d.generate_envelope("identical task")
    assert mock_llm_client.call_count == 1


# --- v0.1.3 Tiered routing + stakes policy ---


def test_v013_exports_present():
    import opendaisugi
    assert hasattr(opendaisugi, "DEFAULT_LOW_STAKES_ENVELOPE")
    assert hasattr(opendaisugi, "LowStakesNotConfigured")
    assert hasattr(opendaisugi, "ModelLadderExhausted")
    assert hasattr(opendaisugi, "StakesInheritanceWarning")
    assert hasattr(opendaisugi, "ThinkingBudget")
    from opendaisugi import (
        LowStakesNotConfigured,
    )
    assert LowStakesNotConfigured is not None


def test_v013_all_contains_new_symbols():
    from opendaisugi import __all__
    for name in (
        "DEFAULT_LOW_STAKES_ENVELOPE",
        "LowStakesNotConfigured",
        "ModelLadderExhausted",
        "StakesInheritanceWarning",
        "ThinkingBudget",
    ):
        assert name in __all__, f"{name!r} missing from __all__"


def test_daisugi_with_default_low_stakes_exposed():
    from opendaisugi import Daisugi
    assert callable(getattr(Daisugi, "with_default_low_stakes", None))


def test_v020_exports_present():
    import opendaisugi
    assert hasattr(opendaisugi, "RefinementRecord")
    assert hasattr(opendaisugi, "RefinementLog")
    assert hasattr(opendaisugi, "FallbackHandler")
    assert hasattr(opendaisugi, "FallbackOutcome")
    assert hasattr(opendaisugi, "HaltHandler")
    assert hasattr(opendaisugi, "RecomputeHandler")
    from opendaisugi import (
        HaltHandler,
    )
    assert HaltHandler is not None


def test_v020_all_contains_new_symbols():
    from opendaisugi import __all__
    for name in (
        "RefinementRecord",
        "RefinementLog",
        "FallbackHandler",
        "FallbackOutcome",
        "HaltHandler",
        "RecomputeHandler",
    ):
        assert name in __all__, f"{name!r} missing from __all__"


def test_v021_exports_present():
    """v0.2.1 promotes make_cache_key to the public surface."""
    from opendaisugi import make_cache_key  # must not raise
    assert callable(make_cache_key)


def test_v021_all_contains_make_cache_key():
    import opendaisugi
    assert "make_cache_key" in opendaisugi.__all__


def test_version_exposed():
    import opendaisugi
    assert isinstance(opendaisugi.__version__, str)
    assert opendaisugi.__version__.count(".") >= 2


def test_v030_exports_present():
    """v0.3.0 surfaces distillation types on the top-level package."""
    from opendaisugi import (
        CompiledPathway,
        Distiller,
        PathwayMatch,
        PathwayStore,
        TendReport,
    )
    assert CompiledPathway is not None
    assert PathwayMatch is not None
    assert PathwayStore is not None
    assert Distiller is not None
    assert TendReport is not None


def test_v030_all_contains_distillation_types():
    import opendaisugi
    for name in ("CompiledPathway", "PathwayMatch", "PathwayStore", "Distiller", "TendReport"):
        assert name in opendaisugi.__all__, f"missing: {name}"
