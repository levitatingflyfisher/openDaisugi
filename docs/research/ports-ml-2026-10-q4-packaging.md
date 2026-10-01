# Q4: Calling Python from the Go and Rust binaries, and packaging an "ML pack"

Date of research: 2026-10-02. Web sources only. Where a claim has no primary source, it says "could not verify" or "inference". Numbers from PyPI JSON and the pytorch.org index were fetched live on this date.

## 1. Calling Python from Rust and Go

### 1.1 Subprocess worker versus embedding

What primary sources show:

- Ollama's Go server runs inference in a child process. Its source says "All GGUF models are served via the upstream llama-server subprocess." [S1] The dev docs say native code is built under `build/lib/ollama` and GPU backends are separate libraries next to the binary. [S2] (Older Ollama builds used a CGO runner; the current main file no longer does. Secondary pages describe one runner process per model. I did not verify that from a primary source.)
- kluctl/go-embed-python is a Go library that ships a Python distribution. Its README says it "does not require CGO and solely relies on executing Python inside another process", with no Python needed on the host. [S3]
- Tauri documents the sidecar pattern: "Common use cases are Python CLI applications or API servers bundled using pyinstaller", via `bundle.externalBin`. [S4]
- Zed does not embed Python. Its docs say it uses a toolchain concept to find the project's venv (including `uv sync` envs) and starts language servers with that interpreter path. [S5]

Inference (not stated by one source): a subprocess keeps a Python crash, a CUDA fault or an out-of-memory kill away from the gate. The gate is a safety part, so this matters. PyO3 turns Python exceptions into Rust `Result` values, but a C-extension segfault in the same process kills the host. I could not find a PyO3 doc page that states process-level isolation either way. Could not verify.

### 1.2 PyO3 embedding

- `auto-initialize` "changes `Python::attach` to automatically initialize a Python interpreter (by calling `Python::initialize`) if needed". Without it you call `Python::initialize()` yourself. [S6]
- The GIL: the `Python<'py>` token proves the thread is attached. Free-threaded Python removes the GIL. PyO3 offers `pyo3::sync` helpers for both builds. [S7]
- Free-threaded: PyO3 0.23+ supports it (3.13 experimental, 3.14 official). It uses "a completely new ABI" with no limited-API equivalent yet, so `abi3` wheels cannot load on it. Threads must call `Python::attach()`. Long Rust work should detach to avoid GC stalls. [S8] Another PyO3 page says both builds can load the new `abi3t` ABI, which needs Python 3.15+. [S9]
- Linking: "By default PyO3 links to `libpython`." [S9] There are two embed modes. Dynamic links `libpython3.x.so`; the OS must find it at run time, and for non-technical users you must ship the shared library and set `LD_LIBRARY_PATH` or a wrapper. Static (`libpython.a`) means one binary, but PyO3 has no first-class support: you must export symbols (`-Wl,--export-dynamic`) for extension modules, and compiler and LTO flags must match. `auto-initialize` is deliberately off for static embedding. [S9]
- PyPy cannot be embedded. [S9]
- The same page points to PyOxidizer for shipping an embedding app. [S9] The repo shows `archived: false` but its last push was 2024-12-24 [S10]. Treat it as stale. Could not verify its maintenance status further.

### 1.3 Go: cgo and libpython

- DataDog/go-python3: archived, last push 2021-11-02. Its README says it "Currently supports python-3.7 only" and points to go-python/cpy3. [S11]
- go-python/cpy3: not archived, but last push 2022-08-16. [S11]
- sbinet/go-python: archived, "CPython2 C-API". [S11]
- go-python/gopy: active (push 2026-09-27). It generates a CPython extension from a Go package, so it is the reverse direction (Python calls Go). [S11]
- cgo needs a C toolchain and Python headers at build time. That breaks the "static Go binary" goal. (Inference from the go-python3 README, which requires pkg-config and `python3.7-dev`. [S11])

### 1.4 What real projects choose

Ollama, Tauri apps and kluctl all choose child processes. Embedding in PyO3 is common when the host is a Python extension. For a Rust binary that embeds Python, the docs warn of distribution work. [S1][S3][S4][S9] My read: subprocess wins for a static binary, a pinned runtime and fail-closed gating.

## 2. python-build-standalone (PBS)

- What: "self-contained, highly portable, high-performance Python distributions" with PGO, LTO and BOLT. [S12][S13]
- Owner: maintained by Astral. Primary recommended use is through uv's managed Python. [S13]
- License: MPL-2.0 for the build tooling, with extra license files for bundled parts (OpenSSL, SQLite, zlib and others). [S13] I confirmed the LICENSE file is MPL 2.0. CPython itself stays under the PSF license (not verified on a PBS page; general knowledge, so treat as unverified).
- Platforms: Linux, macOS, Windows. Python 3.10 to 3.15, standard and free-threaded builds. [S13]
- Archive types: `install_only`, `install_only_stripped`, and `full`. [S13][S14]
- Linux runtime needs: glibc 2.17 or newer on most targets (RISC-V 2.28). Musl builds exist; the fully static `+static` flavor "cannot load dynamically linked Python extension modules". [S15] That rules static musl out for a PyTorch pack, because torch is a native extension. (Inference from that quote.)
- Release 20261001 has these x86_64 Linux assets (sizes from the GitHub API): 3.12.15 gnu `install_only` 66.9 MB, `install_only_stripped` 34.3 MB; 3.13.16 gnu 75.3 / 35.1 MB; 3.14.8 gnu 75.0 / 36.3 MB; 3.12.15 musl 50.4 MB. [S14]
- Hashing: every release publishes a `SHA256SUMS` asset. For `cpython-3.12.15+20261001-x86_64-unknown-linux-gnu-install_only.tar.gz` it lists `0e56475e23f61a9c7d254fe16bc4931544026d591fc1cb8b770712ee94fa6260`. [S14] So a tool can pin tag, filename and hash.
- uv: uv "uses python-build-standalone distributions from Astral for CPython". [S16] uv's bundled download list has per-build entries with `url`, `sha256`, `os`, `arch`, `libc`, `variant` (`freethreaded`, `debug`), `build`. [S17] Whether uv enforces that sha256 on download: could not verify from docs (the field exists; enforcement is inference).
- uv can be pointed at your own list: `UV_PYTHON_DOWNLOADS_JSON_URL` takes "a local path or URL pointing to a JSON list" that overrides the hard-coded list. [S18] And `uv python install --mirror` replaces the base URL `https://github.com/astral-sh/python-build-standalone/releases/download`, and "Distributions can be read from a local directory by using the file:// URL scheme." [S19]

## 3. uv: pinned, locked, hashed, offline

All flag text is from the uv CLI reference page [S19] and the environment page [S18].

- Pinned Python: `uv python install 3.12.15` installs a specific patch. `--install-dir` / `UV_PYTHON_INSTALL_DIR` sets where it goes (default `~/.local/share/uv/python`). If you use `--install-dir`, "UV_PYTHON_INSTALL_DIR will need to be set for subsequent operations for uv to discover the Python installation." [S19]
- `UV_PYTHON_CACHE_DIR` holds the downloaded archives before install. Download control: the `python-downloads` setting (`automatic` or `manual`), `UV_PYTHON_DOWNLOADS` and `--no-python-downloads`. [S16][S18]
- `--managed-python` / `UV_MANAGED_PYTHON` requires uv's own Python and ignores system Python. [S19]
- Lock: `--locked` "Assert that the uv.lock will remain unchanged"; `--frozen` runs "without updating the uv.lock file". [S19] Per the sync doc, `--locked` fails if the lock is stale; `--frozen` skips the check. [S20] Sync is exact by default and removes packages not in the lock. [S20]
- Hash checking: for `uv pip install -r`, "By default, uv will verify any available hashes in the requirements file, but will not require that all requirements have an associated hash." `--require-hashes` (env `UV_REQUIRE_HASHES`) requires a hash on every requirement, with exact `==` pins or direct URLs. [S19]
- Export: `uv export --format requirements-txt` exists, and it has `--no-hashes` ("Omit hashes in the generated output"), which implies hashes are on by default. Also `--no-emit-project`. Formats: requirements.txt, pylock.toml, CycloneDX. [S19][S20]
- Offline: `--offline` / `UV_OFFLINE` "When disabled, uv will only use locally cached data and locally available files." [S19] `--find-links` / `UV_FIND_LINKS` adds local dirs of wheels. `--no-index` ignores the registry and uses only direct URLs and `--find-links`. `--no-build` refuses sdists; `--no-binary` refuses wheels. [S18][S19] The cache should sit on the same filesystem as the env for speed. [S21]
- Could not verify from docs: that `uv.lock` stores wheel hashes (very likely, since export emits them, but I did not read the lock-format page).

Recipe I would test (not run here):

1. Build time, online: `uv export --format requirements-txt --no-emit-project -o ml.txt` from a locked project, with the torch CPU index set in `[tool.uv.sources]`.
2. Build time: fetch every wheel into `wheels/` with `pip download --require-hashes -r ml.txt -d wheels/`. I found no uv download-only command in the CLI reference, so this step uses pip (could not verify an equivalent in uv).
3. Ship PBS tarball, `wheels/`, `ml.txt`, and a pinned `SHA256SUMS` line.
4. On the user's machine: verify the tarball hash, unpack to `$DATA/ml/python`, then `UV_OFFLINE=1 uv venv --python $DATA/ml/python/bin/python $DATA/ml/venv` and `uv pip install --require-hashes --no-index --find-links $DATA/ml/wheels -r ml.txt --python $DATA/ml/venv/bin/python`.

## 4. Wheel sizes, Linux x86_64 (live data, 2026-10-02)

Source: PyPI JSON for each project [S22]; download.pytorch.org index plus HTTP Content-Length [S23].

| Item | Size |
|---|---|
| torch 2.14.1 CPU wheel, cp312 (`download.pytorch.org/whl/cpu`) | 196.3 MB |
| torch 2.14.1 default PyPI wheel, cp312 manylinux_2_28 | 554.6 MB |
| torch 2.11.0 cu128 wheel, cp312 (newest cu128 listed) | 820.3 MB |
| nvidia-cudnn-cu13 9.24 | 553 MB |
| nvidia-cublas 13.8 | 439 MB |
| triton 3.8.0 cp312 | 248 MB |
| nvidia-cusolver 12.3 | 246 MB |
| nvidia-nccl-cu13 2.30 | 216 MB |
| nvidia-cusparselt-cu13 0.8 | 170 MB |
| nvidia-cusparse 12.8 | 170 MB |
| nvidia-cufft 12.4 | 162 MB |
| nvidia-nvshmem-cu13 3.4.5 | 60 MB |
| nvidia-cuda-nvrtc 13.4 | 53 MB |
| transformers 5.18.0 (py3-none-any) | 12.6 MB |
| peft 0.21.2 | 0.8 MB |
| trl 1.14.1 | 0.9 MB |
| bitsandbytes 0.50.2 | 43 MB |
| numpy 2.5.3 cp312 manylinux | 17 MB |

- The default PyPI torch 2.14.1 requires `cuda-toolkit[...]==13.0.3`, `nvidia-cudnn-cu13`, `nvidia-nccl-cu13`, `nvidia-cusparselt-cu13`, `nvidia-nvshmem-cu13` and `triton` on Linux. [S22] The sum of just the rows above (torch + listed NVIDIA wheels + triton) is about 2.9 GB, and the cuda-toolkit extras add more. So a default install is roughly 3 GB or more of wheels; a CPU-only install is about 200 MB for torch.
- The CPU wheel list on pytorch.org shows 2.13, 2.14.0 and 2.14.1 `+cpu` for cp312 x86_64. [S23]
- The peft, trl, transformers wheels are tiny. Their cost is the dependencies: transformers needs huggingface-hub, tokenizers, safetensors, numpy, regex, pyyaml, tqdm, typer; trl needs accelerate, datasets, jinja2; peft needs torch, accelerate, psutil. [S22] I did not measure sizes of tokenizers, safetensors, accelerate, datasets or pyarrow, and did not measure unpacked (on-disk) size. Could not verify.
- Python 3.14 free-threaded (`cp314t`) torch wheels exist on PyPI at the same listed size. [S22]
- Rough pack total, CPU-only: PBS 35 to 67 MB + torch 196 MB + ~100 MB others, so about 350 to 450 MB packed (estimate, not measured).

## 5. Arch and AUR packaging

- optdepends: PKGBUILD(5) says they are "not essential for base functionality, but may be necessary to make full use of the contents", "for informational purposes only and are not utilized by pacman during dependency resolution". Format `optdepends=('python: for library bindings')`; per-arch forms exist. [S24] The package guideline says optional dependencies go in `optdepends`, and that info is printed on install, so keep it out of `.install` files. [S25]
- Split packages: `pkgbase` plus a `pkgname` array and a `package_NAME()` function each. Per-package overrides allowed: pkgdesc, arch, url, license, depends, optdepends, provides, conflicts, replaces, backup, options, install, changelog. Makepkg does not check split-package depends before building; the global arrays must cover everything needed to build. [S24]
- Real examples, from the Arch package repo:
  - ollama: `pkgbase=ollama`, `pkgname=(ollama ollama-rocm ollama-cuda ollama-vulkan ollama-docs)`; the GPU packages `depends+=(ollama cuda)` etc. [S26]
  - python-pytorch: one pkgbase with `python-pytorch`, `-opt`, `-cuda`, `-opt-cuda`, `-rocm`, `-opt-rocm`, `-xpu`, `-opt-xpu`. [S27]
  - AUR python-peft: `arch=('any')`, plain `depends` on `python-pytorch` and `python-transformers`, built with `python -m build`. [S28]
  So the Arch way to ship an "ML runtime" is a separate package (or split package) that depends on system `python-pytorch`, not a vendored env.
- Network in build(): the wiki says makepkg "Downloads source files from servers" first, then checks integrity, unpacks, builds. [S29] Sources must be in `source=()` with checksums. [S30] Rust guideline: run `cargo fetch --locked` in `prepare()` so "later build() and other stages to be run entirely offline", then `cargo build --frozen`. [S31] Go guideline: set GOPATH to srcdir and `go mod download` in `prepare()`. [S32] Non-free guideline: downloading inside build is "not recommended" because "the user may have no Internet access". [S33] I did not find a sentence that says "build() must never touch the network" as a hard rule. It is a strong convention backed by the above. Could not verify a stricter written rule.
- Vendored Python envs: the Python guideline says installing is done into `$pkgdir/usr/lib/python<ver>/site-packages`, the standards path is build a wheel with `python-build` and install with `python-installer`, and putting a prebuilt `.whl` in `source` is "discouraged in favor of building from source" unless only wheels exist. [S34] I found no text that bans a bundled venv in so many words. Could not verify. A venv built in package() would also fail the guideline path above, and a package that pip-installs at install time breaks pacman file tracking (inference).
- AUR: it holds PKGBUILDs, not binary packages. [S35]

## 6. Projects with a native binary plus on-demand runtime

- Ollama (Go): one server binary; GPU backends (CUDA, ROCm, Vulkan) ship as separate libraries in `lib/ollama`; Linux install is tarballs, with a separate `ollama-linux-amd64-rocm.tar.zst` for AMD; NVIDIA drivers are installed separately. Inference runs in a `llama-server` subprocess. Models are pulled on demand (models in `~/.ollama/models/blobs/` per a secondary source). [S1][S2][S36]
- Zed (Rust): detects the user's venv and starts tools with it. [S5] Its node runtime code has `allow_binary_download` and `Pin` or `Latest` version strategies, so Zed can download and pin Node. [S37] Extensions get `download_file`, `npm_install_package`, `node_binary_path` and `latest_github_release` helpers to fetch their own language servers. [S38] I could not confirm Zed's download URL or hash handling.
- kluctl go-embed-python (Go): embeds a Python distribution, runs it as a child process, no CGO. [S3]
- Tauri apps: sidecar binaries (PyInstaller-frozen Python) listed in `externalBin`. Known issue: NSIS installer may not replace a sidecar on reinstall. [S4]
- uv itself: downloads PBS builds on demand into a managed dir. [S16]

## Recommendation: `daisugi pack install ml`

Design (my judgment, built on the sources above):

1. **Process boundary, not embedding.** The Go and Rust binaries start a Python worker child process. Reason: static binaries, no cgo, no libpython, crash isolation for the gate, and it matches Ollama, kluctl and Tauri. [S1][S3][S4] Wire format: length-prefixed frames (4-byte length, then JSON or msgpack) over stdin and stdout; stderr for logs only; a version frame first; a hard timeout and kill; restart on exit. Both binaries implement the same client, so the parity tests can compare them.
2. **Pack layout** under `$XDG_DATA_HOME/daisugi/packs/ml/<pack-version>/`: `python/` (unpacked PBS), `venv/`, `wheels/` (cache), `manifest.json`, `lock/ml.txt`.
3. **Pin everything in a manifest shipped inside the binary or signed release:** PBS release tag plus filename plus sha256 (from `SHA256SUMS` [S14]); the hashed requirements file made by `uv export` [S19]; the torch index URL. Default to the CPU torch wheel (196 MB) and make CUDA an explicit second pack (`pack install ml-cuda`), because the default wheels pull about 3 GB. [S22][S23]
4. **Install steps:** download PBS tarball, check sha256, unpack; create venv with that interpreter; `uv pip install --require-hashes --no-index --find-links wheels -r ml.txt` when offline, or the same with `--index-url` and without `--no-index` when online. Set `UV_OFFLINE=1` for the offline path. [S19] Point uv at the unpacked interpreter, so `UV_PYTHON_INSTALL_DIR` is not needed. If you prefer uv to fetch Python, use `UV_PYTHON_INSTALL_DIR` plus `--mirror file://...` or `UV_PYTHON_DOWNLOADS_JSON_URL` to a local JSON. [S18][S19]
5. **Offline bundle:** a `daisugi-ml-pack-<ver>-linux-x86_64.tar.zst` containing the PBS tarball, wheels dir and lock. `daisugi pack install ml --from <file>` makes zero network calls. Free-threaded Python (`3.14t`) is optional and not worth it now: abi3 wheels cannot load on it and many wheels lag. [S8]
6. **Verification:** after install, run the worker's self-test frame, check `torch.__version__`, and record installed hashes in `manifest.json`. Never auto-update; `pack update` is explicit.
7. **Pin Python to 3.12 or 3.13** (gnu build, glibc 2.17 floor [S15]). Do not use static musl PBS [S15].
8. **Arch/AUR:** ship `daisugi` as a split pkgbase: `daisugi` (static binaries), `daisugi-ml` (depends on `python`, `python-pytorch`, `python-transformers`, `python-peft`, plus the worker `.py` files). The base package lists `daisugi-ml` in `optdepends`. [S24][S26][S27][S28] Do not build or fill a venv in `package()`, and do not download in `build()`; keep `daisugi pack install ml` as the path for non-Arch users, and have it detect a system `daisugi-ml` first. [S31][S33][S34]

Open items I could not verify: uv's enforcement of PBS sha256, `uv.lock` wheel-hash storage docs, Zed's exact download mechanics, a written Arch ban on vendored venvs, and unpacked sizes of the ML stack.

## Sources

- S1 https://raw.githubusercontent.com/ollama/ollama/main/llm/server.go
- S2 https://raw.githubusercontent.com/ollama/ollama/main/docs/development.md
- S3 https://raw.githubusercontent.com/kluctl/go-embed-python/main/README.md
- S4 https://v2.tauri.app/develop/sidecar/
- S5 https://zed.dev/docs/languages/python
- S6 https://pyo3.rs/main/features.html
- S7 https://pyo3.rs/main/python-from-rust.html
- S8 https://pyo3.rs/main/free-threading.html
- S9 https://raw.githubusercontent.com/PyO3/pyo3/main/guide/src/building-and-distribution.md
- S10 https://api.github.com/repos/indygreg/PyOxidizer
- S11 https://api.github.com/repos/DataDog/go-python3 ; https://api.github.com/repos/go-python/cpy3 ; https://api.github.com/repos/sbinet/go-python ; https://api.github.com/repos/go-python/gopy ; https://raw.githubusercontent.com/DataDog/go-python3/master/README.md
- S12 https://gregoryszorc.com/docs/python-build-standalone/main/
- S13 https://github.com/astral-sh/python-build-standalone ; https://raw.githubusercontent.com/astral-sh/python-build-standalone/main/LICENSE
- S14 https://api.github.com/repos/astral-sh/python-build-standalone/releases/latest ; https://github.com/astral-sh/python-build-standalone/releases/download/20261001/SHA256SUMS
- S15 https://gregoryszorc.com/docs/python-build-standalone/main/running.html
- S16 https://docs.astral.sh/uv/concepts/python-versions/
- S17 https://raw.githubusercontent.com/astral-sh/uv/main/crates/uv-python/download-metadata.json
- S18 https://docs.astral.sh/uv/reference/environment/
- S19 https://docs.astral.sh/uv/reference/cli/
- S20 https://docs.astral.sh/uv/concepts/projects/sync/ ; https://docs.astral.sh/uv/concepts/projects/export/
- S21 https://docs.astral.sh/uv/concepts/cache/
- S22 https://pypi.org/pypi/torch/json (and /peft, /transformers, /trl, /bitsandbytes, /numpy, /triton, /nvidia-* JSON)
- S23 https://download.pytorch.org/whl/cpu/torch/ ; https://download.pytorch.org/whl/cu128/torch/
- S24 https://man.archlinux.org/man/PKGBUILD.5.en.raw
- S25 https://wiki.archlinux.org/title/Arch_package_guidelines
- S26 https://gitlab.archlinux.org/archlinux/packaging/packages/ollama/-/raw/main/PKGBUILD
- S27 https://gitlab.archlinux.org/archlinux/packaging/packages/python-pytorch/-/raw/main/PKGBUILD
- S28 https://aur.archlinux.org/cgit/aur.git/plain/PKGBUILD?h=python-peft
- S29 https://wiki.archlinux.org/title/Arch_package_guidelines (Makepkg duties)
- S30 https://wiki.archlinux.org/title/PKGBUILD
- S31 https://wiki.archlinux.org/title/Developer-manual:package-guidelines/rust
- S32 https://wiki.archlinux.org/title/Developer-manual:package-guidelines/go
- S33 https://wiki.archlinux.org/title/Developer-manual:package-guidelines/non-free
- S34 https://wiki.archlinux.org/title/Developer-manual:package-guidelines/python
- S35 https://wiki.archlinux.org/title/AUR_submission_guidelines
- S36 https://docs.ollama.com/linux
- S37 https://raw.githubusercontent.com/zed-industries/zed/main/crates/node_runtime/src/node_runtime.rs
- S38 https://docs.rs/zed_extension_api/latest/zed_extension_api/

Note: the Arch wiki blocks normal page fetches (bot wall), so wiki text was read through its `action=raw` view and the man.archlinux.org raw page. The uv CLI page was parsed from HTML locally.
