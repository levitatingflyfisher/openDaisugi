# ADR-0009 — Adopt `ruff format` as the one formatter

**Status:** Accepted (Phase E — stylistic formalization).

## Context

The repo had `ruff` *lint* configured (`F/E9/B/I`, 100-col soft, `CONVENTIONS.md`)
but **no formatter**. Style was hand-maintained, so 262 of 310 files diverged from
a consistent format and every diff carried avoidable whitespace noise. The
`[tool.ruff]` block itself anticipated this: "tighten later via a new ADR." This
is that ADR.

## Decision

`ruff format` is the single source of formatting truth. Defaults, plus the
existing `line-length = 100` and `target-version = "py312"`. No `black`, no
hand-formatting debates.

- **Config:** `[tool.ruff.format]` in `pyproject.toml` (defaults).
- **Sweep:** one deliberate, semantics-neutral commit reformats the whole tree.
  Neutrality is proven by the full test suite passing unchanged across the sweep.
- **CI:** `ruff format --check` joins `ruff check` as a required gate, so new code
  lands conformant and never re-churns.

The lint stance is unchanged (still `F/E9/B/I`; `E501` still not enforced —
`ruff format` reflows code but deliberately does not force-split long strings/URLs,
matching the grandfathered-lines note in `CONVENTIONS.md`).

## Consequences

- One large formatting commit in history; `git blame` users can `--ignore-rev` it
  (recorded in `.git-blame-ignore-revs`).
- Contributors run `ruff format` (or an editor-on-save hook); CI rejects unformatted
  diffs. Zero style review comments from here on.
- Function-local / conditional imports (a deliberate openDaisugi idiom for optional
  deps and cycle-avoidance) are preserved — `ruff format` reorders whitespace, not
  import placement, and `E402` stays disabled.
