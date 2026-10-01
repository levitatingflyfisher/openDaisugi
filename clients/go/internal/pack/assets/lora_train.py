"""QLoRA trainer for openDaisugi envelope generation (v0.10.0).

Runs on a GPU box (≥16 GB VRAM recommended; RTX 4080 is sufficient for
QLoRA on a 7B base model). Consumes a JSONL emitted by
``opendaisugi.lora.dataset.emit_jsonl`` and produces a LoRA adapter
suitable for merge or inference-only use.

Heavy dependencies (``torch``, ``peft``, ``trl``, ``transformers``,
``bitsandbytes``) are **lazy-imported** inside ``_train`` so the module
can be imported on machines without a GPU — only the argparser and the
top-level ``main`` are available until the training code path runs.

Typical invocation::

    python -m opendaisugi.lora.train \\
        --jsonl train.jsonl \\
        --base-model ibm-granite/granite-4.1-3b \\
        --output adapters/robin \\
        --qlora

Designed to be portable: no openDaisugi-specific training logic lives
here beyond format choice (alpaca/chat). The output is a standard
Hugging Face PEFT adapter directory.
"""

from __future__ import annotations

import argparse
from typing import Sequence


def _build_parser() -> argparse.ArgumentParser:
    """Build the CLI argument parser.

    Factored out so tests can exercise argparse without importing torch.
    """
    parser = argparse.ArgumentParser(
        prog="python -m opendaisugi.lora.train",
        description="QLoRA trainer for envelope generation (runs on GPU box)",
    )
    parser.add_argument(
        "--jsonl",
        required=True,
        help="Path to training JSONL emitted by opendaisugi.lora.dataset.emit_jsonl",
    )
    parser.add_argument(
        "--base-model",
        default=None,
        help="Hugging Face model id or local path to fine-tune (default: the model "
        "`daisugi models use` recorded, else the catalog default for this hardware)",
    )
    parser.add_argument(
        "--data-dir",
        default=None,
        help="Daisugi data directory that holds the recorded model (default: ~/.opendaisugi)",
    )
    parser.add_argument(
        "--output",
        required=True,
        help="Directory to write the LoRA adapter and tokenizer",
    )
    parser.add_argument(
        "--format",
        choices=["alpaca", "chat"],
        default="alpaca",
        help="Dataset format (must match emit_jsonl --format)",
    )
    parser.add_argument(
        "--qlora",
        action="store_true",
        help="Load base model in 4-bit (NF4) — required for 7B on 16 GB VRAM",
    )
    parser.add_argument(
        "--lora-r",
        type=int,
        default=16,
        help="LoRA rank (default 16)",
    )
    parser.add_argument(
        "--lora-alpha",
        type=int,
        default=32,
        help="LoRA alpha (default 32)",
    )
    parser.add_argument(
        "--lora-dropout",
        type=float,
        default=0.05,
    )
    parser.add_argument(
        "--epochs",
        type=int,
        default=3,
    )
    parser.add_argument(
        "--lr",
        type=float,
        default=2e-4,
    )
    parser.add_argument(
        "--batch-size",
        type=int,
        default=4,
    )
    parser.add_argument(
        "--grad-accum",
        type=int,
        default=4,
        help="Gradient accumulation steps (effective batch = batch-size * grad-accum)",
    )
    parser.add_argument(
        "--max-seq-len",
        type=int,
        default=2048,
    )
    parser.add_argument(
        "--seed",
        type=int,
        default=42,
    )
    parser.add_argument(
        "--dry-run",
        action="store_true",
        help="Parse args + load dataset, then exit without training (smoke test)",
    )
    return parser


def resolve_base_model(args: argparse.Namespace) -> str:
    """--base-model, else the recorded garden model, else the catalog
    default for this hardware."""
    if args.base_model:
        return args.base_model
    from pathlib import Path

    from opendaisugi.hardware import detect_voice_hardware
    from opendaisugi.model_catalog import base_model

    data_dir = Path(args.data_dir) if args.data_dir else Path.home() / ".opendaisugi"
    return base_model(data_dir, detect_voice_hardware())


def has_experts(config) -> bool:
    """Whether a model config has mixture-of-experts layers."""
    n = getattr(config, "num_local_experts", None) or getattr(config, "num_experts", None)
    return bool(n)


def sft_kwargs(args: argparse.Namespace, cuda: bool, moe: bool = False) -> dict:
    """The SFTConfig arguments. trl calls the sequence limit max_length
    (it was max_seq_length before trl 0.20). With no GPU, transformers
    trains in bf16 on the CPU only when use_cpu is set. The loss is the
    model's own ("nll"): trl's default chunked loss reads MoE fields that
    a Granite 4 hybrid config does not have. A model with no experts gets
    no router loss: a dense Granite 4 keeps MoE fields in its config, and
    trl would ask it for router logits it does not have."""
    kw = {
        "output_dir": args.output,
        "num_train_epochs": args.epochs,
        "per_device_train_batch_size": args.batch_size,
        "gradient_accumulation_steps": args.grad_accum,
        "learning_rate": args.lr,
        "max_length": args.max_seq_len,
        "logging_steps": 10,
        "save_strategy": "epoch",
        "bf16": True,
        "seed": args.seed,
        "report_to": "none",
        "use_cpu": not cuda,
        "loss_type": "nll",
    }
    if not moe:
        kw["router_aux_loss_coef"] = 0.0
    return kw


def _train(args: argparse.Namespace) -> int:
    # Lazy imports — fail loudly on the GPU box only, never at module import.
    # In --dry-run we import only `datasets` so CPU-only laptops can smoke-test
    # the JSONL path without triggering bitsandbytes CUDA init.
    from datasets import load_dataset

    dataset = load_dataset("json", data_files=args.jsonl, split="train")

    if args.format == "alpaca":

        def fmt(row):
            instruction = row["instruction"]
            output = row["output"]
            return {"text": f"### Instruction:\n{instruction}\n\n### Response:\n{output}"}

        dataset = dataset.map(fmt, remove_columns=dataset.column_names)
    # chat format is already in {"messages": [...]} — SFTTrainer handles it.

    if args.dry_run:
        print(f"[dry-run] parsed args + loaded dataset ({len(dataset)} rows); skipping training")
        return 0

    import torch
    from peft import LoraConfig
    from transformers import AutoModelForCausalLM, AutoTokenizer, BitsAndBytesConfig
    from trl import SFTConfig, SFTTrainer

    tokenizer = AutoTokenizer.from_pretrained(args.base_model, use_fast=True)
    if tokenizer.pad_token is None:
        tokenizer.pad_token = tokenizer.eos_token

    quant_config = None
    if args.qlora:
        quant_config = BitsAndBytesConfig(
            load_in_4bit=True,
            bnb_4bit_quant_type="nf4",
            bnb_4bit_compute_dtype=torch.bfloat16,
            bnb_4bit_use_double_quant=True,
        )

    model = AutoModelForCausalLM.from_pretrained(
        args.base_model,
        quantization_config=quant_config,
        torch_dtype=torch.bfloat16,
        device_map="auto",
    )

    lora_config = LoraConfig(
        r=args.lora_r,
        lora_alpha=args.lora_alpha,
        lora_dropout=args.lora_dropout,
        bias="none",
        task_type="CAUSAL_LM",
        target_modules=["q_proj", "k_proj", "v_proj", "o_proj"],
    )

    sft_config = SFTConfig(
        **sft_kwargs(args, cuda=torch.cuda.is_available(), moe=has_experts(model.config))
    )

    trainer = SFTTrainer(
        model=model,
        args=sft_config,
        train_dataset=dataset,
        peft_config=lora_config,
        processing_class=tokenizer,
    )
    trainer.train()
    trainer.save_model(args.output)
    tokenizer.save_pretrained(args.output)
    print(f"Adapter saved to {args.output}")
    return 0


def main(argv: Sequence[str] | None = None) -> int:
    """CLI entry point. Parses args, then hands off to ``_train``."""
    parser = _build_parser()
    args = parser.parse_args(argv)
    args.base_model = resolve_base_model(args)
    return _train(args)


if __name__ == "__main__":
    raise SystemExit(main())
