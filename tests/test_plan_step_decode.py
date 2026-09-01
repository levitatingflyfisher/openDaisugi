"""A model may send a plan step as a string that holds the step's dict.

The schema the model sees names every registered step type, and a step that
arrives as a JSON string or as a Python dict literal is decoded once. Any
other string still fails.
"""

from __future__ import annotations

import json

import pytest
from pydantic import ValidationError

from opendaisugi.distiller import GeneralizedTemplate
from opendaisugi.models import STEP_TYPE_REGISTRY, ActionPlan, ShellStep


def _plan(steps):
    return ActionPlan.model_validate({"source": "x", "task": "t", "steps": steps})


def test_schema_names_every_registered_step_type():
    schema = GeneralizedTemplate.model_json_schema()
    text = json.dumps(schema)
    items = schema["$defs"]["ActionPlan"]["properties"]["steps"]["items"]
    assert items, "steps items must not be an empty schema"
    for name in STEP_TYPE_REGISTRY:
        assert f'"const": "{name}"' in text, name


def test_step_as_json_string_is_decoded():
    plan = _plan(['{"type": "shell", "command": "ls", "id": "s1"}'])
    assert isinstance(plan.steps[0], ShellStep)
    assert plan.steps[0].command == "ls"


def test_step_as_python_dict_literal_is_decoded():
    plan = _plan(["{'type': 'shell', 'command': 'ls', 'id': 's1', 'metadata': {'a': True}}"])
    assert isinstance(plan.steps[0], ShellStep)
    assert plan.steps[0].metadata == {"a": True}


@pytest.mark.parametrize(
    "bad",
    [
        '{"type": "no_such_step", "id": "s1"}',
        '["shell", "ls"]',
        "ls -la",
        "{'type': 'shell', 'command': __import__('os').getcwd(), 'id': 's1'}",
        '{"type": "shell", "command": "' + "a" * 70_000 + '", "id": "s1"}',
        "{'a': " * 3000 + "1" + "}" * 3000,
        '{"a": ' * 3000 + "1" + "}" * 3000,
    ],
)
def test_other_strings_still_fail(bad):
    with pytest.raises(ValidationError):
        _plan([bad])


@pytest.mark.asyncio
async def test_generalize_template_through_claude_code_takes_string_steps(monkeypatch):
    """The full claude-code path: a reply whose steps are Python dict
    strings gives a template of real steps, with no re-ask."""
    from unittest.mock import AsyncMock, MagicMock, patch

    from opendaisugi import distiller as dist_mod
    from opendaisugi.claude_code_llm import ClaudeCodeInstructorClient
    from opendaisugi.distiller import _generalize_template
    from opendaisugi.models import Envelope, Permission

    reply = json.dumps(
        {
            "task_description": "list files",
            "plan_template": {
                "source": "distilled",
                "task": "list files",
                "steps": ["{'type': 'shell', 'command': 'ls', 'id': 's1', 'depends_on': []}"],
            },
        }
    )
    # The structured call asks for --output-format json: the reply rides
    # in the CLI's result envelope.
    reply = json.dumps({"type": "result", "is_error": False, "result": reply}).encode()
    proc = MagicMock()
    proc.communicate = AsyncMock(return_value=(reply, b""))
    proc.returncode = 0
    spawn = AsyncMock(return_value=proc)
    monkeypatch.setattr(dist_mod, "get_instructor_client", lambda _m: ClaudeCodeInstructorClient())
    env = Envelope(generated_by="test", task="T", permissions=Permission(shell=True))
    plan = _plan([ShellStep(id="s1", command="ls")])
    with patch("opendaisugi.claude_code_llm.asyncio.create_subprocess_exec", spawn):
        out = await _generalize_template(plan=plan, envelope=env, pitfalls=[], model="haiku")
    assert isinstance(out.plan_template.steps[0], ShellStep)
    assert spawn.await_count == 1
