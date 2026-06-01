# Pathway Skill Format (v0.7.0)

openDaisugi exports and imports compiled pathways as **markdown files with YAML frontmatter** — the same shape used by Claude Code skills, Hermes skills, and OpenClaw skills. The body is human- and LLM-readable documentation; the frontmatter carries the canonical JSON bundle that openDaisugi's importer reads.

This means a compiled pathway is distributable through any existing skill-sharing mechanism (git repos, plugin marketplaces, skill bundles) — no separate pathway registry needed.

## File shape

```markdown
---
name: delete-stale-tmp-files
description: Remove .tmp files older than N days from a directory tree
daisugi:
  opendaisugi_version: 0.7.0
  schema_version: 1
  pathway:
    id: pathway_abc12345
    task_description: delete stale .tmp files
    task_embedding: [0.12, 0.04, 0.91, ...]
    embedding_model: sentence-transformers/all-MiniLM-L6-v2
    envelope:
      id: env_5ce91b
      generated_by: daisugi-distiller
      permissions:
        shell: true
        shell_allowlist: [find, rm]
        forbidden_paths: [/etc, /boot, /sys, /proc]
      invariants:
        - type: no_side_effects
          description: dry-run mode; files listed but not deleted
    plan_template:
      id: plan_8d3f01
      source: distilled
      steps:
        - id: s1
          type: shell
          command: "find {target_dir} -name '*.tmp' -mtime +{days} -delete"
    source_trace_ids: [trace_001, trace_002]
    distilled_at: 1745000000.0
---

# Delete stale tmp files

Z3-verified pathway for cleaning up temporary files older than N days.

## Verified permissions
- shell: true (allowlist: [find, rm])
- forbidden_paths: [/etc, /boot, /sys, /proc]

## Usage
Install with: `daisugi pathways import path/to/this-skill.md`
```

## Frontmatter keys

- **`name`** — kebab-case slug derived from the task description. Used by skill-frameworks that index by filename.
- **`description`** — one-line summary. Matches the pathway's `task_description`.
- **`daisugi`** — the openDaisugi-specific payload. Other frontmatter keys (e.g. agent-framework-specific metadata) may coexist.
- **`daisugi.schema_version`** — integer. Bumped when the on-disk bundle shape changes incompatibly. Imports fail if the bundle's version is higher than the library's current `BUNDLE_SCHEMA_VERSION`.
- **`daisugi.opendaisugi_version`** — informational. The version of openDaisugi that produced the bundle.
- **`daisugi.pathway`** — the CompiledPathway JSON, produced by Pydantic `model_dump(mode="json")`. Lossless.

## Import verification

`daisugi pathways import <path>` is not a raw deserialization — it runs the full Z3 verification pipeline against the declared envelope before admitting the pathway to the local store. Failure modes (structured `PathwayImportError` codes):

- **`SCHEMA_INCOMPATIBLE`** — missing `daisugi:` key, missing `pathway` subkey, or `schema_version` newer than this library.
- **`VERIFICATION_FAILED`** — the plan template does not verify against the envelope *now*, on this machine, with this library version. Includes the list of violations.
- **`DUPLICATE_ID`** — a pathway with the same ID already exists. Pass `--overwrite` to replace.

This means even a maliciously crafted skill file cannot smuggle in a plan that exceeds its declared envelope — the envelope constrains execution, and the plan is re-checked against it. The trust boundary is "do I trust this envelope's declared permissions," not "does this plan do what it claims."

## Consumer integration

### Claude Code
Pathway skills drop into the standard Claude Code skill directory and are discoverable via the normal skill mechanism. The LLM reads the markdown body; `daisugi pathways import` reads the frontmatter.

### Hermes
Hermes is Python-based and organizes skills as directory bundles. A pathway skill can sit alongside Python skills in the same collection; Hermes treats the `.md` as documentation and `daisugi` executes it as a compiled pathway when matched. The specific Hermes skill-index convention may require wrapping the markdown file in a small directory structure — see the Hermes integration notes when that target is live.

### OpenClaw
OpenClaw is Node.js. The skill format is identical; consumption goes through the MCP server (`daisugi mcp serve` → `find_pathway` tool) since OpenClaw cannot import Python directly. The skill `.md` file lives in OpenClaw's skill directory for discovery; execution routes through MCP.

### Other MCP-speaking agents
Any agent that speaks MCP can call `find_pathway` / `envelope_for` / `verify_plan` against a `daisugi mcp serve` process. Pathway skills are the distribution mechanism; MCP is the invocation mechanism.

## Export formats

`daisugi pathways export <id> -o <path> --format X` also supports:

- **`json`** — bare canonical bundle (no markdown wrapper). For machine consumers that don't want to parse frontmatter.
- **`mermaid`** — plan DAG as a Mermaid flowchart + permissions summary. Paste into a README or PR description. One-way (not re-importable).
- **`md`** — human-readable audit report. One-way.
- **`smtlib`** — SMT-LIB2 proof artifact. Run `z3 pathway.smt2` to independently confirm the pathway verifies without installing openDaisugi. One-way, niche, useful for security auditors.

## Versioning

- Bumping `BUNDLE_SCHEMA_VERSION` is a breaking-import change. Library readers reject newer bundles.
- Additive fields on `CompiledPathway` (new defaulted fields) do **not** require a schema bump — Pydantic tolerates them.
- Removing or renaming a field requires a bump plus (ideally) an on-import migration path.

## Portability caveats

- **Path abstraction.** A pathway that hardcodes `/home/alice/tmp` is not portable. Future versions will add a `metadata.portability` tag distinguishing `portable` from `requires_path_adaptation`, with template substitution on import. For v0.7, portability is the caller's responsibility.
- **Embedding model.** `task_embedding` is specific to the embedding model that produced it. If you import a pathway embedded by `all-MiniLM-L6-v2` into a store whose lookup uses a different model, similarity scoring will be meaningless. The `embedding_model` field is informational for now — v0.8+ may add compatibility checks.
- **Python/library version.** The bundle encodes the openDaisugi version that produced it. Imports from significantly older or newer versions should work if the schema version matches, but Z3 re-verification is the authoritative gate.
