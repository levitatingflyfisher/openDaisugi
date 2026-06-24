"""LoRA training-data pipeline (v0.5.0).

openDaisugi's three-tier story compresses frontier knowledge into the
compiled-pathway store (Tier 0). LoRA is the next compression step:
take the journal's successful (task, envelope) pairs and fine-tune a
small base model so Tier-1 weights — not just Tier-1 prompts —
encode the conventions the frontier learned.

This package ships the *dataset* side of that pipeline: scanning the
journal and emitting training pairs in standard formats (JSONL alpaca
/ chat-turns). The actual fine-tune runs outside the library, on
whatever hardware the user has — see README for the recipe.
"""

from __future__ import annotations

from opendaisugi.lora.dataset import (
    DatasetStats,
    TrainingExample,
    emit_jsonl,
    iter_training_examples,
)

__all__ = [
    "DatasetStats",
    "TrainingExample",
    "emit_jsonl",
    "iter_training_examples",
]
