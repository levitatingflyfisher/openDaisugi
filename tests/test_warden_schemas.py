import json

from typer.testing import CliRunner

from opendaisugi.cli import app
from opendaisugi.mcp_schemas import import_schema, load_schemas

TOOLS = {
    "tools": [
        {
            "name": "get_issue",
            "inputSchema": {"type": "object", "properties": {"repo": {}, "number": {}}},
        }
    ]
}


def test_import_and_list(tmp_path):
    src = tmp_path / "gh.json"
    src.write_text(json.dumps(TOOLS))
    root = tmp_path / "warden"
    p = import_schema(root, "github", src)
    assert p == root / "schemas" / "github.json" and oct(p.stat().st_mode & 0o777) == "0o600"
    assert set(load_schemas(root)) == {"github"}
    r = CliRunner().invoke(app, ["warden", "schemas", "list", "--root", str(root)])
    assert r.exit_code == 0 and "github  1 tools" in r.output


def test_register_refuses_a_rule_the_schema_does_not_have(tmp_path):
    root = tmp_path / "warden"
    src = tmp_path / "gh.json"
    src.write_text(json.dumps(TOOLS))
    import_schema(root, "github", src)
    env = {
        "generated_by": "test",
        "task": "t",
        "stakes": "medium",
        "permissions": {
            "mcp_rules": [
                {
                    "tool": "github/get_issue",
                    "args": [{"op": "equals", "path": "repo_name", "value": "x"}],
                }
            ]
        },
        "invariants": [],
        "postconditions": [],
    }
    f = tmp_path / "env.json"
    f.write_text(json.dumps(env))
    r = CliRunner().invoke(
        app, ["warden", "register", str(f), "--root", str(root), "--session", "s"]
    )
    assert r.exit_code == 2
    assert "names argument 'repo_name'; the schema has: number, repo" in r.output
    assert not (root / "envelopes").exists() or not any((root / "envelopes").iterdir())


def test_register_notes_when_no_schema(tmp_path, capsys):
    root = tmp_path / "warden"
    env = {
        "generated_by": "test",
        "task": "t",
        "stakes": "medium",
        "permissions": {"mcp_rules": [{"tool": "github/get_issue", "known": ["repo"]}]},
        "invariants": [],
        "postconditions": [],
    }
    f = tmp_path / "env.json"
    f.write_text(json.dumps(env))
    r = CliRunner().invoke(
        app, ["warden", "register", str(f), "--root", str(root), "--session", "s"]
    )
    assert r.exit_code == 0 and "no schema on file for github" in r.output
