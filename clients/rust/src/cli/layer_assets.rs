// The skill and the OpenClaw plugin, included from the package (layers.rs).

/// The opendaisugi-checklist skill, byte for byte the package's.
pub const SKILL_FILES: &[(&str, &str)] = &[
    ("SKILL.md", include_str!("../../../../src/opendaisugi/skills/opendaisugi-checklist/SKILL.md")),
    ("references/authoring-a-dsl.md", include_str!("../../../../src/opendaisugi/skills/opendaisugi-checklist/references/authoring-a-dsl.md")),
    ("references/contract-orchestration.md", include_str!("../../../../src/opendaisugi/skills/opendaisugi-checklist/references/contract-orchestration.md")),
    ("references/delegation.md", include_str!("../../../../src/opendaisugi/skills/opendaisugi-checklist/references/delegation.md")),
    ("references/git-registry.md", include_str!("../../../../src/opendaisugi/skills/opendaisugi-checklist/references/git-registry.md")),
    ("references/mcp-usage.md", include_str!("../../../../src/opendaisugi/skills/opendaisugi-checklist/references/mcp-usage.md")),
    ("references/passive-capture.md", include_str!("../../../../src/opendaisugi/skills/opendaisugi-checklist/references/passive-capture.md")),
    ("references/postconditions.md", include_str!("../../../../src/opendaisugi/skills/opendaisugi-checklist/references/postconditions.md")),
    ("references/vla-integration.md", include_str!("../../../../src/opendaisugi/skills/opendaisugi-checklist/references/vla-integration.md")),
    ("references/when-to-delegate.md", include_str!("../../../../src/opendaisugi/skills/opendaisugi-checklist/references/when-to-delegate.md")),
    ("references/worked-example-council.md", include_str!("../../../../src/opendaisugi/skills/opendaisugi-checklist/references/worked-example-council.md")),
];

/// The OpenClaw before_tool_call plugin, byte for byte.
pub const OPENCLAW_PLUGIN: &[(&str, &str)] = &[
    ("index.mjs", include_str!("../../../../src/opendaisugi/install_assets/openclaw_plugin/index.mjs")),
    ("openclaw.plugin.json", include_str!("../../../../src/opendaisugi/install_assets/openclaw_plugin/openclaw.plugin.json")),
    ("package.json", include_str!("../../../../src/opendaisugi/install_assets/openclaw_plugin/package.json")),
];
