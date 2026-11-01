# ADR-0027: Argument rules for MCP tools

- **Status:** Accepted
- **Date:** 2026-11-01

## Context

`mcp_allowlist` named a tool. A tool in the list ran with any arguments. A read tool such as
`mcp__filesystem__read_file {"path": "/etc/passwd"}` passed whatever `file_read` said. The owner
asked (2026-10-22) for "argument-level MCP policy": allow a tool only with certain arguments. The
shape of an argument is the server's job. What the argument means is the envelope's job.

## Decision

- `Permission.mcp_rules` is a list of `{tool, args, known}`. `args` are predicate expressions over
  the call's arguments. One new op, `path_in`, places a path argument inside `file_read` or
  `file_write`.
- A tool a rule names is decided by its rules alone, and an unknown top-level key denies. Any
  other tool is decided by `mcp_allowlist` on its name.
- A string argument that is an absolute path (or `~/`) and exists is a read, for every admitted
  MCP tool.
- `equals`, `not_equals`, `in_set` and `not_in_set` no longer treat `true` as `1`, in every scope.
- `matches` in a rule must be anchored. The kernel keeps `re.search` everywhere else.
- A schema is an authoring-time check. `warden schemas import` stores it; `warden register`
  reads it.
- Caps are counts: 262144 characters, 64 levels.

## Consequences

- The allowlist alone no longer admits an existing path outside `file_read`. A denial names the fix
  (`path_in` in a rule).
- Python is the oracle. The Go and Rust ports match it message for message, in the warden and in
  the standalone verifier. Their subsumption twin is narrower than the oracle's Z3 proof and
  refuses where it cannot match expressions (`clients/ADJUDICATIONS.md`, MP-R-1).
- Rules are per call. Cross-call claims, results, live schemas and sprig's `Spawn` stay open.
