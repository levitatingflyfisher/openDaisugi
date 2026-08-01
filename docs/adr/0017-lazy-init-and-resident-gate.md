# ADR-0017 — Lazy package exports and a resident gate: the gate's cost was import, not Z3

- **Status:** Accepted
- **Date:** 2026-08-28

## Context

`import opendaisugi` ran 54 eager submodule imports (~460-530 ms). Z3 was a real but
minor part of that; `agentic_executor`, networkx (via the DAG-check import), and pydantic
model building made up most of the rest. Every gate call is a fresh process, so every
tool call paid the whole package-init cost — not just Z3's. Verification itself is 0.6 to
4 ms. The harness docs and ADR-0007 attributed the cost to Z3; measuring `-X importtime`
showed the actual breakdown was import-dominated by the package's own `__init__.py`, plus
`cli.py`'s own top-level imports of `envelope`, `journal`, and `ingest` (all of which
transitively pull `opendaisugi.verify`, and `verify.py` pulled networkx unconditionally
for the DAG stage even when a plan never reaches it).

## Decision

1. PEP 562 lazy exports in `opendaisugi/__init__.py`: the 54 eager `from opendaisugi.X
   import ...` lines become a generated `_LAZY` map plus a `__getattr__` that imports a
   name's owning module on first access, caches it, and returns it. The `Daisugi` facade
   moves to `facade.py` (kept out of `__init__.py` so nothing about it is eager). Every
   public name in `__all__` still resolves as `opendaisugi.<name>`.

   One real gotcha: `verify` is both an exported function
   (`opendaisugi.verify.verify`) and its own owning submodule's basename. Python's
   import system unconditionally binds `pkg.verify = <the submodule>` the first time
   *anything* imports `opendaisugi.verify` — including `facade.py`'s own `from
   opendaisugi.verify import verify as _verify` — which silently overwrites the lazy
   function export via ordinary attribute lookup (`__getattr__` is never re-invoked,
   because normal lookup now "succeeds" with the wrong cached value). Fixed with a
   swapped-in module `__class__` whose `__getattribute__` re-resolves that one colliding
   name through `_LAZY` on every access, ignoring whatever the import system's own
   auto-bind wrote into `__dict__`.

2. `cli.py`'s own top-level imports of `opendaisugi.envelope`, `opendaisugi.journal`,
   `opendaisugi.ingest`, `opendaisugi.verify`, and `opendaisugi.supervisor` move into the
   command functions that use them — these were pulling Z3/networkx at CLI-import time
   too, independent of the package `__init__`.

3. `verify.py` imports networkx (`opendaisugi.dag.check_dag`) only at the DAG stage,
   inside both call sites (`_verify` and `verify_step`), not at module top level.

4. A resident gate (`daisugi gate serve`, `gate_server.py`) on `<root>/gate.sock` and a
   stdlib-only client (`gate_client.py`) that the installed hook now runs
   (`-m opendaisugi.gate_client`, replacing `-m opendaisugi.gate`). The client falls back
   to the in-process gate on *any* server failure — no socket, refused, timeout, garbage
   or partial reply, wrong types, or a reply from a socket that fails an `os.lstat`
   ownership/mode/socket-type trust guard (never `Path.exists()`, which follows symlinks).
   The fallback is the same `run_argv()` the server itself calls, so the process entry
   (`main`), the server, and the client's fallback cannot drift. Fail-closed is unchanged:
   a well-formed, cleanly "allowed" reply from an untrusted socket is still rejected.

   The server is thread-per-connection (`ThreadingMixIn`), not sequential — a later plan
   blocks a gate call for up to 90 s on an operator, and a single-connection server would
   freeze every other session for that whole window. `z3_checks.py`/`predicate_z3.py`/
   `subsumption.py`/`vacuity.py` call bare `z3.Solver()` (the process-wide default Z3
   context), and Z3 releases the GIL for native work — so two handler threads inside Z3
   at once is a genuine data race on shared native state, not merely a crash risk: it can
   produce a wrong, non-crashing verdict. A module-level `z3_checks.Z3_SOLVE_LOCK` (a plain
   `threading.Lock`) now serializes every AST-build-and-solve region reachable from
   `verify()`: `check_envelope_self_consistency`, `check_plan_against_envelope`,
   `predicate_z3.verify_predicate_z3` (including its `compile_to_z3` call — building
   Z3 terms concurrently with another thread's solve reproducibly raised
   `Z3Exception('context mismatch')` before the lock covered it too),
   `subsumption.py`'s `_patterns_subsume`/`envelope_subsumes` (reached from `verify()`'s
   Stage 1b skill-delegation check), and `vacuity.py`'s `_compute_vacuity` (both its
   contradiction and tautology checks — reached from `verify()`'s per-invariant/
   postcondition predicate check and from `aliases.py`'s registration path; one unlocked
   participant on the shared context would have voided the mutual-exclusion guarantee
   for every other locked solve running concurrently, so this was a required completion
   of the fix, not an optional extra). Solves are 0.6-4 ms, so serializing them is free;
   the lock is scoped to the solve only, never around I/O — the later 90 s operator-ask
   must stay outside it, or the server would re-serialize and reintroduce head-of-line
   blocking. `jit_metrics.py`'s own `compile_to_z3` call is *not* covered — it's an
   offline analysis tool, not reachable from the resident server's request path — ledgered,
   not fixed.

   The lock closes the verdict-correctness race for every region it wraps, but not a
   narrower one found while proving it: a guarded function's local Z3 wrapper objects
   (the `Solver`, the `Bool`/`String`/`Int` terms it built) are freed when its stack frame
   is torn down, which happens *after* `with Z3_SOLVE_LOCK:` has already released the
   lock — so their native ref-count decrements (`Z3_dec_ref`) run off-lock. This is not
   an exit-time-only artifact: that off-lock decrement can fire during normal operation
   too, overlapping another thread's in-flight *locked* solve on the same shared context.
   What was actually observed — `Z3_del_context` spinning at process exit — is
   accumulated corruption from that overlap surfacing when the context finally tears
   down, not evidence the overlap itself is confined to exit. The residual fail-open
   window this leaves is structurally bounded, not closed: `verify()`'s `allow` is
   exactly `violations == []`, so a race has to manufacture that precise empty state
   rather than corrupt arbitrary bookkeeping; violation lists are extend-only through the
   pipeline; the pure-Python Stage-1 permission check (no Z3 involved) already denies most
   unsafe plans before any solve runs; `enforce` mode denies on any exception, and
   corrupted native state is a plausible way to raise one, not to quietly return "allow";
   and the resident server's own fail-closed fallback means any anomaly there routes to a
   fresh in-process gate on a single, uncontended Z3 context — no concurrency, so no
   version of this race, on that path. None of that makes the window zero. The airtight
   fix is a `z3.Context()` per thread, threaded through the verify pipeline — deferred as
   a known residual, not attempted here; it would touch `z3_checks.py`, `predicate_z3.py`,
   `subsumption.py`, and `vacuity.py` far beyond a solve-region lock.

## Measured

| Probe | Before | After |
|---|---|---|
| `import opendaisugi` (3 warm runs) | ~460-530 ms | ~20-25 ms |
| `daisugi --help` (warm, median of 3) | 0.75-0.80 s | 0.45-0.48 s |
| `python -m opendaisugi.gate` cold call (client, no server) | ~0.70-0.80 s | unchanged — this is the documented fallback path |
| gate round trip with the resident server (median of 20, in-process `ask_server`) | n/a | ~1.3 ms |
| `python -m opendaisugi.gate_client` subprocess with the server running (median of 20, includes Python startup) | n/a | ~46 ms |

`daisugi --help` did not drop as far as `import opendaisugi` because Typer/Click/Rich's
own import and help-rendering cost (unaffected by this ADR) is now the dominant remaining
term.

## Consequences

- The 30 s hook timeout in `gate_settings_json` can stay; a cold fallback still fits.
- sprig's hard-coded 10 s in `cli.go:76` is now comfortable; the 30 s in `sprig-hook`
  and `sprig-mcp` are conservative. Not changed here.
- An existing installed hook still on `-m opendaisugi.gate` is rewritten in place to
  `-m opendaisugi.gate_client` by `_patch_claude_gate` on the next plain
  `daisugi install --gate` — not warned-and-skipped — so the one real user gets the fast
  path without an `--uninstall` round trip. A hook with a genuinely different mode/config
  still warns, unchanged.
- A tool that imports `opendaisugi` and reads names via `getattr` at import time now
  pays on first access instead of at import. No known caller does.
- The resident server's Z3 thread-safety posture (§Decision 4) is a deliberate, bounded
  design, not an oversight: `Z3_SOLVE_LOCK` serializes every AST-build-and-solve region
  reachable from `verify()`. It does not close the narrower off-lock object-teardown
  race — that can overlap a live locked solve during *normal* operation, not just at
  process exit, though accumulated corruption from it only became visible as a
  `Z3_del_context` hang at exit. The residual fail-open window this leaves is
  structurally bounded (§Decision 4: `allow` requires exactly `violations == []`,
  extend-only violation lists, the Z3-free Stage-1 permission short-circuit, `enforce`
  denying on any exception, and single-context fallback on any server anomaly), not
  eliminated. The airtight fix — `z3.Context()` per thread — is deferred as a known
  residual, documented rather than silently accepted; see `gate_server.serve()`'s
  docstring and `z3_checks.Z3_SOLVE_LOCK`'s.
