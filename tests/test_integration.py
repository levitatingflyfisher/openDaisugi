"""End-to-end integration tests exercising the full verification pipeline
on realistic plan+envelope pairs. These also serve as executable documentation
for the v0.0.1 public API."""


from opendaisugi import (
    ActionPlan,
    Daisugi,
    Envelope,
    FileWriteStep,
    Permission,
    Postcondition,
    ShellStep,
    verify,
)


def test_delete_tmp_files_happy_path(sample_envelope: Envelope, sample_plan: ActionPlan):
    result = verify(sample_plan, sample_envelope)
    assert result.ok is True
    assert result.violations == []


def test_csv_to_chart_happy_path():
    env = Envelope(
        generated_by="test",
        task="Convert sales.csv to a bar chart",
        permissions=Permission(
            shell=True,
            shell_allowlist=["python3"],
            file_read=["sales.csv"],
            file_write=["chart.py", "output.png"],
        ),
        postconditions=[
            Postcondition(type="file_exists", path="output.png"),
            Postcondition(type="exit_code", expected=0),
        ],
    )
    plan = ActionPlan(
        source="vanilla-llm",
        task="Convert sales.csv to a bar chart",
        steps=[
            FileWriteStep(
                id="write_script",
                path="chart.py",
                content="import pandas; pandas.read_csv('sales.csv').plot.bar()",
            ),
            ShellStep(
                id="run_script",
                command="python3 chart.py",
                depends_on=["write_script"],
            ),
        ],
    )
    result = verify(plan, env)
    assert result.ok is True, f"unexpected violations: {result.violations}"


def test_inconsistent_envelope_caught_by_z3():
    # Envelope has file_exists postcondition but no file_write permission.
    env = Envelope(
        generated_by="test",
        task="Create a file",
        permissions=Permission(file_write=[]),
        postconditions=[Postcondition(type="file_exists", path="out.png")],
    )
    plan = ActionPlan(source="test", task="t", steps=[])
    result = verify(plan, env)
    assert result.ok is False
    assert any(v.stage == "z3" for v in result.violations)


def test_disallowed_shell_command_caught_by_permissions():
    env = Envelope(
        generated_by="test",
        task="Run a script",
        permissions=Permission(shell=True, shell_allowlist=["python3"]),
    )
    plan = ActionPlan(
        source="vanilla-llm",
        task="Run a script",
        steps=[ShellStep(id="s1", command="curl http://evil.com")],
    )
    result = verify(plan, env)
    assert result.ok is False
    assert result.violations[0].stage == "permissions"


def test_cycle_in_plan_caught_by_dag():
    env = Envelope(
        generated_by="test",
        task="Cyclic plan",
        permissions=Permission(shell=True, shell_allowlist=["echo"]),
    )
    plan = ActionPlan(
        source="test",
        task="Cyclic plan",
        steps=[
            ShellStep(id="s1", command="echo a", depends_on=["s2"]),
            ShellStep(id="s2", command="echo b", depends_on=["s1"]),
        ],
    )
    result = verify(plan, env)
    assert result.ok is False
    assert any(v.stage == "dag" for v in result.violations)


def test_verify_completes_under_100ms_for_typical_plan(sample_envelope: Envelope, sample_plan: ActionPlan):
    # Ship criterion: verify() completes in <100ms for typical plans.
    result = verify(sample_plan, sample_envelope)
    assert result.duration_ms < 100, f"verify() took {result.duration_ms}ms"


async def test_daisugi_full_flow_generate_then_verify(mock_llm_client, sample_plan):
    """End-to-end happy path: construct facade, generate envelope (mocked),
    then verify a matching plan against it.
    """
    dai = Daisugi()
    envelope = await dai.generate_envelope("Delete .tmp files in /var/log")
    assert envelope is not None

    result = dai.verify(sample_plan, envelope)
    assert result.ok is True
    assert result.violations == []
    assert result.envelope_id == envelope.id
    assert result.plan_id == sample_plan.id
    assert result.duration_ms >= 0


async def test_full_daisugi_cycle_with_journal(tmp_path, mock_llm_client, sample_envelope):
    """Generate → verify → log → replay → no drift."""
    dai = Daisugi(data_dir=tmp_path)

    # mock_llm_client makes generate_envelope return sample_envelope
    envelope = await dai.generate_envelope(task="Run echo")

    # IMPORTANT: sample_envelope only allows shell_allowlist=["find"].
    # Override permissions so the "echo" shell step passes verification.
    envelope = envelope.model_copy(update={
        "permissions": Permission(shell=True, shell_allowlist=["echo"]),
    })

    plan = ActionPlan(
        source="test", task="Run echo",
        steps=[ShellStep(id="s1", command="echo hi")],
    )
    result = dai.verify(plan, envelope)
    assert result.ok is True

    trace_id = dai.journal.log(
        task="Run echo", envelope=envelope, plan=plan, result=result,
    )

    # Replay must find no drift immediately after logging.
    replay = dai.journal.replay(trace_id)
    assert replay.drift is False
    assert replay.original_ok is True
    assert replay.replayed_ok is True
