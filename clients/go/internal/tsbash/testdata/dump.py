"""Dump py-tree-sitter's bash trees in the form tsbash_test.Dump writes.

    python dump.py commands.txt out.jsonl     # one command per line (JSON strings)

Each input line is a JSON string, so multi-line commands fit on one line.
"""

import json
import sys

import tree_sitter_bash as tsb
from tree_sitter import Language, Parser

parser = Parser(Language(tsb.language()))


def dump(src: bytes) -> str:
    root = parser.parse(src).root_node
    out = [f"E{'true' if root.has_error else 'false'}"]

    def walk(n) -> None:
        out.append(f"({n.type} {n.start_byte} {n.end_byte}")
        if n.is_missing:
            out.append(" M")
        if n.type == "command":
            nm = n.child_by_field_name("name")
            if nm is not None:
                out.append(f" N{nm.start_byte}:{nm.end_byte}")
        for c in n.children:
            walk(c)
        out.append(")")

    walk(root)
    return "".join(out)


def main() -> None:
    sys.setrecursionlimit(100000)
    with open(sys.argv[1], encoding="utf-8") as f, open(sys.argv[2], "w", encoding="utf-8") as o:
        for line in f:
            if not line.strip():
                continue
            src = json.loads(line)
            o.write(json.dumps({"src": src, "dump": dump(src.encode("utf-8", "surrogatepass"))}) + "\n")


if __name__ == "__main__":
    main()
