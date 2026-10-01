# Train a LoRA adapter from the Go or Rust daisugi

The `daisugi` binaries are static and carry no Python. Two jobs stay in
Python's ML stack: LoRA training and the SmolVLA reference policy. An ML
pack gives the binaries a Python for them. A pack is a pinned CPython, a
virtual environment made with it, and the wheels of a hashed lock, under
your data directory. Nothing on your PATH is used, and nothing is
installed outside the pack.

## Install the train pack

```bash
daisugi pack list
daisugi pack install train
daisugi pack status
```

`install` fetches the pinned CPython (34 MB) and checks its sha256, makes a
virtual environment with it, and installs the 57 wheels of
`packs/train.lock` with `pip --require-hashes --only-binary=:all:`: CPU
PyTorch, transformers, peft, trl, datasets and their dependencies, about
350 MB to download and 1.5 GB on disk. It ends with a self-test that
imports each package and prints its version. A file whose hash does not
match its pin stops the install, names the file, and leaves no pack
behind.

The pack lives in `~/.opendaisugi/packs/train/` (`--data-dir` moves it).
`daisugi pack remove train` deletes that directory and nothing else.

## Train

```bash
daisugi lora export train.jsonl        # the Python daisugi only (uv run daisugi ...)
daisugi lora train --jsonl train.jsonl --output adapters/mine
```

`lora train` takes the trainer's own options (`python -m
opendaisugi.lora.train --help`). The base model is `--base-model`, else
the one `daisugi models use` recorded, else the default for this box. The
trainer's lines stream on stderr; the last line says where the adapter is.

On a CPU, training is slow. One step on 4 examples with
`ibm-granite/granite-4.0-1b` took about 14 minutes on a 4-core i5 from
2017, and needed about 4 GB of memory. A GPU pack is listed
(`train-cuda`) but not built in this release.

## Install with no network

On a machine with network:

```bash
daisugi pack bundle train train-pack.tar
```

Copy the file, then:

```bash
daisugi pack install train --offline train-pack.tar
```

A directory works too (give `bundle` a path that does not end in `.tar`).
The offline install checks the CPython tarball and every wheel against the
pins in the binary, never against anything in the bundle.

## Run a job yourself

```bash
daisugi pack run train selftest
daisugi pack run vla-ref vla-chunk --input case.json
```

Options for `pack run` go before the pack name; every word after the job
name goes to the job. A job's progress goes to stderr and its result to
stdout as JSON. If the worker fails or dies, you get one line and exit 1.

## On Arch Linux

The AUR package `daisugi-ml` (split from `opendaisugi`) provides the train
pack with the system's Python and the Arch PyTorch and Hugging Face
packages. It installs no virtual environment. `daisugi pack list` shows it
as `system`; a pack you install yourself comes first.

## What is where

- The catalog and locks: `packs/` in the repository.
- The worker and its protocol: `src/opendaisugi/pack/worker.py`.
- The rulings: PK-R-1 to PK-R-11 in `clients/ADJUDICATIONS.md`.
