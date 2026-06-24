"""Parser protocol, models, and registry for transcript import."""

from __future__ import annotations

from pathlib import Path
from typing import Protocol

from pydantic import BaseModel

from opendaisugi.models import ActionStep


class Episode(BaseModel):
    """A coherent unit of work extracted from an agent transcript."""

    id: str
    task: str
    context: str | None = None
    steps: list[ActionStep]
    source_range: dict  # {"first_message": int, "last_message": int}


class ParseResult(BaseModel):
    """Output of parsing an agent transcript into episodes."""

    source: str  # "claude-code", "hermes", "openclaw", etc.
    source_file: str
    parsed_at: str
    episodes: list[Episode]


class ConversationParser(Protocol):
    """Protocol for transcript parsers. Implement and register for new formats."""

    def parse(self, path: Path) -> ParseResult: ...


_PARSERS: dict[str, type] = {}


def register_parser(format_name: str, parser_cls: type) -> None:
    """Register a parser class for a given format name."""
    _PARSERS[format_name] = parser_cls


def get_parser(format_name: str = "claude-code", **kwargs) -> ConversationParser:
    """Instantiate a registered parser by format name.

    Extra ``kwargs`` are forwarded to the parser constructor
    (e.g. ``min_tools``, ``max_tools``, ``model``).
    """
    if format_name not in _PARSERS:
        available = sorted(_PARSERS) or ["(none registered)"]
        raise ValueError(
            f"Unknown parser format: {format_name!r}. "
            f"Available: {available}"
        )
    return _PARSERS[format_name](**kwargs)


def _register_builtin_parsers() -> None:
    from opendaisugi.parsers.claude_code import ClaudeCodeParser

    register_parser("claude-code", ClaudeCodeParser)


_register_builtin_parsers()
