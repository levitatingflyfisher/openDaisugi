"""The facts the benches must not guess.

Where each compiled verifier client lives and how it is built. What each gate
path does when it breaks. Which package each matcher backend needs. Which loops
we know how to look for.

Every fact cites the file it came from, and `tests/bench/test_options.py`
re-reads that file. A pinned fact with no test is a rumour.
"""

from __future__ import annotations

import os
import shutil
import sys
from dataclasses import dataclass
from pathlib import Path


def repo_root() -> Path | None:
    """The checkout root, or None.

    The root is the directory that holds pyproject.toml and src/opendaisugi.
    An installed wheel has no checkout, so the compiled clients and the
    committed corpora are unreachable. Callers report that instead of guessing.
    """
    here = Path(__file__).resolve()
    for parent in here.parents:
        if (parent / "pyproject.toml").is_file() and (parent / "src" / "opendaisugi").is_dir():
            return parent
    return None


REPO_ROOT = repo_root()


@dataclass(frozen=True)
class ClientSpec:
    """One conformance client: how to run it, how to build it, where that is written.

    `profile` is the conformance profile the client implements. A `full` client
    runs every stage of the oracle. A `core` client runs delegation safety,
    permissions and the DAG check only, so it cannot see a denial that turns
    on an invariant, a postcondition or plan-vs-envelope subsumption.
    """

    name: str
    argv: tuple[str, ...]
    probe: str | None
    build_steps: tuple[str, ...]
    build_cwd: str
    readme: str
    profile: str = "full"


VERIFIER_CLIENTS: dict[str, ClientSpec] = {
    "python": ClientSpec(
        name="python",
        argv=(sys.executable, "-m", "opendaisugi.conformance"),
        probe=None,
        build_steps=(),
        build_cwd=".",
        readme="docs/spec/conformance.md",
    ),
    "rust": ClientSpec(
        name="rust",
        argv=("clients/rust/target/release/conform",),
        probe="clients/rust/target/release/conform",
        build_steps=("cargo build --release",),
        build_cwd="clients/rust",
        readme="clients/rust/README.md",
    ),
    "go": ClientSpec(
        name="go",
        argv=("clients/go/conform",),
        probe="clients/go/conform",
        build_steps=("go build -o conform ./cmd/conform",),
        build_cwd="clients/go",
        readme="clients/go/README.md",
    ),
    "typescript": ClientSpec(
        name="typescript",
        argv=("node", "clients/ts/dist/conform.js"),
        probe="clients/ts/dist/conform.js",
        build_steps=("npm install", "npm run build"),
        build_cwd="clients/ts",
        readme="clients/ts/README.md",
    ),
    "lean": ClientSpec(
        name="lean",
        argv=("clients/lean/.lake/build/bin/conform",),
        probe="clients/lean/.lake/build/bin/conform",
        build_steps=("lake build",),
        build_cwd="clients/lean",
        readme="clients/lean/README.md",
        profile="core",
    ),
}


def client_is_built(spec: ClientSpec) -> bool:
    """True when this client can run right now. Python is always available.

    A probe that is the binary itself must be executable. A stale or half
    written file is absent, not a client that answers nothing.
    """
    if spec.probe is None:
        return True
    root = repo_root()
    if root is None:
        return False
    probe = root / spec.probe
    if not probe.is_file():
        return False
    if spec.argv[0] == "node":
        return shutil.which("node") is not None
    return os.access(probe, os.X_OK)


def client_argv(spec: ClientSpec) -> list[str]:
    """Absolute argv for the client, or an empty list when it is not built."""
    if not client_is_built(spec):
        return []
    if spec.probe is None:
        return list(spec.argv)
    root = repo_root()
    if root is None:
        return []
    return [str(root / a) if a.startswith("clients/") else a for a in spec.argv]


def gate_client_argv(spec: ClientSpec, env: "dict[str, str] | None" = None) -> list[str]:
    """The argv the gate dispatches to for spec, or [] when this box has none.

    The gate finds a compiled client only through OPENDAISUGI_<NAME>_CLIENT
    (a path to it) or as ``daisugi-conform-<name>`` on PATH, never through a
    source checkout, so an installed gate, a gate run from a checkout and a
    gate binary kept anywhere else all decide alike. Python needs no lookup.
    A TypeScript client is a script run with ``node`` from PATH.
    """
    if spec.probe is None:
        return list(spec.argv)
    env = os.environ if env is None else env
    path = env.get(f"OPENDAISUGI_{spec.name.upper()}_CLIENT") or shutil.which(
        f"daisugi-conform-{spec.name}", path=env.get("PATH")
    )
    if not path or not os.path.isfile(path):
        return []
    if spec.argv[0] == "node":
        node = shutil.which("node", path=env.get("PATH"))
        return [node, path] if node else []
    return [path] if os.access(path, os.X_OK) else []


def build_hint(spec: ClientSpec) -> str:
    """The one line an operator can paste to build this client."""
    return f"cd {spec.build_cwd} && " + " && ".join(spec.build_steps)


@dataclass(frozen=True)
class GatePathSpec:
    """One way a tool call reaches the gate, and what happens when that way breaks.

    `fmt` is the gate's `--format` for this path, the one its hook or
    extension passes. pi's built-in names are lowercase and classify only
    under `pi`; every other path speaks the Claude shape.
    """

    name: str
    title: str
    fail_open_class: str
    evidence: str
    source: str
    quote: str
    fmt: str = "claude"


GATE_PATHS: dict[str, GatePathSpec] = {
    "claude-hook": GatePathSpec(
        name="claude-hook",
        title="claude PreToolUse hook",
        fail_open_class="hard",
        evidence="measured",
        source="docs/harness/harness-comparison.md",
        quote="must exit 2",
    ),
    "codex-hooks": GatePathSpec(
        name="codex-hooks",
        title="codex hooks.json",
        fail_open_class="soft",
        evidence="measured",
        source="docs/harness/harness-comparison.md",
        quote="hooks fail open",
    ),
    "pi-ext": GatePathSpec(
        name="pi-ext",
        title="pi extension",
        fail_open_class="hard",
        evidence="designed",
        source="docs/plans/2026-09-08-workshop/00-master-spec.md",
        quote="unreachable gate",
        fmt="pi",
    ),
    "opencode-plugin": GatePathSpec(
        name="opencode-plugin",
        title="opencode plugin",
        fail_open_class="deny-only",
        evidence="designed",
        source="docs/plans/2026-09-08-workshop/00-master-spec.md",
        quote="deny-only by design",
    ),
    "sprig-gate": GatePathSpec(
        name="sprig-gate",
        title="sprig in-process gate",
        fail_open_class="hard",
        evidence="measured",
        source="docs/harness/harness-comparison.md",
        quote="in-process `Executor`",
    ),
    "mcp": GatePathSpec(
        name="mcp",
        title="MCP bridge",
        fail_open_class="advisory",
        evidence="measured",
        source="docs/harness/harness-comparison.md",
        quote="near fail-open",
    ),
}


@dataclass(frozen=True)
class MatcherSpec:
    """One pathway embedder, and the package whose absence degrades it to lexical."""

    name: str
    package: str | None
    extra: str | None
    note: str


MATCHER_BACKENDS: dict[str, MatcherSpec] = {
    "all-MiniLM-L6-v2": MatcherSpec(
        name="all-MiniLM-L6-v2",
        package="sentence_transformers",
        extra="opendaisugi[search]",
        note="torch, about 90MB",
    ),
    "potion": MatcherSpec(
        name="potion",
        package="model2vec",
        extra="opendaisugi[potion]",
        note="torch-free, numpy only, about 30MB",
    ),
    "lexical": MatcherSpec(
        name="lexical",
        package=None,
        extra=None,
        note="keyword floor, no model, no download",
    ),
    "int8": MatcherSpec(
        name="int8",
        package="onnxruntime",
        extra="opendaisugi[int8]",
        note="quantized MiniLM through onnxruntime, no torch",
    ),
}


@dataclass(frozen=True)
class LoopSpec:
    """One harness we can run in a pane, and the gate path it uses."""

    name: str
    binary: str
    install: str
    gate_path: str


LOOPS: dict[str, LoopSpec] = {
    "sprig": LoopSpec("sprig", "sprig", "cd harness/sprig && go build ./cmd/sprig", "sprig-gate"),
    "claude-code": LoopSpec(
        "claude-code", "claude", "npm install -g @anthropic-ai/claude-code", "claude-hook"
    ),
    "codex": LoopSpec("codex", "codex", "npm install -g @openai/codex", "codex-hooks"),
    "pi": LoopSpec("pi", "pi", "daisugi install --harness pi", "pi-ext"),
    "opencode": LoopSpec("opencode", "opencode", "npm install -g opencode-ai", "opencode-plugin"),
}


def loop_is_installed(spec: LoopSpec) -> bool:
    """True when the loop's binary is on PATH. Never raises."""
    return shutil.which(spec.binary) is not None
