"""User configuration for opendaisugi.

Loads from and saves to ``~/.opendaisugi/config.yaml``. The Daisugi facade
constructor kwargs override whatever is loaded from disk — config.yaml is
a default source, not an authoritative one.
"""

from __future__ import annotations

import json
import os
import re
from collections.abc import Callable, Mapping
from dataclasses import dataclass
from pathlib import Path

import yaml
from pydantic import BaseModel, Field


class FloorConfig(BaseModel):
    """Which pane backend drives the floor, and what runs when a pane blocks.

    ``backend`` is LIVE. The running code reads it on the next pane pick,
    with no restart. ``auto`` takes the first of coppice, herdr, tmux that
    answers. A named backend that is not available is refused, never
    downgraded to a different one.

    ``notify_cmd`` runs with the event JSON on stdin when a pane blocks. It
    is the operator's own command. openDaisugi ships no relay and no
    default.
    """

    backend: str = "auto"  # auto | coppice | herdr | tmux
    notify_cmd: str | None = None
    tmux_socket: str | None = None  # tmux -L NAME; None uses the default server
    coppice_socket: str | None = None  # --socket PATH; None uses coppice's own default


class Config(BaseModel):
    """Typed config with sensible defaults for every field."""

    model: str = "anthropic/claude-sonnet-4-20250514"
    max_task_chars: int = 4000
    z3_timeout_ms: int = 500
    data_dir: Path = Field(default_factory=lambda: Path.home() / ".opendaisugi")
    # Background distillation consent (Phase A). None = never asked (distinct
    # from an explicit no); True = distil repeated tasks in the background;
    # False = declined. Distillation only ever affects *efficiency* (the guard
    # enforces safety regardless), so it is safe to automate once consented.
    auto_tend: bool | None = None
    # ADR-0015: a qualified local model for the gateway's local rung — easy
    # turns route here ahead of any cloud downgrade (zero quota; cache
    # stickiness never blocks the local rung). None = no local rung. Set it to
    # the model id `daisugi tiers setup` qualified, and start the proxy with
    # `daisugi gateway` (the flag --local-model overrides per run).
    gateway_local_model: str | None = None
    # Which chooser picks the model for each gateway turn. "rules" is the
    # built-in heuristic with no dependency. "switchyard" hands the choice to
    # NVIDIA NeMo Switchyard, which `daisugi gateway` starts as a managed
    # child on loopback. "off" forwards every turn unchanged and only meters
    # it. `daisugi gateway` reads this field once when it starts. A change
    # here needs a restart of the gateway, unlike gateway_local_model.
    gateway_router: str = "rules"  # rules | switchyard | off
    # The route id the gateway sends as `model` to select the Switchyard
    # route. Switchyard wants the route id, never a real model name.
    switchyard_route_id: str = "daisugi"
    # The two target ids of the Switchyard route. The gateway meters a turn
    # by comparing the served target that Switchyard names against these ids.
    switchyard_capable_model: str = "claude-sonnet-5"
    # None until you name a cheaper model for the efficient tier. A local
    # model id runs on llm_base_url, or on Ollama at loopback when no host is
    # recorded. A claude-* id runs on the Anthropic API.
    switchyard_efficient_model: str | None = None
    # Every Switchyard cloud tier forwards your own login by default, as the
    # gateway does without Switchyard. Name an environment variable here to
    # make every cloud tier use that key instead. That key is billed per token.
    switchyard_api_key_env: str | None = None
    # ADR-0010 compound-shell decomposition, persisted so it also reaches the
    # paths no CLI flag can: `hook auto-tend` runs from cron and from a detached
    # spawn. Off by default — with it on, an envelope's allowlist admits
    # `a && b` (every head checked) instead of the blanket metachar rejection.
    shell_allow_decomposition: bool = False

    # --- module-selection knobs (see opendaisugi.swap / `daisugi modules`) -----
    # These make the pipeline's stages configurable. Two of them the running
    # code actually reads (shell_allow_decomposition above; gate_mode below when
    # the hook does not force --mode); the rest are RECORDED PREFERENCES the hot
    # path does not consume yet — the GUI marks them "not yet enforced". They
    # declare intent and are the wiring seam for the alternative; the current
    # implementation (python / MiniLM / evidence-inferred) runs regardless.

    # Gate verdict mode used ONLY as the fallback when the installed hook command
    # does not pass --mode. The hook DOES pass --mode explicitly today (the one
    # thing the agent cannot rewrite), so the flag always wins and this never
    # weakens the authorization boundary; it drives the verdict only for entries
    # invoked without the flag. The import-failure `|| exit 2` backstop is
    # install-time — full enforce still needs `daisugi install --enforce`.
    gate_mode: str = "shadow"  # shadow | enforce

    # Whether an installed gate hook hands a would-deny to a present operator
    # (Task 8, `_maybe_ask`) before letting it stand. Declared here for
    # completeness, not consulted at hook-evaluation time the way `gate_mode`
    # is: `--ask` is baked into the installed command at `daisugi install
    # --gate --ask`, a static argv the agent cannot rewrite, and it must stay
    # that way — unlike `gate_mode`'s narrow, explicitly-documented fallback,
    # letting a writable config.yaml silently turn on an operator hand-off
    # for an already-installed hook would be the exact authority leak
    # `gate_mode`'s own precedence rule exists to prevent.
    gate_ask: bool = False

    # Preferred verifier client. python is the only runtime checker and ALWAYS
    # enforces; the compiled clients are a conformance harness, so this records
    # the client you would run for shadow/experimental differential checking.
    verifier_client: str = "python"  # python | rust | go | typescript | lean

    # Pathway-reuse embedder. all-MiniLM-L6-v2 needs torch. potion is
    # model2vec with no torch, so it runs where torch cannot. lexical is
    # stdlib and numpy with no model and no download. int8 is MiniLM
    # quantized, run through onnxruntime with no torch. All four are built.
    # A missing package for MiniLM, potion, or int8 falls back to lexical,
    # warned once, rather than silently distilling zero pathways. A switch
    # of backend changes the embedding space; re-run `tend`. See ADR-0018,
    # ADR-0019, ADR-0021.
    matcher_model: str = "all-MiniLM-L6-v2"  # | potion | lexical | int8

    # Preferred LLM backend for envelope generation / planning. The running
    # backend is set by OPENDAISUGI_LLM_BACKEND / model; this records intent.
    # `daisugi tiers setup --remote` also writes this field, with a second
    # vocabulary for the same field: the wire a probed remote host speaks.
    # Both readings answer "what backend does this operator's traffic run
    # on". The field is a free string with no runtime dispatch on it, so
    # the overlap is cosmetic, not a collision.
    llm_backend: str | None = None
    # None means auto-detect: pick a backend that runs on this box. Otherwise
    # claude-code | anthropic | llamafile | ollama | openai-compatible | anthropic-compatible

    # The self-hosted model host `daisugi tiers setup --remote` probed and
    # recorded: any box on the tailnet or the LAN. The local machine stays
    # the zero-config default. A remote host is a swap, not a requirement.
    # None means no remote host is recorded. llm_host_kind is the wire the
    # probe found, never a guess. See opendaisugi.model_host.probe().
    # llm_host_model is the model name that host serves. It stays separate
    # from `model` above, which is opendaisugi's own envelope-generation
    # model id in litellm "provider/model" form. Writing a bare host model
    # name such as "qwen3-coder" into `model` would break envelope
    # generation.
    llm_base_url: str | None = None
    llm_host_kind: str | None = None  # ollama | openai | anthropic
    llm_host_model: str | None = None
    llm_context_window: int | None = None  # tokens; None means the probe could not tell

    # Preferred envelope source. evidence-inferred runs zero-LLM; llm-generated
    # needs a configured backend, so the choice is partly derived, not free.
    envelope_source: str = "evidence-inferred"  # | llm-generated

    # Preferred pathway-store backend. sqlite is the local default; the
    # git-backed shared registry is a separate opt-in set up with
    # `daisugi registry init` (a repo + signing keys), not a config toggle.
    pathway_store_backend: str = "sqlite"  # sqlite | git

    # Floor-report preference (spec-01/06). Herdr's own liveness is
    # detected from the installed Stop/Notification hooks, not this field
    # — it only matters for coppice, which isn't built yet (spec-02):
    # `daisugi install --gate --report coppice` records the intent here so
    # `daisugi modules` can show it honestly ahead of the harness existing.
    floor_report: str | None = None  # None | herdr | coppice

    # The floor's pane backend, nested beside floor_report above. floor_report
    # names which hook writes state and is CFG. floor.backend names which
    # backend drives the pane and is LIVE. See FloorConfig.
    floor: FloorConfig = Field(default_factory=FloorConfig)

    # Voice bridge settings. voice_device stays "cpu" until the operator opts
    # in. pick_engine() uses CUDA only when this says "cuda" and ctranslate2
    # reports a visible device. A Pascal GPU (sm_61) has crashed other torch-based
    # inference under CUDA before, see ADR-0019. CUDA is opt-in and verified,
    # never auto-detected.
    voice_engine: str = "faster-whisper"  # faster-whisper | parakeet
    voice_model: str = "tiny.en"  # a faster-whisper model id, or for parakeet a local model dir
    voice_device: str = "cpu"  # cpu | cuda
    voice_compute_type: str = "int8"
    # The optional cleanup pass is off by default. It never uses a paid model
    # unless voice_cleanup_model names one.
    voice_cleanup: bool = False
    voice_cleanup_model: str | None = None
    voice_cleanup_base_url: str | None = None
    voice_server_url: str = "http://127.0.0.1:7477"


def default_config() -> Config:
    """Return a Config populated entirely from field defaults."""
    return Config()


def load_config(path: Path | None = None) -> Config:
    """Load config from ``path`` (default: ``~/.opendaisugi/config.yaml``).

    Returns ``default_config()`` when the file does not exist. Unknown keys
    in the YAML file are silently ignored so that a config written by a
    newer version of opendaisugi still loads on an older version.
    """
    if path is None:
        path = Path.home() / ".opendaisugi" / "config.yaml"
    if not path.exists():
        return default_config()

    raw = yaml.safe_load(path.read_text()) or {}
    known = {f for f in Config.model_fields}
    filtered = {k: v for k, v in raw.items() if k in known}
    return Config(**filtered)


def save_config(config: Config, path: Path | None = None) -> None:
    """Write ``config`` to ``path`` as YAML, creating parent dirs if needed.

    ``Path`` values are serialized as strings. No atomic-write ceremony —
    config.yaml is user-editable and written rarely.
    """
    if path is None:
        path = Path.home() / ".opendaisugi" / "config.yaml"
    path.parent.mkdir(parents=True, exist_ok=True)
    data = config.model_dump(mode="json")
    # Pydantic serializes Path to str in mode="json"; yaml.safe_dump is fine with it.
    path.write_text(yaml.safe_dump(data, sort_keys=True))


def auto_tend_enabled(config: Config) -> bool:
    """True only when the user has explicitly consented to background distillation.

    Unasked (None) and declined (False) both mean "do not auto-tend" — consent
    is opt-in, never assumed.
    """
    return config.auto_tend is True


def ensure_auto_tend_consent(
    config: Config,
    ask: Callable[[], bool],
    *,
    path: Path | None = None,
) -> Config:
    """Ask once whether to distil in the background, persist the answer, return it.

    If ``config.auto_tend`` is already set (True or False), returns ``config``
    unchanged without calling ``ask`` — the question is asked exactly once, ever.
    Otherwise ``ask()`` is invoked (a ``() -> bool`` the caller wires to a prompt),
    the choice is written to ``config`` on disk, and the updated Config returned.
    """
    if config.auto_tend is not None:
        return config
    decided = config.model_copy(update={"auto_tend": bool(ask())})
    save_config(decided, path)
    return decided


# A gate hook command is found by its words, never by a substring, so a
# foreign hook such as `opendaisugi.gateway-watch`, or a `--mode` inside a
# quoted path, is not taken for ours. The command is split as the shell
# splits it (quotes, backslashes, `;` `|` `&` runs as their own words), each
# simple command is read on its own, and these forms are a gate hook:
#
#   [NAME=val ...] [env [opts] [NAME=val ...]] [uv run [opts]] python*|py [flags] -m opendaisugi.gate[_client] ARGS
#   [NAME=val ...] .../daisugi gate check ARGS            (the Go binary)
#   sh|bash|dash|zsh|ksh [opts] -c '<one of the above>'
#
# A command whose words hold `opendaisugi.gate[_client]` (alone, or joined
# to `-m`) in any other form is an unknown gate hook: it may gate, but this
# CLI cannot read its mode, so status says so and uninstall refuses it.
GATE_HOOK_MARKER = "DAISUGI_GATE_HOOK=opendaisugi.gate"
_GATE_MODULES = ("opendaisugi.gate", "opendaisugi.gate_client")
_ASSIGNMENT = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*=")
_PYTHON = re.compile(r"^(python[0-9.]*|py)$")  # py: the launcher name
_JOINED_MODULE = re.compile(r"^-[A-Za-z]*m(opendaisugi\.gate(_client)?)$")
_SHELLS = ("sh", "bash", "dash", "zsh", "ksh")
_PY_NOARG_FLAGS = "bBdEhiIOPqRsSuvx"
_ENV_VALUE_OPTS = ("-u", "--unset", "-C", "--chdir")
_UV_VALUE_OPTS = (
    "--project", "--directory", "--python", "-p", "--with", "--with-editable",
    "--with-requirements", "--env-file", "--extra", "--group", "--only-group", "--no-group",
    "--package", "--index", "--default-index", "--index-url", "--extra-index-url",
    "--find-links", "-f", "--cache-dir", "--config-file", "-w",
)  # fmt: skip
_MAX_SHELL_DEPTH = 3


def command_tokens(command: str) -> list[tuple[str, bool]] | None:
    """Split a command as a POSIX shell does: (word, is_operator) pairs,
    where an operator is an unquoted run of ; | &. None on an unclosed
    quote."""
    out: list[tuple[str, bool]] = []
    cur: list[str] = []
    have = False
    i, n = 0, len(command)

    def flush() -> None:
        nonlocal cur, have
        if have:
            out.append(("".join(cur), False))
        cur, have = [], False

    while i < n:
        c = command[i]
        if c in " \t\r\n":
            flush()
            i += 1
        elif c in ";|&":
            flush()
            j = i
            while j < n and command[j] in ";|&":
                j += 1
            out.append((command[i:j], True))
            i = j
        elif c == "'":
            j = command.find("'", i + 1)
            if j < 0:
                return None
            cur.append(command[i + 1 : j])
            have = True
            i = j + 1
        elif c == '"':
            have = True
            i += 1
            while True:
                if i >= n:
                    return None
                c = command[i]
                if c == '"':
                    i += 1
                    break
                if c == "\\" and i + 1 < n and command[i + 1] in '"\\$`\n':
                    if command[i + 1] != "\n":
                        cur.append(command[i + 1])
                    i += 2
                    continue
                cur.append(c)
                i += 1
        elif c == "\\":
            if i + 1 < n and command[i + 1] != "\n":
                cur.append(command[i + 1])
                have = True
            i += 2
        else:
            cur.append(c)
            have = True
            i += 1
    flush()
    return out


def _segments(tokens: list[tuple[str, bool]]) -> list[list[str]]:
    segs: list[list[str]] = [[]]
    for word, is_op in tokens:
        if is_op:
            segs.append([])
        else:
            segs[-1].append(word)
    return [s for s in segs if s]


def _base(word: str) -> str:
    return word.rsplit("/", 1)[-1]


def _skip_env(words: list[str]) -> list[str]:
    """Drop leading NAME=val words and an `env [opts] [NAME=val ...]` prefix."""
    while words and _ASSIGNMENT.match(words[0]):
        words = words[1:]
    if words and _base(words[0]) == "env":
        words = words[1:]
        while words and words[0].startswith("-"):
            if words[0] == "--":
                words = words[1:]
                break
            words = words[2:] if words[0] in _ENV_VALUE_OPTS else words[1:]
        while words and _ASSIGNMENT.match(words[0]):
            words = words[1:]
    return words


def _python_module_args(words: list[str]) -> list[str] | None:
    """For `python* [flags] -m MODULE ARGS`, the words after a gate MODULE."""
    i = 1
    while i < len(words):
        w = words[i]
        if not w.startswith("-") or w == "-" or w.startswith("--"):
            return None  # a script, stdin, or a long option this reader does not model
        rest = w[1:]
        module = None
        for k, ch in enumerate(rest):
            if ch in _PY_NOARG_FLAGS:
                continue
            if ch == "m":
                module = rest[k + 1 :]
                if not module:
                    if i + 1 >= len(words):
                        return None
                    i += 1
                    module = words[i]
                break
            if ch in "WX":
                if not rest[k + 1 :]:
                    i += 1
                break
            return None  # -c code, or a flag this reader does not model
        if module is not None:
            return words[i + 1 :] if module in _GATE_MODULES else None
        i += 1
    return None


def _segment_gate_args(words: list[str], depth: int) -> tuple[str | None, list[str] | None]:
    """("gate", args), ("unknown", None) or (None, None) for one simple command."""
    words = _skip_env(words)
    if len(words) >= 2 and _base(words[0]) == "uv" and words[1] == "run":
        words = words[2:]
        while words and words[0].startswith("-"):
            if words[0] == "--":
                words = words[1:]
                break
            words = words[2:] if words[0] in _UV_VALUE_OPTS else words[1:]
        words = _skip_env(words)
    if words and _PYTHON.match(_base(words[0])):
        args = _python_module_args(words)
        if args is not None:
            return "gate", args
    elif len(words) >= 3 and _base(words[0]) == "daisugi" and words[1:3] == ["gate", "check"]:
        return "gate", words[3:]
    elif words and _base(words[0]) in _SHELLS and depth < _MAX_SHELL_DEPTH:
        for i, w in enumerate(words[1:], start=1):
            if not w.startswith("-") or w.startswith("--"):
                break
            if "c" in w[1:] and i + 1 < len(words):
                return _gate_hook(words[i + 1], depth + 1)
    if any(w in _GATE_MODULES or _JOINED_MODULE.match(w) for w in words):
        return "unknown", None
    return None, None


def _gate_hook(command: str, depth: int = 0) -> tuple[str | None, list[str] | None]:
    tokens = command_tokens(command)
    if tokens is None:
        return ("unknown", None) if "opendaisugi.gate" in command else (None, None)
    unknown = False
    for seg in _segments(tokens):
        kind, args = _segment_gate_args(seg, depth)
        if kind == "gate":
            return kind, args
        unknown = unknown or kind == "unknown"
    return ("unknown", None) if unknown else (None, None)


def gate_hook_kind(command: object) -> str | None:
    """ "gate", "unknown" (holds the gate module in a form not read), or None."""
    if not isinstance(command, str):
        return None
    return _gate_hook(command)[0]


def gate_hook_args(command: object) -> list[str] | None:
    """The words after the gate entry point, up to the end of its simple
    command, or None if ``command`` is not a gate hook in a form read."""
    if not isinstance(command, str):
        return None
    kind, args = _gate_hook(command)
    return args if kind == "gate" else None


def gate_hook_mode(command: object) -> str | None:
    """The ``--mode`` a gate hook passes (argparse: the last one wins), or
    None if the command is not a gate hook or names no valid mode."""
    args = gate_hook_args(command)
    if args is None:
        return None
    mode = None
    for i, w in enumerate(args):
        if w == "--mode" and i + 1 < len(args):
            mode = args[i + 1]
        elif w.startswith("--mode="):
            mode = w.split("=", 1)[1]
    return mode if mode in ("shadow", "enforce") else None


def is_record_hook(command: object, event: str | None = None) -> bool:
    """True for a `daisugi hook record ...` command, by its words; with
    ``event``, only the one passing `--event <event>`."""
    if not isinstance(command, str):
        return False
    tokens = command_tokens(command)
    if not tokens:
        return False
    for seg in _segments(tokens):
        words = _skip_env(seg)
        if len(words) < 3 or _base(words[0]) != "daisugi" or words[1:3] != ["hook", "record"]:
            continue
        if event is None:
            return True
        for i, w in enumerate(words):
            if (w == "--event" and words[i + 1 : i + 2] == [event]) or w == f"--event={event}":
                return True
    return False


@dataclass(frozen=True)
class ResolvedField:
    """One setting as the running code will see it, and where it came from."""

    key: str
    value: str
    source: str  # file | default | env | auto | global | project | project+global | info


def _read_raw(path: Path) -> dict:
    if not path.exists():
        return {}
    try:
        raw = yaml.safe_load(path.read_text(encoding="utf-8"))
    except yaml.YAMLError:
        return {}
    return raw if isinstance(raw, dict) else {}


def configured_backend(path: Path | None = None) -> str | None:
    """``llm_backend`` as the config file sets it, else None.

    None means auto-detect. The field defaults to None, so a file the product
    wrote without a choice reads as no choice, and a file that names a
    backend is honoured as written. A missing, unreadable, or malformed file
    pins nothing.
    """
    if path is None:
        from opendaisugi import DEFAULT_DATA_DIR

        path = DEFAULT_DATA_DIR / "config.yaml"
    try:
        value = load_config(path).llm_backend
    except (OSError, yaml.YAMLError, ValueError):
        return None
    return value.strip() if isinstance(value, str) and value.strip() else None


def _unknown_keys(raw: dict, model: type[BaseModel], *, prefix: str = "") -> list[str]:
    fields = model.model_fields
    out: list[str] = []
    for key, value in raw.items():
        dotted = f"{prefix}{key}"
        if key not in fields:
            out.append(dotted)
            continue
        annotation = fields[key].annotation
        if (
            isinstance(value, dict)
            and isinstance(annotation, type)
            and issubclass(annotation, BaseModel)
        ):
            out.extend(_unknown_keys(value, annotation, prefix=f"{dotted}."))
    return out


def unknown_config_keys(path: Path) -> list[str]:
    """Keys in the file that no Config field reads (load_config drops them
    silently), including inside a nested group like ``floor:``. A nested
    key is reported dotted, e.g. ``floor.backnd``, so a misspelling under
    a group is not silently dropped the same way a misspelled top-level
    key would be."""
    return sorted(_unknown_keys(_read_raw(path), Config))


def installed_hook_mode(settings_path: Path) -> str | None:
    """The ``--mode`` the installed Claude Code gate hook passes, or None if no hook.

    ``"unknown"`` when a gate hook is in a form this CLI does not read and no
    read hook enforces: it may enforce, so it is never reported as shadow.

    The hook command is the one thing the agent cannot rewrite, so this is the
    mode that actually governs verdicts on the Claude path (gate.resolve_gate_mode).
    """
    try:
        data = json.loads(settings_path.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return None
    first: str | None = None
    unknown = False
    for entry in (data.get("hooks") or {}).get("PreToolUse") or []:
        for hook in entry.get("hooks") or []:
            command = hook.get("command")
            if gate_hook_kind(command) == "unknown":
                unknown = True
            mode = gate_hook_mode(command)
            if mode and first is None:
                first = mode
    # A gate hook in a form not read may enforce: only a read enforce hook
    # outranks it, never a read shadow one.
    if first == "enforce":
        return first
    return "unknown" if unknown else first


def gate_hook_program(command: object) -> str | None:
    """The program of a gate hook in the ``daisugi gate check`` form (the
    form the Go and Rust installs write), or None for any other command."""
    if not isinstance(command, str):
        return None
    tokens = command_tokens(command)
    if tokens is None:
        return None
    for seg in _segments(tokens):
        words = _skip_env(seg)
        if len(words) >= 3 and _base(words[0]) == "daisugi" and words[1:3] == ["gate", "check"]:
            return words[0]
    return None


def missing_hook_programs(settings_path: Path) -> list[tuple[str, str | None]]:
    """(program, mode) for each gate hook in the settings file whose program
    is an absolute path that is not there, such as a binary a package
    manager removed on upgrade. That hook fails on every call. Anything the
    file holds that is not the usual shape is skipped, never raised."""
    try:
        data = json.loads(settings_path.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return []
    out: list[tuple[str, str | None]] = []
    hooks = data.get("hooks") if isinstance(data, dict) else None
    pre = hooks.get("PreToolUse") if isinstance(hooks, dict) else None
    for entry in pre if isinstance(pre, list) else []:
        hs = entry.get("hooks") if isinstance(entry, dict) else None
        for hook in hs if isinstance(hs, list) else []:
            command = hook.get("command") if isinstance(hook, dict) else None
            prog = gate_hook_program(command)
            if prog and prog.startswith("/") and not os.path.exists(prog):
                item = (prog, gate_hook_mode(command))
                if item not in out:
                    out.append(item)
    return out


def missing_hook_warning(settings_path: Path, prog: str, mode: str | None) -> str:
    """What `gate status` says about a gate hook whose program is gone."""
    if mode == "enforce":
        return (
            f"warning: the gate hook in {settings_path} runs {prog}, which does not exist, "
            "so every call is denied. Run: daisugi install --gate --enforce"
        )
    return (
        f"warning: the gate hook in {settings_path} runs {prog}, which does not exist, "
        "so the gate sees no call. Run: daisugi install --gate"
    )


# "unknown" is a gate hook in a form this CLI does not read: it may enforce.
_STRICTNESS = {"enforce": 3, "unknown": 2, "shadow": 1}


@dataclass(frozen=True)
class EffectiveHook:
    """The gate hook mode across the two files a hook can live in.

    ``daisugi install --gate`` writes a machine-global hook to
    ``~/.claude/settings.json`` (no ``--session``, so it resolves to the shared
    ``default`` envelope). ``daisugi start`` (plan 5) writes a hook scoped to
    ``<cwd>/.claude/settings.json``. Claude Code fires PreToolUse hooks from
    every settings source that has one — a call is allowed only if EVERY firing
    hook allows it — so the mode that actually governs verdicts, when both
    exist, is the STRICTER of the two (enforce beats shadow), never just
    whichever one a truth surface happened to read.
    """

    mode: str | None  # enforce | unknown | shadow | None (no hook anywhere)
    cwd_mode: str | None
    global_mode: str | None


def effective_hook_mode(*, home: Path, cwd: Path) -> EffectiveHook:
    """Read both hook files and report the mode that actually governs verdicts."""
    global_path = home / ".claude" / "settings.json"
    cwd_path = cwd / ".claude" / "settings.json"
    global_mode = installed_hook_mode(global_path)
    # Same file (cwd IS home, or an explicit alias in a test) — one hook, not two.
    cwd_mode = installed_hook_mode(cwd_path) if cwd_path != global_path else None

    candidates = [m for m in (cwd_mode, global_mode) if m]
    mode = max(candidates, key=lambda m: _STRICTNESS.get(m, 0)) if candidates else None
    return EffectiveHook(mode=mode, cwd_mode=cwd_mode, global_mode=global_mode)


def hook_source_label(eff: EffectiveHook) -> str:
    """The ONE vocabulary every truth surface (`config`, `gate status`, the TUI
    header) uses for where the effective hook mode came from: ``global`` (only
    ``~/.claude/settings.json``), ``project`` (only ``<cwd>/.claude/settings.json``
    — what `daisugi start` writes), ``project+global`` (both — the verdict is
    their intersection), or ``""`` when neither hook exists (the caller falls
    back to its own config/default source in that case).

    A single function, not three copies of the same if/elif chain, so the
    three surfaces can't drift the way `config` (``hook``) and the TUI header
    (``global``) already had before this was factored out.
    """
    if eff.cwd_mode and eff.global_mode:
        return "project+global"
    if eff.cwd_mode:
        return "project"
    if eff.global_mode:
        return "global"
    return ""


def resolved_config(
    path: Path | None = None,
    *,
    home: Path | None = None,
    cwd: Path | None = None,
    env: Mapping[str, str] | None = None,
) -> list[ResolvedField]:
    """Every Config field with its source, plus the two derived, load-bearing values.

    ``gate_mode (resolved)`` is the EFFECTIVE mode across both a machine-global
    hook (``~/.claude/settings.json``) and a directory-scoped one
    (``<cwd>/.claude/settings.json``, what ``daisugi start`` writes) — never
    just the global one, or the source would silently under-report an active
    ``daisugi start --enforce`` hook as ``default``. When only the
    directory-scoped hook exists, the source says so explicitly (``project``,
    not a bare ``global``-flavored label that reads as "the usual global one").
    When both exist, three extra rows spell out what each one is doing and how
    they combine — the honest fact that verdicts are the intersection.
    """
    from opendaisugi.llm import _auto_backend

    home = home or Path.home()
    if cwd is None:
        try:
            cwd = Path.cwd()
        except OSError:
            # The working directory was deleted out from under this process —
            # fall back to reporting the global-only view (== home, so the
            # cwd-hook check is a no-op) rather than crash `daisugi config`.
            cwd = home
    env = os.environ if env is None else env
    path = path or home / ".opendaisugi" / "config.yaml"
    cfg = load_config(path)
    raw = _read_raw(path)
    out: list[ResolvedField] = []
    for key in Config.model_fields:
        value = getattr(cfg, key)
        source = "file" if key in raw else "default"
        if isinstance(value, BaseModel):
            # A nested group prints as its leaves. Printing the model itself
            # would put a pydantic repr on a truth surface, and a repr is
            # not a setting.
            nested_raw = raw.get(key) or {}
            for sub_key in type(value).model_fields:
                out.append(
                    ResolvedField(
                        f"{key}.{sub_key}",
                        str(getattr(value, sub_key)),
                        "file" if sub_key in nested_raw else "default",
                    )
                )
            continue
        if key == "llm_backend" and value is None:
            value = "auto"
        out.append(ResolvedField(key, str(value), source))
    backend_env = env.get("OPENDAISUGI_LLM_BACKEND")
    backend_file = configured_backend(path)
    if backend_env:
        backend_source = "env"
    elif backend_file:
        backend_source = "file"
    else:
        backend_source = "auto"
    # The fallback reads only the env this call was given, never the process
    # env or the default data dir, so the value comes from the source the row names.
    out.append(
        ResolvedField(
            "llm_backend (resolved)",
            backend_env or backend_file or _auto_backend(env),
            backend_source,
        )
    )
    eff = effective_hook_mode(home=home, cwd=cwd)
    if eff.mode:
        out.append(ResolvedField("gate_mode (resolved)", eff.mode, hook_source_label(eff)))
        if eff.cwd_mode and eff.global_mode:
            out.append(ResolvedField("gate_mode (project)", eff.cwd_mode, "project"))
            out.append(ResolvedField("gate_mode (global)", eff.global_mode, "global"))
            out.append(
                ResolvedField(
                    "gate_mode (coexistence)",
                    "both fire; the verdict is the intersection — either one enforcing denies",
                    "info",
                )
            )
    else:
        out.append(
            ResolvedField(
                "gate_mode (resolved)", cfg.gate_mode, "file" if "gate_mode" in raw else "default"
            )
        )
    return out
