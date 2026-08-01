# openDaisugi CLI review — against clig.dev

*2026-08-24. Reviewed the Typer CLI (`daisugi`, 19 top-level commands + sub-apps) against the
Command Line Interface Guidelines. Verdict: **solid, above average for a tool this size.** Good
stream discipline, wide machine-output coverage, mapped exit codes, help-on-no-args. A handful of
real gaps; one fixed here, the rest recommended. Checklist source: `iss-skills/skills/
tool-interface-design/references/clig-cli-checklist.md`.*

## Scorecard

| Area | State | Notes |
|---|---|---|
| Naming | **pass** | `daisugi` — lowercase, memorable, no collision. |
| No-args → help | **pass** | `no_args_is_help=True` on every Typer app. |
| stdout vs stderr | **pass** | 52 `err=True` sites — errors/status go to stderr. |
| Machine output | **pass** | 28 `--json` flags across commands. |
| Exit codes | **pass** | 58 mapped `typer.Exit(code=…)`; 0 on success. |
| `--version` | **FIXED** | Was missing at top level; added an eager `--version` callback → prints `0.43.0`, exit 0. |
| `--help` quality | **partial** | Typer renders clean help, but most commands lack **worked examples**. |
| `NO_COLOR` / TTY | **partial** | No explicit `NO_COLOR` handling; color is Rich/Typer-managed. The one place that emits ANSI directly (`dashboard` live loop) is already `isatty`-guarded. |
| `--plain` | **gap** | `--json` exists; no `--plain` tabular mode for `grep`/`awk`. |
| `--quiet` / `--verbose` | **partial** | Present on some commands, not global. |
| Confirmation on destructive | **partial** | Only 2 `confirm` sites. Audit gate-disarm, uninstall, fold, and any overwrite. |
| XDG config | **deviation** | Data lives in `~/.opendaisugi`, not `~/.config`. Defensible (it's a data store, not just config) but not XDG. |

## Fixed in this pass
- **`daisugi --version`** (clig.dev "ship `--version`"). Added a top-level eager callback; prints
  the version to stdout and exits 0. Tested (`test_version_flag_prints_version_and_exits_zero`).

## Recommended, in priority order
1. **Respect `NO_COLOR` / `--no-color` explicitly.** Even though Rich mostly handles it, set the
   behaviour deliberately so a piped or `NO_COLOR=1` run is guaranteed plain. Low risk, high trust.
2. **Add worked examples to `--help`** on the commands people reach for first (`install`, `gate`,
   `dashboard`, `onboard`, `metrics`). clig.dev: lead with real examples. Cheap, high-value.
3. **Audit destructive commands for confirmation.** Anything that deletes, disarms the gate,
   overwrites, or rewrites history should confirm on a TTY and honour `--yes`/`--no-input`. Match
   the guard rail to the blast radius (the doctrine's law 7).
4. **`--plain` output** where `--json` exists, for `grep`/`awk` pipelines. Nice-to-have.
5. **XDG-aware data dir (optional, non-breaking).** Read `XDG_DATA_HOME` / `XDG_CONFIG_HOME` when
   set, else fall back to `~/.opendaisugi`. Do **not** move the default — that orphans existing
   journals/pathways for no real gain (no-backwards-compat-theater cuts both ways: a gratuitous move
   is churn).
6. **Global `--quiet`/`--verbose`** on the root callback for consistency across subcommands.

## Not a problem (deliberately)
- The `~/.opendaisugi` data dir is a *store* (journal, pathways, config, gate state), not just a
  config file. Many tools keep a single `~/.tool` home. XDG-awareness via env vars is the clean
  compromise; a forced relocation is not worth the breakage.
- Typer already gives consistent flag parsing, `-h`/`--help`, and subcommand structure — the
  clig.dev "use a real arg parser" and "consistent subcommands" boxes are ticked by the framework.
