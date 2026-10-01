# Cold-start speed budgets

Roadmap item 3d: measure the cold start of every command and every gate
check. Then set a budget and a test for each.

The budgets are in `speed-budgets.json`, the machine-readable twin of this
page. `scripts/speed.py` measures. `tests/test_speed.py` enforces.

## Why the gate check matters most

An agent pays the gate check on every tool call. A Claude Code session makes
hundreds of calls, so a slow gate makes a slow agent. The other commands run
once per human action. They only need to feel instant.

## What is measured

One process start, from `exec` to exit. Each run is a fresh process. The first
round is a warm-up and is dropped. So the numbers are warm-disk cold starts:
the page cache holds the binary, but the process has cached nothing. Each
surface runs 21 times. The runs are interleaved: round 1 starts every surface
once, then round 2, and so on. One burst of noise then hits all surfaces
alike, and a ratio between two surfaces stays fair.

| Tool | Implementations | Commands |
|---|---|---|
| `daisugi` | Python, Go, Rust | `--version`, `--help`, `gate check`, `gate status` |
| `daisugi-gate` (the hook binary) | Python in-process, Python client with no resident server, Go, Rust | the hook check |
| `coppice` | Go, Rust | `--version`, `--help` |
| `sprig` | Go, Rust | `--version`, `--help` |

The Python `daisugi` runs as `python -m opendaisugi.cli_entry`. That is the
module the `daisugi` console script calls.

The gate check gets one fixed fake hook payload and one fake envelope. The
payload is a `Bash` call, `ls -la src`. The envelope allows `ls`, `cat` and
`echo`, in enforce mode. The check runs the whole path: parse, shell
decomposition, verification, audit write.

Two guards stop a broken gate from looking fast:

- Before it times a gate, the script sends one call the envelope must deny
  (`rm -rf /work/x`). It needs the real deny: exit 2 and a `DENIED` line on
  stderr. A gate that allows it did not load the envelope, and the surface is
  an error.
- A timed run must print `{"continue": true}`. A deny or a crash is an error.

`sprig --help` exits 2 in Go (its flag package) and in Rust (the port copies
the exit code). The script accepts exit 2 only there, and only when the output
looks like help (`Usage` or the `-version` flag line).

Everything is fake. HOME and every XDG directory are in a scratch directory on
real disk (`.speed/scratch` in the checkout, or `SPEED_SCRATCH`). PATH is
`/usr/bin:/bin`. No command opens a network connection, a model, a live floor
or a real harness.

The `py-client` row is the entry the installed Claude Code hook uses
(`gate_client`). With a resident gate it answers in about 40 ms, by its own
docstring. The row here is the case with no resident gate, which is the first
call of every session.

## How to run

```
scripts/speed-build.sh all           # under heavy.sh; builds the Go and Rust binaries
uv run --no-sync python scripts/speed.py            # table
uv run --no-sync python scripts/speed.py --json     # JSON
uv run --no-sync python scripts/speed.py --check    # exit 1 over budget
SPEED_FACTOR=3 uv run --no-sync python scripts/speed.py --check   # slow runner
```

`speed-build.sh` writes `<tool>-go` and `<tool>-rs` into `.speed/bin`
(`SPEED_BIN_DIR`). The Rust `daisugi` builds Z3, which takes about half an
hour. A copied target directory does not help, because the Z3 cmake cache
names the directory it was made in. Set `SPEED_DAISUGI_RS_DIR` to the
`clients/rust` directory of a checkout that is already built, to use its
binaries.

## Measured medians

Measured on 2026-10-15 on a shared box: a 4-core Fedora workstation where
other builders ran. The runs went through `heavy.sh`, so none of my own jobs
ran in parallel. The box was never idle. Treat the numbers as upper bounds for
a quiet machine. Two full passes of 21 runs each agreed within 3 percent on
every surface, except for the Python `--version` row (see below), which has
one pass.

The Rust `daisugi` and `daisugi-gate` rows describe the main checkout's
release builds of 2026-10-14 and 2026-10-09. They were not rebuilt from this
tree. The Go binaries, `coppice-rs` and `sprig-rs` were built from this tree.

Each median in the table is the larger of the two passes. That is the number
the budget came from.

| Surface | Median ms | p95 ms | Budget ms |
|---|---:|---:|---:|
| daisugi-gate Rust, hook check | 7.4 | 11.6 | 15 |
| daisugi-gate Go, hook check | 12.2 | 28.9 | 25 |
| daisugi-gate Python, hook check | 380.2 | 398.8 | 775 |
| daisugi-gate Python client, no resident gate | 377.5 | 396.1 | 775 |
| daisugi Rust, `gate check` | 8.6 | 9.3 | 20 |
| daisugi Go, `gate check` | 14.7 | 16.6 | 30 |
| daisugi Python, `gate check` | 494.7 | 528.7 | 1000 |
| daisugi Rust, `gate status` | 3.4 | 3.8 | 9 |
| daisugi Go, `gate status` | 6.3 | 7.3 | 15 |
| daisugi Python, `gate status` | 460.6 | 491.3 | 925 |
| daisugi Rust, `--version` | 2.4 | 2.5 | 8 |
| daisugi Rust, `--help` | 2.3 | 2.6 | 8 |
| daisugi Go, `--version` | 5.2 | 5.5 | 15 |
| daisugi Go, `--help` | 5.2 | 5.7 | 15 |
| daisugi Python, `--version` | 42.7 | 372.4 | 90 |
| daisugi Python, `--help` | 474.1 | 493.3 | 950 |
| coppice Rust, `--version` | 1.6 | 5.0 | 7 |
| coppice Rust, `--help` | 1.5 | 2.6 | 7 |
| coppice Go, `--version` | 3.9 | 11.6 | 9 |
| coppice Go, `--help` | 3.8 | 8.4 | 9 |
| sprig Rust, `--version` | 1.2 | 3.5 | 7 |
| sprig Rust, `--help` | 1.1 | 2.3 | 7 |
| sprig Go, `--version` | 2.7 | 6.8 | 8 |
| sprig Go, `--help` | 2.8 | 6.5 | 8 |

The p95 column is the worse of the two passes. Some p95 values are high
because other builders loaded the box during a pass. The medians are stable.

The Python `--version` p95 of 372 ms is a pass in which two of the 21 runs hit
the old slow path of about 360 ms. This pass is the only one made after the
fast path was added. I could not tell the cause from the numbers. It may be a
`.pyc` compile on a first start after a rebase. The median is 42.7 ms.

The gate check, in short: Rust 7 ms, Go 12 ms, Python 380 ms. The native gates
are 30 to 50 times faster than the Python oracle. An earlier hand measurement,
kept in a memory note, put the Python hook round trip at about 0.55 s. This
measurement finds 0.38 s for the in-process gate on this box.

## How the budgets were set

One rule for every surface:

    budget = round_up(max(2 x median, median + 5 ms))

`median` is the larger of the two 21-run medians. `round_up` goes to a tidy
step: 1 ms under 10 ms, 5 ms under 100 ms, 25 ms under 1 s, 100 ms above.
The 2x margin covers a noisy box. The 5 ms floor stops a 1 ms command from
getting a 2 ms budget that one scheduler hiccup breaks. `speed.suggest_budget`
holds the rule. `scripts/speed.py --suggest` prints a fresh set from a new
measurement. Raise a budget only with a measurement and a reason in the
commit.

Four relative rules sit beside the absolute ones. The Go gate and the Rust
gate must each take under 0.1 of the Python gate time. The same holds for
`daisugi gate check`. Both sides run in the same interleaved pass. The real
ratio is about 0.03. A relative rule has no machine speed in it, so it holds
on a slow runner.

## What CI checks

The `speed` job in `.github/workflows/clients.yml` rebuilds nothing. It takes
the Go and Rust `daisugi` and `daisugi-gate` binaries that the `go` and `rust`
jobs built. It names them `<tool>-go` and `<tool>-rs`, sets `SPEED_BIN_DIR`,
and runs `tests/test_speed.py`.

On a CI runner (`CI` is set) the native binaries get the relative rules only.
The runner is slower and noisier than the box that set the absolute budgets.
The absolute native budgets are a local check. They run when `CI` is not set,
or when `SPEED_STRICT=1`. The Python rows keep their absolute budgets in CI,
at the CI factor.

`coppice` and `sprig` are built in other jobs, so the `speed` job has no
binaries for them. A relative rule whose fast binary is missing skips with a
reason. It never fails.

## How the tests stay quiet

- They compare the median of 5 runs after a warm-up. Never one run.
- The factor is 3 by default (`ci_factor` in the JSON). Each budget already
  has a 2x margin. So a surface fails only when it runs about 6x slower than
  on the measuring box. `SPEED_STRICT=1` sets the factor to 1 for a quiet
  local box. `SPEED_FACTOR=n` sets it directly.
- Native tests skip when their binaries are not built. The Python tests always
  run. A test that measured nothing fails.
- The tests of the checking logic use made-up numbers. They cannot flake.

## How the import figures were found

```
PYTHONPATH=src python -X importtime -m opendaisugi.cli --version 2>&1 \
  | grep -E "\| [a-z_.A-Z0-9]+$" | sort -t'|' -k2 -n | tail
PYTHONPATH=src python -X importtime -m opendaisugi.gate --mode enforce \
  --root <gate root> --format claude < hook.json 2>&1 | sort -t'|' -k2 -n | tail
```

The second column is the cumulative time of an import in microseconds.
`-X importtime` adds some cost of its own, so use the figures for ranking, not
as exact times.

## The Python `--version` fast path

`daisugi --version` took 363 ms. About 190 ms of that was top-level imports in
`cli.py` that `--version` never uses: `opendaisugi.approval` (which pulls in
pydantic, 130 ms), `asyncio`, `yaml`, `parsers`, `executor` and `run_session`.

The console script now calls `opendaisugi.cli_entry:main`. That module checks
for a lone `--version` first. It prints the version and a newline, the same
bytes as before, and exits 0, without importing the CLI. Any other argument
loads the CLI. The median is now 42.7 ms. `python -m opendaisugi.cli` still
takes the old path, so the recorded cli cases are unchanged.
`tests/test_cli_entry.py` checks that the fast path imports no pydantic,
typer, click, yaml or asyncio, and that its output equals the CLI's.

## Concerns and larger finds

These are not fixed here.

1. **Python `daisugi --help` and every other command still load all of
   `cli.py`.** That is 450 ms. The same import list applies. A fix moves those
   imports into the functions that use them, across a 7,000-line file. It
   needs care with the Typer annotations.
2. **The Python gate is import bound.** Of its 380 ms, about 135 ms is pydantic
   model construction in `opendaisugi.models`. About 35 ms is `predicate`,
   20 ms is Z3, and 25 ms is `config` (PyYAML). A resident gate (`gate serve`)
   removes all of it, and `gate_client` already uses one. The cold row is what
   the first call of a session pays.
3. **The resident Python gate is not measured.** It needs a background server
   and a socket. Its docstring claims about 40 ms.
4. **Go `daisugi gate check` is 2x slower than Rust** (15 against 8 ms). The Go
   `daisugi-gate` is 12 against 7 ms. Both are far inside the budget. If the
   Go gate needs to be faster, start with the runtime start and the embedded
   tables, not the check itself.
5. **No `sprig-hook` or `sprig-mcp` rows.** `sprig-hook` runs a gate command,
   so it costs the gate plus its own start. Its own start is not measured.
6. **The Rust `daisugi` rows depend on a prebuilt binary** (see above). A full
   rebuild is a half hour of Z3.
7. **Installed scripts need a reinstall** to pick up the new console entry
   (`uv sync` or `uv pip install -e .`).
