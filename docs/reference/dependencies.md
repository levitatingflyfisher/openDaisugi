# Dependencies

Status: reference, current as of 2026-10-09. This page lists every third-party
package, module, crate and native library openDaisugi uses, with its licence, why we
need it, and its removal status. It replaces the research sweep of 2026-09-26
(`docs/research/dependency-sweep-2026-09-26.md`, now a pointer here). The full
reasoning of that sweep is in this file's git history.

The rule each row answers to: use every import we need for exactness and
collaboration, and none we do not; use nothing we cannot legally use; stay
local-first. Our own code is MIT.

Status words:

- **keep**: needed; no plan to remove it.
- **optional**: only in an extra, or only in a build tool; nothing installs it by
  default.
- **open**: a replacement is wanted and not yet done.
- **removed**: gone from the dependency list, with the date.

## Python

Licences come from the installed wheel metadata. `uv.lock` is tracked and holds the
exact versions.

### Core (`pip install opendaisugi`)

| Package | Licence | Why we need it | Status |
|---|---|---|---|
| pydantic | MIT | Every model (`Envelope`, `ActionPlan`, pathways, config). The ports copy its validation messages. | keep |
| z3-solver | MIT | The verifier's SMT back end. | keep |
| pyyaml | MIT | Config, journal and install files (`safe_load`, `safe_dump`). | keep |
| typer | MIT | The `daisugi` CLI. The Go and Rust CLIs copy its parse and help output, so it stays while Python is the oracle. | keep |
| click | BSD-3-Clause | `cli.py` uses `click.Choice`; typer no longer depends on it. | keep (small) |
| numpy | BSD-3-Clause and others (0BSD, MIT, Zlib, CC0-1.0) | Similarity, clustering, the lexical matcher, audio resampling. | keep |
| networkx | BSD-3-Clause | Was four graph calls in `dag.py`. Our own walk now gives networkx's answers (tested against it on 2000 random plans). | removed 2026-10-09 |

`rich` arrives only through typer; `src/opendaisugi` never imports it.

### Extras

| Extra: package | Licence | Why we need it | Status |
|---|---|---|---|
| `[generate]` httpx | BSD-3-Clause | Our own model client (`llm_client.py`): the Anthropic Messages wire and OpenAI-compatible chat. | optional |
| `[gateway]` httpx, uvicorn | BSD-3-Clause | The Python token-saving gateway (the oracle; Go and Rust have their own). | optional |
| `[search]` sentence-transformers | Apache-2.0 | The MiniLM matcher through torch. Not the default matcher (`lexical` is). torch pulls NVIDIA's proprietary CUDA wheels: see Licences. | optional; open (the int8 matcher runs the same model with no torch) |
| `[potion]` model2vec | MIT | The potion matcher. The Go and Rust ports load potion with their own code. | optional; open (port the loader back) |
| `[potion]` huggingface_hub | Apache-2.0 | `snapshot_download` of the potion model at a pinned revision. | optional (declared 2026-10-09) |
| `[int8]` onnxruntime | MIT | Runs MiniLM int8 with no torch (ADR-0021). Model files come through `_model_fetch.py`, sha256-pinned. | optional |
| `[int8]` tokenizers | Apache-2.0 | Reads MiniLM's `tokenizer.json`. | optional; open (a small WordPiece of our own) |
| `[voice]` faster-whisper | MIT | Speech to text on CTranslate2, no torch. Models load at pinned revisions (`voice/pins.py`). It depends on `av`: see Licences. | optional |
| `[voice]` sounddevice | MIT | Microphone capture. | optional |
| `[voice]` huggingface_hub | Apache-2.0 | `voice/engines.py` reads its cache-miss error. | optional (declared 2026-10-09) |
| `[voice]` av | BSD-3-Clause, wheel bundles GPL-2.0-or-later x264/x265 | Was our webm/opus fallback when `ffmpeg` is absent. Our code now uses the `ffmpeg` binary only and never imports av. faster-whisper still installs it. | removed from our list 2026-10-09 |
| `[hub]` huggingface_hub | Apache-2.0 | `daisugi models pin`: resolves and fetches a file from the Hub, pinned to its commit (`model_registry.py`). | optional (new 2026-10-09) |
| `[mcp]` mcp | MIT | The Python MCP server (FastMCP). It is the oracle the Go and Rust servers copy byte for byte (ruling K3-1). About 20 transitive packages. | optional; open |
| `[shell]` tree-sitter, tree-sitter-bash | MIT, MIT | Bash grammar parsing for compound-shell decomposition (ADR-0010). | optional |
| `[tui]` textual | MIT | `daisugi dashboard --tui` and the other terminal views. | optional |
| `[tui]` textual-serve | MIT | `daisugi dashboard --serve` in a browser. Its own page loaded Google Fonts; we now hand it our page (`tui_web/app_index.html`), which loads the font from its local static folder, so a view makes no request off the machine. | optional |
| `[sign]` cryptography | Apache-2.0 OR BSD-3-Clause | Ed25519 signing of skill contracts. Never write our own. | optional |
| `[robotics]` mujoco | Apache-2.0 | The MuJoCo executor. | optional |
| `[vla]` torch, transformers | BSD-style and others; Apache-2.0 | The Python SmolVLA executor (`vla_executor.py`). The Go and Rust executors run the ONNX export and need neither. | optional (new 2026-10-09) |
| `[lora]` torch, transformers, peft, trl, bitsandbytes, datasets, accelerate | Apache-2.0 and mixed | LoRA fine-tuning. Never in `[dev]`. | optional |

### Dev and test (`[dev]`)

| Package | Licence | Why | Status |
|---|---|---|---|
| pytest, pytest-cov, pytest-asyncio | MIT, MIT, Apache-2.0 | The suite. | keep |
| ruff (one exact version) | MIT | Lint and format (ADR-0009). | keep |
| build | MIT | sdist and wheel. | keep |
| twine | Apache-2.0 | PyPI upload. No script used it; it pulled 22 packages into every dev venv. Run `uvx twine` at release time. | removed 2026-10-09 |
| litellm, instructor | MIT | Were the model client. Our own `llm_client.py` replaced them. | removed (before this page) |

`[dev]` pulls in `[mcp]`, `[shell]`, `[sign]`, `[gateway]`, `[tui]`, `[voice]`,
`[potion]`, `[int8]` and `[generate]` so their tests run. It never pulls `[search]`,
`[lora]` or `[vla]`.

## Go

| Module | Licence | Used by, and why | Status |
|---|---|---|---|
| github.com/mattn/go-sqlite3 | MIT (SQLite public domain) | `clients/go`: pathway store and trace journal. | keep |
| golang.org/x/text | BSD-3-Clause | `clients/go`: NFKC normalization, as Python's `urllib.parse` does. | keep |
| go.mitchellh.com/libghostty | MIT | coppice: the VT engine, confined to `internal/vt` by a boundary test. | keep |
| github.com/creack/pty | MIT | coppice: one PTY per pane. | keep |
| github.com/coder/websocket | ISC | coppice web: the phone client transport. | keep |
| github.com/BurntSushi/toml | MIT | coppice: config and detection manifests. | keep |
| github.com/mdp/qrterminal/v3, rsc.io/qr | MIT, BSD-3-Clause | coppice: sign-in QR codes in the terminal. | keep; open (a small encoder of our own) |
| golang.org/x/term, golang.org/x/sys | BSD-3-Clause | coppice: raw mode for attach and the TUI. | keep |

`harness/sprig` has no third-party module. The Go client carries its own ports of
what Python takes from libraries: the model client, the potion embedder, YAML and
JSON output, Python regex semantics.

## Rust

| Crate set | Licences | Status |
|---|---|---|
| `clients/rust`: serde, serde_json, tree-sitter, tree-sitter-bash, regex, libc, jiter (pydantic-core's JSON parser), z3-sys (Z3 5.1.0, vendored), rustls, rustls-native-certs, rusqlite (bundled SQLite), num-bigint, num-traits, miniz_oxide, ring; build: cc (mujoco feature); dev: rcgen | MIT or Apache-2.0 duals, MIT, ISC, BSD-3-Clause, Unicode-3.0; ring is Apache-2.0 AND ISC; `r-efi` offers MIT among its choices (UEFI only, never linked here) | keep |
| `clients/rust`: thiserror | MIT OR Apache-2.0 | keep; open (error derives only; it pulls a second `syn`) |
| `harness/coppice-rs`: serde, serde_json, libc, toml, regex, rustls, rustls-pki-types, ring (all pinned) | as above | keep |
| `harness/sprig-rs`: rustls, rustls-pki-types, ring | as above | keep |

`clients/rust/NOTICE` lists the linked crates and their licences.

## JavaScript

| Surface | Packages | Licence | Status |
|---|---|---|---|
| coppice web static files | none | | keep |
| pi extension, OpenCode plugin, OpenClaw plugin | none (types only, host-provided SDKs) | | keep |
| `clients/ts` | web-tree-sitter (runtime); typescript, @types/node (dev); lock committed | MIT, Apache-2.0, MIT | keep |
| `examples/integrations/openclaw` | @modelcontextprotocol/sdk, pinned to 1.30.1 | MIT | keep (an example, not shipped) |
| `harness/coppice/e2e` | playwright (tests only) | Apache-2.0 | keep |

The tree-sitter-bash grammar wasm for `clients/ts` is fetched by
`clients/ts/vendor/fetch.sh` and checked by sha256 before it is put in place.

## Native libraries and build tools

| Item | Licence | How it is fetched | Status |
|---|---|---|---|
| Z3 5.1.0 | MIT | `clients/go/scripts/native.sh`, sha256 of the release tarball; z3-src in Rust; the wheel in Python | keep |
| tree-sitter runtime, tree-sitter-bash | MIT | `native.sh`, sha256-pinned sdists | keep |
| SQLite | public domain | amalgamations inside go-sqlite3 and rusqlite | keep |
| LLVM libc++, libc++abi, libunwind (from zig 0.16.0) | Apache-2.0 WITH LLVM-exception | `native.sh` | keep |
| libghostty-vt (with simdutf, Highway, Wuffs, uucode) | MIT; the bundled parts under MIT or BSD choices | `native.sh`, or `harness/coppice/scripts/toolchain.sh` at a pinned commit | keep |
| MuJoCo 3.12.0, ONNX Runtime 1.23.2 | Apache-2.0, MIT | `native.sh --mujoco`, `--onnxruntime`, sha256-pinned; never linked into the shipped binaries | optional |
| zig 0.16.0 | MIT | `toolchain.sh`, sha256 per platform | build tool |
| cmake 4.4.3 | BSD-3-Clause | `toolchain.sh`, `uv tool install cmake==4.4.3` when no cmake is on PATH | build tool |
| ring's C and assembly | Apache-2.0 AND ISC | ring's build script | keep |

`toolchain.sh` builds under `${XDG_DATA_HOME:-~/.local/share}/opendaisugi` and sets no
global Go setting (changed 2026-10-09).

## Models

| Model | Licence | Fetched by | Pin |
|---|---|---|---|
| all-MiniLM-L6-v2 (int8 ONNX) | Apache-2.0 | `_search.py` through `_model_fetch.py` | revision and sha256 per file |
| potion-base-8M | MIT | Python `snapshot_download`; Go and Rust own loaders | revision (and sha256 in Go and Rust) |
| faster-whisper models | MIT | `voice/engines.py` | revision per model |
| Moonshine | MIT | Go and Rust voice | per NOTICE |
| Parakeet-TDT-0.6B v2 GGUF | CC-BY-4.0 | voice bridge | revision; attribution in NOTICE |
| SmolVLA base, SmolVLM2-500M tokenizer | Apache-2.0 | `clients/vla_export.py` | `packs/vla-ref.lock` |
| Catalog entries (`model_catalog.json`) | apache-2.0 for Granite, Ministral and Gemma; "Llama 3.2 Community License" for the two Llama entries | `daisugi models` | id only. Both machine-picked defaults are apache-2.0, held by a test in each language |

## Licences to know

- **NVIDIA CUDA wheels.** torch, in `[search]`, `[lora]` and `[vla]`, pulls
  `nvidia-*` and `cuda-*` wheels under NVIDIA's proprietary licence. Each user may
  install them. No bundle or default install includes them, and `[dev]` does not
  install those extras. NOTICE says so.
- **The av wheel.** faster-whisper depends on av, whose wheel bundles x264 and x265
  under GPL-2.0-or-later. Using it is legal. A bundle we distribute must not ship it.
  Our own code never imports av.
- **certifi, tqdm.** MPL-2.0, file-level copyleft; no duty unless we change their files.
- **Llama 3.2.** The catalog shows its licence by name. It is never the default.

Nothing in the list is a licence we cannot use.

## Open

1. Replace the `mcp` SDK in the Python server with a stdio JSON-RPC server of our
   own, ported back from `clients/go/internal/mcpwire`. It is the oracle of the Go
   and Rust MCP servers (K3-1), so the bar is: the 260 recorded MCP sessions in
   `clients/fixtures/k3` re-record with no change except `serverInfo.version`, and
   `gen_tools.py --check` still passes. The sessions also cover what the ports
   refuse (resources/read, completion/complete, the tasks/* methods,
   resources/subscribe), so the Python server must answer those as the SDK does.
2. Port the potion loader and a WordPiece tokenizer back to Python, then drop
   model2vec, tokenizers and `huggingface_hub` from `[potion]` and `[int8]`.
3. Drop `[search]` once nothing needs torch for the default path.
4. Rust `thiserror`, coppice `qrterminal`: small own code would replace each.
5. Version skew: tree-sitter 0.25 in Rust and TypeScript against 0.26 in Python and
   Go; SQLite in Rust older than in Go.
