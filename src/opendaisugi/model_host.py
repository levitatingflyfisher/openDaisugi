"""Probe a self-hosted model server and report what is there.

The zero-config path is the machine in front of you. A remote host on the
tailnet or the LAN is a swap, not a requirement, and this module never
trusts it blind. :func:`probe` makes one round trip per candidate wire:
the Ollama native API, an OpenAI compatible ``/v1``, or the Anthropic
Messages wire. It reports exactly what it found, and it reports when it
found nothing. :func:`record` persists the result. This module never
guesses a fact it did not check.

The probe needs the ``[gateway]`` extra, which provides httpx. The token
saving gateway needs the same extra. httpx is imported inside
:func:`probe`, so a bare ``pip install opendaisugi`` still imports this
module. Only a call to ``probe()`` without the extra raises a teaching
``ImportError``.
"""

from __future__ import annotations

import re
import time
from dataclasses import dataclass
from pathlib import Path
from typing import TYPE_CHECKING

from opendaisugi.config import Config, load_config, save_config
from opendaisugi.exceptions import ModelHostUnknownError

if TYPE_CHECKING:
    import httpx

# The documented Ollama default port. A bare host with no port and no
# embedded ":" assumes it. Other servers need an explicit port. There is no
# common default across llamafile, vLLM, and LM Studio.
_OLLAMA_DEFAULT_PORT = 11434

# The floor agentic coding needs. Below it, expect truncation on real repos
# and long tool outputs.
CONTEXT_FLOOR = 32768

_KIND_ORDER = ("ollama", "openai", "anthropic")

_NUM_CTX_RE = re.compile(r"\bnum_ctx\s+(\d+)")


@dataclass(frozen=True)
class HostInfo:
    """What one round trip to a candidate model host found."""

    base_url: str
    kind: str  # ollama | openai | anthropic | unknown
    models: list[str]
    chosen: str | None
    context_window: int | None
    latency_ms: float
    warnings: list[str]
    # False when no candidate wire got any HTTP response at all. The host is
    # then unreachable, which is a different failure from a host that
    # answered on no known wire.
    reachable: bool = True


@dataclass
class _Reach:
    """Records whether any request in one probe got an HTTP response."""

    answered: bool = False


@dataclass(frozen=True)
class _Probed:
    """One wire's positive identification: the host speaks it, and this is what it has."""

    models: list[str]
    chosen: str | None
    context_window: int | None


def context_floor_warning(context_window: int | None) -> str | None:
    """Return the one honest warning about a context window, or None when it needs none.

    :func:`probe` uses it for what it found. The CLI uses it for what an
    explicit ``--context`` override means. The two never drift.
    """
    if context_window is None:
        return "context window unknown; set it with `--context 32768` if you know it"
    if context_window < CONTEXT_FLOOR:
        return (
            f"{context_window} tokens is below the 32K floor agentic coding needs; "
            "expect truncation"
        )
    return None


def parse_remote(spec: str) -> tuple[str, int | None]:
    """Split a ``host[:port]`` CLI argument into ``(host, port)``.

    No port suffix means ``port=None``. :func:`probe` then assumes the
    Ollama default port, the most common self-hosted default. A suffix that
    is not decimal stays part of the host. An IPv6 literal is out of scope.
    Name the port explicitly in that case.
    """
    if "://" in spec:
        raise ValueError(f"give host[:port] without a scheme, for example box:11434; got {spec!r}")
    if ":" in spec:
        host, _, port_s = spec.rpartition(":")
        if port_s.isdigit():
            return host, int(port_s)
    return spec, None


def _base_url(host: str, port: int | None) -> str:
    if port is not None:
        return f"http://{host}:{port}"
    if ":" in host:
        return f"http://{host}"
    return f"http://{host}:{_OLLAMA_DEFAULT_PORT}"


def _ollama_context_length(show: dict) -> int | None:
    """Return the model's architectural window from ``model_info``.

    The key carries a family prefix: ``llama.context_length``,
    ``qwen2.context_length``, and so on. See POST /api/show at
    docs.ollama.com/api.
    """
    info = show.get("model_info")
    if not isinstance(info, dict):
        return None
    for key, value in info.items():
        if key.endswith(".context_length") and isinstance(value, int):
            return value
    return None


def _ollama_num_ctx(show: dict) -> int | None:
    """Return the model's deployed window when the Modelfile pins ``num_ctx``.

    This can be smaller than the architectural max, and it is what runs.
    The Ollama docs show ``num_ctx`` inside ``modelfile`` as a
    ``PARAMETER num_ctx N`` line for some models and inside ``parameters``
    for others. See tests/fixtures/model_host/SOURCES.txt. Both are checked.
    """
    for field_name in ("parameters", "modelfile"):
        text = show.get(field_name)
        if isinstance(text, str):
            m = _NUM_CTX_RE.search(text)
            if m:
                return int(m.group(1))
    return None


def _send(
    cl: "httpx.Client", reach: _Reach, method: str, url: str, **kw
) -> "httpx.Response | None":
    """Make one request. Return None on any transport error. Mark the host answered on any response."""
    try:
        resp = cl.request(method, url, **kw)
    except Exception:
        return None
    reach.answered = True
    return resp


def _probe_ollama(
    cl: "httpx.Client", base: str, timeout_s: float, reach: _Reach
) -> "_Probed | None":
    resp = _send(cl, reach, "GET", f"{base}/api/tags", timeout=timeout_s)
    if resp is None or resp.status_code != 200:
        return None
    try:
        data = resp.json()
    except Exception:
        return None
    if not isinstance(data, dict) or not isinstance(data.get("models"), list):
        return None
    models = [m["name"] for m in data["models"] if isinstance(m, dict) and m.get("name")]
    chosen = models[0] if models else None
    context_window = None
    if chosen is not None:
        show_resp = _send(
            cl, reach, "POST", f"{base}/api/show", json={"model": chosen}, timeout=timeout_s
        )
        # The host is Ollama. A failed /api/show only leaves the context unknown.
        if show_resp is not None and show_resp.status_code == 200:
            try:
                show = show_resp.json()
            except Exception:
                show = None
            if isinstance(show, dict):
                context_window = _ollama_num_ctx(show) or _ollama_context_length(show)
    return _Probed(models=models, chosen=chosen, context_window=context_window)


# Extension keys some OpenAI compatible servers attach to a /v1/models
# entry. No published standard covers this, so this is best effort only.
# See tests/fixtures/model_host/SOURCES.txt.
_OPENAI_CONTEXT_KEYS = ("context_length", "context_window", "max_model_len", "n_ctx_train")


def _probe_openai(
    cl: "httpx.Client", base: str, timeout_s: float, reach: _Reach
) -> "_Probed | None":
    resp = _send(cl, reach, "GET", f"{base}/v1/models", timeout=timeout_s)
    if resp is None or resp.status_code != 200:
        return None
    try:
        data = resp.json()
    except Exception:
        return None
    if not isinstance(data, dict):
        return None
    entries = data.get("data")
    if not isinstance(entries, list):
        return None
    models = [e["id"] for e in entries if isinstance(e, dict) and e.get("id")]
    chosen = models[0] if models else None
    context_window = None
    for e in entries:
        if isinstance(e, dict) and e.get("id") == chosen:
            for key in _OPENAI_CONTEXT_KEYS:
                value = e.get(key)
                if isinstance(value, int):
                    context_window = value
                    break
            break
    return _Probed(models=models, chosen=chosen, context_window=context_window)


# The placeholder model name the Anthropic wire probe sends. A server that
# echoes it back has not told us which model it serves.
_PROBE_MODEL = "daisugi-probe"


def _probe_anthropic(
    cl: "httpx.Client", base: str, timeout_s: float, reach: _Reach
) -> "_Probed | None":
    body = {
        "model": _PROBE_MODEL,
        "max_tokens": 1,
        "messages": [{"role": "user", "content": "hi"}],
    }
    resp = _send(cl, reach, "POST", f"{base}/v1/messages", json=body, timeout=timeout_s)
    if resp is None or resp.status_code != 200:
        return None
    try:
        data = resp.json()
    except Exception:
        return None
    if not isinstance(data, dict) or data.get("type") != "message":
        return None
    model_name = data.get("model")
    if not isinstance(model_name, str) or not model_name or model_name == _PROBE_MODEL:
        # The server did not name a model it serves. Record none, not a guess.
        return _Probed(models=[], chosen=None, context_window=None)
    return _Probed(models=[model_name], chosen=model_name, context_window=None)


_PROBERS = {"ollama": _probe_ollama, "openai": _probe_openai, "anthropic": _probe_anthropic}


def probe(
    host: str,
    port: int | None,
    *,
    kind: str = "auto",
    timeout_s: float = 3.0,
    client: "httpx.Client | None" = None,
) -> HostInfo:
    """Identify what runs at ``host[:port]`` in one round trip per candidate wire.

    ``kind="auto"`` tries the Ollama native API, then an OpenAI compatible
    ``/v1``, then the Anthropic Messages wire. The first one that answers
    wins. An explicit ``kind`` runs that one probe only. If it does not
    answer, the result is ``kind="unknown"``. There is never a silent
    fallback to a wire the operator did not ask for. ``client`` is
    injectable so tests can supply an ``httpx.MockTransport``. Production
    creates its own client and closes it.
    """
    try:
        import httpx
    except ImportError as exc:
        raise ImportError(
            "the model-host probe needs the [gateway] extra: uv add 'opendaisugi[gateway]' "
            "or pip install 'opendaisugi[gateway]'"
        ) from exc

    if kind != "auto" and kind not in _PROBERS:
        raise ValueError(
            f"unknown host kind {kind!r}; choose one of: auto, {', '.join(_KIND_ORDER)}"
        )

    base = _base_url(host, port)
    order = list(_KIND_ORDER) if kind == "auto" else [kind]

    owns_client = client is None
    cl = client or httpx.Client()
    t0 = time.monotonic()
    reach = _Reach()
    try:
        for k in order:
            result = _PROBERS[k](cl, base, timeout_s, reach)
            if result is None:
                continue
            latency_ms = (time.monotonic() - t0) * 1000
            warning = context_floor_warning(result.context_window)
            return HostInfo(
                base_url=base,
                kind=k,
                models=result.models,
                chosen=result.chosen,
                context_window=result.context_window,
                latency_ms=latency_ms,
                warnings=[warning] if warning else [],
            )
        latency_ms = (time.monotonic() - t0) * 1000
        if reach.answered:
            warning = (
                f"could not identify a model server at {base}; it answered on none of: "
                f"{', '.join(order)}. Check the port, or pass --kind for the wire it speaks."
            )
        else:
            warning = (
                f"could not reach {base}; no request got a response. "
                "Check that the server runs and that this machine can reach it."
            )
        return HostInfo(
            base_url=base,
            kind="unknown",
            models=[],
            chosen=None,
            context_window=None,
            latency_ms=latency_ms,
            warnings=[warning],
            reachable=reach.answered,
        )
    finally:
        if owns_client:
            cl.close()


_KIND_TO_BACKEND = {
    "ollama": "ollama",
    "openai": "openai-compatible",
    "anthropic": "anthropic-compatible",
}


# Every value `daisugi gateway --upstream-kind` accepts. "anthropic" is the
# real API and turns the count-tokens shim off. The other three are
# self-hosted wires and turn it on.
UPSTREAM_KINDS = ("anthropic", "ollama", "openai-compatible", "anthropic-compatible")


def upstream_kind_for_recorded_host(host_kind: str) -> str:
    """Map a recorded ``llm_host_kind`` onto the gateway's ``upstream_kind`` vocabulary.

    A recorded host is self-hosted whatever wire it speaks. The result is
    never ``"anthropic"``, the value that stands for the real API and turns
    the count-tokens shim off. An unknown kind maps to
    ``"anthropic-compatible"`` so the shim stays on.
    """
    return _KIND_TO_BACKEND.get(host_kind, "anthropic-compatible")


def record(
    info: HostInfo,
    *,
    model: str | None = None,
    context_window: int | None = None,
    config_path: Path | None = None,
) -> Config:
    """Persist a probed host as the operator's recorded model host.

    Refuses an unidentified host with ``ModelHostUnknownError``. There is
    nothing honest to write for a wire the probe could not identify.
    ``model`` and ``context_window`` override the probe's own ``chosen``
    and ``context_window``. They carry an explicit ``--model`` or
    ``--context`` the operator supplied. Returns the updated, saved
    ``Config``.
    """
    if info.kind not in _KIND_TO_BACKEND:
        reason = info.warnings[-1] if info.warnings else f"unknown host kind {info.kind!r}"
        raise ModelHostUnknownError(f"cannot record {info.base_url}: {reason}")

    cfg = load_config(config_path)
    cfg = cfg.model_copy(
        update={
            "llm_base_url": info.base_url,
            "llm_host_kind": info.kind,
            "llm_host_model": model or info.chosen,
            "llm_context_window": (
                context_window if context_window is not None else info.context_window
            ),
            "llm_backend": _KIND_TO_BACKEND[info.kind],
        }
    )
    save_config(cfg, config_path)
    return cfg


def harness_env(info: HostInfo) -> dict[str, str]:
    """Return the environment variables that point an Anthropic wire harness at this host.

    Claude Code is such a harness. The result is empty when the host does
    not speak that wire. An ``openai`` host needs an OpenAI wire harness
    instead, or the gateway's OpenAI wire path.
    """
    if info.kind == "ollama":
        # docs.ollama.com/api/anthropic-compatibility: base_url WITHOUT /v1,
        # because the SDK appends it. The api key is accepted but not
        # validated. "ollama" is the documented literal token.
        return {"ANTHROPIC_BASE_URL": info.base_url, "ANTHROPIC_AUTH_TOKEN": "ollama"}
    if info.kind == "anthropic":
        return {"ANTHROPIC_BASE_URL": info.base_url}
    return {}


def describe_host(info: HostInfo, config: Config) -> list[str]:
    """Return the lines ``daisugi tiers setup --remote`` prints.

    One line per fact, then the exact env a harness needs. The warning is
    read fresh off ``config``, not off ``info.warnings``, so an explicit
    ``--context`` override shows honestly instead of a stale probe time
    warning.
    """
    lines = [
        f"host: {info.base_url} ({info.kind})",
        f"model: {config.llm_host_model or 'unset'}",
    ]
    if config.llm_context_window:
        lines.append(f"context window: {config.llm_context_window} tokens")
    else:
        lines.append("context window: unknown")
    warning = context_floor_warning(config.llm_context_window)
    if warning:
        lines.append(f"warning: {warning}")
    env = harness_env(info)
    if env:
        lines.append("point your harness at it:")
        for k, v in env.items():
            lines.append(f"  {k}={v}")
        lines.append(
            f"or run: daisugi gateway --upstream {info.base_url} and point the harness "
            "at the gateway. The gateway answers count_tokens itself. Most self-hosted servers do not."
        )
    else:
        lines.append(
            "this host speaks the OpenAI wire only. Point an OpenAI wire harness at it "
            "directly, or route it through `daisugi gateway --openai-upstream`."
        )
    return lines
