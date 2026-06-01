# Deployment

Operator's guide for running opendaisugi inside an organization. Answers
the "how do I put this in front of my team and keep it boring"
questions: install, config, data, logs, keys, upgrades.

This is a plain library, not a service. There is nothing to stand up, no
daemon to restart, no port to open. Deployment is about where its bytes
live and where its records go.

## Install

```bash
uv add opendaisugi                   # core
uv add opendaisugi[sign]             # + ed25519 contract signing
uv add opendaisugi[search]           # + pathway semantic search
uv add opendaisugi[lora]             # + LoRA training-data export
uv add opendaisugi[robotics]         # + MuJoCo executor
uv add opendaisugi[all]              # everything
```

Core has no native deps beyond `z3-solver` (pulled automatically) and
`pydantic`. Extras are heavy on purpose — install only what a given
service needs.

Python 3.12 or newer. No 3.11 backport planned; we use match statements
and newer typing throughout.

## Data directory

All on-disk state lives under a single configurable directory (default
`~/.opendaisugi`). Nothing outside that directory is written unless the
caller explicitly constructs a `Journal`, `EnvelopeCache`, or
`PathwayStore` with a different path.

| File                         | Purpose                                  | Safe to delete?  |
| ---------------------------- | ---------------------------------------- | ---------------- |
| `envelope_cache.db`          | SQLite cache of generated envelopes      | Yes (regenerates)|
| `pathways.db`                | SQLite store of distilled pathways       | Yes (rebuild via `Distiller.tend`) |
| `journal/*.jsonl`            | Run history (one file per day)           | Yes (loses audit trail) |
| `journal/refinements.jsonl`  | Per-step rejection + recompute records   | Yes (loses audit trail) |
| `trusted_signers.json`       | Trusted-signer registry (public keys)    | No (rebuild from team records) |

To relocate: `Daisugi(data_dir="/var/lib/opendaisugi")` or
`OPENDAISUGI_DATA_DIR=...`. To share across hosts (e.g. NFS), be aware
that SQLite performs poorly over NFS — prefer a node-local directory and
aggregate via log shipping instead.

## Configuration

Two surfaces: a Python config object (`opendaisugi.Config`) and env
vars. Env vars never override explicit constructor args; env is the
default when no arg is passed.

```python
from opendaisugi import Daisugi

d = Daisugi(
    model="anthropic/claude-sonnet-4-20250514",
    z3_timeout_ms=500,
    data_dir="/var/lib/opendaisugi",
)
```

Env vars in use:

| Var                           | Meaning                                          |
| ----------------------------- | ------------------------------------------------ |
| `OPENDAISUGI_DATA_DIR`        | Override default data directory                  |
| `OPENDAISUGI_LLM_BACKEND`     | `claude-code` routes via a local Claude Code install instead of API |
| `ANTHROPIC_API_KEY`           | Used by `generate_envelope` when backend is the API |

Configs are data — stash them in whatever secret manager your org
already uses. The library reads them once at construction.

## Logging

opendaisugi emits structured records on hierarchical loggers rooted at
`opendaisugi`. A `NullHandler` is attached by default so importing the
library is silent — records only surface if the host attaches its own
handler.

Logger names:

| Logger                     | What fires                                 |
| -------------------------- | ------------------------------------------ |
| `opendaisugi.verify`       | `verify.pass`, `verify.fail`               |
| `opendaisugi.contracts`    | `delegation.allow`, `delegation.deny`      |
| `opendaisugi.signing`      | `signing.verify_ok`, `signing.verify_failed`, `signing.signer_unknown` |
| `opendaisugi.supervisor`   | `run.start`, `run.end`, `run.rejected_by_verify`, `run.step_halted`, `run.step_recomputed`, `run.approval_denied` |

Records carry structured fields via `extra=` (e.g. `run_id`,
`envelope_id`, `violation_count`, `violation_stages`, `step_id`,
`approved_by`, `status`). These are attributes on the `LogRecord` —
pick them up with any `structlog` processor, the `python-json-logger`
formatter, or a custom `Filter`.

Minimal stdout JSON example:

```python
import logging
from pythonjsonlogger.json import JsonFormatter

h = logging.StreamHandler()
h.setFormatter(JsonFormatter())
root = logging.getLogger("opendaisugi")
root.addHandler(h)
root.setLevel(logging.INFO)
```

Route by logger prefix: e.g. mirror `opendaisugi.supervisor` to an audit
sink (run decisions) and `opendaisugi.verify` to a metrics sink (policy
evaluation throughput).

## Trusted-signer registry

When contracts are signed (v0.15+ `[sign]` extra), verification resolves
trusted signers through a JSON registry at
`~/.opendaisugi/trusted_signers.json`. The file is a plain dict of
signer-name → base64-public-key. Manage it however your org manages
allowlists — hand-edited, committed to a config repo, pushed by
configuration management, or built at install time from a central
directory.

Programmatic access:

```python
from opendaisugi import TrustedSignerRegistry, default_registry_path

reg = TrustedSignerRegistry.load(default_registry_path())
reg.add("robin-v1-distiller", "BASE64PUBKEY==")
reg.save()
```

Rotation is a file replacement: generate a new keypair, publish the
public half into the registries of all callers, re-sign published
contracts with the new private key. The library does no automatic
revocation — removing a signer from the registry is the revocation.

## Backup and restore

Everything is on-disk files. Backup is a file copy of the data
directory; restore is the reverse. SQLite DBs should be backed up while
no writer is active (or use `sqlite3 .backup`); JSONL journals are
append-only and safe to copy in-flight.

For retention: JSONL journals roll daily. Delete older files to bound
growth; no index is kept outside the files themselves.

## Multi-instance

The library is process-local and reentrant. Multiple processes can run
concurrently against different data directories without coordination.
Sharing a single SQLite cache or journal across processes works (SQLite
handles the locking) but does not scale past a few concurrent writers —
give each process its own data directory and aggregate via log shipping
if you need org-level visibility.

No background threads, no network listeners, no file watchers. The only
I/O is synchronous reads/writes to the data directory plus outbound LLM
calls from `generate_envelope` / `Distiller`.

## Upgrades

Minor versions are additive (new operators, new exports, new optional
extras). Breaking changes go in the CHANGELOG under a dedicated
"Breaking" section. Pin `opendaisugi>=X.Y,<X.Y+1` if you want to hold a
minor line.

The envelope and pathway-bundle schemas are versioned independently of
the library. When a schema version bumps, the library reads older
formats and writes the newest; a one-time migration runs on first
access. See `docs/pathway-skill-format.md` for the pathway bundle.

## Diagnostics

```bash
# Dump cached envelope stats
python -c "from opendaisugi import Daisugi; print(Daisugi().cache.stats())"

# Replay a past run against the current code
python -c "from opendaisugi import Daisugi; Daisugi().journal.replay('run_abc123')"

# List trusted signers
python -c "from opendaisugi import TrustedSignerRegistry, default_registry_path; \
           print(TrustedSignerRegistry.load(default_registry_path()).names())"
```

For deeper inspection: set `opendaisugi.verify` to `DEBUG` to see every
Z3 timeout and predicate eval; set `opendaisugi.supervisor` to `DEBUG`
to see every approval decision. The library does not emit PII into
logs — envelope IDs, step IDs, and violation stages are all non-payload
identifiers.
