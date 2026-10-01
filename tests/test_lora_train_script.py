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
    args = parser.parse_args(
        [
            "--jsonl",
            "train.jsonl",
            "--base-model",
            "acme/any-model",
            "--output",
            "adapters/robin",
            "--qlora",
            "--lora-r",
            "8",
            "--epochs",
            "1",
            "--dry-run",
        ]
    )
    assert args.jsonl == "train.jsonl"
    assert args.base_model == "acme/any-model"
    assert args.output == "adapters/robin"
    assert args.qlora is True
    assert args.lora_r == 8
    assert args.epochs == 1
    assert args.dry_run is True


def test_lora_train_parser_defaults():
    from opendaisugi.lora.train import _build_parser

    parser = _build_parser()
    args = parser.parse_args(["--jsonl", "t.jsonl", "--output", "out"])
    assert args.base_model is None  # resolved by resolve_base_model
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


def test_the_base_model_is_the_garden_choice_else_the_hardware_default(tmp_path, monkeypatch):
    from opendaisugi import model_catalog as mc
    from opendaisugi.lora.train import _build_parser, resolve_base_model

    monkeypatch.setenv("OPENDAISUGI_VOICE_HARDWARE", "16,8,0")
    parser = _build_parser()
    base = ["--jsonl", "t.jsonl", "--output", "o", "--data-dir", str(tmp_path)]
    assert resolve_base_model(parser.parse_args(base)) == "ibm-granite/granite-4.1-3b"
    monkeypatch.setenv("OPENDAISUGI_VOICE_HARDWARE", "4,2,0")
    assert resolve_base_model(parser.parse_args(base)) == "ibm-granite/granite-4.0-1b"
    mc.record_choice(tmp_path, "acme/chosen")
    assert resolve_base_model(parser.parse_args(base)) == "acme/chosen"
    named = parser.parse_args(base + ["--base-model", "acme/named"])
    assert resolve_base_model(named) == "acme/named"


def test_the_sft_config_uses_the_names_the_pinned_trl_takes():
    # trl renamed SFTConfig's max_seq_length to max_length; the train
    # pack pins trl 1.14.1, which has only the new name.
    from opendaisugi.lora.train import _build_parser, sft_kwargs

    args = _build_parser().parse_args(
        ["--jsonl", "x", "--output", "out", "--max-seq-len", "64", "--batch-size", "4"]
    )
    kw = sft_kwargs(args, cuda=True)
    assert kw["max_length"] == 64 and "max_seq_length" not in kw
    assert kw["per_device_train_batch_size"] == 4 and kw["output_dir"] == "out"


def test_with_no_gpu_the_trainer_asks_for_the_cpu():
    # transformers refuses bf16 with no GPU unless use_cpu is set.
    from opendaisugi.lora.train import _build_parser, sft_kwargs

    args = _build_parser().parse_args(["--jsonl", "x", "--output", "out"])
    assert sft_kwargs(args, cuda=False)["use_cpu"] is True
    assert sft_kwargs(args, cuda=True)["use_cpu"] is False


def test_the_loss_is_the_model_s_own():
    # trl 1.14's default chunked loss reads MoE fields a Granite 4 hybrid
    # config does not have; the plain loss is the model's own forward.
    from opendaisugi.lora.train import _build_parser, sft_kwargs

    args = _build_parser().parse_args(["--jsonl", "x", "--output", "out"])
    assert sft_kwargs(args, cuda=False)["loss_type"] == "nll"


def test_the_router_loss_is_off_for_a_model_with_no_experts():
    # A dense Granite 4 (num_local_experts 0) keeps MoE fields in its
    # config; trl then asks it for router logits it does not have.
    from types import SimpleNamespace

    from opendaisugi.lora.train import _build_parser, has_experts, sft_kwargs

    assert not has_experts(SimpleNamespace(num_local_experts=0, output_router_logits=False))
    assert not has_experts(SimpleNamespace())
    assert has_experts(SimpleNamespace(num_local_experts=8))
    assert has_experts(SimpleNamespace(num_experts=4))
    args = _build_parser().parse_args(["--jsonl", "x", "--output", "out"])
    assert sft_kwargs(args, cuda=False, moe=False)["router_aux_loss_coef"] == 0.0
    assert "router_aux_loss_coef" not in sft_kwargs(args, cuda=False, moe=True)
