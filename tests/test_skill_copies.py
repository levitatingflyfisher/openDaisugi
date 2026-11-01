from pathlib import Path

REPO = Path(__file__).resolve().parents[1]
SRC = REPO / "src/opendaisugi/skills/opendaisugi-checklist"
COPIES = [
    REPO / "skills/opendaisugi-checklist",
    REPO / "clients/go/internal/install/assets/skill/opendaisugi-checklist",
]


def test_the_three_copies_are_identical():
    for rel in ["SKILL.md", "references/authoring-a-dsl.md"]:
        want = (SRC / rel).read_bytes()
        for c in COPIES:
            assert (c / rel).read_bytes() == want, f"{c / rel} differs from {SRC / rel}"


def test_skill_body_is_port_neutral():
    body = (SRC / "SKILL.md").read_text()
    step2 = body.split("2. **Invent the DSL.**", 1)[1].split("3. **Author the envelope.**", 1)[0]
    assert "custom_step_allowlist" in step2 and "any port" in step2
    assert step2.index("JSON step record") < step2.index("Python package")
    assert "Pydantic discriminator" not in (SRC / "references/authoring-a-dsl.md").read_text()


def test_the_frontmatter_is_strict_yaml():
    """Hermes reads SKILL.md frontmatter with a strict YAML parser. A plain
    scalar that holds ": " (as in "Domain-agnostic: applies") made it fail."""
    import yaml

    for skill in [SRC, *COPIES]:
        text = (skill / "SKILL.md").read_text()
        assert text.startswith("---\n"), skill
        head = text.split("\n---\n", 1)[0][4:]
        doc = yaml.load(head, Loader=yaml.SafeLoader)
        assert isinstance(doc, dict), skill
        assert doc["name"] == "opendaisugi-checklist"
        assert isinstance(doc["description"], str) and ": " not in doc["description"]
        assert set(doc) == {"name", "description"}
