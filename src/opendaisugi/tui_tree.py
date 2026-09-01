"""One session's prompt tree. Rewind has two axes and says what it will not restore.

Enter opens the rewind menu. The Tree widget owns the ``enter`` key (its
``select_cursor`` posts ``NodeSelected``), so we drive rewind from
``on_tree_node_selected`` — the natural "the operator pressed enter on this node"
signal — rather than a screen binding the Tree would shadow. ``L`` labels via the
shared command line; ``Ctrl+O`` cycles filters.
"""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path

from textual.app import ComposeResult
from textual.binding import Binding
from textual.screen import ModalScreen
from textual.widgets import Static, Tree

from opendaisugi.claude_transcript import read_turns
from opendaisugi.session_tree import SessionTree
from opendaisugi.tui_base import CockpitScreen

FILTERS = ("all", "no tools", "prompts", "labeled", "verdicts")
_TOOL_TYPES = {"tool_call", "verdict", "tool_result"}


@dataclass(frozen=True)
class Node:
    id: str
    parent: str | None
    kind: str
    label: str
    has_checkpoint: bool = False


def nodes_for_sprig(tree: SessionTree) -> list[Node]:
    labels = {
        e.data.get("target"): e.data.get("label") for e in tree.entries() if e.type == "label"
    }
    cps = {e.parent_id for e in tree.entries() if e.type == "checkpoint"}
    out = []
    for e in tree.entries():
        if not e.id or e.type in ("session", "label", "head", "checkpoint", "state"):
            continue
        text = e.data.get("text") or " ".join(
            str(e.data.get(k, "")) for k in ("name", "detail") if e.data.get(k)
        )
        if e.type == "verdict":
            text = f"{e.data.get('decision', '')} {e.data.get('toolUseId', '')} · {e.data.get('clause', '')}"
        tag = f" [{labels[e.id]}]" if e.id in labels else ""
        out.append(Node(e.id, e.parent_id, e.type, f"{e.type}: {text[:60]}{tag}", e.id in cps))
    return out


def nodes_for_claude(transcript: Path, tree: SessionTree | None) -> list[Node]:
    verdicts = {}
    if tree is not None:
        for e in tree.entries():
            if e.type == "verdict" and e.data.get("toolUseId"):
                verdicts[e.data["toolUseId"]] = e.data
    out = []
    for t in read_turns(transcript):
        kind = "prompt" if (t.kind == "user" and not t.tool_result_ids) else t.kind
        out.append(Node(t.uuid, t.parent_uuid, kind, f"{kind}: {t.text[:60]}"))
        for u in t.tool_uses:
            v = verdicts.get(u["id"])
            vtxt = (
                f"{v.get('decision')} {u['id']} · {v.get('clause', '')}"
                if v
                else "(no verdict recorded)"
            )
            out.append(Node(f"{t.uuid}/{u['id']}", t.uuid, "verdict", f"{u['name']} → {vtxt}"))
    return out


class RewindMenu(ModalScreen[str]):
    BINDINGS = [
        Binding("c", "pick('conversation')", "c conversation"),
        Binding("w", "pick('workspace')", "w workspace"),
        Binding("b", "pick('both')", "b both"),
        Binding("f", "pick('fork')", "f fork"),
        Binding("escape,n", "pick('never')", "n never mind"),
    ]

    def __init__(self, node: Node, *, workspace_ok: bool, skipped: list[str]) -> None:
        super().__init__()
        self.node, self.workspace_ok, self.skipped = node, workspace_ok, skipped

    def compose(self) -> ComposeResult:
        rows = [f"Rewind to: {self.node.label}", "", "c  Restore conversation (move the head here)"]
        if self.workspace_ok:
            rows += ["w  Restore workspace (a rollback ref is taken first)", "b  Restore both"]
            if self.skipped:
                rows.append(f"   will NOT restore: {', '.join(self.skipped[:5])}")
        rows += ["f  Fork here (new session, this path copied)", "n  Never mind"]
        yield Static("\n".join(rows), id="menu")

    def action_pick(self, choice: str) -> None:
        self.dismiss(choice)


class TreeScreen(CockpitScreen):
    BINDINGS = [
        Binding("ctrl+o", "cycle_filter", "^O filter", show=True),
        Binding("L", "label", "L label", show=True),
    ]

    def __init__(self) -> None:
        super().__init__(name="tree")
        self._filter = 0
        self._node_cache: list[Node] = []
        self._labeling: Node | None = None

    def compose_body(self) -> ComposeResult:
        yield Static("no session selected · press t on a row", id="tree-title")
        yield Tree("session", id="tree")

    def on_mount(self) -> None:
        super().on_mount()  # the shared header
        self.reload()

    def on_screen_resume(self) -> None:
        super().on_screen_resume()  # the shared header
        self.reload()

    def clear_prefill(self) -> None:
        self._labeling = None

    # --- data ---------------------------------------------------------------
    def _session(self) -> SessionTree | None:
        sid = self.app.selected_session
        if not sid:
            return None
        try:
            return SessionTree.open(self.app.data_dir / "sessions", sid)
        except FileNotFoundError:
            return None

    def reload(self) -> None:
        tree_w = self.query_one("#tree", Tree)
        tree_w.clear()
        st = self._session()
        if st is None:
            self.query_one("#tree-title", Static).update("no session selected · press t on a row")
            return
        meta = st.meta()
        self.query_one("#tree-title", Static).update(
            f"{st.session_id} · {meta.get('harness')} · filter: {FILTERS[self._filter]}  ·  "
            "⏎ rewind/fork · ^O filter · L label"
        )
        if meta.get("harness") == "claude-code" and meta.get("transcriptPath"):
            self._node_cache = nodes_for_claude(Path(meta["transcriptPath"]), st)
        else:
            self._node_cache = nodes_for_sprig(st)
        shown = [n for n in self._node_cache if self._visible(n)]
        by_parent: dict[str | None, list[Node]] = {}
        for n in shown:
            key = n.parent if any(m.id == n.parent for m in shown) else None
            by_parent.setdefault(key, []).append(n)

        def add(parent_widget, pid):
            for n in by_parent.get(pid, []):
                w = parent_widget.add(n.label, data=n, expand=True)
                add(w, n.id)

        add(tree_w.root, None)
        tree_w.root.expand()

    def _visible(self, n: Node) -> bool:
        f = FILTERS[self._filter]
        return (
            f == "all"
            or (f == "no tools" and n.kind not in _TOOL_TYPES)
            or (f == "prompts" and n.kind == "prompt")
            or (f == "labeled" and n.label.endswith("]"))
            or (f == "verdicts" and n.kind == "verdict")
        )

    # --- actions ------------------------------------------------------------
    def action_cycle_filter(self) -> None:
        self._filter = (self._filter + 1) % len(FILTERS)
        self.reload()
        self.app.set_status(f"filter: {FILTERS[self._filter]}")

    def _selected(self) -> Node | None:
        node = self.query_one("#tree", Tree).cursor_node
        return node.data if node is not None and isinstance(node.data, Node) else None

    def action_label(self) -> None:
        n = self._selected()
        if n is None:
            self.app.set_status("select an entry to label")
            return
        self._labeling = n
        self.app.open_cmd(prefill="")
        self.app.set_status(f"label {n.id}, then ⏎")

    def on_cmd_submitted(self, value: str) -> bool:
        n = self._labeling
        if n is None:
            return False
        self._labeling = None
        st = self._session()
        if st is not None and "/" not in n.id:
            st.append("label", {"target": n.id, "label": value})
            self.app.set_status(f"labeled {n.id}: {value}")
            self.reload()
        else:
            self.app.set_status("labels on Claude transcript rows live in daisugi only (not yet)")
        return True

    def on_tree_node_selected(self, event) -> None:
        # Enter on a node (the Tree posts this; a programmatic move_cursor does
        # not) opens the rewind menu.
        self.action_rewind()

    def action_rewind(self) -> None:
        n = self._selected()
        st = self._session()
        if n is None or st is None:
            return
        cps = [e for e in st.entries() if e.type == "checkpoint"]
        if st.meta().get("harness") == "claude-code":
            on_path = cps
        else:
            path_ids = {x.id for x in st.path_to(n.id)} if "/" not in n.id else set()
            on_path = [e for e in cps if e.parent_id in path_ids]
        cp = on_path[-1] if on_path else None
        skipped = list((cp.data.get("skipped") if cp else []) or [])
        self.app.push_screen(
            RewindMenu(n, workspace_ok=cp is not None, skipped=skipped),
            callback=lambda choice: self._apply(choice, n, cp),
        )

    def _apply(self, choice: str, n: Node, cp) -> None:
        st = self._session()
        if st is None or choice == "never":
            return
        meta = st.meta()
        if meta.get("harness") == "claude-code":
            if choice == "fork":
                head = st.head()
                child = (
                    st.fork(head) if head else None
                )  # daisugi's own entries; Claude owns prompts
                self.app.set_status(
                    f"fork: run `claude --resume {st.session_id} --fork-session` "
                    f"(daisugi registered {child.session_id if child else 'no child'}); "
                    "rewind inside Claude with Esc Esc"
                )
            else:
                self.app.set_status(
                    "rewind inside Claude with Esc Esc; daisugi sees the new head on the next tool call"
                )
            return
        if choice in ("conversation", "both"):
            st.set_head(n.id)
        if choice in ("workspace", "both") and cp is not None:
            from opendaisugi.checkpoints import restore

            rb = restore(
                Path(meta["cwd"]), ref=cp.data["ref"], session_id=st.session_id, entry_id=n.id
            )
            self.app.set_status(f"workspace restored to {cp.data['ref']} · rollback at {rb.ref}")
        if choice == "conversation":
            self.app.set_status(f"head moved to {n.id} · workspace not restored")
        if choice == "fork":
            child = st.fork(n.id)
            self.app.set_status(
                f"forked {st.session_id} at {n.id} → {child.session_id} · "
                f"resume with `sprig --resume {child.session_id}`"
            )
        self.reload()
