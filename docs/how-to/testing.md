# Run every test suite

This page lists each suite, the command that runs it on your machine, and
the CI job that runs it on a push. No suite needs a model, a `claude`
binary or a real upstream. The ones that could reach one refuse to, or skip.

Two rules keep a local run safe:

- Run heavy suites one at a time. The cgo and cargo builds, `native.sh`,
  the golden compares and the browser suite each take several GB of
  memory. On a small box, run each one under a cap, for example
  `systemd-run --user --scope -p MemoryMax=5G <command>`, with
  `GOFLAGS=-p=2` and `CARGO_BUILD_JOBS=2`.
- Put scratch directories on real disk. On some systems `/tmp` is RAM.

## Python: the oracle

```sh
uv sync --locked --extra dev      # once, and after uv.lock changes
uv run --no-sync ruff check .
uv run --no-sync ruff format --check .
HF_HUB_OFFLINE=1 uv run --no-sync pytest -q
uv run --no-sync pytest -q tests/test_adversarial.py
uv run --no-sync daisugi gate audit
```

`uv.lock` pins every package and its hash. `--locked` stops when the lock
no longer matches `pyproject.toml`; run `uv lock` and commit both files.
`[dev]` leaves out `[search]` (torch and the CUDA wheels). A test that needs
sentence-transformers skips, or stubs it with the
`sentence_transformers_installed` fixture in `tests/conftest.py`.

CI: the `lint-and-test` job in `.github/workflows/ci.yml`.

## coppice and sprig

coppice links libghostty-vt through cgo. `harness/coppice/scripts/toolchain.sh`
installs zig and builds the library once.

```sh
cd harness/coppice
go vet ./...
gofmt -l .                                  # prints nothing when clean
COPPICE_REQUIRE_TOOLCHAIN=1 go test -p 1 ./... -count=1
COPPICE_REQUIRE_TOOLCHAIN=1 go test -p 1 ./... -count=1 -race
cd ../sprig && go vet ./... && go test ./... -count=1 -race
```

`COPPICE_REQUIRE_TOOLCHAIN=1` turns a missing library into a failure, not
a skip. Many tests bind a unix socket under `TMPDIR`, and a socket path may
not pass 108 bytes. With a long `TMPDIR` those tests fail with
`bind: invalid argument`; leave `TMPDIR` unset or short.

CI: the `coppice` and `sprig` jobs in `.github/workflows/coppice.yml`. They
run only when a file under `harness/coppice/` or `harness/sprig/` changes.

## The Go and Rust daisugi

Both link static Z3 5.1.0 and tree-sitter-bash from the native prefix.
`clients/go/scripts/native.sh` builds that prefix once into
`~/.cache/opendaisugi/native/<stamp>` and checks it on every later run. The
first build takes a long time; do not delete the prefix.

```sh
clients/go/scripts/native.sh
clients/go/scripts/build.sh                  # daisugi, daisugi-gate, conform
cd clients/go
export CGO_CFLAGS="-O2 -g -DOPENDAISUGI_NATIVE=$(scripts/native.sh --print-key)"
go test -p 1 ./... -count=1
for p in pathway-probe gateway-probe recall-probe; do go build -o "$SCRATCH/$p" ./cmd/$p; done
```

```sh
. clients/rust/tools/z3-build-env.sh
cd clients/rust
cargo build --release --locked
cargo test --release --locked
```

The golden compares run a binary over the cases the oracle recorded in
`clients/fixtures/` and fail on any disagreement or stale fixture. Give
each run its own scratch directories:

```sh
export DAISUGI_GATE_SCRATCH=$SCRATCH/gate DAISUGI_CLI_SCRATCH=$SCRATCH/cli \
  DAISUGI_PATHWAY_SCRATCH=$SCRATCH/pathway DAISUGI_GARDEN_SCRATCH=$SCRATCH/garden \
  DAISUGI_GATEWAY_SCRATCH=$SCRATCH/gateway DAISUGI_RECALL_PROBE=$SCRATCH/recall-probe \
  HF_HUB_OFFLINE=1
B=clients/go                                  # or clients/rust/target/release
uv run --no-sync python clients/gate_compare.py --binary $B/daisugi-gate
uv run --no-sync python clients/cli_compare.py --binary $B/daisugi
uv run --no-sync python clients/pathway_compare.py --binary $B/daisugi --probe $SCRATCH/pathway-probe
uv run --no-sync python clients/garden_compare.py --binary $B/daisugi
uv run --no-sync python clients/gateway_compare.py --binary $B/daisugi
```

For the Rust client the probes are in `clients/rust/target/release`. The
gateway compare answers from its own fake upstream. `--oracle` also reruns
the Python side and `--fuzz N` adds seeded fuzz items; both take longer.

CI: `.github/workflows/clients.yml`. The `native` job builds the prefix on
a cache miss and saves it, keyed on the native stamp. The `go` and `rust`
jobs restore it, build, test and upload the binaries. The `compare` job runs
each compare for each language as its own matrix entry.

## End to end: coppice, the web floor and the release tarball

```sh
cd harness/coppice/e2e
npm ci && npx playwright install chromium
./run.sh                                      # see harness/coppice/e2e/README.md
```

It starts coppice on a scratch socket and data directory with fake
harnesses, checks the TUI in coppice's own virtual terminal and the web
floor in headless chromium, and saves screenshots and screens. Set
`COPPICE_BIN` to test a coppice you built already.

```sh
scripts/release.sh 0.0.0-dev
scripts/smoke-tarball.sh dist/opendaisugi-0.0.0-dev-linux-x86_64.tar.gz
```

`smoke-tarball.sh` unpacks the tarball into a scratch HOME, with PATH set to
the binaries and `/usr/bin:/bin`. It installs the gate, checks that the
hook allows a Read and denies `rm -rf /etc`, and starts and stops a
coppice server.

CI: the `e2e` job in `.github/workflows/clients.yml`. It uploads the
screenshots and logs as the `e2e` artifact.
