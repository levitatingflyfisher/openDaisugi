"""Recipes: the docs/recipes pages as data files that run through install.

A recipe is a JSON file inside the package. Each step names one install
primitive and never a shell command:

- ``install``: layers, runtimes, enforce, report, base_url (the install path);
- ``harness``: the pi or OpenCode extension;
- ``config``: router keys in config.yaml;
- ``warden-init``: a starter envelope for a workspace;
- ``note``: text; nothing runs.

``apply`` runs the steps and writes a receipt under ``<data>/recipes/``;
``revert`` undoes them from that receipt, never from defaults.
"""

from __future__ import annotations

import json
import re
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Callable

from opendaisugi.config import load_config, save_config
from opendaisugi.datahome import data_home

RECIPE_DIR = Path(__file__).parent

STEP_KINDS = ("install", "harness", "config", "warden-init", "note")

# The config keys a `config` step may set: the ones the router install sets.
CONFIG_KEYS = (
    "routing_mode",
    "routing_model",
    "router_local_model",
    "switchyard_efficient_model",
    "switchyard_capable_model",
    "switchyard_api_key_env",
)
ROUTING_MODES = ("rules", "switchyard", "knn", "model", "off")

_PARAM_RE = re.compile(r"\{([A-Za-z_][A-Za-z0-9_]*)\}")


# ---------------------------------------------------------------------------
# Loading
# ---------------------------------------------------------------------------


def names() -> list[str]:
    """The shipped recipe names, sorted."""
    return sorted(p.stem for p in RECIPE_DIR.glob("*.json"))


def _bad(file: str, where: str, why: str) -> ValueError:
    return ValueError(f"{file}: {where}: {why}")


def _check_strings(file: str, where: str, value: Any, declared: set[str]) -> None:
    """Every ``{param}`` a string names must be declared."""
    if isinstance(value, str):
        for name in _PARAM_RE.findall(value):
            if name not in declared:
                raise _bad(file, where, f"param '{name}' is not declared")
    elif isinstance(value, list):
        for i, v in enumerate(value):
            _check_strings(file, f"{where}[{i}]", v, declared)
    elif isinstance(value, dict):
        for k, v in value.items():
            _check_strings(file, f"{where}.{k}", v, declared)


def _validate(raw: Any, file: str) -> dict:
    from opendaisugi.install import COMPONENTS, SUPPORTED_HARNESSES

    if not isinstance(raw, dict):
        raise ValueError(f"{file}: the recipe is not an object")
    for key in ("name", "title", "doc"):
        if not isinstance(raw.get(key), str):
            raise _bad(file, key, "must be a string")
    if not isinstance(raw.get("slots"), dict):
        raise _bad(file, "slots", "must be an object")
    params = raw.get("params")
    if not isinstance(params, dict):
        raise _bad(file, "params", "must be an object")
    for pname, p in params.items():
        if not isinstance(p, dict) or not isinstance(p.get("help"), str):
            raise _bad(file, f"params.{pname}", "needs a help string")
    steps = raw.get("steps")
    if not isinstance(steps, list):
        raise _bad(file, "steps", "must be a list")
    declared = set(params)
    known = {c[0] for c in COMPONENTS}
    for i, step in enumerate(steps):
        where = f"steps[{i}]"
        if not isinstance(step, dict):
            raise _bad(file, where, "must be an object")
        kind = step.get("kind")
        if kind not in STEP_KINDS:
            raise ValueError(f"{file}: {where}.kind '{kind}' is not one of {', '.join(STEP_KINDS)}")
        when = step.get("when")
        if when is not None and when not in declared:
            raise _bad(file, f"{where}.when", f"param '{when}' is not declared")
        if kind == "install":
            only = step.get("only")
            if not isinstance(only, list) or not only or not all(isinstance(x, str) for x in only):
                raise _bad(file, f"{where}.only", "must be a list of component names")
            for x in only:
                if x not in known:
                    raise _bad(file, f"{where}.only", f"unknown component '{x}'")
            runtime = step.get("runtime", [])
            if not isinstance(runtime, list) or not all(isinstance(x, str) for x in runtime):
                raise _bad(file, f"{where}.runtime", "must be a list of runtime names")
            if not isinstance(step.get("enforce", False), bool):
                raise _bad(file, f"{where}.enforce", "must be true or false")
            for key in ("report", "base_url"):
                if step.get(key) is not None and not isinstance(step[key], str):
                    raise _bad(file, f"{where}.{key}", "must be a string or null")
        elif kind == "harness":
            if step.get("name") not in SUPPORTED_HARNESSES:
                raise _bad(
                    file,
                    f"{where}.name",
                    f"'{step.get('name')}' is not one of {', '.join(SUPPORTED_HARNESSES)}",
                )
        elif kind == "config":
            sets = step.get("set")
            if not isinstance(sets, dict) or not sets:
                raise _bad(file, f"{where}.set", "must be an object of config keys")
            for k, v in sets.items():
                if k not in CONFIG_KEYS:
                    raise _bad(file, f"{where}.set.{k}", f"'{k}' is not a key a recipe may set")
                if not isinstance(v, str):
                    raise _bad(file, f"{where}.set.{k}", "must be a string")
        elif kind == "warden-init":
            if not isinstance(step.get("workspace"), str):
                raise _bad(file, f"{where}.workspace", "must be a string")
        elif kind == "note" and not isinstance(step.get("text"), str):
            raise _bad(file, f"{where}.text", "must be a string")
        _check_strings(file, where, {k: v for k, v in step.items() if k != "kind"}, declared)
    return raw


def load(name: str) -> dict:
    """The recipe called ``name``, validated. Raises ``ValueError`` on a bad file."""
    path = RECIPE_DIR / f"{name}.json"
    try:
        raw = json.loads(path.read_text(encoding="utf-8"))
    except ValueError as exc:
        raise ValueError(f"{path.name}: {exc}") from exc
    return _validate(raw, path.name)


def load_all() -> list[dict]:
    """Every shipped recipe, validated."""
    return [load(n) for n in names()]


# ---------------------------------------------------------------------------
# Showing
# ---------------------------------------------------------------------------


def _step_summary(step: dict) -> str:
    kind = step["kind"]
    if kind == "install":
        parts = [f"--only {','.join(step['only'])}"]
        if step.get("runtime"):
            parts.append(f"--runtime {','.join(step['runtime'])}")
        if step.get("enforce"):
            parts.append("--enforce")
        if step.get("report"):
            parts.append(f"--report {step['report']}")
        if step.get("base_url"):
            parts.append(f"--base-url {step['base_url']}")
        return " ".join(parts)
    if kind == "harness":
        return step["name"]
    if kind == "config":
        return " ".join(f"{k}={v}" for k, v in step["set"].items())
    if kind == "warden-init":
        return f"workspace={step['workspace']}"
    return step["text"]


def _metavar(recipe: dict, param: str) -> str:
    return recipe["params"][param].get("metavar", "VALUE")


def render_show(recipe: dict) -> str:
    """The text `daisugi recipe show NAME` prints."""
    lines = [recipe["title"], "", "Slots"]
    for slot, filled in recipe["slots"].items():
        lines.append(f"  {slot}: {filled}")
    if recipe["params"]:
        lines += ["", "Params"]
        for pname, p in recipe["params"].items():
            if p.get("required"):
                tag = "required"
            elif "default" in p:
                tag = f"default {p['default']}"
            else:
                tag = "optional"
            lines.append(f"  {pname} ({tag}): {p['help']}")
    lines += ["", "Steps"]
    for i, step in enumerate(recipe["steps"], 1):
        line = f"  {i}. {step['kind']}  {_step_summary(step)}"
        if step.get("when"):
            line += f"  (only with --set {step['when']}={_metavar(recipe, step['when'])})"
        lines.append(line)
    return "\n".join(lines)


# ---------------------------------------------------------------------------
# Params
# ---------------------------------------------------------------------------


def resolve_params(recipe: dict, given: dict[str, str]) -> dict[str, str]:
    """Defaults filled in, in the recipe's param order.

    Raises ``ValueError`` on a key the recipe does not declare and on a
    missing required param.
    """
    for key in given:
        if key not in recipe["params"]:
            raise ValueError(f"recipe '{recipe['name']}' has no param '{key}'")
    out: dict[str, str] = {}
    for pname, p in recipe["params"].items():
        if pname in given:
            out[pname] = given[pname]
        elif "default" in p:
            out[pname] = p["default"]
        elif p.get("required"):
            raise ValueError(
                f"recipe '{recipe['name']}' needs --set {pname}={_metavar(recipe, pname)}"
            )
    return out


def _fill(value: Any, params: dict[str, str], recipe: dict, index: int) -> Any:
    """``value`` with each ``{param}`` replaced by its value."""
    if isinstance(value, str):

        def sub(m: re.Match) -> str:
            name = m.group(1)
            if name not in params:
                raise ValueError(f"step {index} needs --set {name}={_metavar(recipe, name)}")
            return params[name]

        return _PARAM_RE.sub(sub, value)
    if isinstance(value, list):
        return [_fill(v, params, recipe, index) for v in value]
    if isinstance(value, dict):
        return {k: _fill(v, params, recipe, index) for k, v in value.items()}
    return value


# ---------------------------------------------------------------------------
# Planning and applying
# ---------------------------------------------------------------------------


def _recipes_dir(home: Path) -> Path:
    return data_home(home=home) / "recipes"


def receipt_path(name: str, home: Path) -> Path:
    return _recipes_dir(home) / f"{name}.json"


def _config_path(home: Path) -> Path:
    return data_home(home=home) / "config.yaml"


def _envelope_path(home: Path) -> Path:
    return data_home(home=home) / "warden" / "envelopes" / "default.json"


def _runtimes(step: dict, home: Path) -> list:
    from opendaisugi.install import _select_runtimes, detect_runtimes

    return _select_runtimes(step["runtime"]) if step.get("runtime") else detect_runtimes(home=home)


def _check_config(cfg, update: dict[str, str]) -> None:
    """The checks `daisugi install --routing-mode` makes, before any write."""
    mode = update.get("routing_mode", cfg.routing_mode)
    if "routing_mode" in update and mode not in ROUTING_MODES:
        raise ValueError(
            f"unknown routing_mode '{mode}'. Choose one of: {', '.join(ROUTING_MODES)}"
        )
    if mode != "switchyard":
        return
    candidate = cfg.model_copy(update=update)
    if not candidate.switchyard_efficient_model:
        raise ValueError(
            "routing_mode switchyard needs switchyard_efficient_model: "
            "a local model id, or a claude-* id"
        )
    from opendaisugi.routing_switchyard import render_switchyard_toml, targets_from_config

    try:
        render_switchyard_toml(
            targets_from_config(candidate),
            route_id=cfg.switchyard_route_id,
            api_key_present=lambda name: True,
        )
    except ValueError as exc:
        raise ValueError(f"cannot build the Switchyard route: {exc}") from exc


def _config_changes(step: dict, home: Path) -> tuple[dict[str, tuple[Any, str]], Any]:
    cfg = load_config(_config_path(home))
    update = dict(step["set"])
    _check_config(cfg, update)
    changes = {k: (getattr(cfg, k), v) for k, v in update.items() if getattr(cfg, k) != v}
    return changes, cfg


def _shown(value: Any) -> str:
    return "(unset)" if value is None else str(value)


def plan_apply(recipe: dict, params: dict[str, str], *, home: Path) -> list[dict]:
    """What each step would do, without writing anything.

    A step is ``{index, kind, summary, writes, state, notes}``. ``state`` is
    ``run``, ``skip`` or ``kept``. ``notes`` are extra lines for the step.
    """
    from opendaisugi.install import component_layers, harness_extension_target
    from opendaisugi.install import install as _install

    plan: list[dict] = []
    for i, raw in enumerate(recipe["steps"], 1):
        when = raw.get("when")
        if when is not None and when not in params:
            plan.append(
                {
                    "index": i,
                    "kind": raw["kind"],
                    "summary": _step_summary(raw),
                    "writes": [],
                    "state": "skip",
                    "notes": [f"skipped (no --set {when})"],
                }
            )
            continue
        step = _fill(raw, params, recipe, i)
        kind = step["kind"]
        entry: dict = {
            "index": i,
            "kind": kind,
            "summary": _step_summary(step),
            "writes": [],
            "state": "run",
            "notes": [],
        }
        if kind == "install":
            runtimes = _runtimes(step, home)
            if step.get("enforce"):
                env_dir = data_home(home=home) / "warden" / "envelopes"
                if not (env_dir.exists() and any(env_dir.glob("*.json"))):
                    raise ValueError(f"step {i}: enforce needs a registered envelope")
            if not runtimes:
                entry["state"] = "kept"
                entry["notes"] = ["no supported agent runtime detected"]
            else:
                res = _install(
                    home=home,
                    dry_run=True,
                    runtimes=runtimes,
                    layers=component_layers(step["only"]),
                    enforce=bool(step.get("enforce")),
                    base_url=step.get("base_url"),
                    report=step.get("report"),
                )
                seen: list[str] = []
                for s in res.planned:
                    if s.supported and s.target is not None and str(s.target) not in seen:
                        seen.append(str(s.target))
                entry["writes"] = seen
        elif kind == "harness":
            entry["writes"] = [str(harness_extension_target(step["name"], home=home))]
        elif kind == "config":
            changes, _cfg = _config_changes(step, home)
            if not changes:
                entry["state"] = "kept"
            else:
                entry["writes"] = [str(_config_path(home))]
                entry["notes"] = [
                    f"{k}: {_shown(old)} -> {new}" for k, (old, new) in changes.items()
                ]
        elif kind == "warden-init":
            if _envelope_path(home).exists():
                entry["state"] = "kept"
            else:
                entry["writes"] = [str(_envelope_path(home))]
        plan.append(entry)
    return plan


def _plan_lines(recipe: dict, plan: list[dict], verb: str) -> list[str]:
    total = len(recipe["steps"])
    lines: list[str] = []
    for e in plan:
        lines.append(f"[{e['index']}/{total}] {e['kind']}  {e['summary']}")
        if e["state"] == "skip":
            lines.append(f"  {e['notes'][0]}")
        elif e["state"] == "kept":
            lines.append("  " + (e["notes"][0] if e["notes"] else "kept"))
        else:
            lines += [f"  {verb} {w}" for w in e["writes"]]
            if e["kind"] == "config":
                lines += [f"    {n}" for n in e["notes"]]
    return lines


def render_plan(recipe: dict, plan: list[dict]) -> list[str]:
    """The dry-run text: one block per step."""
    return _plan_lines(recipe, plan, "would write")


class StepFailed(Exception):
    """A step raised; ``index`` is its 1-based number."""

    def __init__(self, index: int, why: str) -> None:
        super().__init__(why)
        self.index = index


def _lacking(runtimes: list, layers: set, home: Path) -> dict[str, list[str]]:
    """{runtime key: component names} of the layers each runtime does not hold yet."""
    from opendaisugi.install import component_names, layer_present, runtime_key

    out: dict[str, list[str]] = {}
    for rt in runtimes:
        unsupported = rt.unsupported_layers() if hasattr(rt, "unsupported_layers") else {}
        absent = {
            layer
            for layer in layers
            if layer not in unsupported and not layer_present(rt, home, layer)
        }
        if absent:
            out[runtime_key(rt)] = component_names(absent)
    return out


def _merge_undo(new: Any, old: Any) -> Any:
    """The undo of a step that ran again: what this run added, plus what an earlier run did."""
    if new is None:
        return old
    if isinstance(new, dict) and isinstance(old, dict) and "restore" in new and "restore" in old:
        # The first run saw the value from before the recipe: it wins.
        return {"restore": {**new["restore"], **old["restore"]}}
    if (
        isinstance(new, dict)
        and isinstance(old, dict)
        and "uninstall" in new
        and "uninstall" in old
    ):
        from opendaisugi.install import COMPONENTS

        order = [c[0] for c in COMPONENTS]
        merged = {k: list(v) for k, v in old["uninstall"].items()}
        for key, comps in new["uninstall"].items():
            have = merged.setdefault(key, [])
            have += [c for c in comps if c not in have]
        return {
            "uninstall": {
                k: sorted(v, key=lambda c: order.index(c) if c in order else len(order))
                for k, v in merged.items()
            }
        }
    return new


def _run_step(
    raw: dict, step: dict, params: dict[str, str], *, home: Path, index: int
) -> tuple[list[str], list[str], Any]:
    """Run one step. Returns (written paths, extra lines, undo)."""
    from opendaisugi.install import component_layers, install_harness_extension
    from opendaisugi.install import install as _install

    kind = step["kind"]
    if kind == "note":
        return [], [], None
    if kind == "install":
        runtimes = _runtimes(step, home)
        if not runtimes:
            return [], ["no supported agent runtime detected"], None
        layers = component_layers(step["only"])
        # What each runtime lacks now: only that is this recipe's to undo.
        lacking = _lacking(runtimes, layers, home)
        res = _install(
            home=home,
            yes=True,
            runtimes=runtimes,
            layers=layers,
            enforce=bool(step.get("enforce")),
            base_url=step.get("base_url"),
            report=step.get("report"),
        )
        if res.failures:
            raise RuntimeError("; ".join(res.failures))
        written: list[str] = []
        for p in res.modified_files:
            if str(p) not in written:
                written.append(str(p))
        undo = {"uninstall": lacking} if written and lacking else None
        return written, [], undo
    if kind == "harness":
        from opendaisugi.install import harness_extension_target
        from opendaisugi.install import install_harness_extension as _ihe

        existed = harness_extension_target(step["name"], home=home).exists()
        paths = _ihe(step["name"], home=home)
        return [str(p) for p in paths], [], (None if existed else {"harness": step["name"]})
    if kind == "config":
        changes, cfg = _config_changes(step, home)
        if not changes:
            return [], [], None
        path = _config_path(home)
        new = cfg.model_copy(update={k: v for k, (_old, v) in changes.items()})
        save_config(new, path)
        written = [str(path)]
        if new.routing_mode == "switchyard":
            from opendaisugi.routing_switchyard import (
                render_switchyard_toml,
                targets_from_config,
                write_switchyard_config,
            )

            toml = write_switchyard_config(
                data_home(home=home),
                render_switchyard_toml(
                    targets_from_config(new),
                    route_id=new.switchyard_route_id,
                    api_key_present=lambda name: True,
                ),
            )
            written.append(str(toml))
        lines = [f"{k}: {_shown(old)} -> {v}" for k, (old, v) in changes.items()]
        return written, lines, {"restore": {k: old for k, (old, _v) in changes.items()}}
    # warden-init
    from opendaisugi.warden import register_envelope, starter_envelope

    target = _envelope_path(home)
    if target.exists():
        return [], [], None
    decompose = load_config(_config_path(home)).shell_allow_decomposition
    ws = Path(step["workspace"]).expanduser().resolve()
    path = register_envelope(
        starter_envelope(ws, allow_shell_decomposition=decompose),
        root=data_home(home=home) / "warden",
    )
    return [str(path)], [], {"remove_envelope": "default"}


def _read_receipt(name: str, home: Path) -> dict | None:
    path = receipt_path(name, home)
    try:
        d = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return None
    return d if isinstance(d, dict) else None


def apply(
    recipe: dict,
    params: dict[str, str],
    *,
    home: Path,
    out: Callable[[str], None],
    quiet: bool = False,
) -> dict:
    """Run the steps in order and write the receipt. Returns the receipt.

    Stops at the first failing step, writes the receipt with ``completed``
    set to the steps before it, and raises ``StepFailed``.
    """
    total = len(recipe["steps"])
    plan = plan_apply(recipe, params, home=home)  # checks every step before any write
    old = _read_receipt(recipe["name"], home) or {}
    old_undo = {s.get("index"): s.get("undo") for s in old.get("steps", []) if isinstance(s, dict)}
    done: list[dict] = []
    completed = 0
    failure: StepFailed | None = None
    for i, raw in enumerate(recipe["steps"], 1):
        entry = plan[i - 1]
        header = f"[{i}/{total}] {raw['kind']}  {entry['summary']}"
        if not quiet:
            out(header)
        if entry["state"] == "skip":
            if not quiet:
                out(f"  {entry['notes'][0]}")
            completed = i
            if old_undo.get(i) is not None:  # the earlier run's undo stays
                done.append({"index": i, "kind": raw["kind"], "undo": old_undo[i]})
            continue
        step = _fill(raw, params, recipe, i)
        try:
            written, lines, undo = _run_step(raw, step, params, home=home, index=i)
        except Exception as exc:  # noqa: BLE001 - a step failure stops the recipe
            failure = StepFailed(i, str(exc))
            break
        if not quiet:
            for w in written:
                out(f"  wrote {w}")
            for ln in lines:
                out(f"    {ln}" if step["kind"] == "config" else f"  {ln}")
            if not written and not lines and step["kind"] != "note":
                out("  kept")
        undo = _merge_undo(undo, old_undo.get(i))  # a kept step keeps the earlier undo
        done.append({"index": i, "kind": raw["kind"], "undo": undo})
        completed = i
    if failure is not None:
        # Steps this run did not reach keep the undo of the run that did them.
        have = {d["index"] for d in done}
        for s_old in old.get("steps", []):
            if isinstance(s_old, dict) and s_old.get("index") not in have:
                done.append(s_old)
        done.sort(key=lambda d: d["index"])
    receipt = {
        "recipe": recipe["name"],
        "applied_at": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
        "params": params,
        "completed": completed,
        "steps": done,
    }
    path = receipt_path(recipe["name"], home)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(receipt, indent=2) + "\n", encoding="utf-8")
    if failure is not None:
        raise failure
    return receipt


# ---------------------------------------------------------------------------
# Reverting
# ---------------------------------------------------------------------------


def _undo_summary(undo: Any) -> str:
    if undo is None:
        return "nothing to undo"
    if "uninstall" in undo:
        parts = [f"{','.join(comps)} from {key}" for key, comps in undo["uninstall"].items()]
        return "uninstall " + "; ".join(parts) + " (added by this recipe)"
    if "harness" in undo:
        return f"remove {undo['harness']} extension"
    if "restore" in undo:
        return "restore " + " ".join(f"{k}={_shown(v)}" for k, v in undo["restore"].items())
    return f"remove envelope {undo['remove_envelope']}"


_KNOWN_UNDO = ("uninstall", "harness", "restore", "remove_envelope")


def unread_undo(receipt: dict) -> bool:
    """True when a step of the receipt holds an undo this version does not read."""
    for s in receipt.get("steps", []):
        undo = s.get("undo") if isinstance(s, dict) else 0
        if undo is not None and not (
            isinstance(undo, dict) and any(k in undo for k in _KNOWN_UNDO)
        ):
            return True
    return False


def revert_plan(name: str, *, home: Path) -> tuple[dict, dict] | None:
    """(recipe, receipt) for ``name``, or None when there is no receipt."""
    receipt = _read_receipt(name, home)
    if receipt is None:
        return None
    return load(name), receipt


def revert_lines(recipe: dict, receipt: dict) -> list[str]:
    """The dry-run text of a revert: the steps in reverse order."""
    total = len(recipe["steps"])
    lines = []
    for s in reversed(receipt.get("steps", [])):
        lines.append(f"[{s['index']}/{total}] {s['kind']}  {_undo_summary(s.get('undo'))}")
    return lines


def revert(name: str, *, home: Path, out: Callable[[str], None]) -> int:
    """Undo every executed step of ``name`` from its receipt, last first.

    Returns 0 when every step was undone and the receipt is deleted. Returns 1
    when there is no receipt (nothing is printed then). Returns 2 when an
    uninstall was refused or failed: the reason is printed under its step and
    the receipt is rewritten with only the steps not undone.
    """
    found = revert_plan(name, home=home)
    if found is None:
        return 1
    recipe, receipt = found
    from opendaisugi.install import (
        _RUNTIME_KEYS,
        _select_runtimes,
        component_layers,
        uninstall,
        uninstall_harness_extension,
    )

    total = len(recipe["steps"])
    left: list[dict] = []
    for s in reversed(receipt.get("steps", [])):
        undo = s.get("undo")
        out(f"[{s['index']}/{total}] {s['kind']}  {_undo_summary(undo)}")
        if undo is None:
            continue
        removed: list[str] = []
        failures: list[str] = []
        if "uninstall" in undo:
            for key, comps in undo["uninstall"].items():
                if key not in _RUNTIME_KEYS:
                    failures.append(f"{key}: not a runtime this CLI knows")
                    continue
                res = uninstall(
                    home=home,
                    runtimes=_select_runtimes([key]),
                    layers=component_layers(comps),
                )
                removed += [str(p) for p in res.modified_files]
                failures += res.failures
        elif "harness" in undo:
            removed = [str(p) for p in uninstall_harness_extension(undo["harness"], home=home)]
        elif "restore" in undo:
            path = _config_path(home)
            cfg = load_config(path)
            save_config(cfg.model_copy(update=undo["restore"]), path)
            out(f"  wrote {path}")
            continue
        else:
            target = (
                data_home(home=home) / "warden" / "envelopes" / f"{undo['remove_envelope']}.json"
            )
            if target.exists():
                target.unlink()
                removed = [str(target)]
        for p in removed:
            out(f"  removed {p}")
        if failures:
            for f in failures:
                out(f"  not undone: {f}")
            left.append(s)
        elif not removed:
            out("  kept")
    if left:
        left.reverse()
        receipt["steps"] = left
        receipt_path(name, home).write_text(json.dumps(receipt, indent=2) + "\n", encoding="utf-8")
        return 2
    receipt_path(name, home).unlink()
    return 0
