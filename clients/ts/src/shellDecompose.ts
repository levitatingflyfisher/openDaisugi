/**
 * Port of shell_decompose.py — fail-closed compound-shell decomposition via
 * web-tree-sitter + the vendored tree-sitter-bash grammar (pinned 0.25.1,
 * same version as the oracle's wheel; see clients/ts/README.md).
 */

import { Parser, Language, Node } from "web-tree-sitter";
import { fileURLToPath } from "node:url";
import path from "node:path";

const WRITE_REDIRECT_OPS = new Set([">", ">>", "&>", "&>>", ">|", ">&"]);
const READ_REDIRECT_OPS = new Set(["<", "<&"]);
const FD_CLOSE_OPS = new Set([">&-", "<&-"]);

export interface Decomposition {
  ok: boolean;
  heads: string[];
  commands: string[];
  reads: string[];
  writes: string[];
  reason: string;
}

function reject(reason: string): Decomposition {
  return { ok: false, heads: [], commands: [], reads: [], writes: [], reason };
}

let parserPromise: Promise<Parser> | null = null;

/** Resolve the vendored grammar wasm relative to this module (dist/ or src/). */
function defaultWasmPath(): string {
  const here = path.dirname(fileURLToPath(import.meta.url));
  // dist/shellDecompose.js -> ../vendor/tree-sitter-bash.wasm
  return path.resolve(here, "..", "vendor", "tree-sitter-bash.wasm");
}

export async function getParser(wasmPath?: string): Promise<Parser> {
  if (!parserPromise) {
    parserPromise = (async () => {
      await Parser.init();
      const lang = await Language.load(wasmPath ?? defaultWasmPath());
      const parser = new Parser();
      parser.setLanguage(lang);
      return parser;
    })();
  }
  return parserPromise;
}

function literalText(node: Node): string | null {
  if (node.type === "word") {
    return node.text;
  }
  if (node.type === "raw_string") {
    return node.text.slice(1, -1);
  }
  if (node.type === "string") {
    const parts = node.children.filter((c): c is Node => c !== null && c.type !== '"');
    if (parts.every((c) => c.type === "string_content")) {
      return parts.map((c) => c.text).join("");
    }
  }
  return null;
}

/** Returns (readPath, writePath, rejectReason) — at most one of the first two is non-null. */
function classifyFileRedirect(node: Node): {
  read: string | null;
  write: string | null;
  reject: string | null;
} {
  let operator: string | null = null;
  let destination: Node | null = null;
  for (const child of node.children) {
    if (child === null) continue;
    if (child.type === "file_descriptor") continue;
    if (operator === null) {
      operator = child.type;
      continue;
    }
    destination = child;
    break;
  }

  if (operator !== null && FD_CLOSE_OPS.has(operator) && destination === null) {
    return { read: null, write: null, reject: null };
  }
  if (operator === null || destination === null) {
    return {
      read: null,
      write: null,
      reject: `unrecognized shell redirection (${JSON.stringify(node.text)})`,
    };
  }
  if (destination.type === "number") {
    if (operator === ">&" || operator === "<&") {
      return { read: null, write: null, reject: null };
    }
    return {
      read: null,
      write: null,
      reject: `unrecognized shell redirection (${JSON.stringify(node.text)})`,
    };
  }

  const p = literalText(destination);
  if (p === null) {
    return {
      read: null,
      write: null,
      reject: `non-literal redirect target (${JSON.stringify(destination.text)})`,
    };
  }
  if (WRITE_REDIRECT_OPS.has(operator)) return { read: null, write: p, reject: null };
  if (READ_REDIRECT_OPS.has(operator)) return { read: p, write: null, reject: null };
  return { read: null, write: null, reject: `unrecognized shell redirection operator (${JSON.stringify(operator)})` };
}

// G-4: a raw newline inside a `command` node span (outside a quote/heredoc/
// substitution, and not a `\` line-continuation) is a tree-sitter
// statement-fusion artifact that hides a later executing head. Rather than
// fail closed on every such case, the top-level decompose loop below REPAIRS
// the parse: it rewrites each fused newline to an explicit `;` (the
// unambiguous separator tree-sitter will not fuse) and re-parses the whole
// command, preserving compound context (if/then/else, loops). See
// opendaisugi/shell_decompose.py's module docstring for the full rationale.
const MULTILINE_LEGAL = new Set([
  "string", "raw_string", "ansi_c_string", "translated_string",
  "command_substitution", "process_substitution", "arithmetic_expansion",
  "heredoc_body", "heredoc_redirect",
]);

// web-tree-sitter reports node.startIndex/endIndex as UTF-16 code-unit
// offsets (NOT UTF-8 byte offsets), so scan the JS string directly.
function collectProtected(node: Node, out: Array<[number, number]>): void {
  if (MULTILINE_LEGAL.has(node.type)) {
    out.push([node.startIndex, node.endIndex]);
    return;
  }
  for (const child of node.children) {
    if (child !== null) collectProtected(child, out);
  }
}

/** Offsets of statement-terminator newlines fused into a single `command` node's own span. */
function bareNewlineOffsets(node: Node, src: string): number[] {
  const prot: Array<[number, number]> = [];
  collectProtected(node, prot);
  const offsets: number[] = [];
  for (let i = node.startIndex; i < node.endIndex; i++) {
    const ch = src.charCodeAt(i);
    if (ch !== 0x0a && ch !== 0x0d) continue;
    if (prot.some(([a, z]) => a <= i && i < z)) continue;
    if (i > 0 && src.charCodeAt(i - 1) === 0x5c) continue; // backslash line continuation
    offsets.push(i);
  }
  return offsets;
}

/** Union of fused-newline offsets across every `command` node in the tree, sorted. */
function allFusedNewlineOffsets(root: Node, src: string): number[] {
  const offsets = new Set<number>();
  function walk(node: Node): void {
    if (node.type === "command") {
      for (const o of bareNewlineOffsets(node, src)) offsets.add(o);
    }
    for (const child of node.children) {
      if (child !== null) walk(child);
    }
  }
  walk(root);
  return Array.from(offsets).sort((a, b) => a - b);
}

/**
 * End offsets of `comment` nodes — the newline that terminates each `#`
 * comment. A fused newline here must NOT become `;`: a comment runs to the
 * newline, so rewriting it would pull the next statement into the comment and
 * hide its head (a fail-OPEN that `hasError` cannot catch, since
 * `head;# note;sed` is valid shell). Keep such newlines verbatim.
 */
function commentEndOffsets(root: Node): Set<number> {
  const ends = new Set<number>();
  function walk(node: Node): void {
    if (node.type === "comment") ends.add(node.endIndex);
    for (const child of node.children) {
      if (child !== null) walk(child);
    }
  }
  walk(root);
  return ends;
}

/**
 * Replace each fused newline with `;` — the separator tree-sitter won't
 * fuse — but with a space where a `;` would abut an existing separator
 * (adjacency-guarded so the rewrite never forms `;;`, which is a `case`-only
 * token elsewhere and a syntax error in real bash), and left as a newline
 * where it terminates a `#` comment (so the comment's terminator survives).
 */
function rewriteFusedNewlines(
  src: string,
  offsets: number[],
  commentEnds: Set<number> = new Set(),
): string {
  const cut = new Set(offsets);
  const out: string[] = [];
  for (let i = 0; i < src.length; i++) {
    if (cut.has(i) && !commentEnds.has(i)) {
      let prev: string | null = null;
      for (let j = out.length - 1; j >= 0; j--) {
        if (out[j] !== " " && out[j] !== "\t") {
          prev = out[j]!;
          break;
        }
      }
      out.push(prev === ";" || prev === "&" || prev === "|" ? " " : ";");
    } else {
      out.push(src[i]!);
    }
  }
  return out.join("");
}

// Splitting at every bare newline strictly reduces the newline count, so a
// fragment can never re-fuse — this bound only trips on a logic bug, loudly.
const MAX_FUSION_SPLIT_DEPTH = 64;

export async function decomposeCommand(command: string, wasmPath?: string): Promise<Decomposition> {
  const parser = await getParser(wasmPath);
  return decomposeInner(parser, command, 0);
}

function decomposeInner(parser: Parser, command: string, depth: number): Decomposition {
  const tree = parser.parse(command);
  if (tree === null) {
    return reject("malformed shell (parse error)");
  }
  const root = tree.rootNode;
  if (root.hasError) {
    tree.delete();
    return reject("malformed shell (parse error)");
  }

  // G-4 repair: see the module-level comment above `MULTILINE_LEGAL`. A clean
  // re-parse of the rewrite decomposes correctly; a rewrite that is not valid
  // shell (`;` is illegal right after `then`/`do`/`else`) still fails closed.
  const fused = allFusedNewlineOffsets(root, command);
  if (fused.length > 0) {
    // Capture comment ends before freeing the tree — `root` is invalid after.
    const commentEnds = commentEndOffsets(root);
    tree.delete();
    if (depth >= MAX_FUSION_SPLIT_DEPTH) {
      throw new Error(
        "fusion-repair recursion exceeded; a rewrite re-fused, which should " +
        "be impossible (each pass replaces newlines with ';')"
      );
    }
    const rewritten = rewriteFusedNewlines(command, fused, commentEnds);
    if (rewritten === command) {
      // Every fused newline terminates a comment; none can be rewritten to `;`
      // without burying the next statement in the comment. Can't resolve
      // locally — fail closed, don't hide a head.
      return reject("ambiguous shell (bare newline inside command — parser statement fusion)");
    }
    const rewrittenTree = parser.parse(rewritten);
    const rewrittenHasError = rewrittenTree === null || rewrittenTree.rootNode.hasError;
    if (rewrittenTree !== null) rewrittenTree.delete();
    if (rewrittenHasError) {
      return reject("ambiguous shell (bare newline inside command — parser statement fusion)");
    }
    return decomposeInner(parser, rewritten, depth + 1);
  }

  const heads: string[] = [];
  const commands: string[] = [];
  const reads: string[] = [];
  const writes: string[] = [];
  let reason: string | null = null;

  function visit(node: Node): void {
    if (reason !== null) return;
    if (node.isMissing) {
      reason = "malformed shell (missing token)";
      return;
    }
    if (node.type === "file_redirect") {
      const { read, write, reject: rej } = classifyFileRedirect(node);
      if (rej !== null) {
        reason = rej;
      } else if (read !== null) {
        reads.push(read);
      } else if (write !== null) {
        writes.push(write);
      }
      return;
    }
    if (node.type === "command") {
      const name = node.childForFieldName("name");
      if (name === null) {
        reason = "command with no resolvable head";
        return;
      }
      const nameChildren = name.children.filter((c): c is Node => c !== null);
      if (nameChildren.length !== 1 || nameChildren[0]!.type !== "word") {
        reason = `non-literal command head (${JSON.stringify(name.text)})`;
        return;
      }
      const head = name.text;
      heads.push(head);
      commands.push(node.text);
      // fall through: keep walking for substitutions inside arguments
    }
    for (const child of node.children) {
      if (child === null) continue;
      visit(child);
      if (reason !== null) return;
    }
  }

  visit(root);
  // Every string we need (heads/commands/reads/writes/reason) is already
  // copied out into plain JS strings above — safe to free the wasm-heap
  // tree now rather than let it grow monotonically across the corpus.
  tree.delete();

  if (reason !== null) {
    return reject(reason);
  }
  if (heads.length === 0) {
    return reject("no command heads found");
  }
  return { ok: true, heads, commands, reads, writes, reason: "" };
}
