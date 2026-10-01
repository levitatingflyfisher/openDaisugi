"""The model catalog for the garden.

``model_catalog.json`` beside this file is the curated list, shared with
the Go and Rust clients. Each entry holds only: id, params (an integer
count), license, context_length, gguf (a GGUF repo id or null), suits
(``capable`` or ``weak``) and a one-line note on what the model is good
at. Any other model works too: ``daisugi models search`` reads the
Hugging Face API, and ``daisugi models use ID`` records any id, a local
path or a GGUF file as given.

The default comes from the hardware probe the voice bridge and ``tiers
setup`` use: a box is ``capable`` with enough RAM or GPU memory, else
``weak``. ``OPENDAISUGI_VOICE_HARDWARE`` replaces the probe in tests.
"""

from __future__ import annotations

import http.client
import json
import os
import urllib.error
import urllib.parse
import urllib.request
from collections.abc import Callable, Mapping
from pathlib import Path
from typing import Any

from opendaisugi.hardware import VoiceHardware

CATALOG_PATH = Path(__file__).with_name("model_catalog.json")
CHOICE_FILE = "garden_model.json"
ENTRY_KEYS = ("id", "params", "license", "context_length", "gguf", "suits", "note")
DEFAULT_ENDPOINT = "https://huggingface.co"
SEARCH_LIMIT = 100
FETCH_TIMEOUT = 30
_EXPAND = ("cardData", "gguf", "pipeline_tag", "safetensors", "tags")
_HEADERS = ("ID", "SIZE", "LICENSE", "CONTEXT", "GGUF", "SUITS", "GOOD AT")


class Offline(Exception):
    """The Hugging Face API could not be reached, or HF_HUB_OFFLINE is set."""


class HTTPStatus(Exception):
    """The API answered with a status other than 200."""

    def __init__(self, code: int):
        super().__init__(f"HTTP {code}")
        self.code = code


class BadAnswer(Exception):
    """The API answered with something that is not a model list."""


def load() -> dict[str, Any]:
    return json.loads(CATALOG_PATH.read_text(encoding="utf-8"))


def hardware_class(hw: VoiceHardware, cat: dict[str, Any] | None = None) -> str:
    cat = cat or load()
    if hw.vram_gb >= cat["capable_min_vram_gb"]:
        return "capable"
    if hw.ram_gb is not None and hw.ram_gb >= cat["capable_min_ram_gb"]:
        return "capable"
    return "weak"


def default_model(hw: VoiceHardware, cat: dict[str, Any] | None = None) -> str:
    cat = cat or load()
    return cat["defaults"][hardware_class(hw, cat)]


def hardware_line(hw: VoiceHardware, cat: dict[str, Any] | None = None) -> str:
    cat = cat or load()
    ram = "an unknown amount of RAM" if hw.ram_gb is None else f"{hw.ram_gb:g} GB of RAM"
    gpu = f" and a GPU with {hw.vram_gb:g} GB" if hw.vram_gb > 0 else ""
    return f"This box has {ram}{gpu}, so the default is {default_model(hw, cat)}."


def in_use(data_dir: Path) -> str | None:
    """The id ``daisugi models use`` recorded, or None."""
    try:
        body = json.loads((Path(data_dir) / CHOICE_FILE).read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return None
    model = body.get("model") if isinstance(body, dict) else None
    return model if isinstance(model, str) and model.strip() else None


def record_choice(data_dir: Path, model_id: str) -> Path:
    """Record ``model_id`` as given. Any id, path or GGUF file is accepted."""
    if not model_id.strip():
        raise ValueError("a model id is not blank")
    path = Path(data_dir) / CHOICE_FILE
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps({"model": model_id}, indent=2) + "\n", encoding="utf-8")
    return path


def base_model(data_dir: Path, hw: VoiceHardware) -> str:
    """The model the garden trains from: the recorded choice, else the
    default for this hardware."""
    return in_use(data_dir) or default_model(hw)


def fmt_params(n: int | None) -> str:
    return "?" if n is None else f"{n / 1e9:.1f}B"


def endpoint(env: Mapping[str, str]) -> str:
    return (env.get("HF_ENDPOINT") or DEFAULT_ENDPOINT).rstrip("/")


def offline_env(env: Mapping[str, str]) -> bool:
    """HF_HUB_OFFLINE, read as huggingface_hub reads it."""
    return (env.get("HF_HUB_OFFLINE") or "").upper() in ("1", "ON", "YES", "TRUE")


def search_target(query: str) -> str:
    """The request target, built in one fixed order. Every port encodes
    the query the same way: unreserved ASCII kept, every other byte of
    its UTF-8 as %XX."""
    parts = [
        "search=" + urllib.parse.quote(query, safe=""),
        f"limit={SEARCH_LIMIT}",
        "sort=downloads",
        "direction=-1",
    ]
    parts += [f"expand%5B%5D={e}" for e in _EXPAND]
    return "/api/models?" + "&".join(parts)


def _int(v: Any) -> int | None:
    return v if isinstance(v, int) and not isinstance(v, bool) else None


def _obj(v: Any) -> dict[str, Any]:
    return v if isinstance(v, dict) else {}


def _license(row: dict[str, Any]) -> str | None:
    card = _obj(row.get("cardData"))
    lic = card.get("license")
    if lic == "other" and isinstance(card.get("license_name"), str):
        return card["license_name"]
    if isinstance(lic, str):
        return lic
    tags = row.get("tags")
    for t in tags if isinstance(tags, list) else []:
        if isinstance(t, str) and t.startswith("license:"):
            return t[len("license:") :]
    return None


def parse_listing(raw: bytes, cat: dict[str, Any] | None = None) -> list[dict[str, Any]]:
    """The API's model list, each entry in the catalog's fields. An entry
    that is not an object with a string id is skipped."""
    cat = cat or load()
    try:
        body = json.loads(raw)
    except ValueError as exc:
        raise BadAnswer("not JSON") from exc
    if not isinstance(body, list):
        raise BadAnswer("not a list")
    rows = []
    for row in body:
        if not isinstance(row, dict) or not isinstance(row.get("id"), str):
            continue
        st, gg = _obj(row.get("safetensors")), _obj(row.get("gguf"))
        params = _int(st.get("total"))
        if params is None:
            params = _int(gg.get("total"))
        tags = row.get("tags")
        has_gguf = "gguf" in row and isinstance(row["gguf"], dict)
        has_gguf = has_gguf or (isinstance(tags, list) and "gguf" in tags)
        suits = None
        if params is not None:
            suits = "weak" if params <= cat["weak_max_params"] else "capable"
        note = row.get("pipeline_tag")
        rows.append(
            {
                "id": row["id"],
                "params": params,
                "license": _license(row),
                "context_length": _int(gg.get("context_length")),
                "gguf": row["id"] if has_gguf else None,
                "suits": suits,
                "note": note if isinstance(note, str) else "",
            }
        )
    return rows


def filter_rows(
    rows: list[dict[str, Any]], max_params_b: float, licenses: list[str], limit: int
) -> list[dict[str, Any]]:
    """Keep rows at most ``max_params_b`` billion parameters (0: any size;
    an unknown size is kept only then) and, when ``licenses`` names any,
    under one of them. The first ``limit`` are returned."""
    out = []
    for r in rows:
        if max_params_b > 0 and (r["params"] is None or r["params"] > max_params_b * 1e9):
            continue
        if licenses and r["license"] not in licenses:
            continue
        out.append(r)
    return out[:limit]


def fetch(url: str) -> bytes:
    req = urllib.request.Request(url, headers={"User-Agent": "opendaisugi"})
    try:
        with urllib.request.urlopen(req, timeout=FETCH_TIMEOUT) as resp:
            return resp.read()
    except urllib.error.HTTPError as exc:
        raise HTTPStatus(exc.code) from exc
    except (urllib.error.URLError, http.client.HTTPException, OSError, ValueError) as exc:
        raise Offline(str(exc)) from exc


def search(
    query: str,
    *,
    env: Mapping[str, str] | None = None,
    fetch: Callable[[str], bytes] | None = None,
) -> list[dict[str, Any]]:
    """Every model the API lists for ``query``, unfiltered. ``fetch``
    defaults to this module's own, looked up at call time."""
    env = os.environ if env is None else env
    if offline_env(env):
        raise Offline("HF_HUB_OFFLINE is set")
    get = fetch or globals()["fetch"]
    return parse_listing(get(endpoint(env) + search_target(query)))


def _cells(r: dict[str, Any]) -> list[str]:
    ctx = r["context_length"]
    return [
        r["id"],
        fmt_params(r["params"]),
        r["license"] or "?",
        "?" if ctx is None else str(ctx),
        "yes" if r["gguf"] else "no",
        r["suits"] or "?",
        r["note"],
    ]


def table_lines(rows: list[dict[str, Any]]) -> list[str]:
    """A header and one line per row, columns padded, the last never."""
    grid = [list(_HEADERS)] + [_cells(r) for r in rows]
    widths = [max(len(row[i]) for row in grid) for i in range(len(_HEADERS) - 1)]
    lines = []
    for row in grid:
        cells = [c.ljust(w) for c, w in zip(row, widths, strict=False)] + [row[-1]]
        lines.append(("  " + "  ".join(cells)).rstrip())
    return lines
