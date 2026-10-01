"""`daisugi agent-profiles list`: names, wires, URLs and whether each key is
set. It never carries a key or a key variable's name."""

from __future__ import annotations

from opendaisugi import agent_profiles

TABLE = {
    "desk": {
        "wire": "openai",
        "base_url": "http://desk:8000/v1",
        "model": "m",
        "key_env": "DESK_KEY",
    },
    "local": {"wire": "openai", "base_url": "http://127.0.0.1:1/v1"},
    "bad": {"wire": "smoke"},
}


def test_rows_are_the_valid_profiles_by_name_with_no_key():
    rows = agent_profiles.list_rows(agent_profiles.resolve(TABLE, {"DESK_KEY": "sk-secret"}))
    assert [r["name"] for r in rows] == ["desk", "local"]
    assert rows[0] == {
        "name": "desk",
        "wire": "openai",
        "base_url": "http://desk:8000/v1",
        "model": "m",
        "key_set": True,
    }
    assert "sk-secret" not in repr(rows) and "DESK_KEY" not in repr(rows)


def test_key_set_is_false_only_for_a_named_variable_that_is_not_set():
    rows = {r["name"]: r for r in agent_profiles.list_rows(agent_profiles.resolve(TABLE, {}))}
    assert rows["desk"]["key_set"] is False
    assert rows["local"]["key_set"] is True
    empty = agent_profiles.list_rows(agent_profiles.resolve(TABLE, {"DESK_KEY": ""}))
    assert empty[0]["key_set"] is False
