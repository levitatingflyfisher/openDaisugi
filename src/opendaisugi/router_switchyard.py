"""NVIDIA NeMo Switchyard as the gateway's external model chooser.

Switchyard picks which model serves each turn. The gateway stays the meter.
This module finds the ``switchyard-server`` binary, renders its TOML
deployment file, starts and stops it as a managed child, and probes its
health.

Facts pinned from NVIDIA-NeMo/Switchyard tag v0.3.0, commit 336196f, read on
2026-09-23: README.md, docs/cli_reference.md, docs/reference/toml_schema.md,
and the source of crates/switchyard-server and crates/switchyard-translation.

- The proxy is the Rust binary ``switchyard-server``. ``pip install
  nemo-switchyard`` installs the embeddable Python library, a different
  thing. The install command below builds the server from NVIDIA's
  repository at the pinned tag.
- Liveness is ``GET /health``. ``switchyard-server --version`` prints the
  version. ``--dry-run`` checks a config and binds no socket.
- The config is TOML with ``schema_version = 1`` and three tables:
  ``[llm_clients.*]`` with ``format``, ``base_url``, and ``api_key_env`` or
  ``forward_auth`` but never both; ``[targets.*]`` with ``id``, the exact
  upstream model id, and ``llm_client``; ``[routes.*]`` with ``id``,
  ``type``, and the keys of that type. A client sends the route ``id`` as the
  request ``model``.
- ``stage_router`` needs ``capable_target``, ``efficient_target``, ``picker``
  and ``confidence_threshold``. There is no ``escalation`` route type. An
  escalation policy is ``type = "llm_classifier"`` with ``mode =
  "escalation"``.
- Every routed response names the target that served it. The server writes
  the target ``id`` into the body ``model``, in a buffered body and in the
  streamed ``message_start`` event. It also sets the response header
  ``x-model-router-selected-model`` to the same id, and it never passes an
  upstream copy of that header through.
- ``api_key_env`` names a variable that must be set and not empty when the
  server loads. An Anthropic ``forward_auth`` client forwards the caller's
  ``authorization`` or ``x-api-key``. Of the ``anthropic-beta`` values it
  keeps only the ``oauth-*`` ones.
- ``/v1/messages/count_tokens`` also resolves the route from ``model``.

The server's own ``--host`` default is ``0.0.0.0``. ``DEFAULT_HOST`` here is
daisugi's choice, not Switchyard's: the managed child binds loopback only.
"""

from __future__ import annotations

import contextlib
import ctypes
import json
import os
import shutil
import signal
import subprocess
import sys
import time
import tomllib
import urllib.error
import urllib.request
from collections.abc import Callable
from dataclasses import dataclass
from pathlib import Path
from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from opendaisugi.config import Config

BINARY_NAME = "switchyard-server"
PINNED_TAG = "v0.3.0"
INSTALL_CMD = (
    "cargo install --locked --git https://github.com/NVIDIA-NeMo/Switchyard "
    f"--tag {PINNED_TAG} switchyard-server"
)
HEALTH_PATH = "/health"
SELECTED_MODEL_HEADER = "x-model-router-selected-model"
DEFAULT_HOST = "127.0.0.1"
DEFAULT_PORT = 4000

Which = Callable[[str], "str | None"]


def locate_binary(*, which: Which | None = None) -> str | None:
    """The absolute path of ``switchyard-server`` on PATH, or None."""
    return (which or shutil.which)(BINARY_NAME)


def check_prerequisite(*, which: Which | None = None) -> str | None:
    """None when the binary is on PATH. Otherwise a message that names the install."""
    if locate_binary(which=which):
        return None
    return (
        f"{BINARY_NAME} is not on PATH. The Switchyard proxy is a Rust binary, not a pip "
        f"package. It needs a Rust toolchain: https://rust-lang.org/tools/install. "
        f"Install it with: {INSTALL_CMD}"
    )


def binary_version(
    *,
    which: Which | None = None,
    run: Callable[..., subprocess.CompletedProcess] = subprocess.run,
) -> str | None:
    """The output of ``switchyard-server --version``, or None when it will not run."""
    binary = locate_binary(which=which)
    if binary is None:
        return None
    try:
        result = run([binary, "--version"], capture_output=True, text=True, timeout=5.0)
    except (OSError, subprocess.SubprocessError):
        return None
    return (result.stdout or "").strip() or None


PICKERS = ("efficient_first", "capable_first")


@dataclass(frozen=True)
class SwitchyardTargets:
    """The two tiers the stage router chooses between.

    The gateway meters a turn by the target id that Switchyard names in the
    response, so ``capable_id`` and ``efficient_id`` must differ. The capable
    tier is a cloud client. The efficient tier is a local host when
    ``efficient_local`` is true, and a cloud client when it is false.

    A cloud client forwards the caller's own login, as the gateway does
    without Switchyard. It uses a key only when ``api_key_env`` names the
    variable, and then every cloud client uses that key. A local client gets
    no credential at all, so the caller's login never reaches a local host.
    """

    capable_id: str
    efficient_id: str
    efficient_client_format: str
    efficient_base_url: str
    capable_client_format: str = "anthropic_messages"
    capable_base_url: str = "https://api.anthropic.com"
    efficient_local: bool = True
    api_key_env: str | None = None


def _env_is_set(name: str) -> bool:
    return bool(os.environ.get(name))


def _toml_str(value: str) -> str:
    # A JSON string is a valid TOML basic string: the same quotes and escapes.
    return json.dumps(value)


FORWARDS_LOGIN = "forwards your own login, as the gateway does"
NO_CREDENTIAL = "no credential: a local host"


def _key_mode(api_key_env: str) -> str:
    return f"uses {api_key_env}: billed per token to that key"


def _client_lines(name: str, fmt: str, base_url: str, auth: str | None) -> list[str]:
    """One client table. ``auth`` is None for no credential, "forward", or a variable name."""
    lines = [f"[llm_clients.{name}]", f"format = {_toml_str(fmt)}"]
    lines.append(f"base_url = {_toml_str(base_url)}")
    if auth == "forward":
        lines.append("forward_auth = true")
    elif auth is not None:
        lines.append(f"api_key_env = {_toml_str(auth)}")
    return lines + [""]


def _cloud_auth(targets: SwitchyardTargets) -> str:
    return targets.api_key_env or "forward"


def client_auth_modes(targets: SwitchyardTargets) -> dict[str, str]:
    """One plain line per client that says how it authenticates upstream."""
    cloud = _key_mode(targets.api_key_env) if targets.api_key_env else FORWARDS_LOGIN
    return {"capable": cloud, "efficient": NO_CREDENTIAL if targets.efficient_local else cloud}


def render_switchyard_toml(
    targets: SwitchyardTargets,
    *,
    route_id: str = "daisugi",
    picker: str = "efficient_first",
    confidence_threshold: float = 0.5,
    api_key_present: Callable[[str], bool] = _env_is_set,
) -> str:
    """Render a schema_version 1 deployment with one two-tier stage_router route.

    A cloud client uses ``forward_auth = true`` unless ``api_key_env`` names
    a variable. A named variable that is not set now raises ValueError,
    because the server refuses to load with it empty, and a silent switch to
    another way of paying is not what the operator chose. Values the schema
    rejects raise ValueError here too, before a file is written.
    """
    if not route_id:
        raise ValueError("the route id must not be empty")
    if picker not in PICKERS:
        raise ValueError(f"picker must be one of {', '.join(PICKERS)}, not {picker!r}")
    if not 0.0 <= confidence_threshold <= 1.0:
        raise ValueError(f"confidence_threshold must be in [0, 1], not {confidence_threshold}")
    if targets.capable_id == targets.efficient_id:
        raise ValueError(
            f"the capable and efficient tiers name the same model {targets.capable_id!r}. "
            "Pick a different efficient model."
        )
    if targets.api_key_env and not api_key_present(targets.api_key_env):
        raise ValueError(
            f"switchyard_api_key_env names {targets.api_key_env}, which is not set. "
            "Export it before the gateway starts, or clear the field to forward your login."
        )
    cloud = _cloud_auth(targets)

    lines = ["schema_version = 1", ""]
    lines += _client_lines(
        "capable", targets.capable_client_format, targets.capable_base_url, cloud
    )
    lines += _client_lines(
        "efficient",
        targets.efficient_client_format,
        targets.efficient_base_url,
        None if targets.efficient_local else cloud,
    )
    lines += ["[targets.capable]", f"id = {_toml_str(targets.capable_id)}"]
    lines += ['llm_client = "capable"', ""]
    lines += ["[targets.efficient]", f"id = {_toml_str(targets.efficient_id)}"]
    lines += ['llm_client = "efficient"', ""]
    lines += ["[routes.daisugi]", f"id = {_toml_str(route_id)}", 'type = "stage_router"']
    lines += ['capable_target = "capable"', 'efficient_target = "efficient"']
    lines += [f"picker = {_toml_str(picker)}", f"confidence_threshold = {confidence_threshold}"]
    return "\n".join(lines) + "\n"


def write_switchyard_config(
    data_dir: Path, toml_text: str, *, name: str = "switchyard.toml"
) -> Path:
    """Write ``<data_dir>/<name>`` with mode 0600 and return its path."""
    path = Path(data_dir) / name
    path.parent.mkdir(parents=True, exist_ok=True)
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w", encoding="utf-8") as fh:
        fh.write(toml_text)
    os.chmod(path, 0o600)
    return path


class SwitchyardConfigError(ValueError):
    """A Switchyard TOML file that the gateway cannot meter honestly."""


_ANTHROPIC_API = "https://api.anthropic.com"
_DEFAULT_LOCAL_BASE_URL = "http://127.0.0.1:11434"


def _openai_root(base_url: str) -> str:
    base = base_url.rstrip("/")
    return base if base.endswith("/v1") else f"{base}/v1"


def targets_from_config(cfg: Config) -> SwitchyardTargets | None:
    """The two tiers that config names, or None when no efficient model is set.

    A ``claude-*`` efficient model is a cloud client on the Anthropic API.
    Any other id is a local model on the host that ``daisugi tiers setup
    --remote`` recorded, or on Ollama at loopback when no host is recorded.
    A recorded host that speaks the Anthropic wire gets an
    ``anthropic_messages`` client. Every other host gets ``openai_chat`` at
    its ``/v1`` root, because the Switchyard client only appends
    ``/chat/completions``.
    """
    efficient = cfg.switchyard_efficient_model
    if not efficient:
        return None
    if efficient.startswith("claude-"):
        return SwitchyardTargets(
            capable_id=cfg.switchyard_capable_model,
            efficient_id=efficient,
            efficient_client_format="anthropic_messages",
            efficient_base_url=_ANTHROPIC_API,
            efficient_local=False,
            api_key_env=cfg.switchyard_api_key_env,
        )
    base = cfg.llm_base_url or _DEFAULT_LOCAL_BASE_URL
    kind = cfg.llm_host_kind if cfg.llm_base_url else "ollama"
    if kind == "anthropic":
        fmt, url = "anthropic_messages", base.rstrip("/")
    else:
        fmt, url = "openai_chat", _openai_root(base)
    return SwitchyardTargets(
        capable_id=cfg.switchyard_capable_model,
        efficient_id=efficient,
        efficient_client_format=fmt,
        efficient_base_url=url,
        api_key_env=cfg.switchyard_api_key_env,
    )


# Route types whose tiers the gateway can meter, and the keys that name them.
_TIER_KEYS = (
    ("capable_target", "efficient_target"),
    ("strong_target", "weak_target"),
)


def route_targets_from_toml(path: Path, route_id: str) -> tuple[str, str]:
    """The (capable id, efficient id) pair of the route ``route_id`` in ``path``.

    The gateway meters a turn by these ids, so it reads them from the file
    that the server loads, never from a second copy. A file it cannot read,
    a route it cannot find, or a route without two tiers raises
    SwitchyardConfigError. The gateway then refuses to start.
    """
    try:
        doc = tomllib.loads(Path(path).read_text(encoding="utf-8"))
    except (OSError, UnicodeDecodeError, tomllib.TOMLDecodeError) as exc:
        raise SwitchyardConfigError(f"cannot read {path}: {exc}") from exc
    routes = doc.get("routes")
    targets = doc.get("targets")
    if not isinstance(routes, dict) or not isinstance(targets, dict):
        raise SwitchyardConfigError(f"{path} has no route with id {route_id!r}")
    route = next(
        (r for r in routes.values() if isinstance(r, dict) and r.get("id") == route_id), None
    )
    if route is None:
        raise SwitchyardConfigError(f"{path} has no route with id {route_id!r}")
    for high_key, low_key in _TIER_KEYS:
        if high_key in route and low_key in route:
            break
    else:
        raise SwitchyardConfigError(
            f"route {route_id!r} in {path} does not name two tiers. The gateway meters "
            "capable_target and efficient_target, or strong_target and weak_target."
        )
    ids = []
    for key in (high_key, low_key):
        name = route[key]
        target = targets.get(name) if isinstance(name, str) else None
        if not isinstance(target, dict) or not isinstance(target.get("id"), str):
            raise SwitchyardConfigError(
                f"route {route_id!r} in {path} names target {name!r}, which is missing"
            )
        ids.append(target["id"])
    if ids[0] == ids[1]:
        raise SwitchyardConfigError(
            f"route {route_id!r} in {path} uses the model {ids[0]!r} for both tiers"
        )
    return ids[0], ids[1]


def client_auth_from_toml(path: Path, route_id: str) -> dict[str, str] | None:
    """How each tier of the route in ``path`` authenticates, read from that file.

    This is the file the running child loaded, so it says who pays now,
    whatever the environment of the caller holds. None when the file or the
    route cannot be read.
    """
    try:
        doc = tomllib.loads(Path(path).read_text(encoding="utf-8"))
        route = next(r for r in doc["routes"].values() if r.get("id") == route_id)
        clients = doc.get("llm_clients", {})
        keys = next(pair for pair in _TIER_KEYS if pair[0] in route and pair[1] in route)
        modes = {}
        for tier, key in zip(("capable", "efficient"), keys, strict=True):
            client = clients.get(doc["targets"][route[key]]["llm_client"], {})
            if client.get("api_key_env"):
                modes[tier] = _key_mode(str(client["api_key_env"]))
            elif client.get("forward_auth") is True:
                modes[tier] = FORWARDS_LOGIN
            else:
                modes[tier] = "no credential"
        return modes
    except (OSError, UnicodeDecodeError, tomllib.TOMLDecodeError, StopIteration):
        return None
    except (KeyError, TypeError, AttributeError):
        return None


# --- the managed child ---------------------------------------------------------

# Popen objects of the children this process started, by pid, so a liveness
# check can reap an exited child instead of seeing a zombie as alive.
_CHILDREN: dict[int, subprocess.Popen] = {}


@dataclass(frozen=True)
class SwitchyardHandle:
    """A switchyard-server child that answered its health check."""

    pid: int
    host: str
    port: int
    config_path: Path
    argv: list[str]
    log_path: Path | None = None

    @property
    def base_url(self) -> str:
        return f"http://{self.host}:{self.port}"


_PR_SET_PDEATHSIG = 1


def _parent_death_signal() -> Callable[[], None] | None:
    """A preexec hook that makes Linux send the child SIGTERM when this process dies.

    A gateway killed with SIGKILL runs no cleanup, so without this its child
    would keep serving loopback. The hook runs in the forked child before
    exec. If the parent already died before the hook ran, the child stops
    itself. Returns None where prctl is not available.
    """
    if not sys.platform.startswith("linux"):
        return None
    try:
        libc = ctypes.CDLL(None, use_errno=True)
        prctl = libc.prctl
    except (OSError, AttributeError):
        return None
    parent = os.getpid()

    def preexec() -> None:
        prctl(_PR_SET_PDEATHSIG, int(signal.SIGTERM), 0, 0, 0)
        if os.getppid() != parent:
            os.kill(os.getpid(), signal.SIGTERM)

    return preexec


def _default_spawn(argv: list[str], log_path: Path | None) -> int:
    """Start the child in its own session. Its output goes to ``log_path``.

    On Linux the child gets SIGTERM from the kernel when this process dies.
    """
    if log_path is not None:
        log_path.parent.mkdir(parents=True, exist_ok=True)
        out = open(log_path, "ab")  # noqa: SIM115 - the child owns the handle
    else:
        out = subprocess.DEVNULL
    try:
        proc = subprocess.Popen(
            argv,
            stdin=subprocess.DEVNULL,
            stdout=out,
            stderr=out,
            start_new_session=True,
            preexec_fn=_parent_death_signal(),
        )
    finally:
        if out is not subprocess.DEVNULL:
            out.close()
    _CHILDREN[proc.pid] = proc
    return proc.pid


def _default_alive(pid: int) -> bool:
    proc = _CHILDREN.get(pid)
    if proc is not None:
        return proc.poll() is None
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    except PermissionError:
        return True
    return True


def probe_health(host: str, port: int, *, timeout_s: float = 0.5) -> bool:
    """True when ``GET /health`` on host:port answers 200."""
    try:
        with urllib.request.urlopen(
            f"http://{host}:{port}{HEALTH_PATH}", timeout=timeout_s
        ) as resp:
            return resp.status == 200
    except (urllib.error.URLError, OSError, ValueError):
        return False


def _log_tail(log_path: Path | None, lines: int = 5) -> str:
    if log_path is None:
        return ""
    try:
        text = log_path.read_text(encoding="utf-8", errors="replace")
    except OSError:
        return ""
    tail = [line for line in text.splitlines() if line.strip()][-lines:]
    return "\n".join(f"    {line}" for line in tail)


def child_files_for(data_dir: Path, port: int) -> dict[str, Path]:
    """The per-port files of one child: its config copy, its output log, and its routing log.

    Each child loads its own copy, so a later install or another gateway that
    rewrites the shared switchyard.toml cannot change what this child runs.
    """
    gw = Path(data_dir) / "gateway"
    return {
        "config": gw / f"switchyard-{port}.toml",
        "log": gw / f"switchyard-{port}.log",
        "routing_log": gw / f"switchyard-routing-{port}.jsonl",
    }


def state_path_for(data_dir: Path, port: int) -> Path:
    """The state file of the child on ``port``. One file per port, so two gateways never share one."""
    return Path(data_dir) / "gateway" / f"switchyard-{port}.json"


def read_state(state_path: Path) -> dict | None:
    """The state a start wrote: pid, host, port, config path, log path.

    None when the file is absent, or when pid, host or port has the wrong type.
    """
    try:
        data = json.loads(Path(state_path).read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return None
    if not isinstance(data, dict):
        return None
    pid, host, port = data.get("pid"), data.get("host"), data.get("port")
    if type(pid) is not int or type(port) is not int or not isinstance(host, str):
        return None
    return data


def list_states(data_dir: Path) -> list[tuple[Path, dict | None]]:
    """Every child state file under ``data_dir``, sorted, each with its state or None."""
    paths = sorted((Path(data_dir) / "gateway").glob("switchyard-*.json"))
    return [(path, read_state(path)) for path in paths]


def _write_state(
    state_path: Path,
    handle: SwitchyardHandle,
    route_id: str | None,
    auth: dict[str, str] | None,
) -> None:
    state_path.parent.mkdir(parents=True, exist_ok=True)
    payload = {
        "pid": handle.pid,
        "host": handle.host,
        "port": handle.port,
        "config_path": str(handle.config_path),
        "log_path": str(handle.log_path) if handle.log_path else None,
        "route_id": route_id,
        "auth": auth,
    }
    state_path.write_text(json.dumps(payload), encoding="utf-8")


def start_switchyard(
    config_path: Path,
    *,
    host: str = DEFAULT_HOST,
    port: int = DEFAULT_PORT,
    binary: str | None = None,
    routing_log_path: Path | None = None,
    log_path: Path | None = None,
    state_path: Path | None = None,
    route_id: str | None = None,
    auth: dict[str, str] | None = None,
    spawn: Callable[[list[str], Path | None], int] = _default_spawn,
    health_probe: Callable[[str, int], bool] = probe_health,
    alive: Callable[[int], bool] = _default_alive,
    kill: Callable[[int, int], None] = os.kill,
    wait_s: float = 10.0,
    poll_interval_s: float = 0.05,
    kill_grace_s: float = 2.0,
) -> tuple[SwitchyardHandle | None, str]:
    """Start switchyard-server as a managed child and wait for ``GET /health``.

    Returns (handle, message). The handle is None when the start fails, and
    the message then says why and names the next command. The child binds
    127.0.0.1 only. A port that already answers is refused, so a server this
    call did not start is never taken for the child. A child that exits, or
    does not answer in ``wait_s``, gets no handle. A child that is still
    running at the timeout gets SIGTERM, and SIGKILL when it is still alive
    after ``kill_grace_s``, because no state file names it. The state file is
    written only after the health check passes.
    """
    if host != DEFAULT_HOST:
        return None, (
            f"the managed switchyard-server binds {DEFAULT_HOST} only, not {host}. "
            "The gateway in front of it is the one door."
        )
    resolved = binary or BINARY_NAME
    if health_probe(host, port):
        return None, (
            f"something already answers on http://{host}:{port}{HEALTH_PATH}. "
            "If it is a switchyard-server a gateway started earlier, stop it with "
            "`daisugi router stop`. Else pick another port with --switchyard-port."
        )
    argv = [resolved, "--config", str(config_path), "--host", host, "--port", str(port)]
    if routing_log_path is not None:
        argv += ["--routing-log-file", str(routing_log_path)]
    shown = " ".join(argv)
    dry_run = f"{resolved} --config {config_path} --dry-run"
    try:
        pid = spawn(argv, log_path)
    except OSError as exc:
        return None, f"could not start `{shown}`: {exc}"
    t0 = time.monotonic()
    while True:
        if health_probe(host, port):
            handle = SwitchyardHandle(
                pid=pid,
                host=host,
                port=port,
                config_path=Path(config_path),
                argv=argv,
                log_path=log_path,
            )
            if state_path is not None:
                _write_state(state_path, handle, route_id, auth)
            return handle, f"started `{shown}`, pid {pid}, healthy on {handle.base_url}"
        if not alive(pid):
            tail = _log_tail(log_path)
            where = f" Its log is {log_path}:\n{tail}" if tail else ""
            return None, (
                f"`{shown}`, pid {pid}, exited before it answered.{where}\n"
                f"  Check the config with: {dry_run}"
            )
        if time.monotonic() - t0 >= wait_s:
            break
        time.sleep(poll_interval_s)
    with contextlib.suppress(ProcessLookupError, PermissionError):
        kill(pid, signal.SIGTERM)
    grace_end = time.monotonic() + kill_grace_s
    while alive(pid) and time.monotonic() < grace_end:
        time.sleep(poll_interval_s)
    if alive(pid):
        with contextlib.suppress(ProcessLookupError, PermissionError):
            kill(pid, signal.SIGKILL)
    _reap(pid, 0.0)
    where = f" Its log is {log_path}." if log_path else ""
    return None, (
        f"`{shown}`, pid {pid}, did not answer {HEALTH_PATH} in {wait_s:g}s, so it was "
        f"stopped.{where}\n  Check the config with: {dry_run}"
    )


def _reap(pid: int, wait_s: float) -> bool:
    """Wait up to ``wait_s`` for a child this process started. True when it has exited."""
    proc = _CHILDREN.get(pid)
    if proc is None:
        return False
    try:
        if wait_s > 0:
            proc.wait(timeout=wait_s)
        else:
            proc.poll()
    except subprocess.TimeoutExpired:
        return False
    if proc.returncode is None:
        return False
    _CHILDREN.pop(pid, None)
    return True


def stop_own_child(
    handle: SwitchyardHandle,
    state_path: Path,
    *,
    kill: Callable[[int, int], None] = os.kill,
    wait_s: float = 0.0,
) -> str:
    """Stop the child this gateway started, by its own handle, not by the state file.

    The state file is removed only while it still names this pid, so a file
    that another gateway wrote since then stays in place.
    """
    pid = handle.pid
    state = read_state(state_path)
    if state is not None and state.get("pid") == pid:
        Path(state_path).unlink(missing_ok=True)
    try:
        kill(pid, signal.SIGTERM)
    except ProcessLookupError:
        _reap(pid, 0.0)
        return f"switchyard-server, pid {pid}, was already stopped"
    if wait_s > 0 and pid in _CHILDREN and not _reap(pid, wait_s):
        return f"sent SIGTERM to switchyard-server, pid {pid}; it still drains"
    return f"sent SIGTERM to switchyard-server, pid {pid}"


def child_is_running(pid: int) -> bool:
    """True when ``pid`` is alive and, where /proc can tell, still switchyard-server."""
    line = _proc_cmdline(pid)
    if line is not None:
        return BINARY_NAME in line
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    except PermissionError:
        return True
    return True


def _proc_cmdline(pid: int) -> str | None:
    """The command line of ``pid`` from /proc, or None where /proc cannot tell."""
    try:
        raw = Path(f"/proc/{pid}/cmdline").read_bytes()
    except OSError:
        return None
    return raw.replace(b"\0", b" ").decode("utf-8", "replace").strip()


def stop_switchyard(
    state_path: Path,
    *,
    kill: Callable[[int, int], None] | None = None,
    cmdline: Callable[[int], str | None] | None = None,
    wait_s: float = 0.0,
) -> str:
    """Send SIGTERM to the child that the state file names, then remove the file.

    Switchyard drains active requests for up to its --shutdown-timeout, 30s
    by default, before it exits. A pid whose command line does not name
    switchyard-server gets no signal: the pid may belong to another program
    now. A missing state file, or a process that is gone, is reported and
    never raised. With ``wait_s``, a child this process started is waited
    for that long, so its port is free when this returns.
    """
    kill = kill or os.kill
    cmdline = cmdline or _proc_cmdline
    state_path = Path(state_path)
    if not state_path.exists():
        return "no switchyard-server state file; nothing to stop"
    state = read_state(state_path)
    state_path.unlink(missing_ok=True)
    if state is None:
        return f"the state file {state_path} was unreadable; removed it"
    pid = state["pid"]
    line = cmdline(pid)
    if line == "":
        # An exited child that nobody has reaped yet has an empty command line.
        _reap(pid, 0.0)
        return f"switchyard-server, pid {pid}, already exited"
    if line is not None and BINARY_NAME not in line:
        return f"pid {pid} is not switchyard-server now; sent no signal and removed the state"
    try:
        kill(pid, signal.SIGTERM)
    except ProcessLookupError:
        return f"switchyard-server, pid {pid}, was already stopped"
    except PermissionError:
        return f"no permission to signal pid {pid}; sent no signal and removed the state"
    if wait_s > 0 and pid in _CHILDREN and not _reap(pid, wait_s):
        return f"sent SIGTERM to switchyard-server, pid {pid}; it still drains"
    return f"sent SIGTERM to switchyard-server, pid {pid}"
