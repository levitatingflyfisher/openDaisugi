"""Tests for the LoRA trainer script (v0.10.0).

These tests never trigger actual training — they only verify the
argparser and that the module is importable on machines without
torch/peft/trl.
"""

from __future__ import annotations

import importlib
import sys


def test_lora_train_module_imports_without_heavy_deps():
    # Even if the user doesn't have torch/peft installed, the module
    # itself should import — the lazy imports live inside _train.
    mod = importlib.import_module("opendaisugi.lora.train")
    assert callable(mod.main)
    assert callable(mod._build_parser)


def test_lora_train_parser_requires_jsonl_and_output():
    from opendaisugi.lora.train import _build_parser
    parser = _build_parser()
    # Missing required args: should SystemExit.
    import pytest
    with pytest.raises(SystemExit):
        parser.parse_args([])


def test_lora_train_parser_accepts_full_invocation():
    from opendaisugi.lora.train import _build_parser
    parser = _build_parser()
    args = parser.parse_args([
        "--jsonl", "train.jsonl",
        "--base-model", "Qwen/Qwen2.5-1.5B-Instruct",
        "--output", "adapters/robin",
        "--qlora",
        "--lora-r", "8",
        "--epochs", "1",
        "--dry-run",
    ])
    assert args.jsonl == "train.jsonl"
    assert args.base_model == "Qwen/Qwen2.5-1.5B-Instruct"
    assert args.output == "adapters/robin"
    assert args.qlora is True
    assert args.lora_r == 8
    assert args.epochs == 1
    assert args.dry_run is True


def test_lora_train_parser_defaults():
    from opendaisugi.lora.train import _build_parser
    parser = _build_parser()
    args = parser.parse_args(["--jsonl", "t.jsonl", "--output", "out"])
    assert args.format == "alpaca"
    assert args.qlora is False
    assert args.lora_r == 16
    assert args.lora_alpha == 32
    assert args.epochs == 3
    assert args.batch_size == 4
    assert args.grad_accum == 4
    assert args.max_seq_len == 2048


def test_lora_train_module_does_not_import_torch_at_load_time():
    """Importing the trainer module must not pull in torch.

    Runs in a subprocess so sys.modules is clean — otherwise an earlier
    test in the session could have already imported torch and falsely
    satisfy the check.
    """
    import subprocess

    result = subprocess.run(
        [
            sys.executable,
            "-c",
            "import sys; import opendaisugi.lora.train; "
            "assert 'torch' not in sys.modules, sorted(k for k in sys.modules if 'torch' in k)",
        ],
        capture_output=True,
        text=True,
    )
    assert result.returncode == 0, result.stderr
