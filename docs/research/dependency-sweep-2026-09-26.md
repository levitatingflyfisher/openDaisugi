# Dependency sweep (2026-09-26)

Status: research. Nothing here is decided. The owner decides every item marked **Open**.

This document lists every third-party package, crate, module and native library that
openDaisugi pulls in. It gives each one a verdict: **keep**, **make optional**, **replace**
(with our own small code) or **drop**. It ends with a ranked list of the ten changes worth
the most, and a list of license problems.

The owner's rule, as the test for each verdict: "be amazingly functional, using all the
imports we need for collaboration and precision, and none that we don't; code is cheap, and
reinventing the wheel is fine if it buys efficiency, exactness, a better implementation, or
vertical control." Also: minimize dependencies, use nothing we cannot legally use, and stay
local-first.

## 1. How the counts were made

Each ecosystem counts a different thing. The table names the unit.

| Ecosystem | Source read | Unit | Count |
|---|---|---|---|
| Python, core install | `pyproject.toml`, `uv.lock` | packages in the closure of `[project].dependencies`, Linux | **16** (6 direct, 10 transitive) |
| Python, every extra | `uv.lock` | `[[package]]` entries in the lock | **176** |
| Python, `[dev]` | `uv.lock` | closure of the `[dev]` extra | **143** |
| Go, `clients/go` | `go list -deps ./...` | third-party modules linked | **2** (5 in `go list -m all`) |
| Go, `harness/coppice` | `go list -deps ./...` | third-party modules linked | **8** (10 in `go list -m all`) |
| Go, `harness/sprig` | `go list -deps ./...` | third-party modules linked | **2** |
| Go, all three | union of the above | distinct modules linked | **10** |
| Rust, `clients/rust` | `cargo tree -e normal` | direct crates / linked crates / with build crates / lock entries | **15 / 64 / 73 / 130** |
| JavaScript | package manifests, source imports | runtime npm packages | **1** (`web-tree-sitter` in `clients/ts`), plus 3 dev |
| Lean, `clients/lean` | `lake-manifest.json` | packages | **0** |
| Native C, C++ and Zig | `clients/go/scripts/native.sh`, NOTICE files | libraries compiled in | **12** (section 6) |

`go list -m all` lists modules in the module graph that are never linked. For `clients/go` these
are `golang.org/x/mod`, `x/sync` and `x/tools` (from `x/text`'s own `go.mod`). For coppice
they are `mattn/go-colorable` and `mattn/go-isatty` (from `qrterminal`'s `go.mod`). The
linked counts come from `go list -deps -f '{{if not .Standard}}...'`.

**Caveat on every Python count.** `uv.lock` is git-ignored (`.gitignore` line 19) and was last
written 2026-09-24. It has drifted from the working tree:

- The lock resolves `mcp` 2.2.0, but the uncommitted `pyproject.toml` now says `mcp>=1.27,<2`.
  The venv holds `mcp` 1.30.0. The `mcp` row below uses 1.30.0 from the venv metadata.
- The lock resolves `ruff` 0.16.6, but `pyproject.toml` pins `ruff==0.16.9`.

Closures were computed with a script over `uv.lock` that drops dependencies marked only for
Windows, macOS or emscripten. Licenses come from the installed wheel metadata in `.venv`
(`importlib.metadata`), except where a row says otherwise.

## 2. Python

### 2.1 Core (`pip install opendaisugi`)

| Package (locked) | What it is for | Imported in `src/opendaisugi` | Transitive | License | Network on import or by default | Verdict |
|---|---|---|---|---|---|---|
| pydantic 2.13.5 | Every model: `Envelope`, `ActionPlan`, pathways, config | 23 files (`models.py`, `predicate.py`, `pathway.py`, `llm.py`, `cli.py`, ...) | 4 (pydantic-core, annotated-types, typing-extensions, typing-inspection) | MIT | None | **Keep.** The ports copy its validation messages (`clients/go/internal/pmodel`, `clients/rust/src/pathways/pmodel`). It is the oracle's schema. |
| z3-solver 5.1.0.0 | The verifier's SMT back end | `predicate_z3.py`, `subsumption.py`, `regex_to_z3.py`, `vacuity.py`, `z3_checks.py`, `portability.py` | 0 (51 MB wheel with `libz3.so`) | MIT | None | **Keep.** This is the thesis. Same 5.1.0 in Go and Rust. |
| networkx 3.6.1 | DAG checks: `DiGraph`, `find_cycle`, `topological_sort`, `topological_generations` | `dag.py` only (117 lines) | 0 | BSD-3-Clause | None | **Replace.** Four graph calls. Go and Rust already do it by hand (`clients/rust/src/dag.rs`). About 60 lines of Kahn's algorithm, ported back from the Rust. |
| pyyaml 6.0.3 | Config, journal, install files | 8 files (`config.py`, `journal.py`, `cli.py`, `install.py`, `cockpit.py`, ...) | 0 | MIT | None | **Keep.** Uses `safe_load`, `safe_dump`, `dump`. The Go client ports its output (`clients/go/internal/pyyaml`). |
| typer 0.27.2 | The `daisugi` CLI | `cli.py` (5,636 lines) | 6 (rich, pygments, markdown-it-py, mdurl, shellingham, annotated-doc) | MIT | None | **Keep for now.** The Go CLI copies typer's parse and help behavior (`clients/go/internal/cli/parse.go`, `routercmd.go`), and `clients/cli_compare.py` compares stdout and stderr. A replacement is a change in three languages. See 2.4 for a bug. |
| numpy 2.5.3 | Similarity, clustering, lexical matcher, audio | 11 files (`_similarity.py`, `pathway_store.py`, `distiller.py`, `gateway_cluster.py`, `voice/audio.py`, ...) | 0 | BSD-3-Clause AND 0BSD AND MIT AND Zlib AND CC0-1.0 | None | **Keep.** Zero transitive packages. Core distill and the lexical matcher need it (ADR-0019). |

`rich` is never imported by `src/opendaisugi`. It arrives only through typer.

### 2.2 Extras

"New" means packages beyond the core closure of 16.

| Extra and package (locked) | What it is for | Imported in | Transitive (new) | License | Network on import or by default | Verdict |
|---|---|---|---|---|---|---|
| `[generate]` litellm 1.100.0 | Model calls: envelope generation, `llm_check`, episode splitting, voice cleanup, delegating executor, tier-1 | `llm.py`, `llm_check.py`, `parsers/claude_code.py`, `voice/cleanup.py`, `delegating_executor.py`; `tier1.py` through `llm.py` | 54 (48 new: openai, boto3, botocore, aiohttp, tiktoken, tokenizers, huggingface-hub, hf-xet, jsonschema, ...) | MIT per wheel metadata. The repo keeps `enterprise/` under a separate license ([LICENSE](https://github.com/BerriAI/litellm/blob/main/LICENSE)); the wheel has `litellm/proxy/enterprise_billing/` with no license header of its own (checked, 92 MB wheel). | **Yes on import**: fetches its model cost map from GitHub (`litellm_core_utils/get_model_cost_map.py`, `httpx.get`). Fixed for us by `LITELLM_LOCAL_MODEL_COST_MAP` in `src/opendaisugi/__init__.py` (commit 5450879f). `litellm.telemetry = True` is set in its `__init__.py` but nothing in the wheel reads it (grep, 1.100.0). | **Replace** (rank 2). |
| `[generate]` instructor 1.16.0 | JSON-mode structured replies, pydantic validation, re-ask | `llm.py` only | 39 (27 new: openai, aiohttp, jinja2, tenacity, docstring-parser, jiter, ...) | MIT | None found (not verified by a socket guard) | **Replace** (rank 2). |
| `[search]` sentence-transformers 6.0.1 | The default matcher, `all-MiniLM-L6-v2` | `_search.py` | 50 (39 new: torch, triton, six `nvidia-*` and three `cuda-*` wheels, transformers, scikit-learn, scipy, sympy, ...) | Apache-2.0. The CUDA wheels torch pulls are NVIDIA-proprietary (section 8). | **Yes by default when `[search]` is installed** (every dev and CI venv): `SentenceTransformer(...)` goes through `huggingface_hub`, which makes an HTTP request even when the file is cached, unless `HF_HUB_OFFLINE` is set ([HF docs](https://huggingface.co/docs/huggingface_hub/package_reference/environment_variables)). Per the docs, not run here. | **Drop** (rank 3). The int8 backend runs the same model with no torch. |
| `[potion]` model2vec 0.9.0 | The potion matcher | `_search.py` (`StaticModel.from_pretrained`) | 22 (19 new: huggingface-hub, hf-xet, tokenizers, safetensors, joblib, jinja2, httpx, ...) | MIT | **Yes on use**: `from_pretrained(model_id)` has no revision pin and no sha256. The Go and Rust ports pin revision `bf8b0566...` and three sha256s (`clients/go/internal/embed/potion/fetch.go`). | **Replace** (rank 4). Port the Go/Rust loader and tokenizer back. |
| `[int8]` onnxruntime 1.30.0 | Runs MiniLM int8 with no torch (ADR-0021) | `_search.py` | 4 (3 new: protobuf, flatbuffers, packaging) | MIT | None. Model files come through our own `_model_fetch.py`, sha256-pinned. | **Keep, optional.** |
| `[int8]` tokenizers 0.23.2 | Loads MiniLM's `tokenizer.json` | `_search.py` | 15 (13 new: huggingface-hub, hf-xet, httpx, fsspec, ...) | Apache-2.0 | None on import. The pulled `huggingface_hub` does nothing unless called. | **Replace** (rank 4). BERT WordPiece is small; the Go potion tokenizer (`clients/go/internal/embed/potion/tokenizer.go`) shows the pattern. |
| `[voice]` faster-whisper 1.2.1 | Speech to text on CTranslate2 | `voice/engines.py` | 22 (19 new: ctranslate2, onnxruntime, av, huggingface-hub, tokenizers, ...) | MIT | **Yes on use**: `WhisperModel("tiny.en")` downloads from Hugging Face with no revision pin (`voice/engines.py` line 83, `voice/pins.py`). Per the HF docs, a cached model still makes a request. | **Keep, optional.** Pin the model through `_model_fetch.py` and pass a local path (**Open**, section 9). |
| `[voice]` sounddevice 0.5.6 | Microphone capture | `voice/ptt.py` | 2 (cffi, pycparser) | MIT | None | **Keep, optional.** |
| `[voice]` av 18.1.0 | webm/opus decode when `ffmpeg` is absent | `voice/audio.py` | 0 packages, but `av.libs/` bundles 32 shared libraries (72 MB), including `libx264` and `libx265` | BSD-3-Clause for PyAV. The bundled x264 and x265 are GPL-2.0-or-later (verified: both `.so` files are in `.venv/.../av.libs/`). See [pyav-ffmpeg#262](https://github.com/PyAV-Org/pyav-ffmpeg/issues/262). | None | **Drop** (rank 8). |
| `[voice-parakeet]` sherpa-onnx 1.13.8 | The opt-in second speech engine | `voice/engines.py` | 0 | Apache-2.0 (not installed here; not verified from metadata) | Model is a 460 MB manual download; never fetched automatically (`voice/pins.py`) | **Keep, optional.** |
| `[mcp]` mcp 1.30.0 (venv; lock says 2.2.0) | The MCP server (11 tools) on the SDK's FastMCP class; mcp 2 renamed FastMCP to MCPServer, hence the `<2` pin | `mcp_server.py` (406 lines) | 25 on Linux (about 20 new: starlette, sse-starlette, uvicorn, httpx, httpx-sse, pyjwt, python-multipart, pydantic-settings, python-dotenv, jsonschema, ...) | MIT | None | **Replace** (rank 6). The 1.x to 2.x rename already forced a pin. |
| `[gateway]` httpx 0.28.1 | Gateway upstream client; model host probe | `gateway_asgi.py`, `model_host.py` | 6 (5 new: anyio, certifi, h11, httpcore, idna) | BSD-3-Clause | None. The gateway is a network proxy by purpose. | **Keep, optional.** |
| `[gateway]` uvicorn 0.52.4 | Serves the gateway ASGI app | `gateway_asgi.py` | 2 (click, h11) | BSD-3-Clause | None | **Keep, optional.** The Go and Rust gateways already exist, so the Python one is the oracle only. |
| `[shell]` tree-sitter 0.26.0 | Bash grammar parsing for compound-shell decomposition (ADR-0010) | `shell_decompose.py` | 0 | MIT | None | **Keep, optional.** |
| `[shell]` tree-sitter-bash 0.25.1 | The bash grammar | `shell_decompose.py` | 0 | MIT | None | **Keep, optional.** |
| `[tui]` textual 8.2.8 | `daisugi dashboard --tui` and the other TUIs | `tui.py`, `tui_base.py`, `tui_tree.py`, `tui_sessions.py`, `tui_wiring.py`, `start.py` | 7 (2 new: mdit-py-plugins, platformdirs) | MIT | None | **Keep, optional.** |
| `[tui]` textual-serve 1.1.3 | `daisugi dashboard --serve` in a browser | `tui.py` | 20 (15 new: aiohttp, aiohttp-jinja2, jinja2, yarl, multidict, ...) | MIT | **Yes on every page view**: its `templates/app_index.html` loads `https://fonts.googleapis.com/css?family=Roboto%20Mono` (grep of the installed package). | **Drop** (rank 7). coppice web is the browser surface. |
| `[sign]` cryptography 50.0.1 | Ed25519 signing of skill contracts | `signing.py` | 2 (cffi, pycparser) | Apache-2.0 OR BSD-3-Clause | None | **Keep, optional.** Never write our own Ed25519. |
| `[robotics]` mujoco 3.12.0 | MuJoCo executor | `executor_mujoco.py`, `vla_executor.py` | 5 (absl-py, etils, glfw, pyopengl, numpy) | Apache-2.0 (not installed here; not verified from metadata) | None known | **Keep, optional.** `vla_executor.py` also imports torch and transformers, which `[robotics]` does not declare. |
| `[lora]` torch, transformers, peft, trl, bitsandbytes, datasets, accelerate | LoRA fine-tuning | `lora/train.py` | 60 total (49 new, including pandas, pyarrow, the `nvidia-*` wheels) | Apache-2.0 and mixed; `nvidia-cublas` is proprietary | `from_pretrained` downloads; `datasets` and `transformers` send HF telemetry unless disabled (per the HF docs) | **Keep, optional, never in `[dev]`.** It already is not. |

### 2.3 Dev and test only (`[dev]`)

| Package | Use | License | Verdict |
|---|---|---|---|
| pytest 9.1.1, pytest-cov 7.1.0, pytest-asyncio 1.4.0 | The suite | MIT, MIT, Apache-2.0 | **Keep.** pytest-cov installs `a1_coverage.pth`, which runs on every interpreter start (it only acts when `COVERAGE_PROCESS_START` is set). |
| ruff ==0.16.9 | Lint and format (ADR-0009) | MIT | **Keep.** Lock says 0.16.6; see section 1. |
| build 1.6.0 | sdist and wheel | MIT | **Keep.** |
| twine 7.0.0 | PyPI upload | Apache-2.0 | **Make it a release-time tool** (`uvx twine`). It pulls 22 new packages (keyring, secretstorage, jeepney, readme-renderer, ...) into every dev and CI venv. |
| `opendaisugi[search, mcp, shell, sign, gateway, tui, voice, potion, int8, generate]` | Test coverage for every extra | as above | See rank 3: `[search]` puts torch and the CUDA wheels (about 5.3 GB in this venv: `torch` 1.2 GB, `nvidia` 3.2 GB, `triton` 895 MB, of a 6.1 GB venv) into every dev and CI install. `[generate]` puts litellm into CI. |
| Playwright (not a Python dependency) | coppice web smoke test | Apache-2.0 | **Keep.** A machine tool at `~/.cache/oh-visual-loop/node_modules/playwright`; the test skips without it (`harness/coppice/internal/web/smoke_test.go`). |

### 2.4 Undeclared imports (bugs)

1. **`click` (verified).** `cli.py` line 26 does `import click` and line 929 uses
   `click.Choice`. typer 0.27 vendors click as `typer._click` and no longer depends on it
   (`uv.lock`, typer's dependencies). Nothing in the core closure provides `click`. Test run
   here: `sys.modules['click'] = None; import opendaisugi.cli` raises
   `ModuleNotFoundError` at line 26. A bare `pip install opendaisugi` therefore cannot start
   `daisugi`. It works in every dev venv only because uvicorn, mcp, litellm and model2vec
   bring `click` in.
2. **`huggingface_hub`.** `model_registry.py` (lines 82 and 128, reached from `cli.py` line
   3529) and `bench/layers/matcher.py` import it. No extra declares it.
3. **torch and transformers for `vla_executor.py`.** No extra with the executor declares them.

### 2.5 Network calls outside the model call itself

No file in `src/opendaisugi` outside `bench/` sets `HF_HUB_OFFLINE`, `HF_HUB_DISABLE_TELEMETRY`
or `DO_NOT_TRACK`. So:

- The **default** config (`config.py` line 125: `matcher_model = "all-MiniLM-L6-v2"`) reaches
  huggingface.co each time the MiniLM embedder is built, when `[search]` is installed (every dev
  and CI venv). Per the HF docs, a cached file still gets a metadata request. Not run here.
- On a bare install the same default names a backend the install cannot run.
  `effective_matcher` in `_search.py` falls back to lexical and warns once per process. So the
  default is either a network call or a warning, never a clean run.
- potion (Python) and `WhisperModel` do the same, and neither pins a revision.
- A logged-in Hugging Face user's token is sent on those requests by default
  (`HF_HUB_DISABLE_IMPLICIT_TOKEN`, same page).

This is the same class of problem as the litellm cost-map fetch. It sits on the default path.

## 3. Go

### 3.1 `clients/go` (module `daisugi-verify`, go 1.25.12)

| Module | For | Where | License | Network | Verdict |
|---|---|---|---|---|---|
| github.com/mattn/go-sqlite3 v1.14.52 | Pathway store and trace journal; bundles SQLite 3.53.4 | `internal/pathways/store.go`, `internal/tracejournal/journal.go` | MIT; SQLite public domain | None | **Keep.** cgo is already required for Z3. |
| golang.org/x/text v0.30.0 | NFKC normalization, as Python's `urllib.parse` does | `internal/gate/urlparse.go` | BSD-3-Clause | None | **Keep.** Exact Unicode tables are the point. |

Graph only, not linked: `golang.org/x/mod`, `x/sync`, `x/tools`.

The Go client also carries its own ports of things Python takes from libraries:
`internal/llm` (instructor and litellm, two back ends), `internal/embed/potion` (model2vec and
tokenizers), `internal/pyyaml`, `internal/pyjson`, `internal/pyre`, `internal/netproxy`.

`clients/go/.gomodcache/` (5.1 MB, git-ignored by `clients/go/.gitignore`) holds
`mvdan.cc/sh` v2.6.4 and some test modules. Nothing links them. It is stale scratch.

### 3.2 `harness/coppice` (go 1.26.0, toolchain go1.26.8)

| Module | For | Where | License | Network | Verdict |
|---|---|---|---|---|---|
| go.mitchellh.com/libghostty v0.0.0-20260908040635-9f448dfe8052 | cgo binding to the Ghostty VT engine | `internal/vt/vt.go` only (enforced by `internal/boundary/boundary_test.go`) | MIT | None at run time | **Keep.** No tags upstream; the author promises no API stability (`harness/coppice/PINS.md`). The confinement to `internal/vt` is the right control. |
| github.com/creack/pty v1.1.24 | PTY per pane | `internal/pane/pty.go` | MIT | None | **Keep.** |
| github.com/coder/websocket v1.8.15 | Phone client transport | `internal/web/ws.go` | ISC | None | **Keep.** |
| github.com/BurntSushi/toml v1.5.0 | Config and detect manifests | `internal/config/config.go`, `internal/detect/manifest.go` | MIT | None | **Keep.** |
| github.com/mdp/qrterminal/v3 v3.2.1 | Sign-in and CA QR in the terminal | `internal/web/qr.go` | MIT | None | **Keep.** |
| rsc.io/qr v0.2.0 | QR encoder under qrterminal | indirect | BSD-3-Clause | None | **Keep.** |
| golang.org/x/term v0.45.0 | Raw mode for `coppice attach` | `internal/attach`, `internal/tui/run.go`, `internal/cli/cli.go` | BSD-3-Clause | None | **Keep.** |
| golang.org/x/sys v0.47.0 | Under x/term and pty | indirect | BSD-3-Clause | None | **Keep.** |

Graph only, not linked: `mattn/go-colorable`, `mattn/go-isatty`.

Push notifications go only to a self-hosted ntfy server the user names; there is no default
(`internal/web/push.go` line 90). Coppice ships Apache-2.0 agent-detection manifests from Herdr
with attribution (`harness/coppice/NOTICE`).

### 3.3 `harness/sprig` (go 1.25.12)

| Module | For | License | Verdict |
|---|---|---|---|
| golang.org/x/term v0.45.0, golang.org/x/sys v0.47.0 | Terminal raw mode | BSD-3-Clause | **Keep.** Sprig is on hold with the owner. |

## 4. Rust (`clients/rust`, crate `daisugi-verify-conform`)

Direct dependencies from `Cargo.toml`, with the size of each one's own subtree
(`cargo tree -e normal,build -p <crate>`):

| Crate (locked) | For | Subtree | License | Network | Verdict |
|---|---|---|---|---|---|
| serde 1.0.229, serde_json 1.0.151 (`preserve_order`) | JSON in and out | 7 | MIT OR Apache-2.0 | None | **Keep.** |
| jiter =0.14.0 | pydantic-core's own JSON parser, so envelope parsing matches the oracle byte for byte | 20 (bitvec, lexical-*, ahash, ...) | MIT | None | **Keep.** Exactness is the reason. |
| z3-sys =0.13.1 (`vendored`, z3-src 501.0.1) | Z3 5.1.0, static | 6 | MIT | Build fetches nothing; source is in the crate | **Keep.** |
| tree-sitter 0.25.10, tree-sitter-bash 0.25.1 | Bash decomposition | 17 | MIT | None | **Keep, align to 0.26** (rank 10). |
| regex 1.13.1 | Pattern work | 4 | MIT OR Apache-2.0 | None | **Keep.** |
| rusqlite =0.31.0 (`bundled`, libsqlite3-sys 0.28 = SQLite 3.45.0) | Pathway store, trace journal | 19 | MIT; SQLite public domain | None | **Keep, bump** (rank 10). |
| rustls 0.23.45 (ring), rustls-native-certs 0.8.4 | HTTPS for `llm_check` and the distiller's model call | 13 + 3 | Apache-2.0 OR ISC OR MIT; ring is Apache-2.0 AND ISC; subtle is BSD-3-Clause | Only when a model call runs | **Keep.** |
| num-bigint 0.4.8, num-traits 0.2.19 | Python-int token counts in the gateway | 3 | MIT OR Apache-2.0 | None | **Keep.** Already in the tree through jiter. |
| miniz_oxide 0.8.9 | gzip and deflate of upstream answers, as httpx does | 1 (adler2) | MIT OR Zlib OR Apache-2.0 | None | **Keep.** Pure Rust. |
| thiserror 1.0.69 | Error derives | proc-macro, pulls `syn` 2 | MIT OR Apache-2.0 | None | **Replace, low value.** The tree has two `syn` versions (2.0.119 and 3.0.3). Hand-written `Display` impls drop one. |
| libc 0.2.189 | System calls | 0 | MIT OR Apache-2.0 | None | **Keep.** |

Dev only: rcgen 0.14 (test CA). `clients/rust/NOTICE` lists the linked crates and their licenses.
I found no gap by reading it; I did not diff it against `cargo tree` by script.

## 5. JavaScript and TypeScript

| Surface | Runtime npm packages | Notes | Verdict |
|---|---|---|---|
| coppice web, `harness/coppice/internal/web/static/` (7,250 lines of JS) | **0** | No `package.json`. No external URL in any `.js`, `.html`, `.css` or the manifest (grep). Tests in `_tests/` run on Node with a browser stub. | **Keep.** This is the model for the rest. |
| pi extension, `src/opendaisugi/harness_pi/extension/index.ts` | **0** | Imports `@earendil-works/pi-coding-agent` as `import type` only, plus `node:net`, `node:fs`, `node:path`. | **Keep.** |
| OpenCode plugin, `src/opendaisugi/harness_opencode/plugin/daisugi-gate.ts` | **0** | Imports `@opencode-ai/plugin` as `import type` only, plus Node built-ins. | **Keep.** |
| OpenClaw plugin, `src/opendaisugi/install_assets/openclaw_plugin/` | **0** declared | Imports `openclaw/plugin-sdk/plugin-entry`, which the host provides. | **Keep.** |
| `clients/ts` (conformance client) | **1**: `web-tree-sitter` 0.25.10 | Dev: `typescript`, `@types/node`, `undici-types`. Lock committed. The grammar is `vendor/tree-sitter-bash.wasm`, fetched by `vendor/fetch.sh`. | **Keep, align to 0.26** (rank 10). |
| `examples/integrations/openclaw` | `@modelcontextprotocol/sdk` ^1.0.0, no lock | A demo, not shipped. | **Keep as example.** Pin it if it stays. |

## 6. Native code

| Library | Built by | Linked into | License | Verdict |
|---|---|---|---|---|
| Z3 5.1.0 | `clients/go/scripts/native.sh` (sha256 of the GitHub release tarball); z3-src in Rust; the `z3-solver` wheel in Python | Go `daisugi`, `daisugi-gate`, `conform`; Rust binaries; Python | MIT | **Keep.** |
| tree-sitter runtime 0.26.0 | `native.sh` from the PyPI sdist (sha256 pinned) | Go gate | MIT; Unicode tables under the Unicode license | **Keep.** Rust and TS are on 0.25.10. |
| tree-sitter-bash 0.25.1 | `native.sh` from the PyPI sdist | Go gate | MIT | **Keep.** |
| SQLite 3.53.4 (Go) and 3.45.0 (Rust) | go-sqlite3 amalgamation; libsqlite3-sys amalgamation | Go and Rust `daisugi` | Public domain | **Keep. Align** (rank 10). Python uses the system `sqlite3` module. |
| LLVM libc++, libc++abi, libunwind (from zig 0.16.0) | `native.sh` copies zig's builds beside `libz3.a` | Go binaries (Z3 is C++) | Apache-2.0 WITH LLVM-exception (no binary notice needed) | **Keep.** Listed in `clients/go/NOTICE`. `harness/coppice/NOTICE` does not name it. Whether `libghostty-vt.a` embeds libc++ is not verified. |
| libghostty-vt (ghostty commit `b0c421fc...`) | `native.sh` or `harness/coppice/scripts/toolchain.sh`, zig ReleaseFast | `coppice` | MIT | **Keep.** Heaviest native build; needs zig 0.16.0 exactly. |
| simdutf 5.2.8 | inside ghostty's build | `coppice` | Apache-2.0 or MIT (MIT used) | **Keep.** |
| Highway | inside ghostty's build | `coppice` | Apache-2.0 or BSD-3-Clause (BSD used) | **Keep.** |
| Wuffs | inside ghostty's build | `coppice` | MIT and Apache-2.0 (MIT used) | **Keep.** |
| uucode | inside ghostty's build | `coppice` | MIT; Unicode License V3 tables | **Keep.** |
| Zig compiler_rt and std | zig 0.16.0 | `coppice` | MIT | **Keep.** |
| ring (C and assembly from BoringSSL) | ring's build script | Rust binaries | Apache-2.0 AND ISC | **Keep.** |

Build tool fetches: `toolchain.sh` downloads zig from ziglang.org (sha256 checked for
x86_64-linux only on this box; the other three platforms are taken from the published index)
and runs `uv tool install cmake` unpinned. It also writes two **global** Go settings with
`go env -w`: `GOTOOLCHAIN=auto` and `PKG_CONFIG`. `GOTOOLCHAIN=auto` makes every Go build on
the machine free to download a toolchain (`harness/coppice/PINS.md`, "Global Go
environment"). See Open item 5.

## 7. The ranked list: ten changes by value

| # | Change | What it buys | Cost | Risk |
|---|---|---|---|---|
| 1 | **Lock the Python tree.** Commit `uv.lock` (remove it from `.gitignore`), and make CI run `uv sync --locked --extra dev` in place of `uv pip install -e ".[dev]"` (`.github/workflows/ci.yml` line 38, `clients.yml` line 165). | Today CI resolves fresh on every run. litellm 1.82.7 and 1.82.8 carried a credential stealer; 1.82.8 ran it from a `.pth` file at every Python start ([litellm security update](https://docs.litellm.ai/blog/security-update-march-2026)). Under the current CI config, a run in such a window installs it, because `[dev]` pulls `[generate]`. "Make optional" does not stop a `.pth` payload; only a lock or a drop does. Also gives reproducible builds, as Go, Rust and TS already have. | Small: one `.gitignore` line, two CI lines, a lock refresh. | Low. Lock churn in diffs. |
| 2 | **Replace litellm and instructor with our own model call**, `opendaisugi/modelcall.py`: three wires (Anthropic Messages; OpenAI chat completions, which also covers Ollama's and llamafile's `/v1`; `claude -p`), stdlib `http.client`, pydantic validation, instructor-style re-ask. Port the design back from `clients/go/internal/llm/llm.go` and `clients/rust/src/llm/`. | Drops `[generate]`'s 52 new packages (openai, boto3, aiohttp, tiktoken, ...), the cost-map fetch, the need for commit 5450879f, the biggest supply-chain target in the tree, and about 2.4 s of import (`llm.py` comment). Full control of the request bytes. | **Medium to large.** (a) The Go and Rust clients cover only `anthropic/` and claude-code; they return `ErrUnsupported` for any other model (`llm.go` line 52). The Python users need the local wire: `tier1.py` (Ollama, llamafile), `voice/cleanup.py`, `llm_check.py`, `delegating_executor.py`. So Go and Rust need the OpenAI wire too, for parity. `gateway_openai.py` already has the usage and SSE shapes. (b) The ports copy instructor's log lines and litellm's banner byte for byte (`llm.go` header), and `clients/go/internal/llm/gen_schemas.py` imports instructor and litellm to write `schemas_gen.go` and to `--check` it. Freeze the current texts as fixtures, then change them in three languages at once. | Medium. Retry and error wording change; fixtures must be regenerated. |
| 3 | **Change the default matcher and drop `[search]`.** Set `config.py` line 125 to a torch-free backend; remove `opendaisugi[search]` from `[dev]`; drop the extra or leave it unlisted. | Removes torch, triton and seven NVIDIA wheels (about 5.3 GB of a 6.1 GB venv) from dev and CI, every proprietary license in the tree (14 NVIDIA packages in this venv, section 8), and the Hugging Face request on the default path. The Go and Rust ports can then run the default, which they cannot today (they have lexical and potion only). | Small in code. Tests that need `[search]` must move to int8 or be deleted. | Existing MiniLM pathways go stale. Under the no-backwards-compat rule this is a clean break; say so in the CHANGELOG. |
| 4 | **Own the embedders.** Port potion's loader and tokenizer back from `clients/go/internal/embed/potion` (revision and sha256 pinned, through `_model_fetch.py`); write a small WordPiece tokenizer for int8; replace `huggingface_hub` in `model_registry.py` with `_model_fetch.py`. | Drops model2vec (19 new), tokenizers (13 new), huggingface-hub and hf-xet. Closes the unpinned download. `[int8]` shrinks to onnxruntime plus 3. Python and the ports then share one loader design, which makes parity exact by construction. | Medium: two tokenizers, cross-checked against the libraries on the existing FPR corpus (`scripts/matcher_fpr.py`) before the libraries go. | Low to medium. A tokenizer edge case (Unicode, accents) shifts embeddings; the parity corpus catches it. |
| 5 | **Fix the undeclared imports.** Replace `click.Choice` in `cli.py` line 929 with typer's own choice support and remove `import click`; declare or remove `huggingface_hub` (see rank 4); declare torch and transformers where `vla_executor.py` is used. Add a CI job that installs the bare core and runs `daisugi --help`. | A bare `pip install opendaisugi` cannot start `daisugi` today (verified, section 2.4). | Tiny. | Low. |
| 6 | **Replace the `mcp` SDK** with a stdio JSON-RPC server of our own for the 11 tools in `mcp_server.py`. | Drops about 20 packages (starlette, sse-starlette, uvicorn, pyjwt, python-multipart, pydantic-settings, ...), and the 1.x to 2.x API churn that already forced the `<2` pin. | Small to medium: a few hundred lines plus tests. We must track MCP protocol revisions ourselves. | Medium. A host that needs a newer protocol feature breaks until we add it. |
| 7 | **Drop `textual-serve`** (`daisugi dashboard --serve`). Keep `textual` for the terminal. | Stops a call to Google Fonts on every browser view, and drops aiohttp and 14 more. coppice web is already the one browser surface, with zero external URLs. | Small: remove the flag and one import in `tui.py`. | Low. The browser dashboard goes away until coppice web shows the same view. |
| 8 | **Drop `av` from `[voice]`.** Require the `ffmpeg` binary for webm/opus, or have coppice web record 16 kHz mono PCM WAV in the browser so no decode is needed. | Removes 72 MB of bundled FFmpeg whose wheel carries GPL-2.0-or-later x264 and x265 (verified in `av.libs/`). We only decode audio; we need no video encoders. | Small. | Low. A box with no `ffmpeg` and a webm upload loses voice until the browser sends WAV. |
| 9 | **Replace networkx** with about 60 lines in `dag.py`, ported back from `clients/rust/src/dag.rs`. | Core drops to 5 direct packages. The oracle and the ports then share one algorithm for topological order, so order ties cannot drift. Possibly a faster import when a plan reaches `check_dag` (`verify.py` lines 957, 1048); not measured. | Small. | Low. `topological_generations` order must match exactly; the existing gate fixtures check it. |
| 10 | **Align versions across the ports.** tree-sitter 0.26 in Rust and TS (now 0.25.10; Python and Go are 0.26.0); bump rusqlite so Rust's bundled SQLite (3.45.0) matches Go's (3.53.4); drop Rust `thiserror` (its `thiserror-impl` is the only user of `syn` 2.0.119, checked with `cargo tree -i`, so the build loses one `syn`); delete the stale `clients/go/.gomodcache`. | Parity: the same grammar runtime and the same SQLite everywhere. SQLite 3.45.0 is two years older than the Go copy (known issues not checked). | Small to medium. rusqlite is pinned `=0.31.0` for a reason not written down in `Cargo.toml`; find it first. | Low to medium. |

Not ranked, but cheap: move `twine` out of `[dev]` to `uvx twine` at release time (22 packages);
set `HF_HUB_DISABLE_TELEMETRY=1` and `HF_HUB_DISABLE_IMPLICIT_TOKEN=1` in
`src/opendaisugi/__init__.py` beside the litellm setting, for as long as any HF library stays.

## 8. License problems

| Item | Where | Problem | Severity | Action |
|---|---|---|---|---|
| The CUDA wheels under torch | `[search]`, `[lora]`, and so every `[dev]` and CI install | The lock names six `nvidia-*` and three `cuda-*` packages; torch's own metadata pulls more, and this venv holds 17. Of those, 14 are LicenseRef-NVIDIA-Proprietary or NVIDIA-SOFTWARE-LICENSE (read from the installed metadata); `nvidia-nvtx` and `cuda-pathfinder` are Apache-2.0; `cuda-toolkit` states none. Legal to use under NVIDIA's terms. Not free software. The project does not redistribute it. | Medium, against the FLOSS value | Rank 3 removes them from everything but `[lora]`. |
| PyAV wheel | `[voice]` | The wheel ships only PyAV's BSD license text, but bundles x264 and x265 (GPL-2.0-or-later). Using it is legal. Shipping it inside any bundle we distribute would make that bundle GPL. | Medium if we ever bundle; low today | Rank 8. |
| litellm `enterprise/` | `[generate]` | The repo licenses `enterprise/` separately. The 1.100.0 wheel is MIT by metadata; `litellm/proxy/enterprise_billing/` has no header of its own. We never import the proxy. | Low | Rank 2 removes the question. |
| Herdr manifests | coppice | Apache-2.0 files inside an MIT product. Attributed in `harness/coppice/NOTICE`. | None | Keep the NOTICE current. |
| libc++ in coppice | coppice | Not named in `harness/coppice/NOTICE`. Its license asks no binary notice, so this is not a violation. | None | Add a line for completeness. |
| certifi, tqdm | many extras | MPL-2.0 (file-level copyleft). No obligation unless we modify their files. | None | None. |

I found nothing we cannot legally use.

## 9. Open items for the owner

1. **Commit `uv.lock`, or keep it ignored?** Options: (a) commit it and use `uv sync --locked`
   in CI; (b) keep it ignored and pin CI with a constraints file. Recommendation: (a). Go, Rust
   and TS already commit their locks.
2. **The new default `matcher_model`.** Options: `lexical` (no model, no download, ADR-0019),
   `potion` (8 MB pinned download, better recall, the owner's box already uses it), `int8`
   (MiniLM quality, 23 MB, needs AVX2 or aarch64, not in the ports). Recommendation: `potion`
   with the pinned loader of rank 4, falling back to `lexical` with a warning when the files are
   absent and the network is off.
3. **textual-serve, av, mcp: drop, or keep optional?** Recommendation: drop textual-serve and
   av; replace mcp (ranks 6 to 8).
4. **faster-whisper model pinning.** Options: pin a revision and sha256 and load from a local
   path, or leave it to faster-whisper. Recommendation: pin, through `_model_fetch.py`.
5. **The global `go env -w GOTOOLCHAIN=auto` in `toolchain.sh`.** Options: keep it; scope it to
   the build scripts with an environment variable; require Go 1.26 on the box. Recommendation:
   scope it, so the script leaves no global setting and no build downloads a toolchain unasked.
6. **Version skew across the ports** (rank 10): approve aligning Rust and TS to tree-sitter 0.26
   and Rust's SQLite to Go's.
7. **typer.** Keep it while Python is the oracle, because the Go CLI copies its behavior. Replace
   it only if the Python CLI stops being the oracle for `cli_compare.py`.

## 10. What was not checked

- No socket guard was run for instructor, mcp, textual, typer, faster-whisper or
  sentence-transformers. "None" in the network column means no fetch was found by reading or
  grep, not a proven absence. The Hugging Face requests are per its documentation.
- License rows for packages not installed here (sherpa-onnx, mujoco, peft, trl, bitsandbytes,
  datasets, accelerate) are not verified from metadata.
- Whether `libghostty-vt.a` embeds libc++ is not verified.
- Known vulnerabilities in SQLite 3.45.0 were not looked up.
- I did not check whether any venv or uv cache on this box ever held litellm 1.82.7 or 1.82.8.

## 11. Fit with the strategy notes

`docs/plans/2026-09-24-omarchy/strategy-2026-09-26.md` asks for a dogfood week and "one port
job running in the background" before any new design. Ranks 1, 3, 5, 7 and 8 are small and fit
beside the dogfood week. Rank 2 is a three-language change and should be the one background port
job, not a second one. The grafts note says "the local-first default is a local worker"; that
needs the OpenAI-compatible wire, which rank 2 adds to Go and Rust. I found no direct conflict.
