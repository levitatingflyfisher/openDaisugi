# ADR-0026: Four parts get new names: warden, router, steward and Pando R2

- **Status:** Accepted
- **Date:** 2026-10-30

## Context

Four parts had names that no longer said what the part does, or that clashed.

- The **gate** watches every tool call and records it. By default it only watches. When enforce
  is on, it also stops the calls outside the envelope. "Gate" says only the second half.
- The **gateway** is a local proxy in front of the model provider. Its inner policy
  (`off|rules|switchyard|knn|model`) was already called "router" in the config, so the proxy and
  its policy shared a word.
- The **foreman** is whoever sits in the chair of the floor, on any model. It is a role.
- The never-ending conversation memory had no name of its own, only "the foreman's chat".

The owner ruled on 2026-10-28, 2026-10-29 and 2026-10-30 (ledger items R-1, R-2, A1).

## Names

| Old | New | In one line | Lore name |
|---|---|---|---|
| gate | **warden** | An observer and a supervisor: it watches and records every action; with enforce on, it stops the ones outside the envelope. | bannin |
| gateway | **router** | A local proxy in front of your model provider. | |
| the router (inner policy) | **routing mode** | How the router picks a model: `off`, `rules`, `switchyard`, `knn`, `model`. | |
| foreman | **steward** | The role in the chair: whoever answers you, on any model. | bantō |
| conversation memory | **Pando R2** (Reversible Recollections) | The steward's log and its summary tree. | |
| the `delegate` MCP tool | **spawn** | The tool and the module; the English word "delegate" stays in prose. | |

Switchyard stays the name of NVIDIA's backend. `daisugi`, `coppice`, `sprig`, `journal`, `garden`,
`envelope`, `verdict`, `route` (the dry-run verb) and `fence` do not change.

In code, Pando R2 is `pando`: five lowercase letters, no clash, a valid identifier in Python, Go
and Rust, and the same style as the other part names. `r2` is not in the identifier, because
`pando_r2` reads as a version. This wave builds no `pando` package and moves no store. That is
ADR-0023.

Inside coppice, the pane step that was called "router" (direct, gateway, switchyard) is now
"route" (direct, router, switchyard), so no sentence says "router router". A matcher "gate" in
the herdr detection manifests is a different thing and is now a "condition".

## Decision

A clean break (R-2). No alias on any surface a person types or edits: command, flag, config key,
environment variable, protocol verb, pane label. Wave 1 already refused `--gate`; `--warden` is
refused the same way, since the hook is part of every install.

Five narrow reads of the owner's existing data stay, each one line of code, because the data
cannot be replaced:

1. `daisugi install` moves `<data>/gate` to `<data>/warden` and `<data>/gateway` to
   `<data>/router`, rewrites the six old config keys, brings an old hook entry to the new words
   (its mode stays), and moves old pi and OpenCode extensions under `<data>/moved-aside/`. It
   refuses with one line when a server still listens or when both directories exist.
2. coppice keeps the steward's on-disk directory name `foreman` (`steward.DirName`) until ADR-0023
   moves the store once. Old log lines with `kind: foreman` read as `steward`. Backup areas keep the
   name, so an old backup restores.
3. The warden's private-path rules guard both names of the log directory and of the turn directory.
4. coppice refuses `[foreman]` and a top-level `gateway =` in `coppice.toml`, and names the new key.
5. `install` names an envelope that still allows `opendaisugi/delegate`. It edits no envelope.

A names check, `scripts/names-check.sh`, fails while an old word is in a tracked file or path. A line
tagged `names-ok` reads an old name on purpose.

## Consequences

- The owner runs `daisugi install` once. `docs/start-here.md` has the steps.
- The golden fixtures of 16 suites are regenerated from the oracle and pinned again.
- Friends on old docs: none exist yet.
- Dated plans, ADRs 1 to 22, research, exploration, the CHANGELOG history and `ADJUDICATIONS.md`
  keep their words. They are records.

## Records

A plan dated 2026-09 that says "the gate" is a true statement about 2026-09. Rewriting it would make
the record lie about what the parts were called, and `git log -S` would stop finding the decision by
its word. This ADR is the record of the rename.
