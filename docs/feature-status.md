# Feature status matrix

One row per feature. "Maturity" is what we're comfortable claiming today,
not what the spec aspires to.

| Feature | Since | Maturity | Notes |
|---|---|---|---|
| `Envelope` / `Permission` / `Invariant` models | v0.0.1 | Production-candidate | Schema stable since v0.0.1; minor additions each version. |
| Predicate algebra AST | v0.9.0 | Production-candidate | Grammar in `predicate.py`; meta-DSL for LLM-authored invariants. |
| `verify(plan, envelope)` — Stage 1 static | v0.0.1 | Production-candidate | Core thesis. Z3-backed since v0.11.0. Strict mode + alias resolution since v0.27.0. |
| Strict-mode verification (`strict=` param) | v0.27.0 | Production-candidate | Default-on at `stakes` high/physical. Opaque-invariant rejection. `resolve_strict()` helper. |
| Z3 vacuity detection (`vacuity.py`) | v0.27.0 | Working | `check_vacuity(expr)` — tautology / contradiction / non_trivial. Wired into alias registration and invariant evaluation. |
| `AliasRegistry` on core `verify()` | v0.27.0 | Working | `verify(..., aliases=reg)` resolves alias refs before evaluation. Unresolved alias is a Violation, not a silent pass. |
| `compile_to_z3` — real Z3 trees | v0.11.0 | Production-candidate | Emits InRe/String/Real, not BoolVal. |
| Regex → Z3 translator | v0.11.0 | Working | Defined subset of `re`; see limitations.md. |
| `Contract` + `verify_delegation` | v0.11.0 | Working | Skills-as-contracts. v0.12 will add signature verification. |
| `envelope_subsumes` | v0.11.0 | Working | ShellStep-shaped symbolic steps; file/network admitted structurally. |
| `Supervisor` / `RunSession` / `StepExecutor` | v0.1.0 | Working | Python-level permission gate; not an OS sandbox. |
| `Journal` + replay | v0.1.0 | Working | SQLite-backed; used in all examples. |
| `generate_envelope` (LLM-authored) | v0.0.1 | Working | Requires LLM API; caches prompt/model version. |
| Stage 2 output verification | v0.9.0 | Working | `verify_completed_step` — discharges soft nodes at runtime. |
| `CartesianMoveStep` / `JointMoveStep` / `GripperStep` / `SimulationResetStep` | v0.8.0 | Experimental | Step types work; MuJoCo executor is the weak link. |
| `MuJoCoExecutor` | v0.8.0 | Experimental | Lazy-imported; requires `[robotics]` extra. See limitations.md. |
| `Distiller` — pathway compilation from journal | v0.3.0 | Working | Used by `Daisugi.tend()`. |
| `PathwayStore` + semantic search | v0.3.0 | Working | Requires `[search]` extra (sentence-transformers ~80MB). |
| Pathway export / import (`portability.py`) | v0.7.0 | Experimental | Schema versioned; no marketplace. |
| `Gardener` (A/B, prune, merge, regression) | v0.4.0 | Working | Pathway lifecycle management. |
| Tier-1 pluggable local-model providers | v0.4.0 | Working | `ClaudeCodeTier1Provider`, `LiteLLMTier1Provider`. |
| Token-tier accounting | v0.4.0 | Working | `tier_stats` derives routing stats from journal. |
| LoRA training-data pipeline | v0.5.0 | Working | `emit_jsonl` from journal. `python -m opendaisugi.lora.train` runs SFTTrainer + QLoRA end-to-end (v0.10.0). Requires `[lora]` extra. |
| MCP server | v0.10.0 | Working | `daisugi mcp serve`; exposes envelope_for, verify_plan, etc. Requires `[mcp]` extra. |
| ClaudeCode LLM backend | v0.12.0 | Working | `OPENDAISUGI_LLM_BACKEND=claude-code` or `--llm claude-code` routes every LLM call through `claude -p`. No API key required. |
| Integration adapters (Hermes, OpenClaw) | v0.10.0 | Working | Narrow wrappers; see `examples/integrations/`. |
| CLI (`daisugi generate-envelope`, `verify`, `journal`, etc.) | v0.1.0 | Working | Typer-based. |
| Envelope cache | v0.1.2 | Working | SQLite; prompt-version-keyed. |
| Refinement-aware envelope gen | v0.2.1 | Working | Past refinements injected as hints. |
| CEGAR refinement (one-shot) | v0.2.0 | Working | Multi-round refinement is future work. |
| Low-stakes permissive envelope | v0.1.3 | Working | Skips LLM call for trivial tasks. |
| Contract cryptographic signing | v0.15.0 | Working | `opendaisugi.signing` ships `sign_contract` / `verify_signature_raw` / `TrustedSignerRegistry`. `contracts._verify_signature` consults the registry against `trusted_signers`. Requires `[sign]` extra. |
| Distributed pathway registry | v0.25.0 | Working | `GitPathwayStore` + `PathwayBundle` + `daisugi registry init/pull/publish/status/pull-and-tend`. Git-backed, content-addressed bundles. |
| Passive capture hook (`daisugi hook record`) | v0.21.0 | Working | Observation only, fail-open by contract — never blocks the host. Feeds distillation. |
| Call-time gate (`daisugi gate`) | v0.35.0 | Working | ADR-0007. Deny-by-default `verify()` per live tool call; shadow default, `--enforce` flips, `disarm` kill switch. Claude Code deny path contract-tested live (exit 2); Hermes/OpenClaw block shapes emitted but **unverified**. ~0.6 s round trip, import-dominated. |
| `AgenticStep` + `AgenticExecutor` | v0.36.0 | Working | Tool-using delegation inside the parent envelope (ADR-0007 §3). Static `--allowedTools` wall (envelope ∩ request) + the call-time gate in the sub-agent's own hook config, registered outside the workspace. Physical stakes refuse it. Live-tested: an out-of-envelope read attempted by a real sub-agent is denied by the gate. |
| Adversarial corpus + merge gate (`daisugi gate audit`) | v0.37.0 | Working | Roadmap Stage 3. Deterministic, content-addressed corpus (13 attacks / 7 categories, 9 benign) run through the real gate; `tests/test_adversarial.py` is a required CI check (attack-denial 1.00; FP rate 0.33, all known/budgeted). Comparison arms (no-gate / literal-glob / gate) published. Yellow-paper §8 states the safety-only guarantee + limits. Live layer's stochastic bait-rate not yet run at N for CIs. |
| Gate onboarding (`daisugi gate quickstart` / `init`) | v0.38.0 | Working | Roadmap Stage 6. `starter_envelope` generates a reviewable tight default; quickstart prints the shadow→report→enforce→disarm flow. Tutorial in `docs/tutorials/`. Time-to-first not yet measured on a clean machine. |
| Distillation-fidelity benchmark harness (`opendaisugi.benchmark`) | v0.39.0 | Experimental | Roadmap Stage 4 *ruler*: seeded, content-addressed paired cold/warm runs, t-based 95% CIs, safety-direction check, `meets_stage4_bar`. Execution is an injected runner; **no numbers yet** — the real measurement waits on a local-model runner (verified with a fake runner only). |
| String length / arithmetic in algebra | — | Planned | Grammar extension. |

## Legend

- **Production-candidate** — Core thesis, tested, used in shipped examples, schema stable.
- **Working** — Functional, tested in unit + integration tests, not heavily battle-tested in production.
- **Experimental** — Shipped but has sharp edges. May not work on your configuration.
- **Planned** — Hook or spec exists; real implementation deferred to a named future version.

## Rule of thumb

If your evaluation depends on a row marked **Experimental** or **Planned**,
hold off. If it depends on **Working**, expect to read source and report
issues. **Production-candidate** rows are the audit-ready surface.
