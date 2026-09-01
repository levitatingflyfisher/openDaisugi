"""The module map: what's wired, what could be swapped in, at every stage.

openDaisugi is a pipeline of swappable stages — the harness it plugs into, the
verifier that checks each action, the matcher that finds a reusable pathway, the
model backend, the token router. This renders that pipeline as an honest ASCII
wiring diagram: for each stage, which module is *active*, which are *available*
to swap in, and which are *possible* (a designed slot with no shipped
implementation yet). It is a status tool and a roadmap at once — a blank slot is
an invitation, not a bug.

Detection is best-effort and read-only. Nothing here changes wiring; it reports
it. ``daisugi modules`` prints this; ``--json`` emits the same as data.
"""

from __future__ import annotations

import importlib.util
import json
import os
import shutil
import stat
from collections.abc import Callable, Mapping
from dataclasses import asdict, dataclass, field
from pathlib import Path

# State of one candidate module within a stage.
ACTIVE = "active"  # wired and in use right now
AVAILABLE = "available"  # installed / present — could be swapped in today
POSSIBLE = "possible"  # a designed slot; no shipped implementation yet


@dataclass
class Module:
    name: str
    state: str
    note: str = ""


@dataclass
class Stage:
    key: str
    title: str
    role: str  # one line: what this stage does
    modules: list[Module] = field(default_factory=list)

    @property
    def swappable(self) -> bool:
        # A stage is a real swap-point when more than one module is actually
        # runnable today (active or available). One-impl stages are honest
        # about being single-impl — the slot exists, the alternatives don't yet.
        return sum(m.state in (ACTIVE, AVAILABLE) for m in self.modules) > 1


def _have(mod: str) -> bool:
    return importlib.util.find_spec(mod) is not None


def _int8_unsupported_reason() -> str | None:
    """Why int8 cannot run here, or None when it can. See ADR-0021.

    Two different causes get two different notes. A missing package needs
    an install. A CPU with no pinned build needs a different matcher, and
    an install would not help.
    """
    if not _have("onnxruntime"):
        return "needs opendaisugi[int8]"
    from opendaisugi._search import _detect_int8_variant
    from opendaisugi.exceptions import MatcherNotAvailable

    try:
        _detect_int8_variant()
    except MatcherNotAvailable:
        return "no int8 build for this CPU"
    return None


def _client_modules(gate_root: Path) -> list[Module]:
    """One module per compiled verifier client, honest about built and last dispatch.

    ACTIVE only when the binary is present AND the last dispatch through it
    succeeded. A binary that exists but has never answered is AVAILABLE, not
    ACTIVE. Presence is not proof it works. ``gate_root`` is where the gate
    records its dispatches, under the data dir this map describes.
    """
    from opendaisugi.bench.options import VERIFIER_CLIENTS, build_hint, client_is_built
    from opendaisugi.verifier_dispatch import last_dispatch

    record = last_dispatch(gate_root)
    labels = {
        "rust": "rust",
        "go": "go (mvdan)",
        "typescript": "typescript",
        "lean": "lean (proven core)",
    }
    out: list[Module] = []
    for key, label in labels.items():
        spec = VERIFIER_CLIENTS[key]
        if not client_is_built(spec):
            out.append(Module(label, POSSIBLE, build_hint(spec)))
        elif record.get("client") == key and record.get("ok"):
            out.append(Module(label, ACTIVE, "built; last dispatch succeeded"))
        else:
            out.append(Module(label, AVAILABLE, "built; no successful dispatch yet"))
    return out


def _claude_hook_installed(
    kind_substr: str, *, event: str = "PreToolUse", home: Path | None = None
) -> bool:
    """Is a hook containing ``kind_substr`` registered for Claude Code on
    ``event`` (default PreToolUse), under ``home`` (default ``Path.home()``)?
    """
    settings = (home or Path.home()) / ".claude" / "settings.json"
    try:
        data = json.loads(settings.read_text())
    except (OSError, ValueError):
        return False
    entries = data.get("hooks", {}).get(event, [])
    cmds = [h.get("command", "") for e in entries for h in e.get("hooks", [])]
    return any(kind_substr in c for c in cmds)


def _pi_extension_installed(home: Path) -> bool:
    """Is the daisugi-gate extension materialized where pi auto-discovers it?"""
    return (home / ".pi" / "agent" / "extensions" / "daisugi-gate" / "index.ts").exists()


def _pi_detected(home: Path) -> bool:
    """Has pi ever run on this box? Its home directory is the only trace it leaves."""
    return (home / ".pi").is_dir()


def _opencode_plugin_installed(home: Path, env: Mapping[str, str] | None = None) -> bool:
    """Is the daisugi gate plugin in OpenCode's global plugin directory?"""
    from opendaisugi.install import opencode_plugin_path

    return opencode_plugin_path(home, env).exists()


def _opencode_detected(
    home: Path, which: Callable[[str], str | None], env: Mapping[str, str] | None = None
) -> bool:
    """Is OpenCode on PATH, or does its config directory exist?"""
    from opendaisugi.install import opencode_plugin_path

    return which("opencode") is not None or opencode_plugin_path(home, env).parent.parent.is_dir()


def _coppice_socket_present(
    *, home: Path | None = None, env: Mapping[str, str] | None = None
) -> bool:
    """Does a coppice server socket answer, owned by this user?

    Checked with lstat so a symlink cannot lie. The path formula matches
    ``floor.coppice_backend.default_socket_path``: the runtime dir when
    ``XDG_RUNTIME_DIR`` is set, else ``home`` joined with
    ``.opendaisugi/coppice``. ``home`` and ``env`` exist so a test can point
    this at a fixture directory and a fixture environment instead of the
    real ``Path.home()`` and ``os.environ``, the same isolation
    ``default_socket_path(env=...)`` already gives its own caller.
    """
    env = os.environ if env is None else env
    runtime = env.get("XDG_RUNTIME_DIR")
    base = (
        Path(runtime) / "coppice" if runtime else (home or Path.home()) / ".opendaisugi" / "coppice"
    )
    try:
        st = os.lstat(base / "server.sock")
    except OSError:
        return False
    return stat.S_ISSOCK(st.st_mode) and st.st_uid == os.getuid()


def _switchyard_module(gateway_router: str, which: Callable[[str], str | None]) -> Module:
    """The NeMo Switchyard row of the router stage.

    ACTIVE when config selects it and the binary is on PATH. The gateway reads
    that choice when it starts, so the row points at `daisugi router status`
    for whether a child runs now. A selection without the binary is POSSIBLE
    and says so.
    """
    from opendaisugi.router_switchyard import INSTALL_CMD, locate_binary

    present = locate_binary(which=which) is not None
    selected = gateway_router == "switchyard"
    if selected and present:
        return Module(
            "NeMo Switchyard",
            ACTIVE,
            "selected; `daisugi gateway` starts it. The gateway row says if turns flow. "
            "See `daisugi router status`",
        )
    if selected:
        return Module(
            "NeMo Switchyard", POSSIBLE, f"selected, but the binary is missing: {INSTALL_CMD}"
        )
    if present:
        return Module(
            "NeMo Switchyard",
            AVAILABLE,
            "binary found: daisugi install --gateway --router switchyard --efficient-model <id>",
        )
    return Module("NeMo Switchyard", POSSIBLE, f"needs the binary: {INSTALL_CMD}")


def detect_stages(
    data_dir: Path,
    *,
    home: Path | None = None,
    which: Callable[[str], str | None] = shutil.which,
    env: Mapping[str, str] | None = None,
) -> list[Stage]:
    """Build the wiring snapshot for ``data_dir`` (read-only, best-effort).

    ``home`` (default ``Path.home()``) and ``which`` (default
    ``shutil.which``) exist so the floor stage's herdr detection is
    testable without touching the real ``~/.claude/settings.json`` or PATH.
    ``env`` (default ``os.environ``) isolates the coppice socket check from
    the real ``XDG_RUNTIME_DIR`` the same way.
    """
    from opendaisugi.onboarding import default_transcript_roots, gather_status

    status = gather_status(data_dir)
    roots = default_transcript_roots()
    present = {h for h, r in roots.items() if r.exists()}
    home_dir = home or Path.home()
    pi_installed = _pi_extension_installed(home_dir)
    pi_present = _pi_detected(home_dir)
    oc_installed = _opencode_plugin_installed(home_dir, env)
    oc_present = _opencode_detected(home_dir, which, env)

    def harness(name: str, hid: str) -> Module:
        return Module(
            name,
            ACTIVE if hid in present else POSSIBLE,
            "transcripts found" if hid in present else "not detected on this box",
        )

    env = os.environ if env is None else env
    backend = env.get("OPENDAISUGI_LLM_BACKEND", "").strip()
    gateway_on = bool(
        os.environ.get("OPENDAISUGI_GATEWAY_BASE_URL") or _claude_hook_installed("gateway")
    )

    # The pathway embedder (used by both matcher reuse and distill clustering) is
    # chosen by matcher_model. Report each backend honestly: ACTIVE when it is the
    # selection AND its package is present, AVAILABLE when installed but not chosen,
    # POSSIBLE when the package is missing or the backend is not built (ADR-0018).
    from opendaisugi._search import effective_matcher
    from opendaisugi.config import load_config

    matcher_cfg = load_config(data_dir / "config.yaml")
    matcher_sel = matcher_cfg.matcher_model
    switchyard_row = _switchyard_module(matcher_cfg.gateway_router, which)
    voice_sel = matcher_cfg.voice_engine
    # Both hooks are required for ACTIVE — one alone is a half-wired install,
    # not a working floor report (Fix round 1, Finding 1c).
    herdr_stop_on = _claude_hook_installed("--event stop", event="Stop", home=home)
    herdr_notif_on = _claude_hook_installed("--event notification", event="Notification", home=home)
    herdr_hooks_on = herdr_stop_on and herdr_notif_on
    herdr_hooks_partial = herdr_stop_on != herdr_notif_on
    herdr_binary_present = which("herdr") is not None
    # The floor_backend stage: which pane backend actually answers, not which
    # hook writes state. coppice_socket_answers reads the real socket, so it
    # is the only row here that starts ACTIVE the moment a server is up.
    coppice_socket_answers = _coppice_socket_present(home=home, env=env)
    coppice_on_path = which("coppice") is not None
    tmux_on_path = which("tmux") is not None
    if herdr_hooks_on:
        # Whole-branch review, minor 8: "hooks installed" alone overstated
        # it — the Herdr CLI contract these hooks shell out to
        # (_state_report.py's module docstring) is UNVERIFIED against a
        # real Herdr process on this box.
        herdr_state, herdr_note = ACTIVE, "hooks installed; Herdr CLI contract unverified"
    elif herdr_hooks_partial:
        herdr_state = AVAILABLE
        herdr_note = "one of two hooks installed; run `daisugi install --gate --report herdr`"
    elif herdr_binary_present:
        herdr_state, herdr_note = AVAILABLE, "run `daisugi install --gate --report herdr`"
    else:
        herdr_state, herdr_note = POSSIBLE, "install Herdr first"
    potion_installed = _have("model2vec")
    int8_reason = _int8_unsupported_reason()
    int8_installed = int8_reason is None
    # lexical has no package to be missing, so it is never POSSIBLE (ADR-0019):
    # ACTIVE when it is what actually runs (selected directly, or the
    # package-absence fallback landed here), AVAILABLE otherwise.
    lexical_effective = effective_matcher(matcher_cfg) == "lexical"

    def _emb_state(key: str, installed: bool) -> str:
        if matcher_sel == key and installed:
            return ACTIVE
        return AVAILABLE if installed else POSSIBLE

    def _lexical_state() -> str:
        return ACTIVE if lexical_effective else AVAILABLE

    def _lexical_desc() -> str:
        if not lexical_effective or matcher_sel == "lexical":
            return "keyword floor — no model, no download"
        # Active only because the configured backend's package is missing.
        return f"keyword floor — active: {matcher_sel}'s package is not installed"

    # One more backend line when `daisugi tiers setup --remote` recorded a
    # self-hosted model host. The four fixed modules never change. This
    # reads config and never guesses: an absent llm_base_url means no line.
    # The env var outranks the file, and the file outranks auto-detection, the
    # same rungs resolve_backend climbs. An `auto` row is ACTIVE when nothing
    # names a backend, and its line says what auto-detection picks here.
    from opendaisugi.llm import _auto_backend

    named = backend or (matcher_cfg.llm_backend or "").strip()
    auto_pick = _auto_backend(env, which=which)
    backend_modules = [
        Module(
            "auto",
            AVAILABLE if named else ACTIVE,
            f"pick what runs here: {auto_pick}" if not named else "pick what runs here",
        ),
        Module("claude-code", ACTIVE if named == "claude-code" else AVAILABLE, "no API key"),
        Module("anthropic-api", ACTIVE if named == "anthropic" else AVAILABLE, "BYOK"),
        Module(
            "llamafile / local", ACTIVE if named == "llamafile" else AVAILABLE, "base_url, offline"
        ),
        Module("ollama", ACTIVE if named == "ollama" else AVAILABLE, "if running"),
    ]
    if matcher_cfg.llm_base_url:
        from urllib.parse import urlparse

        netloc = urlparse(matcher_cfg.llm_base_url).netloc or matcher_cfg.llm_base_url
        model_part = matcher_cfg.llm_host_model or "model unset"
        ctx_part = (
            f"{matcher_cfg.llm_context_window // 1024}k"
            if matcher_cfg.llm_context_window
            else "context unknown"
        )
        backend_modules.append(
            Module(
                f"{netloc} ({matcher_cfg.llm_host_kind}, {model_part}, {ctx_part})",
                # AVAILABLE, not ACTIVE. A recorded host is not a harness
                # that routes through it right now. Nothing here checks
                # whether ANTHROPIC_BASE_URL or `daisugi gateway --upstream`
                # point at it. A claim of ACTIVE would be dressed-up control.
                AVAILABLE,
                "recorded. Export the env lines from `tiers setup --remote` to use it",
            )
        )

    faster_whisper_installed = _have("faster_whisper")
    sherpa_onnx_installed = _have("sherpa_onnx")

    def _voice_state(key: str, installed: bool) -> str:
        if voice_sel == key and installed:
            return ACTIVE
        return AVAILABLE if installed else POSSIBLE

    return [
        Stage(
            "harness",
            "harness (agent host)",
            "the agent whose tool-calls we sit in front of",
            [
                harness("claude-code", "claude-code"),
                harness("codex", "codex"),
                Module(
                    "pi",
                    ACTIVE if pi_installed else (AVAILABLE if pi_present else POSSIBLE),
                    "gate extension installed"
                    if pi_installed
                    else (
                        "pi detected. Run `daisugi install --harness pi`"
                        if pi_present
                        else "not detected on this box"
                    ),
                ),
                Module(
                    "opencode",
                    ACTIVE if oc_installed else (AVAILABLE if oc_present else POSSIBLE),
                    "gate plugin installed: in-process deny-only hook; fail-closed"
                    if oc_installed
                    else (
                        "opencode detected. Run `daisugi install --harness opencode`"
                        if oc_present
                        else "not detected on this box"
                    ),
                ),
                Module("hermes", POSSIBLE, "adapter designed, not built"),
                Module("openclaw", POSSIBLE, "adapter designed, not built"),
                Module("<any via AGENTS.md>", POSSIBLE, "vendor-neutral hook contract"),
            ],
        ),
        Stage(
            "gate",
            "gate (call-time)",
            "checks each proposed action BEFORE it runs; fail-closed",
            [
                Module(
                    "claude PreToolUse",
                    ACTIVE if _claude_hook_installed("daisugi hook") else POSSIBLE,
                    "installed"
                    if _claude_hook_installed("daisugi hook")
                    else "run `daisugi install`",
                ),
                Module("codex hooks.json", AVAILABLE, "fail-open class — soft gate"),
                Module(
                    "pi tool_call (in-process)",
                    ACTIVE if pi_installed else POSSIBLE,
                    "native block, no exit-2 convention, no fail-open outer timeout"
                    if pi_installed
                    else "run `daisugi install --harness pi`",
                ),
                Module(
                    "opencode tool.execute.before, in-process",
                    ACTIVE if oc_installed else POSSIBLE,
                    "in-process deny-only hook; fail-closed on every call. apply_patch is "
                    "checked path by path. Writes to OpenCode's config, to .opencode plugin and tool "
                    "directories and to any opencode.json are hard-denied. Every ask is permanent: the gate has no fixed "
                    "project root for OpenCode. A coppice pane will not start without the "
                    "plugin or with --pure. Outside coppice, OpenCode runs ungated if the "
                    "plugin fails to load or with --pure. Plugins under OPENCODE_CONFIG_DIR "
                    "are not guarded"
                    if oc_installed
                    else "run `daisugi install --harness opencode`",
                ),
                Module("shadow / off", AVAILABLE, "config: gate mode"),
            ],
        ),
        Stage(
            "verifier",
            "verifier (the checker)",
            "proves the action stays inside the envelope (SMT-backed)",
            [
                Module("python (in-process)", ACTIVE, "the oracle; always runs"),
                *_client_modules(data_dir / "gate"),
            ],
        ),
        Stage(
            "shell",
            "shell decomposition",
            "splits `a && b` into heads so each is checked (ADR-0010/14)",
            [
                Module(
                    "tree-sitter-bash",
                    ACTIVE if status.shell_grammar_installed else POSSIBLE,
                    "installed"
                    if status.shell_grammar_installed
                    else "uv add 'opendaisugi[shell]'",
                ),
                Module("reject-compound", AVAILABLE, "the fail-closed default when off"),
            ],
        ),
        Stage(
            "envelope",
            "envelope source",
            "where the 'what is allowed' spec comes from",
            [
                Module("evidence-inferred", ACTIVE, "ADR-0016 — from observed steps, zero-LLM"),
                Module(
                    "llm-generated",
                    AVAILABLE if backend else POSSIBLE,
                    f"backend={backend}" if backend else "set OPENDAISUGI_LLM_BACKEND",
                ),
            ],
        ),
        Stage(
            "backend",
            "model backend",
            "the LLM for envelope generation / planning (Tier-1)",
            backend_modules,
        ),
        Stage(
            "matcher",
            "matcher / embedder (pathway reuse)",
            "finds a stored pathway to reuse instead of re-planning",
            [
                Module(
                    "all-MiniLM-L6-v2",
                    _emb_state("all-MiniLM-L6-v2", status.search_extra_installed),
                    "torch, ~90MB"
                    if status.search_extra_installed
                    else "needs opendaisugi[search]",
                ),
                Module(
                    "potion static",
                    _emb_state("potion", potion_installed),
                    "torch-free, numpy-only, ~30MB, offline"
                    if potion_installed
                    else "needs opendaisugi[potion]",
                ),
                Module(
                    "int8 / fp16 onnx",
                    _emb_state("int8", int8_installed),
                    "onnx, ~23MB, no torch" if int8_installed else int8_reason,
                ),
                Module("lexical / intent", _lexical_state(), _lexical_desc()),
            ],
        ),
        Stage(
            "router",
            "token router / gateway",
            "routes turns to cheap vs frontier models to save tokens",
            [
                Module(
                    "daisugi gateway",
                    ACTIVE if gateway_on else POSSIBLE,
                    "base_url wired" if gateway_on else "daisugi install --gateway",
                ),
                switchyard_row,
                Module("off (direct)", AVAILABLE, "default; start routing with `daisugi gateway`"),
            ],
        ),
        Stage(
            "distill",
            "distillation (the gardener)",
            "clusters verified traces into reusable pathways (`tend`)",
            [
                Module(
                    "sentence-transformers",
                    _emb_state("all-MiniLM-L6-v2", status.search_extra_installed),
                    "torch clustering embedder"
                    if status.search_extra_installed
                    else "needs [search]",
                ),
                Module(
                    "potion (torch-free)",
                    _emb_state("potion", potion_installed),
                    "numpy-only clustering" if potion_installed else "needs [potion]",
                ),
                Module(
                    "int8 (onnx, no torch)",
                    _emb_state("int8", int8_installed),
                    "onnx clustering" if int8_installed else int8_reason,
                ),
                Module("lexical (no model)", _lexical_state(), _lexical_desc()),
                Module("no-embedder", AVAILABLE, "journal only, 0 pathways (graceful)"),
            ],
        ),
        Stage(
            "stores",
            "stores (local-first)",
            "where trust and pathways live — on your disk",
            [
                Module("journal (sqlite)", ACTIVE, f"{status.journal_total} traces"),
                Module("pathway store (sqlite)", ACTIVE, f"{status.pathway_count} pathways"),
                Module("git-backed store", AVAILABLE, "shareable pathway registry"),
            ],
        ),
        Stage(
            "floor_report",
            "floor report (state → a pane host)",
            "tells the pane host: idle, working, blocked, done",
            [
                Module("herdr", herdr_state, herdr_note),
                Module(
                    "coppice",
                    # POSSIBLE regardless of the recorded preference. This
                    # stage reports which hook writes state; it never checks
                    # a socket. The floor_backend stage checks the socket
                    # and is the one that says whether coppice is built or
                    # installed; this stage does not repeat that claim.
                    POSSIBLE,
                    "the report path is not wired yet",
                ),
                Module(
                    "none",
                    # ACTIVE purely on the absence of herdr hooks — the
                    # coppice preference no longer flips this (Finding 2):
                    # recording an intent for an unbuilt server does not
                    # make anything actually listen.
                    ACTIVE if not herdr_hooks_on else AVAILABLE,
                    "no floor listening",
                ),
            ],
        ),
        Stage(
            "floor_backend",
            "pane backend",
            "drives the harness inside a real pane: coppice, herdr, or tmux",
            [
                Module(
                    "coppice",
                    ACTIVE
                    if coppice_socket_answers
                    else (AVAILABLE if coppice_on_path else POSSIBLE),
                    "server running"
                    if coppice_socket_answers
                    else (
                        "built, run `coppice server start`"
                        if coppice_on_path
                        else "build it in harness/coppice"
                    ),
                ),
                Module(
                    "herdr",
                    AVAILABLE if herdr_binary_present else POSSIBLE,
                    "installed" if herdr_binary_present else "install herdr from herdr.dev",
                ),
                Module(
                    "tmux",
                    AVAILABLE if tmux_on_path else POSSIBLE,
                    "installed" if tmux_on_path else "install tmux 3.2 or newer",
                ),
            ],
        ),
        Stage(
            "voice_engine",
            "voice engine (speech to text)",
            "turns a recorded clip into text for the voice bridge",
            [
                Module(
                    "faster-whisper",
                    _voice_state("faster-whisper", faster_whisper_installed),
                    "CPU int8 by default"
                    if faster_whisper_installed
                    else "needs opendaisugi[voice]",
                ),
                Module(
                    "parakeet",
                    AVAILABLE if sherpa_onnx_installed else POSSIBLE,
                    "needs a downloaded model directory"
                    if sherpa_onnx_installed
                    else "needs opendaisugi[voice-parakeet]",
                ),
            ],
        ),
    ]


_GLYPH = {ACTIVE: "●", AVAILABLE: "○", POSSIBLE: "·"}
_STATE_KEY = {ACTIVE: "on", AVAILABLE: "avail", POSSIBLE: "off"}


def render_wiring(
    data_dir: Path,
    *,
    width: int = 66,
    plain: bool | None = None,
    home: Path | None = None,
    which: Callable[[str], str | None] = shutil.which,
) -> str:
    """Render the wiring snapshot as an ASCII pipeline.

    ``home`` and ``which`` pass straight through to detect_stages(), so the
    floor stage's herdr detection is testable here too, without touching
    the real ``~/.claude/settings.json`` or the real PATH. detect_stages()
    still reads other real host state on its own, such as harness
    transcript roots, that neither parameter reaches.
    """
    from opendaisugi import console

    g = console.glyphs() if plain is None else (console.ASCII_BOX if plain else console.BOX)
    stages = detect_stages(data_dir, home=home, which=which)
    out: list[str] = []
    out.append(f"openDaisugi — module wiring   (data dir: {data_dir})")
    out.append("")
    out.append("  a task from your agent")
    inner = width - 4
    for st in stages:
        out.append(f"        {g['v']}")
        out.append(f"        {g['down']}")
        # Tag by how a choice here takes effect (see opendaisugi.swap):
        # [live] = the running code reads it now; [cfg] = needs a restart/
        # reinstall; [planned] = you can record it but nothing reads it yet.
        from opendaisugi.swap import effect_of

        eff = effect_of(st.key)
        tag = f" [{eff}] " if eff else ""
        head = f"{g['tl']}{g['h']} {st.title} "
        head = head + g["h"] * max(0, width - len(head) - len(tag) - 1) + tag + g["tr"]
        out.append(head)
        out.append(f"{g['v']} {st.role[:inner].ljust(inner)} {g['v']}")
        # modules: active first, wrapped
        line = "  "
        for m in st.modules:
            chunk = f"{g[_STATE_KEY[m.state]]} {m.name}   "
            if len(line) + len(chunk) > inner:
                out.append(f"{g['v']} {line[:inner].ljust(inner)} {g['v']}")
                line = "  "
            line += chunk
        if line.strip():
            out.append(f"{g['v']} {line[:inner].ljust(inner)} {g['v']}")
        out.append(g["bl"] + g["h"] * (width - 2) + g["br"])
    out.append(f"        {g['v']}")
    out.append(f"        {g['down']}")
    out.append("  verified action runs  (or falls back / is refused)")
    out.append("")
    from opendaisugi.swap import effect_of

    out.append(
        f"legend:  {g['on']} active   {g['avail']} available (swap in)   "
        f"{g['off']} possible / planned"
    )
    out.append("         [live]    = takes effect now")
    out.append("         [cfg]     = a real choice, but needs a restart/reinstall")
    out.append("         [planned] = you can record it, but nothing reads it yet")
    n_live = sum(effect_of(s.key) == "live" for s in stages)
    n_cfg = sum(effect_of(s.key) == "cfg" for s in stages)
    n_planned = sum(effect_of(s.key) == "planned" for s in stages)
    out.append(
        f"         {n_live} live · {n_cfg} need a restart/reinstall · "
        f"{n_planned} planned (not wired yet)."
    )
    return "\n".join(out)


def wiring_json(
    data_dir: Path,
    *,
    home: Path | None = None,
    which: Callable[[str], str | None] = shutil.which,
) -> str:
    """Render the wiring snapshot as JSON.

    ``home`` and ``which`` pass straight through to detect_stages(), the
    same isolation render_wiring() already offers.
    """
    stages = detect_stages(data_dir, home=home, which=which)
    return json.dumps([asdict(s) for s in stages], indent=2)
